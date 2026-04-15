package builtin

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/millken/deepai/pkg/models"
)

type grepMatch struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Content string `json:"content"`
}

func GrepHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	args := call.Arguments
	pattern, _ := args["pattern"].(string)
	if strings.TrimSpace(pattern) == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("pattern is required")
	}

	path, _ := args["path"].(string)
	if strings.TrimSpace(path) == "" {
		path = "."
	}
	path = resolveVirtualPath(ctx, path)

	caseInsensitive := false
	if v, ok := args["case_insensitive"].(bool); ok {
		caseInsensitive = v
	}

	var filePatterns []string
	if globArg, ok := args["glob"].(string); ok && strings.TrimSpace(globArg) != "" {
		filePatterns = []string{strings.TrimSpace(globArg)}
	}

	maxResults := 100
	if v, ok := args["max_results"].(float64); ok && int(v) > 0 {
		maxResults = int(v)
	}

	re, err := compileGrepPattern(pattern, caseInsensitive)
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("invalid pattern: %w", err)
	}

	matches, err := searchDir(path, re, filePatterns, maxResults)
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, err
	}

	if len(matches) == 0 {
		return models.ToolResult{
			CallID:   call.ID,
			ToolName: call.Name,
			Content:  "No matches found.",
		}, nil
	}

	var b strings.Builder
	for _, m := range matches {
		fmt.Fprintf(&b, "%s:%d: %s\n", m.File, m.Line, m.Content)
	}

	truncated := ""
	if len(matches) == maxResults {
		truncated = fmt.Sprintf("\n(results capped at %d)", maxResults)
	}

	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Content:  b.String() + truncated,
	}, nil
}

func compileGrepPattern(pattern string, caseInsensitive bool) (*regexp.Regexp, error) {
	flags := ""
	if caseInsensitive {
		flags = "(?i)"
	}
	return regexp.Compile(flags + pattern)
}

func searchDir(root string, re *regexp.Regexp, filePatterns []string, maxResults int) ([]grepMatch, error) {
	var matches []grepMatch

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if len(matches) >= maxResults {
			return filepath.SkipDir
		}

		// Skip hidden dirs and common non-code dirs
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" || name == "__pycache__" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip binary-ish files and hidden files
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if isBinaryExt(filepath.Ext(d.Name())) {
			return nil
		}

		// Apply glob filter
		if len(filePatterns) > 0 {
			matched := false
			for _, gp := range filePatterns {
				if gm, _ := filepath.Match(gp, d.Name()); gm {
					matched = true
					break
				}
			}
			if !matched {
				return nil
			}
		}

		fileMatches, err := searchFile(path, re, maxResults-len(matches))
		if err != nil {
			return nil
		}
		matches = append(matches, fileMatches...)
		return nil
	})

	return matches, err
}

func searchFile(path string, re *regexp.Regexp, limit int) ([]grepMatch, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var matches []grepMatch
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if re.MatchString(scanner.Text()) {
			matches = append(matches, grepMatch{
				File:    path,
				Line:    lineNum,
				Content: strings.TrimSpace(scanner.Text()),
			})
			if len(matches) >= limit {
				break
			}
		}
	}
	return matches, scanner.Err()
}

// isBinaryExt returns true for file extensions that should be skipped.
func isBinaryExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".png", ".jpg", ".jpeg", ".gif", ".bmp", ".ico", ".webp",
		".zip", ".tar", ".gz", ".bz2", ".xz", ".zst",
		".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx",
		".exe", ".dll", ".so", ".dylib", ".a", ".o",
		".wasm", ".sqlite", ".db",
		".mp3", ".mp4", ".wav", ".avi", ".mov", ".mkv", ".flv":
		return true
	}
	return false
}

func GrepTool() models.Tool {
	return models.Tool{
		Name:        "grep",
		Description: "Search file contents by regex pattern. Returns matching file:line:content entries. Skips binary files and hidden directories (.git, node_modules, vendor).",
		Groups:      []string{"builtin", "file_ops"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern":         map[string]any{"type": "string", "description": "Regex pattern to search for"},
				"path":            map[string]any{"type": "string", "description": "Directory or file to search in (default: current directory)"},
				"glob":            map[string]any{"type": "string", "description": "Filter files by glob pattern (e.g. *.go, *.ts)"},
				"case_insensitive": map[string]any{"type": "boolean", "description": "Case-insensitive search (default: false)"},
				"max_results":     map[string]any{"type": "number", "description": "Maximum number of results (default: 100)"},
			},
			"required": []any{"pattern"},
		},
		Handler: GrepHandler,
	}
}
