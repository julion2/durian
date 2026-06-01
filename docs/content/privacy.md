---
title: Privacy
toc: false
---

Durian is local-first. Your mail and metadata stay on your device.

## No telemetry

No analytics, no usage stats, no error reporting, no auto-updater. The CLI and GUI never contact a Durian-operated server, because none exists.

## Where your data lives

| Data | Location | Encrypted at rest |
|---|---|---|
| Mail bodies, subjects, headers, addresses, drafts, attachment metadata | `~/.local/share/durian/email.db` | yes (AES-256-GCM, per-column) |
| Contacts (name + email) | `~/.local/share/durian/contacts.db` | yes (AES-256-GCM) |
| Full-text search index | `email.db` (`messages_blind_fts`) | yes (blind-token via HMAC) |
| Configuration | `~/.config/durian/*.pkl` | no (plain text, version-controllable) |
| OAuth tokens, IMAP/SMTP passwords | OS keychain | yes (Keychain ACL / libsecret) |
| Master encryption key | OS keychain (`durian-db` / `master`) | yes (Keychain ACL / libsecret) |

The master key never leaves the keychain except when you explicitly run `durian master-key export` for backup. See [Encryption at rest](docs/cli/encryption-at-rest/) for the user-facing story.

## Network connections

Durian only connects to servers **you configure**:

- Your IMAP/SMTP provider over TLS.
- OAuth issuers (Google, Microsoft) — only during `durian auth login` and token refresh.
- An optional self-hosted tag sync server, off by default.

There is no Durian-operated CDN, telemetry endpoint, or update server.

## Wipe everything

```bash
rm -rf ~/.local/share/durian ~/.local/state/durian ~/.config/durian
```

Plus delete keychain entries whose service starts with `durian-`.
