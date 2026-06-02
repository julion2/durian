package imap

import (
	"net/mail"
	"testing"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/store"
)

func TestEvalExpr_FieldFrom(t *testing.T) {
	msg := &store.Message{FromAddr: "alice@work.test"}
	expr, _ := parseRuleQuery("from:@work.test")
	if !evalExpr(expr, msg, nil, nil) {
		t.Error("expected from:@work.test to match")
	}

	expr2, _ := parseRuleQuery("from:@example.com")
	if evalExpr(expr2, msg, nil, nil) {
		t.Error("expected from:@example.com to NOT match")
	}
}

func TestEvalExpr_FieldTo(t *testing.T) {
	msg := &store.Message{ToAddrs: "newsletter@example.com, bob@test.com"}
	expr, _ := parseRuleQuery("to:newsletter@")
	if !evalExpr(expr, msg, nil, nil) {
		t.Error("expected to:newsletter@ to match")
	}
}

func TestEvalExpr_FieldSubject(t *testing.T) {
	msg := &store.Message{Subject: "Weekly Status Report"}
	expr, _ := parseRuleQuery("subject:status")
	if !evalExpr(expr, msg, nil, nil) {
		t.Error("expected subject:status to match (case-insensitive)")
	}
}

func TestEvalExpr_HasAttachment(t *testing.T) {
	msg := &store.Message{}
	expr, _ := parseRuleQuery("has:attachment")
	if evalExpr(expr, msg, nil, nil) {
		t.Error("expected has:attachment to NOT match with 0 attachments")
	}
	atts := []RuleAttachment{{ContentType: "application/pdf", Filename: "doc.pdf"}}
	if !evalExpr(expr, msg, atts, nil) {
		t.Error("expected has:attachment to match with attachments")
	}
}

func TestEvalExpr_HasAttachmentType(t *testing.T) {
	msg := &store.Message{}
	atts := []RuleAttachment{
		{ContentType: "application/pdf", Filename: "contract.pdf"},
		{ContentType: "image/jpeg", Filename: "photo.jpg"},
	}

	expr, _ := parseRuleQuery("has:attachment:pdf")
	if !evalExpr(expr, msg, atts, nil) {
		t.Error("expected has:attachment:pdf to match")
	}

	expr2, _ := parseRuleQuery("has:attachment:xlsx")
	if evalExpr(expr2, msg, atts, nil) {
		t.Error("expected has:attachment:xlsx to NOT match")
	}

	// Match by file extension when content-type is generic
	atts2 := []RuleAttachment{{ContentType: "application/octet-stream", Filename: "data.csv"}}
	expr3, _ := parseRuleQuery("has:attachment:csv")
	if !evalExpr(expr3, msg, atts2, nil) {
		t.Error("expected has:attachment:csv to match by filename")
	}
}

func TestEvalExpr_CC(t *testing.T) {
	msg := &store.Message{CCAddrs: "alice@example.com, bob@work.test"}
	expr, _ := parseRuleQuery("cc:@work.test")
	if !evalExpr(expr, msg, nil, nil) {
		t.Error("expected cc:@work.test to match")
	}

	expr2, _ := parseRuleQuery("cc:@other.com")
	if evalExpr(expr2, msg, nil, nil) {
		t.Error("expected cc:@other.com to NOT match")
	}
}

func TestEvalExpr_BareWord(t *testing.T) {
	msg := &store.Message{Subject: "Invoice for January", BodyText: "Please find attached."}
	expr, _ := parseRuleQuery("invoice")
	if !evalExpr(expr, msg, nil, nil) {
		t.Error("expected bare word 'invoice' to match subject")
	}

	expr2, _ := parseRuleQuery("attached")
	if !evalExpr(expr2, msg, nil, nil) {
		t.Error("expected bare word 'attached' to match body")
	}

	expr3, _ := parseRuleQuery("missing")
	if evalExpr(expr3, msg, nil, nil) {
		t.Error("expected bare word 'missing' to NOT match")
	}
}

func TestEvalExpr_BooleanAND(t *testing.T) {
	msg := &store.Message{FromAddr: "alice@work.test", Subject: "Report"}
	expr, _ := parseRuleQuery("from:@work.test subject:report")
	if !evalExpr(expr, msg, nil, nil) {
		t.Error("expected implicit AND to match")
	}

	msg2 := &store.Message{FromAddr: "alice@work.test", Subject: "Hello"}
	if evalExpr(expr, msg2, nil, nil) {
		t.Error("expected implicit AND to NOT match when subject differs")
	}
}

func TestEvalExpr_BooleanOR(t *testing.T) {
	msg := &store.Message{FromAddr: "alice@work.test"}
	expr, _ := parseRuleQuery("from:@work.test OR from:@example.com")
	if !evalExpr(expr, msg, nil, nil) {
		t.Error("expected OR to match first branch")
	}

	msg2 := &store.Message{FromAddr: "bob@other.com"}
	if evalExpr(expr, msg2, nil, nil) {
		t.Error("expected OR to NOT match when neither branch matches")
	}
}

func TestEvalExpr_BooleanNOT(t *testing.T) {
	msg := &store.Message{FromAddr: "alice@work.test"}
	expr, _ := parseRuleQuery("NOT from:@example.com")
	if !evalExpr(expr, msg, nil, nil) {
		t.Error("expected NOT to match when inner doesn't match")
	}

	expr2, _ := parseRuleQuery("NOT from:@work.test")
	if evalExpr(expr2, msg, nil, nil) {
		t.Error("expected NOT to NOT match when inner matches")
	}
}

func TestEvalExpr_Parentheses(t *testing.T) {
	msg := &store.Message{FromAddr: "alice@work.test", Subject: "Report"}
	expr, _ := parseRuleQuery("(from:@work.test OR from:@example.com) subject:report")
	if !evalExpr(expr, msg, nil, nil) {
		t.Error("expected grouped OR + AND to match")
	}
}

func TestEvalExpr_CaseInsensitive(t *testing.T) {
	msg := &store.Message{FromAddr: "Alice@WORK.TEST"}
	expr, _ := parseRuleQuery("from:@work.test")
	if !evalExpr(expr, msg, nil, nil) {
		t.Error("expected case-insensitive match on from")
	}
}

func TestEvalExpr_Header(t *testing.T) {
	msg := &store.Message{}
	hdr := mail.Header{"List-Id": []string{"<python-dev.python.org>"}}

	expr, _ := parseRuleQuery("header:list-id:python-dev")
	if !evalExpr(expr, msg, nil, hdr) {
		t.Error("expected header:list-id to match")
	}

	expr2, _ := parseRuleQuery("header:list-id:ruby")
	if evalExpr(expr2, msg, nil, hdr) {
		t.Error("expected header:list-id:ruby to NOT match")
	}
}

func TestEvalExpr_HeaderReturnPath(t *testing.T) {
	msg := &store.Message{}
	hdr := mail.Header{"Return-Path": []string{"<bounces@lists.test>"}}

	expr, _ := parseRuleQuery("header:return-path:bounces@")
	if !evalExpr(expr, msg, nil, hdr) {
		t.Error("expected header:return-path to match")
	}
}

func TestEvalExpr_HeaderExistence(t *testing.T) {
	msg := &store.Message{}

	// Header present → should match
	hdr := mail.Header{"List-Unsubscribe": []string{"<mailto:unsub@test.com>"}}
	expr, _ := parseRuleQuery("header:list-unsubscribe:")
	if !evalExpr(expr, msg, nil, hdr) {
		t.Error("expected header:list-unsubscribe: to match when header exists")
	}

	// Header absent → should NOT match
	hdrEmpty := mail.Header{}
	if evalExpr(expr, msg, nil, hdrEmpty) {
		t.Error("expected header:list-unsubscribe: to NOT match when header is absent")
	}

	// Nil header → should NOT match
	if evalExpr(expr, msg, nil, nil) {
		t.Error("expected header:list-unsubscribe: to NOT match with nil header")
	}
}

func TestMatchingRules(t *testing.T) {
	rules := []config.RuleConfig{
		{Name: "Work", Match: "from:@work.test", AddTags: []string{"work"}},
		{Name: "Newsletter", Match: "to:newsletter@", AddTags: []string{"newsletter"}, RemoveTags: []string{"inbox"}},
		{Name: "Bad rule", Match: "(((", AddTags: []string{"bad"}},
	}

	msg := &store.Message{
		FromAddr: "alice@work.test",
		ToAddrs:  "newsletter@work.test",
	}

	matched := MatchingRules(rules, msg, nil, nil, "myaccount", nil)
	if len(matched) != 2 {
		t.Errorf("expected 2 matched rules, got %d", len(matched))
	}
}

func TestWildcardMatchesEverything(t *testing.T) {
	rules := []config.RuleConfig{
		{Name: "CatchAll", Match: "*", AddTags: []string{"all"}},
	}

	msg := &store.Message{FromAddr: "anyone@anywhere.com", Subject: "Whatever"}
	matched := MatchingRules(rules, msg, nil, nil, "test", nil)
	if len(matched) != 1 {
		t.Errorf("wildcard * should match everything, got %d matches", len(matched))
	}
}

func TestMatchingRules_AccountScope(t *testing.T) {
	rules := []config.RuleConfig{
		{Name: "Scoped", Match: "from:@work.test", AddTags: []string{"work"}, Accounts: []string{"personal"}},
		{Name: "Global", Match: "from:@work.test", AddTags: []string{"all"}},
	}

	msg := &store.Message{FromAddr: "alice@work.test"}

	// Should match only the global rule when account doesn't match
	matched := MatchingRules(rules, msg, nil, nil, "office", nil)
	if len(matched) != 1 || matched[0].Name != "Global" {
		t.Errorf("expected only Global rule, got %v", matched)
	}

	// Should match both when account matches
	matched2 := MatchingRules(rules, msg, nil, nil, "personal", nil)
	if len(matched2) != 2 {
		t.Errorf("expected 2 rules for matching account, got %d", len(matched2))
	}
}
