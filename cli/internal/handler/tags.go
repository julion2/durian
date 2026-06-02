package handler

import "github.com/julion2/durian/cli/internal/protocol"

// ListTags returns all known tags.
func (h *Handler) ListTags() protocol.Response {
	tags, err := h.store.ListTags()
	if err != nil {
		return protocol.Fail(protocol.ErrBackendError, err)
	}
	return protocol.SuccessWithTags(tags)
}

// ListTagsForAccounts returns tags scoped to specific accounts.
func (h *Handler) ListTagsForAccounts(accounts []string) protocol.Response {
	tags, err := h.store.ListTags(accounts...)
	if err != nil {
		return protocol.Fail(protocol.ErrBackendError, err)
	}
	return protocol.SuccessWithTags(tags)
}
