# durian API: what a scalable GPUI client needs

Notes from wiring the GPUI UI (`crates/mail`, `crates/calendar`) to durian's HTTP API
(`openapi.yaml` in julion2/durian, read side only so far). Each gap names what the client does
today and the endpoint or field that would remove the workaround. Ordered by impact on large
mailboxes (tens of thousands of threads).

## 1. Paging for `/search`

**Today:** `limit` only, no offset or cursor. "Load more" means re-requesting the same query
with a larger limit, so the server re-does and re-sends everything already shown. That is
O(n²) over a scroll session.

**Proposal:** keyset pagination, stable under new mail arriving at the top.

```
GET /search?query=…&limit=200&after=<cursor>
→ { ok, results: [...], next_cursor: "1728468000:9f86d081884c7d65" | null }
```

The cursor is `(timestamp, thread_id)` of the last row. The server continues strictly after it
in the result order. The client's list model already treats results as pages, so adopting
this changes one call.

## 2. List-ready search rows

**Today:** `MailSearchResult` has `thread_id, subject, from, date, timestamp, tags` (tags as one
comma string). A mail list also needs, per row:

| field | why |
|---|---|
| `snippet` | the preview line (otherwise the client must `enrich` and download whole threads) |
| `message_count` | "(5)" next to the participants |
| `participants` (names, latest first) | "Mara, Tobias, me" |
| `account` | account filter / color, without loading the thread |
| `has_attachment` | the paperclip column |
| `tags: string[]` | same shape as on messages |

With these, `enrich` can stay small (the opened thread plus a few ahead), and the first paint
of a 2 000-row inbox is one small response.

## 3. Change notifications beyond new mail

**Today:** SSE `/events` carries `new_mail`, `outbox_update` and `calendar_updated`. Tag
changes made by the CLI (`durian tag` writes the DB directly), by tag-sync pulls or by
provider sync emit nothing. A client can only re-run its searches, which is fine for a
200-row inbox and wasteful for a large one.

**Proposal**, either of:

- an SSE event `tags_changed { thread_ids: [...], account }`, emitted from the store's tag
  journal (which every writer goes through) rather than from the HTTP handler; or
- a change log: `GET /changes?since=<seq>` → `{ seq, threads_changed: [...],
  threads_removed: [...] }`, with SSE only carrying "something changed, seq=N". This also
  survives reconnects, because the client resumes from its last `seq` (the SSE stream today
  has no replay).

The second is more work, but it is what makes a long-running client correct.

## 4. Fetch several threads at once

**Today:** one `GET /threads/{id}` per thread. Prefetching the next few in the direction of
travel means N requests.

**Proposal:** `GET /threads?ids=a,b,c` (or `POST /threads/batch`) → `{ threads: {id: ThreadContent} }`.

## 5. Calendar list fields

**Today:** `/calendars/events` has no organizer or attendees, so "is this my event or an
invitation?" needs a `/calendars/event` request per event. The client does that lazily when an
event is selected.

**Proposal:** add `organizer_email` (or `is_organizer: bool`) and `attendee_count` to the
summary. `my_response: organizer` already exists, but only covers events with an attendee list.

## 6. Smaller things

- **`account` in `/calendars*` is required, but there is no `GET /accounts`.** The client
  derives accounts from message rows, which misses accounts with no mail in the loaded set.
- **Compression:** large `enrich` responses (HTML bodies) would benefit from gzip; ureq
  supports it transparently.
- **Query capability:** durian's date offsets are whole days, and `m` means months. A
  `date:5h..` / `date:30min..` form would let the client stop cutting hour windows locally.
- **Search as you type:** the text index is blind (HMAC tokens of whole words), so the server
  can't match a word by its beginning. The client narrows locally while a word is being typed
  and sends it once typing pauses. A prefix-capable index (e.g. tokens of each word's first
  3–5 characters as well) would remove the pause.
- **Accounts in queries:** there is no `account:` field; the client scopes with `path:<account>/`,
  which works, but reads like an implementation detail.
- **Errors:** responses carry `{ok:false, error}` text only. A machine-readable `code`
  (`bad_query`, `not_found`, `locked`) would let the UI react precisely, e.g. highlight the
  bad query term.

## 7. Measured against a real mailbox (0.4.0)

- **Every search has a fixed cost of ~100–500 ms**, independent of `limit` (50 or 1000 rows) and
  `enrich`; `version` answers in ~4 ms. The cost is in evaluating the query, not in the rows.
  This is what a user waits for on every view switch and search – worth profiling server-side.
- **`enrich` content has no `html`**, only `/threads/{id}` does. The client treats enriched
  content as a list preview and opens threads through `/threads`.
- **Attachments include embedded parts.** `disposition` and `content_id` are enough to tell
  them apart (the client hides parts the HTML references and inlines them as `data:` URIs);
  a `referenced: bool`, computed where the HTML is parsed anyway, would save every client
  that work.

## What the client does meanwhile

- Every view is a server search (base query of the view + translated user query). Results
  are kept per page, and only the open thread and a few ahead are fetched in full.
- Stale answers are dropped: each search carries a generation number, and typing is
  debounced.
- Everything goes through the background executor. The HTTP agent reuses connections.
- **Read-only:** GET endpoints only. Writes (tags, send, drafts, calendar) come later as
  intents, with optimistic UI and rollback on failure.
