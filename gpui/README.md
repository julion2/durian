# Durian GPUI

The existing GPUI mail/calendar prototype integrated with Durian's Go backend.
This is an **experimental client with live writes**, not yet a replacement for all Swift features.
Tags, encrypted drafts and durable outbox submission use the Go API. Starting the existing Go
server starts its normal sync/outbox workers; use disposable, provider-disabled accounts for tests.
Calendar details, deletes and RSVP use the exact-UID API, with no legacy reference fallback.
Unconfirmed calendar writes have explicit recovery within the running workspace. The recovery
queue is not persisted across app exit; do not close the app with unresolved calendar changes.
Provider delivery and native macOS write acceptance remain unverified.
Do not use these calendar write paths with real calendars yet.

## Build and run

```sh
bazel build //gpui:durian
bazel run //gpui:durian           # real Durian configuration and encrypted store
bazel run //gpui:durian -- --demo # explicit bundled fixtures, never a connection-error fallback
```

Bazel pins Rust 1.99 and resolves the Cargo lockfile through rules_rust/crate_universe; this is
not a shell wrapper around Cargo. The GPUI family remains locked to gpui-pre 0.3.8 and macOS
wry to 0.57.0. Assets are embedded in debug and optimized Bazel builds. The packaged Go
executable is `bazel-bin/gpui/durian-cli`, beside the GUI `bazel-bin/gpui/durian`; copy both
together when moving the build. Also keep `mailrender` beside them; Linux additionally needs
the sibling `mailrender-extensions/` directory, containing the request-filter library.
Copy actual files, not symlinks back into a build sandbox. Linux builds require GTK3 and
WebKitGTK API 4.1 >= 2.40 development packages (`libwebkit2gtk-4.1-dev` on Debian); installed
apps need the corresponding runtime libraries and WebKit subprocess/sandbox support.

Pkl must be on PATH. On Linux, `.agents/setup` installs the pinned tools and native development
libraries needed in Debian Orbs. Cargo is also supported from this directory:

```sh
cargo test --workspace --locked
DURIAN_BIN=/absolute/path/to/durian cargo run --locked
```

Normal startup selects an explicitly configured `DURIAN_BIN`, the packaged `durian-cli`, a
legacy sibling `durian`, or `durian` on PATH, rejecting the GUI itself. It starts its owned
backend off the UI thread and validates READY API 1, loopback address and authentication token.
Quit kills/reaps only that owned process. An owned server crash triggers at most three
replacement attempts per app lifetime (250 ms, 500 ms, 1 s backoff; each handshake has a 10 s
deadline). Each replacement uses new credentials but retains the workspace, current query,
editors and ordered writer queue. Reads refresh against the replacement; dispatched writes keep
their original client and uncertain sends keep their frozen identity. Initial startup failures
are terminal; exhausted restart budgets leave the editor visible with a server error.

Alternatively, set `MAIL_BACKEND` to a server origin (without `/api/v1`) and `MAIL_TOKEN` to its
bearer token through your environment. External servers are never spawned, restarted or killed
by the GUI. Tokens are not logged. After an external server rotates its token, restart the GUI
with the new token; same-session SSE disconnects reconnect automatically.

## Read-side behavior

- Profiles, identity, folder queries and remote-image policy come from the existing API.
  Startup overlaps window creation, profiles and the first search; it never persists a mail-list
  snapshot. Identity/profile completion reuses the matching in-flight startup page instead of
  dispatching another copy. Different queries still run immediately; refresh/reconnect invalidates
  the startup answer. A failed startup search shows an explicit retry, not a duplicate request.
  Old servers can fall back to cached/evaluated profile configuration.
- Search runs on the server, with local refinement only for terms the API cannot express.
  Generations reject obsolete answers. The first page is 200 rows, with previews for the first
  50; later pages increase the limit without enrichment. A full thread replaces its preview,
  preserving full HTML and provider-scoped message identity.
- Timed response reads interrupted by Linux signals resume on the same connection with the
  remaining deadline. This never replays an HTTP request or retries a transmission; the normal
  20 s request deadline remains. TLS/proxy connection setup has no added retry behavior.
- Full-thread cache: 300 in-memory entries. Tags/timestamps reconcile ordinary searches;
  freshness events invalidate content even if those stamps are unchanged. Old thread, image
  and calendar replies cannot overwrite a newer refresh.
- Authenticated SSE uses one cancellable worker, bounded 64 KiB frames, coalesced hints,
  500 ms debounce with a 2 s burst ceiling, and 250 ms–30 s reconnect backoff. Every connection
  refetches authoritative state because the backend has no replay. A 60 s reconciliation also
  catches dropped hints and external tag changes that do not broadcast SSE. Cmd-R/Ctrl-R is
  an explicit refresh; failures keep the old list visible with an error.
- Sidebar badges use scoped server count queries, not the loaded page or current search.
  Unknown counts remain unknown and transient failures retain the last known values. Only HTTP
  429 retries automatically, with four attempts and bounded 500 ms / 1 s / 2 s backoff.
  On an empty startup list, the initial count batch waits for the first search response so it
  cannot crowd the single SQLite connection before useful mail; search failure also releases it.
- Calendar windows include local calendars. Details and occurrences are scoped by account,
  calendar and UID. An invitation without a calendar matches only an unambiguous occurrence
  in its own account; unloaded days never claim to be conflict-free.
- Decrypted attachments and live HTML snapshots use UID/UUID-scoped private temporary
  directories with lifetime locks, not a persistent mail cache. Startup only cleans abandoned
  sessions; quit removes its own session. Explicit downloads are collision-safe 0600 files
  in Downloads. Cache/config defaults respect XDG on Linux.
- Avatar lookups are external requests (Gravatar/Brandfetch), as in the prototype. Set
  `MAIL_AVATARS=0` to disable them. Pictures remain memory-only and own addresses are excluded.
  The cache holds 256 entries, queues at most 32 requests, and admits four active requests
  across workspace replacements. Old-session results are rejected; already-running HTTP
  requests retain their 8 s timeout. Decoding rejects dimensions above 2048×2048.

## Write behavior and limits

- Thread tags intentionally use global thread identity, without an account qualifier. Optimistic
  changes survive periodic reads until the ordered request completes; failure restores confirmed
  state and refreshes it. Account filtering does not redefine a thread's identity.
- Recipients retain email addresses, not only display names. Explicit save/send commits pending
  To/Cc/Bcc text; ambiguous name-only input must be corrected. Replies capture the original full
  message/account/RFC headers before editing, including through pop-out and draft recovery.
  Mail To/Cc/Bcc suggestions use authenticated `/contacts/search` after 150 ms of idle input,
  keep server usage order and omit already-selected addresses. Each field permits one active
  lookup plus the newest pending query; stale answers cannot replace newer input. Loading and
  failure stay visible without blocking typing. Enter/Tab selects the highlighted canonical
  `Name <email>` result, including when the typed prefix contains `@`; manual addresses remain
  available without a result. Demo mode and calendar guests retain their local suggestions.
- Draft JSON is Swift-compatible and encrypted by Go. The entire recovery envelope is limited
  to 1 MiB, including base64 attachments and frozen send payloads; oversized writes are refused,
  not silently trimmed. Save/delete/send are ordered per draft. Waiting ACKs cannot close over
  newer, unsubmitted editor input. Pending writes are bounded to 128 intents / 8 MiB, four active.
- Enqueue uses a persisted idempotency key and frozen payload. Unknown outcomes remain locked
  for retry of that same request. Only confirmed outbox cancellation restores an editable draft
  and rotates its send key; 404/409 do not mean cancellation. A passed countdown or missing row
  never means delivery. SSE status hints and GET outbox reconciliation distinguish failed,
  reconciliation-required and delivered-with-warning states; provider reconciliation remains
  a Go CLI operation, not an automatic retry.
- Calendar creates allocate a stable UID before submission; edits fetch master details, preserve
  all-day civil dates and reject ambiguous/nonexistent local times. Local saves and scoped provider
  sync are separate acknowledgements. Local-only calendars skip provider sync; remote deletion
  does not offer a fake in-memory Undo. A lost DELETE response retains account/calendar/UID,
  blocks further changes to that identity, and exposes **Recover calendar changes**. Recovery
  checks the original calendar and exact UID before scoped delete sync. If the event still
  exists, it refreshes and asks the user to review it instead of repeating DELETE. Failed reads
  retain recovery; failed sync stays retryable. No PUT/DELETE/RSVP is replayed automatically.
  This queue survives an owned-backend replacement, not a full app restart. Durable recovery
  needs a separately agreed backend storage contract; no plaintext intent journal is written.

## Rendering and Linux

The UI is the existing `crates/ui`, `crates/mail` and `crates/calendar` implementation, not a
replacement web application. macOS retains the native WKWebView integration and its cached-view
overlay lifecycle. HTML still passes through the existing allowlist sanitizer and CSP; remote
images follow the backend setting, default off.

Linux renders sanitized HTML through a separate WebKitGTK snapshot helper, never a GTK widget
embedded inside GPUI. It uses an ephemeral context, disabled cookies/storage, sandboxed web
processes, disabled sender JavaScript and an isolated host measurement script. Every worker's
request filter must acknowledge readiness before jobs start. File/HTTP requests, replacement
navigation, downloads and permission prompts are denied. The shared sanitizer/CSP allows only
HTTPS image loads when configured (including CSS images); external images are off by default.

The unchanged NDJSON protocol admits three jobs, 664 CSS-pixel snapshots and at most four
4000-pixel JPEG strips. Long HTML is visibly clipped at 16000 pixels. Links use native GPUI
hitboxes and desktop opening; snapshots are not selectable HTML. **Show plain text** switches
to the message's selectable text part, with Control+C/Control+A on Linux, and can switch back.
The light HTML canvas is retained inside both shell themes; this is not a dark-mode HTML rewrite.

Plain text stays readable while loading or after failure. The helper has 8 s readiness /15 s
job deadlines; the client independently enforces 10/20 s, including external `MAILRENDER_BIN`
implementations. Failure terminates the owned Linux process group and falls back without
replaying. Missing request filters or attempted sandbox disabling are refusals, not permission
to render unprotected. `.agents/setup` installs the native prerequisites; no Rust dependency
or macOS live-view lifecycle change is needed for this engine.

Verified on Linux X11/Xvfb with WebKitGTK 2.50.6: real table/CSS/CID-image pixels, concurrent
same-path output, four long-document strips, HTTPS positive controls, TLS hostname rejection,
blocked scripts/imports/frames/HTTP downgrades, a real stalled-image deadline, and native
light/dark snapshot/text/copy/link/fallback/quit flows. HTTPS tests use a private mount namespace
with a temporary CA, not disabled TLS or modified system trust. macOS compilation/runtime,
Wayland interaction and older WebKit versions remain unverified here. A live embedded Linux
webview and distribution installation packaging are separate work.

In this Orb's Mesa software stack Vulkan painted black; disabling its ICD selects the working
OpenGL path. This is a test-environment setting, not a forced production renderer:

```sh
VK_ICD_FILENAMES=/dev/null bazel run //gpui:durian -- --demo
```

## Verification

```sh
bazel test //gpui/... //integration:gpui_test
bazel test //cli/... //integration:integration_test
bazel test //integration:gpui_ui_test --nocache_test_results
bazel test //integration:gpui_startup_ui_test --nocache_test_results
bazel test //integration:gpui_calendar_ui_test --nocache_test_results
bazel test //integration:mailrender_test //integration:gpui_html_ui_test --nocache_test_results
```

The contract test runs the actual Rust Client against an authenticated Go server and encrypted
throwaway SQLite data: wrong token, scoped search/counts, full HTML versus preview, duplicate
Message-ID identities, contact email/name-prefix lookup, external tag reconciliation and calendar detail.

The opt-in Linux UI test uses Xvfb, Openbox, xdotool, ImageMagick and Tesseract (installed by
`.agents/setup`). It drives the packaged native app with real keyboard input and asserts its
rendered text, Go CLI state and process cleanup. Cases include live archive/read/unread writes,
search/empty results with unchanged scoped badges, manual and 60 s periodic refresh,
owned-backend reconnecting/recovery/crash-budget exhaustion, real SSE connection, quit cleanup
and explicit startup failure. Further cases cover silent/stalled renderer deadlines, composer
survival across a server restart, delayed draft acknowledgement, Bcc recovery after app restart,
enqueue, confirmed Undo and a second inline reply without reusing the first send's receipt.
Contact cases cover a server-only address, visible loading/failure, recovery and stale-answer
rejection, canonical selection from an email prefix and recipient retention across app restart.
The startup UI contract holds real Go response bodies behind explicit barriers. It checks one
initial request, navigation away/back with pending responses, refresh after a real tag change,
visible failure with manual retry, and an empty inbox releasing sidebar counts. The fixture is
already read, so legitimate mark-read writes/refreshes cannot be mistaken for startup duplicates.
The calendar UI contract injects lost create/delete acknowledgements, rejected edits/RSVP,
failed reconciliation and failed scoped sync against the real Go server. It verifies exact
neighbor preservation, stable create UID, editor/notes retention, native clipboard shortcuts,
midnight end time, own-versus-other conflicts and explicit recovery without repeated writes.
Screenshots/logs go to Bazel's undeclared test outputs;
`DURIAN_UI_ARTIFACT_DIR` overrides that location when running the script directly. These are
synthetic fixtures, not a test of IMAP/SMTP providers or the user's mailbox.

Useful diagnostics: `MAIL_TRACE=1`, `MAIL_APPEARANCE=light|dark`, `MAIL_FOLDER=<name>` and
`MAIL_HTML=snapshot` (macOS). The CLI logs to its normal XDG state directory. Do not publish
real mailbox traces without reviewing them.

## Measured Linux startup baseline

[Benchmark tooling and results](../tools/startup-bench/README.md) cover 10k/100k encrypted
synthetic messages, five warm and five targeted-file-cold runs per corpus. The measured build
is the earlier baseline plus the sender-name crash fix, not this complete integrated revision.
It uses Bazel fastbuild and Xvfb/llvmpipe CPU rendering, not an optimized release or macOS.

The 500 ms useful-mail goal was missed in all 20 runs. Median useful-list times were 964 ms
(10k warm), 1885 ms (10k targeted cold), 1248 ms (100k warm), and 10470 ms (100k targeted cold).
First frame is only a loading shell; body measurements are expanded plaintext, not HTML-complete.
The later broad-search SQL-limit fix is a separate correctness result, not a startup speedup.

A separate **optimized integrated** before/after matrix now covers 40 native launches on an
8-CPU/16-GB Linux orb with the same software renderer. Deferring initial badge counts reduces
100k median useful-list time from 1525 to 1080 ms warm and 7961 to 5248 ms targeted-cold;
10k warm lists remain about 399 ms. Expanded plaintext still misses 500 ms. Separately,
batched encrypted search verification and ASCII-only casing reduce the broad 100k store search
median from 17.870 to 6.848 s and profiling-process peak RSS from 2.09 GiB to 46 MiB. Unicode
uses the original contextual caser and all collision checks remain enforced. See the benchmark
report for exact inputs, full vectors and limits; these are not Mac/hardware-GPU guarantees.

A subsequent 20-run, 100k-only optimized comparison isolates startup request reuse on the
integrated Linux HTML build: median visible list **1285 → 859 ms warm**, **5767 → 5287 ms
targeted-cold**; expanded plaintext **1362 → 980 ms warm**, **5907 → 5389 ms targeted-cold**.
Go and renderer binaries are identical across that pair. The 500 ms goal is still unmet, and
the cold runs still spend several seconds waiting for the first store search. These samples
are separate from the earlier matrices, not a replacement for their observations.

## Remaining parity work, in order

1. Agree durable calendar recovery across app exit and finish provider/macOS write
   acceptance. Larger durable attachments require extending the Go draft storage contract,
   rather than using temporary paths or pretending an oversized draft was saved.
2. Profile and improve startup and broad search on large encrypted stores. The 400–500 ms goal
   remains unmet in the measured Linux baseline. Paging still re-fetches the prefix
   because the API has no cursor; selected-thread priority is not a strict bound on all HTTP work.
3. Add custom keymaps, settings reload, notifications,
   account/auth management and complete message-header inspection.
   Existing Swift managers and tests are the reference for behavior, not a second backend.
4. Finish Linux distribution installation packaging and Wayland acceptance. Exercise macOS
   packaging/WebKit on macOS separately; Linux tests cannot establish native macOS behavior.

The imported prototype's historical design/API notes live under `docs/`; this README describes
the integration. Runtime demo assets/fixtures came from the private prototype and include
Linear-derived material. Review provenance and replace private/demo data before public release.
