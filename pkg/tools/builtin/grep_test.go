package builtin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func createTestTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(path), 0755)
		os.WriteFile(path, []byte(content), 0644)
	}
	return root
}

func TestGrepHandler_BasicMatch(t *testing.T) {
	root := createTestTree(t, map[string]string{
		"a.txt": "hello world\nfoo bar\nhello again",
		"b.txt": "no match here",
	})

	result, err := GrepHandler(context.Background(), models.ToolCall{
		ID:     "grep-1",
		Name:   "grep",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"pattern": "hello",
			"path":    root,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("result error: %s", result.Error)
	}
	// Should find 2 matches in a.txt
	if !contains(result.Content, "a.txt:1:") {
		t.Errorf("missing a.txt:1, got: %s", result.Content)
	}
	if !contains(result.Content, "a.txt:3:") {
		t.Errorf("missing a.txt:3, got: %s", result.Content)
	}
	if contains(result.Content, "b.txt") {
		t.Errorf("should not match b.txt, got: %s", result.Content)
	}
}

func TestGrepHandler_CaseInsensitive(t *testing.T) {
	root := createTestTree(t, map[string]string{
		"a.txt": "Hello World\nhello world",
	})

	result, err := GrepHandler(context.Background(), models.ToolCall{
		ID:     "grep-2",
		Name:   "grep",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"pattern":          "HELLO",
			"path":             root,
			"case_insensitive": true,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("result error: %s", result.Error)
	}
	if !contains(result.Content, "a.txt:1:") || !contains(result.Content, "a.txt:2:") {
		t.Errorf("expected 2 matches, got: %s", result.Content)
	}
}

func TestGrepHandler_GlobFilter(t *testing.T) {
	root := createTestTree(t, map[string]string{
		"a.go": "func main()",
		"b.ts": "function hello()",
	})

	result, err := GrepHandler(context.Background(), models.ToolCall{
		ID:     "grep-3",
		Name:   "grep",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"pattern": "func",
			"path":    root,
			"glob":    "*.go",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("result error: %s", result.Error)
	}
	if !contains(result.Content, "a.go") {
		t.Errorf("expected a.go match, got: %s", result.Content)
	}
	if contains(result.Content, "b.ts") {
		t.Errorf("should not match b.ts with glob *.go, got: %s", result.Content)
	}
}

func TestGrepHandler_NoMatch(t *testing.T) {
	root := createTestTree(t, map[string]string{
		"a.txt": "hello world",
	})

	result, err := GrepHandler(context.Background(), models.ToolCall{
		ID:     "grep-4",
		Name:   "grep",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"pattern": "notfound",
			"path":    root,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Content != "No matches found." {
		t.Errorf("expected no matches message, got: %s", result.Content)
	}
}

func TestGrepHandler_SkipsHiddenAndBinary(t *testing.T) {
	root := createTestTree(t, map[string]string{
		".hidden/a.txt":  "secret stuff",
		"normal/a.txt":   "public hello",
		"img/a.png":      "public hello",
		"node_modules/a.txt": "dependency code",
	})

	result, err := GrepHandler(context.Background(), models.ToolCall{
		ID:     "grep-5",
		Name:   "grep",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"pattern": "public hello",
			"path":    root,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("result error: %s", result.Error)
	}
	if contains(result.Content, ".hidden") {
		t.Errorf("should skip .hidden dir, got: %s", result.Content)
	}
	if contains(result.Content, "node_modules") {
		t.Errorf("should skip node_modules, got: %s", result.Content)
	}
	if contains(result.Content, "img") {
		t.Errorf("should skip .png files, got: %s", result.Content)
	}
	if !contains(result.Content, "normal") {
		t.Errorf("expected normal/a.txt match, got: %s", result.Content)
	}
}

func TestGrepHandler_RegexPattern(t *testing.T) {
	root := createTestTree(t, map[string]string{
		"a.txt": "func (s *Server) Start()",
		"b.txt": "type Server struct",
	})

	result, err := GrepHandler(context.Background(), models.ToolCall{
		ID:     "grep-6",
		Name:   "grep",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"pattern": `func.*Server.*Start`,
			"path":    root,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("result error: %s", result.Error)
	}
	if !contains(result.Content, "a.txt:1:") {
		t.Errorf("expected a.go match, got: %s", result.Content)
	}
	if contains(result.Content, "b.txt") {
		t.Errorf("should not match b.txt, got: %s", result.Content)
	}
}

func TestGrepHandler_MaxResults(t *testing.T) {
	root := createTestTree(t, map[string]string{
		"a.txt": "match line 1\nmatch line 2\nmatch line 3\nmatch line 4\nmatch line 5",
	})

	result, err := GrepHandler(context.Background(), models.ToolCall{
		ID:     "grep-7",
		Name:   "grep",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"pattern":     "match",
			"path":        root,
			"max_results": float64(2),
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("result error: %s", result.Error)
	}
	if !contains(result.Content, "results capped at 2") {
		t.Errorf("expected cap notice, got: %s", result.Content)
	}
}

func TestGrepHandler_MissingPattern(t *testing.T) {
	_, err := GrepHandler(context.Background(), models.ToolCall{
		ID:     "grep-8",
		Name:   "grep",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"path": "/tmp",
		},
	})
	if err == nil {
		t.Fatal("expected error when pattern is missing")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && searchString(s, sub)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
