package gmailbackend

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/julion2/durian/cli/internal/mailsend"
)

func TestSenderSendsRawWithBccAndAdoptsID(t *testing.T) {
	var rawMIME []byte

	mux := http.NewServeMux()
	mux.HandleFunc("/users/me/messages/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("send method = %s, want POST", r.Method)
		}
		var body struct {
			Raw string `json:"raw"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode send body: %v", err)
		}
		rawMIME, _ = base64.URLEncoding.DecodeString(body.Raw)
		writeJSON(t, w, map[string]string{"id": "sent1"})
	})
	mux.HandleFunc("/users/me/messages/sent1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"payload": map[string]any{
				"headers": []map[string]string{{"name": "Message-Id", "value": "<gmail-assigned@mail.gmail.com>"}},
			},
		})
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

func TestSenderPreservesCanonicalRawMIME(t *testing.T) {
	want := []byte("From: me@example.com\r\nTo: you@example.com\r\nContent-Disposition: reaction\r\n\r\nemoji\r\n")
	var got []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me/messages/send", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Raw string `json:"raw"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		got, _ = base64.URLEncoding.DecodeString(body.Raw)
		writeJSON(t, w, map[string]string{"id": "sent1"})
	})
	mux.HandleFunc("/users/me/messages/sent1", func(w http.ResponseWriter, _ *http.Request) { writeJSON(t, w, map[string]any{}) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if err := (&Sender{b: newTestBackend(t, srv)}).Send(t.Context(), &mailsend.Message{RawMIME: want}); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("raw MIME changed:\n got %q\nwant %q", got, want)
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
