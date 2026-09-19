package mailsend

import (
	"errors"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	base := errors.New("boom")
	if got := Classify(base); got != KindTransient {
		t.Errorf("untagged error = %v, want KindTransient", got)
	}
	for _, kind := range []Kind{KindTransient, KindNetwork, KindPermanent, KindAmbiguous, KindDeliveredWithWarning} {
		wrapped := &Error{Kind: kind, Err: base}
		if got := Classify(wrapped); got != kind {
			t.Errorf("Classify(%v) = %v, want %v", kind, got, kind)
		}
		if !errors.Is(wrapped, base) {
			t.Errorf("Error should unwrap to the underlying cause")
		}
	}
}

func TestGenerateMessageID(t *testing.T) {
	id := GenerateMessageID("Jane Doe <jane@example.com>")
	if !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, "@example.com>") {
		t.Errorf("id = %q, want <uuid@example.com>", id)
	}
	a := GenerateMessageID("jane@example.com")
	b := GenerateMessageID("jane@example.com")
	if a == b {
		t.Error("two ids should be unique")
	}
	// No domain -> falls back to localhost, never empty.
	if got := GenerateMessageID("nobody"); !strings.HasSuffix(got, "@localhost>") {
		t.Errorf("id without domain = %q, want @localhost", got)
	}
}
