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

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/imap"
	"github.com/julion2/durian/cli/internal/store"
)

// reconcileFolderFlags runs the three-way flag pass for one folder after its
// fetch loop completed. The engine owns the merge (a port of the legacy
// (*imap.Syncer).syncFlags three-way, cli/internal/imap/sync_flags.go): per
// message it compares the flag state implied by local tags and the current
// server flags (Backend.FetchFlags) against the store's last-synced baseline
// (synced_flags). Local changes vs the baseline are uploaded via ApplyFlags;
// server changes are downloaded into tags via ToTagOps. Each flag is settled on
// its own by ResolveFlags — whichever side differs from the baseline moved it,
// and when both did they moved a boolean to the same value — so there is no
// side that "wins" and no rule about who does. Deleted and Completed stay
// server-owned, because no tag can express them and a local false is an absent
// representation rather than a decision. Keeping the merge here makes it
// provider-neutral: a
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
	if e.opts.DryRun {
		return flagReconcileResult{pendingFlags: pendingFlags}
	}
	if e.opts.NoFlags {
		return flagReconcileResult{
			pendingFlags: mergePendingFlags(pendingFlags, pendingFlagsFromMap(deltaFlags)),
		}
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
	_, nativePatches := b.(backend.TagMutationWriter)
	if nativePatches {
		// JMAP user intent is captured at the mutation boundary and sent as an
		// individual keyword property patch. Never reconstruct intent from
		// ambient local tags and a stale baseline for such a backend; this pass
		// remains responsible only for downloading server keyword changes.
		upload = false
	}
	if e.opts.Mode == UploadOnly && !upload {
		// Native patches were already flushed before this pass. Upload-only must
		// neither consume queued server work nor materialize the mailbox merely
		// to discover that no generic flag upload is allowed.
		return flagReconcileResult{pendingFlags: pendingFlags}
	}
	download := e.opts.Mode != UploadOnly

	// Full scans are durable keyset pages, not mailbox-sized slices. Include
	// bounded explicit/delta refs outside the page so urgent work is not delayed
	// behind a large replacement snapshot.
	scanActive := pendingFlags.FullScan && e.opts.Mode != UploadOnly
	var rows []store.FolderFlagRow
	var err error
	if scanActive {
		rows, err = e.opts.Store.GetFolderFlagStatePage(
			e.opts.Account, folder.Name, pendingFlags.ScanAfterID, maxFullScanRowsPerSync+1,
		)
		if err == nil {
			refSet := make(map[string]struct{}, len(pendingFlags.Refs)+len(deltaFlags))
			for _, ref := range pendingFlags.Refs {
				refSet[ref] = struct{}{}
			}
			for ref := range deltaFlags {
				refSet[ref] = struct{}{}
			}
			refs := make([]string, 0, len(refSet))
			for ref := range refSet {
				refs = append(refs, ref)
			}
			var explicit []store.FolderFlagRow
			explicit, err = e.opts.Store.GetFolderFlagStateForRefs(e.opts.Account, folder.Name, refs)
			if err == nil {
				selected := make(map[int64]struct{}, len(rows)+len(explicit))
				for _, row := range rows {
					selected[row.RowID] = struct{}{}
				}
				for _, row := range explicit {
					if _, exists := selected[row.RowID]; !exists {
						rows = append(rows, row)
						selected[row.RowID] = struct{}{}
					}
				}
				sort.Slice(rows, func(i, j int) bool { return rows[i].RowID < rows[j].RowID })
			}
		}
	} else {
		rows, err = e.opts.Store.GetFolderFlagState(e.opts.Account, folder.Name)
	}
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
		// A legacy-migrated row may carry no flag baseline because the legacy
		// syncer kept it in its own state file. An uninitialized baseline parses
		// as "unread/unflagged", so a stored read or flagged message would look
		// locally changed and become a false upload candidate. Seed that non-empty
		// state from the stored server columns (adopting server state, like the
		// legacy first-sync branch). Leave a logically empty legacy row alone:
		// its candidate comparison is already correct, and avoiding an eager
		// sentinel write prevents an otherwise unnecessary mailbox-wide sweep.
		// A delta reingest initializes it atomically from the old row in Store.
		if !row.SyncedFlagsInitialized {
			seededBaseline := joinFlags(imap.FlagState{Seen: row.IsSeen, Flagged: row.IsFlagged})
			if seededBaseline != "" {
				if err := e.opts.Store.SetSyncedFlagsByDBID(row.RowID, seededBaseline); err != nil {
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
				row.SyncedFlagsInitialized = true
				rows[i].SyncedFlags = seededBaseline
				rows[i].SyncedFlagsInitialized = true
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
			upload, download, nativePatches, answeredUnsupported, result,
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
	fetchIncomplete, upload, download, nativePatches, answeredUnsupported bool,
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

		// The state both sides should end up in. Generic backends resolve each
		// field against the shared baseline. Native-patch backends upload durable
		// user intent separately, so their fetched server state is authoritative
		// for this download pass and must not resurrect ambient local tags.
		//
		// Computing the upload from local-vs-server instead is what made a
		// server-side change look like a local removal. A star set remotely is
		// absent locally for the same reason it was absent before — nobody
		// touched it here — and diffing without the baseline read that as the
		// user un-starring, so the next upload deleted the new star.
		target := imap.ResolveFlags(baseline, local, serverState)
		if nativePatches {
			target = serverState
		}

		// Local changed vs the baseline: push the difference between the
		// resolved state and the server so the server converges on it.
		// DiffFlags emits only the locally-owned flags (Seen/Flagged/Answered);
		// Deleted and the server-only $Completed keyword never travel upward.
		//
		// Which halves of target actually reached their side. The baseline may
		// only record what one of these carried out — see imap.AdvanceBaseline.
		var pushed, pulled bool

		if upload && imap.NeedsUpload(local, baseline) {
			ref := backend.RemoteRef{Folder: folder.Name, ID: row.RemoteRef}
			toAdd, toRemove := imap.DiffFlags(target, serverState)
			add := backendFlagsFromState(imap.FlagStateFromIMAP(toAdd))
			remove := backendFlagsFromState(imap.FlagStateFromIMAP(toRemove))
			switch {
			case len(toAdd) == 0 && len(toRemove) == 0:
				// NeedsUpload compares local against the baseline, so it can
				// fire while the resolved state already matches the server —
				// the $Completed mask over Flagged is the live case. Nothing to
				// send, but the server does hold the resolved state, so the
				// locally-owned fields have converged and may advance.
				pushed = true
			default:
				if err := b.ApplyFlags(ctx, ref, add, remove); err != nil {
					// A provider error can carry credential-derived text, so do not
					// propagate its text to logs or caller-facing results. Continue;
					// the unchanged baseline retries this message on the next sync.
					slog.Warn("Flag upload failed", "module", "SYNCENGINE",
						"folder", folder.Name, "message_id", row.MessageID)
					result.Errors = append(result.Errors, fmt.Errorf("flag upload for %s failed", row.MessageID))
					continue
				}
				pushed = true
				uploaded++
			}
		}

		// Server changed vs the baseline: bring the change down to the same
		// resolved state the upload above pushed. Native-patch backends always
		// apply the selected server state because their explicit mutation journal,
		// not ambient local tags, is the source of local user intent.
		if download && (nativePatches || imap.NeedsDownload(serverState, baseline)) {
			pulled = true
			if !target.Equal(local) {
				add, remove := target.ToTagOps()
				// target was computed from a row snapshot taken before
				// FetchFlags. A tag change landing in that window is invisible
				// to the merge, and ToTagOps is absolute — writing it blind
				// would revert whatever the user did mid-sync, and the baseline
				// written below would then agree with the reverted tags, so no
				// later run could tell anything was lost. Write only while the
				// snapshot still holds.
				applied, err := e.opts.Store.ModifyFlagTagsIfUnchangedByDBID(
					row.RowID, imap.FlagTagVocabulary(),
					flagTagsOf(row.Tags), add, remove)
				switch {
				case err != nil:
					result.Errors = append(result.Errors, fmt.Errorf("flag tags for %s: %w", row.MessageID, err))
					failed = append(failed, row.RemoteRef)
					pulled = false
				case !applied:
					// The local side moved under us. The download did not
					// happen, so the baseline must not record it; the next sync
					// re-reads both sides and merges properly.
					failed = append(failed, row.RemoteRef)
					pulled = false
				default:
					downloaded++
				}
			} else {
				downloaded++
			}
		}

		// One write, covering only what actually happened. Writing target
		// wholesale — as both branches used to, independently — records the
		// other side's change as reconciled when the run never carried it out:
		// UploadOnly would bank a server flag it never wrote locally, and the
		// next run would read the local absence as the user removing it and
		// delete it from the provider.
		if pushed || pulled {
			next := imap.AdvanceBaseline(baseline, local, serverState, target, pushed, pulled)
			if !next.Equal(baseline) {
				if err := e.opts.Store.SetSyncedFlagsByDBID(row.RowID, joinFlags(next)); err != nil {
					result.Errors = append(result.Errors, fmt.Errorf("flag baseline for %s: %w", row.MessageID, err))
					failed = append(failed, row.RemoteRef)
				}
			}
		}
	}
	return uploaded, downloaded, failed
}

// flagTagsOf narrows a row's tags to the ones a flag decision depends on, so a
// concurrent change to an unrelated tag does not look like a conflict.
func flagTagsOf(tags []string) []string {
	var out []string
	for _, tag := range tags {
		if slices.Contains(imap.FlagTagVocabulary(), tag) {
			out = append(out, tag)
		}
	}
	return out
}

func folderStateEqual(a, b FolderState) bool {
	return bytes.Equal(a.Cursor, b.Cursor) &&
		a.PendingFlags.ReplayCount == b.PendingFlags.ReplayCount &&
		a.PendingFlags.FullScan == b.PendingFlags.FullScan &&
		a.PendingFlags.ScanAfterID == b.PendingFlags.ScanAfterID &&
		a.PendingFlags.SnapshotInProgress == b.PendingFlags.SnapshotInProgress &&
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
	snapshotInProgress := false
	for _, pending := range groups {
		acc.addPending(pending)
		if pending.ReplayCount > replayCount {
			replayCount = pending.ReplayCount
		}
		snapshotInProgress = snapshotInProgress || pending.SnapshotInProgress
	}
	merged := acc.pendingFlags()
	merged.ReplayCount = replayCount
	merged.SnapshotInProgress = snapshotInProgress
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
