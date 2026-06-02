package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// RulesConfig holds all user-defined filter rules
type RulesConfig struct {
	Rules []RuleConfig `pkl:"rules" json:"rules"`
}

// RuleConfig defines a single filter rule applied at sync time
type RuleConfig struct {
	Name        string   `pkl:"name" json:"name"`
	Match       string   `pkl:"match" json:"match"`
	AddTags     []string `pkl:"add_tags" json:"add_tags"`
	RemoveTags  []string `pkl:"remove_tags" json:"remove_tags"`
	Accounts    []string `pkl:"accounts" json:"accounts"`         // If set, only apply to these accounts (by alias)
	Exec        string   `pkl:"exec" json:"exec"`                 // Optional: external command to run (stdin=email JSON, stdout=tag ops JSON)
	ExecTimeout int      `pkl:"exec_timeout" json:"exec_timeout"` // Timeout in seconds (default: 10)
	AllowedTags []string `pkl:"allowed_tags" json:"allowed_tags"` // Optional: restrict exec output to these tags
}

// LoadRules loads filter rules from the given path.
// Returns an empty slice if the file doesn't exist.
func LoadRules(path string) ([]RuleConfig, error) {
	if path == "" {
		path = RulesPath()
	}
	path = ExpandPath(path)

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}

	var cfg RulesConfig
	if err := loadInto(path, &cfg); err != nil {
		return nil, fmt.Errorf("failed to load rules: %w", err)
	}

	return cfg.Rules, nil
}

// RulesPath returns the default rules config path
func RulesPath() string {
	return filepath.Join(filepath.Dir(DefaultPath()), "rules.pkl")
}
