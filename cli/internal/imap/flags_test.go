package imap

import (
	"testing"

	"github.com/emersion/go-imap"
)

func TestFlagStateFromIMAP(t *testing.T) {
	tests := []struct {
		name     string
		flags    []string
		expected FlagState
	}{
		{
			name:     "empty flags",
			flags:    []string{},
			expected: FlagState{},
		},
		{
			name:     "seen flag",
			flags:    []string{imap.SeenFlag},
			expected: FlagState{Seen: true},
		},
		{
			name:     "all flags",
			flags:    []string{imap.SeenFlag, imap.FlaggedFlag, imap.AnsweredFlag, imap.DeletedFlag},
			expected: FlagState{Seen: true, Flagged: true, Answered: true, Deleted: true},
		},
		{
			name:     "with unknown flags",
			flags:    []string{imap.SeenFlag, "\\Custom", imap.FlaggedFlag},
			expected: FlagState{Seen: true, Flagged: true},
		},
		{
			name:     "completed keyword",
			flags:    []string{imap.SeenFlag, imap.FlaggedFlag, "$Completed"},
			expected: FlagState{Seen: true, Flagged: true, Completed: true},
		},
		{
			name:     "completed without flagged",
			flags:    []string{imap.SeenFlag, "$Completed"},
			expected: FlagState{Seen: true, Completed: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FlagStateFromIMAP(tt.flags)
			if got != tt.expected {
				t.Errorf("FlagStateFromIMAP() = %+v, want %+v", got, tt.expected)
			}
		})
	}
}

func TestFlagStateFromTags(t *testing.T) {
	tests := []struct {
		name     string
		tags     []string
		expected FlagState
	}{
		{
			name:     "no tags - defaults to seen",
			tags:     []string{},
			expected: FlagState{Seen: true},
		},
		{
			name:     "unread tag",
			tags:     []string{"unread"},
			expected: FlagState{Seen: false},
		},
		{
			name:     "flagged tag",
			tags:     []string{"flagged"},
			expected: FlagState{Seen: true, Flagged: true},
		},
		{
			name:     "unread and flagged",
			tags:     []string{"unread", "flagged"},
			expected: FlagState{Seen: false, Flagged: true},
		},
		{
			name:     "all sync tags (deleted not mapped)",
			tags:     []string{"flagged", "replied", "deleted"},
			expected: FlagState{Seen: true, Flagged: true, Answered: true},
		},
		{
			name:     "with non-sync tags",
			tags:     []string{"inbox", "unread", "attachment", "flagged"},
			expected: FlagState{Seen: false, Flagged: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FlagStateFromTags(tt.tags)
			if got != tt.expected {
				t.Errorf("FlagStateFromTags() = %+v, want %+v", got, tt.expected)
			}
		})
	}
}

func TestFlagStateToIMAPFlags(t *testing.T) {
	tests := []struct {
		name     string
		state    FlagState
		expected []string
	}{
		{
			name:     "empty state",
			state:    FlagState{},
			expected: nil,
		},
		{
			name:     "seen only",
			state:    FlagState{Seen: true},
			expected: []string{imap.SeenFlag},
		},
		{
			name:     "all flags",
			state:    FlagState{Seen: true, Flagged: true, Answered: true, Deleted: true},
			expected: []string{imap.SeenFlag, imap.FlaggedFlag, imap.AnsweredFlag, imap.DeletedFlag},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.state.ToIMAPFlags()
			if len(got) != len(tt.expected) {
				t.Errorf("ToIMAPFlags() len = %d, want %d", len(got), len(tt.expected))
				return
			}
			for i, flag := range got {
				if flag != tt.expected[i] {
					t.Errorf("ToIMAPFlags()[%d] = %s, want %s", i, flag, tt.expected[i])
				}
			}
		})
	}
}

func TestFlagStateToTagOps(t *testing.T) {
	tests := []struct {
		name           string
		state          FlagState
		expectedAdd    []string
		expectedRemove []string
	}{
		{
			name:           "unread message",
			state:          FlagState{Seen: false},
			expectedAdd:    []string{"unread"},
			expectedRemove: []string{"flagged", "replied"},
		},
		{
			name:           "read message",
			state:          FlagState{Seen: true},
			expectedAdd:    nil,
			expectedRemove: []string{"unread", "flagged", "replied"},
		},
		{
			name:           "flagged unread message",
			state:          FlagState{Seen: false, Flagged: true},
			expectedAdd:    []string{"unread", "flagged"},
			expectedRemove: []string{"replied"},
		},
		{
			name:           "flagged and completed - no flagged tag",
			state:          FlagState{Seen: true, Flagged: true, Completed: true},
			expectedAdd:    nil,
			expectedRemove: []string{"unread", "flagged", "replied"},
		},
		{
			name:           "flagged without completed - flagged tag set",
			state:          FlagState{Seen: true, Flagged: true},
			expectedAdd:    []string{"flagged"},
			expectedRemove: []string{"unread", "replied"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			add, remove := tt.state.ToTagOps()
			if !slicesEqual(add, tt.expectedAdd) {
				t.Errorf("ToTagOps() add = %v, want %v", add, tt.expectedAdd)
			}
			if !slicesEqual(remove, tt.expectedRemove) {
				t.Errorf("ToTagOps() remove = %v, want %v", remove, tt.expectedRemove)
			}
		})
	}
}

func TestDiffFlags(t *testing.T) {
	tests := []struct {
		name           string
		local          FlagState
		server         FlagState
		expectedAdd    []string
		expectedRemove []string
	}{
		{
			name:           "no difference",
			local:          FlagState{Seen: true},
			server:         FlagState{Seen: true},
			expectedAdd:    nil,
			expectedRemove: nil,
		},
		{
			name:           "local seen, server not",
			local:          FlagState{Seen: true},
			server:         FlagState{},
			expectedAdd:    []string{imap.SeenFlag},
			expectedRemove: nil,
		},
		{
			name:           "server seen, local not",
			local:          FlagState{},
			server:         FlagState{Seen: true},
			expectedAdd:    nil,
			expectedRemove: []string{imap.SeenFlag},
		},
		{
			name:           "multiple differences",
			local:          FlagState{Seen: true, Flagged: true},
			server:         FlagState{Answered: true},
			expectedAdd:    []string{imap.SeenFlag, imap.FlaggedFlag},
			expectedRemove: []string{imap.AnsweredFlag},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			add, remove := DiffFlags(tt.local, tt.server)
			if !slicesEqual(add, tt.expectedAdd) {
				t.Errorf("DiffFlags() add = %v, want %v", add, tt.expectedAdd)
			}
			if !slicesEqual(remove, tt.expectedRemove) {
				t.Errorf("DiffFlags() remove = %v, want %v", remove, tt.expectedRemove)
			}
		})
	}
}

func TestNeedsUpload(t *testing.T) {
	tests := []struct {
		name     string
		local    FlagState
		stored   FlagState
		expected bool
	}{
		{
			name:     "no change",
			local:    FlagState{Seen: true},
			stored:   FlagState{Seen: true},
			expected: false,
		},
		{
			name:     "seen changed",
			local:    FlagState{Seen: true},
			stored:   FlagState{Seen: false},
			expected: true,
		},
		{
			name:     "flagged changed",
			local:    FlagState{Flagged: true},
			stored:   FlagState{Flagged: false},
			expected: true,
		},
		{
			// Deleted is server-owned. This local state is unreachable in
			// production — FlagStateFromTags never sets Deleted, deliberately,
			// because Durian's "deleted" means moved-to-trash while \Deleted
			// means pending expunge. Treating a difference as a local change
			// made every row with a \Deleted baseline a permanent upload
			// candidate.
			name:     "deleted never counts as a local change",
			local:    FlagState{Deleted: true},
			stored:   FlagState{Deleted: false},
			expected: false,
		},
		{
			name:     "completed suppresses flagged upload",
			local:    FlagState{Seen: true, Flagged: false},
			stored:   FlagState{Seen: true, Flagged: true, Completed: true},
			expected: false, // Flagged was removed by Completed download, not a local change
		},
		{
			name:     "flagged change without completed still uploads",
			local:    FlagState{Seen: true, Flagged: false},
			stored:   FlagState{Seen: true, Flagged: true, Completed: false},
			expected: true, // Genuine local un-flag
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NeedsUpload(tt.local, tt.stored)
			if got != tt.expected {
				t.Errorf("NeedsUpload() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestNeedsDownload(t *testing.T) {
	tests := []struct {
		name     string
		server   FlagState
		stored   FlagState
		expected bool
	}{
		{
			name:     "no change",
			server:   FlagState{Seen: true},
			stored:   FlagState{Seen: true},
			expected: false,
		},
		{
			name:     "seen changed",
			server:   FlagState{Seen: true},
			stored:   FlagState{Seen: false},
			expected: true,
		},
		{
			name:     "deleted changed - included",
			server:   FlagState{Deleted: true},
			stored:   FlagState{Deleted: false},
			expected: true, // Deleted changes ARE downloaded
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NeedsDownload(tt.server, tt.stored)
			if got != tt.expected {
				t.Errorf("NeedsDownload() = %v, want %v", got, tt.expected)
			}
		})
	}
}

// Helper function to compare slices
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestResolveFlagsTruthTable walks the full three-way space for one boolean
// flag. Every case has an unambiguous answer: where two sides agree the odd one
// out is the change and it carries, and where all three agree there is nothing
// to do. The rows where local and server both differ from the baseline are the
// ones a two-way merge gets wrong; an OR gets the "both cleared it" row wrong
// on its own.
func TestResolveFlagsTruthTable(t *testing.T) {
	tests := []struct {
		name                    string
		baseline, local, server bool
		want                    bool
	}{
		{"nobody moved, all false", false, false, false, false},
		{"nobody moved, all true", true, true, true, true},
		{"server set it", false, false, true, true},
		{"server cleared it", true, true, false, false},
		{"local set it", false, true, false, true},
		{"local cleared it", true, false, true, false},
		{"both set it", false, true, true, true},
		{"both cleared it", true, false, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The same table drives all three locally-owned flags at once, so a
			// field wired to the wrong source cannot hide behind the others.
			got := ResolveFlags(
				FlagState{Seen: tt.baseline, Flagged: tt.baseline, Answered: tt.baseline},
				FlagState{Seen: tt.local, Flagged: tt.local, Answered: tt.local},
				FlagState{Seen: tt.server, Flagged: tt.server, Answered: tt.server},
			)
			for name, v := range map[string]bool{
				"Seen": got.Seen, "Flagged": got.Flagged, "Answered": got.Answered,
			} {
				if v != tt.want {
					t.Errorf("%s = %v, want %v (baseline=%v local=%v server=%v)",
						name, v, tt.want, tt.baseline, tt.local, tt.server)
				}
			}
		})
	}
}

// TestResolveFlagsIgnoresFlaggedMaskedByCompleted covers the one place a local
// false is produced by Durian rather than by the user: ToTagOps emits "flagged"
// only for Flagged && !Completed, so while the baseline records $Completed the
// tag is suppressed and FlagStateFromTags reports Flagged=false.
//
// Resolving that as a local unstar loses the star twice over — the server's
// still-set flag is not restored when $Completed clears, and the run after
// that, with the mask gone from the baseline, pushes the absence as a real
// removal. NeedsUpload already excludes this case; the resolver has to agree.
func TestResolveFlagsIgnoresFlaggedMaskedByCompleted(t *testing.T) {
	got := ResolveFlags(
		FlagState{Flagged: true, Completed: true},
		FlagState{},              // the mask suppressed the tag
		FlagState{Flagged: true}, // server reopened the follow-up
	)
	if !got.Flagged {
		t.Errorf("got %+v, want Flagged kept: the local absence is the mask, not a user unstar", got)
	}
	if got.Completed {
		t.Errorf("got %+v, want Completed to follow the server's clear", got)
	}

	// Without $Completed in the baseline the same shape IS a user unstar, and
	// must still resolve that way.
	unmasked := ResolveFlags(
		FlagState{Flagged: true},
		FlagState{},
		FlagState{Flagged: true},
	)
	if unmasked.Flagged {
		t.Errorf("got %+v, want Flagged cleared: an unmasked local absence is a real change", unmasked)
	}
}

// TestResolveFlagsKeepsDeletedAndCompletedServerOwned pins the asymmetry.
// FlagStateFromTags can never report either flag, so a local false is the
// absence of a representation rather than the user clearing it. Resolving them
// like the rest would read that absence as a local change and push a removal,
// un-marking a pending expunge the engine had only witnessed.
func TestResolveFlagsKeepsDeletedAndCompletedServerOwned(t *testing.T) {
	kept := ResolveFlags(
		FlagState{Deleted: true, Completed: true},
		FlagState{}, // what tags always imply
		FlagState{Deleted: true, Completed: true},
	)
	if !kept.Deleted || !kept.Completed {
		t.Errorf("got %+v, want Deleted and Completed kept from the server", kept)
	}

	cleared := ResolveFlags(
		FlagState{Deleted: true, Completed: true},
		FlagState{},
		FlagState{},
	)
	if cleared.Deleted || cleared.Completed {
		t.Errorf("got %+v, want both to follow the server's clear", cleared)
	}
}
