package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/contacts"
	"github.com/julion2/durian/cli/internal/store"
)

// newTestRouter sets up a mux.Router with all routes, mirroring serve.go.
func newTestRouter(h *Handler, hub *EventHub) *mux.Router {
	r := mux.NewRouter()
	r.HandleFunc("/api/v1/search", h.SearchHandler).Methods("GET")
	r.HandleFunc("/api/v1/search/count", h.SearchCountHandler).Methods("GET")
	r.HandleFunc("/api/v1/tags", h.ListTagsHandler).Methods("GET")
	r.HandleFunc("/api/v1/threads/{thread_id}", h.ShowThreadHandler).Methods("GET")
	r.HandleFunc("/api/v1/threads/{thread_id}/tags", h.TagThreadHandler).Methods("POST")
	r.HandleFunc("/api/v1/message/body", h.ShowMessageBodyHandler).Methods("GET")
	r.HandleFunc("/api/v1/messages/{message_id}/attachments/{part_id}", h.DownloadAttachmentHandler).Methods("GET")
	r.HandleFunc("/api/v1/contacts/search", h.SearchContactsHandler).Methods("GET")
	r.HandleFunc("/api/v1/contacts/usage", h.IncrementContactUsageHandler).Methods("POST")
	r.HandleFunc("/api/v1/contacts", h.ListContactsHandler).Methods("GET")
	if hub != nil {
		r.Handle("/api/v1/events", hub).Methods("GET")
	}
	r.HandleFunc("/api/v1/calendars/events", h.CalendarEventsHandler).Methods("GET")
	r.HandleFunc("/api/v1/calendars/event", h.CalendarEventHandler).Methods("GET")
	r.HandleFunc("/api/v1/calendars/event", h.CalendarPutEventHandler).Methods("PUT")
	r.HandleFunc("/api/v1/calendars/event", h.CalendarDeleteEventHandler).Methods("DELETE")
	r.HandleFunc("/api/v1/calendars/rsvp", h.CalendarRsvpHandler).Methods("POST")
	r.HandleFunc("/api/v1/calendars/sync/event", h.CalendarSyncEventHandler).Methods("POST")
	r.HandleFunc("/api/v1/calendars", h.CalendarsHandler).Methods("GET")
	r.HandleFunc("/api/v1/outbox/send", h.EnqueueOutboxHandler).Methods("POST")
	r.HandleFunc("/api/v1/messages/{message_id}/reactions", h.EnqueueReactionHandler).Methods("POST")
	r.HandleFunc("/api/v1/outbox", h.ListOutboxHandler).Methods("GET")
	r.HandleFunc("/api/v1/outbox/{id}", h.DeleteOutboxHandler).Methods("DELETE")
	return r
}

func newTestContactsDB(t *testing.T) *contacts.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := contacts.Open(filepath.Join(dir, "contacts.db"))
	if err != nil {
		t.Fatalf("open contacts: %v", err)
	}
	if err := db.Init(); err != nil {
		t.Fatalf("init contacts: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// --- Search ---

func TestSearchHandler_OK(t *testing.T) {
	db := newTestStore(t)
	seedStoreData(t, db)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("GET", "/api/v1/search?query=tag:inbox&limit=10", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["ok"] != true {
		t.Errorf("ok = %v", resp["ok"])
	}
}

func TestSearchHandler_MissingQuery(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("GET", "/api/v1/search", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestSearchHandler_QueryTooLong(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	longQuery := strings.Repeat("x", 1025)
	req := httptest.NewRequest("GET", "/api/v1/search?query="+longQuery, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestSearchHandler_InvalidLimit(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("GET", "/api/v1/search?query=test&limit=abc", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestSearchCountHandler_OK(t *testing.T) {
	db := newTestStore(t)
	seedStoreData(t, db)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("GET", "/api/v1/search/count?query=tag:inbox", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]int
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["count"] == 0 {
		t.Error("expected non-zero count for tag:inbox")
	}
}

func TestSearchCountHandler_MissingQuery(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("GET", "/api/v1/search/count", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// --- Thread ---

func TestShowThreadHandler_OK(t *testing.T) {
	db := newTestStore(t)
	seedStoreData(t, db)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	m1, _ := db.GetByMessageID("msg1@test")

	req := httptest.NewRequest("GET", "/api/v1/threads/"+m1.ThreadID, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["ok"] != true {
		t.Errorf("ok = %v", resp["ok"])
	}
}

func TestShowThreadHandler_NotFound(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("GET", "/api/v1/threads/nonexistent", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		// Handler returns JSON with ok:false, not HTTP 404
		var resp map[string]any
		json.NewDecoder(w.Body).Decode(&resp)
		if resp["ok"] != false {
			t.Errorf("expected ok=false for nonexistent thread")
		}
	}
}

// --- Tags ---

func TestListTagsHandler(t *testing.T) {
	db := newTestStore(t)
	seedStoreData(t, db)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("GET", "/api/v1/tags", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestTagThreadHandler_OK(t *testing.T) {
	db := newTestStore(t)
	seedStoreData(t, db)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	m1, _ := db.GetByMessageID("msg1@test")
	body := `{"tags":"+archived -unread"}`

	req := httptest.NewRequest("POST", "/api/v1/threads/"+m1.ThreadID+"/tags", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	// Verify tags were applied
	tags, _ := db.GetTagsByMessageID("msg1@test")
	tagSet := make(map[string]bool)
	for _, tag := range tags {
		tagSet[tag] = true
	}
	if !tagSet["archived"] {
		t.Error("expected 'archived' tag")
	}
	if tagSet["unread"] {
		t.Error("'unread' should be removed")
	}
}

func TestTagThreadHandler_InvalidBody(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("POST", "/api/v1/threads/abc/tags", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// --- Message Body ---

func TestShowMessageBodyHandler_OK(t *testing.T) {
	db := newTestStore(t)
	seedStoreData(t, db)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("GET", "/api/v1/message/body?id=msg1@test", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestShowMessageBodyHandler_MissingID(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("GET", "/api/v1/message/body", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// --- Attachment ---

func TestDownloadAttachmentHandler_InvalidPartID(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("GET", "/api/v1/messages/msg1@test/attachments/abc", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestDownloadAttachmentHandler_NotFound(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("GET", "/api/v1/messages/nonexistent/attachments/1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// --- Outbox ---

func TestOutboxEnqueueAndList(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	body := `{"from":"alice@x","to":["bob@x"],"subject":"Test","body":"Hello"}`
	req := httptest.NewRequest("POST", "/api/v1/outbox/send", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("enqueue status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	var enqResp map[string]any
	json.NewDecoder(w.Body).Decode(&enqResp)
	if enqResp["ok"] != true {
		t.Errorf("enqueue ok = %v", enqResp["ok"])
	}

	// List outbox
	req = httptest.NewRequest("GET", "/api/v1/outbox", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d", w.Code)
	}

	var items []map[string]any
	json.NewDecoder(w.Body).Decode(&items)
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if items[0]["subject"] != "Test" {
		t.Errorf("subject = %v", items[0]["subject"])
	}
}

func TestOutboxEnqueue_MissingFrom(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	body := `{"to":["bob@x"],"subject":"Test"}`
	req := httptest.NewRequest("POST", "/api/v1/outbox/send", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestOutboxEnqueue_MissingTo(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	body := `{"from":"alice@x","subject":"Test"}`
	req := httptest.NewRequest("POST", "/api/v1/outbox/send", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestOutboxEnqueue_InvalidJSON(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("POST", "/api/v1/outbox/send", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestOutboxEnqueueWithDelay(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	body := `{"from":"alice@x","to":["bob@x"],"subject":"Delayed","delay_seconds":5}`
	req := httptest.NewRequest("POST", "/api/v1/outbox/send", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	sendAfter := int64(resp["send_after"].(float64))
	if sendAfter <= time.Now().Unix() {
		t.Error("send_after should be in the future")
	}
}

func TestReactionEnqueueDerivesAccountScopedReply(t *testing.T) {
	db := newTestStore(t)
	for _, msg := range []*store.Message{
		{MessageID: "target@test", Account: "work", FromAddr: "Author <author@test>", Subject: "Meeting", Refs: "<root@test>", Date: 1, CreatedAt: 1},
		{MessageID: "target@test", Account: "personal", FromAddr: "wrong@test", Subject: "Wrong", Date: 1, CreatedAt: 1},
	} {
		if err := db.InsertMessage(msg); err != nil {
			t.Fatal(err)
		}
	}
	target, _ := db.GetByMessageIDAndAccount("target@test", "work")
	if err := db.InsertHeader(target.ID, "reply-to", "Replies <reply@test>"); err != nil {
		t.Fatal(err)
	}
	h := New(db, nil)
	h.SetConfig(&config.Config{Accounts: []config.AccountConfig{{Name: "Work", Email: "me@work.test"}, {Name: "Personal", Email: "me@personal.test"}}})
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("POST", "/api/v1/messages/target@test/reactions", strings.NewReader(`{"account":"work","emoji":"👍"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var response struct {
		SendAfter int64 `json:"send_after"`
	}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	delay := response.SendAfter - time.Now().Unix()
	if delay < reactionSendDelay-1 || delay > reactionSendDelay {
		t.Errorf("reaction delay = %ds, want %ds", delay, reactionSendDelay)
	}
	items, err := db.ListOutbox()
	if err != nil || len(items) != 1 {
		t.Fatalf("outbox = %v err=%v", items, err)
	}
	var draft OutboxDraft
	if err := json.Unmarshal([]byte(items[0].DraftJSON), &draft); err != nil {
		t.Fatal(err)
	}
	if draft.Account != "work" || draft.From != "me@work.test" || len(draft.To) != 1 || draft.To[0] != `"Replies" <reply@test>` {
		t.Errorf("account-derived fields = %+v", draft)
	}
	if draft.Subject != "Re: Meeting" || draft.InReplyTo != "target@test" || draft.References != "<root@test> <target@test>" {
		t.Errorf("thread fields = %+v", draft)
	}
}

func TestReactionRejectsUnsupportedDuplicateAndWrongAccount(t *testing.T) {
	db := newTestStore(t)
	if err := db.InsertMessage(&store.Message{MessageID: "target@test", Account: "work", FromAddr: "author@test", Subject: "Hi", Date: 1, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	h := New(db, nil)
	h.SetConfig(&config.Config{Accounts: []config.AccountConfig{{Name: "Work", Email: "me@work.test"}, {Name: "Personal", Email: "me@personal.test"}}})
	r := newTestRouter(h, nil)
	post := func(body string) int {
		req := httptest.NewRequest("POST", "/api/v1/messages/target@test/reactions", strings.NewReader(body))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	if got := post(`{"account":"work","emoji":"🔥"}`); got != http.StatusBadRequest {
		t.Errorf("unsupported emoji status = %d", got)
	}
	if got := post(`{"account":"personal","emoji":"👍"}`); got != http.StatusNotFound {
		t.Errorf("wrong account status = %d", got)
	}
	if got := post(`{"account":"work","emoji":"👍"}`); got != http.StatusOK {
		t.Fatalf("first status = %d", got)
	}
	if got := post(`{"account":"work","emoji":"👍"}`); got != http.StatusConflict {
		t.Errorf("duplicate status = %d", got)
	}
	items, err := db.ListOutbox()
	if err != nil || len(items) != 1 {
		t.Fatalf("outbox = %v err=%v", items, err)
	}
	req := httptest.NewRequest("DELETE", fmt.Sprintf("/api/v1/outbox/%d", items[0].ID), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("undo status = %d", w.Code)
	}
	if got := post(`{"account":"work","emoji":"👍"}`); got != http.StatusOK {
		t.Errorf("status after undo = %d, want 200", got)
	}
}

func TestOutboxDelete(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	// Enqueue first
	body := `{"from":"alice@x","to":["bob@x"],"subject":"Delete me"}`
	req := httptest.NewRequest("POST", "/api/v1/outbox/send", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var enqResp map[string]any
	json.NewDecoder(w.Body).Decode(&enqResp)
	id := int64(enqResp["id"].(float64))

	// Delete
	req = httptest.NewRequest("DELETE", fmt.Sprintf("/api/v1/outbox/%d", id), nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %s", w.Code, w.Body.String())
	}

	// Verify empty
	req = httptest.NewRequest("GET", "/api/v1/outbox", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var items []map[string]any
	json.NewDecoder(w.Body).Decode(&items)
	if len(items) != 0 {
		t.Errorf("got %d items after delete, want 0", len(items))
	}
}

func TestOutboxDelete_InvalidID(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("DELETE", "/api/v1/outbox/abc", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestOutboxDelete_NotFound(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("DELETE", "/api/v1/outbox/999", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// --- Contacts ---

func TestSearchContactsHandler_NoDB(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil) // nil contacts
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("GET", "/api/v1/contacts/search?query=alice", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestSearchContactsHandler_MissingQuery(t *testing.T) {
	db := newTestStore(t)
	cdb := newTestContactsDB(t)
	h := New(db, cdb)
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("GET", "/api/v1/contacts/search", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestSearchContactsHandler_OK(t *testing.T) {
	db := newTestStore(t)
	cdb := newTestContactsDB(t)
	cdb.Add("alice@example.com", "Alice Smith", "test")
	h := New(db, cdb)
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("GET", "/api/v1/contacts/search?query=alice", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	var results []map[string]any
	json.NewDecoder(w.Body).Decode(&results)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
}

func TestSearchContactsHandler_ByName(t *testing.T) {
	db := newTestStore(t)
	cdb := newTestContactsDB(t)
	cdb.Add("bob@example.com", "Bob Jones", "test")
	h := New(db, cdb)
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("GET", "/api/v1/contacts/search?name=Bob+Jones", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	var results []map[string]any
	json.NewDecoder(w.Body).Decode(&results)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
}

func TestListContactsHandler(t *testing.T) {
	db := newTestStore(t)
	cdb := newTestContactsDB(t)
	cdb.Add("a@example.com", "A", "test")
	cdb.Add("b@example.com", "B", "test")
	h := New(db, cdb)
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("GET", "/api/v1/contacts?limit=10", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	var results []map[string]any
	json.NewDecoder(w.Body).Decode(&results)
	if len(results) != 2 {
		t.Errorf("got %d contacts, want 2", len(results))
	}
}

func TestIncrementContactUsageHandler(t *testing.T) {
	db := newTestStore(t)
	cdb := newTestContactsDB(t)
	cdb.Add("inc@example.com", "Inc", "test")
	h := New(db, cdb)
	r := newTestRouter(h, nil)

	body := `{"emails":["inc@example.com"]}`
	req := httptest.NewRequest("POST", "/api/v1/contacts/usage", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

// --- EventHub ---

func TestEventHub_BroadcastAndSubscribe(t *testing.T) {
	hub := NewEventHub()

	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	hub.Broadcast(NewMailEvent{
		Account:  "test",
		TotalNew: 1,
	})

	select {
	case msg := <-ch:
		if !strings.Contains(string(msg), "new_mail") {
			t.Errorf("expected new_mail event, got %q", msg)
		}
	case <-time.After(time.Second):
		t.Error("timeout waiting for event")
	}
}

func TestEventHub_BroadcastOutbox(t *testing.T) {
	hub := NewEventHub()

	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	hub.BroadcastOutbox(OutboxUpdateEvent{
		ItemID: 1,
		Status: "sent",
	})

	select {
	case msg := <-ch:
		if !strings.Contains(string(msg), "outbox_update") {
			t.Errorf("expected outbox_update event, got %q", msg)
		}
	case <-time.After(time.Second):
		t.Error("timeout waiting for event")
	}
}

func TestEventHub_SlowSubscriberDropped(t *testing.T) {
	hub := NewEventHub()

	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)

	// Fill up the buffer (capacity 8)
	for i := 0; i < 10; i++ {
		hub.Broadcast(NewMailEvent{Account: "test", TotalNew: i})
	}

	// Should have 8 messages (buffer size), rest dropped
	count := 0
	for {
		select {
		case <-ch:
			count++
		default:
			goto done
		}
	}
done:
	if count != 8 {
		t.Errorf("got %d events, want 8 (buffer capacity)", count)
	}
}

// --- sanitizeFilename ---

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"report.pdf", "report.pdf"},
		{"../../../etc/passwd", "passwd"},
		{"", "attachment"},
		{".", "attachment"},
		{"file\x00name.txt", "filename.txt"},
		{`file"name.txt`, "filename.txt"},
		{"/path/to/file.txt", "file.txt"},
	}

	for _, tt := range tests {
		got := sanitizeFilename(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
