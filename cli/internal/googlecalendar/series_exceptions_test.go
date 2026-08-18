package googlecalendar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
)

// seriesWithInstancesPage puts the master LAST on purpose: the API gives no
// ordering guarantee, and folding instances into their master while paging
// would drop every instance that arrives first.
const seriesWithInstancesPage = `{
	"items": [
		{
			"id": "evt-series_20260810T090000Z",
			"status": "cancelled",
			"recurringEventId": "evt-series",
			"originalStartTime": {"dateTime": "2026-08-10T09:00:00Z"}
		},
		{
			"id": "evt-series_20260817T090000Z",
			"status": "confirmed",
			"etag": "\"etag-moved\"",
			"iCalUID": "uid-series@google.com",
			"summary": "Standup (moved)",
			"location": "Room 2",
			"recurringEventId": "evt-series",
			"originalStartTime": {"dateTime": "2026-08-17T09:00:00Z"},
			"start": {"dateTime": "2026-08-18T14:00:00Z"},
			"end": {"dateTime": "2026-08-18T15:00:00Z"}
		},
		{
			"id": "evt-series",
			"status": "confirmed",
			"etag": "\"etag-series\"",
			"iCalUID": "uid-series@google.com",
			"summary": "Standup",
			"start": {"dateTime": "2026-08-03T09:00:00Z"},
			"end": {"dateTime": "2026-08-03T10:00:00Z"},
			"recurrence": ["RRULE:FREQ=WEEKLY;BYDAY=MO", "EXDATE:20260831T090000Z"]
		}
	]
}`

func TestFetchMasterEventsFoldsSeriesInstances(t *testing.T) {
	var sawShowDeleted string
	mux := http.NewServeMux()
	mux.HandleFunc("/calendars/cal-primary/events", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(t, w, r) {
			return
		}
		sawShowDeleted = r.URL.Query().Get("showDeleted")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(seriesWithInstancesPage))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	events, err := testClient(srv).FetchMasterEvents(context.Background(), "cal-primary")
	if err != nil {
		t.Fatalf("FetchMasterEvents: %v", err)
	}
	if sawShowDeleted != "true" {
		t.Errorf("showDeleted = %q, want \"true\"", sawShowDeleted)
	}

	// The two instances are folded into the master, not returned beside it.
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 series master: %+v", len(events), events)
	}
	master := events[0]
	if master.ID != "evt-series" {
		t.Fatalf("event = %q, want the series master", master.ID)
	}

	// EXDATE from the recurrence lines plus the cancelled instance.
	wantExDates := []string{"2026-08-31T09:00:00Z", "2026-08-10T09:00:00Z"}
	if len(master.ExceptionDates) != len(wantExDates) {
		t.Fatalf("exception dates = %v, want %v", master.ExceptionDates, wantExDates)
	}
	got := make(map[string]bool, len(master.ExceptionDates))
	for _, d := range master.ExceptionDates {
		got[d.UTC().Format(time.RFC3339)] = true
	}
	for _, want := range wantExDates {
		if !got[want] {
			t.Errorf("missing exception date %s (have %v)", want, master.ExceptionDates)
		}
	}

	if len(master.Overrides) != 1 {
		t.Fatalf("overrides = %+v, want exactly the moved occurrence", master.Overrides)
	}
	override := master.Overrides[0]
	if !override.RecurrenceID.Equal(time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("override recurrence id = %s, want 2026-08-17T09:00:00Z", override.RecurrenceID)
	}
	if !override.Start.Equal(time.Date(2026, 8, 18, 14, 0, 0, 0, time.UTC)) {
		t.Errorf("override start = %s, want the moved time 2026-08-18T14:00:00Z", override.Start)
	}
	if override.Subject != "Standup (moved)" || override.Location != "Room 2" {
		t.Errorf("override lost its own content: %+v", override)
	}
	if override.Recurrence != nil || len(override.ExceptionDates) != 0 {
		t.Error("override carries series state of its own")
	}
}

// An instance whose master is missing has nothing to attach to. Promoting it
// to a master would upload a series collapsed to that single occurrence.
func TestFetchMasterEventsDropsOrphanInstances(t *testing.T) {
	const orphanPage = `{
		"items": [
			{
				"id": "evt-gone_20260810T090000Z",
				"status": "confirmed",
				"iCalUID": "uid-gone@google.com",
				"summary": "Orphan",
				"recurringEventId": "evt-gone",
				"originalStartTime": {"dateTime": "2026-08-10T09:00:00Z"},
				"start": {"dateTime": "2026-08-10T09:00:00Z"},
				"end": {"dateTime": "2026-08-10T10:00:00Z"}
			}
		]
	}`
	mux := http.NewServeMux()
	mux.HandleFunc("/calendars/cal-primary/events", func(w http.ResponseWriter, r *http.Request) {
		if !requireBearer(t, w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(orphanPage))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	events, err := testClient(srv).FetchMasterEvents(context.Background(), "cal-primary")
	if err != nil {
		t.Fatalf("FetchMasterEvents: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("got %d events, want none: %+v", len(events), events)
	}
}

func TestRecurrenceFromGoogleMarksUnmappableRuleOpaque(t *testing.T) {
	start := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)

	rec, _, opaque := recurrenceFromGoogle([]string{"RRULE:FREQ=SECONDLY"}, start, "evt-x")
	if rec != nil {
		t.Errorf("unmappable rule produced a recurrence: %+v", rec)
	}
	if !opaque {
		t.Error("unmappable rule was not marked opaque; the next upload would clear the series")
	}

	rec, _, opaque = recurrenceFromGoogle(nil, start, "evt-x")
	if rec != nil || opaque {
		t.Errorf("an event with no recurrence lines = (%+v, opaque=%v), want (nil, false)", rec, opaque)
	}
}

func TestEventToGoogleOmitsOpaqueRecurrence(t *testing.T) {
	ev := calendar.Event{
		ID:      "evt-series",
		ICalUID: "uid-series@google.com",
		Subject: "Standup",
		Start:   time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC),
		End:     time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC),
	}
	ev.OpaqueRecurrence = true

	body := eventToGoogle(ev, false)
	if _, present := body["recurrence"]; present {
		t.Error("body carries a recurrence key for an opaque series; the PATCH would clear it")
	}

	ev.OpaqueRecurrence = false
	body = eventToGoogle(ev, false)
	if _, present := body["recurrence"]; !present {
		t.Error("body omits the recurrence key for a normal event")
	}
}

// A recurrence line durian cannot hold must make the whole series opaque. The
// upload rebuilds the recurrence from the parts it understood, so a dropped
// EXDATE revives a cancelled occurrence on the server — and for a meeting it
// reappears in every attendee's calendar.
func TestRecurrenceFromGoogleMarksLostRecurrenceLinesOpaque(t *testing.T) {
	start := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		lines []string
	}{
		{"unreadable EXDATE", []string{"RRULE:FREQ=WEEKLY;BYDAY=MO", "EXDATE:not-a-date"}},
		{"RDATE", []string{"RRULE:FREQ=WEEKLY;BYDAY=MO", "RDATE:20260901T090000Z"}},
		{"EXRULE", []string{"RRULE:FREQ=WEEKLY;BYDAY=MO", "EXRULE:FREQ=MONTHLY"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, _, opaque := recurrenceFromGoogle(tc.lines, start, "evt-x")
			if !opaque {
				t.Error("series was not marked opaque; the next upload would drop the line")
			}
			if rec != nil {
				t.Errorf("recurrence = %+v, want nil so no write path can rebuild it", rec)
			}
		})
	}

	// A clean series must stay writable.
	rec, exDates, opaque := recurrenceFromGoogle(
		[]string{"RRULE:FREQ=WEEKLY;BYDAY=MO", "EXDATE:20260810T090000Z"}, start, "evt-x")
	if opaque || rec == nil || len(exDates) != 1 {
		t.Errorf("clean series = (rec=%v, exDates=%v, opaque=%v), want it fully parsed",
			rec, exDates, opaque)
	}
}
