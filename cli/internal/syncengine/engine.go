package syncengine

import (
	"bytes"
	"context"
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
	// Folders optionally restricts the sync to these folders (matched against
	// backend Folder.Name or Folder.Display); empty means all folders that
	// FetchFolders reports.
	Folders []string
	// DryRun logs what would happen without writing to the store or advancing
	// cursors.
	DryRun bool
	// NoFlags skips the per-folder flag reconciliation pass entirely (parity
	// with the legacy --no-flags). Messages still get their flag-derived tags
	// at ingest time; only the three-way upload/download pass is disabled.
	NoFlags bool
}

// Result aggregates the outcome of one Engine.Sync run.
type Result struct {
	// Folders is the number of folders processed.
	Folders int
	// New is the number of messages ingested (new or updated).
	New int
	// Deleted is the number of server-side deletions applied locally.
	Deleted int
	// Moved is the number of local archive/delete actions uploaded to the
	// server (INBOX messages moved to Archive/Trash).
	Moved int
	// Errors collects per-folder and per-message errors; the sync continues
	// past them (like the legacy syncer continues past a failed mailbox).
	Errors []error
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
		if e.opts.Mode != UploadOnly {
			if err := e.syncFolder(ctx, b, folder, result); err != nil {
				slog.Warn("Folder sync failed, continuing", "module", "SYNCENGINE",
					"account", e.opts.Account, "folder", folder.Name, "err", err)
				result.Errors = append(result.Errors, fmt.Errorf("folder %s: %w", folder.Name, err))
				continue
			}
		}
		e.reconcileFolderFlags(ctx, b, folder, result)
	}

	// Upload local archive/delete actions (INBOX messages that lost the "inbox"
	// tag) to the server. Runs after downloads so it sees the freshest folders.
	e.uploadFolderMoves(ctx, b, folders, result)

	slog.Info("Sync complete", "module", "SYNCENGINE", "account", e.opts.Account, // encgrep:allow account identifier (config name) and counts, not an encrypted column
		"folders", result.Folders, "new", result.New, "deleted", result.Deleted,
		"moved", result.Moved, "errors", len(result.Errors), "dry_run", e.opts.DryRun)
	return result, nil
}

// folderSelected reports whether the folder passes the Options.Folders filter.
func (e *Engine) folderSelected(folder backend.Folder) bool {
	if len(e.opts.Folders) == 0 {
		return true
	}
	for _, want := range e.opts.Folders {
		if strings.EqualFold(want, folder.Name) || strings.EqualFold(want, folder.Display) {
			return true
		}
	}
	return false
}

// syncFolder pages through one folder's changes until the backend reports no
// more, persisting the cursor after every successfully processed batch so a
// crash mid-folder resumes where it left off.
func (e *Engine) syncFolder(ctx context.Context, b backend.Backend, folder backend.Folder, result *Result) error {
	cursor, err := e.opts.Cursors.Get(e.opts.Account, folder.Name)
	if err != nil {
		return fmt.Errorf("load cursor: %w", err)
	}

	// sessionRefs maps RemoteRef.ID -> Message-ID for messages ingested in THIS
	// run of this folder. It is only a fallback: the backend resolves most
	// deletions to a durable Message-ID itself (Deletion.MessageID), so this
	// covers the rare same-run arrive-then-delete case. A deletion the backend
	// cannot resolve and that was not seen this run is logged and skipped, which
	// is safe — the row lingers rather than risking deleting the wrong message.
	sessionRefs := make(map[string]string)

	// fetched counts messages pulled this run, to enforce MaxPerFolder.
	fetched := 0
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("canceled: %w", err)
		}

		res, err := b.FetchMessages(ctx, folder.Name, cursor, e.opts.BatchLimit)
		if err != nil {
			return fmt.Errorf("fetch messages: %w", err)
		}

		for _, msg := range res.Messages {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("canceled: %w", err)
			}
			if e.opts.DryRun {
				slog.Debug("[dry-run] Would ingest message", "module", "SYNCENGINE",
					"folder", folder.Name, "ref", msg.Ref.ID, "message_id", msg.MessageID)
				result.New++
				continue
			}
			messageID, err := Ingest(e.opts.Store, msg, folder.Name, folder.Role, e.opts.Ingest)
			if err != nil {
				slog.Warn("Ingest failed", "module", "SYNCENGINE",
					"folder", folder.Name, "ref", msg.Ref.ID, "err", err)
				result.Errors = append(result.Errors, fmt.Errorf("ingest %s/%s: %w", folder.Name, msg.Ref.ID, err))
				continue
			}
			sessionRefs[msg.Ref.ID] = messageID
			result.New++
		}

		for _, del := range res.Deleted {
			if e.handleDeleted(folder, del, sessionRefs, result) {
				result.Deleted++
			}
		}

		// Persist the cursor only after the batch was fully processed, and
		// never in dry-run (advancing it would silently skip these changes on
		// the next real sync).
		if !e.opts.DryRun {
			if err := e.opts.Cursors.Set(e.opts.Account, folder.Name, res.Cursor); err != nil {
				return fmt.Errorf("persist cursor: %w", err)
			}
		}

		fetched += len(res.Messages)

		if !res.HasMore {
			return nil
		}
		// Stop at the per-folder cap (newest-first), so a first sync of a large
		// folder does not page its entire history — parity with the legacy
		// syncer's GetIMAPMaxMessages.
		if e.opts.MaxPerFolder > 0 && fetched >= e.opts.MaxPerFolder {
			slog.Debug("Reached per-folder message cap, stopping", "module", "SYNCENGINE", // encgrep:allow folder name and cap counts are operational sync metadata, not message content
				"folder", folder.Name, "cap", e.opts.MaxPerFolder, "fetched", fetched)
			return nil
		}
		// Defensive guard: a backend that reports HasMore without changing
		// anything would loop forever; bail out instead.
		if len(res.Messages) == 0 && len(res.Deleted) == 0 && bytes.Equal(cursor, res.Cursor) {
			return fmt.Errorf("backend reported HasMore without progress (cursor unchanged, no changes)")
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
func (e *Engine) reconcileFolderFlags(ctx context.Context, b backend.Backend, folder backend.Folder, result *Result) {
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

	refs := make([]backend.RemoteRef, 0, len(rows))
	for _, row := range rows {
		refs = append(refs, backend.RemoteRef{Folder: folder.Name, ID: row.RemoteRef})
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
			continue // Not on the server (anymore); the deletion path owns removals.
		}

		local := imap.FlagStateFromTags(row.Tags)
		serverState := flagStateFromBackend(serverFlags)
		// The ORIGINAL stored baseline; both branches below deliberately compare
		// against it (not one the upload branch may have just advanced),
		// mirroring the legacy three-way's conflict detection.
		baseline := imap.FlagStateFromIMAP(splitFlags(row.SyncedFlags))

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
