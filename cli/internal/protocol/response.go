package protocol

import "github.com/julion2/durian/cli/internal/mail"

// ErrorCode represents standardized error codes for client handling
type ErrorCode string

const (
	ErrNone         ErrorCode = ""
	ErrInvalidJSON  ErrorCode = "INVALID_JSON"
	ErrUnknownCmd   ErrorCode = "UNKNOWN_COMMAND"
	ErrNotFound     ErrorCode = "NOT_FOUND"
	ErrParseFailed  ErrorCode = "PARSE_FAILED"
	ErrBackendError ErrorCode = "BACKEND_ERROR"
	ErrFileError    ErrorCode = "FILE_ERROR"
)

// Response represents a JSON response sent to the client
type Response struct {
	OK        bool                `json:"ok"`
	ErrorCode ErrorCode           `json:"error_code,omitempty"`
	Error     string              `json:"error,omitempty"`
	Results   []mail.Mail         `json:"results"`
	Mail      *mail.MailContent   `json:"mail,omitempty"`
	Thread      *mail.ThreadContent            `json:"thread,omitempty"`
	Threads     map[string]*mail.ThreadContent `json:"threads,omitempty"`
	MessageBody *mail.MessageBody              `json:"message_body,omitempty"`
	Tags        []string                       `json:"tags,omitempty"`
}

// Success returns a successful response with no data
func Success() Response {
	return Response{OK: true}
}

// SuccessWithResults returns a successful response with mail results
func SuccessWithResults(results []mail.Mail) Response {
	return Response{OK: true, Results: results}
}

// SuccessWithResultsAndThreads returns a successful response with mail results and enriched thread data
func SuccessWithResultsAndThreads(results []mail.Mail, threads map[string]*mail.ThreadContent) Response {
	return Response{OK: true, Results: results, Threads: threads}
}

// SuccessWithMail returns a successful response with mail content
func SuccessWithMail(m *mail.MailContent) Response {
	return Response{OK: true, Mail: m}
}

// SuccessWithThread returns a successful response with thread content
func SuccessWithThread(t *mail.ThreadContent) Response {
	return Response{OK: true, Thread: t}
}

// SuccessWithMessageBody returns a successful response with a message body
func SuccessWithMessageBody(b *mail.MessageBody) Response {
	return Response{OK: true, MessageBody: b}
}

// SuccessWithTags returns a successful response with a list of tags
func SuccessWithTags(tags []string) Response {
	return Response{OK: true, Tags: tags}
}

// Fail returns a failed response with an error code and message
func Fail(code ErrorCode, err error) Response {
	return Response{
		OK:        false,
		ErrorCode: code,
		Error:     err.Error(),
	}
}

// FailWithMessage returns a failed response with an error code and custom message
func FailWithMessage(code ErrorCode, message string) Response {
	return Response{
		OK:        false,
		ErrorCode: code,
		Error:     message,
	}
}
