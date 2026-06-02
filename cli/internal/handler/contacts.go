package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/julion2/durian/cli/internal/contacts"
)

// SearchContactsHandler handles GET /api/v1/contacts/search
// Query params:
//   - query: prefix search on email/name
//   - name:  exact name match (for avatar lookup)
//   - limit: max results (default 10)
func (h *Handler) SearchContactsHandler(w http.ResponseWriter, r *http.Request) {
	if h.contacts == nil {
		http.Error(w, "contacts database not available", http.StatusServiceUnavailable)
		return
	}

	name := r.URL.Query().Get("name")
	if name != "" {
		contact, err := h.contacts.FindByExactName(name)
		if err != nil {
			slog.Error("FindByExactName failed", "module", "CONTACTS", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if contact == nil {
			writeJSON(w, []contacts.Contact{})
			return
		}
		writeJSON(w, []contacts.Contact{*contact})
		return
	}

	query := r.URL.Query().Get("query")
	if query == "" {
		http.Error(w, "missing 'query' or 'name' parameter", http.StatusBadRequest)
		return
	}

	limit := 10
	if s := r.URL.Query().Get("limit"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			limit = v
		}
	}

	results, err := h.contacts.Search(query, limit)
	if err != nil {
		slog.Error("Contact search failed", "module", "CONTACTS", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if results == nil {
		results = []contacts.Contact{}
	}
	writeJSON(w, results)
}

// ListContactsHandler handles GET /api/v1/contacts
// Query params:
//   - limit: max results (default 100)
func (h *Handler) ListContactsHandler(w http.ResponseWriter, r *http.Request) {
	if h.contacts == nil {
		http.Error(w, "contacts database not available", http.StatusServiceUnavailable)
		return
	}

	limit := 100
	if s := r.URL.Query().Get("limit"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			limit = v
		}
	}

	results, err := h.contacts.List(limit)
	if err != nil {
		slog.Error("Contact list failed", "module", "CONTACTS", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if results == nil {
		results = []contacts.Contact{}
	}
	writeJSON(w, results)
}

// IncrementContactUsageHandler handles POST /api/v1/contacts/usage
// Body: {"emails": ["a@b.c", ...]}
func (h *Handler) IncrementContactUsageHandler(w http.ResponseWriter, r *http.Request) {
	if h.contacts == nil {
		http.Error(w, "contacts database not available", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Emails []string `json:"emails"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	for _, addr := range req.Emails {
		if err := h.contacts.IncrementUsage(addr); err != nil {
			// ADR-0001 §6 redaction: do not log contact addresses; addr_len gives
			// enough signal for "is the input plausible".
			slog.Warn("Failed to increment usage", "module", "CONTACTS", "addr_len", len(addr), "err", err)
		}
	}

	writeJSON(w, struct{}{})
}
