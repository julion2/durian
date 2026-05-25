package handler

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"

	"github.com/durian-dev/durian/cli/internal/smtp"
)

// sanitizeOutboxError returns a string safe to persist in outbox.last_error
// and broadcast over SSE to the GUI. ADR-0001 audit medium: the raw
// err.Error() string from json.Unmarshal, base64.DecodeString,
// SMTP-server 5xx responses, etc. can echo fragments of the draft body
// (subject, recipient addresses, raw bytes) — none of which should
// reach a serialized DB column or the SSE channel that any GUI tab can
// observe.
//
// Strategy: classify into known-safe categories that return a fixed
// or value-stripped sentence; unknown errors fall back to a generic
// "send failed (see serve.log)" so the full message is only available
// in the redact-wrapped slog stream, not in user-facing surfaces.
func sanitizeOutboxError(err error) string {
	if err == nil {
		return ""
	}

	// SMTP errors: keep the protocol code, drop the server-supplied
	// message which on Gmail/Office365 frequently echoes the offending
	// header (To: address, Subject fragment) verbatim.
	var smtpErr *smtp.SMTPError
	if errors.As(err, &smtpErr) {
		category := "transient"
		if smtpErr.IsPermanent() {
			category = "permanent"
		}
		return fmt.Sprintf("smtp %d %s (server response stripped)", smtpErr.Code, category)
	}

	// JSON unmarshal errors: the type-error variant echoes the value at
	// the offending offset. The syntax-error variant echoes the byte
	// offset only (safe) but other implementations may include context.
	var jsonSyntax *json.SyntaxError
	var jsonType *json.UnmarshalTypeError
	if errors.As(err, &jsonSyntax) {
		return fmt.Sprintf("invalid draft JSON: syntax error at offset %d", jsonSyntax.Offset)
	}
	if errors.As(err, &jsonType) {
		return fmt.Sprintf("invalid draft JSON: type mismatch at offset %d (expected %s)", jsonType.Offset, jsonType.Type)
	}

	// Base64 attachment decode: the corrupt-input error includes the
	// offending byte position which can be a substring of the encoded
	// data; the position alone is benign.
	var b64Err base64.CorruptInputError
	if errors.As(err, &b64Err) {
		return fmt.Sprintf("attachment decode failed: corrupt base64 at byte %d", int64(b64Err))
	}

	// Network errors: timeout / connection refused / DNS — these
	// include host names which are public (the configured SMTP server)
	// and do not echo draft content. Safe to surface verbatim.
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return fmt.Sprintf("network error (%s): %s", netErr.Op, netErr.Err)
	}

	// Default: opaque sentinel. The full error is in serve.log via the
	// matching slog.Error call at the original site; the GUI sees only
	// that something failed, not what the server said.
	return "send failed (details in serve.log)"
}
