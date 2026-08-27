package store

// Message represents an email message stored in the database.
type Message struct {
	ID int64
	// StableID is the immutable provider object id when available. It is the
	// local identity for JMAP messages; MessageID remains RFC 5322 metadata.
	StableID  string
	MessageID string
	ThreadID  string
	InReplyTo string
	Refs      string
	Subject   string
	FromAddr  string
	ToAddrs   string
	CCAddrs   string
	Date      int64
	CreatedAt int64
	BodyText  string
	BodyHTML  string
	Mailbox   string
	Flags     string
	UID       uint32
	Size      int
	// FetchedBody indicates whether the full body has been fetched (vs headers-only).
	FetchedBody bool
	// RemoteRef is the backend's provider handle for the message
	// (IMAP UID as a decimal string, Microsoft Graph message id).
	RemoteRef string
	// SyncedFlags is the last-synced flag baseline as a comma-joined
	// IMAP-style flag string (same format as Flags, e.g. `\Seen,\Flagged`).
	SyncedFlags string
	// SyncedFlagsInitialized distinguishes an explicit empty baseline from a
	// legacy row that has never established one. Non-empty baselines are always
	// initialized regardless of this field.
	SyncedFlagsInitialized bool
	// Account is the account identifier for this message (e.g. "work").
	// Each account has its own rows, uniquely keyed by StableID when present and
	// by MessageID as a fallback for backends without a stable object id.
	Account string
}

// Attachment represents file metadata attached to a message.
type Attachment struct {
	ID          int64
	MessageDBID int64
	PartID      int
	Filename    string
	ContentType string
	Size        int
	Disposition string
	ContentID   string
}

// SearchResult represents a thread-level search result.
// Field names match the handler's SearchResult for API compatibility.
type SearchResult struct {
	Thread       string   `json:"thread"`
	Subject      string   `json:"subject"`
	Authors      string   `json:"authors"`
	Recipients   string   `json:"recipients"`
	DateRelative string   `json:"date_relative"`
	Timestamp    int64    `json:"timestamp"`
	Tags         []string `json:"tags"`
}

// OutboxItem represents a queued message waiting to be sent.
type OutboxItem struct {
	ID        int64
	DraftJSON string
	Attempts  int
	LastError string
	CreatedAt int64
}
