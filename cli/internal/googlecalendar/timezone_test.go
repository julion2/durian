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
				ID: "master", ETag: `"version"`, Start: googleDateTime{DateTime: tc.day + "T09:00:00", TimeZone: "Europe/Berlin"},
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
					if strings.HasSuffix(r.URL.Path, "/master") {
						_ = json.NewEncoder(w).Encode(master)
					} else {
						_ = json.NewEncoder(w).Encode(map[string]any{"items": []googleEvent{master, moved}, "nextSyncToken": "settled"})
					}
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
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"etag":"version"}`))
			return
		}
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

func TestTimezoneFoldProviderRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name, start, end, startZone, endZone string
		fold, allDay                         bool
	}{
		{"first fold", "2026-10-25T00:15:00Z", "2026-10-25T00:45:00Z", "Europe/Berlin", "Europe/Berlin", true, false},
		{"second fold", "2026-10-25T01:15:00Z", "2026-10-25T01:45:00Z", "Europe/Berlin", "Europe/Berlin", true, false},
		{"fold nanoseconds", "2026-10-25T01:15:00.123456789Z", "2026-10-25T01:45:00.987654321Z", "Europe/Berlin", "Europe/Berlin", true, false},
		{"first fold boundary", "2026-10-25T00:00:00Z", "2026-10-25T00:30:00Z", "Europe/Berlin", "Europe/Berlin", true, false},
		{"wall clock reversal", "2026-10-25T00:30:00Z", "2026-10-25T01:00:00Z", "Europe/Berlin", "", true, false},
		{"end only fold", "2026-10-25T00:00:00Z", "2026-10-25T01:15:00Z", "Asia/Tokyo", "Europe/Berlin", true, false},
		{"start only fold", "2026-10-25T01:15:00Z", "2026-10-25T02:15:00Z", "Europe/Berlin", "Asia/Tokyo", true, false},
		{"half hour fold", "2026-04-04T15:15:00Z", "2026-04-04T16:15:00Z", "Australia/Lord_Howe", "Australia/Lord_Howe", true, false},
		{"before fold", "2026-10-24T23:15:00Z", "2026-10-24T23:45:00Z", "Europe/Berlin", "Europe/Berlin", false, false},
		{"after fold boundary", "2026-10-25T02:00:00Z", "2026-10-25T02:30:00Z", "Europe/Berlin", "Europe/Berlin", false, false},
		{"ordinary timed", "2026-10-26T08:00:00Z", "2026-10-26T09:00:00Z", "Europe/Berlin", "Asia/Tokyo", false, false},
		{"UTC at fold instant", "2026-10-25T01:15:00Z", "2026-10-25T01:45:00Z", "UTC", "UTC", false, false},
		{"all day", "2026-10-25T00:00:00Z", "2026-10-26T00:00:00Z", "", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, _ := time.Parse(time.RFC3339, tc.start)
			end, _ := time.Parse(time.RFC3339, tc.end)
			ev := calendar.Event{Start: start, End: end, TimeZone: tc.startZone, EndTimeZone: tc.endZone, AllDay: tc.allDay}
			endZone := tc.endZone
			if endZone == "" {
				endZone = tc.startZone
			}
			var stored map[string]any
			writes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
					if err := json.NewDecoder(r.Body).Decode(&stored); err != nil {
						t.Error(err)
					}
					if tc.fold {
						for key, want := range map[string]string{"start": tc.start, "end": tc.end} {
							dt := stored[key].(map[string]any)
							if dt["dateTime"] != want || dt["timeZone"] != "UTC" {
								t.Errorf("%s must use unambiguous UTC: %v", key, dt)
							}
						}
						ext, ok := stored["extendedProperties"].(map[string]any)
						if !ok {
							t.Error("missing original-zone metadata")
						} else {
							private := ext["private"].(map[string]any)
							if len(private) != 2 || private["durian.fold.v1.startZone"] != tc.startZone || private["durian.fold.v1.endZone"] != endZone {
								t.Errorf("original zones = %v", private)
							}
						}
					} else if !tc.allDay {
						if stored["start"].(map[string]any)["timeZone"] != tc.startZone || stored["end"].(map[string]any)["timeZone"] != endZone {
							t.Errorf("ordinary zones changed: %v", stored)
						}
					}
					stored["id"] = "event"
					stored["etag"] = `"version"`
					// Apply Google's per-key PATCH deletion semantics, rather than
					// echoing null property values as if they were stored strings.
					if ext, ok := stored["extendedProperties"].(map[string]any); ok {
						for key, value := range ext["private"].(map[string]any) {
							if value == nil {
								delete(ext["private"].(map[string]any), key)
							}
						}
					}
				}
				if strings.HasSuffix(r.URL.Path, "/events") && r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{stored}, "nextSyncToken": "settled"})
				} else {
					_ = json.NewEncoder(w).Encode(stored)
				}
			}))
			defer srv.Close()
			c := testClient(srv)
			ctx := context.Background()
			check := func(got calendar.Event, err error) {
				t.Helper()
				if err != nil || !got.Start.Equal(start) || !got.End.Equal(end) || got.TimeZone != tc.startZone || got.EndTimeZone != endZone || got.AllDay != tc.allDay {
					t.Fatalf("round trip = %+v / %v", got, err)
				}
			}
			check(c.CreateEvent(ctx, "cal", ev, calendarsync.CreateOptions{}))
			if err := c.UpdateEvent(ctx, "cal", "event", calendarsync.UpdateSpec{Event: ev}); err != nil {
				t.Fatal(err)
			}
			check(c.GetEvent(ctx, "cal", "event"))
			masters, err := c.FetchMasterEvents(ctx, "cal")
			if err != nil || len(masters) != 1 {
				t.Fatalf("snapshot = %+v / %v", masters, err)
			}
			check(masters[0], nil)
			delta, err := c.FetchMasterEventsDelta(ctx, "cal", "old")
			if err != nil || delta.Cursor != "settled" || len(delta.ChangedMasters) != 1 {
				t.Fatalf("delta = %+v / %v", delta, err)
			}
			check(delta.ChangedMasters[0], nil)
			if writes != 2 {
				t.Fatalf("writes = %d", writes)
			}
		})
	}
}

func TestTimezoneFoldRecurringWritesRejected(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer srv.Close()
	c := testClient(srv)
	for _, kind := range []string{"parsed rule", "raw rule", "opaque rule", "occurrence", "exception", "recurrence id"} {
		t.Run(kind, func(t *testing.T) {
			ev := calendar.Event{
				Start: time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC),
				End:   time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC), TimeZone: "Europe/Berlin",
			}
			switch kind {
			case "parsed rule":
				ev.Recurrence = testMeetingEvent().Recurrence
			case "raw rule":
				ev.RecurrenceRule = "FREQ=WEEKLY"
			case "opaque rule":
				ev.OpaqueRecurrence = true
			case "recurrence id":
				ev.RecurrenceID = ev.Start
			default:
				ev.Type = kind
			}
			if _, err := c.CreateEvent(context.Background(), "cal", ev, calendarsync.CreateOptions{}); err == nil || !strings.Contains(err.Error(), "ambiguous recurring") {
				t.Errorf("CreateEvent error = %v", err)
			}
			if err := c.UpdateEvent(context.Background(), "cal", "event", calendarsync.UpdateSpec{Event: ev}); err == nil || !strings.Contains(err.Error(), "ambiguous recurring") {
				t.Errorf("UpdateEvent error = %v", err)
			}
		})
	}
	if requests.Load() != 0 {
		t.Errorf("ambiguous recurring writes reached network: %d", requests.Load())
	}
}

func TestTimezoneFoldMalformedMetadataAbortsReads(t *testing.T) {
	for _, kind := range []string{"missing start", "missing end", "empty", "custom", "local", "null", "non-string", "date", "series", "instance", "offsetless", "missing endpoint"} {
		t.Run(kind, func(t *testing.T) {
			private := map[string]any{"durian.fold.v1.startZone": "Europe/Berlin", "durian.fold.v1.endZone": "Asia/Tokyo", "other-app": "keep"}
			bad := map[string]any{
				"id": "bad", "start": map[string]string{"dateTime": "2026-10-25T01:15:00Z", "timeZone": "UTC"},
				"end":                map[string]string{"dateTime": "2026-10-25T02:15:00Z", "timeZone": "UTC"},
				"extendedProperties": map[string]any{"private": private},
			}
			switch kind {
			case "missing start":
				delete(private, "durian.fold.v1.startZone")
			case "missing end":
				delete(private, "durian.fold.v1.endZone")
			case "empty":
				private["durian.fold.v1.endZone"] = ""
			case "custom":
				private["durian.fold.v1.startZone"] = "Custom/Unsupported"
			case "local":
				private["durian.fold.v1.startZone"] = "Local"
			case "null":
				private["durian.fold.v1.startZone"] = nil
			case "non-string":
				private["durian.fold.v1.startZone"] = 123
			case "date":
				bad["start"] = map[string]string{"date": "2026-10-25"}
			case "series":
				bad["recurrence"] = []string{"RRULE:FREQ=WEEKLY"}
			case "instance":
				bad["recurringEventId"] = "master"
			case "offsetless":
				bad["start"] = map[string]string{"dateTime": "2026-10-25T02:15:00", "timeZone": "Europe/Berlin"}
			case "missing endpoint":
				delete(bad, "end")
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/bad") {
					_ = json.NewEncoder(w).Encode(bad)
				} else {
					_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{
						googleEvent{ID: "good", Start: googleDateTime{DateTime: "2026-10-26T08:00:00Z"}, End: googleDateTime{DateTime: "2026-10-26T09:00:00Z"}}, bad,
					}, "nextSyncToken": "must-not-advance"})
				}
			}))
			defer srv.Close()
			c := testClient(srv)
			ctx := context.Background()
			if _, err := c.GetEvent(ctx, "cal", "bad"); err == nil {
				t.Error("GetEvent accepted malformed fold metadata")
			}
			if events, err := c.FetchMasterEvents(ctx, "cal"); err == nil || events != nil {
				t.Errorf("unsafe partial snapshot = %+v / %v", events, err)
			}
			if events, err := c.FetchInstances(ctx, "cal", time.Now(), time.Now().Add(time.Hour)); err == nil || events != nil {
				t.Errorf("unsafe instance snapshot = %+v / %v", events, err)
			}
			if result, err := c.FetchMasterEventsDelta(ctx, "cal", "old"); err == nil || result.Cursor != "" || len(result.ChangedMasters) != 0 {
				t.Errorf("unsafe delta = %+v / %v", result, err)
			}
		})
	}
}

func TestTimezoneFoldMetadataUsesResponseInstants(t *testing.T) {
	// A response may render the same instant with an offset instead of Z.
	// Metadata restores zones, never reinterprets the returned civil clock.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"event",
			"start":{"dateTime":"2026-10-25T02:15:00+01:00","timeZone":"Europe/Berlin"},
			"end":{"dateTime":"2026-10-25T02:45:00+01:00","timeZone":"Europe/Berlin"},
			"extendedProperties":{"private":{"durian.fold.v1.startZone":"Europe/Berlin","durian.fold.v1.endZone":"Asia/Tokyo","other-app":"keep"}}}`))
	}))
	defer srv.Close()
	ev, err := testClient(srv).GetEvent(context.Background(), "cal", "event")
	if err != nil || ev.Start.Format(time.RFC3339) != "2026-10-25T01:15:00Z" || ev.End.Format(time.RFC3339) != "2026-10-25T01:45:00Z" || ev.TimeZone != "Europe/Berlin" || ev.EndTimeZone != "Asia/Tokyo" {
		t.Fatalf("GetEvent = %+v / %v", ev, err)
	}
}

func TestTimezoneFoldTombstoneMayOmitEndpoints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"id":"deleted","status":"cancelled",
			"extendedProperties":{"private":{"durian.fold.v1.startZone":"Europe/Berlin","durian.fold.v1.endZone":"Asia/Tokyo"}}}],"nextSyncToken":"settled"}`))
	}))
	defer srv.Close()
	result, err := testClient(srv).FetchMasterEventsDelta(context.Background(), "cal", "old")
	if err != nil || result.Cursor != "settled" || len(result.RemovedIDs) != 1 || result.RemovedIDs[0] != "deleted" {
		t.Fatalf("delta = %+v / %v", result, err)
	}
}
