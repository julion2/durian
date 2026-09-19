---
title: CLI
weight: 2
---

The terminal client and engine. `durian` handles IMAP sync, SMTP send, the
local SQLite store, and the HTTP API the GUI talks to. If you live in a
terminal, it's the whole product. If you use the GUI, it's still running
under the hood.

The CLI is built with [Cobra](https://github.com/spf13/cobra) — every
subcommand has `--help`, ships a man page, and supports tab completion.

{{< cards >}}
  {{< card link="reference" title="Reference" subtitle="Every subcommand with practical examples." >}}
  {{< card link="calendar" title="Calendar" subtitle="Read and edit Outlook/Google calendars offline in a local vdir." >}}
  {{< card link="calendar-sync" title="Calendar Sync" subtitle="Two-way sync with a notification preview before anything emails." >}}
  {{< card link="completion" title="Shell Completion" subtitle="Tab-complete account names in zsh, bash, fish, carapace." >}}
{{< /cards >}}

## At a glance

```bash
durian sync                       # pull mail via IMAP, push tag/flag changes
durian search "tag:inbox"         # query the local store (notmuch syntax)
durian tag "thread:<thread>" +todo  # add/remove tags
durian show <thread>              # render a thread to stdout
durian send --to ...              # send mail (with $EDITOR fallback)
durian auth status                # OAuth/password state per account
```

Run `durian` with no arguments for the complete list.

## Files

| Path | What |
|---|---|
| `~/.config/durian/` | Pkl config files — `config.pkl`, `profiles.pkl`, `rules.pkl`, `keymaps.pkl`, `groups.pkl` |
| `~/.local/share/durian/email.db` | SQLite store: messages, tags, attachments |
| `~/.local/share/durian/contacts.db` | Local address book |
| `~/.local/state/durian/serve.log` | HTTP server log (truncated on each `durian serve` start) |
| `~/.cache/durian/<email>-imap-state.json` | Per-account IMAP sync state (UIDs, flags) |

XDG variables are respected — set `$XDG_CONFIG_HOME`, `$XDG_DATA_HOME`,
`$XDG_STATE_HOME` to override.
