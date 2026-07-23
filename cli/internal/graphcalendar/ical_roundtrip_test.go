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

	got, err := ICalToEvent(data)
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

	got, err := ICalToEvent(data)
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

	got, err := ICalToEvent(data)
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

	got, err := ICalToEvent(data)
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
		if h != `outlook.timezone="UTC"` {
			t.Errorf("page %d Prefer header = %q, want outlook.timezone UTC", i+1, h)
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
