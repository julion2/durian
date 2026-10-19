package mail

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/mail"
	"testing"
)

func multipartWithAttachments(t *testing.T) []byte {
	t.Helper()
	a1 := base64.StdEncoding.EncodeToString([]byte("FIRST-attachment-bytes"))
	a2 := base64.StdEncoding.EncodeToString([]byte("SECOND-attachment-bytes"))
	return []byte("From: a@example.com\r\n" +
		"To: b@example.com\r\n" +
		"Subject: test\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"BOUND\"\r\n" +
		"\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"body text\r\n" +
		"--BOUND\r\n" +
		"Content-Type: application/pdf; name=\"one.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"Content-Disposition: attachment; filename=\"one.pdf\"\r\n" +
		"\r\n" + a1 + "\r\n" +
		"--BOUND\r\n" +
		"Content-Type: image/png; name=\"two.png\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"Content-Disposition: attachment; filename=\"two.png\"\r\n" +
		"\r\n" + a2 + "\r\n" +
		"--BOUND--\r\n")
}

func TestExtractAttachmentPart(t *testing.T) {
	raw := multipartWithAttachments(t)
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	parsed := NewParser().Parse(msg)

	for _, tc := range []struct {
		partID    int
		wantBytes string
		wantType  string
	}{
		{1, "FIRST-attachment-bytes", "application/pdf"},
		{2, "SECOND-attachment-bytes", "image/png"},
	} {
		t.Run(fmt.Sprintf("part-%d", tc.partID), func(t *testing.T) {
			data, ctype, err := ExtractAttachmentPart(raw, tc.partID)
			if err != nil {
				t.Fatalf("ExtractAttachmentPart(%d): %v", tc.partID, err)
			}
			if string(data) != tc.wantBytes {
				t.Errorf("bytes = %q, want %q", data, tc.wantBytes)
			}
			if ctype != tc.wantType {
				t.Errorf("content-type = %q, want %q", ctype, tc.wantType)
			}
			if got := parsed.Attachments[tc.partID-1].Size; got != len(tc.wantBytes) {
				t.Errorf("attachment size = %d, want decoded byte count %d", got, len(tc.wantBytes))
			}
		})
	}
}

func TestSinglePartAttachmentSizeIsDecoded(t *testing.T) {
	for _, encoding := range []string{"base64", "quoted-printable", "8bit"} {
		t.Run(encoding, func(t *testing.T) {
			want := "caf\xc3\xa9\x00"
			wire := want
			if encoding == "base64" {
				wire = "Y2Fm\r\nw6kA\r\n"
			} else if encoding == "quoted-printable" {
				wire = "caf=C3=A9=00"
			}
			raw := []byte("Content-Type: application/octet-stream; name=example.bin\r\nContent-Disposition: attachment; filename=example.bin\r\nContent-Transfer-Encoding: " + encoding + "\r\n\r\n" + wire)
			msg, err := mail.ReadMessage(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			parsed := NewParser().Parse(msg)
			if len(parsed.Attachments) != 1 || parsed.Attachments[0].Size != len(want) {
				t.Fatalf("attachment size must count %d decoded bytes: %+v", len(want), parsed.Attachments)
			}
			data, _, err := ExtractAttachmentPart(raw, 1)
			if err != nil || string(data) != want {
				t.Fatalf("download = %q, %v; want %q", data, err, want)
			}
		})
	}
}

func TestExtractAttachmentPart_Errors(t *testing.T) {
	raw := multipartWithAttachments(t)
	if _, _, err := ExtractAttachmentPart(raw, 0); err == nil {
		t.Error("partID 0 should error")
	}
	if _, _, err := ExtractAttachmentPart(raw, 3); err == nil {
		t.Error("out-of-range partID should error")
	}
}
