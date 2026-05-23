package redact

import (
	"bytes"
	"context"
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

func TestHandle_RedactsAllAddressKeys(t *testing.T) {
	for _, key := range []string{"to", "from", "cc", "bcc", "reply_to", "recipient", "sender"} {
		t.Run(key, func(t *testing.T) {
			var buf bytes.Buffer
			log := newTestLogger(&buf)
			log.Info("addr", key, "alice@example.com")
			if strings.Contains(buf.String(), "alice@example.com") {
				t.Errorf("address leaked under key %q:\n%s", key, buf.String())
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
	for _, key := range []string{"subject", "body", "from", "contact_email", "draft_json"} {
		if !IsSensitive(key) {
			t.Errorf("IsSensitive(%q) = false, want true", key)
		}
	}
	for _, key := range []string{"email", "account", "id", "module", "err"} {
		if IsSensitive(key) {
			t.Errorf("IsSensitive(%q) = true, want false", key)
		}
	}
}
