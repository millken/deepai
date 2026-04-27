package builtin

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

// initGitRepo creates a temporary git repo with an initial commit.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %s: %v", strings.Join(args, " "), out, err)
		}
	}
	run("init")
	run("config", "user.name", "test")
	run("config", "user.email", "test@test.com")
	run("config", "commit.gpgsign", "false")
	// Initial commit so HEAD exists
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0644)
	run("add", "README.md")
	run("commit", "-m", "init")
	return dir
}

func callID(t *testing.T) string {
	t.Helper()
	return "call-1"
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), out, err)
	}
	return strings.TrimSpace(string(out))
}

func TestGitStatusHandler_Clean(t *testing.T) {
	dir := initGitRepo(t)
	result, err := GitStatusHandler(context.Background(), models.ToolCall{
		ID:        callID(t),
		Name:      "git_status",
		Arguments: map[string]any{"working_dir": dir},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, `"clean":true`) {
		t.Fatalf("expected clean repo, got: %s", result.Content)
	}
}

func TestGitStatusHandler_Dirty(t *testing.T) {
	dir := initGitRepo(t)
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("hello"), 0644)

	result, err := GitStatusHandler(context.Background(), models.ToolCall{
		ID:        callID(t),
		Name:      "git_status",
		Arguments: map[string]any{"working_dir": dir},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Content, `"clean":true`) {
		t.Fatal("expected dirty repo")
	}
}

func TestGitStatusHandler_StructuredData(t *testing.T) {
	dir := initGitRepo(t)
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("hello"), 0644)
	result, err := GitStatusHandler(context.Background(), models.ToolCall{
		ID:        callID(t),
		Name:      "git_status",
		Arguments: map[string]any{"working_dir": dir},
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Clean     bool `json:"clean"`
		Untracked []string `json:"untracked"`
	}
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if payload.Clean || len(payload.Untracked) != 1 || payload.Untracked[0] != "new.txt" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	if result.Data["entries"] == nil {
		t.Fatal("expected structured entries in result.Data")
	}
}

func TestGitLogHandler(t *testing.T) {
	dir := initGitRepo(t)
	result, err := GitLogHandler(context.Background(), models.ToolCall{
		ID:        callID(t),
		Name:      "git_log",
		Arguments: map[string]any{"working_dir": dir, "oneline": true, "count": float64(5)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "init") {
		t.Fatalf("expected log with 'init', got: %s", result.Content)
	}
}

func TestGitLogHandler_StructuredEntries(t *testing.T) {
	dir := initGitRepo(t)
	result, err := GitLogHandler(context.Background(), models.ToolCall{
		ID:        callID(t),
		Name:      "git_log",
		Arguments: map[string]any{"working_dir": dir, "count": float64(1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(payload.Entries) != 1 {
		t.Fatalf("unexpected entries: %+v", payload.Entries)
	}
	if payload.Entries[0]["subject"] != "init" {
		t.Fatalf("unexpected subject: %+v", payload.Entries[0])
	}
}

func TestGitDiffHandler_StructuredData(t *testing.T) {
	dir := initGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "diff.txt"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, dir, "add", "--", "diff.txt")
	result, err := GitDiffHandler(context.Background(), models.ToolCall{
		ID:        callID(t),
		Name:      "git_diff",
		Arguments: map[string]any{"working_dir": dir, "staged": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Staged bool     `json:"staged"`
		Files  []string `json:"files"`
		Diff   string   `json:"diff"`
	}
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !payload.Staged || len(payload.Files) != 1 || payload.Files[0] != "diff.txt" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	if !strings.Contains(payload.Diff, "+hello") {
		t.Fatalf("unexpected diff: %q", payload.Diff)
	}
}

func TestGitAddHandler_WithDoubleDash(t *testing.T) {
	dir := initGitRepo(t)
	// Create a file with a name that could be mistaken for an option
	os.WriteFile(filepath.Join(dir, "-wierd.txt"), []byte("tricky"), 0644)

	result, err := GitAddHandler(context.Background(), models.ToolCall{
		ID:   callID(t),
		Name: "git_add",
		Arguments: map[string]any{
			"working_dir": dir,
			"files":       []interface{}{"-wierd.txt"},
		},
	})
	if err != nil {
		t.Fatalf("git add with dash-prefixed file should succeed: %v", err)
	}
	if !strings.Contains(result.Content, "-wierd.txt") {
		t.Fatalf("expected staged file in output, got: %s", result.Content)
	}
}

func TestGitAddHandler_NoFiles(t *testing.T) {
	_, err := GitAddHandler(context.Background(), models.ToolCall{
		ID:        callID(t),
		Name:      "git_add",
		Arguments: map[string]any{"working_dir": t.TempDir(), "files": []interface{}{}},
	})
	if err == nil {
		t.Fatal("expected error for empty files array")
	}
}

func TestGitCommitHandler_NoStagedChanges(t *testing.T) {
	dir := initGitRepo(t)
	_, err := GitCommitHandler(context.Background(), models.ToolCall{
		ID:   callID(t),
		Name: "git_commit",
		Arguments: map[string]any{
			"working_dir": dir,
			"message":     "should fail",
		},
	})
	if err == nil {
		t.Fatal("expected error when no staged changes")
	}
}

func TestGitCommitHandler_WithAuthorOverride(t *testing.T) {
	dir := initGitRepo(t)
	gitOutput(t, dir, "config", "--unset", "user.name")
	gitOutput(t, dir, "config", "--unset", "user.email")
	if err := os.WriteFile(filepath.Join(dir, "override.txt"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, dir, "add", "--", "override.txt")

	_, err := GitCommitHandler(context.Background(), models.ToolCall{
		ID:   callID(t),
		Name: "git_commit",
		Arguments: map[string]any{
			"working_dir":  dir,
			"message":      "override author",
			"author_name":  "Tool Bot",
			"author_email": "toolbot@example.com",
		},
	})
	if err != nil {
		t.Fatalf("expected commit with explicit author override: %v", err)
	}
	if got := gitOutput(t, dir, "log", "-1", "--pretty=format:%an <%ae>"); got != "Tool Bot <toolbot@example.com>" {
		t.Fatalf("unexpected author: %q", got)
	}
}

func TestGitCommitHandler_MissingIdentityMessage(t *testing.T) {
	dir := initGitRepo(t)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global.gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "")
	t.Setenv("GIT_AUTHOR_EMAIL", "")
	t.Setenv("GIT_COMMITTER_NAME", "")
	t.Setenv("GIT_COMMITTER_EMAIL", "")
	gitOutput(t, dir, "config", "--unset", "user.name")
	gitOutput(t, dir, "config", "--unset", "user.email")
	if err := os.WriteFile(filepath.Join(dir, "missing.txt"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, dir, "add", "--", "missing.txt")

	_, err := GitCommitHandler(context.Background(), models.ToolCall{
		ID:   callID(t),
		Name: "git_commit",
		Arguments: map[string]any{
			"working_dir": dir,
			"message":     "missing identity",
		},
	})
	if err == nil {
		t.Fatal("expected identity error")
	}
	if !strings.Contains(err.Error(), "git user identity is not configured") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGitPushHandler_NoWorkingTreeCheck(t *testing.T) {
	dir := initGitRepo(t)
	// Create an uncommitted file — this should NOT block push
	os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("uncommitted"), 0644)

	_, err := GitPushHandler(context.Background(), models.ToolCall{
		ID:        callID(t),
		Name:      "git_push",
		Arguments: map[string]any{"working_dir": dir},
	})
	// Push will fail (no remote configured), but it should NOT fail with
	// "there are uncommitted changes" — that check has been removed.
	if err != nil && strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatal("git_push should not check for uncommitted changes")
	}
}

func TestGitPushHandler_SetUpstream(t *testing.T) {
	dir := initGitRepo(t)
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	cmd := exec.Command("git", "init", "--bare", remoteDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %s: %v", out, err)
	}
	gitOutput(t, dir, "remote", "add", "origin", remoteDir)
	gitOutput(t, dir, "checkout", "-b", "feature/tool-push")
	if err := os.WriteFile(filepath.Join(dir, "push.txt"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, dir, "add", "--", "push.txt")
	gitOutput(t, dir, "commit", "-m", "push me")

	result, err := GitPushHandler(context.Background(), models.ToolCall{
		ID:   callID(t),
		Name: "git_push",
		Arguments: map[string]any{
			"working_dir":  dir,
			"set_upstream": true,
		},
	})
	if err != nil {
		t.Fatalf("push with upstream should succeed: %v", err)
	}
	if got := gitOutput(t, dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); got != "origin/feature/tool-push" {
		t.Fatalf("unexpected upstream: %q", got)
	}
	var payload struct {
		SetUpstream bool `json:"set_upstream"`
	}
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !payload.SetUpstream {
		t.Fatal("expected set_upstream=true in result")
	}
}

func TestGitResetHandler_UnstageFiles(t *testing.T) {
	dir := initGitRepo(t)
	// Stage two files
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0644)
	cmd := exec.Command("git", "add", "--", "a.txt", "b.txt")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %s: %v", out, err)
	}

	// Reset only a.txt
	_, err := GitResetHandler(context.Background(), models.ToolCall{
		ID:   "call-1",
		Name: "git_reset",
		Arguments: map[string]any{
			"working_dir": dir,
			"files":       []interface{}{"a.txt"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify a.txt is unstaged but b.txt is still staged
	cmd = exec.Command("git", "diff", "--cached", "--name-only")
	cmd.Dir = dir
	output, _ := cmd.Output()
	staged := string(output)
	if strings.Contains(staged, "a.txt") {
		t.Fatal("a.txt should be unstaged")
	}
	if !strings.Contains(staged, "b.txt") {
		t.Fatal("b.txt should still be staged")
	}
}

func TestGitResetHandler_EmptyFilesParameter(t *testing.T) {
	dir := initGitRepo(t)
	// Stage a file so the index is non-empty
	os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep"), 0644)
	cmd := exec.Command("git", "add", "--", "keep.txt")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %s: %v", out, err)
	}

	// Pass files with only empty/whitespace entries — should error, not reset everything
	_, err := GitResetHandler(context.Background(), models.ToolCall{
		ID:   "call-1",
		Name: "git_reset",
		Arguments: map[string]any{
			"working_dir": dir,
			"files":       []interface{}{"", "  "},
		},
	})
	if err == nil {
		t.Fatal("expected error for empty files parameter")
	}
	if !strings.Contains(err.Error(), "no valid paths") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the index was NOT modified
	cmd = exec.Command("git", "diff", "--cached", "--name-only")
	cmd.Dir = dir
	output, _ := cmd.Output()
	if !strings.Contains(string(output), "keep.txt") {
		t.Fatal("keep.txt should still be staged — reset should not have run")
	}
}
