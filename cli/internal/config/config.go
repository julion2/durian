package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Load loads configuration from the given path
// If path is empty, uses default path resolution
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}

	path = ExpandPath(path)

	var cfg Config
	if err := cachedLoad(path, &cfg, func() error { return loadInto(path, &cfg) }); err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	// Validate aliases
	if err := cfg.ValidateAliases(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return &cfg, nil
}

// DefaultPath returns the default config path
// Respects XDG_CONFIG_HOME, falls back to ~/.config/durian/config.pkl
func DefaultPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "durian", "config.pkl")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	return filepath.Join(home, ".config", "durian", "config.pkl")
}

// DefaultDataDir returns the durian data directory.
// Respects XDG_DATA_HOME, falls back to ~/.local/share/durian
func DefaultDataDir() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "durian")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "durian")
}

// CalendarBaseDir resolves the vdir base directory for calendars: the override
// wins, then the configured Calendar.VdirPath, then the default data dir's
// "calendars" subdir. Shared by the CLI calendar commands and the HTTP API so
// both resolve the same location.
func CalendarBaseDir(cfg *Config, override string) string {
	if override != "" {
		return override
	}
	if base := ExpandPath(cfg.Calendar.VdirPath); base != "" {
		return base
	}
	return filepath.Join(DefaultDataDir(), "calendars")
}

// DefaultStateDir returns the durian state directory.
// Respects XDG_STATE_HOME, falls back to ~/.local/state/durian
func DefaultStateDir() string {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "durian")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "durian")
}

// ExpandPath expands ~ and environment variables in path
func ExpandPath(path string) string {
	if path == "" {
		return path
	}

	// Expand ~
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			path = filepath.Join(home, path[2:])
		}
	} else if path == "~" {
		home, err := os.UserHomeDir()
		if err == nil {
			path = home
		}
	}

	// Expand environment variables
	path = os.ExpandEnv(path)

	return filepath.Clean(path)
}

// Exists checks if config file exists at path
func Exists(path string) bool {
	if path == "" {
		path = DefaultPath()
	}
	path = ExpandPath(path)

	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	return !info.IsDir()
}

// Default returns an empty default config
func Default() *Config {
	return &Config{
		Signatures: map[string]string{},
		Accounts:   []AccountConfig{},
	}
}
