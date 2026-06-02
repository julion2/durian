# Changelog

All notable changes to Durian are documented here. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); the project itself adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html) once it leaves alpha.

The release body on GitHub Releases mirrors the corresponding section of this file.

## [v0.4.0] - 2026-06-02

The headline of this release is **at-rest encryption** of the local SQLite store. Mail bodies, subjects, headers, addresses, drafts, attachment metadata, and contact entries are now AES-256-GCM encrypted in `email.db` and `contacts.db`, with the master key in your OS keychain. Full-text search continues to work against a blind-token FTS5 index — no plaintext leaves the encryption layer. See the new [Encryption at rest](https://julion2.github.io/durian/docs/cli/encryption-at-rest/) walkthrough and [ADR-0001](https://julion2.github.io/durian/docs/developers/design/0001-mail-content-encryption-at-rest/).

### Before you upgrade — read this

This release ships an automatic, one-shot schema migration from v9 to v22 that encrypts every existing row in your local DB in place. The migration runs on the first `durian serve` start after upgrade and is **not reversible** — downgrading back to v0.3.x cannot read the migrated DB.

Strongly recommended before upgrading:

```bash
durian master-key export --out ~/durian-master.age
cp -a ~/.local/share/durian ~/durian-backup-v0.3.x
```

The first command writes a passphrase-encrypted backup of your master key — keep it somewhere outside `~/.local/share/durian` (password manager, hardware token, external drive). Losing it after a future keychain wipe makes the encrypted DB unrecoverable. The second snapshots your pre-migration DB so a rollback to v0.3.x is possible by restoring the snapshot.

The migration takes seconds on small mailboxes, up to a minute on 50k-message mailboxes.

### Security

- AES-256-GCM column encryption with per-purpose HKDF-SHA256 sub-keys (subject, body, addrs, headers, draft, meta, contact, fts-token). Master key in OS keychain (`durian-db` / `master`), reused per process via a Keyring with cached AEAD.
- Blind-token FTS5 search index — HMAC over content, post-decrypt filter defeats HMAC truncation collisions, bigram phrase queries supported.
- `PRAGMA secure_delete = ON` on every connection. Auto-VACUUM on `store.Open` when freelist exceeds threshold (Time-Machine restore hygiene).
- Sensitive logging redacted via `slog` wrapper, with a CI grep gate preventing regressions.
- Plaintext shadow columns (`mailbox`, `account`, `flags`) dropped from the schema — only operational metadata stays plaintext (message-id, date, size, flags, UID, account/mailbox names).
- Full ADR-0001 audit follow-ups closed: rate-limit oracles, post-decrypt FTS filter, VACUUM ordering, env-var lifecycle, bigram HMAC input, sensitive-key redaction, secure_delete behavioural test, Keyring.Wipe honesty pass (removed).
- OAuth endpoint HTML-escapes provider-supplied error params.
- Go SDK pinned to 1.25.10 (net/mail CVE); `x/net` bumped to v0.55.0 (five net/html CVEs).
- HTTP API: request body size limits + server timeouts (#218).
- HTTP API: per-session auth token between GUI and `durian serve` (#219).
- SAST / SCA workflow and CycloneDX SBOM attached to every release.

### Added

- `durian master-key export/import` — passphrase-encrypted age-file backup and restore of the master key.
- `durian serve --no-auth` — disable auth-token requirement for experimental clients.
- Account-name completion via Cobra `ValidArgsFunction` for all account-taking subcommands.
- New docs: [Encryption at rest](https://julion2.github.io/durian/docs/cli/encryption-at-rest/) walkthrough, ADR-0001 (mail content encryption at rest), §Encryption layer in [Architecture](https://julion2.github.io/durian/docs/developers/architecture/).
- "Back up your encryption key" called out as the first next step in getting-started.

### Changed

- Landing page, privacy page, security page, README updated to reflect encrypted-at-rest by default.
- Linux GUI: `profiles.pkl` + `config.pkl` loaded via Pkl subprocess — parity with macOS GUI.
- CI: GUI workflow runs on `macos-26` (uses macOS 26 SDK APIs like `glassEffect`).
- `rules_apple` bumped 4.3.2 → 4.5.3.
- Landing page redesigned with the keymap cycler and a Pkl-config preview card.

### Fixed

- Quoted reply blocks stripped from plain-text and GMX HTML bodies on render.
- `durian attachment --save` properly decodes the attachment part (was emitting base64 in some cases, #230).
- Single-part non-text messages now land as attachments instead of empty bodies (#228).
- Local draft row deleted when the IMAP-side draft is deleted (#232).
- Edit-Draft is per-message, not per-thread (#233).

### Known issues

- [#231](https://github.com/julion2/durian/issues/231) — Sent mail occasionally appears twice in a thread right after send. Microsoft 365 / Exchange rewrites the Message-ID server-side, breaking the UPSERT conflict key. The duplicate self-heals after 1–2 sync cycles. Tracked for v0.4.1.

### Coming next

- [#257](https://github.com/julion2/durian/issues/257) — Optional hardware-backed master key (YubiKey via age-plugin-yubikey). Opt-in, keychain remains the default. Backlog, P3.

## [v0.3.0] - 2026-05-01

Major release covering Pkl config migration, attachment prefetch cache, sidebar profiles, request-size limits, and per-session auth token between GUI and CLI. Adds a Linux Qt GUI (read-only MVP). Roughly 50 PRs (#143–#221). Full PR list on [the GitHub release page](https://github.com/julion2/durian/releases/tag/v0.3.0).

## [v0.2.2] - 2026-03-30

Search-enrichment perf — trim large bodies before returning them in `/search` responses (95% size reduction). Preview-text cache fix in Swift GUI. See [release notes](https://github.com/julion2/durian/releases/tag/v0.2.2).

## [v0.2.0] - 2026-03-29

Outgoing-mail formatting overhaul: HTML margin normalization, header quoting for display names containing commas, attachment-type detection via magic bytes. Vim count-multiplier and `gg`/`G` motions in thread view. See [release notes](https://github.com/julion2/durian/releases/tag/v0.2.0).

## [v0.1.5] - 2026-03-23

Keymap latency reduction in thread view. `golang.org/x/net` v0.26.0 → v0.52.0. Compose fix, signature fix, README rewrite with screenshots. See [release notes](https://github.com/julion2/durian/releases/tag/v0.1.5).

## [v0.1.4] - 2026-03-22

Five fixes covering sync, parser, threading, and a DNS rebinding hardening pass. HTML-signature example added to docs. See [release notes](https://github.com/julion2/durian/releases/tag/v0.1.4).

## [v0.1.3] - 2026-03-21

Pre-public prep — license, disclaimer, brew docs. Security: attachment-filename sanitization to prevent path traversal. CI fixes for the release-build pipeline. See [release notes](https://github.com/julion2/durian/releases/tag/v0.1.3).

## [v0.1.2] - 2026-03-19

Release-build CI pipeline introduced (GitHub Actions tag-triggered build + Homebrew-tap auto-update). See [release notes](https://github.com/julion2/durian/releases/tag/v0.1.2).
