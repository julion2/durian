package store

import (
	"fmt"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// ftsTerm describes one bare or subject-scoped FTS term extracted from
// the parsed search AST. Used by postDecryptFilter (ADR-0001 audit H2)
// to verify each candidate row's plaintext actually contains the term
// — the blind-FTS5 index's 80-bit HMAC tokens have a non-zero collision
// probability, and without the recheck a chosen-plaintext attacker who
// can observe search-count deltas can confirm token collisions and
// recover the fts_token sub-key one bit at a time.
//
// scope=ftsScopeBare → verify against subject + body_text + body_html
// (matches what TokenizeFTS indexed). scope=ftsScopeSubject → subject
// only. phrase=true → substring-match the full phrase (with the same
// uniseg word-form normalization the tokenizer applied so the recheck
// stays consistent with what the index would accept). phrase=false →
// every space-separated word must appear in the haystack.
type ftsTerm struct {
	value  string
	phrase bool
	scope  ftsScope
}

type ftsScope uint8

const (
	ftsScopeBare ftsScope = iota
	ftsScopeSubject
)

// extractFTSTerms walks the parsed AST and returns every term that
// flowed through the blind-FTS5 index (i.e. would benefit from a
// post-decrypt recheck). Other field expressions (tag:, path:, date:,
// from:, to:, cc:, has:) are SQL-side filters with no HMAC-collision
// exposure and are skipped. NOT-wrapped subtrees are also skipped:
// a NOT can hide-then-reveal but cannot confirm a token match, so
// the recheck has nothing to verify for negative branches.
func extractFTSTerms(node exprNode) []ftsTerm {
	var out []ftsTerm
	collectFTSTerms(node, &out, false)
	return out
}

func collectFTSTerms(node exprNode, out *[]ftsTerm, underNot bool) {
	switch n := node.(type) {
	case *binaryExpr:
		collectFTSTerms(n.left, out, underNot)
		collectFTSTerms(n.right, out, underNot)
	case *notExpr:
		collectFTSTerms(n.child, out, !underNot)
	case *bareExpr:
		if underNot {
			return
		}
		*out = append(*out, ftsTerm{value: n.value, phrase: n.phrase, scope: ftsScopeBare})
	case *fieldExpr:
		if underNot || n.field != "subject" {
			return
		}
		*out = append(*out, ftsTerm{value: n.value, phrase: n.phrase, scope: ftsScopeSubject})
	}
}

// haystacks bundles the decrypted plaintext for one candidate message,
// already lower-cased for the case-insensitive substring checks the
// filter runs. Populated by postDecryptFetch in one batch query.
type haystacks struct {
	subjectLower string
	bodyLower    string // body_text + " " + body_html (HTML tags retained — they're stripped by tokenizer, so a tag-name match is still a real word match)
}

// postDecryptFilter returns the subset of candidateIDs whose decrypted
// plaintext actually contains every fts term. ADR §4 specifies this
// post-decrypt recheck as mandatory for the blind-FTS path; the audit
// found it was promised but never implemented.
//
// When terms is empty (search has no FTS-bound terms, e.g. tag:inbox
// alone), the filter is a no-op and returns candidateIDs unchanged
// — there is no collision-via-search-count oracle to plug because the
// blind-FTS index wasn't consulted.
func (d *DB) postDecryptFilter(candidateIDs []int64, node exprNode) ([]int64, error) {
	if node == nil || len(candidateIDs) == 0 || len(extractFTSTerms(node)) == 0 {
		return candidateIDs, nil
	}
	hs, err := d.postDecryptFetch(candidateIDs)
	if err != nil {
		return nil, fmt.Errorf("post-decrypt fetch: %w", err)
	}
	// Per-candidate truth for each non-FTS leaf (tag:/from:/date:/...). Without
	// it, evalFTS would assume those branches match, and an `OR` with a non-FTS
	// branch (e.g. `from:alice OR hawaii`) would keep a message that only
	// entered through a blind-index collision for the FTS term — re-opening the
	// search-count oracle the recheck exists to close.
	leafSets, err := d.sqlLeafSets(node, candidateIDs)
	if err != nil {
		return nil, err
	}
	lower := cases.Lower(language.Und)
	surviving := candidateIDs[:0]
	for _, id := range candidateIDs {
		hay, ok := hs[id]
		if !ok {
			// Row vanished between FTS-MATCH and fetch (DELETE race).
			// Conservatively drop — better a stale empty result than a
			// false positive that the attacker can use as a signal.
			continue
		}
		if evalFTS(node, hay, id, lower, leafSets) {
			surviving = append(surviving, id)
		}
	}
	return surviving, nil
}

// sqlLeafSets computes, for every non-FTS field leaf in the query, the subset of
// candidateIDs that genuinely satisfy that leaf's SQL predicate — reusing
// fieldToSQL so the recheck can't drift from the candidate query's semantics.
// evalFTS consults these instead of assuming a non-FTS branch matched. Returns
// nil when the query has no non-FTS leaves (a pure-FTS query needs none).
func (d *DB) sqlLeafSets(node exprNode, ids []int64) (map[*fieldExpr]map[int64]bool, error) {
	var leaves []*fieldExpr
	collectSQLLeaves(node, &leaves)
	if len(leaves) == 0 {
		return nil, nil
	}
	idPH := make([]string, len(ids))
	idArgs := make([]any, len(ids))
	for i, id := range ids {
		idPH[i] = "?"
		idArgs[i] = id
	}
	idIn := "m.id IN (" + strings.Join(idPH, ",") + ")"

	sets := make(map[*fieldExpr]map[int64]bool, len(leaves))
	for _, leaf := range leaves {
		where, params, err := d.fieldToSQL(leaf)
		if err != nil {
			return nil, fmt.Errorf("sql leaf recheck (%s): %w", leaf.field, err)
		}
		args := append(append([]any{}, idArgs...), params...)
		rows, err := d.db.Query("SELECT m.id FROM messages m WHERE "+idIn+" AND ("+where+")", args...)
		if err != nil {
			return nil, fmt.Errorf("sql leaf recheck (%s): %w", leaf.field, err)
		}
		set := make(map[int64]bool)
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan sql leaf id: %w", err)
			}
			set[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterate sql leaf ids: %w", err)
		}
		sets[leaf] = set
	}
	return sets, nil
}

// collectSQLLeaves gathers every field leaf that is enforced in SQL rather than
// through the blind-FTS index (i.e. not subject:). bare/subject FTS leaves and
// star are handled by evalFTS directly.
func collectSQLLeaves(node exprNode, out *[]*fieldExpr) {
	switch n := node.(type) {
	case *binaryExpr:
		collectSQLLeaves(n.left, out)
		collectSQLLeaves(n.right, out)
	case *notExpr:
		collectSQLLeaves(n.child, out)
	case *fieldExpr:
		if n.field != "subject" {
			*out = append(*out, n)
		}
	}
}

// evalFTS re-evaluates a query's blind-FTS conditions against one decrypted
// message, mirroring the boolean structure the SQL WHERE clause used. This is
// what keeps the collision recheck honest for positive FTS terms WITHOUT
// collapsing OR into AND — the earlier "every extracted term must match" list
// silently turned `a OR b` into `a AND b`, shrinking (not growing) the result
// set with each added OR branch.
//
//   - FTS leaves (bare, subject:) are substring-checked against the plaintext.
//   - Non-FTS field leaves (tag:, from:, to:, date:, path:, has:, ...) use their
//     real per-candidate SQL truth from leafSets, NOT a blanket true — otherwise
//     an `OR` with a non-FTS branch would mask a blind-index collision in the
//     FTS branch and leak it through SearchCount.
//   - NOT subtrees are negated over the real evaluation of their child, so a
//     `hawaii OR NOT tag:x` cannot keep a collision candidate that actually has
//     tag x.
func evalFTS(node exprNode, hay haystacks, id int64, lower cases.Caser, leafSets map[*fieldExpr]map[int64]bool) bool {
	switch n := node.(type) {
	case *binaryExpr:
		if n.op == "OR" {
			return evalFTS(n.left, hay, id, lower, leafSets) || evalFTS(n.right, hay, id, lower, leafSets)
		}
		return evalFTS(n.left, hay, id, lower, leafSets) && evalFTS(n.right, hay, id, lower, leafSets)
	case *notExpr:
		return !evalFTS(n.child, hay, id, lower, leafSets)
	case *bareExpr:
		t := ftsTerm{value: lower.String(n.value), phrase: n.phrase, scope: ftsScopeBare}
		return termMatchesHay(t, hay.subjectLower+"\x00"+hay.bodyLower)
	case *fieldExpr:
		if n.field == "subject" {
			t := ftsTerm{value: lower.String(n.value), phrase: n.phrase, scope: ftsScopeSubject}
			return termMatchesHay(t, hay.subjectLower)
		}
		return leafSets[n][id]
	default: // starExpr matches every row
		return true
	}
}

// termMatchesHay checks one term against one haystack. Phrase queries
// must appear as a contiguous substring; word-AND queries must have
// every space-separated word appear somewhere. The tokenizer normalizes
// to lowercase, so the haystack here is already lower-cased by the
// caller; the term has been lower-cased by postDecryptFilter.
func termMatchesHay(t ftsTerm, hay string) bool {
	if t.phrase {
		return strings.Contains(hay, t.value)
	}
	for _, word := range strings.Fields(t.value) {
		if !strings.Contains(hay, word) {
			return false
		}
	}
	return true
}

// postDecryptFetch loads subject + body plaintext for every id in one
// query and returns a map keyed by id. Uses an IN (?,?,?) param list;
// SQLite's default SQLITE_MAX_VARIABLE_NUMBER is 999 which dwarfs any
// realistic candidate set from a single search. Callers that paginate
// must keep the chunk size below that bound.
func (d *DB) postDecryptFetch(ids []int64) (map[int64]haystacks, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	q := `SELECT id, subject_ct, body_text_ct, body_html_ct FROM messages
		WHERE id IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()
	lower := cases.Lower(language.Und)
	out := make(map[int64]haystacks, len(ids))
	for rows.Next() {
		var id int64
		var subjectCT, bodyTextCT, bodyHTMLCT []byte
		if err := rows.Scan(&id, &subjectCT, &bodyTextCT, &bodyHTMLCT); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		subj, err := d.decryptSubject("", subjectCT)
		if err != nil {
			return nil, fmt.Errorf("decrypt subject id=%d: %w", id, err)
		}
		bodyText, err := d.decryptBody("", bodyTextCT)
		if err != nil {
			return nil, fmt.Errorf("decrypt body_text id=%d: %w", id, err)
		}
		bodyHTML, err := d.decryptBody("", bodyHTMLCT)
		if err != nil {
			return nil, fmt.Errorf("decrypt body_html id=%d: %w", id, err)
		}
		out[id] = haystacks{
			subjectLower: lower.String(subj),
			bodyLower:    lower.String(bodyText) + " " + lower.String(bodyHTML),
		}
	}
	return out, rows.Err()
}
