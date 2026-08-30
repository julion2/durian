package imap

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	goimap "github.com/emersion/go-imap"
)

// flagTransport is the slice of the IMAP client the flag pass actually uses.
// The pass reads server flags and writes flag deltas; everything else it does
// is decision-making against the store and the state file. Naming that slice
// lets those decisions be exercised without a live server — which matters here,
// because the defects this reconciliation has had were all decisions, never
// protocol.
type flagTransport interface {
	FetchFlags(uids []uint32) (map[uint32][]string, error)
	AddFlags(uid uint32, flags []string) error
	RemoveFlags(uid uint32, flags []string) error
}

type folderMoveTransport interface {
	ListMailboxes() ([]*goimap.MailboxInfo, error)
	SupportsCapability(capability string) (bool, error)
	CreateMailbox(name string) error
	MoveMessageToMailbox(uid uint32, messageID, destMailbox string) error
}

// flags returns the transport the flag pass should use. Production always gets
// the real client; tests substitute a scripted one.
func (s *Syncer) flags() flagTransport {
	if s.flagTransportOverride != nil {
		return s.flagTransportOverride
	}
	return s.client
}

func (s *Syncer) folderMoves() folderMoveTransport {
	if s.folderMoveTransportOverride != nil {
		return s.folderMoveTransportOverride
	}
	return s.client
}

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
// Returns (flagsUploaded, flagsDownloaded, moved, moveError).
//
// This works for ALL messages on the server, not just those downloaded by durian.
// It builds a UID<->Message-ID mapping on first run (cached in state).
func (s *Syncer) syncFlags(mailboxName string, mboxState *MailboxState, allUIDs []uint32) (int, int, int, error) {
	var uploaded, downloaded, moved, flagErrors int
	var mappingErr, moveErr error

	if len(allUIDs) == 0 {
		return 0, 0, 0, nil
	}

	// 1. Ensure we have Message-ID mapping for all UIDs
	if err := s.ensureMessageIDMapping(mailboxName, mboxState, allUIDs); err != nil {
		mappingErr = err
		slog.Debug("Failed to build Message-ID mapping", "module", "SYNC", "err", err)
		// Continue anyway - we'll work with what we have
	}

	// 2. Fetch current flags from server for ALL UIDs
	serverFlags, err := s.flags().FetchFlags(allUIDs)
	if err != nil {
		fmt.Fprintf(s.output, "    Warning: failed to fetch flags: %v\n", err)
		return 0, 0, 0, errors.Join(mappingErr, fmt.Errorf("fetch flags: %w", err))
	}

	// 3. Get all local messages with tags in a single batch query
	slog.Debug("Starting flag sync", "module", "SYNC", "mailbox", mailboxName, "server_uids", len(allUIDs), "mapped_uids", mboxState.GetMappedUIDCount()) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys

	localMessages, err := s.store.GetAllMessagesWithTags(mailboxName, s.accountName())
	if err != nil {
		slog.Debug("Failed to get messages from store", "module", "SYNC", "err", err)
		return 0, 0, 0, errors.Join(mappingErr, fmt.Errorf("load local messages for flag sync: %w", err))
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
		if !s.options.DryRun {
			_ = s.store.BackfillUID(messageID, s.accountName(), uid, mailboxName)
		}

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
			// First sync for this message: no baseline exists, so a local
			// difference cannot be told apart from a flag the user never set.
			// The server is authoritative here — but the baseline may only
			// record that once the local side actually holds it. Writing it up
			// front, as this did, banked a reconciliation that had not happened:
			// an UploadOnly run recorded server state it never applied locally,
			// and the next run read the local absence as a user change and
			// pushed it back.
			if s.options.Mode == SyncUploadOnly && !localState.Equal(serverState) {
				continue
			}
			applied, err := s.downloadFlagChanges(messageID, tags, localState, serverState)
			switch {
			case err != nil:
				slog.Debug("Error downloading flags", "module", "SYNC", "uid", uid, "err", err) // encgrep:allow word "flags" in message text, no flag value logged
				flagErrors++
			case !applied:
				// The tags moved under this run; leave the baseline unset so the
				// next poll resolves against what the user actually did.
				slog.Debug("First-sync flag write refused", "module", "SYNC", "uid", uid)
			default:
				if !localState.Equal(serverState) {
					downloaded++
					slog.Debug("First-sync downloaded flags", "module", "SYNC", "uid", uid, "message_id", messageID, "flags", serverState) // encgrep:allow flags value redacted at runtime by redact.SensitiveSlogKeys ("flags")
				}
				if !s.options.DryRun {
					mboxState.SetMessageFlags(uid, serverState)
				}
			}
			continue
		}

		// One resolved state for both directions, decided per flag against the
		// baseline. Whichever side differs from it moved that flag; when both
		// differ they moved a boolean to the same value, so there is nothing to
		// arbitrate and no rule about who wins. Deleted and Completed stay
		// server-owned because no tag can express them.
		targetState := ResolveFlags(storedState, localState, serverState)

		// Which halves actually reached their side. The baseline may only record
		// what one of these carried out.
		var pushed, pulled bool

		if NeedsUpload(localState, storedState) && s.options.Mode != SyncDownloadOnly {
			if err := s.uploadFlagChanges(uid, targetState, serverState); err != nil {
				slog.Debug("Error uploading flags", "module", "SYNC", "uid", uid, "err", err) // encgrep:allow word "flags" in message text, no flag value logged
				flagErrors++
			} else {
				pushed = true
				uploaded++
				slog.Debug("Uploaded flags", "module", "SYNC", "uid", uid, "from", storedState, "to", targetState) // encgrep:allow IMAP flag-state transition for sync debug; from/to here are state directions, not addresses
			}
		}

		if NeedsDownload(serverState, storedState) && s.options.Mode != SyncUploadOnly {
			applied, err := s.downloadFlagChanges(messageID, tags, localState, targetState)
			switch {
			case err != nil:
				slog.Debug("Error downloading flags", "module", "SYNC", "uid", uid, "err", err) // encgrep:allow word "flags" in message text, no flag value logged
				flagErrors++
			case !applied:
				slog.Debug("Flag write refused, local state moved", "module", "SYNC", "uid", uid)
			default:
				pulled = true
				if !targetState.Equal(localState) {
					downloaded++
					slog.Debug("Downloaded flags", "module", "SYNC", "uid", uid, "from", localState, "to", targetState) // encgrep:allow IMAP flag-state transition for sync debug; from/to here are state directions, not addresses
				}
			}
		}

		// A refused download does not cancel a successful upload: the pushed
		// fields reached the server and must advance, only the pulled half
		// waits for the next poll.
		if (pushed || pulled) && !s.options.DryRun {
			next := AdvanceBaseline(storedState, localState, serverState, targetState, pushed, pulled)
			if !next.Equal(storedState) {
				mboxState.SetMessageFlags(uid, next)
			}
		}
	}

	// Clean up stale inbox tags for messages no longer on server.
	// This catches messages that existed before durian (e.g., from mbsync) which
	// have no SyncedUID and thus aren't caught by GetDeletedUIDs.
	mappingComplete := mappingErr == nil && len(mboxState.GetMissingMappingUIDs(allUIDs)) == 0
	if strings.EqualFold(mailboxName, "INBOX") && !s.options.DryRun && mappingComplete {
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
		}
	}

	// Upload folder moves for INBOX messages that lost their "inbox" tag
	if strings.EqualFold(mailboxName, "INBOX") && s.options.Mode != SyncDownloadOnly {
		moved, moveErr = s.uploadFolderMoves(mboxState, localMessages, allUIDs)
	}

	// Gmail: sync X-GM-LABELS → tags (only for All Mail, not Spam/Trash)
	if s.isGmailAllMail(mailboxName) && !s.options.DryRun {
		s.syncGmailLabels(mboxState, allUIDs)
	}

	slog.Debug("Flag sync complete", "module", "SYNC", "checked", checkedCount, "uploaded", uploaded, "downloaded", downloaded, "moved", moved, "errors", flagErrors, "move_error", moveErr != nil)

	if uploaded > 0 || downloaded > 0 || moved > 0 || flagErrors > 0 {
		if s.options.DryRun {
			fmt.Fprintf(s.output, "    ⚑ Flags: %d would upload, %d would download (dry-run)\n", uploaded, downloaded)
		} else if flagErrors > 0 {
			fmt.Fprintf(s.output, "    ⚑ Flags: %d uploaded, %d downloaded, %d moved, %d errors\n", uploaded, downloaded, moved, flagErrors)
		} else {
			fmt.Fprintf(s.output, "    ⚑ Flags: %d uploaded, %d downloaded, %d moved\n", uploaded, downloaded, moved)
		}
	}

	return uploaded, downloaded, moved, errors.Join(mappingErr, moveErr)
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

// folderMoveDestination classifies the local folder intent. The GUI and tag
// handler use "trash"; "deleted" is retained for stores written by older
// clients. A message that still has "inbox" has no pending move.
func folderMoveDestination(tags []string) (string, bool) {
	dest := "archive"
	for _, tag := range tags {
		switch tag {
		case "inbox":
			return "", false
		case "trash", "deleted":
			dest = "trash"
		}
	}
	return dest, true
}

// inferArchiveMailboxName places a new Archive beside the server's existing
// special-use mailboxes. A successful LIST does not by itself reveal the
// personal namespace, so fail closed when those siblings provide no unique
// answer instead of guessing a top-level name.
func inferArchiveMailboxName(mailboxes []*goimap.MailboxInfo) (string, error) {
	prefixes := make(map[string]struct{})
	roles := []SpecialUseRole{RoleTrash, RoleSent, RoleDrafts, RoleJunk}
	for _, mailbox := range mailboxes {
		isSpecialUseSibling := false
		for _, attr := range mailbox.Attributes {
			if slices.ContainsFunc(roles, func(role SpecialUseRole) bool {
				return strings.EqualFold(attr, string(role))
			}) {
				isSpecialUseSibling = true
				break
			}
		}
		if !isSpecialUseSibling {
			continue
		}

		prefix := ""
		if mailbox.Delimiter != "" {
			if i := strings.LastIndex(mailbox.Name, mailbox.Delimiter); i >= 0 {
				prefix = mailbox.Name[:i+len(mailbox.Delimiter)]
			}
		}
		prefixes[prefix] = struct{}{}
	}

	if len(prefixes) == 0 {
		return "", fmt.Errorf("cannot infer Archive namespace from special-use mailboxes")
	}
	if len(prefixes) != 1 {
		return "", fmt.Errorf("cannot infer Archive namespace: special-use mailboxes have different parents")
	}
	for prefix := range prefixes {
		return prefix + "Archive", nil
	}
	panic("unreachable")
}

// uploadFolderMoves detects INBOX messages whose local tags no longer include
// "inbox" and moves them to the appropriate IMAP folder (Trash or Archive).
// Returns the number of messages moved and any failure that left a requested
// move pending.
func (s *Syncer) uploadFolderMoves(mboxState *MailboxState, localMessages map[string][]string, allUIDs []uint32) (int, error) {
	// Build O(1) lookup set for server UIDs
	allUIDSet := make(map[uint32]struct{}, len(allUIDs))
	for _, uid := range allUIDs {
		allUIDSet[uid] = struct{}{}
	}

	// Scan for messages that lost the "inbox" tag
	var moves []folderMove
	for messageID, tags := range localMessages {
		dest, pending := folderMoveDestination(tags)
		if !pending {
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

		moves = append(moves, folderMove{uid: uid, messageID: messageID, dest: dest})
	}

	if len(moves) == 0 {
		return 0, nil
	}

	var needTrash, needArchive bool
	for _, move := range moves {
		if move.dest == "trash" {
			needTrash = true
		} else {
			needArchive = true
		}
	}

	moveClient := s.folderMoves()
	supportsMove, err := moveClient.SupportsCapability("MOVE")
	if err != nil {
		return 0, fmt.Errorf("check MOVE capability: %w", err)
	}
	if !supportsMove {
		return 0, fmt.Errorf("IMAP server does not support safe message moves (MOVE capability missing)")
	}

	var moveErrors []error
	var mailboxes []*goimap.MailboxInfo
	if (needTrash && s.trashMailbox == "") || (needArchive && s.archiveMailbox == "") {
		var err error
		mailboxes, err = moveClient.ListMailboxes()
		if err != nil {
			return 0, fmt.Errorf("resolve folder-move mailboxes: %w", err)
		}
	}

	// Lazily resolve only the destination mailbox names these moves need, using
	// the same successful LIST for role absence and namespace inference.
	if needTrash && s.trashMailbox == "" {
		if trash, err := FindMailboxByRoleIn(mailboxes, RoleTrash); err == nil {
			s.trashMailbox = trash
			slog.Debug("Resolved trash mailbox", "module", "SYNC", "account", s.accountName(), "mailbox", trash) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		} else {
			slog.Warn("No trash mailbox found", "module", "SYNC", "account", s.accountName(), "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
			moveErrors = append(moveErrors, fmt.Errorf("resolve trash mailbox: %w", err))
		}
	}
	if needArchive && s.archiveMailbox == "" {
		if archive, err := FindMailboxByRoleIn(mailboxes, RoleArchive); err == nil {
			s.archiveMailbox = archive
			slog.Debug("Resolved archive mailbox", "module", "SYNC", "account", s.accountName(), "mailbox", archive) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		} else if !errors.Is(err, ErrMailboxRoleNotFound) {
			moveErrors = append(moveErrors, fmt.Errorf("resolve archive mailbox: %w", err))
		} else {
			// Gmail's All Mail is an archive destination, but \All alone does
			// not imply Gmail semantics on another provider.
			if s.isGmail() {
				if allMail, allErr := FindMailboxByRoleIn(mailboxes, RoleAll); allErr == nil {
					s.archiveMailbox = allMail
					slog.Debug("Resolved Gmail archive mailbox", "module", "SYNC", "account", s.accountName(), "mailbox", allMail) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
				} else if !errors.Is(allErr, ErrMailboxRoleNotFound) {
					moveErrors = append(moveErrors, fmt.Errorf("resolve Gmail all-mail mailbox: %w", allErr))
				}
			}
			if s.archiveMailbox == "" {
				archive, inferErr := inferArchiveMailboxName(mailboxes)
				if inferErr != nil {
					moveErrors = append(moveErrors, fmt.Errorf("resolve archive mailbox: %w", inferErr))
				} else if s.options.DryRun {
					s.archiveMailbox = archive
					fmt.Fprintf(s.output, "    + Would create archive mailbox %s (dry-run)\n", archive)
				} else if createErr := moveClient.CreateMailbox(archive); createErr != nil {
					slog.Warn("Create archive mailbox failed", "module", "SYNC", "account", s.accountName(), "mailbox", archive, "err", createErr) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
					moveErrors = append(moveErrors, fmt.Errorf("create archive mailbox: %w", createErr))
				} else {
					s.archiveMailbox = archive
					slog.Info("Created archive mailbox", "module", "SYNC", "account", s.accountName(), "mailbox", archive) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
					fmt.Fprintf(s.output, "    + Created archive mailbox %s\n", archive)
				}
			}
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

		if err := moveClient.MoveMessageToMailbox(m.uid, m.messageID, destMailbox); err != nil {
			slog.Debug("Folder move failed", "module", "SYNC", "uid", m.uid, "dest", destMailbox, "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
			moveErrors = append(moveErrors, fmt.Errorf("move UID %d to %s: %w", m.uid, destMailbox, err))
			continue
		}

		// Clean up INBOX tracking state so next sync doesn't see this as "deleted from server"
		mboxState.RemoveSyncedUID(m.uid)

		moved++
		slog.Info("Moved message", "module", "SYNC", "uid", m.uid, "message_id", m.messageID, "dest", destMailbox) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
	}

	if moved > 0 {
		fmt.Fprintf(s.output, "    ↗ Moved %d messages\n", moved)
	}

	return moved, errors.Join(moveErrors...)
}

// uploadFlagChanges moves the server to the resolved state.
//
// It used to carry a delete path here — copy to trash, set \Deleted, expunge —
// gated on local.Deleted && !server.Deleted. That gate can never open:
// FlagStateFromTags never sets Deleted, deliberately, because Durian's
// "trash" tag (and legacy "deleted") means moved-to-trash while \Deleted means
// pending expunge. The branch was unreachable, and an unreachable expunge is
// not something to leave standing. Deletes travel through uploadFolderMoves,
// which uses UID MOVE to the trash mailbox explicitly.
func (s *Syncer) uploadFlagChanges(uid uint32, target, server FlagState) error {
	// Use AddFlags/RemoveFlags rather than a full store, to preserve
	// server-only keywords like $Completed that ToIMAPFlags() doesn't include.
	toAdd, toRemove := DiffFlags(target, server)
	if len(toAdd) == 0 && len(toRemove) == 0 {
		// NeedsUpload compares local against the baseline, so it can fire with
		// nothing to send.
		return nil
	}

	if s.options.DryRun {
		slog.Debug("[dry-run] Would upload flags", "module", "SYNC", "uid", uid, "add", toAdd, "remove", toRemove) // encgrep:allow word "flags" in message text, no flag value logged
		return nil
	}

	// Each half only when it has something to say. Naming which half failed
	// matters because the two are not equivalent to recover from: a failed add
	// leaves the server missing a flag, a failed remove leaves one set, and the
	// baseline stays behind either way so the next fetch resolves it.
	if len(toAdd) > 0 {
		if err := s.flags().AddFlags(uid, toAdd); err != nil {
			return fmt.Errorf("add flags on UID %d: %w", uid, err)
		}
	}
	if len(toRemove) > 0 {
		if err := s.flags().RemoveFlags(uid, toRemove); err != nil {
			return fmt.Errorf("remove flags on UID %d: %w", uid, err)
		}
	}
	return nil
}

// downloadFlagChanges writes the resolved state to the message's tags, but only
// while the tags the decision was read from still hold. snapshotTags is what
// this run read before talking to the server; a change landing in that window
// is invisible to the merge, and ToTagOps writes absolutely, so an unguarded
// write would revert it and the baseline advanced afterwards would agree with
// the reverted tags — leaving nothing for a later run to detect.
//
// Reports whether the write happened. A refusal is not an error: the caller
// leaves the pulled half of its baseline alone and the next poll reconciles
// against fresh state.
func (s *Syncer) downloadFlagChanges(messageID string, snapshotTags []string, current, target FlagState) (bool, error) {
	if current.Equal(target) {
		return true, nil
	}

	addTags, removeTags := target.ToTagOps()

	if s.options.DryRun {
		slog.Debug("[dry-run] Would update tags", "module", "SYNC", "message_id", messageID, "add", addTags, "remove", removeTags)
		return true, nil
	}

	applied, err := s.store.ModifyFlagTagsIfUnchanged(messageID, s.accountName(),
		FlagTagVocabulary(), flagTagsOf(snapshotTags), addTags, removeTags)
	if err != nil {
		return false, fmt.Errorf("store flag tag write: %w", err)
	}
	return applied, nil
}

// flagTagsOf narrows a message's tags to the ones a flag decision depends on,
// so an unrelated tag arriving mid-sync does not read as a conflict.
func flagTagsOf(tags []string) []string {
	var out []string
	for _, tag := range tags {
		if slices.Contains(FlagTagVocabulary(), tag) {
			out = append(out, tag)
		}
	}
	return out
}
