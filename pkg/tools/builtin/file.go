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
	path, _ := args["path"].(string)
	if strings.TrimSpace(path) == "" {
		if p, ok := args["file_path"].(string); ok {
			path = p
		}
	}
	if strings.TrimSpace(path) == "" {
		if p, ok := args["filePath"].(string); ok {
			path = p
		}
	}
	if strings.TrimSpace(path) == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("path is required")
	}
	path = resolveReadablePath(ctx, path)

	data, err := os.ReadFile(path)
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("read failed: %w", err)
	}

	startLine, _ := args["start_line"].(float64)
	endLine, _ := args["end_line"].(float64)
	withLineNumbers, _ := args["line_numbers"].(bool)
	text := string(data)
	lines := splitFileLines(text)

	// Line-range slicing takes precedence over byte limit; line numbers are
	// implicitly enabled when a range is requested so the AI can refer back to
	// specific lines for follow-up edits.
	if startLine > 0 || endLine > 0 {
		total := len(lines)
		if total == 0 {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: ""}, nil
		}
		s := int(startLine)
		e := int(endLine)
		if s <= 0 {
			s = 1
		}
		if e <= 0 || e > total {
			e = total
		}
		if startLine > 0 && endLine > 0 && s > e {
			s, e = e, s
		}
		if s < 1 {
			s = 1
		}
		if e > total {
			e = total
		}
		if s > total {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: ""}, nil
		}
		if s > e {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: ""}, nil
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
		text = string(data)
		lines = splitFileLines(text)
	}

	if withLineNumbers {
		if len(lines) == 0 {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: ""}, nil
		}
		width := numWidth(len(lines))
		var b strings.Builder
		for i, ln := range lines {
			fmt.Fprintf(&b, "%*d\t%s\n", width, i+1, ln)
		}
		return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: b.String()}, nil
	}

	return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: string(data)}, nil
}

func splitFileLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if strings.HasSuffix(text, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
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
	displayPath := strings.TrimSpace(path)
	path = resolveWritablePath(ctx, path)
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

	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Content:  fmt.Sprintf("Written %d bytes to %s", len(content), displayPath),
		Data:     map[string]any{"start_line": 1},
	}, nil
}

func GlobHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	args := call.Arguments
	pattern, ok := args["pattern"].(string)
	if !ok || strings.TrimSpace(pattern) == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("pattern is required")
	}
	pattern = resolveReadablePath(ctx, pattern)
	if root, ok := args["root"].(string); ok && strings.TrimSpace(root) != "" {
		root = resolveReadablePath(ctx, root)
		if !filepath.IsAbs(pattern) {
			pattern = filepath.Join(root, pattern)
		}
	}

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("glob failed: %w", err)
	}

	for i, match := range matches {
		matches[i] = displayVirtualPath(ctx, match)
	}
	data, _ := json.Marshal(matches)
	return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: string(data)}, nil
}

func GlobTool() models.Tool {
	return models.Tool{
		Name:         "glob",
		Description:  "List files matching a glob pattern. Use this instead of ls/find globs via bash.",
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
		Description:  "Read a file's contents. Use this instead of cat/head/tail/sed via bash. Optional start_line/end_line (1-based, inclusive) restrict to a range; line_numbers prefixes each line with its number.",
		Groups:       []string{"builtin", "file_ops"},
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":         map[string]any{"type": "string", "description": "File path to read"},
				"file_path":    map[string]any{"type": "string", "description": "Alias of path (deprecated)"},
				"filePath":     map[string]any{"type": "string", "description": "Alias of path (deprecated)"},
				"start_line":   map[string]any{"type": "number", "description": "1-based inclusive start line; enables line-range mode"},
				"end_line":     map[string]any{"type": "number", "description": "1-based inclusive end line; pairs with start_line"},
				"line_numbers": map[string]any{"type": "boolean", "description": "Prefix each line with its 1-based line number (auto when range is set)"},
				"limit":        map[string]any{"type": "number", "description": "Maximum bytes to read (ignored when range is set)"},
			},
		},
		Handler: ReadFileHandler,
	}
}

func WriteFileTool() models.Tool {
	return models.Tool{
		Name:        "write_file",
		Description: "Write content to a file, creating parent directories as needed. Use this instead of echo>/cat>/tee via bash.",
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

func resolveReadablePath(ctx context.Context, path string) string {
	return tools.ResolveVirtualPath(ctx, path)
}

func resolveWritablePath(ctx context.Context, path string) string {
	return tools.ResolveVirtualPath(ctx, path)
}

func displayVirtualPath(ctx context.Context, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return path
	}
	if virtual := displayVirtualUserDataPath(ctx, path); virtual != "" {
		return virtual
	}
	threadID := tools.ThreadIDFromContext(ctx)
	if threadID != "" {
		if root, err := tools.ACPWorkspaceDir(threadID); err == nil {
			if virtual := displayVirtualPathFromRoot(path, root, "/mnt/acp-workspace"); virtual != "" {
				return virtual
			}
		}
	}
	return path
}

func displayVirtualUserDataPath(ctx context.Context, path string) string {
	threadID := tools.ThreadIDFromContext(ctx)
	if threadID == "" {
		return ""
	}
	root := strings.TrimSpace(os.Getenv("DEEPAI_DATA_ROOT"))
	if root == "" {
		root = filepath.Join(os.TempDir(), "deepai-go-data")
	}
	userDataRoot := filepath.Join(root, "threads", threadID, "user-data")
	return displayVirtualPathFromRoot(path, userDataRoot, "/mnt/user-data")
}

func displayVirtualPathFromRoot(path, root, virtualRoot string) string {
	cleanPath := filepath.Clean(path)
	cleanRoot := filepath.Clean(root)
	rel, err := filepath.Rel(cleanRoot, cleanPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return ""
	}
	if rel == "." {
		return virtualRoot
	}
	return virtualRoot + "/" + filepath.ToSlash(rel)
}
