package chat

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func gitOrSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

func runGitOrFatal(t *testing.T, dir string, args ...string) {
	t.Helper()
	base := []string{"-C", dir, "-c", "user.email=test@test", "-c", "user.name=test", "-c", "commit.gpgsign=false"}
	cmd := exec.Command("git", append(base, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFileOrFatal(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// initRepo creates a repo with one committed file and one pre-existing
// untracked file — the "user's own dirty state" that must stay in the
// baseline.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGitOrFatal(t, dir, "init", "-q")
	writeFileOrFatal(t, filepath.Join(dir, "committed.go"), "package x\n")
	runGitOrFatal(t, dir, "add", "committed.go")
	runGitOrFatal(t, dir, "commit", "-q", "-m", "init")
	writeFileOrFatal(t, filepath.Join(dir, "userdirty.go"), "package x // user's own\n")
	return dir
}

func TestWorktreeSnapshotAttribution(t *testing.T) {
	gitOrSkip(t)
	dir := initRepo(t)

	s0 := takeWorktreeSnapshot(dir)
	if s0.root == "" {
		t.Fatal("expected a git snapshot, got unavailable")
	}

	// "During the turn": modify a tracked file and create a new one; leave
	// the user's pre-turn untracked file alone.
	writeFileOrFatal(t, filepath.Join(dir, "committed.go"), "package x\n\nfunc F() {}\n")
	writeFileOrFatal(t, filepath.Join(dir, "newfile.go"), "package x\n\nfunc G() {}\n")

	s1 := takeWorktreeSnapshot(dir)
	changed := s1.changedSince(s0)

	// git may resolve the tempdir through symlinks (e.g. /tmp on macOS);
	// compare by basename set.
	got := map[string]bool{}
	for _, p := range changed {
		got[filepath.Base(p)] = true
	}
	if len(changed) != 2 || !got["committed.go"] || !got["newfile.go"] {
		t.Fatalf("changedSince = %v, want exactly {committed.go, newfile.go}", changed)
	}
}

func TestWorktreeSnapshotRedirtiedFileIsAttributed(t *testing.T) {
	gitOrSkip(t)
	dir := initRepo(t)

	s0 := takeWorktreeSnapshot(dir)

	// The user's untracked file keeps its "??" porcelain status, but its
	// content changes during the turn — only the stat fingerprint can see
	// this. Force a distinct mtime so coarse filesystem timestamps cannot
	// mask the size-equal-content-different edge... content length differs
	// here anyway; the Chtimes guards the equal-length variant.
	path := filepath.Join(dir, "userdirty.go")
	writeFileOrFatal(t, path, "package x // rewritten during the agent turn\n")
	if err := os.Chtimes(path, time.Now(), time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	s1 := takeWorktreeSnapshot(dir)
	changed := s1.changedSince(s0)
	if len(changed) != 1 || filepath.Base(changed[0]) != "userdirty.go" {
		t.Fatalf("changedSince = %v, want exactly {userdirty.go}", changed)
	}
}

func TestWorktreeSnapshotUserBaselineNotAttributed(t *testing.T) {
	gitOrSkip(t)
	dir := initRepo(t)

	// Nothing happens during the "turn".
	s0 := takeWorktreeSnapshot(dir)
	s1 := takeWorktreeSnapshot(dir)
	if changed := s1.changedSince(s0); changed != nil {
		t.Fatalf("changedSince = %v, want nil (user's dirty file is baseline)", changed)
	}
}

func TestWorktreeSnapshotNonGitDir(t *testing.T) {
	gitOrSkip(t)
	dir := t.TempDir()
	s := takeWorktreeSnapshot(dir)
	if s.root != "" {
		t.Fatalf("expected unavailable snapshot for non-git dir, got root %q", s.root)
	}
	if changed := s.changedSince(worktreeSnapshot{}); changed != nil {
		t.Fatalf("changedSince on unavailable snapshot = %v, want nil", changed)
	}
}

func TestParsePorcelainZ(t *testing.T) {
	// A rename record carries a second NUL-terminated origin path that must
	// be consumed, not parsed as a standalone entry.
	raw := []byte("R  new.go\x00old.go\x00?? added.go\x00 M mod.go\x00")
	entries := parsePorcelainZ(raw)
	want := []porcelainEntry{
		{status: "R ", path: "new.go"},
		{status: "??", path: "added.go"},
		{status: " M", path: "mod.go"},
	}
	if len(entries) != len(want) {
		t.Fatalf("parsePorcelainZ = %+v, want %+v", entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Fatalf("entry %d = %+v, want %+v", i, entries[i], want[i])
		}
	}
}
