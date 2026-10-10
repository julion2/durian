package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func getSettings(t *testing.T, h *Handler) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	newTestRouter(h, nil).ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/settings", nil))
	if w.Code != http.StatusOK {
		return w.Code, nil
	}
	var body struct {
		OK       bool           `json:"ok"`
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	if !body.OK {
		t.Fatalf("ok = false: %s", w.Body.String())
	}
	return w.Code, body.Settings
}

func writeConfig(t *testing.T, path, settings string) {
	t.Helper()
	// an account and a tag sync key the response must not carry
	config := `{"settings": ` + settings + `, "sync": {"tag_sync": {"url": "http://nas:8724", "api_key": "secret-key"}},
		"accounts": [{"name": "Work", "email": "me@work.example", "auth": {"password_keychain": "work-account"}}]}`
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsHandler_ServesTheSettingsOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, path, `{"theme": "dark", "notifications_enabled": true, "load_remote_images": true, "accent_color": "#ff8800"}`)
	h := New(newTestStore(t), nil)
	h.SetConfigPath(path)

	code, got := getSettings(t, h)
	want := map[string]any{"theme": "dark", "notifications_enabled": true, "load_remote_images": true, "accent_color": "#ff8800"}
	if code != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Fatalf("GET /settings = %d %v, want %v", code, got, want)
	}
}

// The file is read per request: switching remote images on or off applies
// without restarting serve.
func TestSettingsHandler_FollowsEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfig(t, path, `{"theme": "system", "load_remote_images": false}`)
	h := New(newTestStore(t), nil)
	h.SetConfigPath(path)
	if _, got := getSettings(t, h); got["load_remote_images"] != false {
		t.Fatalf("load_remote_images = %v before the edit, want false", got["load_remote_images"])
	}

	writeConfig(t, path, `{"theme": "system", "load_remote_images": true}`)
	_, got := getSettings(t, h)
	if got["load_remote_images"] != true {
		t.Fatalf("load_remote_images = %v after the edit, want true", got["load_remote_images"])
	}
	if _, set := got["accent_color"]; set {
		t.Fatalf("accent_color = %v, want it omitted when unset", got["accent_color"])
	}
}

func TestSettingsHandler_BrokenConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"settings": `), 0o600); err != nil {
		t.Fatal(err)
	}
	h := New(newTestStore(t), nil)
	h.SetConfigPath(path)
	if code, _ := getSettings(t, h); code != http.StatusInternalServerError {
		t.Fatalf("GET /settings with a broken config = %d, want 500", code)
	}
}
