// Remote-mirror tests: folding a round of changes into the last known remote
// state must reproduce exactly what a full read would have returned, because
// the planner cannot tell the two apart — and every rail in twosync.go is
// built on the assumption that the remote map is complete and current.

package calendarsync

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad time %q: %v", s, err)
	}
	return tm
}

// mirrorWith returns a mirror entry holding the given events, keyed by UID.
func mirrorWith(events ...Event) CalendarMirror {
	m := CalendarMirror{Events: make(map[string]Event, len(events))}
	for _, ev := range events {
		m.Events[ev.ICalUID] = ev
	}
	return m
}

func TestApplyDeltaUpsertsAndRemoves(t *testing.T) {
	entry := mirrorWith(
		Event{ID: "id-a", ICalUID: "uid-a", Subject: "A"},
		Event{ID: "id-b", ICalUID: "uid-b", Subject: "B"},
	)
	entry.Cursor = "old-cursor"

	remote := entry.applyDelta(DeltaResult{
		ChangedMasters:   []Event{{ID: "id-a", ICalUID: "uid-a", Subject: "A changed"}},
		RemovedIDs:       []string{"id-b"},
		Cursor:           "new-cursor",
		ParamFingerprint: "fp/1",
	}, "Work")

	if len(remote) != 1 {
		t.Fatalf("remote set = %+v, want only uid-a", remote)
	}
	if remote["uid-a"].Subject != "A changed" {
		t.Errorf("uid-a = %q, want the changed subject", remote["uid-a"].Subject)
	}
	if _, still := remote["uid-b"]; still {
		t.Error("uid-b survived its removal")
	}
	if entry.Cursor != "new-cursor" || entry.ParamFingerprint != "fp/1" {
		t.Errorf("cursor = %q / %q, want new-cursor / fp/1", entry.Cursor, entry.ParamFingerprint)
	}
}

// An unsettled round reported no cursor. Recording one anyway would declare
// the changes it never got to report already seen.
func TestApplyDeltaKeepsCursorWhenRoundDidNotSettle(t *testing.T) {
	entry := mirrorWith(Event{ID: "id-a", ICalUID: "uid-a"})
	entry.Cursor = "old-cursor"
	entry.ParamFingerprint = "fp/1"

	entry.applyDelta(DeltaResult{
		ChangedMasters: []Event{{ID: "id-a", ICalUID: "uid-a", Subject: "changed"}},
		Cursor:         "",
	}, "Work")

	if entry.Cursor != "old-cursor" || entry.ParamFingerprint != "fp/1" {
		t.Errorf("cursor = %q / %q, want it untouched", entry.Cursor, entry.ParamFingerprint)
	}
}

// A reset round reports what EXISTS, never what vanished. Merging it would
// keep every event deleted while the cursor was unusable.
func TestApplyDeltaReplacesOnReset(t *testing.T) {
	entry := mirrorWith(
		Event{ID: "id-a", ICalUID: "uid-a"},
		Event{ID: "id-stale", ICalUID: "uid-stale"},
	)

	remote := entry.applyDelta(DeltaResult{
		ChangedMasters: []Event{{ID: "id-a", ICalUID: "uid-a", Subject: "A"}},
		Cursor:         "fresh",
		Reset:          true,
	}, "Work")

	if len(remote) != 1 {
		t.Fatalf("remote set = %+v, want only the events the reset round reported", remote)
	}
	if _, lingering := remote["uid-stale"]; lingering {
		t.Error("an event deleted while the cursor was dead survived the reset")
	}
}

func TestApplyDeltaFoldsOccurrenceChangeIntoKnownMaster(t *testing.T) {
	entry := mirrorWith(Event{
		ID: "id-series", ICalUID: "uid-series", Subject: "Standup",
		Start: at(t, "2026-08-03T09:00:00Z"), End: at(t, "2026-08-03T10:00:00Z"),
	})

	remote := entry.applyDelta(DeltaResult{
		ChangedOverrides: []OverrideChange{{
			MasterID:     "id-series",
			RecurrenceID: at(t, "2026-08-17T09:00:00Z"),
			Event: Event{
				Subject: "Standup (moved)",
				Start:   at(t, "2026-08-18T14:00:00Z"),
				End:     at(t, "2026-08-18T15:00:00Z"),
			},
		}},
		Cursor: "c1",
	}, "Work")

	master := remote["uid-series"]
	if len(master.Overrides) != 1 {
		t.Fatalf("overrides = %+v, want the moved occurrence", master.Overrides)
	}
	if master.Overrides[0].Subject != "Standup (moved)" {
		t.Errorf("override subject = %q", master.Overrides[0].Subject)
	}
	if !master.Overrides[0].RecurrenceID.Equal(at(t, "2026-08-17T09:00:00Z")) {
		t.Errorf("override recurrence id = %s", master.Overrides[0].RecurrenceID)
	}
}

// A date is either moved or cancelled, never both: cancelling an occurrence
// that currently has an override has to drop that override, or the local
// expansion would render the meeting the server just removed.
func TestApplyDeltaCancellationSupersedesOverride(t *testing.T) {
	recID := at(t, "2026-08-17T09:00:00Z")
	entry := mirrorWith(Event{
		ID: "id-series", ICalUID: "uid-series",
		Overrides: []Event{{RecurrenceID: recID, Subject: "moved"}},
	})

	remote := entry.applyDelta(DeltaResult{
		ChangedOverrides: []OverrideChange{{
			MasterID: "id-series", RecurrenceID: recID, Cancelled: true,
		}},
		Cursor: "c1",
	}, "Work")

	master := remote["uid-series"]
	if len(master.Overrides) != 0 {
		t.Errorf("overrides = %+v, want the override dropped by the cancellation", master.Overrides)
	}
	if len(master.ExceptionDates) != 1 || !master.ExceptionDates[0].Equal(recID) {
		t.Errorf("exception dates = %v, want exactly the cancelled date", master.ExceptionDates)
	}
}

// The mirror is the only place that knows the master; an occurrence change for
// an unknown one has nowhere to go and must not invent a series.
func TestApplyDeltaIgnoresOccurrenceChangeWithoutMaster(t *testing.T) {
	entry := mirrorWith()
	remote := entry.applyDelta(DeltaResult{
		ChangedOverrides: []OverrideChange{{
			MasterID: "id-unknown", RecurrenceID: at(t, "2026-08-17T09:00:00Z"),
		}},
		Cursor: "c1",
	}, "Work")

	if len(remote) != 0 {
		t.Errorf("remote set = %+v, want nothing invented", remote)
	}
}

// A provider reporting a changed master does not necessarily repeat every
// exception of that series. Overwriting wholesale would resurrect months of
// cancelled dates on the next rename.
func TestApplyDeltaKeepsExceptionsAChangedMasterDidNotRepeat(t *testing.T) {
	knownCancel := at(t, "2026-08-10T09:00:00Z")
	knownMove := at(t, "2026-08-17T09:00:00Z")
	entry := mirrorWith(Event{
		ID: "id-series", ICalUID: "uid-series", Subject: "Standup",
		ExceptionDates: []time.Time{knownCancel},
		Overrides:      []Event{{RecurrenceID: knownMove, Subject: "moved"}},
	})

	remote := entry.applyDelta(DeltaResult{
		// The renamed master arrives with no exceptions at all.
		ChangedMasters: []Event{{ID: "id-series", ICalUID: "uid-series", Subject: "Daily sync"}},
		Cursor:         "c1",
	}, "Work")

	master := remote["uid-series"]
	if master.Subject != "Daily sync" {
		t.Errorf("subject = %q, want the renamed one", master.Subject)
	}
	if len(master.ExceptionDates) != 1 || !master.ExceptionDates[0].Equal(knownCancel) {
		t.Errorf("exception dates = %v, want the previously known cancellation kept", master.ExceptionDates)
	}
	if len(master.Overrides) != 1 || !master.Overrides[0].RecurrenceID.Equal(knownMove) {
		t.Errorf("overrides = %+v, want the previously known move kept", master.Overrides)
	}
}

// Newer information about a specific date wins over what the mirror held.
func TestApplyDeltaPrefersFreshExceptionsOverCarriedOver(t *testing.T) {
	recID := at(t, "2026-08-17T09:00:00Z")
	entry := mirrorWith(Event{
		ID: "id-series", ICalUID: "uid-series",
		Overrides: []Event{{RecurrenceID: recID, Subject: "old move"}},
	})

	remote := entry.applyDelta(DeltaResult{
		ChangedMasters: []Event{{
			ID: "id-series", ICalUID: "uid-series",
			Overrides: []Event{{RecurrenceID: recID, Subject: "new move"}},
		}},
		Cursor: "c1",
	}, "Work")

	master := remote["uid-series"]
	if len(master.Overrides) != 1 || master.Overrides[0].Subject != "new move" {
		t.Errorf("overrides = %+v, want the fresh copy to win", master.Overrides)
	}
}

// Delta feeds replay: the same change may show up in consecutive rounds, and
// a removal may arrive for something already gone.
func TestApplyDeltaIsIdempotent(t *testing.T) {
	round := DeltaResult{
		ChangedMasters: []Event{{ID: "id-a", ICalUID: "uid-a", Subject: "A"}},
		RemovedIDs:     []string{"id-gone"},
		Cursor:         "c1",
	}

	entry := mirrorWith()
	first := entry.applyDelta(round, "Work")
	second := entry.applyDelta(round, "Work")

	if len(first) != len(second) || first["uid-a"].Subject != second["uid-a"].Subject {
		t.Errorf("replaying a round changed the result: %+v vs %+v", first, second)
	}
}

func TestApplyDeltaSortsExceptionsDeterministically(t *testing.T) {
	entry := mirrorWith(Event{ID: "id-series", ICalUID: "uid-series"})

	remote := entry.applyDelta(DeltaResult{
		ChangedOverrides: []OverrideChange{
			{MasterID: "id-series", RecurrenceID: at(t, "2026-09-07T09:00:00Z"),
				Event: Event{Subject: "later"}},
			{MasterID: "id-series", RecurrenceID: at(t, "2026-08-17T09:00:00Z"),
				Event: Event{Subject: "earlier"}},
			{MasterID: "id-series", RecurrenceID: at(t, "2026-08-31T09:00:00Z"),
				Cancelled: true},
			{MasterID: "id-series", RecurrenceID: at(t, "2026-08-10T09:00:00Z"),
				Cancelled: true},
		},
		Cursor: "c1",
	}, "Work")

	master := remote["uid-series"]
	if len(master.Overrides) != 2 || master.Overrides[0].Subject != "earlier" {
		t.Errorf("overrides not sorted by recurrence id: %+v", master.Overrides)
	}
	if len(master.ExceptionDates) != 2 ||
		!master.ExceptionDates[0].Equal(at(t, "2026-08-10T09:00:00Z")) {
		t.Errorf("exception dates not sorted: %v", master.ExceptionDates)
	}
}

// MARK: - Store

func TestMirrorStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewFileMirrorStore(dir)

	mirror := newRemoteMirror()
	mirror.Calendars["cal-1"] = CalendarMirror{
		Cursor:           "tok-1",
		ParamFingerprint: "fp/1",
		Events: map[string]Event{
			"uid-a": {ID: "id-a", ICalUID: "uid-a", Subject: "A",
				Start: at(t, "2026-08-03T09:00:00Z"), End: at(t, "2026-08-03T10:00:00Z"),
				ExceptionDates: []time.Time{at(t, "2026-08-10T09:00:00Z")}},
		},
	}
	if err := store.Save(mirror); err != nil {
		t.Fatalf("Save: %v", err)
	}

	back, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	entry := back.Calendars["cal-1"]
	if entry.Cursor != "tok-1" || entry.ParamFingerprint != "fp/1" {
		t.Errorf("cursor = %q / %q", entry.Cursor, entry.ParamFingerprint)
	}
	ev := entry.Events["uid-a"]
	if ev.Subject != "A" || len(ev.ExceptionDates) != 1 {
		t.Errorf("event did not survive the round trip: %+v", ev)
	}
}

func TestMirrorStoreMissingFileIsEmpty(t *testing.T) {
	mirror, err := NewFileMirrorStore(t.TempDir()).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(mirror.Calendars) != 0 {
		t.Errorf("fresh mirror = %+v, want empty", mirror.Calendars)
	}
}

// Nothing in the mirror is irreplaceable, so a corrupt file must degrade to a
// full download rather than failing the sync.
func TestMirrorStoreCorruptFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	store := NewFileMirrorStore(dir)
	path := filepath.Join(dir, ".durian-calsync-mirror.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	mirror, err := store.Load()
	if err != nil {
		t.Fatalf("Load returned an error instead of degrading: %v", err)
	}
	if len(mirror.Calendars) != 0 {
		t.Errorf("mirror = %+v, want empty", mirror.Calendars)
	}
}
