package mail

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"

	"github.com/durian-dev/durian/cli/internal/encoding"
	"github.com/durian-dev/durian/cli/internal/sanitize"
)

// detectMagic returns MIME type and extension from file magic bytes.
// data may be base64-encoded; transferEncoding is checked to decode first.
func detectMagic(data []byte, transferEncoding string) (mimeType, ext string) {
	if len(data) < 8 {
		return "", ""
	}
	// Decode transfer encoding to get raw bytes for magic check
	raw := data
	if strings.Contains(strings.ToLower(transferEncoding), "base64") {
		// Only need first few bytes — decode a small chunk
		chunk := data
		if len(chunk) > 64 {
			chunk = chunk[:64]
		}
		if decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(chunk))); err == nil {
			raw = decoded
		}
	}
	if len(raw) < 4 {
		return "", ""
	}
	switch {
	case bytes.HasPrefix(raw, []byte("%PDF")):
		return "application/pdf", ".pdf"
	case bytes.HasPrefix(raw, []byte{0x89, 'P', 'N', 'G'}):
		return "image/png", ".png"
	case bytes.HasPrefix(raw, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg", ".jpg"
	case bytes.HasPrefix(raw, []byte("GIF8")):
		return "image/gif", ".gif"
	case bytes.HasPrefix(raw, []byte{0x50, 0x4B, 0x03, 0x04}):
		return "application/zip", ".zip"
	case bytes.HasPrefix(raw, []byte("<?xml")):
		return "application/xml", ".xml"
	default:
		return "", ""
	}
}

// Parser handles MIME parsing of email messages
type Parser struct{}

// NewParser creates a new mail parser
func NewParser() *Parser {
	return &Parser{}
}

// Parse extracts content from a mail.Message and returns MailContent
func (p *Parser) Parse(msg *mail.Message) *MailContent {
	content := &MailContent{
		From:       encoding.DecodeHeader(msg.Header.Get("From")),
		To:         encoding.DecodeHeader(msg.Header.Get("To")),
		CC:         encoding.DecodeHeader(msg.Header.Get("Cc")),
		Subject:    encoding.DecodeHeader(msg.Header.Get("Subject")),
		Date:       msg.Header.Get("Date"),
		MessageID:  msg.Header.Get("Message-ID"),
		InReplyTo:  msg.Header.Get("In-Reply-To"),
		References: msg.Header.Get("References"),
	}

	textBody, htmlBody, attachments := p.extractBody(msg)
	content.Body = textBody
	content.HTML = sanitize.SanitizeHTML(htmlBody)
	content.Attachments = attachments

	return content
}

// extractBody extracts text, HTML and attachments from a mail message
func (p *Parser) extractBody(msg *mail.Message) (string, string, []AttachmentInfo) {
	contentType := msg.Header.Get("Content-Type")
	transferEncoding := msg.Header.Get("Content-Transfer-Encoding")
	charset := encoding.GetCharset(contentType)

	if contentType == "" {
		contentType = "text/plain"
	}

	mediaType, params, _ := mime.ParseMediaType(contentType)
	var attachments []AttachmentInfo

	if strings.HasPrefix(mediaType, "text/plain") {
		body, _ := io.ReadAll(msg.Body)
		text := encoding.DecodeBody(body, transferEncoding, charset)
		return text, "", nil
	}

	if strings.HasPrefix(mediaType, "text/html") {
		body, _ := io.ReadAll(msg.Body)
		html := encoding.DecodeBody(body, transferEncoding, charset)
		return encoding.HTMLToText(html), html, nil
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		return p.extractMultipart(msg.Body, params["boundary"])
	}

	// Single-part non-text message (e.g. application/zip from Google DMARC reports,
	// application/pdf, image/*, application/octet-stream with a filename). RFC 7489
	// §A.1 explicitly allows DMARC aggregate reports as a single application/zip
	// part with no multipart wrapper. Treat the whole body as one attachment
	// instead of stuffing decoded binary bytes into BodyText.
	contentDisp := msg.Header.Get("Content-Disposition")
	dispMediaType, dispParams, _ := mime.ParseMediaType(contentDisp)
	isAttachment := mediaType != "" && !strings.HasPrefix(mediaType, "text/") &&
		(strings.HasPrefix(dispMediaType, "attachment") || strings.HasPrefix(dispMediaType, "inline") ||
			params["name"] != "" || dispParams["filename"] != "")
	if isAttachment {
		body, _ := io.ReadAll(msg.Body)
		name := encoding.DecodeHeader(dispParams["filename"])
		if name == "" {
			name = encoding.DecodeHeader(params["name"])
		}
		disposition := "attachment"
		if strings.HasPrefix(dispMediaType, "inline") {
			disposition = "inline"
		}
		if mediaType == "application/octet-stream" || name == "" {
			if detected, ext := detectMagic(body, transferEncoding); detected != "" {
				if mediaType == "application/octet-stream" {
					mediaType = detected
				}
				if name == "" {
					name = "attachment" + ext
				}
			}
		}
		if name == "" {
			name = "unnamed"
		}
		attachments = append(attachments, AttachmentInfo{
			Filename:    name,
			ContentType: mediaType,
			Size:        len(body),
			Disposition: disposition,
			ContentID:   msg.Header.Get("Content-Id"),
		})
		return "", "", attachments
	}

	body, _ := io.ReadAll(msg.Body)
	return encoding.DecodeBody(body, transferEncoding, charset), "", attachments
}

// extractMultipart recursively extracts content from multipart messages
func (p *Parser) extractMultipart(r io.Reader, boundary string) (string, string, []AttachmentInfo) {
	mr := multipart.NewReader(r, boundary)
	var textContent, htmlContent string
	var attachments []AttachmentInfo

	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}

		contentType := part.Header.Get("Content-Type")
		contentDisp := part.Header.Get("Content-Disposition")
		transferEncoding := part.Header.Get("Content-Transfer-Encoding")
		charset := encoding.GetCharset(contentType)
		mediaType, params, _ := mime.ParseMediaType(contentType)

		if strings.Contains(contentDisp, "attachment") || (part.FileName() != "" && !strings.HasPrefix(mediaType, "text/")) {
			name := encoding.DecodeHeader(part.FileName())
			disposition := "attachment"
			if strings.Contains(contentDisp, "inline") {
				disposition = "inline"
			}
			attBody, _ := io.ReadAll(part)

			// Detect real type from magic bytes when Content-Type is generic
			if mediaType == "application/octet-stream" || name == "" {
				if detected, ext := detectMagic(attBody, transferEncoding); detected != "" {
					if mediaType == "application/octet-stream" {
						mediaType = detected
					}
					if name == "" {
						name = "attachment" + ext
					}
				}
			}
			if name == "" {
				name = "unnamed"
			}

			attachments = append(attachments, AttachmentInfo{
				Filename:    name,
				ContentType: mediaType,
				Size:        len(attBody),
				Disposition: disposition,
				ContentID:   part.Header.Get("Content-Id"),
			})
			continue
		}

		body, _ := io.ReadAll(part)

		if strings.HasPrefix(mediaType, "text/plain") && textContent == "" {
			textContent = encoding.DecodeBody(body, transferEncoding, charset)
		} else if strings.HasPrefix(mediaType, "text/html") && htmlContent == "" {
			htmlContent = encoding.DecodeBody(body, transferEncoding, charset)
		} else if strings.HasPrefix(mediaType, "multipart/") {
			nested, nestedHTML, atts := p.extractMultipart(bytes.NewReader(body), params["boundary"])
			if textContent == "" {
				textContent = nested
			}
			if htmlContent == "" {
				htmlContent = nestedHTML
			}
			attachments = append(attachments, atts...)
		}
	}

	if textContent == "" && htmlContent != "" {
		textContent = encoding.HTMLToText(htmlContent)
	}

	return textContent, htmlContent, attachments
}
