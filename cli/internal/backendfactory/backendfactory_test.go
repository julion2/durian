package backendfactory

import (
	"testing"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/gmailbackend"
	"github.com/julion2/durian/cli/internal/graphbackend"
	"github.com/julion2/durian/cli/internal/imapbackend"
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

func TestPushWatchIsStaticByBackendType(t *testing.T) {
	tests := []struct {
		name         string
		account      *config.AccountConfig
		capabilities backend.Capabilities
		wantPush     bool
		wantInbox    bool
	}{
		{name: "IMAP", account: &config.AccountConfig{SyncEngine: "engine"}, capabilities: (&imapbackend.Backend{}).Capabilities(), wantPush: true, wantInbox: true},
		{name: "JMAP", account: &config.AccountConfig{SyncEngine: "jmap"}, capabilities: (&jmapbackend.Backend{}).Capabilities(), wantPush: true},
		{name: "Graph", account: &config.AccountConfig{SyncEngine: "graph"}, capabilities: (&graphbackend.Backend{}).Capabilities()},
		{name: "Gmail", account: &config.AccountConfig{SyncEngine: "gmail"}, capabilities: (&gmailbackend.Backend{}).Capabilities()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PushWatch(tt.account); got != tt.wantPush {
				t.Fatalf("PushWatch() = %v, want %v", got, tt.wantPush)
			}
			if got := tt.capabilities.PushWatch; got != tt.wantPush {
				t.Fatalf("backend capability PushWatch = %v, static selection = %v", got, tt.wantPush)
			}
			if got := PushInboxOnly(tt.account); got != tt.wantInbox {
				t.Fatalf("PushInboxOnly() = %v, want %v", got, tt.wantInbox)
			}
		})
	}
}
