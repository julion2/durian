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
	data, err := json.Marshal(EventToGraphBody(e))
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
