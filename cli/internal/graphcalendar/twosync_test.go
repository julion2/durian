package graphcalendar

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// MARK: - Test harness

// masterEvent builds one Graph master-event JSON object for the fake server.
func masterEvent(id, uid, subject, changeKey string) map[string]any {
	return map[string]any{
		"id":                   id,
		"iCalUId":              uid,
		"subject":              subject,
		"start":                map[string]string{"dateTime": "2026-07-23T10:00:00.0000000", "timeZone": "UTC"},
		"end":                  map[string]string{"dateTime": "2026-07-23T11:00:00.0000000", "timeZone": "UTC"},
		"isAllDay":             false,
		"location":             map[string]string{"displayName": "HQ"},
		"body":                 map[string]string{"contentType": "text", "content": "agenda"},
		"bodyPreview":          "agenda",
		"type":                 "singleInstance",
		"changeKey":            changeKey,
		"lastModifiedDateTime": "2026-07-20T09:00:00Z",
	}
}

// contentHashOf decodes one fake master-event JSON object exactly like the
// client does and returns its eventContentHash — the expected RemoteHash
// baseline after syncing against that event.
func contentHashOf(t *testing.T, m map[string]any) string {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal master event: %v", err)
	}
	var ge graphEvent
	if err := json.Unmarshal(data, &ge); err != nil {
		t.Fatalf("unmarshal master event: %v", err)
	}
	ev, ok := eventFromGraph(ge)
	if !ok {
		t.Fatal("eventFromGraph rejected the fake master event")
	}
	return eventContentHash(ev)
}

// syncHarness serves the Graph read and write endpoints of calendar cal1 from
// mutable fakes, recording every mutation, and provides a client plus a temp
// calendar dir for driving Plan/Apply/Sync directly.
type syncHarness struct {
	t      *testing.T
	srv    *httptest.Server
	client *Client
	calDir string
	status CalendarStatus

	// events is what GET /me/calendars/cal1/events returns.
	events []map[string]any

	// getResponses maps event id -> GET /me/events/{id} response (the settled
	// event the post-write read-back sees, possibly with a different changeKey
	// than the create/patch response). A GET for an id without one returns
	// 404, driving the read-back fallback to the write-response changeKey.
	getResponses map[string]map[string]any

	// createResponse is returned by POST create; a POST with none set fails
	// the test (unexpected remote write).
	createResponse map[string]any
	createBodies   []map[string]any
	// patchResponses maps event id -> PATCH response body; a PATCH for an id
	// without one fails the test.
	patchResponses map[string]map[string]any
	patchBodies    map[string]map[string]any
	deletedIDs     []string
	// deleteStatus is the HTTP status for DELETE (default 204).
	deleteStatus int
	// mutations counts every POST/PATCH/DELETE received.
	mutations int
}

func newSyncHarness(t *testing.T) *syncHarness {
	t.Helper()
	h := &syncHarness{
		t:            t,
		calDir:       t.TempDir(),
		status:       CalendarStatus{Items: map[string]ItemStatus{}},
		patchBodies:  map[string]map[string]any{},
		deleteStatus: http.StatusNoContent,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /me/calendars/cal1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"value": h.events}); err != nil {
			t.Errorf("encode events: %v", err)
		}
	})
	mux.HandleFunc("POST /me/calendars/cal1/events", func(w http.ResponseWriter, r *http.Request) {
		h.mutations++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode create body: %v", err)
		}
		h.createBodies = append(h.createBodies, body)
		if h.createResponse == nil {
			t.Error("unexpected POST create (no createResponse configured)")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(h.createResponse); err != nil {
			t.Errorf("encode create response: %v", err)
		}
	})
	mux.HandleFunc("GET /me/events/{id}", func(w http.ResponseWriter, r *http.Request) {
		resp, ok := h.getResponses[r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode get response: %v", err)
		}
	})
	mux.HandleFunc("PATCH /me/events/{id}", func(w http.ResponseWriter, r *http.Request) {
		h.mutations++
		id := r.PathValue("id")
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode patch body: %v", err)
		}
		h.patchBodies[id] = body
		resp, ok := h.patchResponses[id]
		if !ok {
			t.Errorf("unexpected PATCH for %s (no patchResponse configured)", id)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode patch response: %v", err)
		}
	})
	mux.HandleFunc("DELETE /me/events/{id}", func(w http.ResponseWriter, r *http.Request) {
		h.mutations++
		h.deletedIDs = append(h.deletedIDs, r.PathValue("id"))
		w.WriteHeader(h.deleteStatus)
	})

	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	h.client = testClient(h.srv)
	return h
}

// sync runs one Sync (Plan + Apply) pass and fails the test on error.
func (h *syncHarness) sync(opts SyncOptions) SyncStats {
	h.t.Helper()
	stats, err := Sync(context.Background(), h.client, Calendar{ID: "cal1", Name: "Work"}, h.calDir, &h.status, opts)
	if err != nil {
		h.t.Fatalf("Sync: %v", err)
	}
	return stats
}

// plan runs one Plan pass and fails the test on error.
func (h *syncHarness) plan() CalendarPlan {
	h.t.Helper()
	plan, err := Plan(context.Background(), h.client, Calendar{ID: "cal1", Name: "Work"}, h.calDir, h.status)
	if err != nil {
		h.t.Fatalf("Plan: %v", err)
	}
	return plan
}

// icsPath returns the expected UID-derived .ics path.
func (h *syncHarness) icsPath(uid string) string {
	return filepath.Join(h.calDir, sanitizeName(uid)+".ics")
}

// writeLocal writes a valid local .ics with the given UID and subject and
// returns its path.
func (h *syncHarness) writeLocal(uid, subject string) string {
	h.t.Helper()
	data, err := EventToICal(Event{
		ICalUID:      uid,
		Subject:      subject,
		Start:        time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC),
		End:          time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC),
		LastModified: time.Date(2026, 7, 29, 8, 0, 0, 0, time.UTC),
	})
	if err != nil {
		h.t.Fatalf("EventToICal: %v", err)
	}
	path := h.icsPath(uid)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		h.t.Fatalf("write local ics: %v", err)
	}
	return path
}

// conflictBackups returns the .conflict-* backups of the given file.
func (h *syncHarness) conflictBackups(path string) []string {
	h.t.Helper()
	matches, err := filepath.Glob(path + ".conflict-*")
	if err != nil {
		h.t.Fatalf("glob backups: %v", err)
	}
	return matches
}

// MARK: - Download direction

func TestSyncDownloadNewRemote(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck1")}

	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{Downloaded: 1}) {
		t.Fatalf("stats = %+v, want Downloaded=1 only", stats)
	}

	body, err := os.ReadFile(h.icsPath("uid-1"))
	if err != nil {
		t.Fatalf("downloaded .ics missing: %v", err)
	}
	if !strings.Contains(string(body), "SUMMARY:Standup") || !strings.Contains(string(body), "UID:uid-1") {
		t.Errorf("downloaded .ics content wrong:\n%s", body)
	}

	st, ok := h.status.Items["uid-1"]
	if !ok {
		t.Fatal("status entry for uid-1 not recorded")
	}
	if st.GraphID != "g1" || st.RemoteHash != contentHashOf(t, h.events[0]) || st.LocalHash != hashBytes(body) {
		t.Errorf("status = %+v, want GraphID=g1 RemoteHash of remote content LocalHash of file bytes", st)
	}

	// vdir metadata is written like Export.
	if name, err := os.ReadFile(filepath.Join(h.calDir, "displayname")); err != nil || strings.TrimSpace(string(name)) != "Work" {
		t.Errorf("displayname = %q, err=%v, want Work", name, err)
	}
}

func TestSyncUnchangedSecondRunIsNoop(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck1")}
	h.sync(SyncOptions{})

	before, _ := os.ReadFile(h.icsPath("uid-1"))
	// Graph churns the changeKey without any content change; identical content
	// must still be a no-op.
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck-churned")}
	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{}) {
		t.Fatalf("second run stats = %+v, want all zero", stats)
	}
	after, _ := os.ReadFile(h.icsPath("uid-1"))
	if string(before) != string(after) {
		t.Error("second run rewrote the local file")
	}
}

func TestSyncRemoteChangedDownloadsUpdate(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck1")}
	h.sync(SyncOptions{})

	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup v2", "ck2")}
	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{Downloaded: 1}) {
		t.Fatalf("stats = %+v, want Downloaded=1 only", stats)
	}

	body, err := os.ReadFile(h.icsPath("uid-1"))
	if err != nil {
		t.Fatalf("read updated .ics: %v", err)
	}
	if !strings.Contains(string(body), "SUMMARY:Standup v2") {
		t.Errorf("local file not updated:\n%s", body)
	}
	if st := h.status.Items["uid-1"]; st.RemoteHash != contentHashOf(t, h.events[0]) || st.LocalHash != hashBytes(body) {
		t.Errorf("status not updated: %+v", st)
	}
}

func TestSyncRemoteDeletedPrunesLocal(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck1")}
	h.sync(SyncOptions{})

	h.events = nil
	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{Pruned: 1}) {
		t.Fatalf("stats = %+v, want Pruned=1 only", stats)
	}
	if _, err := os.Stat(h.icsPath("uid-1")); !os.IsNotExist(err) {
		t.Errorf("pruned file still exists (err=%v)", err)
	}
	if _, ok := h.status.Items["uid-1"]; ok {
		t.Error("status entry not dropped after prune")
	}
}

func TestSyncUntrackedBothPresent(t *testing.T) {
	// Identical content -> adopted without a rewrite; differing content ->
	// remote wins on first sight.
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck1")}
	h.sync(SyncOptions{})
	canonical, _ := os.ReadFile(h.icsPath("uid-1"))

	// Fresh status simulates a lost/absent state file.
	h.status = CalendarStatus{Items: map[string]ItemStatus{}}
	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{}) {
		t.Fatalf("adopt stats = %+v, want all zero", stats)
	}
	if st, ok := h.status.Items["uid-1"]; !ok || st.GraphID != "g1" || st.RemoteHash != contentHashOf(t, h.events[0]) || st.LocalHash != hashBytes(canonical) {
		t.Errorf("adopt did not record status: %+v ok=%v", st, ok)
	}

	// Diverged local copy, still untracked: remote wins.
	h.status = CalendarStatus{Items: map[string]ItemStatus{}}
	h.writeLocal("uid-1", "Diverged local")
	stats = h.sync(SyncOptions{})
	if stats != (SyncStats{Downloaded: 1}) {
		t.Fatalf("remote-wins stats = %+v, want Downloaded=1 only", stats)
	}
	if body, _ := os.ReadFile(h.icsPath("uid-1")); !strings.Contains(string(body), "SUMMARY:Standup") {
		t.Errorf("remote content did not win:\n%s", body)
	}
	if h.mutations != 0 {
		t.Errorf("download-direction sync performed %d remote mutations", h.mutations)
	}
}

// MARK: - Plan purity

func TestPlanClassifiesWithoutMutating(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck1")}
	localPath := h.writeLocal("uid-local", "Local only")
	before, _ := os.ReadFile(localPath)

	plan := h.plan()
	kinds := map[string]ActionKind{}
	for _, a := range plan.Actions {
		kinds[a.UID] = a.Kind
	}
	if len(plan.Actions) != 2 || kinds["uid-1"] != ActionDownloadNew || kinds["uid-local"] != ActionUploadCreate {
		t.Fatalf("plan actions wrong: %+v", plan.Actions)
	}

	// Planning must not touch anything.
	if len(h.status.Items) != 0 {
		t.Errorf("Plan mutated status: %+v", h.status.Items)
	}
	if _, err := os.Stat(h.icsPath("uid-1")); !os.IsNotExist(err) {
		t.Errorf("Plan wrote a local file (err=%v)", err)
	}
	if after, _ := os.ReadFile(localPath); string(before) != string(after) {
		t.Error("Plan modified the local file")
	}
	if h.mutations != 0 {
		t.Errorf("Plan performed %d remote mutations", h.mutations)
	}
}

// MARK: - Upload direction

func TestSyncUploadCreate(t *testing.T) {
	h := newSyncHarness(t)
	oldPath := h.writeLocal("uid-local", "My event")
	// Graph assigns its own iCalUId on create.
	h.createResponse = masterEvent("g-new", "remote-uid-1", "My event", "ck-new")
	h.getResponses = map[string]map[string]any{"g-new": masterEvent("g-new", "remote-uid-1", "My event", "ck-new")}

	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{Uploaded: 1}) {
		t.Fatalf("stats = %+v, want Uploaded=1 only", stats)
	}

	// The POST body carries the local event as a Graph resource.
	if len(h.createBodies) != 1 {
		t.Fatalf("createBodies = %d, want exactly one POST", len(h.createBodies))
	}
	body := h.createBodies[0]
	if body["subject"] != "My event" {
		t.Errorf("POST subject = %v, want My event", body["subject"])
	}
	start, _ := body["start"].(map[string]any)
	if start["dateTime"] != "2026-07-30T09:00:00" || start["timeZone"] != "UTC" {
		t.Errorf("POST start = %v, want 2026-07-30T09:00:00 UTC", start)
	}
	end, _ := body["end"].(map[string]any)
	if end["dateTime"] != "2026-07-30T10:00:00" || end["timeZone"] != "UTC" {
		t.Errorf("POST end = %v, want 2026-07-30T10:00:00 UTC", end)
	}

	// The local file is rewritten from the created event under the remote UID.
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("pre-create local file still exists (err=%v)", err)
	}
	newBody, err := os.ReadFile(h.icsPath("remote-uid-1"))
	if err != nil {
		t.Fatalf("rewritten .ics missing: %v", err)
	}
	if !strings.Contains(string(newBody), "UID:remote-uid-1") || !strings.Contains(string(newBody), "SUMMARY:My event") {
		t.Errorf("rewritten .ics content wrong:\n%s", newBody)
	}

	// Status is keyed by the remote iCalUId with the returned id and the
	// content hash of the read-back event.
	st, ok := h.status.Items["remote-uid-1"]
	if !ok {
		t.Fatal("status entry for remote-uid-1 not recorded")
	}
	if st.GraphID != "g-new" || st.RemoteHash != contentHashOf(t, h.getResponses["g-new"]) || st.LocalHash != hashBytes(newBody) {
		t.Errorf("status = %+v, want GraphID=g-new RemoteHash of read-back content LocalHash of rewritten bytes", st)
	}
	if _, ok := h.status.Items["uid-local"]; ok {
		t.Error("stale status under the local UID must not exist")
	}
}

func TestSyncUploadUpdate(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck1")}
	h.sync(SyncOptions{})

	h.writeLocal("uid-1", "Standup edited locally")
	h.patchResponses = map[string]map[string]any{"g1": {"id": "g1", "changeKey": "ck-patched"}}
	h.getResponses = map[string]map[string]any{"g1": masterEvent("g1", "uid-1", "Standup edited locally", "ck-patched")}

	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{Uploaded: 1}) {
		t.Fatalf("stats = %+v, want Uploaded=1 only", stats)
	}
	if h.patchBodies["g1"] == nil || h.patchBodies["g1"]["subject"] != "Standup edited locally" {
		t.Errorf("PATCH body = %v, want subject 'Standup edited locally'", h.patchBodies["g1"])
	}

	localBody, _ := os.ReadFile(h.icsPath("uid-1"))
	if !strings.Contains(string(localBody), "edited locally") {
		t.Error("upload update must not overwrite the local edit")
	}
	st := h.status.Items["uid-1"]
	if st.GraphID != "g1" || st.RemoteHash != contentHashOf(t, h.getResponses["g1"]) || st.LocalHash != hashBytes(localBody) {
		t.Errorf("status = %+v, want RemoteHash of read-back content LocalHash of local bytes", st)
	}
}

func TestUploadCreateThenNoop(t *testing.T) {
	// Graph's changeKey is NOT a stable etag: the POST response, the read-back
	// GET and the next FetchMasterEvents all report DIFFERENT changeKeys for
	// the same untouched event. With the content-hash baseline the second sync
	// must still be a clean no-op.
	h := newSyncHarness(t)
	h.writeLocal("uid-local", "My event")
	h.createResponse = masterEvent("g-new", "remote-uid-1", "My event", "ck-response")
	h.getResponses = map[string]map[string]any{"g-new": masterEvent("g-new", "remote-uid-1", "My event", "ck-settled")}

	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{Uploaded: 1}) {
		t.Fatalf("stats = %+v, want Uploaded=1 only", stats)
	}
	st, ok := h.status.Items["remote-uid-1"]
	if !ok {
		t.Fatal("status entry for remote-uid-1 not recorded")
	}
	if st.RemoteHash != contentHashOf(t, h.getResponses["g-new"]) {
		t.Errorf("RemoteHash = %q, want the content hash of the read-back event", st.RemoteHash)
	}
	body, err := os.ReadFile(h.icsPath("remote-uid-1"))
	if err != nil {
		t.Fatalf("rewritten .ics missing: %v", err)
	}
	if st.LocalHash != hashBytes(body) {
		t.Errorf("LocalHash = %q, want hash of the rewritten file bytes", st.LocalHash)
	}

	// Second sync: identical content, yet ANOTHER changeKey. Clean no-op.
	h.events = []map[string]any{masterEvent("g-new", "remote-uid-1", "My event", "ck-churned")}
	if stats := h.sync(SyncOptions{}); stats != (SyncStats{}) {
		t.Fatalf("second run stats = %+v, want all zero", stats)
	}
}

func TestUploadCreateReadBackFailureFallsBack(t *testing.T) {
	// When the post-create GET fails (no getResponses -> 404), the sync must
	// still succeed and record the content hash of the POST response event.
	h := newSyncHarness(t)
	h.writeLocal("uid-local", "My event")
	h.createResponse = masterEvent("g-new", "remote-uid-1", "My event", "ck-response")

	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{Uploaded: 1}) {
		t.Fatalf("stats = %+v, want Uploaded=1 only", stats)
	}
	if st := h.status.Items["remote-uid-1"]; st.RemoteHash != contentHashOf(t, h.createResponse) {
		t.Errorf("RemoteHash = %q, want fallback to the POST response content hash", st.RemoteHash)
	}
}

func TestUploadUpdateThenNoop(t *testing.T) {
	// PATCH analog of TestUploadCreateThenNoop: the baseline comes from the
	// read-back content, the local edit stays untouched, and the next sync —
	// reporting yet another changeKey for identical content — is a clean
	// no-op instead of a conflict.
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck1")}
	h.sync(SyncOptions{})

	h.writeLocal("uid-1", "Standup edited locally")
	h.patchResponses = map[string]map[string]any{"g1": {"id": "g1", "changeKey": "ck-response"}}
	h.getResponses = map[string]map[string]any{"g1": masterEvent("g1", "uid-1", "Standup edited locally", "ck-settled")}

	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{Uploaded: 1}) {
		t.Fatalf("stats = %+v, want Uploaded=1 only", stats)
	}
	localBody, _ := os.ReadFile(h.icsPath("uid-1"))
	if !strings.Contains(string(localBody), "edited locally") {
		t.Error("upload update must not overwrite the local edit")
	}
	st := h.status.Items["uid-1"]
	if st.RemoteHash != contentHashOf(t, h.getResponses["g1"]) {
		t.Errorf("RemoteHash = %q, want the content hash of the read-back event", st.RemoteHash)
	}
	if st.LocalHash != hashBytes(localBody) {
		t.Errorf("LocalHash = %q, want hash of the untouched local bytes", st.LocalHash)
	}

	// Second sync: identical content, yet ANOTHER changeKey. Clean no-op.
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup edited locally", "ck-churned")}
	if stats := h.sync(SyncOptions{}); stats != (SyncStats{}) {
		t.Fatalf("second run stats = %+v, want all zero", stats)
	}
	if after, _ := os.ReadFile(h.icsPath("uid-1")); string(after) != string(localBody) {
		t.Error("second run rewrote the local file")
	}
}

func TestSyncDeleteAfterCreateIsDeleteRemote(t *testing.T) {
	// Live repro of the changeKey-churn bug: create an event, then delete it
	// locally. Graph reports a DIFFERENT changeKey on the read-back and again
	// on the next FetchMasterEvents even though the content never changed —
	// the second sync must still classify this as a clean remote delete, not
	// as a "remote changed + local deleted" conflict that re-downloads the
	// event.
	h := newSyncHarness(t)
	h.writeLocal("uid-local", "My event")
	h.createResponse = masterEvent("g-new", "remote-uid-1", "My event", "ck-response")
	h.getResponses = map[string]map[string]any{"g-new": masterEvent("g-new", "remote-uid-1", "My event", "ck-settled")}

	if stats := h.sync(SyncOptions{}); stats != (SyncStats{Uploaded: 1}) {
		t.Fatalf("create stats = %+v, want Uploaded=1 only", stats)
	}

	h.events = []map[string]any{masterEvent("g-new", "remote-uid-1", "My event", "ck-churned")}
	if err := os.Remove(h.icsPath("remote-uid-1")); err != nil {
		t.Fatalf("remove local file: %v", err)
	}

	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{DeletedRemote: 1}) {
		t.Fatalf("stats = %+v, want DeletedRemote=1 only (not a conflict)", stats)
	}
	if len(h.deletedIDs) != 1 || h.deletedIDs[0] != "g-new" {
		t.Errorf("deletedIDs = %v, want [g-new]", h.deletedIDs)
	}
	if _, ok := h.status.Items["remote-uid-1"]; ok {
		t.Error("status entry not dropped after remote delete")
	}
}

func TestSyncDeleteRemote(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck1")}
	h.sync(SyncOptions{})

	if err := os.Remove(h.icsPath("uid-1")); err != nil {
		t.Fatalf("remove local file: %v", err)
	}
	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{DeletedRemote: 1}) {
		t.Fatalf("stats = %+v, want DeletedRemote=1 only", stats)
	}
	if len(h.deletedIDs) != 1 || h.deletedIDs[0] != "g1" {
		t.Errorf("deletedIDs = %v, want [g1]", h.deletedIDs)
	}
	if _, ok := h.status.Items["uid-1"]; ok {
		t.Error("status entry not dropped after remote delete")
	}
}

func TestSyncDeleteRemote404IsSuccess(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck1")}
	h.sync(SyncOptions{})

	if err := os.Remove(h.icsPath("uid-1")); err != nil {
		t.Fatal(err)
	}
	h.deleteStatus = http.StatusNotFound
	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{DeletedRemote: 1}) {
		t.Fatalf("stats = %+v, want DeletedRemote=1 (404 treated as success)", stats)
	}
	if _, ok := h.status.Items["uid-1"]; ok {
		t.Error("status entry not dropped after 404 delete")
	}
}

// MARK: - Conflicts

func TestSyncConflictRemotePolicy(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck1")}
	h.sync(SyncOptions{})

	h.writeLocal("uid-1", "Local edit")
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Remote edit", "ck2")}

	// Default policy (empty = "remote"): remote wins, local backed up.
	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{Conflicts: 1}) {
		t.Fatalf("stats = %+v, want Conflicts=1 only", stats)
	}

	body, _ := os.ReadFile(h.icsPath("uid-1"))
	if !strings.Contains(string(body), "SUMMARY:Remote edit") {
		t.Errorf("remote content did not win:\n%s", body)
	}
	backups := h.conflictBackups(h.icsPath("uid-1"))
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want exactly one .conflict-* file", backups)
	}
	if backup, _ := os.ReadFile(backups[0]); !strings.Contains(string(backup), "Local edit") {
		t.Errorf("backup does not preserve the local edit:\n%s", backup)
	}
	if st := h.status.Items["uid-1"]; st.RemoteHash != contentHashOf(t, h.events[0]) || st.LocalHash != hashBytes(body) {
		t.Errorf("status not set to remote: %+v", st)
	}
	if h.mutations != 0 {
		t.Errorf("remote-wins conflict performed %d remote mutations", h.mutations)
	}
}

func TestSyncConflictLocalPolicy(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck1")}
	h.sync(SyncOptions{})

	h.writeLocal("uid-1", "Local edit")
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Remote edit", "ck2")}
	h.patchResponses = map[string]map[string]any{"g1": {"id": "g1", "changeKey": "ck3"}}
	h.getResponses = map[string]map[string]any{"g1": masterEvent("g1", "uid-1", "Local edit", "ck3")}

	stats := h.sync(SyncOptions{Conflict: "local"})
	if stats != (SyncStats{Conflicts: 1}) {
		t.Fatalf("stats = %+v, want Conflicts=1 only", stats)
	}
	if h.patchBodies["g1"] == nil || h.patchBodies["g1"]["subject"] != "Local edit" {
		t.Errorf("PATCH body = %v, want the local edit", h.patchBodies["g1"])
	}
	body, _ := os.ReadFile(h.icsPath("uid-1"))
	if !strings.Contains(string(body), "Local edit") {
		t.Error("local-wins conflict must keep the local file")
	}
	if len(h.conflictBackups(h.icsPath("uid-1"))) != 0 {
		t.Error("local-wins conflict must not create a backup")
	}
	if st := h.status.Items["uid-1"]; st.RemoteHash != contentHashOf(t, h.getResponses["g1"]) || st.LocalHash != hashBytes(body) {
		t.Errorf("status not set to local baseline: %+v", st)
	}
}

func TestSyncConflictNewerPolicy(t *testing.T) {
	// Local LAST-MODIFIED (2026-07-29, from writeLocal) is newer than the
	// remote lastModifiedDateTime (2026-07-20) -> local wins via PATCH.
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck1")}
	h.sync(SyncOptions{})

	h.writeLocal("uid-1", "Local edit")
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Remote edit", "ck2")}
	h.patchResponses = map[string]map[string]any{"g1": {"id": "g1", "changeKey": "ck3"}}
	h.getResponses = map[string]map[string]any{"g1": masterEvent("g1", "uid-1", "Local edit", "ck3")}

	stats := h.sync(SyncOptions{Conflict: "newer"})
	if stats != (SyncStats{Conflicts: 1}) {
		t.Fatalf("stats = %+v, want Conflicts=1 only", stats)
	}
	if h.patchBodies["g1"] == nil || h.patchBodies["g1"]["subject"] != "Local edit" {
		t.Errorf("newer policy should have uploaded the newer local edit, PATCH body = %v", h.patchBodies["g1"])
	}
	if st := h.status.Items["uid-1"]; st.RemoteHash != contentHashOf(t, h.getResponses["g1"]) {
		t.Errorf("status not updated after newer-wins upload: %+v", st)
	}
}

func TestSyncConflictRemoteDeletedLocalChanged(t *testing.T) {
	// Remote deleted + local edited, remote policy: the deletion wins, but
	// the local edit survives as a backup.
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck1")}
	h.sync(SyncOptions{})

	h.writeLocal("uid-1", "Local edit")
	h.events = nil

	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{Conflicts: 1}) {
		t.Fatalf("stats = %+v, want Conflicts=1 only", stats)
	}
	if _, err := os.Stat(h.icsPath("uid-1")); !os.IsNotExist(err) {
		t.Errorf("local file should be gone after remote-deletion-wins (err=%v)", err)
	}
	backups := h.conflictBackups(h.icsPath("uid-1"))
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want exactly one", backups)
	}
	if backup, _ := os.ReadFile(backups[0]); !strings.Contains(string(backup), "Local edit") {
		t.Error("backup does not preserve the local edit")
	}
	if _, ok := h.status.Items["uid-1"]; ok {
		t.Error("status entry not dropped")
	}
	if h.mutations != 0 {
		t.Errorf("remote-deletion-wins performed %d remote mutations", h.mutations)
	}
}

// MARK: - Dry run

func TestSyncDryRunTouchesNothing(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck1")}

	stats := h.sync(SyncOptions{DryRun: true})
	if stats != (SyncStats{Downloaded: 1}) {
		t.Fatalf("dry-run stats = %+v, want Downloaded=1 only", stats)
	}
	if _, err := os.Stat(h.icsPath("uid-1")); !os.IsNotExist(err) {
		t.Errorf("dry run wrote a file (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(h.calDir, "displayname")); !os.IsNotExist(err) {
		t.Errorf("dry run wrote vdir metadata (err=%v)", err)
	}
	if len(h.status.Items) != 0 {
		t.Errorf("dry run mutated status: %+v", h.status.Items)
	}
}

func TestSyncDryRunSkipsRemoteWrites(t *testing.T) {
	h := newSyncHarness(t)
	path := h.writeLocal("uid-local", "My event")
	before, _ := os.ReadFile(path)
	// No createResponse configured: any POST would fail the test.

	stats := h.sync(SyncOptions{DryRun: true})
	if stats != (SyncStats{Uploaded: 1}) {
		t.Fatalf("dry-run stats = %+v, want Uploaded=1 only", stats)
	}
	if h.mutations != 0 {
		t.Errorf("dry run performed %d remote mutations", h.mutations)
	}
	if after, _ := os.ReadFile(path); string(before) != string(after) {
		t.Error("dry run modified the local file")
	}
	if len(h.status.Items) != 0 {
		t.Errorf("dry run mutated status: %+v", h.status.Items)
	}
}

// MARK: - Local scan robustness

func TestScanLocalItemsSkipsBadFiles(t *testing.T) {
	h := newSyncHarness(t)
	h.writeLocal("uid-good", "Fine")
	if err := os.WriteFile(filepath.Join(h.calDir, "broken.ics"), []byte("not an ics"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.calDir, "notes.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}

	items, err := scanLocalItems(h.calDir)
	if err != nil {
		t.Fatalf("scanLocalItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %v, want only uid-good", items)
	}
	li, ok := items["uid-good"]
	if !ok {
		t.Fatalf("uid-good missing from scan: %v", items)
	}
	if li.event.Subject != "Fine" || li.mtime.IsZero() {
		t.Errorf("scan did not keep parsed event/mtime: %+v", li)
	}
}

// MARK: - Content hash

func TestEventContentHashIgnoresVolatileFields(t *testing.T) {
	base := Event{
		ID:           "g1",
		ICalUID:      "uid-1",
		Subject:      "Standup",
		Location:     "HQ",
		Description:  "agenda",
		Start:        time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC),
		End:          time.Date(2026, 7, 23, 11, 0, 0, 0, time.UTC),
		LastModified: time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC),
		ChangeKey:    "ck1",
		Type:         "singleInstance",
	}

	// Identity/bookkeeping churn must not move the hash.
	churned := base
	churned.ID = "g-other"
	churned.ICalUID = "uid-other"
	churned.ChangeKey = "ck-churned"
	churned.LastModified = base.LastModified.Add(48 * time.Hour)
	churned.Type = "seriesMaster"
	if eventContentHash(churned) != eventContentHash(base) {
		t.Error("hash must ignore ID/ICalUID/ChangeKey/LastModified/Type")
	}

	// CRLF vs LF descriptions are the same content.
	crlf := base
	crlf.Description = "line1\r\nline2"
	lf := base
	lf.Description = "line1\nline2"
	if eventContentHash(crlf) != eventContentHash(lf) {
		t.Error("hash must normalize description line endings")
	}

	// Every meaningful field must move the hash.
	for name, mutate := range map[string]func(*Event){
		"Subject":     func(e *Event) { e.Subject = "Standup v2" },
		"Location":    func(e *Event) { e.Location = "Elsewhere" },
		"Description": func(e *Event) { e.Description = "new agenda" },
		"Start":       func(e *Event) { e.Start = e.Start.Add(time.Hour) },
		"End":         func(e *Event) { e.End = e.End.Add(time.Hour) },
		"AllDay":      func(e *Event) { e.AllDay = true },
		"Recurrence": func(e *Event) {
			e.Recurrence = &Recurrence{
				Pattern: RecurrencePattern{Type: "daily", Interval: 1},
				Range:   RecurrenceRange{Type: "noEnd", StartDate: "2026-07-23"},
			}
		},
	} {
		changed := base
		mutate(&changed)
		if eventContentHash(changed) == eventContentHash(base) {
			t.Errorf("hash must change when %s changes", name)
		}
	}
}

// MARK: - FileStateStore

func TestFileStateStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStateStore(dir)

	// A directory with no prior state loads empty.
	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load empty: %v", err)
	}
	if len(state.Calendars) != 0 {
		t.Fatalf("fresh state not empty: %+v", state)
	}

	state.Calendars["cal1"] = CalendarStatus{Items: map[string]ItemStatus{
		"uid-1": {GraphID: "g1", RemoteHash: "rh1", LocalHash: "abc"},
	}}
	if err := store.Save(state); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := loaded.Calendars["cal1"].Items["uid-1"]
	if !ok || got != (ItemStatus{GraphID: "g1", RemoteHash: "rh1", LocalHash: "abc"}) {
		t.Errorf("round-trip mismatch: %+v ok=%v", got, ok)
	}

	// The status is bound to the directory: a different dir stays empty.
	other, err := NewFileStateStore(t.TempDir()).Load()
	if err != nil {
		t.Fatalf("Load other dir: %v", err)
	}
	if len(other.Calendars) != 0 {
		t.Errorf("other directory not empty: %+v", other)
	}

	// A corrupted file is backed up and treated as empty.
	if err := os.WriteFile(store.path(), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.Load()
	if err != nil {
		t.Fatalf("Load corrupted: %v", err)
	}
	if len(recovered.Calendars) != 0 {
		t.Errorf("corrupted state not treated as empty: %+v", recovered)
	}
}
