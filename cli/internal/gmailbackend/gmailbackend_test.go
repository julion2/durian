package gmailbackend

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

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
	mux.HandleFunc("/users/me/messages", func(w http.ResponseWriter, r *http.Request) {
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
	gc := decodeCursor(result.Cursor)
	if gc.HistoryID != "12345" || gc.PageToken != "" {
		t.Errorf("cursor = %+v, want {HistoryID:12345}", gc)
	}
}

func TestFetchMessagesPaginates(t *testing.T) {
	mux := http.NewServeMux()
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

func TestFlagsAndTagLabels(t *testing.T) {
	f := flagsFromLabels([]string{"INBOX", "STARRED"})
	if !f.Seen || !f.Flagged {
		t.Errorf("flags = %+v, want Seen=true (no UNREAD) Flagged=true (STARRED)", f)
	}
	if got := tagLabels([]string{"UNREAD", "STARRED"}); got != nil {
		t.Errorf("tagLabels = %v, want nil (only reserved flag labels)", got)
	}
}

func TestWriteMethodsNotImplemented(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	b := newTestBackend(t, srv)
	if err := b.Send(context.Background(), nil); !errors.Is(err, errNotImplemented) {
		t.Errorf("Send err = %v, want errNotImplemented", err)
	}
	if _, err := b.FetchFlags(context.Background(), "ALL", nil); !errors.Is(err, errNotImplemented) {
		t.Errorf("FetchFlags err = %v, want errNotImplemented", err)
	}
}
