package sanitize

import (
	"strings"
	"testing"
)

func TestSanitizeHTMLEmpty(t *testing.T) {
	if got := SanitizeHTML(""); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestAllowedTagsPreserved(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"paragraph", "<p>Hello</p>", "<p>Hello</p>"},
		{"bold+italic", "<b>bold</b> and <em>italic</em>", "<b>bold</b> and <em>italic</em>"},
		{"heading", "<h1>Title</h1>", "<h1>Title</h1>"},
		{"list", "<ul><li>item</li></ul>", "<ul><li>item</li></ul>"},
		{"blockquote", "<blockquote>quote</blockquote>", "<blockquote>quote</blockquote>"},
		{"preformatted", "<pre><code>x := 1</code></pre>", "<pre><code>x := 1</code></pre>"},
		{"line break", "line<br>break", "line<br>break"},
		{"horizontal rule", "<hr>", "<hr>"},
		{"table", "<table><tr><td>cell</td></tr></table>", "<table><tr><td>cell</td></tr></table>"},
		{"sub+sup", "H<sub>2</sub>O x<sup>2</sup>", "H<sub>2</sub>O x<sup>2</sup>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeHTML(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestScriptExecutionVectorsRemoved(t *testing.T) {
	tests := []struct {
		name  string
		input string
		must  string // must NOT appear in output
	}{
		{"script tag", `<script>alert('xss')</script>`, "script"},
		{"script content", `<p>ok</p><script>alert(1)</script>`, "alert"},
		{"iframe", `<iframe src="http://evil.com"></iframe>`, "iframe"},
		{"object", `<object data="evil.swf"></object>`, "object"},
		{"embed", `<embed src="evil.swf">`, "embed"},
		{"base tag", `<base href="http://evil.com/">`, "base"},
		{"meta refresh", `<meta http-equiv="refresh" content="0;url=http://evil.com">`, "meta"},
		{"link tag", `<link rel="stylesheet" href="http://evil.com/style.css">`, "link"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeHTML(tt.input)
			if strings.Contains(strings.ToLower(got), tt.must) {
				t.Errorf("output should not contain %q, got %q", tt.must, got)
			}
		})
	}
}

func TestEventHandlersStripped(t *testing.T) {
	tests := []string{
		`<div onclick="alert(1)">click</div>`,
		`<img src="x" onerror="alert(1)">`,
		`<p onmouseover="alert(1)">hover</p>`,
		`<body onload="alert(1)">text</body>`,
		`<a href="#" onfocus="alert(1)">link</a>`,
	}
	for _, input := range tests {
		got := SanitizeHTML(input)
		if strings.Contains(strings.ToLower(got), "on") && strings.Contains(got, "alert") {
			t.Errorf("event handler not stripped from %q, got %q", input, got)
		}
	}
}

func TestJavascriptURIsBlocked(t *testing.T) {
	tests := []string{
		`<a href="javascript:alert(1)">click</a>`,
		`<a href="JAVASCRIPT:alert(1)">click</a>`,
		`<a href="javascript:void(0)">click</a>`,
		`<a href="	javascript:alert(1)">click</a>`,
	}
	for _, input := range tests {
		got := SanitizeHTML(input)
		if strings.Contains(strings.ToLower(got), "javascript") {
			t.Errorf("javascript URI not blocked in %q, got %q", input, got)
		}
	}
}

func TestImageSrcRestrictions(t *testing.T) {
	// 1x1 pixel valid base64 images for each format
	pngB64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8/5+hHgAHggJ/PchI7wAAAABJRU5ErkJggg=="
	jpegB64 := "/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAgGBgcGBQgHBwcJCQgKDBQNDAsLDBkSEw8UHRofHh0aHBwgJC4nICIsIxwcKDcpLDAxNDQ0Hyc5PTgyPC4zNDL/2wBDAQkJCQwLDBgNDRgyIRwhMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjL/wAARCAABAAEDASIAAhEBAxEB/8QAHwAAAQUBAQEBAQEAAAAAAAAAAAECAwQFBgcICQoL/8QAFBABAAAAAAAAAAAAAAAAAAAACf/EABQRAQAAAAAAAAAAAAAAAAAAAAD/2gAMAwEAAhEDEQA/AKgA/9k="
	gifB64 := "R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7"

	tests := []struct {
		name    string
		input   string
		allowed bool
	}{
		{"data png", `<img src="data:image/png;base64,` + pngB64 + `">`, true},
		{"data jpeg", `<img src="data:image/jpeg;base64,` + jpegB64 + `">`, true},
		{"data gif", `<img src="data:image/gif;base64,` + gifB64 + `">`, true},
		{"data svg blocked", `<img src="data:image/svg+xml;base64,` + pngB64 + `">`, false},
		{"data SVG mixed case", `<img src="data:image/SVG+XML;base64,` + pngB64 + `">`, false},
		{"data svg mixed case 2", `<img src="data:image/Svg+Xml;base64,` + pngB64 + `">`, false},
		{"cid reference", `<img src="cid:image001@01D12345.6789ABCD">`, true},
		{"https url", `<img src="https://example.com/img.png">`, true},
		{"http url", `<img src="http://example.com/img.png">`, true},
		{"data text/html blocked", `<img src="data:text/html,<script>alert(1)</script>">`, false},
		{"javascript blocked", `<img src="javascript:alert(1)">`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeHTML(tt.input)
			hasSrc := strings.Contains(got, "src=")
			if tt.allowed && !hasSrc {
				t.Errorf("expected src to be allowed, got %q", got)
			}
			if !tt.allowed && hasSrc {
				t.Errorf("expected src to be blocked, got %q", got)
			}
		})
	}
}

func TestCSSPropertyWhitelist(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		allowed bool
	}{
		{"color allowed", `<p style="color: red">text</p>`, true},
		{"background-color", `<p style="background-color: #fff">text</p>`, true},
		{"font-size", `<span style="font-size: 14px">text</span>`, true},
		{"text-align", `<p style="text-align: center">text</p>`, true},
		{"margin", `<p style="margin: 10px">text</p>`, true},
		{"display allowed", `<p style="display: none">hidden</p>`, true},
		{"position allowed", `<p style="position: absolute">text</p>`, true},
		{"width allowed", `<p style="width: 9999px">text</p>`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeHTML(tt.input)
			hasStyle := strings.Contains(got, "style=")
			if tt.allowed && !hasStyle {
				t.Errorf("expected style to be preserved, got %q", got)
			}
			if !tt.allowed && hasStyle {
				t.Errorf("expected style to be stripped, got %q", got)
			}
		})
	}
}

func TestCSSExpressionBlocked(t *testing.T) {
	got := SanitizeHTML(`<p style="color: expression(alert(1))">text</p>`)
	if strings.Contains(got, "expression") {
		t.Errorf("expression() should be blocked, got %q", got)
	}
}

func TestCSSUrlAllowed(t *testing.T) {
	// url() is safe under CSP default-src 'none' — resource loading is blocked
	got := SanitizeHTML(`<p style="background-image: url(https://example.com/bg.png)">text</p>`)
	if !strings.Contains(got, "url(") {
		t.Errorf("url() should be allowed (CSP blocks loading), got %q", got)
	}
}

func TestXSSPayloads(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"img onerror", `<img src=x onerror=alert(1)>`},
		{"svg onload", `<svg/onload=alert(1)>`},
		{"script in attribute", `<div style="background:url('javascript:alert(1)')">x</div>`},
		{"data uri xss", `<a href="data:text/html,<script>alert(1)</script>">click</a>`},
		{"nested script", `<p><scr<script>ipt>alert(1)</scr</script>ipt></p>`},
		{"null byte", "<img src=\"java\x00script:alert(1)\">"},
		{"entity encoding", `<a href="&#106;avascript:alert(1)">click</a>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeHTML(tt.input)
			lower := strings.ToLower(got)
			if strings.Contains(lower, "alert") && (strings.Contains(lower, "onerror") ||
				strings.Contains(lower, "onload") ||
				strings.Contains(lower, "javascript") ||
				strings.Contains(lower, "<script")) {
				t.Errorf("XSS payload not sanitized: %q → %q", tt.input, got)
			}
		})
	}
}

func TestIdAttributeBlocked(t *testing.T) {
	got := SanitizeHTML(`<div id="clobbered">text</div>`)
	if strings.Contains(got, "id=") {
		t.Errorf("id attribute should be blocked (DOM clobbering), got %q", got)
	}
}

func TestLinkSafety(t *testing.T) {
	got := SanitizeHTML(`<a href="https://example.com">link</a>`)
	if !strings.Contains(got, "nofollow") {
		t.Errorf("expected rel=nofollow on external link, got %q", got)
	}
}

func TestTableAttributes(t *testing.T) {
	got := SanitizeHTML(`<table><tr><td colspan="2" rowspan="3">cell</td></tr></table>`)
	if !strings.Contains(got, `colspan="2"`) || !strings.Contains(got, `rowspan="3"`) {
		t.Errorf("expected colspan/rowspan preserved, got %q", got)
	}
}

func TestFontTagPreserved(t *testing.T) {
	got := SanitizeHTML(`<font color="red" face="Arial" size="3">text</font>`)
	if !strings.Contains(got, "<font") {
		t.Errorf("font tag should be preserved, got %q", got)
	}
	if !strings.Contains(got, `color="red"`) {
		t.Errorf("font color should be preserved, got %q", got)
	}
	if !strings.Contains(got, `face="Arial"`) {
		t.Errorf("font face should be preserved, got %q", got)
	}
	if !strings.Contains(got, "text") {
		t.Errorf("font content should be preserved, got %q", got)
	}
}

func TestStyleTagPreservation(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantCSS    string // must appear in output
		notWantCSS string // must NOT appear in output (empty = skip check)
	}{
		{
			"safe CSS preserved",
			`<style>.header { color: red; font-size: 14px; }</style><p>text</p>`,
			".header { color: red; font-size: 14px; }",
			"",
		},
		{
			"@import stripped",
			"<style>@import url('https://evil.com/track.css');\n.body { margin: 0; }</style>",
			".body { margin: 0; }",
			"@import",
		},
		{
			"@IMPORT case insensitive",
			"<style>@IMPORT 'tracker.css';\np { color: blue; }</style>",
			"p { color: blue; }",
			"@import",
		},
		{
			"multiple style blocks",
			`<style>.a { color: red; }</style><p>mid</p><style>.b { color: blue; }</style>`,
			".b { color: blue; }",
			"",
		},
		{
			"empty style after cleaning",
			"<style>@import url('evil.css');</style><p>text</p>",
			"<p>text</p>",
			"<style>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeHTML(tt.input)
			if !strings.Contains(got, tt.wantCSS) {
				t.Errorf("expected output to contain %q, got %q", tt.wantCSS, got)
			}
			if tt.notWantCSS != "" && strings.Contains(strings.ToLower(got), strings.ToLower(tt.notWantCSS)) {
				t.Errorf("output should not contain %q, got %q", tt.notWantCSS, got)
			}
		})
	}

	// Verify first block also preserved in multi-block test
	got := SanitizeHTML(`<style>.a { color: red; }</style><p>mid</p><style>.b { color: blue; }</style>`)
	if !strings.Contains(got, ".a { color: red; }") {
		t.Errorf("first style block not preserved, got %q", got)
	}
}

func TestTableCellAttributes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"td width", `<table><tr><td width="50%">x</td></tr></table>`, `width="50%"`},
		{"th height", `<table><tr><th height="30">x</th></tr></table>`, `height="30"`},
		{"td valign", `<table><tr><td valign="top">x</td></tr></table>`, `valign="top"`},
		{"td valign middle", `<table><tr><td valign="middle">x</td></tr></table>`, `valign="middle"`},
		{"table cellpadding", `<table cellpadding="5"><tr><td>x</td></tr></table>`, `cellpadding="5"`},
		{"table cellspacing", `<table cellspacing="0"><tr><td>x</td></tr></table>`, `cellspacing="0"`},
		{"table border", `<table border="1"><tr><td>x</td></tr></table>`, `border="1"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeHTML(tt.input)
			if !strings.Contains(got, tt.want) {
				t.Errorf("expected %q in output, got %q", tt.want, got)
			}
		})
	}
}

func TestRealWorldEmailSanitization(t *testing.T) {
	t.Run("gmail style email", func(t *testing.T) {
		input := `<html><head><style>
.gmail_default { font-family: Arial; font-size: 14px; }
table.container { width: 100%; }
td.content { padding: 20px; }
</style></head><body>
<table class="container"><tr><td class="content">
<p class="gmail_default">Hello world</p>
</td></tr></table>
</body></html>`
		got := SanitizeHTML(input)
		if !strings.Contains(got, ".gmail_default") {
			t.Errorf("gmail style class not preserved, got %q", got)
		}
		if !strings.Contains(got, "<table") {
			t.Errorf("table structure not preserved, got %q", got)
		}
		if !strings.Contains(got, "Hello world") {
			t.Errorf("body text not preserved, got %q", got)
		}
	})

	t.Run("outlook inline styles with td width", func(t *testing.T) {
		input := `<table><tr>
<td width="600" style="padding: 10px; font-family: Arial">
<p style="color: #333; font-size: 14px">Newsletter content</p>
</td></tr></table>`
		got := SanitizeHTML(input)
		if !strings.Contains(got, `width="600"`) {
			t.Errorf("td width not preserved, got %q", got)
		}
		if !strings.Contains(got, "padding:") || !strings.Contains(got, "font-family:") {
			t.Errorf("inline styles not preserved, got %q", got)
		}
		if !strings.Contains(got, "Newsletter content") {
			t.Errorf("content not preserved, got %q", got)
		}
	})

	t.Run("newsletter with media queries", func(t *testing.T) {
		input := `<style>
@media only screen and (max-width: 600px) {
  .container { width: 100% !important; }
  .mobile-hide { display: none; }
}
.header { background-color: #f0f0f0; }
</style>
<table class="container"><tr><td>
<div class="header"><p>News</p></div>
</td></tr></table>`
		got := SanitizeHTML(input)
		if !strings.Contains(got, "@media") {
			t.Errorf("media query not preserved, got %q", got)
		}
		if !strings.Contains(got, ".header") {
			t.Errorf("header style not preserved, got %q", got)
		}
		if !strings.Contains(got, "<table") {
			t.Errorf("table not preserved, got %q", got)
		}
	})

	t.Run("email with import stripped but rest preserved", func(t *testing.T) {
		input := `<style>
@import url('https://fonts.googleapis.com/css?family=Roboto');
.body { font-family: 'Roboto', sans-serif; margin: 0; }
h1 { color: #333; }
</style>
<h1>Welcome</h1>
<p class="body">Content here</p>`
		got := SanitizeHTML(input)
		if strings.Contains(got, "@import") {
			t.Errorf("@import not stripped, got %q", got)
		}
		if !strings.Contains(got, ".body") {
			t.Errorf("body style not preserved, got %q", got)
		}
		if !strings.Contains(got, "Welcome") {
			t.Errorf("heading not preserved, got %q", got)
		}
	})
}
