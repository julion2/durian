package store

import (
	"slices"
	"testing"
	"time"
)

func TestMigrateLegacyProviderIdentityScopePreservesLocalState(t *testing.T) {
	db := newTestDB(t)
	msg := &Message{
		StableID: "email-1", MessageID: "legacy@example.test", RemoteRef: "email-1",
		Account: "work", Mailbox: "JMAP-ALL", Date: 1, CreatedAt: 1,
	}
	if err := db.InsertMessage(msg); err != nil {
		t.Fatal(err)
	}
	if err := db.AddTag(msg.ID, "local-only"); err != nil {
		t.Fatal(err)
	}
	if err := db.ModifyTagsByMessageDBIDAndJournal(msg.ID, []string{"flagged"}, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSyncedFlagsByDBID(msg.ID, `\Seen`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSyncedLabelsByDBID(msg.ID, "inbox,project"); err != nil {
		t.Fatal(err)
	}

	const prefix = "account-scope:"
	if err := db.MigrateLegacyProviderIdentityScope("work", "JMAP-ALL", prefix); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateLegacyProviderIdentityScope("work", "JMAP-ALL", prefix); err != nil {
		t.Fatalf("idempotent migration: %v", err)
	}
	scoped, err := db.GetByRemoteRef("work", "JMAP-ALL", prefix+"email-1")
	if err != nil || scoped == nil {
		t.Fatalf("scoped row = %#v, err=%v", scoped, err)
	}
	if scoped.ID != msg.ID || scoped.StableID != prefix+"email-1" {
		t.Fatalf("scoped identity = %#v, want original row %d", scoped, msg.ID)
	}
	tags, err := db.GetMessageTags(msg.ID)
	if err != nil || !slices.Equal(tags, []string{"flagged", "local-only"}) {
		t.Fatalf("preserved tags = %v, err=%v", tags, err)
	}
	flags, err := db.GetFolderFlagState("work", "JMAP-ALL")
	if err != nil || len(flags) != 1 || flags[0].SyncedFlags != `\Seen` {
		t.Fatalf("preserved flag baseline = %+v, err=%v", flags, err)
	}
	labels, err := db.GetLabelState("work")
	if err != nil || len(labels) != 1 || labels[0].SyncedLabels != "inbox,project" {
		t.Fatalf("preserved label baseline = %+v, err=%v", labels, err)
	}
	mutations, err := db.ReadProviderTagMutations("work")
	if err != nil || len(mutations) != 1 || mutations[0].RowID != msg.ID || mutations[0].RemoteRef != prefix+"email-1" {
		t.Fatalf("preserved provider mutation = %+v, err=%v", mutations, err)
	}
}

// TestMixedStableAndFallbackIdentityIsAmbiguous covers the store that a real
// multi-account setup produces: a JMAP object carrying a stable id beside an
// IMAP row that has none, both holding the same RFC Message-ID.
//
// Ambiguity here is a property of the stored rows, not of their identity kind.
// Resolving to the single stable row would hand the caller the wrong object's
// body and attachments whenever it meant the IMAP one — and the IMAP row would
// have no working Message-ID lookup at all. Both must stay reachable by their
// row id, and the raw lookup must refuse to guess.
func TestMixedStableAndFallbackIdentityIsAmbiguous(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	stable := &Message{
		StableID: "jmap-obj", MessageID: "mixed@example.com", Subject: "From JMAP",
		Date: now, CreatedAt: now, Mailbox: "ALL", Account: "work",
		RemoteRef: "jmap-obj", FetchedBody: true, BodyText: "jmap body",
	}
	fallback := &Message{
		MessageID: "mixed@example.com", Subject: "From IMAP",
		Date: now + 1, CreatedAt: now + 1, Mailbox: "INBOX", Account: "personal",
		RemoteRef: "42", FetchedBody: true, BodyText: "imap body",
	}
	if err := db.InsertMessage(stable); err != nil {
		t.Fatalf("insert stable: %v", err)
	}
	if err := db.InsertMessage(fallback); err != nil {
		t.Fatalf("insert fallback: %v", err)
	}
	if stable.ID == fallback.ID {
		t.Fatalf("both identities collapsed to row %d", stable.ID)
	}

	if _, err := db.GetByMessageID("mixed@example.com"); err == nil {
		t.Error("raw Message-ID lookup resolved a shared id instead of reporting ambiguity")
	}

	// Each row still has to be reachable by its own identity, or the ambiguity
	// guard would just have made the messages unopenable.
	gotStable, err := db.GetByDBID(stable.ID)
	if err != nil || gotStable == nil {
		t.Fatalf("get stable row: %+v err=%v", gotStable, err)
	}
	if gotStable.BodyText != "jmap body" {
		t.Errorf("stable row body = %q, want the JMAP object's", gotStable.BodyText)
	}
	gotFallback, err := db.GetByDBID(fallback.ID)
	if err != nil || gotFallback == nil {
		t.Fatalf("get fallback row: %+v err=%v", gotFallback, err)
	}
	if gotFallback.BodyText != "imap body" {
		t.Errorf("fallback row body = %q, want the IMAP row's", gotFallback.BodyText)
	}
}

// TestUpsertReportsSecondStableObjectAsCreated covers the interaction between
// stable identities and the created/before-image detection. The detection used
// to key on (Message-ID, account), which matches the FIRST object when a
// provider holds several sharing one Message-ID: the second would be reported
// as already stored, so its initial tags would never be seeded and its arrival
// would not be announced, while the before-image captured belonged to the other
// row entirely.
func TestUpsertReportsSecondStableObjectAsCreated(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	first := &Message{
		StableID: "obj-1", MessageID: "shared@example.com", Subject: "First",
		Date: now, CreatedAt: now, Mailbox: "ALL", Account: "work", RemoteRef: "obj-1",
		SyncedFlags: `\Seen`, SyncedFlagsInitialized: true,
	}
	created, err := db.UpsertMessageWithInitialTags(first, []string{"inbox"})
	if err != nil {
		t.Fatalf("upsert first: %v", err)
	}
	if !created {
		t.Fatal("first object: created = false, want true")
	}

	second := &Message{
		StableID: "obj-2", MessageID: "shared@example.com", Subject: "Second",
		Date: now + 1, CreatedAt: now + 1, Mailbox: "ALL", Account: "work", RemoteRef: "obj-2",
		SyncedFlags: "", SyncedFlagsInitialized: true,
	}
	created, err = db.UpsertMessageWithInitialTags(second, []string{"inbox", "unread"})
	if err != nil {
		t.Fatalf("upsert second: %v", err)
	}
	if !created {
		t.Fatal("second object: created = false — the Message-ID it shares with the first is not its identity")
	}
	if first.ID == second.ID {
		t.Fatalf("both objects landed on row %d", first.ID)
	}

	// The seeding is the visible consequence: a row reported as existing skips
	// it, and nothing later puts those tags back.
	tags, err := db.GetMessageTags(second.ID)
	if err != nil {
		t.Fatalf("get tags: %v", err)
	}
	if !slices.Contains(tags, "unread") {
		t.Errorf("second object tags = %v, want the initial tags seeded", tags)
	}
}

// TestSetSyncedFlagsByDBIDKeepsEmptyBaselineInitialized guards the row-addressed
// setter against the same ambiguity SetSyncedFlags encodes around: an
// initialized-but-empty baseline stored raw reads back as "never initialized",
// which sends the reconciler down the legacy seeding path on a row whose
// emptiness is the truth.
func TestSetSyncedFlagsByDBIDKeepsEmptyBaselineInitialized(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	msg := &Message{
		StableID: "obj-empty", MessageID: "empty-baseline@example.com", Subject: "Empty",
		Date: now, CreatedAt: now, Mailbox: "ALL", Account: "work", RemoteRef: "obj-empty",
		SyncedFlags: `\Seen`, SyncedFlagsInitialized: true,
	}
	if _, err := db.UpsertMessage(msg); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// The server clears every flag: a real, initialized, empty baseline.
	if err := db.SetSyncedFlagsByDBID(msg.ID, ""); err != nil {
		t.Fatalf("set synced flags: %v", err)
	}

	rows, err := db.GetFolderFlagState("work", "ALL")
	if err != nil {
		t.Fatalf("flag state: %v", err)
	}
	var found bool
	for _, row := range rows {
		if row.RowID != msg.ID {
			continue
		}
		found = true
		if row.SyncedFlags != "" {
			t.Errorf("baseline = %q, want empty", row.SyncedFlags)
		}
		if !row.SyncedFlagsInitialized {
			t.Error("baseline reads as uninitialized after an explicit empty write")
		}
	}
	if !found {
		t.Fatalf("row %d missing from folder flag state", msg.ID)
	}
}
