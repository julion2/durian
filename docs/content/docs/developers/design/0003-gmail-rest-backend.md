---
title: "ADR-0003: Gmail sync over the REST API with labels as tags"
weight: 3
---

- **Status:** Accepted (implemented 2026; in daily use)
- **Date:** 2026-08-17
- **Author:** @julion2 (solo project)
- **Supersedes:** the IMAP/`X-GM-LABELS` path for Gmail (still available via `sync_engine = "legacy"`)
- **Superseded by:** —

## TL;DR

Sync Gmail over the **Gmail REST API** instead of IMAP, behind the same
`backend.Backend` seam introduced for Microsoft Graph. Gmail's real model is
**labels, not folders**, so `gmailbackend` maps labels directly to Durian tags
(`LabelsAreTags`) and implements the optional `LabelWriter` interface — tag
changes upload as `users.messages.modify` label edits. Incremental sync uses
`users.history.list` (historyId cursor); the initial pass pages
`users.messages.list`. Sending is `users.messages.send` with a raw base64url
MIME body. It is the default for Google OAuth accounts (`EffectiveSyncEngine` →
`gmail`), uses the existing `https://mail.google.com/` scope (no re-consent), and
needs only the Gmail API enabled once in the Cloud project.

## Context

The legacy Gmail path rode IMAP with `X-GM-LABELS`, which fought the model the
whole way:

- Gmail folders are **virtual** — the syncer had to special-case `All Mail`,
  `Spam`, and `Trash` and treat everything else as a label projection.
- A message lives under *many* labels but IMAP wants it in *one* folder, so
  label↔folder mapping was a constant source of dedup and move edge cases.
- Gmail IMAP IDLE and flag semantics are idiosyncratic and occasionally flaky.

The Microsoft Graph migration had already introduced a **provider-neutral
`Backend` seam** (`FetchFolders`/`FetchMessages`/`ApplyFlags`/… with a
`Capabilities` struct) and a provider-neutral `syncengine`. That made a native
Gmail backend cheap: implement the interface, describe Gmail's quirks through
capability flags, and reuse the whole engine — ingest, three-way flag merge,
notification emission — unchanged.

## Decision

### `gmailbackend` implements the Backend seam

`cli/internal/gmailbackend/gmailbackend.go` — `New(account)` — talks to the Gmail
REST API. There are **no folders**: `FetchFolders` returns a single "All Mail"
stream, and each `Message` carries its full set of labels in `Message.Labels`.
Label id ⇄ tag-name resolution is built once (`loadLabels`, with a `tagToID`
reverse map) and refreshed as needed.

Capabilities advertised to the engine:

| Flag | Why |
|---|---|
| `LabelsAreTags` | `Message.Labels` is the authoritative tag set; the engine mirrors labels ⇄ tags instead of folder-role mapping. |
| `FlagChangesInDelta` | `history.list` already carries flag changes, so the flag pass is O(changes). |
| `AnsweredUnsupported` | Gmail has no per-message `\Answered`; see below. |

It also implements `backend.LabelWriter` (`LabelTags`, `ApplyLabels`), which the
engine's `uploadLabelChanges` uses to push per-message tag diffs. The static
assertion `var _ backend.LabelWriter = (*Backend)(nil)` keeps that contract
honest at compile time.

### Cursor = historyId

The cursor is opaque JSON: the initial `messages.list` pageToken plus a
historyId snapshot, then `users.history.list` from that historyId for every
incremental sync. It is namespaced with a `-gmail` suffix so it can never be fed
to another backend. Bodies are fetched lazily (`FetchBody`) for full offline use.

### `\Answered` is excluded from the merge

Gmail exposes no durable per-message "answered" flag. If the engine tried to
reconcile Answered against Gmail it would flip the local state back every sync
(a ping-pong). `AnsweredUnsupported` tells the three-way merge to pin Answered to
the local baseline and never upload or download it. The other flags
(Seen/Flagged via `modify`, Deleted/Completed) reconcile normally.

### Sending

`cli/internal/gmailbackend/sender.go` — `NewSender` — submits via
`users.messages.send` with a raw base64url-encoded MIME message and an explicit
`Bcc` header. Addresses are guarded against CR/LF header injection before the
message is assembled.

### Scope and enablement

The existing `https://mail.google.com/` OAuth scope already authorizes the Gmail
API, so no new consent is needed for mail. The one operational requirement is
enabling the **Gmail API** in the Google Cloud project — otherwise the first
sync fails with "Gmail API has not been used in project … or it is disabled"
(documented in [OAuth setup](../../../auth/oauth/)). The separate
`.../auth/calendar` scope added for calendar sync does require a one-time
re-login, but that is [ADR-0002](../0002-calendar-two-way-sync-and-recurrence/)'s
concern, not this one.

## Alternatives considered

- **Keep Gmail on IMAP.** Rejected as the default: the folder/label impedance
  mismatch is exactly what the labels-as-tags backend removes. Kept available as
  `sync_engine = "legacy"` for anyone who prefers it or can't enable the API.
- **A Gmail-specific syncer (not behind the neutral seam).** Rejected: it would
  duplicate the engine's ingest, merge, and notification logic. Implementing
  `Backend` reuses all of it.
- **Gmail Pub/Sub push in `durian serve`.** Not built because it requires a
  public receiving endpoint. The provider-neutral engine watcher polls Gmail's
  incremental history cursor instead.

## Consequences

- Gmail is a first-class, live backend — the default for Google accounts, in
  daily use — not an IMAP shim. `sync_engine = "legacy"` is the escape hatch.
- Tags and Gmail labels are the same thing; a tag that maps to a real label
  round-trips to the server, and `LabelTags` bounds the vocabulary the uploader
  will touch.
- Switching an existing Google account to `gmail` (or back) forces a fresh full
  resync because the cursor formats are incompatible — safe, because the store
  upserts by Message-ID.
- Answered/replied state is local-only for Gmail accounts by design.
- Enabling the Gmail API in the Cloud project is now a setup step for Gmail users.
