# Manual two-party LIVE mail slice

**This sends real mail. Do not run concurrently with another OAuth refresh or
calendar/mail harness, particularly the Microsoft refresh worker.** Normal tests
do not include the `demointegration` Go build tag; the Bazel target is `manual`.

## Live result, 2026-10-19: one direction verified, one delivery unresolved

The first live public-flow run passed Google → Microsoft, including encrypted
sync, HTTP search/thread/body, account ownership and attachment SHA-256. The
reverse direction was accepted and appears in Graph Sent, but did not arrive
in Gmail within 90 seconds. Two subsequent private read-only inspections found
no matching received Gmail message (including Spam/Trash), no remaining Graph
draft and no matching non-delivery notice in the inspected Graph folders.
No resend was attempted. This is NOT a passing bidirectional delivery test;
the cause is still unknown. Evidence is under `.amp/in/artifacts/mail-live/`.

One subsequent, distinct Gmail → Outlook HTML message passed the real sync and
HTTP readback, including Unicode filename, HTML/plain projection and exact bytes
of a 51-byte text file and a 768 KiB binary file. Outlook delivered it to Junk,
not Inbox. Gmail Sent retained a third, zero-byte attachment, but Outlook's raw
received MIME and attachment API both omitted it; this is an observed provider
delivery difference, not a Durian parser exception. The first readback exposed a
test-guard bug rejecting Graph's OData folder-delta cursor. After fixing that
narrow GET path, readback of the **same** message passed without another send.

## Preconditions and commands

Use only the two dedicated, quiescent demo mailboxes. Disable mailbox forwarding
and do not edit provider drafts in another client during a run. The boundary
checks submitted recipients, not provider-side forwarding/routing rules.

The private environment must already provide:

- `DURIAN_GOOGLE_TEST_EMAIL`, `DURIAN_GOOGLE_TEST_CLIENT_ID`,
  `DURIAN_GOOGLE_TEST_CLIENT_SECRET`, `DURIAN_GOOGLE_TEST_REFRESH_TOKEN`.
- `DURIAN_MS_TEST_EMAIL`, `DURIAN_MS_TEST_CLIENT_ID`, `DURIAN_MS_TEST_TENANT`,
  `DURIAN_MS_TEST_REFRESH_TOKEN`.

Both exact emails are authenticated through userinfo before any mail operation.
Use mail-capable consent; Microsoft needs the existing adapter's combined
calendar/mail scopes. Microsoft token rotation is persisted through the existing
`amp secrets set` adapter, so that command must be available and authorized.
No local config, keychain, CLI server, or externally running Durian is needed.
Never paste credentials into the commands or logs.

Offline safety tests and compilation only (no live test execution):

```sh
bazel test //cli/internal/demotest:demotest_test
bazel build //cli/internal/demotest:demotest_live_integration_test
```

After credential refresh has finished, from the repository root, run **once**:

```sh
bazel build //cli/internal/demotest:demotest_live_integration_test
DURIAN_MAIL_LIVE_TEST=1 \
  ./bazel-bin/cli/internal/demotest/demotest_live_integration_test_/demotest_live_integration_test \
  -test.run='^TestLiveMailPublicFlow$' -test.count=1 -test.timeout=6m -test.v
```

Running the built binary directly inherits the private environment without
passing credential values through Bazel test action arguments/cache. Do not use
test repetition or automatic reruns. A failed/timeout run requires private
provider inspection before deciding whether a new run is appropriate.

`TestLiveHTMLPublicFlow` sends one additional Gmail → Outlook message. To inspect
an already-sent HTML case without enqueueing or sending, set
`DURIAN_MAIL_READBACK_MARKER` to its exact `durianlive…` run marker and run only
that test. The marker is not a credential; it must match the original test case.
Readback mode refuses a bidirectional run and asserts zero sender invocations.

## Exercised path and bounds

One modest Unicode subject/body message with a deterministic 51-byte UTF-8
attachment per direction: `google-demo` → `microsoft-demo`, then reverse. Each
direction has its own random explicit idempotency key and a 90-second total
delivery/readback deadline; the second direction never runs after a failure.

1. Real loopback HTTP `POST /api/v1/outbox/send` and `GET /api/v1/outbox`.
2. Real five-second `OutboxWorker.Start` loop with `Mail.SenderFor`, preserving
   durable provider Message-ID persistence and send classification. An extra
   test-only single-attempt/context gate prevents any sender re-invocation.
3. Actual Gmail/Graph senders using the **same** identity-verified `*Mail` as
   every receiving and attachment backend. No raw-send shortcut or testseeder.
4. Real `syncengine.Sync` in download-only mode into a temporary encrypted
   `store.DB` with a random, memory-only master key. Both returned errors and
   `Result.Errors` must be empty. Cursors use isolated `XDG_CACHE_HOME` and
   provider-specific demo suffixes; Graph sync selects Inbox and Junk, Gmail ALL.
   Ordinary sync passes are capped at 20 messages (Graph page size is a provider
   soft-limit exception), with 30-second pass deadlines and a five-minute run
   context. No engine watcher or production server is involved.
5. Real handler HTTP search → thread → body → attachment readback. The receiving
   row must have the correct account and provider remote reference, not just a
   worker-created local Sent row. Unicode content and envelope are checked;
   downloaded attachment SHA-256 must equal the independently fixed input hash.

Successful messages and any provider drafts left by a failure are **preserved**.
No remote deletion, reconciliation requeue, invitation, inferred-recipient reply,
or large upload session is enabled. Temporary local state is removed on exit;
there is deliberately no recoverable local key/outbox after the test process
exits. On uncertainty, use the dedicated mailboxes privately, not an automatic
resend. Output contains only fixed stage diagnostics and direction numbers;
production logs are discarded to avoid IDs, subjects, addresses or token leaks.

## Guard extension and deferred capture

Graph flag synchronization uses `POST /v1.0/$batch`. The only allowed exception
is 1–20 unique numbered subrequests, all exact `GET
/me/messages/<safe-id>?$select=id,isRead,flag`, without bodies, headers,
dependencies or extra fields. Exact HTTPS host checks still apply. Offline tests
exercise real `graphbackend.FetchFlags` chunking and reject mutations, `/users`,
foreign recipients, mixed unsafe batches, expanded reads and host bypasses.

Opaque Graph cursor reads also permit the provider's exact
`/me/mailFolders('safe-id')/messages/delta` spelling. This does not allow arbitrary
OData predicates, alternate users, percent-encoded paths or mutations.

**Mail capture/replay is deferred.** The existing `calendartest.Recorder` is
calendar-JSON-specific: it cannot preserve MIME faithfully, retains subjects/IDs,
and has no per-demo-message mailbox-content gate. Wiring it around mailbox
enumeration could retain unrelated third-party content. This slice therefore
does not record provider responses. A follow-up needs an explicit mail allowlist,
strict run-marker/ownership filtering, MIME sanitization and offline replay;
current offline tests use synthetic transports only.
