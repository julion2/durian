package calendarsync_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/julion2/durian/cli/internal/calendarsync"
)

// These are state-sequence tests rather than decision-table tests. Each one
// keeps the first client's pending operation alive while a second client or a
// process restart changes what the next reconciliation sees.
func TestAdversarialSecondClientWinsRaceWithPendingLocalUpdate(t *testing.T) {
	for _, tc := range []struct {
		name        string
		writeStatus int
		remoteAfter []map[string]any
		firstStats  SyncStats
	}{
		{
			name:        "second client edits",
			writeStatus: http.StatusPreconditionFailed,
			remoteAfter: []map[string]any{masterEvent("g1", "uid-1", "Second client edit", "ck2")},
			firstStats:  SyncStats{Skipped: 1},
		},
		{
			name:        "second client deletes",
			writeStatus: http.StatusNotFound,
			remoteAfter: nil,
			firstStats:  SyncStats{Failed: 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSyncHarness(t)
			h.events = []map[string]any{masterEvent("g1", "uid-1", "Baseline", "ck1")}
			if stats := h.sync(SyncOptions{}); stats != (SyncStats{Downloaded: 1}) {
				t.Fatalf("seed stats = %+v, want one download", stats)
			}

			// Client one has a pending upload. The plan captures ck1, then the
			// other client changes the event before Apply reaches Graph.
			h.writeLocal("uid-1", "Pending local edit")
			plan := h.plan()
			if len(plan.Actions) != 1 || plan.Actions[0].Kind != ActionUploadUpdate {
				t.Fatalf("pending plan = %+v, want one upload-update", plan.Actions)
			}
			h.events = tc.remoteAfter
			h.patchStatus = tc.writeStatus
			stats, err := Apply(context.Background(), h.client, plan, &h.status, SyncOptions{})
			if err != nil {
				t.Fatalf("Apply stale plan: %v", err)
			}
			if stats != tc.firstStats {
				t.Fatalf("stale-plan stats = %+v, want %+v", stats, tc.firstStats)
			}
			if h.mutations != 1 {
				t.Fatalf("remote mutation attempts = %d, want exactly the one conditional PATCH", h.mutations)
			}
			if h.notifications != 0 {
				t.Fatalf("attendee notifications = %d, want 0 for this attendee-less appointment", h.notifications)
			}

			// A fresh read must reconcile rather than retrying the stale write.
			h.patchStatus = 0
			if stats := h.sync(SyncOptions{}); stats != (SyncStats{Conflicts: 1}) {
				t.Fatalf("fresh reconciliation stats = %+v, want one remote-wins conflict", stats)
			}
			if h.mutations != 1 {
				t.Errorf("fresh reconciliation added a remote mutation: total=%d, want 1", h.mutations)
			}
			if h.notifications != 0 {
				t.Errorf("fresh reconciliation sent %d attendee notifications", h.notifications)
			}

			path := h.icsPath("uid-1")
			backups := h.conflictBackups(path)
			if len(backups) != 1 {
				t.Fatalf("pending local edit backups = %v, want exactly one", backups)
			}
			backup, err := os.ReadFile(backups[0])
			if err != nil || !strings.Contains(string(backup), "SUMMARY:Pending local edit") {
				t.Errorf("pending edit was not preserved in backup: err=%v\n%s", err, backup)
			}

			if tc.remoteAfter == nil {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("local item survived the second-client deletion: err=%v", err)
				}
				if _, ok := h.status.Items["uid-1"]; ok {
					t.Error("deleted pair remains in status")
				}
				return
			}

			body, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(body), "SUMMARY:Second client edit") {
				t.Errorf("local state did not converge on second-client edit: err=%v\n%s", err, body)
			}
			st, ok := h.status.Items["uid-1"]
			if !ok || st.RemoteHash != contentHashOf(t, tc.remoteAfter[0]) || st.LocalHash != hashBytes(body) {
				t.Errorf("final status = %+v (present=%v), want exact remote/local baselines", st, ok)
			}
		})
	}
}

func TestAdversarialOfflineUpdateThenDeleteSurvivesRestart(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Baseline", "ck1")}
	if stats := h.sync(SyncOptions{}); stats != (SyncStats{Downloaded: 1}) {
		t.Fatalf("seed stats = %+v, want one download", stats)
	}

	stateDir := t.TempDir()
	store := NewFileStateStore(stateDir)
	state := &calendarsync.SyncState{Calendars: map[string]CalendarStatus{"cal1": h.status}}
	if err := store.Save(state); err != nil {
		t.Fatalf("save baseline before restart: %v", err)
	}

	// Offline, the user edits and then deletes before either operation can be
	// synced. Only the final absence is observable after process restart.
	h.writeLocal("uid-1", "Offline edit that is then deleted")
	if err := os.Remove(h.icsPath("uid-1")); err != nil {
		t.Fatalf("offline delete: %v", err)
	}
	restarted, err := store.Load()
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	h.status = restarted.Calendars["cal1"]
	if stats := h.sync(SyncOptions{}); stats != (SyncStats{DeletedRemote: 1}) {
		t.Fatalf("post-restart stats = %+v, want one remote delete", stats)
	}
	if h.mutations != 1 || len(h.deletedIDs) != 1 || h.deletedIDs[0] != "g1" {
		t.Errorf("remote mutations=%d deleted=%v, want exactly DELETE g1", h.mutations, h.deletedIDs)
	}
	if h.notifications != 0 {
		t.Errorf("attendee notifications = %d, want 0 for the attendee-less appointment", h.notifications)
	}
	if _, ok := h.status.Items["uid-1"]; ok {
		t.Error("successful delete remains tracked")
	}

	restarted.Calendars["cal1"] = h.status
	if err := store.Save(restarted); err != nil {
		t.Fatalf("save post-delete state: %v", err)
	}
	// Simulate Graph's next read after the successful DELETE, then restart
	// again. Reconciliation must be idempotent and must not issue DELETE #2.
	h.events = nil
	again, err := store.Load()
	if err != nil {
		t.Fatalf("second reload: %v", err)
	}
	h.status = again.Calendars["cal1"]
	if stats := h.sync(SyncOptions{}); stats != (SyncStats{}) {
		t.Fatalf("second restart stats = %+v, want a no-op", stats)
	}
	if h.mutations != 1 || h.notifications != 0 {
		t.Errorf("second restart totals: mutations=%d notifications=%d, want 1/0", h.mutations, h.notifications)
	}
}

type adversarialDeltaProvider struct {
	fakeDeltaProvider
	failOnce       map[string]bool
	updateAttempts map[string]int
	mutations      int
	notifications  int
}

func (p *adversarialDeltaProvider) UpdateEvent(ctx context.Context, calendarID, eventID string, spec calendarsync.UpdateSpec) error {
	p.mutations++
	p.updateAttempts[eventID]++
	if p.failOnce[eventID] {
		delete(p.failOnce, eventID)
		return fmt.Errorf("simulated provider failure after earlier delta actions")
	}
	if spec.NotifyAttendees {
		p.notifications++
	}
	return p.fakeProvider.UpdateEvent(ctx, calendarID, eventID, spec)
}

func TestAdversarialDeltaApplyFailureThenRestartRetryIsIdempotent(t *testing.T) {
	accountDir := t.TempDir()
	p := &adversarialDeltaProvider{
		fakeDeltaProvider: fakeDeltaProvider{rounds: []calendarsync.DeltaResult{{
			ChangedMasters: []Event{event("id-a", "uid-a", "A"), event("id-b", "uid-b", "B")},
			Cursor:         "tok-1",
			Reset:          true,
		}}},
		failOnce:       map[string]bool{"id-b": true},
		updateAttempts: map[string]int{},
	}
	stateStore := NewFileStateStore(accountDir)
	mirrorStore := calendarsync.NewFileMirrorStore(accountDir)

	state, _ := stateStore.Load()
	mirror, _ := mirrorStore.Load()
	plans, err := calendarsync.PlanAll(context.Background(), p, accountDir, nil, state, mirror)
	if err != nil {
		t.Fatalf("seed PlanAll: %v", err)
	}
	if stats, err := calendarsync.ApplyAll(context.Background(), p, state, plans, SyncOptions{}); err != nil || stats != (SyncStats{Downloaded: 2}) {
		t.Fatalf("seed ApplyAll stats=%+v err=%v, want two downloads", stats, err)
	}
	if err := stateStore.Save(state); err != nil {
		t.Fatalf("save seed state: %v", err)
	}
	if err := mirrorStore.Save(mirror); err != nil {
		t.Fatalf("save seed mirror: %v", err)
	}

	calDir := plans[0].Dir
	editedA := event("id-a", "uid-a", "A locally edited")
	editedB := event("id-b", "uid-b", "B locally edited")
	writeLocalICS(t, calDir, editedA)
	writeLocalICS(t, calDir, editedB)
	p.rounds = append(p.rounds, calendarsync.DeltaResult{Cursor: "tok-2"})

	state, _ = stateStore.Load()
	mirror, _ = mirrorStore.Load()
	plans, err = calendarsync.PlanAll(context.Background(), p, accountDir, nil, state, mirror)
	if err != nil {
		t.Fatalf("partial-failure PlanAll: %v", err)
	}
	stats, err := calendarsync.ApplyAll(context.Background(), p, state, plans, SyncOptions{})
	if err != nil {
		t.Fatalf("partial-failure ApplyAll: %v", err)
	}
	if stats != (SyncStats{Uploaded: 1, Failed: 1}) {
		t.Fatalf("partial-failure stats = %+v, want one upload and one retained failure", stats)
	}
	if p.updateAttempts["id-a"] != 1 || p.updateAttempts["id-b"] != 1 {
		t.Fatalf("first attempt counts = %v, want id-a=1 id-b=1", p.updateAttempts)
	}
	if err := stateStore.Save(state); err != nil {
		t.Fatalf("save partial state: %v", err)
	}
	if err := mirrorStore.Save(mirror); err != nil {
		t.Fatalf("save partial mirror: %v", err)
	}

	// The process restarts after persisting both the successful item's state
	// and the consumed delta cursor. The next feed round reports the settled
	// successful write. Only id-b may be retried.
	p.rounds = append(p.rounds, calendarsync.DeltaResult{
		ChangedMasters: []Event{editedA},
		Cursor:         "tok-3",
	})
	state, _ = stateStore.Load()
	mirror, _ = mirrorStore.Load()
	plans, err = calendarsync.PlanAll(context.Background(), p, accountDir, nil, state, mirror)
	if err != nil {
		t.Fatalf("restart PlanAll: %v", err)
	}
	stats, err = calendarsync.ApplyAll(context.Background(), p, state, plans, SyncOptions{})
	if err != nil || stats != (SyncStats{Uploaded: 1}) {
		t.Fatalf("restart ApplyAll stats=%+v err=%v, want only failed item retried", stats, err)
	}
	if p.updateAttempts["id-a"] != 1 || p.updateAttempts["id-b"] != 2 {
		t.Errorf("retry counts = %v, want successful id-a once and failed id-b twice", p.updateAttempts)
	}
	if p.mutations != 3 || p.notifications != 0 {
		t.Errorf("totals: mutations=%d notifications=%d, want 3 attempts and no attendee mail", p.mutations, p.notifications)
	}

	if err := stateStore.Save(state); err != nil {
		t.Fatalf("save retry state: %v", err)
	}
	if err := mirrorStore.Save(mirror); err != nil {
		t.Fatalf("save retry mirror: %v", err)
	}
	p.rounds = append(p.rounds, calendarsync.DeltaResult{ChangedMasters: []Event{editedB}, Cursor: "tok-4"})
	state, _ = stateStore.Load()
	mirror, _ = mirrorStore.Load()
	plans, err = calendarsync.PlanAll(context.Background(), p, accountDir, nil, state, mirror)
	if err != nil {
		t.Fatalf("settled PlanAll: %v", err)
	}
	if got := planKinds(plans); len(got) != 0 {
		t.Fatalf("settled plan = %v, want no actions", got)
	}
	finalStatus := state.Calendars["cal1"]
	for _, expected := range []Event{editedA, editedB} {
		path := filepath.Join(calDir, sanitizeName(expected.ICalUID)+".ics")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read final %s: %v", expected.ICalUID, err)
			continue
		}
		local, err := ICalToEvent(data, testOwnerEmail)
		if err != nil || local.Subject != expected.Subject {
			t.Errorf("final local %s = subject %q err=%v, want %q", expected.ICalUID, local.Subject, err, expected.Subject)
		}
		st, ok := finalStatus.Items[expected.ICalUID]
		if !ok || st.LocalHash != hashBytes(data) || st.RemoteHash != eventContentHash(expected, testOwnerEmail) {
			t.Errorf("final status %s = %+v (present=%v), want exact local/remote hashes", expected.ICalUID, st, ok)
		}
	}
	if p.mutations != 3 || p.notifications != 0 {
		t.Errorf("settled read changed totals: mutations=%d notifications=%d", p.mutations, p.notifications)
	}
}
