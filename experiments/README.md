# GPUI experiments

> **Spike, not a product.** Two throw-away Rust prototypes of the Durian
> three-pane mail UI (folders · thread list · thread detail) built on Zed's
> [GPUI](https://github.com/zed-industries/zed/tree/main/crates/gpui) to see
> what the framework can do on Linux. The Kit experiment now reconstructs the
> existing macOS app, using sample mail by default. Compare with the Qt/QML
> spike in `linux/`. Neither replaces the production clients.

| Directory   | Stack                                                                  | What it shows                                                       |
| ----------- | ---------------------------------------------------------------------- | ------------------------------------------------------------------- |
| `gpui/`     | raw `gpui` + `gpui_platform` (git pin to Zed `916fc2b8`)               | hand-rolled layout with `div()`, `uniform_list`, SVG icons, theming |
| `gpui-kit/` | [`gpui-kit`](https://github.com/longbridge/gpui-kit) 0.6.4 (crates.io) | macOS-style grouped mail list, avatars, continuous thread cards, separate plain-text composer, resizable panes |
| `mail-webview/` | Wry 0.57 + Tao 0.37 | separate full-CSS HTML preview: WKWebView on macOS, WebKitGTK on Linux/Wayland |
| `mail-cef/` | CEF 154.4.0 (Chromium), offscreen | retained Linux inline-HTML prototype; macOS builds no longer require this helper |

Both share the data model and HTTP client in `gpui/src/data.rs`
(`gpui-kit` includes it via `#[path]`).

## Design exploration

The current direction is a native reconstruction of the Swift client in
`gpui-kit/`, using `docs/screenshot-light.png`, `docs/screenshot-compose.png`,
and the Swift views as references. It keeps the three-pane layout, pinned/month
groups, visible message bodies, indented own messages, and separate composer.

Superseded browser design studies are not part of this native prototype stack.
They are not the current design direction or implementations of the Rust UI.

## Run

```bash
cd experiments/gpui-kit
cargo run --locked              # deterministic sample mail; no server needed
cargo run --locked -- --live    # opt in to an already-running local durian serve
```

Use `./run-desktop.sh` to build the optional standalone WebKit helper and, on
Linux only, the embedded Chromium renderer. macOS embeds the system WKWebView
directly in GPUI; it no longer downloads, bundles, or starts CEF. Linux builds
also need `libwebkit2gtk-4.1-dev` for the standalone preview. Running only
`cargo run` does not build those helpers. The first Linux CEF build downloads
roughly 300 MiB and expands to over 1 GiB of runtime files.
`DURIAN_CEF_HELPER` can point to an already-built Linux helper. CEF stays out of
GPUI's library loader and main-thread event loop.

The raw experiment remains available with `cargo run` in `experiments/gpui`;
it uses its earlier server/seed fallback and keyboard bindings.

First build compiles GPUI itself (~15 min on a laptop, a few GB in `target/`);
incremental builds take seconds. Linux needs Vulkan + Wayland or X11 headers
(`libvulkan-dev libwayland-dev libxkbcommon-dev libx11-xcb-dev` on Debian).

PDF previews need Poppler: `sudo apt-get install poppler-utils` on Debian or
`brew install poppler` on macOS. On a minimal Linux desktop, native Save dialogs
also need `xdg-desktop-portal`, `xdg-desktop-portal-gtk`, and a session D-Bus.
Use `gpui-kit/run-desktop.sh` to supply a session bus when the desktop has none.
Install the Rust toolchain and optional preview runtime packages explicitly;
these experiments are not built by the repository's Bazel targets.

## Keys (Kit)

| Key            | Action                                                   |
| -------------- | -------------------------------------------------------- |
| `j` / `k`, ↑/↓ | next / previous thread in list; scroll in focused thread  |
| `g g` / `G`    | first / last thread in list; top / bottom in reader       |
| `Enter` / `l`  | focus reader                                             |
| `h`           | return focus to list                                     |
| `n` / `N`     | next / previous message                                  |
| `J` / `K`      | next / previous folder                                   |
| `/`            | search across folders in a popup (up to 25 results)      |
| `t`            | filter/create/toggle tags in a popup (demo edits only)   |
| `o`            | choose an attachment from the focused message           |
| `v`            | switch focused message between safe HTML and plain text |
| `Shift+V`      | full HTML in a separate WebKit window; Escape closes it  |
| `Alt+V`        | focus embedded HTML; Tab/Shift+Tab links, Enter confirms |
| `Ctrl+A/C`, `Cmd+A/C` | select/copy HTML while embedded browser has focus |
| `Esc`          | dismiss top dialog first; otherwise return to the list  |
| `r`            | open reply to focused message in a separate window      |
| `ctrl-r`       | reload folder (Kit)                                      |
| `c`            | new plain-text message in a separate window              |
| `ctrl-shift-t` | toggle light / dark theme                                |
| `ctrl-w` / `cmd-w` | close composer (Linux / macOS), confirm if edited     |
| `Tab` / `Shift-Tab` | move between compose fields                          |
| ↑/↓, `Enter`, `Esc` | navigate, accept, dismiss popups                     |
| ←/→, `Esc`, `ctrl-s` / `cmd-s` | PDF pages, close preview, Save as…        |
| `j` / `k`, ↑/↓, Page Up/Down | scroll within a tall attachment preview   |
| `ctrl-q`       | quit main window only when no other windows are open     |

Threads are newest-first, with all message bodies visible and selectable.
Click the recipient line to show full addresses. The sample includes a
16-message conversation with a long newest message. No external avatar
services are contacted. There is no app login or user-profile UI.

**Read-only:** archive, delete, pin, and mark-read show an explicit prototype
notice. Selection does not mark mail read. Compose allows editing recipient,
Cc/Bcc, subject, and plain text, but Send is disabled and drafts are not saved.
Closing an edited composer asks whether to discard, including sender-only edits.
Live mode loads configured From identities; demo mode retains its sample sender
without reading real config. This is not yet a functional mail editor:
rich-text editing, draft persistence, and sending are omitted.
Demo folders filter their seed messages by the same tags as the API. Live mode
is capped at 200 loaded threads and is not a scalability benchmark.

### Draft/outbox groundwork (not connected to the composer yet)

`gpui-kit/src/delivery.rs` exercises the existing `/local-drafts` and
`/outbox/send` contracts. It saves a send intent before enqueueing, freezes its
content and idempotency key across uncertain responses and restarts, and records
**queued**, never **delivered**. Draft-load is read-only; retry requires an
explicit action. Partial drafts round-trip without parsing away unfinished
recipient input. GPUI records use a versioned schema and `gpui-` ID namespace;
Swift/IMAP drafts are not imported or overwritten.

Tests use disposable loopback HTTP sinks, including lost responses, failed saves,
Unicode/JSON size boundaries, and recovery of pending sends. They do not exercise
SMTP. The composer still has no write path: authenticated API access,
UI recovery/error states, single-window ownership per draft, and actual
backend-to-SMTP integration remain work to do. The backend draft endpoint has no
revision check, so this is not a concurrent multi-client editing protocol.

Sender discovery in `gpui-kit/src/accounts.rs` uses the existing
Swift/Qt `config.pkl` source (`XDG_CONFIG_HOME`, otherwise `~/.config`). Pkl emits
only a name/email projection; GPUI neither decodes the full config nor queries
Keychain/OAuth status. This adds no backend endpoint. Config order matches
Swift; unlike the CLI, it does not reorder `default=true` accounts. The reader
uses `PKL_EXEC` or the installed Pkl CLI, an embedded `Config.pkl` schema, a
10-second process deadline and a 64 KiB output bound. Diagnostics that could
quote config secrets are not surfaced. Tests use synthetic accounts with
unevaluated, throwing secret fields.

The live From picker loads asynchronously and starts with no demo identity.
The first configured account initializes an unedited composer. Empty/error
states offer Reload senders; reloading returns focus to To so the disappearing
reload button cannot strand keyboard focus. Completion does not steal focus or
overwrite a manually edited reply recipient. Tab reaches the native account
menu, arrows select, Enter accepts, and Escape restores trigger focus. Send
remains disabled; selecting an account makes no backend write.

### HTML, attachments, and popups

- HTML messages use embedded WKWebView on macOS and the Chromium helper on
  Linux, including sender CSS and same-message CID images. Remote content stays
  blocked. Plain text (`v`) and the standalone WebKit preview remain available.
  While CID attachments load, the native safe-subset renderer displays text.
- Image (PNG/JPEG/GIF/WebP, first frame), UTF-8 text, and PDF attachments open in
  separate preview windows. Other types can be saved. Downloads are limited to
  20 MiB; raster previews to 8192×8192 / 64 MiB decoding budget; text previews to
  100,000 characters. PDF subprocesses time out after 20 seconds; Poppler is not
  an isolated security sandbox. Save refuses to overwrite existing files.
- Search covers all demo folders and full message text, or calls the live API
  after a 300 ms debounce. Enter opens the selected thread; Escape returns to
  the folder. Tags can be created/toggled for the current demo thread only and
  are not persisted. Live tags remain read-only.
- To/Cc/Bcc offer up to eight contact suggestions after two characters. ↑/↓
  selects; Enter or Tab accepts; Escape dismisses. Each row has a local initials
  avatar, not a downloaded contact photo. Existing recipients are
  excluded across fields. Demo contacts are synthetic; `--live` uses the
  contacts endpoint. Live API calls require local `durian serve --no-auth`;
  authenticated API connections are not implemented.

The Design Review sample contains HTML with a CID diagram, a deliberately
blocked tracking image, and PNG/TXT/two-page PDF attachments. It is the fixture
for preview checks; the other sample threads are not an attachment catalogue.

### Native macOS HTML embedding

`gpui-kit/src/browser_mac.rs` embeds a real WKWebView child in the GPUI window
through Wry. It uses the shared mail sanitizer, CSP, exact CID image map and
Swift dark transform. Only the mail body is WebKit; navigation, message cards,
search and composer remain GPUI. No CEF process or framework is needed on macOS.

The native child's visible rectangle follows the GPUI content mask, and its
document offset follows clipping in the continuous thread list. Mail shortcuts
return to GPUI while selection and links use the browser adapter. Because
AppKit children appear above GPUI's Metal surface, known dialogs, notifications
and registered popovers temporarily use a one-shot WK snapshot in GPUI; closing
the overlay restores the live child. This is not continuous bitmap streaming.

This remains an experimental adapter, not a drop-in native composition layer.
GPUI Kit 0.6.4 does not expose all context-menu/tooltip visibility, so those
surfaces are not covered by the snapshot bridge. Clicking between multiple
HTML bodies also needs explicit message-selection integration; use Durian's
message-navigation keys to select the target before message-scoped commands.
The macOS runner could not establish actual trackpad/wheel traversal across
message boundaries or capture the complete native window. Do not infer those
checks from successful selection/copy or standalone WebKit tests.

The final single follow-up on 2026-10-08 used the exact instrumented source
(`browser_mac.rs` SHA-256
`1573a20c8b8882628fa1722d3d8eae94730a6080a171d81631a178dd2d25ad98`).
The private `cargo build --locked` passed in **165 seconds with two jobs**
using the diagnostic profile overrides below. The native Swift helper compiled.
An earlier Mac suite hit `WouldBlock` in a loopback Delivery fixture's
`read_line`. Accepted test sockets now explicitly clear inherited nonblocking
mode. The final Mac suite passed **36 tests, 0 failed, 2 ignored**;
all eight Delivery tests also passed 30 parallel-suite repetitions on both
Linux and macOS (240 per platform). Production send remains disabled.

The previous diagnostic run found native slash arriving in GPUI as `a`
(`Archive`), zero uniquely labelled Search AX buttons, and unproven HTML focus.
GPUI reconstructs printable keys from physical keycodes and the active layout;
the old Unicode-payload/keycode-0 probe was incompatible with that path. The
probe now inverts `UCKeyTranslate` on the active layout for slash, query and
shortcut keys, preferring ordinary unshifted/Shift ANSI positions. It never
changes the system layout or posts global events. Unsupported layouts fail
before clipboard mutation. This harness correction does not change production
routing. In the final run, slash was VK26 + Shift and reached GPUI as `/`.

**Verified:** Root-Slash and WK→Search burst each retained and copied the exact
13-character synthetic query. WK was ready, visible and the actual native first
responder before the burst. Both paths passed Escape, HTML copy after Escape
and dark-theme change, and normal Ctrl+Q exit 0. Startup traces had no key/action
contamination. **Still unverified:** AX reported the Search text field but not
its editor focus; no uniquely labelled Search button was available for AX Press.
Keyboard behavior is therefore verified independently, not full accessibility
acceptance. No retry was performed. Full clipboard Item/Type/Data restoration,
own-process exit and private-source/cache/runtime cleanup were confirmed.

**Orb-only accessibility follow-up (no further Mac run):** two separate causes
were found in the exact Cargo.lock releases; downloaded crate checksums matched
the lockfile.

- **Command names are app-owned.** Durian's icon-only `command` helper set a
  tooltip, not an accessible name. GPUI Component 0.6.4 derives button names
  from `accessibility_label` or the visible `label`, never the tooltip
  ([implementation](https://github.com/longbridge/gpui-kit/blob/3c387ae0a3e9b14ee39fe98be2b51a882800aa16/crates/component/src/button/button.rs#L694-L697)).
  The helper now uses the existing label for both. On an isolated Linux
  Xvfb/D-Bus session, AT-SPI found zero named Search buttons before the fix and
  exactly one afterward; its `click` action opened the `Search all mail` entry.
  `cargo build --locked` and tests passed (32 passed, 2 ignored). This verifies
  the shared name and action path, not the new macOS AXTitle/AXPress result.
- **Editor accessibility focus is library-owned.** Component `Input` attaches
  `Role::TextInput` and the name/value to an outer frame tracking a private
  `frame_focus_handle`, not the input state's actual keyboard focus handle
  ([frame](https://github.com/longbridge/gpui-kit/blob/3c387ae0a3e9b14ee39fe98be2b51a882800aa16/crates/component/src/input/input.rs#L619-L691)).
  The inner `InputBaseState` tracks the real handle with `id("input-state")`
  but no accessibility role
  ([editor](https://github.com/longbridge/gpui-kit/blob/3c387ae0a3e9b14ee39fe98be2b51a882800aa16/crates/base/src/input/base/state.rs#L4174-L4177)).
  GPUI 0.3.5 only reports focus on an existing node; otherwise its
  [tree builder](https://docs.rs/crate/gpui-pre/0.3.5/source/src/window/a11y.rs)
  falls back to the window root. The Linux run logged `search.focus=true` and
  `focused element ... has an id but no role`; AT-SPI focus stayed false.
  GPUI forwards that `TreeUpdate` to AccessKit macOS. AccessKit's
  [focus resolver](https://github.com/AccessKit/accesskit/blob/c88605b96d04431f9c3c792464a0f2f253480e94/platforms/macos/src/adapter.rs#L248-L263)
  returns no focused child for a window-root focus, consistent with the earlier
  AXWindow result. This gap exists before AppKit, not only in the Mac harness.

No input-focus workaround, dependency patch or version change was made. The
appropriate next step is a GPUI Kit regression test and fix associating the
editable accessibility node with the real editor focus, without duplicating
keyboard-focus handles or breaking suffix/clear-button navigation. Prefer a
verified upstream release; a pinned dependency patch needs separate approval
and platform verification. The orb's extra AT-SPI `EditableText` probe found
that interface unavailable; it did not verify accessible text editing. Native
macOS confirmation of the button fix also requires a separately approved run.

The tested helpers are preserved in `gpui-kit/verification/`. In an agreed Mac
UI-test window, `gpui-kit/verify-macos.sh /tmp/gpui-…/target/debug/durian-gpui-kit`
copies them into private `/tmp/gpui-*` storage, compiles the Swift helper, and
runs only synthetic demo mail against its own PID. It preserves/restores every
clipboard item/type and verifies the restoration; never collect or publish the
private clipboard backup. The diagnostic wrapper has run on macOS; this does
not establish keyboard acceptance. It preserves recovery files on failure and
cleans up on success.

For diagnosis, start with an **existing private demo binary**; only the small
Swift probe needs compiling. From the transferred `experiments/gpui-kit/`:

```bash
bash verify-macos.sh --diagnose /tmp/gpui-existing/target/debug/durian-gpui-kit
```

This starts fresh apps for root slash, the uniquely labelled Search button's AX
Press action (independent of keyboard routing), and a WebKit slash/query burst.
It warms AccessKit before bounded AX tree dumps, checks exact synthetic-query
copy even if AX labels are missing, and attempts Escape/HTML-copy/theme/Ctrl+Q
only after query copy succeeds. Each command targets only its own PID; no wheel,
desktop capture, permission changes, real accounts, or Send. The diagnostic
exit status means collection finished, **not acceptance passed**. Inspect
`diagnosis.json`, labelled AX dumps and logs in the printed `/tmp/gpui-keyboard.*`
directory. Copy only those evidence files; do not share clipboard backups or
the entire working directory. Remove the private directory after confirmed
clipboard restoration and evidence transfer.

An instrumented debug build adds `DURIAN_INPUT_TRACE=1`: native responder/route,
GPUI ingress/resolved action/context, Search open/editor-focus, synthetic probe
match and theme state. HTML traces distinguish missing content, pending CID
images, absent embed/native child, document readiness, visibility/overlay state,
focus-call result and actual native responder. The burst requires both fixture
copy and a ready, visible WK responder; earlier views' traces cannot satisfy it.
Hooks are passive and disabled for `--live` and release
builds; no query, mail or clipboard text is logged. A binary without these hooks
can still run the external controls, but missing traces are **unknown**, not a
failed dispatch. Linux native slash/query/theme checks verified the trace path
and absence of typed query text in logs; they do not verify AppKit routing.

If no usable private binary remains, first package **all** transitive inputs
with `bash gpui-kit/verification/pack-source.sh /tmp/gpui-source.tgz` from this
directory. This includes `DarkModeTransform.swift`, previously missed during
transfer. Extract under `/tmp/gpui-*`; set `CARGO_HOME`, `CARGO_TARGET_DIR`,
`TMPDIR` and Swift module caches there. Reuse an existing private target/cache
only if still available and authorized. For a fresh diagnostic build, these
command-line overrides avoid optimized dependency builds and debug symbols
without changing the repository profile:

```bash
cargo --config 'profile.dev.debug=0' --config 'profile.dev.package."*".opt-level=0' build --locked
```

The first clean Mac diagnostic build with these overrides took **284 seconds**;
this is not a runtime-performance baseline. Do not change profiles on a reusable
cache unnecessarily. Run only in an explicitly authorized UI/build window;
leave the main checkout and all other threads' resources untouched.

### Retained CEF embedding experiment (Linux default)

`mail-cef` paints bounded BGRA frames into GPUI's normal render tree. GPUI owns
message-card geometry, the continuous thread scrollbar, clipping, popups, and
keyboard focus. Only the visible body region is rasterized; a long email does
not allocate a document-sized texture. Mouse selection and focused link keys
are forwarded to Chromium; j/k, search, tags, and Escape stay with Durian.
Copy transfers selected text to the native clipboard. Links ask for native
confirmation without navigating the mail document. Theme changes recreate the
body using the Swift dark transform; images are not inverted.

The helper reuses `mail-webview/src/document.rs` for mail filtering and CSP,
embeds only the parent's bounded CID PNGs as data images, and additionally
cancels non-data resource requests in CEF. No mail HTML file or local web server
is needed. The CEF sandbox remains enabled. This is **not a security audit or
an OS-level network prohibition**. Storage is temporary and browser processes
are closed when their message view is released.

This proves embedding, not production scalability: each opened HTML body owns
a Chromium process group, a substantial RAM cost. A shared browser service,
an idle-browser budget, release packaging/signing, and accessibility-tree
bridging are still needed. The offscreen HTML body does not currently expose
its DOM to native assistive technology; plain text remains native. Oversized
fixed-width mail and displays above 2× need additional work. Sending is still
disabled. No production client or backend was changed.

```bash
cd experiments/mail-cef
cargo build --locked
LD_LIBRARY_PATH="$PWD/target/debug" cargo test --locked  # Linux
python3 verify.py target/debug/durian-mail-cef
# macOS: helper=$(./build-macos.sh); python3 verify.py "$helper"
```

The real-engine verifier covers light/dark × 640/320 CSS pixels at 2×: CSS grid
geometry, unchanged CID colors, Unicode selection, mouse dragging, scrolling
to a known pixel marker, keyboard link activation, normal shutdown, and zero
requests to a reachable tracking/CSS canary. It passes on Linux and macOS under
ordinary load. Linux also has actual GPUI visual and keyboard checks. On macOS,
the earlier CEF-backed GPUI build and launch, embedded subprocess startup, absence of a
separate CEF window, native Alt+V → Cmd+A/C copying of the expected fixture text,
and complete process cleanup were checked; native visual verification is still
limited by screen-capture permissions.

Stress limits remain: during heavy macOS build/link load, one narrow-resize
check failed its grid-pixel assertion despite the renderer acknowledgment;
that run stopped before recording whether later frames recovered. Under full
CPU saturation, shutdown exceeded the verifier's five-second budget; a sampled
stack showed progress through CEF shutdown and subsequent temporary-cache
deletion, not a deadlock. These are unresolved load-related limitations, not
production-readiness claims.

### Real browser HTML spike

`Browser preview…` / `Shift+V` passes the current message and bounded CID PNGs
over stdin to `durian-mail-webview`. Mail is not written to a temporary HTML
file or served over HTTP. The helper owns its own native window and main-thread
event loop. GPUI remains responsive; a WebKit crash does not replace its reader.

**This is a separate window, not inline GPUI integration.** Wry's supported
Wayland path requires an attached GTK container, which GPUI does not provide.
GPUI Kit's `gpui-wry` currently targets macOS/Windows. This spike validates
WebKit and the shared mail policy. Inline embedding is separate: the Kit app's
macOS adapter owns its WKWebView child, while Linux still uses `mail-cef`.

The helper preserves email style blocks, inline CSS, tables, and responsive
layout. It removes active content and external resource attributes; a strict
Content Security Policy blocks external CSS, background images, fonts, scripts,
frames, forms, and network connections. Only same-message images are served by
an exact-key in-memory custom protocol. Storage is ephemeral. This is layered
mail-content protection, **not an OS-level no-network sandbox or a security
audit**. Remote-image opt-in, embedded media, and arbitrary data images are not
implemented.

The preview takes the GPUI theme when opened; reopen after changing theme.
Dark mode reuses the actual JavaScript from the Swift client's
`DarkModeTransform.swift`, without inverting images. Browser selection,
scrolling, arrows and Page Up/Down work normally. Escape / Cmd-W / Ctrl-W close
the window. Activating an external link closes the preview and asks for native
GPUI confirmation before opening the system browser.

```bash
cd experiments/mail-webview
cargo build --locked
cargo test --locked
python3 verify.py                 # native desktop required; binds loopback :9876
cargo run --locked -- --demo      # synthetic CSS/CID fixture, no mail server
cargo run --locked -- --demo --dark --narrow
```

Verified on Linux/Wayland (WebKitGTK 2.50.6) and arm64 macOS 26.6.2 (WKWebView):
three Rust tests and four real-browser states (light/dark × wide/narrow).
Checks assert CSS geometry, CID decoding, selection, scrolling, dark colors,
unchanged image filtering, and CSP violations. A reachable local canary receives
zero tracking/CSS requests. GPUI launch/return/link confirmation and contact
avatars were additionally exercised on Wayland. macOS screenshots were blocked
by the capture environment; synthetic Escape injection was inconclusive, so
full macOS keyboard/GPUI integration is not claimed. Sending remains disabled;
this browser spike does not verify SMTP delivery.

### Linux native-embedding boundary

The preferred mail engine is WebKitGTK, but this is not a drop-in replacement
for CEF in the existing Wayland window. GPUI owns its Wayland connection and
surface; GTK3 cannot import them as a GTK widget parent through its supported
APIs. Calling WebKitGTK directly rather than through Wry does not remove that
ownership boundary. A separate GTK window works but is not inline embedding.

The pinned GPUI exposes `Application::with_platform` and `run_embedded`, so a
GTK-owned host can be implemented outside GPUI without necessarily forking it.
That requires a `Platform`/`PlatformWindow` backend: scene rendering and atlas,
frame scheduling, input and IME, native handles, lifecycle and platform services.
It is not implemented here. Before replacing the Linux renderer, such a host
must prove real GPUI and WebKitGTK content in one Wayland window, clipping and
overlapping popups, keyboard/IME transfer, resize/scale changes and teardown.
The existing Linux CEF prototype remains available while that work is unresolved.

References: [GPUI platform injection](https://github.com/zed-industries/zed/blob/d89e9c2124b2786a390c7a451c7488601b4da2e1/crates/gpui/src/app.rs#L142-L180),
[embedded run loop](https://github.com/zed-industries/zed/blob/d89e9c2124b2786a390c7a451c7488601b4da2e1/crates/gpui/src/app.rs#L249-L269),
[GTK3 Wayland window API](https://gitlab.gnome.org/GNOME/gtk/-/blob/gtk-3-24/gdk/wayland/gdkwaylandwindow.h),
[GPUI Kit's unsupported Linux WebView example](https://github.com/longbridge/gpui-kit/blob/v0.6.4/examples/webview/src/main.rs#L22-L37).

### Checking the Kit experiment

```bash
cd experiments/gpui-kit
cargo build --locked
cargo test --locked
# Requires Poppler and Pkl (pkl on PATH, or set PKL_EXEC).
cargo test --locked -- --ignored  # PDF rendering and safe identity projection
```

Tests cover filtering, grouping, newest-first fixtures, reply addressing,
Unicode quotations, recipient completion, dirty-draft detection, HTML filtering,
CID resolution, attachment decoding, and API response mapping. Native
visual checks use Wayland, Inter, and a 2× display; install Inter for the intended
typography (`fonts-inter` on Debian). The app still runs with font fallback.
Light/dark mode, long threads, message focus, search/empty results, and compose
can be exercised without a server. macOS glass and the full Swift editor are
not reproduced; Linux rendering does not verify macOS integration.

## Notes

- The crates.io `gpui 0.2.2` release predates the `gpui_platform` split; the
  raw spike pins both crates to the same Zed commit instead.
- `gpui-kit` ships its own GPUI snapshot (`gpui-pre 0.3.x`). Never add a
  separate `gpui` dependency next to it — the types will not line up.
- No Bazel integration on purpose; build these crates explicitly with Cargo.
