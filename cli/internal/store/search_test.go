package store

import (
	"testing"
	"time"
)

func seedSearchDB(t *testing.T) *DB {
	t.Helper()
	db := newTestDB(t)
	now := time.Now().Unix()

	messages := []*Message{
		{MessageID: "s1@x", Subject: "Invoice for January", FromAddr: "alice@example.com",
			ToAddrs: "bob@example.com", Date: now - 3600, CreatedAt: now, BodyText: "Please find the invoice attached.", Mailbox: "INBOX", FetchedBody: true},
		{MessageID: "s2@x", Subject: "Meeting tomorrow", FromAddr: "bob@example.com",
			ToAddrs: "alice@example.com", Date: now - 1800, CreatedAt: now, BodyText: "Let's discuss the project plan.", Mailbox: "INBOX", FetchedBody: true},
		{MessageID: "s3@x", Subject: "Re: Meeting tomorrow", FromAddr: "alice@example.com",
			ToAddrs: "bob@example.com", InReplyTo: "<s2@x>", Refs: "<s2@x>",
			Date: now - 900, CreatedAt: now, BodyText: "Sounds good, see you then.", Mailbox: "INBOX", FetchedBody: true},
		{MessageID: "s4@x", Subject: "Weekly report", FromAddr: "charlie@example.com",
			ToAddrs: "team@example.com", Date: now - 600, CreatedAt: now, BodyText: "Attached is the weekly report with invoice details.", Mailbox: "INBOX", FetchedBody: true},
		{MessageID: "s5@x", Subject: "Vacation plans", FromAddr: "alice@example.com",
			ToAddrs: "family@example.com", Date: now - 300, CreatedAt: now, BodyText: "I'm thinking about Hawaii.", Mailbox: "Sent", FetchedBody: true},
	}

	for _, msg := range messages {
		if err := db.InsertMessage(msg); err != nil {
			t.Fatalf("seed %s: %v", msg.MessageID, err)
		}
	}

	// Add some tags
	for _, msg := range messages {
		m, _ := db.GetByMessageID(msg.MessageID)
		db.AddTag(m.ID, "inbox")
	}
	m1, _ := db.GetByMessageID("s1@x")
	db.AddTag(m1.ID, "unread")

	return db
}

func TestSearch_All(t *testing.T) {
	db := seedSearchDB(t)
	results, err := db.Search("*", 50)
	if err != nil {
		t.Fatalf("search *: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected results for *")
	}
}

func TestSearch_FromField(t *testing.T) {
	db := seedSearchDB(t)
	results, err := db.Search("from:alice", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	// alice sent s1, s3, s5 — s3 is in same thread as s2
	// Thread grouping: s1 thread, s2+s3 thread, s5 thread → alice appears in 3 threads
	if len(results) < 2 {
		t.Errorf("got %d results for from:alice, want at least 2", len(results))
	}
}

func TestSearch_Tag(t *testing.T) {
	db := seedSearchDB(t)
	results, err := db.Search("tag:unread", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("got %d results for tag:unread, want 1", len(results))
	}
}

func TestSearch_NotTag(t *testing.T) {
	db := seedSearchDB(t)
	all, _ := db.Search("*", 50)
	withoutUnread, _ := db.Search("NOT tag:unread", 50)

	if len(withoutUnread) >= len(all) {
		t.Errorf("NOT tag:unread (%d) should have fewer results than * (%d)",
			len(withoutUnread), len(all))
	}
}

func TestSearch_FTS_BodyText(t *testing.T) {
	db := seedSearchDB(t)
	results, err := db.Search("invoice", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	// "invoice" appears in s1 (subject+body) and s4 (body)
	if len(results) < 1 {
		t.Errorf("got %d results for 'invoice', want at least 1", len(results))
	}
}

func TestSearch_Subject(t *testing.T) {
	db := seedSearchDB(t)
	results, err := db.Search("subject:vacation", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("got %d results for subject:vacation, want 1", len(results))
	}
}

// TestSearch_PhraseWordOrder asserts that a quoted phrase query
// distinguishes word order via the bigram tokens: two messages share
// the same word set but differ in adjacent-pair sequence — only the
// one whose body matches the phrase order should come back.
func TestSearch_PhraseWordOrder(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()
	db.InsertMessage(&Message{
		MessageID: "ordered@x", Subject: "Ordered",
		FromAddr: "a@x", Date: now, CreatedAt: now,
		BodyText: "quick brown fox jumps", FetchedBody: true,
	})
	db.InsertMessage(&Message{
		MessageID: "scrambled@x", Subject: "Scrambled",
		FromAddr: "a@x", Date: now + 1, CreatedAt: now + 1,
		BodyText: "fox brown quick jumps", FetchedBody: true,
	})

	// Unquoted: word-AND should hit both.
	bare, err := db.Search("quick brown fox", 10)
	if err != nil {
		t.Fatalf("bare: %v", err)
	}
	if len(bare) != 2 {
		t.Errorf("unquoted 'quick brown fox' got %d hits, want 2 (both share the words)", len(bare))
	}

	// Quoted: phrase match should hit ordered@x only.
	phrase, err := db.Search(`"quick brown fox"`, 10)
	if err != nil {
		t.Fatalf("phrase: %v", err)
	}
	if len(phrase) != 1 {
		t.Fatalf(`phrase "quick brown fox" got %d hits, want 1`, len(phrase))
	}
}

func TestSearch_ThreadGrouping(t *testing.T) {
	db := seedSearchDB(t)
	// s2 and s3 are in the same thread — should appear as one result
	results, err := db.Search("from:bob", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	// bob sent s2, which is in thread with s3 → 1 thread
	if len(results) != 1 {
		t.Errorf("got %d results for from:bob, want 1 (thread grouping)", len(results))
	}
}

func TestSearch_ResultHasTags(t *testing.T) {
	db := seedSearchDB(t)
	results, err := db.Search("tag:unread", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("no results")
	}
	if len(results[0].Tags) == 0 {
		t.Error("result should have tags")
	}
}

func TestSearch_Limit(t *testing.T) {
	db := seedSearchDB(t)
	results, err := db.Search("*", 2)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) > 2 {
		t.Errorf("got %d results with limit 2", len(results))
	}
}

func TestLex(t *testing.T) {
	tests := []struct {
		query string
		count int
	}{
		{"*", 1},
		{"", 1},
		{"from:alice", 1},
		{"from:alice tag:inbox", 2},
		{"NOT tag:spam", 2},
		{"hello world", 2},
		{"from:alice subject:meeting hello", 3},
		{"from:alice OR from:bob", 3},
		{"(from:alice OR from:bob) AND tag:inbox", 7},
	}
	for _, tt := range tests {
		tokens := lex(tt.query)
		if len(tokens) != tt.count {
			t.Errorf("lex(%q) = %d tokens, want %d", tt.query, len(tokens), tt.count)
		}
	}
}

func TestLex_Not(t *testing.T) {
	tokens := lex("NOT tag:spam")
	if len(tokens) != 2 {
		t.Fatalf("expected 2 tokens, got %d", len(tokens))
	}
	if tokens[0].kind != tokNot {
		t.Errorf("token[0].kind = %d, want tokNot", tokens[0].kind)
	}
	if tokens[1].kind != tokField || tokens[1].field != "tag" || tokens[1].value != "spam" {
		t.Errorf("token[1] = %+v, want field tag:spam", tokens[1])
	}
}

func TestLex_QuotedPhrase(t *testing.T) {
	cases := []struct {
		query   string
		wantKnd lexTokenKind
		wantFld string
		wantVal string
	}{
		{`"hello world"`, tokBare, "", "hello world"},
		{`subject:"deal with this"`, tokField, "subject", "deal with this"},
		// Quoted segment must not consume the keyword-promotion path —
		// "AND" stays a literal phrase, not a binary-op token.
		{`"AND"`, tokBare, "", "AND"},
	}
	for _, c := range cases {
		toks := lex(c.query)
		if len(toks) != 1 {
			t.Errorf("lex(%q) yielded %d tokens, want 1", c.query, len(toks))
			continue
		}
		got := toks[0]
		if got.kind != c.wantKnd || got.field != c.wantFld || got.value != c.wantVal || !got.phrase {
			t.Errorf("lex(%q) = %+v, want kind=%d field=%q value=%q phrase=true",
				c.query, got, c.wantKnd, c.wantFld, c.wantVal)
		}
	}
}

func TestLex_QuotedPhraseMixedWithBareTerms(t *testing.T) {
	// `from:alice "two words" tag:inbox` → 3 tokens, middle is phrase bare.
	toks := lex(`from:alice "two words" tag:inbox`)
	if len(toks) != 3 {
		t.Fatalf("got %d tokens, want 3: %+v", len(toks), toks)
	}
	if toks[1].kind != tokBare || toks[1].value != "two words" || !toks[1].phrase {
		t.Errorf("middle token = %+v, want phrase bare 'two words'", toks[1])
	}
	if toks[0].phrase || toks[2].phrase {
		t.Errorf("non-quoted terms must not have phrase=true (got %v / %v)", toks[0].phrase, toks[2].phrase)
	}
}

func TestSearch_OR(t *testing.T) {
	db := seedSearchDB(t)
	// alice sent s1, s3, s5; charlie sent s4
	// Threads: {s1}, {s2,s3}, {s4}, {s5} → alice in 3 threads, charlie in 1 → 4 total
	results, err := db.Search("from:alice OR from:charlie", 50)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 4 {
		t.Errorf("got %d results for from:alice OR from:charlie, want 4", len(results))
	}
}

func TestSearch_AND_explicit(t *testing.T) {
	db := seedSearchDB(t)
	// Only s1 has both from:alice AND tag:unread
	results, err := db.Search("from:alice AND tag:unread", 50)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("got %d results for from:alice AND tag:unread, want 1", len(results))
	}
}

func TestSearch_Parentheses(t *testing.T) {
	db := seedSearchDB(t)
	// (from:alice OR from:bob) AND tag:inbox
	// All messages have inbox. alice: s1,s3,s5; bob: s2
	// Matching threads: {s1}, {s2,s3}, {s5} → 3 threads
	results, err := db.Search("(from:alice OR from:bob) AND tag:inbox", 50)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("got %d results for (from:alice OR from:bob) AND tag:inbox, want 3", len(results))
	}
}

func TestFormatDateRelative(t *testing.T) {
	now := time.Now()

	// Today
	ts := now.Add(-1 * time.Hour).Unix()
	r := formatDateRelative(ts)
	if r == "" {
		t.Error("empty for today")
	}

	// Far past
	old := time.Date(2020, 1, 15, 10, 30, 0, 0, time.UTC).Unix()
	r = formatDateRelative(old)
	if r != "2020-01-15" {
		t.Errorf("old date = %q, want 2020-01-15", r)
	}
}

func TestSearch_DateRange(t *testing.T) {
	db := newTestDB(t)

	// Insert messages at known dates
	jan := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC).Unix()
	jun := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC).Unix()

	db.InsertMessage(&Message{
		MessageID: "jan@x", Subject: "January msg", FromAddr: "a@x",
		Date: jan, CreatedAt: jan, FetchedBody: true,
	})
	db.InsertMessage(&Message{
		MessageID: "jun@x", Subject: "June msg", FromAddr: "a@x",
		Date: jun, CreatedAt: jun, FetchedBody: true,
	})

	results, err := db.Search("date:2024-01..2024-03", 10)
	if err != nil {
		t.Fatalf("date search: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("got %d results for Jan-Mar range, want 1", len(results))
	}
}

func TestSearch_DateRelativeKeywords(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()

	// Insert a message from today and one from 2 months ago
	today := time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, now.Location())
	old := today.AddDate(0, -2, -5)

	db.InsertMessage(&Message{
		MessageID: "today@x", Subject: "Today msg", FromAddr: "a@x",
		Date: today.Unix(), CreatedAt: today.Unix(), FetchedBody: true,
	})
	db.InsertMessage(&Message{
		MessageID: "old@x", Subject: "Old msg", FromAddr: "a@x",
		Date: old.Unix(), CreatedAt: old.Unix(), FetchedBody: true,
	})

	// date:today should match only today's message
	results, err := db.Search("date:today", 10)
	if err != nil {
		t.Fatalf("date:today: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("date:today got %d results, want 1", len(results))
	}

	// date:week should match today's message
	results, _ = db.Search("date:week", 10)
	if len(results) != 1 {
		t.Errorf("date:week got %d results, want 1", len(results))
	}

	// date:year should match both
	results, _ = db.Search("date:year", 10)
	if len(results) != 2 {
		t.Errorf("date:year got %d results, want 2", len(results))
	}

	// date:month should match only today
	results, _ = db.Search("date:month", 10)
	if len(results) != 1 {
		t.Errorf("date:month got %d results, want 1", len(results))
	}
}

func TestSearch_DateOpenRanges(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()

	today := time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, now.Location())
	threeDaysAgo := today.AddDate(0, 0, -3)
	twoWeeksAgo := today.AddDate(0, 0, -14)

	db.InsertMessage(&Message{
		MessageID: "today@x", Subject: "Today msg", FromAddr: "a@x",
		Date: today.Unix(), CreatedAt: today.Unix(), FetchedBody: true,
	})
	db.InsertMessage(&Message{
		MessageID: "3d@x", Subject: "3 days ago msg", FromAddr: "a@x",
		Date: threeDaysAgo.Unix(), CreatedAt: threeDaysAgo.Unix(), FetchedBody: true,
	})
	db.InsertMessage(&Message{
		MessageID: "2w@x", Subject: "2 weeks ago msg", FromAddr: "a@x",
		Date: twoWeeksAgo.Unix(), CreatedAt: twoWeeksAgo.Unix(), FetchedBody: true,
	})

	// date:..7d — older than 7 days, should match only 2w@x
	results, err := db.Search("date:..7d", 10)
	if err != nil {
		t.Fatalf("date:..7d: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("date:..7d got %d results, want 1", len(results))
	}

	// date:..3d — older than 3 days; 3d@x is at 10am but boundary is midnight, so not matched
	results, err = db.Search("date:..3d", 10)
	if err != nil {
		t.Fatalf("date:..3d: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("date:..3d got %d results, want 1", len(results))
	}

	// date:7d.. — since 7 days ago, should match today@x and 3d@x
	results, err = db.Search("date:7d..", 10)
	if err != nil {
		t.Fatalf("date:7d..: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("date:7d.. got %d results, want 2", len(results))
	}

	// date:week.. — same as 7d.., should match today@x and 3d@x
	results, err = db.Search("date:week..", 10)
	if err != nil {
		t.Fatalf("date:week..: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("date:week.. got %d results, want 2", len(results))
	}
}

func TestResolveRelativeDate_Unknown(t *testing.T) {
	_, _, err := resolveRelativeDate("invalid")
	if err == nil {
		t.Error("expected error for unknown keyword")
	}
}

func TestSearch_AccountFilter(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	db.InsertMessage(&Message{
		MessageID: "acc1@x", Subject: "From work", FromAddr: "a@x",
		Date: now, CreatedAt: now, FetchedBody: true, Account: "work",
	})
	db.InsertMessage(&Message{
		MessageID: "acc2@x", Subject: "From personal", FromAddr: "b@x",
		Date: now - 100, CreatedAt: now, FetchedBody: true, Account: "personal",
	})
	// Cross-account message: one row per account
	db.InsertMessage(&Message{
		MessageID: "acc3@x", Subject: "Cross account", FromAddr: "c@x",
		Date: now - 200, CreatedAt: now, FetchedBody: true, Account: "work",
	})
	db.InsertMessage(&Message{
		MessageID: "acc3@x", Subject: "Cross account", FromAddr: "c@x",
		Date: now - 200, CreatedAt: now, FetchedBody: true, Account: "personal",
	})

	// path:work/** should match acc1 and acc3
	results, err := db.Search("path:work/**", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("got %d results for path:work/**, want 2", len(results))
	}

	// path:personal/** should match acc2 and acc3
	results, err = db.Search("path:personal/**", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("got %d results for path:personal/**, want 2", len(results))
	}
}

func TestExtractAccountFromPath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"work/**", "work"},
		{"personal/**", "personal"},
		{"backup/*", "backup"},
		{"work/INBOX", "work"},
		{"work", "work"},
		{"", ""},
	}
	for _, tt := range tests {
		got := extractAccountFromPath(tt.input)
		if got != tt.want {
			t.Errorf("extractAccountFromPath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSearch_MultiAccountOR(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	db.InsertMessage(&Message{
		MessageID: "m1@x", Subject: "Work mail", FromAddr: "a@x",
		Date: now, CreatedAt: now, FetchedBody: true, Account: "work",
	})
	db.InsertMessage(&Message{
		MessageID: "m2@x", Subject: "Personal mail", FromAddr: "b@x",
		Date: now - 100, CreatedAt: now, FetchedBody: true, Account: "personal",
	})
	db.InsertMessage(&Message{
		MessageID: "m3@x", Subject: "Other mail", FromAddr: "c@x",
		Date: now - 200, CreatedAt: now, FetchedBody: true, Account: "other",
	})

	// Multi-account query: path:work/** OR path:personal/** should match work + personal
	results, err := db.Search("(path:work/** OR path:personal/**)", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("got %d results for multi-account OR, want 2", len(results))
	}
}

func TestSearch_UnknownField(t *testing.T) {
	db := newTestDB(t)
	_, err := db.Search("unknown:value", 10)
	if err == nil {
		t.Error("expected error for unknown field")
	}
}

func TestExtractAccounts(t *testing.T) {
	tests := []struct {
		query string
		want  []string
	}{
		{"tag:inbox", nil},
		{"(tag:sent) AND (path:habric/**)", []string{"habric"}},
		{"(tag:inbox) AND (path:work/** OR path:gmail/**)", []string{"work", "gmail"}},
		{"*", nil},
		{"path:personal/**", []string{"personal"}},
	}
	for _, tt := range tests {
		got := extractAccounts(tt.query)
		if len(got) != len(tt.want) {
			t.Errorf("extractAccounts(%q) = %v, want %v", tt.query, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("extractAccounts(%q)[%d] = %q, want %q", tt.query, i, got[i], tt.want[i])
			}
		}
	}
}

func TestGetThreadTags_AccountScoped(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().Unix()

	// Insert messages in same thread but different accounts
	msg1 := &Message{
		MessageID: "scoped@x", Subject: "Scoped", FromAddr: "a@x",
		Date: now, CreatedAt: now, FetchedBody: true, Account: "work",
	}
	db.InsertMessage(msg1)
	db.AddTag(msg1.ID, "sent")

	msg2 := &Message{
		MessageID: "scoped@x", Subject: "Scoped", FromAddr: "a@x",
		Date: now, CreatedAt: now, FetchedBody: true, Account: "gmail",
	}
	db.InsertMessage(msg2)
	db.AddTag(msg2.ID, "inbox")

	threadID := msg1.ThreadID

	// No filter → both tags
	all, _ := db.getThreadTags(threadID)
	if len(all) != 2 {
		t.Errorf("all tags = %v, want 2", all)
	}

	// Work only
	work, _ := db.getThreadTags(threadID, "work")
	if len(work) != 1 || work[0] != "sent" {
		t.Errorf("work tags = %v, want [sent]", work)
	}

	// Gmail only
	gmail, _ := db.getThreadTags(threadID, "gmail")
	if len(gmail) != 1 || gmail[0] != "inbox" {
		t.Errorf("gmail tags = %v, want [inbox]", gmail)
	}
}
