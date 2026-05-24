package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/durian-dev/durian/cli/internal/config"
	"github.com/durian-dev/durian/cli/internal/keychain"
	"github.com/durian-dev/durian/cli/internal/oauth"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

const (
	// PasswordKeychainService is the service name for password-based auth
	PasswordKeychainService = "durian-password"
)

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Manage authentication for email accounts",
	Long:  `Manage authentication (OAuth or password) for email accounts.`,
}

var authLoginCmd = &cobra.Command{
	Use:   "login <account>",
	Short: "Authenticate with an email account",
	Long: "Authenticate with an email account using OAuth or password.",
	Example: `  durian auth login gmail       # Use alias
  durian auth login work         # Use alias
  durian auth login you@company.com  # Use full email`,
	Args: cobra.ExactArgs(1),
	ValidArgsFunction: completeAccounts,
	RunE: runAuthLogin,
}

var authStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show authentication status for all accounts",
	Long:  `Display the authentication status for all configured accounts.`,
	RunE:  runAuthStatus,
}

var authLogoutCmd = &cobra.Command{
	Use:   "logout <account>",
	Short: "Remove credentials for an account",
	Long: "Remove stored OAuth tokens or passwords from the keychain.",
	Args: cobra.ExactArgs(1),
	ValidArgsFunction: completeAccounts,
	RunE: runAuthLogout,
}

var authRefreshCmd = &cobra.Command{
	Use:   "refresh <account>",
	Short: "Manually refresh OAuth token for an account",
	Long: "Force a token refresh (normally done automatically).",
	Args: cobra.ExactArgs(1),
	ValidArgsFunction: completeAccounts,
	RunE: runAuthRefresh,
}

func init() {
	rootCmd.AddCommand(authCmd)
	authCmd.AddCommand(authLoginCmd)
	authCmd.AddCommand(authStatusCmd)
	authCmd.AddCommand(authLogoutCmd)
	authCmd.AddCommand(authRefreshCmd)
}

func runAuthLogin(cmd *cobra.Command, args []string) error {
	identifier := args[0]

	// Get config
	cfg := GetConfig()
	if cfg == nil {
		return errors.New("no configuration loaded")
	}

	// Find account by email, alias, or name
	account, err := cfg.GetAccountByIdentifier(identifier)
	if err != nil {
		return fmt.Errorf("account not found: %s\nMake sure it's configured in your config.pkl\nAvailable accounts: %s",
			identifier, cfg.ListAccountIdentifiers())
	}

	// Determine auth type: OAuth or Password
	if account.OAuth != nil && account.OAuth.Provider != "" {
		return runOAuthLogin(account)
	}

	// Check if password auth is configured
	if account.SMTP.Auth == "password" || account.IMAP.Auth == "password" {
		return runPasswordLogin(account)
	}

	return fmt.Errorf("no authentication method configured for %s\nAdd [accounts.oauth] or set auth = \"password\" in config.pkl", account.Email)
}

// runOAuthLogin handles OAuth authentication
func runOAuthLogin(account *config.AccountConfig) error {
	authEmail := account.GetAuthEmail()

	// Shared mailbox: reuse existing token from delegating user
	if account.AuthEmail != "" {
		if token, err := oauth.LoadToken(authEmail); err == nil && !token.IsExpired() {
			fmt.Printf("✓ Reusing token from %s for shared mailbox %s\n", authEmail, account.Email) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
			fmt.Printf("✓ Token expires in %s\n", formatDuration(token.ExpiresIn()))
			return nil
		}
		// Token missing or expired — fall through to OAuth flow for the delegating user
	}

	// Get provider
	provider, err := oauth.GetProvider(account.OAuth.Provider, account.OAuth.Tenant)
	if err != nil {
		return err
	}

	fmt.Printf("Starting OAuth authentication for %s (%s)...\n\n", authEmail, account.OAuth.Provider) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys

	// Run OAuth flow (authenticate as the delegating user for shared mailboxes)
	token, err := oauth.Authenticate(provider, account.OAuth.ClientID, account.OAuth.ClientSecret, authEmail)
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	// Save token under the auth email (delegating user)
	if err := oauth.SaveToken(authEmail, token); err != nil {
		return fmt.Errorf("failed to save token: %w", err)
	}

	fmt.Printf("\n✓ Successfully authenticated with %s\n", account.OAuth.Provider) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
	fmt.Printf("✓ Token stored securely in Keychain\n")
	fmt.Printf("✓ Token expires in %s\n", formatDuration(token.ExpiresIn()))

	return nil
}

// runPasswordLogin handles password-based authentication
func runPasswordLogin(account *config.AccountConfig) error {
	fmt.Printf("Password authentication for %s\n\n", account.Email) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys

	// Prompt for password
	password, err := promptPassword("Enter password: ")
	if err != nil {
		return fmt.Errorf("failed to read password: %w", err)
	}

	if password == "" {
		return errors.New("password cannot be empty")
	}

	// Store password in keychain
	if err := keychain.SetPassword(PasswordKeychainService, account.Email, password); err != nil {
		return fmt.Errorf("failed to save password: %w", err)
	}

	fmt.Printf("\n✓ Password stored securely in Keychain\n")
	fmt.Printf("✓ Service: %s\n", PasswordKeychainService)
	fmt.Printf("✓ Account: %s\n", account.Email) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys

	return nil
}

// stdinPromptReader is a single bufio.Reader over os.Stdin shared across all
// non-TTY promptPassword calls. A fresh per-call bufio.Reader would
// over-read into its own buffer and the next prompt would see EOF — broken
// for any flow that prompts twice (e.g. passphrase confirmation).
var stdinPromptReader = bufio.NewReader(os.Stdin)

// promptPassword securely prompts for a password (hides input)
func promptPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)

	// Check if stdin is a terminal
	if term.IsTerminal(int(os.Stdin.Fd())) {
		// Read password without echo
		password, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr) // Print newline after password input
		if err != nil {
			return "", err
		}
		return string(password), nil
	}

	// Fallback for non-terminal (e.g., piped input). Use the shared reader
	// so successive prompts in one invocation each get their own line.
	password, err := stdinPromptReader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(password), nil
}

func runAuthStatus(cmd *cobra.Command, args []string) error {
	cfg := GetConfig()
	if cfg == nil {
		return errors.New("no configuration loaded")
	}

	if len(cfg.Accounts) == 0 {
		fmt.Println("No accounts configured.") // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		return nil
	}

	if jsonOutput {
		type accountStatus struct {
			Email    string `json:"email"`
			Alias    string `json:"alias,omitempty"`
			AuthType string `json:"auth_type"`
			Status   string `json:"status"`
		}
		var statuses []accountStatus
		for _, account := range cfg.Accounts {
			statuses = append(statuses, accountStatus{
				Email:    account.Email,
				Alias:    account.Alias,
				AuthType: getAuthType(&account),
				Status:   getAccountStatusShort(&account),
			})
		}
		return json.NewEncoder(os.Stdout).Encode(statuses)
	}

	fmt.Println("Authentication Status:")
	fmt.Println()

	for _, account := range cfg.Accounts {
		status := getAccountStatus(&account)
		// Show alias in parentheses if set
		emailDisplay := account.Email
		if account.Alias != "" {
			emailDisplay = fmt.Sprintf("%s (%s)", account.Email, account.Alias)
		}
		fmt.Printf("  %-40s  %-12s  %s\n", emailDisplay, getAuthType(&account), status) // encgrep:allow user-facing TUI lists user's own accounts
	}

	return nil
}

func runAuthLogout(cmd *cobra.Command, args []string) error {
	identifier := args[0]

	cfg := GetConfig()
	if cfg == nil {
		return errors.New("no configuration loaded")
	}

	// Find account to determine auth type
	account, err := cfg.GetAccountByIdentifier(identifier)
	if err != nil {
		// Account not in config, try to delete both types (using identifier as email)
		oauthDeleted := oauth.DeleteToken(identifier) == nil && keychain.Exists(oauth.KeychainService, identifier)
		pwDeleted := keychain.DeletePassword(PasswordKeychainService, identifier) == nil && keychain.Exists(PasswordKeychainService, identifier)

		if !oauthDeleted && !pwDeleted {
			fmt.Printf("No credentials found for %s\n", identifier)
			return nil
		}

		fmt.Printf("✓ Logged out from %s\n", identifier)
		fmt.Printf("✓ Credentials removed from Keychain\n")
		return nil
	}

	// Delete based on account type
	if account.OAuth != nil && account.OAuth.Provider != "" {
		// Shared mailbox: token belongs to the delegating user
		if account.AuthEmail != "" {
			fmt.Printf("Shared mailbox %s uses token from %s\n", account.Email, account.AuthEmail) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
			fmt.Printf("Run: durian auth logout %s\n", account.AuthEmail) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
			return nil
		}

		// OAuth account
		_, err := oauth.LoadToken(account.Email)
		if err != nil {
			if errors.Is(err, oauth.ErrTokenNotFound) {
				fmt.Printf("No token found for %s\n", account.Email) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
				return nil
			}
			return err
		}

		if err := oauth.DeleteToken(account.Email); err != nil {
			return fmt.Errorf("failed to delete token: %w", err)
		}
	} else {
		// Password account
		if !keychain.Exists(PasswordKeychainService, account.Email) {
			fmt.Printf("No password found for %s\n", account.Email) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
			return nil
		}

		if err := keychain.DeletePassword(PasswordKeychainService, account.Email); err != nil {
			return fmt.Errorf("failed to delete password: %w", err)
		}
	}

	fmt.Printf("✓ Logged out from %s\n", account.Email) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
	fmt.Printf("✓ Credentials removed from Keychain\n")

	return nil
}

func runAuthRefresh(cmd *cobra.Command, args []string) error {
	identifier := args[0]

	cfg := GetConfig()
	if cfg == nil {
		return errors.New("no configuration loaded")
	}

	// Find account
	account, err := cfg.GetAccountByIdentifier(identifier)
	if err != nil {
		return fmt.Errorf("account not found: %s\nAvailable accounts: %s", identifier, cfg.ListAccountIdentifiers())
	}

	// Only OAuth accounts can be refreshed
	if account.OAuth == nil || account.OAuth.Provider == "" {
		return fmt.Errorf("%s uses password authentication (no refresh needed)", account.Email)
	}

	// For shared mailboxes, the token is stored under the delegating user
	authEmail := account.GetAuthEmail()

	// Load existing token
	token, err := oauth.LoadToken(authEmail)
	if err != nil {
		if errors.Is(err, oauth.ErrTokenNotFound) {
			return fmt.Errorf("no token found for %s\nRun: durian auth login %s", authEmail, account.GetAliasOrName())
		}
		return err
	}

	// Get provider
	provider, err := oauth.GetProvider(token.Provider, account.OAuth.Tenant)
	if err != nil {
		return err
	}

	fmt.Printf("Refreshing token for %s...\n", authEmail)

	// Refresh token
	newToken, err := oauth.RefreshAccessToken(provider, account.OAuth.ClientID, account.OAuth.ClientSecret, token)
	if err != nil {
		if errors.Is(err, oauth.ErrTokenExpired) {
			// Delete invalid token
			_ = oauth.DeleteToken(authEmail)
			return fmt.Errorf("refresh token expired\nRun: durian auth login %s", account.GetAliasOrName())
		}
		return fmt.Errorf("refresh failed: %w", err)
	}

	// Save new token
	if err := oauth.SaveToken(authEmail, newToken); err != nil {
		return fmt.Errorf("failed to save token: %w", err)
	}

	fmt.Printf("✓ Token refreshed successfully\n")
	fmt.Printf("✓ New token expires in %s\n", formatDuration(newToken.ExpiresIn()))

	return nil
}

// getAccountStatusShort returns a machine-readable status for JSON output.
func getAccountStatusShort(account *config.AccountConfig) string {
	if account.OAuth != nil && account.OAuth.Provider != "" {
		token, err := oauth.LoadToken(account.GetAuthEmail())
		if err != nil {
			return "not_authenticated"
		}
		if token.IsExpired() {
			return "expired"
		}
		return "valid"
	}
	if account.SMTP.Auth == "password" || account.IMAP.Auth == "password" {
		if keychain.Exists(PasswordKeychainService, account.Email) {
			return "stored"
		}
		return "not_authenticated"
	}
	return "none"
}

func getAccountStatus(account *config.AccountConfig) string {
	// Check OAuth accounts
	if account.OAuth != nil && account.OAuth.Provider != "" {
		token, err := oauth.LoadToken(account.GetAuthEmail())
		if err != nil {
			if errors.Is(err, oauth.ErrTokenNotFound) {
				return fmt.Sprintf("✗ Not authenticated\n%34sRun: durian auth login %s", "", account.Email)
			}
			return fmt.Sprintf("✗ Error: %v", err)
		}

		if token.IsExpired() {
			return "⚠ Expired (will refresh on next use)"
		}

		expiresIn := token.ExpiresIn()
		if expiresIn < 5*time.Minute {
			return fmt.Sprintf("⚠ Expiring soon (%s)", formatDuration(expiresIn))
		}

		return fmt.Sprintf("✓ Valid (expires in %s)", formatDuration(expiresIn))
	}

	// Check password accounts
	if account.SMTP.Auth == "password" || account.IMAP.Auth == "password" {
		if keychain.Exists(PasswordKeychainService, account.Email) {
			return "✓ Password stored"
		}
		return fmt.Sprintf("✗ No password\n%34sRun: durian auth login %s", "", account.Email)
	}

	return "— (no auth configured)"
}

func getAuthType(account *config.AccountConfig) string {
	if account.OAuth != nil && account.OAuth.Provider != "" {
		return account.OAuth.Provider
	}
	if account.SMTP.Auth == "password" || account.IMAP.Auth == "password" {
		return "password"
	}
	return "—"
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		return "expired"
	}

	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60

	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}
