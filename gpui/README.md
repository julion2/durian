# linear-gpui

Native GPUI recreation of Linear's sidebar + "My issues" list, built from values extracted
from the running Linear desktop app (Electron → Chrome DevTools Protocol).

```
cargo run          # Linear: sidebar + My issues
cargo run -- mail  # mail prototype built from the same primitives
```

The mail prototype (`src/mail/`) applies Linear's issue detail, split inbox and command menu
patterns to a keyboard-driven, tag-based mail client: J/K navigate, ↵/F focus mode, Esc back,
E done, S star, ; expand all, counts like `5j`. Data comes from `data/mail.json`.

HTML mail: `helpers/mailrender` (Swift, compiled by `build.rs`) is a helper process with a pool of
offscreen WKWebViews (no JS, no network, non-persistent store). The app sends sanitized HTML,
gets back a 2x PNG plus link rects, and shows the snapshot natively with link hit-boxes.
`src/mail/html_render.rs` keeps a priority wish list (open thread, then 6 ahead / 2 behind in the
direction of travel), drops stale jobs and caches results in `~/Library/Caches/linear-gpui/html`.
Until a snapshot is ready the text/plain part is shown. `MAIL_TRACE=1` logs latency and cache hits.

Composer: inline reply under the thread (r / a), full dialog (c, Forward, pop-out) with recipient
chips + suggestions, Cc/Bcc, Markdown formatting (⌘B / ⌘I / ⌘E, list/quote continuation, toolbar),
⌘↵ to send. Drafts follow Linear: replies autosave per thread, closing a composer with content asks
"Save to drafts?", g d opens the drafts page. Attachments preview with Space (Quick Look) and
download to ~/Downloads.

Testing input end to end: `tools/typekeys.swift` posts real HID key events (US key codes,
`\n` `\t` `\e`, `⌘` chords); combine with `MAIL_TRACE=1`.

## Layout

```
tools/extract/        CDP scripts run against `Linear --remote-debugging-port=9333`
  cdp.mjs             eval / screenshot helper
  rootvars.js         runtime theme tokens (:root custom properties)
  dump.js             computed-style tree of sidebar / header / rows + inline SVGs
  data.js             list rows + sidebar as structured data (scrolls the virtual list)
  hover.mjs, pseudo.mjs   hover states and ::before/::after rings/shadows
tools/build_assets.mjs   design/raw → assets/icons/*.svg (mono, sprite-resolved) + data/my_issues.json
design/raw/           raw extraction output (source of truth for every number in src/)
design/*.png          reference capture vs. GPUI captures

src/
  theme.rs            tokens in Linear's own LCH values, converted to sRGB at startup
  model.rs            fixture types + grouping into "Urgent / Blocking / Other active / …"
  ui/                 primitives: Icon, IconButton, TabPill, Chip, StatusIcon (canvas-painted)
  sidebar.rs          Sidebar entity (active item, collapsible sections)
  issue_list.rs       header + virtualized `list()` with group headers and issue rows
  workspace.rs        window chrome + sidebar + floating content panel
  mail/               model (Mailbox entity), sidebar, thread list (uniform_list), thread view,
                      root with actions + key bindings
```

## Re-extracting

```
osascript -e 'quit app "Linear"'; open -a Linear --args --remote-debugging-port=9333
node tools/extract/cdp.mjs eval tools/extract/dump.js > …   # see scripts for outputs
node tools/build_assets.mjs
```

Notes: GPUI renders `svg()` as a tinted alpha mask, so icon colors live in Rust, not in the SVGs.
Status icons are drawn procedurally (ring + progress pie) like Linear does. Shaders are compiled
at runtime (`runtime_shaders`) so no Xcode Metal toolchain is required.
