package calendar

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedCollection writes one event into dir, creating it.
func seedCollection(t *testing.T, dir, uid, subject string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	ev := Event{
		ICalUID:      uid,
		Subject:      subject,
		Start:        mustTime(t, "2026-08-03T09:00:00Z"),
		End:          mustTime(t, "2026-08-03T10:00:00Z"),
		LastModified: mustTime(t, "2026-08-01T00:00:00Z"),
	}
	data, err := EventToICal(ev)
	if err != nil {
		t.Fatalf("EventToICal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, uid+".ics"), data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// A collection may live anywhere — that is the whole point of the type, and
// what lets a configured local calendar point at an existing khal folder.
func TestReadCollectionsAcceptsUnrelatedDirectories(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "somewhere", "privat")
	b := filepath.Join(root, "elsewhere", "verein")
	seedCollection(t, a, "uid-a", "Zahnarzt")
	seedCollection(t, b, "uid-b", "Mitgliederversammlung")

	cals, err := ReadCollections([]Collection{
		{Dir: a, Name: "Privat", HexColor: "#8ab4f8"},
		{Dir: b, Name: "Verein"},
	})
	if err != nil {
		t.Fatalf("ReadCollections: %v", err)
	}
	if len(cals) != 2 {
		t.Fatalf("got %d calendars, want 2: %+v", len(cals), cals)
	}
	// Sorted by name: Privat, Verein.
	if cals[0].Name != "Privat" || cals[0].HexColor != "#8ab4f8" {
		t.Errorf("first calendar = %+v, want the configured name and color", cals[0])
	}
	if len(cals[0].Events) != 1 || cals[0].Events[0].Subject != "Zahnarzt" {
		t.Errorf("Privat events = %+v", cals[0].Events)
	}
	if cals[1].Name != "Verein" || len(cals[1].Events) != 1 {
		t.Errorf("second calendar = %+v", cals[1])
	}
}

// A configured calendar names a path the user chose, not one a sync produced —
// it is allowed to not exist yet.
func TestReadCollectionsTreatsAMissingDirAsEmpty(t *testing.T) {
	cals, err := ReadCollections([]Collection{
		{Dir: filepath.Join(t.TempDir(), "not-created-yet"), Name: "Privat"},
	})
	if err != nil {
		t.Fatalf("ReadCollections: %v", err)
	}
	if len(cals) != 1 || cals[0].Name != "Privat" || len(cals[0].Events) != 0 {
		t.Errorf("got %+v, want one empty calendar named Privat", cals)
	}
}

// The configured name wins over the meta file, so renaming a calendar in the
// config does not require touching the folder.
func TestReadCollectionsPrefersTheConfiguredName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "collection")
	seedCollection(t, dir, "uid-a", "Termin")
	if err := os.WriteFile(filepath.Join(dir, "displayname"), []byte("Aus der Datei"), 0o600); err != nil {
		t.Fatalf("write displayname: %v", err)
	}

	cals, err := ReadCollections([]Collection{{Dir: dir, Name: "Aus der Config"}})
	if err != nil {
		t.Fatalf("ReadCollections: %v", err)
	}
	if cals[0].Name != "Aus der Config" {
		t.Errorf("name = %q, want the configured one", cals[0].Name)
	}

	// Without a configured name the meta file still wins over the dir name.
	cals, err = ReadCollections([]Collection{{Dir: dir}})
	if err != nil {
		t.Fatalf("ReadCollections: %v", err)
	}
	if cals[0].Name != "Aus der Datei" {
		t.Errorf("name = %q, want the displayname meta file", cals[0].Name)
	}
}

func TestCollectionsUnderSkipsHiddenRecoveryDirectories(t *testing.T) {
	accountDir := t.TempDir()
	seedCollection(t, filepath.Join(accountDir, "Work"), "uid-a", "Termin")
	seedCollection(t, filepath.Join(accountDir, ".orphaned"), "uid-b", "Backup")

	cols, err := CollectionsUnder(accountDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 1 || filepath.Base(cols[0].Dir) != "Work" {
		t.Fatalf("collections = %+v, want only Work", cols)
	}
}

func TestWriteEventInCreatesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "privat")
	cols := []Collection{{Dir: dir, Name: "Privat"}}

	path, err := WriteEventIn(cols, "Privat", Event{
		ICalUID: "uid-new", Subject: "Neu",
		Start: mustTime(t, "2026-08-03T09:00:00Z"),
		End:   mustTime(t, "2026-08-03T10:00:00Z"),
	})
	if err != nil {
		t.Fatalf("WriteEventIn: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("event file not written: %v", err)
	}
}

// A read-only calendar exists so another tool can own the folder. Every write
// path has to refuse it, not just the ones that happen to check.
func TestWriteEventInRefusesReadOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "verein")
	cols := []Collection{{Dir: dir, Name: "Verein", ReadOnly: true}}

	if _, err := WriteEventIn(cols, "Verein", Event{ICalUID: "uid-new", Subject: "Neu"}); err == nil {
		t.Fatal("WriteEventIn wrote to a read-only calendar")
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("a read-only calendar had its directory created")
	}
}

func TestResolveEventInFindsAcrossCollections(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "privat")
	b := filepath.Join(root, "verein")
	seedCollection(t, a, "uid-a", "Zahnarzt")
	seedCollection(t, b, "uid-b", "Mitgliederversammlung")
	cols := []Collection{{Dir: a, Name: "Privat"}, {Dir: b, Name: "Verein"}}

	_, ev, calName, err := ResolveEventIn(cols, "uid-b", "")
	if err != nil {
		t.Fatalf("ResolveEventIn: %v", err)
	}
	if ev.Subject != "Mitgliederversammlung" || calName != "Verein" {
		t.Errorf("resolved %q in %q, want the Verein event", ev.Subject, calName)
	}

	// The calendar filter matches the configured name.
	if _, _, _, err := ResolveEventIn(cols, "uid-b", "Privat"); err == nil {
		t.Error("the calendar filter did not scope the lookup")
	}
}

// TestResolveEventInReportsSharedUIDAcrossAccounts covers the case a fast path
// used to swallow. The resolver read "<uid>.ics" directly and returned the
// first collection where it parsed, so the same UID in two accounts silently
// resolved to whichever came first — and every command built on this then acted
// on an event the user had not named.
//
// An exact UID is a single-resource reference: ambiguity is reported, never
// resolved by position.
func TestResolveEventInReportsSharedUIDAcrossAccounts(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	personal := filepath.Join(root, "personal")
	seedCollection(t, work, "shared-uid", "Standup")
	seedCollection(t, personal, "shared-uid", "Zahnarzt")
	cols := []Collection{
		{Dir: work, Name: "Work", Account: "work"},
		{Dir: personal, Name: "Personal", Account: "personal"},
	}

	_, _, _, err := ResolveEventIn(cols, "shared-uid", "")
	if err == nil {
		t.Fatal("ResolveEventIn resolved a UID held by two accounts instead of reporting it")
	}
	for _, want := range []string{"Standup", "Zahnarzt", "work", "personal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q; the user cannot tell the candidates apart", err, want)
		}
	}

	// Naming one of them resolves it, which is what makes the error actionable.
	_, ev, calName, err := ResolveEventIn(cols, "shared-uid", "Personal")
	if err != nil {
		t.Fatalf("scoped resolution: %v", err)
	}
	if ev.Subject != "Zahnarzt" || calName != "Personal" {
		t.Errorf("resolved %q in %q, want the Personal event", ev.Subject, calName)
	}
}

// TestResolveEventInFindsUIDUnderAnyFilename pins why the fast path could not
// simply collect its hits instead of being removed: a file is free to be named
// anything, so proving that one "<uid>.ics" match is unique means reading the
// others regardless.
func TestResolveEventInFindsUIDUnderAnyFilename(t *testing.T) {
	dir := t.TempDir()
	seedCollection(t, dir, "renamed-uid", "Offsite")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one seeded file, got %d", len(entries))
	}
	old := filepath.Join(dir, entries[0].Name())
	if err := os.Rename(old, filepath.Join(dir, "not-the-uid.ics")); err != nil {
		t.Fatalf("rename: %v", err)
	}

	_, ev, _, err := ResolveEventIn([]Collection{{Dir: dir, Name: "Cal"}}, "renamed-uid", "")
	if err != nil {
		t.Fatalf("ResolveEventIn: %v", err)
	}
	if ev.Subject != "Offsite" {
		t.Errorf("resolved %q, want Offsite", ev.Subject)
	}
}

// Pointing a configured calendar at a vdir BASE directory instead of a
// collection yields an empty calendar with nothing to explain it — the mistake
// is easy to make, since durian's own account layout is exactly such a base.
func TestInspectCollectionsDetectsAVdirBase(t *testing.T) {
	base := filepath.Join(t.TempDir(), "taskwarrior")
	seedCollection(t, filepath.Join(base, "blocks"), "uid-a", "Block")

	cols := []Collection{{Dir: base, Name: "Taskwarrior"}}
	cals, err := ReadCollections(cols)
	if err != nil {
		t.Fatalf("ReadCollections: %v", err)
	}
	if len(cals) != 1 || len(cals[0].Events) != 0 {
		t.Fatalf("expected one empty calendar, got %+v", cals)
	}

	bad := InspectCollections(cols, cals)
	if len(bad) != 1 {
		t.Fatalf("InspectCollections = %+v, want the vdir base flagged", bad)
	}
	if len(bad[0].SubCollections) != 1 ||
		!strings.HasSuffix(bad[0].SubCollections[0], "blocks") {
		t.Errorf("sub-collections = %v, want the blocks dir", bad[0].SubCollections)
	}
	if hint := bad[0].Hint(); !strings.Contains(hint, "blocks") ||
		!strings.Contains(hint, "Taskwarrior") {
		t.Errorf("hint = %q, want it to name the calendar and the corrected path", hint)
	}
}

// A collection that legitimately holds events, and one that is simply empty,
// must never be flagged.
func TestInspectCollectionsIgnoresHealthyAndEmptyCollections(t *testing.T) {
	root := t.TempDir()
	withEvents := filepath.Join(root, "privat")
	seedCollection(t, withEvents, "uid-a", "Termin")
	brandNew := filepath.Join(root, "neu")

	cols := []Collection{{Dir: withEvents, Name: "Privat"}, {Dir: brandNew, Name: "Neu"}}
	cals, err := ReadCollections(cols)
	if err != nil {
		t.Fatalf("ReadCollections: %v", err)
	}
	if bad := InspectCollections(cols, cals); len(bad) != 0 {
		t.Errorf("InspectCollections = %+v, want nothing flagged", bad)
	}
}

// Correcting a too-high path by appending the collection name to an already
// corrected one produces a doubled segment. The directory then does not exist,
// and without this the calendar is silently empty — the exact symptom the
// correction was meant to cure.
func TestInspectCollectionsDetectsADoubledPathSegment(t *testing.T) {
	base := filepath.Join(t.TempDir(), "taskwarrior")
	collection := filepath.Join(base, "blocks")
	seedCollection(t, collection, "uid-a", "Block")

	doubled := filepath.Join(collection, "blocks")
	cols := []Collection{{Dir: doubled, Name: "Taskwarrior"}}
	cals, err := ReadCollections(cols)
	if err != nil {
		t.Fatalf("ReadCollections: %v", err)
	}

	bad := InspectCollections(cols, cals)
	if len(bad) != 1 || !bad[0].Missing {
		t.Fatalf("InspectCollections = %+v, want the missing path flagged", bad)
	}
	if len(bad[0].SubCollections) != 1 || bad[0].SubCollections[0] != collection {
		t.Errorf("suggestions = %v, want the real collection %q", bad[0].SubCollections, collection)
	}
	if hint := bad[0].Hint(); !strings.Contains(hint, "does not exist") {
		t.Errorf("hint = %q, want it to say the path is missing", hint)
	}
}

// Creating a calendar at a path that does not exist yet is legitimate: the
// first write creates it. Warning about that would nag rather than help, so it
// is reported only when there is somewhere better to point.
func TestInspectCollectionsStaysQuietForANewCalendar(t *testing.T) {
	root := t.TempDir()
	cols := []Collection{{Dir: filepath.Join(root, "neu"), Name: "Neu"}}
	cals, err := ReadCollections(cols)
	if err != nil {
		t.Fatalf("ReadCollections: %v", err)
	}
	if bad := InspectCollections(cols, cals); len(bad) != 0 {
		t.Errorf("InspectCollections = %+v, want silence for a not-yet-created calendar", bad)
	}
}
