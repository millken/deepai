package builtin

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/millken/deepai/pkg/models"
)

type dirEntry struct {
	Name    string `json:"name"`
	IsDir   bool   `json:"is_dir"`
	Mode    string `json:"mode,omitempty"`
	Size    int64  `json:"size,omitempty"`
	ModTime string `json:"mod_time,omitempty"`
}

func ListDirHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	args := call.Arguments
	path, _ := args["path"].(string)
	if strings.TrimSpace(path) == "" {
		path = "."
	}
	path = resolveReadablePath(ctx, path)

	dirs, err := os.ReadDir(path)
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("list dir failed: %w", err)
	}

	showAll, _ := args["all"].(bool)

	entries := make([]dirEntry, 0, len(dirs))
	for _, d := range dirs {
		name := d.Name()
		if !showAll && strings.HasPrefix(name, ".") {
			continue
		}
		info, err := d.Info()
		if err != nil {
			continue
		}
		entries = append(entries, dirEntry{
			Name:    name,
			IsDir:   d.IsDir(),
			Mode:    info.Mode().String(),
			Size:    info.Size(),
			ModTime: info.ModTime().Format("2006-01-02 15:04"),
		})
	}

	// Sort: directories first, then files, alphabetically within each group
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entries[i].Name < entries[j].Name
	})

	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%s %8d %s  %s\n", e.Mode, e.Size, e.ModTime, e.Name)
	}

	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Content:  b.String(),
		Data: map[string]any{
			"entries": entries,
		},
	}, nil
}

func ListDirTool() models.Tool {
	return models.Tool{
		Name:        "list_dir",
		Description: "List directory contents with file metadata. Use this instead of ls via bash. Shows files and subdirectories sorted (dirs first); use it to understand project structure.",
		Groups:      []string{"builtin", "file_ops"},
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Directory path to list (default: current directory)"},
				"all":  map[string]any{"type": "boolean", "description": "Show hidden entries like .claude, .git (default: false)"},
			},
			"required": []any{},
		},
		Handler: ListDirHandler,
	}
}
