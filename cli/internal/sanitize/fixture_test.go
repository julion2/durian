package sanitize

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestVoidElementsDoNotEatContent verifies that void elements (meta, link, base)
// are stripped without swallowing subsequent HTML content.
// Regression test: SkipElementsContent on void elements caused bluemonday to eat
// everything after the first <meta> because void elements never produce a closing
// tag token from Go's html.Tokenizer.
func TestVoidElementsDoNotEatContent(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			"meta then content",
			`<meta charset="UTF-8"><p>visible</p>`,
			"visible",
		},
		{
			"link then content",
			`<link rel="stylesheet" href="x"><p>visible</p>`,
			"visible",
		},
		{
			"base then content",
			`<base href="x"><p>visible</p>`,
			"visible",
		},
		{
			"full email structure",
			`<html><head><meta charset="UTF-8"><link href="x"></head><body><table><tr><td>data</td></tr></table><a href="https://example.com">Click</a></body></html>`,
			"data",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SanitizeHTML(tt.input)
			if !strings.Contains(result, tt.want) {
				t.Errorf("Expected %q in output, got: %s", tt.want, result)
			}
		})
	}
}

// --- helpers for structural checks ---

func mustContain(t *testing.T, result, pattern, label string) {
	t.Helper()
	if !strings.Contains(strings.ToLower(result), strings.ToLower(pattern)) {
		t.Errorf("must survive: %s (%q not found)", label, pattern)
	}
}

func mustNotContain(t *testing.T, result, pattern, label string) {
	t.Helper()
	if strings.Contains(strings.ToLower(result), strings.ToLower(pattern)) {
		t.Errorf("must die: %s (%q found)", label, pattern)
	}
}

func mustMatch(t *testing.T, result string, re *regexp.Regexp, label string) {
	t.Helper()
	if !re.MatchString(result) {
		t.Errorf("must survive: %s (regex %s not matched)", label, re.String())
	}
}

// assertMustDie checks elements that must never survive sanitization.
func assertMustDie(t *testing.T, result string) {
	t.Helper()
	mustNotContain(t, result, "<script", "script tag")
	mustNotContain(t, result, "<iframe", "iframe tag")
	mustNotContain(t, result, "<object", "object tag")
	mustNotContain(t, result, "<embed", "embed tag")
	mustNotContain(t, result, "<form", "form tag")
	mustNotContain(t, result, "<input", "input tag")
	mustNotContain(t, result, "<textarea", "textarea tag")
	mustNotContain(t, result, "<select", "select tag")
	mustNotContain(t, result, "<button", "button tag")
	mustNotContain(t, result, "<link ", "link tag")
	mustNotContain(t, result, "<base ", "base tag")
	mustNotContain(t, result, "<meta ", "meta tag")
	mustNotContain(t, result, "javascript:", "javascript: URL")

	// on* event handlers
	onHandler := regexp.MustCompile(`(?i)\bon\w+=`)
	if onHandler.MatchString(result) {
		t.Errorf("must die: on* event handler found")
	}
}

// --- compiled regexes for attribute checks ---

var (
	reStyleBlock  = regexp.MustCompile(`(?i)<style[^>]*>[\s\S]+?</style>`)
	reInlineStyle = regexp.MustCompile(`(?i)style="[^"]+?"`)
	reClassAttr   = regexp.MustCompile(`(?i)class="[^"]*?"`)
	reCellpadding = regexp.MustCompile(`(?i)cellpadding="[^"]*?"`)
	reCellspacing = regexp.MustCompile(`(?i)cellspacing="[^"]*?"`)
	reBorderAttr  = regexp.MustCompile(`(?i)\bborder="[^"]*?"`)
	reWidthAttr   = regexp.MustCompile(`(?i)\bwidth="[^"]*?"`)
	reHeightAttr  = regexp.MustCompile(`(?i)\bheight="[^"]*?"`)
	reBgcolor     = regexp.MustCompile(`(?i)bgcolor="[^"]*?"`)
	reAlignAttr   = regexp.MustCompile(`(?i)\balign="[^"]*?"`)
	reValignAttr  = regexp.MustCompile(`(?i)valign="[^"]*?"`)
	reColspan     = regexp.MustCompile(`(?i)colspan="[^"]*?"`)
	reFontColor   = regexp.MustCompile(`(?i)<font[^>]+color=`)
	reFontFace    = regexp.MustCompile(`(?i)<font[^>]+face=`)
	reFontSize    = regexp.MustCompile(`(?i)<font[^>]+size=`)
	reAHref       = regexp.MustCompile(`(?i)<a [^>]*href="[^"]*?"`)
	reImgSrc      = regexp.MustCompile(`(?i)<img [^>]*src="[^"]*?"`)
	reImgAlt      = regexp.MustCompile(`(?i)<img [^>]*alt="`)
	reImgWidth    = regexp.MustCompile(`(?i)<img [^>]*width="`)
	reImgHeight   = regexp.MustCompile(`(?i)<img [^>]*height="`)
	reRoleAttr    = regexp.MustCompile(`(?i)role="[^"]*?"`)
	reMSOComment  = regexp.MustCompile(`<!--\[if`)
	reDirAttr     = regexp.MustCompile(`(?i)dir="[^"]*?"`)
	reLangAttr    = regexp.MustCompile(`(?i)lang="[^"]*?"`)
)

type tableAttrs struct {
	th, cellpadding, cellspacing, border  bool
	width, height, bgcolor, align, valign bool
	colspan                               bool
}

// assertTableLayout checks table structure and layout attributes.
func assertTableLayout(t *testing.T, result string, attrs tableAttrs) {
	t.Helper()
	mustContain(t, result, "<table", "table tag")
	mustContain(t, result, "<tr", "tr tag")
	mustContain(t, result, "<td", "td tag")
	if attrs.th {
		mustContain(t, result, "<th", "th tag")
	}
	if attrs.cellpadding {
		mustMatch(t, result, reCellpadding, "cellpadding attr")
	}
	if attrs.cellspacing {
		mustMatch(t, result, reCellspacing, "cellspacing attr")
	}
	if attrs.border {
		mustMatch(t, result, reBorderAttr, "border attr")
	}
	if attrs.width {
		mustMatch(t, result, reWidthAttr, "width attr")
	}
	if attrs.height {
		mustMatch(t, result, reHeightAttr, "height attr")
	}
	if attrs.bgcolor {
		mustMatch(t, result, reBgcolor, "bgcolor attr")
	}
	if attrs.align {
		mustMatch(t, result, reAlignAttr, "align attr")
	}
	if attrs.valign {
		mustMatch(t, result, reValignAttr, "valign attr")
	}
	if attrs.colspan {
		mustMatch(t, result, reColspan, "colspan attr")
	}
}

func loadFixture(t *testing.T, name string) (input, output string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name+".html"))
	if err != nil {
		t.Fatalf("Failed to read fixture %s: %v", name, err)
	}
	input = string(raw)
	output = SanitizeHTML(input)
	t.Logf("%d → %d chars (%.1f%%)", len(input), len(output),
		float64(len(output))/float64(len(input))*100)
	return
}

// --- per-fixture tests ---

func TestFixture_AppleMailNewsletter(t *testing.T) {
	_, result := loadFixture(t, "apple_mail_newsletter")

	assertMustDie(t, result)
	assertTableLayout(t, result, tableAttrs{
		cellpadding: true, cellspacing: true, border: true,
		width: true, height: true, bgcolor: true, align: true, valign: true,
	})

	// Style
	mustMatch(t, result, reStyleBlock, "style block")
	mustMatch(t, result, reInlineStyle, "inline style")
	mustMatch(t, result, reClassAttr, "class attr")

	// Legacy
	mustMatch(t, result, reFontColor, "font color")
	mustMatch(t, result, reFontFace, "font face")
	mustMatch(t, result, reFontSize, "font size")
	mustContain(t, result, "<center", "center tag")

	// Links & images
	mustMatch(t, result, reAHref, "a href")
	mustMatch(t, result, reImgSrc, "img src")
	mustMatch(t, result, reImgAlt, "img alt")
	mustMatch(t, result, reImgWidth, "img width")
	mustMatch(t, result, reImgHeight, "img height")

	// Text & structure
	mustContain(t, result, "<b>", "bold tag")
	mustContain(t, result, "<br", "br tag")
	mustContain(t, result, "<div", "div tag")

	// Attributes
	mustMatch(t, result, reRoleAttr, "role attr")

	// Comments
	mustMatch(t, result, reMSOComment, "MSO conditional comment")
}

func TestFixture_CIBuildNotification(t *testing.T) {
	_, result := loadFixture(t, "ci_build_notification")

	assertMustDie(t, result)
	assertTableLayout(t, result, tableAttrs{
		th: true, cellpadding: true, cellspacing: true, border: true,
		width: true, bgcolor: true, align: true, valign: true,
	})

	// Style
	mustMatch(t, result, reStyleBlock, "style block")
	mustMatch(t, result, reInlineStyle, "inline style")
	mustMatch(t, result, reClassAttr, "class attr")

	// Links & images
	mustMatch(t, result, reAHref, "a href")
	mustMatch(t, result, reImgSrc, "img src")
	mustMatch(t, result, reImgAlt, "img alt")
	mustMatch(t, result, reImgWidth, "img width")
	mustMatch(t, result, reImgHeight, "img height")

	// Text elements
	mustContain(t, result, "<h2", "h2 tag")
	mustContain(t, result, "<h3", "h3 tag")
	mustContain(t, result, "<p", "p tag")
	mustContain(t, result, "<code", "code tag")
	mustContain(t, result, "<pre", "pre tag")
	mustContain(t, result, "<em", "em tag")

	// Structure
	mustContain(t, result, "<br", "br tag")
	mustContain(t, result, "<hr", "hr tag")

	// Attributes
	mustMatch(t, result, reRoleAttr, "role attr")
}

func TestFixture_DarkmodeDigest(t *testing.T) {
	_, result := loadFixture(t, "darkmode_digest")

	assertMustDie(t, result)
	assertTableLayout(t, result, tableAttrs{
		cellpadding: true, cellspacing: true, border: true,
		width: true, height: true, bgcolor: true, align: true, valign: true,
	})

	// Style
	mustMatch(t, result, reStyleBlock, "style block")
	mustMatch(t, result, reInlineStyle, "inline style")
	mustMatch(t, result, reClassAttr, "class attr")

	// Links & images
	mustMatch(t, result, reAHref, "a href")
	mustMatch(t, result, reImgSrc, "img src")
	mustMatch(t, result, reImgAlt, "img alt")
	mustMatch(t, result, reImgWidth, "img width")
	mustMatch(t, result, reImgHeight, "img height")

	// Text & structure
	mustContain(t, result, "<span", "span tag")
	mustContain(t, result, "<br", "br tag")
	mustContain(t, result, "<div", "div tag")

	// Attributes
	mustMatch(t, result, reLangAttr, "lang attr")
	mustMatch(t, result, reDirAttr, "dir attr")
	mustMatch(t, result, reRoleAttr, "role attr")

	// Comments
	mustMatch(t, result, reMSOComment, "MSO conditional comment")
}

func TestFixture_ExampleNewsletter(t *testing.T) {
	_, result := loadFixture(t, "example_newsletter")

	assertMustDie(t, result)
	assertTableLayout(t, result, tableAttrs{
		cellpadding: true, cellspacing: true, border: true,
		width: true, height: true, bgcolor: true, align: true, valign: true,
	})

	// Style
	mustMatch(t, result, reStyleBlock, "style block")
	mustMatch(t, result, reInlineStyle, "inline style")
	mustMatch(t, result, reClassAttr, "class attr")
	mustNotContain(t, result, "@import", "@import in CSS")

	// Table groups
	mustContain(t, result, "<tbody", "tbody tag")

	// Links & images
	mustMatch(t, result, reAHref, "a href")
	mustMatch(t, result, reImgSrc, "img src")
	mustMatch(t, result, reImgAlt, "img alt")
	mustMatch(t, result, reImgWidth, "img width")
	mustMatch(t, result, reImgHeight, "img height")

	// Text elements
	mustContain(t, result, "<h1", "h1 tag")
	mustContain(t, result, "<p", "p tag")
	mustContain(t, result, "<b>", "bold tag")
	mustContain(t, result, "<span", "span tag")

	// Structure
	mustContain(t, result, "<div", "div tag")

	// Attributes
	mustMatch(t, result, reRoleAttr, "role attr")

	// Comments
	mustMatch(t, result, reMSOComment, "MSO conditional comment")
}

func TestFixture_GmailForwardedChain(t *testing.T) {
	_, result := loadFixture(t, "gmail_forwarded_chain")

	assertMustDie(t, result)
	assertTableLayout(t, result, tableAttrs{
		th: true, cellpadding: true, cellspacing: true, border: true,
		align: true, bgcolor: true,
	})

	// Style (inline only, no style blocks)
	mustMatch(t, result, reInlineStyle, "inline style")
	mustMatch(t, result, reClassAttr, "class attr")

	// Table groups
	mustContain(t, result, "<thead", "thead tag")
	mustContain(t, result, "<tbody", "tbody tag")
	mustContain(t, result, "<colgroup", "colgroup tag")
	mustContain(t, result, "<col ", "col tag")

	// Links
	mustMatch(t, result, reAHref, "a href")

	// Text elements
	mustContain(t, result, "<p", "p tag")
	mustContain(t, result, "<b>", "bold tag")
	mustContain(t, result, "<i>", "italic tag")
	mustContain(t, result, "<strong", "strong tag")
	mustContain(t, result, "<blockquote", "blockquote tag")

	// Structure
	mustContain(t, result, "<div", "div tag")
	mustContain(t, result, "<br", "br tag")
	mustContain(t, result, "<ul", "ul tag")
	mustContain(t, result, "<li", "li tag")

	// Attributes
	mustMatch(t, result, reDirAttr, "dir attr")
}

func TestFixture_LegacyMarketingSale(t *testing.T) {
	_, result := loadFixture(t, "legacy_marketing_sale")

	assertMustDie(t, result)
	assertTableLayout(t, result, tableAttrs{
		cellpadding: true, cellspacing: true, border: true,
		width: true, height: true, bgcolor: true, align: true, valign: true,
		colspan: true,
	})

	// Style
	mustMatch(t, result, reStyleBlock, "style block")
	mustMatch(t, result, reInlineStyle, "inline style")

	// Legacy
	mustMatch(t, result, reFontColor, "font color")
	mustMatch(t, result, reFontFace, "font face")
	mustMatch(t, result, reFontSize, "font size")
	mustContain(t, result, "<center", "center tag")

	// Links & images
	mustMatch(t, result, reAHref, "a href")
	mustMatch(t, result, reImgSrc, "img src")
	mustMatch(t, result, reImgAlt, "img alt")
	mustMatch(t, result, reImgWidth, "img width")
	mustMatch(t, result, reImgHeight, "img height")

	// Text
	mustContain(t, result, "<b>", "bold tag")
	mustContain(t, result, "<s>", "strikethrough tag")

	// Structure
	mustContain(t, result, "<div", "div tag")
	mustContain(t, result, "<br", "br tag")

	// Comments
	mustMatch(t, result, reMSOComment, "MSO conditional comment")
}

func TestFixture_OutlookOrderConfirmation(t *testing.T) {
	_, result := loadFixture(t, "outlook_order_confirmation")

	assertMustDie(t, result)
	assertTableLayout(t, result, tableAttrs{
		th: true, cellpadding: true, cellspacing: true, border: true,
		width: true, bgcolor: true, align: true, valign: true, colspan: true,
	})

	// Style
	mustMatch(t, result, reStyleBlock, "style block")
	mustMatch(t, result, reInlineStyle, "inline style")
	mustMatch(t, result, reClassAttr, "class attr")

	// Links & images
	mustMatch(t, result, reAHref, "a href")
	mustMatch(t, result, reImgSrc, "img src")
	mustMatch(t, result, reImgAlt, "img alt")
	mustMatch(t, result, reImgWidth, "img width")
	mustMatch(t, result, reImgHeight, "img height")

	// Text elements
	mustContain(t, result, "<h1", "h1 tag")
	mustContain(t, result, "<h3", "h3 tag")
	mustContain(t, result, "<p", "p tag")
	mustContain(t, result, "<strong", "strong tag")
	mustContain(t, result, "<span", "span tag")

	// Structure
	mustContain(t, result, "<div", "div tag")
	mustContain(t, result, "<br", "br tag")

	// Attributes
	mustMatch(t, result, reRoleAttr, "role attr")

	// Comments
	mustMatch(t, result, reMSOComment, "MSO conditional comment")
}

// TestAllFixturesPresent ensures no fixture file is forgotten — if someone adds
// a new .html to testdata/ they must also add a TestFixture_* function.
func TestAllFixturesPresent(t *testing.T) {
	files, err := filepath.Glob("testdata/*.html")
	if err != nil {
		t.Fatalf("Failed to glob testdata: %v", err)
	}

	known := map[string]bool{
		"apple_mail_newsletter":      true,
		"ci_build_notification":      true,
		"darkmode_digest":            true,
		"example_newsletter":         true,
		"gmail_forwarded_chain":      true,
		"gmail_mobile_forward":       true, // quote_test.go: Fixture_GmailMobileForward
		"icloud_mail_reply":          true, // quote_test.go: Fixture_iCloudMail
		"legacy_marketing_sale":      true,
		"outlook_desktop_german":     true, // quote_test.go: Fixture_OutlookDesktopGerman
		"outlook_ios_forward":        true, // quote_test.go: Fixture_OutlookIOSForward
		"outlook_order_confirmation": true,
		"protonmail_reply":           true, // quote_test.go: Fixture_ProtonMail
		"spark_mail_forward":         true, // quote_test.go: Fixture_SparkForward
		"spark_mail_reply":           true, // quote_test.go: Fixture_SparkMail
		"thunderbird_reply":          true, // quote_test.go: Fixture_Thunderbird
		"yahoo_mail_reply":           true, // quote_test.go: Fixture_YahooMail
	}

	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".html")
		if !known[name] {
			t.Errorf("Fixture %q has no dedicated TestFixture_* function — add structural assertions", name)
		}
	}

	for name := range known {
		path := filepath.Join("testdata", fmt.Sprintf("%s.html", name))
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("Expected fixture file %s missing", path)
		}
	}
}
