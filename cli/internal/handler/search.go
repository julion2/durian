package handler

import (
	"strings"

	"github.com/julion2/durian/cli/internal/mail"
	"github.com/julion2/durian/cli/internal/protocol"
	"github.com/julion2/durian/cli/internal/store"
)

// Search handles the "search" command.
// enrichLimit controls thread enrichment: 0 = off, >0 = enrich up to N threads
// (search uses limit for the result list, show uses enrichLimit for bodies).
func (h *Handler) Search(query string, limit int, enrichLimit int) protocol.Response {
	if limit == 0 {
		limit = 50
	}

	expanded, err := h.expandGroups(query)
	if err != nil {
		return protocol.FailWithMessage(protocol.ErrBackendError, "expand groups: "+err.Error())
	}

	results, err := h.store.Search(expanded, limit)
	if err != nil {
		return protocol.Fail(protocol.ErrBackendError, err)
	}

	mails := make([]mail.Mail, len(results))
	for i, r := range results {
		mails[i] = mail.Mail{
			ThreadID:  r.Thread,
			Subject:   r.Subject,
			From:      r.Authors,
			To:        r.Recipients,
			Date:      r.DateRelative,
			Timestamp: r.Timestamp,
			Tags:      strings.Join(r.Tags, ","),
		}
	}

	if enrichLimit <= 0 {
		return protocol.SuccessWithResults(mails)
	}

	// Enrich threads from store — collect all messages first, then batch-fetch
	// tags and attachments to avoid per-message N+1 queries.
	type threadMsgs struct {
		threadID string
		msgs     []*store.Message
	}
	var enriched []threadMsgs
	var allMsgIDs []int64
	for i, r := range results {
		if i >= enrichLimit {
			break
		}
		msgs, err := h.store.GetByThread(r.Thread)
		if err != nil || len(msgs) == 0 {
			continue
		}
		enriched = append(enriched, threadMsgs{r.Thread, msgs})
		for _, m := range msgs {
			allMsgIDs = append(allMsgIDs, m.ID)
		}
	}

	tagMap, _ := h.store.GetMessageTagsBatch(allMsgIDs)
	attMap, _ := h.store.GetAttachmentsByMessages(allMsgIDs)

	threads := make(map[string]*mail.ThreadContent, len(enriched))
	for _, e := range enriched {
		threads[e.threadID] = h.convertThread(e.threadID, e.msgs, true, tagMap, attMap)
	}

	return protocol.SuccessWithResultsAndThreads(mails, threads)
}
