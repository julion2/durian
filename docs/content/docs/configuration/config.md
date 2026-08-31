---
title: config.pkl
weight: 1
---

The main configuration file. Defines accounts, app-wide settings, sync intervals, and signatures.

```pkl
import "modulepath:/Config.pkl" as C

settings {
  theme = "system"
  notifications_enabled = true
  load_remote_images = false
}

sync {
  gui_auto_sync = true
  auto_fetch_interval = 120
  full_sync_interval = 7200
}

signatures {
  ["default"] = "Best regards"
}

accounts {
  new {
    name = "Personal"
    email = "you@example.com"
    alias = "personal"
    smtp { host = "smtp.example.com"; port = 587; auth = "password" }
    imap { host = "imap.example.com"; port = 993; auth = "password" }
    auth { username = "you@example.com" }
  }
}
```

## settings

| Field | Type | Default | Notes |
|---|---|---|---|
| `theme` | `"light" \| "dark" \| "system"` | `"system"` | GUI theme. `"light"` / `"dark"` force the app chrome regardless of the macOS appearance; `"system"` follows it. Email body rendering picks up the same value. |
| `notifications_enabled` | `Boolean` | `true` | Global notification toggle (override per account) |
| `load_remote_images` | `Boolean` | `false` | Block tracking pixels by default |
| `accent_color` | `String?` | `null` | Hex color, e.g. `"#3B82F6"` |

## sync

| Field | Type | Default | Notes |
|---|---|---|---|
| `gui_auto_sync` | `Boolean` | `true` | GUI syncs on launch and periodically |
| `auto_fetch_interval` | `Int` (seconds) | `120` | Quick sync interval |
| `full_sync_interval` | `Int` (seconds) | `7200` | Full sync interval |
| `tag_sync` | object? | `null` | Optional remote tag sync — see [tag sync server](https://github.com/julion2/durian/tree/main/sync) |
| `attachment_cache` | object? | `null` | `{ max_size_mb, ttl_days }` |
| `indexed_headers` | `Listing<String>?` | `null` | Extra MIME headers to fetch + index on top of the built-in seven (`List-Id`, `List-Unsubscribe`, `Precedence`, `X-Mailer`, `Return-Path`, `X-GitHub-Reason`, `Authentication-Results`). Use for provider-specific rules. After editing, run `durian sync --backfill-headers` once to populate existing messages. |

### Extra `indexed_headers` example

```pkl
sync {
  indexed_headers {
    "X-GitLab-NotificationReason"   // own_activity / assigned / mentioned
    "X-GitLab-Project-Path"
    "X-Spam-Status"
    "Auto-Submitted"
  }
}
```

Then in `rules.pkl`:

```pkl
new { name = "GitLab mentions"; match = "header:x-gitlab-notificationreason:mentioned"; add_tags { "gitlab/mention" } }
new { name = "Spam";           match = "header:x-spam-status:Yes";                        add_tags { "spam" } }
```

The built-in seven cover ~90% of inbox-zero patterns; user additions handle the long tail without code changes. After editing the config, `durian sync --backfill-headers` re-fetches headers for existing messages so old mails match the new rules. New mails pick up the change automatically on the next sync.

## calendar

Optional. Calendar sync mirrors Microsoft Outlook and Google Calendar OAuth
accounts to a local vdir of `.ics` files (see [Calendar](../../cli/calendar/)).

| Field | Type | Default | Notes |
|---|---|---|---|
| `vdir_path` | String | `~/.local/share/durian/calendars` | Layout `<vdir_path>/<account-dir>/<calendar>/<event>.ics`; `$XDG_DATA_HOME`-aware; override per run with `--out`. |
| `autosync` | Bool | `true` | Background sync in `durian serve`. With `autosync_upload = "none"` it is strictly **download-only**. |
| `autosync_interval` | Int (s) | `600` | Minimum 60; a value below 60 falls back to 600. |
| `autosync_upload` | `"none"` \| `"safe"` | `"none"` | `"safe"` auto-applies only attendee-less creates/edits; remote deletes, conflicts, and RSVPs always wait for an interactive `durian calendar sync`. |
| `conflict` | `"newer"` \| `"remote"` \| `"local"` | `"newer"` | Both-sides-edit policy; a losing local file is backed up to `<file>.conflict-<timestamp>`. |
| `local_calendars` | `Listing<C.LocalCalendar>` | empty | On-disk-only calendars (below). |

`conflict`, `autosync`, and `autosync_upload` resolve as: per-account override →
global `calendar.*` → schema default. Set `accounts[].calendar.enabled = false`
for a mail-only account: it is omitted from calendar reads, GUI loading, manual
and background sync. A calendar block only applies to Microsoft and Google OAuth
accounts.

### local_calendars

Calendars that live only on disk — never uploaded, pruned, or the source of an
invitation. `durian calendar list local` shows them.

| Field | Type | Notes |
|---|---|---|
| `name` | String (required) | Non-empty, unique case-insensitively; also the `--calendar` value. |
| `path` | String (required) | A directory of `.ics` files (a vdir *collection*, not a base); `~` expanded; created on first write. |
| `color` | String? | `#RRGGBB`; falls back to the collection's `color` meta file. |
| `read_only` | Bool (default false) | Refuses `new`, `rsvp`, `delete`. |

{{< callout type="warning" >}}
Each entry must be `new C.LocalCalendar { ... }` — a bare `new { ... }` inside a
typed `Listing` evaluates to `Dynamic` and Pkl rejects it. The key is
`local_calendars` (not `local`) because `local` is a Pkl keyword.
{{< /callout >}}

```pkl
calendar = (C.calendar) {
  vdir_path = "~/.local/share/durian/calendars"
  autosync = true
  autosync_upload = "none"
  conflict = "newer"
  local_calendars {
    new C.LocalCalendar { name = "Privat"; path = "~/calendars/privat"; color = "#8E44AD" }
    new C.LocalCalendar { name = "Feiertage"; path = "~/calendars/holidays"; read_only = true }
  }
}
```

### Per-account calendar

An account may carry its own `calendar { enabled, dir, include, conflict,
autosync, autosync_upload }`: `enabled` controls whether the account participates
in calendar reads and sync at all (default `true`), `dir` is the subdirectory
under `vdir_path` (default the alias, else the lowercased name), `include`
selects calendar display names for export and sync (empty = all), and the three
overrides above.

## accounts

A `Listing<AccountConfig>`. Each entry can be a literal `new { ... }` (password auth) or amend a provider preset (`(C.gmail) { ... }`, `(C.microsoft365) { ... }`).

| Field | Notes |
|---|---|
| `name` | Display name in the sidebar |
| `email` | Account address |
| `alias` | Short name for CLI (`durian sync <alias>`) |
| `display_name` | "From" header value |
| `default` | `true` on the default compose account |
| `default_signature` | Signature key from `signatures {}` |
| `notifications` | Per-account override of `settings.notifications_enabled` |
| `smtp` | Optional `{ host, port, auth }`; not needed by native Graph, Gmail, or JMAP sending |
| `imap` | Optional `{ host, port, auth, max_messages }`; required by IMAP engines and JMAP's server-side draft-command fallback |
| `jmap` | `{ session_url, auth }`; `auth` is `password` or `bearer` |
| `auth` | `{ username }` for password, or `oauth { client_id, client_secret }` for Google |
| `sync_engine` | Which sync path this account uses — see [Sync engine](#sync-engine) |
| `calendar` | Per-account calendar overrides — see [calendar](#calendar) |

### Sync engine

`sync_engine` selects how an account syncs. It resolves per account
(`EffectiveSyncEngine`): unset defaults to `graph` for Microsoft OAuth, `gmail`
for Google OAuth, and `legacy` for everything else — so the `C.microsoft365` and
`C.gmail` presets already pick the right one and you rarely set it by hand.

| Value | Backend | Providers |
|---|---|---|
| `legacy` | The classic IMAP syncer | Any IMAP account, including Gmail (opt back with `sync_engine = "legacy"`) |
| `engine` | Provider-neutral engine on the IMAP backend | Generic IMAP only |
| `graph` | Provider-neutral engine on Microsoft Graph | Microsoft only — **required** for Microsoft |
| `gmail` | Provider-neutral engine on the Gmail REST API | Google only — the default for Gmail |
| `jmap` | Provider-neutral engine on JMAP Mail and Submission | Fastmail and compatible JMAP servers |

The `gmail` engine syncs over the Gmail API instead of IMAP: it maps Gmail
**labels to tags**, downloads full message bodies for offline use, and syncs
incrementally via the history API. It needs the Gmail API enabled once in your
Google Cloud project — see [OAuth setup](../../auth/oauth/).

The `jmap` engine discovers capabilities from `jmap.session_url`, downloads all
mail for offline use, maps mailbox memberships to tags, follows `Email/changes`
state tokens, listens to EventSource push notifications in `durian serve`, and
sends via JMAP Submission. Use `auth = "bearer"` for a provider API token (for
example Fastmail) or `auth = "password"` for HTTP Basic authentication, then run
`durian auth login <alias>`. The session URL must use HTTPS, except that HTTP is
allowed for loopback addresses. Durian intentionally supports JMAP EventSource
push, not the optional RFC 8887 WebSocket transport.

`imap.max_messages` limits ordinary initial and incremental engine passes. The
schema default is 5000. An explicit `0` makes `durian sync` an unlimited full
local-first sync; `durian serve` retains a 5000-message safety cap per pass so a
large initial sync yields before its watchdog and resumes from its cursor on the
normal cadence. If a Gmail history ID or JMAP Email state expires, the
authoritative replacement snapshot deliberately completes the whole mailbox
before advancing its cursor; otherwise applying the cap could treat a partial
ID set as complete and delete valid local mail. Individual provider requests
remain paged and bounded.

Fastmail needs no IMAP or SMTP configuration:

```pkl
new {
  name = "Fastmail"
  email = "you@fastmail.com"
  alias = "fastmail"
  sync_engine = "jmap"
  jmap {
    session_url = "https://api.fastmail.com/jmap/session"
    auth = "bearer"
  }
}
```

Run `durian auth login fastmail` and paste a Fastmail API token.

IMAP and SMTP blocks are not required for sync or sending. Compose autosaves,
the outbox, and undo-send are local and work with JMAP, but the server-side
`durian draft save` and `durian draft delete` commands still require an IMAP
configuration; Durian does not currently upload drafts through JMAP.
When a JMAP account also configures password-authenticated IMAP,
`durian auth login <alias>` prompts separately for the JMAP credential and the
IMAP password so neither overwrites the other.

`durian validate` rejects: a value outside the five above; `graph` without a Microsoft OAuth
account; `gmail` without a Google OAuth account; and `legacy`/`engine` on a
Microsoft account (Microsoft must use Graph). `jmap` requires an absolute HTTP(S)
session URL and a `jmap` block; conversely, configuring a `jmap` block requires
`sync_engine = "jmap"`. Remote HTTP is rejected. Changing `sync_engine` triggers a
fresh full resync (the per-backend cursors are incompatible) but is safe — the
store adopts matching legacy rows by provider object ID when available and uses
RFC Message-ID only as metadata and a fallback for protocols without stable IDs.

### Provider presets

`Config.pkl` exposes:

- `C.microsoft365` — pre-fills Microsoft endpoints, `auth = "oauth"`. Default OAuth app is bundled.
- `C.microsoft365Shared` — shared mailbox variant; needs `auth_email` (the delegating user).
- `C.gmail` — pre-fills Google endpoints, `auth = "oauth"`. **Requires your own `client_id` / `client_secret`** — see [OAuth setup](../../auth/oauth/).

## signatures

A map of label → HTML string. Reference per-account via `default_signature = "<label>"`.

```pkl
signatures {
  ["default"] = "Best regards"
  ["work"] = """
    <b>Your Name</b><br>
    Position
    """
}
```

## Validate

```bash
durian validate config
```

Errors point to the specific field and line.
