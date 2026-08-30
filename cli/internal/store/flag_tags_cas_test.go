package store

import (
	"slices"
	"testing"
	"time"
)

// flagWatch is the tag set a flag-sync caller's decision depends on. Tests
// use it as the CAS scope so anything outside it must be invisible to the
// comparison.
var flagWatch = []string{"unread", "flagged"}

// mustAddTag seeds a tag and fails the test if the seeding itself failed. A
// silently dropped seed would leave the CAS comparing against a state the test
// never actually created, which is the one way these assertions could pass for
// the wrong reason.
func mustAddTag(t *testing.T, db *DB, id int64, tags ...string) {
	t.Helper()
	for _, tag := range tags {
		if err := db.AddTag(id, tag); err != nil {
			t.Fatalf("seed tag %q: %v", tag, err)
		}
	}
}

// mustInsertMessage is the same guarantee for a row the test depends on.
func mustInsertMessage(t *testing.T, db *DB, msg *Message) {
	t.Helper()
	if err := db.InsertMessage(msg); err != nil {
		t.Fatalf("insert %s for %s: %v", msg.MessageID, msg.Account, err)
	}
}

func TestModifyFlagTagsIfUnchanged_AppliesWhenSnapshotHolds(t *testing.T) {
	db := newTestDB(t)
	id := insertTestMessageForAccount(t, db, "cas-ok@x", "work")

	mustAddTag(t, db, id, "unread")
	mustAddTag(t, db, id, "flagged")

	applied, err := db.ModifyFlagTagsIfUnchanged("cas-ok@x", "work",
		flagWatch, []string{"unread", "flagged"},
		[]string{"replied"}, []string{"unread"})
	if err != nil {
		t.Fatalf("modify: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true: watched tags match the snapshot exactly")
	}

	// Both halves of the op must have landed: the add and the remove.
	tags, err := db.GetMessageTags(id)
	if err != nil {
		t.Fatalf("get tags: %v", err)
	}
	if !slices.Equal(tags, []string{"flagged", "replied"}) {
		t.Errorf("tags = %v, want [flagged replied]", tags)
	}
}

func TestModifyFlagTagsIfUnchanged_MissWritesNothing(t *testing.T) {
	db := newTestDB(t)
	id := insertTestMessageForAccount(t, db, "cas-miss@x", "work")

	// The caller snapshotted [unread], but "flagged" landed in between —
	// exactly the race the CAS exists to catch.
	mustAddTag(t, db, id, "unread")
	mustAddTag(t, db, id, "flagged")

	before, err := db.GetMessageTags(id)
	if err != nil {
		t.Fatalf("get tags before: %v", err)
	}

	applied, err := db.ModifyFlagTagsIfUnchanged("cas-miss@x", "work",
		flagWatch, []string{"unread"},
		[]string{"replied"}, []string{"unread"})
	if err != nil {
		t.Fatalf("modify: %v", err)
	}
	if applied {
		t.Fatal("applied = true, want false: watched state moved past the snapshot")
	}

	// A miss must leave the row exactly as it was — a half-applied add
	// would corrupt the state the caller is about to re-read and retry on.
	after, err := db.GetMessageTags(id)
	if err != nil {
		t.Fatalf("get tags after: %v", err)
	}
	if !slices.Equal(after, before) {
		t.Errorf("tags changed on CAS miss: before %v, after %v", before, after)
	}
	if slices.Contains(after, "replied") {
		t.Error("addTags leaked through a failed CAS")
	}
}

func TestModifyFlagTagsIfUnchanged_IgnoresForeignTags(t *testing.T) {
	db := newTestDB(t)
	id := insertTestMessageForAccount(t, db, "cas-foreign@x", "work")

	// Tags outside watch — mailbox tags and a rule tag that a concurrent
	// rule run could add mid-sync. None of these may look like a flag
	// conflict, or every rule firing would abort the flag merge.
	mustAddTag(t, db, id, "unread")
	mustAddTag(t, db, id, "inbox")
	mustAddTag(t, db, id, "important")
	mustAddTag(t, db, id, "newsletter")

	applied, err := db.ModifyFlagTagsIfUnchanged("cas-foreign@x", "work",
		flagWatch, []string{"unread"},
		[]string{"flagged"}, []string{"unread"})
	if err != nil {
		t.Fatalf("modify: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true: unwatched tags must not affect the comparison")
	}

	tags, err := db.GetMessageTags(id)
	if err != nil {
		t.Fatalf("get tags: %v", err)
	}
	if !slices.Equal(tags, []string{"flagged", "important", "inbox", "newsletter"}) {
		t.Errorf("tags = %v, want [flagged important inbox newsletter]", tags)
	}
}

func TestModifyFlagTagsIfUnchanged_ScopedToAccount(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	// Same Message-ID under two accounts. The personal row carries
	// "flagged", which would fail the CAS if the function ever compared
	// against the wrong account's row.
	mustInsertMessage(t, db, &Message{
		MessageID: "cas-acct@x", Subject: "Acct scope",
		FromAddr: "a@x", Date: now, CreatedAt: now, FetchedBody: true,
		Mailbox: "INBOX", Account: "work", RemoteRef: "r1",
	})
	mustInsertMessage(t, db, &Message{
		MessageID: "cas-acct@x", Subject: "Acct scope",
		FromAddr: "a@x", Date: now, CreatedAt: now, FetchedBody: true,
		Mailbox: "INBOX", Account: "personal", RemoteRef: "r2",
	})
	workID := messageDBIDForAccount(t, db, "cas-acct@x", "work")
	persID := messageDBIDForAccount(t, db, "cas-acct@x", "personal")

	mustAddTag(t, db, workID, "unread")
	mustAddTag(t, db, persID, "unread", "flagged")

	applied, err := db.ModifyFlagTagsIfUnchanged("cas-acct@x", "work",
		flagWatch, []string{"unread"},
		[]string{"flagged"}, []string{"unread"})
	if err != nil {
		t.Fatalf("modify: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true: work row matched, personal must not bleed in")
	}

	workTags, err := db.GetMessageTags(workID)
	if err != nil {
		t.Fatalf("get work tags: %v", err)
	}
	if !slices.Equal(workTags, []string{"flagged"}) {
		t.Errorf("work tags = %v, want [flagged]", workTags)
	}

	// The other account's row must be untouched by a write scoped to work.
	persTags, err := db.GetMessageTags(persID)
	if err != nil {
		t.Fatalf("get personal tags: %v", err)
	}
	if !slices.Equal(persTags, []string{"flagged", "unread"}) {
		t.Errorf("personal tags = %v, want [flagged unread]", persTags)
	}
}

func TestModifyFlagTagsIfUnchanged_ExpectedOrderIrrelevant(t *testing.T) {
	db := newTestDB(t)
	id := insertTestMessageForAccount(t, db, "cas-order@x", "work")

	mustAddTag(t, db, id, "flagged")
	mustAddTag(t, db, id, "unread")

	// expected in reverse of sorted order — callers build it from a
	// snapshot whose ordering is not guaranteed, so a set comparison is
	// what makes this API usable.
	applied, err := db.ModifyFlagTagsIfUnchanged("cas-order@x", "work",
		flagWatch, []string{"unread", "flagged"},
		nil, []string{"unread"})
	if err != nil {
		t.Fatalf("modify: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true: expected order must not matter")
	}

	tags, err := db.GetMessageTags(id)
	if err != nil {
		t.Fatalf("get tags: %v", err)
	}
	if !slices.Equal(tags, []string{"flagged"}) {
		t.Errorf("tags = %v, want [flagged]", tags)
	}
}

func TestModifyFlagTagsIfUnchanged_MissingRowIsNoOp(t *testing.T) {
	db := newTestDB(t)
	insertTestMessageForAccount(t, db, "cas-exists@x", "work")

	// Message unknown under a known account. Reported as applied so the
	// caller can advance past a row that no longer exists (e.g. expunged),
	// rather than retrying it forever.
	applied, err := db.ModifyFlagTagsIfUnchanged("cas-gone@x", "work",
		flagWatch, nil, []string{"unread"}, nil)
	if err != nil {
		t.Fatalf("missing message: %v", err)
	}
	if !applied {
		t.Error("missing message: applied = false, want true (no-op)")
	}
	if tags, _ := db.GetTagsByMessageID("cas-gone@x"); len(tags) != 0 {
		t.Errorf("missing message grew tags: %v", tags)
	}

	// Account not in the accounts table at all.
	applied, err = db.ModifyFlagTagsIfUnchanged("cas-exists@x", "nobody",
		flagWatch, nil, []string{"unread"}, nil)
	if err != nil {
		t.Fatalf("unknown account: %v", err)
	}
	if !applied {
		t.Error("unknown account: applied = false, want true (no-op)")
	}
	// The message does exist under "work" — the unknown-account no-op must
	// not have written to it.
	if tags, _ := db.GetTagsByMessageID("cas-exists@x"); len(tags) != 0 {
		t.Errorf("unknown account wrote tags to existing row: %v", tags)
	}
}
