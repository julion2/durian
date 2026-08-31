package imap

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/julion2/durian/cli/internal/redact"
)

func TestIMAPServerErrorPreservesCallerTextAndProvidesSafeLogText(t *testing.T) {
	const response = "NO short multiword response echoing token abc123"
	raw := errors.New(response)
	err := imapServerError(fmt.Errorf("failed to select mailbox Private: %w", raw), "select IMAP mailbox failed")
	if !strings.Contains(err.Error(), response) || !strings.Contains(err.Error(), "Private") {
		t.Fatalf("Error() lost caller-facing context: %q", err.Error())
	}
	if !errors.Is(err, raw) {
		t.Fatal("IMAP redaction marker broke errors.Is")
	}
	var safeErr redact.SafeLogError
	if !errors.As(err, &safeErr) {
		t.Fatalf("error %T is not marked safe for logging", err)
	}
	if strings.Contains(safeErr.SafeLogText(), response) || strings.Contains(safeErr.SafeLogText(), "Private") {
		t.Fatalf("safe log text leaked IMAP data: %q", safeErr.SafeLogText())
	}
}
