package config

import "strings"

// Config represents the complete Durian configuration
type Config struct {
	Settings   SettingsConfig    `pkl:"settings" json:"settings"`
	Sync       SyncConfig        `pkl:"sync" json:"sync"`
	Contacts   ContactsConfig    `pkl:"contacts" json:"contacts"`
	Calendar   CalendarConfig    `pkl:"calendar" json:"calendar"`
	Signatures map[string]string `pkl:"signatures" json:"signatures"`
	Accounts   []AccountConfig   `pkl:"accounts" json:"accounts"`
}

// SettingsConfig holds app settings from config.pkl [settings].
type SettingsConfig struct {
	Theme                string `pkl:"theme" json:"theme"`
	NotificationsEnabled bool   `pkl:"notifications_enabled" json:"notifications_enabled"`
	LoadRemoteImages     bool   `pkl:"load_remote_images" json:"load_remote_images"`
	AccentColor          string `pkl:"accent_color" json:"accent_color"`
}

// SyncConfig contains sync settings from config.pkl [sync].
type SyncConfig struct {
	GuiAutoSync       bool                   `pkl:"gui_auto_sync" json:"gui_auto_sync"`
	AutoFetchInterval int                    `pkl:"auto_fetch_interval" json:"auto_fetch_interval"`
	FullSyncInterval  int                    `pkl:"full_sync_interval" json:"full_sync_interval"`
	TagSync           *TagSyncConfig         `pkl:"tag_sync" json:"tag_sync"`
	AttachmentCache   *AttachmentCacheConfig `pkl:"attachment_cache" json:"attachment_cache"`
	// IndexedHeaders supplements the built-in set of MIME headers that
	// sync fetches into message_headers for rule matching. See
	// cli/internal/imap/sync_mailbox.go builtinSelectedHeaders for the
	// built-ins. Case-insensitive merge, user entries are additive only.
	IndexedHeaders []string `pkl:"indexed_headers" json:"indexed_headers"`
}

// AttachmentCacheConfig configures the GUI attachment cache.
type AttachmentCacheConfig struct {
	MaxSizeMB int `pkl:"max_size_mb" json:"max_size_mb"` // Max cache size in MB (default: 100)
	TTLDays   int `pkl:"ttl_days" json:"ttl_days"`       // Days to keep cached attachments (default: 7)
}

// TagSyncConfig configures the optional remote tag sync server.
type TagSyncConfig struct {
	URL    string `pkl:"url" json:"url"`         // e.g. "http://nas:8724"
	APIKey string `pkl:"api_key" json:"api_key"` // Shared secret
}

// ContactsConfig contains contacts database settings
type ContactsConfig struct {
	DBPath string `pkl:"db_path" json:"db_path"` // Path to SQLite DB (default: ~/.local/share/durian/contacts.db)
}

// CalendarConfig contains global calendar export settings.
type CalendarConfig struct {
	VdirPath string `pkl:"vdir_path" json:"vdir_path"` // Base dir for exported vdir calendars (default: ~/.local/share/durian/calendars)
}

// AccountCalendarConfig configures the vdir calendar export of one account.
type AccountCalendarConfig struct {
	Dir     string   `pkl:"dir" json:"dir"`         // Subdir name under the vdir base path (default: alias, else lowercased name)
	Include []string `pkl:"include" json:"include"` // Calendar display names to export (empty = all)
}

// AccountConfig represents a single email account
type AccountConfig struct {
	Name             string                 `pkl:"name" json:"name"`
	DisplayName      string                 `pkl:"display_name" json:"display_name"` // Full name for From header (e.g., "Julian Schenker")
	Email            string                 `pkl:"email" json:"email"`
	AuthEmail        string                 `pkl:"auth_email" json:"auth_email"` // Delegating user for shared mailbox OAuth (token owner)
	Alias            string                 `pkl:"alias" json:"alias"`           // Short alias for CLI (e.g., "work", "personal")
	Default          bool                   `pkl:"default" json:"default"`
	DefaultSignature string                 `pkl:"default_signature" json:"default_signature"`
	Notifications    *bool                  `pkl:"notifications" json:"notifications"` // Per-account notification override (nil = use global setting)
	SMTP             SMTPConfig             `pkl:"smtp" json:"smtp"`
	IMAP             IMAPConfig             `pkl:"imap" json:"imap"`
	Auth             *AuthConfig            `pkl:"auth" json:"auth"`
	OAuth            *OAuthConfig           `pkl:"oauth" json:"oauth"`
	Calendar         *AccountCalendarConfig `pkl:"calendar" json:"calendar"` // Vdir calendar export settings (nil = defaults)
	// SyncEngine selects the sync implementation for this account:
	//   ""/"legacy" (default) — the classic IMAP syncer;
	//   "engine"              — the provider-agnostic engine on the IMAP backend;
	//   "graph"               — the engine on the Microsoft Graph backend.
	// Not supported for Google accounts (the engine has no X-GM-LABELS path);
	// "graph" additionally requires a Microsoft account.
	SyncEngine string `pkl:"sync_engine" json:"sync_engine"`
}

// UsesSyncEngine reports whether this account should sync via the new
// provider-agnostic engine (on either backend) instead of the legacy IMAP syncer.
func (a *AccountConfig) UsesSyncEngine() bool {
	return a.SyncEngine == "engine" || a.SyncEngine == "graph"
}

// UsesGraphBackend reports whether the engine should drive the Microsoft Graph
// backend rather than the IMAP backend for this account.
func (a *AccountConfig) UsesGraphBackend() bool {
	return a.SyncEngine == "graph"
}

// CalendarDir returns the subdirectory name under the vdir base path for this
// account's calendar export: the configured calendar dir if set, else the
// account alias, else the lowercased account name.
func (a *AccountConfig) CalendarDir() string {
	if a.Calendar != nil && a.Calendar.Dir != "" {
		return a.Calendar.Dir
	}
	if a.Alias != "" {
		return a.Alias
	}
	return strings.ToLower(a.Name)
}

// CalendarInclude returns the list of calendar display names to export for
// this account, or nil to export all calendars.
func (a *AccountConfig) CalendarInclude() []string {
	if a.Calendar == nil {
		return nil
	}
	return a.Calendar.Include
}

// GetAuthEmail returns the email used for OAuth token lookup.
// For shared mailboxes, this is the delegating user; otherwise the account email.
func (a *AccountConfig) GetAuthEmail() string {
	if a.AuthEmail != "" {
		return a.AuthEmail
	}
	return a.Email
}

// SMTPConfig contains SMTP server settings.
// Note: TLS is decided by port (465 = implicit TLS, 587 = STARTTLS) in
// cli/internal/smtp/client.go — there is intentionally no ssl flag.
type SMTPConfig struct {
	Host              string `pkl:"host" json:"host"`
	Port              int    `pkl:"port" json:"port"`
	Auth              string `pkl:"auth" json:"auth"`                               // "password" or "oauth2"
	MaxAttachmentSize string `pkl:"max_attachment_size" json:"max_attachment_size"` // e.g. "25MB", default 25MB
}

// IMAPConfig contains IMAP server settings.
// Note: TLS is always implicit (port 993, wrapped unconditionally in
// cli/internal/imap/client.go) — there is intentionally no ssl flag.
type IMAPConfig struct {
	Host        string   `pkl:"host" json:"host"`
	Port        int      `pkl:"port" json:"port"`
	Auth        string   `pkl:"auth" json:"auth"`
	MaxMessages int      `pkl:"max_messages" json:"max_messages"`
	BatchSize   int      `pkl:"batch_size" json:"batch_size"`
	Mailboxes   []string `pkl:"mailboxes" json:"mailboxes"`
}

// AuthConfig contains password-based authentication settings.
// The keychain service name is the hardcoded constant
// keychain.PasswordKeychainService; there is no override field.
type AuthConfig struct {
	Username string `pkl:"username" json:"username"`
}

// OAuthConfig contains OAuth2 authentication settings
type OAuthConfig struct {
	Provider     string `pkl:"provider" json:"provider"`           // "google", "microsoft"
	ClientID     string `pkl:"client_id" json:"client_id"`         // Azure App Client ID or Google Client ID
	ClientSecret string `pkl:"client_secret" json:"client_secret"` // Required for Google, optional for Microsoft
	Tenant       string `pkl:"tenant" json:"tenant"`               // Microsoft tenant (default: "common")
}
