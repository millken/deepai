package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
)

// mockProvider returns a fixed commit message.
type mockProvider struct {
	message string
	err     error
}

func (m *mockProvider) Chat(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	if m.err != nil {
		return llm.ChatResponse{}, m.err
	}
	return llm.ChatResponse{
		Message: models.Message{Role: models.RoleAI, Content: m.message},
	}, nil
}

func (m *mockProvider) Stream(_ context.Context, _ llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk, 1)
	close(ch)
	return ch, nil
}

// capturingProvider records the user prompt sent to Chat and returns a fixed message.
type capturingProvider struct {
	mu       sync.Mutex
	captured string
	response string
}

func (c *capturingProvider) Chat(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range req.Messages {
		c.captured += m.Content
	}
	return llm.ChatResponse{Message: models.Message{Content: c.response}}, nil
}

func (c *capturingProvider) Stream(_ context.Context, _ llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk, 1)
	close(ch)
	return ch, nil
}

func (c *capturingProvider) capturedPrompt() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.captured
}

// initAutoCommitRepo creates a git repo with an initial commit.
func initAutoCommitRepo(t *testing.T) string {
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
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0644)
	run("add", "README.md")
	run("commit", "-m", "init")
	return dir
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

func TestGitAutoCommit_NoStagedChanges(t *testing.T) {
	dir := initAutoCommitRepo(t)
	tool := GitAutoCommitTool(nil)

	_, err := tool.Handler(context.Background(), models.ToolCall{
		ID:        "call-1",
		Name:      "git_auto_commit",
		Arguments: map[string]any{"working_dir": dir},
	})
	if err == nil {
		t.Fatal("expected error when no staged changes")
	}
	if !strings.Contains(err.Error(), "no staged changes") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGitAutoCommit_EmptyFilesParameter(t *testing.T) {
	dir := initAutoCommitRepo(t)
	tool := GitAutoCommitTool(nil)

	_, err := tool.Handler(context.Background(), models.ToolCall{
		ID:   "call-1",
		Name: "git_auto_commit",
		Arguments: map[string]any{
			"working_dir": dir,
			"files":       []interface{}{"", "  "},
		},
	})
	if err == nil {
		t.Fatal("expected error for empty files")
	}
	if !strings.Contains(err.Error(), "no valid paths") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGitAutoCommit_WithExplicitFiles(t *testing.T) {
	dir := initAutoCommitRepo(t)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0644)

	provider := &mockProvider{message: "feat: add main.go"}
	tool := GitAutoCommitTool(provider)

	result, err := tool.Handler(context.Background(), models.ToolCall{
		ID:   "call-1",
		Name: "git_auto_commit",
		Arguments: map[string]any{
			"working_dir": dir,
			"files":       []interface{}{"main.go"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "feat: add main.go") {
		t.Fatalf("expected commit message in result, got: %s", result.Content)
	}
}

func TestGitAutoCommit_PreStagedFilesRejected(t *testing.T) {
	dir := initAutoCommitRepo(t)
	// Pre-stage an unrelated file
	os.WriteFile(filepath.Join(dir, "unrelated.go"), []byte("package unrelated\n"), 0644)
	cmd := exec.Command("git", "add", "--", "unrelated.go")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %s: %v", out, err)
	}
	// Now try to commit a different file
	os.WriteFile(filepath.Join(dir, "target.go"), []byte("package target\n"), 0644)

	tool := GitAutoCommitTool(&mockProvider{message: "feat: add target"})

	_, err := tool.Handler(context.Background(), models.ToolCall{
		ID:   "call-1",
		Name: "git_auto_commit",
		Arguments: map[string]any{
			"working_dir": dir,
			"files":       []interface{}{"target.go"},
		},
	})
	if err == nil {
		t.Fatal("expected error when pre-staged unrelated files exist")
	}
	if !strings.Contains(err.Error(), "pre-staged files not in the requested list") {
		t.Fatalf("expected pre-staged error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "unrelated.go") {
		t.Fatalf("error should name the extra file, got: %v", err)
	}
}

func TestGitAutoCommit_PartialStagedPromptExcludesUnstaged(t *testing.T) {
	dir := initAutoCommitRepo(t)
	// Create two files
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0644)
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("package b\n"), 0644)
	// Stage only a.go
	cmd := exec.Command("git", "add", "--", "a.go")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %s: %v", out, err)
	}

	// Use capturing provider to verify the LLM prompt content
	provider := &capturingProvider{response: "feat: add a module"}
	tool := GitAutoCommitTool(provider)

	_, err := tool.Handler(context.Background(), models.ToolCall{
		ID:        "call-1",
		Name:      "git_auto_commit",
		Arguments: map[string]any{"working_dir": dir},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	prompt := provider.capturedPrompt()
	if strings.Contains(prompt, "b.go") {
		t.Fatalf("LLM prompt should NOT contain unstaged b.go.\nPrompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "a.go") {
		t.Fatalf("LLM prompt should contain staged a.go.\nPrompt:\n%s", prompt)
	}
}

func TestGitAutoCommit_PushFailure(t *testing.T) {
	dir := initAutoCommitRepo(t)
	os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n"), 0644)

	provider := &mockProvider{message: "chore: add x"}
	tool := GitAutoCommitTool(provider)

	result, err := tool.Handler(context.Background(), models.ToolCall{
		ID:   "call-1",
		Name: "git_auto_commit",
		Arguments: map[string]any{
			"working_dir": dir,
			"files":       []interface{}{"x.go"},
			"auto_push":   true,
		},
	})
	if err == nil {
		t.Fatal("expected error for push failure")
	}
	if !strings.Contains(err.Error(), "push failed") {
		t.Fatalf("expected push failure error, got: %v", err)
	}
	if result.Content == "" {
		t.Fatal("result content should contain commit info even when push fails")
	}
}

func TestGitAutoCommit_FallbackWhenNoProvider(t *testing.T) {
	dir := initAutoCommitRepo(t)
	os.WriteFile(filepath.Join(dir, "fallback.go"), []byte("package fallback\n"), 0644)

	tool := GitAutoCommitTool(nil)

	result, err := tool.Handler(context.Background(), models.ToolCall{
		ID:   "call-1",
		Name: "git_auto_commit",
		Arguments: map[string]any{
			"working_dir": dir,
			"files":       []interface{}{"fallback.go"},
			"description": "test fallback",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "test fallback") {
		t.Fatalf("expected fallback message with 'test fallback', got: %s", result.Content)
	}
}

func TestGitAutoCommit_WithAuthorOverride(t *testing.T) {
	dir := initAutoCommitRepo(t)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global.gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "")
	t.Setenv("GIT_AUTHOR_EMAIL", "")
	t.Setenv("GIT_COMMITTER_NAME", "")
	t.Setenv("GIT_COMMITTER_EMAIL", "")
	gitOutput(t, dir, "config", "--unset", "user.name")
	gitOutput(t, dir, "config", "--unset", "user.email")
	if err := os.WriteFile(filepath.Join(dir, "override.go"), []byte("package override\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tool := GitAutoCommitTool(&mockProvider{message: "feat: add override"})
	_, err := tool.Handler(context.Background(), models.ToolCall{
		ID:   "call-author-override",
		Name: "git_auto_commit",
		Arguments: map[string]any{
			"working_dir":  dir,
			"files":        []interface{}{"override.go"},
			"author_name":  "Tool Bot",
			"author_email": "toolbot@example.com",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := gitOutput(t, dir, "log", "-1", "--pretty=format:%an <%ae>"); got != "Tool Bot <toolbot@example.com>" {
		t.Fatalf("unexpected author: %q", got)
	}
}

func TestGitAutoCommit_MissingIdentityMessage(t *testing.T) {
	dir := initAutoCommitRepo(t)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global.gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "")
	t.Setenv("GIT_AUTHOR_EMAIL", "")
	t.Setenv("GIT_COMMITTER_NAME", "")
	t.Setenv("GIT_COMMITTER_EMAIL", "")
	gitOutput(t, dir, "config", "--unset", "user.name")
	gitOutput(t, dir, "config", "--unset", "user.email")
	if err := os.WriteFile(filepath.Join(dir, "missing.go"), []byte("package missing\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tool := GitAutoCommitTool(&mockProvider{message: "feat: add missing"})
	_, err := tool.Handler(context.Background(), models.ToolCall{
		ID:   "call-missing-identity",
		Name: "git_auto_commit",
		Arguments: map[string]any{
			"working_dir": dir,
			"files":       []interface{}{"missing.go"},
		},
	})
	if err == nil {
		t.Fatal("expected identity error")
	}
	if !strings.Contains(err.Error(), "git user identity is not configured") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGitAutoCommit_SetUpstreamOnPush(t *testing.T) {
	dir := initAutoCommitRepo(t)
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	cmd := exec.Command("git", "init", "--bare", remoteDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %s: %v", out, err)
	}
	gitOutput(t, dir, "remote", "add", "origin", remoteDir)
	gitOutput(t, dir, "checkout", "-b", "feature/tool-push")
	if err := os.WriteFile(filepath.Join(dir, "push.go"), []byte("package push\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tool := GitAutoCommitTool(&mockProvider{message: "feat: add push"})
	result, err := tool.Handler(context.Background(), models.ToolCall{
		ID:   "call-push-upstream",
		Name: "git_auto_commit",
		Arguments: map[string]any{
			"working_dir":  dir,
			"files":        []interface{}{"push.go"},
			"auto_push":    true,
			"set_upstream": true,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := gitOutput(t, dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); got != "origin/feature/tool-push" {
		t.Fatalf("unexpected upstream: %q", got)
	}
	var payload struct {
		Pushed      bool `json:"pushed"`
		SetUpstream bool `json:"set_upstream"`
	}
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !payload.Pushed || !payload.SetUpstream {
		t.Fatalf("unexpected push payload: %+v", payload)
	}
}

func TestGitAutoCommit_NormalizesRequestedPaths(t *testing.T) {
	dir := initAutoCommitRepo(t)
	path := filepath.Join(dir, "normalized.go")
	if err := os.WriteFile(path, []byte("package normalized\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, dir, "add", "--", "normalized.go")

	tool := GitAutoCommitTool(&mockProvider{message: "feat: normalize path"})
	_, err := tool.Handler(context.Background(), models.ToolCall{
		ID:   "call-normalize",
		Name: "git_auto_commit",
		Arguments: map[string]any{
			"working_dir": dir,
			"files":       []interface{}{path},
		},
	})
	if err != nil {
		t.Fatalf("absolute path should normalize to staged repo-relative path: %v", err)
	}
	if got := gitOutput(t, dir, "log", "-1", "--pretty=%s"); got != "feat: normalize path" {
		t.Fatalf("unexpected commit message: %q", got)
	}
}

func TestCleanCommitMessage(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"feat: add feature\n\nSome extra body", "feat: add feature"},
		{"```feat: add feature```", "feat: add feature"},
		{`"feat: add feature"`, "feat: add feature"},
		{"```text\nfeat: add feature\n```", "feat: add feature"},
		{"  feat: add feature  ", "feat: add feature"},
	}
	for _, tt := range tests {
		got := cleanCommitMessage(tt.input)
		if got != tt.want {
			t.Errorf("cleanCommitMessage(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestToSet(t *testing.T) {
	set := toSet([]string{"a.go", "b.go", "", "a.go"})
	if len(set) != 2 {
		t.Fatalf("expected 2 items, got %d", len(set))
	}
	if !set["a.go"] || !set["b.go"] {
		t.Fatal("expected a.go and b.go in set")
	}
}
