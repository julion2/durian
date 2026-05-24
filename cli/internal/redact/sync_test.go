package redact

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestGrepGateTokensInSync asserts the bash grep-gate
// (.github/scripts/encryption-grep-gate.sh) carries every key from
// SensitiveSlogKeys in its TOKENS regex. Drift between the runtime
// allow-list (this package) and the pre-merge tripwire was H1 of the
// post-step-8 ADR-0001 audit — mailbox + account names were encrypted at
// rest, the redact wrapper had a stale legacy spelling (mailbox_name),
// the grep-gate didn't include the new keys at all, and production
// IMAP code logged them to serve.log unredacted for three months.
//
// If this test fails, the bash script needs updating. The test prints
// the exact TOKENS='...' line to paste into the script.
func TestGrepGateTokensInSync(t *testing.T) {
	scriptPath := findGrepGateScript(t)
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read grep-gate script %s: %v", scriptPath, err)
	}
	got, ok := extractTokensRegex(string(data))
	if !ok {
		t.Fatalf("could not find TOKENS=... in %s", scriptPath)
	}
	have := make(map[string]struct{}, strings.Count(got, "|")+1)
	for _, t := range strings.Split(got, "|") {
		have[t] = struct{}{}
	}

	var missing []string
	for _, k := range SensitiveSlogKeys {
		// The grep-gate's TOKENS use substring match against the source line;
		// any key whose literal string isn't covered by *some* entry in the
		// regex is a drift. We require the key itself (or a strict prefix
		// of it) to be present so the grep actually fires on a slog.String("key", ...).
		if !tokenCovered(k, have) {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Errorf("grep-gate TOKENS regex is missing entries for keys: %v", missing)
		t.Logf("Add the missing tokens to %s — replacement line:\nTOKENS='%s'",
			scriptPath, suggestedTokensLine(have, missing))
	}
}

// tokenCovered reports whether the grep regex `have` carries an entry
// that would match the literal key string on a source line. Equality
// is the common case; we also allow strict-prefix matches because
// e.g. "mailbox" in the regex still grep-matches a slog call that
// names "mailbox_name".
func tokenCovered(key string, have map[string]struct{}) bool {
	if _, ok := have[key]; ok {
		return true
	}
	for tok := range have {
		if strings.HasPrefix(key, tok) {
			return true
		}
	}
	return false
}

// extractTokensRegex pulls the single-quoted value of TOKENS=... from a
// bash script. Tolerates surrounding whitespace and inline comments.
func extractTokensRegex(script string) (string, bool) {
	re := regexp.MustCompile(`(?m)^TOKENS='([^']+)'`)
	m := re.FindStringSubmatch(script)
	if len(m) != 2 {
		return "", false
	}
	return m[1], true
}

// suggestedTokensLine builds a TOKENS='...' line that includes the
// current `have` set plus the missing keys, deduplicated and sorted by
// declaration order in SensitiveSlogKeys (so the printed regex stays
// readable rather than alphabetized into nonsense).
func suggestedTokensLine(have map[string]struct{}, missing []string) string {
	out := make([]string, 0, len(have)+len(missing))
	for tok := range have {
		out = append(out, tok)
	}
	out = append(out, missing...)
	// Dedup preserving first appearance.
	seen := make(map[string]struct{}, len(out))
	deduped := out[:0]
	for _, t := range out {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		deduped = append(deduped, t)
	}
	return strings.Join(deduped, "|")
}

// findGrepGateScript locates the bash grep-gate relative to the test
// binary. bazel runs tests from a sandbox; we walk up looking for a
// .github directory. The repo layout is fixed so a short walk suffices.
func findGrepGateScript(t *testing.T) string {
	t.Helper()
	// Start from the test source file location — under bazel this resolves
	// to the runfiles tree; outside bazel it's the repo path.
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(here)
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(dir, ".github", "scripts", "encryption-grep-gate.sh")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skipf("grep-gate script not reachable from %s — likely running under sandboxed test runner without repo access", here)
	return ""
}
