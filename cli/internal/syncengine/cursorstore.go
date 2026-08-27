// Package syncengine drives a provider-agnostic backend.Backend: it walks the
// backend's folders, pages incremental changes via opaque per-folder cursors,
// ingests messages into the SQLite store, and applies folder/flag/rule tags.
//
// It coexists with the legacy imap.Syncer. Backend-specific cursor suffixes let
// Graph, Gmail, JMAP, and opt-in IMAP state coexist without one implementation
// reading another's cursor format.
package syncengine

import (
	"bytes"
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
	// GetState returns the cursor and pending work for (account, folder).
	GetState(account, folder string) (FolderState, error)
	// Commit atomically persists a cursor and its pending flag work.
	Commit(account, folder string, cursor backend.Cursor, pending PendingFlags) error
}

// PendingFlags is flag reconciliation work that must survive engine restarts.
// ReplayCount records whether a replacement snapshot has already been replayed
// after its first failed flag pass.
type PendingFlags struct {
	Refs        []string `json:"refs,omitempty"`
	FullScan    bool     `json:"fullScan,omitempty"`
	ScanAfterID int64    `json:"scanAfterId,omitempty"`
	ReplayCount int      `json:"replayCount,omitempty"`
}

// FolderState is the atomic persisted state for one backend folder.
type FolderState struct {
	Cursor       backend.Cursor `json:"cursor,omitempty"`
	PendingFlags PendingFlags   `json:"pendingFlags,omitempty"`
}

type cursorFile struct {
	Version int                    `json:"version"`
	Folders map[string]FolderState `json:"folders"`
}

// pendingFlagsKey stores the new pending-work map inside the legacy
// map[folder][]byte cursor file. The NUL prefix cannot be a provider folder
// name. Older binaries preserve this unknown entry while continuing to read
// every real folder cursor verbatim, so downgrading does not force a resync.
const pendingFlagsKey = "\x00durian.pendingFlags.v1"

// FileCursorStore is a CursorStore backed by one legacy-compatible JSON file
// per account at <cacheDir>/<account>-backend-cursors.json. Real folder values
// remain opaque cursors; pending work lives under pendingFlagsKey.
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
func (f *FileCursorStore) load(account string) (map[string]FolderState, error) {
	path := f.path(account)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]FolderState), nil
		}
		return nil, fmt.Errorf("read cursor file: %w", err)
	}

	var envelope cursorFile
	if err := json.Unmarshal(data, &envelope); err == nil && envelope.Version == 1 && envelope.Folders != nil {
		// c327b9e briefly wrote a versioned envelope that pre-366 binaries could
		// not decode. Rewrite it once under the caller's lock into the compatible
		// representation before returning any state.
		if err := f.save(account, envelope.Folders); err != nil {
			return nil, fmt.Errorf("migrate versioned cursor file: %w", err)
		}
		return envelope.Folders, nil
	}

	// Files written before pending flag state used a plain map. Read that
	// representation indefinitely so upgrades never force a full resync.
	legacy := make(map[string][]byte)
	if err := json.Unmarshal(data, &legacy); err != nil {
		backupPath := fmt.Sprintf("%s.corrupted.%d", path, time.Now().Unix())
		if renameErr := os.Rename(path, backupPath); renameErr != nil {
			slog.Warn("Corrupted cursor file, backup failed", "module", "SYNCENGINE", "path", path, "err", renameErr)
		} else {
			slog.Warn("Corrupted cursor file backed up, starting fresh", "module", "SYNCENGINE", "backup", backupPath)
		}
		return make(map[string]FolderState), nil
	}
	pendingByFolder := make(map[string]PendingFlags)
	pendingCorrupted := false
	if pendingJSON, ok := legacy[pendingFlagsKey]; ok {
		if err := json.Unmarshal(pendingJSON, &pendingByFolder); err != nil {
			// The opaque cursors are still valid, but discarding unknown pending
			// refs could lose flag changes already crossed by those cursors. Mark
			// every real folder for a lossless full reconciliation instead.
			slog.Warn("Undecodable pending flag state, scheduling full reconciliation",
				"module", "SYNCENGINE", "path", path, "err", err)
			pendingByFolder = make(map[string]PendingFlags)
			pendingCorrupted = true
		}
		delete(legacy, pendingFlagsKey)
	}
	states := make(map[string]FolderState, len(legacy))
	for folder, cursor := range legacy {
		pending := pendingByFolder[folder]
		if pendingCorrupted {
			pending.FullScan = true
		}
		states[folder] = FolderState{Cursor: backend.Cursor(cursor), PendingFlags: pending}
	}
	if pendingCorrupted {
		if err := f.save(account, states); err != nil {
			return nil, fmt.Errorf("persist pending flag recovery: %w", err)
		}
	}
	return states, nil
}

// Get returns the persisted cursor for (account, folder), or nil if none.
func (f *FileCursorStore) Get(account, folder string) (backend.Cursor, error) {
	state, err := f.GetState(account, folder)
	return state.Cursor, err
}

// GetState returns the cursor and pending work for (account, folder).
func (f *FileCursorStore) GetState(account, folder string) (FolderState, error) {
	account = f.resolveAccount(account)
	lockFile, err := f.acquireLock(account)
	if err != nil {
		return FolderState{}, err
	}
	defer releaseLock(lockFile)

	cursors, err := f.load(account)
	if err != nil {
		return FolderState{}, err
	}
	return cursors[folder], nil
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
	state := cursors[folder]
	if bytes.Equal(state.Cursor, cursor) {
		return nil
	}
	state.Cursor = cursor
	cursors[folder] = state
	return f.save(account, cursors)
}

func (f *FileCursorStore) GetPendingFlags(account, folder string) (PendingFlags, error) {
	state, err := f.GetState(account, folder)
	return state.PendingFlags, err
}

func (f *FileCursorStore) Commit(account, folder string, cursor backend.Cursor, pending PendingFlags) error {
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
	cursors[folder] = FolderState{Cursor: cursor, PendingFlags: pending}
	return f.save(account, cursors)
}

func (f *FileCursorStore) save(account string, cursors map[string]FolderState) error {
	legacy := make(map[string][]byte, len(cursors)+1)
	pendingByFolder := make(map[string]PendingFlags)
	for folder, state := range cursors {
		legacy[folder] = []byte(state.Cursor)
		if len(state.PendingFlags.Refs) > 0 || state.PendingFlags.FullScan || state.PendingFlags.ReplayCount != 0 {
			pendingByFolder[folder] = state.PendingFlags
		}
	}
	if len(pendingByFolder) > 0 {
		pendingJSON, err := json.Marshal(pendingByFolder)
		if err != nil {
			return fmt.Errorf("marshal pending flags: %w", err)
		}
		legacy[pendingFlagsKey] = pendingJSON
	}
	data, err := json.MarshalIndent(legacy, "", "  ")
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
