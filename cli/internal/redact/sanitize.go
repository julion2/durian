package redact

import (
	"encoding/base64"
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

// sanitizeText base64-encodes every whitespace-delimited field longer than
// maxSafeRun, keeping short fields — our own context, library errors —
// readable. Returns s unchanged when nothing exceeds the limit, so normal
// diagnostics are untouched (zero blast radius on the common path).
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
			fields[i] = "base64:" + base64.StdEncoding.EncodeToString([]byte(f))
			changed = true
		}
	}
	if !changed {
		return s
	}
	return strings.Join(fields, " ")
}
