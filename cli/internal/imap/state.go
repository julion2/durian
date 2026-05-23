package imap

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/durian-dev/durian/cli/internal/config"
)

// State tracks sync state for an account
type State struct {
	Mailboxes map[string]*MailboxState `json:"mailboxes"`
}

// MailboxState tracks sync state for a mailbox
type MailboxState struct {
	UIDValidity uint32   `json:"uid_validity"`
	LastUID     uint32   `json:"last_uid"`
	SyncedUIDs  []uint32 `json:"synced_uids"`

	// MessageFlags tracks the last-synced flag state for each message
	// Key is UID, value is the FlagState at last sync
	MessageFlags map[uint32]FlagState `json:"message_flags,omitempty"`

	// UIDToMessageID maps IMAP UIDs to Message-IDs
	// This allows us to look up the message for flag sync
	UIDToMessageID map[uint32]string `json:"uid_to_message_id,omitempty"`

	// MessageIDToUID is the reverse mapping for quick lookup
	// Used to find UID when we only have a Message-ID
	MessageIDToUID map[string]uint32 `json:"message_id_to_uid,omitempty"`

	// Transient set for O(1) UID lookups (not serialized)
	syncedSet map[uint32]struct{} `json:"-"`
}

// NewState creates a new empty state
func NewState() *State {
	return &State{
		Mailboxes: make(map[string]*MailboxState),
	}
}

// GetMailboxState returns the state for a mailbox, creating it if needed
func (s *State) GetMailboxState(mailbox string) *MailboxState {
	if s.Mailboxes == nil {
		s.Mailboxes = make(map[string]*MailboxState)
	}

	if _, ok := s.Mailboxes[mailbox]; !ok {
		s.Mailboxes[mailbox] = &MailboxState{
			SyncedUIDs:     make([]uint32, 0),
			MessageFlags:   make(map[uint32]FlagState),
			UIDToMessageID: make(map[uint32]string),
			MessageIDToUID: make(map[string]uint32),
		}
	}

	// Initialize maps if nil (for backwards compatibility with old state files)
	if s.Mailboxes[mailbox].MessageFlags == nil {
		s.Mailboxes[mailbox].MessageFlags = make(map[uint32]FlagState)
	}
	if s.Mailboxes[mailbox].UIDToMessageID == nil {
		s.Mailboxes[mailbox].UIDToMessageID = make(map[uint32]string)
	}
	if s.Mailboxes[mailbox].MessageIDToUID == nil {
		s.Mailboxes[mailbox].MessageIDToUID = make(map[string]uint32)
	}

	return s.Mailboxes[mailbox]
}

// ensureSyncedSet lazily builds the transient set from the SyncedUIDs slice
func (ms *MailboxState) ensureSyncedSet() {
	if ms.syncedSet == nil {
		ms.syncedSet = make(map[uint32]struct{}, len(ms.SyncedUIDs))
		for _, uid := range ms.SyncedUIDs {
			ms.syncedSet[uid] = struct{}{}
		}
	}
}

// IsUIDSynced checks if a UID has been synced
func (ms *MailboxState) IsUIDSynced(uid uint32) bool {
	ms.ensureSyncedSet()
	_, ok := ms.syncedSet[uid]
	return ok
}

// AddSyncedUID marks a UID as synced
func (ms *MailboxState) AddSyncedUID(uid uint32) {
	ms.ensureSyncedSet()
	if _, ok := ms.syncedSet[uid]; !ok {
		ms.syncedSet[uid] = struct{}{}
		ms.SyncedUIDs = append(ms.SyncedUIDs, uid)
		if uid > ms.LastUID {
			ms.LastUID = uid
		}
	}
}

// GetUnsyncedUIDs returns UIDs that haven't been synced yet
func (ms *MailboxState) GetUnsyncedUIDs(allUIDs []uint32) []uint32 {
	ms.ensureSyncedSet()

	var unsynced []uint32
	for _, uid := range allUIDs {
		if _, ok := ms.syncedSet[uid]; !ok {
			unsynced = append(unsynced, uid)
		}
	}

	return unsynced
}

// GetDeletedUIDs returns UIDs that are locally synced but no longer exist on server
// These messages were either deleted or moved to another folder
func (ms *MailboxState) GetDeletedUIDs(serverUIDs []uint32) []uint32 {
	serverSet := make(map[uint32]bool)
	for _, uid := range serverUIDs {
		serverSet[uid] = true
	}

	var deleted []uint32
	for _, uid := range ms.SyncedUIDs {
		if !serverSet[uid] {
			deleted = append(deleted, uid)
		}
	}

	return deleted
}

// RemoveSyncedUID removes a UID from the synced list and cleans up related maps
func (ms *MailboxState) RemoveSyncedUID(uid uint32) {
	ms.ensureSyncedSet()
	delete(ms.syncedSet, uid)

	// Remove from SyncedUIDs slice
	for i, u := range ms.SyncedUIDs {
		if u == uid {
			ms.SyncedUIDs = append(ms.SyncedUIDs[:i], ms.SyncedUIDs[i+1:]...)
			break
		}
	}

	// Clean up MessageFlags
	delete(ms.MessageFlags, uid)

	// Clean up UID <-> MessageID mappings
	if msgID, ok := ms.UIDToMessageID[uid]; ok {
		delete(ms.MessageIDToUID, msgID)
		delete(ms.UIDToMessageID, uid)
	}
}

// NeedsFullResync returns true if UIDVALIDITY changed
func (ms *MailboxState) NeedsFullResync(newUIDValidity uint32) bool {
	return ms.UIDValidity != 0 && ms.UIDValidity != newUIDValidity
}

// Reset clears the mailbox state for a full resync
func (ms *MailboxState) Reset(uidValidity uint32) {
	ms.UIDValidity = uidValidity
	ms.LastUID = 0
	ms.SyncedUIDs = make([]uint32, 0)
	ms.MessageFlags = make(map[uint32]FlagState)
	ms.UIDToMessageID = make(map[uint32]string)
	ms.MessageIDToUID = make(map[string]uint32)
}

// GetMessageFlags returns the stored flag state for a UID
func (ms *MailboxState) GetMessageFlags(uid uint32) (FlagState, bool) {
	if ms.MessageFlags == nil {
		return FlagState{}, false
	}
	flags, ok := ms.MessageFlags[uid]
	return flags, ok
}

// SetMessageFlags stores the flag state for a UID
func (ms *MailboxState) SetMessageFlags(uid uint32, flags FlagState) {
	if ms.MessageFlags == nil {
		ms.MessageFlags = make(map[uint32]FlagState)
	}
	ms.MessageFlags[uid] = flags
}

// GetMessageID returns the Message-ID for a UID
func (ms *MailboxState) GetMessageID(uid uint32) (string, bool) {
	if ms.UIDToMessageID == nil {
		return "", false
	}
	id, ok := ms.UIDToMessageID[uid]
	return id, ok
}

// SetMessageID stores the Message-ID for a UID (both directions)
func (ms *MailboxState) SetMessageID(uid uint32, messageID string) {
	if ms.UIDToMessageID == nil {
		ms.UIDToMessageID = make(map[uint32]string)
	}
	if ms.MessageIDToUID == nil {
		ms.MessageIDToUID = make(map[string]uint32)
	}
	ms.UIDToMessageID[uid] = messageID
	ms.MessageIDToUID[messageID] = uid
}

// GetUIDByMessageID returns the UID for a given Message-ID
func (ms *MailboxState) GetUIDByMessageID(messageID string) (uint32, bool) {
	if ms.MessageIDToUID == nil {
		return 0, false
	}
	uid, ok := ms.MessageIDToUID[messageID]
	return uid, ok
}

// GetMappedUIDCount returns the number of UIDs with Message-ID mapping
func (ms *MailboxState) GetMappedUIDCount() int {
	if ms.UIDToMessageID == nil {
		return 0
	}
	return len(ms.UIDToMessageID)
}

// GetMissingMappingUIDs returns UIDs that don't have a Message-ID mapping yet
func (ms *MailboxState) GetMissingMappingUIDs(allUIDs []uint32) []uint32 {
	if ms.UIDToMessageID == nil {
		return allUIDs
	}

	var missing []uint32
	for _, uid := range allUIDs {
		if _, ok := ms.UIDToMessageID[uid]; !ok {
			missing = append(missing, uid)
		}
	}
	return missing
}

// GetUIDsWithFlags returns all UIDs that have stored flag state
func (ms *MailboxState) GetUIDsWithFlags() []uint32 {
	if ms.MessageFlags == nil {
		return nil
	}
	uids := make([]uint32, 0, len(ms.MessageFlags))
	for uid := range ms.MessageFlags {
		uids = append(uids, uid)
	}
	return uids
}

// StateManager handles loading and saving sync state
type StateManager struct {
	cacheDir string
}

// NewStateManager creates a new state manager
func NewStateManager() *StateManager {
	// Use XDG cache dir or fallback to ~/.cache/durian
	cacheDir := os.Getenv("XDG_CACHE_HOME")
	if cacheDir == "" {
		home, _ := os.UserHomeDir()
		cacheDir = filepath.Join(home, ".cache")
	}

	return &StateManager{
		cacheDir: filepath.Join(cacheDir, "durian"),
	}
}

// statePath returns the path to the state file for an account
func (sm *StateManager) statePath(email string) string {
	return filepath.Join(sm.cacheDir, fmt.Sprintf("%s-imap-state.json", email))
}

// lockPath returns the path to the lock file for an account
func (sm *StateManager) lockPath(email string) string {
	return sm.statePath(email) + ".lock"
}

// acquireLock acquires an exclusive file lock for the account state.
// Uses non-blocking flock with retry to avoid hanging indefinitely
// when another process (e.g. watcher) holds the lock.
func (sm *StateManager) acquireLock(email string) (*os.File, error) {
	if err := os.MkdirAll(sm.cacheDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create cache dir: %w", err)
	}
	if err := os.Chmod(sm.cacheDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to chmod cache dir: %w", err)
	}

	lockFile, err := os.OpenFile(sm.lockPath(email), os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open lock file: %w", err)
	}

	// Try non-blocking first
	err = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return lockFile, nil
	}

	// Lock is held — retry with backoff (max 30s)
	slog.Debug("Lock busy, waiting", "module", "SYNC", "account", email) // encgrep:allow account email plaintext per ADR-0001 §3
	deadline := time.Now().Add(5 * time.Second)
	delay := 250 * time.Millisecond
	for time.Now().Before(deadline) {
		time.Sleep(delay)
		err = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return lockFile, nil
		}
		if delay < 2*time.Second {
			delay *= 2
		}
	}

	lockFile.Close()
	return nil, fmt.Errorf("sync lock timeout: another sync is running for %s", email)
}

// releaseLock releases the file lock
func releaseLock(lockFile *os.File) {
	if lockFile != nil {
		syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		lockFile.Close()
	}
}

// Load loads the sync state for an account
func (sm *StateManager) Load(email string) (*State, *os.File, error) {
	lockFile, err := sm.acquireLock(email)
	if err != nil {
		return nil, nil, err
	}

	path := sm.statePath(email)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewState(), lockFile, nil
		}
		releaseLock(lockFile)
		return nil, nil, fmt.Errorf("failed to read state file: %w", err)
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		// Corrupted state file — backup and start fresh
		backupPath := fmt.Sprintf("%s.corrupted.%d", path, time.Now().Unix())
		if renameErr := os.Rename(path, backupPath); renameErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: corrupted state file and failed to create backup: %v\n", renameErr)
		} else {
			fmt.Fprintf(os.Stderr, "Warning: corrupted state file backed up to %s, starting fresh\n", backupPath)
		}
		return NewState(), lockFile, nil
	}

	// Rebuild reverse maps for backwards compatibility and consistency
	// This ensures MessageIDToUID is always in sync with UIDToMessageID
	for _, mbox := range state.Mailboxes {
		if mbox.UIDToMessageID != nil {
			if mbox.MessageIDToUID == nil {
				mbox.MessageIDToUID = make(map[string]uint32)
			}
			// Rebuild from UIDToMessageID
			for uid, messageID := range mbox.UIDToMessageID {
				mbox.MessageIDToUID[messageID] = uid
			}
		}
	}

	return &state, lockFile, nil
}

// Save saves the sync state for an account
func (sm *StateManager) Save(email string, state *State) error {
	path := sm.statePath(email)

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	// Atomic write: temp file + rename to prevent corruption on crash
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath) // Clean up on failure
		return fmt.Errorf("failed to rename state file: %w", err)
	}

	return nil
}

// Delete removes the state file for an account
func (sm *StateManager) Delete(email string) error {
	path := sm.statePath(email)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ExpandPath expands ~ in path to home directory
func ExpandPath(path string) string {
	return config.ExpandPath(path)
}
