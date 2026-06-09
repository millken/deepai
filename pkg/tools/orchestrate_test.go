package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitDiffer_CapturesCommittedChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "tester")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "baseline")

	ctx := context.Background()
	d := newGitDiffer(ctx, dir)
	if d.baseline == "" {
		t.Fatal("baseline commit not captured")
	}

	// Simulate the coder making a change AND committing it (the coder profile
	// auto-commits). Plain `git diff` would be empty here — the baseline diff
	// must still surface the change for the reviewer.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v2-changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("commit", "-am", "coder change")

	diff, err := d.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff, "v2-changed") {
		t.Fatalf("baseline diff missed the committed change:\n%s", diff)
	}

	// A brand-new uncommitted file should also appear (intent-to-add surface).
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("brand new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff2, err := d.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff2: %v", err)
	}
	if !strings.Contains(diff2, "new.txt") {
		t.Fatalf("baseline diff missed the new untracked file:\n%s", diff2)
	}
}

func TestDetectVerifyCommand(t *testing.T) {
	cases := []struct {
		marker string
		want   string
	}{
		{"go.mod", "go build ./..."},
		{"Cargo.toml", "cargo build"},
		{"tsconfig.json", "npx --no-install tsc --noEmit"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, c.marker), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := detectVerifyCommand(dir); got != c.want {
			t.Fatalf("%s: got %q, want %q", c.marker, got, c.want)
		}
	}
	// Unknown stack → empty (review-only), and explicit arg always wins.
	if got := detectVerifyCommand(t.TempDir()); got != "" {
		t.Fatalf("unknown stack: got %q, want empty", got)
	}
	if got := resolveVerifyCommand("make check", t.TempDir()); got != "make check" {
		t.Fatalf("explicit arg should win, got %q", got)
	}
}
