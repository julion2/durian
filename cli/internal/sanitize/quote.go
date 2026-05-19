package sanitize

import (
	"regexp"
	"strings"
)

// quotePatterns defines HTML patterns that indicate quoted/forwarded content.
var quotePatterns = []string{
	// Outlook Web
	`<div id="mail-editor-reference-message-container"`,
	`<div id="appendonsend"`,
	`<div id="divRplyFwdMsg"`,
	`<div name="divRplyFwdMsg"`,

	// Outlook Mobile
	`<div class="ms-outlook-mobile-reference-message`,

	// Gmail
	`<div class="gmail_quote"`,
	`<div class="gmail_extra"`,
	`<blockquote class="gmail_quote"`,

	// Apple Mail / iCloud
	`<blockquote type="cite"`,
	`<div id="AppleMailSignature"`,

	// Yahoo Mail
	`<div class="yahoo_quoted"`,
	`<div id="yahoo_quoted_`,

	// ProtonMail
	`<div class="protonmail_quote"`,
	`<blockquote class="protonmail_quote"`,

	// Thunderbird
	`<div class="moz-cite-prefix"`,
	`<blockquote cite="mid:`,

	// Spark (uses messageReplySection in some versions)
	`<div name="messageReplySection"`,

	// NOTE: Generic <blockquote> is intentionally NOT in this list.
	// It would strip legitimate user quotes (citations, code, etc.).
}

// mobileSignatures are auto-generated client signatures that should be treated
// as "no real user content" — when only these appear above a quote, the original
// (with the forward/reply intact) is kept.
var mobileSignatures = []string{
	"sent from outlook for ios",
	"sent from outlook for android",
	"sent from my iphone",
	"sent from my ipad",
	"sent from my android",
	"sent from mail for windows",
	"get outlook for ios",
	"get outlook for android",
	"von meinem iphone gesendet",
	"von meinem ipad gesendet",
	"gesendet von outlook für ios",
}

// quoteRegexPatterns defines regex patterns for quoted content that can't be matched
// with simple string patterns (e.g. inline styles with variable values).
var quoteRegexPatterns = []*regexp.Regexp{
	// Outlook Desktop: <div style="border: none; border-top: solid #E1E1E1 1.0pt; padding: ...">
	regexp.MustCompile(`(?i)<div[^>]*style="[^"]*border-top:\s*solid\s[^"]*padding:[^"]*">`),
	// Outlook Desktop variant: padding + border-style: solid none none (either order)
	regexp.MustCompile(`(?i)<div[^>]*style="[^"]*border-style:\s*solid\s+none\s+none[^"]*">`),
	// Outlook: <hr> followed by Von:/From: header block
	regexp.MustCompile(`(?i)<hr[^>]*>\s*<div[^>]*>(?:\s*<font[^>]*>)?\s*(?:<[^>]*>)*\s*<b>(?:Von|From):</b>`),
	// Forwarded message separators (German/English)
	regexp.MustCompile(`(?i)-{3,}\s*Urspr(?:ü|&uuml;|&#xFC;)ngliche Nachricht\s*-{3,}`),
	regexp.MustCompile(`(?i)-{3,}\s*Original Message\s*-{3,}`),

	// Durian: <div style="color: #555;"><p ...>On ..., ... wrote:</p>
	regexp.MustCompile(`(?i)<div[^>]*style="color:\s*#555;?"[^>]*>\s*<p[^>]*>On\s`),

	// Spark / generic "On <date> ... wrote:" line directly followed by a blockquote.
	// Spark uses no distinguishing class/id, but the pattern is reliable:
	// matches "On 5 Apr 2026 ... wrote:<br/><blockquote>" (and similar variants).
	// The opening <div> or <p> wrapping this attribution is the cut point.
	regexp.MustCompile(`(?i)<(?:div|p)[^>]*>\s*On\s+\d[^<]*?wrote:\s*<br[^>]*>\s*<blockquote`),

	// GMX / web.de Web: <div style="margin: ...; padding: ...; border-left: 2px solid rgb(...)">
	// followed by a header block with <strong>Gesendet:</strong> / <strong>Von:</strong>.
	// GMX does not use a stable class/id, but the border-left + Gesendet/Von combination
	// is reliable. We anchor on the opening <div> with border-left so the quote box
	// (including its surrounding margin/padding) is the cut point.
	regexp.MustCompile(`(?is)<div[^>]*style="[^"]*border-left:[^"]*"[^>]*>\s*<div[^>]*>\s*<div[^>]*>\s*<strong>(?:Gesendet|Sent|Von|From):`),
}

// StripQuotedContent removes quoted reply content from HTML.
func StripQuotedContent(html string) string {
	if html == "" {
		return html
	}

	htmlLower := strings.ToLower(html)

	earliestIdx := -1
	for _, pattern := range quotePatterns {
		idx := strings.Index(htmlLower, strings.ToLower(pattern))
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

	stripped := html[:earliestIdx]
	stripped = strings.TrimRight(stripped, " \t\n\r")

	// If stripping leaves only empty HTML or just a mobile signature
	// (e.g. "Sent from Outlook for iOS"), keep the original so the
	// forwarded content remains visible.
	if isEffectivelyEmpty(stripped) {
		return html
	}

	return stripped
}

// textQuotePatterns are regex anchors for the start of a quoted/forwarded
// block in plain-text mail bodies. The earliest match wins; everything
// from the match position onward is treated as quoted content.
//
// We anchor on the BEGINNING OF A LINE so we don't trip over the same words
// appearing inside legitimate prose (e.g. "Betreff dieses Treffens…").
var textQuotePatterns = []*regexp.Regexp{
	// GMX / web.de / many German webmailers: header block of a quoted reply.
	// `Gesendet:` is the most distinctive marker; we accept it followed by
	// optional `Von:`/`An:`/`Betreff:` lines but key on Gesendet alone.
	regexp.MustCompile(`(?m)^[ \t]*Gesendet:\s`),
	// Outlook English plain-text reply header.
	regexp.MustCompile(`(?m)^[ \t]*Sent:\s.*\r?\n[ \t]*(?:From|To|Subject):`),
	// German Outlook plain-text reply header.
	regexp.MustCompile(`(?m)^[ \t]*Von:\s.*\r?\n[ \t]*(?:Gesendet|An|Betreff):`),
	// Forwarded-message separators.
	regexp.MustCompile(`(?im)^[ \t]*-{3,}\s*Urspr(?:ü|ue)ngliche Nachricht\s*-{3,}`),
	regexp.MustCompile(`(?im)^[ \t]*-{3,}\s*Original Message\s*-{3,}`),
	regexp.MustCompile(`(?im)^[ \t]*-{3,}\s*Forwarded message\s*-{3,}`),
	// "On <date>, <name> wrote:" attribution line (Apple Mail, Spark, Gmail).
	regexp.MustCompile(`(?m)^On\s+\w[^\n]{0,200}?\s+wrote:\s*$`),
	// "Am <date> schrieb <name>:" attribution line (Thunderbird/Apple Mail DE).
	regexp.MustCompile(`(?m)^Am\s+\w[^\n]{0,200}?\s+schrieb\s[^\n]{0,200}?:\s*$`),
}

// StripQuotedTextContent removes quoted reply / forwarded content from a
// plain-text mail body. Mirrors StripQuotedContent for HTML.
//
// Like the HTML variant, if stripping leaves only whitespace or a mobile
// signature the original is returned so the user still sees something.
func StripQuotedTextContent(text string) string {
	if text == "" {
		return text
	}

	earliestIdx := -1
	for _, re := range textQuotePatterns {
		loc := re.FindStringIndex(text)
		if loc != nil && (earliestIdx == -1 || loc[0] < earliestIdx) {
			earliestIdx = loc[0]
		}
	}

	if earliestIdx == -1 {
		return text
	}

	stripped := strings.TrimRight(text[:earliestIdx], " \t\n\r")

	textLower := strings.ToLower(strings.TrimSpace(stripped))
	if textLower == "" {
		return text
	}
	for _, sig := range mobileSignatures {
		if strings.Contains(textLower, sig) {
			remainder := strings.TrimSpace(strings.ReplaceAll(textLower, sig, ""))
			if len(remainder) < 5 {
				return text
			}
		}
	}
	return stripped
}

// htmlTagOrSpace matches HTML tags and whitespace.
var htmlTagOrSpace = regexp.MustCompile(`(?:<[^>]*>|\s|&nbsp;)+`)

// isEmptyHTML returns true if the HTML contains no visible text content.
func isEmptyHTML(html string) bool {
	return strings.TrimSpace(htmlTagOrSpace.ReplaceAllString(html, "")) == ""
}

// isEffectivelyEmpty returns true if the HTML contains no meaningful user
// content — either truly empty or only an auto-generated mobile signature.
func isEffectivelyEmpty(html string) bool {
	// Strip all tags and entities to get plain text
	text := htmlTagOrSpace.ReplaceAllString(html, " ")
	text = strings.TrimSpace(text)
	if text == "" {
		return true
	}

	textLower := strings.ToLower(text)
	for _, sig := range mobileSignatures {
		// Check if the text is JUST the signature (with optional surrounding noise)
		if strings.Contains(textLower, sig) {
			// Remove the signature and check if anything substantive remains
			remainder := strings.ReplaceAll(textLower, sig, "")
			remainder = strings.TrimSpace(remainder)
			if len(remainder) < 5 {
				return true
			}
		}
	}
	return false
}
