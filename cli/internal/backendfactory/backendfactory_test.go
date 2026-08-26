package backendfactory

import (
	"testing"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/jmapbackend"
)

func TestJMAPBackendSelection(t *testing.T) {
	account := &config.AccountConfig{
		Email: "me@example.test", SyncEngine: "jmap",
		JMAP: &config.JMAPConfig{SessionURL: "https://mail.example.test/jmap/session", Auth: "password"},
	}
	b, err := New(account)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, ok := b.(*jmapbackend.Backend); !ok {
		t.Fatalf("New() returned %T, want *jmapbackend.Backend", b)
	}
	if got := CursorSuffix(account); got != "-jmap" {
		t.Fatalf("CursorSuffix() = %q, want -jmap", got)
	}
}
