package mail

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"strconv"
	"strings"
)

const syntheticMessageIDPrefix = "durian-synthetic-"

// SyntheticMessageID returns a fallback identity for an IMAP message without a
// Message-ID header. UIDVALIDITY distinguishes UID epochs so a newly assigned
// UID cannot overwrite an older local row after a mailbox reset.
func SyntheticMessageID(uidValidity, uid uint32, mailbox, account string) string {
	return fmt.Sprintf("durian-synthetic-v2-%d-%d-%s@%s", uidValidity, uid, mailbox, account)
}

// IsSyntheticMessageID reports whether id is one of Durian's fallback IDs.
func IsSyntheticMessageID(id string) bool {
	_, ok := SyntheticMessageSequence(id)
	return ok
}

// SyntheticMessageUIDValidity returns the UIDVALIDITY embedded in a v2
// synthetic ID. Legacy generated IDs did not record an epoch.
func SyntheticMessageUIDValidity(id string) (uint32, bool) {
	uidValidity, _, hasUIDValidity, ok := syntheticMessageParts(id)
	return uidValidity, ok && hasUIDValidity
}

// SyntheticMessageSequence returns the UID embedded in a generated legacy or
// v2 synthetic ID. The UID is an immutable ordering tie-breaker for otherwise
// indistinguishable messages; unlike RemoteRef it does not change when a
// partially completed replacement is retried.
func SyntheticMessageSequence(id string) (uint64, bool) {
	_, uid, _, ok := syntheticMessageParts(id)
	return uid, ok
}

func syntheticMessageParts(id string) (uint32, uint64, bool, bool) {
	remainder, ok := strings.CutPrefix(id, syntheticMessageIDPrefix)
	if !ok {
		return 0, 0, false, false
	}
	if strings.HasPrefix(remainder, "v2-") {
		parts := strings.SplitN(remainder, "-", 4)
		if len(parts) != 4 || parts[0] != "v2" || !strings.Contains(parts[3], "@") {
			return 0, 0, false, false
		}
		uidValidity, err := strconv.ParseUint(parts[1], 10, 32)
		if err != nil {
			return 0, 0, false, false
		}
		uid, err := strconv.ParseUint(parts[2], 10, 32)
		return uint32(uidValidity), uid, true, err == nil
	}
	uidText, suffix, ok := strings.Cut(remainder, "-")
	if !ok || !strings.Contains(suffix, "@") {
		return 0, 0, false, false
	}
	uid, err := strconv.ParseUint(uidText, 10, 32)
	return 0, uid, false, err == nil
}

// SyntheticFingerprint identifies the parsed, user-visible content of a
// message without depending on IMAP UIDs or mutable local-delivery headers such
// as Status and X-Status. It is used only transiently during UIDVALIDITY
// recovery; the digest is not persisted.
func SyntheticFingerprint(content *MailContent, dateUnix int64) [sha256.Size]byte {
	h := sha256.New()
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], uint64(dateUnix))
	_, _ = h.Write(number[:])
	for _, field := range []string{
		content.From,
		content.To,
		content.CC,
		content.BCC,
		content.Subject,
		content.InReplyTo,
		content.References,
		content.Body,
		content.HTML,
	} {
		writeFingerprintField(h, field)
	}
	writeFingerprintNumber(h, uint64(len(content.Attachments)))
	for i, attachment := range content.Attachments {
		partID := attachment.PartID
		if partID == 0 {
			partID = i + 1
		}
		writeFingerprintNumber(h, uint64(partID))
		writeFingerprintField(h, attachment.Filename)
		writeFingerprintField(h, attachment.ContentType)
		writeFingerprintNumber(h, uint64(attachment.Size))
		writeFingerprintField(h, attachment.Disposition)
		writeFingerprintField(h, attachment.ContentID)
	}
	var result [sha256.Size]byte
	copy(result[:], h.Sum(nil))
	return result
}

func writeFingerprintField(h hash.Hash, field string) {
	writeFingerprintNumber(h, uint64(len(field)))
	_, _ = h.Write([]byte(field))
}

func writeFingerprintNumber(h hash.Hash, number uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], number)
	_, _ = h.Write(encoded[:])
}
