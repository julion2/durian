// Safe-autosync tests: FilterNonNotifying must keep everything
// FilterDownloadOnly keeps plus ONLY the provably non-notifying uploads —
// never a conflict, never a remote delete (attendees or not), never anything
// for which Action.Notifies() is true — and applying a filtered plan against
// a provider must never reach DeleteEvent/RespondToEvent nor carry
// IncludeAttendees on any create/update. Plus the two safety prerequisites:
// the upload-update recipient count is the local∪remote attendee union (the
// add-first-attendee hole), and a first-sight overwrite backs up the
// divergent local file.

package calendarsync_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/julion2/durian/cli/internal/calendarsync"
)

// MARK: - FilterNonNotifying partition

func TestFilterNonNotifyingPartition(t *testing.T) {
	alice := Attendee{Name: "Alice", Email: "alice@example.com", Type: "required", Response: "none"}
	withAtt := Event{Attendees: []Attendee{alice}}

	// Every ActionKind x attendee shape. wantNotifies is the expected
	// Notifies() verdict, wantKept whether FilterNonNotifying keeps it.
	cases := []struct {
		action       Action
		wantNotifies bool
		wantKept     bool
	}{
		{Action{Kind: ActionDownloadNew, UID: "d1"}, false, true},
		{Action{Kind: ActionDownloadUpdate, UID: "d2"}, false, true},
		{Action{Kind: ActionAdopt, UID: "a1"}, false, true},
		{Action{Kind: ActionPruneLocal, UID: "p1"}, false, true},
		{Action{Kind: ActionDropStatus, UID: "s1"}, false, true},
		// Attendee-less create/update: the non-notifying uploads safe mode
		// may auto-apply.
		{Action{Kind: ActionUploadCreate, UID: "u1", OwnerIsOrganizer: true}, false, true},
		{Action{Kind: ActionUploadUpdate, UID: "u2", OwnerIsOrganizer: true, RemoteExists: true}, false, true},
		// Create with an attendee: an invitation wave.
		{Action{Kind: ActionUploadCreate, UID: "n1", OwnerIsOrganizer: true, LocalEvent: withAtt, Recipients: 1}, true, false},
		// Update adding the FIRST attendee: local has one, remote has none —
		// the union recipient count (Fix A) must catch it.
		{Action{Kind: ActionUploadUpdate, UID: "n2", OwnerIsOrganizer: true, RemoteExists: true, LocalEvent: withAtt, Recipients: 1}, true, false},
		// Update removing ALL attendees: remote still has one — the removed
		// attendee is notified.
		{Action{Kind: ActionUploadUpdate, UID: "n3", OwnerIsOrganizer: true, RemoteExists: true, Remote: withAtt, Recipients: 1}, true, false},
		// Deletes are excluded ENTIRELY — the attendee-less one does not
		// notify but is still never automated (mass-delete safety).
		{Action{Kind: ActionDeleteRemote, UID: "r1", RemoteExists: true}, false, false},
		{Action{Kind: ActionDeleteRemote, UID: "r2", RemoteExists: true, Remote: withAtt, Recipients: 1}, true, false},
		{Action{Kind: ActionConflict, UID: "c1"}, true, false},
		{Action{Kind: ActionRsvp, UID: "v1", RsvpCall: true, Rsvp: OwnerRespAccepted}, true, false},
		// Rebaseline-only RSVP: status-only, kept.
		{Action{Kind: ActionRsvp, UID: "v2", RsvpCall: false, Rsvp: OwnerRespDeclined}, false, true},
	}

	var all []Action
	var wantKept []string
	wantAutoUploads, wantDeferred := 0, 0
	for _, c := range cases {
		if got := c.action.Notifies(); got != c.wantNotifies {
			t.Errorf("Notifies(%s %s) = %v, want %v", c.action.Kind, c.action.UID, got, c.wantNotifies)
		}
		all = append(all, c.action)
		if c.wantKept {
			wantKept = append(wantKept, c.action.UID)
			if c.action.RemoteMutation() {
				wantAutoUploads++
			}
		} else {
			wantDeferred++
		}
	}

	plans := []CalendarPlan{{
		Calendar: Calendar{ID: "cal1", Name: "Work"},
		Dir:      "/tmp/vdir/work",
		Actions:  append([]Action(nil), all...),
	}}

	filtered, autoUploads, deferred := calendarsync.FilterNonNotifying(plans)

	if autoUploads != wantAutoUploads {
		t.Errorf("autoUploads = %d, want %d (attendee-less create + update)", autoUploads, wantAutoUploads)
	}
	if deferred != wantDeferred {
		t.Errorf("deferred = %d, want %d (notifying uploads, ALL deletes, conflict, rsvp-call)", deferred, wantDeferred)
	}
	if len(filtered) != 1 {
		t.Fatalf("filtered plans = %d, want 1", len(filtered))
	}
	if filtered[0].Calendar != plans[0].Calendar || filtered[0].Dir != plans[0].Dir {
		t.Errorf("filtered plan must keep calendar and dir: %+v", filtered[0])
	}

	if len(filtered[0].Actions) != len(wantKept) {
		t.Fatalf("kept %d actions, want %d: %+v", len(filtered[0].Actions), len(wantKept), filtered[0].Actions)
	}
	for i, a := range filtered[0].Actions {
		if a.UID != wantKept[i] {
			t.Errorf("kept action %d = %s, want %s (order must be preserved)", i, a.UID, wantKept[i])
		}
		// The hard invariants: nothing surviving the filter may notify,
		// delete remotely, or resolve a conflict.
		if a.Notifies() {
			t.Errorf("filtered plan contains a notifying action: %s %s", a.Kind, a.UID)
		}
		if a.Kind == ActionDeleteRemote || a.Kind == ActionConflict {
			t.Errorf("filtered plan contains a %s action: %s", a.Kind, a.UID)
		}
	}

	// The input must not be mutated.
	if len(plans[0].Actions) != len(all) {
		t.Errorf("input plan mutated: %d actions left, want %d", len(plans[0].Actions), len(all))
	}
}

// MARK: - Safe-filtered apply never notifies, never deletes

// TestApplySafeFilteredPlanNeverNotifies builds a real diff whose plan
// contains a notifying create (attendees), a notifying update (first attendee
// added), deletes with AND without attendees, an rsvp-with-call and a
// conflict — alongside a non-notifying create and update — then applies the
// FilterNonNotifying'd plan and asserts the provider never sees a delete, an
// RSVP, or IncludeAttendees=true, while the safe uploads DID go through with
// IncludeAttendees=false.
func TestApplySafeFilteredPlanNeverNotifies(t *testing.T) {
	ctx := context.Background()
	owner := testOwnerEmail
	alice := Attendee{Name: "Alice", Email: "alice@example.com", Type: "required", Response: "none"}

	// Remote seed: three plain appointments (one to gain its first attendee,
	// one to be edited, one to be deleted), one for a conflict, and two
	// meetings the owner merely attends (one to be deleted -> decline route,
	// one for the RSVP flip).
	solo := remoteEvent("g-solo", "uid-solo", "Solo")
	solo.IsOrganizer = true
	edit := remoteEvent("g-edit", "uid-edit", "Standup")
	del := remoteEvent("g-del", "uid-del", "Errand")
	conf := remoteEvent("g-conf", "uid-conf", "Planning")
	meetDel := remoteEvent("g-meetdel", "uid-meetdel", "Design review")
	meetDel.Organizer = &Person{Name: "Olivia", Email: "olivia@example.com"}
	meetDel.Attendees = []Attendee{
		{Name: "Olivia", Email: "olivia@example.com", Type: "required", Response: "accepted"},
		{Name: "Me", Email: owner, Type: "required", Response: "accepted"},
	}
	meet := remoteEvent("g-meet", "uid-meet", "Retro")
	meet.Organizer = &Person{Name: "Olivia", Email: "olivia@example.com"}
	meet.Attendees = []Attendee{
		{Name: "Olivia", Email: "olivia@example.com", Type: "required", Response: "accepted"},
		{Name: "Me", Email: owner, Type: "required", Response: "none"},
	}

	f := &fakeProvider{
		events:       []Event{solo, edit, del, conf, meetDel, meet},
		createResult: remoteEvent("g-new", "uid-new", "Local only"),
	}
	accountDir := t.TempDir()
	state := &calendarsync.SyncState{}
	if stats, err := calendarsync.SyncAll(ctx, f, accountDir, nil, state, SyncOptions{}); err != nil {
		t.Fatalf("seed SyncAll: %v", err)
	} else if stats != (SyncStats{Downloaded: 6}) {
		t.Fatalf("seed stats = %+v, want Downloaded=6 only", stats)
	}
	calDir := filepath.Join(accountDir, "Work")

	// Local changes. Notifying/deferred: uid-solo gains its FIRST attendee,
	// a new meeting file with an attendee, both deletes, the RSVP flip, the
	// conflict. Safe/auto: a subject edit and a new attendee-less file.
	soloEdited := solo
	soloEdited.Attendees = []Attendee{alice}
	writeLocalICS(t, calDir, soloEdited)
	createMeet := remoteEvent("", "uid-create-meet", "Kickoff")
	createMeet.Attendees = []Attendee{alice}
	writeLocalICS(t, calDir, createMeet)
	for _, uid := range []string{"uid-del", "uid-meetdel"} {
		if err := os.Remove(filepath.Join(calDir, sanitizeName(uid)+".ics")); err != nil {
			t.Fatal(err)
		}
	}
	accepted := meet
	accepted.Attendees = []Attendee{
		{Name: "Olivia", Email: "olivia@example.com", Type: "required", Response: "accepted"},
		{Name: "Me", Email: owner, Type: "required", Response: "accepted"},
	}
	accepted.OwnerResponse = OwnerRespAccepted
	writeLocalICS(t, calDir, accepted)
	confLocal := conf
	confLocal.Subject = "Planning (local edit)"
	writeLocalICS(t, calDir, confLocal)
	edited := edit
	edited.Subject = "Standup edited"
	writeLocalICS(t, calDir, edited)
	writeLocalICS(t, calDir, remoteEvent("", "uid-create-plain", "Local only"))
	// The conflict's remote side changes too.
	confRemote := conf
	confRemote.Subject = "Planning (remote edit)"
	f.events = []Event{solo, edit, del, confRemote, meetDel, meet}

	plans, err := calendarsync.PlanAll(ctx, f, accountDir, nil, state)
	if err != nil {
		t.Fatalf("PlanAll: %v", err)
	}

	// Sanity: the unfiltered plan really contains every dangerous kind — the
	// "provider never notified" assertions below would be vacuous otherwise.
	kinds := map[ActionKind]int{}
	rsvpCalls, notifying := 0, 0
	for _, p := range plans {
		for _, a := range p.Actions {
			kinds[a.Kind]++
			if a.Kind == ActionRsvp && a.RsvpCall {
				rsvpCalls++
			}
			if a.Notifies() {
				notifying++
			}
			if a.UID == "uid-solo" {
				// The Fix-A regression inside the full engine: the union
				// recipient count catches the first locally added attendee.
				if a.Kind != ActionUploadUpdate || a.Recipients != 1 || !a.Notifies() {
					t.Errorf("add-first-attendee action = kind %s recipients %d notifies %v, want upload-update/1/true",
						a.Kind, a.Recipients, a.Notifies())
				}
			}
		}
	}
	if kinds[ActionUploadCreate] != 2 || kinds[ActionUploadUpdate] != 2 ||
		kinds[ActionDeleteRemote] != 2 || kinds[ActionConflict] != 1 || rsvpCalls != 1 {
		t.Fatalf("scenario incomplete: kinds=%v rsvpCalls=%d", kinds, rsvpCalls)
	}
	if notifying != 5 {
		t.Fatalf("notifying actions = %d, want 5 (meeting create, attendee update, meeting delete, conflict, rsvp-call)", notifying)
	}

	filtered, autoUploads, deferred := calendarsync.FilterNonNotifying(plans)
	if autoUploads != 2 || deferred != 6 {
		t.Errorf("autoUploads=%d deferred=%d, want 2/6", autoUploads, deferred)
	}

	stats, err := calendarsync.ApplyAll(ctx, f, state, filtered, SyncOptions{})
	if err != nil {
		t.Fatalf("ApplyAll(filtered): %v", err)
	}

	// The absolute constraints: no RSVP call, no remote delete, and no
	// create/update that carries attendees.
	if len(f.responds) != 0 {
		t.Errorf("RespondToEvent was called for %v — safe apply must never RSVP", f.responds)
	}
	if len(f.deletes) != 0 {
		t.Errorf("DeleteEvent was called for %v — safe apply must never delete", f.deletes)
	}
	for i, opts := range f.createOpts {
		if opts.IncludeAttendees {
			t.Errorf("createOpts[%d].IncludeAttendees = true — safe apply must never invite", i)
		}
	}
	for id, spec := range f.updateSpecs {
		if spec.IncludeAttendees || spec.AttendeesOnly {
			t.Errorf("updateSpecs[%s] carries attendees (%+v) — safe apply must never invite", id, spec)
		}
	}

	// The safe uploads DID happen: exactly one create and one update.
	if len(f.createOpts) != 1 || len(f.createEvents) != 1 {
		t.Fatalf("CreateEvent calls = %d, want exactly the attendee-less create", len(f.createOpts))
	}
	if len(f.createEvents[0].Attendees) != 0 {
		t.Errorf("created event carries attendees: %+v", f.createEvents[0].Attendees)
	}
	if len(f.updateSpecs) != 1 {
		t.Fatalf("UpdateEvent calls = %v, want exactly the subject edit", f.updateSpecs)
	}
	if spec, ok := f.updateSpecs["g-edit"]; !ok || spec.Event.Subject != "Standup edited" {
		t.Errorf("updateSpecs = %+v, want the g-edit subject edit", f.updateSpecs)
	}
	if stats.Uploaded != 2 || stats.DeletedRemote != 0 || stats.Conflicts != 0 || stats.Rsvps != 0 {
		t.Errorf("stats = %+v, want Uploaded=2 and zero deletes/conflicts/rsvps", stats)
	}
}

// MARK: - Fix A: add-first-attendee must surface in the notification preview

// TestAddFirstAttendeeReportsInviteInPreview is the regression test for the
// upload-update recipient hole: a tracked personal event (no attendees
// anywhere) gains its first attendee locally. The remote side alone counts 0
// recipients, but the PATCH will upload the local attendee and the provider
// WILL invite them — the plan and the interactive preview must say so.
func TestAddFirstAttendeeReportsInviteInPreview(t *testing.T) {
	ctx := context.Background()
	solo := remoteEvent("g1", "uid-1", "Solo")
	solo.IsOrganizer = true
	f := &fakeProvider{events: []Event{solo}}
	accountDir := t.TempDir()
	state := &calendarsync.SyncState{}
	if _, err := calendarsync.SyncAll(ctx, f, accountDir, nil, state, SyncOptions{}); err != nil {
		t.Fatalf("seed SyncAll: %v", err)
	}

	withAtt := solo
	withAtt.Attendees = []Attendee{{Name: "Alice", Email: "alice@example.com", Type: "required", Response: "none"}}
	writeLocalICS(t, filepath.Join(accountDir, "Work"), withAtt)

	plans, err := calendarsync.PlanAll(ctx, f, accountDir, nil, state)
	if err != nil {
		t.Fatalf("PlanAll: %v", err)
	}
	var action Action
	found := false
	for _, p := range plans {
		for _, a := range p.Actions {
			if a.UID == "uid-1" {
				action, found = a, true
			}
		}
	}
	if !found || action.Kind != ActionUploadUpdate {
		t.Fatalf("expected an upload-update for uid-1, got found=%v %+v", found, action)
	}
	if action.Recipients != 1 {
		t.Errorf("Recipients = %d, want 1 (the union with the local attendee, not the empty remote set)", action.Recipients)
	}
	if !action.Notifies() || !action.AutoDeferred() {
		t.Errorf("Notifies=%v AutoDeferred=%v, want true/true — safe autosync must defer this", action.Notifies(), action.AutoDeferred())
	}

	notifications := PlanNotifications(plans, "remote", false)
	if len(notifications) != 1 || notifications[0].Category != NotifyUpdate || notifications[0].Recipients != 1 {
		t.Fatalf("preview = %+v, want exactly one UPDATE to 1 recipient", notifications)
	}
}

// MARK: - Fix B: first-sight overwrite backs up the local file

func TestFirstSightOverwriteBacksUpLocalFile(t *testing.T) {
	ctx := context.Background()
	remote := remoteEvent("g1", "uid-1", "Remote version")

	t.Run("divergent local file is backed up", func(t *testing.T) {
		f := &fakeProvider{events: []Event{remote}}
		accountDir := t.TempDir()
		calDir := filepath.Join(accountDir, "Work")
		if err := os.MkdirAll(calDir, 0o700); err != nil {
			t.Fatal(err)
		}
		local := remote
		local.ID = ""
		local.Subject = "Local version"
		path := writeLocalICS(t, calDir, local)

		// No state file: the pair is untracked -> "remote wins on first
		// sight" (the situation after a corrupted-state reset).
		state := &calendarsync.SyncState{}
		stats, err := calendarsync.SyncAll(ctx, f, accountDir, nil, state, SyncOptions{})
		if err != nil {
			t.Fatalf("SyncAll: %v", err)
		}
		if stats != (SyncStats{Downloaded: 1}) {
			t.Fatalf("stats = %+v, want Downloaded=1 only", stats)
		}
		if len(f.createOpts)+len(f.updateSpecs)+len(f.deletes)+len(f.responds) != 0 {
			t.Error("first sight must never write remotely")
		}

		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read local file: %v", err)
		}
		if !strings.Contains(string(body), "SUMMARY:Remote version") {
			t.Errorf("remote content did not win:\n%s", body)
		}
		backups, err := filepath.Glob(path + ".conflict-*")
		if err != nil || len(backups) != 1 {
			t.Fatalf("backups = %v (err=%v), want exactly one .conflict- backup", backups, err)
		}
		backup, err := os.ReadFile(backups[0])
		if err != nil {
			t.Fatalf("read backup: %v", err)
		}
		if !strings.Contains(string(backup), "SUMMARY:Local version") {
			t.Errorf("backup does not carry the divergent local content:\n%s", backup)
		}
	})

	t.Run("identical local file is adopted without backup", func(t *testing.T) {
		f := &fakeProvider{events: []Event{remote}}
		accountDir := t.TempDir()
		calDir := filepath.Join(accountDir, "Work")
		if err := os.MkdirAll(calDir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := writeLocalICS(t, calDir, remote)

		state := &calendarsync.SyncState{}
		stats, err := calendarsync.SyncAll(ctx, f, accountDir, nil, state, SyncOptions{})
		if err != nil {
			t.Fatalf("SyncAll: %v", err)
		}
		if stats != (SyncStats{}) {
			t.Fatalf("stats = %+v, want all zero (adopt)", stats)
		}
		if backups, _ := filepath.Glob(path + ".conflict-*"); len(backups) != 0 {
			t.Errorf("adopt must not create backups: %v", backups)
		}
	})

	t.Run("fresh download has nothing to back up", func(t *testing.T) {
		f := &fakeProvider{events: []Event{remote}}
		accountDir := t.TempDir()

		state := &calendarsync.SyncState{}
		stats, err := calendarsync.SyncAll(ctx, f, accountDir, nil, state, SyncOptions{})
		if err != nil {
			t.Fatalf("SyncAll: %v", err)
		}
		if stats != (SyncStats{Downloaded: 1}) {
			t.Fatalf("stats = %+v, want Downloaded=1 only", stats)
		}
		if backups, _ := filepath.Glob(filepath.Join(accountDir, "Work", "*.conflict-*")); len(backups) != 0 {
			t.Errorf("fresh download must not create backups: %v", backups)
		}
	})
}
