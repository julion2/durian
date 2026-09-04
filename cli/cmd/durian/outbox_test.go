package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/julion2/durian/cli/internal/dbcrypto"
	"github.com/julion2/durian/cli/internal/store"
)

func TestOutboxCLIListsDurableReconciliationState(t *testing.T) {
	db := newOutboxCLIStore(t)
	id := enqueueOutboxCLIItem(t, db, `<correlation@example.test>`)
	if item, err := db.ClaimNextOutboxItem(); err != nil || item == nil || item.ID != id {
		t.Fatalf("claim = %+v, %v", item, err)
	}
	if err := db.MarkOutboxReconciliationRequired(id, "Provider outcome requires verification"); err != nil {
		t.Fatal(err)
	}
	entries, err := listOutboxEntries(db)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	entry := entries[0]
	if entry.ID != id || entry.MessageID != `<correlation@example.test>` || entry.Status != "verification-required" ||
		!entry.InFlight || entry.DeliveryConfirmed || entry.LastError == "" {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestOutboxCLIReconcilesOnlyClaimedVerifiedItems(t *testing.T) {
	t.Run("delivered removes claim", func(t *testing.T) {
		db := newOutboxCLIStore(t)
		id := enqueueOutboxCLIItem(t, db, `<delivered@example.test>`)
		if _, err := db.ClaimNextOutboxItem(); err != nil {
			t.Fatal(err)
		}
		entry, err := outboxEntryForReconciliation(db, id, "delivered")
		if err != nil {
			t.Fatal(err)
		}
		if status, err := reconcileOutboxEntry(db, entry, "delivered"); err != nil || status != "removed as delivered" {
			t.Fatalf("reconcile = %q, %v", status, err)
		}
		if entries, err := listOutboxEntries(db); err != nil || len(entries) != 0 {
			t.Fatalf("remaining entries = %+v, %v", entries, err)
		}
	})

	t.Run("not delivered requeues claim", func(t *testing.T) {
		db := newOutboxCLIStore(t)
		id := enqueueOutboxCLIItem(t, db, `<retry@example.test>`)
		if _, err := db.ClaimNextOutboxItem(); err != nil {
			t.Fatal(err)
		}
		entry, err := outboxEntryForReconciliation(db, id, "not-delivered")
		if err != nil {
			t.Fatal(err)
		}
		if status, err := reconcileOutboxEntry(db, entry, "not-delivered"); err != nil || status != "requeued as not delivered" {
			t.Fatalf("reconcile = %q, %v", status, err)
		}
		entries, err := listOutboxEntries(db)
		if err != nil || len(entries) != 1 || entries[0].InFlight || entries[0].Status != "queued" {
			t.Fatalf("requeued entries = %+v, %v", entries, err)
		}
	})

	t.Run("pending item rejected", func(t *testing.T) {
		db := newOutboxCLIStore(t)
		id := enqueueOutboxCLIItem(t, db, `<pending@example.test>`)
		if _, err := outboxEntryForReconciliation(db, id, "delivered"); !errors.Is(err, store.ErrOutboxItemNotInFlight) {
			t.Fatalf("pending reconciliation error = %v", err)
		}
	})

	t.Run("confirmed delivery cannot be requeued", func(t *testing.T) {
		db := newOutboxCLIStore(t)
		id := enqueueOutboxCLIItem(t, db, `<confirmed@example.test>`)
		if _, err := db.ClaimNextOutboxItem(); err != nil {
			t.Fatal(err)
		}
		if err := db.MarkOutboxDeliveryConfirmed(id, "Provider accepted delivery"); err != nil {
			t.Fatal(err)
		}
		if _, err := outboxEntryForReconciliation(db, id, "not-delivered"); !errors.Is(err, store.ErrOutboxDeliveryConfirmed) {
			t.Fatalf("confirmed requeue error = %v", err)
		}
	})

	t.Run("missing correlation ID rejected", func(t *testing.T) {
		db := newOutboxCLIStore(t)
		id := enqueueOutboxCLIItem(t, db, "")
		if _, err := db.ClaimNextOutboxItem(); err != nil {
			t.Fatal(err)
		}
		if _, err := outboxEntryForReconciliation(db, id, "delivered"); err == nil || !strings.Contains(err.Error(), "no durable Message-ID") {
			t.Fatalf("missing Message-ID error = %v", err)
		}
	})

	t.Run("explicit outcome required", func(t *testing.T) {
		db := newOutboxCLIStore(t)
		id := enqueueOutboxCLIItem(t, db, `<outcome@example.test>`)
		if _, err := outboxEntryForReconciliation(db, id, "not_delivered"); err == nil || !strings.Contains(err.Error(), "--outcome") {
			t.Fatalf("invalid outcome error = %v", err)
		}
	})
}

func TestOutboxCLIRequiresConfirmationForNoninteractiveMutation(t *testing.T) {
	previous := noInput
	noInput = true
	t.Cleanup(func() { noInput = previous })
	entry := outboxCLIEntry{ID: 1, MessageID: `<verify@example.test>`, InFlight: true}
	if confirmed, err := confirmOutboxReconciliation(entry, "not-delivered", false); err == nil || confirmed || !strings.Contains(err.Error(), "pass --yes") {
		t.Fatalf("noninteractive confirmation = %v, %v", confirmed, err)
	}
	if confirmed, err := confirmOutboxReconciliation(entry, "not-delivered", true); err != nil || !confirmed {
		t.Fatalf("explicit confirmation = %v, %v", confirmed, err)
	}
}

func newOutboxCLIStore(t *testing.T) *store.DB {
	t.Helper()
	keyring, err := dbcrypto.NewKeyring(bytes.Repeat([]byte{0x5a}, dbcrypto.MasterKeyLen))
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(":memory:", keyring)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Init(); err != nil {
		t.Fatal(err)
	}
	return db
}

func enqueueOutboxCLIItem(t *testing.T, db *store.DB, messageID string) int64 {
	t.Helper()
	draft := `{"from":"sender@example.test","to":["recipient@example.test"],"message_id":` + strconvQuote(messageID) + `}`
	id, err := db.Enqueue(draft, 0)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func strconvQuote(value string) string {
	quoted, _ := json.Marshal(value)
	return string(quoted)
}
