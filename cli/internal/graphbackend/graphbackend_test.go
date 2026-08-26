package graphbackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
)

// newTestBackend builds a Backend pointed at the given test server, with the
// token source stubbed out (no keychain, no network).
func newTestBackend(t *testing.T, srv *httptest.Server) *Backend {
	t.Helper()

	account := &config.AccountConfig{
		Name:  "test",
		Email: "test@example.com",
		OAuth: &config.OAuthConfig{Provider: "microsoft", ClientID: "client-id", Tenant: "common"},
	}
	b, err := New(account)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	b.baseURL = srv.URL + "/v1.0"
	b.httpClient = srv.Client()
	b.tokenFn = func(context.Context) (string, error) { return "test-token", nil }
	return b
}

// writeJSON encodes v as the response body.
func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("failed to encode response: %v", err)
	}
}

func TestNewRejectsNonMicrosoft(t *testing.T) {
	if _, err := New(&config.AccountConfig{Email: "a@b.c"}); err == nil {
		t.Error("New() accepted an account without OAuth")
	}
	if _, err := New(&config.AccountConfig{
		Email: "a@b.c",
		OAuth: &config.OAuthConfig{Provider: "google"},
	}); err == nil {
		t.Error("New() accepted a Google OAuth account")
	}
}

func TestFetchFolders(t *testing.T) {
	// Well-known folder name -> real Graph folder id. "archive" is absent
	// (404) to exercise the tolerated-missing-folder path.
	wellKnown := map[string]string{
		"inbox":        "id-inbox",
		"sentitems":    "id-sent",
		"drafts":       "id-drafts",
		"deleteditems": "id-trash",
		"junkemail":    "id-junk",
	}

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/me/mailFolders/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/v1.0/me/mailFolders/")
		id, ok := wellKnown[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(t, w, map[string]string{"id": id})
	})
	mux.HandleFunc("/v1.0/me/mailFolders", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-token")
		}
		if r.URL.Query().Get("$skiptoken") == "" {
			writeJSON(t, w, map[string]any{
				"value": []map[string]string{
					{"id": "id-inbox", "displayName": "Inbox"},
					{"id": "id-sent", "displayName": "Sent Items"},
				},
				"@odata.nextLink": srv.URL + "/v1.0/me/mailFolders?$top=100&$skiptoken=page2",
			})
			return
		}
		writeJSON(t, w, map[string]any{
			"value": []map[string]string{
				{"id": "id-trash", "displayName": "Deleted Items"},
				{"id": "id-projects", "displayName": "Projects"},
			},
		})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBackend(t, srv)
	folders, err := b.FetchFolders(context.Background())
	if err != nil {
		t.Fatalf("FetchFolders() failed: %v", err)
	}

	if len(folders) != 4 {
		t.Fatalf("FetchFolders() returned %d folders, want 4 (paging not followed?)", len(folders))
	}

	wantRoles := map[string]backend.Role{
		"id-inbox":    backend.RoleInbox,
		"id-sent":     backend.RoleSent,
		"id-trash":    backend.RoleTrash,
		"id-projects": backend.RoleNone,
	}
	wantDisplay := map[string]string{
		"id-inbox":    "Inbox",
		"id-sent":     "Sent Items",
		"id-trash":    "Deleted Items",
		"id-projects": "Projects",
	}
	for _, f := range folders {
		if f.Role != wantRoles[f.Name] {
			t.Errorf("folder %s: Role = %q, want %q", f.Name, f.Role, wantRoles[f.Name])
		}
		if f.Display != wantDisplay[f.Name] {
			t.Errorf("folder %s: Display = %q, want %q", f.Name, f.Display, wantDisplay[f.Name])
		}
		if !f.Selectable {
			t.Errorf("folder %s: Selectable = false, want true", f.Name)
		}
	}
}

func TestFetchMessagesRecoversFromExpiredToken(t *testing.T) {
	const rawMIME = "Message-ID: <abc@example.com>\r\nSubject: Hi\r\n\r\nBody\r\n"
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/me/mailFolders/folder1/messages/delta", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("$deltatoken") == "stale" {
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write([]byte(`{"error":{"code":"SyncStateNotFound","message":"sync state generation is not found"}}`))
			return
		}
		// Fresh restart (no stale token) returns the folder's messages.
		writeJSON(t, w, map[string]any{
			"value": []map[string]any{
				{"id": "msg1", "internetMessageId": "<abc@example.com>", "isRead": true},
			},
			"@odata.deltaLink": srv.URL + "/v1.0/me/mailFolders/folder1/messages/delta?$deltatoken=fresh",
		})
	})
	mux.HandleFunc("/v1.0/me/messages/msg1/$value", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(rawMIME))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBackend(t, srv)
	stale := backend.Cursor(srv.URL + "/v1.0/me/mailFolders/folder1/messages/delta?$deltatoken=stale")
	result, err := b.FetchMessages(context.Background(), "folder1", stale, 50)
	if err != nil {
		t.Fatalf("FetchMessages should recover from 410 SyncStateNotFound, got: %v", err)
	}
	if len(result.Messages) != 1 || result.Messages[0].Ref.ID != "msg1" {
		t.Fatalf("expected 1 recovered message, got %+v", result.Messages)
	}
	if !result.FullSnapshot || len(result.Present) != 1 || result.Present[0].ID != "msg1" {
		t.Fatalf("expired-token recovery is not authoritative: %+v", result)
	}
}

func TestFetchMessagesRejectsOffOriginCursorBeforeAuthorization(t *testing.T) {
	var attackerCalls int32
	attacker := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&attackerCalls, 1)
	}))
	defer attacker.Close()
	trusted := httptest.NewServer(http.NotFoundHandler())
	defer trusted.Close()
	b := newTestBackend(t, trusted)
	tokenCalls := 0
	b.tokenFn = func(context.Context) (string, error) {
		tokenCalls++
		return "secret", nil
	}

	_, err := b.FetchMessages(t.Context(), "folder1", backend.Cursor(attacker.URL+"/steal"), 50)
	if err == nil || !strings.Contains(err.Error(), "origin differs") {
		t.Fatalf("FetchMessages() error = %v, want off-origin rejection", err)
	}
	if tokenCalls != 0 || atomic.LoadInt32(&attackerCalls) != 0 {
		t.Fatalf("token calls=%d attacker calls=%d, want request rejected before authorization", tokenCalls, attackerCalls)
	}
}

func TestAuthenticatedRequestRejectsOffOriginRedirect(t *testing.T) {
	var attackerCalls int32
	attacker := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&attackerCalls, 1)
	}))
	defer attacker.Close()
	trusted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/steal", http.StatusFound)
	}))
	defer trusted.Close()
	b, err := New(&config.AccountConfig{
		Email: "test@example.com", OAuth: &config.OAuthConfig{Provider: "microsoft"},
	})
	if err != nil {
		t.Fatal(err)
	}
	b.baseURL = trusted.URL
	b.tokenFn = func(context.Context) (string, error) { return "secret", nil }

	_, err = b.do(t.Context(), http.MethodGet, trusted.URL+"/redirect", nil)
	if err == nil || !strings.Contains(err.Error(), "origin differs") {
		t.Fatalf("do() error = %v, want redirect origin rejection", err)
	}
	if atomic.LoadInt32(&attackerCalls) != 0 {
		t.Fatal("off-origin redirect target was contacted")
	}
}

func TestDecodeJSONLimitedRejectsOversizedResponse(t *testing.T) {
	var out map[string]any
	err := decodeJSONLimited(strings.NewReader(`{"ok":true}x`), int64(len(`{"ok":true}`)), &out)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("decodeJSONLimited() error = %v", err)
	}
}

func TestMailboxRouting(t *testing.T) {
	own, err := New(&config.AccountConfig{
		Email: "me@example.com",
		OAuth: &config.OAuthConfig{Provider: "microsoft"},
	})
	if err != nil {
		t.Fatalf("New(own): %v", err)
	}
	if own.mailbox != "/me" {
		t.Errorf("own mailbox = %q, want /me", own.mailbox)
	}

	// A shared mailbox (Email) accessed with a delegating user's token (AuthEmail)
	// must route to /users/{address}, not /me (which would sync the token owner).
	shared, err := New(&config.AccountConfig{
		Email:     "shared@example.com",
		AuthEmail: "me@example.com",
		OAuth:     &config.OAuthConfig{Provider: "microsoft"},
	})
	if err != nil {
		t.Fatalf("New(shared): %v", err)
	}
	if shared.mailbox != "/users/shared@example.com" {
		t.Errorf("delegated mailbox = %q, want /users/shared@example.com", shared.mailbox)
	}
}

func TestFetchMessagesConcurrentBodiesBounded(t *testing.T) {
	const n = 15
	var inFlight, maxInFlight int32
	var srv *httptest.Server

	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/me/mailFolders/f/messages/delta", func(w http.ResponseWriter, _ *http.Request) {
		items := make([]map[string]any, 0, n)
		for i := 0; i < n; i++ {
			items = append(items, map[string]any{
				"id":                fmt.Sprintf("m%d", i),
				"internetMessageId": fmt.Sprintf("<m%d@example.com>", i),
				"receivedDateTime":  "2026-07-01T10:00:00Z",
			})
		}
		writeJSON(t, w, map[string]any{"value": items, "@odata.deltaLink": srv.URL + "/done"})
	})
	mux.HandleFunc("/v1.0/me/messages/", func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt32(&inFlight, 1)
		for { // record the high-water mark of simultaneous fetches
			old := atomic.LoadInt32(&maxInFlight)
			if cur <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
				break
			}
		}
		time.Sleep(3 * time.Millisecond) // widen the overlap window
		atomic.AddInt32(&inFlight, -1)

		_, _ = w.Write([]byte("Subject: hi\r\n\r\nbody"))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBackend(t, srv)
	result, err := b.FetchMessages(context.Background(), "f", nil, 50)
	if err != nil {
		t.Fatalf("FetchMessages: %v", err)
	}
	if len(result.Messages) != n {
		t.Errorf("got %d messages, want %d", len(result.Messages), n)
	}
	if maxInFlight > fetchConcurrency {
		t.Errorf("max in-flight %d exceeded bound %d", maxInFlight, fetchConcurrency)
	}
	if maxInFlight < 2 {
		t.Errorf("max in-flight %d — bodies fetched serially, not concurrently", maxInFlight)
	}
}

func TestFetchMessagesHoldsCursorWhenBodyFetchFails(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/me/mailFolders/f/messages/delta", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"value":            []map[string]any{{"id": "m1", "internetMessageId": "<m1@example.com>"}},
			"@odata.deltaLink": srv.URL + "/done",
		})
	})
	mux.HandleFunc("/v1.0/me/messages/m1/$value", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBackend(t, srv)
	if _, err := b.FetchMessages(t.Context(), "f", nil, 50); err == nil {
		t.Fatal("FetchMessages() succeeded after body fetch failed")
	}
}

func TestFetchMessagesMarksForbiddenBodyUnavailable(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/me/mailFolders/f/messages/delta", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("$deltatoken") == "stale" {
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write([]byte(`{"error":{"code":"SyncStateNotFound"}}`))
			return
		}
		writeJSON(t, w, map[string]any{
			"value":            []map[string]any{{"id": "protected", "internetMessageId": "<protected@example.com>"}},
			"@odata.deltaLink": srv.URL + "/done",
		})
	})
	mux.HandleFunc("/v1.0/me/messages/protected/$value", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBackend(t, srv)
	stale := backend.Cursor(srv.URL + "/v1.0/me/mailFolders/f/messages/delta?$deltatoken=stale")
	result, err := b.FetchMessages(t.Context(), "f", stale, 50)
	if err != nil {
		t.Fatalf("FetchMessages() error = %v", err)
	}
	if len(result.Messages) != 0 || len(result.Present) != 1 || result.Present[0].ID != "protected" {
		t.Fatalf("replacement result = %+v", result)
	}
	if len(result.Unavailable) != 1 || result.Unavailable[0].ID != "protected" {
		t.Fatalf("Unavailable = %+v, want protected", result.Unavailable)
	}
}

func TestFetchMessagesDelta(t *testing.T) {
	const rawMIME = "Message-ID: <abc@example.com>\r\nSubject: Hi\r\n\r\nBody\r\n"

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/me/mailFolders/folder1/messages/delta", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("$select"); got != deltaSelect {
			t.Errorf("delta $select = %q, want %q", got, deltaSelect)
		}
		writeJSON(t, w, map[string]any{
			"value": []map[string]any{
				{
					"id":                "msg1",
					"internetMessageId": "<abc@example.com>",
					"isRead":            true,
					"flag":              map[string]string{"flagStatus": "flagged"},
					"categories":        []string{"Blue"},
					"receivedDateTime":  "2026-07-01T10:00:00Z",
				},
				{
					"id":       "gone1",
					"@removed": map[string]string{"reason": "deleted"},
				},
			},
			"@odata.deltaLink": srv.URL + "/v1.0/me/mailFolders/folder1/messages/delta?$deltatoken=final",
		})
	})
	mux.HandleFunc("/v1.0/me/messages/msg1/$value", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(rawMIME))
	})
	mux.HandleFunc("/v1.0/nextpage", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"value":            []map[string]any{},
			"@odata.nextLink":  srv.URL + "/v1.0/nextpage2",
			"@odata.deltaLink": "",
		})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBackend(t, srv)

	// Initial call (empty cursor) -> deltaLink, no more pages.
	result, err := b.FetchMessages(context.Background(), "folder1", nil, 50)
	if err != nil {
		t.Fatalf("FetchMessages() failed: %v", err)
	}

	if len(result.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(result.Messages))
	}
	msg := result.Messages[0]
	if msg.MessageID != "abc@example.com" {
		t.Errorf("MessageID = %q, want %q (angle brackets not stripped?)", msg.MessageID, "abc@example.com")
	}
	if string(msg.Raw) != rawMIME {
		t.Errorf("Raw = %q, want %q", msg.Raw, rawMIME)
	}
	if msg.Ref.Folder != "folder1" || msg.Ref.ID != "msg1" {
		t.Errorf("Ref = %+v, want {folder1 msg1}", msg.Ref)
	}
	wantFlags := backend.Flags{Seen: true, Flagged: true}
	if msg.Flags != wantFlags {
		t.Errorf("Flags = %+v, want %+v", msg.Flags, wantFlags)
	}
	if len(msg.Labels) != 1 || msg.Labels[0] != "Blue" {
		t.Errorf("Labels = %v, want [Blue]", msg.Labels)
	}
	wantDate := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	if !msg.InternalDate.Equal(wantDate) {
		t.Errorf("InternalDate = %v, want %v", msg.InternalDate, wantDate)
	}

	if len(result.Deleted) != 1 {
		t.Fatalf("got %d deletions, want 1", len(result.Deleted))
	}
	if result.Deleted[0].Ref.ID != "gone1" || result.Deleted[0].Ref.Folder != "folder1" {
		t.Errorf("Deleted[0].Ref = %+v, want {folder1 gone1}", result.Deleted[0].Ref)
	}

	wantCursor := srv.URL + "/v1.0/me/mailFolders/folder1/messages/delta?$deltatoken=final"
	if string(result.Cursor) != wantCursor {
		t.Errorf("Cursor = %q, want deltaLink %q", result.Cursor, wantCursor)
	}
	if result.HasMore {
		t.Error("HasMore = true, want false (deltaLink page)")
	}

	// Cursor pointing at a page with a nextLink -> HasMore, cursor = nextLink.
	result, err = b.FetchMessages(context.Background(), "folder1", backend.Cursor(srv.URL+"/v1.0/nextpage"), 50)
	if err != nil {
		t.Fatalf("FetchMessages(nextpage) failed: %v", err)
	}
	if !result.HasMore {
		t.Error("HasMore = false, want true (nextLink page)")
	}
	if want := srv.URL + "/v1.0/nextpage2"; string(result.Cursor) != want {
		t.Errorf("Cursor = %q, want nextLink %q", result.Cursor, want)
	}
}

func TestFetchFlags(t *testing.T) {
	// Message id -> flag state served via $batch; "gone1" 404s.
	messages := map[string]map[string]any{
		"msg1": {"id": "msg1", "isRead": true, "flag": map[string]string{"flagStatus": "flagged"}},
	}

	var batchCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/$batch", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&batchCalls, 1)
		if r.Method != http.MethodPost {
			t.Errorf("batch method = %s, want POST", r.Method)
		}
		var envelope struct {
			Requests []struct {
				ID     string `json:"id"`
				Method string `json:"method"`
				URL    string `json:"url"`
			} `json:"requests"`
		}
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			t.Fatalf("failed to decode batch request: %v", err)
		}
		if len(envelope.Requests) > 20 {
			t.Errorf("batch carried %d sub-requests, want <= 20", len(envelope.Requests))
		}

		var responses []map[string]any
		for _, req := range envelope.Requests {
			if req.Method != http.MethodGet {
				t.Errorf("sub-request method = %s, want GET", req.Method)
			}
			if !strings.HasPrefix(req.URL, "/me/messages/") {
				t.Errorf("sub-request url = %q, want Graph-relative /me/messages/... path", req.URL)
			}
			msgID := strings.TrimPrefix(req.URL, "/me/messages/")
			msgID = strings.SplitN(msgID, "?", 2)[0]
			if body, ok := messages[msgID]; ok {
				responses = append(responses, map[string]any{"id": req.ID, "status": 200, "body": body})
			} else {
				responses = append(responses, map[string]any{"id": req.ID, "status": 404})
			}
		}
		writeJSON(t, w, map[string]any{"responses": responses})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBackend(t, srv)

	// One resolvable message plus one that 404s (deleted server-side).
	flags, err := b.FetchFlags(context.Background(), "folder1", []backend.RemoteRef{
		{Folder: "folder1", ID: "msg1"},
		{Folder: "folder1", ID: "gone1"},
	})
	if err != nil {
		t.Fatalf("FetchFlags() failed: %v", err)
	}
	if len(flags) != 1 {
		t.Fatalf("got %d flag entries, want 1 (404 not skipped?): %v", len(flags), flags)
	}
	want := backend.Flags{Seen: true, Flagged: true}
	if got, ok := flags["msg1"]; !ok || got != want {
		t.Errorf("flags[msg1] = %+v (present=%v), want %+v", got, ok, want)
	}
	if _, ok := flags["gone1"]; ok {
		t.Error("flags contains gone1, want 404 message absent from map")
	}

	// More than 20 refs must be chunked into multiple $batch calls.
	atomic.StoreInt32(&batchCalls, 0)
	var refs []backend.RemoteRef
	for i := 0; i < 45; i++ {
		id := fmt.Sprintf("bulk%d", i)
		messages[id] = map[string]any{"id": id, "isRead": true}
		refs = append(refs, backend.RemoteRef{Folder: "folder1", ID: id})
	}
	flags, err = b.FetchFlags(context.Background(), "folder1", refs)
	if err != nil {
		t.Fatalf("FetchFlags(45 refs) failed: %v", err)
	}
	if got := atomic.LoadInt32(&batchCalls); got != 3 {
		t.Errorf("server saw %d $batch calls for 45 refs, want 3 (chunks of 20)", got)
	}
	if len(flags) != 45 {
		t.Errorf("got %d flag entries, want 45", len(flags))
	}
	if got := flags["bulk44"]; !got.Seen {
		t.Errorf("flags[bulk44] = %+v, want Seen=true (last chunk not fetched?)", got)
	}
}

func TestFetchFlagsDoesNotTreatTransientSubresponseAsMissing(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		writeJSON(t, w, map[string]any{"responses": []map[string]any{{"id": "0", "status": 429}}})
	}))
	defer srv.Close()
	b := newTestBackend(t, srv)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := b.FetchFlags(ctx, "folder1", []backend.RemoteRef{{Folder: "folder1", ID: "msg1"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FetchFlags() error = %v, want context deadline while backing off", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("batch calls = %d, want 1 before cancellation", got)
	}
}

func TestFetchFlagsSkipsForbiddenSubresponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"responses": []map[string]any{{"id": "0", "status": 403}}})
	}))
	defer srv.Close()
	b := newTestBackend(t, srv)
	flags, err := b.FetchFlags(t.Context(), "folder1", []backend.RemoteRef{{Folder: "folder1", ID: "msg1"}})
	if err != nil || len(flags) != 0 {
		t.Fatalf("FetchFlags() flags=%v error=%v, want inaccessible item skipped", flags, err)
	}
}

func TestApplyFlags(t *testing.T) {
	type patch struct {
		IsRead *bool `json:"isRead"`
		Flag   *struct {
			FlagStatus string `json:"flagStatus"`
		} `json:"flag"`
	}

	tests := []struct {
		name           string
		add, remove    backend.Flags
		wantRequest    bool
		wantIsRead     *bool  // nil = isRead must be absent from the body
		wantFlagStatus string // "" = flag must be absent from the body
	}{
		{
			name:        "add Seen",
			add:         backend.Flags{Seen: true},
			wantRequest: true,
			wantIsRead:  boolPtr(true),
		},
		{
			name:        "remove Seen",
			remove:      backend.Flags{Seen: true},
			wantRequest: true,
			wantIsRead:  boolPtr(false),
		},
		{
			name:           "add Flagged",
			add:            backend.Flags{Flagged: true},
			wantRequest:    true,
			wantFlagStatus: "flagged",
		},
		{
			name:           "remove Flagged",
			remove:         backend.Flags{Flagged: true},
			wantRequest:    true,
			wantFlagStatus: "notFlagged",
		},
		{
			name:           "add Completed",
			add:            backend.Flags{Completed: true},
			wantRequest:    true,
			wantFlagStatus: "complete",
		},
		{
			name:        "empty add/remove makes no request",
			wantRequest: false,
		},
		{
			name:        "Answered and Deleted are ignored",
			add:         backend.Flags{Answered: true, Deleted: true},
			wantRequest: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			var got patch
			mux := http.NewServeMux()
			mux.HandleFunc("/v1.0/me/messages/msg1", func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				if r.Method != http.MethodPatch {
					t.Errorf("method = %s, want PATCH", r.Method)
				}
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("failed to decode PATCH body: %v", err)
				}
				w.WriteHeader(http.StatusOK)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			b := newTestBackend(t, srv)
			ref := backend.RemoteRef{Folder: "folder1", ID: "msg1"}
			if err := b.ApplyFlags(context.Background(), ref, tc.add, tc.remove); err != nil {
				t.Fatalf("ApplyFlags() failed: %v", err)
			}

			if !tc.wantRequest {
				if n := atomic.LoadInt32(&calls); n != 0 {
					t.Errorf("server saw %d requests, want 0 for a no-op flag change", n)
				}
				return
			}
			if n := atomic.LoadInt32(&calls); n != 1 {
				t.Fatalf("server saw %d requests, want 1", n)
			}

			switch {
			case tc.wantIsRead == nil && got.IsRead != nil:
				t.Errorf("PATCH body isRead = %v, want field absent", *got.IsRead)
			case tc.wantIsRead != nil && (got.IsRead == nil || *got.IsRead != *tc.wantIsRead):
				t.Errorf("PATCH body isRead = %v, want %v", got.IsRead, *tc.wantIsRead)
			}
			switch {
			case tc.wantFlagStatus == "" && got.Flag != nil:
				t.Errorf("PATCH body flag = %+v, want field absent", *got.Flag)
			case tc.wantFlagStatus != "" && (got.Flag == nil || got.Flag.FlagStatus != tc.wantFlagStatus):
				t.Errorf("PATCH body flag = %+v, want flagStatus %q", got.Flag, tc.wantFlagStatus)
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }

func TestMove(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/me/messages/old-id/move", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		var body struct {
			DestinationID string `json:"destinationId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("failed to decode move body: %v", err)
		}
		if body.DestinationID != "id-archive" {
			t.Errorf("destinationId = %q, want %q", body.DestinationID, "id-archive")
		}
		// Graph reassigns the message id on move.
		writeJSON(t, w, map[string]any{"id": "new-id", "isRead": true})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBackend(t, srv)
	ref := backend.RemoteRef{Folder: "id-inbox", ID: "old-id"}
	moved, err := b.Move(context.Background(), ref, "id-archive")
	if err != nil {
		t.Fatalf("Move() failed: %v", err)
	}
	want := backend.RemoteRef{Folder: "id-archive", ID: "new-id"}
	if moved != want {
		t.Errorf("Move() = %+v, want %+v (Graph reassigns the id on move)", moved, want)
	}
}

// TestMoveGoneRef proves a 404 from the move endpoint surfaces as
// backend.ErrRefGone: the source id died (moved or deleted by another client)
// and no retry can bring it back, so the engine must reconcile rather than
// keep failing the sync.
func TestMoveGoneRef(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/me/messages/dead-id/move", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(t, w, map[string]any{"error": map[string]any{
			"code":    "ErrorItemNotFound",
			"message": "The specified object was not found in the store.",
		}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBackend(t, srv)
	ref := backend.RemoteRef{Folder: "id-inbox", ID: "dead-id"}
	_, err := b.Move(context.Background(), ref, "id-archive")
	if err == nil {
		t.Fatal("Move() succeeded, want error")
	}
	if !errors.Is(err, backend.ErrRefGone) {
		t.Errorf("Move() error = %v, want one wrapping backend.ErrRefGone", err)
	}
	if !strings.Contains(err.Error(), "ErrorItemNotFound") {
		t.Errorf("Move() error = %v, want the Graph error body preserved", err)
	}
}

func TestThrottleRetry(t *testing.T) {
	var calls int32
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/me/mailFolders/f/messages/delta", func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeJSON(t, w, map[string]any{
			"value":            []map[string]any{},
			"@odata.deltaLink": srv.URL + "/v1.0/me/mailFolders/f/messages/delta?$deltatoken=done",
		})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	b := newTestBackend(t, srv)
	result, err := b.FetchMessages(context.Background(), "f", nil, 0)
	if err != nil {
		t.Fatalf("FetchMessages() failed after throttle: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("server saw %d calls, want 2 (429 then retry)", got)
	}
	if want := srv.URL + "/v1.0/me/mailFolders/f/messages/delta?$deltatoken=done"; string(result.Cursor) != want {
		t.Errorf("Cursor = %q, want %q", result.Cursor, want)
	}
}

func TestMutationIsNotRetriedAfterThrottle(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	b := newTestBackend(t, srv)
	resp, err := b.do(t.Context(), http.MethodPost, srv.URL+"/mutation", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("mutation calls = %d, want 1", got)
	}
}
