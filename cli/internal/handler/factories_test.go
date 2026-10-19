package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/mailsend"
	"github.com/julion2/durian/cli/internal/store"
)

func TestBackendFactorySetterDrivesReactionHeaderFetch(t *testing.T) {
	db := newTestStore(t)
	if err := db.InsertMessage(&store.Message{MessageID: "factory@test", Account: "demo", FromAddr: "author@example.test",
		Mailbox: "ALL", RemoteRef: "remote-1", Subject: "Demo", Date: 1, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	row := accountRow(t, db, "factory@test", "demo")
	h := New(db, nil)
	h.SetConfig(&config.Config{Accounts: []config.AccountConfig{{Name: "Demo", Email: "demo@example.test"}}})
	calls := 0
	h.SetBackendFactory(func(a *config.AccountConfig) (backend.Backend, error) {
		calls++
		if a.Email != "demo@example.test" {
			t.Error("factory received wrong account")
		}
		return &fakeBackend{fetchBody: func(_ context.Context, ref backend.RemoteRef, w io.Writer) error {
			if ref.ID != "remote-1" {
				t.Error("wrong remote message")
			}
			_, err := io.WriteString(w, "From: author@example.test\r\nReply-To: reply@example.test\r\n\r\nbody")
			return err
		}}, nil
	})
	r := newTestRouter(h, nil)
	recorder := httptest.NewRecorder()
	r.ServeHTTP(recorder, httptest.NewRequest("POST", fmt.Sprintf("/api/v1/messages/local:%d/reactions", row.ID),
		strings.NewReader(`{"emoji":"👍","idempotency_key":"factory-reaction"}`)))
	items, err := db.ListOutbox()
	if recorder.Code != http.StatusOK || err != nil || len(items) != 1 || calls != 1 {
		t.Fatal("public reaction route did not use injected backend")
	}
	var draft OutboxDraft
	if err := json.Unmarshal([]byte(items[0].DraftJSON), &draft); err != nil {
		t.Fatal(err)
	}
	if len(draft.To) != 1 || !strings.Contains(draft.To[0], "reply@example.test") {
		t.Fatal("injected Reply-To was not used")
	}
}

type durableFactorySender struct {
	send func(*mailsend.Message, func(string) error) error
}

func (s *durableFactorySender) Send(context.Context, *mailsend.Message) error {
	return fmt.Errorf("durable seam was lost")
}
func (s *durableFactorySender) SavesSentCopy() bool { return true }
func (s *durableFactorySender) SendAfterPersist(_ context.Context, m *mailsend.Message, persist func(string) error) error {
	return s.send(m, persist)
}

func TestOutboxSenderFactorySetterPreservesDurableProtocol(t *testing.T) {
	db := newTestStore(t)
	draft := OutboxDraft{From: "demo@example.test", To: []string{"peer@example.test"}, Body: "test", MessageID: "<local@example.test>"}
	data, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Enqueue(string(data), time.Now().Unix()-1); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "Demo", Email: "demo@example.test"}}}
	worker := NewOutboxWorker(db, cfg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls, sends := 0, 0
	worker.SetSenderFactory(func(a *config.AccountConfig) (mailsend.Sender, error) {
		calls++
		if a.Email != "demo@example.test" {
			t.Error("sender factory received wrong account")
		}
		return &durableFactorySender{send: func(msg *mailsend.Message, persist func(string) error) error {
			sends++
			defer cancel()
			if msg.MessageID != draft.MessageID || len(msg.To) != 1 || msg.To[0] != draft.To[0] {
				t.Error("outbox sender lost message fields")
			}
			if err := persist("<provider@example.test>"); err != nil {
				t.Error("outbox did not persist provider Message-ID")
			}
			return &mailsend.Error{Kind: mailsend.KindAmbiguous, Err: fmt.Errorf("fake uncertain result")}
		}}, nil
	})
	done := make(chan struct{})
	go func() { defer close(done); worker.Start(ctx) }()
	select {
	case <-done:
	case <-time.After(12 * time.Second):
		cancel()
		<-done
		t.Fatal("injected sender was not reached by public worker loop")
	}
	items, err := db.ListOutbox()
	if err != nil || len(items) != 1 || calls != 1 || sends != 1 || !items[0].InFlight {
		t.Fatal("injected durable send did not retain ambiguous claim")
	}
	var persisted OutboxDraft
	if json.Unmarshal([]byte(items[0].DraftJSON), &persisted) != nil || persisted.MessageID != "<provider@example.test>" {
		t.Fatal("provider Message-ID was not durable before uncertain send")
	}
}

type cancellationSender struct{ entered chan struct{} }

func (s *cancellationSender) SavesSentCopy() bool { return true }
func (s *cancellationSender) Send(ctx context.Context, _ *mailsend.Message) error {
	close(s.entered)
	<-ctx.Done()
	return &mailsend.Error{Kind: mailsend.KindAmbiguous, Err: ctx.Err()}
}

func TestOutboxCancellationStopsInFlightSendAndFurtherClaims(t *testing.T) {
	db := newTestStore(t)
	draft, err := json.Marshal(OutboxDraft{From: "demo@example.test", To: []string{"peer@example.test"}, Body: "test", MessageID: "<cancel@example.test>"})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := db.Enqueue(string(draft), time.Now().Unix()-1); err != nil {
			t.Fatal(err)
		}
	}
	worker := NewOutboxWorker(db, &config.Config{Accounts: []config.AccountConfig{{Name: "Demo", Email: "demo@example.test"}}}, nil)
	s := &cancellationSender{entered: make(chan struct{})}
	worker.SetSenderFactory(func(*config.AccountConfig) (mailsend.Sender, error) { return s, nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); worker.Start(ctx) }()
	select {
	case <-s.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not start send")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("in-flight send did not inherit cancellation")
	}
	items, err := db.ListOutbox()
	if err != nil || len(items) != 2 {
		t.Fatal("cancellation lost queued items")
	}
	claimed := 0
	for _, item := range items {
		if item.InFlight {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatal("cancellation lost ambiguous claim or claimed another item")
	}
}
