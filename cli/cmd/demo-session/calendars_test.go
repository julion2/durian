package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
	"github.com/julion2/durian/cli/internal/calendarsync"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/handler"
)

const googleCalendarID = "offline@group.calendar.google.com"
const graphCalendarID = "offline-calendar"

// No real request, token refresh or persistence CLI can escape this fixture.
// The real providers and guards sit ABOVE this transport, not fake providers.
type calendarTransport struct {
	t         *testing.T
	events    map[bool]map[string]map[string]any
	refreshes map[string]int
	writes    int
}

func (f *calendarTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Helper()
	reply := func(status int, value any) (*http.Response, error) {
		data, _ := json.Marshal(value)
		return &http.Response{Request: r, StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
	}
	if r.URL.Host == "oauth2.googleapis.com" || r.URL.Host == "login.microsoftonline.com" {
		f.refreshes[r.URL.Host]++
		// No refresh_token in response: a fake test must never invoke durable
		// secret persistence. Rotation durability has separate existing tests.
		return reply(200, map[string]any{"access_token": "offline-access", "expires_in": 3600})
	}
	if r.Header.Get("Authorization") != "Bearer offline-access" {
		return nil, errors.New("missing guarded credential")
	}
	if strings.Contains(r.URL.Path, "userinfo") {
		email := googleEmail
		if r.URL.Host == "graph.microsoft.com" {
			email = msEmail
		}
		return reply(200, map[string]any{"email": email, "sub": "offline", "email_verified": true})
	}
	google := r.URL.Host == "www.googleapis.com"
	base := "/v1.0/me/calendars/" + graphCalendarID
	if google {
		base = "/calendar/v3/calendars/" + googleCalendarID
	}
	if r.Method == "GET" && google && r.URL.Path == "/calendar/v3/users/me/calendarList/"+googleCalendarID {
		return reply(200, map[string]any{"id": googleCalendarID, "summary": "Durian Test", "description": "Durian isolated calendar integration tests. No attendees.", "accessRole": "owner", "primary": false})
	}
	if r.Method == "GET" && !google && r.URL.Path == base {
		return reply(200, map[string]any{"id": graphCalendarID, "name": "Durian Test", "isDefaultCalendar": false, "canEdit": true, "canShare": true, "owner": map[string]string{"address": msEmail}})
	}
	if r.URL.Host != "www.googleapis.com" && r.URL.Host != "graph.microsoft.com" {
		return nil, errors.New("unexpected host")
	}
	if r.Method == "GET" && (r.URL.Path == base+"/events" || r.URL.Path == base+"/calendarView") {
		values := []any{}
		for _, ev := range f.events[google] {
			if !google && strings.HasSuffix(r.URL.Path, "/calendarView") && ev["type"] == "seriesMaster" {
				instance := make(map[string]any)
				for k, v := range ev {
					instance[k] = v
				}
				instance["id"], instance["seriesMasterId"], instance["type"], instance["recurrence"] = ev["id"].(string)+"-instance", ev["id"], "occurrence", nil
				values = append(values, instance)
				continue
			}
			values = append(values, ev)
		}
		key := "value"
		if google {
			key = "items"
		}
		return reply(200, map[string]any{key: values})
	}
	if r.Method == "POST" && r.URL.Path == base+"/events" {
		var ev map[string]any
		if json.NewDecoder(r.Body).Decode(&ev) != nil {
			return nil, errors.New("bad create body")
		}
		id, _ := ev["id"].(string)
		if google {
			if r.URL.Query().Get("sendUpdates") != "none" {
				f.t.Fatal("notifying Google create")
			}
			ev["iCalUID"], ev["etag"], ev["status"] = "google-"+id, "etag-1", "confirmed"
		} else {
			id = fmt.Sprintf("graph-%d", len(f.events[false])+1)
			ev["id"], ev["iCalUId"], ev["changeKey"], ev["@odata.etag"] = id, "uid-"+id, "etag-1", "etag-1"
			ev["type"], ev["isOrganizer"], ev["attendees"] = "singleInstance", true, []any{}
			if ev["isOnlineMeeting"] != false || ev["isReminderOn"] != false {
				f.t.Fatal("unsafe Graph defaults")
			}
		}
		if _, ok := ev["attendees"]; google && ok {
			f.t.Fatal("Google attendee field")
		}
		f.events[google][id] = ev
		f.writes++
		return reply(201, ev)
	}
	global := "/v1.0/me/events/"
	if google {
		global = base + "/events/"
	}
	if strings.HasPrefix(r.URL.Path, global) {
		id := strings.TrimPrefix(r.URL.Path, global)
		if strings.HasSuffix(id, "/calendar") {
			if _, ok := f.events[false][strings.TrimSuffix(id, "/calendar")]; ok {
				return reply(200, map[string]string{"id": graphCalendarID})
			}
		}
		ev, ok := f.events[google][id]
		if !ok {
			return reply(404, map[string]string{"error": "not found"})
		}
		switch r.Method {
		case "GET":
			return reply(200, ev)
		case "PATCH":
			var patch map[string]any
			if json.NewDecoder(r.Body).Decode(&patch) != nil {
				return nil, errors.New("bad patch")
			}
			for k, v := range patch {
				ev[k] = v
			}
			f.writes++
			return reply(200, ev)
		case "DELETE":
			delete(f.events[google], id)
			f.writes++
			return reply(204, nil)
		}
	}
	f.t.Errorf("unexpected offline calendar request: %s %s", r.Method, r.URL.Path)
	return nil, errors.New("unexpected request")
}

func calendarFixture(t *testing.T) *calendarTransport {
	t.Helper()
	for k, v := range map[string]string{
		"DURIAN_MAIL_LIVE_TEST": "1", "DURIAN_CALENDAR_LIVE_TEST": "1",
		"DURIAN_GOOGLE_TEST_EMAIL": googleEmail, "DURIAN_MS_TEST_EMAIL": msEmail,
		"DURIAN_GOOGLE_TEST_CLIENT_ID": "fake", "DURIAN_GOOGLE_TEST_CLIENT_SECRET": "fake",
		"DURIAN_GOOGLE_TEST_REFRESH_TOKEN": "fake", "DURIAN_MS_TEST_CLIENT_ID": "fake",
		"DURIAN_MS_TEST_REFRESH_TOKEN": "fake", "DURIAN_MS_TEST_TENANT": "consumers",
		"DURIAN_GOOGLE_TEST_CALENDAR_ID": googleCalendarID, "DURIAN_MS_TEST_CALENDAR_ID": graphCalendarID,
	} {
		t.Setenv(k, v)
	}
	f := &calendarTransport{t: t, events: map[bool]map[string]map[string]any{true: {}, false: {}}, refreshes: map[string]int{}}
	old := http.DefaultTransport
	http.DefaultTransport = f
	t.Cleanup(func() { http.DefaultTransport = old })
	return f
}

func TestOfflineCalendarProductionRouterAndRunner(t *testing.T) {
	f := calendarFixture(t)
	_, s, err := newSessionFromEnv(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	cfg := sessionConfig(t.TempDir(), true)
	h := handler.New(nil, nil)
	h.SetConfig(cfg)
	h.SetCalendarEventSyncer(handler.NewCalendarEventSyncer(cfg, s.providerFor))
	hub := handler.NewEventHub()
	router := handler.NewRouter(h, hub, "offline", "offline")
	router.Use(s.policy(cfg))
	request := func(method, path string, value any, want int) []byte {
		t.Helper()
		data, _ := json.Marshal(value)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(string(data))))
		if w.Code != want {
			t.Fatalf("%s %s: status %d want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		return w.Body.Bytes()
	}
	for i := range cfg.Accounts {
		a := &cfg.Accounts[i]
		if err := s.download(context.Background(), cfg, a, hub); err != nil {
			t.Fatal(err)
		}
		body := request("GET", "/api/v1/calendars?account="+a.Alias, nil, 200)
		if !strings.Contains(string(body), "Durian Test") {
			t.Fatal("pinned calendar unavailable")
		}
		put := map[string]any{"account": a.Alias, "calendar": "Durian Test", "uid": "local-" + a.Alias, "subject": s.prefix + a.Alias,
			"start": "2026-10-20T10:00:00Z", "end": "2026-10-20T11:00:00Z", "attendees": []string{}, "replace_attendees": true}
		request("PUT", "/api/v1/calendars/event", put, 200)
		before := f.writes
		if err := s.download(context.Background(), cfg, a, hub); err != nil {
			t.Fatal(err)
		}
		if f.writes != before {
			t.Fatal("periodic download uploaded a pending GUI save")
		}
		sync := map[string]any{"account": a.Alias, "calendar": "Durian Test", "uid": put["uid"], "operation": "save"}
		dir := filepath.Join(config.CalendarBaseDir(cfg, ""), a.CalendarDir())
		release, ok, err := calendarsync.AcquireRunLock(dir)
		if err != nil || !ok {
			t.Fatal("lock")
		}
		request("POST", "/api/v1/calendars/sync/event", sync, 409)
		release()
		body = request("POST", "/api/v1/calendars/sync/event", sync, 200)
		if f.writes != before+1 || !strings.Contains(string(body), `"applied":true`) {
			t.Fatal("explicit save did not create once")
		}
		// Production create may replace the local UID with the provider UID.
		// Discover the settled identity through the public native read route.
		body = request("GET", "/api/v1/calendars/events?account="+a.Alias+"&q="+url.QueryEscape(s.prefix), nil, 200)
		var listing struct {
			Events []calendar.CalendarEvent `json:"events"`
		}
		if json.Unmarshal(body, &listing) != nil || len(listing.Events) != 1 {
			t.Fatal("created event not readable through native API")
		}
		uid := listing.Events[0].UID
		put["uid"], put["subject"] = uid, s.prefix+a.Alias+" edited"
		request("PUT", "/api/v1/calendars/event", put, 200)
		if err := s.download(context.Background(), cfg, a, hub); err != nil {
			t.Fatal(err)
		}
		if f.writes != before+1 {
			t.Fatal("download uploaded pending update")
		}
		sync["uid"] = uid
		request("POST", "/api/v1/calendars/sync/event", sync, 200)
		if f.writes != before+2 {
			t.Fatal("explicit update missing")
		}
		for key, value := range map[string]any{"attendees": []string{"stranger@example.test"}, "request_online_meeting": true, "calendar": "Primary", "subject": "unprefixed"} {
			old := put[key]
			put[key] = value
			request("PUT", "/api/v1/calendars/event", put, 400)
			if old == nil {
				delete(put, key)
			} else {
				put[key] = old
			}
		}
		request("POST", "/api/v1/calendars/rsvp", sync, 400)
		request("DELETE", "/api/v1/calendars/event?account="+a.Alias+"&calendar=Durian%20Test&uid="+url.QueryEscape(uid), nil, 200)
		if err := s.download(context.Background(), cfg, a, hub); err != nil {
			t.Fatal(err)
		}
		if f.writes != before+2 {
			t.Fatal("download uploaded pending delete")
		}
		sync["operation"] = "delete"
		request("POST", "/api/v1/calendars/sync/event", sync, 200)
		if f.writes != before+3 {
			t.Fatal("explicit delete missing")
		}
		p, _ := s.providerFor(a)
		ev := calendar.Event{Subject: s.prefix + "direct", Start: time.Now(), End: time.Now().Add(time.Hour)}
		if _, err := p.CreateEvent(context.Background(), "primary", ev, calendarsync.CreateOptions{}); err == nil {
			t.Fatal("unscoped create accepted")
		}
		ev.Attendees = []calendar.Attendee{{Email: googleEmail}}
		id := googleCalendarID
		if i == 1 {
			id = graphCalendarID
		}
		if _, err := p.CreateEvent(context.Background(), id, ev, calendarsync.CreateOptions{}); err == nil {
			t.Fatal("attendee create accepted")
		}
		// Existing scoped events (including a Graph series that requires a
		// mailbox-global expanded-master read) download without gaining write
		// ownership. Native actions must fail clearly, not silently edit them.
		oldID, oldUID := "abcde", "previous-uid"
		old := map[string]any{"id": oldID, "iCalUID": oldUID, "summary": "Durian Test previous run", "status": "confirmed", "etag": "old",
			"start": map[string]string{"dateTime": "2026-10-20T10:00:00Z", "timeZone": "UTC"}, "end": map[string]string{"dateTime": "2026-10-20T11:00:00Z", "timeZone": "UTC"}}
		if i == 1 {
			oldID = "previous-series"
			old = map[string]any{"id": oldID, "iCalUId": oldUID, "subject": "Durian Test previous run", "type": "seriesMaster", "changeKey": "old", "@odata.etag": "old",
				"attendees": []any{}, "isOrganizer": true, "isOnlineMeeting": false, "isReminderOn": false,
				"start": map[string]string{"dateTime": "2026-10-20T10:00:00", "timeZone": "UTC"}, "end": map[string]string{"dateTime": "2026-10-20T11:00:00", "timeZone": "UTC"},
				"recurrence":           map[string]any{"pattern": map[string]any{"type": "daily", "interval": 1}, "range": map[string]any{"type": "numbered", "startDate": "2026-10-20", "numberOfOccurrences": 3, "recurrenceTimeZone": "UTC"}},
				"exceptionOccurrences": []any{}, "cancelledOccurrences": []any{}}
		}
		f.events[i == 0][oldID] = old
		if err := s.download(context.Background(), cfg, a, hub); err != nil {
			t.Fatal(err)
		}
		body = request("GET", "/api/v1/calendars/events?account="+a.Alias+"&q=previous", nil, 200)
		if !strings.Contains(string(body), oldUID) {
			t.Fatal("existing scoped event missing from native reads")
		}
		put["uid"] = oldUID
		request("PUT", "/api/v1/calendars/event", put, 400)
		request("DELETE", "/api/v1/calendars/event?account="+a.Alias+"&calendar=Durian%20Test&uid="+oldUID, nil, 400)
		if err := p.DeleteEvent(context.Background(), id, oldID, "old", false); err == nil {
			t.Fatal("read enrolled delete ownership")
		}
		ev.Attendees = nil
		if err := p.UpdateEvent(context.Background(), id, oldID, calendarsync.UpdateSpec{Event: ev}); err == nil {
			t.Fatal("read enrolled update ownership")
		}
		if f.writes != before+3 {
			t.Fatal("existing event mutated")
		}
		// An empty replacement must also fail if a stored attendee exists,
		// even on an event carrying this run's prefix. No silent removal.
		name := "subject"
		if i == 0 {
			name = "summary"
		}
		old[name] = s.prefix + "with guests"
		guest := map[string]any{"email": googleEmail}
		if i == 1 {
			guest = map[string]any{"type": "required", "emailAddress": map[string]string{"address": googleEmail}}
		}
		old["attendees"] = []any{guest}
		if err := s.download(context.Background(), cfg, a, hub); err != nil {
			t.Fatal(err)
		}
		request("PUT", "/api/v1/calendars/event", put, 400)
		if f.writes != before+3 {
			t.Fatal("empty replacement removed stored attendees")
		}
	}
	for _, count := range f.refreshes {
		if count != 1 {
			t.Fatal("token sources not shared")
		}
	}
}

func TestCalendarSetupRequiresOptInAndExactPins(t *testing.T) {
	f := calendarFixture(t)
	t.Setenv("DURIAN_CALENDAR_LIVE_TEST", "")
	if _, _, err := newSessionFromEnv(context.Background(), true); err == nil || len(f.refreshes) != 0 {
		t.Fatal("calendar opt-in bypass")
	}
	t.Setenv("DURIAN_CALENDAR_LIVE_TEST", "1")
	t.Setenv("DURIAN_GOOGLE_TEST_CALENDAR_ID", "")
	if _, _, err := newSessionFromEnv(context.Background(), true); err == nil || len(f.refreshes) != 0 {
		t.Fatal("missing pin reached credentials")
	}
	// Mail-only remains available without calendar opt-in or IDs.
	t.Setenv("DURIAN_CALENDAR_LIVE_TEST", "")
	_, c, err := newSessionFromEnv(context.Background(), false)
	if err != nil || c != nil {
		t.Fatal("mail-only default changed", err)
	}
}
