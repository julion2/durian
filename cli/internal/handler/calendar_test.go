package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
	"github.com/julion2/durian/cli/internal/config"
)

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
		Accounts: []config.AccountConfig{{Name: "Work", Email: "me@example.com", Alias: "work"}},
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

	// An explicit attendee list in the request replaces the existing one.
	body = `{"account":"work","calendar":"Calendar","uid":"evt-meeting","subject":"Weekly Sync (moved)","start":"2026-08-06T10:00:00Z","end":"2026-08-06T11:00:00Z","attendees":[{"email":"bob@example.com","type":"required"}]}`
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v1/calendars/event", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT with attendees status %d: %s", w.Code, w.Body.String())
	}
	if code := getJSON(t, r, "/api/v1/calendars/event?account=work&ref=evt-meeting", &got); code != http.StatusOK {
		t.Fatalf("GET status %d", code)
	}
	if len(got.Event.Attendees) != 1 || got.Event.Attendees[0].Email != "bob@example.com" {
		t.Errorf("attendees = %+v, want the explicit replacement", got.Event.Attendees)
	}

	// Moving an event to another calendar is rejected.
	body = `{"account":"work","calendar":"Other","uid":"evt-meeting","subject":"x","start":"2026-08-06T10:00:00Z","end":"2026-08-06T11:00:00Z"}`
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v1/calendars/event", strings.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("PUT calendar move = %d, want 400", w.Code)
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
