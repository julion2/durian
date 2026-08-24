// Download-only safety tests: FilterDownloadOnly must partition plans exactly
// along Action.RemoteMutation — nothing that writes to the remote calendar
// survives the filter — and applying a filtered plan against a provider must
// never reach Create/Update/Delete/RespondToEvent. Plus the cross-process run
// lock guarding a whole Load -> Plan -> Apply -> Save cycle.

package calendarsync_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/julion2/durian/cli/internal/calendarsync"
)

// MARK: - FilterDownloadOnly partition

func TestFilterDownloadOnlyPartition(t *testing.T) {
	// One action of every ActionKind, including both RSVP variants.
	all := []Action{
		{Kind: ActionDownloadNew, UID: "d1"},
		{Kind: ActionDownloadUpdate, UID: "d2"},
		{Kind: ActionAdopt, UID: "a1"},
		{Kind: ActionPruneLocal, UID: "p1"},
		{Kind: ActionDropStatus, UID: "s1"},
		{Kind: ActionUploadCreate, UID: "u1"},
		{Kind: ActionUploadUpdate, UID: "u2"},
		{Kind: ActionDeleteRemote, UID: "r1"},
		{Kind: ActionConflict, UID: "c1"},
		{Kind: ActionRsvp, UID: "v1", RsvpCall: true, Rsvp: OwnerRespAccepted},
		{Kind: ActionRsvp, UID: "v2", RsvpCall: false, Rsvp: OwnerRespDeclined},
	}
	plans := []CalendarPlan{{
		Calendar: Calendar{ID: "cal1", Name: "Work"},
		Dir:      "/tmp/vdir/work",
		Removed:  true,
		Actions:  append([]Action(nil), all...),
	}}

	filtered, suppressed := calendarsync.FilterDownloadOnly(plans)

	if suppressed != 5 {
		t.Errorf("suppressed = %d, want 5 (upload-create, upload-update, delete-remote, conflict, rsvp-with-call)", suppressed)
	}
	if len(filtered) != 1 {
		t.Fatalf("filtered plans = %d, want 1", len(filtered))
	}
	if filtered[0].Calendar != plans[0].Calendar || filtered[0].Dir != plans[0].Dir {
		t.Errorf("filtered plan must keep calendar and dir: %+v", filtered[0])
	}
	if !filtered[0].Removed {
		t.Error("filtered plan lost removed-calendar marker")
	}

	wantKept := []string{"d1", "d2", "a1", "p1", "s1", "v2"}
	if len(filtered[0].Actions) != len(wantKept) {
		t.Fatalf("kept %d actions, want %d: %+v", len(filtered[0].Actions), len(wantKept), filtered[0].Actions)
	}
	for i, a := range filtered[0].Actions {
		if a.UID != wantKept[i] {
			t.Errorf("kept action %d = %s, want %s (order must be preserved)", i, a.UID, wantKept[i])
		}
		if a.RemoteMutation() {
			t.Errorf("filtered plan contains a remote mutation: %s %s", a.Kind, a.UID)
		}
	}

	// The input must not be mutated.
	if len(plans[0].Actions) != len(all) {
		t.Errorf("input plan mutated: %d actions left, want %d", len(plans[0].Actions), len(all))
	}
}

func TestFilterEventKeepsOnlyTheExplicitEvent(t *testing.T) {
	plans := []CalendarPlan{
		{
			Calendar: Calendar{Name: "Work"},
			Actions: []Action{
				{Kind: ActionUploadUpdate, UID: "chosen"},
				{Kind: ActionDeleteRemote, UID: "other"},
			},
		},
		{
			Calendar: Calendar{Name: "Personal"},
			Removed:  true,
			Actions:  []Action{{Kind: ActionUploadUpdate, UID: "chosen"}},
		},
	}

	filtered, found := calendarsync.FilterEvent(plans, "work", "chosen")
	if !found {
		t.Fatal("FilterEvent did not find the chosen event")
	}
	if len(filtered) != 2 || len(filtered[0].Actions) != 1 || len(filtered[1].Actions) != 0 {
		t.Fatalf("filtered plans = %+v", filtered)
	}
	if got := filtered[0].Actions[0]; got.UID != "chosen" || got.Kind != ActionUploadUpdate {
		t.Fatalf("kept action = %+v", got)
	}
	if filtered[1].Removed {
		t.Error("FilterEvent propagated removed marker to an unmatched calendar")
	}
}

func TestFilterEventPreservesCompleteRemovedCalendarPlan(t *testing.T) {
	plans := []CalendarPlan{{
		Calendar: Calendar{Name: "Deleted"},
		Removed:  true,
		Actions:  []Action{{Kind: calendarsync.ActionArchiveLocal, UID: "chosen"}},
	}}
	filtered, found := calendarsync.FilterEvent(plans, "Deleted", "chosen")
	if !found || len(filtered) != 1 || !filtered[0].Removed {
		t.Fatalf("filtered = %+v, found = %v", filtered, found)
	}
}

func TestFilterEventReportsMissingEvent(t *testing.T) {
	plans := []CalendarPlan{{
		Calendar: Calendar{Name: "Work"},
		Actions:  []Action{{Kind: ActionUploadUpdate, UID: "other"}},
	}}
	filtered, found := calendarsync.FilterEvent(plans, "Work", "missing")
	if found || len(filtered) != 1 || len(filtered[0].Actions) != 0 {
		t.Fatalf("filtered = %+v, found = %v", filtered, found)
	}
}

func TestFilterEventRejectsAmbiguousCalendarName(t *testing.T) {
	plans := []calendarsync.CalendarPlan{
		{Calendar: calendarsync.Calendar{ID: "cal1", Name: "Work"}, Dir: "/tmp/vdir/Work", Actions: []calendarsync.Action{{Kind: calendarsync.ActionUploadUpdate, UID: "chosen"}}},
		{Calendar: calendarsync.Calendar{ID: "cal2", Name: "Work"}, Dir: "/tmp/vdir/Work-abcdef", Actions: []calendarsync.Action{{Kind: calendarsync.ActionUploadUpdate, UID: "chosen"}}},
	}
	filtered, found := calendarsync.FilterEvent(plans, "Work", "chosen")
	if found {
		t.Fatal("FilterEvent accepted an ambiguous display name")
	}
	for _, plan := range filtered {
		if len(plan.Actions) != 0 {
			t.Fatalf("ambiguous filter retained actions: %+v", filtered)
		}
	}

	filtered, found = calendarsync.FilterEvent(plans, "cal2", "chosen")
	if !found || len(filtered[0].Actions) != 0 || len(filtered[1].Actions) != 1 {
		t.Fatalf("stable collection ref filter = %+v, found=%v", filtered, found)
	}
}

// MARK: - Filtered apply never reaches provider writes

// TestApplyFilteredPlanNeverWritesRemote builds a real diff whose plan
// contains every remote-mutating kind (upload-create, upload-update,
// delete-remote, conflict, rsvp-with-call) alongside a plain download, then
// applies the FilterDownloadOnly'd plan and asserts the provider's write
// methods were never called while the download still happened.
func TestApplyFilteredPlanNeverWritesRemote(t *testing.T) {
	ctx := context.Background()
	owner := testOwnerEmail

	// Remote seed: three plain appointments and one meeting the owner merely
	// attends (foreign organizer, owner not yet responded).
	plain := remoteEvent("g-plain", "uid-plain", "Errand")
	edit := remoteEvent("g-edit", "uid-edit", "Standup")
	conf := remoteEvent("g-conf", "uid-conf", "Retro")
	meet := remoteEvent("g-meet", "uid-meet", "Design review")
	meet.Organizer = &Person{Name: "Olivia", Email: "olivia@example.com"}
	meet.Attendees = []Attendee{
		{Name: "Olivia", Email: "olivia@example.com", Type: "required", Response: "accepted"},
		{Name: "Me", Email: owner, Type: "required", Response: "none"},
	}

	f := &fakeProvider{events: []Event{plain, edit, conf, meet}}
	accountDir := t.TempDir()
	state := &calendarsync.SyncState{}

	stats, err := calendarsync.SyncAll(ctx, f, accountDir, nil, state, SyncOptions{})
	if err != nil {
		t.Fatalf("seed SyncAll: %v", err)
	}
	if stats != (SyncStats{Downloaded: 4}) {
		t.Fatalf("seed stats = %+v, want Downloaded=4 only", stats)
	}
	calDir := filepath.Join(accountDir, "Work")

	// Local changes that would each trigger a notifying remote mutation:
	// delete-remote (file removed), upload-update (subject edited),
	// upload-create (new local-only file), rsvp-with-call (owner PARTSTAT
	// flipped to ACCEPTED, nothing else).
	if err := os.Remove(filepath.Join(calDir, sanitizeName("uid-plain")+".ics")); err != nil {
		t.Fatal(err)
	}
	edited := edit
	edited.Subject = "Standup edited"
	writeLocalICS(t, calDir, edited)
	created := remoteEvent("", "uid-create", "Local only")
	writeLocalICS(t, calDir, created)
	accepted := meet
	accepted.Attendees = []Attendee{
		{Name: "Olivia", Email: "olivia@example.com", Type: "required", Response: "accepted"},
		{Name: "Me", Email: owner, Type: "required", Response: "accepted"},
	}
	accepted.OwnerResponse = OwnerRespAccepted
	writeLocalICS(t, calDir, accepted)
	// Conflict: uid-conf changes on both sides. Plus one fresh remote-only
	// event — the download that MUST still happen after filtering.
	confLocal := conf
	confLocal.Subject = "Retro (local edit)"
	writeLocalICS(t, calDir, confLocal)
	confRemote := conf
	confRemote.Subject = "Retro (remote edit)"
	fresh := remoteEvent("g-fresh", "uid-fresh", "Fresh remote event")
	f.events = []Event{plain, edit, confRemote, meet, fresh}

	plans, err := calendarsync.PlanAll(ctx, f, accountDir, nil, state, nil)
	if err != nil {
		t.Fatalf("PlanAll: %v", err)
	}

	// Sanity: the unfiltered plan really contains every mutating kind — the
	// "provider never called" assertion below would be vacuous otherwise.
	kinds := map[ActionKind]int{}
	rsvpCalls := 0
	for _, p := range plans {
		for _, a := range p.Actions {
			kinds[a.Kind]++
			if a.Kind == ActionRsvp && a.RsvpCall {
				rsvpCalls++
			}
		}
	}
	for _, k := range []ActionKind{ActionUploadCreate, ActionUploadUpdate, ActionDeleteRemote, ActionConflict, ActionDownloadNew} {
		if kinds[k] == 0 {
			t.Fatalf("scenario did not produce a %s action: %v", k, kinds)
		}
	}
	if rsvpCalls == 0 {
		t.Fatalf("scenario did not produce an rsvp-with-call action: %v", kinds)
	}

	filtered, suppressed := calendarsync.FilterDownloadOnly(plans)
	if suppressed != 5 {
		t.Errorf("suppressed = %d, want 5", suppressed)
	}

	applyStats, err := calendarsync.ApplyAll(ctx, f, state, filtered, SyncOptions{})
	if err != nil {
		t.Fatalf("ApplyAll(filtered): %v", err)
	}

	// The absolute constraint: no provider write of any kind.
	if len(f.createOpts) != 0 || len(f.createEvents) != 0 {
		t.Errorf("CreateEvent was called %d time(s) — download-only apply must never create", len(f.createOpts))
	}
	if len(f.updateSpecs) != 0 {
		t.Errorf("UpdateEvent was called for %v — download-only apply must never update", f.updateSpecs)
	}
	if len(f.deletes) != 0 {
		t.Errorf("DeleteEvent was called for %v — download-only apply must never delete", f.deletes)
	}
	if len(f.responds) != 0 {
		t.Errorf("RespondToEvent was called for %v — download-only apply must never RSVP", f.responds)
	}

	// The read direction still works: the fresh remote event was downloaded.
	if applyStats.Downloaded != 1 {
		t.Errorf("applyStats = %+v, want exactly the fresh event downloaded", applyStats)
	}
	if applyStats.Uploaded != 0 || applyStats.DeletedRemote != 0 || applyStats.Conflicts != 0 || applyStats.Rsvps != 0 {
		t.Errorf("applyStats = %+v, want zero remote-write stats", applyStats)
	}
	if _, err := os.Stat(filepath.Join(calDir, sanitizeName("uid-fresh")+".ics")); err != nil {
		t.Errorf("fresh remote event was not downloaded: %v", err)
	}
}

// MARK: - Run lock

func TestAcquireRunLockExcludesSecondHolder(t *testing.T) {
	dir := t.TempDir()

	release1, ok, err := calendarsync.AcquireRunLock(dir)
	if err != nil || !ok {
		t.Fatalf("first AcquireRunLock: ok=%v err=%v, want ok=true", ok, err)
	}

	release2, ok, err := calendarsync.AcquireRunLock(dir)
	if err != nil {
		t.Fatalf("second AcquireRunLock errored: %v (want ok=false, nil error)", err)
	}
	if ok {
		release2()
		t.Fatal("second AcquireRunLock succeeded while the first holder is alive")
	}

	if err := release1(); err != nil {
		t.Fatalf("release: %v", err)
	}

	release3, ok, err := calendarsync.AcquireRunLock(dir)
	if err != nil || !ok {
		t.Fatalf("re-acquire after release: ok=%v err=%v, want ok=true", ok, err)
	}
	if err := release3(); err != nil {
		t.Fatalf("release after re-acquire: %v", err)
	}
}
