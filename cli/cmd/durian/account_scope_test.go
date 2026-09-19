package main

import (
	"strings"
	"testing"

	"github.com/julion2/durian/cli/internal/config"
)

func TestScopeQueryByAccountsResolvesAliasesAndSpaces(t *testing.T) {
	previousConfig := cfg
	cfg = &config.Config{Accounts: []config.AccountConfig{
		{Name: "Work Mail", Alias: "office", Email: "work@example.com"},
		{Name: "Personal", Alias: "home", Email: "personal@example.com"},
	}}
	t.Cleanup(func() { cfg = previousConfig })

	query, err := scopeQueryByAccounts("tag:unread", []string{"office", "personal@example.com"})
	if err != nil {
		t.Fatalf("scope query: %v", err)
	}
	for _, want := range []string{`path:"work mail"`, `path:"personal"`, " OR "} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q missing %q", query, want)
		}
	}
}

func TestResolveAccountStoreKeysRejectsUnknownAccount(t *testing.T) {
	previousConfig := cfg
	cfg = &config.Config{Accounts: []config.AccountConfig{{Name: "Work", Alias: "office"}}}
	t.Cleanup(func() { cfg = previousConfig })

	if _, err := resolveAccountStoreKeys([]string{"wrok"}); err == nil || !strings.Contains(err.Error(), "office") {
		t.Fatalf("unknown account error = %v", err)
	}
}
