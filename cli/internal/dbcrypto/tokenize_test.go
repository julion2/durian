package dbcrypto

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestWordsForFTS(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   \t\n   ", nil},
		{"hello world", []string{"hello", "world"}},
		// UAX#29 keeps "don't" as one word, splits on punctuation that's
		// not part of a word. Trailing punctuation drops out.
		{"Hello, World!", []string{"hello", "world"}},
		{"don't stop", []string{"don't", "stop"}},
		// Lowercase + diacritics preserved (ADR-0001 §4 keeps them ON by
		// default for German/French users).
		{"Grüße aus München", []string{"grüße", "aus", "münchen"}},
		// Numbers count as words.
		{"item 42 ok", []string{"item", "42", "ok"}},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := wordsForFTS(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("wordsForFTS(%q) = %#v, want %#v", c.in, got, c.want)
			}
		})
	}
}

func TestTokenizeFTS_DeterministicAndUnique(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, KeyLen)

	a := TokenizeFTS(key, "Hello World")
	b := TokenizeFTS(key, "Hello World")
	if a != b {
		t.Errorf("TokenizeFTS not deterministic for the same input:\n  a=%q\n  b=%q", a, b)
	}

	// Different input → different output (a few cherry-picked pairs).
	other := TokenizeFTS(key, "World Hello")
	if a == other {
		t.Error("expected different output for reordered input (bigrams should differ)")
	}
}

func TestTokenizeFTS_DifferentKeysProduceDifferentTokens(t *testing.T) {
	k1 := bytes.Repeat([]byte{0x01}, KeyLen)
	k2 := bytes.Repeat([]byte{0x02}, KeyLen)
	if TokenizeFTS(k1, "hello") == TokenizeFTS(k2, "hello") {
		t.Error("tokens with different keys must not collide")
	}
}

func TestTokenizeFTS_UnigramsAndBigrams(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, KeyLen)
	// 3 words → 3 unigrams + 2 bigrams = 5 tokens.
	out := TokenizeFTS(key, "alpha beta gamma")
	tokens := strings.Fields(out)
	if got := len(tokens); got != 5 {
		t.Errorf("got %d tokens, want 5 (3 unigrams + 2 bigrams)", got)
	}
	// Each token must be exactly 2*FTSTokenLen hex characters.
	for i, tok := range tokens {
		if len(tok) != 2*FTSTokenLen {
			t.Errorf("token %d has length %d, want %d", i, len(tok), 2*FTSTokenLen)
		}
	}
}

func TestTokenizeFTS_BigramOrderMatters(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, KeyLen)
	// "ab cd" vs "abc d" must not collide via bigram generation —
	// the unit-separator 0x1f between adjacent words distinguishes them.
	ab := TokenizeFTS(key, "ab cd")
	abc := TokenizeFTS(key, "abc d")
	if ab == abc {
		t.Error("bigram delimiter failed to distinguish word boundary")
	}
}

func TestTokenizeFTS_EmptyInput(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, KeyLen)
	for _, s := range []string{"", "   ", "...", "!!!"} {
		if got := TokenizeFTS(key, s); got != "" {
			t.Errorf("TokenizeFTS(%q) = %q, want empty", s, got)
		}
	}
}

func TestTokenizeFTS_PanicsOnBadKey(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on short key")
		}
	}()
	TokenizeFTS(make([]byte, 16), "hello")
}

// TestTokenizeFTSPhrase_MatchesWriteSide asserts that the query-time
// phrase tokenizer produces exactly the set the write-time TokenizeFTS
// produced for the same text. That equivalence is the entire reason
// phrase queries can ride the existing index — every token the writer
// stored must be reproducible at read time without re-encrypting.
func TestTokenizeFTSPhrase_MatchesWriteSide(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, KeyLen)
	for _, s := range []string{"alpha beta gamma", "  weird  spacing  here  "} {
		got := strings.Fields(TokenizeFTSPhrase(key, s))
		want := strings.Fields(TokenizeFTS(key, s))
		if len(got) != len(want) {
			t.Fatalf("input %q: got %d tokens, want %d", s, len(got), len(want))
		}
		gotSet := map[string]bool{}
		for _, tok := range got {
			gotSet[tok] = true
		}
		for _, tok := range want {
			if !gotSet[tok] {
				t.Errorf("input %q: write-side token %q missing from phrase tokens", s, tok)
			}
		}
	}
}

func TestTokenizeFTSPhrase_SingleWordDegrades(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, KeyLen)
	// 1 word → 1 unigram, no bigrams.
	out := TokenizeFTSPhrase(key, "alpha")
	tokens := strings.Fields(out)
	if len(tokens) != 1 {
		t.Errorf("got %d tokens, want 1 (single-word phrase)", len(tokens))
	}
	if tokens[0] != TokenizeFTSQuery(key, "alpha") {
		t.Error("single-word phrase must produce the same unigram as TokenizeFTSQuery")
	}
}

// TestTokenizeFTS_BigramForgeryUnitSeparator asserts ADR-0001 audit
// medium: a literal U+001F (unit separator) byte smuggled into a word
// must NOT produce the same HMAC as the legitimate adjacent-pair
// bigram of the two halves. The pre-fix encoding wrote bigrams as
// `word_i || 0x1F || word_{i+1}`, so HMAC("foo" + 0x1F + "bar") was
// byte-identical to HMAC of the single segment "foo\x1Fbar" treated
// as one word. Length-prefixed encoding is bijective: distinct word
// pairs cannot collide regardless of contents.
func TestTokenizeFTS_BigramForgeryUnitSeparator(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, KeyLen)
	// Legitimate bigram from the two-word segmentation. Use unigram-
	// only TokenizeFTSQuery on "foo bar" and full TokenizeFTS to
	// compare the bigram token shape.
	full := strings.Fields(TokenizeFTS(key, "foo bar"))
	if len(full) != 3 {
		t.Fatalf("expected 2 unigrams + 1 bigram = 3 tokens, got %d", len(full))
	}
	legitimateBigram := full[2]

	// Attempt to forge: HMAC of the bigram-input bytes assembled by
	// the pre-fix encoding, "foo" + 0x1F + "bar".
	forgeryInput := []byte("foo")
	forgeryInput = append(forgeryInput, 0x1F)
	forgeryInput = append(forgeryInput, "bar"...)
	forgery := hmacHex(key, forgeryInput)

	if legitimateBigram == forgery {
		t.Errorf("bigram forgery via 0x1F succeeded — encoding is not bijective")
	}
}

func TestTokenizeFTSPhrase_EmptyInput(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, KeyLen)
	for _, s := range []string{"", "   ", "..."} {
		if got := TokenizeFTSPhrase(key, s); got != "" {
			t.Errorf("TokenizeFTSPhrase(%q) = %q, want empty", s, got)
		}
	}
}
