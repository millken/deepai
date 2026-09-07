package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/millken/deepai/pkg/models"
)

func FindHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	args := call.Arguments

	path, _ := args["path"].(string)
	if strings.TrimSpace(path) == "" {
		path = "."
	}
	path = resolveReadablePath(ctx, path)

	name, _ := args["name"].(string)
	fileType, _ := args["type"].(string)
	includeHidden, _ := args["include_hidden"].(bool)

	maxResults := FindMaxResults
	if v, ok := args["max_results"].(float64); ok && int(v) > 0 {
		maxResults = int(v)
	}

	var results []string
	err := filepath.WalkDir(path, func(fp string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if len(results) >= maxResults {
			return filepath.SkipDir
		}

		// Skip hidden / vendor dirs unless explicitly requested. Always allow
		// the root itself.
		if d.IsDir() && fp != path {
			dirName := d.Name()
			if !includeHidden && (dirName == ".git" || dirName == "node_modules" || dirName == "vendor" || dirName == "__pycache__" || strings.HasPrefix(dirName, ".")) {
				return filepath.SkipDir
			}
		}

		// Apply name filter (optional; empty means match everything).
		if name != "" {
			matched, _ := filepath.Match(name, d.Name())
			if !matched {
				return nil
			}
		}

		// Apply type filter
		switch fileType {
		case "file":
			if d.IsDir() {
				return nil
			}
		case "dir":
			if !d.IsDir() {
				return nil
			}
		}

		results = append(results, displayVirtualPath(ctx, fp))
		return nil
	})
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("find failed: %w", err)
	}

	if len(results) == 0 {
		return models.ToolResult{
			CallID:   call.ID,
			ToolName: call.Name,
			Content:  "No files found.",
		}, nil
	}

	truncated := ""
	if len(results) == maxResults {
		truncated = fmt.Sprintf("\n(results capped at %d; narrow the name/path or raise max_results for more)", maxResults)
	}

	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Content:  strings.Join(results, "\n") + truncated,
	}, nil
}

func FindTool() models.Tool {
	return models.Tool{
		Name:         "find",
		Description:  "Recursively find files and directories. name is an optional glob (e.g. *_test.go); when omitted, every entry is listed.",
		Groups:       []string{"builtin", "file_ops"},
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":           map[string]any{"type": "string", "description": "Root directory to search in (default: current directory)"},
				"name":           map[string]any{"type": "string", "description": "Optional file/dir name glob (e.g. *_test.go, *.yaml, main.*)"},
				"type":           map[string]any{"type": "string", "description": "Filter by type: 'file' or 'dir'"},
				"include_hidden": map[string]any{"type": "boolean", "description": "Descend into .git/.github/vendor/node_modules/__pycache__ and other dotted dirs"},
				"max_results":    map[string]any{"type": "number", "description": "Maximum number of results (default: 50)"},
			},
			"required": []any{},
		},
		Handler: FindHandler,
	}
}
