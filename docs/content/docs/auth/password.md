---
title: Password Setup
weight: 2
---

For email providers that don't support OAuth (e.g., GMX, web.de, custom SMTP), Durian uses password authentication with the OS keychain.

On Linux, install `secret-tool` (libsecret). Then use `durian auth login <account>` — the CLI handles storing the password.

## Config + Login

Add to your config.pkl:

```pkl
accounts {
  new {
    name = "GMX"
    email = "you@gmx.de"
    smtp {
      host = "mail.gmx.net"
      port = 587
      auth = "password"
      max_attachment_size = "20MB"
    }
    imap {
      host = "imap.gmx.net"
      port = 993
      auth = "password"
    }
    auth {
      username = "you@gmx.de"
    }
  }
}
```

Then run:

```bash
durian auth login gmx         # alias
durian auth login you@gmx.de  # email
```

## Disabling the Keychain Access Dialog

By default, macOS prompts you to allow access every time `durian` reads from the Keychain. To disable this:

### Option 1: Allow All Applications (Easiest)

1. Open **Keychain Access.app** (Cmd+Space → "Keychain Access")
2. Search for your entry (e.g., "gmx-smtp")
3. Double-click the entry
4. Go to the **Access Control** tab
5. Select **"Allow all applications to access this item"**
6. Click **Save Changes** (enter your Mac password)

### Option 2: Allow Only Durian (More Secure)

1. Open **Keychain Access.app**
2. Search for your entry (e.g., "gmx-smtp")
3. Double-click the entry
4. Go to the **Access Control** tab
5. Keep **"Confirm before allowing access"** selected
6. Click the **+** button under "Always allow access by these applications"
7. Navigate to your durian binary (e.g., `~/.local/bin/durian`)
8. Click **Save Changes**

After this, `durian` will no longer prompt for Keychain access.

## Common Providers

| Provider | SMTP Host | Port | Notes |
|---|---|---|---|
| GMX | mail.gmx.net | 587 | Max 20MB attachments |
| web.de | smtp.web.de | 587 | Max 20MB attachments |
| Yahoo | smtp.mail.yahoo.com | 587 | Use app password |
| iCloud | smtp.mail.me.com | 587 | Use app-specific password |
| Fastmail | smtp.fastmail.com | 587 | IMAP/SMTP fallback; native [JMAP](../../configuration/config/#sync-engine) is recommended |

## Troubleshooting

| Error | Solution |
|---|---|
| `keychain entry not found` | Run `durian auth login <account>` again |
| `failed to get password from keychain` | Ensure the email/alias matches your config and retry login |
| Repeated access dialogs | Follow [Disabling the Keychain Access Dialog](#disabling-the-keychain-access-dialog) above |
| `authentication failed` | Verify password is correct, try app-specific password |

## App-Specific Passwords

Many providers require app-specific passwords instead of your main password:

- **GMX/web.de**: Account settings → Security → App passwords
- **Yahoo**: Account security → Generate app password
- **iCloud**: appleid.apple.com → Security → App-Specific Passwords

Using app-specific passwords is more secure and often required when 2FA is enabled.
