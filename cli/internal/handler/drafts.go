package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/julion2/durian/cli/internal/store"
)

// SaveLocalDraftHandler handles PUT /api/v1/local-drafts/{id}.
func (h *Handler) SaveLocalDraftHandler(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		http.Error(w, "Missing draft ID", http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB
	var body struct {
		DraftJSON json.RawMessage `json:"draft_json"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	draft := &store.LocalDraft{
		ID:        id,
		DraftJSON: string(body.DraftJSON),
	}
	if err := h.store.SaveLocalDraft(draft); err != nil {
		slog.Error("Failed to save local draft", "module", "DRAFTS", "id", id, "err", err)
		http.Error(w, "Failed to save draft", http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{"ok": true})
}

// GetLocalDraftHandler handles GET /api/v1/local-drafts/{id}.
func (h *Handler) GetLocalDraftHandler(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	draft, err := h.store.GetLocalDraft(id)
	if err != nil {
		http.Error(w, "Draft not found", http.StatusNotFound)
		return
	}

	writeJSON(w, map[string]any{
		"id":          draft.ID,
		"draft_json":  json.RawMessage(draft.DraftJSON),
		"created_at":  draft.CreatedAt,
		"modified_at": draft.ModifiedAt,
	})
}

// DeleteLocalDraftHandler handles DELETE /api/v1/local-drafts/{id}.
func (h *Handler) DeleteLocalDraftHandler(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if err := h.store.DeleteLocalDraft(id); err != nil {
		http.Error(w, "Draft not found", http.StatusNotFound)
		return
	}

	writeJSON(w, map[string]any{"ok": true})
}

// ListLocalDraftsHandler handles GET /api/v1/local-drafts.
func (h *Handler) ListLocalDraftsHandler(w http.ResponseWriter, r *http.Request) {
	drafts, err := h.store.ListLocalDrafts()
	if err != nil {
		slog.Error("Failed to list local drafts", "module", "DRAFTS", "err", err)
		http.Error(w, "Failed to list drafts", http.StatusInternalServerError)
		return
	}

	type entry struct {
		ID         string          `json:"id"`
		DraftJSON  json.RawMessage `json:"draft_json"`
		CreatedAt  int64           `json:"created_at"`
		ModifiedAt int64           `json:"modified_at"`
	}

	entries := make([]entry, 0, len(drafts))
	for _, d := range drafts {
		entries = append(entries, entry{
			ID:         d.ID,
			DraftJSON:  json.RawMessage(d.DraftJSON),
			CreatedAt:  d.CreatedAt,
			ModifiedAt: d.ModifiedAt,
		})
	}

	writeJSON(w, entries)
}
