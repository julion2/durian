package mail

import (
	"bytes"
	"net/mail"
	"testing"
)

func TestSyntheticMessageIDSeparatesUIDValidityEpochs(t *testing.T) {
	first := SyntheticMessageID(10, 42, "INBOX", "work")
	second := SyntheticMessageID(11, 42, "INBOX", "work")
	if first == second {
		t.Fatalf("UIDVALIDITY epochs produced the same ID %q", first)
	}
	if !IsSyntheticMessageID(first) || IsSyntheticMessageID("real@example.com") {
		t.Fatal("synthetic ID detection disagrees with generated IDs")
	}
	if uid, ok := SyntheticMessageSequence(first); !ok || uid != 42 {
		t.Fatalf("v2 sequence = %d, %v", uid, ok)
	}
	if uid, ok := SyntheticMessageSequence("durian-synthetic-7-INBOX@work"); !ok || uid != 7 {
		t.Fatalf("legacy sequence = %d, %v", uid, ok)
	}
	for _, realID := range []string{
		"durian-synthetic-alerts@example.com",
		"durian-synthetic-v2-not-a-generated-id@example.com",
	} {
		if IsSyntheticMessageID(realID) {
			t.Fatalf("real Message-ID %q classified as synthetic", realID)
		}
	}
}

func TestSyntheticFingerprintIgnoresMutableDeliveryHeaders(t *testing.T) {
	first := parseFingerprintContent(t, "Status: RO\r\nX-Status: A\r\n")
	second := parseFingerprintContent(t, "Status: O\r\nX-Status: F\r\n")
	if got, want := SyntheticFingerprint(first, 123), SyntheticFingerprint(second, 123); got != want {
		t.Fatalf("mutable delivery headers changed fingerprint: %x != %x", got, want)
	}

	changed := *second
	changed.Body = "different body"
	if SyntheticFingerprint(first, 123) == SyntheticFingerprint(&changed, 123) {
		t.Fatal("different parsed content produced the same fingerprint")
	}
	if SyntheticFingerprint(first, 123) == SyntheticFingerprint(first, 124) {
		t.Fatal("different receive dates produced the same fingerprint")
	}
}

func TestSyntheticFingerprintLengthPrefixesFields(t *testing.T) {
	first := &MailContent{From: "ab", To: "c"}
	second := &MailContent{From: "a", To: "bc"}
	if SyntheticFingerprint(first, 0) == SyntheticFingerprint(second, 0) {
		t.Fatal("field-boundary ambiguity produced the same fingerprint")
	}
	first.Attachments = []AttachmentInfo{{Filename: "report.pdf", ContentType: "application/pdf", Size: 10}}
	second = &MailContent{From: "ab", To: "c", Attachments: []AttachmentInfo{{Filename: "report.pdf", ContentType: "application/pdf", Size: 11}}}
	if SyntheticFingerprint(first, 0) == SyntheticFingerprint(second, 0) {
		t.Fatal("different attachment metadata produced the same fingerprint")
	}
}

func parseFingerprintContent(t *testing.T, mutableHeaders string) *MailContent {
	t.Helper()
	raw := []byte("From: sender@example.com\r\n" +
		"To: receiver@example.com\r\n" +
		"Subject: Missing ID\r\n" +
		"Date: Thu, 27 Aug 2026 10:00:00 +0000\r\n" +
		mutableHeaders + "\r\nbody")
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return NewParser().Parse(parsed)
}
