package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHumanTextNeutralizesTerminalAndBidiControls(t *testing.T) {
	got := humanText("safe\x1b]0;spoof\a\u202e.txt", false)
	if strings.ContainsRune(got, '\x1b') || strings.ContainsRune(got, '\a') || strings.ContainsRune(got, '\u202e') {
		t.Fatalf("unsafe controls remain in %q", got)
	}
	for _, marker := range []string{"U+001B", "U+0007", "U+202E"} {
		if !strings.Contains(got, marker) {
			t.Errorf("sanitized text %q missing %s", got, marker)
		}
	}
}

func TestTruncateUsesGraphemesAndDisplayWidth(t *testing.T) {
	got := truncate("👍🏽界abc", 5)
	if !utf8.ValidString(got) {
		t.Fatalf("truncate returned invalid UTF-8: %q", got)
	}
	if visibleWidth(got) > 5 {
		t.Fatalf("visible width %d exceeds 5: %q", visibleWidth(got), got)
	}
	if strings.Contains(got, "�") {
		t.Fatalf("truncate split a grapheme: %q", got)
	}
}

func TestVisibleWidthUsesTerminalCells(t *testing.T) {
	if got := visibleWidth("界"); got != 2 {
		t.Fatalf("CJK width = %d, want 2", got)
	}
}
