package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/julion2/durian/cli/internal/config"
)

var profileTestAccounts = []config.AccountConfig{
	{Name: "Work", DisplayName: "Ada Lovelace", Email: "ada@work.example", Alias: "w", Default: true,
		SMTP: config.SMTPConfig{Host: "smtp.work.example"}, Auth: &config.AuthConfig{Username: "ada@work.example"}},
	{Name: "Home", Email: "ada@home.example", Alias: "h"},
}

func TestResolveProfiles(t *testing.T) {
	profiles := []config.ProfileConfig{
		{Name: "All", Accounts: []string{"*"}, Folders: []config.FolderConfig{{Name: "Inbox", Icon: "tray", Query: "tag:inbox"}}},
		// name, alias and address, in any case; the same account twice; one unknown
		{Name: "Mixed", Default: true, Color: "#3b82f6", Accounts: []string{"WORK", "h", "ada@work.example", " ", "old"}},
	}
	got := resolveProfiles(profiles, profileTestAccounts)

	all := got[0]
	if !all.AllAccounts || len(all.Accounts) != 2 || all.Accounts[0].Name != "Work" || all.Accounts[1].Name != "Home" {
		t.Errorf("All: %+v", all)
	}
	if len(all.Folders) != 1 || all.Folders[0] != (profileFolder{Name: "Inbox", Icon: "tray", Query: "tag:inbox"}) {
		t.Errorf("All folders: %+v", all.Folders)
	}

	mixed := got[1]
	var names []string
	for _, a := range mixed.Accounts {
		names = append(names, a.Name)
	}
	if strings.Join(names, ",") != "Work,Home,old" {
		t.Errorf("Mixed accounts = %v, want Work,Home,old (deduplicated, unknown kept)", names)
	}
	if mixed.Accounts[0].Email != "ada@work.example" || mixed.Accounts[0].DisplayName != "Ada Lovelace" || !mixed.Accounts[0].Default {
		t.Errorf("resolved account lost fields: %+v", mixed.Accounts[0])
	}
	if mixed.Accounts[2] != (profileAccount{Name: "old"}) {
		t.Errorf("unknown reference: %+v", mixed.Accounts[2])
	}
	if mixed.AllAccounts || !mixed.Default || mixed.Color != "#3b82f6" {
		t.Errorf("Mixed: %+v", mixed)
	}
	if mixed.Folders == nil {
		t.Error("folders must be an empty array, not null")
	}
}

func profilesRequest(t *testing.T, h *Handler) (int, map[string]json.RawMessage, string) {
	t.Helper()
	w := httptest.NewRecorder()
	newTestRouter(h, nil).ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/profiles", nil))
	var body map[string]json.RawMessage
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return w.Code, body, w.Body.String()
}

func TestProfilesHandler(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"profiles":[{"name":"Work","accounts":["w"],"default":true,"folders":[{"name":"Inbox","icon":"tray","query":"tag:inbox"}]}]}`)

	h := New(newTestStore(t), nil)
	h.SetProfilesPath(path)

	// no config: nothing to resolve against
	if code, _, _ := profilesRequest(t, h); code != http.StatusServiceUnavailable {
		t.Fatalf("without config: status %d, want 503", code)
	}

	h.SetConfig(&config.Config{Accounts: profileTestAccounts})
	code, body, raw := profilesRequest(t, h)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	var profiles []profileResponse
	if err := json.Unmarshal(body["profiles"], &profiles); err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].Accounts[0].Email != "ada@work.example" || profiles[0].Folders[0].Query != "tag:inbox" {
		t.Errorf("profiles = %+v", profiles)
	}
	var accounts []profileAccount
	if err := json.Unmarshal(body["accounts"], &accounts); err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 {
		t.Errorf("accounts = %+v", accounts)
	}
	// transport and credentials stay on the server
	for _, secret := range []string{"smtp.work.example", "username", "smtp", "imap", "oauth"} {
		if strings.Contains(strings.ToLower(raw), secret) {
			t.Errorf("response contains %q: %s", secret, raw)
		}
	}

	// an edit shows up without a restart
	write(`{"profiles":[{"name":"Home","accounts":["h"]}]}`)
	_, body, _ = profilesRequest(t, h)
	if err := json.Unmarshal(body["profiles"], &profiles); err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].Name != "Home" {
		t.Errorf("after edit: %+v", profiles)
	}

	// a broken file is a server error, without its details
	write(`{"profiles": [`)
	if code, _, raw := profilesRequest(t, h); code != http.StatusInternalServerError || strings.Contains(raw, "unexpected") {
		t.Errorf("broken file: %d %q", code, raw)
	}

	// no profiles.pkl: an empty list, the accounts still there
	h.SetProfilesPath(filepath.Join(dir, "missing.json"))
	code, body, _ = profilesRequest(t, h)
	if code != http.StatusOK || string(body["profiles"]) != "[]" {
		t.Errorf("missing file: %d %s", code, body["profiles"])
	}
}

func TestProfilesHandlerAccountPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	if err := os.WriteFile(path, []byte(`{"profiles":[{"name":"Alias","accounts":[" WORK "]},{"name":"Email","accounts":["SHARED@EXAMPLE.COM"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Accounts: []config.AccountConfig{
		{Name: "work", Email: "personal@example.com", Alias: "personal"},
		{Name: "shared@example.com", Email: "other@example.com"},
		{Name: "company", Email: "shared@example.com", Alias: "work"},
	}}
	if err := cfg.ValidateAliases(); err != nil {
		t.Fatal(err)
	}
	h := New(newTestStore(t), nil)
	h.SetConfig(cfg)
	h.SetProfilesPath(path)
	code, body, raw := profilesRequest(t, h)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	var profiles []profileResponse
	if err := json.Unmarshal(body["profiles"], &profiles); err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 {
		t.Fatalf("got %d profiles, want 2", len(profiles))
	}
	for _, p := range profiles {
		if len(p.Accounts) != 1 || p.Accounts[0].Name != "company" || p.Accounts[0].Email != "shared@example.com" {
			t.Errorf("profile %s: got accounts %+v, want company", p.Name, p.Accounts)
		}
	}
}
