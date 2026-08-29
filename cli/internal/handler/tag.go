package handler

import (
	"log/slog"
	"strings"
	"time"

	"github.com/julion2/durian/cli/internal/protocol"
	"github.com/julion2/durian/cli/internal/store"
	"github.com/julion2/durian/cli/internal/tagsync"
)

// Tag handles the "tag" command.
func (h *Handler) Tag(query string, tags string) protocol.Response {
	return h.tag(query, tags, false)
}

// PreviewTag validates and resolves a tag operation without modifying data.
func (h *Handler) PreviewTag(query string, tags string) protocol.Response {
	return h.tag(query, tags, true)
}

func (h *Handler) tag(query string, tags string, dryRun bool) protocol.Response {
	tagList := strings.Fields(tags)
	if len(tagList) == 0 {
		return protocol.FailWithMessage(protocol.ErrInvalidJSON, "no tags provided")
	}

	add, remove := splitTagOps(tagList)
	add, remove = enforceExclusiveTags(add, remove)

	// Expand group references
	expanded, err := h.expandGroups(query)
	if err != nil {
		return protocol.FailWithMessage(protocol.ErrBackendError, "expand groups: "+err.Error())
	}

	// Resolve every selector through the shared query parser so searches,
	// counts, and mutations enforce the same field semantics.
	results, err := h.store.Search(expanded, 1000000)
	if err != nil {
		return protocol.Fail(protocol.ErrBackendError, err)
	}
	// The accounts the query restricted itself to. The filter narrows the
	// search; everything derived from the results afterwards has to be narrowed
	// by the same set, or the operation escapes the scope the user asked for.
	// It used to escape: the search found only the selected account's threads,
	// and the mutation then ran thread-wide, writing tags into accounts that
	// were never selected.
	accounts := h.store.QueryAccounts(expanded)

	threadIDs := make([]string, 0, len(results))
	seenThread := make(map[string]bool, len(results))
	for _, r := range results {
		if seenThread[r.Thread] {
			continue
		}
		seenThread[r.Thread] = true
		threadIDs = append(threadIDs, r.Thread)
	}

	if len(threadIDs) == 0 {
		return protocol.FailWithMessage(protocol.ErrNotFound, "query matched no threads; no tags were changed")
	}

	changedThreads := 0
	for _, threadID := range threadIDs {
		// One resolved set of rows per thread, feeding every effect below.
		// Each of them working out "which messages" for itself is how they
		// drifted apart in the first place.
		target, err := h.store.ThreadTagTarget(threadID, accounts)
		if err != nil {
			return protocol.Fail(protocol.ErrBackendError, err)
		}
		if len(target) == 0 {
			// The thread matched, but none of its messages are in scope.
			continue
		}

		ids := make([]int64, 0, len(target))
		for _, row := range target {
			ids = append(ids, row.DBID)
		}

		var changed bool
		if dryRun {
			changed, err = h.store.PreviewTagChangesByDBIDs(ids, add, remove)
		} else {
			changed, err = h.store.ModifyTagsByDBIDs(ids, add, remove)
		}
		if err != nil {
			return protocol.Fail(protocol.ErrBackendError, err)
		}
		if changed {
			changedThreads++
		}
		if !dryRun && (h.tagSync != nil || h.tagSyncEnabled) {
			h.journalTagChanges(target, add, remove)
		}
		if !dryRun && h.syncTrigger != nil {
			for _, account := range targetAccounts(target) {
				h.syncTrigger.TriggerSync(account)
			}
		}
		if !dryRun && h.tagSync != nil {
			go h.pushTagChanges(target, add, remove)
		}
	}

	slog.Info("Tag operation complete", "module", "TAG", "dry_run", dryRun, "matched_threads", len(threadIDs), "changed_threads", changedThreads, "add", add, "remove", remove)
	return protocol.SuccessWithTagChanges(len(threadIDs), changedThreads)
}

// targetAccounts returns the distinct accounts a resolved target spans, in
// first-seen order. The sync trigger reads them from the target rather than
// asking the store again: a second query is a second chance to disagree with
// what was actually written.
func targetAccounts(target []store.TagTargetRow) []string {
	seen := make(map[string]bool, len(target))
	out := make([]string, 0, len(target))
	for _, row := range target {
		if row.Account == "" || seen[row.Account] {
			continue
		}
		seen[row.Account] = true
		out = append(out, row.Account)
	}
	return out
}

// journalTagChanges records tag changes in the local journal for later sync.
// It journals exactly the rows that were written — re-querying the thread would
// record entries for messages the operation deliberately left alone.
func (h *Handler) journalTagChanges(target []store.TagTargetRow, add, remove []string) {
	now := time.Now().Unix()
	for _, row := range target {
		for _, tag := range add {
			h.store.JournalTagChange(row.MessageID, row.Account, tag, "add", now)
		}
		for _, tag := range remove {
			h.store.JournalTagChange(row.MessageID, row.Account, tag, "remove", now)
		}
	}
}

// pushTagChanges sends tag changes to the remote sync server for exactly the
// rows that were written, for the same reason the journal does.
func (h *Handler) pushTagChanges(target []store.TagTargetRow, add, remove []string) {
	var changes []tagsync.TagChange
	now := time.Now().Unix()
	for _, row := range target {
		for _, tag := range add {
			changes = append(changes, tagsync.TagChange{
				MessageID: row.MessageID,
				Account:   row.Account,
				Tag:       tag,
				Action:    "add",
				Timestamp: now,
			})
		}
		for _, tag := range remove {
			changes = append(changes, tagsync.TagChange{
				MessageID: row.MessageID,
				Account:   row.Account,
				Tag:       tag,
				Action:    "remove",
				Timestamp: now,
			})
		}
	}
	if len(changes) == 0 {
		return
	}

	if err := h.tagSync.Push(changes); err != nil {
		slog.Warn("Tag sync push failed", "module", "TAGSYNC", "err", err)
	}
}

// enforceExclusiveTags ensures mutually exclusive tags don't coexist.
// When one tag from an exclusive group is added, the others are removed.
func enforceExclusiveTags(add, remove []string) ([]string, []string) {
	exclusive := []string{"archive", "trash", "inbox"}

	removeSet := make(map[string]bool, len(remove))
	for _, r := range remove {
		removeSet[r] = true
	}

	for _, a := range add {
		for _, ex := range exclusive {
			if a == ex {
				// Remove all other tags in the group
				for _, other := range exclusive {
					if other != a && !removeSet[other] {
						remove = append(remove, other)
						removeSet[other] = true
					}
				}
				break
			}
		}
	}

	return add, remove
}

// splitTagOps separates a tag operations list ("+tag", "-tag") into add and remove slices.
func splitTagOps(tagList []string) (add, remove []string) {
	for _, t := range tagList {
		if strings.HasPrefix(t, "+") {
			add = append(add, strings.TrimPrefix(t, "+"))
		} else if strings.HasPrefix(t, "-") {
			remove = append(remove, strings.TrimPrefix(t, "-"))
		}
	}
	return
}
