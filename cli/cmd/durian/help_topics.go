package main

import "github.com/spf13/cobra"

var queryHelpTopic = &cobra.Command{
	Use:   "query",
	Short: "Query syntax and supported selectors",
	Long: `Durian queries select sets of local mail threads.

Combine expressions with AND, OR, NOT, and parentheses. Supported selectors:
  from:, to:, subject:, body:, tag:, date:, before:, after:, has:, path:,
  group:, and thread:.

thread:<id> is an exact thread reference. path:<account> is the legacy account
scope selector; prefer --account on search, count, and tag. Unknown and
unimplemented selectors are errors and never broaden a query.

Examples:
  durian search 'tag:inbox AND from:alice@example.com'
  durian search 'has:attachment' --account work
  durian tag --dry-run 'thread:00000000000022ca' +todo`,
}

var identifiersHelpTopic = &cobra.Command{
	Use:   "identifiers",
	Short: "Public thread, message, event, and attachment references",
	Long: `Use the references printed by list and detail commands:

  thread:<thread-id>       mail thread
  message:<rfc-message-id> individual message (add --account if ambiguous)
  event:<ical-uid>         calendar event

Attachments are addressed by a message reference plus their integer part ID:
  durian attachment 'message:id@example.com' --account work --save 2

Full identifiers are stable script values. Event prefixes may be accepted when
unique; ambiguity is reported instead of selecting an arbitrary resource.`,
}

var accountsHelpTopic = &cobra.Command{
	Use:   "accounts",
	Short: "Account names, aliases, email addresses, and scope",
	Long: `Account input accepts the configured alias, name, or email address
case-insensitively. Output prefers the alias and also exposes the canonical
lowercase store key in JSON.

Read filters use repeatable --account/-a flags with OR semantics:
  durian search 'tag:unread' -a work -a personal

Calendar reads additionally accept the reserved local account. --from selects
a sender identity for send; it is not an account filter.`,
}

var outputHelpTopic = &cobra.Command{
	Use:   "output",
	Short: "Human, pipe, and JSON output contracts",
	Long: `Without --json, Durian prints human-readable resources to stdout and
status, progress, warnings, and errors to stderr. Redirected output has no
spinner or interactive prompt. --no-input forbids prompts and editors.

--json writes exactly one JSON value to stdout: arrays for lists, objects for
details, and effect objects for mutations. Empty lists are []. Public fields
use snake_case and exclude database and provider IDs. Status remains on stderr.
On error stdout is empty and the command exits nonzero.`,
}

var calendarTimeHelpTopic = &cobra.Command{
	Use:   "calendar-time",
	Short: "Calendar time formats and list windows",
	Long: `Calendar times accept RFC3339, '2006-01-02 15:04', a date, today, or
tomorrow. --duration accepts Go-style durations such as 30m and 1h30m.

calendar list defaults to the next seven days. Use one of --today, --week, or
--month, or use --from/--to for an explicit window. Presets cannot be combined
with an explicit window.`,
}

func init() {
	rootCmd.AddCommand(queryHelpTopic, identifiersHelpTopic, accountsHelpTopic, outputHelpTopic, calendarTimeHelpTopic)
}
