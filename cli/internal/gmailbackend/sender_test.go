package gmailbackend

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/julion2/durian/cli/internal/mailsend"
)

func TestSenderSendsRawWithBccAndAdoptsID(t *testing.T) {
	var rawMIME []byte

	mux := http.NewServeMux()
	mux.HandleFunc("/users/me/drafts", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("send method = %s, want POST", r.Method)
		}
		var body struct {
			Message struct {
				Raw string `json:"raw"`
			} `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode send body: %v", err)
		}
		rawMIME, _ = base64.URLEncoding.DecodeString(body.Message.Raw)
		writeJSON(t, w, map[string]any{"id": "draft1", "message": map[string]string{"id": "prepared1"}})
	})
	mux.HandleFunc("/users/me/messages/prepared1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"payload": map[string]any{
				"headers": []map[string]string{{"name": "Message-Id", "value": "<gmail-assigned@mail.gmail.com>"}},
			},
		})
	})
	mux.HandleFunc("/users/me/drafts/send", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]string{"id": "sent1"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := &Sender{b: newTestBackend(t, srv)}
	m := &mailsend.Message{
		MessageID: "<ours@example.com>",
		From:      "me@gmail.com",
		To:        []string{"alice@example.com"},
		BCC:       []string{"bob@example.com"},
		Subject:   "Hi",
		Body:      "hello",
	}
	if err := s.Send(context.Background(), m); err != nil {
		t.Fatalf("Send: %v", err)
	}

	mime := string(rawMIME)
	// Bcc must ride as a MIME header for Gmail's raw send (SMTP omits it).
	if !strings.Contains(mime, "Bcc: bob@example.com") {
		t.Errorf("Bcc header missing from raw MIME:\n%s", mime)
	}
	if !strings.Contains(mime, "Subject: Hi") {
		t.Errorf("subject missing from raw MIME:\n%s", mime)
	}
	// The Message-ID Gmail stored is adopted so the local Sent row dedupes.
	if m.MessageID != "<gmail-assigned@mail.gmail.com>" {
		t.Errorf("MessageID = %q, want the adopted Gmail id", m.MessageID)
	}
}

func TestClassifyGmailSendError(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   mailsend.Kind
	}{
		{http.StatusBadRequest, "", mailsend.KindPermanent},                              // 400 -> poison
		{http.StatusForbidden, "insufficient permission", mailsend.KindPermanent},        // 403 permission
		{http.StatusForbidden, `{"reason":"rateLimitExceeded"}`, mailsend.KindTransient}, // 403 quota -> retry
		{http.StatusTooManyRequests, "", mailsend.KindTransient},                         // 429 -> retry
		{http.StatusServiceUnavailable, "", mailsend.KindTransient},                      // 503 -> retry
	}
	for _, c := range cases {
		err := classifyGmailSendError(&statusError{status: c.status, body: c.body})
		if got := mailsend.Classify(err); got != c.want {
			t.Errorf("status %d body %q classified %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

func TestSenderPersistsGmailMessageIDBeforeAmbiguousFinalSend(t *testing.T) {
	persisted := false
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me/drafts", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"id": "draft1", "message": map[string]string{"id": "prepared1"}})
	})
	mux.HandleFunc("/users/me/messages/prepared1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"payload": map[string]any{
			"headers": []map[string]string{{"name": "Message-Id", "value": "<gmail-exact@example.test>"}},
		}})
	})
	mux.HandleFunc("/users/me/drafts/send", func(http.ResponseWriter, *http.Request) {
		if !persisted {
			t.Error("final Gmail send started before Message-ID persistence")
		}
		panic(http.ErrAbortHandler)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	s := &Sender{b: newTestBackend(t, server)}
	err := s.SendAfterPersist(t.Context(), &mailsend.Message{From: "sender@example.test", To: []string{"recipient@example.test"}}, func(messageID string) error {
		if messageID != "<gmail-exact@example.test>" {
			t.Fatalf("persisted Message-ID = %q", messageID)
		}
		persisted = true
		return nil
	})
	if !persisted || mailsend.Classify(err) != mailsend.KindAmbiguous {
		t.Fatalf("final Gmail transport loss = persisted %v, error %v, kind %v", persisted, err, mailsend.Classify(err))
	}
}

func TestSenderTreatsGmailFinalServerErrorAsAmbiguous(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me/drafts", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"id": "draft1", "message": map[string]string{"id": "prepared1"}})
	})
	mux.HandleFunc("/users/me/messages/prepared1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"payload": map[string]any{
			"headers": []map[string]string{{"name": "Message-Id", "value": "<gmail-exact@example.test>"}},
		}})
	})
	mux.HandleFunc("/users/me/drafts/send", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream response lost", http.StatusServiceUnavailable)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	s := &Sender{b: newTestBackend(t, server)}
	err := s.SendAfterPersist(t.Context(), &mailsend.Message{From: "sender@example.test", To: []string{"recipient@example.test"}}, func(string) error { return nil })
	if got := mailsend.Classify(err); got != mailsend.KindAmbiguous {
		t.Fatalf("final Gmail 503 classified %v, want ambiguous: %v", got, err)
	}
}

func TestSenderDoesNotSendWhenGmailMessageIDPersistenceFails(t *testing.T) {
	persistErr := errors.New("disk unavailable")
	sent := false
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me/drafts", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"id": "draft1", "message": map[string]string{"id": "prepared1"}})
	})
	mux.HandleFunc("/users/me/messages/prepared1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"payload": map[string]any{
			"headers": []map[string]string{{"name": "Message-Id", "value": "<gmail-exact@example.test>"}},
		}})
	})
	mux.HandleFunc("/users/me/drafts/send", func(http.ResponseWriter, *http.Request) { sent = true })
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	s := &Sender{b: newTestBackend(t, server)}
	err := s.SendAfterPersist(t.Context(), &mailsend.Message{From: "sender@example.test", To: []string{"recipient@example.test"}}, func(string) error { return persistErr })
	if !errors.Is(err, persistErr) || sent {
		t.Fatalf("persistence failure = error %v, sent %v", err, sent)
	}
}
