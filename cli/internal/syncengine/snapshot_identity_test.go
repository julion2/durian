package syncengine

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/dbcrypto"
	"github.com/julion2/durian/cli/internal/store"
)

func TestEngineReplacementPreservesDistinctSyntheticRowsAndTags(t *testing.T) {
	db := newTestDB(t)
	ingestOptions := IngestOptions{
		Account: testAccount,
		FilterRules: []config.RuleConfig{{
			Name: "first ingest only", Match: "*", AddTags: []string{"first-ingest"},
		}},
	}
	date := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	oldRaw := []byte("From: notifier@example.com\r\n" +
		"To: " + testAccount + "\r\n" +
		"Subject: Identical alert\r\n" +
		"Date: Thu, 27 Aug 2026 10:00:00 +0000\r\n" +
		"Status: RO\r\nX-Status: A\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: application/pdf; name=\"report.pdf\"\r\n" +
		"Content-Disposition: attachment; filename=\"report.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		"JVBERi0xLjQK")
	replacementRaw := bytes.ReplaceAll(oldRaw, []byte("Status: RO\r\nX-Status: A"), []byte("Status: O\r\nX-Status: F"))
	oldIDs := []string{
		"durian-synthetic-2-INBOX@" + testAccount,
		"durian-synthetic-1-INBOX@" + testAccount,
	}
	for i, messageID := range oldIDs {
		ref := fmt.Sprintf("%d", 2-i)
		msg := backend.Message{
			MessageID:    messageID,
			Ref:          backend.RemoteRef{Folder: "INBOX", ID: ref},
			Raw:          oldRaw,
			InternalDate: date,
		}
		if _, _, created, err := Ingest(db, msg, "INBOX", backend.RoleInbox, ingestOptions); err != nil || !created {
			t.Fatalf("seed %s: created=%v err=%v", messageID, created, err)
		}
		stored, _ := db.GetByMessageID(messageID)
		if err := db.AddTag(stored.ID, fmt.Sprintf("local-%d", 2-i)); err != nil {
			t.Fatal(err)
		}
		if err := db.RemoveTag(stored.ID, "first-ingest"); err != nil {
			t.Fatal(err)
		}
	}

	newIDs := []string{
		"durian-synthetic-v2-99-102-INBOX@" + testAccount,
		"durian-synthetic-v2-99-101-INBOX@" + testAccount,
	}
	messages := make([]backend.Message, 0, 2)
	present := make([]backend.RemoteRef, 0, 2)
	for i, messageID := range newIDs {
		ref := backend.RemoteRef{Folder: "INBOX", ID: fmt.Sprintf("%d", 102-i)}
		messages = append(messages, backend.Message{
			MessageID: messageID, Ref: ref, Raw: replacementRaw, InternalDate: date,
		})
		present = append(present, ref)
	}
	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Role: backend.RoleInbox, Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {{
			Messages: messages, Present: present, Cursor: backend.Cursor("replacement"), FullSnapshot: true,
		}},
	})
	backendWithIdentity := &identityCursorBackend{Backend: &backendOnly{Backend: fake}, adopted: make(map[string]string)}
	engine := New(Options{Store: db, Cursors: newMemCursorStore(), Account: testAccount, Ingest: ingestOptions})
	result, err := engine.Sync(t.Context(), backendWithIdentity)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("replacement result=%+v err=%v", result, err)
	}
	if result.New != 0 || result.Deduplicated != 2 || result.Deleted != 0 {
		t.Fatalf("replacement counts = new %d dedup %d deleted %d", result.New, result.Deduplicated, result.Deleted)
	}
	for i, messageID := range oldIDs {
		stored, err := db.GetByMessageID(messageID)
		if err != nil || stored == nil {
			t.Fatalf("preserved row %s = %+v, %v", messageID, stored, err)
		}
		wantRef := fmt.Sprintf("%d", 102-i)
		if stored.RemoteRef != wantRef {
			t.Fatalf("%s remote ref = %q, want %q", messageID, stored.RemoteRef, wantRef)
		}
		if stored.BodyText != "" || stored.BodyHTML != "" {
			t.Fatalf("attachment-only %s gained a body: text=%q html=%q", messageID, stored.BodyText, stored.BodyHTML)
		}
		attachments, err := db.GetAttachmentsByMessage(stored.ID)
		if err != nil || len(attachments) != 1 || attachments[0].Filename != "report.pdf" {
			t.Fatalf("%s attachments = %+v, err=%v", messageID, attachments, err)
		}
		wantTag := fmt.Sprintf("local-%d", 2-i)
		if tags := mustTags(t, db, messageID); !slices.Contains(tags, wantTag) {
			t.Fatalf("%s lost %s: %v", messageID, wantTag, tags)
		} else if slices.Contains(tags, "first-ingest") {
			t.Fatalf("%s reran first-ingest rules: %v", messageID, tags)
		}
		if got := backendWithIdentity.adopted[wantRef]; got != messageID {
			t.Fatalf("cursor identity for ref %s = %q, want %q", wantRef, got, messageID)
		}
	}
	for _, messageID := range newIDs {
		if stored, _ := db.GetByMessageID(messageID); stored != nil {
			t.Fatalf("duplicate provisional row remains: %+v", stored)
		}
	}
}

func TestEngineReplacementRecoveredUpsertFailureHoldsCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.db")
	kr, err := dbcrypto.NewKeyring(bytes.Repeat([]byte{0x42}, dbcrypto.MasterKeyLen))
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(path, kr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Init(); err != nil {
		t.Fatal(err)
	}

	date := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	raw := []byte("From: notifier@example.com\r\n" +
		"To: " + testAccount + "\r\n" +
		"Subject: Durable recovery\r\n" +
		"Date: Thu, 27 Aug 2026 10:00:00 +0000\r\n\r\nsame body")
	const oldID = "durian-synthetic-1-INBOX@test@example.com"
	seed := backend.Message{
		MessageID: oldID, Ref: backend.RemoteRef{Folder: "INBOX", ID: "1"},
		Raw: raw, InternalDate: date,
	}
	if _, _, created, err := Ingest(db, seed, "INBOX", backend.RoleInbox, IngestOptions{Account: testAccount}); err != nil || !created {
		t.Fatalf("seed: created=%v err=%v", created, err)
	}
	stored, _ := db.GetByMessageID(oldID)
	if err := db.AddTag(stored.ID, "local"); err != nil {
		t.Fatal(err)
	}

	// Break the post-upsert FTS write with a permanent store error. The message
	// upsert itself runs in the same transaction and must roll back with it.
	schemaDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := schemaDB.Exec("DROP TABLE messages_blind_fts"); err != nil {
		schemaDB.Close()
		t.Fatal(err)
	}
	if err := schemaDB.Close(); err != nil {
		t.Fatal(err)
	}

	replacement := backend.Message{
		MessageID: "durian-synthetic-v2-99-2-INBOX@" + testAccount,
		Ref:       backend.RemoteRef{Folder: "INBOX", ID: "2"},
		Raw:       raw, InternalDate: date,
	}
	folder := backend.Folder{Name: "INBOX", Role: backend.RoleInbox, Selectable: true}
	fake := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
		"INBOX": {{
			Messages: []backend.Message{replacement}, Present: []backend.RemoteRef{replacement.Ref},
			Cursor: backend.Cursor("replacement"), FullSnapshot: true,
		}},
	})
	backendWithIdentity := &identityCursorBackend{Backend: &backendOnly{Backend: fake}, adopted: make(map[string]string)}
	cursors := newMemCursorStore()
	if err := cursors.Set(testAccount, "INBOX", backend.Cursor("old")); err != nil {
		t.Fatal(err)
	}
	result, err := newTestEngine(db, cursors).Sync(t.Context(), backendWithIdentity)
	if err != nil || len(result.Errors) == 0 {
		t.Fatalf("replacement result=%+v err=%v, want durable upsert error", result, err)
	}
	if got, _ := cursors.Get(testAccount, "INBOX"); string(got) != "old" {
		t.Fatalf("cursor advanced to %q after recovered upsert failure", got)
	}
	stored, err = db.GetByMessageID(oldID)
	if err != nil || stored == nil || stored.RemoteRef != "1" {
		t.Fatalf("canonical row after failure = %+v, err=%v", stored, err)
	}
	if tags := mustTags(t, db, oldID); !slices.Contains(tags, "inbox") || !slices.Contains(tags, "local") {
		t.Fatalf("canonical tags changed after failure: %v", tags)
	}
	if provisional, _ := db.GetByMessageID(replacement.MessageID); provisional != nil {
		t.Fatalf("provisional row survived failed transaction: %+v", provisional)
	}
	if len(backendWithIdentity.adopted) != 0 {
		t.Fatalf("failed identity was written into cursor: %v", backendWithIdentity.adopted)
	}
}

func TestEngineReplacementDoesNotAdoptRealGeneratedGrammarID(t *testing.T) {
	db := newTestDB(t)
	date := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	const realID = "durian-synthetic-7-INBOX@test@example.com"
	realRaw := rawMessage(realID, "notifier@example.com", testAccount, "Same content", "same body")
	noIDRaw := bytes.Replace(realRaw, []byte("Message-ID: <"+realID+">\r\n"), nil, 1)
	seed := backend.Message{
		MessageID: realID, Ref: backend.RemoteRef{Folder: "INBOX", ID: "7"},
		Raw: realRaw, InternalDate: date,
	}
	if _, _, created, err := Ingest(db, seed, "INBOX", backend.RoleInbox, IngestOptions{Account: testAccount}); err != nil || !created {
		t.Fatalf("seed: created=%v err=%v", created, err)
	}
	realStored, _ := db.GetByMessageID(realID)
	if realStored.SyntheticIdentity {
		t.Fatal("real Message-ID was marked as generated")
	}
	if err := db.AddTag(realStored.ID, "local-real"); err != nil {
		t.Fatal(err)
	}

	const provisionalID = "durian-synthetic-v2-99-8-INBOX@test@example.com"
	replacement := backend.Message{
		MessageID: provisionalID, Ref: backend.RemoteRef{Folder: "INBOX", ID: "8"},
		Raw: noIDRaw, InternalDate: date,
	}
	folder := backend.Folder{Name: "INBOX", Role: backend.RoleInbox, Selectable: true}
	fake := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
		"INBOX": {{
			Messages: []backend.Message{replacement}, Present: []backend.RemoteRef{replacement.Ref},
			Cursor: backend.Cursor("replacement"), FullSnapshot: true,
		}},
	})
	backendWithIdentity := &identityCursorBackend{Backend: &backendOnly{Backend: fake}, adopted: make(map[string]string)}
	engine := New(Options{
		Store: db, Cursors: newMemCursorStore(), Account: testAccount, Mode: DownloadOnly,
		Ingest: IngestOptions{Account: testAccount},
	})
	result, err := engine.Sync(t.Context(), backendWithIdentity)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("replacement result=%+v err=%v", result, err)
	}
	realStored, err = db.GetByMessageID(realID)
	if err != nil || realStored == nil || realStored.RemoteRef != "7" || !slices.Contains(mustTags(t, db, realID), "local-real") {
		t.Fatalf("real row was consumed: row=%+v tags=%v err=%v", realStored, mustTags(t, db, realID), err)
	}
	generated, err := db.GetByMessageID(provisionalID)
	if err != nil || generated == nil || generated.RemoteRef != "8" || !generated.SyntheticIdentity {
		t.Fatalf("no-ID row = %+v, err=%v", generated, err)
	}
	if len(backendWithIdentity.adopted) != 0 {
		t.Fatalf("real identity was adopted into cursor: %v", backendWithIdentity.adopted)
	}
}

func TestEngineReplacementRetryKeepsDuplicateIdentityOrder(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	if err := cursors.Set(testAccount, "INBOX", backend.Cursor("old")); err != nil {
		t.Fatal(err)
	}
	date := time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC)
	raw := []byte("From: notifier@example.com\r\n" +
		"To: " + testAccount + "\r\n" +
		"Subject: Identical alert\r\n" +
		"Date: Thu, 27 Aug 2026 10:00:00 +0000\r\n\r\nsame body")
	oldIDs := []string{
		"durian-synthetic-v2-10-4-INBOX@" + testAccount,
		"durian-synthetic-v2-10-3-INBOX@" + testAccount,
	}
	for i, messageID := range oldIDs {
		ref := fmt.Sprintf("%d", 4-i)
		msg := backend.Message{
			MessageID: messageID, Ref: backend.RemoteRef{Folder: "INBOX", ID: ref},
			Raw: raw, InternalDate: date,
		}
		if _, _, created, err := Ingest(db, msg, "INBOX", backend.RoleInbox, IngestOptions{Account: testAccount}); err != nil || !created {
			t.Fatalf("seed %s: created=%v err=%v", messageID, created, err)
		}
		stored, _ := db.GetByMessageID(messageID)
		if err := db.AddTag(stored.ID, fmt.Sprintf("copy-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	message := func(uid uint32) backend.Message {
		return backend.Message{
			MessageID:    fmt.Sprintf("durian-synthetic-v2-99-%d-INBOX@%s", uid, testAccount),
			Ref:          backend.RemoteRef{Folder: "INBOX", ID: fmt.Sprintf("%d", uid)},
			Raw:          raw,
			InternalDate: date,
		}
	}
	page := func(uid uint32, cursor string, more bool) backend.FetchResult {
		msg := message(uid)
		return backend.FetchResult{
			Messages: []backend.Message{msg}, Present: []backend.RemoteRef{msg.Ref},
			Cursor: backend.Cursor(cursor), HasMore: more, FullSnapshot: true,
		}
	}
	folder := backend.Folder{Name: "INBOX", Role: backend.RoleInbox, Selectable: true}
	first := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
		"INBOX": {
			page(9, "page-one", true),
			page(8, "page-two", true),
			page(7, "after-three", true),
		},
	})
	first.fetchErrByCursor = map[string]error{"after-three": errors.New("replacement completion unavailable")}
	firstWithIdentity := &identityCursorBackend{Backend: &backendOnly{Backend: first}, adopted: make(map[string]string)}
	engine := New(Options{
		Store: db, Cursors: cursors, Account: testAccount,
		Ingest: IngestOptions{
			Account: testAccount,
			FilterRules: []config.RuleConfig{{
				Name: "first ingest only", Match: "*", AddTags: []string{"first-ingest"},
			}},
		},
	})
	result, err := engine.Sync(t.Context(), firstWithIdentity)
	if err != nil || len(result.Errors) == 0 {
		t.Fatalf("failed replacement result=%+v err=%v, want completion error", result, err)
	}
	if cursor, _ := cursors.Get(testAccount, "INBOX"); string(cursor) != "after-three" {
		t.Fatalf("failed replacement checkpoint = %q, want after-three", cursor)
	}
	if high, _ := db.GetByMessageID(oldIDs[0]); high.RemoteRef != "9" {
		t.Fatalf("failed replacement mapped high copy to ref %q, want 9", high.RemoteRef)
	}
	if low, _ := db.GetByMessageID(oldIDs[1]); low.RemoteRef != "8" {
		t.Fatalf("failed replacement mapped low copy to ref %q, want 8", low.RemoteRef)
	}
	currentID := message(7).MessageID
	current, err := db.GetByMessageID(currentID)
	if err != nil || current == nil || current.RemoteRef != "7" {
		t.Fatalf("new current-epoch copy = %+v, err=%v", current, err)
	}
	if err := db.RemoveTag(current.ID, "first-ingest"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddTag(current.ID, "local-new-copy"); err != nil {
		t.Fatal(err)
	}

	retry := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
		"INBOX": {{Cursor: backend.Cursor("done"), FullSnapshot: true}},
	})
	retryWithIdentity := &identityCursorBackend{Backend: &backendOnly{Backend: retry}, adopted: make(map[string]string)}
	result, err = engine.Sync(t.Context(), retryWithIdentity)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("replacement retry result=%+v err=%v", result, err)
	}
	for i, messageID := range oldIDs {
		stored, err := db.GetByMessageID(messageID)
		if err != nil || stored == nil {
			t.Fatalf("stored %s = %+v, err=%v", messageID, stored, err)
		}
		wantRef := fmt.Sprintf("%d", 9-i)
		wantTag := fmt.Sprintf("copy-%d", i)
		if stored.RemoteRef != wantRef || !slices.Contains(mustTags(t, db, messageID), wantTag) {
			t.Fatalf("%s after retry: ref=%q tags=%v, want ref=%q tag=%q", messageID, stored.RemoteRef, mustTags(t, db, messageID), wantRef, wantTag)
		}
	}
	current, err = db.GetByMessageID(currentID)
	if err != nil || current == nil || current.RemoteRef != "7" {
		t.Fatalf("current-epoch copy after retry = %+v, err=%v", current, err)
	}
	if tags := mustTags(t, db, currentID); !slices.Contains(tags, "local-new-copy") || slices.Contains(tags, "first-ingest") {
		t.Fatalf("current-epoch retry reran first-ingest work or lost local state: %v", tags)
	}
	if cursor, _ := cursors.Get(testAccount, "INBOX"); string(cursor) != "done" {
		t.Fatalf("completed replacement cursor = %q, want done", cursor)
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

func TestEngineReplacementRemovesOldStableRefsAfterPermanentHydratedIngestFailure(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	prior := backend.Message{
		StableID: "prior", MessageID: "broken@example.com", Ref: backend.RemoteRef{Folder: "INBOX", ID: "prior"},
		Raw: rawMessage("broken@example.com", "a@example.com", testAccount, "Prior", "body"),
	}
	if _, _, _, err := Ingest(db, prior, "INBOX", backend.RoleNone, IngestOptions{Account: testAccount}); err != nil {
		t.Fatalf("seed prior copy: %v", err)
	}
	priorDuplicate := backend.Message{
		StableID: "prior-duplicate", MessageID: prior.MessageID,
		Ref: backend.RemoteRef{Folder: "INBOX", ID: "prior-duplicate"},
		Raw: rawMessage(prior.MessageID, "b@example.com", testAccount, "Prior duplicate", "other body"),
	}
	if _, _, _, err := Ingest(db, priorDuplicate, "INBOX", backend.RoleNone, IngestOptions{Account: testAccount}); err != nil {
		t.Fatalf("seed duplicate prior copy: %v", err)
	}
	ref := backend.RemoteRef{Folder: "INBOX", ID: "broken"}
	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {{
			Cursor: backend.Cursor("replacement"), FullSnapshot: true,
			Present: []backend.RemoteRef{ref},
		}},
	})
	fake.snapshotMessages = map[string]backend.Message{
		ref.ID: {StableID: ref.ID, MessageID: "broken@example.com", Ref: ref, Raw: []byte("malformed header line\r\n\r\nbody")},
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
	if got, _ := db.GetByRemoteRef(testAccount, "INBOX", prior.Ref.ID); got != nil {
		t.Fatal("old account-scoped ref survived hydrated stable-ID replacement")
	}
	if got, _ := db.GetByRemoteRef(testAccount, "INBOX", priorDuplicate.Ref.ID); got != nil {
		t.Fatal("duplicate old account-scoped ref survived hydrated stable-ID replacement")
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

// gmailbackend builds hydrated messages without Message.MessageID, so the
// preservation of an older local copy must not depend on that field being set.
func TestEngineReplacementPreservesPriorCopyWhenHydratorOmitsMessageID(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	prior := backend.Message{
		MessageID: "shared@example.com", Ref: backend.RemoteRef{Folder: "INBOX", ID: "prior"},
		Raw: rawMessage("shared@example.com", "a@example.com", testAccount, "Prior", "body"),
	}
	if _, _, _, err := Ingest(db, prior, "INBOX", backend.RoleNone, IngestOptions{Account: testAccount}); err != nil {
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
