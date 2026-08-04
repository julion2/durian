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
	Use:   "list <account>",
	Short: "List upcoming calendar events (from the local vdir)",
	Long: `List events from the locally synced calendars of a Microsoft account.

Reads only the local vdir (run 'durian calendar sync' first to populate it), so
it works offline. Recurring events are expanded into their occurrences within
the selected window (the next 7 days by default).`,
	Example: `  durian calendar list work
  durian calendar list work --today
  durian calendar list work --from 2026-08-01 --to 2026-08-31
  durian calendar list work --calendar "Team" --json`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeAccounts,
	RunE:              runCalendarList,
}

var calendarSearchCmd = &cobra.Command{
	Use:   "search <account> <query>",
	Short: "Search calendar events by text (from the local vdir)",
	Long: `Search all locally synced events of a Microsoft account for a text query,
matching the subject, location, description and attendee addresses
(case-insensitive). Reads only the local vdir; works offline.`,
	Example: `  durian calendar search work standup
  durian calendar search work "design review" --calendar "Team"`,
	Args:              cobra.MinimumNArgs(2),
	ValidArgsFunction: completeAccounts,
	RunE:              runCalendarSearch,
}

var calendarShowCmd = &cobra.Command{
	Use:   "show <account> <event>",
	Short: "Show one calendar event in detail (from the local vdir)",
	Long: `Show the full detail of a single event: time, location, organizer,
attendees with their RSVP status, description, online-meeting link and
recurrence. The event is matched by iCalUID (exact or prefix) or by a unique
substring of its subject.`,
	Example: `  durian calendar show work standup
  durian calendar show work 1A2B3C --calendar "Team"`,
	Args:              cobra.MinimumNArgs(2),
	ValidArgsFunction: completeAccounts,
	RunE:              runCalendarShow,
}

var (
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
	accountDir = filepath.Join(config.CalendarBaseDir(cfg, ""), account.CalendarDir())
	return accountDir, account.GetAuthEmail(), account, nil
}

// occurrence pairs an expanded event with its calendar for sorting/printing.
type occurrence struct {
	cal   calendar.LocalCalendar
	event calendar.Event
}

func runCalendarList(cmd *cobra.Command, args []string) error {
	accountDir, owner, account, err := resolveVdirAccount(args[0])
	if err != nil {
		return err
	}

	from, to, err := listWindow(time.Now())
	if err != nil {
		return err
	}

	calendars, err := calendar.ReadCalendars(accountDir, owner)
	if err != nil {
		return fmt.Errorf("failed to read local calendars: %w", err)
	}

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
			out = append(out, calendar.ToCalendarEvent(o.cal.Name, o.event, false))
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if len(occs) == 0 {
		if len(calendars) == 0 {
			fmt.Printf("No local calendars for %s yet. Run 'durian calendar sync %s' first.\n",
				account.GetAliasOrName(), account.GetAliasOrName())
		} else {
			fmt.Println("No events in the selected window.")
		}
		return nil
	}

	fmt.Fprintf(os.Stderr, "%s — %s to %s (%d event(s))\n",
		styHeader("Calendar "+account.GetAliasOrName()),
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
			calSwatch(o.cal.HexColor, o.cal.Name) + eventMarkers(o.event),
		})
	}
	printColumns(os.Stdout, rows)
	return nil
}

// listWindow computes [from, to) from the list flags, relative to now, reusing
// the shared calendar.CalendarWindow (the --today/--month flags only pick
// the default span).
func listWindow(now time.Time) (from, to time.Time, err error) {
	span := 7 * 24 * time.Hour
	switch {
	case calListToday:
		span = 24 * time.Hour
	case calListMonth:
		span = 30 * 24 * time.Hour
	}
	return calendar.CalendarWindow(calListFrom, calListTo, span, now)
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
	accountDir, owner, _, err := resolveVdirAccount(args[0])
	if err != nil {
		return err
	}
	query := strings.ToLower(strings.Join(args[1:], " "))

	calendars, err := calendar.ReadCalendars(accountDir, owner)
	if err != nil {
		return fmt.Errorf("failed to read local calendars: %w", err)
	}

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
			out = append(out, calendar.ToCalendarEvent(o.cal.Name, o.event, false))
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
	rows := [][]string{{styHeader("DATE"), styHeader("SUBJECT"), styHeader("CALENDAR")}}
	for _, o := range matches {
		rows = append(rows, []string{
			o.event.Start.Format("2006-01-02 15:04"),
			styAccent(truncate(orDash(o.event.Subject), 50)),
			calSwatch(o.cal.HexColor, o.cal.Name),
		})
	}
	printColumns(os.Stdout, rows)
	return nil
}

func runCalendarShow(cmd *cobra.Command, args []string) error {
	accountDir, owner, _, err := resolveVdirAccount(args[0])
	if err != nil {
		return err
	}
	ref := strings.Join(args[1:], " ")

	_, e, calName, err := calendar.ResolveLocalEvent(accountDir, owner, ref, calShowCalendar)
	if err != nil {
		return err
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(calendar.ToCalendarEvent(calName, e, true))
	}

	printEventDetail(e, calName)
	return nil
}

// printEventDetail renders one event as a labeled block (see show.go's style).
func printEventDetail(e calendar.Event, calName string) {
	fmt.Println(styAccent(orDash(e.Subject)))
	fmt.Println(strings.Repeat("─", 50))
	field := func(label, value string) {
		if value != "" {
			fmt.Printf("%s %s\n", styDim(fmt.Sprintf("%-11s", label+":")), value)
		}
	}

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
			fmt.Printf("  %-12s %s\n", styDim(partStatLabel(a.Response)), attendeeLabel(a))
		}
	}
	if e.Description != "" {
		fmt.Printf("\n%s\n%s\n", styDim("Description:"), strings.TrimSpace(e.Description))
	}
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
