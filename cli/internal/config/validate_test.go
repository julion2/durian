package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// --- ValidateConfig ---

func TestValidateConfig_ValidMinimal(t *testing.T) {
	cfg := &Config{
		Accounts: []AccountConfig{
			{Name: "Test", Email: "test@example.com"},
		},
	}
	errs := ValidateConfig(cfg)
	for _, e := range errs {
		if e.Severity == "error" {
			t.Errorf("unexpected error: %s", e)
		}
	}
}

func TestValidateConfig_NoAccounts(t *testing.T) {
	cfg := &Config{}
	errs := ValidateConfig(cfg)
	if len(errs) == 0 {
		t.Error("expected warning for no accounts")
	}
}

func TestValidateConfig_MissingEmail(t *testing.T) {
	cfg := &Config{Accounts: []AccountConfig{{Name: "Test"}}}
	errs := ValidateConfig(cfg)
	hasError := false
	for _, e := range errs {
		if strings.Contains(e.Field, "email") && e.Severity == "error" {
			hasError = true
		}
	}
	if !hasError {
		t.Error("expected error for missing email")
	}
}

func TestValidateConfig_InvalidSMTPAuth(t *testing.T) {
	cfg := &Config{Accounts: []AccountConfig{{
		Name: "Test", Email: "test@example.com",
		SMTP: SMTPConfig{Host: "smtp.example.com", Port: 587, Auth: "plain"},
	}}}
	errs := ValidateConfig(cfg)
	hasError := false
	for _, e := range errs {
		if strings.Contains(e.Message, "password") && strings.Contains(e.Message, "oauth2") {
			hasError = true
		}
	}
	if !hasError {
		t.Error("expected error for invalid auth method")
	}
}

func TestValidateConfig_OAuthGoogleRequiresClientID(t *testing.T) {
	cfg := &Config{Accounts: []AccountConfig{{
		Name: "Test", Email: "test@example.com",
		SMTP:  SMTPConfig{Host: "smtp.gmail.com", Port: 587, Auth: "oauth2"},
		OAuth: &OAuthConfig{Provider: "google"},
	}}}
	errs := ValidateConfig(cfg)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Field, "client_id") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected error for missing Google client_id, got errors: %v", errs)
	}
}

func TestValidateConfig_SyncEngineInvalidValue(t *testing.T) {
	cfg := &Config{Accounts: []AccountConfig{{
		Name: "Test", Email: "test@example.com",
		SyncEngine: "turbo",
	}}}
	errs := ValidateConfig(cfg)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Field, "sync_engine") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected error for invalid sync_engine value, got errors: %v", errs)
	}
}

func TestValidateConfig_SyncEngineRejectsGmail(t *testing.T) {
	cfg := &Config{Accounts: []AccountConfig{{
		Name: "Test", Email: "test@example.com",
		SMTP:       SMTPConfig{Host: "smtp.gmail.com", Port: 587, Auth: "oauth2"},
		OAuth:      &OAuthConfig{Provider: "google", ClientID: "id", ClientSecret: "secret"},
		SyncEngine: "engine",
	}}}
	errs := ValidateConfig(cfg)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Field, "sync_engine") && strings.Contains(e.Message, "Gmail") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected error rejecting sync_engine=engine for Google account, got errors: %v", errs)
	}
}

func TestValidateConfig_CalendarConflictPolicy(t *testing.T) {
	cfg := &Config{Accounts: []AccountConfig{{
		Name: "Test", Email: "test@example.com",
		OAuth:    &OAuthConfig{Provider: "microsoft"},
		Calendar: &AccountCalendarConfig{Conflict: "merge"},
	}}}
	errs := ValidateConfig(cfg)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Field, "calendar.conflict") && e.Severity == "error" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected error for invalid calendar conflict policy, got errors: %v", errs)
	}

	// Valid values (and empty) pass.
	for _, valid := range []string{"", "remote", "local", "newer"} {
		cfg.Accounts[0].Calendar.Conflict = valid
		for _, e := range ValidateConfig(cfg) {
			if strings.Contains(e.Field, "calendar.conflict") {
				t.Errorf("unexpected error for conflict=%q: %s", valid, e)
			}
		}
	}
}

func TestAccountCalendarConflictPolicyDefault(t *testing.T) {
	a := &AccountConfig{}
	if got := a.CalendarConflictPolicy(); got != "remote" {
		t.Errorf("nil calendar block: policy = %q, want remote", got)
	}
	a.Calendar = &AccountCalendarConfig{}
	if got := a.CalendarConflictPolicy(); got != "remote" {
		t.Errorf("empty conflict: policy = %q, want remote", got)
	}
	a.Calendar.Conflict = "newer"
	if got := a.CalendarConflictPolicy(); got != "newer" {
		t.Errorf("policy = %q, want newer", got)
	}
}

func TestCalendarAutosyncResolution(t *testing.T) {
	on, off := true, false
	cfg := &Config{}
	a := &AccountConfig{}

	// Absent everywhere: the schema default (on) applies — configs that do
	// not amend the Pkl schema carry no calendar block at all.
	if !cfg.CalendarAutosyncEnabled(a) {
		t.Error("no calendar config anywhere: want the default (enabled)")
	}
	cfg.Calendar.Autosync = &on
	if !cfg.CalendarAutosyncEnabled(a) {
		t.Error("global on, no account block: want enabled")
	}
	a.Calendar = &AccountCalendarConfig{}
	if !cfg.CalendarAutosyncEnabled(a) {
		t.Error("global on, account block without override: want enabled")
	}
	a.Calendar.Autosync = &off
	if cfg.CalendarAutosyncEnabled(a) {
		t.Error("account override false must win over global on")
	}
	cfg.Calendar.Autosync = &off
	a.Calendar.Autosync = &on
	if !cfg.CalendarAutosyncEnabled(a) {
		t.Error("account override true must win over global off")
	}
	a.Calendar.Autosync = nil
	if cfg.CalendarAutosyncEnabled(a) {
		t.Error("global off, no override: want disabled")
	}
	cfg.Calendar.Autosync = nil
	a.Calendar.Autosync = &off
	if cfg.CalendarAutosyncEnabled(a) {
		t.Error("global absent, account override false: want disabled")
	}
}

func TestCalendarAutosyncInterval(t *testing.T) {
	cfg := &Config{}
	if got := cfg.CalendarAutosyncInterval(); got != 600*time.Second {
		t.Errorf("unset interval = %v, want the 600s default", got)
	}
	cfg.Calendar.AutosyncInterval = 300
	if got := cfg.CalendarAutosyncInterval(); got != 300*time.Second {
		t.Errorf("interval = %v, want 300s", got)
	}
	cfg.Calendar.AutosyncInterval = 60
	if got := cfg.CalendarAutosyncInterval(); got != 60*time.Second {
		t.Errorf("interval = %v, want 60s (schema minimum)", got)
	}
	cfg.Calendar.AutosyncInterval = 30
	if got := cfg.CalendarAutosyncInterval(); got != 600*time.Second {
		t.Errorf("below-minimum interval = %v, want the 600s default", got)
	}
}

func TestValidateConfig_SignatureRefMissing(t *testing.T) {
	cfg := &Config{
		Signatures: map[string]string{"default": "Best regards"},
		Accounts: []AccountConfig{{
			Name: "Test", Email: "test@example.com",
			DefaultSignature: "nonexistent",
		}},
	}
	errs := ValidateConfig(cfg)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "nonexistent") && strings.Contains(e.Message, "not found") {
			found = true
		}
	}
	if !found {
		t.Error("expected warning for missing signature reference")
	}
}

func TestValidateConfig_MultipleDefaults(t *testing.T) {
	cfg := &Config{Accounts: []AccountConfig{
		{Name: "A", Email: "a@example.com", Default: true},
		{Name: "B", Email: "b@example.com", Default: true},
	}}
	errs := ValidateConfig(cfg)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "2 accounts marked as default") {
			found = true
		}
	}
	if !found {
		t.Error("expected warning for multiple defaults")
	}
}

// --- ValidateRules ---

func okValidator(_ string) error   { return nil }
func failValidator(_ string) error { return errors.New("parse error") }

func TestValidateRules_Valid(t *testing.T) {
	rules := []RuleConfig{{Name: "Test", Match: "from:@test.com", AddTags: []string{"test"}}}
	errs := ValidateRules(rules, nil, okValidator)
	for _, e := range errs {
		if e.Severity == "error" {
			t.Errorf("unexpected error: %s", e)
		}
	}
}

func TestValidateRules_MissingName(t *testing.T) {
	rules := []RuleConfig{{Match: "from:test", AddTags: []string{"x"}}}
	errs := ValidateRules(rules, nil, okValidator)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Field, "name") {
			found = true
		}
	}
	if !found {
		t.Error("expected error for missing name")
	}
}

func TestValidateRules_InvalidMatch(t *testing.T) {
	rules := []RuleConfig{{Name: "Bad", Match: "(((", AddTags: []string{"x"}}}
	errs := ValidateRules(rules, nil, failValidator)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "invalid expression") {
			found = true
		}
	}
	if !found {
		t.Error("expected error for invalid match expression")
	}
}

func TestValidateRules_NoOp(t *testing.T) {
	rules := []RuleConfig{{Name: "NoOp", Match: "from:test"}}
	errs := ValidateRules(rules, nil, okValidator)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "no-op") {
			found = true
		}
	}
	if !found {
		t.Error("expected warning for no-op rule")
	}
}

func TestValidateRules_UnknownAccount(t *testing.T) {
	cfg := &Config{Accounts: []AccountConfig{{Name: "Work", Alias: "work"}}}
	rules := []RuleConfig{{Name: "R", Match: "from:test", AddTags: []string{"x"}, Accounts: []string{"nonexistent"}}}
	errs := ValidateRules(rules, cfg, okValidator)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "nonexistent") {
			found = true
		}
	}
	if !found {
		t.Error("expected warning for unknown account")
	}
}

// --- ValidateProfiles ---

func TestValidateProfiles_Valid(t *testing.T) {
	profiles := []ProfileConfig{{Name: "All", Accounts: []string{"*"}}}
	errs := ValidateProfiles(profiles, nil)
	for _, e := range errs {
		if e.Severity == "error" {
			t.Errorf("unexpected error: %s", e)
		}
	}
}

func TestValidateProfiles_MissingName(t *testing.T) {
	profiles := []ProfileConfig{{Accounts: []string{"*"}}}
	errs := ValidateProfiles(profiles, nil)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Field, "name") {
			found = true
		}
	}
	if !found {
		t.Error("expected error for missing name")
	}
}

func TestValidateProfiles_InvalidColor(t *testing.T) {
	profiles := []ProfileConfig{{Name: "Test", Accounts: []string{"*"}, Color: "not-hex"}}
	errs := ValidateProfiles(profiles, nil)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "hex color") {
			found = true
		}
	}
	if !found {
		t.Error("expected error for invalid color")
	}
}

func TestValidateProfiles_ValidColors(t *testing.T) {
	tests := []string{"#FFF", "#3B82F6", "#abc", "#AABBCC"}
	for _, c := range tests {
		profiles := []ProfileConfig{{Name: "T", Accounts: []string{"*"}, Color: c}}
		errs := ValidateProfiles(profiles, nil)
		for _, e := range errs {
			if strings.Contains(e.Message, "hex color") {
				t.Errorf("color %q should be valid but got error: %s", c, e)
			}
		}
	}
}

func TestValidateProfiles_UnknownAccount(t *testing.T) {
	cfg := &Config{Accounts: []AccountConfig{{Name: "Work", Alias: "work"}}}
	profiles := []ProfileConfig{{Name: "T", Accounts: []string{"bogus"}}}
	errs := ValidateProfiles(profiles, cfg)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "bogus") {
			found = true
		}
	}
	if !found {
		t.Error("expected warning for unknown account")
	}
}

func TestValidateProfiles_FolderMissingQuery(t *testing.T) {
	profiles := []ProfileConfig{{
		Name: "T", Accounts: []string{"*"},
		Folders: []FolderConfig{{Name: "Inbox", Icon: "tray"}},
	}}
	errs := ValidateProfiles(profiles, nil)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Field, "query") {
			found = true
		}
	}
	if !found {
		t.Error("expected error for missing folder query")
	}
}

// --- ValidateKeymaps ---

func TestValidateKeymaps_Valid(t *testing.T) {
	km := &KeymapConfig{
		GlobalSettings: KeymapGlobalSettings{SequenceTimeout: 1.0},
		Keymaps: []KeymapEntry{
			{Action: "next_email", Key: "j", Context: "list"},
		},
	}
	errs := ValidateKeymaps(km)
	for _, e := range errs {
		if e.Severity == "error" {
			t.Errorf("unexpected error: %s", e)
		}
	}
}

func TestValidateKeymaps_UnknownAction(t *testing.T) {
	km := &KeymapConfig{
		GlobalSettings: KeymapGlobalSettings{SequenceTimeout: 1.0},
		Keymaps:        []KeymapEntry{{Action: "fly_to_moon", Key: "x"}},
	}
	errs := ValidateKeymaps(km)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "fly_to_moon") {
			found = true
		}
	}
	if !found {
		t.Error("expected warning for unknown action")
	}
}

func TestValidateKeymaps_InvalidModifier(t *testing.T) {
	km := &KeymapConfig{
		GlobalSettings: KeymapGlobalSettings{SequenceTimeout: 1.0},
		Keymaps:        []KeymapEntry{{Action: "next_email", Key: "j", Modifiers: []string{"alt"}}},
	}
	errs := ValidateKeymaps(km)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "alt") {
			found = true
		}
	}
	if !found {
		t.Error("expected error for invalid modifier")
	}
}

func TestValidateKeymaps_DuplicateBinding(t *testing.T) {
	km := &KeymapConfig{
		GlobalSettings: KeymapGlobalSettings{SequenceTimeout: 1.0},
		Keymaps: []KeymapEntry{
			{Action: "next_email", Key: "j", Context: "list"},
			{Action: "scroll_down", Key: "j", Context: "list"},
		},
	}
	errs := ValidateKeymaps(km)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "duplicate") {
			found = true
		}
	}
	if !found {
		t.Error("expected warning for duplicate binding")
	}
}

func TestValidateKeymaps_InvalidContext(t *testing.T) {
	km := &KeymapConfig{
		GlobalSettings: KeymapGlobalSettings{SequenceTimeout: 1.0},
		Keymaps:        []KeymapEntry{{Action: "next_email", Key: "j", Context: "unknown_ctx"}},
	}
	errs := ValidateKeymaps(km)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "unknown_ctx") {
			found = true
		}
	}
	if !found {
		t.Error("expected error for unknown context")
	}
}

// --- isValidEmail ---

func TestIsValidEmail(t *testing.T) {
	tests := []struct {
		email string
		valid bool
	}{
		{"user@example.com", true},
		{"user@sub.domain.co.uk", true},
		{"a@b.c", true},
		{"", false},
		{"noat", false},
		{"@example.com", false},
		{"user@", false},
		{"user@.com", false},
		{"user@com.", false},
	}
	for _, tt := range tests {
		if got := isValidEmail(tt.email); got != tt.valid {
			t.Errorf("isValidEmail(%q) = %v, want %v", tt.email, got, tt.valid)
		}
	}
}
