package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func TestInvokeACPAgentToolUsesPerThreadWorkspace(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DEEPAI_DATA_ROOT", root)

	tool := InvokeACPAgentTool(map[string]ACPAgentConfig{
		"demo": {
			Description: "Demo ACP agent",
			Command:     "sh",
			Args: []string{
				"-c",
				"printf '%s' \"$DEEPAI_ACP_PROMPT\" > result.txt; printf 'done from %s' \"$PWD\"",
			},
		},
	})

	ctx := WithThreadID(context.Background(), "thread-acp-1")
	result, err := tool.Handler(ctx, models.ToolCall{
		ID:   "call-acp-1",
		Name: tool.Name,
		Arguments: map[string]any{
			"agent":  "demo",
			"prompt": "build a demo",
		},
	})
	if err != nil {
		t.Fatalf("tool handler error: %v", err)
	}
	if result.Status != models.CallStatusCompleted {
		t.Fatalf("status=%q want %q", result.Status, models.CallStatusCompleted)
	}

	expectedDir := filepath.Join(root, "threads", "thread-acp-1", "acp-workspace")
	if !strings.Contains(result.Content, expectedDir) {
		t.Fatalf("content=%q want workspace %q", result.Content, expectedDir)
	}

	data, err := os.ReadFile(filepath.Join(expectedDir, "result.txt"))
	if err != nil {
		t.Fatalf("read ACP output: %v", err)
	}
	if got := string(data); got != "build a demo" {
		t.Fatalf("prompt file=%q want %q", got, "build a demo")
	}
}

func TestResolveVirtualPathMapsACPWorkspace(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DEEPAI_DATA_ROOT", root)

	ctx := WithThreadID(context.Background(), "thread-acp-2")
	got := ResolveVirtualPath(ctx, "/mnt/acp-workspace/out/report.txt")
	want := filepath.Join(root, "threads", "thread-acp-2", "acp-workspace", "out", "report.txt")
	if got != want {
		t.Fatalf("path=%q want %q", got, want)
	}
}

func TestInvokeACPAgentToolRequiresThreadID(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DEEPAI_DATA_ROOT", root)

	tool := InvokeACPAgentTool(map[string]ACPAgentConfig{
		"demo": {Command: "sh", Args: []string{"-c", "printf 'ok'"}},
	})

	result, err := tool.Handler(context.Background(), models.ToolCall{
		ID:   "call-acp-missing-thread",
		Name: tool.Name,
		Arguments: map[string]any{
			"agent":  "demo",
			"prompt": "build a demo",
		},
	})
	if err == nil {
		t.Fatal("expected error when thread id is missing")
	}
	if !strings.Contains(err.Error(), "thread id is required") {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != models.CallStatusFailed {
		t.Fatalf("status=%q want %q", result.Status, models.CallStatusFailed)
	}
}

func TestResolveVirtualPath_ConfinesTraversal(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DEEPAI_DATA_ROOT", root)
	ctx := WithThreadID(context.Background(), "thread-1")
	base := filepath.Join(root, "threads", "thread-1", "user-data")

	withinBase := func(p string) bool {
		rel, err := filepath.Rel(base, p)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
	}

	// Normal path resolves under the thread's user-data root.
	if got := ResolveVirtualPath(ctx, "/mnt/user-data/notes/a.txt"); got != filepath.Join(base, "notes", "a.txt") {
		t.Fatalf("normal resolve = %q, want %q", got, filepath.Join(base, "notes", "a.txt"))
	}

	// Parent-traversal escape must be confined (clamped within base).
	if got := ResolveVirtualPath(ctx, "/mnt/user-data/../../../../etc/passwd"); !withinBase(got) {
		t.Fatalf("traversal escaped confinement: %q", got)
	}
	// Cross-thread escape must be confined.
	if got := ResolveVirtualPath(ctx, "/mnt/user-data/../../thread-2/user-data/secret"); !withinBase(got) {
		t.Fatalf("cross-thread escape: %q", got)
	}
	// acp-workspace traversal must be confined too.
	acpBase := filepath.Join(root, "threads", "thread-1", "acp-workspace")
	got := ResolveVirtualPath(ctx, "/mnt/acp-workspace/../../../etc/passwd")
	if rel, err := filepath.Rel(acpBase, got); err == nil && strings.HasPrefix(rel, "..") {
		t.Fatalf("acp-workspace traversal escaped: %q", got)
	}
}
