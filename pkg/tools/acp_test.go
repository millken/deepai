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
	t.Setenv("DEERFLOW_DATA_ROOT", root)

	tool := InvokeACPAgentTool(map[string]ACPAgentConfig{
		"demo": {
			Description: "Demo ACP agent",
			Command:     "sh",
			Args: []string{
				"-c",
				"printf '%s' \"$DEERFLOW_ACP_PROMPT\" > result.txt; printf 'done from %s' \"$PWD\"",
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
	t.Setenv("DEERFLOW_DATA_ROOT", root)

	ctx := WithThreadID(context.Background(), "thread-acp-2")
	got := ResolveVirtualPath(ctx, "/mnt/acp-workspace/out/report.txt")
	want := filepath.Join(root, "threads", "thread-acp-2", "acp-workspace", "out", "report.txt")
	if got != want {
		t.Fatalf("path=%q want %q", got, want)
	}
}
