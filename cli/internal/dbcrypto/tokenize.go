package dbcrypto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"

	"github.com/rivo/uniseg"
)

// FTSTokenLen is the truncated HMAC length per ADR-0001 §4 — 80 bits
// (10 bytes) renders as 20 hex characters. Truncation trades a small
// collision probability for index-size savings; the post-decrypt
// false-positive filter (step 7c) catches the rare collisions before
// returning results to the user.
const FTSTokenLen = 10

// TokenizeFTS produces a space-separated list of blind tokens suitable
// for inserting into the FTS5 messages_blind_fts shadow table. The
// pipeline mirrors ADR-0001 §4:
//
//  1. UAX#29 word segmentation via rivo/uniseg, so Asian scripts, German
//     compounds and CJK punctuation all segment correctly.
//  2. Lowercase + Unicode-aware whitespace/punctuation drop. Empty
//     segments are discarded; no stop-word removal (the ADR keeps every
//     word to make frequency-analysis attacks harder).
//  3. For each surviving word, emit HMAC-SHA256(key, word) truncated to
//     FTSTokenLen bytes and hex-encoded — that fixed-length ASCII form
//     is what FTS5 actually indexes.
//  4. Adjacent unigram bigrams: HMAC(word_i || "\x1f" || word_{i+1})
//     truncated the same way. The 0x1f unit-separator avoids any
//     ambiguity between "ab cd" and "a bcd". Bigrams enable 2-word
//     phrase queries without leaking which positional pair matched.
//
// Returns an empty string for empty/whitespace-only input.
func TokenizeFTS(key []byte, plaintext string) string {
	words := wordsForFTS(plaintext)
	if len(words) == 0 {
		return ""
	}
	out := make([]string, 0, len(words)*2)
	for _, w := range words {
		out = append(out, hmacHex(key, []byte(w)))
	}
	for i := 0; i < len(words)-1; i++ {
		var buf strings.Builder
		buf.WriteString(words[i])
		buf.WriteByte(0x1f)
		buf.WriteString(words[i+1])
		out = append(out, hmacHex(key, []byte(buf.String())))
	}
	return strings.Join(out, " ")
}

// TokenizeFTSQuery returns the same per-word HMAC tokens TokenizeFTS
// produces but WITHOUT the adjacent-pair bigrams. Use this on the
// read side for plain word-AND queries — emitting the index-time
// bigrams as additional query tokens would AND-require the searcher's
// word pair to also appear consecutively in the source, turning a
// word-AND query into a phrase-match by accident. Phrase-query
// support reuses TokenizeFTS (with bigrams) once the parser learns
// quote-for-phrase syntax.
func TokenizeFTSQuery(key []byte, plaintext string) string {
	words := wordsForFTS(plaintext)
	if len(words) == 0 {
		return ""
	}
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = hmacHex(key, []byte(w))
	}
	return strings.Join(out, " ")
}

// wordsForFTS runs plaintext through the segment+normalize pipeline,
// returning the lowercase word forms ready to HMAC. Exposed (lowercase)
// for tests only — production callers always go through TokenizeFTS.
func wordsForFTS(plaintext string) []string {
	if plaintext == "" {
		return nil
	}
	state := -1
	rest := plaintext
	var out []string
	for len(rest) > 0 {
		var seg string
		seg, rest, state = uniseg.FirstWordInString(rest, state)
		w := normalizeWord(seg)
		if w == "" {
			continue
		}
		out = append(out, w)
	}
	return out
}

// normalizeWord lowercases seg and drops it if everything in it is
// whitespace or punctuation. ADR-0001 §4 footnote keeps diacritics
// (ON by default for German/French mail use) — we deliberately do not
// run NFKD-strip here.
func normalizeWord(seg string) string {
	hasLetterOrDigit := false
	for _, r := range seg {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			hasLetterOrDigit = true
			break
		}
	}
	if !hasLetterOrDigit {
		return ""
	}
	return strings.ToLower(seg)
}

// hmacHex returns the hex-encoded prefix of HMAC-SHA256(key, msg)
// truncated to FTSTokenLen bytes.
func hmacHex(key, msg []byte) string {
	if len(key) != KeyLen {
		// Programming error — fts_token sub-key must be 32 bytes. Panic
		// rather than return a corrupt index that step-7c readers would
		// silently fail to match against.
		panic("dbcrypto: TokenizeFTS requires a 32-byte HMAC key")
	}
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	sum := m.Sum(nil)
	return hex.EncodeToString(sum[:FTSTokenLen])
}
