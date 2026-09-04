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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
)

func TestChangesAcceptsRFCOverlapsAndEmptyProgressPage(t *testing.T) {
	t.Run("overlap", func(t *testing.T) {
		s := newTestJMAPServer(t)
		s.handler = func(method string, _ map[string]interface{}) interface{} {
			switch method {
			case "Mailbox/get":
				return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
			case "Email/changes":
				return map[string]interface{}{
					"accountId": "a1", "oldState": "s1", "newState": "s2", "hasMoreChanges": false,
					"created": []string{"e1"}, "updated": []string{"e1"}, "destroyed": []string{},
				}
			case "Email/get":
				return map[string]interface{}{"accountId": "a1", "state": "s2", "list": []interface{}{emailObject("e1", nil, nil)}, "notFound": []string{}}
			}
			t.Fatalf("unexpected method %s", method)
			return nil
		}
		b := s.backend(t)
		result, err := b.FetchMessages(t.Context(), allMailStream, currentCursor(t, b, jmapCursor{EmailState: "s1"}), 10)
		if err != nil || len(result.Messages) != 1 || decodeCursor(result.Cursor).EmailState != "s2" {
			t.Fatalf("overlapping changes result = %+v, err=%v", result, err)
		}
	})

	t.Run("empty progress page", func(t *testing.T) {
		s := newTestJMAPServer(t)
		s.handler = func(method string, _ map[string]interface{}) interface{} {
			switch method {
			case "Mailbox/get":
				return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
			case "Email/changes":
				return map[string]interface{}{
					"accountId": "a1", "oldState": "s1", "newState": "s2", "hasMoreChanges": true,
					"created": []string{}, "updated": []string{}, "destroyed": []string{},
				}
			}
			t.Fatalf("unexpected method %s", method)
			return nil
		}
		b := s.backend(t)
		result, err := b.FetchMessages(t.Context(), allMailStream, currentCursor(t, b, jmapCursor{EmailState: "s1"}), 10)
		if err != nil || !result.HasMore || decodeCursor(result.Cursor).EmailState != "s2" {
			t.Fatalf("empty changes progress result = %+v, err=%v", result, err)
		}
	})
}

func TestBlobNotFoundDoesNotDeleteEmailOrAdvanceCursor(t *testing.T) {
	s := newTestJMAPServer(t)
	s.downloadStatus = http.StatusNotFound
	s.handler = func(method string, _ map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		case "Email/changes":
			return map[string]interface{}{
				"accountId": "a1", "oldState": "s1", "newState": "s2", "hasMoreChanges": false,
				"created": []string{}, "updated": []string{"e1"}, "destroyed": []string{},
			}
		case "Email/get":
			return map[string]interface{}{"accountId": "a1", "state": "s2", "list": []interface{}{emailObject("e1", nil, nil)}, "notFound": []string{}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	b := s.backend(t)
	result, err := b.FetchMessages(t.Context(), allMailStream, currentCursor(t, b, jmapCursor{EmailState: "s1"}), 10)
	if err == nil || len(result.Cursor) != 0 || len(result.Deleted) != 0 {
		t.Fatalf("blob 404 result = %+v, err=%v", result, err)
	}
}

func TestUnknownEmailMailboxDoesNotAdvanceCursor(t *testing.T) {
	s := newTestJMAPServer(t)
	s.handler = func(method string, _ map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		case "Email/changes":
			return map[string]interface{}{
				"accountId": "a1", "oldState": "s1", "newState": "s2", "hasMoreChanges": false,
				"created": []string{}, "updated": []string{"e1"}, "destroyed": []string{},
			}
		case "Email/get":
			return map[string]interface{}{
				"accountId": "a1", "state": "s2",
				"list": []interface{}{emailObject("e1", nil, map[string]bool{"unknown-mailbox": true})}, "notFound": []string{},
			}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	b := s.backend(t)
	result, err := b.FetchMessages(t.Context(), allMailStream, currentCursor(t, b, jmapCursor{EmailState: "s1"}), 10)
	if err == nil || len(result.Cursor) != 0 {
		t.Fatalf("unknown mailbox result = %+v, err=%v", result, err)
	}
}

func TestGetRejectsMalformedResponses(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]interface{})
	}{
		{name: "missing account", mutate: func(r map[string]interface{}) { delete(r, "accountId") }},
		{name: "wrong account", mutate: func(r map[string]interface{}) { r["accountId"] = "other" }},
		{name: "missing state", mutate: func(r map[string]interface{}) { delete(r, "state") }},
		{name: "missing list", mutate: func(r map[string]interface{}) { delete(r, "list") }},
		{name: "null list", mutate: func(r map[string]interface{}) { r["list"] = nil }},
		{name: "missing not found", mutate: func(r map[string]interface{}) { delete(r, "notFound") }},
		{name: "omitted requested id", mutate: func(r map[string]interface{}) { r["notFound"] = []string{} }},
		{name: "duplicate list id", mutate: func(r map[string]interface{}) {
			r["list"] = []interface{}{emailObject("e1", nil, nil), emailObject("e1", nil, nil)}
		}},
		{name: "list and not found overlap", mutate: func(r map[string]interface{}) { r["notFound"] = []string{"e1", "e2"} }},
		{name: "unexpected id", mutate: func(r map[string]interface{}) { r["notFound"] = []string{"e3"} }},
		{name: "incomplete object", mutate: func(r map[string]interface{}) {
			r["list"] = []interface{}{map[string]interface{}{"id": "e1"}}
		}},
		{name: "false mailbox membership", mutate: func(r map[string]interface{}) {
			r["list"] = []interface{}{emailObject("e1", nil, map[string]bool{"inbox-id": false})}
		}},
		{name: "false keyword", mutate: func(r map[string]interface{}) {
			r["list"] = []interface{}{emailObject("e1", map[string]bool{"$seen": false}, nil)}
		}},
		{name: "invalid received at", mutate: func(r map[string]interface{}) {
			email := emailObject("e1", nil, nil)
			email["receivedAt"] = "not-a-date"
			r["list"] = []interface{}{email}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestJMAPServer(t)
			s.handler = func(method string, _ map[string]interface{}) interface{} {
				if method != "Email/get" {
					t.Fatalf("unexpected method %s", method)
				}
				response := map[string]interface{}{
					"accountId": "a1", "state": "s1",
					"list": []interface{}{emailObject("e1", nil, nil)}, "notFound": []string{"e2"},
				}
				tt.mutate(response)
				return response
			}
			b := s.backend(t)
			if objects, missing, state, err := b.getEmailObjects(t.Context(), []string{"e1", "e2"}); err == nil {
				t.Fatalf("malformed Email/get accepted: objects=%+v missing=%v state=%q", objects, missing, state)
			}
		})
	}
}

func TestCoreLimitsBoundGetBatchesAndPayloadSizes(t *testing.T) {
	s := newTestJMAPServer(t)
	s.limits = map[string]interface{}{
		"maxObjectsInGet": 2,
		"maxSizeRequest":  300,
		"maxSizeUpload":   3,
	}
	getCalls := 0
	s.handler = func(method string, args map[string]interface{}) interface{} {
		if method != "Email/get" {
			t.Fatalf("unexpected method %s", method)
		}
		getCalls++
		ids := args["ids"].([]interface{})
		list := make([]interface{}, 0, len(ids))
		for _, rawID := range ids {
			id := rawID.(string)
			list = append(list, emailObject(id, nil, nil))
		}
		return map[string]interface{}{"accountId": "a1", "state": "s1", "list": list, "notFound": []interface{}{}}
	}
	b := s.backend(t)
	objects, _, _, err := b.getEmailObjects(t.Context(), []string{"e1", "e2", "e3", "e4", "e5"})
	if err != nil || len(objects) != 5 || getCalls != 3 {
		t.Fatalf("get objects=%d calls=%d err=%v", len(objects), getCalls, err)
	}
	if _, err := b.client.upload(t.Context(), []byte("four"), "text/plain"); err == nil || !strings.Contains(err.Error(), "maxSizeUpload 3") {
		t.Fatalf("oversized upload error = %v", err)
	}
	largeArgs := map[string]interface{}{"accountId": b.client.accountID, "value": strings.Repeat("x", 400)}
	if err := b.client.call(t.Context(), []string{coreCapability}, "Core/echo", largeArgs, nil); err == nil || !strings.Contains(err.Error(), "maxSizeRequest 300") {
		t.Fatalf("oversized request error = %v", err)
	}
}

func TestFetchBodyRejectsOversizedDownload(t *testing.T) {
	s := newTestJMAPServer(t)
	s.downloadBytes = maxMessageBytes + 1
	s.handler = func(method string, _ map[string]interface{}) interface{} {
		if method != "Email/get" {
			t.Fatalf("unexpected method %s", method)
		}
		return map[string]interface{}{
			"accountId": "a1", "state": "s1",
			"list":     []interface{}{emailObject("e1", map[string]bool{}, map[string]bool{"inbox-id": true})},
			"notFound": []interface{}{},
		}
	}
	b := s.backend(t)
	err := b.FetchBody(t.Context(), currentRef(t, b, "e1"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized FetchBody error = %v", err)
	}
}

func TestUploadRejectsMalformedSuccessResponses(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]interface{})
	}{
		{name: "missing account", mutate: func(r map[string]interface{}) { delete(r, "accountId") }},
		{name: "wrong account", mutate: func(r map[string]interface{}) { r["accountId"] = "other" }},
		{name: "missing blob", mutate: func(r map[string]interface{}) { delete(r, "blobId") }},
		{name: "invalid blob", mutate: func(r map[string]interface{}) { r["blobId"] = "bad=" }},
		{name: "missing type", mutate: func(r map[string]interface{}) { delete(r, "type") }},
		{name: "wrong type", mutate: func(r map[string]interface{}) { r["type"] = "application/octet-stream" }},
		{name: "missing size", mutate: func(r map[string]interface{}) { delete(r, "size") }},
		{name: "wrong size", mutate: func(r map[string]interface{}) { r["size"] = 4 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestJMAPServer(t)
			response := map[string]interface{}{
				"accountId": "a1", "blobId": "uploaded-blob", "type": "text/plain", "size": 3,
			}
			tt.mutate(response)
			s.uploadResponse = response
			b := s.backend(t)
			if err := b.ensure(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := b.client.upload(t.Context(), []byte("abc"), "text/plain"); err == nil {
				t.Fatal("malformed upload success response accepted")
			}
		})
	}
}

func TestUploadAcceptsEquivalentMediaType(t *testing.T) {
	s := newTestJMAPServer(t)
	s.uploadResponse = map[string]interface{}{
		"accountId": "a1", "blobId": "uploaded-blob",
		"type": `TEXT/PLAIN;charset="UTF-8"; format=flowed`, "size": 3,
	}
	b := s.backend(t)
	if err := b.ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	blobID, err := b.client.upload(t.Context(), []byte("abc"), "text/plain; format=flowed; charset=utf-8")
	if err != nil || blobID != "uploaded-blob" {
		t.Fatalf("upload blob = %q, err=%v", blobID, err)
	}
}

func TestCoreLimitsRequireCompleteUsableCapability(t *testing.T) {
	complete := `{
		"maxSizeUpload": 1,
		"maxConcurrentUpload": 1,
		"maxSizeRequest": 1,
		"maxConcurrentRequests": 1,
		"maxCallsInRequest": 1,
		"maxObjectsInGet": 1,
		"maxObjectsInSet": 1,
		"collationAlgorithms": []
	}`
	var limits coreLimits
	if err := json.Unmarshal([]byte(complete), &limits); err != nil {
		t.Fatalf("complete core capability: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"maxSizeUpload":1}`), &limits); err == nil || !strings.Contains(err.Error(), "missing required") {
		t.Fatalf("incomplete core capability error = %v", err)
	}

	s := newTestJMAPServer(t)
	s.limits = map[string]interface{}{"maxCallsInRequest": 0}
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
	if _, err := b.FetchFolders(t.Context()); err == nil || !strings.Contains(err.Error(), "does not permit API reads") {
		t.Fatalf("zero-limit discovery error = %v", err)
	}

	s = newTestJMAPServer(t)
	s.limits = map[string]interface{}{
		"maxSizeUpload":       0,
		"maxConcurrentUpload": 0,
		"maxObjectsInSet":     0,
	}
	s.handler = func(method string, _ map[string]interface{}) interface{} {
		if method != "Mailbox/get" {
			t.Fatalf("unexpected method %s", method)
		}
		return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
	}
	b = s.backend(t)
	if _, err := b.FetchFolders(t.Context()); err != nil {
		t.Fatalf("read-only limits rejected discovery: %v", err)
	}
	if _, err := b.client.upload(t.Context(), []byte("x"), "text/plain"); err == nil || !strings.Contains(err.Error(), "does not permit uploads") {
		t.Fatalf("zero-limit upload error = %v", err)
	}
	if err := b.updateEmail(t.Context(), "e1", map[string]interface{}{"keywords/test": true}); err == nil || !strings.Contains(err.Error(), "does not permit Email/set") {
		t.Fatalf("zero-limit set error = %v", err)
	}
}

func TestSingleEmailSetResponsesMustCoverRequestedID(t *testing.T) {
	tests := []struct {
		name     string
		response map[string]interface{}
	}{
		{name: "empty response", response: map[string]interface{}{}},
		{name: "wrong account", response: map[string]interface{}{
			"accountId": "other", "updated": map[string]interface{}{"e1": nil}, "notUpdated": map[string]interface{}{},
		}},
		{name: "missing outcome", response: map[string]interface{}{
			"accountId": "a1", "updated": map[string]interface{}{}, "notUpdated": map[string]interface{}{},
		}},
		{name: "conflicting outcomes", response: map[string]interface{}{
			"accountId": "a1", "updated": map[string]interface{}{"e1": nil},
			"notUpdated": map[string]interface{}{"e1": map[string]string{"type": "serverFail"}},
		}},
		{name: "unexpected outcome", response: map[string]interface{}{
			"accountId": "a1", "updated": map[string]interface{}{"other": nil}, "notUpdated": map[string]interface{}{},
		}},
		{name: "invalid updated value", response: map[string]interface{}{
			"accountId": "a1", "updated": map[string]interface{}{"e1": "invalid"}, "notUpdated": map[string]interface{}{},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestJMAPServer(t)
			s.handler = func(method string, _ map[string]interface{}) interface{} {
				if method != "Email/set" {
					t.Fatalf("unexpected method %s", method)
				}
				return tt.response
			}
			if err := s.backend(t).updateEmail(t.Context(), "e1", map[string]interface{}{"keywords/$seen": true}); err == nil {
				t.Fatal("malformed Email/set response accepted")
			}
		})
	}
}

func TestSetResponsesRequireStateFields(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]interface{})
	}{
		{name: "missing oldState", mutate: func(response map[string]interface{}) { delete(response, "oldState") }},
		{name: "invalid oldState", mutate: func(response map[string]interface{}) { response["oldState"] = 42 }},
		{name: "missing newState", mutate: func(response map[string]interface{}) { delete(response, "newState") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestJMAPServer(t)
			s.handler = func(method string, _ map[string]interface{}) interface{} {
				if method != "Email/set" {
					t.Fatalf("unexpected method %s", method)
				}
				response := map[string]interface{}{
					"accountId": "a1", "oldState": "s1", "newState": "s2",
					"updated": map[string]interface{}{"e1": nil}, "notUpdated": map[string]interface{}{},
				}
				test.mutate(response)
				s.rawResponses = true
				return response
			}
			if err := s.backend(t).updateEmail(t.Context(), "e1", map[string]interface{}{"keywords/$seen": true}); err == nil {
				t.Fatal("malformed Email/set state accepted")
			}
		})
	}

	s := newTestJMAPServer(t)
	s.handler = func(method string, _ map[string]interface{}) interface{} {
		if method != "Email/set" {
			t.Fatalf("unexpected method %s", method)
		}
		s.rawResponses = true
		return map[string]interface{}{
			"accountId": "a1", "oldState": nil, "newState": "s2",
			"updated": map[string]interface{}{"e1": nil}, "notUpdated": map[string]interface{}{},
		}
	}
	if err := s.backend(t).updateEmail(t.Context(), "e1", map[string]interface{}{"keywords/$seen": true}); err != nil {
		t.Fatalf("explicit null oldState rejected: %v", err)
	}
}

func TestEmailSetAcceptsComputedUpdatedProperties(t *testing.T) {
	s := newTestJMAPServer(t)
	s.handler = func(method string, _ map[string]interface{}) interface{} {
		if method != "Email/set" {
			t.Fatalf("unexpected method %s", method)
		}
		return map[string]interface{}{
			"accountId": "a1", "updated": map[string]interface{}{
				"e1": map[string]interface{}{"keywords": map[string]bool{"$seen": true}},
			}, "notUpdated": map[string]interface{}{},
		}
	}
	if err := s.backend(t).updateEmail(t.Context(), "e1", map[string]interface{}{"keywords/$seen": true}); err != nil {
		t.Fatalf("computed updated properties rejected: %v", err)
	}
}

func TestCoreRequestLimitIsSharedAcrossAccountClients(t *testing.T) {
	s := newTestJMAPServer(t)
	s.limits = map[string]interface{}{"maxConcurrentRequests": 1, "maxConcurrentUpload": 1}
	var active atomic.Int32
	var maximum atomic.Int32
	s.handler = func(method string, _ map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		case "Core/echo":
			current := active.Add(1)
			defer active.Add(-1)
			for observed := maximum.Load(); current > observed && !maximum.CompareAndSwap(observed, current); observed = maximum.Load() {
			}
			time.Sleep(40 * time.Millisecond)
			return map[string]interface{}{}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}

	first := s.backend(t)
	second := s.backend(t)
	if _, err := first.FetchFolders(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := second.FetchFolders(t.Context()); err != nil {
		t.Fatal(err)
	}
	if first.client.apiSem != second.client.apiSem {
		t.Fatal("same JMAP account received per-client request limiters")
	}
	if first.client.uploadSem != second.client.uploadSem {
		t.Fatal("same JMAP account received per-client upload limiters")
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, b := range []*Backend{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var result map[string]interface{}
			errs <- b.client.call(t.Context(), []string{coreCapability}, "Core/echo", map[string]interface{}{}, &result)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := maximum.Load(); got != 1 {
		t.Fatalf("maximum concurrent account API requests = %d, want 1", got)
	}
}

func TestJMAPKeywordRoundTripAndPropertyPatches(t *testing.T) {
	keyword, err := encodeDurianKeyword("Project/Alpha")
	if err != nil {
		t.Fatal(err)
	}
	if tag, ok := decodeDurianKeyword(keyword); !ok || tag != "Project/Alpha" {
		t.Fatalf("decode %q = %q, %v", keyword, tag, ok)
	}
	if tag, ok := decodeDurianKeyword("durian-74"); ok || tag != "" {
		t.Fatalf("invalid UTF-8 keyword decoded as %q, %v", tag, ok)
	}
	conflictingKeyword, err := encodeDurianKeyword("project-alpha")
	if err != nil {
		t.Fatal(err)
	}
	b := &Backend{
		mailboxToTag: map[string]string{"inbox-id": "inbox", "project-id": "project-alpha"},
		tagToID:      map[string]string{"inbox": "inbox-id", "project-alpha": "project-id"},
	}
	labels := b.labelsFor(map[string]bool{"inbox-id": true, "project-id": true}, map[string]bool{
		keyword: true, conflictingKeyword: true, "custom": true, "durian-74": true, "$forwarded": true,
	})
	if want := []string{"Project/Alpha", "inbox", "jmap-keyword/custom", "jmap-keyword/durian-74", "jmap-keyword/" + conflictingKeyword, "project-alpha"}; !slices.Equal(labels, want) {
		t.Fatalf("labels = %v, want %v", labels, want)
	}
	if _, err := keywordForTag("jmap-keyword/$seen"); err == nil {
		t.Fatal("reserved system keyword accepted as a label tag")
	}
	if _, err := keywordForTag("jmap-keyword/Uppercase"); err == nil {
		t.Fatal("invalid uppercase JMAP keyword accepted")
	}
	if _, err := encodeDurianKeyword(""); err == nil {
		t.Fatal("empty tag accepted as a Durian keyword")
	}
	if b.ManagesLabelTag("jmap-keyword/Uppercase") || b.ManagesLabelTag("") {
		t.Fatal("invalid keyword-backed tag reported as provider-managed")
	}
	if !b.ManagesLabelTag(strings.Repeat("a", 155)) || b.ManagesLabelTag(strings.Repeat("a", 156)) {
		t.Fatal("arbitrary tag encodability boundary reported incorrectly")
	}

	s := newTestJMAPServer(t)
	var patches []map[string]interface{}
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			mailboxes := append(testMailboxes(), map[string]interface{}{
				"id": "flagged-mailbox", "name": "Flagged", "role": nil, "isSubscribed": true,
			})
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": mailboxes, "notFound": nil}
		case "Email/get":
			return map[string]interface{}{"accountId": "a1", "state": "s1", "list": []interface{}{
				emailObject("e1", nil, map[string]bool{"inbox-id": true, "flagged-mailbox": true}),
			}, "notFound": []interface{}{}}
		case "Email/set":
			patches = append(patches, args["update"].(map[string]interface{})["e1"].(map[string]interface{}))
			return map[string]interface{}{"accountId": "a1", "updated": map[string]interface{}{"e1": nil}, "notUpdated": map[string]interface{}{}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	remote := s.backend(t)
	if err := remote.ApplyLabels(t.Context(), currentRef(t, remote, "e1"), []string{"Project/Alpha"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := remote.ApplyLabels(t.Context(), currentRef(t, remote, "e1"), []string{"jmap-keyword/foo/bar~baz"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := remote.ApplyTagMutation(t.Context(), currentRef(t, remote, "e1"), "unread", true); err != nil {
		t.Fatal(err)
	}
	if err := remote.ApplyTagMutation(t.Context(), currentRef(t, remote, "e1"), "flagged", true); err != nil {
		t.Fatal(err)
	}
	flaggedMailboxTag := "flagged~" + mailboxTagSuffix("flagged-mailbox")
	if labels := remote.labelsFor(map[string]bool{"flagged-mailbox": true}, nil); !slices.Equal(labels, []string{flaggedMailboxTag}) {
		t.Fatalf("flagged mailbox labels = %v, want [%s]", labels, flaggedMailboxTag)
	}
	if err := remote.ApplyLabels(t.Context(), currentRef(t, remote, "e1"), nil, []string{flaggedMailboxTag}); err != nil {
		t.Fatal(err)
	}
	if len(patches) != 5 {
		t.Fatalf("keyword patches = %#v", patches)
	}
	removedFlaggedMailbox, hasRemovedFlaggedMailbox := patches[4]["mailboxIds/flagged-mailbox"]
	if patches[0]["keywords/"+keyword] != true ||
		patches[1]["keywords/foo~1bar~0baz"] != true || patches[2]["keywords/$seen"] != nil ||
		patches[3]["keywords/$flagged"] != true || !hasRemovedFlaggedMailbox || removedFlaggedMailbox != nil {
		t.Fatalf("keyword patches = %#v", patches)
	}
}

func TestDraftKeywordSuppressesSentLabelWithoutDraftsMailbox(t *testing.T) {
	s := newTestJMAPServer(t)
	mailboxes := []map[string]interface{}{
		{"id": "inbox-id", "name": "Inbox", "role": "inbox", "isSubscribed": true},
		{"id": "sent-id", "name": "Sent", "role": "sent", "isSubscribed": true},
	}
	var patch map[string]interface{}
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": mailboxes, "notFound": nil}
		case "Email/get":
			return map[string]interface{}{"accountId": "a1", "state": "s1", "list": []interface{}{
				emailObject("e1", map[string]bool{"$draft": true}, map[string]bool{"sent-id": true}),
			}, "notFound": []interface{}{}}
		case "Email/set":
			patch = args["update"].(map[string]interface{})["e1"].(map[string]interface{})
			return map[string]interface{}{"accountId": "a1", "updated": map[string]interface{}{"e1": nil}, "notUpdated": map[string]interface{}{}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	b := s.backend(t)
	if err := b.loadMailboxes(t.Context()); err != nil {
		t.Fatal(err)
	}
	if labels := b.labelsFor(map[string]bool{"sent-id": true}, map[string]bool{"$draft": true}); !slices.Equal(labels, []string{"draft"}) {
		t.Fatalf("draft labels = %v, want [draft]", labels)
	}
	if err := b.ApplyLabels(t.Context(), currentRef(t, b, "e1"), nil, []string{"draft"}); err != nil {
		t.Fatal(err)
	}
	value, ok := patch["keywords/$draft"]
	if !ok || value != nil || len(patch) != 1 {
		t.Fatalf("clear draft patch = %#v", patch)
	}
}

func TestApplyLabelsKeepsMailboxAndKeywordNamespacesDistinct(t *testing.T) {
	mixedKeyword, err := encodeDurianKeyword("Project Alpha")
	if err != nil {
		t.Fatal(err)
	}
	exactKeyword, err := encodeDurianKeyword("project-alpha")
	if err != nil {
		t.Fatal(err)
	}
	archiveKeyword, err := encodeDurianKeyword("Archive")
	if err != nil {
		t.Fatal(err)
	}

	s := newTestJMAPServer(t)
	var patches []map[string]interface{}
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			mailboxes := append(testMailboxes(), map[string]interface{}{
				"id": "project-alpha-id", "name": "Project Alpha", "role": nil, "isSubscribed": true,
			})
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": mailboxes, "notFound": nil}
		case "Email/get":
			return map[string]interface{}{"accountId": "a1", "state": "s1", "list": []interface{}{
				emailObject("e1", map[string]bool{mixedKeyword: true, exactKeyword: true}, map[string]bool{"project-alpha-id": true}),
			}, "notFound": []interface{}{}}
		case "Email/set":
			patches = append(patches, args["update"].(map[string]interface{})["e1"].(map[string]interface{}))
			return map[string]interface{}{"accountId": "a1", "updated": map[string]interface{}{"e1": nil}, "notUpdated": map[string]interface{}{}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	remote := s.backend(t)
	if _, err := remote.FetchFolders(t.Context()); err != nil {
		t.Fatal(err)
	}
	wantExactLabel := "jmap-keyword/" + exactKeyword
	if labels := remote.labelsFor(
		map[string]bool{"project-alpha-id": true},
		map[string]bool{mixedKeyword: true, exactKeyword: true},
	); !slices.Equal(labels, []string{"Project Alpha", wantExactLabel, "project-alpha"}) {
		t.Fatalf("conflicting mailbox/keyword labels = %v", labels)
	}
	if err := remote.ApplyLabels(t.Context(), currentRef(t, remote, "e1"), nil, []string{"Project Alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := remote.ApplyLabels(t.Context(), currentRef(t, remote, "e1"), nil, []string{wantExactLabel}); err != nil {
		t.Fatal(err)
	}
	if err := remote.ApplyLabels(t.Context(), currentRef(t, remote, "e1"), []string{"Archive"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := remote.ApplyLabels(t.Context(), currentRef(t, remote, "e1"), []string{"jmap-keyword/project-alpha"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(patches) != 4 || patches[0]["keywords/"+mixedKeyword] != nil ||
		patches[1]["keywords/"+exactKeyword] != nil || patches[2]["keywords/"+archiveKeyword] != true ||
		patches[3]["keywords/project-alpha"] != true {
		t.Fatalf("namespace patches = %#v", patches)
	}
	for _, patch := range patches {
		if _, aliasesMailbox := patch["mailboxIds/project-alpha-id"]; aliasesMailbox {
			t.Fatalf("keyword mutation aliased project-alpha mailbox: %#v", patch)
		}
	}
}

func TestFetchSnapshotMetadataDoesNotDownloadBodies(t *testing.T) {
	s := newTestJMAPServer(t)
	s.handler = func(method string, _ map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		case "Email/get":
			return map[string]interface{}{
				"accountId": "a1",
				"state":     "s1",
				"list":      []interface{}{emailObject("e1", map[string]bool{"$seen": true}, map[string]bool{"inbox-id": true})},
				"notFound":  []interface{}{},
			}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	b := s.backend(t)
	if _, err := b.FetchFolders(t.Context()); err != nil {
		t.Fatal(err)
	}
	ref := currentRef(t, b, "e1")
	ref.Folder = allMailStream
	batch, err := b.FetchSnapshotMetadata(t.Context(), []backend.RemoteRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	messages := batch.Messages
	if len(messages) != 1 || messages[0].Ref.ID != b.scopedEmailID("e1") || !messages[0].Flags.Seen {
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
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		case "Email/get":
			return map[string]interface{}{"accountId": "a1", "state": "s1", "list": []interface{}{emailObject("e1", map[string]bool{"$seen": true}, map[string]bool{"inbox-id": true})}, "notFound": []interface{}{}}
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
	if err := b.ApplyFlags(t.Context(), currentRef(t, b, "e1"), backend.Flags{Flagged: true}, backend.Flags{Seen: true}); err != nil {
		t.Fatal(err)
	}
	if err := b.ApplyLabels(t.Context(), currentRef(t, b, "e1"), []string{"archive"}, []string{"inbox"}); err != nil {
		t.Fatal(err)
	}
	seenPatch, hasSeenPatch := patches[0]["keywords/$seen"]
	if len(patches) != 2 || patches[0]["keywords/$flagged"] != true || !hasSeenPatch || seenPatch != nil || patches[1]["mailboxIds/archive-id"] != true {
		t.Fatalf("patches = %#v", patches)
	}
	ref, err := b.Append(t.Context(), "drafts-id", backend.Flags{Seen: true}, []byte(testRaw))
	if err != nil || ref.ID != b.scopedEmailID("imported") || string(s.uploaded) != testRaw {
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
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": mailboxes, "notFound": nil}
		case "Mailbox/set":
			createdArchive = true
			return map[string]interface{}{"accountId": "a1", "created": map[string]interface{}{"archive": map[string]string{"id": "archive-id"}}, "notCreated": map[string]interface{}{}}
		case "Email/get":
			return map[string]interface{}{"accountId": "a1", "state": "s1", "list": []interface{}{emailObject("e1", nil, map[string]bool{"inbox-id": true})}, "notFound": []interface{}{}}
		case "Email/set":
			emailPatch = args["update"].(map[string]interface{})["e1"].(map[string]interface{})
			return map[string]interface{}{"accountId": "a1", "updated": map[string]interface{}{"e1": nil}, "notUpdated": map[string]interface{}{}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	b := s.backend(t)
	if err := b.ApplyLabels(t.Context(), currentRef(t, b, "e1"), []string{"archive"}, []string{"inbox"}); err != nil {
		t.Fatal(err)
	}
	if !createdArchive || emailPatch["mailboxIds/archive-id"] != true || emailPatch["mailboxIds/inbox-id"] != nil {
		t.Fatalf("created=%v patch=%#v", createdArchive, emailPatch)
	}
}

func TestApplyLabelsCreatesMissingTrashMailbox(t *testing.T) {
	s := newTestJMAPServer(t)
	createdTrash := false
	var emailPatch map[string]interface{}
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			mailboxes := testMailboxes()
			if createdTrash {
				mailboxes = append(mailboxes, map[string]interface{}{
					"id": "trash-id", "name": "Trash", "role": "trash", "isSubscribed": true,
				})
			}
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": mailboxes, "notFound": nil}
		case "Mailbox/set":
			create := args["create"].(map[string]interface{})
			trash := create["trash"].(map[string]interface{})
			if trash["role"] != "trash" || trash["name"] != "Trash" {
				t.Fatalf("trash create = %#v", trash)
			}
			createdTrash = true
			return map[string]interface{}{"accountId": "a1", "created": map[string]interface{}{"trash": map[string]string{"id": "trash-id"}}, "notCreated": map[string]interface{}{}}
		case "Email/get":
			return map[string]interface{}{"accountId": "a1", "state": "s1", "list": []interface{}{emailObject("e1", nil, map[string]bool{"inbox-id": true})}, "notFound": []interface{}{}}
		case "Email/set":
			emailPatch = args["update"].(map[string]interface{})["e1"].(map[string]interface{})
			return map[string]interface{}{"accountId": "a1", "updated": map[string]interface{}{"e1": nil}, "notUpdated": map[string]interface{}{}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	b := s.backend(t)
	if err := b.ApplyLabels(t.Context(), currentRef(t, b, "e1"), []string{"trash"}, []string{"inbox"}); err != nil {
		t.Fatal(err)
	}
	if !createdTrash || emailPatch["mailboxIds/trash-id"] != true || emailPatch["mailboxIds/inbox-id"] != nil {
		t.Fatalf("created=%v patch=%#v", createdTrash, emailPatch)
	}
	if _, encodedAsKeyword := emailPatch["keywords/durian-orzgcyti"]; encodedAsKeyword {
		t.Fatalf("trash intent encoded as a keyword: %#v", emailPatch)
	}
}

func TestApplyLabelsMissingRoleCreationFailureDoesNotMutateEmail(t *testing.T) {
	s := newTestJMAPServer(t)
	emailSetCalled := false
	s.handler = func(method string, _ map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		case "Mailbox/set":
			return map[string]interface{}{
				"accountId":  "a1",
				"created":    map[string]interface{}{},
				"notCreated": map[string]interface{}{"trash": map[string]string{"type": "invalidProperties"}},
			}
		case "Email/set":
			emailSetCalled = true
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	b := s.backend(t)
	if err := b.ApplyLabels(t.Context(), currentRef(t, b, "e1"), []string{"trash"}, []string{"inbox"}); err == nil {
		t.Fatal("missing Trash creation unexpectedly succeeded")
	}
	if emailSetCalled {
		t.Fatal("email mutated after role mailbox creation failed")
	}
}

func TestApplyLabelsRejectsForeignRefBeforeCreatingRoleMailbox(t *testing.T) {
	s := newTestJMAPServer(t)
	mailboxSetCalled := false
	emailSetCalled := false
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes()[:3], "notFound": nil}
		case "Mailbox/set":
			mailboxSetCalled = true
			return map[string]interface{}{}
		case "Email/set":
			emailSetCalled = true
			return map[string]interface{}{}
		}
		t.Fatalf("unexpected method %s with args %#v", method, args)
		return nil
	}
	b := s.backend(t)
	err := b.ApplyLabels(t.Context(), backend.RemoteRef{ID: "foreign-scope:e1"}, []string{"trash"}, []string{"inbox"})
	if !errors.Is(err, backend.ErrRefGone) {
		t.Fatalf("foreign ref error = %v, want ErrRefGone", err)
	}
	if mailboxSetCalled || emailSetCalled {
		t.Fatalf("foreign ref mutated account: mailboxSet=%v emailSet=%v", mailboxSetCalled, emailSetCalled)
	}
}

func TestMissingRoleEncodedKeywordRemainsNativeAndRemovable(t *testing.T) {
	trashKeyword, err := encodeDurianKeyword("trash")
	if err != nil {
		t.Fatal(err)
	}
	b := &Backend{mailboxToTag: map[string]string{}, tagToID: map[string]string{}}
	if labels := b.labelsFor(nil, map[string]bool{trashKeyword: true}); !slices.Equal(labels, []string{"jmap-keyword/" + trashKeyword}) {
		t.Fatalf("missing-role keyword labels = %v", labels)
	}

	s := newTestJMAPServer(t)
	var emailPatch map[string]interface{}
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		case "Email/get":
			return map[string]interface{}{"accountId": "a1", "state": "s1", "list": []interface{}{
				emailObject("e1", map[string]bool{trashKeyword: true}, map[string]bool{"inbox-id": true}),
			}, "notFound": []interface{}{}}
		case "Email/set":
			emailPatch = args["update"].(map[string]interface{})["e1"].(map[string]interface{})
			return map[string]interface{}{"accountId": "a1", "updated": map[string]interface{}{"e1": nil}, "notUpdated": map[string]interface{}{}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	remote := s.backend(t)
	if err := remote.ApplyLabels(t.Context(), currentRef(t, remote, "e1"), nil, []string{"jmap-keyword/" + trashKeyword}); err != nil {
		t.Fatal(err)
	}
	value, exists := emailPatch["keywords/"+trashKeyword]
	if !exists || value != nil {
		t.Fatalf("native trash keyword removal patch = %#v", emailPatch)
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

func TestConsumeEventsDoesNotCheckpointTruncatedEvent(t *testing.T) {
	c := &client{accountID: "a1"}
	called := false
	event := "id: 9\ndata: {\"@type\":\"StateChange\",\"changed\":{\"a1\":{\"Email\":\"s2\"}}}\n"
	lastID, _, _, err := c.consumeEvents(t.Context(), strings.NewReader(event), "7", func() { called = true })
	if err != nil {
		t.Fatal(err)
	}
	if called || lastID != "7" {
		t.Fatalf("truncated event called=%v lastID=%q, want false and 7", called, lastID)
	}
	lastID, _, _, err = c.consumeEvents(t.Context(), strings.NewReader(event+"\n"), lastID, func() { called = true })
	if err != nil {
		t.Fatal(err)
	}
	if !called || lastID != "9" {
		t.Fatalf("complete event called=%v lastID=%q, want true and 9", called, lastID)
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

func TestWatchStopsOnNoContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	c := &client{
		httpClient: server.Client(),
		session:    session{EventSourceURL: server.URL},
		accountID:  "a1",
	}
	if err := c.watch(t.Context(), func() {}); err != nil {
		t.Fatalf("watch 204 response = %v", err)
	}
}

func TestWatchExpandsEventSourcePathTemplate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events/Email/no/30" {
			t.Errorf("EventSource path = %q", r.URL.Path)
		}
		http.Error(w, "stop", http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	c := &client{
		httpClient: server.Client(),
		session: session{
			EventSourceURL: server.URL + "/events/{types}/{closeafter}/{ping}",
		},
		accountID: "a1",
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

func TestValidateJMAPRedirect(t *testing.T) {
	origin, _ := http.NewRequest(http.MethodGet, "https://mail.example.test/jmap", nil)
	same, _ := http.NewRequest(http.MethodGet, "https://mail.example.test/next", nil)
	if err := validateJMAPRedirect(same, []*http.Request{origin}); err != nil {
		t.Fatalf("same-origin redirect rejected: %v", err)
	}
	offOrigin, _ := http.NewRequest(http.MethodGet, "https://attacker.example/steal", nil)
	if err := validateJMAPRedirect(offOrigin, []*http.Request{origin}); err == nil {
		t.Fatal("off-origin redirect accepted")
	}
	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = origin
	}
	if err := validateJMAPRedirect(same, via); err == nil {
		t.Fatal("redirect limit was not enforced")
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
