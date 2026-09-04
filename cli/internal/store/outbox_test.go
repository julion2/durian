package store

import (
	"errors"
	"testing"
	"time"
)

func TestEnqueueAndClaim(t *testing.T) {
	db := newTestDB(t)

	id, err := db.Enqueue(`{"to":["bob@x"],"subject":"Hi"}`, 0)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero ID")
	}

	item, err := db.ClaimNextOutboxItem()
	if err != nil {
		t.Fatalf("claim: %v", err)
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
	if !item.InFlight {
		t.Error("claimed item is not marked in-flight")
	}
	if item.DeliveryConfirmed {
		t.Error("newly claimed item is incorrectly marked delivered")
	}
}

func TestEnqueueIdempotentReturnsOriginalItem(t *testing.T) {
	db := newTestDB(t)
	firstID, firstSendAfter, err := db.EnqueueIdempotent(`{"subject":"first"}`, 123, "send-action-1")
	if err != nil {
		t.Fatal(err)
	}
	secondID, secondSendAfter, err := db.EnqueueIdempotent(`{"subject":"duplicate"}`, 456, "send-action-1")
	if err != nil {
		t.Fatal(err)
	}
	if secondID != firstID || firstSendAfter != 123 || secondSendAfter != firstSendAfter {
		t.Fatalf("idempotent enqueue = first (%d, %d), second (%d, %d)", firstID, firstSendAfter, secondID, secondSendAfter)
	}
	items, err := db.ListOutbox()
	if err != nil || len(items) != 1 || items[0].DraftJSON != `{"subject":"first"}` {
		t.Fatalf("idempotent outbox = %#v, %v", items, err)
	}
	if err := db.DeletePendingOutboxItem(firstID); err != nil {
		t.Fatal(err)
	}
	afterDeleteID, _, err := db.EnqueueIdempotent(`{"subject":"late retry"}`, 789, "send-action-1")
	if err != nil || afterDeleteID != firstID {
		t.Fatalf("post-delivery retry id=%d err=%v, want tombstoned id %d", afterDeleteID, err, firstID)
	}
	if items, err := db.ListOutbox(); err != nil || len(items) != 0 {
		t.Fatalf("post-delete idempotent retry recreated outbox: %#v, %v", items, err)
	}
}

func TestClaimEmpty(t *testing.T) {
	db := newTestDB(t)

	item, err := db.ClaimNextOutboxItem()
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if item != nil {
		t.Error("expected nil for empty queue")
	}
}

func TestClaimSendAfter(t *testing.T) {
	db := newTestDB(t)

	// Enqueue with send_after in the future
	future := time.Now().Unix() + 3600
	db.Enqueue(`{"subject":"delayed"}`, future)

	// Should not claim yet
	item, _ := db.ClaimNextOutboxItem()
	if item != nil {
		t.Error("should not claim item with future send_after")
	}

	// Enqueue one with send_after=0 (immediate)
	id2, _ := db.Enqueue(`{"subject":"immediate"}`, 0)

	item, _ = db.ClaimNextOutboxItem()
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
	if _, err := db.ClaimNextOutboxItem(); err != nil {
		t.Fatal(err)
	}

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
	if items[0].InFlight {
		t.Error("failed item remained in-flight")
	}
}

func TestDeferOutboxItemPostponesWithoutCountingAttempt(t *testing.T) {
	db := newTestDB(t)
	id, _ := db.Enqueue(`{"subject":"offline"}`, 0)
	if _, err := db.ClaimNextOutboxItem(); err != nil {
		t.Fatal(err)
	}

	if err := db.DeferOutboxItem(id, time.Now().Unix()+30, "offline"); err != nil {
		t.Fatal(err)
	}
	item, err := db.ClaimNextOutboxItem()
	if err != nil {
		t.Fatal(err)
	}
	if item != nil {
		t.Fatal("deferred item was immediately dequeued")
	}
	items, err := db.ListOutbox()
	if err != nil || len(items) != 1 {
		t.Fatalf("ListOutbox() = %#v, %v", items, err)
	}
	if items[0].Attempts != 0 || items[0].LastError != "offline" {
		t.Fatalf("deferred item = %#v, want zero attempts and recorded error", items[0])
	}
}

func TestClaimSkipsPoisoned(t *testing.T) {
	db := newTestDB(t)

	id, _ := db.Enqueue(`{"subject":"poison"}`, 0)

	// Mark 5 attempts (poison threshold)
	for i := 0; i < 5; i++ {
		item, err := db.ClaimNextOutboxItem()
		if err != nil || item == nil {
			t.Fatalf("claim attempt %d: item=%#v err=%v", i+1, item, err)
		}
		if err := db.MarkAttempted(id, "fail"); err != nil {
			t.Fatal(err)
		}
		_, _ = db.db.Exec("UPDATE outbox SET last_attempted_at = 0 WHERE id = ?", id)
	}

	// Should not be claimed
	item, _ := db.ClaimNextOutboxItem()
	if item != nil {
		t.Error("poisoned item should not be claimed")
	}
}

func TestPoisonOutboxItem(t *testing.T) {
	db := newTestDB(t)

	id, _ := db.Enqueue(`{"subject":"bad"}`, 0)
	if _, err := db.ClaimNextOutboxItem(); err != nil {
		t.Fatal(err)
	}

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

	// Should not be claimed
	item, _ := db.ClaimNextOutboxItem()
	if item != nil {
		t.Error("poisoned item should not be claimed")
	}
}

func TestDeletePendingOutboxItem(t *testing.T) {
	db := newTestDB(t)

	id, _ := db.Enqueue(`{"subject":"delete me"}`, 0)

	err := db.DeletePendingOutboxItem(id)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	items, _ := db.ListOutbox()
	if len(items) != 0 {
		t.Errorf("got %d items after delete, want 0", len(items))
	}
}

func TestDeletePendingOutboxItemNotFound(t *testing.T) {
	db := newTestDB(t)

	err := db.DeletePendingOutboxItem(999)
	if !errors.Is(err, ErrOutboxItemNotFound) {
		t.Errorf("error = %v, want ErrOutboxItemNotFound", err)
	}
}

func TestClaimWinsRaceWithPendingDelete(t *testing.T) {
	db := newTestDB(t)
	id, _ := db.Enqueue(`{"subject":"already sending"}`, 0)
	if item, err := db.ClaimNextOutboxItem(); err != nil || item == nil || item.ID != id {
		t.Fatalf("claim = %#v, %v", item, err)
	}

	if err := db.DeletePendingOutboxItem(id); !errors.Is(err, ErrOutboxItemInFlight) {
		t.Fatalf("pending delete error = %v, want ErrOutboxItemInFlight", err)
	}
	items, err := db.ListOutbox()
	if err != nil || len(items) != 1 || !items[0].InFlight {
		t.Fatalf("claimed row after rejected delete = %#v, %v", items, err)
	}
	if err := db.DeleteClaimedOutboxItem(id); err != nil {
		t.Fatalf("delete claimed item: %v", err)
	}
}

func TestClaimedDeleteRejectsPendingItem(t *testing.T) {
	db := newTestDB(t)
	id, _ := db.Enqueue(`{"subject":"not sending"}`, 0)
	if err := db.DeleteClaimedOutboxItem(id); !errors.Is(err, ErrOutboxItemNotInFlight) {
		t.Fatalf("claimed delete error = %v, want ErrOutboxItemNotInFlight", err)
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

func TestClaimExponentialBackoff(t *testing.T) {
	db := newTestDB(t)

	id, _ := db.Enqueue(`{"subject":"backoff"}`, 0)

	// First attempt fails
	if _, err := db.ClaimNextOutboxItem(); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkAttempted(id, "timeout"); err != nil {
		t.Fatal(err)
	}

	// Immediately after attempt 1: backoff = 1*1*30 = 30s
	// Should NOT be claimed immediately
	item, _ := db.ClaimNextOutboxItem()
	if item != nil {
		t.Error("should respect exponential backoff after attempt 1")
	}
}

func TestClaimOrdersByAttempts(t *testing.T) {
	db := newTestDB(t)

	id1, _ := db.Enqueue(`{"subject":"retried"}`, 0)
	id2, _ := db.Enqueue(`{"subject":"fresh"}`, 0)

	// Mark id1 as attempted once (but make backoff expire by manipulating directly)
	if _, err := db.ClaimNextOutboxItem(); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkAttempted(id1, "fail"); err != nil {
		t.Fatal(err)
	}
	// Reset last_attempted_at to the past so backoff is satisfied
	db.db.Exec("UPDATE outbox SET last_attempted_at = 0 WHERE id = ?", id1)

	// Fresh item (0 attempts) should come first
	item, _ := db.ClaimNextOutboxItem()
	if item == nil {
		t.Fatal("expected item")
	}
	if item.ID != id2 {
		t.Errorf("got ID %d, want %d (fresh item first)", item.ID, id2)
	}
}

func TestClaimIsDurableAndNeverAutomaticallyReclaimed(t *testing.T) {
	path := t.TempDir() + "/mail.db"
	kr := testKeyring(t)
	db, err := Open(path, kr)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Init(); err != nil {
		t.Fatal(err)
	}
	id, err := db.Enqueue(`{"subject":"possibly sent"}`, 0)
	if err != nil {
		t.Fatal(err)
	}
	item, err := db.ClaimNextOutboxItem()
	if err != nil || item == nil || item.ID != id || !item.InFlight {
		t.Fatalf("first claim = %#v, %v", item, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(path, kr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Init(); err != nil {
		t.Fatal(err)
	}
	item, err = db.ClaimNextOutboxItem()
	if err != nil {
		t.Fatal(err)
	}
	if item != nil {
		t.Fatalf("claimed item was automatically reclaimed after restart: %#v", item)
	}
	items, err := db.ListOutbox()
	if err != nil || len(items) != 1 || !items[0].InFlight {
		t.Fatalf("persisted outbox = %#v, %v", items, err)
	}
}

func TestOutboxFailureTransitionsRequireClaim(t *testing.T) {
	db := newTestDB(t)
	id, _ := db.Enqueue(`{"subject":"ready"}`, 0)
	if err := db.MarkAttempted(id, "must not apply"); err == nil {
		t.Fatal("unclaimed item was marked attempted")
	}
	if err := db.DeferOutboxItem(id, time.Now().Unix()+30, "must not apply"); err == nil {
		t.Fatal("unclaimed item was deferred")
	}
	if err := db.PoisonOutboxItem(id, "must not apply"); err == nil {
		t.Fatal("unclaimed item was poisoned")
	}
	if err := db.UpdateClaimedOutboxDraft(id, `{"message_id":"<nope@example.com>"}`); err == nil {
		t.Fatal("unclaimed draft correlation metadata was updated")
	}
	if err := db.MarkOutboxReconciliationRequired(id, "must not apply"); err == nil {
		t.Fatal("unclaimed item was marked for reconciliation")
	}
	if err := db.RequeueClaimedOutboxItem(id, "must not apply"); err == nil {
		t.Fatal("unclaimed item was requeued")
	}
}

func TestOutboxReconciliationPreservesCorrelationAndRequiresExplicitRequeue(t *testing.T) {
	db := newTestDB(t)
	id, _ := db.Enqueue(`{"subject":"legacy"}`, 0)
	if item, err := db.ClaimNextOutboxItem(); err != nil || item == nil {
		t.Fatalf("claim = %#v, %v", item, err)
	}
	updated := `{"message_id":"<correlation@example.com>","subject":"legacy"}`
	if err := db.UpdateClaimedOutboxDraft(id, updated); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkOutboxReconciliationRequired(id, "verify provider"); err != nil {
		t.Fatal(err)
	}
	items, err := db.ListOutbox()
	if err != nil || len(items) != 1 || !items[0].InFlight || items[0].DeliveryConfirmed || items[0].DraftJSON != updated || items[0].LastError != "verify provider" {
		t.Fatalf("reconciliation state = %#v, %v", items, err)
	}
	if item, err := db.ClaimNextOutboxItem(); err != nil || item != nil {
		t.Fatalf("reconciliation item was automatically reclaimed: %#v, %v", item, err)
	}
	if err := db.RequeueClaimedOutboxItem(id, "verified not delivered"); err != nil {
		t.Fatal(err)
	}
	if item, err := db.ClaimNextOutboxItem(); err != nil || item == nil || item.ID != id {
		t.Fatalf("explicitly requeued claim = %#v, %v", item, err)
	}
}

func TestConfirmedOutboxDeliveryCannotBeRequeued(t *testing.T) {
	db := newTestDB(t)
	id, _ := db.Enqueue(`{"message_id":"<confirmed@example.com>"}`, 0)
	if item, err := db.ClaimNextOutboxItem(); err != nil || item == nil {
		t.Fatalf("claim = %#v, %v", item, err)
	}
	if err := db.MarkOutboxDeliveryConfirmed(id, "delivered; Sent filing failed"); err != nil {
		t.Fatal(err)
	}
	items, err := db.ListOutbox()
	if err != nil || len(items) != 1 || !items[0].InFlight || !items[0].DeliveryConfirmed {
		t.Fatalf("confirmed delivery state = %#v, %v", items, err)
	}
	if err := db.RequeueClaimedOutboxItem(id, "incorrect"); !errors.Is(err, ErrOutboxDeliveryConfirmed) {
		t.Fatalf("confirmed delivery requeue error = %v, want ErrOutboxDeliveryConfirmed", err)
	}
	if err := db.DeleteClaimedOutboxItem(id); err != nil {
		t.Fatal(err)
	}
}
