package imap

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-sasl"

	"github.com/durian-dev/durian/cli/internal/config"
	"github.com/durian-dev/durian/cli/internal/keychain"
	"github.com/durian-dev/durian/cli/internal/oauth"
)

const (
	// DefaultTimeout for IMAP operations
	DefaultTimeout = 30 * time.Second
)

// SpecialUseRole defines IMAP SPECIAL-USE mailbox roles (RFC 6154)
type SpecialUseRole string

const (
	RoleSent    SpecialUseRole = "\\Sent"
	RoleDrafts  SpecialUseRole = "\\Drafts"
	RoleTrash   SpecialUseRole = "\\Trash"
	RoleJunk    SpecialUseRole = "\\Junk"
	RoleArchive SpecialUseRole = "\\Archive"
	RoleAll     SpecialUseRole = "\\All"
)

// defaultRoleFallbacks maps SPECIAL-USE roles to common folder names for fallback
var defaultRoleFallbacks = map[SpecialUseRole][]string{
	RoleSent: {
		"Sent", "Sent Items", "Sent Messages", "INBOX.Sent",
		"[Gmail]/Sent Mail", "[Gmail]/Gesendet", "Gesendete Elemente", "Envoyés",
	},
	RoleDrafts: {
		"Drafts", "Draft", "INBOX.Drafts",
		"[Gmail]/Drafts", "[Gmail]/Entwürfe", "Entwürfe", "Brouillons",
	},
	RoleTrash: {
		"Trash", "Deleted Items", "Deleted Messages", "INBOX.Trash",
		"[Gmail]/Trash", "[Gmail]/Papierkorb", "Gelöschte Elemente", "Papierkorb", "Corbeille",
	},
	RoleJunk: {
		"Junk", "Spam", "INBOX.Junk", "INBOX.Spam",
		"[Gmail]/Spam", "Junk-E-Mail",
	},
	RoleArchive: {
		"Archive", "Archives", "INBOX.Archive",
		"Archiv",
	},
	RoleAll: {
		"[Gmail]/All Mail", "[Gmail]/Alle Nachrichten", "[Gmail]/Todos",
		"[Gmail]/Tous les messages", "[Gmail]/Tutti i messaggi",
		"All Mail",
	},
}

// Client wraps an IMAP client connection
type Client struct {
	account          *config.AccountConfig
	conn             *client.Client
	timeout          time.Duration
	fetchGmailLabels bool // When true, include X-GM-LABELS in FETCH requests
}

// NewClient creates a new IMAP client for the given account
func NewClient(account *config.AccountConfig) *Client {
	return &Client{
		account: account,
		timeout: DefaultTimeout,
	}
}

// Connect establishes a TLS connection to the IMAP server
func (c *Client) Connect() error {
	addr := fmt.Sprintf("%s:%d", c.account.IMAP.Host, c.account.IMAP.Port)

	// Connect with timeout
	dialer := &net.Dialer{Timeout: c.timeout}
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to connect to %s: %w", addr, err)
	}

	// Enable TCP keepalive so the OS detects dead connections (stale IDLE, NAT
	// timeout, etc.) within ~2-3 minutes rather than hanging indefinitely.
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(60 * time.Second)
	}

	// Wrap with TLS (IMAPS - port 993)
	tlsConfig := &tls.Config{
		ServerName: c.account.IMAP.Host,
	}
	tlsConn := tls.Client(conn, tlsConfig)

	// Create IMAP client
	c.conn, err = client.New(tlsConn)
	if err != nil {
		conn.Close()
		return fmt.Errorf("failed to create IMAP client: %w", err)
	}

	return nil
}

// Authenticate authenticates with the IMAP server using OAuth2 or password
func (c *Client) Authenticate() error {
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}

	switch c.account.IMAP.Auth {
	case "oauth2":
		return c.authenticateOAuth2()
	case "password", "":
		return c.authenticatePassword()
	default:
		return fmt.Errorf("unsupported auth method: %s", c.account.IMAP.Auth)
	}
}

// authenticateOAuth2 authenticates using XOAUTH2
func (c *Client) authenticateOAuth2() error {
	if c.account.OAuth == nil {
		return fmt.Errorf("OAuth not configured for %s", c.account.Email)
	}
	// Get valid OAuth token (auto-refreshes if needed).
	// For shared mailboxes, the token belongs to the delegating user (AuthEmail).
	token, err := oauth.GetValidToken(
		c.account.GetAuthEmail(),
		c.account.OAuth.ClientID,
		c.account.OAuth.ClientSecret,
		c.account.OAuth.Tenant,
	)
	if err != nil {
		return fmt.Errorf("failed to get OAuth token: %w", err)
	}

	// Create XOAUTH2 SASL client
	saslClient := NewXOAuth2Client(c.account.Email, token.AccessToken)

	// Authenticate
	if err := c.conn.Authenticate(saslClient); err != nil {
		return fmt.Errorf("XOAUTH2 authentication failed: %w", err)
	}

	return nil
}

// authenticatePassword authenticates using PLAIN/LOGIN
func (c *Client) authenticatePassword() error {
	username := c.account.Email
	if c.account.Auth != nil && c.account.Auth.Username != "" {
		username = c.account.Auth.Username
	}

	// Get password from unified keychain service
	password, err := keychain.GetPassword("durian-password", c.account.Email)
	if err != nil {
		return fmt.Errorf("failed to get password: %w\nRun: durian auth login %s", err, c.account.Email)
	}

	// Try PLAIN auth first
	if ok, _ := c.conn.Support("AUTH=PLAIN"); ok {
		saslClient := sasl.NewPlainClient("", username, password)
		if err := c.conn.Authenticate(saslClient); err != nil {
			return fmt.Errorf("PLAIN authentication failed: %w", err)
		}
		return nil
	}

	// Fall back to LOGIN
	if err := c.conn.Login(username, password); err != nil {
		return fmt.Errorf("LOGIN authentication failed: %w", err)
	}

	return nil
}

// ListMailboxes returns all mailboxes on the server
func (c *Client) ListMailboxes() ([]*imap.MailboxInfo, error) {
	if c.conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	mailboxes := make(chan *imap.MailboxInfo, 100)
	done := make(chan error, 1)

	go func() {
		done <- c.conn.List("", "*", mailboxes)
	}()

	var result []*imap.MailboxInfo
	for mbox := range mailboxes {
		result = append(result, mbox)
	}

	if err := <-done; err != nil {
		return nil, fmt.Errorf("failed to list mailboxes: %w", err)
	}

	return result, nil
}

// SelectMailbox selects a mailbox (read-write)
func (c *Client) SelectMailbox(name string) (*imap.MailboxStatus, error) {
	if c.conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	status, err := c.conn.Select(name, false)
	if err != nil {
		return nil, fmt.Errorf("failed to select mailbox %s: %w", name, err)
	}

	return status, nil
}

// SearchAll returns all message UIDs in the current mailbox
func (c *Client) SearchAll() ([]uint32, error) {
	if c.conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	criteria := imap.NewSearchCriteria()
	criteria.WithoutFlags = []string{} // Search all messages

	uids, err := c.conn.UidSearch(criteria)
	if err != nil {
		return nil, fmt.Errorf("failed to search messages: %w", err)
	}

	return uids, nil
}

// SearchUIDRange returns UIDs >= minUID. Pass maxUID=0 for no upper bound (*).
func (c *Client) SearchUIDRange(minUID, maxUID uint32) ([]uint32, error) {
	if c.conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	uidRange := new(imap.SeqSet)
	if maxUID == 0 {
		// minUID:* — open-ended range
		uidRange.AddRange(minUID, 0)
	} else {
		uidRange.AddRange(minUID, maxUID)
	}

	criteria := imap.NewSearchCriteria()
	criteria.Uid = uidRange

	uids, err := c.conn.UidSearch(criteria)
	if err != nil {
		return nil, fmt.Errorf("failed to search UIDs %d:*: %w", minUID, err)
	}

	// IMAP UID range "minUID:*" always includes the highest UID even if it's
	// below minUID (RFC 3501 §6.4.8). Filter out false positives.
	filtered := uids[:0]
	for _, uid := range uids {
		if uid >= minUID {
			filtered = append(filtered, uid)
		}
	}
	return filtered, nil
}

// FetchMessages fetches messages by UID
func (c *Client) FetchMessages(uids []uint32) ([]*imap.Message, error) {
	if c.conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	if len(uids) == 0 {
		return nil, nil
	}

	seqSet := new(imap.SeqSet)
	for _, uid := range uids {
		seqSet.AddNum(uid)
	}

	// Fetch full message (headers + body)
	items := []imap.FetchItem{
		imap.FetchUid,
		imap.FetchFlags,
		imap.FetchInternalDate,
		imap.FetchRFC822,
	}
	if c.fetchGmailLabels {
		items = append(items, "X-GM-LABELS")
	}

	slog.Debug("Fetching messages", "module", "IMAP", "uids", len(uids), "items", items)

	messages := make(chan *imap.Message, len(uids))
	done := make(chan error, 1)

	go func() {
		done <- c.conn.UidFetch(seqSet, items, messages)
	}()

	var result []*imap.Message
	for msg := range messages {
		slog.Debug("Received message", "module", "IMAP", "uid", msg.Uid, "body_parts", len(msg.Body))
		result = append(result, msg)
	}

	if err := <-done; err != nil {
		slog.Debug("Fetch error", "module", "IMAP", "err", err)
		return nil, fmt.Errorf("failed to fetch messages: %w", err)
	}

	slog.Debug("Fetch completed", "module", "IMAP", "count", len(result))
	return result, nil
}

// Idle starts IDLE mode and returns when there's an update or the stop channel is closed
func (c *Client) Idle(stop <-chan struct{}, updates chan<- bool) error {
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}

	// Set up updates channel
	updatesChan := make(chan client.Update, 10)
	c.conn.Updates = updatesChan

	// Start IDLE with 4-minute renewal. The default (25min) is too long —
	// Microsoft 365 has ~20min TCP idle timeout, Yahoo ~5min, Gmail ~10-15min.
	// Restarting every 4min keeps the connection alive on all providers.
	done := make(chan error, 1)
	go func() {
		done <- c.conn.Idle(stop, &client.IdleOptions{
			LogoutTimeout: 4 * time.Minute,
		})
	}()

	// Wait for updates or completion
	for {
		select {
		case update := <-updatesChan:
			switch update.(type) {
			case *client.MailboxUpdate, *client.ExpungeUpdate, *client.MessageUpdate:
				updates <- true
			}
		case err := <-done:
			return err
		}
	}
}

// FetchFlags fetches only flags for the given UIDs (faster than full fetch).
// When fetchGmailLabels is enabled, X-GM-LABELS is included automatically.
func (c *Client) FetchFlags(uids []uint32) (map[uint32][]string, error) {
	if c.conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	if len(uids) == 0 {
		return make(map[uint32][]string), nil
	}

	seqSet := new(imap.SeqSet)
	for _, uid := range uids {
		seqSet.AddNum(uid)
	}

	// Fetch only UID and FLAGS (much faster than full message)
	items := []imap.FetchItem{
		imap.FetchUid,
		imap.FetchFlags,
	}

	messages := make(chan *imap.Message, len(uids))
	done := make(chan error, 1)

	go func() {
		done <- c.conn.UidFetch(seqSet, items, messages)
	}()

	result := make(map[uint32][]string)
	for msg := range messages {
		result[msg.Uid] = msg.Flags
	}

	if err := <-done; err != nil {
		return nil, fmt.Errorf("failed to fetch flags: %w", err)
	}

	return result, nil
}

// FetchGmailLabels fetches X-GM-LABELS for the given UIDs in batches.
// Returns map[UID] -> []label strings.
func (c *Client) FetchGmailLabels(uids []uint32) (map[uint32][]string, error) {
	if c.conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	if len(uids) == 0 {
		return make(map[uint32][]string), nil
	}

	const batchSize = 500
	result := make(map[uint32][]string)
	items := []imap.FetchItem{imap.FetchUid, "X-GM-LABELS"}

	for i := 0; i < len(uids); i += batchSize {
		end := i + batchSize
		if end > len(uids) {
			end = len(uids)
		}
		batch := uids[i:end]

		seqSet := new(imap.SeqSet)
		for _, uid := range batch {
			seqSet.AddNum(uid)
		}

		messages := make(chan *imap.Message, len(batch))
		done := make(chan error, 1)

		go func() {
			done <- c.conn.UidFetch(seqSet, items, messages)
		}()

		for msg := range messages {
			raw, ok := msg.Items["X-GM-LABELS"]
			if !ok || raw == nil {
				continue
			}
			labels, ok := raw.([]interface{})
			if !ok {
				continue
			}
			var strs []string
			for _, l := range labels {
				s := fmt.Sprintf("%v", l)
				s = strings.Trim(s, "\"")
				strs = append(strs, s)
			}
			result[msg.Uid] = strs
		}

		if err := <-done; err != nil {
			return nil, fmt.Errorf("failed to fetch gmail labels: %w", err)
		}
	}

	return result, nil
}

// FetchEnvelopes fetches ENVELOPE data for given UIDs to extract Message-IDs
// Returns map[UID] -> MessageID
// This is used to build the UID <-> Message-ID mapping for flag sync
func (c *Client) FetchEnvelopes(uids []uint32) (map[uint32]string, error) {
	if c.conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	if len(uids) == 0 {
		return make(map[uint32]string), nil
	}

	result := make(map[uint32]string)

	// Process in batches to avoid timeout on large mailboxes
	const batchSize = 500
	for i := 0; i < len(uids); i += batchSize {
		end := i + batchSize
		if end > len(uids) {
			end = len(uids)
		}
		batch := uids[i:end]

		batchResult, err := c.fetchEnvelopesBatch(batch)
		if err != nil {
			return nil, err
		}

		for uid, msgID := range batchResult {
			result[uid] = msgID
		}
	}

	return result, nil
}

// fetchEnvelopesBatch fetches envelopes for a batch of UIDs
func (c *Client) fetchEnvelopesBatch(uids []uint32) (map[uint32]string, error) {
	seqSet := new(imap.SeqSet)
	for _, uid := range uids {
		seqSet.AddNum(uid)
	}

	// Fetch UID and ENVELOPE (contains Message-ID)
	items := []imap.FetchItem{
		imap.FetchUid,
		imap.FetchEnvelope,
	}

	messages := make(chan *imap.Message, len(uids))
	done := make(chan error, 1)

	go func() {
		done <- c.conn.UidFetch(seqSet, items, messages)
	}()

	result := make(map[uint32]string)
	for msg := range messages {
		if msg.Envelope != nil && msg.Envelope.MessageId != "" {
			// Clean up Message-ID (remove < > brackets if present)
			messageID := strings.Trim(msg.Envelope.MessageId, "<>")
			result[msg.Uid] = messageID
		}
	}

	if err := <-done; err != nil {
		return nil, fmt.Errorf("failed to fetch envelopes: %w", err)
	}

	return result, nil
}

// StoreFlags sets flags on a message (replaces existing flags)
func (c *Client) StoreFlags(uid uint32, flags []string) error {
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)

	// Use FLAGS.SILENT to set flags without response
	item := imap.FormatFlagsOp(imap.SetFlags, true)

	// Convert []string to []interface{} - go-imap requires this type
	ifaceFlags := make([]interface{}, len(flags))
	for i, f := range flags {
		ifaceFlags[i] = f
	}

	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)

	go func() {
		done <- c.conn.UidStore(seqSet, item, ifaceFlags, messages)
	}()

	// Drain any messages
	for range messages {
	}

	if err := <-done; err != nil {
		return fmt.Errorf("failed to store flags for UID %d: %w", uid, err)
	}

	return nil
}

// AddFlags adds flags to a message (keeps existing flags)
func (c *Client) AddFlags(uid uint32, flags []string) error {
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}

	if len(flags) == 0 {
		return nil
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)

	item := imap.FormatFlagsOp(imap.AddFlags, true) // .SILENT - no response

	// Convert []string to []interface{} - go-imap requires this type
	ifaceFlags := make([]interface{}, len(flags))
	for i, f := range flags {
		ifaceFlags[i] = f
	}

	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)

	go func() {
		done <- c.conn.UidStore(seqSet, item, ifaceFlags, messages)
	}()

	// Drain the channel (should be empty with SILENT)
	for range messages {
	}

	if err := <-done; err != nil {
		return fmt.Errorf("failed to add flags for UID %d: %w", uid, err)
	}

	return nil
}

// RemoveFlags removes flags from a message
func (c *Client) RemoveFlags(uid uint32, flags []string) error {
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}

	if len(flags) == 0 {
		return nil
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)

	item := imap.FormatFlagsOp(imap.RemoveFlags, true) // .SILENT - no response

	// Convert []string to []interface{} - go-imap requires this type
	ifaceFlags := make([]interface{}, len(flags))
	for i, f := range flags {
		ifaceFlags[i] = f
	}

	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)

	go func() {
		done <- c.conn.UidStore(seqSet, item, ifaceFlags, messages)
	}()

	// Drain the channel (should be empty with SILENT)
	for range messages {
	}

	if err := <-done; err != nil {
		return fmt.Errorf("failed to remove flags for UID %d: %w", uid, err)
	}

	return nil
}

// Append uploads a message to a mailbox.
// The returned UID is always 0 because the go-imap v1 library does not
// expose the APPENDUID response code (RFC 4315 UIDPLUS). The \Recent flag
// search that was here before is unreliable with concurrent clients.
func (c *Client) Append(mailbox string, flags []string, date time.Time, message []byte) (uint32, error) {
	if c.conn == nil {
		return 0, fmt.Errorf("not connected")
	}

	literal := bytes.NewReader(message)

	if err := c.conn.Append(mailbox, flags, date, literal); err != nil {
		return 0, fmt.Errorf("failed to append message to %s: %w", mailbox, err)
	}

	return 0, nil
}

// Delete marks a message as deleted and expunges it
func (c *Client) Delete(uid uint32) error {
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)

	// Add \Deleted flag
	item := imap.FormatFlagsOp(imap.AddFlags, true)
	ifaceFlags := []interface{}{imap.DeletedFlag}

	messages := make(chan *imap.Message, 1)
	done := make(chan error, 1)

	go func() {
		done <- c.conn.UidStore(seqSet, item, ifaceFlags, messages)
	}()

	// Drain messages
	for range messages {
	}

	if err := <-done; err != nil {
		return fmt.Errorf("failed to mark message %d as deleted: %w", uid, err)
	}

	// Expunge to permanently remove
	if err := c.conn.Expunge(nil); err != nil {
		return fmt.Errorf("failed to expunge message %d: %w", uid, err)
	}

	return nil
}

// FindDraftsMailbox finds the Drafts mailbox using SPECIAL-USE attributes
// Falls back to common names if SPECIAL-USE is not available
func (c *Client) FindDraftsMailbox() (string, error) {
	return c.FindMailboxByRole(RoleDrafts)
}

// FindSentMailbox finds the Sent mailbox using SPECIAL-USE attributes
// Falls back to common names if SPECIAL-USE is not available
func (c *Client) FindSentMailbox() (string, error) {
	return c.FindMailboxByRole(RoleSent)
}

// FindTrashMailbox finds the Trash mailbox using SPECIAL-USE attributes
// Falls back to common names if SPECIAL-USE is not available
func (c *Client) FindTrashMailbox() (string, error) {
	return c.FindMailboxByRole(RoleTrash)
}

// FindArchiveMailbox finds the Archive mailbox using SPECIAL-USE attributes
// Falls back to common names if SPECIAL-USE is not available
func (c *Client) FindArchiveMailbox() (string, error) {
	return c.FindMailboxByRole(RoleArchive)
}

// CopyToMailbox copies a message to another mailbox by UID
func (c *Client) CopyToMailbox(uid uint32, destMailbox string) error {
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)

	if err := c.conn.UidCopy(seqSet, destMailbox); err != nil {
		return fmt.Errorf("failed to copy UID %d to %s: %w", uid, destMailbox, err)
	}

	return nil
}

// Expunge permanently removes messages marked as \Deleted in the current mailbox
func (c *Client) Expunge() error {
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}

	if err := c.conn.Expunge(nil); err != nil {
		return fmt.Errorf("failed to expunge: %w", err)
	}

	return nil
}

// SearchByMessageID searches for a message by its Message-ID header
// Returns the UID if found, 0 if not found
func (c *Client) SearchByMessageID(messageID string) (uint32, error) {
	if c.conn == nil {
		return 0, fmt.Errorf("not connected")
	}

	// Clean up Message-ID (remove < > if present)
	messageID = strings.Trim(messageID, "<>")

	criteria := imap.NewSearchCriteria()
	criteria.Header.Add("Message-ID", "<"+messageID+">")

	uids, err := c.conn.UidSearch(criteria)
	if err != nil {
		return 0, fmt.Errorf("failed to search for Message-ID: %w", err)
	}

	if len(uids) == 0 {
		return 0, nil // Not found
	}

	return uids[0], nil
}

// FindMailboxByRole finds a mailbox by SPECIAL-USE role (RFC 6154)
// First checks for the SPECIAL-USE attribute, then falls back to common names
func (c *Client) FindMailboxByRole(role SpecialUseRole) (string, error) {
	if c.conn == nil {
		return "", fmt.Errorf("not connected")
	}

	mailboxes, err := c.ListMailboxes()
	if err != nil {
		return "", err
	}

	// First pass: look for SPECIAL-USE attribute
	roleStr := string(role)
	for _, mbox := range mailboxes {
		for _, attr := range mbox.Attributes {
			if strings.EqualFold(attr, roleStr) {
				return mbox.Name, nil
			}
		}
	}

	// Second pass: fallback to common names
	fallbacks, ok := defaultRoleFallbacks[role]
	if !ok {
		return "", fmt.Errorf("no mailbox found with role %s", role)
	}

	for _, name := range fallbacks {
		for _, mbox := range mailboxes {
			if strings.EqualFold(mbox.Name, name) {
				return mbox.Name, nil
			}
		}
	}

	return "", fmt.Errorf("no mailbox found with role %s", role)
}

// GetSyncMailboxes returns mail-relevant mailboxes to sync.
// Uses LIST and filters out \Noselect containers and non-mail Exchange folders
// (calendar, contacts, tasks, etc.) via the ExcludedIMAPMailboxes blacklist.
func (c *Client) GetSyncMailboxes() ([]string, error) {
	if c.conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	mailboxes, err := c.ListMailboxes()
	if err != nil {
		return nil, err
	}

	var result []string
	for _, mbox := range mailboxes {
		// Skip \Noselect folders (container folders, not real mailboxes)
		isNoselect := false
		for _, attr := range mbox.Attributes {
			if strings.EqualFold(attr, "\\Noselect") {
				isNoselect = true
				break
			}
		}
		if isNoselect {
			continue
		}

		// Skip non-mail Exchange folders
		if config.IsIMAPMailboxExcluded(mbox.Name) {
			slog.Debug("Skipping excluded mailbox", "module", "IMAP", "mailbox", mbox.Name) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
			continue
		}

		result = append(result, mbox.Name)
	}

	slog.Debug("Sync mailboxes", "module", "IMAP", "count", len(result)) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
	return result, nil
}

// FetchBodyStructure fetches the BODYSTRUCTURE for a single message by UID.
// Returns the parsed MIME tree used to locate attachment sections.
func (c *Client) FetchBodyStructure(uid uint32) (*imap.BodyStructure, error) {
	if c.conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)

	items := []imap.FetchItem{
		imap.FetchUid,
		imap.FetchBodyStructure,
	}

	messages := make(chan *imap.Message, 1)
	done := make(chan error, 1)

	go func() {
		done <- c.conn.UidFetch(seqSet, items, messages)
	}()

	var result *imap.Message
	for msg := range messages {
		result = msg
	}

	if err := <-done; err != nil {
		return nil, fmt.Errorf("failed to fetch BODYSTRUCTURE for UID %d: %w", uid, err)
	}
	if result == nil {
		return nil, fmt.Errorf("no message found for UID %d", uid)
	}

	return result.BodyStructure, nil
}

// FetchBodySection fetches a specific MIME section by UID and streams it to w.
// sectionPath uses IMAP section numbering (e.g. []int{2,1} for section "2.1").
// Uses BODY.PEEK to avoid setting \Seen.
func (c *Client) FetchBodySection(uid uint32, sectionPath []int, w io.Writer) error {
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}

	section := &imap.BodySectionName{
		BodyPartName: imap.BodyPartName{
			Path: sectionPath,
		},
		Peek: true,
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddNum(uid)

	items := []imap.FetchItem{section.FetchItem()}

	messages := make(chan *imap.Message, 1)
	done := make(chan error, 1)

	go func() {
		done <- c.conn.UidFetch(seqSet, items, messages)
	}()

	var msg *imap.Message
	for m := range messages {
		msg = m
	}

	if err := <-done; err != nil {
		return fmt.Errorf("failed to fetch BODY[%v] for UID %d: %w", sectionPath, uid, err)
	}
	if msg == nil {
		return fmt.Errorf("no message found for UID %d", uid)
	}

	// go-imap may return the literal under a slightly different key than
	// the exact BodySectionName we requested (e.g. server omits PEEK).
	// Iterate msg.Body to find the response.
	for _, body := range msg.Body {
		if body == nil {
			continue
		}
		if _, err := io.Copy(w, body); err != nil {
			return fmt.Errorf("failed to stream section: %w", err)
		}
		return nil
	}

	return fmt.Errorf("section %v not found in response for UID %d", sectionPath, uid)
}

// FetchHeadersOnly fetches only the RFC822 headers for a batch of UIDs.
// Returns a map of UID → raw header bytes.
func (c *Client) FetchHeadersOnly(uids []uint32) (map[uint32][]byte, error) {
	if c.conn == nil {
		return nil, fmt.Errorf("not connected")
	}
	if len(uids) == 0 {
		return make(map[uint32][]byte), nil
	}

	section := &imap.BodySectionName{
		BodyPartName: imap.BodyPartName{
			Specifier: imap.HeaderSpecifier,
		},
		Peek: true,
	}

	seqSet := new(imap.SeqSet)
	for _, uid := range uids {
		seqSet.AddNum(uid)
	}

	items := []imap.FetchItem{imap.FetchUid, section.FetchItem()}

	messages := make(chan *imap.Message, len(uids))
	done := make(chan error, 1)
	go func() {
		done <- c.conn.UidFetch(seqSet, items, messages)
	}()

	result := make(map[uint32][]byte)
	for msg := range messages {
		for _, body := range msg.Body {
			if body == nil {
				continue
			}
			data, err := io.ReadAll(body)
			if err == nil && len(data) > 0 {
				result[msg.Uid] = data
			}
			break
		}
	}

	if err := <-done; err != nil {
		return nil, fmt.Errorf("fetch headers: %w", err)
	}
	return result, nil
}

// Close closes the IMAP connection
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}

	// Logout gracefully
	if err := c.conn.Logout(); err != nil {
		// Still try to close the connection
		c.conn.Close()
		return err
	}

	return c.conn.Close()
}

// IsConnected returns true if the connection is still alive
func (c *Client) IsConnected() bool {
	if c.conn == nil {
		return false
	}
	// Send NOOP to check connection - this also acts as a keepalive
	return c.conn.Noop() == nil
}

// Reconnect closes the current connection and establishes a new one
func (c *Client) Reconnect() error {
	// Close existing connection if any
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}

	// Connect and authenticate
	if err := c.Connect(); err != nil {
		return fmt.Errorf("reconnect failed: %w", err)
	}

	if err := c.Authenticate(); err != nil {
		c.Close()
		return fmt.Errorf("reconnect auth failed: %w", err)
	}

	slog.Debug("Reconnected", "module", "IMAP", "host", c.account.IMAP.Host) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
	return nil
}

// Account returns the account config
func (c *Client) Account() *config.AccountConfig {
	return c.account
}

// XOAuth2Client implements go-sasl Client interface for XOAUTH2
type XOAuth2Client struct {
	username string
	token    string
}

// NewXOAuth2Client creates a new XOAUTH2 SASL client
func NewXOAuth2Client(username, token string) *XOAuth2Client {
	return &XOAuth2Client{
		username: username,
		token:    token,
	}
}

// Start begins SASL authentication
func (c *XOAuth2Client) Start() (mech string, ir []byte, err error) {
	mech = "XOAUTH2"
	// XOAUTH2 format: user=<email>\x01auth=Bearer <token>\x01\x01
	ir = []byte(fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", c.username, c.token))
	return
}

// Next handles server challenges
func (c *XOAuth2Client) Next(challenge []byte) ([]byte, error) {
	// Server sent an error - return empty response to get error details
	return nil, fmt.Errorf("XOAUTH2 error: %s", string(challenge))
}
