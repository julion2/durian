package store

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/durian-dev/durian/cli/internal/dbcrypto"
)

// SearchCount returns the number of threads matching a query.
func (d *DB) SearchCount(query string) (int, error) {
	where, params, err := d.parseQuery(query)
	if err != nil {
		return 0, fmt.Errorf("parse query: %w", err)
	}

	q := "SELECT COUNT(DISTINCT m.thread_id) FROM messages m"
	if where != "" {
		q += " WHERE " + where
	}

	var count int
	if err := d.db.QueryRow(q, params...).Scan(&count); err != nil {
		return 0, fmt.Errorf("search count: %w", err)
	}
	return count, nil
}

// Search finds threads matching a search query string.
// Results are grouped by thread and ordered by most recent message date descending.
func (d *DB) Search(query string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 50
	}

	where, params, err := d.parseQuery(query)
	if err != nil {
		return nil, fmt.Errorf("parse query: %w", err)
	}

	q := `
		SELECT
			m.thread_id,
			MAX(m.subject) AS subject,
			GROUP_CONCAT(DISTINCT m.from_addr) AS authors,
			MAX(m.date) AS max_date,
			(SELECT m2.to_addrs FROM messages m2
			 WHERE m2.thread_id = m.thread_id
			 ORDER BY m2.date DESC LIMIT 1) AS recipients
		FROM messages m
	`
	if where != "" {
		q += " WHERE " + where
	}
	q += `
		GROUP BY m.thread_id
		ORDER BY max_date DESC
		LIMIT ?
	`
	params = append(params, limit)

	rows, err := d.db.Query(q, params...)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}

	// Collect results first and close rows before making additional queries.
	// With SetMaxOpenConns(1), nested queries while rows are open would deadlock.
	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		err := rows.Scan(&r.Thread, &r.Subject, &r.Authors, &r.Timestamp, &r.Recipients)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		r.DateRelative = formatDateRelative(r.Timestamp)
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate search results: %w", err)
	}
	rows.Close()

	// Fetch tags for all threads in one batch query instead of per-thread.
	// Scope to queried accounts so cross-account threads only show
	// tags from the current profile's accounts.
	accounts := extractAccounts(query)
	threadIDs := make([]string, len(results))
	for i, r := range results {
		threadIDs[i] = r.Thread
	}
	tagMap, err := d.getThreadTagsBatch(threadIDs, accounts...)
	if err != nil {
		return nil, fmt.Errorf("get thread tags: %w", err)
	}
	for i := range results {
		results[i].Tags = tagMap[results[i].Thread]
	}

	return results, nil
}

// getThreadTagsBatch returns distinct tags for multiple threads in a single query.
// Returns map[threadID][]tags. When accounts are provided, only tags from those
// accounts are included.
func (d *DB) getThreadTagsBatch(threadIDs []string, accounts ...string) (map[string][]string, error) {
	if len(threadIDs) == 0 {
		return make(map[string][]string), nil
	}

	placeholders := make([]string, len(threadIDs))
	params := make([]interface{}, 0, len(threadIDs)+len(accounts))
	for i, id := range threadIDs {
		placeholders[i] = "?"
		params = append(params, id)
	}

	q := `SELECT DISTINCT m.thread_id, t.tag FROM tags t
		JOIN messages m ON m.id = t.message_id
		WHERE m.thread_id IN (` + strings.Join(placeholders, ",") + `)`

	if len(accounts) > 0 {
		acctPH := make([]string, len(accounts))
		for i, a := range accounts {
			acctPH[i] = "?"
			params = append(params, a)
		}
		q += " AND m.account IN (" + strings.Join(acctPH, ",") + ")"
	}
	q += " ORDER BY m.thread_id, t.tag"

	rows, err := d.db.Query(q, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string][]string)
	for rows.Next() {
		var threadID, tag string
		if err := rows.Scan(&threadID, &tag); err != nil {
			return nil, err
		}
		result[threadID] = append(result[threadID], tag)
	}
	return result, rows.Err()
}

// getThreadTags returns distinct tags for messages in a thread.
// When accounts are provided, only tags from those accounts are included.
func (d *DB) getThreadTags(threadID string, accounts ...string) ([]string, error) {
	q := `SELECT DISTINCT t.tag FROM tags t
		JOIN messages m ON m.id = t.message_id
		WHERE m.thread_id = ?`
	params := []interface{}{threadID}
	if len(accounts) > 0 {
		placeholders := make([]string, len(accounts))
		for i, a := range accounts {
			placeholders[i] = "?"
			params = append(params, a)
		}
		q += " AND m.account IN (" + strings.Join(placeholders, ",") + ")"
	}
	q += " ORDER BY t.tag"
	rows, err := d.db.Query(q, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tags []string
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, err
		}
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}

// --- AST node types ---

// exprNode is the interface for all expression tree nodes.
type exprNode interface {
	exprNode()
}

type fieldExpr struct {
	field string
	value string
}

type bareExpr struct {
	value string
}

type starExpr struct{}

type binaryExpr struct {
	op    string // "AND" or "OR"
	left  exprNode
	right exprNode
}

type notExpr struct {
	child exprNode
}

func (*fieldExpr) exprNode()  {}
func (*bareExpr) exprNode()   {}
func (*starExpr) exprNode()   {}
func (*binaryExpr) exprNode() {}
func (*notExpr) exprNode()    {}

// --- Lexer ---

type lexTokenKind int

const (
	tokField lexTokenKind = iota
	tokBare
	tokStar
	tokLParen
	tokRParen
	tokAnd
	tokOr
	tokNot
)

const tokEOF lexTokenKind = -1

type lexToken struct {
	kind  lexTokenKind
	field string // only for tokField
	value string // for tokField and tokBare
}

// lex breaks a query string into lexer tokens.
func lex(query string) []lexToken {
	query = strings.TrimSpace(query)
	if query == "" || query == "*" {
		return []lexToken{{kind: tokStar}}
	}

	query = strings.NewReplacer("(", " ( ", ")", " ) ").Replace(query)
	parts := strings.Fields(query)

	var tokens []lexToken
	for _, p := range parts {
		switch {
		case p == "(":
			tokens = append(tokens, lexToken{kind: tokLParen})
		case p == ")":
			tokens = append(tokens, lexToken{kind: tokRParen})
		case strings.EqualFold(p, "AND"):
			tokens = append(tokens, lexToken{kind: tokAnd})
		case strings.EqualFold(p, "OR"):
			tokens = append(tokens, lexToken{kind: tokOr})
		case strings.EqualFold(p, "NOT"):
			tokens = append(tokens, lexToken{kind: tokNot})
		case p == "*":
			tokens = append(tokens, lexToken{kind: tokStar})
		default:
			if idx := strings.Index(p, ":"); idx > 0 {
				tokens = append(tokens, lexToken{
					kind:  tokField,
					field: strings.ToLower(p[:idx]),
					value: p[idx+1:],
				})
			} else {
				tokens = append(tokens, lexToken{kind: tokBare, value: p})
			}
		}
	}
	return tokens
}

// --- Parser (recursive descent) ---
//
// Grammar:
//
//	expr     → or_expr
//	or_expr  → and_expr ("OR" and_expr)*
//	and_expr → unary ("AND"? unary)*
//	unary    → "NOT" unary | primary
//	primary  → "(" expr ")" | field:value | bare_word | "*"

const maxParseDepth = 50

type parser struct {
	tokens []lexToken
	pos    int
	depth  int
}

func (p *parser) peek() lexToken {
	if p.pos >= len(p.tokens) {
		return lexToken{kind: tokEOF}
	}
	return p.tokens[p.pos]
}

func (p *parser) next() lexToken {
	tok := p.peek()
	if p.pos < len(p.tokens) {
		p.pos++
	}
	return tok
}

func parse(tokens []lexToken) (exprNode, error) {
	if len(tokens) == 0 {
		return &starExpr{}, nil
	}
	p := &parser{tokens: tokens}
	node, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.tokens) {
		return nil, fmt.Errorf("unexpected token at position %d", p.pos)
	}
	return node, nil
}

func (p *parser) parseExpr() (exprNode, error) {
	return p.parseOr()
}

func (p *parser) parseOr() (exprNode, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tokOr {
		p.next()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &binaryExpr{op: "OR", left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (exprNode, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		tok := p.peek()
		if tok.kind == tokAnd {
			p.next()
		} else if tok.kind == tokField || tok.kind == tokBare || tok.kind == tokStar ||
			tok.kind == tokNot || tok.kind == tokLParen {
			// implicit AND between adjacent terms
		} else {
			break
		}
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &binaryExpr{op: "AND", left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseUnary() (exprNode, error) {
	if p.peek().kind == tokNot {
		p.next()
		child, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &notExpr{child: child}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (exprNode, error) {
	tok := p.peek()
	switch tok.kind {
	case tokLParen:
		p.depth++
		if p.depth > maxParseDepth {
			return nil, fmt.Errorf("query too deeply nested (max %d levels)", maxParseDepth)
		}
		p.next()
		node, err := p.parseExpr()
		p.depth--
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tokRParen {
			return nil, fmt.Errorf("expected closing parenthesis")
		}
		p.next()
		return node, nil
	case tokField:
		p.next()
		return &fieldExpr{field: tok.field, value: tok.value}, nil
	case tokBare:
		p.next()
		return &bareExpr{value: tok.value}, nil
	case tokStar:
		p.next()
		return &starExpr{}, nil
	default:
		return nil, fmt.Errorf("unexpected token at position %d", p.pos)
	}
}

// --- SQL generation ---

// exprToSQL walks the expression tree and produces a SQL WHERE clause with parameters.
// Promoted to a method on *DB so the bareExpr and subject: cases can reach the
// FTSToken sub-key for tokenizing user input against messages_blind_fts.
func (d *DB) exprToSQL(node exprNode) (string, []interface{}, error) {
	switch n := node.(type) {
	case *binaryExpr:
		leftSQL, leftParams, err := d.exprToSQL(n.left)
		if err != nil {
			return "", nil, err
		}
		rightSQL, rightParams, err := d.exprToSQL(n.right)
		if err != nil {
			return "", nil, err
		}
		return "(" + leftSQL + " " + n.op + " " + rightSQL + ")", append(leftParams, rightParams...), nil

	case *notExpr:
		childSQL, childParams, err := d.exprToSQL(n.child)
		if err != nil {
			return "", nil, err
		}
		return "NOT (" + childSQL + ")", childParams, nil

	case *fieldExpr:
		return d.fieldToSQL(n)

	case *bareExpr:
		// ADR-0001 step 7c: bare FTS search flips from the plaintext
		// messages_fts to the blind-token messages_blind_fts. The
		// user term is run through the same TokenizeFTS pipeline that
		// populated the index in step 7a+b, so the hex tokens produced
		// here line up with the tokens stored at write time.
		toks := dbcrypto.TokenizeFTSQuery(d.keyring.FTSToken, n.value)
		if toks == "" {
			// All-stop-words / punctuation-only input — never matches anything.
			return "1=0", nil, nil
		}
		return "m.id IN (SELECT rowid FROM messages_blind_fts WHERE messages_blind_fts MATCH ?)",
			[]interface{}{toks}, nil

	case *starExpr:
		return "1=1", nil, nil

	default:
		return "", nil, fmt.Errorf("unknown expression node type: %T", node)
	}
}

// parseQuery translates a search query into a SQL WHERE clause and parameters.
func (d *DB) parseQuery(query string) (where string, params []interface{}, err error) {
	tokens := lex(query)
	node, err := parse(tokens)
	if err != nil {
		return "", nil, err
	}
	if _, ok := node.(*starExpr); ok {
		return "", nil, nil
	}
	return d.exprToSQL(node)
}

// fieldToSQL converts a field expression into a SQL clause. Method on
// *DB so the subject: case can tokenize the user query against the
// FTSToken sub-key for messages_blind_fts (ADR-0001 step 7c).
func (d *DB) fieldToSQL(f *fieldExpr) (string, []interface{}, error) {
	switch f.field {
	case "from":
		return "m.from_addr LIKE ?", []interface{}{"%" + f.value + "%"}, nil

	case "to":
		return "m.to_addrs LIKE ?", []interface{}{"%" + f.value + "%"}, nil

	case "cc":
		return "m.cc_addrs LIKE ?", []interface{}{"%" + f.value + "%"}, nil

	case "subject":
		// ADR-0001 step 7c: subject: scoped FTS flips to messages_blind_fts.
		// subject_tok:(tok1 tok2 ...) AND's each token against the
		// subject_tok column only.
		toks := dbcrypto.TokenizeFTSQuery(d.keyring.FTSToken, f.value)
		if toks == "" {
			return "1=0", nil, nil
		}
		return "m.id IN (SELECT rowid FROM messages_blind_fts WHERE messages_blind_fts MATCH ?)",
			[]interface{}{"subject_tok:(" + toks + ")"}, nil

	case "tag":
		return "EXISTS (SELECT 1 FROM tags WHERE tags.message_id = m.id AND tags.tag = ?)",
			[]interface{}{f.value}, nil

	case "date":
		return parseDateRange(f.value)

	case "path":
		account := extractAccountFromPath(f.value)
		if account != "" {
			return "m.account = ?", []interface{}{account}, nil
		}
		return "1=1", nil, nil

	case "has":
		val := strings.ToLower(f.value)
		if val == "attachment" {
			return "EXISTS (SELECT 1 FROM attachments WHERE attachments.message_db_id = m.id)", nil, nil
		}
		if strings.HasPrefix(val, "attachment:") {
			wantType := val[len("attachment:"):]
			return "EXISTS (SELECT 1 FROM attachments WHERE attachments.message_db_id = m.id AND (LOWER(attachments.content_type) LIKE ? OR LOWER(attachments.filename) LIKE ?))",
				[]interface{}{"%" + wantType + "%", "%." + wantType}, nil
		}
		return "", nil, fmt.Errorf("unknown has: value %q (try: attachment, attachment:pdf)", f.value)

	case "folder", "thread", "id", "mimetype":
		return "1=1", nil, nil

	case "group":
		return "", nil, fmt.Errorf("group:%s was not expanded — check groups.pkl", f.value)

	default:
		return "", nil, fmt.Errorf("unknown query field: %q", f.field)
	}
}

// parseDateRange parses date queries into a SQL BETWEEN/comparison clause.
// Supports:
//   - Relative keywords: date:today, date:yesterday, date:week, date:2week,
//     date:month, date:2month, date:year, date:2year, date:30d, date:90d
//   - Ranges: date:2024-01..2024-02, date:2024-01-15..2024-02-28
//   - Open ranges: date:..month (older than 1 month), date:month.. (since 1 month ago)
func parseDateRange(value string) (string, []interface{}, error) {
	// Try relative keyword first (no ".." separator)
	if !strings.Contains(value, "..") {
		from, to, err := resolveRelativeDate(value)
		if err != nil {
			return "", nil, err
		}
		return "m.date BETWEEN ? AND ?", []interface{}{from, to}, nil
	}

	parts := strings.SplitN(value, "..", 2)
	if len(parts) != 2 {
		return "", nil, fmt.Errorf("date range must be FROM..TO, got %q", value)
	}

	now := time.Now()
	endOfDay := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 0, now.Location())

	// Open start: date:..X (older than X)
	if parts[0] == "" {
		to, err := resolveDateBound(parts[1], true)
		if err != nil {
			return "", nil, fmt.Errorf("parse date to: %w", err)
		}
		return "m.date <= ?", []interface{}{to}, nil
	}

	// Open end: date:X.. (since X)
	if parts[1] == "" {
		from, err := resolveDateBound(parts[0], true)
		if err != nil {
			return "", nil, fmt.Errorf("parse date from: %w", err)
		}
		return "m.date BETWEEN ? AND ?", []interface{}{from, endOfDay.Unix()}, nil
	}

	from, err := resolveDateBound(parts[0], true)
	if err != nil {
		return "", nil, fmt.Errorf("parse date from: %w", err)
	}
	to, err := resolveDateBound(parts[1], false)
	if err != nil {
		return "", nil, fmt.Errorf("parse date to: %w", err)
	}

	return "m.date BETWEEN ? AND ?", []interface{}{from, to}, nil
}

// resolveDateBound resolves a date string as either an absolute date or relative keyword.
// isStart controls whether to return the start or end of the resolved period.
func resolveDateBound(s string, isStart bool) (int64, error) {
	// Try relative keyword first
	from, to, err := resolveRelativeDate(s)
	if err == nil {
		if isStart {
			return from, nil
		}
		return to, nil
	}

	// Try absolute date
	if isStart {
		return parseDate(s)
	}
	return parseDateEnd(s)
}

// resolveRelativeDate converts a relative keyword to a (from, to) Unix timestamp pair.
func resolveRelativeDate(keyword string) (int64, int64, error) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	endOfDay := today.Add(24*time.Hour - time.Second)

	switch strings.ToLower(keyword) {
	case "today":
		return today.Unix(), endOfDay.Unix(), nil
	case "yesterday":
		y := today.AddDate(0, 0, -1)
		return y.Unix(), today.Add(-time.Second).Unix(), nil
	case "week":
		return today.AddDate(0, 0, -7).Unix(), endOfDay.Unix(), nil
	case "2week":
		return today.AddDate(0, 0, -14).Unix(), endOfDay.Unix(), nil
	case "month":
		return today.AddDate(0, -1, 0).Unix(), endOfDay.Unix(), nil
	case "2month":
		return today.AddDate(0, -2, 0).Unix(), endOfDay.Unix(), nil
	case "year":
		return today.AddDate(-1, 0, 0).Unix(), endOfDay.Unix(), nil
	case "2year":
		return today.AddDate(-2, 0, 0).Unix(), endOfDay.Unix(), nil
	default:
		// Try relative offset syntax: Nd (days), Nw (weeks), Nm (months), Ny (years)
		kw := strings.ToLower(keyword)
		if len(kw) >= 2 {
			suffix := kw[len(kw)-1]
			n, err := strconv.Atoi(kw[:len(kw)-1])
			if err == nil && n > 0 {
				switch suffix {
				case 'd':
					return today.AddDate(0, 0, -n).Unix(), endOfDay.Unix(), nil
				case 'w':
					return today.AddDate(0, 0, -n*7).Unix(), endOfDay.Unix(), nil
				case 'm':
					return today.AddDate(0, -n, 0).Unix(), endOfDay.Unix(), nil
				case 'y':
					return today.AddDate(-n, 0, 0).Unix(), endOfDay.Unix(), nil
				}
			}
		}
		return 0, 0, fmt.Errorf("unknown date keyword: %q (try: today, yesterday, week, month, year, 30d, 2w, 6m, 1y)", keyword)
	}
}

// parseDate parses a date string into a Unix timestamp (start of day/month).
func parseDate(s string) (int64, error) {
	for _, layout := range []string{"2006-01-02", "2006-01"} {
		t, err := time.Parse(layout, s)
		if err == nil {
			return t.Unix(), nil
		}
	}
	return 0, fmt.Errorf("unsupported date format: %q", s)
}

// parseDateEnd parses a date string into a Unix timestamp (end of day/month).
func parseDateEnd(s string) (int64, error) {
	for _, layout := range []string{"2006-01-02", "2006-01"} {
		t, err := time.Parse(layout, s)
		if err == nil {
			if layout == "2006-01" {
				// End of month
				t = t.AddDate(0, 1, 0).Add(-time.Second)
			} else {
				// End of day
				t = t.Add(24*time.Hour - time.Second)
			}
			return t.Unix(), nil
		}
	}
	return 0, fmt.Errorf("unsupported date format: %q", s)
}

// extractAccounts parses the query and collects account names from path: filters.
func extractAccounts(query string) []string {
	tokens := lex(query)
	node, err := parse(tokens)
	if err != nil {
		return nil
	}
	var accounts []string
	collectAccounts(node, &accounts)
	return accounts
}

// collectAccounts walks the AST and extracts accounts from path: field expressions.
func collectAccounts(node exprNode, accounts *[]string) {
	switch n := node.(type) {
	case *fieldExpr:
		if n.field == "path" {
			if a := extractAccountFromPath(n.value); a != "" {
				*accounts = append(*accounts, a)
			}
		}
	case *binaryExpr:
		collectAccounts(n.left, accounts)
		collectAccounts(n.right, accounts)
	case *notExpr:
		collectAccounts(n.child, accounts)
	}
}

// extractAccountFromPath extracts the account folder name from a path pattern.
// e.g. "work/**" → "work", "personal/INBOX" → "personal"
func extractAccountFromPath(value string) string {
	value = strings.TrimRight(value, "*")
	value = strings.TrimRight(value, "/")
	if idx := strings.Index(value, "/"); idx > 0 {
		return value[:idx]
	}
	return value
}

// formatDateRelative formats a Unix timestamp as a human-readable relative date.
func formatDateRelative(ts int64) string {
	t := time.Unix(ts, 0)
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	yesterday := today.AddDate(0, 0, -1)
	weekAgo := today.AddDate(0, 0, -7)

	switch {
	case t.After(today):
		return t.Format("15:04")
	case t.After(yesterday):
		return "Yesterday " + t.Format("15:04")
	case t.After(weekAgo):
		return t.Format("Mon 15:04")
	case t.Year() == now.Year():
		return t.Format("Jan 02")
	default:
		return t.Format("2006-01-02")
	}
}
