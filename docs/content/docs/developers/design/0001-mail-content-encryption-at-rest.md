---
title: "ADR-0001: Mail content encryption at rest with searchable blind-token FTS5"
weight: 1
---

- **Status:** Proposed
- **Date:** 2026-05-17
- **Author:** @julion2 (solo project; external crypto review tracked in §Open Questions)
- **Issue:** [#217](https://github.com/julion2/durian/issues/217)
- **Supersedes:** —
- **Superseded by:** —

## TL;DR

Encrypt sensitive columns of `email.db` and `contacts.db` at the application
layer using AES-256-GCM with per-field random nonces. Master key lives in the
OS keychain (macOS Keychain / libsecret); per-purpose sub-keys are derived via
HKDF-SHA256. Stay on `modernc.org/sqlite` (pure-Go, no driver change).

Body/subject/header search keeps working via a **blind-token FTS5 index**:
plaintext is segmented via [`github.com/rivo/uniseg`](https://github.com/rivo/uniseg)
(pure-Go UAX #29 word-boundary implementation), each token is run through
HMAC-SHA256-truncated-to-80-bit, and the resulting opaque token strings are
indexed in a regular FTS5 virtual table. Tokenization combines **word-level
unigrams and bigrams** for phrase-query support up to 2 words.

What this defends against: forensic recovery of deleted DB pages, unencrypted
Time Machine / iCloud Drive backups, theft of the DB file from an unlocked
filesystem. What it does **not** defend against: memory dumps of the running
app, an attacker who has the OS keychain, statistical analysis of FTS5 token
frequencies over time.

## Context

Durian stores email bodies, subjects, headers, attachments metadata, drafts,
outbox content (`email.db`) and the contact directory (`contacts.db`) as plain
SQLite databases under `~/.local/share/durian/` (XDG) on both macOS and Linux.
FileVault / LUKS cover the common at-rest case but the issue ([#217]) lists
concrete gaps:

- unencrypted Time Machine / external backups
- iCloud Drive sync of `~/` contents (Documents/Desktop sync, etc.)
- device loss without FDE enabled
- forensic recovery of deleted SQLite pages (SQLite does not overwrite freed
  pages by default)

We investigated three families of solutions and rejected two of them. The
discarded paths are documented in **Alternatives Considered** below so the
trade-off chain stays auditable.

The current schema lives in `cli/internal/store/store.go` (table `messages`,
FTS5 virtual table `messages_fts`, plus `tags`, `attachments`,
`message_headers`, `outbox`, `local_drafts`, `tag_journal`, `metadata`) and
`cli/internal/contacts/db.go` (table `contacts`). Search is done via FTS5
`MATCH` on `messages_fts(subject, from_addr, to_addrs, body_text)` —
see `cli/internal/store/search.go:445`. Triggers `messages_ai/ad/au` keep the
FTS5 shadow tables in sync.

Existing keychain plumbing (`cli/internal/keychain`) already shells out to
`security` on macOS and `secret-tool` on Linux; it currently stores IMAP/SMTP
passwords (`durian-password / <email>`) and OAuth tokens (`durian / <email>`).

## Decision

### 1. Encryption library and primitive

- **AES-256-GCM** via Go stdlib `crypto/cipher.NewGCM(crypto/aes.NewCipher(key))`.
  No third-party crypto. Random 12-byte nonce per encryption from
  `crypto/rand`.
- Stored format per encrypted field (single `BLOB` column):
  `version(1B) || nonce(12B) || ciphertext || tag(16B)` where `version=0x01`
  identifies the cipher suite. Future rotation to ChaCha20-Poly1305 would bump
  the version byte; readers route on the version prefix.
- We do **not** use AES-GCM-SIV. Standard AES-GCM with random nonces gives
  ~2^96 unique nonces — at realistic mail volumes the collision probability is
  negligible. SIV is a third-party dep we don't need.
- Rejected for hot-path encryption: `github.com/FiloSottile/age` (Filippo
  Valsorda's modern file-encryption tool — excellent design, but the API
  is stream-and-recipient-oriented for encrypt-once-decrypt-later file
  workflows. For per-field encryption with thousands of small blobs per
  mailbox, the ~200-byte age header per blob plus the stream-wrapping
  overhead don't pay for themselves; AES-GCM with stdlib gives the same
  primitive guarantees with 29 bytes of envelope overhead and a direct
  byte-slice API). Also rejected: `nacl/secretbox` (uses XSalsa20-Poly1305,
  fine cryptographically but no stdlib availability on newer Go versions
  without `golang.org/x/crypto`).
- age **is** used for one specific path: the `master-key export` / `import`
  subcommands (§5). That is exactly the use case age was built for — a
  single file, encrypted once with a user passphrase via scrypt, decrypted
  later — and using it there spares us from rolling a passphrase-KDF
  ourselves for disaster recovery. Concretely: `durian master-key export
  --out master.age` writes an age-passphrase-encrypted blob; the matching
  `import --from master.age` prompts for the same passphrase and restores
  the master into the keychain. age is a runtime dependency of the CLI,
  but only on the export/import code path — it is **not** linked into any
  encrypt/decrypt path that runs during normal mail operations.

### 2. Key hierarchy

Single 32-byte **master key** in the OS keychain, derived sub-keys via
HKDF-SHA256 (Go stdlib `crypto/hkdf`, promoted from x/crypto in Go 1.24 —
no new direct dependency added).

| Sub-key            | HKDF info string         | Purpose                                |
| ------------------ | ------------------------ | -------------------------------------- |
| `body_key`         | `"durian/v1/body"`       | encrypts `messages.body_text/html`     |
| `subject_key`      | `"durian/v1/subject"`    | encrypts `messages.subject`            |
| `headers_key`      | `"durian/v1/headers"`    | encrypts `message_headers.value`       |
| `addrs_key`        | `"durian/v1/addrs"`      | **(retired, see §3)** previously encrypted `from_addr`, `to_addrs`, `cc_addrs` |
| `draft_key`        | `"durian/v1/draft"`      | encrypts `local_drafts.draft_json`, `outbox.draft_json` |
| `contact_key`      | `"durian/v1/contact"`    | **(retired, see §3)** previously encrypted `contacts.email`, `contacts.name` |
| `fts_token_key`    | `"durian/v1/fts-token"`  | HMAC key for blind FTS5 tokens (§4)    |
| `meta_key`         | `"durian/v1/meta"`       | encrypts `mailbox` name, custom `flags`, `account` string |

`addrs_key` and `contact_key` are still derived for forward-compatibility
(the v11→v12 migration populated `from_addr_ct` / `to_addrs_ct` /
`cc_addrs_ct` columns under `addrs_key`; the step-6 contacts migration
populated `email_ct` / `name_ct` under `contact_key`) but no current
read or write path consumes either — see §3 for why message addresses
and contact rows both ended up staying plaintext. Removing the derivation
is a no-op cleanup that would force a sub-key index renumber on every
existing install; deferred until a future migration touches the keyring
shape for unrelated reasons.

Keychain layout (new):

| Service       | Account   | Value                                |
| ------------- | --------- | ------------------------------------ |
| `durian-db`   | `master`  | 64-char hex of 32 random bytes       |

Existing services (`durian`, `durian-password`) stay untouched.

Rotation: a sub-key rotation is a per-purpose re-encrypt without touching the
master. A master rotation requires re-encrypting every field — out of scope
for V1, will get its own ADR when needed.

### 3. Schema changes

Affected columns become `BLOB`. Plaintext `TEXT` columns are dropped after
migration. Migrations are additive, gated on a new `schema_version` step.

`messages` table:

| Old column      | New column       | Notes                            |
| --------------- | ---------------- | -------------------------------- |
| `subject TEXT`  | `subject BLOB`   | encrypted with `subject_key`     |
| `body_text TEXT`| `body_text BLOB` | encrypted with `body_key`        |
| `body_html TEXT`| `body_html BLOB` | encrypted with `body_key`        |

`from_addr`, `to_addrs`, `cc_addrs` **stay plaintext TEXT** — moved
from the encrypted set during implementation. Rationale: these
addresses are already plaintext on the wire (IMAP server logs,
recipient-side `From:` headers, anti-spam routing, sending-server
`Return-Path:`), at every Mail Transfer Agent they cross, and in the
recipient's Sent folder. Encrypting them at rest while they shout on
the wire is an asymmetric, low-value tradeoff. The two implementable
alternatives both fail their cost/benefit test:

- **Determinstic-HMAC search tokens** ([Naveed et al, "Inference
  Attacks on Property-Preserving Encrypted Databases", CCS 2015])
  enable substring search but leak token-frequency tables that are
  trivially attackable for address-class data — `com`, `gma`, `info`,
  `support` are unambiguous markers in any normal mailbox.
- **Char-trigram FTS5** preserves substring-search UX but produces a
  dense, skewed frequency distribution that yields *strictly more*
  leakage than the plaintext-on-disk we'd be trying to avoid (the
  attacker who can build a trigram histogram over your DB already has
  the file; the plaintext just spares them the statistical step).

The cost side: `from:partial` substring queries (e.g. `from:ali`
matching `alice@example.com`) are a real UX habit that any
blind-FTS-MATCH path breaks at word boundary. Acceptable plaintext
leak, undebatable UX preservation.

Out of scope for V1: an opt-in power-user encrypt-from-to flag for
threat models where the local DB is genuinely the only place these
addresses live (e.g. an air-gapped mail archive). Will get its own
ADR if asked for.

Folder names, IMAP keywords and account email addresses can themselves be
sensitive (`Archive/Healthcare`, `Drafts/Resignation`, `$Confidential`,
`alice@workplace.example`). They are therefore **also encrypted**, with
plaintext integer surrogate columns kept for indexing and joins:

| Old column      | New columns                          | Notes |
| --------------- | ------------------------------------ | ----- |
| `mailbox TEXT`  | `mailbox_id INTEGER` (FK) + new `mailboxes(id INTEGER PK, name BLOB)` | name encrypted with `meta_key` |
| `account TEXT`  | `account_id INTEGER` (FK) + new `accounts(id INTEGER PK, name BLOB)`  | name encrypted with `meta_key` |
| `flags TEXT`    | `is_seen INTEGER`, `is_flagged INTEGER`, `is_deleted INTEGER` (0/1) + `flags_other BLOB` | the three hot-path booleans stay plaintext to keep "show unread" / "show flagged" / "hide deleted" UI filters O(1) instead of O(n)+decrypt; everything else (custom keywords like `$Sensitive`, `\Answered`, `\Draft`, `\Recent`, user-defined keywords) goes encrypted with `meta_key`. The plaintext booleans leak per-message activity patterns when correlated with `date` and `mailbox_id` — documented in Threat Model. |

**Keychain-lookup interaction.** Today the keychain is keyed by email address:
`durian / <email>` for OAuth tokens, `durian-password / <email>` for IMAP
passwords (see `cli/internal/keychain/`, `cli/internal/oauth/keychain.go`).
With `accounts.name` encrypted, every keychain lookup at sync startup now
demands a prior decrypt of the `accounts.name` BLOB to reconstruct the
lookup string. Acceptable cost (one decrypt per account, cached for the
process lifetime). Migration of the existing keychain entries to be keyed by
`account_id` instead is deliberately **out of scope** for V1 — it would
orphan installed keys and force every user to re-auth on upgrade. A future
ADR may revisit if multi-account UX demands it.

Columns that **stay plaintext** with a documented information leak:

- `id`, `date`, `created_at`, `uid`, `size`, `fetched_body` — numeric, no PII.
- `message_id`, `thread_id`, `in_reply_to`, `refs` — RFC-5322 IDs. **Leak:**
  thread topology and, for some providers (Gmail, Microsoft), the sending
  server's domain via the ID suffix. Forensic analysts can reconstruct
  conversation graphs from these alone. Accepted because threading without a
  plaintext join key would require either FTS5-based ID reconstruction
  (heavy) or a separate encrypted-but-deterministic ID derivation (opens
  deterministic-encryption side channels). See Threat Model §6.

`message_headers.value` → `BLOB`. Header `name` stays plaintext (already a
small enumerable set per RFC 5322).

`contacts.email` and `contacts.name` **stay plaintext TEXT** — moved
from the encrypted set during implementation (step 7g). Same β-revision
reasoning as `from_addr` / `to_addrs` / `cc_addrs` above: every contact
row in the directory was either harvested from an incoming `From:` /
`To:` / `Cc:` header or typed into a `compose` field that immediately
becomes a wire address. Encrypting the local mirror while the same
addresses shout on every wire hop is asymmetric protection. The cost
side is concrete: `UNIQUE(email)` upsert and the `email LIKE 'ali%'` /
`name LIKE 'ali%'` autocomplete that the compose-recipient picker
relies on both need plaintext indexes — neither survives random-nonce
AES-GCM, and a deterministic-token replacement (Naveed et al, CCS 2015)
would leak a contact-frequency histogram that re-identifies the user's
top correspondents from the ciphertext alone. `id` stays plaintext
(UUID), `source`, `last_used`, `usage_count`, `created_at` stay
plaintext as before.

`outbox.draft_json`, `local_drafts.draft_json` → `BLOB`.

Indexes that referenced encrypted columns (e.g. `idx_messages_from_addr`) are
**dropped**. They are unusable on ciphertext. Replacement: the blind-token
FTS5 index covers the common search cases; for exact lookups on `from_addr`
we accept full-scan + decrypt or rely on the FTS5 path.

### 4. Searchable blind-token FTS5

The current `messages_fts` virtual table is rebuilt as an **external-content
FTS5 table over opaque HMAC tokens** instead of plaintext.

Pipeline on insert/update (encapsulated in Go, run inside the existing
trigger replacement code path):

1. Source text per indexed field is normalized: NFC (`golang.org/x/text/unicode/norm`),
   lowercase, diacritic-strip via `transform.Chain(norm.NFD, runes.Remove(unicode.Mn), norm.NFC)`
   (configurable, default ON).
2. Segment into words with `uniseg.NewWords(input)` from
   [`github.com/rivo/uniseg`](https://github.com/rivo/uniseg), a pure-Go
   implementation of Unicode Text Segmentation (UAX #29). This handles CJK,
   combining marks, ZWJ, emoji clusters and RTL scripts correctly — a
   hand-rolled "split on non-letter" pass cannot, and the standard library
   does not ship a UAX #29 word-segmenter. Drop tokens that contain only
   punctuation or whitespace. Stop-words are **not** dropped (see Threat
   Model §6).
3. **(retired)** Earlier drafts had an address-field special case here that
   emitted `local`, `domain`, `local@domain` tokens. With `from_addr`,
   `to_addrs`, `cc_addrs` now kept plaintext (see §3), the special case
   is gone — address search uses `LIKE` on the plaintext column directly.
4. For each token `t`: `tok = base32(truncate(HMAC-SHA256(fts_token_key, t), 10))`.
   Output is a 16-char ASCII string, FTS5-tokenizer-safe. 80-bit truncation
   is chosen over 64 bit to raise the cost of long-term token-frequency
   correlation across multiple DB snapshots (see Threat Model §6). The +23 %
   index-size cost is negligible vs. the encrypted body storage itself.
5. For each adjacent token pair `(t1, t2)` within the same field: emit
   `base32(truncate(HMAC-SHA256(fts_token_key, t1 || "\x00" || t2), 10))`
   into the parallel bigram stream.
6. Insert the unigram-and-bigram blob (space-separated) into `messages_fts`.

FTS5 schema:

```sql
CREATE VIRTUAL TABLE messages_fts USING fts5(
    subject_tokens,        -- unigrams + bigrams for subject
    body_tokens,           -- unigrams + bigrams for body_text
    content='',            -- contentless: we never store plaintext here
    tokenize='ascii'       -- input is already opaque base32 ASCII
);
```

The implementation (`store/store.go` v15→v16 migration) currently uses
the column names `subject_tok` / `body_tok` and an `unicode61` tokenizer
(plus `from_tok` / `to_tok` columns that are kept for the v11→v12
migration's column-set but unused by the read path — see §3 on address
plaintext). Step 7e dropped the parallel plaintext `messages_fts` and
its triggers; the blind-token table is now the only FTS.

Search at runtime:

- User query string is run through the **same** normalization + tokenization +
  HMAC pipeline.
- Single-word query → unigram MATCH.
- Two-word phrase `"alice bob"` → exact bigram MATCH (precise).
- Longer phrase `"alice bob charlie"` → bigram intersection
  `(alice⌒bob) AND (bob⌒charlie)`. May have false positives where the words
  appear in the same mail but not adjacent in that order. False positives are
  filtered post-hoc by decrypting the matching rows and running a
  `strings.Contains` on the original phrase.
- Prefix queries (`alice*`) are **not supported** — opaque tokens have no
  prefix relation to plaintext prefixes. Document this as a known limitation.

Index size: ~2× the plaintext token count (unigrams + bigrams). For a 50k-mail
mailbox averaging 500 body words this is ~50M FTS5 rows, comfortably handled
by SQLite on modern hardware. Initial indexing cost: one-time HMAC over the
whole corpus.

### 5. Migration

V1 migration (`store.migrate()` adds a new version step):

1. Bump `schema_version` to next free integer.
2. Open transaction, for each `messages` row: read plaintext, encrypt each
   sensitive field with the appropriate sub-key, write `BLOB` back via
   `UPDATE`. Same for `message_headers`, `local_drafts`, `outbox`, `contacts`.
3. Drop and recreate `messages_fts` using the new schema, repopulate by
   reading decrypted rows and running them through the tokenizer pipeline.
4. `VACUUM` to reclaim plaintext pages.
5. Run `PRAGMA secure_delete = ON` permanently from this point on (a
   forward-only switch on the DB header).

Since Durian is pre-1.0 and has no production users with multi-GB mailboxes,
the migration runs synchronously at first open after upgrade. Progress is
logged. If interrupted, the transaction rolls back; on next open we detect
the half-migrated state via `schema_version` and resume.

Master key bootstrap: if `durian-db / master` is missing on first run of an
encrypted-build, generate via `crypto/rand`, store, derive sub-keys, proceed.
If it's missing on a DB that's already migrated → hard error with explicit
recovery instructions. Never silently delete or recreate.

**Master-key loss — recovery procedure.** A lost or wiped keychain entry on
an already-migrated DB means the encrypted columns are unrecoverable. The
impact differs per data class and the error message **must** spell this out:

- IMAP-sourced mails (`messages` rows with a server-side UID): **recoverable
  by resync.** Move the broken DB aside, re-add the account, IMAP refetches
  bodies and headers from the server.
- `local_drafts` and unsent `outbox` rows: **lost.** These exist only
  locally; encrypted bodies of unsent drafts cannot be reconstructed.
- `contacts.db` rows added via the local CLI (`source='manual'`): **lost.**
  Rows imported from IMAP (`source='imported'`) regenerate on next sync.

To close the foot-gun, V1 ships a `durian master-key export --out FILE`
subcommand. It prints the 64-char hex master key (after re-authenticating
via the OS keychain prompt) so users can write it to a password manager or
an offline backup. The complementary `durian master-key import --from FILE`
restores it. Both subcommands log a single audit line; no auto-export ever
happens. This is V1 scope because losing unsent drafts on a keychain mishap
is a once-and-they-leave class of bug.

### 6. Threat model

Defends against:

- Forensic recovery of deleted DB pages (combined with `PRAGMA secure_delete = ON`).
- Unencrypted Time Machine / iCloud Drive backups containing the DB file.
- Theft of the DB file when the filesystem is not FDE-encrypted.
- Casual `sqlite3 email.db ".dump"` from another user account on the same
  machine (assuming the keychain is locked).

Does **not** defend against:

- Memory dumps of the running `durian serve` process — sub-keys and recently
  decrypted plaintext live in RAM. See **Memory hygiene** below for the
  partial mitigation we do apply.
- An attacker who has unlocked the OS keychain (`security unlock-keychain`,
  or a malicious app prompted-and-allowed by the user).
- Statistical correlation attacks: an attacker who can observe the FTS5
  index over time can learn token frequencies. Stop-words are deliberately
  **not** dropped to mitigate this (dropping public-list stop-words would
  let an attacker fingerprint mails by which tokens are missing). 80-bit
  HMAC truncation puts the per-token-pair collision probability at ~2^-40;
  for a 25M-token mailbox we expect single-digit collisions, which manifest
  as false-positive matches the decrypt-and-verify pass filters out.
  Cryptographically the collision rate is irrelevant — integrity is carried
  by AES-GCM tags, not by the FTS index — but the wider token space raises
  the cost of frequency analysis across repeated DB snapshots (Time Machine
  / iCloud).
- Side channels in HKDF / HMAC / AES-GCM implementations (we rely on stdlib).
- A flawed Durian implementation: V1 ships without external cryptographic
  review (see Open Questions §3 for the deferred-review policy). Cipher
  suite is identifiable via the version byte so rotation remains possible
  if review surfaces a primitive change. The encryption code is
  concentrated in one `internal/dbcrypto` package for audit ergonomics.
- Per-message read/flag/delete activity is observable via the plaintext
  `is_seen` / `is_flagged` / `is_deleted` booleans correlated with `date`
  and `mailbox_id`. Accepted trade-off (§3) to keep common UI filters
  O(1). An attacker with repeated DB snapshots over time can infer when
  the user reads, flags, or deletes mail per mailbox. Custom IMAP
  keywords (`$Sensitive`, `$Confidential`, etc.) are **not** in this leak
  — they sit in the encrypted `flags_other`.
- Information leakage from `message_id` / `thread_id` / `in_reply_to` /
  `refs` (see §3): thread topology and sometimes provider domain remain
  observable. Mitigation: none in V1 — a future ADR may revisit if a
  realistic threat model demands it.
- Information leakage from `from_addr` / `to_addrs` / `cc_addrs` (see
  §3): an attacker with the DB file can read every sender/recipient
  address verbatim plus the frequency of correspondence per address —
  enough to reconstruct the user's communication graph. Mitigation:
  none, deliberately. The same data is already exposed at the IMAP
  server, all MTAs in transit, the recipient's Sent folder and
  anti-spam routing logs; encrypting it at rest while it shouts on
  the wire is asymmetric protection. The two implementable encrypted
  alternatives both fail their cost/benefit test (deterministic-HMAC
  search tokens leak token-frequency tables — Naveed et al CCS 2015;
  char-trigram FTS5 leaks dense skewed trigram frequencies that yield
  *more* information than the plaintext we would be hiding). Future
  ADR may add an opt-in encrypt-from-to flag for users whose threat
  model puts the local DB as the genuinely-only-place these addresses
  live (air-gapped archive, etc.).
- Information leakage from `contacts.email` / `contacts.name` (see
  §3): the directory is the projected union of every `From:` / `To:` /
  `Cc:` the user has ever seen plus the addresses typed into compose.
  An attacker with `contacts.db` recovers the user's full address book
  and (via `usage_count` + `last_used`, also plaintext) a ranking of
  who they correspond with most. Mitigation: none, deliberately. Same
  on-the-wire-anyway argument as messages-side addresses; same
  cost/benefit failure for the implementable encrypted alternatives.

### Threat-actor personas

Reviewers find it easier to validate a threat model against named personas
than against a flat enumeration. The four personas this ADR's design is
scored against:

| # | Persona                                       | What they have                                     | Defended? |
| - | --------------------------------------------- | -------------------------------------------------- | --------- |
| 1 | Forensic analyst with a full filesystem image | Cold copy of `~/.local/share/durian/`, no keychain | **Yes** — ciphertext + secure_delete + locked keychain |
| 2 | Cloud-backup provider under compulsion        | Time Machine / iCloud snapshot of the DB file      | **Yes** — same as #1; backups carry ciphertext only |
| 3 | Malware with user-level live access           | Read access to the running process's memory and keychain | **No** — sub-keys and plaintext live in RAM; OS keychain is unlocked for the user session |
| 4 | Stolen unlocked laptop                        | Running session, browser, terminal                 | **Partial** — same as #3 while the lid is open; closing the lid + FileVault re-locks the keychain |

Persona 3 is explicitly out of V1 scope. Defense there requires a hardware
key, kernel-level memory protection, or moving the crypto into a separate
process — all bigger investments than this ADR.

### Memory hygiene

Go's GC can make heap copies, so wipe is best-effort, not guaranteed. We
still:

- Derive sub-keys once at process start, store them in a `keyring` struct,
  keep them for the process lifetime (rederivation per row is wasteful, and
  the master-key exposure window doesn't shrink either way).
- Allocate plaintext buffers with `make([]byte, n)` so they live on the heap
  exactly once; wipe via `crypto/subtle.ConstantTimeCopy` of a zero buffer
  before returning from the decrypt helper. `runtime.KeepAlive` anchors the
  buffer through the wipe.
- Do **not** rely on `[]byte` slice reuse from a sync.Pool for plaintext.
  Pool reuse can leave plaintext stranded in unrelated allocations.
- Document explicitly that the running process is a soft target. Users who
  need memory-dump resistance run an FDE-encrypted suspend / hibernate or
  exit `durian serve` when stepping away.

### Logging audit

A Durian build with encryption is only as private as its least careful log
statement. Mitigations, all part of the implementation PR:

- Pre-merge grep gate: CI runs
  `grep -rnE 'slog\.|log\.|fmt\.Print' cli/ | grep -E 'subject|body_text|body_html|from_addr|to_addrs|cc_addrs|email|draft_json'`
  and fails if matches appear outside an explicit allow-list. The allow-list
  is reviewed in this ADR's follow-up issue.
- A small `slog.Handler` wrapper in `cli/internal/redact` runs every attr
  value through a redactor that replaces any string matched against the
  encrypted-field registry with `[REDACTED]`. The registry is generated
  from the encrypted-column list above. Defense in depth: even if a future
  contributor adds `slog.String("subject", msg.Subject)`, the wrapper
  scrubs it.
- IMAP / SMTP error paths sanitized: server responses can echo back
  Subject lines. The IMAP layer's error wrapper truncates and base64s any
  string that's longer than 80 characters, with a comment pointing at this
  ADR.
- Stack traces are stripped of sensitive arguments by using `errors.New` /
  `fmt.Errorf("...: %w", err)` with redaction-aware `%w` wrapping; we do
  **not** use `pkg/errors.Wrap` with full struct dumps.

### 7. Out of scope (separate future ADRs)

- Master-key rotation procedure.
- Encrypted attachments-on-disk (`attachments` table only stores metadata
  today; raw attachment bytes live in the IMAP server cache or are fetched
  on demand).
- Sync server (`sync/main.go`): different data domain (tag changes only, no
  user content). Stays unencrypted.
- Linux-Wayland-clipboard plaintext leakage of decrypted bodies — UI concern.
- Multi-profile / multi-master-key support.

## Consequences

**Positive:**

- Issue #217's threat-model is addressed without dropping pure-Go SQLite.
- No CGo, no SQLCipher fork hunt (see Alternatives §3), no Bazel cc-toolchain
  cross-compile pain. Build pipeline stays as-is.
- Cipher suite is identifiable by version byte → forward-compatible.
- HKDF means we can roll one sub-key without touching the master.

**Negative:**

- Schema migration is a one-way door for existing DBs. Mitigated by pre-1.0
  status.
- Prefix search (`alice*`) is gone.
- Long phrase queries can hit the post-decrypt false-positive filter, making
  them slower than today on huge mailboxes.
- We are rolling our own searchable-encryption scheme. Even with conservative
  primitives this is the highest-risk part of the design. External review
  is deferred per Open Questions §3 but remains a non-negotiable pre-1.0
  gate; the code is structured (single `internal/dbcrypto` package, version
  byte for cipher rotation, RFC test vectors in CI) to make that review
  tractable when the trigger fires.
- Memory footprint goes up: decrypted hot rows are held in RAM longer (sub-key
  re-derivation per row would be wasteful).
- Logging discipline becomes a hard requirement — a single careless
  `slog.String("subject", ...)` defeats the whole scheme. CI gate + redact
  wrapper documented in the *Logging audit* section above.
- Three new tables (`mailboxes`, `accounts`) and a new BLOB columns layout
  meant every query that previously joined on `messages.mailbox` /
  `messages.account` got a JOIN rewrite (executed in step 7f when the
  plaintext shadow columns were finally dropped). Same churn for the
  `messages.flags` TEXT column, now reconstructed from
  `is_seen` / `is_flagged` / `is_deleted` + the encrypted `flags_other`
  BLOB on every read.

**Neutral:**

- Performance: AES-GCM with AES-NI is ~1-2 GB/s per core; on a 50k-mail
  mailbox with average 5 KB body the full-decrypt-scan worst case is
  ~250 MB → well under a second. Initial migration is bounded by the same.

## Alternatives Considered

### Alt 1 — SQLCipher via cgo (rejected)

The original direction suggested by issue #217. Investigated `mutecomm`,
`xeodou`, `thinkgos`, `openprivacy`, `Boolean-Autocrat` and even
`mattn/go-sqlite3` itself.

Findings: upstream `mattn/go-sqlite3` does **not** ship SQLCipher. Every
existing Go SQLCipher binding is a derivative of `mutecomm/go-sqlcipher`,
pins a 2020-era SQLCipher 4.4.x, and has zero-to-one active maintainers.
`openprivacy/go-sqlcipher` is the only one with a real production user
(Cwtch), but lives on a Gitea instance with an exotic import path and is
still bound to an old upstream baseline.

Adopting any of them requires: cgo, libtomcrypt or system OpenSSL on Linux,
cross-toolchain story in CI (we currently cross-compile linux/{amd64,arm64}
from macOS in release.yml — pure-Go-only), and accepting unreviewed
crypto-relevant code that lags upstream by 5+ years.

For a 2026-greenfield project with active development the cost/benefit is
clearly negative. The only Win over Alt 0 (this ADR) would be transparent
column-level access — at the price of a moribund ecosystem.

### Alt 2 — `ncruces/go-sqlite3` + `vfs/adiantum` (rejected)

Pure-Go SQLite (Wasm via wazero) with an Adiantum-encrypted VFS shipped by
the same author. Actively maintained, NIST-XTS variant also available, and
transparent: search and SQL keep working unchanged.

Why rejected: it's a full driver swap from `modernc.org/sqlite` to
`ncruces/go-sqlite3`. The latter runs SQLite inside a Wasm sandbox per
connection, raising RAM use and changing the threading model. Performance
benchmarks are competitive but not equivalent. The encryption is whole-file,
which is exactly what we explicitly did **not** want for issue #217 — we want
per-field control (e.g. headers vs body) so that we can keep useful indexes
on metadata columns. Adiantum gives us no such granularity.

Worth revisiting if a future ADR ever needs transparent whole-file at-rest
encryption with no schema impact.

### Alt 3 — keep modernc, do nothing beyond `PRAGMA secure_delete = ON` (rejected)

Cheapest option. Closes the "forensic recovery of deleted pages" sub-bullet
of #217 and nothing else. Does not address backups or stolen-DB scenarios.

Rejected because the threat model in #217 explicitly enumerates the backup
and stolen-DB cases as in-scope.

### Alt 4 — column encryption with **unigram-only** blind tokens (rejected as V1)

Same as the accepted design but tokenize on unigrams only.

Trade-offs:

- Index size ~50% of the bigram variant.
- Phrase queries degrade to boolean AND of words — `"from sales"` matches
  every mail containing either word. For a mail client where "find by
  sender + subject phrase" is a hot path this destroys search precision and
  thereby the "mail you can grep" value prop.
- Implementation is trivially simpler.

Rejected because the search-quality regression is user-visible and severe.
Bigram cost is modest.

### Alt 5 — column encryption with **character-trigram** blind tokens (rejected)

Tokenize on overlapping 3-character substrings.

Pros: substring/prefix search works (`"vertr*"` finds `vertrag`), no
language-specific tokenizer needed, robust against CJK and other
non-whitespace-delimited scripts.

Cons: index explodes (~5-10× plaintext byte size), each query becomes a
multi-trigram intersection, and at scale the FTS5 query planner struggles.

Worth a follow-up ADR if we discover that word-tokenization is breaking
on non-Latin scripts in real user data.

### Alt 6 — search on plaintext-header fields only, body un-searchable (rejected)

Skip the FTS5 blind-token machinery entirely. Keep subject/from/to as
encrypted columns, drop body search.

Rejected: body search is a stated value proposition.

### Alt 7 — `b` + linear-decrypt-scan fallback for body (rejected as V1)

Index only plaintext-header tokens in FTS5, expose a `--full` flag that
linear-scans + decrypts every body for substring match.

Considered seriously as the lower-risk V1. Rejected in favor of the
blind-token approach because:

- Blind-token FTS5 is **not meaningfully more complex** than linear-scan
  once you already have HMAC-and-encrypt machinery for the column-level
  scheme. The crypto code is the same; the FTS5 schema is a few extra
  CREATE statements. "Simpler" was the apparent argument for Alt 7, but
  it doesn't survive contact with the actual implementation surface.
- Linear-decrypt-scan at 50k+ mails is multi-second on cold cache, which
  re-creates the "search is slow → don't use search" UX problem.
- A two-step rollout (no body search → blind-token body search) means a
  second user-visible migration of `messages_fts`.

The linear-scan-with-decrypt path is still useful as the **false-positive
filter** for long-phrase queries — see §4. Keeping it as a single
non-optional codepath rather than a UI flag.

## Implementation plan

V1 is intentionally a big surface, so the implementation lands as a sequence
of small PRs rather than one monolith. Each step leaves `main` green and
user-runnable.

1. **Schema-only migration.** Add `mailboxes` / `accounts` tables, the
   `mailbox_id` / `account_id` FK columns, and the new BLOB column shapes
   — all populated from existing plaintext via `INSERT ... SELECT`. No
   encryption yet; BLOBs hold raw UTF-8. Verifies that the new joins
   compile and that the migration path is reversible. **Shipped.**
2. **`cli/internal/dbcrypto` package.** Isolated crypto primitives: AES-GCM
   encrypt/decrypt with the `version || nonce || ct || tag` envelope, HKDF
   sub-key derivation, RFC-5869 test vectors, AES-GCM round-trip tests,
   `crypto/subtle` wipe helper. Zero callers yet. **Shipped.**
3. **`cli/internal/redact` slog wrapper + CI grep gate.** Lands before any
   encryption goes live so the gate is enforced from the first day real
   plaintext flows through encryption code paths. **Shipped.**
4. **Master-key bootstrap + `master-key export/import` subcommands.**
   Keychain plumbing, recovery procedure UX, audit-log line. Tested without
   any DB writes yet. **Shipped.**
5. **Pilot encryption: `subject` column only.** Smallest blast radius,
   exercises end-to-end (migration of one column, encrypt-on-write,
   decrypt-on-read, FTS5 plain-tokens still works because subject FTS5
   rebuild is deferred to step 7). **Shipped (#235, merged).**
6. **Roll out encryption to remaining columns** (body, addrs, headers,
   drafts, contacts, meta). Each as its own PR if it carries a non-trivial
   query rewrite. **Shipped (PRs #236 / #237, draft).**
7. **Blind-token FTS5.** Tokenizer pipeline (uniseg + normalize + HMAC),
   bigram derivation, FTS5 rebuild, search-path rewrite, post-decrypt
   false-positive filter for long phrases. **Shipped** in seven sub-steps:
   - **7 a+b**: blind-token FTS5 infrastructure + parallel index.
   - **7c**: flip search reads from plaintext `messages_fts` to
     `messages_blind_fts`.
   - **7d**: β-revision — `from_addr` / `to_addrs` / `cc_addrs` move
     back to plaintext (substring-search UX vs. wire-plaintext
     asymmetric protection); drop the dead `*_ct` columns.
   - **7e**: drop plaintext `subject` / `body_text` / `body_html` /
     `message_headers.value` / drafts and the old `messages_fts` table;
     VACUUM.
   - **7g**: extend β-revision to `contacts.email` / `contacts.name`
     (same argument as message addresses); drop `email_ct` / `name_ct`.
   - **7f**: drop `messages.mailbox` / `messages.account` /
     `messages.flags` plaintext shadow columns now that the structured
     FK / boolean / blob replacements are written by every path. SQLite
     12-step table-rebuild for the in-place `UNIQUE` constraint swap.
   - **Bigram phrase queries**: activate the unused-at-read-time
     bigrams written by the step-7 a+b tokenizer; quoted phrases
     (`"foo bar"`, `subject:"foo bar"`) now ride the bigram path to
     anchor word order.
8. **`PRAGMA secure_delete = ON`** flipped in `store.Open` and verified by
   a smoke test that asserts the pragma is set on every new connection.
   **Pending** — separate PR after the step-7 stack lands.

Steps 1–4 are pure infrastructure and can ship as alpha-grade without
user-visible change. The first user-visible behaviour change is step 5.

## Open questions

1. **Diacritic stripping default.** Lossless search (`müller` vs `muller`
   distinguishable) vs intuitive matching. Current default: ON, because
   German and French mail users will expect that. Reviewer input welcome.
2. **Stop-word policy.** Strict "no stop-word dropping" is the conservative
   choice; if measured token frequencies on real corpora turn out uniform
   enough that correlation attacks are infeasible, we could reconsider.
   Tracked as a follow-up measurement task post-V1.
3. **External cryptographic review.** Deferred to **pre-1.0 or first
   production user with sensitive data — whichever comes first.** Until
   then this feature ships with an explicit "best-effort, unaudited
   searchable-encryption scheme" disclaimer in release notes and in the
   security policy. The primitives (AES-256-GCM, HMAC-SHA256, HKDF-SHA256)
   are all stdlib; the only novel composition is bigram blind-token FTS5,
   which is a variant of well-understood prior art (Cash et al., "Highly-
   Scalable Searchable Symmetric Encryption with Support for Boolean
   Queries", CRYPTO 2013). Self-review against RFC test vectors is
   mandatory. r/crypto / Filippo Valsorda outreach is mandatory when
   either trigger above fires, not before.
4. **`message_id`/`thread_id` topology leak.** Currently accepted as the
   cost of keeping threading cheap. Open to a future ADR that proposes
   deterministic-but-encrypted IDs if a concrete threat model emerges.

Resolved (kept here for traceability):

- ~~Token truncation width.~~ → 80 bit (§4). Resolved in response to
  early-review feedback re: token-frequency correlation across snapshots.
- ~~Address tokenization.~~ → emit three tokens per address (local,
  domain, full). ~~Resolved in §4 step 3.~~ **Superseded:** addresses
  themselves moved to plaintext (see §3 "from_addr/to_addrs/cc_addrs
  stay plaintext TEXT" + §6 threat-model bullet). Substring search
  served by `LIKE` on the plaintext column; no FTS5 address tokens
  exist in V1.
- ~~Contact encryption.~~ → `contacts.email` and `contacts.name`
  moved to plaintext in step 7g (see §3 + §6 threat-model bullet).
  Same β-revision reasoning as the message-side addresses: contact
  rows are projections of wire addresses, so encrypting only the
  local mirror is asymmetric protection that costs `UNIQUE(email)`
  upsert and prefix-LIKE autocomplete. The dead `email_ct` /
  `name_ct` columns were dropped; `contact_key` is retired but
  still derived (see §2 sub-key table).
- ~~Mailbox / flags / account plaintext leak.~~ → encrypted, with integer
  surrogate columns for indexing (§3). Resolved in response to early-review
  feedback.
- ~~System-flag plaintext leak.~~ → all flags encrypted, not just custom
  ones (§3). Resolved in response to behavior-correlation argument.
- ~~Tokenizer library.~~ → `github.com/rivo/uniseg` (§4 step 2). Resolved
  after verifying no UAX #29 word-segmenter ships in `golang.org/x/text`.
- ~~Account encryption + keychain lookup conflict.~~ → decrypt-on-startup,
  cache for process lifetime; keychain keyed-by-email kept (§3). Resolved
  by accepting one decrypt per account at sync start.
- ~~Bigram phrase-query activation.~~ → `TokenizeFTSPhrase` emits the
  same unigram + adjacent-pair-bigram set that `TokenizeFTS` writes at
  index time; the lexer recognizes `"foo bar"` and `subject:"foo bar"`
  as single phrase tokens with `phrase=true`; `exprToSQL` /
  `fieldToSQL` dispatch to the phrase tokenizer. The bigrams that step
  7 a+b put into the index now anchor word order at query time, and
  word-AND searches keep the unigram-only `TokenizeFTSQuery` path so
  they don't accidentally promote into phrase matches. Resolved with
  no new ciphertext, no migration, and a single new dbcrypto function.

## References

- Issue #217: Encrypt SQLite databases at rest.
- Michael Nygard, "Documenting Architecture Decisions", 2011.
- SQLite FTS5 documentation: <https://www.sqlite.org/fts5.html>.
- HKDF: RFC 5869.
- Unicode Text Segmentation: <https://unicode.org/reports/tr29/>
  (`github.com/rivo/uniseg` implements this in pure Go; verified at ADR
  date 2026-05-17 that neither the standard library nor `golang.org/x/text`
  ships a UAX #29 word-segmenter).
- NIST SP 800-38D (AES-GCM nonce uniqueness requirement).
- Cash, Jaeger, Jarecki et al., "Highly-Scalable Searchable Symmetric
  Encryption with Support for Boolean Queries", CRYPTO 2013. Academic
  baseline for blind-token searchable encryption schemes.

[#217]: https://github.com/julion2/durian/issues/217
