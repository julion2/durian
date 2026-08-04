package graphbackend

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/julion2/durian/cli/internal/mailsend"
)

func TestSenderCreatesDraftAndSends(t *testing.T) {
	var got graphDraft
	sent := false

	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/me/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("draft create method = %s, want POST", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode draft: %v", err)
		}
		writeJSON(t, w, map[string]string{"id": "draft1", "internetMessageId": "<graph-assigned@outlook.com>"})
	})
	mux.HandleFunc("/v1.0/me/messages/draft1/send", func(w http.ResponseWriter, _ *http.Request) {
		sent = true
		w.WriteHeader(http.StatusAccepted)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := &Sender{b: newTestBackend(t, srv)}
	m := &mailsend.Message{
		MessageID:   "<ours@example.com>",
		Subject:     "Hi",
		Body:        "<p>hello</p>",
		IsHTML:      true,
		To:          []string{"Alice <alice@example.com>"},
		CC:          []string{"carol@example.com"},
		BCC:         []string{"Bob <bob@example.com>"},
		Attachments: []mailsend.Attachment{{Filename: "a.txt", MIMEType: "text/plain", Data: []byte("data")}},
	}
	if err := s.Send(context.Background(), m); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !sent {
		t.Fatal("draft was never sent")
	}
	// The sender adopts Graph's Message-ID so the local Sent row dedupes.
	if m.MessageID != "<graph-assigned@outlook.com>" {
		t.Errorf("MessageID = %q, want Graph's <graph-assigned@outlook.com>", m.MessageID)
	}

	if got.Body.ContentType != "HTML" {
		t.Errorf("body contentType = %q, want HTML", got.Body.ContentType)
	}
	if len(got.ToRecipients) != 1 || got.ToRecipients[0].EmailAddress.Address != "alice@example.com" ||
		got.ToRecipients[0].EmailAddress.Name != "Alice" {
		t.Errorf("toRecipients = %+v", got.ToRecipients)
	}
	// BCC delivered as a typed recipient (not a MIME header) — the whole point.
	if len(got.BccRecipients) != 1 || got.BccRecipients[0].EmailAddress.Address != "bob@example.com" {
		t.Errorf("bccRecipients = %+v, want bob@example.com", got.BccRecipients)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].Name != "a.txt" ||
		got.Attachments[0].ODataType != "#microsoft.graph.fileAttachment" ||
		got.Attachments[0].ContentBytes != base64.StdEncoding.EncodeToString([]byte("data")) {
		t.Errorf("attachment = %+v", got.Attachments)
	}
}

func TestSenderUploadsLargeAttachment(t *testing.T) {
	// A file ≥ uploadThreshold must go through an upload session in chunks,
	// never inline in the draft.
	large := make([]byte, uploadThreshold+uploadChunkSize+7) // > 1 chunk
	for i := range large {
		large[i] = byte(i)
	}

	var draft graphDraft
	var uploaded []byte
	sent := false
	var srv *httptest.Server

	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/me/messages", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&draft); err != nil {
			t.Errorf("decode draft: %v", err)
		}
		writeJSON(t, w, map[string]string{"id": "d1"})
	})
	mux.HandleFunc("/v1.0/me/messages/d1/attachments/createUploadSession", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]string{"uploadUrl": srv.URL + "/upload"})
	})
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("chunk method = %s, want PUT", r.Method)
		}
		if r.Header.Get("Content-Range") == "" {
			t.Error("chunk missing Content-Range header")
		}
		b, _ := io.ReadAll(r.Body)
		uploaded = append(uploaded, b...)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1.0/me/messages/d1/send", func(w http.ResponseWriter, _ *http.Request) {
		sent = true
		w.WriteHeader(http.StatusAccepted)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	s := &Sender{b: newTestBackend(t, srv)}
	err := s.Send(context.Background(), &mailsend.Message{
		Subject:     "big",
		Body:        "x",
		To:          []string{"a@example.com"},
		Attachments: []mailsend.Attachment{{Filename: "big.bin", Data: large}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !sent {
		t.Fatal("draft was never sent")
	}
	if len(draft.Attachments) != 0 {
		t.Errorf("large attachment rode inline (%d) instead of an upload session", len(draft.Attachments))
	}
	if !bytes.Equal(uploaded, large) {
		t.Errorf("uploaded %d bytes, want %d (chunks lost/reordered?)", len(uploaded), len(large))
	}
}

func TestSenderReplyUsesCreateReply(t *testing.T) {
	createReplyCalled := false
	sent := false
	var patched graphDraft
	var srv *httptest.Server

	mux := http.NewServeMux()
	// Resolve the original message id by its RFC822 Message-ID.
	mux.HandleFunc("/v1.0/me/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("a reply must not POST a fresh draft; got %s /me/messages", r.Method)
		}
		writeJSON(t, w, map[string]any{"value": []map[string]string{{"id": "orig1"}}})
	})
	mux.HandleFunc("/v1.0/me/messages/orig1/createReply", func(w http.ResponseWriter, _ *http.Request) {
		createReplyCalled = true
		writeJSON(t, w, map[string]string{"id": "reply1", "internetMessageId": "<graph-reply@outlook.com>"})
	})
	mux.HandleFunc("/v1.0/me/messages/reply1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("draft update method = %s, want PATCH", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&patched); err != nil {
			t.Errorf("decode patch: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1.0/me/messages/reply1/send", func(w http.ResponseWriter, _ *http.Request) {
		sent = true
		w.WriteHeader(http.StatusAccepted)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	s := &Sender{b: newTestBackend(t, srv)}
	m := &mailsend.Message{
		Subject:   "Re: Hi",
		Body:      "reply body",
		InReplyTo: "<original@example.com>",
		To:        []string{"alice@example.com"},
	}
	if err := s.Send(context.Background(), m); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !createReplyCalled {
		t.Error("a reply should go through createReply (for the threading headers)")
	}
	if !sent {
		t.Error("reply draft was never sent")
	}
	if patched.Body.Content != "reply body" {
		t.Errorf("patched body = %q, want our composed reply", patched.Body.Content)
	}
	if m.MessageID != "<graph-reply@outlook.com>" {
		t.Errorf("MessageID = %q, want the reply draft's id", m.MessageID)
	}
}

func TestClassifyGraphSendError(t *testing.T) {
	cases := []struct {
		status int
		want   mailsend.Kind
	}{
		{http.StatusBadRequest, mailsend.KindPermanent},         // 400 malformed → poison
		{http.StatusForbidden, mailsend.KindPermanent},          // 403 → poison
		{http.StatusTooManyRequests, mailsend.KindTransient},    // 429 throttle → retry
		{http.StatusServiceUnavailable, mailsend.KindTransient}, // 503 → retry
	}
	for _, c := range cases {
		err := classifyGraphSendError(&statusError{status: c.status})
		if got := mailsend.Classify(err); got != c.want {
			t.Errorf("status %d classified %v, want %v", c.status, got, c.want)
		}
	}
}
