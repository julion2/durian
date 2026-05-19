package sanitize

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadQuoteFixture reads an HTML test fixture from testdata/.
func loadQuoteFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

// assertStripped verifies that stripping keeps the reply and removes the quoted part.
func assertStripped(t *testing.T, name, input, expectedKeep, expectedRemove string) {
	t.Helper()
	result := StripQuotedContent(input)
	if expectedKeep != "" && !strings.Contains(result, expectedKeep) {
		t.Errorf("[%s] result should contain %q, got: %q", name, expectedKeep, result)
	}
	if expectedRemove != "" && strings.Contains(result, expectedRemove) {
		t.Errorf("[%s] result should NOT contain %q, got: %q", name, expectedRemove, result)
	}
}

// --- Empty / no-op cases ---

func TestStripQuotedContent_Empty(t *testing.T) {
	if StripQuotedContent("") != "" {
		t.Error("empty input should return empty")
	}
}

func TestStripQuotedContent_NoQuotes(t *testing.T) {
	html := `<p>Just a plain email with no quotes.</p>`
	result := StripQuotedContent(html)
	if result != html {
		t.Errorf("unquoted HTML should be unchanged, got: %q", result)
	}
}

func TestStripQuotedContent_PureForwardKept(t *testing.T) {
	// If stripping leaves empty content, the original should be kept
	html := `<blockquote class="gmail_quote">Forwarded content here</blockquote>`
	result := StripQuotedContent(html)
	if !strings.Contains(result, "Forwarded content here") {
		t.Error("pure forward (no reply text) should keep the original")
	}
}

// --- Outlook Web ---

func TestStripQuotedContent_OutlookWeb_ReferenceMessageContainer(t *testing.T) {
	html := `<p>My reply text</p><div id="mail-editor-reference-message-container"><p>Original</p></div>`
	assertStripped(t, "Outlook Web reference container", html, "My reply text", "Original")
}

func TestStripQuotedContent_OutlookWeb_AppendOnSend(t *testing.T) {
	html := `<p>My reply</p><div id="appendonsend"><p>quoted</p></div>`
	assertStripped(t, "Outlook appendonsend", html, "My reply", "quoted")
}

func TestStripQuotedContent_OutlookWeb_DivRplyFwdMsg(t *testing.T) {
	html := `<p>Reply above</p><div id="divRplyFwdMsg"><b>From:</b> Someone</div>`
	assertStripped(t, "Outlook divRplyFwdMsg", html, "Reply above", "Someone")
}

func TestStripQuotedContent_OutlookWeb_DivRplyFwdMsgByName(t *testing.T) {
	html := `<p>Reply</p><div name="divRplyFwdMsg">Original msg</div>`
	assertStripped(t, "Outlook divRplyFwdMsg (name attr)", html, "Reply", "Original msg")
}

// --- Outlook Mobile ---

func TestStripQuotedContent_OutlookMobile(t *testing.T) {
	html := `<p>Sent from iPhone</p><div class="ms-outlook-mobile-reference-message">Original</div>`
	assertStripped(t, "Outlook Mobile", html, "Sent from iPhone", "Original")
}

// --- Gmail ---

func TestStripQuotedContent_GmailQuote(t *testing.T) {
	html := `<p>Hi Alice,</p><div class="gmail_quote"><blockquote>Original email</blockquote></div>`
	assertStripped(t, "gmail_quote div", html, "Hi Alice", "Original email")
}

func TestStripQuotedContent_GmailExtra(t *testing.T) {
	html := `<p>My reply</p><div class="gmail_extra"><br>On Mon wrote:</div>`
	assertStripped(t, "gmail_extra div", html, "My reply", "On Mon wrote")
}

func TestStripQuotedContent_GmailBlockquoteClass(t *testing.T) {
	html := `<p>Thanks!</p><blockquote class="gmail_quote">Previous message</blockquote>`
	assertStripped(t, "gmail_quote blockquote", html, "Thanks", "Previous message")
}

// --- Apple Mail ---

func TestStripQuotedContent_AppleMail(t *testing.T) {
	html := `<p>Cool.</p><blockquote type="cite">Original Apple Mail text</blockquote>`
	assertStripped(t, "Apple Mail cite", html, "Cool", "Original Apple Mail text")
}

// --- Outlook Desktop (regex patterns) ---

func TestStripQuotedContent_OutlookDesktop_BorderTop(t *testing.T) {
	html := `<p>My reply.</p><div style="border:none;border-top:solid #E1E1E1 1.0pt;padding:3.0pt 0cm 0cm 0cm"><b>From:</b> Bob</div>`
	assertStripped(t, "Outlook Desktop border-top", html, "My reply", "Bob")
}

func TestStripQuotedContent_OutlookDesktop_BorderStyle(t *testing.T) {
	html := `<p>Reply</p><div style="padding:3.0pt 0cm 0cm 0cm;border-style:solid none none none"><b>From:</b> Alice</div>`
	assertStripped(t, "Outlook Desktop border-style", html, "Reply", "Alice")
}

func TestStripQuotedContent_OutlookHR_GermanVon(t *testing.T) {
	html := `<p>Hallo Bob,</p><hr><div><b>Von:</b> Alice</div>`
	assertStripped(t, "Outlook HR Von", html, "Hallo Bob", "Alice")
}

func TestStripQuotedContent_OutlookHR_EnglishFrom(t *testing.T) {
	html := `<p>Hi Bob</p><hr><div><b>From:</b> Alice</div>`
	assertStripped(t, "Outlook HR From", html, "Hi Bob", "Alice")
}

// --- GMX / web.de Web (border-left quote box) ---

func TestStripQuotedContent_GMX_BorderLeftGesendet(t *testing.T) {
	html := `<div>Hi Julian,</div>` +
		`<div>der 20.7. passt.</div>` +
		`<div>Viele Grüße, Georg</div>` +
		`<div style="margin: 10px 5px 5px 10px; padding: 10px 0px 10px 10px; border-left: 2px solid rgb(195, 217, 229)">` +
		`<div style="margin: 0px 0px 10px">` +
		`<div><strong>Gesendet: </strong>Montag, 18. Mai 2026 um 21:08</div>` +
		`<div><strong>Von: </strong>Julian Schenker</div>` +
		`</div><div>Quoted reply text here.</div></div>`
	assertStripped(t, "GMX border-left Gesendet", html, "der 20.7. passt", "Quoted reply text here")
}

func TestStripQuotedContent_GMX_BorderLeftVon(t *testing.T) {
	html := `<div>Reply body.</div>` +
		`<div style="border-left: 2px solid rgb(195, 217, 229); padding-left: 10px">` +
		`<div><div><strong>Von: </strong>Alice</div></div>Original content.</div>`
	assertStripped(t, "GMX border-left Von", html, "Reply body", "Original content")
}

// --- Forwarded message separators ---

func TestStripQuotedContent_GermanForward(t *testing.T) {
	html := `<p>Schau mal:</p>---------- Urspr&uuml;ngliche Nachricht ----------<br>Original`
	assertStripped(t, "German forward separator", html, "Schau mal", "Original")
}

func TestStripQuotedContent_GermanForwardUTF8(t *testing.T) {
	html := `<p>Schau mal:</p>---------- Ursprüngliche Nachricht ----------<br>Original`
	assertStripped(t, "German forward separator (UTF-8)", html, "Schau mal", "Original")
}

func TestStripQuotedContent_EnglishForward(t *testing.T) {
	html := `<p>Check this:</p>---------- Original Message ----------<br>Content`
	assertStripped(t, "English forward separator", html, "Check this", "Content")
}

// --- Durian custom format ---

func TestStripQuotedContent_DurianFormat(t *testing.T) {
	html := `<p>Reply text</p><div style="color:#555;"><p>On Jan 1, Alice wrote:</p></div>`
	assertStripped(t, "Durian custom format", html, "Reply text", "Alice wrote")
}

// --- Earliest match wins ---

func TestStripQuotedContent_EarliestMatchWins(t *testing.T) {
	// Gmail quote before Apple Mail blockquote - should cut at Gmail
	html := `<p>My reply</p><div class="gmail_quote">Gmail quoted</div><blockquote type="cite">Apple quoted</blockquote>`
	result := StripQuotedContent(html)
	if strings.Contains(result, "Gmail quoted") {
		t.Error("should cut at earliest match (Gmail)")
	}
	if strings.Contains(result, "Apple quoted") {
		t.Error("should not contain content after earliest match")
	}
	if !strings.Contains(result, "My reply") {
		t.Error("should keep content before earliest match")
	}
}

// --- Generic blockquotes are NOT stripped ---

func TestStripQuotedContent_GenericBlockquoteKept(t *testing.T) {
	// A user includes a legitimate blockquote (e.g. a citation, code snippet).
	// Generic <blockquote> without quote-specific class should NOT be stripped.
	html := `<p>Here is a quote from a book:</p><blockquote>The only way to learn a new programming language is by writing programs in it.</blockquote><p>What do you think?</p>`
	result := StripQuotedContent(html)
	if !strings.Contains(result, "only way to learn") {
		t.Error("legitimate user blockquote should not be stripped")
	}
	if !strings.Contains(result, "What do you think") {
		t.Error("content after legitimate blockquote should not be stripped")
	}
}

// --- Mobile signature detection (real-world Outlook iOS forward bug) ---

func TestStripQuotedContent_OutlookMobileForwardWithSignature(t *testing.T) {
	// User forwards a mail via Outlook iOS — only the auto-signature
	// "Sent from Outlook for iOS" appears above the forward. The forward
	// content should NOT be lost.
	html := `<div><br></div><span>Sent from <a href="https://aka.ms/o0ukef">Outlook for iOS</a></span><div class="ms-outlook-mobile-reference-message"><hr><b>From:</b> Alice<br><b>Subject:</b> Important news<br><p>This is the forwarded content that must not be lost.</p></div>`
	result := StripQuotedContent(html)
	if !strings.Contains(result, "forwarded content that must not be lost") {
		t.Error("Outlook iOS forward with only mobile signature should keep forward content")
	}
}

func TestStripQuotedContent_iPhoneMailForward(t *testing.T) {
	html := `<div>Sent from my iPhone</div><blockquote class="gmail_quote"><p>Forwarded body here</p></blockquote>`
	result := StripQuotedContent(html)
	if !strings.Contains(result, "Forwarded body here") {
		t.Error("'Sent from my iPhone' alone should not cause forward to be stripped")
	}
}

func TestStripQuotedContent_GermaniPhoneForward(t *testing.T) {
	html := `<div>Von meinem iPhone gesendet</div><blockquote class="gmail_quote">Forwarded</blockquote>`
	result := StripQuotedContent(html)
	if !strings.Contains(result, "Forwarded") {
		t.Error("German iPhone signature should be detected as effectively empty")
	}
}

func TestStripQuotedContent_RealUserTextWithMobileSig(t *testing.T) {
	// User wrote actual text PLUS the signature — strip should still work
	html := `<p>Hi Alice, please see below.</p><div>Sent from my iPhone</div><blockquote class="gmail_quote">Original message</blockquote>`
	result := StripQuotedContent(html)
	if !strings.Contains(result, "Hi Alice") {
		t.Error("real user text should be kept")
	}
	if strings.Contains(result, "Original message") {
		t.Error("quoted content should still be stripped when user wrote real text")
	}
}

// --- Edge cases ---

func TestStripQuotedContent_WhitespaceTrimmed(t *testing.T) {
	html := `<p>Reply</p>
	<div class="gmail_quote">quoted</div>`
	result := StripQuotedContent(html)
	if strings.HasSuffix(result, " ") || strings.HasSuffix(result, "\n") || strings.HasSuffix(result, "\t") {
		t.Errorf("trailing whitespace should be trimmed, got: %q", result)
	}
}

func TestStripQuotedContent_CaseInsensitiveStringPatterns(t *testing.T) {
	html := `<p>Reply</p><DIV CLASS="gmail_quote">Original</DIV>`
	result := StripQuotedContent(html)
	if strings.Contains(result, "Original") {
		t.Error("string patterns should be case-insensitive")
	}
}

func TestStripQuotedContent_OnlyWhitespaceAboveQuote(t *testing.T) {
	// If there's only whitespace before the quote, keep the original
	html := `   <blockquote class="gmail_quote">Content</blockquote>`
	result := StripQuotedContent(html)
	if !strings.Contains(result, "Content") {
		t.Error("whitespace-only reply should keep original")
	}
}

// --- Multi-level forwards (current behavior: only first match) ---

func TestStripQuotedContent_MultiLevelForward(t *testing.T) {
	// Reply → forward 1 → forward 2
	// Current behavior: cuts at first forward, loses both
	// After fix: should still cut at first, but this documents current state
	html := `<p>My reply</p><div class="gmail_quote">First forward<div class="gmail_quote">Second forward</div></div>`
	result := StripQuotedContent(html)
	if strings.Contains(result, "First forward") {
		t.Error("should cut at first forward")
	}
	if !strings.Contains(result, "My reply") {
		t.Error("should keep reply")
	}
}

// --- Real-world fixture tests (anonymized) ---

func TestStripQuotedContent_Fixture_OutlookIOSForward(t *testing.T) {
	// Real Outlook iOS forward: user wrote nothing, only "Sent from Outlook for iOS"
	// auto-signature appears above the forward. The forward content must NOT be lost.
	html := loadQuoteFixture(t, "outlook_ios_forward.html")
	result := StripQuotedContent(html)

	// Forward content must be preserved
	mustContain(t, result, "Quarterly Update Q1 2026", "newsletter heading")
	mustContain(t, result, "Annual Award 2026", "section heading")
	mustContain(t, result, "Catalog Entry Deadline", "section heading")
	mustContain(t, result, "newsletter@example.com", "forwarded sender")

	// Length check: should be close to original (only minor whitespace trim possible)
	if len(result) < len(html)-100 {
		t.Errorf("result too short: %d vs original %d (forward content was stripped)", len(result), len(html))
	}
}

func TestStripQuotedContent_Fixture_GmailMobileForward(t *testing.T) {
	// Gmail mobile app forward with nested Outlook "Original Message" inside.
	// User wrote nothing — pure forward, must keep everything.
	html := loadQuoteFixture(t, "gmail_mobile_forward.html")
	result := StripQuotedContent(html)

	// Both forward layers must survive (the isEffectivelyEmpty guard kicks in)
	mustContain(t, result, "Forwarded message", "outer Gmail forward marker")
	mustContain(t, result, "Original Message", "inner Outlook forward marker")
	mustContain(t, result, "Spring Conference 2026", "deepest forwarded subject")
	mustContain(t, result, "Registration deadline", "deepest forwarded body")
}

func TestStripQuotedContent_Fixture_OutlookDesktopGerman(t *testing.T) {
	// Classic Outlook Desktop forward (German): user wrote a real reply,
	// then forward header with border-top:solid style, then forwarded body.
	// User text must be kept, forward must be stripped.
	html := loadQuoteFixture(t, "outlook_desktop_german.html")
	result := StripQuotedContent(html)

	// User reply text must survive
	mustContain(t, result, "Hallo Alice", "user greeting")
	mustContain(t, result, "nächstes Meeting", "user message body")

	// Forwarded content must be stripped
	mustNotContain(t, result, "Industry Report Q1 2026", "forwarded subject")
	mustNotContain(t, result, "Marktwachstum um 12", "forwarded body")
	mustNotContain(t, result, "Charlie Newsletter", "forwarded sender")
}

func TestStripQuotedContent_Fixture_YahooMail(t *testing.T) {
	html := loadQuoteFixture(t, "yahoo_mail_reply.html")
	result := StripQuotedContent(html)

	mustContain(t, result, "Thanks for the update", "user reply")
	mustContain(t, result, "Best,", "user signature")
	mustNotContain(t, result, "phase 1 is complete", "quoted content")
	mustNotContain(t, result, "Friday standup", "quoted content")
}

func TestStripQuotedContent_Fixture_ProtonMail(t *testing.T) {
	html := loadQuoteFixture(t, "protonmail_reply.html")
	result := StripQuotedContent(html)

	mustContain(t, result, "Got it", "user reply")
	mustContain(t, result, "Cheers,", "user signature")
	mustNotContain(t, result, "API rate limit", "quoted content")
	mustNotContain(t, result, "deprecated legacy", "quoted content")
}

func TestStripQuotedContent_Fixture_iCloudMail(t *testing.T) {
	html := loadQuoteFixture(t, "icloud_mail_reply.html")
	result := StripQuotedContent(html)

	mustContain(t, result, "Sounds good", "user reply")
	mustContain(t, result, "book the meeting room", "user reply body")
	mustNotContain(t, result, "Q2 roadmap", "quoted content")
	mustNotContain(t, result, "Tuesday or Wednesday afternoon", "quoted content")
}

func TestStripQuotedContent_Fixture_Thunderbird(t *testing.T) {
	html := loadQuoteFixture(t, "thunderbird_reply.html")
	result := StripQuotedContent(html)

	mustContain(t, result, "danke für die schnelle", "user reply")
	mustContain(t, result, "Viele Grüße", "user signature")
	mustNotContain(t, result, "Race-Condition", "quoted content")
	mustNotContain(t, result, "fix/sync-race", "quoted branch name")
}

func TestStripQuotedContent_Fixture_SparkMail(t *testing.T) {
	html := loadQuoteFixture(t, "spark_mail_reply.html")
	result := StripQuotedContent(html)

	mustContain(t, result, "sounds good", "user reply")
	mustContain(t, result, "Alice Anderson", "user signature")
	mustNotContain(t, result, "Design draft: end of week 1", "quoted content")
	mustNotContain(t, result, "Final handoff", "quoted content")
}

func TestStripQuotedContent_Fixture_SparkForward(t *testing.T) {
	// Spark forward with no user text — should keep entire forward content.
	// Format: "---------- Forwarded message ----------" + From/To/Subject
	// followed by an unclassed <blockquote> containing the forwarded HTML.
	html := loadQuoteFixture(t, "spark_mail_forward.html")
	result := StripQuotedContent(html)

	mustContain(t, result, "Forwarded message", "forward marker")
	mustContain(t, result, "Upgrade your account", "forwarded body heading")
	mustContain(t, result, "Subscription ID", "forwarded body details")
	mustContain(t, result, "noreply@example.com", "forwarded sender")
}

// --- StripQuotedTextContent (plain-text bodies) ---

func assertTextStripped(t *testing.T, name, input, expectedKeep, expectedRemove string) {
	t.Helper()
	result := StripQuotedTextContent(input)
	if expectedKeep != "" && !strings.Contains(result, expectedKeep) {
		t.Errorf("[%s] result should contain %q, got: %q", name, expectedKeep, result)
	}
	if expectedRemove != "" && strings.Contains(result, expectedRemove) {
		t.Errorf("[%s] result should NOT contain %q, got: %q", name, expectedRemove, result)
	}
}

func TestStripQuotedTextContent_GMX_Gesendet(t *testing.T) {
	text := "Hi Julian,\n\nder 20.7. nachmittags würde gut passen.\n\nViele Grüße\nGeorg\n\n" +
		"Gesendet: Montag, 18. Mai 2026 um 21:08\n" +
		"Von: \"Julian Schenker\" <julian@habric.com>\n" +
		"An: dram-hn@gmx.de\n" +
		"Betreff: Re: Treffen heute\n\n" +
		"Hey Georg, danke dir für den Call.\n"
	assertTextStripped(t, "GMX Gesendet", text, "der 20.7. nachmittags", "danke dir für den Call")
}

func TestStripQuotedTextContent_OutlookEnglish(t *testing.T) {
	text := "My reply.\n\nSent: Monday, May 18, 2026 9:08 PM\nFrom: Julian\nTo: Georg\nSubject: Re: Hi\n\nOriginal body."
	assertTextStripped(t, "Outlook English Sent/From", text, "My reply", "Original body")
}

func TestStripQuotedTextContent_OutlookGerman(t *testing.T) {
	text := "Antwort.\n\nVon: Alice\nGesendet: Montag\nAn: Bob\nBetreff: Test\n\nOriginal."
	assertTextStripped(t, "Outlook German Von/Gesendet", text, "Antwort", "Original.")
}

func TestStripQuotedTextContent_OnWrote(t *testing.T) {
	text := "Thanks!\n\nOn 19 May 2026, Alice wrote:\n> Original message body here\n> more\n"
	assertTextStripped(t, "On wrote attribution", text, "Thanks", "Original message body")
}

func TestStripQuotedTextContent_GermanForward(t *testing.T) {
	text := "Schau mal:\n\n--- Ursprüngliche Nachricht ---\nVon: Alice\n\nOriginal text."
	assertTextStripped(t, "German forward separator", text, "Schau mal", "Original text")
}

func TestStripQuotedTextContent_NoQuote(t *testing.T) {
	text := "A short standalone email with no replies or forwards."
	if got := StripQuotedTextContent(text); got != text {
		t.Errorf("unmodified text changed: %q vs %q", got, text)
	}
}

func TestStripQuotedTextContent_OnlyMobileSig(t *testing.T) {
	// If stripping leaves only "Sent from my iPhone" we keep the original
	// so the forwarded body is still visible.
	text := "Sent from my iPhone\n\nOn 19 May 2026, Alice wrote:\nOriginal content."
	got := StripQuotedTextContent(text)
	if !strings.Contains(got, "Original content") {
		t.Errorf("expected fallback to original, got: %q", got)
	}
}

func TestStripQuotedTextContent_Empty(t *testing.T) {
	if got := StripQuotedTextContent(""); got != "" {
		t.Errorf("empty input should return empty, got %q", got)
	}
}

// --- isEmptyHTML ---

func TestIsEmptyHTML(t *testing.T) {
	cases := []struct {
		input string
		empty bool
	}{
		{"", true},
		{"   ", true},
		{"<p></p>", true},
		{"<p>  </p>", true},
		{"<br><br>", true},
		{"&nbsp;&nbsp;", true},
		{"<div><span></span></div>", true},
		{"<p>text</p>", false},
		{"Hello", false},
		{"<p>  x  </p>", false},
	}
	for _, tc := range cases {
		got := isEmptyHTML(tc.input)
		if got != tc.empty {
			t.Errorf("isEmptyHTML(%q) = %v, want %v", tc.input, got, tc.empty)
		}
	}
}
