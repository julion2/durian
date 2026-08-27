package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/mail"
)

func TestOutputThreadFormattedIncludesActionableReferences(t *testing.T) {
	previousConfig := cfg
	cfg = &config.Config{Accounts: []config.AccountConfig{{Name: "Work", Alias: "office"}}}
	t.Cleanup(func() { cfg = previousConfig })

	thread := &mail.ThreadContent{
		ThreadID: "abc123",
		Subject:  "Report",
		Messages: []mail.MessageInfo{{
			MessageID: "report@example.com",
			Account:   "work",
			From:      "sender@example.com",
			Date:      "Today",
			Tags:      []string{"inbox", "unread"},
			Body:      "Attached.",
			Attachments: []mail.AttachmentInfo{{
				PartID:      2,
				Filename:    "report.pdf",
				ContentType: "application/pdf",
				Size:        2048,
			}},
		}},
	}

	var output bytes.Buffer
	if err := outputThreadFormatted(&output, thread); err != nil {
		t.Fatalf("outputThreadFormatted: %v", err)
	}
	for _, want := range []string{
		"Thread: thread:abc123",
		"Message: message:report@example.com",
		"Account: office",
		"Tags: inbox, unread",
		"[2] report.pdf (application/pdf, 2.0 KB)",
		"durian attachment 'message:report@example.com' --account 'office' --save 2",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("output missing %q:\n%s", want, output.String())
		}
	}
}

func TestNormalizeThreadReference(t *testing.T) {
	for _, input := range []string{"abc123", "thread:abc123", "THREAD:abc123"} {
		if got := normalizeThreadReference(input); got != "abc123" {
			t.Errorf("normalizeThreadReference(%q) = %q", input, got)
		}
	}
}
