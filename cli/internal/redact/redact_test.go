package redact

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// newTestLogger returns a logger writing to buf, wrapped with redact.
func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	inner := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(Wrap(inner))
}

func TestHandle_RedactsSensitiveStringKey(t *testing.T) {
	var buf bytes.Buffer
	log := newTestLogger(&buf)
	log.Info("Sending", "subject", "Top-secret Q4 strategy", "id", 42)

	out := buf.String()
	if strings.Contains(out, "Top-secret") {
		t.Errorf("sensitive value leaked into log output:\n%s", out)
	}
	if !strings.Contains(out, "subject="+Placeholder) {
		t.Errorf("expected subject=%s, got:\n%s", Placeholder, out)
	}
	// Non-sensitive attr passes through.
	if !strings.Contains(out, "id=42") {
		t.Errorf("non-sensitive attr was dropped:\n%s", out)
	}
}

// TestHandle_RedactsMailboxAndAccountKeys asserts the keys added by
// audit H1: mailboxes.name and accounts.name are encrypted at rest
// since step 6, but the original allow-list used legacy spellings
// (mailbox_name, contact_email) while production IMAP code uses the
// bare forms (mailbox, account) — three months of plaintext mailbox
// names leaked into serve.log because of that drift.
func TestHandle_RedactsMailboxAndAccountKeys(t *testing.T) {
	for _, key := range []string{"mailbox", "account", "dest", "trash", "archive", "synthetic_id"} {
		t.Run(key, func(t *testing.T) {
			var buf bytes.Buffer
			log := newTestLogger(&buf)
			log.Info("sync", key, "Archive/Healthcare")
			if strings.Contains(buf.String(), "Archive/Healthcare") {
				t.Errorf("encrypted-at-rest value leaked under key %q:\n%s", key, buf.String())
			}
		})
	}
}

// TestHandle_PreservesAddressKeys asserts the β-revision: from / to /
// cc / bcc / reply_to / recipient / sender are plaintext-by-design per
// ADR §3 (they're on the wire anyway), so the wrapper must NOT redact
// them — that would silently break legitimate IMAP/SMTP diagnostic
// logs. The previous registry erroneously redacted these.
func TestHandle_PreservesAddressKeys(t *testing.T) {
	for _, key := range []string{"to", "from", "cc", "bcc", "reply_to", "recipient", "sender"} {
		t.Run(key, func(t *testing.T) {
			var buf bytes.Buffer
			log := newTestLogger(&buf)
			log.Info("addr", key, "alice@example.com")
			if !strings.Contains(buf.String(), "alice@example.com") {
				t.Errorf("address was incorrectly redacted under key %q (β-revision says plaintext):\n%s", key, buf.String())
			}
		})
	}
}

func TestHandle_PreservesAccountEmailKey(t *testing.T) {
	// "email" is the account identifier, plaintext-by-design per ADR-0001 §3.
	// The wrapper must not redact it — only "contact_email" is scrubbed.
	var buf bytes.Buffer
	log := newTestLogger(&buf)
	log.Info("sync", "email", "user@example.com", "contact_email", "secret@example.com")

	out := buf.String()
	if !strings.Contains(out, "user@example.com") {
		t.Errorf("account email was incorrectly redacted:\n%s", out)
	}
	if strings.Contains(out, "secret@example.com") {
		t.Errorf("contact email leaked:\n%s", out)
	}
}

func TestHandle_RedactsNonStringValues(t *testing.T) {
	// A future slog.Any("subject", struct{...}) call must still be scrubbed.
	var buf bytes.Buffer
	log := newTestLogger(&buf)
	type secret struct{ Body string }
	log.Info("msg", "body", secret{Body: "do not log this"})

	out := buf.String()
	if strings.Contains(out, "do not log this") {
		t.Errorf("non-string sensitive value leaked:\n%s", out)
	}
	if !strings.Contains(out, "body="+Placeholder) {
		t.Errorf("expected body=%s, got:\n%s", Placeholder, out)
	}
}

func TestWithAttrs_RedactsBoundAttrs(t *testing.T) {
	// Attrs bound via With(...) must also be scrubbed, not just per-call attrs.
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	log := slog.New(Wrap(inner)).With("subject", "leaked once, leaked forever")
	log.Info("twice")
	log.Info("twice again")

	out := buf.String()
	if strings.Contains(out, "leaked") {
		t.Errorf("bound sensitive attr leaked across log calls:\n%s", out)
	}
}

func TestWithGroup_RedactsNestedAttrs(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	log := slog.New(Wrap(inner)).WithGroup("mail")
	log.Info("event", "subject", "nested-secret", "id", 7)

	out := buf.String()
	if strings.Contains(out, "nested-secret") {
		t.Errorf("nested sensitive attr leaked through group:\n%s", out)
	}
}

func TestRedact_HandlesGroupValue(t *testing.T) {
	// slog.Group(...) packages attrs into one Attr with KindGroup; we must
	// recurse so a sensitive key inside the group is also scrubbed.
	var buf bytes.Buffer
	log := newTestLogger(&buf)
	log.Info("event", slog.Group("payload", slog.String("subject", "in-group secret"), slog.Int("size", 12)))

	out := buf.String()
	if strings.Contains(out, "in-group secret") {
		t.Errorf("sensitive value inside slog.Group leaked:\n%s", out)
	}
	if !strings.Contains(out, "size=12") {
		t.Errorf("non-sensitive group attr was dropped:\n%s", out)
	}
}

func TestWrap_NilHandlerPassesThrough(t *testing.T) {
	if got := Wrap(nil); got != nil {
		t.Errorf("Wrap(nil) = %v, want nil", got)
	}
}

func TestEnabled_DelegatesToWrapped(t *testing.T) {
	// Inner handler set to Warn — Debug should be disabled, Error enabled.
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	h := Wrap(inner)
	if h.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("Debug should be disabled (wrapped level=Warn)")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("Error should be enabled (wrapped level=Warn)")
	}
}

func TestIsSensitive(t *testing.T) {
	// Encrypted-at-rest per ADR §3 (post-β-revision set).
	for _, key := range []string{"subject", "body", "contact_email", "draft_json", "mailbox", "account", "synthetic_id"} {
		if !IsSensitive(key) {
			t.Errorf("IsSensitive(%q) = false, want true (encrypted at rest per ADR §3)", key)
		}
	}
	// Plaintext-by-design per ADR §3 + β-revision.
	for _, key := range []string{"email", "from", "to", "cc", "id", "module", "err", "uid", "date"} {
		if IsSensitive(key) {
			t.Errorf("IsSensitive(%q) = true, want false (plaintext-by-design per ADR §3 β-revision)", key)
		}
	}
}

// TestHandle_SanitizesServerEchoInError asserts G2 of the encryption audit:
// IMAP/SMTP server responses can echo mail content (Subject lines, quoted
// headers) back in error text, and "err" is not a key-based redaction target.
// A long server-echoed run must be redacted out of the readable log.
func TestHandle_SanitizesServerEchoInError(t *testing.T) {
	// A single long run — the shape of a raw server literal / quoted header.
	leak := "X-Confidential-Subject:TopSecretMergerWithAcmeCorpQ4-boardroom-only-do-not-forward-2026"
	if len(leak) <= maxSafeRun {
		t.Fatalf("test fixture must exceed maxSafeRun (%d), got %d", maxSafeRun, len(leak))
	}

	t.Run("error value", func(t *testing.T) {
		var buf bytes.Buffer
		log := newTestLogger(&buf)
		log.Warn("append failed", "err", errors.New("imap: NO "+leak))
		if strings.Contains(buf.String(), leak) {
			t.Errorf("server-echoed run leaked via err value:\n%s", buf.String())
		}
		if !strings.Contains(buf.String(), "[redacted ") {
			t.Errorf("expected redaction marker, got:\n%s", buf.String())
		}
	})

	t.Run("err.Error() logged as string", func(t *testing.T) {
		var buf bytes.Buffer
		log := newTestLogger(&buf)
		log.Warn("append failed", "err", "imap: NO "+leak)
		if strings.Contains(buf.String(), leak) {
			t.Errorf("server-echoed run leaked via err string:\n%s", buf.String())
		}
	})
}

// TestHandle_PreservesShortErrors asserts the sanitizer's blast radius is
// zero on the common path: ordinary error diagnostics pass through readable.
func TestHandle_PreservesShortErrors(t *testing.T) {
	var buf bytes.Buffer
	log := newTestLogger(&buf)
	log.Warn("select failed", "err", errors.New("connection reset by peer"))
	if !strings.Contains(buf.String(), "connection reset by peer") {
		t.Errorf("short error was mangled:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "[redacted ") {
		t.Errorf("short error should not be redacted:\n%s", buf.String())
	}
}

// TestSanitizeTextIsNotReversible is the point of the redaction: an earlier
// version base64-encoded the over-long field, which looks like redaction but
// hands the plaintext back to anyone with `base64 -d`. The marker must carry
// no recoverable content.
func TestSanitizeTextIsNotReversible(t *testing.T) {
	secret := strings.Repeat("Quarterly-Results-CONFIDENTIAL-", 5) // > maxSafeRun
	got := sanitizeText(secret)

	if strings.Contains(got, "CONFIDENTIAL") {
		t.Fatalf("plaintext survived redaction: %q", got)
	}
	if strings.Contains(got, base64.StdEncoding.EncodeToString([]byte(secret))) {
		t.Fatalf("output carries a reversible encoding of the input: %q", got)
	}
	for _, dec := range []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		base64.URLEncoding.DecodeString,
	} {
		for _, field := range strings.Fields(got) {
			if raw, err := dec(field); err == nil && strings.Contains(string(raw), "CONFIDENTIAL") {
				t.Fatalf("a field decoded back to the plaintext: %q", field)
			}
		}
	}

	// Still useful: the same input redacts to the same marker, so repeated
	// occurrences stay correlatable, and the length is preserved.
	if sanitizeText(secret) != got {
		t.Error("redaction is not deterministic; identical values must be correlatable")
	}
	if !strings.Contains(got, "155B") {
		t.Errorf("byte length not reported, got %q", got)
	}
}
