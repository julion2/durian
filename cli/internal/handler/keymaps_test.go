package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/julion2/durian/cli/internal/config"
)

func keymapsRequest(t *testing.T, h *Handler) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	newTestRouter(h, nil).ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/keymaps", nil))
	return w.Code, w.Body.String()
}

// decodeKeymapsBody decodes a 200 body generically, so the test checks the
// wire contract (field names, presence) rather than the Go structs.
func decodeKeymapsBody(t *testing.T, raw string) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	if body["ok"] != true {
		t.Fatalf("ok = %v, want true: %s", body["ok"], raw)
	}
	if _, present := body["config"]; !present {
		t.Fatalf("config key missing: %s", raw)
	}
	return body
}

func TestKeymapsHandler_Contract(t *testing.T) {
	// pkl-go needs $HOME, which Bazel's sandbox does not set.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "keymaps.pkl")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	h := New(newTestStore(t), nil)
	// Credentials in config.pkl must never reach this endpoint.
	h.SetConfig(&config.Config{Accounts: profileTestAccounts})
	h.SetKeymapsPath(path)

	// Absent file: ok with config null (the client uses its defaults).
	code, raw := keymapsRequest(t, h)
	if code != http.StatusOK {
		t.Fatalf("absent: status %d: %s", code, raw)
	}
	if body := decodeKeymapsBody(t, raw); body["config"] != nil {
		t.Fatalf("absent: config = %v, want null", body["config"])
	}

	// Omitted fields carry the defaults; explicit false stays false; the
	// user's bindings come back raw, not merged with any built-in set.
	write(`
keymaps {
  new { action = "archive"; key = "e" }
  new { action = "delete"; key = "dd"; sequence = true; enabled = false }
  new { action = "calendar_next_period"; key = "]"; modifiers { "alt" }; context = "calendar" }
}
`)
	code, raw = keymapsRequest(t, h)
	if code != http.StatusOK {
		t.Fatalf("defaults: status %d: %s", code, raw)
	}
	want := map[string]any{
		"keymaps": []any{
			map[string]any{"action": "archive", "key": "e", "modifiers": []any{}, "enabled": true,
				"sequence": false, "supports_count": false, "context": "list"},
			map[string]any{"action": "delete", "key": "dd", "modifiers": []any{}, "enabled": false,
				"sequence": true, "supports_count": false, "context": "list"},
			map[string]any{"action": "calendar_next_period", "key": "]", "modifiers": []any{"alt"}, "enabled": true,
				"sequence": false, "supports_count": false, "context": "calendar"},
		},
		"global_settings": map[string]any{"keymaps_enabled": true, "show_keymap_hints": false, "sequence_timeout": 1.0},
	}
	if got := decodeKeymapsBody(t, raw)["config"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults:\n got  %v\n want %v", got, want)
	}
	for _, secret := range []string{"ada@work.example", "smtp", "username", "accounts"} {
		if strings.Contains(raw, secret) {
			t.Errorf("response contains %q: %s", secret, raw)
		}
	}

	// Modified file: the edit shows on the next request (cache is by content),
	// including an explicit global keymaps_enabled = false.
	write(`
global_settings { keymaps_enabled = false; sequence_timeout = 0.4 }
keymaps { new { action = "archive"; key = "y"; tags = "+done" } }
`)
	code, raw = keymapsRequest(t, h)
	if code != http.StatusOK {
		t.Fatalf("modified: status %d: %s", code, raw)
	}
	cfg := decodeKeymapsBody(t, raw)["config"].(map[string]any)
	gs := cfg["global_settings"].(map[string]any)
	if gs["keymaps_enabled"] != false || gs["sequence_timeout"] != 0.4 {
		t.Errorf("modified: global_settings = %v", gs)
	}
	entries := cfg["keymaps"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["key"] != "y" || entries[0].(map[string]any)["tags"] != "+done" {
		t.Errorf("modified: keymaps = %v", entries)
	}

	// Malformed file: a sanitized 500, not the Pkl error text.
	write(`keymaps { new { action = "archive"; key = `)
	code, raw = keymapsRequest(t, h)
	if code != http.StatusInternalServerError || strings.TrimSpace(raw) != "failed to load keymaps" {
		t.Errorf("malformed: %d %q", code, raw)
	}

	// Evaluates but fails validation (untyped file, a modifier the schema
	// rejects): a sanitized 422.
	write(`keymaps { new { action = "archive"; key = "e"; modifiers { "option" } } }`)
	code, raw = keymapsRequest(t, h)
	if code != http.StatusUnprocessableEntity || strings.Contains(raw, "option") {
		t.Errorf("invalid: %d %q", code, raw)
	}

	// An unknown action is only a warning: still served.
	write(`keymaps { new { action = "calendar_teleport"; key = "x"; context = "calendar" } }`)
	if code, raw = keymapsRequest(t, h); code != http.StatusOK {
		t.Errorf("unknown action: %d %q", code, raw)
	}

	// Deleted file: back to null.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	code, raw = keymapsRequest(t, h)
	if code != http.StatusOK || decodeKeymapsBody(t, raw)["config"] != nil {
		t.Errorf("deleted: %d %s", code, raw)
	}
}

// Keymaps do not depend on config.pkl being loaded (unlike /profiles).
func TestKeymapsHandler_WithoutConfig(t *testing.T) {
	h := New(newTestStore(t), nil)
	h.SetKeymapsPath(filepath.Join(t.TempDir(), "keymaps.pkl"))
	if code, raw := keymapsRequest(t, h); code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
}
