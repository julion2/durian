package calendar

import (
	"strings"
	"testing"
	"time"
)

// weeklyMaster is a Monday 09:00-10:00 UTC series with no end, the fixture the
// exception tests deviate from.
func weeklyMaster(t *testing.T) Event {
	t.Helper()
	return Event{
		ICalUID: "series-uid",
		Subject: "Standup",
		Start:   mustTime(t, "2026-08-03T09:00:00Z"), // a Monday
		End:     mustTime(t, "2026-08-03T10:00:00Z"),
		Recurrence: &Recurrence{
			Pattern: RecurrencePattern{Type: "weekly", Interval: 1, DaysOfWeek: []string{"monday"}},
			Range:   RecurrenceRange{Type: "noEnd", StartDate: "2026-08-03"},
		},
	}
}

// starts renders the occurrence starts as RFC3339 for readable assertions.
func starts(events []Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Start.UTC().Format(time.RFC3339))
	}
	return out
}

func TestExpandOccurrencesDropsCancelledDates(t *testing.T) {
	master := weeklyMaster(t)
	master.ExceptionDates = []time.Time{mustTime(t, "2026-08-10T09:00:00Z")}

	got := ExpandOccurrences(master,
		mustTime(t, "2026-08-03T00:00:00Z"), mustTime(t, "2026-08-25T00:00:00Z"))

	want := []string{"2026-08-03T09:00:00Z", "2026-08-17T09:00:00Z", "2026-08-24T09:00:00Z"}
	if diff := strings.Join(starts(got), ","); diff != strings.Join(want, ",") {
		t.Errorf("occurrences = %v, want %v", starts(got), want)
	}
}

func TestExpandOccurrencesUsesOverrideTimes(t *testing.T) {
	master := weeklyMaster(t)
	master.Overrides = []Event{{
		ICalUID:      "series-uid",
		Subject:      "Standup (moved)",
		RecurrenceID: mustTime(t, "2026-08-10T09:00:00Z"),
		Start:        mustTime(t, "2026-08-11T14:00:00Z"),
		End:          mustTime(t, "2026-08-11T15:00:00Z"),
	}}

	got := ExpandOccurrences(master,
		mustTime(t, "2026-08-03T00:00:00Z"), mustTime(t, "2026-08-18T00:00:00Z"))

	want := []string{"2026-08-03T09:00:00Z", "2026-08-11T14:00:00Z", "2026-08-17T09:00:00Z"}
	if strings.Join(starts(got), ",") != strings.Join(want, ",") {
		t.Fatalf("occurrences = %v, want %v", starts(got), want)
	}
	for _, e := range got {
		if e.Start.Equal(mustTime(t, "2026-08-11T14:00:00Z")) && e.Subject != "Standup (moved)" {
			t.Errorf("moved occurrence kept the master subject %q", e.Subject)
		}
	}
}

// An occurrence pushed out of the queried window must disappear from it, even
// though the rule still generates its original date inside the window.
func TestExpandOccurrencesDropsOverrideMovedOutOfWindow(t *testing.T) {
	master := weeklyMaster(t)
	master.Overrides = []Event{{
		ICalUID:      "series-uid",
		Subject:      "Standup (postponed)",
		RecurrenceID: mustTime(t, "2026-08-10T09:00:00Z"),
		Start:        mustTime(t, "2026-09-14T09:00:00Z"),
		End:          mustTime(t, "2026-09-14T10:00:00Z"),
	}}

	got := ExpandOccurrences(master,
		mustTime(t, "2026-08-03T00:00:00Z"), mustTime(t, "2026-08-18T00:00:00Z"))

	want := []string{"2026-08-03T09:00:00Z", "2026-08-17T09:00:00Z"}
	if strings.Join(starts(got), ",") != strings.Join(want, ",") {
		t.Errorf("occurrences = %v, want %v", starts(got), want)
	}
}

// The mirror case: an occurrence pulled INTO the window from outside has no
// rule-generated date inside it to hang on, and is the reason the override
// loop cannot be driven from the raw expansion.
func TestExpandOccurrencesAddsOverrideMovedIntoWindow(t *testing.T) {
	master := weeklyMaster(t)
	master.Overrides = []Event{{
		ICalUID:      "series-uid",
		Subject:      "Standup (pulled forward)",
		RecurrenceID: mustTime(t, "2026-09-14T09:00:00Z"),
		Start:        mustTime(t, "2026-08-12T09:00:00Z"),
		End:          mustTime(t, "2026-08-12T10:00:00Z"),
	}}

	got := ExpandOccurrences(master,
		mustTime(t, "2026-08-03T00:00:00Z"), mustTime(t, "2026-08-18T00:00:00Z"))

	want := []string{"2026-08-03T09:00:00Z", "2026-08-10T09:00:00Z",
		"2026-08-12T09:00:00Z", "2026-08-17T09:00:00Z"}
	if strings.Join(starts(got), ",") != strings.Join(want, ",") {
		t.Errorf("occurrences = %v, want %v", starts(got), want)
	}
}

func TestExpandedOccurrencesCarryNoSeriesState(t *testing.T) {
	master := weeklyMaster(t)
	master.ExceptionDates = []time.Time{mustTime(t, "2026-08-10T09:00:00Z")}
	master.Overrides = []Event{{
		ICalUID:      "series-uid",
		RecurrenceID: mustTime(t, "2026-08-17T09:00:00Z"),
		Start:        mustTime(t, "2026-08-17T11:00:00Z"),
		End:          mustTime(t, "2026-08-17T12:00:00Z"),
	}}

	for _, e := range ExpandOccurrences(master,
		mustTime(t, "2026-08-03T00:00:00Z"), mustTime(t, "2026-08-25T00:00:00Z")) {
		if len(e.ExceptionDates) != 0 || len(e.Overrides) != 0 {
			t.Errorf("occurrence at %s still carries series state", e.Start)
		}
	}
}

// MARK: - iCal round trip

func TestICalRoundTripPreservesSeriesExceptions(t *testing.T) {
	master := weeklyMaster(t)
	master.ExceptionDates = []time.Time{
		mustTime(t, "2026-08-24T09:00:00Z"),
		mustTime(t, "2026-08-10T09:00:00Z"), // deliberately out of order
	}
	master.Overrides = []Event{{
		ICalUID:      "series-uid",
		Subject:      "Standup (moved)",
		Location:     "Room 2",
		RecurrenceID: mustTime(t, "2026-08-17T09:00:00Z"),
		Start:        mustTime(t, "2026-08-18T14:00:00Z"),
		End:          mustTime(t, "2026-08-18T15:00:00Z"),
	}}

	data, err := EventToICal(master)
	if err != nil {
		t.Fatalf("EventToICal: %v", err)
	}
	if !strings.Contains(string(data), "EXDATE") {
		t.Fatalf("serialized series has no EXDATE:\n%s", data)
	}
	if !strings.Contains(string(data), "RECURRENCE-ID") {
		t.Fatalf("serialized series has no RECURRENCE-ID:\n%s", data)
	}

	back, err := ICalToEvent(data, "")
	if err != nil {
		t.Fatalf("ICalToEvent: %v", err)
	}
	if got := starts(back.Overrides); len(got) != 1 || got[0] != "2026-08-18T14:00:00Z" {
		t.Errorf("override starts = %v, want [2026-08-18T14:00:00Z]", got)
	}
	if back.Overrides[0].Subject != "Standup (moved)" || back.Overrides[0].Location != "Room 2" {
		t.Errorf("override lost its content: %+v", back.Overrides[0])
	}
	if !back.Overrides[0].RecurrenceID.Equal(mustTime(t, "2026-08-17T09:00:00Z")) {
		t.Errorf("override recurrence id = %s, want 2026-08-17T09:00:00Z", back.Overrides[0].RecurrenceID)
	}
	// Exception dates come back sorted regardless of the input order.
	if len(back.ExceptionDates) != 2 ||
		!back.ExceptionDates[0].Equal(mustTime(t, "2026-08-10T09:00:00Z")) ||
		!back.ExceptionDates[1].Equal(mustTime(t, "2026-08-24T09:00:00Z")) {
		t.Errorf("exception dates = %v, want the two dates sorted ascending", back.ExceptionDates)
	}
}

// Serializing the same series twice must yield identical bytes: the local file
// hash is the sync engine's local-side identity, so any ordering wobble would
// register as a spurious local edit on every scan.
func TestICalSeriesSerializationIsStable(t *testing.T) {
	master := weeklyMaster(t)
	master.LastModified = mustTime(t, "2026-08-01T00:00:00Z")
	master.ExceptionDates = []time.Time{
		mustTime(t, "2026-08-24T09:00:00Z"),
		mustTime(t, "2026-08-10T09:00:00Z"),
	}
	master.Overrides = []Event{
		{ICalUID: "series-uid", RecurrenceID: mustTime(t, "2026-09-07T09:00:00Z"),
			Start: mustTime(t, "2026-09-07T11:00:00Z"), End: mustTime(t, "2026-09-07T12:00:00Z"),
			LastModified: mustTime(t, "2026-08-01T00:00:00Z")},
		{ICalUID: "series-uid", RecurrenceID: mustTime(t, "2026-08-17T09:00:00Z"),
			Start: mustTime(t, "2026-08-18T14:00:00Z"), End: mustTime(t, "2026-08-18T15:00:00Z"),
			LastModified: mustTime(t, "2026-08-01T00:00:00Z")},
	}

	first, err := EventToICal(master)
	if err != nil {
		t.Fatalf("EventToICal: %v", err)
	}
	// Reverse both lists; the output must not move.
	master.ExceptionDates[0], master.ExceptionDates[1] = master.ExceptionDates[1], master.ExceptionDates[0]
	master.Overrides[0], master.Overrides[1] = master.Overrides[1], master.Overrides[0]
	second, err := EventToICal(master)
	if err != nil {
		t.Fatalf("EventToICal: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("serialization depends on input ordering:\n%s\n---\n%s", first, second)
	}
}

func TestICalRejectsOverridesWithoutMaster(t *testing.T) {
	doc := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:series-uid\r\nDTSTAMP:20260801T000000Z\r\n" +
		"RECURRENCE-ID:20260817T090000Z\r\nDTSTART:20260818T140000Z\r\nDTEND:20260818T150000Z\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	if _, err := ICalToEvent([]byte(doc), ""); err == nil {
		t.Fatal("ICalToEvent accepted a document with no series master")
	}
}

// MARK: - Content hash

func TestContentHashCoversSeriesExceptions(t *testing.T) {
	base := weeklyMaster(t)
	baseline := EventContentHash(base, "")

	cancelled := base
	cancelled.ExceptionDates = []time.Time{mustTime(t, "2026-08-10T09:00:00Z")}
	if EventContentHash(cancelled, "") == baseline {
		t.Error("cancelling an occurrence did not move the content hash")
	}

	moved := base
	moved.Overrides = []Event{{
		ICalUID:      "series-uid",
		RecurrenceID: mustTime(t, "2026-08-10T09:00:00Z"),
		Start:        mustTime(t, "2026-08-11T14:00:00Z"),
		End:          mustTime(t, "2026-08-11T15:00:00Z"),
	}}
	if EventContentHash(moved, "") == baseline {
		t.Error("moving an occurrence did not move the content hash")
	}
	if EventContentHash(moved, "") == EventContentHash(cancelled, "") {
		t.Error("a moved and a cancelled occurrence hash identically")
	}

	// CoreContentHash must react too — an exception is a core edit, not an
	// attendee response.
	if CoreContentHash(cancelled, "") == CoreContentHash(base, "") {
		t.Error("cancelling an occurrence did not move the core hash")
	}
}

func TestContentHashIgnoresExceptionOrdering(t *testing.T) {
	a := weeklyMaster(t)
	a.ExceptionDates = []time.Time{
		mustTime(t, "2026-08-10T09:00:00Z"), mustTime(t, "2026-08-24T09:00:00Z"),
	}
	b := weeklyMaster(t)
	b.ExceptionDates = []time.Time{
		mustTime(t, "2026-08-24T09:00:00Z"), mustTime(t, "2026-08-10T09:00:00Z"),
	}
	if EventContentHash(a, "") != EventContentHash(b, "") {
		t.Error("content hash depends on exception-date ordering")
	}
}

// MARK: - Opaque recurrence

// An event whose recurrence cannot be rendered as an RRULE must come back
// marked opaque, so the upload path omits the field instead of clearing the
// series remotely.
func TestICalMarksUnmappableRecurrenceOpaque(t *testing.T) {
	e := weeklyMaster(t)
	e.Recurrence = &Recurrence{
		Pattern: RecurrencePattern{Type: "lunar", Interval: 1},
		Range:   RecurrenceRange{Type: "noEnd"},
	}

	data, err := EventToICal(e)
	if err != nil {
		t.Fatalf("EventToICal: %v", err)
	}
	if strings.Contains(string(data), "RRULE") {
		t.Fatalf("unmappable recurrence produced an RRULE:\n%s", data)
	}

	back, err := ICalToEvent(data, "")
	if err != nil {
		t.Fatalf("ICalToEvent: %v", err)
	}
	if back.Recurrence != nil {
		t.Errorf("expected no parsed recurrence, got %+v", back.Recurrence)
	}
	if !back.OpaqueRecurrence {
		t.Error("event lost the opaque-recurrence marker; the next upload would clear the series")
	}
}

func TestICalLeavesPlainEventsNotOpaque(t *testing.T) {
	e := Event{
		ICalUID: "plain-uid",
		Subject: "Lunch",
		Start:   mustTime(t, "2026-08-03T12:00:00Z"),
		End:     mustTime(t, "2026-08-03T13:00:00Z"),
	}
	data, err := EventToICal(e)
	if err != nil {
		t.Fatalf("EventToICal: %v", err)
	}
	back, err := ICalToEvent(data, "")
	if err != nil {
		t.Fatalf("ICalToEvent: %v", err)
	}
	if back.OpaqueRecurrence {
		t.Error("a non-recurring event was marked opaque; its recurrence would never be writable")
	}
}

// MARK: - EXDATE line parsing

func TestParseExDateLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []string
	}{
		{"utc date-time", "EXDATE:20260810T090000Z", []string{"2026-08-10T09:00:00Z"}},
		{"comma separated", "EXDATE:20260810T090000Z,20260824T090000Z",
			[]string{"2026-08-10T09:00:00Z", "2026-08-24T09:00:00Z"}},
		{"value date", "EXDATE;VALUE=DATE:20260810", []string{"2026-08-10T00:00:00Z"}},
		// Zurich is UTC+2 in August, so 09:00 local is 07:00 UTC. Reading it
		// as UTC would place the exception two hours off the occurrence it
		// cancels, and the cancellation would silently miss.
		{"tzid", "EXDATE;TZID=Europe/Zurich:20260810T090000", []string{"2026-08-10T07:00:00Z"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseExDateLine(tc.line)
			if err != nil {
				t.Fatalf("ParseExDateLine(%q): %v", tc.line, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d dates, want %d: %v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i].UTC().Format(time.RFC3339) != tc.want[i] {
					t.Errorf("date %d = %s, want %s", i, got[i].UTC().Format(time.RFC3339), tc.want[i])
				}
			}
		})
	}
}

func TestParseExDateLineRejectsGarbage(t *testing.T) {
	for _, line := range []string{"EXDATE", "EXDATE:", "EXDATE:not-a-date"} {
		if _, err := ParseExDateLine(line); err == nil {
			t.Errorf("ParseExDateLine(%q) accepted a malformed line", line)
		}
	}
}

// MARK: - Hash migration

// The exceptions field must not exist in the digest input of an event that has
// none. Every field contributes a NUL terminator, so an unconditional empty
// field would move the hash of EVERY event in every calendar — and the first
// sync after that reads as "remote changed" everywhere, which turns a pending
// local edit into a CONFLICT that the default policy rolls back instead of
// uploading.
//
// The constants below are the digests as they were before series exceptions
// existed. They are golden on purpose: only a deliberate, migration-aware
// change may update them.
func TestContentHashUnchangedForEventsWithoutExceptions(t *testing.T) {
	e := Event{
		ICalUID:     "plain-uid",
		Subject:     "Lunch",
		Location:    "Canteen",
		Description: "with the team",
		Start:       mustTime(t, "2026-08-03T12:00:00Z"),
		End:         mustTime(t, "2026-08-03T13:00:00Z"),
		Attendees: []Attendee{
			{Email: "a@example.com", Type: "required", Response: "accepted"},
		},
		Organizer: &Person{Email: "owner@example.com"},
	}

	const (
		wantFull = "a82e6a0e9f927c7e7f5b213285b28aae10c6610072edc9ef1572bff7161b7d4c"
		wantCore = "661fb5a3b9be92d927d5c44670310123a09699661236786354059ee278459f05"
	)
	if got := EventContentHash(e, "owner@example.com"); got != wantFull {
		t.Errorf("EventContentHash = %q, want %q — a changed digest re-baselines every\n"+
			"tracked event on upgrade; update the golden only with a migration story", got, wantFull)
	}
	if got := CoreContentHash(e, "owner@example.com"); got != wantCore {
		t.Errorf("CoreContentHash = %q, want %q", got, wantCore)
	}
}

// A recurring series with no exceptions is still an exception-less event.
func TestContentHashUnchangedForSeriesWithoutExceptions(t *testing.T) {
	master := weeklyMaster(t)
	bare := master
	bare.ExceptionDates = nil
	bare.Overrides = nil

	empty := master
	empty.ExceptionDates = []time.Time{}
	empty.Overrides = []Event{}

	if EventContentHash(bare, "") != EventContentHash(empty, "") {
		t.Error("an empty exception list hashes differently from no list at all")
	}
}

// MARK: - Value types and time zones

// RECURRENCE-ID states a date the MASTER's rule produced, so it must use the
// master's value type. Taking it from the override truncates the time when a
// timed occurrence is turned into an all-day one, and the key then matches no
// occurrence at all.
func TestRecurrenceIDFollowsTheMastersValueType(t *testing.T) {
	master := weeklyMaster(t) // timed, 09:00-10:00
	master.Overrides = []Event{{
		ICalUID:      "series-uid",
		Subject:      "Standup (all day now)",
		AllDay:       true,
		RecurrenceID: mustTime(t, "2026-08-17T09:00:00Z"),
		Start:        mustTime(t, "2026-08-17T00:00:00Z"),
		End:          mustTime(t, "2026-08-18T00:00:00Z"),
	}}

	data, err := EventToICal(master)
	if err != nil {
		t.Fatalf("EventToICal: %v", err)
	}
	if strings.Contains(string(data), "RECURRENCE-ID;VALUE=DATE:") {
		t.Errorf("RECURRENCE-ID was written as a DATE for a timed series:\n%s", data)
	}

	back, err := ICalToEvent(data, "")
	if err != nil {
		t.Fatalf("ICalToEvent: %v", err)
	}
	if len(back.Overrides) != 1 {
		t.Fatalf("overrides = %+v", back.Overrides)
	}
	if !back.Overrides[0].RecurrenceID.Equal(mustTime(t, "2026-08-17T09:00:00Z")) {
		t.Errorf("recurrence id = %s, want the original 09:00 instant preserved",
			back.Overrides[0].RecurrenceID)
	}
}

// The vdir is shared with khal and other CalDAV clients, which state the zone
// and a floating local time. Reading that as UTC places the exception a whole
// offset away from the occurrence it cancels.
func TestICalToEventHonorsTZIDOnExceptions(t *testing.T) {
	doc := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:series-uid\r\nDTSTAMP:20260801T000000Z\r\n" +
		"DTSTART:20260803T070000Z\r\nDTEND:20260803T080000Z\r\n" +
		"RRULE:FREQ=WEEKLY;BYDAY=MO\r\n" +
		"EXDATE;TZID=Europe/Zurich:20260810T090000\r\n" +
		"END:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:series-uid\r\nDTSTAMP:20260801T000000Z\r\n" +
		"RECURRENCE-ID;TZID=Europe/Zurich:20260817T090000\r\n" +
		"DTSTART:20260818T120000Z\r\nDTEND:20260818T130000Z\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	back, err := ICalToEvent([]byte(doc), "")
	if err != nil {
		t.Fatalf("ICalToEvent: %v", err)
	}
	// Zurich is UTC+2 in August, so 09:00 local is 07:00 UTC.
	if len(back.ExceptionDates) != 1 ||
		!back.ExceptionDates[0].Equal(mustTime(t, "2026-08-10T07:00:00Z")) {
		t.Errorf("exception dates = %v, want [2026-08-10T07:00:00Z]", back.ExceptionDates)
	}
	if len(back.Overrides) != 1 ||
		!back.Overrides[0].RecurrenceID.Equal(mustTime(t, "2026-08-17T07:00:00Z")) {
		t.Errorf("override recurrence id = %v, want 2026-08-17T07:00:00Z",
			back.Overrides[0].RecurrenceID)
	}
}

// RFC 5545 forbids TZID on a DATE value. Honoring it anyway shifts an all-day
// exception onto the previous day for every zone east of UTC.
func TestParseExDateLineIgnoresTZIDOnDateValues(t *testing.T) {
	got, err := ParseExDateLine("EXDATE;TZID=Europe/Zurich;VALUE=DATE:20260817")
	if err != nil {
		t.Fatalf("ParseExDateLine: %v", err)
	}
	if len(got) != 1 || !got[0].Equal(mustTime(t, "2026-08-17T00:00:00Z")) {
		t.Errorf("got %v, want [2026-08-17T00:00:00Z] — a DATE must not be shifted by TZID", got)
	}
}
