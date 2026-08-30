// Package backend defines the provider-agnostic mail sync abstraction.
//
// IMAP, Microsoft Graph, Gmail REST, and JMAP backends implement Backend, so the
// sync engine, store, tags and search stay provider-neutral. Message identity is
// always the RFC822 Message-ID plus the account; RemoteRef is only the provider's
// own transient handle for follow-up operations and is never used as a primary key.
package backend

import (
	"context"
	"errors"
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
	// MessageID is an optional stable identity precondition for operations on
	// providers whose handles can be reused (notably IMAP after UIDVALIDITY).
	MessageID string
}

// ErrRefGone reports that a RemoteRef no longer resolves to a message on the
// server: it was moved, deleted or expunged by another client since the ref was
// stored. Backends wrap it around the provider's own error (Graph 404
// ErrorItemNotFound, IMAP UID no longer present) so callers can distinguish
// "this handle is permanently dead, reconcile locally" from a transient
// failure worth retrying. Retrying an ErrRefGone operation can never succeed —
// providers that renumber on move (Graph) never resurrect an old id.
var ErrRefGone = errors.New("remote ref no longer exists on server")

// ErrPartialFlags reports that FetchFlags resolved a usable subset of the
// requested refs. The returned map must contain every successfully resolved
// ref; the engine reconciles that subset, reports the error, and leaves omitted
// refs pending without pinning message-download progress.
var ErrPartialFlags = errors.New("some remote flags remain unresolved")

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
	// FullSnapshot reports that this page is part of a complete replacement
	// snapshot. Present contains every remote ref that existed in this page of
	// that snapshot. Once the final page is processed, the engine removes local
	// refs that were not present in any page. A sequence may switch once from
	// delta to FullSnapshot when an intermediate provider cursor expires and
	// enumeration restarts from the beginning; it must never switch back to
	// delta. Delta-capable backends use this to recover safely when a server can
	// no longer calculate changes from a cursor.
	FullSnapshot bool
	Present      []RemoteRef
	// Unavailable is the subset of Present whose body the provider explicitly
	// denied access to. Replacement reconciliation preserves an existing local
	// copy but does not require a locally absent ref to be hydrated.
	Unavailable []RemoteRef
}

// Capabilities describes backend-specific behavior the sync engine adapts to,
// so provider quirks stay out of the engine's control flow.
type Capabilities struct {
	// PushWatch reports real push/delta notifications rather than poll-only.
	PushWatch bool
	// FlagChangesInDelta reports that FetchMessages already surfaces server-side
	// flag/read-state changes (a message reappears in the delta with its new
	// flags). The engine then reconciles flags from the delta stream instead of
	// polling every message's flags each sync, which is O(changes) not O(mailbox).
	FlagChangesInDelta bool
	// LabelsAreTags reports that Message.Labels is the authoritative tag set for
	// each message (Gmail/JMAP), so the engine mirrors those labels to Durian tags —
	// adding new ones and removing labels the server dropped — instead of the
	// folder-role tag mapping. Durian-local tags (rules, flags) are left intact.
	LabelsAreTags bool
	// AnsweredUnsupported reports that the backend cannot persist the \Answered
	// flag — Gmail has no answered label, and Graph's message resource has no
	// answered property, so its ApplyFlags translates only isRead and
	// flagStatus. The engine then excludes Answered from the three-way flag
	// merge for this backend: a local "replied" tag would otherwise be uploaded
	// (silently dropped by the provider), recorded in the baseline, then removed
	// on the next sync when the server reports the message as un-answered — a
	// ping-pong that flips the tag every sync. Default false keeps the full
	// merge for IMAP and JMAP, which do round-trip \Answered.
	AnsweredUnsupported bool
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

	// FetchFlags returns the current server flag state for the given messages in a
	// folder, keyed by RemoteRef.ID. Messages the backend cannot resolve are simply
	// absent from the map. The engine drives the three-way merge; the backend only
	// reports server state (and applies changes via ApplyFlags). A backend may
	// return a non-nil map with an error wrapping ErrPartialFlags when only a
	// subset resolved; other errors make the map unusable.
	FetchFlags(ctx context.Context, folder string, refs []RemoteRef) (map[string]Flags, error)

	// Move relocates ref into destFolder, returning its new handle. Returns an
	// error wrapping ErrRefGone when ref no longer exists on the server.
	Move(ctx context.Context, ref RemoteRef, destFolder string) (RemoteRef, error)

	// Append stores msg into folder with the given flags (drafts, sent copies).
	Append(ctx context.Context, folder string, flags Flags, msg []byte) (RemoteRef, error)

	// Send submits msg for delivery.
	Send(ctx context.Context, msg []byte) error

	// Watch blocks and invokes onChange whenever folder changes, until ctx is done.
	// An empty folder requests an account-wide watch; implementations that only
	// support per-folder push should watch INBOX in that case.
	Watch(ctx context.Context, folder string, onChange func()) error

	// Capabilities describes optional behaviors the sync engine should adapt to.
	Capabilities() Capabilities

	// Close releases the backend's resources.
	Close() error
}

// LabelWriter is an optional capability of a LabelsAreTags backend: it uploads
// local tag changes as label modifications. The engine type-asserts for it and
// runs the label-upload pass only when both the assertion and the LabelsAreTags
// capability hold, so folder-based backends need not implement it.
type LabelWriter interface {
	// LabelTags returns the vocabulary of tags that correspond to real server
	// labels (system + user), so the engine can tell an uploadable label change
	// from a Durian-local tag (rule tags, "ephemeral", ...) that must not leak
	// to the provider.
	LabelTags(ctx context.Context) ([]string, error)

	// ApplyLabels resolves each add/remove tag to its server label and applies
	// the change to ref in one call. An unknown added tag returns an error rather
	// than silently losing a local change. The engine resets its baseline
	// to the deterministic (local tags ∩ LabelTags) set itself, so ApplyLabels
	// returns only an error.
	ApplyLabels(ctx context.Context, ref RemoteRef, add, remove []string) error
}

// SnapshotHydrator is implemented by metadata-first backends. A replacement
// snapshot can list every remote ref cheaply; the engine then asks for full
// messages only for refs that are not already in the local read model before it
// advances the replacement cursor.
type SnapshotBatch struct {
	// Messages contains every requested ref that still exists.
	Messages []Message
	// Missing contains requested refs that the provider explicitly reported as
	// gone after the authoritative snapshot was listed. The engine removes them
	// from that snapshot rather than retrying an impossible hydration forever.
	Missing []RemoteRef
}

type SnapshotHydrator interface {
	// FetchSnapshotMetadata returns current flags and labels for locally existing
	// refs without downloading their RFC 5322 bodies. Every requested ref must
	// appear in either Messages or Missing.
	FetchSnapshotMetadata(ctx context.Context, refs []RemoteRef) (SnapshotBatch, error)
	// FetchSnapshotMessages returns complete messages for locally absent refs.
	FetchSnapshotMessages(ctx context.Context, refs []RemoteRef) (SnapshotBatch, error)
}

// IdentityCursorUpdater is implemented by backends whose replacement cursor
// stores Message-IDs alongside transient remote refs. During recovery the
// engine may adopt a pre-reset synthetic ID after content matching; the backend
// must write those canonical IDs into the same page cursor before it is
// checkpointed or used to fetch the next page.
type IdentityCursorUpdater interface {
	AdoptMessageIdentities(cursor Cursor, identities map[string]string) (Cursor, error)
}
