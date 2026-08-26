// Package jmapbackend implements Durian's provider-neutral mail backend on
// RFC 8620 (JMAP Core) and RFC 8621 (JMAP Mail and Submission).
package jmapbackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/keychain"
)

const allMailStream = "JMAP-ALL"

var getCredential = keychain.GetPassword

type Backend struct {
	account *config.AccountConfig
	client  *client

	mu          sync.Mutex
	initialized bool
	initErr     error
	mailboxes   map[string]jmapMailbox
	tagToID     map[string]string
}

var (
	_ backend.Backend     = (*Backend)(nil)
	_ backend.LabelWriter = (*Backend)(nil)
)

type jmapMailbox struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ParentID     string `json:"parentId"`
	Role         string `json:"role"`
	IsSubscribed bool   `json:"isSubscribed"`
}

type jmapEmail struct {
	ID         string          `json:"id"`
	BlobID     string          `json:"blobId"`
	ThreadID   string          `json:"threadId"`
	MailboxIDs map[string]bool `json:"mailboxIds"`
	Keywords   map[string]bool `json:"keywords"`
	ReceivedAt string          `json:"receivedAt"`
	MessageID  []string        `json:"messageId"`
}

type jmapCursor struct {
	Position   int    `json:"position,omitempty"`
	Snapshot   string `json:"snapshot,omitempty"`
	EmailState string `json:"emailState,omitempty"`
}

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
			httpClient: &http.Client{Timeout: 90 * time.Second},
			sessionURL: account.JMAP.SessionURL,
			credential: credential{mode: mode, username: username},
		},
	}, nil
}

func (b *Backend) ensure(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.initialized {
		return b.initErr
	}
	b.initialized = true
	secret, err := getCredential(keychain.PasswordKeychainService, b.account.Email)
	if err != nil {
		if errors.Is(err, keychain.ErrNotFound) {
			b.initErr = fmt.Errorf("no JMAP credential stored for %s; run durian auth login %s", b.account.Email, b.account.GetAliasOrName())
		} else {
			b.initErr = fmt.Errorf("load JMAP credential: %w", err)
		}
		return b.initErr
	}
	b.client.credential.secret = secret
	b.initErr = b.client.discover(ctx)
	return b.initErr
}

func (b *Backend) loadMailboxes(ctx context.Context) error {
	if err := b.ensure(ctx); err != nil {
		return err
	}
	var result struct {
		List []jmapMailbox `json:"list"`
	}
	args := map[string]interface{}{
		"accountId":  b.client.accountID,
		"properties": []string{"id", "name", "parentId", "role", "isSubscribed"},
	}
	if err := b.client.call(ctx, []string{coreCapability, mailCapability}, "Mailbox/get", args, &result); err != nil {
		return err
	}
	mailboxes := make(map[string]jmapMailbox, len(result.List))
	tagToID := make(map[string]string, len(result.List))
	for _, mailbox := range result.List {
		mailboxes[mailbox.ID] = mailbox
		tag := mailboxTag(mailbox)
		if tag != "" {
			tagToID[strings.ToLower(tag)] = mailbox.ID
		}
	}
	b.mu.Lock()
	b.mailboxes = mailboxes
	b.tagToID = tagToID
	b.mu.Unlock()
	return nil
}

func mailboxTag(mailbox jmapMailbox) string {
	switch strings.ToLower(mailbox.Role) {
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
	return strings.TrimSpace(mailbox.Name)
}

func (b *Backend) FetchFolders(ctx context.Context) ([]backend.Folder, error) {
	if err := b.loadMailboxes(ctx); err != nil {
		return nil, fmt.Errorf("load JMAP mailboxes: %w", err)
	}
	return []backend.Folder{{Name: allMailStream, Display: "All Mail", Role: backend.RoleAll, Selectable: true}}, nil
}

func decodeCursor(cursor backend.Cursor) jmapCursor {
	var state jmapCursor
	if len(cursor) != 0 {
		_ = json.Unmarshal(cursor, &state)
	}
	return state
}

func encodeCursor(cursor jmapCursor) backend.Cursor {
	encoded, _ := json.Marshal(cursor)
	return encoded
}

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
	if state.EmailState == "" {
		return b.initialPage(ctx, state, limit)
	}
	return b.changesPage(ctx, state, limit)
}

func (b *Backend) initialPage(ctx context.Context, state jmapCursor, limit int) (backend.FetchResult, error) {
	if state.Snapshot == "" {
		var err error
		state.Snapshot, err = b.currentEmailState(ctx)
		if err != nil {
			return backend.FetchResult{}, fmt.Errorf("snapshot JMAP email state: %w", err)
		}
	}
	var query struct {
		QueryState string   `json:"queryState"`
		Position   int      `json:"position"`
		IDs        []string `json:"ids"`
		Total      int      `json:"total"`
	}
	args := map[string]interface{}{
		"accountId":      b.client.accountID,
		"position":       state.Position,
		"limit":          limit,
		"calculateTotal": true,
		"sort":           []map[string]interface{}{{"property": "receivedAt", "isAscending": false}},
	}
	if err := b.client.call(ctx, []string{coreCapability, mailCapability}, "Email/query", args, &query); err != nil {
		return backend.FetchResult{}, err
	}
	messages, missing, err := b.getMessages(ctx, query.IDs)
	if err != nil {
		return backend.FetchResult{}, err
	}
	deleted := deletions(allMailStream, missing)
	next := state.Position + len(query.IDs)
	hasMore := next < query.Total && len(query.IDs) > 0
	if hasMore {
		state.Position = next
	} else {
		state = jmapCursor{EmailState: state.Snapshot}
	}
	return backend.FetchResult{Messages: messages, Deleted: deleted, Cursor: encodeCursor(state), HasMore: hasMore}, nil
}

func (b *Backend) changesPage(ctx context.Context, state jmapCursor, limit int) (backend.FetchResult, error) {
	var changes struct {
		OldState       string   `json:"oldState"`
		NewState       string   `json:"newState"`
		HasMoreChanges bool     `json:"hasMoreChanges"`
		Created        []string `json:"created"`
		Updated        []string `json:"updated"`
		Destroyed      []string `json:"destroyed"`
	}
	args := map[string]interface{}{
		"accountId":  b.client.accountID,
		"sinceState": state.EmailState,
		"maxChanges": limit,
	}
	if err := b.client.call(ctx, []string{coreCapability, mailCapability}, "Email/changes", args, &changes); err != nil {
		var methodErr *methodError
		if errors.As(err, &methodErr) && methodErr.Type == "cannotCalculateChanges" {
			return b.initialPage(ctx, jmapCursor{}, limit)
		}
		return backend.FetchResult{}, err
	}
	ids := append(append([]string(nil), changes.Created...), changes.Updated...)
	ids = uniqueStrings(ids)
	messages, missing, err := b.getMessages(ctx, ids)
	if err != nil {
		return backend.FetchResult{}, err
	}
	destroyed := append(append([]string(nil), changes.Destroyed...), missing...)
	state.EmailState = changes.NewState
	return backend.FetchResult{
		Messages: messages,
		Deleted:  deletions(allMailStream, uniqueStrings(destroyed)),
		Cursor:   encodeCursor(state),
		HasMore:  changes.HasMoreChanges,
	}, nil
}

func (b *Backend) currentEmailState(ctx context.Context) (string, error) {
	var result struct {
		State string `json:"state"`
	}
	args := map[string]interface{}{"accountId": b.client.accountID, "ids": []string{}, "properties": []string{"id"}}
	if err := b.client.call(ctx, []string{coreCapability, mailCapability}, "Email/get", args, &result); err != nil {
		return "", err
	}
	if result.State == "" {
		return "", errors.New("Email/get returned an empty state")
	}
	return result.State, nil
}

func (b *Backend) getEmailObjects(ctx context.Context, ids []string) ([]jmapEmail, []string, string, error) {
	if err := b.ensure(ctx); err != nil {
		return nil, nil, "", err
	}
	if len(ids) == 0 {
		return nil, nil, "", nil
	}
	var result struct {
		State    string      `json:"state"`
		List     []jmapEmail `json:"list"`
		NotFound []string    `json:"notFound"`
	}
	args := map[string]interface{}{
		"accountId":  b.client.accountID,
		"ids":        ids,
		"properties": []string{"id", "blobId", "threadId", "mailboxIds", "keywords", "receivedAt", "messageId"},
	}
	if err := b.client.call(ctx, []string{coreCapability, mailCapability}, "Email/get", args, &result); err != nil {
		return nil, nil, "", err
	}
	return result.List, result.NotFound, result.State, nil
}

func (b *Backend) getMessages(ctx context.Context, ids []string) ([]backend.Message, []string, error) {
	objects, missing, _, err := b.getEmailObjects(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	messages := make([]backend.Message, 0, len(objects))
	for _, email := range objects {
		body, err := b.downloadRaw(ctx, email)
		if err != nil {
			return nil, nil, fmt.Errorf("download email %s: %w", email.ID, err)
		}
		received, _ := time.Parse(time.RFC3339, email.ReceivedAt)
		messageID := ""
		if len(email.MessageID) > 0 {
			messageID = email.MessageID[0]
		}
		messages = append(messages, backend.Message{
			MessageID:    messageID,
			Ref:          backend.RemoteRef{Folder: allMailStream, ID: email.ID},
			Raw:          body,
			Flags:        flagsFromKeywords(email.Keywords),
			Labels:       b.labelsFor(email.MailboxIDs),
			InternalDate: received,
		})
	}
	return messages, missing, nil
}

func (b *Backend) labelsFor(mailboxIDs map[string]bool) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	labels := make([]string, 0, len(mailboxIDs))
	for id, present := range mailboxIDs {
		if !present {
			continue
		}
		if mailbox, ok := b.mailboxes[id]; ok {
			if tag := mailboxTag(mailbox); tag != "" {
				labels = append(labels, tag)
			}
		}
	}
	sort.Strings(labels)
	return labels
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
	return io.ReadAll(r)
}

func (b *Backend) FetchBody(ctx context.Context, ref backend.RemoteRef, w io.Writer) error {
	if err := b.ensure(ctx); err != nil {
		return err
	}
	objects, _, _, err := b.getEmailObjects(ctx, []string{ref.ID})
	if err != nil {
		return err
	}
	if len(objects) == 0 {
		return fmt.Errorf("%w: JMAP email %s", backend.ErrRefGone, ref.ID)
	}
	r, err := b.client.download(ctx, objects[0].BlobID, ref.ID+".eml", "message/rfc822")
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = io.Copy(w, r)
	return err
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

func (b *Backend) ApplyFlags(ctx context.Context, ref backend.RemoteRef, add, remove backend.Flags) error {
	patch := make(map[string]interface{})
	setFlagPatch(patch, "$seen", add.Seen, remove.Seen)
	setFlagPatch(patch, "$flagged", add.Flagged, remove.Flagged)
	setFlagPatch(patch, "$answered", add.Answered, remove.Answered)
	setFlagPatch(patch, "$completed", add.Completed, remove.Completed)
	if len(patch) == 0 {
		return nil
	}
	return b.updateEmail(ctx, ref.ID, patch)
}

func setFlagPatch(patch map[string]interface{}, keyword string, add, remove bool) {
	if add {
		patch["keywords/"+keyword] = true
	} else if remove {
		patch["keywords/"+keyword] = nil
	}
}

func (b *Backend) FetchFlags(ctx context.Context, _ string, refs []backend.RemoteRef) (map[string]backend.Flags, error) {
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.ID)
	}
	objects, _, _, err := b.getEmailObjects(ctx, ids)
	if err != nil {
		return nil, err
	}
	result := make(map[string]backend.Flags, len(objects))
	for _, email := range objects {
		result[email.ID] = flagsFromKeywords(email.Keywords)
	}
	return result, nil
}

func (b *Backend) Move(ctx context.Context, ref backend.RemoteRef, destFolder string) (backend.RemoteRef, error) {
	patch := map[string]interface{}{"mailboxIds/" + destFolder: true}
	if ref.Folder != "" && ref.Folder != allMailStream && ref.Folder != destFolder {
		patch["mailboxIds/"+ref.Folder] = nil
	}
	if err := b.updateEmail(ctx, ref.ID, patch); err != nil {
		return backend.RemoteRef{}, err
	}
	return backend.RemoteRef{Folder: destFolder, ID: ref.ID}, nil
}

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

func (b *Backend) ApplyLabels(ctx context.Context, ref backend.RemoteRef, add, remove []string) error {
	if err := b.loadMailboxes(ctx); err != nil {
		return err
	}
	for _, tag := range add {
		if strings.EqualFold(tag, "archive") && b.mailboxIDForTag("archive") == "" {
			if err := b.createArchiveMailbox(ctx); err != nil {
				return fmt.Errorf("create JMAP archive mailbox: %w", err)
			}
			break
		}
	}
	patch := make(map[string]interface{})
	b.mu.Lock()
	for _, tag := range add {
		if id := b.tagToID[strings.ToLower(tag)]; id != "" {
			patch["mailboxIds/"+id] = true
		}
	}
	for _, tag := range remove {
		if id := b.tagToID[strings.ToLower(tag)]; id != "" {
			patch["mailboxIds/"+id] = nil
		}
	}
	b.mu.Unlock()
	if len(patch) == 0 {
		return nil
	}
	return b.updateEmail(ctx, ref.ID, patch)
}

func (b *Backend) createArchiveMailbox(ctx context.Context) error {
	var result struct {
		Created map[string]struct {
			ID string `json:"id"`
		} `json:"created"`
		NotCreated map[string]methodError `json:"notCreated"`
	}
	args := map[string]interface{}{
		"accountId": b.client.accountID,
		"create": map[string]interface{}{
			"archive": map[string]interface{}{
				"name":         "Archive",
				"role":         "archive",
				"isSubscribed": true,
			},
		},
	}
	if err := b.client.call(ctx, []string{coreCapability, mailCapability}, "Mailbox/set", args, &result); err != nil {
		return err
	}
	if createErr, ok := result.NotCreated["archive"]; ok {
		return &createErr
	}
	if result.Created["archive"].ID == "" {
		return errors.New("Mailbox/set returned no created archive mailbox")
	}
	return b.loadMailboxes(ctx)
}

func (b *Backend) updateEmail(ctx context.Context, id string, patch map[string]interface{}) error {
	if err := b.ensure(ctx); err != nil {
		return err
	}
	var result struct {
		NotUpdated map[string]methodError `json:"notUpdated"`
	}
	args := map[string]interface{}{
		"accountId": b.client.accountID,
		"update":    map[string]interface{}{id: patch},
	}
	if err := b.client.call(ctx, []string{coreCapability, mailCapability}, "Email/set", args, &result); err != nil {
		return err
	}
	if setErr, ok := result.NotUpdated[id]; ok {
		if setErr.Type == "notFound" {
			return fmt.Errorf("%w: JMAP email %s", backend.ErrRefGone, id)
		}
		return &setErr
	}
	return nil
}

func (b *Backend) Append(ctx context.Context, folder string, flags backend.Flags, msg []byte) (backend.RemoteRef, error) {
	if err := b.loadMailboxes(ctx); err != nil {
		return backend.RemoteRef{}, err
	}
	keywords := flagsToKeywords(flags)
	b.mu.Lock()
	if mailbox, ok := b.mailboxes[folder]; ok && strings.EqualFold(mailbox.Role, "drafts") {
		keywords["$draft"] = true
	}
	b.mu.Unlock()
	blobID, err := b.client.upload(ctx, msg, "message/rfc822")
	if err != nil {
		return backend.RemoteRef{}, fmt.Errorf("upload message: %w", err)
	}
	var result struct {
		Created map[string]struct {
			ID string `json:"id"`
		} `json:"created"`
		NotCreated map[string]methodError `json:"notCreated"`
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
	if err := b.client.call(ctx, []string{coreCapability, mailCapability}, "Email/import", args, &result); err != nil {
		return backend.RemoteRef{}, err
	}
	if importErr, ok := result.NotCreated["0"]; ok {
		return backend.RemoteRef{}, &importErr
	}
	created, ok := result.Created["0"]
	if !ok || created.ID == "" {
		return backend.RemoteRef{}, errors.New("Email/import returned no created email")
	}
	return backend.RemoteRef{Folder: folder, ID: created.ID}, nil
}

func (b *Backend) Send(ctx context.Context, msg []byte) error {
	return b.sendRaw(ctx, msg)
}

func (b *Backend) Watch(ctx context.Context, _ string, onChange func()) error {
	if err := b.ensure(ctx); err != nil {
		return err
	}
	return b.client.watch(ctx, onChange)
}

func (b *Backend) Capabilities() backend.Capabilities {
	return backend.Capabilities{
		ServerSideSent:     true,
		PushWatch:          true,
		FlagChangesInDelta: true,
		LabelsAreTags:      true,
	}
}

func (b *Backend) Close() error {
	if b.client != nil && b.client.httpClient != nil {
		b.client.httpClient.CloseIdleConnections()
	}
	return nil
}

func deletions(folder string, ids []string) []backend.Deletion {
	result := make([]backend.Deletion, 0, len(ids))
	for _, id := range ids {
		result = append(result, backend.Deletion{Ref: backend.RemoteRef{Folder: folder, ID: id}})
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
