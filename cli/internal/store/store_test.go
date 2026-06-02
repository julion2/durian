package store

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/julion2/durian/cli/internal/dbcrypto"
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
	if version != 22 {
		t.Errorf("version = %d, want 22", version)
	}
}

func TestOpen_RejectsNilKeyring(t *testing.T) {
	if _, err := Open(":memory:", nil); err == nil {
		t.Error("Open(nil keyring) should error")
	}
}

// TestOpen_SecureDeleteEnabled asserts ADR-0001 step 8: every fresh
// connection has PRAGMA secure_delete = ON, so DELETE / UPDATE
// overwrites freed pages with zeros before reuse. The plaintext
// metadata Durian deliberately keeps (from_addr, to_addrs, cc_addrs,
// message_id, dates) would otherwise stay recoverable in the .db file
// until something else writes over the page.
func TestOpen_SecureDeleteEnabled(t *testing.T) {
	db := newTestDB(t)
	var v int
	if err := db.db.QueryRow("PRAGMA secure_delete").Scan(&v); err != nil {
		t.Fatalf("query pragma: %v", err)
	}
	if v != 1 {
		t.Errorf("PRAGMA secure_delete = %d, want 1", v)
	}
}

// TestSecureDelete_ScrubsRawBytes is the ADR-0001 audit #252 follow-up
// to TestOpen_SecureDeleteEnabled: pragma-set-to-1 only proves SQLite
// accepted the directive, not that the freed bytes are actually being
// overwritten on disk. This test asserts the real property — INSERT a
// plaintext marker into a plaintext column (from_addr stays plaintext
// post-β-revision), DELETE the row, checkpoint WAL into the main file,
// close, then grep the raw bytes of every on-disk file (the .db, its
// -wal, its -shm). The marker must be absent from all three.
//
// A pre-deletion sanity grep proves the marker really was on disk in the
// first place — without it, "absent after delete" could just mean the
// insert never reached the bytes we read.
// TestFreelistTriggersVacuum pins the ADR-0001 audit #251 calibration.
// Concrete page counts come from realistic scenarios:
//   - healthy 50k-page DB after a sync (~10 free pages)
//   - same DB after months of churn without VACUUM (~800 free pages)
//   - post-Time-Machine-restore residue (10 %+ stranded plaintext)
//   - tiny test DB where the ratio threshold catches what the floor misses
//
// If the calibration drifts, this test catches the user-visible
// regression (false fires on every Open, or missed restore residue).
func TestFreelistTriggersVacuum(t *testing.T) {
	cases := []struct {
		name      string
		pages     int64
		freelist  int64
		wantFires bool
	}{
		{"healthy post-sync (50k pages, 10 free)", 50_000, 10, false},
		{"long-running but small churn (50k, 800)", 50_000, 800, false},
		{"large absolute residue (50k, 6000 ~= 12 %)", 50_000, 6000, true},
		{"restore residue ratio (10k pages, 30 % free)", 10_000, 3_000, true},
		{"tiny DB at ratio (1000 pages, 100 free = 10 %)", 1_000, 100, true},
		{"tiny DB below ratio (1000 pages, 50 free = 5 %)", 1_000, 50, false},
		{"empty DB", 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := freelistTriggersVacuum(c.pages, c.freelist); got != c.wantFires {
				t.Errorf("freelistTriggersVacuum(%d, %d) = %v, want %v",
					c.pages, c.freelist, got, c.wantFires)
			}
		})
	}
}

// TestVacuumIfFreelistHigh_FiresWhenForced exercises the end-to-end
// path: artificially set the floor so any non-empty freelist triggers,
// allocate + delete enough rows to inflate the freelist, run the helper
// directly, then assert PRAGMA freelist_count dropped. Real-Open
// integration is implicit: every other store test goes through Open and
// gets the production thresholds (which never fire on small test DBs).
func TestVacuumIfFreelistHigh_FiresWhenForced(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "freelist.db")

	db, err := Open(dbPath, testKeyring(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}

	// Inflate the freelist: insert many rows, then delete them all.
	// secure_delete=ON zeros the pages but they stay on the freelist
	// until VACUUM compacts them away.
	for i := range 200 {
		msg := &Message{
			MessageID: fmt.Sprintf("inflate-%d@example.com", i),
			Subject:   strings.Repeat("padding ", 50),
			FromAddr:  fmt.Sprintf("sender-%d@example.com", i),
			ToAddrs:   "bob@example.com",
			Date:      1700000000,
			CreatedAt: 1700000000,
			Mailbox:   "INBOX",
			BodyText:  strings.Repeat("body ", 200),
		}
		if err := db.InsertMessage(msg); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if _, err := db.db.Exec("DELETE FROM messages"); err != nil {
		t.Fatalf("bulk delete: %v", err)
	}

	var beforeFree int64
	if err := db.db.QueryRow("PRAGMA freelist_count").Scan(&beforeFree); err != nil {
		t.Fatalf("read freelist pre-VACUUM: %v", err)
	}
	if beforeFree == 0 {
		t.Skip("could not inflate freelist on this SQLite build — test inert")
	}

	// Lower both thresholds for this test so the helper actually fires.
	origRatio, origFloor := freelistVacuumRatio, freelistVacuumFloor
	t.Cleanup(func() {
		freelistVacuumRatio = origRatio
		freelistVacuumFloor = origFloor
	})
	freelistVacuumRatio = 0
	freelistVacuumFloor = 1

	if err := vacuumIfFreelistHigh(db.db); err != nil {
		t.Fatalf("vacuumIfFreelistHigh: %v", err)
	}

	var afterFree int64
	if err := db.db.QueryRow("PRAGMA freelist_count").Scan(&afterFree); err != nil {
		t.Fatalf("read freelist post-VACUUM: %v", err)
	}
	if afterFree >= beforeFree {
		t.Errorf("freelist did not shrink: before=%d after=%d", beforeFree, afterFree)
	}
}

func TestSecureDelete_ScrubsRawBytes(t *testing.T) {
	const marker = "DURIAN_SECURE_DELETE_MARKER_3F8B7A1E"

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "email.db")

	db, err := Open(dbPath, testKeyring(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}

	msg := &Message{
		MessageID: "marker@example.com",
		Subject:   "test",
		FromAddr:  marker + "@example.com",
		ToAddrs:   "bob@example.com",
		Date:      1700000000,
		CreatedAt: 1700000000,
		Mailbox:   "INBOX",
	}
	if err := db.InsertMessage(msg); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Force the row into the main DB so the pre-delete sanity grep is
	// looking at the right file — without this, the WAL holds the bytes
	// and the main .db is still empty.
	if _, err := db.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatalf("pre-delete checkpoint: %v", err)
	}
	if !rawDBContains(t, dbPath, marker) {
		t.Fatalf("test setup broken: marker %q not present in raw bytes pre-delete", marker)
	}

	if err := db.DeleteByMessageID("marker@example.com"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Merge the secure-delete-zeroed page from WAL into the main DB file.
	if _, err := db.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatalf("post-delete checkpoint: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		path := dbPath + suffix
		b, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			t.Fatalf("read %s: %v", path, err)
		}
		if bytes.Contains(b, []byte(marker)) {
			t.Errorf("marker %q still present in %s after secure_delete (%d bytes)", marker, path, len(b))
		}
	}
}

// rawDBContains returns whether the raw bytes of dbPath (or its WAL
// sidecar, if the marker hasn't been checkpointed yet) contain the
// marker. Used by the secure_delete sanity check.
func rawDBContains(t *testing.T, dbPath, marker string) bool {
	t.Helper()
	for _, suffix := range []string{"", "-wal"} {
		path := dbPath + suffix
		b, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			t.Fatalf("read %s: %v", path, err)
		}
		if bytes.Contains(b, []byte(marker)) {
			return true
		}
	}
	return false
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
	if version != 22 {
		t.Fatalf("version = %d, want 22", version)
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
