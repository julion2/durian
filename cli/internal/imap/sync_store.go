package imap

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"

	goimap "github.com/emersion/go-imap"

	durianmail "github.com/julion2/durian/cli/internal/mail"
	"github.com/julion2/durian/cli/internal/store"
	"github.com/julion2/durian/cli/internal/syncidentity"
)

// accountName returns the account identifier (e.g. "work") used as the
// account column in the SQLite store.
func (s *Syncer) accountName() string {
	return s.account.AccountIdentifier()
}

// storeInsertMessage parses a raw email and inserts it into the SQLite store.
// Eagerly applies folder and content tags at insert time.
func (s *Syncer) storeInsertMessage(mailboxName string, uidValidity uint32, matcher *syncidentity.Matcher, imapMsg *goimap.Message, msgBody []byte) (string, error) {
	parsed, err := mail.ReadMessage(bytes.NewReader(msgBody))
	if err != nil {
		return "", fmt.Errorf("parse message: %w", err)
	}

	content := s.parser.Parse(parsed)
	var dateUnix int64
	if t, err := mail.ParseDate(content.Date); err == nil {
		dateUnix = t.Unix()
	} else {
		// Fallback to IMAP internal date
		dateUnix = imapMsg.InternalDate.Unix()
	}
	messageID := strings.Trim(content.MessageID, "<>")
	syntheticIdentity := messageID == ""
	recoveredIdentity := false
	if messageID == "" {
		if matcher != nil {
			messageID = matcher.Match(content, dateUnix)
			recoveredIdentity = messageID != ""
		}
		if messageID == "" {
			messageID = durianmail.SyntheticMessageID(uidValidity, imapMsg.Uid, mailboxName, s.accountName())
		}
		slog.Warn("Message has no Message-ID, using synthetic ID", "module", "SYNC",
			"uid", imapMsg.Uid, "mailbox", mailboxName, "synthetic_id", messageID)
	}

	storeMsg := StoreMessageFromContent(messageID, content, dateUnix, time.Now().Unix())
	storeMsg.Mailbox = mailboxName
	storeMsg.Flags = strings.Join(imapMsg.Flags, ",")
	storeMsg.UID = imapMsg.Uid
	storeMsg.Size = len(msgBody)
	storeMsg.FetchedBody = true
	storeMsg.Account = s.accountName()
	storeMsg.SyntheticIdentity = syntheticIdentity

	if err := s.store.InsertMessage(storeMsg); err != nil {
		if recoveredIdentity {
			matcher.Restore(messageID)
		}
		return "", fmt.Errorf("insert message: %w", err)
	}
	if recoveredIdentity {
		// The existing row now points at the new UID. Preserve its attachments,
		// indexed headers and rule-derived tags, and never repeat exec hooks.
		matcher.Commit(messageID)
		return messageID, nil
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
			return "", fmt.Errorf("insert attachment %d: %w", i, err)
		}
	}

	// Store selected headers for rule matching and analysis (builtin set
	// plus user-added entries from config.pkl sync.indexed_headers).
	for _, hdrName := range s.headerSet() {
		if v := parsed.Header.Get(hdrName); v != "" {
			_ = s.store.InsertHeader(storeMsg.ID, strings.ToLower(hdrName), v)
		}
	}

	// Apply tags: Gmail All Mail uses X-GM-LABELS; everything else uses folder mapping.
	if s.isGmailAllMail(mailboxName) {
		for _, tag := range gmailLabelsToTags(imapMsg) {
			if err := s.store.AddTag(storeMsg.ID, tag); err != nil {
				return "", fmt.Errorf("add gmail label tag %q: %w", tag, err)
			}
		}
	} else {
		mapping := s.getFolderTagMapping(mailboxName)
		if mapping != nil {
			for _, tag := range mapping.AddTags {
				if err := s.store.AddTag(storeMsg.ID, tag); err != nil {
					return "", fmt.Errorf("add folder tag %q: %w", tag, err)
				}
			}
		}
	}

	// Apply flag-based tags (unread, flagged, replied)
	flagState := FlagStateFromIMAP(imapMsg.Flags)
	flagAdd, _ := flagState.ToTagOps()
	for _, tag := range flagAdd {
		if err := s.store.AddTag(storeMsg.ID, tag); err != nil {
			return "", fmt.Errorf("add flag tag %q: %w", tag, err)
		}
	}

	// Eagerly detect calendar content
	if bytes.Contains(msgBody, []byte("text/calendar")) {
		if err := s.store.AddTag(storeMsg.ID, "cal"); err != nil {
			return "", fmt.Errorf("add cal tag: %w", err)
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
					return "", fmt.Errorf("add rule tag %q: %w", tag, err)
				}
			}
			for _, tag := range removeTags {
				if err := s.store.RemoveTag(storeMsg.ID, tag); err != nil {
					return "", fmt.Errorf("remove rule tag %q: %w", tag, err)
				}
			}
			slog.Debug("Applied filter rule", "module", "SYNC", "rule", rule.Name, "message_id", messageID)
		}
	}

	return messageID, nil
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
