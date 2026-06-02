package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"github.com/julion2/durian/cli/internal/store"
)

// newDraftsRouter wires only the local-draft routes. The main newTestRouter
// helper doesn't include them; keeping draft routes here avoids expanding
// that helper for test-only use.
func newDraftsRouter(h *Handler) *mux.Router {
	r := mux.NewRouter()
	r.HandleFunc("/api/v1/local-drafts", h.ListLocalDraftsHandler).Methods("GET")
	r.HandleFunc("/api/v1/local-drafts/{id}", h.GetLocalDraftHandler).Methods("GET")
	r.HandleFunc("/api/v1/local-drafts/{id}", h.SaveLocalDraftHandler).Methods("PUT")
	r.HandleFunc("/api/v1/local-drafts/{id}", h.DeleteLocalDraftHandler).Methods("DELETE")
	return r
}

// --- Save ---

func TestSaveLocalDraftHandler_OK(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newDraftsRouter(h)

	body := `{"draft_json":{"subject":"Hi","to":["bob@example.com"]}}`
	req := httptest.NewRequest("PUT", "/api/v1/local-drafts/abc-123", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}

	// Verify the draft was persisted
	got, err := db.GetLocalDraft("abc-123")
	if err != nil {
		t.Fatalf("GetLocalDraft: %v", err)
	}
	if !strings.Contains(got.DraftJSON, "bob@example.com") {
		t.Errorf("draft_json = %q, expected recipient", got.DraftJSON)
	}
}

func TestSaveLocalDraftHandler_InvalidBody(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newDraftsRouter(h)

	req := httptest.NewRequest("PUT", "/api/v1/local-drafts/abc", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// --- Get ---

func TestGetLocalDraftHandler_OK(t *testing.T) {
	db := newTestStore(t)
	if err := db.SaveLocalDraft(&store.LocalDraft{
		ID:        "fetch-me",
		DraftJSON: `{"subject":"test"}`,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h := New(db, nil)
	r := newDraftsRouter(h)

	req := httptest.NewRequest("GET", "/api/v1/local-drafts/fetch-me", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["id"] != "fetch-me" {
		t.Errorf("id = %v, want fetch-me", resp["id"])
	}
	if _, ok := resp["created_at"]; !ok {
		t.Error("response missing created_at")
	}
	if _, ok := resp["modified_at"]; !ok {
		t.Error("response missing modified_at")
	}
}

func TestGetLocalDraftHandler_NotFound(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newDraftsRouter(h)

	req := httptest.NewRequest("GET", "/api/v1/local-drafts/nope", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// --- Delete ---

func TestDeleteLocalDraftHandler_OK(t *testing.T) {
	db := newTestStore(t)
	if err := db.SaveLocalDraft(&store.LocalDraft{
		ID: "del", DraftJSON: `{}`,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h := New(db, nil)
	r := newDraftsRouter(h)

	req := httptest.NewRequest("DELETE", "/api/v1/local-drafts/del", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	if _, err := db.GetLocalDraft("del"); err == nil {
		t.Error("draft still exists after DELETE")
	}
}

func TestDeleteLocalDraftHandler_NotFound(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newDraftsRouter(h)

	req := httptest.NewRequest("DELETE", "/api/v1/local-drafts/nope", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// --- List ---

func TestListLocalDraftsHandler_Empty(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newDraftsRouter(h)

	req := httptest.NewRequest("GET", "/api/v1/local-drafts", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var entries []map[string]any
	if err := json.NewDecoder(w.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries, want 0", len(entries))
	}
}

func TestListLocalDraftsHandler_WithEntries(t *testing.T) {
	db := newTestStore(t)
	for _, id := range []string{"a", "b", "c"} {
		if err := db.SaveLocalDraft(&store.LocalDraft{
			ID: id, DraftJSON: `{"id":"` + id + `"}`,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	h := New(db, nil)
	r := newDraftsRouter(h)

	req := httptest.NewRequest("GET", "/api/v1/local-drafts", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var entries []map[string]any
	if err := json.NewDecoder(w.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("got %d entries, want 3", len(entries))
	}
	// Each entry must carry id + draft_json + timestamps
	for _, e := range entries {
		for _, key := range []string{"id", "draft_json", "created_at", "modified_at"} {
			if _, ok := e[key]; !ok {
				t.Errorf("entry missing %q: %v", key, e)
			}
		}
	}
}

// --- Save + round-trip via handler ---

func TestSaveThenGetLocalDraft_Roundtrip(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newDraftsRouter(h)

	// Save
	saveBody := `{"draft_json":{"subject":"Round trip","to":["a@b.com"],"body":"hi"}}`
	saveReq := httptest.NewRequest("PUT", "/api/v1/local-drafts/rt", bytes.NewReader([]byte(saveBody)))
	saveRes := httptest.NewRecorder()
	r.ServeHTTP(saveRes, saveReq)
	if saveRes.Code != http.StatusOK {
		t.Fatalf("save status = %d", saveRes.Code)
	}

	// Get
	getReq := httptest.NewRequest("GET", "/api/v1/local-drafts/rt", nil)
	getRes := httptest.NewRecorder()
	r.ServeHTTP(getRes, getReq)
	if getRes.Code != http.StatusOK {
		t.Fatalf("get status = %d", getRes.Code)
	}

	var resp struct {
		ID        string          `json:"id"`
		DraftJSON json.RawMessage `json:"draft_json"`
	}
	if err := json.NewDecoder(getRes.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != "rt" {
		t.Errorf("id = %q", resp.ID)
	}
	if !strings.Contains(string(resp.DraftJSON), "Round trip") {
		t.Errorf("draft_json missing subject: %s", resp.DraftJSON)
	}
}
