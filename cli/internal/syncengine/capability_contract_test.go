package syncengine

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/store"
)

// The engine adapts to backend.Capabilities, not to providers. Its real input
// space is therefore the set of capability combinations actually shipped, and
// the tests in engine_test.go set those bits one at a time — which leaves the
// full Gmail profile (all three flag bits together) never exercised, and makes
// a scenario's provider context a matter of reading which line assigned which
// bit.
//
// This file runs one scenario set against all four named production profiles
// (backend.ProfileIMAP and friends). Every cell asserts; a profile that reaches
// a different outcome states that outcome rather than opting out, because the
// per-profile divergence is the behavior under test.
//
// The adapters are held to these same names by
// TestAdapterCapabilitiesMatchNamedProfiles in backendfactory, so a profile
// here cannot drift away from what a provider declares.

// contractProfile is one column of the matrix.
type contractProfile struct {
	name string
	caps backend.Capabilities
}

func contractProfiles() []contractProfile {
	return []contractProfile{
		{"imap", backend.ProfileIMAP},
		{"graph", backend.ProfileGraph},
		{"jmap", backend.ProfileJMAP},
		{"gmail", backend.ProfileGmail},
	}
}

const contractMsgID = "contract@example.com"

// contractOutcome is the state a flag decision touches, gathered after a Sync.
// Asserting on all of it at once is deliberate: a scenario that checks only the
// local tags can pass while the baseline it left behind guarantees a wrong
// decision on the next run — the failure mode behind the recent flag fixes.
type contractOutcome struct {
	// Tags are the Durian tags on the subject message, sorted.
	Tags []string
	// Baseline is the persisted messages.synced_flags before-image.
	Baseline string
	// Uploaded is what the engine pushed back to the provider.
	Uploaded []applyFlagsCall
	// FetchedRefs are the refs the flag pass asked the provider about, sorted.
	FetchedRefs []string
	// Pending is the flag work the engine persisted for the next run.
	Pending PendingFlags
}

// contractEnv is one (profile, scenario) run in progress. It owns the store,
// the fake provider and the cursor store, and it models the folder topology the
// profile implies: a label backend delivers one RoleAll stream and carries tags
// as labels, a folder backend delivers per-role folders.
type contractEnv struct {
	t       *testing.T
	db      *store.DB
	fake    *fakeBackend
	cursors *memCursorStore
	caps    backend.Capabilities
	folder  backend.Folder
	ref     backend.RemoteRef
	opts    Options
}

// newContractEnv seeds a single message that the provider reports with
// serverFlags, then runs one sync so the row and its flag baseline exist. What
// a scenario does afterwards is a change against an established before-image,
// which is the state the engine actually reasons about.
func newContractEnv(t *testing.T, caps backend.Capabilities, serverFlags backend.Flags) *contractEnv {
	t.Helper()

	folder := backend.Folder{Name: "INBOX", Role: backend.RoleInbox, Selectable: true}
	var labels []string
	if caps.LabelsAreTags {
		// Label backends have no per-role folders; everything arrives on one
		// stream and "inbox" is a label, not a folder role.
		folder = backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
		labels = []string{"inbox"}
	}

	ref := backend.RemoteRef{Folder: folder.Name, ID: "c1"}
	msg := backend.Message{
		MessageID: contractMsgID,
		Ref:       ref,
		Raw:       rawMessage(contractMsgID, "sender@example.com", testAccount, "hi", "body"),
		Flags:     serverFlags,
		Labels:    labels,
	}

	db := newTestDB(t)
	fake := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
		folder.Name: {{Messages: []backend.Message{msg}, Cursor: backend.Cursor("c-init")}},
	})
	fake.caps = caps
	fake.flagsByRef[ref.ID] = serverFlags
	if caps.LabelsAreTags {
		// Read state and starring are flags on every provider, never labels.
		// Gmail's systemLabelTags omits STARRED and UNREAD and labelToTag
		// returns "" for both; JMAP's LabelTags come from mailboxIds while
		// $seen/$flagged/$answered travel as keywords. Putting a flag name in
		// here would let the label pass write flag tags, and every conclusion
		// this matrix draws about flag behavior on a label profile would be an
		// artifact of the fake rather than a fact about the engine.
		fake.labelVocab = []string{"inbox"}
	}

	env := &contractEnv{
		t:       t,
		db:      db,
		fake:    fake,
		cursors: newMemCursorStore(),
		caps:    caps,
		folder:  folder,
		ref:     ref,
	}
	env.opts = Options{
		Store:   db,
		Cursors: env.cursors,
		Account: testAccount,
		Ingest:  IngestOptions{Account: testAccount},
	}

	env.sync()
	return env
}

// sync runs one Sync with the env's current options.
func (e *contractEnv) sync() {
	e.t.Helper()
	if _, err := New(e.opts).Sync(context.Background(), e.fake); err != nil {
		e.t.Fatalf("sync: %v", err)
	}
}

// redeliver scripts the message back into the next delta page. A backend that
// reports FlagChangesInDelta only fetches flags for refs that reappeared, so
// without this a server-side change is invisible to it — while a polling
// backend would see it either way. Modelling that difference is the point;
// hiding it would make the delta profiles pass for the wrong reason.
func (e *contractEnv) redeliver(serverFlags backend.Flags, labels []string) {
	e.t.Helper()
	// The fake indexes its script by call count. A sync that ran past the end
	// of the script still advanced that counter, so appending alone can file
	// this page behind the index, where no call will ever reach it — and a
	// delta profile then sees no change while a polling one still finds the row
	// on its own. Point the counter at the new page.
	script := e.fake.scripts[e.folder.Name]
	e.fake.calls[e.folder.Name] = len(script)
	e.fake.scripts[e.folder.Name] = append(script, backend.FetchResult{
		Messages: []backend.Message{{
			MessageID: contractMsgID,
			Ref:       e.ref,
			Raw:       rawMessage(contractMsgID, "sender@example.com", testAccount, "hi", "body"),
			Flags:     serverFlags,
			Labels:    labels,
		}},
		Cursor: backend.Cursor("c-next"),
	})
}

// serverSets changes what the provider reports for the subject message, and
// redelivers it so both delta and polling profiles can observe the change.
func (e *contractEnv) serverSets(flags backend.Flags) {
	e.t.Helper()
	e.fake.flagsByRef[e.ref.ID] = flags
	labels := []string(nil)
	if e.caps.LabelsAreTags {
		labels = []string{"inbox"}
	}
	e.redeliver(flags, labels)
}

// localTags applies a local tag change, standing in for a GUI action.
func (e *contractEnv) localTags(add, remove []string) {
	e.t.Helper()
	e.localTagsFor(contractMsgID, add, remove)
}

// localTagsFor is localTags for a message other than the subject one, used by
// the scenarios that need a second row.
func (e *contractEnv) localTagsFor(messageID string, add, remove []string) {
	e.t.Helper()
	if err := e.db.ModifyTagsByMessageIDAndAccount(messageID, testAccount, add, remove); err != nil {
		e.t.Fatalf("modify tags: %v", err)
	}
}

// deliverNew scripts a brand-new message into the next delta page and registers
// its server flag state, so a scenario can watch a first insert next to the
// established subject row. The labels are the caller's choice because they are
// part of the scenario: a label backend's arrival carries the labels that agree
// with its flags, and a folder backend ignores them entirely.
func (e *contractEnv) deliverNew(msgID, refID string, flags backend.Flags, labels []string) {
	e.t.Helper()
	e.fake.flagsByRef[refID] = flags
	e.fake.scripts[e.folder.Name] = append(e.fake.scripts[e.folder.Name], backend.FetchResult{
		Messages: []backend.Message{{
			MessageID: msgID,
			Ref:       backend.RemoteRef{Folder: e.folder.Name, ID: refID},
			Raw:       rawMessage(msgID, "sender@example.com", testAccount, "hi", "body"),
			Flags:     flags,
			Labels:    labels,
		}},
		Cursor: backend.Cursor("c-fresh"),
	})
}

// baselineFor reads the persisted synced_flags baseline of any message in the
// env's folder; observe() only carries the subject message's.
func (e *contractEnv) baselineFor(messageID string) string {
	e.t.Helper()
	rows, err := e.db.GetFolderFlagState(testAccount, e.folder.Name)
	if err != nil {
		e.t.Fatalf("flag state: %v", err)
	}
	for _, row := range rows {
		if row.MessageID == messageID {
			return row.SyncedFlags
		}
	}
	e.t.Fatalf("no flag row for %s", messageID)
	return ""
}

// uploadsFor filters the recorded ApplyFlags calls down to one remote ref.
func uploadsFor(calls []applyFlagsCall, refID string) []applyFlagsCall {
	var out []applyFlagsCall
	for _, call := range calls {
		if call.ref.ID == refID {
			out = append(out, call)
		}
	}
	return out
}

// observe collects the post-sync state. Provider call records are reset by the
// caller when a scenario needs to distinguish passes.
func (e *contractEnv) observe() contractOutcome {
	e.t.Helper()

	tags := slices.Clone(mustTags(e.t, e.db, contractMsgID))
	slices.Sort(tags)

	var baseline string
	rows, err := e.db.GetFolderFlagState(testAccount, e.folder.Name)
	if err != nil {
		e.t.Fatalf("flag state: %v", err)
	}
	for _, row := range rows {
		if row.MessageID == contractMsgID {
			baseline = row.SyncedFlags
		}
	}

	var fetched []string
	for _, call := range e.fake.fetchFlagsCalls {
		for _, ref := range call.refs {
			fetched = append(fetched, ref.ID)
		}
	}
	slices.Sort(fetched)
	fetched = slices.Compact(fetched)

	pending, err := e.cursors.GetPendingFlags(testAccount, e.folder.Name)
	if err != nil {
		e.t.Fatalf("pending flags: %v", err)
	}

	return contractOutcome{
		Tags:        tags,
		Baseline:    baseline,
		Uploaded:    slices.Clone(e.fake.applyFlagsCalls),
		FetchedRefs: fetched,
		Pending:     pending,
	}
}

// effectiveUploads counts the ApplyFlags calls that would actually change
// something at the provider, ignoring calls whose add and remove are both
// empty.
func effectiveUploads(calls []applyFlagsCall) int {
	var n int
	var empty backend.Flags
	for _, call := range calls {
		if call.add != empty || call.remove != empty {
			n++
		}
	}
	return n
}

// resetCalls clears the provider call records so a scenario can assert on the
// second sync alone rather than on the seeding sync as well.
func (e *contractEnv) resetCalls() {
	e.fake.fetchFlagsCalls = nil
	e.fake.applyFlagsCalls = nil
	e.fake.labelCalls = nil
}

// MARK: - Scenarios

// TestContractServerSetsFlagged covers the plain download branch on a
// successful FetchFlags: the provider flags a message, and the local "flagged"
// tag plus the baseline must both follow. No profile suppresses Flagged, so all
// four are expected to reach the same state by different routes — the delta
// profiles from the redelivered page, IMAP from its mailbox poll.
func TestContractServerSetsFlagged(t *testing.T) {
	for _, p := range contractProfiles() {
		t.Run(p.name, func(t *testing.T) {
			env := newContractEnv(t, p.caps, backend.Flags{Seen: true})
			env.resetCalls()

			env.serverSets(backend.Flags{Seen: true, Flagged: true})
			env.sync()
			got := env.observe()

			if !slices.Contains(got.Tags, "flagged") {
				t.Errorf("tags = %v, want to contain \"flagged\"", got.Tags)
			}
			if !slices.Contains(splitFlags(got.Baseline), "\\Flagged") {
				t.Errorf("baseline = %q, want to record \\Flagged", got.Baseline)
			}
			// A server-side change must not travel back as a local one, and must
			// not cost a round trip either. A folder profile reads the row as
			// locally changed here — the redelivered page wrote the new flag
			// tags at ingest, against a baseline that predates it — but the diff
			// against the server is empty, so the call is skipped rather than
			// sent empty.
			if len(got.Uploaded) != 0 {
				t.Errorf("uploaded %+v, want no ApplyFlags at all for a server-side change", got.Uploaded)
			}
		})
	}
}

// TestContractServerClearsFlagged is the mirror: the provider drops the flag,
// so the tag must go and the baseline must stop claiming it. A baseline that
// keeps \Flagged here would make the next sync read a local removal as a local
// change and push it back, which is the shape of a flag ping-pong.
func TestContractServerClearsFlagged(t *testing.T) {
	for _, p := range contractProfiles() {
		t.Run(p.name, func(t *testing.T) {
			env := newContractEnv(t, p.caps, backend.Flags{Seen: true, Flagged: true})
			env.resetCalls()

			env.serverSets(backend.Flags{Seen: true})
			env.sync()
			got := env.observe()

			if slices.Contains(got.Tags, "flagged") {
				t.Errorf("tags = %v, want \"flagged\" removed", got.Tags)
			}
			if slices.Contains(splitFlags(got.Baseline), "\\Flagged") {
				t.Errorf("baseline = %q, still records \\Flagged after the server cleared it", got.Baseline)
			}
		})
	}
}

// TestContractServerReportsAnswered is where the profiles genuinely diverge,
// and it is the reason this matrix exists.
//
// Setup, identical for every profile: the message was ingested un-answered, the
// user replies locally (a "replied" tag), and the provider reports the message
// as still NOT answered on the next sync. That is a truthful report on Gmail,
// which has no \Answered equivalent and silently drops the flag on upload.
//
// On a profile that round-trips \Answered, the server's word is authoritative
// and the three-way merge runs unmodified. On a profile with
// AnsweredUnsupported, the engine pins the server's Answered to the baseline
// (engine.go:988) precisely so the report cannot strip the local tag.
func TestContractServerReportsAnswered(t *testing.T) {
	for _, p := range contractProfiles() {
		t.Run(p.name, func(t *testing.T) {
			env := newContractEnv(t, p.caps, backend.Flags{Seen: true})

			// Sync one: the reply is a local change against a baseline with no
			// \Answered, so every profile uploads it and advances the baseline.
			// The provider's report is irrelevant here, which is why a single
			// sync cannot tell the profiles apart.
			env.localTags([]string{"replied"}, nil)
			env.serverSets(backend.Flags{Seen: true})
			env.sync()

			// Sync two: the local side is now clean against its baseline, so the
			// upload branch is silent and the server's report is the only input.
			// It still says un-answered — truthfully on Gmail, which dropped the
			// flag. This is where the capability decides the outcome.
			env.resetCalls()
			env.serverSets(backend.Flags{Seen: true})
			env.sync()
			got := env.observe()

			want := answeredExpectation(p.caps)
			if slices.Contains(got.Tags, "replied") != want.keepsRepliedTag {
				t.Errorf("tags = %v, want replied-present = %v", got.Tags, want.keepsRepliedTag)
			}
			if slices.Contains(splitFlags(got.Baseline), "\\Answered") != want.baselineRecordsAnswered {
				t.Errorf("baseline = %q, want \\Answered-recorded = %v",
					got.Baseline, want.baselineRecordsAnswered)
			}

			var uploadedAnswered bool
			for _, call := range got.Uploaded {
				if call.add.Answered {
					uploadedAnswered = true
				}
			}
			if uploadedAnswered != want.uploadsAnswered {
				t.Errorf("uploaded %+v, want \\Answered-uploaded = %v",
					got.Uploaded, want.uploadsAnswered)
			}
		})
	}
}

// answeredContract is what one profile is required to do when a local "replied"
// tag meets a server that reports the message as un-answered.
type answeredContract struct {
	// keepsRepliedTag: does the local "replied" tag survive the sync?
	keepsRepliedTag bool
	// baselineRecordsAnswered: does synced_flags come out carrying \Answered?
	baselineRecordsAnswered bool
	// uploadsAnswered: does the engine call ApplyFlags with Answered in add?
	uploadsAnswered bool
}

// answeredExpectation states what the outcome above must be, as a rule over the
// capability rather than a table over the four profiles. ProductionProfiles
// already records which adapter carries which bits; keying this on the profile
// too would state the same fact twice, and would quietly have no opinion about
// a fifth adapter. Branching on the bit means any future backend that declares
// AnsweredUnsupported inherits the contract without an edit here.
func answeredExpectation(caps backend.Capabilities) answeredContract {
	if caps.AnsweredUnsupported {
		// The provider has no answered flag, so its report of "un-answered" is
		// not evidence about the user's reply — it is the only thing the
		// provider can ever say. The engine pins Answered to the baseline so
		// the report cannot strip the tag, and the baseline keeps the \Answered
		// that the first sync's upload recorded.
		return answeredContract{
			keepsRepliedTag:         true,
			baselineRecordsAnswered: true,
			uploadsAnswered:         false,
		}
	}

	// On a provider that round-trips \Answered, the server is authoritative. It
	// acknowledged the upload and still reports the message as un-answered, so
	// either the flag did not stick or another client cleared it; both mean the
	// local tag is stale. Server-wins is the right resolution, and the baseline
	// has to drop \Answered with it — a baseline that kept the flag would read
	// as a local change on the next run and push it back, which is the
	// ping-pong this capability exists to prevent.
	return answeredContract{
		keepsRepliedTag:         false,
		baselineRecordsAnswered: false,
		uploadsAnswered:         false,
	}
}

// TestContractServerSetsSeen: another client reads the message, so the local
// "unread" tag drops, the baseline records \Seen, and nothing goes back to the
// provider. One contract for all four profiles — no capability makes a
// server-side read marker mean something different.
//
// This is the scenario that motivated the initialized flag. A message that
// arrives unread has a legitimately empty baseline. On redelivery the upsert
// updated is_seen while leaving synced_flags alone, and ingest applies only the
// add half of the flag tags by design (ingest.go), so "unread" stayed. The old
// seed then read is_seen — already advanced by this same sync — as the
// before-image, the stale tag looked like a local mark-unread, and the engine
// uploaded a \Seen removal: it told the server to un-read a message the user
// had just read elsewhere. Every profile did this, and on a delta backend it
// happened on every flag change.
//
// Distinguishing "no baseline" from "empty baseline" is what stops the seed
// firing on a row whose emptiness is the truth.
func TestContractServerSetsSeen(t *testing.T) {
	for _, p := range contractProfiles() {
		t.Run(p.name, func(t *testing.T) {
			env := newContractEnv(t, p.caps, backend.Flags{})
			env.resetCalls()

			env.serverSets(backend.Flags{Seen: true})
			env.sync()
			got := env.observe()

			if slices.Contains(got.Tags, "unread") {
				t.Errorf("tags = %v, want \"unread\" dropped after the server read it", got.Tags)
			}
			if !slices.Contains(splitFlags(got.Baseline), "\\Seen") {
				t.Errorf("baseline = %q, want to record \\Seen", got.Baseline)
			}
			for _, call := range got.Uploaded {
				if call.remove.Seen {
					t.Errorf("uploaded %+v: pushed a \\Seen removal for a server-side read", got.Uploaded)
				}
			}
		})
	}
}

// TestContractServerClearsSeen is the mirror: the server reports the message
// unread again, so the "unread" tag must come back and the baseline must stop
// claiming \Seen. A baseline that kept \Seen would make the restored tag look
// like a local mark-unread on the next run and push a \Seen removal the server
// already performed.
func TestContractServerClearsSeen(t *testing.T) {
	for _, p := range contractProfiles() {
		t.Run(p.name, func(t *testing.T) {
			env := newContractEnv(t, p.caps, backend.Flags{Seen: true})
			env.resetCalls()

			env.serverSets(backend.Flags{})
			env.sync()
			got := env.observe()

			if !slices.Contains(got.Tags, "unread") {
				t.Errorf("tags = %v, want to contain \"unread\" after the server cleared \\Seen", got.Tags)
			}
			if slices.Contains(splitFlags(got.Baseline), "\\Seen") {
				t.Errorf("baseline = %q, still records \\Seen after the server cleared it", got.Baseline)
			}
			if n := effectiveUploads(got.Uploaded); n != 0 {
				t.Errorf("uploaded %+v, want no effective upload for a server-side change", got.Uploaded)
			}
		})
	}
}

// deliversServerDeleted names the profiles whose adapter can report a
// server-side \Deleted at all. It matches on the profile's identity rather than
// its capability tuple: the bits do not encode this, so comparing them would
// silently adopt any future adapter that happened to declare the same three
// flags. The ability is not expressible as a capability either — a bit that one
// adapter sets and the engine never branches on is worse than saying plainly
// that this scenario has one applicable column.
var deliversServerDeleted = map[string]bool{"imap": true}

// deliversCompleted names the profiles whose adapter can report $Completed.
// IMAP carries the keyword, Graph maps flagStatus "complete" onto it and JMAP
// reads $completed from its keywords. Gmail has no equivalent and never sets
// the field, so a Gmail cell here would assert an engine response to a state
// that adapter cannot produce.
var deliversCompleted = map[string]bool{"imap": true, "graph": true, "jmap": true}

// TestContractServerSetsDeleted records what the engine actually does with a
// server-side \Deleted, which is deliberately asymmetric: ToTagOps never maps
// \Deleted to a tag (locally "deleted" means moved-to-trash; \Deleted means
// pending expunge, and conflating them once made uploads purge mail), and the
// merge is server-wins for Deleted. So NO tag appears — the download's only
// visible effect is the baseline recording \Deleted alongside \Seen.
//
// The second sync is where that asymmetry used to bite (issue #399). Local tags
// can never express Deleted, so the row read as locally changed against its
// \Deleted baseline and the engine pushed a removal back — un-marking a pending
// expunge another client had set and it had only witnessed. Resolving each flag
// against the baseline keeps Deleted server-owned precisely because a local
// false is an absent representation rather than a decision, so nothing is
// pushed and the baseline keeps the flag.
//
// IMAP only, and not because the other profiles are exempt from the merge —
// they cannot produce the input. Graph reports deletions as @removed delta
// entries and flagsFromGraph never sets Deleted; Gmail and JMAP move to a trash
// label or mailbox. Running this against them would assert an engine response
// to a server state those adapters cannot deliver, which is the fixture-fiction
// this matrix exists to avoid. Expressing it as a capability would mean a bit
// that exactly one adapter ever sets and the engine never branches on.
func TestContractServerSetsDeleted(t *testing.T) {
	for _, p := range contractProfiles() {
		if !deliversServerDeleted[p.name] {
			continue
		}
		t.Run(p.name, func(t *testing.T) {
			env := newContractEnv(t, p.caps, backend.Flags{Seen: true})
			env.resetCalls()

			env.serverSets(backend.Flags{Seen: true, Deleted: true})
			env.sync()
			got := env.observe()

			for _, tag := range []string{"deleted", "trash"} {
				if slices.Contains(got.Tags, tag) {
					t.Errorf("tags = %v, want no %q tag from a server-side \\Deleted", got.Tags, tag)
				}
			}
			if !slices.Contains(splitFlags(got.Baseline), "\\Deleted") {
				t.Errorf("baseline = %q, want to record \\Deleted (server-wins merge)", got.Baseline)
			}
			if n := effectiveUploads(got.Uploaded); n != 0 {
				t.Errorf("uploaded %+v, want no effective upload in the download sync", got.Uploaded)
			}

			env.resetCalls()
			env.sync()
			got = env.observe()

			// Not merely "nothing effective": no call at all, which
			// effectiveUploads would not distinguish. Deleted is now excluded
			// from NeedsUpload, so a \Deleted baseline no longer makes the row
			// an upload candidate at all — before that it was one on every run,
			// each sending an empty ApplyFlags.
			if len(got.Uploaded) != 0 {
				t.Errorf("uploaded %+v, want no ApplyFlags at all for a server-owned \\Deleted", got.Uploaded)
			}
			if !slices.Contains(splitFlags(got.Baseline), "\\Deleted") {
				t.Errorf("baseline = %q, want \\Deleted kept: the server owns it", got.Baseline)
			}
			if !env.fake.flagsByRef[env.ref.ID].Deleted {
				t.Errorf("provider state = %+v, want the pending expunge left alone",
					env.fake.flagsByRef[env.ref.ID])
			}
		})
	}
}

// TestContractCompletedMasksFlagged: the server marks a followed-up message
// completed ($Completed, Outlook's follow-up-done keyword). ToTagOps emits
// "flagged" only for Flagged && !Completed, so the download strips the local
// "flagged" tag even though \Flagged itself is still set — and the baseline
// keeps both \Flagged and $Completed. The second sync is the masking under
// test: the missing tag makes local.Flagged differ from the stored baseline,
// and without the stored.Completed mask in imap.NeedsUpload every subsequent
// run would read that as a local un-flag and push a \Flagged removal, undoing
// the follow-up state on the server. On a delta profile the mask also keeps
// the row out of candidacy entirely, so no flags are fetched at all.
func TestContractCompletedMasksFlagged(t *testing.T) {
	for _, p := range contractProfiles() {
		if !deliversCompleted[p.name] {
			continue
		}
		t.Run(p.name, func(t *testing.T) {
			env := newContractEnv(t, p.caps, backend.Flags{Seen: true, Flagged: true})
			env.resetCalls()

			flags := backend.Flags{Seen: true, Flagged: true, Completed: true}
			env.fake.flagsByRef[env.ref.ID] = flags
			var labels []string
			if p.caps.LabelsAreTags {
				labels = []string{"inbox"}
			}
			env.redeliver(flags, labels)
			env.sync()
			got := env.observe()

			if slices.Contains(got.Tags, "flagged") {
				t.Errorf("tags = %v, want \"flagged\" removed for a completed follow-up", got.Tags)
			}
			for _, want := range []string{"\\Flagged", "$Completed"} {
				if !slices.Contains(splitFlags(got.Baseline), want) {
					t.Errorf("baseline = %q, want to record %s", got.Baseline, want)
				}
			}
			if n := effectiveUploads(got.Uploaded); n != 0 {
				t.Errorf("uploaded %+v, want no effective upload in the download sync", got.Uploaded)
			}

			env.resetCalls()
			env.sync()
			got = env.observe()

			if n := effectiveUploads(got.Uploaded); n != 0 {
				t.Errorf("uploaded %+v, want the Completed mask to suppress the flagged diff", got.Uploaded)
			}
			if p.caps.FlagChangesInDelta {
				// The mask keeps the row from even becoming an upload candidate,
				// so a delta backend has nothing to fetch.
				if len(got.FetchedRefs) != 0 {
					t.Errorf("fetched %v, want no flag fetch for a masked row on a delta profile", got.FetchedRefs)
				}
			} else if !slices.Contains(got.FetchedRefs, env.ref.ID) {
				// IMAP polls regardless; the mask matters after the fetch.
				t.Errorf("fetched %v, want the poll to still cover %s", got.FetchedRefs, env.ref.ID)
			}
			if slices.Contains(got.Tags, "flagged") {
				t.Errorf("tags = %v, want \"flagged\" to stay absent", got.Tags)
			}

			// The follow-up is reopened: the server clears $Completed and the
			// star it never dropped becomes visible again. The local side still
			// reports Flagged=false, but only because the mask suppressed the
			// tag — it is not the user unstarring, and resolving it as one would
			// lose the star both here and, once the mask lifts, on the provider.
			env.resetCalls()
			env.serverSets(backend.Flags{Seen: true, Flagged: true})
			env.sync()
			got = env.observe()

			if !slices.Contains(got.Tags, "flagged") {
				t.Errorf("tags = %v, want the server-owned star restored once $Completed cleared", got.Tags)
			}
			if b := splitFlags(got.Baseline); !slices.Contains(b, "\\Flagged") || slices.Contains(b, "$Completed") {
				t.Errorf("baseline = %q, want \\Flagged kept and $Completed dropped", got.Baseline)
			}
			for _, call := range got.Uploaded {
				if call.remove.Flagged {
					t.Errorf("uploaded %+v: read the masked local absence as a user unstar", got.Uploaded)
				}
			}

			// And the run after that, with the mask no longer in the baseline,
			// must not rediscover the absence as a change either.
			env.resetCalls()
			env.sync()
			got = env.observe()
			if n := effectiveUploads(got.Uploaded); n != 0 {
				t.Errorf("uploaded %+v, want nothing once the star is back in sync", got.Uploaded)
			}
			if !env.fake.flagsByRef[env.ref.ID].Flagged {
				t.Errorf("provider state = %+v, want the star left alone",
					env.fake.flagsByRef[env.ref.ID])
			}
		})
	}
}

// TestContractFirstInsertVsExistingRow: a fresh arrival and an established row
// go through the same flag pass, but only the established one may produce work.
// Ingest seeds a new row's baseline from the flags it arrived with, so its
// first flag pass finds local == baseline == server and does nothing — while
// the seeded row, starred locally before the same sync, is a genuine upload
// candidate. A fresh row whose baseline started empty instead would parse as
// unread/unflagged, read its own arrival flags as local changes, and turn
// every first sync into one spurious upload per message.
func TestContractFirstInsertVsExistingRow(t *testing.T) {
	const freshID = "fresh@example.com"
	const freshRef = "c2"
	for _, p := range contractProfiles() {
		t.Run(p.name, func(t *testing.T) {
			env := newContractEnv(t, p.caps, backend.Flags{Seen: true})
			env.resetCalls()

			env.localTags([]string{"flagged"}, nil)
			var labels []string
			if p.caps.LabelsAreTags {
				labels = []string{"inbox"}
			}
			env.deliverNew(freshID, freshRef, backend.Flags{Seen: true, Flagged: true}, labels)
			env.sync()
			got := env.observe()

			if b := env.baselineFor(freshID); b != "\\Seen,\\Flagged" {
				t.Errorf("fresh baseline = %q, want the arrival flags \\Seen,\\Flagged", b)
			}
			if calls := uploadsFor(got.Uploaded, freshRef); len(calls) != 0 {
				t.Errorf("uploaded %+v for the fresh row, want none (its first flag pass is a no-op)", calls)
			}
			if !slices.Contains(mustTags(t, env.db, freshID), "flagged") {
				t.Errorf("fresh tags missing \"flagged\" from its arrival flags")
			}

			// The established row still behaves normally: its local star is a
			// change against the pre-existing baseline and must go up.
			var effective []applyFlagsCall
			var empty backend.Flags
			for _, call := range got.Uploaded {
				if call.add != empty || call.remove != empty {
					effective = append(effective, call)
				}
			}
			if len(effective) != 1 || effective[0].ref.ID != env.ref.ID || !effective[0].add.Flagged {
				t.Errorf("effective uploads = %+v, want exactly one \\Flagged add for %s", effective, env.ref.ID)
			}
			if got.Baseline != "\\Seen,\\Flagged" {
				t.Errorf("baseline = %q, want \\Seen,\\Flagged after the upload advanced it", got.Baseline)
			}
		})
	}
}

// TestContractMissingLegacyBaseline distinguishes the two meanings of an empty
// synced_flags. A legacy-migrated row (baseline lost, but the stored server
// flags say \Seen) is re-seeded from those flags before candidacy, so it does
// not masquerade as a local change; on a delta profile that must mean no flag
// fetch happens at all — the seeding exists precisely to stop a migrated
// mailbox from fetching flags for every message. An initialized-empty row (the
// message genuinely arrived with no flags) must NOT be seeded: its empty
// baseline is correct, and the proof is that a real local read against it is
// still detected and uploaded rather than swallowed by a bogus seed.
func TestContractMissingLegacyBaseline(t *testing.T) {
	for _, p := range contractProfiles() {
		t.Run(p.name, func(t *testing.T) {
			// Legacy/missing: the row's stored server flags carry \Seen but the
			// baseline is gone (the legacy syncer kept it in its own state file).
			env := newContractEnv(t, p.caps, backend.Flags{Seen: true})
			if err := env.db.SetSyncedFlags(contractMsgID, testAccount, ""); err != nil {
				t.Fatalf("clear baseline: %v", err)
			}
			env.resetCalls()
			env.sync()
			got := env.observe()

			if got.Baseline != "\\Seen" {
				t.Errorf("baseline = %q, want re-seeded to \\Seen from the stored server flags", got.Baseline)
			}
			if n := effectiveUploads(got.Uploaded); n != 0 {
				t.Errorf("uploaded %+v, want none — a re-seeded row is not a local change", got.Uploaded)
			}
			// A row whose baseline was never initialized is reconciled against
			// the provider rather than seeded blind from its stored columns.
			// Those columns hold whatever an earlier sync happened to write;
			// only the provider knows the current state. Delta profiles
			// therefore do fetch it — which is the point of tracking
			// initialization separately from emptiness.
			if p.caps.FlagChangesInDelta && len(got.FetchedRefs) == 0 {
				t.Errorf("fetched nothing, want the uninitialized row reconciled against the provider")
			}

			// Initialized-empty: the message arrived unread with no flags, so the
			// empty baseline is the truth and must survive the sync untouched.
			env = newContractEnv(t, p.caps, backend.Flags{})
			env.resetCalls()
			env.sync()
			if got := env.observe(); got.Baseline != "" {
				t.Errorf("baseline = %q, want empty to stay empty (no flags to seed from)", got.Baseline)
			}

			// The user reads the message. Against the legitimately empty baseline
			// this is a real local change; a seed here would have swallowed it.
			env.localTags(nil, []string{"unread"})
			env.resetCalls()
			env.sync()
			got = env.observe()

			var uploadedSeen bool
			for _, call := range got.Uploaded {
				if call.add.Seen {
					uploadedSeen = true
				}
			}
			if !uploadedSeen {
				t.Errorf("uploaded %+v, want the local read pushed as a \\Seen add", got.Uploaded)
			}
			if got.Baseline != "\\Seen" {
				t.Errorf("baseline = %q, want \\Seen after the upload advanced it", got.Baseline)
			}
		})
	}
}

// TestContractConcurrentLocalMutation races a GUI mark-unread against a sync
// that is downloading a server-side star: the mutation fires inside FetchFlags,
// after the flag pass read its row snapshot and before it writes.
//
// One contract for all four profiles, because a user's action does not become
// discardable depending on which provider they use. The mutation survives, and
// the sync it raced leaves the baseline alone so the next run can merge it
// against the server properly and upload it.
//
// The failure this guards against is silent. ToTagOps writes the merged state
// absolutely, so a blind write reverts the mark-unread — and the baseline
// written immediately after would then agree with the reverted tags, leaving
// nothing for a later run to detect. The user's click disappears with no error
// anywhere.
func TestContractConcurrentLocalMutation(t *testing.T) {
	for _, p := range contractProfiles() {
		t.Run(p.name, func(t *testing.T) {
			env := newContractEnv(t, p.caps, backend.Flags{Seen: true})
			env.resetCalls()

			env.serverSets(backend.Flags{Seen: true, Flagged: true})
			env.fake.beforeFetchFlags = func() {
				env.fake.beforeFetchFlags = nil
				env.localTags([]string{"unread"}, nil)
			}
			env.sync()
			got := env.observe()

			if len(got.FetchedRefs) == 0 {
				t.Fatal("flag pass fetched nothing; the mid-sync mutation never raced anything")
			}
			if !slices.Contains(got.Tags, "unread") {
				t.Errorf("tags = %v, want the mid-sync \"unread\" preserved", got.Tags)
			}
			// The refused write has to leave a trace. Without the ref queued,
			// this scenario would still pass on the strength of the local tag
			// making the row an upload candidate again — the queue would be
			// broken and nothing here would notice.
			if !slices.Contains(got.Pending.Refs, env.ref.ID) {
				t.Errorf("pending refs = %v, want %q queued after the refused write",
					got.Pending.Refs, env.ref.ID)
			}
			if n := effectiveUploads(got.Uploaded); n != 0 {
				t.Errorf("uploaded %+v, want nothing effective during the racing sync itself", got.Uploaded)
			}

			// The racing sync must not advance the baseline past the state it
			// failed to apply. Leaving it at the pre-sync \Seen is what makes
			// the preserved "unread" readable as a pending local change on the
			// next run instead of as already-reconciled.
			if !slices.Contains(splitFlags(got.Baseline), "\\Seen") {
				t.Errorf("baseline = %q, want the pre-race \\Seen left untouched", got.Baseline)
			}

			// The deferred work has to actually happen: the next run sees the
			// preserved mutation as a local change and pushes it.
			env.resetCalls()
			env.sync()
			got = env.observe()

			var removedSeen bool
			for _, call := range got.Uploaded {
				if call.remove.Seen {
					removedSeen = true
				}
				// Nothing here touched the star. It changed on the server and
				// nowhere else, so an upload that removes it is the engine
				// deleting a change it was supposed to be receiving.
				if call.remove.Flagged {
					t.Errorf("uploaded %+v: removed the server's own \\Flagged", got.Uploaded)
				}
			}
			if !removedSeen {
				t.Errorf("uploaded %+v, want the next run to propagate the mark-unread", got.Uploaded)
			}

			// Both changes survive. Seen moved only locally and Flagged only on
			// the server, so each field has exactly one claimant and there is
			// nothing to arbitrate.
			for _, tag := range []string{"unread", "flagged"} {
				if !slices.Contains(got.Tags, tag) {
					t.Errorf("tags = %v, want %q kept: it was changed on exactly one side", got.Tags, tag)
				}
			}
			if b := splitFlags(got.Baseline); slices.Contains(b, "\\Seen") || !slices.Contains(b, "\\Flagged") {
				t.Errorf("baseline = %q, want the resolved state (no \\Seen, \\Flagged)", got.Baseline)
			}
			if final := env.fake.flagsByRef[env.ref.ID]; final.Seen || !final.Flagged {
				t.Errorf("provider state = %+v, want seen cleared and flagged kept", final)
			}
			if len(got.Pending.Refs) != 0 {
				t.Errorf("pending refs = %v, want the queue drained once the retry succeeded", got.Pending.Refs)
			}
		})
	}
}

// TestContractNoFlagsSkipsFlagPass: Options.NoFlags disables only the flag
// pass, not the sync. Ingest must still run — a fresh unread arrival still gets
// its row, its topology tag and its flag-derived "unread" tag — while
// FetchFlags and ApplyFlags are never called on any profile. A NoFlags run that
// still fetched flags would defeat the option's point (parity with the legacy
// --no-flags escape hatch for providers with broken flag endpoints).
func TestContractNoFlagsSkipsFlagPass(t *testing.T) {
	const freshID = "noflags-fresh@example.com"
	for _, p := range contractProfiles() {
		t.Run(p.name, func(t *testing.T) {
			env := newContractEnv(t, p.caps, backend.Flags{Seen: true})
			env.opts.NoFlags = true
			env.resetCalls()

			var labels []string
			if p.caps.LabelsAreTags {
				labels = []string{"inbox"}
			}
			env.deliverNew(freshID, "c2", backend.Flags{}, labels)
			env.sync()
			got := env.observe()

			if len(got.FetchedRefs) != 0 || len(env.fake.fetchFlagsCalls) != 0 {
				t.Errorf("FetchFlags called with %v under NoFlags, want never", got.FetchedRefs)
			}
			if len(got.Uploaded) != 0 {
				t.Errorf("ApplyFlags called %+v under NoFlags, want never", got.Uploaded)
			}
			tags := mustTags(t, env.db, freshID)
			for _, want := range []string{"inbox", "unread"} {
				if !slices.Contains(tags, want) {
					t.Errorf("fresh tags = %v, missing %q — ingest (and its flag-derived tags) must still run", tags, want)
				}
			}
		})
	}
}

// TestContractPartialFlagFetch: the provider resolves the subject message but
// throttles the second one (ErrPartialFlags with a partial map). The resolved
// message must reconcile now — a server-side star lands as tag and baseline —
// while the unresolved one is neither guessed at nor dropped: its local star
// stays un-uploaded, its baseline untouched, and its ref is persisted in
// PendingFlags so the next run retries it. Discarding the partial map would
// stall the resolved message too; forgetting the pending ref would strand the
// local change until the user happened to touch the message again.
func TestContractPartialFlagFetch(t *testing.T) {
	const secondID = "second@example.com"
	const secondRef = "c2"
	for _, p := range contractProfiles() {
		t.Run(p.name, func(t *testing.T) {
			env := newContractEnv(t, p.caps, backend.Flags{Seen: true})
			var labels []string
			if p.caps.LabelsAreTags {
				labels = []string{"inbox"}
			}
			env.deliverNew(secondID, secondRef, backend.Flags{Seen: true}, labels)
			env.sync()
			env.resetCalls()

			// The server stars the subject message; the user stars the second one
			// locally. Only the subject ref resolves in the partial fetch.
			env.serverSets(backend.Flags{Seen: true, Flagged: true})
			env.localTagsFor(secondID, []string{"flagged"}, nil)
			env.fake.fetchFlagsErr = fmt.Errorf("throttled: %w", backend.ErrPartialFlags)
			env.fake.fetchFlagsPartial = true
			env.fake.fetchFlagsResolvable = map[string]bool{env.ref.ID: true}
			env.sync()
			got := env.observe()

			if !slices.Contains(got.Tags, "flagged") {
				t.Errorf("tags = %v, want the resolved message's server star downloaded", got.Tags)
			}
			if !slices.Contains(splitFlags(got.Baseline), "\\Flagged") {
				t.Errorf("baseline = %q, want \\Flagged recorded for the resolved message", got.Baseline)
			}
			if !slices.Equal(got.Pending.Refs, []string{secondRef}) {
				t.Errorf("pending refs = %v, want exactly [%s] queued for the next run", got.Pending.Refs, secondRef)
			}
			if calls := uploadsFor(got.Uploaded, secondRef); len(calls) != 0 {
				t.Errorf("uploaded %+v for the unresolved message, want its upload deferred", calls)
			}
			if b := env.baselineFor(secondID); b != "\\Seen" {
				t.Errorf("unresolved baseline = %q, want untouched \\Seen", b)
			}

			// The endpoint recovers: the queued ref is retried, the deferred local
			// star goes up, and the queue drains.
			env.fake.fetchFlagsErr = nil
			env.fake.fetchFlagsPartial = false
			env.fake.fetchFlagsResolvable = nil
			env.resetCalls()
			env.sync()
			got = env.observe()

			calls := uploadsFor(got.Uploaded, secondRef)
			if len(calls) != 1 || !calls[0].add.Flagged {
				t.Errorf("uploaded %+v for the retried message, want one \\Flagged add", calls)
			}
			if b := env.baselineFor(secondID); b != "\\Seen,\\Flagged" {
				t.Errorf("retried baseline = %q, want \\Seen,\\Flagged after the upload", b)
			}
			if len(got.Pending.Refs) != 0 || got.Pending.FullScan {
				t.Errorf("pending = %+v after the retry, want drained", got.Pending)
			}
		})
	}
}

// TestContractUploadOnlyDoesNotBankUndownloadedServerFlags: the watcher's
// upload-only pass pushes a local mark-unread while the server holds a star
// this run is forbidden to download. The push itself is the easy half — the
// contract under test is the baseline it leaves behind. A baseline field is a
// claim that both sides agreed on that value, and an upload-only run never
// wrote \Flagged locally, so a baseline that records it is a false claim. The
// second sync is where the lie is consumed: the banked star makes the
// locally-absent "flagged" read as the user un-starring, and the next
// upload-only pass deletes from the provider a change it only ever witnessed.
func TestContractUploadOnlyDoesNotBankUndownloadedServerFlags(t *testing.T) {
	for _, p := range contractProfiles() {
		t.Run(p.name, func(t *testing.T) {
			env := newContractEnv(t, p.caps, backend.Flags{Seen: true})
			env.resetCalls()

			env.localTags([]string{"unread"}, nil)
			env.serverSets(backend.Flags{Seen: true, Flagged: true})
			env.opts.Mode = UploadOnly
			env.sync()
			got := env.observe()

			// The upload touches only the flag the user moved. Seen goes down
			// because it changed locally; Flagged changed only on the server, and
			// diffing without the baseline is what once made its local absence
			// read as a removal to push.
			if final := env.fake.flagsByRef[env.ref.ID]; final.Seen || !final.Flagged {
				t.Errorf("provider state = %+v, want the mark-unread pushed and the server's star untouched", final)
			}
			// UploadOnly skips ingest and the download branch entirely, so the
			// local side must be exactly as the user left it.
			if !slices.Contains(got.Tags, "unread") {
				t.Errorf("tags = %v, want \"unread\" kept — no download ran to reconcile it", got.Tags)
			}
			if slices.Contains(splitFlags(got.Baseline), "\\Flagged") {
				t.Errorf("baseline = %q, banked \\Flagged that was never applied locally", got.Baseline)
			}

			// The regression run. With an honest baseline the second pass finds
			// local == baseline and pushes nothing; with the star banked it
			// pushes a \Flagged removal and the provider loses the change.
			env.resetCalls()
			env.sync()
			if final := env.fake.flagsByRef[env.ref.ID]; !final.Flagged {
				t.Errorf("provider state = %+v after second upload-only run, want the star still set", final)
			}
		})
	}
}

// TestContractDownloadOnlyDoesNotBankUnuploadedLocalFlags is the mirror: the
// download-only pass brings the server's star down while the local mark-unread
// is forbidden to travel up. The baseline may advance Flagged — that change was
// carried out — but not Seen, whose change went nowhere. Banking the un-upload
// would erase the only record that the mark-unread is still owed to the server:
// the next bidirectional run would find local == baseline, read the server's
// unchanged Seen=true as fresh, and quietly re-read the message under the user.
func TestContractDownloadOnlyDoesNotBankUnuploadedLocalFlags(t *testing.T) {
	for _, p := range contractProfiles() {
		t.Run(p.name, func(t *testing.T) {
			env := newContractEnv(t, p.caps, backend.Flags{Seen: true})
			env.resetCalls()

			env.localTags([]string{"unread"}, nil)
			env.serverSets(backend.Flags{Seen: true, Flagged: true})
			env.opts.Mode = DownloadOnly
			env.sync()
			got := env.observe()

			// The star comes down, and the download writes the resolved state —
			// which keeps the local mark-unread, because Seen moved only locally
			// and the merge has no reason to take the server's value.
			for _, tag := range []string{"flagged", "unread"} {
				if !slices.Contains(got.Tags, tag) {
					t.Errorf("tags = %v, want %q after the download-only run", got.Tags, tag)
				}
			}
			// Seen must stay at the server's word: nothing was uploaded, so the
			// mark-unread is still pending and the baseline is the only place
			// that knows.
			if !slices.Contains(splitFlags(got.Baseline), "\\Seen") {
				t.Errorf("baseline = %q, banked the un-uploaded mark-unread as reconciled", got.Baseline)
			}

			// The owed upload happens on the next full sync. If the first run
			// had banked Seen=false, this run would instead see a server-side
			// "change" to Seen=true and revert the user's mark-unread locally.
			env.opts.Mode = Bidirectional
			env.resetCalls()
			env.sync()
			got = env.observe()

			if !slices.Contains(got.Tags, "unread") {
				t.Errorf("tags = %v, want \"unread\" kept — the local change must win, not vanish", got.Tags)
			}
			var removedSeen bool
			for _, call := range got.Uploaded {
				if call.remove.Seen {
					removedSeen = true
				}
			}
			if !removedSeen {
				t.Errorf("uploaded %+v, want the deferred mark-unread pushed as a \\Seen removal", got.Uploaded)
			}
		})
	}
}

// TestContractUploadSucceedsThenLocalWriteRefused splits one row's sync down
// the middle: the upload of the local mark-unread succeeds, then a mid-sync
// reply makes the CAS refuse the download of the server's star. pushed and
// pulled are now different answers for the same row, and the baseline must
// follow them per flag — Seen advanced (its change reached the server),
// Flagged did not (its change never reached the local tags). A single
// wholesale write of the resolved state can only get one of them wrong: it
// would bank \Flagged, and the retry the pending queue schedules would find
// local == baseline and never deliver the star.
func TestContractUploadSucceedsThenLocalWriteRefused(t *testing.T) {
	for _, p := range contractProfiles() {
		t.Run(p.name, func(t *testing.T) {
			env := newContractEnv(t, p.caps, backend.Flags{Seen: true})
			env.resetCalls()

			env.localTags([]string{"unread"}, nil)
			env.serverSets(backend.Flags{Seen: true, Flagged: true})
			env.fake.beforeFetchFlags = func() {
				env.fake.beforeFetchFlags = nil
				env.localTags([]string{"replied"}, nil)
			}
			env.sync()
			got := env.observe()

			if len(got.FetchedRefs) == 0 {
				t.Fatal("flag pass fetched nothing; the mid-sync mutation never raced anything")
			}
			// The upload half went through before the race mattered: the
			// mark-unread reached the provider and the star was left alone.
			if final := env.fake.flagsByRef[env.ref.ID]; final.Seen || !final.Flagged {
				t.Errorf("provider state = %+v, want \\Seen removed and the star untouched", final)
			}
			// The download half was refused. ToTagOps is absolute — its write
			// would have stripped the mid-sync "replied" — so the refusal is
			// visible as both local changes surviving and the star's tag absent.
			for _, tag := range []string{"unread", "replied"} {
				if !slices.Contains(got.Tags, tag) {
					t.Errorf("tags = %v, want %q to survive the refused write", got.Tags, tag)
				}
			}
			if slices.Contains(got.Tags, "flagged") {
				t.Errorf("tags = %v, want no \"flagged\" — the download was refused, not applied", got.Tags)
			}
			// The half-run baseline: only the pushed flag advances. Recording
			// \Flagged here would mark the refused download as done and starve
			// the retry below of any difference to act on.
			if slices.Contains(splitFlags(got.Baseline), "\\Flagged") {
				t.Errorf("baseline = %q, banked a server change the refused write never applied", got.Baseline)
			}
			if !slices.Contains(got.Pending.Refs, env.ref.ID) {
				t.Errorf("pending refs = %v, want %q queued so the refused download is retried",
					got.Pending.Refs, env.ref.ID)
			}
		})
	}
}

// TestContractTagSourceFollowsProfile: the capability, not the folder, decides
// where a message's organizational tags come from. The arrival carries a label
// ("projects") that no folder role maps to, and deliberately does NOT carry an
// inbox label. On a LabelsAreTags profile the labels are the truth: "projects"
// is mirrored to a tag and no "inbox" tag appears, because the RoleAll stream
// contributes nothing. On a folder profile Message.Labels must be ignored
// outright — the inbox role supplies "inbox", and a leaked "projects" tag here
// would mean label data from a provider that models folders bled into tags.
func TestContractTagSourceFollowsProfile(t *testing.T) {
	const labelledID = "labelled@example.com"
	for _, p := range contractProfiles() {
		t.Run(p.name, func(t *testing.T) {
			env := newContractEnv(t, p.caps, backend.Flags{Seen: true})
			env.resetCalls()

			env.deliverNew(labelledID, "c2", backend.Flags{Seen: true}, []string{"projects"})
			env.sync()
			tags := mustTags(t, env.db, labelledID)

			if p.caps.LabelsAreTags {
				if !slices.Contains(tags, "projects") {
					t.Errorf("tags = %v, want the label mirrored to a \"projects\" tag", tags)
				}
				if slices.Contains(tags, "inbox") {
					t.Errorf("tags = %v, want no \"inbox\" tag — the RoleAll stream must not imply one", tags)
				}
			} else {
				if !slices.Contains(tags, "inbox") {
					t.Errorf("tags = %v, want \"inbox\" from the folder role", tags)
				}
				if slices.Contains(tags, "projects") {
					t.Errorf("tags = %v, want Message.Labels ignored on a folder profile", tags)
				}
			}
		})
	}
}
