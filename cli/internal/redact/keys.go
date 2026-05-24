// Sensitive slog attribute keys — the single source of truth for both the
// runtime redact handler (this package) and the pre-merge encryption-grep
// gate (.github/scripts/encryption-grep-gate.sh). The grep-gate's TOKENS
// regex is required to cover every entry here; TestGrepGateKeysInSync
// asserts the bash script is up-to-date with this list.
//
// ADR-0001 §Logging audit calls this out as the SSOT pattern: a new
// encrypted column means appending its idiomatic slog key here once, and
// the build fails until the grep-gate's regex is updated to match. Two
// parallel allow-lists drifting was H1 of the post-step-8 audit (mailbox
// + account names were encrypted at rest but logged plaintext to
// serve.log because the wrapper's registry held mailbox_name /
// contact_email while the production code used mailbox / account).

package redact

// SensitiveSlogKeys enumerates every slog attribute key whose VALUE must
// be scrubbed before delegation. Adding a new encrypted field means:
//
//  1. Add the idiomatic key here.
//  2. Run `bazel test //cli/internal/redact/...` — if the grep gate's
//     TOKENS regex doesn't already cover it, the test fails and prints
//     the updated TOKENS line to paste into the bash script.
//
// Keys that are intentionally NOT redacted (because the column is
// plaintext-by-design per ADR §3): the message-side address fields
// (from / to / cc / bcc / reply_to / recipient / sender), contacts.email
// under the bare "email" key, RFC-5322 ids (message_id / in_reply_to /
// refs), dates, sizes, activity booleans, and the integer FK columns
// (mailbox_id / account_id). Use prefixed forms (contact_email,
// mailbox_name) when logging a value that IS encrypted to keep the
// no-redact set unambiguous.
var SensitiveSlogKeys = []string{
	// messages
	"subject",
	"body",
	"body_text",
	"body_html",
	"snippet",
	// headers
	"header_value",
	// drafts / outbox
	"draft",
	"draft_json",
	// contacts (note: "email" alone is intentionally NOT redacted — it is
	// the account identifier elsewhere; use "contact_email" for contacts)
	"contact_email",
	"contact_name",
	// meta_key columns (mailbox/account names encrypted in step 6+, the
	// non-boolean flags subset encrypted in flags_other)
	"mailbox",        // mailboxes.name — encrypted post-step-6 (audit H1)
	"mailbox_name",   // legacy spelling, kept for back-compat
	"account",        // accounts.name — encrypted post-step-6 (audit H1)
	"account_name",   // explicit form
	"dest",           // IMAP move-destination mailbox (audit H1)
	"trash",          // resolved trash mailbox name (audit H1)
	"archive",        // resolved archive mailbox name (audit H1)
	"folder",         // generic mailbox/folder alias
	"synthetic_id",   // embeds mailbox + account in the synthetic
	"flags",          // full IMAP flags string includes encrypted flags_other
}

// sensitiveSlogKeySet is the set form built once at init from
// SensitiveSlogKeys. Lookup is O(1) on the hot path in Handler.handle.
var sensitiveSlogKeySet = func() map[string]struct{} {
	out := make(map[string]struct{}, len(SensitiveSlogKeys))
	for _, k := range SensitiveSlogKeys {
		out[k] = struct{}{}
	}
	return out
}()

// IsSensitive reports whether key would be redacted by Wrap-wrapped handlers.
// Exported so tests and tooling (e.g. the grep-gate sync test) can query
// the registry without poking at package internals.
func IsSensitive(key string) bool {
	_, ok := sensitiveSlogKeySet[key]
	return ok
}
