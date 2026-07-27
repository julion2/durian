package graphcalendar

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// MARK: - Round-trip

func TestICalRoundTripTimed(t *testing.T) {
	orig := Event{
		ID:           "AAA123",
		ICalUID:      "ical-uid-1",
		Subject:      "Lunch, with; team\nnotes",
		Location:     "Room 1",
		Description:  "bring the Q3 numbers",
		Start:        time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC),
		End:          time.Date(2026, 7, 23, 11, 0, 0, 0, time.UTC),
		LastModified: time.Date(2026, 7, 1, 8, 30, 0, 0, time.UTC),
	}

	data, err := EventToICal(orig)
	if err != nil {
		t.Fatalf("EventToICal: %v", err)
	}
	ics := string(data)

	for _, want := range []string{
		"BEGIN:VCALENDAR\r\n",
		"BEGIN:VEVENT\r\n",
		"UID:ical-uid-1\r\n",
		"DTSTART:20260723T100000Z\r\n",
		"DTEND:20260723T110000Z\r\n",
		"DTSTAMP:20260701T083000Z\r\n",
		"LAST-MODIFIED:20260701T083000Z\r\n",
		"END:VEVENT\r\n",
	} {
		if !strings.Contains(ics, want) {
			t.Errorf("ICS missing %q:\n%s", want, ics)
		}
	}
	if strings.Contains(strings.ReplaceAll(ics, "\r\n", ""), "\n") {
		t.Errorf("ICS contains bare newlines:\n%s", ics)
	}

	got, err := ICalToEvent(data, "")
	if err != nil {
		t.Fatalf("ICalToEvent: %v", err)
	}

	if got.ICalUID != orig.ICalUID {
		t.Errorf("ICalUID = %q, want %q", got.ICalUID, orig.ICalUID)
	}
	if got.Subject != orig.Subject {
		t.Errorf("Subject = %q, want %q", got.Subject, orig.Subject)
	}
	if got.Location != orig.Location {
		t.Errorf("Location = %q, want %q", got.Location, orig.Location)
	}
	if got.Description != orig.Description {
		t.Errorf("Description = %q, want %q", got.Description, orig.Description)
	}
	if !got.Start.Equal(orig.Start) {
		t.Errorf("Start = %v, want %v", got.Start, orig.Start)
	}
	if !got.End.Equal(orig.End) {
		t.Errorf("End = %v, want %v", got.End, orig.End)
	}
	if got.AllDay {
		t.Errorf("AllDay = true, want false")
	}
	if !got.LastModified.Equal(orig.LastModified) {
		t.Errorf("LastModified = %v, want %v", got.LastModified, orig.LastModified)
	}
	if got.Recurrence != nil {
		t.Errorf("Recurrence = %+v, want nil", got.Recurrence)
	}
}

func TestICalRoundTripAllDay(t *testing.T) {
	orig := Event{
		ID:      "BBB456",
		Subject: "Holiday",
		AllDay:  true,
		Start:   time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC),
		End:     time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC), // Exclusive end
	}

	data, err := EventToICal(orig)
	if err != nil {
		t.Fatalf("EventToICal: %v", err)
	}
	ics := string(data)

	for _, want := range []string{
		"UID:BBB456\r\n", // Falls back to ID when ICalUID is empty
		"DTSTART;VALUE=DATE:20260723\r\n",
		"DTEND;VALUE=DATE:20260724\r\n",
	} {
		if !strings.Contains(ics, want) {
			t.Errorf("all-day ICS missing %q:\n%s", want, ics)
		}
	}
	if strings.Contains(ics, "DTSTART:") {
		t.Errorf("all-day ICS must not contain a timed DTSTART:\n%s", ics)
	}

	got, err := ICalToEvent(data, "")
	if err != nil {
		t.Fatalf("ICalToEvent: %v", err)
	}
	if !got.AllDay {
		t.Errorf("AllDay = false, want true")
	}
	if got.ICalUID != "BBB456" {
		t.Errorf("ICalUID = %q, want BBB456", got.ICalUID)
	}
	if !got.Start.Equal(orig.Start) {
		t.Errorf("Start = %v, want %v", got.Start, orig.Start)
	}
	if !got.End.Equal(orig.End) {
		t.Errorf("End = %v, want %v", got.End, orig.End)
	}
}

func TestICalRoundTripAllDayNonMidnight(t *testing.T) {
	// An all-day event whose Start/End carry stray times (a hand-edited file,
	// or a zone-shifted source) serializes as pure DATE values and parses back
	// at the midnight day boundaries — the .ics can never carry a timed
	// all-day boundary.
	orig := Event{
		ID:      "DDD000",
		Subject: "Offsite",
		AllDay:  true,
		Start:   time.Date(2026, 7, 23, 9, 30, 0, 0, time.UTC),
		End:     time.Date(2026, 7, 25, 18, 0, 0, 0, time.UTC),
	}

	data, err := EventToICal(orig)
	if err != nil {
		t.Fatalf("EventToICal: %v", err)
	}
	ics := string(data)
	for _, want := range []string{
		"DTSTART;VALUE=DATE:20260723\r\n",
		"DTEND;VALUE=DATE:20260725\r\n",
	} {
		if !strings.Contains(ics, want) {
			t.Errorf("all-day ICS missing %q:\n%s", want, ics)
		}
	}

	got, err := ICalToEvent(data, "")
	if err != nil {
		t.Fatalf("ICalToEvent: %v", err)
	}
	if !got.AllDay {
		t.Error("AllDay = false, want true")
	}
	if !got.Start.Equal(time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("Start = %v, want 2026-07-23 midnight UTC", got.Start)
	}
	if !got.End.Equal(time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("End = %v, want 2026-07-25 midnight UTC", got.End)
	}
}

func TestICalRoundTripWeeklyRecurrence(t *testing.T) {
	orig := Event{
		ID:      "CCC789",
		ICalUID: "ical-uid-3",
		Subject: "Team sync",
		Start:   time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC),
		End:     time.Date(2026, 7, 20, 9, 30, 0, 0, time.UTC),
		Recurrence: &Recurrence{
			Pattern: RecurrencePattern{
				Type:       "weekly",
				Interval:   2,
				DaysOfWeek: []string{"monday", "wednesday"},
			},
			Range: RecurrenceRange{
				Type:      "endDate",
				StartDate: "2026-07-20",
				EndDate:   "2026-11-02",
			},
		},
	}

	data, err := EventToICal(orig)
	if err != nil {
		t.Fatalf("EventToICal: %v", err)
	}
	ics := string(data)

	// rrule-go emits parts in FREQ;INTERVAL;UNTIL;BYDAY order. UNTIL is the
	// end (23:59:59Z) of Graph's inclusive endDate.
	wantRRule := "RRULE:FREQ=WEEKLY;INTERVAL=2;UNTIL=20261102T235959Z;BYDAY=MO,WE\r\n"
	if !strings.Contains(ics, wantRRule) {
		t.Errorf("ICS missing %q:\n%s", wantRRule, ics)
	}

	got, err := ICalToEvent(data, "")
	if err != nil {
		t.Fatalf("ICalToEvent: %v", err)
	}
	if got.Recurrence == nil {
		t.Fatalf("Recurrence = nil, want weekly recurrence")
	}
	rec := got.Recurrence
	if rec.Pattern.Type != "weekly" {
		t.Errorf("Pattern.Type = %q, want weekly", rec.Pattern.Type)
	}
	if rec.Pattern.Interval != 2 {
		t.Errorf("Pattern.Interval = %d, want 2", rec.Pattern.Interval)
	}
	if want := []string{"monday", "wednesday"}; !reflect.DeepEqual(rec.Pattern.DaysOfWeek, want) {
		t.Errorf("Pattern.DaysOfWeek = %v, want %v", rec.Pattern.DaysOfWeek, want)
	}
	if rec.Range.Type != "endDate" {
		t.Errorf("Range.Type = %q, want endDate", rec.Range.Type)
	}
	if rec.Range.EndDate != "2026-11-02" {
		t.Errorf("Range.EndDate = %q, want 2026-11-02", rec.Range.EndDate)
	}
	if rec.Range.StartDate != "2026-07-20" {
		t.Errorf("Range.StartDate = %q, want 2026-07-20", rec.Range.StartDate)
	}
}

func TestICalRoundTripNumberedDaily(t *testing.T) {
	orig := Event{
		ID:      "DDD012",
		ICalUID: "ical-uid-4",
		Subject: "Standup",
		Start:   time.Date(2026, 8, 3, 8, 0, 0, 0, time.UTC),
		End:     time.Date(2026, 8, 3, 8, 15, 0, 0, time.UTC),
		Recurrence: &Recurrence{
			Pattern: RecurrencePattern{Type: "daily", Interval: 1},
			Range:   RecurrenceRange{Type: "numbered", NumberOfOccurrences: 10},
		},
	}

	data, err := EventToICal(orig)
	if err != nil {
		t.Fatalf("EventToICal: %v", err)
	}
	if want := "RRULE:FREQ=DAILY;COUNT=10\r\n"; !strings.Contains(string(data), want) {
		t.Errorf("ICS missing %q:\n%s", want, data)
	}

	got, err := ICalToEvent(data, "")
	if err != nil {
		t.Fatalf("ICalToEvent: %v", err)
	}
	if got.Recurrence == nil {
		t.Fatalf("Recurrence = nil, want daily recurrence")
	}
	if got.Recurrence.Pattern.Type != "daily" || got.Recurrence.Pattern.Interval != 1 {
		t.Errorf("Pattern = %+v, want daily interval 1", got.Recurrence.Pattern)
	}
	if got.Recurrence.Range.Type != "numbered" || got.Recurrence.Range.NumberOfOccurrences != 10 {
		t.Errorf("Range = %+v, want numbered 10", got.Recurrence.Range)
	}
}

// MARK: - FetchMasterEvents

func TestFetchMasterEvents(t *testing.T) {
	var preferHeaders []string
	var srv *httptest.Server

	masterEvent := func(id, typ string) string {
		return fmt.Sprintf(`{
			"id": %q,
			"iCalUId": "ical-%s",
			"subject": "Event %s",
			"body": {"contentType": "text", "content": "full body %s"},
			"bodyPreview": "preview %s",
			"start": {"dateTime": "2026-07-23T10:00:00.0000000", "timeZone": "UTC"},
			"end": {"dateTime": "2026-07-23T11:00:00.0000000", "timeZone": "UTC"},
			"isAllDay": false,
			"location": {"displayName": "HQ"},
			"type": %q,
			"changeKey": "ck-%s",
			"lastModifiedDateTime": "2026-07-20T09:00:00Z"
		}`, id, id, id, id, id, typ, id)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/me/calendars/cal1/events", func(w http.ResponseWriter, r *http.Request) {
		preferHeaders = append(preferHeaders, r.Header.Get("Prefer"))
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want bearer test-token", got)
		}

		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			// Page 2: a seriesMaster with a weekly recurrence, and an
			// occurrence that must be skipped.
			fmt.Fprintf(w, `{"value": [
				{
					"id": "master1",
					"iCalUId": "ical-master1",
					"subject": "Weekly sync",
					"body": {"contentType": "html", "content": "<p>hi</p>"},
					"bodyPreview": "hi",
					"start": {"dateTime": "2026-07-20T09:00:00.0000000", "timeZone": "UTC"},
					"end": {"dateTime": "2026-07-20T09:30:00.0000000", "timeZone": "UTC"},
					"isAllDay": false,
					"location": {"displayName": "Zoom"},
					"type": "seriesMaster",
					"changeKey": "ck-master1",
					"recurrence": {
						"pattern": {
							"type": "weekly",
							"interval": 2,
							"daysOfWeek": ["monday", "wednesday"],
							"firstDayOfWeek": "sunday"
						},
						"range": {
							"type": "endDate",
							"startDate": "2026-07-20",
							"endDate": "2026-11-02"
						}
					},
					"lastModifiedDateTime": "2026-07-21T10:00:00Z"
				},
				%s
			]}`, masterEvent("occ1", "occurrence"))
			return
		}
		fmt.Fprintf(w, `{"value": [%s], "@odata.nextLink": %q}`,
			masterEvent("single1", "singleInstance"),
			srv.URL+"/me/calendars/cal1/events?page=2")
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	c := testClient(srv)
	events, err := c.FetchMasterEvents(context.Background(), "cal1")
	if err != nil {
		t.Fatalf("FetchMasterEvents: %v", err)
	}

	if len(preferHeaders) != 2 {
		t.Fatalf("expected 2 pages fetched, got %d", len(preferHeaders))
	}
	for i, h := range preferHeaders {
		if h != `outlook.timezone="UTC", IdType="ImmutableId"` {
			t.Errorf("page %d Prefer header = %q, want outlook.timezone UTC + immutable ids", i+1, h)
		}
	}

	// The occurrence must be skipped: one singleInstance + one seriesMaster.
	if len(events) != 2 {
		t.Fatalf("expected 2 events (occurrence skipped), got %d: %+v", len(events), events)
	}

	single := events[0]
	if single.ID != "single1" || single.Type != "singleInstance" || single.ChangeKey != "ck-single1" {
		t.Errorf("single fields mismatched: %+v", single)
	}
	if single.Description != "full body single1" {
		t.Errorf("single.Description = %q, want full text body preferred over preview", single.Description)
	}
	if single.Recurrence != nil {
		t.Errorf("single.Recurrence = %+v, want nil", single.Recurrence)
	}

	master := events[1]
	if master.ID != "master1" || master.Type != "seriesMaster" || master.ChangeKey != "ck-master1" {
		t.Errorf("master fields mismatched: %+v", master)
	}
	// HTML body: fall back to bodyPreview.
	if master.Description != "hi" {
		t.Errorf("master.Description = %q, want bodyPreview fallback for html body", master.Description)
	}
	if master.Recurrence == nil {
		t.Fatalf("master.Recurrence = nil, want weekly recurrence")
	}
	rec := master.Recurrence
	if rec.Pattern.Type != "weekly" || rec.Pattern.Interval != 2 {
		t.Errorf("master recurrence pattern = %+v, want weekly interval 2", rec.Pattern)
	}
	if want := []string{"monday", "wednesday"}; !reflect.DeepEqual(rec.Pattern.DaysOfWeek, want) {
		t.Errorf("master recurrence days = %v, want %v", rec.Pattern.DaysOfWeek, want)
	}
	if rec.Range.Type != "endDate" || rec.Range.EndDate != "2026-11-02" {
		t.Errorf("master recurrence range = %+v, want endDate 2026-11-02", rec.Range)
	}
	if want := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC); !master.Start.Equal(want) {
		t.Errorf("master.Start = %v, want %v", master.Start, want)
	}
}

// MARK: - Meeting metadata serialization (Stage 1, read-only)

func TestEventToICalMeetingMetadata(t *testing.T) {
	orig := Event{
		ID:      "MEET1",
		ICalUID: "ical-meet1",
		Subject: "Design review",
		Start:   time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC),
		End:     time.Date(2026, 7, 23, 11, 0, 0, 0, time.UTC),
		// Deliberately unsorted: bob before alice. Output must sort by email.
		Attendees: []Attendee{
			{Name: "Bob Example", Email: "bob@example.com", Type: "optional", Response: "declined"},
			{Name: "Alice Example", Email: "alice@example.com", Type: "required", Response: "accepted"},
		},
		Organizer:        &Person{Name: "Olivia Organizer", Email: "olivia@example.com"},
		IsOnlineMeeting:  true,
		OnlineMeetingURL: "https://teams.microsoft.com/l/meetup-join/abc123",
		OwnerResponse:    OwnerRespAccepted,
	}

	data, err := EventToICal(orig)
	if err != nil {
		t.Fatalf("EventToICal: %v", err)
	}
	ics := string(data)

	// go-ical emits params sorted alphabetically: CN, PARTSTAT, ROLE, RSVP.
	aliceLine := "ATTENDEE;CN=Alice Example;PARTSTAT=ACCEPTED;ROLE=REQ-PARTICIPANT;RSVP=TRUE:mailto:alice@example.com\r\n"
	bobLine := "ATTENDEE;CN=Bob Example;PARTSTAT=DECLINED;ROLE=OPT-PARTICIPANT;RSVP=TRUE:mailto:bob@example.com\r\n"
	for _, want := range []string{
		"ORGANIZER;CN=Olivia Organizer:mailto:olivia@example.com\r\n",
		aliceLine,
		bobLine,
		"URL:https://teams.microsoft.com/l/meetup-join/abc123\r\n",
		"X-MICROSOFT-SKYPETEAMSMEETINGURL:https://teams.microsoft.com/l/meetup-join/abc123\r\n",
	} {
		if !strings.Contains(ics, want) {
			t.Errorf("ICS missing %q:\n%s", want, ics)
		}
	}
	// Not cancelled: no STATUS line.
	if strings.Contains(ics, "STATUS:") {
		t.Errorf("non-cancelled ICS must not contain STATUS:\n%s", ics)
	}
	// Attendees sorted by email: alice before bob.
	if strings.Index(ics, aliceLine) > strings.Index(ics, bobLine) {
		t.Errorf("attendees not sorted by email:\n%s", ics)
	}

	// The document must parse back with attendees and organizer intact
	// (attendees sorted by email); the join URL parses back for display.
	got, err := ICalToEvent(data, "")
	if err != nil {
		t.Fatalf("ICalToEvent: %v", err)
	}
	if got.Subject != orig.Subject || !got.Start.Equal(orig.Start) {
		t.Errorf("core fields lost: %+v", got)
	}
	wantAttendees := []Attendee{
		{Name: "Alice Example", Email: "alice@example.com", Type: "required", Response: "accepted"},
		{Name: "Bob Example", Email: "bob@example.com", Type: "optional", Response: "declined"},
	}
	if !reflect.DeepEqual(got.Attendees, wantAttendees) {
		t.Errorf("Attendees = %+v, want %+v", got.Attendees, wantAttendees)
	}
	if got.Organizer == nil || got.Organizer.Email != "olivia@example.com" || got.Organizer.Name != "Olivia Organizer" {
		t.Errorf("Organizer = %+v, want Olivia Organizer <olivia@example.com>", got.Organizer)
	}
	if got.OnlineMeetingURL != orig.OnlineMeetingURL {
		t.Errorf("OnlineMeetingURL = %q, want %q (parsed back for display)", got.OnlineMeetingURL, orig.OnlineMeetingURL)
	}
	if !got.IsOnlineMeeting {
		t.Error("IsOnlineMeeting = false, want true (join URL present)")
	}
}

func TestICalRoundTripMeetingMetadata(t *testing.T) {
	// Golden round-trip: ICalToEvent(EventToICal(ev), owner) preserves
	// Attendees, Organizer and OwnerResponse for representable values.
	orig := Event{
		ID:      "MEET2",
		ICalUID: "ical-meet2",
		Subject: "Planning",
		Start:   time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC),
		End:     time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC),
		Attendees: []Attendee{
			{Name: "Alice Example", Email: "alice@example.com", Type: "required", Response: "accepted"},
			{Name: "Bob Example", Email: "bob@example.com", Type: "optional", Response: "tentativelyAccepted"},
			{Name: "Me Myself", Email: "me@example.com", Type: "required", Response: "declined"},
		},
		Organizer:     &Person{Name: "Olivia Organizer", Email: "olivia@example.com"},
		OwnerResponse: OwnerRespDeclined,
	}

	data, err := EventToICal(orig)
	if err != nil {
		t.Fatalf("EventToICal: %v", err)
	}
	got, err := ICalToEvent(data, "ME@EXAMPLE.COM") // owner match is case-insensitive
	if err != nil {
		t.Fatalf("ICalToEvent: %v", err)
	}

	if !reflect.DeepEqual(got.Attendees, orig.Attendees) {
		t.Errorf("Attendees = %+v, want %+v", got.Attendees, orig.Attendees)
	}
	if got.Organizer == nil || *got.Organizer != *orig.Organizer {
		t.Errorf("Organizer = %+v, want %+v", got.Organizer, orig.Organizer)
	}
	if got.OwnerResponse != OwnerRespDeclined {
		t.Errorf("OwnerResponse = %q, want declined (from the owner's ATTENDEE PARTSTAT)", got.OwnerResponse)
	}
	if got.RequestOnlineMeeting {
		t.Error("RequestOnlineMeeting = true without a marker")
	}
}

func TestICalToEventOwnerAsOrganizer(t *testing.T) {
	data, err := EventToICal(Event{
		ID:        "ORG1",
		ICalUID:   "ical-org1",
		Subject:   "My meeting",
		Start:     time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC),
		End:       time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC),
		Organizer: &Person{Name: "Me", Email: "me@example.com"},
		Attendees: []Attendee{
			{Name: "Alice", Email: "alice@example.com", Type: "required", Response: "none"},
		},
	})
	if err != nil {
		t.Fatalf("EventToICal: %v", err)
	}
	got, err := ICalToEvent(data, "me@example.com")
	if err != nil {
		t.Fatalf("ICalToEvent: %v", err)
	}
	if got.OwnerResponse != OwnerRespOrganizer {
		t.Errorf("OwnerResponse = %q, want organizer (ORGANIZER matches owner)", got.OwnerResponse)
	}
}

func TestICalToEventTeamsMarker(t *testing.T) {
	data, err := EventToICal(Event{
		ID:      "TEAMS1",
		ICalUID: "ical-teams1",
		Subject: "Teams meeting",
		Start:   time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC),
		End:     time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("EventToICal: %v", err)
	}
	marked := strings.Replace(string(data), "END:VEVENT",
		"X-DURIAN-CREATE-TEAMS-MEETING:TRUE\r\nEND:VEVENT", 1)

	got, err := ICalToEvent([]byte(marked), "me@example.com")
	if err != nil {
		t.Fatalf("ICalToEvent: %v", err)
	}
	if !got.RequestOnlineMeeting {
		t.Error("RequestOnlineMeeting = false, want true (marker present)")
	}

	// EventToICal never re-emits the marker: one-shot by construction.
	rendered, err := EventToICal(got)
	if err != nil {
		t.Fatalf("EventToICal re-render: %v", err)
	}
	if strings.Contains(string(rendered), "X-DURIAN-CREATE-TEAMS-MEETING") {
		t.Error("re-rendered ICS still carries the Teams marker")
	}
}

func TestEventToICalCancelled(t *testing.T) {
	data, err := EventToICal(Event{
		ID:          "CANC1",
		ICalUID:     "ical-canc1",
		Subject:     "Cancelled meeting",
		Start:       time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC),
		End:         time.Date(2026, 7, 23, 11, 0, 0, 0, time.UTC),
		IsCancelled: true,
	})
	if err != nil {
		t.Fatalf("EventToICal: %v", err)
	}
	if want := "STATUS:CANCELLED\r\n"; !strings.Contains(string(data), want) {
		t.Errorf("cancelled ICS missing %q:\n%s", want, data)
	}
}

func TestEventToICalAttendeePartStatMapping(t *testing.T) {
	data, err := EventToICal(Event{
		ID:      "PART1",
		ICalUID: "ical-part1",
		Subject: "PARTSTAT mapping",
		Start:   time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC),
		End:     time.Date(2026, 7, 23, 11, 0, 0, 0, time.UTC),
		Attendees: []Attendee{
			{Email: "a@example.com", Type: "required", Response: "tentativelyAccepted"},
			{Email: "b@example.com", Type: "required", Response: "notResponded"},
			{Email: "c@example.com", Type: "required", Response: "none"},
			{Email: "d@example.com", Type: "required", Response: "organizer"},
			{Email: "e@example.com", Type: "resource", Response: "accepted"},
		},
	})
	if err != nil {
		t.Fatalf("EventToICal: %v", err)
	}
	ics := string(data)
	for _, want := range []string{
		"ATTENDEE;PARTSTAT=TENTATIVE;ROLE=REQ-PARTICIPANT;RSVP=TRUE:mailto:a@example.com\r\n",
		"ATTENDEE;PARTSTAT=NEEDS-ACTION;ROLE=REQ-PARTICIPANT;RSVP=TRUE:mailto:b@example.com\r\n",
		"ATTENDEE;PARTSTAT=NEEDS-ACTION;ROLE=REQ-PARTICIPANT;RSVP=TRUE:mailto:c@example.com\r\n",
		"ATTENDEE;PARTSTAT=ACCEPTED;ROLE=REQ-PARTICIPANT;RSVP=TRUE:mailto:d@example.com\r\n",
		"ATTENDEE;PARTSTAT=ACCEPTED;ROLE=NON-PARTICIPANT;RSVP=TRUE:mailto:e@example.com\r\n",
	} {
		if !strings.Contains(ics, want) {
			t.Errorf("ICS missing %q:\n%s", want, ics)
		}
	}
}

// TestICalRoundTripCancelled pins STATUS:CANCELLED in BOTH directions. The
// write side existed from the start; the read side did not, which made the
// flag write-only: anything that parsed a cancelled event and re-serialized it
// (the GUI write handler, the RSVP path) dropped the cancellation, and the
// next sync read that as a local edit and patched the cancelled meeting.
func TestICalRoundTripCancelled(t *testing.T) {
	src := Event{
		ICalUID:     "evt-cancelled",
		Subject:     "Cancelled Sync",
		Start:       time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC),
		End:         time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC),
		IsCancelled: true,
	}
	data, err := EventToICal(src)
	if err != nil {
		t.Fatalf("EventToICal: %v", err)
	}
	if !strings.Contains(string(data), "STATUS:CANCELLED") {
		t.Fatalf("serialized event lacks STATUS:CANCELLED:\n%s", data)
	}

	got, err := ICalToEvent(data, "me@example.com")
	if err != nil {
		t.Fatalf("ICalToEvent: %v", err)
	}
	if !got.IsCancelled {
		t.Error("IsCancelled lost on parse; the flag must survive a round trip")
	}

	// A re-serialization of the parsed event keeps the marker, so a local
	// rewrite is byte-stable and does not register as a content change.
	again, err := EventToICal(got)
	if err != nil {
		t.Fatalf("EventToICal (2nd): %v", err)
	}
	if !strings.Contains(string(again), "STATUS:CANCELLED") {
		t.Error("re-serialized event dropped STATUS:CANCELLED")
	}

	// An uncancelled event must not pick the flag up.
	src.IsCancelled = false
	plain, err := EventToICal(src)
	if err != nil {
		t.Fatalf("EventToICal (plain): %v", err)
	}
	back, err := ICalToEvent(plain, "me@example.com")
	if err != nil {
		t.Fatalf("ICalToEvent (plain): %v", err)
	}
	if back.IsCancelled {
		t.Error("IsCancelled set for an event without STATUS:CANCELLED")
	}
}
