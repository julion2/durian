package graphcalendar

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad time %q: %v", s, err)
	}
	return tm
}

// writeLocalCalendar creates a calendar dir with a displayname and the given
// events serialized via EventToICal.
func writeLocalCalendar(t *testing.T, accountDir, name string, events ...Event) string {
	t.Helper()
	calDir := filepath.Join(accountDir, sanitizeName(name))
	if err := os.MkdirAll(calDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(calDir, "displayname"), []byte(name+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		data, err := EventToICal(e)
		if err != nil {
			t.Fatalf("EventToICal: %v", err)
		}
		if err := os.WriteFile(filepath.Join(calDir, sanitizeName(e.ICalUID)+".ics"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return calDir
}

func TestExpandOccurrencesNonRecurring(t *testing.T) {
	e := Event{ICalUID: "a", Subject: "one", Start: mustTime(t, "2026-01-10T09:00:00Z"), End: mustTime(t, "2026-01-10T10:00:00Z")}
	from := mustTime(t, "2026-01-01T00:00:00Z")
	to := mustTime(t, "2026-01-31T00:00:00Z")

	if got := ExpandOccurrences(e, from, to); len(got) != 1 {
		t.Fatalf("in-window: want 1 occurrence, got %d", len(got))
	}
	if got := ExpandOccurrences(e, mustTime(t, "2026-02-01T00:00:00Z"), mustTime(t, "2026-03-01T00:00:00Z")); len(got) != 0 {
		t.Fatalf("out-of-window: want 0 occurrences, got %d", len(got))
	}
}

func TestExpandOccurrencesWeeklyEndExclusive(t *testing.T) {
	// Weekly on Monday, starting Mon 2026-01-05 09:00.
	e := Event{
		ICalUID: "w", Subject: "standup",
		Start: mustTime(t, "2026-01-05T09:00:00Z"),
		End:   mustTime(t, "2026-01-05T09:30:00Z"),
		Recurrence: &Recurrence{
			Pattern: RecurrencePattern{Type: "weekly", Interval: 1, DaysOfWeek: []string{"monday"}},
			Range:   RecurrenceRange{Type: "noEnd", StartDate: "2026-01-05"},
		},
	}
	// Window [Jan 5, Feb 2): Mondays Jan 5,12,19,26 — Feb 2 is a Monday but
	// equals the exclusive end, so it must be dropped.
	got := ExpandOccurrences(e, mustTime(t, "2026-01-05T00:00:00Z"), mustTime(t, "2026-02-02T00:00:00Z"))
	if len(got) != 4 {
		t.Fatalf("want 4 weekly occurrences, got %d", len(got))
	}
	for _, occ := range got {
		if d := occ.End.Sub(occ.Start); d != 30*time.Minute {
			t.Errorf("occurrence duration = %v, want 30m", d)
		}
		if occ.Start.Weekday() != time.Monday {
			t.Errorf("occurrence not on Monday: %s", occ.Start)
		}
	}
}

func TestParseWhen(t *testing.T) {
	now := mustTime(t, "2026-07-27T12:34:00Z")
	cases := []struct {
		in     string
		allDay bool
		want   string // RFC3339 of expected time, "" = expect error
	}{
		{"2026-07-28T14:00:00Z", false, "2026-07-28T14:00:00Z"},
		{"2026-07-28 14:00", false, "2026-07-28T14:00:00Z"},
		{"2026-07-28", true, "2026-07-28T00:00:00Z"},
		{"today", true, "2026-07-27T00:00:00Z"},
		{"tomorrow", true, "2026-07-28T00:00:00Z"},
		{"not-a-date", false, ""},
	}
	for _, c := range cases {
		got, allDay, err := ParseWhen(c.in, now)
		if c.want == "" {
			if err == nil {
				t.Errorf("ParseWhen(%q): want error, got %s", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseWhen(%q): %v", c.in, err)
			continue
		}
		if !got.Equal(mustTime(t, c.want)) {
			t.Errorf("ParseWhen(%q) = %s, want %s", c.in, got, c.want)
		}
		if allDay != c.allDay {
			t.Errorf("ParseWhen(%q) allDay = %v, want %v", c.in, allDay, c.allDay)
		}
	}
}

func TestResolveLocalEvent(t *testing.T) {
	dir := t.TempDir()
	writeLocalCalendar(t, dir, "Work",
		Event{ICalUID: "uid-alpha", Subject: "Sprint planning", Start: mustTime(t, "2026-01-05T09:00:00Z"), End: mustTime(t, "2026-01-05T10:00:00Z")},
		Event{ICalUID: "uid-beta", Subject: "Sprint review", Start: mustTime(t, "2026-01-09T14:00:00Z"), End: mustTime(t, "2026-01-09T15:00:00Z")},
	)
	writeLocalCalendar(t, dir, "Personal",
		Event{ICalUID: "xid-gamma", Subject: "Dentist", Start: mustTime(t, "2026-01-06T08:00:00Z"), End: mustTime(t, "2026-01-06T08:30:00Z")},
	)

	// Exact UID.
	if _, ev, _, err := ResolveLocalEvent(dir, "", "uid-alpha", ""); err != nil || ev.Subject != "Sprint planning" {
		t.Fatalf("exact UID: ev=%q err=%v", ev.Subject, err)
	}
	// Unique prefix.
	if _, ev, _, err := ResolveLocalEvent(dir, "", "xid-", ""); err != nil || ev.Subject != "Dentist" {
		t.Fatalf("prefix: ev=%q err=%v", ev.Subject, err)
	}
	// Unique subject substring.
	if _, ev, _, err := ResolveLocalEvent(dir, "", "dentist", ""); err != nil || ev.ICalUID != "xid-gamma" {
		t.Fatalf("subject: ev=%q err=%v", ev.ICalUID, err)
	}
	// Ambiguous ("sprint" matches two).
	if _, _, _, err := ResolveLocalEvent(dir, "", "sprint", ""); err == nil {
		t.Fatal("ambiguous subject: want error, got nil")
	}
	// None.
	if _, _, _, err := ResolveLocalEvent(dir, "", "nonexistent", ""); err == nil {
		t.Fatal("no match: want error, got nil")
	}
	// "uid-" matches two events in Work, so it stays ambiguous even with the
	// calendar filter applied.
	if _, _, _, err := ResolveLocalEvent(dir, "", "uid-", "Work"); err == nil {
		t.Fatal("uid- in Work should be ambiguous")
	}
	// The calendar filter excludes non-matching calendars: "gamma" is only in
	// Personal, so filtering to Work finds nothing.
	if _, _, _, err := ResolveLocalEvent(dir, "", "gamma", "Work"); err == nil {
		t.Fatal("gamma filtered to Work should not match")
	}
}

func TestWriteLocalEventRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeLocalCalendar(t, dir, "Work") // empty calendar dir with displayname

	e := Event{
		ICalUID: "new-1", Subject: "Lunch",
		Start: mustTime(t, "2026-01-05T12:00:00Z"), End: mustTime(t, "2026-01-05T13:00:00Z"),
		Location: "Cafe",
	}
	path, err := WriteLocalEvent(dir, "Work", e)
	if err != nil {
		t.Fatalf("WriteLocalEvent: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("written file missing: %v", err)
	}

	cals, err := ReadCalendars(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range cals {
		for _, ev := range c.Events {
			if ev.ICalUID == "new-1" && ev.Subject == "Lunch" && ev.Location == "Cafe" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("written event not found by ReadCalendars")
	}

	// Unknown calendar is rejected.
	if _, err := WriteLocalEvent(dir, "Nope", e); err == nil {
		t.Fatal("write to unknown calendar: want error, got nil")
	}
}
