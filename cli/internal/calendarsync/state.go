// Persistent status for the two-way calendar sync (vdirsyncer model): per
// calendar, per item UID, the last-synced identity of both sides — the
// provider's event id, the content hash of the remote event and the hash of
// the local .ics file. Comparing the current state of each side against this
// status is what lets the sync engine distinguish "changed here" from
// "changed there" from "deleted".
//
// FileStateStore mirrors syncengine.FileCursorStore / imap.StateManager file
// handling: one JSON file per account under the XDG cache dir, an exclusive
// flock guard around every read/write, atomic temp-file + rename writes, and
// corrupted files backed up and treated as empty (a lost status only costs a
// re-convergence run, never data).

package calendarsync

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ItemStatus is the last-synced snapshot of one calendar item (keyed by its
// iCalendar UID) on both sides.
type ItemStatus struct {
	// RemoteID is the provider's event id, needed to address the remote event
	// for future uploads/deletes. The JSON tag keeps the historical "graph_id"
	// name so existing state files stay valid.
	RemoteID string `json:"graph_id"`
	// RemoteHash is the eventContentHash of the remote event at last sync; a
	// differing current content hash means the remote side changed. The remote
	// etag is deliberately NOT used here: it is not stable — Graph rewrites it
	// between a write and subsequent reads without any content change, which
	// would misread every freshly written pair as "remote changed".
	RemoteHash string `json:"remote_hash"`
	// LocalHash is the SHA-256 hex digest of the local .ics file bytes at last
	// sync; a differing current hash means the local side changed.
	LocalHash string `json:"local_hash"`
	// OwnerResponse is the owner's RSVP at last sync — the baseline B of the
	// ActionRsvp three-way diff (local L vs baseline B vs remote R). The zero
	// value ("", None) doubles as the pre-Stage-2 default, which the
	// idempotency guard re-baselines without any remote call.
	OwnerResponse OwnerResp `json:"owner_response,omitempty"`
	// AttendeeHash is the attendeeSetHash of the attendee set (emails+types,
	// responses excluded) at last sync, so an attendee add/remove is detected
	// as a real scheduling change and attendee-only edits can be scoped into
	// their own PATCH. "" means unknown (pre-Stage-2 status file).
	AttendeeHash string `json:"attendee_hash,omitempty"`
}

// CalendarStatus is the sync status of one calendar: one ItemStatus per item
// UID.
type CalendarStatus struct {
	Items map[string]ItemStatus `json:"items"`
}

// SyncState is the whole per-account sync status: one CalendarStatus per
// remote calendar id.
type SyncState struct {
	Calendars map[string]CalendarStatus `json:"calendars"`
}

// newSyncState returns an empty, fully initialized SyncState.
func newSyncState() *SyncState {
	return &SyncState{Calendars: make(map[string]CalendarStatus)}
}

// normalize initializes any nil maps (e.g. after unmarshaling a sparse file)
// so callers can index without nil checks.
func (s *SyncState) normalize() {
	if s.Calendars == nil {
		s.Calendars = make(map[string]CalendarStatus)
	}
	for id, cal := range s.Calendars {
		if cal.Items == nil {
			cal.Items = make(map[string]ItemStatus)
			s.Calendars[id] = cal
		}
	}
}

// FileStateStore persists a SyncState INSIDE the account's vdir directory at
// <accountDir>/.durian-calsync-state.json. The status belongs to the local
// collection, not the account: keying it by account alone would make the same
// account synced to two different directories share one status, which
// misclassifies every item in the empty directory as a local deletion (and
// would delete the whole remote calendar). Storing it in the dir means a fresh
// directory starts with fresh state (correct first-sync = download all), and
// deleting the dir resets the sync. The hidden dotfile sits at the vdir base,
// above the per-calendar collection subdirs, so khal/vdirsyncer ignore it.
// Follows the syncengine.FileCursorStore file handling (0700 dir, 0600 file,
// flock on a .lock sibling, atomic temp + rename).
type FileStateStore struct {
	dir string
}

// NewFileStateStore creates a state store whose status file lives inside dir
// (the account's vdir base directory, i.e. <vdir_path>/<account-dir>).
func NewFileStateStore(dir string) *FileStateStore {
	return &FileStateStore{dir: dir}
}

// path returns the state file path inside the vdir directory.
func (f *FileStateStore) path() string {
	return filepath.Join(f.dir, ".durian-calsync-state.json")
}

// acquireLock takes an exclusive flock on <path>.lock, retrying with backoff
// for up to 5 seconds (same pattern as syncengine.FileCursorStore) so two
// concurrent syncs cannot interleave read-modify-write cycles.
func (f *FileStateStore) acquireLock() (*os.File, error) {
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create vdir dir: %w", err)
	}

	lockFile, err := os.OpenFile(f.path()+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("failed to open calendar state lock file: %w", err)
	}

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		return lockFile, nil
	}

	slog.Debug("Calendar state lock busy, waiting", "module", "CALSYNC", "dir", f.dir)
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

	closeLockFile(lockFile)
	return nil, fmt.Errorf("calendar state lock timeout for %s", f.dir)
}

// releaseLock unlocks and closes the lock file.
func releaseLock(lockFile *os.File) {
	if lockFile != nil {
		if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
			slog.Warn("Failed to release calendar state lock", "module", "GRAPHCAL",
				"path", lockFile.Name(), "err", err)
		}
		closeLockFile(lockFile)
	}
}

// closeLockFile closes the flock handle and reports a failure instead of
// discarding it. Nothing is ever written through this descriptor — it exists
// only to carry the advisory lock — so a Close error cannot lose data here,
// but swallowing it would hide a descriptor leak, and an unexplained silent
// Close is exactly what static analysis flags on a writable handle.
func closeLockFile(lockFile *os.File) {
	if err := lockFile.Close(); err != nil {
		slog.Warn("Failed to close calendar state lock file", "module", "GRAPHCAL",
			"path", lockFile.Name(), "err", err)
	}
}

// Load reads the sync state for an account. A missing file yields an empty
// state; a corrupted file is backed up and treated as empty (the next sync
// re-converges via the first-sight rules rather than losing data).
func (f *FileStateStore) Load() (*SyncState, error) {
	lockFile, err := f.acquireLock()
	if err != nil {
		return nil, err
	}
	defer releaseLock(lockFile)

	path := f.path()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return newSyncState(), nil
		}
		return nil, fmt.Errorf("failed to read calendar state file: %w", err)
	}

	state := &SyncState{}
	if err := json.Unmarshal(data, state); err != nil {
		backupPath := fmt.Sprintf("%s.corrupted.%d", path, time.Now().Unix())
		if renameErr := os.Rename(path, backupPath); renameErr != nil {
			slog.Warn("Corrupted calendar state file, backup failed", "module", "CALSYNC",
				"path", path, "err", renameErr)
		} else {
			slog.Warn("Corrupted calendar state file backed up, starting fresh", "module", "CALSYNC",
				"backup", backupPath)
		}
		return newSyncState(), nil
	}
	state.normalize()
	return state, nil
}

// Save persists the sync state for an account atomically (temp file + rename)
// under the flock.
func (f *FileStateStore) Save(state *SyncState) error {
	lockFile, err := f.acquireLock()
	if err != nil {
		return err
	}
	defer releaseLock(lockFile)

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal calendar state: %w", err)
	}

	path := f.path()
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("failed to write calendar state file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename calendar state file: %w", err)
	}
	return nil
}
