package calendar

import (
	"os"
	"path/filepath"
	"strings"
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
	calDir := filepath.Join(accountDir, SanitizeName(name))
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
		if err := os.WriteFile(filepath.Join(calDir, SanitizeName(e.ICalUID)+".ics"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return calDir
}

func TestExpandOccurrencesNonRecurring(t *testing.T) {
	from := mustTime(t, "2026-01-10T00:00:00Z")
	to := mustTime(t, "2026-01-11T00:00:00Z")
	cases := []struct {
		name       string
		start, end string
		want       int
	}{
		{"inside window", "2026-01-10T09:00:00Z", "2026-01-10T10:00:00Z", 1},
		{"starts before from, ends inside", "2026-01-09T23:00:00Z", "2026-01-10T01:00:00Z", 1},
		{"starts inside, ends after to", "2026-01-10T23:00:00Z", "2026-01-11T01:00:00Z", 1},
		{"multi-day spanning the whole window", "2026-01-08T00:00:00Z", "2026-01-13T00:00:00Z", 1},
		{"entirely before window", "2026-01-08T09:00:00Z", "2026-01-08T10:00:00Z", 0},
		{"entirely after window", "2026-01-12T09:00:00Z", "2026-01-12T10:00:00Z", 0},
		{"end touches from (end == from)", "2026-01-09T23:00:00Z", "2026-01-10T00:00:00Z", 0},
		{"start touches to (start == to)", "2026-01-11T00:00:00Z", "2026-01-11T01:00:00Z", 0},
		{"barely overlaps from (end just after)", "2026-01-09T23:00:00Z", "2026-01-10T00:00:01Z", 1},
		{"barely overlaps to (start just before)", "2026-01-10T23:59:59Z", "2026-01-11T01:00:00Z", 1},
	}
	for _, c := range cases {
		e := Event{ICalUID: "a", Subject: "one", Start: mustTime(t, c.start), End: mustTime(t, c.end)}
		got := ExpandOccurrences(e, from, to)
		if len(got) != c.want {
			t.Errorf("%s: want %d occurrences, got %d", c.name, c.want, len(got))
			continue
		}
		if c.want == 1 && (!got[0].Start.Equal(e.Start) || !got[0].End.Equal(e.End)) {
			t.Errorf("%s: occurrence times changed: got %s-%s", c.name, got[0].Start, got[0].End)
		}
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

func TestExpandOccurrencesRecurringOverlap(t *testing.T) {
	// Daily 23:00-01:00 (crosses midnight), starting Thu 2026-01-08.
	e := Event{
		ICalUID: "n", Subject: "night shift",
		Start: mustTime(t, "2026-01-08T23:00:00Z"),
		End:   mustTime(t, "2026-01-09T01:00:00Z"),
		Recurrence: &Recurrence{
			Pattern: RecurrencePattern{Type: "daily", Interval: 1},
			Range:   RecurrenceRange{Type: "noEnd", StartDate: "2026-01-08"},
		},
	}
	// Window [Jan 10, Jan 11): the Jan 9 23:00 occurrence starts before the
	// window but runs until Jan 10 01:00, so it overlaps; the Jan 10 23:00
	// occurrence starts inside. The Jan 8 23:00 occurrence ends Jan 9 01:00,
	// well before the window, and Jan 11 23:00 starts after the exclusive end.
	got := ExpandOccurrences(e, mustTime(t, "2026-01-10T00:00:00Z"), mustTime(t, "2026-01-11T00:00:00Z"))
	if len(got) != 2 {
		t.Fatalf("want 2 occurrences (pre-window overlapper + in-window), got %d", len(got))
	}
	if !got[0].Start.Equal(mustTime(t, "2026-01-09T23:00:00Z")) || !got[0].End.Equal(mustTime(t, "2026-01-10T01:00:00Z")) {
		t.Errorf("first occurrence = %s-%s, want Jan 9 23:00 - Jan 10 01:00", got[0].Start, got[0].End)
	}
	if !got[1].Start.Equal(mustTime(t, "2026-01-10T23:00:00Z")) || !got[1].End.Equal(mustTime(t, "2026-01-11T01:00:00Z")) {
		t.Errorf("second occurrence = %s-%s, want Jan 10 23:00 - Jan 11 01:00", got[1].Start, got[1].End)
	}
	seen := map[string]bool{}
	for _, occ := range got {
		key := occ.Start.Format(time.RFC3339)
		if seen[key] {
			t.Errorf("duplicate occurrence at %s", key)
		}
		seen[key] = true
	}

	// An occurrence whose end touches `from` exactly does not overlap: with
	// window [Jan 10 01:00, Jan 11), the Jan 9 23:00 occurrence ends at
	// Jan 10 01:00 == from and must be dropped.
	got = ExpandOccurrences(e, mustTime(t, "2026-01-10T01:00:00Z"), mustTime(t, "2026-01-11T00:00:00Z"))
	if len(got) != 1 || !got[0].Start.Equal(mustTime(t, "2026-01-10T23:00:00Z")) {
		t.Fatalf("end==from occurrence must be dropped: got %d occurrence(s)", len(got))
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
	} else if !strings.Contains(err.Error(), "event:uid-alpha") || !strings.Contains(err.Error(), "event:uid-beta") {
		t.Fatalf("ambiguity error does not expose candidate UIDs: %v", err)
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

func TestCollectionAccountForPath(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	personal := filepath.Join(base, "personal")
	cols := []Collection{
		{Dir: work, Account: "office"},
		{Dir: personal, Account: "personal"},
	}
	if got := CollectionAccountForPath(cols, filepath.Join(work, "event.ics")); got != "office" {
		t.Fatalf("account = %q, want office", got)
	}
	if got := CollectionAccountForPath(cols, filepath.Join(base, "outside.ics")); got != "" {
		t.Fatalf("outside account = %q, want empty", got)
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
