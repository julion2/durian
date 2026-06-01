---
title: Security
toc: false
---

{{< callout type="warning" >}}
**Early Alpha — no external security audit.** Use at your own risk for non-critical mail.
{{< /callout >}}

## Network

- **IMAP**: TLS-only — implicit TLS (993) or STARTTLS, no plaintext fallback.
- **SMTP**: implicit TLS (465) or STARTTLS (587/25); STARTTLS is required, the connection fails closed if the server doesn't offer it.
- **OAuth**: HTTPS to the provider's authorization endpoint.
- **Tag sync** (optional): runs over plain HTTP. Use only on a trusted network (Tailnet, LAN) — never the public internet.

## Credentials

OAuth tokens and passwords are stored in **macOS Keychain** or **libsecret** (Linux), never on disk in plaintext.

## Data at rest

Sensitive columns in `email.db` and `contacts.db` are **encrypted by Durian** using AES-256-GCM with per-purpose sub-keys derived from a 32-byte master via HKDF-SHA256. See [ADR-0001](docs/developers/design/0001-mail-content-encryption-at-rest/) for the full design.

| Encrypted | Plaintext (by design) |
|---|---|
| message subject, body text + HTML | message-id, references, in-reply-to (needed for thread reconstruction) |
| individual header values | dates, sizes, flags (is-seen, is-flagged, is-deleted) |
| from / to / cc addresses | account name, mailbox name (operational metadata) |
| draft JSON, outbox JSON | UID per mailbox |
| attachment filename, content-type, size | attachment payload (already encoded by the MIME layer) |
| contact name + email |  |
| FTS5 search index (blind-token via HMAC) |  |

The master key lives in the OS keychain (`durian-db` / `master` on macOS Keychain or libsecret on Linux). `durian master-key export` writes a passphrase-encrypted age file as a recovery artifact — keep that file somewhere safe, otherwise losing the keychain entry means losing locally-only data (drafts, manual contacts). See [the encryption-at-rest doc](docs/cli/encryption-at-rest/) for the user-facing workflow.

**FileVault / full-disk encryption is still recommended** as a second layer for the columns we deliberately keep plaintext, and to protect the keychain itself when the device is powered off.

**What encryption-at-rest does *not* protect:** anything in process memory while `durian serve` is running. A user-mode attacker with the same UID can read the master from the daemon's RAM (debugger, /proc, ptrace). Mitigation here is the operating system (run on a non-shared user account, lock the device when away), not the database layer. A future opt-in [hardware-key option](https://github.com/julion2/durian/issues/257) (YubiKey-backed master) would raise the cost of *exfiltrating the master for later offline decrypt* but does not change the runtime model.

## Reporting a vulnerability

Use [GitHub's private security advisory](https://github.com/julion2/durian/security/advisories/new) flow. Please do not open a public issue.
