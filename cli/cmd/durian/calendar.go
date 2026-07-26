package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/graphcalendar"
	"github.com/spf13/cobra"
)

var calendarCmd = &cobra.Command{
	Use:   "calendar",
	Short: "Calendar operations",
	Long:  "Work with Outlook calendars via Microsoft Graph.",
}

var calendarExportCmd = &cobra.Command{
	Use:   "export <account>",
	Short: "Export Outlook calendars as vdir .ics files",
	Long: `Export all calendars of a Microsoft account into a vdir layout that
vdirsyncer / khal can read: one directory per calendar (with a displayname
file) and one .ics file per event instance in the export window.

The export is one-way and read-only (Calendars.Read); recurring events are
written as expanded occurrences, so no RRULEs appear in the output.`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeAccounts,
	RunE:              runCalendarExport,
}

var calendarSyncCmd = &cobra.Command{
	Use:   "sync <account>",
	Short: "Two-way sync Outlook calendars with a local vdir",
	Long: `Synchronize the calendars of a Microsoft account with a local vdir of
.ics files (one directory per calendar, one file per master event, named by
the event UID; recurring series are stored as their master with an RRULE).

Remote changes are applied locally (download new/updated events, prune events
deleted in Outlook), and local changes are pushed to Outlook (create new
events, update edited ones, delete remotely what was deleted locally).
Conflicts — events changed on both sides — are resolved per the account's
calendar conflict policy ("remote" by default; a conflicting local file is
always backed up to <file>.conflict-<timestamp> first).

Meetings are fully supported: creating an event with ATTENDEE lines sends
invitations, organizer edits send updates, deleting an organizer meeting
sends cancellations, deleting a meeting you merely attend declines it, and
changing your own PARTSTAT sends an RSVP to the organizer (suppress the
response email with --silent-rsvp). An X-DURIAN-CREATE-TEAMS-MEETING:TRUE
line requests a Teams meeting on create.

The sync first builds a plan and prints it, including a preview of every
email the plan will cause Graph to send. If the plan contains changes to
Outlook (uploads, remote deletes, conflicts, RSVPs), it asks for confirmation
before applying — declining aborts the entire run, local-only actions
included, so "no" always means no changes anywhere. --yes skips the prompt;
--dry-run stops after printing the plan.`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeAccounts,
	RunE:              runCalendarSync,
}

var (
	calendarExportOut         string
	calendarExportDaysBack    int
	calendarExportDaysForward int

	calendarSyncOut        string
	calendarSyncDryRun     bool
	calendarSyncYes        bool
	calendarSyncSilentRSVP bool
)

func init() {
	rootCmd.AddCommand(calendarCmd)
	calendarCmd.AddCommand(calendarExportCmd)
	calendarCmd.AddCommand(calendarSyncCmd)

	calendarExportCmd.Flags().StringVar(&calendarExportOut, "out", "",
		"Output directory (default: $XDG_DATA_HOME/durian/calendars)")
	calendarExportCmd.Flags().IntVar(&calendarExportDaysBack, "days-back", 30,
		"Include events starting up to this many days in the past")
	calendarExportCmd.Flags().IntVar(&calendarExportDaysForward, "days-forward", 365,
		"Include events starting up to this many days in the future")

	calendarSyncCmd.Flags().StringVar(&calendarSyncOut, "out", "",
		"Vdir base directory (default: $XDG_DATA_HOME/durian/calendars)")
	calendarSyncCmd.Flags().BoolVar(&calendarSyncDryRun, "dry-run", false,
		"Print the sync plan without writing files, changing Outlook or saving state")
	calendarSyncCmd.Flags().BoolVar(&calendarSyncYes, "yes", false,
		"Apply changes to Outlook without asking for confirmation")
	calendarSyncCmd.Flags().BoolVar(&calendarSyncSilentRSVP, "silent-rsvp", false,
		"Record RSVP responses (accept/decline) without notifying the organizer")
}

// calendarBaseDir resolves the vdir base directory: the --out override wins,
// then the configured calendar vdir_path, then the default data dir.
func calendarBaseDir(cfg *config.Config, override string) string {
	if override != "" {
		return override
	}
	if base := config.ExpandPath(cfg.Calendar.VdirPath); base != "" {
		return base
	}
	return filepath.Join(config.DefaultDataDir(), "calendars")
}

func runCalendarExport(cmd *cobra.Command, args []string) error {
	cfg := GetConfig()
	if cfg == nil {
		return errors.New("no configuration loaded")
	}

	account, err := cfg.GetAccountByIdentifier(args[0])
	if err != nil {
		return fmt.Errorf("account not found: %s\nAvailable accounts: %s", args[0], cfg.ListAccountIdentifiers())
	}
	if account.OAuth == nil || account.OAuth.Provider != "microsoft" {
		return errors.New("calendar export requires a Microsoft OAuth account")
	}

	client, err := graphcalendar.New(account)
	if err != nil {
		return err
	}

	// Base dir: --out overrides; else the configured vdir_path; else the default.
	// The account's calendars go under base/<account-dir>/ (khal layout).
	outDir := filepath.Join(calendarBaseDir(cfg, calendarExportOut), account.CalendarDir())

	now := time.Now()
	from := now.AddDate(0, 0, -calendarExportDaysBack)
	to := now.AddDate(0, 0, calendarExportDaysForward)

	include := account.CalendarInclude()
	stats, err := graphcalendar.Export(cmd.Context(), client, outDir, from, to, include)
	if err != nil {
		if graphcalendar.IsAuthError(err) {
			return fmt.Errorf("calendar export failed — Graph consent may be missing, run 'durian auth login %s' to consent Calendars.Read: %w",
				account.GetAliasOrName(), err)
		}
		return fmt.Errorf("calendar export failed: %w", err)
	}

	scope := "all calendars"
	if len(include) > 0 {
		scope = fmt.Sprintf("%d selected calendar(s)", len(include))
	}
	fmt.Printf("Exported %d events across %d calendars (%s) to %s\n", stats.Events, stats.Calendars, scope, outDir)
	return nil
}

func runCalendarSync(cmd *cobra.Command, args []string) error {
	cfg := GetConfig()
	if cfg == nil {
		return errors.New("no configuration loaded")
	}

	account, err := cfg.GetAccountByIdentifier(args[0])
	if err != nil {
		return fmt.Errorf("account not found: %s\nAvailable accounts: %s", args[0], cfg.ListAccountIdentifiers())
	}
	if account.OAuth == nil || account.OAuth.Provider != "microsoft" {
		return errors.New("calendar sync requires a Microsoft OAuth account")
	}

	client, err := graphcalendar.New(account)
	if err != nil {
		return err
	}

	accountDir := filepath.Join(calendarBaseDir(cfg, calendarSyncOut), account.CalendarDir())

	// The status lives inside accountDir, so it is bound to this exact local
	// collection (see FileStateStore doc): syncing the same account to a
	// different directory must not reuse another directory's status.
	store := graphcalendar.NewFileStateStore(accountDir)
	state, err := store.Load()
	if err != nil {
		return fmt.Errorf("failed to load calendar sync state: %w", err)
	}

	plans, err := graphcalendar.PlanAll(cmd.Context(), client, accountDir, account.CalendarInclude(), state)
	if err != nil {
		if graphcalendar.IsAuthError(err) {
			return fmt.Errorf("calendar sync failed — Graph consent may be missing, run 'durian auth login %s' to consent Calendars.ReadWrite: %w",
				account.GetAliasOrName(), err)
		}
		return fmt.Errorf("calendar sync failed: %w", err)
	}

	policy := account.CalendarConflictPolicy()
	summary := summarizePlans(plans, policy)
	fmt.Fprintf(os.Stderr, "Plan for %s: %d download(s), %d prune(s), %d upload(s), %d update(s), %d remote delete(s), %d conflict(s), %d RSVP(s)\n",
		account.GetAliasOrName(), summary.downloads, summary.prunes,
		summary.uploadCreates, summary.uploadUpdates, summary.deleteRemotes,
		summary.conflicts, summary.rsvps)
	const maxListed = 20
	for i, line := range summary.remoteLines {
		if i == maxListed {
			fmt.Fprintf(os.Stderr, "  ... and %d more\n", len(summary.remoteLines)-maxListed)
			break
		}
		fmt.Fprintln(os.Stderr, "  "+line)
	}

	// Notification preview: every email applying this plan will make Graph
	// send, enumerated BEFORE the confirmation gate.
	notifications := graphcalendar.PlanNotifications(plans, policy, calendarSyncSilentRSVP)
	printNotificationPreview(notifications)

	if calendarSyncDryRun {
		fmt.Println("Dry run: nothing applied.")
		return nil
	}

	remoteCount := len(summary.remoteLines)
	if remoteCount > 0 && !calendarSyncYes {
		fmt.Fprintf(os.Stderr, "Apply %d change(s) to Outlook (%d uploads, %d remote deletes, %d conflicts, %d RSVPs; %d notification message(s))? [y/N] ",
			remoteCount, summary.uploadCreates+summary.uploadUpdates, summary.deleteRemotes,
			summary.conflicts, summary.rsvps, len(notifications))
		answer, readErr := bufio.NewReader(os.Stdin).ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		// A read error (closed/empty stdin, non-tty) counts as "no". Declining
		// aborts the whole run — local-only downloads/prunes included — so the
		// answer "no" always means "nothing changed anywhere".
		if readErr != nil || (answer != "y" && answer != "yes") {
			fmt.Fprintln(os.Stderr, "aborted, no changes made")
			return nil
		}
	}

	opts := graphcalendar.SyncOptions{Conflict: policy, SilentRSVP: calendarSyncSilentRSVP}
	stats, applyErr := graphcalendar.ApplyAll(cmd.Context(), client, state, plans, opts)
	// Save state even on partial failure: remote operations that already
	// succeeded are recorded in the status and must not be replayed.
	if saveErr := store.Save(state); saveErr != nil {
		saveErr = fmt.Errorf("failed to save calendar sync state: %w", saveErr)
		if applyErr != nil {
			return errors.Join(applyErr, saveErr)
		}
		return saveErr
	}
	if applyErr != nil {
		if graphcalendar.IsAuthError(applyErr) {
			return fmt.Errorf("calendar sync failed — Graph consent may be missing, run 'durian auth login %s' to consent Calendars.ReadWrite: %w",
				account.GetAliasOrName(), applyErr)
		}
		return fmt.Errorf("calendar sync failed: %w", applyErr)
	}

	fmt.Printf("Calendar sync for %s: %d downloaded, %d pruned, %d uploaded, %d deleted remotely, %d RSVP(s) sent, %d conflict(s) resolved (%s wins), %d skipped (dir: %s)\n",
		account.GetAliasOrName(), stats.Downloaded, stats.Pruned,
		stats.Uploaded, stats.DeletedRemote, stats.Rsvps, stats.Conflicts, policy,
		stats.Skipped, accountDir)
	return nil
}

// printNotificationPreview lists every email message the plan will make Graph
// send — category, event, calendar, recipient count — plus a total line, or
// an explicit all-clear when the plan sends nothing.
func printNotificationPreview(notifications []graphcalendar.Notification) {
	if len(notifications) == 0 {
		fmt.Fprintln(os.Stderr, "No emails will be sent.") // encgrep:allow static user-facing CLI text, no message content
		return
	}
	total := 0
	for _, n := range notifications {
		fmt.Fprintf(os.Stderr, "  %s: %s [%s] -> %d recipient(s)\n",
			n.Category, n.Summary, n.Calendar, n.Recipients)
		total += n.Recipients
	}
	fmt.Fprintf(os.Stderr, "%d message(s) to %d recipient(s) will be sent.\n", len(notifications), total)
}

// planSummary aggregates the per-kind counts of all calendar plans plus the
// formatted list of remote-mutating actions for the confirmation output.
type planSummary struct {
	downloads     int
	prunes        int
	uploadCreates int
	uploadUpdates int
	deleteRemotes int
	conflicts     int
	rsvps         int
	remoteLines   []string
}

// summarizePlans counts every planned action and renders one human-readable
// line per action that would change the Outlook calendar.
func summarizePlans(plans []graphcalendar.CalendarPlan, conflictPolicy string) planSummary {
	var s planSummary
	for _, p := range plans {
		for _, a := range p.Actions {
			switch a.Kind {
			case graphcalendar.ActionDownloadNew, graphcalendar.ActionDownloadUpdate:
				s.downloads++
			case graphcalendar.ActionPruneLocal:
				s.prunes++
			case graphcalendar.ActionUploadCreate:
				s.uploadCreates++
				s.remoteLines = append(s.remoteLines, fmt.Sprintf("UPLOAD (create): %s [%s]", a.Summary, p.Calendar.Name))
			case graphcalendar.ActionUploadUpdate:
				s.uploadUpdates++
				s.remoteLines = append(s.remoteLines, fmt.Sprintf("UPLOAD (update): %s [%s]", a.Summary, p.Calendar.Name))
			case graphcalendar.ActionDeleteRemote:
				s.deleteRemotes++
				s.remoteLines = append(s.remoteLines, fmt.Sprintf("DELETE REMOTE: %s [%s]", a.Summary, p.Calendar.Name))
			case graphcalendar.ActionConflict:
				s.conflicts++
				s.remoteLines = append(s.remoteLines, fmt.Sprintf("CONFLICT (%s wins): %s [%s]", conflictPolicy, a.Summary, p.Calendar.Name))
			case graphcalendar.ActionRsvp:
				// Rebaseline-only RSVPs touch no remote state and are not
				// listed; a real response is a gated remote mutation.
				if a.RemoteMutation() {
					s.rsvps++
					s.remoteLines = append(s.remoteLines, fmt.Sprintf("RSVP (%s): %s [%s]", a.Rsvp, a.Summary, p.Calendar.Name))
				}
			}
		}
	}
	return s
}
