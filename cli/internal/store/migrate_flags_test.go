package store

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestMigrateV25RepairsLegacyCommaFlags(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v25-flags.db")
	kr := testKeyring(t)
	db, err := Open(dbPath, kr)
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	if err := db.Init(); err != nil {
		t.Fatalf("init seed: %v", err)
	}
	for _, msg := range []*Message{
		{MessageID: "affected@example.com", Date: 1, CreatedAt: 1, Mailbox: "ALL", Account: "work", RemoteRef: "r1"},
		{MessageID: "keyword@example.com", Date: 2, CreatedAt: 2, Mailbox: "ALL", Account: "work", RemoteRef: "r2"},
		{MessageID: "bare-seen@example.com", Date: 3, CreatedAt: 3, Mailbox: "ALL", Account: "work", RemoteRef: "r3"},
	} {
		if err := db.InsertMessage(msg); err != nil {
			t.Fatalf("insert seed %s: %v", msg.MessageID, err)
		}
	}
	legacyCT, err := kr.EncryptMeta([]byte(`\Seen,\Flagged,\Deleted,\Answered,$Forwarded`))
	if err != nil {
		t.Fatalf("encrypt legacy flags: %v", err)
	}
	keywordCT, err := kr.EncryptMeta([]byte(`$Label,X`))
	if err != nil {
		t.Fatalf("encrypt keyword: %v", err)
	}
	bareSeenCT, err := kr.EncryptMeta([]byte(`Seen`))
	if err != nil {
		t.Fatalf("encrypt bare Seen: %v", err)
	}
	if _, err := db.db.Exec(`UPDATE messages SET
		is_seen = 0, is_flagged = 0, is_deleted = 0,
		flags_other = CASE message_id
			WHEN 'affected@example.com' THEN ?
			WHEN 'bare-seen@example.com' THEN ?
			ELSE ? END`, legacyCT, bareSeenCT, keywordCT); err != nil {
		t.Fatalf("write v25 on-disk rows: %v", err)
	}
	if _, err := db.db.Exec("UPDATE schema_version SET version = 25 WHERE rowid = 1"); err != nil {
		t.Fatalf("set v25: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	db, err = Open(dbPath, kr)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Init(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var version, seen, flagged, deleted int
	var repairedCT, preservedCT []byte
	if err := db.db.QueryRow(`SELECT is_seen, is_flagged, is_deleted, flags_other
		FROM messages WHERE message_id = 'affected@example.com'`).Scan(&seen, &flagged, &deleted, &repairedCT); err != nil {
		t.Fatalf("read repaired row: %v", err)
	}
	if seen != 1 || flagged != 1 || deleted != 1 {
		t.Errorf("booleans = (%d,%d,%d), want (1,1,1)", seen, flagged, deleted)
	}
	other, err := kr.DecryptMeta(repairedCT)
	if err != nil {
		t.Fatalf("decrypt repaired flags_other: %v", err)
	}
	if got, want := string(other), `\Answered $Forwarded`; got != want {
		t.Errorf("flags_other = %q, want %q", got, want)
	}
	msg, err := db.GetByMessageID("affected@example.com")
	if err != nil {
		t.Fatalf("get repaired message: %v", err)
	}
	if got, want := msg.Flags, `\Seen \Flagged \Deleted \Answered $Forwarded`; got != want {
		t.Errorf("reconstructed flags = %q, want %q", got, want)
	}
	rows, err := db.GetFolderFlagState("work", "ALL")
	if err != nil {
		t.Fatalf("get flag state: %v", err)
	}
	if len(rows) != 3 || !rows[0].IsSeen || !rows[0].IsFlagged || rows[0].SyncedFlags != "" {
		t.Fatalf("flag baseline input = %+v, want migrated booleans with empty baseline", rows)
	}
	if !rows[2].IsSeen {
		t.Fatalf("bare Seen row = %+v, want migrated is_seen", rows[2])
	}
	if err := db.db.QueryRow(`SELECT flags_other FROM messages WHERE message_id = 'keyword@example.com'`).Scan(&preservedCT); err != nil {
		t.Fatalf("read keyword row: %v", err)
	}
	if !bytes.Equal(preservedCT, keywordCT) {
		t.Error("unrelated comma-bearing keyword ciphertext changed")
	}
	if err := db.db.QueryRow("SELECT version FROM schema_version WHERE rowid = 1").Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != 28 {
		t.Errorf("version = %d, want 28", version)
	}

	// A second Init must not re-encrypt or otherwise change the repaired row.
	if err := db.Init(); err != nil {
		t.Fatalf("second init: %v", err)
	}
	var secondCT []byte
	if err := db.db.QueryRow(`SELECT flags_other FROM messages WHERE message_id = 'affected@example.com'`).Scan(&secondCT); err != nil {
		t.Fatalf("read row after second init: %v", err)
	}
	if !bytes.Equal(secondCT, repairedCT) {
		t.Error("second Init changed migrated ciphertext")
	}

	// Init completes v26 before any sync ingest can run. The first subsequent
	// upsert must therefore capture the repaired before-image, independent of
	// the old comma representation that existed on disk.
	created, err := db.UpsertMessage(&Message{
		MessageID: "affected@example.com", Date: 1, CreatedAt: 1,
		Mailbox: "ALL", Account: "work", RemoteRef: "r1",
		SyncedFlagsInitialized: true,
	})
	if err != nil || created {
		t.Fatalf("post-migration upsert created=%v err=%v", created, err)
	}
	rows, err = db.GetFolderFlagState("work", "ALL")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rows[0].SyncedFlags, `\Seen,\Flagged,\Answered,\Deleted`; got != want || !rows[0].SyncedFlagsInitialized {
		t.Fatalf("post-v26 captured baseline=%q initialized=%v, want %q", got, rows[0].SyncedFlagsInitialized, want)
	}
}

func TestMigrateV25RollsBackOnDecryptFailure(t *testing.T) {
	db := newTestDB(t)
	kr := testKeyring(t)
	for _, id := range []string{"valid@example.com", "invalid@example.com"} {
		if err := db.InsertMessage(&Message{MessageID: id, Date: 1, CreatedAt: 1}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	validCT, err := kr.EncryptMeta([]byte(`\Seen,\Answered`))
	if err != nil {
		t.Fatalf("encrypt valid row: %v", err)
	}
	if _, err := db.db.Exec(`UPDATE messages SET flags_other = CASE message_id
		WHEN 'valid@example.com' THEN ? ELSE X'010203' END`, validCT); err != nil {
		t.Fatalf("write v25 rows: %v", err)
	}
	if _, err := db.db.Exec("UPDATE schema_version SET version = 25 WHERE rowid = 1"); err != nil {
		t.Fatalf("set v25: %v", err)
	}

	if err := db.Init(); err == nil {
		t.Fatal("migration with invalid ciphertext succeeded")
	}
	var version, seen int
	var afterCT []byte
	if err := db.db.QueryRow("SELECT version FROM schema_version WHERE rowid = 1").Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != 25 {
		t.Errorf("version = %d, want rollback to 25", version)
	}
	if err := db.db.QueryRow(`SELECT is_seen, flags_other FROM messages
		WHERE message_id = 'valid@example.com'`).Scan(&seen, &afterCT); err != nil {
		t.Fatalf("read valid row: %v", err)
	}
	if seen != 0 || !bytes.Equal(afterCT, validCT) {
		t.Errorf("valid row was partially migrated: seen=%d ciphertext_changed=%v", seen, !bytes.Equal(afterCT, validCT))
	}

	replacementCT, err := kr.EncryptMeta([]byte(`$Unrelated`))
	if err != nil {
		t.Fatalf("encrypt replacement: %v", err)
	}
	if _, err := db.db.Exec(`UPDATE messages SET flags_other = ? WHERE message_id = 'invalid@example.com'`, replacementCT); err != nil {
		t.Fatalf("replace invalid ciphertext: %v", err)
	}
	if err := db.Init(); err != nil {
		t.Fatalf("retry migration: %v", err)
	}
	if err := db.db.QueryRow("SELECT version FROM schema_version WHERE rowid = 1").Scan(&version); err != nil {
		t.Fatalf("read retry version: %v", err)
	}
	if version != 28 {
		t.Errorf("retry version = %d, want 28", version)
	}
}
