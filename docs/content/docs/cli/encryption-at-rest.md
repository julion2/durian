---
title: Encryption at rest
weight: 3
---

Durian's local SQLite store is encrypted by default. This page is the user-facing walkthrough — what gets encrypted, where the key lives, how to back it up, and how to recover.

For the cryptographic design and threat model, see [ADR-0001](../../developers/design/0001-mail-content-encryption-at-rest/).

## What gets encrypted

Sensitive columns in `~/.local/share/durian/email.db` and `~/.local/share/durian/contacts.db` are sealed with AES-256-GCM per column:

- Mail subjects, bodies (text + HTML), individual header values
- From / to / cc address columns
- Draft and outbox JSON
- Contact name and email
- Attachment filename, content-type, size
- The full-text search index (HMAC blind tokens — no plaintext)

Deliberately stays plaintext, because IMAP sync and SQLite query planning need it:

- `message_id`, references, in-reply-to (thread reconstruction)
- Date, size, flags (is-seen, is-flagged, is-deleted)
- Account and mailbox names, UID per mailbox

The full column-by-column table is in ADR-0001 §3.

## Where the master key lives

A single 32-byte master is stored in the OS keychain:

- **macOS:** Keychain, service `durian-db`, account `master`. Inspect with `security find-generic-password -s durian-db -a master`.
- **Linux:** libsecret (`secret-tool lookup service durian-db account master`).

Per-purpose sub-keys (subject, body, addrs, headers, draft, meta, contact, fts-token) are derived once at `durian serve` start via HKDF-SHA256 and held in process memory. They never leave the daemon — the GUI never sees a key, only decrypted payloads over the local HTTP API.

The first run of any `durian` command that needs the keyring auto-generates the master and writes it to the keychain. No setup is required.

## Backup: export the master to an age file

If you ever lose the keychain entry — Mac wipe, Migration Assistant going sideways, fresh OS install — the encrypted DB is unreadable without the master. **Export it once, store the file in your password manager or on a hardware token.**

```bash
durian master-key export --output ~/durian-master.age
```

You'll be prompted for a passphrase. The output is an ASCII-armored age file (scrypt recipient), so any standard `age` install can also decrypt it:

```bash
age --decrypt ~/durian-master.age
```

The file content is the 64-character hex of the master. Treat it like a root password: anyone with the file plus passphrase can decrypt every mail in your local DB.

Write to stdout if you want to pipe straight into a password manager:

```bash
durian master-key export --output - | pbcopy   # macOS clipboard
```

## Restore: import an age file into a fresh keychain

On a new machine (or after `security delete-generic-password -s durian-db`), bring the master back before `durian sync`:

```bash
durian master-key import --source ~/durian-master.age
```

Passphrase prompt, decrypt, write to the keychain. Refuses to overwrite an existing entry unless you pass `--force` (using `--force` on a populated DB with a different master makes that DB unreadable — the warning is real).

## What if I lose both the keychain entry and the export?

IMAP-sourced mail is recoverable — re-sync from the server with a fresh keyring, you'll re-fetch everything. **Local-only data is gone:**

- Crash-recovery drafts that hadn't been saved to IMAP yet
- Manually-added contacts that aren't derived from your mail history
- Local tag history (until your next `tag-sync push`)

So: export the master at least once, soon. The `durian master-key export` command is the entire disaster-recovery story.

## Inspecting headers

Because `message_headers.value` is encrypted at rest, raw `sqlite3` access doesn't work for inspecting individual mail headers anymore. Two CLI flags are the canonical replacement:

```bash
durian show <thread-id> --headers                    # what's in the local DB right now
durian show <thread-id> --header list-id             # filter to one header (case-insensitive)
durian show <thread-id> --raw-headers                # bypass the indexed allowlist, fetch from IMAP
durian show <thread-id> --raw-headers --header x-spam-status
```

`--headers` reads from `message_headers` and decrypts via the meta sub-key. `--raw-headers` does a `BODY.PEEK[HEADER]` IMAP fetch — useful when you want to see headers that aren't on the indexed allowlist (the built-in seven plus your `sync.indexed_headers`). See the [rules-writing walkthrough](../../configuration/rules/#finding-the-header-value-for-a-rule).

## Performance

On Apple M2: ~290 ns per AES-GCM decrypt on the hot path with the cached AEAD (PR #258, [#254.1](https://github.com/julion2/durian/pull/258)). A full-mailbox scan of 50,000 messages costs ~15 ms of crypto work — dwarfed by SQLite row I/O and IMAP latency.

## Limits, honest version

Encryption-at-rest protects the **on-disk** state. It does not protect:

- **Process memory of a running `durian serve`.** A user-mode attacker with the same UID (malware in another app under the same login, a debugger, ptrace) can read the master and every sub-key from RAM. Mitigation lives at the OS layer — don't share user accounts, lock the device when stepping away, run FileVault.
- **Backups while the device is unlocked.** A Time Machine snapshot or iCloud Drive sync of `~/.local/share/durian/` carries an at-rest-encrypted DB *plus* a keychain that is unlocked for the logged-in user. The DB is ciphertext, the keychain is software-protected — see ADR-0001 §6 for the trade-off.

A future opt-in [hardware-key option](https://github.com/julion2/durian/issues/257) (master wrapped against a YubiKey PIV slot) would raise the bar against the "exfiltrate the master for offline decrypt" case but does not change the runtime model.

## See also

- [ADR-0001](../../developers/design/0001-mail-content-encryption-at-rest/) — the full design and threat model
- [Security overview](../../../security/) — network, credentials, vulnerability reporting
- [Architecture: encryption layer](../../developers/architecture/#encryption-layer) — where this lives in the codebase
