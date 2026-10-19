package googlecalendar

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
	"github.com/julion2/durian/cli/internal/calendarsync"
)

// These are loopback payload contracts, not claims of live-provider acceptance.
func TestTimezoneWeeklyProviderRoundTrip(t *testing.T) {
	for _, tc := range []struct{ day, utcHour, until string }{
		{"2026-03-23", "08", "20260413T070000Z"},
		{"2026-03-30", "07", "20260413T070000Z"},
		{"2026-10-19", "07", "20261102T080000Z"},
		{"2026-10-26", "08", "20261102T080000Z"},
	} {
		t.Run(tc.day, func(t *testing.T) {
			rule := "FREQ=WEEKLY;BYDAY=MO;UNTIL=" + tc.until
			master := googleEvent{
				ID: "master", Start: googleDateTime{DateTime: tc.day + "T09:00:00", TimeZone: "Europe/Berlin"},
				End:        googleDateTime{DateTime: tc.day + "T19:00:00", TimeZone: "Asia/Tokyo"},
				Recurrence: []string{"RRULE:" + rule, "EXDATE;TZID=Europe/Berlin:20260330T090000,20261026T090000"},
			}
			moved := googleEvent{
				ID: "override", RecurringEventID: "master",
				OriginalStartTime: googleDateTime{DateTime: "2026-10-26T09:00:00+01:00", TimeZone: "Europe/Berlin"},
				Start:             googleDateTime{DateTime: "2026-10-26T10:00:00", TimeZone: "Europe/Berlin"},
				End:               googleDateTime{DateTime: "2026-10-26T20:00:00", TimeZone: "Asia/Tokyo"},
			}
			var writes atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(map[string]any{"items": []googleEvent{master, moved}, "nextSyncToken": "settled"})
					return
				}
				writes.Add(1)
				var body struct {
					Start, End googleDateTime
					Recurrence []string
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				start, _, err := parseGoogleTime(body.Start)
				if err != nil || start.Format(time.RFC3339) != tc.day+"T"+tc.utcHour+":00:00Z" || body.Start.TimeZone != "Europe/Berlin" {
					t.Errorf("start contract = %+v / %v / %v", body.Start, start, err)
				}
				end, _, err := parseGoogleTime(body.End)
				if err != nil || end.Format(time.RFC3339) != tc.day+"T10:00:00Z" || body.End.TimeZone != "Asia/Tokyo" {
					t.Errorf("end contract = %+v / %v / %v", body.End, end, err)
				}
				want := "RRULE:" + rule + "|EXDATE:20260330T070000Z|EXDATE:20261026T080000Z"
				if strings.Join(body.Recurrence, "|") != want {
					t.Errorf("recurrence = %v, want %s", body.Recurrence, want)
				}
				_ = json.NewEncoder(w).Encode(master)
			}))
			defer srv.Close()
			c := testClient(srv)
			ctx := context.Background()
			events, err := c.FetchMasterEvents(ctx, "cal")
			if err != nil || len(events) != 1 {
				t.Fatalf("FetchMasterEvents = %v / %v", events, err)
			}
			ev := events[0]
			if ev.Start.Format(time.RFC3339) != tc.day+"T"+tc.utcHour+":00:00Z" || ev.TimeZone != "Europe/Berlin" || ev.EndTimeZone != "Asia/Tokyo" || ev.RecurrenceRule != rule || ev.Recurrence == nil || ev.Recurrence.Range.TimeZone != "Europe/Berlin" || ev.Recurrence.Range.StartDate != tc.day {
				t.Fatalf("lost timezone/rule: %+v", ev)
			}
			if len(ev.Overrides) != 1 || ev.Overrides[0].RecurrenceID.Format(time.RFC3339) != "2026-10-26T08:00:00Z" || ev.Overrides[0].Start.Format(time.RFC3339) != "2026-10-26T09:00:00Z" || ev.Overrides[0].TimeZone != "Europe/Berlin" || ev.Overrides[0].EndTimeZone != "Asia/Tokyo" {
				t.Fatalf("moved override = %+v", ev.Overrides)
			}
			delta, err := c.FetchMasterEventsDelta(ctx, "cal", "")
			if err != nil || delta.Cursor != "settled" || len(delta.ChangedMasters) != 1 || len(delta.ChangedMasters[0].Overrides) != 1 || delta.ChangedMasters[0].TimeZone != "Europe/Berlin" {
				t.Fatalf("delta = %+v / %v", delta, err)
			}
			created, err := c.CreateEvent(ctx, "cal", ev, calendarsync.CreateOptions{})
			if err != nil || created.TimeZone != ev.TimeZone || !created.Start.Equal(ev.Start) {
				t.Fatalf("CreateEvent = %+v / %v", created, err)
			}
			if err := c.UpdateEvent(ctx, "cal", "master", calendarsync.UpdateSpec{Event: ev}); err != nil {
				t.Fatal(err)
			}
			if writes.Load() != 2 {
				t.Fatalf("writes = %d", writes.Load())
			}
		})
	}
}

func TestTimezoneOffsetAndAllDayControls(t *testing.T) {
	for _, tc := range []struct {
		dt   googleDateTime
		want string
	}{
		{googleDateTime{DateTime: "2026-03-30T09:00:00-04:00", TimeZone: "Europe/Berlin"}, "2026-03-30T13:00:00Z"},
		{googleDateTime{DateTime: "2026-10-25T02:30:00+01:00", TimeZone: "Europe/Berlin"}, "2026-10-25T01:30:00Z"},
		{googleDateTime{Date: "2026-03-30", TimeZone: "Europe/Berlin"}, "2026-03-30T00:00:00Z"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(googleEvent{ID: "event", Start: tc.dt, End: tc.dt})
		}))
		c := testClient(srv)
		ev, err := c.GetEvent(context.Background(), "cal", "event")
		if err != nil || ev.Start.Format(time.RFC3339) != tc.want {
			t.Fatalf("GetEvent = %+v / %v, want %s", ev, err, tc.want)
		}
		body := eventToGoogle(ev, false)
		if ev.AllDay && (ev.TimeZone != "" || ev.EndTimeZone != "" || body["start"].(map[string]string)["date"] != "2026-03-30") {
			t.Errorf("all-day converted into instant: %+v / %v", ev, body)
		}
		srv.Close()
	}
}

func TestTimezoneUnsupportedReadsAndWritesFailSafely(t *testing.T) {
	for _, dt := range []googleDateTime{
		{DateTime: "2026-03-30T09:00:00Z", TimeZone: "Custom/Unsupported"},
		{DateTime: "2026-03-30T09:00:00"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []googleEvent{{ID: "existing", Start: dt, End: dt}}, "nextSyncToken": "must-not-advance"})
		}))
		c := testClient(srv)
		if events, err := c.FetchMasterEvents(context.Background(), "cal"); err == nil || events != nil {
			t.Errorf("unsafe partial snapshot = %+v / %v", events, err)
		}
		if result, err := c.FetchMasterEventsDelta(context.Background(), "cal", "old"); err == nil || result.Cursor != "" {
			t.Errorf("unsafe delta = %+v / %v", result, err)
		}
		srv.Close()
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer srv.Close()
	c := testClient(srv)
	for _, ev := range []calendar.Event{{Floating: true}, {TimeZone: "Custom/Unsupported"}} {
		if _, err := c.CreateEvent(context.Background(), "cal", ev, calendarsync.CreateOptions{}); err == nil {
			t.Error("CreateEvent accepted unrepresentable event")
		}
		for _, attendeesOnly := range []bool{false, true} {
			err := c.UpdateEvent(context.Background(), "cal", "event", calendarsync.UpdateSpec{Event: ev, AttendeesOnly: attendeesOnly})
			if err == nil || (ev.Floating && !strings.Contains(err.Error(), "floating")) {
				t.Errorf("UpdateEvent error = %v", err)
			}
		}
	}
	if requests.Load() != 0 {
		t.Errorf("invalid writes reached network: %d", requests.Load())
	}
}

func TestTimezoneRawRuleAndInheritedEndOnUpdate(t *testing.T) {
	rule := "FREQ=WEEKLY;UNTIL=20260413T070000Z;BYDAY=MO"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body googleEvent
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Start.TimeZone != "Europe/Berlin" || body.End.TimeZone != "Europe/Berlin" || len(body.Recurrence) != 1 || body.Recurrence[0] != "RRULE:"+rule {
			t.Errorf("lost inherited zone or exact raw-only rule: %+v", body)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	ev := calendar.Event{
		Start:    time.Date(2026, 3, 23, 8, 0, 0, 0, time.UTC),
		End:      time.Date(2026, 3, 23, 9, 0, 0, 0, time.UTC),
		TimeZone: "Europe/Berlin", RecurrenceRule: rule,
	}
	if err := testClient(srv).UpdateEvent(context.Background(), "cal", "event", calendarsync.UpdateSpec{Event: ev}); err != nil {
		t.Fatal(err)
	}
}
