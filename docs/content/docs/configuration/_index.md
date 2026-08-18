---
title: Configuration
weight: 4
---

Durian is configured entirely through [Apple Pkl](https://pkl-lang.org) files in `~/.config/durian/` (or `$XDG_CONFIG_HOME/durian/`). Pkl is typed and validated — schema errors fail fast with a line number.

| File | Purpose |
|---|---|
| [`config.pkl`](config/) | Accounts (IMAP/SMTP/OAuth), settings, sync intervals, signatures |
| [`profiles.pkl`](profiles/) | Sidebar folders and account groupings |
| [`rules.pkl`](rules/) | Filter rules: tag/move/forward on incoming mail |
| [`groups.pkl`](groups/) | Contact groups (e.g. `group:vip` in queries) |
| [`keymaps.pkl`](keymaps/) | Custom keyboard bindings |

Run `durian validate` to check syntax and types of all files. Schema definitions are embedded in the binary — you don't need to maintain them.

## Pkl crash course

Pkl looks like a typed mix of Lisp, Kotlin, and JSON. The shapes you'll see in Durian:

- **Object literals**: `new { name = "Personal"; email = "you@example.com" }`
- **Listings**: `accounts { new { ... } new { ... } }` (collection of objects)
- **Map literals**: `signatures { ["default"] = "Best regards" }` (key → value)
- **Imports**: `import "modulepath:/Config.pkl" as C` — gives access to provider presets like `(C.gmail)` and `(C.microsoft365)`.
- **Amends** (`(C.gmail) { ... }`): clone an object and override fields.

If a value has a default in the schema, you can omit it. Pkl reports unknown fields and type mismatches at evaluation time.

## Where the examples live

Each Pkl file ships with a generously commented example:

- [`config-example.pkl`](https://github.com/julion2/durian/blob/main/docs/config-example.pkl)
- [`profiles-example.pkl`](https://github.com/julion2/durian/blob/main/docs/profiles-example.pkl)
- [`rules-example.pkl`](https://github.com/julion2/durian/blob/main/docs/rules-example.pkl)
- [`groups-example.pkl`](https://github.com/julion2/durian/blob/main/docs/groups-example.pkl)
- [`keymaps-example.pkl`](https://github.com/julion2/durian/blob/main/docs/keymaps-example.pkl)

`curl -o ~/.config/durian/<name>.pkl https://raw.githubusercontent.com/julion2/durian/main/docs/<name>-example.pkl` and trim what you don't need.
