package main

import (
	"errors"
	"slices"
	"testing"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/keychain"
)

func TestCredentialRequestsKeepsJMAPAndIMAPSecretsSeparate(t *testing.T) {
	account := &config.AccountConfig{
		SyncEngine: "jmap",
		JMAP:       &config.JMAPConfig{Auth: "bearer"},
		IMAP:       config.IMAPConfig{Host: "imap.example.test", Auth: "password"},
	}
	requests := credentialRequests(account)
	if len(requests) != 2 {
		t.Fatalf("credential requests = %+v, want JMAP token and IMAP password", requests)
	}
	if requests[0].service != keychain.JMAPKeychainService || requests[1].service != keychain.PasswordKeychainService {
		t.Fatalf("credential services = %q, %q", requests[0].service, requests[1].service)
	}
}

func TestDeleteCredentialsRemovesJMAPAndLegacyEntries(t *testing.T) {
	existing := map[string]bool{
		keychain.JMAPKeychainService:     true,
		keychain.PasswordKeychainService: true,
	}
	var removed []string
	found, err := deleteCredentials("me@example.test",
		[]string{keychain.JMAPKeychainService, keychain.PasswordKeychainService},
		func(service, _ string) bool { return existing[service] },
		func(service, _ string) error {
			removed = append(removed, service)
			return nil
		})
	if err != nil || !found {
		t.Fatalf("deleteCredentials() found=%v err=%v", found, err)
	}
	if !slices.Equal(removed, []string{keychain.JMAPKeychainService, keychain.PasswordKeychainService}) {
		t.Fatalf("removed services = %v", removed)
	}
}

func TestDeleteCredentialsAttemptsEveryServiceAfterFailure(t *testing.T) {
	var removed []string
	found, err := deleteCredentials("me@example.test", []string{"first", "second"},
		func(string, string) bool { return true },
		func(service, _ string) error {
			removed = append(removed, service)
			if service == "first" {
				return errors.New("keychain unavailable")
			}
			return nil
		})
	if !found || err == nil {
		t.Fatalf("deleteCredentials() found=%v err=%v", found, err)
	}
	if !slices.Equal(removed, []string{"first", "second"}) {
		t.Fatalf("removed services = %v, want both attempted", removed)
	}
}
