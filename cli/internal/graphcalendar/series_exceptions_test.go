package graphcalendar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
)

// seriesWithExceptionPage lists the exception BEFORE its master on purpose:
// /events gives no ordering guarantee, so folding while paging would drop
// every exception that arrives first. The plain "occurrence" entry must be
// skipped — it is the rule's own output, already reproduced locally.
const seriesWithExceptionPage = `{
	"value": [
		{
			"id": "evt-series-exc",
			"iCalUId": "ical-series",
			"subject": "Standup (moved)",
			"body": {"contentType": "text", "content": "moved"},
			"start": {"dateTime": "2026-08-18T14:00:00.0000000", "timeZone": "UTC"},
			"end": {"dateTime": "2026-08-18T15:00:00.0000000", "timeZone": "UTC"},
			"location": {"displayName": "Room 2"},
			"type": "exception",
			"seriesMasterId": "evt-series",
			"originalStart": "2026-08-17T09:00:00.0000000",
			"changeKey": "ck-exc",
			"lastModifiedDateTime": "2026-08-01T09:00:00Z"
		},
		{
			"id": "evt-series-occ",
			"iCalUId": "ical-series",
			"subject": "Standup",
			"start": {"dateTime": "2026-08-24T09:00:00.0000000", "timeZone": "UTC"},
			"end": {"dateTime": "2026-08-24T10:00:00.0000000", "timeZone": "UTC"},
			"type": "occurrence",
			"seriesMasterId": "evt-series",
			"originalStart": "2026-08-24T09:00:00.0000000",
			"changeKey": "ck-occ",
			"lastModifiedDateTime": "2026-08-01T09:00:00Z"
		},
		{
			"id": "evt-series",
			"iCalUId": "ical-series",
			"subject": "Standup",
			"body": {"contentType": "text", "content": "series body"},
			"start": {"dateTime": "2026-08-03T09:00:00.0000000", "timeZone": "UTC"},
			"end": {"dateTime": "2026-08-03T10:00:00.0000000", "timeZone": "UTC"},
			"type": "seriesMaster",
			"changeKey": "ck-series",
			"lastModifiedDateTime": "2026-08-01T09:00:00Z",
			"recurrence": {
				"pattern": {"type": "weekly", "interval": 1, "daysOfWeek": ["monday"]},
				"range": {"type": "noEnd", "startDate": "2026-08-03"}
			}
		}
	]
}`

func TestFetchMasterEventsFoldsExceptions(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/me/calendars/cal1/events", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want bearer test-token", got)
		}
		if got := r.URL.Query().Get("$select"); !strings.Contains(got, "seriesMasterId") ||
			!strings.Contains(got, "originalStart") {
			t.Errorf("$select = %q, want seriesMasterId and originalStart", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(seriesWithExceptionPage))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewWithToken("owner@example.com", srv.URL, "test-token", srv.Client())
	events, err := client.FetchMasterEvents(context.Background(), "cal1")
	if err != nil {
		t.Fatalf("FetchMasterEvents: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 series master: %+v", len(events), events)
	}
	master := events[0]
	if master.ID != "evt-series" {
		t.Fatalf("event = %q, want the series master", master.ID)
	}
	if master.Recurrence == nil {
		t.Fatal("series master lost its recurrence")
	}

	if len(master.Overrides) != 1 {
		t.Fatalf("overrides = %+v, want exactly the exception", master.Overrides)
	}
	override := master.Overrides[0]
	if !override.RecurrenceID.Equal(time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("override recurrence id = %s, want the ORIGINAL start 2026-08-17T09:00:00Z",
			override.RecurrenceID)
	}
	if !override.Start.Equal(time.Date(2026, 8, 18, 14, 0, 0, 0, time.UTC)) {
		t.Errorf("override start = %s, want the moved time 2026-08-18T14:00:00Z", override.Start)
	}
	if override.Subject != "Standup (moved)" || override.Location != "Room 2" {
		t.Errorf("override lost its own content: %+v", override)
	}
	if override.Recurrence != nil {
		t.Error("override carries a recurrence of its own")
	}
}

// An exception whose master is absent has nothing to attach to. Promoting it
// to a master would upload a series collapsed to that single occurrence.
func TestFetchMasterEventsDropsOrphanExceptions(t *testing.T) {
	const orphanPage = `{
		"value": [
			{
				"id": "evt-orphan",
				"iCalUId": "ical-orphan",
				"subject": "Orphan",
				"start": {"dateTime": "2026-08-17T09:00:00.0000000", "timeZone": "UTC"},
				"end": {"dateTime": "2026-08-17T10:00:00.0000000", "timeZone": "UTC"},
				"type": "exception",
				"seriesMasterId": "evt-gone",
				"originalStart": "2026-08-17T09:00:00.0000000",
				"changeKey": "ck-orphan",
				"lastModifiedDateTime": "2026-08-01T09:00:00Z"
			}
		]
	}`
	mux := http.NewServeMux()
	mux.HandleFunc("/me/calendars/cal1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(orphanPage))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewWithToken("owner@example.com", srv.URL, "test-token", srv.Client())
	events, err := client.FetchMasterEvents(context.Background(), "cal1")
	if err != nil {
		t.Fatalf("FetchMasterEvents: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("got %d events, want none: %+v", len(events), events)
	}
}

func TestEventToGraphBodyOmitsOpaqueRecurrence(t *testing.T) {
	ev := calendar.Event{
		ID:      "evt-series",
		ICalUID: "ical-series",
		Subject: "Standup",
		Start:   time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC),
		End:     time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC),
	}

	ev.OpaqueRecurrence = true
	if _, present := EventToGraphBody(ev, false)["recurrence"]; present {
		t.Error("body carries a recurrence key for an opaque series; the PATCH would clear it")
	}

	ev.OpaqueRecurrence = false
	body := EventToGraphBody(ev, false)
	if rec, present := body["recurrence"]; !present || rec != nil {
		t.Errorf("body recurrence = (%v, present=%v), want an explicit nil that clears the series",
			rec, present)
	}
}
