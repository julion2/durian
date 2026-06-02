package imap

import (
	"fmt"
	"log/slog"
	"net/mail"
	"strings"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/store"
)

// RuleAttachment holds the attachment metadata needed for rule evaluation.
// Kept minimal to avoid importing cli/internal/mail.
type RuleAttachment struct {
	ContentType string
	Filename    string
}

// ValidateRuleQuery checks if a rule match expression is syntactically valid.
func ValidateRuleQuery(query string) error {
	_, err := parseRuleQuery(query)
	return err
}

// MatchingRules returns the rules whose match expression matches the message.
// account is the account identifier (e.g. alias); header is the parsed mail header.
// groups enables group: expansion in rule match expressions.
func MatchingRules(rules []config.RuleConfig, msg *store.Message, attachments []RuleAttachment, header mail.Header, account string, groups map[string]config.GroupEntry) []config.RuleConfig {
	var matched []config.RuleConfig
	for _, rule := range rules {
		if len(rule.Accounts) > 0 && !accountMatches(rule.Accounts, account) {
			continue
		}

		// Expand group: references in match expression
		matchExpr := rule.Match
		if len(groups) > 0 {
			expanded, err := config.ExpandGroupsInQuery(matchExpr, groups)
			if err != nil {
				slog.Warn("Group expansion failed in rule", "module", "RULES", "name", rule.Name, "err", err)
			} else {
				matchExpr = expanded
			}
		}

		expr, err := parseRuleQuery(matchExpr)
		if err != nil {
			slog.Warn("Skipping malformed rule", "module", "RULES", "name", rule.Name, "match", matchExpr, "err", err)
			continue
		}
		if evalExpr(expr, msg, attachments, header) {
			matched = append(matched, rule)
		}
	}
	return matched
}

func accountMatches(accounts []string, account string) bool {
	for _, a := range accounts {
		if strings.EqualFold(a, account) {
			return true
		}
	}
	return false
}

// --- AST node types (mirrors store/search.go, evaluated in-memory) ---

type ruleNode interface {
	ruleNode()
}

type ruleFieldNode struct {
	field string
	value string
}

type ruleBareNode struct {
	value string
}

type ruleBinaryNode struct {
	op    string // "AND" or "OR"
	left  ruleNode
	right ruleNode
}

type ruleNotNode struct {
	child ruleNode
}

func (*ruleFieldNode) ruleNode()  {}
func (*ruleBareNode) ruleNode()   {}
func (*ruleBinaryNode) ruleNode() {}
func (*ruleNotNode) ruleNode()    {}

// --- Lexer (reuses same token types as store/search.go) ---

type ruleTokenKind int

const (
	rtokField ruleTokenKind = iota
	rtokBare
	rtokLParen
	rtokRParen
	rtokAnd
	rtokOr
	rtokNot
)

const rtokEOF ruleTokenKind = -1

type ruleToken struct {
	kind  ruleTokenKind
	field string
	value string
}

func lexRule(query string) []ruleToken {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}

	query = strings.NewReplacer("(", " ( ", ")", " ) ").Replace(query)
	parts := strings.Fields(query)

	var tokens []ruleToken
	for _, p := range parts {
		switch {
		case p == "(":
			tokens = append(tokens, ruleToken{kind: rtokLParen})
		case p == ")":
			tokens = append(tokens, ruleToken{kind: rtokRParen})
		case strings.EqualFold(p, "AND"):
			tokens = append(tokens, ruleToken{kind: rtokAnd})
		case strings.EqualFold(p, "OR"):
			tokens = append(tokens, ruleToken{kind: rtokOr})
		case strings.EqualFold(p, "NOT"):
			tokens = append(tokens, ruleToken{kind: rtokNot})
		default:
			if idx := strings.Index(p, ":"); idx > 0 {
				tokens = append(tokens, ruleToken{
					kind:  rtokField,
					field: strings.ToLower(p[:idx]),
					value: p[idx+1:],
				})
			} else {
				tokens = append(tokens, ruleToken{kind: rtokBare, value: p})
			}
		}
	}
	return tokens
}

// --- Recursive descent parser ---

type ruleParser struct {
	tokens []ruleToken
	pos    int
}

func (p *ruleParser) peek() ruleToken {
	if p.pos >= len(p.tokens) {
		return ruleToken{kind: rtokEOF}
	}
	return p.tokens[p.pos]
}

func (p *ruleParser) next() ruleToken {
	tok := p.peek()
	if p.pos < len(p.tokens) {
		p.pos++
	}
	return tok
}

func parseRuleQuery(query string) (ruleNode, error) {
	tokens := lexRule(query)
	if len(tokens) == 0 {
		return nil, fmt.Errorf("empty rule query")
	}
	p := &ruleParser{tokens: tokens}
	node, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.tokens) {
		return nil, fmt.Errorf("unexpected token at position %d", p.pos)
	}
	return node, nil
}

func (p *ruleParser) parseExpr() (ruleNode, error) {
	return p.parseOr()
}

func (p *ruleParser) parseOr() (ruleNode, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == rtokOr {
		p.next()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &ruleBinaryNode{op: "OR", left: left, right: right}
	}
	return left, nil
}

func (p *ruleParser) parseAnd() (ruleNode, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		tok := p.peek()
		if tok.kind == rtokAnd {
			p.next()
		} else if tok.kind == rtokField || tok.kind == rtokBare ||
			tok.kind == rtokNot || tok.kind == rtokLParen {
			// implicit AND
		} else {
			break
		}
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &ruleBinaryNode{op: "AND", left: left, right: right}
	}
	return left, nil
}

func (p *ruleParser) parseUnary() (ruleNode, error) {
	if p.peek().kind == rtokNot {
		p.next()
		child, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &ruleNotNode{child: child}, nil
	}
	return p.parsePrimary()
}

func (p *ruleParser) parsePrimary() (ruleNode, error) {
	tok := p.peek()
	switch tok.kind {
	case rtokLParen:
		p.next()
		node, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != rtokRParen {
			return nil, fmt.Errorf("expected closing parenthesis")
		}
		p.next()
		return node, nil
	case rtokField:
		p.next()
		return &ruleFieldNode{field: tok.field, value: tok.value}, nil
	case rtokBare:
		p.next()
		return &ruleBareNode{value: tok.value}, nil
	default:
		return nil, fmt.Errorf("unexpected token at position %d", p.pos)
	}
}

// --- In-memory evaluation ---

func evalExpr(node ruleNode, msg *store.Message, attachments []RuleAttachment, header mail.Header) bool {
	switch n := node.(type) {
	case *ruleBinaryNode:
		if n.op == "OR" {
			return evalExpr(n.left, msg, attachments, header) || evalExpr(n.right, msg, attachments, header)
		}
		return evalExpr(n.left, msg, attachments, header) && evalExpr(n.right, msg, attachments, header)
	case *ruleNotNode:
		return !evalExpr(n.child, msg, attachments, header)
	case *ruleFieldNode:
		switch n.field {
		case "from":
			return strings.Contains(strings.ToLower(msg.FromAddr), strings.ToLower(n.value))
		case "to":
			return strings.Contains(strings.ToLower(msg.ToAddrs), strings.ToLower(n.value))
		case "cc":
			return strings.Contains(strings.ToLower(msg.CCAddrs), strings.ToLower(n.value))
		case "subject":
			return strings.Contains(strings.ToLower(msg.Subject), strings.ToLower(n.value))
		case "has":
			val := strings.ToLower(n.value)
			if val == "attachment" {
				return len(attachments) > 0
			}
			// has:attachment:TYPE — match on attachment content type
			if strings.HasPrefix(val, "attachment:") {
				wantType := val[len("attachment:"):]
				for _, att := range attachments {
					if strings.Contains(strings.ToLower(att.ContentType), wantType) {
						return true
					}
					if strings.HasSuffix(strings.ToLower(att.Filename), "."+wantType) {
						return true
					}
				}
				return false
			}
			return false
		case "header":
			// header:Name:value — split on first ":" in value
			idx := strings.Index(n.value, ":")
			if idx <= 0 {
				return false
			}
			hdrName := n.value[:idx]
			hdrSearch := strings.ToLower(n.value[idx+1:])
			hdrVal := header.Get(hdrName)
			if hdrSearch == "" {
				return hdrVal != "" // existence check
			}
			return strings.Contains(strings.ToLower(hdrVal), hdrSearch)
		default:
			return false
		}
	case *ruleBareNode:
		if n.value == "*" {
			return true // wildcard: match everything
		}
		v := strings.ToLower(n.value)
		return strings.Contains(strings.ToLower(msg.Subject), v) ||
			strings.Contains(strings.ToLower(msg.BodyText), v)
	default:
		return false
	}
}
