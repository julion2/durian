package handler

import (
	"log/slog"
	"strings"
	"time"

	"github.com/durian-dev/durian/cli/internal/protocol"
	"github.com/durian-dev/durian/cli/internal/tagsync"
)

// Tag handles the "tag" command.
func (h *Handler) Tag(query string, tags string) protocol.Response {
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

	// Collect thread IDs to modify
	var threadIDs []string
	if strings.HasPrefix(expanded, "thread:") {
		threadIDs = []string{strings.TrimPrefix(expanded, "thread:")}
	} else {
		// Search for matching threads
		results, err := h.store.Search(expanded, 1000000)
		if err != nil {
			return protocol.Fail(protocol.ErrBackendError, err)
		}
		for _, r := range results {
			threadIDs = append(threadIDs, r.Thread)
		}
	}

	if len(threadIDs) == 0 {
		return protocol.Success()
	}

	for _, threadID := range threadIDs {
		if err := h.store.ModifyTagsByThread(threadID, add, remove); err != nil {
			return protocol.Fail(protocol.ErrBackendError, err)
		}
		if h.tagSync != nil || h.tagSyncEnabled {
			h.journalTagChanges(threadID, add, remove)
		}
		if h.syncTrigger != nil {
			accounts, err := h.store.GetAccountsByThread(threadID)
			if err != nil {
				slog.Debug("Failed to get accounts for sync trigger", "module", "TAG", "thread", threadID, "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
			}
			for _, account := range accounts {
				h.syncTrigger.TriggerSync(account)
			}
		}
		if h.tagSync != nil {
			go h.pushTagChanges(threadID, add, remove)
		}
	}

	slog.Info("Tag operation complete", "module", "TAG", "threads", len(threadIDs), "add", add, "remove", remove)
	return protocol.Success()
}

// journalTagChanges records tag changes in the local journal for later sync.
// Uses GetAccountsByThread instead of GetByThread to avoid dedup dropping
// multi-account entries.
func (h *Handler) journalTagChanges(threadID string, add, remove []string) {
	// Get all (message_id, account) pairs without dedup
	msgs, err := h.store.GetAllByThread(threadID)
	if err != nil || len(msgs) == 0 {
		return
	}
	now := time.Now().Unix()
	for _, msg := range msgs {
		for _, tag := range add {
			h.store.JournalTagChange(msg.MessageID, msg.Account, tag, "add", now)
		}
		for _, tag := range remove {
			h.store.JournalTagChange(msg.MessageID, msg.Account, tag, "remove", now)
		}
	}
}

// pushTagChanges sends tag changes for a thread to the remote sync server.
func (h *Handler) pushTagChanges(threadID string, add, remove []string) {
	msgs, err := h.store.GetAllByThread(threadID)
	if err != nil || len(msgs) == 0 {
		return
	}

	var changes []tagsync.TagChange
	now := time.Now().Unix()
	for _, msg := range msgs {
		for _, tag := range add {
			changes = append(changes, tagsync.TagChange{
				MessageID: msg.MessageID,
				Account:   msg.Account,
				Tag:       tag,
				Action:    "add",
				Timestamp: now,
			})
		}
		for _, tag := range remove {
			changes = append(changes, tagsync.TagChange{
				MessageID: msg.MessageID,
				Account:   msg.Account,
				Tag:       tag,
				Action:    "remove",
				Timestamp: now,
			})
		}
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
