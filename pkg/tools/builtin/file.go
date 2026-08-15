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
	withLineNumbers, lineNumbersSet := args["line_numbers"].(bool)
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
		// Range mode renders line numbers by default so follow-up edits can
		// reference exact positions, but line_numbers=false returns the raw span
		// — the numbers are otherwise pasted straight into edit_file's old_string
		// and can never match the file.
		if lineNumbersSet && !withLineNumbers {
			return models.ToolResult{
				CallID:   call.ID,
				ToolName: call.Name,
				Content:  strings.Join(selected, "\n") + "\n",
			}, nil
		}
		var b strings.Builder
		width := numWidth(e)
		for i, ln := range selected {
			fmt.Fprintf(&b, "%*d\t%s\n", width, s+i, ln)
		}
		return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: b.String()}, nil
	}

	// T2a structural outline: a large CODE file read without a range, byte
	// limit, or full=true returns head + symbol signatures + tail instead of
	// full content. Restricted to extensions with a symbol extractor — for
	// non-code files (CSV, YAML, wordlists) an outline is silent data loss, so
	// they keep the original full-content behavior. limit=0 counts as "no
	// limit" (matching the limit branch below), not as an outline bypass.
	limitArg, hasLimit := args["limit"].(float64)
	hasLimit = hasLimit && limitArg > 0
	full, _ := args["full"].(bool)
	if ReadFileOutlineThreshold > 0 && !full && !hasLimit &&
		len(lines) > ReadFileOutlineThreshold && extToLang(filepath.Ext(path)) != "" {
		return models.ToolResult{
			CallID:   call.ID,
			ToolName: call.Name,
			Content:  buildFileOutline(lines, filepath.Ext(path)),
		}, nil
	}

	if hasLimit && int(limitArg) < len(data) {
		data = data[:int(limitArg)]
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
	// start_line is the 1-based file line where the written content begins.
	// Overwrite starts at line 1; append starts after the file's existing lines.
	startLine := 1
	if appendMode {
		if existing, rerr := os.ReadFile(path); rerr == nil {
			startLine = 1 + strings.Count(string(existing), "\n")
		}
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
		Data:     map[string]any{"start_line": startLine},
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
		Description:  "List files matching a glob pattern (e.g. *.go).",
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
		Description:  "Read a file's contents. Optional start_line/end_line (1-based, inclusive) restrict to a range; line_numbers prefixes each line with its number and a TAB (on by default in range mode — pass line_numbers=false to get the raw span for pasting into edit_file's old_string). Very large files return a structural outline (head + symbol signatures with line numbers + tail); pass full=true or a start_line/end_line range to get exact content.",
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
				"line_numbers": map[string]any{"type": "boolean", "description": "Prefix each line with its 1-based line number and a TAB (defaults on when a range is set; pass false for raw text to reuse in edit_file)"},
				"limit":        map[string]any{"type": "number", "description": "Maximum bytes to read (ignored when range is set)"},
				"full":         map[string]any{"type": "boolean", "description": "Force full content for large files instead of the structural outline"},
			},
		},
		Handler: ReadFileHandler,
	}
}

func WriteFileTool() models.Tool {
	return models.Tool{
		Name:        "write_file",
		Description: "Write content to a file, creating parent directories as needed.",
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
		CodeMapTool(),
	}
}

func resolveReadablePath(ctx context.Context, path string) string {
	return expandHomePath(tools.ResolveVirtualPath(ctx, path))
}

func resolveWritablePath(ctx context.Context, path string) string {
	return expandHomePath(tools.ResolveVirtualPath(ctx, path))
}

func expandHomePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, path[2:])
		}
	}
	return path
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
