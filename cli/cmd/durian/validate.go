package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/imap"
)

var validateCmd = &cobra.Command{
	Use:   "validate [config|rules|profiles|keymaps|groups]",
	Short: "Validate configuration files",
	Long: "Validate Durian config files. Pass a name to validate just one.",
	Example: `  durian validate
  durian validate config
  durian validate rules
  durian validate profiles
  durian validate groups`,
	Args: cobra.MaximumNArgs(1),
	RunE: runValidate,
}

func init() {
	rootCmd.AddCommand(validateCmd)
}

func runValidate(cmd *cobra.Command, args []string) error {
	target := ""
	if len(args) > 0 {
		target = strings.ToLower(args[0])
		switch target {
		case "config", "rules", "profiles", "keymaps", "groups":
		default:
			return fmt.Errorf("unknown target %q (valid: config, rules, profiles, keymaps, groups)", target)
		}
	}

	configDir := filepath.Dir(config.DefaultPath())
	hasErrors := false

	// Always load config first (needed as reference for other validations)
	var loadedCfg *config.Config
	configPath := config.DefaultPath()
	if cfgFile != "" {
		configPath = cfgFile
	}

	if config.Exists(configPath) {
		var err error
		loadedCfg, err = config.Load(configPath)
		if err != nil && (target == "" || target == "config") {
			printError("config.pkl", fmt.Sprintf("failed to parse: %v", err))
			hasErrors = true
			loadedCfg = config.Default()
		}
	} else {
		loadedCfg = config.Default()
		if target == "" || target == "config" {
			printSkipped("config.pkl", "not found")
		}
	}

	// Validate config.pkl
	if target == "" || target == "config" {
		if config.Exists(configPath) {
			errs := config.ValidateConfig(loadedCfg)
			if printResults("config.pkl", errs, configSummary(loadedCfg)) {
				hasErrors = true
			}
		}
	}

	// Validate rules.pkl
	if target == "" || target == "rules" {
		rulesPath := filepath.Join(configDir, "rules.pkl")
		rules, err := config.LoadRules("")
		if err != nil {
			printError("rules.pkl", fmt.Sprintf("failed to parse: %v", err))
			hasErrors = true
		} else if rules == nil {
			printSkipped("rules.pkl", "not found at "+rulesPath)
		} else {
			errs := config.ValidateRules(rules, loadedCfg, imap.ValidateRuleQuery)
			if printResults("rules.pkl", errs, fmt.Sprintf("%d rules", len(rules))) {
				hasErrors = true
			}
		}
	}

	// Validate profiles.pkl
	if target == "" || target == "profiles" {
		profiles, err := config.LoadProfiles("")
		if err != nil {
			printError("profiles.pkl", fmt.Sprintf("failed to parse: %v", err))
			hasErrors = true
		} else if profiles == nil {
			printSkipped("profiles.pkl", "not found (using defaults)")
		} else {
			errs := config.ValidateProfiles(profiles, loadedCfg)
			if printResults("profiles.pkl", errs, fmt.Sprintf("%d profiles", len(profiles))) {
				hasErrors = true
			}
		}
	}

	// Validate keymaps.pkl
	if target == "" || target == "keymaps" {
		keymaps, err := config.LoadKeymaps("")
		if err != nil {
			printError("keymaps.pkl", fmt.Sprintf("failed to parse: %v", err))
			hasErrors = true
		} else if keymaps == nil {
			printSkipped("keymaps.pkl", "not found (using defaults)")
		} else {
			errs := config.ValidateKeymaps(keymaps)
			if printResults("keymaps.pkl", errs, fmt.Sprintf("%d bindings", len(keymaps.Keymaps))) {
				hasErrors = true
			}
		}
	}

	// Validate groups.pkl
	if target == "" || target == "groups" {
		groups, err := config.LoadGroups("")
		if err != nil {
			printError("groups.pkl", fmt.Sprintf("failed to parse: %v", err))
			hasErrors = true
		} else if groups == nil {
			printSkipped("groups.pkl", "not found (optional)")
		} else {
			errs := config.ValidateGroups(groups)
			if printResults("groups.pkl", errs, fmt.Sprintf("%d groups", len(groups))) {
				hasErrors = true
			}
		}
	}

	if hasErrors {
		os.Exit(1)
	}
	return nil
}

func configSummary(cfg *config.Config) string {
	if len(cfg.Accounts) == 0 {
		return "no accounts"
	}
	names := make([]string, 0, len(cfg.Accounts))
	for _, a := range cfg.Accounts {
		names = append(names, a.Name)
	}
	return fmt.Sprintf("%d accounts (%s)", len(cfg.Accounts), strings.Join(names, ", "))
}

// printResults prints validation results and returns true if errors were found.
func printResults(file string, errs []config.ValidationError, summary string) bool {
	errors := 0
	warnings := 0
	for _, e := range errs {
		if e.Severity == "error" {
			errors++
		} else {
			warnings++
		}
	}

	if errors == 0 && warnings == 0 {
		fmt.Printf("  ✓ %s — %s\n", file, summary)
		return false
	}

	if errors > 0 {
		fmt.Printf("  ✗ %s — %s\n", file, summary)
	} else {
		fmt.Printf("  ~ %s — %s\n", file, summary)
	}
	for _, e := range errs {
		if e.Severity == "error" {
			fmt.Printf("    ✗ %s\n", e)
		} else {
			fmt.Printf("    ~ %s\n", e)
		}
	}
	return errors > 0
}

func printError(file, msg string) {
	fmt.Printf("  ✗ %s — %s\n", file, msg)
}

func printSkipped(file, msg string) {
	fmt.Printf("  - %s — %s\n", file, msg)
}
