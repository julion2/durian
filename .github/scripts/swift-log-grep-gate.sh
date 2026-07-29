#!/usr/bin/env bash
# Swift log grep gate — ADR-0001 §6 (Logging discipline), GUI side.
#
# The Go-side encryption-grep-gate.sh guards cli/. This is its counterpart
# for macos/: os_log output goes to the unified log store (readable via
# `log stream`, captured by sysdiagnose), and Log.swift forces
# `privacy: .public` on the composed message — so any mail *content*
# interpolated into a Log.* call is recoverable by the same forensic-image
# attacker (ADR-0001 Persona 1) that subject/body encryption defeats.
#
# Scope: mail CONTENT and correspondents (subject, snippet, body, sender
# address, recipient lists). Attachment filenames and mailbox names are a
# separate, lower-grade class tracked with the attachment-cache work and are
# deliberately NOT gated here yet.
#
# Two-stage filter (mirrors the Go gate):
#   1. Match any line in macos/ that is a Log.debug/info/warning/error call
#      AND contains a sensitive token.
#   2. Drop lines carrying an explicit `// encgrep:allow <reason>` annotation.
#
# Any remaining hit fails the build. To allow a line, add the annotation with
# a one-line reason. When you need to see a sensitive value while debugging,
# use Log.sensitive(...) (privacy: .private) instead — redacted in release
# and in `log stream`, visible only when a debugger is attached.

set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"

# subject/snippet = encrypted subject + preview; \.body = message/draft body;
# \.from / from= = sender address interpolation; draft\.(to|cc|bcc) =
# recipient-list interpolation. Counts (draft.to.count) must be bound to a
# local first so they never appear on a Log line — see EmailSendingManager.
TOKENS='subject|snippet|\.body[^A-Za-z]|\.from|from=|draft\.(to|cc|bcc)'

# Anchor the token match to the line CONTENT (after the `:lineno:` prefix
# grep -rn prepends) so Swift filenames never false-positive.
hits=$(grep -rnE 'Log\.(debug|info|warning|error)' macos/ \
  | grep -E ":[0-9]+:.*($TOKENS)" \
  | grep -vF 'encgrep:allow' \
  || true)

if [ -z "$hits" ]; then
  echo "swift-log-grep-gate: OK — no unannotated leaks found."
  exit 0
fi

cat >&2 <<EOF
swift-log-grep-gate: FAIL

The following Log.* calls interpolate mail content or correspondents that
ADR-0001 marks as encrypted at rest, into the unified log store. Either:

  - remove the sensitive value from the log call (log ids/counts instead), or
  - use Log.sensitive(...) for a debugger-only, release-redacted channel, or
  - add \`// encgrep:allow <one-line reason>\` if the match is a false
    positive (e.g. the user's own account email).

Offending lines:
EOF

echo "$hits" >&2
exit 1
