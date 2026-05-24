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
