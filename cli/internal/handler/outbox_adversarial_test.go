package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/dbcrypto"
	"github.com/julion2/durian/cli/internal/mailsend"
	"github.com/julion2/durian/cli/internal/store"
)

// adversarialCaptureSender models the irreversible provider boundary without
// touching a real account. A message is captured before result is returned, so
// an ambiguous result accurately means that the provider may have accepted it.
type adversarialCaptureSender struct {
	mu       sync.Mutex
	messages []*mailsend.Message
	result   error
}

func (s *adversarialCaptureSender) Send(_ context.Context, msg *mailsend.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := *msg
	copy.To = append([]string(nil), msg.To...)
	copy.CC = append([]string(nil), msg.CC...)
	copy.BCC = append([]string(nil), msg.BCC...)
	s.messages = append(s.messages, &copy)
	return s.result
}

func (*adversarialCaptureSender) SavesSentCopy() bool { return false }

func (s *adversarialCaptureSender) captured() []*mailsend.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*mailsend.Message(nil), s.messages...)
}

func newAdversarialHandlerStore(t *testing.T) (*store.DB, string, *dbcrypto.Keyring) {
	t.Helper()
	keyring, err := dbcrypto.NewKeyring(bytes.Repeat([]byte{0x5a}, dbcrypto.MasterKeyLen))
	if err != nil {
		t.Fatalf("create keyring: %v", err)
	}
	path := filepath.Join(t.TempDir(), "mail.db")
	db, err := store.Open(path, keyring)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := db.Init(); err != nil {
		_ = db.Close()
		t.Fatalf("init store: %v", err)
	}
	return db, path, keyring
}

func reopenAdversarialHandlerStore(t *testing.T, path string, keyring *dbcrypto.Keyring) *store.DB {
	t.Helper()
	db, err := store.Open(path, keyring)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	if err := db.Init(); err != nil {
		_ = db.Close()
		t.Fatalf("reinit store: %v", err)
	}
	return db
}

func messageFromAdversarialDraft(draft *OutboxDraft) *mailsend.Message {
	return &mailsend.Message{
		MessageID:  draft.MessageID,
		From:       draft.From,
		To:         draft.To,
		CC:         draft.CC,
		BCC:        draft.BCC,
		Subject:    draft.Subject,
		Body:       draft.Body,
		InReplyTo:  draft.InReplyTo,
		References: draft.References,
	}
}

func TestAdversarialLostEnqueueResponseRetryAfterCompletionSendsOnce(t *testing.T) {
	db, path, keyring := newAdversarialHandlerStore(t)
	bodyBytes, err := json.Marshal(OutboxDraft{
		IdempotencyKey: "lost-response-action",
		From:           "alice@example.test",
		To:             []string{"bob@example.test"},
		Subject:        "Re: retained",
		Body:           "one reply",
		InReplyTo:      "<parent@example.test>",
		References:     "<root@example.test> <parent@example.test>",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := string(bodyBytes)

	// The first request commits, but its response is deliberately discarded.
	first := httptest.NewRecorder()
	New(db, nil).EnqueueOutboxHandler(first, httptest.NewRequest(http.MethodPost, "/api/v1/outbox/send", strings.NewReader(body)))
	if first.Code != http.StatusOK {
		t.Fatalf("initial enqueue status = %d, body=%s", first.Code, first.Body.String())
	}
	items, err := db.ListOutbox()
	if err != nil || len(items) != 1 {
		t.Fatalf("committed outbox after lost response = %#v, %v", items, err)
	}
	originalID := items[0].ID

	item, err := db.ClaimNextOutboxItem()
	if err != nil || item == nil || item.ID != originalID {
		t.Fatalf("claim committed send = %#v, %v", item, err)
	}
	var draft OutboxDraft
	if err := json.Unmarshal([]byte(item.DraftJSON), &draft); err != nil {
		t.Fatalf("decode queued draft: %v", err)
	}
	capture := &adversarialCaptureSender{}
	if err := capture.Send(context.Background(), messageFromAdversarialDraft(&draft)); err != nil {
		t.Fatalf("capture send: %v", err)
	}
	if err := db.MarkOutboxDeliveryConfirmed(item.ID, ""); err != nil {
		t.Fatalf("confirm delivery: %v", err)
	}
	if err := db.DeleteClaimedOutboxItem(item.ID); err != nil {
		t.Fatalf("complete delivery: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close first process: %v", err)
	}

	// A restarted client retries the exact action because it never received the
	// first HTTP response. The durable idempotency tombstone must not enqueue it.
	db = reopenAdversarialHandlerStore(t, path, keyring)
	defer db.Close()
	retry := httptest.NewRecorder()
	New(db, nil).EnqueueOutboxHandler(retry, httptest.NewRequest(http.MethodPost, "/api/v1/outbox/send", strings.NewReader(body)))
	if retry.Code != http.StatusOK {
		t.Fatalf("retry status = %d, body=%s", retry.Code, retry.Body.String())
	}
	var response struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(retry.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode retry response: %v", err)
	}
	if response.ID != originalID {
		t.Fatalf("retry returned id %d, want completed id %d", response.ID, originalID)
	}
	if items, err := db.ListOutbox(); err != nil || len(items) != 0 {
		t.Fatalf("retry recreated a deliverable row: %#v, %v", items, err)
	}
	captured := capture.captured()
	if len(captured) != 1 {
		t.Fatalf("provider submissions = %d, want exactly one", len(captured))
	}
	if captured[0].Body != "one reply" || captured[0].InReplyTo != "<parent@example.test>" || captured[0].References != "<root@example.test> <parent@example.test>" {
		t.Fatalf("captured reply lost payload/threading: %#v", captured[0])
	}
}

func TestAdversarialAmbiguousProviderAcceptanceStaysClaimedAcrossRestart(t *testing.T) {
	db, path, keyring := newAdversarialHandlerStore(t)
	draft := OutboxDraft{
		MessageID: "<ambiguous@example.test>", From: "alice@example.test",
		To: []string{"bob@example.test"}, Subject: "Re: ambiguous", Body: "possibly delivered",
		InReplyTo: "<parent@example.test>", References: "<root@example.test> <parent@example.test>",
	}
	payload, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.Enqueue(string(payload), 0)
	if err != nil {
		t.Fatal(err)
	}
	item, err := db.ClaimNextOutboxItem()
	if err != nil || item == nil || item.ID != id {
		t.Fatalf("claim = %#v, %v", item, err)
	}
	capture := &adversarialCaptureSender{result: &mailsend.Error{Kind: mailsend.KindAmbiguous, Err: errors.New("connection lost after DATA")}}
	sendErr := capture.Send(context.Background(), messageFromAdversarialDraft(&draft))
	if sendErr == nil {
		t.Fatal("fake provider did not return its ambiguous completion")
	}
	if !NewOutboxWorker(db, nil, nil).handleSendError(item, &draft, sendErr) {
		t.Fatal("worker stopped without preserving ambiguous state")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db = reopenAdversarialHandlerStore(t, path, keyring)
	defer db.Close()
	if next, err := db.ClaimNextOutboxItem(); err != nil || next != nil {
		t.Fatalf("restart automatically resubmitted ambiguous delivery: %#v, %v", next, err)
	}
	items, err := db.ListOutbox()
	if err != nil || len(items) != 1 {
		t.Fatalf("retained ambiguous row = %#v, %v", items, err)
	}
	if !items[0].InFlight || items[0].DeliveryConfirmed || items[0].DraftJSON != string(payload) || !strings.Contains(items[0].LastError, "Verify the provider outcome") {
		t.Fatalf("ambiguous state/payload was not surfaced intact: %#v", items[0])
	}
	if got := len(capture.captured()); got != 1 {
		t.Fatalf("provider submissions after restart = %d, want one", got)
	}
}

func TestAdversarialAcceptedMessageSentFilingFailureSurvivesRestart(t *testing.T) {
	db, path, keyring := newAdversarialHandlerStore(t)
	draft := OutboxDraft{
		MessageID: "<accepted@example.test>", From: "alice@example.test",
		To: []string{"bob@example.test"}, Subject: "accepted", Body: "durable payload",
	}
	payload, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.Enqueue(string(payload), 0)
	if err != nil {
		t.Fatal(err)
	}
	item, err := db.ClaimNextOutboxItem()
	if err != nil || item == nil || item.ID != id {
		t.Fatalf("claim = %#v, %v", item, err)
	}

	capture := &adversarialCaptureSender{}
	msg := messageFromAdversarialDraft(&draft)
	if err := capture.Send(context.Background(), msg); err != nil {
		t.Fatalf("provider acceptance: %v", err)
	}
	worker := NewOutboxWorker(db, nil, nil)
	if err := db.MarkOutboxDeliveryConfirmed(id, ""); err != nil {
		t.Fatalf("persist provider acceptance: %v", err)
	}
	account := &config.AccountConfig{
		Name: "work", Email: draft.From,
		IMAP: config.IMAPConfig{Host: "127.0.0.1", Port: 1},
	}
	if err := worker.saveToLocalStore(account, msg, &draft); err != nil {
		t.Fatalf("local Sent projection unexpectedly failed before filing test: %v", err)
	}
	if err := worker.appendToSent(account, msg, capture.SavesSentCopy()); err == nil {
		t.Fatal("closed loopback IMAP endpoint unexpectedly accepted Sent filing")
	}
	const warning = "Message was delivered, but filing it in Sent requires manual remediation."
	if err := db.MarkOutboxDeliveryConfirmed(id, warning); err != nil {
		t.Fatalf("retain Sent filing failure: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db = reopenAdversarialHandlerStore(t, path, keyring)
	defer db.Close()
	if next, err := db.ClaimNextOutboxItem(); err != nil || next != nil {
		t.Fatalf("restart redelivered accepted message: %#v, %v", next, err)
	}
	items, err := db.ListOutbox()
	if err != nil || len(items) != 1 {
		t.Fatalf("retained accepted row = %#v, %v", items, err)
	}
	if !items[0].InFlight || !items[0].DeliveryConfirmed || items[0].DraftJSON != string(payload) || items[0].LastError != warning {
		t.Fatalf("accepted filing-failure state/payload = %#v", items[0])
	}
	if err := db.RequeueClaimedOutboxItem(id, "retry"); !errors.Is(err, store.ErrOutboxDeliveryConfirmed) {
		t.Fatalf("accepted message was requeueable: %v", err)
	}
	if got := len(capture.captured()); got != 1 {
		t.Fatalf("provider submissions after restart = %d, want one", got)
	}
	stored, err := db.GetByMessageID(strings.Trim(draft.MessageID, "<>"))
	if err != nil || stored == nil || stored.BodyText != draft.Body || stored.Flags != `\Seen` {
		t.Fatalf("local Sent projection = %#v, %v", stored, err)
	}

	// The HTTP state exposed to the GUI must distinguish this from an unsent
	// failure, not merely retain an internal SQL bit.
	recorder := httptest.NewRecorder()
	New(db, nil).ListOutboxHandler(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/outbox", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "\"delivery_confirmed\":true") || !strings.Contains(recorder.Body.String(), warning) || !strings.Contains(recorder.Body.String(), strconv.FormatInt(id, 10)) {
		t.Fatalf("surfaced outbox state status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
