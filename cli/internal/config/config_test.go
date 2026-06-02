package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	// Bazel's sandbox does not set HOME, which breaks os.UserHomeDir()
	// and all tests that rely on ~ expansion. Set a deterministic fallback.
	if os.Getenv("HOME") == "" {
		dir, err := os.MkdirTemp("", "durian-config-test-*")
		if err != nil {
			panic(err)
		}
		os.Setenv("HOME", dir)
		defer os.RemoveAll(dir)
	}
	os.Exit(m.Run())
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name        string
		file        string
		wantErr     bool
		errContains string
	}{
		{
			name:    "valid config",
			file:    "testdata/valid_config.pkl",
			wantErr: false,
		},
		{
			name:    "minimal config",
			file:    "testdata/minimal_config.pkl",
			wantErr: false,
		},
		{
			name:        "invalid syntax",
			file:        "testdata/invalid_syntax.pkl",
			wantErr:     true,
			errContains: "failed to load config",
		},
		{
			name:        "nonexistent file",
			file:        "testdata/does_not_exist.pkl",
			wantErr:     true,
			errContains: "failed to load config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(tt.file)
			if tt.wantErr {
				if err == nil {
					t.Errorf("Load() expected error, got nil")
				} else if tt.errContains != "" && !containsString(err.Error(), tt.errContains) {
					t.Errorf("Load() error = %v, want error containing %q", err, tt.errContains)
				}
				return
			}
			if err != nil {
				t.Errorf("Load() unexpected error: %v", err)
				return
			}
			if cfg == nil {
				t.Errorf("Load() returned nil config")
			}
		})
	}
}

func TestLoadValidConfig(t *testing.T) {
	cfg, err := Load("testdata/valid_config.pkl")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Signatures
	if len(cfg.Signatures) != 2 {
		t.Errorf("Signatures count = %d, want 2", len(cfg.Signatures))
	}
	if cfg.Signatures["personal"] != "Cheers,\nTest User" {
		t.Errorf("Signatures[personal] = %q, want %q", cfg.Signatures["personal"], "Cheers,\nTest User")
	}

	// Accounts
	if len(cfg.Accounts) != 2 {
		t.Errorf("Accounts count = %d, want 2", len(cfg.Accounts))
	}

	// First account (Work)
	work := cfg.Accounts[0]
	if work.Name != "Work" {
		t.Errorf("Accounts[0].Name = %q, want %q", work.Name, "Work")
	}
	if work.Email != "test@work.com" {
		t.Errorf("Accounts[0].Email = %q, want %q", work.Email, "test@work.com")
	}
	if !work.Default {
		t.Error("Accounts[0].Default = false, want true")
	}
	if work.SMTP.Host != "smtp.work.com" {
		t.Errorf("Accounts[0].SMTP.Host = %q, want %q", work.SMTP.Host, "smtp.work.com")
	}
	if work.SMTP.Port != 587 {
		t.Errorf("Accounts[0].SMTP.Port = %d, want %d", work.SMTP.Port, 587)
	}
	if work.SMTP.Auth != "password" {
		t.Errorf("Accounts[0].SMTP.Auth = %q, want %q", work.SMTP.Auth, "password")
	}
	if work.Auth == nil || work.Auth.Username != "test@work.com" {
		username := ""
		if work.Auth != nil {
			username = work.Auth.Username
		}
		t.Errorf("Accounts[0].Auth.Username = %q, want %q", username, "test@work.com")
	}

	// Second account (Personal with OAuth)
	personal := cfg.Accounts[1]
	if personal.SMTP.Auth != "oauth2" {
		t.Errorf("Accounts[1].SMTP.Auth = %q, want %q", personal.SMTP.Auth, "oauth2")
	}
	if personal.OAuth == nil || personal.OAuth.Provider != "google" {
		provider := ""
		if personal.OAuth != nil {
			provider = personal.OAuth.Provider
		}
		t.Errorf("Accounts[1].OAuth.Provider = %q, want %q", provider, "google")
	}
}

func TestDefaultPath(t *testing.T) {
	// Test with XDG_CONFIG_HOME set
	t.Run("with XDG_CONFIG_HOME", func(t *testing.T) {
		oldXDG := os.Getenv("XDG_CONFIG_HOME")
		defer os.Setenv("XDG_CONFIG_HOME", oldXDG)

		os.Setenv("XDG_CONFIG_HOME", "/custom/config")
		path := DefaultPath()
		expected := "/custom/config/durian/config.pkl"
		if path != expected {
			t.Errorf("DefaultPath() = %q, want %q", path, expected)
		}
	})

	// Test without XDG_CONFIG_HOME (uses ~/.config)
	t.Run("without XDG_CONFIG_HOME", func(t *testing.T) {
		oldXDG := os.Getenv("XDG_CONFIG_HOME")
		defer os.Setenv("XDG_CONFIG_HOME", oldXDG)

		os.Unsetenv("XDG_CONFIG_HOME")
		path := DefaultPath()

		home, _ := os.UserHomeDir()
		expected := filepath.Join(home, ".config", "durian", "config.pkl")
		if path != expected {
			t.Errorf("DefaultPath() = %q, want %q", path, expected)
		}
	})
}

func TestExpandPath(t *testing.T) {
	home, _ := os.UserHomeDir()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "empty path",
			input: "",
			want:  "",
		},
		{
			name:  "tilde only",
			input: "~",
			want:  home,
		},
		{
			name:  "tilde with path",
			input: "~/.config/durian",
			want:  filepath.Join(home, ".config", "durian"),
		},
		{
			name:  "absolute path",
			input: "/usr/local/etc/durian",
			want:  "/usr/local/etc/durian",
		},
		{
			name:  "relative path",
			input: "config/durian",
			want:  "config/durian",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExpandPath(tt.input)
			if got != tt.want {
				t.Errorf("ExpandPath(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestExpandPathEnvVar(t *testing.T) {
	oldVal := os.Getenv("TEST_DURIAN_VAR")
	defer os.Setenv("TEST_DURIAN_VAR", oldVal)

	os.Setenv("TEST_DURIAN_VAR", "/test/path")

	got := ExpandPath("$TEST_DURIAN_VAR/config.pkl")
	want := "/test/path/config.pkl"
	if got != want {
		t.Errorf("ExpandPath() = %q, want %q", got, want)
	}
}

func TestExists(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{
			name: "existing file",
			path: "testdata/valid_config.pkl",
			want: true,
		},
		{
			name: "nonexistent file",
			path: "testdata/nonexistent.pkl",
			want: false,
		},
		{
			name: "directory (not a file)",
			path: "testdata",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Exists(tt.path)
			if got != tt.want {
				t.Errorf("Exists(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestDefault(t *testing.T) {
	cfg := Default()

	if cfg == nil {
		t.Fatal("Default() returned nil")
	}
	if len(cfg.Accounts) != 0 {
		t.Errorf("Default Accounts length = %d, want 0", len(cfg.Accounts))
	}
	if cfg.Signatures == nil {
		t.Error("Default Signatures is nil")
	}
}

func TestGetDefaultAccount(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *Config
		wantEmail string
		wantErr   error
	}{
		{
			name: "has default account",
			cfg: &Config{
				Accounts: []AccountConfig{
					{Name: "A", Email: "a@test.com", Default: false},
					{Name: "B", Email: "b@test.com", Default: true},
				},
			},
			wantEmail: "b@test.com",
			wantErr:   nil,
		},
		{
			name: "no default - returns first",
			cfg: &Config{
				Accounts: []AccountConfig{
					{Name: "A", Email: "a@test.com", Default: false},
					{Name: "B", Email: "b@test.com", Default: false},
				},
			},
			wantEmail: "a@test.com",
			wantErr:   nil,
		},
		{
			name: "no accounts",
			cfg: &Config{
				Accounts: []AccountConfig{},
			},
			wantEmail: "",
			wantErr:   ErrNoAccounts,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acc, err := tt.cfg.GetDefaultAccount()
			if err != tt.wantErr {
				t.Errorf("GetDefaultAccount() error = %v, want %v", err, tt.wantErr)
				return
			}
			if tt.wantErr == nil && acc.Email != tt.wantEmail {
				t.Errorf("GetDefaultAccount().Email = %q, want %q", acc.Email, tt.wantEmail)
			}
		})
	}
}

func TestGetAccountByEmail(t *testing.T) {
	cfg := &Config{
		Accounts: []AccountConfig{
			{Name: "Work", Email: "work@test.com"},
			{Name: "Personal", Email: "personal@test.com"},
		},
	}

	tests := []struct {
		name     string
		email    string
		wantName string
		wantErr  error
	}{
		{
			name:     "found",
			email:    "work@test.com",
			wantName: "Work",
			wantErr:  nil,
		},
		{
			name:     "not found",
			email:    "unknown@test.com",
			wantName: "",
			wantErr:  ErrAccountNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acc, err := cfg.GetAccountByEmail(tt.email)
			if err != tt.wantErr {
				t.Errorf("GetAccountByEmail() error = %v, want %v", err, tt.wantErr)
				return
			}
			if tt.wantErr == nil && acc.Name != tt.wantName {
				t.Errorf("GetAccountByEmail().Name = %q, want %q", acc.Name, tt.wantName)
			}
		})
	}
}

func TestGetAccountByName(t *testing.T) {
	cfg := &Config{
		Accounts: []AccountConfig{
			{Name: "Work", Email: "work@test.com"},
			{Name: "Personal", Email: "personal@test.com"},
		},
	}

	tests := []struct {
		name      string
		accName   string
		wantEmail string
		wantErr   error
	}{
		{
			name:      "found",
			accName:   "Work",
			wantEmail: "work@test.com",
			wantErr:   nil,
		},
		{
			name:      "not found",
			accName:   "Unknown",
			wantEmail: "",
			wantErr:   ErrAccountNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acc, err := cfg.GetAccountByName(tt.accName)
			if err != tt.wantErr {
				t.Errorf("GetAccountByName() error = %v, want %v", err, tt.wantErr)
				return
			}
			if tt.wantErr == nil && acc.Email != tt.wantEmail {
				t.Errorf("GetAccountByName().Email = %q, want %q", acc.Email, tt.wantEmail)
			}
		})
	}
}

func TestGetAccountByEmailNoAccounts(t *testing.T) {
	cfg := &Config{Accounts: []AccountConfig{}}
	_, err := cfg.GetAccountByEmail("any@test.com")
	if err != ErrNoAccounts {
		t.Errorf("GetAccountByEmail() error = %v, want %v", err, ErrNoAccounts)
	}
}

func TestGetAccountByNameNoAccounts(t *testing.T) {
	cfg := &Config{Accounts: []AccountConfig{}}
	_, err := cfg.GetAccountByName("any")
	if err != ErrNoAccounts {
		t.Errorf("GetAccountByName() error = %v, want %v", err, ErrNoAccounts)
	}
}

func TestGetSignature(t *testing.T) {
	cfg := &Config{
		Signatures: map[string]string{
			"personal": "Cheers",
			"work":     "Best regards",
		},
	}

	tests := []struct {
		name    string
		sigName string
		want    string
		wantErr error
	}{
		{
			name:    "found",
			sigName: "personal",
			want:    "Cheers",
			wantErr: nil,
		},
		{
			name:    "not found",
			sigName: "unknown",
			want:    "",
			wantErr: ErrSignatureNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cfg.GetSignature(tt.sigName)
			if err != tt.wantErr {
				t.Errorf("GetSignature() error = %v, want %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("GetSignature() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetSignatureNilMap(t *testing.T) {
	cfg := &Config{Signatures: nil}
	_, err := cfg.GetSignature("any")
	if err != ErrSignatureNotFound {
		t.Errorf("GetSignature() error = %v, want %v", err, ErrSignatureNotFound)
	}
}

func TestHasAccounts(t *testing.T) {
	tests := []struct {
		name     string
		accounts []AccountConfig
		want     bool
	}{
		{
			name:     "no accounts",
			accounts: []AccountConfig{},
			want:     false,
		},
		{
			name:     "has accounts",
			accounts: []AccountConfig{{Name: "Test"}},
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Accounts: tt.accounts}
			if got := cfg.HasAccounts(); got != tt.want {
				t.Errorf("HasAccounts() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAccountCount(t *testing.T) {
	tests := []struct {
		name     string
		accounts []AccountConfig
		want     int
	}{
		{
			name:     "empty",
			accounts: []AccountConfig{},
			want:     0,
		},
		{
			name:     "one account",
			accounts: []AccountConfig{{Name: "A"}},
			want:     1,
		},
		{
			name:     "multiple accounts",
			accounts: []AccountConfig{{Name: "A"}, {Name: "B"}, {Name: "C"}},
			want:     3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Accounts: tt.accounts}
			if got := cfg.AccountCount(); got != tt.want {
				t.Errorf("AccountCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		input   string
		want    int64
		wantErr bool
	}{
		// Valid cases
		{"1B", 1, false},
		{"100B", 100, false},
		{"1KB", 1024, false},
		{"10KB", 10 * 1024, false},
		{"1MB", 1024 * 1024, false},
		{"25MB", 25 * 1024 * 1024, false},
		{"1GB", 1024 * 1024 * 1024, false},
		// Case insensitive
		{"25mb", 25 * 1024 * 1024, false},
		{"25Mb", 25 * 1024 * 1024, false},
		{"1kb", 1024, false},
		// With whitespace
		{"  25MB  ", 25 * 1024 * 1024, false},
		{"25 MB", 25 * 1024 * 1024, false},
		// Decimal values
		{"1.5MB", int64(1.5 * 1024 * 1024), false},
		{"2.5GB", int64(2.5 * 1024 * 1024 * 1024), false},
		// Without unit (defaults to bytes)
		{"1024", 1024, false},
		// Invalid cases
		{"", 0, true},
		{"MB", 0, true},
		{"abc", 0, true},
		{"-1MB", 0, true},
		{"25TB", 0, true}, // TB not supported
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseSize(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseSize(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ParseSize(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestFormatSize(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{0, "0B"},
		{500, "500B"},
		{1024, "1.0KB"},
		{1536, "1.5KB"},
		{1024 * 1024, "1.0MB"},
		{25 * 1024 * 1024, "25.0MB"},
		{1024 * 1024 * 1024, "1.0GB"},
		{int64(2.5 * 1024 * 1024 * 1024), "2.5GB"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := FormatSize(tt.input)
			if got != tt.want {
				t.Errorf("FormatSize(%d) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestGetMaxAttachmentSize(t *testing.T) {
	tests := []struct {
		name    string
		account AccountConfig
		want    int64
	}{
		{
			name:    "not configured - uses default",
			account: AccountConfig{SMTP: SMTPConfig{}},
			want:    DefaultMaxAttachmentSize,
		},
		{
			name:    "empty string - uses default",
			account: AccountConfig{SMTP: SMTPConfig{MaxAttachmentSize: ""}},
			want:    DefaultMaxAttachmentSize,
		},
		{
			name:    "invalid format - uses default",
			account: AccountConfig{SMTP: SMTPConfig{MaxAttachmentSize: "invalid"}},
			want:    DefaultMaxAttachmentSize,
		},
		{
			name:    "configured 20MB",
			account: AccountConfig{SMTP: SMTPConfig{MaxAttachmentSize: "20MB"}},
			want:    20 * 1024 * 1024,
		},
		{
			name:    "configured 1GB",
			account: AccountConfig{SMTP: SMTPConfig{MaxAttachmentSize: "1GB"}},
			want:    1024 * 1024 * 1024,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.account.GetMaxAttachmentSize()
			if got != tt.want {
				t.Errorf("GetMaxAttachmentSize() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestGetAccountByIdentifier(t *testing.T) {
	cfg := &Config{
		Accounts: []AccountConfig{
			{Name: "GMX", Email: "julian@gmx.de", Alias: "gmx"},
			{Name: "Habric", Email: "julian@habric.com", Alias: "habric"},
			{Name: "Schenker Capital", Email: "julian@sc.com", Alias: "sc"},
			{Name: "TUM", Email: "julian@tum.de"}, // No alias - should use name
		},
	}

	tests := []struct {
		name       string
		identifier string
		wantEmail  string
		wantErr    error
	}{
		// Email lookup
		{
			name:       "exact email match",
			identifier: "julian@gmx.de",
			wantEmail:  "julian@gmx.de",
		},
		{
			name:       "email with different case",
			identifier: "Julian@GMX.de",
			wantEmail:  "", // Email match is case-sensitive
			wantErr:    ErrAccountNotFound,
		},
		// Alias lookup
		{
			name:       "alias lowercase",
			identifier: "gmx",
			wantEmail:  "julian@gmx.de",
		},
		{
			name:       "alias uppercase",
			identifier: "GMX",
			wantEmail:  "julian@gmx.de",
		},
		{
			name:       "alias mixed case",
			identifier: "Habric",
			wantEmail:  "julian@habric.com",
		},
		{
			name:       "short alias",
			identifier: "sc",
			wantEmail:  "julian@sc.com",
		},
		// Name lookup (fallback)
		{
			name:       "name when no alias",
			identifier: "tum",
			wantEmail:  "julian@tum.de",
		},
		{
			name:       "name uppercase when no alias",
			identifier: "TUM",
			wantEmail:  "julian@tum.de",
		},
		// Not found
		{
			name:       "not found",
			identifier: "unknown",
			wantErr:    ErrAccountNotFound,
		},
		{
			name:       "empty identifier",
			identifier: "",
			wantErr:    ErrAccountNotFound,
		},
		{
			name:       "whitespace only",
			identifier: "   ",
			wantErr:    ErrAccountNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acc, err := cfg.GetAccountByIdentifier(tt.identifier)
			if tt.wantErr != nil {
				if err != tt.wantErr {
					t.Errorf("GetAccountByIdentifier(%q) error = %v, want %v", tt.identifier, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Errorf("GetAccountByIdentifier(%q) unexpected error: %v", tt.identifier, err)
				return
			}
			if acc.Email != tt.wantEmail {
				t.Errorf("GetAccountByIdentifier(%q).Email = %q, want %q", tt.identifier, acc.Email, tt.wantEmail)
			}
		})
	}
}

func TestGetAccountByIdentifierNoAccounts(t *testing.T) {
	cfg := &Config{Accounts: []AccountConfig{}}
	_, err := cfg.GetAccountByIdentifier("any")
	if err != ErrNoAccounts {
		t.Errorf("GetAccountByIdentifier() error = %v, want %v", err, ErrNoAccounts)
	}
}

func TestValidateAliases(t *testing.T) {
	tests := []struct {
		name     string
		accounts []AccountConfig
		wantErr  bool
		errMsg   string
	}{
		{
			name: "valid aliases",
			accounts: []AccountConfig{
				{Name: "GMX", Email: "a@test.com", Alias: "gmx"},
				{Name: "Habric", Email: "b@test.com", Alias: "habric"},
			},
			wantErr: false,
		},
		{
			name: "no aliases - valid",
			accounts: []AccountConfig{
				{Name: "GMX", Email: "a@test.com"},
				{Name: "Habric", Email: "b@test.com"},
			},
			wantErr: false,
		},
		{
			name: "duplicate aliases",
			accounts: []AccountConfig{
				{Name: "GMX", Email: "a@test.com", Alias: "mail"},
				{Name: "Habric", Email: "b@test.com", Alias: "mail"},
			},
			wantErr: true,
			errMsg:  "duplicate alias",
		},
		{
			name: "duplicate aliases different case",
			accounts: []AccountConfig{
				{Name: "GMX", Email: "a@test.com", Alias: "Mail"},
				{Name: "Habric", Email: "b@test.com", Alias: "mail"},
			},
			wantErr: true,
			errMsg:  "duplicate alias",
		},
		{
			name: "alias matches another email (lowercase)",
			accounts: []AccountConfig{
				{Name: "GMX", Email: "a@test.com", Alias: "btest"},
				{Name: "Habric", Email: "btest"},
			},
			wantErr: true,
			errMsg:  "conflicts with email",
		},
		{
			name: "invalid alias with spaces",
			accounts: []AccountConfig{
				{Name: "GMX", Email: "a@test.com", Alias: "my mail"},
			},
			wantErr: true,
			errMsg:  "invalid alias",
		},
		{
			name: "invalid alias with special chars",
			accounts: []AccountConfig{
				{Name: "GMX", Email: "a@test.com", Alias: "mail@work"},
			},
			wantErr: true,
			errMsg:  "invalid alias",
		},
		{
			name: "valid alias with dash and underscore",
			accounts: []AccountConfig{
				{Name: "GMX", Email: "a@test.com", Alias: "my-mail_1"},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Accounts: tt.accounts}
			err := cfg.ValidateAliases()
			if tt.wantErr {
				if err == nil {
					t.Errorf("ValidateAliases() expected error containing %q, got nil", tt.errMsg)
				} else if !containsString(err.Error(), tt.errMsg) {
					t.Errorf("ValidateAliases() error = %v, want error containing %q", err, tt.errMsg)
				}
			} else if err != nil {
				t.Errorf("ValidateAliases() unexpected error: %v", err)
			}
		})
	}
}

func TestGetAliasOrName(t *testing.T) {
	tests := []struct {
		name    string
		account AccountConfig
		want    string
	}{
		{
			name:    "has alias",
			account: AccountConfig{Name: "Schenker Capital", Alias: "sc"},
			want:    "sc",
		},
		{
			name:    "no alias - uses name",
			account: AccountConfig{Name: "TUM", Alias: ""},
			want:    "TUM",
		},
		{
			name:    "both empty",
			account: AccountConfig{Name: "", Alias: ""},
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.account.GetAliasOrName()
			if got != tt.want {
				t.Errorf("GetAliasOrName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestListAccountIdentifiers(t *testing.T) {
	cfg := &Config{
		Accounts: []AccountConfig{
			{Name: "GMX", Email: "a@gmx.de", Alias: "gmx"},
			{Name: "Habric", Email: "b@habric.com"}, // No alias
			{Email: "c@test.com"},                   // No name or alias
		},
	}

	identifiers := cfg.ListAccountIdentifiers()

	if len(identifiers) != 3 {
		t.Fatalf("ListAccountIdentifiers() returned %d items, want 3", len(identifiers))
	}

	// First should be alias
	if identifiers[0] != "gmx" {
		t.Errorf("identifiers[0] = %q, want %q", identifiers[0], "gmx")
	}

	// Second should be name (no alias)
	if identifiers[1] != "Habric" {
		t.Errorf("identifiers[1] = %q, want %q", identifiers[1], "Habric")
	}

	// Third should be email (no name or alias)
	if identifiers[2] != "c@test.com" {
		t.Errorf("identifiers[2] = %q, want %q", identifiers[2], "c@test.com")
	}
}

// Helper function
func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

func TestIsIMAPMailboxExcluded(t *testing.T) {
	tests := []struct {
		name     string
		mailbox  string
		expected bool
	}{
		// Exact matches (excluded)
		{"Junk exact", "Junk", true},
		{"Spam exact", "Spam", true},
		{"Trash exact", "Trash", true},
		{"Deleted exact", "Deleted", true},
		{"Deleted Items exact", "Deleted Items", true},
		{"Deleted Messages exact", "Deleted Messages", true},

		// Case insensitive
		{"junk lowercase", "junk", true},
		{"TRASH uppercase", "TRASH", true},
		{"deleted items mixed", "deleted items", true},

		// Prefix with word boundary (space)
		{"Deleted Items variant", "Deleted Elements", true},
		{"Junk Mail", "Junk Mail", true},
		{"Trash Items", "Trash Items", true},

		// Prefix with path separator
		{"Trash subfolder", "Trash/Old", true},
		{"Junk subfolder", "Junk/Reports", true},

		// NOT excluded — no word boundary
		{"DeletedArchive no boundary", "DeletedArchive", false},
		{"JunkYard no boundary", "JunkYard", false},
		{"TrashCan no boundary", "TrashCan", false},
		{"SpamFilter no boundary", "SpamFilter", false},

		// NOT excluded — different names
		{"INBOX", "INBOX", false},
		{"Sent", "Sent", false},
		{"Archive", "Archive", false},
		{"Drafts", "Drafts", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsIMAPMailboxExcluded(tt.mailbox)
			if got != tt.expected {
				t.Errorf("IsIMAPMailboxExcluded(%q) = %v, want %v", tt.mailbox, got, tt.expected)
			}
		})
	}
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
