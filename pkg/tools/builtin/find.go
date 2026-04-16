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
	path = resolveVirtualPath(ctx, path)

	name, _ := args["name"].(string)
	fileType, _ := args["type"].(string)

	maxResults := 200
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

		// Skip hidden dirs and common non-code dirs (but not the root itself)
		if d.IsDir() && fp != path {
			dirName := d.Name()
			if dirName == ".git" || dirName == "node_modules" || dirName == "vendor" || dirName == "__pycache__" || strings.HasPrefix(dirName, ".") {
				return filepath.SkipDir
			}
		}

		// Apply name filter
		if name != "" {
			matched, _ := filepath.Match(name, d.Name())
			if !matched {
				if d.IsDir() {
					// Don't prune — children might still match
					return nil
				}
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

		results = append(results, fp)
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
		truncated = fmt.Sprintf("\n(results capped at %d)", maxResults)
	}

	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Content:  strings.Join(results, "\n") + truncated,
	}, nil
}

func FindTool() models.Tool {
	return models.Tool{
		Name:        "find",
		Description: "Recursively find files by name pattern. Unlike glob, supports deep directory traversal (e.g. find all *_test.go in the entire project).",
		Groups:      []string{"builtin", "file_ops"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":        map[string]any{"type": "string", "description": "Root directory to search in (default: current directory)"},
				"name":        map[string]any{"type": "string", "description": "File name pattern with wildcards (e.g. *_test.go, *.yaml, main.*)"},
				"type":        map[string]any{"type": "string", "description": "Filter by type: 'file' or 'dir'"},
				"max_results": map[string]any{"type": "number", "description": "Maximum number of results (default: 200)"},
			},
			"required": []any{"name"},
		},
		Handler: FindHandler,
	}
}
