package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoInputDisablesPrompts(t *testing.T) {
	previous := noInput
	noInput = true
	t.Cleanup(func() { noInput = previous })
	if canPrompt() {
		t.Fatal("prompting enabled with --no-input")
	}
}

func TestLoadStartupConfigRejectsMissingExplicitPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.pkl")
	loaded, err := loadStartupConfig(path)
	if err == nil {
		t.Fatal("loadStartupConfig() succeeded for missing explicit path")
	}
	if loaded != nil {
		t.Fatalf("config = %#v, want nil", loaded)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error = %q, want path %q", err, path)
	}
}

func TestLoadStartupConfigAllowsMissingDefault(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	loaded, err := loadStartupConfig("")
	if err != nil {
		t.Fatalf("loadStartupConfig() error = %v", err)
	}
	if loaded == nil {
		t.Fatal("config is nil")
	}
	if len(loaded.Accounts) != 0 {
		t.Fatalf("default accounts = %d, want 0", len(loaded.Accounts))
	}
}

func TestLoadStartupConfigRejectsInvalidExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"accounts":`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	loaded, err := loadStartupConfig(path)
	if err == nil {
		t.Fatal("loadStartupConfig() succeeded for invalid config")
	}
	if loaded != nil {
		t.Fatalf("config = %#v, want nil", loaded)
	}
}
