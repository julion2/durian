package calendartest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGoogleGuardIsolatesWrites(t *testing.T) {
	const cal = "dedicated@group.calendar.google.com"
	const prefix = "Durian Test unique-run-123456 "
	source := &TokenSource{accessToken: "fake-token", expires: time.Now().Add(time.Hour)}
	g, err := NewGoogleGuard(cal, prefix, source)
	if err != nil {
		t.Fatal(err)
	}
	forwarded := 0
	g.transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		forwarded++
		if r.Header.Get("Authorization") != "Bearer fake-token" {
			t.Error("missing guarded auth")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	base := GoogleBase + "/calendars/" + cal + "/events"
	sum := sha256.Sum256([]byte(prefix + "single"))
	id := hex.EncodeToString(sum[:])
	body := `{"id":"` + id + `","summary":"` + prefix + `single","start":{"dateTime":"2026-03-28T10:00:00+01:00"}}`
	request := func(method, url, body string, allow bool) {
		t.Helper()
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, url, reader)
		before := forwarded
		resp, err := g.HTTPClient().Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		if allow && (err != nil || forwarded != before+1) {
			t.Fatal("valid scoped request rejected")
		}
		if !allow && (err == nil || forwarded != before) {
			t.Fatal("unsafe request reached transport")
		}
	}
	request("POST", base+"?sendUpdates=none", body, true)
	request("PATCH", base+"/"+id+"?sendUpdates=none", `{"summary":"`+prefix+`edit"}`, true)
	for _, metadata := range []string{
		`{"private":{"durian.fold.v1.startZone":"Europe/Berlin","durian.fold.v1.endZone":"America/New_York"}}`,
		`{"private":{"durian.fold.v1.startZone":null,"durian.fold.v1.endZone":null}}`,
	} {
		patch := `{"summary":"` + prefix + `edit","extendedProperties":` + metadata + `}`
		request("PATCH", base+"/"+id+"?sendUpdates=none", patch, true)
		create := strings.TrimSuffix(body, "}") + `,"extendedProperties":` + metadata + `}`
		request("POST", base+"?sendUpdates=none", create, !strings.Contains(metadata, "null"))
	}
	for _, metadata := range []string{
		`null`, `{}`, `{"private":null}`, `{"shared":{}}`,
		`{"private":{"durian.fold.v1.startZone":"Europe/Berlin"}}`,
		`{"private":{"durian.fold.v1.startZone":null,"durian.fold.v1.endZone":"Europe/Berlin"}}`,
		`{"private":{"durian.fold.v1.startZone":"","durian.fold.v1.endZone":"Europe/Berlin"}}`,
		`{"private":{"durian.fold.v1.startZone":"Local","durian.fold.v1.endZone":"Europe/Berlin"}}`,
		`{"private":{"durian.fold.v1.startZone":"Invalid/Zone","durian.fold.v1.endZone":"Europe/Berlin"}}`,
		`{"private":{"durian.fold.v1.startZone":{},"durian.fold.v1.endZone":"Europe/Berlin"}}`,
		`{"private":{"durian.fold.v1.startZone":"Europe/Berlin","durian.fold.v1.endZone":"Europe/Berlin","extra":"forbidden"}}`,
		`{"private":{"durian.fold.v1.startZone":"Europe/Berlin","durian.fold.v1.endZone":"Europe/Berlin"},"shared":{}}`,
	} {
		patch := `{"summary":"` + prefix + `edit","extendedProperties":` + metadata + `}`
		request("PATCH", base+"/"+id+"?sendUpdates=none", patch, false)
		create := strings.TrimSuffix(body, "}") + `,"extendedProperties":` + metadata + `}`
		request("POST", base+"?sendUpdates=none", create, false)
	}
	request("DELETE", base+"/"+id+"_20260329T090000Z?sendUpdates=none", "", true)
	for _, tc := range []struct{ name, method, url, body string }{
		{"primary", "POST", strings.Replace(base, cal, "primary", 1) + "?sendUpdates=none", body},
		{"other-calendar", "POST", strings.Replace(base, cal, "other@group.calendar.google.com", 1) + "?sendUpdates=none", body},
		{"other-host", "POST", strings.Replace(base, "www.googleapis.com", "example.com", 1) + "?sendUpdates=none", body},
		{"http", "POST", strings.Replace(base, "https:", "http:", 1) + "?sendUpdates=none", body},
		{"attendees", "POST", base + "?sendUpdates=none", strings.Replace(body, `"id":`, `"attendees":[{"email":"other@example.test"}],"id":`, 1)},
		{"conference", "POST", base + "?sendUpdates=none", strings.Replace(body, `"id":`, `"conferenceData":{},"id":`, 1)},
		{"notify", "POST", base + "?sendUpdates=all", body},
		{"duplicate-notify", "POST", base + "?sendUpdates=none&sendUpdates=all", body},
		{"malformed-query", "POST", base + "?sendUpdates=none&bad=%zz", body},
		{"missing-policy", "POST", base, body},
		{"unowned-delete", "DELETE", base + "/abcde9999?sendUpdates=none", ""},
		{"lookalike-instance", "DELETE", base + "/" + id + "_else?sendUpdates=none", ""},
		{"unrelated-create-id", "POST", base + "?sendUpdates=none", strings.Replace(body, id, "abcde9999", 1)},
		{"wrong-prefix", "POST", base + "?sendUpdates=none", strings.Replace(body, prefix, "unrelated ", 1)},
		{"calendar-delete", "DELETE", strings.TrimSuffix(base, "/events") + "?sendUpdates=none", ""},
		{"move", "POST", base + "/" + id + "/move?sendUpdates=none", body},
	} {
		t.Run(tc.name, func(t *testing.T) { request(tc.method, tc.url, tc.body, false) })
	}
	// A rejected create must not authorize a later delete of that ID.
	request("DELETE", base+"/abcde9999?sendUpdates=none", "", false)
	methodOverride, _ := http.NewRequest("GET", base, nil)
	methodOverride.Header.Set("X-HTTP-Method-Override", "DELETE")
	if _, err := g.HTTPClient().Do(methodOverride); err == nil {
		t.Fatal("method override accepted")
	}
	// HTTP clients must not follow even a same-host redirect after validation.
	g.transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 307, Header: http.Header{"Location": {GoogleBase + "/calendars/primary/events"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})
	req, _ := http.NewRequest("GET", base, nil)
	if resp, err := g.HTTPClient().Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("redirect accepted")
	}
}

func TestLiveOAuthOptInAndMemoryRefresh(t *testing.T) {
	t.Setenv("DURIAN_CALENDAR_LIVE_TEST", "")
	if _, err := NewTokenSource(Google); err == nil {
		t.Fatal("live source without opt-in")
	}
	t.Setenv("DURIAN_CALENDAR_LIVE_TEST", "1")
	t.Setenv("DURIAN_MS_TEST_CLIENT_ID", "")
	t.Setenv("DURIAN_MS_TEST_REFRESH_TOKEN", "")
	if _, err := NewTokenSource(Microsoft); err == nil {
		t.Fatal("Graph accepted missing test-only credentials")
	}
	s := &TokenSource{clientID: "fake-id", clientSecret: "fake-secret", refreshToken: "original"}
	calls := 0
	s.client = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://oauth2.googleapis.com/token" {
			t.Fatal("untrusted token endpoint")
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		want := "original"
		if calls == 2 {
			want = "rotated"
		}
		if r.Form.Get("refresh_token") != want {
			t.Fatal("refresh rotation not retained in memory")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"access","refresh_token":"rotated","expires_in":3600}`))}, nil
	})}
	for i := 0; i < 2; i++ {
		if _, err := s.Token(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatal("cached token not reused")
	}
	s.expires = time.Now()
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("expired token not refreshed")
	}
}

func TestRecordingRemovesIdentityAndUnknownFields(t *testing.T) {
	clean, err := cleanJSON([]byte(`{"id":"abc123","iCalUID":"abc123@google.com","summary":"Durian Test bob@example.test","creator":{"email":"private@example.test"},"organizer":{"displayName":"Private Name"},"access_token":"secret","newProviderField":"private","start":{"dateTime":"2026-03-29T09:00:00+02:00","timeZone":"Europe/Berlin"},"nextSyncToken":"opaque-cursor"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"@", "Private", "private", "secret", "opaque-cursor", "creator", "organizer", "access_token"} {
		if strings.Contains(string(clean), forbidden) {
			t.Errorf("recording retained %q", forbidden)
		}
	}
	for _, retained := range []string{"abc123", "2026-03-29T09:00:00+02:00", "Europe/Berlin"} {
		if !strings.Contains(string(clean), retained) {
			t.Errorf("recording lost temporal contract %q", retained)
		}
	}
}

func TestRecordingFoldMetadataRoundTrip(t *testing.T) {
	for _, pair := range []string{
		`"durian.fold.v1.startZone":"Europe/Berlin","durian.fold.v1.endZone":"America/New_York"`,
		`"durian.fold.v1.startZone":null,"durian.fold.v1.endZone":null`,
	} {
		input := `{"extendedProperties":{"private":{` + pair + `,"unknown":"secret"},"shared":{"private":"secret"}}}`
		want := []byte(`{"extendedProperties":{"private":{` + pair + `}}}`)
		r := &Recorder{CalendarID: "test-calendar", Next: transportFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(input))}, nil
		})}
		req, _ := http.NewRequest("PATCH", GoogleBase+"/calendars/test-calendar/events/abcde?sendUpdates=none", strings.NewReader(input))
		resp, err := r.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		path := filepath.Join(t.TempDir(), "recording.json")
		if err := r.Save(path); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var replay Replay
		if err := json.Unmarshal(data, &replay.Recording); err != nil {
			t.Fatal(err)
		}
		e := replay.Recording.Exchanges[0]
		var request, response, expected any
		json.Unmarshal(e.Request, &request)
		json.Unmarshal(e.Response, &response)
		json.Unmarshal(want, &expected)
		if !reflect.DeepEqual(request, expected) || !reflect.DeepEqual(response, expected) {
			t.Fatal("recording did not retain exactly the fold metadata pair")
		}
		req, _ = http.NewRequest("PATCH", GoogleBase+"/calendars/test-calendar/events/abcde?sendUpdates=none", strings.NewReader(input))
		resp, err = replay.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if replay.Index != 1 {
			t.Fatal("replay did not consume exchange")
		}
	}
}

func TestRecordingGraphTemporalFieldsAndSafePaging(t *testing.T) {
	input := `{"value":[{"id":"event-1","subject":"Durian Test single","iCalUId":"uid@example.test","@odata.etag":"version","changeKey":"version","type":"seriesMaster","seriesMasterId":"master-1","isAllDay":false,"start":{"dateTime":"2026-03-22T01:30:00","timeZone":"UTC"},"end":{"dateTime":"2026-03-22T02:15:00","timeZone":"UTC"},"originalStartTimeZone":"W. Europe Standard Time","recurrence":{"pattern":{"type":"weekly","interval":1,"daysOfWeek":["sunday"],"firstDayOfWeek":"monday","organizer":{"displayName":"Private Person"}},"range":{"type":"numbered","startDate":"2026-03-22","numberOfOccurrences":3,"recurrenceTimeZone":"W. Europe Standard Time","access_token":"secret"}},"location":{"displayName":"test room","locationEmailAddress":"private@example.test"},"organizer":{"emailAddress":{"name":"Private Person","address":"private@example.test"}},"attendees":[],"access_token":"secret"}],"@odata.nextLink":"https://graph.microsoft.com/v1.0/me/calendars/pinned-calendar/calendarView?$skiptoken=opaque-cursor&startDateTime=2026-01-01T00%3A00%3A00Z"}`
	clean, err := cleanCalendarJSON([]byte(input), "pinned-calendar")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"@example", "private@example", "Private Person", "secret", "access_token", "organizer", "attendees", "locationEmailAddress", "opaque-cursor", "pinned-calendar"} {
		if strings.Contains(string(clean), forbidden) {
			t.Errorf("Graph recording retained %q", forbidden)
		}
	}
	for _, retained := range []string{"weekly", "numbered", "sunday", "2026-03-22", "numberOfOccurrences", "W. Europe Standard Time", "test room", "test-calendar/calendarView", "redacted-cursor-"} {
		if !strings.Contains(string(clean), retained) {
			t.Errorf("Graph recording lost %q", retained)
		}
	}
	var page struct {
		Next string `json:"@odata.nextLink"`
	}
	json.Unmarshal(clean, &page)
	u, err := url.Parse(page.Next)
	if err != nil || u.Host != "graph.microsoft.com" || u.Path != "/v1.0/me/calendars/test-calendar/calendarView" {
		t.Fatal("unsafe or unusable normalized nextLink")
	}
	// The cursor in the follow-up request must match the response placeholder.
	req, _ := http.NewRequest("GET", strings.Replace(page.Next, "test-calendar", "pinned-calendar", 1), nil)
	e, err := requestRecord(req, "pinned-calendar")
	if err != nil || e.Query.Get("$skiptoken") != u.Query().Get("$skiptoken") {
		t.Fatal("Graph paging cursor is not replay-stable")
	}
	for _, link := range []string{
		"https://example.test/stolen?access_token=secret", GraphBase + "/me/calendars/other/calendarView?$skiptoken=secret",
		GraphBase + "/users/private@example.test/events", GraphBase + "/me/calendars/pinned-calendar/calendarView?access_token=secret",
		GraphBase + "/me/calendars/pinned-calendar/calendarView?$skiptoken=a&$skiptoken=b",
		GraphBase + "/me/calendars/pinned-calendar/calendarView?startDateTime=secret",
		GraphBase + "/me/events/event-1/instances#secret",
	} {
		if cleanGraphNextLink(link, "pinned-calendar") != "" {
			t.Error("unsafe Graph nextLink retained")
		}
	}
}
