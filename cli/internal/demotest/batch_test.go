package demotest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/demotest"
)

const graphBatch = "https://graph.microsoft.com/v1.0/$batch"
const flagRead = `{"id":"0","method":"GET","url":"/me/messages/m1?$select=id,isRead,flag"}`

func TestGraphFetchFlagsUsesGuardedReadOnlyBatch(t *testing.T) {
	fake := &fakeMail{t: t}
	batches := 0
	transport := transportFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "userinfo") {
			return fake.RoundTrip(r)
		}
		if r.Method != "POST" || r.URL.String() != graphBatch || r.Header.Get("Authorization") != "Bearer fake-ms" {
			t.Fatal("flag read escaped authenticated batch endpoint")
		}
		var envelope struct {
			Requests []struct{ ID, Method, URL string }
		}
		if json.NewDecoder(r.Body).Decode(&envelope) != nil || len(envelope.Requests) > 20 || len(envelope.Requests) == 0 {
			t.Fatal("invalid backend batch shape")
		}
		var responses []any
		for _, item := range envelope.Requests {
			responses = append(responses, map[string]any{"id": item.ID, "status": 200,
				"body": map[string]any{"isRead": true, "flag": map[string]string{"flagStatus": "flagged"}}})
		}
		body, _ := json.Marshal(map[string]any{"responses": responses})
		batches++
		return response(r, 200, string(body)), nil
	})
	m, err := demotest.New(context.Background(), options(transport))
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.BackendFor(account(false))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	refs := make([]backend.RemoteRef, 41)
	for i := range refs {
		refs[i] = backend.RemoteRef{ID: fmt.Sprintf("m%d", i)}
	}
	flags, err := b.FetchFlags(context.Background(), "inbox", refs)
	if err != nil || len(flags) != len(refs) || batches != 3 {
		t.Fatal("real FetchFlags did not traverse guarded batches")
	}
	for _, f := range flags {
		if !f.Seen || !f.Flagged {
			t.Fatal("flag results were lost")
		}
	}
}

func TestGraphBatchCannotSmuggleOtherOperations(t *testing.T) {
	m, fake := setup(t)
	c, _ := m.HTTPClient(account(false))
	wrap := func(item string) string { return `{"requests":[` + item + `]}` }
	for _, body := range []string{
		`{}`, `{"requests":null}`, `{"requests":[]}`,
		wrap(strings.TrimSuffix(strings.Repeat(flagRead+",", 21), ",")),
		wrap(flagRead + "," + flagRead),
		wrap(flagRead) + `{}`,
		`{"requests":[` + flagRead + `],"Requests":[]}`,
		`{"requests":[` + flagRead + `],"extra":true}`,
		wrap(strings.Replace(flagRead, `"method":"GET"`, `"method":"GET","Method":"POST"`, 1)),
		wrap(strings.Replace(flagRead, `"id":"0"`, `"id":"arbitrary"`, 1)),
		wrap(strings.TrimSuffix(flagRead, "}") + `,"headers":{"X-HTTP-Method-Override":"POST"}}`),
		wrap(strings.TrimSuffix(flagRead, "}") + `,"body":{"toRecipients":[{"emailAddress":{"address":"third-party@example.test"}}]}}`),
		wrap(strings.TrimSuffix(flagRead, "}") + `,"dependsOn":["1"]}`),
	} {
		before := fake.calls
		if request(t, c, "POST", graphBatch, "application/json", body) == nil || fake.calls != before {
			t.Fatal("unsafe batch envelope reached transport")
		}
	}
	for _, method := range []string{"POST", "PATCH", "DELETE", "get"} {
		before := fake.calls
		if request(t, c, "POST", graphBatch, "application/json", wrap(strings.Replace(flagRead, "GET", method, 1))) == nil || fake.calls != before {
			t.Fatal("batch mutation reached transport")
		}
	}
	for _, path := range []string{
		"/users/other/messages/m1?$select=id,isRead,flag",
		"https://graph.microsoft.com/me/messages/m1?$select=id,isRead,flag",
		"//evil.example.test/me/messages/m1?$select=id,isRead,flag",
		"/me/messages/m1/send?$select=id,isRead,flag",
		"/me/messages/m1%2fsend?$select=id,isRead,flag",
		"/me/messages/../m1?$select=id,isRead,flag",
		"/me/messages/m1?$select=id,isRead,flag,toRecipients",
		"/me/messages/m1?$select=id,isRead,flag&$expand=attachments",
		"/me/messages/m1?$select=body",
	} {
		unsafe := strings.Replace(flagRead, "/me/messages/m1?$select=id,isRead,flag", path, 1)
		// A valid first item must not authorize any part of a mixed batch.
		body := wrap(flagRead + "," + strings.Replace(unsafe, `"id":"0"`, `"id":"1"`, 1))
		before := fake.calls
		if request(t, c, "POST", graphBatch, "application/json", body) == nil || fake.calls != before {
			t.Fatal("foreign or expanded batch read reached transport")
		}
	}
	for _, endpoint := range []string{
		"https://evil.example.test/v1.0/$batch",
		"https://graph.microsoft.com.evil.example.test/v1.0/$batch",
		"https://graph.microsoft.com:443/v1.0/$batch",
		"https://graph.microsoft.com/v1.0/$batch?unsafe=1",
		"http://graph.microsoft.com/v1.0/$batch",
		graphBase + "$batch",
	} {
		before := fake.calls
		if request(t, c, "POST", endpoint, "application/json", wrap(flagRead)) == nil || fake.calls != before {
			t.Fatal("batch bypassed exact host/path/query checks")
		}
	}
	for _, contentType := range []string{"", "text/plain"} {
		before := fake.calls
		if request(t, c, "POST", graphBatch, contentType, wrap(flagRead)) == nil || fake.calls != before {
			t.Fatal("batch bypassed content type check")
		}
	}
	before := fake.calls
	r, _ := http.NewRequest("POST", graphBatch, strings.NewReader(wrap(flagRead)))
	r.Header.Set("Content-Type", "application/json")
	r.Host = "evil.example.test"
	resp, err := c.Do(r)
	if resp != nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if err == nil || fake.calls != before {
		t.Fatal("batch Host override reached transport")
	}
	if request(t, c, "GET", graphBatch, "application/json", wrap(flagRead)) == nil {
		t.Fatal("batch accepted wrong outer method")
	}
	gmail, _ := m.HTTPClient(account(true))
	before = fake.calls
	if request(t, gmail, "POST", graphBatch, "application/json", wrap(flagRead)) == nil || fake.calls != before {
		t.Fatal("Gmail capability gained Graph batch access")
	}
}
