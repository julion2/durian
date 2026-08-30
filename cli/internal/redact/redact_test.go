package redact

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// TestSanitizeStripsURLUserinfo covers the credentials the length heuristic
// cannot see. A URL carrying userinfo is usually well under maxSafeRun, so a
// connection error quoting the URL it dialled logged the password verbatim.
// Position, not length, is what makes userinfo sensitive.
func TestSanitizeStripsURLUserinfo(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		secrets []string
	}{
		{
			name:    "user and password in a dial error",
			in:      `dial https://alice:hunter2@imap.example.com/x: refused`,
			want:    `dial https://[REDACTED]@imap.example.com/x: refused`,
			secrets: []string{"alice", "hunter2"},
		},
		{
			name:    "user only",
			in:      `get imaps://alice@mail.example.com failed`,
			want:    `get imaps://[REDACTED]@mail.example.com failed`,
			secrets: []string{"alice"},
		},
		{
			name:    "at sign inside the password",
			in:      `https://u:p@ss@host.example.com/path`,
			want:    `https://[REDACTED]@host.example.com/path`,
			secrets: []string{"p@ss"},
		},
		{
			name: "no userinfo is left alone",
			in:   `dial https://imap.example.com/x: refused`,
			want: `dial https://imap.example.com/x: refused`,
		},
		{
			// The "@" belongs to a query value, not to authority userinfo.
			// The complete value is still sensitive and must be removed.
			name:    "at sign in a query value",
			in:      `https://api.example.com/send?to=bob@example.com failed`,
			want:    `https://api.example.com/send?to=[REDACTED] failed`,
			secrets: []string{"bob@example.com"},
		},
		{
			name:    "multiple query values and fragment",
			in:      `Get "https://api.example.com/token?code=short-secret&state=also-secret#access-token": 401`,
			want:    `Get "https://api.example.com/token?code=[REDACTED]&state=[REDACTED]#[REDACTED]": 401`,
			secrets: []string{"short-secret", "also-secret", "access-token"},
		},
		{
			name:    "query without equals",
			in:      `https://api.example.com/callback?short-secret`,
			want:    `https://api.example.com/callback?[REDACTED]`,
			secrets: []string{"short-secret"},
		},
		{
			name:    "fragment without query",
			in:      `https://api.example.com/callback#access-token`,
			want:    `https://api.example.com/callback#[REDACTED]`,
			secrets: []string{"access-token"},
		},
		{
			name:    "question mark inside fragment",
			in:      `https://api.example.com/callback#access_token=short-secret?x=y`,
			want:    `https://api.example.com/callback#[REDACTED]`,
			secrets: []string{"short-secret", "x=y"},
		},
		{
			name:    "IPv6 URL query",
			in:      `Get "https://[::1]/callback?code=short-secret": refused`,
			want:    `Get "https://[::1]/callback?code=[REDACTED]": refused`,
			secrets: []string{"short-secret"},
		},
		{
			name:    "query URL followed by adjacent safe URL",
			in:      `https://a.test/c?code=short-secret,https://b.test/health`,
			want:    `https://a.test/c?code=[REDACTED],https://b.test/health`,
			secrets: []string{"short-secret"},
		},
		{
			name: "plain text untouched",
			in:   `connection reset by peer`,
			want: `connection reset by peer`,
		},
		{
			// Two URLs in one whitespace-delimited field. Scanning fields and
			// taking the first "://" per field stopped at the safe one and let
			// the credentials behind it through untouched.
			name:    "safe URL followed by a credential URL, no whitespace",
			in:      `tried https://safe.example,https://alice:hunter2@imap.example/x`,
			want:    `tried https://safe.example,https://[REDACTED]@imap.example/x`,
			secrets: []string{"alice", "hunter2"},
		},
		{
			// Every URL, not just the first one that matches.
			name:    "two credential URLs, no whitespace",
			in:      `https://a:b@h1.example/x,https://c:d@h2.example/y`,
			want:    `https://[REDACTED]@h1.example/x,https://[REDACTED]@h2.example/y`,
			secrets: []string{"a:b", "c:d"},
		},
		{
			name:    "three URLs, credentials in the middle",
			in:      `[https://one.example|https://u:p@two.example|https://three.example]`,
			want:    `[https://one.example|https://[REDACTED]@two.example|https://three.example]`,
			secrets: []string{"u:p"},
		},
		// RFC 3986 sub-delims are legal unencoded in a userinfo. Treating any
		// of them as the end of the authority made the scan stop before the
		// "@" and find nothing, so a password containing one was logged whole.
		{
			name:    "comma in the password",
			in:      `https://alice:pa,ss@host.example/x`,
			want:    `https://[REDACTED]@host.example/x`,
			secrets: []string{"pa,ss"},
		},
		{
			name:    "semicolon in the password",
			in:      `https://alice:pa;ss@host.example/x`,
			want:    `https://[REDACTED]@host.example/x`,
			secrets: []string{"pa;ss"},
		},
		{
			name:    "apostrophe in the password",
			in:      `https://alice:pa'ss@host.example/x`,
			want:    `https://[REDACTED]@host.example/x`,
			secrets: []string{"pa'ss"},
		},
		{
			name:    "parentheses in the password",
			in:      `https://alice:pa(ss)@host.example/x`,
			want:    `https://[REDACTED]@host.example/x`,
			secrets: []string{"pa(ss)"},
		},
		{
			// The complete sub-delims production from RFC 3986 §2.2, not just
			// the five that used to terminate the authority. Written against
			// the grammar rather than against examples: the earlier version of
			// this scan looked correct precisely because every fixture used an
			// alphanumeric password.
			name:    "every RFC 3986 sub-delim in the password",
			in:      `https://alice:!$&'()*+,;=@host.example/x`,
			want:    `https://[REDACTED]@host.example/x`,
			secrets: []string{`!$&'()*+,;=`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeText(tt.in)
			if got != tt.want {
				t.Errorf("sanitizeText() = %q, want %q", got, tt.want)
			}
			for _, secret := range tt.secrets {
				if strings.Contains(got, secret) {
					t.Errorf("output still contains %q: %q", secret, got)
				}
			}
		})
	}
}

// TestSanitizeStripsUserinfoPreservesWhitespace pins that the scan works on the
// raw bytes. An earlier version split on whitespace and rejoined with single
// spaces, which both hid the second URL in a field and rewrote the layout of
// any multi-line error it touched.
func TestSanitizeStripsUserinfoPreservesWhitespace(t *testing.T) {
	in := "dial failed:\n\thttps://u:p@host.example/x\n\tretrying"
	want := "dial failed:\n\thttps://[REDACTED]@host.example/x\n\tretrying"
	if len(in) > maxSafeRun {
		t.Fatalf("fixture is %d bytes; the length pass would collapse whitespace and mask what this asserts", len(in))
	}
	if got := sanitizeText(in); got != want {
		t.Errorf("sanitizeText() = %q, want %q", got, want)
	}
}

// TestSanitizeStripsUserinfoBeforeLengthCheck pins the ordering. The userinfo
// pass has to run before the early return for short strings, or the case it
// exists for — a short URL — never reaches it.
func TestSanitizeStripsUserinfoBeforeLengthCheck(t *testing.T) {
	short := `https://u:p@h.io/x`
	if len(short) > maxSafeRun {
		t.Fatalf("fixture is %d bytes, no longer under the %d-byte threshold", len(short), maxSafeRun)
	}
	if got := sanitizeText(short); strings.Contains(got, "u:p") {
		t.Errorf("sanitizeText() = %q, want the userinfo stripped despite the short input", got)
	}
}

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

type providerError struct {
	secret string
	target error
}

func (e *providerError) Error() string { return "provider response: " + e.secret }
func (e *providerError) Unwrap() error { return e.target }

func TestExternalErrorPreservesErrorAndRedactsOnlyTheLog(t *testing.T) {
	sentinel := errors.New("retry classification sentinel")
	providerErr := &providerError{secret: "short multiword response with token abc123", target: sentinel}
	err := fmt.Errorf("fetch inbox: %w", ExternalError(providerErr, "provider request failed: status 401: response body "+Placeholder))

	if got, want := err.Error(), "fetch inbox: "+providerErr.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, sentinel) {
		t.Fatal("ExternalError broke errors.Is through the provider error")
	}
	var gotProvider *providerError
	if !errors.As(err, &gotProvider) || gotProvider != providerErr {
		t.Fatal("ExternalError broke errors.As through the provider error")
	}

	var buf bytes.Buffer
	newTestLogger(&buf).Warn("sync failed", "err", err)
	out := buf.String()
	for _, secret := range []string{"short multiword response", "abc123"} {
		if strings.Contains(out, secret) {
			t.Errorf("provider-controlled text %q leaked:\n%s", secret, out)
		}
	}
	for _, context := range []string{"fetch inbox", "status 401", Placeholder} {
		if !strings.Contains(out, context) {
			t.Errorf("safe context %q missing:\n%s", context, out)
		}
	}
}

func TestHandleSanitizesURLSecretsInUnmarkedErrorAndStringFallback(t *testing.T) {
	for _, value := range []any{
		errors.New(`Get "https://alice:hunter2@api.example.test/token?code=short-secret": unauthorized`),
		`Get "https://alice:hunter2@api.example.test/token?code=short-secret": unauthorized`,
	} {
		var buf bytes.Buffer
		newTestLogger(&buf).Warn("request failed", "err", value)
		out := buf.String()
		for _, secret := range []string{"alice", "hunter2", "short-secret"} {
			if strings.Contains(out, secret) {
				t.Errorf("URL secret %q leaked from %T:\n%s", secret, value, out)
			}
		}
		if !strings.Contains(out, "api.example.test/token?code="+Placeholder) {
			t.Errorf("safe URL context missing from %T:\n%s", value, out)
		}
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
