package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"

	"github.com/durian-dev/durian/cli/internal/contacts"
	"github.com/spf13/cobra"
)

var contactsCmd = &cobra.Command{
	Use:   "contacts",
	Short: "Manage contacts database",
	Long:  `Manage the local contacts database for email autocomplete.`,
}

var contactsInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize the contacts database",
	Long:  `Create the contacts database file and schema.`,
	RunE:  runContactsInit,
}

var contactsImportCmd = &cobra.Command{
	Use:   "import",
	Short: "Import contacts from email store",
	Long: "Import email addresses from the local email store (From, To, Cc headers).",
	RunE: runContactsImport,
}

var contactsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all contacts",
	Long:  `List all contacts in the database, ordered by usage.`,
	RunE:  runContactsList,
}

var contactsSearchCmd = &cobra.Command{
	Use:   "search <query>",
	Short: "Search contacts",
	Long:  `Search contacts by email or name prefix. Used for autocomplete.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runContactsSearch,
}

var contactsAddCmd = &cobra.Command{
	Use:   "add <email> [name]",
	Short: "Add a contact manually",
	Long:  `Add a new contact to the database manually.`,
	Args:  cobra.RangeArgs(1, 2),
	RunE:  runContactsAdd,
}

var contactsDeleteCmd = &cobra.Command{
	Use:   "delete <email>",
	Short: "Delete a contact",
	Long:  `Remove a contact from the database by email.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runContactsDelete,
}

// Flags
var (
	contactsLimit int
)

func init() {
	rootCmd.AddCommand(contactsCmd)
	contactsCmd.AddCommand(contactsInitCmd)
	contactsCmd.AddCommand(contactsImportCmd)
	contactsCmd.AddCommand(contactsListCmd)
	contactsCmd.AddCommand(contactsSearchCmd)
	contactsCmd.AddCommand(contactsAddCmd)
	contactsCmd.AddCommand(contactsDeleteCmd)

	// Add flags
	contactsListCmd.Flags().IntVarP(&contactsLimit, "limit", "n", 100, "maximum number of contacts to show")
	contactsSearchCmd.Flags().IntVarP(&contactsLimit, "limit", "n", 20, "maximum number of results")
}

// getDBPath returns the database path from config or default
func getDBPath() string {
	if cfg != nil && cfg.Contacts.DBPath != "" {
		return cfg.Contacts.DBPath
	}
	return contacts.DefaultDBPath()
}

func runContactsInit(cmd *cobra.Command, args []string) error {
	dbPath := getDBPath()
	slog.Debug("Initializing contacts database", "path", dbPath)

	db, err := contacts.Open(dbPath, bootstrapKeyring())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	if err := db.Init(); err != nil {
		return fmt.Errorf("initialize schema: %w", err)
	}

	if jsonOutput {
		output := map[string]string{
			"status":  "ok",
			"db_path": dbPath,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(output)
	}

	fmt.Printf("Contacts database initialized at: %s\n", dbPath)
	return nil
}

func runContactsImport(cmd *cobra.Command, args []string) error {
	dbPath := getDBPath()
	slog.Debug("Importing contacts", "path", dbPath)

	// Open/create database
	db, err := contacts.Open(dbPath, bootstrapKeyring())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	// Initialize schema if needed
	if err := db.Init(); err != nil {
		return fmt.Errorf("initialize schema: %w", err)
	}

	// Open email store for address extraction
	emailDB, err := openEmailDB()
	if err != nil {
		return fmt.Errorf("open email store: %w", err)
	}
	defer emailDB.Close()

	fmt.Println("Extracting addresses from email store...") // encgrep:allow user-facing TUI, no PII
	contactList, err := contacts.ImportFromStore(emailDB)
	if err != nil {
		return fmt.Errorf("import from store: %w", err)
	}

	slog.Debug("Found unique addresses", "count", len(contactList))

	// Add to database
	fmt.Printf("Importing %d contacts...\n", len(contactList))
	added, updated, err := db.AddBatch(contactList)
	if err != nil {
		return fmt.Errorf("add contacts: %w", err)
	}

	// Clean up any invalid entries
	cleaned, _ := db.CleanInvalid()
	if cleaned > 0 {
		fmt.Printf("Cleaned %d invalid entries\n", cleaned)
	}

	// Get final count
	total, _ := db.Count()

	if jsonOutput {
		output := map[string]interface{}{
			"status":   "ok",
			"imported": added,
			"updated":  updated,
			"total":    total,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(output)
	}

	fmt.Printf("Import complete: %d contacts added, %d total in database\n", added, total)
	return nil
}

func runContactsList(cmd *cobra.Command, args []string) error {
	dbPath := getDBPath()

	db, err := contacts.Open(dbPath, bootstrapKeyring())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	contactList, err := db.List(contactsLimit)
	if err != nil {
		return fmt.Errorf("list contacts: %w", err)
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(contactList)
	}

	if len(contactList) == 0 {
		fmt.Println("No contacts found. Run 'durian contacts import' to import from your email store.") // encgrep:allow user-facing TUI, no PII
		return nil
	}

	// Pretty print with tabwriter
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "EMAIL\tNAME\tUSAGE\tSOURCE")
	fmt.Fprintln(w, "-----\t----\t-----\t------")
	for _, c := range contactList {
		name := c.Name
		if name == "" {
			name = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", c.Email, name, c.UsageCount, c.Source)
	}
	w.Flush()

	total, _ := db.Count()
	if total > contactsLimit {
		fmt.Printf("\n(showing %d of %d contacts, use -n to show more)\n", contactsLimit, total)
	}

	return nil
}

func runContactsSearch(cmd *cobra.Command, args []string) error {
	query := args[0]
	dbPath := getDBPath()

	db, err := contacts.Open(dbPath, bootstrapKeyring())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	contactList, err := db.Search(query, contactsLimit)
	if err != nil {
		return fmt.Errorf("search contacts: %w", err)
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(contactList)
	}

	if len(contactList) == 0 {
		fmt.Printf("No contacts found matching '%s'\n", query)
		return nil
	}

	for _, c := range contactList {
		fmt.Println(c.FormatDisplay())
	}

	return nil
}

func runContactsAdd(cmd *cobra.Command, args []string) error {
	email := args[0]
	name := ""
	if len(args) > 1 {
		name = args[1]
	}

	dbPath := getDBPath()

	db, err := contacts.Open(dbPath, bootstrapKeyring())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	// Initialize schema if needed
	if err := db.Init(); err != nil {
		return fmt.Errorf("initialize schema: %w", err)
	}

	if err := db.Add(email, name, contacts.SourceManual); err != nil {
		return fmt.Errorf("add contact: %w", err)
	}

	if jsonOutput {
		output := map[string]string{
			"status": "ok",
			"email":  email,
			"name":   name,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(output)
	}

	fmt.Printf("Added contact: %s", email) // encgrep:allow user-facing TUI echoes user's own input
	if name != "" {
		fmt.Printf(" (%s)", name)
	}
	fmt.Println()

	return nil
}

func runContactsDelete(cmd *cobra.Command, args []string) error {
	email := args[0]
	dbPath := getDBPath()

	db, err := contacts.Open(dbPath, bootstrapKeyring())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	if err := db.Delete(email); err != nil {
		return fmt.Errorf("delete contact: %w", err)
	}

	if jsonOutput {
		output := map[string]string{
			"status": "ok",
			"email":  email,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(output)
	}

	fmt.Printf("Deleted contact: %s\n", email) // encgrep:allow user-facing TUI echoes user's own input
	return nil
}
