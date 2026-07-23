package main

import (
	"errors"
	"fmt"
	"path/filepath"
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

var (
	calendarExportOut         string
	calendarExportDaysBack    int
	calendarExportDaysForward int
)

func init() {
	rootCmd.AddCommand(calendarCmd)
	calendarCmd.AddCommand(calendarExportCmd)

	calendarExportCmd.Flags().StringVar(&calendarExportOut, "out", "",
		"Output directory (default: $XDG_DATA_HOME/durian/calendars)")
	calendarExportCmd.Flags().IntVar(&calendarExportDaysBack, "days-back", 30,
		"Include events starting up to this many days in the past")
	calendarExportCmd.Flags().IntVar(&calendarExportDaysForward, "days-forward", 365,
		"Include events starting up to this many days in the future")
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
	base := calendarExportOut
	if base == "" {
		base = config.ExpandPath(cfg.Calendar.VdirPath)
	}
	if base == "" {
		base = filepath.Join(config.DefaultDataDir(), "calendars")
	}
	outDir := filepath.Join(base, account.CalendarDir())

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
