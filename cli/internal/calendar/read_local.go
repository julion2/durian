// Offline readers for the local vdir: enumerate calendars and events straight
// from the .ics files on disk (no Graph call, no token), expand recurring
// masters into concrete occurrences for a time window, resolve a single event
// by a short reference, and parse the human date/time inputs the CLI accepts.
//
// These power the local-first `durian calendar` subcommands (list/search/show/
// new/rsvp/delete): every one of them reads or writes the vdir only, and the
// actual Outlook effect happens later through `durian calendar sync`.

package calendar

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/teambition/rrule-go"
)

// ErrReadOnly is wrapped by write operations that target a read-only calendar
// collection, so the HTTP layer can map it to 403 Forbidden rather than a
// generic 400.
var ErrReadOnly = errors.New("calendar is read-only")

// LocalCalendar is one calendar directory of the vdir, with its display name,
// color (from the displayname/color meta files) and the master events parsed
// from its .ics files.
type LocalCalendar struct {
	Name     string
	HexColor string
	Dir      string
	Events   []Event
	// Account is the identifier the calendar belongs to ("local" for a
	// configured local calendar), so a listing spanning several accounts can
	// tell two identically named calendars apart.
	Account string
}

// Collection names one calendar collection directory before its events are
// read. It is what lets a set of calendars come from somewhere other than the
// subdirectories of one account dir — a configured local calendar points at an
// arbitrary path and carries its own display metadata.
type Collection struct {
	// Dir is the directory holding the .ics files.
	Dir string
	// Name overrides the display name; empty falls back to the "displayname"
	// meta file and then to the directory name.
	Name string
	// HexColor overrides the color; empty falls back to the "color" meta file.
	HexColor string
	// ReadOnly marks a collection durian must never write to.
	ReadOnly bool
	// Owner is the account email whose own RSVP is recognized while parsing
	// this collection's events. It belongs to the collection rather than to
	// the read call because one read can span several accounts, and the wrong
	// owner would read another person's PARTSTAT as the user's own.
	Owner string
	// Account is the identifier this collection belongs to, carried so output
	// covering several accounts can say which one an event came from.
	Account string
}

// CollectionsUnder enumerates the calendar collections of a vdir account
// directory: one per subdirectory, display metadata taken from the meta files.
// A missing dir yields no collections (nothing synced yet), not an error.
func CollectionsUnder(accountDir string) ([]Collection, error) {
	return CollectionsUnderFor(accountDir, "", "")
}

// CollectionsUnderFor is CollectionsUnder with the owning account attached to
// every collection it produces.
func CollectionsUnderFor(accountDir, account, owner string) ([]Collection, error) {
	entries, err := os.ReadDir(accountDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read calendar dir %s: %w", accountDir, err)
	}

	var cols []Collection
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		cols = append(cols, Collection{
			Dir:     filepath.Join(accountDir, entry.Name()),
			Name:    readMetaFile(filepath.Join(accountDir, entry.Name()), "displayname", entry.Name()),
			Account: account,
			Owner:   owner,
		})
	}
	return cols, nil
}

// ReadCalendars enumerates every calendar directory under accountDir and parses
// the events of each from disk — entirely offline. See ReadCollections.
func ReadCalendars(accountDir, owner string) ([]LocalCalendar, error) {
	cols, err := CollectionsUnderFor(accountDir, "", owner)
	if err != nil {
		return nil, err
	}
	return ReadCollections(cols)
}

// ReadCollections parses the events of the given collections from disk —
// entirely offline. The display name comes from the collection when it states
// one, else from the "displayname" meta file, else from the directory name;
// the color likewise falls back to the "color" file. Each collection carries
// its own Owner, so a read spanning several accounts recognizes each one's own
// RSVP. A collection whose directory does not exist yet contributes an empty calendar
// rather than an error — a configured local calendar is allowed to be created
// on first write.
func ReadCollections(cols []Collection) ([]LocalCalendar, error) {
	var calendars []LocalCalendar
	for _, col := range cols {
		items, _, err := ScanLocalItems(col.Dir, col.Owner)
		if err != nil {
			return nil, err
		}
		events := make([]Event, 0, len(items))
		for _, it := range items {
			events = append(events, it.Event)
		}
		sort.Slice(events, func(i, j int) bool { return events[i].Start.Before(events[j].Start) })

		calendars = append(calendars, LocalCalendar{
			Name:     firstNonEmpty(col.Name, readMetaFile(col.Dir, "displayname", filepath.Base(col.Dir))),
			HexColor: firstNonEmpty(col.HexColor, readMetaFile(col.Dir, "color", "")),
			Dir:      col.Dir,
			Events:   events,
			Account:  col.Account,
		})
	}
	sort.Slice(calendars, func(i, j int) bool {
		if calendars[i].Account != calendars[j].Account {
			return calendars[i].Account < calendars[j].Account
		}
		return calendars[i].Name < calendars[j].Name
	})
	return calendars, nil
}

// firstNonEmpty returns the first non-empty argument, or "".
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
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

// ExpandOccurrences returns the concrete occurrences of e that overlap
// [from, to): included iff start < to && end > from (half-open, so an event
// merely touching an edge — end == from or start == to — is dropped). A
// non-recurring event yields itself when it overlaps the window, including one
// that started before `from` but is still running inside it; a seriesMaster is
// expanded via its RRULE. Every occurrence preserves the master's duration
// (End = Start + (e.End - e.Start)) and copies all other fields (subject,
// location, attendees, ...) from the master — only Start/End shift.
//
// The RRULE describes the series as it was DEFINED; the exceptions describe
// what actually happened to it. So the raw expansion is only an intermediate
// result — applySeriesExceptions turns it into the real occurrence list.
func ExpandOccurrences(e Event, from, to time.Time) []Event {
	if e.Recurrence == nil {
		if overlaps(e.Start, e.End, from, to) {
			return []Event{e}
		}
		return nil
	}

	opt, err := RecurrenceToROption(*e.Recurrence)
	if err != nil {
		slog.Warn("Cannot expand recurrence, showing master only", "module", "GRAPHCAL",
			"uid", e.ICalUID, "err", err)
		if overlaps(e.Start, e.End, from, to) {
			return []Event{e}
		}
		return nil
	}
	opt.Dtstart = e.Start.UTC()
	rule, err := rrule.NewRRule(*opt)
	if err != nil {
		slog.Warn("Cannot build recurrence rule, showing master only", "module", "GRAPHCAL",
			"uid", e.ICalUID, "err", err)
		if overlaps(e.Start, e.End, from, to) {
			return []Event{e}
		}
		return nil
	}

	duration := e.End.Sub(e.Start)
	var raw []Event
	// Between matches occurrence STARTS only, so it would miss an occurrence
	// that begins before `from` yet is still running inside the window. Widen
	// the lower bound by one occurrence duration to catch it; the overlap test
	// below filters precisely, so the widening never admits an occurrence that
	// ends at or before `from`. Between(a, b, true) is inclusive on both ends;
	// our window is end-exclusive ([from, to)), so an occurrence landing
	// exactly on `to` is dropped to stay consistent with the single-event case.
	for _, start := range rule.Between(from.Add(-duration), to, true) {
		if !start.Before(to) {
			continue
		}
		occ := e
		// An occurrence is not a series: it carries neither the exception list
		// nor the overrides of its master, only the date it happens on.
		occ.ExceptionDates = nil
		occ.Overrides = nil
		occ.Start = start.UTC()
		occ.End = start.UTC().Add(duration)
		if !overlaps(occ.Start, occ.End, from, to) {
			continue
		}
		raw = append(raw, occ)
	}
	return applySeriesExceptions(e, raw, from, to)
}

// applySeriesExceptions folds a master's series exceptions into the raw
// occurrences its RRULE produced for [from, to), returning what the series
// ACTUALLY looks like in that window.
//
// raw holds one Event per date the rule generated; each already overlaps the
// window, and each carries its rule-generated Start — which is exactly the
// RECURRENCE-ID an exception would refer to. master.ExceptionDates lists the
// cancelled dates and master.Overrides the modified ones, every override
// keyed by the ORIGINAL start it replaces (RecurrenceID), not by where it
// ended up.
func applySeriesExceptions(master Event, raw []Event, from, to time.Time) []Event {
	if len(master.ExceptionDates) == 0 && len(master.Overrides) == 0 {
		return raw
	}

	cancelled := make(map[int64]bool, len(master.ExceptionDates))
	for _, d := range master.ExceptionDates {
		cancelled[d.UTC().Unix()] = true
	}
	overridden := make(map[int64]Event, len(master.Overrides))
	for _, o := range master.Overrides {
		overridden[o.RecurrenceID.UTC().Unix()] = o
	}

	// raw decides only which UNMODIFIED occurrences survive. A cancelled date
	// is gone; a modified one is skipped here on purpose, because the override
	// that replaces it is emitted from master.Overrides below instead.
	var out []Event
	for _, occ := range raw {
		key := occ.Start.UTC().Unix()
		if cancelled[key] {
			continue
		}
		if _, modified := overridden[key]; modified {
			continue
		}
		out = append(out, occ)
	}

	// Overrides are placed by where they ACTUALLY happen, never by the date
	// they replace. Driving this loop from master.Overrides instead of from
	// raw is what makes both directions of a moved occurrence work: one moved
	// OUT of the window fails the overlap test and vanishes, and one moved IN
	// from outside is still emitted even though the rule produced no
	// occurrence for it inside [from, to) to hang it on.
	for _, o := range master.Overrides {
		if cancelled[o.RecurrenceID.UTC().Unix()] {
			continue
		}
		if !overlaps(o.Start, o.End, from, to) {
			continue
		}
		out = append(out, o)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// overlaps reports whether the event interval [start, end) intersects the
// query window [from, to). Both intervals are half-open: an event whose end
// equals `from`, or whose start equals `to`, does not overlap.
func overlaps(start, end, from, to time.Time) bool {
	return start.Before(to) && end.After(from)
}

// ResolveLocalEvent finds exactly one event in the vdir for a short reference
// used by show/rsvp/delete. ref matches (in order) an exact iCalUID, a unique
// iCalUID prefix, or a unique case-insensitive substring of the subject. When
// calFilter is non-empty only that calendar (by display name, case-insensitive)
// is searched. It returns the REAL .ics file path (from the on-disk scan, so
// callers can rewrite/remove it), the parsed event and the calendar name.
// Ambiguous or absent matches are errors that name the candidates.
func ResolveLocalEvent(accountDir, owner, ref, calFilter string) (path string, ev Event, calendar string, err error) {
	cols, err := CollectionsUnder(accountDir)
	if err != nil {
		return "", Event{}, "", err
	}
	if len(cols) == 0 {
		return "", Event{}, "", fmt.Errorf("no local calendars yet — run 'durian calendar sync' first")
	}
	for i := range cols {
		cols[i].Owner = owner
	}
	return ResolveEventIn(cols, ref, calFilter)
}

// ResolveEventIn is ResolveLocalEvent over an explicit collection set, so a
// caller whose calendars do not live under one account directory — the
// configured local calendars — resolves references the same way.
func ResolveEventIn(cols []Collection, ref, calFilter string) (path string, ev Event, calendar string, err error) {
	// There used to be a fast path here: read "<uid>.ics" directly and return
	// the first collection where it parsed with a matching UID. It contradicted
	// the contract this resolver exists to keep — the same UID in two accounts
	// returned whichever collection came first, and the ambiguity check below
	// was never reached.
	//
	// It cannot be repaired by collecting the hits either. A file named
	// anything else can carry the same UID, so proving a single "<uid>.ics"
	// match is unambiguous requires reading the other files anyway. An
	// optimisation that has to do the work it exists to avoid is not one.
	//
	// The scan below is the whole resolution, for every kind of reference.
	type match struct {
		path     string
		event    Event
		calendar string
		account  string
	}
	var exact, prefix, subject []match
	lowerRef := strings.ToLower(ref)
	for _, col := range cols {
		calDir := col.Dir
		calName := collectionName(col)
		if calFilter != "" && !strings.EqualFold(calName, calFilter) {
			continue
		}
		items, _, err := ScanLocalItems(calDir, col.Owner)
		if err != nil {
			return "", Event{}, "", err
		}
		for _, it := range items {
			m := match{path: it.Path, event: it.Event, calendar: calName, account: col.Account}
			switch {
			case it.Event.ICalUID == ref:
				exact = append(exact, m)
			case strings.HasPrefix(it.Event.ICalUID, ref):
				prefix = append(prefix, m)
			case strings.Contains(strings.ToLower(it.Event.Subject), lowerRef):
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
				candidate := fmt.Sprintf("%q [event:%s, calendar:%s", m.event.Subject, m.event.ICalUID, m.calendar)
				if m.account != "" {
					candidate += ", account:" + m.account
				}
				names = append(names, candidate+"]")
			}
			return "", Event{}, "", fmt.Errorf("%q matches %d events, be more specific: %s",
				ref, len(tier), strings.Join(names, ", "))
		}
	}
	return "", Event{}, "", fmt.Errorf("no event matches %q", ref)
}

// CollectionAccountForPath returns the account owning an event file resolved
// from cols. It returns an empty string when path is outside every collection.
func CollectionAccountForPath(cols []Collection, path string) string {
	path = filepath.Clean(path)
	for _, col := range cols {
		rel, err := filepath.Rel(filepath.Clean(col.Dir), path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return col.Account
		}
	}
	return ""
}

// ParseWhen parses the date/time inputs the CLI accepts, all interpreted as
// UTC: RFC3339, "2006-01-02 15:04", a bare "2006-01-02" (reported as all-day),
// and the keywords "today"/"tomorrow" (all-day, midnight UTC). now is passed in
// so callers stay testable.
func ParseWhen(s string, now time.Time) (t time.Time, allDay bool, err error) {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "today":
		return DateOnly(now), true, nil
	case "tomorrow":
		return DateOnly(now).AddDate(0, 0, 1), true, nil
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

// DateOnly truncates t to midnight UTC of its calendar day.
func DateOnly(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// CalendarWindow computes the [from, to) window for a list query. fromStr/toStr
// (see ParseWhen) override the defaults; when absent, from is midnight today
// and to is from + defaultSpan. Shared by the CLI `list` command and the HTTP
// API. Returns an error if the resulting end is not after the start.
func CalendarWindow(fromStr, toStr string, defaultSpan time.Duration, now time.Time) (from, to time.Time, err error) {
	from = DateOnly(now)
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
	cols, err := CollectionsUnder(accountDir)
	if err != nil {
		return "", err
	}
	return WriteEventIn(cols, calendarName, e)
}

// WriteEventIn is WriteLocalEvent over an explicit collection set. Unlike the
// account path it CREATES the collection directory when it is missing: a
// configured local calendar is a path the user named, not one a sync produced,
// so the first write is what brings it into existence. A read-only collection
// is refused.
func WriteEventIn(cols []Collection, calendarName string, e Event) (string, error) {
	var target *Collection
	var names []string
	for i := range cols {
		name := collectionName(cols[i])
		names = append(names, name)
		if strings.EqualFold(name, calendarName) {
			target = &cols[i]
			break
		}
	}
	if target == nil {
		if len(names) == 0 {
			return "", fmt.Errorf("no local calendars yet — run 'durian calendar sync' first")
		}
		return "", fmt.Errorf("calendar %q not found; available: %s", calendarName, strings.Join(names, ", "))
	}
	if target.ReadOnly {
		return "", fmt.Errorf("calendar %q is read-only: %w", calendarName, ErrReadOnly)
	}
	dir := target.Dir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("failed to create calendar dir %s: %w", dir, err)
	}

	data, err := EventToICal(e)
	if err != nil {
		return "", fmt.Errorf("failed to serialize new event: %w", err)
	}
	path := filepath.Join(dir, EventFileName(e.ICalUID))
	if err := WriteFileAtomic(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// CollectionName is the display name of a collection: what it states, else its
// "displayname" meta file, else the directory name. Callers that match a
// collection by name must use this rather than Collection.Name, which is empty
// for collections discovered under an account directory.
func CollectionName(col Collection) string {
	return collectionName(col)
}

// CollectionOwner is the account email of the collection with the given display
// name — whose own RSVP the event parse recognizes. Empty when no collection
// matches, and for a local calendar, which has no account holder to speak for.
func CollectionOwner(cols []Collection, name string) string {
	for _, col := range cols {
		if strings.EqualFold(CollectionName(col), name) {
			return col.Owner
		}
	}
	return ""
}

// collectionName is the display name of a collection: what it states, else
// its "displayname" meta file, else the directory name.
func collectionName(col Collection) string {
	if col.Name != "" {
		return col.Name
	}
	return readMetaFile(col.Dir, "displayname", filepath.Base(col.Dir))
}

// MisconfiguredCollection is a configured calendar whose path does not point at
// a collection of .ics files, together with the paths that do.
//
// Both shapes it catches produce the same useless symptom — an empty calendar
// with nothing to explain it — and both are easy to reach: durian's own account
// layout is a vdir base, so pointing one level too high is natural, and
// correcting it by appending the collection name to an already-corrected path
// is how the opposite happens.
type MisconfiguredCollection struct {
	Collection Collection
	// Missing reports that the directory does not exist at all, as opposed to
	// existing but holding only sub-collections.
	Missing bool
	// SubCollections are the paths that do contain .ics files: the collections
	// below a vdir base, or — for a missing path — what its nearest existing
	// ancestor offers.
	SubCollections []string
}

// Hint renders the diagnosis as a multi-line message naming the corrected path.
func (m MisconfiguredCollection) Hint() string {
	name := CollectionName(m.Collection)
	var b strings.Builder
	if m.Missing {
		fmt.Fprintf(&b, "calendar %q points at %s, which does not exist.", name, m.Collection.Dir)
	} else {
		fmt.Fprintf(&b, "calendar %q (%s) contains no .ics files but %d calendar collection",
			name, m.Collection.Dir, len(m.SubCollections))
		if len(m.SubCollections) != 1 {
			b.WriteString("s")
		}
		b.WriteString(".")
	}

	b.WriteString("\n  Point the entry at ")
	if len(m.SubCollections) == 1 {
		fmt.Fprintf(&b, "this instead:\n    path = %q", m.SubCollections[0])
	} else {
		b.WriteString("one of these instead:")
		for _, sub := range m.SubCollections {
			fmt.Fprintf(&b, "\n    path = %q", sub)
		}
	}
	return b.String()
}

// InspectCollections reports the configured calendars whose path cannot hold
// the events they were expected to.
//
// Only collections that yielded no events are examined, so a working calendar
// never pays for the check and never trips it. A missing directory is reported
// ONLY when its nearest existing ancestor offers somewhere better to point:
// creating a calendar at a path that does not exist yet is legitimate — the
// first write creates it — and warning about that would nag rather than help.
func InspectCollections(cols []Collection, calendars []LocalCalendar) []MisconfiguredCollection {
	empty := make(map[string]bool, len(calendars))
	for _, cal := range calendars {
		if len(cal.Events) == 0 {
			empty[cal.Dir] = true
		}
	}

	var out []MisconfiguredCollection
	for _, col := range cols {
		if !empty[col.Dir] {
			continue
		}
		if _, err := os.Stat(col.Dir); err != nil {
			if subs := nearbyCollections(col.Dir); len(subs) > 0 {
				out = append(out, MisconfiguredCollection{
					Collection: col, Missing: true, SubCollections: subs,
				})
			}
			continue
		}
		if subs := subCollectionsOf(col.Dir); len(subs) > 0 {
			out = append(out, MisconfiguredCollection{Collection: col, SubCollections: subs})
		}
	}
	return out
}

// nearbyCollections suggests where a missing path probably meant to point.
//
// It reports the nearest existing ancestor ONLY when that ancestor is itself a
// collection — which is precisely what a doubled path segment looks like, and
// nothing else does. Suggesting the ancestor's sibling collections instead
// would flag the perfectly ordinary case of adding a new calendar next to an
// existing one, which is the difference between a warning worth reading and
// one worth silencing.
func nearbyCollections(dir string) []string {
	for cur := filepath.Dir(dir); cur != "" && cur != "/" && cur != "."; cur = filepath.Dir(cur) {
		if _, err := os.Stat(cur); err != nil {
			continue
		}
		if hasICS(cur) {
			return []string{cur}
		}
		return nil
	}
	return nil
}

// hasICS reports whether dir directly contains at least one .ics file.
func hasICS(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".ics") {
			return true
		}
	}
	return false
}

// subCollectionsOf returns the direct subdirectories of dir that contain at
// least one .ics file, sorted.
func subCollectionsOf(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if sub := filepath.Join(dir, entry.Name()); hasICS(sub) {
			out = append(out, sub)
		}
	}
	sort.Strings(out)
	return out
}
