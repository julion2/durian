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

// TestOutputThreadFormattedEscapesRemoteControlSequences covers the fields the
// human renderer used to print raw. A Message-ID, a provider-mirrored tag and
// an attachment filename are all sender-controlled, so an OSC sequence or a
// bidi override in any of them reaches the terminal and rewrites the lines
// around it — including the copy-paste command this view offers.
//
// shellQuote is not a substitute: it protects the shell from metacharacters,
// not the terminal from escapes, and would quote an intact sequence into a
// command the user is invited to run.
func TestOutputThreadFormattedEscapesRemoteControlSequences(t *testing.T) {
	previousConfig := cfg
	cfg = &config.Config{Accounts: []config.AccountConfig{{Name: "Work", Alias: "office"}}}
	t.Cleanup(func() { cfg = previousConfig })

	const (
		osc  = "\x1b]0;pwned\x07"
		bidi = "‮"
	)
	thread := &mail.ThreadContent{
		ThreadID: "abc123",
		Subject:  "Report",
		Messages: []mail.MessageInfo{{
			MessageID: "evil" + osc + bidi + "@example.com",
			Account:   "work",
			From:      "sender@example.com",
			Date:      "Today",
			Tags:      []string{"inbox", "label" + osc},
			Body:      "Attached.",
			Attachments: []mail.AttachmentInfo{{
				PartID:      2,
				Filename:    "report" + bidi + ".pdf",
				ContentType: "application/pdf",
				Size:        2048,
			}},
		}},
	}

	var output bytes.Buffer
	if err := outputThreadFormatted(&output, thread); err != nil {
		t.Fatalf("outputThreadFormatted: %v", err)
	}
	got := output.String()

	if strings.Contains(got, "\x1b") {
		t.Errorf("an escape character reached the terminal:\n%q", got)
	}
	if strings.Contains(got, bidi) {
		t.Errorf("a bidi override reached the terminal:\n%q", got)
	}
	// Escaped, not dropped: the identifier still has to be recognisable, and
	// the marker is what tells the user something was there.
	if !strings.Contains(got, "U+001B") {
		t.Errorf("the escape was removed rather than shown:\n%s", got)
	}
	if !strings.Contains(got, "U+202E") {
		t.Errorf("the bidi override was removed rather than shown:\n%s", got)
	}
}
