---
title: "ADR-0002: Calendar two-way sync with a local vdir and safe recurrence round-trips"
weight: 2
---

- **Status:** Accepted (implemented 2026)
- **Date:** 2026-08-17
- **Author:** @julion2 (solo project)
- **Supersedes:** —
- **Superseded by:** —

## TL;DR

Mirror Microsoft Outlook and Google calendars into a local **vdir** — a
directory of plain `.ics` files, one per master event — and make every read and
edit local-first. Reads (`list`, `search`, `show`) and edits (`new`, `rsvp`,
`delete`) touch only the vdir; nothing reaches a provider or sends mail except
`durian calendar sync`, which runs **Plan → notification preview → confirm →
Apply**. A provider-neutral `CalendarProvider` seam (Microsoft `graphcalendar`,
Google `googlecalendar`) keeps the sync engine provider-agnostic. Recurring
series are stored as a **single** file with an `RRULE`; rules the model can't
express are preserved verbatim as `X-DURIAN-OPAQUE-RECURRENCE` so a round-trip
never rewrites them. Conflicts resolve by a `newer`/`remote`/`local` policy and
always back up the losing local file first.

## Context

Durian is local-first for mail (offline store, encrypted at rest — see
[ADR-0001](../0001-mail-content-encryption-at-rest/)). Calendar had to match
that: usable offline, no surprise emails, no server as the only source of truth.
Two forces shaped the design.

**Calendar writes send mail.** Creating a meeting with attendees, editing it,
deleting it, or changing your RSVP all generate invitations, updates,
cancellations, or replies. A naive "edit = immediately push" model would fire
email on every keystroke-level change and on every background sync. That is
unacceptable for a client that also runs an autosync loop.

**Recurrence is lossy across providers.** iCalendar `RRULE`, Microsoft Graph's
`recurrence` object, and Google's `recurrence` strings do not map cleanly to one
another. A model that "understands" recurrence by re-serializing it will corrupt
any rule it doesn't fully model on the first round-trip.

We also wanted the mail Backend seam and the calendar to stay independent: mail
sync (`syncengine` + `backend.Backend`) and calendar sync (`calendarsync` +
`CalendarProvider`) have different cursors, different conflict semantics, and
different "sending" rules, so coupling them would help neither.

## Decision

### A local vdir as the source of truth

Events live under `calendar.vdir_path` in
`<vdir>/<account-dir>/<calendar>/<uid>.ics` — one directory per calendar, one
`.ics` per master event. The neutral model in `cli/internal/calendar/` owns the
`Event` type, RRULE ⇄ model serialization (`ical_roundtrip.go`), atomic writes
(`WriteFileAtomic`), and content hashing (`EventContentHash`, `CoreContentHash`,
`AttendeeSetHash`) used to detect real changes.

All read and edit commands operate on this vdir and never open a socket. This is
what makes the calendar work on a plane and makes edits instant.

### Plan → preview → confirm → Apply

`cli/internal/calendarsync/twosync.go` exposes `Plan`/`PlanAll` (build the full
change set without touching anything), `Apply`/`ApplyAll`, and `Sync`/`SyncAll`
(the two chained). A sync:

1. **Plans** downloads, prunes, uploads, updates, remote deletes, conflicts, and
   RSVPs.
2. **Previews** every email the plan would send — one `INVITE:`/`CANCEL:`/… line
   each, then a recipient total, or `No emails will be sent.`
3. **Confirms** interactively (`[y/N]`; a non-tty counts as "no"). Declining
   aborts the *entire* run, including the harmless local downloads.

The preview is generated from the same plan that Apply executes, so it can never
drift from what actually happens.

### The `CalendarProvider` seam

`calendarsync.go` defines `CalendarProvider` (`Owner`, `ListCalendars`,
`FetchMasterEvents`, `FetchInstances`, `GetEvent`, `CreateEvent`, `UpdateEvent`,
`DeleteEvent`, `RespondToEvent`, `IsAuthError`) plus an optional
`DeltaCalendarProvider` for incremental fetch. `graphcalendar` and
`googlecalendar` implement it; `newCalendarProvider` switches on the OAuth
provider. The engine speaks only the seam, so a third provider is one new client.

### Recurrence: model what we can, preserve the rest

A whole series is one file and one item — no per-occurrence editing from the CLI.
The model handles the common rules (daily; weekly on given days; monthly by date
or nth weekday; yearly; all with interval + end-date or count). Anything it
cannot express is flagged `OpaqueRecurrence` and written back **byte-for-byte**
as `X-DURIAN-OPAQUE-RECURRENCE`, so an unusual server rule survives an unlimited
number of round-trips untouched. Series exceptions (a single modified or
cancelled occurrence) are compared with `seriesExceptionsEqual` rather than
rewritten.

### Conflicts and backups

A conflict is an event changed (or deleted) on both sides since the last sync.
`SyncOptions.conflictPolicy` decides the winner: `newer` (default — later
`lastModifiedDateTime` vs local `LAST-MODIFIED`/mtime, ties to remote),
`remote`, or `local`. Whenever a local file loses or is overwritten, it is first
copied to `<file>.conflict-<unix-timestamp>`. **Local data is never silently
lost.**

### Safety rails on upload

The uploader is built so a retry or a race can never double-send or clobber:
idempotency keys on create; etag write-preconditions (a changed remote copy is
*skipped* and re-planned, not overwritten); a local re-hash guard before every
overwrite or prune; an organizer-role gate (attendee uploads only for meetings
you organize; deleting an attended meeting routes as a decline, never a cancel);
never re-invite on a local-wins/remote-deleted event that still has attendees;
and corrupt-file suppression so an unreadable `.ics` never triggers a
cancellation. The summary distinguishes `skipped` (a rail deferred it; retry
next run) from `failed` (the provider rejected it).

### Background autosync is two-way but never notifies

`durian serve` runs one autosync loop per eligible account
(`calendar_autosync.go`). It can upload — so it is genuinely two-way in `"safe"`
mode — but its hard invariant is that it can **never email a real person or
delete a remote event** without an interactive sync:

- `autosync_upload = "none"` (default) — strictly download-only.
- `autosync_upload = "safe"` — additionally auto-applies only provably
  non-notifying uploads (attendee-less creates/edits). Every remote delete,
  conflict, RSVP, and notifying upload still waits for an interactive
  `durian calendar sync`.

A cross-process run lock serializes Load → Plan → Apply → Save per directory so a
manual sync and the autosync loop can never collide.

## Alternatives considered

- **Server as source of truth (no local store).** Rejected: breaks offline use
  and makes every edit a network round-trip. The whole product is local-first.
- **Push edits immediately.** Rejected: sends mail on every change and on every
  background cycle, with no chance to review. The preview/confirm gate is the
  point.
- **A full recurrence model that always re-serializes.** Rejected: guarantees
  corruption of any rule the model doesn't fully cover. The opaque-passthrough is
  strictly safer.
- **One shared seam for mail and calendar.** Rejected: different cursors,
  conflict semantics, and sending rules; a shared interface would be all special
  cases.

## Consequences

- The calendar is fully usable offline; edits are instant and reversible via the
  `.conflict` backups.
- No calendar action can email anyone without an explicit, previewed
  `durian calendar sync` — including everything the GUI does.
- Adding a provider is a `CalendarProvider` implementation, nothing in the engine.
- A single cancelled occurrence of an Outlook series can linger locally —
  Microsoft Graph v1.0 gives no tombstone for it. Google is unaffected. This is
  documented in [Calendar Sync](../../../cli/calendar-sync/#recurring-events) as a
  known caveat.
- `export` (one-way, expanded occurrences, no `RRULE`) and `sync` (series kept
  with `RRULE`) must point at different directories; mixing them is unsupported.
