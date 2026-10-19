package graphcalendar

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

// Loopback verifies the payload contract, not live Graph acceptance.
func TestTimezoneWeeklyProviderRoundTrip(t *testing.T) {
	for _, tc := range []struct{ day, utcHour string }{
		{"2026-03-23", "08"}, {"2026-03-30", "07"},
		{"2026-10-19", "07"}, {"2026-10-26", "08"},
	} {
		for _, rangeZone := range []string{"", "Romance Standard Time"} {
			t.Run(tc.day+"/"+rangeZone, func(t *testing.T) {
				master := graphEvent{
					ID: "master", Type: "seriesMaster",
					Start:                 graphDateTime{DateTime: tc.day + "T" + tc.utcHour + ":00:00.0000000", TimeZone: "UTC"},
					End:                   graphDateTime{DateTime: tc.day + "T10:00:00.0000000", TimeZone: "UTC"},
					OriginalStartTimeZone: "W. Europe Standard Time", OriginalEndTimeZone: "Tokyo Standard Time",
					Recurrence: &calendar.Recurrence{
						Pattern: calendar.RecurrencePattern{Type: "weekly", Interval: 1, DaysOfWeek: []string{"monday"}},
						Range:   calendar.RecurrenceRange{Type: "endDate", StartDate: tc.day, EndDate: "2026-11-02", TimeZone: rangeZone},
					},
				}
				moved := graphEvent{
					ID: "override", Type: "exception", SeriesMasterID: "master", OriginalStart: "2026-10-26T09:00:00+01:00",
					Start:                 graphDateTime{DateTime: "2026-10-26T09:00:00", TimeZone: "UTC"},
					End:                   graphDateTime{DateTime: "2026-10-26T11:00:00", TimeZone: "UTC"},
					OriginalStartTimeZone: "W. Europe Standard Time", OriginalEndTimeZone: "Tokyo Standard Time",
				}
				wantRangeZone := "Europe/Berlin"
				if rangeZone != "" {
					wantRangeZone = "Europe/Paris"
				}
				var writes atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if !strings.Contains(r.Header.Get("Prefer"), `outlook.timezone="UTC"`) {
						t.Errorf("missing UTC preference: %s", r.Header.Get("Prefer"))
					}
					if r.Method == http.MethodGet {
						selectFields := r.URL.Query().Get("$select")
						if !strings.Contains(selectFields, "originalStartTimeZone") || !strings.Contains(selectFields, "originalEndTimeZone") {
							t.Errorf("missing original zones in select: %s", selectFields)
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"value": []graphEvent{moved, master}})
						return
					}
					writes.Add(1)
					var body graphEvent
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					berlin, _ := time.LoadLocation("Europe/Berlin")
					tokyo, _ := time.LoadLocation("Asia/Tokyo")
					start, err := time.ParseInLocation(graphWriteDateFormat, body.Start.DateTime, berlin)
					if err != nil || start.UTC().Format(time.RFC3339) != tc.day+"T"+tc.utcHour+":00:00Z" || body.Start.TimeZone != "Europe/Berlin" || start.Hour() != 9 {
						t.Errorf("start contract = %+v / %v / %v", body.Start, start, err)
					}
					end, err := time.ParseInLocation(graphWriteDateFormat, body.End.DateTime, tokyo)
					if err != nil || end.UTC().Format(time.RFC3339) != tc.day+"T10:00:00Z" || body.End.TimeZone != "Asia/Tokyo" || end.Hour() != 19 {
						t.Errorf("end contract = %+v / %v / %v", body.End, end, err)
					}
					if body.Recurrence == nil || body.Recurrence.Range.TimeZone != wantRangeZone || body.Recurrence.Range.StartDate != tc.day || body.Recurrence.Pattern.Type != "weekly" {
						t.Errorf("recurrence contract = %+v", body.Recurrence)
					}
					body.ID = "created"
					_ = json.NewEncoder(w).Encode(body)
				}))
				defer srv.Close()
				c := testClient(srv)
				ctx := context.Background()
				events, err := c.FetchMasterEvents(ctx, "cal")
				if err != nil || len(events) != 1 {
					t.Fatalf("FetchMasterEvents = %+v / %v", events, err)
				}
				ev := events[0]
				if ev.TimeZone != "Europe/Berlin" || ev.EndTimeZone != "Asia/Tokyo" || ev.Start.Format(time.RFC3339) != tc.day+"T"+tc.utcHour+":00:00Z" || ev.Recurrence == nil || ev.Recurrence.Range.TimeZone != wantRangeZone {
					t.Fatalf("lost timezone: %+v", ev)
				}
				if len(ev.Overrides) != 1 || ev.Overrides[0].RecurrenceID.Format(time.RFC3339) != "2026-10-26T08:00:00Z" || ev.Overrides[0].Start.Format(time.RFC3339) != "2026-10-26T09:00:00Z" || ev.Overrides[0].TimeZone != "Europe/Berlin" || ev.Overrides[0].EndTimeZone != "Asia/Tokyo" {
					t.Fatalf("moved override = %+v", ev.Overrides)
				}
				created, err := c.CreateEvent(ctx, "cal", ev, calendarsync.CreateOptions{})
				if err != nil || created.TimeZone != ev.TimeZone || !created.Start.Equal(ev.Start) {
					t.Fatalf("CreateEvent = %+v / %v", created, err)
				}
				if err := c.UpdateEvent(ctx, "cal", "master", calendarsync.UpdateSpec{Event: ev}); err != nil {
					t.Fatal(err)
				}
				if writes.Load() != 2 {
					t.Errorf("writes = %d", writes.Load())
				}
			})
		}
	}
}

func TestTimezoneOffsetlessAndAllDayControls(t *testing.T) {
	for _, tc := range []struct {
		dt, zone, original, want string
		allDay                   bool
	}{
		{"2026-03-30T09:00:00", "W. Europe Standard Time", "", "2026-03-30T07:00:00Z", false},
		{"2026-03-30T09:00:00-04:00", "Europe/Berlin", "", "2026-03-30T13:00:00Z", false},
		{"2026-10-25T02:30:00+01:00", "Europe/Berlin", "", "2026-10-25T01:30:00Z", false},
		{"2026-03-29T22:00:00", "UTC", "W. Europe Standard Time", "2026-03-30T00:00:00Z", true},
		{"2026-03-30T00:00:00", "W. Europe Standard Time", "", "2026-03-30T00:00:00Z", true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(graphEvent{ID: "event", Start: graphDateTime{tc.dt, tc.zone}, End: graphDateTime{tc.dt, tc.zone}, OriginalStartTimeZone: tc.original, OriginalEndTimeZone: tc.original, IsAllDay: tc.allDay})
		}))
		c := testClient(srv)
		ev, err := c.GetEvent(context.Background(), "cal", "event")
		if err != nil || ev.Start.Format(time.RFC3339) != tc.want {
			t.Fatalf("GetEvent = %+v / %v, want %s", ev, err, tc.want)
		}
		body := EventToGraphBody(ev, false)
		if tc.allDay && (ev.TimeZone != "" || ev.EndTimeZone != "" || body["start"].(map[string]string)["dateTime"] != "2026-03-30T00:00:00") {
			t.Errorf("all-day converted into instant: %+v / %v", ev, body)
		}
		if tc.want == "2026-10-25T01:30:00Z" {
			if body != nil {
				t.Errorf("ambiguous Graph write did not fail closed: %v", body)
			}
		} else if !tc.allDay {
			boundary := body["start"].(map[string]string)
			loc, _ := time.LoadLocation(boundary["timeZone"])
			written, err := time.ParseInLocation(graphWriteDateFormat, boundary["dateTime"], loc)
			if err != nil || !written.Equal(ev.Start) {
				t.Errorf("write changed instant: %v / %v", boundary, err)
			}
		}
		srv.Close()
	}
}

func TestTimezoneCustomZonesAndFloatingFailSafely(t *testing.T) {
	for _, field := range []string{"originalStartTimeZone", "originalEndTimeZone", "recurrence"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			event := map[string]any{"id": "existing", "type": "singleInstance", field: "tzone://Microsoft/Custom"}
			if field == "recurrence" {
				event[field] = map[string]any{"range": map[string]string{"recurrenceTimeZone": "tzone://Microsoft/Custom"}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{event}})
		}))
		c := testClient(srv)
		if events, err := c.FetchMasterEvents(context.Background(), "cal"); err == nil || events != nil {
			t.Errorf("unsafe partial snapshot = %+v / %v", events, err)
		}
		srv.Close()
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer srv.Close()
	c := testClient(srv)
	for _, ev := range []calendar.Event{
		{Floating: true}, {TimeZone: "tzone://Microsoft/Custom"},
		{TimeZone: "Europe/Berlin", Start: time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC)},
		{TimeZone: "Europe/Berlin", Start: time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC)},
		{EndTimeZone: "Europe/Berlin", End: time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC)},
	} {
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

func TestTimezoneCLDRMappingLoads(t *testing.T) {
	// Validate every generated entry against the installed IANA database.
	if len(windowsZones) != 139 {
		t.Fatalf("incomplete CLDR 48 global mapping: %d", len(windowsZones))
	}
	for windows, iana := range windowsZones {
		if _, err := time.LoadLocation(iana); err != nil {
			t.Errorf("%s -> %s: %v", windows, iana, err)
		}
	}
}

func TestTimezoneBodyInheritsEndAndRecurrenceZone(t *testing.T) {
	ev := calendar.Event{
		Start:      time.Date(2026, 3, 23, 8, 0, 0, 0, time.UTC),
		End:        time.Date(2026, 3, 23, 9, 0, 0, 0, time.UTC),
		TimeZone:   "Europe/Berlin",
		Recurrence: &calendar.Recurrence{Range: calendar.RecurrenceRange{Type: "noEnd", StartDate: "2026-03-23"}},
	}
	body := EventToGraphBody(ev, false)
	if body["end"].(map[string]string)["timeZone"] != "Europe/Berlin" || body["recurrence"].(*calendar.Recurrence).Range.TimeZone != "Europe/Berlin" {
		t.Fatalf("lost inherited zones: %v", body)
	}
	if ev.Recurrence.Range.TimeZone != "" {
		t.Error("body builder mutated its caller's recurrence")
	}
}
