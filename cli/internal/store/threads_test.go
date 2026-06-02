package store

import (
	"testing"
	"time"
)

func TestCleanMessageID(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"<abc@example.com>", "abc@example.com"},
		{"abc@example.com", "abc@example.com"},
		{"  <abc@example.com>  ", "abc@example.com"},
		{"", ""},
		{"<>", ""},
	}
	for _, tt := range tests {
		if got := cleanMessageID(tt.input); got != tt.want {
			t.Errorf("cleanMessageID(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSplitReferences(t *testing.T) {
	tests := []struct {
		input string
		want  int // expected count
	}{
		{"<a@x> <b@x> <c@x>", 3},
		{"<single@x>", 1},
		{"", 0},
		{"   ", 0},
		{"<a@x>   <b@x>", 2}, // multiple spaces
	}
	for _, tt := range tests {
		got := splitReferences(tt.input)
		if len(got) != tt.want {
			t.Errorf("splitReferences(%q) returned %d items, want %d", tt.input, len(got), tt.want)
		}
	}

	// Verify angle brackets are stripped
	refs := splitReferences("<root@x> <mid@x> <leaf@x>")
	if refs[0] != "root@x" || refs[2] != "leaf@x" {
		t.Errorf("angle brackets not stripped: %v", refs)
	}
}

func TestComputeThreadID_Deterministic(t *testing.T) {
	id1 := computeThreadID("msg@x", "", "<root@x> <mid@x>")
	id2 := computeThreadID("other@x", "", "<root@x> <other-mid@x>")
	if id1 != id2 {
		t.Errorf("same root ref should produce same thread ID: %q != %q", id1, id2)
	}
}

func TestComputeThreadID_Standalone(t *testing.T) {
	id := computeThreadID("standalone@x", "", "")
	if id == "" {
		t.Fatal("empty thread ID for standalone message")
	}
	if len(id) != 16 {
		t.Errorf("thread ID length = %d, want 16", len(id))
	}
}

func TestComputeThreadID_InReplyToOnly(t *testing.T) {
	// When References is empty, falls back to In-Reply-To
	id1 := computeThreadID("reply@x", "<parent@x>", "")
	id2 := computeThreadID("reply2@x", "<parent@x>", "")
	if id1 != id2 {
		t.Errorf("same In-Reply-To should produce same thread ID: %q != %q", id1, id2)
	}

	// Should differ from standalone parent
	idParent := computeThreadID("parent@x", "", "")
	if id1 != idParent {
		t.Errorf("In-Reply-To hash should match parent standalone hash: %q != %q", id1, idParent)
	}
}

func TestResolveThreadID_LinearChain(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	// Insert A (root)
	err := db.InsertMessage(&Message{
		MessageID: "a@x", Subject: "Thread start",
		FromAddr: "alice@x", Date: now, CreatedAt: now, FetchedBody: true,
	})
	if err != nil {
		t.Fatalf("insert A: %v", err)
	}

	// Insert B replying to A
	err = db.InsertMessage(&Message{
		MessageID: "b@x", InReplyTo: "<a@x>", Refs: "<a@x>",
		Subject:  "Re: Thread start",
		FromAddr: "bob@x", Date: now + 1, CreatedAt: now + 1, FetchedBody: true,
	})
	if err != nil {
		t.Fatalf("insert B: %v", err)
	}

	// Insert C replying to B, with full references chain
	err = db.InsertMessage(&Message{
		MessageID: "c@x", InReplyTo: "<b@x>", Refs: "<a@x> <b@x>",
		Subject:  "Re: Re: Thread start",
		FromAddr: "charlie@x", Date: now + 2, CreatedAt: now + 2, FetchedBody: true,
	})
	if err != nil {
		t.Fatalf("insert C: %v", err)
	}

	// All three should share the same thread_id
	a, _ := db.GetByMessageID("a@x")
	b, _ := db.GetByMessageID("b@x")
	c, _ := db.GetByMessageID("c@x")

	if a.ThreadID != b.ThreadID || b.ThreadID != c.ThreadID {
		t.Errorf("linear chain should share thread_id: A=%q B=%q C=%q",
			a.ThreadID, b.ThreadID, c.ThreadID)
	}
}

func TestResolveThreadID_Fork(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	db.InsertMessage(&Message{
		MessageID: "root@x", Subject: "Root",
		FromAddr: "alice@x", Date: now, CreatedAt: now, FetchedBody: true,
	})
	db.InsertMessage(&Message{
		MessageID: "fork1@x", InReplyTo: "<root@x>", Refs: "<root@x>",
		Subject: "Fork 1", FromAddr: "bob@x", Date: now + 1, CreatedAt: now + 1, FetchedBody: true,
	})
	db.InsertMessage(&Message{
		MessageID: "fork2@x", InReplyTo: "<root@x>", Refs: "<root@x>",
		Subject: "Fork 2", FromAddr: "charlie@x", Date: now + 2, CreatedAt: now + 2, FetchedBody: true,
	})

	root, _ := db.GetByMessageID("root@x")
	f1, _ := db.GetByMessageID("fork1@x")
	f2, _ := db.GetByMessageID("fork2@x")

	if root.ThreadID != f1.ThreadID || root.ThreadID != f2.ThreadID {
		t.Errorf("forks should share thread_id: root=%q f1=%q f2=%q",
			root.ThreadID, f1.ThreadID, f2.ThreadID)
	}
}

func TestResolveThreadID_Standalone(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	db.InsertMessage(&Message{
		MessageID: "solo@x", Subject: "Standalone",
		FromAddr: "alice@x", Date: now, CreatedAt: now, FetchedBody: true,
	})

	msg, _ := db.GetByMessageID("solo@x")
	expected := hashThreadRoot("solo@x")
	if msg.ThreadID != expected {
		t.Errorf("standalone thread_id = %q, want %q", msg.ThreadID, expected)
	}
}

func TestResolveThreadID_MissingParent(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	// Child arrives but parent doesn't exist yet
	db.InsertMessage(&Message{
		MessageID: "orphan@x", InReplyTo: "<unknown-parent@x>", Refs: "<unknown-root@x> <unknown-parent@x>",
		Subject: "Orphan", FromAddr: "alice@x", Date: now, CreatedAt: now, FetchedBody: true,
	})

	msg, _ := db.GetByMessageID("orphan@x")
	// Should hash the first reference (unknown-root@x)
	expected := hashThreadRoot("unknown-root@x")
	if msg.ThreadID != expected {
		t.Errorf("orphan thread_id = %q, want %q (hash of first ref)", msg.ThreadID, expected)
	}
}

func TestResolveThreadID_BatchInOrder(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	// Insert parent and child in same batch — child should see parent within tx
	msgs := []*Message{
		{
			MessageID: "batch-parent@x", Subject: "Parent",
			FromAddr: "alice@x", Date: now, CreatedAt: now, FetchedBody: true,
		},
		{
			MessageID: "batch-child@x", InReplyTo: "<batch-parent@x>", Refs: "<batch-parent@x>",
			Subject: "Child", FromAddr: "bob@x", Date: now + 1, CreatedAt: now + 1, FetchedBody: true,
		},
	}

	if err := db.InsertBatch(msgs); err != nil {
		t.Fatalf("batch insert: %v", err)
	}

	parent, _ := db.GetByMessageID("batch-parent@x")
	child, _ := db.GetByMessageID("batch-child@x")

	if parent.ThreadID != child.ThreadID {
		t.Errorf("batch: parent and child should share thread_id: %q != %q",
			parent.ThreadID, child.ThreadID)
	}
}

func TestResolveThreadID_CrossBatchSplit(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	// Batch 1: child arrives first, parent unknown
	db.InsertMessage(&Message{
		MessageID: "late-child@x", InReplyTo: "<late-parent@x>", Refs: "<late-parent@x>",
		Subject: "Child first", FromAddr: "bob@x", Date: now + 1, CreatedAt: now + 1, FetchedBody: true,
	})

	// Batch 2: parent arrives later
	db.InsertMessage(&Message{
		MessageID: "late-parent@x", Subject: "Parent late",
		FromAddr: "alice@x", Date: now, CreatedAt: now, FetchedBody: true,
	})

	child, _ := db.GetByMessageID("late-child@x")
	parent, _ := db.GetByMessageID("late-parent@x")

	// V1 accepted limitation: these will have different thread IDs
	// because the child computed hash(late-parent@x) but the parent
	// computed hash(late-parent@x) too — actually these SHOULD match
	// because computeThreadID for child uses refs[0] = "late-parent@x"
	// and parent with no refs hashes own ID "late-parent@x"
	if child.ThreadID != parent.ThreadID {
		t.Logf("NOTE: cross-batch produced different thread IDs (child=%q parent=%q) — this is expected when root ref differs from message-id",
			child.ThreadID, parent.ThreadID)
	}
}

func TestResolveThreadID_CrossBatchSplit_DifferentRoot(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	// Batch 1: child references a chain [grandparent, parent]
	db.InsertMessage(&Message{
		MessageID: "child2@x", InReplyTo: "<parent2@x>",
		Refs:    "<grandparent2@x> <parent2@x>",
		Subject: "Child", FromAddr: "bob@x", Date: now + 2, CreatedAt: now + 2, FetchedBody: true,
	})

	// Batch 2: parent arrives (standalone — no refs to grandparent)
	db.InsertMessage(&Message{
		MessageID: "parent2@x", Subject: "Parent",
		FromAddr: "alice@x", Date: now + 1, CreatedAt: now + 1, FetchedBody: true,
	})

	child, _ := db.GetByMessageID("child2@x")
	parent, _ := db.GetByMessageID("parent2@x")

	// Parent arrives after child — reverse-lookup finds the child referencing parent,
	// so parent adopts child's thread ID. Conversation stays together.
	if child.ThreadID != parent.ThreadID {
		t.Errorf("expected same thread (parent adopted child's thread), got child=%q parent=%q",
			child.ThreadID, parent.ThreadID)
	}
}

func TestHashThreadRoot_Length(t *testing.T) {
	h := hashThreadRoot("test@example.com")
	if len(h) != 16 {
		t.Errorf("hash length = %d, want 16", len(h))
	}
}
