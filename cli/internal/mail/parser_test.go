package mail

import (
	"encoding/base64"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadTestMail(t *testing.T, filename string) *mail.Message {
	t.Helper()

	path := filepath.Join("testdata", filename)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Failed to open test file %s: %v", filename, err)
	}
	defer f.Close()

	msg, err := mail.ReadMessage(f)
	if err != nil {
		t.Fatalf("Failed to parse test file %s: %v", filename, err)
	}

	return msg
}

func TestParserSimpleText(t *testing.T) {
	msg := loadTestMail(t, "simple_text.eml")
	parser := NewParser()

	content := parser.Parse(msg)

	// Check headers
	if content.From != "sender@example.com" {
		t.Errorf("From = %q, want %q", content.From, "sender@example.com")
	}
	if content.To != "recipient@example.com" {
		t.Errorf("To = %q, want %q", content.To, "recipient@example.com")
	}
	if content.Subject != "Simple Text Email" {
		t.Errorf("Subject = %q, want %q", content.Subject, "Simple Text Email")
	}
	if !strings.Contains(content.Date, "18 Dec 2025") {
		t.Errorf("Date = %q, should contain '18 Dec 2025'", content.Date)
	}

	// Check body
	if !strings.Contains(content.Body, "Hello, this is a simple text email") {
		t.Errorf("Body should contain text content, got: %q", content.Body)
	}
	if !strings.Contains(content.Body, "Best regards") {
		t.Errorf("Body should contain 'Best regards', got: %q", content.Body)
	}

	// HTML should be empty for plain text
	if content.HTML != "" {
		t.Errorf("HTML should be empty for plain text email, got: %q", content.HTML)
	}

	// No attachments
	if len(content.Attachments) != 0 {
		t.Errorf("Attachments should be empty, got: %v", content.Attachments)
	}
}

func TestParserSimpleHTML(t *testing.T) {
	msg := loadTestMail(t, "simple_html.eml")
	parser := NewParser()

	content := parser.Parse(msg)

	// Check headers
	if content.Subject != "Simple HTML Email" {
		t.Errorf("Subject = %q, want %q", content.Subject, "Simple HTML Email")
	}

	// HTML should contain the original HTML
	if !strings.Contains(content.HTML, "<h1>Hello World</h1>") {
		t.Errorf("HTML should contain h1 tag, got: %q", content.HTML)
	}
	if !strings.Contains(content.HTML, "<strong>HTML</strong>") {
		t.Errorf("HTML should contain strong tag, got: %q", content.HTML)
	}

	// Body should be stripped text (either from w3m or fallback)
	if !strings.Contains(content.Body, "Hello World") {
		t.Errorf("Body should contain 'Hello World', got: %q", content.Body)
	}
}

func TestParserMultipartAlternative(t *testing.T) {
	msg := loadTestMail(t, "multipart_alternative.eml")
	parser := NewParser()

	content := parser.Parse(msg)

	// Check headers
	if content.Subject != "Multipart Alternative Email" {
		t.Errorf("Subject = %q, want %q", content.Subject, "Multipart Alternative Email")
	}

	// Body should be from text/plain part
	if !strings.Contains(content.Body, "This is the plain text version") {
		t.Errorf("Body should contain plain text content, got: %q", content.Body)
	}

	// HTML should be from text/html part
	if !strings.Contains(content.HTML, "<strong>HTML</strong>") {
		t.Errorf("HTML should contain HTML content, got: %q", content.HTML)
	}

	// No attachments
	if len(content.Attachments) != 0 {
		t.Errorf("Attachments should be empty, got: %v", content.Attachments)
	}
}

func TestParserMultipartWithAttachment(t *testing.T) {
	msg := loadTestMail(t, "multipart_with_attachment.eml")
	parser := NewParser()

	content := parser.Parse(msg)

	// Check headers
	if content.Subject != "Email with Attachment" {
		t.Errorf("Subject = %q, want %q", content.Subject, "Email with Attachment")
	}

	// Body should contain text
	if !strings.Contains(content.Body, "This email has an attachment") {
		t.Errorf("Body should contain text content, got: %q", content.Body)
	}

	// Should have 2 attachments
	if len(content.Attachments) != 2 {
		t.Errorf("Should have 2 attachments, got: %v", content.Attachments)
	}

	// Check attachment names
	hasDocument := false
	hasImage := false
	for _, att := range content.Attachments {
		if att.Filename == "document.pdf" {
			hasDocument = true
		}
		if att.Filename == "image.png" {
			hasImage = true
		}
	}
	if !hasDocument {
		t.Error("Should have document.pdf attachment")
	}
	if !hasImage {
		t.Error("Should have image.png attachment")
	}
}

func TestParserEncodedHeaders(t *testing.T) {
	msg := loadTestMail(t, "encoded_headers.eml")
	parser := NewParser()

	content := parser.Parse(msg)

	// From should be decoded
	if !strings.Contains(content.From, "Thomas Müller") {
		t.Errorf("From should contain 'Thomas Müller', got: %q", content.From)
	}
	if !strings.Contains(content.From, "mueller@example.com") {
		t.Errorf("From should contain email address, got: %q", content.From)
	}

	// To should be decoded (Base64 encoded "Schröder")
	if !strings.Contains(content.To, "Schröder") {
		t.Errorf("To should contain 'Schröder', got: %q", content.To)
	}

	// Subject should be decoded
	if !strings.Contains(content.Subject, "Grüße aus München") {
		t.Errorf("Subject should contain 'Grüße aus München', got: %q", content.Subject)
	}

	// Body should be decoded from quoted-printable
	if !strings.Contains(content.Body, "Schöne Grüße") {
		t.Errorf("Body should contain 'Schöne Grüße', got: %q", content.Body)
	}
}

func TestParserBase64Body(t *testing.T) {
	msg := loadTestMail(t, "base64_body.eml")
	parser := NewParser()

	content := parser.Parse(msg)

	// Check subject
	if content.Subject != "Base64 Encoded Body" {
		t.Errorf("Subject = %q, want %q", content.Subject, "Base64 Encoded Body")
	}

	// Body should be decoded from base64
	if !strings.Contains(content.Body, "Hello, this is a base64 encoded email body") {
		t.Errorf("Body should contain decoded text, got: %q", content.Body)
	}
	if !strings.Contains(content.Body, "Best regards") {
		t.Errorf("Body should contain 'Best regards', got: %q", content.Body)
	}
}

func TestParserISO88591(t *testing.T) {
	msg := loadTestMail(t, "iso88591.eml")
	parser := NewParser()

	content := parser.Parse(msg)

	// Check subject
	if content.Subject != "ISO-8859-1 Email" {
		t.Errorf("Subject = %q, want %q", content.Subject, "ISO-8859-1 Email")
	}

	// Body should be converted from ISO-8859-1 to UTF-8
	if !strings.Contains(content.Body, "Grüße aus Deutschland") {
		t.Errorf("Body should contain 'Grüße aus Deutschland', got: %q", content.Body)
	}
	if !strings.Contains(content.Body, "äöüß") {
		t.Errorf("Body should contain 'äöüß', got: %q", content.Body)
	}
}

func TestNewParser(t *testing.T) {
	parser := NewParser()
	if parser == nil {
		t.Error("NewParser() should not return nil")
	}
}

func TestDetectMagic(t *testing.T) {
	tests := []struct {
		name             string
		data             []byte
		transferEncoding string
		wantMime         string
		wantExt          string
	}{
		{"PDF magic", []byte("%PDF-1.4 rest of content here"), "", "application/pdf", ".pdf"},
		{"PNG magic", append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, make([]byte, 8)...), "", "image/png", ".png"},
		{"JPEG magic", append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, make([]byte, 8)...), "", "image/jpeg", ".jpg"},
		{"GIF magic", []byte("GIF89a rest of content"), "", "image/gif", ".gif"},
		{"ZIP magic", append([]byte{0x50, 0x4B, 0x03, 0x04}, make([]byte, 8)...), "", "application/zip", ".zip"},
		{"XML magic", []byte("<?xml version=\"1.0\"?>"), "", "application/xml", ".xml"},
		{"unknown bytes", []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09}, "", "", ""},
		{"too short", []byte{0x89, 'P', 'N'}, "", "", ""},
		{"empty data", []byte{}, "", "", ""},
		{
			"base64 PDF",
			[]byte(base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 fake pdf content here"))),
			"base64",
			"application/pdf", ".pdf",
		},
		{
			"base64 PNG",
			[]byte(base64.StdEncoding.EncodeToString(append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, make([]byte, 16)...))),
			"base64",
			"image/png", ".png",
		},
		{
			"base64 data with 7bit encoding ignored",
			[]byte(base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 content"))),
			"7bit",
			"", "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMime, gotExt := detectMagic(tt.data, tt.transferEncoding)
			if gotMime != tt.wantMime {
				t.Errorf("detectMagic() mime = %q, want %q", gotMime, tt.wantMime)
			}
			if gotExt != tt.wantExt {
				t.Errorf("detectMagic() ext = %q, want %q", gotExt, tt.wantExt)
			}
		})
	}
}

func TestParserHTMLOnlyFallback(t *testing.T) {
	msg := loadTestMail(t, "html_only.eml")
	parser := NewParser()

	content := parser.Parse(msg)

	if content.Subject != "HTML Only Email" {
		t.Errorf("Subject = %q, want %q", content.Subject, "HTML Only Email")
	}

	// HTML should contain the original HTML
	if !strings.Contains(content.HTML, "<strong>HTML</strong>") {
		t.Errorf("HTML should contain strong tag, got: %q", content.HTML)
	}

	// Body should be auto-generated from HTML (fallback)
	if !strings.Contains(content.Body, "Important Update") {
		t.Errorf("Body should contain text derived from HTML, got: %q", content.Body)
	}
}

func TestParserInlineAttachment(t *testing.T) {
	msg := loadTestMail(t, "inline_attachment.eml")
	parser := NewParser()

	content := parser.Parse(msg)

	if content.Subject != "Inline Attachment Email" {
		t.Errorf("Subject = %q, want %q", content.Subject, "Inline Attachment Email")
	}

	// Body should contain the text part
	if !strings.Contains(content.Body, "inline image below") {
		t.Errorf("Body should contain text content, got: %q", content.Body)
	}

	// Should have 1 inline attachment
	if len(content.Attachments) != 1 {
		t.Fatalf("Should have 1 attachment, got %d", len(content.Attachments))
	}

	att := content.Attachments[0]
	if att.Filename != "photo.png" {
		t.Errorf("Attachment filename = %q, want %q", att.Filename, "photo.png")
	}
	if att.Disposition != "inline" {
		t.Errorf("Attachment disposition = %q, want %q", att.Disposition, "inline")
	}
	if att.ContentID != "<photo001@example.com>" {
		t.Errorf("Attachment ContentID = %q, want %q", att.ContentID, "<photo001@example.com>")
	}
}

func TestParserUnnamedAttachmentMagicDetection(t *testing.T) {
	msg := loadTestMail(t, "unnamed_attachment_pdf.eml")
	parser := NewParser()

	content := parser.Parse(msg)

	if content.Subject != "Unnamed Attachment" {
		t.Errorf("Subject = %q, want %q", content.Subject, "Unnamed Attachment")
	}

	// Body should contain the text part
	if !strings.Contains(content.Body, "See the attached file") {
		t.Errorf("Body should contain text content, got: %q", content.Body)
	}

	// Should have 1 attachment with magic-detected name and type
	if len(content.Attachments) != 1 {
		t.Fatalf("Should have 1 attachment, got %d", len(content.Attachments))
	}

	att := content.Attachments[0]
	if att.Filename != "attachment.pdf" {
		t.Errorf("Attachment filename = %q, want %q (from magic detection)", att.Filename, "attachment.pdf")
	}
	if att.ContentType != "application/pdf" {
		t.Errorf("Attachment ContentType = %q, want %q (from magic detection)", att.ContentType, "application/pdf")
	}
}

func TestParserSinglePartZipAttachment(t *testing.T) {
	// Google sends DMARC aggregate reports as a single-part application/zip
	// message (no multipart wrapper) per RFC 7489 §A.1. The parser must
	// emit one AttachmentInfo and not stuff the decoded zip into BodyText.
	msg := loadTestMail(t, "singlepart_zip_dmarc.eml")
	parser := NewParser()

	content := parser.Parse(msg)

	if content.Body != "" {
		t.Errorf("Body should be empty for binary single-part attachment, got %d bytes", len(content.Body))
	}
	if content.HTML != "" {
		t.Errorf("HTML should be empty, got %d bytes", len(content.HTML))
	}
	if len(content.Attachments) != 1 {
		t.Fatalf("Should have 1 attachment, got %d", len(content.Attachments))
	}

	att := content.Attachments[0]
	wantName := "google.com!habric.com!1766001600!1766088000.zip"
	if att.Filename != wantName {
		t.Errorf("Filename = %q, want %q", att.Filename, wantName)
	}
	if att.ContentType != "application/zip" {
		t.Errorf("ContentType = %q, want %q", att.ContentType, "application/zip")
	}
	if att.Disposition != "attachment" {
		t.Errorf("Disposition = %q, want %q", att.Disposition, "attachment")
	}
	if att.Size == 0 {
		t.Errorf("Size = 0, want non-zero")
	}
}

func TestParserEmptyContentType(t *testing.T) {
	msg := loadTestMail(t, "empty_content_type.eml")
	parser := NewParser()

	content := parser.Parse(msg)

	if content.Subject != "No Content Type" {
		t.Errorf("Subject = %q, want %q", content.Subject, "No Content Type")
	}

	// Should fall back to text/plain
	if !strings.Contains(content.Body, "no Content-Type header") {
		t.Errorf("Body should contain text content, got: %q", content.Body)
	}
	if !strings.Contains(content.Body, "default to text/plain") {
		t.Errorf("Body should contain full text, got: %q", content.Body)
	}

	// No HTML for plain text
	if content.HTML != "" {
		t.Errorf("HTML should be empty, got: %q", content.HTML)
	}
}
