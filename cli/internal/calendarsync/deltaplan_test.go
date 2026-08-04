// End-to-end tests of the incremental download seam: PlanAll driven by a
// DeltaCalendarProvider must produce exactly the actions a full read would
// have produced. The whole design rests on the planner being unable to tell
// the two apart, so that is what these lock down — plus the rule that gives
// the cursor its safety: it is only ever recorded for a round that settled,
// and only the caller decides when to persist it.

package calendarsync_test

import (
	"context"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendarsync"
)

// fakeDeltaProvider is a fakeProvider that also serves a change feed. rounds
// are handed out in order, one per FetchMasterEventsDelta call, so a test can
// script a first full round followed by incremental ones.
type fakeDeltaProvider struct {
	fakeProvider

	rounds []calendarsync.DeltaResult
	// cursors records the cursor the engine passed on each round, which is how
	// the tests observe that a cursor was (or was not) carried forward.
	cursors []string
	// fingerprint is what DeltaParamFingerprint reports; tests change it to
	// simulate the query shape moving underneath a stored cursor.
	fingerprint string
	// fullReads counts FetchMasterEvents calls — the fallback path that must
	// NOT run once the provider offers a feed.
	fullReads int
}

func (f *fakeDeltaProvider) FetchMasterEvents(ctx context.Context, calendarID string) ([]Event, error) {
	f.fullReads++
	return f.fakeProvider.FetchMasterEvents(ctx, calendarID)
}

func (f *fakeDeltaProvider) DeltaParamFingerprint() string {
	if f.fingerprint == "" {
		return "fake/v1"
	}
	return f.fingerprint
}

func (f *fakeDeltaProvider) FetchMasterEventsDelta(_ context.Context, _, cursor string) (calendarsync.DeltaResult, error) {
	f.cursors = append(f.cursors, cursor)
	if len(f.rounds) == 0 {
		return calendarsync.DeltaResult{ParamFingerprint: f.DeltaParamFingerprint()}, nil
	}
	round := f.rounds[0]
	f.rounds = f.rounds[1:]
	if round.ParamFingerprint == "" {
		round.ParamFingerprint = f.DeltaParamFingerprint()
	}
	return round, nil
}

// event builds a minimal remote event.
func event(id, uid, subject string) Event {
	return Event{
		ID: id, ICalUID: uid, Subject: subject,
		Start: time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC),
	}
}

// planKinds renders the planned actions as "uid:kind" for readable asserts.
func planKinds(plans []calendarsync.CalendarPlan) map[string]calendarsync.ActionKind {
	out := map[string]calendarsync.ActionKind{}
	for _, p := range plans {
		for _, a := range p.Actions {
			out[a.UID] = a.Kind
		}
	}
	return out
}

func TestPlanAllUsesTheChangeFeedWhenAvailable(t *testing.T) {
	accountDir := t.TempDir()
	p := &fakeDeltaProvider{rounds: []calendarsync.DeltaResult{{
		ChangedMasters: []Event{event("id-a", "uid-a", "A")},
		Cursor:         "tok-1",
		Reset:          true,
	}}}

	mirror := calendarsync.NewFileMirrorStore(accountDir)
	m, err := mirror.Load()
	if err != nil {
		t.Fatalf("mirror load: %v", err)
	}
	state, err := calendarsync.NewFileStateStore(accountDir).Load()
	if err != nil {
		t.Fatalf("state load: %v", err)
	}

	plans, err := calendarsync.PlanAll(context.Background(), p, accountDir, nil, state, m)
	if err != nil {
		t.Fatalf("PlanAll: %v", err)
	}
	if p.fullReads != 0 {
		t.Errorf("full read ran %d time(s) despite the provider offering a feed", p.fullReads)
	}
	if got := planKinds(plans)["uid-a"]; got != calendarsync.ActionDownloadNew {
		t.Errorf("uid-a = %v, want a new download", got)
	}
}

// The second run must resume from the cursor the first one settled on, and an
// event the feed does not mention must stay in the plan's remote view — this
// is exactly what the mirror exists for.
func TestPlanAllResumesFromTheStoredCursor(t *testing.T) {
	accountDir := t.TempDir()
	p := &fakeDeltaProvider{rounds: []calendarsync.DeltaResult{
		{ChangedMasters: []Event{event("id-a", "uid-a", "A")}, Cursor: "tok-1", Reset: true},
		{ChangedMasters: []Event{event("id-b", "uid-b", "B")}, Cursor: "tok-2"},
	}}

	mirrorStore := calendarsync.NewFileMirrorStore(accountDir)
	stateStore := calendarsync.NewFileStateStore(accountDir)

	m, _ := mirrorStore.Load()
	state, _ := stateStore.Load()
	if _, err := calendarsync.PlanAll(context.Background(), p, accountDir, nil, state, m); err != nil {
		t.Fatalf("first PlanAll: %v", err)
	}
	if err := mirrorStore.Save(m); err != nil {
		t.Fatalf("mirror save: %v", err)
	}

	m2, _ := mirrorStore.Load()
	state2, _ := stateStore.Load()
	plans, err := calendarsync.PlanAll(context.Background(), p, accountDir, nil, state2, m2)
	if err != nil {
		t.Fatalf("second PlanAll: %v", err)
	}

	if len(p.cursors) != 2 || p.cursors[0] != "" || p.cursors[1] != "tok-1" {
		t.Fatalf("cursors = %q, want an empty first round then tok-1", p.cursors)
	}
	kinds := planKinds(plans)
	if kinds["uid-b"] != calendarsync.ActionDownloadNew {
		t.Errorf("uid-b = %v, want a new download", kinds["uid-b"])
	}
	// uid-a was never mentioned by the second round, yet it must still be
	// planned from the mirror rather than read as a remote deletion.
	if kinds["uid-a"] != calendarsync.ActionDownloadNew {
		t.Errorf("uid-a = %v, want it still present via the mirror (not pruned)", kinds["uid-a"])
	}
}

// A changed query shape invalidates the cursor. Google answers a mismatch with
// 400 rather than 410, so noticing it here is the only thing between a working
// sync and one that fails identically on every run.
func TestPlanAllDropsCursorWhenTheQueryShapeChanges(t *testing.T) {
	accountDir := t.TempDir()
	p := &fakeDeltaProvider{
		fingerprint: "fake/v1",
		rounds: []calendarsync.DeltaResult{
			{ChangedMasters: []Event{event("id-a", "uid-a", "A")}, Cursor: "tok-1", Reset: true},
			{ChangedMasters: []Event{event("id-a", "uid-a", "A")}, Cursor: "tok-2", Reset: true},
		},
	}

	mirrorStore := calendarsync.NewFileMirrorStore(accountDir)
	stateStore := calendarsync.NewFileStateStore(accountDir)

	m, _ := mirrorStore.Load()
	state, _ := stateStore.Load()
	if _, err := calendarsync.PlanAll(context.Background(), p, accountDir, nil, state, m); err != nil {
		t.Fatalf("first PlanAll: %v", err)
	}
	if err := mirrorStore.Save(m); err != nil {
		t.Fatalf("mirror save: %v", err)
	}

	p.fingerprint = "fake/v2"
	m2, _ := mirrorStore.Load()
	state2, _ := stateStore.Load()
	if _, err := calendarsync.PlanAll(context.Background(), p, accountDir, nil, state2, m2); err != nil {
		t.Fatalf("second PlanAll: %v", err)
	}

	if len(p.cursors) != 2 || p.cursors[1] != "" {
		t.Errorf("cursors = %q, want the stored one discarded after the shape change", p.cursors)
	}
}

// Passing no mirror disables the feed entirely — what the preview and dry-run
// callers rely on to avoid touching a cursor they will never commit.
func TestPlanAllWithoutMirrorReadsInFull(t *testing.T) {
	accountDir := t.TempDir()
	p := &fakeDeltaProvider{fakeProvider: fakeProvider{events: []Event{event("id-a", "uid-a", "A")}}}

	state, _ := calendarsync.NewFileStateStore(accountDir).Load()
	plans, err := calendarsync.PlanAll(context.Background(), p, accountDir, nil, state, nil)
	if err != nil {
		t.Fatalf("PlanAll: %v", err)
	}
	if p.fullReads != 1 {
		t.Errorf("full reads = %d, want exactly 1", p.fullReads)
	}
	if len(p.cursors) != 0 {
		t.Errorf("the feed was consulted without a mirror: %q", p.cursors)
	}
	if got := planKinds(plans)["uid-a"]; got != calendarsync.ActionDownloadNew {
		t.Errorf("uid-a = %v, want a new download", got)
	}
}

// A round that never settled reported no cursor. Carrying one forward anyway
// would declare the changes it never got to report already seen.
func TestPlanAllDoesNotAdvanceAnUnsettledCursor(t *testing.T) {
	accountDir := t.TempDir()
	p := &fakeDeltaProvider{rounds: []calendarsync.DeltaResult{
		{ChangedMasters: []Event{event("id-a", "uid-a", "A")}, Cursor: "tok-1", Reset: true},
		{ChangedMasters: []Event{event("id-b", "uid-b", "B")}, Cursor: ""},
		{},
	}}

	mirrorStore := calendarsync.NewFileMirrorStore(accountDir)
	stateStore := calendarsync.NewFileStateStore(accountDir)

	for range 3 {
		m, _ := mirrorStore.Load()
		state, _ := stateStore.Load()
		if _, err := calendarsync.PlanAll(context.Background(), p, accountDir, nil, state, m); err != nil {
			t.Fatalf("PlanAll: %v", err)
		}
		if err := mirrorStore.Save(m); err != nil {
			t.Fatalf("mirror save: %v", err)
		}
	}

	// Round 2 did not settle, so round 3 must retry from tok-1 rather than
	// from whatever round 2 half-reported.
	if len(p.cursors) != 3 || p.cursors[1] != "tok-1" || p.cursors[2] != "tok-1" {
		t.Errorf("cursors = %q, want the unsettled round to be retried from tok-1", p.cursors)
	}
}

// A remote deletion reaches the planner as a tombstone, not as an absence —
// the one classification a change feed cannot express by omission.
func TestPlanAllPrunesOnDeltaTombstone(t *testing.T) {
	accountDir := t.TempDir()
	p := &fakeDeltaProvider{rounds: []calendarsync.DeltaResult{
		{ChangedMasters: []Event{event("id-a", "uid-a", "A")}, Cursor: "tok-1", Reset: true},
		{RemovedIDs: []string{"id-a"}, Cursor: "tok-2"},
	}}

	mirrorStore := calendarsync.NewFileMirrorStore(accountDir)
	stateStore := calendarsync.NewFileStateStore(accountDir)

	// First run: download uid-a and converge, so the second run has a baseline
	// to detect the deletion against.
	m, _ := mirrorStore.Load()
	state, _ := stateStore.Load()
	plans, err := calendarsync.PlanAll(context.Background(), p, accountDir, nil, state, m)
	if err != nil {
		t.Fatalf("first PlanAll: %v", err)
	}
	if _, err := calendarsync.ApplyAll(context.Background(), p, state, plans,
		calendarsync.SyncOptions{}); err != nil {
		t.Fatalf("ApplyAll: %v", err)
	}
	if err := stateStore.Save(state); err != nil {
		t.Fatalf("state save: %v", err)
	}
	if err := mirrorStore.Save(m); err != nil {
		t.Fatalf("mirror save: %v", err)
	}

	m2, _ := mirrorStore.Load()
	state2, _ := stateStore.Load()
	plans2, err := calendarsync.PlanAll(context.Background(), p, accountDir, nil, state2, m2)
	if err != nil {
		t.Fatalf("second PlanAll: %v", err)
	}
	if got := planKinds(plans2)["uid-a"]; got != calendarsync.ActionPruneLocal {
		t.Errorf("uid-a = %v, want the local copy pruned after the tombstone", got)
	}
}
