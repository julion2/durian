package handler

import (
	"testing"

	"github.com/julion2/durian/cli/internal/calendarsync"
)

func TestGUIOperationAllowsOnlyMatchingPlannerActions(t *testing.T) {
	tests := []struct {
		operation string
		kind      calendarsync.ActionKind
		want      bool
	}{
		{"save", calendarsync.ActionUploadCreate, true},
		{"save", calendarsync.ActionUploadUpdate, true},
		{"save", calendarsync.ActionAdopt, true},
		{"save", calendarsync.ActionDownloadUpdate, false},
		{"save", calendarsync.ActionConflict, false},
		{"rsvp", calendarsync.ActionRsvp, true},
		{"rsvp", calendarsync.ActionUploadUpdate, false},
		{"delete", calendarsync.ActionDeleteRemote, true},
		{"delete", calendarsync.ActionDropStatus, true},
		{"delete", calendarsync.ActionDownloadNew, false},
		{"unknown", calendarsync.ActionUploadCreate, false},
	}
	for _, tt := range tests {
		t.Run(tt.operation+"/"+string(tt.kind), func(t *testing.T) {
			if got := guiOperationAllows(tt.operation, calendarsync.Action{Kind: tt.kind}); got != tt.want {
				t.Fatalf("guiOperationAllows(%q, %q) = %v, want %v", tt.operation, tt.kind, got, tt.want)
			}
		})
	}
}
