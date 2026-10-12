# From prototype to durian's UI: plan after the first live run

Consolidated from four read-only analyses (2026-10-10): HTML rendering + sandbox, durian
backend performance + profiles, real data vs. demo assumptions (108 threads), client startup.
Mail content was never logged; samples were deleted.

## What we learned

**Startup.** The client is not the bottleneck for the list: mapping 200 rows costs ~3 ms.
The time goes to (1) the connect starting only after the window exists, (2) a serial
`/version` + a 50-thread sent sample used only to guess identity, (3) durian's search:
every `tag:` query full-scans `messages` through a correlated `EXISTS`
(`store/search.go:790`) – 40 ms warm, 1.5 s cold – while driving from the tag index takes
~3 ms. Window bring-up is ~150 ms warm (more on the first launch of a new binary).

**HTML.** The network block holds (40+ vectors, zero hits; JS off). Gaps: link hrefs reach
`open_url` unchecked (`file:`, custom schemes), no client-side sanitizer, no navigation
policy in the helper, no timeout or height cap, and snapshots taller than 8192 pt render
blank (GPU atlas limit). Quality suffers from the fixed 600 px viewport (mobile breakpoints
fire, wide tables clip), blocked remote images without placeholders, no base stylesheet,
and a light-only bitmap without selection or find. durian's own GPUI draft PR #429 already
embeds a live WKWebView in a GPUI window (`browser_mac.rs`); PR #427 has a sanitizer
(`document.rs`, html5ever allowlist + CSP); the Swift client has a dark-mode transform.

**Real data.** Outlook replies re-attach the quoted original's inline images while durian
strips the quote from the HTML → orphaned `cid` parts get listed (eQMS: 11 chips for 1
attachment; 12 of 296 messages). Text bodies are raw text (CRLF, `____` rules, `[cid:…]`
placeholders), not Markdown. durian already strips quotes and signatures server-side.
Calendar invitations arrive empty (durian drops the `text/calendar` part). System tags
were partly wrong; subjects can start with `Re:`/`AW:` or be empty.

**Profiles.** Defined in `profiles.pkl` (name, accounts, color, folders); no API exposes
them – the macOS client reads the pkl itself.

## Plan

### 1. Client correctness (no open decisions)
- inline `image/*` parts with a content id count as embedded even when the (stripped)
  HTML no longer references them; listed only when referenced but not inlinable
- show the thread first, inline images after; fetch them in parallel
- plain-text renderer for text bodies (CRLF, rules, `[cid:…]`, linkified URLs) instead of
  Markdown; quote splitter stays as a fallback; `hidden_signature` folded with `quoted`
- correct system tags; display subject without reply prefixes, "(no subject)"
- `has:` not refined locally on rows that aren't loaded
- link allowlist (https, http, mailto) + fragment links; helper: navigation policy,
  per-job timeout + worker reload, HTML size cap; tall snapshots in ≤ 8192 pt strips
- client-side sanitizer stage (PR #427 `document.rs`) before any renderer
- calendar threads with an empty body: an invitation placeholder until the backend sends
  the part

### 2. Startup to ≤ 400 ms
- connect starts on a thread at the top of `main()`, parallel to window bring-up
- first request is the inbox search itself; identity from `/profiles` (backend) or cached
- helper spawned at start; calendar after the first paint
- optional: encrypted local snapshot of the list rows for instant first paint (decision)

### 3. HTML architecture
- target: live WKWebView embedded in the GPUI window (port of PR #429), sanitized, base
  stylesheet, real width, dark-mode transform, selection/find; snapshots remain for
  previews / Linux
- remote images: policy is a decision (see below)

### 4. Backend (durian)
- `tag:` → `m.id IN (SELECT message_id FROM tags WHERE tag=?)` (~5 lines; 100–500 ms → ~10–20 ms)
- indexes `messages(thread_id, date DESC)`, `tags(tag, message_id)`; `cache_size`/`mmap_size`
- `/threads`: batch tags/attachments (N+1), gzip
- `GET /profiles` (with `load_remote_images` and other client settings); optional `profile:`
  query field. Until then the client evaluates `profiles.pkl` with the `pkl` CLI and a copy of
  the schema, cached by content hash (durian needs pkl anyway: pkl-go runs `pkl server`). A
  pure-Rust evaluator (`pkl_core`?) would drop the CLI – only if it handles `modulepath:`
  imports and `amends`; `/profiles` makes it moot
- expose the calendar part of invitations; `referenced` on attachments
- later: list-ready rows, keyset cursor, `/changes?since=`, remote-image proxy, indexed
  `in_reply_to`/`refs` lookups (sync holds the single connection for seconds today)

### 5. Scale
- sidebar counts in one pass (today 27 passes per render), cached `visible()`
- free-text refine without lowercasing every body per call; `to:` without `contacts()`
- coalesce notifications from the live source (loading states as events)

## Decisions (owner, 2026-10-10)

- **Order:** client first. Backend changes later as PRs from a fresh clone of julion2/durian.
  The draft GPUI PRs #426–#433 will likely be abandoned in favor of this client (the owner
  decides; nothing is closed from here). Mind the open stability series #417–#424: #422
  touches `NonScrollingWebView` and quote readability, #421 quoting, #419 the store – backend
  PRs and the WebView port build on them, not against them.
- **No local list snapshot.** Startup speed comes from the backend fix and from connecting
  in parallel with window bring-up; no mail content on disk.
- **Remote images follow durian's existing setting** `load_remote_images` (default false,
  read by the Swift client from durian's settings; no API yet). Off: clean placeholders.
  On: the WebView may load images.
- **Renderer:** finish part 1 on the current pipeline, then port the live WKWebView (PR #429).

## Status (2026-10-10, evening)

Done: part 1 (client correctness), the client side of part 2 (startup), profiles (interim via
pkl + cache), part 5 (scale). Measured warm against the real mailbox: window ~180–300 ms,
start folder 214–460 ms (scoped to the default profile), open thread +60–150 ms, its HTML
~0.8 s. What still varies is durian's search (200 ms – 1 s for the same query: the `tag:`
full scan) – the first backend PR.

Next: part 3 (live WKWebView, port of PR #429), then the backend PR series (part 4).

## Status (2026-10-10, night)

Part 3 done: live WKWebView (port of PR #429) with app cursor, link status line, the app's
context menu, overlays, dark mode; sender avatars like the Swift app (Gravatar / Brandfetch,
never for own addresses, memory only). Backend PR 1 prepared in the fresh clone:
`perf/search-tag-index` – `tag:` via `m.id IN (SELECT … idx_tags_tag)`, plan test fails on
the old clause, all 31 CLI packages green.

## Status (2026-10-10, late)

Part 4 (backend) merged: tag search via the index (#434), `/profiles` (#436), page-first
search with a statement cache (#437), `/threads` 5.6× faster plus the quote-cut fix (#438),
`/settings` (#439), invitations kept at ingest and caught up on by sync (#440). Compression
(zstd) dropped: serve is loopback-only, where it made every request slower.

Client side of #439/#440:
- HTML follows `load_remote_images` from `/settings` (servers without it: off). Threads shown
  before the setting arrived are fetched again when it differs. The "remote images blocked"
  note appears only when an image was blocked.
- The invitation card works live: the message carries its event; the card prefers the
  calendar's copy when that day is loaded (calendar, latest answer), else the mail's. The
  day strip and conflicts only for loaded days; cancellations struck through without
  answers; one-occurrence updates say so. Answering stays read-only until intents exist.
- Checked end to end against durian main on synthetic data (isolated HOME, port 9724).
