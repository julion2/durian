package syncengine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/dbcrypto"
	"github.com/julion2/durian/cli/internal/store"
)

// testAccount is the account identifier used across the engine tests.
const testAccount = "test@example.com"

// newTestDB replicates the store package's test bootstrap (store_test.go):
// pinned keyring bytes, in-memory DB, Init, cleanup-close.
func newTestDB(t *testing.T) *store.DB {
	t.Helper()
	kr, err := dbcrypto.NewKeyring(bytes.Repeat([]byte{0x42}, dbcrypto.MasterKeyLen))
	if err != nil {
		t.Fatalf("test keyring: %v", err)
	}
	db, err := store.Open(":memory:", kr)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// rawMessage builds a minimal but valid RFC822 message.
func rawMessage(msgID, from, to, subject, body string) []byte {
	return []byte("From: " + from + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"Message-ID: <" + msgID + ">\r\n" +
		"Date: Mon, 20 Jul 2026 10:00:00 +0000\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		body + "\r\n")
}

// memCursorStore is a map-backed CursorStore so the tests avoid the
// filesystem/flock machinery of FileCursorStore.
type memCursorStore struct {
	cursors map[string]backend.Cursor
}

func newMemCursorStore() *memCursorStore {
	return &memCursorStore{cursors: make(map[string]backend.Cursor)}
}

func (m *memCursorStore) key(account, folder string) string { return account + "\x00" + folder }

func (m *memCursorStore) Get(account, folder string) (backend.Cursor, error) {
	return m.cursors[m.key(account, folder)], nil
}

func (m *memCursorStore) Set(account, folder string, cursor backend.Cursor) error {
	m.cursors[m.key(account, folder)] = cursor
	return nil
}

// fetchFlagsCall records one FetchFlags invocation for assertions.
type fetchFlagsCall struct {
	folder string
	refs   []backend.RemoteRef
}

// applyFlagsCall records one ApplyFlags invocation for assertions.
type applyFlagsCall struct {
	ref    backend.RemoteRef
	add    backend.Flags
	remove backend.Flags
}

// fakeBackend implements backend.Backend with per-folder scripted FetchResults.
// Successive FetchMessages calls for a folder consume the script in order; when
// the script is exhausted it reports an unchanged folder (prior cursor, no
// changes, HasMore false).
type fakeBackend struct {
	folders     []backend.Folder
	scripts     map[string][]backend.FetchResult
	calls       map[string]int
	seenCursors map[string][]backend.Cursor // cursor argument of each FetchMessages call

	// flagsByRef scripts the server flag state FetchFlags reports per
	// RemoteRef.ID. Empty by default, so unrelated tests see "not on server"
	// for every message and the engine's flag pass no-ops.
	flagsByRef map[string]backend.Flags
	// fetchFlagsCalls / applyFlagsCalls record the flag-pass invocations.
	fetchFlagsCalls []fetchFlagsCall
	applyFlagsCalls []applyFlagsCall
	// moveCalls records Backend.Move invocations (folder-move upload pass).
	moveCalls []moveCall
	// moveErr, when set, is returned by every Move instead of succeeding.
	moveErr error
	// caps is returned by Capabilities(); zero value matches the default IMAP-like
	// backend. Tests set FlagChangesInDelta to exercise the delta flag path.
	caps backend.Capabilities
}

type moveCall struct {
	ref  backend.RemoteRef
	dest string
}

func newFakeBackend(folders []backend.Folder, scripts map[string][]backend.FetchResult) *fakeBackend {
	return &fakeBackend{
		folders:     folders,
		scripts:     scripts,
		calls:       make(map[string]int),
		seenCursors: make(map[string][]backend.Cursor),
		flagsByRef:  make(map[string]backend.Flags),
	}
}

func (f *fakeBackend) FetchFolders(ctx context.Context) ([]backend.Folder, error) {
	return f.folders, nil
}

func (f *fakeBackend) FetchMessages(ctx context.Context, folder string, cursor backend.Cursor, limit int) (backend.FetchResult, error) {
	idx := f.calls[folder]
	f.calls[folder]++
	f.seenCursors[folder] = append(f.seenCursors[folder], cursor)
	script := f.scripts[folder]
	if idx < len(script) {
		return script[idx], nil
	}
	// Unchanged folder: prior cursor verbatim, no changes.
	return backend.FetchResult{Cursor: cursor}, nil
}

func (f *fakeBackend) FetchBody(ctx context.Context, ref backend.RemoteRef, w io.Writer) error {
	return fmt.Errorf("fakeBackend: FetchBody not scripted")
}

func (f *fakeBackend) ApplyFlags(ctx context.Context, ref backend.RemoteRef, add, remove backend.Flags) error {
	f.applyFlagsCalls = append(f.applyFlagsCalls, applyFlagsCall{ref: ref, add: add, remove: remove})
	return nil
}

func (f *fakeBackend) FetchFlags(ctx context.Context, folder string, refs []backend.RemoteRef) (map[string]backend.Flags, error) {
	f.fetchFlagsCalls = append(f.fetchFlagsCalls, fetchFlagsCall{folder: folder, refs: slices.Clone(refs)})
	result := make(map[string]backend.Flags)
	for _, ref := range refs {
		if flags, ok := f.flagsByRef[ref.ID]; ok {
			result[ref.ID] = flags
		}
	}
	return result, nil
}

func (f *fakeBackend) Move(ctx context.Context, ref backend.RemoteRef, destFolder string) (backend.RemoteRef, error) {
	f.moveCalls = append(f.moveCalls, moveCall{ref: ref, dest: destFolder})
	if f.moveErr != nil {
		return backend.RemoteRef{}, f.moveErr
	}
	return backend.RemoteRef{Folder: destFolder, ID: ref.ID}, nil
}

func (f *fakeBackend) Append(ctx context.Context, folder string, flags backend.Flags, msg []byte) (backend.RemoteRef, error) {
	return backend.RemoteRef{Folder: folder, ID: "appended"}, nil
}

func (f *fakeBackend) Send(ctx context.Context, msg []byte) error { return nil }

func (f *fakeBackend) Watch(ctx context.Context, folder string, onChange func()) error { return nil }

func (f *fakeBackend) Capabilities() backend.Capabilities { return f.caps }

func (f *fakeBackend) Close() error { return nil }

// compile-time interface check
var _ backend.Backend = (*fakeBackend)(nil)

// newTestEngine wires an Engine with the shared test account on both the
// engine options and the ingest options (the real caller does the same).
func newTestEngine(db *store.DB, cursors CursorStore) *Engine {
	return New(Options{
		Store:   db,
		Cursors: cursors,
		Account: testAccount,
		Ingest:  IngestOptions{Account: testAccount},
	})
}

// TestEngineFolderFilterMatchesRole proves a caller-supplied "INBOX" selects
// the inbox on a provider that names folders by opaque id and displays them in
// the mailbox's own language (a German Microsoft Graph tenant), and that a
// filter matching nothing fails loudly instead of reporting an empty success.
func TestEngineFolderFilterMatchesRole(t *testing.T) {
	db := newTestDB(t)
	// Shaped like a real Graph mailbox: ids for names, localized displays.
	folders := []backend.Folder{
		{Name: "AQMkAGQ3-inbox", Display: "Posteingang", Role: backend.RoleInbox, Selectable: true},
		{Name: "AQMkAGQ3-sent", Display: "Gesendete Elemente", Role: backend.RoleSent, Selectable: true},
	}
	scripts := map[string][]backend.FetchResult{
		"AQMkAGQ3-inbox": {{
			Messages: []backend.Message{{
				MessageID: "neu@example.com",
				Ref:       backend.RemoteRef{Folder: "AQMkAGQ3-inbox", ID: "1"},
				Raw:       rawMessage("neu@example.com", "b@example.com", testAccount, "Neu", "body"),
			}},
			Cursor: backend.Cursor("c1"),
		}},
	}
	fake := newFakeBackend(folders, scripts)

	engine := New(Options{
		Store: db, Cursors: newMemCursorStore(), Account: testAccount,
		Folders: []string{"INBOX"},
		Ingest:  IngestOptions{Account: testAccount},
	})
	res, err := engine.Sync(context.Background(), fake)
	if err != nil {
		t.Fatalf("sync with INBOX filter: %v", err)
	}
	if res.Folders != 1 {
		t.Errorf("Folders = %d, want 1 (INBOX must match the inbox role)", res.Folders)
	}
	if res.New != 1 {
		t.Errorf("New = %d, want 1", res.New)
	}

	// A filter that matches nothing is a caller bug, not a quiet success.
	engine = New(Options{
		Store: db, Cursors: newMemCursorStore(), Account: testAccount,
		Folders: []string{"Nonexistent"},
		Ingest:  IngestOptions{Account: testAccount},
	})
	if _, err := engine.Sync(context.Background(), fake); err == nil {
		t.Error("sync with a filter matching no folder returned nil error")
	}
}

// TestEngineReportsNewInboxArrivals proves the engine reports which messages
// are genuinely new in an inbox-role folder — the provider-neutral basis for
// new-mail notifications — and that arrivals elsewhere do not qualify.
func TestEngineReportsNewInboxArrivals(t *testing.T) {
	db := newTestDB(t)
	folders := []backend.Folder{
		{Name: "INBOX", Display: "Inbox", Role: backend.RoleInbox, Selectable: true},
		{Name: "Sent", Display: "Sent", Role: backend.RoleSent, Selectable: true},
	}
	scripts := map[string][]backend.FetchResult{
		"INBOX": {{
			Messages: []backend.Message{{
				MessageID: "arrived@example.com",
				Ref:       backend.RemoteRef{Folder: "INBOX", ID: "1"},
				Raw:       rawMessage("arrived@example.com", "b@example.com", testAccount, "Hello", "body"),
			}},
			Cursor: backend.Cursor("c1"),
		}},
		"Sent": {{
			Messages: []backend.Message{{
				MessageID: "iwrote@example.com",
				Ref:       backend.RemoteRef{Folder: "Sent", ID: "2"},
				Raw:       rawMessage("iwrote@example.com", testAccount, "c@example.com", "My reply", "body"),
			}},
			Cursor: backend.Cursor("c1"),
		}},
	}
	fake := newFakeBackend(folders, scripts)
	cursors := newMemCursorStore()

	res, err := newTestEngine(db, cursors).Sync(context.Background(), fake)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if got, want := res.NewMessageIDs, []string{"arrived@example.com"}; !slices.Equal(got, want) {
		t.Errorf("NewMessageIDs = %v, want %v (only inbox arrivals notify)", got, want)
	}

	// Second sync: nothing new, so nothing to notify about — a re-delivered
	// message must not be announced as an arrival a second time.
	res, err = newTestEngine(db, cursors).Sync(context.Background(), fake)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if len(res.NewMessageIDs) != 0 {
		t.Errorf("NewMessageIDs = %v on a no-change sync, want none", res.NewMessageIDs)
	}
}

// TestIsRetryableStoreError pins which ingest failures may hold a folder's
// cursor back. Getting this wrong is costly in both directions: treating a
// permanent failure as retryable re-downloads the folder forever without
// progressing, and treating write contention as permanent drops the message.
func TestIsRetryableStoreError(t *testing.T) {
	retryable := []string{
		"insert message: resolve mailbox: insert mailbox: database is locked (5) (SQLITE_BUSY)",
		"insert message: database is locked (517)",
		"database table is locked",
	}
	for _, msg := range retryable {
		if !isRetryableStoreError(errors.New(msg)) {
			t.Errorf("isRetryableStoreError(%q) = false, want true", msg)
		}
	}
	permanent := []string{
		"insert message: UNIQUE constraint failed: messages.message_id",
		"parse message: malformed MIME header",
		"encrypt subject: key unavailable",
	}
	for _, msg := range permanent {
		if isRetryableStoreError(errors.New(msg)) {
			t.Errorf("isRetryableStoreError(%q) = true, want false (would block the folder forever)", msg)
		}
	}
}

// mustTags fetches the tags for a Message-ID or fails the test.
func mustTags(t *testing.T, db *store.DB, messageID string) []string {
	t.Helper()
	tags, err := db.GetTagsByMessageID(messageID)
	if err != nil {
		t.Fatalf("get tags for %s: %v", messageID, err)
	}
	return tags
}

// TestEngineUploadsFolderMoves proves the upload pass: an INBOX message whose
// "inbox" tag was removed locally (GUI archive) is moved to Archive via
// Backend.Move on the next sync, a "deleted"-tagged one goes to Trash, and an
// untouched one stays put.
func TestEngineUploadsFolderMoves(t *testing.T) {
	db := newTestDB(t)
	folders := []backend.Folder{
		{Name: "INBOX", Display: "Inbox", Role: backend.RoleInbox, Selectable: true},
		{Name: "Archive", Display: "Archive", Role: backend.RoleArchive, Selectable: true},
		{Name: "Trash", Display: "Trash", Role: backend.RoleTrash, Selectable: true},
	}
	scripts := map[string][]backend.FetchResult{
		"INBOX": {{
			Messages: []backend.Message{
				{
					MessageID: "keep@example.com",
					Ref:       backend.RemoteRef{Folder: "INBOX", ID: "1"},
					Raw:       rawMessage("keep@example.com", "a@example.com", testAccount, "Keep", "body"),
				},
				{
					MessageID: "arch@example.com",
					Ref:       backend.RemoteRef{Folder: "INBOX", ID: "2"},
					Raw:       rawMessage("arch@example.com", "b@example.com", testAccount, "Archive me", "body"),
				},
				{
					MessageID: "del@example.com",
					Ref:       backend.RemoteRef{Folder: "INBOX", ID: "3"},
					Raw:       rawMessage("del@example.com", "c@example.com", testAccount, "Delete me", "body"),
				},
			},
			Cursor: backend.Cursor("c1"),
		}},
	}
	fake := newFakeBackend(folders, scripts)
	cursors := newMemCursorStore()

	// First sync ingests all three into INBOX (each gets the "inbox" tag).
	if _, err := newTestEngine(db, cursors).Sync(context.Background(), fake); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Simulate GUI actions: archive one (drop inbox), delete another
	// (drop inbox, add deleted).
	if err := db.ModifyTagsByMessageIDAndAccount("arch@example.com", testAccount, nil, []string{"inbox"}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if err := db.ModifyTagsByMessageIDAndAccount("del@example.com", testAccount, []string{"deleted"}, []string{"inbox"}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Second sync uploads the moves.
	res, err := newTestEngine(db, cursors).Sync(context.Background(), fake)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if res.Moved != 2 {
		t.Errorf("Result.Moved = %d, want 2", res.Moved)
	}

	dests := map[string]string{} // Move ref.ID -> destination
	for _, m := range fake.moveCalls {
		dests[m.ref.ID] = m.dest
	}
	if len(fake.moveCalls) != 2 {
		t.Fatalf("moveCalls = %d, want 2: %+v", len(fake.moveCalls), fake.moveCalls)
	}
	if dests["2"] != "Archive" {
		t.Errorf("archived message dest = %q, want Archive", dests["2"])
	}
	if dests["3"] != "Trash" {
		t.Errorf("deleted message dest = %q, want Trash", dests["3"])
	}

	// The kept message was never moved and stays in INBOX.
	if keep, _ := db.GetByMessageID("keep@example.com"); keep == nil || keep.Mailbox != "INBOX" {
		t.Errorf("kept message should remain in INBOX")
	}
	// The archived message's row now points at Archive (not deleted locally).
	if arch, _ := db.GetByMessageID("arch@example.com"); arch == nil || arch.Mailbox != "Archive" {
		t.Errorf("archived message should have mailbox Archive")
	}
}

// TestEngineFolderMoveGoneRefReconciles proves the dead-ref path: when the
// server no longer knows the message's handle (Graph renumbers on move, so a
// message archived from another client leaves a permanently dead id behind),
// the move is not a sync error, and the local row is reconciled so the same
// doomed move is not retried on every subsequent sync.
func TestEngineFolderMoveGoneRefReconciles(t *testing.T) {
	db := newTestDB(t)
	folders := []backend.Folder{
		{Name: "INBOX", Display: "Inbox", Role: backend.RoleInbox, Selectable: true},
		{Name: "Archive", Display: "Archive", Role: backend.RoleArchive, Selectable: true},
	}
	scripts := map[string][]backend.FetchResult{
		"INBOX": {{
			Messages: []backend.Message{{
				MessageID: "gone@example.com",
				Ref:       backend.RemoteRef{Folder: "INBOX", ID: "stale-id"},
				Raw:       rawMessage("gone@example.com", "b@example.com", testAccount, "Gone", "body"),
			}},
			Cursor: backend.Cursor("c1"),
		}},
	}
	fake := newFakeBackend(folders, scripts)
	cursors := newMemCursorStore()

	if _, err := newTestEngine(db, cursors).Sync(context.Background(), fake); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if err := db.ModifyTagsByMessageIDAndAccount("gone@example.com", testAccount, nil, []string{"inbox"}); err != nil {
		t.Fatalf("archive: %v", err)
	}

	// The server says the handle is dead — as Graph does with 404
	// ErrorItemNotFound after the message moved elsewhere.
	fake.moveErr = fmt.Errorf("failed to move stale-id: %w", backend.ErrRefGone)

	res, err := newTestEngine(db, cursors).Sync(context.Background(), fake)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Errorf("Result.Errors = %v, want none (a gone ref is not a sync failure)", res.Errors)
	}
	if len(fake.moveCalls) != 1 {
		t.Fatalf("moveCalls = %d, want 1", len(fake.moveCalls))
	}
	// Row survives — the message still exists on the server, just elsewhere.
	row, _ := db.GetByMessageID("gone@example.com")
	if row == nil {
		t.Fatalf("row deleted; a gone ref must not lose the message locally")
	}
	if row.Mailbox != "Archive" {
		t.Errorf("mailbox = %q, want Archive (reconciled to the intended destination)", row.Mailbox)
	}

	// Third sync: the row is out of INBOX, so the dead move is never retried.
	if _, err := newTestEngine(db, cursors).Sync(context.Background(), fake); err != nil {
		t.Fatalf("third sync: %v", err)
	}
	if len(fake.moveCalls) != 1 {
		t.Errorf("moveCalls = %d after third sync, want 1 (dead move must not repeat)", len(fake.moveCalls))
	}
}

// TestEngineUploadOnlySkipsDownload proves --upload-only pushes local moves
// without re-ingesting: the archived message moves, but nothing is downloaded
// (so a still-in-server-INBOX message is not re-tagged "inbox").
func TestEngineUploadOnlySkipsDownload(t *testing.T) {
	db := newTestDB(t)
	folders := []backend.Folder{
		{Name: "INBOX", Display: "Inbox", Role: backend.RoleInbox, Selectable: true},
		{Name: "Archive", Display: "Archive", Role: backend.RoleArchive, Selectable: true},
	}
	scripts := map[string][]backend.FetchResult{
		"INBOX": {{
			Messages: []backend.Message{{
				MessageID: "arch@example.com",
				Ref:       backend.RemoteRef{Folder: "INBOX", ID: "9"},
				Raw:       rawMessage("arch@example.com", "b@example.com", testAccount, "Archive me", "body"),
			}},
			Cursor: backend.Cursor("c1"),
		}},
	}
	fake := newFakeBackend(folders, scripts)
	cursors := newMemCursorStore()

	// Ingest, then archive locally.
	if _, err := newTestEngine(db, cursors).Sync(context.Background(), fake); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if err := db.ModifyTagsByMessageIDAndAccount("arch@example.com", testAccount, nil, []string{"inbox"}); err != nil {
		t.Fatalf("archive: %v", err)
	}

	up := New(Options{Store: db, Cursors: cursors, Account: testAccount, Mode: UploadOnly, Ingest: IngestOptions{Account: testAccount}})
	res, err := up.Sync(context.Background(), fake)
	if err != nil {
		t.Fatalf("upload-only sync: %v", err)
	}
	if res.New != 0 {
		t.Errorf("upload-only Result.New = %d, want 0 (no download)", res.New)
	}
	if res.Moved != 1 {
		t.Errorf("upload-only Result.Moved = %d, want 1", res.Moved)
	}
	if len(fake.moveCalls) != 1 || fake.moveCalls[0].dest != "Archive" {
		t.Errorf("expected one move to Archive, got %+v", fake.moveCalls)
	}
}

// TestEngineSyncIngests proves the end-to-end path: fake backend -> Engine.Sync
// -> Ingest -> store rows + folder-role tags + flag tags.
func TestEngineSyncIngests(t *testing.T) {
	db := newTestDB(t)

	folders := []backend.Folder{
		{Name: "INBOX", Display: "Inbox", Role: backend.RoleInbox, Selectable: true},
		{Name: "Archive", Display: "Archive", Role: backend.RoleArchive, Selectable: true},
	}
	scripts := map[string][]backend.FetchResult{
		"INBOX": {{
			Messages: []backend.Message{{
				MessageID: "unread-msg@example.com",
				Ref:       backend.RemoteRef{Folder: "INBOX", ID: "101"},
				Raw:       rawMessage("unread-msg@example.com", "alice@example.com", testAccount, "Hello unread", "unread body"),
				Flags:     backend.Flags{Seen: false},
			}},
			Cursor: backend.Cursor("inbox-c1"),
		}},
		"Archive": {{
			Messages: []backend.Message{{
				MessageID: "seen-msg@example.com",
				Ref:       backend.RemoteRef{Folder: "Archive", ID: "201"},
				Raw:       rawMessage("seen-msg@example.com", "bob@example.com", testAccount, "Old news", "archived body"),
				Flags:     backend.Flags{Seen: true},
			}},
			Cursor: backend.Cursor("archive-c1"),
		}},
	}
	fake := newFakeBackend(folders, scripts)
	cursors := newMemCursorStore()

	res, err := newTestEngine(db, cursors).Sync(context.Background(), fake)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("sync errors: %v", res.Errors)
	}
	if res.Folders != 2 {
		t.Errorf("Result.Folders = %d, want 2", res.Folders)
	}
	if res.New != 2 {
		t.Errorf("Result.New = %d, want 2", res.New)
	}
	if res.Deleted != 0 {
		t.Errorf("Result.Deleted = %d, want 0", res.Deleted)
	}

	// INBOX message: row present, "inbox" + "unread" tags (Seen=false).
	inboxMsg, err := db.GetByMessageID("unread-msg@example.com")
	if err != nil {
		t.Fatalf("get inbox msg: %v", err)
	}
	if inboxMsg == nil {
		t.Fatal("inbox message not ingested")
	}
	if inboxMsg.Mailbox != "INBOX" {
		t.Errorf("inbox msg mailbox = %q, want INBOX", inboxMsg.Mailbox)
	}
	if inboxMsg.Subject != "Hello unread" {
		t.Errorf("inbox msg subject = %q, want %q", inboxMsg.Subject, "Hello unread")
	}
	if inboxMsg.Account != testAccount {
		t.Errorf("inbox msg account = %q, want %q", inboxMsg.Account, testAccount)
	}
	inboxTags := mustTags(t, db, "unread-msg@example.com")
	if !slices.Contains(inboxTags, "inbox") {
		t.Errorf("inbox msg tags = %v, want to contain %q", inboxTags, "inbox")
	}
	if !slices.Contains(inboxTags, "unread") {
		t.Errorf("inbox msg tags = %v, want to contain %q (Seen=false)", inboxTags, "unread")
	}

	// Archive message: row present, "archive" tag, no "inbox", no "unread" (Seen=true).
	archMsg, err := db.GetByMessageID("seen-msg@example.com")
	if err != nil {
		t.Fatalf("get archive msg: %v", err)
	}
	if archMsg == nil {
		t.Fatal("archive message not ingested")
	}
	if archMsg.Mailbox != "Archive" {
		t.Errorf("archive msg mailbox = %q, want Archive", archMsg.Mailbox)
	}
	archTags := mustTags(t, db, "seen-msg@example.com")
	if !slices.Contains(archTags, "archive") {
		t.Errorf("archive msg tags = %v, want to contain %q", archTags, "archive")
	}
	if slices.Contains(archTags, "inbox") {
		t.Errorf("archive msg tags = %v, must not contain %q", archTags, "inbox")
	}
	if slices.Contains(archTags, "unread") {
		t.Errorf("archive msg tags = %v, must not contain %q (Seen=true)", archTags, "unread")
	}

	// Cursors persisted per folder.
	for folder, want := range map[string]string{"INBOX": "inbox-c1", "Archive": "archive-c1"} {
		got, _ := cursors.Get(testAccount, folder)
		if string(got) != want {
			t.Errorf("persisted cursor for %s = %q, want %q", folder, got, want)
		}
	}
}

// TestEngineSyncDeletion covers both documented handleDeleted behaviors:
// a user folder (RoleNone, no tag mapping) deletes the row, a role folder
// removes only the folder tag and keeps the row.
func TestEngineSyncDeletion(t *testing.T) {
	t.Run("user folder deletes row", func(t *testing.T) {
		db := newTestDB(t)
		folders := []backend.Folder{
			{Name: "Projects", Role: backend.RoleNone, Selectable: true},
		}
		scripts := map[string][]backend.FetchResult{
			"Projects": {
				{ // first sync: ingest
					Messages: []backend.Message{{
						MessageID: "proj-msg@example.com",
						Ref:       backend.RemoteRef{Folder: "Projects", ID: "301"},
						Raw:       rawMessage("proj-msg@example.com", "carol@example.com", testAccount, "Project plan", "plan body"),
						Flags:     backend.Flags{Seen: true},
					}},
					Cursor: backend.Cursor("proj-c1"),
				},
				{ // second sync: server-side deletion, durable Message-ID resolved
					Deleted: []backend.Deletion{{
						Ref:       backend.RemoteRef{Folder: "Projects", ID: "301"},
						MessageID: "proj-msg@example.com",
					}},
					Cursor: backend.Cursor("proj-c2"),
				},
			},
		}
		fake := newFakeBackend(folders, scripts)
		engine := newTestEngine(db, newMemCursorStore())

		if _, err := engine.Sync(context.Background(), fake); err != nil {
			t.Fatalf("first sync: %v", err)
		}
		if msg, err := db.GetByMessageID("proj-msg@example.com"); err != nil || msg == nil {
			t.Fatalf("message not ingested by first sync (msg=%v, err=%v)", msg, err)
		}

		res, err := engine.Sync(context.Background(), fake)
		if err != nil {
			t.Fatalf("second sync: %v", err)
		}
		if len(res.Errors) != 0 {
			t.Fatalf("second sync errors: %v", res.Errors)
		}
		if res.Deleted != 1 {
			t.Errorf("Result.Deleted = %d, want 1", res.Deleted)
		}
		msg, err := db.GetByMessageID("proj-msg@example.com")
		if err != nil {
			t.Fatalf("get after delete: %v", err)
		}
		if msg != nil {
			t.Errorf("row for user-folder message still present after deletion, want removed")
		}
	})

	t.Run("graph-style deletion resolves via remote_ref", func(t *testing.T) {
		// Graph delta @removed items carry only the provider id (no Message-ID);
		// the engine must resolve it to the durable key via the persisted
		// remote_ref (Ingest stores Ref.ID as remote_ref).
		db := newTestDB(t)
		folders := []backend.Folder{
			{Name: "Projects", Role: backend.RoleNone, Selectable: true},
		}
		scripts := map[string][]backend.FetchResult{
			"Projects": {
				{
					Messages: []backend.Message{{
						MessageID: "graph-del@example.com",
						Ref:       backend.RemoteRef{Folder: "Projects", ID: "graph-id-9"},
						Raw:       rawMessage("graph-del@example.com", "carol@example.com", testAccount, "Plan", "body"),
						Flags:     backend.Flags{Seen: true},
					}},
					Cursor: backend.Cursor("g-c1"),
				},
				{ // deletion with NO Message-ID, only the provider handle
					Deleted: []backend.Deletion{{
						Ref:       backend.RemoteRef{Folder: "Projects", ID: "graph-id-9"},
						MessageID: "",
					}},
					Cursor: backend.Cursor("g-c2"),
				},
			},
		}
		fake := newFakeBackend(folders, scripts)
		engine := newTestEngine(db, newMemCursorStore())

		if _, err := engine.Sync(context.Background(), fake); err != nil {
			t.Fatalf("first sync: %v", err)
		}
		res, err := engine.Sync(context.Background(), fake)
		if err != nil {
			t.Fatalf("second sync: %v", err)
		}
		if res.Deleted != 1 {
			t.Errorf("Result.Deleted = %d, want 1 (resolved via remote_ref)", res.Deleted)
		}
		if msg, _ := db.GetByMessageID("graph-del@example.com"); msg != nil {
			t.Error("row still present; deletion did not resolve via remote_ref")
		}
	})

	t.Run("role folder removes tag, keeps row", func(t *testing.T) {
		db := newTestDB(t)
		folders := []backend.Folder{
			{Name: "INBOX", Role: backend.RoleInbox, Selectable: true},
		}
		scripts := map[string][]backend.FetchResult{
			"INBOX": {
				{
					Messages: []backend.Message{{
						MessageID: "moved-msg@example.com",
						Ref:       backend.RemoteRef{Folder: "INBOX", ID: "401"},
						Raw:       rawMessage("moved-msg@example.com", "dave@example.com", testAccount, "Will be moved", "moved body"),
						Flags:     backend.Flags{Seen: true},
					}},
					Cursor: backend.Cursor("inbox-c1"),
				},
				{
					Deleted: []backend.Deletion{{
						Ref:       backend.RemoteRef{Folder: "INBOX", ID: "401"},
						MessageID: "moved-msg@example.com",
					}},
					Cursor: backend.Cursor("inbox-c2"),
				},
			},
		}
		fake := newFakeBackend(folders, scripts)
		engine := newTestEngine(db, newMemCursorStore())

		if _, err := engine.Sync(context.Background(), fake); err != nil {
			t.Fatalf("first sync: %v", err)
		}
		if tags := mustTags(t, db, "moved-msg@example.com"); !slices.Contains(tags, "inbox") {
			t.Fatalf("precondition failed: tags = %v, want to contain %q", tags, "inbox")
		}

		res, err := engine.Sync(context.Background(), fake)
		if err != nil {
			t.Fatalf("second sync: %v", err)
		}
		if res.Deleted != 1 {
			t.Errorf("Result.Deleted = %d, want 1", res.Deleted)
		}
		msg, err := db.GetByMessageID("moved-msg@example.com")
		if err != nil {
			t.Fatalf("get after role-folder deletion: %v", err)
		}
		if msg == nil {
			t.Fatal("row for role-folder message was deleted, want kept (message likely moved)")
		}
		if tags := mustTags(t, db, "moved-msg@example.com"); slices.Contains(tags, "inbox") {
			t.Errorf("tags after role-folder deletion = %v, must not contain %q", tags, "inbox")
		}
	})
}

// TestEnginePagination proves the engine follows HasMore across batches with
// the updated cursor and terminates when HasMore is false.
func TestEnginePagination(t *testing.T) {
	db := newTestDB(t)
	folders := []backend.Folder{
		{Name: "INBOX", Role: backend.RoleInbox, Selectable: true},
	}
	scripts := map[string][]backend.FetchResult{
		"INBOX": {
			{
				Messages: []backend.Message{{
					MessageID: "page1@example.com",
					Ref:       backend.RemoteRef{Folder: "INBOX", ID: "501"},
					Raw:       rawMessage("page1@example.com", "erin@example.com", testAccount, "First page", "page one"),
					Flags:     backend.Flags{Seen: false},
				}},
				Cursor:  backend.Cursor("c1"),
				HasMore: true,
			},
			{
				Messages: []backend.Message{{
					MessageID: "page2@example.com",
					Ref:       backend.RemoteRef{Folder: "INBOX", ID: "502"},
					Raw:       rawMessage("page2@example.com", "frank@example.com", testAccount, "Second page", "page two"),
					Flags:     backend.Flags{Seen: true},
				}},
				Cursor:  backend.Cursor("c2"),
				HasMore: false,
			},
		},
	}
	fake := newFakeBackend(folders, scripts)
	cursors := newMemCursorStore()

	res, err := newTestEngine(db, cursors).Sync(context.Background(), fake)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("sync errors: %v", res.Errors)
	}
	if res.New != 2 {
		t.Errorf("Result.New = %d, want 2 (both pages)", res.New)
	}
	if got := fake.calls["INBOX"]; got != 2 {
		t.Errorf("FetchMessages calls = %d, want exactly 2 (loop must terminate)", got)
	}
	// Second call must have been made with the cursor from the first batch.
	if got := fake.seenCursors["INBOX"]; len(got) != 2 || string(got[1]) != "c1" {
		t.Errorf("second-call cursor = %v, want [nil c1]", got)
	}
	for _, id := range []string{"page1@example.com", "page2@example.com"} {
		msg, err := db.GetByMessageID(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if msg == nil {
			t.Errorf("message %s not ingested", id)
		}
	}
	if got, _ := cursors.Get(testAccount, "INBOX"); string(got) != "c2" {
		t.Errorf("persisted cursor = %q, want %q", got, "c2")
	}
}

// TestEngineMaxPerFolder proves the per-folder cap stops paging early even when
// the backend keeps reporting HasMore, so a first sync does not pull a folder's
// entire history (parity with the legacy syncer's GetIMAPMaxMessages).
func TestEngineMaxPerFolder(t *testing.T) {
	db := newTestDB(t)
	folders := []backend.Folder{
		{Name: "INBOX", Role: backend.RoleInbox, Selectable: true},
	}
	// Three batches, every one advertising more to come.
	mk := func(id, cursor string) backend.FetchResult {
		return backend.FetchResult{
			Messages: []backend.Message{{
				MessageID: id,
				Ref:       backend.RemoteRef{Folder: "INBOX", ID: id},
				Raw:       rawMessage(id, "s@example.com", testAccount, "capped", "body"),
				Flags:     backend.Flags{Seen: true},
			}},
			Cursor:  backend.Cursor(cursor),
			HasMore: true,
		}
	}
	scripts := map[string][]backend.FetchResult{
		"INBOX": {mk("cap1@example.com", "c1"), mk("cap2@example.com", "c2"), mk("cap3@example.com", "c3")},
	}
	fake := newFakeBackend(folders, scripts)

	engine := New(Options{
		Store:        db,
		Cursors:      newMemCursorStore(),
		Account:      testAccount,
		MaxPerFolder: 2,
		Ingest:       IngestOptions{Account: testAccount},
	})

	res, err := engine.Sync(context.Background(), fake)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.New != 2 {
		t.Errorf("Result.New = %d, want 2 (capped)", res.New)
	}
	if got := fake.calls["INBOX"]; got != 2 {
		t.Errorf("FetchMessages calls = %d, want exactly 2 (cap must stop paging)", got)
	}
	if msg, _ := db.GetByMessageID("cap3@example.com"); msg != nil {
		t.Error("third message ingested, want skipped by the per-folder cap")
	}
}

// TestEngineFlagUpload proves the upload half of the engine's three-way flag
// pass: a message ingested unread (Seen=false, so its stored baseline has no
// \Seen) that the user then marks read locally (unread tag removed) is pushed
// to the server via ApplyFlags with add.Seen=true on the next sync, and the
// store's synced_flags baseline advances to include \Seen.
func TestEngineFlagUpload(t *testing.T) {
	db := newTestDB(t)
	folders := []backend.Folder{
		{Name: "INBOX", Role: backend.RoleInbox, Selectable: true},
	}
	scripts := map[string][]backend.FetchResult{
		"INBOX": {{
			Messages: []backend.Message{{
				MessageID: "flagup@example.com",
				Ref:       backend.RemoteRef{Folder: "INBOX", ID: "601"},
				Raw:       rawMessage("flagup@example.com", "gina@example.com", testAccount, "Mark me read", "flag body"),
				Flags:     backend.Flags{Seen: false},
			}},
			Cursor: backend.Cursor("flag-c1"),
		}},
	}
	fake := newFakeBackend(folders, scripts)
	engine := newTestEngine(db, newMemCursorStore())

	if _, err := engine.Sync(context.Background(), fake); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if n := len(fake.applyFlagsCalls); n != 0 {
		t.Fatalf("ApplyFlags called %d times on first sync, want 0 (nothing changed)", n)
	}

	// The server still reports the message unread (unchanged since ingest).
	fake.flagsByRef["601"] = backend.Flags{Seen: false}

	// The user marks the message read locally: the unread tag is removed.
	if err := db.ModifyTagsByMessageIDAndAccount("flagup@example.com", testAccount, nil, []string{"unread"}); err != nil {
		t.Fatalf("mark read locally: %v", err)
	}

	res, err := engine.Sync(context.Background(), fake)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("second sync errors: %v", res.Errors)
	}

	// The local Seen change must have been uploaded via ApplyFlags.
	if len(fake.applyFlagsCalls) != 1 {
		t.Fatalf("ApplyFlags calls = %d, want exactly 1", len(fake.applyFlagsCalls))
	}
	call := fake.applyFlagsCalls[0]
	if call.ref.Folder != "INBOX" || call.ref.ID != "601" {
		t.Errorf("ApplyFlags ref = %+v, want {INBOX 601}", call.ref)
	}
	if !call.add.Seen {
		t.Errorf("ApplyFlags add = %+v, want Seen=true", call.add)
	}
	if call.remove != (backend.Flags{}) {
		t.Errorf("ApplyFlags remove = %+v, want no flags removed", call.remove)
	}

	// The stored baseline must have advanced to include \Seen, so the next
	// sync no longer treats the change as pending.
	rows, err := db.GetFolderFlagState(testAccount, "INBOX")
	if err != nil {
		t.Fatalf("get folder flag state: %v", err)
	}
	var found bool
	for _, row := range rows {
		if row.MessageID == "flagup@example.com" {
			found = true
			if !strings.Contains(row.SyncedFlags, `\Seen`) {
				t.Errorf("synced_flags baseline = %q, want to contain \\Seen", row.SyncedFlags)
			}
		}
	}
	if !found {
		t.Fatal("no folder flag row for flagup@example.com")
	}
}

// TestEngineNewVsDeduplicated proves the run counts a genuinely new message as
// New but a re-delivered one (same Message-ID, e.g. a delta flag change) as
// Deduplicated — so "new" reflects arrivals, not re-syncs.
func TestEngineLabelsAsTags(t *testing.T) {
	db := newTestDB(t)
	// A RoleInbox folder proves the folder-role "inbox" mapping is skipped when
	// labels are authoritative.
	folders := []backend.Folder{{Name: "ALL", Role: backend.RoleInbox, Selectable: true}}
	deliver := func(labels []string, cursor string) backend.FetchResult {
		return backend.FetchResult{
			Messages: []backend.Message{{
				MessageID: "lbl@example.com",
				Ref:       backend.RemoteRef{Folder: "ALL", ID: "701"},
				Raw:       rawMessage("lbl@example.com", "a@example.com", testAccount, "hi", "body"),
				Labels:    labels,
			}},
			Cursor: backend.Cursor(cursor),
		}
	}
	scripts := map[string][]backend.FetchResult{
		"ALL": {
			deliver([]string{"newsletter"}, "c1"),
			deliver([]string{"important"}, "c2"), // newsletter dropped, important added
		},
	}
	fake := newFakeBackend(folders, scripts)
	fake.caps.LabelsAreTags = true
	engine := newTestEngine(db, newMemCursorStore())

	has := func(want string) bool {
		rows, err := db.GetFolderFlagState(testAccount, "ALL")
		if err != nil {
			t.Fatalf("flag state: %v", err)
		}
		for _, r := range rows {
			if r.MessageID == "lbl@example.com" {
				for _, tag := range r.Tags {
					if tag == want {
						return true
					}
				}
			}
		}
		return false
	}

	if _, err := engine.Sync(context.Background(), fake); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if !has("newsletter") || has("inbox") {
		t.Errorf("after first sync: want newsletter tag, no inbox (folder-role skipped)")
	}
	// A local rule tag the user/rules added must survive label reconciliation.
	if err := db.ModifyTagsByMessageIDAndAccount("lbl@example.com", testAccount, []string{"todo"}, nil); err != nil {
		t.Fatalf("add local tag: %v", err)
	}

	if _, err := engine.Sync(context.Background(), fake); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if has("newsletter") {
		t.Error("newsletter tag should be removed when its label was dropped")
	}
	if !has("important") {
		t.Error("important tag should be added from the new label")
	}
	if !has("todo") {
		t.Error("local rule tag 'todo' must survive label reconciliation")
	}
}

func TestEngineNewVsDeduplicated(t *testing.T) {
	db := newTestDB(t)
	folders := []backend.Folder{{Name: "INBOX", Role: backend.RoleInbox, Selectable: true}}
	msg := backend.Message{
		MessageID: "dedup@example.com",
		Ref:       backend.RemoteRef{Folder: "INBOX", ID: "701"},
		Raw:       rawMessage("dedup@example.com", "a@example.com", testAccount, "hi", "body"),
		Flags:     backend.Flags{Seen: false},
	}
	scripts := map[string][]backend.FetchResult{
		"INBOX": {
			{Messages: []backend.Message{msg}, Cursor: backend.Cursor("c1")},
			{Messages: []backend.Message{msg}, Cursor: backend.Cursor("c2")}, // re-delivery
		},
	}
	fake := newFakeBackend(folders, scripts)
	engine := newTestEngine(db, newMemCursorStore())

	r1, err := engine.Sync(context.Background(), fake)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if r1.New != 1 || r1.Deduplicated != 0 {
		t.Errorf("first sync: New=%d Deduplicated=%d, want 1/0", r1.New, r1.Deduplicated)
	}

	r2, err := engine.Sync(context.Background(), fake)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if r2.New != 0 || r2.Deduplicated != 1 {
		t.Errorf("second sync: New=%d Deduplicated=%d, want 0/1 (a re-delivery is not new)", r2.New, r2.Deduplicated)
	}
}

// TestEngineFlagPassScopedToDelta proves a delta backend fetches flags only for
// messages that changed — surfaced in the delta or dirty locally — instead of
// polling the whole folder. That scoping is the sync-speed win.
func TestEngineFlagPassScopedToDelta(t *testing.T) {
	db := newTestDB(t)
	folders := []backend.Folder{{Name: "INBOX", Role: backend.RoleInbox, Selectable: true}}
	scripts := map[string][]backend.FetchResult{
		"INBOX": {{
			Messages: []backend.Message{
				{MessageID: "d601@example.com", Ref: backend.RemoteRef{Folder: "INBOX", ID: "601"},
					Raw: rawMessage("d601@example.com", "a@example.com", testAccount, "one", "b1"), Flags: backend.Flags{Seen: false}},
				{MessageID: "d602@example.com", Ref: backend.RemoteRef{Folder: "INBOX", ID: "602"},
					Raw: rawMessage("d602@example.com", "a@example.com", testAccount, "two", "b2"), Flags: backend.Flags{Seen: false}},
			},
			Cursor: backend.Cursor("d-c1"),
		}},
	}
	fake := newFakeBackend(folders, scripts)
	fake.caps.FlagChangesInDelta = true
	engine := newTestEngine(db, newMemCursorStore())

	if _, err := engine.Sync(context.Background(), fake); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	// Discard first-sync fetches: both messages are new, hence delta candidates.
	fake.fetchFlagsCalls = nil

	// The user marks 602 read locally; 601 is untouched and absent from the next
	// delta (the second FetchMessages reports no changes).
	if err := db.ModifyTagsByMessageIDAndAccount("d602@example.com", testAccount, nil, []string{"unread"}); err != nil {
		t.Fatalf("mark 602 read: %v", err)
	}
	fake.flagsByRef["602"] = backend.Flags{Seen: false} // server still unread

	if _, err := engine.Sync(context.Background(), fake); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	// Exactly one flag fetch, for 602 only — 601 (unchanged, not in the delta)
	// is never polled. A full-folder poll would have fetched both.
	if len(fake.fetchFlagsCalls) != 1 {
		t.Fatalf("FetchFlags calls = %d, want 1 (scoped to changes)", len(fake.fetchFlagsCalls))
	}
	got := fake.fetchFlagsCalls[0].refs
	if len(got) != 1 || got[0].ID != "602" {
		t.Errorf("FetchFlags refs = %+v, want only [602] (whole-folder poll eliminated)", got)
	}
	// The local read change was still uploaded.
	if len(fake.applyFlagsCalls) != 1 || fake.applyFlagsCalls[0].ref.ID != "602" || !fake.applyFlagsCalls[0].add.Seen {
		t.Errorf("ApplyFlags = %+v, want one Seen upload for 602", fake.applyFlagsCalls)
	}
}

// TestEngineNoFlags proves Options.NoFlags disables the flag pass entirely:
// FetchFlags must never be called.
func TestEngineNoFlags(t *testing.T) {
	db := newTestDB(t)
	folders := []backend.Folder{
		{Name: "INBOX", Role: backend.RoleInbox, Selectable: true},
	}
	scripts := map[string][]backend.FetchResult{
		"INBOX": {{
			Messages: []backend.Message{{
				MessageID: "noflags@example.com",
				Ref:       backend.RemoteRef{Folder: "INBOX", ID: "701"},
				Raw:       rawMessage("noflags@example.com", "hank@example.com", testAccount, "No flag pass", "body"),
				Flags:     backend.Flags{Seen: false},
			}},
			Cursor: backend.Cursor("nf-c1"),
		}},
	}
	fake := newFakeBackend(folders, scripts)

	engine := New(Options{
		Store:   db,
		Cursors: newMemCursorStore(),
		Account: testAccount,
		NoFlags: true,
		Ingest:  IngestOptions{Account: testAccount},
	})

	res, err := engine.Sync(context.Background(), fake)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.New != 1 {
		t.Errorf("Result.New = %d, want 1 (ingest must still run)", res.New)
	}
	if n := len(fake.fetchFlagsCalls); n != 0 {
		t.Errorf("FetchFlags called %d times with NoFlags=true, want 0", n)
	}
}

// TestFileCursorStoreRoundTrip proves file persistence: Set then Get returns
// the same bytes, and an unknown folder returns nil.
func TestFileCursorStoreRoundTrip(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	fcs := NewFileCursorStore(testAccount)
	want := []byte("opaque-cursor-\x00\x01\x02-bytes")
	if err := fcs.Set(testAccount, "INBOX", backend.Cursor(want)); err != nil {
		t.Fatalf("set: %v", err)
	}

	got, err := fcs.Get(testAccount, "INBOX")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("round-trip cursor = %q, want %q", got, want)
	}

	// A fresh store instance must see the persisted cursor (real file I/O).
	got2, err := NewFileCursorStore(testAccount).Get(testAccount, "INBOX")
	if err != nil {
		t.Fatalf("get via fresh store: %v", err)
	}
	if !bytes.Equal(got2, want) {
		t.Errorf("fresh-store cursor = %q, want %q", got2, want)
	}

	unknown, err := fcs.Get(testAccount, "NoSuchFolder")
	if err != nil {
		t.Fatalf("get unknown: %v", err)
	}
	if unknown != nil {
		t.Errorf("unknown folder cursor = %q, want nil", unknown)
	}
}
