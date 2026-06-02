---
title: Reference
weight: 1
---

The `durian` CLI is the engine — it handles IMAP sync, SMTP send, SQLite
storage, and exposes the HTTP API the GUI talks to. This page covers each
subcommand with one or two practical examples. Run `durian` (no args) for
the full list, or check the installed man pages:

```bash
man durian-sync
man durian-search
durian <cmd> --help
```

## sync — fetch and push mail

```bash
durian sync                          # all accounts, all mailboxes
durian sync personal                 # one account by alias
durian sync personal INBOX           # one mailbox
durian sync --debug                  # verbose logging to stderr
durian sync --upload-only            # only push local tag/flag changes
durian sync --download-only          # only fetch from server
durian sync --no-flags               # skip flag sync (bodies only)
durian sync --backfill-headers       # refetch headers for existing rows
durian sync --dry-run                # report what would happen, write nothing
```

Bidirectional by default — local tag changes are uploaded as IMAP flags / folder moves, and server-side flag changes are pulled down. The first sync of a large mailbox can take a few minutes; subsequent syncs are incremental.

The GUI runs `durian serve`, which keeps a long-lived IDLE connection open per account — explicit `durian sync` is mainly useful for cron jobs or troubleshooting.

## search — query the local store

```bash
durian search "tag:inbox" -l 10
durian search "from:boss@company.com AND has:attachment:pdf"
durian search "group:vip AND date:1w.." --json
durian count "tag:unread"
```

Uses [notmuch-style syntax](../gui/search/) — terms are ANDed by default; `OR`/`NOT` are explicit. `--json` emits machine-readable output for piping into other tools.

## tag — modify tags

```bash
durian tag "tag:inbox AND from:newsletter" +newsletter -inbox
durian tag <thread-id> +todo
durian tag list                       # show all tags + counts
```

Tags must be prefixed with `+` (add) or `-` (remove). Both can be mixed in one call.

## show — display a thread

```bash
durian show <thread-id>
durian show <thread-id> --html
```

Renders the thread to stdout — useful for piping into `less` or grepping a specific thread.

## attachment — list or download

```bash
durian attachment <message-id>                              # list parts
durian attachment <message-id> --save 2 --output ./out/     # download part 2 into ./out/
durian attachment <message-id> --save 2                     # download part 2 into the current dir
```

Part IDs come from the `list` output. `--save <part>` selects the part, `-o, --output <dir>` picks the target directory (defaults to `.`). The original filename is preserved.

## send — send an email

```bash
durian send --to bob@x.com --subject Hi --body "Hello"
durian send --to bob@x.com --subject Draft       # opens $EDITOR
durian send --to bob@x.com --subject "PR" --attach patch.diff
durian send --to bob@x.com --subject "Newsletter" --body-file newsletter.html --html
durian send --to bob@x.com --subject "Re: PR" \
            --in-reply-to "<orig@host>" --references "<root@host> <orig@host>" \
            --body "ack"
durian send --to bob@x.com --subject "huge" --attach video.mov --force
```

If `--body` is omitted, your `$EDITOR` opens with a temp file. `--body-file`
reads the body from disk (use with `--html` for HTML mail). `--in-reply-to`
and `--references` set the threading headers when scripting replies.
`--force` overrides the per-account attachment-size limit (see
`max_attachment_size_mb` in `config.pkl`).

## draft — manage IMAP drafts

```bash
durian draft save --to alice@x.com --subject WIP --body "..."
durian draft save --replace "<original-id>" ...
durian draft delete "<message-id>"
```

`--replace` overwrites an existing draft on IMAP by Message-ID — useful for autosave loops in scripts.

## rules — apply filter rules

```bash
durian rules apply                    # apply rules.pkl to all messages
durian rules apply --dry-run          # preview changes without writing
```

Rules normally run automatically on incoming mail during sync. `apply` is for backfilling — e.g. after editing `rules.pkl` you may want to re-tag your existing inbox.

## validate — check config

```bash
durian validate                       # all files
durian validate config                # just config.pkl
durian validate rules
durian validate profiles
durian validate keymaps
durian validate groups
```

Reports the offending field with file path and line. Run before `auth login` or `sync` if you've edited Pkl files.

## auth — manage credentials

```bash
durian auth login personal            # interactive (password or OAuth)
durian auth status                    # all accounts + token state
durian auth refresh personal          # force OAuth token refresh
durian auth logout personal           # remove from keychain
```

Credentials live in the macOS Keychain — see [OAuth setup](../auth/oauth/) and [Password setup](../auth/password/).

## master-key — back up the at-rest encryption key

```bash
durian master-key export -o ~/durian-master.age        # passphrase-encrypted age file
durian master-key export --output -                    # to stdout
durian master-key import --source ~/durian-master.age  # restore into a fresh keychain
durian master-key import --source FILE --force         # overwrite an existing entry
```

The previous `--out` / `--from` flag names are kept as deprecated aliases for one
release — they still work but print a deprecation warning.

The master encrypts every sensitive column in `email.db` + `contacts.db`. Lose it and the local DB is unrecoverable. See the [Encryption at rest](../encryption-at-rest/) walkthrough.

## contacts — local address book

```bash
durian contacts init                  # create the contacts DB (auto on first sync)
durian contacts import                # extract addresses from email store
durian contacts list
durian contacts search alice
durian contacts add bob@x.com "Bob Roberts"
durian contacts delete bob@x.com
```

Used by the GUI compose autocomplete. `import` walks your existing mail and seeds the DB.

## group — list contact groups

```bash
durian group list                     # all groups + member counts
durian group members vip              # members of one group
```

Groups are defined in `groups.pkl` — edit the file to add or remove members. The CLI is read-only.

## tag-sync — multi-machine tag replication

```bash
durian tag-sync init                  # one-shot bulk push of all local tags
```

Incremental `push` / `pull` subcommands are not yet implemented — for now tag changes
are pushed/pulled automatically as part of every `durian sync` (and continuously while
`durian serve` is running). `tag-sync init` is the one-time bootstrap to seed a fresh
sync server from an existing local DB.

Optional. Requires a self-hosted [tag sync server](https://github.com/julion2/durian/tree/main/sync) configured in `config.pkl`:

```pkl
sync {
  tag_sync { url = "http://nas:8724"; api_key = "your-secret" }
}
```

Run only on a trusted network — the protocol has no TLS or rate limiting.

## serve — HTTP API for the GUI

```bash
durian serve                          # default port 9723
durian serve --port 8080
durian serve --debug                  # debug-level logging to serve.log
durian serve --no-auth                # skip bearer-token auth (experimental clients)
```

Used by the GUI as a child process — you don't normally need to start this yourself. Logs go to `~/.local/state/durian/serve.log` (truncated on each start).

### Auth & bind

`serve` binds to `127.0.0.1` only and enforces a per-session bearer token. On startup it prints a single machine-readable line to stdout:

```
READY token=<hex> addr=127.0.0.1:9723
```

The macOS GUI captures this line from the child process's stdout pipe and includes the token as `Authorization: Bearer <hex>` on every request. Requests without a valid token get `401`. Requests from a non-loopback Host header get `403`.

**`--no-auth`** disables the bearer-token check (loopback host check is still enforced). Useful for experimental clients that don't implement the stdout-READY handshake — e.g. the Linux Qt GUI — and for ad-hoc `curl` testing. The READY line is still printed (with empty `token=`) so parsers don't break.

> Threat model note: bearer-token auth raises the bar against curious local processes, but it is not a hardened sandbox. Any process running as your user can already read your config, dbus, browser session, etc. — and could just spawn its own `durian serve --no-auth` on another port. Treat the token as defence-in-depth, not isolation.

## Global flags

| Flag | Effect |
|---|---|
| `--debug` | Debug-level logging |
| `--json` | Machine-readable JSON output (where supported) |
| `-c, --config <file>` | Override config file (default `~/.config/durian/config.pkl`) |
| `--help` | Per-command help |
