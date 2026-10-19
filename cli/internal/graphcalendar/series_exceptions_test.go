package graphcalendar

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
)

// Fixture components for a master, modified occurrence and ordinary occurrence.
// Real /events lists the master only; exceptions come from GET master.
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
			"originalStart": "2026-08-17T09:00:00Z",
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
			"originalStart": "2026-08-24T09:00:00+02:00",
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
	var fixture eventPage
	if err := json.Unmarshal([]byte(seriesWithExceptionPage), &fixture); err != nil {
		t.Fatal(err)
	}
	masterDetail := fixture.Value[2]
	masterDetail.ExceptionOccurrences = fixture.Value[:1]
	masterDetail.CancelledOccurrences = []string{"OID.evt-series.2026-08-24"}
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
		_ = json.NewEncoder(w).Encode(eventPage{Value: []graphEvent{fixture.Value[2]}})
	})
	mux.HandleFunc("/me/events/evt-series", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("$expand") != "" && r.URL.Query().Get("$expand") != "exceptionOccurrences" {
			t.Error("unexpected expansion")
		}
		_ = json.NewEncoder(w).Encode(masterDetail)
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
	if len(master.ExceptionDates) != 1 || master.ExceptionDates[0].Format(time.RFC3339) != "2026-08-24T09:00:00Z" {
		t.Fatalf("cancellation lost: %v", master.ExceptionDates)
	}
	readBack, err := client.GetEvent(context.Background(), "cal1", master.ID)
	if err != nil || !reflect.DeepEqual(readBack, master) {
		t.Fatalf("GetEvent differs from snapshot: %+v / %v", readBack, err)
	}
	ics, err := calendar.EventToICal(master)
	if err != nil {
		t.Fatal(err)
	}
	back, err := calendar.ICalToEvent(ics, "owner@example.com")
	if err != nil || len(back.Overrides) != 1 || len(back.ExceptionDates) != 1 || !back.Overrides[0].RecurrenceID.Equal(override.RecurrenceID) || !back.ExceptionDates[0].Equal(master.ExceptionDates[0]) {
		t.Fatalf("ICS lost exclusions: %+v / %v", back, err)
	}
}

// A surprising orphan is not a safe empty snapshot: do not promote it to an
// independent event or silently discard a known exclusion.
func TestFetchMasterEventsRejectsOrphanExceptions(t *testing.T) {
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
				"originalStart": "2026-08-17T09:00:00Z",
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
	if err == nil || events != nil {
		t.Errorf("expected snapshot failure, got %+v / %v", events, err)
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

// originalStart is a DateTimeOffset and arrives with its offset inline, unlike
// start/end which are bare local times with the zone in a sibling field. The
// bare layout does not degrade on the offset form, it fails outright — which
// silently dropped every exception Graph reported.
func TestParseGraphDateTimeAcceptsBothWireShapes(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Time
	}{
		{"offset form, Z", "2026-08-17T09:00:00Z",
			time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)},
		{"offset form, numeric", "2026-08-17T09:00:00+02:00",
			time.Date(2026, 8, 17, 7, 0, 0, 0, time.UTC)},
		{"bare local form", "2026-08-17T09:00:00.0000000",
			time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)},
		{"bare local form, no fraction", "2026-08-17T09:00:00",
			time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGraphDateTime(tc.value)
			if err != nil {
				t.Fatalf("parseGraphDateTime(%q): %v", tc.value, err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("got %s, want %s", got.UTC(), tc.want)
			}
		})
	}
}

func exclusionFixture(t *testing.T) graphEvent {
	t.Helper()
	var page eventPage
	if err := json.Unmarshal([]byte(seriesWithExceptionPage), &page); err != nil {
		t.Fatal(err)
	}
	master := page.Value[2]
	master.ExceptionOccurrences = page.Value[:1]
	master.CancelledOccurrences = []string{"OID.evt-series.2026-08-24"}
	return master
}

// Both read APIs must fail, returning no partial event/snapshot even when an
// otherwise valid single event preceded the bad series in the collection.
func TestSeriesExclusionsRejectIncompleteSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*graphEvent)
	}{
		{"wrong-master-id", func(m *graphEvent) { m.ID = "another-series" }},
		{"missing-rule", func(m *graphEvent) { m.Recurrence = nil }},
		{"missing-expanded-exceptions", func(m *graphEvent) { m.ExceptionOccurrences = nil }},
		{"missing-cancellations", func(m *graphEvent) { m.CancelledOccurrences = nil }},
		{"missing-original", func(m *graphEvent) { m.ExceptionOccurrences[0].OriginalStart = "" }},
		{"invalid-original", func(m *graphEvent) { m.ExceptionOccurrences[0].OriginalStart = "2026-08-17T09:00:00" }},
		{"foreign-exception", func(m *graphEvent) { m.ExceptionOccurrences[0].SeriesMasterID = "other" }},
		{"unknown-exception-type", func(m *graphEvent) { m.ExceptionOccurrences[0].Type = "occurrence" }},
		{"bad-exception-time", func(m *graphEvent) { m.ExceptionOccurrences[0].Start.DateTime = "invalid" }},
		{"bad-exception-zone", func(m *graphEvent) { m.ExceptionOccurrences[0].OriginalStartTimeZone = "custom-zone" }},
		{"opaque-cancellation", func(m *graphEvent) { m.CancelledOccurrences = []string{"opaque"} }},
		{"foreign-cancellation", func(m *graphEvent) { m.CancelledOccurrences = []string{"OID.evt-series-other.2026-08-24"} }},
		{"bad-cancellation-date", func(m *graphEvent) { m.CancelledOccurrences = []string{"OID.evt-series.2026-02-30"} }},
		{"cancellation-date-suffix", func(m *graphEvent) { m.CancelledOccurrences = []string{"OID.evt-series.2026-08-24.extra"} }},
		{"overlap", func(m *graphEvent) { m.CancelledOccurrences = []string{"OID.evt-series.2026-08-17"} }},
		{"disagreeing-identities", func(m *graphEvent) { m.ExceptionOccurrences[0].OccurrenceID = "OID.evt-series.2026-08-18" }},
		{"foreign-oid-with-valid-original", func(m *graphEvent) { m.ExceptionOccurrences[0].OccurrenceID = "OID.other.2026-08-17" }},
		{"conflicting-duplicate-id", func(m *graphEvent) {
			other := m.ExceptionOccurrences[0]
			other.Subject = "changed"
			m.ExceptionOccurrences = append(m.ExceptionOccurrences, other)
		}},
		{"conflicting-duplicate-original", func(m *graphEvent) {
			other := m.ExceptionOccurrences[0]
			other.ID = "another-exception"
			m.ExceptionOccurrences = append(m.ExceptionOccurrences, other)
		}},
		{"foreign-exception-page", func(m *graphEvent) { m.ExceptionsNextLink = "https://example.test/steal" }},
		{"unknown-cancellation-page", func(m *graphEvent) { m.CancelledNextLink = "https://example.test/steal" }},
		{"unknown-master-page", func(m *graphEvent) { m.NextLink = "https://example.test/steal" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detail := exclusionFixture(t)
			plain := detail
			single := plain
			single.ID, single.Type, single.Recurrence = "single", "singleInstance", nil
			tc.mutate(&detail)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/me/calendars/cal/events" {
					_ = json.NewEncoder(w).Encode(eventPage{Value: []graphEvent{single, plain}})
				} else if r.URL.Path == "/me/events/evt-series" {
					if r.URL.Query().Get("$expand") == "" {
						_ = json.NewEncoder(w).Encode(plain)
					} else {
						_ = json.NewEncoder(w).Encode(detail)
					}
				} else {
					t.Error("followed untrusted or unknown resource")
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			c := testClient(srv)
			if events, err := c.FetchMasterEvents(context.Background(), "cal"); err == nil || events != nil {
				t.Fatalf("partial snapshot: %+v / %v", events, err)
			}
			if event, err := c.GetEvent(context.Background(), "cal", "evt-series"); err == nil || event.ID != "" {
				t.Fatalf("partial event: %+v / %v", event, err)
			}
		})
	}
}

func TestSeriesExclusionsPagingDuplicatesAndOrdering(t *testing.T) {
	for _, cycle := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "cycle"}[cycle], func(t *testing.T) {
			detail := exclusionFixture(t)
			first := detail.ExceptionOccurrences[0]
			second := first
			second.ID, second.OriginalStart = "second", "2026-08-10T09:00:00Z"
			// The expanded response supplies one exception, the next page supplies
			// an earlier exception AND the same first ID again (page overlap).
			detail.CancelledOccurrences = []string{"OID.evt-series.2026-08-31", "OID.evt-series.2026-08-24", "OID.evt-series.2026-08-24"}
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Prefer") != preferMaster {
					t.Error("lost read preferences")
				}
				switch r.URL.Path {
				case "/me/calendars/cal/events":
					page := eventPage{Value: []graphEvent{detail}}
					if r.URL.Query().Get("page") == "" {
						page.NextLink = srv.URL + "/me/calendars/cal/events?page=2"
					}
					_ = json.NewEncoder(w).Encode(page)
				case "/me/events/evt-series":
					out := detail
					out.ExceptionsNextLink = srv.URL + "/me/events/evt-series/exceptionOccurrences?page=2"
					_ = json.NewEncoder(w).Encode(out)
				case "/me/events/evt-series/exceptionOccurrences":
					page := eventPage{Value: []graphEvent{second, first}}
					if cycle {
						page.NextLink = srv.URL + "/me/events/evt-series/exceptionOccurrences?page=2"
					}
					_ = json.NewEncoder(w).Encode(page)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			c := testClient(srv)
			events, err := c.FetchMasterEvents(context.Background(), "cal")
			if cycle {
				if err == nil || events != nil {
					t.Fatal("cyclic page produced a partial snapshot")
				}
				return
			}
			if err != nil || len(events) != 1 {
				t.Fatalf("snapshot: %+v / %v", events, err)
			}
			got := events[0]
			if len(got.Overrides) != 2 || got.Overrides[0].ID != "second" || len(got.ExceptionDates) != 2 || got.ExceptionDates[0].Day() != 24 {
				t.Fatalf("unstable or duplicated exclusions: %+v", got)
			}
			// A documented occurrenceId is sufficient when originalStart is
			// absent; use the original clock, never the moved start.
			detail.ExceptionOccurrences[0].OriginalStart = ""
			detail.ExceptionOccurrences[0].OccurrenceID = "OID.evt-series.2026-08-17"
			// Remove the overlap copy because it still has originalStart and
			// would correctly count as an inconsistent same-ID response.
			first = detail.ExceptionOccurrences[0]
			back, err := c.GetEvent(context.Background(), "cal", "evt-series")
			if err != nil || !reflect.DeepEqual(back, got) {
				t.Fatalf("OID read differs: %+v / %v", back, err)
			}
		})
	}
}
