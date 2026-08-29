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

	f, err := createAttachmentFile(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := commitAttachmentFile(f, path, false); err == nil {
		t.Fatal("commit overwrote an existing file without force")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	if string(data) != "original" {
		t.Fatalf("existing content = %q, want original", data)
	}

	f, err = createAttachmentFile(path)
	if err != nil {
		t.Fatalf("create with force: %v", err)
	}
	if _, err := f.WriteString("replacement"); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	if err := commitAttachmentFile(f, path, true); err != nil {
		t.Fatalf("commit with force: %v", err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read replacement: %v", err)
	}
	if string(data) != "replacement" {
		t.Fatalf("forced content = %q, want replacement", data)
	}
}

// TestAttachmentDownloadFailureKeepsExistingFile is the contract --force used to
// break. Opening the destination with O_TRUNC emptied the user's file before
// the download had produced a byte, and the cleanup on failure then removed
// what was left — a failed download destroyed the file it was meant to replace.
//
// The destination may only change once the content exists.
func TestAttachmentDownloadFailureKeepsExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.pdf")
	if err := os.WriteFile(path, []byte("irreplaceable"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	// A download that starts, writes something, and then fails.
	f, err := createAttachmentFile(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.WriteString("partial"); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	discardAttachmentFile(f)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("destination is gone after a failed download: %v", err)
	}
	if string(data) != "irreplaceable" {
		t.Errorf("destination = %q, want the original content untouched", data)
	}

	// And nothing is left lying around next to it.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory = %v, want only the destination", names)
	}
}

// TestAttachmentCommitReservesTheNameWithoutForce covers the window between the
// caller's early existence check and the rename. Without force the destination
// is reserved with O_EXCL, so a file that appears in between is reported rather
// than silently replaced.
func TestAttachmentCommitReservesTheNameWithoutForce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "late.pdf")

	f, err := createAttachmentFile(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.WriteString("downloaded"); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Someone else gets there first, after the command's own check passed.
	if err := os.WriteFile(path, []byte("someone else"), 0o600); err != nil {
		t.Fatalf("seed racing file: %v", err)
	}

	if err := commitAttachmentFile(f, path, false); err == nil {
		t.Fatal("commit replaced a file that appeared after the initial check")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "someone else" {
		t.Errorf("destination = %q, want the racing content kept", data)
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
