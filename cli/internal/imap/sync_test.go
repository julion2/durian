package imap

import (
	"fmt"
	"testing"
)

func TestMatchMailbox(t *testing.T) {
	tests := []struct {
		name     string
		mailbox  string
		pattern  string
		expected bool
	}{
		// Exact matches
		{
			name:     "exact match INBOX",
			mailbox:  "INBOX",
			pattern:  "INBOX",
			expected: true,
		},
		{
			name:     "exact match case insensitive",
			mailbox:  "inbox",
			pattern:  "INBOX",
			expected: true,
		},
		{
			name:     "exact match Sent",
			mailbox:  "Sent",
			pattern:  "Sent",
			expected: true,
		},

		// Prefix matches
		{
			name:     "prefix match Sent Items",
			mailbox:  "Sent Items",
			pattern:  "Sent",
			expected: true,
		},
		{
			name:     "prefix match Sent Messages",
			mailbox:  "Sent Messages",
			pattern:  "Sent",
			expected: true,
		},
		{
			name:     "prefix match case insensitive",
			mailbox:  "SENT ITEMS",
			pattern:  "sent",
			expected: true,
		},
		{
			name:     "prefix match Drafts subfolder",
			mailbox:  "Drafts/Important",
			pattern:  "Drafts",
			expected: true,
		},

		// No match
		{
			name:     "no match different names",
			mailbox:  "Archive",
			pattern:  "Sent",
			expected: false,
		},
		{
			name:     "no match partial in middle",
			mailbox:  "My Sent Folder",
			pattern:  "Sent",
			expected: false,
		},
		{
			name:     "no match suffix",
			mailbox:  "NotSent",
			pattern:  "Sent",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchMailbox(tt.mailbox, tt.pattern)
			if got != tt.expected {
				t.Errorf("matchMailbox(%q, %q) = %v, want %v",
					tt.mailbox, tt.pattern, got, tt.expected)
			}
		})
	}
}

func TestMatchMailbox_WordBoundary(t *testing.T) {
	// Specifically test the word boundary fix
	tests := []struct {
		name     string
		mailbox  string
		pattern  string
		expected bool
	}{
		{
			name:     "SentBackup should not match Sent",
			mailbox:  "SentBackup",
			pattern:  "Sent",
			expected: false,
		},
		{
			name:     "Sent Items should match Sent",
			mailbox:  "Sent Items",
			pattern:  "Sent",
			expected: true,
		},
		{
			name:     "Drafts/sub should match Drafts",
			mailbox:  "Drafts/subfolder",
			pattern:  "Drafts",
			expected: true,
		},
		{
			name:     "DraftsOld should not match Drafts",
			mailbox:  "DraftsOld",
			pattern:  "Drafts",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchMailbox(tt.mailbox, tt.pattern)
			if got != tt.expected {
				t.Errorf("matchMailbox(%q, %q) = %v, want %v",
					tt.mailbox, tt.pattern, got, tt.expected)
			}
		})
	}
}

func TestIsConnectionError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"nil error", nil, false},
		{"connection closed", fmt.Errorf("connection closed"), true},
		{"connection reset", fmt.Errorf("connection reset by peer"), true},
		{"broken pipe", fmt.Errorf("write: broken pipe"), true},
		{"EOF", fmt.Errorf("unexpected EOF"), true},
		{"timeout", fmt.Errorf("i/o timeout"), true},
		{"closed connection", fmt.Errorf("use of closed network connection"), true},
		{"auth error", fmt.Errorf("authentication failed"), false},
		{"generic error", fmt.Errorf("something went wrong"), false},
		{"wrapped connection error", fmt.Errorf("fetch: %w", fmt.Errorf("connection closed")), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isConnectionError(tt.err)
			if got != tt.expected {
				t.Errorf("isConnectionError(%v) = %v, want %v", tt.err, got, tt.expected)
			}
		})
	}
}

func TestExtractMessageIDFromBody(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		expected string
	}{
		{
			name:     "standard Message-ID",
			body:     "From: test@test.com\r\nMessage-ID: <abc123@example.com>\r\nSubject: Test\r\n\r\nBody",
			expected: "abc123@example.com",
		},
		{
			name:     "Message-Id lowercase d",
			body:     "From: test@test.com\r\nMessage-Id: <def456@example.com>\r\nSubject: Test\r\n\r\nBody",
			expected: "def456@example.com",
		},
		{
			name:     "no Message-ID header",
			body:     "From: test@test.com\r\nSubject: Test\r\n\r\nBody",
			expected: "",
		},
		{
			name:     "empty body",
			body:     "",
			expected: "",
		},
		{
			name:     "malformed email",
			body:     "not a valid email",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractMessageIDFromBody([]byte(tt.body))
			if got != tt.expected {
				t.Errorf("extractMessageIDFromBody() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestSyncOptions_Defaults(t *testing.T) {
	// Test nil options handling
	opts := &SyncOptions{}

	if opts.DryRun {
		t.Error("expected DryRun to be false by default")
	}

	if opts.Quiet {
		t.Error("expected Quiet to be false by default")
	}

	if len(opts.Mailboxes) != 0 {
		t.Error("expected Mailboxes to be empty by default")
	}
}

func TestSyncResult_Aggregation(t *testing.T) {
	result := &SyncResult{
		Account: "test@example.com",
	}

	// Add mailbox results
	result.Mailboxes = append(result.Mailboxes, MailboxResult{
		Name:        "INBOX",
		TotalMsgs:   100,
		NewMsgs:     5,
		SkippedMsgs: 1,
	})

	result.Mailboxes = append(result.Mailboxes, MailboxResult{
		Name:        "Sent",
		TotalMsgs:   50,
		NewMsgs:     3,
		SkippedMsgs: 0,
	})

	// Aggregate
	for _, mb := range result.Mailboxes {
		result.TotalNew += mb.NewMsgs
		result.TotalSkipped += mb.SkippedMsgs
	}

	if result.TotalNew != 8 {
		t.Errorf("expected TotalNew 8, got %d", result.TotalNew)
	}

	if result.TotalSkipped != 1 {
		t.Errorf("expected TotalSkipped 1, got %d", result.TotalSkipped)
	}
}

func TestBatchSplitting(t *testing.T) {
	tests := []struct {
		name          string
		totalUIDs     int
		batchSize     int
		expectedBatch int
	}{
		{
			name:          "exact batch",
			totalUIDs:     100,
			batchSize:     100,
			expectedBatch: 1,
		},
		{
			name:          "multiple batches",
			totalUIDs:     250,
			batchSize:     100,
			expectedBatch: 3,
		},
		{
			name:          "single item",
			totalUIDs:     1,
			batchSize:     100,
			expectedBatch: 1,
		},
		{
			name:          "empty",
			totalUIDs:     0,
			batchSize:     100,
			expectedBatch: 0,
		},
		{
			name:          "large batch size",
			totalUIDs:     50,
			batchSize:     5000,
			expectedBatch: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate batch calculation
			var batchCount int
			if tt.totalUIDs > 0 {
				batchCount = (tt.totalUIDs + tt.batchSize - 1) / tt.batchSize
			}

			if batchCount != tt.expectedBatch {
				t.Errorf("expected %d batches, got %d", tt.expectedBatch, batchCount)
			}

			// Verify all items are covered
			totalProcessed := 0
			for i := 0; i < tt.totalUIDs; i += tt.batchSize {
				end := i + tt.batchSize
				if end > tt.totalUIDs {
					end = tt.totalUIDs
				}
				totalProcessed += end - i
			}

			if totalProcessed != tt.totalUIDs {
				t.Errorf("expected to process %d items, processed %d", tt.totalUIDs, totalProcessed)
			}
		})
	}
}

func TestMailboxResult(t *testing.T) {
	result := MailboxResult{
		Name:        "INBOX",
		TotalMsgs:   100,
		NewMsgs:     10,
		SkippedMsgs: 2,
		Error:       nil,
	}

	if result.Name != "INBOX" {
		t.Errorf("expected Name INBOX, got %s", result.Name)
	}

	if result.TotalMsgs != 100 {
		t.Errorf("expected TotalMsgs 100, got %d", result.TotalMsgs)
	}

	if result.NewMsgs != 10 {
		t.Errorf("expected NewMsgs 10, got %d", result.NewMsgs)
	}

	if result.SkippedMsgs != 2 {
		t.Errorf("expected SkippedMsgs 2, got %d", result.SkippedMsgs)
	}

	if result.Error != nil {
		t.Error("expected Error to be nil")
	}
}

func TestHeaderSet_MergesBuiltinAndUser(t *testing.T) {
	cases := []struct {
		name string
		user []string
		want []string
	}{
		{
			name: "no user additions returns builtins",
			user: nil,
			want: []string{"List-Id", "List-Unsubscribe", "Precedence",
				"X-Mailer", "Return-Path", "X-GitHub-Reason",
				"Authentication-Results"},
		},
		{
			name: "user additions are appended",
			user: []string{"X-GitLab-NotificationReason", "X-Spam-Status"},
			want: []string{"List-Id", "List-Unsubscribe", "Precedence",
				"X-Mailer", "Return-Path", "X-GitHub-Reason",
				"Authentication-Results",
				"X-GitLab-NotificationReason", "X-Spam-Status"},
		},
		{
			name: "case-insensitive dedup against builtins",
			user: []string{"list-id", "LIST-UNSUBSCRIBE", "X-Spam-Status"},
			want: []string{"List-Id", "List-Unsubscribe", "Precedence",
				"X-Mailer", "Return-Path", "X-GitHub-Reason",
				"Authentication-Results", "X-Spam-Status"},
		},
		{
			name: "case-insensitive dedup within user list",
			user: []string{"X-Spam-Status", "x-spam-status", "X-SPAM-STATUS"},
			want: []string{"List-Id", "List-Unsubscribe", "Precedence",
				"X-Mailer", "Return-Path", "X-GitHub-Reason",
				"Authentication-Results", "X-Spam-Status"},
		},
		{
			name: "empty + whitespace-only entries dropped",
			user: []string{"", "   ", "X-Spam-Status"},
			want: []string{"List-Id", "List-Unsubscribe", "Precedence",
				"X-Mailer", "Return-Path", "X-GitHub-Reason",
				"Authentication-Results", "X-Spam-Status"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Syncer{options: &SyncOptions{IndexedHeaders: c.user}}
			got := s.headerSet()
			if len(got) != len(c.want) {
				t.Fatalf("len = %d, want %d (got=%v)", len(got), len(c.want), got)
			}
			for i, h := range c.want {
				if got[i] != h {
					t.Errorf("[%d] = %q, want %q", i, got[i], h)
				}
			}
		})
	}
}
