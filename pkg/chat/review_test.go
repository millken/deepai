package chat

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// Committing a change makes it clean, and the dirty-state delta then
// reports it exactly as it reports a revert: the path simply leaves
// `git status --porcelain`. changesSince is what keeps a turn that
// committed its own work attributable — without it the mission gate saw an
// empty change set, called it idle, and handed a finished change back
// unreviewed.
func TestWorktreeSnapshotCommittedChangeStaysAttributed(t *testing.T) {
	gitOrSkip(t)
	dir := initRepo(t)

	s0 := takeWorktreeSnapshot(dir)
	if s0.head == "" {
		t.Fatal("expected a resolvable HEAD in a repo with one commit")
	}

	writeFileOrFatal(t, filepath.Join(dir, "committed.go"), "package x\n\nfunc F() {}\n")
	runGitOrFatal(t, dir, "add", "committed.go")
	runGitOrFatal(t, dir, "commit", "-q", "-m", "the turn's work")

	s1 := takeWorktreeSnapshot(dir)
	if got := s1.changedSince(s0); len(got) != 0 {
		t.Fatalf("the dirty delta sees %v; a committed file is clean, which is the whole problem", got)
	}
	changed := s1.changesSince(s0)
	if len(changed) != 1 || filepath.Base(changed[0]) != "committed.go" {
		t.Fatalf("changesSince = %v, want exactly {committed.go}", changed)
	}
	// The user's own untracked file was not committed and did not change:
	// commit attribution must not widen the baseline's exclusions.
	for _, p := range changed {
		if filepath.Base(p) == "userdirty.go" {
			t.Errorf("the user's pre-turn dirty file leaked into attribution: %v", changed)
		}
	}
}

// Commit attribution needs two resolvable HEADs. A repo with no commits
// yet (or a baseline written before this existed) must degrade to the
// dirty delta rather than guess.
func TestWorktreeSnapshotCommitAttributionNeedsTwoHeads(t *testing.T) {
	gitOrSkip(t)
	dir := t.TempDir()
	runGitOrFatal(t, dir, "init", "-q")

	s0 := takeWorktreeSnapshot(dir)
	if s0.root == "" {
		t.Fatal("expected a git snapshot in a fresh repo")
	}
	if s0.head != "" {
		t.Fatalf("head = %q in a repo with no commits, want empty", s0.head)
	}
	writeFileOrFatal(t, filepath.Join(dir, "first.go"), "package x\n")
	runGitOrFatal(t, dir, "add", "first.go")
	runGitOrFatal(t, dir, "commit", "-q", "-m", "first")

	s1 := takeWorktreeSnapshot(dir)
	if got := s1.committedSince(s0); got != nil {
		t.Fatalf("committedSince = %v with no baseline commit, want nil", got)
	}
	if got := s1.committedSince(s1); got != nil {
		t.Fatalf("committedSince = %v for an unmoved HEAD, want nil", got)
	}
}

// buildReviewDiff must diff from the baseline COMMIT when HEAD moved: a
// bare `git diff -- <path>` on a committed change is empty, and an empty
// diff is the one input that makes a reviewer pass a change it never saw.
func TestBuildReviewDiffCoversCommittedWork(t *testing.T) {
	gitOrSkip(t)
	dir := initRepo(t)

	base := takeWorktreeSnapshot(dir)
	writeFileOrFatal(t, filepath.Join(dir, "committed.go"), "package x\n\nfunc Committed() {}\n")
	runGitOrFatal(t, dir, "add", "committed.go")
	runGitOrFatal(t, dir, "commit", "-q", "-m", "the turn's work")
	writeFileOrFatal(t, filepath.Join(dir, "committed.go"), "package x\n\nfunc Committed() {}\n\nfunc Staged() {}\n")

	snap := takeWorktreeSnapshot(dir)
	scope := []string{filepath.Join(snap.root, "committed.go")}

	// Without a baseline the diff is index-relative: the committed hunk
	// appears only as context, never as the change under review.
	if diff, _ := buildReviewDiff(dir, snap, worktreeSnapshot{}, scope); strings.Contains(diff, "+func Committed()") {
		t.Fatalf("index-relative diff unexpectedly carries the committed hunk:\n%s", diff)
	}
	diff, oversized := buildReviewDiff(dir, snap, base, scope)
	if oversized {
		t.Fatal("small diff flagged oversized")
	}
	for _, want := range []string{"+func Committed()", "+func Staged()"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff from the baseline commit is missing %q:\n%s", want, diff)
		}
	}
}

// Staging is the step before the commit the gate already learned to see.
// `git diff` (worktree vs index) is empty once the edit is added, which is
// the same empty diff that makes a reviewer pass a change it never saw.
func TestBuildReviewDiffCoversStagedWork(t *testing.T) {
	gitOrSkip(t)
	dir := initRepo(t)

	base := takeWorktreeSnapshot(dir)
	writeFileOrFatal(t, filepath.Join(dir, "committed.go"), "package x\n\nfunc StagedEdit() {}\n")
	runGitOrFatal(t, dir, "add", "committed.go")
	writeFileOrFatal(t, filepath.Join(dir, "brand_new.go"), "package x\n\nfunc StagedNew() {}\n")
	runGitOrFatal(t, dir, "add", "brand_new.go")

	snap := takeWorktreeSnapshot(dir)
	if snap.head != base.head {
		t.Fatal("staging must not move HEAD — that is the committed-work case")
	}
	scope := []string{
		filepath.Join(snap.root, "committed.go"),
		filepath.Join(snap.root, "brand_new.go"),
	}

	diff, oversized := buildReviewDiff(dir, snap, base, scope)
	if oversized {
		t.Fatal("small diff flagged oversized")
	}
	for _, want := range []string{"+func StagedEdit()", "+func StagedNew()"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("diff is missing %q:\n%s", want, diff)
		}
	}

	// Manual /review has no baseline. Already-committed history must stay
	// out, but the staged edit is the dirty tree the user asked to review.
	manual, _ := buildReviewDiff(dir, snap, worktreeSnapshot{}, scope)
	if !strings.Contains(manual, "+func StagedEdit()") || !strings.Contains(manual, "+func StagedNew()") {
		t.Fatalf("manual review diff dropped staged work:\n%s", manual)
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
