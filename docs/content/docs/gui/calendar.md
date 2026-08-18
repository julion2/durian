---
title: Calendar
weight: 8
---

The macOS app has a full calendar that reads every configured account's vdir
plus your local-only calendars. It replaces the mail view **in place** — not a
separate window — and, unlike the mail view, is deliberately **not** filtered by
the active mail profile: you always see all of your calendars. It reads and
writes, and every write is local-first (see [Local-first writes](#local-first-writes)).

## Opening the calendar

- **Calendar menu → Show Calendar** (the item flips to **Show Mail** while the
  calendar is open).
- `Cmd+Opt+C` toggles between mail and calendar.
- From the mail list, the vim sequence `gc` ("go calendar").
- Leave with `q` or `Esc`.

Entering the calendar forces a refresh.

## Views

A segmented picker (or the Calendar menu, where a checkmark marks the active
view) switches between Agenda, Week, Month, and Year. Prev / Today / Next step
by the view's own unit — a week for Agenda and Week, a month for Month, a year
for Year — and `Cmd+Opt+[` / `Cmd+Opt+]` step as well.

- **Agenda** — a ~30-day list from the anchor date, grouped by local day.
  `j`/`k` move the cursor.
- **Week** — an hourly 00–24 grid, seven columns, with an all-day header row.
  Overlapping events sit side-by-side in lanes (~40 pt each, up to ~4); more
  concurrent events than fit collapse to a `+X` block you tap to cycle. A red
  line marks the current time.
- **Month** — a 6x7 grid with up to three event pills per day. Tap a pill to
  select it; tap an empty day to drill in (switches to Agenda anchored there).
- **Year** — twelve mini-months; a day with events gets a dot, and tapping a
  month drills in.

## Editing in the Week grid

- Double-click an empty slot to create an event at that hour.
- Drag an event's body to move it (across days too).
- Drag its top or bottom edge to resize.

Moves and resizes snap to 15 minutes; `Esc` cancels a drag in progress.
Recurring events, all-day events, and blocks clamped to the day edge can't be
dragged — use the form instead.

## The event form

The form opens in the right pane (New: `n`, `o`, `O`, or the **+** button; Edit:
`i` or the pencil).

- **Title** and **Location**.
- A **Calendar** picker — for **new events only**; an existing event can't be
  moved to another calendar.
- **All-Day** vs **Time Slot**, with **Starts** / **Ends**.
- New events get a **Meeting** card: an online-meeting toggle (Teams on
  Microsoft, Google Meet on Google) and an attendee list (type an email and
  press Return; an invalid address shows a hint and is ignored).
- **Notes**.

Save with `Cmd+Return` (disabled until the title is non-empty and the end is at
or after the start); Cancel or `Esc` discards.

## Local-first writes

Creating, editing, deleting, moving, and RSVPing all write the local `.ics`
immediately and update the view optimistically — **nothing is sent
automatically.** Invitations, online-meeting creation, and RSVP replies go out
only when you run a sync from the terminal, which previews them first:

```bash
durian calendar sync "Work"
```

A banner reminds you:

> Run `durian calendar sync "Work"` to send the invitations — automatic sync
> does not send them.

What background autosync does with your edits depends on `autosync_upload`. By
default (`"none"`) it only downloads, so even a plain, attendee-less event you
create in the GUI waits for a manual `durian calendar sync`. With `"safe"`,
autosync additionally uploads those non-notifying edits on its own — genuinely
two-way — while anything that would email someone (invitations, RSVP replies)
still waits for the manual, previewed sync. Autosync never sends mail or deletes
a remote event in either mode. See [Calendar Sync](../../cli/calendar-sync/).

## Event detail pane

Selecting an event shows a detail pane with cards for When (plus a **Recurring**
badge), Location, Calendar (a color dot and name), People (organizer and
attendees with contact avatars and status glyphs — green check = accepted,
red × = declined, orange ? = tentative, a dotted ring = no response), your own
status with **Accept / Tentative / Decline** buttons (only for meetings you
attend, not organize; your current response is highlighted), a **Join** link for
an online meeting, and the description. The list returns summaries; full detail
is fetched lazily when you open an event.

## Calendar sidebar

Toggle it with `s`, the toolbar button, or `Cmd+Opt+S` (collapsed by default,
and the choice persists). It shows a mini-month and your calendars grouped by
account, each with an event count and its color dot. Click a calendar to hide or
show it (an eye vs. a slashed eye); visibility is tracked per (account,
calendar) and persists. When nothing is synced yet it reads
`No calendars synced yet.`

## Hide declined events

`D`, or the Calendar menu item (a checkmark shows the state, which persists),
hides events you have genuinely declined. Events you organize are never hidden.

## Keymaps

The Calendar context has its own vim bindings — see
[Keymaps](../keymaps/) for the full table.
