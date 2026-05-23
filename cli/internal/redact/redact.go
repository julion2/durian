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
	"log/slog"
)

// Placeholder replaces sensitive values in log output.
const Placeholder = "[REDACTED]"

// sensitiveKeys is the registry generated from ADR-0001 §3 (Encrypted fields).
// Adding a new encrypted column means adding its idiomatic slog key here.
var sensitiveKeys = map[string]struct{}{
	// messages
	"subject":   {},
	"body":      {},
	"body_text": {},
	"body_html": {},
	"snippet":   {},
	// addresses (encrypted in addrs_key)
	"to":        {},
	"from":      {},
	"cc":        {},
	"bcc":       {},
	"reply_to":  {},
	"recipient": {},
	"sender":    {},
	// headers
	"header_value": {},
	// drafts / outbox
	"draft":      {},
	"draft_json": {},
	// contacts (note: "email" alone is intentionally NOT redacted — it is
	// the account identifier elsewhere; use "contact_email" for contacts.)
	"contact_email": {},
	"contact_name":  {},
	// meta_key columns
	"mailbox_name": {},
	"flags":        {},
}

// IsSensitive reports whether key would be redacted by Wrap-wrapped handlers.
// Exported so tests and tooling (e.g. the grep-gate allow-list audit) can
// query the registry without poking at internals.
func IsSensitive(key string) bool {
	_, ok := sensitiveKeys[key]
	return ok
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
	if _, sensitive := sensitiveKeys[a.Key]; sensitive {
		return slog.String(a.Key, Placeholder)
	}
	return a
}
