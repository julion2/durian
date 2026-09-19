package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/contacts"
	"github.com/julion2/durian/cli/internal/store"
)

// accountRow returns the exact stored row for one Message-ID and account.
func accountRow(t *testing.T, db *store.DB, messageID, account string) *store.Message {
	t.Helper()
	rows, err := db.GetAllByMessageID(messageID)
	if err != nil {
		t.Fatalf("get rows for %s: %v", messageID, err)
	}
	for _, row := range rows {
		if row.Account == account {
			return row
		}
	}
	t.Fatalf("no %s row for %s", account, messageID)
	return nil
}

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

func TestReactionEnqueueDerivesReplyFromTargetRow(t *testing.T) {
	db := newTestStore(t)
	for _, msg := range []*store.Message{
		{MessageID: "target@test", Account: "work", FromAddr: "Author <author@test>", Subject: "Meeting", Refs: "<root@test>", Date: 1, CreatedAt: 1},
		{MessageID: "target@test", Account: "personal", FromAddr: "wrong@test", Subject: "Wrong", Date: 1, CreatedAt: 1},
	} {
		if err := db.InsertMessage(msg); err != nil {
			t.Fatal(err)
		}
	}
	target := accountRow(t, db, "target@test", "work")
	if err := db.InsertHeader(target.ID, "reply-to", "Replies <reply@test>"); err != nil {
		t.Fatal(err)
	}
	h := New(db, nil)
	h.SetConfig(&config.Config{Accounts: []config.AccountConfig{{Name: "Work", Email: "me@work.test"}, {Name: "Personal", Email: "me@personal.test"}}})
	r := newTestRouter(h, nil)

	// The opaque identifier addresses one row, so the duplicate Message-ID in
	// the other account cannot be reacted to by mistake.
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/messages/local:%d/reactions", target.ID),
		strings.NewReader(`{"emoji":"\ud83d\udc4d","idempotency_key":"action-1"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var response struct {
		SendAfter int64  `json:"send_after"`
		Recipient string `json:"recipient"`
	}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	delay := response.SendAfter - time.Now().Unix()
	if delay < reactionSendDelay-1 || delay > reactionSendDelay {
		t.Errorf("reaction delay = %ds, want %ds", delay, reactionSendDelay)
	}
	if response.Recipient != `"Replies" <reply@test>` {
		t.Errorf("response recipient = %q", response.Recipient)
	}
	items, err := db.ListOutbox()
	if err != nil || len(items) != 1 {
		t.Fatalf("outbox = %v err=%v", items, err)
	}
	var draft OutboxDraft
	if err := json.Unmarshal([]byte(items[0].DraftJSON), &draft); err != nil {
		t.Fatal(err)
	}
	if draft.Kind != outboxKindReaction || draft.Account != "work" || draft.From != "me@work.test" ||
		len(draft.To) != 1 || draft.To[0] != `"Replies" <reply@test>` {
		t.Errorf("account-derived fields = %+v", draft)
	}
	if draft.Subject != "Re: Meeting" || draft.InReplyTo != "target@test" || draft.References != "<root@test> <target@test>" {
		t.Errorf("thread fields = %+v", draft)
	}
	if draft.MessageID == "" || draft.IdempotencyKey == "" {
		t.Errorf("durable correlation fields = %+v", draft)
	}

	// Replaying the same action must not enqueue a second emoji.
	replay := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/messages/local:%d/reactions", target.ID),
		strings.NewReader(`{"emoji":"\ud83d\udc4d","idempotency_key":"action-1"}`))
	replayRecorder := httptest.NewRecorder()
	r.ServeHTTP(replayRecorder, replay)
	if replayRecorder.Code != http.StatusOK {
		t.Fatalf("replay status = %d", replayRecorder.Code)
	}
	if items, err := db.ListOutbox(); err != nil || len(items) != 1 {
		t.Fatalf("replayed action duplicated the outbox: %v, %v", items, err)
	}
}

// TestReactionResolvesMissingReplyToViaBackend covers the rows the engine
// synced before Reply-To was a marker: no backfill ever writes one for them,
// so the first reaction fetches the message and indexes both markers itself.
func TestReactionResolvesMissingReplyToViaBackend(t *testing.T) {
	db := newTestStore(t)
	if err := db.InsertMessage(&store.Message{
		MessageID: "legacy@test", Account: "work", FromAddr: "Author <author@test>",
		Subject: "Old mail", Mailbox: "ALL", RemoteRef: "graph-42", Date: 1, CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	target := accountRow(t, db, "legacy@test", "work")
	raw := "From: Author <author@test>\r\n" +
		"Reply-To: Replies <reply@test>\r\n" +
		"Subject: Old mail\r\n\r\nbody\r\n"
	var fetched []backend.RemoteRef
	fake := &fakeBackend{fetchBody: func(ctx context.Context, ref backend.RemoteRef, w io.Writer) error {
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			t.Error("header fetch ran without a deadline")
		}
		fetched = append(fetched, ref)
		_, err := io.WriteString(w, raw)
		return err
	}}
	h := New(db, nil)
	h.SetConfig(&config.Config{Accounts: []config.AccountConfig{{Name: "Work", Email: "me@work.test"}}})
	h.newBackend = func(*config.AccountConfig) (backend.Backend, error) { return fake, nil }
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/messages/local:%d/reactions", target.ID),
		strings.NewReader(`{"emoji":"\ud83d\udc4d","idempotency_key":"resolve-1"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if len(fetched) != 1 || fetched[0].Folder != "ALL" || fetched[0].ID != "graph-42" || fetched[0].MessageID != "legacy@test" {
		t.Fatalf("backend fetches = %+v", fetched)
	}
	if !fake.closed {
		t.Error("backend was not closed after the header fetch")
	}

	// Both markers are now durable, exactly as Ingest writes them — the empty
	// Content-Disposition records that it was inspected and absent.
	for _, marker := range []string{"reply-to", "content-disposition"} {
		indexed, err := db.HasHeader(target.ID, marker)
		if err != nil || !indexed {
			t.Fatalf("%s indexed = %v, err = %v", marker, indexed, err)
		}
	}
	if got, err := db.GetHeader(target.ID, "content-disposition"); err != nil || got != "" {
		t.Errorf("content-disposition = %q, err = %v", got, err)
	}

	// The fetched Reply-To, not the From address, is the reply recipient.
	items, err := db.ListOutbox()
	if err != nil || len(items) != 1 {
		t.Fatalf("outbox = %v err=%v", items, err)
	}
	var draft OutboxDraft
	if err := json.Unmarshal([]byte(items[0].DraftJSON), &draft); err != nil {
		t.Fatal(err)
	}
	if len(draft.To) != 1 || draft.To[0] != `"Replies" <reply@test>` {
		t.Fatalf("resolved recipient = %v", draft.To)
	}

	// A second reaction reuses the stored markers rather than the provider.
	second := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/messages/local:%d/reactions", target.ID),
		strings.NewReader(`{"emoji":"\u2764\ufe0f","idempotency_key":"resolve-2"}`))
	secondRecorder := httptest.NewRecorder()
	r.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusOK {
		t.Fatalf("second status = %d body=%s", secondRecorder.Code, secondRecorder.Body.String())
	}
	if len(fetched) != 1 {
		t.Fatalf("second reaction refetched headers: %+v", fetched)
	}
}

// TestReactionReportsFailedHeaderResolution proves every way the on-demand
// header fetch can fail is reported to the client and enqueues nothing, so
// the user never sees a silently dropped reaction — or, worse, one sent after
// the GUI told them it failed. The deadline case is the one that matters
// most: imapbackend.FetchBody ignores its context, so the handler has to
// enforce the budget itself instead of trusting the provider call to return.
func TestReactionReportsFailedHeaderResolution(t *testing.T) {
	cases := []struct {
		name       string
		fetchBody  func(context.Context, backend.RemoteRef, io.Writer) error
		wantStatus int
	}{
		{
			name: "provider error",
			fetchBody: func(context.Context, backend.RemoteRef, io.Writer) error {
				return errors.New("provider unavailable")
			},
			wantStatus: http.StatusBadGateway,
		},
		{
			// A body that is not a message must not be recorded as "headers
			// inspected, none present": that would route the reaction to From
			// on the strength of garbage.
			name: "unparseable body",
			fetchBody: func(_ context.Context, _ backend.RemoteRef, w io.Writer) error {
				_, err := io.WriteString(w, "not a message")
				return err
			},
			wantStatus: http.StatusBadGateway,
		},
		{
			// The provider call never returns on its own and never looks at
			// ctx, which is exactly how imapbackend.FetchBody behaves. The
			// handler must still answer within its own budget.
			name: "fetch ignores the deadline",
			fetchBody: func(_ context.Context, _ backend.RemoteRef, _ io.Writer) error {
				time.Sleep(1500 * time.Millisecond)
				return nil
			},
			wantStatus: http.StatusBadGateway,
		},
		{
			name: "message gone from the server",
			fetchBody: func(context.Context, backend.RemoteRef, io.Writer) error {
				return fmt.Errorf("%w: graph-99", backend.ErrRefGone)
			},
			wantStatus: http.StatusConflict,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Keep the deadline case fast; the value is irrelevant to the rest.
			previous := reactionHeaderFetchTimeout
			reactionHeaderFetchTimeout = 50 * time.Millisecond
			t.Cleanup(func() { reactionHeaderFetchTimeout = previous })

			db := newTestStore(t)
			if err := db.InsertMessage(&store.Message{
				MessageID: "unreachable@test", Account: "work", FromAddr: "author@test",
				Subject: "Old mail", Mailbox: "ALL", RemoteRef: "graph-99", Date: 1, CreatedAt: 1,
			}); err != nil {
				t.Fatal(err)
			}
			target := accountRow(t, db, "unreachable@test", "work")
			h := New(db, nil)
			h.SetConfig(&config.Config{Accounts: []config.AccountConfig{{Name: "Work", Email: "me@work.test"}}})
			closed := make(chan struct{}, 1)
			h.newBackend = func(*config.AccountConfig) (backend.Backend, error) {
				return &fakeBackend{fetchBody: tc.fetchBody, onClose: func() { closed <- struct{}{} }}, nil
			}
			r := newTestRouter(h, nil)

			req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/messages/local:%d/reactions", target.ID),
				strings.NewReader(`{"emoji":"\ud83d\udc4d","idempotency_key":"unreachable-1"}`))
			w := httptest.NewRecorder()
			started := time.Now()
			r.ServeHTTP(w, req)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("handler took %v; the fetch budget was not enforced", elapsed)
			}
			if strings.Contains(w.Body.String(), "backfill") {
				t.Errorf("error still tells the user to run a backfill: %s", w.Body.String())
			}
			if items, err := db.ListOutbox(); err != nil || len(items) != 0 {
				t.Fatalf("failed resolution enqueued %v (err %v)", items, err)
			}
			// A failed fetch must not leave a marker claiming the header was seen.
			for _, marker := range []string{"reply-to", "content-disposition"} {
				if indexed, err := db.HasHeader(target.ID, marker); err != nil || indexed {
					t.Fatalf("%s indexed after a failed fetch = %v, err = %v", marker, indexed, err)
				}
			}
			// The connection is released even when the provider call outlived
			// the handler's answer.
			select {
			case <-closed:
			case <-time.After(5 * time.Second):
				t.Fatal("backend was never closed")
			}
		})
	}
}

// TestReactionNotQueuedAfterClientGaveUp proves a reaction whose client
// disconnected during the header fetch is dropped rather than queued: the GUI
// has already shown "Reaction Not Queued" and cleared its pending state, so a
// late enqueue would send an emoji the user believes was never sent.
func TestReactionNotQueuedAfterClientGaveUp(t *testing.T) {
	db := newTestStore(t)
	if err := db.InsertMessage(&store.Message{
		MessageID: "slow@test", Account: "work", FromAddr: "author@test",
		Subject: "Old mail", Mailbox: "ALL", RemoteRef: "graph-7", Date: 1, CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	target := accountRow(t, db, "slow@test", "work")
	h := New(db, nil)
	h.SetConfig(&config.Config{Accounts: []config.AccountConfig{{Name: "Work", Email: "me@work.test"}}})
	ctx, clientGaveUp := context.WithCancel(context.Background())
	h.newBackend = func(*config.AccountConfig) (backend.Backend, error) {
		return &fakeBackend{fetchBody: func(_ context.Context, _ backend.RemoteRef, w io.Writer) error {
			// The fetch itself succeeds, but only after the client is gone.
			clientGaveUp()
			_, err := io.WriteString(w, "From: author@test\r\nSubject: Old mail\r\n\r\nbody\r\n")
			return err
		}}, nil
	}
	r := newTestRouter(h, nil)

	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/messages/local:%d/reactions", target.ID),
		strings.NewReader(`{"emoji":"\ud83d\udc4d","idempotency_key":"slow-1"}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatalf("reaction was queued for a client that had gone away: %s", w.Body.String())
	}
	if items, err := db.ListOutbox(); err != nil || len(items) != 0 {
		t.Fatalf("outbox after client disconnect = %v (err %v)", items, err)
	}
}

func TestReactionRejectsUnsupportedWrongAccountAndUnfetchableReplyTo(t *testing.T) {
	db := newTestStore(t)
	if err := db.InsertMessage(&store.Message{MessageID: "target@test", Account: "work", FromAddr: "author@test", Subject: "Hi", Date: 1, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	target := accountRow(t, db, "target@test", "work")
	h := New(db, nil)
	h.SetConfig(&config.Config{Accounts: []config.AccountConfig{{Name: "Work", Email: "me@work.test"}, {Name: "Personal", Email: "me@personal.test"}}})
	r := newTestRouter(h, nil)
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/messages/local:%d/reactions", target.ID), strings.NewReader(body))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	if got := post(`{"emoji":"\ud83d\udd25"}`); got.Code != http.StatusBadRequest {
		t.Errorf("unsupported emoji status = %d", got.Code)
	}
	if got := post(`{"account":"personal","emoji":"\ud83d\udc4d"}`); got.Code != http.StatusNotFound {
		t.Errorf("wrong account status = %d", got.Code)
	}
	// A legacy IMAP row carries no RemoteRef, so there is no provider handle
	// to fetch its headers with. The IMAP syncer backfills those markers on
	// its own, so this stays a 409 rather than a failed fetch.
	if got := post(`{"emoji":"\ud83d\udc4d"}`); got.Code != http.StatusConflict {
		t.Fatalf("unfetchable Reply-To response = %d %s", got.Code, got.Body.String())
	}
	if err := db.InsertHeader(target.ID, "reply-to", ""); err != nil {
		t.Fatal(err)
	}
	// An indexed but empty Reply-To means the reply goes to From.
	if got := post(`{"emoji":"\ud83d\udc4d","idempotency_key":"first"}`); got.Code != http.StatusOK {
		t.Fatalf("first status = %d", got.Code)
	}
	items, err := db.ListOutbox()
	if err != nil || len(items) != 1 {
		t.Fatalf("outbox = %v err=%v", items, err)
	}
	var fallbackDraft OutboxDraft
	if err := json.Unmarshal([]byte(items[0].DraftJSON), &fallbackDraft); err != nil {
		t.Fatal(err)
	}
	if len(fallbackDraft.To) != 1 || fallbackDraft.To[0] != "<author@test>" {
		t.Fatalf("From fallback recipient = %v", fallbackDraft.To)
	}
	req := httptest.NewRequest("DELETE", fmt.Sprintf("/api/v1/outbox/%d", items[0].ID), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("undo status = %d", w.Code)
	}
	// Reacting again after an Undo is a new action, so it must queue again.
	if got := post(`{"emoji":"\ud83d\udc4d","idempotency_key":"second"}`); got.Code != http.StatusOK {
		t.Errorf("status after undo = %d, want 200", got.Code)
	}
	if items, err := db.ListOutbox(); err != nil || len(items) != 1 {
		t.Fatalf("re-reaction after undo = %v, %v", items, err)
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
