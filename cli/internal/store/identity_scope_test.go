package store

import (
	"slices"
	"testing"
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
