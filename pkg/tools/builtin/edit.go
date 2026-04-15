package builtin

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/millken/deepai/pkg/models"
)

func EditFileHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	args := call.Arguments
	path, _ := args["path"].(string)
	oldStr, _ := args["old_string"].(string)
	newStr, _ := args["new_string"].(string)

	if strings.TrimSpace(path) == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("path is required")
	}
	if oldStr == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("old_string is required")
	}
	if newStr == oldStr {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("old_string and new_string are identical")
	}

	path = resolveVirtualPath(ctx, path)
	replaceAll, _ := args["replace_all"].(bool)

	data, err := os.ReadFile(path)
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("read failed: %w", err)
	}

	content := string(data)
	count := strings.Count(content, oldStr)
	if count == 0 {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("old_string not found in %s", path)
	}

	if !replaceAll && count > 1 {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf(
			"old_string matches %d times in %s; provide more context to make it unique, or set replace_all=true",
			count, path,
		)
	}

	var updated string
	if replaceAll {
		updated = strings.ReplaceAll(content, oldStr, newStr)
	} else {
		updated = strings.Replace(content, oldStr, newStr, 1)
	}

	if err := os.WriteFile(path, []byte(updated), 0644); err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("write failed: %w", err)
	}

	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Content:  fmt.Sprintf("Replaced %d occurrence(s) in %s", count, path),
	}, nil
}

func EditFileTool() models.Tool {
	return models.Tool{
		Name:        "edit_file",
		Description: "Replace exact text in a file. old_string must uniquely match (use replace_all for multiple matches). Fails safely if no match or ambiguous match.",
		Groups:      []string{"builtin", "file_ops"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":        map[string]any{"type": "string", "description": "File path to edit"},
				"old_string":  map[string]any{"type": "string", "description": "Exact text to find (must be unique unless replace_all is set)"},
				"new_string":  map[string]any{"type": "string", "description": "Replacement text"},
				"replace_all": map[string]any{"type": "boolean", "description": "Replace all occurrences instead of requiring a unique match"},
			},
			"required": []any{"path", "old_string", "new_string"},
		},
		Handler: EditFileHandler,
	}
}
