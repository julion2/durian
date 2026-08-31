package handler

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/dbcrypto"
	"github.com/julion2/durian/cli/internal/mailsend"
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

func TestOutboxSaveToLocalStoreMarksMessageSeen(t *testing.T) {
	db := newTestStore(t)
	worker := NewOutboxWorker(db, nil, nil)
	account := &config.AccountConfig{Name: "work", Email: "sender@example.com"}
	worker.saveToLocalStore(account,
		&mailsend.Message{MessageID: "<sent@example.com>"},
		&OutboxDraft{To: []string{"recipient@example.com"}, Subject: "Sent"})

	msg, err := db.GetByMessageID("sent@example.com")
	if err != nil {
		t.Fatalf("GetByMessageID: %v", err)
	}
	if msg == nil || msg.Flags != `\Seen` {
		t.Fatalf("saved message = %+v, want \\Seen", msg)
	}
}

func TestNewMailInfosUsesExactStableMessageIdentifiers(t *testing.T) {
	db := newTestStore(t)
	now := time.Now().Unix()
	first := &store.Message{
		StableID: "email-1", MessageID: "duplicate@example.com", Subject: "First",
		FromAddr: "First Sender <first@example.com>", Date: now, CreatedAt: now,
		BodyText: "first body", Account: "work", Mailbox: "ALL", FetchedBody: true,
	}
	second := &store.Message{
		StableID: "email-2", MessageID: "duplicate@example.com", Subject: "Second",
		FromAddr: "Second Sender <second@example.com>", Date: now + 1, CreatedAt: now,
		BodyText: "second body", Account: "work", Mailbox: "ALL", FetchedBody: true,
	}
	for _, msg := range []*store.Message{first, second} {
		if err := db.InsertMessage(msg); err != nil {
			t.Fatalf("insert %s: %v", msg.StableID, err)
		}
	}

	identifiers := []string{
		"local:" + strconv.FormatInt(second.ID, 10),
		"local:" + strconv.FormatInt(first.ID, 10),
	}
	infos := newMailInfos(db, "work", identifiers)
	if len(infos) != 2 {
		t.Fatalf("newMailInfos returned %d messages, want 2", len(infos))
	}
	if infos[0].Subject != "Second" || infos[0].From != "Second Sender" || infos[1].Subject != "First" {
		t.Fatalf("newMailInfos returned wrong duplicate rows: %+v", infos)
	}
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

	resp := h.Tag("thread:"+m1.ThreadID, "+archived -unread", nil)

	if !resp.OK {
		t.Fatalf("Tag failed: %s", resp.Error)
	}
	if resp.MatchedThreads == nil || resp.ChangedThreads == nil || *resp.MatchedThreads != 1 || *resp.ChangedThreads != 1 {
		t.Fatalf("tag effect = matched %v, changed %v; want 1, 1", resp.MatchedThreads, resp.ChangedThreads)
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
	resp := h.Tag("tag:inbox", "+archived", nil)

	if !resp.OK {
		t.Errorf("tag by search query should succeed, got error: %s", resp.Error)
	}
}

func TestStoreTagRejectsUnsupportedQueryWithoutChanges(t *testing.T) {
	db := newTestStore(t)
	seedStoreData(t, db)
	h := New(db, nil)

	resp := h.Tag("folder:INBOX", "+todo", nil)
	if resp.OK {
		t.Fatal("tag with unsupported query field succeeded")
	}
	if !strings.Contains(resp.Error, "not supported") {
		t.Fatalf("error = %q, want unsupported-field error", resp.Error)
	}

	resp = h.Search("tag:todo", 10, 0)
	if !resp.OK {
		t.Fatalf("search todo: %s", resp.Error)
	}
	if len(resp.Results) != 0 {
		t.Fatalf("todo results = %d, want 0", len(resp.Results))
	}
}

func TestStoreTagNoMatchesFails(t *testing.T) {
	db := newTestStore(t)
	seedStoreData(t, db)
	h := New(db, nil)

	resp := h.Tag("from:nobody@example.com", "+todo", nil)
	if resp.OK {
		t.Fatal("tag with no matches succeeded")
	}
	if resp.ErrorCode != protocol.ErrNotFound {
		t.Fatalf("error code = %q, want %q", resp.ErrorCode, protocol.ErrNotFound)
	}
}

func TestPreviewTagReportsEffectWithoutChanges(t *testing.T) {
	db := newTestStore(t)
	seedStoreData(t, db)
	h := New(db, nil)

	resp := h.PreviewTag("tag:inbox", "+todo", nil)
	if !resp.OK || resp.MatchedThreads == nil || resp.ChangedThreads == nil || *resp.ChangedThreads == 0 {
		t.Fatalf("preview effect = %+v", resp)
	}
	search := h.Search("tag:todo", 10, 0)
	if !search.OK || len(search.Results) != 0 {
		t.Fatalf("preview modified tags: %+v", search)
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

// TestTagAccountScopeStaysWithinSelectedAccounts is the contract the account
// filter used to break. It narrowed the thread search only: the mutation then
// ran thread-wide, so tagging in one account wrote into every other account
// sharing that thread — and the journal recorded those writes too, so the
// change propagated on the next sync.
//
// One resolved set of rows feeds the mutation, the journal and the trigger, so
// there is no second place for the scope to be decided differently.
func TestTagAccountScopeStaysWithinSelectedAccounts(t *testing.T) {
	db := newTestStore(t)
	now := time.Now().Unix()

	// The same conversation in two accounts: one thread, rows in both.
	for _, account := range []string{"work", "personal"} {
		if err := db.InsertMessage(&store.Message{
			MessageID: "shared-" + account + "@test", Subject: "Shared thread",
			FromAddr: "a@example.com", ToAddrs: "b@example.com",
			Refs: "<root@test>", InReplyTo: "<root@test>",
			Date: now, CreatedAt: now, Mailbox: "INBOX",
			Account: account, FetchedBody: true,
		}); err != nil {
			t.Fatalf("insert %s message: %v", account, err)
		}
	}

	work, err := db.GetByMessageID("shared-work@test")
	if err != nil || work == nil {
		t.Fatalf("get work message: %+v err=%v", work, err)
	}
	personal, err := db.GetByMessageID("shared-personal@test")
	if err != nil || personal == nil {
		t.Fatalf("get personal message: %+v err=%v", personal, err)
	}
	if work.ThreadID != personal.ThreadID {
		t.Fatalf("fixture threads differ (%q vs %q); this case needs one shared thread",
			work.ThreadID, personal.ThreadID)
	}

	h := New(db, nil)
	resp := h.Tag("thread:"+work.ThreadID, "+archived", []string{"work"})
	if !resp.OK {
		t.Fatalf("Tag failed: %s", resp.Error)
	}

	workTags, err := db.GetMessageTags(work.ID)
	if err != nil {
		t.Fatalf("get work tags: %v", err)
	}
	if !slices.Contains(workTags, "archived") {
		t.Errorf("work tags = %v, want the tag applied", workTags)
	}

	personalTags, err := db.GetMessageTags(personal.ID)
	if err != nil {
		t.Fatalf("get personal tags: %v", err)
	}
	if slices.Contains(personalTags, "archived") {
		t.Errorf("personal tags = %v: the mutation escaped the selected account", personalTags)
	}
}

// recordingTrigger captures which accounts a mutation asked to be synced.
type recordingTrigger struct{ accounts []string }

func (r *recordingTrigger) TriggerSync(account string) {
	r.accounts = append(r.accounts, account)
}

// seedSharedThread puts one conversation into every named account and returns
// the messages by account.
func seedSharedThread(t *testing.T, db *store.DB, accounts ...string) map[string]*store.Message {
	t.Helper()
	now := time.Now().Unix()
	out := make(map[string]*store.Message, len(accounts))
	for _, account := range accounts {
		id := "shared-" + account + "@test"
		if err := db.InsertMessage(&store.Message{
			MessageID: id, Subject: "Shared thread",
			FromAddr: "a@example.com", ToAddrs: "b@example.com",
			Refs: "<root@test>", InReplyTo: "<root@test>",
			Date: now, CreatedAt: now, Mailbox: "INBOX",
			Account: account, FetchedBody: true,
		}); err != nil {
			t.Fatalf("insert %s message: %v", account, err)
		}
		msg, err := db.GetByMessageID(id)
		if err != nil || msg == nil {
			t.Fatalf("get %s message: %+v err=%v", account, msg, err)
		}
		out[account] = msg
	}
	return out
}

// TestTagAccountScopeGovernsEveryEffect checks the rest of the contract: the
// scope has to reach the counts, the journal and the sync trigger, not only the
// stored tags. Each of those used to work out "which messages" for itself, so
// asserting one of them says nothing about the others.
func TestTagAccountScopeGovernsEveryEffect(t *testing.T) {
	db := newTestStore(t)
	msgs := seedSharedThread(t, db, "work", "personal")
	thread := msgs["work"].ThreadID

	trigger := &recordingTrigger{}
	h := New(db, nil)
	h.SetSyncTrigger(trigger)
	h.EnableTagJournal()

	resp := h.Tag("thread:"+thread, "+archived", []string{"work"})
	if !resp.OK {
		t.Fatalf("Tag failed: %s", resp.Error)
	}

	// Counts come from the resolved targets. Reporting the search's thread
	// count would claim work happened in accounts that were skipped.
	if resp.MatchedThreads == nil || *resp.MatchedThreads != 1 {
		t.Errorf("matched threads = %v, want 1", resp.MatchedThreads)
	}
	if resp.ChangedThreads == nil || *resp.ChangedThreads != 1 {
		t.Errorf("changed threads = %v, want 1", resp.ChangedThreads)
	}

	journal, err := db.ReadTagJournal()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if len(journal) == 0 {
		t.Fatal("nothing journalled; the assertions below would hold even if every write was dropped")
	}
	for _, entry := range journal {
		if entry.Account != "work" {
			t.Errorf("journal entry for account %q (message %q): the unselected account would be tagged remotely on the next sync",
				entry.Account, entry.MessageID)
		}
	}

	if !slices.Equal(trigger.accounts, []string{"work"}) {
		t.Errorf("sync triggered for %v, want only [work]", trigger.accounts)
	}

	// The fifth consumer. The push itself runs in a goroutine against a
	// concrete client, but its payload is a pure function of the same target,
	// so the scope is assertable without the network.
	target, err := db.ThreadTagTarget(thread, []string{"work"})
	if err != nil {
		t.Fatalf("resolve target: %v", err)
	}
	changes := tagChangesFor(target, []string{"archived"}, nil, 0)
	if len(changes) == 0 {
		t.Fatal("no changes built; the loop below would assert nothing")
	}
	for _, change := range changes {
		if change.Account != "work" {
			t.Errorf("remote push carries account %q (message %q): the unselected account would be tagged on the server",
				change.Account, change.MessageID)
		}
	}
}

// TestTagWithoutAccountScopeSpansTheThread pins the other half of the contract:
// no filter means the operation stays thread-wide across accounts, which is
// what makes tagging a set operation rather than a per-account one.
func TestTagWithoutAccountScopeSpansTheThread(t *testing.T) {
	db := newTestStore(t)
	msgs := seedSharedThread(t, db, "work", "personal")

	trigger := &recordingTrigger{}
	h := New(db, nil)
	h.SetSyncTrigger(trigger)

	resp := h.Tag("thread:"+msgs["work"].ThreadID, "+archived", nil)
	if !resp.OK {
		t.Fatalf("Tag failed: %s", resp.Error)
	}
	for account, msg := range msgs {
		tags, err := db.GetMessageTags(msg.ID)
		if err != nil {
			t.Fatalf("get %s tags: %v", account, err)
		}
		if !slices.Contains(tags, "archived") {
			t.Errorf("%s tags = %v, want the tag applied thread-wide", account, tags)
		}
	}
	slices.Sort(trigger.accounts)
	if !slices.Equal(trigger.accounts, []string{"personal", "work"}) {
		t.Errorf("sync triggered for %v, want both accounts", trigger.accounts)
	}
}

// TestTagAccountTermInQueryIsNotAScope is the case that distinguishes carrying
// the scope from inferring it. "path:work" here is a search term the user
// wrote, not the clause --account produces, and the two are textually
// identical. With no scope passed, the operation stays thread-wide: the query
// decides which threads match, never which messages are written.
//
// The other unscoped test uses a bare "thread:" selector, so it would keep
// passing if query inference came back.
func TestTagAccountTermInQueryIsNotAScope(t *testing.T) {
	db := newTestStore(t)
	msgs := seedSharedThread(t, db, "work", "personal")

	h := New(db, nil)
	resp := h.Tag("path:work", "+archived", nil)
	if !resp.OK {
		t.Fatalf("Tag failed: %s", resp.Error)
	}

	for account, msg := range msgs {
		tags, err := db.GetMessageTags(msg.ID)
		if err != nil {
			t.Fatalf("get %s tags: %v", account, err)
		}
		if !slices.Contains(tags, "archived") {
			t.Errorf("%s tags = %v: an account term in the query narrowed the mutation", account, tags)
		}
	}
}

// TestTagMultipleAccountScopeIsTheUnion covers the third row of the contract:
// several accounts select their union, not their intersection and not the
// first one.
func TestTagMultipleAccountScopeIsTheUnion(t *testing.T) {
	db := newTestStore(t)
	msgs := seedSharedThread(t, db, "work", "personal", "archive")

	h := New(db, nil)
	resp := h.Tag("thread:"+msgs["work"].ThreadID, "+archived", []string{"work", "personal"})
	if !resp.OK {
		t.Fatalf("Tag failed: %s", resp.Error)
	}

	for _, account := range []string{"work", "personal"} {
		tags, err := db.GetMessageTags(msgs[account].ID)
		if err != nil {
			t.Fatalf("get %s tags: %v", account, err)
		}
		if !slices.Contains(tags, "archived") {
			t.Errorf("%s tags = %v, want the tag applied", account, tags)
		}
	}
	tags, err := db.GetMessageTags(msgs["archive"].ID)
	if err != nil {
		t.Fatalf("get archive tags: %v", err)
	}
	if slices.Contains(tags, "archived") {
		t.Errorf("archive tags = %v: an unselected account was included", tags)
	}
}

// TestTagPreviewSharesTheScopeAndWritesNothing pins that the dry run resolves
// the same rows as the write. A preview that computed its own selection could
// report an effect the real run would not produce, or miss one it would.
func TestTagPreviewSharesTheScopeAndWritesNothing(t *testing.T) {
	db := newTestStore(t)
	msgs := seedSharedThread(t, db, "work", "personal")

	// The selected account already carries the tag; the other one does not.
	// A preview that resolved its own, wider selection would see work to do
	// and report an effect the scoped write will never produce.
	if err := db.AddTag(msgs["work"].ID, "archived"); err != nil {
		t.Fatalf("seed existing tag: %v", err)
	}

	h := New(db, nil)
	resp := h.PreviewTag("thread:"+msgs["work"].ThreadID, "+archived", []string{"work"})
	if !resp.OK {
		t.Fatalf("PreviewTag failed: %s", resp.Error)
	}
	if resp.MatchedThreads == nil || *resp.MatchedThreads != 1 {
		t.Errorf("matched threads = %v, want 1", resp.MatchedThreads)
	}
	if resp.ChangedThreads == nil || *resp.ChangedThreads != 0 {
		t.Errorf("changed threads = %v, want 0: the selected account already has the tag", resp.ChangedThreads)
	}

	// The seeded tag is still the only one: the preview neither removed it nor
	// added the other account's.
	workTags, err := db.GetMessageTags(msgs["work"].ID)
	if err != nil {
		t.Fatalf("get work tags: %v", err)
	}
	if !slices.Contains(workTags, "archived") {
		t.Errorf("work tags = %v: the preview removed the seeded tag", workTags)
	}
	personalTags, err := db.GetMessageTags(msgs["personal"].ID)
	if err != nil {
		t.Fatalf("get personal tags: %v", err)
	}
	if slices.Contains(personalTags, "archived") {
		t.Errorf("personal tags = %v: a preview wrote to the store", personalTags)
	}
	journal, err := db.ReadTagJournal()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if len(journal) != 0 {
		t.Errorf("preview journalled %d entries", len(journal))
	}
}

// TestTagNamingAnAbsentAccountChangesNothing covers a scope that selects an
// account the thread has no messages in. The search still matches the thread —
// it is reached through the accounts that do hold it — so this is the case the
// empty-target skip exists for: matched by the query, empty after scoping.
//
// Nothing may be written, nothing journalled, and the thread must not be
// counted as matched.
func TestTagNamingAnAbsentAccountChangesNothing(t *testing.T) {
	db := newTestStore(t)
	msgs := seedSharedThread(t, db, "work", "personal")

	h := New(db, nil)
	h.EnableTagJournal()
	resp := h.Tag("thread:"+msgs["work"].ThreadID, "+archived", []string{"archive"})

	// Demanded explicitly rather than guarded behind resp.OK: a failure
	// response or absent counts would otherwise satisfy this by accident.
	if !resp.OK {
		t.Fatalf("Tag failed: %s", resp.Error)
	}
	if resp.MatchedThreads == nil || *resp.MatchedThreads != 0 {
		t.Errorf("matched threads = %v, want 0: no message was in scope", resp.MatchedThreads)
	}
	if resp.ChangedThreads == nil || *resp.ChangedThreads != 0 {
		t.Errorf("changed threads = %v, want 0", resp.ChangedThreads)
	}
	for account, msg := range msgs {
		tags, err := db.GetMessageTags(msg.ID)
		if err != nil {
			t.Fatalf("get %s tags: %v", account, err)
		}
		if slices.Contains(tags, "archived") {
			t.Errorf("%s tags = %v: an out-of-scope operation wrote anyway", account, tags)
		}
	}
	journal, err := db.ReadTagJournal()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if len(journal) != 0 {
		t.Errorf("journalled %d entries for an out-of-scope operation", len(journal))
	}
}
