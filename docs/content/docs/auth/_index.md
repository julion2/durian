---
title: Authentication
weight: 5
---

Durian supports three authentication methods:

- **OAuth 2.0** — for Gmail and Microsoft 365. Tokens are stored in the OS keychain and auto-refreshed.
- **Bearer API token** — for Fastmail and other JMAP providers that issue API tokens.
- **Password** — for providers such as GMX, web.de, iCloud, and custom IMAP or Basic-auth JMAP servers.

API tokens and passwords are stored in the OS keychain.

Pick the guide for your provider:

- [OAuth setup](oauth/) — Gmail, Microsoft 365
- [JMAP configuration](../configuration/config/#sync-engine) — Fastmail and compatible JMAP providers
- [Password setup](password/) — IMAP/SMTP providers
