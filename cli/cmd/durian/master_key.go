package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/durian-dev/durian/cli/internal/agekey"
	"github.com/durian-dev/durian/cli/internal/dbcrypto"
	"github.com/durian-dev/durian/cli/internal/keychain"
)

// envMasterKeyHex is an override env var read by bootstrapKeyring before
// touching the OS keychain. Intended exclusively for integration tests
// and CI environments that have no Secret Service implementation.
//
// ADR-0001 audit medium-priority: the variable name includes "SECRET"
// so log scrubbers, GitHub push-protection, and generic
// secret-detection patterns flag it automatically. The legacy name
// DURIAN_MASTER_KEY_HEX (without _SECRET) is still recognized for
// one release as a deprecation grace period; setting it emits a Warn
// telling the operator to rename.
//
// On any successful read, bootstrapKeyring immediately os.Unsetenv's
// the variable so it doesn't propagate to child processes (sync
// helpers, exec'd hooks) and doesn't appear in coredumps captured
// after the read. Visibility into legitimate test/CI use is preserved
// via the Warn log line.
const (
	envMasterKeyHex       = "DURIAN_MASTER_KEY_HEX_SECRET"
	envMasterKeyHexLegacy = "DURIAN_MASTER_KEY_HEX"
)

// Flags
var (
	masterKeyExportOut   string
	masterKeyImportFrom  string
	masterKeyImportForce bool
)

var masterKeyCmd = &cobra.Command{
	Use:   "master-key",
	Short: "Manage the at-rest encryption master key",
	Long: `Manage the master key that protects Durian's encrypted database
columns (mail bodies, subjects, contacts, drafts).

The master key lives in the OS keychain. These subcommands let you back
it up to a passphrase-protected age file so the encrypted DB stays
recoverable if the keychain entry is lost.

WARNING: losing both the keychain entry AND any export file makes
local-only data (drafts, manual contacts) unrecoverable. IMAP-sourced
mail can be re-synced from the server.`,
}

var masterKeyExportCmd = &cobra.Command{
	Use:   "export --out FILE",
	Short: "Export the master key to a passphrase-encrypted age file",
	Long: `Read the master key from the OS keychain, encrypt it with an
age scrypt passphrase, and write the armored ciphertext to FILE.

The on-disk format is the same 64-character hex representation that the
keychain stores, so the file round-trips cleanly with 'master-key import'
and can also be decrypted manually with the 'age' CLI.

Use --out - to write to stdout.`,
	RunE: runMasterKeyExport,
}

var masterKeyImportCmd = &cobra.Command{
	Use:   "import --from FILE",
	Short: "Import a master key from an age file into the OS keychain",
	Long: `Decrypt an age file produced by 'master-key export' (or any age
file containing a 64-char hex master key) and store the result in the OS
keychain.

By default this refuses to overwrite an existing keychain entry. Use
--force only when you intend to replace the current master — note that
this makes any DB encrypted with the previous master unreadable.`,
	RunE: runMasterKeyImport,
}

func init() {
	rootCmd.AddCommand(masterKeyCmd)
	masterKeyCmd.AddCommand(masterKeyExportCmd)
	masterKeyCmd.AddCommand(masterKeyImportCmd)

	masterKeyExportCmd.Flags().StringVar(&masterKeyExportOut, "out", "", "destination file (or '-' for stdout)")
	masterKeyExportCmd.MarkFlagRequired("out")

	masterKeyImportCmd.Flags().StringVar(&masterKeyImportFrom, "from", "", "source age file")
	masterKeyImportCmd.Flags().BoolVar(&masterKeyImportForce, "force", false, "overwrite an existing keychain entry")
	masterKeyImportCmd.MarkFlagRequired("from")
}

// bootstrapKeyring loads (or, on first run, generates) the ADR-0001 master
// key from the OS keychain and derives the sub-key keyring consumed by
// store.Open. Fatal-exits on any error so the daemon never starts in a
// state where encrypt-on-write would silently no-op.
//
// If DURIAN_MASTER_KEY_HEX_SECRET (or the legacy DURIAN_MASTER_KEY_HEX)
// is set, the keychain path is skipped entirely and the env var's
// 64-char hex is used verbatim. This exists for the integration test
// and headless CI where there is no Secret Service implementation.
// A Warn-level audit line accompanies every use so accidental
// production deployments are visible in serve.log.
//
// ADR-0001 audit medium: the env var is os.Unsetenv'd immediately on
// read so it doesn't propagate to child processes or land in coredumps
// captured after the read. The legacy name emits an additional Warn
// asking the operator to rename — one-release deprecation grace period.
func bootstrapKeyring() *dbcrypto.Keyring {
	rawCanonical := strings.TrimSpace(os.Getenv(envMasterKeyHex))
	rawLegacy := strings.TrimSpace(os.Getenv(envMasterKeyHexLegacy))
	raw := rawCanonical
	source := envMasterKeyHex
	if raw == "" && rawLegacy != "" {
		raw = rawLegacy
		source = envMasterKeyHexLegacy
		slog.Warn("DURIAN_MASTER_KEY_HEX is deprecated — rename to DURIAN_MASTER_KEY_HEX_SECRET so secret-detection tooling catches it",
			"module", "MASTER-KEY")
	}
	// Clear BOTH names from the process environment regardless of which
	// (if either) was set — defense in depth against later code
	// re-reading. Errors from Unsetenv are not actionable here; in the
	// worst case the original value stays in /proc/self/environ but we
	// have not made things worse than the pre-fix state.
	_ = os.Unsetenv(envMasterKeyHex)
	_ = os.Unsetenv(envMasterKeyHexLegacy)
	if raw != "" {
		master, err := hex.DecodeString(raw)
		if err != nil || len(master) != dbcrypto.MasterKeyLen {
			slog.Error(source+" is set but not a valid 64-char hex of 32 bytes",
				"module", "MASTER-KEY", "err", err, "len", len(master))
			fmt.Fprintln(os.Stderr, "Error: "+source+" must be a 64-character hex string (32 bytes)")
			os.Exit(1)
		}
		kr, err := dbcrypto.NewKeyring(master)
		if err != nil {
			slog.Error("Keyring derivation failed", "module", "MASTER-KEY", "err", err)
			fmt.Fprintln(os.Stderr, "Error: keyring derivation failed:", err)
			os.Exit(1)
		}
		slog.Warn("Master key sourced from "+source+" (test/CI mode — NOT for production)",
			"module", "MASTER-KEY")
		return kr
	}

	existed := keychain.Exists(keychain.DBKeychainService, keychain.DBAccountMaster)
	master, err := keychain.GetOrCreateKey(keychain.DBKeychainService, keychain.DBAccountMaster, dbcrypto.MasterKeyLen)
	if err != nil {
		slog.Error("Master key bootstrap failed", "module", "MASTER-KEY", "err", err)
		fmt.Fprintln(os.Stderr, "Error: master key bootstrap failed:", err)
		os.Exit(1)
	}
	kr, err := dbcrypto.NewKeyring(master)
	if err != nil {
		slog.Error("Keyring derivation failed", "module", "MASTER-KEY", "err", err)
		fmt.Fprintln(os.Stderr, "Error: keyring derivation failed:", err)
		os.Exit(1)
	}
	if existed {
		slog.Info("Master key loaded from keychain", "module", "MASTER-KEY",
			"service", keychain.DBKeychainService, "account", keychain.DBAccountMaster)
	} else {
		slog.Info("Master key generated and stored in keychain (first run)", "module", "MASTER-KEY",
			"service", keychain.DBKeychainService, "account", keychain.DBAccountMaster)
	}
	return kr
}

func runMasterKeyExport(cmd *cobra.Command, args []string) error {
	// Read existing master from keychain (hex-encoded). If it doesn't exist,
	// refuse — generating one here would commit the user to encryption they
	// might not want, and 'durian serve' is the canonical bootstrap path.
	hexKey, err := keychain.GetPassword(keychain.DBKeychainService, keychain.DBAccountMaster)
	if err != nil {
		if errors.Is(err, keychain.ErrNotFound) {
			return fmt.Errorf("no master key in keychain (run 'durian serve' once to bootstrap it)")
		}
		return fmt.Errorf("read master key: %w", err)
	}
	// Sanity-check the stored value before we ship it anywhere.
	raw, err := hex.DecodeString(hexKey)
	if err != nil || len(raw) != dbcrypto.MasterKeyLen {
		return fmt.Errorf("stored master key is not a valid %d-byte hex string", dbcrypto.MasterKeyLen)
	}

	pass, err := promptPassword("Passphrase to encrypt the master key: ")
	if err != nil {
		return fmt.Errorf("read passphrase: %w", err)
	}
	confirm, err := promptPassword("Confirm passphrase: ")
	if err != nil {
		return fmt.Errorf("read passphrase confirmation: %w", err)
	}
	if pass != confirm {
		return fmt.Errorf("passphrases do not match")
	}

	ct, err := agekey.Encrypt([]byte(hexKey), pass)
	if err != nil {
		return fmt.Errorf("encrypt master key: %w", err)
	}

	if masterKeyExportOut == "-" {
		if _, err := os.Stdout.Write(ct); err != nil {
			return fmt.Errorf("write stdout: %w", err)
		}
		slog.Info("Master key exported", "module", "MASTER-KEY", "out", "stdout")
		return nil
	}
	// 0600 — even on systems where umask would already trim group/other,
	// being explicit prevents accidental world-readable backups.
	if err := os.WriteFile(masterKeyExportOut, ct, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", masterKeyExportOut, err)
	}
	fmt.Fprintf(os.Stderr, "Master key exported to %s (age-encrypted, 0600)\n", masterKeyExportOut)
	slog.Info("Master key exported", "module", "MASTER-KEY", "out", masterKeyExportOut)
	return nil
}

func runMasterKeyImport(cmd *cobra.Command, args []string) error {
	ct, err := os.ReadFile(masterKeyImportFrom)
	if err != nil {
		return fmt.Errorf("read %s: %w", masterKeyImportFrom, err)
	}

	pass, err := promptPassword("Passphrase to decrypt the master key: ")
	if err != nil {
		return fmt.Errorf("read passphrase: %w", err)
	}

	plaintext, err := agekey.Decrypt(ct, pass)
	if err != nil {
		// Don't differentiate "bad passphrase" from "corrupt ciphertext" —
		// both look identical to the user, and the distinction is a
		// passphrase-guessing oracle.
		return fmt.Errorf("decrypt master key: invalid passphrase or corrupted file")
	}
	hexKey := string(plaintext)
	raw, err := hex.DecodeString(hexKey)
	if err != nil || len(raw) != dbcrypto.MasterKeyLen {
		return fmt.Errorf("decoded value is not a valid %d-byte hex master key", dbcrypto.MasterKeyLen)
	}

	if keychain.Exists(keychain.DBKeychainService, keychain.DBAccountMaster) && !masterKeyImportForce {
		return fmt.Errorf("master key already present in keychain; re-run with --force to overwrite (this makes any DB encrypted with the previous master unreadable)")
	}
	if err := keychain.SetPassword(keychain.DBKeychainService, keychain.DBAccountMaster, hexKey); err != nil {
		return fmt.Errorf("store master key: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Master key imported from %s into keychain (%s/%s)\n",
		masterKeyImportFrom, keychain.DBKeychainService, keychain.DBAccountMaster)
	slog.Info("Master key imported", "module", "MASTER-KEY", "from", masterKeyImportFrom, "forced", masterKeyImportForce)
	return nil
}
