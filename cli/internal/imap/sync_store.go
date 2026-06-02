package imap

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"

	goimap "github.com/emersion/go-imap"

	"github.com/julion2/durian/cli/internal/store"
)


// accountName returns the account identifier (e.g. "work") used as the
// account column in the SQLite store.
func (s *Syncer) accountName() string {
	return s.account.AccountIdentifier()
}

// storeInsertMessage parses a raw email and inserts it into the SQLite store.
// Eagerly applies folder and content tags at insert time.
func (s *Syncer) storeInsertMessage(mailboxName string, imapMsg *goimap.Message, msgBody []byte) error {
	parsed, err := mail.ReadMessage(bytes.NewReader(msgBody))
	if err != nil {
		return fmt.Errorf("parse message: %w", err)
	}

	content := s.parser.Parse(parsed)
	messageID := strings.Trim(content.MessageID, "<>")
	if messageID == "" {
		// Generate synthetic Message-ID from UID + account to avoid losing the message
		messageID = fmt.Sprintf("durian-synthetic-%d-%s@%s", imapMsg.Uid, mailboxName, s.accountName())
		slog.Warn("Message has no Message-ID, using synthetic ID", "module", "SYNC",
			"uid", imapMsg.Uid, "mailbox", mailboxName, "synthetic_id", messageID)
	}

	var dateUnix int64
	if t, err := mail.ParseDate(content.Date); err == nil {
		dateUnix = t.Unix()
	} else {
		// Fallback to IMAP internal date
		dateUnix = imapMsg.InternalDate.Unix()
	}

	storeMsg := &store.Message{
		MessageID:   messageID,
		Subject:     content.Subject,
		FromAddr:    content.From,
		ToAddrs:     content.To,
		CCAddrs:     content.CC,
		InReplyTo:   content.InReplyTo,
		Refs:        content.References,
		BodyText:    content.Body,
		BodyHTML:    content.HTML,
		Date:        dateUnix,
		CreatedAt:   time.Now().Unix(),
		Mailbox:     mailboxName,
		Flags:       strings.Join(imapMsg.Flags, ","),
		UID:         imapMsg.Uid,
		Size:        len(msgBody),
		FetchedBody: true,
		Account:     s.accountName(),
	}

	if err := s.store.InsertMessage(storeMsg); err != nil {
		return fmt.Errorf("insert message: %w", err)
	}

	// Clear old attachments on upsert, then re-insert
	_ = s.store.DeleteAttachmentsByMessageDBID(storeMsg.ID)
	for i, att := range content.Attachments {
		partID := att.PartID
		if partID == 0 {
			partID = i + 1
		}
		if err := s.store.InsertAttachment(&store.Attachment{
			MessageDBID: storeMsg.ID,
			PartID:      partID,
			Filename:    att.Filename,
			ContentType: att.ContentType,
			Size:        att.Size,
			Disposition: att.Disposition,
			ContentID:   att.ContentID,
		}); err != nil {
			return fmt.Errorf("insert attachment %d: %w", i, err)
		}
	}

	// Store selected headers for rule matching and analysis
	for _, hdrName := range selectedHeaders {
		if v := parsed.Header.Get(hdrName); v != "" {
			_ = s.store.InsertHeader(storeMsg.ID, strings.ToLower(hdrName), v)
		}
	}

	// Apply tags: Gmail All Mail uses X-GM-LABELS; everything else uses folder mapping.
	if s.isGmailAllMail(mailboxName) {
		for _, tag := range gmailLabelsToTags(imapMsg) {
			if err := s.store.AddTag(storeMsg.ID, tag); err != nil {
				return fmt.Errorf("add gmail label tag %q: %w", tag, err)
			}
		}
	} else {
		mapping := s.getFolderTagMapping(mailboxName)
		if mapping != nil {
			for _, tag := range mapping.AddTags {
				if err := s.store.AddTag(storeMsg.ID, tag); err != nil {
					return fmt.Errorf("add folder tag %q: %w", tag, err)
				}
			}
		}
	}

	// Apply flag-based tags (unread, flagged, replied)
	flagState := FlagStateFromIMAP(imapMsg.Flags)
	flagAdd, _ := flagState.ToTagOps()
	for _, tag := range flagAdd {
		if err := s.store.AddTag(storeMsg.ID, tag); err != nil {
			return fmt.Errorf("add flag tag %q: %w", tag, err)
		}
	}

	// Eagerly detect calendar content
	if bytes.Contains(msgBody, []byte("text/calendar")) {
		if err := s.store.AddTag(storeMsg.ID, "cal"); err != nil {
			return fmt.Errorf("add cal tag: %w", err)
		}
	}

	// Apply user-defined filter rules
	if len(s.options.FilterRules) > 0 {
		atts := make([]RuleAttachment, len(content.Attachments))
		for i, a := range content.Attachments {
			atts[i] = RuleAttachment{ContentType: a.ContentType, Filename: a.Filename}
		}
		matched := MatchingRules(s.options.FilterRules, storeMsg, atts, parsed.Header, s.accountName(), s.options.Groups)
		slog.Debug("Filter rules matched", "module", "RULES", "matched", len(matched), "total", len(s.options.FilterRules), "message_id", messageID)
		for _, rule := range matched {
			addTags := rule.AddTags
			removeTags := rule.RemoveTags

			// Run exec hook if configured
			if rule.Exec != "" {
				currentTags, _ := s.store.GetTagsByMessageID(storeMsg.MessageID)
				execOut, err := RunExecRule(rule, storeMsg, currentTags, s.accountName())
				if err != nil {
					slog.Warn("Exec rule failed, using static tags", "module", "RULES", "rule", rule.Name, "err", err)
				} else if execOut != nil {
					execAdd := execOut.AddTags
					execRemove := execOut.RemoveTags

					// Filter by allowed_tags if set
					if len(rule.AllowedTags) > 0 {
						execAdd = filterAllowedTags(execAdd, rule.AllowedTags, rule.Name)
						execRemove = filterAllowedTags(execRemove, rule.AllowedTags, rule.Name)
					}

					addTags = append(addTags, execAdd...)
					removeTags = append(removeTags, execRemove...)
				}
			}

			for _, tag := range addTags {
				if err := s.store.AddTag(storeMsg.ID, tag); err != nil {
					return fmt.Errorf("add rule tag %q: %w", tag, err)
				}
			}
			for _, tag := range removeTags {
				if err := s.store.RemoveTag(storeMsg.ID, tag); err != nil {
					return fmt.Errorf("remove rule tag %q: %w", tag, err)
				}
			}
			slog.Debug("Applied filter rule", "module", "SYNC", "rule", rule.Name, "message_id", messageID)
		}
	}

	return nil
}

// extractMessageIDFromBody extracts Message-ID from raw email body using net/mail
func extractMessageIDFromBody(body []byte) string {
	msg, err := mail.ReadMessage(bytes.NewReader(body))
	if err != nil {
		return ""
	}

	messageID := msg.Header.Get("Message-ID")
	if messageID == "" {
		messageID = msg.Header.Get("Message-Id")
	}

	// Remove < and > brackets
	return strings.Trim(messageID, "<>")
}
