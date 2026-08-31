---
title: Architecture
weight: 1
---

Durian is a terminal-first email client with a SwiftUI GUI on macOS and a Qt6 GUI MVP on Linux. This document explains how the pieces fit together so you can navigate the codebase without reading every file.

## Components

```text
┌──────────────────┐       ┌──────────────────┐       ┌──────────────────┐
│  Swift GUI       │       │  Qt GUI (Linux,  │       │  Tag Sync Server │
│  (macos/)        │       │  experimental)   │       │  (sync/, opt.)   │
└────────┬─────────┘       └────────┬─────────┘       └────────▲─────────┘
         │ HTTP                      │ HTTP                      │ HTTP
         │ localhost:9723            │ localhost:9723            │ Tailnet / LAN
         ▼                           ▼                           │
┌────────────────────────────────────────────────────┐           │
│  Go CLI (`durian serve`)                            │           │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌────────┐ │           │
│  │ handler  │ │ watcher  │ │   imap   │ │ store  │ │           │
│  │ (HTTP)   │ │ (IDLE)   │ │  (sync)  │ │(SQLite)│ │           │
│  └──────────┘ └──────────┘ └────▲─────┘ └────────┘ │           │
└──────────────────────────────────┼──────────────────┘           │
                                   │ IMAP/IDLE                    │
                                   ▼                              │
                         ┌──────────────────┐                     │
                         │  Provider IMAP/  │                     │
                         │  SMTP servers    │                     │
                         │  (Gmail, etc.)   │                     │
                         └──────────────────┘                     │
                                                                  │
                         (tag changes pushed/pulled ──────────────┘
                          via `durian tagsync push/pull`)
```

**One backend, many frontends.** The Go CLI is the only component that talks to the mail providers (IMAP/SMTP, Microsoft Graph, Gmail API, JMAP) and owns the SQLite store. Both GUIs are thin HTTP clients to `localhost:9723` — they never touch the DB directly.

## Directory layout

| Path | Purpose |
|---|---|
| `cli/cmd/durian/` | CLI commands (`sync`, `serve`, `auth`, `search`, `send`, `validate`, `contacts`, …) |
| `cli/internal/handler/` | HTTP API handlers + IMAP IDLE watcher (`watcher.go`) + native-backend poll/push watcher (`enginewatcher.go`) + SSE event hub |
| `cli/internal/backend/` | The provider-neutral `Backend` seam: interface, `Capabilities`, `LabelWriter`, neutral `Message`/`Folder`/`Flags`/`Cursor` types |
| `cli/internal/imapbackend/`, `graphbackend/`, `gmailbackend/`, `jmapbackend/` | Provider-neutral backend implementations (IMAP, Microsoft Graph, Gmail REST, JMAP) |
| `cli/internal/syncengine/` | Provider-neutral sync engine: cursor-paged fetch, ingest, three-way flag merge, folder-move + label upload |
| `cli/internal/imap/` | Legacy IMAP syncer (`sync_engine = "legacy"`) + the low-level IMAP client that `imapbackend` reuses |
| `cli/internal/store/` | SQLite schema, FTS5 search, tags, attachments, local drafts, outbox |
| `cli/internal/config/` | Pkl parsing + `durian validate`; `EffectiveSyncEngine` backend routing |
| `cli/internal/oauth/` | OAuth flows (Google, Microsoft); separate Graph-token minting with per-identity caching |
| `cli/internal/calendar/`, `calendarsync/`, `graphcalendar/`, `googlecalendar/` | Calendar: vdir model, two-way sync engine + `CalendarProvider` seam, and its two provider clients |
| `cli/internal/smtp/`, `draft/`, `sanitize/`, `contacts/` | Supporting packages |
| `macos/durian/` | Swift GUI: `Managers/` (state), `Views/` (SwiftUI), `Models/`, `Network/EmailBackend.swift`, `Keymaps/` (vim engine) |
| `linux/` | Qt6/QML GUI (read-only MVP) |
| `sync/` | Optional self-hosted tag sync server |
| `integration/` | Shell-based API contract tests |

## Runtime topology

### The CLI as the one-and-only IMAP client

When the GUI launches, it spawns `durian serve` as a child process (see `macos/durian/Network/EmailBackend.swift`). `durian serve`:

1. Starts an HTTP server on `localhost:9723` (configurable via `--port`).
2. Opens the SQLite store at `~/.local/share/durian/email.db` (or `$XDG_DATA_HOME/durian/email.db`).
3. Starts one IDLE watcher goroutine per configured account (`cli/internal/handler/watcher.go`).
4. Streams `new_mail` and `outbox_update` events to connected SSE subscribers via `cli/internal/handler/events.go`.

The GUI never talks IMAP directly. Every action the user takes in the UI — opening a thread, changing a tag, sending a draft — becomes an HTTP call to the backend.

### Config file ownership

`~/.config/durian/config.pkl` (or `$XDG_CONFIG_HOME/durian/config.pkl`) is **read by both the Go CLI and the Swift GUI**, each with its own Pkl evaluator. Fields land in one of three categories:

- **Go-only** (e.g. `accounts.imap.host`, `sync.tag_sync.url`) — consumed by `durian sync`, `durian serve`, etc.
- **Swift-only** (e.g. `settings.theme`, `sync.gui_auto_sync`) — read directly by `macos/durian/Managers/ConfigManager.swift`.
- **Shared for validation** (e.g. `settings.accent_color`) — Go's `durian validate` checks format before Swift loads.

Pkl schemas enforce structure at eval time, but each side only decodes the fields it needs. Adding a GUI-only field to config.pkl doesn't need a matching Go struct.

### The HTTP API

All endpoints are under `/api/v1/` — see `openapi.yaml` for the full contract. The main categories:

| Category | Examples |
|---|---|
| Reading | `GET /search`, `GET /search/count`, `GET /threads/{id}`, `GET /message/body`, `GET /tags` |
| Writing | `POST /threads/{id}/tags`, `POST /outbox/send`, `PUT /local-drafts/{id}`, `POST /contacts/usage` |
| Real-time | `GET /events` (Server-Sent Events stream with heartbeat) |
| Attachments | `GET /messages/{id}/attachments/{part_id}` (streams raw bytes) |

Integration tests in `integration/integration_test.sh` exercise the contract end-to-end against a real `durian serve` process.

## Storage model

One SQLite file at `~/.local/share/durian/email.db` (or `$XDG_DATA_HOME/durian/email.db`). Schema version is bumped on every migration step (encryption, sync-engine baselines, and data repairs); current version is v30.

- `messages` — one row per local message identity. `stable_id` is the immutable native object identity when a provider has one (JMAP `Email.id`), so JMAP objects remain distinct even with missing or duplicate Message-IDs. `message_id` is RFC 5322 metadata and the compatibility identity for older protocols; that fallback cannot distinguish same-account duplicates. Other plaintext columns: `thread_id`, `in_reply_to`, `refs`, `from_addr`, `to_addrs`, `cc_addrs`, `date`, `created_at`, `size`, `uid`, `account_id` + `mailbox_id` (FKs), `is_seen` / `is_flagged` / `is_deleted` booleans. Encrypted BLOBs: `subject_ct`, `body_text_ct`, `body_html_ct`, `flags_other` (remaining IMAP flags and keywords).
- `tags` — tag join table, one row per (message_id, tag).
- `message_headers` — raw headers used by filter rules (List-Id, Authentication-Results, …). The `value` column is encrypted (`value_ct` BLOB); `name` stays plaintext for SQL filtering.
- `attachments` — per-part metadata. `filename_ct`, `content_type_ct`, `size_ct` are encrypted; `part_id`, `disposition`, `content_id` stay plaintext (needed for fetch correlation with the IMAP server).
- `local_drafts` — crash-recovery drafts kept locally until saved to IMAP. The `draft_json` payload is encrypted (`draft_json_ct` BLOB).
- `outbox` — queued outgoing messages with `send_after` timestamp for undo-send. `draft_json_ct` BLOB; `attempts`, `last_error`, `created_at`, `last_attempted_at`, `send_after` plaintext.
- `provider_tag_mutations` — durable explicit `unread` / `flagged` / `replied` intent for provider-native property patches. Newer intent supersedes an older entry for the same message and tag. A native-patch backend retains the entry until the provider accepts it and the local read model reflects it. On a non-dry-run pass with flag upload enabled, generic backends discard these entries because their baseline merge owns flag synchronization.
- `mailboxes`, `accounts` — operational lookup tables. `name_ct` encrypted (mailbox / account display names are sensitive); integer IDs are the FK targets for `messages.mailbox_id` / `messages.account_id`.
- `messages_blind_fts` — FTS5 virtual table indexing HMAC-blind tokens of subject + body + addresses. No plaintext lives here; see [§Encryption layer](#encryption-layer) below.

### Encryption layer

Sensitive columns are AES-256-GCM encrypted at the application layer via `cli/internal/dbcrypto/` (see [ADR-0001](design/0001-mail-content-encryption-at-rest/)). A 32-byte master in the OS keychain (`durian-db` / `master`) is bootstrapped at `durian serve` start and derives one HKDF-SHA256 sub-key per purpose (subject, body, addrs, headers, draft, meta, contact, fts-token). Sub-keys live in process RAM with a cached `cipher.AEAD` so the hot encrypt/decrypt path stays at ~290 ns/op.

What stays plaintext on purpose: `message_id`, `thread_id`, `date`, `account`, `mailbox`, UID, flags, sizes. These are needed for IMAP sync correlation and for SQLite query planning. ADR-0001 §3 has the exact column-by-column table.

Search runs against `messages_blind_fts`, an FTS5 index built from HMAC-bigram tokens (`cli/internal/dbcrypto/tokenize.go`). The FTS index contains no plaintext — the same token in two different mails produces the same HMAC, and the post-decrypt filter (`cli/internal/store/search_filter.go`) re-checks any FTS hit against the decrypted body to defeat HMAC truncation collisions. Bigram phrase queries work via consecutive-token AND'ing.

Disk hygiene: `PRAGMA secure_delete = ON` is set on every connection (zeroes freed pages on DELETE / UPDATE). On `store.Open`, the freelist is inspected and auto-VACUUM runs if the file is unusually fragmented — covers the Time-Machine-restore-of-an-old-backup case. See ADR-0001 §6 "Disk hygiene".

Search uses notmuch-style query syntax (`tag:inbox AND from:boss@example.com`) parsed in `cli/internal/store/search.go` into SQL + FTS5 MATCH.

## Sync model

Durian talks to mail providers through **one neutral seam**. An
account's `sync_engine` (resolved by `AccountConfig.EffectiveSyncEngine`) picks
the path:

| `sync_engine` | Provider | Path |
|---|---|---|
| `legacy` (default for IMAP) | any IMAP | the classic `imap.Syncer` + IDLE watcher |
| `graph` (default + required for Microsoft) | Microsoft 365 | `graphbackend` + `syncengine` + poll watcher |
| `gmail` (default for Google) | Gmail | `gmailbackend` + `syncengine`, synced on demand |
| `jmap` | Fastmail / JMAP | `jmapbackend` + `syncengine` + EventSource push watcher |
| `engine` | generic IMAP | `imapbackend` + `syncengine` (opt-in) |

### The Backend seam

`cli/internal/backend/backend.go` defines the `Backend` interface every provider
implements — `FetchFolders`, `FetchMessages` (cursor-paged incremental),
`FetchBody`, `ApplyFlags`/`FetchFlags`, `Move`, `Append`, `Send`, `Watch`,
`Capabilities`, `Close`. The engine addresses a message by `(StableID, account)`
when the provider supplies an immutable object identity, and falls back to
`(Message-ID, account)` otherwise. RFC Message-ID remains threading/search
metadata and may be absent or duplicated. A `RemoteRef` (folder + provider id)
is the follow-up operation handle; for JMAP it happens to carry the same
immutable Email ID.

A `Capabilities` struct lets the engine adapt to provider quirks without
branching on provider names: `PushWatch`, `FlagChangesInDelta` (the delta already
carries flag changes, so the flag pass is O(changes)), `LabelsAreTags` (Gmail/JMAP — `Message.Labels`
is the authoritative tag set), and `AnsweredUnsupported` (Gmail can't persist
`\Answered`, so it's excluded from the merge to stop per-sync ping-pong). A
label-native backend also implements the optional `LabelWriter` interface
(`LabelTags`, `ApplyLabels`). JMAP implements `ArbitraryLabelWriter` to extend it
with arbitrary custom-keyword tags, and `TagMutationWriter` for explicit
`$seen`/`$flagged`/`$answered` property patches.

Outbound transport behavior belongs to `mailsend.Sender`: `SavesSentCopy`
decides whether Durian appends an IMAP Sent copy without branching on provider
names.

The implementations: **`imapbackend`** wraps the existing `cli/internal/imap`
client; **`graphbackend`** speaks Microsoft Graph (`/me` or `/users/{email}` for
shared mailboxes, cursor = Graph delta URL, native `/move`); **`gmailbackend`**
speaks the Gmail REST API (no folders — one "All Mail" stream, labels-as-tags,
cursor = `history.list` historyId); **`jmapbackend`** discovers RFC 8620/8621
endpoints, enforces the advertised Core request/object/concurrency limits,
exposes one account-wide stream, maps mailbox memberships and custom keywords
to tags, stores immutable Email IDs, and advances an `Email/changes` state
cursor. Its sender creates structured Email objects and references separately
uploaded attachment blobs; raw append still uses `Email/import`. `backendfactory` is the shared
composition root used by sync, body/attachment fetching, and daemon watchers.
Graph, Gmail, and JMAP send via a dedicated `sender.go` in each package.

### The engine

`cli/internal/syncengine/engine.go` drives any `Backend`: discover folders,
cursor-page `FetchMessages` until drained (persisting the cursor only after a
fully-ingested batch), `Ingest` each message (row + attachments + indexed
headers + tags — folder-role mapping, or `reconcileLabels` for label backends),
handle deletions, run a **three-way flag merge** for generic backends (local tags vs server flags vs
the `synced_flags` baseline — local wins Seen/Flagged/Answered, server wins
Deleted/Completed), then upload local intents: `uploadFolderMoves` (an INBOX
message that lost its `inbox` tag is `Move`d to Archive/Trash) and
`uploadLabelChanges` (per-message tag diffs via `ApplyLabels` for Gmail/JMAP).
For JMAP, explicit local unread/flagged/replied intents are durably journaled
and sent as property patches before the next download, so stale ambient local
state is not reconstructed as intent. The engine emits
`Result.NewMessageIdentifiers` for provider-neutral new-mail notification
instead of the legacy path's IMAP-UIDNEXT diffing.

### Watchers in `durian serve`

- **IMAP IDLE** (`handler/watcher.go`, `WatcherManager`) — one long-lived IDLE
  connection per legacy account; wakes on new mail or a `TriggerSync` signal.
  Accounts using the provider-neutral sync engine are skipped here.
- **Graph poll** (`handler/enginewatcher.go`) — Graph has no usable desktop push,
  so each Graph account gets two polling loops (a fast inbox pass, a slow
  full-mailbox pass) funneled through one per-account mutex. Cadence adapts to
  whether an SSE client is attached (inbox 30 s active / 2 m idle; full 5 m / 15 m),
  with backoff and jitter.
- **JMAP EventSource** (`handler/enginewatcher.go`) — an account-wide push stream
  triggers serialized incremental syncs; a slow full poll remains as recovery
  for dropped notifications. RFC 8887 WebSocket push is intentionally not
  implemented; EventSource is the supported JMAP push transport.

Gmail and Graph accounts use the polling loops. JMAP and opt-in engine/IMAP
accounts use statically selected EventSource or IMAP IDLE push, plus a slow
safety poll. A test keeps that startup-time selection aligned with each
backend's `PushWatch` capability without connecting during watcher setup. Local
tag mutations trigger an immediate, coalesced upload-only engine pass for all
engine accounts.

Daemon-triggered engine passes have a 5-minute watchdog so a stalled provider
does not hold the per-account mutex indefinitely. An authoritative replacement
snapshot may extend that deadline to 60 minutes: the snapshot must complete
before its cursor can advance. JMAP recovery emits bounded anchored query pages
instead of retaining a complete remote-ID set in its cursor, although the engine
still holds the presence set in memory until final deletion reconciliation. The
extension applies only to that recovery and its flag reconciliation; the
ordinary deadline still bounds later folders and the upload pass rather than
granting the entire account pass another hour.
Explicit `durian sync` commands remain caller-controlled.

### Live JMAP tests

The build-tagged live suite is a normal Bazel target, so `bazel test //cli/...`
always compiles it and reports it skipped when credentials are absent. To run it
against a test account, pass the environment through Bazel:

```sh
bazel test //cli/internal/jmapbackend:jmapbackend_live_integration_test \
  --test_env=DURIAN_JMAP_TEST_SESSION_URL \
  --test_env=DURIAN_JMAP_TEST_USERNAME \
  --test_env=DURIAN_JMAP_TEST_PASSWORD \
  --test_env=DURIAN_JMAP_TEST_AUTH \
  --test_env=DURIAN_JMAP_TEST_RECIPIENT_USERNAME \
  --test_env=DURIAN_JMAP_TEST_RECIPIENT_PASSWORD
```

Set the applicable variables in the invoking shell. The session URL and primary
credentials are required. `DURIAN_JMAP_TEST_AUTH` optionally selects `password`
(the default) or `bearer`. To additionally exercise delivery between accounts,
set and pass `DURIAN_JMAP_TEST_RECIPIENT_USERNAME` and
`DURIAN_JMAP_TEST_RECIPIENT_PASSWORD`. Unset optional variables remain absent
when passed this way. The tests create, send,
modify, and delete real messages; use disposable test accounts. The JMAP session
URL must be HTTPS unless it addresses loopback.

### Routing and validation

`EffectiveSyncEngine` defaults Microsoft OAuth to `graph`, Google OAuth to
`gmail`, and everything else to `legacy` (the provider presets set the value
explicitly). `durian validate` rejects the impossible combinations: `graph` on a
non-Microsoft account, `gmail` on a non-Google account, `jmap` without its
session configuration, and `legacy`/`engine` on
a Microsoft account (Microsoft must use Graph). Each backend namespaces its
cursor file (`-graph`, `-gmail`, `-jmap`, unsuffixed for IMAP) so switching engines can't
feed one backend another's incompatible cursor — it just forces a fresh full
resync, which is safe because the store upserts by native stable identity when
available and by Message-ID only as a compatibility fallback.

## Calendar

Calendar is a second provider-neutral subsystem, deliberately **separate** from
the mail Backend seam. Events live in a local **vdir** — a directory of `.ics`
files, one per master event (`<vdir>/<account-dir>/<calendar>/<uid>.ics`) —
modelled in `cli/internal/calendar/` (the neutral `Event`, RRULE ⇄ model
round-trip, atomic writes, content hashing).

`cli/internal/calendarsync/` is the two-way sync engine. It defines its own
`CalendarProvider` seam (`ListCalendars`, `FetchMasterEvents`, `CreateEvent`,
`UpdateEvent`, `DeleteEvent`, `RespondToEvent`, plus an optional
`DeltaCalendarProvider` for incremental fetch) with two clients:
`cli/internal/graphcalendar/` (Microsoft) and `cli/internal/googlecalendar/`
(Google). `calendar.go`'s `newCalendarProvider` switches on the OAuth provider.

The model is **local-first**: all reads and edits touch only the vdir. The
engine's `Plan` → preview → `Apply` pipeline (`twosync.go`) is the only thing
that reaches a provider or sends mail, and it prints every outgoing invitation
before applying. A conflict policy (`newer`/`remote`/`local`) resolves
both-sides edits, always backing up the losing local file first. `durian serve`
runs an autosync loop (`calendar_autosync.go`): download-only by default, or
two-way under `autosync_upload = "safe"` (uploading only non-notifying changes) —
but it can never send mail or delete a remote event on its own. See
[ADR-0002](design/0002-calendar-two-way-sync-and-recurrence/)
for the design and the recurrence handling.

## Optional tag sync

For multi-machine setups, `sync/` contains a small self-hosted server that stores `(message_id, account, tag, action, timestamp)` tuples. Clients push local changes and pull remote ones via HTTP. JMAP accounts already synchronize normal Durian tags through custom Email keywords, so they do not need this server for that purpose; tag sync remains useful for local-only/cross-provider state. Its Message-ID protocol cannot distinguish two same-account provider objects with a duplicate Message-ID, another reason to prefer native JMAP keywords for JMAP mail. Auth is a shared API key; **run it only on a trusted network** (Tailnet, LAN) — it has no TLS and no rate limiting. See the [tag sync README](https://github.com/julion2/durian/tree/main/sync) for setup.

## Design decisions

**Why one HTTP API instead of direct DB access?**
The GUI and CLI are separate processes written in different languages. Going through HTTP means the GUI never needs SQLite bindings, never has to worry about schema migrations, and gets a stable contract it can rely on. It also lets us ship a Linux GUI in Qt without duplicating Go code.

**Why SQLite + FTS5 instead of Maildir + notmuch?**
A single file is easier to back up, move between machines, and query with SQL when debugging. FTS5 is fast enough for a few hundred thousand messages and supports the same tag-based search model as notmuch.

**Why Swift for the macOS GUI instead of one cross-platform GUI?**
Native SwiftUI integrates cleanly with macOS features (keychain, notifications, look and feel, window management). The Linux Qt GUI is a separate, deliberately independent implementation — we'd rather have two small native clients than one big Electron-style shell.

**Why Bazel?**
Three languages (Go, Swift, C++/Qt), two platforms, one binary cache, reproducible builds. The alternative would be `go build` + `xcodebuild` + `cmake` + shell glue. The cost is a higher learning curve; the benefit is that CI and local builds stay identical.

## Logging

- **Go CLI**: `log/slog` with a `"module"` key. `durian serve` writes to `~/.local/state/durian/serve.log` (or `$XDG_STATE_HOME/durian/serve.log`, truncated on each start). Other commands write to stderr. Debug level via `--debug`.
- **Swift GUI**: wrapped in `macos/durian/Utilities/Log.swift` using `os.Logger`. View in Console.app with subsystem filter `org.js-lab.durian` (release) or `org.js-lab.durian.nightly` (debug).
- **Tag sync server**: stdout + systemd journal.

## Where to look next

- **Adding a new API endpoint**: `cli/internal/handler/` + matching entry in `cli/cmd/durian/serve.go` route list + `openapi.yaml`.
- **Changing the sync logic**: `cli/internal/syncengine/engine.go` for the neutral engine (Graph/Gmail/JMAP/opt-in IMAP); `cli/internal/imap/sync_mailbox.go` + `sync_flags.go` for the legacy IMAP path. Adding a provider = a new `backend.Backend` implementation plus factory wiring.
- **Adding a GUI feature**: start in the appropriate Swift Manager (`macos/durian/Managers/`), wire it to views.
- **Adding a CLI command**: `cli/cmd/durian/` — each command is a Cobra subcommand.
- **Onboarding end users**: [Getting Started](../../getting-started/).
