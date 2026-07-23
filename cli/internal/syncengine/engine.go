package syncengine

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/julion2/durian/cli/internal/backend"
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
}

// Result aggregates the outcome of one Engine.Sync run.
type Result struct {
	// Folders is the number of folders processed.
	Folders int
	// New is the number of messages ingested (new or updated).
	New int
	// Deleted is the number of server-side deletions applied locally.
	Deleted int
	// Errors collects per-folder and per-message errors; the sync continues
	// past them (like the legacy syncer continues past a failed mailbox).
	Errors []error
}

// Engine drives a backend.Backend: folder discovery, cursor-paged incremental
// fetch, ingest, deletions, and a download-side flag pass.
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
		if err := e.syncFolder(ctx, b, folder, result); err != nil {
			slog.Warn("Folder sync failed, continuing", "module", "SYNCENGINE",
				"account", e.opts.Account, "folder", folder.Name, "err", err)
			result.Errors = append(result.Errors, fmt.Errorf("folder %s: %w", folder.Name, err))
		}
	}

	slog.Info("Sync complete", "module", "SYNCENGINE", "account", e.opts.Account,
		"folders", result.Folders, "new", result.New, "deleted", result.Deleted,
		"errors", len(result.Errors), "dry_run", e.opts.DryRun)
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

			e.applyDownloadFlagRemovals(messageID, msg.Flags, result)
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
			slog.Debug("Reached per-folder message cap, stopping", "module", "SYNCENGINE",
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

// applyDownloadFlagRemovals applies the remove half of the server flag state
// to local tags (Ingest applies the add half, matching the legacy insert
// path). Together they make the server flags authoritative for messages that
// came through the delta — e.g. a message marked read on the server sheds its
// local "unread" tag on re-fetch.
//
// This is the pragmatic Phase 1 flag sync: download-only, no three-way merge.
// The legacy syncer's three-way model (sync_flags.go) needs a persisted
// last-synced flag baseline per message, which does not exist yet on the
// engine path. TODO(Phase 2): persist per-message flag baselines alongside
// RemoteRefs and port the NeedsUpload/NeedsDownload/Merge logic, including
// the upload half via backend.ApplyFlags. Until then, local flag changes are
// not uploaded, and a local unsynced change can be overwritten by the next
// server delta for that message.
func (e *Engine) applyDownloadFlagRemovals(messageID string, flags backend.Flags, result *Result) {
	if e.opts.Mode == UploadOnly || e.opts.DryRun {
		return
	}
	_, remove := flagStateFromBackend(flags).ToTagOps()
	if len(remove) == 0 {
		return
	}
	if err := e.opts.Store.ModifyTagsByMessageIDAndAccount(messageID, e.opts.Account, nil, remove); err != nil {
		slog.Debug("Flag tag removal failed", "module", "SYNCENGINE", "message_id", messageID, "err", err)
		result.Errors = append(result.Errors, fmt.Errorf("flag tags for %s: %w", messageID, err))
	}
}

// handleDeleted processes one handle the source no longer holds in the folder.
// Mirrors the legacy handleDeletedUID: for role folders (which have a tag
// mapping) only the folder's tags are removed — the message was most likely
// moved and its row must survive; for user folders (no mapping) the row is
// deleted, unless the store shows the message was since ingested from another
// folder (the Mailbox column moved on), in which case deleting would destroy a
// live message. Reports whether a local change was actually applied.
func (e *Engine) handleDeleted(folder backend.Folder, del backend.Deletion, sessionRefs map[string]string, result *Result) bool {
	// Prefer the durable Message-ID the backend resolved from its own map;
	// fall back to a message ingested earlier in THIS run (rare same-run
	// arrive-then-delete). If neither resolves, skip safely.
	messageID := del.MessageID
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
		slog.Debug("Removing folder tags for moved message", "module", "SYNCENGINE",
			"folder", folder.Name, "message_id", messageID, "tags", mapping.addTags)
		if err := e.opts.Store.ModifyTagsByMessageIDAndAccount(messageID, e.opts.Account, nil, mapping.addTags); err != nil {
			slog.Warn("Remove folder tags failed", "module", "SYNCENGINE", "message_id", messageID, "err", err)
			result.Errors = append(result.Errors, fmt.Errorf("untag deleted %s: %w", messageID, err))
			return false
		}
		return true
	}

	// User folder without tag mapping: delete the row — but not if the message
	// has since been ingested from a different folder in this same run (the
	// Mailbox column reflects the latest ingest).
	if existing, err := e.opts.Store.GetByMessageID(messageID); err == nil && existing != nil && existing.Mailbox != folder.Name {
		slog.Debug("Message moved to another folder, keeping row", "module", "SYNCENGINE",
			"message_id", messageID, "folder", folder.Name, "current_mailbox", existing.Mailbox)
		return false
	}

	slog.Debug("Deleting message removed from untagged folder", "module", "SYNCENGINE",
		"folder", folder.Name, "message_id", messageID)
	if err := e.opts.Store.DeleteByMessageIDAndAccount(messageID, e.opts.Account); err != nil {
		slog.Warn("Store delete failed", "module", "SYNCENGINE", "message_id", messageID, "err", err)
		result.Errors = append(result.Errors, fmt.Errorf("delete %s: %w", messageID, err))
		return false
	}
	return true
}
