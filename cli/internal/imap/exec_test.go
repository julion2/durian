package imap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/store"
)

func writeTestScript(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

func TestRunExecRule_NoExec(t *testing.T) {
	rule := config.RuleConfig{Name: "Static", Match: "from:test"}
	out, err := RunExecRule(rule, &store.Message{}, nil, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != nil {
		t.Error("expected nil for rule without exec")
	}
}

func TestRunExecRule_Success(t *testing.T) {
	script := writeTestScript(t, "classify.sh", `#!/bin/bash
cat > /dev/null
echo '{"add_tags":["important","finance"],"remove_tags":["unread"]}'
`)
	rule := config.RuleConfig{Name: "Classify", Exec: script, ExecTimeout: 5}
	msg := &store.Message{
		FromAddr: "alice@example.com",
		Subject:  "Invoice Q4",
		BodyText: "Please find attached",
	}

	out, err := RunExecRule(rule, msg, []string{"inbox", "unread"}, "work")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.AddTags) != 2 || out.AddTags[0] != "important" || out.AddTags[1] != "finance" {
		t.Errorf("add_tags = %v, want [important, finance]", out.AddTags)
	}
	if len(out.RemoveTags) != 1 || out.RemoveTags[0] != "unread" {
		t.Errorf("remove_tags = %v, want [unread]", out.RemoveTags)
	}
}

func TestRunExecRule_ReceivesJSON(t *testing.T) {
	// Script echoes back the from field to verify input
	script := writeTestScript(t, "echo_from.sh", `#!/bin/bash
INPUT=$(cat)
FROM=$(echo "$INPUT" | grep -o '"from":"[^"]*"' | head -1)
if echo "$FROM" | grep -q "alice@test.com"; then
  echo '{"add_tags":["matched"]}'
else
  echo '{"add_tags":["no-match"]}'
fi
`)
	rule := config.RuleConfig{Name: "Echo", Exec: script}
	msg := &store.Message{FromAddr: "alice@test.com", Subject: "Test"}

	out, err := RunExecRule(rule, msg, nil, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.AddTags) != 1 || out.AddTags[0] != "matched" {
		t.Errorf("add_tags = %v, want [matched]", out.AddTags)
	}
}

func TestRunExecRule_EmptyOutput(t *testing.T) {
	script := writeTestScript(t, "noop.sh", `#!/bin/bash
cat > /dev/null
`)
	rule := config.RuleConfig{Name: "NoOp", Exec: script}

	out, err := RunExecRule(rule, &store.Message{}, nil, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.AddTags) != 0 && len(out.RemoveTags) != 0 {
		t.Errorf("expected empty output, got add=%v remove=%v", out.AddTags, out.RemoveTags)
	}
}

func TestRunExecRule_NonZeroExit(t *testing.T) {
	script := writeTestScript(t, "fail.sh", `#!/bin/bash
echo "something went wrong" >&2
exit 1
`)
	rule := config.RuleConfig{Name: "Fail", Exec: script}

	_, err := RunExecRule(rule, &store.Message{}, nil, "test")
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Errorf("error = %q, should mention failure", err.Error())
	}
}

func TestRunExecRule_Timeout(t *testing.T) {
	script := writeTestScript(t, "slow.sh", `#!/bin/bash
sleep 30
`)
	rule := config.RuleConfig{Name: "Slow", Exec: script, ExecTimeout: 1}

	_, err := RunExecRule(rule, &store.Message{}, nil, "test")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, should mention timeout", err.Error())
	}
}

func TestRunExecRule_InvalidJSON(t *testing.T) {
	script := writeTestScript(t, "bad_json.sh", `#!/bin/bash
cat > /dev/null
echo 'not json'
`)
	rule := config.RuleConfig{Name: "BadJSON", Exec: script}

	_, err := RunExecRule(rule, &store.Message{}, nil, "test")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid JSON") {
		t.Errorf("error = %q, should mention invalid JSON", err.Error())
	}
}

func TestFilterAllowedTags(t *testing.T) {
	filtered := filterAllowedTags(
		[]string{"finance", "spam", "unknown", "important"},
		[]string{"finance", "important", "newsletter"},
		"test-rule",
	)
	if len(filtered) != 2 {
		t.Fatalf("got %d tags, want 2", len(filtered))
	}
	if filtered[0] != "finance" || filtered[1] != "important" {
		t.Errorf("filtered = %v, want [finance, important]", filtered)
	}
}

func TestFilterAllowedTags_Empty(t *testing.T) {
	filtered := filterAllowedTags(nil, []string{"a"}, "test")
	if len(filtered) != 0 {
		t.Errorf("expected empty, got %v", filtered)
	}
}

func TestRunExecRule_DefaultTimeout(t *testing.T) {
	// Just verify it doesn't panic with zero timeout (should use default)
	script := writeTestScript(t, "quick.sh", `#!/bin/bash
cat > /dev/null
echo '{}'
`)
	rule := config.RuleConfig{Name: "Quick", Exec: script} // ExecTimeout = 0 → default

	_, err := RunExecRule(rule, &store.Message{}, nil, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
