---
title: "ADR-0004: Native JMAP mail, submission, and EventSource push"
weight: 4
---

- **Status:** Proposed (implemented in PR #349)
- **Date:** 2026-08-26
- **Author:** @julion2

## TL;DR

Durian supports RFC 8620 Core and RFC 8621 Mail/Submission through the existing
provider-neutral backend and sync engine. JMAP mailbox memberships become
canonical Durian tags, incremental synchronization follows `Email/changes`
state tokens, and `cannotCalculateChanges` recovers through an authoritative
ID-only replacement snapshot. Sending uses `Email/import` followed by
`EmailSubmission/set`. `durian serve` consumes JMAP EventSource state changes;
RFC 8887 WebSocket push is intentionally not implemented.

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

The backend exposes one synthetic all-mail stream. Special-use mailboxes map to
fixed tags (`inbox`, `sent`, `draft`, `archive`, `trash`, `spam`); user mailboxes
map to lowercase parent paths. Ambiguous normalized paths receive a stable
mailbox-ID-derived suffix so downloads and later uploads resolve to the same
mailbox.

Initial synchronization captures an Email state and pages message IDs/bodies.
Steady state uses `Email/changes`. When that state expires, Durian captures a
new state, queries the complete remote ID set, refreshes flags and mailbox
memberships for existing local messages, downloads bodies only for missing IDs,
and reconciles absent local references. The replacement cursor is persisted
only after hydration, deletion, label, and flag reconciliation all succeed.
The same neutral `FullSnapshot`/`Present` contract is used for Gmail history-ID
expiry.

Submission imports an RFC 5322 message into Drafts, creates an
`EmailSubmission`, and lets the server maintain Sent. Local compose autosave,
outbox, and undo-send work normally; the separate `durian draft save/delete`
commands remain IMAP-only for now.

Push uses RFC 8620 EventSource with `Last-Event-ID`, reconnect backoff, and a
slow polling safety net. WebSocket push would add another transport without
improving the state-token correctness model and is therefore out of scope.

## Consequences

- A JMAP-only account needs neither IMAP nor SMTP for sync and sending.
- Mail remains fully available from Durian's encrypted local read model.
- Mailbox creation is limited to the Archive mailbox required to preserve
  JMAP's at-least-one-mailbox invariant; arbitrary local tags do not create
  server mailboxes.
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
- **Persist mailbox-sized ID sets as steady-state cursors.** Rejected for state
  expiry recovery; authoritative replacement IDs are consumed as recovery
  state and the durable steady-state cursor remains the provider's EmailState.
