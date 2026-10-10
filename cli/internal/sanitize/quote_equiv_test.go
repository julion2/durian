package sanitize

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The regexp-based implementation StripQuotedContent had before the
// prefilters and the hand-written scanners (with the fix of lowercasing ASCII
// only). Both must give the same result for every input; it stays here as the
// reference.

var refTagOrSpace = regexp.MustCompile(`(?:<[^>]*>|\s|&nbsp;)+`)

func refIsEffectivelyEmpty(html string) bool {
	text := strings.TrimSpace(refTagOrSpace.ReplaceAllString(html, " "))
	if text == "" {
		return true
	}
	textLower := strings.ToLower(text)
	for _, sig := range mobileSignatures {
		if strings.Contains(textLower, sig) {
			remainder := strings.TrimSpace(strings.ReplaceAll(textLower, sig, ""))
			if len(remainder) < 5 {
				return true
			}
		}
	}
	return false
}

func refStripQuotedContent(html string) string {
	if html == "" {
		return html
	}
	htmlLower := []byte(html)
	for i, c := range htmlLower {
		if 'A' <= c && c <= 'Z' {
			htmlLower[i] = c + 32
		}
	}
	earliestIdx := -1
	for _, pattern := range quotePatterns {
		idx := strings.Index(string(htmlLower), strings.ToLower(pattern))
		if idx != -1 && (earliestIdx == -1 || idx < earliestIdx) {
			earliestIdx = idx
		}
	}
	for _, re := range quoteRegexPatterns {
		loc := re.FindStringIndex(html)
		if loc != nil && (earliestIdx == -1 || loc[0] < earliestIdx) {
			earliestIdx = loc[0]
		}
	}
	if earliestIdx == -1 {
		return html
	}
	stripped := strings.TrimRight(html[:earliestIdx], " \t\n\r")
	if refIsEffectivelyEmpty(stripped) {
		return html
	}
	return stripped
}

// quoteEdgeCases hit the corners of the scanner and the prefilters.
var quoteEdgeCases = []string{
	"", " ", "<", ">", "<>", "a<b", "a < b > c", "&nbsp", "&nbsp;", "&NBSP;", "&nbsp;&nbsp;x",
	"\v  x", "\t\n\f\r x", "<p>\n</p>", "<p", "x<p>y<", "<<>>", "<a\n href=x>",
	"\xff<\xfe>\xfd", "a\x00b",
	"<p>Sent from my iPhone</p><blockquote>q</blockquote>",
	"<p>Sent from my iPhone Sent from my iPhone</p><div class=\"gmail_quote\">q</div>",
	// (?i) matches U+212A (Kelvin) for k and U+017F (long s) for s; the
	// needles must not rely on either letter
	"<p>hi</p><div>On 5 Apr 2026, a wrote:<br><blocKquote>q</blocKquote>",
	"<p>hi</p><div style=\"border-left: 1px\"><div><div><ſtrong>Von:</strong>",
	"<p>hi</p><DIV STYLE=\"BORDER-TOP: SOLID #E1E1E1 1PT; PADDING: 3PT\">q</DIV>",
	"<p>hi</p><div style=\"border-style: solid none none; padding: 0\">q</div>",
	"<p>hi</p><div \u017Ftyle=\"border-\u017Ftyle: \u017Folid none none\">q</div>",
	"<p>hi</p><HR><div><font><b>From:</b> x",
	"<p>hi</p>-----Urspr&uuml;ngliche Nachricht-----",
	"<p>hi</p>---ORIGINAL MESSAGE---",
	"<p>hi</p><DIV ID=\"divRplyFwdMsg\">q</DIV>",
	"<p>hi</p><div id=\"appleMailSignature\">x</div><div class=\"gmail_quote\">q</div>",
	"<<div class=\"gmail_quote\">", "<div class=\"gmail_quote\"",
	"<p>hi</p>---Original Me\u017F\u017Fage---",
	"<p>hi</p><div style=\"color: #555\"><p>On Mon, x wrote:</p>",
}

func quoteInputs(t testing.TB) []string {
	inputs := append([]string(nil), quoteEdgeCases...)
	files, err := filepath.Glob("testdata/*.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, string(data))
	}
	return inputs
}

// earliestQuotePattern only looks where a tag opens.
func TestQuotePatternsOpenATag(t *testing.T) {
	for _, p := range quotePatterns {
		if !strings.HasPrefix(p, "<") {
			t.Errorf("quote pattern %q doesn't start with '<'", p)
		}
	}
}

func TestCollapseTagsAndSpace_MatchesRegexp(t *testing.T) {
	for _, in := range quoteInputs(t) {
		for _, repl := range []string{"", " "} {
			if got, want := collapseTagsAndSpace(in, repl), refTagOrSpace.ReplaceAllString(in, repl); got != want {
				t.Errorf("collapse(%.60q, %q) = %.60q, regexp gives %.60q", in, repl, got, want)
			}
		}
	}
}

func TestStripQuotedContent_MatchesRegexp(t *testing.T) {
	for _, in := range quoteInputs(t) {
		if got, want := StripQuotedContent(in), refStripQuotedContent(in); got != want {
			t.Errorf("StripQuotedContent(%.60q) = %.60q, regexp version gives %.60q", in, got, want)
		}
	}
}

// go test ./internal/sanitize -run '^$' -fuzz FuzzCollapseTagsAndSpace
func FuzzCollapseTagsAndSpace(f *testing.F) {
	for _, in := range quoteEdgeCases {
		f.Add(in)
	}
	f.Fuzz(func(t *testing.T, in string) {
		for _, repl := range []string{"", " "} {
			if got, want := collapseTagsAndSpace(in, repl), refTagOrSpace.ReplaceAllString(in, repl); got != want {
				t.Errorf("collapse(%q, %q) = %q, regexp gives %q", in, repl, got, want)
			}
		}
	})
}

// go test ./internal/sanitize -run '^$' -fuzz FuzzStripQuotedContent
func FuzzStripQuotedContent(f *testing.F) {
	for _, in := range quoteEdgeCases {
		f.Add(in)
	}
	f.Fuzz(func(t *testing.T, in string) {
		if got, want := StripQuotedContent(in), refStripQuotedContent(in); got != want {
			t.Errorf("StripQuotedContent(%q) = %q, regexp version gives %q", in, got, want)
		}
	})
}

// go test ./internal/sanitize -run '^$' -bench StripQuotedContent
func BenchmarkStripQuotedContent(b *testing.B) {
	inputs := quoteInputs(b)
	for _, c := range []struct {
		name string
		fn   func(string) string
	}{{"regexp", refStripQuotedContent}, {"current", StripQuotedContent}} {
		b.Run(c.name, func(b *testing.B) {
			for b.Loop() {
				for _, in := range inputs {
					c.fn(in)
				}
			}
		})
	}
}
