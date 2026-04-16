// Package builtin provides the built-in file and shell-oriented tools.
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

func ReadFileHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	args := call.Arguments
	path, ok := args["path"].(string)
	if !ok || strings.TrimSpace(path) == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("path is required")
	}
	path = resolveVirtualPath(ctx, path)

	data, err := os.ReadFile(path)
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("read failed: %w", err)
	}

	if limit, ok := args["limit"].(float64); ok && limit > 0 && int(limit) < len(data) {
		data = data[:int(limit)]
	}

	return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: string(data)}, nil
}

func WriteFileHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	args := call.Arguments
	path, ok := args["path"].(string)
	if !ok || strings.TrimSpace(path) == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("path is required")
	}
	path = resolveVirtualPath(ctx, path)
	content, ok := args["content"].(string)
	if !ok {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("content is required")
	}
	appendMode, _ := args["append"].(bool)

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("mkdir failed: %w", err)
	}
	if appendMode {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("open failed: %w", err)
		}
		defer file.Close()
		if _, err := file.WriteString(content); err != nil {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("append failed: %w", err)
		}
	} else if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("write failed: %w", err)
	}

	return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: fmt.Sprintf("Written %d bytes to %s", len(content), path)}, nil
}

func GlobHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	args := call.Arguments
	pattern, ok := args["pattern"].(string)
	if !ok || strings.TrimSpace(pattern) == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("pattern is required")
	}
	pattern = resolveVirtualPath(ctx, pattern)
	if root, ok := args["root"].(string); ok && strings.TrimSpace(root) != "" {
		root = resolveVirtualPath(ctx, root)
		if !filepath.IsAbs(pattern) {
			pattern = filepath.Join(root, pattern)
		}
	}

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("glob failed: %w", err)
	}

	data, _ := json.Marshal(matches)
	return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: string(data)}, nil
}

func GlobTool() models.Tool {
	return models.Tool{
		Name:        "glob",
		Description: "List files matching a glob pattern.",
		Groups:      []string{"builtin", "file_ops"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "Glob pattern (e.g. *.go)"},
				"root":    map[string]any{"type": "string", "description": "Root directory for relative patterns"},
			},
			"required": []any{"pattern"},
		},
		Handler: GlobHandler,
	}
}

func ReadFileTool() models.Tool {
	return models.Tool{
		Name:        "read_file",
		Description: "Read the contents of a file.",
		Groups:      []string{"builtin", "file_ops"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":  map[string]any{"type": "string", "description": "File path to read"},
				"limit": map[string]any{"type": "number", "description": "Maximum bytes to read"},
			},
			"required": []any{"path"},
		},
		Handler: ReadFileHandler,
	}
}

func WriteFileTool() models.Tool {
	return models.Tool{
		Name:        "write_file",
		Description: "Write content to a file.",
		Groups:      []string{"builtin", "file_ops"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "File path to write"},
				"content": map[string]any{"type": "string", "description": "Content to write"},
				"append":  map[string]any{"type": "boolean", "description": "Append instead of overwrite"},
			},
			"required": []any{"path", "content"},
		},
		Handler: WriteFileHandler,
	}
}

// FileTools returns all file operation tools.
func FileTools() []models.Tool {
	return []models.Tool{
		ReadFileTool(),
		WriteFileTool(),
		EditFileTool(),
		ListDirTool(),
		GlobTool(),
		GrepTool(),
		FindTool(),
	}
}

func resolveVirtualPath(ctx context.Context, path string) string {
	path = strings.TrimSpace(path)
	if !strings.HasPrefix(path, "/mnt/user-data/") {
		return path
	}
	threadID := tools.ThreadIDFromContext(ctx)
	if threadID == "" {
		return path
	}
	root := strings.TrimSpace(os.Getenv("DEERFLOW_DATA_ROOT"))
	if root == "" {
		root = filepath.Join(os.TempDir(), "deerflow-go-data")
	}
	suffix := strings.TrimPrefix(path, "/mnt/user-data/")
	return filepath.Join(root, "threads", threadID, "user-data", filepath.FromSlash(suffix))
}
