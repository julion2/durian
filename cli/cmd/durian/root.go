package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/durian-dev/durian/cli/internal/config"
	"github.com/durian-dev/durian/cli/internal/redact"
	"github.com/durian-dev/durian/cli/internal/store"
	"github.com/spf13/cobra"
)

// Set via Bazel x_defs (workspace status stamping)
var (
	version   = "dev"
	gitCommit = ""
	gitDirty  = ""
)

// Global flags
var (
	cfgFile    string
	jsonOutput bool
	debugMode  bool
)

// Global config (loaded at startup)
var cfg *config.Config

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "durian",
	Short: "Durian Mail CLI",
	Long:  `Durian is a macOS email client. CLI backend for IMAP sync, SMTP send, and SQLite storage.`,
	// Show help when called without subcommands
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Help()
	},
}

// Execute runs the root command
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.Version = formatVersion()

	// Global flags available to all commands
	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "", "config file (default: ~/.config/durian/config.pkl)")
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "output as JSON")
	rootCmd.PersistentFlags().BoolVar(&debugMode, "debug", false, "enable debug logging")

	installColoredHelp(rootCmd)

	// Load config before command execution
	cobra.OnInitialize(initConfig, initLogger)
}

func formatVersion() string {
	v := version
	if gitDirty == "true" {
		v += "-dirty"
	}
	if gitCommit != "" {
		v += " (" + gitCommit + ")"
	}
	return v
}

// initConfig loads configuration from file
func initConfig() {
	var err error

	// Try to load config from specified path or default
	if config.Exists(cfgFile) {
		cfg, err = config.Load(cfgFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to load config: %v\n", err)
			cfg = config.Default()
		}
	} else {
		// No config file found - use defaults
		if cfgFile != "" {
			// User specified a path but file doesn't exist
			fmt.Fprintf(os.Stderr, "Warning: config file not found: %s\n", cfgFile)
		}
		// Silently use defaults if no custom path specified
		// (most users won't have config initially)
		cfg = config.Default()
	}
}

// GetConfig returns the loaded configuration
// This is useful for subcommands that need access to config
func GetConfig() *config.Config {
	return cfg
}

// initLogger configures the default slog logger.
// Non-serve commands: Error on stderr (or Debug with --debug).
// The serve command overrides this to write to serve.log.
func initLogger() {
	level := slog.LevelError
	if debugMode {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(redact.Wrap(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))))
}

// openEmailDB opens and initializes the shared SQLite email store, with
// the ADR-0001 keyring bootstrapped from the OS keychain. Used by the
// one-shot CLI commands; serve has its own copy of this bootstrap.
func openEmailDB() (*store.DB, error) {
	keyring := bootstrapKeyring()
	db, err := store.Open(store.DefaultDBPath(), keyring)
	if err != nil {
		return nil, fmt.Errorf("open email store: %w", err)
	}
	if err := db.Init(); err != nil {
		db.Close()
		return nil, fmt.Errorf("init email store: %w", err)
	}
	return db, nil
}
