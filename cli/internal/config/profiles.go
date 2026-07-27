package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// ProfileConfig represents a single profile entry.
type ProfileConfig struct {
	Name     string         `pkl:"name" json:"name"`
	Accounts []string       `pkl:"accounts" json:"accounts"`
	Default  bool           `pkl:"default" json:"default"`
	Color    string         `pkl:"color" json:"color"`
	Folders  []FolderConfig `pkl:"folders" json:"folders"`
}

// FolderConfig represents a folder entry within a profile.
type FolderConfig struct {
	Name  string `pkl:"name" json:"name"`
	Icon  string `pkl:"icon" json:"icon"`
	Query string `pkl:"query" json:"query"`
}

type profilesFile struct {
	Profiles []ProfileConfig `pkl:"profiles" json:"profiles"`
}

// LoadProfiles loads and parses profiles.pkl from the given path.
// If path is empty, uses the default config directory.
// Returns nil (not error) if the file doesn't exist.
func LoadProfiles(path string) ([]ProfileConfig, error) {
	if path == "" {
		path = filepath.Join(filepath.Dir(DefaultPath()), "profiles.pkl")
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}

	var cfg profilesFile
	if err := cachedLoad(path, &cfg, func() error { return loadInto(path, &cfg) }); err != nil {
		return nil, fmt.Errorf("failed to load profiles: %w", err)
	}

	return cfg.Profiles, nil
}
