package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/julion2/durian/cli/internal/store"
	"github.com/spf13/cobra"
)

var (
	outboxReconcileOutcome string
	outboxReconcileYes     bool
)

var outboxCmd = &cobra.Command{
	Use:   "outbox",
	Short: "Inspect and reconcile durable outgoing messages",
}

var outboxListCmd = &cobra.Command{
	Use:   "list",
	Short: "List queued and claimed outgoing messages",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		db, err := openEmailDBReadOnly()
		if err != nil {
			return err
		}
		defer db.Close()
		entries, err := listOutboxEntries(db)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(entries)
		}
		if len(entries) == 0 {
			fmt.Println("Outbox is empty.")
			return nil
		}
		for _, entry := range entries {
			fmt.Printf("%d  %s  %s\n", entry.ID, entry.Status, humanText(entry.MessageID, false))
			fmt.Printf("    created: %s; attempts: %d\n", time.Unix(entry.CreatedAt, 0).Format(time.RFC3339), entry.Attempts)
			if entry.LastError != "" {
				fmt.Printf("    last error: %s\n", humanText(entry.LastError, false))
			}
		}
		return nil
	},
}

var outboxReconcileCmd = &cobra.Command{
	Use:   "reconcile <id>",
	Short: "Record a provider-verified delivery outcome",
	Long: `Record a provider-verified delivery outcome for a claimed outbox item.

Stop Durian's GUI and server first, then search the provider for the exact
Message-ID shown by 'durian outbox list'. Choosing not-delivered requeues the
message and may cause a duplicate delivery if the provider verification was
wrong. Non-interactive use requires --yes.`,
	Args: cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("invalid outbox item ID %q", args[0])
		}
		releaseOutbox, err := store.AcquireOutboxLifecycle(store.DefaultDBPath())
		if err != nil {
			if errors.Is(err, store.ErrOutboxLifecycleLocked) {
				return errors.New("cannot reconcile while the Durian GUI or server owns the outbox; stop it and retry")
			}
			return err
		}
		defer releaseOutbox()
		db, err := openEmailDB()
		if err != nil {
			return err
		}
		defer db.Close()
		entry, err := outboxEntryForReconciliation(db, id, outboxReconcileOutcome)
		if err != nil {
			return err
		}
		confirmed, err := confirmOutboxReconciliation(entry, outboxReconcileOutcome, outboxReconcileYes)
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Fprintln(os.Stderr, "aborted, outbox item unchanged")
			if jsonOutput {
				return writeJSON(map[string]any{"id": id, "status": "aborted"})
			}
			return nil
		}
		status, err := reconcileOutboxEntry(db, entry, outboxReconcileOutcome)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(map[string]any{"id": id, "message_id": entry.MessageID, "status": status})
		}
		fmt.Printf("Reconciled outbox item %d (%s): %s.\n", id, humanText(entry.MessageID, false), status)
		return nil
	},
}

type outboxCLIEntry struct {
	ID                int64  `json:"id"`
	MessageID         string `json:"message_id"`
	Status            string `json:"status"`
	Attempts          int    `json:"attempts"`
	LastError         string `json:"last_error,omitempty"`
	CreatedAt         int64  `json:"created_at"`
	InFlight          bool   `json:"in_flight"`
	DeliveryConfirmed bool   `json:"delivery_confirmed"`
}

func init() {
	outboxCmd.AddCommand(outboxListCmd, outboxReconcileCmd)
	rootCmd.AddCommand(outboxCmd)
	outboxReconcileCmd.Flags().StringVar(&outboxReconcileOutcome, "outcome", "", "verified outcome: delivered or not-delivered")
	outboxReconcileCmd.Flags().BoolVar(&outboxReconcileYes, "yes", false, "record the verified outcome without prompting")
}

func listOutboxEntries(db *store.DB) ([]outboxCLIEntry, error) {
	items, err := db.ListOutbox()
	if err != nil {
		return nil, err
	}
	entries := make([]outboxCLIEntry, 0, len(items))
	for _, item := range items {
		var draft struct {
			MessageID string `json:"message_id"`
		}
		if err := json.Unmarshal([]byte(item.DraftJSON), &draft); err != nil {
			return nil, fmt.Errorf("decode outbox item %d: %w", item.ID, err)
		}
		status := "queued"
		switch {
		case item.DeliveryConfirmed:
			status = "delivery-confirmed"
		case item.InFlight:
			status = "verification-required"
		case item.Attempts >= 5:
			status = "failed"
		}
		entries = append(entries, outboxCLIEntry{
			ID: item.ID, MessageID: draft.MessageID, Status: status, Attempts: item.Attempts,
			LastError: item.LastError, CreatedAt: item.CreatedAt, InFlight: item.InFlight,
			DeliveryConfirmed: item.DeliveryConfirmed,
		})
	}
	return entries, nil
}

func outboxEntryForReconciliation(db *store.DB, id int64, outcome string) (outboxCLIEntry, error) {
	if outcome != "delivered" && outcome != "not-delivered" {
		return outboxCLIEntry{}, errors.New("--outcome must be delivered or not-delivered")
	}
	entries, err := listOutboxEntries(db)
	if err != nil {
		return outboxCLIEntry{}, err
	}
	for _, entry := range entries {
		if entry.ID != id {
			continue
		}
		if !entry.InFlight {
			return outboxCLIEntry{}, fmt.Errorf("%w: %d", store.ErrOutboxItemNotInFlight, id)
		}
		if entry.MessageID == "" {
			return outboxCLIEntry{}, errors.New("outbox item has no durable Message-ID and cannot be verified safely")
		}
		if outcome == "not-delivered" && entry.DeliveryConfirmed {
			return outboxCLIEntry{}, fmt.Errorf("%w: %d", store.ErrOutboxDeliveryConfirmed, id)
		}
		return entry, nil
	}
	return outboxCLIEntry{}, fmt.Errorf("%w: %d", store.ErrOutboxItemNotFound, id)
}

func confirmOutboxReconciliation(entry outboxCLIEntry, outcome string, yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	if !canPrompt() {
		return false, errors.New("cannot confirm reconciliation because input is not interactive; pass --yes after provider verification")
	}
	meaning := "was delivered; remove the durable claim"
	if outcome == "not-delivered" {
		meaning = "was not delivered; requeue it for sending"
	}
	prompt := fmt.Sprintf("Confirm exact Message-ID %q %s?", humanText(entry.MessageID, false), meaning)
	return confirmPrompt(prompt), nil
}

func reconcileOutboxEntry(db *store.DB, entry outboxCLIEntry, outcome string) (string, error) {
	switch outcome {
	case "delivered":
		if err := db.DeleteClaimedOutboxItem(entry.ID); err != nil {
			return "", err
		}
		return "removed as delivered", nil
	case "not-delivered":
		if err := db.RequeueClaimedOutboxItem(entry.ID, "Provider delivery verified as not delivered; queued for retry"); err != nil {
			return "", err
		}
		return "requeued as not delivered", nil
	default:
		return "", errors.New("--outcome must be delivered or not-delivered")
	}
}
