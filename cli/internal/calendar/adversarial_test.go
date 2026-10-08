package calendar

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This is an RFC 5545 wall-clock sequence crossing the Europe/Berlin spring
// transition. Expectations are built with time.Date in that location rather
// than by using the recurrence implementation under test. The EXDATE is the
// provider tombstone for the transition-day occurrence.
func TestAdversarialBerlinDSTRecurrenceAndTombstone(t *testing.T) {
	const doc = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//adversarial//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:berlin-weekly\r\nDTSTAMP:20260301T000000Z\r\n" +
		"DTSTART;TZID=Europe/Berlin:20260322T090000\r\n" +
		"DTEND;TZID=Europe/Berlin:20260322T100000\r\n" +
		"RRULE:FREQ=WEEKLY;COUNT=3\r\n" +
		"EXDATE;TZID=Europe/Berlin:20260329T090000\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	master, err := ICalToEvent([]byte(doc), "")
	if err != nil {
		t.Fatalf("ICalToEvent: %v", err)
	}
	got := ExpandOccurrences(master,
		time.Date(2026, 3, 21, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC))

	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("load Europe/Berlin: %v", err)
	}
	want := []time.Time{
		time.Date(2026, 3, 22, 9, 0, 0, 0, berlin).UTC(),
		time.Date(2026, 4, 5, 9, 0, 0, 0, berlin).UTC(),
	}
	if len(got) != len(want) {
		t.Fatalf("starts = %v, want %v; transition-day EXDATE must cancel exactly one occurrence", starts(got), formatInstants(want))
	}
	for i := range want {
		if !got[i].Start.Equal(want[i]) {
			t.Errorf("occurrence %d = %s, want %s (09:00 Europe/Berlin)", i, got[i].Start, want[i])
		}
	}
}

func TestZonedRecurrenceRoundTripAcrossFallDST(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 18, 9, 0, 0, 0, berlin)
	event := Event{
		ICalUID: "berlin-fall", Start: start, End: start.Add(time.Hour),
		Recurrence: &Recurrence{
			Pattern: RecurrencePattern{Type: "weekly", Interval: 1, DaysOfWeek: []string{"sunday"}},
			Range: RecurrenceRange{Type: "numbered", StartDate: "2026-10-18",
				NumberOfOccurrences: 4, TimeZone: "Europe/Berlin"},
		},
	}
	data, err := EventToICal(event)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "DTSTART;TZID=Europe/Berlin:20261018T090000") {
		t.Fatalf("serialized event lost TZID:\n%s", data)
	}
	back, err := ICalToEvent(data, "")
	if err != nil {
		t.Fatal(err)
	}
	if back.Start.Location().String() != "Europe/Berlin" || back.Recurrence.Range.TimeZone != "Europe/Berlin" {
		t.Fatalf("round trip lost zone: start=%s recurrence=%+v", back.Start.Location(), back.Recurrence)
	}
	got := ExpandOccurrences(back,
		time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC))
	if len(got) != 4 {
		t.Fatalf("occurrences = %v, want four", starts(got))
	}
	for i, occurrence := range got {
		if occurrence.Start.In(berlin).Hour() != 9 {
			t.Errorf("occurrence %d = %s, want 09:00 Europe/Berlin", i, occurrence.Start)
		}
	}
}

func TestRecurrenceEndDateUsesSeriesCivilDate(t *testing.T) {
	losAngeles, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	rec := Recurrence{
		Pattern: RecurrencePattern{Type: "daily", Interval: 1},
		Range: RecurrenceRange{Type: "endDate", StartDate: "2026-01-01",
			EndDate: "2026-01-31", TimeZone: "America/Los_Angeles"},
	}
	opt, err := RecurrenceToROption(rec)
	if err != nil {
		t.Fatal(err)
	}
	// The final local second is on the following UTC date.
	if got := opt.Until.UTC().Format(time.RFC3339); got != "2026-02-01T07:59:59Z" {
		t.Fatalf("until = %s, want local end-of-day across UTC boundary", got)
	}
	back, err := ROptionToRecurrence(opt, time.Date(2026, 1, 1, 9, 0, 0, 0, losAngeles))
	if err != nil {
		t.Fatal(err)
	}
	if back.Range.StartDate != "2026-01-01" || back.Range.EndDate != "2026-01-31" ||
		back.Range.TimeZone != "America/Los_Angeles" {
		t.Errorf("range = %+v, want original local date boundaries", back.Range)
	}
}

func TestAdversarialAllDayTombstoneKeepsDateAcrossBerlinDST(t *testing.T) {
	const doc = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//adversarial//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:berlin-all-day\r\nDTSTAMP:20260301T000000Z\r\n" +
		"DTSTART;VALUE=DATE:20260322\r\nDTEND;VALUE=DATE:20260323\r\n" +
		"RRULE:FREQ=WEEKLY;COUNT=3\r\n" +
		"EXDATE;TZID=Europe/Berlin;VALUE=DATE:20260329\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	master, err := ICalToEvent([]byte(doc), "")
	if err != nil {
		t.Fatalf("ICalToEvent: %v", err)
	}
	got := ExpandOccurrences(master,
		time.Date(2026, 3, 21, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC))
	want := []string{"2026-03-22T00:00:00Z", "2026-04-05T00:00:00Z"}
	if actual := starts(got); !reflect.DeepEqual(actual, want) {
		t.Errorf("all-day starts = %v, want %v; TZID must not move VALUE=DATE", actual, want)
	}
	for _, occurrence := range got {
		if !occurrence.AllDay || occurrence.End.Sub(occurrence.Start) != 24*time.Hour {
			t.Errorf("all-day occurrence changed value type/boundary: %+v", occurrence)
		}
	}
}

func formatInstants(values []time.Time) []string {
	out := make([]string, len(values))
	for i := range values {
		out[i] = values[i].UTC().Format(time.RFC3339)
	}
	return out
}

// FuzzAdversarialICSRoundTripExceptionInstants checks an independently built
// invariant: serializing then parsing may reorder exception records, but it
// must preserve every cancellation instant and every override identity and
// actual interval. Inputs are constrained to valid calendar values so parser
// rejection cannot make the property vacuous.
func FuzzAdversarialICSRoundTripExceptionInstants(f *testing.F) {
	f.Add(uint64(0), false, uint8(2), int8(3))
	f.Add(uint64(0xfeedbeef), true, uint8(4), int8(-2))
	f.Add(uint64(0x12345678), false, uint8(1), int8(12))

	f.Fuzz(func(t *testing.T, seed uint64, allDay bool, rawCount uint8, rawMove int8) {
		year := 2024 + int(seed%12)
		month := time.Month(1 + (seed/12)%12)
		day := 1 + int((seed/144)%20)
		hour := int((seed / 2880) % 23)
		start := time.Date(year, month, day, hour, 0, 0, 0, time.UTC)
		duration := time.Duration(30+int(seed%150)) * time.Minute
		if allDay {
			start = time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
			duration = 24 * time.Hour
		}
		master := Event{
			ICalUID: "fuzz-series",
			Subject: fmt.Sprintf("series-%x", seed),
			Start:   start,
			End:     start.Add(duration),
			AllDay:  allDay,
			Recurrence: &Recurrence{
				Pattern: RecurrencePattern{Type: "weekly", Interval: 1, DaysOfWeek: []string{weekdayName(start.Weekday())}},
				Range:   RecurrenceRange{Type: "numbered", StartDate: start.Format(GraphDateFormat), NumberOfOccurrences: 8},
			},
		}
		count := 1 + int(rawCount%4)
		for i := 0; i < count; i++ {
			master.ExceptionDates = append(master.ExceptionDates, start.AddDate(0, 0, 7*(i+1)))
		}
		recurrenceID := start.AddDate(0, 0, 7*6)
		move := int(rawMove)%25 - 12
		overrideStart := recurrenceID.Add(time.Duration(move) * time.Hour)
		if allDay {
			overrideStart = recurrenceID.AddDate(0, 0, move%5)
		}
		master.Overrides = []Event{{
			ICalUID:      master.ICalUID,
			Subject:      "moved",
			AllDay:       allDay,
			RecurrenceID: recurrenceID,
			Start:        overrideStart,
			End:          overrideStart.Add(duration),
		}}

		data, err := EventToICal(master)
		if err != nil {
			t.Fatalf("EventToICal: %v", err)
		}
		back, err := ICalToEvent(data, "")
		if err != nil {
			t.Fatalf("ICalToEvent: %v\n%s", err, data)
		}
		if back.ICalUID != master.ICalUID || back.AllDay != master.AllDay ||
			!back.Start.Equal(master.Start) || !back.End.Equal(master.End) {
			t.Fatalf("master changed: got=%+v want=%+v", back, master)
		}
		if back.Recurrence == nil || back.Recurrence.Pattern.Type != "weekly" ||
			back.Recurrence.Range.Type != "numbered" || back.Recurrence.Range.NumberOfOccurrences != 8 {
			t.Fatalf("recurrence changed: %+v", back.Recurrence)
		}
		if !reflect.DeepEqual(formatInstants(back.ExceptionDates), formatInstants(master.ExceptionDates)) {
			t.Fatalf("exception instants changed: got=%v want=%v", formatInstants(back.ExceptionDates), formatInstants(master.ExceptionDates))
		}
		if len(back.Overrides) != 1 ||
			!back.Overrides[0].RecurrenceID.Equal(recurrenceID) ||
			!back.Overrides[0].Start.Equal(overrideStart) ||
			!back.Overrides[0].End.Equal(overrideStart.Add(duration)) {
			t.Fatalf("override instants changed: got=%+v want recurrence=%s start=%s end=%s",
				back.Overrides, recurrenceID, overrideStart, overrideStart.Add(duration))
		}
	})
}

func weekdayName(day time.Weekday) string {
	return [...]string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}[day]
}
