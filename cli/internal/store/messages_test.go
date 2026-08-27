package store

import (
	"strings"
	"testing"
	"time"
)

func TestInsertAndGetMessage(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	err := db.InsertMessage(&Message{
		MessageID:   "test@example.com",
		Subject:     "Hello World",
		FromAddr:    "alice@example.com",
		ToAddrs:     "bob@example.com",
		Date:        now,
		CreatedAt:   now,
		BodyText:    "This is a test",
		BodyHTML:    "<p>This is a test</p>",
		Mailbox:     "INBOX",
		FetchedBody: true,
		Account:     "work",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	msg, err := db.GetByMessageID("test@example.com")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if msg == nil {
		t.Fatal("message not found")
	}
	if msg.Subject != "Hello World" {
		t.Errorf("subject = %q, want %q", msg.Subject, "Hello World")
	}
	if msg.FromAddr != "alice@example.com" {
		t.Errorf("from = %q, want %q", msg.FromAddr, "alice@example.com")
	}
	if msg.BodyText != "This is a test" {
		t.Errorf("body = %q, want %q", msg.BodyText, "This is a test")
	}
	if !msg.FetchedBody {
		t.Error("fetched_body should be true")
	}
	if msg.ThreadID == "" {
		t.Error("thread_id should not be empty")
	}
}

func TestSyncedLabelsRoundTrip(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()
	msg := &Message{MessageID: "lbl@example.com", Subject: "x", Date: now, CreatedAt: now, Mailbox: "ALL", Account: "work"}
	if err := db.InsertMessage(msg); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// A fresh message has an empty label baseline.
	if got, err := db.GetSyncedLabels("lbl@example.com", "work"); err != nil || got != "" {
		t.Fatalf("initial synced_labels = %q err=%v, want empty", got, err)
	}
	if err := db.SetSyncedLabels("lbl@example.com", "work", "inbox,newsletter"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got, err := db.GetSyncedLabels("lbl@example.com", "work"); err != nil || got != "inbox,newsletter" {
		t.Errorf("synced_labels = %q err=%v, want inbox,newsletter", got, err)
	}
	// Unknown message errors on set.
	if err := db.SetSyncedLabels("nope@example.com", "work", "x"); err == nil {
		t.Error("SetSyncedLabels for unknown message should error")
	}
}

func TestFolderFlagState_RoundTrip(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	insert := func(messageID, mailbox, remoteRef, syncedFlags string) *Message {
		t.Helper()
		msg := &Message{
			MessageID:   messageID,
			Subject:     "Flag state",
			Date:        now,
			CreatedAt:   now,
			Mailbox:     mailbox,
			Account:     "work",
			RemoteRef:   remoteRef,
			SyncedFlags: syncedFlags,
		}
		if err := db.InsertMessage(msg); err != nil {
			t.Fatalf("insert %s: %v", messageID, err)
		}
		return msg
	}

	tagged := insert("tagged@example.com", "INBOX", "101", `\Seen,\Flagged`)
	for _, tag := range []string{"inbox", "flagged"} {
		if err := db.AddTag(tagged.ID, tag); err != nil {
			t.Fatalf("add tag: %v", err)
		}
	}
	insert("untagged@example.com", "INBOX", "102", "")
	insert("no-ref@example.com", "INBOX", "", "")         // excluded: empty remote_ref
	insert("elsewhere@example.com", "Archive", "103", "") // excluded: other mailbox

	rows, err := db.GetFolderFlagState("work", "INBOX")
	if err != nil {
		t.Fatalf("get folder flag state: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (%+v)", len(rows), rows)
	}
	byID := make(map[string]FolderFlagRow, len(rows))
	for _, r := range rows {
		byID[r.MessageID] = r
	}
	got, ok := byID["tagged@example.com"]
	if !ok {
		t.Fatal("tagged@example.com missing")
	}
	if got.RemoteRef != "101" {
		t.Errorf("remote_ref = %q, want %q", got.RemoteRef, "101")
	}
	if got.SyncedFlags != `\Seen,\Flagged` {
		t.Errorf("synced_flags = %q, want %q", got.SyncedFlags, `\Seen,\Flagged`)
	}
	if len(got.Tags) != 2 {
		t.Errorf("tags = %v, want [inbox flagged]", got.Tags)
	}
	got, ok = byID["untagged@example.com"]
	if !ok {
		t.Fatal("untagged@example.com missing (tag-less messages must still appear)")
	}
	if len(got.Tags) != 0 {
		t.Errorf("tags = %v, want none", got.Tags)
	}

	// Re-upsert with empty remote_ref / synced_flags must NOT clobber the
	// stored values (CASE ... != '' upsert rules).
	insert("tagged@example.com", "INBOX", "", "")
	rows, err = db.GetFolderFlagState("work", "INBOX")
	if err != nil {
		t.Fatalf("get after re-upsert: %v", err)
	}
	byID = make(map[string]FolderFlagRow, len(rows))
	for _, r := range rows {
		byID[r.MessageID] = r
	}
	if got := byID["tagged@example.com"]; got.RemoteRef != "101" || got.SyncedFlags != `\Seen,\Flagged` {
		t.Errorf("after empty re-upsert: remote_ref = %q, synced_flags = %q; want preserved", got.RemoteRef, got.SyncedFlags)
	}

	// A re-delivered message with DIFFERENT non-empty server flags (a delta
	// re-ingest after a server-side flag change) must also NOT overwrite the
	// baseline — it is owned by the reconciliation, not by ingest. Clobbering it
	// here corrupts the three-way merge and reverts the server change.
	insert("tagged@example.com", "INBOX", "101", `\Seen`) // server now reports only \Seen
	rows, err = db.GetFolderFlagState("work", "INBOX")
	if err != nil {
		t.Fatalf("get after non-empty re-ingest: %v", err)
	}
	byID = make(map[string]FolderFlagRow, len(rows))
	for _, r := range rows {
		byID[r.MessageID] = r
	}
	if got := byID["tagged@example.com"]; got.SyncedFlags != `\Seen,\Flagged` {
		t.Errorf("after non-empty re-ingest: synced_flags = %q, want the original %q preserved", got.SyncedFlags, `\Seen,\Flagged`)
	}

	// SetSyncedFlags updates the baseline in place.
	if err := db.SetSyncedFlags("tagged@example.com", "work", `\Seen`); err != nil {
		t.Fatalf("set synced flags: %v", err)
	}
	rows, err = db.GetFolderFlagState("work", "INBOX")
	if err != nil {
		t.Fatalf("get after set: %v", err)
	}
	byID = make(map[string]FolderFlagRow, len(rows))
	for _, r := range rows {
		byID[r.MessageID] = r
	}
	if got := byID["tagged@example.com"]; got.SyncedFlags != `\Seen` {
		t.Errorf("synced_flags after set = %q, want %q", got.SyncedFlags, `\Seen`)
	}

	// Unknown message / account must error.
	if err := db.SetSyncedFlags("nope@example.com", "work", `\Seen`); err == nil {
		t.Error("SetSyncedFlags(unknown message) should error")
	}
	if err := db.SetSyncedFlags("tagged@example.com", "nobody", `\Seen`); err == nil {
		t.Error("SetSyncedFlags(unknown account) should error")
	}

	// Unknown mailbox / account resolve to no rows without error.
	if rows, err := db.GetFolderFlagState("work", "NoSuchBox"); err != nil || len(rows) != 0 {
		t.Errorf("unknown mailbox: rows = %v, err = %v; want empty, nil", rows, err)
	}
	if rows, err := db.GetFolderFlagState("nobody", "INBOX"); err != nil || len(rows) != 0 {
		t.Errorf("unknown account: rows = %v, err = %v; want empty, nil", rows, err)
	}
}

func TestUpsert_HeadersOnlyThenBody(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	// First insert: headers only (no body)
	err := db.InsertMessage(&Message{
		MessageID:   "upsert@example.com",
		Subject:     "Upsert Test",
		FromAddr:    "alice@example.com",
		Date:        now,
		CreatedAt:   now,
		Mailbox:     "INBOX",
		Flags:       "\\Seen",
		FetchedBody: false,
	})
	if err != nil {
		t.Fatalf("insert headers: %v", err)
	}

	msg, _ := db.GetByMessageID("upsert@example.com")
	if msg.FetchedBody {
		t.Error("should not have body yet")
	}

	// Second insert: now with body
	err = db.InsertMessage(&Message{
		MessageID:   "upsert@example.com",
		Subject:     "Upsert Test",
		FromAddr:    "alice@example.com",
		Date:        now,
		CreatedAt:   now,
		BodyText:    "Now with body",
		BodyHTML:    "<p>Now with body</p>",
		Mailbox:     "INBOX",
		Flags:       "\\Seen \\Answered",
		FetchedBody: true,
	})
	if err != nil {
		t.Fatalf("upsert with body: %v", err)
	}

	msg, _ = db.GetByMessageID("upsert@example.com")
	if !msg.FetchedBody {
		t.Error("should have body after upsert")
	}
	if msg.BodyText != "Now with body" {
		t.Errorf("body = %q, want %q", msg.BodyText, "Now with body")
	}
	// Flags should be updated
	if msg.Flags != "\\Seen \\Answered" {
		t.Errorf("flags = %q, want %q", msg.Flags, "\\Seen \\Answered")
	}
}

func TestUpsert_DoesNotOverwriteBody(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	// Insert with body
	db.InsertMessage(&Message{
		MessageID: "keep-body@x", Subject: "Keep",
		FromAddr: "a@x", Date: now, CreatedAt: now,
		BodyText: "Original body", FetchedBody: true,
	})

	// Upsert with headers-only (fetched_body=false) — should NOT overwrite body
	db.InsertMessage(&Message{
		MessageID: "keep-body@x", Subject: "Keep",
		FromAddr: "a@x", Date: now, CreatedAt: now,
		BodyText: "", FetchedBody: false,
	})

	msg, _ := db.GetByMessageID("keep-body@x")
	if msg.BodyText != "Original body" {
		t.Errorf("body was overwritten: %q", msg.BodyText)
	}
	if !msg.FetchedBody {
		t.Error("fetched_body flag was reset")
	}
}

func TestUpdateBody(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	db.InsertMessage(&Message{
		MessageID: "update-body@x", Subject: "Update",
		FromAddr: "a@x", Date: now, CreatedAt: now,
		FetchedBody: false,
	})

	err := db.UpdateBody("update-body@x", "New body", "<p>New body</p>")
	if err != nil {
		t.Fatalf("update body: %v", err)
	}

	msg, _ := db.GetByMessageID("update-body@x")
	if msg.BodyText != "New body" {
		t.Errorf("body = %q, want %q", msg.BodyText, "New body")
	}
	if !msg.FetchedBody {
		t.Error("fetched_body should be true after update")
	}
}

func TestUpdateBody_NotFound(t *testing.T) {
	db := newTestDB(t)
	err := db.UpdateBody("nonexistent@x", "body", "html")
	if err == nil {
		t.Error("expected error for nonexistent message")
	}
}

func TestGetByThread(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	db.InsertMessage(&Message{
		MessageID: "t-root@x", Subject: "Thread",
		FromAddr: "a@x", Date: now, CreatedAt: now, FetchedBody: true,
	})
	db.InsertMessage(&Message{
		MessageID: "t-reply@x", InReplyTo: "<t-root@x>", Refs: "<t-root@x>",
		Subject: "Re: Thread", FromAddr: "b@x", Date: now + 1, CreatedAt: now + 1, FetchedBody: true,
	})

	root, _ := db.GetByMessageID("t-root@x")
	msgs, err := db.GetByThread(root.ThreadID)
	if err != nil {
		t.Fatalf("get by thread: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	// Should be ordered by date ascending
	if msgs[0].MessageID != "t-root@x" {
		t.Errorf("first message = %q, want t-root@x", msgs[0].MessageID)
	}
}

func TestGetByThreadPreservesAllOwningAccounts(t *testing.T) {
	db := newTestDB(t)
	for _, account := range []string{"work", "personal"} {
		message := &Message{
			MessageID: "shared@x", Subject: "Shared", FromAddr: "a@x",
			Account: account, Date: 1, CreatedAt: 1, FetchedBody: true,
		}
		if err := db.InsertMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	message, err := db.GetByMessageIDAndAccount("shared@x", "work")
	if err != nil {
		t.Fatal(err)
	}
	messages, err := db.GetByThread(message.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || strings.Join(messages[0].Accounts, ",") != "personal,work" {
		t.Fatalf("deduplicated accounts = %#v", messages)
	}
	if len(messages[0].AccountRows) != 2 {
		t.Fatalf("deduplicated account rows = %v, want two", messages[0].AccountRows)
	}
}

func TestMessageExists(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	exists, _ := db.MessageExists("nope@x")
	if exists {
		t.Error("should not exist")
	}

	db.InsertMessage(&Message{
		MessageID: "exists@x", Subject: "Exists",
		FromAddr: "a@x", Date: now, CreatedAt: now, FetchedBody: true,
	})

	exists, _ = db.MessageExists("exists@x")
	if !exists {
		t.Error("should exist after insert")
	}
}

func TestDeleteByMessageID(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	db.InsertMessage(&Message{
		MessageID: "del@x", Subject: "Delete me",
		FromAddr: "a@x", Date: now, CreatedAt: now, FetchedBody: true,
	})

	err := db.DeleteByMessageID("del@x")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	msg, _ := db.GetByMessageID("del@x")
	if msg != nil {
		t.Error("message should be deleted")
	}
}

func TestDeleteByMessageID_NotFound(t *testing.T) {
	db := newTestDB(t)
	err := db.DeleteByMessageID("nonexistent@x")
	if err == nil {
		t.Error("expected error for nonexistent message")
	}
}

func TestInsertBatch(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	msgs := []*Message{
		{MessageID: "batch1@x", Subject: "Batch 1", FromAddr: "a@x", Date: now, CreatedAt: now, FetchedBody: true},
		{MessageID: "batch2@x", Subject: "Batch 2", FromAddr: "b@x", Date: now + 1, CreatedAt: now + 1, FetchedBody: true},
		{MessageID: "batch3@x", Subject: "Batch 3", FromAddr: "c@x", Date: now + 2, CreatedAt: now + 2, FetchedBody: true},
	}

	if err := db.InsertBatch(msgs); err != nil {
		t.Fatalf("batch: %v", err)
	}

	for _, m := range msgs {
		got, _ := db.GetByMessageID(m.MessageID)
		if got == nil {
			t.Errorf("message %q not found after batch", m.MessageID)
		}
	}
}

func TestUpsert_CrossAccount(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	// Same message_id, two different accounts → two rows
	err := db.InsertMessage(&Message{
		MessageID: "cross@x", Subject: "Cross",
		FromAddr: "a@x", Date: now, CreatedAt: now, FetchedBody: true,
		Account: "work",
	})
	if err != nil {
		t.Fatalf("insert work: %v", err)
	}

	err = db.InsertMessage(&Message{
		MessageID: "cross@x", Subject: "Cross",
		FromAddr: "a@x", Date: now, CreatedAt: now, FetchedBody: true,
		Account: "personal",
	})
	if err != nil {
		t.Fatalf("insert personal: %v", err)
	}

	// Count rows — should be 2
	var count int
	db.db.QueryRow("SELECT COUNT(*) FROM messages WHERE message_id = ?", "cross@x").Scan(&count)
	if count != 2 {
		t.Errorf("got %d rows, want 2 (one per account)", count)
	}

	// GetByMessageID returns one (LIMIT 1)
	msg, _ := db.GetByMessageID("cross@x")
	if msg == nil {
		t.Fatal("GetByMessageID returned nil")
	}
}

func TestGetByThread_Dedup(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	// Same message in two accounts → same thread
	db.InsertMessage(&Message{
		MessageID: "td-root@x", Subject: "Thread dedup",
		FromAddr: "a@x", Date: now, CreatedAt: now, FetchedBody: true,
		Account: "work",
	})
	db.InsertMessage(&Message{
		MessageID: "td-root@x", Subject: "Thread dedup",
		FromAddr: "a@x", Date: now, CreatedAt: now, FetchedBody: true,
		Account: "personal",
	})

	root, _ := db.GetByMessageID("td-root@x")
	msgs, err := db.GetByThread(root.ThreadID)
	if err != nil {
		t.Fatalf("get by thread: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("got %d messages, want 1 (dedup across accounts)", len(msgs))
	}
}

func TestDeleteByMessageIDAndAccount(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	db.InsertMessage(&Message{
		MessageID: "del-acct@x", Subject: "Del",
		FromAddr: "a@x", Date: now, CreatedAt: now, FetchedBody: true,
		Account: "work",
	})
	db.InsertMessage(&Message{
		MessageID: "del-acct@x", Subject: "Del",
		FromAddr: "a@x", Date: now, CreatedAt: now, FetchedBody: true,
		Account: "personal",
	})

	err := db.DeleteByMessageIDAndAccount("del-acct@x", "work")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Should still find personal row
	var count int
	db.db.QueryRow("SELECT COUNT(*) FROM messages WHERE message_id = ?", "del-acct@x").Scan(&count)
	if count != 1 {
		t.Errorf("got %d rows after delete, want 1", count)
	}
}
