// Core-vs-response change classification: attendee RESPONSES are orthogonal
// to the user-editable core, so a remote RSVP must refresh the local rendering
// without ever turning a concurrent local core edit into a conflict — the
// regression locked here is a local time edit being classified as a conflict
// (and, under the default "remote" policy, silently discarded) just because an
// attendee accepted the meeting remotely. Legacy state files without a
// CoreHash baseline must keep the historical behavior bit for bit until their
// next rebaseline.

package calendarsync_test

import (
	"os"
	"strings"
	"testing"
	"time"
)

// aliceMeeting builds the remote meeting shared by the core-change tests: a
// meeting the owner merely attends (accepted), with Alice's response as given.
func aliceMeeting(changeKey, subject, aliceResp string) map[string]any {
	return meetingMaster("g1", "uid-1", subject, changeKey, false, "accepted",
		att("Me", testOwnerEmail, "required", "accepted"),
		att("Alice", "alice@example.com", "required", aliceResp))
}

// shiftMeetingHour moves the fake meeting one hour later — the remote
// rendering of the local time edit the tests make via rewriteLocal.
func shiftMeetingHour(m map[string]any) map[string]any {
	m["start"] = map[string]string{"dateTime": "2026-07-23T11:00:00.0000000", "timeZone": "UTC"}
	m["end"] = map[string]string{"dateTime": "2026-07-23T12:00:00.0000000", "timeZone": "UTC"}
	return m
}

// shiftLocalHour is the rewriteLocal mutation matching shiftMeetingHour.
func shiftLocalHour(ev *Event) {
	ev.Start = ev.Start.Add(time.Hour)
	ev.End = ev.End.Add(time.Hour)
}

// stripCoreHash blanks every CoreHash in the harness status, simulating a
// state file written before the CoreHash baseline existed.
func stripCoreHash(h *syncHarness) {
	for uid, st := range h.status.Items {
		st.CoreHash = ""
		h.status.Items[uid] = st
	}
}

// aliceResponse parses the local .ics and returns Alice's recorded response.
func aliceResponse(t *testing.T, h *syncHarness) string {
	t.Helper()
	data, err := os.ReadFile(h.icsPath("uid-1"))
	if err != nil {
		t.Fatalf("read local ics: %v", err)
	}
	ev, err := ICalToEvent(data, testOwnerEmail)
	if err != nil {
		t.Fatalf("parse local ics: %v", err)
	}
	for _, a := range ev.Attendees {
		if strings.EqualFold(a.Email, "alice@example.com") {
			return a.Response
		}
	}
	t.Fatal("Alice missing from the local attendee list")
	return ""
}

func TestLocalCoreEditSurvivesRemoteResponseChange(t *testing.T) {
	// THE regression: baseline a meeting, move it an hour locally while Alice
	// accepts remotely (full content hash moves, core hash does not). The plan
	// must be an UploadUpdate — never a conflict that defers and then discards
	// the local edit under the default "remote" policy.
	h := newSyncHarness(t)
	h.events = []map[string]any{aliceMeeting("ck1", "Design sync", "notResponded")}
	h.sync(SyncOptions{})

	h.rewriteLocal("uid-1", shiftLocalHour)
	h.events = []map[string]any{aliceMeeting("ck2", "Design sync", "accepted")}

	plan := h.plan()
	if len(plan.Actions) != 1 || plan.Actions[0].Kind != ActionUploadUpdate {
		t.Fatalf("plan = %+v, want exactly one upload-update (NOT a conflict)", plan.Actions)
	}

	// Applying uploads the time edit; the read-back — which carries Alice's
	// fresh response — becomes the new baseline.
	settled := shiftMeetingHour(aliceMeeting("ck3", "Design sync", "accepted"))
	h.patchResponses = map[string]map[string]any{"g1": {"id": "g1", "changeKey": "ck3"}}
	h.getResponses = map[string]map[string]any{"g1": settled}

	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{Uploaded: 1}) {
		t.Fatalf("stats = %+v, want Uploaded=1 only", stats)
	}
	if h.patchBodies["g1"] == nil {
		t.Fatal("UpdateEvent (PATCH) was not called")
	}
	start, _ := h.patchBodies["g1"]["start"].(map[string]any)
	if start["dateTime"] != "2026-07-23T11:00:00" {
		t.Errorf("PATCH start = %v, want the local time edit 11:00", start)
	}
	// The owner merely attends: the PATCH must never carry attendees.
	if _, ok := h.patchBodies["g1"]["attendees"]; ok {
		t.Error("attendee-side edit must not upload attendees")
	}

	// The read-back absorbed the remote response change: the next sync of the
	// settled event (churned changeKey) is a clean no-op.
	h.events = []map[string]any{shiftMeetingHour(aliceMeeting("ck-churned", "Design sync", "accepted"))}
	if stats := h.sync(SyncOptions{}); stats != (SyncStats{}) {
		t.Fatalf("follow-up stats = %+v, want all zero", stats)
	}
}

func TestRemoteResponseOnlyChangeRefreshesLocal(t *testing.T) {
	// No local edit: a remote response-only change is a plain download
	// refresh — never a conflict — and the local file ends up carrying the
	// fresh PARTSTAT.
	h := newSyncHarness(t)
	h.events = []map[string]any{aliceMeeting("ck1", "Design sync", "notResponded")}
	h.sync(SyncOptions{})

	h.events = []map[string]any{aliceMeeting("ck2", "Design sync", "accepted")}
	plan := h.plan()
	if len(plan.Actions) != 1 || plan.Actions[0].Kind != ActionDownloadUpdate {
		t.Fatalf("plan = %+v, want exactly one download-update", plan.Actions)
	}
	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{Downloaded: 1}) {
		t.Fatalf("stats = %+v, want Downloaded=1 only", stats)
	}
	if got := aliceResponse(t, h); got != "accepted" {
		t.Errorf("local PARTSTAT for Alice = %q, want accepted", got)
	}
	if h.mutations != 0 || h.notifications != 0 {
		t.Errorf("response refresh wrote remotely: mutations=%d notifications=%d",
			h.mutations, h.notifications)
	}
}

func TestBothCoreEditsStillConflict(t *testing.T) {
	// A real conflict is untouched: local time edit + remote subject edit
	// (with a response change on top) still classifies as a conflict, and the
	// default "remote" policy still backs the local file up without any
	// remote write.
	h := newSyncHarness(t)
	h.events = []map[string]any{aliceMeeting("ck1", "Design sync", "notResponded")}
	h.sync(SyncOptions{})

	h.rewriteLocal("uid-1", shiftLocalHour)
	h.events = []map[string]any{aliceMeeting("ck2", "Design sync v2", "accepted")}

	plan := h.plan()
	if len(plan.Actions) != 1 || plan.Actions[0].Kind != ActionConflict {
		t.Fatalf("plan = %+v, want exactly one conflict", plan.Actions)
	}
	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{Conflicts: 1}) {
		t.Fatalf("stats = %+v, want Conflicts=1 only", stats)
	}
	if backups := h.conflictBackups(h.icsPath("uid-1")); len(backups) != 1 {
		t.Errorf("backups = %v, want exactly one .conflict-* file", backups)
	}
	if h.mutations != 0 || h.notifications != 0 {
		t.Errorf("remote-wins conflict wrote remotely: mutations=%d notifications=%d",
			h.mutations, h.notifications)
	}
}

func TestLegacyStateWithoutCoreHash(t *testing.T) {
	t.Run("unchanged calendar plans nothing", func(t *testing.T) {
		// The first post-upgrade sync of an untouched calendar MUST be a
		// strict no-op: zero actions, zero remote writes, zero emails.
		h := newSyncHarness(t)
		h.events = []map[string]any{aliceMeeting("ck1", "Design sync", "notResponded")}
		h.sync(SyncOptions{})
		stripCoreHash(h) // simulate a pre-upgrade state file

		h.events = []map[string]any{aliceMeeting("ck-churned", "Design sync", "notResponded")}
		plan := h.plan()
		if len(plan.Actions) != 0 {
			t.Fatalf("plan = %+v, want zero actions", plan.Actions)
		}
		if stats := h.sync(SyncOptions{}); stats != (SyncStats{}) {
			t.Fatalf("stats = %+v, want all zero", stats)
		}
		if h.mutations != 0 || h.notifications != 0 {
			t.Errorf("post-upgrade no-op sync wrote remotely: mutations=%d notifications=%d",
				h.mutations, h.notifications)
		}
	})

	t.Run("response change plus local edit keeps the historical conflict", func(t *testing.T) {
		// Without a CoreHash baseline the engine cannot prove the remote
		// change is response-only, so the pre-fix classification (conflict)
		// applies unchanged — and the remote-wins rebaseline then records the
		// CoreHash, switching the item to the improved logic.
		h := newSyncHarness(t)
		h.events = []map[string]any{aliceMeeting("ck1", "Design sync", "notResponded")}
		h.sync(SyncOptions{})
		stripCoreHash(h)

		h.rewriteLocal("uid-1", shiftLocalHour)
		h.events = []map[string]any{aliceMeeting("ck2", "Design sync", "accepted")}

		plan := h.plan()
		if len(plan.Actions) != 1 || plan.Actions[0].Kind != ActionConflict {
			t.Fatalf("plan = %+v, want the historical conflict", plan.Actions)
		}
		if stats := h.sync(SyncOptions{}); stats != (SyncStats{Conflicts: 1}) {
			t.Fatalf("stats = %+v, want Conflicts=1 only", stats)
		}
		if st := h.status.Items["uid-1"]; st.CoreHash == "" {
			t.Error("conflict rebaseline must record the CoreHash (migration completes)")
		}
	})
}
