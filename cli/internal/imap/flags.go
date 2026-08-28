package imap

import (
	"github.com/emersion/go-imap"
)

// FlagState represents the sync-relevant flags for a message
type FlagState struct {
	Seen      bool `json:"seen"`
	Flagged   bool `json:"flagged"`
	Answered  bool `json:"answered"`
	Deleted   bool `json:"deleted"`
	Completed bool `json:"completed"` // Outlook $Completed keyword — marks completed follow-ups
}

// Equal checks if two FlagStates are equal
func (f FlagState) Equal(other FlagState) bool {
	return f.Seen == other.Seen &&
		f.Flagged == other.Flagged &&
		f.Answered == other.Answered &&
		f.Deleted == other.Deleted &&
		f.Completed == other.Completed
}

// IsEmpty checks if all flags are false
func (f FlagState) IsEmpty() bool {
	return !f.Seen && !f.Flagged && !f.Answered && !f.Deleted && !f.Completed
}

// ResolveFlags decides the post-sync state of every flag from all three sides.
//
// Merge below cannot do this: it ORs local and server without consulting the
// baseline, so a flag the user cleared can never win — it looks identical to a
// flag the user never set. The baseline is what tells those apart, and for a
// boolean it settles every field on its own:
//
//	local == baseline   only the server moved, take the server
//	server == baseline  only the local side moved, take local
//	otherwise           both moved, and for a boolean that means both moved to
//	                    !baseline, so they already agree
//
// There is no conflict to arbitrate, which is why this needs no rule about
// which side wins.
//
// Deleted and Completed stay server-owned. FlagStateFromTags can never report
// either one — no tag maps to them — so a local false is the absence of a
// representation rather than a user's decision, and reading it as "the user
// cleared this" is what made the engine un-mark pending expunges it had only
// witnessed.
func ResolveFlags(baseline, local, server FlagState) FlagState {
	// While the baseline records $Completed, ToTagOps deliberately suppresses
	// the "flagged" tag, so the local side reports Flagged=false no matter what
	// the user wants. That absence is the mask's doing, not a decision, and
	// resolving it as a local change loses the star: clearing $Completed on the
	// server would leave the still-flagged message untagged here, and the next
	// run — with the mask no longer active — would upload the absence as a real
	// unstar. Normalize it to the baseline, which is the mask NeedsUpload
	// already applies for the same reason.
	if baseline.Completed {
		local.Flagged = baseline.Flagged
	}

	return FlagState{
		Seen:      resolveFlag(baseline.Seen, local.Seen, server.Seen),
		Flagged:   resolveFlag(baseline.Flagged, local.Flagged, server.Flagged),
		Answered:  resolveFlag(baseline.Answered, local.Answered, server.Answered),
		Deleted:   server.Deleted,
		Completed: server.Completed,
	}
}

func resolveFlag(baseline, local, server bool) bool {
	if local == baseline {
		return server
	}
	return local
}

// Merge combines two FlagStates using OR logic (except Deleted which uses
// server value). Superseded by ResolveFlags on the sync engine's path, but
// still live in the legacy imap.Syncer.
func (f FlagState) Merge(server FlagState) FlagState {
	return FlagState{
		Seen:      f.Seen || server.Seen,
		Flagged:   f.Flagged || server.Flagged,
		Answered:  f.Answered || server.Answered,
		Deleted:   server.Deleted,   // Server wins for deletes
		Completed: server.Completed, // Server wins for completed (server-only concept)
	}
}

// ToIMAPFlags converts FlagState to IMAP flag strings
func (f FlagState) ToIMAPFlags() []string {
	var flags []string
	if f.Seen {
		flags = append(flags, imap.SeenFlag)
	}
	if f.Flagged {
		flags = append(flags, imap.FlaggedFlag)
	}
	if f.Answered {
		flags = append(flags, imap.AnsweredFlag)
	}
	if f.Deleted {
		flags = append(flags, imap.DeletedFlag)
	}
	return flags
}

// FlagStateFromIMAP creates a FlagState from IMAP flags
func FlagStateFromIMAP(flags []string) FlagState {
	state := FlagState{}
	for _, flag := range flags {
		switch flag {
		case imap.SeenFlag:
			state.Seen = true
		case imap.FlaggedFlag:
			state.Flagged = true
		case imap.AnsweredFlag:
			state.Answered = true
		case imap.DeletedFlag:
			state.Deleted = true
		case "$Completed":
			state.Completed = true
		}
	}
	return state
}

// FlagTagVocabulary is the set of tags FlagStateFromTags reads and ToTagOps
// writes. A caller that needs to know whether a flag decision is still valid
// compares only these: a rule adding an unrelated tag mid-sync says nothing
// about the flag state and must not invalidate the decision.
func FlagTagVocabulary() []string {
	return []string{"unread", "flagged", "replied"}
}

// FlagStateFromTags creates a FlagState from local tags.
// Note: uses "unread" tag (inverse of Seen)
// Note: "deleted" tag is NOT mapped to \Deleted IMAP flag.
// \Deleted means "permanently expunge" in IMAP, while "deleted"
// means "moved to trash". Uploading \Deleted would cause servers to purge messages.
func FlagStateFromTags(tags []string) FlagState {
	state := FlagState{
		Seen: true, // Default to seen (no unread tag)
	}

	for _, tag := range tags {
		switch tag {
		case "unread":
			state.Seen = false
		case "flagged":
			state.Flagged = true
		case "replied":
			state.Answered = true
		}
	}

	return state
}

// ToTagOps converts FlagState to tag add/remove operations.
// Returns tags to add and tags to remove.
func (f FlagState) ToTagOps() (add []string, remove []string) {
	if f.Seen {
		remove = append(remove, "unread")
	} else {
		add = append(add, "unread")
	}

	if f.Flagged && !f.Completed {
		add = append(add, "flagged")
	} else {
		remove = append(remove, "flagged")
	}

	if f.Answered {
		add = append(add, "replied")
	} else {
		remove = append(remove, "replied")
	}

	// Note: \Deleted IMAP flag is NOT synced to "deleted" tag.
	// \Deleted means "permanently expunge" in IMAP, durian handles
	// deletes via copy-to-trash + expunge in uploadFlagChanges instead.

	return add, remove
}

// DiffFlags returns the flags that differ between local and server
// Returns: flagsToAdd (to server), flagsToRemove (from server)
func DiffFlags(local, server FlagState) (toAdd, toRemove []string) {
	// Seen
	if local.Seen && !server.Seen {
		toAdd = append(toAdd, imap.SeenFlag)
	} else if !local.Seen && server.Seen {
		toRemove = append(toRemove, imap.SeenFlag)
	}

	// Flagged
	if local.Flagged && !server.Flagged {
		toAdd = append(toAdd, imap.FlaggedFlag)
	} else if !local.Flagged && server.Flagged {
		toRemove = append(toRemove, imap.FlaggedFlag)
	}

	// Answered
	if local.Answered && !server.Answered {
		toAdd = append(toAdd, imap.AnsweredFlag)
	} else if !local.Answered && server.Answered {
		toRemove = append(toRemove, imap.AnsweredFlag)
	}

	// Deleted - sync bidirectionally (server may auto-move to Trash)
	if local.Deleted && !server.Deleted {
		toAdd = append(toAdd, imap.DeletedFlag)
	} else if !local.Deleted && server.Deleted {
		toRemove = append(toRemove, imap.DeletedFlag)
	}

	return toAdd, toRemove
}

// NeedsUpload checks if local flags differ from stored state (needs upload to server)
func NeedsUpload(local, stored FlagState) bool {
	// When server marked a message as Completed, ToTagOps removes the local
	// "flagged" tag. Don't treat that removal as a local change — it's the
	// result of a prior download, not a user action.
	flaggedChanged := local.Flagged != stored.Flagged && !stored.Completed

	return local.Seen != stored.Seen ||
		flaggedChanged ||
		local.Answered != stored.Answered ||
		local.Deleted != stored.Deleted
}

// NeedsDownload checks if server flags differ from stored state (needs download to local)
func NeedsDownload(server, stored FlagState) bool {
	return server.Seen != stored.Seen ||
		server.Flagged != stored.Flagged ||
		server.Answered != stored.Answered ||
		server.Deleted != stored.Deleted ||
		server.Completed != stored.Completed
}
