// Package imapbackend adapts the existing IMAP client (cli/internal/imap) to
// the provider-agnostic backend.Backend interface. It is a strangler-fig
// wrapper: all protocol work is delegated to imap.Client, no behavior of the
// existing syncer is changed, and both code paths coexist.
//
// Cursor encoding: the per-folder backend.Cursor is a JSON snapshot of
// imap.MailboxState (UIDVALIDITY, synced UID set, per-UID flags and
// UID<->Message-ID maps). An empty cursor, or a UIDVALIDITY mismatch on the
// server, triggers a full resync of the folder.
package imapbackend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"

	goimap "github.com/emersion/go-imap"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/imap"
)

// roleMappings pairs IMAP SPECIAL-USE roles with their backend.Role in a fixed
// order, so role resolution is deterministic.
var roleMappings = []struct {
	imapRole imap.SpecialUseRole
	role     backend.Role
}{
	{imap.RoleSent, backend.RoleSent},
	{imap.RoleDrafts, backend.RoleDrafts},
	{imap.RoleTrash, backend.RoleTrash},
	{imap.RoleJunk, backend.RoleJunk},
	{imap.RoleArchive, backend.RoleArchive},
	{imap.RoleAll, backend.RoleAll},
}

// Backend implements backend.Backend on top of an imap.Client.
type Backend struct {
	account *config.AccountConfig
	client  *imap.Client
	// ownsClient reports whether this Backend created the connection (New) and
	// may therefore reconnect it on a dropped socket. When the client is
	// caller-owned (NewWithClient, e.g. a watcher's IDLE loop), reconnecting
	// would open a new socket that aggressive servers like M365 reject, so we
	// never do it — matching the legacy syncer's ownsClient guard.
	ownsClient bool
}

// Compile-time check that Backend satisfies the interface.
var _ backend.Backend = (*Backend)(nil)

// New creates a connected, authenticated IMAP backend for the given account.
func New(account *config.AccountConfig) (*Backend, error) {
	client := imap.NewClient(account)

	if err := client.Connect(); err != nil {
		return nil, fmt.Errorf("failed to connect IMAP backend for %s: %w", account.Email, err)
	}
	if err := client.Authenticate(); err != nil {
		client.Close()
		return nil, fmt.Errorf("failed to authenticate IMAP backend for %s: %w", account.Email, err)
	}

	return &Backend{account: account, client: client, ownsClient: true}, nil
}

// NewWithClient wraps an already-connected imap.Client (e.g. so a watcher can
// reuse a connection). Close still closes the underlying client. The connection
// is treated as caller-owned, so this Backend will not reconnect it on failure.
func NewWithClient(account *config.AccountConfig, client *imap.Client) *Backend {
	return &Backend{account: account, client: client, ownsClient: false}
}

// MARK: - Folders

// FetchFolders returns the syncable mailboxes with resolved special-use roles.
// Role detection delegates to imap.Client.FindMailboxByRole (SPECIAL-USE
// attribute with common-name fallback); INBOX is matched by name per RFC 3501.
func (b *Backend) FetchFolders(_ context.Context) ([]backend.Folder, error) {
	var folders []backend.Folder
	err := b.withReconnect(func() error {
		f, e := b.fetchFoldersOnce()
		if e != nil {
			return e
		}
		folders = f
		return nil
	})
	return folders, err
}

// fetchFoldersOnce performs one folder-listing pass (no reconnect handling).
func (b *Backend) fetchFoldersOnce() ([]backend.Folder, error) {
	names, err := b.client.GetSyncMailboxes()
	if err != nil {
		return nil, fmt.Errorf("failed to list sync mailboxes: %w", err)
	}

	// Resolve special-use roles. A mailbox keeps the first role that claims it.
	roleByName := make(map[string]backend.Role)
	for _, m := range roleMappings {
		name, err := b.client.FindMailboxByRole(m.imapRole)
		if err != nil {
			continue // Role not present on this server
		}
		if _, taken := roleByName[name]; !taken {
			roleByName[name] = m.role
		}
	}

	folders := make([]backend.Folder, 0, len(names))
	for _, name := range names {
		role := roleByName[name]
		if strings.EqualFold(name, "INBOX") {
			role = backend.RoleInbox
		}
		folders = append(folders, backend.Folder{
			Name:    name,
			Display: name,
			Role:    role,
			// GetSyncMailboxes already filters \Noselect containers.
			Selectable: true,
		})
	}

	slog.Debug("Fetched folders", "module", "IMAPBACKEND", "count", len(folders))
	return folders, nil
}

// MARK: - Messages

// FetchMessages returns the changes in folder since cursor. The cursor is a
// JSON-encoded imap.MailboxState; empty cursor or a UIDVALIDITY change resets
// it and treats every server UID as new (full resync). New UIDs are fetched
// newest-first, capped at limit (limit <= 0 means no cap).
func (b *Backend) FetchMessages(_ context.Context, folder string, cursor backend.Cursor, limit int) (backend.FetchResult, error) {
	var result backend.FetchResult
	// Retry-safe: fetchMessagesOnce re-decodes the cursor and re-selects the
	// folder on every call, so a reconnect-and-retry restarts from clean state.
	err := b.withReconnect(func() error {
		r, e := b.fetchMessagesOnce(folder, cursor, limit)
		if e != nil {
			return e
		}
		result = r
		return nil
	})
	return result, err
}

// fetchMessagesOnce performs one incremental fetch pass (no reconnect handling).
func (b *Backend) fetchMessagesOnce(folder string, cursor backend.Cursor, limit int) (backend.FetchResult, error) {
	var result backend.FetchResult

	state, err := decodeCursor(cursor)
	if err != nil {
		return result, fmt.Errorf("failed to decode cursor for %s: %w", folder, err)
	}

	status, err := b.client.SelectMailbox(folder)
	if err != nil {
		return result, fmt.Errorf("failed to select %s: %w", folder, err)
	}

	if state.UIDValidity == 0 || state.NeedsFullResync(status.UidValidity) {
		if state.UIDValidity != 0 {
			slog.Info("UIDVALIDITY changed, full resync", "module", "IMAPBACKEND",
				"folder", folder, "old", state.UIDValidity, "new", status.UidValidity)
		}
		state.Reset(status.UidValidity)
	}

	serverUIDs, err := b.client.SearchAll()
	if err != nil {
		return result, fmt.Errorf("failed to search %s: %w", folder, err)
	}

	newUIDs := state.GetUnsyncedUIDs(serverUIDs)
	deletedUIDs := state.GetDeletedUIDs(serverUIDs)

	// Newest first (highest UID first).
	sort.Slice(newUIDs, func(i, j int) bool { return newUIDs[i] > newUIDs[j] })

	if limit > 0 && len(newUIDs) > limit {
		newUIDs = newUIDs[:limit]
		result.HasMore = true
	}

	if len(newUIDs) > 0 {
		fetched, err := b.client.FetchMessages(newUIDs)
		if err != nil {
			return result, fmt.Errorf("failed to fetch messages in %s: %w", folder, err)
		}

		for _, msg := range fetched {
			raw := readRawBody(msg.Body)
			if len(raw) == 0 {
				slog.Warn("Message has no body data, skipping", "module", "IMAPBACKEND",
					"folder", folder, "uid", msg.Uid)
				continue
			}

			messageID := extractMessageID(raw)
			if messageID == "" {
				// Synthetic Message-ID so the message is not lost (mirrors syncer behavior).
				messageID = fmt.Sprintf("durian-synthetic-%d-%s@%s", msg.Uid, folder, b.account.AccountIdentifier())
				slog.Warn("Message has no Message-ID, using synthetic ID", "module", "IMAPBACKEND",
					"folder", folder, "uid", msg.Uid, "synthetic_id", messageID)
			}

			flagState := imap.FlagStateFromIMAP(msg.Flags)

			result.Messages = append(result.Messages, backend.Message{
				MessageID:    messageID,
				Ref:          backend.RemoteRef{Folder: folder, ID: formatUID(msg.Uid)},
				Raw:          raw,
				Flags:        toBackendFlags(flagState),
				InternalDate: msg.InternalDate,
			})

			state.AddSyncedUID(msg.Uid)
			state.SetMessageFlags(msg.Uid, flagState)
			state.SetMessageID(msg.Uid, messageID)
		}
	}

	for _, uid := range deletedUIDs {
		result.Deleted = append(result.Deleted, backend.RemoteRef{Folder: folder, ID: formatUID(uid)})
	}
	for _, uid := range deletedUIDs {
		state.RemoveSyncedUID(uid)
	}

	newCursor, err := encodeCursor(state)
	if err != nil {
		return result, fmt.Errorf("failed to encode cursor for %s: %w", folder, err)
	}
	result.Cursor = newCursor

	slog.Debug("Fetched messages", "module", "IMAPBACKEND", "folder", folder,
		"new", len(result.Messages), "deleted", len(result.Deleted), "has_more", result.HasMore)
	return result, nil
}

// FetchBody streams the full RFC822 message for ref to w. Uses BODY.PEEK[]
// (empty section path = entire message) so \Seen is not set.
func (b *Backend) FetchBody(_ context.Context, ref backend.RemoteRef, w io.Writer) error {
	uid, err := parseUID(ref)
	if err != nil {
		return err
	}

	if _, err := b.client.SelectMailbox(ref.Folder); err != nil {
		return fmt.Errorf("failed to select %s: %w", ref.Folder, err)
	}

	if err := b.client.FetchBodySection(uid, nil, w); err != nil {
		return fmt.Errorf("failed to fetch body for UID %d in %s: %w", uid, ref.Folder, err)
	}
	return nil
}

// MARK: - Flags / Move / Append

// ApplyFlags adds and removes flags on ref. Flags are translated via
// imap.FlagState.ToIMAPFlags, which (matching existing sync behavior) never
// uploads the server-only $Completed keyword.
func (b *Backend) ApplyFlags(_ context.Context, ref backend.RemoteRef, add, remove backend.Flags) error {
	uid, err := parseUID(ref)
	if err != nil {
		return err
	}

	// Retry-safe: adding/removing the same flags twice is idempotent.
	return b.withReconnect(func() error {
		if _, err := b.client.SelectMailbox(ref.Folder); err != nil {
			return fmt.Errorf("failed to select %s: %w", ref.Folder, err)
		}
		if addFlags := toFlagState(add).ToIMAPFlags(); len(addFlags) > 0 {
			if err := b.client.AddFlags(uid, addFlags); err != nil {
				return fmt.Errorf("failed to add flags on UID %d in %s: %w", uid, ref.Folder, err)
			}
		}
		if removeFlags := toFlagState(remove).ToIMAPFlags(); len(removeFlags) > 0 {
			if err := b.client.RemoveFlags(uid, removeFlags); err != nil {
				return fmt.Errorf("failed to remove flags on UID %d in %s: %w", uid, ref.Folder, err)
			}
		}
		return nil
	})
}

// Move relocates ref into destFolder via the IMAP copy + \Deleted + expunge
// dance. IMAP (without UIDPLUS support in go-imap v1) does not report the new
// UID, so the returned ref has an empty ID; the next sync of destFolder
// re-establishes the mapping via Message-ID.
func (b *Backend) Move(_ context.Context, ref backend.RemoteRef, destFolder string) (backend.RemoteRef, error) {
	uid, err := parseUID(ref)
	if err != nil {
		return backend.RemoteRef{}, err
	}

	if _, err := b.client.SelectMailbox(ref.Folder); err != nil {
		return backend.RemoteRef{}, fmt.Errorf("failed to select %s: %w", ref.Folder, err)
	}
	if err := b.client.CopyToMailbox(uid, destFolder); err != nil {
		return backend.RemoteRef{}, fmt.Errorf("failed to copy UID %d to %s: %w", uid, destFolder, err)
	}
	// Delete marks \Deleted and expunges (removes the source copy).
	if err := b.client.Delete(uid); err != nil {
		return backend.RemoteRef{}, fmt.Errorf("failed to delete source UID %d in %s: %w", uid, ref.Folder, err)
	}

	return backend.RemoteRef{Folder: destFolder, ID: ""}, nil
}

// Append stores msg into folder. The go-imap v1 client does not expose
// APPENDUID, so the returned ref has an empty ID (like Move, the next sync
// resolves it via Message-ID). The append date is time.Now(), matching how
// the existing draft/sent-copy code stamps appended messages.
func (b *Backend) Append(_ context.Context, folder string, flags backend.Flags, msg []byte) (backend.RemoteRef, error) {
	if _, err := b.client.Append(folder, toFlagState(flags).ToIMAPFlags(), time.Now(), msg); err != nil {
		return backend.RemoteRef{}, fmt.Errorf("failed to append to %s: %w", folder, err)
	}
	return backend.RemoteRef{Folder: folder, ID: ""}, nil
}

// MARK: - Send / Watch

// Send is not supported: the IMAP backend only syncs mailboxes. Outbound mail
// goes through the SMTP path; this stub exists to satisfy backend.Backend.
func (b *Backend) Send(_ context.Context, _ []byte) error {
	return fmt.Errorf("not supported: IMAP backend does not send; use SMTP backend")
}

// Watch blocks in IMAP IDLE on folder and invokes onChange for every mailbox
// update until ctx is done.
func (b *Backend) Watch(ctx context.Context, folder string, onChange func()) error {
	if _, err := b.client.SelectMailbox(folder); err != nil {
		return fmt.Errorf("failed to select %s for watch: %w", folder, err)
	}

	for {
		ctxDone, err := b.idleOnce(ctx, onChange)
		if err != nil {
			return fmt.Errorf("watch on %s failed: %w", folder, err)
		}
		if ctxDone {
			return ctx.Err()
		}
	}
}

// idleOnce runs one IDLE session. Returns ctxDone=true when ctx was cancelled;
// otherwise the session ended normally (renewal) or with an error.
func (b *Backend) idleOnce(ctx context.Context, onChange func()) (ctxDone bool, err error) {
	stop := make(chan struct{})
	updates := make(chan bool, 8)
	idleErr := make(chan error, 1)

	go func() {
		idleErr <- b.client.Idle(stop, updates)
	}()

	for {
		select {
		case <-ctx.Done():
			close(stop)
			// Drain updates so Idle's forwarding send cannot block, then wait
			// for the IDLE goroutine to exit.
			for {
				select {
				case <-updates:
				case <-idleErr:
					return true, nil
				}
			}
		case <-updates:
			onChange()
		case err := <-idleErr:
			if err != nil {
				return false, fmt.Errorf("idle: %w", err)
			}
			return false, nil
		}
	}
}

// MARK: - Capabilities / Close

// Capabilities reports IMAP backend behavior. Gmail and Microsoft auto-save
// sent mail server-side; generic IMAP providers do not. IMAP has no native
// atomic move here (copy + expunge), but IDLE provides push notifications.
func (b *Backend) Capabilities() backend.Capabilities {
	serverSideSent := false
	if b.account.OAuth != nil {
		switch b.account.OAuth.Provider {
		case "google", "microsoft":
			serverSideSent = true
		}
	}
	return backend.Capabilities{
		ServerSideSent: serverSideSent,
		NativeMove:     false,
		PushWatch:      true,
	}
}

// Close closes the underlying IMAP connection.
func (b *Backend) Close() error {
	return b.client.Close()
}

// MARK: - Connection resilience

// withReconnect runs op and, if it fails with a connection-level error on a
// self-owned connection, reconnects the IMAP client once and retries op.
// Mirrors the legacy syncer's per-mailbox reconnect-and-retry (M365 in
// particular drops long-lived connections mid-sync). op MUST be idempotent and
// must re-establish its own mailbox selection, since a reconnect resets the
// server-side selected state — only wrap read/flag operations, never Append or
// Move (which would duplicate on retry) or streaming FetchBody.
func (b *Backend) withReconnect(op func() error) error {
	err := op()
	if err == nil || !b.ownsClient || !isConnectionError(err) {
		return err
	}

	slog.Warn("Connection lost, reconnecting", "module", "IMAPBACKEND", "err", err)
	if rerr := b.client.Reconnect(); rerr != nil {
		return fmt.Errorf("%w (reconnect failed: %v)", err, rerr)
	}
	slog.Debug("Reconnected, retrying operation", "module", "IMAPBACKEND")
	return op()
}

// isConnectionError reports whether err is a dropped/broken IMAP connection.
// Local copy of the unexported imap.isConnectionError (no-edits constraint on
// the imap package); keep the two in sync.
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection closed") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "use of closed network connection")
}

// MARK: - Cursor encoding

// decodeCursor decodes a JSON cursor into an imap.MailboxState. An empty or
// nil cursor yields a fresh state (full resync).
func decodeCursor(cursor backend.Cursor) (*imap.MailboxState, error) {
	state := &imap.MailboxState{}
	if len(cursor) == 0 {
		return state, nil
	}
	if err := json.Unmarshal(cursor, state); err != nil {
		return nil, fmt.Errorf("unmarshal mailbox state: %w", err)
	}
	return state, nil
}

// encodeCursor serializes the mailbox state back into an opaque cursor.
func encodeCursor(state *imap.MailboxState) (backend.Cursor, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("marshal mailbox state: %w", err)
	}
	return backend.Cursor(data), nil
}

// MARK: - Helpers

// toBackendFlags converts an imap.FlagState to the neutral backend.Flags.
func toBackendFlags(f imap.FlagState) backend.Flags {
	return backend.Flags{
		Seen:      f.Seen,
		Flagged:   f.Flagged,
		Answered:  f.Answered,
		Deleted:   f.Deleted,
		Completed: f.Completed,
	}
}

// toFlagState converts neutral backend.Flags to an imap.FlagState.
func toFlagState(f backend.Flags) imap.FlagState {
	return imap.FlagState{
		Seen:      f.Seen,
		Flagged:   f.Flagged,
		Answered:  f.Answered,
		Deleted:   f.Deleted,
		Completed: f.Completed,
	}
}

// formatUID renders a UID as the decimal RemoteRef.ID.
func formatUID(uid uint32) string {
	return strconv.FormatUint(uint64(uid), 10)
}

// parseUID parses the decimal UID out of a RemoteRef.
func parseUID(ref backend.RemoteRef) (uint32, error) {
	uid, err := strconv.ParseUint(ref.ID, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid IMAP UID %q in ref for %s: %w", ref.ID, ref.Folder, err)
	}
	return uint32(uid), nil
}

// readRawBody extracts the full RFC822 literal from a fetched message's body
// sections (the reader can only be consumed once).
func readRawBody(body map[*goimap.BodySectionName]goimap.Literal) []byte {
	for _, literal := range body {
		if literal == nil {
			continue
		}
		data, err := io.ReadAll(literal)
		if err == nil && len(data) > 0 {
			return data
		}
	}
	return nil
}

// extractMessageID parses the Message-ID header from a raw RFC822 message,
// with angle brackets stripped.
func extractMessageID(raw []byte) string {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	messageID := msg.Header.Get("Message-ID")
	if messageID == "" {
		messageID = msg.Header.Get("Message-Id")
	}
	return strings.Trim(messageID, "<>")
}
