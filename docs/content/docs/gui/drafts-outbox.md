---
title: Drafts & Outbox
weight: 4
---

Durian separates two states: a **draft** is something you're still writing; an **outbox** entry is something you've sent but is waiting in a delay window so you can undo.

## Drafts

### Local autosave

Every keystroke in the compose window is saved to the local SQLite store (`local_drafts` table). If the app crashes, the next launch reopens recovered drafts as compose windows — nothing is lost.

Local drafts are not visible to other devices — they live only on the machine that wrote them.

### Saving to IMAP

`Cmd+S` (or **File → Save Draft**) uploads the draft to the IMAP `Drafts` folder. From there it appears on every device that syncs the same account, including other Durian installs.

Drafts saved to IMAP also remain in the local store until explicitly discarded — closing the compose window doesn't delete them.

## Outbox

### Undo-send window

Hitting `Cmd+Return` doesn't send immediately. It writes the message to the `outbox` table with a `send_after` timestamp a few seconds in the future. During that window:

- A toast banner shows **Sending in 5s — Undo**.
- Clicking **Undo** (or pressing `Cmd+Z` while the banner is up) cancels the send and reopens the compose window with the original content.
- After the timer elapses, the SMTP send happens.

The window is configurable; the GUI defaults to a few seconds.

Each send action carries a client-generated idempotency key. If the HTTP response
is lost and the client repeats the request, the server returns the original
outbox entry instead of creating another delivery. That reservation remains
durable after the entry is sent or cancelled.

### Queued while offline

If the network is down (NetworkMonitor detects this), outbox entries stay queued and retry automatically on reconnect. You'll see them under **Outbox** in the sidebar with a status badge.

The server processes the same encrypted outbox, so messages queued by the GUI will eventually go out while `durian serve` runs.

### Resolving an uncertain delivery

If Durian loses the provider response after submitting a message, it cannot safely retry: the provider may already have delivered it. The entry remains claimed with status `reconciliation_required` instead. Resolve it as follows:

1. Stop the Durian GUI and `durian serve` so no sender is using the claim.
2. Run `durian outbox list` (or `durian --json outbox list`) and copy the entry's exact `Message-ID`.
3. Search the provider's Sent mail and delivery records for that exact `Message-ID`. Do not decide from the subject or recipients, which are not unique.
4. Record the verified outcome:

```bash
# The provider delivered the exact Message-ID: remove the durable claim.
durian outbox reconcile <id> --outcome delivered

# The provider definitively did not deliver it: release the claim for retry.
durian outbox reconcile <id> --outcome not-delivered
```

Both commands ask for confirmation. Automation must add `--yes`; `--no-input` without `--yes` is rejected. Choosing `not-delivered` incorrectly can send a duplicate. A claim already marked `delivery-confirmed` cannot be requeued.

These commands operate directly on Durian's encrypted local store. They do not require the unauthenticated localhost HTTP API to be exposed.

## CLI access

```bash
durian search "tag:draft" -l 10        # list IMAP drafts
durian draft delete <message-id>       # delete a draft on IMAP
durian outbox list                     # list queued and claimed sends
```

Local-only drafts are not visible to `durian search` — they're in a separate table.
