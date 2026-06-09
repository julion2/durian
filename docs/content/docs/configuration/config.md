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
| `smtp` | `{ host, port, auth }` |
| `imap` | `{ host, port, auth, max_messages }` |
| `auth` | `{ username }` for password, or `oauth { client_id, client_secret }` for Google |

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
