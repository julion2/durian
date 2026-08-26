package jmapbackend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/keychain"
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

type testMethodResponse struct {
	name  string
	value interface{}
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
		responseName := method
		if response, ok := result.(testMethodResponse); ok {
			responseName = response.name
			result = response.value
		}
		responses := []interface{}{[]interface{}{responseName, result, "0"}}
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

func TestBuildMailboxMappingsCanonicalAndDeterministic(t *testing.T) {
	mailboxes := map[string]jmapMailbox{
		"role":      {ID: "role", Name: "Whatever", Role: "InBoX"},
		"collision": {ID: "collision", Name: " INBOX "},
		"parent-a":  {ID: "parent-a", Name: "Projects"},
		"parent-b":  {ID: "parent-b", Name: "projects"},
		"leaf-a":    {ID: "leaf-a", Name: "Current", ParentID: "parent-a"},
		"leaf-b":    {ID: "leaf-b", Name: "CURRENT", ParentID: "parent-b"},
		"cycle-a":   {ID: "cycle-a", Name: "A", ParentID: "cycle-b"},
		"cycle-b":   {ID: "cycle-b", Name: "B", ParentID: "cycle-a"},
		"orphan":    {ID: "orphan", Name: "Orphan", ParentID: "missing"},
	}
	forward, reverse := buildMailboxMappings(mailboxes)
	if forward["role"] != "inbox" || reverse["inbox"] != "role" {
		t.Fatalf("special role did not win inbox collision: forward=%v reverse=%v", forward, reverse)
	}
	if forward["collision"] != "inbox~"+mailboxTagSuffix("collision") {
		t.Errorf("ordinary inbox collision = %q", forward["collision"])
	}
	if forward["leaf-a"] == forward["leaf-b"] || !strings.HasPrefix(forward["leaf-a"], "projects/current~") || !strings.HasPrefix(forward["leaf-b"], "projects/current~") {
		t.Errorf("duplicate canonical paths not disambiguated: %q %q", forward["leaf-a"], forward["leaf-b"])
	}
	if forward["orphan"] != "orphan" || forward["cycle-a"] == "" || forward["cycle-b"] == "" {
		t.Errorf("missing-parent/cycle mappings = %v", forward)
	}
	forward2, reverse2 := buildMailboxMappings(mailboxes)
	if !mapsEqual(forward, forward2) || !mapsEqual(reverse, reverse2) {
		t.Fatalf("mapping is nondeterministic: %v/%v then %v/%v", forward, reverse, forward2, reverse2)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func TestConsumeEventsSupportsMultilineAndLargeData(t *testing.T) {
	c := &client{accountID: "a1"}
	largePadding := strings.Repeat(" ", 70<<10)
	events := "id: 7\ndata: {\"@type\":\"StateChange\",\ndata: \"changed\":{\"a1\":{\"Email\":\"s2\"}}}" + largePadding + "\n\n"
	called := 0
	lastID, err := c.consumeEvents(t.Context(), strings.NewReader(events), "", func() { called++ })
	if err != nil || lastID != "7" || called != 1 {
		t.Fatalf("consumeEvents() = id %q, calls %d, err %v", lastID, called, err)
	}
}

func TestEnsureRetriesDiscoveryAndMigratesLegacyOnlyAfterSuccess(t *testing.T) {
	originalGet, originalSet := getCredential, setCredential
	t.Cleanup(func() { getCredential, setCredential = originalGet, originalSet })
	var jmapGets, legacyGets, migrations, sessions int
	getCredential = func(service, _ string) (string, error) {
		if service == keychain.JMAPKeychainService {
			jmapGets++
			return "", keychain.ErrNotFound
		}
		legacyGets++
		return "secret", nil
	}
	setCredential = func(service, _, password string) error {
		if service != keychain.JMAPKeychainService || password != "secret" {
			t.Fatalf("unexpected migration: %q %q", service, password)
		}
		migrations++
		return nil
	}
	s := newTestJMAPServer(t)
	baseHandler := s.server.Config.Handler
	s.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session" {
			sessions++
			if sessions <= 4 {
				http.Error(w, "temporary", http.StatusServiceUnavailable)
				return
			}
		}
		baseHandler.ServeHTTP(w, r)
	})
	b, err := New(&config.AccountConfig{Name: "Test", Email: "me@example.test", SyncEngine: "jmap", JMAP: &config.JMAPConfig{SessionURL: s.server.URL + "/session", Auth: "password"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ensure(t.Context()); err == nil || migrations != 0 {
		t.Fatalf("first ensure err=%v migrations=%d", err, migrations)
	}
	if err := b.ensure(t.Context()); err != nil || migrations != 1 || sessions != 5 || jmapGets != 2 || legacyGets != 2 {
		t.Fatalf("retry ensure err=%v migrations=%d sessions=%d gets=%d/%d", err, migrations, sessions, jmapGets, legacyGets)
	}
}

func TestEnsurePrefersJMAPCredential(t *testing.T) {
	originalGet, originalSet := getCredential, setCredential
	t.Cleanup(func() { getCredential, setCredential = originalGet, originalSet })
	legacyGets, migrations := 0, 0
	getCredential = func(service, _ string) (string, error) {
		if service == keychain.JMAPKeychainService {
			return "secret", nil
		}
		legacyGets++
		return "legacy", nil
	}
	setCredential = func(_, _, _ string) error { migrations++; return nil }
	s := newTestJMAPServer(t)
	b, err := New(&config.AccountConfig{Name: "Test", Email: "me@example.test", SyncEngine: "jmap", JMAP: &config.JMAPConfig{SessionURL: s.server.URL + "/session", Auth: "password"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ensure(t.Context()); err != nil || legacyGets != 0 || migrations != 0 {
		t.Fatalf("ensure err=%v legacyGets=%d migrations=%d", err, legacyGets, migrations)
	}
}

func TestQueryAllEmailIDsRejectsIncompleteResult(t *testing.T) {
	s := newTestJMAPServer(t)
	b := s.backend(t)
	if err := b.ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.handler = func(method string, args map[string]interface{}) interface{} {
		if method != "Email/query" {
			t.Fatalf("unexpected method %s", method)
		}
		if _, paged := args["anchor"]; paged {
			return map[string]interface{}{"position": 1, "ids": []string{}, "total": 2}
		}
		return map[string]interface{}{"position": 0, "ids": []string{"e1"}, "total": 2}
	}
	if _, err := b.queryAllEmailIDsOnce(t.Context()); !errors.Is(err, errIncompleteQuery) {
		t.Fatalf("query error = %v, want errIncompleteQuery", err)
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
	if got := strings.Join(first.Messages[0].Labels, ","); got != "inbox,project-x" {
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

func TestInitialSyncCapturesStableIDSetBeforePagingBodies(t *testing.T) {
	s := newTestJMAPServer(t)
	queryCalls := 0
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"state": "mb1", "list": testMailboxes()}
		case "Email/query":
			queryCalls++
			if queryCalls == 1 {
				if args["position"] != float64(0) {
					t.Errorf("first query position = %#v", args["position"])
				}
				return map[string]interface{}{"queryState": "q1", "position": 0, "ids": []string{"e1", "e2"}, "total": 3, "limit": 2}
			}
			if args["anchor"] != "e2" || args["anchorOffset"] != float64(1) {
				t.Errorf("second query anchor args = %#v", args)
			}
			if _, ok := args["position"]; ok {
				t.Error("anchored query must not also set position")
			}
			return map[string]interface{}{"queryState": "q1", "position": 2, "ids": []string{"e3"}, "total": 3, "limit": 2}
		case "Email/get":
			ids, _ := args["ids"].([]interface{})
			if len(ids) == 0 {
				return map[string]interface{}{"state": "s1", "list": []interface{}{}, "notFound": []interface{}{}}
			}
			list := make([]interface{}, 0, len(ids))
			for _, rawID := range ids {
				id := rawID.(string)
				list = append(list, emailObject(id, nil, map[string]bool{"inbox-id": true}))
			}
			return map[string]interface{}{"state": "s1", "list": list, "notFound": []interface{}{}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	b := s.backend(t)
	first, err := b.FetchMessages(t.Context(), allMailStream, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Messages) != 2 || !first.HasMore {
		t.Fatalf("first page = %#v", first)
	}
	second, err := b.FetchMessages(t.Context(), allMailStream, first.Cursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Messages) != 1 || second.HasMore || queryCalls != 2 {
		t.Fatalf("second page = %#v, query calls = %d", second, queryCalls)
	}
}

func TestCannotCalculateChangesStartsReplacementSnapshot(t *testing.T) {
	s := newTestJMAPServer(t)
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"state": "mb1", "list": testMailboxes()}
		case "Email/changes":
			return testMethodResponse{name: "error", value: methodError{Type: "cannotCalculateChanges"}}
		case "Email/query":
			return map[string]interface{}{"ids": []string{"e1"}, "total": 1}
		case "Email/get":
			ids, _ := args["ids"].([]interface{})
			if len(ids) == 0 {
				return map[string]interface{}{"state": "new-state", "list": []interface{}{}, "notFound": []interface{}{}}
			}
			return map[string]interface{}{"state": "new-state", "list": []interface{}{emailObject("e1", nil, map[string]bool{"inbox-id": true})}, "notFound": []interface{}{}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	b := s.backend(t)
	result, err := b.FetchMessages(t.Context(), allMailStream, encodeCursor(jmapCursor{EmailState: "expired"}), 10)
	if err != nil {
		t.Fatal(err)
	}
	if !result.FullSnapshot || len(result.Messages) != 0 || len(result.Present) != 1 {
		t.Fatalf("replacement result = %#v", result)
	}
}

func TestFetchSnapshotMetadataDoesNotDownloadBodies(t *testing.T) {
	s := newTestJMAPServer(t)
	s.handler = func(method string, _ map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"state": "mb1", "list": testMailboxes()}
		case "Email/get":
			return map[string]interface{}{
				"state":    "s1",
				"list":     []interface{}{emailObject("e1", map[string]bool{"$seen": true}, map[string]bool{"inbox-id": true})},
				"notFound": []interface{}{},
			}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	b := s.backend(t)
	if _, err := b.FetchFolders(t.Context()); err != nil {
		t.Fatal(err)
	}
	batch, err := b.FetchSnapshotMetadata(t.Context(), []backend.RemoteRef{{Folder: allMailStream, ID: "e1"}})
	if err != nil {
		t.Fatal(err)
	}
	messages := batch.Messages
	if len(messages) != 1 || messages[0].Ref.ID != "e1" || !messages[0].Flags.Seen {
		t.Fatalf("metadata = %+v", messages)
	}
	if got, want := messages[0].Labels, []string{"inbox"}; !slices.Equal(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
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
		case "Email/get":
			return map[string]interface{}{"state": "s1", "list": []interface{}{emailObject("e1", nil, map[string]bool{"inbox-id": true})}, "notFound": []interface{}{}}
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

func TestWatchReturnsPermanentHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "revoked", http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	c := &client{
		httpClient: &http.Client{Timeout: time.Second},
		session:    session{EventSourceURL: server.URL},
		accountID:  "a1",
	}
	err := c.watch(t.Context(), func() {})
	var statusErr *statusError
	if !errors.As(err, &statusErr) || statusErr.Status != http.StatusUnauthorized {
		t.Fatalf("watch error = %v, want HTTP 401", err)
	}
}

func TestMutatingMethodIsNotRetriedAfterServerError(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.Error(w, "ambiguous failure", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	c := &client{httpClient: server.Client(), session: session{APIURL: server.URL}}
	err := c.call(t.Context(), []string{coreCapability, mailCapability, submissionCapability}, "EmailSubmission/set", map[string]interface{}{}, nil)
	if err == nil || calls != 1 {
		t.Fatalf("call error = %v, requests = %d; mutating call must not retry", err, calls)
	}
}

func TestValidateJMAPURLAllowsOnlySecureOrLoopback(t *testing.T) {
	for _, raw := range []string{"https://api.example.test/jmap", "http://localhost:8080/jmap", "http://127.0.0.1/jmap", "http://[::1]/jmap"} {
		if err := validateJMAPURL(raw); err != nil {
			t.Errorf("validateJMAPURL(%q) = %v", raw, err)
		}
	}
	if err := validateJMAPURL("http://api.example.test/jmap"); err == nil {
		t.Fatal("insecure non-loopback JMAP URL was accepted")
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
