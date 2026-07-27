package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/julion2/durian/cli/internal/graphcalendar"
	"github.com/spf13/cobra"
)

// Local-first calendar write commands (new / rsvp / delete). None of them talk
// to Graph: they create, edit or remove a local .ics file in the vdir. The
// change reaches Outlook only on the next `durian calendar sync`, which shows
// its notification preview and confirmation gate first — so no write command
// can ever send mail on its own.

var calendarNewCmd = &cobra.Command{
	Use:   "new <account>",
	Short: "Create a calendar event locally (pushed on next sync)",
	Long: `Create a new event as a local .ics file in one of the account's synced
calendars. Nothing is sent to Outlook now — the event is uploaded on the next
'durian calendar sync', which previews any invitations before sending. Add
--teams to request a Teams online meeting on creation.`,
	Example: `  durian calendar new work --calendar "Calendar" -s "Lunch" --start "2026-08-01 12:00" --duration 1h
  durian calendar new work --calendar "Calendar" -s "Review" --start "2026-08-02 09:00" --attendee a@x.com --teams`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeAccounts,
	RunE:              runCalendarNew,
}

var calendarRsvpCmd = &cobra.Command{
	Use:   "rsvp <account> <event> <accept|decline|tentative>",
	Short: "Set your RSVP on a meeting locally (sent on next sync)",
	Long: `Set your response (accept/decline/tentative) on a meeting you were invited
to. This edits your own participation status in the local .ics; the reply is
sent to the organizer on the next 'durian calendar sync'. Only works when you
are an attendee (not the organizer).`,
	Example: `  durian calendar rsvp work standup accept
  durian calendar rsvp work "team sync" decline`,
	Args:              cobra.ExactArgs(3),
	ValidArgsFunction: completeAccounts,
	RunE:              runCalendarRsvp,
}

var calendarDeleteCmd = &cobra.Command{
	Use:   "delete <account> <event>",
	Short: "Delete a calendar event locally (applied on next sync)",
	Long: `Remove an event's local .ics file. The deletion is propagated to Outlook on
the next 'durian calendar sync' — cancelling the meeting if you organize it, or
declining it if you are only an attendee (the sync previews this first).`,
	Example: `  durian calendar delete work standup
  durian calendar delete work 1A2B3C --yes`,
	Args:              cobra.ExactArgs(2),
	ValidArgsFunction: completeAccounts,
	RunE:              runCalendarDelete,
}

var (
	calNewCalendar    string
	calNewSubject     string
	calNewStart       string
	calNewEnd         string
	calNewDuration    string
	calNewAllDay      bool
	calNewLocation    string
	calNewDescription string
	calNewAttendees   []string
	calNewTeams       bool

	calRsvpCalendar   string
	calDeleteCalendar string
	calDeleteYes      bool
)

func init() {
	calendarCmd.AddCommand(calendarNewCmd, calendarRsvpCmd, calendarDeleteCmd)

	calendarNewCmd.Flags().StringVar(&calNewCalendar, "calendar", "", "Target calendar (by display name; required)")
	calendarNewCmd.Flags().StringVarP(&calNewSubject, "subject", "s", "", "Event subject (required)")
	calendarNewCmd.Flags().StringVar(&calNewStart, "start", "", "Start (RFC3339, \"2006-01-02 15:04\", \"2006-01-02\", today, tomorrow; required)")
	calendarNewCmd.Flags().StringVar(&calNewEnd, "end", "", "End (same formats as --start)")
	calendarNewCmd.Flags().StringVar(&calNewDuration, "duration", "", "Duration instead of --end (e.g. 30m, 1h30m)")
	calendarNewCmd.Flags().BoolVar(&calNewAllDay, "all-day", false, "All-day event")
	calendarNewCmd.Flags().StringVar(&calNewLocation, "location", "", "Location")
	calendarNewCmd.Flags().StringVar(&calNewDescription, "description", "", "Description / body")
	calendarNewCmd.Flags().StringArrayVar(&calNewAttendees, "attendee", nil, "Attendee email (repeatable)")
	calendarNewCmd.Flags().BoolVar(&calNewTeams, "teams", false, "Create as a Teams online meeting")

	calendarRsvpCmd.Flags().StringVar(&calRsvpCalendar, "calendar", "", "Only this calendar (by display name)")
	calendarDeleteCmd.Flags().StringVar(&calDeleteCalendar, "calendar", "", "Only this calendar (by display name)")
	calendarDeleteCmd.Flags().BoolVar(&calDeleteYes, "yes", false, "Delete without confirmation")
}

func runCalendarNew(cmd *cobra.Command, args []string) error {
	accountDir, _, account, err := resolveVdirAccount(args[0])
	if err != nil {
		return err
	}
	if calNewCalendar == "" || calNewSubject == "" || calNewStart == "" {
		return fmt.Errorf("--calendar, --subject and --start are required")
	}
	if calNewEnd != "" && calNewDuration != "" {
		return fmt.Errorf("use either --end or --duration, not both")
	}

	now := time.Now()
	start, startAllDay, err := graphcalendar.ParseWhen(calNewStart, now)
	if err != nil {
		return err
	}
	allDay := calNewAllDay || startAllDay

	end, err := eventEnd(start, allDay, now)
	if err != nil {
		return err
	}
	if !end.After(start) {
		return fmt.Errorf("end %s is not after start %s", end.Format(time.RFC3339), start.Format(time.RFC3339))
	}

	attendees := make([]graphcalendar.Attendee, 0, len(calNewAttendees))
	for _, email := range calNewAttendees {
		email = strings.TrimSpace(email)
		if email == "" {
			continue
		}
		if !strings.Contains(email, "@") {
			return fmt.Errorf("invalid --attendee %q: not an email address", email)
		}
		attendees = append(attendees, graphcalendar.Attendee{Email: email, Type: "required"})
	}

	event := graphcalendar.Event{
		ICalUID:              uuid.NewString(),
		Subject:              calNewSubject,
		Location:             calNewLocation,
		Description:          calNewDescription,
		Start:                start,
		End:                  end,
		AllDay:               allDay,
		Attendees:            attendees,
		RequestOnlineMeeting: calNewTeams,
	}

	path, err := graphcalendar.WriteLocalEvent(accountDir, calNewCalendar, event)
	if err != nil {
		return err
	}

	fmt.Println(okLine("Created %q in %q", calNewSubject, calNewCalendar))
	fmt.Fprintf(os.Stderr, "  %s\n", styDim(path))
	printSyncReminder(account.GetAliasOrName(), len(attendees) > 0 || calNewTeams)
	return nil
}

// eventEnd computes the end time from --end/--duration, or a default (1h for a
// timed event, +1 day for an all-day one).
func eventEnd(start time.Time, allDay bool, now time.Time) (time.Time, error) {
	var end time.Time
	switch {
	case calNewEnd != "":
		e, _, err := graphcalendar.ParseWhen(calNewEnd, now)
		if err != nil {
			return time.Time{}, err
		}
		end = e
	case calNewDuration != "":
		d, err := time.ParseDuration(calNewDuration)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid --duration %q: %w", calNewDuration, err)
		}
		end = start.Add(d)
	case allDay:
		end = start.AddDate(0, 0, 1)
	default:
		end = start.Add(time.Hour)
	}
	// All-day events span whole days; Graph rejects an all-day event shorter
	// than 24h, so snap a shorter span (e.g. --start today --duration 1h) up.
	if allDay && end.Sub(start) < 24*time.Hour {
		end = start.AddDate(0, 0, 1)
	}
	return end, nil
}

func runCalendarRsvp(cmd *cobra.Command, args []string) error {
	accountDir, owner, account, err := resolveVdirAccount(args[0])
	if err != nil {
		return err
	}
	ref := args[1]
	response, err := parseRsvpVerb(args[2])
	if err != nil {
		return err
	}

	path, event, calName, err := graphcalendar.ResolveLocalEvent(accountDir, owner, ref, calRsvpCalendar)
	if err != nil {
		return err
	}

	if isOwnerOrganizer(event, owner) {
		return fmt.Errorf("cannot RSVP to %q — you are the organizer", orDash(event.Subject))
	}
	if !setOwnerResponse(&event, owner, response) {
		return fmt.Errorf("you (%s) are not an attendee of %q, cannot RSVP", owner, orDash(event.Subject))
	}

	data, err := graphcalendar.EventToICal(event)
	if err != nil {
		return fmt.Errorf("failed to serialize event: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("failed to write %s: %w", path, err)
	}

	fmt.Println(okLine("RSVP %q set to %q in %q", orDash(event.Subject), rsvpVerbLabel(response), calName))
	printSyncReminder(account.GetAliasOrName(), true)
	return nil
}

// parseRsvpVerb maps the CLI verb to the canonical owner response.
func parseRsvpVerb(verb string) (graphcalendar.OwnerResp, error) {
	switch strings.ToLower(verb) {
	case "accept":
		return graphcalendar.OwnerRespAccepted, nil
	case "decline":
		return graphcalendar.OwnerRespDeclined, nil
	case "tentative":
		return graphcalendar.OwnerRespTentative, nil
	default:
		return "", fmt.Errorf("unknown RSVP %q, use accept, decline or tentative", verb)
	}
}

func rsvpVerbLabel(r graphcalendar.OwnerResp) string {
	switch r {
	case graphcalendar.OwnerRespAccepted:
		return "accepted"
	case graphcalendar.OwnerRespDeclined:
		return "declined"
	default:
		return "tentative"
	}
}

// isOwnerOrganizer reports whether the account owner organizes this event.
func isOwnerOrganizer(e graphcalendar.Event, owner string) bool {
	if e.OwnerResponse == graphcalendar.OwnerRespOrganizer {
		return true
	}
	return e.Organizer != nil && owner != "" && strings.EqualFold(e.Organizer.Email, owner)
}

// setOwnerResponse updates the owner's own ATTENDEE response (the field that
// EventToICal turns into the PARTSTAT parameter the sync later reads) and the
// canonical OwnerResponse. It reports false when the owner is not among the
// attendees, so a local file cannot carry an RSVP for a meeting the owner was
// not invited to.
func setOwnerResponse(e *graphcalendar.Event, owner string, resp graphcalendar.OwnerResp) bool {
	graphResp := map[graphcalendar.OwnerResp]string{
		graphcalendar.OwnerRespAccepted:  "accepted",
		graphcalendar.OwnerRespDeclined:  "declined",
		graphcalendar.OwnerRespTentative: "tentativelyAccepted",
	}[resp]
	for i := range e.Attendees {
		if strings.EqualFold(e.Attendees[i].Email, owner) {
			e.Attendees[i].Response = graphResp
			e.OwnerResponse = resp
			return true
		}
	}
	return false
}

func runCalendarDelete(cmd *cobra.Command, args []string) error {
	accountDir, owner, account, err := resolveVdirAccount(args[0])
	if err != nil {
		return err
	}
	ref := args[1]

	path, event, calName, err := graphcalendar.ResolveLocalEvent(accountDir, owner, ref, calDeleteCalendar)
	if err != nil {
		return err
	}

	if !calDeleteYes {
		if !confirmPrompt(fmt.Sprintf("Delete %q (%s) from %q locally?",
			orDash(event.Subject), event.Start.Format("2006-01-02 15:04"), calName)) {
			fmt.Fprintln(os.Stderr, "aborted, nothing deleted")
			return nil
		}
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("failed to delete %s: %w", path, err)
	}

	fmt.Println(okLine("Deleted %q from %q locally", orDash(event.Subject), calName))
	printSyncReminder(account.GetAliasOrName(), len(event.Attendees) > 0)
	return nil
}

// confirmPrompt writes a [y/N] prompt to stderr and reads one line from stdin;
// a read error or anything but y/yes counts as "no" (same pattern as the
// calendar sync gate).
func confirmPrompt(msg string) bool {
	fmt.Fprint(os.Stderr, msg+" [y/N] ")
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

// printSyncReminder tells the user how to apply the local change, warning when
// applying it will send mail to attendees.
func printSyncReminder(account string, mayNotify bool) {
	msg := fmt.Sprintf("Run 'durian calendar sync %s' to apply.", account)
	if mayNotify {
		msg += " It will preview any attendee notifications before sending."
	}
	fmt.Fprintln(os.Stderr, styDim(msg))
}
