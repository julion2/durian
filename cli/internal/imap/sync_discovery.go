package imap

import (
	"fmt"
	"log/slog"
	"strings"

	goimap "github.com/emersion/go-imap"

	"github.com/julion2/durian/cli/internal/config"
)

// isGmail returns true if the account is a Gmail/Google Workspace account.
func (s *Syncer) isGmail() bool {
	return s.account.OAuth != nil && s.account.OAuth.Provider == "google"
}

// isGmailAllMail returns true if this is Gmail and the mailbox is All Mail.
func (s *Syncer) isGmailAllMail(mailboxName string) bool {
	if !s.isGmail() {
		return false
	}
	// Check SPECIAL-USE \All attribute
	for _, mbox := range s.serverMailboxes {
		if mbox.Name == mailboxName {
			for _, attr := range mbox.Attributes {
				if attr == "\\All" {
					return true
				}
			}
		}
	}
	return false
}

// gmailSystemLabelTags maps Gmail system labels to durian tags.
// Empty string means the label is recognized but ignored (no tag created).
var gmailSystemLabelTags = map[string]string{
	"\\Inbox":     "inbox",
	"\\Sent":      "sent",
	"\\Draft":     "draft",
	"\\Starred":   "flagged",
	"\\Important": "important",
	"\\Trash":     "trash",
	"\\Spam":      "spam",
	// Gmail category tabs — ignore (not useful as tags)
	"CATEGORY_PERSONAL":   "",
	"CATEGORY_SOCIAL":     "",
	"CATEGORY_PROMOTIONS": "",
	"CATEGORY_UPDATES":    "",
	"CATEGORY_FORUMS":     "",
}

// gmailLabelTags is the subset of gmailSystemLabelTags that produce actual tags.
// Used by syncGmailLabels to detect stale tags that should be removed.
var gmailLabelTags = func() map[string]string {
	m := make(map[string]string)
	for k, v := range gmailSystemLabelTags {
		if v != "" {
			m[k] = v
		}
	}
	return m
}()

// gmailLabelsToTags extracts X-GM-LABELS from an IMAP message and converts them to tags.
func gmailLabelsToTags(msg *goimap.Message) []string {
	raw, ok := msg.Items["X-GM-LABELS"]
	if !ok || raw == nil {
		return nil
	}

	labels, ok := raw.([]interface{})
	if !ok {
		return nil
	}

	var tags []string
	for _, l := range labels {
		label := fmt.Sprintf("%v", l)
		label = strings.Trim(label, "\"")

		if tag, ok := gmailSystemLabelTags[label]; ok {
			if tag != "" {
				tags = append(tags, tag)
			}
			// Empty string = recognized but ignored (e.g. CATEGORY_*)
		} else {
			// User labels → lowercase tag with spaces→hyphens
			tag := strings.ToLower(label)
			tag = strings.ReplaceAll(tag, " ", "-")
			if tag != "" {
				tags = append(tags, tag)
			}
		}
	}
	return tags
}



// getMailboxesToSync returns the list of mailboxes to sync
func (s *Syncer) getMailboxesToSync() ([]string, error) {
	// If specific mailboxes are requested via CLI, use those
	if len(s.options.Mailboxes) > 0 {
		return s.options.Mailboxes, nil
	}

	// Gmail: sync only All Mail + Spam + Trash (labels come via X-GM-LABELS)
	if s.isGmail() && len(s.account.IMAP.Mailboxes) == 0 {
		slog.Info("Gmail account detected, using All Mail sync strategy", "module", "SYNC") // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		return s.resolveGmailMailboxes()
	}

	// If explicit mailboxes are configured in config, use those
	if len(s.account.IMAP.Mailboxes) > 0 {
		configuredMailboxes := s.account.GetIMAPMailboxes()

		// List all mailboxes on server
		serverMailboxes, err := s.client.ListMailboxes()
		if err != nil {
			return nil, fmt.Errorf("failed to list mailboxes: %w", err)
		}

		// Match configured patterns against server mailboxes
		var result []string
		for _, serverMbox := range serverMailboxes {
			name := serverMbox.Name

			// Skip excluded mailboxes
			if config.IsIMAPMailboxExcluded(name) {
				continue
			}

			// Check if mailbox matches any configured pattern
			for _, pattern := range configuredMailboxes {
				if matchMailbox(name, pattern) {
					result = append(result, name)
					break
				}
			}
		}

		return result, nil
	}

	// No explicit config - use SPECIAL-USE auto-detection
	// This auto-detects localized folder names (e.g., "Gesendete Elemente" for Sent)
	mailboxes, err := s.client.GetSyncMailboxes()
	if err != nil {
		slog.Debug("SPECIAL-USE detection failed, falling back to defaults", "module", "SYNC", "err", err)
		// Fallback to default names (legacy behavior)
		return s.getMailboxesByName(config.DefaultIMAPMailboxes)
	}

	if len(mailboxes) == 0 {
		slog.Debug("No mailboxes detected via SPECIAL-USE, falling back to defaults", "module", "SYNC") // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		return s.getMailboxesByName(config.DefaultIMAPMailboxes)
	}

	return mailboxes, nil
}

// resolveGmailMailboxes finds the localized Gmail system mailboxes via SPECIAL-USE.
// Gmail localizes folder names (e.g., "[Gmail]/Alle Nachrichten" for All Mail in German).
func (s *Syncer) resolveGmailMailboxes() ([]string, error) {
	allMail, err := s.client.FindMailboxByRole(RoleAll)
	if err != nil {
		return nil, fmt.Errorf("cannot find All Mail folder: %w", err)
	}

	var result []string
	result = append(result, allMail)

	if spam, err := s.client.FindMailboxByRole(RoleJunk); err == nil {
		result = append(result, spam)
	}
	if trash, err := s.client.FindMailboxByRole(RoleTrash); err == nil {
		result = append(result, trash)
	}

	slog.Info("Gmail mailboxes resolved", "module", "SYNC", "mailboxes", result) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
	return result, nil
}

// getMailboxesByName finds mailboxes by matching against a list of names (legacy fallback)
func (s *Syncer) getMailboxesByName(names []string) ([]string, error) {
	serverMailboxes, err := s.client.ListMailboxes()
	if err != nil {
		return nil, fmt.Errorf("failed to list mailboxes: %w", err)
	}

	var result []string
	for _, serverMbox := range serverMailboxes {
		name := serverMbox.Name

		if config.IsIMAPMailboxExcluded(name) {
			continue
		}

		for _, pattern := range names {
			if matchMailbox(name, pattern) {
				result = append(result, name)
				break
			}
		}
	}

	return result, nil
}

// matchMailbox checks if a mailbox name matches a pattern
func matchMailbox(name, pattern string) bool {
	// Case-insensitive comparison
	nameLower := strings.ToLower(name)
	patternLower := strings.ToLower(pattern)

	// Exact match
	if nameLower == patternLower {
		return true
	}

	// Prefix match with word boundary (e.g., "Sent" matches "Sent Items" but not "SentBackup")
	if strings.HasPrefix(nameLower, patternLower) && len(nameLower) > len(patternLower) {
		next := nameLower[len(patternLower)]
		if next == ' ' || next == '/' {
			return true
		}
	}

	return false
}
