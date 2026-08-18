package keychain

import (
	"fmt"
	"os/exec"
	"strings"
)

// commandRunner creates exec.Cmd instances. Tests override this to avoid real keychain access.
var commandRunner = exec.Command

// GetPassword retrieves a password from the macOS Keychain
func GetPassword(service, account string) (string, error) {
	cmd := commandRunner("security", "find-generic-password",
		"-s", service,
		"-a", account,
		"-w", // Output only the password
	)

	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() == 44 {
				return "", ErrNotFound
			}
			return "", fmt.Errorf("keychain error: %s", string(exitErr.Stderr))
		}
		return "", fmt.Errorf("failed to get password: %w", err)
	}

	return strings.TrimSpace(string(output)), nil
}

// SetPassword stores a password in the macOS Keychain.
//
// -U makes add-generic-password update an existing item in place. Deleting
// first would open a window in which a concurrent writer recreates the item
// between the delete and the add, and the add then fails with "the specified
// item already exists" — which is exactly what happens when several accounts
// share one identity and rotate its refresh token at the same time.
func SetPassword(service, account, password string) error {
	cmd := commandRunner("security", "add-generic-password",
		"-s", service,
		"-a", account,
		"-w", password,
		"-U",
	)

	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to set password: %s", string(output))
	}

	return nil
}

// DeletePassword removes a password from the macOS Keychain
func DeletePassword(service, account string) error {
	cmd := commandRunner("security", "delete-generic-password",
		"-s", service,
		"-a", account,
	)

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() == 44 {
				return nil
			}
		}
		return fmt.Errorf("failed to delete password: %w", err)
	}

	return nil
}
