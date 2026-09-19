package jmapbackend

import (
	"encoding/json"

	"github.com/julion2/durian/cli/internal/backend"
)

type jmapCursor struct {
	AccountScope  string   `json:"accountScope,omitempty"`  // Authenticated provider account that issued this cursor.
	Snapshot      string   `json:"snapshot,omitempty"`      // Email state from before an in-progress snapshot.
	SnapshotSet   bool     `json:"snapshotSet,omitempty"`   // Snapshot is present, including a valid empty state.
	PendingIDs    []string `json:"pendingIds,omitempty"`    // Remaining initial-sync IDs (legacy cursor shape).
	EmailState    string   `json:"emailState,omitempty"`    // Last fully applied Email state token.
	EmailStateSet bool     `json:"emailStateSet,omitempty"` // EmailState is present, including a valid empty state.
	Replacement   bool     `json:"replacement,omitempty"`   // Authoritative state-expiry recovery is in progress.
	QueryState    string   `json:"queryState,omitempty"`    // Email/query state captured on its first page.
	QueryStateSet bool     `json:"queryStateSet,omitempty"` // QueryState is present, including a valid empty state.
	QueryAnchor   string   `json:"queryAnchor,omitempty"`   // Last ID from the preceding query page.
	QuerySeen     int      `json:"querySeen,omitempty"`     // Number of IDs emitted by preceding pages.
	QueryTotal    int      `json:"queryTotal,omitempty"`    // Total reported by the first query page.
}

func decodeCursor(cursor backend.Cursor) jmapCursor {
	var state jmapCursor
	if len(cursor) != 0 {
		_ = json.Unmarshal(cursor, &state)
	}
	// Preserve cursors written before explicit presence bits were added.
	state.SnapshotSet = state.SnapshotSet || state.Snapshot != ""
	state.EmailStateSet = state.EmailStateSet || state.EmailState != ""
	state.QueryStateSet = state.QueryStateSet || state.QueryState != ""
	return state
}

func encodeCursor(cursor jmapCursor) backend.Cursor {
	encoded, _ := json.Marshal(cursor)
	return encoded
}

// LegacyIdentityMigration recognizes cursors written before JMAP Email IDs
// were bound to the authenticated account, but only after the operator attests
// that the endpoint and credential still identify the same provider account.
// A non-empty scope is never rewritten because it proves either a current
// cursor or an account retarget.
func (b *Backend) LegacyIdentityMigration(cursor backend.Cursor) (backend.Cursor, string, bool) {
	if len(cursor) == 0 || !b.account.JMAP.TrustLegacyIdentity {
		return nil, "", false
	}
	var state jmapCursor
	if err := json.Unmarshal(cursor, &state); err != nil || state.AccountScope != "" {
		return nil, "", false
	}
	// Old releases only persisted either a completed non-empty Email state, an
	// initial snapshot with remaining IDs, or a replacement snapshot. Reject
	// syntactically valid but unknown/corrupt JSON rather than treating it as
	// proof that every unscoped local identity belongs to this provider account.
	validLegacy := state.EmailState != "" ||
		(state.Snapshot != "" && (len(state.PendingIDs) > 0 || state.Replacement))
	if !validLegacy {
		return nil, "", false
	}
	// Do not trust the old state token under the newly discovered scope. A
	// replacement proves which migrated IDs still exist before queued local
	// intent can be uploaded.
	return encodeCursor(jmapCursor{AccountScope: b.client.accountScope}), b.client.accountScope + ":", true
}
