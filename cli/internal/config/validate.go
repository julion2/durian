package config

import (
	"fmt"
	"regexp"
	"strings"
)

// ValidationError represents a single validation issue.
type ValidationError struct {
	File     string // "config.pkl", "rules.pkl", etc.
	Field    string // e.g. "accounts[0].smtp.host"
	Message  string
	Severity string // "error" or "warning"
}

func (e ValidationError) String() string {
	if e.Field != "" {
		return fmt.Sprintf("[%s] %s: %s", e.File, e.Field, e.Message)
	}
	return fmt.Sprintf("[%s] %s", e.File, e.Message)
}

// RuleQueryValidator is a function that validates a rule match expression.
// Injected from the imap package to avoid a circular dependency.
type RuleQueryValidator func(query string) error

// ValidateConfig validates config.pkl and returns any issues found.
func ValidateConfig(cfg *Config) []ValidationError {
	var errs []ValidationError
	add := func(field, msg string) {
		errs = append(errs, ValidationError{File: "config.pkl", Field: field, Message: msg, Severity: "error"})
	}
	warn := func(field, msg string) {
		errs = append(errs, ValidationError{File: "config.pkl", Field: field, Message: msg, Severity: "warning"})
	}

	// Settings validation
	if cfg.Settings.AccentColor != "" {
		hexColor := regexp.MustCompile(`^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)
		if !hexColor.MatchString(cfg.Settings.AccentColor) {
			add("settings.accent_color", fmt.Sprintf("invalid hex color: %q (expected #RGB or #RRGGBB)", cfg.Settings.AccentColor))
		}
	}

	// indexed_headers: RFC 822 header names = ASCII letters + digits +
	// hyphen, starting with a letter. The runtime merge in imap.headerSet
	// drops empty strings and dedups case-insensitively, but a typo like
	// "X Gitlab Reason " (spaces) would silently be a no-op — surface it
	// at validate time instead.
	headerName := regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*$`)
	for i, name := range cfg.Sync.IndexedHeaders {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			add(fmt.Sprintf("sync.indexed_headers[%d]", i), "empty header name")
			continue
		}
		if !headerName.MatchString(trimmed) {
			add(fmt.Sprintf("sync.indexed_headers[%d]", i),
				fmt.Sprintf("invalid MIME header name: %q (must match [A-Za-z][A-Za-z0-9-]*)", name))
		}
	}

	if len(cfg.Accounts) == 0 {
		warn("accounts", "no accounts configured")
		return errs
	}

	// Alias validation (reuse existing)
	if err := cfg.ValidateAliases(); err != nil {
		add("accounts", err.Error())
	}

	defaultCount := 0
	for i, acct := range cfg.Accounts {
		prefix := fmt.Sprintf("accounts[%d]", i)

		if acct.Email == "" {
			add(prefix+".email", "required")
		} else if !isValidEmail(acct.Email) {
			add(prefix+".email", fmt.Sprintf("invalid email: %q", acct.Email))
		}

		if acct.Name == "" {
			warn(prefix+".name", "account name is empty")
		}

		if acct.Default {
			defaultCount++
		}

		if acct.AuthEmail != "" && !isValidEmail(acct.AuthEmail) {
			add(prefix+".auth_email", fmt.Sprintf("invalid email: %q", acct.AuthEmail))
		}

		// Signature reference
		if acct.DefaultSignature != "" && cfg.Signatures != nil {
			if _, ok := cfg.Signatures[acct.DefaultSignature]; !ok {
				keys := make([]string, 0, len(cfg.Signatures))
				for k := range cfg.Signatures {
					keys = append(keys, k)
				}
				warn(prefix+".default_signature", fmt.Sprintf("%q not found in [signatures] (available: %s)", acct.DefaultSignature, strings.Join(keys, ", ")))
			}
		}

		// SMTP
		if acct.SMTP.Host != "" || acct.SMTP.Port != 0 {
			if acct.SMTP.Host == "" {
				add(prefix+".smtp.host", "required when smtp is configured")
			}
			if acct.SMTP.Port <= 0 {
				add(prefix+".smtp.port", "must be a positive integer")
			}
			if acct.SMTP.Auth != "" && acct.SMTP.Auth != "password" && acct.SMTP.Auth != "oauth2" {
				add(prefix+".smtp.auth", fmt.Sprintf("must be \"password\" or \"oauth2\", got %q", acct.SMTP.Auth))
			}
			if acct.SMTP.MaxAttachmentSize != "" {
				if _, err := ParseSize(acct.SMTP.MaxAttachmentSize); err != nil {
					add(prefix+".smtp.max_attachment_size", err.Error())
				}
			}
		}

		// IMAP
		if acct.IMAP.Host != "" || acct.IMAP.Port != 0 {
			if acct.IMAP.Host == "" {
				add(prefix+".imap.host", "required when imap is configured")
			}
			if acct.IMAP.Port <= 0 {
				add(prefix+".imap.port", "must be a positive integer")
			}
			if acct.IMAP.Auth != "" && acct.IMAP.Auth != "password" && acct.IMAP.Auth != "oauth2" {
				add(prefix+".imap.auth", fmt.Sprintf("must be \"password\" or \"oauth2\", got %q", acct.IMAP.Auth))
			}
		}

		// OAuth
		if acct.SMTP.Auth == "oauth2" || acct.IMAP.Auth == "oauth2" {
			if acct.OAuth == nil || acct.OAuth.Provider == "" {
				add(prefix+".oauth.provider", "required when auth is oauth2")
			} else if acct.OAuth.Provider != "google" && acct.OAuth.Provider != "microsoft" {
				add(prefix+".oauth.provider", fmt.Sprintf("must be \"google\" or \"microsoft\", got %q", acct.OAuth.Provider))
			}
			if acct.OAuth != nil && acct.OAuth.Provider == "google" {
				if acct.OAuth.ClientID == "" {
					add(prefix+".oauth.client_id", "required for Google OAuth")
				}
				if acct.OAuth.ClientSecret == "" {
					add(prefix+".oauth.client_secret", "required for Google OAuth")
				}
			}
		}
	}

	if defaultCount > 1 {
		warn("accounts", fmt.Sprintf("%d accounts marked as default (expected at most 1)", defaultCount))
	}

	return errs
}

// ValidateRules validates rules.pkl entries. Pass a RuleQueryValidator to check match syntax.
func ValidateRules(rules []RuleConfig, cfg *Config, queryValidator RuleQueryValidator) []ValidationError {
	var errs []ValidationError
	add := func(field, msg string) {
		errs = append(errs, ValidationError{File: "rules.pkl", Field: field, Message: msg, Severity: "error"})
	}
	warn := func(field, msg string) {
		errs = append(errs, ValidationError{File: "rules.pkl", Field: field, Message: msg, Severity: "warning"})
	}

	aliases := configAliasSet(cfg)

	for i, rule := range rules {
		prefix := fmt.Sprintf("rules[%d]", i)

		if rule.Name == "" {
			add(prefix+".name", "required")
		}

		if rule.Match == "" {
			add(prefix+".match", "required")
		} else if queryValidator != nil {
			if err := queryValidator(rule.Match); err != nil {
				add(prefix+".match", fmt.Sprintf("invalid expression: %v", err))
			}
		}

		if len(rule.AddTags) == 0 && len(rule.RemoveTags) == 0 && rule.Exec == "" {
			warn(prefix, "rule has no add_tags, remove_tags, or exec (no-op)")
		}

		if rule.ExecTimeout < 0 {
			add(prefix+".exec_timeout", "must be non-negative")
		}

		for _, acct := range rule.Accounts {
			if _, ok := aliases[strings.ToLower(acct)]; !ok {
				warn(prefix+".accounts", fmt.Sprintf("account %q not found in config", acct))
			}
		}
	}

	return errs
}

// ValidateProfiles validates profiles.pkl entries against the config.
func ValidateProfiles(profiles []ProfileConfig, cfg *Config) []ValidationError {
	var errs []ValidationError
	add := func(field, msg string) {
		errs = append(errs, ValidationError{File: "profiles.pkl", Field: field, Message: msg, Severity: "error"})
	}
	warn := func(field, msg string) {
		errs = append(errs, ValidationError{File: "profiles.pkl", Field: field, Message: msg, Severity: "warning"})
	}

	aliases := configAliasSet(cfg)
	hexColor := regexp.MustCompile(`^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)
	defaultCount := 0

	for i, profile := range profiles {
		prefix := fmt.Sprintf("profile[%d]", i)

		if profile.Name == "" {
			add(prefix+".name", "required")
		}

		if len(profile.Accounts) == 0 {
			add(prefix+".accounts", "required (use [\"*\"] for all accounts)")
		}

		for _, acct := range profile.Accounts {
			if acct == "*" {
				continue
			}
			if _, ok := aliases[strings.ToLower(acct)]; !ok {
				warn(prefix+".accounts", fmt.Sprintf("account %q not found in config", acct))
			}
		}

		if profile.Default {
			defaultCount++
		}

		if profile.Color != "" && !hexColor.MatchString(profile.Color) {
			add(prefix+".color", fmt.Sprintf("invalid hex color: %q (expected #RGB or #RRGGBB)", profile.Color))
		}

		for j, folder := range profile.Folders {
			fp := fmt.Sprintf("%s.folders[%d]", prefix, j)
			if folder.Name == "" {
				add(fp+".name", "required")
			}
			if folder.Icon == "" {
				warn(fp+".icon", "empty (will show no icon)")
			}
			if folder.Query == "" && folder.Icon != "" {
				// Section headers have no query (and typically no icon)
				add(fp+".query", "required (omit query only for section headers)")
			}
		}
	}

	if defaultCount > 1 {
		warn("profiles", fmt.Sprintf("%d profiles marked as default (expected at most 1)", defaultCount))
	}

	return errs
}

// Known valid keymap values.
var (
	// validKeymapActions maps action names to their descriptions.
	validKeymapActions = map[string]string{
		"next_email": "Next email", "prev_email": "Previous email",
		"first_email": "First email", "last_email": "Last email",
		"page_down": "Half-page down", "page_up": "Half-page up",
		"archive": "Archive email", "compose": "Compose new email",
		"reply": "Reply", "reply_all": "Reply all", "forward": "Forward",
		"toggle_read": "Toggle read/unread", "toggle_star": "Toggle star",
		"delete": "Delete email", "tag_op": "Tag operation",
		"go_inbox": "Go to inbox", "go_sent": "Go to sent",
		"go_drafts": "Go to drafts", "go_archive": "Go to archive",
		"search": "Search emails", "close_detail": "Close/back",
		"reload_inbox": "Reload inbox", "tag_picker": "Open tag picker",
		"enter_visual_mode": "Enter visual mode", "enter_toggle_mode": "Enter toggle visual mode",
		"toggle_selection": "Toggle current email", "exit_visual_mode": "Exit visual mode",
		"select_next": "Next item", "select_prev": "Previous item",
		"exit_insert": "Exit insert mode",
		"scroll_down": "Scroll down", "scroll_up": "Scroll up",
		"enter_thread": "Enter thread view",
		"next_message": "Next message", "prev_message": "Previous message",
		"next_profile": "Next profile", "prev_profile": "Previous profile",
		"open_in_browser": "Open in browser", "copy_link": "Copy link",
		"go_folder": "Go to folder by position", "next_folder": "Next folder",
		"prev_folder": "Previous folder", "folder_picker": "Open folder picker",
	}
	validKeymapModifiers = map[string]bool{"cmd": true, "ctrl": true, "shift": true}
	validKeymapContexts  = map[string]bool{
		"list": true, "search": true, "tag_picker": true, "thread": true, "compose_normal": true,
	}
)

// ValidateKeymaps validates keymaps.pkl.
func ValidateKeymaps(keymaps *KeymapConfig) []ValidationError {
	var errs []ValidationError
	add := func(field, msg string) {
		errs = append(errs, ValidationError{File: "keymaps.pkl", Field: field, Message: msg, Severity: "error"})
	}
	warn := func(field, msg string) {
		errs = append(errs, ValidationError{File: "keymaps.pkl", Field: field, Message: msg, Severity: "warning"})
	}

	if keymaps.GlobalSettings.SequenceTimeout <= 0 {
		warn("global_settings.sequence_timeout", "should be positive (using default 1.0s)")
	}

	seen := make(map[string]int) // "key+modifiers+context" → index
	for i, entry := range keymaps.Keymaps {
		prefix := fmt.Sprintf("keymaps[%d]", i)

		if entry.Action == "" {
			add(prefix+".action", "required")
		} else if _, ok := validKeymapActions[entry.Action]; !ok {
			warn(prefix+".action", fmt.Sprintf("unknown action %q", entry.Action))
		}

		if entry.Key == "" {
			add(prefix+".key", "required")
		}

		for _, mod := range entry.Modifiers {
			if !validKeymapModifiers[mod] {
				add(prefix+".modifiers", fmt.Sprintf("unknown modifier %q (valid: cmd, ctrl, shift)", mod))
			}
		}

		ctx := entry.Context
		if ctx == "" {
			ctx = "list"
		}
		if !validKeymapContexts[ctx] {
			add(prefix+".context", fmt.Sprintf("unknown context %q", ctx))
		}

		// Check duplicates
		dupKey := fmt.Sprintf("%s|%s|%s", entry.Key, strings.Join(entry.Modifiers, ","), ctx)
		if prev, ok := seen[dupKey]; ok {
			warn(prefix, fmt.Sprintf("duplicate binding: same key/modifiers/context as keymaps[%d]", prev))
		}
		seen[dupKey] = i
	}

	return errs
}

// configAliasSet builds a lowercase set of account aliases + names from config.
func configAliasSet(cfg *Config) map[string]bool {
	aliases := make(map[string]bool)
	if cfg == nil {
		return aliases
	}
	for _, acct := range cfg.Accounts {
		if acct.Alias != "" {
			aliases[strings.ToLower(acct.Alias)] = true
		}
		if acct.Name != "" {
			aliases[strings.ToLower(acct.Name)] = true
		}
	}
	return aliases
}

// isValidEmail does a basic email format check.
func isValidEmail(email string) bool {
	// Simple check: has @ with something before and after, and a dot after @
	at := strings.Index(email, "@")
	if at <= 0 || at >= len(email)-1 {
		return false
	}
	domain := email[at+1:]
	return strings.Contains(domain, ".") && !strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".")
}
