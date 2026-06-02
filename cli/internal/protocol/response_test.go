package protocol

import (
	"errors"
	"testing"

	"github.com/julion2/durian/cli/internal/mail"
)

func TestSuccess(t *testing.T) {
	resp := Success()

	if !resp.OK {
		t.Error("Success() should return OK=true")
	}
	if resp.ErrorCode != "" {
		t.Errorf("Success() should have empty ErrorCode, got %q", resp.ErrorCode)
	}
	if resp.Error != "" {
		t.Errorf("Success() should have empty Error, got %q", resp.Error)
	}
	if resp.Results != nil {
		t.Error("Success() should have nil Results")
	}
	if resp.Mail != nil {
		t.Error("Success() should have nil Mail")
	}
}

func TestSuccessWithResults(t *testing.T) {
	mails := []mail.Mail{
		{
			ThreadID: "thread1",
			Subject:  "Test Subject",
			From:     "sender@example.com",
			Date:     "Today",
			Tags:     "inbox,unread",
		},
		{
			ThreadID: "thread2",
			Subject:  "Another Subject",
			From:     "other@example.com",
			Date:     "Yesterday",
			Tags:     "inbox",
		},
	}

	resp := SuccessWithResults(mails)

	if !resp.OK {
		t.Error("SuccessWithResults() should return OK=true")
	}
	if len(resp.Results) != 2 {
		t.Errorf("SuccessWithResults() should have 2 results, got %d", len(resp.Results))
	}
	if resp.Results[0].ThreadID != "thread1" {
		t.Errorf("First result ThreadID = %q, want %q", resp.Results[0].ThreadID, "thread1")
	}
	if resp.Results[1].Subject != "Another Subject" {
		t.Errorf("Second result Subject = %q, want %q", resp.Results[1].Subject, "Another Subject")
	}
}

func TestSuccessWithResultsEmpty(t *testing.T) {
	resp := SuccessWithResults([]mail.Mail{})

	if !resp.OK {
		t.Error("SuccessWithResults() with empty slice should return OK=true")
	}
	if len(resp.Results) != 0 {
		t.Errorf("SuccessWithResults() with empty slice should have 0 results, got %d", len(resp.Results))
	}
}

func TestSuccessWithMail(t *testing.T) {
	content := &mail.MailContent{
		From:        "sender@example.com",
		To:          "recipient@example.com",
		Subject:     "Test Subject",
		Date:        "Thu, 18 Dec 2025 10:00:00 +0100",
		Body:        "Hello, this is the body.",
		HTML:        "<p>Hello, this is the body.</p>",
		Attachments: []mail.AttachmentInfo{
			{Filename: "file.pdf", ContentType: "application/pdf", Disposition: "attachment"},
			{Filename: "image.png", ContentType: "image/png", Disposition: "attachment"},
		},
	}

	resp := SuccessWithMail(content)

	if !resp.OK {
		t.Error("SuccessWithMail() should return OK=true")
	}
	if resp.Mail == nil {
		t.Fatal("SuccessWithMail() should have non-nil Mail")
	}
	if resp.Mail.From != "sender@example.com" {
		t.Errorf("Mail.From = %q, want %q", resp.Mail.From, "sender@example.com")
	}
	if resp.Mail.Subject != "Test Subject" {
		t.Errorf("Mail.Subject = %q, want %q", resp.Mail.Subject, "Test Subject")
	}
	if len(resp.Mail.Attachments) != 2 {
		t.Errorf("Mail.Attachments should have 2 items, got %d", len(resp.Mail.Attachments))
	}
}

func TestFail(t *testing.T) {
	err := errors.New("something went wrong")
	resp := Fail(ErrBackendError, err)

	if resp.OK {
		t.Error("Fail() should return OK=false")
	}
	if resp.ErrorCode != ErrBackendError {
		t.Errorf("Fail() ErrorCode = %q, want %q", resp.ErrorCode, ErrBackendError)
	}
	if resp.Error != "something went wrong" {
		t.Errorf("Fail() Error = %q, want %q", resp.Error, "something went wrong")
	}
}

func TestFailWithMessage(t *testing.T) {
	resp := FailWithMessage(ErrNotFound, "thread not found")

	if resp.OK {
		t.Error("FailWithMessage() should return OK=false")
	}
	if resp.ErrorCode != ErrNotFound {
		t.Errorf("FailWithMessage() ErrorCode = %q, want %q", resp.ErrorCode, ErrNotFound)
	}
	if resp.Error != "thread not found" {
		t.Errorf("FailWithMessage() Error = %q, want %q", resp.Error, "thread not found")
	}
}

func TestErrorCodes(t *testing.T) {
	// Verify all error codes are distinct
	codes := []ErrorCode{
		ErrNone,
		ErrInvalidJSON,
		ErrUnknownCmd,
		ErrNotFound,
		ErrParseFailed,
		ErrBackendError,
		ErrFileError,
	}

	seen := make(map[ErrorCode]bool)
	for _, code := range codes {
		if code != ErrNone && seen[code] {
			t.Errorf("Duplicate error code: %q", code)
		}
		seen[code] = true
	}

	// Verify expected values
	if ErrInvalidJSON != "INVALID_JSON" {
		t.Errorf("ErrInvalidJSON = %q, want %q", ErrInvalidJSON, "INVALID_JSON")
	}
	if ErrUnknownCmd != "UNKNOWN_COMMAND" {
		t.Errorf("ErrUnknownCmd = %q, want %q", ErrUnknownCmd, "UNKNOWN_COMMAND")
	}
	if ErrNotFound != "NOT_FOUND" {
		t.Errorf("ErrNotFound = %q, want %q", ErrNotFound, "NOT_FOUND")
	}
}
