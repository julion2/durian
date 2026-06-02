package handler

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/dbcrypto"
	"github.com/julion2/durian/cli/internal/protocol"
	"github.com/julion2/durian/cli/internal/store"
)

// mockFetcher implements AttachmentFetcher for testing.
type mockFetcher struct {
	data []byte
}

func (m *mockFetcher) FetchAttachment(_ context.Context, _, _ string,
	_ uint32, _, _, _ string, _ int, w io.Writer) error {
	_, err := w.Write(m.data)
	return err
}

// --- Store-backed handler tests ---

func newTestStore(t *testing.T) *store.DB {
	t.Helper()
	kr, err := dbcrypto.NewKeyring(bytes.Repeat([]byte{0x42}, dbcrypto.MasterKeyLen))
	if err != nil {
		t.Fatalf("test keyring: %v", err)
	}
	db, err := store.Open(":memory:", kr)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := db.Init(); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func seedStoreData(t *testing.T, db *store.DB) {
	t.Helper()
	now := time.Now().Unix()

	msgs := []*store.Message{
		{
			MessageID: "msg1@test", Subject: "Hello World",
			FromAddr: "alice@example.com", ToAddrs: "bob@example.com",
			Date: now - 3600, CreatedAt: now, BodyText: "First message body",
			BodyHTML: "<p>First message body</p>", Mailbox: "INBOX", FetchedBody: true,
		},
		{
			MessageID: "msg2@test", Subject: "Re: Hello World",
			FromAddr: "bob@example.com", ToAddrs: "alice@example.com",
			InReplyTo: "<msg1@test>", Refs: "<msg1@test>",
			Date: now, CreatedAt: now, BodyText: "Reply body",
			Mailbox: "INBOX", FetchedBody: true,
		},
		{
			MessageID: "msg3@test", Subject: "Other Thread",
			FromAddr: "charlie@example.com", ToAddrs: "alice@example.com",
			Date: now - 7200, CreatedAt: now, BodyText: "Different thread",
			Mailbox: "INBOX", FetchedBody: true,
		},
	}

	for _, msg := range msgs {
		if err := db.InsertMessage(msg); err != nil {
			t.Fatalf("insert %s: %v", msg.MessageID, err)
		}
	}

	// Add tags
	m1, _ := db.GetByMessageID("msg1@test")
	m2, _ := db.GetByMessageID("msg2@test")
	m3, _ := db.GetByMessageID("msg3@test")

	db.AddTag(m1.ID, "inbox")
	db.AddTag(m1.ID, "unread")
	db.AddTag(m2.ID, "inbox")
	db.AddTag(m3.ID, "inbox")
	db.AddTag(m3.ID, "flagged")
}

func TestNew(t *testing.T) {
	db := newTestStore(t)

	h := New(db, nil)

	if h.store != db {
		t.Error("store should be set")
	}
	if h.parser == nil {
		t.Error("parser should not be nil")
	}
}

func TestHandleDispatch(t *testing.T) {
	db := newTestStore(t)
	seedStoreData(t, db)

	h := New(db, nil)

	t.Run("search", func(t *testing.T) {
		cmd := protocol.Command{Cmd: "search", Query: "tag:inbox", Limit: 10}
		resp := h.Handle(cmd)
		if !resp.OK {
			t.Errorf("Handle(search) should return OK, got error: %s", resp.Error)
		}
		if len(resp.Results) == 0 {
			t.Error("expected search results")
		}
	})

	t.Run("show thread", func(t *testing.T) {
		m1, _ := db.GetByMessageID("msg1@test")
		cmd := protocol.Command{Cmd: "show", Thread: m1.ThreadID}
		resp := h.Handle(cmd)
		if !resp.OK {
			t.Errorf("Handle(show) should return OK, got error: %s", resp.Error)
		}
		if resp.Thread == nil {
			t.Error("expected thread content")
		}
	})

	t.Run("tag", func(t *testing.T) {
		m1, _ := db.GetByMessageID("msg1@test")
		cmd := protocol.Command{Cmd: "tag", Query: "thread:" + m1.ThreadID, Tags: "+archived"}
		resp := h.Handle(cmd)
		if !resp.OK {
			t.Errorf("Handle(tag) should return OK, got error: %s", resp.Error)
		}
	})

	t.Run("unknown command", func(t *testing.T) {
		cmd := protocol.Command{Cmd: "invalid_command"}
		resp := h.Handle(cmd)
		if resp.OK {
			t.Error("Handle() should return error for unknown command")
		}
		if resp.ErrorCode != protocol.ErrUnknownCmd {
			t.Errorf("ErrorCode = %q, want %q", resp.ErrorCode, protocol.ErrUnknownCmd)
		}
	})
}

func TestStoreSearch(t *testing.T) {
	db := newTestStore(t)
	seedStoreData(t, db)

	h := New(db, nil)
	resp := h.Search("tag:inbox", 10, 0)

	if !resp.OK {
		t.Fatalf("Search failed: %s", resp.Error)
	}
	if len(resp.Results) == 0 {
		t.Fatal("expected results from store search")
	}
}

func TestStoreSearchWithEnrichment(t *testing.T) {
	db := newTestStore(t)
	seedStoreData(t, db)

	h := New(db, nil)
	resp := h.Search("tag:inbox", 10, 5)

	if !resp.OK {
		t.Fatalf("Search failed: %s", resp.Error)
	}
	if len(resp.Results) == 0 {
		t.Fatal("expected results")
	}
	if len(resp.Threads) == 0 {
		t.Error("expected enriched threads")
	}
}

func TestStoreShowThread(t *testing.T) {
	db := newTestStore(t)
	seedStoreData(t, db)

	// Get the thread ID for msg1 (which shares a thread with msg2)
	m1, _ := db.GetByMessageID("msg1@test")

	h := New(db, nil)
	resp := h.ShowThread(m1.ThreadID)

	if !resp.OK {
		t.Fatalf("ShowThread failed: %s", resp.Error)
	}
	if resp.Thread == nil {
		t.Fatal("Thread should not be nil")
	}
	if len(resp.Thread.Messages) != 2 {
		t.Errorf("expected 2 messages in thread, got %d", len(resp.Thread.Messages))
	}
	if resp.Thread.Subject != "Hello World" {
		t.Errorf("Subject = %q, want %q", resp.Thread.Subject, "Hello World")
	}

	// Verify messages have tags
	foundTags := false
	for _, msg := range resp.Thread.Messages {
		if len(msg.Tags) > 0 {
			foundTags = true
			break
		}
	}
	if !foundTags {
		t.Error("expected messages to have tags")
	}
}

func TestStoreShowThreadNotFound(t *testing.T) {
	db := newTestStore(t)

	h := New(db, nil)
	resp := h.ShowThread("nonexistent")

	if resp.OK {
		t.Error("should fail for nonexistent thread")
	}
	if resp.ErrorCode != protocol.ErrNotFound {
		t.Errorf("ErrorCode = %q, want %q", resp.ErrorCode, protocol.ErrNotFound)
	}
}

func TestStoreShowMessageBody(t *testing.T) {
	db := newTestStore(t)
	seedStoreData(t, db)

	h := New(db, nil)
	resp := h.ShowMessageBody("msg1@test")

	if !resp.OK {
		t.Fatalf("ShowMessageBody failed: %s", resp.Error)
	}
	if resp.MessageBody == nil {
		t.Fatal("MessageBody should not be nil")
	}
	if resp.MessageBody.Body != "First message body" {
		t.Errorf("Body = %q, want %q", resp.MessageBody.Body, "First message body")
	}
	if resp.MessageBody.HTML != "<p>First message body</p>" {
		t.Errorf("HTML = %q, want %q", resp.MessageBody.HTML, "<p>First message body</p>")
	}
}

func TestStoreShowMessageBodyNotFound(t *testing.T) {
	db := newTestStore(t)

	h := New(db, nil)
	resp := h.ShowMessageBody("nonexistent@test")

	if resp.OK {
		t.Error("should fail for nonexistent message")
	}
	if resp.ErrorCode != protocol.ErrNotFound {
		t.Errorf("ErrorCode = %q, want %q", resp.ErrorCode, protocol.ErrNotFound)
	}
}

func TestStoreTag(t *testing.T) {
	db := newTestStore(t)
	seedStoreData(t, db)

	m1, _ := db.GetByMessageID("msg1@test")
	h := New(db, nil)

	resp := h.Tag("thread:"+m1.ThreadID, "+archived -unread")

	if !resp.OK {
		t.Fatalf("Tag failed: %s", resp.Error)
	}

	// Store should be updated
	tags, err := db.GetTagsByMessageID("msg1@test")
	if err != nil {
		t.Fatalf("get tags: %v", err)
	}
	tagSet := make(map[string]bool)
	for _, tag := range tags {
		tagSet[tag] = true
	}
	if !tagSet["archived"] {
		t.Error("expected 'archived' tag in store")
	}
	if tagSet["unread"] {
		t.Error("'unread' should have been removed from store")
	}
	if !tagSet["inbox"] {
		t.Error("'inbox' should still be in store")
	}
}

func TestStoreTagBySearchQuery(t *testing.T) {
	db := newTestStore(t)
	seedStoreData(t, db)

	h := New(db, nil)
	resp := h.Tag("tag:inbox", "+archived")

	if !resp.OK {
		t.Errorf("tag by search query should succeed, got error: %s", resp.Error)
	}
}

func TestStoreListTags(t *testing.T) {
	db := newTestStore(t)
	seedStoreData(t, db)

	h := New(db, nil)
	resp := h.ListTags()

	if !resp.OK {
		t.Fatalf("ListTags failed: %s", resp.Error)
	}
	if len(resp.Tags) == 0 {
		t.Error("expected tags from store")
	}

	tagSet := make(map[string]bool)
	for _, tag := range resp.Tags {
		tagSet[tag] = true
	}
	if !tagSet["inbox"] || !tagSet["unread"] || !tagSet["flagged"] {
		t.Errorf("expected inbox, unread, flagged; got %v", resp.Tags)
	}
}

func TestEnforceExclusiveTags(t *testing.T) {
	tests := []struct {
		name       string
		add        []string
		remove     []string
		wantRemove map[string]bool
	}{
		{
			name:       "archive removes trash and inbox",
			add:        []string{"archive"},
			remove:     nil,
			wantRemove: map[string]bool{"trash": true, "inbox": true},
		},
		{
			name:       "trash removes archive and inbox",
			add:        []string{"trash"},
			remove:     nil,
			wantRemove: map[string]bool{"archive": true, "inbox": true},
		},
		{
			name:       "inbox removes archive and trash",
			add:        []string{"inbox"},
			remove:     nil,
			wantRemove: map[string]bool{"archive": true, "trash": true},
		},
		{
			name:       "no duplicates if already removing",
			add:        []string{"archive"},
			remove:     []string{"inbox"},
			wantRemove: map[string]bool{"inbox": true, "trash": true},
		},
		{
			name:       "non-exclusive tags unchanged",
			add:        []string{"flagged"},
			remove:     []string{"unread"},
			wantRemove: map[string]bool{"unread": true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, gotRemove := enforceExclusiveTags(tt.add, tt.remove)
			gotSet := make(map[string]bool, len(gotRemove))
			for _, r := range gotRemove {
				if gotSet[r] {
					t.Errorf("duplicate in remove: %q", r)
				}
				gotSet[r] = true
			}
			for want := range tt.wantRemove {
				if !gotSet[want] {
					t.Errorf("expected %q in remove, got %v", want, gotRemove)
				}
			}
			if len(gotSet) != len(tt.wantRemove) {
				t.Errorf("remove = %v, want keys %v", gotRemove, tt.wantRemove)
			}
		})
	}
}

func TestSplitTagOps(t *testing.T) {
	add, remove := splitTagOps([]string{"+read", "-unread", "+archived", "-inbox"})

	if len(add) != 2 || add[0] != "read" || add[1] != "archived" {
		t.Errorf("add = %v, want [read archived]", add)
	}
	if len(remove) != 2 || remove[0] != "unread" || remove[1] != "inbox" {
		t.Errorf("remove = %v, want [unread inbox]", remove)
	}
}

func TestStoreDownloadAttachment(t *testing.T) {
	db := newTestStore(t)
	msg := &store.Message{
		MessageID: "msg1@test", Subject: "Test",
		FromAddr: "a@test", ToAddrs: "b@test",
		Date: time.Now().Unix(), CreatedAt: time.Now().Unix(),
		Mailbox: "INBOX", FetchedBody: true,
		Account: "test-account", UID: 42,
	}
	if err := db.InsertMessage(msg); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if err := db.InsertAttachment(&store.Attachment{
		MessageDBID: msg.ID, PartID: 1,
		Filename: "report.pdf", ContentType: "application/pdf",
		Size: 100, Disposition: "attachment",
	}); err != nil {
		t.Fatalf("insert attachment: %v", err)
	}

	h := New(db, nil)
	h.SetFetcher(&mockFetcher{data: []byte("fake-pdf-bytes")})
	w := httptest.NewRecorder()

	err := h.DownloadAttachment("msg1@test", 1, w)
	if err != nil {
		t.Fatalf("DownloadAttachment failed: %v", err)
	}

	// Verify response headers
	if ct := w.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("Content-Type = %q, want application/pdf", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename="report.pdf"` {
		t.Errorf("Content-Disposition = %q, want attachment; filename=\"report.pdf\"", cd)
	}

	// Verify body streamed from fetcher
	if w.Body.String() != "fake-pdf-bytes" {
		t.Errorf("Body = %q, want fake-pdf-bytes", w.Body.String())
	}
}

func TestStoreDownloadAttachmentNoFetcher(t *testing.T) {
	db := newTestStore(t)
	msg := &store.Message{
		MessageID: "msg1@test", Subject: "Test",
		FromAddr: "a@test", ToAddrs: "b@test",
		Date: time.Now().Unix(), CreatedAt: time.Now().Unix(),
		Mailbox: "INBOX", FetchedBody: true,
	}
	if err := db.InsertMessage(msg); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if err := db.InsertAttachment(&store.Attachment{
		MessageDBID: msg.ID, PartID: 1,
		Filename: "report.pdf", ContentType: "application/pdf",
		Size: 100, Disposition: "attachment",
	}); err != nil {
		t.Fatalf("insert attachment: %v", err)
	}

	h := New(db, nil)
	w := httptest.NewRecorder()

	err := h.DownloadAttachment("msg1@test", 1, w)
	if err == nil {
		t.Error("expected error when no fetcher is set")
	}
}

func TestStoreDownloadAttachmentNotFound(t *testing.T) {
	db := newTestStore(t)

	h := New(db, nil)
	w := httptest.NewRecorder()

	err := h.DownloadAttachment("nonexistent@test", 1, w)
	if err == nil {
		t.Error("expected error for nonexistent attachment")
	}
}
