package graphcalendar

import (
	"encoding/json"
	"testing"
	"time"
)

// marshalBody round-trips an EventToGraphBody result through JSON so the
// assertions see exactly the wire shape Graph would receive.
func marshalBody(t *testing.T, e Event) map[string]any {
	t.Helper()
	return marshalBodyAttendees(t, e, false)
}

func marshalBodyAttendees(t *testing.T, e Event, includeAttendees bool) map[string]any {
	t.Helper()
	data, err := json.Marshal(EventToGraphBody(e, includeAttendees))
	if err != nil {
		t.Fatalf("marshal graph body: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal graph body: %v", err)
	}
	return m
}

func TestEventToGraphBodyTimed(t *testing.T) {
	m := marshalBody(t, Event{
		Subject:     "Standup",
		Description: "agenda",
		Location:    "HQ",
		Start:       time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC),
		End:         time.Date(2026, 7, 23, 11, 0, 0, 0, time.UTC),
	})

	if m["subject"] != "Standup" {
		t.Errorf("subject = %v", m["subject"])
	}
	if m["isAllDay"] != false {
		t.Errorf("isAllDay = %v, want false", m["isAllDay"])
	}
	start, _ := m["start"].(map[string]any)
	if start["dateTime"] != "2026-07-23T10:00:00" || start["timeZone"] != "UTC" {
		t.Errorf("start = %v, want 2026-07-23T10:00:00 UTC", start)
	}
	end, _ := m["end"].(map[string]any)
	if end["dateTime"] != "2026-07-23T11:00:00" || end["timeZone"] != "UTC" {
		t.Errorf("end = %v, want 2026-07-23T11:00:00 UTC", end)
	}
	body, _ := m["body"].(map[string]any)
	if body["contentType"] != "text" || body["content"] != "agenda" {
		t.Errorf("body = %v, want text/agenda", body)
	}
	location, _ := m["location"].(map[string]any)
	if location["displayName"] != "HQ" {
		t.Errorf("location = %v, want HQ", location)
	}
	// recurrence must be present as an explicit null so a PATCH clears a
	// dropped RRULE.
	if rec, ok := m["recurrence"]; !ok || rec != nil {
		t.Errorf("recurrence = %v (present=%v), want explicit null", rec, ok)
	}
}

func TestEventToGraphBodyAllDay(t *testing.T) {
	m := marshalBody(t, Event{
		Subject: "Holiday",
		AllDay:  true,
		Start:   time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC),
		End:     time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC),
	})

	if m["isAllDay"] != true {
		t.Errorf("isAllDay = %v, want true", m["isAllDay"])
	}
	start, _ := m["start"].(map[string]any)
	if start["dateTime"] != "2026-07-23T00:00:00" || start["timeZone"] != "UTC" {
		t.Errorf("start = %v, want midnight UTC", start)
	}
	end, _ := m["end"].(map[string]any)
	if end["dateTime"] != "2026-07-24T00:00:00" || end["timeZone"] != "UTC" {
		t.Errorf("end = %v, want next-day midnight UTC (exclusive)", end)
	}
}

func TestEventToGraphBodyAllDaySnap(t *testing.T) {
	// Graph rejects an all-day event spanning less than one full day; the
	// write boundary must snap such events to a one-day span so they can
	// never reach Graph malformed.
	cases := []struct {
		name       string
		start, end time.Time
		wantStart  string
		wantEnd    string
	}{
		{
			name:      "same-instant end snaps to next day",
			start:     time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC),
			end:       time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC),
			wantStart: "2026-07-23T00:00:00",
			wantEnd:   "2026-07-24T00:00:00",
		},
		{
			name:      "sub-day timed span snaps to next day",
			start:     time.Date(2026, 7, 23, 9, 0, 0, 0, time.UTC),
			end:       time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC),
			wantStart: "2026-07-23T00:00:00",
			wantEnd:   "2026-07-24T00:00:00",
		},
		{
			name:      "end before start snaps to next day",
			start:     time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC),
			end:       time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC),
			wantStart: "2026-07-23T00:00:00",
			wantEnd:   "2026-07-24T00:00:00",
		},
		{
			name:      "cross-midnight sub-24h span keeps its day boundary",
			start:     time.Date(2026, 7, 23, 20, 0, 0, 0, time.UTC),
			end:       time.Date(2026, 7, 24, 2, 0, 0, 0, time.UTC),
			wantStart: "2026-07-23T00:00:00",
			wantEnd:   "2026-07-24T00:00:00",
		},
		{
			name:      "multi-day span preserved",
			start:     time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC),
			end:       time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC),
			wantStart: "2026-07-23T00:00:00",
			wantEnd:   "2026-07-26T00:00:00",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := marshalBody(t, Event{Subject: "Holiday", AllDay: true, Start: c.start, End: c.end})
			start, _ := m["start"].(map[string]any)
			end, _ := m["end"].(map[string]any)
			if start["dateTime"] != c.wantStart || end["dateTime"] != c.wantEnd {
				t.Errorf("start/end = %v / %v, want %s / %s", start["dateTime"], end["dateTime"], c.wantStart, c.wantEnd)
			}
		})
	}
}

func TestEventToGraphBodyWeeklyRecurrence(t *testing.T) {
	m := marshalBody(t, Event{
		Subject: "Weekly sync",
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
				EndDate:   "2026-12-31",
			},
		},
	})

	rec, _ := m["recurrence"].(map[string]any)
	if rec == nil {
		t.Fatalf("recurrence missing: %v", m)
	}
	pattern, _ := rec["pattern"].(map[string]any)
	if pattern["type"] != "weekly" || pattern["interval"] != float64(2) {
		t.Errorf("pattern = %v, want weekly interval 2", pattern)
	}
	days, _ := pattern["daysOfWeek"].([]any)
	if len(days) != 2 || days[0] != "monday" || days[1] != "wednesday" {
		t.Errorf("daysOfWeek = %v, want [monday wednesday]", days)
	}
	rng, _ := rec["range"].(map[string]any)
	if rng["type"] != "endDate" || rng["startDate"] != "2026-07-20" || rng["endDate"] != "2026-12-31" {
		t.Errorf("range = %v, want endDate 2026-07-20..2026-12-31", rng)
	}
}

func TestEventToGraphBodyAttendeeGating(t *testing.T) {
	meeting := Event{
		Subject: "Design review",
		Start:   time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC),
		End:     time.Date(2026, 7, 23, 11, 0, 0, 0, time.UTC),
		Attendees: []Attendee{
			{Name: "Alice Example", Email: "alice@example.com", Type: "required", Response: "accepted"},
			{Name: "Bob Example", Email: "bob@example.com", Type: "optional", Response: "declined"},
		},
		Organizer:            &Person{Name: "Olivia Organizer", Email: "olivia@example.com"},
		IsOrganizer:          true,
		IsCancelled:          true,
		IsOnlineMeeting:      true,
		OnlineMeetingURL:     "https://teams.microsoft.com/l/meetup-join/abc123",
		OwnerResponse:        OwnerRespAccepted,
		RequestOnlineMeeting: true,
	}

	// Without includeAttendees (non-organizer writes) the body must contain
	// ONLY the core fields — no attendee/organizer/RSVP/meeting keys, so a
	// PATCH can never trigger scheduling mail.
	m := marshalBodyAttendees(t, meeting, false)
	allowed := map[string]bool{
		"subject": true, "body": true, "start": true, "end": true,
		"isAllDay": true, "location": true, "recurrence": true,
	}
	for key := range m {
		if !allowed[key] {
			t.Errorf("attendee-free graph body contains forbidden key %q", key)
		}
	}

	// With includeAttendees (organizer writes) ONLY the attendee list is
	// added: never the organizer, responses, cancellation or online-meeting
	// state — Graph owns those.
	m = marshalBodyAttendees(t, meeting, true)
	attendees, ok := m["attendees"].([]any)
	if !ok || len(attendees) != 2 {
		t.Fatalf("attendees = %v, want 2 entries", m["attendees"])
	}
	first, _ := attendees[0].(map[string]any)
	addr, _ := first["emailAddress"].(map[string]any)
	if first["type"] != "required" || addr["address"] != "alice@example.com" || addr["name"] != "Alice Example" {
		t.Errorf("attendee[0] = %v, want required alice@example.com", first)
	}
	if _, ok := first["status"]; ok {
		t.Error("attendee upload must not carry a response status")
	}
	for _, forbidden := range []string{
		"organizer", "responseStatus",
		"isOnlineMeeting", "onlineMeeting", "onlineMeetingUrl", "onlineMeetingProvider",
		"isCancelled", "isOrganizer", "transactionId",
	} {
		if _, ok := m[forbidden]; ok {
			t.Errorf("graph body must not contain %q", forbidden)
		}
	}
}
