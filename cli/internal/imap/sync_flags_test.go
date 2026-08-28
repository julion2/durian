package imap

import (
	"bytes"
	"io"
	"slices"
	"testing"

	goimap "github.com/emersion/go-imap"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/dbcrypto"
	"github.com/julion2/durian/cli/internal/store"
)

const (
	// A non-INBOX mailbox keeps the tests on the flag reconciliation itself:
	// INBOX additionally runs stale-tag cleanup and folder-move upload, which
	// would need a live client and would blur what an assertion here proves.
	flagSyncMailbox = "Archive"
	flagSyncAccount = "test"
)

type flagCall struct {
	uid   uint32
	flags []string
}

// fakeFlagTransport scripts the server side of the flag pass. Add/Remove
// mutate serverFlags so a later FetchFlags observes what the syncer wrote —
// the two-run tests depend on the second run seeing the first run's uploads.
type fakeFlagTransport struct {
	serverFlags map[uint32][]string
	addCalls    []flagCall
	removeCalls []flagCall
	// onUpload runs before each AddFlags/RemoveFlags. syncFlags snapshots the
	// local tags before it uploads and writes them after, so this is the hook
	// a test uses to land a concurrent tag change inside that window and force
	// the download's compare-and-swap to refuse.
	onUpload func()
}

func (f *fakeFlagTransport) FetchFlags(uids []uint32) (map[uint32][]string, error) {
	out := make(map[uint32][]string, len(uids))
	for _, uid := range uids {
		if flags, ok := f.serverFlags[uid]; ok {
			out[uid] = slices.Clone(flags)
		}
	}
	return out, nil
}

func (f *fakeFlagTransport) AddFlags(uid uint32, flags []string) error {
	if f.onUpload != nil {
		f.onUpload()
	}
	f.addCalls = append(f.addCalls, flagCall{uid: uid, flags: slices.Clone(flags)})
	for _, flag := range flags {
		if !slices.Contains(f.serverFlags[uid], flag) {
			f.serverFlags[uid] = append(f.serverFlags[uid], flag)
		}
	}
	return nil
}

func (f *fakeFlagTransport) RemoveFlags(uid uint32, flags []string) error {
	if f.onUpload != nil {
		f.onUpload()
	}
	f.removeCalls = append(f.removeCalls, flagCall{uid: uid, flags: slices.Clone(flags)})
	f.serverFlags[uid] = slices.DeleteFunc(slices.Clone(f.serverFlags[uid]), func(fl string) bool {
		return slices.Contains(flags, fl)
	})
	return nil
}

// uploadedFlags flattens every flag that travelled to the server, in either
// direction. Tests that promise "this flag never went up" must check both
// call lists — an errant \Deleted could arrive as an add or as a remove.
func (f *fakeFlagTransport) uploadedFlags() []string {
	var out []string
	for _, c := range f.addCalls {
		out = append(out, c.flags...)
	}
	for _, c := range f.removeCalls {
		out = append(out, c.flags...)
	}
	return out
}

func newFlagTestDB(t *testing.T) *store.DB {
	t.Helper()
	kr, err := dbcrypto.NewKeyring(bytes.Repeat([]byte{0x42}, dbcrypto.MasterKeyLen))
	if err != nil {
		t.Fatalf("test keyring: %v", err)
	}
	db, err := store.Open(":memory:", kr)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := db.Init(); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seedFlagMessage inserts a message with the given tags. Every message gets at
// least one tag on purpose: syncFlags reads the local side via
// GetAllMessagesWithTags, which joins on the tags table, so a tagless message
// is invisible to the pass and its test would assert against a no-op.
func seedFlagMessage(t *testing.T, db *store.DB, messageID string, tags ...string) {
	t.Helper()
	msg := &store.Message{
		MessageID: messageID, Subject: "Test " + messageID,
		FromAddr: "a@x", Date: 1700000000, CreatedAt: 1700000000,
		FetchedBody: true, Account: flagSyncAccount, Mailbox: flagSyncMailbox,
	}
	if err := db.InsertMessage(msg); err != nil {
		t.Fatalf("insert %s: %v", messageID, err)
	}
	for _, tag := range tags {
		if err := db.AddTag(msg.ID, tag); err != nil {
			t.Fatalf("seed tag %q: %v", tag, err)
		}
	}
}

// newFlagSyncer builds a Syncer whose flag pass runs entirely against the fake
// transport and the in-memory store; the nil client is never touched because
// every UID the tests use is pre-mapped, so ensureMessageIDMapping returns
// before its envelope fetch.
func newFlagSyncer(db *store.DB, fake *fakeFlagTransport, mode SyncMode) (*Syncer, *MailboxState) {
	s := &Syncer{
		account:               &config.AccountConfig{Name: flagSyncAccount},
		options:               &SyncOptions{Mode: mode, Store: db},
		output:                io.Discard,
		store:                 db,
		flagTransportOverride: fake,
	}
	return s, NewState().GetMailboxState(flagSyncMailbox)
}

func messageTags(t *testing.T, db *store.DB, messageID string) []string {
	t.Helper()
	tags, err := db.GetTagsByMessageID(messageID)
	if err != nil {
		t.Fatalf("get tags for %s: %v", messageID, err)
	}
	return tags
}

func TestSyncFlags_LocalMarkUnreadRemovesSeenOnServer(t *testing.T) {
	db := newFlagTestDB(t)
	seedFlagMessage(t, db, "unread@test", "unread")

	fake := &fakeFlagTransport{serverFlags: map[uint32][]string{
		1: {goimap.SeenFlag},
	}}
	s, mbox := newFlagSyncer(db, fake, SyncBidirectional)
	mbox.SetMessageID(1, "unread@test")
	mbox.SetMessageFlags(1, FlagState{Seen: true})

	s.syncFlags(flagSyncMailbox, mbox, []uint32{1})

	// The mark-unread must reach the server as exactly one \Seen removal. A
	// resolver that ORs local and server instead of consulting the baseline
	// cannot ever let a cleared flag win, and this is the assertion that
	// catches that regression.
	var removed []string
	for _, c := range fake.removeCalls {
		removed = append(removed, c.flags...)
	}
	if !slices.Equal(removed, []string{goimap.SeenFlag}) {
		t.Errorf("removed flags = %v, want [%s]", removed, goimap.SeenFlag)
	}

	// Nothing else may travel: an add alongside the removal would mean the
	// resolver invented a change neither side made.
	for _, c := range fake.addCalls {
		if len(c.flags) > 0 {
			t.Errorf("AddFlags carried %v, want no flags added", c.flags)
		}
	}
	if len(fake.serverFlags[1]) != 0 {
		t.Errorf("server flags after sync = %v, want none", fake.serverFlags[1])
	}
}

func TestSyncFlags_ServerDeletedNeverWrittenBack(t *testing.T) {
	db := newFlagTestDB(t)
	seedFlagMessage(t, db, "pending-expunge@test", "archive")

	fake := &fakeFlagTransport{serverFlags: map[uint32][]string{
		1: {goimap.SeenFlag, goimap.DeletedFlag},
	}}
	s, mbox := newFlagSyncer(db, fake, SyncBidirectional)
	mbox.SetMessageID(1, "pending-expunge@test")
	mbox.SetMessageFlags(1, FlagState{Seen: true})

	// Two runs on purpose. The first run banks \Deleted into the baseline;
	// the old defect only surfaced on the run after that, when the local
	// absence of \Deleted read as a user change and pushed a removal that
	// un-marked another client's pending expunge.
	s.syncFlags(flagSyncMailbox, mbox, []uint32{1})
	s.syncFlags(flagSyncMailbox, mbox, []uint32{1})

	if slices.Contains(fake.uploadedFlags(), goimap.DeletedFlag) {
		t.Errorf("\\Deleted travelled to the server: adds=%v removes=%v",
			fake.addCalls, fake.removeCalls)
	}
	if !slices.Contains(fake.serverFlags[1], goimap.DeletedFlag) {
		t.Errorf("server flags = %v, want \\Deleted still set: the pending expunge belongs to the server", fake.serverFlags[1])
	}

	// \Deleted must not leak into tags either: durian's "deleted" tag means
	// moved-to-trash, and inventing it here would make the move uploader
	// treat a mere witness of \Deleted as a user delete.
	tags := messageTags(t, db, "pending-expunge@test")
	if slices.Contains(tags, "deleted") || slices.Contains(tags, "trash") {
		t.Errorf("local tags = %v, want no deleted/trash tag", tags)
	}
}

func TestSyncFlags_UploadOnlyDoesNotConsumeServerChange(t *testing.T) {
	db := newFlagTestDB(t)
	seedFlagMessage(t, db, "uponly@test", "unread")

	fake := &fakeFlagTransport{serverFlags: map[uint32][]string{
		1: {goimap.SeenFlag, goimap.FlaggedFlag},
	}}
	s, mbox := newFlagSyncer(db, fake, SyncUploadOnly)
	mbox.SetMessageID(1, "uponly@test")
	mbox.SetMessageFlags(1, FlagState{Seen: true, Flagged: false})

	s.syncFlags(flagSyncMailbox, mbox, []uint32{1})

	if !slices.Contains(fake.serverFlags[1], goimap.FlaggedFlag) {
		t.Fatalf("server flags = %v, want \\Flagged still set after upload-only run", fake.serverFlags[1])
	}

	// The server's star was never applied locally, so the baseline may not
	// claim it was reconciled. Banking it here is exactly the lie the next
	// run consumes: the local side lacks a flag the baseline says both sides
	// agreed on, which reads as the user unstarring.
	baseline, ok := mbox.GetMessageFlags(1)
	if !ok {
		t.Fatal("baseline missing after sync")
	}
	if baseline.Flagged {
		t.Errorf("baseline = %+v, want Flagged=false: the server change was not pulled", baseline)
	}

	// The live failure mode was the second watcher-triggered UploadOnly run
	// stripping the star off the server.
	s.syncFlags(flagSyncMailbox, mbox, []uint32{1})
	if !slices.Contains(fake.serverFlags[1], goimap.FlaggedFlag) {
		t.Errorf("server flags = %v after second upload-only run, want \\Flagged still set", fake.serverFlags[1])
	}
}

func TestSyncFlags_DownloadOnlyDoesNotConsumeLocalChange(t *testing.T) {
	db := newFlagTestDB(t)
	seedFlagMessage(t, db, "downonly@test", "unread")

	fake := &fakeFlagTransport{serverFlags: map[uint32][]string{
		1: {goimap.SeenFlag, goimap.FlaggedFlag},
	}}
	s, mbox := newFlagSyncer(db, fake, SyncDownloadOnly)
	mbox.SetMessageID(1, "downonly@test")
	mbox.SetMessageFlags(1, FlagState{Seen: true, Flagged: false})

	s.syncFlags(flagSyncMailbox, mbox, []uint32{1})

	// The server's star lands, and the download must not flatten the local
	// mark-unread while doing so — the resolved state carries both changes.
	tags := messageTags(t, db, "downonly@test")
	if !slices.Contains(tags, "flagged") {
		t.Errorf("tags = %v, want flagged: the server change was not pulled", tags)
	}
	if !slices.Contains(tags, "unread") {
		t.Errorf("tags = %v, want unread kept: download flattened the local change", tags)
	}

	// The mark-unread never went up, so the baseline must keep claiming
	// Seen=true. Banking the un-uploaded change would erase the only evidence
	// a later bidirectional run has that an upload is still owed.
	baseline, ok := mbox.GetMessageFlags(1)
	if !ok {
		t.Fatal("baseline missing after sync")
	}
	if !baseline.Seen {
		t.Errorf("baseline = %+v, want Seen=true: the local change was not pushed", baseline)
	}

	if got := fake.uploadedFlags(); len(got) > 0 {
		t.Errorf("uploaded %v in download-only mode, want nothing", got)
	}
}

func TestSyncFlags_RefusedDownloadAdvancesOnlyPushedFields(t *testing.T) {
	db := newFlagTestDB(t)
	seedFlagMessage(t, db, "refused@test", "unread")

	fake := &fakeFlagTransport{serverFlags: map[uint32][]string{
		1: {goimap.SeenFlag, goimap.FlaggedFlag},
	}}
	s, mbox := newFlagSyncer(db, fake, SyncBidirectional)
	mbox.SetMessageID(1, "refused@test")
	mbox.SetMessageFlags(1, FlagState{Seen: true, Flagged: false})

	// The upload runs between the tag snapshot and the download's write, so
	// its transport call is where a concurrent user action can land. Adding
	// "replied" changes the watched flag tags and must make the CAS refuse —
	// an unguarded absolute write would strip the tag right back off.
	hooked := false
	fake.onUpload = func() {
		if hooked {
			return
		}
		hooked = true
		if err := db.ModifyTagsByMessageIDAndAccount(
			"refused@test", flagSyncAccount, []string{"replied"}, nil); err != nil {
			t.Fatalf("concurrent tag write: %v", err)
		}
	}

	s.syncFlags(flagSyncMailbox, mbox, []uint32{1})
	if !hooked {
		t.Fatal("upload never ran, the concurrent write was not exercised")
	}

	// The push half really happened and must count.
	if slices.Contains(fake.serverFlags[1], goimap.SeenFlag) {
		t.Errorf("server flags = %v, want \\Seen removed", fake.serverFlags[1])
	}

	// Seen reached the server, so it advances; Flagged only travelled in the
	// refused direction, so it must not. Advancing it anyway would tell the
	// next run the star was already delivered locally, and the pull would
	// never be retried.
	baseline, ok := mbox.GetMessageFlags(1)
	if !ok {
		t.Fatal("baseline missing after sync")
	}
	if baseline.Seen {
		t.Errorf("baseline = %+v, want Seen=false: the pushed field must advance", baseline)
	}
	if baseline.Flagged {
		t.Errorf("baseline = %+v, want Flagged=false: the refused pull must not advance", baseline)
	}

	// The refused write means the tags stay exactly as the user left them:
	// still unread, the mid-sync reply recorded, and no flagged tag yet.
	tags := messageTags(t, db, "refused@test")
	if !slices.Contains(tags, "unread") {
		t.Errorf("tags = %v, want unread kept: refused download must not revert the mark-unread", tags)
	}
	if !slices.Contains(tags, "replied") {
		t.Errorf("tags = %v, want replied kept: the concurrent change is the state to preserve", tags)
	}
	if slices.Contains(tags, "flagged") {
		t.Errorf("tags = %v, want no flagged tag: the download was refused", tags)
	}
}

func TestSyncFlags_FirstSyncCASMissLeavesBaselineUninitialized(t *testing.T) {
	db := newFlagTestDB(t)
	// The first message exists only to give the run an upload, whose transport
	// call is the window where the second message's tags change concurrently.
	seedFlagMessage(t, db, "carrier@test", "unread")
	seedFlagMessage(t, db, "firstsync@test", "unread")

	fake := &fakeFlagTransport{serverFlags: map[uint32][]string{
		1: {goimap.SeenFlag},
		2: {goimap.SeenFlag},
	}}
	s, mbox := newFlagSyncer(db, fake, SyncBidirectional)
	mbox.SetMessageID(1, "carrier@test")
	mbox.SetMessageFlags(1, FlagState{Seen: true})
	// UID 2 gets a mapping but no stored baseline: this is its first sync.
	mbox.SetMessageID(2, "firstsync@test")

	hooked := false
	fake.onUpload = func() {
		if hooked {
			return
		}
		hooked = true
		if err := db.ModifyTagsByMessageIDAndAccount(
			"firstsync@test", flagSyncAccount, []string{"replied"}, nil); err != nil {
			t.Fatalf("concurrent tag write: %v", err)
		}
	}

	s.syncFlags(flagSyncMailbox, mbox, []uint32{1, 2})
	if !hooked {
		t.Fatal("upload never ran, the concurrent write was not exercised")
	}

	// The refused first-sync write must leave the baseline unset. Recording
	// the server state anyway would claim a reconciliation that never reached
	// the tags, and the next run would read the still-divergent local state
	// as a user change instead of finishing the first sync.
	if baseline, ok := mbox.GetMessageFlags(2); ok {
		t.Errorf("baseline = %+v after refused first-sync write, want none stored", baseline)
	}
}
