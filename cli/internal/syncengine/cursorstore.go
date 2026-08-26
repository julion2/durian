// Package syncengine drives a provider-agnostic backend.Backend: it walks the
// backend's folders, pages incremental changes via opaque per-folder cursors,
// ingests messages into the SQLite store, and applies folder/flag/rule tags.
//
// It coexists with the legacy imap.Syncer. Backend-specific cursor suffixes let
// Graph, Gmail, JMAP, and opt-in IMAP state coexist without one implementation
// reading another's cursor format.
package syncengine

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
)

// CursorStore persists per-folder incremental-sync cursors. Cursors are opaque
// to the engine (owned by the Backend that issued them); the store just keeps
// them safe between runs.
type CursorStore interface {
	// Get returns the persisted cursor for (account, folder), or nil if none.
	Get(account, folder string) (backend.Cursor, error)
	// Set persists the cursor for (account, folder).
	Set(account, folder string, cursor backend.Cursor) error
}

// FileCursorStore is a CursorStore backed by one JSON file per account at
// <cacheDir>/<account>-backend-cursors.json holding map[folder][]byte
// (the []byte cursor is base64-encoded by encoding/json).
//
// It mirrors the legacy imap.StateManager file handling: cache dir resolved
// via XDG_CACHE_HOME else ~/.cache, subdir "durian" with mode 0700, an
// exclusive flock guard around every read/write, and atomic writes via
// temp-file + rename so a crash never leaves a torn file.
type FileCursorStore struct {
	cacheDir string
	// account is the default account used when Get/Set receive an empty
	// account argument; when non-empty arguments are passed they win, so one
	// FileCursorStore can serve multiple accounts (one file each).
	account string
	// suffix namespaces the cursor file by backend, so a cursor written by one
	// backend (e.g. an IMAP MailboxState) is never fed to a different backend
	// (e.g. a Graph deltaLink) for the same account. Empty for IMAP, "-graph"
	// for Graph, "-gmail" for Gmail, and "-jmap" for JMAP.
	suffix string
}

// NewFileCursorStore creates a cursor store for the given account (IMAP backend).
func NewFileCursorStore(account string) *FileCursorStore {
	return NewFileCursorStoreWithSuffix(account, "")
}

// NewFileCursorStoreWithSuffix creates a cursor store whose file name is
// namespaced by suffix, so different backends for the same account keep separate
// cursor files (their cursor payloads are not interchangeable).
func NewFileCursorStoreWithSuffix(account, suffix string) *FileCursorStore {
	cacheDir := os.Getenv("XDG_CACHE_HOME")
	if cacheDir == "" {
		home, _ := os.UserHomeDir()
		cacheDir = filepath.Join(home, ".cache")
	}
	return &FileCursorStore{
		cacheDir: filepath.Join(cacheDir, "durian"),
		account:  account,
		suffix:   suffix,
	}
}

// resolveAccount applies the constructor default when no account is given.
func (f *FileCursorStore) resolveAccount(account string) string {
	if account == "" {
		return f.account
	}
	return account
}

// path returns the cursor file path for an account.
func (f *FileCursorStore) path(account string) string {
	return filepath.Join(f.cacheDir, fmt.Sprintf("%s%s-backend-cursors.json", account, f.suffix))
}

// acquireLock takes an exclusive flock on <path>.lock, retrying with backoff
// for up to 5 seconds (same pattern as imap.StateManager.acquireLock) so two
// concurrent syncs cannot interleave read-modify-write cycles.
func (f *FileCursorStore) acquireLock(account string) (*os.File, error) {
	if err := os.MkdirAll(f.cacheDir, 0700); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	if err := os.Chmod(f.cacheDir, 0700); err != nil {
		return nil, fmt.Errorf("chmod cache dir: %w", err)
	}

	lockFile, err := os.OpenFile(f.path(account)+".lock", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("open cursor lock file: %w", err)
	}

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		return lockFile, nil
	}

	slog.Debug("Cursor lock busy, waiting", "module", "SYNCENGINE", "account", account) // encgrep:allow account identifier (config name), not an encrypted column
	deadline := time.Now().Add(5 * time.Second)
	delay := 250 * time.Millisecond
	for time.Now().Before(deadline) {
		time.Sleep(delay)
		if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return lockFile, nil
		}
		if delay < 2*time.Second {
			delay *= 2
		}
	}

	lockFile.Close()
	return nil, fmt.Errorf("cursor lock timeout for account %s", account)
}

// releaseLock unlocks and closes the lock file.
func releaseLock(lockFile *os.File) {
	if lockFile != nil {
		syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		lockFile.Close()
	}
}

// load reads the cursor map for an account. A missing file yields an empty
// map; a corrupted file is backed up and treated as empty (a lost cursor only
// costs a full resync, never data).
func (f *FileCursorStore) load(account string) (map[string][]byte, error) {
	path := f.path(account)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string][]byte), nil
		}
		return nil, fmt.Errorf("read cursor file: %w", err)
	}

	cursors := make(map[string][]byte)
	if err := json.Unmarshal(data, &cursors); err != nil {
		backupPath := fmt.Sprintf("%s.corrupted.%d", path, time.Now().Unix())
		if renameErr := os.Rename(path, backupPath); renameErr != nil {
			slog.Warn("Corrupted cursor file, backup failed", "module", "SYNCENGINE", "path", path, "err", renameErr)
		} else {
			slog.Warn("Corrupted cursor file backed up, starting fresh", "module", "SYNCENGINE", "backup", backupPath)
		}
		return make(map[string][]byte), nil
	}
	return cursors, nil
}

// Get returns the persisted cursor for (account, folder), or nil if none.
func (f *FileCursorStore) Get(account, folder string) (backend.Cursor, error) {
	account = f.resolveAccount(account)
	lockFile, err := f.acquireLock(account)
	if err != nil {
		return nil, err
	}
	defer releaseLock(lockFile)

	cursors, err := f.load(account)
	if err != nil {
		return nil, err
	}
	cursor, ok := cursors[folder]
	if !ok {
		return nil, nil
	}
	return backend.Cursor(cursor), nil
}

// Set persists the cursor for (account, folder) with a read-modify-write under
// the flock, then an atomic temp-file + rename.
func (f *FileCursorStore) Set(account, folder string, cursor backend.Cursor) error {
	account = f.resolveAccount(account)
	lockFile, err := f.acquireLock(account)
	if err != nil {
		return err
	}
	defer releaseLock(lockFile)

	cursors, err := f.load(account)
	if err != nil {
		return err
	}
	cursors[folder] = []byte(cursor)

	data, err := json.MarshalIndent(cursors, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cursors: %w", err)
	}

	path := f.path(account)
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("write cursor file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename cursor file: %w", err)
	}
	return nil
}
