package main

import (
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
)

// ParseRSVPVerb/SetOwnerResponse moved to cli/internal/calendar (rsvp_test.go
// covers them); runCalendarRsvp calls the exported versions.

func TestParseHexColor(t *testing.T) {
	if r, g, b, ok := parseHexColor("#FF8000"); !ok || r != 255 || g != 128 || b != 0 {
		t.Errorf("parseHexColor(#FF8000) = %d,%d,%d,%v", r, g, b, ok)
	}
	if _, _, _, ok := parseHexColor("auto"); ok {
		t.Error("parseHexColor(auto): want ok=false")
	}
	if _, _, _, ok := parseHexColor(""); ok {
		t.Error("parseHexColor(empty): want ok=false")
	}
}

func TestVisibleWidth(t *testing.T) {
	cases := map[string]int{
		"plain":                             5,
		"":                                  0,
		"\x1b[1mBold\x1b[0m":                4,
		"\x1b[36;1mcyan bold\x1b[0m":        9,
		"● dot":                             5, // multi-byte rune counts as one
		"\x1b[2mdim\x1b[0m \x1b[1mb\x1b[0m": 5,
	}
	for in, want := range cases {
		if got := visibleWidth(in); got != want {
			t.Errorf("visibleWidth(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestPrintColumnsAlignsColoredCells(t *testing.T) {
	// A colored cell and a plain cell of the same visible width must produce
	// identically aligned columns (tabwriter counted the escape bytes and
	// misaligned them — the bug this replaces).
	rows := [][]string{
		{styHeaderRaw("DATE"), "SUBJECT", "CAL"},
		{"2026-08-01", "\x1b[36;1mShort\x1b[0m", "Work"},
		{"08-02", "A much longer subject", "\x1b[2mHome\x1b[0m"},
	}
	var b strings.Builder
	printColumns(&b, rows)
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3:\n%s", len(lines), b.String())
	}
	// After stripping ANSI, every line's third column must start at the same
	// offset — i.e. the visible layout is aligned.
	stripped := make([]string, len(lines))
	for i, l := range lines {
		stripped[i] = ansiSGRPattern.ReplaceAllString(l, "")
	}
	col3 := []string{"CAL", "Work", "Home"}
	want := strings.Index(stripped[0], "CAL")
	for i, l := range stripped {
		if idx := strings.Index(l, col3[i]); idx != want {
			t.Errorf("line %d: column 3 at %d, want %d:\n%q", i, idx, want, l)
		}
	}
	// Single-cell rows (headers/separators) pass through verbatim.
	var hb strings.Builder
	printColumns(&hb, [][]string{{"HEADER"}, {""}, {"a", "b"}})
	if got := hb.String(); got != "HEADER\n\na  b\n" {
		t.Errorf("header/separator output = %q", got)
	}
}

// styHeaderRaw emits a bold cell regardless of terminal detection, so the
// alignment test always exercises real escape sequences.
func styHeaderRaw(s string) string { return "\x1b[1m" + s + "\x1b[0m" }

func TestEventEndAllDaySnap(t *testing.T) {
	// Global flags feed eventEnd; reset them after the test.
	t.Cleanup(func() { calNewEnd, calNewDuration = "", "" })
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	// An all-day event with a sub-day duration snaps to one full day.
	calNewEnd, calNewDuration = "", "1h"
	end, err := eventEnd(start, true, now)
	if err != nil {
		t.Fatalf("eventEnd: %v", err)
	}
	if want := start.AddDate(0, 0, 1); !end.Equal(want) {
		t.Errorf("all-day 1h end = %v, want snapped to %v", end, want)
	}

	// A timed event keeps its duration.
	calNewDuration = "30m"
	end, err = eventEnd(start, false, now)
	if err != nil || !end.Equal(start.Add(30*time.Minute)) {
		t.Errorf("timed 30m end = %v err=%v, want start+30m", end, err)
	}

	// Defaults: 1h timed, 1 day all-day.
	calNewEnd, calNewDuration = "", ""
	if end, _ = eventEnd(start, false, now); !end.Equal(start.Add(time.Hour)) {
		t.Errorf("default timed end = %v, want start+1h", end)
	}
	if end, _ = eventEnd(start, true, now); !end.Equal(start.AddDate(0, 0, 1)) {
		t.Errorf("default all-day end = %v, want start+1d", end)
	}
}

func TestApplyCalendarModifyPatchesOnlyExplicitFields(t *testing.T) {
	start := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	recurrence := &calendar.Recurrence{Pattern: calendar.RecurrencePattern{Type: "weekly", Interval: 1}}
	event := calendar.Event{
		ICalUID: "uid-1", Subject: "Standup", Location: "Room 1", Description: "Keep me",
		Start: start, End: start.Add(45 * time.Minute), Recurrence: recurrence,
		Attendees: []calendar.Attendee{{Email: "a@example.com", Type: "required"}},
	}

	got, err := applyCalendarModify(event, calendarModifyOptions{
		start: "2026-08-25 14:00", startSet: true,
	}, start)
	if err != nil {
		t.Fatalf("applyCalendarModify: %v", err)
	}
	if got.Subject != event.Subject || got.Location != event.Location || got.Description != event.Description {
		t.Errorf("omitted text fields changed: %+v", got)
	}
	if got.End.Sub(got.Start) != 45*time.Minute {
		t.Errorf("duration = %v, want 45m", got.End.Sub(got.Start))
	}
	if got.Recurrence != recurrence || len(got.Attendees) != 1 {
		t.Error("recurrence or attendees were not preserved")
	}
}

func TestApplyCalendarModifySupportsDurationAndClearingText(t *testing.T) {
	start := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	event := calendar.Event{Subject: "Standup", Location: "Room 1", Description: "Notes",
		Start: start, End: start.Add(time.Hour)}
	got, err := applyCalendarModify(event, calendarModifyOptions{
		duration: "30m", durationSet: true, locationSet: true, descriptionSet: true,
	}, start)
	if err != nil {
		t.Fatalf("applyCalendarModify: %v", err)
	}
	if got.End.Sub(got.Start) != 30*time.Minute || got.Location != "" || got.Description != "" {
		t.Errorf("modified event = %+v", got)
	}
}

func TestApplyCalendarModifyPreservesAllDaySpan(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	event := calendar.Event{Start: start, End: start.AddDate(0, 0, 3), AllDay: true}
	got, err := applyCalendarModify(event, calendarModifyOptions{
		start: "2026-09-01", startSet: true,
	}, start)
	if err != nil {
		t.Fatalf("applyCalendarModify: %v", err)
	}
	if want := got.Start.AddDate(0, 0, 3); !got.End.Equal(want) {
		t.Errorf("end = %v, want %v (three-day span)", got.End, want)
	}
}

func TestApplyCalendarModifyValidatesPatch(t *testing.T) {
	start := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	event := calendar.Event{Start: start, End: start.AddDate(0, 0, 1), AllDay: true}
	if _, err := applyCalendarModify(event, calendarModifyOptions{}, start); err == nil {
		t.Error("empty patch: want error")
	}
	if _, err := applyCalendarModify(event, calendarModifyOptions{
		allDaySet: true, allDay: false,
	}, start); err == nil {
		t.Error("all-day conversion without explicit times: want error")
	}
	if _, err := applyCalendarModify(event, calendarModifyOptions{
		endSet: true, end: "2026-08-25", durationSet: true, duration: "1h",
	}, start); err == nil {
		t.Error("end plus duration: want error")
	}
	if _, err := applyCalendarModify(event, calendarModifyOptions{
		startSet: true, start: "invalid",
	}, start); err == nil || !strings.Contains(err.Error(), "parse --start") {
		t.Errorf("invalid start error = %v, want --start context", err)
	}
	if _, err := applyCalendarModify(event, calendarModifyOptions{
		endSet: true, end: "invalid",
	}, start); err == nil || !strings.Contains(err.Error(), "parse --end") {
		t.Errorf("invalid end error = %v, want --end context", err)
	}
}

func TestRecurrenceSummary(t *testing.T) {
	e := calendar.Event{Recurrence: &calendar.Recurrence{
		Pattern: calendar.RecurrencePattern{Type: "weekly", Interval: 2, DaysOfWeek: []string{"monday", "wednesday"}},
		Range:   calendar.RecurrenceRange{Type: "numbered", NumberOfOccurrences: 10},
	}}
	got := recurrenceSummary(e)
	if got == "" {
		t.Fatal("recurrenceSummary empty for a recurring event")
	}
	if recurrenceSummary(calendar.Event{}) != "" {
		t.Error("recurrenceSummary: want empty for non-recurring event")
	}
}

// MARK: - Multi-account read targets

func TestContributingAccountsDerivesFromTheResult(t *testing.T) {
	cals := []calendar.LocalCalendar{
		{Name: "Calendar", Account: "h"},
		{Name: "Ich bin weg", Account: "h"},
		{Name: "Privat", Account: "local"},
		{Name: "Ohne Account"}, // an account-less read (ReadCalendars path)
	}
	got := contributingAccounts(cals)
	if len(got) != 2 || got[0] != "h" || got[1] != "local" {
		t.Errorf("contributingAccounts = %v, want [h local] in first-seen order", got)
	}
}

// The label must describe what is SHOWN. With no --account the request covers
// every configured account, but most have no calendar vdir at all — reporting
// that count would describe the query instead of the answer.
func TestTargetLabel(t *testing.T) {
	if got := targetLabel([]string{"h"}); got != "h" {
		t.Errorf("single account label = %q, want the account itself", got)
	}
	if got := targetLabel([]string{"h", "gm", "local"}); got != "3 accounts" {
		t.Errorf("multi label = %q, want a count", got)
	}
}

// Two accounts can each have a calendar named "Calendar", so the account is
// prefixed — but only when more than one is in play, to keep the far more
// common single-account output unchanged.
func TestCalendarLabelPrefixesOnlyWhenAmbiguous(t *testing.T) {
	cal := calendar.LocalCalendar{Name: "Calendar", Account: "h"}

	if got := calendarLabel(cal, false); !strings.HasSuffix(got, "Calendar") || strings.Contains(got, "h/") {
		t.Errorf("single-account label = %q, want no account prefix", got)
	}
	if got := calendarLabel(cal, true); !strings.HasSuffix(got, "h/Calendar") {
		t.Errorf("multi-account label = %q, want the account prefixed", got)
	}

	// A calendar with no account (the plain ReadCalendars path) is never
	// prefixed with an empty segment.
	plain := calendar.LocalCalendar{Name: "Calendar"}
	if got := calendarLabel(plain, true); strings.Contains(got, "/") {
		t.Errorf("account-less label = %q, want no prefix", got)
	}
}
