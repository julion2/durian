package store

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/durian-dev/durian/cli/internal/dbcrypto"
)

// testKeyring returns a deterministic keyring used across every store test.
// Pinned bytes so seeded ciphertexts decode the same across test runs.
func testKeyring(t *testing.T) *dbcrypto.Keyring {
	t.Helper()
	kr, err := dbcrypto.NewKeyring(bytes.Repeat([]byte{0x42}, dbcrypto.MasterKeyLen))
	if err != nil {
		t.Fatalf("test keyring: %v", err)
	}
	return kr
}

func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:", testKeyring(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenAndInit(t *testing.T) {
	db := newTestDB(t)

	// Verify schema_version exists and is current
	var version int
	err := db.db.QueryRow("SELECT version FROM schema_version WHERE rowid = 1").Scan(&version)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != 10 {
		t.Errorf("version = %d, want 10", version)
	}
}

func TestOpen_RejectsNilKeyring(t *testing.T) {
	if _, err := Open(":memory:", nil); err == nil {
		t.Error("Open(nil keyring) should error")
	}
}

func TestInitIdempotent(t *testing.T) {
	db := newTestDB(t)

	// Calling Init() again should not fail
	if err := db.Init(); err != nil {
		t.Fatalf("second init: %v", err)
	}
}

func TestDefaultDBPath(t *testing.T) {
	path := DefaultDBPath()
	if path == "" {
		t.Fatal("empty path")
	}
	if !contains(path, "email.db") {
		t.Errorf("path %q does not contain email.db", path)
	}
	if !contains(path, "durian/") {
		t.Errorf("path %q does not contain durian/", path)
	}
}

// TestMigrateV9_PopulatesMailboxesAndAccounts seeds a v8-shaped database
// with three messages spanning two accounts and three mailbox spellings
// (INBOX, inbox, Drafts), then runs Init() to trigger the v8→v9 migration
// and asserts that the lookup tables are populated correctly, that all
// messages get non-null FKs, and that the activity-boolean derivation from
// `flags` works for the `\Seen` case.
func TestMigrateV9_PopulatesMailboxesAndAccounts(t *testing.T) {
	// Seed a v8-shaped DB on disk, then re-open via store.Open which runs
	// migrate() up to v9. Shared-memory DSNs are brittle across separate
	// modernc handles, so we use a tempdir file.
	dbPath := filepath.Join(t.TempDir(), "step1.db")

	seedDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	seed := []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO schema_version (rowid, version) VALUES (1, 8)`,
		`CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id TEXT NOT NULL,
			thread_id TEXT NOT NULL,
			in_reply_to TEXT,
			refs TEXT,
			subject TEXT,
			from_addr TEXT,
			to_addrs TEXT,
			cc_addrs TEXT,
			date INTEGER,
			created_at INTEGER NOT NULL,
			body_text TEXT,
			body_html TEXT,
			mailbox TEXT,
			flags TEXT,
			uid INTEGER DEFAULT 0,
			size INTEGER DEFAULT 0,
			fetched_body INTEGER DEFAULT 0,
			account TEXT DEFAULT '',
			UNIQUE(message_id, account)
		)`,
		// Mirror the FTS5 virtual table + triggers that a real v8 DB would
		// have. Without these, Init() creates them on open and the v8→v9
		// UPDATE on messages fires `messages_au` which writes into an
		// unsynced FTS5 shadow → "database disk image is malformed".
		`CREATE VIRTUAL TABLE messages_fts USING fts5(
			subject, from_addr, to_addrs, body_text,
			content='messages',
			content_rowid='id'
		)`,
		`CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN
			INSERT INTO messages_fts(rowid, subject, from_addr, to_addrs, body_text)
			VALUES (new.id, new.subject, new.from_addr, new.to_addrs, new.body_text);
		END`,
		`CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN
			INSERT INTO messages_fts(messages_fts, rowid, subject, from_addr, to_addrs, body_text)
			VALUES ('delete', old.id, old.subject, old.from_addr, old.to_addrs, old.body_text);
		END`,
		`CREATE TRIGGER messages_au AFTER UPDATE ON messages BEGIN
			INSERT INTO messages_fts(messages_fts, rowid, subject, from_addr, to_addrs, body_text)
			VALUES ('delete', old.id, old.subject, old.from_addr, old.to_addrs, old.body_text);
			INSERT INTO messages_fts(rowid, subject, from_addr, to_addrs, body_text)
			VALUES (new.id, new.subject, new.from_addr, new.to_addrs, new.body_text);
		END`,
		`INSERT INTO messages (message_id, thread_id, created_at, mailbox, flags, account)
		 VALUES
		   ('m1', 't1', 1, 'INBOX',  '\Seen',    'alice@example.com'),
		   ('m2', 't2', 2, 'inbox',  '',         'alice@example.com'),
		   ('m3', 't3', 3, 'Drafts', '\Flagged', 'bob@example.com')`,
	}
	for _, stmt := range seed {
		if _, err := seedDB.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\nstmt: %s", err, stmt)
		}
	}
	seedDB.Close()

	// Re-open via the production code path; this triggers migrate() v8→v9 (and v10).
	sd, err := Open(dbPath, testKeyring(t))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { sd.Close() })
	if err := sd.Init(); err != nil {
		t.Fatalf("init/migrate: %v", err)
	}
	db := sd.db

	// Schema version must be 10 (v9 mailbox/account migration, v10 subject_ct).
	var version int
	if err := db.QueryRow("SELECT version FROM schema_version WHERE rowid = 1").Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != 10 {
		t.Fatalf("version = %d, want 10", version)
	}

	// mailboxes must contain exactly INBOX and Drafts (case-collapsed).
	rows, err := db.Query("SELECT name FROM mailboxes ORDER BY name")
	if err != nil {
		t.Fatalf("query mailboxes: %v", err)
	}
	var mboxes []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan mailbox: %v", err)
		}
		mboxes = append(mboxes, n)
	}
	rows.Close()
	if len(mboxes) != 2 {
		t.Fatalf("mailboxes count = %d (%v), want 2", len(mboxes), mboxes)
	}
	if mboxes[0] != "Drafts" || mboxes[1] != "INBOX" {
		t.Errorf("mailboxes = %v, want [Drafts INBOX]", mboxes)
	}

	// accounts must contain exactly the two distinct email addresses.
	rows, err = db.Query("SELECT name FROM accounts ORDER BY name")
	if err != nil {
		t.Fatalf("query accounts: %v", err)
	}
	var accs []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan account: %v", err)
		}
		accs = append(accs, n)
	}
	rows.Close()
	if len(accs) != 2 || accs[0] != "alice@example.com" || accs[1] != "bob@example.com" {
		t.Errorf("accounts = %v, want [alice@example.com bob@example.com]", accs)
	}

	// All messages must have non-null FKs. m1 and m2 (INBOX/inbox) must
	// resolve to the same mailbox_id.
	type row struct {
		msgID     string
		mailboxID sql.NullInt64
		accountID sql.NullInt64
		isSeen    int
		isFlagged int
		isDeleted int
	}
	msgRows, err := db.Query(`
		SELECT message_id, mailbox_id, account_id, is_seen, is_flagged, is_deleted
		FROM messages ORDER BY message_id`)
	if err != nil {
		t.Fatalf("query messages: %v", err)
	}
	defer msgRows.Close()
	var got []row
	for msgRows.Next() {
		var r row
		if err := msgRows.Scan(&r.msgID, &r.mailboxID, &r.accountID, &r.isSeen, &r.isFlagged, &r.isDeleted); err != nil {
			t.Fatalf("scan message: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != 3 {
		t.Fatalf("messages count = %d, want 3", len(got))
	}
	for _, r := range got {
		if !r.mailboxID.Valid {
			t.Errorf("%s: mailbox_id is null", r.msgID)
		}
		if !r.accountID.Valid {
			t.Errorf("%s: account_id is null", r.msgID)
		}
	}
	if got[0].mailboxID.Int64 != got[1].mailboxID.Int64 {
		t.Errorf("m1 (INBOX) and m2 (inbox) resolved to different mailbox_ids: %d vs %d",
			got[0].mailboxID.Int64, got[1].mailboxID.Int64)
	}

	// Activity-boolean derivation: m1 has \Seen, m3 has \Flagged, m2 has nothing.
	if got[0].isSeen != 1 || got[0].isFlagged != 0 || got[0].isDeleted != 0 {
		t.Errorf("m1 booleans = (seen=%d flagged=%d deleted=%d), want (1,0,0)",
			got[0].isSeen, got[0].isFlagged, got[0].isDeleted)
	}
	if got[1].isSeen != 0 || got[1].isFlagged != 0 || got[1].isDeleted != 0 {
		t.Errorf("m2 booleans = (seen=%d flagged=%d deleted=%d), want (0,0,0)",
			got[1].isSeen, got[1].isFlagged, got[1].isDeleted)
	}
	if got[2].isSeen != 0 || got[2].isFlagged != 1 || got[2].isDeleted != 0 {
		t.Errorf("m3 booleans = (seen=%d flagged=%d deleted=%d), want (0,1,0)",
			got[2].isSeen, got[2].isFlagged, got[2].isDeleted)
	}
}

// TestMigrateV10_BackfillsSubjectCt seeds a v9-shaped DB with three messages
// (one normal subject, one empty, one with non-ASCII) and verifies that the
// v9→v10 migration adds subject_ct, encrypts the non-empty subjects, leaves
// the empty one as NULL, and that subsequent reads via the production code
// path return the original plaintext.
func TestMigrateV10_BackfillsSubjectCt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "step5.db")

	// Seed a v9-shaped DB on disk. Schema is the v9 columns; pin
	// schema_version = 9 so migrate() only runs the v9→v10 step.
	seedDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	seed := []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO schema_version (rowid, version) VALUES (1, 9)`,
		`CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id TEXT NOT NULL,
			thread_id TEXT NOT NULL,
			in_reply_to TEXT,
			refs TEXT,
			subject TEXT,
			from_addr TEXT,
			to_addrs TEXT,
			cc_addrs TEXT,
			date INTEGER,
			created_at INTEGER NOT NULL,
			body_text TEXT,
			body_html TEXT,
			mailbox TEXT,
			flags TEXT,
			uid INTEGER DEFAULT 0,
			size INTEGER DEFAULT 0,
			fetched_body INTEGER DEFAULT 0,
			account TEXT DEFAULT '',
			mailbox_id INTEGER,
			account_id INTEGER,
			is_seen INTEGER NOT NULL DEFAULT 0,
			is_flagged INTEGER NOT NULL DEFAULT 0,
			is_deleted INTEGER NOT NULL DEFAULT 0,
			UNIQUE(message_id, account)
		)`,
		`INSERT INTO messages (message_id, thread_id, in_reply_to, refs, subject,
		    from_addr, to_addrs, cc_addrs, date, created_at,
		    body_text, body_html, mailbox, flags) VALUES
		   ('m1', 't1', '', '', 'Hello world',           '', '', '', 0, 1, '', '', '', ''),
		   ('m2', 't2', '', '', '',                       '', '', '', 0, 2, '', '', '', ''),
		   ('m3', 't3', '', '', 'Grüße aus München 🌍', '', '', '', 0, 3, '', '', '', '')`,
		// Real v9 DBs have messages_fts populated from earlier inserts. Mirror
		// that here so the UPDATE inside the v9→v10 backfill doesn't trip the
		// AU trigger's FTS5 delete sentinel (SQLITE_CORRUPT_VTAB on empty fts).
		`CREATE VIRTUAL TABLE messages_fts USING fts5(
			subject, from_addr, to_addrs, body_text,
			content='messages',
			content_rowid='id'
		)`,
		`INSERT INTO messages_fts(rowid, subject, from_addr, to_addrs, body_text)
		 SELECT id, subject, COALESCE(from_addr, ''), COALESCE(to_addrs, ''), COALESCE(body_text, '')
		 FROM messages`,
	}
	for _, stmt := range seed {
		if _, err := seedDB.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\nstmt: %s", err, stmt)
		}
	}
	seedDB.Close()

	sd, err := Open(dbPath, testKeyring(t))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { sd.Close() })
	if err := sd.Init(); err != nil {
		t.Fatalf("init/migrate: %v", err)
	}

	var version int
	if err := sd.db.QueryRow("SELECT version FROM schema_version WHERE rowid = 1").Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != 10 {
		t.Fatalf("version = %d, want 10", version)
	}

	// Raw check: subject_ct must be NULL for empty subject, non-NULL for the
	// other two. Plaintext subject column unchanged.
	rows, err := sd.db.Query("SELECT message_id, subject, subject_ct FROM messages ORDER BY message_id")
	if err != nil {
		t.Fatalf("query messages: %v", err)
	}
	defer rows.Close()
	type row struct {
		id        string
		subject   string
		subjectCT []byte
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.subject, &r.subjectCT); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != 3 {
		t.Fatalf("messages = %d, want 3", len(got))
	}
	if got[0].subject != "Hello world" || len(got[0].subjectCT) == 0 {
		t.Errorf("m1: subject=%q ct.len=%d, want non-empty ct", got[0].subject, len(got[0].subjectCT))
	}
	if got[1].subject != "" || got[1].subjectCT != nil {
		t.Errorf("m2: subject=%q ct=%v, want empty subject and NULL ct", got[1].subject, got[1].subjectCT)
	}
	if got[2].subject != "Grüße aus München 🌍" || len(got[2].subjectCT) == 0 {
		t.Errorf("m3: subject=%q ct.len=%d, want non-empty ct", got[2].subject, len(got[2].subjectCT))
	}

	// Round-trip via the production read path: GetByMessageID must return
	// the decrypted subject, which has to equal the original plaintext.
	for _, want := range got {
		msg, err := sd.GetByMessageID(want.id)
		if err != nil {
			t.Fatalf("get %s: %v", want.id, err)
		}
		if msg == nil {
			t.Fatalf("get %s: nil message", want.id)
		}
		if msg.Subject != want.subject {
			t.Errorf("%s: read subject %q, want %q", want.id, msg.Subject, want.subject)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && searchSubstring(s, sub)
}

func searchSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
