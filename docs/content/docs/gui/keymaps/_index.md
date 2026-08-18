---
title: Keymaps
weight: 5
---

Durian uses vim-style key bindings throughout the GUI. The key sequence
engine supports **chord sequences** (e.g. `gi`, `gs`, `ga`), **counts**
(`3j`, `5n`), and **modal contexts** — bindings can be scoped to "list",
"thread", "search", "tag picker", "calendar", or "compose normal", and the same key
can do different things in each.

Customize via `~/.config/durian/keymaps.pkl`. Run `durian validate keymaps`
to check your overrides. See [Custom keymaps](#custom-keymaps) below.

## Email list (default context)

Active when you're in the thread list — typically the left/main pane
without a thread expanded.

### Navigation

| Key | Action |
|---|---|
| `j` / `k` | Next / previous email (`3j` for ↓3) |
| `↓` / `↑` | Same as `j` / `k` |
| `gg` | Jump to first email |
| `G` | Jump to last email |
| `Ctrl-d` / `Ctrl-u` | Page down / up |
| `l` / `Enter` | Open thread (enter thread context) |
| `q` / `Esc` | Close detail |

### Folders & profiles

| Key | Action |
|---|---|
| `gi` | Go to Inbox |
| `gs` | Go to Sent |
| `gd` | Go to Drafts |
| `ga` | Go to Archive |
| `g1` … `g9` | Go to user folder slot 1–9 |
| `J` / `K` | Next / previous folder in sidebar |
| `gf` | Folder picker |

### Actions

| Key | Action |
|---|---|
| `r` | Reply |
| `R` | Reply all |
| `f` | Forward |
| `c` | Compose new |
| `a` | Archive |
| `u` | Toggle read / unread |
| `s` | Toggle star |
| `dd` | Delete (move to trash) |
| `t` | Tag picker |
| `/` (or `⌘/`) | Search |
| `⌘r` | Reload inbox |

### Selection mode

| Key | Action |
|---|---|
| `v` | Enter visual mode (range selection) |
| `V` | Enter toggle mode (multi-pick) |
| `Space` | Toggle current row in selection |
| `Esc` | Exit visual / toggle mode |

## Thread (open message)

Active once you've opened a thread with `l` or `Enter`. The cursor now
lives **inside** the thread — `j` / `k` scroll the message body, `n` / `N`
jump between messages within the thread.

| Key | Action |
|---|---|
| `j` / `k` | Scroll thread body down / up (with count) |
| `Ctrl-d` / `Ctrl-u` | Page down / up within the thread |
| **`n`** | **Next message in the thread (`3n` jumps 3 forward)** |
| **`N`** | **Previous message in the thread** |
| `gg` | First message in the thread |
| `G` | Last message in the thread |
| `r` | Reply to the focused message |
| `h` / `Esc` | Close thread, back to list |

`n` / `N` move a per-message focus indicator; combined with a count
(`5n`) you skip several messages at once, which is faster than scrolling
through a long quote-heavy thread.

## Search popup

When the search popup is open (`/`), navigation keys work differently so
they don't collide with typing the query:

| Key | Action |
|---|---|
| `Ctrl-j` / `Ctrl-k` | Next / previous result |
| `Ctrl-n` / `Ctrl-p` | Same |
| `Enter` | Open selected thread |
| `Esc` | Close popup |

## Tag picker

Same pattern when the tag picker is open (`t`):

| Key | Action |
|---|---|
| `Ctrl-j` / `Ctrl-k` | Next / previous tag |
| `Ctrl-n` / `Ctrl-p` | Same |
| `Enter` | Apply selected tag |
| `Esc` | Close picker |

## Compose editor

The compose editor has its own modal vim mode (normal, insert, visual).
See [Vim compose](vim-compose/) for the full reference. One handy default:

| Key | Action |
|---|---|
| `jk` (in insert mode) | Exit to normal mode |

## Calendar

Active in the calendar (enter it with `gc` from the list, or `Cmd+Opt+C`).
`j` / `k` move the event cursor; the rest mirror the mail-list verbs where it
makes sense (`i` edit, `dd` delete, `n` new).

| Key | Action |
|---|---|
| `j` / `k` | Next / previous event (`3j` for ↓3) |
| `gg` / `G` | First / last event |
| `]` / `l` | Next period (week, month, or year — by the active view) |
| `[` / `h` | Previous period |
| `t` | Jump to today |
| `n` / `o` / `O` | New event |
| `i` | Edit the selected event |
| `dd` | Delete the selected event |
| `s` | Toggle the calendar sidebar |
| `D` | Toggle hide-declined |
| `q` / `Esc` | Back to mail |

Switching views (Agenda / Week / Month / Year) and stepping periods also live in
the **Calendar** menu: `Cmd+Opt+C` show/hide, `Cmd+Opt+S` sidebar, `Cmd+Opt+[`
and `Cmd+Opt+]` previous / next period. Editing in the Week grid (drag to move or
resize) is documented in [Calendar](../calendar/#editing-in-the-week-grid).

## Custom keymaps

Override any binding in `~/.config/durian/keymaps.pkl`. The same `key` +
`modifiers` + `context` triple as a default replaces that default; set
`enabled = false` to remove it; any other entry is appended.

```pkl
import "modulepath:/Keymaps.pkl" as K

keymaps: Listing<K.KeymapEntry> = new {
  // Use Shift-J / Shift-K to jump messages in a thread (instead of n / N)
  new { action = "next_message"; key = "J"; context = "thread"; supports_count = true }
  new { action = "prev_message"; key = "K"; context = "thread"; supports_count = true }

  // Disable the default `dd` delete chord
  new { action = "delete"; key = "dd"; sequence = true; enabled = false }
}
```

After editing:

```bash
durian validate keymaps
```

The GUI picks up changes on next launch (or via "Reload Keymaps" if you
have it bound).

### Action reference

The actions available in `keymaps.pkl` (full list also in
`schema/Keymaps.pkl`):

| Action | Notes |
|---|---|
| `next_email` / `prev_email` | List navigation |
| `first_email` / `last_email` | List jump |
| `page_down` / `page_up` | Both contexts |
| `enter_thread` | Open focused thread |
| `next_message` / `prev_message` | **Thread context only** |
| `scroll_down` / `scroll_up` | Thread body |
| `close_detail` | Back to list |
| `archive`, `delete` | Tag mutations |
| `toggle_read`, `toggle_star` | Flag toggles |
| `reply`, `reply_all`, `forward`, `compose` | Composition |
| `go_inbox`, `go_sent`, `go_drafts`, `go_archive` | Folder jumps |
| `go_folder` | User folder (requires `key = "gN"`) |
| `next_folder` / `prev_folder` | Sidebar navigation |
| `folder_picker` | Open folder picker |
| `search` | Open search popup |
| `tag_picker` | Open tag picker |
| `enter_visual_mode`, `enter_toggle_mode` | Selection modes |
| `toggle_selection`, `exit_visual_mode` | Selection control |
| `select_next` / `select_prev` | Within popups (search, tag picker) |
| `exit_insert` | Compose insert → normal mode |
| `reload_inbox` | Trigger a sync + reload |
| `go_calendar` | Enter the calendar (bound to `gc` in the list) |
| `calendar_prev_period` / `calendar_next_period` | Step the calendar window |
| `calendar_today` | Jump to today |
| `calendar_new` / `calendar_edit` / `calendar_delete` | Event create / edit / delete |
| `calendar_toggle_sidebar` / `calendar_toggle_declined` | Calendar view toggles |

All actions accept `supports_count = true` if they're motion-style and
benefit from a numeric prefix (e.g. `5j`, `3n`).
