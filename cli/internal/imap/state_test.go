package imap

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewState(t *testing.T) {
	state := NewState()

	if state.Mailboxes == nil {
		t.Error("expected Mailboxes to be initialized")
	}

	if len(state.Mailboxes) != 0 {
		t.Errorf("expected empty Mailboxes, got %d", len(state.Mailboxes))
	}
}

func TestState_GetMailboxState(t *testing.T) {
	state := NewState()

	// Get state for new mailbox
	ms := state.GetMailboxState("INBOX")

	if ms == nil {
		t.Fatal("expected non-nil MailboxState")
	}

	if ms.UIDValidity != 0 {
		t.Errorf("expected UIDValidity 0, got %d", ms.UIDValidity)
	}

	if ms.LastUID != 0 {
		t.Errorf("expected LastUID 0, got %d", ms.LastUID)
	}

	if ms.SyncedUIDs == nil {
		t.Error("expected SyncedUIDs to be initialized")
	}

	// Get same mailbox again - should return same state
	ms2 := state.GetMailboxState("INBOX")
	ms2.UIDValidity = 123

	if state.GetMailboxState("INBOX").UIDValidity != 123 {
		t.Error("expected same MailboxState instance")
	}

	// Get different mailbox
	msSent := state.GetMailboxState("Sent")
	if msSent.UIDValidity != 0 {
		t.Error("expected new mailbox to have UIDValidity 0")
	}
}

func TestMailboxState_IsUIDSynced(t *testing.T) {
	ms := &MailboxState{
		SyncedUIDs: []uint32{100, 200, 300},
	}

	tests := []struct {
		uid      uint32
		expected bool
	}{
		{100, true},
		{200, true},
		{300, true},
		{150, false},
		{0, false},
		{400, false},
	}

	for _, tt := range tests {
		got := ms.IsUIDSynced(tt.uid)
		if got != tt.expected {
			t.Errorf("IsUIDSynced(%d) = %v, want %v", tt.uid, got, tt.expected)
		}
	}
}

func TestMailboxState_AddSyncedUID(t *testing.T) {
	ms := &MailboxState{
		SyncedUIDs: make([]uint32, 0),
	}

	// Add first UID
	ms.AddSyncedUID(100)
	if !ms.IsUIDSynced(100) {
		t.Error("expected UID 100 to be synced")
	}
	if ms.LastUID != 100 {
		t.Errorf("expected LastUID 100, got %d", ms.LastUID)
	}

	// Add higher UID
	ms.AddSyncedUID(200)
	if ms.LastUID != 200 {
		t.Errorf("expected LastUID 200, got %d", ms.LastUID)
	}

	// Add lower UID - LastUID should not change
	ms.AddSyncedUID(50)
	if ms.LastUID != 200 {
		t.Errorf("expected LastUID still 200, got %d", ms.LastUID)
	}

	// Add duplicate - should not add again
	lenBefore := len(ms.SyncedUIDs)
	ms.AddSyncedUID(100)
	if len(ms.SyncedUIDs) != lenBefore {
		t.Error("expected duplicate UID to not be added")
	}
}

func TestMailboxState_GetUnsyncedUIDs(t *testing.T) {
	ms := &MailboxState{
		SyncedUIDs: []uint32{100, 200, 300},
	}

	tests := []struct {
		name     string
		allUIDs  []uint32
		expected []uint32
	}{
		{
			name:     "all synced",
			allUIDs:  []uint32{100, 200, 300},
			expected: nil,
		},
		{
			name:     "none synced",
			allUIDs:  []uint32{400, 500, 600},
			expected: []uint32{400, 500, 600},
		},
		{
			name:     "mixed",
			allUIDs:  []uint32{100, 150, 200, 250, 300, 350},
			expected: []uint32{150, 250, 350},
		},
		{
			name:     "empty all",
			allUIDs:  []uint32{},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ms.GetUnsyncedUIDs(tt.allUIDs)

			if len(got) != len(tt.expected) {
				t.Errorf("got %v, want %v", got, tt.expected)
				return
			}

			for i, uid := range got {
				if uid != tt.expected[i] {
					t.Errorf("got[%d] = %d, want %d", i, uid, tt.expected[i])
				}
			}
		})
	}
}

func TestMailboxReplacementCompleteRequiresEverySearchedUID(t *testing.T) {
	state := NewState().GetMailboxState("INBOX")
	state.Reset(2)
	state.AddSyncedUID(3)
	if mailboxReplacementComplete(state, []uint32{1, 2, 3}) {
		t.Fatal("partial replacement reported complete")
	}
	state.AddSyncedUID(1)
	state.AddSyncedUID(2)
	if !mailboxReplacementComplete(state, []uint32{1, 2, 3}) {
		t.Fatal("fully resolved replacement reported incomplete")
	}
}

func TestMailboxState_NeedsFullResync(t *testing.T) {
	tests := []struct {
		name           string
		currentUID     uint32
		newUID         uint32
		expectedResync bool
	}{
		{
			name:           "first sync (UIDValidity 0)",
			currentUID:     0,
			newUID:         12345,
			expectedResync: false,
		},
		{
			name:           "same UIDValidity",
			currentUID:     12345,
			newUID:         12345,
			expectedResync: false,
		},
		{
			name:           "different UIDValidity",
			currentUID:     12345,
			newUID:         67890,
			expectedResync: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := &MailboxState{UIDValidity: tt.currentUID}
			got := ms.NeedsFullResync(tt.newUID)
			if got != tt.expectedResync {
				t.Errorf("NeedsFullResync(%d) = %v, want %v", tt.newUID, got, tt.expectedResync)
			}
		})
	}
}

func TestMailboxState_Reset(t *testing.T) {
	ms := &MailboxState{
		UIDValidity: 12345,
		LastUID:     500,
		SyncedUIDs:  []uint32{100, 200, 300, 400, 500},
	}
	if !ms.IsUIDSynced(100) {
		t.Fatal("expected UID 100 to be synced before reset")
	}

	ms.Reset(67890)

	if ms.UIDValidity != 67890 {
		t.Errorf("expected UIDValidity 67890, got %d", ms.UIDValidity)
	}

	if ms.LastUID != 0 {
		t.Errorf("expected LastUID 0, got %d", ms.LastUID)
	}

	if len(ms.SyncedUIDs) != 0 {
		t.Errorf("expected empty SyncedUIDs, got %v", ms.SyncedUIDs)
	}
	if ms.IsUIDSynced(100) {
		t.Error("expected transient synced UID index to be cleared")
	}
}

func TestStateManager_LoadSave(t *testing.T) {
	// Create temp directory for testing
	tmpDir, err := os.MkdirTemp("", "durian-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create state manager with custom cache dir
	sm := &StateManager{cacheDir: tmpDir}

	email := "test@example.com"

	// Load non-existent state - should return new empty state
	state, lock, err := sm.Load(email)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	defer releaseLock(lock)

	if state == nil {
		t.Fatal("expected non-nil state")
	}

	if len(state.Mailboxes) != 0 {
		t.Errorf("expected empty state, got %d mailboxes", len(state.Mailboxes))
	}

	// Modify and save state
	ms := state.GetMailboxState("INBOX")
	ms.UIDValidity = 12345
	ms.AddSyncedUID(100)
	ms.AddSyncedUID(200)

	msSent := state.GetMailboxState("Sent")
	msSent.UIDValidity = 67890
	msSent.AddSyncedUID(50)

	if err := sm.Save(email, state); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Release lock before re-loading
	releaseLock(lock)

	// Verify file exists
	statePath := filepath.Join(tmpDir, email+"-imap-state.json")
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		t.Error("expected state file to exist")
	}

	// Load again and verify
	loaded, lock2, err := sm.Load(email)
	if err != nil {
		t.Fatalf("Load after save failed: %v", err)
	}
	defer releaseLock(lock2)

	inboxState := loaded.GetMailboxState("INBOX")
	if inboxState.UIDValidity != 12345 {
		t.Errorf("expected INBOX UIDValidity 12345, got %d", inboxState.UIDValidity)
	}

	if inboxState.LastUID != 200 {
		t.Errorf("expected INBOX LastUID 200, got %d", inboxState.LastUID)
	}

	if len(inboxState.SyncedUIDs) != 2 {
		t.Errorf("expected 2 synced UIDs, got %d", len(inboxState.SyncedUIDs))
	}

	sentState := loaded.GetMailboxState("Sent")
	if sentState.UIDValidity != 67890 {
		t.Errorf("expected Sent UIDValidity 67890, got %d", sentState.UIDValidity)
	}
}

func TestStateManagerLoadReadOnlyDoesNotCreateFiles(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "missing-cache")
	sm := &StateManager{cacheDir: cacheDir}

	state, err := sm.LoadReadOnly("test@example.com")
	if err != nil {
		t.Fatalf("LoadReadOnly: %v", err)
	}
	if state == nil || len(state.Mailboxes) != 0 {
		t.Fatalf("state = %+v, want a new empty state", state)
	}
	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Fatalf("read-only load created cache directory: %v", err)
	}
}

func TestMailboxState_GetDeletedUIDs(t *testing.T) {
	tests := []struct {
		name       string
		synced     []uint32
		serverUIDs []uint32
		expected   []uint32
	}{
		{
			name:       "no deletions",
			synced:     []uint32{100, 200, 300},
			serverUIDs: []uint32{100, 200, 300},
			expected:   nil,
		},
		{
			name:       "all deleted",
			synced:     []uint32{100, 200, 300},
			serverUIDs: []uint32{},
			expected:   []uint32{100, 200, 300},
		},
		{
			name:       "partial deletion",
			synced:     []uint32{100, 200, 300},
			serverUIDs: []uint32{200},
			expected:   []uint32{100, 300},
		},
		{
			name:       "empty synced",
			synced:     []uint32{},
			serverUIDs: []uint32{100, 200},
			expected:   nil,
		},
		{
			name:       "server has extra UIDs",
			synced:     []uint32{100, 200},
			serverUIDs: []uint32{100, 200, 300, 400},
			expected:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := &MailboxState{SyncedUIDs: tt.synced}
			got := ms.GetDeletedUIDs(tt.serverUIDs)
			if len(got) != len(tt.expected) {
				t.Errorf("GetDeletedUIDs() = %v, want %v", got, tt.expected)
				return
			}
			for i, uid := range got {
				if uid != tt.expected[i] {
					t.Errorf("got[%d] = %d, want %d", i, uid, tt.expected[i])
				}
			}
		})
	}
}

func TestMailboxState_RemoveSyncedUID(t *testing.T) {
	t.Run("removes from all maps", func(t *testing.T) {
		ms := &MailboxState{
			SyncedUIDs:     []uint32{100, 200, 300},
			MessageFlags:   map[uint32]FlagState{100: {Seen: true}, 200: {Flagged: true}, 300: {}},
			UIDToMessageID: map[uint32]string{100: "msg100@test", 200: "msg200@test", 300: "msg300@test"},
			MessageIDToUID: map[string]uint32{"msg100@test": 100, "msg200@test": 200, "msg300@test": 300},
		}

		ms.RemoveSyncedUID(200)

		if ms.IsUIDSynced(200) {
			t.Error("UID 200 should no longer be synced")
		}
		if len(ms.SyncedUIDs) != 2 {
			t.Errorf("expected 2 SyncedUIDs, got %d", len(ms.SyncedUIDs))
		}
		if _, ok := ms.MessageFlags[200]; ok {
			t.Error("MessageFlags for UID 200 should be removed")
		}
		if _, ok := ms.UIDToMessageID[200]; ok {
			t.Error("UIDToMessageID for UID 200 should be removed")
		}
		if _, ok := ms.MessageIDToUID["msg200@test"]; ok {
			t.Error("MessageIDToUID for msg200@test should be removed")
		}

		// Other UIDs untouched
		if !ms.IsUIDSynced(100) || !ms.IsUIDSynced(300) {
			t.Error("other UIDs should remain synced")
		}
	})

	t.Run("remove non-existent UID", func(t *testing.T) {
		ms := &MailboxState{
			SyncedUIDs:     []uint32{100},
			MessageFlags:   make(map[uint32]FlagState),
			UIDToMessageID: make(map[uint32]string),
			MessageIDToUID: make(map[string]uint32),
		}

		// Should not panic
		ms.RemoveSyncedUID(999)
		if len(ms.SyncedUIDs) != 1 {
			t.Errorf("expected 1 SyncedUID, got %d", len(ms.SyncedUIDs))
		}
	})
}

func TestMailboxState_MessageIDMapping(t *testing.T) {
	ms := &MailboxState{}

	// Get from nil maps
	_, ok := ms.GetMessageID(100)
	if ok {
		t.Error("expected false from nil map")
	}
	_, ok = ms.GetUIDByMessageID("msg@test")
	if ok {
		t.Error("expected false from nil map")
	}

	// Set creates maps and stores bidirectionally
	ms.SetMessageID(100, "msg100@test")
	ms.SetMessageID(200, "msg200@test")

	id, ok := ms.GetMessageID(100)
	if !ok || id != "msg100@test" {
		t.Errorf("GetMessageID(100) = %q, %v; want %q, true", id, ok, "msg100@test")
	}

	uid, ok := ms.GetUIDByMessageID("msg200@test")
	if !ok || uid != 200 {
		t.Errorf("GetUIDByMessageID(msg200@test) = %d, %v; want 200, true", uid, ok)
	}

	// Overwrite
	ms.SetMessageID(100, "new@test")
	id, _ = ms.GetMessageID(100)
	if id != "new@test" {
		t.Errorf("expected overwritten ID, got %q", id)
	}

	// GetMappedUIDCount
	if ms.GetMappedUIDCount() != 2 {
		t.Errorf("expected 2 mapped UIDs, got %d", ms.GetMappedUIDCount())
	}
}

func TestMailboxState_GetMissingMappingUIDs(t *testing.T) {
	tests := []struct {
		name     string
		mapped   map[uint32]string
		allUIDs  []uint32
		expected []uint32
	}{
		{
			name:     "nil map returns all",
			mapped:   nil,
			allUIDs:  []uint32{100, 200, 300},
			expected: []uint32{100, 200, 300},
		},
		{
			name:     "all mapped",
			mapped:   map[uint32]string{100: "a", 200: "b", 300: "c"},
			allUIDs:  []uint32{100, 200, 300},
			expected: nil,
		},
		{
			name:     "partial",
			mapped:   map[uint32]string{100: "a"},
			allUIDs:  []uint32{100, 200, 300},
			expected: []uint32{200, 300},
		},
		{
			name:     "empty input",
			mapped:   map[uint32]string{100: "a"},
			allUIDs:  []uint32{},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := &MailboxState{UIDToMessageID: tt.mapped}
			got := ms.GetMissingMappingUIDs(tt.allUIDs)
			if len(got) != len(tt.expected) {
				t.Errorf("got %v, want %v", got, tt.expected)
				return
			}
			for i, uid := range got {
				if uid != tt.expected[i] {
					t.Errorf("got[%d] = %d, want %d", i, uid, tt.expected[i])
				}
			}
		})
	}
}

func TestMailboxState_MessageFlags(t *testing.T) {
	ms := &MailboxState{}

	// Get from nil map
	_, ok := ms.GetMessageFlags(100)
	if ok {
		t.Error("expected false from nil map")
	}

	// Set creates map
	flags := FlagState{Seen: true, Flagged: true}
	ms.SetMessageFlags(100, flags)

	got, ok := ms.GetMessageFlags(100)
	if !ok {
		t.Error("expected flag state to exist")
	}
	if !got.Equal(flags) {
		t.Errorf("got %+v, want %+v", got, flags)
	}

	// GetUIDsWithFlags
	ms.SetMessageFlags(200, FlagState{Seen: true})
	uids := ms.GetUIDsWithFlags()
	if len(uids) != 2 {
		t.Errorf("expected 2 UIDs with flags, got %d", len(uids))
	}
}

func TestStateManager_LoadCorruptedFile(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "durian-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	sm := &StateManager{cacheDir: tmpDir}
	email := "corrupt@example.com"

	// Write invalid JSON to the state file
	statePath := filepath.Join(tmpDir, email+"-imap-state.json")
	if err := os.WriteFile(statePath, []byte("{this is not valid json!!!"), 0600); err != nil {
		t.Fatalf("failed to write corrupted file: %v", err)
	}

	// Load should succeed with fresh state (not return error)
	state, lock, err := sm.Load(email)
	if err != nil {
		t.Fatalf("Load should recover from corruption, got error: %v", err)
	}
	defer releaseLock(lock)

	if state == nil {
		t.Fatal("expected non-nil state")
	}
	if len(state.Mailboxes) != 0 {
		t.Errorf("expected fresh empty state, got %d mailboxes", len(state.Mailboxes))
	}

	// Original file should be gone (renamed to backup)
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Error("expected original corrupted file to be renamed")
	}

	// Backup file should exist
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("failed to read dir: %v", err)
	}
	foundBackup := false
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".lock" && entry.Name() != email+"-imap-state.json" {
			foundBackup = true
			// Verify backup has the corrupted content
			backupData, err := os.ReadFile(filepath.Join(tmpDir, entry.Name()))
			if err != nil {
				t.Fatalf("failed to read backup: %v", err)
			}
			if string(backupData) != "{this is not valid json!!!" {
				t.Errorf("backup content mismatch: %q", string(backupData))
			}
		}
	}
	if !foundBackup {
		t.Error("expected backup file to exist")
	}
}

func TestMailboxState_EnsureSyncedSet(t *testing.T) {
	// Simulates what happens after JSON deserialization: slice populated, set is nil
	ms := &MailboxState{
		SyncedUIDs: []uint32{100, 200, 300},
	}

	// syncedSet should be nil before first access
	if ms.syncedSet != nil {
		t.Error("expected nil syncedSet before first access")
	}

	// First lookup triggers lazy init
	if !ms.IsUIDSynced(200) {
		t.Error("expected UID 200 to be synced after lazy init")
	}

	// Set should now be populated
	if ms.syncedSet == nil {
		t.Error("expected syncedSet to be initialized")
	}
	if len(ms.syncedSet) != 3 {
		t.Errorf("expected 3 entries in syncedSet, got %d", len(ms.syncedSet))
	}

	// Add a new UID — should update both slice and set
	ms.AddSyncedUID(400)
	if !ms.IsUIDSynced(400) {
		t.Error("expected UID 400 to be synced")
	}
	if len(ms.SyncedUIDs) != 4 {
		t.Errorf("expected 4 SyncedUIDs, got %d", len(ms.SyncedUIDs))
	}
}

func TestStateManager_SaveAtomicAndPermissions(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "durian-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	sm := &StateManager{cacheDir: tmpDir}
	email := "test@example.com"

	state := NewState()
	state.GetMailboxState("INBOX").UIDValidity = 123

	if err := sm.Save(email, state); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Verify no temp file left behind
	tmpPath := filepath.Join(tmpDir, email+"-imap-state.json.tmp")
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Error("temp file should not exist after successful save")
	}

	// Verify permissions are 0600
	statePath := filepath.Join(tmpDir, email+"-imap-state.json")
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != 0600 {
		t.Errorf("expected permissions 0600, got %04o", perm)
	}
}

func TestStateManager_Delete(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "durian-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	sm := &StateManager{cacheDir: tmpDir}
	email := "test@example.com"

	// Create and save state
	state := NewState()
	state.GetMailboxState("INBOX").UIDValidity = 123
	if err := sm.Save(email, state); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Delete
	if err := sm.Delete(email); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Verify file is gone
	statePath := filepath.Join(tmpDir, email+"-imap-state.json")
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Error("expected state file to be deleted")
	}

	// Delete non-existent - should not error
	if err := sm.Delete("nonexistent@example.com"); err != nil {
		t.Errorf("Delete non-existent should not error: %v", err)
	}
}
