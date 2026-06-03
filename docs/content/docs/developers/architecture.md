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

**One backend, many frontends.** The Go CLI is the only component that talks IMAP/SMTP and owns the SQLite store. Both GUIs are thin HTTP clients to `localhost:9723` — they never touch the DB directly.

## Directory layout

| Path | Purpose |
|---|---|
| `cli/cmd/durian/` | CLI commands (`sync`, `serve`, `auth`, `search`, `send`, `validate`, `contacts`, …) |
| `cli/internal/handler/` | HTTP API handlers + IMAP IDLE watcher + SSE event hub |
| `cli/internal/imap/` | IMAP sync logic (mailbox discovery, flag sync, message insertion, Gmail label handling) |
| `cli/internal/store/` | SQLite schema, FTS5 search, tags, attachments, local drafts, outbox |
| `cli/internal/config/` | Pkl parsing + `durian validate` |
| `cli/internal/oauth/` | OAuth flows (Google, Microsoft) |
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

One SQLite file at `~/.local/share/durian/email.db` (or `$XDG_DATA_HOME/durian/email.db`). Schema version is bumped on every ADR-0001 migration step; current floor is v22.

- `messages` — one row per email. Plaintext columns: `message_id`, `thread_id`, `in_reply_to`, `refs`, `from_addr`, `to_addrs`, `cc_addrs`, `date`, `created_at`, `size`, `uid`, `account_id` + `mailbox_id` (FKs), `is_seen` / `is_flagged` / `is_deleted` booleans. Encrypted BLOBs: `subject_ct`, `body_text_ct`, `body_html_ct`, `flags_other_ct` (non-canonical IMAP flags).
- `tags` — tag join table, one row per (message_id, tag).
- `message_headers` — raw headers used by filter rules (List-Id, Authentication-Results, …). The `value` column is encrypted (`value_ct` BLOB); `name` stays plaintext for SQL filtering.
- `attachments` — per-part metadata. `filename_ct`, `content_type_ct`, `size_ct` are encrypted; `part_id`, `disposition`, `content_id` stay plaintext (needed for fetch correlation with the IMAP server).
- `local_drafts` — crash-recovery drafts kept locally until saved to IMAP. The `draft_json` payload is encrypted (`draft_json_ct` BLOB).
- `outbox` — queued outgoing messages with `send_after` timestamp for undo-send. `draft_json_ct` BLOB; `attempts`, `last_error`, `created_at`, `last_attempted_at`, `send_after` plaintext.
- `mailboxes`, `accounts` — operational lookup tables. `name_ct` encrypted (mailbox / account display names are sensitive); integer IDs are the FK targets for `messages.mailbox_id` / `messages.account_id`.
- `messages_blind_fts` — FTS5 virtual table indexing HMAC-blind tokens of subject + body + addresses. No plaintext lives here; see [§Encryption layer](#encryption-layer) below.

### Encryption layer

Sensitive columns are AES-256-GCM encrypted at the application layer via `cli/internal/dbcrypto/` (see [ADR-0001](design/0001-mail-content-encryption-at-rest/)). A 32-byte master in the OS keychain (`durian-db` / `master`) is bootstrapped at `durian serve` start and derives one HKDF-SHA256 sub-key per purpose (subject, body, addrs, headers, draft, meta, contact, fts-token). Sub-keys live in process RAM with a cached `cipher.AEAD` so the hot encrypt/decrypt path stays at ~290 ns/op.

What stays plaintext on purpose: `message_id`, `thread_id`, `date`, `account`, `mailbox`, UID, flags, sizes. These are needed for IMAP sync correlation and for SQLite query planning. ADR-0001 §3 has the exact column-by-column table.

Search runs against `messages_blind_fts`, an FTS5 index built from HMAC-bigram tokens (`cli/internal/dbcrypto/tokenize.go`). The FTS index contains no plaintext — the same token in two different mails produces the same HMAC, and the post-decrypt filter (`cli/internal/store/search_filter.go`) re-checks any FTS hit against the decrypted body to defeat HMAC truncation collisions. Bigram phrase queries work via consecutive-token AND'ing.

Disk hygiene: `PRAGMA secure_delete = ON` is set on every connection (zeroes freed pages on DELETE / UPDATE). On `store.Open`, the freelist is inspected and auto-VACUUM runs if the file is unusually fragmented — covers the Time-Machine-restore-of-an-old-backup case. See ADR-0001 §6 "Disk hygiene".

Search uses notmuch-style query syntax (`tag:inbox AND from:boss@example.com`) parsed in `cli/internal/store/search.go` into SQL + FTS5 MATCH.

## Sync model

Per account, `durian serve` runs an IDLE loop in `handler/watcher.go`:

1. On startup, run a full sync via `imap.Syncer` (`cli/internal/imap/sync.go` and helpers in `sync_mailbox.go`, `sync_flags.go`, `sync_discovery.go`, `sync_store.go`).
2. Enter IMAP IDLE on the INBOX.
3. On IDLE wake (new mail event) or on explicit `TriggerSync` signal (e.g. user tagged something), break IDLE and run an incremental sync.
4. Broadcast `new_mail` events via the EventHub so the GUI can refresh.
5. On connection loss, reconnect with exponential backoff.

**Deduplication**: before downloading new UIDs, `dedupUnsyncedUIDs` fetches envelopes and checks whether each Message-ID already exists in the store (from another folder). Existing messages get their folder tags updated instead of being re-downloaded. See `cli/internal/imap/sync_mailbox.go`.

**Gmail**: labels come via `X-GM-LABELS`, not folders. The Gmail code path syncs only `All Mail`, `Spam`, and `Trash` — regular folders are virtual. Label → tag mapping lives in `sync_discovery.go` (`gmailLabelsToTags`).

**Flag sync**: bidirectional. Local tag changes are uploaded to IMAP (mapped to the corresponding flag or folder move), and server-side flag changes are pulled down. See `cli/internal/imap/sync_flags.go`.

## Optional tag sync

For multi-machine setups, `sync/` contains a small self-hosted server that stores `(message_id, account, tag, action, timestamp)` tuples. Clients push local changes and pull remote ones via HTTP. Auth is a shared API key; **run it only on a trusted network** (Tailnet, LAN) — it has no TLS and no rate limiting. See the [tag sync README](https://github.com/julion2/durian/tree/main/sync) for setup.

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
- **Changing the sync logic**: `cli/internal/imap/sync_mailbox.go` is the main loop; `sync_flags.go` handles flag/tag propagation.
- **Adding a GUI feature**: start in the appropriate Swift Manager (`macos/durian/Managers/`), wire it to views.
- **Adding a CLI command**: `cli/cmd/durian/` — each command is a Cobra subcommand.
- **Onboarding end users**: [Getting Started](../../getting-started/).
