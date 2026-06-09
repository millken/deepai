package builtin

import (
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
		// Backward-compatible alias: some model/tooling stacks use query.
		pattern, _ = args["query"].(string)
	}
	if strings.TrimSpace(pattern) == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("pattern is required")
	}

	path, _ := args["path"].(string)
	if strings.TrimSpace(path) == "" {
		path = "."
	}
	path = resolveReadablePath(ctx, path)

	caseInsensitive := false
	if v, ok := args["case_insensitive"].(bool); ok {
		caseInsensitive = v
	}

	var extFilter map[string]bool
	if typeArg, ok := args["type"].(string); ok && strings.TrimSpace(typeArg) != "" {
		extFilter = typeToExts(strings.TrimSpace(typeArg))
	}

	// glob as fallback for custom patterns
	var globPatterns []string
	if globArg, ok := args["glob"].(string); ok && strings.TrimSpace(globArg) != "" {
		globPatterns = []string{strings.TrimSpace(globArg)}
	}

	maxResults := 100
	if v, ok := args["max_results"].(float64); ok && int(v) > 0 {
		maxResults = int(v)
	}

	contextLines := 0
	if v, ok := args["context"].(float64); ok && int(v) > 0 {
		contextLines = int(v)
	}

	includeHidden, _ := args["include_hidden"].(bool)

	re, err := compileGrepPattern(pattern, caseInsensitive)
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("invalid pattern: %w", err)
	}

	// Support searching a single file directly
	info, statErr := os.Stat(path)
	if statErr != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("stat failed: %w", statErr)
	}
	var matches []grepMatch
	if !info.IsDir() {
		fileMatches, ferr := searchFile(path, re, maxResults)
		if ferr != nil {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("search failed: %w", ferr)
		}
		matches = fileMatches
	} else {
		matches, err = searchDir(path, re, extFilter, globPatterns, maxResults, includeHidden)
		if err != nil {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name}, err
		}
	}
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
	displayMatches := displayGrepMatches(ctx, matches)
	if contextLines > 0 {
		// Group matches by file and render with context lines
		renderMatchesWithContext(&b, matches, contextLines, func(path string) string {
			return displayVirtualPath(ctx, path)
		})
	} else {
		for _, m := range displayMatches {
			fmt.Fprintf(&b, "%s:%d: %s\n", m.File, m.Line, m.Content)
		}
	}

	truncated := ""
	if len(matches) == maxResults {
		truncated = fmt.Sprintf("\n(results capped at %d)", maxResults)
	}

	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Content:  b.String() + truncated,
		Data: map[string]any{
			"matches":     displayMatches,
			"truncated":   len(matches) == maxResults,
			"max_results": maxResults,
			"context":     contextLines,
		},
	}, nil
}

func compileGrepPattern(pattern string, caseInsensitive bool) (*regexp.Regexp, error) {
	flags := ""
	if caseInsensitive {
		flags = "(?i)"
	}
	return regexp.Compile(flags + pattern)
}

func searchDir(root string, re *regexp.Regexp, extFilter map[string]bool, globPatterns []string, maxResults int, includeHidden bool) ([]grepMatch, error) {
	var matches []grepMatch

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if len(matches) >= maxResults {
			return filepath.SkipDir
		}

		// Skip hidden / vendor dirs unless explicitly opted in.
		if d.IsDir() && path != root {
			name := d.Name()
			if !includeHidden && (name == ".git" || name == "node_modules" || name == "vendor" || name == "__pycache__" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip binary-ish files; skip dotfiles unless include_hidden is set.
		if !includeHidden && strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if isBinaryExt(filepath.Ext(d.Name())) {
			return nil
		}

		// Apply type-based extension filter
		if len(extFilter) > 0 {
			if !extFilter[filepath.Ext(path)] {
				return nil
			}
		}

		// Apply glob filter
		if len(globPatterns) > 0 {
			matched := false
			for _, gp := range globPatterns {
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
	lines, err := readFileLines(path)
	if err != nil {
		return nil, err
	}

	var matches []grepMatch
	for i, line := range lines {
		if re.MatchString(line) {
			matches = append(matches, grepMatch{
				File:    path,
				Line:    i + 1,
				Content: line,
			})
			if len(matches) >= limit {
				break
			}
		}
	}
	return matches, nil
}

// renderMatchesWithContext groups matches by file and renders surrounding lines.
func renderMatchesWithContext(b *strings.Builder, matches []grepMatch, contextLines int, displayPath func(string) string) {
	type fileRequest struct {
		path    string
		matches []grepMatch
	}

	// Group by file preserving order
	var files []fileRequest
	byFile := map[string]*fileRequest{}
	for _, m := range matches {
		fr, ok := byFile[m.File]
		if !ok {
			files = append(files, fileRequest{path: m.File})
			fr = &files[len(files)-1]
			byFile[m.File] = fr
		}
		fr.matches = append(fr.matches, m)
	}

	for i, fr := range files {
		if i > 0 {
			b.WriteString("--\n")
		}
		lines, err := readFileLines(fr.path)
		if err != nil {
			for _, m := range fr.matches {
				fmt.Fprintf(b, "%s:%d: %s\n", m.File, m.Line, m.Content)
			}
			continue
		}

		// Collect all line numbers to show (match + context), preserving order
		show := map[int]bool{}
		for _, m := range fr.matches {
			for ln := m.Line - contextLines; ln <= m.Line+contextLines; ln++ {
				if ln >= 1 && ln <= len(lines) {
					show[ln] = true
				}
			}
		}

		prev := 0
		for ln := 1; ln <= len(lines); ln++ {
			if !show[ln] {
				continue
			}
			if prev > 0 && ln > prev+1 {
				b.WriteString("...\n")
			}
			shownPath := fr.path
			if displayPath != nil {
				shownPath = displayPath(fr.path)
			}
			fmt.Fprintf(b, "%s:%d: %s\n", shownPath, ln, lines[ln-1])
			prev = ln
		}
	}
}

func displayGrepMatches(ctx context.Context, matches []grepMatch) []grepMatch {
	if len(matches) == 0 {
		return nil
	}
	out := make([]grepMatch, len(matches))
	for i, match := range matches {
		out[i] = grepMatch{
			File:    displayVirtualPath(ctx, match.File),
			Line:    match.Line,
			Content: match.Content,
		}
	}
	return out
}

func readFileLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return strings.Split(string(data), "\n"), nil
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
		Description: "Search file contents by regex pattern. Use this instead of grep/rg/ag via bash. Returns matching file:line:content entries. Skips binary files and hidden directories (.git, node_modules, vendor).",
		Groups:      []string{"builtin", "file_ops"},
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern":          map[string]any{"type": "string", "description": "Regex pattern to search for"},
				"query":            map[string]any{"type": "string", "description": "Alias of pattern (deprecated)"},
				"path":             map[string]any{"type": "string", "description": "Directory or file to search in (default: current directory)"},
				"type":             map[string]any{"type": "string", "description": "Filter by file type: go, py, js, ts, java, rust, c, cpp, rb, php, rs, sql, sh, html, css, json, yaml, xml, md, proto"},
				"glob":             map[string]any{"type": "string", "description": "Filter files by glob pattern (e.g. *.txt). Use 'type' instead for common languages."},
				"case_insensitive": map[string]any{"type": "boolean", "description": "Case-insensitive search (default: false)"},
				"include_hidden":   map[string]any{"type": "boolean", "description": "Search inside .git/.github/vendor/node_modules/__pycache__ and dotfiles"},
				"context":          map[string]any{"type": "number", "description": "Number of context lines before and after each match (default: 0)"},
				"max_results":      map[string]any{"type": "number", "description": "Maximum number of results (default: 100)"},
			},
		},
		Handler: GrepHandler,
	}
}

// typeToExts maps a ripgrep-style type name to a set of file extensions.
func typeToExts(typ string) map[string]bool {
	m := map[string][]string{
		"go":    {".go"},
		"py":    {".py", ".pyi", ".pyw"},
		"js":    {".js", ".mjs", ".cjs"},
		"ts":    {".ts", ".tsx", ".mts", ".cts"},
		"java":  {".java"},
		"rust":  {".rs"},
		"rs":    {".rs"},
		"c":     {".c", ".h"},
		"cpp":   {".cpp", ".cc", ".cxx", ".hpp", ".hh", ".hxx"},
		"rb":    {".rb"},
		"php":   {".php"},
		"sql":   {".sql"},
		"sh":    {".sh", ".bash"},
		"html":  {".html", ".htm"},
		"css":   {".css", ".scss", ".less"},
		"json":  {".json"},
		"yaml":  {".yaml", ".yml"},
		"xml":   {".xml"},
		"md":    {".md", ".mdx"},
		"proto": {".proto"},
	}
	exts, ok := m[strings.ToLower(typ)]
	if !ok {
		return nil
	}
	s := make(map[string]bool, len(exts))
	for _, e := range exts {
		s[e] = true
	}
	return s
}
