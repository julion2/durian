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
	if version != 12 {
		t.Errorf("version = %d, want 12", version)
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
	if version != 12 {
		t.Fatalf("version = %d, want 12", version)
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
	if version != 12 {
		t.Fatalf("version = %d, want 12", version)
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

// TestMigrateV11_BackfillsBodyCt seeds a v10-shaped DB with a mix of
// body_text/body_html populations and asserts the v10→v11 migration
// encrypts only the non-empty ones, leaves the rest NULL, and that
// GetByMessageID round-trips through decryptBody.
func TestMigrateV11_BackfillsBodyCt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "step6a.db")

	seedDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	seed := []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO schema_version (rowid, version) VALUES (1, 10)`,
		`CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id TEXT NOT NULL,
			thread_id TEXT NOT NULL,
			in_reply_to TEXT,
			refs TEXT,
			subject TEXT,
			subject_ct BLOB,
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
		   ('m1', 't1', '', '', '', '', '', '', 0, 1, 'plain text body only', '',          '', ''),
		   ('m2', 't2', '', '', '', '', '', '', 0, 2, '',                     '<p>html</p>','', ''),
		   ('m3', 't3', '', '', '', '', '', '', 0, 3, 'both formats',         '<p>both</p>','', ''),
		   ('m4', 't4', '', '', '', '', '', '', 0, 4, '',                     '',          '', '')`,
		// FTS5 + content for the AU trigger to operate on. step 5/10 wrote
		// subject_ct via migration; step 6a leaves both columns plaintext
		// in this seed because the rows have empty subjects.
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
	if version != 12 {
		t.Fatalf("version = %d, want 12", version)
	}

	// Raw check: body_text_ct populated only where body_text non-empty.
	rows, err := sd.db.Query(`SELECT message_id, body_text, body_text_ct, body_html, body_html_ct
		FROM messages ORDER BY message_id`)
	if err != nil {
		t.Fatalf("query messages: %v", err)
	}
	defer rows.Close()
	type row struct {
		id           string
		text, html   string
		textCT, htmlCT []byte
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.text, &r.textCT, &r.html, &r.htmlCT); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != 4 {
		t.Fatalf("messages = %d, want 4", len(got))
	}
	// m1: text only
	if got[0].text == "" || len(got[0].textCT) == 0 || got[0].htmlCT != nil {
		t.Errorf("m1 unexpected: text=%q textCT.len=%d htmlCT=%v", got[0].text, len(got[0].textCT), got[0].htmlCT)
	}
	// m2: html only
	if got[1].textCT != nil || len(got[1].htmlCT) == 0 {
		t.Errorf("m2 unexpected: textCT=%v htmlCT.len=%d", got[1].textCT, len(got[1].htmlCT))
	}
	// m3: both
	if len(got[2].textCT) == 0 || len(got[2].htmlCT) == 0 {
		t.Errorf("m3 unexpected: textCT.len=%d htmlCT.len=%d", len(got[2].textCT), len(got[2].htmlCT))
	}
	// m4: neither
	if got[3].textCT != nil || got[3].htmlCT != nil {
		t.Errorf("m4 unexpected: textCT=%v htmlCT=%v", got[3].textCT, got[3].htmlCT)
	}

	// Round-trip via production read path.
	for _, want := range got {
		msg, err := sd.GetByMessageID(want.id)
		if err != nil {
			t.Fatalf("get %s: %v", want.id, err)
		}
		if msg == nil {
			t.Fatalf("get %s: nil", want.id)
		}
		if msg.BodyText != want.text {
			t.Errorf("%s: body_text=%q, want %q", want.id, msg.BodyText, want.text)
		}
		if msg.BodyHTML != want.html {
			t.Errorf("%s: body_html=%q, want %q", want.id, msg.BodyHTML, want.html)
		}
	}
}

// TestMigrateV12_BackfillsAddrsCt seeds a v11-shaped DB with a mix of
// from/to/cc populations and asserts the v11→v12 migration encrypts
// only the non-empty addresses, leaves the rest NULL, and that
// GetByMessageID round-trips through decryptAddr.
func TestMigrateV12_BackfillsAddrsCt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "step6-addrs.db")
	seedDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	seed := []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO schema_version (rowid, version) VALUES (1, 11)`,
		`CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id TEXT NOT NULL,
			thread_id TEXT NOT NULL,
			in_reply_to TEXT,
			refs TEXT,
			subject TEXT,
			subject_ct BLOB,
			from_addr TEXT,
			to_addrs TEXT,
			cc_addrs TEXT,
			date INTEGER,
			created_at INTEGER NOT NULL,
			body_text TEXT,
			body_text_ct BLOB,
			body_html TEXT,
			body_html_ct BLOB,
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
		   ('m1', 't1', '', '', '', 'a@x.com',  'b@x.com',  '',         0, 1, '', '', '', ''),
		   ('m2', 't2', '', '', '', '',         '',         'c@x.com',  0, 2, '', '', '', ''),
		   ('m3', 't3', '', '', '', 'a@x.com',  'b@x.com',  'c@x.com',  0, 3, '', '', '', ''),
		   ('m4', 't4', '', '', '', '',         '',         '',         0, 4, '', '', '', '')`,
		`CREATE VIRTUAL TABLE messages_fts USING fts5(
			subject, from_addr, to_addrs, body_text,
			content='messages',
			content_rowid='id'
		)`,
		`INSERT INTO messages_fts(rowid, subject, from_addr, to_addrs, body_text)
		 SELECT id, COALESCE(subject, ''), COALESCE(from_addr, ''), COALESCE(to_addrs, ''), COALESCE(body_text, '')
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
	if version != 12 {
		t.Fatalf("version = %d, want 12", version)
	}

	rows, err := sd.db.Query(`SELECT message_id, from_addr, from_addr_ct,
		to_addrs, to_addrs_ct, cc_addrs, cc_addrs_ct
		FROM messages ORDER BY message_id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	type row struct {
		id                                       string
		from, to, cc                             string
		fromCT, toCT, ccCT                       []byte
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.from, &r.fromCT, &r.to, &r.toCT, &r.cc, &r.ccCT); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != 4 {
		t.Fatalf("rows = %d, want 4", len(got))
	}
	checks := []struct {
		idx                                 int
		wantFromCT, wantToCT, wantCCCT bool
	}{
		{0, true, true, false},
		{1, false, false, true},
		{2, true, true, true},
		{3, false, false, false},
	}
	for _, c := range checks {
		r := got[c.idx]
		if (len(r.fromCT) > 0) != c.wantFromCT {
			t.Errorf("%s from_addr_ct populated=%v, want %v", r.id, len(r.fromCT) > 0, c.wantFromCT)
		}
		if (len(r.toCT) > 0) != c.wantToCT {
			t.Errorf("%s to_addrs_ct populated=%v, want %v", r.id, len(r.toCT) > 0, c.wantToCT)
		}
		if (len(r.ccCT) > 0) != c.wantCCCT {
			t.Errorf("%s cc_addrs_ct populated=%v, want %v", r.id, len(r.ccCT) > 0, c.wantCCCT)
		}
	}

	// Round-trip via the production read path.
	for _, want := range got {
		msg, err := sd.GetByMessageID(want.id)
		if err != nil {
			t.Fatalf("get %s: %v", want.id, err)
		}
		if msg == nil {
			t.Fatalf("get %s: nil", want.id)
		}
		if msg.FromAddr != want.from || msg.ToAddrs != want.to || msg.CCAddrs != want.cc {
			t.Errorf("%s addrs mismatch: got (%q/%q/%q), want (%q/%q/%q)",
				want.id, msg.FromAddr, msg.ToAddrs, msg.CCAddrs,
				want.from, want.to, want.cc)
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
