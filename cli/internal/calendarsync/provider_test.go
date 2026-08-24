// Provider-seam conformance tests: a minimal fake CalendarProvider locks the
// engine's safety rails at the CalendarProvider interface itself, independent
// of any real provider — what the engine passes across the seam (role-gated
// attendee flags, idempotency keys, planned etags) and how it reacts to the
// neutral error sentinels (ErrPrecondition skips, ErrNotFound folds into
// success). Future providers inherit these guarantees for free.

package calendarsync_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendarsync"
)

// fakeProvider is an in-memory CalendarProvider recording every write call.
type fakeProvider struct {
	// events is what FetchMasterEvents returns.
	events []Event
	// calendars and eventsByCalendar support collection identity tests. When
	// calendars is nil, the single-calendar fields below retain their defaults.
	calendars        []Calendar
	eventsByCalendar map[string][]Event
	// calendarName and noCalendars control ListCalendars for collection
	// lifecycle tests. Empty name defaults to Work.
	calendarName string
	noCalendars  bool

	// createResult is returned by CreateEvent; the received event/options are
	// recorded per call.
	createResult Event
	createEvents []Event
	createOpts   []calendarsync.CreateOptions

	// updateErr, when set, fails every UpdateEvent; specs are recorded by
	// event id.
	updateErr   error
	updateSpecs map[string]calendarsync.UpdateSpec

	// deleteErr, when set, fails every DeleteEvent; deleted ids, their etags
	// and the notification intent the engine passed are recorded.
	deleteErr    error
	deletes      []string
	deleteETags  map[string]string
	deleteNotify map[string]bool

	// responds records RespondToEvent calls (event id -> response).
	responds map[string]calendarsync.OwnerResp
}

func (f *fakeProvider) Owner() string { return testOwnerEmail }

func (f *fakeProvider) ListCalendars(context.Context) ([]Calendar, error) {
	if f.noCalendars {
		return nil, nil
	}
	if f.calendars != nil {
		return f.calendars, nil
	}
	name := f.calendarName
	if name == "" {
		name = "Work"
	}
	return []Calendar{{ID: "cal1", Name: name}}, nil
}

func (f *fakeProvider) FetchMasterEvents(_ context.Context, calendarID string) ([]Event, error) {
	if f.eventsByCalendar != nil {
		return f.eventsByCalendar[calendarID], nil
	}
	return f.events, nil
}

func (f *fakeProvider) FetchInstances(context.Context, string, time.Time, time.Time) ([]Event, error) {
	return nil, nil
}

func (f *fakeProvider) GetEvent(context.Context, string, string) (Event, error) {
	// No settled read-back: the engine falls back to the create response.
	return Event{}, fmt.Errorf("no read-back: %w", calendarsync.ErrNotFound)
}

func (f *fakeProvider) CreateEvent(_ context.Context, _ string, ev Event, opts calendarsync.CreateOptions) (Event, error) {
	f.createEvents = append(f.createEvents, ev)
	f.createOpts = append(f.createOpts, opts)
	return f.createResult, nil
}

func (f *fakeProvider) UpdateEvent(_ context.Context, _, eventID string, spec calendarsync.UpdateSpec) error {
	if f.updateSpecs == nil {
		f.updateSpecs = map[string]calendarsync.UpdateSpec{}
	}
	f.updateSpecs[eventID] = spec
	return f.updateErr
}

func (f *fakeProvider) DeleteEvent(_ context.Context, _, eventID, etag string, notify bool) error {
	if f.deleteETags == nil {
		f.deleteETags = map[string]string{}
		f.deleteNotify = map[string]bool{}
	}
	f.deletes = append(f.deletes, eventID)
	f.deleteETags[eventID] = etag
	f.deleteNotify[eventID] = notify
	return f.deleteErr
}

func (f *fakeProvider) RespondToEvent(_ context.Context, _, eventID string, resp calendarsync.OwnerResp, _ bool, _ string) error {
	if f.responds == nil {
		f.responds = map[string]calendarsync.OwnerResp{}
	}
	f.responds[eventID] = resp
	return nil
}

func (f *fakeProvider) IsAuthError(error) bool { return false }

// remoteEvent builds one plain remote appointment for the fake provider.
func remoteEvent(id, uid, subject string) Event {
	return Event{
		ID:           id,
		ICalUID:      uid,
		Subject:      subject,
		Location:     "HQ",
		Description:  "agenda",
		Start:        time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC),
		End:          time.Date(2026, 7, 23, 11, 0, 0, 0, time.UTC),
		LastModified: time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC),
		ETag:         "etag-1",
		Type:         "singleInstance",
	}
}

// writeLocalICS serializes ev and writes it as <sanitized UID>.ics in dir.
func writeLocalICS(t *testing.T, dir string, ev Event) string {
	t.Helper()
	data, err := EventToICal(ev)
	if err != nil {
		t.Fatalf("EventToICal: %v", err)
	}
	path := filepath.Join(dir, sanitizeName(ev.ICalUID)+".ics")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write local ics: %v", err)
	}
	return path
}

// syncFake runs one Sync pass against the fake provider.
func syncFake(t *testing.T, f *fakeProvider, dir string, status *CalendarStatus) SyncStats {
	t.Helper()
	stats, err := Sync(context.Background(), f, Calendar{ID: "cal1", Name: "Work"}, dir, status, SyncOptions{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return stats
}

func TestCalendarRenameKeepsStableCollection(t *testing.T) {
	f := &fakeProvider{events: []Event{remoteEvent("g1", "uid-1", "Standup")}, calendarName: "Old name"}
	accountDir := t.TempDir()
	state := &calendarsync.SyncState{Calendars: map[string]CalendarStatus{}}
	plans, err := calendarsync.PlanAll(context.Background(), f, accountDir, nil, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := calendarsync.ApplyAll(context.Background(), f, state, plans, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	oldDir := filepath.Join(accountDir, "Old name")

	f.calendarName = "New name"
	plans, err = calendarsync.PlanAll(context.Background(), f, accountDir, nil, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Dir != oldDir || len(plans[0].Actions) != 0 {
		t.Fatalf("rename plan = %+v, want stable old dir and no event actions", plans)
	}
	if _, err := calendarsync.ApplyAll(context.Background(), f, state, plans, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	name, err := os.ReadFile(filepath.Join(oldDir, "displayname"))
	if err != nil || string(name) != "New name\n" {
		t.Fatalf("displayname = %q err=%v", name, err)
	}
	if _, err := os.Stat(filepath.Join(accountDir, "New name")); !os.IsNotExist(err) {
		t.Fatalf("rename created a second collection: %v", err)
	}
}

func TestSameNameCalendarsUseStableDistinctCollections(t *testing.T) {
	f := &fakeProvider{calendars: []Calendar{{ID: "cal1", Name: "Work"}, {ID: "cal2", Name: "Work"}}}
	accountDir := t.TempDir()
	state := &calendarsync.SyncState{Calendars: map[string]CalendarStatus{}}
	plans, err := calendarsync.PlanAll(context.Background(), f, accountDir, nil, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[0].Dir == plans[1].Dir {
		t.Fatalf("same-name calendar dirs = %+v, want two distinct collections", plans)
	}
	if _, err := calendarsync.ApplyAll(context.Background(), f, state, plans, SyncOptions{}); err != nil {
		t.Fatal(err)
	}

	f.calendars = []Calendar{{ID: "cal2", Name: "Work"}, {ID: "cal1", Name: "Work"}}
	again, err := calendarsync.PlanAll(context.Background(), f, accountDir, nil, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 2 || again[0].Dir != plans[1].Dir || again[1].Dir != plans[0].Dir {
		t.Fatalf("same-name calendar dirs changed: first=%+v second=%+v", plans, again)
	}
}

func TestRecreatedSameNameCalendarDoesNotClaimDeletedCollection(t *testing.T) {
	f := &fakeProvider{
		calendars:        []Calendar{{ID: "old", Name: "Work"}},
		eventsByCalendar: map[string][]Event{"old": {remoteEvent("g1", "uid-1", "Old event")}},
	}
	accountDir := t.TempDir()
	state := &calendarsync.SyncState{Calendars: map[string]CalendarStatus{}}
	plans, err := calendarsync.PlanAll(context.Background(), f, accountDir, nil, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := calendarsync.ApplyAll(context.Background(), f, state, plans, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	oldDir := state.Calendars["old"].Dir

	f.calendars = []Calendar{{ID: "new", Name: "Work"}}
	f.eventsByCalendar = map[string][]Event{"new": {}}
	plans, err = calendarsync.PlanAll(context.Background(), f, accountDir, nil, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("recreate plans = %+v, want live and removed calendar", plans)
	}
	for _, plan := range plans {
		if plan.Calendar.ID == "new" {
			if filepath.Base(plan.Dir) == oldDir {
				t.Fatalf("new calendar claimed deleted collection %q", oldDir)
			}
			for _, action := range plan.Actions {
				if action.Kind == calendarsync.ActionUploadCreate {
					t.Fatalf("old local event would be uploaded into recreated calendar: %+v", action)
				}
			}
		}
	}
}

func TestRemoteCalendarDeletionPrunesUnchangedCollection(t *testing.T) {
	f := &fakeProvider{events: []Event{remoteEvent("g1", "uid-1", "Standup")}}
	accountDir := t.TempDir()
	state := &calendarsync.SyncState{Calendars: map[string]CalendarStatus{}}
	plans, err := calendarsync.PlanAll(context.Background(), f, accountDir, nil, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := calendarsync.ApplyAll(context.Background(), f, state, plans, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	f.noCalendars = true
	plans, err = calendarsync.PlanAll(context.Background(), f, accountDir, nil, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || !plans[0].Removed || plans[0].Actions[0].Kind != calendarsync.ActionPruneLocal {
		t.Fatalf("removed calendar plan = %+v", plans)
	}
	filtered, suppressed := calendarsync.FilterDownloadOnly(plans)
	if suppressed != 0 || !filtered[0].Removed {
		t.Fatalf("filtered removed calendar plan = %+v, suppressed = %d", filtered, suppressed)
	}
	if _, err := calendarsync.ApplyAll(context.Background(), f, state, filtered, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Calendars["cal1"]; ok {
		t.Error("deleted calendar status retained")
	}
	if _, err := os.Stat(filepath.Join(accountDir, "Work")); !os.IsNotExist(err) {
		t.Errorf("unchanged deleted collection retained: %v", err)
	}
}

func TestRemovedCalendarCleanupWaitsAfterStaleLocalFile(t *testing.T) {
	event := remoteEvent("g1", "uid-1", "Standup")
	f := &fakeProvider{events: []Event{event}}
	accountDir := t.TempDir()
	state := &calendarsync.SyncState{Calendars: map[string]CalendarStatus{}}
	plans, err := calendarsync.PlanAll(context.Background(), f, accountDir, nil, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := calendarsync.ApplyAll(context.Background(), f, state, plans, SyncOptions{}); err != nil {
		t.Fatal(err)
	}

	f.noCalendars = true
	plans, err = calendarsync.PlanAll(context.Background(), f, accountDir, nil, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	event.Subject = "Changed after planning"
	writeLocalICS(t, filepath.Join(accountDir, "Work"), event)
	stats, err := calendarsync.ApplyAll(context.Background(), f, state, plans, SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if stats != (SyncStats{Skipped: 1}) {
		t.Fatalf("stats = %+v, want one skipped stale action and no cleanup failure", stats)
	}
	if _, err := os.Stat(filepath.Join(accountDir, "Work")); err != nil {
		t.Fatalf("stale removed collection was cleaned up: %v", err)
	}
}

func TestSyncRemovesLegacyMetadataOnlyOrphanCollection(t *testing.T) {
	f := &fakeProvider{noCalendars: true}
	accountDir := t.TempDir()
	orphanDir := filepath.Join(accountDir, "Deleted calendar")
	if err := os.Mkdir(orphanDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{"displayname": "Deleted calendar\n", "color": "#123456\n"} {
		if err := os.WriteFile(filepath.Join(orphanDir, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	legacyOrphan := "local.ics.orphan-123"
	if err := os.WriteFile(filepath.Join(orphanDir, legacyOrphan), []byte("local edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := &calendarsync.SyncState{Calendars: map[string]CalendarStatus{}}

	plans, err := calendarsync.PlanAll(context.Background(), f, accountDir, nil, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || !plans[0].Removed || plans[0].Dir != orphanDir || len(plans[0].Actions) != 0 {
		t.Fatalf("orphan cleanup plan = %+v", plans)
	}
	if _, err := calendarsync.ApplyAll(context.Background(), f, state, plans, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Calendars[""]; ok {
		t.Fatal("legacy orphan cleanup created an empty calendar state key")
	}
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Fatalf("metadata-only orphan retained: %v", err)
	}
	archived, err := os.ReadFile(filepath.Join(accountDir, ".orphaned", "Deleted calendar", legacyOrphan))
	if err != nil || string(archived) != "local edit" {
		t.Fatalf("recovery file = %q, err %v", archived, err)
	}
}

func TestSyncPreservesUntrackedCalendarDirectoryWithUserData(t *testing.T) {
	f := &fakeProvider{noCalendars: true}
	accountDir := t.TempDir()
	localDir := filepath.Join(accountDir, "Untracked")
	if err := os.Mkdir(localDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "event.ics"), []byte("user data"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := &calendarsync.SyncState{Calendars: map[string]CalendarStatus{}}

	plans, err := calendarsync.PlanAll(context.Background(), f, accountDir, nil, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 0 {
		t.Fatalf("plans = %+v, want no cleanup for user data", plans)
	}
}

func TestSyncPreservesEmptyUntrackedCalendarDirectory(t *testing.T) {
	f := &fakeProvider{noCalendars: true}
	accountDir := t.TempDir()
	localDir := filepath.Join(accountDir, "Empty user calendar")
	if err := os.Mkdir(localDir, 0o700); err != nil {
		t.Fatal(err)
	}
	state := &calendarsync.SyncState{Calendars: map[string]CalendarStatus{}}

	plans, err := calendarsync.PlanAll(context.Background(), f, accountDir, nil, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 0 {
		t.Fatalf("plans = %+v, want no cleanup for empty user calendar", plans)
	}
	if _, err := os.Stat(localDir); err != nil {
		t.Fatalf("empty user calendar was removed: %v", err)
	}
}

func TestRemoteCalendarDeletionArchivesLocalEdit(t *testing.T) {
	f := &fakeProvider{events: []Event{remoteEvent("g1", "uid-1", "Standup")}}
	accountDir := t.TempDir()
	state := &calendarsync.SyncState{Calendars: map[string]CalendarStatus{}}
	plans, _ := calendarsync.PlanAll(context.Background(), f, accountDir, nil, state, nil)
	if _, err := calendarsync.ApplyAll(context.Background(), f, state, plans, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(accountDir, "Work", "uid-1.ics")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte("X-LOCAL-EDIT:yes\r\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	f.noCalendars = true
	plans, err = calendarsync.PlanAll(context.Background(), f, accountDir, nil, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plans[0].Actions[0].Kind != calendarsync.ActionArchiveLocal {
		t.Fatalf("local edit action = %s, want archive-local", plans[0].Actions[0].Kind)
	}
	stats, err := calendarsync.ApplyAll(context.Background(), f, state, plans, SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Archived != 1 {
		t.Fatalf("stats = %+v, want Archived=1", stats)
	}
	matches, _ := filepath.Glob(filepath.Join(accountDir, ".orphaned", "Work", "uid-1.ics.orphan-*"))
	if len(matches) != 1 {
		t.Fatalf("orphan backups = %v, want one", matches)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("scannable edited file retained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(accountDir, "Work")); !os.IsNotExist(err) {
		t.Errorf("deleted calendar collection retained: %v", err)
	}
}

func TestBindMailboxQuarantinesLegacyDelegatedVdir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".durian-calsync-run.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	oldDir := filepath.Join(dir, "Calendar")
	if err := os.Mkdir(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "old.ics"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := &calendarsync.SyncState{Calendars: map[string]CalendarStatus{
		"old": {Items: map[string]ItemStatus{}},
	}}

	if _, _, err := calendarsync.BindMailbox(dir, state, "shared@example.com", true, false); !errors.Is(err, calendarsync.ErrMailboxRebindNeeded) {
		t.Fatalf("dry BindMailbox error = %v", err)
	}
	if _, err := os.Stat(oldDir); err != nil {
		t.Fatalf("dry bind mutated vdir: %v", err)
	}

	fresh, backup, err := calendarsync.BindMailbox(dir, state, "shared@example.com", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Mailbox != "shared@example.com" || len(fresh.Calendars) != 0 {
		t.Fatalf("fresh state = %+v", fresh)
	}
	if _, err := os.Stat(filepath.Join(backup, "Calendar", "old.ics")); err != nil {
		t.Fatalf("legacy event not quarantined: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".durian-calsync-run.lock")); err != nil {
		t.Fatalf("run lock moved out of active vdir: %v", err)
	}
}

func TestBindMailboxQuarantinesLegacyVdirWithEmptyState(t *testing.T) {
	dir := t.TempDir()
	oldDir := filepath.Join(dir, "Calendar")
	if err := os.Mkdir(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "old.ics"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := &calendarsync.SyncState{Calendars: map[string]CalendarStatus{}}

	if _, _, err := calendarsync.BindMailbox(dir, state, "shared@example.com", true, false); !errors.Is(err, calendarsync.ErrMailboxRebindNeeded) {
		t.Fatalf("dry BindMailbox error = %v", err)
	}
	fresh, backup, err := calendarsync.BindMailbox(dir, state, "shared@example.com", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Mailbox != "shared@example.com" || len(fresh.Calendars) != 0 {
		t.Fatalf("fresh state = %+v", fresh)
	}
	if _, err := os.Stat(filepath.Join(backup, "Calendar", "old.ics")); err != nil {
		t.Fatalf("legacy event not quarantined: %v", err)
	}
}

func TestSeamCreateRoleGateAndIdempotencyKey(t *testing.T) {
	base := Event{
		ICalUID: "uid-local",
		Subject: "Kickoff",
		Start:   time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC),
		End:     time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC),
		Attendees: []Attendee{
			{Name: "Alice", Email: "alice@example.com", Type: "required", Response: "none"},
		},
	}

	t.Run("foreign organizer never carries attendees", func(t *testing.T) {
		f := &fakeProvider{createResult: remoteEvent("g-new", "remote-uid", "Kickoff")}
		dir := t.TempDir()
		ev := base
		ev.Organizer = &Person{Name: "Org", Email: "organizer@example.com"}
		writeLocalICS(t, dir, ev)

		status := CalendarStatus{Items: map[string]ItemStatus{}}
		if stats := syncFake(t, f, dir, &status); stats != (SyncStats{Uploaded: 1}) {
			t.Fatalf("stats = %+v, want Uploaded=1 only", stats)
		}
		if len(f.createOpts) != 1 {
			t.Fatalf("createOpts = %d, want exactly one create", len(f.createOpts))
		}
		if f.createOpts[0].IncludeAttendees {
			t.Error("foreign-organizer create must not ask for attendees (role gate)")
		}
		if f.createOpts[0].IdempotencyKey == "" {
			t.Error("create must carry an idempotency key (R1)")
		}
	})

	t.Run("owner organizer includes attendees", func(t *testing.T) {
		f := &fakeProvider{createResult: remoteEvent("g-new", "remote-uid", "Kickoff")}
		dir := t.TempDir()
		writeLocalICS(t, dir, base) // no ORGANIZER: the owner creates

		status := CalendarStatus{Items: map[string]ItemStatus{}}
		if stats := syncFake(t, f, dir, &status); stats != (SyncStats{Uploaded: 1}) {
			t.Fatalf("stats = %+v, want Uploaded=1 only", stats)
		}
		if len(f.createOpts) != 1 || !f.createOpts[0].IncludeAttendees {
			t.Errorf("owner-organized meeting create must ask for attendees: %+v", f.createOpts)
		}
	})
}

func TestSeamPreconditionFailureSkips(t *testing.T) {
	f := &fakeProvider{events: []Event{remoteEvent("g1", "uid-1", "Standup")}}
	dir := t.TempDir()
	status := CalendarStatus{Items: map[string]ItemStatus{}}
	if stats := syncFake(t, f, dir, &status); stats != (SyncStats{Downloaded: 1}) {
		t.Fatalf("seed stats = %+v, want Downloaded=1 only", stats)
	}
	baseline := status.Items["uid-1"]

	// Local edit + a provider whose conditional update reports the remote
	// changed since planning.
	edited := remoteEvent("", "uid-1", "Standup edited")
	writeLocalICS(t, dir, edited)
	f.updateErr = fmt.Errorf("simulated etag mismatch: %w", calendarsync.ErrPrecondition)

	stats := syncFake(t, f, dir, &status)
	if stats != (SyncStats{Skipped: 1}) {
		t.Fatalf("stats = %+v, want Skipped=1 only (R2: no clobber, no failure)", stats)
	}
	if status.Items["uid-1"] != baseline {
		t.Error("a precondition failure must not move the status baseline")
	}
	spec, ok := f.updateSpecs["g1"]
	if !ok {
		t.Fatal("UpdateEvent was not called for g1")
	}
	if spec.ETag != "etag-1" {
		t.Errorf("UpdateSpec.ETag = %q, want the planned etag etag-1", spec.ETag)
	}
	if spec.IncludeAttendees || spec.AttendeesOnly {
		t.Errorf("plain content edit must not touch attendees: %+v", spec)
	}
}

func TestSeamDeleteNotFoundIsSuccess(t *testing.T) {
	f := &fakeProvider{events: []Event{remoteEvent("g1", "uid-1", "Standup")}}
	dir := t.TempDir()
	status := CalendarStatus{Items: map[string]ItemStatus{}}
	if stats := syncFake(t, f, dir, &status); stats != (SyncStats{Downloaded: 1}) {
		t.Fatalf("seed stats = %+v, want Downloaded=1 only", stats)
	}

	// The user deletes the local file; the provider reports the remote event
	// already gone — the engine folds that into success.
	if err := os.Remove(filepath.Join(dir, "uid-1.ics")); err != nil {
		t.Fatal(err)
	}
	f.deleteErr = fmt.Errorf("already gone: %w", calendarsync.ErrNotFound)

	stats := syncFake(t, f, dir, &status)
	if stats != (SyncStats{DeletedRemote: 1}) {
		t.Fatalf("stats = %+v, want DeletedRemote=1 only (404 folded into success)", stats)
	}
	if len(f.deletes) != 1 || f.deletes[0] != "g1" {
		t.Errorf("deletes = %v, want [g1]", f.deletes)
	}
	if f.deleteETags["g1"] != "etag-1" {
		t.Errorf("delete etag = %q, want the planned etag etag-1", f.deleteETags["g1"])
	}
	if _, ok := status.Items["uid-1"]; ok {
		t.Error("status entry must be dropped after the folded delete")
	}
}

// MARK: - Notification intent across the seam

// ownedMeeting is remoteEvent plus the owner as organizer and one other
// attendee — the shape every notifying write needs.
func ownedMeeting(id, uid, subject string) Event {
	ev := remoteEvent(id, uid, subject)
	ev.Organizer = &Person{Email: testOwnerEmail}
	// Type must be set as both real providers set it: the attendee baseline
	// hashes "email|type", so an empty type here would survive the iCal
	// round-trip as "required" and fake an attendee change on every edit.
	ev.Attendees = []Attendee{
		{Email: testOwnerEmail, Type: "required", Response: "accepted"},
		{Email: "guest@example.com", Type: "required", Response: "needsAction"},
	}
	return ev
}

// TestSeamRescheduleStatesNotifyIntent is the regression for the leak that
// made Google silent: the notification used to be derived from the attendee
// upload gate, so moving a meeting — which changes no attendee — told the
// provider not to notify, and the guests never learned it moved. Graph hides
// this (it notifies on its own), Google does not.
func TestSeamRescheduleStatesNotifyIntent(t *testing.T) {
	f := &fakeProvider{events: []Event{ownedMeeting("g1", "uid-1", "Standup")}}
	dir := t.TempDir()
	status := CalendarStatus{Items: map[string]ItemStatus{}}
	if stats := syncFake(t, f, dir, &status); stats != (SyncStats{Downloaded: 1}) {
		t.Fatalf("seed stats = %+v, want Downloaded=1 only", stats)
	}

	// Move the meeting an hour later by editing the downloaded file, so the
	// attendee set is byte-identical to what the sync last saw.
	path := filepath.Join(dir, "uid-1.ics")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read downloaded ics: %v", err)
	}
	ev, err := ICalToEvent(data, testOwnerEmail)
	if err != nil {
		t.Fatalf("parse downloaded ics: %v", err)
	}
	ev.Start = ev.Start.Add(time.Hour)
	ev.End = ev.End.Add(time.Hour)
	writeLocalICS(t, dir, ev)

	if stats := syncFake(t, f, dir, &status); stats != (SyncStats{Uploaded: 1}) {
		t.Fatalf("stats = %+v, want Uploaded=1 only", stats)
	}
	spec, ok := f.updateSpecs["g1"]
	if !ok {
		t.Fatal("UpdateEvent was not called for g1")
	}
	if !spec.NotifyAttendees {
		t.Error("a reschedule of an owned meeting must state NotifyAttendees, or Google stays silent")
	}
	if spec.IncludeAttendees {
		t.Error("the attendee set did not change; it must not be uploaded")
	}
}

// TestSeamCancelStatesNotifyIntent covers the delete side: the organizer's
// delete cancels the meeting for everyone, so the cancellation mail must be
// asked for explicitly.
func TestSeamCancelStatesNotifyIntent(t *testing.T) {
	f := &fakeProvider{events: []Event{ownedMeeting("g1", "uid-1", "Standup")}}
	dir := t.TempDir()
	status := CalendarStatus{Items: map[string]ItemStatus{}}
	if stats := syncFake(t, f, dir, &status); stats != (SyncStats{Downloaded: 1}) {
		t.Fatalf("seed stats = %+v, want Downloaded=1 only", stats)
	}
	if err := os.Remove(filepath.Join(dir, "uid-1.ics")); err != nil {
		t.Fatal(err)
	}

	if stats := syncFake(t, f, dir, &status); stats != (SyncStats{DeletedRemote: 1}) {
		t.Fatalf("stats = %+v, want DeletedRemote=1 only", stats)
	}
	if !f.deleteNotify["g1"] {
		t.Error("cancelling an owned meeting must state notify, or the guests are never told")
	}
}

// TestSeamPlainAppointmentIsSilent is the other half: an attendee-less
// appointment notifies nobody. The autosync safe-upload gate depends on this
// staying false.
func TestSeamPlainAppointmentIsSilent(t *testing.T) {
	f := &fakeProvider{events: []Event{remoteEvent("g1", "uid-1", "Focus time")}}
	dir := t.TempDir()
	status := CalendarStatus{Items: map[string]ItemStatus{}}
	if stats := syncFake(t, f, dir, &status); stats != (SyncStats{Downloaded: 1}) {
		t.Fatalf("seed stats = %+v, want Downloaded=1 only", stats)
	}

	edited := remoteEvent("", "uid-1", "Focus time (moved)")
	writeLocalICS(t, dir, edited)
	if stats := syncFake(t, f, dir, &status); stats != (SyncStats{Uploaded: 1}) {
		t.Fatalf("stats = %+v, want Uploaded=1 only", stats)
	}
	if f.updateSpecs["g1"].NotifyAttendees {
		t.Error("an attendee-less appointment must never notify")
	}
}
