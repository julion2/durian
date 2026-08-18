---
title: Calendar Sync
weight: 4
---

`durian calendar sync` is the two-way bridge between your local vdir (see
[Calendar](../calendar/)) and a provider (Microsoft Outlook or Google Calendar).
It pulls new and changed events down, prunes remote deletions, and pushes your
local creates, edits, deletes, and RSVPs up — and it is the **only** command
that ever sends an email on your behalf.

Contrast it with `durian calendar export`, which is one-way and read-only: it
dumps the provider's events as expanded occurrences into a separate tree and
never writes back.

## Plan, preview, confirm

```bash
durian calendar sync <account> [--dry-run] [--yes] [--silent-rsvp] [--out DIR]
```

A sync runs in three visible steps:

1. **Plan.** Durian builds the full plan and prints the counts — downloads,
   prunes, uploads, updates, remote deletes, conflicts, RSVPs — *without
   touching anything yet*.
2. **Notification preview.** It lists every email the plan will send, one line
   each, then a total:

   ```
   INVITE: Design review [Work] -> 3 recipient(s)
   CANCEL: Old standup [Work] -> 5 recipient(s)
   Total: 8 recipients across 2 emails
   ```

   or `No emails will be sent.` The preview mirrors exactly what apply does.
3. **Confirm.** If the plan has remote changes, Durian asks
   `Apply N change(s) to Outlook ...? [y/N]`. **Declining aborts the entire run —
   including the local downloads and prunes.** A non-tty or closed stdin counts
   as "no".

Flags: `--dry-run` plans and previews but applies nothing and saves no state;
`--yes` skips the confirmation; `--silent-rsvp` records an RSVP locally without
emailing the organizer; `--out DIR` writes to a different vdir root.

A completed run prints, for example:

```
Calendar sync for Work: 4 downloaded, 1 pruned, 2 uploaded, 0 deleted remotely,
1 RSVP(s) sent, 1 conflict(s) resolved (newer wins), 0 skipped, 0 failed
```

## Meeting and RSVP semantics

- Creating an event with `--attendee` sends invitations.
- Editing a meeting sends updates to its attendees.
- Deleting a meeting you **organize** cancels it (attendees are notified);
  deleting one you only **attend** sends a decline instead.
- Changing your status with `rsvp` sends an RSVP — but only when you are an
  attendee, never when you are the organizer.

## Background autosync (`durian serve`)

`durian serve` runs one autosync loop per eligible account (an OAuth account on
a supported provider with `autosync` enabled). Each loop starts after a jittered
30–90 s delay, then runs every `autosync_interval` (default 600 s, minimum 60; a
value below 60 falls back to 600), with a 3-minute per-cycle timeout.

{{< callout type="warning" >}}
**No autosync run can ever email a real person or delete a remote event.**
{{< /callout >}}

- **Default (`autosync_upload = "none"`): strictly download-only.** It pulls
  remote changes and never uploads, deletes remotely, resolves conflicts, or
  sends RSVPs. Pending local changes are logged with a reminder to
  `run durian calendar sync`.
- **`autosync_upload = "safe"`** additionally auto-applies only provably
  non-notifying uploads — creates and edits of **attendee-less** events. Every
  remote delete, every conflict, every RSVP, and any notifying upload still wait
  for an interactive `durian calendar sync`.

Delegated and shared mailboxes are skipped by autosync (their `/me` resolves to
the token owner, not the mailbox). When anything changed, the GUI receives a
`calendar_updated` event over its SSE stream.

## Conflicts and `.conflict` backups

A conflict is an event changed (or deleted) on both sides since the last sync.
The `conflict` policy decides the winner: `newer` (default), `remote`, or
`local`. Under `newer`, the side with the later modification time wins — the
provider's `lastModifiedDateTime` versus the local file's `LAST-MODIFIED` (or
its mtime) — and a tie goes to the remote.

Whenever a local file loses or is overwritten, Durian first copies it to
`<file>.conflict-<unix-timestamp>`. **Local data is never silently lost.** The
same backup happens on first sight, when an untracked event exists on both sides
with differing content (remote wins, your local copy is kept as a backup).

## Safety rails

The uploader is built to never double-send or clobber:

- **Idempotency keys** on create, so a retried create can't produce a duplicate
  or a second invitation wave.
- **Etag write-preconditions.** If the remote copy changed under you, the write
  is *skipped* (counted `skipped`, not clobbered) and re-planned next run.
- A **local re-hash guard** before every overwrite or prune.
- An **organizer role gate:** attendee uploads happen only for meetings you
  organize, and deleting an attended meeting is routed as a decline, never a
  cancel.
- **Never re-invite:** a local-wins or remote-deleted event that still has
  attendees is skipped, not re-blasted.
- **Corrupt-file suppression:** an unreadable `.ics` never triggers a
  cancellation.

The summary line distinguishes `skipped` (a precondition or safety rail deferred
the action; it will retry) from `failed` (the provider rejected it).

## Rate limits

A `429` is honored via `Retry-After` — both the numeric delay-seconds form and
the HTTP-date form (a date in the past means retry now) — clamped to a 2-minute
cap so the run lock is never held for an hour. Without a header, Durian uses
truncated exponential backoff, `min(2^n s, 32 s)` plus up to 1 s of jitter. A
`503`/`504` gets a single 2 s retry. A `Retry-After` beyond the cap logs a
warning and waits the cap.

## State and the run lock

Each account keeps its sync state plus a mirror/cursor file inside the account's
vdir directory. Incremental syncs use the provider change feed (Google's sync
token); a mirror older than 7 days, or a changed query, forces a full reconcile.
A cross-process run lock serializes Load → Plan → Apply → Save per directory, so
a manual sync and the background autosync can never collide.

## Recurring events

A whole series is one file and one item — Durian does not offer per-occurrence
edits from the CLI. It understands the common rules (daily; weekly on given
days; monthly by date or nth weekday; yearly; all with an interval and an
end date or count). A rule it cannot express is shown as-is and kept **exactly**
as the server has it (marked `X-DURIAN-OPAQUE-RECURRENCE`) so a round-trip never
rewrites it. See the design note,
[Calendar two-way sync and recurrence](../../developers/design/0002-calendar-two-way-sync-and-recurrence/),
for the internals.

{{< callout type="warning" >}}
**Microsoft caveat.** A single cancelled occurrence of an Outlook series may keep
showing locally — Microsoft Graph v1.0 gives no tombstone for it. Google is
unaffected.
{{< /callout >}}

`export` writes expanded occurrences (no `RRULE`); `sync` keeps the series with
its `RRULE`. Point the two at **different** directories — never mix them.
