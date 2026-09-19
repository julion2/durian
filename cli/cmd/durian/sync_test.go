package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/dbcrypto"
	"github.com/julion2/durian/cli/internal/imap"
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

func TestSyncRejectsConflictingModesBeforeConfigLoad(t *testing.T) {
	previousDownload, previousUpload := syncDownloadOnly, syncUploadOnly
	syncDownloadOnly, syncUploadOnly = true, true
	t.Cleanup(func() { syncDownloadOnly, syncUploadOnly = previousDownload, previousUpload })
	if err := runSync(syncCmd, nil); err == nil {
		t.Fatal("conflicting sync modes were accepted")
	}
}

func TestSyncRejectsForceWithoutBackfillBeforeConfigLoad(t *testing.T) {
	previousForce, previousBackfill := syncBackfillHeadersForce, syncBackfillHeaders
	syncBackfillHeadersForce, syncBackfillHeaders = true, false
	t.Cleanup(func() { syncBackfillHeadersForce, syncBackfillHeaders = previousForce, previousBackfill })
	if err := runSync(syncCmd, nil); err == nil {
		t.Fatal("--force without --backfill-headers was accepted")
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

// TestFirstSyncErrorDecidesBeforeOutput pins the ordering `durian output`
// promises: on error, stdout is empty. The JSON document used to be written
// first, so a consumer parsing stdout saw a complete-looking result while the
// command exited nonzero — the failure was visible only to whoever also
// checked the status.
func TestFirstSyncErrorDecidesBeforeOutput(t *testing.T) {
	t.Run("all succeeded", func(t *testing.T) {
		results := []*imap.SyncResult{
			{Account: "work"},
			{Account: "personal"},
		}
		if err := firstSyncError(results); err != nil {
			t.Errorf("firstSyncError = %v, want nil", err)
		}
	})

	t.Run("a later account failed", func(t *testing.T) {
		// The order matters: earlier successes must not suppress the failure,
		// which is exactly what writing their results first amounted to.
		results := []*imap.SyncResult{
			{Account: "work"},
			{Account: "personal", Error: errors.New("connection refused")},
		}
		err := firstSyncError(results)
		if err == nil {
			t.Fatal("firstSyncError = nil, want the failure of a later account")
		}
		if !strings.Contains(err.Error(), "personal") {
			t.Errorf("error %q does not name the failed account", err)
		}
		if !strings.Contains(err.Error(), "connection refused") {
			t.Errorf("error %q drops the cause", err)
		}
	})

	t.Run("no accounts", func(t *testing.T) {
		if err := firstSyncError(nil); err != nil {
			t.Errorf("firstSyncError = %v, want nil", err)
		}
	})
}
