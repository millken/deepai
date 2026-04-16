package tools

import (
	"context"
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
