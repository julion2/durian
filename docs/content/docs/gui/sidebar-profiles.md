---
title: Sidebar & Profiles
weight: 2
---

The sidebar shows folders for the currently-selected profile. Profiles bundle accounts and folder definitions — switch between them with `Cmd+1`, `Cmd+2`, etc.

## Profiles

Defined in `~/.config/durian/profiles.pkl`. Each profile selects one or more accounts and a list of folders (clickable queries) and headers (non-clickable section labels).

```pkl
profiles: Listing<P.ProfileConfig> = new {
  new {
    name = "All"; accounts = new { "*" }; default = true
    folders = P.standardFolders
  }
  new {
    name = "Work"; accounts = new { "work" }; color = "#EF4444"
    folders {
      new { name = "Inbox"; icon = "tray"; query = "tag:inbox AND NOT tag:sent" }
      new { name = "Triage" }                                  // section header
      new { name = "To-Do"; icon = "checklist"; query = "tag:todo" }
    }
  }
}
```

- `accounts = new { "*" }` matches every account.
- `accounts = new { "work" }` matches by alias from `config.pkl`.
- `color` tints the active-profile indicator.
- An entry with no `query` field renders as a section header.

See [Configuration → profiles.pkl](../../configuration/profiles/) for the full reference.

## Folder queries

Folders are saved searches. The query language is the same as the `durian search` syntax:

| Query | Matches |
|---|---|
| `tag:inbox` | Anything tagged inbox |
| `tag:inbox AND NOT tag:sent` | Inbox excluding self-sent |
| `group:vip AND tag:unread` | Unread mail from VIP contacts |
| `path:work` | Mail in the `work` account (this is account scope, not an IMAP folder path) |
| `date:6m..` | Last 6 months |

## Standard folders

The `P.standardFolders` listing in `Profiles.pkl` provides the obvious starter set: Inbox, Sent, Drafts, Archive, Spam, Trash. Use it as-is or splice individual entries (`for (f in P.systemFolders) { f }`).

## Smart views

Because folders are queries, you can create views that don't map to IMAP folders at all:

```pkl
new { name = "Unread VIPs"; icon = "star"; query = "group:vip AND tag:unread" }
new { name = "Has invoice"; icon = "doc"; query = "subject:invoice has:attachment:pdf" }
new { name = "This week"; icon = "calendar"; query = "date:1w.." }
```

Groups (`group:vip`) are defined in [groups.pkl](../../configuration/groups/).

## Keyboard navigation

| Key | Action |
|---|---|
| `J` / `K` | Next / previous folder |
| `gi` / `gs` / `gd` / `ga` | Go to inbox / sent / drafts / archive |
| `g1` … `g9` | Jump to the Nth folder in the sidebar |
| `gf` | Open folder picker |
| `Cmd+1` … `Cmd+9` | Switch profile |
