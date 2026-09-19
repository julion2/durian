package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/julion2/durian/cli/internal/contacts"
	"github.com/julion2/durian/cli/internal/store"
)

// newTestRouter sets up a mux.Router with all routes, mirroring serve.go.
func newTestRouter(h *Handler, hub *EventHub) *mux.Router {
	r := mux.NewRouter()
	r.UseEncodedPath()
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

func TestDownloadAttachmentHandler_OpaqueIdentifier(t *testing.T) {
	db := newTestStore(t)
	msg := &store.Message{
		StableID: "email-1", MessageID: "duplicate@example.com", Subject: "First",
		FromAddr: "a@test", ToAddrs: "b@test", Account: "work", Mailbox: "ALL",
		UID: 41, Date: time.Now().Unix(), CreatedAt: time.Now().Unix(), FetchedBody: true,
	}
	if err := db.InsertMessage(msg); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if err := db.InsertAttachment(&store.Attachment{
		MessageDBID: msg.ID, PartID: 1, Filename: "opaque.txt", ContentType: "text/plain", Size: 7,
	}); err != nil {
		t.Fatalf("insert attachment: %v", err)
	}

	h := New(db, nil)
	h.SetFetcher(&mockFetcher{data: []byte("payload")})
	r := newTestRouter(h, nil)
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/messages/local%%3A%d/attachments/1", msg.ID), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "payload" {
		t.Errorf("body = %q, want payload", got)
	}
}

func TestDownloadAttachmentHandler_EncodedLegacyIdentifier(t *testing.T) {
	db := newTestStore(t)
	msg := &store.Message{
		MessageID: "part/percent%plus+@example.com", Subject: "Legacy",
		FromAddr: "a@test", ToAddrs: "b@test", Account: "work", Mailbox: "INBOX",
		UID: 42, Date: time.Now().Unix(), CreatedAt: time.Now().Unix(), FetchedBody: true,
	}
	if err := db.InsertMessage(msg); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if err := db.InsertAttachment(&store.Attachment{
		MessageDBID: msg.ID, PartID: 1, Filename: "legacy.txt", ContentType: "text/plain", Size: 7,
	}); err != nil {
		t.Fatalf("insert attachment: %v", err)
	}

	h := New(db, nil)
	h.SetFetcher(&mockFetcher{data: []byte("payload")})
	r := newTestRouter(h, nil)
	path := fmt.Sprintf("/api/v1/messages/%s/attachments/1", url.PathEscape(msg.MessageID))
	req := httptest.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "payload" {
		t.Errorf("body = %q, want payload", got)
	}
}

// --- Outbox ---

func TestOutboxEnqueueAndList(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	body := `{"idempotency_key":"send-action-1","from":"alice@x","to":["bob@x"],"subject":"Test","body":"Hello"}`
	req := httptest.NewRequest("POST", "/api/v1/outbox/send", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("enqueue status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	var enqResp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&enqResp); err != nil {
		t.Fatal(err)
	}
	if enqResp["ok"] != true {
		t.Errorf("enqueue ok = %v", enqResp["ok"])
	}
	firstID := enqResp["id"]
	req = httptest.NewRequest("POST", "/api/v1/outbox/send", strings.NewReader(body))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var retryResp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&retryResp); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || retryResp["id"] != firstID {
		t.Fatalf("idempotent enqueue retry status=%d response=%v, want original id %v", w.Code, retryResp, firstID)
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
	if messageID, ok := items[0]["message_id"].(string); !ok || !strings.HasPrefix(messageID, "<") || !strings.HasSuffix(messageID, "@x>") {
		t.Errorf("message_id = %#v, want durable sender-scoped correlation ID", items[0]["message_id"])
	}
	if inFlight, ok := items[0]["in_flight"].(bool); !ok || inFlight {
		t.Errorf("in_flight = %#v, want false", items[0]["in_flight"])
	}
	if confirmed, ok := items[0]["delivery_confirmed"].(bool); !ok || confirmed {
		t.Errorf("delivery_confirmed = %#v, want false", items[0]["delivery_confirmed"])
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

func TestOutboxEnqueue_MissingIdempotencyKey(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	body := `{"from":"alice@x","to":["bob@x"],"subject":"Test"}`
	req := httptest.NewRequest("POST", "/api/v1/outbox/send", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "idempotency_key") {
		t.Errorf("status = %d body=%q, want missing idempotency key error", w.Code, w.Body.String())
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

	body := `{"idempotency_key":"delayed-action","from":"alice@x","to":["bob@x"],"subject":"Delayed","delay_seconds":5}`
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

func TestOutboxDelete(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newTestRouter(h, nil)

	// Enqueue first
	body := `{"idempotency_key":"delete-action","from":"alice@x","to":["bob@x"],"subject":"Delete me"}`
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

func TestOutboxDeleteRejectsClaimedDelivery(t *testing.T) {
	db := newTestStore(t)
	h := New(db, nil)
	r := newTestRouter(h, nil)
	id, err := db.Enqueue(`{"from":"alice@x","to":["bob@x"],"subject":"Claimed"}`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if item, err := db.ClaimNextOutboxItem(); err != nil || item == nil || item.ID != id {
		t.Fatalf("claim = %#v, %v", item, err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("DELETE", fmt.Sprintf("/api/v1/outbox/%d", id), nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("Undo after claim status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	items, err := db.ListOutbox()
	if err != nil || len(items) != 1 || !items[0].InFlight {
		t.Fatalf("claimed row after Undo = %#v, %v", items, err)
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
