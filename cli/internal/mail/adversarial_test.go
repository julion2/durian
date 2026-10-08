package mail

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"testing"
)

// Generate valid nested MIME rather than mostly invalid random headers. The
// independently generated payload must survive attachment extraction exactly,
// including empty files, binary octets, RFC 2231 filenames and part reordering.
func FuzzAdversarialNestedMIMERoundTrip(f *testing.F) {
	f.Add([]byte{}, false, false)
	f.Add([]byte{0, 255, '\r', '\n', '=', 128}, true, true)
	f.Add([]byte("Outlook and Thunderbird multipart reply"), false, true)
	f.Fuzz(func(t *testing.T, payload []byte, attachmentFirst, quotedPrintable bool) {
		if len(payload) > 8192 {
			payload = payload[:8192]
		}
		prefix := payload
		if len(prefix) > 16 {
			prefix = prefix[:16]
		}
		body := "mail-body-" + hex.EncodeToString(prefix)
		filename := "Résumé-" + hex.EncodeToString(prefix) + ".bin"
		var raw bytes.Buffer
		mixed := multipart.NewWriter(&raw)
		if err := mixed.SetBoundary("durian-mixed"); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&raw, "From: alice@example.test\r\nTo: bob@example.test\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=%q\r\n\r\n",
			mime.BEncoding.Encode("UTF-8", "Re: Grüße"), mixed.Boundary())
		attachment := func() {
			encoding := "base64"
			if quotedPrintable {
				encoding = "quoted-printable"
			}
			part, err := mixed.CreatePart(textproto.MIMEHeader{
				"Content-Type":              {"application/octet-stream"},
				"Content-Disposition":       {mime.FormatMediaType("attachment", map[string]string{"filename": filename})},
				"Content-Transfer-Encoding": {encoding},
			})
			if err != nil {
				t.Fatal(err)
			}
			if quotedPrintable {
				writer := quotedprintable.NewWriter(part)
				writer.Binary = true
				if _, err := writer.Write(payload); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
			} else if _, err := part.Write([]byte(base64.StdEncoding.EncodeToString(payload))); err != nil {
				t.Fatal(err)
			}
		}
		if attachmentFirst {
			attachment()
		}
		part, err := mixed.CreatePart(textproto.MIMEHeader{"Content-Type": {"multipart/alternative; boundary=durian-alternative"}})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(part, "--durian-alternative\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n%s\r\n--durian-alternative\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>%s</p>\r\n--durian-alternative--\r\n",
			base64.StdEncoding.EncodeToString([]byte(body)), html.EscapeString(body))
		if !attachmentFirst {
			attachment()
		}
		if err := mixed.Close(); err != nil {
			t.Fatal(err)
		}
		message, err := mail.ReadMessage(bytes.NewReader(raw.Bytes()))
		if err != nil {
			t.Fatalf("valid generated message rejected: %v", err)
		}
		parsed := NewParser().Parse(message)
		if parsed.Subject != "Re: Grüße" || !strings.Contains(parsed.Body, body) || !strings.Contains(parsed.HTML, body) {
			t.Fatalf("subject/body alternative lost: subject=%q body=%q html=%q", parsed.Subject, parsed.Body, parsed.HTML)
		}
		if len(parsed.Attachments) != 1 || parsed.Attachments[0].Filename != filename {
			t.Fatalf("attachment metadata=%+v, want exactly %q", parsed.Attachments, filename)
		}
		got, mediaType, err := ExtractAttachmentPart(raw.Bytes(), 1)
		if err != nil || mediaType != "application/octet-stream" || !bytes.Equal(got, payload) {
			t.Fatalf("attachment roundtrip: got=%x want=%x type=%q err=%v", got, payload, mediaType, err)
		}
	})
}
