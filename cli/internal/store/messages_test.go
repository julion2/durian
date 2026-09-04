package store

import (
	"fmt"
	"path/filepath"
	"slices"
	"sync"
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

func TestStableIdentityKeepsDuplicateMessageIDsDistinct(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()
	first := &Message{
		StableID: "email-1", MessageID: "duplicate@example.com", Subject: "First",
		Date: now, CreatedAt: now, Mailbox: "ALL", Account: "work", RemoteRef: "email-1",
	}
	second := &Message{
		StableID: "email-2", MessageID: "duplicate@example.com", Subject: "Second",
		Date: now + 1, CreatedAt: now + 1, Mailbox: "ALL", Account: "work", RemoteRef: "email-2",
	}
	if err := db.InsertMessage(first); err != nil {
		t.Fatalf("insert first: %v", err)
	}
	if err := db.InsertMessage(second); err != nil {
		t.Fatalf("insert second: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("duplicate Message-IDs collapsed to row %d", first.ID)
	}
	if _, err := db.GetByMessageID(first.MessageID); err == nil {
		t.Fatal("ambiguous stable Message-ID selected an arbitrary row")
	}
	thread, err := db.GetByThread(first.ThreadID)
	if err != nil || len(thread) != 2 {
		t.Fatalf("thread rows = %d, err=%v; want 2", len(thread), err)
	}
	if err := db.SetSyncedFlagsByDBID(first.ID, `\Seen`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSyncedLabelsByDBID(second.ID, "project"); err != nil {
		t.Fatal(err)
	}
	flagRows, err := db.GetFolderFlagState("work", "ALL")
	if err != nil || len(flagRows) != 2 {
		t.Fatalf("flag rows = %+v, err=%v", flagRows, err)
	}
	byRef := make(map[string]FolderFlagRow, len(flagRows))
	for _, row := range flagRows {
		byRef[row.RemoteRef] = row
	}
	if byRef["email-1"].SyncedFlags != `\Seen` || byRef["email-2"].SyncedFlags != "" {
		t.Fatalf("row-specific flags = %+v", byRef)
	}
	labelRows, err := db.GetLabelState("work")
	if err != nil || len(labelRows) != 2 {
		t.Fatalf("label rows = %+v, err=%v", labelRows, err)
	}
	for _, row := range labelRows {
		if row.RemoteRef == "email-2" && row.SyncedLabels != "project" {
			t.Errorf("second label baseline = %q", row.SyncedLabels)
		}
	}
	if err := db.DeleteByDBID(first.ID); err != nil {
		t.Fatal(err)
	}
	remaining, err := db.GetByRemoteRef("work", "ALL", "email-2")
	if err != nil || remaining == nil || remaining.ID != second.ID {
		t.Fatalf("second row after deleting first = %+v, err=%v", remaining, err)
	}
}

func TestGetByRemoteRefScopesIMAPUIDToMailbox(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()
	inbox := &Message{
		MessageID: "inbox@example.com", Date: now, CreatedAt: now,
		Mailbox: "INBOX", Account: "work", RemoteRef: "1",
	}
	archive := &Message{
		MessageID: "archive@example.com", Date: now, CreatedAt: now,
		Mailbox: "Archive", Account: "work", RemoteRef: "1",
	}
	if err := db.InsertMessage(inbox); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertMessage(archive); err != nil {
		t.Fatal(err)
	}
	gotInbox, err := db.GetByRemoteRef("work", "INBOX", "1")
	if err != nil || gotInbox == nil || gotInbox.ID != inbox.ID {
		t.Fatalf("inbox ref = %+v, err=%v", gotInbox, err)
	}
	gotLowercaseInbox, err := db.GetByRemoteRef("work", "inbox", "1")
	if err != nil || gotLowercaseInbox == nil || gotLowercaseInbox.ID != inbox.ID {
		t.Fatalf("lowercase inbox ref = %+v, err=%v", gotLowercaseInbox, err)
	}
	gotArchive, err := db.GetByRemoteRef("work", "Archive", "1")
	if err != nil || gotArchive == nil || gotArchive.ID != archive.ID {
		t.Fatalf("archive ref = %+v, err=%v", gotArchive, err)
	}
	gotEmpty, err := db.GetByRemoteRef("work", "INBOX", "")
	if err != nil || gotEmpty != nil {
		t.Fatalf("empty ref = %+v, err=%v; want nil", gotEmpty, err)
	}
}

func TestStableIdentityClaimsMatchingLegacyRow(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()
	legacy := &Message{
		MessageID: "legacy@example.com", Subject: "Legacy", Date: now, CreatedAt: now,
		Mailbox: "ALL", Account: "work", RemoteRef: "email-1",
	}
	if err := db.InsertMessage(legacy); err != nil {
		t.Fatal(err)
	}
	exists, err := db.MessageIdentityExistsForAccount("email-1", legacy.MessageID, legacy.RemoteRef, legacy.Account)
	if err != nil || !exists {
		t.Fatalf("stable identity did not recognize claimable legacy row: exists=%v err=%v", exists, err)
	}
	stable := &Message{
		StableID: "email-1", MessageID: legacy.MessageID, Subject: "Hydrated", Date: now,
		CreatedAt: now, Mailbox: "ALL", Account: "work", RemoteRef: "email-1",
	}
	if err := db.InsertMessage(stable); err != nil {
		t.Fatal(err)
	}
	if stable.ID != legacy.ID {
		t.Fatalf("stable identity created row %d instead of claiming legacy row %d", stable.ID, legacy.ID)
	}
	claimed, err := db.GetByDBID(legacy.ID)
	if err != nil || claimed == nil || claimed.StableID != "email-1" {
		t.Fatalf("claimed row = %+v, err=%v", claimed, err)
	}
}

func TestSyntheticIdentityProvenanceRoundTrip(t *testing.T) {
	db := newTestDB(t)
	generated := &Message{
		MessageID: "durian-synthetic-1-INBOX@work", Subject: "generated",
		Date: 1, CreatedAt: 1, Mailbox: "INBOX", Account: "work",
		SyntheticIdentity: true,
	}
	if err := db.InsertMessage(generated); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetByMessageID(generated.MessageID)
	if err != nil || stored == nil || !stored.SyntheticIdentity {
		t.Fatalf("generated row = %+v, err=%v", stored, err)
	}

	// A metadata update that lacks provenance must not erase established proof.
	if err := db.InsertMessage(&Message{
		MessageID: generated.MessageID, Subject: "update", Date: 1, CreatedAt: 2,
		Mailbox: "INBOX", Account: "work",
	}); err != nil {
		t.Fatal(err)
	}
	stored, err = db.GetByMessageID(generated.MessageID)
	if err != nil || stored == nil || !stored.SyntheticIdentity {
		t.Fatalf("updated generated row = %+v, err=%v", stored, err)
	}

	// Exact generated grammar is not provenance for a real sender-supplied ID.
	real := &Message{
		MessageID: "durian-synthetic-2-INBOX@work", Subject: "real",
		Date: 1, CreatedAt: 1, Mailbox: "INBOX", Account: "work",
	}
	if err := db.InsertMessage(real); err != nil {
		t.Fatal(err)
	}
	stored, err = db.GetByMessageID(real.MessageID)
	if err != nil || stored == nil || stored.SyntheticIdentity {
		t.Fatalf("real row = %+v, err=%v", stored, err)
	}
}

func TestIngestPendingRequiresExplicitCompletion(t *testing.T) {
	db := newTestDB(t)
	const messageID = "pending@example.com"
	fingerprint := []byte("complete parsed-content fingerprint")
	if err := db.InsertMessage(&Message{
		MessageID: messageID, Subject: "pending", Date: 1, CreatedAt: 1,
		Mailbox: "INBOX", Account: "work", SyntheticFingerprint: fingerprint, IngestPending: true,
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetByMessageID(messageID)
	if err != nil || stored == nil || !stored.IngestPending || string(stored.SyntheticFingerprint) != string(fingerprint) {
		t.Fatalf("new pending row = %+v, err=%v", stored, err)
	}

	// A normal upsert cannot accidentally clear an incomplete ingest.
	retry := &Message{
		MessageID: messageID, Subject: "retry", Date: 1, CreatedAt: 2,
		Mailbox: "INBOX", Account: "work",
	}
	if err := db.InsertMessage(retry); err != nil {
		t.Fatal(err)
	}
	stored, err = db.GetByMessageID(messageID)
	if err != nil || stored == nil || !stored.IngestPending || !retry.IngestPending {
		t.Fatalf("pending row after retry upsert = %+v, effective pending=%t, err=%v", stored, retry.IngestPending, err)
	}
	if stored.IngestGeneration != 1 {
		t.Fatalf("initial ingest generation = %d, want 1", stored.IngestGeneration)
	}
	if err := db.MarkMessageIngestComplete(stored.ID, stored.IngestGeneration); err != nil {
		t.Fatal(err)
	}

	// Conversely, re-delivery cannot turn a completed row pending again.
	redelivery := &Message{
		MessageID: messageID, Subject: "redelivery", Date: 1, CreatedAt: 3,
		Mailbox: "INBOX", Account: "work", IngestPending: true,
	}
	if err := db.InsertMessage(redelivery); err != nil {
		t.Fatal(err)
	}
	stored, err = db.GetByMessageID(messageID)
	if err != nil || stored == nil || stored.IngestPending || redelivery.IngestPending {
		t.Fatalf("completed row after redelivery = %+v, effective pending=%t, err=%v", stored, redelivery.IngestPending, err)
	}

	refresh := &Message{
		MessageID: messageID, Subject: "refresh", Date: 1, CreatedAt: 4,
		Mailbox: "INBOX", Account: "work", IngestPending: true, StartIngestOnConflict: true,
	}
	if err := db.InsertMessage(refresh); err != nil {
		t.Fatal(err)
	}
	stored, err = db.GetByMessageID(messageID)
	if err != nil || stored == nil || !stored.IngestPending || !refresh.IngestPending || refresh.IngestGeneration != 2 {
		t.Fatalf("explicit refresh did not mark row pending: stored=%+v effective=%t err=%v", stored, refresh.IngestPending, err)
	}

	// A backend that normally fast-paths completed rows still takes ownership
	// when retrying a row that was already pending.
	pendingRetry := &Message{
		MessageID: messageID, Subject: "pending retry", Date: 1, CreatedAt: 5,
		Mailbox: "INBOX", Account: "work", IngestPending: true,
	}
	if err := db.InsertMessage(pendingRetry); err != nil {
		t.Fatal(err)
	}
	if pendingRetry.IngestGeneration != 3 {
		t.Fatalf("pending retry generation = %d, want 3", pendingRetry.IngestGeneration)
	}
	if err := db.MarkMessageIngestComplete(stored.ID, refresh.IngestGeneration); err != nil {
		t.Fatal(err)
	}
	stored, err = db.GetByMessageID(messageID)
	if err != nil || stored == nil || !stored.IngestPending || stored.IngestGeneration != pendingRetry.IngestGeneration {
		t.Fatalf("stale refresh completion changed pending retry: stored=%+v err=%v", stored, err)
	}
}

func TestStableIdentityDoesNotClaimFallbackRefreshInProgress(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "promotion-refresh.db")
	kr := testKeyring(t)
	first, err := Open(dbPath, kr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	if err := first.Init(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(dbPath, kr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })

	fallback := &Message{
		MessageID: "refresh-duplicate@example.com", Subject: "Same content",
		FromAddr: "sender@example.com", ToAddrs: "recipient@example.com",
		BodyText: "same body", Date: 10, CreatedAt: 10,
		Mailbox: "INBOX", Account: "work", FetchedBody: true,
	}
	if err := first.InsertMessage(fallback); err != nil {
		t.Fatal(err)
	}
	if err := first.InsertAttachment(&Attachment{
		MessageDBID: fallback.ID, PartID: 1, Filename: "same.bin",
		ContentType: "application/octet-stream", Size: 1, Disposition: "attachment",
	}); err != nil {
		t.Fatal(err)
	}

	refreshA := *fallback
	refreshA.ID = 0
	refreshA.IngestPending = true
	refreshA.StartIngestOnConflict = true
	if err := first.InsertMessage(&refreshA); err != nil {
		t.Fatal(err)
	}
	refreshB := *fallback
	refreshB.ID = 0
	refreshB.IngestPending = true
	refreshB.StartIngestOnConflict = true
	if err := second.InsertMessage(&refreshB); err != nil {
		t.Fatal(err)
	}
	if refreshB.IngestGeneration <= refreshA.IngestGeneration {
		t.Fatalf("second refresh generation = %d, want newer than %d", refreshB.IngestGeneration, refreshA.IngestGeneration)
	}

	// Writer A finishes after writer B has taken ownership. Its stale completion
	// must not expose B's in-progress enrichment as complete.
	if err := first.MarkMessageIngestComplete(fallback.ID, refreshA.IngestGeneration); err != nil {
		t.Fatal(err)
	}
	if pending, err := second.GetByDBID(fallback.ID); err != nil || pending == nil ||
		!pending.IngestPending || pending.IngestGeneration != refreshB.IngestGeneration {
		t.Fatalf("fallback refresh state = %+v, err=%v", pending, err)
	}

	stable := *fallback
	stable.ID = 0
	stable.StableID = "provider-object"
	stable.RemoteRef = "provider-object"
	if err := second.InsertMessage(&stable); err != nil {
		t.Fatal(err)
	}
	if stable.ID == fallback.ID {
		t.Fatalf("stable object claimed fallback row %d while its enrichment was pending", fallback.ID)
	}
	pending, err := first.GetByDBID(fallback.ID)
	if err != nil || pending == nil || pending.StableID != "" || !pending.IngestPending {
		t.Fatalf("fallback after competing stable ingest = %+v, err=%v", pending, err)
	}
	if err := second.MarkMessageIngestComplete(fallback.ID, refreshB.IngestGeneration); err != nil {
		t.Fatal(err)
	}
	pending, err = first.GetByDBID(fallback.ID)
	if err != nil || pending == nil || pending.IngestPending || pending.IngestGeneration != refreshB.IngestGeneration {
		t.Fatalf("fallback after owning writer completed = %+v, err=%v", pending, err)
	}
}

func TestInsertMessageParsesCommaAndWhitespaceSeparatedFlags(t *testing.T) {
	tests := []struct {
		flags string
		want  string
	}{
		{flags: `\Seen,\Flagged,\Answered`, want: `\Seen \Flagged \Answered`},
		{flags: `\Seen \Flagged \Answered`, want: `\Seen \Flagged \Answered`},
		{flags: `$Label,X`, want: `$Label,X`},
		{flags: `\Answered,\Draft`, want: `\Answered,\Draft`},
		// Once a standard flag identifies a comma-joined legacy list, commas
		// inside keywords are irreducibly ambiguous and split as components.
		{flags: `\Seen,$Label,X`, want: `\Seen $Label X`},
	}
	for _, tc := range tests {
		t.Run(tc.flags, func(t *testing.T) {
			db := newTestDB(t)
			msg := &Message{
				MessageID: "flags@example.com",
				Subject:   "Flags",
				Date:      time.Now().Unix(),
				Mailbox:   "INBOX",
				Account:   "work",
				Flags:     tc.flags,
			}
			if err := db.InsertMessage(msg); err != nil {
				t.Fatalf("insert: %v", err)
			}
			got, err := db.GetByMessageID("flags@example.com")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.Flags != tc.want {
				t.Errorf("flags = %q, want %q", got.Flags, tc.want)
			}
		})
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

func TestUpsertMessageCapturesLegacyFlagBeforeImage(t *testing.T) {
	db := newTestDB(t)
	legacy := &Message{
		MessageID: "legacy-flags@example.com", Subject: "legacy", Date: 1, CreatedAt: 1,
		Mailbox: "ALL", Account: "work", RemoteRef: "r1", FetchedBody: true,
		Flags: `\Seen,\Flagged,\Answered,\Deleted,$Completed,$Other`,
	}
	if err := db.InsertMessage(legacy); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	created, err := db.UpsertMessage(&Message{
		MessageID: legacy.MessageID, Subject: "delta", Date: 1, CreatedAt: 1,
		Mailbox: "ALL", Account: "work", RemoteRef: "r1", FetchedBody: true,
		SyncedFlagsInitialized: true,
	})
	if err != nil || created {
		t.Fatalf("delta upsert created=%v err=%v, want existing row", created, err)
	}
	rows, err := db.GetFolderFlagState("work", "ALL")
	if err != nil || len(rows) != 1 {
		t.Fatalf("flag rows=%+v err=%v", rows, err)
	}
	if got, want := rows[0].SyncedFlags, `\Seen,\Flagged,\Answered,\Deleted,$Completed`; got != want {
		t.Fatalf("captured baseline=%q, want %q", got, want)
	}
	if !rows[0].SyncedFlagsInitialized {
		t.Fatal("captured baseline remains uninitialized")
	}
}

func TestUpsertMessageCapturesCommaSeparatedOtherFlags(t *testing.T) {
	db := newTestDB(t)
	msg := &Message{
		MessageID: "legacy-other-flags@example.com", Subject: "legacy", Date: 1, CreatedAt: 1,
		Mailbox: "ALL", Account: "work", RemoteRef: "r1", FetchedBody: true,
	}
	if err := db.InsertMessage(msg); err != nil {
		t.Fatal(err)
	}
	legacyCT, err := db.encryptMeta(`\Answered,$Completed,keyword,with,commas`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec("UPDATE messages SET flags_other = ? WHERE id = ?", legacyCT, msg.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertMessage(&Message{
		MessageID: msg.MessageID, Subject: "delta", Date: 1, CreatedAt: 1,
		Mailbox: "ALL", Account: "work", RemoteRef: "r1",
		SyncedFlagsInitialized: true,
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.GetFolderFlagState("work", "ALL")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if got, want := rows[0].SyncedFlags, `\Answered,$Completed`; got != want {
		t.Fatalf("captured baseline=%q, want %q", got, want)
	}
}

func TestExplicitEmptySyncedFlagsUsesStoreSentinel(t *testing.T) {
	db := newTestDB(t)
	msg := &Message{
		MessageID: "empty-baseline@example.com", Subject: "empty", Date: 1, CreatedAt: 1,
		Mailbox: "ALL", Account: "work", RemoteRef: "r1",
		SyncedFlagsInitialized: true,
	}
	created, err := db.UpsertMessage(msg)
	if err != nil || !created {
		t.Fatalf("upsert created=%v err=%v", created, err)
	}
	var stored string
	if err := db.db.QueryRow("SELECT synced_flags FROM messages WHERE id = ?", msg.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != syncedFlagsEmpty {
		t.Fatalf("stored baseline=%q, want sentinel %q", stored, syncedFlagsEmpty)
	}
	rows, err := db.GetFolderFlagState("work", "ALL")
	if err != nil || len(rows) != 1 || rows[0].SyncedFlags != "" || !rows[0].SyncedFlagsInitialized {
		t.Fatalf("decoded flag rows=%+v err=%v", rows, err)
	}
	if err := db.SetSyncedFlags(msg.MessageID, msg.Account, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow("SELECT synced_flags FROM messages WHERE id = ?", msg.ID).Scan(&stored); err != nil || stored != syncedFlagsEmpty {
		t.Fatalf("SetSyncedFlags empty stored=%q err=%v", stored, err)
	}
}

func TestInitializedUpsertDoesNotDecryptStoredFlags(t *testing.T) {
	db := newTestDB(t)
	msg := &Message{
		MessageID: "hot-path@example.com", Subject: "full", Date: 1, CreatedAt: 1,
		Mailbox: "ALL", Account: "work", RemoteRef: "r1", FetchedBody: true,
		SyncedFlagsInitialized: true,
	}
	if err := db.InsertMessage(msg); err != nil {
		t.Fatal(err)
	}
	// Invalid ciphertext makes any attempted before-image decrypt fail. An
	// initialized row must bypass that work and replace the provider metadata.
	if _, err := db.db.Exec("UPDATE messages SET flags_other = X'010203' WHERE id = ?", msg.ID); err != nil {
		t.Fatal(err)
	}
	created, err := db.UpsertMessage(&Message{
		MessageID: msg.MessageID, Subject: "metadata", Date: 1, CreatedAt: 1,
		Mailbox: "ALL", Account: "work", RemoteRef: "r1",
		SyncedFlagsInitialized: true,
	})
	if err != nil || created {
		t.Fatalf("initialized metadata upsert created=%v err=%v", created, err)
	}
}

func TestCorruptLegacyFlagsDoNotBlockBatchIngest(t *testing.T) {
	db := newTestDB(t)
	corrupt := &Message{
		MessageID: "corrupt@example.com", Subject: "before", Date: 1, CreatedAt: 1,
		Mailbox: "ALL", Account: "work", RemoteRef: "r1", Flags: `\Seen`, FetchedBody: true,
	}
	if err := db.InsertMessage(corrupt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec("UPDATE messages SET flags_other = X'010203' WHERE id = ?", corrupt.ID); err != nil {
		t.Fatal(err)
	}
	healthy := &Message{
		MessageID: "healthy@example.com", Subject: "healthy", Date: 2, CreatedAt: 2,
		Mailbox: "ALL", Account: "work", RemoteRef: "r2", FetchedBody: true,
		SyncedFlagsInitialized: true,
	}
	corruptUpdate := &Message{
		MessageID: corrupt.MessageID, Subject: "after", Date: 1, CreatedAt: 1,
		Mailbox: "ALL", Account: "work", RemoteRef: "r1", FetchedBody: true,
		SyncedFlagsInitialized: true,
	}
	if err := db.InsertBatch([]*Message{corruptUpdate, healthy}); err != nil {
		t.Fatalf("batch ingest: %v", err)
	}
	if got, err := db.GetByMessageID(corrupt.MessageID); err != nil || got.Subject != "after" {
		t.Fatalf("corrupt row after ingest=%+v err=%v", got, err)
	}
	if got, err := db.GetByMessageID(healthy.MessageID); err != nil || got == nil {
		t.Fatalf("healthy sibling=%+v err=%v", got, err)
	}
	rows, err := db.GetFolderFlagState("work", "ALL")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.MessageID == corrupt.MessageID {
			found = true
			if row.SyncedFlags != `\Seen` || !row.SyncedFlagsInitialized {
				t.Fatalf("degraded baseline=%q initialized=%v, want Seen", row.SyncedFlags, row.SyncedFlagsInitialized)
			}
		}
	}
	if !found {
		t.Fatal("corrupt row missing from flag state")
	}
}

func TestUpsertMessageWithInitialTagsRollsBackTogether(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.db.Exec(`CREATE TRIGGER fail_initial_tag BEFORE INSERT ON tags
		BEGIN SELECT RAISE(ABORT, 'tag failure'); END`); err != nil {
		t.Fatal(err)
	}
	msg := &Message{
		MessageID: "atomic-tags@example.com", Subject: "atomic", Date: 1, CreatedAt: 1,
		Mailbox: "ALL", Account: "work", RemoteRef: "r1",
		SyncedFlagsInitialized: true,
	}
	if _, err := db.UpsertMessageWithInitialTags(msg, []string{"unread"}); err == nil {
		t.Fatal("upsert succeeded despite tag failure")
	}
	if exists, err := db.MessageExistsForAccount(msg.MessageID, msg.Account); err != nil || exists {
		t.Fatalf("message survived failed initial tags: exists=%v err=%v", exists, err)
	}
	if _, err := db.db.Exec("DROP TRIGGER fail_initial_tag"); err != nil {
		t.Fatal(err)
	}
	created, err := db.UpsertMessageWithInitialTags(msg, []string{"unread"})
	if err != nil || !created {
		t.Fatalf("retry created=%v err=%v", created, err)
	}
	rows, err := db.GetFolderFlagState("work", "ALL")
	if err != nil || len(rows) != 1 || !slices.Contains(rows[0].Tags, "unread") {
		t.Fatalf("retry rows=%+v err=%v", rows, err)
	}
}

func TestConcurrentUpsertReportsSingleCreatorAndCapturesOnce(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "concurrent.db")
	kr := testKeyring(t)
	first, err := Open(dbPath, kr)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err := first.Init(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(dbPath, kr)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	dbs := []*DB{first, second}
	created := make([]bool, len(dbs))
	errs := make([]error, len(dbs))
	var wg sync.WaitGroup
	for round := 0; round < 40; round++ {
		messageID := fmt.Sprintf("concurrent-%d@example.com", round)
		for i, db := range dbs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				created[i], errs[i] = db.UpsertMessage(&Message{
					MessageID: messageID, Subject: "same", Date: 1, CreatedAt: 1,
					Mailbox: "ALL", Account: "work", RemoteRef: messageID,
					SyncedFlagsInitialized: true,
				})
			}()
		}
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("round %d concurrent errors=%v", round, errs)
		}
		if created[0] == created[1] {
			t.Fatalf("round %d created results=%v, want exactly one creator", round, created)
		}

		legacyID := fmt.Sprintf("legacy-concurrent-%d@example.com", round)
		if err := first.InsertMessage(&Message{
			MessageID: legacyID, Subject: "legacy", Date: 1, CreatedAt: 1,
			Mailbox: "ALL", Account: "work", RemoteRef: legacyID,
		}); err != nil {
			t.Fatal(err)
		}
		for i, db := range dbs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = db.UpsertMessage(&Message{
					MessageID: legacyID, Subject: "delta", Date: 1, CreatedAt: 1,
					Mailbox: "ALL", Account: "work", RemoteRef: legacyID,
					Flags: `\Seen,\Flagged`, SyncedFlagsInitialized: true,
				})
			}()
		}
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("round %d concurrent capture errors=%v", round, errs)
		}
		rows, err := first.GetFolderFlagState("work", "ALL")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, row := range rows {
			if row.MessageID == legacyID {
				found = true
				if row.SyncedFlags != "" || !row.SyncedFlagsInitialized {
					t.Fatalf("round %d captured baseline=%q initialized=%v, want explicit empty before-image", round, row.SyncedFlags, row.SyncedFlagsInitialized)
				}
				if !row.IsSeen || !row.IsFlagged {
					t.Fatalf("round %d final provider columns are half-updated: seen=%v flagged=%v", round, row.IsSeen, row.IsFlagged)
				}
			}
		}
		if !found {
			t.Fatalf("round %d legacy concurrent row missing", round)
		}
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

func TestUpsert_MetadataOnlyPreservesFullTextSearch(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()
	full := &Message{
		MessageID:              "metadata-search@x",
		Subject:                "Quarterly narwhal report",
		FromAddr:               "sender@example.com",
		ToAddrs:                "recipient@example.com",
		CCAddrs:                "copy@example.com",
		Date:                   now,
		CreatedAt:              now,
		BodyText:               "the aardvark body text",
		BodyHTML:               "<p>the aardvark body text</p>",
		Mailbox:                "INBOX",
		Account:                "work",
		RemoteRef:              "old-ref",
		FetchedBody:            true,
		SyncedFlagsInitialized: true,
	}
	if err := db.InsertMessage(full); err != nil {
		t.Fatalf("insert full message: %v", err)
	}
	for _, query := range []string{"subject:narwhal", "aardvark"} {
		if got, err := db.Search(query, 10); err != nil || len(got) != 1 {
			t.Fatalf("search %q before metadata update = %d results, %v; want 1", query, len(got), err)
		}
	}

	// Gmail/JMAP replacement snapshots can carry only identity, flags and a
	// provider reference for a message whose full content is already local.
	var changesBefore int64
	if err := db.db.QueryRow("SELECT total_changes()").Scan(&changesBefore); err != nil {
		t.Fatalf("read changes before metadata update: %v", err)
	}
	metadata := &Message{
		MessageID: "metadata-search@x",
		Mailbox:   "INBOX",
		Account:   "work",
		RemoteRef: "new-ref",
		Flags:     `\Seen`,
	}
	if err := db.InsertMessage(metadata); err != nil {
		t.Fatalf("insert metadata-only update: %v", err)
	}
	var changesAfter int64
	if err := db.db.QueryRow("SELECT total_changes()").Scan(&changesAfter); err != nil {
		t.Fatalf("read changes after metadata update: %v", err)
	}
	if got := changesAfter - changesBefore; got != 1 {
		t.Errorf("metadata-only update changed %d rows, want 1 messages row and no FTS rewrite", got)
	}

	for _, query := range []string{"subject:narwhal", "aardvark"} {
		if got, err := db.Search(query, 10); err != nil || len(got) != 1 {
			t.Errorf("search %q after metadata update = %d results, %v; want 1", query, len(got), err)
		}
	}
	stored, err := db.GetByMessageID(full.MessageID)
	if err != nil || stored == nil {
		t.Fatalf("stored message after metadata update = %+v, %v", stored, err)
	}
	if stored.Subject != full.Subject || stored.FromAddr != full.FromAddr ||
		stored.ToAddrs != full.ToAddrs || stored.CCAddrs != full.CCAddrs ||
		stored.BodyText != full.BodyText || stored.BodyHTML != full.BodyHTML || !stored.FetchedBody {
		t.Errorf("stored content after metadata update = %+v, want content from %+v", stored, full)
	}
	if stored.RemoteRef != metadata.RemoteRef || stored.Flags != metadata.Flags {
		t.Errorf("stored metadata = remote_ref %q flags %q, want %q and %q",
			stored.RemoteRef, stored.Flags, metadata.RemoteRef, metadata.Flags)
	}
}

func TestUpsert_FullReingestIndexesEffectiveStoredValues(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()
	if err := db.InsertMessage(&Message{
		MessageID: "full-reingest@x", Subject: "Old headline", FromAddr: "old@example.com",
		ToAddrs: "old-to@example.com", CCAddrs: "old-cc@example.com",
		Date: now, CreatedAt: now, BodyText: "stored platypus body", BodyHTML: "<p>stored platypus body</p>",
		Mailbox: "INBOX", Account: "work", FetchedBody: true,
	}); err != nil {
		t.Fatalf("insert original message: %v", err)
	}

	incoming := &Message{
		MessageID: "full-reingest@x", Subject: "Revised wombat headline", FromAddr: "new@example.com",
		ToAddrs: "new-to@example.com", CCAddrs: "new-cc@example.com",
		Date: now, CreatedAt: now, BodyText: "incoming echidna body", BodyHTML: "<p>incoming echidna body</p>",
		Mailbox: "INBOX", Account: "work", FetchedBody: true,
	}
	if err := db.InsertMessage(incoming); err != nil {
		t.Fatalf("full reingest: %v", err)
	}

	stored, err := db.GetByMessageID(incoming.MessageID)
	if err != nil || stored == nil {
		t.Fatalf("get reingested message = %+v, %v", stored, err)
	}
	if stored.Subject != incoming.Subject || stored.FromAddr != incoming.FromAddr ||
		stored.ToAddrs != incoming.ToAddrs || stored.CCAddrs != incoming.CCAddrs {
		t.Errorf("effective metadata = %+v, want incoming metadata %+v", stored, incoming)
	}
	if stored.BodyText != "stored platypus body" || stored.BodyHTML != "<p>stored platypus body</p>" {
		t.Errorf("stored body = %q / %q, want original body", stored.BodyText, stored.BodyHTML)
	}
	for _, query := range []string{"subject:wombat", "platypus"} {
		if got, err := db.Search(query, 10); err != nil || len(got) != 1 {
			t.Errorf("search %q after full reingest = %d results, %v; want 1", query, len(got), err)
		}
	}
}

func TestUpsert_MetadataOnlyFirstInsertBuildsFTS(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()
	metadata := &Message{
		MessageID: "metadata-first@x", Subject: "Capybara notice", FromAddr: "sender@example.com",
		ToAddrs: "recipient@example.com", CCAddrs: "copy@example.com",
		Date: now, CreatedAt: now, Mailbox: "INBOX", Account: "work",
	}
	if err := db.InsertMessage(metadata); err != nil {
		t.Fatalf("metadata-only first insert: %v", err)
	}
	stored, err := db.GetByMessageID(metadata.MessageID)
	if err != nil || stored == nil || stored.Subject != metadata.Subject || stored.CCAddrs != metadata.CCAddrs {
		t.Fatalf("stored metadata-only message = %+v, %v", stored, err)
	}
	if got, err := db.Search("subject:capybara", 10); err != nil || len(got) != 1 {
		t.Fatalf("search metadata-only first insert = %d results, %v; want 1", len(got), err)
	}
}

func TestUpsert_MetadataOnlyRepairsMissingFTSWithoutBreakingIntegrity(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()
	full := &Message{
		MessageID: "repair-fts@x", Subject: "Manatee report", FromAddr: "sender@example.com",
		ToAddrs: "recipient@example.com", CCAddrs: "copy@example.com",
		Date: now, CreatedAt: now, BodyText: "durable pangolin body",
		Mailbox: "INBOX", Account: "work", FetchedBody: true,
	}
	if err := db.InsertMessage(full); err != nil {
		t.Fatalf("insert full message: %v", err)
	}
	if _, err := db.db.Exec("DELETE FROM messages_blind_fts WHERE rowid = ?", full.ID); err != nil {
		t.Fatalf("remove FTS row: %v", err)
	}
	if err := db.InsertMessage(&Message{
		MessageID: full.MessageID, Mailbox: "INBOX", Account: "work", RemoteRef: "new-ref",
	}); err != nil {
		t.Fatalf("metadata-only repair: %v", err)
	}
	for _, query := range []string{"subject:manatee", "pangolin"} {
		if got, err := db.Search(query, 10); err != nil || len(got) != 1 {
			t.Errorf("search %q after FTS repair = %d results, %v; want 1", query, len(got), err)
		}
	}

	if _, err := db.db.Exec(`INSERT INTO messages_blind_fts(messages_blind_fts, rank)
		VALUES ('integrity-check', 1)`); err != nil {
		t.Errorf("blind FTS integrity-check: %v", err)
	}
	var messages, ftsRows, orphans, duplicateRowIDs int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&messages); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if err := db.db.QueryRow("SELECT COUNT(*) FROM messages_blind_fts").Scan(&ftsRows); err != nil {
		t.Fatalf("count FTS rows: %v", err)
	}
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM messages_blind_fts f
		LEFT JOIN messages m ON m.id = f.rowid WHERE m.id IS NULL`).Scan(&orphans); err != nil {
		t.Fatalf("count FTS orphans: %v", err)
	}
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM (
		SELECT rowid FROM messages_blind_fts GROUP BY rowid HAVING COUNT(*) > 1
	)`).Scan(&duplicateRowIDs); err != nil {
		t.Fatalf("count duplicate FTS rowids: %v", err)
	}
	if ftsRows != messages || orphans != 0 || duplicateRowIDs != 0 {
		t.Errorf("FTS shape: messages=%d rows=%d orphans=%d duplicate_rowids=%d",
			messages, ftsRows, orphans, duplicateRowIDs)
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

	// Two accounts hold this Message-ID and the lookup takes no account, so it
	// cannot name one of them. Returning either — as it used to, with LIMIT 1 —
	// hands the caller a row it did not ask for.
	if _, err := db.GetByMessageID("cross@x"); err == nil {
		t.Error("GetByMessageID picked one of two accounts instead of reporting ambiguity")
	}
	candidates, err := db.GetAllByMessageID("cross@x")
	if err != nil {
		t.Fatalf("GetAllByMessageID: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("GetAllByMessageID returned %d rows, want 2", len(candidates))
	}
}

func TestGetByThread_Dedup(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	// Same message in two accounts → same thread
	work := &Message{
		MessageID: "td-root@x", Subject: "Thread dedup",
		FromAddr: "a@x", Date: now, CreatedAt: now, FetchedBody: true,
		Account: "work",
	}
	if err := db.InsertMessage(work); err != nil {
		t.Fatalf("insert work: %v", err)
	}
	if err := db.InsertMessage(&Message{
		MessageID: "td-root@x", Subject: "Thread dedup",
		FromAddr: "a@x", Date: now, CreatedAt: now, FetchedBody: true,
		Account: "personal",
	}); err != nil {
		t.Fatalf("insert personal: %v", err)
	}

	// Read the thread id off the insert: a Message-ID shared by two accounts is
	// ambiguous by design, and this test is about the thread, not the lookup.
	msgs, err := db.GetByThread(work.ThreadID)
	if err != nil {
		t.Fatalf("get by thread: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("got %d messages, want 1 (dedup across accounts)", len(msgs))
	}
}

func TestGetByThreadDoesNotDedupStableObjectsAcrossAccounts(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()
	work := &Message{
		StableID: "work-email", MessageID: "stable-cross-account@x", Subject: "Stable objects",
		FromAddr: "a@x", Date: now, CreatedAt: now, FetchedBody: true, Account: "work",
	}
	personal := &Message{
		StableID: "personal-email", MessageID: work.MessageID, Subject: work.Subject,
		FromAddr: "a@x", Date: now, CreatedAt: now, FetchedBody: true, Account: "personal",
	}
	if err := db.InsertMessage(work); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertMessage(personal); err != nil {
		t.Fatal(err)
	}
	msgs, err := db.GetByThread(work.ThreadID)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("stable thread rows = %d, err=%v; want both provider objects", len(msgs), err)
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
