package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// KeymapConfig represents the full keymaps configuration.
type KeymapConfig struct {
	Keymaps        []KeymapEntry        `pkl:"keymaps" json:"keymaps"`
	GlobalSettings KeymapGlobalSettings `pkl:"global_settings" json:"global_settings"`
}

// KeymapEntry represents a single keymap binding.
type KeymapEntry struct {
	Action        string   `pkl:"action" json:"action"`
	Key           string   `pkl:"key" json:"key"`
	Modifiers     []string `pkl:"modifiers" json:"modifiers"`
	Enabled       bool     `pkl:"enabled" json:"enabled"`
	Sequence      bool     `pkl:"sequence" json:"sequence"`
	SupportsCount bool     `pkl:"supports_count" json:"supports_count"`
	Context       string   `pkl:"context" json:"context"`
	Tags          string   `pkl:"tags" json:"tags"`
}

// KeymapGlobalSettings contains global keymap preferences.
type KeymapGlobalSettings struct {
	KeymapsEnabled  bool    `pkl:"keymaps_enabled" json:"keymaps_enabled"`
	ShowKeymapHints bool    `pkl:"show_keymap_hints" json:"show_keymap_hints"`
	SequenceTimeout float64 `pkl:"sequence_timeout" json:"sequence_timeout"`
}

// LoadKeymaps loads and parses keymaps.pkl from the given path.
// If path is empty, uses the default config directory.
// Returns nil (not error) if the file doesn't exist.
func LoadKeymaps(path string) (*KeymapConfig, error) {
	if path == "" {
		path = filepath.Join(filepath.Dir(DefaultPath()), "keymaps.pkl")
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}

	var cfg KeymapConfig
	if err := cachedLoad(path, &cfg, func() error { return loadInto(path, &cfg) }); err != nil {
		return nil, fmt.Errorf("failed to load keymaps: %w", err)
	}

	return &cfg, nil
}
