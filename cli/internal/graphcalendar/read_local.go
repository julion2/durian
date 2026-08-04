// Offline readers for the local vdir: enumerate calendars and events straight
// from the .ics files on disk (no Graph call, no token), expand recurring
// masters into concrete occurrences for a time window, resolve a single event
// by a short reference, and parse the human date/time inputs the CLI accepts.
//
// These power the local-first `durian calendar` subcommands (list/search/show/
// new/rsvp/delete): every one of them reads or writes the vdir only, and the
// actual Outlook effect happens later through `durian calendar sync`.

package graphcalendar

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/teambition/rrule-go"
)

// LocalCalendar is one calendar directory of the vdir, with its display name,
// color (from the displayname/color meta files) and the master events parsed
// from its .ics files.
type LocalCalendar struct {
	Name     string
	HexColor string
	Dir      string
	Events   []Event
}

// ReadCalendars enumerates every calendar directory under accountDir and parses
// the events of each from disk — entirely offline. The calendar name comes from
// the "displayname" meta file (falling back to the directory name) and the
// color from the "color" file. owner is the account email, threaded into the
// parse so the owner's own RSVP is recognized. A missing accountDir yields an
// empty slice (nothing synced yet), not an error.
func ReadCalendars(accountDir, owner string) ([]LocalCalendar, error) {
	entries, err := os.ReadDir(accountDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read calendar dir %s: %w", accountDir, err)
	}

	var calendars []LocalCalendar
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		calDir := filepath.Join(accountDir, entry.Name())
		items, _, err := scanLocalItems(calDir, owner)
		if err != nil {
			return nil, err
		}
		events := make([]Event, 0, len(items))
		for _, it := range items {
			events = append(events, it.event)
		}
		sort.Slice(events, func(i, j int) bool { return events[i].Start.Before(events[j].Start) })

		calendars = append(calendars, LocalCalendar{
			Name:     readMetaFile(calDir, "displayname", entry.Name()),
			HexColor: readMetaFile(calDir, "color", ""),
			Dir:      calDir,
			Events:   events,
		})
	}
	sort.Slice(calendars, func(i, j int) bool { return calendars[i].Name < calendars[j].Name })
	return calendars, nil
}

// readMetaFile reads a single-line vdir meta file (displayname/color),
// returning fallback when it is missing or empty.
func readMetaFile(calDir, name, fallback string) string {
	data, err := os.ReadFile(filepath.Join(calDir, name))
	if err != nil {
		return fallback
	}
	if s := strings.TrimSpace(string(data)); s != "" {
		return s
	}
	return fallback
}

// ExpandOccurrences returns the concrete occurrences of e that start within
// [from, to). A non-recurring event yields itself when its start falls in the
// window; a seriesMaster is expanded via its RRULE. Every occurrence preserves
// the master's duration (End = Start + (e.End - e.Start)) and copies all other
// fields (subject, location, attendees, ...) from the master — only Start/End
// shift.
func ExpandOccurrences(e Event, from, to time.Time) []Event {
	if e.Recurrence == nil {
		if inWindow(e.Start, from, to) {
			return []Event{e}
		}
		return nil
	}

	opt, err := recurrenceToROption(*e.Recurrence)
	if err != nil {
		slog.Warn("Cannot expand recurrence, showing master only", "module", "GRAPHCAL",
			"uid", e.ICalUID, "err", err)
		if inWindow(e.Start, from, to) {
			return []Event{e}
		}
		return nil
	}
	opt.Dtstart = e.Start.UTC()
	rule, err := rrule.NewRRule(*opt)
	if err != nil {
		slog.Warn("Cannot build recurrence rule, showing master only", "module", "GRAPHCAL",
			"uid", e.ICalUID, "err", err)
		if inWindow(e.Start, from, to) {
			return []Event{e}
		}
		return nil
	}

	duration := e.End.Sub(e.Start)
	var out []Event
	// Between(from, to, true) is inclusive on both ends; our window is
	// end-exclusive ([from, to)), so an occurrence landing exactly on `to` is
	// dropped to stay consistent with inWindow (used for single events).
	for _, start := range rule.Between(from, to, true) {
		if !start.Before(to) {
			continue
		}
		occ := e
		occ.Start = start.UTC()
		occ.End = start.UTC().Add(duration)
		out = append(out, occ)
	}
	return out
}

// inWindow reports whether t is within [from, to) (start inclusive, end
// exclusive).
func inWindow(t, from, to time.Time) bool {
	return !t.Before(from) && t.Before(to)
}

// ResolveLocalEvent finds exactly one event in the vdir for a short reference
// used by show/rsvp/delete. ref matches (in order) an exact iCalUID, a unique
// iCalUID prefix, or a unique case-insensitive substring of the subject. When
// calFilter is non-empty only that calendar (by display name, case-insensitive)
// is searched. It returns the REAL .ics file path (from the on-disk scan, so
// callers can rewrite/remove it), the parsed event and the calendar name.
// Ambiguous or absent matches are errors that name the candidates.
func ResolveLocalEvent(accountDir, owner, ref, calFilter string) (path string, ev Event, calendar string, err error) {
	entries, err := os.ReadDir(accountDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", Event{}, "", fmt.Errorf("no local calendars yet — run 'durian calendar sync' first")
		}
		return "", Event{}, "", fmt.Errorf("failed to read calendar dir %s: %w", accountDir, err)
	}

	// Fast path: an exact UID whose file follows the "<uid>.ics" naming scheme
	// (every synced or `new`-created event does) — read just that one file
	// instead of parsing the whole vdir.
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		calDir := filepath.Join(accountDir, entry.Name())
		calName := readMetaFile(calDir, "displayname", entry.Name())
		if calFilter != "" && !strings.EqualFold(calName, calFilter) {
			continue
		}
		p := filepath.Join(calDir, sanitizeName(ref)+".ics")
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			continue
		}
		if e, parseErr := ICalToEvent(data, owner); parseErr == nil && e.ICalUID == ref {
			return p, e, calName, nil
		}
	}

	type match struct {
		path     string
		event    Event
		calendar string
	}
	var exact, prefix, subject []match
	lowerRef := strings.ToLower(ref)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		calDir := filepath.Join(accountDir, entry.Name())
		calName := readMetaFile(calDir, "displayname", entry.Name())
		if calFilter != "" && !strings.EqualFold(calName, calFilter) {
			continue
		}
		items, _, err := scanLocalItems(calDir, owner)
		if err != nil {
			return "", Event{}, "", err
		}
		for _, it := range items {
			m := match{path: it.path, event: it.event, calendar: calName}
			switch {
			case it.event.ICalUID == ref:
				exact = append(exact, m)
			case strings.HasPrefix(it.event.ICalUID, ref):
				prefix = append(prefix, m)
			case strings.Contains(strings.ToLower(it.event.Subject), lowerRef):
				subject = append(subject, m)
			}
		}
	}

	for _, tier := range [][]match{exact, prefix, subject} {
		if len(tier) == 1 {
			return tier[0].path, tier[0].event, tier[0].calendar, nil
		}
		if len(tier) > 1 {
			var names []string
			for _, m := range tier {
				names = append(names, fmt.Sprintf("%q [%s]", m.event.Subject, m.calendar))
			}
			return "", Event{}, "", fmt.Errorf("%q matches %d events, be more specific: %s",
				ref, len(tier), strings.Join(names, ", "))
		}
	}
	return "", Event{}, "", fmt.Errorf("no event matches %q", ref)
}

// ParseWhen parses the date/time inputs the CLI accepts, all interpreted as
// UTC: RFC3339, "2006-01-02 15:04", a bare "2006-01-02" (reported as all-day),
// and the keywords "today"/"tomorrow" (all-day, midnight UTC). now is passed in
// so callers stay testable.
func ParseWhen(s string, now time.Time) (t time.Time, allDay bool, err error) {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "today":
		return dateOnly(now), true, nil
	case "tomorrow":
		return dateOnly(now).AddDate(0, 0, 1), true, nil
	}
	if t, err = time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), false, nil
	}
	if t, err = time.ParseInLocation("2006-01-02 15:04", s, time.UTC); err == nil {
		return t, false, nil
	}
	if t, err = time.ParseInLocation("2006-01-02", s, time.UTC); err == nil {
		return t, true, nil
	}
	return time.Time{}, false, fmt.Errorf("cannot parse date/time %q (use RFC3339, \"2006-01-02 15:04\", \"2006-01-02\", today or tomorrow)", s)
}

// dateOnly truncates t to midnight UTC of its calendar day.
func dateOnly(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// CalendarWindow computes the [from, to) window for a list query. fromStr/toStr
// (see ParseWhen) override the defaults; when absent, from is midnight today
// and to is from + defaultSpan. Shared by the CLI `list` command and the HTTP
// API. Returns an error if the resulting end is not after the start.
func CalendarWindow(fromStr, toStr string, defaultSpan time.Duration, now time.Time) (from, to time.Time, err error) {
	from = dateOnly(now)
	if strings.TrimSpace(fromStr) != "" {
		if from, _, err = ParseWhen(fromStr, now); err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	to = from.Add(defaultSpan)
	if strings.TrimSpace(toStr) != "" {
		if to, _, err = ParseWhen(toStr, now); err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("window end %s is not after start %s",
			to.Format(time.RFC3339), from.Format(time.RFC3339))
	}
	return from, to, nil
}

// EventMatchesQuery reports whether the already-lower-cased query occurs in the
// event's subject, location, description or any attendee name/address. An empty
// query matches everything. Shared by the CLI `search` command and the HTTP API.
func EventMatchesQuery(e Event, lowerQuery string) bool {
	if lowerQuery == "" {
		return true
	}
	if strings.Contains(strings.ToLower(e.Subject), lowerQuery) ||
		strings.Contains(strings.ToLower(e.Location), lowerQuery) ||
		strings.Contains(strings.ToLower(e.Description), lowerQuery) {
		return true
	}
	for _, a := range e.Attendees {
		if strings.Contains(strings.ToLower(a.Email), lowerQuery) ||
			strings.Contains(strings.ToLower(a.Name), lowerQuery) {
			return true
		}
	}
	return false
}

// WriteLocalEvent serializes e into a new .ics file in the calendar directory
// whose display name matches calendarName (case-insensitive), returning the
// written path. The calendar directory must already exist (created by a prior
// sync/export) — otherwise the next `durian calendar sync` would never scan the
// file, so this errors listing the available calendars instead. The filename is
// derived from the event's iCalUID.
func WriteLocalEvent(accountDir, calendarName string, e Event) (string, error) {
	calendars, err := ReadCalendars(accountDir, "")
	if err != nil {
		return "", err
	}
	var dir string
	var names []string
	for _, cal := range calendars {
		names = append(names, cal.Name)
		if strings.EqualFold(cal.Name, calendarName) {
			dir = cal.Dir
			break
		}
	}
	if dir == "" {
		if len(names) == 0 {
			return "", fmt.Errorf("no local calendars yet — run 'durian calendar sync' first")
		}
		return "", fmt.Errorf("calendar %q not found; available: %s", calendarName, strings.Join(names, ", "))
	}

	data, err := EventToICal(e)
	if err != nil {
		return "", fmt.Errorf("failed to serialize new event: %w", err)
	}
	path := filepath.Join(dir, sanitizeName(e.ICalUID)+".ics")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("failed to write %s: %w", path, err)
	}
	return path, nil
}
