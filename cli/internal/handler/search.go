package handler

import (
	"strings"

	"github.com/julion2/durian/cli/internal/mail"
	"github.com/julion2/durian/cli/internal/protocol"
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

	// Batch the light conversations as well as their metadata. A corrupt body
	// is a failed search, not a successful page with silently missing previews.
	threadIDs := make([]string, min(len(results), enrichLimit))
	for i := range threadIDs {
		threadIDs[i] = results[i].Thread
	}
	enriched, err := h.store.GetThreadPreviews(threadIDs)
	if err != nil {
		return protocol.Fail(protocol.ErrBackendError, err)
	}
	var allMsgIDs []int64
	for _, msgs := range enriched {
		allMsgIDs = append(allMsgIDs, threadMessageRowIDs(msgs)...)
	}

	tagMap, _ := h.store.GetMessageTagsBatch(allMsgIDs)
	attMap, _ := h.store.GetAttachmentsByMessages(allMsgIDs)
	headerMap, _ := h.store.HeadersByMessageDBIDs(allMsgIDs)

	threads := make(map[string]*mail.ThreadContent, len(enriched))
	for id, msgs := range enriched {
		threads[id] = h.convertThreadWithHeaders(id, msgs, true, tagMap, attMap, headerMap)
	}

	return protocol.SuccessWithResultsAndThreads(mails, threads)
}
