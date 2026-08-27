package syncengine

import (
	"context"
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
		// The label vocabulary is what the provider round-trips as a real
		// label. It follows from the capability rather than being a fixed list:
		// AnsweredUnsupported means there is no answered label to mirror, so
		// listing "replied" here would let the label pass delete a tag the flag
		// pass just went out of its way to protect.
		fake.labelVocab = []string{"inbox", "unread", "flagged"}
		if !caps.AnsweredUnsupported {
			fake.labelVocab = append(fake.labelVocab, "replied")
		}
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
	// The fake indexes its script by call count, so a new page has to be
	// appended; replacing the slice would leave the index past the end and the
	// folder would report itself unchanged.
	e.fake.scripts[e.folder.Name] = append(e.fake.scripts[e.folder.Name], backend.FetchResult{
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
	if err := e.db.ModifyTagsByMessageIDAndAccount(contractMsgID, testAccount, add, remove); err != nil {
		e.t.Fatalf("modify tags: %v", err)
	}
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
			// A server-side change must not travel back as a local one. The
			// engine may still issue an ApplyFlags whose add and remove are both
			// empty: on a folder profile the redelivered page writes the new flag
			// tags at ingest, so the flag pass reads the row as locally changed
			// against a baseline that predates the page, and computes an empty
			// diff against the server. Harmless on the wire, but a wasted round
			// trip; the contract here is only that nothing effective is pushed.
			if n := effectiveUploads(got.Uploaded); n != 0 {
				t.Errorf("uploaded %+v, want no effective upload for a server-side change", got.Uploaded)
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
