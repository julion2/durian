# mail

The mail client's views on GPUI, built with the [`ui`](../ui) crate: sidebar (accounts, tags,
saved views), thread list (split and list layouts), thread view with HTML mail, composer with live
Markdown, drafts, command menu, tag picker, context menus and the shortcut sheet.

## Wiring it up

```rust
ui::init(cx)?;                                   // fonts + component keys
ui::theme::apply_appearance(cx);
mail::bind_keys(cx);                             // mail commands (j/k, e, r, ⌘K …)
mail::set_cache_root(app_cache_dir);             // rendered HTML, outbox
let data: mail::model::MailData = backend.load()?;
cx.open_window(options, |window, cx| cx.new(|cx| mail::MailWorkspace::new(data, window, cx)));
```

`MailData` is the seam to the data source: the user, accounts (with aliases), tags, saved views
and threads. Messages carry their HTML body (if any) and attachments carry a file path; the
views never fetch anything themselves.

## What's inside

| Module | What |
|---|---|
| `model` | `MailData`, `Mailbox` (selection, filters, drafts, read/star/tag state) |
| `query` | tag-based search: `tag:` `from:` `has:` `newer:` …; the adapter translates to the CLI |
| `commands` | one table for key bindings, the command menu and the shortcut sheet |
| `html_render` | WebKit snapshot pool for HTML mail (Swift helper, built by `build.rs`) |
| `mime` | RFC 5322 multipart/alternative for sending (Markdown → text + HTML) |

## Requirements

macOS with the Xcode command line tools: `build.rs` compiles `helpers/mailrender` with
`swiftc`. Set `MAIL_TRACE=1` to log input-to-paint latency and render timings.
