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
