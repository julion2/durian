---
title: keymaps.pkl
weight: 5
---

Override or add keyboard shortcuts. Your file is **merged** with the built-in defaults — only include the bindings you want to change.

For the built-in default set and how chords/contexts work conceptually, see [GUI → Keymaps](../../gui/keymaps/) and [keymaps-example.pkl](https://github.com/julion2/durian/blob/main/docs/keymaps-example.pkl).

## Skeleton

```pkl
global_settings {
  keymaps_enabled = true
  sequence_timeout = 1.0      // seconds to wait for next key in a sequence
}

keymaps {
  new { action = "archive"; key = "e" }                                    // rebind a → e
  new { action = "delete"; key = "dd"; sequence = true; enabled = false }  // disable default
  new { action = "tag_op"; key = "T"; tags = "+todo -inbox" }              // add custom binding
}
```

## Binding fields

| Field | Type | Notes |
|---|---|---|
| `action` | `String` | Built-in action name (see below) |
| `key` | `String` | Single key, or chord like `"gi"`, `"dd"` |
| `modifiers` | `Listing<String>?` | `{ "cmd" }`, `{ "ctrl" }`, `{ "shift" }`, or omit |
| `sequence` | `Boolean?` | `true` for multi-key chords |
| `supports_count` | `Boolean?` | `true` to accept count prefix (`5j`) |
| `enabled` | `Boolean?` | `false` disables a default |
| `context` | `String?` | Where this binding is active |
| `tags` | `String?` | For `tag_op` only — e.g. `"+todo -inbox"` |

## Contexts

| Context | When active |
|---|---|
| `list` (default) | Email list view |
| `thread` | Open thread view |
| `search` | Search popup |
| `tag_picker` | Tag picker popup |
| `compose_normal` | Compose window in normal (vim) mode |

## Action names

A non-exhaustive list — see `keymaps-example.pkl` for the full set:

```text
next_email, prev_email, first_email, last_email, page_down, page_up
archive, delete, toggle_read, toggle_star, tag_picker, tag_op
compose, reply, reply_all, forward
go_inbox, go_sent, go_drafts, go_archive, go_folder
next_folder, prev_folder, folder_picker
search, close_detail, reload_inbox
enter_visual_mode, enter_toggle_mode, toggle_selection, exit_visual_mode
enter_thread, scroll_down, scroll_up, next_message, prev_message
exit_insert    (compose vim mode)
```

## Override semantics

Bindings match by `(key + modifiers + context)`. A binding in your `keymaps.pkl` with the same triple as a default **replaces** the default. To remove a default without replacing, set `enabled = false`.

## Validate

```bash
durian validate keymaps
```
