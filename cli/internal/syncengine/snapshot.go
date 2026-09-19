package syncengine

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/julion2/durian/cli/internal/backend"
)

func (e *Engine) inspectSnapshotRecovery(folders []backend.Folder) (map[string]FolderState, bool, error) {
	states := make(map[string]FolderState)
	recoveryPending := false
	for _, folder := range folders {
		if !folder.Selectable || !e.folderSelected(folder) {
			continue
		}
		state, err := e.opts.Cursors.GetState(e.opts.Account, folder.Name)
		if err != nil {
			return states, true, fmt.Errorf("load %s cursor: %w", folder.Name, err)
		}
		states[folder.Name] = state
		if state.PendingFlags.SnapshotInProgress {
			recoveryPending = true
			continue
		}
		snapshot, err := e.opts.Store.GetSnapshotState(e.opts.Account, folder.Name)
		if err != nil {
			return states, true, fmt.Errorf("load %s snapshot staging: %w", folder.Name, err)
		}
		if snapshot.Active {
			recoveryPending = true
		}
	}
	return states, recoveryPending, nil
}

func (e *Engine) hydrateFullSnapshot(ctx context.Context, b backend.Backend, folder backend.Folder, present, unavailable, skipHydration map[string]struct{}, deltaFlags map[string]backend.Flags, result *Result) ([]string, error) {
	refs := make([]string, 0, len(present))
	for ref := range present {
		refs = append(refs, ref)
	}
	rows, err := e.opts.Store.SnapshotRowsForRefs(e.opts.Account, folder.Name, refs)
	if err != nil {
		return nil, fmt.Errorf("load full-snapshot hydration state for %s: %w", folder.Name, err)
	}
	existing := make(map[string]struct{}, len(rows))
	messageRowIDs := make(map[string]int64, len(rows))
	for _, row := range rows {
		existing[row.RemoteRef] = struct{}{}
		messageRowIDs[row.RemoteRef] = row.RowID
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
		return nil, nil
	}
	hydrator, ok := b.(backend.SnapshotHydrator)
	if !ok {
		if len(missing) > 0 {
			slog.Warn("Snapshot refs are absent locally and backend cannot hydrate them, skipping refs", "module", "SYNCENGINE",
				"folder", folder.Name, "count", len(missing)) // encgrep:allow folder and count are operational sync metadata
			removeMissingSnapshotRefs(present, deltaFlags, missing)
		}
		return nil, nil
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
			return nil, fmt.Errorf("refresh full snapshot metadata: %w", err)
		}
		if err := validateSnapshotBatch(batch, hydrated); err != nil {
			return nil, err
		}
		removeMissingSnapshotRefs(present, deltaFlags, hydrated.Missing)
		for _, msg := range hydrated.Messages {
			deltaFlags[msg.Ref.ID] = msg.Flags
			if e.opts.Ingest.LabelsAsTags {
				if err := reconcileLabels(e.opts.Store, messageRowIDs[msg.Ref.ID], msg.Labels); err != nil {
					return nil, fmt.Errorf("reconcile snapshot labels for %s: %w", msg.Ref.ID, err)
				}
			}
		}
	}
	failedLegacyMessageIDs := make([]string, 0)
	bodyBatchSize := batchSize
	if providerLimit := b.Capabilities().BodyBatchLimit; providerLimit > 0 {
		bodyBatchSize = min(bodyBatchSize, providerLimit)
	}
	for start := 0; start < len(missing); start += bodyBatchSize {
		end := min(start+bodyBatchSize, len(missing))
		batch := missing[start:end]
		hydrated, err := hydrator.FetchSnapshotMessages(ctx, batch)
		if err != nil {
			return nil, fmt.Errorf("hydrate full snapshot: %w", err)
		}
		if err := validateSnapshotBatch(batch, hydrated); err != nil {
			return nil, err
		}
		removeMissingSnapshotRefs(present, deltaFlags, hydrated.Missing)
		for _, msg := range hydrated.Messages {
			deltaFlags[msg.Ref.ID] = msg.Flags
			messageID, rowID, created, err := Ingest(e.opts.Store, msg, folder.Name, folder.Role, e.opts.Ingest)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("ingest hydrated snapshot message %s: %w", msg.Ref.ID, err))
				if isRetryableStoreError(err) {
					return nil, fmt.Errorf("cursor held back: hydrated snapshot message %s could not be stored yet", msg.Ref.ID)
				}
				// A stable provider identity makes the current ref authoritative;
				// legacy no-stable-ID backends preserve matching prior refs.
				if msg.StableID == "" {
					msgID := msg.MessageID
					if msgID == "" {
						msgID = messageIDFromRaw(msg.Raw)
					}
					if msgID != "" {
						failedLegacyMessageIDs = append(failedLegacyMessageIDs, msgID)
					}
				}
				removeMissingSnapshotRefs(present, deltaFlags, []backend.RemoteRef{msg.Ref})
				continue
			}
			if created {
				result.New++
				if folder.Role == backend.RoleInbox || (b.Capabilities().LabelsAreTags && tagsContain(msg.Labels, "inbox")) {
					result.NewMessageIdentifiers = append(result.NewMessageIdentifiers, messageIdentifier(messageID, msg.StableID, rowID))
				}
			} else {
				result.Deduplicated++
			}
		}
	}
	priorRefs, err := e.opts.Store.SnapshotRefsForMessageIDs(e.opts.Account, folder.Name, failedLegacyMessageIDs)
	if err != nil {
		return nil, fmt.Errorf("load malformed snapshot preservation refs: %w", err)
	}
	var preserved []string
	for _, messageID := range failedLegacyMessageIDs {
		preserved = append(preserved, priorRefs[messageID]...)
	}
	return preserved, nil
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

// reconcileStagedSnapshot streams rows absent from durable authoritative
// presence. Deleting while keyset-paging is safe because IDs are monotonic.
func (e *Engine) reconcileStagedSnapshot(folder backend.Folder, result *Result) {
	var after int64
	for {
		rows, err := e.opts.Store.SnapshotAbsentRows(e.opts.Account, folder.Name, after, 500)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("page snapshot absences for %s: %w", folder.Name, err))
			return
		}
		if len(rows) == 0 {
			return
		}
		for _, row := range rows {
			after = row.RowID
			if e.handleDeleted(folder, backend.Deletion{Ref: backend.RemoteRef{Folder: folder.Name, ID: row.RemoteRef}, MessageID: row.MessageID}, nil, result) {
				result.Deleted++
			}
		}
	}
}
