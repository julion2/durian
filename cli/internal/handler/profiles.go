package handler

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/julion2/durian/cli/internal/config"
)

// profileAccount is the public face of a configured account: what a client
// needs to show it and to scope searches to it (path:<name>/**). Transport
// and credential settings never leave the server.
type profileAccount struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
	Alias       string `json:"alias,omitempty"`
	Default     bool   `json:"default,omitempty"`
}

type profileFolder struct {
	Name  string `json:"name"`
	Icon  string `json:"icon,omitempty"`
	Query string `json:"query"`
}

type profileResponse struct {
	Name    string `json:"name"`
	Default bool   `json:"default,omitempty"`
	Color   string `json:"color,omitempty"`
	// AllAccounts is true for a profile written as accounts = { "*" }.
	AllAccounts bool             `json:"all_accounts,omitempty"`
	Accounts    []profileAccount `json:"accounts"`
	Folders     []profileFolder  `json:"folders"`
}

// SetProfilesPath sets where profiles.pkl is read from: next to the config
// file serve runs with. Empty means the default config directory.
func (h *Handler) SetProfilesPath(path string) {
	h.profilesPath = path
}

// ProfilesHandler serves profiles.pkl with its account references resolved,
// so clients need neither the pkl CLI nor the schema to show profiles. The
// file is read per request; config.LoadProfiles caches its evaluation by
// content, so an edit shows up on the next request without a restart.
func (h *Handler) ProfilesHandler(w http.ResponseWriter, _ *http.Request) {
	if h.cfg == nil {
		http.Error(w, "config not loaded", http.StatusServiceUnavailable)
		return
	}
	profiles, err := config.LoadProfiles(h.profilesPath)
	if err != nil {
		slog.Error("Failed to load profiles", "module", "API", "err", err)
		http.Error(w, "failed to load profiles", http.StatusInternalServerError)
		return
	}
	accounts := make([]profileAccount, 0, len(h.cfg.Accounts))
	for i := range h.cfg.Accounts {
		accounts = append(accounts, publicAccount(&h.cfg.Accounts[i]))
	}
	writeJSON(w, map[string]any{
		"ok":       true,
		"profiles": resolveProfiles(profiles, h.cfg.Accounts),
		"accounts": accounts,
	})
}

func publicAccount(a *config.AccountConfig) profileAccount {
	return profileAccount{Name: a.Name, DisplayName: a.DisplayName, Email: a.Email, Alias: a.Alias, Default: a.Default}
}

// resolveProfiles turns each profile's account references into accounts. A
// reference may be an account's name, alias or address (as anywhere in the
// config), case-insensitively; "*" means every account. A reference that
// matches no account is kept by name only, so a client can still scope by it
// and show that it's unknown.
func resolveProfiles(profiles []config.ProfileConfig, accounts []config.AccountConfig) []profileResponse {
	find := func(ref string) (*config.AccountConfig, bool) {
		for i := range accounts {
			a := &accounts[i]
			for _, key := range []string{a.Name, a.Alias, a.Email} {
				if key != "" && strings.EqualFold(key, ref) {
					return a, true
				}
			}
		}
		return nil, false
	}
	out := make([]profileResponse, 0, len(profiles))
	for _, p := range profiles {
		resp := profileResponse{
			Name:     p.Name,
			Default:  p.Default,
			Color:    p.Color,
			Accounts: []profileAccount{},
			Folders:  make([]profileFolder, 0, len(p.Folders)),
		}
		seen := map[string]bool{}
		add := func(a profileAccount) {
			if key := strings.ToLower(a.Name); !seen[key] {
				seen[key] = true
				resp.Accounts = append(resp.Accounts, a)
			}
		}
		for _, ref := range p.Accounts {
			ref = strings.TrimSpace(ref)
			switch ref {
			case "*":
				resp.AllAccounts = true
				for i := range accounts {
					add(publicAccount(&accounts[i]))
				}
			case "":
			default:
				if a, ok := find(ref); ok {
					add(publicAccount(a))
				} else {
					add(profileAccount{Name: ref})
				}
			}
		}
		for _, f := range p.Folders {
			resp.Folders = append(resp.Folders, profileFolder(f))
		}
		out = append(out, resp)
	}
	return out
}
