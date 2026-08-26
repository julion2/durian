package config

import (
	"strings"
	"time"
)

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
	// Autosync enables the background download-only calendar sync in
	// `durian serve`. It NEVER writes to the remote calendar — uploads,
	// remote deletes, conflicts and RSVPs stay behind the interactive
	// `durian calendar sync` confirmation gate. A pointer so an absent
	// calendar block (configs that do not amend the schema get no Pkl
	// defaults) resolves to the schema default true instead of false.
	Autosync *bool `pkl:"autosync" json:"autosync"`
	// AutosyncInterval is the autosync interval in seconds (Pkl default 600,
	// schema minimum 60). Polling a calendar API harder than the 120 s mail
	// fetch buys nothing and courts provider throttling. Use
	// Config.CalendarAutosyncInterval for the resolved time.Duration.
	AutosyncInterval int `pkl:"autosync_interval" json:"autosync_interval"`
	// AutosyncUpload selects what the background autosync may push to the
	// remote calendar: "none" (default, also for "" / absent block) keeps
	// autosync strictly download-only; "safe" additionally auto-applies
	// provably non-notifying local changes — creates/edits of attendee-less
	// events. Remote deletes, conflicts, RSVPs and anything that could make
	// the provider send email always wait for the interactive
	// `durian calendar sync`. Use Config.CalendarAutosyncUploadSafe for the
	// per-account resolution.
	AutosyncUpload string `pkl:"autosync_upload" json:"autosync_upload"`
	// Conflict is the GLOBAL two-way sync conflict policy ("remote"|"local"|
	// "newer"), used when an account does not set its own. Empty resolves to
	// the schema default "newer". Use Config.CalendarConflictPolicy.
	Conflict string `pkl:"conflict" json:"conflict"`
	// Local lists the calendars that exist only on disk. They belong to no
	// account and no provider, which is what keeps them out of the sync
	// engine: it plans only the calendars a provider reports, so a directory
	// no provider knows about is never uploaded and never pruned.
	Local []LocalCalendarConfig `pkl:"local_calendars" json:"local_calendars"`
}

// LocalCalendarConfig is one calendar that lives only on disk: a directory of
// .ics files in the vdir collection layout, with no remote counterpart.
type LocalCalendarConfig struct {
	// Name is the display name and the identifier used with --calendar.
	Name string `pkl:"name" json:"name"`
	// Path is the directory holding the .ics files. It may be anywhere —
	// pointing it at an existing khal or vdirsyncer collection is the point.
	// Use LocalCalendarPath for the expanded form.
	Path string `pkl:"path" json:"path"`
	// Color is a "#RRGGBB" display color, or "" to fall back to the
	// collection's own color meta file.
	Color string `pkl:"color" json:"color"`
	// ReadOnly refuses every write to this calendar, so a folder another tool
	// owns can be displayed without durian ever editing it.
	ReadOnly bool `pkl:"read_only" json:"read_only"`
}

// LocalCalendarAccount is the reserved account identifier under which the
// local calendars are addressed. It is not a real account: it has no
// credentials, no provider and no sync state, and `durian calendar sync`
// rejects it.
const LocalCalendarAccount = "local"

// LocalCalendars returns the configured local calendars with their paths
// expanded. Entries without a name or path are dropped — a nameless calendar
// cannot be addressed and a pathless one has nothing to show.
func (c *Config) LocalCalendars() []LocalCalendarConfig {
	out := make([]LocalCalendarConfig, 0, len(c.Calendar.Local))
	for _, lc := range c.Calendar.Local {
		name := strings.TrimSpace(lc.Name)
		if name == "" || strings.TrimSpace(lc.Path) == "" {
			continue
		}
		// Names address the calendar, so a duplicate would make one of them
		// unreachable and the resolution order arbitrary. Keep the first. Match
		// with EqualFold — exactly how FindLocalCalendar looks them up — so a
		// Unicode name whose ToLower differs but folds equal can't slip past
		// dedup and then shadow the earlier entry at lookup time.
		if containsFoldName(out, name) {
			continue
		}
		lc.Name = name
		lc.Path = ExpandPath(lc.Path)
		out = append(out, lc)
	}
	return out
}

// containsFoldName reports whether any calendar in cals has a name equal to want
// under Unicode case folding (the same comparison FindLocalCalendar uses).
func containsFoldName(cals []LocalCalendarConfig, want string) bool {
	for _, lc := range cals {
		if strings.EqualFold(lc.Name, want) {
			return true
		}
	}
	return false
}

// FindLocalCalendar returns the local calendar with the given display name
// (case-insensitive).
func (c *Config) FindLocalCalendar(name string) (LocalCalendarConfig, bool) {
	for _, lc := range c.LocalCalendars() {
		if strings.EqualFold(lc.Name, name) {
			return lc, true
		}
	}
	return LocalCalendarConfig{}, false
}

// AccountCalendarConfig configures the vdir calendar features of one account.
type AccountCalendarConfig struct {
	// Enabled controls whether this account participates in calendar reads and
	// sync. Nil mirrors the schema/default behavior: enabled.
	Enabled *bool    `pkl:"enabled" json:"enabled"`
	Dir     string   `pkl:"dir" json:"dir"`         // Subdir name under the vdir base path (default: alias, else lowercased name)
	Include []string `pkl:"include" json:"include"` // Calendar display names to export and sync (empty = all)
	// Conflict overrides the global calendar.conflict policy for this account
	// when an event changed on both sides: "remote" keeps the provider version
	// (the local file is backed up first), "local" keeps the local file,
	// "newer" keeps the side modified last. Empty = use the global setting.
	Conflict string `pkl:"conflict" json:"conflict"`
	// Autosync overrides the global calendar.autosync toggle for this
	// account (nil = use the global setting).
	Autosync *bool `pkl:"autosync" json:"autosync"`
	// AutosyncUpload overrides the global calendar.autosync_upload mode
	// ("none" or "safe") for this account ("" = use the global setting).
	AutosyncUpload string `pkl:"autosync_upload" json:"autosync_upload"`
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
	JMAP             *JMAPConfig            `pkl:"jmap" json:"jmap"`
	Auth             *AuthConfig            `pkl:"auth" json:"auth"`
	OAuth            *OAuthConfig           `pkl:"oauth" json:"oauth"`
	Calendar         *AccountCalendarConfig `pkl:"calendar" json:"calendar"` // Vdir calendar export settings (nil = defaults)
	// SyncEngine selects the sync implementation for this account:
	//   "engine" — the provider-agnostic engine on the IMAP backend;
	//   "graph"  — the engine on the Microsoft Graph backend;
	//   "gmail"  — the engine on the Gmail REST backend;
	//   "jmap"   — the engine on the configured JMAP server;
	//   "legacy" — the classic IMAP syncer.
	// When unset, Microsoft accounts default to "graph", Google accounts to
	// "gmail", and everything else to "legacy" (see EffectiveSyncEngine).
	SyncEngine string `pkl:"sync_engine" json:"sync_engine"`
}

// EffectiveSyncEngine resolves the sync engine for this account, applying the
// default when sync_engine is unset: Microsoft accounts (including shared/
// delegated mailboxes) sync via Graph, all others via the legacy IMAP syncer.
// Microsoft accounts may not opt into an IMAP path — validation rejects
// "legacy"/"engine" for them (see ValidateConfig).
func (a *AccountConfig) EffectiveSyncEngine() string {
	if a.SyncEngine != "" {
		return a.SyncEngine
	}
	if a.OAuth != nil {
		switch a.OAuth.Provider {
		case "microsoft":
			return "graph"
		case "google":
			return "gmail"
		}
	}
	return "legacy"
}

// UsesSyncEngine reports whether this account should sync via the new
// provider-agnostic engine (on any backend) instead of the legacy IMAP syncer.
func (a *AccountConfig) UsesSyncEngine() bool {
	e := a.EffectiveSyncEngine()
	return e == "engine" || e == "graph" || e == "gmail" || e == "jmap"
}

// UsesGraphBackend reports whether the engine should drive the Microsoft Graph
// backend rather than the IMAP backend for this account.
func (a *AccountConfig) UsesGraphBackend() bool {
	return a.EffectiveSyncEngine() == "graph"
}

// UsesGmailBackend reports whether the engine should drive the Gmail REST
// backend rather than the IMAP backend for this account.
func (a *AccountConfig) UsesGmailBackend() bool {
	return a.EffectiveSyncEngine() == "gmail"
}

// UsesJMAPBackend reports whether the engine should drive the standards-based
// JMAP backend rather than IMAP or a provider-specific REST backend.
func (a *AccountConfig) UsesJMAPBackend() bool {
	return a.EffectiveSyncEngine() == "jmap"
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

// CalendarInclude returns the calendar display names included in export and
// two-way sync for this account, or nil to include all calendars.
func (a *AccountConfig) CalendarInclude() []string {
	if a.Calendar == nil {
		return nil
	}
	return a.Calendar.Include
}

// CalendarEnabled reports whether this account participates in calendar reads
// and sync. Accounts remain enabled when the block or field is absent for
// backwards compatibility with configs written before the switch existed.
func (a *AccountConfig) CalendarEnabled() bool {
	return a.Calendar == nil || a.Calendar.Enabled == nil || *a.Calendar.Enabled
}

// CalendarConflictPolicy resolves the two-way calendar sync conflict policy
// for an account: the account override if set, else the global
// calendar.conflict, else the schema default "newer".
func (c *Config) CalendarConflictPolicy(a *AccountConfig) string {
	if a != nil && a.Calendar != nil && a.Calendar.Conflict != "" {
		return a.Calendar.Conflict
	}
	if c.Calendar.Conflict != "" {
		return c.Calendar.Conflict
	}
	return "newer"
}

// CalendarAutosyncEnabled resolves the effective calendar autosync toggle for
// an account: the per-account override when set, else the global
// calendar.autosync setting, else the schema default (on).
func (c *Config) CalendarAutosyncEnabled(a *AccountConfig) bool {
	if a.Calendar != nil && a.Calendar.Autosync != nil {
		return *a.Calendar.Autosync
	}
	if c.Calendar.Autosync != nil {
		return *c.Calendar.Autosync
	}
	return true // mirrors the Pkl schema default: autosync on
}

// CalendarAutosyncUploadSafe resolves whether the background calendar
// autosync may auto-apply the provably non-notifying subset of local changes
// for this account: the per-account calendar.autosync_upload override when
// set, else the global calendar.autosync_upload, else "none". Only the exact
// value "safe" enables it — every other value (including absent) stays
// download-only, the fail-safe default.
func (c *Config) CalendarAutosyncUploadSafe(a *AccountConfig) bool {
	mode := c.Calendar.AutosyncUpload
	if a != nil && a.Calendar != nil && a.Calendar.AutosyncUpload != "" {
		mode = a.Calendar.AutosyncUpload
	}
	return mode == "safe"
}

// calendarAutosyncDefaultInterval mirrors the Pkl schema default for
// calendar.autosync_interval (600 s), used when the configured value is
// missing or below the schema minimum of 60 s (e.g. a Go-constructed Config
// that bypassed Pkl evaluation).
const calendarAutosyncDefaultInterval = 600 * time.Second

// CalendarAutosyncInterval returns the calendar autosync interval as a
// duration, falling back to the 60-second default when the configured value
// is below the schema minimum of 60 seconds.
func (c *Config) CalendarAutosyncInterval() time.Duration {
	if c.Calendar.AutosyncInterval < 60 {
		return calendarAutosyncDefaultInterval
	}
	return time.Duration(c.Calendar.AutosyncInterval) * time.Second
}

// GetAuthEmail returns the email used for OAuth token lookup.
// For shared mailboxes, this is the delegating user; otherwise the account email.
func (a *AccountConfig) GetAuthEmail() string {
	if a.AuthEmail != "" {
		return a.AuthEmail
	}
	return a.Email
}

// IsDelegatedMailbox reports whether this account authenticates as a different
// user than its own address (a shared mailbox reached via a delegating token
// owner).
func (a *AccountConfig) IsDelegatedMailbox() bool {
	return a.AuthEmail != "" && a.AuthEmail != a.Email
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

// JMAPConfig contains the JMAP session endpoint and authentication mode. The
// credential itself is stored in the JMAP-specific OS keychain service under
// the account email.
type JMAPConfig struct {
	SessionURL string `pkl:"session_url" json:"session_url"`
	Auth       string `pkl:"auth" json:"auth"` // "password" or "bearer"
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
