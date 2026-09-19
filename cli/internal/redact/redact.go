// Package redact provides a slog.Handler wrapper that scrubs attribute
// values whose keys identify ADR-0001 encrypted fields. This is defence in
// depth: the primary check is the CI grep gate (see
// .github/scripts/encryption-grep-gate.sh); the wrapper catches the day a
// future contributor adds slog.String("subject", msg.Subject) without
// reviewer pushback.
//
// Match policy: attribute KEY equality (case-sensitive). Values at matched
// keys are replaced with Placeholder regardless of type. Groups are walked
// recursively so nested attrs are also scrubbed.
//
// Keys that are *substrings* of sensitive identifiers but legitimately used
// elsewhere (notably "email", which is the account identifier across the
// codebase and is plaintext-by-design per ADR-0001 §3 table) are NOT in the
// set. Use the prefixed form (e.g. "contact_email") when logging a value
// that is actually encrypted.
package redact

import (
	"context"
	"errors"
	"log/slog"
	"strings"
)

// Placeholder replaces sensitive values in log output. The set of keys
// triggering replacement is defined in keys.go (SensitiveSlogKeys) so the
// runtime wrapper and the pre-merge grep gate share one source of truth.
const Placeholder = "[REDACTED]"

// SafeLogError is an error whose full text is needed by callers but contains
// externally controlled data that must not be written to a log. Implementors
// return a safe replacement for their own Error text; Handler preserves any
// trusted wrapping context around it.
type SafeLogError interface {
	error
	SafeLogText() string
}

type externalError struct {
	err      error
	safeText string
}

func (e *externalError) Error() string       { return e.err.Error() }
func (e *externalError) Unwrap() error       { return e.err }
func (e *externalError) SafeLogText() string { return e.safeText }

// ExternalError marks err as containing provider-controlled text while
// preserving its Error string and unwrap chain for control flow and user-facing
// errors. safeText must contain only trusted text suitable for a log.
func ExternalError(err error, safeText string) error {
	if err == nil {
		return nil
	}
	if safeText == "" {
		safeText = Placeholder
	}
	return &externalError{err: err, safeText: safeText}
}

// Handler is a slog.Handler that scrubs sensitive attribute values before
// delegating to the wrapped handler.
type Handler struct {
	wrapped slog.Handler
}

// Wrap returns a Handler that delegates to h, scrubbing attributes at
// sensitive keys (see package doc). If h is nil, Wrap returns nil so the
// caller's existing fallback behaviour kicks in.
func Wrap(h slog.Handler) slog.Handler {
	if h == nil {
		return nil
	}
	return &Handler{wrapped: h}
}

func (r *Handler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return r.wrapped.Enabled(ctx, lvl)
}

func (r *Handler) Handle(ctx context.Context, rec slog.Record) error {
	// Record.Attrs iterates and AddAttrs appends — we can't mutate in
	// place, so rebuild. NewRecord copies Time/Level/Message/PC verbatim.
	out := slog.NewRecord(rec.Time, rec.Level, rec.Message, rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redact(a))
		return true
	})
	return r.wrapped.Handle(ctx, out)
}

func (r *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		red[i] = redact(a)
	}
	return &Handler{wrapped: r.wrapped.WithAttrs(red)}
}

func (r *Handler) WithGroup(name string) slog.Handler {
	return &Handler{wrapped: r.wrapped.WithGroup(name)}
}

// redact returns a copy of a with the value scrubbed if a.Key is sensitive.
// Group-kind values are walked recursively so attrs inside a slog.Group
// are also covered.
func redact(a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		inner := a.Value.Group()
		out := make([]slog.Attr, len(inner))
		for i, sub := range inner {
			out[i] = redact(sub)
		}
		// slog.GroupValue takes Attrs variadic.
		anyAttrs := make([]any, len(out))
		for i, x := range out {
			anyAttrs[i] = x
		}
		return slog.Group(a.Key, anyAttrs...)
	}
	if _, sensitive := sensitiveSlogKeySet[a.Key]; sensitive {
		return slog.String(a.Key, Placeholder)
	}
	// Marked provider errors retain their full text for callers but substitute
	// SafeLogText here. Unmarked errors (and err.Error() logged as a string)
	// still pass through the URL and long-run fallback sanitizer. See ADR-0001
	// §Logging audit.
	if a.Value.Kind() == slog.KindAny {
		if err, ok := a.Value.Any().(error); ok {
			if s := safeErrorText(err); s != err.Error() {
				return slog.String(a.Key, s)
			}
			return a
		}
	}
	if _, isErrKey := errorAttrKeys[a.Key]; isErrKey && a.Value.Kind() == slog.KindString {
		if s := sanitizeText(a.Value.String()); s != a.Value.String() {
			return slog.String(a.Key, s)
		}
	}
	return a
}

// safeErrorText substitutes the safe text of the first marked error in the
// unwrap chain. Standard %w wrappers include the wrapped Error text verbatim,
// so replacing that suffix retains Durian's own operation context while
// removing the provider-controlled portion. A custom wrapper that does not
// include it falls back to the safe text rather than risking disclosure.
func safeErrorText(err error) string {
	var safeErr SafeLogError
	if !errors.As(err, &safeErr) {
		return sanitizeText(err.Error())
	}

	safeText := sanitizeText(safeErr.SafeLogText())
	externalText := safeErr.Error()
	fullText := err.Error()
	if externalText == "" {
		return safeText
	}
	if i := strings.LastIndex(fullText, externalText); i >= 0 {
		return sanitizeText(fullText[:i] + safeText + fullText[i+len(externalText):])
	}
	return safeText
}
