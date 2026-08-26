package config

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	// ErrNoAccounts is returned when no accounts are configured
	ErrNoAccounts = errors.New("no accounts configured")
	// ErrNoDefaultAccount is returned when no default account is found
	ErrNoDefaultAccount = errors.New("no default account configured")
	// ErrAccountNotFound is returned when the requested account is not found
	ErrAccountNotFound = errors.New("account not found")
	// ErrSignatureNotFound is returned when the requested signature is not found
	ErrSignatureNotFound = errors.New("signature not found")
	// ErrDuplicateAlias is returned when two accounts have the same alias
	ErrDuplicateAlias = errors.New("duplicate alias found")
	// ErrInvalidAlias is returned when an alias contains invalid characters
	ErrInvalidAlias = errors.New("invalid alias: use only a-z, 0-9, -, _")
)

// GetDefaultAccount returns the account marked as default
// If no account is marked default, returns the first account
func (c *Config) GetDefaultAccount() (*AccountConfig, error) {
	if len(c.Accounts) == 0 {
		return nil, ErrNoAccounts
	}

	for i := range c.Accounts {
		if c.Accounts[i].Default {
			return &c.Accounts[i], nil
		}
	}

	// Return first account if none marked as default
	return &c.Accounts[0], nil
}

// GetAccountByEmail returns the account with the given email
func (c *Config) GetAccountByEmail(email string) (*AccountConfig, error) {
	if len(c.Accounts) == 0 {
		return nil, ErrNoAccounts
	}

	for i := range c.Accounts {
		if c.Accounts[i].Email == email {
			return &c.Accounts[i], nil
		}
	}
	return nil, ErrAccountNotFound
}

// GetAccountByName returns the account with the given name
func (c *Config) GetAccountByName(name string) (*AccountConfig, error) {
	if len(c.Accounts) == 0 {
		return nil, ErrNoAccounts
	}

	for i := range c.Accounts {
		if strings.EqualFold(c.Accounts[i].Name, name) {
			return &c.Accounts[i], nil
		}
	}
	return nil, ErrAccountNotFound
}

// GetAccountByIdentifier finds an account by email, alias, or name (in that order)
// The lookup is case-insensitive for alias and name.
func (c *Config) GetAccountByIdentifier(identifier string) (*AccountConfig, error) {
	if len(c.Accounts) == 0 {
		return nil, ErrNoAccounts
	}

	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return nil, ErrAccountNotFound
	}

	// 1. Exact email match (case-sensitive)
	for i := range c.Accounts {
		if c.Accounts[i].Email == identifier {
			return &c.Accounts[i], nil
		}
	}

	// 2. Case-insensitive alias match
	identifierLower := strings.ToLower(identifier)
	for i := range c.Accounts {
		if c.Accounts[i].Alias != "" && strings.ToLower(c.Accounts[i].Alias) == identifierLower {
			return &c.Accounts[i], nil
		}
	}

	// 3. Case-insensitive name match
	for i := range c.Accounts {
		if strings.ToLower(c.Accounts[i].Name) == identifierLower {
			return &c.Accounts[i], nil
		}
	}

	return nil, ErrAccountNotFound
}

// ValidateAliases checks that all aliases are unique and valid
func (c *Config) ValidateAliases() error {
	aliasRegex := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	seen := make(map[string]string) // alias -> email

	for _, account := range c.Accounts {
		if account.Alias == "" {
			continue
		}

		// Validate alias format
		if !aliasRegex.MatchString(account.Alias) {
			return fmt.Errorf("%w: '%s' for account %s", ErrInvalidAlias, account.Alias, account.Email)
		}

		aliasLower := strings.ToLower(account.Alias)

		// Check for duplicate aliases
		if existingEmail, exists := seen[aliasLower]; exists {
			return fmt.Errorf("%w: '%s' used by both %s and %s", ErrDuplicateAlias, account.Alias, existingEmail, account.Email)
		}

		// Check that alias doesn't conflict with another account's email
		for _, other := range c.Accounts {
			if strings.ToLower(other.Email) == aliasLower {
				return fmt.Errorf("alias '%s' conflicts with email address %s", account.Alias, other.Email)
			}
		}

		seen[aliasLower] = account.Email
	}

	return nil
}

// GetAliasOrName returns the alias if set, otherwise the name
func (a *AccountConfig) GetAliasOrName() string {
	if a.Alias != "" {
		return a.Alias
	}
	return a.Name
}

// ListAccountIdentifiers returns a list of all available identifiers (aliases/names)
func (c *Config) ListAccountIdentifiers() []string {
	var identifiers []string
	for _, account := range c.Accounts {
		if account.Alias != "" {
			identifiers = append(identifiers, account.Alias)
		} else if account.Name != "" {
			identifiers = append(identifiers, account.Name)
		} else {
			identifiers = append(identifiers, account.Email)
		}
	}
	return identifiers
}

// GetSignature returns the signature with the given name
func (c *Config) GetSignature(name string) (string, error) {
	if c.Signatures == nil {
		return "", ErrSignatureNotFound
	}

	sig, ok := c.Signatures[name]
	if !ok {
		return "", ErrSignatureNotFound
	}

	return sig, nil
}

// HasAccounts returns true if at least one account is configured
func (c *Config) HasAccounts() bool {
	return len(c.Accounts) > 0
}

// AccountCount returns the number of configured accounts
func (c *Config) AccountCount() int {
	return len(c.Accounts)
}

// DefaultMaxAttachmentSize is the default maximum attachment size (25MB)
const DefaultMaxAttachmentSize int64 = 25 * 1024 * 1024

// ErrInvalidSize is returned when a size string cannot be parsed
var ErrInvalidSize = errors.New("invalid size format")

// ParseSize parses a human-readable size string (e.g. "25MB", "1GB", "500KB")
// and returns the size in bytes. Supports B, KB, MB, GB (case-insensitive).
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, ErrInvalidSize
	}

	// Match number followed by optional unit
	re := regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*(B|KB|MB|GB)?$`)
	matches := re.FindStringSubmatch(s)
	if matches == nil {
		return 0, fmt.Errorf("%w: %s", ErrInvalidSize, s)
	}

	value, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s", ErrInvalidSize, s)
	}

	unit := matches[2]
	if unit == "" {
		unit = "B"
	}

	var multiplier float64
	switch unit {
	case "B":
		multiplier = 1
	case "KB":
		multiplier = 1024
	case "MB":
		multiplier = 1024 * 1024
	case "GB":
		multiplier = 1024 * 1024 * 1024
	}

	return int64(value * multiplier), nil
}

// FormatSize formats a size in bytes as a human-readable string
func FormatSize(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)

	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.1fGB", float64(bytes)/GB)
	case bytes >= MB:
		return fmt.Sprintf("%.1fMB", float64(bytes)/MB)
	case bytes >= KB:
		return fmt.Sprintf("%.1fKB", float64(bytes)/KB)
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}

// GetMaxAttachmentSize returns the max attachment size for the account in bytes.
// Returns DefaultMaxAttachmentSize if not configured or invalid.
func (a *AccountConfig) GetMaxAttachmentSize() int64 {
	if a.SMTP.MaxAttachmentSize == "" {
		return DefaultMaxAttachmentSize
	}

	size, err := ParseSize(a.SMTP.MaxAttachmentSize)
	if err != nil {
		return DefaultMaxAttachmentSize
	}

	return size
}

// IMAP defaults
const (
	DefaultIMAPMaxMessages = 5000
	DefaultIMAPBatchSize   = 100 // Smaller batches for stability and progress feedback
)

// DefaultIMAPMailboxes are the mailboxes synced by default if not configured
var DefaultIMAPMailboxes = []string{"INBOX", "Sent", "Sent Items", "Sent Messages", "Drafts", "Archive"}

// ExcludedIMAPMailboxes are excluded from sync by default.
// Includes trash/spam and Exchange/M365 non-mail folders.
var ExcludedIMAPMailboxes = []string{
	// Trash/Spam
	"Junk", "Spam", "Trash", "Deleted", "Deleted Items", "Deleted Messages",
	"Junk-E-Mail", "Papierkorb", "Gelöschte Elemente",
	// Exchange non-mail folders (contain Exchange objects, not emails)
	"Calendar", "Kalender",
	"Contacts", "Kontakte",
	"Tasks", "Aufgaben",
	"Journal", "Notes", "Notizen",
	"Conversation History", "Unterhaltungsverlauf",
	"Sync Issues",
	// Exchange system folders
	"RSS Feeds", "RSS-Feeds",
	"Outbox", "Postausgang",
	"Suggested Contacts", "Clutter",
	"PersonMetadata", "Snoozed",
	"ExternalContacts", "Recipient Cache",
	"Yammer Root", "Files",
	"Social Activity Notifications",
	"Quick Step Settings", "Conversation Action Settings",
	// Outlook reminder folder
	"Erneut erinnern aktiviert",
}

// GetIMAPMaxMessages returns the max messages per mailbox, defaulting to 5000
func (a *AccountConfig) GetIMAPMaxMessages() int {
	if a.IMAP.MaxMessages <= 0 {
		return DefaultIMAPMaxMessages
	}
	return a.IMAP.MaxMessages
}

// GetIMAPBatchSize returns the batch size for fetching, defaulting to
// DefaultIMAPBatchSize (100) when not configured.
func (a *AccountConfig) GetIMAPBatchSize() int {
	if a.IMAP.BatchSize <= 0 {
		return DefaultIMAPBatchSize
	}
	return a.IMAP.BatchSize
}

// AccountIdentifier returns the lowercased account name, used as the
// account column in SQLite and as a watcher map key.
func (a *AccountConfig) AccountIdentifier() string {
	return strings.ToLower(a.Name)
}

// GetIMAPMailboxes returns the mailboxes to sync, using smart defaults if not configured
func (a *AccountConfig) GetIMAPMailboxes() []string {
	if len(a.IMAP.Mailboxes) > 0 {
		return a.IMAP.Mailboxes
	}
	return DefaultIMAPMailboxes
}

// IsIMAPMailboxExcluded checks if a mailbox should be excluded from sync
func IsIMAPMailboxExcluded(name string) bool {
	nameLower := strings.ToLower(name)
	for _, excluded := range ExcludedIMAPMailboxes {
		excludedLower := strings.ToLower(excluded)
		if nameLower == excludedLower {
			return true
		}
		// Prefix match with word boundary (e.g., "Deleted" matches "Deleted Items" but not "DeletedArchive")
		if strings.HasPrefix(nameLower, excludedLower) && len(nameLower) > len(excludedLower) {
			next := nameLower[len(excludedLower)]
			if next == ' ' || next == '/' {
				return true
			}
		}
	}
	return false
}

// GetAccountsWithIMAP returns all accounts that have IMAP configured
func (c *Config) GetAccountsWithIMAP() []*AccountConfig {
	var accounts []*AccountConfig
	for i := range c.Accounts {
		if c.Accounts[i].IMAP.Host != "" {
			accounts = append(accounts, &c.Accounts[i])
		}
	}
	return accounts
}

// GetGraphAccounts returns all accounts that sync through the Microsoft Graph
// backend. These are exactly the accounts the IMAP IDLE watcher must skip, so
// the daemon drives them with the polling engine watcher instead. IMAP config
// is irrelevant here: a Graph account needs no IMAP host at all.
func (c *Config) GetGraphAccounts() []*AccountConfig {
	var accounts []*AccountConfig
	for i := range c.Accounts {
		if c.Accounts[i].UsesGraphBackend() {
			accounts = append(accounts, &c.Accounts[i])
		}
	}
	return accounts
}

// GetEngineAccounts returns accounts driven by the provider-neutral background
// watcher in durian serve. Gmail is excluded: its API engine remains on-demand
// and is triggered periodically by the GUI rather than by this daemon watcher.
func (c *Config) GetEngineAccounts() []*AccountConfig {
	var accounts []*AccountConfig
	for i := range c.Accounts {
		if c.Accounts[i].UsesSyncEngine() && !c.Accounts[i].UsesGmailBackend() {
			accounts = append(accounts, &c.Accounts[i])
		}
	}
	return accounts
}

// GetMailSyncAccounts returns every account with a usable sync transport.
// Native API accounts do not need an IMAP host.
func (c *Config) GetMailSyncAccounts() []*AccountConfig {
	var accounts []*AccountConfig
	for i := range c.Accounts {
		account := &c.Accounts[i]
		if account.IMAP.Host != "" || account.UsesGraphBackend() || account.UsesGmailBackend() || account.UsesJMAPBackend() {
			accounts = append(accounts, account)
		}
	}
	return accounts
}
