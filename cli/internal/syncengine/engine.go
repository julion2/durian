package syncengine

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
	durianmail "github.com/julion2/durian/cli/internal/mail"
	"github.com/julion2/durian/cli/internal/store"
	"github.com/julion2/durian/cli/internal/syncidentity"
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
	// NoFlags skips flag synchronization entirely (parity with the legacy
	// --no-flags), including provider-native explicit tag patches. Messages
	// still get their flag-derived tags at ingest time, and delta-carried flag
	// work is retained for the next sync that enables reconciliation.
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
	// NewMessageIdentifiers are exact local:<row-id> identifiers for arrivals
	// with provider-stable identity and Message-ID fallbacks for legacy rows.
	// Only inbox-role folders contribute: a message appearing in Sent or Archive
	// is not an arrival the user wants to hear about. Empty in dry-run.
	NewMessageIdentifiers []string
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
	deadline := time.Time{}
	if e.opts.Timeout > 0 {
		deadline = time.Now().Add(e.opts.Timeout)
	}
	if !e.opts.DryRun {
		lockCtx, cancel := contextWithDeadline(ctx, deadline)
		release, err := e.opts.Store.AcquireAccountSync(lockCtx, e.opts.Account)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("syncengine: %w", err)
		}
		defer release()
	}

	// A label backend (Gmail/JMAP) mirrors Message.Labels to tags instead of using the
	// folder-role mapping; carry the capability into ingest.
	e.opts.Ingest.LabelsAsTags = b.Capabilities().LabelsAreTags

	requestCtx, cancel := contextWithDeadline(ctx, deadline)
	folders, err := b.FetchFolders(requestCtx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("fetch folders: %w", err)
	}
	legacyIdentityMigration := false
	if !e.opts.DryRun {
		legacyIdentityMigration, err = e.migrateLegacyProviderIdentities(b, folders)
		if err != nil {
			return nil, fmt.Errorf("migrate legacy provider identities: %w", err)
		}
	}
	initialStates := make(map[string]FolderState)
	snapshotRecoveryPending := false
	var snapshotRecoveryErr error
	if !e.opts.DryRun {
		initialStates, snapshotRecoveryPending, snapshotRecoveryErr = e.inspectSnapshotRecovery(folders)
	}
	deferProviderUploads := legacyIdentityMigration || snapshotRecoveryPending || snapshotRecoveryErr != nil

	result := &Result{}
	if snapshotRecoveryErr != nil {
		result.Errors = append(result.Errors, fmt.Errorf("inspect snapshot recovery: %w", snapshotRecoveryErr))
	}
	_, hasTagMutationWriter := b.(backend.TagMutationWriter)
	filterProviderMutationErrors := hasTagMutationWriter && e.opts.Mode != DownloadOnly && !e.opts.NoFlags && !e.opts.DryRun
	providerMutationErrorsFiltered := false
	if filterProviderMutationErrors {
		// A pre-download mutation may refer to an identity that the authoritative
		// snapshot replaces or safely rekeys. Keep real unresolved failures, but
		// do not report a failed sync after the queue entry was consumed or removed.
		defer func() {
			if !providerMutationErrorsFiltered {
				e.dropResolvedProviderTagMutationErrors(result)
			}
		}()
	}
	if !deferProviderUploads {
		mutationCtx, cancel := contextWithDeadline(ctx, deadline)
		e.uploadProviderTagMutations(mutationCtx, b, result)
		cancel()
	}
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
		state, loaded := initialStates[folder.Name]
		if !loaded {
			state, err = e.opts.Cursors.GetState(e.opts.Account, folder.Name)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("folder %s: load cursor state: %w", folder.Name, err))
				continue
			}
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
		pendingFlags := state.PendingFlags
		folderDeadline := deadline
		if folderSync != nil {
			deltaFlags = folderSync.deltaFlags
			pendingFlags = folderSync.baseState.PendingFlags
			if folderSync.deadline.After(folderDeadline) {
				folderDeadline = folderSync.deadline
			}
		}
		folderCtx, cancel := contextWithDeadline(ctx, folderDeadline)
		flagResult := e.reconcileFolderFlags(folderCtx, b, folder, deltaFlags, pendingFlags, result)
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
				next.PendingFlags.SnapshotInProgress = false
			}
		} else if flagResult.reconciled {
			// A complete flag pass reconciles all queued work, regardless of
			// whether this run downloaded a delta.
			next.PendingFlags.ReplayCount = 0
			if folderSync != nil && folderSync.fullSnapshot {
				next.PendingFlags.SnapshotInProgress = false
			}
		}
		committed := false
		if !folderStateEqual(state, next) {
			if err := e.opts.Cursors.Commit(e.opts.Account, folder.Name, next.Cursor, next.PendingFlags); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("folder %s: persist cursor state: %w", folder.Name, err))
			} else {
				committed = true
			}
		} else {
			committed = true
		}
		if committed && folderSync != nil && folderSync.fullSnapshot && !next.PendingFlags.SnapshotInProgress {
			if err := e.opts.Store.ClearSnapshot(e.opts.Account, folder.Name); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("folder %s: clear completed snapshot: %w", folder.Name, err))
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
	// A legacy identity migration forces a replacement snapshot. Do not send
	// any old local intent unless that download completed cleanly; in particular,
	// an upload-only watcher pass cannot prove the migrated refs still belong to
	// the authenticated provider account.
	allowUploads := !deferProviderUploads || (e.opts.Mode != UploadOnly && len(result.Errors) == 0)
	if allowUploads {
		uploadCtx, cancel := contextWithDeadline(ctx, deadline)
		e.uploadFolderMoves(uploadCtx, b, folders, result)
		e.uploadProviderTagMutations(uploadCtx, b, result)
		e.uploadLabelChanges(uploadCtx, b, result)
		cancel()
	}
	if filterProviderMutationErrors {
		e.dropResolvedProviderTagMutationErrors(result)
		providerMutationErrorsFiltered = true
	}

	slog.Info("Sync complete", "module", "SYNCENGINE", "account", e.opts.Account, // encgrep:allow account identifier (config name) and counts, not an encrypted column
		"folders", result.Folders, "new", result.New, "deleted", result.Deleted,
		"moved", result.Moved, "errors", len(result.Errors), "dry_run", e.opts.DryRun)
	return result, nil
}

func (e *Engine) migrateLegacyProviderIdentities(b backend.Backend, folders []backend.Folder) (bool, error) {
	migrator, ok := b.(backend.LegacyIdentityMigrator)
	if !ok {
		return false, nil
	}
	migrated := false
	for _, folder := range folders {
		if !folder.Selectable || !e.folderSelected(folder) {
			continue
		}
		state, err := e.opts.Cursors.GetState(e.opts.Account, folder.Name)
		if err != nil {
			return migrated, fmt.Errorf("load %s cursor: %w", folder.Name, err)
		}
		scopedCursor, prefix, migrate := migrator.LegacyIdentityMigration(state.Cursor)
		if !migrate {
			continue
		}
		if err := e.opts.Store.MigrateLegacyProviderIdentityScope(e.opts.Account, folder.Name, prefix); err != nil {
			return migrated, fmt.Errorf("scope %s rows: %w", folder.Name, err)
		}
		state.PendingFlags.SnapshotInProgress = true
		state.PendingFlags.FullScan = true
		if err := e.opts.Cursors.Commit(e.opts.Account, folder.Name, scopedCursor, state.PendingFlags); err != nil {
			return migrated, fmt.Errorf("persist scoped %s cursor: %w", folder.Name, err)
		}
		migrated = true
	}
	return migrated, nil
}

// isRetryableStoreError reports whether a failed ingest is worth retrying on
// the next pass. That includes an occupied full-ingest stripe and SQLite write
// contention between the daemon's watchers and a separate `durian sync`
// process. Those succeed on a retry; a malformed message or a constraint
// violation never will, so everything else is treated as permanent and skipped
// rather than blocking the folder's cursor.
func isRetryableStoreError(err error) bool {
	if errors.Is(err, store.ErrMessageIngestLock) {
		return true
	}
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
	snapshotEpisodeActive := false
	orphanedSnapshotMarker := false
	if !e.opts.DryRun {
		snapshot, err := e.opts.Store.GetSnapshotState(e.opts.Account, folder.Name)
		if err != nil {
			return nil, fmt.Errorf("inspect snapshot staging: %w", err)
		}
		if snapshot.Active && !state.PendingFlags.SnapshotInProgress && snapshot.Complete && bytes.Equal(state.Cursor, snapshot.CheckpointCursor) {
			// The final cursor commit won the crash race; only staging cleanup was
			// interrupted. Finish it before following the ordinary change feed.
			if err := e.opts.Store.ClearSnapshot(e.opts.Account, folder.Name); err != nil {
				return nil, fmt.Errorf("clear committed snapshot: %w", err)
			}
			snapshot.Active = false
		}
		if snapshot.Active {
			snapshotEpisodeActive = true
			baseState.Cursor = snapshot.BaseCursor
			baseState.PendingFlags.SnapshotInProgress = true
			baseState.PendingFlags.FullScan = true
			if !snapshot.Complete {
				cursor = snapshot.CheckpointCursor
			}
			if !state.PendingFlags.SnapshotInProgress || (!snapshot.Complete && !bytes.Equal(state.Cursor, snapshot.CheckpointCursor)) {
				checkpoint := state.Cursor
				if !snapshot.Complete {
					checkpoint = snapshot.CheckpointCursor
				}
				if err := e.opts.Cursors.Commit(e.opts.Account, folder.Name, checkpoint, baseState.PendingFlags); err != nil {
					return nil, fmt.Errorf("repair snapshot checkpoint: %w", err)
				}
			}
			if snapshot.Complete {
				errorsBefore := len(result.Errors)
				e.reconcileStagedSnapshot(folder, result)
				if len(result.Errors) > errorsBefore {
					return nil, errors.New("full-snapshot reconciliation failed")
				}
				return &folderSyncResult{
					cursor: snapshot.CheckpointCursor, fullSnapshot: true,
					deadline: deadline, baseState: baseState,
				}, nil
			}
		} else if state.PendingFlags.SnapshotInProgress {
			// The cursor file alone cannot prove which replacement pages reached
			// SQLite. Restart authoritatively rather than skipping unseen refs,
			// but retain the durable marker so a crash before the first new page
			// is staged cannot release pre-migration provider uploads.
			cursor = nil
			baseState.Cursor = nil
			orphanedSnapshotMarker = true
			if err := e.opts.Cursors.Commit(e.opts.Account, folder.Name, nil, baseState.PendingFlags); err != nil {
				return nil, fmt.Errorf("reset orphaned snapshot cursor: %w", err)
			}
		}
	}
	// A missing cursor normally means a capped first sync. If provider rows are
	// already present, however, the cursor was lost or reset. For a backend whose
	// initial pages carry complete presence refs, finish that snapshot and
	// reconcile it authoritatively so stale provider identities cannot linger.
	authoritativeInitial := false
	if len(cursor) == 0 && b.Capabilities().InitialSnapshotIsAuthoritative {
		hasRows, err := e.opts.Store.HasFolderRemoteRefs(e.opts.Account, folder.Name)
		if err != nil {
			return nil, fmt.Errorf("load initial-snapshot state for %s: %w", folder.Name, err)
		}
		authoritativeInitial = hasRows || orphanedSnapshotMarker
		if authoritativeInitial && e.opts.RecoveryTimeout > 0 {
			deadline = time.Now().Add(e.opts.RecoveryTimeout)
		}
	}
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
	snapshotSeenRefs := make(map[string]struct{})
	snapshotUnavailable := make(map[string]struct{})
	snapshotSkipHydration := make(map[string]struct{})
	snapshotPreserveRefs := make(map[string]struct{})
	fullSnapshot := false
	snapshotModeSet := false
	var syntheticMatcher *syncidentity.Matcher
	identityCursorUpdater, canUpdateIdentityCursor := b.(backend.IdentityCursorUpdater)

	// fetched counts messages pulled this run, to enforce MaxPerFolder.
	fetched := 0
	bodyBatchLimit := e.opts.BatchLimit
	if providerLimit := b.Capabilities().BodyBatchLimit; providerLimit > 0 {
		bodyBatchLimit = min(bodyBatchLimit, providerLimit)
	}
	// ingestFailed marks that at least one message in the current batch could
	// not be stored, which makes this batch's cursor unsafe to persist.
	ingestFailed := false
	for {
		// Full-snapshot collections are page-local; durable staging detects
		// duplicates across pages and owns mailbox-wide presence.
		clear(snapshotRefs)
		clear(snapshotSeenRefs)
		clear(snapshotUnavailable)
		clear(snapshotSkipHydration)
		clear(snapshotPreserveRefs)
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("canceled: %w", err)
		}

		fetchCtx, cancel := contextWithDeadline(ctx, deadline)
		res, err := b.FetchMessages(fetchCtx, folder.Name, cursor, bodyBatchLimit)
		cancel()
		if err != nil {
			if errors.Is(err, backend.ErrSnapshotInvalidated) && !e.opts.DryRun && snapshotEpisodeActive {
				// The provider's live query moved after one or more pages were
				// staged. Restore the base cursor before discarding SQLite staging:
				// if a process dies between the two stores, restart still sees the
				// active episode and cannot release provider uploads.
				baseState.PendingFlags.SnapshotInProgress = false
				if commitErr := e.opts.Cursors.Commit(e.opts.Account, folder.Name, baseState.Cursor, baseState.PendingFlags); commitErr != nil {
					return nil, errors.Join(err, fmt.Errorf("restore cursor after invalidated snapshot: %w", commitErr))
				}
				if clearErr := e.opts.Store.ClearSnapshot(e.opts.Account, folder.Name); clearErr != nil {
					return nil, errors.Join(err, fmt.Errorf("clear invalidated snapshot: %w", clearErr))
				}
			}
			return nil, fmt.Errorf("fetch messages: %w", err)
		}
		if authoritativeInitial {
			res.FullSnapshot = true
		}
		if snapshotModeSet && fullSnapshot && !res.FullSnapshot {
			if !e.opts.DryRun && snapshotEpisodeActive {
				baseState.PendingFlags.SnapshotInProgress = false
				if commitErr := e.opts.Cursors.Commit(e.opts.Account, folder.Name, baseState.Cursor, baseState.PendingFlags); commitErr != nil {
					return nil, fmt.Errorf("restore cursor after invalid replacement snapshot: %w", commitErr)
				}
				if clearErr := e.opts.Store.ClearSnapshot(e.opts.Account, folder.Name); clearErr != nil {
					return nil, fmt.Errorf("abort invalid replacement snapshot: %w", clearErr)
				}
			}
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
		pageFlags := deltaFlags
		var reportedSnapshotRefs []string
		if res.FullSnapshot {
			// The durable FullScan marker owns final flag reconciliation. Keep
			// only this page's metadata while staging to avoid mailbox-sized maps.
			deltaFlags = nil
			pageFlags = make(map[string]backend.Flags, len(res.Messages))
			clear(sessionRefs)
			for _, ref := range res.Present {
				if _, seen := snapshotSeenRefs[ref.ID]; seen {
					return nil, fmt.Errorf("backend reported duplicate ref %q in replacement snapshot", ref.ID)
				}
				snapshotSeenRefs[ref.ID] = struct{}{}
				snapshotRefs[ref.ID] = struct{}{}
			}
			for _, ref := range res.Unavailable {
				if _, present := snapshotRefs[ref.ID]; !present {
					return nil, fmt.Errorf("backend reported unavailable ref %q outside snapshot presence", ref.ID)
				}
				snapshotUnavailable[ref.ID] = struct{}{}
			}
			reportedSnapshotRefs = make([]string, 0, len(snapshotSeenRefs))
			for ref := range snapshotSeenRefs {
				reportedSnapshotRefs = append(reportedSnapshotRefs, ref)
			}
			if !e.opts.DryRun {
				if !snapshotEpisodeActive {
					if err := e.opts.Store.BeginSnapshot(e.opts.Account, folder.Name, baseState.Cursor); err != nil {
						return nil, fmt.Errorf("begin snapshot: %w", err)
					}
					snapshotEpisodeActive = true
				}
				// Validate the whole page before ingesting or applying explicit
				// deletions. A malformed/duplicate replacement page must have no
				// local side effects before the episode is rolled back.
				if err := e.opts.Store.ValidateSnapshotPageRefs(e.opts.Account, folder.Name, reportedSnapshotRefs); err != nil {
					baseState.PendingFlags.SnapshotInProgress = false
					if commitErr := e.opts.Cursors.Commit(e.opts.Account, folder.Name, baseState.Cursor, baseState.PendingFlags); commitErr != nil {
						return nil, errors.Join(fmt.Errorf("validate snapshot page: %w", err), fmt.Errorf("restore snapshot base cursor: %w", commitErr))
					}
					if clearErr := e.opts.Store.ClearSnapshot(e.opts.Account, folder.Name); clearErr != nil {
						return nil, errors.Join(fmt.Errorf("validate snapshot page: %w", err), fmt.Errorf("clear invalid snapshot: %w", clearErr))
					}
					return nil, fmt.Errorf("validate snapshot page: %w", err)
				}
			}
		}

		adoptedIdentities := make(map[string]string)
		failedLegacyMessageIDs := make([]string, 0)
		for _, msg := range res.Messages {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("canceled: %w", err)
			}
			// Record the server flags the delta carried, whether or not the
			// message is new: a re-appearing message signals a server flag change.
			pageFlags[msg.Ref.ID] = msg.Flags
			if e.opts.DryRun {
				slog.Debug("[dry-run] Would ingest message", "module", "SYNCENGINE",
					"folder", folder.Name, "ref", msg.Ref.ID, "message_id", msg.MessageID)
				result.New++
				if res.FullSnapshot {
					snapshotSkipHydration[msg.Ref.ID] = struct{}{}
				}
				continue
			}
			provisionalMessageID := msg.MessageID
			if res.FullSnapshot && syntheticMatcher == nil && canUpdateIdentityCursor && len(msg.Raw) > 0 && messageIDFromRaw(msg.Raw) == "" {
				if currentUIDValidity, ok := durianmail.SyntheticMessageUIDValidity(provisionalMessageID); ok {
					syntheticMatcher, err = syncidentity.New(e.opts.Store, e.opts.Account, folder.Name, currentUIDValidity)
					if err != nil {
						return nil, fmt.Errorf("prepare synthetic identity recovery: %w", err)
					}
				}
			}
			msg, recoveredMessageID, initialIngestComplete := adoptSyntheticIdentity(msg, syntheticMatcher)
			ingestOptions := e.opts.Ingest
			ingestOptions.IdentityRecovered = recoveredMessageID != "" && initialIngestComplete
			messageID, rowID, created, err := Ingest(e.opts.Store, msg, folder.Name, folder.Role, ingestOptions)
			if err != nil {
				if recoveredMessageID != "" {
					if messageUpsertCompleted(err) {
						syntheticMatcher.Commit(recoveredMessageID)
					} else {
						syntheticMatcher.Restore(recoveredMessageID)
					}
				}
				slog.Warn("Ingest failed", "module", "SYNCENGINE",
					"folder", folder.Name, "ref", msg.Ref.ID, "err", err)
				result.Errors = append(result.Errors, fmt.Errorf("ingest %s/%s: %w", folder.Name, msg.Ref.ID, err))
				// A retryable failure holds the cursor back. A new message that
				// cannot be stored for a permanent reason would fail identically
				// forever, so it is skipped; a recovered existing identity is the
				// exception because skipping it would corrupt the replacement map.
				if recoveredMessageID != "" || isRetryableStoreError(err) {
					// Once a replacement message has reserved an existing canonical
					// identity, every failed durable upsert must hold the cursor. A
					// permanent-skip path would let hydration discard the new ref and
					// reconciliation remove the old row/tag before the cursor advances.
					ingestFailed = true
				} else if res.FullSnapshot {
					// A full-body snapshot already made its one ingestion attempt.
					// Mark a permanently malformed item so hydration can skip it
					// when absent locally. A stable provider identity makes the
					// current ref authoritative; only legacy Message-ID identities
					// may preserve an older ref after a failed replacement.
					snapshotSkipHydration[msg.Ref.ID] = struct{}{}
					if msg.StableID == "" {
						failedMessageID := msg.MessageID
						if failedMessageID == "" {
							failedMessageID = messageIDFromRaw(msg.Raw)
						}
						if failedMessageID != "" {
							failedLegacyMessageIDs = append(failedLegacyMessageIDs, failedMessageID)
						}
					}
				}
				continue
			}
			if recoveredMessageID != "" {
				syntheticMatcher.Commit(recoveredMessageID)
			}
			sessionRefs[msg.Ref.ID] = messageID
			if recoveredMessageID != "" && messageID != provisionalMessageID {
				adoptedIdentities[msg.Ref.ID] = messageID
			}
			// A re-delivered message (e.g. a flag change surfaced by the delta)
			// is an update, not a new arrival — count the two separately.
			if created {
				result.New++
				// Only inbox arrivals are worth notifying about; a message
				// ingested into Sent is the user's own, and Archive/Junk are
				// not events they asked to be interrupted for.
				if folder.Role == backend.RoleInbox || (b.Capabilities().LabelsAreTags && tagsContain(msg.Labels, "inbox")) {
					result.NewMessageIdentifiers = append(result.NewMessageIdentifiers, messageIdentifier(messageID, msg.StableID, rowID))
				}
			} else {
				result.Deduplicated++
			}
		}
		if len(adoptedIdentities) > 0 {
			res.Cursor, err = identityCursorUpdater.AdoptMessageIdentities(res.Cursor, adoptedIdentities)
			if err != nil {
				return nil, fmt.Errorf("update cursor with recovered synthetic identities: %w", err)
			}
		}

		for _, del := range res.Deleted {
			if res.FullSnapshot {
				delete(snapshotRefs, del.Ref.ID)
				delete(snapshotUnavailable, del.Ref.ID)
				delete(snapshotSkipHydration, del.Ref.ID)
				delete(pageFlags, del.Ref.ID)
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

		if res.FullSnapshot {
			recoveryCtx, cancel := contextWithDeadline(ctx, deadline)
			hydratedPreserveRefs, hydrateErr := e.hydrateFullSnapshot(recoveryCtx, b, folder, snapshotRefs, snapshotUnavailable, snapshotSkipHydration, pageFlags, result)
			cancel()
			if hydrateErr != nil {
				return nil, hydrateErr
			}
			for _, ref := range hydratedPreserveRefs {
				snapshotPreserveRefs[ref] = struct{}{}
			}
			priorRefs, err := e.opts.Store.SnapshotRefsForMessageIDs(e.opts.Account, folder.Name, failedLegacyMessageIDs)
			if err != nil {
				return nil, fmt.Errorf("load snapshot preservation refs: %w", err)
			}
			for _, messageID := range failedLegacyMessageIDs {
				for _, ref := range priorRefs[messageID] {
					snapshotPreserveRefs[ref] = struct{}{}
				}
			}
			if !e.opts.DryRun {
				present := make([]string, 0, len(snapshotRefs))
				for ref := range snapshotRefs {
					present = append(present, ref)
				}
				preserved := make([]string, 0, len(snapshotPreserveRefs))
				for ref := range snapshotPreserveRefs {
					preserved = append(preserved, ref)
				}
				if err := e.opts.Store.StageSnapshotPage(e.opts.Account, folder.Name, reportedSnapshotRefs, present, preserved, res.Cursor, !res.HasMore); err != nil {
					return nil, fmt.Errorf("stage snapshot page: %w", err)
				}
				baseState.PendingFlags.SnapshotInProgress = true
				baseState.PendingFlags.FullScan = true
				if err := e.opts.Cursors.Commit(e.opts.Account, folder.Name, res.Cursor, baseState.PendingFlags); err != nil {
					return nil, fmt.Errorf("checkpoint snapshot page: %w", err)
				}
			}
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
			if fullSnapshot && !e.opts.DryRun {
				errorsBefore := len(result.Errors)
				e.reconcileStagedSnapshot(folder, result)
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

func adoptSyntheticIdentity(msg backend.Message, matcher *syncidentity.Matcher) (backend.Message, string, bool) {
	if matcher == nil || !durianmail.IsSyntheticMessageID(msg.MessageID) || len(msg.Raw) == 0 {
		return msg, "", false
	}
	if messageID, initialIngestComplete, err := matcher.MatchRaw(msg.MessageID, msg.Raw, msg.InternalDate); err == nil && messageID != "" {
		msg.MessageID = messageID
		return msg, messageID, initialIngestComplete
	}
	return msg, "", false
}

func messageIdentifier(messageID, stableID string, rowID int64) string {
	if stableID != "" {
		return fmt.Sprintf("local:%d", rowID)
	}
	return messageID
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
	var stored *store.Message
	if del.Ref.ID != "" {
		if msg, err := e.opts.Store.GetByRemoteRef(e.opts.Account, folder.Name, del.Ref.ID); err != nil {
			slog.Debug("remote_ref deletion lookup failed", "module", "SYNCENGINE",
				"folder", folder.Name, "ref", del.Ref.ID, "err", err)
		} else if msg != nil {
			stored = msg
			messageID = msg.MessageID
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
		var err error
		if stored != nil {
			err = e.opts.Store.ModifyTagsByMessageDBID(stored.ID, nil, mapping.addTags)
		} else {
			err = e.opts.Store.ModifyTagsByMessageIDAndAccount(messageID, e.opts.Account, nil, mapping.addTags)
		}
		if err != nil {
			slog.Warn("Remove folder tags failed", "module", "SYNCENGINE", "message_id", messageID, "err", err) // encgrep:allow Message-ID is a plaintext RFC822 header / stable key, not an encrypted column
			result.Errors = append(result.Errors, fmt.Errorf("untag deleted %s: %w", messageID, err))
			return false
		}
		return true
	}

	// User folder without tag mapping: delete the row — but not if the message
	// has since been ingested from a different folder in this same run (the
	// Mailbox column reflects the latest ingest).
	existing := stored
	if existing == nil {
		existing, _ = e.opts.Store.GetByMessageID(messageID)
	}
	if existing != nil && existing.Mailbox != folder.Name {
		slog.Debug("Message moved to another folder, keeping row", "module", "SYNCENGINE", // encgrep:allow Message-ID and folder/mailbox names are operational sync metadata, not encrypted columns
			"message_id", messageID, "folder", folder.Name, "current_mailbox", existing.Mailbox)
		return false
	}

	slog.Debug("Deleting message removed from untagged folder", "module", "SYNCENGINE", // encgrep:allow folder name and Message-ID are operational sync metadata, not encrypted columns
		"folder", folder.Name, "message_id", messageID)
	var err error
	if stored != nil {
		err = e.opts.Store.DeleteByDBID(stored.ID)
	} else {
		err = e.opts.Store.DeleteByMessageIDAndAccount(messageID, e.opts.Account)
	}
	if err != nil {
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
		result.Errors = append(result.Errors, fmt.Errorf("load inbox state for folder moves: %w", err))
		return
	}

	for _, r := range rows {
		if tagsContain(r.Tags, "inbox") {
			continue // still in inbox — nothing to upload
		}
		dest, role, ok := moveDestination(r.Tags, folders)
		if !ok {
			result.Errors = append(result.Errors, fmt.Errorf("move %s: account has no %s folder", r.MessageID, role))
			continue
		}
		if e.opts.DryRun {
			slog.Debug("[dry-run] Would move message out of inbox", "module", "SYNCENGINE", // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
				"message_id", r.MessageID, "dest", dest)
			result.Moved++
			continue
		}
		ref := backend.RemoteRef{Folder: inbox.Name, ID: r.RemoteRef, MessageID: r.MessageID}
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
					result.Errors = append(result.Errors, fmt.Errorf("reconcile moved message %s: %w", r.MessageID, err))
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
			result.Errors = append(result.Errors, fmt.Errorf("record moved message %s: %w", r.MessageID, err))
			continue
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

	arbitrary, managesArbitraryLabels := lw.(backend.ArbitraryLabelWriter)
	seeded := 0
	var afterID int64
	for {
		rows, err := e.opts.Store.GetLabelStatePage(e.opts.Account, afterID, 500)
		if err != nil {
			slog.Warn("Label upload: load label state failed", "module", "SYNCENGINE",
				"account", e.opts.Account, "err", err)
			return
		}
		if len(rows) == 0 {
			break
		}
		afterID = rows[len(rows)-1].RowID
		if managesArbitraryLabels {
			for _, row := range rows {
				for _, tag := range row.Tags {
					if arbitrary.ManagesLabelTag(tag) {
						vocab[tag] = true
					}
				}
				// A custom keyword removed from its final local message no longer
				// occurs in row.Tags or the provider vocabulary. Keep labels from the
				// durable baseline in scope so the removal is still uploaded.
				for _, tag := range decodeLabelBaseline(row.SyncedLabels) {
					if arbitrary.ManagesLabelTag(tag) {
						vocab[tag] = true
					}
				}
			}
		}

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
			if r.SyncedLabels == "" && !managesArbitraryLabels {
				_, _, seedBaseline := diffLabels(r.Tags, nil, vocab)
				if len(seedBaseline) > 0 && !e.opts.DryRun {
					encoded, err := encodeLabelBaseline(seedBaseline)
					if err != nil {
						result.Errors = append(result.Errors, fmt.Errorf("encode label baseline for %s: %w", r.MessageID, err))
					} else if err := e.opts.Store.SetSyncedLabelsByDBID(r.RowID, encoded); err != nil {
						result.Errors = append(result.Errors, fmt.Errorf("seed label baseline for %s: %w", r.MessageID, err))
					} else {
						seeded++
					}
				}
				continue
			}
			added, removed, newBaseline := diffLabels(r.Tags, decodeLabelBaseline(r.SyncedLabels), vocab)
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
			encoded, err := encodeLabelBaseline(newBaseline)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("encode label baseline for %s: %w", r.MessageID, err))
				continue
			}
			if err := e.opts.Store.SetSyncedLabelsByDBID(r.RowID, encoded); err != nil {
				slog.Warn("Label upload: update baseline failed", "module", "SYNCENGINE",
					"message_id", r.MessageID, "err", err)
			}
			result.Moved++
			slog.Info("Uploaded label change", "module", "SYNCENGINE", // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
				"message_id", r.MessageID, "add", added, "remove", removed)
		}
	}
	if seeded > 0 {
		slog.Info("Seeded label baselines for migrated messages", "module", "SYNCENGINE",
			"account", e.opts.Account, "count", seeded) // encgrep:allow account identifier (config name) + count, not content
	}
}

type providerTagMutationError struct {
	mutationID int64
	messageID  string
	err        error
}

func (e *providerTagMutationError) Error() string {
	return fmt.Sprintf("apply tag mutation for %s: %v", e.messageID, e.err)
}

func (e *providerTagMutationError) Unwrap() error { return e.err }

// uploadProviderTagMutations sends durable user flag intent before downloading
// deltas. This makes a JMAP property patch the source of truth for local edits;
// the generic baseline pass that follows still downloads remote changes and
// supports providers without this optional capability.
func (e *Engine) uploadProviderTagMutations(ctx context.Context, b backend.Backend, result *Result) {
	if e.opts.Mode == DownloadOnly || e.opts.NoFlags {
		return
	}
	tw, ok := b.(backend.TagMutationWriter)
	if !ok {
		if e.opts.DryRun {
			return
		}
		if err := e.opts.Store.ClearProviderTagMutationsForAccount(e.opts.Account); err != nil {
			result.Errors = append(result.Errors, err)
		}
		return
	}
	mutations, err := e.opts.Store.ReadProviderTagMutations(e.opts.Account)
	if err != nil {
		result.Errors = append(result.Errors, err)
		return
	}
	for _, mutation := range mutations {
		if err := ctx.Err(); err != nil {
			return
		}
		if mutation.RemoteRef == "" {
			continue
		}
		if e.opts.DryRun {
			slog.Debug("[dry-run] Would upload explicit tag mutation", "module", "SYNCENGINE",
				"message_id", mutation.MessageID, "tag", mutation.Tag, "action", mutation.Action)
			continue
		}
		if err := tw.ApplyTagMutation(ctx, backend.RemoteRef{ID: mutation.RemoteRef}, mutation.Tag, mutation.Action == "add"); err != nil {
			if errors.Is(err, backend.ErrRefGone) {
				if clearErr := e.opts.Store.ClearProviderTagMutation(mutation.ID); clearErr != nil {
					result.Errors = append(result.Errors, &providerTagMutationError{
						mutationID: mutation.ID, messageID: mutation.MessageID, err: clearErr,
					})
				}
				continue
			}
			result.Errors = append(result.Errors, &providerTagMutationError{
				mutationID: mutation.ID, messageID: mutation.MessageID, err: err,
			})
			continue
		}
		var add, remove []string
		if mutation.Action == "add" {
			add = []string{mutation.Tag}
		} else {
			remove = []string{mutation.Tag}
		}
		if err := e.opts.Store.ModifyTagsByMessageDBID(mutation.RowID, add, remove); err != nil {
			// The provider already accepted the idempotent property patch. Keep
			// the queue entry so the next pass also repairs the local read model.
			result.Errors = append(result.Errors, &providerTagMutationError{
				mutationID: mutation.ID, messageID: mutation.MessageID,
				err: fmt.Errorf("restore local tag intent: %w", err),
			})
			continue
		}
		if err := e.opts.Store.ClearProviderTagMutation(mutation.ID); err != nil {
			result.Errors = append(result.Errors, &providerTagMutationError{
				mutationID: mutation.ID, messageID: mutation.MessageID, err: err,
			})
		}
	}
}

func (e *Engine) dropResolvedProviderTagMutationErrors(result *Result) {
	deduplicated := result.Errors[:0]
	kept := make(map[int64]struct{})
	for _, resultErr := range result.Errors {
		var mutationErr *providerTagMutationError
		if !errors.As(resultErr, &mutationErr) {
			deduplicated = append(deduplicated, resultErr)
			continue
		}
		if _, duplicate := kept[mutationErr.mutationID]; duplicate {
			continue
		}
		kept[mutationErr.mutationID] = struct{}{}
		deduplicated = append(deduplicated, resultErr)
	}
	result.Errors = deduplicated

	mutations, err := e.opts.Store.ReadProviderTagMutations(e.opts.Account)
	if err != nil {
		result.Errors = append(result.Errors, err)
		return
	}
	pending := make(map[int64]struct{}, len(mutations))
	for _, mutation := range mutations {
		pending[mutation.ID] = struct{}{}
	}
	filtered := result.Errors[:0]
	for _, resultErr := range result.Errors {
		var mutationErr *providerTagMutationError
		if !errors.As(resultErr, &mutationErr) {
			filtered = append(filtered, resultErr)
			continue
		}
		if _, unresolved := pending[mutationErr.mutationID]; !unresolved {
			continue
		}
		filtered = append(filtered, resultErr)
	}
	result.Errors = filtered
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

// Label baselines use one CSV record so provider keywords containing commas,
// quotes, or newlines remain distinct. Plain comma-joined baselines written by
// older versions are valid CSV and continue to decode as before. If a legacy
// label contains an unescaped quote or newline, fall back to the old split so
// upgrading does not make that already-stored value unreadable.
func encodeLabelBaseline(labels []string) (string, error) {
	if len(labels) == 0 {
		return "", nil
	}
	var encoded strings.Builder
	writer := csv.NewWriter(&encoded)
	if err := writer.Write(labels); err != nil {
		return "", err
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return "", err
	}
	return strings.TrimSuffix(encoded.String(), "\n"), nil
}

func decodeLabelBaseline(encoded string) []string {
	if encoded == "" {
		return nil
	}
	reader := csv.NewReader(strings.NewReader(encoded))
	labels, err := reader.Read()
	if err == nil {
		if _, trailingErr := reader.Read(); trailingErr == io.EOF {
			return labels
		}
	}
	return strings.Split(encoded, ",")
}

// moveDestination decides where an INBOX message that lost the "inbox" tag
// should go, from its remaining tags and the available server folders: a
// message tagged "trash" (or the legacy "deleted") goes to Trash, otherwise
// to Archive. Returns ok=false — leave it in place — when the role has no folder
// on this account, so a missing Archive/Trash never sends mail somewhere wrong.
// The returned role identifies the unresolved intent for error reporting.
func moveDestination(tags []string, folders []backend.Folder) (string, backend.Role, bool) {
	role := backend.RoleArchive
	if tagsContain(tags, "trash") || tagsContain(tags, "deleted") {
		role = backend.RoleTrash
	}
	dest := folderNameByRole(folders, role)
	if dest == "" {
		return "", role, false
	}
	return dest, role, true
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
