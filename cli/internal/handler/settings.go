package handler

import (
	"log/slog"
	"net/http"

	"github.com/julion2/durian/cli/internal/config"
)

// settingsResponse is the settings section of config.pkl as clients see it.
// Fields are copied one by one so that a setting added to the config later
// is served only once someone decides it belongs here.
type settingsResponse struct {
	Theme                string `json:"theme"`
	NotificationsEnabled bool   `json:"notifications_enabled"`
	LoadRemoteImages     bool   `json:"load_remote_images"`
	AccentColor          string `json:"accent_color,omitempty"`
}

// SetConfigPath sets the config file /settings reads: the one serve runs
// with. Empty means the default path.
func (h *Handler) SetConfigPath(path string) {
	h.configPath = path
}

// SettingsHandler serves the app settings of config.pkl, so clients follow
// them (load_remote_images above all) without the pkl CLI or the schema. The
// file is read per request; config.Load caches its evaluation by content, so
// an edit shows up on the next request without a restart.
func (h *Handler) SettingsHandler(w http.ResponseWriter, _ *http.Request) {
	cfg, err := config.Load(h.configPath)
	if err != nil {
		slog.Error("Failed to load settings", "module", "API", "err", err)
		http.Error(w, "failed to load settings", http.StatusInternalServerError)
		return
	}
	s := cfg.Settings
	writeJSON(w, map[string]any{
		"ok": true,
		"settings": settingsResponse{
			Theme:                s.Theme,
			NotificationsEnabled: s.NotificationsEnabled,
			LoadRemoteImages:     s.LoadRemoteImages,
			AccentColor:          s.AccentColor,
		},
	})
}
