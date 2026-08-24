---
title: Calendar
weight: 3
---

`durian calendar` keeps your Outlook and Google calendars in a local
**vdir** — a directory of plain `.ics` files — and lets you read and edit them
entirely offline. Nothing leaves your machine until you explicitly run
`durian calendar sync`, which previews every email it would send before it
sends anything.

## The vdir

Calendars live under `vdir_path` (default `~/.local/share/durian/calendars`,
`$XDG_DATA_HOME`-aware) in the layout:

```
<vdir_path>/<account-dir>/<calendar>/<uid>.ics
```

One directory per calendar, one `.ics` per master event. A recurring series is
a **single** file with an `RRULE`, not one file per occurrence.

## Local-first: what touches the network

Only three things talk to a provider: `export`, `sync`, and the background
autosync in `durian serve`. Everything else is offline:

| Command | Touches the provider? |
|---------|-----------------------|
| `list`, `search`, `show` | No — reads the vdir |
| `new`, `rsvp`, `delete` | No — writes the vdir |
| `export` | Reads the provider (one-way dump) |
| `sync` | Two-way, and the **only** command that sends mail |

The edit commands **never send an invitation, update, or RSVP on their own** —
they only rewrite a local `.ics`. Mail is sent exclusively by
`durian calendar sync`, and only after it shows you a preview and you confirm.

Calendar sync targets Microsoft Outlook (Teams online meetings) and Google
Calendar (Google Meet). Each needs an OAuth account with calendar consent —
run `durian auth login <account>` once (see [OAuth](../../auth/oauth/)). An
auth failure prints `run 'durian auth login <account>' to consent`.

## Reading your calendar (offline)

```bash
durian calendar list [account...] [--account A]... [--calendar NAME]
                     [--today | --week | --month] [--from D] [--to D] [--json]
```

With **no arguments** `list` covers every configured account **plus** your
local-only calendars for the next 7 days. Bare account arguments still narrow
it, or use the repeatable `--account`. Windows: `--week` = 7 days (the
default), `--today` = 24 h, `--month` = 30 days, or an explicit `--from`/`--to`.
Recurring events are expanded to their occurrences; rows are marked `[online]`
and `[accepted]`/`[declined]`/`[tentative]`, and a multi-account listing prefixes
each row with `account/Calendar`.

```bash
durian calendar search <query> [--account A] [--calendar NAME] [--json]
```

Case-insensitive substring search over subject, location, description, and
attendee addresses, across every account and your local calendars. The query is
a positional argument — the account moved to `--account` because a free-text
query can't be told apart from an account name.

```bash
durian calendar show <event> [--account A] [--calendar NAME] [--json]
```

Matches `<event>` by iCalUID (exact or prefix) or by a unique subject
substring, and prints When, Location, Organizer, your status, the online-meeting
link, the recurrence one-liner, and every attendee with their RSVP.

{{< callout type="info" >}}
`show` and `new` interpret and display times in **UTC** (the GUI uses your local
time zone, so the two surfaces can differ).
{{< /callout >}}

### The WHEN grammar

`--from`, `--to`, and `--start` accept: RFC 3339 (`2026-08-03T09:00:00Z`),
`YYYY-MM-DD HH:MM`, `YYYY-MM-DD`, `today`, and `tomorrow`.

```bash
durian calendar list
durian calendar list work --today
durian calendar list --account work --account local --today
durian calendar list --from 2026-08-01 --to 2026-08-31
durian calendar search standup
durian calendar show 1A2B3C --account work
```

## Editing locally (applied on the next sync)

Each of these only writes a local `.ics`. Run `durian calendar sync <account>`
afterwards to push the change to the provider (see
[Calendar Sync](../calendar-sync/)).

```bash
durian calendar new <account> --calendar NAME -s SUBJECT --start WHEN
       [--end WHEN | --duration 1h30m] [--all-day]
       [--location L] [--description D]
       [--attendee a@example.com]... [--online-meeting]
```

`--calendar`, `--subject`, and `--start` are required. The default duration is
1 h for a timed event, or one day (snapped to 24 h) for `--all-day`.
`--online-meeting` requests a Teams meeting on Microsoft accounts and a Google
Meet on Google accounts (`--teams` is a deprecated alias). Times are interpreted
in UTC.

```bash
durian calendar modify <account> <event> [--calendar NAME]
       [--subject SUBJECT] [--start WHEN]
       [--end WHEN | --duration 1h30m] [--all-day | --all-day=false]
       [--location L] [--description D]
```

Patches only the fields whose flags are present; recurrence, attendees and all
other omitted fields are preserved. Pass an empty value such as `--location ""`
to clear a text field. `edit` is an alias for `modify`.

```bash
durian calendar rsvp <account> <event> <accept|decline|tentative> [--calendar NAME]
```

Sets your own participation status. It errors if you are the organizer, or not
an attendee of the event.

```bash
durian calendar delete <account> <event> [--calendar NAME] [--yes]
```

Removes the local `.ics`. On the next sync this cancels the event (if you
organize it) or declines it (if you only attend). Prompts `[y/N]` unless
`--yes`.

## Local-only calendars

The reserved account name `local` selects the calendars configured under
`calendar.local_calendars` (see
[Configuration → calendar](../../configuration/config/#local_calendars)). They
belong to no provider: **never uploaded, never pruned, and never the source of
an invitation.**

```bash
durian calendar list local
durian calendar new local --calendar "Privat" --subject "Zahnarzt" \
       --start "2026-08-20 09:00"
```

Creating a local event writes the `.ics` (making the calendar folder if it does
not exist yet) and prints **no** "run sync" reminder — there is nothing to sync.

- `durian calendar sync local` and `durian calendar export local` are rejected:
  `"local" holds local-only calendars — they have no provider to sync or export from`.
- Attendees and online meetings are rejected on a local event (the API returns
  `400`).
- A calendar marked `read_only` refuses `new`, `rsvp`, and `delete` (the API
  returns `403`).

{{< callout type="info" >}}
**Misconfigured `path`.** When a configured local calendar turns up empty,
Durian prints a stderr warning (never in `--json`) naming the corrected `path`.
Two shapes trip it: the path points at a vdir *base* (a folder of collections
with no `.ics` of its own), or a collection segment is doubled. A path that
simply does not exist yet is silent — the first write creates it.
{{< /callout >}}

## See also

- [Calendar Sync](../calendar-sync/) — the two-way sync model, the notification
  preview, autosync, and conflict handling.
- [Calendar (GUI)](../../gui/calendar/) — the macOS calendar app.
