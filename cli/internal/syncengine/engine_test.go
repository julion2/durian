package syncengine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
	cursors             map[string]backend.Cursor
	pending             map[string]PendingFlags
	stateReads          int
	commits             int
	serializedRefCounts []int
}

func newMemCursorStore() *memCursorStore {
	return &memCursorStore{cursors: make(map[string]backend.Cursor), pending: make(map[string]PendingFlags)}
}

func (m *memCursorStore) key(account, folder string) string { return account + "\x00" + folder }

func (m *memCursorStore) Get(account, folder string) (backend.Cursor, error) {
	return m.cursors[m.key(account, folder)], nil
}

func (m *memCursorStore) GetState(account, folder string) (FolderState, error) {
	m.stateReads++
	key := m.key(account, folder)
	return FolderState{Cursor: m.cursors[key], PendingFlags: m.pending[key]}, nil
}

func (m *memCursorStore) Set(account, folder string, cursor backend.Cursor) error {
	m.cursors[m.key(account, folder)] = cursor
	return nil
}

func (m *memCursorStore) GetPendingFlags(account, folder string) (PendingFlags, error) {
	return m.pending[m.key(account, folder)], nil
}

func (m *memCursorStore) Commit(account, folder string, cursor backend.Cursor, pending PendingFlags) error {
	key := m.key(account, folder)
	m.cursors[key] = cursor
	m.pending[key] = pending
	m.commits++
	m.serializedRefCounts = append(m.serializedRefCounts, len(pending.Refs))
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
	folders []backend.Folder
	scripts map[string][]backend.FetchResult
	// fetchByCursor, when set, models a provider state machine whose response
	// is selected by the persisted opaque cursor rather than call order.
	fetchByCursor    func(folder string, cursor backend.Cursor) backend.FetchResult
	fetchErrByCursor map[string]error
	calls            map[string]int
	seenCursors      map[string][]backend.Cursor // cursor argument of each FetchMessages call

	// flagsByRef scripts the server flag state FetchFlags reports per
	// RemoteRef.ID. Empty by default, so unrelated tests see "not on server"
	// for every message and the engine's flag pass no-ops.
	flagsByRef        map[string]backend.Flags
	fetchFlagsErr     error
	fetchFlagsPartial bool
	// fetchFlagsResolvable narrows a partial result to a genuine subset: when
	// fetchFlagsPartial is set, only refs listed here resolve, and the rest are
	// omitted alongside the error. Nil means nothing resolves, which is the
	// degenerate partial (empty map + error) the older tests rely on.
	fetchFlagsResolvable map[string]bool
	// beforeFetchFlags runs at the top of FetchFlags, between ingest and the
	// flag pass of the same Sync call. It is the seam for modelling a local
	// mutation that lands mid-sync rather than between two syncs.
	beforeFetchFlags func()
	// fetchFlagsCalls / applyFlagsCalls record the flag-pass invocations.
	fetchFlagsCalls []fetchFlagsCall
	applyFlagsCalls []applyFlagsCall
	applyFlagsErr   error
	// moveCalls records Backend.Move invocations (folder-move upload pass).
	moveCalls []moveCall
	// moveErr, when set, is returned by every Move instead of succeeding.
	moveErr error
	// labelVocab is what LabelTags reports; labelCalls records ApplyLabels
	// invocations (label-upload pass). Both are the LabelWriter side used by the
	// Gmail-style label-upload tests. Empty vocab makes the pass a no-op.
	labelVocab       []string
	labelCalls       []labelCall
	snapshotMessages map[string]backend.Message
	snapshotMissing  map[string]bool
	snapshotHydrated []backend.RemoteRef
	snapshotMetadata map[string]backend.Message
	metadataMissing  map[string]bool
	metadataFetched  []backend.RemoteRef
	// caps is returned by Capabilities(); zero value matches the default IMAP-like
	// backend. Tests set FlagChangesInDelta to exercise the delta flag path.
	caps backend.Capabilities
}

// backendOnly deliberately hides optional interfaces implemented by fakeBackend
// so tests can exercise Graph/IMAP-style full-body replacement snapshots.
type backendOnly struct{ backend.Backend }

type delayedBackend struct {
	backend.Backend
	delays []time.Duration
	calls  int
}

type delayedFlagsBackend struct {
	backend.Backend
	delay time.Duration
}

func (b *delayedBackend) FetchMessages(ctx context.Context, folder string, cursor backend.Cursor, limit int) (backend.FetchResult, error) {
	if b.calls < len(b.delays) && b.delays[b.calls] > 0 {
		timer := time.NewTimer(b.delays[b.calls])
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return backend.FetchResult{}, ctx.Err()
		case <-timer.C:
		}
	}
	b.calls++
	return b.Backend.FetchMessages(ctx, folder, cursor, limit)
}

func (b *delayedFlagsBackend) FetchFlags(ctx context.Context, folder string, refs []backend.RemoteRef) (map[string]backend.Flags, error) {
	timer := time.NewTimer(b.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return b.Backend.FetchFlags(ctx, folder, refs)
	}
}

type labelCall struct {
	ref    backend.RemoteRef
	add    []string
	remove []string
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
	if err := f.fetchErrByCursor[string(cursor)]; err != nil {
		delete(f.fetchErrByCursor, string(cursor))
		return backend.FetchResult{}, err
	}
	if f.fetchByCursor != nil {
		return f.fetchByCursor(folder, cursor), nil
	}
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

func (f *fakeBackend) FetchSnapshotMessages(_ context.Context, refs []backend.RemoteRef) (backend.SnapshotBatch, error) {
	f.snapshotHydrated = append(f.snapshotHydrated, refs...)
	var result backend.SnapshotBatch
	for _, ref := range refs {
		if f.snapshotMissing[ref.ID] {
			result.Missing = append(result.Missing, ref)
			continue
		}
		if message, ok := f.snapshotMessages[ref.ID]; ok {
			result.Messages = append(result.Messages, message)
		}
	}
	return result, nil
}

func (f *fakeBackend) FetchSnapshotMetadata(_ context.Context, refs []backend.RemoteRef) (backend.SnapshotBatch, error) {
	f.metadataFetched = append(f.metadataFetched, refs...)
	result := backend.SnapshotBatch{Messages: make([]backend.Message, 0, len(refs))}
	for _, ref := range refs {
		if f.metadataMissing[ref.ID] {
			result.Missing = append(result.Missing, ref)
			continue
		}
		message, ok := f.snapshotMetadata[ref.ID]
		if !ok {
			message.Ref = ref
		}
		result.Messages = append(result.Messages, message)
	}
	return result, nil
}

func (f *fakeBackend) ApplyFlags(ctx context.Context, ref backend.RemoteRef, add, remove backend.Flags) error {
	f.applyFlagsCalls = append(f.applyFlagsCalls, applyFlagsCall{ref: ref, add: add, remove: remove})
	if f.applyFlagsErr != nil {
		return f.applyFlagsErr
	}
	// A real provider stores what it is told, so a later FetchFlags reports it
	// back. Without this the scripted state is frozen and a test cannot tell an
	// upload that converged from one that undid the server's own change.
	state := f.flagsByRef[ref.ID]
	for _, m := range []struct {
		field *bool
		add   bool
		rm    bool
	}{
		{&state.Seen, add.Seen, remove.Seen},
		{&state.Flagged, add.Flagged, remove.Flagged},
		{&state.Answered, add.Answered, remove.Answered},
		{&state.Deleted, add.Deleted, remove.Deleted},
		{&state.Completed, add.Completed, remove.Completed},
	} {
		if m.add {
			*m.field = true
		} else if m.rm {
			*m.field = false
		}
	}
	f.flagsByRef[ref.ID] = state
	return nil
}

func (f *fakeBackend) FetchFlags(ctx context.Context, folder string, refs []backend.RemoteRef) (map[string]backend.Flags, error) {
	if f.beforeFetchFlags != nil {
		f.beforeFetchFlags()
	}
	f.fetchFlagsCalls = append(f.fetchFlagsCalls, fetchFlagsCall{folder: folder, refs: slices.Clone(refs)})
	if f.fetchFlagsErr != nil {
		if f.fetchFlagsPartial {
			resolved := make(map[string]backend.Flags)
			for _, ref := range refs {
				if !f.fetchFlagsResolvable[ref.ID] {
					continue
				}
				if flags, ok := f.flagsByRef[ref.ID]; ok {
					resolved[ref.ID] = flags
				}
			}
			return resolved, f.fetchFlagsErr
		}
		return nil, f.fetchFlagsErr
	}
	result := make(map[string]backend.Flags)
	for _, ref := range refs {
		if flags, ok := f.flagsByRef[ref.ID]; ok {
			result[ref.ID] = flags
		}
	}
	return result, nil
}

func (f *fakeBackend) LabelTags(ctx context.Context) ([]string, error) { return f.labelVocab, nil }

func (f *fakeBackend) ApplyLabels(ctx context.Context, ref backend.RemoteRef, add, remove []string) error {
	f.labelCalls = append(f.labelCalls, labelCall{ref: ref, add: add, remove: remove})
	return nil
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

func TestEngineReportsInboxLabelArrivalFromAllMailStream(t *testing.T) {
	db := newTestDB(t)
	fake := newFakeBackend([]backend.Folder{{Name: "ALL", Role: backend.RoleAll, Selectable: true}}, map[string][]backend.FetchResult{
		"ALL": {{
			Messages: []backend.Message{{
				MessageID: "jmap-arrival@example.com",
				Ref:       backend.RemoteRef{Folder: "ALL", ID: "j1"},
				Raw:       rawMessage("jmap-arrival@example.com", "a@example.com", testAccount, "Hello", "body"),
				Labels:    []string{"inbox"},
			}},
			Cursor: backend.Cursor("c1"),
		}},
	})
	fake.caps.LabelsAreTags = true
	res, err := newTestEngine(db, newMemCursorStore()).Sync(context.Background(), fake)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := res.NewMessageIDs, []string{"jmap-arrival@example.com"}; !slices.Equal(got, want) {
		t.Fatalf("NewMessageIDs = %v, want %v", got, want)
	}
}

func TestEngineReconcilesReplacementFullSnapshot(t *testing.T) {
	db := newTestDB(t)
	folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
	message := func(id, ref string) backend.Message {
		return backend.Message{
			MessageID: id, Ref: backend.RemoteRef{Folder: "ALL", ID: ref},
			Raw: rawMessage(id, "a@example.com", testAccount, "Hello", "body"),
		}
	}
	fake := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
		"ALL": {
			{Messages: []backend.Message{message("keep@example.com", "r1"), message("stale@example.com", "r2")}, Cursor: backend.Cursor("c1")},
			{Messages: []backend.Message{message("keep@example.com", "r1")}, Cursor: backend.Cursor("replacement-page-1"), HasMore: true, FullSnapshot: true, Present: []backend.RemoteRef{{Folder: "ALL", ID: "r1"}}},
			{Messages: []backend.Message{message("new@example.com", "r3")}, Cursor: backend.Cursor("c2"), FullSnapshot: true, Present: []backend.RemoteRef{{Folder: "ALL", ID: "r3"}}},
		},
	})
	cursors := newMemCursorStore()
	engine := New(Options{
		Store: db, Cursors: cursors, Account: testAccount, MaxPerFolder: 1,
		Ingest: IngestOptions{Account: testAccount},
	})
	if _, err := engine.Sync(context.Background(), fake); err != nil {
		t.Fatal(err)
	}
	res, err := engine.Sync(context.Background(), fake)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 {
		t.Fatalf("Deleted = %d, want 1", res.Deleted)
	}
	if stale, _ := db.GetByMessageID("stale@example.com"); stale != nil {
		t.Fatalf("stale message still present: %#v", stale)
	}
	if keep, _ := db.GetByMessageID("keep@example.com"); keep == nil {
		t.Fatal("present message was removed")
	}
	if added, _ := db.GetByMessageID("new@example.com"); added == nil {
		t.Fatal("second replacement page was skipped by the normal message cap")
	}
	if got := string(cursors.cursors[cursors.key(testAccount, "ALL")]); got != "c2" {
		t.Fatalf("persisted cursor = %q, want final replacement cursor", got)
	}
}

func TestEngineHydratesLocallyMissingSnapshotMessages(t *testing.T) {
	db := newTestDB(t)
	folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
	message := func(id, ref string) backend.Message {
		return backend.Message{MessageID: id, Ref: backend.RemoteRef{Folder: "ALL", ID: ref}, Raw: rawMessage(id, "a@example.com", testAccount, "Hello", "body")}
	}
	fake := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
		"ALL": {
			{Messages: []backend.Message{message("keep@example.com", "r1")}, Cursor: backend.Cursor("old")},
			{Cursor: backend.Cursor("new"), FullSnapshot: true, Present: []backend.RemoteRef{{Folder: "ALL", ID: "r1"}, {Folder: "ALL", ID: "r2"}}},
		},
	})
	fake.snapshotMessages = map[string]backend.Message{"r2": message("missed@example.com", "r2")}
	cursors := newMemCursorStore()
	engine := newTestEngine(db, cursors)
	if _, err := engine.Sync(t.Context(), fake); err != nil {
		t.Fatal(err)
	}
	result, err := engine.Sync(t.Context(), fake)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("replacement sync result=%+v err=%v", result, err)
	}
	if len(fake.snapshotHydrated) != 1 || fake.snapshotHydrated[0].ID != "r2" {
		t.Fatalf("hydrated refs = %+v, want only r2", fake.snapshotHydrated)
	}
	if got, _ := db.GetByMessageID("missed@example.com"); got == nil {
		t.Fatal("locally missing snapshot message was not ingested")
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "new" {
		t.Fatalf("cursor = %q, want new", got)
	}
}

func TestEngineCompletesSnapshotWhenListedMessagesDisappearDuringHydration(t *testing.T) {
	db := newTestDB(t)
	folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
	seed := backend.Message{
		MessageID: "existing@example.com", Ref: backend.RemoteRef{Folder: "ALL", ID: "r1"},
		Raw: rawMessage("existing@example.com", "a@example.com", testAccount, "Existing", "body"),
	}
	fake := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
		"ALL": {
			{Messages: []backend.Message{seed}, Cursor: backend.Cursor("old")},
			{Cursor: backend.Cursor("new"), FullSnapshot: true, Present: []backend.RemoteRef{{Folder: "ALL", ID: "r1"}, {Folder: "ALL", ID: "r2"}}},
		},
	})
	fake.metadataMissing = map[string]bool{"r1": true}
	fake.snapshotMissing = map[string]bool{"r2": true}
	cursors := newMemCursorStore()
	engine := newTestEngine(db, cursors)
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("seed sync result=%+v err=%v", result, err)
	}
	result, err := engine.Sync(t.Context(), fake)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("replacement sync result=%+v err=%v", result, err)
	}
	if result.Deleted != 1 {
		t.Fatalf("Deleted = %d, want existing ref removed after provider reported it gone", result.Deleted)
	}
	if got, _ := db.GetByMessageID("existing@example.com"); got != nil {
		t.Fatalf("disappeared existing message still present: %#v", got)
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "new" {
		t.Fatalf("cursor = %q, want replacement cursor new", got)
	}
}

func TestEngineRefreshesExistingSnapshotMetadata(t *testing.T) {
	db := newTestDB(t)
	folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
	seed := backend.Message{
		MessageID: "existing@example.com",
		Ref:       backend.RemoteRef{Folder: "ALL", ID: "r1"},
		Raw:       rawMessage("existing@example.com", "a@example.com", testAccount, "Hello", "body"),
		Flags:     backend.Flags{Seen: true},
		Labels:    []string{"old-label"},
	}
	fake := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
		"ALL": {
			{Messages: []backend.Message{seed}, Cursor: backend.Cursor("old")},
			{Cursor: backend.Cursor("new"), FullSnapshot: true, Present: []backend.RemoteRef{{Folder: "ALL", ID: "r1"}}},
			{Cursor: backend.Cursor("new"), FullSnapshot: true, Present: []backend.RemoteRef{{Folder: "ALL", ID: "r1"}}},
		},
	})
	fake.caps.LabelsAreTags = true
	fake.caps.FlagChangesInDelta = true
	fake.flagsByRef["r1"] = backend.Flags{}
	fake.snapshotMetadata = map[string]backend.Message{
		"r1": {Ref: backend.RemoteRef{Folder: "ALL", ID: "r1"}, Labels: []string{"new-label"}},
	}
	cursors := newMemCursorStore()
	engine := newTestEngine(db, cursors)
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("seed sync result=%+v err=%v", result, err)
	}
	if err := db.ModifyTagsByMessageIDAndAccount("existing@example.com", testAccount, []string{"local"}, nil); err != nil {
		t.Fatal(err)
	}
	fake.fetchFlagsErr = errors.New("temporary flag fetch failure")
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) == 0 {
		t.Fatalf("failed replacement sync result=%+v err=%v", result, err)
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "old" {
		t.Fatalf("cursor after failed flag reconciliation = %q, want old", got)
	}
	fake.fetchFlagsErr = nil
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("replacement sync result=%+v err=%v", result, err)
	}
	if got := fake.metadataFetched; len(got) != 2 || got[0].ID != "r1" || got[1].ID != "r1" {
		t.Fatalf("metadata refs = %+v, want r1 retried", got)
	}
	rows, err := db.GetFolderFlagState(testAccount, "ALL")
	if err != nil || len(rows) != 1 {
		t.Fatalf("flag state = %+v, err=%v", rows, err)
	}
	for _, want := range []string{"new-label", "local", "unread"} {
		if !slices.Contains(rows[0].Tags, want) {
			t.Errorf("tags = %v, missing %q", rows[0].Tags, want)
		}
	}
	if slices.Contains(rows[0].Tags, "old-label") {
		t.Errorf("tags = %v, stale label remains", rows[0].Tags)
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "new" {
		t.Fatalf("cursor = %q, want new", got)
	}
}

func TestEngineAdvancesReplacementAfterRepeatedFlagFetchFailure(t *testing.T) {
	db := newTestDB(t)
	folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
	message := backend.Message{
		MessageID: "pending@example.com", Ref: backend.RemoteRef{Folder: "ALL", ID: "r1"},
		Raw: rawMessage("pending@example.com", "a@example.com", testAccount, "Pending", "body"),
	}
	fake := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
		"ALL": {
			{Messages: []backend.Message{message}, Cursor: backend.Cursor("old")},
			{Cursor: backend.Cursor("replacement-1"), FullSnapshot: true, Present: []backend.RemoteRef{message.Ref}},
			{Cursor: backend.Cursor("replacement-2"), FullSnapshot: true, Present: []backend.RemoteRef{message.Ref}},
			{Cursor: backend.Cursor("delta-1")},
			{Cursor: backend.Cursor("delta-2")},
		},
	})
	fake.flagsByRef["r1"] = backend.Flags{}
	cursors := newMemCursorStore()
	engine := newTestEngine(db, cursors)
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("seed result=%+v err=%v", result, err)
	}
	fake.fetchFlagsErr = errors.New("permanent flag endpoint failure")
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) == 0 {
		t.Fatalf("first failed replacement result=%+v err=%v", result, err)
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "old" {
		t.Fatalf("cursor after first failure = %q, want old", got)
	}
	pending, _ := cursors.GetPendingFlags(testAccount, "ALL")
	if !slices.Equal(pending.Refs, []string{"r1"}) || pending.ReplayCount != 1 {
		t.Fatalf("pending after first failure = %+v", pending)
	}
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) == 0 {
		t.Fatalf("second failed replacement result=%+v err=%v", result, err)
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "replacement-2" {
		t.Fatalf("cursor after repeated failure = %q, want replacement-2", got)
	}
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) == 0 {
		t.Fatalf("pending retry result=%+v err=%v", result, err)
	}
	if got := fake.seenCursors["ALL"]; len(got) != 4 || string(got[1]) != "old" || string(got[2]) != "old" || string(got[3]) != "replacement-2" {
		t.Fatalf("FetchMessages cursors = %q, want one replay then progress", got)
	}
	fake.fetchFlagsErr = nil
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("successful pending retry result=%+v err=%v", result, err)
	}
	pending, _ = cursors.GetPendingFlags(testAccount, "ALL")
	if len(pending.Refs) != 0 || pending.ReplayCount != 0 {
		t.Fatalf("pending flags were not cleared: %+v", pending)
	}
}

func TestEngineCursorDrivenReplacementEpisodesSurviveUploadOnlyPasses(t *testing.T) {
	db := newTestDB(t)
	folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
	message := backend.Message{
		MessageID: "cursor-driven@example.com", Ref: backend.RemoteRef{Folder: "ALL", ID: "r1"},
		Raw: rawMessage("cursor-driven@example.com", "a@example.com", testAccount, "Cursor", "body"),
	}
	fake := newFakeBackend([]backend.Folder{folder}, nil)
	fake.caps.FlagChangesInDelta = true
	fake.flagsByRef["r1"] = backend.Flags{}
	replacements := 0
	fake.fetchByCursor = func(_ string, cursor backend.Cursor) backend.FetchResult {
		switch string(cursor) {
		case "":
			return backend.FetchResult{Messages: []backend.Message{message}, Cursor: backend.Cursor("steady-0")}
		case "steady-0", "replacement-2":
			replacements++
			return backend.FetchResult{
				Cursor: backend.Cursor(fmt.Sprintf("replacement-%d", replacements)), FullSnapshot: true,
				Present: []backend.RemoteRef{message.Ref},
			}
		case "replacement-4":
			return backend.FetchResult{Cursor: backend.Cursor("steady-5")}
		default:
			return backend.FetchResult{Cursor: cursor}
		}
	}

	cursors := newMemCursorStore()
	download := newTestEngine(db, cursors)
	upload := New(Options{
		Store: db, Cursors: cursors, Account: testAccount, Mode: UploadOnly,
		Ingest: IngestOptions{Account: testAccount},
	})
	if result, err := download.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("seed result=%+v err=%v", result, err)
	}
	fake.fetchFlagsErr = errors.New("flag endpoint unavailable")

	for episode := 0; episode < 2; episode++ {
		if result, err := download.Sync(t.Context(), fake); err != nil || len(result.Errors) == 0 {
			t.Fatalf("episode %d first replacement result=%+v err=%v", episode, result, err)
		}
		state, _ := cursors.GetState(testAccount, "ALL")
		if state.PendingFlags.ReplayCount != 1 || !slices.Equal(state.PendingFlags.Refs, []string{"r1"}) {
			t.Fatalf("episode %d held state = %+v", episode, state)
		}

		// The watcher runs upload-only after local mutations. Neither the
		// mutation nor this unrelated pass may reset the replacement replay.
		add, remove := []string{"flagged"}, []string(nil)
		if episode == 1 {
			add, remove = nil, []string{"flagged"}
		}
		if err := db.ModifyTagsByMessageIDAndAccount(message.MessageID, testAccount, add, remove); err != nil {
			t.Fatal(err)
		}
		if result, err := upload.Sync(t.Context(), fake); err != nil || len(result.Errors) == 0 {
			t.Fatalf("episode %d upload-only result=%+v err=%v", episode, result, err)
		}
		state, _ = cursors.GetState(testAccount, "ALL")
		if state.PendingFlags.ReplayCount != 1 || len(state.PendingFlags.Refs) != 1 {
			t.Fatalf("episode %d state after upload-only = %+v", episode, state)
		}

		if result, err := download.Sync(t.Context(), fake); err != nil || len(result.Errors) == 0 {
			t.Fatalf("episode %d replay result=%+v err=%v", episode, result, err)
		}
	}

	fake.fetchFlagsErr = nil
	if result, err := download.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("recovery result=%+v err=%v", result, err)
	}
	state, _ := cursors.GetState(testAccount, "ALL")
	if string(state.Cursor) != "steady-5" || len(state.PendingFlags.Refs) != 0 || state.PendingFlags.ReplayCount != 0 {
		t.Fatalf("recovered state = %+v", state)
	}
	wantCursors := []backend.Cursor{
		backend.Cursor(""), backend.Cursor("steady-0"), backend.Cursor("steady-0"),
		backend.Cursor("replacement-2"), backend.Cursor("replacement-2"), backend.Cursor("replacement-4"),
	}
	if got := fake.seenCursors["ALL"]; !slices.EqualFunc(got, wantCursors, func(a, b backend.Cursor) bool { return bytes.Equal(a, b) }) {
		t.Fatalf("FetchMessages cursors = %q, want %q", got, wantCursors)
	}
	for _, count := range cursors.serializedRefCounts {
		if count > 1 {
			t.Fatalf("duplicate pending queue growth: serialized ref counts = %v", cursors.serializedRefCounts)
		}
	}
}

func TestEngineAdvancesFinalDeltaWhenFlagFetchFails(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	message := backend.Message{
		MessageID: "delta-replay@example.com", Ref: backend.RemoteRef{Folder: "INBOX", ID: "m1"},
		Raw:   rawMessage("delta-replay@example.com", "a@example.com", testAccount, "Replay", "body"),
		Flags: backend.Flags{Flagged: true},
	}
	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Role: backend.RoleInbox, Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {
			{Messages: []backend.Message{message}, Cursor: backend.Cursor("c1")},
			{Messages: []backend.Message{message}, Cursor: backend.Cursor("c2")},
			{Messages: []backend.Message{message}, Cursor: backend.Cursor("c3")},
		},
	})
	fake.caps.FlagChangesInDelta = true
	fake.flagsByRef["m1"] = message.Flags
	engine := newTestEngine(db, cursors)
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("seed result=%+v err=%v", result, err)
	}
	fake.fetchFlagsErr = errors.New("permanent Graph batch failure")
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) == 0 {
		t.Fatalf("failed delta result=%+v err=%v", result, err)
	}
	if got, _ := cursors.Get(testAccount, "INBOX"); string(got) != "c2" {
		t.Fatalf("cursor after failed flag fetch = %q, want c2", got)
	}
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) == 0 {
		t.Fatalf("next failed delta result=%+v err=%v", result, err)
	}
	if got, _ := cursors.Get(testAccount, "INBOX"); string(got) != "c3" {
		t.Fatalf("cursor after repeated flag failure = %q, want c3", got)
	}
	if got := fake.seenCursors["INBOX"]; len(got) != 3 || string(got[1]) != "c1" || string(got[2]) != "c2" {
		t.Fatalf("FetchMessages cursors = %q, want forward progress despite flag failures", got)
	}
}

func TestEngineDeltaCommitPreservesReplacementReplay(t *testing.T) {
	db := newTestDB(t)
	message := backend.Message{
		MessageID: "delta-interleave@example.com", Ref: backend.RemoteRef{Folder: "INBOX", ID: "m1"},
		Raw: rawMessage("delta-interleave@example.com", "a@example.com", testAccount, "Delta", "body"),
	}
	if _, _, err := Ingest(db, message, "INBOX", backend.RoleInbox, IngestOptions{Account: testAccount}); err != nil {
		t.Fatal(err)
	}
	cursors := newMemCursorStore()
	key := cursors.key(testAccount, "INBOX")
	cursors.cursors[key] = backend.Cursor("c1")
	cursors.pending[key] = PendingFlags{Refs: []string{"m1"}, ReplayCount: 1}
	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Role: backend.RoleInbox, Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {{Messages: []backend.Message{message}, Cursor: backend.Cursor("c2")}},
	})
	fake.caps.FlagChangesInDelta = true
	fake.fetchFlagsErr = errors.New("flag endpoint unavailable")

	result, err := newTestEngine(db, cursors).Sync(t.Context(), fake)
	if err != nil || len(result.Errors) == 0 {
		t.Fatalf("delta result=%+v err=%v", result, err)
	}
	state, _ := cursors.GetState(testAccount, "INBOX")
	if string(state.Cursor) != "c2" || state.PendingFlags.ReplayCount != 1 || !slices.Equal(state.PendingFlags.Refs, []string{"m1"}) {
		t.Fatalf("state after unrelated delta = %+v", state)
	}
}

func TestEngineCheckpointsMultiPageDeltaWithoutUnboundedPendingRefs(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	folder := backend.Folder{Name: "INBOX", Role: backend.RoleInbox, Selectable: true}
	message := func(n int) backend.Message {
		id := fmt.Sprintf("page-%d@example.com", n)
		ref := fmt.Sprintf("ref-%d", n)
		return backend.Message{
			MessageID: id, Ref: backend.RemoteRef{Folder: "INBOX", ID: ref},
			Raw: rawMessage(id, "a@example.com", testAccount, "Checkpoint", "body"),
		}
	}
	fake := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
		"INBOX": {
			{Messages: []backend.Message{message(1)}, Cursor: backend.Cursor("page-1"), HasMore: true},
			{Messages: []backend.Message{message(2)}, Cursor: backend.Cursor("page-2"), HasMore: true},
			{Messages: []backend.Message{message(3)}, Cursor: backend.Cursor("final")},
		},
	})
	fake.caps.FlagChangesInDelta = true
	for n := 1; n <= 3; n++ {
		fake.flagsByRef[fmt.Sprintf("ref-%d", n)] = backend.Flags{}
	}
	engine := newTestEngine(db, cursors)
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("multi-page sync result=%+v err=%v", result, err)
	}
	if got, want := cursors.stateReads, 1; got != want {
		t.Fatalf("state reads = %d, want %d", got, want)
	}
	if got, want := cursors.commits, 3; got != want {
		t.Fatalf("commits = %d, want %d", got, want)
	}
	if got, want := cursors.serializedRefCounts, []int{1, 2, 0}; !slices.Equal(got, want) {
		t.Fatalf("serialized pending refs per commit = %v, want %v", got, want)
	}

	// A quiet pass and an upload-only pass with no local mutations read the
	// state once each but do not rewrite an unchanged cursor file.
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("quiet sync result=%+v err=%v", result, err)
	}
	upload := New(Options{
		Store: db, Cursors: cursors, Account: testAccount, Mode: UploadOnly,
		Ingest: IngestOptions{Account: testAccount},
	})
	if result, err := upload.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("upload-only sync result=%+v err=%v", result, err)
	}
	if got, want := cursors.stateReads, 3; got != want {
		t.Fatalf("state reads after quiet passes = %d, want %d", got, want)
	}
	if got, want := cursors.commits, 3; got != want {
		t.Fatalf("commits after quiet passes = %d, want %d", got, want)
	}
}

func TestEngineCheckpointsOrdinaryPageBeforeLaterFailure(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	folder := backend.Folder{Name: "INBOX", Role: backend.RoleInbox, Selectable: true}
	message := backend.Message{
		MessageID: "checkpoint@example.com",
		Ref:       backend.RemoteRef{Folder: "INBOX", ID: "r1"},
		Raw:       rawMessage("checkpoint@example.com", "a@example.com", testAccount, "Checkpoint", "body"),
	}
	fake := newFakeBackend([]backend.Folder{folder}, nil)
	fake.caps.FlagChangesInDelta = true
	fake.flagsByRef["r1"] = backend.Flags{}
	fake.fetchErrByCursor = map[string]error{"page-1": errors.New("page 2 unavailable")}
	fake.fetchByCursor = func(_ string, cursor backend.Cursor) backend.FetchResult {
		if len(cursor) == 0 {
			return backend.FetchResult{Messages: []backend.Message{message}, Cursor: backend.Cursor("page-1"), HasMore: true}
		}
		return backend.FetchResult{Cursor: backend.Cursor("final")}
	}
	engine := newTestEngine(db, cursors)

	result, err := engine.Sync(t.Context(), fake)
	if err != nil || len(result.Errors) == 0 {
		t.Fatalf("first sync result=%+v err=%v, want later-page error", result, err)
	}
	state, _ := cursors.GetState(testAccount, "INBOX")
	if string(state.Cursor) != "page-1" || !slices.Equal(state.PendingFlags.Refs, []string{"r1"}) {
		t.Fatalf("state after later-page failure = %+v", state)
	}
	if err := db.ModifyTagsByMessageIDAndAccount(message.MessageID, testAccount, nil, []string{"unread", "inbox"}); err != nil {
		t.Fatal(err)
	}

	result, err = engine.Sync(t.Context(), fake)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("retry result=%+v err=%v", result, err)
	}
	if got := fake.seenCursors["INBOX"]; len(got) != 3 || string(got[2]) != "page-1" {
		t.Fatalf("FetchMessages cursors = %q, want retry from page-1", got)
	}
	rows, err := db.GetFolderFlagState(testAccount, "INBOX")
	if err != nil || len(rows) != 1 {
		t.Fatalf("flag rows = %+v, %v", rows, err)
	}
	if tagsContain(rows[0].Tags, "unread") || tagsContain(rows[0].Tags, "inbox") {
		t.Fatalf("replay restored local read/archive tags: %v", rows[0].Tags)
	}
	if len(fake.applyFlagsCalls) != 1 || !fake.applyFlagsCalls[0].add.Seen {
		t.Fatalf("read mutation upload calls = %+v", fake.applyFlagsCalls)
	}
}

func TestPendingFlagOverflowUsesBoundedFullScan(t *testing.T) {
	refs := make([]string, 200_000)
	for i := range refs {
		refs[i] = fmt.Sprintf("provider-ref-%06d", i)
	}
	pending := pendingFlagsFromRefs(refs)
	if !pending.FullScan || len(pending.Refs) != 0 {
		t.Fatalf("200k pending refs = %+v, want compact full-scan marker", pending)
	}
	encoded, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 64 {
		t.Fatalf("full-scan marker JSON = %d bytes, want <= 64: %s", len(encoded), encoded)
	}

	oversized := pendingFlagsFromRefs([]string{strings.Repeat("x", maxPendingFlagRefBytes+1)})
	if !oversized.FullScan || len(oversized.Refs) != 0 {
		t.Fatalf("oversized ref did not use full-scan marker: %+v", oversized)
	}
}

func TestEngineFullScanReconciliationUsesBoundedFetchBatches(t *testing.T) {
	db := newTestDB(t)
	const rowCount = maxPendingFlagRefs + 17
	deltaFlags := make(map[string]backend.Flags, rowCount)
	for i := 0; i < rowCount; i++ {
		id := fmt.Sprintf("batch-%04d@example.com", i)
		ref := fmt.Sprintf("ref-%04d", i)
		if err := db.InsertMessage(&store.Message{
			MessageID: id, Subject: "batch", Date: 1, CreatedAt: int64(i + 1),
			Mailbox: "ALL", Account: testAccount, RemoteRef: ref,
			Flags: `\Seen`, SyncedFlags: `\Seen`, FetchedBody: true,
		}); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
		deltaFlags[ref] = backend.Flags{Seen: true}
	}

	folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
	fake := newFakeBackend([]backend.Folder{folder}, nil)
	fake.caps.FlagChangesInDelta = true
	fake.fetchFlagsErr = errors.New("systemic flag outage")
	engine := newTestEngine(db, newMemCursorStore())
	failed := engine.reconcileFolderFlags(t.Context(), fake, folder, deltaFlags, PendingFlags{}, &Result{})
	if !failed.fetchFailed || !failed.pendingFlags.FullScan || len(failed.pendingFlags.Refs) > maxPendingFlagRefs {
		t.Fatalf("failed reconciliation = %+v, want bounded full scan", failed)
	}
	if len(fake.fetchFlagsCalls) != 1 || len(fake.fetchFlagsCalls[0].refs) != flagFetchBatchSize {
		t.Fatalf("outage fetch calls = %+v, want one %d-ref request", fake.fetchFlagsCalls, flagFetchBatchSize)
	}

	fake.fetchFlagsErr = nil
	fake.fetchFlagsCalls = nil
	for ref, flags := range deltaFlags {
		fake.flagsByRef[ref] = flags
	}
	progressed := engine.reconcileFolderFlags(t.Context(), fake, folder, nil, failed.pendingFlags, &Result{})
	if progressed.reconciled || progressed.failed() || !progressed.pendingFlags.FullScan || progressed.pendingFlags.ScanAfterID == 0 {
		t.Fatalf("first recovery = %+v, want persisted full-scan progress", progressed)
	}
	wantCalls := maxFullScanRowsPerSync / flagFetchBatchSize
	if len(fake.fetchFlagsCalls) != wantCalls {
		t.Fatalf("first recovery fetch calls = %d, want %d", len(fake.fetchFlagsCalls), wantCalls)
	}
	for i, call := range fake.fetchFlagsCalls {
		if len(call.refs) > flagFetchBatchSize {
			t.Fatalf("first recovery call %d has %d refs, limit %d", i, len(call.refs), flagFetchBatchSize)
		}
	}

	fake.fetchFlagsCalls = nil
	recovered := engine.reconcileFolderFlags(t.Context(), fake, folder, nil, progressed.pendingFlags, &Result{})
	if !recovered.reconciled || recovered.failed() || recovered.pendingFlags.FullScan || len(recovered.pendingFlags.Refs) != 0 {
		t.Fatalf("second recovery = %+v, want completed reconciliation", recovered)
	}
	if len(fake.fetchFlagsCalls) != 1 || len(fake.fetchFlagsCalls[0].refs) != rowCount-maxFullScanRowsPerSync {
		t.Fatalf("second recovery fetch calls = %+v, want one %d-ref request", fake.fetchFlagsCalls, rowCount-maxFullScanRowsPerSync)
	}
}

func TestEngineRejectsSnapshotToDeltaTransition(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	_ = cursors.Set(testAccount, "ALL", backend.Cursor("old"))
	fake := newFakeBackend([]backend.Folder{{Name: "ALL", Selectable: true}}, map[string][]backend.FetchResult{
		"ALL": {
			{Cursor: backend.Cursor("snapshot"), HasMore: true, FullSnapshot: true},
			{Cursor: backend.Cursor("delta"), FullSnapshot: false},
		},
	})
	result, err := newTestEngine(db, cursors).Sync(t.Context(), fake)
	if err != nil {
		t.Fatalf("Sync returned top-level error: %v", err)
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0].Error(), "replacement-snapshot back to delta") {
		t.Fatalf("Sync errors = %v", result.Errors)
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "old" {
		t.Fatalf("cursor advanced to %q", got)
	}
}

func TestEngineAllowsDeltaToReplacementRestart(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	_ = cursors.Set(testAccount, "ALL", backend.Cursor("old"))
	message := func(id, ref string) backend.Message {
		return backend.Message{
			MessageID: id, Ref: backend.RemoteRef{Folder: "ALL", ID: ref},
			Raw: rawMessage(id, "a@example.com", testAccount, "Hello", "body"),
		}
	}
	fake := newFakeBackend([]backend.Folder{{Name: "ALL", Selectable: true}}, map[string][]backend.FetchResult{
		"ALL": {
			{Messages: []backend.Message{message("delta@example.com", "d1")}, Cursor: backend.Cursor("expired-next"), HasMore: true},
			{Messages: []backend.Message{message("snapshot@example.com", "s1")}, Cursor: backend.Cursor("replacement"), FullSnapshot: true, Present: []backend.RemoteRef{{Folder: "ALL", ID: "d1"}, {Folder: "ALL", ID: "s1"}}},
		},
	})
	result, err := newTestEngine(db, cursors).Sync(t.Context(), fake)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("Sync result=%+v err=%v", result, err)
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "replacement" {
		t.Fatalf("cursor = %q, want replacement", got)
	}
	for _, id := range []string{"delta@example.com", "snapshot@example.com"} {
		if got, _ := db.GetByMessageID(id); got == nil {
			t.Fatalf("message %s was not retained", id)
		}
	}
}

func TestEngineUsesLatestOrdinaryCheckpointAsReplacementReplayBase(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	_ = cursors.Set(testAccount, "ALL", backend.Cursor("old"))
	message := backend.Message{
		MessageID: "transition@example.com", Ref: backend.RemoteRef{Folder: "ALL", ID: "r1"},
		Raw: rawMessage("transition@example.com", "a@example.com", testAccount, "Transition", "body"),
	}
	fake := newFakeBackend([]backend.Folder{{Name: "ALL", Selectable: true}}, map[string][]backend.FetchResult{
		"ALL": {
			{Messages: []backend.Message{message}, Cursor: backend.Cursor("ordinary-page"), HasMore: true},
			{Messages: []backend.Message{message}, Present: []backend.RemoteRef{message.Ref}, Cursor: backend.Cursor("replacement"), FullSnapshot: true},
		},
	})
	fake.caps.FlagChangesInDelta = true
	fake.fetchFlagsErr = errors.New("flag endpoint unavailable")

	result, err := newTestEngine(db, cursors).Sync(t.Context(), fake)
	if err != nil || len(result.Errors) == 0 {
		t.Fatalf("Sync result=%+v err=%v, want flag error", result, err)
	}
	state, _ := cursors.GetState(testAccount, "ALL")
	if string(state.Cursor) != "ordinary-page" || state.PendingFlags.ReplayCount != 1 {
		t.Fatalf("replacement replay base = %+v, want ordinary-page with replay count 1", state)
	}
}

func TestEngineDryRunReplacementDoesNotRequireHydrator(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	_ = cursors.Set(testAccount, "INBOX", backend.Cursor("old"))
	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {{
			Cursor: backend.Cursor("replacement"), FullSnapshot: true,
			Present: []backend.RemoteRef{{Folder: "INBOX", ID: "1"}, {Folder: "INBOX", ID: "2"}},
		}},
	})
	engine := New(Options{
		Store: db, Cursors: cursors, Account: testAccount, DryRun: true,
		Ingest: IngestOptions{Account: testAccount},
	})
	result, err := engine.Sync(t.Context(), &backendOnly{Backend: fake})
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("Sync result=%+v err=%v", result, err)
	}
	if result.New != 2 {
		t.Fatalf("New = %d, want 2 projected arrivals", result.New)
	}
	if got, _ := cursors.Get(testAccount, "INBOX"); string(got) != "old" {
		t.Fatalf("dry-run cursor = %q, want old", got)
	}
}

func TestEngineReplacementSkipsAbsentRefWithoutHydrator(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {{
			Cursor: backend.Cursor("replacement"), FullSnapshot: true,
			Present: []backend.RemoteRef{{Folder: "INBOX", ID: "collapsed-or-expunged"}},
		}},
	})
	result, err := newTestEngine(db, cursors).Sync(t.Context(), &backendOnly{Backend: fake})
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("Sync result=%+v err=%v", result, err)
	}
	if got, _ := cursors.Get(testAccount, "INBOX"); string(got) != "replacement" {
		t.Fatalf("cursor = %q, want replacement", got)
	}
}

func TestEngineReplacementToleratesDuplicateMessageIDRefs(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	message := func(ref string) backend.Message {
		return backend.Message{
			MessageID: "duplicate@example.com", Ref: backend.RemoteRef{Folder: "INBOX", ID: ref},
			Raw: rawMessage("duplicate@example.com", "a@example.com", testAccount, "Duplicate", "body"),
		}
	}
	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {{
			Messages: []backend.Message{message("r1"), message("r2")}, Cursor: backend.Cursor("replacement"), FullSnapshot: true,
			Present: []backend.RemoteRef{{Folder: "INBOX", ID: "r1"}, {Folder: "INBOX", ID: "r2"}},
		}},
	})
	result, err := newTestEngine(db, cursors).Sync(t.Context(), &backendOnly{Backend: fake})
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("Sync result=%+v err=%v", result, err)
	}
	if got, _ := cursors.Get(testAccount, "INBOX"); string(got) != "replacement" {
		t.Fatalf("cursor = %q, want replacement", got)
	}
}

func TestEngineReplacementRemovesRefExpungedOnLaterPage(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	message := backend.Message{
		MessageID: "expunged@example.com", Ref: backend.RemoteRef{Folder: "INBOX", ID: "gone"},
		Raw: rawMessage("expunged@example.com", "a@example.com", testAccount, "Gone", "body"),
	}
	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {
			{
				Messages: []backend.Message{message}, Cursor: backend.Cursor("page-1"), HasMore: true, FullSnapshot: true,
				Present: []backend.RemoteRef{message.Ref},
			},
			{
				Cursor: backend.Cursor("replacement"), FullSnapshot: true,
				Deleted: []backend.Deletion{{Ref: message.Ref, MessageID: message.MessageID}},
			},
		},
	})
	result, err := newTestEngine(db, cursors).Sync(t.Context(), &backendOnly{Backend: fake})
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("Sync result=%+v err=%v", result, err)
	}
	if got, _ := db.GetByMessageID(message.MessageID); got != nil {
		t.Fatalf("expunged message remains: %#v", got)
	}
	if got, _ := cursors.Get(testAccount, "INBOX"); string(got) != "replacement" {
		t.Fatalf("cursor = %q, want replacement", got)
	}
}

func TestEngineDryRunFullBodyReplacementCountsEachArrivalOnce(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	message := backend.Message{
		MessageID: "projected@example.com", Ref: backend.RemoteRef{Folder: "INBOX", ID: "1"},
		Raw: rawMessage("projected@example.com", "a@example.com", testAccount, "Projected", "body"),
	}
	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {{
			Messages: []backend.Message{message}, Cursor: backend.Cursor("replacement"), FullSnapshot: true,
			Present: []backend.RemoteRef{message.Ref},
		}},
	})
	engine := New(Options{
		Store: db, Cursors: cursors, Account: testAccount, DryRun: true,
		Ingest: IngestOptions{Account: testAccount},
	})
	result, err := engine.Sync(t.Context(), &backendOnly{Backend: fake})
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("Sync result=%+v err=%v", result, err)
	}
	if result.New != 1 {
		t.Fatalf("New = %d, want one projected arrival", result.New)
	}
}

func TestEngineReplacementSkipsPermanentIngestFailureWithoutHydrator(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {{
			Messages: []backend.Message{{
				MessageID: "broken@example.com", Ref: backend.RemoteRef{Folder: "INBOX", ID: "broken"},
				Raw: []byte("malformed header line\r\n\r\nbody"),
			}},
			Cursor: backend.Cursor("replacement"), FullSnapshot: true,
			Present: []backend.RemoteRef{{Folder: "INBOX", ID: "broken"}},
		}},
	})
	engine := newTestEngine(db, cursors)
	result, err := engine.Sync(t.Context(), &backendOnly{Backend: fake})
	if err != nil {
		t.Fatalf("Sync error = %v", err)
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0].Error(), "parse message") {
		t.Fatalf("Sync errors = %v, want one permanent parse failure", result.Errors)
	}
	if got, _ := cursors.Get(testAccount, "INBOX"); string(got) != "replacement" {
		t.Fatalf("cursor = %q, want replacement", got)
	}
}

func TestEngineReplacementSkipsPermanentHydratedIngestFailure(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	prior := backend.Message{
		MessageID: "broken@example.com", Ref: backend.RemoteRef{Folder: "INBOX", ID: "prior"},
		Raw: rawMessage("broken@example.com", "a@example.com", testAccount, "Prior", "body"),
	}
	if _, _, err := Ingest(db, prior, "INBOX", backend.RoleNone, IngestOptions{Account: testAccount}); err != nil {
		t.Fatalf("seed prior copy: %v", err)
	}
	ref := backend.RemoteRef{Folder: "INBOX", ID: "broken"}
	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {{
			Cursor: backend.Cursor("replacement"), FullSnapshot: true,
			Present: []backend.RemoteRef{ref},
		}},
	})
	fake.snapshotMessages = map[string]backend.Message{
		ref.ID: {MessageID: "broken@example.com", Ref: ref, Raw: []byte("malformed header line\r\n\r\nbody")},
	}
	result, err := newTestEngine(db, cursors).Sync(t.Context(), fake)
	if err != nil {
		t.Fatalf("Sync error = %v", err)
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0].Error(), "parse message") {
		t.Fatalf("Sync errors = %v, want one permanent hydrated parse failure", result.Errors)
	}
	if got, _ := cursors.Get(testAccount, "INBOX"); string(got) != "replacement" {
		t.Fatalf("cursor = %q, want replacement", got)
	}
	if got, _ := db.GetByMessageID(prior.MessageID); got == nil {
		t.Fatal("older local copy was removed after hydrated body failed permanently")
	}
}

func TestEngineReplacementPreservesExistingRefAfterPermanentIngestFailure(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	valid := backend.Message{
		MessageID: "existing@example.com", Ref: backend.RemoteRef{Folder: "INBOX", ID: "existing"},
		Raw: rawMessage("existing@example.com", "a@example.com", testAccount, "Existing", "body"),
	}
	broken := valid
	broken.Raw = []byte("malformed header line\r\n\r\nbody")
	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {
			{Messages: []backend.Message{valid}, Cursor: backend.Cursor("old")},
			{
				Messages: []backend.Message{broken}, Cursor: backend.Cursor("replacement"), FullSnapshot: true,
				Present: []backend.RemoteRef{{Folder: "INBOX", ID: "existing"}},
			},
		},
	})
	engine := newTestEngine(db, cursors)
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("seed result=%+v err=%v", result, err)
	}
	result, err := engine.Sync(t.Context(), &backendOnly{Backend: fake})
	if err != nil || len(result.Errors) != 1 {
		t.Fatalf("replacement result=%+v err=%v", result, err)
	}
	if got, _ := db.GetByMessageID("existing@example.com"); got == nil {
		t.Fatal("existing local copy was removed after malformed replacement body")
	}
	if got, _ := cursors.Get(testAccount, "INBOX"); string(got) != "replacement" {
		t.Fatalf("cursor = %q, want replacement", got)
	}
}

func TestEngineReplacementPreservesUnavailableExistingRef(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	folder := backend.Folder{Name: "INBOX", Selectable: true}
	seed := backend.Message{
		MessageID: "protected@example.com", Ref: backend.RemoteRef{Folder: "INBOX", ID: "protected"},
		Raw: rawMessage("protected@example.com", "a@example.com", testAccount, "Protected", "body"),
	}
	fake := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
		"INBOX": {
			{Messages: []backend.Message{seed}, Cursor: backend.Cursor("old")},
			{
				Cursor: backend.Cursor("replacement"), FullSnapshot: true,
				Present:     []backend.RemoteRef{{Folder: "INBOX", ID: "protected"}, {Folder: "INBOX", ID: "new-protected"}},
				Unavailable: []backend.RemoteRef{{Folder: "INBOX", ID: "protected"}, {Folder: "INBOX", ID: "new-protected"}},
			},
		},
	})
	engine := newTestEngine(db, cursors)
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("seed result=%+v err=%v", result, err)
	}
	result, err := engine.Sync(t.Context(), &backendOnly{Backend: fake})
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("replacement result=%+v err=%v", result, err)
	}
	if got, _ := db.GetByMessageID("protected@example.com"); got == nil {
		t.Fatal("existing inaccessible message was removed")
	}
	if got, _ := cursors.Get(testAccount, "INBOX"); string(got) != "replacement" {
		t.Fatalf("cursor = %q, want replacement", got)
	}
}

func TestEngineLetsReplacementFinishBeyondOrdinaryTimeout(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	fake := newFakeBackend([]backend.Folder{{Name: "ALL", Selectable: true}}, map[string][]backend.FetchResult{
		"ALL": {
			{Cursor: backend.Cursor("page-1"), HasMore: true, FullSnapshot: true},
			{Cursor: backend.Cursor("final"), FullSnapshot: true},
		},
	})
	delayed := &delayedBackend{Backend: fake, delays: []time.Duration{0, 100 * time.Millisecond}}
	engine := New(Options{
		Store: db, Cursors: cursors, Account: testAccount,
		Timeout: 50 * time.Millisecond, RecoveryTimeout: 500 * time.Millisecond,
		Ingest: IngestOptions{Account: testAccount},
	})
	result, err := engine.Sync(t.Context(), delayed)
	if !errors.Is(err, context.DeadlineExceeded) || len(result.Errors) != 0 {
		t.Fatalf("Sync result=%+v err=%v, want ordinary deadline after completed recovery", result, err)
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "final" {
		t.Fatalf("cursor = %q, want final", got)
	}
}

func TestEngineReplacementFlagDeadlineMakesBoundedProgress(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	message := backend.Message{
		MessageID: "deadline@example.com", Ref: backend.RemoteRef{Folder: "ALL", ID: "r1"},
		Raw: rawMessage("deadline@example.com", "a@example.com", testAccount, "Deadline", "body"),
	}
	if _, _, err := Ingest(db, message, "ALL", backend.RoleAll, IngestOptions{Account: testAccount}); err != nil {
		t.Fatal(err)
	}
	if err := cursors.Set(testAccount, "ALL", backend.Cursor("old")); err != nil {
		t.Fatal(err)
	}
	fake := newFakeBackend([]backend.Folder{{Name: "ALL", Role: backend.RoleAll, Selectable: true}}, nil)
	replacements := 0
	fake.fetchByCursor = func(_ string, _ backend.Cursor) backend.FetchResult {
		replacements++
		return backend.FetchResult{
			Cursor: backend.Cursor(fmt.Sprintf("replacement-%d", replacements)), FullSnapshot: true,
			Messages: []backend.Message{message}, Present: []backend.RemoteRef{message.Ref},
		}
	}
	delayed := &delayedFlagsBackend{Backend: fake, delay: 100 * time.Millisecond}
	engine := New(Options{
		Store: db, Cursors: cursors, Account: testAccount,
		Timeout: 5 * time.Millisecond, RecoveryTimeout: 20 * time.Millisecond,
		Ingest: IngestOptions{Account: testAccount},
	})

	for pass := 0; pass < 2; pass++ {
		result, err := engine.Sync(t.Context(), delayed)
		if !errors.Is(err, context.DeadlineExceeded) || len(result.Errors) == 0 {
			t.Fatalf("pass %d result=%+v err=%v, want recorded flag deadline", pass, result, err)
		}
	}
	state, _ := cursors.GetState(testAccount, "ALL")
	if string(state.Cursor) != "replacement-2" || state.PendingFlags.ReplayCount != 0 || !slices.Equal(state.PendingFlags.Refs, []string{"r1"}) {
		t.Fatalf("state after two deadline passes = %+v", state)
	}
	if got, want := fake.seenCursors["ALL"], []backend.Cursor{backend.Cursor("old"), backend.Cursor("old")}; !slices.EqualFunc(got, want, func(a, b backend.Cursor) bool { return bytes.Equal(a, b) }) {
		t.Fatalf("FetchMessages cursors = %q, want one bounded replay %q", got, want)
	}
}

func TestEngineDoesNotPersistSnapshotCursorWhenReconciliationFails(t *testing.T) {
	db := newTestDB(t)
	message := backend.Message{
		MessageID: "stale@example.com", Ref: backend.RemoteRef{Folder: "ALL", ID: "stale"},
		Raw: rawMessage("stale@example.com", "a@example.com", testAccount, "Stale", "body"),
	}
	cursors := newMemCursorStore()
	fake := newFakeBackend([]backend.Folder{{Name: "ALL", Selectable: true}}, map[string][]backend.FetchResult{
		"ALL": {
			{Messages: []backend.Message{message}, Cursor: backend.Cursor("old")},
			{Cursor: backend.Cursor("final"), FullSnapshot: true},
		},
	})
	engine := newTestEngine(db, cursors)
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("seed sync result=%+v err=%v", result, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := engine.Sync(t.Context(), fake)
	if err != nil {
		t.Fatalf("Sync returned top-level error: %v", err)
	}
	if len(result.Errors) == 0 {
		t.Fatal("Sync unexpectedly succeeded with closed reconciliation store")
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "old" {
		t.Fatalf("cursor advanced to %q after reconciliation failure", got)
	}
}

func TestEnginePersistsReplacementCursorWhenFlagUploadFails(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	message := backend.Message{
		MessageID: "upload-denied@example.com", Ref: backend.RemoteRef{Folder: "INBOX", ID: "m1"},
		Raw:   rawMessage("upload-denied@example.com", "a@example.com", testAccount, "Denied", "body"),
		Flags: backend.Flags{Flagged: true},
	}
	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Role: backend.RoleInbox, Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {
			{Messages: []backend.Message{message}, Cursor: backend.Cursor("old")},
			{Cursor: backend.Cursor("replacement"), FullSnapshot: true, Present: []backend.RemoteRef{message.Ref}},
		},
	})
	fake.flagsByRef["m1"] = backend.Flags{Flagged: true}
	engine := newTestEngine(db, cursors)
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("seed result=%+v err=%v", result, err)
	}
	if err := db.ModifyTagsByMessageIDAndAccount("upload-denied@example.com", testAccount, nil, []string{"unread"}); err != nil {
		t.Fatal(err)
	}
	fake.applyFlagsErr = errors.New("permanent access denied")
	result, err := engine.Sync(t.Context(), &backendOnly{Backend: fake})
	if err != nil || len(result.Errors) != 1 || !strings.Contains(result.Errors[0].Error(), "flag upload") {
		t.Fatalf("replacement result=%+v err=%v", result, err)
	}
	if got, _ := cursors.Get(testAccount, "INBOX"); string(got) != "replacement" {
		t.Fatalf("cursor = %q, want replacement despite durable local upload failure", got)
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
				Flags:     backend.Flags{Seen: true, Flagged: true},
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
	if !slices.Contains(archTags, "flagged") {
		t.Errorf("archive msg tags = %v, want to contain %q", archTags, "flagged")
	}
	if got := strings.Fields(archMsg.Flags); !slices.Contains(got, `\Seen`) || !slices.Contains(got, `\Flagged`) {
		t.Errorf("archive msg flags = %q, want Seen and Flagged to round-trip", archMsg.Flags)
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

func TestEngineMaxPerFolderPersistsProgressWhenFlagFetchFails(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	message := func(id string) backend.Message {
		return backend.Message{
			MessageID: id, Ref: backend.RemoteRef{Folder: "INBOX", ID: id},
			Raw: rawMessage(id, "s@example.com", testAccount, "Capped", "body"),
		}
	}
	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {
			{Messages: []backend.Message{message("cap1@example.com")}, Cursor: backend.Cursor("page1"), HasMore: true},
			{Messages: []backend.Message{message("cap2@example.com")}, Cursor: backend.Cursor("page2"), HasMore: true},
		},
	})
	fake.fetchFlagsErr = errors.New("permanent flag failure")
	engine := New(Options{
		Store: db, Cursors: cursors, Account: testAccount, MaxPerFolder: 2,
		Ingest: IngestOptions{Account: testAccount},
	})
	result, err := engine.Sync(t.Context(), fake)
	if err != nil || len(result.Errors) != 1 || result.New != 2 {
		t.Fatalf("Sync result=%+v err=%v", result, err)
	}
	if got, _ := cursors.Get(testAccount, "INBOX"); string(got) != "page2" {
		t.Fatalf("cursor = %q, want capped progress page2", got)
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

func TestEngineDeltaReadDoesNotBecomeFalseLocalUnread(t *testing.T) {
	tests := []struct {
		name string
		caps backend.Capabilities
	}{
		{name: "Graph", caps: backend.Capabilities{FlagChangesInDelta: true}},
		{name: "Gmail", caps: backend.Capabilities{FlagChangesInDelta: true, LabelsAreTags: true, AnsweredUnsupported: true}},
		{name: "JMAP", caps: backend.Capabilities{FlagChangesInDelta: true, LabelsAreTags: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newTestDB(t)
			cursors := newMemCursorStore()
			folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
			unread := backend.Message{
				MessageID: "remote-read@example.com",
				Ref:       backend.RemoteRef{Folder: folder.Name, ID: "r1"},
				Raw:       rawMessage("remote-read@example.com", "a@example.com", testAccount, "Subject", "body"),
			}
			read := unread
			read.Flags = backend.Flags{Seen: true}
			fake := newFakeBackend([]backend.Folder{folder}, nil)
			fake.fetchByCursor = func(_ string, cursor backend.Cursor) backend.FetchResult {
				switch string(cursor) {
				case "":
					return backend.FetchResult{Messages: []backend.Message{unread}, Cursor: backend.Cursor("c1")}
				case "c1":
					return backend.FetchResult{Messages: []backend.Message{read}, Cursor: backend.Cursor("c2")}
				default:
					return backend.FetchResult{Cursor: cursor}
				}
			}
			fake.caps = tt.caps
			engine := newTestEngine(db, cursors)

			fake.flagsByRef["r1"] = backend.Flags{}
			if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
				t.Fatalf("first pass result=%+v err=%v", result, err)
			}
			rows, err := db.GetFolderFlagState(testAccount, folder.Name)
			if err != nil || len(rows) != 1 || !tagsContain(rows[0].Tags, "unread") || rows[0].SyncedFlags != "" {
				t.Fatalf("first pass rows=%+v err=%v, want unread with logical empty baseline", rows, err)
			}

			fake.flagsByRef["r1"] = backend.Flags{Seen: true}
			if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
				t.Fatalf("second pass result=%+v err=%v", result, err)
			}
			rows, err = db.GetFolderFlagState(testAccount, folder.Name)
			if err != nil || len(rows) != 1 {
				t.Fatalf("second pass rows=%+v err=%v", rows, err)
			}
			if tagsContain(rows[0].Tags, "unread") || rows[0].SyncedFlags != `\Seen` {
				t.Fatalf("second pass row=%+v, want read tag state and Seen baseline", rows[0])
			}
			if len(fake.applyFlagsCalls) != 0 {
				t.Fatalf("server read was uploaded as a local unread mutation: %+v", fake.applyFlagsCalls)
			}

			if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
				t.Fatalf("quiet pass result=%+v err=%v", result, err)
			}
			if len(fake.applyFlagsCalls) != 0 {
				t.Fatalf("quiet pass did not converge: %+v", fake.applyFlagsCalls)
			}
		})
	}
}

func TestEngineLegacyDeltaTransitionsUseCapturedBeforeImage(t *testing.T) {
	tests := []struct {
		name         string
		storedFlags  string
		localRead    bool
		serverFlags  backend.Flags
		wantUnread   bool
		wantBaseline string
		wantUpload   backend.Flags
	}{
		{
			name:        "server marks legacy read message unread",
			storedFlags: `\Seen`, serverFlags: backend.Flags{},
			wantUnread: true, wantBaseline: "",
		},
		{
			name:      "local read survives legacy unread redelivery",
			localRead: true, serverFlags: backend.Flags{},
			wantBaseline: `\Seen`, wantUpload: backend.Flags{Seen: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newTestDB(t)
			const messageID = "legacy-transition@example.com"
			seed := &store.Message{
				MessageID: messageID, Subject: "legacy", Date: 1, CreatedAt: 1,
				Mailbox: "ALL", Account: testAccount, RemoteRef: "r1",
				Flags: tt.storedFlags, FetchedBody: true,
			}
			if err := db.InsertMessage(seed); err != nil {
				t.Fatal(err)
			}
			if tt.localRead {
				if err := db.AddTag(seed.ID, "unread"); err != nil {
					t.Fatal(err)
				}
				if err := db.ModifyTagsByMessageIDAndAccount(messageID, testAccount, nil, []string{"unread"}); err != nil {
					t.Fatal(err)
				}
			}

			folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
			message := backend.Message{
				MessageID: messageID, Ref: backend.RemoteRef{Folder: folder.Name, ID: "r1"},
				Raw:   rawMessage(messageID, "a@example.com", testAccount, "delta", "body"),
				Flags: tt.serverFlags,
			}
			fake := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
				folder.Name: {{Messages: []backend.Message{message}, Cursor: backend.Cursor("c1")}},
			})
			fake.caps = backend.Capabilities{FlagChangesInDelta: true, LabelsAreTags: true, AnsweredUnsupported: true}
			fake.flagsByRef["r1"] = tt.serverFlags
			result, err := newTestEngine(db, newMemCursorStore()).Sync(t.Context(), fake)
			if err != nil || len(result.Errors) != 0 {
				t.Fatalf("sync result=%+v err=%v", result, err)
			}
			rows, err := db.GetFolderFlagState(testAccount, folder.Name)
			if err != nil || len(rows) != 1 {
				t.Fatalf("rows=%+v err=%v", rows, err)
			}
			if got := tagsContain(rows[0].Tags, "unread"); got != tt.wantUnread {
				t.Errorf("unread=%v, want %v; tags=%v", got, tt.wantUnread, rows[0].Tags)
			}
			if rows[0].SyncedFlags != tt.wantBaseline || !rows[0].SyncedFlagsInitialized {
				t.Errorf("baseline=%q initialized=%v, want %q initialized", rows[0].SyncedFlags, rows[0].SyncedFlagsInitialized, tt.wantBaseline)
			}
			if tt.wantUpload == (backend.Flags{}) {
				if len(fake.applyFlagsCalls) != 0 {
					t.Errorf("unexpected uploads: %+v", fake.applyFlagsCalls)
				}
			} else if len(fake.applyFlagsCalls) != 1 || fake.applyFlagsCalls[0].add != tt.wantUpload {
				t.Errorf("uploads=%+v, want add=%+v", fake.applyFlagsCalls, tt.wantUpload)
			}
		})
	}
}

func TestEngineDeltaCompoundFlagsReconcileFromExplicitEmptyBaseline(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
	message := backend.Message{
		MessageID: "compound@example.com", Ref: backend.RemoteRef{Folder: folder.Name, ID: "r1"},
		Raw: rawMessage("compound@example.com", "a@example.com", testAccount, "compound", "body"),
	}
	readFlagged := message
	readFlagged.Flags = backend.Flags{Seen: true, Flagged: true}
	fake := newFakeBackend([]backend.Folder{folder}, nil)
	fake.fetchByCursor = func(_ string, cursor backend.Cursor) backend.FetchResult {
		switch string(cursor) {
		case "":
			return backend.FetchResult{Messages: []backend.Message{message}, Cursor: backend.Cursor("c1")}
		case "c1":
			return backend.FetchResult{Messages: []backend.Message{readFlagged}, Cursor: backend.Cursor("c2")}
		default:
			return backend.FetchResult{Cursor: cursor}
		}
	}
	fake.caps.FlagChangesInDelta = true
	fake.flagsByRef["r1"] = backend.Flags{}
	engine := newTestEngine(db, cursors)
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("first pass result=%+v err=%v", result, err)
	}
	fake.flagsByRef["r1"] = readFlagged.Flags
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("second pass result=%+v err=%v", result, err)
	}
	rows, err := db.GetFolderFlagState(testAccount, folder.Name)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if tagsContain(rows[0].Tags, "unread") || !tagsContain(rows[0].Tags, "flagged") || rows[0].SyncedFlags != `\Seen,\Flagged` {
		t.Fatalf("compound reconciliation row=%+v", rows[0])
	}
	if len(fake.applyFlagsCalls) != 0 {
		t.Fatalf("compound server change produced upload: %+v", fake.applyFlagsCalls)
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

func TestEngineSeedsMigratedFlagBaseline(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v25-flags.db")
	kr, err := dbcrypto.NewKeyring(bytes.Repeat([]byte{0x42}, dbcrypto.MasterKeyLen))
	if err != nil {
		t.Fatalf("test keyring: %v", err)
	}
	db, err := store.Open(dbPath, kr)
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	if err := db.Init(); err != nil {
		t.Fatalf("init seed: %v", err)
	}
	if err := db.InsertMessage(&store.Message{
		MessageID: "mig@example.com", Subject: "x", Date: 1, CreatedAt: 1,
		Mailbox: "ALL", Account: testAccount, RemoteRef: "r1",
		SyncedFlags: "", FetchedBody: true,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db.ModifyTagsByMessageIDAndAccount("mig@example.com", testAccount, []string{"flagged"}, nil); err != nil {
		t.Fatalf("seed flag-derived tag: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	// Reproduce the exact v25 corruption from the old insert path: the standard
	// flags remained comma-joined inside encrypted flags_other while both
	// boolean columns and synced_flags stayed false/empty.
	legacyCT, err := kr.EncryptMeta([]byte(`\Seen,\Flagged`))
	if err != nil {
		t.Fatalf("encrypt legacy flags: %v", err)
	}
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw v25 db: %v", err)
	}
	if _, err := rawDB.Exec(`UPDATE messages SET is_seen = 0, is_flagged = 0,
		is_deleted = 0, flags_other = ? WHERE message_id = 'mig@example.com'`, legacyCT); err != nil {
		t.Fatalf("write old on-disk row: %v", err)
	}
	if _, err := rawDB.Exec("UPDATE schema_version SET version = 25 WHERE rowid = 1"); err != nil {
		t.Fatalf("set v25: %v", err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatalf("close raw v25 db: %v", err)
	}

	db, err = store.Open(dbPath, kr)
	if err != nil {
		t.Fatalf("reopen v25 db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Init(); err != nil {
		t.Fatalf("migrate v25 db: %v", err)
	}
	migrated, err := db.GetByMessageID("mig@example.com")
	if err != nil {
		t.Fatalf("get migrated message: %v", err)
	}
	if got, want := migrated.Flags, `\Seen \Flagged`; got != want {
		t.Fatalf("migrated flags = %q, want %q", got, want)
	}

	folders := []backend.Folder{{Name: "ALL", Role: backend.RoleInbox, Selectable: true}}
	fake := newFakeBackend(folders, nil) // no messages to fetch this run
	fake.caps.FlagChangesInDelta = true  // a delta backend, like Gmail
	engine := newTestEngine(db, newMemCursorStore())

	if _, err := engine.Sync(context.Background(), fake); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// The read+flagged message must be seeded, not fetched.
	for _, c := range fake.fetchFlagsCalls {
		for _, r := range c.refs {
			if r.ID == "r1" {
				t.Error("migrated read message should be baseline-seeded, not a FetchFlags candidate")
			}
		}
	}
	// Its baseline is now the stored server flag state.
	found := false
	for _, r := range mustFlagRows(t, db) {
		if r.MessageID == "mig@example.com" {
			found = true
			if r.SyncedFlags != `\Seen,\Flagged` {
				t.Errorf("synced_flags = %q, want \\Seen,\\Flagged (seeded from migrated flags)", r.SyncedFlags)
			}
		}
	}
	if !found {
		t.Fatal("migrated message is missing from folder flag state")
	}
}

func mustFlagRows(t *testing.T, db *store.DB) []store.FolderFlagRow {
	t.Helper()
	rows, err := db.GetFolderFlagState(testAccount, "ALL")
	if err != nil {
		t.Fatalf("flag state: %v", err)
	}
	return rows
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

func TestEngineNoFlagsRetainsDeltaForLaterReconciliation(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
	read := backend.Message{
		MessageID: "no-flags-delta@example.com",
		Ref:       backend.RemoteRef{Folder: folder.Name, ID: "r1"},
		Raw:       rawMessage("no-flags-delta@example.com", "a@example.com", testAccount, "No flags", "body"),
		Flags:     backend.Flags{Seen: true},
	}
	unread := read
	unread.Flags = backend.Flags{}
	fake := newFakeBackend([]backend.Folder{folder}, nil)
	fake.caps.FlagChangesInDelta = true
	fake.fetchByCursor = func(_ string, cursor backend.Cursor) backend.FetchResult {
		switch string(cursor) {
		case "":
			return backend.FetchResult{Messages: []backend.Message{read}, Cursor: backend.Cursor("c1")}
		case "c1":
			return backend.FetchResult{Messages: []backend.Message{unread}, Cursor: backend.Cursor("c2")}
		default:
			return backend.FetchResult{Cursor: cursor}
		}
	}
	fake.flagsByRef["r1"] = backend.Flags{Seen: true}
	if result, err := newTestEngine(db, cursors).Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("seed result=%+v err=%v", result, err)
	}

	fake.flagsByRef["r1"] = backend.Flags{}
	noFlags := New(Options{
		Store: db, Cursors: cursors, Account: testAccount, NoFlags: true,
		Ingest: IngestOptions{Account: testAccount},
	})
	if result, err := noFlags.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("no-flags result=%+v err=%v", result, err)
	}
	state, err := cursors.GetState(testAccount, folder.Name)
	if err != nil || string(state.Cursor) != "c2" || !slices.Equal(state.PendingFlags.Refs, []string{"r1"}) {
		t.Fatalf("state after no-flags=%+v err=%v", state, err)
	}

	if result, err := newTestEngine(db, cursors).Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("recovery result=%+v err=%v", result, err)
	}
	rows, err := db.GetFolderFlagState(testAccount, folder.Name)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if !tagsContain(rows[0].Tags, "unread") || rows[0].SyncedFlags != "" || !rows[0].SyncedFlagsInitialized {
		t.Fatalf("recovered row=%+v", rows[0])
	}
	state, _ = cursors.GetState(testAccount, folder.Name)
	if len(state.PendingFlags.Refs) != 0 {
		t.Fatalf("pending flags not cleared: %+v", state.PendingFlags)
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

func TestFileCursorStorePersistsPendingFlagsAndLoadsLegacyFiles(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	fcs := NewFileCursorStore(testAccount)
	pending := PendingFlags{Refs: []string{"r1", "r2"}, ReplayCount: 1}
	if err := fcs.Commit(testAccount, "INBOX", backend.Cursor("cursor"), pending); err != nil {
		t.Fatalf("commit: %v", err)
	}
	fresh := NewFileCursorStore(testAccount)
	got, err := fresh.GetPendingFlags(testAccount, "INBOX")
	if err != nil || !slices.Equal(got.Refs, pending.Refs) || got.ReplayCount != pending.ReplayCount {
		t.Fatalf("pending round trip = %+v, %v", got, err)
	}
	if err := fresh.Set(testAccount, "INBOX", backend.Cursor("updated")); err != nil {
		t.Fatalf("set cursor: %v", err)
	}
	got, _ = fresh.GetPendingFlags(testAccount, "INBOX")
	if !slices.Equal(got.Refs, pending.Refs) {
		t.Fatalf("Set erased pending state: %+v", got)
	}

	// A pre-366 binary reads the file as map[string][]byte. It must see the
	// provider cursor unchanged and preserve the unknown pending-state entry
	// when it performs its own read-modify-write.
	data, err := os.ReadFile(fcs.path(testAccount))
	if err != nil {
		t.Fatal(err)
	}
	legacyView := make(map[string][]byte)
	if err := json.Unmarshal(data, &legacyView); err != nil {
		t.Fatalf("pre-366 decode failed: %v\n%s", err, data)
	}
	if string(legacyView["INBOX"]) != "updated" || len(legacyView[pendingFlagsKey]) == 0 {
		t.Fatalf("pre-366 view = %#v", legacyView)
	}
	legacyView["INBOX"] = []byte("downgraded")
	data, err = json.MarshalIndent(legacyView, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fcs.path(testAccount), data, 0600); err != nil {
		t.Fatal(err)
	}
	state, err := fresh.GetState(testAccount, "INBOX")
	if err != nil || string(state.Cursor) != "downgraded" || !slices.Equal(state.PendingFlags.Refs, pending.Refs) {
		t.Fatalf("state after pre-366 rewrite = %+v, %v", state, err)
	}

	envelopeAccount := "envelope@example.com"
	envelopeStore := NewFileCursorStore(envelopeAccount)
	envelopeData, err := json.Marshal(cursorFile{Version: 1, Folders: map[string]FolderState{
		"ALL": {Cursor: backend.Cursor("c327-cursor"), PendingFlags: pending},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envelopeStore.path(envelopeAccount), envelopeData, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := envelopeStore.GetState(envelopeAccount, "ALL"); err != nil {
		t.Fatalf("migrate c327 envelope: %v", err)
	}
	migratedData, err := os.ReadFile(envelopeStore.path(envelopeAccount))
	if err != nil {
		t.Fatal(err)
	}
	migratedLegacyView := make(map[string][]byte)
	if err := json.Unmarshal(migratedData, &migratedLegacyView); err != nil || string(migratedLegacyView["ALL"]) != "c327-cursor" {
		t.Fatalf("migrated c327 file is not downgrade-compatible: %v\n%s", err, migratedData)
	}

	corruptAccount := "corrupt-pending@example.com"
	corruptStore := NewFileCursorStore(corruptAccount)
	corruptData, err := json.Marshal(map[string][]byte{
		"ALL":           []byte("durable-cursor"),
		pendingFlagsKey: []byte("{invalid"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corruptStore.path(corruptAccount), corruptData, 0600); err != nil {
		t.Fatal(err)
	}
	corruptState, err := corruptStore.GetState(corruptAccount, "ALL")
	if err != nil || string(corruptState.Cursor) != "durable-cursor" || !corruptState.PendingFlags.FullScan {
		t.Fatalf("corrupt pending recovery = %+v, %v", corruptState, err)
	}
	repairedData, err := os.ReadFile(corruptStore.path(corruptAccount))
	if err != nil {
		t.Fatal(err)
	}
	repairedView := make(map[string][]byte)
	if err := json.Unmarshal(repairedData, &repairedView); err != nil {
		t.Fatalf("decode repaired cursor file: %v", err)
	}
	var repairedPending map[string]PendingFlags
	if err := json.Unmarshal(repairedView[pendingFlagsKey], &repairedPending); err != nil || !repairedPending["ALL"].FullScan {
		t.Fatalf("persisted pending recovery = %+v, %v", repairedPending, err)
	}

	legacyAccount := "legacy@example.com"
	legacyStore := NewFileCursorStore(legacyAccount)
	if err := os.MkdirAll(legacyStore.cacheDir, 0700); err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(map[string][]byte{"ALL": []byte("legacy-cursor")})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyStore.path(legacyAccount), data, 0600); err != nil {
		t.Fatal(err)
	}
	legacyCursor, err := legacyStore.Get(legacyAccount, "ALL")
	if err != nil || string(legacyCursor) != "legacy-cursor" {
		t.Fatalf("legacy cursor = %q, %v", legacyCursor, err)
	}
}

// sameSet reports whether got and want hold the same elements (order-agnostic).
func sameSet(got, want []string) bool {
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	slices.Sort(g)
	slices.Sort(w)
	return slices.Equal(g, w)
}

func TestDiffLabels(t *testing.T) {
	vocab := map[string]bool{"inbox": true, "important": true, "trash": true, "project-x": true}
	tests := []struct {
		name                          string
		tags, baseline                []string
		wantAdd, wantRemove, wantBase []string
	}{
		{
			name: "archive drops inbox label",
			tags: []string{"work"}, baseline: []string{"inbox"},
			wantAdd: nil, wantRemove: []string{"inbox"}, wantBase: nil, // "work" not in vocab
		},
		{
			name: "add a user label",
			tags: []string{"inbox", "project-x"}, baseline: []string{"inbox"},
			wantAdd: []string{"project-x"}, wantRemove: nil, wantBase: []string{"inbox", "project-x"},
		},
		{
			name: "local non-vocab tag is never pushed",
			tags: []string{"inbox", "ephemeral"}, baseline: []string{"inbox"},
			wantAdd: nil, wantRemove: nil, wantBase: []string{"inbox"},
		},
		{
			name: "delete adds trash, drops inbox",
			tags: []string{"trash"}, baseline: []string{"inbox"},
			wantAdd: []string{"trash"}, wantRemove: []string{"inbox"}, wantBase: []string{"trash"},
		},
		{
			name: "no change is a no-op",
			tags: []string{"inbox"}, baseline: []string{"inbox"},
			wantAdd: nil, wantRemove: nil, wantBase: []string{"inbox"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			add, remove, base := diffLabels(tt.tags, tt.baseline, vocab)
			if !sameSet(add, tt.wantAdd) {
				t.Errorf("added = %v, want %v", add, tt.wantAdd)
			}
			if !sameSet(remove, tt.wantRemove) {
				t.Errorf("removed = %v, want %v", remove, tt.wantRemove)
			}
			if !sameSet(base, tt.wantBase) {
				t.Errorf("newBaseline = %v, want %v", base, tt.wantBase)
			}
		})
	}
}

// TestEngineUploadsLabelChanges proves the label-upload pass turns local tag
// edits on a LabelsAreTags backend into ApplyLabels calls and resets the
// baseline to server truth, while never pushing a Durian-local tag.
func TestEngineUploadsLabelChanges(t *testing.T) {
	db := newTestDB(t)
	folders := []backend.Folder{{Name: "ALL", Role: backend.RoleAll, Selectable: true}}
	scripts := map[string][]backend.FetchResult{
		"ALL": {{
			Messages: []backend.Message{{
				MessageID: "lbl@example.com",
				Ref:       backend.RemoteRef{Folder: "ALL", ID: "701"},
				Raw:       rawMessage("lbl@example.com", "a@example.com", testAccount, "hi", "body"),
				Labels:    []string{"inbox"},
			}},
			Cursor: backend.Cursor("c1"),
		}},
	}
	fake := newFakeBackend(folders, scripts)
	fake.caps.LabelsAreTags = true
	fake.labelVocab = []string{"inbox", "important", "trash"}
	cursors := newMemCursorStore()

	// First sync ingests the message with the "inbox" label as its baseline.
	if _, err := newTestEngine(db, cursors).Sync(context.Background(), fake); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// GUI actions: archive (drop inbox), add a real label (important), and add a
	// Durian-local tag (todo) that is NOT in the vocabulary.
	if err := db.ModifyTagsByMessageIDAndAccount("lbl@example.com", testAccount,
		[]string{"important", "todo"}, []string{"inbox"}); err != nil {
		t.Fatalf("edit tags: %v", err)
	}

	res, err := newTestEngine(db, cursors).Sync(context.Background(), fake)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if res.Moved != 1 {
		t.Errorf("Result.Moved = %d, want 1", res.Moved)
	}
	if len(fake.labelCalls) != 1 {
		t.Fatalf("labelCalls = %d, want 1: %+v", len(fake.labelCalls), fake.labelCalls)
	}
	call := fake.labelCalls[0]
	if call.ref.ID != "701" {
		t.Errorf("ApplyLabels ref = %q, want 701", call.ref.ID)
	}
	if !sameSet(call.add, []string{"important"}) {
		t.Errorf("ApplyLabels add = %v, want [important] (todo must not be pushed)", call.add)
	}
	if !sameSet(call.remove, []string{"inbox"}) {
		t.Errorf("ApplyLabels remove = %v, want [inbox]", call.remove)
	}

	// Baseline is reset to server truth (vocabulary tags only): important, not todo.
	base, err := db.GetSyncedLabels("lbl@example.com", testAccount)
	if err != nil {
		t.Fatalf("get baseline: %v", err)
	}
	if !sameSet(strings.Split(base, ","), []string{"important"}) {
		t.Errorf("baseline = %q, want [important]", base)
	}
}

// TestEngineSeedsEmptyLabelBaseline proves a migrated row (label tags present,
// empty synced_labels) is seeded from its current tags WITHOUT any ApplyLabels
// upload — the fix for the label-upload storm where every migrated message
// re-added labels the server already had.
func TestEngineSeedsEmptyLabelBaseline(t *testing.T) {
	db := newTestDB(t)
	folders := []backend.Folder{{Name: "ALL", Role: backend.RoleAll, Selectable: true}}
	scripts := map[string][]backend.FetchResult{
		"ALL": {{
			Messages: []backend.Message{{
				MessageID: "mig@example.com",
				Ref:       backend.RemoteRef{Folder: "ALL", ID: "801"},
				Raw:       rawMessage("mig@example.com", "a@example.com", testAccount, "hi", "body"),
				Labels:    []string{"inbox", "important"},
			}},
			Cursor: backend.Cursor("c1"),
		}},
	}
	fake := newFakeBackend(folders, scripts)
	fake.caps.LabelsAreTags = true
	fake.labelVocab = []string{"inbox", "important"}
	cursors := newMemCursorStore()

	// Ingest (sets baseline), then simulate a migrated row by clearing it.
	if _, err := newTestEngine(db, cursors).Sync(context.Background(), fake); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if err := db.SetSyncedLabels("mig@example.com", testAccount, ""); err != nil {
		t.Fatalf("clear baseline: %v", err)
	}

	// Next sync: uploadLabelChanges must SEED, not upload.
	if _, err := newTestEngine(db, cursors).Sync(context.Background(), fake); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if len(fake.labelCalls) != 0 {
		t.Errorf("expected no ApplyLabels calls (seed only), got %+v", fake.labelCalls)
	}
	base, err := db.GetSyncedLabels("mig@example.com", testAccount)
	if err != nil {
		t.Fatalf("get baseline: %v", err)
	}
	if !sameSet(strings.Split(base, ","), []string{"inbox", "important"}) {
		t.Errorf("seeded baseline = %q, want [inbox important]", base)
	}
}

// TestEngineAnsweredFlagNoPingPong proves the fix for the replied/Answered
// ping-pong: on a backend that can't persist \Answered (Capabilities
// .AnsweredUnsupported, e.g. Gmail), a local "replied" tag must survive repeated
// syncs. Without the serverState.Answered = baseline mask, the third sync's
// download branch strips it (server reports un-answered, baseline says answered).
func TestEngineAnsweredFlagNoPingPong(t *testing.T) {
	db := newTestDB(t)
	folders := []backend.Folder{{Name: "ALL", Role: backend.RoleAll, Selectable: true}}
	scripts := map[string][]backend.FetchResult{
		"ALL": {{
			Messages: []backend.Message{{
				MessageID: "rep@example.com",
				Ref:       backend.RemoteRef{Folder: "ALL", ID: "901"},
				Raw:       rawMessage("rep@example.com", "a@example.com", testAccount, "hi", "body"),
				Flags:     backend.Flags{Seen: true},
			}},
			Cursor: backend.Cursor("c1"),
		}},
	}
	fake := newFakeBackend(folders, scripts)
	fake.caps.AnsweredUnsupported = true
	// The server reports the message as seen but never answered (no \Answered).
	fake.flagsByRef["901"] = backend.Flags{Seen: true}
	cursors := newMemCursorStore()

	if _, err := newTestEngine(db, cursors).Sync(context.Background(), fake); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	// The user replies -> "replied" tag added locally.
	if err := db.ModifyTagsByMessageIDAndAccount("rep@example.com", testAccount, []string{"replied"}, nil); err != nil {
		t.Fatalf("add replied: %v", err)
	}

	// Repeated syncs must not strip the replied tag.
	for i := 1; i <= 3; i++ {
		if _, err := newTestEngine(db, cursors).Sync(context.Background(), fake); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
		if !slices.Contains(mustTags(t, db, "rep@example.com"), "replied") {
			t.Fatalf("replied tag stripped on sync %d (Answered ping-pong)", i)
		}
	}
}

// A FetchFlags outage must not discard a server flag change the delta already
// carried: the folder cursor advances regardless (download progress is
// independent of flag reconciliation), so no later pass re-selects the row and
// the change would otherwise be lost permanently.
func TestEngineAppliesDeltaFlagsWhenFlagFetchFails(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	ref := backend.RemoteRef{Folder: "INBOX", ID: "m1"}
	flagged := backend.Message{
		MessageID: "m1@example.com", Ref: ref, Flags: backend.Flags{Flagged: true},
		Raw: rawMessage("m1@example.com", "a@example.com", testAccount, "Subject", "body"),
	}
	read := flagged
	read.Flags = backend.Flags{Seen: true, Flagged: true}

	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {
			{Messages: []backend.Message{flagged}, Cursor: backend.Cursor("c1")},
			// Second pass: the server marked it read and the delta says so, but
			// the flag fetch is down for this pass.
			{Messages: []backend.Message{read}, Cursor: backend.Cursor("c2")},
		},
	})
	// Seed pass: a successful flag fetch establishes a real baseline, so the
	// baseline-seeding branch stays out of the way on the second pass.
	fake.flagsByRef["m1"] = backend.Flags{Flagged: true}
	engine := newTestEngine(db, cursors)
	if _, err := engine.Sync(t.Context(), fake); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	fake.fetchFlagsPartial = true
	fake.fetchFlagsErr = fmt.Errorf("%w: flag endpoint partially unavailable", backend.ErrPartialFlags)

	result, err := engine.Sync(t.Context(), fake)
	if err != nil {
		t.Fatalf("Sync error = %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("Sync errors = %v, want the flag-fetch failure recorded", result.Errors)
	}
	if got, _ := cursors.Get(testAccount, "INBOX"); string(got) != "c2" {
		t.Fatalf("cursor = %q, want c2 (download progress must not wait on the flag pass)", got)
	}
	rows, err := db.GetFolderFlagState(testAccount, "INBOX")
	if err != nil || len(rows) != 1 {
		t.Fatalf("GetFolderFlagState = %v, %v", rows, err)
	}
	if tagsContain(rows[0].Tags, "unread") {
		t.Error("server read state was discarded when FetchFlags failed; the delta carried it")
	}
}

// gmailbackend builds hydrated messages without Message.MessageID, so the
// preservation of an older local copy must not depend on that field being set.
func TestEngineReplacementPreservesPriorCopyWhenHydratorOmitsMessageID(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	prior := backend.Message{
		MessageID: "shared@example.com", Ref: backend.RemoteRef{Folder: "INBOX", ID: "prior"},
		Raw: rawMessage("shared@example.com", "a@example.com", testAccount, "Prior", "body"),
	}
	if _, _, err := Ingest(db, prior, "INBOX", backend.RoleNone, IngestOptions{Account: testAccount}); err != nil {
		t.Fatalf("seed prior copy: %v", err)
	}
	ref := backend.RemoteRef{Folder: "INBOX", ID: "broken"}
	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {{
			Cursor: backend.Cursor("replacement"), FullSnapshot: true,
			Present: []backend.RemoteRef{ref},
		}},
	})
	// Message-ID resolvable from the raw headers, but the body still fails
	// strict parsing — exactly the shape that reaches the preservation path.
	fake.snapshotMessages = map[string]backend.Message{
		ref.ID: {Ref: ref, Raw: []byte("Message-ID: <shared@example.com>\r\nmalformed header line\r\n\r\nbody")},
	}
	result, err := newTestEngine(db, cursors).Sync(t.Context(), fake)
	if err != nil {
		t.Fatalf("Sync error = %v", err)
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0].Error(), "parse message") {
		t.Fatalf("Sync errors = %v, want one permanent hydrated parse failure", result.Errors)
	}
	if got, _ := cursors.Get(testAccount, "INBOX"); string(got) != "replacement" {
		t.Fatalf("cursor = %q, want replacement (must not wedge)", got)
	}
	if got, _ := db.GetByMessageID(prior.MessageID); got == nil {
		t.Fatal("older local copy was deleted: preservation keyed on an unset Message.MessageID")
	}
}

func TestMessageIDFromRaw(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"simple", "Message-ID: <a@example.com>\r\n\r\nbody", "a@example.com"},
		{"case insensitive", "message-id: <b@example.com>\r\n\r\nbody", "b@example.com"},
		{"after other headers", "From: x@y\r\nMessage-ID: <c@example.com>\r\n\r\nbody", "c@example.com"},
		{"folded", "Message-ID:\r\n <d@example.com>\r\n\r\nbody", "d@example.com"},
		{"absent", "From: x@y\r\n\r\nbody", ""},
		{"body only match ignored", "From: x@y\r\n\r\nMessage-ID: <e@example.com>", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := messageIDFromRaw([]byte(tt.raw)); got != tt.want {
				t.Errorf("messageIDFromRaw = %q, want %q", got, tt.want)
			}
		})
	}
}

// On the success path an omission from FetchFlags is the backend's way of
// reporting a dead ref (graphbackend's 404 branch), so the delta fallback must
// not resurrect stale flags for it.
func TestEngineDoesNotFallBackToDeltaFlagsWhenFetchSucceeds(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	ref := backend.RemoteRef{Folder: "INBOX", ID: "m1"}
	flagged := backend.Message{
		MessageID: "m1@example.com", Ref: ref, Flags: backend.Flags{Flagged: true},
		Raw: rawMessage("m1@example.com", "a@example.com", testAccount, "Subject", "body"),
	}
	read := flagged
	read.Flags = backend.Flags{Seen: true, Flagged: true}

	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {
			{Messages: []backend.Message{flagged}, Cursor: backend.Cursor("c1")},
			{Messages: []backend.Message{read}, Cursor: backend.Cursor("c2")},
		},
	})
	fake.flagsByRef["m1"] = backend.Flags{Flagged: true}
	engine := newTestEngine(db, cursors)
	if _, err := engine.Sync(t.Context(), fake); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	// Second pass: the fetch succeeds but reports the ref as gone by omitting it.
	delete(fake.flagsByRef, "m1")

	result, err := engine.Sync(t.Context(), fake)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("Sync result=%+v err=%v, want a clean pass", result, err)
	}
	rows, err := db.GetFolderFlagState(testAccount, "INBOX")
	if err != nil || len(rows) != 1 {
		t.Fatalf("GetFolderFlagState = %v, %v", rows, err)
	}
	if !tagsContain(rows[0].Tags, "unread") {
		t.Error("delta flags were applied to a ref the backend reported as gone")
	}
}
