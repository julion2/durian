package jmapbackend

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/keychain"
	"github.com/julion2/durian/cli/internal/redact"
)

const testRaw = "From: Alice <alice@example.test>\r\nTo: Me <me@example.test>\r\nSubject: hello\r\nMessage-ID: <m1@example.test>\r\nDate: Tue, 25 Aug 2026 12:00:00 +0000\r\n\r\nbody\r\n"

func TestProviderErrorsSafeLogTextOmitsServerDetails(t *testing.T) {
	const secret = "short multiword response echoing token abc123"
	for name, err := range map[string]interface{ SafeLogText() string }{
		"status": &statusError{Status: http.StatusUnauthorized, Body: secret},
		"method": &methodError{Type: "custom-" + secret, Description: secret},
	} {
		if got := err.SafeLogText(); strings.Contains(got, secret) {
			t.Errorf("%s SafeLogText() leaked server details: %q", name, got)
		}
	}
}

func TestAmbiguousImportProviderBodyRemainsRedactable(t *testing.T) {
	const providerBody = "denied private token"
	s := newTestJMAPServer(t)
	s.apiBody = providerBody
	s.handler = func(method string, _ map[string]interface{}) interface{} {
		if method != "Mailbox/get" {
			t.Fatalf("unexpected method %s", method)
		}
		// This Mailbox/get completes normally; the following Email/import
		// receives the provider-controlled HTTP error.
		s.apiStatus = http.StatusTeapot
		return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": []string{}}
	}
	b := s.backend(t)

	_, err := b.append(t.Context(), "drafts-id", backend.Flags{}, []byte(testRaw), false)
	if err == nil || !errors.Is(err, errEmailCreationOutcomeUnknown) {
		t.Fatalf("append error = %v, want ambiguous creation sentinel", err)
	}
	var statusErr *statusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("append error lost provider status cause: %v", err)
	}

	var output bytes.Buffer
	logger := slog.New(redact.Wrap(slog.NewTextHandler(&output, nil)))
	logger.Error("append failed", "err", err)
	if got := output.String(); strings.Contains(got, providerBody) || !strings.Contains(got, redact.Placeholder) {
		t.Fatalf("redacted log = %q", got)
	}
}

type testJMAPServer struct {
	t      *testing.T
	server *httptest.Server

	handler          func(string, map[string]interface{}) interface{}
	uploaded         []byte
	events           string
	before           []interface{}
	extra            []interface{}
	limits           map[string]interface{}
	uploadResponse   map[string]interface{}
	sessionUsername  *string
	sessionAccounts  map[string]interface{}
	primaryAccounts  map[string]string
	sessionMutate    func(map[string]interface{})
	apiSessionState  *string
	omitUsername     bool
	omitSessionState bool
	rawResponses     bool
	downloadStatus   int
	downloadBytes    int64
	dropAPIResponse  bool
	apiStatus        int
	apiBody          string
	password         string
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
	password := "secret"
	if s.password != "" {
		password = s.password
	}
	if user, supplied, ok := r.BasicAuth(); !ok || user != "me@example.test" || supplied != password {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == "/session":
		limits := map[string]interface{}{
			"maxSizeUpload":         50_000_000,
			"maxConcurrentUpload":   4,
			"maxSizeRequest":        10_000_000,
			"maxConcurrentRequests": 4,
			"maxCallsInRequest":     16,
			"maxObjectsInGet":       500,
			"maxObjectsInSet":       500,
			"collationAlgorithms":   []string{},
		}
		for name, value := range s.limits {
			limits[name] = value
		}
		username := "me@example.test"
		if s.sessionUsername != nil {
			username = *s.sessionUsername
		}
		accounts := map[string]interface{}{"a1": map[string]interface{}{"name": "Test", "isPersonal": true, "isReadOnly": false, "accountCapabilities": map[string]interface{}{mailCapability: map[string]interface{}{}, submissionCapability: map[string]interface{}{}}}}
		if s.sessionAccounts != nil {
			accounts = s.sessionAccounts
		}
		primaryAccounts := map[string]string{mailCapability: "a1", submissionCapability: "a1"}
		if s.primaryAccounts != nil {
			primaryAccounts = s.primaryAccounts
		}
		response := map[string]interface{}{
			"capabilities":    map[string]interface{}{coreCapability: limits, mailCapability: map[string]interface{}{}, submissionCapability: map[string]interface{}{}},
			"accounts":        accounts,
			"primaryAccounts": primaryAccounts,
			"username":        username,
			"apiUrl":          s.server.URL + "/api", "downloadUrl": s.server.URL + "/download/{accountId}/{blobId}/{name}?accept={type}",
			"uploadUrl": s.server.URL + "/upload/{accountId}", "eventSourceUrl": s.server.URL + "/events?types={types}&closeafter={closeafter}&ping={ping}",
			"state": "session-1",
		}
		if s.omitUsername {
			delete(response, "username")
		}
		if s.sessionMutate != nil {
			s.sessionMutate(response)
		}
		_ = json.NewEncoder(w).Encode(response)
	case r.URL.Path == "/api":
		if s.apiStatus != 0 {
			http.Error(w, s.apiBody, s.apiStatus)
			return
		}
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
		if s.dropAPIResponse {
			s.dropAPIResponse = false
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				s.t.Errorf("hijack dropped API response: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		responseName := method
		if response, ok := result.(testMethodResponse); ok {
			responseName = response.name
			result = response.value
		}
		responses := append([]interface{}{}, s.before...)
		responses = append(responses, []interface{}{responseName, result, "0"})
		responses = append(responses, s.extra...)
		if !s.rawResponses {
			for _, response := range responses {
				tuple, ok := response.([]interface{})
				if !ok || len(tuple) < 2 {
					continue
				}
				name, _ := tuple[0].(string)
				completeStandardTestResponse(name, tuple[1])
			}
		}
		s.before = nil
		s.extra = nil
		responseEnvelope := map[string]interface{}{"methodResponses": responses, "sessionState": "session-1"}
		if s.apiSessionState != nil {
			responseEnvelope["sessionState"] = *s.apiSessionState
		}
		if s.omitSessionState {
			delete(responseEnvelope, "sessionState")
		}
		_ = json.NewEncoder(w).Encode(responseEnvelope)
	case strings.HasPrefix(r.URL.Path, "/download/"):
		if s.downloadStatus != 0 {
			http.Error(w, "download failed", s.downloadStatus)
			return
		}
		if s.downloadBytes > 0 {
			chunk := make([]byte, 32<<10)
			remaining := s.downloadBytes
			for remaining > 0 {
				n := min(int64(len(chunk)), remaining)
				if _, err := w.Write(chunk[:n]); err != nil {
					return
				}
				remaining -= n
			}
			return
		}
		_, _ = io.WriteString(w, testRaw)
	case strings.HasPrefix(r.URL.Path, "/upload/"):
		s.uploaded, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		response := s.uploadResponse
		if response == nil {
			response = map[string]interface{}{
				"accountId": "a1", "blobId": "uploaded-blob", "type": r.Header.Get("Content-Type"), "size": len(s.uploaded),
			}
		}
		_ = json.NewEncoder(w).Encode(response)
	case r.URL.Path == "/events":
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, s.events)
	default:
		http.NotFound(w, r)
	}
}

func completeStandardTestResponse(method string, result interface{}) {
	response, ok := result.(map[string]interface{})
	if !ok {
		return
	}
	switch method {
	case "Mailbox/set", "Email/set", "Email/import", "EmailSubmission/set":
		if _, ok := response["oldState"]; !ok {
			response["oldState"] = "state-before"
		}
		if _, ok := response["newState"]; !ok {
			response["newState"] = "state-after"
		}
	}
	switch method {
	case "Email/set", "Email/import":
		created, _ := response["created"].(map[string]interface{})
		for key, value := range created {
			object := make(map[string]interface{})
			switch value := value.(type) {
			case map[string]interface{}:
				for name, field := range value {
					object[name] = field
				}
			case map[string]string:
				for name, field := range value {
					object[name] = field
				}
			default:
				continue
			}
			id, _ := object["id"].(string)
			if _, ok := object["blobId"]; !ok {
				object["blobId"] = "blob-" + id
			}
			if _, ok := object["threadId"]; !ok {
				object["threadId"] = "thread-" + id
			}
			if _, ok := object["size"]; !ok {
				object["size"] = 1
			}
			created[key] = object
		}
	case "Mailbox/get":
		if response["notFound"] == nil {
			response["notFound"] = []interface{}{}
		}
		if mailboxes, ok := response["list"].([]map[string]interface{}); ok {
			for _, mailbox := range mailboxes {
				if _, ok := mailbox["parentId"]; !ok {
					mailbox["parentId"] = nil
				}
				if _, ok := mailbox["role"]; !ok {
					mailbox["role"] = nil
				}
			}
		}
	case "Identity/get":
		if _, ok := response["notFound"]; !ok {
			response["notFound"] = []interface{}{}
		}
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

func currentRef(t *testing.T, b *Backend, id string) backend.RemoteRef {
	t.Helper()
	if err := b.ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	return backend.RemoteRef{ID: b.scopedEmailID(id)}
}

func currentCursor(t *testing.T, b *Backend, state jmapCursor) backend.Cursor {
	t.Helper()
	if err := b.ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	state.AccountScope = b.client.accountScope
	return encodeCursor(state)
}

func testMailboxes() []map[string]interface{} {
	return []map[string]interface{}{
		{"id": "inbox-id", "name": "Inbox", "parentId": nil, "role": "inbox", "isSubscribed": true},
		{"id": "archive-id", "name": "Archive", "parentId": nil, "role": "archive", "isSubscribed": true},
		{"id": "drafts-id", "name": "Drafts", "parentId": nil, "role": "drafts", "isSubscribed": true},
		{"id": "sent-id", "name": "Sent", "parentId": nil, "role": "sent", "isSubscribed": true},
		{"id": "project-id", "name": "Project X", "parentId": nil, "role": nil, "isSubscribed": true},
	}
}

func TestBuildMailboxMappingsCanonicalAndDeterministic(t *testing.T) {
	flaggedSuffix := "flagged~" + mailboxTagSuffix("flagged")
	mailboxes := map[string]jmapMailbox{
		"role":            {ID: "role", Name: "Whatever", Role: "InBoX"},
		"collision":       {ID: "collision", Name: " INBOX "},
		"parent-a":        {ID: "parent-a", Name: "Projects"},
		"parent-b":        {ID: "parent-b", Name: "projects"},
		"leaf-a":          {ID: "leaf-a", Name: "Current", ParentID: "parent-a"},
		"leaf-b":          {ID: "leaf-b", Name: "CURRENT", ParentID: "parent-b"},
		"cycle-a":         {ID: "cycle-a", Name: "A", ParentID: "cycle-b"},
		"cycle-b":         {ID: "cycle-b", Name: "B", ParentID: "cycle-a"},
		"orphan":          {ID: "orphan", Name: "Orphan", ParentID: "missing"},
		"flagged":         {ID: "flagged", Name: "Flagged"},
		"flagged-suffix":  {ID: "flagged-suffix", Name: flaggedSuffix},
		"unread":          {ID: "unread", Name: "Unread"},
		"replied":         {ID: "replied", Name: "Replied"},
		"keyword-mailbox": {ID: "keyword-mailbox", Name: "jmap-keyword/foo"},
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
	for _, id := range []string{"flagged", "unread", "replied"} {
		if want := id + "~" + mailboxTagSuffix(id); forward[id] != want || reverse[want] != id {
			t.Errorf("reserved flag-tag mailbox %q mapped as %q, reverse=%q; want %q", id, forward[id], reverse[want], want)
		}
	}
	if forward["flagged-suffix"] == flaggedSuffix || reverse[forward["flagged-suffix"]] != "flagged-suffix" {
		t.Errorf("post-suffix collision was not disambiguated: forward=%v reverse=%v", forward, reverse)
	}
	if got, want := forward["keyword-mailbox"], "mailbox~"+mailboxTagSuffix("keyword-mailbox")+"/jmap-keyword/foo"; got != want || reverse[want] != "keyword-mailbox" {
		t.Errorf("native-keyword namespace mailbox = %q, reverse=%q; want %q", got, reverse[want], want)
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
	lastID, _, _, err := c.consumeEvents(t.Context(), strings.NewReader(events), "", func() { called++ })
	if err != nil || lastID != "7" || called != 1 {
		t.Fatalf("consumeEvents() = id %q, calls %d, err %v", lastID, called, err)
	}
}

func TestConsumeEventsRejectsOversizedMultilineEventWithoutCheckpoint(t *testing.T) {
	c := &client{accountID: "a1"}
	var event strings.Builder
	event.WriteString("id: 8\n")
	line := strings.Repeat("x", 1024)
	for i := 0; i < maxEventBytes/len(line)+2; i++ {
		event.WriteString("data: ")
		event.WriteString(line)
		event.WriteByte('\n')
	}
	called := false
	lastID, _, _, err := c.consumeEvents(t.Context(), strings.NewReader(event.String()), "7", func() { called = true })
	if err == nil || !strings.Contains(err.Error(), "event exceeds") || lastID != "7" || called {
		t.Fatalf("consumeEvents() = id %q, called %t, err %v", lastID, called, err)
	}
}

func TestConsumeEventsFollowsEventStreamFieldParsing(t *testing.T) {
	c := &client{accountID: "a1"}
	event := "\ufeff: comment\rid:  value  \rretry: 15\rdata:{\"@type\":\"StateChange\",\"changed\":{\"a1\":{\"Email\":\"s2\"}}}\r\r"
	called := 0
	lastID, retry, retrySet, err := c.consumeEvents(t.Context(), strings.NewReader(event), "old", func() { called++ })
	if err != nil || lastID != " value  " || called != 1 || !retrySet || retry != minEventRetry {
		t.Fatalf("consumeEvents() = id %q, retry %v/%v, calls %d, err %v", lastID, retry, retrySet, called, err)
	}

	lastID, _, _, err = c.consumeEvents(t.Context(), strings.NewReader("id\n\n"), lastID, func() {})
	if err != nil || lastID != "" {
		t.Fatalf("colonless id did not clear event id: id=%q err=%v", lastID, err)
	}
}

func TestConsumeEventsClampsProviderRetry(t *testing.T) {
	c := &client{accountID: "a1"}
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{"0", minEventRetry},
		{"999999999", maxEventRetry},
	} {
		_, retry, retrySet, err := c.consumeEvents(t.Context(), strings.NewReader("retry: "+test.value+"\n\n"), "", func() {})
		if err != nil || !retrySet || retry != test.want {
			t.Errorf("retry %s = %v/%v, err=%v, want %v", test.value, retry, retrySet, err, test.want)
		}
	}
}

func TestRetryAfterIsSaturatingAndBounded(t *testing.T) {
	for _, test := range []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{"", 0, false},
		{"invalid", 0, false},
		{"0", minEventRetry, true},
		{"12", 12 * time.Second, true},
		{"300", maxEventRetry, true},
		{"999999999999999999999999999999999999", maxEventRetry, true},
	} {
		got, ok := parseRetryAfter(test.value)
		if got != test.want || ok != test.ok {
			t.Errorf("parseRetryAfter(%q) = %v, %v; want %v, %v", test.value, got, ok, test.want, test.ok)
		}
	}
	future := time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat)
	got, ok := parseRetryAfter(future)
	if !ok || got < 118*time.Second || got > 2*time.Minute {
		t.Fatalf("date-form Retry-After = %v, %v; want about 2m", got, ok)
	}
}

func TestEventHTTPClientBoundsStalledResponseHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	client, err := newEventHTTPClient(server.Client(), 25*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := client.Get(server.URL); err == nil {
		t.Fatal("stalled response headers did not time out")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("response-header timeout took %v", elapsed)
	}
}

func TestConsumeEventsBoundsIdleStream(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	c := &client{accountID: "a1"}
	started := time.Now()
	lastID, _, _, err := c.consumeEventsWithIdleTimeout(t.Context(), func() {}, reader, "checkpoint", func() {}, 25*time.Millisecond)
	if !errors.Is(err, errEventStreamIdle) || lastID != "checkpoint" {
		t.Fatalf("idle stream = id %q, err %v", lastID, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("idle stream timeout took %v", elapsed)
	}
}

type closeIgnoringReader struct {
	release <-chan struct{}
}

func (r closeIgnoringReader) Read([]byte) (int, error) {
	<-r.release
	return 0, io.EOF
}

func (closeIgnoringReader) Close() error { return nil }

func TestConsumeEventsCancelsRequestWhenBodyCloseDoesNotUnblock(t *testing.T) {
	release := make(chan struct{})
	c := &client{accountID: "a1"}
	started := time.Now()
	lastID, _, _, err := c.consumeEventsWithIdleTimeout(
		t.Context(), func() { close(release) }, closeIgnoringReader{release: release},
		"checkpoint", func() {}, 25*time.Millisecond,
	)
	if !errors.Is(err, errEventStreamIdle) || lastID != "checkpoint" {
		t.Fatalf("idle stream = id %q, err %v", lastID, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("request cancellation took %v", elapsed)
	}
}

func TestValidateEventStreamContentType(t *testing.T) {
	for _, value := range []string{"text/event-stream", "text/event-stream; charset=utf-8", "TEXT/EVENT-STREAM"} {
		if err := validateEventStreamContentType(value); err != nil {
			t.Errorf("valid Content-Type %q rejected: %v", value, err)
		}
	}
	for _, value := range []string{"", "application/json", "text/event-stream invalid"} {
		if err := validateEventStreamContentType(value); err == nil {
			t.Errorf("invalid Content-Type %q accepted", value)
		}
	}
}

func TestDiscoveryRejectsMissingSessionAndAccountFields(t *testing.T) {
	for _, field := range []string{
		"capabilities", "accounts", "primaryAccounts", "username", "apiUrl",
		"downloadUrl", "uploadUrl", "eventSourceUrl", "state",
	} {
		t.Run("session "+field, func(t *testing.T) {
			s := newTestJMAPServer(t)
			s.sessionMutate = func(response map[string]interface{}) { delete(response, field) }
			if err := s.backend(t).ensure(t.Context()); err == nil {
				t.Fatalf("Session without %s was accepted", field)
			}
		})
	}
	for _, field := range []string{"name", "isPersonal", "isReadOnly", "accountCapabilities"} {
		t.Run("account "+field, func(t *testing.T) {
			s := newTestJMAPServer(t)
			s.sessionMutate = func(response map[string]interface{}) {
				accounts := response["accounts"].(map[string]interface{})
				delete(accounts["a1"].(map[string]interface{}), field)
			}
			if err := s.backend(t).ensure(t.Context()); err == nil {
				t.Fatalf("Account without %s was accepted", field)
			}
		})
	}
}

func TestDiscoveryRequiresUsableSessionTemplates(t *testing.T) {
	for _, test := range []struct {
		name  string
		field string
		value string
	}{
		{"download accountId", "downloadUrl", "https://example.test/{blobId}/{type}/{name}"},
		{"download blobId", "downloadUrl", "https://example.test/{accountId}/{type}/{name}"},
		{"download type", "downloadUrl", "https://example.test/{accountId}/{blobId}/{name}"},
		{"download name", "downloadUrl", "https://example.test/{accountId}/{blobId}/{type}"},
		{"upload accountId", "uploadUrl", "https://example.test/upload"},
		{"events types", "eventSourceUrl", "https://example.test/{closeafter}/{ping}"},
		{"events closeafter", "eventSourceUrl", "https://example.test/{types}/{ping}"},
		{"events ping", "eventSourceUrl", "https://example.test/{types}/{closeafter}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestJMAPServer(t)
			s.sessionMutate = func(response map[string]interface{}) { response[test.field] = test.value }
			if err := s.backend(t).ensure(t.Context()); err == nil || !strings.Contains(err.Error(), "URI template") {
				t.Fatalf("invalid %s error = %v", test.field, err)
			}
		})
	}
}

func TestAPIResponseRequiresMatchingSessionState(t *testing.T) {
	for _, test := range []struct {
		name      string
		omit      bool
		state     string
		wantStale bool
	}{
		{name: "omitted", omit: true},
		{name: "changed", state: "session-2", wantStale: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestJMAPServer(t)
			s.omitSessionState = test.omit
			if !test.omit {
				s.apiSessionState = &test.state
			}
			s.handler = func(method string, _ map[string]interface{}) interface{} {
				if method != "Email/get" {
					t.Fatalf("unexpected method %s", method)
				}
				return map[string]interface{}{"accountId": "a1", "state": "", "list": []interface{}{}, "notFound": []interface{}{}}
			}
			b := s.backend(t)
			if err := b.ensure(t.Context()); err != nil {
				t.Fatal(err)
			}
			_, err := b.currentEmailState(t.Context())
			var protocolErr *protocolError
			if !errors.As(err, &protocolErr) || !protocolErr.primaryAmbiguous {
				t.Fatalf("sessionState error = %v, want ambiguity-preserving protocol error", err)
			}
			if b.client.sessionStale.Load() != test.wantStale {
				t.Fatalf("session stale = %v, want %v", b.client.sessionStale.Load(), test.wantStale)
			}
			if test.wantStale {
				s.apiSessionState = nil
				if err := b.ensure(t.Context()); err != nil || b.client.sessionStale.Load() {
					t.Fatalf("Session rediscovery = stale %v, err %v", b.client.sessionStale.Load(), err)
				}
			}
		})
	}
}

func TestEmptySessionAndMethodStatesAreValid(t *testing.T) {
	empty := ""
	s := newTestJMAPServer(t)
	s.sessionMutate = func(response map[string]interface{}) { response["state"] = "" }
	s.apiSessionState = &empty
	s.handler = func(method string, _ map[string]interface{}) interface{} {
		if method != "Email/get" {
			t.Fatalf("unexpected method %s", method)
		}
		return map[string]interface{}{"accountId": "a1", "state": "", "list": []interface{}{}, "notFound": []interface{}{}}
	}
	b := s.backend(t)
	if err := b.ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if state, err := b.currentEmailState(t.Context()); err != nil || state != "" {
		t.Fatalf("empty method state = %q, %v", state, err)
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

func TestEnsureDoesNotAdoptLegacyPasswordAsBearerToken(t *testing.T) {
	originalGet := getCredential
	t.Cleanup(func() { getCredential = originalGet })
	legacyGets := 0
	getCredential = func(service, _ string) (string, error) {
		if service == keychain.JMAPKeychainService {
			return "", keychain.ErrNotFound
		}
		legacyGets++
		return "imap-app-password", nil
	}
	s := newTestJMAPServer(t)
	b, err := New(&config.AccountConfig{
		Name: "Test", Email: "me@example.test", SyncEngine: "jmap",
		JMAP: &config.JMAPConfig{SessionURL: s.server.URL + "/session", Auth: "bearer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = b.ensure(t.Context())
	if err == nil || !strings.Contains(err.Error(), "run durian auth login") || legacyGets != 0 {
		t.Fatalf("ensure error=%v legacyGets=%d, want missing JMAP token without legacy lookup", err, legacyGets)
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
			return map[string]interface{}{"accountId": "a1", "queryState": "q1", "canCalculateChanges": true, "position": 1, "ids": []string{}, "total": 2}
		}
		return map[string]interface{}{"accountId": "a1", "queryState": "q1", "canCalculateChanges": true, "position": 0, "ids": []string{"e1"}, "total": 2}
	}
	if _, err := b.queryAllEmailIDsOnce(t.Context()); !errors.Is(err, errIncompleteQuery) {
		t.Fatalf("query error = %v, want errIncompleteQuery", err)
	}
}

func emailObject(id string, keywords map[string]bool, mailboxes map[string]bool) map[string]interface{} {
	if keywords == nil {
		keywords = map[string]bool{}
	}
	if mailboxes == nil {
		mailboxes = map[string]bool{"inbox-id": true}
	}
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
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		case "Email/query":
			return map[string]interface{}{"accountId": "a1", "queryState": "q1", "canCalculateChanges": true, "position": 0, "ids": []string{"e1"}, "total": 1}
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
	if len(first.Messages) != 0 || len(first.Present) != 1 {
		t.Fatalf("initial metadata page = %#v", first)
	}
	hydrated, err := b.FetchSnapshotMessages(t.Context(), first.Present)
	if err != nil || len(hydrated.Messages) != 1 || string(hydrated.Messages[0].Raw) != testRaw {
		t.Fatalf("initial hydration = %#v, %v", hydrated, err)
	}
	initial := hydrated.Messages[0]
	if first.Present[0].ID != initial.Ref.ID {
		t.Fatalf("initial presence = %#v, message ref = %#v", first.Present, initial.Ref)
	}
	if !initial.Flags.Seen || !initial.Flags.Flagged {
		t.Errorf("initial flags = %#v", initial.Flags)
	}
	if got := strings.Join(initial.Labels, ","); got != "inbox,project-x" {
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
	if len(delta.Deleted) != 1 || delta.Deleted[0].Ref.ID != b.scopedEmailID("gone") {
		t.Errorf("delta deletions = %#v", delta.Deleted)
	}
}

func TestProviderAccountChangeForcesScopedReplacement(t *testing.T) {
	s := newTestJMAPServer(t)
	changesCalled := false
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		case "Email/get":
			ids, _ := args["ids"].([]interface{})
			if len(ids) == 0 {
				return map[string]interface{}{"accountId": "a1", "state": "same-state", "list": []interface{}{}, "notFound": []interface{}{}}
			}
			if len(ids) != 1 || ids[0] != "same-id" {
				t.Fatalf("Email/get ids = %#v", ids)
			}
			return map[string]interface{}{"accountId": "a1", "state": "same-state", "list": []interface{}{emailObject("same-id", nil, map[string]bool{"inbox-id": true})}, "notFound": []interface{}{}}
		case "Email/query":
			return map[string]interface{}{"accountId": "a1", "queryState": "same-query", "canCalculateChanges": true, "position": 0, "ids": []string{"same-id"}, "total": 1}
		case "Email/changes":
			changesCalled = true
			return map[string]interface{}{}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	b := s.backend(t)

	result, err := b.FetchMessages(t.Context(), allMailStream, encodeCursor(jmapCursor{
		AccountScope: "different-provider-account", EmailState: "same-state",
	}), 10)
	if err != nil {
		t.Fatal(err)
	}
	if changesCalled {
		t.Fatal("provider account change reused the old account's Email state")
	}
	wantRef := b.scopedEmailID("same-id")
	if !result.FullSnapshot || result.HasMore || len(result.Present) != 1 || result.Present[0].ID != wantRef {
		t.Fatalf("replacement result = %#v, want scoped ref %q", result, wantRef)
	}
	if state := decodeCursor(result.Cursor); state.AccountScope != b.client.accountScope || state.EmailState != "same-state" {
		t.Fatalf("replacement cursor = %+v", state)
	}
	legacy, err := b.FetchMessages(t.Context(), allMailStream, encodeCursor(jmapCursor{EmailState: "same-state"}), 10)
	if err != nil {
		t.Fatal(err)
	}
	if !legacy.FullSnapshot || len(legacy.Present) != 1 || legacy.Present[0].ID != wantRef {
		t.Fatalf("legacy unscoped cursor replacement = %#v", legacy)
	}
	batch, err := b.FetchSnapshotMessages(t.Context(), result.Present)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Messages) != 1 || batch.Messages[0].StableID != wantRef || batch.Messages[0].Ref.ID != wantRef {
		t.Fatalf("hydrated replacement = %#v", batch)
	}
	if err := b.ApplyFlags(t.Context(), backend.RemoteRef{ID: "different-provider-account:same-id"}, backend.Flags{Seen: true}, backend.Flags{}); !errors.Is(err, backend.ErrRefGone) {
		t.Fatalf("old provider ref error = %v, want ErrRefGone", err)
	}
}

func TestProviderAccountScopeIncludesSessionPathAndAuthenticatedUsername(t *testing.T) {
	base := providerAccountScope("https://mail.example.test/jmap/session", "alice@example.test", "a1")
	for name, scope := range map[string]string{
		"session path": providerAccountScope("https://mail.example.test/other/session", "alice@example.test", "a1"),
		"username":     providerAccountScope("https://mail.example.test/jmap/session", "bob@example.test", "a1"),
		"account":      providerAccountScope("https://mail.example.test/jmap/session", "alice@example.test", "a2"),
	} {
		if scope == base {
			t.Errorf("%s change did not change provider account scope", name)
		}
	}
}

func TestLegacyIdentityMigrationScopesRecognizedCursorOnly(t *testing.T) {
	s := newTestJMAPServer(t)
	b := s.backend(t)
	if err := b.ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	legacy := encodeCursor(jmapCursor{EmailState: "legacy-state"})
	if _, _, ok := b.LegacyIdentityMigration(legacy); ok {
		t.Fatal("legacy identity was trusted without explicit opt-in")
	}
	b.account.JMAP.TrustLegacyIdentity = true
	scoped, prefix, ok := b.LegacyIdentityMigration(legacy)
	if !ok || prefix != b.client.accountScope+":" {
		t.Fatalf("legacy migration = ok %v, prefix %q", ok, prefix)
	}
	state := decodeCursor(scoped)
	if state.AccountScope != b.client.accountScope || state.EmailStateSet || state.EmailState != "" {
		t.Fatalf("scoped legacy cursor = %+v", state)
	}
	current := encodeCursor(jmapCursor{AccountScope: "different-account", EmailState: "state"})
	if _, _, ok := b.LegacyIdentityMigration(current); ok {
		t.Fatal("non-empty account scope was treated as a legacy upgrade")
	}
	if _, _, ok := b.LegacyIdentityMigration(backend.Cursor("not-json")); ok {
		t.Fatal("invalid cursor was treated as a legacy upgrade")
	}
	for _, cursor := range []backend.Cursor{backend.Cursor(`{}`), backend.Cursor(`{"unknown":"state"}`)} {
		if _, _, ok := b.LegacyIdentityMigration(cursor); ok {
			t.Fatalf("unrecognized cursor %s was treated as a legacy upgrade", cursor)
		}
	}
}

func TestSessionUsernameMayBeEmptyButMustBePresent(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		s := newTestJMAPServer(t)
		empty := ""
		s.sessionUsername = &empty
		s.handler = func(method string, _ map[string]interface{}) interface{} {
			if method != "Mailbox/get" {
				t.Fatalf("unexpected method %s", method)
			}
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		}
		b := s.backend(t)
		if _, err := b.FetchFolders(t.Context()); err != nil {
			t.Fatalf("empty Session username rejected: %v", err)
		}
		want := providerAccountScope(s.server.URL+"/session", "me@example.test", "a1")
		if b.client.accountScope != want {
			t.Fatalf("empty Session username scope = %q, want configured-principal scope %q", b.client.accountScope, want)
		}
		before := b.client.accountScope
		s.password = "rotated secret"
		b.client.credential.secret = "rotated secret"
		if err := b.client.discover(t.Context()); err != nil {
			t.Fatal(err)
		}
		if b.client.accountScope != before {
			t.Fatal("credential rotation changed provider account scope")
		}
	})

	t.Run("omitted", func(t *testing.T) {
		s := newTestJMAPServer(t)
		s.omitUsername = true
		b := s.backend(t)
		if _, err := b.FetchFolders(t.Context()); err == nil || !strings.Contains(err.Error(), "required username") {
			t.Fatalf("omitted Session username error = %v", err)
		}
	})
}

func TestDiscoveryRequiresUnambiguousWritableMailAccount(t *testing.T) {
	mailAccount := func(readOnly bool) map[string]interface{} {
		return map[string]interface{}{
			"name": "Test", "isPersonal": true, "isReadOnly": readOnly,
			"accountCapabilities": map[string]interface{}{mailCapability: map[string]interface{}{}},
		}
	}
	t.Run("multiple without valid primary", func(t *testing.T) {
		s := newTestJMAPServer(t)
		s.sessionAccounts = map[string]interface{}{"a1": mailAccount(false), "a2": mailAccount(false)}
		s.primaryAccounts = map[string]string{}
		if err := s.backend(t).ensure(t.Context()); err == nil || !strings.Contains(err.Error(), "multiple writable mail accounts") {
			t.Fatalf("ambiguous account discovery error = %v", err)
		}
	})
	t.Run("read-only primary with one writable fallback", func(t *testing.T) {
		s := newTestJMAPServer(t)
		s.sessionAccounts = map[string]interface{}{"a1": mailAccount(true), "a2": mailAccount(false)}
		s.primaryAccounts = map[string]string{mailCapability: "a1"}
		b := s.backend(t)
		if err := b.ensure(t.Context()); err != nil {
			t.Fatal(err)
		}
		if b.client.accountID != "a2" {
			t.Fatalf("selected account = %q, want sole writable a2", b.client.accountID)
		}
	})
}

func TestInitialSyncPagesStableIDsBeforeHydratingBodies(t *testing.T) {
	s := newTestJMAPServer(t)
	queryCalls := 0
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		case "Email/query":
			queryCalls++
			if queryCalls == 1 {
				if args["position"] != float64(0) {
					t.Errorf("first query position = %#v", args["position"])
				}
				return map[string]interface{}{"accountId": "a1", "queryState": "q1", "canCalculateChanges": true, "position": 0, "ids": []string{"e1", "e2"}, "total": 3, "limit": 2}
			}
			if args["anchor"] != "e2" || args["anchorOffset"] != float64(1) {
				t.Errorf("second query anchor args = %#v", args)
			}
			if _, ok := args["position"]; ok {
				t.Error("anchored query must not also set position")
			}
			return map[string]interface{}{"accountId": "a1", "queryState": "q1", "canCalculateChanges": true, "position": 2, "ids": []string{"e3"}, "total": 3, "limit": 2}
		case "Email/get":
			ids, _ := args["ids"].([]interface{})
			if len(ids) == 0 {
				return map[string]interface{}{"accountId": "a1", "state": "s1", "list": []interface{}{}, "notFound": []interface{}{}}
			}
			list := make([]interface{}, 0, len(ids))
			for _, rawID := range ids {
				id := rawID.(string)
				list = append(list, emailObject(id, nil, map[string]bool{"inbox-id": true}))
			}
			return map[string]interface{}{"accountId": "a1", "state": "s1", "list": list, "notFound": []interface{}{}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	b := s.backend(t)
	first, err := b.FetchMessages(t.Context(), allMailStream, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Messages) != 0 || len(first.Present) != 2 || !first.HasMore {
		t.Fatalf("first page = %#v", first)
	}
	firstBodies, err := b.FetchSnapshotMessages(t.Context(), first.Present)
	if err != nil || len(firstBodies.Messages) != 2 {
		t.Fatalf("first page hydration = %#v, %v", firstBodies, err)
	}
	second, err := b.FetchMessages(t.Context(), allMailStream, first.Cursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Messages) != 0 || len(second.Present) != 1 || second.HasMore || queryCalls != 2 {
		t.Fatalf("second page = %#v, query calls = %d", second, queryCalls)
	}
	secondBodies, err := b.FetchSnapshotMessages(t.Context(), second.Present)
	if err != nil || len(secondBodies.Messages) != 1 {
		t.Fatalf("second page hydration = %#v, %v", secondBodies, err)
	}
}

func TestCannotCalculateChangesStartsReplacementSnapshot(t *testing.T) {
	s := newTestJMAPServer(t)
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		case "Email/changes":
			return testMethodResponse{name: "error", value: methodError{Type: "cannotCalculateChanges"}}
		case "Email/query":
			return map[string]interface{}{"accountId": "a1", "queryState": "q1", "canCalculateChanges": true, "position": 0, "ids": []string{"e1"}, "total": 1}
		case "Email/get":
			ids, _ := args["ids"].([]interface{})
			if len(ids) == 0 {
				return map[string]interface{}{"accountId": "a1", "state": "new-state", "list": []interface{}{}, "notFound": []interface{}{}}
			}
			return map[string]interface{}{"accountId": "a1", "state": "new-state", "list": []interface{}{emailObject("e1", nil, map[string]bool{"inbox-id": true})}, "notFound": []interface{}{}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	b := s.backend(t)
	result, err := b.FetchMessages(t.Context(), allMailStream, currentCursor(t, b, jmapCursor{EmailState: "expired"}), 10)
	if err != nil {
		t.Fatal(err)
	}
	if !result.FullSnapshot || len(result.Messages) != 0 || len(result.Present) != 1 {
		t.Fatalf("replacement result = %#v", result)
	}
}

func TestReplacementSnapshotIsPagedWithoutPersistingRemoteIDSet(t *testing.T) {
	s := newTestJMAPServer(t)
	queryCalls := 0
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		case "Email/changes":
			return testMethodResponse{name: "error", value: methodError{Type: "cannotCalculateChanges"}}
		case "Email/get":
			return map[string]interface{}{"accountId": "a1", "state": "new-state", "list": []interface{}{}, "notFound": []interface{}{}}
		case "Email/query":
			queryCalls++
			if queryCalls == 1 {
				if args["position"] != float64(0) || args["limit"] != float64(2) {
					t.Errorf("first replacement query args = %#v", args)
				}
				return map[string]interface{}{"accountId": "a1", "queryState": "q1", "canCalculateChanges": true, "position": 0, "ids": []string{"e1", "e2"}, "total": 3}
			}
			if args["anchor"] != "e2" || args["anchorOffset"] != float64(1) {
				t.Errorf("second replacement query args = %#v", args)
			}
			return map[string]interface{}{"accountId": "a1", "queryState": "q1", "canCalculateChanges": true, "position": 2, "ids": []string{"e3"}, "total": 3}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	b := s.backend(t)
	first, err := b.FetchMessages(t.Context(), allMailStream, currentCursor(t, b, jmapCursor{EmailState: "expired"}), 2)
	if err != nil {
		t.Fatal(err)
	}
	state := decodeCursor(first.Cursor)
	if !first.FullSnapshot || !first.HasMore || len(first.Present) != 2 || !state.Replacement ||
		state.QueryAnchor != "e2" || state.QuerySeen != 2 || state.QueryTotal != 3 || len(state.PendingIDs) != 0 {
		t.Fatalf("first replacement page = %#v, cursor=%+v", first, state)
	}
	second, err := b.FetchMessages(t.Context(), allMailStream, first.Cursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !second.FullSnapshot || second.HasMore || len(second.Present) != 1 || decodeCursor(second.Cursor).EmailState != "new-state" {
		t.Fatalf("second replacement page = %#v", second)
	}
}

func TestReplacementQueryDriftInvalidatesCheckpoint(t *testing.T) {
	s := newTestJMAPServer(t)
	queryCalls := 0
	s.handler = func(method string, _ map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		case "Email/changes":
			return testMethodResponse{name: "error", value: methodError{Type: "cannotCalculateChanges"}}
		case "Email/get":
			return map[string]interface{}{"accountId": "a1", "state": "snapshot-state", "list": []interface{}{}, "notFound": []interface{}{}}
		case "Email/query":
			queryCalls++
			if queryCalls == 1 {
				return map[string]interface{}{"accountId": "a1", "queryState": "q1", "canCalculateChanges": true, "position": 0, "ids": []string{"e1"}, "total": 2}
			}
			return map[string]interface{}{"accountId": "a1", "queryState": "q2", "canCalculateChanges": true, "position": 1, "ids": []string{"e2"}, "total": 2}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	b := s.backend(t)

	first, err := b.FetchMessages(t.Context(), allMailStream, currentCursor(t, b, jmapCursor{EmailState: "expired"}), 1)
	if err != nil || !first.FullSnapshot || !first.HasMore {
		t.Fatalf("first replacement page = %#v, err=%v", first, err)
	}
	if _, err := b.FetchMessages(t.Context(), allMailStream, first.Cursor, 1); !errors.Is(err, backend.ErrSnapshotInvalidated) {
		t.Fatalf("query drift error = %v, want ErrSnapshotInvalidated", err)
	}
}

func TestReplacementSnapshotRejectsMissingOrNullRequiredQueryFields(t *testing.T) {
	tests := []struct {
		name   string
		field  string
		remove bool
	}{
		{name: "missing queryState", field: "queryState", remove: true},
		{name: "null queryState", field: "queryState"},
		{name: "missing position", field: "position", remove: true},
		{name: "null position", field: "position"},
		{name: "missing ids", field: "ids", remove: true},
		{name: "null ids", field: "ids"},
		{name: "missing total", field: "total", remove: true},
		{name: "null total", field: "total"},
		{name: "missing accountId", field: "accountId", remove: true},
		{name: "null accountId", field: "accountId"},
		{name: "missing canCalculateChanges", field: "canCalculateChanges", remove: true},
		{name: "null canCalculateChanges", field: "canCalculateChanges"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestJMAPServer(t)
			s.handler = func(method string, _ map[string]interface{}) interface{} {
				switch method {
				case "Mailbox/get":
					return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
				case "Email/query":
					response := map[string]interface{}{
						"accountId": "a1", "queryState": "q1", "canCalculateChanges": true, "position": 0, "ids": []string{}, "total": 0,
					}
					if tt.remove {
						delete(response, tt.field)
					} else {
						response[tt.field] = nil
					}
					return response
				}
				t.Fatalf("unexpected method %s", method)
				return nil
			}
			b := s.backend(t)
			_, err := b.FetchMessages(t.Context(), allMailStream, currentCursor(t, b, jmapCursor{Snapshot: "s1", Replacement: true}), 10)
			if err == nil || !strings.Contains(err.Error(), "omitted required") {
				t.Fatalf("replacement query error = %v", err)
			}
		})
	}
}

func TestEmptyStateTokensRemainPresentAcrossCursorPaging(t *testing.T) {
	t.Run("initial then changes", func(t *testing.T) {
		s := newTestJMAPServer(t)
		changesCalls := 0
		s.handler = func(method string, args map[string]interface{}) interface{} {
			switch method {
			case "Mailbox/get":
				return map[string]interface{}{"accountId": "a1", "state": "", "list": testMailboxes(), "notFound": nil}
			case "Email/get":
				return map[string]interface{}{"accountId": "a1", "state": "", "list": []interface{}{}, "notFound": []interface{}{}}
			case "Email/query":
				return map[string]interface{}{
					"accountId": "a1", "queryState": "", "canCalculateChanges": true,
					"position": 0, "ids": []interface{}{}, "total": 0,
				}
			case "Email/changes":
				changesCalls++
				if since, ok := args["sinceState"].(string); !ok || since != "" {
					t.Fatalf("sinceState = %#v, want a present empty string", args["sinceState"])
				}
				return map[string]interface{}{
					"accountId": "a1", "oldState": "", "newState": "", "hasMoreChanges": false,
					"created": []interface{}{}, "updated": []interface{}{}, "destroyed": []interface{}{},
				}
			}
			t.Fatalf("unexpected method %s", method)
			return nil
		}
		b := s.backend(t)
		initial, err := b.FetchMessages(t.Context(), allMailStream, nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		state := decodeCursor(initial.Cursor)
		if !state.EmailStateSet || state.EmailState != "" || state.SnapshotSet {
			t.Fatalf("initial empty-state cursor = %+v", state)
		}
		next, err := b.FetchMessages(t.Context(), allMailStream, initial.Cursor, 10)
		if err != nil {
			t.Fatal(err)
		}
		state = decodeCursor(next.Cursor)
		if changesCalls != 1 || !state.EmailStateSet || state.EmailState != "" {
			t.Fatalf("changes calls=%d cursor=%+v", changesCalls, state)
		}
	})

	t.Run("replacement query", func(t *testing.T) {
		s := newTestJMAPServer(t)
		queryCalls := 0
		s.handler = func(method string, args map[string]interface{}) interface{} {
			switch method {
			case "Mailbox/get":
				return map[string]interface{}{"accountId": "a1", "state": "", "list": testMailboxes(), "notFound": nil}
			case "Email/query":
				queryCalls++
				if queryCalls == 1 {
					return map[string]interface{}{
						"accountId": "a1", "queryState": "", "canCalculateChanges": true,
						"position": 0, "ids": []string{"e1"}, "total": 2,
					}
				}
				if args["anchor"] != "e1" {
					t.Fatalf("replacement anchor = %#v", args["anchor"])
				}
				return map[string]interface{}{
					"accountId": "a1", "queryState": "", "canCalculateChanges": true,
					"position": 1, "ids": []string{"e2"}, "total": 2,
				}
			}
			t.Fatalf("unexpected method %s", method)
			return nil
		}
		b := s.backend(t)
		cursor := currentCursor(t, b, jmapCursor{Snapshot: "", SnapshotSet: true, Replacement: true})
		first, err := b.FetchMessages(t.Context(), allMailStream, cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		state := decodeCursor(first.Cursor)
		if !first.HasMore || !state.SnapshotSet || !state.QueryStateSet || state.QueryState != "" {
			t.Fatalf("first replacement cursor = %+v", state)
		}
		second, err := b.FetchMessages(t.Context(), allMailStream, first.Cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		state = decodeCursor(second.Cursor)
		if second.HasMore || !state.EmailStateSet || state.EmailState != "" {
			t.Fatalf("final replacement cursor = %+v", state)
		}
	})

	empty := ""
	if err := validateSetResponseState("Email/set", setResponseState{
		OldState: json.RawMessage(`""`), NewState: &empty,
	}); err != nil {
		t.Fatalf("empty Set response states rejected: %v", err)
	}
}

func TestMailboxGetRejectsMalformedResponses(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]interface{})
	}{
		{name: "missing account", mutate: func(r map[string]interface{}) { delete(r, "accountId") }},
		{name: "wrong account", mutate: func(r map[string]interface{}) { r["accountId"] = "other" }},
		{name: "missing state", mutate: func(r map[string]interface{}) { delete(r, "state") }},
		{name: "missing list", mutate: func(r map[string]interface{}) { delete(r, "list") }},
		{name: "missing not found", mutate: func(r map[string]interface{}) { delete(r, "notFound") }},
		{name: "null not found", mutate: func(r map[string]interface{}) { r["notFound"] = nil }},
		{name: "non-empty not found", mutate: func(r map[string]interface{}) { r["notFound"] = []string{"missing"} }},
		{name: "duplicate id", mutate: func(r map[string]interface{}) {
			mailboxes := r["list"].([]map[string]interface{})
			r["list"] = append(mailboxes, mailboxes[0])
		}},
		{name: "missing parent", mutate: func(r map[string]interface{}) {
			delete(r["list"].([]map[string]interface{})[0], "parentId")
		}},
		{name: "missing role", mutate: func(r map[string]interface{}) {
			delete(r["list"].([]map[string]interface{})[0], "role")
		}},
		{name: "missing subscription", mutate: func(r map[string]interface{}) {
			delete(r["list"].([]map[string]interface{})[0], "isSubscribed")
		}},
		{name: "unknown parent", mutate: func(r map[string]interface{}) {
			r["list"].([]map[string]interface{})[0]["parentId"] = "missing"
		}},
		{name: "duplicate role", mutate: func(r map[string]interface{}) {
			r["list"].([]map[string]interface{})[1]["role"] = "inbox"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestJMAPServer(t)
			s.rawResponses = true
			s.handler = func(method string, _ map[string]interface{}) interface{} {
				if method != "Mailbox/get" {
					t.Fatalf("unexpected method %s", method)
				}
				response := map[string]interface{}{
					"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": []string{},
				}
				tt.mutate(response)
				return response
			}
			b := s.backend(t)
			if err := b.loadMailboxes(t.Context()); err == nil {
				t.Fatal("malformed Mailbox/get response accepted")
			}
			if b.mailboxes != nil {
				t.Fatalf("malformed Mailbox/get installed mailboxes: %+v", b.mailboxes)
			}
		})
	}
}

func TestChangesRejectsMalformedResponsesBeforeAdvancingCursor(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]interface{})
	}{
		{name: "missing account", mutate: func(r map[string]interface{}) { delete(r, "accountId") }},
		{name: "wrong account", mutate: func(r map[string]interface{}) { r["accountId"] = "other" }},
		{name: "missing old state", mutate: func(r map[string]interface{}) { delete(r, "oldState") }},
		{name: "wrong old state", mutate: func(r map[string]interface{}) { r["oldState"] = "other" }},
		{name: "missing new state", mutate: func(r map[string]interface{}) { delete(r, "newState") }},
		{name: "missing has more", mutate: func(r map[string]interface{}) { delete(r, "hasMoreChanges") }},
		{name: "missing created", mutate: func(r map[string]interface{}) { delete(r, "created") }},
		{name: "null updated", mutate: func(r map[string]interface{}) { r["updated"] = nil }},
		{name: "missing destroyed", mutate: func(r map[string]interface{}) { delete(r, "destroyed") }},
		{name: "duplicate id", mutate: func(r map[string]interface{}) { r["updated"] = []string{"e1", "e1"} }},
		{name: "empty id", mutate: func(r map[string]interface{}) { r["updated"] = []string{""} }},
		{name: "has more without state progress", mutate: func(r map[string]interface{}) {
			r["hasMoreChanges"] = true
			r["newState"] = "s1"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestJMAPServer(t)
			getCalled := false
			s.handler = func(method string, _ map[string]interface{}) interface{} {
				switch method {
				case "Mailbox/get":
					return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
				case "Email/changes":
					response := map[string]interface{}{
						"accountId": "a1", "oldState": "s1", "newState": "s2", "hasMoreChanges": false,
						"created": []string{}, "updated": []string{"e1"}, "destroyed": []string{},
					}
					tt.mutate(response)
					return response
				case "Email/get":
					getCalled = true
				}
				t.Fatalf("unexpected method %s", method)
				return nil
			}
			b := s.backend(t)
			result, err := b.FetchMessages(t.Context(), allMailStream, currentCursor(t, b, jmapCursor{EmailState: "s1"}), 10)
			if err == nil {
				t.Fatalf("malformed changes response advanced to %+v", decodeCursor(result.Cursor))
			}
			if getCalled {
				t.Fatal("malformed changes response hydrated messages")
			}
		})
	}
}
