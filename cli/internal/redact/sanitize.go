package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// maxSafeRun is the longest contiguous non-whitespace run logged verbatim in
// an error value. Raw IMAP/SMTP server responses echoed into errors (protocol
// literals, quoted headers, Subject fragments per ADR-0001 §Logging audit)
// appear as long unbroken runs; our own error templates and library messages
// ("connection reset by peer", "context deadline exceeded") do not.
const maxSafeRun = 80

// errorAttrKeys are attribute keys whose STRING values are treated as error
// text (some call sites log err.Error() as a plain string rather than the
// error itself). Error-typed values are caught regardless of key.
var errorAttrKeys = map[string]struct{}{
	"err":   {},
	"error": {},
}

// sanitizeText replaces every whitespace-delimited field longer than
// maxSafeRun with its byte length and a short SHA-256 prefix, keeping short
// fields — our own context, library errors — readable. Returns s unchanged
// when nothing exceeds the limit, so normal diagnostics are untouched (zero
// blast radius on the common path).
//
// The replacement is one-way ON PURPOSE. An earlier version base64-encoded
// the field, which reads as redaction but is a single `base64 -d` away from
// the plaintext — against the forensic reader this package exists to defend
// against, that is no protection at all. The digest prefix keeps what makes
// the log useful (two occurrences of the same literal are still recognizable
// as the same) without keeping the content.
//
// Limitation, documented in ADR-0001 §Logging audit: content shorter than
// maxSafeRun, or spread across whitespace, is not caught. The heuristic
// targets raw server-echoed data, not arbitrary Subject text — the latter is
// stopped at the source by the grep gate + key-based redaction above.
func sanitizeText(s string) string {
	s = stripURLUserinfo(s)
	if len(s) <= maxSafeRun {
		return s
	}
	fields := strings.Fields(s)
	changed := false
	for i, f := range fields {
		if len(f) > maxSafeRun {
			sum := sha256.Sum256([]byte(f))
			fields[i] = fmt.Sprintf("[redacted %dB sha256:%s]", len(f), hex.EncodeToString(sum[:4]))
			changed = true
		}
	}
	if !changed {
		return s
	}
	return strings.Join(fields, " ")
}

// stripURLUserinfo removes the credentials from any URL embedded in error text.
//
// The length heuristic above does not reach these. A URL carrying userinfo is
// often well under maxSafeRun — "https://alice:hunter2@imap.example.com/x" is
// forty characters — so a connection error quoting the URL it dialled logged
// the password verbatim. Length is the wrong signal here anyway: what makes
// userinfo sensitive is its position, not its size.
//
// Deliberately hand-rolled rather than url.Parse: error text carries trailing
// punctuation ("...: connection refused"), wraps URLs in quotes, and truncates
// them, none of which parse. This scan only needs to find the authority and
// whether it has an "@", and it leaves anything it cannot read alone.
//
// It walks the raw string rather than whitespace-separated fields, and keeps
// going after each match. Splitting on whitespace saw only the first URL in a
// field, so a comma-joined pair — "https://ok.example,https://u:p@host/x" —
// left the second one's credentials intact; scanning the raw bytes also means
// tabs and newlines survive byte for byte, where a Fields/Join round trip
// would collapse them to single spaces.
func stripURLUserinfo(s string) string {
	const marker = "://"
	if !strings.Contains(s, marker) {
		return s
	}

	var b strings.Builder
	emitted := 0 // everything before this offset is already in b, or is the prefix
	for i := 0; ; {
		rel := strings.Index(s[i:], marker)
		if rel < 0 {
			break
		}
		authority := i + rel + len(marker)
		end := authority + authorityEnd(s[authority:])
		// Last "@" wins: a password may itself contain one.
		at := strings.LastIndex(s[authority:end], "@")
		if at < 0 {
			i = authority
			continue
		}
		b.WriteString(s[emitted:authority])
		b.WriteString(Placeholder)
		b.WriteByte('@')
		emitted = authority + at + 1
		i = emitted
	}
	if emitted == 0 {
		return s
	}
	b.WriteString(s[emitted:])
	return b.String()
}

// authorityEnd returns the offset of the first byte in s that cannot belong to
// a URL authority: the path, query and fragment delimiters, whitespace, and the
// characters RFC 3986 excludes from userinfo entirely.
//
// The set is deliberately narrow. Comma, semicolon, apostrophe and parentheses
// are sub-delims — legal, unencoded, in a userinfo — so ending the authority at
// them made a password containing one invisible: the scan stopped before the
// "@", found no userinfo, and logged the whole credential.
//
// Nothing is lost by dropping them. Two URLs run together are still separated
// by the "/" inside the following "://", which ends the first authority; the
// loop then finds the next marker on its own.
func authorityEnd(s string) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '/', '?', '#', ' ', '\t', '\n', '\r', '"', '<', '>', '[', ']', '{', '}':
			return i
		}
	}
	return len(s)
}
