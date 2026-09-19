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
	// BCCAddrs holds the blind recipients of a draft. Unlike ToAddrs/CCAddrs,
	// which ADR-0001 §3 (step 7d revision) keeps in plaintext because those
	// addresses travel on the wire anyway, blind recipients are precisely the
	// ones no other recipient ever sees. The column is `bcc_ct` and is only
	// ever stored encrypted under the meta sub-key — there is no plaintext
	// twin, and it is deliberately absent from the FTS index.
	BCCAddrs  string
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
	// SyntheticIdentity records that Durian generated MessageID because the
	// source message had no Message-ID header. The ID's spelling alone cannot
	// prove that: a sender may legally choose the same string.
	SyntheticIdentity bool
	// SyntheticFingerprint is the parsed-content digest used to recover a
	// synthetic identity even when attachment enrichment is still incomplete.
	// It is encrypted at rest.
	SyntheticFingerprint []byte
	// StartIngestOnConflict transiently asks the upsert to mark an existing row
	// pending and take a new ingest generation in the same transaction. Callers
	// that will rebuild attachments and headers set it before exposing a partially
	// refreshed row to other writers.
	StartIngestOnConflict bool
	// IngestPending records that the core message row is durable but first-ingest
	// enrichment (attachments, indexed headers, tags, and rules) is incomplete.
	IngestPending bool
	// IngestGeneration identifies the writer that currently owns an incomplete
	// enrichment. Only that generation may mark the row complete, so an older
	// overlapping writer cannot expose a newer writer's partial attachment set.
	IngestGeneration int64
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
	ID                int64
	DraftJSON         string
	Attempts          int
	LastError         string
	CreatedAt         int64
	InFlight          bool
	DeliveryConfirmed bool
}
