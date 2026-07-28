// Provider-seam conformance tests: a minimal fake CalendarProvider locks the
// engine's safety rails at the CalendarProvider interface itself, independent
// of any real provider — what the engine passes across the seam (role-gated
// attendee flags, idempotency keys, planned etags) and how it reacts to the
// neutral error sentinels (ErrPrecondition skips, ErrNotFound folds into
// success). Future providers inherit these guarantees for free.

package calendarsync_test

import (
	"context"
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

	// createResult is returned by CreateEvent; the received event/options are
	// recorded per call.
	createResult Event
	createEvents []Event
	createOpts   []calendarsync.CreateOptions

	// updateErr, when set, fails every UpdateEvent; specs are recorded by
	// event id.
	updateErr   error
	updateSpecs map[string]calendarsync.UpdateSpec

	// deleteErr, when set, fails every DeleteEvent; deleted ids and their
	// etags are recorded.
	deleteErr   error
	deletes     []string
	deleteETags map[string]string

	// responds records RespondToEvent calls (event id -> response).
	responds map[string]calendarsync.OwnerResp
}

func (f *fakeProvider) Owner() string { return testOwnerEmail }

func (f *fakeProvider) ListCalendars(context.Context) ([]Calendar, error) {
	return []Calendar{{ID: "cal1", Name: "Work"}}, nil
}

func (f *fakeProvider) FetchMasterEvents(context.Context, string) ([]Event, error) {
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

func (f *fakeProvider) DeleteEvent(_ context.Context, _, eventID, etag string) error {
	if f.deleteETags == nil {
		f.deleteETags = map[string]string{}
	}
	f.deletes = append(f.deletes, eventID)
	f.deleteETags[eventID] = etag
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
