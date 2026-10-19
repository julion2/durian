# Guarded native mail demo session (manual, test-only)

This executable lets the real GPUI app use the production HTTP routes with
**only the two already-authorized dedicated Gmail/Outlook demo identities**.
It is not a production `durian serve` option. Bazel marks the executable and
its composition library `testonly`; the executable is also `manual`.

## Safety and limitations

- **Launch only under the parent's explicit live authorization.** Construction
  calls `demotest.NewFromEnv` exactly once. This verifies both exact identities
  and uses the existing token sources, including durable Microsoft refresh-token
  rotation. Do not launch alongside another mail/calendar live test, session,
  or refresh process. The shared source serializes refreshes **within this
  process**, not across processes; the parent must serialize all live work.
- Required inherited variables: `DURIAN_MAIL_LIVE_TEST=1`,
  `DURIAN_GOOGLE_TEST_EMAIL`, `DURIAN_GOOGLE_TEST_CLIENT_ID`,
  `DURIAN_GOOGLE_TEST_CLIENT_SECRET`, `DURIAN_GOOGLE_TEST_REFRESH_TOKEN`,
  `DURIAN_MS_TEST_EMAIL`, `DURIAN_MS_TEST_CLIENT_ID`,
  `DURIAN_MS_TEST_REFRESH_TOKEN`, `DURIAN_MS_TEST_TENANT`.
  Never put their values in command arguments, transcripts, or local files.
- There is no keychain/config credential lookup or unguarded factory fallback.
  HTTP on-demand backend construction uses `BackendFor`; the actual outbox
  worker uses `SenderFor`. Recipient, MIME, draft ownership and operation guards
  remain unchanged. Keep both dedicated mailboxes quiescent and free of
  third-party automatic forwarding/routing.
- **Opening the session does not send mail.** An explicit compose/reaction via
  the native client can enqueue a real send, after the normal undo delay. The
  production outbox worker runs every five seconds. Never blindly resend an
  uncertain action; inspect the private outbox through the authenticated API
  while the session is still alive.
- Graph inferred-recipient reply drafts and large attachment upload sessions
  remain unsupported. Remote mailbox mutations (flags/moves/deletes etc.) stay
  blocked by the guard; local tag edits are not uploaded. No production IDLE,
  engine watcher, tag-sync or calendar-sync process is started.
- Calendar integration is **not supported** here. Account calendars are disabled
  (reads return conflict); valid event-sync requests return `503 calendar sync
  unavailable`. There is no fake successful calendar sync.

## State and bounds

`--output-dir` must name a **new** directory under a private, trusted parent.
Existing directories and symlinks are rejected before identity/token work.
The session creates it as `0700`. Config, profiles and connection files are
`0600`. Do not use a shared/untrusted parent, repository-tracked path, or portal.

The directory retains a mail SQLite store using the production encrypted-column
schema with a freshly generated, memory-only keyring. Keys are never persisted;
**retained encrypted content cannot be reopened after exit**. Some operational
metadata and addresses remain plaintext by the production schema's design.
The production contacts schema has no encryption API, so contacts are instead
isolated entirely in RAM (seeded with just the two demo identities), never an
unencrypted contacts file. Sync cursors are also RAM-only. This deliberately
does not claim whole-database or contacts-at-rest encryption.

`config.json` contains only synthetic settings, named signatures, account
identities and provider names, **no credential values**. `profiles.json` exposes
both accounts as Gmail (`gmail-demo`) and Outlook (`outlook-demo`). The isolated,
initially absent `keymaps.json` uses client defaults. Only files inside the new
root may be edited for synthetic settings/profile/signature tests; no existing
`config.pkl`, keymaps, profiles or keychain is read or written.

Before readiness and every 30 seconds, sequential download-only engine passes
cover Gmail All Mail and Outlook Inbox/Junk/Sent. Each account has a hard outer
30-second context bound, including recovery; batch and ordinary per-folder
limits are 20. Graph page sizes are provider-controlled soft limits and snapshot
recovery may exceed the ordinary message cap; time limits remain effective.
Both initial passes must succeed before readiness. Later failures emit only
`SYNC_FAILED`; cached data remains available, not silently reported as fresh.

Total lifetime, including identity verification/startup, defaults to 10 minutes;
`--duration` accepts 1 second through 30 minutes. SIGTERM/SIGINT cancel the session.
Shutdown cancels HTTP/SSE/provider requests and active sends, joins worker/sync
goroutines before closing stores, and gives HTTP shutdown five seconds of grace.
As with production, cancellation during delivery can mean **unknown outcome**.
All library and HTTP logs are discarded; stdout contains only nonsecret status.

## Build and offline verification

These commands do not refresh tokens or start a live session:

```sh
bazel build //cli/cmd/demo-session:demo-session //cli/cmd/durian:durian
bazel test //cli/cmd/demo-session:demo_session_test \
  //cli/internal/handler:handler_test //cli/internal/contacts:contacts_test \
  //cli/internal/demotest:demotest_test //cli/cmd/durian:durian_test
```

The session test injects `demotest.New` with fake token callbacks and a strictly
offline provider transport, but uses the real Gmail/Graph backends, engine,
SQLite stores, production HTTP router and worker. It never calls NewFromEnv.

## Parent-owned launch and connection

The following are **instructions, not an instruction to run live during coding**.
`DEMO_SESSION_DIR` is a new absolute path under a private parent;
`GPUI_BIN` is the already-built native executable. Credential variables above
must already be securely inherited. Keep shell tracing off.

```sh
set +x
DURIAN_MAIL_LIVE_TEST=1 bazel-bin/cli/cmd/demo-session/demo-session_/demo-session \
  --output-dir "$DEMO_SESSION_DIR" --duration 10m
```

In an Amp orb, use a supervised service instead of a detached shell process;
do **not** publish a portal for the mail API:

```sh
amp orb service start demo-mail --command \
  "exec env DURIAN_MAIL_LIVE_TEST=1 bazel-bin/cli/cmd/demo-session/demo-session_/demo-session --output-dir '$DEMO_SESSION_DIR' --duration 10m"
```

Wait for the nonsecret `READY` line. Unlike production's owned-server handshake,
this line intentionally contains **no token**. Only then read
`$DEMO_SESSION_DIR/connection.json` privately. It contains `MAIL_BACKEND`,
`MAIL_TOKEN`, `pid` and protocol `api: 1`; never cat it into a transcript. The
listener binds only IPv4 loopback, with the same Host and bearer-token checks as
production. `--port 0` (default) selects an unused port. `/api/v1/version` retains
the stamped production `{version, commit}` response shape.

In a separate launch wrapper on the **same host**, connect without putting the
token in argv or displaying it:

```sh
set +x
MAIL_BACKEND="$(jq -er .MAIL_BACKEND "$DEMO_SESSION_DIR/connection.json")" \
MAIL_TOKEN="$(jq -er .MAIL_TOKEN "$DEMO_SESSION_DIR/connection.json")" \
  "$GPUI_BIN"
```

GPUI's external-backend mode does not own/start/stop this server. Stop the
supervised service with `amp orb service stop demo-mail`, or SIGTERM its confirmed
PID for a foreground launch. `connection.json` is removed on graceful shutdown;
the private root is retained for the parent and must be deleted when finished.
Do not reuse it or treat a leftover file after an abrupt kill as live readiness.
