package tagsync

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mockStore implements MetaStore for testing.
type mockStore struct {
	data map[string]int64
}

func newMockStore() *mockStore {
	return &mockStore{data: make(map[string]int64)}
}

func (m *mockStore) GetMeta(key string) int64    { return m.data[key] }
func (m *mockStore) SetMeta(key string, v int64) { m.data[key] = v }

func newTestClient(url string) *Client {
	c := NewClient(url, "test-key")
	return c
}

// --- Push ---

func TestPush_Success(t *testing.T) {
	var received struct {
		Changes  []TagChange `json:"changes"`
		ClientID string      `json:"client_id"`
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/sync" {
			t.Errorf("path = %s, want /v1/sync", r.URL.Path)
		}
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Errorf("api key = %q, want test-key", r.Header.Get("X-API-Key"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type = %q", r.Header.Get("Content-Type"))
		}

		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	changes := []TagChange{
		{MessageID: "msg1@test", Account: "work", Tag: "inbox", Action: "add"},
	}

	err := c.Push(changes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(received.Changes) != 1 {
		t.Fatalf("received %d changes, want 1", len(received.Changes))
	}
	if received.Changes[0].MessageID != "msg1@test" {
		t.Errorf("message_id = %q", received.Changes[0].MessageID)
	}
	if received.Changes[0].ClientID == "" {
		t.Error("client_id should be populated")
	}
	if received.Changes[0].Timestamp == 0 {
		t.Error("timestamp should be populated")
	}
}

func TestPush_Empty(t *testing.T) {
	c := newTestClient("http://should-not-be-called")
	// Empty changes should return nil without making a request
	if err := c.Push(nil); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if err := c.Push([]TagChange{}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPush_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	err := c.Push([]TagChange{{MessageID: "msg1@test", Tag: "inbox", Action: "add"}})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestPush_PreservesExistingTimestamp(t *testing.T) {
	var received struct {
		Changes []TagChange `json:"changes"`
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	changes := []TagChange{
		{MessageID: "msg1@test", Tag: "inbox", Action: "add", Timestamp: 1234567890},
	}
	c.Push(changes)

	if received.Changes[0].Timestamp != 1234567890 {
		t.Errorf("timestamp = %d, want preserved 1234567890", received.Changes[0].Timestamp)
	}
}

// --- Pull ---

func TestPull_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Errorf("api key = %q", r.Header.Get("X-API-Key"))
		}
		if r.URL.Query().Get("since") != "100" {
			t.Errorf("since = %q, want 100", r.URL.Query().Get("since"))
		}

		json.NewEncoder(w).Encode(map[string]any{
			"changes": []TagChange{
				{MessageID: "remote@test", Tag: "archived", Action: "add", Timestamp: 200},
			},
			"sync_at": 200,
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	changes, syncAt, err := c.Pull(100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if syncAt != 200 {
		t.Errorf("sync_at = %d, want 200", syncAt)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1", len(changes))
	}
	if changes[0].MessageID != "remote@test" {
		t.Errorf("message_id = %q", changes[0].MessageID)
	}
	if changes[0].Action != "add" {
		t.Errorf("action = %q, want add", changes[0].Action)
	}
}

func TestPull_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	_, syncAt, err := c.Pull(0)
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if syncAt != 0 {
		t.Errorf("syncAt = %d, want original since value 0", syncAt)
	}
}

func TestPull_EmptyChanges(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"changes": []TagChange{},
			"sync_at": 500,
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	changes, syncAt, err := c.Pull(400)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("got %d changes, want 0", len(changes))
	}
	if syncAt != 500 {
		t.Errorf("sync_at = %d, want 500", syncAt)
	}
}

// --- LoadLastSync / SaveLastSync ---

func TestLoadLastSync_NilStore(t *testing.T) {
	c := newTestClient("http://unused")
	if got := c.LoadLastSync(); got != 0 {
		t.Errorf("LoadLastSync() = %d, want 0 for nil store", got)
	}
}

func TestLoadLastSync_WithStore(t *testing.T) {
	c := newTestClient("http://unused")
	store := newMockStore()
	store.data["tag_sync_at"] = 42
	c.SetStore(store)

	if got := c.LoadLastSync(); got != 42 {
		t.Errorf("LoadLastSync() = %d, want 42", got)
	}
}

func TestSaveLastSync(t *testing.T) {
	c := newTestClient("http://unused")
	store := newMockStore()
	c.SetStore(store)

	c.SaveLastSync(999)
	if store.data["tag_sync_at"] != 999 {
		t.Errorf("store value = %d, want 999", store.data["tag_sync_at"])
	}
}

func TestSaveLastSync_NilStore(t *testing.T) {
	c := newTestClient("http://unused")
	// Should not panic with nil store
	c.SaveLastSync(123)
}

// --- NewClient ---

func TestNewClient_TrimsTrailingSlash(t *testing.T) {
	c := NewClient("http://example.com/sync/", "key")
	if c.url != "http://example.com/sync" {
		t.Errorf("url = %q, want trailing slash trimmed", c.url)
	}
}

func TestNewClient_SetsClientID(t *testing.T) {
	c := NewClient("http://example.com", "key")
	if c.clientID == "" {
		t.Error("clientID should not be empty")
	}
}
