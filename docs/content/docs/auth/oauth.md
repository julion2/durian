---
title: OAuth Setup
weight: 1
---

Durian supports OAuth 2.0 for Microsoft 365 and Google/Gmail.

## Microsoft 365

Durian can use a built-in Microsoft OAuth app by default. If you want to use your own Azure app (recommended for organizations), follow the steps below and set `client_id` in your config. Otherwise, you can skip app registration and omit `client_id` (the default will be used).

1. Go to [Azure Portal](https://portal.azure.com) → App registrations → New registration
2. Name: "Durian Mail" (or anything)
3. Supported account types: "Accounts in any organizational directory"
4. Redirect URI: Web → `http://localhost:8080/callback`
5. Go to API Permissions → Add permissions → **Delegated**:
   - `offline_access`
   - `https://outlook.office.com/SMTP.Send`
   - `https://outlook.office.com/IMAP.AccessAsUser.All`
   - Microsoft Graph → `Mail.ReadWrite`, `Mail.Send`, `Calendars.ReadWrite`
   - For a **shared mailbox**, add the `.Shared` variants too (`Mail.ReadWrite.Shared`, `Mail.Send.Shared`, `Calendars.ReadWrite.Shared`)
6. Grant admin consent (required for work/school accounts)
7. Copy **Application (client) ID**

{{< callout type="info" >}}
Microsoft accounts sync over **Microsoft Graph**, not IMAP — the Graph
permissions above are required, not optional. Durian requests both the
Outlook (IMAP/SMTP) and Graph scopes in a single consent; Azure mints a
separate token per resource behind the scenes. The bundled default app
already has all of these, so you only touch API Permissions for a custom app.
{{< /callout >}}

Add to config.pkl (custom app):

```pkl
oauth {
  provider = "microsoft"
  client_id = "your-client-id"
  // tenant = "common"   // Optional: "common", "organizations", or your tenant ID/domain
}
```

Shared mailboxes: configure the shared mailbox as its own `[[accounts]]` entry and set `auth_email` to the delegating user who has Full Access + Send As.

After registering (or after upgrading Durian to a Graph-capable version), run
`durian auth login <account>` once so the new refresh token carries the Graph
scopes, then confirm Graph access:

```bash
durian auth verify-graph work   # mints a Graph token and lists your mail folders
```

A `403` or "re-consent may be required" here means the scopes above are missing
from the app registration or the consent — re-run `durian auth login`.

## Google

{{< callout type="warning" >}}
Google OAuth tokens expire every 7 days while the app is in "Testing" mode in Google Cloud Console. You will need to re-authenticate periodically with `durian auth login`. This is a Google limitation for unverified apps (see [#147](https://github.com/julion2/durian/issues/147)).
{{< /callout >}}

1. Go to [Google Cloud Console](https://console.cloud.google.com) → APIs & Services
2. Create project (if needed)
3. **Enable APIs** → enable the **Gmail API** and the **Google Calendar API** for the project
4. Configure OAuth consent screen (External, add your email as test user)
5. Credentials → Create credentials → OAuth client ID → Web application
6. Authorized redirect URI: `http://localhost:8080/callback`
7. Copy **Client ID** and **Client Secret**

{{< callout type="info" >}}
Google accounts sync over the **Gmail REST API** by default (labels become
tags — see [Sync engine](../../configuration/config/#sync-engine)). The
`https://mail.google.com/` scope already covers the Gmail API, so there is no
extra scope to grant — but the **Gmail API must be enabled** in your Cloud
project (step 3), or the first sync fails with `Gmail API has not been used in
project ... before or it is disabled`. Prefer IMAP? Set `sync_engine = "legacy"`.
{{< /callout >}}

{{< callout type="warning" >}}
Calendar sync added the `https://www.googleapis.com/auth/calendar` scope.
**Existing Google accounts must re-run `durian auth login` once** to mint a
refresh token that carries it — otherwise calendar sync returns a permission
error while mail keeps working.
{{< /callout >}}

Add to config.pkl:

```pkl
oauth {
  provider = "google"
  client_id = "your-client-id"
  client_secret = "your-client-secret"
}
```

## Usage

```bash
durian auth login you@company.com   # Opens browser for OAuth (email or alias)
durian auth status                  # Show all accounts + token status
durian auth refresh you@company.com # Manual token refresh
durian auth logout you@company.com  # Remove token from Keychain
durian auth verify-graph work        # Microsoft only: confirm Graph scopes work
```

Tokens are stored securely in macOS Keychain and auto-refresh when near expiry.

## Troubleshooting

| Error | Solution |
|---|---|
| `client_secret is missing` | Add `client_secret` to config (required for Google) |
| `redirect_uri_mismatch` | Ensure redirect URI is exactly `http://localhost:8080/callback` |
| `invalid_grant` | Token expired, run `durian auth login` again |
| `AADSTS50011` | Redirect URI not registered in Azure Portal |
| `Gmail API has not been used in project … or it is disabled` | Enable the Gmail API in the Cloud project (Google step 3), or set `sync_engine = "legacy"` to use IMAP |
| Graph `403` / calendar permission error | Missing Graph/calendar scope — re-run `durian auth login`, then `durian auth verify-graph <account>` |
