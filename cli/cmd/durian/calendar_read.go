package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/spf13/cobra"
)

// Local-first calendar read commands (list / search / show). All three read
// only the on-disk vdir (populated by `durian calendar sync`/`export`) — no
// Graph call, no token. Recurring masters are expanded into concrete
// occurrences via calendar.ExpandOccurrences.

var calendarListCmd = &cobra.Command{
	Use:   "list [account...]",
	Short: "List upcoming calendar events (from the local vdir)",
	Long: `List events from the locally synced calendars.

With no account given it covers EVERY configured account plus the local-only
calendars; name accounts (as arguments or with --account) to narrow it down.

Reads only the local vdir (run 'durian calendar sync' first to populate it), so
it works offline. Recurring events are expanded into their occurrences within
the selected window (the next 7 days by default).`,
	Example: `  durian calendar list
  durian calendar list work
  durian calendar list --account work --account local --today
  durian calendar list --from 2026-08-01 --to 2026-08-31
  durian calendar list --calendar "Team" --json`,
	Args:              cobra.ArbitraryArgs,
	ValidArgsFunction: completeCalendarAccounts,
	RunE:              runCalendarList,
}

var calendarSearchCmd = &cobra.Command{
	Use:   "search <query>",
	Short: "Search calendar events by text (from the local vdir)",
	Long: `Search the locally synced events for a text query, matching the subject,
location, description and attendee addresses (case-insensitive).

Covers EVERY configured account plus the local-only calendars unless --account
narrows it. Reads only the local vdir; works offline.`,
	Example: `  durian calendar search standup
  durian calendar search "design review" --account work --calendar "Team"`,
	Args: cobra.MinimumNArgs(1),
	// No ValidArgsFunction: the positional arg is the free-text <query>, not an
	// account. Account completion stays on the --account flag (completeAccounts).
	RunE: runCalendarSearch,
}

var calendarShowCmd = &cobra.Command{
	Use:   "show <event>",
	Short: "Show one calendar event in detail (from the local vdir)",
	Long: `Show the full detail of a single event: time, location, organizer,
attendees with their RSVP status, description, online-meeting link and
recurrence. The event is matched by iCalUID (exact or prefix) or by a unique
substring of its subject, across every configured account plus the local-only
calendars unless --account narrows it.`,
	Example: `  durian calendar show standup
  durian calendar show 1A2B3C --account work --calendar "Team"`,
	Args: cobra.ExactArgs(1),
	RunE: runCalendarShow,
}

var (
	// calAccounts narrows the read commands to specific accounts. Empty means
	// every configured account plus the local-only calendars, which is what
	// someone asking "what is on my calendar" almost always means.
	calAccounts []string

	calListCalendar string
	calListToday    bool
	calListWeek     bool
	calListMonth    bool
	calListFrom     string
	calListTo       string

	calSearchCalendar string
	calShowCalendar   string
)

func init() {
	calendarCmd.AddCommand(calendarListCmd, calendarSearchCmd, calendarShowCmd)

	calendarListCmd.Flags().StringVar(&calListCalendar, "calendar", "", "Only this calendar (by display name)")
	calendarListCmd.Flags().BoolVar(&calListToday, "today", false, "Only today")
	calendarListCmd.Flags().BoolVar(&calListWeek, "week", false, "The next 7 days (default)")
	calendarListCmd.Flags().BoolVar(&calListMonth, "month", false, "The next 30 days")
	calendarListCmd.Flags().StringVar(&calListFrom, "from", "", "Window start (date or date-time; default today)")
	calendarListCmd.Flags().StringVar(&calListTo, "to", "", "Window end (date or date-time)")

	calendarSearchCmd.Flags().StringVar(&calSearchCalendar, "calendar", "", "Only this calendar (by display name)")
	for _, c := range []*cobra.Command{calendarListCmd, calendarSearchCmd, calendarShowCmd} {
		c.Flags().StringArrayVar(&calAccounts, "account", nil,
			"Only this account (repeatable; default: every account plus the local calendars)")
		_ = c.RegisterFlagCompletionFunc("account", completeCalendarAccounts)
	}
	calendarShowCmd.Flags().StringVar(&calShowCalendar, "calendar", "", "Only this calendar (by display name)")
}

// resolveVdirAccount resolves the account and its local vdir directory. It does
// not touch the network; a missing vdir is fine (the readers handle it).
func resolveVdirAccount(identifier string) (accountDir, owner string, account *config.AccountConfig, err error) {
	cfg := GetConfig()
	if cfg == nil {
		return "", "", nil, fmt.Errorf("no configuration loaded")
	}
	account, err = cfg.GetAccountByIdentifier(identifier)
	if err != nil {
		return "", "", nil, fmt.Errorf("account not found: %s\nAvailable accounts: %s", identifier, cfg.ListAccountIdentifiers())
	}
	if !account.CalendarEnabled() {
		return "", "", nil, fmt.Errorf("calendar is disabled for account %s", account.GetAliasOrName())
	}
	accountDir = filepath.Join(config.CalendarBaseDir(cfg, ""), account.CalendarDir())
	return accountDir, account.Email, account, nil
}

// resolveVdirCollections resolves one identifier to the calendar collections it
// names.
//
// The reserved identifier "local" selects the calendars configured under
// calendar.local_calendars, which belong to no account. They get no owner:
// nothing in them is ever sent anywhere, so there is no RSVP of the account
// holder to recognize.
func resolveVdirCollections(identifier string) ([]calendar.Collection, error) {
	cfg := GetConfig()
	if cfg == nil {
		return nil, fmt.Errorf("no configuration loaded")
	}
	if strings.EqualFold(identifier, config.LocalCalendarAccount) {
		return localCalendarCollections(cfg), nil
	}

	accountDir, owner, account, err := resolveVdirAccount(identifier)
	if err != nil {
		return nil, err
	}
	return calendar.CollectionsUnderFor(accountDir, account.GetAliasOrName(), owner)
}

// localCalendarCollections projects the configured local calendars onto the
// neutral collection type.
func localCalendarCollections(cfg *config.Config) []calendar.Collection {
	local := cfg.LocalCalendars()
	cols := make([]calendar.Collection, 0, len(local))
	for _, lc := range local {
		cols = append(cols, calendar.Collection{
			Dir:      lc.Path,
			Name:     lc.Name,
			HexColor: lc.Color,
			ReadOnly: lc.ReadOnly,
			Account:  config.LocalCalendarAccount,
		})
	}
	return cols
}

// calendarTargets resolves the accounts a read command should cover: the ones
// named as arguments or via --account, or — when neither is given — every
// calendar-enabled account plus the local calendars.
//
// Reading everything by default is the point: someone asking what is on their
// calendar means all of it, and the previous mandatory account turned the most
// common question into the most typing. Narrowing stays available, it is just
// no longer the only option.
func calendarTargets(named []string) (cols []calendar.Collection, accounts []string, err error) {
	cfg := GetConfig()
	if cfg == nil {
		return nil, nil, fmt.Errorf("no configuration loaded")
	}

	wanted := append(append([]string{}, named...), calAccounts...)
	if len(wanted) == 0 {
		for i := range cfg.Accounts {
			if cfg.Accounts[i].CalendarEnabled() {
				wanted = append(wanted, cfg.Accounts[i].GetAliasOrName())
			}
		}
		// Only when some are configured — otherwise "local" would be reported
		// as an account that yielded nothing.
		if len(cfg.LocalCalendars()) > 0 {
			wanted = append(wanted, config.LocalCalendarAccount)
		}
		if len(wanted) == 0 {
			return nil, nil, fmt.Errorf("no accounts and no local calendars configured")
		}
	}

	seen := make(map[string]bool, len(wanted))
	for _, id := range wanted {
		if seen[strings.ToLower(id)] {
			continue
		}
		seen[strings.ToLower(id)] = true
		got, err := resolveVdirCollections(id)
		if err != nil {
			return nil, nil, err
		}
		cols = append(cols, got...)
		accounts = append(accounts, id)
	}
	return cols, accounts, nil
}

// contributingAccounts lists the accounts that actually produced a calendar,
// in first-seen order.
//
// This is deliberately derived from the RESULT, not from what was asked for:
// with no --account the request covers every configured account, but most of
// them have no calendar vdir at all. Saying "12 accounts" when two of them have
// calendars describes the query rather than the answer — and it would prefix
// every row with an account name that distinguishes nothing.
func contributingAccounts(calendars []calendar.LocalCalendar) []string {
	seen := make(map[string]bool, len(calendars))
	var out []string
	for _, cal := range calendars {
		if cal.Account == "" || seen[cal.Account] {
			continue
		}
		seen[cal.Account] = true
		out = append(out, cal.Account)
	}
	return out
}

// targetLabel renders the contributing account set for a header line.
func targetLabel(accounts []string) string {
	if len(accounts) == 1 {
		return accounts[0]
	}
	return fmt.Sprintf("%d accounts", len(accounts))
}

// warnMisconfiguredCollections prints the vdir-base diagnosis for any
// configured calendar that turned up empty because its path points one level
// too high. Printed to stderr so it never pollutes --json output.
func warnMisconfiguredCollections(cols []calendar.Collection, calendars []calendar.LocalCalendar) {
	for _, bad := range calendar.InspectCollections(cols, calendars) {
		fmt.Fprintf(os.Stderr, "%s %s\n", styWarn("warning:"), bad.Hint())
	}
}

// calendarLabel renders the calendar cell of a row. Account is a separate
// column so the calendar name remains stable in single- and multi-account output.
func calendarLabel(cal calendar.LocalCalendar) string {
	return calSwatch(cal.HexColor, humanText(cal.Name, false))
}

// occurrence pairs an expanded event with its calendar for sorting/printing.
type occurrence struct {
	cal   calendar.LocalCalendar
	event calendar.Event
}

func runCalendarList(cmd *cobra.Command, args []string) error {
	if err := validateCalendarListWindowFlags(); err != nil {
		return err
	}
	cols, _, err := calendarTargets(args)
	if err != nil {
		return err
	}

	from, to, err := listWindow(time.Now())
	if err != nil {
		return err
	}

	calendars, err := calendar.ReadCollections(cols)
	if err != nil {
		return fmt.Errorf("failed to read local calendars: %w", err)
	}
	warnMisconfiguredCollections(cols, calendars)
	accounts := contributingAccounts(calendars)
	label := targetLabel(accounts)

	var occs []occurrence
	for _, cal := range calendars {
		if calListCalendar != "" && !strings.EqualFold(cal.Name, calListCalendar) {
			continue
		}
		for _, e := range cal.Events {
			for _, occ := range calendar.ExpandOccurrences(e, from, to) {
				occs = append(occs, occurrence{cal: cal, event: occ})
			}
		}
	}
	sort.Slice(occs, func(i, j int) bool { return occs[i].event.Start.Before(occs[j].event.Start) })

	if jsonOutput {
		out := make([]calendar.CalendarEvent, 0, len(occs))
		for _, o := range occs {
			dto := calendar.ToCalendarEvent(o.cal.Name, o.event, false)
			dto.Account = o.cal.Account
			out = append(out, dto)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if len(occs) == 0 {
		if len(calendars) == 0 {
			fmt.Printf("No local calendars for %s yet. Run 'durian calendar sync <account>' first.\n", label) // encgrep:allow account label (config name/alias), a user-facing CLI message — not an encrypted column
		} else {
			fmt.Println("No events in the selected window.")
		}
		return nil
	}

	fmt.Fprintf(os.Stderr, "%s — %s to %s (%d event(s))\n",
		styHeader("Calendar "+label),
		from.Format("2006-01-02"), to.Format("2006-01-02"), len(occs))

	// Colored cells must not go through text/tabwriter (it counts the ANSI
	// escape bytes as width); printColumns pads on the visible width instead.
	var rows [][]string
	var lastDay string
	for _, o := range occs {
		day := o.event.Start.Format("Mon 2006-01-02")
		if day != lastDay {
			if lastDay != "" {
				rows = append(rows, []string{""})
			}
			rows = append(rows, []string{styHeader(day)})
			lastDay = day
		}
		rows = append(rows, []string{
			"  " + eventTimeCol(o.event),
			styAccent(truncate(orDash(o.event.Subject), 50)),
			styDim(truncate(o.event.Location, 24)),
			calendarLabel(o.cal) + eventMarkers(o.event),
			styDim(o.cal.Account),
			styDim(eventRefCell(o.event.ICalUID)),
		})
	}
	printColumns(os.Stdout, rows)
	return nil
}

// eventRefCell renders the "event:<uid>" reference column.
//
// Shared by the list and search views so the escaping cannot be present in one
// and missing in the other, and so it is reachable from a test — both callers
// write straight to stdout through printColumns. A UID comes from whoever
// produced the invitation, so it is remote text like any other cell.
func eventRefCell(uid string) string {
	return "event:" + humanText(uid, false)
}

// listWindow computes [from, to) from the list flags, relative to now, reusing
// the shared calendar.CalendarWindow (the --today/--month flags only pick
// the default span).
func listWindow(now time.Time) (from, to time.Time, err error) {
	span := 7 * 24 * time.Hour
	switch {
	case calListToday:
		span = 24 * time.Hour
	case calListWeek:
		span = 7 * 24 * time.Hour
	case calListMonth:
		span = 30 * 24 * time.Hour
	}
	return calendar.CalendarWindow(calListFrom, calListTo, span, now)
}

func validateCalendarListWindowFlags() error {
	presets := 0
	for _, set := range []bool{calListToday, calListWeek, calListMonth} {
		if set {
			presets++
		}
	}
	if presets > 1 {
		return fmt.Errorf("use only one of --today, --week, or --month")
	}
	if presets > 0 && (calListFrom != "" || calListTo != "") {
		return fmt.Errorf("date presets cannot be combined with --from or --to")
	}
	return nil
}

// eventTimeCol renders the time column: "all-day" or "HH:MM".
func eventTimeCol(e calendar.Event) string {
	if e.AllDay {
		return styDim("all-day")
	}
	return e.Start.Format("15:04")
}

// eventMarkers appends dim markers for online meetings and the owner's RSVP.
func eventMarkers(e calendar.Event) string {
	var m []string
	if e.IsOnlineMeeting || e.OnlineMeetingURL != "" {
		m = append(m, "online")
	}
	switch e.OwnerResponse {
	case calendar.OwnerRespAccepted:
		m = append(m, "accepted")
	case calendar.OwnerRespDeclined:
		m = append(m, "declined")
	case calendar.OwnerRespTentative:
		m = append(m, "tentative")
	}
	if len(m) == 0 {
		return ""
	}
	return " " + styDim("["+strings.Join(m, " ")+"]")
}

func orDash(s string) string {
	if s == "" {
		return "(no subject)"
	}
	return s
}

func runCalendarSearch(cmd *cobra.Command, args []string) error {
	cols, _, err := calendarTargets(nil)
	if err != nil {
		return err
	}
	query := strings.ToLower(strings.Join(args, " "))

	calendars, err := calendar.ReadCollections(cols)
	if err != nil {
		return fmt.Errorf("failed to read local calendars: %w", err)
	}
	warnMisconfiguredCollections(cols, calendars)
	var matches []occurrence
	for _, cal := range calendars {
		if calSearchCalendar != "" && !strings.EqualFold(cal.Name, calSearchCalendar) {
			continue
		}
		for _, e := range cal.Events {
			if calendar.EventMatchesQuery(e, query) {
				matches = append(matches, occurrence{cal: cal, event: e})
			}
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].event.Start.Before(matches[j].event.Start) })

	if jsonOutput {
		out := make([]calendar.CalendarEvent, 0, len(matches))
		for _, o := range matches {
			dto := calendar.ToCalendarEvent(o.cal.Name, o.event, false)
			dto.Account = o.cal.Account
			out = append(out, dto)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if len(matches) == 0 {
		fmt.Println("No matching events.")
		return nil
	}

	// Header and cells are styled per cell (not per line), so printColumns can
	// align them on their visible widths.
	rows := [][]string{{styHeader("EVENT"), styHeader("ACCOUNT"), styHeader("DATE"), styHeader("SUBJECT"), styHeader("CALENDAR")}}
	for _, o := range matches {
		rows = append(rows, []string{
			eventRefCell(o.event.ICalUID),
			o.cal.Account,
			o.event.Start.Format("2006-01-02 15:04"),
			styAccent(truncate(orDash(o.event.Subject), 50)),
			calendarLabel(o.cal),
		})
	}
	printColumns(os.Stdout, rows)
	return nil
}

func runCalendarShow(cmd *cobra.Command, args []string) error {
	cols, _, err := calendarTargets(nil)
	if err != nil {
		return err
	}
	ref := normalizeEventReference(strings.Join(args, " "))

	path, e, calName, err := calendar.ResolveEventIn(cols, ref, calShowCalendar)
	if err != nil {
		// A miss may simply mean a configured calendar points one level too
		// high and contributed nothing to search through.
		if calendars, readErr := calendar.ReadCollections(cols); readErr == nil {
			warnMisconfiguredCollections(cols, calendars)
		}
		return err
	}

	account := calendar.CollectionAccountForPath(cols, path)
	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		dto := calendar.ToCalendarEvent(calName, e, true)
		dto.Account = account
		return enc.Encode(dto)
	}

	printEventDetail(e, calName, account)
	return nil
}

// printEventDetail renders one event as a labeled block (see show.go's style).
func printEventDetail(e calendar.Event, calName, account string) {
	fmt.Println(styAccent(humanText(orDash(e.Subject), false)))
	fmt.Println(strings.Repeat("─", 50))
	field := func(label, value string) {
		if value != "" {
			fmt.Printf("%s %s\n", styDim(fmt.Sprintf("%-11s", label+":")), humanText(value, false))
		}
	}

	field("Event", "event:"+e.ICalUID)
	field("Account", account)
	field("Calendar", calName)
	field("When", eventWhen(e))
	field("Location", e.Location)
	if e.Organizer != nil {
		field("Organizer", personLabel(*e.Organizer))
	}
	if e.OwnerResponse != "" && e.OwnerResponse != calendar.OwnerRespNone {
		field("My status", string(e.OwnerResponse))
	}
	if e.OnlineMeetingURL != "" {
		field("Online", e.OnlineMeetingURL)
	}
	if rec := recurrenceSummary(e); rec != "" {
		field("Repeats", rec)
	}

	if len(e.Attendees) > 0 {
		fmt.Printf("\n%s\n", styDim(fmt.Sprintf("Attendees (%d):", len(e.Attendees))))
		for _, a := range e.Attendees {
			fmt.Printf("  %-12s %s\n", styDim(partStatLabel(a.Response)), humanText(attendeeLabel(a), false))
		}
	}
	if e.Description != "" {
		fmt.Printf("\n%s\n%s\n", styDim("Description:"), humanText(strings.TrimSpace(e.Description), true))
	}
}

func normalizeEventReference(ref string) string {
	ref = strings.TrimSpace(ref)
	if len(ref) >= len("event:") && strings.EqualFold(ref[:len("event:")], "event:") {
		return strings.TrimSpace(ref[len("event:"):])
	}
	return ref
}

// eventWhen renders the time span of an event for the detail view.
func eventWhen(e calendar.Event) string {
	if e.AllDay {
		days := e.End.Sub(e.Start).Hours() / 24
		if days <= 1 {
			return e.Start.Format("Mon 2006-01-02") + styDim(" (all-day)")
		}
		return fmt.Sprintf("%s – %s%s", e.Start.Format("2006-01-02"),
			e.End.AddDate(0, 0, -1).Format("2006-01-02"), styDim(" (all-day)"))
	}
	if e.Start.Format("2006-01-02") == e.End.Format("2006-01-02") {
		return fmt.Sprintf("%s %s–%s %s", e.Start.Format("Mon 2006-01-02"),
			e.Start.Format("15:04"), e.End.Format("15:04"), styDim("UTC"))
	}
	return fmt.Sprintf("%s – %s %s", e.Start.Format("Mon 2006-01-02 15:04"),
		e.End.Format("Mon 2006-01-02 15:04"), styDim("UTC"))
}

// personLabel renders a Person as "Name <email>" (or bare email).
func personLabel(p calendar.Person) string {
	if p.Name != "" && !strings.EqualFold(p.Name, p.Email) {
		return fmt.Sprintf("%s <%s>", p.Name, p.Email)
	}
	return p.Email
}

// attendeeLabel renders an attendee as personLabel plus a dim role suffix for
// optional/resource attendees.
func attendeeLabel(a calendar.Attendee) string {
	label := personLabel(calendar.Person{Name: a.Name, Email: a.Email})
	switch a.Type {
	case "optional":
		return label + styDim(" (optional)")
	case "resource":
		return label + styDim(" (resource)")
	}
	return label
}

// partStatLabel maps a Graph attendee response to a short human label.
func partStatLabel(response string) string {
	switch response {
	case "accepted", "organizer":
		return "accepted"
	case "declined":
		return "declined"
	case "tentativelyAccepted":
		return "tentative"
	default:
		return "no reply"
	}
}

// recurrenceSummary renders a one-line human description of a series, or "" for
// a non-recurring event.
func recurrenceSummary(e calendar.Event) string {
	if e.Recurrence == nil {
		return ""
	}
	p := e.Recurrence.Pattern
	every := "every "
	if p.Interval > 1 {
		every = fmt.Sprintf("every %d ", p.Interval)
	}
	var base string
	switch p.Type {
	case "daily":
		base = every + plural(p.Interval, "day", "days")
	case "weekly":
		base = every + plural(p.Interval, "week", "weeks")
		if len(p.DaysOfWeek) > 0 {
			base += " on " + strings.Join(p.DaysOfWeek, ", ")
		}
	case "absoluteMonthly", "relativeMonthly":
		base = every + plural(p.Interval, "month", "months")
	case "absoluteYearly", "relativeYearly":
		base = every + plural(p.Interval, "year", "years")
	default:
		base = "recurring"
	}
	switch e.Recurrence.Range.Type {
	case "endDate":
		base += " until " + e.Recurrence.Range.EndDate
	case "numbered":
		base += fmt.Sprintf(" for %d occurrences", e.Recurrence.Range.NumberOfOccurrences)
	}
	return base
}

// plural picks the singular form when n <= 1.
func plural(n int, one, many string) string {
	if n <= 1 {
		return one
	}
	return many
}
