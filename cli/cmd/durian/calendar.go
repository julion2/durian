package main

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/julion2/durian/cli/internal/calendarsync"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/googlecalendar"
	"github.com/julion2/durian/cli/internal/graphcalendar"
	"github.com/spf13/cobra"
)

// newCalendarProvider builds the calendar sync backend for an account from its
// OAuth provider. Both backends satisfy calendarsync.CalendarProvider, so the
// commands below are provider-agnostic once constructed.
func newCalendarProvider(account *config.AccountConfig) (calendarsync.CalendarProvider, error) {
	if !account.CalendarEnabled() {
		return nil, fmt.Errorf("calendar is disabled for account %s", account.GetAliasOrName())
	}
	if account.OAuth == nil {
		return nil, errors.New("calendar sync requires an OAuth account")
	}
	switch account.OAuth.Provider {
	case "microsoft":
		return graphcalendar.New(account)
	case "google":
		return googlecalendar.New(account)
	default:
		return nil, fmt.Errorf("calendar sync not supported for provider %q", account.OAuth.Provider)
	}
}

var calendarCmd = &cobra.Command{
	Use:   "calendar",
	Short: "Calendar operations",
	Long:  "Work with your calendars (Microsoft Outlook or Google Calendar) via a local vdir.",
}

var calendarExportCmd = &cobra.Command{
	Use:   "export <account>",
	Short: "Export your calendars as vdir .ics files",
	Long: `Export all calendars of an account (Microsoft Outlook or Google
Calendar) into a vdir layout that vdirsyncer / khal can read: one directory
per calendar (with a displayname file) and one .ics file per event instance
in the export window.

The export is one-way and read-only (Calendars.Read); recurring events are
written as expanded occurrences, so no RRULEs appear in the output.`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeAccounts,
	RunE:              runCalendarExport,
}

var calendarSyncCmd = &cobra.Command{
	Use:   "sync [account]",
	Short: "Two-way sync your calendars with a local vdir",
	Long: `Synchronize one account, or every enabled calendar-capable account
when the account is omitted, with a local vdir of .ics files (one directory per
calendar, one file per master event, named by the event UID; recurring series
are stored as their master with an RRULE).

Remote changes are applied locally (download new/updated events, prune events
deleted remotely), and local changes are pushed to the remote calendar
(create new events, update edited ones, delete remotely what was deleted
locally).
Conflicts — events changed on both sides — are resolved per the account's
calendar conflict policy ("newer" by default — the side modified last wins;
a conflicting local file is always backed up to <file>.conflict-<timestamp>
first).

Meetings are fully supported: creating an event with ATTENDEE lines sends
invitations, organizer edits send updates, deleting an organizer meeting
sends cancellations, deleting a meeting you merely attend declines it, and
changing your own PARTSTAT sends an RSVP to the organizer (suppress the
response email with --silent-rsvp). An X-DURIAN-CREATE-TEAMS-MEETING:TRUE
line requests an online meeting (Teams or Google Meet) on create.

The sync first builds a plan and prints it, including a preview of every
email the plan will cause the provider to send. If the plan contains a local
archive or remote changes (uploads, remote deletes, conflicts, RSVPs), it asks
for confirmation before applying — declining aborts that account's run,
local-only actions included, so "no" always means no changes for that account.
--yes skips every prompt; --dry-run stops after printing each plan.`,
	Args:              cobra.MaximumNArgs(1),
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
		"Print the sync plan without writing files, changing the remote calendar or saving state")
	calendarSyncCmd.Flags().BoolVar(&calendarSyncYes, "yes", false,
		"Apply changes to the remote calendar without asking for confirmation")
	calendarSyncCmd.Flags().BoolVar(&calendarSyncSilentRSVP, "silent-rsvp", false,
		"Record RSVP responses (accept/decline) without notifying the organizer")
}

func runCalendarExport(cmd *cobra.Command, args []string) error {
	cfg := GetConfig()
	if cfg == nil {
		return errors.New("no configuration loaded")
	}

	if err := rejectLocalCalendarAccount(args[0]); err != nil {
		return err
	}
	account, err := cfg.GetAccountByIdentifier(args[0])
	if err != nil {
		return fmt.Errorf("account not found: %s\nAvailable accounts: %s", args[0], cfg.ListAccountIdentifiers())
	}
	client, err := newCalendarProvider(account)
	if err != nil {
		return err
	}

	// Base dir: --out overrides; else the configured vdir_path; else the default.
	// The account's calendars go under base/<account-dir>/ (khal layout).
	outDir := filepath.Join(config.CalendarBaseDir(cfg, calendarExportOut), account.CalendarDir())

	now := time.Now()
	from := now.AddDate(0, 0, -calendarExportDaysBack)
	to := now.AddDate(0, 0, calendarExportDaysForward)

	include := account.CalendarInclude()
	stats, err := calendarsync.Export(cmd.Context(), client, outDir, from, to, include)
	if err != nil {
		if client.IsAuthError(err) {
			return fmt.Errorf("calendar export failed — calendar access may not be granted, run 'durian auth login %s' to consent: %w",
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

// rejectLocalCalendarAccount refuses the reserved local-calendar identifier for
// the commands that talk to a provider.
//
// Local calendars have no provider and no remote side at all. Falling through
// to the account lookup would answer "account not found", which names the
// symptom and hides the reason — the identifier is valid, it just has nothing
// to sync from.
func rejectLocalCalendarAccount(identifier string) error {
	if strings.EqualFold(identifier, config.LocalCalendarAccount) {
		return fmt.Errorf("%q holds local-only calendars — they have no provider to sync or export from",
			config.LocalCalendarAccount)
	}
	return nil
}

func runCalendarSync(cmd *cobra.Command, args []string) error {
	cfg := GetConfig()
	if cfg == nil {
		return errors.New("no configuration loaded")
	}
	accounts, err := calendarSyncTargets(cfg, args)
	if err != nil {
		return err
	}
	reader := bufio.NewReader(os.Stdin)
	var syncErrs []error
	for _, account := range accounts {
		if len(accounts) > 1 {
			fmt.Fprintf(os.Stderr, "\n== Syncing %s ==\n", account.GetAliasOrName())
		}
		if err := runCalendarSyncAccount(cmd, cfg, account, reader); err != nil {
			syncErrs = append(syncErrs, fmt.Errorf("%s: %w", account.GetAliasOrName(), err))
		}
	}
	return errors.Join(syncErrs...)
}

// calendarSyncTargets resolves the optional account argument. With no
// argument, unsupported/password-only accounts are skipped: there is no
// provider calendar to sync for them, while one such mail account should not
// prevent all eligible accounts from running.
func calendarSyncTargets(cfg *config.Config, args []string) ([]*config.AccountConfig, error) {
	if len(args) == 1 {
		if err := rejectLocalCalendarAccount(args[0]); err != nil {
			return nil, err
		}
		account, err := cfg.GetAccountByIdentifier(args[0])
		if err != nil {
			return nil, fmt.Errorf("account not found: %s\nAvailable accounts: %s", args[0], cfg.ListAccountIdentifiers())
		}
		if !account.CalendarEnabled() {
			return nil, fmt.Errorf("calendar is disabled for account %s", account.GetAliasOrName())
		}
		return []*config.AccountConfig{account}, nil
	}

	accounts := make([]*config.AccountConfig, 0, len(cfg.Accounts))
	for i := range cfg.Accounts {
		account := &cfg.Accounts[i]
		if account.CalendarEnabled() && account.OAuth != nil &&
			(account.OAuth.Provider == "microsoft" || account.OAuth.Provider == "google") {
			accounts = append(accounts, account)
		}
	}
	if len(accounts) == 0 {
		return nil, errors.New("no calendar-capable OAuth accounts configured")
	}
	return accounts, nil
}

func runCalendarSyncAccount(cmd *cobra.Command, cfg *config.Config, account *config.AccountConfig, reader *bufio.Reader) error {
	client, err := newCalendarProvider(account)
	if err != nil {
		return err
	}

	accountDir := filepath.Join(config.CalendarBaseDir(cfg, calendarSyncOut), account.CalendarDir())

	// Run lock: one whole Load -> Plan -> Apply -> Save cycle per account dir
	// at a time, across processes — the serve autosync loop takes the same
	// lock, so a background run and this command can never plan from the same
	// baseline and double-execute or clobber each other's saved state.
	release, ok, err := calendarsync.AcquireRunLock(accountDir)
	if err != nil {
		return fmt.Errorf("failed to acquire calendar sync run lock: %w", err)
	}
	if !ok {
		return fmt.Errorf("another calendar sync is running for this account (dir: %s) — try again in a moment", accountDir)
	}
	defer release()

	// The status lives inside accountDir, so it is bound to this exact local
	// collection (see FileStateStore doc): syncing the same account to a
	// different directory must not reuse another directory's status.
	store := calendarsync.NewFileStateStore(accountDir)
	state, err := store.Load()
	if err != nil {
		return fmt.Errorf("failed to load calendar sync state: %w", err)
	}
	state, mailboxBackup, err := calendarsync.BindMailbox(accountDir, state, account.Email, account.IsDelegatedMailbox(), !calendarSyncDryRun)
	if err != nil {
		if errors.Is(err, calendarsync.ErrMailboxRebindNeeded) {
			return fmt.Errorf("calendar sync must first quarantine the legacy vdir for %s; run once without --dry-run", account.GetAliasOrName())
		}
		return fmt.Errorf("failed to bind calendar vdir to mailbox: %w", err)
	}
	if mailboxBackup != "" {
		fmt.Fprintf(os.Stderr, "Existing calendar vdir belonged to the OAuth user's mailbox; moved it to %s before syncing %s.\n", mailboxBackup, account.Email)
	}

	// The mirror holds the last known remote state and the download cursors.
	// It is filled in by PlanAll and persisted only once the plan has been
	// applied — every early return below (dry run, declined gate) therefore
	// leaves the cursor where it was, so the next run sees the same changes
	// again instead of skipping them.
	mirrorStore := calendarsync.NewFileMirrorStore(accountDir)
	mirror, err := mirrorStore.Load()
	if err != nil {
		return fmt.Errorf("failed to load calendar mirror: %w", err)
	}

	plans, err := calendarsync.PlanAll(cmd.Context(), client, accountDir, account.CalendarInclude(), state, mirror)
	if err != nil {
		if client.IsAuthError(err) {
			return fmt.Errorf("calendar sync failed — calendar access may not be granted, run 'durian auth login %s' to consent: %w",
				account.GetAliasOrName(), err)
		}
		return fmt.Errorf("calendar sync failed: %w", err)
	}

	policy := cfg.CalendarConflictPolicy(account)
	summary := summarizePlans(plans, policy)
	fmt.Fprintf(os.Stderr, "Plan for %s: %d download(s), %d prune(s), %d archive(s), %d upload(s), %d update(s), %d remote delete(s), %d conflict(s), %d RSVP(s)\n",
		account.GetAliasOrName(), summary.downloads, summary.prunes,
		summary.archives, summary.uploadCreates, summary.uploadUpdates, summary.deleteRemotes,
		summary.conflicts, summary.rsvps)
	const maxListed = 20
	for i, line := range summary.gatedLines {
		if i == maxListed {
			fmt.Fprintf(os.Stderr, "  ... and %d more\n", len(summary.gatedLines)-maxListed)
			break
		}
		fmt.Fprintln(os.Stderr, "  "+line)
	}

	// Notification preview: every email applying this plan will make Graph
	// send, enumerated BEFORE the confirmation gate.
	notifications := calendarsync.PlanNotifications(plans, policy, calendarSyncSilentRSVP)
	printNotificationPreview(notifications)

	if calendarSyncDryRun {
		fmt.Println("Dry run: nothing applied.")
		return nil
	}

	gatedCount := len(summary.gatedLines)
	if gatedCount > 0 && !calendarSyncYes {
		fmt.Fprintf(os.Stderr, "Apply %d gated change(s) for %s (%d local archives, %d uploads, %d remote deletes, %d conflicts, %d RSVPs; %d notification message(s))? [y/N] ",
			gatedCount, account.GetAliasOrName(), summary.archives, summary.uploadCreates+summary.uploadUpdates, summary.deleteRemotes,
			summary.conflicts, summary.rsvps, len(notifications))
		answer, readErr := reader.ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		// A read error (closed/empty stdin, non-tty) counts as "no". Declining
		// aborts the whole run — local-only downloads/prunes included — so the
		// answer "no" always means "nothing changed for this account".
		if readErr != nil || (answer != "y" && answer != "yes") {
			fmt.Fprintln(os.Stderr, "aborted, no changes made")
			return nil
		}
	}

	opts := calendarsync.SyncOptions{Conflict: policy, SilentRSVP: calendarSyncSilentRSVP}
	stats, applyErr := calendarsync.ApplyAll(cmd.Context(), client, state, plans, opts)
	// Save state even on partial failure: remote operations that already
	// succeeded are recorded in the status and must not be replayed.
	if saveErr := store.Save(state); saveErr != nil {
		saveErr = fmt.Errorf("failed to save calendar sync state: %w", saveErr)
		if applyErr != nil {
			return errors.Join(applyErr, saveErr)
		}
		return saveErr
	}
	// The mirror follows the state, and for the same reason: the actions that
	// did NOT get applied left no baseline behind, so the next run replans
	// them against this same mirror. A mirror that failed to save is not worth
	// failing the sync over — it only costs one full download next time.
	if saveErr := mirrorStore.Save(mirror); saveErr != nil {
		slog.Warn("Failed to save calendar mirror, next sync reads in full", "module", "CALENDAR",
			"account", account.GetAliasOrName(), "err", saveErr) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
	}
	if applyErr != nil {
		if client.IsAuthError(applyErr) {
			return fmt.Errorf("calendar sync failed — calendar access may not be granted, run 'durian auth login %s' to consent: %w",
				account.GetAliasOrName(), applyErr)
		}
		return fmt.Errorf("calendar sync failed: %w", applyErr)
	}

	fmt.Printf("Calendar sync for %s: %d downloaded, %d pruned, %d archived, %d uploaded, %d deleted remotely, %d RSVP(s) sent, %d conflict(s) resolved (%s wins), %d skipped, %d failed (dir: %s)\n", // encgrep:allow user-facing CLI summary contains account alias and local directory, not encrypted content
		account.GetAliasOrName(), stats.Downloaded, stats.Pruned,
		stats.Archived, stats.Uploaded, stats.DeletedRemote, stats.Rsvps, stats.Conflicts, policy,
		stats.Skipped, stats.Failed, accountDir)
	if stats.Failed > 0 {
		// Per-event failures did not abort the run (the remaining events
		// synced), but they must not pass silently either: report them and
		// exit non-zero. The failed items re-plan on the next run.
		return fmt.Errorf("%d event(s) failed to sync — see the log for details, the rest was synced", stats.Failed)
	}
	return nil
}

// printNotificationPreview lists every email message the plan will make Graph
// send — category, event, calendar, recipient count — plus a total line, or
// an explicit all-clear when the plan sends nothing.
func printNotificationPreview(notifications []calendarsync.Notification) {
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
// formatted list of actions that require confirmation.
type planSummary struct {
	downloads     int
	prunes        int
	archives      int
	uploadCreates int
	uploadUpdates int
	deleteRemotes int
	conflicts     int
	rsvps         int
	gatedLines    []string
}

// summarizePlans counts every planned action and renders one human-readable
// line per action that requires confirmation (remote writes or local archive).
func summarizePlans(plans []calendarsync.CalendarPlan, conflictPolicy string) planSummary {
	var s planSummary
	for _, p := range plans {
		for _, a := range p.Actions {
			switch a.Kind {
			case calendarsync.ActionDownloadNew, calendarsync.ActionDownloadUpdate:
				s.downloads++
			case calendarsync.ActionPruneLocal:
				s.prunes++
			case calendarsync.ActionArchiveLocal:
				s.archives++
				s.gatedLines = append(s.gatedLines, fmt.Sprintf("ARCHIVE LOCAL: %s [%s]", a.Summary, p.Calendar.Name))
			case calendarsync.ActionUploadCreate:
				s.uploadCreates++
				s.gatedLines = append(s.gatedLines, fmt.Sprintf("UPLOAD (create): %s [%s]", a.Summary, p.Calendar.Name))
			case calendarsync.ActionUploadUpdate:
				s.uploadUpdates++
				s.gatedLines = append(s.gatedLines, fmt.Sprintf("UPLOAD (update): %s [%s]", a.Summary, p.Calendar.Name))
			case calendarsync.ActionDeleteRemote:
				s.deleteRemotes++
				s.gatedLines = append(s.gatedLines, fmt.Sprintf("DELETE REMOTE: %s [%s]", a.Summary, p.Calendar.Name))
			case calendarsync.ActionConflict:
				s.conflicts++
				s.gatedLines = append(s.gatedLines, fmt.Sprintf("CONFLICT (%s wins): %s [%s]", conflictPolicy, a.Summary, p.Calendar.Name))
			case calendarsync.ActionRsvp:
				// Rebaseline-only RSVPs touch no remote state and are not
				// listed; a real response is a gated remote mutation.
				if a.RemoteMutation() {
					s.rsvps++
					s.gatedLines = append(s.gatedLines, fmt.Sprintf("RSVP (%s): %s [%s]", a.Rsvp, a.Summary, p.Calendar.Name))
				}
			}
		}
	}
	return s
}
