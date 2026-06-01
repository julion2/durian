---
title: Getting Started
weight: 1
---

A sequential walkthrough from zero to a working inbox. If you already know how to install and configure an email client, skip to step 2.

## 1. Install the CLI

The CLI is the backend for everything — install it first, even if you plan to use the GUI.

**macOS or Linux (Homebrew):**

```bash
brew tap julion2/tap
brew install durian             # CLI
brew install --cask durian      # GUI (optional, macOS only)
```

Linuxbrew (Homebrew on Linux) works for the CLI the same way. The macOS GUI cask is macOS-only; on Linux see the [Linux GUI README](https://github.com/julion2/durian/tree/main/linux) for the experimental Qt GUI.

**From source (macOS or Linux):**

```bash
git clone https://github.com/julion2/durian.git
cd durian
./cli/install.sh            # CLI → /usr/local/bin
./macos/install.sh          # GUI → /Applications/Durian.app (macOS only)
```

Verify:

```bash
durian --version
```

The GUI launches `durian serve` automatically as a child process — no manual daemon.

## 2. Create your config

Durian reads `~/.config/durian/config.pkl` (or `$XDG_CONFIG_HOME/durian/config.pkl`). Start from the example:

```bash
mkdir -p ~/.config/durian
curl -o ~/.config/durian/config.pkl \
  https://raw.githubusercontent.com/julion2/durian/main/docs/config-example.pkl
```

Open it and delete the example accounts you don't need. For each account you keep:

- Replace `you@example.com` / `you@company.com` / `you@gmail.com` with your real address
- Adjust `name` and `alias` (the alias is what you'll type in `durian sync <alias>`)
- Set the right SMTP/IMAP `host` and `port`

A minimal password-auth config:

```pkl
accounts {
  new {
    name = "Personal"
    email = "you@example.com"
    alias = "personal"
    smtp {
      host = "smtp.example.com"
      port = 587
      auth = "password"
    }
    imap {
      host = "imap.example.com"
      port = 993
      auth = "password"
    }
    auth {
      username = "you@example.com"
    }
  }
}
```

Validate the config before touching auth:

```bash
durian validate
```

## 3. Authenticate

Pick one guide based on your provider:

- **Gmail or Microsoft 365** → [OAuth setup](../auth/oauth/)
- **GMX, web.de, iCloud, Fastmail, custom IMAP** → [Password setup](../auth/password/)

After following the guide, you should be able to run:

```bash
durian auth status
```

and see your account marked as authenticated.

## 4. First sync

```bash
durian sync personal        # by alias
```

You'll see progress output:

```text
Syncing personal@example.com...
  ↓ INBOX: batch 1/3 (1-500)...
  ↓ INBOX: batch 2/3 (501-1000)...
  ✓ 1234 new, 0 deduplicated, 0 removed
```

If your mailbox is large, the first sync can take a few minutes. Subsequent syncs are incremental.

## 5. Use it

### Option A: GUI

Launch **Durian.app** from `/Applications`. It'll auto-start the backend and show your inbox. Vim-style navigation is enabled by default:

- `j` / `k` — next / previous email
- `Enter` — open thread
- `c` — compose, `r` — reply, `f` — forward
- `/` — search (Cmd+/ also works)
- `gi` / `gs` / `gd` / `ga` — go to inbox / sent / drafts / archive

Full keybinding reference: [Vim compose](../gui/keymaps/vim-compose/) covers the compose editor; list navigation uses the standard vim keys above.

### Option B: CLI

```bash
durian search "tag:inbox" -l 10       # latest 10 inbox emails
durian search "from:boss@company.com" # everything from a sender
durian search "date:today"            # today's mail
durian tag <thread-id> +important     # add a tag
durian send --to bob@x.com --subject Hi --body "Hello"
```

See `durian --help` for the full command list, or `man durian-<cmd>` for detailed reference.

## 6. Common next steps

- **Back up your encryption key** — `durian master-key export --out ~/durian-master.age` writes a passphrase-protected backup. Without it, a lost OS keychain entry means lost local-only data (drafts, manual contacts). See [Encryption at rest](../cli/encryption-at-rest/).
- **Sidebar folders and profiles** — copy [profiles-example.pkl](https://github.com/julion2/durian/blob/main/docs/profiles-example.pkl) to `~/.config/durian/profiles.pkl`
- **Custom keymaps** — copy [keymaps-example.pkl](https://github.com/julion2/durian/blob/main/docs/keymaps-example.pkl) to `~/.config/durian/keymaps.pkl`
- **Filter rules** (auto-tag on sync) — copy [rules-example.pkl](https://github.com/julion2/durian/blob/main/docs/rules-example.pkl) to `~/.config/durian/rules.pkl`
- **Multi-machine tag sync** — see the [tag sync server README](https://github.com/julion2/durian/tree/main/sync)
- **How it actually works** — [Architecture](../developers/architecture/)

## Troubleshooting

| Problem | Check |
|---|---|
| `durian sync` hangs | `tail -f ~/.local/state/durian/serve.log`, kill stale serve with `pkill durian` |
| Auth fails | `durian auth status`, re-run `durian auth login <alias>` |
| GUI doesn't start | Verify `durian --version` works standalone; check Console.app for `org.js-lab.durian` |
| Config parse error | `durian validate` — it tells you the exact field |
| Keychain dialogs on macOS | See [Disabling the Keychain Access Dialog](../auth/password/#disabling-the-keychain-access-dialog) |

If you're still stuck, [file an issue](https://github.com/julion2/durian/issues) with `durian --version`, `durian validate` output, and the relevant log lines.
