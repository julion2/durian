// Package jmapbackend implements Durian's provider-neutral mail backend on
// RFC 8620 (JMAP Core) and RFC 8621 (JMAP Mail and Submission).
//
// Cursor encoding is opaque JSON. It binds all state to the authenticated
// provider account. During initial sync it also contains a start-state snapshot
// and the remaining stable Email/query ID set; after that it contains EmailState.
// If Email/changes returns cannotCalculateChanges, the backend emits a
// metadata-first replacement snapshot and advances EmailState only after the
// engine has hydrated and reconciled the complete remote ID set.
package jmapbackend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/keychain"
)

const allMailStream = "JMAP-ALL"

var (
	getCredential = keychain.GetPassword
	setCredential = keychain.SetPassword
)

var errIncompleteQuery = errors.New("JMAP Email/query returned an incomplete snapshot")

// Backend translates JMAP Mail objects into Durian's provider-neutral model.
type Backend struct {
	account *config.AccountConfig
	client  *client

	mu           sync.Mutex
	initialized  bool
	mailboxes    map[string]jmapMailbox
	mailboxToTag map[string]string
	tagToID      map[string]string
}

var (
	_ backend.Backend              = (*Backend)(nil)
	_ backend.ArbitraryLabelWriter = (*Backend)(nil)
	_ backend.SnapshotHydrator     = (*Backend)(nil)
	_ backend.TagMutationWriter    = (*Backend)(nil)
)

// New creates a JMAP backend for an account configured with sync_engine=jmap.
func New(account *config.AccountConfig) (*Backend, error) {
	if account == nil || account.JMAP == nil || !account.UsesJMAPBackend() {
		return nil, errors.New("JMAP backend requires sync_engine=\"jmap\" and a jmap configuration block")
	}
	if strings.TrimSpace(account.JMAP.SessionURL) == "" {
		return nil, errors.New("JMAP session URL is required")
	}
	mode := account.JMAP.Auth
	if mode == "" {
		mode = "password"
	}
	if mode != "password" && mode != "bearer" {
		return nil, fmt.Errorf("unsupported JMAP auth mode %q", mode)
	}
	username := account.Email
	if account.Auth != nil && account.Auth.Username != "" {
		username = account.Auth.Username
	}
	return &Backend{
		account: account,
		client: &client{
			httpClient: &http.Client{
				Timeout:       90 * time.Second,
				CheckRedirect: validateJMAPRedirect,
			},
			sessionURL: account.JMAP.SessionURL,
			credential: credential{mode: mode, username: username},
		},
	}, nil
}

func (b *Backend) ensure(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.initialized && !b.client.sessionStale.Load() {
		return nil
	}
	b.initialized = false
	secret, err := getCredential(keychain.JMAPKeychainService, b.account.Email)
	legacyCredential := false
	if errors.Is(err, keychain.ErrNotFound) && b.client.credential.mode == "password" {
		secret, err = getCredential(keychain.PasswordKeychainService, b.account.Email)
		legacyCredential = err == nil
	}
	if err != nil {
		if errors.Is(err, keychain.ErrNotFound) {
			return fmt.Errorf("no JMAP credential stored for %s; run durian auth login %s", b.account.Email, b.account.GetAliasOrName())
		} else {
			return fmt.Errorf("load JMAP credential: %w", err)
		}
	}
	b.client.credential.secret = secret
	if err := b.client.discover(ctx); err != nil {
		return err
	}
	if legacyCredential {
		if err := setCredential(keychain.JMAPKeychainService, b.account.Email, secret); err != nil {
			slog.Warn("Could not migrate legacy JMAP credential", "module", "JMAPBACKEND", "account", b.account.AccountIdentifier()) // encgrep:allow account identifier, no credential value
		}
	}
	b.initialized = true
	return nil
}

func (b *Backend) loadMailboxes(ctx context.Context) error {
	if err := b.ensure(ctx); err != nil {
		return err
	}
	var result jmapMailboxGetPage
	args := map[string]interface{}{
		"accountId":  b.client.accountID,
		"properties": []string{"id", "name", "parentId", "role", "isSubscribed"},
	}
	if err := b.client.call(ctx, []string{coreCapability, mailCapability}, "Mailbox/get", args, &result); err != nil {
		return err
	}
	if err := result.validate(b.client.accountID); err != nil {
		return err
	}
	mailboxes := make(map[string]jmapMailbox, len(*result.List))
	for _, mailbox := range *result.List {
		mailboxes[mailbox.ID] = mailbox
	}
	mailboxToTag, tagToID := buildMailboxMappings(mailboxes)
	b.mu.Lock()
	b.mailboxes = mailboxes
	b.mailboxToTag = mailboxToTag
	b.tagToID = tagToID
	b.mu.Unlock()
	return nil
}

func roleTag(role string) string {
	switch strings.ToLower(role) {
	case "inbox":
		return "inbox"
	case "archive":
		return "archive"
	case "drafts":
		return "draft"
	case "sent":
		return "sent"
	case "trash":
		return "trash"
	case "junk":
		return "spam"
	case "all":
		return "all"
	case "important":
		return "important"
	}
	return ""
}

func roleMailboxForTag(tag string) (role, name string, ok bool) {
	switch tag {
	case "inbox":
		return "inbox", "Inbox", true
	case "archive":
		return "archive", "Archive", true
	case "draft":
		return "drafts", "Drafts", true
	case "sent":
		return "sent", "Sent", true
	case "trash":
		return "trash", "Trash", true
	case "spam":
		return "junk", "Junk", true
	case "all":
		return "all", "All Mail", true
	case "important":
		return "important", "Important", true
	default:
		return "", "", false
	}
}

func canonicalMailboxSegment(name string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), " ", "-")
}

func mailboxTagSuffix(id string) string {
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("%x", sum[:4])
}

// buildMailboxMappings creates one stable, reversible Durian tag for every
// mailbox. Special-use roles keep their fixed vocabulary; user mailboxes use
// canonical parent paths, with an ID-derived suffix for ambiguous paths.
func buildMailboxMappings(mailboxes map[string]jmapMailbox) (map[string]string, map[string]string) {
	ids := make([]string, 0, len(mailboxes))
	for id := range mailboxes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	roleOwners := make(map[string]string)
	for _, id := range ids {
		if tag := roleTag(mailboxes[id].Role); tag != "" && roleOwners[tag] == "" {
			roleOwners[tag] = id
		}
	}

	raw := make(map[string]string, len(ids))
	isRole := make(map[string]bool, len(ids))
	for _, id := range ids {
		mailbox := mailboxes[id]
		if tag := roleTag(mailbox.Role); tag != "" && roleOwners[tag] == id {
			raw[id] = tag
			isRole[id] = true
			continue
		}
		var segments []string
		seen := make(map[string]bool)
		current := id
		for current != "" && !seen[current] {
			seen[current] = true
			ancestor, ok := mailboxes[current]
			if !ok {
				break
			}
			segment := canonicalMailboxSegment(ancestor.Name)
			if segment == "" {
				segment = "mailbox~" + mailboxTagSuffix(current)
			}
			segments = append(segments, segment)
			current = ancestor.ParentID
		}
		for left, right := 0, len(segments)-1; left < right; left, right = left+1, right-1 {
			segments[left], segments[right] = segments[right], segments[left]
		}
		raw[id] = strings.Join(segments, "/")
	}

	groups := make(map[string][]string)
	for _, id := range ids {
		groups[raw[id]] = append(groups[raw[id]], id)
	}
	mailboxToTag := make(map[string]string, len(ids))
	tagToID := make(map[string]string, len(ids))
	// Reserve fixed role vocabulary before allocating ordinary names so roles
	// always remain reachable regardless of mailbox ID ordering.
	for _, id := range ids {
		if !isRole[id] {
			continue
		}
		mailboxToTag[id] = raw[id]
		tagToID[raw[id]] = id
	}
	for _, id := range ids {
		if isRole[id] {
			continue
		}
		tag := raw[id]
		suffix := "~" + mailboxTagSuffix(id)
		if strings.HasPrefix(tag, "jmap-keyword/") {
			// jmap-keyword/ is the explicit native-keyword namespace, so a
			// mailbox path must not claim it.
			tag = "mailbox" + suffix + "/" + tag
		} else if len(groups[tag]) > 1 || isExplicitFlagTag(tag) || tagToID[tag] != "" {
			tag += suffix
		}
		for tagToID[tag] != "" {
			tag += suffix
		}
		mailboxToTag[id] = tag
		tagToID[tag] = id
	}
	return mailboxToTag, tagToID
}

// FetchFolders refreshes mailbox metadata and exposes JMAP's account-wide mail
// set as one logical stream. Mailbox memberships are represented as tags.
func (b *Backend) FetchFolders(ctx context.Context) ([]backend.Folder, error) {
	if err := b.loadMailboxes(ctx); err != nil {
		return nil, fmt.Errorf("load JMAP mailboxes: %w", err)
	}
	return []backend.Folder{{Name: allMailStream, Display: "All Mail", Role: backend.RoleAll, Selectable: true}}, nil
}

func (b *Backend) scopedEmailID(id string) string {
	return b.client.accountScope + ":" + id
}

func (b *Backend) rawEmailID(refID string) (string, error) {
	id, ok := strings.CutPrefix(refID, b.client.accountScope+":")
	if !ok || id == "" {
		return "", fmt.Errorf("%w: JMAP ref does not belong to the authenticated account", backend.ErrRefGone)
	}
	return id, nil
}

func (b *Backend) rawEmailIDs(refs []backend.RemoteRef) ([]string, error) {
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		id, err := b.rawEmailID(ref.ID)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// FetchMessages pages an initial account snapshot or follows Email/changes from
// the last fully applied state token.
func (b *Backend) FetchMessages(ctx context.Context, folder string, cursor backend.Cursor, limit int) (backend.FetchResult, error) {
	if folder != allMailStream {
		return backend.FetchResult{}, fmt.Errorf("unknown JMAP stream %q", folder)
	}
	if limit <= 0 {
		limit = 100
	}
	if err := b.loadMailboxes(ctx); err != nil {
		return backend.FetchResult{}, err
	}
	state := decodeCursor(cursor)
	if len(cursor) > 0 && state.AccountScope != b.client.accountScope {
		slog.Info("JMAP provider account changed, starting replacement snapshot", "module", "JMAPBACKEND")
		return b.startReplacement(ctx, limit)
	}
	state.AccountScope = b.client.accountScope
	if state.Replacement {
		return b.replacementPage(ctx, state, limit)
	}
	if !state.EmailStateSet {
		// New initial syncs use the same bounded, anchor-based authoritative
		// enumeration as state-expiry recovery. initialPage remains solely for
		// resuming cursors written by older versions with PendingIDs/Snapshot.
		if !state.SnapshotSet && len(state.PendingIDs) == 0 {
			return b.startReplacement(ctx, limit)
		}
		return b.initialPage(ctx, state, limit)
	}
	return b.changesPage(ctx, state, limit)
}

func (b *Backend) initialPage(ctx context.Context, state jmapCursor, limit int) (backend.FetchResult, error) {
	if !state.SnapshotSet {
		var err error
		state.Snapshot, err = b.currentEmailState(ctx)
		if err != nil {
			return backend.FetchResult{}, fmt.Errorf("snapshot JMAP email state: %w", err)
		}
		state.SnapshotSet = true
		// Capture the complete ID set before paging bodies. Anchoring each query
		// page to the last ID avoids the position shifts caused by concurrent
		// inserts/deletes. A deleted anchor restarts the short ID-only scan.
		state.PendingIDs, err = b.queryAllEmailIDs(ctx)
		if err != nil {
			return backend.FetchResult{}, err
		}
	}
	count := min(limit, len(state.PendingIDs))
	ids := state.PendingIDs[:count]
	messages, missing, err := b.getMessages(ctx, ids)
	if err != nil {
		return backend.FetchResult{}, err
	}
	deleted := b.deletions(allMailStream, missing)
	state.PendingIDs = state.PendingIDs[count:]
	hasMore := len(state.PendingIDs) > 0
	if hasMore {
		state.PendingIDs = append([]string(nil), state.PendingIDs...)
	} else {
		state = jmapCursor{AccountScope: b.client.accountScope, EmailState: state.Snapshot, EmailStateSet: true}
	}
	return backend.FetchResult{
		Messages: messages, Deleted: deleted, Cursor: encodeCursor(state), HasMore: hasMore,
		Present: b.presentRefs(allMailStream, ids, missing),
	}, nil
}

func (b *Backend) queryAllEmailIDs(ctx context.Context) ([]string, error) {
	const maxAnchorRestarts = 3
	for attempt := 0; attempt < maxAnchorRestarts; attempt++ {
		ids, err := b.queryAllEmailIDsOnce(ctx)
		if err == nil {
			return ids, nil
		}
		var methodErr *methodError
		if (!errors.As(err, &methodErr) || methodErr.Type != "anchorNotFound") && !errors.Is(err, errIncompleteQuery) {
			return nil, err
		}
	}
	return nil, errors.New("Email/query could not produce a stable complete snapshot")
}

func (b *Backend) queryAllEmailIDsOnce(ctx context.Context) ([]string, error) {
	queryPageSize := b.client.maxObjectsInGet(1000)
	args := map[string]interface{}{
		"accountId":      b.client.accountID,
		"position":       0,
		"limit":          queryPageSize,
		"calculateTotal": true,
		"sort":           []map[string]interface{}{{"property": "receivedAt", "isAscending": false}},
	}
	var ids []string
	expectedTotal := 0
	expectedQueryState := ""
	expectedQueryStateSet := false
	for {
		var query jmapQueryPage
		if err := b.client.call(ctx, []string{coreCapability, mailCapability}, "Email/query", args, &query); err != nil {
			return nil, err
		}
		if err := query.validate(b.client.accountID); err != nil {
			return nil, err
		}
		queryState, position, pageIDs, total := *query.QueryState, *query.Position, *query.IDs, *query.Total
		if !expectedQueryStateSet {
			expectedQueryState = queryState
			expectedQueryStateSet = true
			expectedTotal = total
		} else if queryState != expectedQueryState || total != expectedTotal {
			return nil, fmt.Errorf("%w: query changed while paging", errIncompleteQuery)
		}
		if position != len(ids) {
			return nil, fmt.Errorf("%w: got position %d, expected %d", errIncompleteQuery, position, len(ids))
		}
		ids = append(ids, pageIDs...)
		if len(pageIDs) == 0 {
			if len(ids) < total {
				return nil, fmt.Errorf("%w: got %d of %d ids", errIncompleteQuery, len(ids), total)
			}
			break
		}
		if position+len(pageIDs) >= total {
			break
		}
		nextAnchor := pageIDs[len(pageIDs)-1]
		if priorAnchor, ok := args["anchor"].(string); ok && priorAnchor == nextAnchor {
			return nil, errors.New("Email/query anchor pagination made no progress")
		}
		delete(args, "position")
		args["anchor"] = nextAnchor
		args["anchorOffset"] = 1
	}
	ids = uniqueStrings(ids)
	if len(ids) != expectedTotal {
		return nil, fmt.Errorf("%w: got %d unique ids, expected %d", errIncompleteQuery, len(ids), expectedTotal)
	}
	return ids, nil
}

func (b *Backend) changesPage(ctx context.Context, state jmapCursor, limit int) (backend.FetchResult, error) {
	var changes jmapChangesPage
	args := map[string]interface{}{
		"accountId":  b.client.accountID,
		"sinceState": state.EmailState,
		"maxChanges": limit,
	}
	if err := b.client.call(ctx, []string{coreCapability, mailCapability}, "Email/changes", args, &changes); err != nil {
		var methodErr *methodError
		if errors.As(err, &methodErr) && methodErr.Type == "cannotCalculateChanges" {
			slog.Info("JMAP state expired, starting paged replacement snapshot", "module", "JMAPBACKEND")
			return b.startReplacement(ctx, limit)
		}
		return backend.FetchResult{}, err
	}
	if err := changes.validate(b.client.accountID, state.EmailState); err != nil {
		return backend.FetchResult{}, err
	}
	destroyedSet := make(map[string]struct{}, len(*changes.Destroyed))
	for _, id := range *changes.Destroyed {
		destroyedSet[id] = struct{}{}
	}
	ids := make([]string, 0, len(*changes.Created)+len(*changes.Updated))
	for _, id := range append(append([]string(nil), (*changes.Created)...), (*changes.Updated)...) {
		if _, destroyed := destroyedSet[id]; !destroyed {
			ids = append(ids, id)
		}
	}
	ids = uniqueStrings(ids)
	messages, missing, err := b.getMessages(ctx, ids)
	if err != nil {
		return backend.FetchResult{}, err
	}
	destroyed := append(append([]string(nil), (*changes.Destroyed)...), missing...)
	state.EmailState = *changes.NewState
	state.EmailStateSet = true
	return backend.FetchResult{
		Messages: messages,
		Deleted:  b.deletions(allMailStream, uniqueStrings(destroyed)),
		Cursor:   encodeCursor(state),
		HasMore:  *changes.HasMoreChanges,
	}, nil
}

func (b *Backend) startReplacement(ctx context.Context, limit int) (backend.FetchResult, error) {
	state, err := b.currentEmailState(ctx)
	if err != nil {
		return backend.FetchResult{}, fmt.Errorf("snapshot JMAP email state: %w", err)
	}
	return b.replacementPage(ctx, jmapCursor{AccountScope: b.client.accountScope, Snapshot: state, SnapshotSet: true, Replacement: true}, limit)
}

func (b *Backend) replacementPage(ctx context.Context, state jmapCursor, limit int) (backend.FetchResult, error) {
	pageLimit := b.client.maxObjectsInGet(limit)
	args := map[string]interface{}{
		"accountId":      b.client.accountID,
		"limit":          pageLimit,
		"calculateTotal": true,
		"sort":           []map[string]interface{}{{"property": "receivedAt", "isAscending": false}},
	}
	if state.QueryAnchor == "" {
		args["position"] = 0
	} else {
		args["anchor"] = state.QueryAnchor
		args["anchorOffset"] = 1
	}
	var query jmapQueryPage
	if err := b.client.call(ctx, []string{coreCapability, mailCapability}, "Email/query", args, &query); err != nil {
		var methodErr *methodError
		if state.QueryStateSet && errors.As(err, &methodErr) && methodErr.Type == "anchorNotFound" {
			return backend.FetchResult{}, fmt.Errorf("%w: replacement anchor disappeared", backend.ErrSnapshotInvalidated)
		}
		return backend.FetchResult{}, fmt.Errorf("query JMAP replacement snapshot page: %w", err)
	}
	if err := query.validate(b.client.accountID); err != nil {
		return backend.FetchResult{}, fmt.Errorf("query JMAP replacement snapshot page: %w", err)
	}
	queryState, position, pageIDs, total := *query.QueryState, *query.Position, *query.IDs, *query.Total
	if position != state.QuerySeen {
		return backend.FetchResult{}, fmt.Errorf("%w: JMAP replacement query position changed: got %d, want %d", backend.ErrSnapshotInvalidated, position, state.QuerySeen)
	}
	if !state.QueryStateSet {
		state.QueryState = queryState
		state.QueryStateSet = true
		state.QueryTotal = total
	} else if queryState != state.QueryState || total != state.QueryTotal {
		return backend.FetchResult{}, fmt.Errorf("%w: JMAP replacement query changed while paging", backend.ErrSnapshotInvalidated)
	}
	uniquePageIDs := uniqueStrings(pageIDs)
	if len(uniquePageIDs) != len(pageIDs) {
		return backend.FetchResult{}, errors.New("JMAP replacement query returned duplicate IDs in one page")
	}
	state.QuerySeen += len(uniquePageIDs)
	if state.QuerySeen > state.QueryTotal || (len(pageIDs) == 0 && state.QuerySeen < state.QueryTotal) {
		return backend.FetchResult{}, fmt.Errorf("%w: %w: got %d of %d ids", backend.ErrSnapshotInvalidated, errIncompleteQuery, state.QuerySeen, state.QueryTotal)
	}
	hasMore := state.QuerySeen < state.QueryTotal
	if hasMore {
		state.QueryAnchor = uniquePageIDs[len(uniquePageIDs)-1]
	} else {
		state = jmapCursor{AccountScope: b.client.accountScope, EmailState: state.Snapshot, EmailStateSet: true}
	}
	return backend.FetchResult{
		Cursor:       encodeCursor(state),
		HasMore:      hasMore,
		FullSnapshot: true,
		Present:      b.presentRefs(allMailStream, uniquePageIDs, nil),
	}, nil
}

// FetchSnapshotMessages hydrates only replacement-snapshot refs absent from
// Durian's local read model; the engine determines that set before calling.
func (b *Backend) FetchSnapshotMessages(ctx context.Context, refs []backend.RemoteRef) (backend.SnapshotBatch, error) {
	if err := b.ensure(ctx); err != nil {
		return backend.SnapshotBatch{}, err
	}
	ids, err := b.rawEmailIDs(refs)
	if err != nil {
		return backend.SnapshotBatch{}, err
	}
	messages, missing, err := b.getMessages(ctx, ids)
	return backend.SnapshotBatch{Messages: messages, Missing: b.presentRefs(allMailStream, missing, nil)}, err
}

// FetchSnapshotMetadata returns current mailbox memberships and keywords in a
// batched Email/get without downloading RFC 5322 blobs.
func (b *Backend) FetchSnapshotMetadata(ctx context.Context, refs []backend.RemoteRef) (backend.SnapshotBatch, error) {
	if err := b.ensure(ctx); err != nil {
		return backend.SnapshotBatch{}, err
	}
	ids, err := b.rawEmailIDs(refs)
	if err != nil {
		return backend.SnapshotBatch{}, err
	}
	objects, missing, _, err := b.getEmailObjects(ctx, ids)
	if err != nil {
		return backend.SnapshotBatch{}, err
	}
	messages := make([]backend.Message, 0, len(objects))
	for _, email := range objects {
		stableID := b.scopedEmailID(email.ID)
		messages = append(messages, backend.Message{
			StableID: stableID, Ref: backend.RemoteRef{Folder: allMailStream, ID: stableID},
			Flags: flagsFromKeywords(email.Keywords), Labels: b.labelsFor(email.MailboxIDs, email.Keywords),
		})
	}
	return backend.SnapshotBatch{Messages: messages, Missing: b.presentRefs(allMailStream, missing, nil)}, nil
}

func (b *Backend) currentEmailState(ctx context.Context) (string, error) {
	var result jmapGetPage
	args := map[string]interface{}{"accountId": b.client.accountID, "ids": []string{}, "properties": []string{"id"}}
	if err := b.client.call(ctx, []string{coreCapability, mailCapability}, "Email/get", args, &result); err != nil {
		return "", err
	}
	if err := result.validate(b.client.accountID, nil); err != nil {
		return "", err
	}
	return *result.State, nil
}

func (b *Backend) getEmailObjects(ctx context.Context, ids []string) ([]jmapEmail, []string, string, error) {
	if err := b.ensure(ctx); err != nil {
		return nil, nil, "", err
	}
	if len(ids) == 0 {
		return nil, nil, "", nil
	}
	chunkSize := b.client.maxObjectsInGet(len(ids))
	var objects []jmapEmail
	var missing []string
	var state string
	for start := 0; start < len(ids); start += chunkSize {
		end := min(start+chunkSize, len(ids))
		chunkIDs := ids[start:end]
		var result jmapGetPage
		args := map[string]interface{}{
			"accountId":  b.client.accountID,
			"ids":        chunkIDs,
			"properties": []string{"id", "blobId", "threadId", "mailboxIds", "keywords", "receivedAt", "messageId"},
		}
		if err := b.client.call(ctx, []string{coreCapability, mailCapability}, "Email/get", args, &result); err != nil {
			return nil, nil, "", err
		}
		if err := result.validate(b.client.accountID, chunkIDs); err != nil {
			return nil, nil, "", err
		}
		if err := b.validateEmailMailboxes(*result.List); err != nil {
			return nil, nil, "", err
		}
		objects = append(objects, (*result.List)...)
		missing = append(missing, (*result.NotFound)...)
		state = *result.State
	}
	return objects, missing, state, nil
}

func (b *Backend) getMessages(ctx context.Context, ids []string) ([]backend.Message, []string, error) {
	objects, missing, _, err := b.getEmailObjects(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	// Each worker can hold a maxMessageBytes MIME body. Four workers preserve
	// useful parallelism without allowing a pathological page to retain 1.2 GB.
	const downloadConcurrency = 4
	messages := make([]backend.Message, len(objects))
	errs := make([]error, len(objects))
	sem := make(chan struct{}, downloadConcurrency)
	var wg sync.WaitGroup
	for i := range objects {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return nil, nil, ctx.Err()
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			email := objects[i]
			body, err := b.downloadRaw(ctx, email)
			if err != nil {
				errs[i] = fmt.Errorf("download email %s: %w", email.ID, err)
				return
			}
			received, _ := time.Parse(time.RFC3339, email.ReceivedAt)
			messageID := ""
			if len(email.MessageID) > 0 {
				messageID = email.MessageID[0]
			}
			stableID := b.scopedEmailID(email.ID)
			messages[i] = backend.Message{
				StableID: stableID, MessageID: messageID, Ref: backend.RemoteRef{Folder: allMailStream, ID: stableID}, Raw: body,
				Flags: flagsFromKeywords(email.Keywords), Labels: b.labelsFor(email.MailboxIDs, email.Keywords), InternalDate: received,
			}
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, nil, err
		}
	}
	return messages, missing, nil
}

func (b *Backend) validateEmailMailboxes(emails []jmapEmail) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.mailboxes == nil {
		return nil
	}
	for _, email := range emails {
		for id := range email.MailboxIDs {
			if _, ok := b.mailboxes[id]; !ok {
				return fmt.Errorf("JMAP Email/get returned unknown mailbox %q for email %q", id, email.ID)
			}
		}
	}
	return nil
}

func (b *Backend) labelsFor(mailboxIDs, keywords map[string]bool) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	labels := make([]string, 0, len(mailboxIDs)+len(keywords))
	isDraft := keywords["$draft"]
	for id, present := range mailboxIDs {
		if !present {
			continue
		}
		if tag := b.mailboxToTag[id]; tag != "" {
			if isDraft && tag == "sent" {
				continue
			}
			labels = append(labels, tag)
		}
	}
	if isDraft {
		labels = append(labels, "draft")
	}
	for keyword, present := range keywords {
		if !present || isFlagKeyword(keyword) {
			continue
		}
		if tag, ok := decodeDurianKeyword(keyword); ok {
			_, _, canonicalRole := roleMailboxForTag(tag)
			if b.tagToID[tag] != "" || canonicalRole || isExplicitFlagTag(tag) || strings.HasPrefix(tag, "jmap-keyword/") {
				labels = append(labels, "jmap-keyword/"+keyword)
			} else {
				labels = append(labels, tag)
			}
		} else {
			labels = append(labels, "jmap-keyword/"+keyword)
		}
	}
	labels = uniqueStrings(labels)
	sort.Strings(labels)
	return labels
}

const durianKeywordPrefix = "durian-"

var keywordEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func encodeDurianKeyword(tag string) (string, error) {
	if tag == "" {
		return "", errors.New("empty tag cannot be encoded as a JMAP keyword")
	}
	keyword := durianKeywordPrefix + strings.ToLower(keywordEncoding.EncodeToString([]byte(tag)))
	if len(keyword) > 255 {
		return "", fmt.Errorf("tag %q is too long for a JMAP keyword", tag)
	}
	return keyword, nil
}

func decodeDurianKeyword(keyword string) (string, bool) {
	if !validJMAPKeyword(keyword) {
		return "", false
	}
	encoded, ok := strings.CutPrefix(keyword, durianKeywordPrefix)
	if !ok || encoded == "" {
		return "", false
	}
	decoded, err := keywordEncoding.DecodeString(strings.ToUpper(encoded))
	if err != nil || len(decoded) == 0 || !utf8.Valid(decoded) {
		return "", false
	}
	canonical, err := encodeDurianKeyword(string(decoded))
	return string(decoded), err == nil && canonical == keyword
}

func isFlagKeyword(keyword string) bool {
	return strings.HasPrefix(keyword, "$")
}

func isExplicitFlagTag(tag string) bool {
	return tag == "unread" || tag == "flagged" || tag == "replied"
}

func validJMAPKeyword(keyword string) bool {
	if len(keyword) == 0 || len(keyword) > 255 || keyword != strings.ToLower(keyword) {
		return false
	}
	for i := 0; i < len(keyword); i++ {
		char := keyword[i]
		if char < 0x21 || char > 0x7e {
			return false
		}
		switch char {
		case '(', ')', '{', ']', '%', '*', '"', '\\':
			return false
		}
	}
	return true
}

func validJMAPID(id string) bool {
	if len(id) == 0 || len(id) > 255 {
		return false
	}
	for i := 0; i < len(id); i++ {
		char := id[i]
		if (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' && char != '_' {
			return false
		}
	}
	return true
}

func keywordForTag(tag string) (string, error) {
	if keyword, ok := strings.CutPrefix(tag, "jmap-keyword/"); ok {
		if !validJMAPKeyword(keyword) {
			return "", fmt.Errorf("invalid native JMAP keyword %q", keyword)
		}
		if isFlagKeyword(keyword) {
			return "", fmt.Errorf("reserved JMAP system keyword %q cannot be used as a label", keyword)
		}
		return keyword, nil
	}
	return encodeDurianKeyword(tag)
}

func pointerEscape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func (b *Backend) downloadRaw(ctx context.Context, email jmapEmail) ([]byte, error) {
	if email.BlobID == "" {
		return nil, errors.New("email has no blobId")
	}
	r, err := b.client.download(ctx, email.BlobID, email.ID+".eml", "message/rfc822")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return readLimited(r, maxMessageBytes, "JMAP message")
}

// FetchBody streams the RFC 5322 body addressed by ref into w.
func (b *Backend) FetchBody(ctx context.Context, ref backend.RemoteRef, w io.Writer) error {
	if err := b.ensure(ctx); err != nil {
		return err
	}
	id, err := b.rawEmailID(ref.ID)
	if err != nil {
		return err
	}
	objects, _, _, err := b.getEmailObjects(ctx, []string{id})
	if err != nil {
		return err
	}
	if len(objects) == 0 {
		return fmt.Errorf("%w: JMAP email %s", backend.ErrRefGone, ref.ID)
	}
	r, err := b.client.download(ctx, objects[0].BlobID, id+".eml", "message/rfc822")
	if err != nil {
		return err
	}
	defer r.Close()
	written, err := io.CopyN(w, r, maxMessageBytes+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if written > maxMessageBytes {
		return fmt.Errorf("JMAP message exceeds %d bytes", maxMessageBytes)
	}
	return nil
}

func flagsFromKeywords(keywords map[string]bool) backend.Flags {
	return backend.Flags{
		Seen:      keywords["$seen"],
		Flagged:   keywords["$flagged"],
		Answered:  keywords["$answered"],
		Completed: keywords["$completed"],
	}
}

func flagsToKeywords(flags backend.Flags) map[string]bool {
	keywords := make(map[string]bool)
	if flags.Seen {
		keywords["$seen"] = true
	}
	if flags.Flagged {
		keywords["$flagged"] = true
	}
	if flags.Answered {
		keywords["$answered"] = true
	}
	if flags.Completed {
		keywords["$completed"] = true
	}
	return keywords
}

// ApplyFlags updates JMAP keywords corresponding to Durian flags.
func (b *Backend) ApplyFlags(ctx context.Context, ref backend.RemoteRef, add, remove backend.Flags) error {
	patch := make(map[string]interface{})
	setFlagPatch(patch, "$seen", add.Seen, remove.Seen)
	setFlagPatch(patch, "$flagged", add.Flagged, remove.Flagged)
	setFlagPatch(patch, "$answered", add.Answered, remove.Answered)
	setFlagPatch(patch, "$completed", add.Completed, remove.Completed)
	if len(patch) == 0 {
		return nil
	}
	return b.updateRefEmail(ctx, ref.ID, patch)
}

// ApplyTagMutation maps explicit local flag-tag intent directly to one JMAP
// keyword property patch. unread is the inverse of $seen.
func (b *Backend) ApplyTagMutation(ctx context.Context, ref backend.RemoteRef, tag string, add bool) error {
	patch := make(map[string]interface{})
	set := func(keyword string, present bool) {
		if present {
			patch["keywords/"+keyword] = true
		} else {
			patch["keywords/"+keyword] = nil
		}
	}
	switch tag {
	case "unread":
		set("$seen", !add)
	case "flagged":
		set("$flagged", add)
	case "replied":
		set("$answered", add)
	default:
		return fmt.Errorf("unsupported explicit JMAP tag mutation %q", tag)
	}
	return b.updateRefEmail(ctx, ref.ID, patch)
}

func setFlagPatch(patch map[string]interface{}, keyword string, add, remove bool) {
	if add {
		patch["keywords/"+keyword] = true
	} else if remove {
		patch["keywords/"+keyword] = nil
	}
}

// FetchFlags returns current JMAP keyword state for refs that still exist.
func (b *Backend) FetchFlags(ctx context.Context, _ string, refs []backend.RemoteRef) (map[string]backend.Flags, error) {
	if err := b.ensure(ctx); err != nil {
		return nil, err
	}
	ids, err := b.rawEmailIDs(refs)
	if err != nil {
		return nil, err
	}
	objects, _, _, err := b.getEmailObjects(ctx, ids)
	if err != nil {
		return nil, err
	}
	result := make(map[string]backend.Flags, len(objects))
	for _, email := range objects {
		result[b.scopedEmailID(email.ID)] = flagsFromKeywords(email.Keywords)
	}
	return result, nil
}

// Move adds the destination mailbox and removes the source mailbox membership.
func (b *Backend) Move(ctx context.Context, ref backend.RemoteRef, destFolder string) (backend.RemoteRef, error) {
	patch := map[string]interface{}{"mailboxIds/" + destFolder: true}
	if ref.Folder != "" && ref.Folder != allMailStream && ref.Folder != destFolder {
		patch["mailboxIds/"+ref.Folder] = nil
	}
	if err := b.updateRefEmail(ctx, ref.ID, patch); err != nil {
		return backend.RemoteRef{}, err
	}
	return backend.RemoteRef{Folder: destFolder, ID: ref.ID}, nil
}

// LabelTags returns canonical tags for every server mailbox.
func (b *Backend) LabelTags(ctx context.Context) ([]string, error) {
	if err := b.loadMailboxes(ctx); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	tags := make([]string, 0, len(b.tagToID))
	for tag := range b.tagToID {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags, nil
}

// ManagesLabelTag reports whether a local tag can be represented as a JMAP
// mailbox membership or custom keyword. Flag tags use ApplyTagMutation instead.
func (b *Backend) ManagesLabelTag(tag string) bool {
	if isExplicitFlagTag(tag) {
		return false
	}
	keyword, native := strings.CutPrefix(tag, "jmap-keyword/")
	if native {
		return validJMAPKeyword(keyword) && !isFlagKeyword(keyword)
	}
	_, err := encodeDurianKeyword(tag)
	return err == nil
}

// ApplyLabels updates mailbox memberships represented by canonical Durian tags.
func (b *Backend) ApplyLabels(ctx context.Context, ref backend.RemoteRef, add, remove []string) error {
	if err := b.loadMailboxes(ctx); err != nil {
		return err
	}
	id, err := b.rawEmailID(ref.ID)
	if err != nil {
		return err
	}
	for _, tag := range add {
		role, name, canonicalRole := roleMailboxForTag(tag)
		if !canonicalRole || b.mailboxIDForTag(tag) != "" {
			continue
		}
		if err := b.createRoleMailbox(ctx, role, name); err != nil {
			return fmt.Errorf("create JMAP %s mailbox: %w", role, err)
		}
		if b.mailboxIDForTag(tag) == "" {
			return fmt.Errorf("created JMAP %s mailbox could not be resolved", role)
		}
	}
	patch := make(map[string]interface{})
	b.mu.Lock()
	apply := func(tags []string, value interface{}) error {
		for _, tag := range tags {
			if tag == "draft" {
				patch["keywords/$draft"] = value
			}
			if !strings.HasPrefix(tag, "jmap-keyword/") {
				if id := b.tagToID[tag]; id != "" {
					patch["mailboxIds/"+id] = value
					continue
				}
				if _, _, canonicalRole := roleMailboxForTag(tag); canonicalRole {
					// A missing role on removal means there is no corresponding
					// membership to remove. Never turn canonical role intent into
					// a custom keyword.
					if value == nil {
						continue
					}
					return fmt.Errorf("JMAP role mailbox %q is unavailable", tag)
				}
			}
			keyword, err := keywordForTag(tag)
			if err != nil {
				return err
			}
			patch["keywords/"+pointerEscape(keyword)] = value
		}
		return nil
	}
	if err := apply(add, true); err != nil {
		b.mu.Unlock()
		return err
	}
	if err := apply(remove, nil); err != nil {
		b.mu.Unlock()
		return err
	}
	b.mu.Unlock()
	if len(patch) == 0 {
		return nil
	}
	objects, missing, _, err := b.getEmailObjects(ctx, []string{id})
	if err != nil {
		return fmt.Errorf("load JMAP mailbox membership: %w", err)
	}
	if len(missing) > 0 || len(objects) == 0 {
		return fmt.Errorf("%w: JMAP email %s", backend.ErrRefGone, ref.ID)
	}
	remaining := make(map[string]bool, len(objects[0].MailboxIDs))
	for id, present := range objects[0].MailboxIDs {
		if present {
			remaining[id] = true
		}
	}
	for path, value := range patch {
		if !strings.HasPrefix(path, "mailboxIds/") {
			continue
		}
		id := strings.TrimPrefix(path, "mailboxIds/")
		if value == nil {
			delete(remaining, id)
		} else {
			remaining[id] = true
		}
	}
	if len(remaining) == 0 {
		archiveID := b.mailboxIDForTag("archive")
		if archiveID == "" {
			if err := b.createArchiveMailbox(ctx); err != nil {
				return fmt.Errorf("create JMAP archive mailbox: %w", err)
			}
			archiveID = b.mailboxIDForTag("archive")
		}
		if archiveID == "" {
			return errors.New("created JMAP archive mailbox could not be resolved")
		}
		patch["mailboxIds/"+archiveID] = true
	}
	return b.updateEmail(ctx, id, patch)
}

func (b *Backend) createArchiveMailbox(ctx context.Context) error {
	return b.createRoleMailbox(ctx, "archive", "Archive")
}

func validateResponseAccount(method, accountID string, responseAccountID *string) error {
	if responseAccountID == nil || *responseAccountID != accountID {
		return fmt.Errorf("JMAP %s omitted required matching accountId", method)
	}
	return nil
}

type setResponseState struct {
	OldState json.RawMessage `json:"oldState"`
	NewState *string         `json:"newState"`
}

func validateSetResponseState(method string, state setResponseState) error {
	oldState := bytes.TrimSpace(state.OldState)
	if len(oldState) == 0 {
		return fmt.Errorf("JMAP %s omitted required oldState", method)
	}
	if !bytes.Equal(oldState, []byte("null")) {
		var value string
		if err := json.Unmarshal(oldState, &value); err != nil {
			return fmt.Errorf("JMAP %s returned invalid oldState", method)
		}
	}
	if state.NewState == nil {
		return fmt.Errorf("JMAP %s omitted required newState", method)
	}
	return nil
}

func mapKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func validateSingleOutcome(method, expected string, successKeys []string, failures map[string]methodError) (*methodError, error) {
	for _, key := range successKeys {
		if key != expected {
			return nil, fmt.Errorf("JMAP %s returned unexpected successful id %q", method, key)
		}
	}
	for key := range failures {
		if key != expected {
			return nil, fmt.Errorf("JMAP %s returned unexpected failed id %q", method, key)
		}
	}
	failure, failed := failures[expected]
	if failed && failure.Type == "" {
		return nil, fmt.Errorf("JMAP %s returned a failed outcome without a type for %q", method, expected)
	}
	if len(successKeys) == 1 && !failed {
		return nil, nil
	}
	if len(successKeys) == 0 && failed {
		return &failure, nil
	}
	return nil, fmt.Errorf("JMAP %s did not return exactly one outcome for %q", method, expected)
}

// createRoleMailbox creates a role mailbox the account is missing and reloads
// the mailbox table so the new id is resolvable by tag.
func (b *Backend) createRoleMailbox(ctx context.Context, role, name string) error {
	var result struct {
		setResponseState
		AccountID *string `json:"accountId"`
		Created   map[string]struct {
			ID string `json:"id"`
		} `json:"created"`
		NotCreated map[string]methodError `json:"notCreated"`
	}
	args := map[string]interface{}{
		"accountId": b.client.accountID,
		"create": map[string]interface{}{
			role: map[string]interface{}{
				"name":         name,
				"role":         role,
				"isSubscribed": true,
			},
		},
	}
	if err := b.client.call(ctx, []string{coreCapability, mailCapability}, "Mailbox/set", args, &result); err != nil {
		return err
	}
	if err := validateResponseAccount("Mailbox/set", b.client.accountID, result.AccountID); err != nil {
		return err
	}
	if err := validateSetResponseState("Mailbox/set", result.setResponseState); err != nil {
		return err
	}
	createErr, err := validateSingleOutcome("Mailbox/set", role, mapKeys(result.Created), result.NotCreated)
	if err != nil {
		return err
	}
	if createErr != nil {
		return createErr
	}
	if !validJMAPID(result.Created[role].ID) {
		return fmt.Errorf("Mailbox/set returned no created %s mailbox", role)
	}
	return b.loadMailboxes(ctx)
}

// destroyEmail removes an email the caller imported but could not complete an
// operation for, so a retry does not accumulate copies.
func (b *Backend) destroyEmail(ctx context.Context, id string) error {
	args := map[string]interface{}{
		"accountId": b.client.accountID,
		"destroy":   []string{id},
	}
	var result struct {
		setResponseState
		AccountID    *string                `json:"accountId"`
		Destroyed    []string               `json:"destroyed"`
		NotDestroyed map[string]methodError `json:"notDestroyed"`
	}
	if err := b.client.call(ctx, []string{coreCapability, mailCapability}, "Email/set", args, &result); err != nil {
		return err
	}
	if err := validateResponseAccount("Email/set", b.client.accountID, result.AccountID); err != nil {
		return err
	}
	if err := validateSetResponseState("Email/set", result.setResponseState); err != nil {
		return err
	}
	destroyErr, err := validateSingleOutcome("Email/set", id, result.Destroyed, result.NotDestroyed)
	if err != nil {
		return err
	}
	if destroyErr != nil {
		return destroyErr
	}
	return nil
}

func (b *Backend) updateEmail(ctx context.Context, id string, patch map[string]interface{}) error {
	if err := b.ensure(ctx); err != nil {
		return err
	}
	var result struct {
		setResponseState
		AccountID  *string                    `json:"accountId"`
		Updated    map[string]json.RawMessage `json:"updated"`
		NotUpdated map[string]methodError     `json:"notUpdated"`
	}
	args := map[string]interface{}{
		"accountId": b.client.accountID,
		"update":    map[string]interface{}{id: patch},
	}
	if err := b.client.call(ctx, []string{coreCapability, mailCapability}, "Email/set", args, &result); err != nil {
		return err
	}
	if err := validateResponseAccount("Email/set", b.client.accountID, result.AccountID); err != nil {
		return err
	}
	if err := validateSetResponseState("Email/set", result.setResponseState); err != nil {
		return err
	}
	setErr, err := validateSingleOutcome("Email/set", id, mapKeys(result.Updated), result.NotUpdated)
	if err != nil {
		return err
	}
	if setErr != nil {
		if setErr.Type == "notFound" {
			return fmt.Errorf("%w: JMAP email %s", backend.ErrRefGone, id)
		}
		return setErr
	}
	return validateUpdatedValue("JMAP Email/set", id, result.Updated[id])
}

func validateUpdatedValue(method, id string, value json.RawMessage) error {
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil
	}
	var computed map[string]json.RawMessage
	if err := json.Unmarshal(value, &computed); err != nil || computed == nil {
		return fmt.Errorf("%s returned an invalid updated value for %q", method, id)
	}
	return nil
}

func (b *Backend) updateRefEmail(ctx context.Context, refID string, patch map[string]interface{}) error {
	if err := b.ensure(ctx); err != nil {
		return err
	}
	id, err := b.rawEmailID(refID)
	if err != nil {
		return err
	}
	return b.updateEmail(ctx, id, patch)
}

// Append uploads and imports an RFC 5322 message into folder.
func (b *Backend) Append(ctx context.Context, folder string, flags backend.Flags, msg []byte) (backend.RemoteRef, error) {
	return b.append(ctx, folder, flags, msg, false)
}

func (b *Backend) append(ctx context.Context, folder string, flags backend.Flags, msg []byte, draft bool) (backend.RemoteRef, error) {
	if err := b.loadMailboxes(ctx); err != nil {
		return backend.RemoteRef{}, err
	}
	keywords := flagsToKeywords(flags)
	b.mu.Lock()
	if mailbox, ok := b.mailboxes[folder]; ok && strings.EqualFold(mailbox.Role, "drafts") {
		keywords["$draft"] = true
	}
	b.mu.Unlock()
	if draft {
		keywords["$draft"] = true
	}
	blobID, err := b.client.upload(ctx, msg, "message/rfc822")
	if err != nil {
		return backend.RemoteRef{}, fmt.Errorf("upload message: %w", err)
	}
	var result struct {
		setResponseState
		AccountID  *string                 `json:"accountId"`
		Created    map[string]createdEmail `json:"created"`
		NotCreated map[string]methodError  `json:"notCreated"`
	}
	args := map[string]interface{}{
		"accountId": b.client.accountID,
		"emails": map[string]interface{}{
			"0": map[string]interface{}{
				"blobId":     blobID,
				"mailboxIds": map[string]bool{folder: true},
				"keywords":   keywords,
				"receivedAt": time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	callErr := b.client.call(ctx, []string{coreCapability, mailCapability}, "Email/import", args, &result)
	if errors.Is(callErr, errJMAPLocalPermanent) {
		return backend.RemoteRef{}, callErr
	}
	var protocolErr *protocolError
	if errors.As(callErr, &protocolErr) {
		return backend.RemoteRef{}, fmt.Errorf("%w: %w", errEmailCreationOutcomeUnknown, callErr)
	}
	if err := validateResponseAccount("Email/import", b.client.accountID, result.AccountID); err != nil {
		if definiteMutationRejection(callErr) {
			return backend.RemoteRef{}, callErr
		}
		if callErr != nil {
			err = errors.Join(err, callErr)
		}
		return backend.RemoteRef{}, fmt.Errorf("%w: %w", errEmailCreationOutcomeUnknown, err)
	}
	if err := validateSetResponseState("Email/import", result.setResponseState); err != nil {
		return backend.RemoteRef{}, fmt.Errorf("%w: %w", errEmailCreationOutcomeUnknown, err)
	}
	importErr, err := validateSingleOutcome("Email/import", "0", mapKeys(result.Created), result.NotCreated)
	if err != nil {
		if callErr != nil {
			err = errors.Join(err, callErr)
		}
		return backend.RemoteRef{}, fmt.Errorf("%w: %w", errEmailCreationOutcomeUnknown, err)
	}
	if importErr != nil {
		return backend.RemoteRef{}, importErr
	}
	created, ok := result.Created["0"]
	if !ok {
		return backend.RemoteRef{}, fmt.Errorf("%w: Email/import omitted the created Email", errEmailCreationOutcomeUnknown)
	}
	if err := validateCreatedEmail("Email/import", created); err != nil {
		return backend.RemoteRef{}, fmt.Errorf("%w: %w", errEmailCreationOutcomeUnknown, err)
	}
	return backend.RemoteRef{Folder: folder, ID: b.scopedEmailID(created.ID)}, nil
}

// Send submits a raw RFC 5322 message through JMAP Submission.
func (b *Backend) Send(ctx context.Context, msg []byte) error {
	return b.sendRaw(ctx, msg)
}

// Watch consumes the account-wide JMAP EventSource stream until ctx ends.
func (b *Backend) Watch(ctx context.Context, _ string, onChange func()) error {
	if err := b.ensure(ctx); err != nil {
		return err
	}
	return b.client.watch(ctx, onChange)
}

// Capabilities reports JMAP's account-wide push and label-native delta support.
func (b *Backend) Capabilities() backend.Capabilities {
	return backend.Capabilities{
		PushWatch:                      true,
		BodyBatchLimit:                 4,
		InitialSnapshotIsAuthoritative: true,
		FlagChangesInDelta:             true,
		LabelsAreTags:                  true,
	}
}

// Close releases idle HTTP connections held by the backend.
func (b *Backend) Close() error {
	if b.client != nil && b.client.httpClient != nil {
		b.client.httpClient.CloseIdleConnections()
	}
	return nil
}

func (b *Backend) deletions(folder string, ids []string) []backend.Deletion {
	result := make([]backend.Deletion, 0, len(ids))
	for _, id := range ids {
		result = append(result, backend.Deletion{Ref: backend.RemoteRef{Folder: folder, ID: b.scopedEmailID(id)}})
	}
	return result
}

func (b *Backend) presentRefs(folder string, ids, missing []string) []backend.RemoteRef {
	missingSet := make(map[string]struct{}, len(missing))
	for _, id := range missing {
		missingSet[id] = struct{}{}
	}
	capacity := len(ids) - len(missingSet)
	if capacity < 0 {
		capacity = 0
	}
	result := make([]backend.RemoteRef, 0, capacity)
	for _, id := range ids {
		if _, absent := missingSet[id]; !absent {
			result = append(result, backend.RemoteRef{Folder: folder, ID: b.scopedEmailID(id)})
		}
	}
	return result
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
