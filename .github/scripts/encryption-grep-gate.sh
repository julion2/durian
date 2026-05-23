#!/usr/bin/env bash
# Encryption grep gate — ADR-0001 §6 (Logging discipline).
#
# Pre-merge tripwire that fails CI if log/print statements appear to leak
# values from columns ADR-0001 marks as encrypted (subject, body_*,
# from_addr, to_addrs, cc_addrs, draft_json, encrypted contact addresses).
#
# Two-stage filter:
#   1. Match any line in cli/ containing slog./log./fmt.Print AND a
#      sensitive token.
#   2. Drop lines that carry an explicit reviewer-checked annotation
#      `// encgrep:allow <reason>`, and skip the redact package itself
#      (which deliberately names these tokens to scrub them).
#
# Any remaining hit is treated as a real leak and fails the build.
# To allow a line, add the annotation with a one-line reason explaining
# why the match is a false positive or plaintext-by-design per the ADR.

set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

# Tokens drawn from ADR-0001 §3 encrypted-columns table (line-substring match).
TOKENS='subject|body_text|body_html|from_addr|to_addrs|cc_addrs|email|draft_json'

# The redact package legitimately names every sensitive token in its
# registry, its test fixtures and its package documentation.
EXCLUDE_PATHS='cli/internal/redact/'

hits=$(grep -rnE 'slog\.|log\.|fmt\.Print' cli/ \
  | grep -E "$TOKENS" \
  | grep -vE "$EXCLUDE_PATHS" \
  | grep -vF 'encgrep:allow' \
  || true)

if [ -z "$hits" ]; then
  echo "encryption-grep-gate: OK — no unannotated leaks found."
  exit 0
fi

cat >&2 <<EOF
encryption-grep-gate: FAIL

The following lines log or print values whose source column is encrypted
per ADR-0001 §3, and do not carry an \`// encgrep:allow <reason>\`
annotation. Either:

  - remove the sensitive value from the log call, or
  - add \`// encgrep:allow <one-line reason>\` if the match is a false
    positive (e.g. account email, filename, user-facing TUI).

Offending lines:
EOF

echo "$hits" >&2
exit 1
