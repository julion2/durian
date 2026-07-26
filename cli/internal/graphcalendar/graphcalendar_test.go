package graphcalendar

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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
	timed := Event{
		ID:           "AAA123",
		ICalUID:      "ical-uid-1",
		Subject:      "Lunch, with; team\nnotes",
		Location:     "Room 1",
		Description:  longDesc,
		Start:        time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC),
		End:          time.Date(2026, 7, 23, 11, 0, 0, 0, time.UTC),
		LastModified: time.Date(2026, 7, 1, 8, 30, 0, 0, time.UTC),
	}

	ics := EventToICS(timed, "-//durian//test//EN")

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

	allDay := Event{
		ID:      "BBB456",
		Subject: "Holiday",
		AllDay:  true,
		Start:   time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC),
		End:     time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC), // Exclusive end
	}

	ics = EventToICS(allDay, "-//durian//test//EN")

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

// MARK: - Export

func TestExport(t *testing.T) {
	calendarView := func(events ...map[string]any) string {
		body, err := json.Marshal(map[string]any{"value": events})
		if err != nil {
			t.Fatalf("marshal calendarView: %v", err)
		}
		return string(body)
	}
	event := func(id, subject, start, end string) map[string]any {
		return map[string]any{
			"id":                   id,
			"iCalUId":              "ical-" + id,
			"subject":              subject,
			"start":                map[string]string{"dateTime": start, "timeZone": "UTC"},
			"end":                  map[string]string{"dateTime": end, "timeZone": "UTC"},
			"isAllDay":             false,
			"location":             map[string]string{"displayName": "HQ"},
			"bodyPreview":          "agenda",
			"lastModifiedDateTime": "2026-07-20T09:00:00Z",
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/me/calendars", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"value": [
			{"id": "cal1", "name": "Work/Team", "hexColor": "#0b8043"},
			{"id": "cal2", "name": "Personal"}
		]}`)
	})
	mux.HandleFunc("/me/calendars/cal1/calendarView", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, calendarView(
			event("ev/1", "Planning", "2026-07-23T10:00:00.0000000", "2026-07-23T11:00:00.0000000"),
			event("ev2", "Review", "2026-07-24T14:00:00.0000000", "2026-07-24T15:00:00.0000000"),
		))
	})
	mux.HandleFunc("/me/calendars/cal2/calendarView", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, calendarView(
			event("ev3", "Dentist", "2026-07-25T09:00:00.0000000", "2026-07-25T09:30:00.0000000"),
		))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := testClient(srv)
	outDir := t.TempDir()
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	stats, err := Export(context.Background(), c, outDir, from, to, nil)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if stats.Calendars != 2 || stats.Events != 3 {
		t.Errorf("stats = %+v, want 2 calendars / 3 events", stats)
	}

	// Directory per calendar, with the path separator sanitized.
	for _, dir := range []string{"Work_Team", "Personal"} {
		info, err := os.Stat(filepath.Join(outDir, dir))
		if err != nil || !info.IsDir() {
			t.Fatalf("expected calendar dir %s: err=%v", dir, err)
		}
	}

	// displayname carries the original (unsanitized) calendar name.
	displayname, err := os.ReadFile(filepath.Join(outDir, "Work_Team", "displayname"))
	if err != nil {
		t.Fatalf("read displayname: %v", err)
	}
	if got := strings.TrimSpace(string(displayname)); got != "Work/Team" {
		t.Errorf("displayname = %q, want Work/Team", got)
	}

	// One .ics per event, sanitized event id as filename.
	for _, f := range []struct{ dir, name, uid string }{
		{"Work_Team", "ev_1.ics", "UID:ical-ev/1"},
		{"Work_Team", "ev2.ics", "UID:ical-ev2"},
		{"Personal", "ev3.ics", "UID:ical-ev3"},
	} {
		body, err := os.ReadFile(filepath.Join(outDir, f.dir, f.name))
		if err != nil {
			t.Fatalf("read %s/%s: %v", f.dir, f.name, err)
		}
		ics := string(body)
		for _, want := range []string{"BEGIN:VCALENDAR", "BEGIN:VEVENT", f.uid, "END:VEVENT", "END:VCALENDAR"} {
			if !strings.Contains(ics, want) {
				t.Errorf("%s/%s missing %q:\n%s", f.dir, f.name, want, ics)
			}
		}
	}

	// color file: written for a calendar with a hexColor, absent otherwise.
	if got, err := os.ReadFile(filepath.Join(outDir, "Work_Team", "color")); err != nil {
		t.Errorf("expected color file for Work/Team: %v", err)
	} else if strings.TrimSpace(string(got)) != "#0b8043" {
		t.Errorf("color = %q, want #0b8043", strings.TrimSpace(string(got)))
	}
	if _, err := os.Stat(filepath.Join(outDir, "Personal", "color")); !os.IsNotExist(err) {
		t.Errorf("Personal has no hexColor; color file should be absent (err=%v)", err)
	}

	// include filter: only the named calendar is exported.
	incDir := t.TempDir()
	incStats, err := Export(context.Background(), c, incDir, from, to, []string{"Personal"})
	if err != nil {
		t.Fatalf("Export with include: %v", err)
	}
	if incStats.Calendars != 1 || incStats.Events != 1 {
		t.Errorf("include stats = %+v, want 1 calendar / 1 event", incStats)
	}
	if _, err := os.Stat(filepath.Join(incDir, "Personal")); err != nil {
		t.Errorf("included calendar not exported: %v", err)
	}
	if _, err := os.Stat(filepath.Join(incDir, "Work_Team")); !os.IsNotExist(err) {
		t.Errorf("Work_Team not in include list but was exported (err=%v)", err)
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
func meetingEvent(t *testing.T) Event {
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

	wantAttendees := []Attendee{
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
	if ev.OwnerResponse != OwnerRespAccepted {
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
		ev.OnlineMeetingURL != "" || ev.IsCancelled || ev.OwnerResponse != OwnerRespNone {
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
	baseHash := eventContentHash(base, testOwnerEmail)

	// Volatile bookkeeping churn must NOT move the hash.
	churned := base
	churned.ChangeKey = "ck-other"
	churned.LastModified = base.LastModified.Add(time.Hour)
	if eventContentHash(churned, testOwnerEmail) != baseHash {
		t.Errorf("changeKey/lastModified churn moved the hash")
	}

	// Attendee ordering must not matter: the hash sorts attendees itself.
	swapped := base
	swapped.Attendees = []Attendee{base.Attendees[1], base.Attendees[0]}
	if eventContentHash(swapped, testOwnerEmail) != baseHash {
		t.Errorf("attendee order moved the hash")
	}

	// Every meaningful meeting change must move the hash.
	changes := map[string]func(*Event){
		"attendee response": func(e *Event) {
			attendees := append([]Attendee(nil), e.Attendees...)
			attendees[1].Response = "accepted"
			e.Attendees = attendees
		},
		"attendee added": func(e *Event) {
			e.Attendees = append(append([]Attendee(nil), e.Attendees...),
				Attendee{Name: "Carol", Email: "carol@example.com", Type: "required", Response: "notResponded"})
		},
		"attendee removed": func(e *Event) {
			e.Attendees = e.Attendees[:1]
		},
		"cancelled": func(e *Event) {
			e.IsCancelled = true
		},
		"join url": func(e *Event) {
			e.OnlineMeetingURL = "https://teams.microsoft.com/l/meetup-join/other"
		},
		"organizer": func(e *Event) {
			e.Organizer = &Person{Name: "New Org", Email: "neworg@example.com"}
		},
	}
	for name, change := range changes {
		ev := base
		change(&ev)
		if eventContentHash(ev, testOwnerEmail) == baseHash {
			t.Errorf("%s change did not move the hash", name)
		}
	}
}

func TestEventContentHashExcludesOwnerState(t *testing.T) {
	// The owner's own RSVP — the responseStatus enum AND the owner's own
	// attendee entry — must never move the content hash: an owner RSVP is
	// handled by the ActionRsvp three-way diff, not as a content change.
	base := meetingEvent(t)
	base.Attendees = append(append([]Attendee(nil), base.Attendees...),
		Attendee{Name: "Me", Email: "ME@example.com", Type: "required", Response: "notResponded"})
	baseHash := eventContentHash(base, testOwnerEmail)

	responded := base
	responded.OwnerResponse = OwnerRespDeclined
	attendees := append([]Attendee(nil), base.Attendees...)
	attendees[len(attendees)-1].Response = "declined"
	responded.Attendees = attendees
	if eventContentHash(responded, testOwnerEmail) != baseHash {
		t.Error("owner RSVP (responseStatus + own attendee entry, case-insensitive) moved the hash")
	}

	// Removing the owner's attendee entry entirely must not move it either.
	withoutOwner := base
	withoutOwner.Attendees = base.Attendees[:len(base.Attendees)-1]
	if eventContentHash(withoutOwner, testOwnerEmail) != baseHash {
		t.Error("presence of the owner's own attendee entry moved the hash")
	}

	// Another attendee's response still moves the hash.
	other := base
	attendees = append([]Attendee(nil), base.Attendees...)
	attendees[0].Response = "declined"
	other.Attendees = attendees
	if eventContentHash(other, testOwnerEmail) == baseHash {
		t.Error("another attendee's response change did not move the hash")
	}
}
