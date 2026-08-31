---
title: "ADR-0004: Native JMAP mail, submission, and EventSource push"
weight: 4
---

- **Status:** Implemented
- **Date:** 2026-08-26
- **Author:** @julion2

## TL;DR

Durian supports RFC 8620 Core and RFC 8621 Mail/Submission through the existing
provider-neutral backend and sync engine. JMAP mailbox memberships and custom
keywords become canonical Durian tags, incremental synchronization follows
`Email/changes` state tokens, and `cannotCalculateChanges` recovers through an
authoritative paged replacement snapshot. Composed mail is created as a
structured Email and submitted with `EmailSubmission/set`; raw append/import
remains available for existing RFC 5322 messages. `durian serve` consumes JMAP
EventSource state changes; RFC 8887 WebSocket push is intentionally not
implemented.

## Context

Fastmail, Stalwart, Cyrus and other servers expose one standardized native API
instead of requiring provider-specific REST adapters or an IMAP/SMTP fallback.
Durian already had a local-first read model and a neutral `backend.Backend`
contract from its Graph and Gmail implementations, so JMAP belongs behind that
contract rather than in the store, handlers, or UI.

## Decision

`cli/internal/jmapbackend` discovers the session document, validates every
server-provided endpoint, and requires HTTPS except for loopback test servers.
It supports HTTP Basic passwords and bearer API tokens stored under the
JMAP-specific `durian-jmap` keychain service. A legacy `durian-password` entry
is accepted once and copied only after successful JMAP discovery, preserving
existing configurations without risking an IMAP password overwrite.

The session's Core capability is operational input, not advisory metadata.
Durian bounds `Email/get` batches by `maxObjectsInGet`, rejects API and upload
payloads above `maxSizeRequest` and `maxSizeUpload`, and gates concurrent API
and upload work by `maxConcurrentRequests` and `maxConcurrentUpload`. Current
set operations contain one object, so they remain within any valid
`maxObjectsInSet`; each request contains one method call and therefore also
fits `maxCallsInRequest`.

The backend exposes one synthetic all-mail stream. Special-use mailboxes map to
fixed tags (`inbox`, `sent`, `draft`, `archive`, `trash`, `spam`); user mailboxes
map to lowercase parent paths. Ambiguous normalized paths receive a stable
mailbox-ID-derived suffix so downloads and later uploads resolve to the same
mailbox.

JMAP keywords are a second, independent tag source. Mailbox tags patch
`mailboxIds`; arbitrary Durian tags are encoded reversibly as lowercase custom
keywords. Native keywords that cannot safely decode to a Durian tag are exposed
under `jmap-keyword/<keyword>` to avoid collisions with mailbox tags. System
keywords beginning with `$` remain flag/state properties and are not surfaced
as arbitrary tags. Local `unread`, `flagged`, and `replied` edits are persisted
as explicit mutation intent and uploaded as property patches
(`keywords/$seen`, `keywords/$flagged`, `keywords/$answered`) before downloading
the next delta. Mailbox and arbitrary-tag changes use the normal durable label
baseline and their respective property patches. A JMAP account therefore does
not require Durian's optional tag-sync server merely to round-trip normal tags.

Each JMAP message is stored by its immutable, account-scoped `Email.id`.
RFC 5322 Message-ID remains searchable threading metadata and is the fallback
identity for protocols that lack a native stable identifier; it is not assumed
to be present or unique. API thread-message identifiers are consequently opaque:
every message uses its local row identifier, including when Message-IDs are
missing or duplicated.
The separate optional cross-device tag-sync protocol remains Message-ID based
and therefore cannot distinguish such duplicates; native JMAP keywords are the
authoritative multi-client path for JMAP tags.

Initial synchronization captures an Email state and pages message IDs/bodies.
Steady state uses `Email/changes`. When that state expires, Durian captures a
new state and enumerates an anchored `Email/query` one bounded page at a time.
The in-process page continuation carries only query state, anchor, and counts
rather than the complete remote ID set. Durian then refreshes flags, keywords,
and mailbox memberships for existing local messages, downloads bodies only for
missing IDs, and reconciles absent local references. The engine still
accumulates the complete presence set in memory for final deletion
reconciliation; durable disk-backed staging is not implemented. A changed query
state or missing anchor fails the run and safely restarts recovery. Intermediate
replacement cursors are deliberately not persisted. Hydration, deletion, and
label reconciliation must complete before the final cursor is written. A flag
reconciliation failure holds that cursor for one bounded replacement replay;
after a repeated failure, the cursor advances with unresolved flag references
queued for later retries.
The same neutral `FullSnapshot`/`Present` contract is used for Gmail history-ID
expiry.

Submission uploads attachment blobs, creates a structured Email in Drafts with
`Email/set` (`bodyStructure`, `bodyValues`, typed address and threading
properties), and creates an `EmailSubmission`. `onSuccessUpdateEmail`
explicitly removes `$draft`, removes Drafts membership, and adds Sent
membership; a missing Sent role mailbox is created first. If that implicit
filing fails after submission is confirmed, Durian retries one idempotent direct
patch. A failed repair is logged but the send still returns success to prevent
duplicate delivery, so the submitted copy may remain misfiled. `Email/import`
remains the correct path for generic raw `Append`/`Backend.Send` input. Local
compose autosave, outbox, and undo-send work normally; the separate
`durian draft save/delete` commands remain IMAP-only for now.

Push uses RFC 8620 EventSource with `Last-Event-ID`, reconnect backoff, and a
slow polling safety net. WebSocket push would add another transport without
improving the state-token correctness model and is therefore out of scope.

## Consequences

- A JMAP-only account needs neither IMAP nor SMTP for sync and sending.
- Mail remains fully available from Durian's encrypted local read model.
- Mailbox creation is limited to role mailboxes needed by move/submission
  invariants. Arbitrary local tags use JMAP keywords, not server mailboxes.
- The neutral backend deliberately keeps Durian multi-provider, but it costs
  JMAP fidelity: bodies are still normalized through RFC 5322 for the local
  read model, and `threadId` is not Durian's universal threading key. Stable
  Email IDs and keywords stay native instead of being discarded at that seam.
- Live integration tests can run against disposable Stalwart or Fastmail
  accounts and are always compiled by the Bazel CLI suite.
- Calendar and contacts JMAP capabilities are not part of this decision.

## Alternatives considered

- **Keep IMAP/SMTP for JMAP providers.** Rejected as the default because it
  gives up standardized state tokens, mailbox metadata, submission, and push;
  IMAP remains available only for the currently IMAP-only draft commands.
- **Add Fastmail-specific REST code.** Rejected because RFC 8620/8621 provides
  the same portable capability seam for Fastmail, Stalwart, Cyrus, and future
  providers.
- **Use RFC 8887 WebSocket push.** Deferred: EventSource is part of JMAP Core,
  works with the same state-token recovery, and avoids a second transport.
- **Persist mailbox-sized ID sets as recovery cursors.** Rejected: replacement
  IDs are emitted as bounded pages, the in-process continuation stores only the
  query anchor/state/counts, and the durable steady-state cursor remains the
  provider's EmailState. Presence refs remain an explicit in-memory cost until
  deletion reconciliation gains disk-backed staging.
