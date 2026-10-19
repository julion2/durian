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
bazel build //app:durian
bazel run //app:durian           # real Durian configuration and encrypted store
bazel run //app:durian -- --demo # explicit bundled fixtures, never a connection-error fallback
```

The application lives in `app/`; `crates/ui` remains its widget library. GPUI dependency,
crate and integration-test names stay unchanged; this is a source-directory move, not a
runtime identity or configuration migration.

Bazel pins Rust 1.99 and resolves the Cargo lockfile through rules_rust/crate_universe; this is
not a shell wrapper around Cargo. The GPUI family remains locked to gpui-pre 0.3.8 and macOS
wry to 0.57.0. Assets are embedded in debug and optimized Bazel builds. The packaged Go
executable is `bazel-bin/app/durian-cli`, beside the GUI `bazel-bin/app/durian`; copy both
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

### macOS application bundle

On a Mac with Xcode, build and open the separate preview application:

```sh
bazel build -c opt //app:DurianPreview
open "bazel-bin/app/Durian Preview.app"
```

Mac builds target macOS **26.0**, matching the Swift app, independently of the installed
Xcode SDK. The automatic `macos` Bazel configuration sets both target and host-tool minima
and `MACOSX_DEPLOYMENT_TARGET`. The Swift renderer receives an explicit, architecture-matched
`-target`; Swift does not use that environment variable as its default target. The bundle's
`LSMinimumSystemVersion` comes from the configured minimum, and packaging rejects any of its
three executables whose Mach-O minimum exceeds it. An SDK 27 build may say `sdk 27.0`, but the
GUI and renderer must still say `minos 26.0`. Do not patch a signed binary or lower only the
plist value to bypass a compatibility error. For a direct Cargo build, set
`MACOSX_DEPLOYMENT_TARGET=26.0` as well.

This is live mail, not the demo. The bundle is named **Durian Preview**, with identifier
`org.js-lab.durian.preview`, and reuses the existing Durian icon. It does not install anything
or replace Swift's Durian/DurianNightly applications. Configuration and the Go mail store are
still shared with Durian; a separate bundle identifier is not a separate mailbox. Quit the
other client before trying it. Calendar provider-write restrictions above still apply.

The bundle contains real copies of `durian`, `durian-cli` and `mailrender`, so Finder does not
depend on Bazel runfiles or a globally installed CLI version. Pkl must still be installed;
the owned CLI inherits PATH with Homebrew and `~/.local/bin` appended for Finder launches.
The native menus expose About, Services, Hide/Quit, new message/event, clipboard/history,
mail/calendar, refresh, window controls and keymap help. Workspace menu entries disable while
an editor, search field or popup owns focus. They add no alternative hardcoded mail shortcuts;
user Pkl bindings remain authoritative. OS shortcuts for Hide/Minimize/Close/Quit and standard
editor shortcuts remain. Closing the last window still quits and cleans up the owned
backend/helper; it does not leave a menu-only process or status-bar item.

Version/build metadata comes from the same Bazel workspace stamp as the Go CLI. The rule signs
copied helpers first and then the application ad-hoc, and runs `codesign --verify --deep --strict`.
This is for local testing, not a notarized distribution or a universal binary. Copy the complete
bundle with `ditto` if moving it; never copy just its main executable. The existing Swift
install/run scripts still build Swift, not this client.

Linux checks cover the portable bundle layout, copied executables/icon, metadata and overwrite
refusal. Finder launch, Dock/application identity, native menu availability, clipboard/Undo,
keymap reload and signing still require Mac acceptance; Linux does not verify those surfaces.

## Read-side behavior

- Profiles, identity, folder queries and remote-image policy come from the existing API.
  Startup overlaps window creation, profiles and the first search; it never persists a mail-list
  snapshot. Identity/profile completion reuses the matching in-flight startup page instead of
  dispatching another copy. Different queries still run immediately; refresh/reconnect invalidates
  the startup answer. A failed startup search shows an explicit retry, not a duplicate request.
  Old servers can fall back to cached/evaluated profile configuration.
- Search runs on the server, with local refinement only for terms the API cannot express.
  Status uses ordinary tags: `tag:unread`, `-tag:unread`, `tag:flagged`. The old `is:` namespace
  is removed, not retained as an alias. The unread-only button and tag completions use the same
  tags that read/unread actions write; read means absence of `unread`, not a separate `read` tag.
  `group:Name`, `group:Name/from`, `group:Name/to` and their `-group:` forms reach Go's existing
  `groups.pkl` expansion unchanged. Names are case-sensitive. Membership is never guessed from
  loaded rows; negation retains backend message semantics, not a new thread-wide exclusion.
  Incomplete tokens wait for the usual idle settle; invalid completed names/directions show
  a search error instead of silently removing the constraint, including on refresh/retry.
  Generations reject obsolete answers. The first page has 50 rows, all with previews. Paging
  fetches the lightweight row prefix and batches only missing/changed previews; already-visible
  content stays in memory. Batches respect the server's 1024-byte query cap and the client's
  unchanged 10 MiB response cap. Superseded searches stop further preview requests. A full
  thread replaces its preview, preserving full HTML and provider-scoped message identity.
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
  Unknown counts remain unknown and transient failures retain the last known values. Searches
  and counts share client admission at eight requests/s, below Go's unchanged ten/s limiter;
  waiting foreground searches go before waiting counts. The first read has no pacing delay.
  Only explicit HTTP 429 search/count responses retry (four total attempts), sharing the
  server's `Retry-After` cooldown (seconds or HTTP date; otherwise 1 s / 2 s / 4 s).
  Admission waits are bounded to 20 s; a longer advertised cooldown is not shortened to retry.
  Superseded count batches cancel while waiting. Other reads and mutations are not paced or
  retried by this mechanism. Large sidebar batches take longer to fill instead of exhausting
  the search budget. Refreshes of unchanged queries coalesce into one follow-up batch rather
  than repeatedly restarting a long batch; the 60 s/manual/SSE triggers remain active.
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

## User keymaps

The client reads the same `keymaps.pkl` as Swift through authenticated `GET /api/v1/keymaps`.
The Go server evaluates the file beside the active `config.pkl` (including `-c`), using the
shared embedded schema; the GUI neither evaluates a second format nor writes configuration.
Use the packaged CLI from this build: older servers without this endpoint show a warning
and keep built-in defaults. Mail loading does not wait for the keymap request; configurable
shortcuts wait for its initial answer so disabled keys cannot briefly execute defaults.

Bindings merge by **key + sorted modifiers + context**, not by action. A new key adds another
binding; remove the old one explicitly with `enabled = false`. Last duplicate wins. Missing
fields use Swift's defaults. Contexts are `list`, `thread`, `search`, `tag_picker`, `calendar`
and `compose_normal`. Multi-key sequences such as `gg`, `dd` and `gi` do not require the
legacy `sequence` flag; exact matches take precedence over prefixes. Counts are capped at
999 and apply only to actions with `supports_count` in that context. Timeout, unmatched
keys, focus/context changes, mouse clicks and reload clear unfinished sequences. Unmodified
character bindings in search/tag text fields do not steal typing.

Reload via **Reload keymaps** in the command menu, or Cmd-Alt-R / Ctrl-Alt-R while no editor
or popup has focus. There is no file watcher yet. Deleting the file restores defaults;
an invalid edit shows a warning and keeps the last working map (unlike Swift's reset to
defaults). `keymaps_enabled = false` suspends configurable bindings, not text editing,
clipboard, Quit or Reload. `?`, command hints and context-menu hints use effective bindings;
shared menus omit shortcuts that differ between contexts.

Cmd-1…Cmd-9 select profiles 1–9 in menu order; Cmd-0 selects profile 10. Without profiles,
the same keys select accounts. Missing slots do nothing. `show_account_1`…`show_account_10`
can be rebound or disabled in Pkl. `show_all_accounts` remains in the command menu but has
no default digit binding; bind it explicitly to Cmd-0 if you prefer the previous behavior.

Icon-only controls expose hover hints; unavailable controls are dim and do not advertise an
active hover. List/thread action menus, tag chips and the header Reply button share the
keyboard actions. Reply/forward on an older message are deliberately unavailable until the
composer can target that exact message; they must not silently act on the latest message.
Inline attachments open the full composer. Snooze and direct sidebar tag/view creation are
not implemented. Save view is session-only; persistent search folders belong in `profiles.pkl`.

Swift defaults replace the prototype's conflicting keys: `a` archives, `R` replies to all,
`f` forwards, `t` opens tags, `l` enters thread mode, `h` returns. `gg`/`G` select the first/last
**loaded** list thread, or focus the first/last message in a thread; repeated `G` scrolls to
the bottom. `n`/`N` navigate actual messages (not timeline events), with clamped counts and
stable message identities across refresh. The target expands and scrolls into view.
Ctrl-D/U move half a viewport.
`gi`/`gs`/`ga`, `g1`–`g9`, `J`/`K` and `gf` navigate profile folders without losing account
scope. `tag_op` and counted `dd` use the existing durable tag-write queue. In Calendar,
`n`/`o`/`O` create, `i` edits, `dd` deletes and `H`/`L` change periods. Existing buttons/menu
actions remain; Cmd-R (also Ctrl-R on Linux) refreshes list, thread or calendar.

`v` selects the inclusive range from an anchor to the cursor; `V` starts individual marks.
In Toggle mode, Space toggles the current mark (keeping at least one) and advances the cursor
without marking the next row. Escape returns to a single cursor. Marked backgrounds and a
separate cursor border distinguish selection from navigation in both list layouts.
Archive, delete, tag operations, read/unread and stars act on the selected IDs through the
existing write queue. Mixed tags/stars are added to all; all-selected tags/stars are removed.
Read/unread marks all read if any are unread, otherwise all unread. Multi-select navigation
does not auto-read threads. This intentionally makes stars a bulk action, unlike Swift's
first-mark-only implementation. Normal reply/forward still address the cursor thread.
Refresh/paging retains explicit marks by ID; changed searches/folders/accounts and opening
thread mode clear them. Right-click selects only the named thread, not other marks.

This is not full Swift action parity. Help lists unavailable bindings explicitly; invoking
one warns rather than substituting an unrelated action. Missing: compose-normal/`exit_insert`, calendar day/time cursor,
`calendar_detail`, `calendar_command` and `calendar_toggle_declined`. Other unknown user
actions are also shown as unavailable. GPUI additions (command/help menus, layouts, appearance,
account selection, calendar views/duplicate and attachment preview) use the same override
mechanism. They cannot shadow a Swift key or sequence prefix. Native macOS key-event/layout
acceptance remains separate from the Linux tests below.

Compose modal editing is deliberately not tied to a Vim engine yet. Key-sequence dispatch
and selection/actions remain separate. `ui::editing::Editor` owns text, anchor/head selection,
Unicode movement, replacement and undo/redo without keys, focus or window state. Selecting a
word then deleting it and directly deleting toward a word boundary use the same edit path;
there is no Vim/Kakoune grammar or new dependency/config format. Normal typing stays default.

The existing multiline editor (compose, inline reply and event notes) now supports Cmd-Z /
Cmd-Shift-Z, plus Ctrl-Z / Ctrl-Shift-Z / Ctrl-Y on Linux. Word movement/selection/deletion
uses Option on macOS and Ctrl on Linux with arrows, Shift-arrows, Backspace/Delete. Unicode
word segments include punctuation; movement skips whitespace before the approached segment.
Each input commit, paste, cut or formatting action is an undo step; no timed typing grouping
yet. History is memory-only, capped at 128 steps / 8 MiB of retained text per editor, and
reset when loading another draft. IME preedit remains visible but excluded from draft text
until committed, and the whole composition is one undo step. Native macOS IME acceptance
remains outstanding. Remaining Swift calendar cursor/detail/command modes are not scheduled
for a parity port; their interaction needs a separate design.

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
VK_ICD_FILENAMES=/dev/null bazel run //app:durian -- --demo
```

## Verification

```sh
bazel test //app/... //integration:gpui_test
bazel test //cli/... //integration:integration_test
bazel test //integration:gpui_ui_test --nocache_test_results
bazel test //integration:gpui_startup_ui_test --nocache_test_results
bazel test //integration:gpui_keymaps_ui_test --nocache_test_results
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
It also starts with 80 configured folder queries, searches and repeatedly refreshes while
badges are filling; every count must finish and no response may be HTTP 429. The backend
contract independently runs 80 counts plus eight foreground searches against Go's real limiter.
Scroll-only cases verify actual body snippets beyond row 50 and after paging beyond row 200,
then repeat after refresh without selecting those messages. The larger fixture has 64 KiB
bodies, so enriching the whole prefix exceeds the response bound. A held paging response also
checks navigation and cancellation of remaining preview requests. Startup shares one `/profiles`
answer between identity and folder scope. Go batches light conversation reads without fetching
HTML/Bcc blobs. Corrupt preview bodies fail the search explicitly instead of silently omitting
their content. The HTTP format is unchanged.
The calendar UI contract injects lost create/delete acknowledgements, rejected edits/RSVP,
failed reconciliation and failed scoped sync against the real Go server. It verifies exact
neighbor preservation, stable create UID, editor/notes retention, native clipboard shortcuts,
midnight end time, own-versus-other conflicts and explicit recovery without repeated writes.
The keymaps UI contract evaluates real Pkl through the owned Go server, then exercises native
overrides/disabled defaults, counts, sequences/timeouts, text/picker/thread contexts, durable
tag operations, reload/error retention, deletion/default restoration and folder navigation.
It also checks range/toggle multi-selection, mixed-state bulk tags/stars/read flags, selection
retention through refresh, bulk archive/delete and counted navigation of distinct messages
sharing an RFC Message-ID. Rendered message positions and bodies are checked together.
Rendered help is checked independently of command execution. All fixtures disable provider
transports; these tests do not send mail.
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

1. Improve useful-mail startup, not just the empty window's first frame. Preview loss beyond
   row 50/paging is fixed and native-tested; measure CLI readiness, metadata, search, preview
   and rendering separately on macOS rather than assuming a cache miss explains the delay.
   Keep mail content in memory only, not in a second persisted list snapshot. Paging still
   re-requests the row prefix, but not unchanged bodies. Broad encrypted search, cursor
   pagination and a compact list-ready API remain work; even a bounded conversation batch
   can exceed the unchanged response byte cap for exceptionally large threads.
2. Design compose modal editing without locking selection/actions to Vim's operator-first
   grammar. The key-independent editing core, multi-selection and message navigation work;
   remaining Swift calendar modes are deliberately not a parity target. macOS keyboard-layout
   acceptance and automatic keymap reload remain.
3. Agree durable calendar recovery across app exit and finish provider/macOS write
   acceptance. Larger durable attachments require extending the Go draft storage contract,
   rather than using temporary paths or pretending an oversized draft was saved.
4. Add settings reload, notifications, account/auth management and complete message-header inspection.
   Existing Swift managers and tests are the reference for behavior, not a second backend.
5. Finish Linux distribution installation packaging and Wayland acceptance. Exercise macOS
   packaging/WebKit on macOS separately; Linux tests cannot establish native macOS behavior.

The imported prototype's historical design/API notes live under `docs/`; this README describes
the integration. Runtime demo assets/fixtures came from the private prototype and include
Linear-derived material. Review provenance and replace private/demo data before public release.
