package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/graphcalendar"
)

// newCalendarHandler builds a Handler whose config points at a temp vdir seeded
// with two events in one "Calendar" of account alias "work".
func newCalendarHandler(t *testing.T) http.Handler {
	t.Helper()
	base := t.TempDir()
	calDir := filepath.Join(base, "work", "Calendar")
	if err := os.MkdirAll(calDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(calDir, "displayname"), []byte("Calendar\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeEvent := func(uid, subject string, start time.Time) {
		data, err := graphcalendar.EventToICal(graphcalendar.Event{
			ICalUID: uid, Subject: subject, Start: start, End: start.Add(time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(calDir, uid+".ics"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeEvent("evt-lunch", "Team Lunch", time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC))
	writeEvent("evt-review", "Design Review", time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC))

	cfg := &config.Config{
		Calendar: config.CalendarConfig{VdirPath: base},
		Accounts: []config.AccountConfig{{Name: "Work", Email: "me@example.com", Alias: "work"}},
	}
	h := New(nil, nil)
	h.SetConfig(cfg)
	return newTestRouter(h, nil)
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
	r := newCalendarHandler(t)
	var resp struct {
		OK        bool                        `json:"ok"`
		Calendars []graphcalendar.CalendarDTO `json:"calendars"`
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
	r := newCalendarHandler(t)
	var resp struct {
		OK     bool                          `json:"ok"`
		Events []graphcalendar.CalendarEvent `json:"events"`
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
	r := newCalendarHandler(t)
	var resp struct {
		Events []graphcalendar.CalendarEvent `json:"events"`
	}
	// Window before both events.
	getJSON(t, r, "/api/v1/calendars/events?account=work&from=2026-07-01&to=2026-07-31", &resp)
	if len(resp.Events) != 0 {
		t.Errorf("want 0 events out of window, got %d", len(resp.Events))
	}
}

func TestCalendarEventsSearch(t *testing.T) {
	r := newCalendarHandler(t)
	var resp struct {
		Events []graphcalendar.CalendarEvent `json:"events"`
	}
	getJSON(t, r, "/api/v1/calendars/events?account=work&q=review", &resp)
	if len(resp.Events) != 1 || resp.Events[0].Subject != "Design Review" {
		t.Errorf("search q=review = %+v", resp.Events)
	}
}

func TestCalendarEventDetail(t *testing.T) {
	r := newCalendarHandler(t)
	var resp struct {
		OK    bool                        `json:"ok"`
		Event graphcalendar.CalendarEvent `json:"event"`
	}
	if code := getJSON(t, r, "/api/v1/calendars/event?account=work&ref=lunch", &resp); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if !resp.OK || resp.Event.UID != "evt-lunch" || resp.Event.Subject != "Team Lunch" {
		t.Errorf("event = %+v", resp.Event)
	}
}

func TestCalendarHandlerErrors(t *testing.T) {
	r := newCalendarHandler(t)
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
