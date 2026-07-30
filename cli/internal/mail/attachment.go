package mail

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net/mail"
	"strings"
)

// ExtractAttachmentPart parses a raw RFC822 message and returns the
// transfer-decoded content and media type of the attachment at partID. partID
// is 1-based in parse order, matching the PartID the ingest path assigns (the
// attachment's position in the parser's attachment list).
//
// This serves attachments from a raw body fetched via Backend.FetchBody, so
// downloads work for engine/Graph-backed messages that carry a provider handle
// (remote_ref) rather than an IMAP UID.
func ExtractAttachmentPart(raw []byte, partID int) (data []byte, contentType string, err error) {
	if partID < 1 {
		return nil, "", fmt.Errorf("invalid attachment part id %d", partID)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, "", fmt.Errorf("parse message: %w", err)
	}
	p := &Parser{captureIndex: partID}
	p.extractBody(msg)
	if p.capturedData == nil {
		return nil, "", fmt.Errorf("attachment part %d not found", partID)
	}
	return p.capturedData, p.capturedType, nil
}

// decodeTransferEncoding reverses a Content-Transfer-Encoding for binary-safe
// output (no charset conversion). base64 uses a newline-tolerant streaming
// decoder (MIME base64 is wrapped at 76 columns). quoted-printable is handled
// for completeness, though mime/multipart already decodes and hides it on
// multipart parts (in which case enc arrives empty here). 7bit/8bit/binary and
// unknown encodings pass through unchanged.
func decodeTransferEncoding(body []byte, enc string) []byte {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "base64":
		out, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(body)))
		if err != nil {
			return body
		}
		return out
	case "quoted-printable":
		out, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
		if err != nil {
			return body
		}
		return out
	default:
		return body
	}
}
