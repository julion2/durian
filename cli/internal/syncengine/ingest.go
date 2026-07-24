package syncengine

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/imap"
	durianmail "github.com/julion2/durian/cli/internal/mail"
	"github.com/julion2/durian/cli/internal/store"
)

// parser is the shared (stateless) RFC822 parser, same as the legacy syncer's.
var parser = durianmail.NewParser()

// builtinIndexedHeaders is a copy of imap.builtinSelectedHeaders (unexported
// there; deliberately NOT edited into an export to keep the strangler-fig
// constraint of touching no existing files). Keep the two lists in sync until
// the legacy syncer is retired.
var builtinIndexedHeaders = []string{
	"List-Id", "List-Unsubscribe", "Precedence",
	"X-Mailer", "Return-Path", "X-GitHub-Reason",
	"Authentication-Results",
}

// folderTagMapping defines which tags to add/remove when a message is found in
// a folder (mirror of imap.FolderTagMapping, keyed by backend.Role instead of
// IMAP SPECIAL-USE attributes).
type folderTagMapping struct {
	addTags    []string
	removeTags []string
}

// roleTagMappings maps backend folder roles to tag operations. It replicates
// imap.specialUseFolderTags (\Trash => +trash,-inbox etc.) plus the INBOX
// special case from getFolderTagMapping: the backend resolves special-use /
// name fallbacks into a Role, so the engine only needs this role-keyed table.
// RoleAll (Gmail All Mail) intentionally has no mapping — see the scope note
// on Ingest.
var roleTagMappings = map[backend.Role]folderTagMapping{
	backend.RoleInbox:   {addTags: []string{"inbox"}},
	backend.RoleSent:    {addTags: []string{"sent"}},
	backend.RoleDrafts:  {addTags: []string{"draft"}},
	backend.RoleTrash:   {addTags: []string{"trash"}, removeTags: []string{"inbox"}},
	backend.RoleJunk:    {addTags: []string{"spam"}, removeTags: []string{"inbox"}},
	backend.RoleArchive: {addTags: []string{"archive"}, removeTags: []string{"inbox"}},
}

// tagMappingForRole returns the tag mapping for a folder role, or nil for
// user folders (RoleNone) and roles without a mapping.
func tagMappingForRole(role backend.Role) *folderTagMapping {
	if m, ok := roleTagMappings[role]; ok {
		return &m
	}
	return nil
}

// flagStateFromBackend converts neutral backend.Flags to imap.FlagState. The
// fields are identical by design; imap.FlagState is reused so ToTagOps /
// ToIMAPFlags stay the single source of truth for flag<->tag semantics.
func flagStateFromBackend(f backend.Flags) imap.FlagState {
	return imap.FlagState{
		Seen:      f.Seen,
		Flagged:   f.Flagged,
		Answered:  f.Answered,
		Deleted:   f.Deleted,
		Completed: f.Completed,
	}
}

// backendFlagsFromState is the inverse of flagStateFromBackend: it converts an
// imap.FlagState (e.g. derived from local tags via imap.FlagStateFromTags)
// into the neutral backend.Flags handed to Backend.ApplyFlags.
func backendFlagsFromState(f imap.FlagState) backend.Flags {
	return backend.Flags{
		Seen:      f.Seen,
		Flagged:   f.Flagged,
		Answered:  f.Answered,
		Deleted:   f.Deleted,
		Completed: f.Completed,
	}
}

// IngestOptions configures message ingestion.
type IngestOptions struct {
	// Account is the account identifier (the store's account column).
	Account string
	// FilterRules are the user-defined filter rules applied at insert time
	// (same type the legacy storeInsertMessage consumes via SyncOptions).
	FilterRules []config.RuleConfig
	// Groups are contact groups for group: expansion in rule match expressions.
	Groups map[string]config.GroupEntry
	// IndexedHeaders are user-added MIME header names indexed on top of the
	// builtin set (config.pkl sync.indexed_headers).
	IndexedHeaders []string
}

// headerSet returns the deduped, case-insensitive union of the builtin header
// allowlist and IndexedHeaders, builtins first (port of (*imap.Syncer).headerSet).
func (o IngestOptions) headerSet() []string {
	seen := make(map[string]struct{}, len(builtinIndexedHeaders)+len(o.IndexedHeaders))
	out := make([]string, 0, len(builtinIndexedHeaders)+len(o.IndexedHeaders))
	for _, h := range builtinIndexedHeaders {
		k := strings.ToLower(h)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, h)
	}
	for _, h := range o.IndexedHeaders {
		k := strings.ToLower(strings.TrimSpace(h))
		if k == "" {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, h)
	}
	return out
}

// Ingest parses a raw message fetched from a backend and writes it to the
// SQLite store: message row, attachments, indexed headers, folder-role tags,
// flag tags, cal tag, and filter rules. It is a provider-neutral port of the
// legacy (*imap.Syncer).storeInsertMessage and must stay behavior-identical
// to it, with one deliberate exception:
//
// SCOPE NOTE: this ingest omits the Gmail X-GM-LABELS branch (isGmailAllMail /
// gmailLabelsToTags). Gmail accounts stay on the legacy imap.Syncer, where All
// Mail label sync is authoritative; the engine path targets Microsoft Graph
// and generic IMAP, where folder-role mapping is authoritative. Routing Gmail
// through this engine would tag All Mail messages with folder-role tags
// instead of their labels.
//
// Returns the canonical Message-ID under which the message was stored, so the
// engine can track RemoteRef->MessageID for deletions and flag updates.
func Ingest(db *store.DB, msg backend.Message, folderName string, role backend.Role, opts IngestOptions) (string, error) {
	parsed, err := mail.ReadMessage(bytes.NewReader(msg.Raw))
	if err != nil {
		return "", fmt.Errorf("parse message: %w", err)
	}

	content := parser.Parse(parsed)
	messageID := strings.Trim(content.MessageID, "<>")
	if messageID == "" {
		// The backend already computed the Message-ID (possibly synthetic) —
		// prefer it so the engine and the backend agree on message identity.
		messageID = strings.Trim(msg.MessageID, "<>")
	}
	if messageID == "" {
		// Last resort: synthesize one from the provider ref so the message is
		// not lost (mirrors the legacy durian-synthetic-<uid> fallback).
		messageID = fmt.Sprintf("durian-synthetic-%s-%s@%s", msg.Ref.ID, folderName, opts.Account)
		slog.Warn("Message has no Message-ID, using synthetic ID", "module", "SYNCENGINE",
			"ref", msg.Ref.ID, "folder", folderName, "synthetic_id", messageID)
	}

	var dateUnix int64
	if t, err := mail.ParseDate(content.Date); err == nil {
		dateUnix = t.Unix()
	} else {
		// Fallback to the server receive time (legacy: IMAP internal date)
		dateUnix = msg.InternalDate.Unix()
	}

	// Flags column: the store splits this comma-joined IMAP-style flag string
	// elsewhere, so derive it via FlagState.ToIMAPFlags to keep the format
	// byte-identical with rows written by the legacy syncer.
	flagState := flagStateFromBackend(msg.Flags)
	flagStr := strings.Join(flagState.ToIMAPFlags(), ",")

	storeMsg := &store.Message{
		MessageID: messageID,
		Subject:   content.Subject,
		FromAddr:  content.From,
		ToAddrs:   content.To,
		CCAddrs:   content.CC,
		InReplyTo: content.InReplyTo,
		Refs:      content.References,
		BodyText:  content.Body,
		BodyHTML:  content.HTML,
		Date:      dateUnix,
		CreatedAt: time.Now().Unix(),
		Mailbox:   folderName,
		Flags:     flagStr,
		// UID stays 0 on the engine path: the uint32 UID column is an IMAP
		// implementation detail; the neutral RemoteRef column carries the
		// provider handle instead.
		UID:         0,
		Size:        len(msg.Raw),
		FetchedBody: true,
		Account:     opts.Account,
		RemoteRef:   msg.Ref.ID,
		// The message's current server flags are the correct initial
		// baseline: the first post-ingest flag pass is then a no-op unless
		// the user changed something locally. joinFlags (not flagStr) so the
		// baseline round-trips $Completed and avoids per-sync download churn.
		SyncedFlags: joinFlags(flagState),
	}

	if err := db.InsertMessage(storeMsg); err != nil {
		return "", fmt.Errorf("insert message: %w", err)
	}

	// Clear old attachments on upsert, then re-insert
	_ = db.DeleteAttachmentsByMessageDBID(storeMsg.ID)
	for i, att := range content.Attachments {
		partID := att.PartID
		if partID == 0 {
			partID = i + 1
		}
		if err := db.InsertAttachment(&store.Attachment{
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

	// Store selected headers for rule matching and analysis (builtin set plus
	// user-added entries from config.pkl sync.indexed_headers).
	for _, hdrName := range opts.headerSet() {
		if v := parsed.Header.Get(hdrName); v != "" {
			_ = db.InsertHeader(storeMsg.ID, strings.ToLower(hdrName), v)
		}
	}

	// Folder-role tags (legacy: SPECIAL-USE tag mapping; see scope note above
	// for why there is no Gmail label branch here).
	if mapping := tagMappingForRole(role); mapping != nil {
		for _, tag := range mapping.addTags {
			if err := db.AddTag(storeMsg.ID, tag); err != nil {
				return "", fmt.Errorf("add folder tag %q: %w", tag, err)
			}
		}
	}

	// Flag-based tags (unread, flagged, replied). Like the legacy insert path
	// this only applies the add half; the engine's per-folder flag
	// reconciliation pass applies removals so re-fetched (updated) messages
	// shed stale flag tags.
	flagAdd, _ := flagState.ToTagOps()
	for _, tag := range flagAdd {
		if err := db.AddTag(storeMsg.ID, tag); err != nil {
			return "", fmt.Errorf("add flag tag %q: %w", tag, err)
		}
	}

	// Eagerly detect calendar content
	if bytes.Contains(msg.Raw, []byte("text/calendar")) {
		if err := db.AddTag(storeMsg.ID, "cal"); err != nil {
			return "", fmt.Errorf("add cal tag: %w", err)
		}
	}

	if err := applyFilterRules(db, storeMsg, content, parsed, opts); err != nil {
		return "", err
	}

	return messageID, nil
}

// applyFilterRules evaluates the user's filter rules against the freshly
// inserted message and applies their tag operations, including exec hooks.
//
// TECH DEBT: the rule matcher and exec runner still live in the imap package
// (imap.MatchingRules / imap.RunExecRule / imap.RuleAttachment). They are
// protocol-agnostic and should move to a neutral package once the legacy
// syncer is retired; importing imap here is the price of the no-edits
// strangler-fig constraint.
func applyFilterRules(db *store.DB, storeMsg *store.Message, content *durianmail.MailContent, parsed *mail.Message, opts IngestOptions) error {
	if len(opts.FilterRules) == 0 {
		return nil
	}

	atts := make([]imap.RuleAttachment, len(content.Attachments))
	for i, a := range content.Attachments {
		atts[i] = imap.RuleAttachment{ContentType: a.ContentType, Filename: a.Filename}
	}
	matched := imap.MatchingRules(opts.FilterRules, storeMsg, atts, parsed.Header, opts.Account, opts.Groups)
	slog.Debug("Filter rules matched", "module", "RULES", "matched", len(matched), "total", len(opts.FilterRules), "message_id", storeMsg.MessageID)
	for _, rule := range matched {
		addTags := rule.AddTags
		removeTags := rule.RemoveTags

		// Run exec hook if configured
		if rule.Exec != "" {
			currentTags, _ := db.GetTagsByMessageID(storeMsg.MessageID)
			execOut, err := imap.RunExecRule(rule, storeMsg, currentTags, opts.Account)
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
			if err := db.AddTag(storeMsg.ID, tag); err != nil {
				return fmt.Errorf("add rule tag %q: %w", tag, err)
			}
		}
		for _, tag := range removeTags {
			if err := db.RemoveTag(storeMsg.ID, tag); err != nil {
				return fmt.Errorf("remove rule tag %q: %w", tag, err)
			}
		}
		slog.Debug("Applied filter rule", "module", "SYNCENGINE", "rule", rule.Name, "message_id", storeMsg.MessageID)
	}
	return nil
}

// filterAllowedTags returns only tags in the allowed list, logging rejects.
// Local copy of the unexported imap.filterAllowedTags (no-edits constraint).
func filterAllowedTags(tags, allowed []string, ruleName string) []string {
	if len(tags) == 0 {
		return tags
	}
	set := make(map[string]bool, len(allowed))
	for _, t := range allowed {
		set[t] = true
	}
	var filtered []string
	for _, t := range tags {
		if set[t] {
			filtered = append(filtered, t)
		} else {
			slog.Warn("Exec rule returned disallowed tag", "module", "RULES", "rule", ruleName, "tag", t)
		}
	}
	return filtered
}
