package mail

// AttachmentInfo represents metadata about a single email attachment
type AttachmentInfo struct {
	PartID      int    `json:"part_id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	Disposition string `json:"disposition"`
	ContentID   string `json:"content_id,omitempty"`
}

// Mail represents a mail summary for list views
type Mail struct {
	ThreadID  string `json:"thread_id"`
	File      string `json:"file"`
	Subject   string `json:"subject"`
	From      string `json:"from"`
	To        string `json:"to,omitempty"`
	Date      string `json:"date"`
	Timestamp int64  `json:"timestamp"`
	Tags      string `json:"tags"`
}

// MailContent represents the full content of an email
type MailContent struct {
	From        string           `json:"from"`
	To          string           `json:"to"`
	CC          string           `json:"cc,omitempty"`
	Subject     string           `json:"subject"`
	Date        string           `json:"date"`
	MessageID   string           `json:"message_id,omitempty"`
	InReplyTo   string           `json:"in_reply_to,omitempty"`
	References  string           `json:"references,omitempty"`
	Body        string           `json:"body"`
	HTML        string           `json:"html,omitempty"`
	Attachments []AttachmentInfo `json:"attachments,omitempty"`
}

// ThreadContent represents a complete email thread with all messages
type ThreadContent struct {
	ThreadID string        `json:"thread_id"`
	Subject  string        `json:"subject"`
	Messages []MessageInfo `json:"messages"`
}

// MessageBody represents the full (unstripped) body of a single message, used for reply quoting
type MessageBody struct {
	Body string `json:"body"`
	HTML string `json:"html,omitempty"`
}

// MessageInfo represents a single message within a thread
type MessageInfo struct {
	ID              string           `json:"id"`
	From            string           `json:"from"`
	To              string           `json:"to,omitempty"`
	CC              string           `json:"cc,omitempty"`
	Date            string           `json:"date"`
	Timestamp       int64            `json:"timestamp"`
	MessageID       string           `json:"message_id,omitempty"`
	InReplyTo       string           `json:"in_reply_to,omitempty"`
	References      string           `json:"references,omitempty"`
	Body            string           `json:"body"`
	HTML            string           `json:"html,omitempty"`
	HiddenSignature string           `json:"hidden_signature,omitempty"`
	Attachments     []AttachmentInfo `json:"attachments,omitempty"`
	Tags            []string         `json:"tags,omitempty"`
}
