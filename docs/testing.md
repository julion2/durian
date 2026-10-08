# Reliability testing

Run the repository setup once in a Linux Orb, then use the bounded quality
runner from the repository root:

```sh
.agents/setup
tools/test_quality.sh quick
tools/test_quality.sh stress
```

`quick` runs the existing Bazel gates for all CLI tests, the sync server, the
API contract, and both server builds. `stress` includes `quick`, then runs all
CLI Go packages with the race detector, repeats the loopback TLS IMAP protocol
and outbox concurrency regressions, and fuzzes the adversarial ICS,
sync-engine, and nested MIME targets for 15 seconds each. Independent gates
continue after failures; the final exit status is nonzero if any gate failed or
timed out. Bazel is capped at four jobs, two test jobs, and 6 GiB of local RAM
for a 4-CPU/8-GB Orb.

These commands use temporary databases, fake keychains, in-process HTTP
servers, and loopback TLS fixtures. They do not validate a real provider,
hosted JMAP event delivery, provider throttling, or internet failures. The
quality runner clears live JMAP credentials for both Bazel and direct Go runs.
The full stress suite is also available through the Test workflow's manual
dispatch; ordinary pull requests run the package, API, and native UI gates.

The optional JMAP integration target skips when its three credential variables
are absent. Running it is an explicit live-account operation: it imports,
submits, and cleans up test messages against the configured account.

```sh
bazel test //cli/internal/jmapbackend:jmapbackend_live_integration_test \
  --test_env=DURIAN_JMAP_TEST_SESSION_URL \
  --test_env=DURIAN_JMAP_TEST_USERNAME \
  --test_env=DURIAN_JMAP_TEST_PASSWORD \
  --test_output=all
```

Native UI render and action tests require macOS 26, Xcode, and Ripgrep
(`brew install ripgrep`, installed by CI). Run:

```sh
tools/native_ui_test.sh
NATIVE_UI_SCENE=1 NATIVE_UI_APPEARANCE=both tools/native_ui_test.sh
tools/native_macos_repro.sh
```

The UI commands exercise the actual `ContentView`, `EmailDetailView`,
`ComposeWindow`, and `ComposeForm` in isolated native processes. Real `V`/`j`
keymaps leave one thread marked while moving the cursor to another thread.
It presses that thread's older card's Reply, Reply-All, and Forward controls by
their exact accessibility identifiers. The real `ContentView` callbacks must
fetch that message's original body exactly once and create a draft attached to
the cursor thread, not the marked thread. The original fixture contains an
ancestor quote absent from the card preview, so accidentally quoting the
stripped preview fails the test. The harness presses the real compose Send
button. At the intercepted HTTP boundary
it checks one outbox POST per action, recipients, CC, subject, threading headers,
and the full quote. Default hosted mode checks loading, empty, and 500-message
focus/scroll states and explicitly hosts the resulting compose draft. Scene
mode instead uses a SwiftUI App with the compose `WindowGroup` extracted
verbatim from `DurianApp.swift`: real `openWindow` must open exactly one new
visible compose window for each action. It does not substitute a hosted window.
The production app initializer is excluded to avoid notification authorization
and external CLI startup. `NATIVE_UI_APPEARANCE=both` compiles once, then runs
separate light- and dark-startup processes with bidirectional app-wide appearance
changes on an already-loaded reply window.
All HTTP requests, including singleton side effects, are intercepted; an OS
sandbox additionally denies network access. Missing buttons or unsupported press
actions fail the gate; they are not skipped.

Same-process screenshots need no Screen Recording permission. Override their
destination with `NATIVE_UI_ARTIFACT_DIR`; the default is `.amp/in/artifacts`.
The harness waits for WebKit's complete DOM with the expected ancestor quote,
then composites WebKit snapshots into native bitmaps over a resolved opaque
window background. Controlled app-wide Aqua and DarkAqua captures cover all
three compose actions and both appearance transitions. These bitmaps omit
window toolbar chrome. Quote contrast is calculated from the DOM's computed
foreground and the appearance-resolved production card background; below 4.5:1
fails the gate. The transition regression failed at 2.49:1 (light to dark) and
2.19:1 (dark to light). Reloading pristine quote HTML on effective appearance
changes restores 6.53:1 in dark mode and 5.74:1 in light mode, without retaining
the dark transform's inline overrides after returning to light mode.
Do not treat a passing send assertion as proof of visual correctness.
PR CI runs both hosted and scene gates with pinned Xcode 26.4.1 and uploads
the synthetic screenshots and output.

`tools/native_macos_repro.sh` runs the exact factory, cache, and diagnostics sources
through a Foundation/Combine XCTest subset, typechecks the full app including
its lifecycle delegate, and measures attachment-cache I/O and main-actor progress.
Both scripts isolate temporary files and compiler caches in `stability-*`
directories under `TMPDIR` and remove them on exit. The subset works around a
local Xcode 27 / macOS 26 Bazel XCTest deployment mismatch; it does not replace
the full Swift suite on a matching toolchain.

These are **in-process UI-to-HTTP tests, not full-app or delivery end-to-end
tests**. Full installed-app initialization, account bootstrap, calendar UI,
configured per-window color-scheme overrides, and provider acceptance/delivery
remain outside their coverage. Long-thread and
cache timings are diagnostic samples, not enforced performance budgets. Cache
tests additionally check that the main actor continues to run during large
writes. Real provider interoperability still needs dedicated disposable test
accounts.

Thunderbird's [testing layers](https://developer.thunderbird.net/thunderbird-development/testing/running-tests)
separate component tests from full-window tests and externally controlled UI
tests. Its [test helpers](https://source-docs.thunderbird.net/en/latest/testing/helpers.html)
include local servers, network redirection, OAuth fixtures, and accessibility
checks. Durian's protocol fixtures and native view harness provide the first
layers; they must not be mistaken for the remaining full-app coverage.

The recurrence-zone upgrade deliberately leaves a pending local edit on a
legacy, zoned, end-date-bounded series as a conflict. Old state does not retain
enough information to distinguish a UTC/civil-date conversion from another
client changing the end date. The existing conflict preview and local backup
are safer than an automatic upload; the migration tests check both that backup
and the absence of provider writes. Unchanged files migrate without this gate.

For every defect, first add the smallest deterministic test that fails for the
observed behavior. Prefer a package-level test with disposable fixtures; use a
fuzz seed for parser or state-machine failures, a loopback protocol fixture for
wire behavior, and an in-process native render/action assertion for macOS UI.
Keep the reproduction enabled while implementing the fix, verify it fails on
the broken revision, then run `quick` and the focused reproduction before the
full `stress` suite.

## Nightly diagnostics

Nightly builds (bundle ID ending in `.nightly`) write bounded local timing
diagnostics to
`~/Library/Application Support/org.js-lab.durian.nightly/Diagnostics/`.
Release builds record nothing and create no files. The directory is `0700`;
`diagnostics.jsonl` and its single rotated predecessor `diagnostics.1.jsonl`
are `0600` and each capped at 64 KiB, so at most 128 KiB is retained. Writes
happen on a background queue; when its backlog is full, events are dropped and
summarized as one `diagnostics.dropped` event.

Each line has exactly this closed schema:

```json
{"v":1,"ts":1700000000,"op":"http.thread","outcome":"ok","status":200,"ms":42,"count":1}
```

`op` is a static category (`http.*` routes from the CLI API, `ui.heartbeat`,
`diagnostics.dropped`), and `outcome` is one of `ok`, `cancelled`,
`transport_error`, `http_error`, `decode_error`, `stall`, or `dropped`. Events
never contain free text, mail content, message/thread/account IDs, URLs,
queries, paths, tokens, or error descriptions. `ui.heartbeat` records a `stall`
when the main actor answers an off-main ping more than 250 ms late; it runs
only while the app is active and awake. Inspect the latest entries with:

```sh
tail -n 50 ~/Library/Application\ Support/org.js-lab.durian.nightly/Diagnostics/diagnostics.jsonl
```

The schema, release no-op, rotation, permissions, and heartbeat rules are
covered by `bazel test //macos:nightly_diagnostics_test` on macOS.
