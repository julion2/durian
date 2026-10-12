package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeKeymapsFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadKeymaps_MissingFileIsNil(t *testing.T) {
	cfg, err := LoadKeymaps(filepath.Join(t.TempDir(), "keymaps.pkl"))
	if err != nil || cfg != nil {
		t.Fatalf("LoadKeymaps(missing) = %+v, %v; want nil, nil", cfg, err)
	}
}

// Omitted fields take the schema/GUI defaults; explicit false and zero stay.
func TestLoadKeymaps_DefaultsVersusExplicit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keymaps.json")
	writeKeymapsFile(t, path, `{"keymaps":[
		{"action":"archive","key":"e"},
		{"action":"delete","key":"dd","sequence":true,"enabled":false,"supports_count":true,
		 "context":"calendar","modifiers":["alt"],"tags":"+x"}
	]}`)
	cfg, err := LoadKeymaps(path)
	if err != nil {
		t.Fatal(err)
	}
	want := &KeymapConfig{
		Keymaps: []KeymapEntry{
			{Action: "archive", Key: "e", Modifiers: []string{}, Enabled: true, Context: "list"},
			{Action: "delete", Key: "dd", Modifiers: []string{"alt"}, Enabled: false, Sequence: true,
				SupportsCount: true, Context: "calendar", Tags: "+x"},
		},
		GlobalSettings: KeymapGlobalSettings{KeymapsEnabled: true, SequenceTimeout: 1.0},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("got  %+v\nwant %+v", cfg, want)
	}

	writeKeymapsFile(t, path, `{"global_settings":{"keymaps_enabled":false,"sequence_timeout":0.5}}`)
	cfg, err = LoadKeymaps(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GlobalSettings.KeymapsEnabled || cfg.GlobalSettings.SequenceTimeout != 0.5 {
		t.Errorf("explicit global settings lost: %+v", cfg.GlobalSettings)
	}
	if cfg.Keymaps == nil {
		t.Error("keymaps must be an empty list, not nil")
	}

	// A global_settings block that sets only one field keeps the other's default.
	writeKeymapsFile(t, path, `{"global_settings":{"sequence_timeout":2.0}}`)
	cfg, err = LoadKeymaps(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.GlobalSettings.KeymapsEnabled || cfg.GlobalSettings.SequenceTimeout != 2.0 {
		t.Errorf("partial global settings: %+v", cfg.GlobalSettings)
	}
}

// Real Pkl, both styles users write: the untyped style of
// docs/keymaps-example.pkl (Pkl omits unset fields) and a module typed against
// the embedded schema (the schema fills them). Both must give the same result.
func TestLoadKeymaps_RealPkl(t *testing.T) {
	t.Setenv(DisableCacheEnv, "1")
	dir := t.TempDir()

	untyped := filepath.Join(dir, "untyped.pkl")
	writeKeymapsFile(t, untyped, `
global_settings {
  keymaps_enabled = false
}
keymaps {
  new { action = "archive"; key = "e" }
  new { action = "delete"; key = "dd"; sequence = true; enabled = false }
  new { action = "calendar_today"; key = "t"; modifiers { "alt" }; context = "calendar" }
  new { action = "tag_op"; key = "T"; tags = "+todo -inbox" }
}
`)
	typed := filepath.Join(dir, "typed.pkl")
	writeKeymapsFile(t, typed, `
import "modulepath:/Keymaps.pkl" as K
global_settings: K.KeymapGlobalSettings = new { keymaps_enabled = false }
keymaps: Listing<K.KeymapEntry> = new {
  new { action = "archive"; key = "e" }
  new { action = "delete"; key = "dd"; sequence = true; enabled = false }
  new { action = "calendar_today"; key = "t"; modifiers { "alt" }; context = "calendar" }
  new { action = "tag_op"; key = "T"; tags = "+todo -inbox" }
}
`)
	want := &KeymapConfig{
		Keymaps: []KeymapEntry{
			{Action: "archive", Key: "e", Modifiers: []string{}, Enabled: true, Context: "list"},
			{Action: "delete", Key: "dd", Modifiers: []string{}, Enabled: false, Sequence: true, Context: "list"},
			{Action: "calendar_today", Key: "t", Modifiers: []string{"alt"}, Enabled: true, Context: "calendar"},
			{Action: "tag_op", Key: "T", Modifiers: []string{}, Enabled: true, Context: "list", Tags: "+todo -inbox"},
		},
		GlobalSettings: KeymapGlobalSettings{KeymapsEnabled: false, SequenceTimeout: 1.0},
	}
	for _, path := range []string{untyped, typed} {
		cfg, err := LoadKeymaps(path)
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(path), err)
		}
		if !reflect.DeepEqual(cfg, want) {
			t.Errorf("%s:\n got  %+v\n want %+v", filepath.Base(path), cfg, want)
		}
		if errs := ValidateKeymaps(cfg); len(errs) != 0 {
			t.Errorf("%s: unexpected validation issues %v", filepath.Base(path), errs)
		}
	}

	// The typed schema rejects a context or modifier outside its unions.
	writeKeymapsFile(t, typed, `
import "modulepath:/Keymaps.pkl" as K
keymaps: Listing<K.KeymapEntry> = new { new { action = "x"; key = "y"; modifiers { "option" } } }
`)
	if _, err := LoadKeymaps(typed); err == nil {
		t.Error("typed file with modifier \"option\" loaded, want a schema error")
	}
}

// With the content cache on, an edit must be seen by the next load and a
// broken edit must fail rather than serve the stale result.
func TestLoadKeymaps_CacheObservesEdits(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv(DisableCacheEnv, "")
	path := filepath.Join(t.TempDir(), "keymaps.pkl")

	writeKeymapsFile(t, path, `keymaps { new { action = "archive"; key = "e" } }`)
	for range 2 { // second load is a cache hit
		cfg, err := LoadKeymaps(path)
		if err != nil || len(cfg.Keymaps) != 1 || cfg.Keymaps[0].Key != "e" || !cfg.Keymaps[0].Enabled {
			t.Fatalf("first content: %+v, %v", cfg, err)
		}
	}

	writeKeymapsFile(t, path, `keymaps { new { action = "archive"; key = "y"; enabled = false } }`)
	cfg, err := LoadKeymaps(path)
	if err != nil || cfg.Keymaps[0].Key != "y" || cfg.Keymaps[0].Enabled {
		t.Fatalf("after edit: %+v, %v", cfg, err)
	}

	writeKeymapsFile(t, path, `keymaps { new { action = `)
	if cfg, err := LoadKeymaps(path); err == nil || !strings.Contains(err.Error(), "failed to load keymaps") {
		t.Fatalf("broken edit: %+v, %v; want an error", cfg, err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if cfg, err := LoadKeymaps(path); cfg != nil || err != nil {
		t.Fatalf("after delete: %+v, %v; want nil, nil", cfg, err)
	}
}
