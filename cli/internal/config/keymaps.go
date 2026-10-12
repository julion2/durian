package config

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/apple/pkl-go/pkl"
)

// Defaults applied to fields a keymaps.pkl leaves out. They mirror the Pkl
// schema (schema/Keymaps.pkl) and the GUI decoder (KeymapTypes.swift /
// KeymapsManager.swift), so an omitted field never reads as false/zero.
const (
	DefaultKeymapContext         = "list"
	DefaultKeymapSequenceTimeout = 1.0
)

// KeymapConfig represents the full keymaps configuration: the user's own
// bindings exactly as written (not merged with any client's built-in
// defaults) plus the global settings, with omitted fields defaulted.
type KeymapConfig struct {
	Keymaps        []KeymapEntry        `json:"keymaps"`
	GlobalSettings KeymapGlobalSettings `json:"global_settings"`
}

// KeymapEntry represents a single keymap binding.
type KeymapEntry struct {
	Action        string   `json:"action"`
	Key           string   `json:"key"`
	Modifiers     []string `json:"modifiers"`
	Enabled       bool     `json:"enabled"`
	Sequence      bool     `json:"sequence"`
	SupportsCount bool     `json:"supports_count"`
	Context       string   `json:"context"`
	// Tags is the tag_op operation ("+todo -inbox"); omitted when unset, as
	// Pkl renders an unset String?.
	Tags string `json:"tags,omitempty"`
}

// KeymapGlobalSettings contains global keymap preferences.
type KeymapGlobalSettings struct {
	KeymapsEnabled  bool    `json:"keymaps_enabled"`
	ShowKeymapHints bool    `json:"show_keymap_hints"`
	SequenceTimeout float64 `json:"sequence_timeout"`
}

// LoadKeymaps loads and parses keymaps.pkl from the given path.
// If path is empty, uses the default config directory.
// Returns nil (not error) if the file doesn't exist.
//
// Both file styles users write are accepted: a module typed against
// modulepath:/Keymaps.pkl (the schema fills defaults) and an untyped one
// (`keymaps { new { ... } }`, as in docs/keymaps-example.pkl) whose omitted
// fields are defaulted here exactly as the GUI's decoder does. The result is
// cached by file content, so an edit is seen on the next call.
func LoadKeymaps(path string) (*KeymapConfig, error) {
	if path == "" {
		path = filepath.Join(filepath.Dir(DefaultPath()), "keymaps.pkl")
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}

	var cfg KeymapConfig
	if err := cachedLoad(path, &cfg, func() error { return loadKeymapsInto(path, &cfg) }); err != nil {
		return nil, fmt.Errorf("failed to load keymaps: %w", err)
	}

	return &cfg, nil
}

// loadKeymapsInto renders the file to JSON (the .json test path is read
// as-is) and decodes it with defaults for omitted fields.
func loadKeymapsInto(path string, out *KeymapConfig) error {
	var data []byte
	if strings.HasSuffix(path, ".json") {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		data = b
	} else {
		text, err := pklEvalJSON(path)
		if err != nil {
			return err
		}
		data = []byte(text)
	}
	cfg, err := decodeKeymapsJSON(data)
	if err != nil {
		return err
	}
	*out = *cfg
	return nil
}

// pklEvalJSON evaluates a .pkl file to JSON text, the same rendering the GUI
// gets from `pkl eval --format json`. Unlike decoding straight into a Go
// struct, this works for untyped (Dynamic) modules too.
func pklEvalJSON(path string) (string, error) {
	subFS, err := fs.Sub(schemaFS, "schema")
	if err != nil {
		return "", err
	}
	ctx := context.Background()
	evaluator, err := pkl.NewEvaluator(ctx,
		pkl.PreconfiguredOptions,
		pkl.WithFs(subFS, "modulepath"),
		func(o *pkl.EvaluatorOptions) { o.OutputFormat = "json" },
	)
	if err != nil {
		return "", err
	}
	defer evaluator.Close()
	return evaluator.EvaluateOutputText(ctx, pkl.FileSource(path))
}

// Wire shapes for decoding: pointers distinguish "omitted" (or null) from an
// explicit false/zero.
type keymapsFileJSON struct {
	Keymaps        []keymapEntryJSON   `json:"keymaps"`
	GlobalSettings *globalSettingsJSON `json:"global_settings"`
}

type keymapEntryJSON struct {
	Action        string   `json:"action"`
	Key           string   `json:"key"`
	Modifiers     []string `json:"modifiers"`
	Enabled       *bool    `json:"enabled"`
	Sequence      *bool    `json:"sequence"`
	SupportsCount *bool    `json:"supports_count"`
	Context       *string  `json:"context"`
	Tags          *string  `json:"tags"`
}

type globalSettingsJSON struct {
	KeymapsEnabled  *bool    `json:"keymaps_enabled"`
	ShowKeymapHints *bool    `json:"show_keymap_hints"`
	SequenceTimeout *float64 `json:"sequence_timeout"`
}

// decodeKeymapsJSON decodes rendered keymaps JSON, defaulting omitted fields
// like the schema and the GUI: enabled and keymaps_enabled true, sequence and
// supports_count false, context "list", sequence_timeout 1.0, modifiers [].
// Explicit values (including false and 0) are kept as written.
func decodeKeymapsJSON(data []byte) (*KeymapConfig, error) {
	var raw keymapsFileJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	cfg := &KeymapConfig{
		Keymaps: make([]KeymapEntry, 0, len(raw.Keymaps)),
		GlobalSettings: KeymapGlobalSettings{
			KeymapsEnabled:  true,
			SequenceTimeout: DefaultKeymapSequenceTimeout,
		},
	}
	if gs := raw.GlobalSettings; gs != nil {
		if gs.KeymapsEnabled != nil {
			cfg.GlobalSettings.KeymapsEnabled = *gs.KeymapsEnabled
		}
		if gs.ShowKeymapHints != nil {
			cfg.GlobalSettings.ShowKeymapHints = *gs.ShowKeymapHints
		}
		if gs.SequenceTimeout != nil {
			cfg.GlobalSettings.SequenceTimeout = *gs.SequenceTimeout
		}
	}
	for _, e := range raw.Keymaps {
		entry := KeymapEntry{
			Action:    e.Action,
			Key:       e.Key,
			Modifiers: e.Modifiers,
			Enabled:   true,
			Context:   DefaultKeymapContext,
		}
		if entry.Modifiers == nil {
			entry.Modifiers = []string{}
		}
		if e.Enabled != nil {
			entry.Enabled = *e.Enabled
		}
		if e.Sequence != nil {
			entry.Sequence = *e.Sequence
		}
		if e.SupportsCount != nil {
			entry.SupportsCount = *e.SupportsCount
		}
		if e.Context != nil {
			entry.Context = *e.Context
		}
		if e.Tags != nil {
			entry.Tags = *e.Tags
		}
		cfg.Keymaps = append(cfg.Keymaps, entry)
	}
	return cfg, nil
}
