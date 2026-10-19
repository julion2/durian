package calendartest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const graphTestPrefix = "Durian Test 0123456789abcdef0123456789abcdef "
const graphTestMetadata = `{"id":"test-calendar","name":"Durian Test","isDefaultCalendar":false,"isSharedWithMe":false,"canEdit":true,"canShare":true,"owner":{"address":"demo@example.test"}}`

type graphTestTransport func(*http.Request) (*http.Response, error)

func (f graphTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func graphTestResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func graphTestJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func graphTestSource() *TokenSource {
	// Cached fake credentials ensure these tests can never refresh a provider
	// token; all Graph calls are intercepted below, including bootstrap.
	return &TokenSource{provider: Microsoft, email: "demo@example.test", accessToken: "fake-graph-test-token", expires: time.Now().Add(time.Hour)}
}

func graphTestDefault(t *testing.T, transport http.RoundTripper) {
	t.Helper()
	old := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = old })
}

func graphTestDo(t *testing.T, g *GraphGuard, method, path, body string, allowed bool) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	r, err := http.NewRequest(method, path, reader)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := g.HTTPClient().Do(r)
	if resp != nil {
		resp.Body.Close()
	}
	if (err == nil) != allowed {
		t.Fatalf("%s expected allowed=%v, got %v", method, allowed, err)
	}
}

func graphTestBody(suffix string) string {
	subject := graphTestPrefix + suffix
	return graphTestJSON(map[string]any{
		"subject": subject, "transactionId": GraphTransactionID(subject),
		"body":     map[string]string{"contentType": "text", "content": "isolated appointment"},
		"start":    map[string]string{"dateTime": "2026-10-20T10:00:00", "timeZone": "UTC"},
		"end":      map[string]string{"dateTime": "2026-10-20T11:00:00", "timeZone": "UTC"},
		"location": map[string]string{"displayName": "stub"}, "isAllDay": false, "recurrence": nil,
	})
}

type graphFake struct {
	t             *testing.T
	events        map[string]map[string]any
	calendars     map[string]string
	transactions  map[string]string
	values        []map[string]any
	next          string
	calls, writes int
	losePOST      bool
}

func (f *graphFake) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Helper()
	f.calls++
	if r.URL.Scheme != "https" || r.URL.Host != "graph.microsoft.com" || r.Header.Get("Authorization") != "Bearer fake-graph-test-token" {
		f.t.Fatal("untrusted endpoint or missing guarded credentials")
	}
	if r.Header.Get("Prefer") != graphPrefer {
		f.t.Fatal("lost immutable-ID/UTC preference")
	}
	path := r.URL.Path
	if path == "/v1.0/me/calendars" {
		return graphTestResponse(r, 200, `{"value":[`+graphTestMetadata+`]}`), nil
	}
	if path == "/v1.0/me/calendars/test-calendar" {
		return graphTestResponse(r, 200, graphTestMetadata), nil
	}
	if r.Method == "POST" && path == "/v1.0/me/calendars/test-calendar/events" {
		f.writes++
		var e map[string]any
		if json.NewDecoder(r.Body).Decode(&e) != nil {
			f.t.Fatal("invalid forwarded JSON")
		}
		if e["isOnlineMeeting"] != false || e["isReminderOn"] != false {
			f.t.Fatal("guard inherited conferencing/reminder defaults")
		}
		tx := e["transactionId"].(string)
		id := f.transactions[tx]
		if id == "" {
			id = fmt.Sprintf("event-%d", len(f.transactions)+1)
			f.transactions[tx] = id
		}
		e["id"], e["type"] = id, "singleInstance"
		if e["recurrence"] != nil {
			e["type"] = "seriesMaster"
		}
		e["attendees"], e["isOrganizer"], e["isOnlineMeeting"] = []any{}, true, false
		e["onlineMeeting"], e["onlineMeetingUrl"], e["@odata.etag"] = nil, nil, `W/"version-1"`
		e["changeKey"] = "version-1"
		f.events[id], f.calendars[id] = e, "test-calendar"
		if f.losePOST {
			return nil, io.ErrUnexpectedEOF
		}
		return graphTestResponse(r, 201, graphTestJSON(e)), nil
	}
	if strings.HasSuffix(path, "/calendarView") || path == "/v1.0/me/calendars/test-calendar/events" || strings.HasSuffix(path, "/instances") {
		if r.Method != "GET" {
			f.t.Fatal("unexpected list mutation")
		}
		if r.URL.Query().Get("$select") == "id" {
			// Exact-ID membership fallback, independent of the scenario's
			// provider listing pages; a moved item is no longer a member.
			values := []map[string]string{}
			for id, event := range f.events {
				instance := event["type"] == "occurrence" || event["type"] == "exception"
				if f.calendars[id] == "test-calendar" && instance == strings.HasSuffix(path, "/calendarView") {
					values = append(values, map[string]string{"id": id})
				}
			}
			return graphTestResponse(r, 200, graphTestJSON(map[string]any{"value": values})), nil
		}
		for _, field := range []string{"id", "seriesMasterId", "isOnlineMeeting", "attendees"} {
			if !strings.Contains(r.URL.Query().Get("$select"), field) {
				f.t.Fatal("binding fields missing from paging select")
			}
		}
		return graphTestResponse(r, 200, graphTestJSON(map[string]any{"value": f.values, "@odata.nextLink": f.next})), nil
	}
	id := strings.TrimPrefix(path, "/v1.0/me/events/")
	if strings.HasSuffix(id, "/calendar") {
		return graphTestResponse(r, 200, graphTestJSON(map[string]string{"id": f.calendars[strings.TrimSuffix(id, "/calendar")]})), nil
	}
	e, ok := f.events[id]
	if !ok {
		return graphTestResponse(r, 404, `{"error":"withheld"}`), nil
	}
	switch r.Method {
	case "GET":
		return graphTestResponse(r, 200, graphTestJSON(e)), nil
	case "PATCH", "DELETE":
		if r.Header.Get("If-Match") != e["@odata.etag"] {
			return graphTestResponse(r, 412, `{"error":{"code":"ErrorPreconditionFailed"}}`), nil
		}
		f.writes++
		if r.Method == "DELETE" {
			delete(f.events, id)
			return graphTestResponse(r, 204, ""), nil
		}
		var patch map[string]any
		if json.NewDecoder(r.Body).Decode(&patch) != nil {
			f.t.Fatal("bad patch")
		}
		for k, v := range patch {
			e[k] = v
		}
		return graphTestResponse(r, 200, graphTestJSON(e)), nil
	default:
		f.t.Fatal("unexpected forwarded request")
		return nil, fmt.Errorf("unexpected request")
	}
}

func graphFixture(t *testing.T) (*GraphGuard, *graphFake) {
	t.Helper()
	f := &graphFake{t: t, events: map[string]map[string]any{}, calendars: map[string]string{}, transactions: map[string]string{}}
	graphTestDefault(t, f)
	pin, err := EnsureGraphCalendar(context.Background(), graphTestSource(), "test-calendar")
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewGraphGuard(pin, graphTestPrefix)
	if err != nil {
		t.Fatal(err)
	}
	return g, f
}

func TestGraphGuardOwnLifecycleAndMovedMembership(t *testing.T) {
	g, f := graphFixture(t)
	base := GraphBase + "/me/calendars/test-calendar/events"
	graphTestDo(t, g, "POST", base, graphTestBody("single"), true)
	if got := g.CreatedIDs(); len(got) != 1 || got[0] != "event-1" {
		t.Fatalf("created IDs: %v", got)
	}
	global := GraphBase + "/me/events/event-1"
	graphTestDo(t, g, "GET", global+"?$select=id,subject", "", true)
	graphTestDo(t, g, "PATCH", global, `{"subject":"`+graphTestPrefix+`edited"}`, true)
	r, _ := http.NewRequest("PATCH", global, strings.NewReader(`{"isAllDay":false}`))
	r.Header.Set("If-Match", "version-1") // real provider's changeKey seam
	resp, err := g.HTTPClient().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatal("caller changeKey was replaced instead of reaching provider")
	}
	resp.Body.Close()
	f.calendars["event-1"] = "other-calendar" // immutable IDs survive moves
	before := f.writes
	for _, method := range []string{"GET", "PATCH", "DELETE"} {
		body := ""
		if method == "PATCH" {
			body = `{"isAllDay":false}`
		}
		graphTestDo(t, g, method, global, body, false)
	}
	if f.writes != before {
		t.Fatal("moved event mutated")
	}
	f.calendars["event-1"] = "test-calendar"
	f.events["event-1"]["attendees"] = []any{map[string]any{"emailAddress": map[string]string{"address": "stub@example.test"}}}
	graphTestDo(t, g, "DELETE", global, "", false)
	f.events["event-1"]["attendees"] = []any{}
	f.events["event-1"]["isOnlineMeeting"] = true
	graphTestDo(t, g, "DELETE", global, "", false)
	f.events["event-1"]["isOnlineMeeting"] = false
	delete(f.events["event-1"], "attendees")
	graphTestDo(t, g, "DELETE", global, "", false)
	f.events["event-1"]["attendees"] = []any{}
	graphTestDo(t, g, "DELETE", global, "", true)
}

func TestGraphGuardBlocksUnsafeRequestsBeforeTransport(t *testing.T) {
	g, f := graphFixture(t)
	base := GraphBase + "/me/calendars/test-calendar/events"
	body := graphTestBody("single")
	for _, tc := range []struct{ name, method, target, body string }{
		{"wrong-calendar", "POST", strings.Replace(base, "test-calendar", "other-calendar", 1), body},
		{"default-calendar", "POST", GraphBase + "/me/calendar/events", body},
		{"global-create", "POST", GraphBase + "/me/events", body},
		{"global-unrelated-delete", "DELETE", GraphBase + "/me/events/unrelated", ""},
		{"global-unrelated-read", "GET", GraphBase + "/me/events/unrelated", ""},
		{"calendar-delete", "DELETE", GraphBase + "/me/calendars/test-calendar", ""},
		{"other-host", "POST", strings.Replace(base, "graph.microsoft.com", "example.test", 1), body},
		{"host-suffix", "POST", strings.Replace(base, "graph.microsoft.com", "graph.microsoft.com.example.test", 1), body},
		{"http", "POST", strings.Replace(base, "https:", "http:", 1), body},
		{"port", "POST", strings.Replace(base, "graph.microsoft.com", "graph.microsoft.com:443", 1), body},
		{"notification-query", "POST", base + "?sendUpdates=none", body},
		{"expand", "GET", base + "?$expand=calendar", ""},
		{"duplicate-query", "GET", base + "?$top=1&$top=2", ""},
		{"bad-query", "GET", base + "?bad=%zz", ""},
		{"wrong-prefix", "POST", base, strings.Replace(body, graphTestPrefix, "unrelated ", 1)},
		{"wrong-transaction", "POST", base, strings.Replace(body, GraphTransactionID(graphTestPrefix+"single"), "unrelated", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := f.calls
			graphTestDo(t, g, tc.method, tc.target, tc.body, false)
			if f.calls != before {
				t.Fatal("unsafe request reached transport")
			}
		})
	}
	for _, extra := range []string{
		`"attendees":[]`, `"attendees":[{"emailAddress":{"address":"stub@example.test"}}]`,
		`"isOnlineMeeting":true`, `"isOnlineMeeting":false`, `"onlineMeetingProvider":"teamsForBusiness"`,
		`"isReminderOn":true`, `"isReminderOn":false`, `"reminderMinutesBeforeStart":0`,
		`"organizer":{}`, `"responseRequested":false`, `"sendResponse":false`, `"attachments":[]`, `"futureRecipient":{}`,
		`"location":{"displayName":"room","locationEmailAddress":"stub@example.test"}`,
		`"start":{"dateTime":"2026-10-20T10:00:00","recipient":"stub@example.test"}`,
		`"body":{"contentType":"html","content":"<img src='https://example.test'>"}`,
		`"recurrence":{"pattern":{"type":"daily","attendees":[]},"range":{"type":"noEnd"}}`,
	} {
		var m map[string]json.RawMessage
		json.Unmarshal([]byte(body), &m)
		var patch map[string]json.RawMessage
		json.Unmarshal([]byte("{"+extra+"}"), &patch)
		for k, v := range patch {
			m[k] = v
		}
		before := f.calls
		graphTestDo(t, g, "POST", base, graphTestJSON(m), false)
		if f.calls != before {
			t.Fatalf("unsafe body reached transport: %s", extra)
		}
	}
	graphTestDo(t, g, "POST", base, body, true)
	for _, action := range []string{"move", "accept", "decline", "forward", "attachments"} {
		graphTestDo(t, g, "POST", GraphBase+"/me/events/event-1/"+action, `{}`, false)
	}
	r, _ := http.NewRequest("GET", base, nil)
	r.Header.Set("X-HTTP-Method-Override", "DELETE")
	if _, err := g.HTTPClient().Do(r); err == nil {
		t.Fatal("method override accepted")
	}
	r, _ = http.NewRequest("DELETE", GraphBase+"/me/events/event-1", nil)
	r.Header.Set("If-Match", `W/"stale"`)
	resp, err := g.HTTPClient().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatal("provider stale-ETag rejection was hidden")
	}
}

func TestGraphGuardInstancesAndPaging(t *testing.T) {
	g, f := graphFixture(t)
	body := strings.Replace(graphTestBody("series"), `"recurrence":null`, `"recurrence":{"pattern":{"type":"daily","interval":1},"range":{"type":"numbered","startDate":"2026-10-20","numberOfOccurrences":2}}`, 1)
	graphTestDo(t, g, "POST", GraphBase+"/me/calendars/test-calendar/events", body, true)
	instance := map[string]any{}
	for k, v := range f.events["event-1"] {
		instance[k] = v
	}
	instance["id"], instance["type"], instance["seriesMasterId"] = "opaque-instance", "occurrence", "event-1"
	f.events["opaque-instance"], f.calendars["opaque-instance"] = instance, "test-calendar"
	f.values = []map[string]any{instance}
	global := GraphBase + "/me/events/opaque-instance"
	graphTestDo(t, g, "DELETE", global, "", false) // no ID-shape guessing
	f.calendars["opaque-instance"] = "other-calendar"
	graphTestDo(t, g, "GET", GraphBase+"/me/events/event-1/instances", "", false)
	if len(g.CreatedIDs()) != 1 {
		t.Fatal("wrong-calendar instance enrolled")
	}
	f.calendars["opaque-instance"] = "test-calendar"
	graphTestDo(t, g, "GET", GraphBase+"/me/events/event-1/instances?startDateTime=2026-10-20&endDateTime=2026-10-22", "", true)
	graphTestDo(t, g, "GET", global, "", true)
	graphTestDo(t, g, "PATCH", global, `{"subject":"`+graphTestPrefix+`instance edited"}`, true)
	f.calendars["opaque-instance"] = "other-calendar"
	graphTestDo(t, g, "DELETE", global, "", false)
	f.calendars["opaque-instance"] = "test-calendar"
	instance["type"] = "exception"
	unrelated := map[string]any{}
	for k, v := range instance {
		unrelated[k] = v
	}
	unrelated["id"], unrelated["seriesMasterId"] = "unrelated-instance", "unrelated-master"
	f.values = append(f.values, unrelated)
	// calendarView (FetchInstances) and /events (FetchMasterEvents) both
	// support pages, retaining the immutable ID preference on every request.
	for _, path := range []string{"calendarView", "events"} {
		target := GraphBase + "/me/calendars/test-calendar/" + path
		f.next = target + "?$skiptoken=opaque"
		graphTestDo(t, g, "GET", target+"?$select=id,subject", "", true)
		f.next = ""
		graphTestDo(t, g, "GET", target+"?$skiptoken=opaque", "", true)
		for _, next := range []string{"https://example.test/stolen", GraphBase + "/me/events", strings.Replace(target, "test-calendar", "other-calendar", 1)} {
			f.next = next
			graphTestDo(t, g, "GET", target, "", false)
		}
	}
	f.next = ""
	graphTestDo(t, g, "DELETE", GraphBase+"/me/events/unrelated-instance", "", false)
	graphTestDo(t, g, "DELETE", global, "", true)
	graphTestDo(t, g, "DELETE", GraphBase+"/me/events/event-1", "", true)
}

func TestGraphGuardLostPOSTRetainsUnknownWithoutSweeping(t *testing.T) {
	g, f := graphFixture(t)
	base := GraphBase + "/me/calendars/test-calendar/events"
	body := graphTestBody("lost")
	f.losePOST = true
	graphTestDo(t, g, "POST", base, body, false)
	if f.writes != 1 || len(g.CreatedIDs()) != 0 || len(g.UnknownTransactions()) != 1 {
		t.Fatal("lost POST was retried or authorized cleanup")
	}
	f.values = []map[string]any{f.events["event-1"]}
	graphTestDo(t, g, "GET", base, "", true)
	graphTestDo(t, g, "DELETE", GraphBase+"/me/events/event-1", "", false)
	graphTestDo(t, g, "POST", base, strings.Replace(body, "isolated appointment", "changed payload", 1), false)
	if f.writes != 1 {
		t.Fatal("same transaction accepted with a different payload")
	}
	f.losePOST = false
	graphTestDo(t, g, "POST", base, body, true) // exact retry; stub deduplicates transactionId
	if len(g.CreatedIDs()) != 1 || len(g.UnknownTransactions()) != 0 || len(f.events) != 1 {
		t.Fatal("exact transaction retry not safely recovered")
	}
}

func TestGraphBootstrapMetadataOwnershipAndCreation(t *testing.T) {
	for _, tc := range []struct {
		name, list, verified string
		allowed, create      bool
	}{
		{"create-absent", `{"value":[]}`, "", true, true},
		{"unique-owned-existing", `{"value":[` + graphTestMetadata + `]}`, "", true, false},
		{"verified-existing", `{"value":[` + graphTestMetadata + `]}`, "test-calendar", true, false},
		{"wrong-pin", `{"value":[` + graphTestMetadata + `]}`, "other-calendar", false, false},
		{"missing-pin", `{"value":[]}`, "test-calendar", false, false},
		{"ambiguous", `{"value":[` + graphTestMetadata + `,` + graphTestMetadata + `]}`, "test-calendar", false, false},
		{"default", `{"value":[` + strings.Replace(graphTestMetadata, `"isDefaultCalendar":false`, `"isDefaultCalendar":true`, 1) + `]}`, "test-calendar", false, false},
		{"shared", `{"value":[` + strings.Replace(graphTestMetadata, `"isSharedWithMe":false`, `"isSharedWithMe":true`, 1) + `]}`, "test-calendar", false, false},
		{"not-owner", `{"value":[` + strings.Replace(graphTestMetadata, `"canShare":true`, `"canShare":false`, 1) + `]}`, "test-calendar", false, false},
		{"wrong-identity", `{"value":[` + strings.Replace(graphTestMetadata, "demo@example.test", "other@example.test", 1) + `]}`, "", false, false},
		{"missing-owner", `{"value":[` + strings.Replace(graphTestMetadata, `"demo@example.test"`, `""`, 1) + `]}`, "", false, false},
		{"wrong-case-name", `{"value":[` + strings.Replace(graphTestMetadata, `"Durian Test"`, `"durian test"`, 1) + `]}`, "", false, false},
		{"not-editable", `{"value":[` + strings.Replace(graphTestMetadata, `"canEdit":true`, `"canEdit":false`, 1) + `]}`, "", false, false},
		{"missing-safety-metadata", `{"value":[{"id":"test-calendar","name":"Durian Test"}]}`, "test-calendar", false, false},
		{"missing-calendar-list", `{}`, "", false, false},
		{"null-calendar-list", `{"value":null}`, "", false, false},
		{"bad-paging-host", `{"value":[],"@odata.nextLink":"https://example.test/calendars"}`, "", false, false},
		{"bad-paging-path", `{"value":[],"@odata.nextLink":"` + GraphBase + `/me/events"}`, "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts := 0
			graphTestDefault(t, graphTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "graph.microsoft.com" || r.URL.Scheme != "https" {
					t.Fatal("unsafe bootstrap host")
				}
				if r.URL.Path == "/v1.0/me/calendars" && r.Method == "GET" {
					if r.URL.Query().Get("$select") != graphCalendarSelect {
						t.Fatal("bootstrap requested more than metadata")
					}
					return graphTestResponse(r, 200, tc.list), nil
				}
				if r.URL.Path == "/v1.0/me/calendars" && r.Method == "POST" {
					posts++
					body, _ := io.ReadAll(r.Body)
					if string(body) != `{"name":"Durian Test"}` {
						t.Fatal("unexpected calendar create body")
					}
					return graphTestResponse(r, 201, `{"id":"test-calendar","name":"Durian Test"}`), nil
				}
				if r.URL.Path == "/v1.0/me/calendars/test-calendar" && r.Method == "GET" {
					return graphTestResponse(r, 200, graphTestMetadata), nil
				}
				t.Fatal("bootstrap performed non-metadata I/O")
				return nil, io.ErrUnexpectedEOF
			}))
			pin, err := EnsureGraphCalendar(context.Background(), graphTestSource(), tc.verified)
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v, err=%v", tc.allowed, err)
			}
			if tc.allowed && pin.ID() != "test-calendar" {
				t.Fatal("wrong pin")
			}
			if tc.allowed {
				_, guardErr := NewGraphGuard(pin, graphTestPrefix)
				if guardErr != nil {
					t.Fatal("verified calendar provenance should authorize guard without human attestation")
				}
			}
			if (posts == 1) != tc.create || posts > 1 {
				t.Fatal("unexpected calendar POST count")
			}
		})
	}
}

func TestGraphGuardRejectsRedirectsBoundsAndCancellation(t *testing.T) {
	for _, redirect := range []string{GraphBase + "/me/calendars/test-calendar/events", "https://example.test/stolen"} {
		t.Run(redirect, func(t *testing.T) {
			g, _ := graphFixture(t)
			calls := 0
			g.transport = graphTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				resp := graphTestResponse(r, 307, "")
				resp.Header.Set("Location", redirect)
				return resp, nil
			})
			graphTestDo(t, g, "GET", GraphBase+"/me/calendars/test-calendar/events", "", false)
			if calls != 1 {
				t.Fatal("redirect followed")
			}
		})
	}
	g, f := graphFixture(t)
	graphTestDo(t, g, "POST", GraphBase+"/me/calendars/test-calendar/events", strings.Repeat(" ", (1<<20)+1), false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, _ := http.NewRequestWithContext(ctx, "GET", GraphBase+"/me/calendars/test-calendar/events", nil)
	before := f.calls
	if _, err := g.HTTPClient().Do(r); err == nil || f.calls != before {
		t.Fatal("canceled request forwarded")
	}
	for _, status := range []int{200, 400, 412} {
		g.transport = graphTestTransport(func(r *http.Request) (*http.Response, error) {
			return graphTestResponse(r, status, strings.Repeat("x", graphLimit+1)), nil
		})
		graphTestDo(t, g, "GET", GraphBase+"/me/calendars/test-calendar/events", "", false)
	}
}

func TestGraphUnverifiedCreateAndCalendarPOSTAreRetained(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(corrupt), func(t *testing.T) {
			g, f := graphFixture(t)
			g.transport = graphTestTransport(func(r *http.Request) (*http.Response, error) {
				resp, err := f.RoundTrip(r)
				if r.Method == "POST" && err == nil {
					if corrupt {
						resp.Body.Close()
						resp.Body = io.NopCloser(strings.NewReader(`{"id":"unrelated"}`))
					} else {
						f.calendars["event-1"] = "other-calendar"
					}
				}
				return resp, err
			})
			graphTestDo(t, g, "POST", GraphBase+"/me/calendars/test-calendar/events", graphTestBody("unverified"), false)
			if len(g.CreatedIDs()) != 0 || len(g.UnknownTransactions()) != 1 {
				t.Fatal("unverified POST authorized cleanup")
			}
		})
	}
	posts := 0
	graphTestDefault(t, graphTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return graphTestResponse(r, 200, `{"value":[]}`), nil
		}
		if r.Method != "POST" || r.URL.Path != "/v1.0/me/calendars" {
			t.Fatal("unexpected bootstrap operation")
		}
		posts++
		return nil, io.ErrUnexpectedEOF
	}))
	pin, err := EnsureGraphCalendar(context.Background(), graphTestSource(), "")
	if err == nil || pin != nil || posts != 1 || !strings.Contains(err.Error(), "unknown") {
		t.Fatal("lost calendar POST did not stop for manual inspection")
	}
}

func TestGraphGuardPreservesProviderFailuresAndCallerIfMatch(t *testing.T) {
	for _, method := range []string{"GET", "POST", "PATCH", "DELETE"} {
		for _, status := range []int{http.StatusBadRequest, http.StatusPreconditionFailed} {
			t.Run(fmt.Sprintf("%s-%d", method, status), func(t *testing.T) {
				g, f := graphFixture(t)
				base := GraphBase + "/me/calendars/test-calendar/events"
				graphTestDo(t, g, "POST", base, graphTestBody("setup"), true)
				membershipChecked, forwarded := false, 0
				const original = `W/"caller-stale-version"`
				const failureBody = `{"error":{"code":"ErrorInvalidRequest","message":"private provider detail"}}`
				g.transport = graphTestTransport(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/calendar") {
						membershipChecked = true
					}
					if r.Method == method && (method != "GET" || r.URL.Path == "/v1.0/me/calendars/test-calendar/events") {
						forwarded++
						if method == "PATCH" || method == "DELETE" {
							if !membershipChecked || r.Header.Get("If-Match") != original {
								t.Fatal("mutation skipped membership check or rewrote explicit caller If-Match")
							}
						}
						resp := graphTestResponse(r, status, failureBody)
						resp.Header.Set("request-id", "mock-provider-request")
						return resp, nil
					}
					return f.RoundTrip(r)
				})
				target, body := base, ""
				if method == "POST" {
					body = graphTestBody("rejected")
				} else if method == "PATCH" || method == "DELETE" {
					target = GraphBase + "/me/events/event-1"
					if method == "PATCH" {
						body = `{"isAllDay":false}`
					}
				}
				var reader io.Reader
				if body != "" {
					reader = strings.NewReader(body)
				}
				req, _ := http.NewRequest(method, target, reader)
				req.Header.Set("If-Match", original)
				resp, err := g.HTTPClient().Do(req)
				if err != nil {
					t.Fatalf("provider HTTP failure became a local transport error: %v", err)
				}
				defer resp.Body.Close()
				data, _ := io.ReadAll(resp.Body)
				if forwarded != 1 || resp.StatusCode != status || string(data) != failureBody || resp.Header.Get("request-id") != "mock-provider-request" {
					t.Fatal("bounded provider failure not observable unchanged by callers/recorders")
				}
				unknown := 0
				if method == "POST" && status != http.StatusBadRequest {
					unknown = 1
				}
				if len(g.CreatedIDs()) != 1 || len(g.UnknownTransactions()) != unknown {
					t.Fatal("failed provider response incorrectly enrolled a cleanup ID")
				}
			})
		}
	}
}

func TestGraphGuardSafetyGETRequires200(t *testing.T) {
	for _, seam := range []string{"calendar-metadata", "event-safety", "membership"} {
		t.Run(seam, func(t *testing.T) {
			g, f := graphFixture(t)
			graphTestDo(t, g, "POST", GraphBase+"/me/calendars/test-calendar/events", graphTestBody("setup"), true)
			before := f.writes
			g.transport = graphTestTransport(func(r *http.Request) (*http.Response, error) {
				resp, err := f.RoundTrip(r)
				if err == nil && r.Method == "GET" && ((seam == "calendar-metadata" && r.URL.Path == "/v1.0/me/calendars/test-calendar") ||
					(seam == "event-safety" && r.URL.Path == "/v1.0/me/events/event-1") ||
					(seam == "membership" && strings.HasSuffix(r.URL.Path, "/calendar"))) {
					// Even a perfectly valid success-shaped JSON body on a
					// non-200 response must not authorize global mutation.
					resp.StatusCode = http.StatusBadRequest
				}
				return resp, err
			})
			graphTestDo(t, g, "PATCH", GraphBase+"/me/events/event-1", `{"isAllDay":false}`, false)
			if f.writes != before {
				t.Fatal("failed safety GET authorized a mailbox-global mutation")
			}
		})
	}
}

func TestGraphBootstrapIdentityProvenanceAndFreshMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, email, metadata string
		listStatus            int
		create, allowed       bool
	}{
		{"new-provenance-without-email", "", graphTestMetadata, 200, true, true},
		{"reuse-requires-demo-identity", "", graphTestMetadata, 200, false, false},
		{"reuse-case-insensitive-identity", "DEMO@EXAMPLE.TEST", graphTestMetadata, 200, false, true},
		{"personal-account-no-shared-property", "demo@example.test", strings.Replace(graphTestMetadata, `"isSharedWithMe":false,`, "", 1), 200, false, true},
		{"no-shared-property-still-requires-owner", "demo@example.test", strings.Replace(strings.Replace(graphTestMetadata, `"isSharedWithMe":false,`, "", 1), `"canShare":true`, `"canShare":false`, 1), 200, false, false},
		{"reuse-owner-changed", "demo@example.test", strings.Replace(graphTestMetadata, "demo@example.test", "other@example.test", 1), 200, false, false},
		{"new-not-owner", "demo@example.test", strings.Replace(graphTestMetadata, `"canShare":true`, `"canShare":false`, 1), 200, true, false},
		{"metadata-list-http-error", "demo@example.test", graphTestMetadata, 400, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts := 0
			graphTestDefault(t, graphTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" {
					posts++
					return graphTestResponse(r, 201, `{"id":"test-calendar","name":"Durian Test"}`), nil
				}
				if r.URL.Path == "/v1.0/me/calendars" {
					if strings.Contains(r.URL.Query().Get("$select"), "isSharedWithMe") {
						t.Fatal("personal Graph endpoint rejects isSharedWithMe selection")
					}
					list := `{"value":[` + graphTestMetadata + `]}`
					if tc.create {
						list = `{"value":[]}`
					}
					return graphTestResponse(r, tc.listStatus, list), nil
				}
				return graphTestResponse(r, 200, tc.metadata), nil
			}))
			s := graphTestSource()
			s.email = tc.email
			pin, err := EnsureGraphCalendar(context.Background(), s, "")
			if (err == nil) != tc.allowed || posts > 1 || (tc.listStatus != 200 && posts != 0) {
				t.Fatal("bootstrap ignored identity, provenance, or fresh ownership metadata")
			}
			if tc.allowed {
				if _, err := NewGraphGuard(pin, graphTestPrefix); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestGraphGuardPersonalAccountMembershipFallback(t *testing.T) {
	g, f := graphFixture(t)
	g.transport = graphTestTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/calendar") {
			// Observed on the real personal-account API: navigation incorrectly
			// reports the primary calendar, even for another folder's event.
			return graphTestResponse(r, 200, `{"id":"primary-calendar"}`), nil
		}
		if strings.HasSuffix(r.URL.Path, "/calendarView") && r.URL.Query().Get("$select") == "id" {
			if r.URL.Query().Get("startDateTime") != "2026-10-20T09:59:59Z" || r.URL.Query().Get("endDateTime") != "2026-10-20T11:00:01Z" {
				t.Fatal("instance membership was not bounded to its actual UTC endpoints")
			}
		}
		return f.RoundTrip(r)
	})
	body := strings.Replace(graphTestBody("series"), `"recurrence":null`, `"recurrence":{"pattern":{"type":"daily","interval":1},"range":{"type":"numbered","startDate":"2026-10-20","numberOfOccurrences":2}}`, 1)
	graphTestDo(t, g, "POST", GraphBase+"/me/calendars/test-calendar/events", body, true)
	graphTestDo(t, g, "PATCH", GraphBase+"/me/events/event-1", `{"isAllDay":false}`, true)
	instance := map[string]any{}
	for k, v := range f.events["event-1"] {
		instance[k] = v
	}
	instance["id"], instance["type"], instance["seriesMasterId"] = "owned-instance", "occurrence", "event-1"
	f.events["owned-instance"], f.calendars["owned-instance"] = instance, "test-calendar"
	f.values = []map[string]any{instance}
	graphTestDo(t, g, "GET", GraphBase+"/me/events/event-1/instances", "", true)
	graphTestDo(t, g, "PATCH", GraphBase+"/me/events/owned-instance", `{"isAllDay":false}`, true)
	before := f.writes
	f.calendars["owned-instance"] = "other-calendar"
	graphTestDo(t, g, "DELETE", GraphBase+"/me/events/owned-instance", "", false)
	f.calendars["event-1"] = "other-calendar"
	graphTestDo(t, g, "DELETE", GraphBase+"/me/events/event-1", "", false)
	if f.writes != before {
		t.Fatal("wrong-calendar event authorized by a stale relationship or matching name")
	}
}

func TestGraphGuardExpandedMasterIsReadOnlyAndOwned(t *testing.T) {
	g, f := graphFixture(t)
	body := strings.Replace(graphTestBody("series"), `"recurrence":null`, `"recurrence":{"pattern":{"type":"daily","interval":1},"range":{"type":"numbered","startDate":"2026-10-20","numberOfOccurrences":2}}`, 1)
	graphTestDo(t, g, "POST", GraphBase+"/me/calendars/test-calendar/events", body, true)
	graphTestDo(t, g, "GET", GraphBase+"/me/events/event-1?$select=id,exceptionOccurrences,cancelledOccurrences&$expand=exceptionOccurrences", "", true)
	before := f.writes
	for _, path := range []string{
		"/me/events/unknown?$expand=exceptionOccurrences",
		"/me/events/event-1?$expand=attendees",
		"/me/events/event-1?$expand=exceptionOccurrences($expand=attendees)",
		"/me/events/event-1/instances?$expand=exceptionOccurrences",
		"/me/events/event-1/instances/exceptionOccurrences",
		"/me/calendars/test-calendar/events?$expand=exceptionOccurrences",
	} {
		graphTestDo(t, g, "GET", GraphBase+path, "", false)
	}
	for _, method := range []string{"POST", "PATCH", "DELETE"} {
		graphTestDo(t, g, method, GraphBase+"/me/events/event-1/exceptionOccurrences", "", false)
	}
	graphTestDo(t, g, "POST", GraphBase+"/me/calendars/test-calendar/events", graphTestBody("single"), true)
	graphTestDo(t, g, "GET", GraphBase+"/me/events/event-2?$expand=exceptionOccurrences", "", false)
	if f.writes != before+1 {
		t.Fatal("read expansion authorized mutations")
	}
	// The only newly allowed continuation is GET of the owned master's bound
	// exception relationship. It does not enroll arbitrary response IDs.
	g.transport = graphTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1.0/me/events/event-1/exceptionOccurrences" {
			return graphTestResponse(r, 200, `{"value":[]}`), nil
		}
		return f.RoundTrip(r)
	})
	graphTestDo(t, g, "GET", GraphBase+"/me/events/event-1/exceptionOccurrences?$skiptoken=cursor", "", true)
}

func TestGraphRecorderExpandedTemporalFields(t *testing.T) {
	input := `{"id":"master","cancelledOccurrences":["OID.master.2026-10-28"],"exceptionOccurrences":[{"id":"exception","occurrenceId":"OID.master.2026-10-27","originalStart":"2026-10-27T08:00:00Z","start":{"dateTime":"2026-10-27T09:00:00","timeZone":"UTC"},"originalStartTimeZone":"Europe/Berlin","organizer":{"emailAddress":{"address":"private@example.test"}},"attendees":[{"emailAddress":{"address":"private@example.test"}}],"body":{"content":"private-body"},"access_token":"secret"}],"exceptionOccurrences@odata.nextLink":"https://graph.microsoft.com/v1.0/me/events/master/exceptionOccurrences?$skiptoken=private-cursor"}`
	clean, err := cleanCalendarJSON([]byte(input), "test-calendar")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private", "attendees", "organizer", "body", "access_token", "secret"} {
		if strings.Contains(string(clean), forbidden) {
			t.Errorf("retained %s", forbidden)
		}
	}
	for _, retained := range []string{"OID.master.2026-10-28", "OID.master.2026-10-27", "originalStart", "Europe/Berlin", "exceptionOccurrences@odata.nextLink", "redacted-cursor-"} {
		if !strings.Contains(string(clean), retained) {
			t.Errorf("lost %s", retained)
		}
	}
	q, ok := cleanGraphQuery(map[string][]string{"$expand": {"exceptionOccurrences"}, "$select": {"id,originalStart,cancelledOccurrences,exceptionOccurrences"}})
	if !ok || q.Get("$expand") != "exceptionOccurrences" {
		t.Fatal("lost safe expansion query")
	}
	if _, ok := cleanGraphQuery(map[string][]string{"$expand": {"exceptionOccurrences($expand=attendees)"}}); ok {
		t.Fatal("allowed recursive arbitrary expansion")
	}
}
