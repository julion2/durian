package graphcalendar

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
)

// testOwnerEmail is the mailbox owner of every test client.
const testOwnerEmail = "me@example.com"

// testClient builds a Client pointed at the given httptest server, with a
// static token so no OAuth path is exercised.
func testClient(srv *httptest.Server) *Client {
	return &Client{
		owner:      testOwnerEmail,
		httpClient: srv.Client(),
		baseURL:    srv.URL,
		tokenFn: func(context.Context) (string, error) {
			return "test-token", nil
		},
	}
}

// MARK: - EventToICS

func TestEventToICS(t *testing.T) {
	longDesc := strings.Repeat("x", 200)
	timed := calendar.Event{
		ID:           "AAA123",
		ICalUID:      "ical-uid-1",
		Subject:      "Lunch, with; team\nnotes",
		Location:     "Room 1",
		Description:  longDesc,
		Start:        time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC),
		End:          time.Date(2026, 7, 23, 11, 0, 0, 0, time.UTC),
		LastModified: time.Date(2026, 7, 1, 8, 30, 0, 0, time.UTC),
	}

	ics := calendar.EventToICS(timed, "-//durian//test//EN")

	for _, want := range []string{
		"BEGIN:VCALENDAR\r\n",
		"VERSION:2.0\r\n",
		"PRODID:-//durian//test//EN\r\n",
		"UID:ical-uid-1\r\n", // ICalUID preferred over ID
		"DTSTART:20260723T100000Z\r\n",
		"DTEND:20260723T110000Z\r\n",
		"DTSTAMP:20260701T083000Z\r\n",
		"LAST-MODIFIED:20260701T083000Z\r\n",
		`SUMMARY:Lunch\, with\; team\nnotes` + "\r\n",
		"LOCATION:Room 1\r\n",
		"END:VEVENT\r\nEND:VCALENDAR\r\n",
	} {
		if !strings.Contains(ics, want) {
			t.Errorf("timed ICS missing %q:\n%s", want, ics)
		}
	}

	// CRLF only: no bare LF may remain once CRLFs are removed.
	if strings.Contains(strings.ReplaceAll(ics, "\r\n", ""), "\n") {
		t.Errorf("ICS contains bare newlines:\n%s", ics)
	}
	if !strings.HasSuffix(ics, "\r\n") {
		t.Errorf("ICS does not end with CRLF")
	}

	// Folding: every physical line stays within 75 octets, and unfolding
	// restores the long DESCRIPTION.
	for _, line := range strings.Split(strings.TrimSuffix(ics, "\r\n"), "\r\n") {
		if len(line) > 75 {
			t.Errorf("physical line exceeds 75 octets (%d): %q", len(line), line)
		}
	}
	if !strings.Contains(ics, "\r\n ") {
		t.Errorf("long DESCRIPTION was not folded:\n%s", ics)
	}
	unfolded := strings.ReplaceAll(ics, "\r\n ", "")
	if !strings.Contains(unfolded, "DESCRIPTION:"+longDesc) {
		t.Errorf("unfolding did not restore DESCRIPTION:\n%s", unfolded)
	}

	allDay := calendar.Event{
		ID:      "BBB456",
		Subject: "Holiday",
		AllDay:  true,
		Start:   time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC),
		End:     time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC), // Exclusive end
	}

	ics = calendar.EventToICS(allDay, "-//durian//test//EN")

	for _, want := range []string{
		"UID:BBB456\r\n", // Falls back to ID when ICalUID is empty
		"DTSTART;VALUE=DATE:20260723\r\n",
		"DTEND;VALUE=DATE:20260724\r\n",
		"SUMMARY:Holiday\r\n",
	} {
		if !strings.Contains(ics, want) {
			t.Errorf("all-day ICS missing %q:\n%s", want, ics)
		}
	}
	if strings.Contains(ics, "DTSTART:") {
		t.Errorf("all-day ICS must not contain a timed DTSTART:\n%s", ics)
	}
}

// MARK: - FetchEvents

func TestFetchEvents(t *testing.T) {
	var preferHeaders []string
	var srv *httptest.Server

	mux := http.NewServeMux()
	mux.HandleFunc("/me/calendars/cal1/calendarView", func(w http.ResponseWriter, r *http.Request) {
		preferHeaders = append(preferHeaders, r.Header.Get("Prefer"))
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want bearer test-token", got)
		}

		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `{
				"value": [{
					"id": "ev2",
					"iCalUId": "ical-2",
					"subject": "Holiday",
					"start": {"dateTime": "2026-07-24T00:00:00.0000000", "timeZone": "UTC"},
					"end": {"dateTime": "2026-07-25T00:00:00.0000000", "timeZone": "UTC"},
					"isAllDay": true,
					"location": {"displayName": ""},
					"bodyPreview": "",
					"lastModifiedDateTime": "2026-07-19T12:00:00Z"
				}]
			}`)
			return
		}
		fmt.Fprintf(w, `{
			"value": [{
				"id": "ev1",
				"iCalUId": "ical-1",
				"subject": "Standup",
				"start": {"dateTime": "2026-07-23T10:00:00.0000000", "timeZone": "UTC"},
				"end": {"dateTime": "2026-07-23T10:30:00.0000000", "timeZone": "UTC"},
				"isAllDay": false,
				"location": {"displayName": "Zoom"},
				"bodyPreview": "daily sync",
				"lastModifiedDateTime": "2026-07-20T09:00:00Z"
			}],
			"@odata.nextLink": %q
		}`, srv.URL+"/me/calendars/cal1/calendarView?page=2")
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	c := testClient(srv)
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	events, err := c.FetchEvents(context.Background(), "cal1", from, to)
	if err != nil {
		t.Fatalf("FetchEvents: %v", err)
	}

	if len(preferHeaders) != 2 {
		t.Fatalf("expected 2 pages fetched, got %d", len(preferHeaders))
	}
	for i, h := range preferHeaders {
		if h != `outlook.timezone="UTC"` {
			t.Errorf("page %d Prefer header = %q, want outlook.timezone UTC", i+1, h)
		}
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 events across pages, got %d", len(events))
	}

	ev1 := events[0]
	if ev1.ID != "ev1" || ev1.ICalUID != "ical-1" || ev1.Subject != "Standup" ||
		ev1.Location != "Zoom" || ev1.Description != "daily sync" || ev1.AllDay {
		t.Errorf("ev1 fields mismatched: %+v", ev1)
	}
	if want := time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC); !ev1.Start.Equal(want) {
		t.Errorf("ev1.Start = %v, want %v", ev1.Start, want)
	}
	if want := time.Date(2026, 7, 23, 10, 30, 0, 0, time.UTC); !ev1.End.Equal(want) {
		t.Errorf("ev1.End = %v, want %v", ev1.End, want)
	}
	if want := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC); !ev1.LastModified.Equal(want) {
		t.Errorf("ev1.LastModified = %v, want %v", ev1.LastModified, want)
	}

	ev2 := events[1]
	if !ev2.AllDay {
		t.Errorf("ev2.AllDay = false, want true")
	}
	if want := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC); !ev2.Start.Equal(want) {
		t.Errorf("ev2.Start = %v, want %v", ev2.Start, want)
	}
	if want := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC); !ev2.End.Equal(want) {
		t.Errorf("ev2.End = %v, want %v", ev2.End, want)
	}
}

// MARK: - Meeting metadata (Stage 1, read-only)

// meetingGraphJSON is a Graph /events resource for an online meeting with two
// attendees, an organizer, and the account owner's accepted RSVP.
const meetingGraphJSON = `{
	"id": "meet1",
	"iCalUId": "ical-meet1",
	"subject": "Design review",
	"body": {"contentType": "text", "content": "please review the drafts"},
	"bodyPreview": "please review the drafts",
	"start": {"dateTime": "2026-07-23T10:00:00.0000000", "timeZone": "UTC"},
	"end": {"dateTime": "2026-07-23T11:00:00.0000000", "timeZone": "UTC"},
	"isAllDay": false,
	"location": {"displayName": "Teams"},
	"type": "singleInstance",
	"changeKey": "ck-meet1",
	"lastModifiedDateTime": "2026-07-20T09:00:00Z",
	"attendees": [
		{
			"type": "required",
			"status": {"response": "accepted", "time": "2026-07-19T08:00:00Z"},
			"emailAddress": {"name": "Alice Example", "address": "alice@example.com"}
		},
		{
			"type": "optional",
			"status": {"response": "declined", "time": "2026-07-19T09:00:00Z"},
			"emailAddress": {"name": "Bob Example", "address": "bob@example.com"}
		}
	],
	"organizer": {"emailAddress": {"name": "Olivia Organizer", "address": "olivia@example.com"}},
	"responseStatus": {"response": "accepted", "time": "2026-07-19T10:00:00Z"},
	"isOnlineMeeting": true,
	"onlineMeeting": {"joinUrl": "https://teams.microsoft.com/l/meetup-join/abc123"},
	"isCancelled": false,
	"isOrganizer": false
}`

// meetingEvent parses meetingGraphJSON into an Event.
func meetingEvent(t *testing.T) calendar.Event {
	t.Helper()
	var ge graphEvent
	if err := json.Unmarshal([]byte(meetingGraphJSON), &ge); err != nil {
		t.Fatalf("unmarshal meeting JSON: %v", err)
	}
	ev, ok := eventFromGraph(ge)
	if !ok {
		t.Fatalf("eventFromGraph returned ok=false")
	}
	return ev
}

func TestEventFromGraphMeetingMetadata(t *testing.T) {
	ev := meetingEvent(t)

	wantAttendees := []calendar.Attendee{
		{Name: "Alice Example", Email: "alice@example.com", Type: "required", Response: "accepted"},
		{Name: "Bob Example", Email: "bob@example.com", Type: "optional", Response: "declined"},
	}
	if !reflect.DeepEqual(ev.Attendees, wantAttendees) {
		t.Errorf("Attendees = %+v, want %+v", ev.Attendees, wantAttendees)
	}
	if ev.Organizer == nil || ev.Organizer.Name != "Olivia Organizer" || ev.Organizer.Email != "olivia@example.com" {
		t.Errorf("Organizer = %+v, want Olivia Organizer <olivia@example.com>", ev.Organizer)
	}
	if !ev.IsOnlineMeeting {
		t.Errorf("IsOnlineMeeting = false, want true")
	}
	if want := "https://teams.microsoft.com/l/meetup-join/abc123"; ev.OnlineMeetingURL != want {
		t.Errorf("OnlineMeetingURL = %q, want %q", ev.OnlineMeetingURL, want)
	}
	if ev.OwnerResponse != calendar.OwnerRespAccepted {
		t.Errorf("OwnerResponse = %q, want accepted", ev.OwnerResponse)
	}
	if ev.IsCancelled {
		t.Errorf("IsCancelled = true, want false")
	}
	if ev.IsOrganizer {
		t.Errorf("IsOrganizer = true, want false")
	}
}

func TestEventFromGraphMeetingMetadataAbsent(t *testing.T) {
	// A plain appointment: no attendees/organizer/onlineMeeting keys at all
	// (calendarView results and simple events) must parse to zero values.
	var ge graphEvent
	if err := json.Unmarshal([]byte(`{
		"id": "plain1",
		"subject": "Dentist",
		"start": {"dateTime": "2026-07-23T10:00:00.0000000", "timeZone": "UTC"},
		"end": {"dateTime": "2026-07-23T11:00:00.0000000", "timeZone": "UTC"}
	}`), &ge); err != nil {
		t.Fatalf("unmarshal plain JSON: %v", err)
	}
	ev, ok := eventFromGraph(ge)
	if !ok {
		t.Fatalf("eventFromGraph returned ok=false")
	}
	if len(ev.Attendees) != 0 || ev.Organizer != nil || ev.IsOnlineMeeting ||
		ev.OnlineMeetingURL != "" || ev.IsCancelled || ev.OwnerResponse != calendar.OwnerRespNone {
		t.Errorf("plain event carries meeting metadata: %+v", ev)
	}
}

func TestEventFromGraphLegacyOnlineMeetingURL(t *testing.T) {
	// onlineMeeting null but the legacy onlineMeetingUrl set: fall back.
	var ge graphEvent
	if err := json.Unmarshal([]byte(`{
		"id": "legacy1",
		"subject": "Old-style meeting",
		"start": {"dateTime": "2026-07-23T10:00:00.0000000", "timeZone": "UTC"},
		"end": {"dateTime": "2026-07-23T11:00:00.0000000", "timeZone": "UTC"},
		"isOnlineMeeting": true,
		"onlineMeeting": null,
		"onlineMeetingUrl": "https://meet.example.com/legacy"
	}`), &ge); err != nil {
		t.Fatalf("unmarshal legacy JSON: %v", err)
	}
	ev, ok := eventFromGraph(ge)
	if !ok {
		t.Fatalf("eventFromGraph returned ok=false")
	}
	if want := "https://meet.example.com/legacy"; ev.OnlineMeetingURL != want {
		t.Errorf("OnlineMeetingURL = %q, want legacy fallback %q", ev.OnlineMeetingURL, want)
	}
}

func TestEventContentHashMeetingMetadata(t *testing.T) {
	base := meetingEvent(t)
	baseHash := calendar.EventContentHash(base, testOwnerEmail)

	// Volatile bookkeeping churn must NOT move the hash.
	churned := base
	churned.ETag = "ck-other"
	churned.LastModified = base.LastModified.Add(time.Hour)
	if calendar.EventContentHash(churned, testOwnerEmail) != baseHash {
		t.Errorf("changeKey/lastModified churn moved the hash")
	}

	// Attendee ordering must not matter: the hash sorts attendees itself.
	swapped := base
	swapped.Attendees = []calendar.Attendee{base.Attendees[1], base.Attendees[0]}
	if calendar.EventContentHash(swapped, testOwnerEmail) != baseHash {
		t.Errorf("attendee order moved the hash")
	}

	// Every meaningful meeting change must move the hash.
	changes := map[string]func(*calendar.Event){
		"attendee response": func(e *calendar.Event) {
			attendees := append([]calendar.Attendee(nil), e.Attendees...)
			attendees[1].Response = "accepted"
			e.Attendees = attendees
		},
		"attendee added": func(e *calendar.Event) {
			e.Attendees = append(append([]calendar.Attendee(nil), e.Attendees...),
				calendar.Attendee{Name: "Carol", Email: "carol@example.com", Type: "required", Response: "notResponded"})
		},
		"attendee removed": func(e *calendar.Event) {
			e.Attendees = e.Attendees[:1]
		},
		"cancelled": func(e *calendar.Event) {
			e.IsCancelled = true
		},
		"join url": func(e *calendar.Event) {
			e.OnlineMeetingURL = "https://teams.microsoft.com/l/meetup-join/other"
		},
		"organizer": func(e *calendar.Event) {
			e.Organizer = &calendar.Person{Name: "New Org", Email: "neworg@example.com"}
		},
	}
	for name, change := range changes {
		ev := base
		change(&ev)
		if calendar.EventContentHash(ev, testOwnerEmail) == baseHash {
			t.Errorf("%s change did not move the hash", name)
		}
	}
}

func TestEventContentHashExcludesOwnerState(t *testing.T) {
	// The owner's own RSVP — the responseStatus enum AND the owner's own
	// attendee entry — must never move the content hash: an owner RSVP is
	// handled by the ActionRsvp three-way diff, not as a content change.
	base := meetingEvent(t)
	base.Attendees = append(append([]calendar.Attendee(nil), base.Attendees...),
		calendar.Attendee{Name: "Me", Email: "ME@example.com", Type: "required", Response: "notResponded"})
	baseHash := calendar.EventContentHash(base, testOwnerEmail)

	responded := base
	responded.OwnerResponse = calendar.OwnerRespDeclined
	attendees := append([]calendar.Attendee(nil), base.Attendees...)
	attendees[len(attendees)-1].Response = "declined"
	responded.Attendees = attendees
	if calendar.EventContentHash(responded, testOwnerEmail) != baseHash {
		t.Error("owner RSVP (responseStatus + own attendee entry, case-insensitive) moved the hash")
	}

	// Removing the owner's attendee entry entirely must not move it either.
	withoutOwner := base
	withoutOwner.Attendees = base.Attendees[:len(base.Attendees)-1]
	if calendar.EventContentHash(withoutOwner, testOwnerEmail) != baseHash {
		t.Error("presence of the owner's own attendee entry moved the hash")
	}

	// Another attendee's response still moves the hash.
	other := base
	attendees = append([]calendar.Attendee(nil), base.Attendees...)
	attendees[0].Response = "declined"
	other.Attendees = attendees
	if calendar.EventContentHash(other, testOwnerEmail) == baseHash {
		t.Error("another attendee's response change did not move the hash")
	}
}
