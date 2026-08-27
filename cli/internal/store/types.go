package store

// Message represents an email message stored in the database.
type Message struct {
	ID        int64
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
	// Account is the account identifier for this message (e.g. "work").
	// Each account has its own row — UNIQUE(message_id, account).
	Account string
	// Accounts contains every account that owns this Message-ID when a thread
	// query deduplicates otherwise-identical rows. It is not persisted.
	Accounts []string
	// AccountRowIDs contains every store row merged by GetByThread. It lets
	// callers batch account-specific metadata without selecting it per message.
	AccountRowIDs []int64
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
