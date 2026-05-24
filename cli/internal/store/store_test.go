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
	if version != 18 {
		t.Errorf("version = %d, want 18", version)
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
	if version != 18 {
		t.Fatalf("version = %d, want 18", version)
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



// TestBlindFTS_InsertAndMatch verifies the step-7 (a+b) round-trip:
// a message inserted via the store API populates messages_blind_fts,
// and a token computed independently with the same FTSToken sub-key
// matches the right rowid via SQLite's FTS5 MATCH operator.
func TestBlindFTS_InsertAndMatch(t *testing.T) {
	db := newTestDB(t)
	kr := testKeyring(t)

	msg := &Message{
		MessageID: "blindtest@example",
		Subject:   "Hello Blindtoken World",
		FromAddr:  "alice@example.com",
		ToAddrs:   "bob@example.com",
		BodyText:  "the quick brown fox",
		CreatedAt: 1,
		Account:   "acct1",
	}
	if err := db.InsertMessage(msg); err != nil {
		t.Fatalf("insert: %v", err)
	}

	tok := dbcrypto.TokenizeFTS(kr.FTSToken, "blindtoken")
	if tok == "" {
		t.Fatal("tokenizer returned empty for non-empty input")
	}
	var rowid int64
	err := db.db.QueryRow(
		`SELECT rowid FROM messages_blind_fts WHERE subject_tok MATCH ? LIMIT 1`,
		tok,
	).Scan(&rowid)
	if err != nil {
		t.Fatalf("blind fts match: %v", err)
	}
	if rowid != msg.ID {
		t.Errorf("rowid = %d, want %d", rowid, msg.ID)
	}

	notInSubject := dbcrypto.TokenizeFTS(kr.FTSToken, "kraftfahrzeughaftpflichtversicherung")
	var dummy int64
	err = db.db.QueryRow(
		`SELECT rowid FROM messages_blind_fts WHERE subject_tok MATCH ? LIMIT 1`,
		notInSubject,
	).Scan(&dummy)
	if err == nil {
		t.Errorf("blind fts matched a word not in any subject (rowid=%d)", dummy)
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
