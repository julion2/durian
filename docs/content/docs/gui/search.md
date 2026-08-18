---
title: Search
weight: 3
---

Press `/` (or `Cmd+/`) to open the search popup. It shows live results as you type, ranked by recency, against the SQLite + FTS5 index in your local store.

## Query syntax

Durian uses notmuch-style queries — terms are ANDed by default; `OR` and `NOT` are explicit.

| Term | Matches |
|---|---|
| `inbox` | Free text in subject/body |
| `tag:inbox` | Tagged with `inbox` |
| `from:alice@example.com` | Sender contains substring |
| `to:me@you.com` | Recipient (To) contains substring |
| `cc:team@you.com` | Cc recipient contains substring |
| `subject:invoice` | Subject contains substring |
| `header:list-id:` | Header present (any value). **Note:** only available in `rules.pkl` match clauses, not in search queries yet — tracked in [#265](https://github.com/julion2/durian/issues/265). |
| `header:x-spam-score:5` | Header value contains substring. Same restriction as above. |
| `has:attachment` | Has any attachment |
| `has:attachment:pdf` | Has a PDF attachment |
| `path:work` | Restrict to an **account** (by alias/name). Only the first path segment matters — `path:work/INBOX` is the same as `path:work`; it does not filter by folder. |
| `group:vip` | Anyone in the `vip` group |
| `group:investor/from` | Mail FROM the group |
| `group:investor/to` | Mail TO the group |
| `date:today` | Today |
| `date:1w..` | Last week |
| `date:2024-01-01..2024-06-30` | Date range |

Combine freely with `AND` (also implied between adjacent terms), `OR`, `NOT`,
and parentheses. `OR` returns the **union** — `a OR b` always matches at least as
much as `a` alone. A double-quoted `"exact phrase"` matches those words in order.

```text
group:vip AND tag:unread
from:boss@company.com AND has:attachment:pdf
subject:contract AND date:6m.. AND NOT tag:sent
(from:alice OR from:bob) AND has:attachment
"quarterly report" OR subject:invoice
```

{{< callout type="info" >}}
Searches run against the encrypted-at-rest store: matches from the blind
full-text index are re-checked against the decrypted message before they count,
so a hash collision can never surface a message that does not really contain
your terms.
{{< /callout >}}

## Popup keymap

| Key | Action |
|---|---|
| `Enter` | Open the highlighted thread |
| `Ctrl+J` / `Ctrl+N` | Next result |
| `Ctrl+K` / `Ctrl+P` | Previous result |
| `Escape` | Close popup |

## Saving searches

Any query can become a sidebar folder by adding it to `profiles.pkl`:

```pkl
new { name = "Unread VIPs"; icon = "star"; query = "group:vip AND tag:unread" }
```

See [Sidebar & Profiles](../sidebar-profiles/).

## Counts

The result count next to a folder reflects the same query — sidebar badges are just `GET /search/count` calls under the hood.
