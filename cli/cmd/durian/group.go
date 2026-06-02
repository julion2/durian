package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/spf13/cobra"
)

var groupCmd = &cobra.Command{
	Use:   "group",
	Short: "Manage contact groups",
	Long:  "Manage contact groups defined in groups.pkl. Edit the file directly to modify.",
}

var groupListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all contact groups",
	RunE:  runGroupList,
}

var groupMembersCmd = &cobra.Command{
	Use:   "members <name>",
	Short: "List members of a group",
	Args:  cobra.ExactArgs(1),
	RunE:  runGroupMembers,
}

func init() {
	rootCmd.AddCommand(groupCmd)
	groupCmd.AddCommand(groupListCmd)
	groupCmd.AddCommand(groupMembersCmd)
}

func loadGroups() (map[string]config.GroupEntry, error) {
	groups, err := config.LoadGroups("")
	if err != nil {
		return nil, err
	}
	if groups == nil {
		return nil, fmt.Errorf("groups.pkl not found at %s", config.GroupsPath())
	}
	return groups, nil
}

func runGroupList(cmd *cobra.Command, args []string) error {
	groups, err := loadGroups()
	if err != nil {
		return err
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(groups)
	}

	if len(groups) == 0 {
		fmt.Println("No groups defined. Edit groups.pkl to add groups.")
		return nil
	}

	// Sort group names for stable output
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "GROUP\tMEMBERS\tDESCRIPTION")
	fmt.Fprintln(w, "-----\t-------\t-----------")
	for _, name := range names {
		group := groups[name]
		desc := group.Description
		if desc == "" {
			desc = "-"
		}
		fmt.Fprintf(w, "%s\t%d\t%s\n", name, len(group.Members), desc)
	}
	w.Flush()

	return nil
}

func runGroupMembers(cmd *cobra.Command, args []string) error {
	groups, err := loadGroups()
	if err != nil {
		return err
	}

	name := args[0]
	group, ok := groups[name]
	if !ok {
		available := make([]string, 0, len(groups))
		for n := range groups {
			available = append(available, n)
		}
		sort.Strings(available)
		return fmt.Errorf("group %q not found (available: %v)", name, available)
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(group)
	}

	if group.Description != "" {
		fmt.Printf("%s — %s\n", name, group.Description)
	} else {
		fmt.Println(name)
	}

	if len(group.Members) == 0 {
		fmt.Println("  (no members)")
		return nil
	}

	for _, person := range group.Members {
		if len(person) == 1 {
			fmt.Printf("  %s\n", person[0])
		} else {
			fmt.Printf("  %s\n", strings.Join(person, ", "))
		}
	}

	return nil
}
