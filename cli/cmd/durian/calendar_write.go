package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/julion2/durian/cli/internal/calendar"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/spf13/cobra"
)

// Local-first calendar write commands (new / modify / rsvp / delete). None of them talk
// to Graph: they create, edit or remove a local .ics file in the vdir. The
// change reaches Outlook only on the next `durian calendar sync`, which shows
// its notification preview and confirmation gate first — so no write command
// can ever send mail on its own.

var calendarNewCmd = &cobra.Command{
	Use:   "new <account>",
	Short: "Create a calendar event locally (pushed on next sync)",
	Long: `Create a new event as a local .ics file in one of the account's synced
calendars. Nothing is sent now — the event is uploaded on the next
'durian calendar sync', which previews any invitations before sending. Add
--online-meeting to request an online meeting on creation (Teams for a
Microsoft account, Google Meet for a Google account).`,
	Example: `  durian calendar new work --calendar "Calendar" -s "Lunch" --start "2026-08-01 12:00" --duration 1h
  durian calendar new work --calendar "Calendar" -s "Review" --start "2026-08-02 09:00" --attendee a@x.com --online-meeting`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeAccounts,
	RunE:              runCalendarNew,
}

var calendarModifyCmd = &cobra.Command{
	Use:     "modify <account> <event>",
	Aliases: []string{"edit"},
	Short:   "Modify a calendar event locally (pushed on next sync)",
	Long: `Patch an existing local event. Only flags you provide are changed; every
other field, including attendees and recurrence, is preserved. Nothing is sent
now — the update is uploaded on the next 'durian calendar sync', behind its
notification preview when the event has attendees.`,
	Example: `  durian calendar modify work standup --start "2026-08-25 09:00" --duration 30m
  durian calendar modify work 1A2B3C --subject "Planning" --location "Room 2"
  durian calendar modify work 1A2B3C --description ""`,
	Args:              cobra.ExactArgs(2),
	ValidArgsFunction: completeAccounts,
	RunE:              runCalendarModify,
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

	calModifyCalendar    string
	calModifySubject     string
	calModifyStart       string
	calModifyEnd         string
	calModifyDuration    string
	calModifyAllDay      bool
	calModifyLocation    string
	calModifyDescription string

	calRsvpCalendar   string
	calDeleteCalendar string
	calDeleteYes      bool
)

func init() {
	calendarCmd.AddCommand(calendarNewCmd, calendarModifyCmd, calendarRsvpCmd, calendarDeleteCmd)

	calendarNewCmd.Flags().StringVar(&calNewCalendar, "calendar", "", "Target calendar (by display name; required)")
	calendarNewCmd.Flags().StringVarP(&calNewSubject, "subject", "s", "", "Event subject (required)")
	calendarNewCmd.Flags().StringVar(&calNewStart, "start", "", "Start (RFC3339, \"2006-01-02 15:04\", \"2006-01-02\", today, tomorrow; required)")
	calendarNewCmd.Flags().StringVar(&calNewEnd, "end", "", "End (same formats as --start)")
	calendarNewCmd.Flags().StringVar(&calNewDuration, "duration", "", "Duration instead of --end (e.g. 30m, 1h30m)")
	calendarNewCmd.Flags().BoolVar(&calNewAllDay, "all-day", false, "All-day event")
	calendarNewCmd.Flags().StringVar(&calNewLocation, "location", "", "Location")
	calendarNewCmd.Flags().StringVar(&calNewDescription, "description", "", "Description / body")
	calendarNewCmd.Flags().StringArrayVar(&calNewAttendees, "attendee", nil, "Attendee email (repeatable)")
	calendarNewCmd.Flags().BoolVar(&calNewTeams, "online-meeting", false,
		"Create with an online meeting (Teams for Microsoft, Google Meet for Google)")
	// Provider-neutral rename; keep the old Microsoft-flavored flag working.
	calendarNewCmd.Flags().BoolVar(&calNewTeams, "teams", false, "")
	_ = calendarNewCmd.Flags().MarkDeprecated("teams", "use --online-meeting")

	calendarModifyCmd.Flags().StringVar(&calModifyCalendar, "calendar", "", "Only this calendar (by display name)")
	calendarModifyCmd.Flags().StringVarP(&calModifySubject, "subject", "s", "", "Replace the event subject")
	calendarModifyCmd.Flags().StringVar(&calModifyStart, "start", "", "Replace start (same formats as calendar new)")
	calendarModifyCmd.Flags().StringVar(&calModifyEnd, "end", "", "Replace end (same formats as calendar new)")
	calendarModifyCmd.Flags().StringVar(&calModifyDuration, "duration", "", "Replace duration instead of --end (e.g. 30m, 1h30m)")
	calendarModifyCmd.Flags().BoolVar(&calModifyAllDay, "all-day", false, "Set all-day state (use --all-day=false to clear)")
	calendarModifyCmd.Flags().StringVar(&calModifyLocation, "location", "", "Replace location (empty clears it)")
	calendarModifyCmd.Flags().StringVar(&calModifyDescription, "description", "", "Replace description (empty clears it)")

	calendarRsvpCmd.Flags().StringVar(&calRsvpCalendar, "calendar", "", "Only this calendar (by display name)")
	calendarDeleteCmd.Flags().StringVar(&calDeleteCalendar, "calendar", "", "Only this calendar (by display name)")
	calendarDeleteCmd.Flags().BoolVar(&calDeleteYes, "yes", false, "Delete without confirmation")
}

func runCalendarNew(cmd *cobra.Command, args []string) error {
	cols, err := resolveVdirCollections(args[0])
	if err != nil {
		return err
	}
	label := args[0]
	if calNewCalendar == "" || calNewSubject == "" || calNewStart == "" {
		return fmt.Errorf("--calendar, --subject and --start are required")
	}
	if calNewEnd != "" && calNewDuration != "" {
		return fmt.Errorf("use either --end or --duration, not both")
	}

	now := time.Now()
	start, startAllDay, err := calendar.ParseWhen(calNewStart, now)
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

	attendees := make([]calendar.Attendee, 0, len(calNewAttendees))
	for _, email := range calNewAttendees {
		email = strings.TrimSpace(email)
		if email == "" {
			continue
		}
		if !strings.Contains(email, "@") {
			return fmt.Errorf("invalid --attendee %q: not an email address", email)
		}
		attendees = append(attendees, calendar.Attendee{Email: email, Type: "required"})
	}

	event := calendar.Event{
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

	path, err := calendar.WriteEventIn(cols, calNewCalendar, event)
	if err != nil {
		return err
	}

	fmt.Println(okLine("Created %q in %q", calNewSubject, calNewCalendar))
	fmt.Fprintf(os.Stderr, "  %s\n", styDim(path))
	printSyncReminder(label, len(attendees) > 0 || calNewTeams)
	return nil
}

type calendarModifyOptions struct {
	subject, start, end, duration, location, description string
	allDay                                               bool
	subjectSet, startSet, endSet, durationSet            bool
	allDaySet, locationSet, descriptionSet               bool
}

func runCalendarModify(cmd *cobra.Command, args []string) error {
	cols, err := resolveVdirCollections(args[0])
	if err != nil {
		return fmt.Errorf("resolve calendar collections: %w", err)
	}
	path, event, calName, err := calendar.ResolveEventIn(cols, args[1], calModifyCalendar)
	if err != nil {
		return fmt.Errorf("resolve event: %w", err)
	}
	for _, col := range cols {
		if col.ReadOnly && strings.EqualFold(calendar.CollectionName(col), calName) {
			return fmt.Errorf("calendar %q is read-only: %w", calName, calendar.ErrReadOnly)
		}
	}

	opts := calendarModifyOptions{
		subject: calModifySubject, start: calModifyStart, end: calModifyEnd,
		duration: calModifyDuration, allDay: calModifyAllDay,
		location: calModifyLocation, description: calModifyDescription,
		subjectSet: cmd.Flags().Changed("subject"), startSet: cmd.Flags().Changed("start"),
		endSet: cmd.Flags().Changed("end"), durationSet: cmd.Flags().Changed("duration"),
		allDaySet: cmd.Flags().Changed("all-day"), locationSet: cmd.Flags().Changed("location"),
		descriptionSet: cmd.Flags().Changed("description"),
	}
	updated, err := applyCalendarModify(event, opts, time.Now())
	if err != nil {
		return fmt.Errorf("modify event: %w", err)
	}
	data, err := calendar.EventToICal(updated)
	if err != nil {
		return fmt.Errorf("failed to serialize event: %w", err)
	}
	if err := calendar.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("failed to write %s: %w", path, err)
	}

	fmt.Println(okLine("Modified %q in %q locally", orDash(updated.Subject), calName))
	owner := calendar.CollectionOwner(cols, calName)
	printSyncReminder(args[0], len(updated.Attendees) > 0 && isOwnerOrganizer(updated, owner))
	return nil
}

// applyCalendarModify is the pure patch operation behind the CLI command.
// Keeping it independent of Cobra and the filesystem pins the important rule:
// omitted flags preserve the complete parsed Event, including recurrence and
// meeting metadata.
func applyCalendarModify(event calendar.Event, opts calendarModifyOptions,
	now time.Time) (calendar.Event, error) {
	if !opts.subjectSet && !opts.startSet && !opts.endSet && !opts.durationSet &&
		!opts.allDaySet && !opts.locationSet && !opts.descriptionSet {
		return event, fmt.Errorf("provide at least one field to modify")
	}
	if opts.endSet && opts.durationSet {
		return event, fmt.Errorf("use either --end or --duration, not both")
	}

	originalStart, originalEnd, originalAllDay := event.Start, event.End, event.AllDay
	if opts.subjectSet {
		event.Subject = opts.subject
	}
	if opts.locationSet {
		event.Location = opts.location
	}
	if opts.descriptionSet {
		event.Description = opts.description
	}
	if opts.startSet {
		start, _, err := calendar.ParseWhen(opts.start, now)
		if err != nil {
			return event, fmt.Errorf("parse --start: %w", err)
		}
		event.Start = start
	}
	if opts.allDaySet {
		event.AllDay = opts.allDay
	}
	if originalAllDay && !event.AllDay && (!opts.startSet || (!opts.endSet && !opts.durationSet)) {
		return event, fmt.Errorf("clearing --all-day requires --start and either --end or --duration")
	}

	switch {
	case opts.endSet:
		end, _, err := calendar.ParseWhen(opts.end, now)
		if err != nil {
			return event, fmt.Errorf("parse --end: %w", err)
		}
		event.End = end
	case opts.durationSet:
		duration, err := time.ParseDuration(opts.duration)
		if err != nil {
			return event, fmt.Errorf("invalid --duration %q: %w", opts.duration, err)
		}
		if duration <= 0 {
			return event, fmt.Errorf("--duration must be positive")
		}
		event.End = event.Start.Add(duration)
	case event.AllDay && (opts.startSet || opts.allDaySet):
		days := 1
		if originalAllDay {
			startDay := calendar.DateOnly(originalStart)
			endDay := calendar.DateOnly(originalEnd)
			days = max(1, int(endDay.Sub(startDay)/(24*time.Hour)))
		}
		event.End = event.Start.AddDate(0, 0, days)
	case opts.startSet:
		event.End = event.Start.Add(originalEnd.Sub(originalStart))
	}

	if event.AllDay {
		event.Start = calendar.DateOnly(event.Start)
		event.End = calendar.DateOnly(event.End)
		if !event.End.After(event.Start) {
			event.End = event.Start.AddDate(0, 0, 1)
		}
	}
	if !event.End.After(event.Start) {
		return event, fmt.Errorf("end %s is not after start %s",
			event.End.Format(time.RFC3339), event.Start.Format(time.RFC3339))
	}
	return event, nil
}

// eventEnd computes the end time from --end/--duration, or a default (1h for a
// timed event, +1 day for an all-day one).
func eventEnd(start time.Time, allDay bool, now time.Time) (time.Time, error) {
	var end time.Time
	switch {
	case calNewEnd != "":
		e, _, err := calendar.ParseWhen(calNewEnd, now)
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
	cols, err := resolveVdirCollections(args[0])
	if err != nil {
		return err
	}
	label := args[0]
	ref := args[1]
	response, err := calendar.ParseRSVPVerb(args[2])
	if err != nil {
		return err
	}

	path, event, calName, err := calendar.ResolveEventIn(cols, ref, calRsvpCalendar)
	if err != nil {
		return err
	}
	owner := calendar.CollectionOwner(cols, calName)

	if isOwnerOrganizer(event, owner) {
		return fmt.Errorf("cannot RSVP to %q — you are the organizer", orDash(event.Subject))
	}
	if !calendar.SetOwnerResponse(&event, owner, response) {
		return fmt.Errorf("you (%s) are not an attendee of %q, cannot RSVP", owner, orDash(event.Subject))
	}

	data, err := calendar.EventToICal(event)
	if err != nil {
		return fmt.Errorf("failed to serialize event: %w", err)
	}
	if err := calendar.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("failed to write %s: %w", path, err)
	}

	fmt.Println(okLine("RSVP %q set to %q in %q", orDash(event.Subject), rsvpVerbLabel(response), calName))
	printSyncReminder(label, true)
	return nil
}

func rsvpVerbLabel(r calendar.OwnerResp) string {
	switch r {
	case calendar.OwnerRespAccepted:
		return "accepted"
	case calendar.OwnerRespDeclined:
		return "declined"
	default:
		return "tentative"
	}
}

// isOwnerOrganizer reports whether the account owner organizes this event.
func isOwnerOrganizer(e calendar.Event, owner string) bool {
	if e.OwnerResponse == calendar.OwnerRespOrganizer {
		return true
	}
	return e.Organizer != nil && owner != "" && strings.EqualFold(e.Organizer.Email, owner)
}

func runCalendarDelete(cmd *cobra.Command, args []string) error {
	cols, err := resolveVdirCollections(args[0])
	if err != nil {
		return err
	}
	label := args[0]
	ref := args[1]

	path, event, calName, err := calendar.ResolveEventIn(cols, ref, calDeleteCalendar)
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
	printSyncReminder(label, len(event.Attendees) > 0)
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
	// A local calendar has no remote side to apply anything to: the write is
	// already the whole story, and pointing at a sync command that refuses
	// this identifier would be worse than saying nothing.
	if strings.EqualFold(account, config.LocalCalendarAccount) {
		return
	}
	msg := fmt.Sprintf("Run 'durian calendar sync %s' to apply.", account)
	if mayNotify {
		msg += " It will preview any attendee notifications before sending."
	}
	fmt.Fprintln(os.Stderr, styDim(msg))
}
