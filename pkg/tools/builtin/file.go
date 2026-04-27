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

	startLine, _ := args["start_line"].(float64)
	endLine, _ := args["end_line"].(float64)
	withLineNumbers, _ := args["line_numbers"].(bool)

	// Line-range slicing takes precedence over byte limit; line numbers are
	// implicitly enabled when a range is requested so the AI can refer back to
	// specific lines for follow-up edits.
	if startLine > 0 || endLine > 0 {
		lines := strings.Split(string(data), "\n")
		total := len(lines)
		s := int(startLine)
		e := int(endLine)
		if s <= 0 {
			s = 1
		}
		if e <= 0 || e > total {
			e = total
		}
		if s > total {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: ""}, nil
		}
		if s > e {
			s, e = e, s
		}
		selected := lines[s-1 : e]
		var b strings.Builder
		// Range mode always renders line numbers so follow-up edits can
		// reference exact positions.
		_ = withLineNumbers
		width := numWidth(e)
		for i, ln := range selected {
			fmt.Fprintf(&b, "%*d\t%s\n", width, s+i, ln)
		}
		return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: b.String()}, nil
	}

	if limit, ok := args["limit"].(float64); ok && limit > 0 && int(limit) < len(data) {
		data = data[:int(limit)]
	}

	if withLineNumbers {
		lines := strings.Split(string(data), "\n")
		width := numWidth(len(lines))
		var b strings.Builder
		for i, ln := range lines {
			fmt.Fprintf(&b, "%*d\t%s\n", width, i+1, ln)
		}
		return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: b.String()}, nil
	}

	return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: string(data)}, nil
}

// numWidth returns the number of decimal digits required to display n.
func numWidth(n int) int {
	if n <= 0 {
		return 1
	}
	w := 0
	for n > 0 {
		w++
		n /= 10
	}
	return w
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
	perm := filePerm(path, 0644)
	if appendMode {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, perm)
		if err != nil {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("open failed: %w", err)
		}
		defer file.Close()
		if _, err := file.WriteString(content); err != nil {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("append failed: %w", err)
		}
	} else if err := os.WriteFile(path, []byte(content), perm); err != nil {
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
		Name:         "glob",
		Description:  "List files matching a glob pattern.",
		Groups:       []string{"builtin", "file_ops"},
		ParallelSafe: true,
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
		Name:         "read_file",
		Description:  "Read a file. Optional start_line/end_line (1-based, inclusive) restrict to a range; line_numbers prefixes each line with its number.",
		Groups:       []string{"builtin", "file_ops"},
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":         map[string]any{"type": "string", "description": "File path to read"},
				"start_line":   map[string]any{"type": "number", "description": "1-based inclusive start line; enables line-range mode"},
				"end_line":     map[string]any{"type": "number", "description": "1-based inclusive end line; pairs with start_line"},
				"line_numbers": map[string]any{"type": "boolean", "description": "Prefix each line with its 1-based line number (auto when range is set)"},
				"limit":        map[string]any{"type": "number", "description": "Maximum bytes to read (ignored when range is set)"},
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
	root := strings.TrimSpace(os.Getenv("DEEPAI_DATA_ROOT"))
	if root == "" {
		root = filepath.Join(os.TempDir(), "deepai-go-data")
	}
	suffix := strings.TrimPrefix(path, "/mnt/user-data/")
	return filepath.Join(root, "threads", threadID, "user-data", filepath.FromSlash(suffix))
}
