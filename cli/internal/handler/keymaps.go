package handler

import (
	"log/slog"
	"net/http"

	"github.com/julion2/durian/cli/internal/config"
)

// SetKeymapsPath sets where keymaps.pkl is read from: next to the config
// file serve runs with. Empty means the default config directory.
func (h *Handler) SetKeymapsPath(path string) {
	h.keymapsPath = path
}

// KeymapsHandler serves keymaps.pkl, read-only, so clients need neither the
// pkl CLI nor the schema to follow the user's bindings. The bindings are the
// user's own, as written: a client merges them with its built-in defaults.
// Omitted fields carry the schema defaults (enabled and keymaps_enabled true,
// sequence_timeout 1.0, context "list"). "config" is null when the file does
// not exist. The file is read per request; config.LoadKeymaps caches its
// evaluation by content, so an edit shows up on the next request without a
// restart.
//
// A file that fails to evaluate is a 500; one that evaluates but has
// error-severity validation issues (what `durian validate keymaps` rejects,
// e.g. an unknown modifier or context in an untyped file) is a 422. Neither
// response carries details; they go to the log. Warnings (unknown actions,
// duplicate bindings) do not fail the request.
func (h *Handler) KeymapsHandler(w http.ResponseWriter, _ *http.Request) {
	keymaps, err := config.LoadKeymaps(h.keymapsPath)
	if err != nil {
		slog.Error("Failed to load keymaps", "module", "API", "err", err)
		http.Error(w, "failed to load keymaps", http.StatusInternalServerError)
		return
	}
	if keymaps != nil {
		invalid := 0
		for _, issue := range config.ValidateKeymaps(keymaps) {
			if issue.Severity == "error" {
				invalid++
				slog.Error("Invalid keymaps", "module", "API", "issue", issue.String())
			}
		}
		if invalid > 0 {
			http.Error(w, "invalid keymaps (run durian validate keymaps)", http.StatusUnprocessableEntity)
			return
		}
	}
	writeJSON(w, map[string]any{
		"ok":     true,
		"config": keymaps,
	})
}
