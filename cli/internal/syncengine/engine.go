package syncengine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/imap"
	"github.com/julion2/durian/cli/internal/store"
)

// defaultBatchLimit caps message bodies per FetchMessages call when the caller
// does not set Options.BatchLimit.
const defaultBatchLimit = 200

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
	// MaxPerFolder caps how many messages are fetched per folder per run
	// (newest first, since the backend returns newest UIDs first), mirroring
	// the legacy syncer's GetIMAPMaxMessages. 0 means unlimited. Without this
	// the engine would page a folder's entire history on first sync.
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

	// A label backend (Gmail) mirrors Message.Labels to tags instead of using the
	// folder-role mapping; carry the capability into ingest.
	e.opts.Ingest.LabelsAsTags = b.Capabilities().LabelsAreTags

	folders, err := b.FetchFolders(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch folders: %w", err)
	}

	result := &Result{}
	for _, folder := range folders {
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("sync canceled: %w", err)
		}
		if !folder.Selectable {
			continue
		}
		if !e.folderSelected(folder) {
			continue
		}

		result.Folders++
		// UploadOnly skips the download/ingest pass entirely — used to push a
		// backlog of local changes (folder moves, flag changes) WITHOUT
		// re-fetching the server's copy, which would re-add role tags like
		// "inbox" to messages the user archived but that haven't moved on the
		// server yet, defeating the upload we're about to do.
		var deltaFlags map[string]backend.Flags
		if e.opts.Mode != UploadOnly {
			df, err := e.syncFolder(ctx, b, folder, result)
			if err != nil {
				slog.Warn("Folder sync failed, continuing", "module", "SYNCENGINE",
					"account", e.opts.Account, "folder", folder.Name, "err", err)
				result.Errors = append(result.Errors, fmt.Errorf("folder %s: %w", folder.Name, err))
				continue
			}
			deltaFlags = df
		}
		e.reconcileFolderFlags(ctx, b, folder, deltaFlags, result)
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

	// Upload local archive/delete actions (INBOX messages that lost the "inbox"
	// tag) to the server. Runs after downloads so it sees the freshest folders.
	// Folder backends move between mailboxes; a LabelsAreTags backend (Gmail) has
	// no folders, so the label-upload pass handles the same archive/delete intent
	// (and arbitrary label changes) by pushing label diffs instead. Each self-
	// gates, so exactly one does work for a given backend.
	e.uploadFolderMoves(ctx, b, folders, result)
	e.uploadLabelChanges(ctx, b, result)

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
// more, persisting the cursor after every successfully processed batch so a
// crash mid-folder resumes where it left off.
func (e *Engine) syncFolder(ctx context.Context, b backend.Backend, folder backend.Folder, result *Result) (map[string]backend.Flags, error) {
	cursor, err := e.opts.Cursors.Get(e.opts.Account, folder.Name)
	if err != nil {
		return nil, fmt.Errorf("load cursor: %w", err)
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

	// fetched counts messages pulled this run, to enforce MaxPerFolder.
	fetched := 0
	// ingestFailed marks that at least one message in the current batch could
	// not be stored, which makes this batch's cursor unsafe to persist.
	ingestFailed := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("canceled: %w", err)
		}

		res, err := b.FetchMessages(ctx, folder.Name, cursor, e.opts.BatchLimit)
		if err != nil {
			return nil, fmt.Errorf("fetch messages: %w", err)
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
				if folder.Role == backend.RoleInbox {
					result.NewMessageIDs = append(result.NewMessageIDs, messageID)
				}
			} else {
				result.Deduplicated++
			}
		}

		for _, del := range res.Deleted {
			if e.handleDeleted(folder, del, sessionRefs, result) {
				result.Deleted++
			}
		}

		// A delta cursor is a promise that everything before it is stored. If
		// any message in this batch failed to ingest (a locked database, a
		// malformed body), advancing past it would drop that message for good:
		// the next delta starts after it and the server never mentions it
		// again. Stop the folder here instead and let the next pass refetch
		// the same batch — ingest is idempotent, so replaying it is free.
		if ingestFailed {
			return nil, fmt.Errorf("cursor held back: a message in this batch could not be stored yet")
		}

		// Persist the cursor only after the batch was fully processed, and
		// never in dry-run (advancing it would silently skip these changes on
		// the next real sync).
		if !e.opts.DryRun {
			if err := e.opts.Cursors.Set(e.opts.Account, folder.Name, res.Cursor); err != nil {
				return nil, fmt.Errorf("persist cursor: %w", err)
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
			return deltaFlags, nil
		}
		// Stop at the per-folder cap (newest-first), so a first sync of a large
		// folder does not page its entire history — parity with the legacy
		// syncer's GetIMAPMaxMessages.
		if e.opts.MaxPerFolder > 0 && fetched >= e.opts.MaxPerFolder {
			slog.Debug("Reached per-folder message cap, stopping", "module", "SYNCENGINE", // encgrep:allow folder name and cap counts are operational sync metadata, not message content
				"folder", folder.Name, "cap", e.opts.MaxPerFolder, "fetched", fetched)
			return deltaFlags, nil
		}
		// Defensive guard: a backend that reports HasMore without changing
		// anything would loop forever; bail out instead.
		if len(res.Messages) == 0 && len(res.Deleted) == 0 && bytes.Equal(cursor, res.Cursor) {
			return nil, fmt.Errorf("backend reported HasMore without progress (cursor unchanged, no changes)")
		}
		cursor = res.Cursor
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
func (e *Engine) reconcileFolderFlags(ctx context.Context, b backend.Backend, folder backend.Folder, deltaFlags map[string]backend.Flags, result *Result) {
	if e.opts.NoFlags || e.opts.DryRun {
		return
	}
	if ctx.Err() != nil {
		return
	}

	upload := e.opts.Mode != DownloadOnly
	download := e.opts.Mode != UploadOnly

	rows, err := e.opts.Store.GetFolderFlagState(e.opts.Account, folder.Name)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("flag sync %s: load flag state: %w", folder.Name, err))
		return
	}
	if len(rows) == 0 {
		return
	}

	// Choose which messages to fetch server flags for. A delta backend already
	// told us which messages changed server-side (they reappear in the delta),
	// so only those — plus any the user changed locally — are candidates; polling
	// every message was O(mailbox) and dominated a sync. FetchFlags stays the
	// source of truth, keeping the three-way merge below byte-identical; only the
	// ref set shrinks. A non-delta backend has no change feed, so it polls all.
	useDelta := b.Capabilities().FlagChangesInDelta
	answeredUnsupported := b.Capabilities().AnsweredUnsupported
	refs := make([]backend.RemoteRef, 0, len(rows))
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
		if useDelta && !e.flagCandidate(row, deltaFlags, upload) {
			continue
		}
		refs = append(refs, backend.RemoteRef{Folder: folder.Name, ID: row.RemoteRef})
	}
	if seeded > 0 {
		slog.Info("Seeded flag baselines for migrated messages", "module", "SYNCENGINE", "folder", folder.Name, "count", seeded) // encgrep:allow folder name + count are operational metadata, not content
	}
	if len(refs) == 0 {
		return
	}

	server, err := b.FetchFlags(ctx, folder.Name, refs)
	if err != nil {
		slog.Warn("Flag fetch failed, continuing", "module", "SYNCENGINE",
			"account", e.opts.Account, "folder", folder.Name, "err", err)
		result.Errors = append(result.Errors, fmt.Errorf("flag sync %s: %w", folder.Name, err))
		return
	}

	uploaded, downloaded := 0, 0
	for _, row := range rows {
		serverFlags, ok := server[row.RemoteRef]
		if !ok {
			continue // Not fetched (no candidate change) or not on the server.
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
					continue // Baseline stays put so the download is retried next sync.
				}
			}
			if err := e.opts.Store.SetSyncedFlags(row.MessageID, e.opts.Account, joinFlags(target)); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("flag baseline for %s: %w", row.MessageID, err))
			}
			downloaded++
		}
	}

	slog.Debug("Flag pass complete", "module", "SYNCENGINE",
		"folder", folder.Name, "uploaded", uploaded, "downloaded", downloaded)
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
// LabelsAreTags backend (Gmail): rather than relocating messages between
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
