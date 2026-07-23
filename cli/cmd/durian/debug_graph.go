package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/graphbackend"
	"github.com/spf13/cobra"
)

// TEMPORARY (phase 2c) live smoke test for the graphbackend read core. Exercises
// FetchFolders + FetchMessages(delta + $value) against real Microsoft Graph.
// Remove once the engine wiring (2d) can drive graphbackend directly.
var debugGraphCmd = &cobra.Command{
	Use:               "debug-graph <account>",
	Short:             "TEMP: smoke-test the Graph backend read core (folders + delta)",
	Hidden:            true,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeAccounts,
	RunE:              runDebugGraph,
}

func init() {
	rootCmd.AddCommand(debugGraphCmd)
}

func runDebugGraph(cmd *cobra.Command, args []string) error {
	cfg := GetConfig()
	if cfg == nil {
		return errors.New("no configuration loaded")
	}
	account, err := cfg.GetAccountByIdentifier(args[0])
	if err != nil {
		return fmt.Errorf("account not found: %s", args[0])
	}

	b, err := graphbackend.New(account)
	if err != nil {
		return err
	}
	defer b.Close()

	ctx := context.Background()

	folders, err := b.FetchFolders(ctx)
	if err != nil {
		return fmt.Errorf("FetchFolders: %w", err)
	}
	fmt.Printf("FetchFolders: %d folders\n", len(folders))
	var inbox *backend.Folder
	for i := range folders {
		f := &folders[i]
		if f.Role != backend.RoleNone {
			fmt.Printf("  role=%-8s display=%q\n", f.Role, f.Display)
		}
		if f.Role == backend.RoleInbox {
			inbox = f
		}
	}
	if inbox == nil {
		return errors.New("no inbox folder found")
	}

	res, err := b.FetchMessages(ctx, inbox.Name, nil, 10)
	if err != nil {
		return fmt.Errorf("FetchMessages(inbox): %w", err)
	}
	fmt.Printf("FetchMessages(inbox): %d new, %d deleted, has_more=%v, cursor=%d bytes\n",
		len(res.Messages), len(res.Deleted), res.HasMore, len(res.Cursor))
	refs := make([]backend.RemoteRef, 0, len(res.Messages))
	for i, m := range res.Messages {
		refs = append(refs, m.Ref)
		if i >= 3 {
			continue
		}
		fmt.Printf("  msg id=%s raw=%d bytes seen=%v flagged=%v labels=%v\n",
			truncate(m.MessageID, 40), len(m.Raw), m.Flags.Seen, m.Flags.Flagged, m.Labels)
	}
	if len(res.Messages) > 3 {
		fmt.Printf("  ... (%d more)\n", len(res.Messages)-3)
	}

	// Read-only: FetchFlags via $batch for the fetched messages.
	flags, err := b.FetchFlags(ctx, inbox.Name, refs)
	if err != nil {
		return fmt.Errorf("FetchFlags: %w", err)
	}
	seen := 0
	for _, f := range flags {
		if f.Seen {
			seen++
		}
	}
	fmt.Printf("FetchFlags($batch): resolved %d/%d refs, %d seen\n", len(flags), len(refs), seen)
	return nil
}
