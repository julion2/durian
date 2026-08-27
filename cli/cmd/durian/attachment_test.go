package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/dbcrypto"
	"github.com/julion2/durian/cli/internal/store"
)

func TestResolveAttachmentMessageRequiresAccountWhenAmbiguous(t *testing.T) {
	db := newAttachmentTestStore(t)
	now := time.Now().Unix()
	for _, account := range []string{"work", "personal"} {
		if err := db.InsertMessage(&store.Message{
			MessageID: "shared@example.com",
			Account:   account,
			Subject:   "Shared",
			FromAddr:  "sender@example.com",
			Date:      now,
			CreatedAt: now,
		}); err != nil {
			t.Fatalf("insert %s message: %v", account, err)
		}
	}
	cfg := &config.Config{Accounts: []config.AccountConfig{
		{Name: "Work", Alias: "office", Email: "work@example.com"},
		{Name: "Personal", Email: "personal@example.com"},
	}}

	if _, err := resolveAttachmentMessage(db, cfg, "shared@example.com", ""); err == nil || !strings.Contains(err.Error(), "multiple accounts") {
		t.Fatalf("unqualified resolution error = %v, want ambiguity", err)
	}
	message, err := resolveAttachmentMessage(db, cfg, "shared@example.com", "office")
	if err != nil {
		t.Fatalf("resolve by alias: %v", err)
	}
	if message.Account != "work" {
		t.Fatalf("resolved account = %q, want work", message.Account)
	}
}

func TestCreateAttachmentFileDoesNotOverwriteByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.pdf")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if f, err := createAttachmentFile(path, false); err == nil {
		f.Close()
		t.Fatal("createAttachmentFile() overwrote existing file without force")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	if string(data) != "original" {
		t.Fatalf("existing content = %q, want original", data)
	}

	f, err := createAttachmentFile(path, true)
	if err != nil {
		t.Fatalf("create with force: %v", err)
	}
	if _, err := f.WriteString("replacement"); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close replacement: %v", err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read replacement: %v", err)
	}
	if string(data) != "replacement" {
		t.Fatalf("forced content = %q, want replacement", data)
	}
}

func TestNormalizeMessageReference(t *testing.T) {
	for _, input := range []string{"id@example.com", "<id@example.com>", "message:id@example.com", "MESSAGE:<id@example.com>"} {
		if got := normalizeMessageReference(input); got != "id@example.com" {
			t.Errorf("normalizeMessageReference(%q) = %q", input, got)
		}
	}
}

func newAttachmentTestStore(t *testing.T) *store.DB {
	t.Helper()
	keyring, err := dbcrypto.NewKeyring(bytes.Repeat([]byte{0x42}, dbcrypto.MasterKeyLen))
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	db, err := store.Open(":memory:", keyring)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := db.Init(); err != nil {
		db.Close()
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
