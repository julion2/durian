package syncengine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/dbcrypto"
	"github.com/julion2/durian/cli/internal/store"
)

// mutateAfterFlagFetchBackend models the smallest unprotected provider race:
// FetchFlags returns a coherent old view, another client changes the provider,
// then Durian applies its local mutation without a MODSEQ/state precondition.
type mutateAfterFlagFetchBackend struct {
	*fakeBackend
	mutate func()
}

func (b *mutateAfterFlagFetchBackend) FetchFlags(ctx context.Context, folder string, refs []backend.RemoteRef) (map[string]backend.Flags, error) {
	flags, err := b.fakeBackend.FetchFlags(ctx, folder, refs)
	if b.mutate != nil {
		mutate := b.mutate
		b.mutate = nil
		mutate()
	}
	return flags, err
}

func syncAdversarial(t *testing.T, opts Options, b backend.Backend) *Result {
	t.Helper()
	result, err := New(opts).Sync(t.Context(), b)
	if err != nil {
		t.Fatalf("Sync returned top-level error: %v", err)
	}
	return result
}

// A remote mutation landing after FetchFlags cannot be detected without a
// provider precondition. These cases establish the behavior the current
// add/remove API can still guarantee: an unrelated late flag is not clobbered,
// remains observable on the next delta/poll, and both sides then converge.
func TestAdversarialLateRemoteMutationDoesNotClobberIndependentLocalIntent(t *testing.T) {
	tests := []struct {
		name       string
		localAdd   []string
		lateRemote backend.Flags
		wantFirst  []string
		wantFinal  []string
	}{
		{
			name:       "remote read after fetch while local star pending",
			localAdd:   []string{"flagged"},
			lateRemote: backend.Flags{Seen: true},
			wantFirst:  []string{"flagged", "inbox", "unread"},
			wantFinal:  []string{"flagged", "inbox"},
		},
		{
			name:       "remote star after fetch while local read pending",
			localAdd:   nil,
			lateRemote: backend.Flags{Flagged: true},
			wantFirst:  []string{"inbox"},
			wantFinal:  []string{"flagged", "inbox"},
		},
	}

	for _, p := range contractProfiles() {
		for _, tt := range tests {
			t.Run(p.name+"/"+tt.name, func(t *testing.T) {
				env := newContractEnv(t, p.caps, backend.Flags{})
				env.resetCalls()
				if tt.name == "remote star after fetch while local read pending" {
					env.localTags(nil, []string{"unread"})
				} else {
					env.localTags(tt.localAdd, nil)
				}

				wrapped := &mutateAfterFlagFetchBackend{fakeBackend: env.fake}
				wrapped.mutate = func() {
					env.fake.flagsByRef[env.ref.ID] = tt.lateRemote
				}
				result := syncAdversarial(t, env.opts, wrapped)
				if len(result.Errors) != 0 {
					t.Fatalf("racing sync errors = %v", result.Errors)
				}

				first := slices.Clone(mustTags(t, env.db, contractMsgID))
				slices.Sort(first)
				if !slices.Equal(first, tt.wantFirst) {
					t.Fatalf("tags immediately after stale fetch = %v, want %v", first, tt.wantFirst)
				}
				provider := env.fake.flagsByRef[env.ref.ID]
				if !provider.Seen || !provider.Flagged {
					t.Fatalf("provider state after independent mutations = %+v, want Seen+Flagged", provider)
				}
				for _, call := range env.fake.applyFlagsCalls {
					if (tt.lateRemote.Seen && call.remove.Seen) || (tt.lateRemote.Flagged && call.remove.Flagged) {
						t.Fatalf("ApplyFlags clobbered late remote state: %+v", call)
					}
				}

				// Delta profiles need the late mutation delivered; IMAP observes
				// it by polling, but redelivery keeps the sequence identical.
				env.serverSets(provider)
				env.resetCalls()
				env.sync()
				final := slices.Clone(mustTags(t, env.db, contractMsgID))
				slices.Sort(final)
				if !slices.Equal(final, tt.wantFinal) {
					t.Fatalf("stabilized tags = %v, want %v", final, tt.wantFinal)
				}
				if n := effectiveUploads(env.fake.applyFlagsCalls); n != 0 {
					t.Fatalf("late remote state was reconstructed as local intent: %+v", env.fake.applyFlagsCalls)
				}
			})
		}
	}
}

func TestAdversarialSecondClientDeletesArrivalDuringDeltaPaging(t *testing.T) {
	for _, p := range contractProfiles() {
		t.Run(p.name, func(t *testing.T) {
			db := newTestDB(t)
			folder := backend.Folder{Name: "Projects", Role: backend.RoleNone, Selectable: true}
			victim := backend.Message{
				MessageID: "paged-victim@example.com", Ref: backend.RemoteRef{Folder: folder.Name, ID: "victim"},
				Raw: rawMessage("paged-victim@example.com", "a@example.com", testAccount, "Victim", "body"),
			}
			survivor := backend.Message{
				MessageID: "paged-survivor@example.com", Ref: backend.RemoteRef{Folder: folder.Name, ID: "survivor"},
				Raw: rawMessage("paged-survivor@example.com", "a@example.com", testAccount, "Survivor", "body"),
			}
			fake := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
				folder.Name: {
					{Messages: []backend.Message{victim}, Cursor: backend.Cursor("page-1"), HasMore: true},
					{Messages: []backend.Message{survivor}, Deleted: []backend.Deletion{{Ref: victim.Ref}}, Cursor: backend.Cursor("final")},
				},
			})
			fake.caps = p.caps

			result := syncAdversarial(t, Options{
				Store: db, Cursors: newMemCursorStore(), Account: testAccount,
				Ingest: IngestOptions{Account: testAccount},
			}, fake)
			if len(result.Errors) != 0 {
				t.Fatalf("sync errors = %v", result.Errors)
			}
			if got, _ := db.GetByMessageID(victim.MessageID); got != nil {
				t.Fatalf("same-run arrival deleted on page 2 survived: %+v", got)
			}
			if got, _ := db.GetByMessageID(survivor.MessageID); got == nil {
				t.Fatal("page-2 survivor was lost")
			}
			if got := fake.seenCursors[folder.Name]; len(got) != 2 || string(got[1]) != "page-1" {
				t.Fatalf("paging cursors = %q, want [empty page-1]", got)
			}
		})
	}
}

func TestAdversarialSecondClientMovesArrivalDuringDeltaPaging(t *testing.T) {
	for _, p := range contractProfiles() {
		t.Run(p.name, func(t *testing.T) {
			db := newTestDB(t)
			inbox := backend.Folder{Name: "INBOX", Role: backend.RoleInbox, Selectable: true}
			archive := backend.Folder{Name: "Archive", Role: backend.RoleArchive, Selectable: true}
			inboxCopy := backend.Message{
				MessageID: "paged-move@example.com", Ref: backend.RemoteRef{Folder: inbox.Name, ID: "old-ref"},
				Raw: rawMessage("paged-move@example.com", "a@example.com", testAccount, "Moved", "body"),
			}
			archiveCopy := inboxCopy
			archiveCopy.Ref = backend.RemoteRef{Folder: archive.Name, ID: "new-ref"}
			fake := newFakeBackend([]backend.Folder{inbox, archive}, map[string][]backend.FetchResult{
				inbox.Name: {
					{Messages: []backend.Message{inboxCopy}, Cursor: backend.Cursor("inbox-page-1"), HasMore: true},
					{Deleted: []backend.Deletion{{Ref: inboxCopy.Ref, MessageID: inboxCopy.MessageID}}, Cursor: backend.Cursor("inbox-final")},
				},
				archive.Name: {{Messages: []backend.Message{archiveCopy}, Cursor: backend.Cursor("archive-final")}},
			})
			fake.caps = p.caps

			result := syncAdversarial(t, Options{
				Store: db, Cursors: newMemCursorStore(), Account: testAccount,
				Ingest: IngestOptions{Account: testAccount},
			}, fake)
			if len(result.Errors) != 0 {
				t.Fatalf("sync errors = %v", result.Errors)
			}
			got, err := db.GetByMessageID(inboxCopy.MessageID)
			if err != nil || got == nil {
				t.Fatalf("moved message disappeared: message=%+v err=%v", got, err)
			}
			if got.Mailbox != archive.Name || got.RemoteRef != archiveCopy.Ref.ID {
				t.Fatalf("moved message location = %s/%s, want %s/%s", got.Mailbox, got.RemoteRef, archive.Name, archiveCopy.Ref.ID)
			}
			if len(fake.moveCalls) != 0 {
				t.Fatalf("downloaded second-client move was uploaded again: %+v", fake.moveCalls)
			}
		})
	}
}

type failNextCursorCommit struct {
	CursorStore
	failures int
}

func (s *failNextCursorCommit) Commit(account, folder string, cursor backend.Cursor, pending PendingFlags) error {
	if s.failures > 0 {
		s.failures--
		return errors.New("injected durable cursor commit interruption")
	}
	return s.CursorStore.Commit(account, folder, cursor, pending)
}

func TestAdversarialJMAPSnapshotRestartUsesDurableSQLiteCheckpoint(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	keyring, err := dbcrypto.NewKeyring(bytes.Repeat([]byte{0x51}, dbcrypto.MasterKeyLen))
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "mail.db")
	db, err := store.Open(dbPath, keyring)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Init(); err != nil {
		db.Close()
		t.Fatal(err)
	}

	folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
	message := func(id, ref string) backend.Message {
		return backend.Message{
			StableID: ref, MessageID: id, Ref: backend.RemoteRef{Folder: folder.Name, ID: ref},
			Raw: rawMessage(id, "a@example.com", testAccount, "Snapshot", "body"),
		}
	}
	keep := message("durable-keep@example.com", "keep")
	stale := message("durable-stale@example.com", "stale")
	added := message("durable-added@example.com", "added")
	for _, msg := range []backend.Message{keep, stale} {
		if _, _, _, err := Ingest(db, msg, folder.Name, folder.Role, IngestOptions{Account: testAccount}); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}

	realCursors := NewFileCursorStoreWithSuffix(testAccount, "-adversarial-jmap")
	if err := realCursors.Commit(testAccount, folder.Name, backend.Cursor("base"), PendingFlags{}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	failing := &failNextCursorCommit{CursorStore: realCursors, failures: 1}
	fake := newFakeBackend([]backend.Folder{folder}, nil)
	fake.caps = backend.ProfileJMAP
	fake.flagsByRef[keep.Ref.ID] = backend.Flags{}
	fake.flagsByRef[added.Ref.ID] = backend.Flags{}
	fake.fetchByCursor = func(_ string, cursor backend.Cursor) backend.FetchResult {
		switch string(cursor) {
		case "base":
			return backend.FetchResult{
				Messages: []backend.Message{keep}, Present: []backend.RemoteRef{keep.Ref},
				Cursor: backend.Cursor("page-1"), HasMore: true, FullSnapshot: true,
			}
		case "page-1":
			return backend.FetchResult{
				Messages: []backend.Message{added}, Present: []backend.RemoteRef{added.Ref},
				Cursor: backend.Cursor("final"), FullSnapshot: true,
			}
		default:
			return backend.FetchResult{Cursor: cursor}
		}
	}

	result := syncAdversarial(t, Options{
		Store: db, Cursors: failing, Account: testAccount,
		Ingest: IngestOptions{Account: testAccount},
	}, fake)
	if len(result.Errors) != 1 {
		db.Close()
		t.Fatalf("interrupted sync errors = %v, want cursor commit failure", result.Errors)
	}
	staged, err := db.GetSnapshotState(testAccount, folder.Name)
	if err != nil || !staged.Active || staged.Complete || string(staged.CheckpointCursor) != "page-1" {
		db.Close()
		t.Fatalf("durable staged checkpoint = %+v, err=%v", staged, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen both durable stores to model a new process, not merely a second
	// Engine value sharing in-memory state.
	db, err = store.Open(dbPath, keyring)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	freshCursors := NewFileCursorStoreWithSuffix(testAccount, "-adversarial-jmap")
	result = syncAdversarial(t, Options{
		Store: db, Cursors: freshCursors, Account: testAccount,
		Ingest: IngestOptions{Account: testAccount},
	}, fake)
	if len(result.Errors) != 0 {
		t.Fatalf("restart errors = %v", result.Errors)
	}
	if got := fake.seenCursors[folder.Name]; len(got) != 2 || string(got[0]) != "base" || string(got[1]) != "page-1" {
		t.Fatalf("provider cursors across restart = %q, want [base page-1]", got)
	}
	state, err := freshCursors.GetState(testAccount, folder.Name)
	if err != nil || string(state.Cursor) != "final" || state.PendingFlags.SnapshotInProgress {
		t.Fatalf("final durable cursor state = %+v, err=%v", state, err)
	}
	staged, err = db.GetSnapshotState(testAccount, folder.Name)
	if err != nil || staged.Active {
		t.Fatalf("snapshot staging after restart = %+v, err=%v", staged, err)
	}
	if got, _ := db.GetByMessageID(stale.MessageID); got != nil {
		t.Fatalf("stale pre-snapshot row survived restart: %+v", got)
	}
	for _, msg := range []backend.Message{keep, added} {
		if got, _ := db.GetByMessageID(msg.MessageID); got == nil {
			t.Fatalf("snapshot row %s missing after restart", msg.MessageID)
		}
	}
}

func TestAdversarialServerOnlyReadIsNotReconstructedAfterPartialFlagRetry(t *testing.T) {
	for _, p := range contractProfiles() {
		t.Run(p.name, func(t *testing.T) {
			env := newContractEnv(t, p.caps, backend.Flags{})
			env.resetCalls()
			env.serverSets(backend.Flags{Seen: true})
			env.fake.fetchFlagsPartial = true
			env.fake.fetchFlagsErr = fmt.Errorf("%w: second client raced the batch", backend.ErrPartialFlags)

			env.sync()
			first := env.observe()
			if slices.Contains(first.Tags, "unread") || !slices.Contains(splitFlags(first.Baseline), "\\Seen") {
				t.Fatalf("server-only read was not applied: tags=%v baseline=%q", first.Tags, first.Baseline)
			}
			if !slices.Contains(first.Pending.Refs, env.ref.ID) {
				t.Fatalf("partial fetch did not retain %q: %+v", env.ref.ID, first.Pending)
			}

			env.fake.fetchFlagsErr = nil
			env.fake.fetchFlagsPartial = false
			env.resetCalls()
			env.sync()
			final := env.observe()
			if len(final.Pending.Refs) != 0 || final.Pending.FullScan {
				t.Fatalf("pending flag work after successful retry = %+v", final.Pending)
			}
			for _, call := range final.Uploaded {
				if call.remove.Seen {
					t.Fatalf("retry reconstructed server read as local mark-unread: %+v", final.Uploaded)
				}
			}
		})
	}
}

func TestAdversarialEmptyDeltaPageIsNotDeletion(t *testing.T) {
	for _, p := range contractProfiles() {
		t.Run(p.name, func(t *testing.T) {
			db := newTestDB(t)
			folder := backend.Folder{Name: "Projects", Role: backend.RoleNone, Selectable: true}
			msg := backend.Message{
				MessageID: "empty-page@example.com", Ref: backend.RemoteRef{Folder: folder.Name, ID: "r1"},
				Raw: rawMessage("empty-page@example.com", "a@example.com", testAccount, "Empty page", "body"),
			}
			fake := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
				folder.Name: {{Messages: []backend.Message{msg}, Cursor: backend.Cursor("seed")}},
			})
			fake.caps = p.caps
			cursors := newMemCursorStore()
			opts := Options{Store: db, Cursors: cursors, Account: testAccount, Ingest: IngestOptions{Account: testAccount}}
			if result := syncAdversarial(t, opts, fake); len(result.Errors) != 0 {
				t.Fatalf("seed errors = %v", result.Errors)
			}

			fake.scripts[folder.Name] = append(fake.scripts[folder.Name],
				backend.FetchResult{Cursor: backend.Cursor("empty-progress"), HasMore: true},
				backend.FetchResult{Cursor: backend.Cursor("quiet-final")},
			)
			if result := syncAdversarial(t, opts, fake); len(result.Errors) != 0 {
				t.Fatalf("empty-page errors = %v", result.Errors)
			}
			if got, _ := db.GetByMessageID(msg.MessageID); got == nil {
				t.Fatal("empty advancing page was treated as authoritative absence")
			}

			fake.scripts[folder.Name] = append(fake.scripts[folder.Name], backend.FetchResult{
				Deleted: []backend.Deletion{{Ref: msg.Ref, MessageID: msg.MessageID}}, Cursor: backend.Cursor("deleted"),
			})
			delete(fake.flagsByRef, msg.Ref.ID)
			if result := syncAdversarial(t, opts, fake); len(result.Errors) != 0 {
				t.Fatalf("deletion errors = %v", result.Errors)
			}
			if got, _ := db.GetByMessageID(msg.MessageID); got != nil {
				t.Fatalf("explicit provider deletion was ignored: %+v", got)
			}

			fake.applyFlagsCalls = nil
			if result := syncAdversarial(t, opts, fake); len(result.Errors) != 0 {
				t.Fatalf("post-delete no-op errors = %v", result.Errors)
			}
			if got, _ := db.GetByMessageID(msg.MessageID); got != nil {
				t.Fatalf("no-op sync resurrected deleted row: %+v", got)
			}
			if n := effectiveUploads(fake.applyFlagsCalls); n != 0 {
				t.Fatalf("post-delete no-op wrote provider flags: %+v", fake.applyFlagsCalls)
			}
		})
	}
}

// FuzzAdversarialFlagSequenceProperties drives valid local edits, remote edits,
// sync boundaries and partial FetchFlags failures through every named profile.
// The seed corpus is intentionally readable so any regression can be replayed
// with ordinary `go test -run=FuzzAdversarialFlagSequenceProperties/<hash>`.
func FuzzAdversarialFlagSequenceProperties(f *testing.F) {
	f.Add([]byte{0, 0, 4, 2, 4})
	f.Add([]byte{1, 1, 3, 5, 4, 0})
	f.Add([]byte{2, 2, 3, 0, 1, 4, 7})
	f.Add([]byte{3, 3, 3, 2, 2, 1, 5, 4})

	f.Fuzz(func(t *testing.T, actions []byte) {
		profiles := contractProfiles()
		profileIndex := 0
		if len(actions) > 0 {
			profileIndex = int(actions[0]) % len(profiles)
			actions = actions[1:]
		}
		if len(actions) > 32 {
			actions = actions[:32]
		}
		profile := profiles[profileIndex]
		db := newTestDB(t)
		folder := backend.Folder{Name: "Fuzz", Role: backend.RoleNone, Selectable: true}
		const messageID = "fuzz-sequence@example.com"
		ref := backend.RemoteRef{Folder: folder.Name, ID: "fuzz-ref"}
		messageWithFlags := func(flags backend.Flags) backend.Message {
			return backend.Message{
				MessageID: messageID, Ref: ref, Flags: flags,
				Raw: rawMessage(messageID, "a@example.com", testAccount, "Fuzz", "body"),
			}
		}
		fake := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
			folder.Name: {{Messages: []backend.Message{messageWithFlags(backend.Flags{})}, Cursor: backend.Cursor("seed")}},
		})
		fake.caps = profile.caps
		fake.flagsByRef[ref.ID] = backend.Flags{}
		cursors := newMemCursorStore()
		opts := Options{Store: db, Cursors: cursors, Account: testAccount, Ingest: IngestOptions{Account: testAccount}}
		run := func(allowErrors bool) {
			t.Helper()
			result := syncAdversarial(t, opts, fake)
			if !allowErrors && len(result.Errors) != 0 {
				t.Fatalf("profile %s actions %v: sync errors = %v", profile.name, actions, result.Errors)
			}
		}
		run(false)
		redeliver := func(flags backend.Flags) {
			script := fake.scripts[folder.Name]
			fake.calls[folder.Name] = len(script)
			fake.scripts[folder.Name] = append(script, backend.FetchResult{
				Messages: []backend.Message{messageWithFlags(flags)},
				Cursor:   backend.Cursor(fmt.Sprintf("remote-%d", len(script))),
			})
		}
		toggleTag := func(tag string) {
			tags := mustTags(t, db, messageID)
			if slices.Contains(tags, tag) {
				if err := db.ModifyTagsByMessageIDAndAccount(messageID, testAccount, nil, []string{tag}); err != nil {
					t.Fatal(err)
				}
			} else if err := db.ModifyTagsByMessageIDAndAccount(messageID, testAccount, []string{tag}, nil); err != nil {
				t.Fatal(err)
			}
		}

		for _, action := range actions {
			switch action % 8 {
			case 0:
				toggleTag("unread")
			case 1:
				toggleTag("flagged")
			case 2:
				state := fake.flagsByRef[ref.ID]
				state.Seen = !state.Seen
				fake.flagsByRef[ref.ID] = state
				redeliver(state)
			case 3:
				state := fake.flagsByRef[ref.ID]
				state.Flagged = !state.Flagged
				fake.flagsByRef[ref.ID] = state
				redeliver(state)
			case 4:
				run(false)
			case 5:
				fake.fetchFlagsPartial = true
				fake.fetchFlagsErr = fmt.Errorf("%w: fuzzed partial batch", backend.ErrPartialFlags)
				run(true)
				fake.fetchFlagsErr = nil
				fake.fetchFlagsPartial = false
			case 6:
				// A remote no-op redelivery is valid on change feeds that
				// coalesce multiple provider events into one current state.
				redeliver(fake.flagsByRef[ref.ID])
			case 7:
				run(false)
				run(false)
			}
		}

		// Force one local mutation through a failed pass and a later retry.
		// This makes pending-work durability an invariant of every generated
		// sequence rather than relying on the generator to happen upon it. First
		// settle generated work so the forced toggle is known to differ from the
		// baseline rather than accidentally undoing an unsynced earlier toggle.
		fake.fetchFlagsErr = nil
		fake.fetchFlagsPartial = false
		run(false)
		run(false)
		toggleTag("flagged")
		wantFlagged := slices.Contains(mustTags(t, db, messageID), "flagged")
		fake.fetchFlagsPartial = true
		fake.fetchFlagsErr = fmt.Errorf("%w: forced pending check", backend.ErrPartialFlags)
		run(true)
		pending, err := cursors.GetPendingFlags(testAccount, folder.Name)
		if err != nil || !slices.Contains(pending.Refs, ref.ID) {
			t.Fatalf("profile %s actions %v: local mutation was not retained pending: %+v err=%v", profile.name, actions, pending, err)
		}
		fake.fetchFlagsErr = nil
		fake.fetchFlagsPartial = false
		run(false)
		run(false)

		pending, err = cursors.GetPendingFlags(testAccount, folder.Name)
		if err != nil || len(pending.Refs) != 0 || pending.FullScan {
			t.Fatalf("profile %s actions %v: pending work did not drain: %+v err=%v", profile.name, actions, pending, err)
		}
		provider := fake.flagsByRef[ref.ID]
		local := mustTags(t, db, messageID)
		if provider.Flagged != wantFlagged || slices.Contains(local, "flagged") != provider.Flagged || slices.Contains(local, "unread") == provider.Seen {
			t.Fatalf("profile %s actions %v: did not converge: provider=%+v local=%v wantFlagged=%v", profile.name, actions, provider, local, wantFlagged)
		}

		// An explicit provider deletion is terminal. Two quiet syncs must not
		// write anything or recreate the row from stale local/cursor state.
		script := fake.scripts[folder.Name]
		fake.calls[folder.Name] = len(script)
		fake.scripts[folder.Name] = append(script, backend.FetchResult{
			Deleted: []backend.Deletion{{Ref: ref, MessageID: messageID}},
			Cursor:  backend.Cursor("deleted"),
		})
		delete(fake.flagsByRef, ref.ID)
		run(false)
		if got, _ := db.GetByMessageID(messageID); got != nil {
			t.Fatalf("profile %s actions %v: explicit deletion left row %+v", profile.name, actions, got)
		}
		fake.applyFlagsCalls = nil
		run(false)
		run(false)
		if got, _ := db.GetByMessageID(messageID); got != nil {
			t.Fatalf("profile %s actions %v: quiet sync resurrected %+v", profile.name, actions, got)
		}
		if n := effectiveUploads(fake.applyFlagsCalls); n != 0 {
			t.Fatalf("profile %s actions %v: quiet sync wrote flags: %+v", profile.name, actions, fake.applyFlagsCalls)
		}
	})
}
