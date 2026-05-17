---
title: Design Decisions (ADRs)
weight: 2
---

Architecture Decision Records following [Michael Nygard's lightweight
format](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions).

Each ADR captures **one** significant decision: its context, what was decided,
the considered alternatives, the trade-offs, and the consequences. ADRs are
**immutable** once accepted — if a decision changes, write a new ADR that
supersedes the old one. Never edit history; that way the rationale chain stays
honest.

## Index

| #    | Title                                                | Status     |
| ---- | ---------------------------------------------------- | ---------- |
| [0001](0001-mail-content-encryption-at-rest/) | Mail content encryption at rest with searchable blind-token FTS5 | Proposed |

## Status lifecycle

- **Proposed** — written, open for review/feedback (issue or PR comments).
- **Accepted** — decision is binding; implementation may start or be in flight.
- **Implemented** — code is merged and shipping.
- **Superseded by ADR-NNNN** — a later ADR replaces this one. Both stay in the
  tree.
- **Rejected** — written but explicitly discarded. Kept for posterity so the
  next person doesn't redo the same investigation.

## Writing a new ADR

1. Copy an existing ADR as the template.
2. Number it `NNNN-kebab-case-title.md`, four digits, monotonically increasing.
3. Open with the standard header (Status / Date / Context / Decision /
   Consequences / Alternatives). Keep it short — if it grows beyond ~5 pages,
   split it.
4. Link related ADRs at the bottom.
5. Add the row to the index above in the same PR.
