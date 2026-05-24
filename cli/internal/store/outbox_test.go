package store

import (
	"testing"
	"time"
)

func TestEnqueueAndDequeue(t *testing.T) {
	db := newTestDB(t)

	id, err := db.Enqueue(`{"to":["bob@x"],"subject":"Hi"}`, 0)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero ID")
	}

	item, err := db.Dequeue()
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if item == nil {
		t.Fatal("expected item")
	}
	if item.ID != id {
		t.Errorf("ID = %d, want %d", item.ID, id)
	}
	if item.DraftJSON != `{"to":["bob@x"],"subject":"Hi"}` {
		t.Errorf("DraftJSON = %q", item.DraftJSON)
	}
	if item.Attempts != 0 {
		t.Errorf("Attempts = %d, want 0", item.Attempts)
	}
}

func TestDequeueEmpty(t *testing.T) {
	db := newTestDB(t)

	item, err := db.Dequeue()
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if item != nil {
		t.Error("expected nil for empty queue")
	}
}

func TestDequeueSendAfter(t *testing.T) {
	db := newTestDB(t)

	// Enqueue with send_after in the future
	future := time.Now().Unix() + 3600
	db.Enqueue(`{"subject":"delayed"}`, future)

	// Should not dequeue yet
	item, _ := db.Dequeue()
	if item != nil {
		t.Error("should not dequeue item with future send_after")
	}

	// Enqueue one with send_after=0 (immediate)
	id2, _ := db.Enqueue(`{"subject":"immediate"}`, 0)

	item, _ = db.Dequeue()
	if item == nil {
		t.Fatal("expected immediate item")
	}
	if item.ID != id2 {
		t.Errorf("got ID %d, want %d (immediate item)", item.ID, id2)
	}
}

func TestMarkAttempted(t *testing.T) {
	db := newTestDB(t)

	id, _ := db.Enqueue(`{"subject":"retry"}`, 0)

	err := db.MarkAttempted(id, "connection refused")
	if err != nil {
		t.Fatalf("mark attempted: %v", err)
	}

	// Verify via ListOutbox
	items, _ := db.ListOutbox()
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if items[0].Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", items[0].Attempts)
	}
	if items[0].LastError != "connection refused" {
		t.Errorf("LastError = %q, want %q", items[0].LastError, "connection refused")
	}
}

func TestDequeueSkipsPoisoned(t *testing.T) {
	db := newTestDB(t)

	id, _ := db.Enqueue(`{"subject":"poison"}`, 0)

	// Mark 5 attempts (poison threshold)
	for i := 0; i < 5; i++ {
		db.MarkAttempted(id, "fail")
	}

	// Should not be dequeued
	item, _ := db.Dequeue()
	if item != nil {
		t.Error("poisoned item should not be dequeued")
	}
}

func TestPoisonOutboxItem(t *testing.T) {
	db := newTestDB(t)

	id, _ := db.Enqueue(`{"subject":"bad"}`, 0)

	err := db.PoisonOutboxItem(id, "permanent failure")
	if err != nil {
		t.Fatalf("poison: %v", err)
	}

	items, _ := db.ListOutbox()
	if items[0].Attempts != 5 {
		t.Errorf("Attempts = %d, want 5", items[0].Attempts)
	}
	if items[0].LastError != "permanent failure" {
		t.Errorf("LastError = %q", items[0].LastError)
	}

	// Should not be dequeued
	item, _ := db.Dequeue()
	if item != nil {
		t.Error("poisoned item should not be dequeued")
	}
}

func TestDeleteOutboxItem(t *testing.T) {
	db := newTestDB(t)

	id, _ := db.Enqueue(`{"subject":"delete me"}`, 0)

	err := db.DeleteOutboxItem(id)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	items, _ := db.ListOutbox()
	if len(items) != 0 {
		t.Errorf("got %d items after delete, want 0", len(items))
	}
}

func TestDeleteOutboxItemNotFound(t *testing.T) {
	db := newTestDB(t)

	err := db.DeleteOutboxItem(999)
	if err == nil {
		t.Error("expected error for nonexistent item")
	}
}

func TestListOutboxOrder(t *testing.T) {
	db := newTestDB(t)

	// Enqueue + override created_at so the ordering is deterministic.
	// Step 7e dropped the plaintext draft_json column, so direct INSERTs
	// that wrote it stopped working — go through Enqueue (which encrypts
	// into draft_json_ct) then patch created_at after the fact.
	id1, _ := db.Enqueue(`{"subject":"first"}`, 0)
	id2, _ := db.Enqueue(`{"subject":"second"}`, 0)
	db.db.Exec("UPDATE outbox SET created_at = ? WHERE id = ?", 1000, id1)
	db.db.Exec("UPDATE outbox SET created_at = ? WHERE id = ?", 2000, id2)

	items, err := db.ListOutbox()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	// Newest first
	if items[0].DraftJSON != `{"subject":"second"}` {
		t.Errorf("first item = %q, want second (newest first)", items[0].DraftJSON)
	}
}

func TestDequeueExponentialBackoff(t *testing.T) {
	db := newTestDB(t)

	id, _ := db.Enqueue(`{"subject":"backoff"}`, 0)

	// First attempt fails
	db.MarkAttempted(id, "timeout")

	// Immediately after attempt 1: backoff = 1*1*30 = 30s
	// Should NOT be dequeued immediately
	item, _ := db.Dequeue()
	if item != nil {
		t.Error("should respect exponential backoff after attempt 1")
	}
}

func TestDequeueOrdersByAttempts(t *testing.T) {
	db := newTestDB(t)

	id1, _ := db.Enqueue(`{"subject":"retried"}`, 0)
	id2, _ := db.Enqueue(`{"subject":"fresh"}`, 0)

	// Mark id1 as attempted once (but make backoff expire by manipulating directly)
	db.MarkAttempted(id1, "fail")
	// Reset last_attempted_at to the past so backoff is satisfied
	db.db.Exec("UPDATE outbox SET last_attempted_at = 0 WHERE id = ?", id1)

	// Fresh item (0 attempts) should come first
	item, _ := db.Dequeue()
	if item == nil {
		t.Fatal("expected item")
	}
	if item.ID != id2 {
		t.Errorf("got ID %d, want %d (fresh item first)", item.ID, id2)
	}
}
