package gmailbackend

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
)

// newTestBackend builds a Backend pointed at the test server with the token
// source stubbed out (no keychain, no network).
func newTestBackend(t *testing.T, srv *httptest.Server) *Backend {
	t.Helper()
	b, err := New(&config.AccountConfig{
		Name:  "test",
		Email: "test@gmail.com",
		OAuth: &config.OAuthConfig{Provider: "google", ClientID: "client-id"},
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	b.baseURL = srv.URL
	b.httpClient = srv.Client()
	b.tokenFn = func(context.Context) (string, error) { return "test-token", nil }
	return b
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

// registerProfile serves users.getProfile so the initial full sync can snapshot
// the start-of-sync historyId.
func registerProfile(t *testing.T, mux *http.ServeMux, historyID string) {
	mux.HandleFunc("/users/me/profile", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"historyId": historyID})
	})
}

func TestNewRejectsNonGoogle(t *testing.T) {
	if _, err := New(&config.AccountConfig{Email: "a@b.c"}); err == nil {
		t.Error("New() accepted an account without OAuth")
	}
	if _, err := New(&config.AccountConfig{
		Email: "a@b.c",
		OAuth: &config.OAuthConfig{Provider: "microsoft"},
	}); err == nil {
		t.Error("New() accepted a Microsoft OAuth account")
	}
}

func TestFetchMessagesListAndRaw(t *testing.T) {
	const rawMIME = "Message-ID: <abc@gmail.com>\r\nSubject: Hi\r\n\r\nBody\r\n"

	mux := http.NewServeMux()
	registerProfile(t, mux, "999")
	mux.HandleFunc("/users/me/messages", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("includeSpamTrash"); got != "true" {
			t.Errorf("includeSpamTrash = %q, want true", got)
		}
		if got := r.URL.Query().Get("maxResults"); got != "50" {
			t.Errorf("maxResults = %q, want 50", got)
		}
		writeJSON(t, w, map[string]any{
			"messages":      []map[string]string{{"id": "m1"}},
			"nextPageToken": "",
		})
	})
	mux.HandleFunc("/users/me/messages/m1", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("format"); got != "RAW" {
			t.Errorf("format = %q, want RAW", got)
		}
		writeJSON(t, w, map[string]any{
			"id":           "m1",
			"threadId":     "t1",
			"labelIds":     []string{"INBOX", "UNREAD", "Label_5"},
			"historyId":    "12345",
			"internalDate": "1710000000000",
			"raw":          base64.URLEncoding.EncodeToString([]byte(rawMIME)),
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBackend(t, srv)
	result, err := b.FetchMessages(context.Background(), allMailStream, nil, 50)
	if err != nil {
		t.Fatalf("FetchMessages: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(result.Messages))
	}
	msg := result.Messages[0]
	if string(msg.Raw) != rawMIME {
		t.Errorf("raw = %q, want the decoded RFC822", msg.Raw)
	}
	if msg.Ref.ID != "m1" {
		t.Errorf("ref id = %q, want m1", msg.Ref.ID)
	}
	// UNREAD -> not seen; no STARRED -> not flagged.
	if msg.Flags.Seen || msg.Flags.Flagged {
		t.Errorf("flags = %+v, want Seen=false Flagged=false", msg.Flags)
	}
	// Reserved flag labels are stripped; the rest become tag labels.
	if len(msg.Labels) != 2 || msg.Labels[0] != "INBOX" || msg.Labels[1] != "Label_5" {
		t.Errorf("labels = %v, want [INBOX Label_5] (UNREAD stripped to a flag)", msg.Labels)
	}
	if msg.InternalDate.UnixMilli() != 1710000000000 {
		t.Errorf("internalDate = %v, want 1710000000000 ms", msg.InternalDate.UnixMilli())
	}

	// Initial sync complete -> cursor carries the historyId for incremental resume.
	if result.HasMore {
		t.Error("HasMore = true, want false (single page)")
	}
	// Resume point is the start-of-sync snapshot (getProfile), not a per-message
	// historyId — so a change made during the sync is not missed.
	gc := decodeCursor(result.Cursor)
	if gc.HistoryID != "999" || gc.PageToken != "" {
		t.Errorf("cursor = %+v, want {HistoryID:999} (the snapshot)", gc)
	}
}

func TestFetchMessagesPaginates(t *testing.T) {
	mux := http.NewServeMux()
	registerProfile(t, mux, "999")
	mux.HandleFunc("/users/me/messages", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"messages":      []map[string]string{},
			"nextPageToken": "PAGE2",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBackend(t, srv)
	result, err := b.FetchMessages(context.Background(), allMailStream, nil, 50)
	if err != nil {
		t.Fatalf("FetchMessages: %v", err)
	}
	if !result.HasMore {
		t.Error("HasMore = false, want true (nextPageToken present)")
	}
	if gc := decodeCursor(result.Cursor); gc.PageToken != "PAGE2" {
		t.Errorf("cursor = %+v, want pageToken PAGE2 to continue paging", gc)
	}
}

func TestInitialSyncFailsOnTransientFetch(t *testing.T) {
	mux := http.NewServeMux()
	registerProfile(t, mux, "999")
	mux.HandleFunc("/users/me/messages", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"messages": []map[string]string{{"id": "m1"}}})
	})
	mux.HandleFunc("/users/me/messages/m1", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // transient, not a 404
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBackend(t, srv)
	// A transient body fetch must fail the whole call so the cursor is not
	// advanced and the engine retries — never silently dropped.
	if _, err := b.FetchMessages(context.Background(), allMailStream, nil, 50); err == nil {
		t.Error("transient message fetch failure must fail the sync, not be skipped")
	}
}

// rawMsgResp builds a users.messages.get?format=RAW response body.
func rawMsgResp(id, raw string, labels []string) map[string]any {
	return map[string]any{
		"id":           id,
		"threadId":     "t",
		"labelIds":     labels,
		"historyId":    "150",
		"internalDate": "1710000000000",
		"raw":          base64.URLEncoding.EncodeToString([]byte(raw)),
	}
}

func TestHistorySyncChangesAndDeletes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me/history", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("startHistoryId"); got != "100" {
			t.Errorf("startHistoryId = %q, want 100", got)
		}
		writeJSON(t, w, map[string]any{
			"history": []map[string]any{
				{"messagesAdded": []map[string]any{{"message": map[string]string{"id": "m2"}}}},
				{"labelsRemoved": []map[string]any{{"message": map[string]string{"id": "m3"}}}},
				{"messagesDeleted": []map[string]any{{"message": map[string]string{"id": "m4"}}}},
			},
			"historyId": "200",
		})
	})
	mux.HandleFunc("/users/me/messages/m2", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, rawMsgResp("m2", "Message-ID: <m2@gmail.com>\r\n\r\nB", []string{"INBOX"}))
	})
	mux.HandleFunc("/users/me/messages/m3", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, rawMsgResp("m3", "Message-ID: <m3@gmail.com>\r\n\r\nC", []string{"INBOX", "STARRED"}))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBackend(t, srv)
	result, err := b.FetchMessages(context.Background(), allMailStream, encodeCursor(gmailCursor{HistoryID: "100"}), 50)
	if err != nil {
		t.Fatalf("FetchMessages(history): %v", err)
	}
	// Added m2 and relabeled m3 are re-fetched (in first-seen order); m4 deleted.
	if len(result.Messages) != 2 || result.Messages[0].Ref.ID != "m2" || result.Messages[1].Ref.ID != "m3" {
		t.Fatalf("messages = %+v, want [m2 m3]", result.Messages)
	}
	if !result.Messages[1].Flags.Flagged || len(result.Messages[1].Labels) != 1 || result.Messages[1].Labels[0] != "INBOX" {
		t.Errorf("m3 = flags %+v labels %v, want Flagged (STARRED) + labels [INBOX]", result.Messages[1].Flags, result.Messages[1].Labels)
	}
	if len(result.Deleted) != 1 || result.Deleted[0].Ref.ID != "m4" {
		t.Errorf("deleted = %+v, want [m4]", result.Deleted)
	}
	if result.HasMore {
		t.Error("HasMore = true, want false")
	}
	if gc := decodeCursor(result.Cursor); gc.HistoryID != "200" {
		t.Errorf("cursor = %+v, want HistoryID 200 (new resume point)", gc)
	}
}

func TestHistoryExpiredRestartsFullSync(t *testing.T) {
	mux := http.NewServeMux()
	registerProfile(t, mux, "999")
	mux.HandleFunc("/users/me/history", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":404,"message":"historyId not found"}}`))
	})
	mux.HandleFunc("/users/me/messages", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"messages": []map[string]string{{"id": "m1"}}})
	})
	mux.HandleFunc("/users/me/messages/m1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, rawMsgResp("m1", "Message-ID: <m1@gmail.com>\r\n\r\nA", []string{"INBOX"}))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBackend(t, srv)
	// A stale historyId (404) must transparently restart the full messages.list sync.
	result, err := b.FetchMessages(context.Background(), allMailStream, encodeCursor(gmailCursor{HistoryID: "5"}), 50)
	if err != nil {
		t.Fatalf("FetchMessages after expiry: %v", err)
	}
	if len(result.Messages) != 1 || result.Messages[0].Ref.ID != "m1" {
		t.Fatalf("messages = %+v, want [m1] from the restarted full sync", result.Messages)
	}
}

func TestFlagsAndTagLabels(t *testing.T) {
	f := flagsFromLabels([]string{"INBOX", "STARRED"})
	if !f.Seen || !f.Flagged {
		t.Errorf("flags = %+v, want Seen=true (no UNREAD) Flagged=true (STARRED)", f)
	}
	if got := tagLabels([]string{"UNREAD", "STARRED"}); got != nil {
		t.Errorf("tagLabels = %v, want nil (only reserved flag labels)", got)
	}
}

func TestApplyFlags(t *testing.T) {
	var body map[string][]string
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me/messages/m1/modify", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("modify method = %s, want POST", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBackend(t, srv)
	// Mark seen and flagged: Seen -> remove UNREAD, Flagged -> add STARRED.
	if err := b.ApplyFlags(context.Background(), backend.RemoteRef{ID: "m1"},
		backend.Flags{Seen: true, Flagged: true}, backend.Flags{}); err != nil {
		t.Fatalf("ApplyFlags: %v", err)
	}
	if len(body["removeLabelIds"]) != 1 || body["removeLabelIds"][0] != "UNREAD" {
		t.Errorf("removeLabelIds = %v, want [UNREAD]", body["removeLabelIds"])
	}
	if len(body["addLabelIds"]) != 1 || body["addLabelIds"][0] != "STARRED" {
		t.Errorf("addLabelIds = %v, want [STARRED]", body["addLabelIds"])
	}
}

func TestFetchFlags(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me/messages/m1", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("format"); got != "minimal" {
			t.Errorf("format = %q, want minimal", got)
		}
		writeJSON(t, w, map[string]any{"labelIds": []string{"INBOX", "UNREAD"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBackend(t, srv)
	flags, err := b.FetchFlags(context.Background(), allMailStream, []backend.RemoteRef{{Folder: allMailStream, ID: "m1"}})
	if err != nil {
		t.Fatalf("FetchFlags: %v", err)
	}
	if f, ok := flags["m1"]; !ok || f.Seen {
		t.Errorf("flags[m1] = %+v (ok=%v), want present with Seen=false (UNREAD)", f, ok)
	}
}

func TestFetchFlagsFailsOnSystemicError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me/messages/m1", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"insufficient permissions"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBackend(t, srv)
	// A systemic 403 (not a 404) must fail the pass rather than silently
	// reporting an incomplete flag set as success.
	if _, err := b.FetchFlags(context.Background(), allMailStream, []backend.RemoteRef{{ID: "m1"}}); err == nil {
		t.Error("systemic error must fail FetchFlags, not be skipped")
	}
}

func TestWriteMethodsNotImplemented(t *testing.T) {
	b := newTestBackend(t, httptest.NewServer(http.NewServeMux()))
	if err := b.Send(context.Background(), nil); err == nil {
		t.Error("Send should report not-implemented")
	}
	if _, err := b.Move(context.Background(), backend.RemoteRef{ID: "m1"}, "Archive"); err == nil {
		t.Error("Move should report not-implemented")
	}
}
