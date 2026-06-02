package smtp

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Message represents an email message to be sent
type Message struct {
	From        string
	To          []string
	CC          []string
	BCC         []string
	Subject     string
	Body        string
	IsHTML      bool
	InReplyTo   string // Message-ID of the message being replied to
	References  string // Space-separated list of Message-IDs in the thread
	Attachments []Attachment
	// GeneratedMessageID is populated by Build() on first call and reused on
	// subsequent calls so that SMTP send and IMAP append share the same ID.
	GeneratedMessageID string
}

// Attachment represents a file attachment
type Attachment struct {
	Filename string
	Data     []byte
	MIMEType string
}

// LoadAttachment loads a file as an attachment
func LoadAttachment(path string) (*Attachment, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read attachment %s: %w", path, err)
	}

	filename := filepath.Base(path)
	mimeType := mime.TypeByExtension(filepath.Ext(path))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	return &Attachment{
		Filename: filename,
		Data:     data,
		MIMEType: mimeType,
	}, nil
}

// Build constructs the RFC 5322 compliant email message
func (m *Message) Build() ([]byte, error) {
	var buf bytes.Buffer

	// Generate Message-ID using sender's domain (reuse on subsequent calls)
	messageID := m.GeneratedMessageID
	if messageID == "" {
		domain := "localhost"
		if at := strings.LastIndex(m.From, "@"); at != -1 {
			d := strings.TrimSpace(m.From[at+1:])
			d = strings.TrimRight(d, ">")
			if d != "" {
				domain = d
			}
		}
		messageID = fmt.Sprintf("<%s@%s>", uuid.New().String(), domain)
		m.GeneratedMessageID = messageID
	}

	// Format date per RFC 5322
	date := time.Now().Format("Mon, 02 Jan 2006 15:04:05 -0700")

	// Write headers
	fmt.Fprintf(&buf, "From: %s\r\n", m.From)
	fmt.Fprintf(&buf, "To: %s\r\n", formatAddressList(m.To))
	if len(m.CC) > 0 {
		fmt.Fprintf(&buf, "Cc: %s\r\n", formatAddressList(m.CC))
	}
	// Note: BCC is not included in headers (by design - recipients are added via RCPT TO only)
	fmt.Fprintf(&buf, "Subject: %s\r\n", encodeHeader(m.Subject))
	fmt.Fprintf(&buf, "Date: %s\r\n", date)
	fmt.Fprintf(&buf, "Message-ID: %s\r\n", messageID)
	if m.InReplyTo != "" {
		fmt.Fprintf(&buf, "In-Reply-To: %s\r\n", ensureAngleBrackets(m.InReplyTo))
	}
	if m.References != "" {
		fmt.Fprintf(&buf, "References: %s\r\n", bracketReferences(m.References))
	}
	fmt.Fprintf(&buf, "MIME-Version: 1.0\r\n")

	// Determine content type
	contentType := "text/plain"
	if m.IsHTML {
		contentType = "text/html"
	}

	body := m.Body
	if m.IsHTML {
		body = `<div style="font-family:system-ui,-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;font-size:14px;line-height:1.47">` + normalizeListStyles(normalizeParagraphs(body)) + `</div>`
	}

	if len(m.Attachments) == 0 {
		// Simple message (plain text or HTML)
		fmt.Fprintf(&buf, "Content-Type: %s; charset=UTF-8\r\n", contentType)
		fmt.Fprintf(&buf, "Content-Transfer-Encoding: quoted-printable\r\n")
		fmt.Fprintf(&buf, "\r\n")
		buf.WriteString(toQuotedPrintable(body))
	} else {
		// Multipart message with attachments
		boundary := generateBoundary()
		fmt.Fprintf(&buf, "Content-Type: multipart/mixed; boundary=\"%s\"\r\n", boundary)
		fmt.Fprintf(&buf, "\r\n")

		// Body part
		fmt.Fprintf(&buf, "--%s\r\n", boundary)
		fmt.Fprintf(&buf, "Content-Type: %s; charset=UTF-8\r\n", contentType)
		fmt.Fprintf(&buf, "Content-Transfer-Encoding: quoted-printable\r\n")
		fmt.Fprintf(&buf, "\r\n")
		buf.WriteString(toQuotedPrintable(body))
		fmt.Fprintf(&buf, "\r\n")

		// Attachment parts
		for _, att := range m.Attachments {
			fmt.Fprintf(&buf, "--%s\r\n", boundary)
			fmt.Fprintf(&buf, "Content-Type: %s; name=\"%s\"\r\n", att.MIMEType, encodeFilename(att.Filename))
			fmt.Fprintf(&buf, "Content-Disposition: attachment; filename=\"%s\"\r\n", encodeFilename(att.Filename))
			fmt.Fprintf(&buf, "Content-Transfer-Encoding: base64\r\n")
			fmt.Fprintf(&buf, "\r\n")

			// Base64 encode with line wrapping at 76 chars
			encoded := base64.StdEncoding.EncodeToString(att.Data)
			for i := 0; i < len(encoded); i += 76 {
				end := i + 76
				if end > len(encoded) {
					end = len(encoded)
				}
				buf.WriteString(encoded[i:end])
				buf.WriteString("\r\n")
			}
		}

		fmt.Fprintf(&buf, "--%s--\r\n", boundary)
	}

	return buf.Bytes(), nil
}

// Recipients returns all recipient addresses (for SMTP RCPT TO)
// Includes To, CC, and BCC recipients
func (m *Message) Recipients() []string {
	recipients := make([]string, 0, len(m.To)+len(m.CC)+len(m.BCC))
	recipients = append(recipients, m.To...)
	recipients = append(recipients, m.CC...)
	recipients = append(recipients, m.BCC...)
	return recipients
}

// encodeHeader encodes a header value using RFC 2047 if it contains non-ASCII
func encodeHeader(s string) string {
	if isASCII(s) {
		return s
	}
	return mime.QEncoding.Encode("UTF-8", s)
}

// encodeFilename encodes a filename for Content-Disposition
func encodeFilename(s string) string {
	if isASCII(s) {
		return s
	}
	return mime.QEncoding.Encode("UTF-8", s)
}

// isASCII checks if a string contains only ASCII characters
func isASCII(s string) bool {
	for _, c := range s {
		if c > 127 {
			return false
		}
	}
	return true
}

// generateBoundary generates a random MIME boundary
func generateBoundary() string {
	return fmt.Sprintf("----=_Part_%s", uuid.New().String())
}

// toQuotedPrintable converts a string to quoted-printable encoding
func toQuotedPrintable(s string) string {
	var buf bytes.Buffer
	lineLen := 0

	for _, b := range []byte(s) {
		var encoded string

		if b == '\r' || b == '\n' {
			// Pass through newlines
			buf.WriteByte(b)
			lineLen = 0
			continue
		} else if (b >= 33 && b <= 60) || (b >= 62 && b <= 126) || b == ' ' || b == '\t' {
			// Printable characters (except =) and whitespace
			encoded = string(b)
		} else {
			// Encode as =XX
			encoded = fmt.Sprintf("=%02X", b)
		}

		// Soft line break at 76 chars
		if lineLen+len(encoded) > 75 {
			buf.WriteString("=\r\n")
			lineLen = 0
		}

		buf.WriteString(encoded)
		lineLen += len(encoded)
	}

	return buf.String()
}

// pTagRe matches opening <p> tags with any attributes (class, style, etc.)
var pTagRe = regexp.MustCompile(`(?i)<p(\s[^>]*)?>`)

// listTagRe matches opening <ul> and <ol> tags with any attributes.
var listTagRe = regexp.MustCompile(`(?i)<(ul|ol)(\s[^>]*)?>`)

// normalizeParagraphs adds margin:0 to all <p> tags so recipient email clients
// don't add extra spacing. Matches the compose editor's margin-reset style.
// Uses regex to handle <p> with any combination of attributes (class, style, etc.)
func normalizeParagraphs(html string) string {
	return pTagRe.ReplaceAllStringFunc(html, func(tag string) string {
		if strings.Contains(tag, "margin") {
			return tag // already has margin set
		}
		if idx := strings.Index(strings.ToLower(tag), "style=\""); idx != -1 {
			// Inject margin:0 at start of existing style attribute
			return tag[:idx+7] + "margin:0; " + tag[idx+7:]
		}
		// No style attribute — add one (handle <p> and <P>)
		lower := strings.ToLower(tag)
		if i := strings.Index(lower, "<p"); i != -1 {
			return tag[:i+2] + ` style="margin:0"` + tag[i+2:]
		}
		return tag
	})
}

// normalizeListStyles adds consistent padding/margin to all <ul>/<ol> tags
// so recipient email clients render lists like the compose editor.
// Matches the editor's CSS: ul, ol { padding-left: 1.5em; margin: 0.3em 0; }
func normalizeListStyles(html string) string {
	return listTagRe.ReplaceAllStringFunc(html, func(tag string) string {
		if strings.Contains(tag, "padding-left") || strings.Contains(tag, "margin") {
			return tag // already has list styles set
		}
		listStyle := "padding-left:1.5em;margin:0.3em 0"
		if idx := strings.Index(strings.ToLower(tag), "style=\""); idx != -1 {
			// Inject at start of existing style attribute
			return tag[:idx+7] + listStyle + "; " + tag[idx+7:]
		}
		// No style attribute — add one after the tag name
		lower := strings.ToLower(tag)
		for _, prefix := range []string{"<ul", "<ol"} {
			if i := strings.Index(lower, prefix); i != -1 {
				insertAt := i + len(prefix)
				return tag[:insertAt] + ` style="` + listStyle + `"` + tag[insertAt:]
			}
		}
		return tag
	})
}

// formatAddressList quotes display names that contain commas for safe use in headers.
// "Hammer, Timo Dr. <t@x.com>" → "\"Hammer, Timo Dr.\" <t@x.com>"
func formatAddressList(addrs []string) string {
	formatted := make([]string, len(addrs))
	for i, addr := range addrs {
		if idx := strings.LastIndex(addr, "<"); idx > 0 {
			name := strings.TrimSpace(addr[:idx])
			rest := addr[idx:]
			if strings.Contains(name, ",") && !strings.HasPrefix(name, "\"") {
				name = "\"" + strings.ReplaceAll(name, "\"", "\\\"") + "\""
			}
			formatted[i] = name + " " + rest
		} else {
			formatted[i] = addr
		}
	}
	return strings.Join(formatted, ", ")
}

// ensureAngleBrackets wraps a Message-ID in <> if not already present.
func ensureAngleBrackets(id string) string {
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(id, "<") {
		id = "<" + id
	}
	if !strings.HasSuffix(id, ">") {
		id = id + ">"
	}
	return id
}

// bracketReferences wraps each space-separated Message-ID in <> if needed.
func bracketReferences(refs string) string {
	parts := strings.Fields(refs)
	for i, p := range parts {
		parts[i] = ensureAngleBrackets(p)
	}
	return strings.Join(parts, " ")
}

// ParseAddress parses an email address string
// Supports formats: "email@example.com" and "Name <email@example.com>"
func ParseAddress(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("empty email address")
	}

	addr, err := mail.ParseAddress(s)
	if err != nil {
		// net/mail can't parse "email <email>" — extract from angle brackets
		if idx := strings.LastIndex(s, "<"); idx != -1 {
			if end := strings.LastIndex(s, ">"); end > idx {
				email := strings.TrimSpace(s[idx+1 : end])
				if strings.Contains(email, "@") {
					return email, nil
				}
			}
		}
		// Try as plain email
		if strings.Contains(s, "@") && !strings.Contains(s, " ") {
			return s, nil
		}
		return "", fmt.Errorf("invalid email address: %s", s)
	}

	return addr.Address, nil
}

// ParseAddressList parses a comma-separated list of email addresses
// Uses net/mail.ParseAddressList for RFC 5322 compliant parsing
// (handles commas inside display names like "Last, First <email>")
func ParseAddressList(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}

	parsed, err := mail.ParseAddressList(s)
	if err != nil {
		// Fallback: try naive split for plain email lists without display names
		parts := strings.Split(s, ",")
		addresses := make([]string, 0, len(parts))
		for _, part := range parts {
			addr, err := ParseAddress(part)
			if err != nil {
				return nil, err
			}
			addresses = append(addresses, addr)
		}
		return addresses, nil
	}

	addresses := make([]string, 0, len(parsed))
	for _, addr := range parsed {
		addresses = append(addresses, addr.Address)
	}
	return addresses, nil
}

// ReadBody reads the message body, stripping comment lines
func ReadBody(r io.Reader) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(data), "\n")
	var body []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		body = append(body, line)
	}

	// Trim leading/trailing empty lines but preserve internal formatting
	result := strings.Join(body, "\n")
	result = strings.TrimSpace(result)

	return result, nil
}
