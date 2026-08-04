package googlecalendar

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
	"github.com/julion2/durian/cli/internal/calendarsync"
)

// capturedRequest records one write request the fake server received.
type capturedRequest struct {
	method string
	path   string
	query  url.Values
	header http.Header
	body   map[string]any
}

// captureServer starts a fake server that records every request into the
// returned slice and answers each with the paired status/JSON response.
// Responses are consumed in order; extra requests get the last one.
func captureServer(t *testing.T, statuses []int, responses []string) (*httptest.Server, *[]capturedRequest) {
	t.Helper()
	var captured []capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(t, w, r) {
			return
		}
		cr := capturedRequest{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.Query(),
			header: r.Header.Clone(),
		}
		if r.Body != nil {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				cr.body = body
			}
		}
		idx := len(captured)
		captured = append(captured, cr)
		if idx >= len(statuses) {
			idx = len(statuses) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statuses[idx])
		_, _ = w.Write([]byte(responses[idx]))
	}))
	return srv, &captured
}

// testMeetingEvent is a timed recurring meeting the owner organizes, used by
// the create/update body assertions.
func testMeetingEvent() calendar.Event {
	return calendar.Event{
		Subject:     "Design review",
		Location:    "Room 4",
		Description: "Agenda notes",
		Start:       time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC),
		End:         time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC),
		Recurrence: &calendar.Recurrence{
			Pattern: calendar.RecurrencePattern{
				Type:       "weekly",
				Interval:   1,
				DaysOfWeek: []string{"monday"},
			},
			Range: calendar.RecurrenceRange{
				Type:      "endDate",
				StartDate: "2026-07-20",
				EndDate:   "2026-12-30",
			},
		},
		Attendees: []calendar.Attendee{
			{Name: "Me", Email: testOwnerEmail, Type: "required", Response: "accepted"},
			{Email: "maybe@example.com", Type: "optional", Response: "tentativelyAccepted"},
			{Email: "room4@example.com", Type: "resource", Response: "none"},
		},
	}
}

// wantRRuleLine is the serialized recurrence of testMeetingEvent.
const wantRRuleLine = "RRULE:FREQ=WEEKLY;UNTIL=20261230T235959Z;BYDAY=MO"

// createdEventJSON is the fake POST response of a successful create.
const createdEventJSON = `{
	"id": "550e8400e29b41d4a716446655440000",
	"etag": "\"etag-created\"",
	"iCalUID": "uid-created@google.com",
	"summary": "Design review",
	"start": {"dateTime": "2026-07-20T08:00:00Z"},
	"end": {"dateTime": "2026-07-20T09:00:00Z"}
}`

// MARK: - CreateEvent

func TestCreateEventOrganizedMeeting(t *testing.T) {
	srv, captured := captureServer(t, []int{http.StatusOK}, []string{createdEventJSON})
	defer srv.Close()

	created, err := testClient(srv).CreateEvent(context.Background(), "cal-primary", testMeetingEvent(),
		calendarsync.CreateOptions{
			IncludeAttendees:     true,
			NotifyAttendees:      true,
			RequestOnlineMeeting: true,
			IdempotencyKey:       "550E8400-E29B-41D4-A716-446655440000",
		})
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	if len(*captured) != 1 {
		t.Fatalf("got %d requests, want 1", len(*captured))
	}
	req := (*captured)[0]
	if req.method != http.MethodPost || req.path != "/calendars/cal-primary/events" {
		t.Errorf("request = %s %s", req.method, req.path)
	}
	if got := req.query.Get("sendUpdates"); got != "all" {
		t.Errorf("sendUpdates = %q, want \"all\"", got)
	}
	if got := req.query.Get("conferenceDataVersion"); got != "1" {
		t.Errorf("conferenceDataVersion = %q, want \"1\"", got)
	}

	body := req.body
	// Idempotency key: UUID lowercased, dashes stripped (valid base32hex).
	if got := body["id"]; got != "550e8400e29b41d4a716446655440000" {
		t.Errorf("body id = %v", got)
	}
	if body["summary"] != "Design review" || body["location"] != "Room 4" || body["description"] != "Agenda notes" {
		t.Errorf("body content = %v / %v / %v", body["summary"], body["location"], body["description"])
	}
	wantStart := map[string]any{"dateTime": "2026-07-20T08:00:00Z", "timeZone": "UTC"}
	if !reflect.DeepEqual(body["start"], wantStart) {
		t.Errorf("body start = %v, want %v", body["start"], wantStart)
	}
	wantEnd := map[string]any{"dateTime": "2026-07-20T09:00:00Z", "timeZone": "UTC"}
	if !reflect.DeepEqual(body["end"], wantEnd) {
		t.Errorf("body end = %v, want %v", body["end"], wantEnd)
	}
	if !reflect.DeepEqual(body["recurrence"], []any{wantRRuleLine}) {
		t.Errorf("body recurrence = %v, want [%s]", body["recurrence"], wantRRuleLine)
	}
	wantAttendees := []any{
		map[string]any{"email": testOwnerEmail, "displayName": "Me", "responseStatus": "accepted"},
		map[string]any{"email": "maybe@example.com", "optional": true, "responseStatus": "tentative"},
		map[string]any{"email": "room4@example.com", "resource": true, "responseStatus": "needsAction"},
	}
	if !reflect.DeepEqual(body["attendees"], wantAttendees) {
		t.Errorf("body attendees = %v, want %v", body["attendees"], wantAttendees)
	}
	// No organizer/etag/status keys are ever uploaded.
	for _, key := range []string{"organizer", "etag", "status"} {
		if _, ok := body[key]; ok {
			t.Errorf("body carries forbidden key %q", key)
		}
	}
	wantConf := map[string]any{
		"createRequest": map[string]any{
			"requestId":             "550e8400e29b41d4a716446655440000",
			"conferenceSolutionKey": map[string]any{"type": "hangoutsMeet"},
		},
	}
	if !reflect.DeepEqual(body["conferenceData"], wantConf) {
		t.Errorf("body conferenceData = %v, want %v", body["conferenceData"], wantConf)
	}

	if created.ID != "550e8400e29b41d4a716446655440000" || created.ETag != `"etag-created"` ||
		created.ICalUID != "uid-created@google.com" {
		t.Errorf("created event = %+v", created)
	}
}

func TestCreateEventAllDayAppointment(t *testing.T) {
	srv, captured := captureServer(t, []int{http.StatusOK}, []string{`{
		"id": "server-assigned",
		"etag": "\"etag-a\"",
		"start": {"date": "2026-07-23"},
		"end": {"date": "2026-07-24"}
	}`})
	defer srv.Close()

	ev := calendar.Event{
		Subject: "Holiday",
		Start:   time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC),
		End:     time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC), // not after start: snapped
		AllDay:  true,
	}
	if _, err := testClient(srv).CreateEvent(context.Background(), "cal-primary", ev,
		calendarsync.CreateOptions{}); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	req := (*captured)[0]
	if got := req.query.Get("sendUpdates"); got != "none" {
		t.Errorf("sendUpdates = %q, want \"none\"", got)
	}
	if req.query.Has("conferenceDataVersion") {
		t.Errorf("conferenceDataVersion sent without RequestOnlineMeeting")
	}
	body := req.body
	if !reflect.DeepEqual(body["start"], map[string]any{"date": "2026-07-23"}) {
		t.Errorf("body start = %v", body["start"])
	}
	// End date not after start snaps to start + 1 day (exclusive end).
	if !reflect.DeepEqual(body["end"], map[string]any{"date": "2026-07-24"}) {
		t.Errorf("body end = %v", body["end"])
	}
	// Attendees stay absent without IncludeAttendees (role gate), the id stays
	// absent without an idempotency key, and no recurrence means empty list.
	if _, ok := body["attendees"]; ok {
		t.Errorf("body carries attendees without IncludeAttendees")
	}
	if _, ok := body["id"]; ok {
		t.Errorf("body carries id without IdempotencyKey")
	}
	if !reflect.DeepEqual(body["recurrence"], []any{}) {
		t.Errorf("body recurrence = %v, want []", body["recurrence"])
	}
}

func TestCreateEventConflictFetchesExisting(t *testing.T) {
	srv, captured := captureServer(t,
		[]int{http.StatusConflict, http.StatusOK},
		[]string{`{"error": {"code": 409, "message": "The requested identifier already exists."}}`, createdEventJSON})
	defer srv.Close()

	created, err := testClient(srv).CreateEvent(context.Background(), "cal-primary", testMeetingEvent(),
		calendarsync.CreateOptions{IdempotencyKey: "550e8400-e29b-41d4-a716-446655440000"})
	if err != nil {
		t.Fatalf("CreateEvent after 409: %v", err)
	}
	if len(*captured) != 2 {
		t.Fatalf("got %d requests, want POST + GET", len(*captured))
	}
	get := (*captured)[1]
	if get.method != http.MethodGet || get.path != "/calendars/cal-primary/events/550e8400e29b41d4a716446655440000" {
		t.Errorf("fold request = %s %s", get.method, get.path)
	}
	if created.ID != "550e8400e29b41d4a716446655440000" || created.ETag != `"etag-created"` {
		t.Errorf("folded event = %+v", created)
	}
}

func TestCreateEventID(t *testing.T) {
	// A UUID key becomes its lowercase dash-less hex form.
	if got := createEventID("550E8400-E29B-41D4-A716-446655440000"); got != "550e8400e29b41d4a716446655440000" {
		t.Errorf("createEventID(uuid) = %q", got)
	}
	// A key outside the base32hex charset falls back to its SHA-256 digest;
	// both branches must satisfy Google's id constraints.
	weird := createEventID("Local-File.ics!")
	if !validGoogleEventID(weird) {
		t.Errorf("createEventID fallback %q not a valid Google event id", weird)
	}
	if weird != createEventID("Local-File.ics!") {
		t.Errorf("createEventID fallback not deterministic")
	}
	if weird == createEventID("Other-File.ics!") {
		t.Errorf("createEventID fallback not key-dependent")
	}
}

// MARK: - UpdateEvent

func TestUpdateEventFull(t *testing.T) {
	srv, captured := captureServer(t, []int{http.StatusOK}, []string{`{"etag": "\"etag-2\""}`})
	defer srv.Close()

	err := testClient(srv).UpdateEvent(context.Background(), "cal-primary", "evt-1", calendarsync.UpdateSpec{
		Event: testMeetingEvent(),
		ETag:  `"etag-1"`,
	})
	if err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}
	req := (*captured)[0]
	if req.method != http.MethodPatch || req.path != "/calendars/cal-primary/events/evt-1" {
		t.Errorf("request = %s %s", req.method, req.path)
	}
	if got := req.header.Get("If-Match"); got != `"etag-1"` {
		t.Errorf("If-Match = %q", got)
	}
	if got := req.query.Get("sendUpdates"); got != "none" {
		t.Errorf("sendUpdates = %q, want \"none\"", got)
	}
	if _, ok := req.body["attendees"]; ok {
		t.Errorf("full update carries attendees without IncludeAttendees")
	}
	if req.body["summary"] != "Design review" {
		t.Errorf("body summary = %v", req.body["summary"])
	}
	if !reflect.DeepEqual(req.body["recurrence"], []any{wantRRuleLine}) {
		t.Errorf("body recurrence = %v", req.body["recurrence"])
	}
}

func TestUpdateEventAttendeesOnly(t *testing.T) {
	srv, captured := captureServer(t, []int{http.StatusOK}, []string{`{"etag": "\"etag-2\""}`})
	defer srv.Close()

	err := testClient(srv).UpdateEvent(context.Background(), "cal-primary", "evt-1", calendarsync.UpdateSpec{
		Event:            testMeetingEvent(),
		IncludeAttendees: true,
		NotifyAttendees:  true,
		AttendeesOnly:    true,
	})
	if err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}
	req := (*captured)[0]
	if got := req.query.Get("sendUpdates"); got != "all" {
		t.Errorf("sendUpdates = %q, want \"all\"", got)
	}
	if req.header.Get("If-Match") != "" {
		t.Errorf("If-Match sent without ETag")
	}
	// The scoped patch carries ONLY the attendee list.
	if len(req.body) != 1 {
		t.Errorf("attendees-only body keys = %v, want just attendees", req.body)
	}
	if _, ok := req.body["attendees"]; !ok {
		t.Errorf("attendees-only body misses attendees: %v", req.body)
	}
}

func TestUpdateEventSentinels(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   error
	}{
		{"412 precondition", http.StatusPreconditionFailed, calendarsync.ErrPrecondition},
		{"404 not found", http.StatusNotFound, calendarsync.ErrNotFound},
		{"410 gone", http.StatusGone, calendarsync.ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := captureServer(t, []int{tc.status}, []string{`{"error": {}}`})
			defer srv.Close()

			err := testClient(srv).UpdateEvent(context.Background(), "cal-primary", "evt-1",
				calendarsync.UpdateSpec{Event: testMeetingEvent(), ETag: `"etag-1"`})
			if !errors.Is(err, tc.want) {
				t.Errorf("UpdateEvent(%d) = %v, want %v sentinel", tc.status, err, tc.want)
			}
		})
	}
}

// MARK: - DeleteEvent

func TestDeleteEvent(t *testing.T) {
	srv, captured := captureServer(t, []int{http.StatusNoContent}, []string{""})
	defer srv.Close()

	if err := testClient(srv).DeleteEvent(context.Background(), "cal-primary", "evt-1", `"etag-1"`, true); err != nil {
		t.Fatalf("DeleteEvent: %v", err)
	}
	req := (*captured)[0]
	if req.method != http.MethodDelete || req.path != "/calendars/cal-primary/events/evt-1" {
		t.Errorf("request = %s %s", req.method, req.path)
	}
	if got := req.header.Get("If-Match"); got != `"etag-1"` {
		t.Errorf("If-Match = %q", got)
	}
}

// TestDeleteEventNotifiesAttendees pins the cancellation mail. Google's
// sendUpdates defaults to "none", so an organizer's delete that omits the
// parameter removes the meeting from everyone's calendar without telling a
// soul — while the CLI preview announces a cancellation mail.
func TestDeleteEventNotifiesAttendees(t *testing.T) {
	cases := []struct {
		notify bool
		want   string
	}{
		{true, "all"},
		{false, "none"},
	}
	for _, tc := range cases {
		srv, captured := captureServer(t, []int{http.StatusNoContent}, []string{""})
		if err := testClient(srv).DeleteEvent(context.Background(), "cal-primary", "evt-1", "", tc.notify); err != nil {
			t.Fatalf("DeleteEvent(notify=%v): %v", tc.notify, err)
		}
		if got := (*captured)[0].query.Get("sendUpdates"); got != tc.want {
			t.Errorf("DeleteEvent(notify=%v) sendUpdates = %q, want %q", tc.notify, got, tc.want)
		}
		srv.Close()
	}
}

// TestUpdateEventNotifiesWithoutAttendeeChange covers a reschedule: the
// attendee set is untouched, so gating the notification on the attendee
// upload would send sendUpdates=none and nobody would learn the meeting moved.
func TestUpdateEventNotifiesWithoutAttendeeChange(t *testing.T) {
	srv, captured := captureServer(t, []int{http.StatusOK}, []string{`{"etag": "\"etag-2\""}`})
	defer srv.Close()

	err := testClient(srv).UpdateEvent(context.Background(), "cal-primary", "evt-1", calendarsync.UpdateSpec{
		Event:            testMeetingEvent(),
		IncludeAttendees: false,
		NotifyAttendees:  true,
	})
	if err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}
	if got := (*captured)[0].query.Get("sendUpdates"); got != "all" {
		t.Errorf("sendUpdates = %q, want \"all\" for a reschedule that changes no attendee", got)
	}
}

func TestDeleteEventSentinels(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   error
	}{
		{"412 precondition", http.StatusPreconditionFailed, calendarsync.ErrPrecondition},
		{"404 not found", http.StatusNotFound, calendarsync.ErrNotFound},
		{"410 gone", http.StatusGone, calendarsync.ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := captureServer(t, []int{tc.status}, []string{`{"error": {}}`})
			defer srv.Close()

			err := testClient(srv).DeleteEvent(context.Background(), "cal-primary", "evt-1", `"etag-1"`, true)
			if !errors.Is(err, tc.want) {
				t.Errorf("DeleteEvent(%d) = %v, want %v sentinel", tc.status, err, tc.want)
			}
		})
	}
}

// MARK: - RespondToEvent

// rsvpEventJSON is the meeting the RSVP tests read back before patching: the
// owner (self) has not responded yet, the organizer accepted, a colleague is
// tentative.
const rsvpEventJSON = `{
	"id": "evt-1",
	"etag": "\"etag-1\"",
	"summary": "Design review",
	"start": {"dateTime": "2026-07-20T08:00:00Z"},
	"end": {"dateTime": "2026-07-20T09:00:00Z"},
	"organizer": {"email": "boss@example.com"},
	"attendees": [
		{"email": "boss@example.com", "displayName": "Boss", "responseStatus": "accepted"},
		{"email": "ME@example.com", "self": true, "responseStatus": "needsAction"},
		{"email": "maybe@example.com", "optional": true, "responseStatus": "tentative"}
	]
}`

func TestRespondToEvent(t *testing.T) {
	srv, captured := captureServer(t,
		[]int{http.StatusOK, http.StatusOK},
		[]string{rsvpEventJSON, `{"etag": "\"etag-2\""}`})
	defer srv.Close()

	err := testClient(srv).RespondToEvent(context.Background(), "cal-primary", "evt-1",
		calendar.OwnerRespTentative, true, "May be late")
	if err != nil {
		t.Fatalf("RespondToEvent: %v", err)
	}
	if len(*captured) != 2 {
		t.Fatalf("got %d requests, want GET + PATCH", len(*captured))
	}
	get, patch := (*captured)[0], (*captured)[1]
	if get.method != http.MethodGet || get.path != "/calendars/cal-primary/events/evt-1" {
		t.Errorf("read-back = %s %s", get.method, get.path)
	}
	if patch.method != http.MethodPatch || patch.path != "/calendars/cal-primary/events/evt-1" {
		t.Errorf("patch = %s %s", patch.method, patch.path)
	}
	if got := patch.query.Get("sendUpdates"); got != "all" {
		t.Errorf("sendUpdates = %q, want \"all\"", got)
	}
	// The patch carries ONLY the attendee array, with everyone's status
	// preserved and just the owner's entry changed (case-insensitive match).
	if len(patch.body) != 1 {
		t.Errorf("RSVP body keys = %v, want just attendees", patch.body)
	}
	wantAttendees := []any{
		map[string]any{"email": "boss@example.com", "displayName": "Boss", "responseStatus": "accepted"},
		map[string]any{"email": "ME@example.com", "responseStatus": "tentative", "comment": "May be late"},
		map[string]any{"email": "maybe@example.com", "optional": true, "responseStatus": "tentative"},
	}
	if !reflect.DeepEqual(patch.body["attendees"], wantAttendees) {
		t.Errorf("RSVP attendees = %v, want %v", patch.body["attendees"], wantAttendees)
	}
}

func TestRespondToEventSilent(t *testing.T) {
	srv, captured := captureServer(t,
		[]int{http.StatusOK, http.StatusOK},
		[]string{rsvpEventJSON, `{"etag": "\"etag-2\""}`})
	defer srv.Close()

	err := testClient(srv).RespondToEvent(context.Background(), "cal-primary", "evt-1",
		calendar.OwnerRespAccepted, false, "")
	if err != nil {
		t.Fatalf("RespondToEvent: %v", err)
	}
	patch := (*captured)[1]
	if got := patch.query.Get("sendUpdates"); got != "none" {
		t.Errorf("sendUpdates = %q, want \"none\"", got)
	}
	attendees, ok := patch.body["attendees"].([]any)
	if !ok || len(attendees) != 3 {
		t.Fatalf("RSVP attendees = %v", patch.body["attendees"])
	}
	owner, ok := attendees[1].(map[string]any)
	if !ok || owner["responseStatus"] != "accepted" {
		t.Errorf("owner entry = %v", attendees[1])
	}
	if _, hasComment := owner["comment"]; hasComment {
		t.Errorf("owner entry carries empty comment: %v", owner)
	}
}

func TestRespondToEventOwnerAbsent(t *testing.T) {
	srv, captured := captureServer(t, []int{http.StatusOK}, []string{`{
		"id": "evt-1",
		"start": {"dateTime": "2026-07-20T08:00:00Z"},
		"end": {"dateTime": "2026-07-20T09:00:00Z"},
		"attendees": [
			{"email": "boss@example.com", "responseStatus": "accepted"}
		]
	}`})
	defer srv.Close()

	// The owner has no attendee entry to update: logged no-op, no PATCH.
	err := testClient(srv).RespondToEvent(context.Background(), "cal-primary", "evt-1",
		calendar.OwnerRespAccepted, true, "")
	if err != nil {
		t.Fatalf("RespondToEvent: %v", err)
	}
	if len(*captured) != 1 {
		t.Errorf("got %d requests, want just the GET (no PATCH)", len(*captured))
	}
}

func TestRespondToEventInvalidState(t *testing.T) {
	c := NewWithToken(testOwnerEmail, "http://unused", "t", http.DefaultClient)
	if err := c.RespondToEvent(context.Background(), "cal", "evt-1", calendar.OwnerRespNone, false, ""); err == nil {
		t.Errorf("RespondToEvent(none) = nil error, want error")
	}
	if err := c.RespondToEvent(context.Background(), "cal", "evt-1", calendar.OwnerRespOrganizer, false, ""); err == nil {
		t.Errorf("RespondToEvent(organizer) = nil error, want error")
	}
}

func TestRespondToEventNotFound(t *testing.T) {
	srv, _ := captureServer(t, []int{http.StatusNotFound}, []string{`{"error": {"code": 404}}`})
	defer srv.Close()

	err := testClient(srv).RespondToEvent(context.Background(), "cal-primary", "evt-1",
		calendar.OwnerRespDeclined, false, "")
	if !errors.Is(err, calendarsync.ErrNotFound) {
		t.Errorf("RespondToEvent(404) = %v, want ErrNotFound sentinel", err)
	}
}

// MARK: - Recurrence round trip

// TestRecurrenceRoundTrip serializes neutral recurrences through the write
// mapping and parses the produced RRULE line back through the read path,
// asserting the sync engine sees identical series definitions on both
// directions.
func TestRecurrenceRoundTrip(t *testing.T) {
	start := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		rec      calendar.Recurrence
		wantLine string
	}{
		{
			name: "weekly until",
			rec: calendar.Recurrence{
				Pattern: calendar.RecurrencePattern{Type: "weekly", Interval: 1, DaysOfWeek: []string{"monday"}},
				Range:   calendar.RecurrenceRange{Type: "endDate", StartDate: "2026-07-20", EndDate: "2026-12-30"},
			},
			wantLine: wantRRuleLine,
		},
		{
			name: "monthly numbered",
			rec: calendar.Recurrence{
				Pattern: calendar.RecurrencePattern{Type: "absoluteMonthly", Interval: 2, DayOfMonth: 20},
				Range:   calendar.RecurrenceRange{Type: "numbered", StartDate: "2026-07-20", NumberOfOccurrences: 10},
			},
			wantLine: "RRULE:FREQ=MONTHLY;INTERVAL=2;COUNT=10;BYMONTHDAY=20",
		},
		{
			name: "relative monthly no end",
			rec: calendar.Recurrence{
				Pattern: calendar.RecurrencePattern{Type: "relativeMonthly", Interval: 1, DaysOfWeek: []string{"monday"}, Index: "third"},
				Range:   calendar.RecurrenceRange{Type: "noEnd", StartDate: "2026-07-20"},
			},
			wantLine: "RRULE:FREQ=MONTHLY;BYSETPOS=3;BYDAY=MO",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := recurrenceToGoogle(&tc.rec, "evt-rt")
			if len(lines) != 1 || lines[0] != tc.wantLine {
				t.Fatalf("recurrenceToGoogle = %v, want [%s]", lines, tc.wantLine)
			}
			back := recurrenceFromGoogle(lines, start, "evt-rt")
			if !reflect.DeepEqual(back, &tc.rec) {
				t.Errorf("round trip = %+v, want %+v", back, &tc.rec)
			}
		})
	}

	// nil and unmappable recurrences serialize to the empty list (clears the
	// series on PATCH).
	if got := recurrenceToGoogle(nil, "evt-rt"); !reflect.DeepEqual(got, []string{}) {
		t.Errorf("recurrenceToGoogle(nil) = %v, want []", got)
	}
	bad := &calendar.Recurrence{Pattern: calendar.RecurrencePattern{Type: "lunar"}}
	if got := recurrenceToGoogle(bad, "evt-rt"); !reflect.DeepEqual(got, []string{}) {
		t.Errorf("recurrenceToGoogle(unmappable) = %v, want []", got)
	}
}
