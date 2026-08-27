package syncengine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/imap"
	"github.com/julion2/durian/cli/internal/store"
)

// defaultBatchLimit caps message bodies per FetchMessages call when the caller
// does not set Options.BatchLimit.
const (
	defaultBatchLimit      = 200
	flagFetchBatchSize     = 500
	maxFullScanRowsPerSync = 1000
	maxPendingFlagRefs     = 1000
	maxPendingFlagRefBytes = 256 * 1024
)

// Mode defines the sync direction (mirror of imap.SyncMode).
type Mode int

const (
	// Bidirectional syncs both directions (default).
	Bidirectional Mode = iota
	// DownloadOnly only downloads server state to the local store.
	DownloadOnly
	// UploadOnly only uploads local changes to the server.
	UploadOnly
)

// Options configures an Engine.
type Options struct {
	// Store is the SQLite store (required).
	Store *store.DB
	// Cursors persists per-folder sync cursors (required).
	Cursors CursorStore
	// Account is the account identifier (the store's account column).
	Account string
	// BatchLimit caps messages per FetchMessages call; <=0 means default (200).
	BatchLimit int
	// MaxPerFolder caps ordinary initial/delta message fetches per folder per run
	// (newest first), mirroring the legacy syncer's GetIMAPMaxMessages. Zero is
	// unlimited. Authoritative state-expiry recovery deliberately bypasses the
	// cap because reconciliation cannot safely use a partial remote ID set.
	MaxPerFolder int
	// Mode is the sync direction.
	Mode Mode
	// Ingest configures message ingestion (rules, groups, indexed headers).
	Ingest IngestOptions
	// Folders optionally restricts the sync to these folders; empty means all
	// folders that FetchFolders reports. Each entry is matched against the
	// backend's Folder.Name, its Folder.Display, and its special-use Role — so
	// the provider-neutral "INBOX" selects an IMAP mailbox named INBOX and a
	// Graph folder whose id is opaque and whose display name is localized
	// ("Posteingang"). A filter that selects no folder at all is an error, not
	// an empty success.
	Folders []string
	// DryRun logs what would happen without writing to the store or advancing
	// cursors.
	DryRun bool
	// NoFlags skips the per-folder flag reconciliation pass entirely (parity
	// with the legacy --no-flags). Messages still get their flag-derived tags
	// at ingest time; only the three-way upload/download pass is disabled.
	NoFlags bool
	// OnProgress, if set, is called after each fetched page with the running
	// count of messages ingested this run — for a live progress line during a
	// large full sync. Must be safe to call from the sync goroutine.
	OnProgress func(count int)
	// Timeout bounds an ordinary sync pass. RecoveryTimeout may extend that
	// deadline after a backend enters an authoritative replacement snapshot.
	// Zero leaves the corresponding mode unbounded (used by explicit CLI syncs).
	Timeout         time.Duration
	RecoveryTimeout time.Duration
}

// Result aggregates the outcome of one Engine.Sync run.
type Result struct {
	// Folders is the number of folders processed.
	Folders int
	// New is the number of genuinely new messages ingested this run.
	New int
	// Deduplicated is the number of messages that already existed locally and
	// were updated in place (e.g. re-delivered by a delta after a flag change).
	Deduplicated int
	// Deleted is the number of server-side deletions applied locally.
	Deleted int
	// Moved is the number of local archive/delete actions uploaded to the
	// server (INBOX messages moved to Archive/Trash).
	Moved int
	// Errors collects per-folder and per-message errors; the sync continues
	// past them (like the legacy syncer continues past a failed mailbox).
	Errors []error
	// NewMessageIDs are the Message-IDs of genuinely new arrivals in inbox-role
	// folders, in ingest order. Only inbox-role folders contribute: a message
	// appearing in Sent or Archive is not an arrival the user wants to hear
	// about. Callers use this to raise new-mail notifications, which is why it
	// is provider-neutral state on the engine rather than something derived
	// from IMAP UIDNEXT. Empty in dry-run.
	NewMessageIDs []string
}

// Engine drives a backend.Backend: folder discovery, cursor-paged incremental
// fetch, ingest, deletions, and a per-folder three-way flag pass.
type Engine struct {
	opts Options
}

// New creates an Engine. Options.Store and Options.Cursors are required.
func New(opts Options) *Engine {
	if opts.BatchLimit <= 0 {
		opts.BatchLimit = defaultBatchLimit
	}
	return &Engine{opts: opts}
}

// Sync runs one full sync pass against the backend. Per-folder errors are
// collected in Result.Errors without aborting the run; the returned error is
// non-nil only for fatal conditions (folder listing failed, context canceled,
// missing options).
func (e *Engine) Sync(ctx context.Context, b backend.Backend) (*Result, error) {
	if e.opts.Store == nil {
		return nil, fmt.Errorf("syncengine: Options.Store is required")
	}
	if e.opts.Cursors == nil {
		return nil, fmt.Errorf("syncengine: Options.Cursors is required")
	}

	// A label backend (Gmail/JMAP) mirrors Message.Labels to tags instead of using the
	// folder-role mapping; carry the capability into ingest.
	e.opts.Ingest.LabelsAsTags = b.Capabilities().LabelsAreTags

	deadline := time.Time{}
	if e.opts.Timeout > 0 {
		deadline = time.Now().Add(e.opts.Timeout)
	}
	requestCtx, cancel := contextWithDeadline(ctx, deadline)
	folders, err := b.FetchFolders(requestCtx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("fetch folders: %w", err)
	}

	result := &Result{}
	for _, folder := range folders {
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("sync canceled: %w", err)
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return result, context.DeadlineExceeded
		}
		if !folder.Selectable {
			continue
		}
		if !e.folderSelected(folder) {
			continue
		}

		result.Folders++
		state, err := e.opts.Cursors.GetState(e.opts.Account, folder.Name)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("folder %s: load cursor state: %w", folder.Name, err))
			continue
		}
		// UploadOnly skips the download/ingest pass entirely — used to push a
		// backlog of local changes (folder moves, flag changes) WITHOUT
		// re-fetching the server's copy, which would re-add role tags like
		// "inbox" to messages the user archived but that haven't moved on the
		// server yet, defeating the upload we're about to do.
		var folderSync *folderSyncResult
		if e.opts.Mode != UploadOnly {
			folderSync, err = e.syncFolder(ctx, b, folder, state, result, deadline)
			if err != nil {
				slog.Warn("Folder sync failed, continuing", "module", "SYNCENGINE",
					"account", e.opts.Account, "folder", folder.Name, "err", err)
				result.Errors = append(result.Errors, fmt.Errorf("folder %s: %w", folder.Name, err))
				continue
			}
		}
		var deltaFlags map[string]backend.Flags
		folderDeadline := deadline
		if folderSync != nil {
			deltaFlags = folderSync.deltaFlags
			if folderSync.deadline.After(folderDeadline) {
				folderDeadline = folderSync.deadline
			}
		}
		folderCtx, cancel := contextWithDeadline(ctx, folderDeadline)
		flagResult := e.reconcileFolderFlags(folderCtx, b, folder, deltaFlags, state.PendingFlags, result)
		cancel()
		if e.opts.DryRun {
			continue
		}

		baseState := state
		if folderSync != nil {
			baseState = folderSync.baseState
		}
		next := baseState
		if folderSync != nil {
			next.Cursor = folderSync.cursor
		}
		next.PendingFlags.Refs = flagResult.pendingFlags.Refs
		next.PendingFlags.FullScan = flagResult.pendingFlags.FullScan
		next.PendingFlags.ScanAfterID = flagResult.pendingFlags.ScanAfterID
		if e.opts.Mode == UploadOnly {
			// Upload-only passes cannot consume queued server-side work. They may
			// upload local changes, but the next download pass must still retry
			// every pending ref and retain the replacement replay marker.
			next.PendingFlags = mergePendingFlags(state.PendingFlags, flagResult.pendingFlags)
		} else if folderSync != nil && folderSync.fullSnapshot && flagResult.failed() {
			if err := ctx.Err(); err != nil {
				return result, fmt.Errorf("sync canceled before cursor persistence: %w", err)
			}
			if baseState.PendingFlags.ReplayCount == 0 {
				next.Cursor = baseState.Cursor
				next.PendingFlags.ReplayCount = 1
			} else {
				// One replay has already refreshed the replacement snapshot. Advance
				// now while retaining unresolved refs, conclusively ending this
				// replacement episode instead of looping on provider tokens.
				next.PendingFlags.ReplayCount = 0
			}
		} else if flagResult.reconciled {
			// A complete flag pass reconciles all queued work, regardless of
			// whether this run downloaded a delta.
			next.PendingFlags.ReplayCount = 0
		}
		if !folderStateEqual(state, next) {
			if err := e.opts.Cursors.Commit(e.opts.Account, folder.Name, next.Cursor, next.PendingFlags); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("folder %s: persist cursor state: %w", folder.Name, err))
			}
		}
	}

	// An explicit folder filter that selected nothing is a caller bug (a typo, or
	// a name that does not exist on this provider), not a healthy quiet sync.
	// Reporting success here is indistinguishable from "nothing changed", which
	// is how a mailbox filter that matched no Graph folder hid behind a
	// twice-a-minute "sync completed successfully" for days.
	if len(e.opts.Folders) > 0 && result.Folders == 0 {
		available := make([]string, 0, len(folders))
		for _, f := range folders {
			available = append(available, f.Display)
		}
		return result, fmt.Errorf("folder filter %v matched no folder on account %s (available: %s)",
			e.opts.Folders, e.opts.Account, strings.Join(available, ", "))
	}

	if !deadline.IsZero() && time.Now().After(deadline) {
		return result, context.DeadlineExceeded
	}

	// Upload local archive/delete actions (INBOX messages that lost the "inbox"
	// tag) to the server. Runs after downloads so it sees the freshest folders.
	// Folder backends move between mailboxes; a LabelsAreTags backend (Gmail/JMAP) has
	// no folders, so the label-upload pass handles the same archive/delete intent
	// (and arbitrary label changes) by pushing label diffs instead. Each self-
	// gates, so exactly one does work for a given backend.
	uploadCtx, cancel := contextWithDeadline(ctx, deadline)
	e.uploadFolderMoves(uploadCtx, b, folders, result)
	e.uploadLabelChanges(uploadCtx, b, result)
	cancel()

	slog.Info("Sync complete", "module", "SYNCENGINE", "account", e.opts.Account, // encgrep:allow account identifier (config name) and counts, not an encrypted column
		"folders", result.Folders, "new", result.New, "deleted", result.Deleted,
		"moved", result.Moved, "errors", len(result.Errors), "dry_run", e.opts.DryRun)
	return result, nil
}

// isRetryableStoreError reports whether a failed ingest is worth retrying on
// the next pass. SQLite reports write contention as "database is locked" —
// which happens because the daemon's watchers and a `durian sync` process write
// this file from separate processes, where the driver's single-connection
// setting offers no protection. Those succeed on a retry; a malformed message
// or a constraint violation never will, so everything else is treated as
// permanent and skipped rather than blocking the folder's cursor.
func isRetryableStoreError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "SQLITE_BUSY")
}

// folderSelected reports whether the folder passes the Options.Folders filter.
//
// A filter entry matches the provider's own folder identifier, its (possibly
// localized) display name, or its special-use role. The role match is what
// makes a caller-supplied "INBOX" portable: IMAP names the mailbox INBOX, but
// Graph identifies folders by opaque id and displays them in the mailbox's
// language, so neither of the first two matches on a German M365 tenant.
func (e *Engine) folderSelected(folder backend.Folder) bool {
	if len(e.opts.Folders) == 0 {
		return true
	}
	for _, want := range e.opts.Folders {
		if strings.EqualFold(want, folder.Name) || strings.EqualFold(want, folder.Display) {
			return true
		}
		// "INBOX" is the historical spelling of the inbox role; every other
		// role matches under its own name ("archive", "sent", ...).
		if folder.Role != backend.RoleNone && strings.EqualFold(want, string(folder.Role)) {
			return true
		}
		if folder.Role == backend.RoleInbox && strings.EqualFold(want, "INBOX") {
			return true
		}
	}
	return false
}

// syncFolder pages through one folder's changes until the backend reports no
// more. It returns the final cursor and this run's flag changes so Sync can
// atomically persist the cursor with any work reconciliation leaves pending.
type folderSyncResult struct {
	deltaFlags   map[string]backend.Flags
	cursor       backend.Cursor
	fullSnapshot bool
	deadline     time.Time
	baseState    FolderState
}

func (e *Engine) syncFolder(ctx context.Context, b backend.Backend, folder backend.Folder, state FolderState, result *Result, deadline time.Time) (*folderSyncResult, error) {
	cursor := state.Cursor
	baseState := state
	// deltaFlags records the server flags carried by messages that appeared in
	// this run's delta, keyed by RemoteRef.ID. For a backend whose delta reports
	// flag changes, this is the complete set of server-side flag changes, so the
	// flag pass reconciles from it instead of polling every message.
	deltaFlags := make(map[string]backend.Flags)

	// sessionRefs maps RemoteRef.ID -> Message-ID for messages ingested in THIS
	// run of this folder. It is only a fallback: the backend resolves most
	// deletions to a durable Message-ID itself (Deletion.MessageID), so this
	// covers the rare same-run arrive-then-delete case. A deletion the backend
	// cannot resolve and that was not seen this run is logged and skipped, which
	// is safe — the row lingers rather than risking deleting the wrong message.
	sessionRefs := make(map[string]string)
	// snapshotRefs accumulates the authoritative refs from a replacement full
	// snapshot. It is separate from sessionRefs because a malformed current
	// message must not make the engine delete an older local copy.
	snapshotRefs := make(map[string]struct{})
	snapshotUnavailable := make(map[string]struct{})
	snapshotSkipHydration := make(map[string]struct{})
	fullSnapshot := false
	snapshotModeSet := false

	// fetched counts messages pulled this run, to enforce MaxPerFolder.
	fetched := 0
	// ingestFailed marks that at least one message in the current batch could
	// not be stored, which makes this batch's cursor unsafe to persist.
	ingestFailed := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("canceled: %w", err)
		}

		fetchCtx, cancel := contextWithDeadline(ctx, deadline)
		res, err := b.FetchMessages(fetchCtx, folder.Name, cursor, e.opts.BatchLimit)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("fetch messages: %w", err)
		}
		if snapshotModeSet && fullSnapshot && !res.FullSnapshot {
			return nil, errors.New("backend switched from replacement-snapshot back to delta pages in one fetch sequence")
		}
		if snapshotModeSet && !fullSnapshot && res.FullSnapshot {
			// A delta page token may expire between pages. The backend then
			// restarts an authoritative enumeration from its first page. Earlier
			// delta changes remain safely ingested, while only pages from this
			// restart contribute to snapshot presence and cursor persistence.
			fullSnapshot = true
			if e.opts.RecoveryTimeout > 0 {
				deadline = time.Now().Add(e.opts.RecoveryTimeout)
			}
		}
		if !snapshotModeSet {
			fullSnapshot = res.FullSnapshot
			snapshotModeSet = true
			if fullSnapshot && e.opts.RecoveryTimeout > 0 {
				deadline = time.Now().Add(e.opts.RecoveryTimeout)
			}
		}
		if res.FullSnapshot {
			for _, ref := range res.Present {
				snapshotRefs[ref.ID] = struct{}{}
			}
			for _, ref := range res.Unavailable {
				if _, present := snapshotRefs[ref.ID]; !present {
					return nil, fmt.Errorf("backend reported unavailable ref %q outside snapshot presence", ref.ID)
				}
				snapshotUnavailable[ref.ID] = struct{}{}
			}
		}

		for _, msg := range res.Messages {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("canceled: %w", err)
			}
			// Record the server flags the delta carried, whether or not the
			// message is new: a re-appearing message signals a server flag change.
			deltaFlags[msg.Ref.ID] = msg.Flags
			if e.opts.DryRun {
				slog.Debug("[dry-run] Would ingest message", "module", "SYNCENGINE",
					"folder", folder.Name, "ref", msg.Ref.ID, "message_id", msg.MessageID)
				result.New++
				if res.FullSnapshot {
					snapshotSkipHydration[msg.Ref.ID] = struct{}{}
				}
				continue
			}
			messageID, created, err := Ingest(e.opts.Store, msg, folder.Name, folder.Role, e.opts.Ingest)
			if err != nil {
				slog.Warn("Ingest failed", "module", "SYNCENGINE",
					"folder", folder.Name, "ref", msg.Ref.ID, "err", err)
				result.Errors = append(result.Errors, fmt.Errorf("ingest %s/%s: %w", folder.Name, msg.Ref.ID, err))
				// Only a retryable failure may hold the cursor back. A message
				// that cannot be stored for a permanent reason would fail
				// identically forever, and holding the cursor for it re-downloads
				// the whole folder on every run without ever making progress.
				if isRetryableStoreError(err) {
					ingestFailed = true
				} else if res.FullSnapshot {
					// A full-body snapshot already made its one ingestion attempt.
					// Mark a permanently malformed item so hydration can skip it
					// when absent locally while preserving any older local copy.
					snapshotSkipHydration[msg.Ref.ID] = struct{}{}
				}
				continue
			}
			sessionRefs[msg.Ref.ID] = messageID
			// A re-delivered message (e.g. a flag change surfaced by the delta)
			// is an update, not a new arrival — count the two separately.
			if created {
				result.New++
				// Only inbox arrivals are worth notifying about; a message
				// ingested into Sent is the user's own, and Archive/Junk are
				// not events they asked to be interrupted for.
				if folder.Role == backend.RoleInbox || (b.Capabilities().LabelsAreTags && tagsContain(msg.Labels, "inbox")) {
					result.NewMessageIDs = append(result.NewMessageIDs, messageID)
				}
			} else {
				result.Deduplicated++
			}
		}

		for _, del := range res.Deleted {
			if res.FullSnapshot {
				delete(snapshotRefs, del.Ref.ID)
				delete(snapshotUnavailable, del.Ref.ID)
				delete(snapshotSkipHydration, del.Ref.ID)
				delete(deltaFlags, del.Ref.ID)
			}
			if e.handleDeleted(folder, del, sessionRefs, result) {
				result.Deleted++
			}
		}

		// A delta cursor is a promise that everything before it is stored. If
		// any message in this batch failed to ingest for a retryable reason (for
		// example, a locked database), advancing past it would drop that message:
		// the next delta starts after it and the server never mentions it
		// again. Stop the folder here instead and let the next pass refetch
		// the same batch — ingest is idempotent, so replaying it is free.
		if ingestFailed {
			return nil, fmt.Errorf("cursor held back: a message in this batch could not be stored yet")
		}

		// Before requesting another ordinary delta page, atomically checkpoint
		// the cursor with all unresolved work that cursor crossed. This prevents
		// a later-page failure from replaying this page's server tags over local
		// mutations made between runs. The explicit queue is bounded: after its
		// size limit it becomes a compact lossless full-scan continuation marker.
		if res.HasMore && !fullSnapshot && !e.opts.DryRun {
			pageFlags := make(map[string]backend.Flags, len(res.Messages))
			for _, msg := range res.Messages {
				pageFlags[msg.Ref.ID] = msg.Flags
			}
			baseState.Cursor = res.Cursor
			baseState.PendingFlags = mergePendingFlags(baseState.PendingFlags, pendingFlagsFromMap(pageFlags))
			if err := e.opts.Cursors.Commit(e.opts.Account, folder.Name, baseState.Cursor, baseState.PendingFlags); err != nil {
				return nil, fmt.Errorf("checkpoint delta page: %w", err)
			}
		}

		fetched += len(res.Messages)
		if e.opts.OnProgress != nil {
			// Total processed, not just new: a legacy->engine migration re-ingests
			// existing messages (counted as deduplicated), which would otherwise
			// leave the progress stuck at 0.
			e.opts.OnProgress(result.New + result.Deduplicated)
		}

		if !res.HasMore {
			if fullSnapshot {
				recoveryCtx, cancel := contextWithDeadline(ctx, deadline)
				err := e.hydrateFullSnapshot(recoveryCtx, b, folder, snapshotRefs, snapshotUnavailable, snapshotSkipHydration, deltaFlags, result)
				cancel()
				if err != nil {
					return nil, err
				}
			}
			if fullSnapshot && !e.opts.DryRun {
				errorsBefore := len(result.Errors)
				e.reconcileFullSnapshot(folder, snapshotRefs, result)
				if len(result.Errors) > errorsBefore {
					return nil, errors.New("full-snapshot reconciliation failed")
				}
			}
			return &folderSyncResult{
				deltaFlags: deltaFlags, cursor: res.Cursor,
				fullSnapshot: fullSnapshot, deadline: deadline, baseState: baseState,
			}, nil
		}
		// Stop at the per-folder cap (newest-first), so a first sync of a large
		// folder does not page its entire history — parity with the legacy
		// syncer's GetIMAPMaxMessages.
		// A replacement snapshot must finish in this run: intermediate cursors
		// are deliberately not persisted, and reconciliation needs Present refs
		// from every page. State-expiry recovery is rare and correctness takes
		// precedence over the normal first-sync cap.
		if e.opts.MaxPerFolder > 0 && fetched >= e.opts.MaxPerFolder && !fullSnapshot {
			slog.Debug("Reached per-folder message cap, stopping", "module", "SYNCENGINE", // encgrep:allow folder name and cap counts are operational sync metadata, not message content
				"folder", folder.Name, "cap", e.opts.MaxPerFolder, "fetched", fetched)
			return &folderSyncResult{deltaFlags: deltaFlags, cursor: res.Cursor, deadline: deadline, baseState: baseState}, nil
		}
		// Defensive guard: a backend that reports HasMore without changing
		// anything would loop forever; bail out instead.
		if len(res.Messages) == 0 && len(res.Deleted) == 0 && bytes.Equal(cursor, res.Cursor) {
			return nil, fmt.Errorf("backend reported HasMore without progress (cursor unchanged, no changes)")
		}
		cursor = res.Cursor
	}
}

func contextWithDeadline(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	if deadline.IsZero() {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, deadline)
}

func (e *Engine) hydrateFullSnapshot(ctx context.Context, b backend.Backend, folder backend.Folder, present, unavailable, skipHydration map[string]struct{}, deltaFlags map[string]backend.Flags, result *Result) error {
	rows, err := e.opts.Store.GetFolderFlagState(e.opts.Account, folder.Name)
	if err != nil {
		return fmt.Errorf("load full-snapshot hydration state for %s: %w", folder.Name, err)
	}
	existing := make(map[string]struct{}, len(rows))
	messageIDs := make(map[string]string, len(rows))
	refsByMessageID := make(map[string]string, len(rows))
	for _, row := range rows {
		existing[row.RemoteRef] = struct{}{}
		messageIDs[row.RemoteRef] = row.MessageID
		refsByMessageID[row.MessageID] = row.RemoteRef
	}
	missing := make([]backend.RemoteRef, 0)
	current := make([]backend.RemoteRef, 0, len(existing))
	for id := range present {
		if _, ok := existing[id]; ok {
			current = append(current, backend.RemoteRef{Folder: folder.Name, ID: id})
		} else if _, skip := skipHydration[id]; skip {
			delete(present, id)
			delete(deltaFlags, id)
		} else if _, inaccessible := unavailable[id]; !inaccessible {
			missing = append(missing, backend.RemoteRef{Folder: folder.Name, ID: id})
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].ID < missing[j].ID })
	sort.Slice(current, func(i, j int) bool { return current[i].ID < current[j].ID })
	if e.opts.DryRun {
		// Dry-run does not populate the local read model, so every snapshot ref
		// would otherwise look absent and metadata-only backends would fail the
		// hydration contract. Report projected arrivals without downloading
		// bodies; reconciliation and cursor persistence are already disabled.
		result.New += len(missing)
		return nil
	}
	hydrator, ok := b.(backend.SnapshotHydrator)
	if !ok {
		if len(missing) > 0 {
			slog.Warn("Snapshot refs are absent locally and backend cannot hydrate them, skipping refs", "module", "SYNCENGINE",
				"folder", folder.Name, "count", len(missing)) // encgrep:allow folder and count are operational sync metadata
			removeMissingSnapshotRefs(present, deltaFlags, missing)
		}
		return nil
	}
	batchSize := e.opts.BatchLimit
	if batchSize <= 0 {
		batchSize = defaultBatchLimit
	}
	for start := 0; start < len(current); start += batchSize {
		end := min(start+batchSize, len(current))
		batch := current[start:end]
		hydrated, err := hydrator.FetchSnapshotMetadata(ctx, batch)
		if err != nil {
			return fmt.Errorf("refresh full snapshot metadata: %w", err)
		}
		if err := validateSnapshotBatch(batch, hydrated); err != nil {
			return err
		}
		removeMissingSnapshotRefs(present, deltaFlags, hydrated.Missing)
		for _, msg := range hydrated.Messages {
			deltaFlags[msg.Ref.ID] = msg.Flags
			if e.opts.Ingest.LabelsAsTags && !e.opts.DryRun {
				if err := reconcileLabels(e.opts.Store, messageIDs[msg.Ref.ID], e.opts.Account, msg.Labels); err != nil {
					return fmt.Errorf("reconcile snapshot labels for %s: %w", msg.Ref.ID, err)
				}
			}
		}
	}
	for start := 0; start < len(missing); start += batchSize {
		end := min(start+batchSize, len(missing))
		batch := missing[start:end]
		hydrated, err := hydrator.FetchSnapshotMessages(ctx, batch)
		if err != nil {
			return fmt.Errorf("hydrate full snapshot: %w", err)
		}
		if err := validateSnapshotBatch(batch, hydrated); err != nil {
			return err
		}
		removeMissingSnapshotRefs(present, deltaFlags, hydrated.Missing)
		for _, msg := range hydrated.Messages {
			deltaFlags[msg.Ref.ID] = msg.Flags
			if e.opts.DryRun {
				result.New++
				continue
			}
			messageID, created, err := Ingest(e.opts.Store, msg, folder.Name, folder.Role, e.opts.Ingest)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("ingest hydrated snapshot message %s: %w", msg.Ref.ID, err))
				if isRetryableStoreError(err) {
					return fmt.Errorf("cursor held back: hydrated snapshot message %s could not be stored yet", msg.Ref.ID)
				}
				// A permanently malformed hydrated body would fail identically on
				// every replacement. Exclude it from authoritative presence so the
				// replacement can complete and a later provider change can retry it.
				// Preserve an older local copy carrying the same durable Message-ID.
				// Hydrators are expected to populate Message.MessageID, but not all
				// do (gmailbackend builds metadata-only messages without it), so fall
				// back to a tolerant header scan — strict parsing is what just failed.
				msgID := msg.MessageID
				if msgID == "" {
					msgID = messageIDFromRaw(msg.Raw)
				}
				if priorRef := refsByMessageID[msgID]; msgID != "" && priorRef != "" {
					present[priorRef] = struct{}{}
				}
				removeMissingSnapshotRefs(present, deltaFlags, []backend.RemoteRef{msg.Ref})
				continue
			}
			if created {
				result.New++
				if folder.Role == backend.RoleInbox || (b.Capabilities().LabelsAreTags && tagsContain(msg.Labels, "inbox")) {
					result.NewMessageIDs = append(result.NewMessageIDs, messageID)
				}
			} else {
				result.Deduplicated++
			}
		}
	}
	return nil
}

func validateSnapshotBatch(requested []backend.RemoteRef, batch backend.SnapshotBatch) error {
	expected := make(map[string]struct{}, len(requested))
	for _, ref := range requested {
		expected[ref.ID] = struct{}{}
	}
	for _, msg := range batch.Messages {
		if _, ok := expected[msg.Ref.ID]; !ok {
			return fmt.Errorf("snapshot hydrator returned unexpected ref %q", msg.Ref.ID)
		}
		delete(expected, msg.Ref.ID)
	}
	for _, ref := range batch.Missing {
		if _, ok := expected[ref.ID]; !ok {
			return fmt.Errorf("snapshot hydrator reported unexpected missing ref %q", ref.ID)
		}
		delete(expected, ref.ID)
	}
	if len(expected) > 0 {
		return fmt.Errorf("snapshot hydrator omitted %d requested messages", len(expected))
	}
	return nil
}

func removeMissingSnapshotRefs(present map[string]struct{}, flags map[string]backend.Flags, missing []backend.RemoteRef) {
	for _, ref := range missing {
		delete(present, ref.ID)
		delete(flags, ref.ID)
	}
}

// reconcileFullSnapshot removes local refs that are absent from an
// authoritative replacement snapshot. This is used only after every page has
// completed; a capped or interrupted backfill never performs reconciliation.
func (e *Engine) reconcileFullSnapshot(folder backend.Folder, present map[string]struct{}, result *Result) {
	rows, err := e.opts.Store.GetFolderFlagState(e.opts.Account, folder.Name)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("load full-snapshot state for %s: %w", folder.Name, err))
		return
	}
	for _, row := range rows {
		if _, ok := present[row.RemoteRef]; ok {
			continue
		}
		del := backend.Deletion{
			Ref:       backend.RemoteRef{Folder: folder.Name, ID: row.RemoteRef},
			MessageID: row.MessageID,
		}
		if e.handleDeleted(folder, del, nil, result) {
			result.Deleted++
		}
	}
}

// reconcileFolderFlags runs the three-way flag pass for one folder after its
// fetch loop completed. The engine owns the merge (a port of the legacy
// (*imap.Syncer).syncFlags three-way, cli/internal/imap/sync_flags.go): per
// message it compares the flag state implied by local tags and the current
// server flags (Backend.FetchFlags) against the store's last-synced baseline
// (synced_flags). Local changes vs the baseline are uploaded via ApplyFlags;
// server changes are downloaded into tags via ToTagOps; conflicts merge with
// local winning for Seen/Flagged/Answered and the server winning for
// Deleted/Completed. Keeping the merge here makes it provider-neutral: a
// backend only reports and applies flags, so cursors (e.g. a Graph deltaLink)
// never need to carry per-message baselines.
//
// Unlike the legacy syncer there is no "no baseline / first sync" branch:
// every engine-ingested message gets its initial baseline at ingest time (the
// server flags it arrived with), so a missing-baseline state cannot occur —
// an empty synced_flags string simply means "no flags set at last sync".
//
// New messages ingested this run already carry their flag tags and baseline,
// so the pass no-ops for them. The pass is skipped entirely in DryRun: it
// would otherwise write to the server, tags and baselines.
type flagReconcileResult struct {
	fetchFailed     bool
	reconcileFailed bool
	reconciled      bool
	pendingFlags    PendingFlags
}

func (r flagReconcileResult) failed() bool { return r.fetchFailed || r.reconcileFailed }

func (e *Engine) reconcileFolderFlags(ctx context.Context, b backend.Backend, folder backend.Folder, deltaFlags map[string]backend.Flags, pendingFlags PendingFlags, result *Result) flagReconcileResult {
	if e.opts.NoFlags || e.opts.DryRun {
		return flagReconcileResult{pendingFlags: pendingFlags}
	}
	if ctx.Err() != nil {
		return flagReconcileResult{
			reconcileFailed: true,
			pendingFlags:    mergePendingFlags(pendingFlags, pendingFlagsFromMap(deltaFlags)),
		}
	}
	reconcileFailed := false
	var localFailed []string

	upload := e.opts.Mode != DownloadOnly
	download := e.opts.Mode != UploadOnly

	rows, err := e.opts.Store.GetFolderFlagState(e.opts.Account, folder.Name)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("flag sync %s: load flag state: %w", folder.Name, err))
		return flagReconcileResult{
			reconcileFailed: true,
			pendingFlags:    mergePendingFlags(pendingFlags, pendingFlagsFromMap(deltaFlags)),
		}
	}
	if len(rows) == 0 {
		return flagReconcileResult{reconciled: true}
	}
	pendingSet := make(map[string]struct{}, len(pendingFlags.Refs))
	for _, ref := range pendingFlags.Refs {
		pendingSet[ref] = struct{}{}
	}

	// Choose which messages to fetch server flags for. A delta backend already
	// told us which messages changed server-side (they reappear in the delta),
	// so only those — plus any the user changed locally — are candidates; polling
	// every message was O(mailbox) and dominated a sync. FetchFlags stays the
	// source of truth, keeping the three-way merge below byte-identical; only the
	// ref set shrinks. A non-delta backend has no change feed, so it polls all.
	useDelta := b.Capabilities().FlagChangesInDelta
	answeredUnsupported := b.Capabilities().AnsweredUnsupported
	candidates := make([]store.FolderFlagRow, 0, min(len(rows), maxFullScanRowsPerSync))
	seeded := 0
	for i := range rows {
		row := rows[i]
		// A legacy-migrated row carries no flag baseline (the legacy syncer kept
		// it in its own state file; the engine upsert deliberately never writes
		// synced_flags). An empty baseline parses to "unread/unflagged", so a read
		// or flagged migrated message looks locally changed and becomes a false
		// upload candidate — turning the first engine sync of a whole migrated
		// mailbox into one FetchFlags request per message. Seed the baseline from
		// the stored server flags (adopting server state, like the legacy
		// first-sync branch). Only seed a NON-empty state: an empty seed is a
		// no-op, and skipping it would swallow a genuine local read/flag change
		// on a message whose baseline is legitimately empty. After seeding, fall
		// through to the normal candidate check against the seeded baseline.
		if row.SyncedFlags == "" {
			seededBaseline := joinFlags(imap.FlagState{Seen: row.IsSeen, Flagged: row.IsFlagged})
			if seededBaseline != "" {
				if err := e.opts.Store.SetSyncedFlags(row.MessageID, e.opts.Account, seededBaseline); err != nil {
					// Don't process this row with an unpersisted baseline: the
					// candidate check and the merge below must agree, and the
					// merge re-reads rows[i]. Skip; the next sync retries.
					result.Errors = append(result.Errors, fmt.Errorf("seed flag baseline for %s: %w", row.MessageID, err))
					reconcileFailed = true
					localFailed = append(localFailed, row.RemoteRef)
					continue
				}
				// Update BOTH the local copy (for the candidate check just below)
				// and the backing slice element (so the merge loop, which re-ranges
				// rows, reads the seeded baseline instead of the empty original).
				row.SyncedFlags = seededBaseline
				rows[i].SyncedFlags = seededBaseline
				seeded++
			}
		}
		_, pending := pendingSet[row.RemoteRef]
		if !useDelta && pendingFlags.FullScan && e.opts.Mode != UploadOnly && !pending && !e.flagCandidate(row, deltaFlags, upload) {
			continue
		}
		if useDelta && !pending && !e.flagCandidate(row, deltaFlags, upload) {
			continue
		}
		candidates = append(candidates, row)
	}

	// A compact full-scan marker replaces an oversized explicit queue. Resume it
	// by stable database row ID and cap the total rows considered in one sync;
	// each network request is capped separately below. Explicit delta/local work
	// is handled first. If that work alone exceeds the budget, restart the scan
	// from zero so advancing the provider cursor cannot lose the omitted refs.
	scanActive := pendingFlags.FullScan && e.opts.Mode != UploadOnly
	scanStart := pendingFlags.ScanAfterID
	scanAfter := scanStart
	scanIncomplete := false
	if e.opts.Mode != UploadOnly && len(candidates) > maxFullScanRowsPerSync {
		scanActive = true
		scanStart = 0
		scanAfter = 0
		candidates = candidates[:0]
	}
	if scanActive {
		selected := make(map[int64]struct{}, len(candidates))
		for _, row := range candidates {
			selected[row.RowID] = struct{}{}
		}
		for _, row := range rows {
			if row.RowID <= scanStart {
				continue
			}
			if _, ok := selected[row.RowID]; !ok {
				if len(candidates) >= maxFullScanRowsPerSync {
					scanIncomplete = true
					break
				}
				candidates = append(candidates, row)
				selected[row.RowID] = struct{}{}
			}
			scanAfter = row.RowID
		}
	}
	if seeded > 0 {
		slog.Info("Seeded flag baselines for migrated messages", "module", "SYNCENGINE", "folder", folder.Name, "count", seeded) // encgrep:allow folder name + count are operational metadata, not content
	}
	if len(candidates) == 0 {
		return flagReconcileResult{
			reconcileFailed: reconcileFailed,
			reconciled:      !reconcileFailed,
			pendingFlags:    pendingFlagsFromRefs(localFailed),
		}
	}

	unresolved := newPendingFlagAccumulator()
	uploaded, downloaded := 0, 0
	systemicFetchFailure := false
	for start := 0; start < len(candidates); start += flagFetchBatchSize {
		chunk := candidates[start:min(start+flagFetchBatchSize, len(candidates))]
		refs := make([]backend.RemoteRef, 0, len(chunk))
		for _, row := range chunk {
			refs = append(refs, backend.RemoteRef{Folder: folder.Name, ID: row.RemoteRef})
		}

		// fetchIncomplete distinguishes "the backend could not resolve this
		// ref" from "the backend positively reports this ref as dead". A
		// systemic failure stops further network requests in this folder; the
		// remaining batches are retained and can still use delta-carried flags.
		server := map[string]backend.Flags(nil)
		fetchIncomplete := systemicFetchFailure
		if !systemicFetchFailure {
			var err error
			server, err = b.FetchFlags(ctx, folder.Name, refs)
			fetchIncomplete = err != nil
			if err != nil {
				slog.Warn("Flag fetch failed, continuing", "module", "SYNCENGINE",
					"folder", folder.Name)
				result.Errors = append(result.Errors, fmt.Errorf("flag sync %s: %w", folder.Name, err))
				if !errors.Is(err, backend.ErrPartialFlags) || server == nil {
					server = nil
					systemicFetchFailure = true
				}
			}
		}
		if fetchIncomplete {
			for _, ref := range refs {
				if _, ok := server[ref.ID]; !ok {
					unresolved.add(ref.ID)
				}
			}
		}

		batchUploaded, batchDownloaded, batchFailed := e.reconcileFlagRows(
			ctx, b, folder, chunk, server, deltaFlags, fetchIncomplete,
			upload, download, answeredUnsupported, result,
		)
		uploaded += batchUploaded
		downloaded += batchDownloaded
		if len(batchFailed) > 0 {
			reconcileFailed = true
			localFailed = append(localFailed, batchFailed...)
		}
	}

	slog.Debug("Flag pass complete", "module", "SYNCENGINE",
		"folder", folder.Name, "uploaded", uploaded, "downloaded", downloaded)
	pendingWork := mergePendingFlags(unresolved.pendingFlags(), pendingFlagsFromRefs(localFailed))
	fetchFailed := unresolved.hasWork()
	failed := fetchFailed || reconcileFailed
	if scanActive && (failed || scanIncomplete) {
		if failed {
			scanAfter = scanStart
		}
		pendingWork = mergePendingFlags(pendingWork, PendingFlags{FullScan: true, ScanAfterID: scanAfter})
	}
	return flagReconcileResult{
		fetchFailed: fetchFailed, reconcileFailed: reconcileFailed,
		reconciled:   !failed && !scanIncomplete,
		pendingFlags: pendingWork,
	}
}

func (e *Engine) reconcileFlagRows(
	ctx context.Context,
	b backend.Backend,
	folder backend.Folder,
	rows []store.FolderFlagRow,
	server map[string]backend.Flags,
	deltaFlags map[string]backend.Flags,
	fetchIncomplete, upload, download, answeredUnsupported bool,
	result *Result,
) (uploaded, downloaded int, failed []string) {
	for _, row := range rows {
		serverFlags, ok := server[row.RemoteRef]
		if !ok {
			// Only when the fetch itself was incomplete: the delta already carried
			// this message's server flags, and the folder cursor has advanced past
			// the page, so no later pass re-selects the row (flagCandidate reads
			// deltaFlags, which is per-run). Falling back beats dropping the
			// server change permanently.
			if !fetchIncomplete {
				continue // Not a candidate, or the backend reports it as gone.
			}
			serverFlags, ok = deltaFlags[row.RemoteRef]
			if !ok {
				continue // Nothing the delta could vouch for either.
			}
		}

		local := imap.FlagStateFromTags(row.Tags)
		serverState := flagStateFromBackend(serverFlags)
		// The ORIGINAL stored baseline; both branches below deliberately compare
		// against it (not one the upload branch may have just advanced),
		// mirroring the legacy three-way's conflict detection.
		baseline := imap.FlagStateFromIMAP(splitFlags(row.SyncedFlags))

		if answeredUnsupported {
			// The backend can't persist \Answered (Gmail has no answered label),
			// so pin the server's Answered to the baseline. This keeps Answered
			// from ever driving a download that would strip a local "replied" tag.
			// The upload branch still advances the baseline's Answered from local
			// (the provider silently ignores the flag), so a replied message stays
			// answered instead of ping-ponging every sync.
			serverState.Answered = baseline.Answered
		}

		// Local changed vs the baseline: push the local-vs-server diff so the
		// server converges on local. DiffFlags only ever emits the user flags
		// ToIMAPFlags covers (Seen/Flagged/Answered/Deleted), never the
		// server-only $Completed keyword — same as the legacy upload path.
		// Note: imap.FlagStateFromTags never sets Deleted, so the legacy
		// copy-to-trash delete branch cannot fire on this path.
		if upload && imap.NeedsUpload(local, baseline) {
			ref := backend.RemoteRef{Folder: folder.Name, ID: row.RemoteRef}
			toAdd, toRemove := imap.DiffFlags(local, serverState)
			add := backendFlagsFromState(imap.FlagStateFromIMAP(toAdd))
			remove := backendFlagsFromState(imap.FlagStateFromIMAP(toRemove))
			if err := b.ApplyFlags(ctx, ref, add, remove); err != nil {
				// Continue with the remaining messages (legacy behavior); the
				// baseline stays put so the upload is retried next sync.
				slog.Warn("Flag upload failed", "module", "SYNCENGINE",
					"folder", folder.Name, "message_id", row.MessageID, "err", err)
				result.Errors = append(result.Errors, fmt.Errorf("flag upload for %s: %w", row.MessageID, err))
				continue
			} else {
				if err := e.opts.Store.SetSyncedFlags(row.MessageID, e.opts.Account, joinFlags(local)); err != nil {
					result.Errors = append(result.Errors, fmt.Errorf("flag baseline for %s: %w", row.MessageID, err))
				}
				uploaded++
			}
		}

		// Server changed vs the baseline: bring the change down. When both
		// sides changed (conflict), merge with local winning for
		// Seen/Flagged/Answered and the server winning for Deleted/Completed.
		if download && imap.NeedsDownload(serverState, baseline) {
			target := serverState
			if imap.NeedsUpload(local, baseline) {
				target = local.Merge(serverState)
			}
			if !target.Equal(local) {
				add, remove := target.ToTagOps()
				if err := e.opts.Store.ModifyTagsByMessageIDAndAccount(row.MessageID, e.opts.Account, add, remove); err != nil {
					result.Errors = append(result.Errors, fmt.Errorf("flag tags for %s: %w", row.MessageID, err))
					failed = append(failed, row.RemoteRef)
					continue // Baseline stays put so the download is retried next sync.
				}
			}
			if err := e.opts.Store.SetSyncedFlags(row.MessageID, e.opts.Account, joinFlags(target)); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("flag baseline for %s: %w", row.MessageID, err))
				failed = append(failed, row.RemoteRef)
			}
			downloaded++
		}
	}
	return uploaded, downloaded, failed
}

func folderStateEqual(a, b FolderState) bool {
	return bytes.Equal(a.Cursor, b.Cursor) &&
		a.PendingFlags.ReplayCount == b.PendingFlags.ReplayCount &&
		a.PendingFlags.FullScan == b.PendingFlags.FullScan &&
		a.PendingFlags.ScanAfterID == b.PendingFlags.ScanAfterID &&
		slices.Equal(a.PendingFlags.Refs, b.PendingFlags.Refs)
}

type pendingFlagAccumulator struct {
	refs        []string
	seen        map[string]struct{}
	refBytes    int
	fullScan    bool
	scanAfterID int64
	overflow    bool
}

func newPendingFlagAccumulator() *pendingFlagAccumulator {
	return &pendingFlagAccumulator{seen: make(map[string]struct{})}
}

func (a *pendingFlagAccumulator) add(ref string) {
	if a.overflow || ref == "" {
		return
	}
	if _, exists := a.seen[ref]; exists {
		return
	}
	if len(a.refs) >= maxPendingFlagRefs || a.refBytes+len(ref) > maxPendingFlagRefBytes {
		a.fullScan = true
		a.scanAfterID = 0
		a.overflow = true
		a.refs = nil
		a.seen = nil
		a.refBytes = 0
		return
	}
	a.seen[ref] = struct{}{}
	a.refs = append(a.refs, ref)
	a.refBytes += len(ref)
}

func (a *pendingFlagAccumulator) addPending(pending PendingFlags) {
	if pending.FullScan {
		if pending.ScanAfterID == 0 {
			a.fullScan = true
			a.scanAfterID = 0
			a.overflow = true
			a.refs = nil
			a.seen = nil
			a.refBytes = 0
			return
		}
		if !a.fullScan || pending.ScanAfterID < a.scanAfterID {
			a.scanAfterID = pending.ScanAfterID
		}
		a.fullScan = true
	}
	for _, ref := range pending.Refs {
		a.add(ref)
	}
}

func (a *pendingFlagAccumulator) hasWork() bool { return a.fullScan || len(a.refs) > 0 }

func (a *pendingFlagAccumulator) pendingFlags() PendingFlags {
	return PendingFlags{Refs: a.refs, FullScan: a.fullScan, ScanAfterID: a.scanAfterID}
}

func pendingFlagsFromRefs(refs []string) PendingFlags {
	acc := newPendingFlagAccumulator()
	for _, ref := range refs {
		acc.add(ref)
	}
	return acc.pendingFlags()
}

func pendingFlagsFromMap[V any](values map[string]V) PendingFlags {
	acc := newPendingFlagAccumulator()
	for ref := range values {
		acc.add(ref)
		if acc.fullScan {
			break
		}
	}
	if !acc.fullScan {
		sort.Strings(acc.refs)
	}
	return acc.pendingFlags()
}

func mergePendingFlags(groups ...PendingFlags) PendingFlags {
	acc := newPendingFlagAccumulator()
	replayCount := 0
	for _, pending := range groups {
		acc.addPending(pending)
		if pending.ReplayCount > replayCount {
			replayCount = pending.ReplayCount
		}
	}
	merged := acc.pendingFlags()
	merged.ReplayCount = replayCount
	return merged
}

// flagCandidate reports whether a delta backend still needs this row's server
// flags fetched: the message either changed server-side (it is in the delta's
// change set) or the user changed its flags locally (an upload is pending).
// Rows that are neither can be skipped, shrinking the flag fetch to O(changes).
func (e *Engine) flagCandidate(row store.FolderFlagRow, deltaFlags map[string]backend.Flags, upload bool) bool {
	if _, changedRemotely := deltaFlags[row.RemoteRef]; changedRemotely {
		return true
	}
	if !upload {
		return false
	}
	local := imap.FlagStateFromTags(row.Tags)
	baseline := imap.FlagStateFromIMAP(splitFlags(row.SyncedFlags))
	return imap.NeedsUpload(local, baseline)
}

// messageIDFromRaw extracts the Message-ID header from a raw RFC822 message
// with a tolerant line scan, returning "" when absent.
//
// It deliberately avoids mail.ReadMessage: the only caller is the
// replacement-snapshot preservation path, which runs precisely when strict
// parsing has already rejected the message. Folded continuation lines are
// honoured so a wrapped Message-ID still resolves. Angle brackets are trimmed
// to match how Ingest records the durable key.
func messageIDFromRaw(raw []byte) string {
	const header = "message-id:"
	for len(raw) > 0 {
		line := raw
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			line, raw = raw[:i], raw[i+1:]
		} else {
			raw = nil
		}
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			return "" // End of headers.
		}
		if len(line) < len(header) || !strings.EqualFold(string(line[:len(header)]), header) {
			continue
		}
		value := string(bytes.TrimSpace(line[len(header):]))
		// Unfold: continuation lines start with space or tab.
		for len(raw) > 0 && (raw[0] == ' ' || raw[0] == '\t') {
			cont := raw
			if i := bytes.IndexByte(raw, '\n'); i >= 0 {
				cont, raw = raw[:i], raw[i+1:]
			} else {
				raw = nil
			}
			value += " " + string(bytes.TrimSpace(bytes.TrimRight(cont, "\r")))
		}
		return strings.Trim(strings.TrimSpace(value), "<>")
	}
	return ""
}

// splitFlags splits a comma-joined IMAP flag string (the store's synced_flags
// baseline format); "" yields nil (no flags set at last sync).
func splitFlags(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// joinFlags renders a FlagState into the store's comma-joined baseline format.
// Unlike ToIMAPFlags (used for uploads, which must never push $Completed), the
// baseline INCLUDES $Completed so a server-side completed message round-trips
// and does not re-trigger a download every sync. FlagStateFromIMAP parses it
// back. Single source of truth for baseline serialization; ingest uses it too.
func joinFlags(f imap.FlagState) string {
	flags := f.ToIMAPFlags()
	if f.Completed {
		flags = append(flags, "$Completed")
	}
	return strings.Join(flags, ",")
}

// handleDeleted processes one handle the source no longer holds in the folder.
// Mirrors the legacy handleDeletedUID: for role folders (which have a tag
// mapping) only the folder's tags are removed — the message was most likely
// moved and its row must survive; for user folders (no mapping) the row is
// deleted, unless the store shows the message was since ingested from another
// folder (the Mailbox column moved on), in which case deleting would destroy a
// live message. Reports whether a local change was actually applied.
func (e *Engine) handleDeleted(folder backend.Folder, del backend.Deletion, sessionRefs map[string]string, result *Result) bool {
	// Prefer the durable Message-ID the backend resolved from its own map
	// (IMAP). Otherwise resolve the provider handle via the persisted remote_ref
	// (Graph deletions carry only the id). Finally fall back to a message
	// ingested earlier in THIS run. If nothing resolves, skip safely.
	messageID := del.MessageID
	if messageID == "" && del.Ref.ID != "" {
		if mid, err := e.opts.Store.GetMessageIDByRemoteRef(e.opts.Account, del.Ref.Folder, del.Ref.ID); err != nil {
			slog.Debug("remote_ref deletion lookup failed", "module", "SYNCENGINE",
				"folder", folder.Name, "ref", del.Ref.ID, "err", err)
		} else {
			messageID = mid
		}
	}
	if messageID == "" {
		messageID = sessionRefs[del.Ref.ID]
	}
	if messageID == "" {
		slog.Debug("No Message-ID for deleted ref, skipping", "module", "SYNCENGINE",
			"folder", folder.Name, "ref", del.Ref.ID)
		return false
	}
	delete(sessionRefs, del.Ref.ID)

	if e.opts.DryRun {
		slog.Debug("[dry-run] Would handle deletion", "module", "SYNCENGINE",
			"folder", folder.Name, "message_id", messageID)
		return true
	}

	if mapping := tagMappingForRole(folder.Role); mapping != nil && len(mapping.addTags) > 0 {
		slog.Debug("Removing folder tags for moved message", "module", "SYNCENGINE", // encgrep:allow folder name, tag names and Message-ID are operational sync metadata, not encrypted columns
			"folder", folder.Name, "message_id", messageID, "tags", mapping.addTags)
		if err := e.opts.Store.ModifyTagsByMessageIDAndAccount(messageID, e.opts.Account, nil, mapping.addTags); err != nil {
			slog.Warn("Remove folder tags failed", "module", "SYNCENGINE", "message_id", messageID, "err", err) // encgrep:allow Message-ID is a plaintext RFC822 header / stable key, not an encrypted column
			result.Errors = append(result.Errors, fmt.Errorf("untag deleted %s: %w", messageID, err))
			return false
		}
		return true
	}

	// User folder without tag mapping: delete the row — but not if the message
	// has since been ingested from a different folder in this same run (the
	// Mailbox column reflects the latest ingest).
	if existing, err := e.opts.Store.GetByMessageID(messageID); err == nil && existing != nil && existing.Mailbox != folder.Name {
		slog.Debug("Message moved to another folder, keeping row", "module", "SYNCENGINE", // encgrep:allow Message-ID and folder/mailbox names are operational sync metadata, not encrypted columns
			"message_id", messageID, "folder", folder.Name, "current_mailbox", existing.Mailbox)
		return false
	}

	slog.Debug("Deleting message removed from untagged folder", "module", "SYNCENGINE", // encgrep:allow folder name and Message-ID are operational sync metadata, not encrypted columns
		"folder", folder.Name, "message_id", messageID)
	if err := e.opts.Store.DeleteByMessageIDAndAccount(messageID, e.opts.Account); err != nil {
		slog.Warn("Store delete failed", "module", "SYNCENGINE", "message_id", messageID, "err", err)
		result.Errors = append(result.Errors, fmt.Errorf("delete %s: %w", messageID, err))
		return false
	}
	return true
}

// uploadFolderMoves pushes local archive/delete actions to the server: INBOX
// messages whose local tags no longer include "inbox" are moved to the Archive
// (or Trash) folder via Backend.Move. This is the upload counterpart to the
// download/flag passes — without it, archiving or deleting a message in the GUI
// never reaches the server on the engine path. Skipped in DownloadOnly mode and
// dry-run-safe (a dry run counts what would move without touching the server).
//
// Data-loss safety: after Move expunges the message from INBOX, the next INBOX
// delta reports it deleted — but INBOX is role-mapped, so handleDeleted only
// strips the "inbox" tag and keeps the row (it never deletes a role-mapped
// folder's message). The row survives locally under its new folder.
func (e *Engine) uploadFolderMoves(ctx context.Context, b backend.Backend, folders []backend.Folder, result *Result) {
	if e.opts.Mode == DownloadOnly {
		return
	}
	var inbox *backend.Folder
	for i := range folders {
		if folders[i].Role == backend.RoleInbox {
			inbox = &folders[i]
			break
		}
	}
	if inbox == nil {
		return
	}

	rows, err := e.opts.Store.GetFolderFlagState(e.opts.Account, inbox.Name)
	if err != nil {
		slog.Warn("Folder-move upload: load inbox state failed", "module", "SYNCENGINE",
			"account", e.opts.Account, "err", err)
		return
	}

	for _, r := range rows {
		if tagsContain(r.Tags, "inbox") {
			continue // still in inbox — nothing to upload
		}
		dest, ok := moveDestination(r.Tags, folders)
		if !ok {
			continue // no resolvable destination (e.g. no Archive folder) — leave in place
		}
		if e.opts.DryRun {
			slog.Debug("[dry-run] Would move message out of inbox", "module", "SYNCENGINE", // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
				"message_id", r.MessageID, "dest", dest)
			result.Moved++
			continue
		}
		ref := backend.RemoteRef{Folder: inbox.Name, ID: r.RemoteRef}
		if _, err := b.Move(ctx, ref, dest); err != nil {
			if errors.Is(err, backend.ErrRefGone) {
				// The message already left INBOX on the server (archived from
				// another client, or moved by an earlier run that then failed to
				// record the new mailbox). Retrying can never succeed — the ref
				// is permanently dead — and the row stays in the inbox folder
				// state, so without local reconciliation this same doomed move
				// is reattempted, and fails the sync, on every single run.
				//
				// Reconcile optimistically: point the row at dest, the mailbox
				// the local tags already asked for. That is where the message
				// most likely is (another client archiving it is the common
				// cause), and it is the one option that both breaks the retry
				// loop and keeps the message — deleting the row would lose a
				// message that is still sitting on the server. If the guess is
				// wrong, the next delta on the real folder re-ingests the row
				// and overwrites both mailbox and remote_ref, which stays stale
				// here (UpdateMailbox only resets the UID).
				slog.Info("Folder-move upload: message already gone from inbox", "module", "SYNCENGINE", // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
					"message_id", r.MessageID, "dest", dest)
				if err := e.opts.Store.UpdateMailbox(r.MessageID, e.opts.Account, dest, 0); err != nil {
					slog.Warn("Folder-move upload: reconcile gone message failed", "module", "SYNCENGINE", // encgrep:allow static message text only; message_id is plaintext and err carries no encrypted column
						"message_id", r.MessageID, "err", err)
				}
				continue
			}
			slog.Warn("Folder-move upload failed", "module", "SYNCENGINE", // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
				"message_id", r.MessageID, "dest", dest, "err", err)
			result.Errors = append(result.Errors, fmt.Errorf("move %s: %w", r.MessageID, err))
			continue
		}
		if err := e.opts.Store.UpdateMailbox(r.MessageID, e.opts.Account, dest, 0); err != nil {
			slog.Warn("Folder-move upload: update mailbox failed", "module", "SYNCENGINE", // encgrep:allow static message text mentions "mailbox"; only message_id (plaintext) and err are logged
				"message_id", r.MessageID, "err", err)
		}
		result.Moved++
		slog.Info("Moved message out of inbox", "module", "SYNCENGINE", // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
			"message_id", r.MessageID, "dest", dest)
	}
}

// uploadLabelChanges is the label-native counterpart to uploadFolderMoves for a
// LabelsAreTags backend (Gmail/JMAP): rather than relocating messages between
// folders, it pushes each message's local tag changes as label add/removes.
// Archiving (dropping the "inbox" tag) becomes a label removal; deleting adds
// the "trash" tag, which maps to Gmail's TRASH. It runs only when the backend
// implements backend.LabelWriter and reports LabelsAreTags, so folder backends
// are untouched. Skipped in DownloadOnly mode and dry-run-safe.
//
// The new baseline is deterministic — the message's local tags intersected with
// the label vocabulary — so Durian-local tags (rule tags, "ephemeral", ...) are
// never pushed and never enter the baseline, keeping it exactly the server's
// label truth for the next download reconcile.
func (e *Engine) uploadLabelChanges(ctx context.Context, b backend.Backend, result *Result) {
	if e.opts.Mode == DownloadOnly {
		return
	}
	lw, ok := b.(backend.LabelWriter)
	if !ok || !b.Capabilities().LabelsAreTags {
		return
	}

	vocabList, err := lw.LabelTags(ctx)
	if err != nil {
		slog.Warn("Label upload: load label vocabulary failed", "module", "SYNCENGINE",
			"account", e.opts.Account, "err", err)
		return
	}
	vocab := make(map[string]bool, len(vocabList))
	for _, t := range vocabList {
		vocab[t] = true
	}

	rows, err := e.opts.Store.GetLabelState(e.opts.Account)
	if err != nil {
		slog.Warn("Label upload: load label state failed", "module", "SYNCENGINE",
			"account", e.opts.Account, "err", err)
		return
	}

	seeded := 0
	for _, r := range rows {
		if err := ctx.Err(); err != nil {
			return
		}
		// A row with no label baseline (synced_labels defaulted to '' — e.g. a
		// message the legacy syncer ingested before the engine recorded label
		// baselines) would make EVERY current label tag look like a brand-new
		// local addition, firing a redundant messages.modify per message to
		// re-add labels the server already has. Seed the baseline from the
		// current server-derived label tags and skip the upload: those tags
		// already reflect what we last downloaded, so there is nothing local to
		// push. Mirrors the flag-baseline seeding in reconcileFolderFlags. A
		// genuine local change is picked up next sync against the seeded baseline.
		if r.SyncedLabels == "" {
			_, _, seedBaseline := diffLabels(r.Tags, nil, vocab)
			if len(seedBaseline) > 0 {
				if err := e.opts.Store.SetSyncedLabels(r.MessageID, e.opts.Account, strings.Join(seedBaseline, ",")); err != nil {
					result.Errors = append(result.Errors, fmt.Errorf("seed label baseline for %s: %w", r.MessageID, err))
				}
				seeded++
			}
			continue
		}
		added, removed, newBaseline := diffLabels(r.Tags, splitFlags(r.SyncedLabels), vocab)
		if len(added) == 0 && len(removed) == 0 {
			continue
		}
		if e.opts.DryRun {
			slog.Debug("[dry-run] Would upload label change", "module", "SYNCENGINE", // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
				"message_id", r.MessageID, "add", added, "remove", removed)
			result.Moved++
			continue
		}
		ref := backend.RemoteRef{ID: r.RemoteRef}
		if err := lw.ApplyLabels(ctx, ref, added, removed); err != nil {
			slog.Warn("Label upload failed", "module", "SYNCENGINE", // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
				"message_id", r.MessageID, "err", err)
			result.Errors = append(result.Errors, fmt.Errorf("apply labels %s: %w", r.MessageID, err))
			continue
		}
		// Persist the baseline only after the server accepted the change, so a
		// failed upload is retried next sync instead of being silently dropped.
		if err := e.opts.Store.SetSyncedLabels(r.MessageID, e.opts.Account, strings.Join(newBaseline, ",")); err != nil {
			slog.Warn("Label upload: update baseline failed", "module", "SYNCENGINE",
				"message_id", r.MessageID, "err", err)
		}
		result.Moved++
		slog.Info("Uploaded label change", "module", "SYNCENGINE", // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
			"message_id", r.MessageID, "add", added, "remove", removed)
	}
	if seeded > 0 {
		slog.Info("Seeded label baselines for migrated messages", "module", "SYNCENGINE",
			"account", e.opts.Account, "count", seeded) // encgrep:allow account identifier (config name) + count, not content
	}
}

// diffLabels computes the label add/remove sets and the new baseline for one
// message. newBaseline is the local tags restricted to the label vocabulary —
// the exact labels the message should carry on the server. added is the
// vocabulary tags gained since the baseline; removed is the baseline tags no
// longer present locally (the baseline is already vocabulary-only, so no vocab
// check is needed there).
func diffLabels(tags, baseline []string, vocab map[string]bool) (added, removed, newBaseline []string) {
	inBaseline := make(map[string]bool, len(baseline))
	for _, t := range baseline {
		inBaseline[t] = true
	}
	inTags := make(map[string]bool, len(tags))
	for _, t := range tags {
		inTags[t] = true
	}
	for _, t := range tags {
		if !vocab[t] {
			continue // Durian-local tag — never a Gmail label, never uploaded
		}
		newBaseline = append(newBaseline, t)
		if !inBaseline[t] {
			added = append(added, t)
		}
	}
	for _, t := range baseline {
		if !inTags[t] {
			removed = append(removed, t)
		}
	}
	return added, removed, newBaseline
}

// moveDestination decides where an INBOX message that lost the "inbox" tag
// should go, from its remaining tags and the available server folders: a
// message tagged "deleted" goes to Trash, otherwise to Archive. Returns
// ("", false) — leave it in place — when the role has no folder on this
// account, so a missing Archive/Trash never sends mail somewhere wrong.
func moveDestination(tags []string, folders []backend.Folder) (string, bool) {
	role := backend.RoleArchive
	if tagsContain(tags, "deleted") {
		role = backend.RoleTrash
	}
	dest := folderNameByRole(folders, role)
	if dest == "" {
		return "", false
	}
	return dest, true
}

// folderNameByRole returns the name of the first folder with the given role, or
// "" if none. The name is what Backend.Move takes as its destination (an IMAP
// mailbox name, or a Graph folder id).
func folderNameByRole(folders []backend.Folder, role backend.Role) string {
	for _, f := range folders {
		if f.Role == role {
			return f.Name
		}
	}
	return ""
}

func tagsContain(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}
