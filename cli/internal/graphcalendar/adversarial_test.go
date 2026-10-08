package graphcalendar

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
)

// This is a simulated Microsoft Graph v1.0 provider response, not a claim of
// live-account validation. Its shape follows the documented Get-event example:
// cancelledOccurrences is a string collection of occurrenceId values and is
// returned only by GET /me/events/{seriesMasterId} when explicitly selected.
// The occurrenceId format below is the documented OID.<master>.<date> form.
func TestAdversarialFetchMasterEventsReadsDocumentedCancelledOccurrences(t *testing.T) {
	const master = `{
		"id": "series-master",
		"iCalUId": "series-uid",
		"subject": "Weekly review",
		"start": {"dateTime": "2026-08-03T09:00:00.0000000", "timeZone": "UTC"},
		"end": {"dateTime": "2026-08-03T10:00:00.0000000", "timeZone": "UTC"},
		"isAllDay": false,
		"type": "seriesMaster",
		"changeKey": "ck-series",
		"lastModifiedDateTime": "2026-08-01T12:00:00Z",
		"recurrence": {
			"pattern": {"type": "weekly", "interval": 1, "daysOfWeek": ["monday"]},
			"range": {"type": "numbered", "startDate": "2026-08-03", "numberOfOccurrences": 3}
		}
	}`

	var detailReads int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /me/calendars/cal1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"value":[%s]}`, master)
	})
	mux.HandleFunc("GET /me/events/series-master", func(w http.ResponseWriter, r *http.Request) {
		detailReads++
		if !strings.Contains(r.URL.Query().Get("$select"), "cancelledOccurrences") {
			t.Errorf("series-master $select = %q, want cancelledOccurrences", r.URL.Query().Get("$select"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `%s`, strings.TrimSuffix(master, "}")+`,
			"cancelledOccurrences":["OID.series-master.2026-08-10"]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewWithToken("owner@example.com", srv.URL, "test-token", srv.Client())
	events, err := client.FetchMasterEvents(context.Background(), "cal1")
	if err != nil {
		t.Fatalf("FetchMasterEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v, want one series master", events)
	}
	if detailReads != 1 {
		t.Errorf("series-master detail reads = %d, want 1; list-events cannot return documented cancellation IDs", detailReads)
	}
	// The synthetic series uses UTC, so the occurrenceId date plus the
	// master's independently known wall time identifies the missing instant.
	want := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	if len(events[0].ExceptionDates) != 1 || !events[0].ExceptionDates[0].Equal(want) {
		t.Errorf("ExceptionDates = %v, want [%s]; cancelled occurrence is still rendered locally", events[0].ExceptionDates, want)
	}
}

func TestCancelledOccurrencesUseSeriesZoneAcrossDSTAndAllDay(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	timed := calendar.Event{
		ID: "master", Start: time.Date(2026, 3, 22, 9, 0, 0, 0, berlin),
		End: time.Date(2026, 3, 22, 10, 0, 0, 0, berlin),
	}
	if err := attachCancelledOccurrences(&timed, []string{"OID.master.2026-03-29"}); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 3, 29, 9, 0, 0, 0, berlin)
	if len(timed.ExceptionDates) != 1 || !timed.ExceptionDates[0].Equal(want) ||
		timed.ExceptionDates[0].Location().String() != "Europe/Berlin" {
		t.Errorf("timed cancellation = %v, want %s in Europe/Berlin", timed.ExceptionDates, want)
	}

	allDay := calendar.Event{ID: "all-day", AllDay: true,
		Start: time.Date(2026, 3, 22, 0, 0, 0, 0, time.UTC)}
	if err := attachCancelledOccurrences(&allDay, []string{"OID.all-day.2026-03-29"}); err != nil {
		t.Fatal(err)
	}
	if got := allDay.ExceptionDates[0].Format(time.RFC3339); got != "2026-03-29T00:00:00Z" {
		t.Errorf("all-day cancellation = %s, want date-valued UTC midnight", got)
	}
	for _, malformed := range []string{
		"OID.other.2026-03-29", "OID.master.2026-3-29", "master.2026-03-29", "OID.master.2026-02-30",
	} {
		copyEvent := timed
		copyEvent.ExceptionDates = nil
		if err := attachCancelledOccurrences(&copyEvent, []string{malformed}); err == nil {
			t.Errorf("malformed cancellation %q was accepted", malformed)
		}
	}
}

func TestGraphWindowsZonePreservedThroughNeutralModelAndWrite(t *testing.T) {
	ge := graphEvent{
		ID: "master", ICalUID: "uid", Type: "seriesMaster",
		Start: graphDateTime{DateTime: "2026-03-22T08:00:00", TimeZone: "UTC"},
		End:   graphDateTime{DateTime: "2026-03-22T09:00:00", TimeZone: "UTC"},
		Recurrence: &calendar.Recurrence{
			Pattern: calendar.RecurrencePattern{Type: "weekly", Interval: 1},
			Range: calendar.RecurrenceRange{Type: "noEnd", StartDate: "2026-03-22",
				TimeZone: "W. Europe Standard Time"},
		},
	}
	ev, ok := eventFromGraph(ge)
	if !ok {
		t.Fatal("eventFromGraph rejected valid zoned series")
	}
	if ev.Start.Location().String() != "Europe/Berlin" || ev.Start.Hour() != 9 ||
		ev.Recurrence.Range.TimeZone != "Europe/Berlin" {
		t.Fatalf("Graph zone was not mapped through CLDR: start=%s recurrence=%+v", ev.Start, ev.Recurrence)
	}
	body := EventToGraphBody(ev, false)
	written := body["recurrence"].(*calendar.Recurrence)
	if written.Range.TimeZone != "W. Europe Standard Time" {
		t.Errorf("written recurrence timezone = %q, want Graph Windows identifier", written.Range.TimeZone)
	}
	start := body["start"].(map[string]string)
	end := body["end"].(map[string]string)
	if start["dateTime"] != "2026-03-22T09:00:00" || start["timeZone"] != "W. Europe Standard Time" ||
		end["dateTime"] != "2026-03-22T10:00:00" || end["timeZone"] != start["timeZone"] {
		t.Fatalf("write lost civil start/end zone: start=%v end=%v", start, end)
	}
}

func TestGraphUTCAndAllDayRecurrenceZonesRoundTrip(t *testing.T) {
	for _, allDay := range []bool{false, true} {
		ge := graphEvent{
			ID: "master", ICalUID: "uid", Type: "seriesMaster", IsAllDay: allDay,
			Start: graphDateTime{DateTime: "2026-03-22T00:00:00", TimeZone: "UTC"},
			End:   graphDateTime{DateTime: "2026-03-23T00:00:00", TimeZone: "UTC"},
			Recurrence: &calendar.Recurrence{
				Pattern: calendar.RecurrencePattern{Type: "daily", Interval: 1},
				Range:   calendar.RecurrenceRange{Type: "noEnd", StartDate: "2026-03-22", TimeZone: "UTC"},
			},
		}
		if allDay {
			ge.Recurrence.Range.TimeZone = "W. Europe Standard Time"
		}
		ev, ok := eventFromGraph(ge)
		if !ok {
			t.Fatal("valid Graph recurrence rejected")
		}
		data, err := calendar.EventToICal(ev)
		if err != nil {
			t.Fatal(err)
		}
		back, err := calendar.ICalToEvent(data, "")
		if err != nil || back.Recurrence == nil || back.Recurrence.Range != ev.Recurrence.Range {
			t.Fatalf("allDay=%v recurrence range changed: remote=%+v parsed=%+v err=%v", allDay, ev.Recurrence, back.Recurrence, err)
		}
	}
}

func TestFetchMasterEventsRejectsUnsafeCancellationSnapshot(t *testing.T) {
	const master = `{"id":"master","iCalUId":"uid","type":"seriesMaster","changeKey":"v1",
		"start":{"dateTime":"2026-03-22T08:00:00","timeZone":"UTC"},
		"end":{"dateTime":"2026-03-22T09:00:00","timeZone":"UTC"},
		"recurrence":{"pattern":{"type":"weekly","interval":1},
		"range":{"type":"noEnd","startDate":"2026-03-22","recurrenceTimeZone":"W. Europe Standard Time"}}}`
	for _, tc := range []struct {
		name, detail string
	}{
		{"changed while reading", strings.Replace(master, `"changeKey":"v1"`, `"changeKey":"v2"`, 1)},
		{"wrong identity", strings.Replace(master, `"id":"master"`, `"id":"other"`, 1)},
		{"unknown zone", strings.Replace(master, "W. Europe Standard Time", "tzone://Microsoft/Custom", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /me/calendars/cal1/events", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"value":[%s]}`, master)
			})
			mux.HandleFunc("GET /me/events/master", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, strings.TrimSuffix(tc.detail, "}")+`,"cancelledOccurrences":["OID.master.2026-03-29"]}`)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			client := NewWithToken("owner@example.com", srv.URL, "test-token", srv.Client())
			events, err := client.FetchMasterEvents(t.Context(), "cal1")
			if err == nil || events != nil {
				t.Fatalf("unsafe snapshot accepted: events=%+v err=%v", events, err)
			}
		})
	}
}

func TestFetchMasterEventsFailsClosedWhenCancellationEnrichmentFails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /me/calendars/cal1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"value":[{
			"id":"master","iCalUId":"uid","type":"seriesMaster",
			"start":{"dateTime":"2026-08-03T09:00:00","timeZone":"UTC"},
			"end":{"dateTime":"2026-08-03T10:00:00","timeZone":"UTC"},
			"recurrence":{"pattern":{"type":"weekly","interval":1},"range":{"type":"noEnd","startDate":"2026-08-03"}}
		}]}`)
	})
	mux.HandleFunc("GET /me/events/master", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewWithToken("owner@example.com", srv.URL, "test-token", srv.Client())
	events, err := client.FetchMasterEvents(context.Background(), "cal1")
	if err == nil || events != nil {
		t.Fatalf("FetchMasterEvents = (%v, %v), want failure and no partial snapshot", events, err)
	}
}
