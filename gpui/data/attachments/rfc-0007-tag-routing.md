# RFC 0007 – Tag-based routing for mailcli

## Summary
No folders. Every message gets tags from rules; the inbox is `tag:inbox -tag:done`.
The CLI and the app share one rule engine, evaluated server-side.

## Rules as code
Rules live in `rules/*.toml` in the repo and are reviewed like code.

```toml
[[rule]]
name  = "alerts"
match = "from:(sentry.io OR deploy.habric.com)"
tags  = ["alerts", "infra"]
```

## Dry runs
`mailcli rules test --since 7d` shows which tags would change before a rule ships.
`mailcli rules diff` prints the tag delta per thread.

## Open questions
1. Rate limit for rule evaluation per account (proposal: 50 rules).
2. Do we keep a separate "archive", or is archive just `-inbox`?
