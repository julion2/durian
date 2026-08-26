package jmapbackend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
)

const testRaw = "From: Alice <alice@example.test>\r\nTo: Me <me@example.test>\r\nSubject: hello\r\nMessage-ID: <m1@example.test>\r\nDate: Tue, 25 Aug 2026 12:00:00 +0000\r\n\r\nbody\r\n"

type testJMAPServer struct {
	t      *testing.T
	server *httptest.Server

	handler  func(string, map[string]interface{}) interface{}
	uploaded []byte
	events   string
	extra    []interface{}
}

func newTestJMAPServer(t *testing.T) *testJMAPServer {
	t.Helper()
	s := &testJMAPServer{t: t}
	s.server = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	t.Cleanup(s.server.Close)
	return s
}

func (s *testJMAPServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if user, password, ok := r.BasicAuth(); !ok || user != "me@example.test" || password != "secret" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == "/session":
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"capabilities":    map[string]interface{}{coreCapability: map[string]interface{}{}, mailCapability: map[string]interface{}{}, submissionCapability: map[string]interface{}{}},
			"accounts":        map[string]interface{}{"a1": map[string]interface{}{"name": "Test", "isPersonal": true, "isReadOnly": false, "accountCapabilities": map[string]interface{}{mailCapability: map[string]interface{}{}, submissionCapability: map[string]interface{}{}}}},
			"primaryAccounts": map[string]string{mailCapability: "a1", submissionCapability: "a1"},
			"apiUrl":          s.server.URL + "/api", "downloadUrl": s.server.URL + "/download/{accountId}/{blobId}/{name}?accept={type}",
			"uploadUrl": s.server.URL + "/upload/{accountId}", "eventSourceUrl": s.server.URL + "/events",
		})
	case r.URL.Path == "/api":
		var envelope struct {
			MethodCalls []json.RawMessage `json:"methodCalls"`
		}
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil || len(envelope.MethodCalls) != 1 {
			s.t.Errorf("decode method call: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var tuple []json.RawMessage
		_ = json.Unmarshal(envelope.MethodCalls[0], &tuple)
		var method string
		var args map[string]interface{}
		_ = json.Unmarshal(tuple[0], &method)
		_ = json.Unmarshal(tuple[1], &args)
		result := interface{}(map[string]interface{}{})
		if s.handler != nil {
			result = s.handler(method, args)
		}
		responses := []interface{}{[]interface{}{method, result, "0"}}
		responses = append(responses, s.extra...)
		s.extra = nil
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"methodResponses": responses, "sessionState": "session-1"})
	case strings.HasPrefix(r.URL.Path, "/download/"):
		_, _ = io.WriteString(w, testRaw)
	case strings.HasPrefix(r.URL.Path, "/upload/"):
		s.uploaded, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"accountId": "a1", "blobId": "uploaded-blob", "type": "message/rfc822", "size": len(s.uploaded)})
	case r.URL.Path == "/events":
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, s.events)
	default:
		http.NotFound(w, r)
	}
}

func (s *testJMAPServer) backend(t *testing.T) *Backend {
	t.Helper()
	original := getCredential
	getCredential = func(_, _ string) (string, error) { return "secret", nil }
	t.Cleanup(func() { getCredential = original })
	b, err := New(&config.AccountConfig{
		Name: "Test", Email: "me@example.test", Alias: "test", SyncEngine: "jmap",
		JMAP: &config.JMAPConfig{SessionURL: s.server.URL + "/session", Auth: "password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testMailboxes() []map[string]interface{} {
	return []map[string]interface{}{
		{"id": "inbox-id", "name": "Inbox", "role": "inbox", "isSubscribed": true},
		{"id": "archive-id", "name": "Archive", "role": "archive", "isSubscribed": true},
		{"id": "drafts-id", "name": "Drafts", "role": "drafts", "isSubscribed": true},
		{"id": "sent-id", "name": "Sent", "role": "sent", "isSubscribed": true},
		{"id": "project-id", "name": "Project X", "role": nil, "isSubscribed": true},
	}
}

func emailObject(id string, keywords map[string]bool, mailboxes map[string]bool) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "blobId": "blob-" + id, "threadId": "thread-1", "mailboxIds": mailboxes,
		"keywords": keywords, "receivedAt": "2026-08-25T12:00:00Z", "messageId": []string{"m1@example.test"},
	}
}

func TestInitialAndIncrementalSync(t *testing.T) {
	s := newTestJMAPServer(t)
	phase := "initial"
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes()}
		case "Email/query":
			return map[string]interface{}{"accountId": "a1", "queryState": "q1", "position": 0, "ids": []string{"e1"}, "total": 1}
		case "Email/get":
			ids, _ := args["ids"].([]interface{})
			if len(ids) == 0 {
				return map[string]interface{}{"accountId": "a1", "state": "s1", "list": []interface{}{}, "notFound": []interface{}{}}
			}
			if phase == "initial" {
				return map[string]interface{}{"accountId": "a1", "state": "s1", "list": []interface{}{emailObject("e1", map[string]bool{"$seen": true, "$flagged": true}, map[string]bool{"inbox-id": true, "project-id": true})}, "notFound": []interface{}{}}
			}
			return map[string]interface{}{"accountId": "a1", "state": "s2", "list": []interface{}{emailObject("e1", map[string]bool{"$answered": true}, map[string]bool{"archive-id": true})}, "notFound": []interface{}{}}
		case "Email/changes":
			return map[string]interface{}{"accountId": "a1", "oldState": "s1", "newState": "s2", "hasMoreChanges": false, "created": []interface{}{}, "updated": []string{"e1"}, "destroyed": []string{"gone"}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	b := s.backend(t)
	folders, err := b.FetchFolders(t.Context())
	if err != nil || len(folders) != 1 || folders[0].Name != allMailStream {
		t.Fatalf("FetchFolders() = %#v, %v", folders, err)
	}
	first, err := b.FetchMessages(t.Context(), allMailStream, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Messages) != 1 || string(first.Messages[0].Raw) != testRaw {
		t.Fatalf("initial messages = %#v", first.Messages)
	}
	if !first.Messages[0].Flags.Seen || !first.Messages[0].Flags.Flagged {
		t.Errorf("initial flags = %#v", first.Messages[0].Flags)
	}
	if got := strings.Join(first.Messages[0].Labels, ","); got != "Project X,inbox" {
		t.Errorf("labels = %q", got)
	}

	phase = "changes"
	delta, err := b.FetchMessages(t.Context(), allMailStream, first.Cursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.Messages) != 1 || !delta.Messages[0].Flags.Answered || strings.Join(delta.Messages[0].Labels, ",") != "archive" {
		t.Errorf("delta message = %#v", delta.Messages)
	}
	if len(delta.Deleted) != 1 || delta.Deleted[0].Ref.ID != "gone" {
		t.Errorf("delta deletions = %#v", delta.Deleted)
	}
}

func TestFlagsLabelsAndAppend(t *testing.T) {
	s := newTestJMAPServer(t)
	var patches []map[string]interface{}
	var importedKeywords map[string]interface{}
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"state": "mb1", "list": testMailboxes()}
		case "Email/get":
			return map[string]interface{}{"state": "s1", "list": []interface{}{emailObject("e1", map[string]bool{"$seen": true}, map[string]bool{"inbox-id": true})}, "notFound": []interface{}{}}
		case "Email/set":
			update := args["update"].(map[string]interface{})
			patches = append(patches, update["e1"].(map[string]interface{}))
			return map[string]interface{}{"accountId": "a1", "oldState": "s1", "newState": "s2", "updated": map[string]interface{}{"e1": nil}, "notUpdated": map[string]interface{}{}}
		case "Email/import":
			emails := args["emails"].(map[string]interface{})
			importedKeywords = emails["0"].(map[string]interface{})["keywords"].(map[string]interface{})
			return map[string]interface{}{"accountId": "a1", "oldState": "s1", "newState": "s2", "created": map[string]interface{}{"0": map[string]string{"id": "imported"}}, "notCreated": map[string]interface{}{}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	b := s.backend(t)
	if err := b.ApplyFlags(t.Context(), backend.RemoteRef{ID: "e1"}, backend.Flags{Flagged: true}, backend.Flags{Seen: true}); err != nil {
		t.Fatal(err)
	}
	if err := b.ApplyLabels(t.Context(), backend.RemoteRef{ID: "e1"}, []string{"archive"}, []string{"inbox"}); err != nil {
		t.Fatal(err)
	}
	if len(patches) != 2 || patches[0]["keywords/$flagged"] != true || patches[0]["keywords/$seen"] != nil || patches[1]["mailboxIds/archive-id"] != true {
		t.Fatalf("patches = %#v", patches)
	}
	ref, err := b.Append(t.Context(), "drafts-id", backend.Flags{Seen: true}, []byte(testRaw))
	if err != nil || ref.ID != "imported" || string(s.uploaded) != testRaw {
		t.Fatalf("Append() = %#v, %v; upload=%q", ref, err, s.uploaded)
	}
	if importedKeywords["$draft"] != true || importedKeywords["$seen"] != true {
		t.Fatalf("imported keywords = %#v", importedKeywords)
	}
}

func TestApplyLabelsCreatesMissingArchiveMailbox(t *testing.T) {
	s := newTestJMAPServer(t)
	createdArchive := false
	var emailPatch map[string]interface{}
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			mailboxes := testMailboxes()
			if !createdArchive {
				mailboxes = append(mailboxes[:1], mailboxes[2:]...)
			}
			return map[string]interface{}{"state": "mb1", "list": mailboxes}
		case "Mailbox/set":
			createdArchive = true
			return map[string]interface{}{"created": map[string]interface{}{"archive": map[string]string{"id": "archive-id"}}, "notCreated": map[string]interface{}{}}
		case "Email/set":
			emailPatch = args["update"].(map[string]interface{})["e1"].(map[string]interface{})
			return map[string]interface{}{"updated": map[string]interface{}{"e1": nil}, "notUpdated": map[string]interface{}{}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	b := s.backend(t)
	if err := b.ApplyLabels(t.Context(), backend.RemoteRef{ID: "e1"}, []string{"archive"}, []string{"inbox"}); err != nil {
		t.Fatal(err)
	}
	if !createdArchive || emailPatch["mailboxIds/archive-id"] != true || emailPatch["mailboxIds/inbox-id"] != nil {
		t.Fatalf("created=%v patch=%#v", createdArchive, emailPatch)
	}
}

func TestWatchDispatchesEmailStateChange(t *testing.T) {
	s := newTestJMAPServer(t)
	s.events = "id: 7\ndata: {\"@type\":\"StateChange\",\"changed\":{\"a1\":{\"Email\":\"s2\"}}}\n\n"
	b := s.backend(t)
	ctx, cancel := context.WithCancel(t.Context())
	called := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() { done <- b.Watch(ctx, "", func() { called <- struct{}{}; cancel() }) }()
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("watch callback not called")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not stop after context cancellation")
	}
}

func TestExpandTemplateEscapesValues(t *testing.T) {
	got := expandTemplate("https://x/{accountId}/{blobId}/{name}?accept={type}", map[string]string{
		"accountId": "a/1", "blobId": "b 1", "name": "mail one.eml", "type": "message/rfc822",
	})
	if !strings.Contains(got, "a%2F1/b%201/mail%20one.eml") || !strings.Contains(got, "message%2Frfc822") {
		t.Fatalf("expandTemplate() = %q", got)
	}
}
