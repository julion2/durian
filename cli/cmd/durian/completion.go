package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// completeAccounts returns Cobra-style completions of every configured account
// identifier (alias, name, or email — same set used by GetAccountByIdentifier).
//
// Usage:
//
//	cmd.ValidArgsFunction = completeAccounts
//	cmd.RegisterFlagCompletionFunc("account", completeAccounts)
func completeAccounts(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	cfg := GetConfig()
	if cfg == nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return cfg.ListAccountIdentifiers(), cobra.ShellCompDirectiveNoFileComp
}

func resolveAccountStoreKeys(identifiers []string) ([]string, error) {
	cfg := GetConfig()
	if cfg == nil {
		return nil, fmt.Errorf("no configuration loaded")
	}
	keys := make([]string, 0, len(identifiers))
	seen := make(map[string]bool, len(identifiers))
	for _, identifier := range identifiers {
		account, err := cfg.GetAccountByIdentifier(identifier)
		if err != nil {
			return nil, fmt.Errorf("account not found: %s\navailable accounts: %s", identifier, strings.Join(cfg.ListAccountIdentifiers(), ", "))
		}
		key := account.AccountIdentifier()
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func scopeQueryByAccounts(query string, identifiers []string) (string, error) {
	if len(identifiers) == 0 {
		return query, nil
	}
	keys, err := resolveAccountStoreKeys(identifiers)
	if err != nil {
		return "", err
	}
	return scopeQueryByAccountKeys(query, keys), nil
}

// scopeQueryByAccountKeys narrows a query to already-resolved store keys, for
// callers that need the same keys for something else and should not resolve
// them twice.
func scopeQueryByAccountKeys(query string, keys []string) string {
	if len(keys) == 0 {
		return query
	}
	clauses := make([]string, len(keys))
	for i, key := range keys {
		clauses[i] = "path:" + strconv.Quote(key)
	}
	return "(" + query + ") AND (" + strings.Join(clauses, " OR ") + ")"
}
