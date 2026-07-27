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

// eventOf decodes one fake master-event JSON object exactly like the client
// does.
func eventOf(t *testing.T, m map[string]any) Event {
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
	return ev
}

// contentHashOf returns the eventContentHash of one fake master-event JSON
// object — the expected RemoteHash baseline after syncing against it.
func contentHashOf(t *testing.T, m map[string]any) string {
	t.Helper()
	return eventContentHash(eventOf(t, m), testOwnerEmail)
}

// att builds one Graph attendee JSON object.
func att(name, email, typ, resp string) map[string]any {
	return map[string]any{
		"type":         typ,
		"status":       map[string]any{"response": resp},
		"emailAddress": map[string]any{"name": name, "address": email},
	}
}

// meetingMaster builds a Graph master-event JSON object for a meeting: the
// organizer is the owner (isOrganizer=true) or organizer@example.com, myResp
// is the owner's responseStatus, and attendees are Graph attendee objects
// (see att).
func meetingMaster(id, uid, subject, changeKey string, isOrganizer bool, myResp string, attendees ...map[string]any) map[string]any {
	m := masterEvent(id, uid, subject, changeKey)
	organizer := "organizer@example.com"
	if isOrganizer {
		organizer = testOwnerEmail
	}
	m["attendees"] = attendees
	m["organizer"] = map[string]any{"emailAddress": map[string]any{"name": "Org", "address": organizer}}
	m["isOrganizer"] = isOrganizer
	m["responseStatus"] = map[string]any{"response": myResp}
	return m
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
	// createStatus, when non-zero, is returned for every POST create instead
	// of the configured response (e.g. 400 for continue-on-error tests).
	createStatus int
	// patchResponses maps event id -> PATCH response body; a PATCH for an id
	// without one fails the test.
	patchResponses map[string]map[string]any
	patchBodies    map[string]map[string]any
	// patchStatus, when non-zero, is returned for every PATCH instead of the
	// configured response (e.g. 412 for If-Match tests).
	patchStatus  int
	patchIfMatch map[string]string
	deletedIDs   []string
	// deleteStatus is the HTTP status for DELETE (default 204).
	deleteStatus  int
	deleteIfMatch map[string]string
	// rsvpCalls records every POST /me/events/{id}/{accept|decline|
	// tentativelyAccept} with its sendResponse flag.
	rsvpCalls []rsvpCall
	// mutations counts every POST/PATCH/DELETE received — ANY write endpoint.
	mutations int
	// notifications counts writes that actually make Graph send mail: creates
	// with attendees, patches/deletes of ids registered in organizerMeeting,
	// and RSVP posts with sendResponse=true.
	notifications int
	// organizerMeeting marks event ids that are organizer-owned meetings with
	// attendees, i.e. whose PATCH/DELETE notifies the attendees.
	organizerMeeting map[string]bool
}

type rsvpCall struct {
	ID   string
	Verb string
	Send bool
}

func newSyncHarness(t *testing.T) *syncHarness {
	t.Helper()
	h := &syncHarness{
		t:                t,
		calDir:           t.TempDir(),
		status:           CalendarStatus{Items: map[string]ItemStatus{}},
		patchBodies:      map[string]map[string]any{},
		patchIfMatch:     map[string]string{},
		deleteStatus:     http.StatusNoContent,
		deleteIfMatch:    map[string]string{},
		organizerMeeting: map[string]bool{},
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
		if h.createStatus != 0 {
			w.WriteHeader(h.createStatus)
			return
		}
		if attendees, ok := body["attendees"].([]any); ok && len(attendees) > 0 {
			h.notifications++ // a create with attendees sends invitations
		}
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
	mux.HandleFunc("POST /me/events/{id}/{verb}", func(w http.ResponseWriter, r *http.Request) {
		h.mutations++
		id, verb := r.PathValue("id"), r.PathValue("verb")
		if verb != "accept" && verb != "decline" && verb != "tentativelyAccept" {
			t.Errorf("unexpected POST /me/events/%s/%s", id, verb)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode rsvp body: %v", err)
		}
		send, _ := body["sendResponse"].(bool)
		h.rsvpCalls = append(h.rsvpCalls, rsvpCall{ID: id, Verb: verb, Send: send})
		if send {
			h.notifications++ // the organizer receives a response email
		}
		w.WriteHeader(http.StatusAccepted)
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
		h.patchIfMatch[id] = r.Header.Get("If-Match")
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode patch body: %v", err)
		}
		h.patchBodies[id] = body
		if h.organizerMeeting[id] {
			h.notifications++ // an organizer edit of a meeting notifies the attendees
		}
		if h.patchStatus != 0 {
			w.WriteHeader(h.patchStatus)
			return
		}
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
		id := r.PathValue("id")
		h.deletedIDs = append(h.deletedIDs, id)
		h.deleteIfMatch[id] = r.Header.Get("If-Match")
		if h.organizerMeeting[id] {
			h.notifications++ // cancelling an organizer meeting notifies the attendees
		}
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

// rewriteLocal simulates a user edit of a downloaded .ics: parse (as the
// owner), mutate, re-serialize, write back.
func (h *syncHarness) rewriteLocal(uid string, mutate func(*Event)) {
	h.t.Helper()
	path := h.icsPath(uid)
	data, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatalf("read local ics: %v", err)
	}
	ev, err := ICalToEvent(data, testOwnerEmail)
	if err != nil {
		h.t.Fatalf("parse local ics: %v", err)
	}
	mutate(&ev)
	out, err := EventToICal(ev)
	if err != nil {
		h.t.Fatalf("serialize local ics: %v", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		h.t.Fatalf("write local ics: %v", err)
	}
}

// setOwnerPartstat flips the owner's own attendee response in an Event.
func setOwnerPartstat(ev *Event, response string) {
	for i := range ev.Attendees {
		if strings.EqualFold(ev.Attendees[i].Email, testOwnerEmail) {
			ev.Attendees[i].Response = response
		}
	}
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

// TestSyncUnparseableLocalFileIsNotADeletion pins the rule that a corrupt
// .ics must never be read as "the user deleted this event". Without the guard
// the file is invisible to the scan, the item looks locally deleted, and the
// sync cancels the remote meeting — mailing every attendee.
func TestSyncUnparseableLocalFileIsNotADeletion(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck1")}
	h.sync(SyncOptions{})

	// Corrupt the tracked file in place, as an unsupported TZID or a truncated
	// write would.
	if err := os.WriteFile(h.icsPath("uid-1"), []byte("BEGIN:VCALENDAR\nnot an ical"), 0o600); err != nil {
		t.Fatalf("corrupt local ics: %v", err)
	}

	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{}) {
		t.Fatalf("stats = %+v, want no actions at all", stats)
	}
	if len(h.deletedIDs) != 0 {
		t.Errorf("deletedIDs = %v, want none: an unreadable file is not a deletion", h.deletedIDs)
	}
	if h.mutations != 0 {
		t.Errorf("mutations = %d, want 0 remote writes", h.mutations)
	}
	if _, ok := h.status.Items["uid-1"]; !ok {
		t.Error("status entry dropped: the item must stay tracked so a repaired file re-plans normally")
	}
}

// TestSyncUnparseableFileDoesNotBlockOtherEvents keeps the guard narrow: one
// bad file suppresses deletions, not the whole calendar.
func TestSyncUnparseableFileDoesNotBlockOtherEvents(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck1")}
	h.sync(SyncOptions{})

	if err := os.WriteFile(h.icsPath("uid-1"), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("corrupt local ics: %v", err)
	}
	h.events = append(h.events, masterEvent("g2", "uid-2", "Retro", "ck2"))

	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{Downloaded: 1}) {
		t.Fatalf("stats = %+v, want the new remote event downloaded", stats)
	}
	if _, err := os.Stat(h.icsPath("uid-2")); err != nil {
		t.Errorf("uid-2 not written: %v", err)
	}
}

// TestSyncFirstSightBacksUpLocalFile covers the untracked pair whose sides
// differ: remote wins, but the local file may hold edits made before the first
// sync ever ran, so it must survive as a backup.
func TestSyncFirstSightBacksUpLocalFile(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck1")}
	path := h.writeLocal("uid-1", "Standup (my local notes)")
	local, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read local ics: %v", err)
	}

	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{Downloaded: 1}) {
		t.Fatalf("stats = %+v, want Downloaded=1", stats)
	}

	backups := h.conflictBackups(path)
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want exactly one .conflict-* backup", backups)
	}
	saved, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(saved) != string(local) {
		t.Error("backup does not hold the original local bytes")
	}
	if h.mutations != 0 {
		t.Errorf("mutations = %d, want 0: first sight never writes to the remote", h.mutations)
	}
}

// TestSyncFirstSightIdenticalKeepsNoBackup guards the other side: an adopted
// pair is not a data-loss risk and must not litter the vdir with backups.
func TestSyncFirstSightIdenticalKeepsNoBackup(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck1")}
	h.sync(SyncOptions{})

	// A second sync over the now-identical pair, with tracking forgotten.
	h.status = CalendarStatus{Items: map[string]ItemStatus{}}
	h.sync(SyncOptions{})

	if backups := h.conflictBackups(h.icsPath("uid-1")); len(backups) != 0 {
		t.Errorf("backups = %v, want none for an adopted pair", backups)
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

// MARK: - Continue on per-event failure

func TestSyncContinuesAfterEventFailure(t *testing.T) {
	// Two tracked events edited locally; every PATCH fails with a Graph 400
	// (e.g. a rejected event body). The failures must be counted — not abort
	// the run — and the unrelated new remote event (sorting AFTER the failing
	// uploads) must still be downloaded.
	h := newSyncHarness(t)
	h.events = []map[string]any{
		masterEvent("g1", "uid-a-1", "One", "ck1"),
		masterEvent("g2", "uid-a-2", "Two", "ck1"),
	}
	h.sync(SyncOptions{})
	baseline1 := h.status.Items["uid-a-1"]
	baseline2 := h.status.Items["uid-a-2"]

	h.writeLocal("uid-a-1", "One edited")
	h.writeLocal("uid-a-2", "Two edited")
	h.events = append(h.events, masterEvent("g3", "uid-z-new", "Three", "ck1"))
	h.patchStatus = http.StatusBadRequest

	stats := h.sync(SyncOptions{})
	if stats.Failed != 2 || stats.Downloaded != 1 || stats.Uploaded != 0 {
		t.Fatalf("stats = %+v, want Failed=2 Downloaded=1 Uploaded=0", stats)
	}
	// The failed items keep their old baseline so the next run re-plans them.
	if h.status.Items["uid-a-1"] != baseline1 || h.status.Items["uid-a-2"] != baseline2 {
		t.Error("failed uploads must not advance their status baseline")
	}
	// The download after the failures still landed.
	if _, err := os.Stat(h.icsPath("uid-z-new")); err != nil {
		t.Errorf("unrelated download missing after failures: %v", err)
	}
	// The local edits survive untouched for the retry.
	if body, _ := os.ReadFile(h.icsPath("uid-a-1")); !strings.Contains(string(body), "One edited") {
		t.Error("failed upload must keep the local edit")
	}
}

func TestSyncCreateFailureContinues(t *testing.T) {
	// A Graph 400 on one create (e.g. "all-day must span whole days") must not
	// block the rest of the plan.
	h := newSyncHarness(t)
	h.writeLocal("aaa-local", "Broken create")
	h.events = []map[string]any{masterEvent("g1", "uid-remote", "Fine", "ck1")}
	h.createStatus = http.StatusBadRequest

	stats := h.sync(SyncOptions{})
	if stats.Failed != 1 || stats.Downloaded != 1 || stats.Uploaded != 0 {
		t.Fatalf("stats = %+v, want Failed=1 Downloaded=1 Uploaded=0", stats)
	}
	// The failed create keeps its local file and stays untracked for the
	// next run.
	if _, err := os.Stat(h.icsPath("aaa-local")); err != nil {
		t.Errorf("failed create must keep the local file: %v", err)
	}
	if _, ok := h.status.Items["aaa-local"]; ok {
		t.Error("failed create must not be tracked")
	}
}

func TestSyncAbortsOnAuthError(t *testing.T) {
	// A 401 means every remaining action would fail identically — the run
	// aborts so the command can print the auth hint.
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "One", "ck1")}
	h.sync(SyncOptions{})
	h.writeLocal("uid-1", "One edited")
	h.patchStatus = http.StatusUnauthorized

	_, err := Sync(context.Background(), h.client, Calendar{ID: "cal1", Name: "Work"}, h.calDir, &h.status, SyncOptions{})
	if err == nil || !IsAuthError(err) {
		t.Fatalf("Sync err = %v, want an auth error abort", err)
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

	items, _, err := scanLocalItems(h.calDir, testOwnerEmail)
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
	if eventContentHash(churned, testOwnerEmail) != eventContentHash(base, testOwnerEmail) {
		t.Error("hash must ignore ID/ICalUID/ChangeKey/LastModified/Type")
	}

	// CRLF vs LF descriptions are the same content.
	crlf := base
	crlf.Description = "line1\r\nline2"
	lf := base
	lf.Description = "line1\nline2"
	if eventContentHash(crlf, testOwnerEmail) != eventContentHash(lf, testOwnerEmail) {
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
		if eventContentHash(changed, testOwnerEmail) == eventContentHash(base, testOwnerEmail) {
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

// MARK: - Owner RSVP

func TestRSVPSendsOnlyOnRealResponseChange(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{meetingMaster("g1", "uid-1", "Design sync", "ck1", false, "accepted",
		att("Me", testOwnerEmail, "required", "accepted"),
		att("Alice", "alice@example.com", "required", "accepted"))}
	h.sync(SyncOptions{})

	// The user flips their own PARTSTAT to DECLINED — nothing else.
	h.rewriteLocal("uid-1", func(ev *Event) { setOwnerPartstat(ev, "declined") })

	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{Rsvps: 1}) {
		t.Fatalf("stats = %+v, want Rsvps=1 only", stats)
	}
	if len(h.rsvpCalls) != 1 || h.rsvpCalls[0] != (rsvpCall{ID: "g1", Verb: "decline", Send: true}) {
		t.Fatalf("rsvpCalls = %+v, want exactly one decline with sendResponse=true", h.rsvpCalls)
	}
	if h.mutations != 1 {
		t.Errorf("mutations = %d, want 1 (the decline only)", h.mutations)
	}
	if st := h.status.Items["uid-1"]; st.OwnerResponse != OwnerRespDeclined {
		t.Errorf("baseline OwnerResponse = %q, want declined", st.OwnerResponse)
	}

	// Remote now reflects the decline (churned changeKey): clean no-op, no
	// second RSVP.
	h.events = []map[string]any{meetingMaster("g1", "uid-1", "Design sync", "ck-churned", false, "declined",
		att("Me", testOwnerEmail, "required", "declined"),
		att("Alice", "alice@example.com", "required", "accepted"))}
	if stats := h.sync(SyncOptions{}); stats != (SyncStats{}) {
		t.Fatalf("third run stats = %+v, want all zero", stats)
	}
	if len(h.rsvpCalls) != 1 {
		t.Errorf("rsvpCalls = %+v, want still exactly one", h.rsvpCalls)
	}
}

func TestRSVPSilentFlag(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{meetingMaster("g1", "uid-1", "Design sync", "ck1", false, "accepted",
		att("Me", testOwnerEmail, "required", "accepted"),
		att("Alice", "alice@example.com", "required", "accepted"))}
	h.sync(SyncOptions{})
	h.rewriteLocal("uid-1", func(ev *Event) { setOwnerPartstat(ev, "tentativelyAccepted") })

	stats := h.sync(SyncOptions{SilentRSVP: true})
	if stats != (SyncStats{Rsvps: 1}) {
		t.Fatalf("stats = %+v, want Rsvps=1 only", stats)
	}
	if len(h.rsvpCalls) != 1 || h.rsvpCalls[0] != (rsvpCall{ID: "g1", Verb: "tentativelyAccept", Send: false}) {
		t.Fatalf("rsvpCalls = %+v, want one tentativelyAccept with sendResponse=false", h.rsvpCalls)
	}
	if h.notifications != 0 {
		t.Errorf("notifications = %d, want 0 (silent RSVP)", h.notifications)
	}
}

func TestRSVPLossyPartstatRoundTripIsNoop(t *testing.T) {
	t.Run("owner listed as attendee", func(t *testing.T) {
		h := newSyncHarness(t)
		h.events = []map[string]any{meetingMaster("g1", "uid-1", "Design sync", "ck1", false, "accepted",
			att("Me", testOwnerEmail, "required", "accepted"),
			att("Alice", "alice@example.com", "required", "notResponded"))}
		h.sync(SyncOptions{})

		h.events = []map[string]any{meetingMaster("g1", "uid-1", "Design sync", "ck-churned", false, "accepted",
			att("Me", testOwnerEmail, "required", "accepted"),
			att("Alice", "alice@example.com", "required", "notResponded"))}
		if stats := h.sync(SyncOptions{}); stats != (SyncStats{}) {
			t.Fatalf("re-sync stats = %+v, want all zero", stats)
		}
		if len(h.rsvpCalls) != 0 || h.mutations != 0 {
			t.Errorf("re-sync performed rsvp/mutations: %+v / %d", h.rsvpCalls, h.mutations)
		}
	})

	t.Run("owner absent from attendee list", func(t *testing.T) {
		// The local file cannot express the owner's response at all (L=None
		// after the lossy round-trip); the accepted baseline must never turn
		// into an un-respond or any other call.
		h := newSyncHarness(t)
		var m map[string]any
		if err := json.Unmarshal([]byte(meetingGraphJSON), &m); err != nil {
			t.Fatal(err)
		}
		h.events = []map[string]any{m}
		h.sync(SyncOptions{})

		var m2 map[string]any
		if err := json.Unmarshal([]byte(meetingGraphJSON), &m2); err != nil {
			t.Fatal(err)
		}
		m2["changeKey"] = "ck-churned"
		h.events = []map[string]any{m2}
		if stats := h.sync(SyncOptions{}); stats != (SyncStats{}) {
			t.Fatalf("re-sync stats = %+v, want all zero", stats)
		}
		if len(h.rsvpCalls) != 0 || h.mutations != 0 {
			t.Errorf("re-sync performed rsvp/mutations: %+v / %d", h.rsvpCalls, h.mutations)
		}
	})
}

func TestRSVPIdempotentWhenLocalMatchesRemote(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{meetingMaster("g1", "uid-1", "Design sync", "ck1", false, "accepted",
		att("Me", testOwnerEmail, "required", "accepted"),
		att("Alice", "alice@example.com", "required", "accepted"))}
	h.sync(SyncOptions{})

	// The user declined locally AND the response is already recorded in
	// Outlook (L == R != B): rebaseline only, no Graph call.
	h.rewriteLocal("uid-1", func(ev *Event) { setOwnerPartstat(ev, "declined") })
	h.events = []map[string]any{meetingMaster("g1", "uid-1", "Design sync", "ck-churned", false, "declined",
		att("Me", testOwnerEmail, "required", "declined"),
		att("Alice", "alice@example.com", "required", "accepted"))}

	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{}) {
		t.Fatalf("stats = %+v, want all zero (rebaseline only)", stats)
	}
	if len(h.rsvpCalls) != 0 || h.mutations != 0 {
		t.Fatalf("idempotency guard failed: rsvpCalls=%+v mutations=%d", h.rsvpCalls, h.mutations)
	}
	if st := h.status.Items["uid-1"]; st.OwnerResponse != OwnerRespDeclined {
		t.Errorf("baseline OwnerResponse = %q, want rebaselined to declined", st.OwnerResponse)
	}

	// And the rebaseline sticks: another run does nothing.
	if stats := h.sync(SyncOptions{}); stats != (SyncStats{}) {
		t.Fatalf("second run stats = %+v, want all zero", stats)
	}
}

// MARK: - Organizer vs non-meeting writes

func TestOrganizerContentEditNotifiesAttendees(t *testing.T) {
	attendees := func(aliceResp string) []map[string]any {
		return []map[string]any{
			att("Alice", "alice@example.com", "required", aliceResp),
			att("Bob", "bob@example.com", "optional", "notResponded"),
		}
	}
	h := newSyncHarness(t)
	h.events = []map[string]any{meetingMaster("g1", "uid-1", "Roadmap", "ck1", true, "organizer", attendees("accepted")...)}
	h.sync(SyncOptions{})

	h.rewriteLocal("uid-1", func(ev *Event) { ev.Subject = "Roadmap v2" })
	h.organizerMeeting["g1"] = true
	h.patchResponses = map[string]map[string]any{"g1": {"id": "g1", "changeKey": "ck2"}}
	h.getResponses = map[string]map[string]any{
		"g1": meetingMaster("g1", "uid-1", "Roadmap v2", "ck2", true, "organizer", attendees("accepted")...),
	}

	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{Uploaded: 1}) {
		t.Fatalf("stats = %+v, want Uploaded=1 only", stats)
	}
	if h.patchBodies["g1"] == nil || h.patchBodies["g1"]["subject"] != "Roadmap v2" {
		t.Errorf("PATCH body = %v, want subject Roadmap v2", h.patchBodies["g1"])
	}
	// Attendee set unchanged: the PATCH must not re-send the attendee list.
	if _, ok := h.patchBodies["g1"]["attendees"]; ok {
		t.Error("content-only edit must not include attendees in the PATCH")
	}
	if h.patchIfMatch["g1"] != "ck1" {
		t.Errorf("PATCH If-Match = %q, want the planned changeKey ck1", h.patchIfMatch["g1"])
	}
	if h.notifications != 1 {
		t.Errorf("notifications = %d, want 1 (attendees notified of the update)", h.notifications)
	}
}

func TestNonMeetingEditSendsNothing(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck1")}
	h.sync(SyncOptions{})

	h.writeLocal("uid-1", "Standup v2")
	h.patchResponses = map[string]map[string]any{"g1": {"id": "g1", "changeKey": "ck2"}}
	h.getResponses = map[string]map[string]any{"g1": masterEvent("g1", "uid-1", "Standup v2", "ck2")}

	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{Uploaded: 1}) {
		t.Fatalf("stats = %+v, want Uploaded=1 only", stats)
	}
	if h.notifications != 0 {
		t.Errorf("notifications = %d, want 0 for a plain appointment edit", h.notifications)
	}
	if _, ok := h.patchBodies["g1"]["attendees"]; ok {
		t.Error("plain appointment PATCH must not carry attendees")
	}
}

func TestUploadCreateWithAttendeesInvites(t *testing.T) {
	h := newSyncHarness(t)
	data, err := EventToICal(Event{
		ICalUID:      "uid-local",
		Subject:      "Kickoff",
		Start:        time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC),
		End:          time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC),
		LastModified: time.Date(2026, 7, 29, 8, 0, 0, 0, time.UTC),
		Attendees: []Attendee{
			{Name: "Alice", Email: "alice@example.com", Type: "required", Response: "none"},
			{Name: "Bob", Email: "bob@example.com", Type: "optional", Response: "none"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.icsPath("uid-local"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	created := meetingMaster("g-new", "remote-uid-1", "Kickoff", "ck-new", true, "organizer",
		att("Alice", "alice@example.com", "required", "none"),
		att("Bob", "bob@example.com", "optional", "none"))
	h.createResponse = created
	h.getResponses = map[string]map[string]any{"g-new": created}

	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{Uploaded: 1}) {
		t.Fatalf("stats = %+v, want Uploaded=1 only", stats)
	}
	if len(h.createBodies) != 1 {
		t.Fatalf("createBodies = %d, want exactly one POST", len(h.createBodies))
	}
	body := h.createBodies[0]
	attendeesSent, _ := body["attendees"].([]any)
	if len(attendeesSent) != 2 {
		t.Errorf("POST attendees = %v, want 2 entries", body["attendees"])
	}
	if txn, _ := body["transactionId"].(string); txn == "" {
		t.Error("POST body missing transactionId (create retry dedup)")
	}
	if _, ok := body["organizer"]; ok {
		t.Error("POST body must never carry an organizer")
	}
	if h.notifications != 1 {
		t.Errorf("notifications = %d, want 1 (one invitation wave)", h.notifications)
	}

	// Re-sync of the settled event is a clean no-op: no second invite wave.
	h.events = []map[string]any{meetingMaster("g-new", "remote-uid-1", "Kickoff", "ck-churned", true, "organizer",
		att("Alice", "alice@example.com", "required", "none"),
		att("Bob", "bob@example.com", "optional", "none"))}
	if stats := h.sync(SyncOptions{}); stats != (SyncStats{}) {
		t.Fatalf("second run stats = %+v, want all zero", stats)
	}
	if len(h.createBodies) != 1 || h.notifications != 1 {
		t.Errorf("second run re-created/re-notified: creates=%d notifications=%d", len(h.createBodies), h.notifications)
	}
}

func TestFirstSightNeverNotifies(t *testing.T) {
	// Untracked pair on both sides with differing content, attendees on both,
	// no state file: must resolve toward remote (download) with ZERO writes.
	h := newSyncHarness(t)
	h.events = []map[string]any{meetingMaster("g1", "uid-1", "Design sync", "ck1", false, "accepted",
		att("Me", testOwnerEmail, "required", "accepted"),
		att("Alice", "alice@example.com", "required", "accepted"))}
	data, err := EventToICal(Event{
		ICalUID: "uid-1",
		Subject: "Divergent local meeting",
		Start:   time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC),
		End:     time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC),
		Attendees: []Attendee{
			{Name: "Me", Email: testOwnerEmail, Type: "required", Response: "declined"},
			{Name: "Alice", Email: "alice@example.com", Type: "required", Response: "none"},
		},
		Organizer: &Person{Name: "Org", Email: "organizer@example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.icsPath("uid-1"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{Downloaded: 1}) {
		t.Fatalf("stats = %+v, want Downloaded=1 only (remote wins on first sight)", stats)
	}
	if h.mutations != 0 || h.notifications != 0 || len(h.rsvpCalls) != 0 {
		t.Errorf("first sight wrote remotely: mutations=%d notifications=%d rsvps=%+v",
			h.mutations, h.notifications, h.rsvpCalls)
	}
	if body, _ := os.ReadFile(h.icsPath("uid-1")); !strings.Contains(string(body), "SUMMARY:Design sync") {
		t.Errorf("remote content did not win:\n%s", body)
	}
}

// MARK: - Delete routing

func TestDeleteAsAttendeeRoutesToDecline(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{meetingMaster("g1", "uid-1", "Design sync", "ck1", false, "accepted",
		att("Me", testOwnerEmail, "required", "accepted"),
		att("Alice", "alice@example.com", "required", "accepted"))}
	h.sync(SyncOptions{})

	if err := os.Remove(h.icsPath("uid-1")); err != nil {
		t.Fatal(err)
	}
	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{DeletedRemote: 1}) {
		t.Fatalf("stats = %+v, want DeletedRemote=1 only", stats)
	}
	if len(h.deletedIDs) != 0 {
		t.Errorf("deletedIDs = %v, want none (decline, not raw delete)", h.deletedIDs)
	}
	if len(h.rsvpCalls) != 1 || h.rsvpCalls[0] != (rsvpCall{ID: "g1", Verb: "decline", Send: true}) {
		t.Fatalf("rsvpCalls = %+v, want one decline with sendResponse=true", h.rsvpCalls)
	}
	if _, ok := h.status.Items["uid-1"]; ok {
		t.Error("status entry not dropped after decline-routed delete")
	}
}

func TestDeleteAsAttendeeSilentRSVP(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{meetingMaster("g1", "uid-1", "Design sync", "ck1", false, "accepted",
		att("Me", testOwnerEmail, "required", "accepted"),
		att("Alice", "alice@example.com", "required", "accepted"))}
	h.sync(SyncOptions{})

	if err := os.Remove(h.icsPath("uid-1")); err != nil {
		t.Fatal(err)
	}
	if stats := h.sync(SyncOptions{SilentRSVP: true}); stats != (SyncStats{DeletedRemote: 1}) {
		t.Fatalf("stats = %+v, want DeletedRemote=1 only", stats)
	}
	if len(h.rsvpCalls) != 1 || h.rsvpCalls[0].Send {
		t.Fatalf("rsvpCalls = %+v, want one decline with sendResponse=false", h.rsvpCalls)
	}
	if h.notifications != 0 {
		t.Errorf("notifications = %d, want 0", h.notifications)
	}
}

func TestDeleteAsOrganizerCancels(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{meetingMaster("g1", "uid-1", "Roadmap", "ck1", true, "organizer",
		att("Alice", "alice@example.com", "required", "accepted"),
		att("Bob", "bob@example.com", "optional", "notResponded"))}
	h.sync(SyncOptions{})
	h.organizerMeeting["g1"] = true

	if err := os.Remove(h.icsPath("uid-1")); err != nil {
		t.Fatal(err)
	}
	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{DeletedRemote: 1}) {
		t.Fatalf("stats = %+v, want DeletedRemote=1 only", stats)
	}
	if len(h.deletedIDs) != 1 || h.deletedIDs[0] != "g1" {
		t.Errorf("deletedIDs = %v, want [g1] (organizer cancels via DELETE)", h.deletedIDs)
	}
	if len(h.rsvpCalls) != 0 {
		t.Errorf("rsvpCalls = %+v, want none", h.rsvpCalls)
	}
	if h.deleteIfMatch["g1"] != "ck1" {
		t.Errorf("DELETE If-Match = %q, want the planned changeKey ck1", h.deleteIfMatch["g1"])
	}
	if h.notifications != 1 {
		t.Errorf("notifications = %d, want 1 (cancellation wave)", h.notifications)
	}
	if _, ok := h.status.Items["uid-1"]; ok {
		t.Error("status entry not dropped after cancel")
	}
}

// MARK: - Conflict safety

func TestConflictNoDeleteRecreateWithAttendees(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{meetingMaster("g1", "uid-1", "Roadmap", "ck1", true, "organizer",
		att("Alice", "alice@example.com", "required", "accepted"),
		att("Bob", "bob@example.com", "optional", "notResponded"))}
	h.sync(SyncOptions{})

	h.rewriteLocal("uid-1", func(ev *Event) { ev.Subject = "Roadmap edited" })
	h.events = nil // remote deleted

	stats := h.sync(SyncOptions{Conflict: "local"})
	if stats != (SyncStats{Skipped: 1}) {
		t.Fatalf("stats = %+v, want Skipped=1 only (refused meeting re-create)", stats)
	}
	if h.mutations != 0 || h.notifications != 0 {
		t.Errorf("refused re-create still wrote remotely: mutations=%d notifications=%d",
			h.mutations, h.notifications)
	}
	if _, err := os.Stat(h.icsPath("uid-1")); err != nil {
		t.Errorf("local file must survive the refusal: %v", err)
	}
	if _, ok := h.status.Items["uid-1"]; !ok {
		t.Error("status entry must survive the refusal (re-planned next run)")
	}
}

func TestPatchPreconditionFailedSkipsAction(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck1")}
	h.sync(SyncOptions{})

	h.writeLocal("uid-1", "Standup edited")
	h.patchStatus = http.StatusPreconditionFailed

	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{Skipped: 1}) {
		t.Fatalf("stats = %+v, want Skipped=1 only (412 skipped, not fatal)", stats)
	}
	if h.mutations != 1 {
		t.Errorf("mutations = %d, want 1 (the rejected PATCH attempt)", h.mutations)
	}
	// The baseline stays untouched, so the next run re-plans from scratch.
	if st := h.status.Items["uid-1"]; st.RemoteHash != contentHashOf(t, h.events[0]) {
		t.Errorf("412 must not move the baseline: %+v", st)
	}
}

// MARK: - Teams marker

func TestTeamsMarkerCreatesOnlineMeetingOnceThenNoop(t *testing.T) {
	h := newSyncHarness(t)
	h.writeLocal("uid-local", "My event")
	raw, err := os.ReadFile(h.icsPath("uid-local"))
	if err != nil {
		t.Fatal(err)
	}
	marked := strings.Replace(string(raw), "END:VEVENT",
		"X-DURIAN-CREATE-TEAMS-MEETING:TRUE\r\nEND:VEVENT", 1)
	if err := os.WriteFile(h.icsPath("uid-local"), []byte(marked), 0o600); err != nil {
		t.Fatal(err)
	}

	settled := masterEvent("g-new", "remote-uid-1", "My event", "ck-settled")
	settled["isOnlineMeeting"] = true
	settled["onlineMeeting"] = map[string]any{"joinUrl": "https://teams.microsoft.com/l/meetup-join/xyz"}
	h.createResponse = settled
	h.getResponses = map[string]map[string]any{"g-new": settled}

	stats := h.sync(SyncOptions{})
	if stats != (SyncStats{Uploaded: 1}) {
		t.Fatalf("stats = %+v, want Uploaded=1 only", stats)
	}
	body := h.createBodies[0]
	if body["isOnlineMeeting"] != true || body["onlineMeetingProvider"] != "teamsForBusiness" {
		t.Errorf("create body = %v, want isOnlineMeeting=true provider=teamsForBusiness", body)
	}

	// The rewritten local file carries the join link but NOT the marker.
	rewritten, err := os.ReadFile(h.icsPath("remote-uid-1"))
	if err != nil {
		t.Fatalf("rewritten .ics missing: %v", err)
	}
	if strings.Contains(string(rewritten), "X-DURIAN-CREATE-TEAMS-MEETING") {
		t.Error("rewritten file still carries the one-shot marker")
	}
	if !strings.Contains(string(rewritten), "teams.microsoft.com") {
		t.Errorf("rewritten file lacks the join link:\n%s", rewritten)
	}

	// Second sync: settled remote (churned changeKey), no local edits: no-op.
	churned := masterEvent("g-new", "remote-uid-1", "My event", "ck-churned")
	churned["isOnlineMeeting"] = true
	churned["onlineMeeting"] = map[string]any{"joinUrl": "https://teams.microsoft.com/l/meetup-join/xyz"}
	h.events = []map[string]any{churned}
	if stats := h.sync(SyncOptions{}); stats != (SyncStats{}) {
		t.Fatalf("second run stats = %+v, want all zero", stats)
	}
	if len(h.createBodies) != 1 {
		t.Errorf("second run created again: %d creates", len(h.createBodies))
	}
}

// MARK: - Plain re-sync and dry-run safety

func TestPlainResyncOfDownloadedMeetingSendsNothing(t *testing.T) {
	for _, policy := range []string{"", "local", "newer"} {
		name := policy
		if name == "" {
			name = "remote(default)"
		}
		t.Run(name, func(t *testing.T) {
			h := newSyncHarness(t)
			var m map[string]any
			if err := json.Unmarshal([]byte(meetingGraphJSON), &m); err != nil {
				t.Fatal(err)
			}
			h.events = []map[string]any{m}
			if stats := h.sync(SyncOptions{Conflict: policy}); stats != (SyncStats{Downloaded: 1}) {
				t.Fatalf("download stats = %+v, want Downloaded=1 only", stats)
			}

			var m2 map[string]any
			if err := json.Unmarshal([]byte(meetingGraphJSON), &m2); err != nil {
				t.Fatal(err)
			}
			m2["changeKey"] = "ck-churned"
			h.events = []map[string]any{m2}
			if stats := h.sync(SyncOptions{Conflict: policy}); stats != (SyncStats{}) {
				t.Fatalf("re-sync stats = %+v, want all zero", stats)
			}
			if h.mutations != 0 || h.notifications != 0 {
				t.Errorf("plain re-sync wrote remotely: mutations=%d notifications=%d",
					h.mutations, h.notifications)
			}
		})
	}
}

func TestDryRunHitsNoWriteEndpoints(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{
		meetingMaster("g1", "uid-1", "A", "ck1", false, "accepted",
			att("Me", testOwnerEmail, "required", "accepted"),
			att("Alice", "alice@example.com", "required", "accepted")),
		meetingMaster("g2", "uid-2", "B", "ck1", false, "accepted",
			att("Me", testOwnerEmail, "required", "accepted"),
			att("Alice", "alice@example.com", "required", "accepted")),
	}
	h.sync(SyncOptions{})

	// Pending: an RSVP (uid-1), a decline-routed delete (uid-2), and an
	// invite-carrying create (uid-local). No create/patch responses are
	// configured — ANY write endpoint hit fails the test.
	h.rewriteLocal("uid-1", func(ev *Event) { setOwnerPartstat(ev, "declined") })
	if err := os.Remove(h.icsPath("uid-2")); err != nil {
		t.Fatal(err)
	}
	data, err := EventToICal(Event{
		ICalUID: "uid-local",
		Subject: "Kickoff",
		Start:   time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC),
		End:     time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC),
		Attendees: []Attendee{
			{Name: "Alice", Email: "alice@example.com", Type: "required", Response: "none"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.icsPath("uid-local"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	mutationsBefore := h.mutations

	stats := h.sync(SyncOptions{DryRun: true})
	if stats != (SyncStats{Uploaded: 1, DeletedRemote: 1, Rsvps: 1}) {
		t.Fatalf("dry-run stats = %+v, want Uploaded=1 DeletedRemote=1 Rsvps=1", stats)
	}
	if h.mutations != mutationsBefore || len(h.rsvpCalls) != 0 || h.notifications != 0 {
		t.Errorf("dry run hit write endpoints: mutations=%d rsvps=%+v notifications=%d",
			h.mutations-mutationsBefore, h.rsvpCalls, h.notifications)
	}
	if st := h.status.Items["uid-1"]; st.OwnerResponse != OwnerRespAccepted {
		t.Errorf("dry run mutated the RSVP baseline: %+v", st)
	}
}

// MARK: - Notification preview

func TestPlanNotificationsPreview(t *testing.T) {
	meeting := Event{Attendees: []Attendee{
		{Email: "alice@example.com", Type: "required"},
		{Email: "bob@example.com", Type: "required"},
	}}
	plans := []CalendarPlan{{
		Calendar: Calendar{ID: "cal1", Name: "Work"},
		Actions: []Action{
			{Kind: ActionUploadCreate, Summary: `"Kickoff" 2026-08-01`, OwnerIsOrganizer: true, Recipients: 3},
			{Kind: ActionUploadCreate, Summary: "plain create", OwnerIsOrganizer: true, Recipients: 0},
			{Kind: ActionUploadUpdate, Summary: "meeting update", OwnerIsOrganizer: true, Recipients: 2},
			{Kind: ActionUploadUpdate, Summary: "attendee-side edit", OwnerIsOrganizer: false, Recipients: 2},
			{Kind: ActionDeleteRemote, Summary: "cancel", OwnerIsOrganizer: true, Recipients: 2, RemoteExists: true, Remote: meeting},
			{Kind: ActionDeleteRemote, Summary: "decline", OwnerIsOrganizer: false, Recipients: 1, RemoteExists: true, Remote: meeting},
			{Kind: ActionDeleteRemote, Summary: "plain delete", OwnerIsOrganizer: true, RemoteExists: true},
			{Kind: ActionRsvp, Summary: "rsvp", RsvpCall: true},
			{Kind: ActionRsvp, Summary: "rebaseline", RsvpCall: false},
			{Kind: ActionDownloadNew, Summary: "download"},
		},
	}}

	got := PlanNotifications(plans, "remote", false)
	want := []struct {
		category   string
		recipients int
	}{
		{NotifyInvite, 3},
		{NotifyUpdate, 2},
		{NotifyCancel, 2},
		{NotifyRSVP, 1},
		{NotifyRSVP, 1},
	}
	if len(got) != len(want) {
		t.Fatalf("notifications = %+v, want %d entries", got, len(want))
	}
	for i, w := range want {
		if got[i].Category != w.category || got[i].Recipients != w.recipients || got[i].Calendar != "Work" {
			t.Errorf("notification[%d] = %+v, want %s to %d recipients", i, got[i], w.category, w.recipients)
		}
	}

	// silent RSVP drops exactly the RSVP messages.
	silent := PlanNotifications(plans, "remote", true)
	if len(silent) != 3 {
		t.Errorf("silent notifications = %+v, want 3 entries (no RSVPs)", silent)
	}

	// Conflicts follow the policy: remote-wins sends nothing, local-wins on a
	// surviving pair is an update, local-wins on a deleted remote meeting is
	// refused (R3) and sends nothing.
	conflictPlans := []CalendarPlan{{
		Calendar: Calendar{Name: "Work"},
		Actions: []Action{
			{Kind: ActionConflict, OwnerIsOrganizer: true, Recipients: 2, RemoteExists: true, LocalExists: true, Remote: meeting},
			{Kind: ActionConflict, OwnerIsOrganizer: true, Recipients: 2, RemoteExists: false, LocalExists: true,
				LocalEvent: meeting},
		},
	}}
	if got := PlanNotifications(conflictPlans, "remote", false); len(got) != 0 {
		t.Errorf("remote-wins conflicts must send nothing, got %+v", got)
	}
	got = PlanNotifications(conflictPlans, "local", false)
	if len(got) != 1 || got[0].Category != NotifyUpdate || got[0].Recipients != 2 {
		t.Errorf("local-wins conflict preview = %+v, want one UPDATE to 2", got)
	}

	// NotifiesRecipients mirrors the preview for plain kinds.
	if ok, n := plans[0].Actions[0].NotifiesRecipients(); !ok || n != 3 {
		t.Errorf("NotifiesRecipients(invite) = %v/%d, want true/3", ok, n)
	}
	if ok, _ := plans[0].Actions[9].NotifiesRecipients(); ok {
		t.Error("NotifiesRecipients(download) = true, want false")
	}
}

// MARK: - Atomic local writes

// TestWriteFileAtomicLeavesNoScannableTemp pins the two properties the vdir
// depends on: the target ends up with the full content, and the temporary
// sibling is never something the local scan would pick up as an event (a
// leftover after a crash must not look like a corrupt .ics).
func TestWriteFileAtomicLeavesNoScannableTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "uid-1.ics")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("new content"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "new content" {
		t.Errorf("content = %q, want %q", got, "new content")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm = %v, want 0600", info.Mode().Perm())
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "uid-1.ics" {
			continue
		}
		t.Errorf("leftover file %q after an atomic write", e.Name())
	}
	if strings.HasSuffix(".uid-1.ics.12345.ics-tmp", ".ics") {
		t.Error("the temp suffix must not end in .ics, or the scan would parse it")
	}
}

// TestSyncDownloadIsAtomic checks the engine path actually uses it: no
// temporary file survives a completed sync.
func TestSyncDownloadIsAtomic(t *testing.T) {
	h := newSyncHarness(t)
	h.events = []map[string]any{masterEvent("g1", "uid-1", "Standup", "ck1")}
	h.sync(SyncOptions{})

	entries, err := os.ReadDir(h.calDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "-tmp") {
			t.Errorf("sync left a temp file behind: %s", e.Name())
		}
	}
	if _, err := os.Stat(h.icsPath("uid-1")); err != nil {
		t.Errorf("downloaded event missing: %v", err)
	}
}
