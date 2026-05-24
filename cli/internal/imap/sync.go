package imap

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	goimap "github.com/emersion/go-imap"

	"github.com/durian-dev/durian/cli/internal/config"
	durianmail "github.com/durian-dev/durian/cli/internal/mail"
	"github.com/durian-dev/durian/cli/internal/store"
)

// SyncMode defines the sync direction
type SyncMode int

const (
	// SyncBidirectional syncs both directions (default)
	SyncBidirectional SyncMode = iota
	// SyncDownloadOnly only downloads from server
	SyncDownloadOnly
	// SyncUploadOnly only uploads local changes to server
	SyncUploadOnly
)

// FolderTagMapping defines which tags to add/remove when syncing a folder
// This is used for deduplication: when a mail already exists locally,
// we update tags instead of downloading again
type FolderTagMapping struct {
	AddTags    []string // Tags to add (e.g., "trash" for Trash folder)
	RemoveTags []string // Tags to remove (e.g., "inbox" when mail moved to Trash)
}

// specialUseFolderTags maps IMAP SPECIAL-USE attributes to tag operations
// These are standardized by RFC 6154
var specialUseFolderTags = map[string]FolderTagMapping{
	"\\Inbox":   {AddTags: []string{"inbox"}, RemoveTags: []string{}},
	"\\Sent":    {AddTags: []string{"sent"}, RemoveTags: []string{}},
	"\\Drafts":  {AddTags: []string{"draft"}, RemoveTags: []string{}},
	"\\Trash":   {AddTags: []string{"trash"}, RemoveTags: []string{"inbox"}},
	"\\Junk":    {AddTags: []string{"spam"}, RemoveTags: []string{"inbox"}},
	"\\Archive": {AddTags: []string{"archive"}, RemoveTags: []string{"inbox"}},
}

// SyncOptions configures the sync behavior
type SyncOptions struct {
	DryRun           bool
	Quiet            bool
	NoFlags          bool                // Skip flag synchronization
	Mode             SyncMode            // Sync direction
	Mailboxes        []string            // Specific mailboxes to sync (empty = all)
	Store            *store.DB           // SQLite store (required)
	FilterRules      []config.RuleConfig          // User-defined filter rules applied at insert time
	Groups           map[string]config.GroupEntry // Contact groups for group: expansion in rules
	BackfillHeaders  bool                         // Fetch and store headers for existing messages
}

// SyncResult contains the results of a sync operation
type SyncResult struct {
	Account           string
	Mailboxes         []MailboxResult
	Duration          time.Duration
	TotalNew          int
	TotalSkipped      int
	TotalDeleted      int // Messages deleted locally (removed from server)
	TotalDeduplicated int // Messages that already existed locally (tags updated)
	FlagsUploaded     int // Flags uploaded to server
	FlagsDownload     int // Flags downloaded from server
	TotalMoved        int // Messages moved between IMAP folders
	NewMessageIDs     []string // Message-IDs of newly downloaded messages
	Error             error
}

// MailboxResult contains the results for a single mailbox
type MailboxResult struct {
	Name             string
	TotalMsgs        uint32
	NewMsgs          int
	SkippedMsgs      int
	DeletedMsgs      int // Messages deleted locally (removed from server)
	DeduplicatedMsgs int // Messages that already existed locally (tags updated)
	FlagsUploaded    int
	FlagsDownload    int
	MovedMsgs        int // Messages moved between IMAP folders
	NewMessageIDs    []string // Message-IDs of newly downloaded messages
	Error            error
}

// Syncer handles IMAP synchronization for an account
type Syncer struct {
	client          *Client
	state           *State
	stateMgr        *StateManager
	stateLock       *os.File             // File lock held during sync
	account         *config.AccountConfig
	options         *SyncOptions
	output          io.Writer
	trashMailbox    string                // Cached trash mailbox name for delete operations
	archiveMailbox  string                // Cached archive mailbox name for archive operations
	serverMailboxes []*goimap.MailboxInfo // Cached mailbox list for exclusion tags
	ownsClient      bool                  // true = syncer manages connection lifecycle
	store           *store.DB            // SQLite store for messages and tags
	parser          *durianmail.Parser   // Email parser for store writes
}

// NewSyncer creates a new syncer for an account
func NewSyncer(account *config.AccountConfig, options *SyncOptions) *Syncer {
	if options == nil {
		options = &SyncOptions{}
	}

	output := io.Writer(os.Stderr)
	if options.Quiet {
		output = io.Discard
	}

	return &Syncer{
		client:     NewClient(account),
		stateMgr:   NewStateManager(),
		account:    account,
		options:    options,
		output:     output,
		ownsClient: true,
		store:      options.Store,
		parser:     durianmail.NewParser(),
	}
}

// NewSyncerWithClient creates a syncer that reuses an existing IMAP connection.
// The caller owns the connection lifecycle (connect, auth, close).
func NewSyncerWithClient(account *config.AccountConfig, client *Client, options *SyncOptions) *Syncer {
	if options == nil {
		options = &SyncOptions{}
	}

	output := io.Writer(os.Stderr)
	if options.Quiet {
		output = io.Discard
	}

	return &Syncer{
		client:     client,
		stateMgr:   NewStateManager(),
		account:    account,
		options:    options,
		output:     output,
		ownsClient: false,
		store:      options.Store,
		parser:     durianmail.NewParser(),
	}
}

// Sync performs a full sync of the account
func (s *Syncer) Sync() (*SyncResult, error) {
	start := time.Now()
	result := &SyncResult{
		Account: s.account.Email,
	}

	// Load state (acquires file lock to prevent concurrent syncs)
	var err error
	s.state, s.stateLock, err = s.stateMgr.Load(s.account.Email)
	if err != nil {
		return nil, fmt.Errorf("failed to load state: %w", err)
	}
	defer releaseLock(s.stateLock)

	// Connect and authenticate (skip if caller owns the connection)
	if s.ownsClient {
		if err := s.client.Connect(); err != nil {
			return nil, err
		}
		defer s.client.Close()

		if err := s.client.Authenticate(); err != nil {
			return nil, err
		}
	}

	// Enable Gmail label fetching if this is a Gmail account
	if s.isGmail() {
		s.client.fetchGmailLabels = true
	}

	// Get mailboxes to sync
	mailboxes, err := s.getMailboxesToSync()
	if err != nil {
		return nil, err
	}

	// Cache server mailbox list for exclusion tag logic
	s.serverMailboxes, err = s.client.ListMailboxes()
	if err != nil {
		slog.Debug("Failed to cache server mailbox list", "module", "SYNC", "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
	}

	// Sync each mailbox with automatic reconnection on failure
	for _, mbox := range mailboxes {
		mboxResult := s.syncMailbox(mbox)

		// Check if error is connection-related and try to reconnect
		if mboxResult.Error != nil && isConnectionError(mboxResult.Error) {
			if !s.ownsClient {
				// Caller owns the connection — don't reconnect (would open a
				// new socket that aggressive servers like M365 reject). Abort
				// early and let the caller's IDLE loop catch up next cycle.
				slog.Debug("Connection lost, aborting (caller-owned connection)", "module", "SYNC", "mailbox", mbox) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
				result.Mailboxes = append(result.Mailboxes, mboxResult)
				result.Error = mboxResult.Error
				break
			}

			slog.Debug("Connection lost, attempting reconnect", "module", "SYNC", "mailbox", mbox) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
			fmt.Fprintf(s.output, "  ⚠ Connection lost, reconnecting...\n")

			if err := s.client.Reconnect(); err != nil {
				slog.Debug("Reconnect failed", "module", "SYNC", "err", err)
				result.Mailboxes = append(result.Mailboxes, mboxResult)
				result.Error = fmt.Errorf("reconnect failed: %w", err)
				break // Can't continue without connection
			}

			// Retry the mailbox after reconnection
			fmt.Fprintf(s.output, "  ✓ Reconnected, retrying %s...\n", mbox)
			mboxResult = s.syncMailbox(mbox)
		}

		result.Mailboxes = append(result.Mailboxes, mboxResult)
		result.TotalNew += mboxResult.NewMsgs
		result.TotalSkipped += mboxResult.SkippedMsgs
		result.TotalDeleted += mboxResult.DeletedMsgs
		result.TotalDeduplicated += mboxResult.DeduplicatedMsgs
		result.FlagsUploaded += mboxResult.FlagsUploaded
		result.FlagsDownload += mboxResult.FlagsDownload
		result.TotalMoved += mboxResult.MovedMsgs
		result.NewMessageIDs = append(result.NewMessageIDs, mboxResult.NewMessageIDs...)

		if mboxResult.Error != nil && result.Error == nil {
			result.Error = mboxResult.Error
		}

		// Save state after each mailbox so progress survives interrupts (Ctrl+C)
		if !s.options.DryRun {
			if err := s.stateMgr.Save(s.account.Email, s.state); err != nil {
				fmt.Fprintf(s.output, "  Warning: failed to save state: %v\n", err)
			}
		}
	}

	// Backfill headers for existing messages (one-time operation)
	if s.options.BackfillHeaders && !s.options.DryRun {
		s.backfillHeaders(mailboxes)
	}

	result.Duration = time.Since(start)
	return result, nil
}

// SyncAccounts syncs multiple accounts
func SyncAccounts(accounts []*config.AccountConfig, options *SyncOptions) ([]*SyncResult, error) {
	var results []*SyncResult

	for _, account := range accounts {
		fmt.Fprintf(os.Stderr, "Syncing %s...\n", account.Email)

		syncer := NewSyncer(account, options)
		result, err := syncer.Sync()

		if err != nil {
			fmt.Fprintf(os.Stderr, "  ✗ %v\n", err)
			result = &SyncResult{
				Account: account.Email,
				Error:   err,
			}
		} else {
			// Compact summary
			parts := []string{}
			if result.TotalNew > 0 {
				parts = append(parts, fmt.Sprintf("%d new", result.TotalNew))
			}
			if result.TotalDeleted > 0 {
				parts = append(parts, fmt.Sprintf("%d deleted", result.TotalDeleted))
			}
			if result.TotalDeduplicated > 0 {
				parts = append(parts, fmt.Sprintf("%d dedup", result.TotalDeduplicated))
			}
			if result.FlagsUploaded > 0 || result.FlagsDownload > 0 {
				parts = append(parts, fmt.Sprintf("%d↑ %d↓ flags", result.FlagsUploaded, result.FlagsDownload))
			}
			if result.TotalMoved > 0 {
				parts = append(parts, fmt.Sprintf("%d moved", result.TotalMoved))
			}
			summary := "up to date"
			if len(parts) > 0 {
				summary = strings.Join(parts, ", ")
			}
			fmt.Fprintf(os.Stderr, "✓ %s (%.1fs)\n", summary, result.Duration.Seconds())
		}

		results = append(results, result)
	}

	return results, nil
}
