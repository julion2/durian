---
title: rules.pkl
weight: 3
---

Filter rules tag, move, or forward incoming mail during sync. Rules run sequentially — order matters when matches overlap.

## Skeleton

```pkl
rules {
  new { name = "Mailing lists"; match = "header:list-id:"; add_tags { "list" } }
  new {
    name = "Bulk notifications"
    match = "header:precedence:bulk OR header:auto-submitted:auto-generated"
    add_tags { "notification"; "ephemeral" }
    remove_tags { "inbox" }
  }
}
```

## Rule fields

| Field | Type | Notes |
|---|---|---|
| `name` | `String` | Shown in `--dry-run` output |
| `match` | `String` | Query expression (see below) |
| `add_tags` | `Listing<String>?` | Tags to add on match |
| `remove_tags` | `Listing<String>?` | Tags to remove on match |
| `accounts` | `Listing<String>?` | Restrict rule to certain account aliases |

## Finding the header value for a rule

Before writing a `header:Name:value` rule, you usually want to know what the provider actually sends. Two CLI flags help:

```bash
durian show <thread-id> --headers                    # what's in the local DB right now
durian show <thread-id> --raw-headers                # full set from IMAP, on demand
durian show <thread-id> --raw-headers --header x-gitlab-pipeline-status
```

`--headers` shows the indexed subset (the built-in seven plus your `sync.indexed_headers` from `config.pkl`). `--raw-headers` bypasses the allowlist and fetches the whole header block from the server — use it to discover headers you didn't know about yet.

Typical workflow when a new provider shows up:

1. `durian show <thread> --raw-headers | grep -iE 'gitlab|spam|list'` — find the candidate.
2. Add the names to `sync.indexed_headers` in `config.pkl` and run `durian validate`.
3. `durian sync --backfill-headers --force <account>` — re-indexes the existing messages against the new set. `--force` is required because the incremental skip otherwise treats "has any header" as "done".
4. `durian show <thread> --headers` — confirm the values are now in the local DB.
5. Write the `header:Name:value` rule and `durian rules apply --dry-run`.

See [Encryption at rest § Inspecting headers](../../cli/encryption-at-rest/#inspecting-headers).

## Match syntax

The same operators as `durian search`, with extra header/attachment matchers:

| Term | Matches |
|---|---|
| `from:value` | Sender contains substring (case-insensitive) |
| `to:value` | Any recipient contains substring |
| `subject:value` | Subject contains substring |
| `header:Name:value` | Any header matches |
| `header:Name:` | Header exists (any value) |
| `has:attachment` | At least one attachment |
| `has:attachment:pdf` | Attachment with that extension/MIME |
| `group:name` | Expands to all addresses in the group |
| `AND` (implicit) | Adjacent terms ANDed |
| `OR` / `NOT` / `( )` | Explicit operators |

## Pattern: ephemeral tagging

Mail tagged `ephemeral` is hidden from the default Inbox and silenced for notifications:

```pkl
new {
  name = "Newsletters"
  match = "header:list-unsubscribe: NOT header:list-id:"
  add_tags { "newsletter"; "ephemeral" }
  remove_tags { "inbox" }
}
```

Sidebar inboxes typically use `tag:inbox AND NOT tag:ephemeral` to surface only "real" mail.

## Pattern: GitHub by reason

GitHub sets `X-GitHub-Reason` on every notification, letting you split by category:

```pkl
new { name = "GH mentions";  match = "header:x-github-reason:mention";        add_tags { "gh"; "gh/mention" } }
new { name = "GH reviews";   match = "header:x-github-reason:review_requested"; add_tags { "gh"; "gh/review" } }
new { name = "GH CI noise";  match = "header:x-github-reason:ci_activity";    add_tags { "gh"; "ephemeral" }; remove_tags { "inbox" } }
```

## Apply to existing mail

Rules normally run on incoming sync only. To backfill after editing:

```bash
durian rules apply --dry-run     # preview
durian rules apply               # commit
```

## Validate

```bash
durian validate rules
```
