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
