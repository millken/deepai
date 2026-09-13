package models

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytes_NeverSplitsARune(t *testing.T) {
	// Every cut position across a mixed ASCII/CJK/emoji string: none may
	// produce invalid UTF-8, and none may grow the string past the budget.
	s := "build.zig 指向正确的测试 ✅ mixed ASCII"
	for n := 0; n <= len(s)+4; n++ {
		got := TruncateBytes(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("TruncateBytes(%d) produced invalid UTF-8: %q", n, got)
		}
		if len(got) > n && n > 0 {
			t.Fatalf("TruncateBytes(%d) returned %d bytes", n, len(got))
		}
		if !strings.HasPrefix(s, got) {
			t.Fatalf("TruncateBytes(%d) is not a prefix of the input: %q", n, got)
		}
	}
}

func TestTruncateBytes_KeepsShortInputAndRejectsNonPositive(t *testing.T) {
	if got := TruncateBytes("短", 100); got != "短" {
		t.Errorf("input shorter than the budget was altered: %q", got)
	}
	if got := TruncateBytes("短", 0); got != "" {
		t.Errorf("zero budget should yield empty, got %q", got)
	}
	if got := TruncateBytes("短", -5); got != "" {
		t.Errorf("negative budget should yield empty, got %q", got)
	}
	// A budget landing inside the first rune yields empty rather than a fragment.
	if got := TruncateBytes("短", 2); got != "" {
		t.Errorf("expected empty rather than a rune fragment, got %q", got)
	}
}
