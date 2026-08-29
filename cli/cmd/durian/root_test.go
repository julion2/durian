package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestJSONCapableCommandsAllExist keeps the allowlist honest. It is keyed by
// command path, so a rename or a typo does not fail loudly — the entry simply
// stops matching, and the command quietly loses JSON support. That surfaces
// only when a user passes --json to a command that does emit JSON and is told
// the flag is unsupported.
func TestJSONCapableCommandsAllExist(t *testing.T) {
	paths := map[string]bool{}
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		paths[cmd.CommandPath()] = true
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(rootCmd)

	for listed := range jsonCapableCommands {
		if !paths[listed] {
			t.Errorf("jsonCapableCommands names %q, which is not a registered command — that command rejects --json", listed)
		}
	}
}

func TestNoInputDisablesPrompts(t *testing.T) {
	previous := noInput
	noInput = true
	t.Cleanup(func() { noInput = previous })
	if canPrompt() {
		t.Fatal("prompting enabled with --no-input")
	}
}

func TestConceptualHelpTopicsAreDiscoverable(t *testing.T) {
	for _, name := range []string{"query", "identifiers", "accounts", "output", "calendar-time"} {
		cmd, _, err := rootCmd.Find([]string{name})
		if err != nil || cmd.Name() != name {
			t.Errorf("help topic %q not found: command=%v err=%v", name, cmd, err)
			continue
		}
		if !cmd.IsAdditionalHelpTopicCommand() {
			t.Errorf("%q is not classified as an additional help topic", name)
		}
	}
}

func TestJSONSupportIsExplicit(t *testing.T) {
	if !commandSupportsJSON(searchCmd) || !commandSupportsJSON(calendarNewCmd) {
		t.Fatal("JSON command reported unsupported")
	}
	if commandSupportsJSON(sendCmd) || commandSupportsJSON(validateCmd) {
		t.Fatal("text-only command reported JSON support")
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
