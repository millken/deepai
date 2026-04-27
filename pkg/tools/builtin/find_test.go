package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

func TestFindHandler_ByName(t *testing.T) {
	root := createTestTree(t, map[string]string{
		"src/main.go":        "code",
		"src/util.go":        "code",
		"src/main_test.go":   "test",
		"pkg/helper.go":      "code",
		"pkg/helper_test.go": "test",
		"docs/README.md":     "doc",
	})

	result, err := FindHandler(context.Background(), models.ToolCall{
		ID:     "find-1",
		Name:   "find",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"path": root,
			"name": "*_test.go",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("result error: %s", result.Error)
	}
	if !contains(result.Content, "main_test.go") {
		t.Errorf("expected main_test.go, got: %s", result.Content)
	}
	if !contains(result.Content, "helper_test.go") {
		t.Errorf("expected helper_test.go, got: %s", result.Content)
	}
	if contains(result.Content, "main.go") && !contains(result.Content, "main_test.go") {
		// main.go alone should not match
		t.Errorf("should not match main.go without _test, got: %s", result.Content)
	}
}

func TestFindHandler_ByExtension(t *testing.T) {
	root := createTestTree(t, map[string]string{
		"src/a.go":  "code",
		"src/b.ts":  "code",
		"src/c.go":  "code",
		"readme.md": "doc",
	})

	result, err := FindHandler(context.Background(), models.ToolCall{
		ID:     "find-2",
		Name:   "find",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"path": root,
			"name": "*.go",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("result error: %s", result.Error)
	}
	if !contains(result.Content, "a.go") || !contains(result.Content, "c.go") {
		t.Errorf("expected a.go and c.go, got: %s", result.Content)
	}
	if contains(result.Content, "b.ts") {
		t.Errorf("should not match b.ts, got: %s", result.Content)
	}
}

func TestFindHandler_TypeDir(t *testing.T) {
	root := createTestTree(t, map[string]string{
		"src/a.go":     "code",
		"src/sub/b.go": "code",
		"docs/readme":  "doc",
	})

	result, err := FindHandler(context.Background(), models.ToolCall{
		ID:     "find-3",
		Name:   "find",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"path": root,
			"name": "src",
			"type": "dir",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("result error: %s", result.Error)
	}
	if !contains(result.Content, "src") {
		t.Errorf("expected src dir, got: %s", result.Content)
	}
}

func TestFindHandler_TypeFile(t *testing.T) {
	root := createTestTree(t, map[string]string{
		"src/main.go": "code",
	})

	result, err := FindHandler(context.Background(), models.ToolCall{
		ID:     "find-4",
		Name:   "find",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"path": root,
			"name": "src",
			"type": "file",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Content != "No files found." {
		t.Errorf("expected no files (src is a dir), got: %s", result.Content)
	}
}

func TestFindHandler_SkipsHidden(t *testing.T) {
	root := createTestTree(t, map[string]string{
		".hidden/a.txt":     "secret",
		"normal/a.txt":      "public",
		"node_modules/b.js": "dep",
		"vendor/c.go":       "dep",
	})

	result, err := FindHandler(context.Background(), models.ToolCall{
		ID:     "find-5",
		Name:   "find",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"path": root,
			"name": "*.txt",
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
	if contains(result.Content, "vendor") {
		t.Errorf("should skip vendor, got: %s", result.Content)
	}
	if !contains(result.Content, "normal") {
		t.Errorf("expected normal/a.txt, got: %s", result.Content)
	}
}

func TestFindHandler_NoMatch(t *testing.T) {
	root := createTestTree(t, map[string]string{
		"a.txt": "hello",
	})

	result, err := FindHandler(context.Background(), models.ToolCall{
		ID:     "find-6",
		Name:   "find",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"path": root,
			"name": "*.xyz",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Content != "No files found." {
		t.Errorf("expected no files found, got: %s", result.Content)
	}
}

func TestFindHandler_MaxResults(t *testing.T) {
	root := createTestTree(t, map[string]string{
		"a.go": "1", "b.go": "2", "c.go": "3", "d.go": "4", "e.go": "5",
	})

	result, err := FindHandler(context.Background(), models.ToolCall{
		ID:     "find-7",
		Name:   "find",
		Status: models.CallStatusPending,
		Arguments: map[string]any{
			"path":        root,
			"name":        "*.go",
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

func TestFindHandler_ReturnsVirtualPaths(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DEEPAI_DATA_ROOT", root)

	threadID := "thread-find-virtual"
	base := filepath.Join(root, "threads", threadID, "user-data", "uploads")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "one.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	ctx := tools.WithThreadID(context.Background(), threadID)
	result, err := FindHandler(ctx, models.ToolCall{
		ID:   "find-virtual",
		Name: "find",
		Arguments: map[string]any{
			"path": "/mnt/user-data/uploads",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "/mnt/user-data/uploads/one.txt") {
		t.Fatalf("content=%q want virtual path", result.Content)
	}
	if strings.Contains(result.Content, "/threads/"+threadID+"/user-data/") {
		t.Fatalf("content=%q leaked internal path", result.Content)
	}
}
