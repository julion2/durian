package googlecalendar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
)

// testOwnerEmail is the calendar owner of every test client.
const testOwnerEmail = "me@example.com"

// testClient builds a Client pointed at the given httptest server, with a
// static token so no OAuth path is exercised.
func testClient(srv *httptest.Server) *Client {
	return NewWithToken(testOwnerEmail, srv.URL, "test-token", srv.Client())
}

// requireBearer fails the request when the static test token is missing, so
// every endpoint asserts the Authorization header.
func requireBearer(t *testing.T, w http.ResponseWriter, r *http.Request) bool {
	t.Helper()
	if r.Header.Get("Authorization") != "Bearer test-token" {
		t.Errorf("missing bearer token on %s", r.URL.Path)
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	return true
}

// MARK: - ListCalendars

func TestListCalendars(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me/calendarList", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(t, w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageToken") == "" {
			_, _ = w.Write([]byte(`{
				"items": [
					{"id": "cal-primary", "summary": "Personal", "backgroundColor": "#9fe1e7"}
				],
				"nextPageToken": "page2"
			}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"items": [
				{"id": "cal-work", "summary": "Work"}
			]
		}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	calendars, err := testClient(srv).ListCalendars(context.Background())
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	if len(calendars) != 2 {
		t.Fatalf("got %d calendars, want 2", len(calendars))
	}
	if calendars[0].ID != "cal-primary" || calendars[0].Name != "Personal" || calendars[0].HexColor != "#9fe1e7" {
		t.Errorf("first calendar = %+v", calendars[0])
	}
	if calendars[1].ID != "cal-work" || calendars[1].Name != "Work" || calendars[1].HexColor != "" {
		t.Errorf("second calendar = %+v", calendars[1])
	}
}

// MARK: - FetchMasterEvents

// masterEventsPage1 holds a timed meeting (offset times, attendees incl. the
// owner, conference data) plus a cancelled tombstone and a detached instance
// that must both be skipped.
const masterEventsPage1 = `{
	"items": [
		{
			"id": "evt-timed",
			"status": "confirmed",
			"etag": "\"etag-timed\"",
			"iCalUID": "uid-timed@google.com",
			"summary": "Design review",
			"description": "Agenda notes",
			"location": "Room 4",
			"updated": "2026-07-01T08:30:00.000Z",
			"start": {"dateTime": "2026-07-20T10:00:00+02:00", "timeZone": "Europe/Zurich"},
			"end": {"dateTime": "2026-07-20T11:00:00+02:00", "timeZone": "Europe/Zurich"},
			"organizer": {"email": "boss@example.com", "displayName": "Boss"},
			"attendees": [
				{"email": "boss@example.com", "displayName": "Boss", "responseStatus": "accepted"},
				{"email": "me@example.com", "self": true, "responseStatus": "accepted"},
				{"email": "maybe@example.com", "optional": true, "responseStatus": "tentative"},
				{"email": "room4@example.com", "resource": true, "responseStatus": "needsAction"}
			],
			"hangoutLink": "https://meet.google.com/legacy",
			"conferenceData": {
				"entryPoints": [
					{"entryPointType": "phone", "uri": "tel:+41-44-000-00-00"},
					{"entryPointType": "video", "uri": "https://meet.google.com/abc-defg-hij"}
				]
			}
		},
		{
			"id": "evt-cancelled",
			"status": "cancelled",
			"etag": "\"etag-cancelled\""
		},
		{
			"id": "evt-detached",
			"status": "confirmed",
			"recurringEventId": "evt-weekly",
			"start": {"dateTime": "2026-07-21T09:00:00Z"},
			"end": {"dateTime": "2026-07-21T10:00:00Z"}
		}
	],
	"nextPageToken": "page2"
}`

// masterEventsPage2 holds an all-day event and a recurring weekly master with
// an RRULE plus an EXDATE line that is dropped.
const masterEventsPage2 = `{
	"items": [
		{
			"id": "evt-allday",
			"status": "confirmed",
			"etag": "\"etag-allday\"",
			"iCalUID": "uid-allday@google.com",
			"summary": "Holiday",
			"start": {"date": "2026-07-23"},
			"end": {"date": "2026-07-24"}
		},
		{
			"id": "evt-weekly",
			"status": "confirmed",
			"etag": "\"etag-weekly\"",
			"iCalUID": "uid-weekly@google.com",
			"summary": "Standup",
			"start": {"dateTime": "2026-07-06T09:00:00Z"},
			"end": {"dateTime": "2026-07-06T09:15:00Z"},
			"recurrence": [
				"EXDATE;TZID=Europe/Zurich:20260713T090000",
				"RRULE:FREQ=WEEKLY;BYDAY=MO;UNTIL=20261230T235959Z"
			],
			"organizer": {"email": "me@example.com", "self": true},
			"attendees": [
				{"email": "me@example.com", "self": true, "responseStatus": "accepted"},
				{"email": "dev@example.com", "responseStatus": "declined"}
			]
		}
	]
}`

func TestFetchMasterEvents(t *testing.T) {
	var pageTokens []string
	mux := http.NewServeMux()
	mux.HandleFunc("/calendars/cal-primary/events", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(t, w, r) {
			return
		}
		if got := r.URL.Query().Get("singleEvents"); got != "false" {
			t.Errorf("singleEvents = %q, want \"false\"", got)
		}
		pageTokens = append(pageTokens, r.URL.Query().Get("pageToken"))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageToken") == "" {
			_, _ = w.Write([]byte(masterEventsPage1))
			return
		}
		_, _ = w.Write([]byte(masterEventsPage2))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	events, err := testClient(srv).FetchMasterEvents(context.Background(), "cal-primary")
	if err != nil {
		t.Fatalf("FetchMasterEvents: %v", err)
	}
	if !reflect.DeepEqual(pageTokens, []string{"", "page2"}) {
		t.Errorf("pageTokens = %v", pageTokens)
	}
	// Cancelled tombstone and detached instance are filtered out.
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3 (cancelled + detached skipped): %+v", len(events), events)
	}

	timed := events[0]
	if timed.ID != "evt-timed" || timed.ICalUID != "uid-timed@google.com" {
		t.Errorf("timed identity = %q / %q", timed.ID, timed.ICalUID)
	}
	if timed.ETag != `"etag-timed"` {
		t.Errorf("timed ETag = %q", timed.ETag)
	}
	if timed.Subject != "Design review" || timed.Location != "Room 4" || timed.Description != "Agenda notes" {
		t.Errorf("timed content = %+v", timed)
	}
	// Offset times normalized to UTC.
	if want := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC); !timed.Start.Equal(want) {
		t.Errorf("timed Start = %v, want %v", timed.Start, want)
	}
	if want := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC); !timed.End.Equal(want) {
		t.Errorf("timed End = %v, want %v", timed.End, want)
	}
	if timed.AllDay || timed.Type != "singleInstance" || timed.Recurrence != nil {
		t.Errorf("timed shape = allDay=%v type=%q rec=%v", timed.AllDay, timed.Type, timed.Recurrence)
	}
	if want := time.Date(2026, 7, 1, 8, 30, 0, 0, time.UTC); !timed.LastModified.Equal(want) {
		t.Errorf("timed LastModified = %v", timed.LastModified)
	}
	wantAttendees := []calendar.Attendee{
		{Name: "Boss", Email: "boss@example.com", Type: "required", Response: "accepted"},
		{Email: "me@example.com", Type: "required", Response: "accepted"},
		{Email: "maybe@example.com", Type: "optional", Response: "tentativelyAccepted"},
		{Email: "room4@example.com", Type: "resource", Response: "none"},
	}
	if !reflect.DeepEqual(timed.Attendees, wantAttendees) {
		t.Errorf("timed Attendees = %+v", timed.Attendees)
	}
	if timed.Organizer == nil || timed.Organizer.Email != "boss@example.com" {
		t.Errorf("timed Organizer = %+v", timed.Organizer)
	}
	if timed.IsOrganizer {
		t.Errorf("timed IsOrganizer = true, want false")
	}
	if timed.OwnerResponse != calendar.OwnerRespAccepted {
		t.Errorf("timed OwnerResponse = %q", timed.OwnerResponse)
	}
	// conferenceData video entry point wins over the legacy hangoutLink.
	if timed.OnlineMeetingURL != "https://meet.google.com/abc-defg-hij" || !timed.IsOnlineMeeting {
		t.Errorf("timed OnlineMeetingURL = %q", timed.OnlineMeetingURL)
	}
	if timed.IsCancelled {
		t.Errorf("timed IsCancelled = true")
	}

	allDay := events[1]
	if !allDay.AllDay {
		t.Errorf("all-day event not marked AllDay")
	}
	if want := time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC); !allDay.Start.Equal(want) {
		t.Errorf("all-day Start = %v", allDay.Start)
	}
	// Exclusive end date kept as-is.
	if want := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC); !allDay.End.Equal(want) {
		t.Errorf("all-day End = %v", allDay.End)
	}

	weekly := events[2]
	if weekly.Type != "seriesMaster" {
		t.Errorf("weekly Type = %q", weekly.Type)
	}
	if weekly.Recurrence == nil {
		t.Fatalf("weekly Recurrence is nil")
	}
	wantRec := &calendar.Recurrence{
		Pattern: calendar.RecurrencePattern{
			Type:       "weekly",
			Interval:   1,
			DaysOfWeek: []string{"monday"},
		},
		Range: calendar.RecurrenceRange{
			Type:      "endDate",
			StartDate: "2026-07-06",
			EndDate:   "2026-12-30",
		},
	}
	if !reflect.DeepEqual(weekly.Recurrence, wantRec) {
		t.Errorf("weekly Recurrence = %+v, want %+v", weekly.Recurrence, wantRec)
	}
	// The owner organizes the series: organizer wins over the attendee RSVP.
	if !weekly.IsOrganizer || weekly.OwnerResponse != calendar.OwnerRespOrganizer {
		t.Errorf("weekly organizer state = %v / %q", weekly.IsOrganizer, weekly.OwnerResponse)
	}
}

// MARK: - FetchInstances

func TestFetchInstances(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/calendars/cal-primary/events", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(t, w, r) {
			return
		}
		q := r.URL.Query()
		if got := q.Get("singleEvents"); got != "true" {
			t.Errorf("singleEvents = %q, want \"true\"", got)
		}
		if got := q.Get("timeMin"); got != "2026-07-01T00:00:00Z" {
			t.Errorf("timeMin = %q", got)
		}
		if got := q.Get("timeMax"); got != "2026-08-01T00:00:00Z" {
			t.Errorf("timeMax = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"items": [
				{
					"id": "evt-weekly_20260706T090000Z",
					"status": "confirmed",
					"recurringEventId": "evt-weekly",
					"iCalUID": "uid-weekly@google.com",
					"summary": "Standup",
					"start": {"dateTime": "2026-07-06T09:00:00Z"},
					"end": {"dateTime": "2026-07-06T09:15:00Z"}
				},
				{"id": "evt-gone", "status": "cancelled"}
			]
		}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	events, err := testClient(srv).FetchInstances(context.Background(), "cal-primary", from, to)
	if err != nil {
		t.Fatalf("FetchInstances: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d instances, want 1 (cancelled skipped)", len(events))
	}
	if events[0].ID != "evt-weekly_20260706T090000Z" || events[0].ICalUID != "uid-weekly@google.com" {
		t.Errorf("instance identity = %+v", events[0])
	}
	if events[0].Type != "occurrence" {
		t.Errorf("instance Type = %q", events[0].Type)
	}
}

// MARK: - GetEvent

func TestGetEvent(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/calendars/cal-primary/events/evt-timed", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(t, w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "evt-timed",
			"status": "cancelled",
			"etag": "\"etag-2\"",
			"iCalUID": "uid-timed@google.com",
			"summary": "Design review",
			"start": {"dateTime": "2026-07-20T10:00:00+02:00"},
			"end": {"dateTime": "2026-07-20T11:00:00+02:00"}
		}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ev, err := testClient(srv).GetEvent(context.Background(), "cal-primary", "evt-timed")
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if ev.ID != "evt-timed" || ev.ETag != `"etag-2"` {
		t.Errorf("event identity = %q / %q", ev.ID, ev.ETag)
	}
	// GetEvent maps a cancelled status instead of filtering it, so the engine
	// sees the cancellation on read-back.
	if !ev.IsCancelled {
		t.Errorf("IsCancelled = false, want true")
	}
	if want := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC); !ev.Start.Equal(want) {
		t.Errorf("Start = %v, want %v", ev.Start, want)
	}
}

// MARK: - IsAuthError

func TestIsAuthError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me/calendarList", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error": {"code": 401, "message": "Invalid Credentials"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := testClient(srv)
	_, err := c.ListCalendars(context.Background())
	if err == nil {
		t.Fatalf("expected error from 401 response")
	}
	if !c.IsAuthError(err) {
		t.Errorf("IsAuthError(401) = false, want true")
	}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"403 insufficientPermissions", &statusError{status: 403, body: `"reason": "insufficientPermissions"`}, true},
		{"403 accessNotConfigured", &statusError{status: 403, body: `"reason": "accessNotConfigured"`}, true},
		{"403 rateLimitExceeded", &statusError{status: 403, body: `"reason": "rateLimitExceeded"`}, false},
		{"403 userRateLimitExceeded", &statusError{status: 403, body: `"reason": "userRateLimitExceeded"`}, false},
		{"404 not found", &statusError{status: 404, body: "Not Found"}, false},
	}
	for _, tc := range cases {
		if got := IsAuthError(tc.err); got != tc.want {
			t.Errorf("IsAuthError(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestDoRequestRetriesRateLimit403 pins the throttle path that IsAuthError
// already knows about but the retry switch used to miss: Google returns 403
// (not 429) for per-user rate limiting, which is transient. Falling through to
// the error path failed the whole sync on a condition that clears by waiting.
func TestDoRequestRetriesRateLimit403(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"errors":[{"reason":"userRateLimitExceeded"}]}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"evt-1"}`))
	}))
	defer srv.Close()

	var out map[string]any
	if err := testClient(srv).doJSON(context.Background(), srv.URL+"/x", nil, &out); err != nil {
		t.Fatalf("doJSON after a 403 throttle: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (one throttled, one retried)", calls)
	}
}

// TestDoRequestDoesNotRetryPermission403 keeps the retry narrow: a genuine
// permission failure must surface immediately, not after three backoffs.
func TestDoRequestDoesNotRetryPermission403(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"errors":[{"reason":"insufficientPermissions"}]}}`))
	}))
	defer srv.Close()

	var out map[string]any
	err := testClient(srv).doJSON(context.Background(), srv.URL+"/x", nil, &out)
	if err == nil {
		t.Fatal("doJSON succeeded on a permission 403")
	}
	if !IsAuthError(err) {
		t.Errorf("IsAuthError = false for a permission 403: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry)", calls)
	}
}
