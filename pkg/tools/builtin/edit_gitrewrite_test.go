package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/models"
)

func TestLikelyGitRewrite(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"git checkout -b feat/x && wc -l miniapp/src", true},
		{"cd /repo && git rebase main", true},
		{"git merge --abort", true},
		{"GIT_EDITOR=true git cherry-pick 1a2b3c", true},
		{"git -C /elsewhere status", false},
		{"git status", false},
		{"git log --oneline -5", false},
		{"git diff && git add x.go && git commit -m 'y'", false},
		{"echo git checkout main", true},
		{"gofmt -l . && go test ./...", false},
		{"git status; git checkout main", true},
		{"git diff\ngit rebase main", true},
	}
	for _, c := range cases {
		if got := likelyGitRewrite(c.cmd); got != c.want {
			t.Errorf("likelyGitRewrite(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestEditReadGate_SameLengthContentChangeRejected(t *testing.T) {
	ctx := gateCtx(t)
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFileHandler(ctx, gateReadCall(path)); err != nil {
		t.Fatal(err)
	}
	// Same byte length, different content: the size shortcut must not save
	// it, only the content fingerprint may — and this one differs.
	if err := os.WriteFile(path, []byte("alpha\nzeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := EditFileHandler(ctx, gateEditCall(path))
	if err == nil || !strings.Contains(err.Error(), "changed on disk") {
		t.Fatalf("same-length content change must be rejected, got %v", err)
	}
}

// contentPreservingRewrite simulates what git checkout/rebase does to a file
// it did not change: same bytes, fresh mtime.
func contentPreservingRewrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
}

func TestEditReadGate_ContentPreservingRewriteAllowed(t *testing.T) {
	ctx := gateCtx(t)
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFileHandler(ctx, gateReadCall(path)); err != nil {
		t.Fatal(err)
	}
	contentPreservingRewrite(t, path, "alpha\nbeta\n")
	if _, err := EditFileHandler(ctx, gateEditCall(path)); err != nil {
		t.Fatalf("content-preserving rewrite (git checkout of an unchanged file) should not demand a re-read, got %v", err)
	}
}

func TestEditReadGate_GitRewriteExplainsStale(t *testing.T) {
	ctx := gateCtx(t)
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFileHandler(ctx, gateReadCall(path)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("rewritten by rebase\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// `echo git checkout main` trips the detector without needing a repo; the
	// handler fires NoteGitRewrite regardless of the command's exit path.
	if _, err := BashHandler(ctx, models.ToolCall{
		ID: "b", Name: "bash", Arguments: map[string]any{"command": "echo git checkout main"},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := EditFileHandler(ctx, gateEditCall(path))
	if err == nil || !strings.Contains(err.Error(), "changed on disk") {
		t.Fatalf("want stale-read gate error, got %v", err)
	}
	if !strings.Contains(err.Error(), "git command") {
		t.Fatalf("stale error after a git rewrite command should carry the git cause, got %v", err)
	}
}

func TestEditReadGate_ExternalChangeHasNoGitCause(t *testing.T) {
	ctx := gateCtx(t)
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFileHandler(ctx, gateReadCall(path)); err != nil {
		t.Fatal(err)
	}
	if _, err := BashHandler(ctx, models.ToolCall{
		ID: "b", Name: "bash", Arguments: map[string]any{"command": "echo git status"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("external edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := EditFileHandler(ctx, gateEditCall(path))
	if err == nil || !strings.Contains(err.Error(), "changed on disk") {
		t.Fatalf("want stale-read gate error, got %v", err)
	}
	if strings.Contains(err.Error(), "git command") {
		t.Fatalf("no git rewrite ran; error must not claim one, got %v", err)
	}
}
