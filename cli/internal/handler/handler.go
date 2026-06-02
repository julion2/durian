package handler

import (
	"context"
	"io"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/contacts"
	"github.com/julion2/durian/cli/internal/mail"
	"github.com/julion2/durian/cli/internal/protocol"
	"github.com/julion2/durian/cli/internal/store"
	"github.com/julion2/durian/cli/internal/tagsync"
)

// AttachmentFetcher fetches attachment bytes directly from the IMAP server.
// Implemented by WatcherManager to break IDLE and stream BODY[section].
type AttachmentFetcher interface {
	FetchAttachment(ctx context.Context, account, mailbox string,
		uid uint32, messageID, filename, contentType string, partIndex int,
		w io.Writer) error
}

// SyncTrigger triggers an upload-only IMAP sync for an account.
// Implemented by WatcherManager to break IDLE and push local tag changes.
type SyncTrigger interface {
	TriggerSync(account string)
}

// Handler processes commands and returns responses
type Handler struct {
	store       *store.DB // SQLite store (primary read backend)
	parser      *mail.Parser
	contacts    *contacts.DB
	cfg         *config.Config                // application config (for outbox worker)
	groups      map[string]config.GroupEntry   // contact groups for query expansion
	fetcher        AttachmentFetcher // optional IMAP attachment fetcher
	syncTrigger    SyncTrigger       // optional sync trigger for tag changes
	tagSync        *tagsync.Client   // optional remote tag sync client
	tagSyncEnabled bool              // true when tag sync is configured (enables journal)
}

// New creates a Handler that reads from the SQLite store.
func New(db *store.DB, contactsDB *contacts.DB) *Handler {
	return &Handler{
		store:    db,
		parser:   mail.NewParser(),
		contacts: contactsDB,
	}
}

// SetFetcher sets the IMAP attachment fetcher (typically the WatcherManager).
func (h *Handler) SetFetcher(f AttachmentFetcher) {
	h.fetcher = f
}

// SetSyncTrigger sets the sync trigger for pushing tag changes to IMAP.
func (h *Handler) SetSyncTrigger(s SyncTrigger) {
	h.syncTrigger = s
}

// SetTagSync sets the optional remote tag sync client.
func (h *Handler) SetTagSync(c *tagsync.Client) {
	h.tagSync = c
	h.tagSyncEnabled = true
}

// EnableTagJournal enables the local tag journal without a remote client.
// Used by CLI commands that modify tags when tag sync is configured.
func (h *Handler) EnableTagJournal() {
	h.tagSyncEnabled = true
}

// SetGroups sets the contact groups for query expansion.
func (h *Handler) SetGroups(groups map[string]config.GroupEntry) {
	h.groups = groups
}

// Groups returns the loaded contact groups.
func (h *Handler) Groups() map[string]config.GroupEntry {
	return h.groups
}

// expandGroups replaces group:NAME references in a query with from: OR-chains.
func (h *Handler) expandGroups(query string) (string, error) {
	return config.ExpandGroupsInQuery(query, h.groups)
}

// SetConfig sets the application config (needed by outbox worker for account lookup).
func (h *Handler) SetConfig(cfg *config.Config) {
	h.cfg = cfg
}

// Config returns the application config.
func (h *Handler) Config() *config.Config {
	return h.cfg
}

// Store returns the database handle.
func (h *Handler) Store() *store.DB {
	return h.store
}

// Handle dispatches a command to the appropriate handler method
func (h *Handler) Handle(cmd protocol.Command) protocol.Response {
	switch cmd.Cmd {
	case "search":
		return h.Search(cmd.Query, cmd.Limit, 0)
	case "show":
		if cmd.Thread != "" {
			return h.ShowThread(cmd.Thread)
		}
		return h.Show(cmd.File)
	case "tag":
		return h.Tag(cmd.Query, cmd.Tags)
	default:
		return protocol.FailWithMessage(protocol.ErrUnknownCmd, "unknown command: "+cmd.Cmd)
	}
}
