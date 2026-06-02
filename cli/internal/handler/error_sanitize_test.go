package handler

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/julion2/durian/cli/internal/smtp"
)

// TestSanitizeOutboxError_StripsSMTPServerResponse asserts ADR-0001
// audit medium: an SMTP 5xx response body (which on Gmail / Office365
// commonly echoes the offending To: address or Subject fragment) must
// not appear in outbox.last_error (DB-persisted, shown in GUI) or in
// the SSE broadcast. The sanitized form keeps the protocol code so
// triage stays possible.
func TestSanitizeOutboxError_StripsSMTPServerResponse(t *testing.T) {
	// Construct a realistic SMTP error carrying a server message that
	// references private content.
	leaky := &smtp.SMTPError{
		Code:    550,
		Message: "5.7.1 [alice@private.example] Recipient address rejected: Subject 'Acquisition Q4 plans' triggered policy",
	}
	got := sanitizeOutboxError(leaky)
	if strings.Contains(got, "alice@private.example") {
		t.Errorf("sanitized output leaks recipient address: %q", got)
	}
	if strings.Contains(got, "Acquisition Q4") {
		t.Errorf("sanitized output leaks subject fragment: %q", got)
	}
	if !strings.Contains(got, "550") {
		t.Errorf("sanitized output should keep the protocol code, got %q", got)
	}
	if !strings.Contains(got, "permanent") {
		t.Errorf("sanitized output should classify 5xx as permanent, got %q", got)
	}
}

func TestSanitizeOutboxError_JSONUnmarshalKeepsOffset(t *testing.T) {
	// json.SyntaxError carries .Offset; the message body may also
	// echo bytes near that offset depending on Go version. We keep
	// the offset (useful for debugging) but never the surrounding
	// bytes — sanitizer should produce a fixed-shape message.
	bad := []byte(`{"subject": "secret-draft-content", THIS-IS-INVALID}`)
	var dst map[string]any
	err := json.Unmarshal(bad, &dst)
	if err == nil {
		t.Fatal("unmarshal should have errored on malformed JSON")
	}
	got := sanitizeOutboxError(err)
	if strings.Contains(got, "secret-draft-content") {
		t.Errorf("sanitized output leaks draft content: %q", got)
	}
	if !strings.Contains(got, "offset") {
		t.Errorf("sanitized output should mention the offset for debugging, got %q", got)
	}
}

func TestSanitizeOutboxError_Base64KeepsBytePos(t *testing.T) {
	_, err := base64.StdEncoding.DecodeString("totally-not-base64-data-with-secret-bytes")
	if err == nil {
		t.Fatal("decode should have errored")
	}
	got := sanitizeOutboxError(err)
	if strings.Contains(got, "secret-bytes") {
		t.Errorf("sanitized output leaks attachment data: %q", got)
	}
	if !strings.Contains(got, "corrupt base64") {
		t.Errorf("sanitized output should name the category, got %q", got)
	}
}

func TestSanitizeOutboxError_NetworkSafeVerbatim(t *testing.T) {
	// Network errors carry hostnames (public, configured by user) and
	// op names (e.g. "dial"). Verbatim is acceptable — there's no
	// draft content in this category.
	netErr := &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: errors.New("connection refused"),
	}
	got := sanitizeOutboxError(netErr)
	if !strings.Contains(got, "dial") {
		t.Errorf("sanitized output should retain the op for triage, got %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("sanitized output should retain the underlying cause for network errors, got %q", got)
	}
}

func TestSanitizeOutboxError_UnknownIsOpaque(t *testing.T) {
	// An error type the sanitizer doesn't recognize. The raw .Error()
	// might contain anything — return the opaque sentinel.
	mystery := errors.New("Subject: Top-secret merger draft contents reflected in error wrap")
	got := sanitizeOutboxError(mystery)
	if strings.Contains(got, "merger draft") {
		t.Errorf("sanitized output leaks unknown-class error contents: %q", got)
	}
	if !strings.Contains(got, "send failed") {
		t.Errorf("sanitized output should use opaque sentinel, got %q", got)
	}
	if !strings.Contains(got, "serve.log") {
		t.Errorf("sanitized output should point operator to the log file for the full error, got %q", got)
	}
}

func TestSanitizeOutboxError_NilIsEmpty(t *testing.T) {
	if got := sanitizeOutboxError(nil); got != "" {
		t.Errorf("sanitizeOutboxError(nil) = %q, want empty", got)
	}
}
