package imap

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/mail"
	"sort"
	"strings"
)

// builtinSelectedHeaders is the universal-default set of MIME headers
// fetched into message_headers for rule matching. Users can extend this
// per-account or per-install via config.pkl's sync.indexed_headers
// listing; the runtime set is the case-insensitive union of these
// built-ins and the user additions (see (*Syncer).headerSet).
//
// These seven cover ~90% of inbox-zero rule patterns: mailing-list
// identification (List-Id, List-Unsubscribe, Precedence), automation
// markers (X-Mailer, Return-Path), GitHub notification routing
// (X-GitHub-Reason), and sender verification (Authentication-Results).
// Provider-specific additions like X-GitLab-NotificationReason,
// X-Spam-Status, etc. belong in the user's indexed_headers config.
var builtinSelectedHeaders = []string{
	"List-Id", "List-Unsubscribe", "Precedence",
	"X-Mailer", "Return-Path", "X-GitHub-Reason",
	"Authentication-Results",
}

// headerSet returns the deduped, case-insensitive union of the built-in
// header allowlist and the user's sync.indexed_headers entries. Output
// preserves built-ins-first order; user entries follow in declaration
// order. Used by both the live-sync header insert (sync_store.go) and
// the backfill-headers path (sync_mailbox.go).
func (s *Syncer) headerSet() []string {
	seen := make(map[string]struct{}, len(builtinSelectedHeaders)+len(s.options.IndexedHeaders))
	out := make([]string, 0, len(builtinSelectedHeaders)+len(s.options.IndexedHeaders))
	for _, h := range builtinSelectedHeaders {
		k := strings.ToLower(h)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, h)
	}
	for _, h := range s.options.IndexedHeaders {
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

// backfillHeaders fetches headers from the IMAP server for messages that
// are already in the store but don't have entries in message_headers yet.
func (s *Syncer) backfillHeaders(mailboxes []string) {
	fmt.Fprintf(s.output, "  Backfilling headers...\n")
	for _, mboxName := range mailboxes {
		s.backfillHeadersForMailbox(mboxName)
	}
}

// backfillHeadersForMailbox fetches and stores raw headers for messages in a
// single mailbox that don't yet have their selected headers populated.
func (s *Syncer) backfillHeadersForMailbox(mboxName string) {
	mboxState := s.state.GetMailboxState(mboxName)
	if _, err := s.client.SelectMailbox(mboxName); err != nil {
		slog.Debug("Backfill: skip mailbox", "module", "SYNC", "mailbox", mboxName, "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		return
	}

	uidsToFetch := s.uidsNeedingHeaderBackfill(mboxState)
	if len(uidsToFetch) == 0 {
		return
	}

	fmt.Fprintf(s.output, "    %s: fetching headers for %d messages...\n", mboxName, len(uidsToFetch))

	const batchSize = 500
	stored := 0
	for i := 0; i < len(uidsToFetch); i += batchSize {
		end := i + batchSize
		if end > len(uidsToFetch) {
			end = len(uidsToFetch)
		}
		stored += s.backfillHeaderBatch(mboxName, mboxState, uidsToFetch[i:end])
	}

	fmt.Fprintf(s.output, "    ✓ %d messages backfilled\n", stored)
}

// uidsNeedingHeaderBackfill returns UIDs in the given mailbox whose messages
// are in the store but don't yet have header rows.
func (s *Syncer) uidsNeedingHeaderBackfill(mboxState *MailboxState) []uint32 {
	var uids []uint32
	for _, uid := range mboxState.SyncedUIDs {
		messageID, ok := mboxState.GetMessageID(uid)
		if !ok || messageID == "" {
			continue
		}
		dbID, err := s.store.GetMessageDBID(messageID, s.accountName())
		if err != nil || dbID == 0 {
			continue
		}
		// Without --force, skip messages that already have at least one
		// header row — incremental backfill. With --force, refetch
		// everything; needed after the user changes sync.indexed_headers
		// because the existing rows reflect the old configured set.
		if !s.options.BackfillHeadersForce {
			if has, _ := s.store.HasHeaders(dbID); has {
				continue
			}
		}
		uids = append(uids, uid)
	}
	return uids
}

// backfillHeaderBatch fetches headers for a batch of UIDs and writes the
// selected headers to the store. Returns the number of messages that had
// headers stored.
func (s *Syncer) backfillHeaderBatch(mboxName string, mboxState *MailboxState, batch []uint32) int {
	headers, err := s.client.FetchHeadersOnly(batch)
	if err != nil {
		slog.Debug("Backfill fetch failed", "module", "SYNC", "mailbox", mboxName, "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		return 0
	}
	stored := 0
	for uid, rawHeader := range headers {
		if s.storeHeadersForUID(uid, rawHeader, mboxState) {
			stored++
		}
	}
	return stored
}

// storeHeadersForUID parses raw headers for one message and inserts the
// selectedHeaders into the store. Returns true if the message was processed.
func (s *Syncer) storeHeadersForUID(uid uint32, rawHeader []byte, mboxState *MailboxState) bool {
	messageID, _ := mboxState.GetMessageID(uid)
	dbID, err := s.store.GetMessageDBID(messageID, s.accountName())
	if err != nil || dbID == 0 {
		return false
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(append(rawHeader, '\r', '\n')))
	if err != nil {
		return false
	}
	for _, hdrName := range s.headerSet() {
		if v := parsed.Header.Get(hdrName); v != "" {
			if err := s.store.InsertHeader(dbID, strings.ToLower(hdrName), v); err != nil {
				slog.Debug("InsertHeader failed", "module", "SYNC", "uid", uid, "header", hdrName, "err", err) // encgrep:allow word "header" in message text, no header value logged
			}
		}
	}
	return true
}

// syncMailbox syncs a single mailbox
func (s *Syncer) syncMailbox(mailboxName string) MailboxResult {
	result := MailboxResult{Name: mailboxName}
	slog.Debug("Syncing mailbox", "module", "SYNC", "mailbox", mailboxName) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys

	// Select mailbox
	status, err := s.client.SelectMailbox(mailboxName)
	if err != nil {
		result.Error = err
		return result
	}
	result.TotalMsgs = status.Messages

	// Get mailbox state
	mboxState := s.state.GetMailboxState(mailboxName)

	// Check UIDVALIDITY
	if mboxState.NeedsFullResync(status.UidValidity) {
		fmt.Fprintf(s.output, "    UIDVALIDITY changed, performing full resync\n")
		mboxState.Reset(status.UidValidity)
	}
	mboxState.UIDValidity = status.UidValidity

	// Get all UIDs
	allUIDs, err := s.client.SearchAll()
	if err != nil {
		result.Error = fmt.Errorf("failed to search messages: %w", err)
		return result
	}
	slog.Debug("Total UIDs on server", "module", "SYNC", "count", len(allUIDs))

	// Get unsynced UIDs
	unsyncedUIDs := mboxState.GetUnsyncedUIDs(allUIDs)
	slog.Debug("Unsynced UIDs", "module", "SYNC", "count", len(unsyncedUIDs))

	// Check for deleted/moved messages (UIDs that are locally synced but no longer on server)
	deletedUIDs := mboxState.GetDeletedUIDs(allUIDs)
	if len(deletedUIDs) > 0 {
		if s.options.DryRun {
			fmt.Fprintf(s.output, "    Would remove %d deleted messages\n", len(deletedUIDs))
			result.DeletedMsgs = len(deletedUIDs)
		} else {
			// When a message disappears from a folder, remove that folder's tags
			// instead of deleting the message. The message may have been moved to
			// another folder (e.g. archived) and will reappear during that folder's sync.
			tagMapping := s.getFolderTagMapping(mailboxName)
			fmt.Fprintf(s.output, "  ✗ %s: %d removed\n", mailboxName, len(deletedUIDs))
			for _, uid := range deletedUIDs {
				s.handleDeletedUID(uid, mailboxName, mboxState, tagMapping)
				result.DeletedMsgs++
			}
		}
	}

	if len(unsyncedUIDs) == 0 {
		// Still run flag sync even if no new messages
		// Flag sync runs even in dry-run mode to show what would happen
		if !s.options.NoFlags {
			uploaded, downloaded, movedMsgs := s.syncFlags(mailboxName, mboxState, allUIDs)
			result.FlagsUploaded = uploaded
			result.FlagsDownload = downloaded
			result.MovedMsgs = movedMsgs
		}
		return result
	}

	// Apply max messages limit
	maxMessages := s.account.GetIMAPMaxMessages()
	if maxMessages > 0 && len(unsyncedUIDs) > maxMessages {
		// Sort descending (newest first) and take the most recent
		sort.Slice(unsyncedUIDs, func(i, j int) bool {
			return unsyncedUIDs[i] > unsyncedUIDs[j]
		})
		unsyncedUIDs = unsyncedUIDs[:maxMessages]
		fmt.Fprintf(s.output, "    Limited to %d most recent messages\n", maxMessages)
	}

	// Deduplication: Check if messages already exist locally (moved from another folder)
	// Fetch Message-IDs for unsynced UIDs first
	toDownload := s.dedupUnsyncedUIDs(mailboxName, mboxState, unsyncedUIDs, &result)

	// Nothing left to download after deduplication
	if len(toDownload) == 0 {
		if result.DeduplicatedMsgs > 0 || result.DeletedMsgs > 0 {
			// Still run flag sync
			if !s.options.NoFlags {
				uploaded, downloaded, movedMsgs := s.syncFlags(mailboxName, mboxState, allUIDs)
				result.FlagsUploaded = uploaded
				result.FlagsDownload = downloaded
				result.MovedMsgs = movedMsgs
			}
		}
		return result
	}

	// Fetch remaining messages in batches
	batchSize := s.account.GetIMAPBatchSize()
	totalBatches := (len(toDownload) + batchSize - 1) / batchSize

	for i := 0; i < len(toDownload); i += batchSize {
		end := i + batchSize
		if end > len(toDownload) {
			end = len(toDownload)
		}
		batch := toDownload[i:end]
		batchNum := (i / batchSize) + 1

		fmt.Fprintf(s.output, "  ↓ %s: batch %d/%d (%d-%d)...\n",
			mailboxName, batchNum, totalBatches, i+1, end)

		if s.options.DryRun {
			result.NewMsgs += len(batch)
			continue
		}

		// Fetch messages
		messages, err := s.client.FetchMessages(batch)
		if err != nil {
			fmt.Fprintf(s.output, "    Warning: batch fetch failed: %v\n", err)
			result.SkippedMsgs += len(batch)
			continue
		}

		// Write to maildir
		for _, msg := range messages {
			// Read message body once (io.Reader can only be read once)
			var msgBody []byte
			for _, literal := range msg.Body {
				data, err := io.ReadAll(literal)
				if err == nil {
					msgBody = data
					break
				}
			}

			if len(msgBody) == 0 {
				slog.Debug("Message has no body data", "module", "SYNC", "uid", msg.Uid) // encgrep:allow "body data" in message text, no body content logged
				fmt.Fprintf(s.output, "    Warning: failed to write message %d: message has no body\n", msg.Uid)
				result.SkippedMsgs++
				continue
			}

			// Insert into SQLite store with eager tags
			if err := s.storeInsertMessage(mailboxName, msg, msgBody); err != nil {
				fmt.Fprintf(s.output, "    Warning: failed to store message %d: %v\n", msg.Uid, err)
				result.SkippedMsgs++
				continue
			}

			// Mark as synced in state (no more .uid marker files needed)
			mboxState.AddSyncedUID(msg.Uid)

			// Store initial flag state
			initialFlags := FlagStateFromIMAP(msg.Flags)
			mboxState.SetMessageFlags(msg.Uid, initialFlags)

			// Extract and store Message-ID for flag sync
			messageID := extractMessageIDFromBody(msgBody)
			if messageID != "" {
				mboxState.SetMessageID(msg.Uid, messageID)
				result.NewMessageIDs = append(result.NewMessageIDs, messageID)
			}

			result.NewMsgs++
		}
	}

	if result.NewMsgs > 0 {
		fmt.Fprintf(s.output, "  ✓ %s: %d new\n", mailboxName, result.NewMsgs)
	}

	// Flag synchronization (after message download)
	// Runs in all modes except when --no-flags is set
	// The syncFlags function internally respects the sync mode and dry-run for upload/download
	if !s.options.NoFlags {
		uploaded, downloaded, movedMsgs := s.syncFlags(mailboxName, mboxState, allUIDs)
		result.FlagsUploaded = uploaded
		result.FlagsDownload = downloaded
		result.MovedMsgs = movedMsgs
	}

	return result
}

// handleDeletedUID processes a single UID that disappeared from the server.
// If the folder has a tag mapping, the folder tags are removed (message was
// likely moved). Otherwise the message is deleted from the store. The UID is
// always removed from the synced set.
func (s *Syncer) handleDeletedUID(uid uint32, mailboxName string, mboxState *MailboxState, tagMapping *FolderTagMapping) {
	defer mboxState.RemoveSyncedUID(uid)

	messageID, hasID := mboxState.GetMessageID(uid)
	if !hasID || messageID == "" {
		slog.Debug("No Message-ID for deleted UID, skipping", "module", "SYNC", "uid", uid)
		return
	}

	if tagMapping != nil && len(tagMapping.AddTags) > 0 {
		// Remove the folder's tags (reverse of adding them on download)
		slog.Debug("Removing folder tags for moved message", "module", "SYNC", // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
			"uid", uid, "message_id", messageID, "folder", mailboxName, "tags", tagMapping.AddTags)
		if err := s.store.ModifyTagsByMessageIDAndAccount(
			messageID, s.accountName(), nil, tagMapping.AddTags); err != nil {
			slog.Warn("remove tags failed", "module", "SYNC", "uid", uid, "err", err)
		}
		return
	}

	// No tag mapping for this folder — delete the message
	slog.Debug("Deleting message removed from untagged folder", "module", "SYNC", // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		"uid", uid, "message_id", messageID, "folder", mailboxName)
	if err := s.store.DeleteByMessageIDAndAccount(messageID, s.accountName()); err != nil {
		slog.Warn("store delete failed", "module", "SYNC", "uid", uid, "err", err)
	}
}

// dedupUnsyncedUIDs fetches envelopes for unsynced UIDs and checks each one
// against the store. Already-known messages get their tags updated in-place
// (they were moved from another folder). Returns the list of UIDs that still
// need to be downloaded. In dry-run mode or when envelopes can't be fetched,
// all unsynced UIDs are returned unchanged.
func (s *Syncer) dedupUnsyncedUIDs(mailboxName string, mboxState *MailboxState, unsyncedUIDs []uint32, result *MailboxResult) []uint32 {
	if s.options.DryRun || len(unsyncedUIDs) == 0 {
		return unsyncedUIDs
	}

	slog.Debug("Checking for duplicates among unsynced UIDs", "module", "SYNC", "count", len(unsyncedUIDs))

	envelopes, err := s.client.FetchEnvelopes(unsyncedUIDs)
	if err != nil {
		slog.Debug("Failed to fetch envelopes for dedup", "module", "SYNC", "err", err)
		return unsyncedUIDs
	}

	// Store ALL Message-IDs from envelopes now, so ensureMessageIDMapping
	// in syncFlags doesn't re-fetch them from the server
	for uid, messageID := range envelopes {
		if messageID != "" {
			mboxState.SetMessageID(uid, messageID)
		}
	}

	// Get folder tag mapping for this mailbox.
	// For Gmail All Mail, skip folder mapping — labels are synced
	// via syncGmailLabels instead (the Archive mapping would
	// incorrectly strip inbox tags).
	var tagMapping *FolderTagMapping
	if !s.isGmailAllMail(mailboxName) {
		tagMapping = s.getFolderTagMapping(mailboxName)
	}

	var toDownload []uint32
	for _, uid := range unsyncedUIDs {
		if s.dedupOneUID(uid, mailboxName, mboxState, envelopes, tagMapping, result) {
			continue
		}
		toDownload = append(toDownload, uid)
	}

	if result.DeduplicatedMsgs > 0 {
		fmt.Fprintf(s.output, "  ~ %s: %d deduplicated\n", mailboxName, result.DeduplicatedMsgs)
	}
	return toDownload
}

// dedupOneUID handles deduplication for a single UID. Returns true if the
// message was deduplicated (already existed locally, tags updated in place),
// or false if the caller needs to download it.
func (s *Syncer) dedupOneUID(uid uint32, mailboxName string, mboxState *MailboxState, envelopes map[uint32]string, tagMapping *FolderTagMapping, result *MailboxResult) bool {
	messageID, hasID := envelopes[uid]
	if !hasID || messageID == "" {
		return false
	}

	exists, err := s.store.MessageExistsForAccount(messageID, s.accountName())
	if err != nil {
		slog.Debug("Failed to check message existence", "module", "SYNC", "message_id", messageID, "err", err)
		return false
	}
	if !exists {
		return false
	}

	// Message exists locally — update tags instead of downloading
	slog.Debug("Message already exists, updating tags", "module", "SYNC", "uid", uid, "message_id", messageID)
	s.applyDedupTags(messageID, mailboxName, tagMapping)

	// Update mailbox and UID to reflect the message's current server folder
	if err := s.store.UpdateMailbox(messageID, s.accountName(), mailboxName, uid); err != nil {
		slog.Debug("Failed to update mailbox", "module", "SYNC", "message_id", messageID, "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
	}

	mboxState.AddSyncedUID(uid)
	mboxState.SetMessageID(uid, messageID)
	result.DeduplicatedMsgs++
	return true
}

// applyDedupTags updates the tags of an existing message that was found in a
// new folder. If the folder has a SPECIAL-USE tag mapping, those tags are
// added/removed. For custom folders, the inbox tag is stripped since the
// message was moved out of INBOX.
func (s *Syncer) applyDedupTags(messageID, mailboxName string, tagMapping *FolderTagMapping) {
	if tagMapping != nil {
		addTags := s.filterConflictingTags(messageID, tagMapping.AddTags)
		if len(addTags) == 0 && len(tagMapping.RemoveTags) == 0 {
			return
		}
		if err := s.store.ModifyTagsByMessageIDAndAccount(messageID, s.accountName(), addTags, tagMapping.RemoveTags); err != nil {
			slog.Debug("Failed to update tags", "module", "SYNC", "message_id", messageID, "err", err)
		}
		return
	}

	// Custom folder with no special-use mapping — remove inbox tag since
	// the message was moved out of INBOX.
	if strings.EqualFold(mailboxName, "INBOX") {
		return
	}
	if err := s.store.ModifyTagsByMessageIDAndAccount(messageID, s.accountName(), nil, []string{"inbox"}); err != nil {
		slog.Debug("Failed to remove inbox tag", "module", "SYNC", "message_id", messageID, "err", err)
	}
}

// isConnectionError checks if an error indicates a lost connection
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "connection closed") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "EOF") ||
		strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "use of closed network connection")
}

// ensureMessageIDMapping builds the UID<->MessageID mapping for all UIDs on server
// This is called once per mailbox and cached in state for future syncs
func (s *Syncer) ensureMessageIDMapping(mailboxName string, mboxState *MailboxState, allUIDs []uint32) error {
	// Check which UIDs are missing from mapping
	missingUIDs := mboxState.GetMissingMappingUIDs(allUIDs)

	if len(missingUIDs) == 0 {
		slog.Debug("All UIDs already mapped", "module", "SYNC", "count", len(allUIDs))
		return nil // All mapped
	}

	slog.Debug("Fetching Message-IDs for mapping", "module", "SYNC", "missing", len(missingUIDs), "total", len(allUIDs))

	// Fetch ENVELOPEs for missing UIDs (in batches)
	envelopes, err := s.client.FetchEnvelopes(missingUIDs)
	if err != nil {
		return fmt.Errorf("failed to fetch envelopes: %w", err)
	}

	// Store mappings
	mappedCount := 0
	for uid, messageID := range envelopes {
		if messageID != "" {
			mboxState.SetMessageID(uid, messageID)
			mappedCount++
		}
	}

	slog.Debug("Mapped new UIDs", "module", "SYNC", "count", mappedCount)
	return nil
}
