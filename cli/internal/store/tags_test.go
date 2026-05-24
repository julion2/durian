package store

import (
	"testing"
	"time"
)

func insertTestMessage(t *testing.T, db *DB, msgID string) int64 {
	t.Helper()
	now := time.Now().Unix()
	msg := &Message{
		MessageID: msgID, Subject: "Test " + msgID,
		FromAddr: "a@x", Date: now, CreatedAt: now, FetchedBody: true,
	}
	if err := db.InsertMessage(msg); err != nil {
		t.Fatalf("insert %s: %v", msgID, err)
	}
	return msg.ID
}

func TestAddAndGetTags(t *testing.T) {
	db := newTestDB(t)
	id := insertTestMessage(t, db, "tag-test@x")

	db.AddTag(id, "inbox")
	db.AddTag(id, "unread")

	tags, err := db.GetMessageTags(id)
	if err != nil {
		t.Fatalf("get tags: %v", err)
	}
	if len(tags) != 2 {
		t.Fatalf("got %d tags, want 2", len(tags))
	}
	// Ordered alphabetically
	if tags[0] != "inbox" || tags[1] != "unread" {
		t.Errorf("tags = %v, want [inbox unread]", tags)
	}
}

func TestAddTag_Duplicate(t *testing.T) {
	db := newTestDB(t)
	id := insertTestMessage(t, db, "dup-tag@x")

	db.AddTag(id, "inbox")
	db.AddTag(id, "inbox") // should not fail

	tags, _ := db.GetMessageTags(id)
	if len(tags) != 1 {
		t.Errorf("got %d tags, want 1 (no duplicates)", len(tags))
	}
}

func TestRemoveTag(t *testing.T) {
	db := newTestDB(t)
	id := insertTestMessage(t, db, "rm-tag@x")

	db.AddTag(id, "inbox")
	db.AddTag(id, "unread")
	db.RemoveTag(id, "unread")

	tags, _ := db.GetMessageTags(id)
	if len(tags) != 1 || tags[0] != "inbox" {
		t.Errorf("tags after remove = %v, want [inbox]", tags)
	}
}

func TestTagThread(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	db.InsertMessage(&Message{
		MessageID: "tt-root@x", Subject: "Thread",
		FromAddr: "a@x", Date: now, CreatedAt: now, FetchedBody: true,
	})
	db.InsertMessage(&Message{
		MessageID: "tt-reply@x", InReplyTo: "<tt-root@x>", Refs: "<tt-root@x>",
		Subject: "Re: Thread", FromAddr: "b@x", Date: now + 1, CreatedAt: now + 1, FetchedBody: true,
	})

	root, _ := db.GetByMessageID("tt-root@x")
	db.TagThread(root.ThreadID, "important")

	reply, _ := db.GetByMessageID("tt-reply@x")
	rootTags, _ := db.GetMessageTags(root.ID)
	replyTags, _ := db.GetMessageTags(reply.ID)

	if len(rootTags) != 1 || rootTags[0] != "important" {
		t.Errorf("root tags = %v", rootTags)
	}
	if len(replyTags) != 1 || replyTags[0] != "important" {
		t.Errorf("reply tags = %v", replyTags)
	}
}

func TestUntagThread(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	db.InsertMessage(&Message{
		MessageID: "ut-root@x", Subject: "Thread",
		FromAddr: "a@x", Date: now, CreatedAt: now, FetchedBody: true,
	})
	root, _ := db.GetByMessageID("ut-root@x")

	db.TagThread(root.ThreadID, "inbox")
	db.UntagThread(root.ThreadID, "inbox")

	tags, _ := db.GetMessageTags(root.ID)
	if len(tags) != 0 {
		t.Errorf("tags after untag = %v, want empty", tags)
	}
}

func TestModifyTagsByThread(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	db.InsertMessage(&Message{
		MessageID: "mod-root@x", Subject: "Thread",
		FromAddr: "a@x", Date: now, CreatedAt: now, FetchedBody: true,
	})
	root, _ := db.GetByMessageID("mod-root@x")

	db.TagThread(root.ThreadID, "inbox")
	db.TagThread(root.ThreadID, "unread")

	// Atomic: remove unread, add archived
	err := db.ModifyTagsByThread(root.ThreadID, []string{"archived"}, []string{"unread"})
	if err != nil {
		t.Fatalf("modify: %v", err)
	}

	tags, _ := db.GetMessageTags(root.ID)
	tagSet := make(map[string]bool)
	for _, tag := range tags {
		tagSet[tag] = true
	}

	if !tagSet["inbox"] || !tagSet["archived"] {
		t.Errorf("expected inbox+archived, got %v", tags)
	}
	if tagSet["unread"] {
		t.Error("unread should have been removed")
	}
}

func TestListTags(t *testing.T) {
	db := newTestDB(t)
	id1 := insertTestMessage(t, db, "lt1@x")
	id2 := insertTestMessage(t, db, "lt2@x")

	db.AddTag(id1, "inbox")
	db.AddTag(id1, "unread")
	db.AddTag(id2, "inbox")
	db.AddTag(id2, "flagged")

	tags, err := db.ListTags()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tags) != 3 {
		t.Errorf("got %d tags, want 3 (flagged, inbox, unread)", len(tags))
	}
}

func TestModifyTagsByMessageID(t *testing.T) {
	db := newTestDB(t)
	id := insertTestMessage(t, db, "modify-mid@x")
	db.AddTag(id, "inbox")
	db.AddTag(id, "unread")

	err := db.ModifyTagsByMessageID("modify-mid@x", []string{"flagged"}, []string{"unread"})
	if err != nil {
		t.Fatalf("modify: %v", err)
	}

	tags, _ := db.GetTagsByMessageID("modify-mid@x")
	tagSet := make(map[string]bool)
	for _, tag := range tags {
		tagSet[tag] = true
	}
	if !tagSet["inbox"] || !tagSet["flagged"] {
		t.Errorf("expected inbox+flagged, got %v", tags)
	}
	if tagSet["unread"] {
		t.Error("unread should have been removed")
	}
}

func TestModifyTagsByMessageID_NotFound(t *testing.T) {
	db := newTestDB(t)

	// Should be a no-op, not an error
	err := db.ModifyTagsByMessageID("nonexistent@x", []string{"inbox"}, nil)
	if err != nil {
		t.Fatalf("expected no-op, got error: %v", err)
	}
}

func TestGetTagsByMessageID(t *testing.T) {
	db := newTestDB(t)
	id := insertTestMessage(t, db, "get-mid@x")
	db.AddTag(id, "inbox")
	db.AddTag(id, "unread")

	tags, err := db.GetTagsByMessageID("get-mid@x")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(tags) != 2 {
		t.Fatalf("got %d tags, want 2", len(tags))
	}
	if tags[0] != "inbox" || tags[1] != "unread" {
		t.Errorf("tags = %v, want [inbox unread]", tags)
	}
}

func TestGetTagsByMessageID_NotFound(t *testing.T) {
	db := newTestDB(t)

	tags, err := db.GetTagsByMessageID("nonexistent@x")
	if err != nil {
		t.Fatalf("expected nil tags, got error: %v", err)
	}
	if len(tags) != 0 {
		t.Errorf("expected empty, got %v", tags)
	}
}

func TestGetAllMessagesWithTags(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	db.InsertMessage(&Message{
		MessageID: "mwt1@x", Subject: "Msg 1",
		FromAddr: "a@x", Date: now, CreatedAt: now, Mailbox: "INBOX", FetchedBody: true,
	})
	db.InsertMessage(&Message{
		MessageID: "mwt2@x", Subject: "Msg 2",
		FromAddr: "b@x", Date: now + 1, CreatedAt: now + 1, Mailbox: "INBOX", FetchedBody: true,
	})
	db.InsertMessage(&Message{
		MessageID: "mwt3@x", Subject: "Msg 3",
		FromAddr: "c@x", Date: now + 2, CreatedAt: now + 2, Mailbox: "Sent", FetchedBody: true,
	})

	m1, _ := db.GetByMessageID("mwt1@x")
	m2, _ := db.GetByMessageID("mwt2@x")
	m3, _ := db.GetByMessageID("mwt3@x")

	db.AddTag(m1.ID, "inbox")
	db.AddTag(m2.ID, "inbox")
	db.AddTag(m2.ID, "unread")
	db.AddTag(m3.ID, "sent")

	result, err := db.GetAllMessagesWithTags("INBOX")
	if err != nil {
		t.Fatalf("get all: %v", err)
	}
	if len(result) != 2 {
		t.Errorf("got %d entries, want 2 (INBOX only)", len(result))
	}
	if len(result["mwt2@x"]) != 2 {
		t.Errorf("mwt2 tags = %v, want 2", result["mwt2@x"])
	}
}

func TestModifyTagsByMessageIDAndAccount(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	// Same message, two accounts with different tags
	db.InsertMessage(&Message{
		MessageID: "acct-tag@x", Subject: "Acct tag test",
		FromAddr: "a@x", Date: now, CreatedAt: now, FetchedBody: true,
		Account: "work",
	})
	db.InsertMessage(&Message{
		MessageID: "acct-tag@x", Subject: "Acct tag test",
		FromAddr: "a@x", Date: now, CreatedAt: now, FetchedBody: true,
		Account: "personal",
	})

	// Add "sent" to work account only
	err := db.ModifyTagsByMessageIDAndAccount("acct-tag@x", "work", []string{"sent"}, nil)
	if err != nil {
		t.Fatalf("modify work tags: %v", err)
	}

	// Add "inbox" to personal account only
	err = db.ModifyTagsByMessageIDAndAccount("acct-tag@x", "personal", []string{"inbox"}, nil)
	if err != nil {
		t.Fatalf("modify personal tags: %v", err)
	}

	// Verify work row has "sent" but not "inbox". Step 7f removed
	// messages.account; lookup is now via the accounts FK.
	workID := messageDBIDForAccount(t, db, "acct-tag@x", "work")
	workTags, _ := db.GetMessageTags(workID)
	if len(workTags) != 1 || workTags[0] != "sent" {
		t.Errorf("work tags = %v, want [sent]", workTags)
	}

	// Verify personal row has "inbox" but not "sent"
	persID := messageDBIDForAccount(t, db, "acct-tag@x", "personal")
	persTags, _ := db.GetMessageTags(persID)
	if len(persTags) != 1 || persTags[0] != "inbox" {
		t.Errorf("personal tags = %v, want [inbox]", persTags)
	}
}

func TestModifyTagsByMessageIDAndAccount_NotFound(t *testing.T) {
	db := newTestDB(t)

	// Should be a no-op
	err := db.ModifyTagsByMessageIDAndAccount("nonexistent@x", "work", []string{"inbox"}, nil)
	if err != nil {
		t.Fatalf("expected no-op, got error: %v", err)
	}
}

// messageDBIDForAccount looks up messages.id for a (message_id, account)
// pair. Step 7f removed messages.account; lookups go via the accounts FK.
func messageDBIDForAccount(t *testing.T, db *DB, msgID, account string) int64 {
	t.Helper()
	var id int64
	if err := db.db.QueryRow(`SELECT m.id FROM messages m
		JOIN accounts ac ON ac.id = m.account_id
		WHERE m.message_id = ? AND ac.name = ?`, msgID, account).Scan(&id); err != nil {
		t.Fatalf("lookup message id (%s, %s): %v", msgID, account, err)
	}
	return id
}

func insertTestMessageForAccount(t *testing.T, db *DB, msgID, account string) int64 {
	t.Helper()
	now := time.Now().Unix()
	msg := &Message{
		MessageID: msgID, Subject: "Test " + msgID,
		FromAddr: "a@x", Date: now, CreatedAt: now,
		FetchedBody: true, Account: account,
	}
	if err := db.InsertMessage(msg); err != nil {
		t.Fatalf("insert %s: %v", msgID, err)
	}
	return msg.ID
}

func TestListTags_AccountScoped(t *testing.T) {
	db := newTestDB(t)

	id1 := insertTestMessageForAccount(t, db, "m1@x", "work")
	id2 := insertTestMessageForAccount(t, db, "m2@x", "gmail")

	db.AddTag(id1, "inbox")
	db.AddTag(id1, "finance")
	db.AddTag(id2, "inbox")
	db.AddTag(id2, "newsletter")

	// All tags
	all, _ := db.ListTags()
	if len(all) != 3 { // finance, inbox, newsletter
		t.Errorf("all tags = %v, want 3", all)
	}

	// Work only
	work, _ := db.ListTags("work")
	if len(work) != 2 {
		t.Fatalf("work tags = %v, want 2", work)
	}
	found := make(map[string]bool)
	for _, tag := range work {
		found[tag] = true
	}
	if !found["inbox"] || !found["finance"] {
		t.Errorf("work tags = %v, want [finance inbox]", work)
	}
	if found["newsletter"] {
		t.Error("work should not have newsletter tag")
	}

	// Gmail only
	gmail, _ := db.ListTags("gmail")
	if len(gmail) != 2 {
		t.Fatalf("gmail tags = %v, want 2", gmail)
	}

	// Both accounts
	both, _ := db.ListTags("work", "gmail")
	if len(both) != 3 {
		t.Errorf("both tags = %v, want 3", both)
	}
}
