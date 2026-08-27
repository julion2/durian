package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/redact"
	"github.com/julion2/durian/cli/internal/store"
	"github.com/spf13/cobra"
	"golang.org/x/term"
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
	noInput    bool
	configErr  error
)

// Global config (loaded at startup)
var cfg *config.Config

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "durian",
	Short: "Durian Mail CLI",
	Long:  `Durian is a macOS email client. CLI backend for provider mail sync, sending, and SQLite storage.`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if cmd == validateCmd {
			return nil
		}
		return configErr
	},
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
	rootCmd.PersistentFlags().BoolVar(&noInput, "no-input", false, "never prompt or open an editor")

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
	cfg, configErr = loadStartupConfig(cfgFile)
	if cfg == nil {
		cfg = config.Default()
	}
}

func loadStartupConfig(path string) (*config.Config, error) {
	if !config.Exists(path) {
		if path != "" {
			return nil, fmt.Errorf("config file not found: %s", config.ExpandPath(path))
		}
		// A missing default config is valid before initial setup.
		return config.Default(), nil
	}

	loaded, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	return loaded, nil
}

// GetConfig returns the loaded configuration
// This is useful for subcommands that need access to config
func GetConfig() *config.Config {
	return cfg
}

func writeJSON(value any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func canPrompt() bool {
	return !noInput && term.IsTerminal(int(os.Stdin.Fd()))
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
	configureStoreAccounts(db, GetConfig())
	return db, nil
}

func configureStoreAccounts(db *store.DB, cfg *config.Config) {
	if cfg == nil {
		return
	}
	aliases := make(map[string]string, len(cfg.Accounts)*3)
	for i := range cfg.Accounts {
		account := &cfg.Accounts[i]
		canonical := account.AccountIdentifier()
		for _, identifier := range []string{account.Name, account.Alias, account.Email} {
			identifier = strings.TrimSpace(identifier)
			if identifier != "" {
				aliases[strings.ToLower(identifier)] = canonical
			}
		}
	}
	db.SetAccountAliases(aliases)
}
