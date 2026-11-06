package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/millken/deepai/pkg/models"
)

// RecallOutputTool reads a tool result that was written out of the prompt.
// ref is the offload id (the filename without .txt) or an absolute path
// that stays inside dir.
func RecallOutputTool(dir string) models.Tool {
	return models.Tool{
		Name: "recall_output",
		Description: "Read the full content of a tool result that was stored outside the prompt. " +
			"Pass ref from the [offloaded: ...] stub (the id, not a guessed path). " +
			"Use this when the stub's tail is not enough.",
		Groups:       []string{"builtin"},
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"ref": map[string]any{
					"type":        "string",
					"description": "Offload id from the stub, or the absolute path named there.",
				},
			},
			"required": []any{"ref"},
		},
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			ref, _ := call.Arguments["ref"].(string)
			path, err := recallOutputPath(dir, ref)
			if err != nil {
				return models.ToolResult{CallID: call.ID, ToolName: call.Name}, err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("recall_output: %w", err)
			}
			return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: string(data)}, nil
		},
	}
}

func recallOutputPath(dir, ref string) (string, error) {
	dir = filepath.Clean(strings.TrimSpace(dir))
	if dir == "" || dir == "." {
		return "", fmt.Errorf("recall_output: offload directory is not configured")
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("recall_output: ref is required")
	}
	var path string
	if filepath.IsAbs(ref) || strings.ContainsAny(ref, `/\`) {
		path = filepath.Clean(ref)
	} else {
		id := strings.TrimSuffix(ref, ".txt")
		if id == "" || id == "." || id == ".." {
			return "", fmt.Errorf("recall_output: invalid ref %q", ref)
		}
		path = filepath.Join(dir, id+".txt")
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("recall_output: ref escapes the offload directory")
	}
	return path, nil
}
