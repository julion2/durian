package imap

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"

	goimap "github.com/emersion/go-imap"
)


// getFolderTagMapping returns the tag mapping for a mailbox based on SPECIAL-USE attributes
// Returns tags to add and remove when a mail is found in this folder
// Used for both new downloads and deduplication (updating tags for existing mails)
func (s *Syncer) getFolderTagMapping(mailboxName string) *FolderTagMapping {
	// Special case: INBOX always gets inbox tag
	if strings.EqualFold(mailboxName, "INBOX") {
		return &FolderTagMapping{
			AddTags:    []string{"inbox"},
			RemoveTags: []string{},
		}
	}

	// Find the mailbox in our cached list and check its SPECIAL-USE attributes
	if m := s.specialUseMappingForMailbox(mailboxName); m != nil {
		return m
	}

	// No special-use attribute — check if the folder name matches a known role fallback
	for role, fallbacks := range defaultRoleFallbacks {
		if !slices.ContainsFunc(fallbacks, func(name string) bool {
			return strings.EqualFold(mailboxName, name)
		}) {
			continue
		}
		if m := lookupSpecialUseMapping(string(role)); m != nil {
			return m
		}
	}

	return nil
}

// specialUseMappingForMailbox looks up the cached mailbox by name and returns
// the tag mapping for its first recognized SPECIAL-USE attribute, or nil.
func (s *Syncer) specialUseMappingForMailbox(mailboxName string) *FolderTagMapping {
	for _, mbox := range s.serverMailboxes {
		if mbox.Name != mailboxName {
			continue
		}
		for _, attr := range mbox.Attributes {
			if m := lookupSpecialUseMapping(attr); m != nil {
				return m
			}
		}
		return nil
	}
	return nil
}

// lookupSpecialUseMapping returns the FolderTagMapping for a SPECIAL-USE
// attribute (case-insensitive), or nil if the attribute is unknown.
func lookupSpecialUseMapping(attr string) *FolderTagMapping {
	normalized := strings.ToLower(attr)
	for specialUse, mapping := range specialUseFolderTags {
		if strings.EqualFold(normalized, strings.ToLower(specialUse)) {
			m := mapping
			return &m
		}
	}
	return nil
}

// filterConflictingTags removes tags from addTags that conflict with the
// message's existing tags. For example, "inbox" should not be re-added to a
// message that already has "archive", "trash", or "spam".
func (s *Syncer) filterConflictingTags(messageID string, addTags []string) []string {
	if len(addTags) == 0 {
		return addTags
	}
	existing, err := s.store.GetTagsByMessageID(messageID)
	if err != nil {
		slog.Debug("Failed to get tags for conflict check", "module", "SYNC", "message_id", messageID, "err", err)
		return addTags
	}
	// Tags that block re-adding "inbox"
	inboxBlockers := []string{"archive", "trash", "spam"}
	var filtered []string
	for _, tag := range addTags {
		if tag == "inbox" && slices.ContainsFunc(existing, func(t string) bool {
			return slices.Contains(inboxBlockers, t)
		}) {
			slog.Debug("Skipping conflicting tag", "module", "SYNC", "message_id", messageID, "skipped", tag, "existing", existing)
			continue
		}
		filtered = append(filtered, tag)
	}
	return filtered
}

// syncFlags synchronizes flags between local store and IMAP server.
// Returns (flagsUploaded, flagsDownloaded, moved)
//
// This works for ALL messages on the server, not just those downloaded by durian.
// It builds a UID<->Message-ID mapping on first run (cached in state).
func (s *Syncer) syncFlags(mailboxName string, mboxState *MailboxState, allUIDs []uint32) (int, int, int) {
	var uploaded, downloaded, moved, flagErrors int

	if len(allUIDs) == 0 {
		return 0, 0, 0
	}

	// 1. Ensure we have Message-ID mapping for all UIDs
	if err := s.ensureMessageIDMapping(mailboxName, mboxState, allUIDs); err != nil {
		slog.Debug("Failed to build Message-ID mapping", "module", "SYNC", "err", err)
		// Continue anyway - we'll work with what we have
	}

	// 2. Fetch current flags from server for ALL UIDs
	serverFlags, err := s.client.FetchFlags(allUIDs)
	if err != nil {
		fmt.Fprintf(s.output, "    Warning: failed to fetch flags: %v\n", err)
		return 0, 0, 0
	}

	// 3. Get all local messages with tags in a single batch query
	slog.Debug("Starting flag sync", "module", "SYNC", "mailbox", mailboxName, "server_uids", len(allUIDs), "mapped_uids", mboxState.GetMappedUIDCount()) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys

	localMessages, err := s.store.GetAllMessagesWithTags(mailboxName, s.accountName())
	if err != nil {
		slog.Debug("Failed to get messages from store", "module", "SYNC", "err", err)
		localMessages = make(map[string][]string)
	}
	slog.Debug("Local messages in folder", "module", "SYNC", "count", len(localMessages)) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys

	// 5. For each UID on server, sync flags
	checkedCount := 0
	for _, uid := range allUIDs {
		messageID, hasMapping := mboxState.GetMessageID(uid)
		if !hasMapping || messageID == "" {
			continue // Can't sync without Message-ID
		}

		// Backfill UID for messages originally synced with uid=0
		_ = s.store.BackfillUID(messageID, s.accountName(), uid, mailboxName)

		// Check if message exists locally and get its tags
		tags, existsLocally := localMessages[messageID]
		if !existsLocally {
			continue // Message not in local folder
		}

		// Get server flags
		serverFlagList, ok := serverFlags[uid]
		if !ok {
			continue // Message not found on server (shouldn't happen)
		}
		serverState := FlagStateFromIMAP(serverFlagList)

		// Convert local tags to flag state
		localState := FlagStateFromTags(tags)

		checkedCount++

		// Get stored state (last sync baseline)
		storedState, hasStoredState := mboxState.GetMessageFlags(uid)

		if !hasStoredState {
			// First sync for this message - server is authoritative (no baseline to detect local changes)
			// Only download server flags to local; don't upload stale local state
			if !s.options.DryRun {
				mboxState.SetMessageFlags(uid, serverState)
			}

			if !localState.Equal(serverState) && s.options.Mode != SyncUploadOnly {
				if err := s.downloadFlagChanges(messageID, localState, serverState); err != nil {
					slog.Debug("Error downloading flags", "module", "SYNC", "uid", uid, "err", err)
					flagErrors++
				} else {
					downloaded++
					slog.Debug("First-sync downloaded flags", "module", "SYNC", "uid", uid, "message_id", messageID, "flags", serverState)
				}
			}
			continue
		}

		// Check for local changes (local differs from stored)
		if NeedsUpload(localState, storedState) && s.options.Mode != SyncDownloadOnly {
			if err := s.uploadFlagChanges(uid, localState, serverState); err != nil {
				slog.Debug("Error uploading flags", "module", "SYNC", "uid", uid, "err", err)
				flagErrors++
			} else {
				uploaded++
				slog.Debug("Uploaded flags", "module", "SYNC", "uid", uid, "from", storedState, "to", localState)
				// Update stored state (skip in dry-run)
				if !s.options.DryRun {
					mboxState.SetMessageFlags(uid, localState)
				}
			}
		}

		// Check for server changes (server differs from stored)
		if NeedsDownload(serverState, storedState) && s.options.Mode != SyncUploadOnly {
			// Check if local was also changed (conflict scenario)
			localChanged := NeedsUpload(localState, storedState)

			var targetState FlagState
			if localChanged {
				// Conflict: both local and server changed - merge (local wins)
				targetState = localState.Merge(serverState)
				slog.Debug("Flag conflict, merging", "module", "SYNC", "uid", uid)
			} else {
				// No local change - server wins (allows server to remove flags)
				targetState = serverState
			}

			if !targetState.Equal(localState) {
				if err := s.downloadFlagChanges(messageID, localState, targetState); err != nil {
					slog.Debug("Error downloading flags", "module", "SYNC", "uid", uid, "err", err)
					flagErrors++
				} else {
					downloaded++
					slog.Debug("Downloaded flags", "module", "SYNC", "uid", uid, "from", localState, "to", targetState)
				}
			}
			// Update stored state (skip in dry-run)
			if !s.options.DryRun {
				mboxState.SetMessageFlags(uid, targetState)
			}
		}
	}

	// Clean up stale inbox tags for messages no longer on server.
	// This catches messages that existed before durian (e.g., from mbsync) which
	// have no SyncedUID and thus aren't caught by GetDeletedUIDs.
	if strings.EqualFold(mailboxName, "INBOX") && !s.options.DryRun {
		serverMessageIDs := make(map[string]bool)
		for _, uid := range allUIDs {
			if messageID, ok := mboxState.GetMessageID(uid); ok && messageID != "" {
				serverMessageIDs[messageID] = true
			}
		}

		cleaned := 0
		for messageID, tags := range localMessages {
			hasInbox := false
			for _, tag := range tags {
				if tag == "inbox" {
					hasInbox = true
					break
				}
			}
			if hasInbox && !serverMessageIDs[messageID] {
				if err := s.store.ModifyTagsByMessageIDAndAccount(messageID, s.accountName(), nil, []string{"inbox"}); err != nil {
					slog.Debug("Failed to remove stale inbox tag", "module", "SYNC", "message_id", messageID, "err", err)
				} else {
					cleaned++
				}
			}
		}
		if cleaned > 0 {
			slog.Debug("Removed stale inbox tags", "module", "SYNC", "count", cleaned)
			slog.Debug("Removed stale inbox tags", "module", "SYNC", "count", cleaned)
		}
	}

	// Upload folder moves for INBOX messages that lost their "inbox" tag
	if strings.EqualFold(mailboxName, "INBOX") && s.options.Mode != SyncDownloadOnly {
		moved = s.uploadFolderMoves(mboxState, localMessages, allUIDs)
	}

	// Gmail: sync X-GM-LABELS → tags (only for All Mail, not Spam/Trash)
	if s.isGmailAllMail(mailboxName) {
		s.syncGmailLabels(mboxState, allUIDs)
	}

	slog.Debug("Flag sync complete", "module", "SYNC", "checked", checkedCount, "uploaded", uploaded, "downloaded", downloaded, "moved", moved, "errors", flagErrors)

	if uploaded > 0 || downloaded > 0 || moved > 0 || flagErrors > 0 {
		if s.options.DryRun {
			fmt.Fprintf(s.output, "    ⚑ Flags: %d would upload, %d would download (dry-run)\n", uploaded, downloaded)
		} else if flagErrors > 0 {
			fmt.Fprintf(s.output, "    ⚑ Flags: %d uploaded, %d downloaded, %d moved, %d errors\n", uploaded, downloaded, moved, flagErrors)
		} else {
			fmt.Fprintf(s.output, "    ⚑ Flags: %d uploaded, %d downloaded, %d moved\n", uploaded, downloaded, moved)
		}
	}

	return uploaded, downloaded, moved
}

// syncGmailLabels fetches X-GM-LABELS for all UIDs and syncs them to tags.
// Adds missing label tags and removes stale system label tags (e.g. inbox
// removed when a message is archived in Gmail).
func (s *Syncer) syncGmailLabels(mboxState *MailboxState, allUIDs []uint32) {
	gmailLabels, err := s.client.FetchGmailLabels(allUIDs)
	if err != nil {
		slog.Warn("Failed to fetch Gmail labels", "module", "SYNC", "err", err)
		return
	}

	// Build reverse map: tag → system label (for detecting stale tags)
	systemTagSet := make(map[string]bool)
	for _, tag := range gmailLabelTags {
		systemTagSet[tag] = true
	}

	updated := 0
	for _, uid := range allUIDs {
		messageID, hasMapping := mboxState.GetMessageID(uid)
		if !hasMapping || messageID == "" {
			continue
		}

		labels := gmailLabels[uid] // may be nil (no labels)

		// Convert labels to expected tags
		expectedTags := make(map[string]bool)
		for _, label := range labels {
			label = strings.Trim(label, "\"")
			if tag, ok := gmailSystemLabelTags[label]; ok {
				if tag != "" {
					expectedTags[tag] = true
				}
			} else {
				tag := strings.ToLower(label)
				tag = strings.ReplaceAll(tag, " ", "-")
				if tag != "" {
					expectedTags[tag] = true
				}
			}
		}

		// Get current tags
		currentTags, err := s.store.GetTagsByMessageID(messageID)
		if err != nil {
			continue
		}
		currentSet := make(map[string]bool, len(currentTags))
		for _, t := range currentTags {
			currentSet[t] = true
		}

		// Compute diff
		var tagsToAdd, tagsToRemove []string
		for tag := range expectedTags {
			if !currentSet[tag] {
				tagsToAdd = append(tagsToAdd, tag)
			}
		}
		// Only remove system label tags (inbox, sent, etc.) — not user tags
		// that might have been added by rules or manually
		for _, tag := range currentTags {
			if systemTagSet[tag] && !expectedTags[tag] {
				tagsToRemove = append(tagsToRemove, tag)
			}
		}

		if len(tagsToAdd) > 0 || len(tagsToRemove) > 0 {
			if err := s.store.ModifyTagsByMessageIDAndAccount(
				messageID, s.accountName(), tagsToAdd, tagsToRemove); err != nil {
				slog.Debug("Failed to sync Gmail labels", "module", "SYNC",
					"message_id", messageID, "err", err)
			} else {
				updated++
			}
		}
	}

	if updated > 0 {
		slog.Info("Gmail labels synced", "module", "SYNC", "updated", updated)
	}
}

// folderMove represents a pending IMAP folder move operation.
type folderMove struct {
	uid       uint32
	messageID string
	dest      string // destination mailbox name
}

// uploadFolderMoves detects INBOX messages whose local tags no longer include
// "inbox" and moves them to the appropriate IMAP folder (Trash or Archive).
// Uses COPY + \Deleted + Expunge since go-imap v1 has no MOVE command.
// Returns the number of messages moved.
func (s *Syncer) uploadFolderMoves(mboxState *MailboxState, localMessages map[string][]string, allUIDs []uint32) int {
	// Build O(1) lookup set for server UIDs
	allUIDSet := make(map[uint32]struct{}, len(allUIDs))
	for _, uid := range allUIDs {
		allUIDSet[uid] = struct{}{}
	}

	// Scan for messages that lost the "inbox" tag
	var moves []folderMove
	for messageID, tags := range localMessages {
		hasInbox := false
		hasDeleted := false
		for _, tag := range tags {
			switch tag {
			case "inbox":
				hasInbox = true
			case "deleted":
				hasDeleted = true
			}
		}
		if hasInbox {
			continue // Still in inbox — nothing to do
		}

		// Resolve UID from state mapping
		uid, ok := mboxState.GetUIDByMessageID(messageID)
		if !ok || uid == 0 {
			continue // No UID mapping — can't move
		}
		if _, onServer := allUIDSet[uid]; !onServer {
			continue // Already gone from INBOX on server
		}

		// Pick destination
		dest := "archive"
		if hasDeleted {
			dest = "trash"
		}
		moves = append(moves, folderMove{uid: uid, messageID: messageID, dest: dest})
	}

	if len(moves) == 0 {
		return 0
	}

	// Lazily resolve destination mailbox names
	if s.trashMailbox == "" {
		if trash, err := s.client.FindTrashMailbox(); err == nil {
			s.trashMailbox = trash
			slog.Debug("Resolved trash mailbox", "module", "SYNC", "account", s.accountName(), "mailbox", trash) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		} else {
			slog.Warn("No trash mailbox found", "module", "SYNC", "account", s.accountName(), "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		}
	}
	if s.archiveMailbox == "" {
		if archive, err := s.client.FindArchiveMailbox(); err == nil {
			s.archiveMailbox = archive
			slog.Debug("Resolved archive mailbox", "module", "SYNC", "account", s.accountName(), "mailbox", archive) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		} else if _, allErr := s.client.FindMailboxByRole(RoleAll); allErr == nil {
			// Gmail/Google Workspace: no \Archive folder, but \All (All Mail) exists.
			// Archiving = just remove from INBOX (message stays in All Mail automatically).
			s.archiveMailbox = "_expunge_only"
			slog.Debug("Gmail detected: archive via expunge-only (All Mail)", "module", "SYNC", "account", s.accountName()) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		} else {
			slog.Warn("No archive mailbox found", "module", "SYNC", "account", s.accountName(), "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		}
	}

	moved := 0
	for _, m := range moves {
		destMailbox := s.archiveMailbox
		if m.dest == "trash" {
			destMailbox = s.trashMailbox
		}
		if destMailbox == "" {
			slog.Debug("No destination mailbox found, skipping move", "module", "SYNC", "account", s.accountName(), "uid", m.uid, "dest", m.dest) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
			continue
		}

		if s.options.DryRun {
			slog.Debug("[dry-run] Would move message", "module", "SYNC", "uid", m.uid, "dest", destMailbox) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
			moved++
			continue
		}

		// COPY to destination (skip for Gmail expunge-only archive)
		if destMailbox != "_expunge_only" {
			if err := s.client.CopyToMailbox(m.uid, destMailbox); err != nil {
				slog.Debug("Copy failed for folder move", "module", "SYNC", "uid", m.uid, "dest", destMailbox, "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
				continue
			}
		}

		// Set \Deleted on source (INBOX)
		if err := s.client.AddFlags(m.uid, []string{goimap.DeletedFlag}); err != nil {
			slog.Debug("AddFlags failed for folder move", "module", "SYNC", "uid", m.uid, "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
			continue
		}

		// Expunge from INBOX
		if err := s.client.Expunge(); err != nil {
			slog.Debug("Expunge failed for folder move", "module", "SYNC", "uid", m.uid, "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		}

		// Clean up INBOX tracking state so next sync doesn't see this as "deleted from server"
		mboxState.RemoveSyncedUID(m.uid)

		moved++
		slog.Info("Moved message", "module", "SYNC", "uid", m.uid, "message_id", m.messageID, "dest", destMailbox) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
	}

	if moved > 0 {
		fmt.Fprintf(s.output, "    ↗ Moved %d messages\n", moved)
	}

	return moved
}

// uploadFlagChanges uploads flag changes to the IMAP server
// For deleted messages: copies to Trash, sets \Deleted flag, and expunges
func (s *Syncer) uploadFlagChanges(uid uint32, local, server FlagState) error {
	// Check if this is a delete operation (deleted locally but not on server)
	isDelete := local.Deleted && !server.Deleted

	if isDelete {
		// Find and cache trash mailbox
		if s.trashMailbox == "" {
			trash, err := s.client.FindTrashMailbox()
			if err != nil {
				slog.Debug("Could not find trash mailbox", "module", "SYNC", "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
				// Continue without copy - just set flag
			} else {
				s.trashMailbox = trash
				slog.Debug("Found trash mailbox", "module", "SYNC", "mailbox", trash) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
			}
		}

		if s.options.DryRun {
			if s.trashMailbox != "" {
				slog.Debug("[dry-run] Would copy to trash, set \\Deleted, and expunge", "module", "SYNC", "uid", uid, "trash", s.trashMailbox) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
			} else {
				slog.Debug("[dry-run] Would set \\Deleted and expunge (no trash mailbox)", "module", "SYNC", "uid", uid) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
			}
			return nil
		}

		// Copy to trash first (if trash mailbox found)
		if s.trashMailbox != "" {
			if err := s.client.CopyToMailbox(uid, s.trashMailbox); err != nil {
				slog.Debug("Copy to trash failed", "module", "SYNC", "uid", uid, "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
				return fmt.Errorf("copy to trash failed for UID %d: %w", uid, err)
			}
			slog.Debug("Copied to trash", "module", "SYNC", "uid", uid, "trash", s.trashMailbox) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		}

		// Set \Deleted flag (use AddFlags to preserve server-only keywords like $Completed)
		if err := s.client.AddFlags(uid, []string{goimap.DeletedFlag}); err != nil {
			return err
		}

		// Expunge to permanently remove from current mailbox
		if err := s.client.Expunge(); err != nil {
			slog.Debug("Expunge failed", "module", "SYNC", "err", err)
		}

		return nil
	}

	// Regular flag update — use AddFlags/RemoveFlags to preserve server-only
	// keywords like $Completed that ToIMAPFlags() doesn't include
	toAdd, toRemove := DiffFlags(local, server)

	if s.options.DryRun {
		slog.Debug("[dry-run] Would upload flags", "module", "SYNC", "uid", uid, "add", toAdd, "remove", toRemove)
		return nil
	}

	if err := s.client.AddFlags(uid, toAdd); err != nil {
		return err
	}
	return s.client.RemoveFlags(uid, toRemove)
}

// downloadFlagChanges downloads flag changes to store
func (s *Syncer) downloadFlagChanges(messageID string, current, target FlagState) error {
	if current.Equal(target) {
		return nil
	}

	addTags, removeTags := target.ToTagOps()

	if s.options.DryRun {
		slog.Debug("[dry-run] Would update tags", "module", "SYNC", "message_id", messageID, "add", addTags, "remove", removeTags)
		return nil
	}

	if err := s.store.ModifyTagsByMessageIDAndAccount(messageID, s.accountName(), addTags, removeTags); err != nil {
		return fmt.Errorf("store flag tag write: %w", err)
	}
	return nil
}
