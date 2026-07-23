// Package backend defines the provider-agnostic mail sync abstraction.
//
// Both the IMAP syncer (Gmail, generic providers) and the Microsoft Graph
// backend implement Backend, so the sync engine, store, tags and search stay
// provider-neutral. Message identity is always the RFC822 Message-ID plus the
// account; RemoteRef is only the provider's own transient handle for follow-up
// operations and is never used as a primary key.
package backend

import (
	"context"
	"io"
	"time"
)

// Role is a special-use folder role (RFC 6154), mapped to Durian tags.
type Role string

const (
	RoleNone    Role = ""
	RoleInbox   Role = "inbox"
	RoleSent    Role = "sent"
	RoleDrafts  Role = "drafts"
	RoleTrash   Role = "trash"
	RoleJunk    Role = "junk"
	RoleArchive Role = "archive"
	RoleAll     Role = "all"
)

// Folder is a syncable mail container with its resolved special-use role.
type Folder struct {
	// Name is the provider's folder identifier: an IMAP mailbox path or a
	// Graph folder id. It is passed back verbatim to FetchMessages/Append.
	Name string
	// Display is the human-readable folder name.
	Display string
	// Role is the special-use role, or RoleNone for user folders.
	Role Role
	// Selectable reports whether messages can live directly in this folder
	// (an IMAP \Noselect container is not selectable).
	Selectable bool
}

// RemoteRef is a provider-specific, non-durable handle to one message inside a
// folder. IMAP: Folder is the mailbox, ID is the decimal UID. Graph: Folder is
// the folder id, ID is the message id. A ref MAY become invalid across syncs
// (e.g. an IMAP UIDVALIDITY reset), so it is never persisted as a key — the
// durable key is (Message.MessageID, account).
type RemoteRef struct {
	Folder string
	ID     string
}

// Cursor is an opaque, per-folder incremental-sync token, owned and interpreted
// solely by the Backend that issued it. The sync engine persists it verbatim
// and hands it back to the next FetchMessages call. IMAP encodes UIDVALIDITY
// plus the synced UID set here; Graph encodes a delta link. An empty Cursor
// requests a full (re)sync of the folder.
type Cursor []byte

// Flags is the provider-neutral message flag state. Backends translate their
// native flags/keywords (IMAP \Seen, Graph isRead, ...) to and from this.
type Flags struct {
	Seen      bool
	Flagged   bool
	Answered  bool
	Deleted   bool
	Completed bool
}

// Message is a fetched message in Durian's neutral model.
type Message struct {
	// MessageID is the RFC822 Message-ID header — the stable cross-provider key.
	MessageID string
	// Ref is the provider handle for follow-up body/flag/move operations.
	Ref RemoteRef
	// Raw is the full RFC822 message, or nil if only metadata was fetched.
	Raw []byte
	// Flags is the current flag state at the source.
	Flags Flags
	// Labels are extra provider labels/categories mapped to Durian tags
	// (Gmail X-GM-LABELS, Graph categories).
	Labels []string
	// InternalDate is the server receive time.
	InternalDate time.Time
}

// Deletion is a message the source no longer holds in a folder. MessageID is
// the durable RFC822 Message-ID when the backend can resolve it — IMAP from its
// UID<->Message-ID map, Graph from the delta payload — so the engine can act on
// a message synced in an earlier run. It may be empty if the backend cannot
// resolve the handle, in which case the engine falls back to same-run tracking.
type Deletion struct {
	Ref       RemoteRef
	MessageID string
}

// FetchResult is the outcome of one incremental FetchMessages call: the changes
// in a folder since the caller's cursor, plus a fresh cursor to persist.
type FetchResult struct {
	// Messages are the new or updated messages, with bodies unless the backend
	// fetches metadata-first; the engine writes them to the store keyed by
	// (MessageID, account).
	Messages []Message
	// Deleted are messages the source no longer holds in this folder; the engine
	// resolves each to (MessageID, account) and untags/removes it.
	Deleted []Deletion
	// Cursor is the token to persist and pass back next time. Never nil after a
	// successful call — an unchanged folder returns the prior cursor verbatim.
	Cursor Cursor
	// HasMore reports that limit was reached and more changes remain; the engine
	// should call FetchMessages again with Cursor to continue paginating.
	HasMore bool
}

// Capabilities describes backend-specific behavior the sync engine adapts to,
// so provider quirks stay out of the engine's control flow.
type Capabilities struct {
	// ServerSideSent reports the provider auto-saves sent mail (Gmail, M365),
	// so Durian must not append its own Sent copy.
	ServerSideSent bool
	// NativeMove reports a true atomic move (Graph) rather than the IMAP
	// copy + \Deleted + expunge dance.
	NativeMove bool
	// PushWatch reports real push/delta notifications rather than poll-only.
	PushWatch bool
}

// Backend is a provider-agnostic mail source. Implementations translate between
// a concrete protocol (IMAP, Microsoft Graph) and Durian's neutral model. The
// connection lifecycle is owned by the implementation; all other methods assume
// a usable backend and return an error if the connection is lost.
type Backend interface {
	// FetchFolders returns the syncable folders with their special-use roles.
	FetchFolders(ctx context.Context) ([]Folder, error)

	// FetchMessages returns the changes in folder since cursor. limit caps how
	// many message bodies are returned in one call; the FetchResult reports
	// whether more remain and carries the cursor to persist for next time.
	FetchMessages(ctx context.Context, folder string, cursor Cursor, limit int) (FetchResult, error)

	// FetchBody streams the full RFC822 body for ref (lazy body/attachment fetch).
	FetchBody(ctx context.Context, ref RemoteRef, w io.Writer) error

	// ApplyFlags adds and removes flags on ref.
	ApplyFlags(ctx context.Context, ref RemoteRef, add, remove Flags) error

	// Move relocates ref into destFolder, returning its new handle.
	Move(ctx context.Context, ref RemoteRef, destFolder string) (RemoteRef, error)

	// Append stores msg into folder with the given flags (drafts, sent copies).
	Append(ctx context.Context, folder string, flags Flags, msg []byte) (RemoteRef, error)

	// Send submits msg for delivery.
	Send(ctx context.Context, msg []byte) error

	// Watch blocks and invokes onChange whenever folder changes, until ctx is done.
	Watch(ctx context.Context, folder string, onChange func()) error

	// Capabilities describes optional behaviors the sync engine should adapt to.
	Capabilities() Capabilities

	// Close releases the backend's resources.
	Close() error
}
