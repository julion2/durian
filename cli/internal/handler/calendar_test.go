package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
	"github.com/julion2/durian/cli/internal/calendarsync"
	"github.com/julion2/durian/cli/internal/config"
)

type fakeCalendarEventSyncer struct {
	account   string
	calendar  string
	uid       string
	operation string
	err       error
	applied   bool
}

func (f *fakeCalendarEventSyncer) SyncCalendarEvent(_ context.Context, account, calendar, uid, operation string) (bool, error) {
	f.account, f.calendar, f.uid, f.operation = account, calendar, uid, operation
	return f.applied, f.err
}

// newCalendarHandler builds a Handler whose config points at a temp vdir seeded
// with two events in one "Calendar" of account alias "work" (owner
// me@example.com), returning the router and the calendar directory for tests
// that seed additional events.
func newCalendarHandler(t *testing.T) (http.Handler, string) {
	t.Helper()
	base := t.TempDir()
	calDir := filepath.Join(base, "work", "Calendar")
	if err := os.MkdirAll(calDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(calDir, "displayname"), []byte("Calendar\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	excludedDir := filepath.Join(base, "work", "Excluded")
	if err := os.MkdirAll(excludedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(excludedDir, "displayname"), []byte("Excluded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeCalendarTestEvent(t, calDir, calendar.Event{
		ICalUID: "evt-lunch", Subject: "Team Lunch",
		Start: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC),
	})
	writeCalendarTestEvent(t, calDir, calendar.Event{
		ICalUID: "evt-review", Subject: "Design Review",
		Start: time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC),
	})

	cfg := &config.Config{
		Calendar: config.CalendarConfig{VdirPath: base},
		Accounts: []config.AccountConfig{{
			Name: "Work", Email: "me@example.com", Alias: "work",
			Calendar: &config.AccountCalendarConfig{Include: []string{"Calendar"}},
		}},
	}
	h := New(nil, nil)
	h.SetConfig(cfg)
	return newTestRouter(h, nil), calDir
}

// writeCalendarTestEvent serializes an event into calDir under its UID name.
func writeCalendarTestEvent(t *testing.T, calDir string, ev calendar.Event) {
	t.Helper()
	data, err := calendar.EventToICal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(calDir, ev.ICalUID+".ics"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func getJSON(t *testing.T, r http.Handler, url string, out any) int {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", url, nil))
	if out != nil && w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("decode %s: %v (body: %s)", url, err, w.Body.String())
		}
	}
	return w.Code
}

func TestCalendarsHandler(t *testing.T) {
	r, _ := newCalendarHandler(t)
	var resp struct {
		OK        bool                   `json:"ok"`
		Calendars []calendar.CalendarDTO `json:"calendars"`
	}
	if code := getJSON(t, r, "/api/v1/calendars?account=work", &resp); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if !resp.OK || len(resp.Calendars) != 1 {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Calendars[0].Name != "Calendar" || resp.Calendars[0].EventCount != 2 {
		t.Errorf("calendar = %+v, want Calendar/2", resp.Calendars[0])
	}
}

func TestCalendarsHandlerRejectsDisabledAccount(t *testing.T) {
	disabled := false
	h := New(nil, nil)
	h.SetConfig(&config.Config{Accounts: []config.AccountConfig{{
		Name: "Work", Email: "me@example.com", Alias: "work",
		Calendar: &config.AccountCalendarConfig{Enabled: &disabled},
	}}})
	r := newTestRouter(h, nil)

	if code := getJSON(t, r, "/api/v1/calendars?account=work", nil); code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", code, http.StatusConflict)
	}
}

func TestCalendarsHandlerRejectsLegacyDelegatedMailboxVdir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "shared")
	state := &calendarsync.SyncState{
		Calendars: map[string]calendarsync.CalendarStatus{
			"owners-calendar": {Items: map[string]calendarsync.ItemStatus{}},
		},
	}
	if err := calendarsync.NewFileStateStore(dir).Save(state); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Calendar: config.CalendarConfig{VdirPath: base},
		Accounts: []config.AccountConfig{{
			Name: "Shared", Email: "shared@example.com", AuthEmail: "owner@example.com", Alias: "shared",
		}},
	}
	h := New(nil, nil)
	h.SetConfig(cfg)
	r := newTestRouter(h, nil)

	if code := getJSON(t, r, "/api/v1/calendars?account=shared", nil); code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", code, http.StatusConflict)
	}
}

func TestCalendarEventsWindow(t *testing.T) {
	r, _ := newCalendarHandler(t)
	var resp struct {
		OK     bool                     `json:"ok"`
		Events []calendar.CalendarEvent `json:"events"`
	}
	getJSON(t, r, "/api/v1/calendars/events?account=work&from=2026-08-01&to=2026-08-10", &resp)
	if !resp.OK || len(resp.Events) != 2 {
		t.Fatalf("want 2 events, got %+v", resp)
	}
	// Sorted by start: lunch (Aug 1) before review (Aug 5).
	if resp.Events[0].Subject != "Team Lunch" || resp.Events[1].Subject != "Design Review" {
		t.Errorf("unsorted or wrong events: %+v", resp.Events)
	}
	// Summary events omit attendees/description (none here anyway).
	if resp.Events[0].UID != "evt-lunch" {
		t.Errorf("uid = %q", resp.Events[0].UID)
	}
	if resp.Events[0].Account != "work" {
		t.Errorf("account = %q, want work", resp.Events[0].Account)
	}
}

func TestCalendarEventsWindowExcludes(t *testing.T) {
	r, _ := newCalendarHandler(t)
	var resp struct {
		Events []calendar.CalendarEvent `json:"events"`
	}
	// Window before both events.
	getJSON(t, r, "/api/v1/calendars/events?account=work&from=2026-07-01&to=2026-07-31", &resp)
	if len(resp.Events) != 0 {
		t.Errorf("want 0 events out of window, got %d", len(resp.Events))
	}
}

func TestCalendarEventsSearch(t *testing.T) {
	r, _ := newCalendarHandler(t)
	var resp struct {
		Events []calendar.CalendarEvent `json:"events"`
	}
	getJSON(t, r, "/api/v1/calendars/events?account=work&q=review", &resp)
	if len(resp.Events) != 1 || resp.Events[0].Subject != "Design Review" {
		t.Errorf("search q=review = %+v", resp.Events)
	}
}

func TestCalendarEventDetail(t *testing.T) {
	r, _ := newCalendarHandler(t)
	var resp struct {
		OK    bool                   `json:"ok"`
		Event calendar.CalendarEvent `json:"event"`
	}
	if code := getJSON(t, r, "/api/v1/calendars/event?account=work&ref=lunch", &resp); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if !resp.OK || resp.Event.UID != "evt-lunch" || resp.Event.Subject != "Team Lunch" {
		t.Errorf("event = %+v", resp.Event)
	}
	if resp.Event.Account != "work" {
		t.Errorf("account = %q, want work", resp.Event.Account)
	}
}

func TestCalendarPutGetDeleteRoundTrip(t *testing.T) {
	r, _ := newCalendarHandler(t)

	body := `{"account":"work","calendar":"Calendar","subject":"New Event","start":"2026-08-03T09:00:00Z","end":"2026-08-03T10:00:00Z","location":"Room 1"}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v1/calendars/event", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status %d: %s", w.Code, w.Body.String())
	}
	var put struct {
		OK    bool                   `json:"ok"`
		Event calendar.CalendarEvent `json:"event"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &put); err != nil {
		t.Fatal(err)
	}
	if !put.OK || put.Event.UID == "" || put.Event.Subject != "New Event" {
		t.Fatalf("put response = %+v", put)
	}
	uid := put.Event.UID

	// Read it back.
	var got struct {
		Event calendar.CalendarEvent `json:"event"`
	}
	if code := getJSON(t, r, "/api/v1/calendars/event?account=work&ref="+uid, &got); code != http.StatusOK {
		t.Fatalf("GET after PUT status %d", code)
	}
	if got.Event.Subject != "New Event" || got.Event.Location != "Room 1" {
		t.Errorf("round-tripped event = %+v", got.Event)
	}

	// Delete it.
	wd := httptest.NewRecorder()
	r.ServeHTTP(wd, httptest.NewRequest("DELETE", "/api/v1/calendars/event?account=work&ref="+uid, nil))
	if wd.Code != http.StatusOK {
		t.Fatalf("DELETE status %d", wd.Code)
	}
	if code := getJSON(t, r, "/api/v1/calendars/event?account=work&ref="+uid, nil); code != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d, want 404", code)
	}
}

func TestCalendarSyncEventHandler(t *testing.T) {
	h := New(nil, nil)
	fake := &fakeCalendarEventSyncer{applied: true}
	h.SetCalendarEventSyncer(fake)
	r := newTestRouter(h, nil)

	w := httptest.NewRecorder()
	body := `{"account":"work","calendar":"Calendar","uid":"evt-lunch","operation":"save"}`
	r.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/calendars/sync/event", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("POST status %d: %s", w.Code, w.Body.String())
	}
	if fake.account != "work" || fake.calendar != "Calendar" || fake.uid != "evt-lunch" || fake.operation != "save" {
		t.Fatalf("sync args = %q, %q, %q, %q", fake.account, fake.calendar, fake.uid, fake.operation)
	}
	if !strings.Contains(w.Body.String(), `"applied":true`) {
		t.Fatalf("response = %s, want applied=true", w.Body.String())
	}
}

func TestCalendarSyncEventHandlerReportsAlreadyConverged(t *testing.T) {
	h := New(nil, nil)
	h.SetCalendarEventSyncer(&fakeCalendarEventSyncer{})
	r := newTestRouter(h, nil)
	w := httptest.NewRecorder()
	body := `{"account":"work","calendar":"Calendar","uid":"evt-lunch","operation":"save"}`
	r.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/calendars/sync/event", strings.NewReader(body)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"applied":false`) {
		t.Fatalf("response = %d %s, want applied=false", w.Code, w.Body.String())
	}
}

func TestCalendarSyncEventHandlerErrors(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		syncer *fakeCalendarEventSyncer
		want   int
	}{
		{name: "invalid", body: `{}`, syncer: &fakeCalendarEventSyncer{}, want: http.StatusBadRequest},
		{name: "operation", body: `{"account":"work","calendar":"Calendar","uid":"u","operation":"launch"}`, syncer: &fakeCalendarEventSyncer{}, want: http.StatusBadRequest},
		{name: "unavailable", body: `{"account":"work","calendar":"Calendar","uid":"u","operation":"save"}`, want: http.StatusServiceUnavailable},
		{name: "busy", body: `{"account":"work","calendar":"Calendar","uid":"u","operation":"save"}`, syncer: &fakeCalendarEventSyncer{err: ErrCalendarSyncBusy}, want: http.StatusConflict},
		{name: "not found", body: `{"account":"work","calendar":"Calendar","uid":"u","operation":"save"}`, syncer: &fakeCalendarEventSyncer{err: ErrCalendarSyncNotFound}, want: http.StatusNotFound},
		{name: "disabled", body: `{"account":"work","calendar":"Calendar","uid":"u","operation":"save"}`, syncer: &fakeCalendarEventSyncer{err: ErrCalendarSyncDisabled}, want: http.StatusConflict},
		{name: "conflict", body: `{"account":"work","calendar":"Calendar","uid":"u","operation":"save"}`, syncer: &fakeCalendarEventSyncer{err: ErrCalendarSyncConflict}, want: http.StatusConflict},
		{name: "provider", body: `{"account":"work","calendar":"Calendar","uid":"u","operation":"save"}`, syncer: &fakeCalendarEventSyncer{err: errors.New("provider failed")}, want: http.StatusBadGateway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := New(nil, nil)
			if tt.syncer != nil {
				h.SetCalendarEventSyncer(tt.syncer)
			}
			r := newTestRouter(h, nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/calendars/sync/event", strings.NewReader(tt.body)))
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.want, w.Body.String())
			}
		})
	}
}

func TestCalendarPutValidation(t *testing.T) {
	r, _ := newCalendarHandler(t)
	cases := []struct {
		body string
		want int
	}{
		{`{"account":"work","calendar":"Calendar","start":"nope","end":"2026-08-03T10:00:00Z"}`, http.StatusBadRequest},
		{`{"account":"work","subject":"x","start":"2026-08-03T09:00:00Z","end":"2026-08-03T10:00:00Z"}`, http.StatusBadRequest},
		{`{"account":"work","calendar":"Calendar","start":"2026-08-03T10:00:00Z","end":"2026-08-03T09:00:00Z"}`, http.StatusBadRequest},
		{`{"calendar":"Calendar","start":"2026-08-03T09:00:00Z","end":"2026-08-03T10:00:00Z"}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v1/calendars/event", strings.NewReader(c.body)))
		if w.Code != c.want {
			t.Errorf("PUT %q = %d, want %d", c.body, w.Code, c.want)
		}
	}
}

func TestCalendarPutAllDaySnap(t *testing.T) {
	// An all-day event shorter than a full day (Graph would reject it) is
	// normalized to midnight UTC day boundaries and snapped to one day.
	r, _ := newCalendarHandler(t)
	body := `{"account":"work","calendar":"Calendar","subject":"Holiday","all_day":true,"start":"2026-08-03T09:00:00Z","end":"2026-08-03T10:00:00Z"}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v1/calendars/event", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status %d: %s", w.Code, w.Body.String())
	}
	var put struct {
		Event calendar.CalendarEvent `json:"event"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &put); err != nil {
		t.Fatal(err)
	}
	if !put.Event.AllDay {
		t.Error("all_day not preserved")
	}
	wantStart := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	if !put.Event.Start.Equal(wantStart) || !put.Event.End.Equal(wantEnd) {
		t.Errorf("start/end = %v / %v, want %v / %v", put.Event.Start, put.Event.End, wantStart, wantEnd)
	}

	// The stored .ics agrees: reading it back yields the snapped day span.
	var got struct {
		Event calendar.CalendarEvent `json:"event"`
	}
	if code := getJSON(t, r, "/api/v1/calendars/event?account=work&ref="+put.Event.UID, &got); code != http.StatusOK {
		t.Fatalf("GET after PUT status %d", code)
	}
	if !got.Event.AllDay || !got.Event.Start.Equal(wantStart) || !got.Event.End.Equal(wantEnd) {
		t.Errorf("stored event = %+v, want all-day %v..%v", got.Event, wantStart, wantEnd)
	}
}

func TestCalendarPutUpdatePreservesMeetingFields(t *testing.T) {
	// A GUI edit only carries subject/time/location/description. The update
	// must merge over the existing event — organizer, attendees, recurrence
	// and the owner's RSVP survive — otherwise the next sync would push the
	// stripped event (e.g. an uninvite wave) to Graph.
	r, calDir := newCalendarHandler(t)
	writeCalendarTestEvent(t, calDir, calendar.Event{
		ICalUID: "evt-meeting", Subject: "Weekly Sync",
		Start: time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC),
		Attendees: []calendar.Attendee{
			{Name: "Me", Email: "me@example.com", Type: "required", Response: "accepted"},
			{Name: "Alice", Email: "alice@example.com", Type: "required", Response: "accepted"},
		},
		Organizer: &calendar.Person{Name: "Org", Email: "organizer@example.com"},
		Recurrence: &calendar.Recurrence{
			Pattern: calendar.RecurrencePattern{Type: "weekly", Interval: 1, DaysOfWeek: []string{"thursday"}},
			Range:   calendar.RecurrenceRange{Type: "noEnd", StartDate: "2026-08-06"},
		},
		IsCancelled: true,
	})

	body := `{"account":"work","calendar":"Calendar","uid":"evt-meeting","subject":"Weekly Sync (moved)","start":"2026-08-06T10:00:00Z","end":"2026-08-06T11:00:00Z"}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v1/calendars/event", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status %d: %s", w.Code, w.Body.String())
	}

	var got struct {
		Event calendar.CalendarEvent `json:"event"`
	}
	if code := getJSON(t, r, "/api/v1/calendars/event?account=work&ref=evt-meeting", &got); code != http.StatusOK {
		t.Fatalf("GET after PUT status %d", code)
	}
	e := got.Event
	if e.Subject != "Weekly Sync (moved)" {
		t.Errorf("subject = %q, want the edit applied", e.Subject)
	}
	if len(e.Attendees) != 2 {
		t.Fatalf("attendees = %+v, want the 2 preserved", e.Attendees)
	}
	if e.Organizer == nil || e.Organizer.Email != "organizer@example.com" {
		t.Errorf("organizer = %+v, want preserved", e.Organizer)
	}
	if !e.Recurring {
		t.Error("recurrence not preserved")
	}
	if e.MyResponse != "accepted" {
		t.Errorf("my_response = %q, want the preserved RSVP", e.MyResponse)
	}
	// A cancellation must survive the edit: dropping STATUS:CANCELLED would
	// register as a local content change and make the next sync patch the
	// cancelled meeting back to life.
	onDisk := readCalendarTestEvent(t, calDir, "evt-meeting")
	if !onDisk.IsCancelled {
		t.Error("IsCancelled dropped by the update merge")
	}

	// Compatibility: old GUI clients sent attendees:[] for every ordinary edit.
	// Without the explicit replacement flag that still means preserve.
	body = `{"account":"work","calendar":"Calendar","uid":"evt-meeting","subject":"Weekly Sync (moved)","start":"2026-08-06T10:00:00Z","end":"2026-08-06T11:00:00Z","attendees":[]}`
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v1/calendars/event", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("legacy PUT status %d: %s", w.Code, w.Body.String())
	}

	// A meeting attendee may still edit ordinary fields (the omitted list
	// above), but may not replace the organizer's attendee set.
	body = `{"account":"work","calendar":"Calendar","uid":"evt-meeting","subject":"Weekly Sync (moved)","start":"2026-08-06T10:00:00Z","end":"2026-08-06T11:00:00Z","attendees":["alice@example.com","bob@example.com"],"replace_attendees":true}`
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v1/calendars/event", strings.NewReader(body)))
	if w.Code != http.StatusForbidden {
		t.Fatalf("invitee PUT with attendees status = %d, want 403: %s", w.Code, w.Body.String())
	}
	got = struct {
		Event calendar.CalendarEvent `json:"event"`
	}{}
	if code := getJSON(t, r, "/api/v1/calendars/event?account=work&ref=evt-meeting", &got); code != http.StatusOK {
		t.Fatalf("GET status %d", code)
	}
	emails := make(map[string]bool, len(got.Event.Attendees))
	for _, attendee := range got.Event.Attendees {
		emails[attendee.Email] = true
	}
	if len(got.Event.Attendees) != 2 || !emails["me@example.com"] || !emails["alice@example.com"] {
		t.Errorf("attendees after rejected edit = %+v, want original set", got.Event.Attendees)
	}

	// Moving an event to another calendar is rejected.
	body = `{"account":"work","calendar":"Other","uid":"evt-meeting","subject":"x","start":"2026-08-06T10:00:00Z","end":"2026-08-06T11:00:00Z"}`
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v1/calendars/event", strings.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("PUT calendar move = %d, want 400", w.Code)
	}
}

func TestCalendarPutAttendeeEditPreservesOrganizerEntry(t *testing.T) {
	r, calDir := newCalendarHandler(t)
	writeCalendarTestEvent(t, calDir, calendar.Event{
		ICalUID: "evt-owned", Subject: "Owned meeting",
		Start:     time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC),
		End:       time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC),
		Organizer: &calendar.Person{Name: "Me", Email: "me@example.com"},
		Attendees: []calendar.Attendee{
			{Name: "Me", Email: "me@example.com", Type: "required", Response: "accepted"},
			{Name: "Alice", Email: "alice@example.com", Type: "optional", Response: "accepted"},
		},
	})

	// A present attendee list replaces the editable invitees. Retained
	// attendees keep their full role/RSVP metadata; new addresses are required.
	body := `{"account":"work","calendar":"Calendar","uid":"evt-owned","subject":"Owned meeting","start":"2026-08-07T09:00:00Z","end":"2026-08-07T10:00:00Z","attendees":["alice@example.com","bob@example.com"],"replace_attendees":true}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v1/calendars/event", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Event calendar.CalendarEvent `json:"event"`
	}
	if code := getJSON(t, r, "/api/v1/calendars/event?account=work&ref=evt-owned", &got); code != http.StatusOK {
		t.Fatalf("GET status %d", code)
	}
	if len(got.Event.Attendees) != 3 {
		t.Fatalf("attendees = %+v, want Alice, Bob and the organizer", got.Event.Attendees)
	}
	byEmail := make(map[string]calendar.AttendeeDTO, len(got.Event.Attendees))
	for _, attendee := range got.Event.Attendees {
		byEmail[attendee.Email] = attendee
	}
	if alice := byEmail["alice@example.com"]; alice.Name != "Alice" || alice.Type != "optional" || alice.Response != "accepted" {
		t.Errorf("retained attendee = %+v, want Alice's metadata preserved", alice)
	}
	if bob := byEmail["bob@example.com"]; bob.Type != "required" {
		t.Errorf("new attendee = %+v, want required Bob", bob)
	}

	// The GUI hides the organizer from its editable attendee list. Clearing the
	// visible list sends [], which removes invitees but retains that implicit
	// organizer attendee entry and its metadata.
	body = `{"account":"work","calendar":"Calendar","uid":"evt-owned","subject":"Owned meeting","start":"2026-08-07T09:00:00Z","end":"2026-08-07T10:00:00Z","attendees":[],"replace_attendees":true}`
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v1/calendars/event", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT clearing attendees status %d: %s", w.Code, w.Body.String())
	}
	got = struct {
		Event calendar.CalendarEvent `json:"event"`
	}{}
	if code := getJSON(t, r, "/api/v1/calendars/event?account=work&ref=evt-owned", &got); code != http.StatusOK {
		t.Fatalf("GET after clear status %d", code)
	}
	if len(got.Event.Attendees) != 1 {
		t.Fatalf("attendees after clear = %+v, want only the organizer entry", got.Event.Attendees)
	}
	organizer := got.Event.Attendees[0]
	if organizer.Email != "me@example.com" || organizer.Name != "Me" || organizer.Response != "accepted" {
		t.Errorf("organizer attendee = %+v, want its metadata preserved", organizer)
	}
}

func TestCalendarPutPlainAppointmentCanGainFirstAttendee(t *testing.T) {
	r, _ := newCalendarHandler(t)
	body := `{"account":"work","calendar":"Calendar","uid":"evt-lunch","subject":"Team Lunch","start":"2026-08-01T12:00:00Z","end":"2026-08-01T13:00:00Z","attendees":["alice@example.com"],"replace_attendees":true}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v1/calendars/event", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT first attendee status %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Event calendar.CalendarEvent `json:"event"`
	}
	if code := getJSON(t, r, "/api/v1/calendars/event?account=work&ref=evt-lunch", &got); code != http.StatusOK {
		t.Fatalf("GET status %d", code)
	}
	if len(got.Event.Attendees) != 1 || got.Event.Attendees[0].Email != "alice@example.com" {
		t.Errorf("attendees = %+v, want the first attendee", got.Event.Attendees)
	}
}

func TestCalendarPutCreateWithAttendeesAndOnlineMeeting(t *testing.T) {
	// A GUI create can carry an attendee email list and an online-meeting
	// request. Both land in the local .ics before the caller chooses whether
	// to follow up through targeted provider sync.
	r, calDir := newCalendarHandler(t)
	body := `{"account":"work","calendar":"Calendar","subject":"Kickoff",` +
		`"start":"2026-08-10T09:00:00Z","end":"2026-08-10T10:00:00Z",` +
		`"attendees":["alice@example.com"," alice@example.com","","Bob@Example.com"],` +
		`"request_online_meeting":true}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v1/calendars/event", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status %d: %s", w.Code, w.Body.String())
	}
	var put struct {
		Event calendar.CalendarEvent `json:"event"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &put); err != nil {
		t.Fatal(err)
	}
	if put.Event.UID == "" {
		t.Fatal("created event has no uid")
	}

	// The written .ics carries the ATTENDEE lines (deduped, required role)
	// and the pending online-meeting marker the sync's create picks up.
	path := filepath.Join(calDir, put.Event.UID+".ics")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Unfold the RFC 5545 75-octet line folding so substring checks work.
	unfold := func(b []byte) string { return strings.ReplaceAll(string(b), "\r\n ", "") }
	ics := unfold(data)
	if got := strings.Count(ics, "ATTENDEE;"); got != 2 {
		t.Errorf("ATTENDEE lines = %d, want 2 (deduped, blanks skipped):\n%s", got, ics)
	}
	for _, want := range []string{"mailto:alice@example.com", "mailto:Bob@Example.com", "ROLE=REQ-PARTICIPANT"} {
		if !strings.Contains(ics, want) {
			t.Errorf("ics missing %q:\n%s", want, ics)
		}
	}
	if !strings.Contains(ics, "X-DURIAN-CREATE-TEAMS-MEETING:TRUE") {
		t.Errorf("ics missing the online-meeting marker:\n%s", ics)
	}

	// A follow-up partial edit that omits attendees must not strip them or the
	// still-pending online-meeting request.
	body = `{"account":"work","calendar":"Calendar","uid":"` + put.Event.UID + `",` +
		`"subject":"Kickoff (moved)","start":"2026-08-10T11:00:00Z","end":"2026-08-10T12:00:00Z",` +
		`"request_online_meeting":false}`
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v1/calendars/event", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT update status %d: %s", w.Code, w.Body.String())
	}
	if data, err = os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	ics = unfold(data)
	if got := strings.Count(ics, "ATTENDEE;"); got != 2 {
		t.Errorf("ATTENDEE lines after edit = %d, want the 2 preserved:\n%s", got, ics)
	}
	if !strings.Contains(ics, "X-DURIAN-CREATE-TEAMS-MEETING:TRUE") {
		t.Errorf("online-meeting marker lost on edit:\n%s", ics)
	}
	if !strings.Contains(ics, "Kickoff (moved)") {
		t.Errorf("edit not applied:\n%s", ics)
	}

	// An attendee entry that does not look like an email is rejected.
	body = `{"account":"work","calendar":"Calendar","subject":"Bad","start":"2026-08-10T09:00:00Z","end":"2026-08-10T10:00:00Z","attendees":["not-an-email"]}`
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v1/calendars/event", strings.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("PUT invalid attendee = %d, want 400", w.Code)
	}
}

func TestCalendarRsvpHandler(t *testing.T) {
	r, calDir := newCalendarHandler(t)
	writeCalendarTestEvent(t, calDir, calendar.Event{
		ICalUID: "evt-invite", Subject: "Planning",
		Start: time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC),
		Attendees: []calendar.Attendee{
			{Name: "Me", Email: "ME@example.com", Type: "required", Response: "none"},
			{Name: "Alice", Email: "alice@example.com", Type: "required", Response: "accepted"},
		},
		Organizer: &calendar.Person{Name: "Org", Email: "organizer@example.com"},
	})

	// The Graph-style verb the GUI sends round-trips into the local .ics.
	body := `{"account":"work","calendar":"Calendar","ref":"evt-invite","response":"accepted"}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/calendars/rsvp", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("POST status %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK    bool                   `json:"ok"`
		Event calendar.CalendarEvent `json:"event"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Event.MyResponse != "accepted" {
		t.Errorf("response = %+v, want ok + my_response accepted", resp)
	}

	// The stored .ics carries the owner's PARTSTAT=ACCEPTED; the other
	// attendee's response is untouched.
	data, err := os.ReadFile(filepath.Join(calDir, "evt-invite.ics"))
	if err != nil {
		t.Fatal(err)
	}
	ev, err := calendar.ICalToEvent(data, "me@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if ev.OwnerResponse != calendar.OwnerRespAccepted {
		t.Errorf("stored OwnerResponse = %q, want accepted", ev.OwnerResponse)
	}
	if !strings.Contains(string(data), "PARTSTAT=ACCEPTED") {
		t.Error("stored .ics has no PARTSTAT=ACCEPTED for the owner")
	}
	for _, a := range ev.Attendees {
		if strings.EqualFold(a.Email, "alice@example.com") && a.Response != "accepted" {
			t.Errorf("other attendee response = %q, want untouched", a.Response)
		}
	}

	// Error paths: bad verb, no attendee entry for the owner (the seeded
	// evt-lunch has no attendees), organizer, unknown ref, missing account.
	cases := []struct {
		name string
		body string
		want int
	}{
		{"bad verb", `{"account":"work","ref":"evt-invite","response":"maybe"}`, http.StatusBadRequest},
		{"not an attendee", `{"account":"work","ref":"evt-lunch","response":"accepted"}`, http.StatusBadRequest},
		{"unknown ref", `{"account":"work","ref":"nonexistent","response":"accepted"}`, http.StatusNotFound},
		{"missing ref", `{"account":"work","response":"accepted"}`, http.StatusBadRequest},
		{"unknown account", `{"account":"nope","ref":"evt-invite","response":"accepted"}`, http.StatusNotFound},
		{"invalid body", `not json`, http.StatusBadRequest},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/calendars/rsvp", strings.NewReader(c.body)))
		if w.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, w.Code, c.want)
		}
	}
}

func TestCalendarRsvpHandlerOrganizer(t *testing.T) {
	r, calDir := newCalendarHandler(t)
	writeCalendarTestEvent(t, calDir, calendar.Event{
		ICalUID: "evt-mine", Subject: "My Meeting",
		Start: time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC),
		Attendees: []calendar.Attendee{
			{Name: "Me", Email: "me@example.com", Type: "required", Response: "organizer"},
			{Name: "Alice", Email: "alice@example.com", Type: "required", Response: "none"},
		},
		Organizer: &calendar.Person{Name: "Me", Email: "me@example.com"},
	})
	body := `{"account":"work","ref":"evt-mine","response":"declined"}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/calendars/rsvp", strings.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("organizer RSVP = %d, want 400", w.Code)
	}
}

func TestCalendarHandlerErrors(t *testing.T) {
	r, _ := newCalendarHandler(t)
	cases := []struct {
		url  string
		want int
	}{
		{"/api/v1/calendars", http.StatusBadRequest},                                  // missing account
		{"/api/v1/calendars?account=nope", http.StatusNotFound},                       // unknown account
		{"/api/v1/calendars/event?account=work", http.StatusBadRequest},               // missing ref
		{"/api/v1/calendars/event?account=work&ref=nonexistent", http.StatusNotFound}, // no match
	}
	for _, c := range cases {
		if code := getJSON(t, r, c.url, nil); code != c.want {
			t.Errorf("GET %s = %d, want %d", c.url, code, c.want)
		}
	}
}

// readCalendarTestEvent parses the .ics the handler wrote, so a test can
// assert on fields the JSON projection does not expose.
func readCalendarTestEvent(t *testing.T, calDir, uid string) calendar.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(calDir, uid+".ics"))
	if err != nil {
		t.Fatalf("read %s.ics: %v", uid, err)
	}
	ev, err := calendar.ICalToEvent(data, "me@example.com")
	if err != nil {
		t.Fatalf("parse %s.ics: %v", uid, err)
	}
	return ev
}

// TestCalendarDeleteKeepsBackup pins the recoverable delete: the event is gone
// from the vdir (so the next sync propagates the deletion) but the bytes
// survive as a non-.ics sibling the local scan ignores.
func TestCalendarDeleteKeepsBackup(t *testing.T) {
	r, calDir := newCalendarHandler(t)
	writeCalendarTestEvent(t, calDir, calendar.Event{
		ICalUID: "evt-gone", Subject: "Draft idea",
		Start: time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC),
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("DELETE", "/api/v1/calendars/event?account=work&ref=evt-gone", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE status %d: %s", w.Code, w.Body.String())
	}

	if _, err := os.Stat(filepath.Join(calDir, "evt-gone.ics")); !os.IsNotExist(err) {
		t.Errorf("the .ics still exists after delete (err=%v); the sync would not see the deletion", err)
	}
	backups, err := filepath.Glob(filepath.Join(calDir, "evt-gone.ics.deleted-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want exactly one recoverable copy", backups)
	}
	if strings.HasSuffix(backups[0], ".ics") {
		t.Error("the backup must not end in .ics, or the local scan would re-adopt it")
	}
}

// TestLogSafeStripsControlCharacters pins the log-injection guard: a calendar
// name or ref carrying newlines must not be able to forge extra log lines.
func TestLogSafeStripsControlCharacters(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Calendar", "Calendar"},
		{"Work\nlevel=ERROR msg=\"forged\"", "Work_level=ERROR msg=\"forged\""},
		{"a\rb\tc", "a_b_c"},
		{"Büro — Termine", "Büro — Termine"},
		{"\x00\x1b[31m", "__[31m"},
	}
	for _, tc := range cases {
		if got := logSafe(tc.in); got != tc.want {
			t.Errorf("logSafe(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// MARK: - Local calendars

// newLocalCalendarHandler builds a Handler whose config declares two local
// calendars in unrelated directories — one writable, one read-only — with no
// account owning them.
func newLocalCalendarHandler(t *testing.T) (http.Handler, string, string) {
	t.Helper()
	root := t.TempDir()
	privat := filepath.Join(root, "somewhere", "privat")
	verein := filepath.Join(root, "elsewhere", "verein")
	for _, d := range []string{privat, verein} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeCalendarTestEvent(t, privat, calendar.Event{
		ICalUID: "evt-dentist", Subject: "Zahnarzt",
		Start: time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC),
	})
	writeCalendarTestEvent(t, verein, calendar.Event{
		ICalUID: "evt-meeting", Subject: "Mitgliederversammlung",
		Start: time.Date(2026, 8, 4, 19, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 4, 21, 0, 0, 0, time.UTC),
	})

	cfg := &config.Config{
		Calendar: config.CalendarConfig{
			VdirPath: filepath.Join(root, "vdir"),
			Local: []config.LocalCalendarConfig{
				{Name: "Privat", Path: privat, Color: "#8ab4f8"},
				{Name: "Verein", Path: verein, ReadOnly: true},
			},
		},
		Accounts: []config.AccountConfig{{Name: "Work", Email: "me@example.com", Alias: "work"}},
	}
	h := New(nil, nil)
	h.SetConfig(cfg)
	return newTestRouter(h, nil), privat, verein
}

func TestCalendarsHandlerServesLocalCalendars(t *testing.T) {
	r, _, _ := newLocalCalendarHandler(t)

	var resp struct {
		Calendars []calendar.CalendarDTO `json:"calendars"`
	}
	if code := getJSON(t, r, "/api/v1/calendars?account=local", &resp); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(resp.Calendars) != 2 {
		t.Fatalf("calendars = %+v, want both local ones", resp.Calendars)
	}
	if resp.Calendars[0].Name != "Privat" || resp.Calendars[0].Color != "#8ab4f8" {
		t.Errorf("first = %+v, want the configured name and color", resp.Calendars[0])
	}
	if resp.Calendars[0].EventCount != 1 {
		t.Errorf("event count = %d, want 1", resp.Calendars[0].EventCount)
	}
}

func TestCalendarEventsHandlerServesLocalEvents(t *testing.T) {
	r, _, _ := newLocalCalendarHandler(t)

	var resp struct {
		Events []calendar.CalendarEvent `json:"events"`
	}
	code := getJSON(t, r,
		"/api/v1/calendars/events?account=local&from=2026-08-01T00:00:00Z&to=2026-08-10T00:00:00Z", &resp)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(resp.Events) != 2 {
		t.Fatalf("events = %+v, want both local events", resp.Events)
	}
	if resp.Events[0].Subject != "Zahnarzt" {
		t.Errorf("first event = %q, want the earlier one", resp.Events[0].Subject)
	}
}

// A read-only calendar exists so another tool can own the folder. Every write
// endpoint must refuse it — a guard on only some of them would make the
// promise hold by accident.
func TestLocalReadOnlyCalendarRefusesEveryWrite(t *testing.T) {
	r, _, verein := newLocalCalendarHandler(t)

	put := `{"account":"local","calendar":"Verein","subject":"Neu",` +
		`"start":"2026-08-05T10:00:00Z","end":"2026-08-05T11:00:00Z"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/v1/calendars/event", strings.NewReader(put))
	r.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Error("PUT created an event in a read-only calendar")
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("DELETE",
		"/api/v1/calendars/event?account=local&ref=evt-meeting&calendar=Verein", nil))
	if w.Code != http.StatusForbidden {
		t.Errorf("DELETE status = %d, want 403", w.Code)
	}

	// The event is still there, untouched.
	if _, err := os.Stat(filepath.Join(verein, "evt-meeting.ics")); err != nil {
		t.Errorf("the read-only event was removed: %v", err)
	}
}

func TestLocalWritableCalendarAcceptsAWrite(t *testing.T) {
	r, privat, _ := newLocalCalendarHandler(t)

	body := `{"account":"local","calendar":"Privat","subject":"Neuer Termin",` +
		`"start":"2026-08-06T10:00:00Z","end":"2026-08-06T11:00:00Z"}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v1/calendars/event", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	entries, err := os.ReadDir(privat)
	if err != nil {
		t.Fatal(err)
	}
	var ics int
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".ics") {
			ics++
		}
	}
	if ics != 2 {
		t.Errorf("got %d .ics files, want the seeded one plus the new one", ics)
	}
}

// An unknown identifier must still 404 — "local" is reserved, not a catch-all.
func TestUnknownCalendarAccountStill404s(t *testing.T) {
	r, _, _ := newLocalCalendarHandler(t)
	if code := getJSON(t, r, "/api/v1/calendars?account=nope", nil); code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
}
