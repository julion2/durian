package keychain

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestHelperProcess is not a real test — it is invoked as a subprocess by
// tests that override commandRunner to simulate security CLI responses.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_TEST_HELPER") != "1" {
		return
	}
	switch os.Getenv("GO_TEST_MODE") {
	case "success":
		fmt.Fprint(os.Stdout, os.Getenv("GO_TEST_STDOUT"))
		os.Exit(0)
	case "exit44":
		os.Exit(44)
	case "exit1":
		fmt.Fprint(os.Stderr, os.Getenv("GO_TEST_STDERR"))
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, "unknown GO_TEST_MODE")
		os.Exit(2)
	}
}

// mockCommand returns a commandRunner replacement that invokes TestHelperProcess
// with the given mode, stdout, and stderr values.
func mockCommand(mode, stdout, stderr string) func(string, ...string) *exec.Cmd {
	return func(name string, args ...string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
		cmd.Env = append(os.Environ(),
			"GO_TEST_HELPER=1",
			"GO_TEST_MODE="+mode,
			"GO_TEST_STDOUT="+stdout,
			"GO_TEST_STDERR="+stderr,
		)
		return cmd
	}
}

func restoreCommandRunner() { commandRunner = exec.Command }

// --- GetPassword ---

func TestGetPassword_Success(t *testing.T) {
	commandRunner = mockCommand("success", "my-secret\n", "")
	defer restoreCommandRunner()

	pw, err := GetPassword("svc", "acct")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pw != "my-secret" {
		t.Errorf("password = %q, want %q", pw, "my-secret")
	}
}

func TestGetPassword_NotFound(t *testing.T) {
	commandRunner = mockCommand("exit44", "", "")
	defer restoreCommandRunner()

	_, err := GetPassword("svc", "acct")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestGetPassword_ExecError(t *testing.T) {
	commandRunner = mockCommand("exit1", "", "security: something went wrong")
	defer restoreCommandRunner()

	_, err := GetPassword("svc", "acct")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("should not be ErrNotFound")
	}
}

// --- SetPassword ---

func TestSetPassword_Success(t *testing.T) {
	commandRunner = mockCommand("success", "", "")
	defer restoreCommandRunner()

	err := SetPassword("svc", "acct", "pw")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSetPassword_Failure(t *testing.T) {
	commandRunner = mockCommand("exit1", "", "add failed")
	defer restoreCommandRunner()

	err := SetPassword("svc", "acct", "pw")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// --- DeletePassword ---

func TestDeletePassword_Success(t *testing.T) {
	commandRunner = mockCommand("success", "", "")
	defer restoreCommandRunner()

	err := DeletePassword("svc", "acct")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeletePassword_NotFoundIsNil(t *testing.T) {
	commandRunner = mockCommand("exit44", "", "")
	defer restoreCommandRunner()

	err := DeletePassword("svc", "acct")
	if err != nil {
		t.Errorf("expected nil for not-found, got: %v", err)
	}
}

func TestDeletePassword_OtherError(t *testing.T) {
	commandRunner = mockCommand("exit1", "", "delete failed")
	defer restoreCommandRunner()

	err := DeletePassword("svc", "acct")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// --- Exists ---

func TestExists_Found(t *testing.T) {
	commandRunner = mockCommand("success", "pw\n", "")
	defer restoreCommandRunner()

	if !Exists("svc", "acct") {
		t.Error("Exists() = false, want true")
	}
}

func TestExists_NotFound(t *testing.T) {
	commandRunner = mockCommand("exit44", "", "")
	defer restoreCommandRunner()

	if Exists("svc", "acct") {
		t.Error("Exists() = true, want false")
	}
}

// --- GetOrCreateKey ---

// scriptedResponse describes one mocked invocation of the `security` CLI.
type scriptedResponse struct {
	mode, stdout, stderr string
}

// scriptCommands replaces commandRunner with a sequence-aware mock. Each call
// consumes one entry from responses, in order. Failing the test for over-runs
// catches accidental extra keychain calls (an easy way to leak credentials).
func scriptCommands(t *testing.T, responses []scriptedResponse) {
	t.Helper()
	var i int
	commandRunner = func(name string, args ...string) *exec.Cmd {
		if i >= len(responses) {
			t.Fatalf("unexpected extra command #%d: %s %v", i+1, name, args)
		}
		r := responses[i]
		i++
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
		cmd.Env = append(os.Environ(),
			"GO_TEST_HELPER=1",
			"GO_TEST_MODE="+r.mode,
			"GO_TEST_STDOUT="+r.stdout,
			"GO_TEST_STDERR="+r.stderr,
		)
		return cmd
	}
}

func TestGetOrCreateKey_ReturnsExisting(t *testing.T) {
	want := make([]byte, 32)
	for i := range want {
		want[i] = byte(i)
	}
	scriptCommands(t, []scriptedResponse{
		{mode: "success", stdout: hex.EncodeToString(want) + "\n"},
	})
	defer restoreCommandRunner()

	got, err := GetOrCreateKey(DBKeychainService, DBAccountMaster, 32)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hex.EncodeToString(got) != hex.EncodeToString(want) {
		t.Errorf("key mismatch:\n got=%x\nwant=%x", got, want)
	}
}

func TestGetOrCreateKey_GeneratesWhenMissing(t *testing.T) {
	// SetPassword unconditionally calls DeletePassword first, then add. So a
	// fresh-key path is: find (NotFound) → delete (NotFound, treated as ok)
	// → add (ok). Three invocations.
	scriptCommands(t, []scriptedResponse{
		{mode: "exit44"},  // find-generic-password → not found
		{mode: "exit44"},  // delete-generic-password → not found (ok)
		{mode: "success"}, // add-generic-password → stored
	})
	defer restoreCommandRunner()

	got, err := GetOrCreateKey(DBKeychainService, DBAccountMaster, 32)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 32 {
		t.Errorf("generated key length = %d, want 32", len(got))
	}
	// A fresh crypto/rand key being all-zero would be astronomically unlikely.
	var allZero [32]byte
	if hex.EncodeToString(got) == hex.EncodeToString(allZero[:]) {
		t.Error("generated key is all zero — rand.Read silently failed?")
	}
}

func TestGetOrCreateKey_RejectsInvalidHex(t *testing.T) {
	scriptCommands(t, []scriptedResponse{
		{mode: "success", stdout: "not-hex-zzzz\n"},
	})
	defer restoreCommandRunner()

	_, err := GetOrCreateKey(DBKeychainService, DBAccountMaster, 32)
	if err == nil {
		t.Fatal("expected error for non-hex stored value, got nil")
	}
	if !strings.Contains(err.Error(), "not valid hex") {
		t.Errorf("error = %v, want it to mention 'not valid hex'", err)
	}
}

func TestGetOrCreateKey_RejectsWrongLength(t *testing.T) {
	// 16 bytes hex-encoded → 32 chars; ask for 32 bytes → mismatch.
	short := strings.Repeat("ab", 16)
	scriptCommands(t, []scriptedResponse{
		{mode: "success", stdout: short + "\n"},
	})
	defer restoreCommandRunner()

	_, err := GetOrCreateKey(DBKeychainService, DBAccountMaster, 32)
	if err == nil {
		t.Fatal("expected error for wrong-length stored key, got nil")
	}
	if !strings.Contains(err.Error(), "length 16, want 32") {
		t.Errorf("error = %v, want length-mismatch message", err)
	}
}

func TestGetOrCreateKey_RejectsBadNbytes(t *testing.T) {
	// No commands should be issued: validation must happen before any keychain access.
	scriptCommands(t, nil)
	defer restoreCommandRunner()

	if _, err := GetOrCreateKey(DBKeychainService, DBAccountMaster, 0); err == nil {
		t.Error("nbytes=0: expected error, got nil")
	}
	if _, err := GetOrCreateKey(DBKeychainService, DBAccountMaster, -1); err == nil {
		t.Error("nbytes=-1: expected error, got nil")
	}
}

func TestGetOrCreateKey_PropagatesLookupError(t *testing.T) {
	scriptCommands(t, []scriptedResponse{
		{mode: "exit1", stderr: "security: SecKeychainSearchCopyNext: The user name or passphrase you entered is not correct."},
	})
	defer restoreCommandRunner()

	_, err := GetOrCreateKey(DBKeychainService, DBAccountMaster, 32)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("lookup error must not be reported as ErrNotFound — would silently regenerate the master key")
	}
}
