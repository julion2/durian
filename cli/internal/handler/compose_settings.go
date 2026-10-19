package handler

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/julion2/durian/cli/internal/config"
)

// ComposeSettingsHandler exposes only named signatures and account defaults.
// Like /settings, it follows edits to the config serve actually runs with.
// Account IDs match the client's lowercased /profiles names, never aliases.
func (h *Handler) ComposeSettingsHandler(w http.ResponseWriter, _ *http.Request) {
	cfg, err := config.Load(h.configPath)
	if err != nil {
		slog.Error("Failed to load compose settings", "module", "API", "err", err)
		http.Error(w, "failed to load compose settings", http.StatusInternalServerError)
		return
	}
	signatures := make(map[string]string, len(cfg.Signatures))
	for name, content := range cfg.Signatures {
		signatures[name] = content
	}
	defaults := make(map[string]string)
	for _, account := range cfg.Accounts {
		if account.DefaultSignature != "" {
			defaults[strings.ToLower(account.Name)] = account.DefaultSignature
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"ok": true, "signatures": signatures, "account_defaults": defaults})
}
