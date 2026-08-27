package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/dbcrypto"
	"github.com/julion2/durian/cli/internal/store"
	"github.com/julion2/durian/cli/internal/tagsync"
)

func TestSyncProgressIsDisabledForRedirectedStderr(t *testing.T) {
	previousQuiet := syncQuiet
	syncQuiet = false
	t.Cleanup(func() { syncQuiet = previousQuiet })
	if shouldShowSyncProgress() {
		t.Fatal("progress enabled while test stderr is redirected")
	}
}

func TestSyncRemoteTagsDryRunDoesNotWrite(t *testing.T) {
	keyring, err := dbcrypto.NewKeyring(bytes.Repeat([]byte{0x42}, dbcrypto.MasterKeyLen))
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	db, err := store.Open(":memory:", keyring)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Init(); err != nil {
		t.Fatalf("init store: %v", err)
	}

	now := time.Now().Unix()
	if err := db.InsertMessage(&store.Message{
		MessageID: "remote@example.com",
		Account:   "work",
		Subject:   "Dry run",
		FromAddr:  "sender@example.com",
		Date:      now,
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	db.JournalTagChange("local@example.com", "work", "todo", "add", now)
	db.SetMeta("tag_sync_at", 100)

	var posts atomic.Int32
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			posts.Add(1)
			http.Error(w, "dry-run must not push", http.StatusInternalServerError)
		case http.MethodGet:
			gets.Add(1)
			if got := r.URL.Query().Get("since"); got != "100" {
				t.Errorf("since = %q, want 100", got)
			}
			if err := json.NewEncoder(w).Encode(map[string]any{
				"changes": []tagsync.TagChange{{
					MessageID: "remote@example.com",
					Account:   "work",
					Tag:       "remote",
					Action:    "add",
				}},
				"sync_at": 200,
			}); err != nil {
				t.Errorf("encode response: %v", err)
			}
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)

	syncRemoteTags(db, &config.TagSyncConfig{URL: server.URL, APIKey: "test-key"}, true)

	if got := posts.Load(); got != 0 {
		t.Fatalf("tag-sync POST requests = %d, want 0", got)
	}
	if got := gets.Load(); got != 1 {
		t.Fatalf("tag-sync GET requests = %d, want 1", got)
	}
	journal, err := db.ReadTagJournal()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if len(journal) != 1 {
		t.Fatalf("journal entries = %d, want 1", len(journal))
	}
	tags, err := db.GetTagsByMessageID("remote@example.com")
	if err != nil {
		t.Fatalf("get tags: %v", err)
	}
	if len(tags) != 0 {
		t.Fatalf("remote tags applied during dry-run: %v", tags)
	}
	if got := db.GetMeta("tag_sync_at"); got != 100 {
		t.Fatalf("tag sync cursor = %d, want 100", got)
	}
}
