package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/millken/deepai/pkg/models"
)

// symbol is a single extracted declaration: its 1-based line and a cleaned
// signature line. Shared by code_map (T6) and, later, read_file's outline mode
// (T2a) — both need "signature + line" for deterministic range-reads.
type symbol struct {
	Line int
	Text string
}

const (
	codeMapDefaultMaxFiles = 100
	// Directories folded out of the map: build artifacts and dependency trees
	// carry no navigational value and dominate token cost. Mirrors the skip
	// lists in find.go / grep.go, plus a few common generated dirs.
)

var codeMapFoldDirs = map[string]bool{
	".git":          true,
	"node_modules":  true,
	"vendor":        true,
	"__pycache__":   true,
	"dist":          true,
	"build":         true,
	"target":        true,
	".idea":         true,
	".vscode":       true,
	".svn":          true,
	".hg":           true,
	".zig-cache":    true,
	".gradle":       true,
	".zig-out":      true,
	".claude":       true,
	".claude-cache": true,
	".superpowers":  true,
}

// extToLang maps a file extension to a language key used for symbol patterns.
// Returns "" for extensions we don't extract symbols from.
func extToLang(ext string) string {
	switch strings.ToLower(ext) {
	case ".go":
		return "go"
	case ".py", ".pyi", ".pyw":
		return "python"
	case ".js", ".mjs", ".cjs", ".jsx":
		return "js"
	case ".ts", ".tsx", ".mts", ".cts":
		return "ts"
	case ".rs":
		return "rust"
	case ".zig":
		return "zig"
	case ".java":
		return "java"
	case ".c", ".h", ".cpp", ".cc", ".cxx", ".hpp", ".hh", ".hxx":
		return "c"
	case ".rb":
		return "ruby"
	case ".php":
		return "php"
	default:
		return ""
	}
}

// symbolPatterns holds per-language line regexes that mark a top-level (or
// method) declaration. Intentionally conservative — deterministic and cheap,
// not a full parser. Missed symbols degrade to "grep it", never to a crash.
var symbolPatterns = map[string][]*regexp.Regexp{
	"go": {
		regexp.MustCompile(`^func\b`),
		regexp.MustCompile(`^type\b`),
	},
	"python": {
		regexp.MustCompile(`^\s*def\s`),
		regexp.MustCompile(`^\s*class\s`),
	},
	"js": {
		regexp.MustCompile(`^\s*(export\s+)?(default\s+)?(async\s+)?function\b`),
		regexp.MustCompile(`^\s*(export\s+)?(default\s+)?(abstract\s+)?class\b`),
		regexp.MustCompile(`^\s*(export\s+)?(const|let|var)\s+\w+\s*=\s*(async\s+)?(\([^)]*\)|\w+)\s*=>`),
	},
	"ts": {
		regexp.MustCompile(`^\s*(export\s+)?(default\s+)?(async\s+)?function\b`),
		regexp.MustCompile(`^\s*(export\s+)?(default\s+)?(abstract\s+)?class\b`),
		regexp.MustCompile(`^\s*(export\s+)?(interface|type|enum)\s`),
		regexp.MustCompile(`^\s*(export\s+)?(const|let|var)\s+\w+\s*=\s*(async\s+)?(\([^)]*\)|\w+)\s*=>`),
	},
	"rust": {
		regexp.MustCompile(`^\s*(pub\s+)?(async\s+)?(unsafe\s+)?fn\b`),
		regexp.MustCompile(`^\s*(pub\s+)?struct\b`),
		regexp.MustCompile(`^\s*(pub\s+)?enum\b`),
		regexp.MustCompile(`^\s*(pub\s+)?trait\b`),
		regexp.MustCompile(`^\s*impl\b`),
	},
	"zig": {
		// Functions: pub/export/extern/inline fn, and `extern "c" fn`.
		regexp.MustCompile(`^\s*(pub\s+)?(export\s+)?(inline\s+)?fn\b`),
		regexp.MustCompile(`^\s*(pub\s+)?extern\b.*\bfn\b`),
		// Type-bearing consts: const Name = [extern|packed] struct/enum/union/opaque/error.
		regexp.MustCompile(`^\s*(pub\s+)?const\s+\w+\s*=\s*(extern\s+|packed\s+)?(struct|enum|union|opaque|error)\b`),
	},
	"java": {
		regexp.MustCompile(`^\s*(public|private|protected|static|final|abstract|\s)*\s(class|interface|enum)\s`),
		regexp.MustCompile(`^\s*(public|private|protected)\s[\w<>\[\].]+\s+\w+\s*\([^;]*\)\s*\{?\s*$`),
	},
	"c": {
		// Free functions: return-type name(args) { — heuristic, no leading keyword.
		regexp.MustCompile(`^[A-Za-z_][\w\s\*]+\s+[\w\*]+\s*\([^;]*\)\s*\{?\s*$`),
		regexp.MustCompile(`^\s*(typedef\s+)?(struct|enum|union)\b`),
	},
	"ruby": {
		regexp.MustCompile(`^\s*def\s`),
		regexp.MustCompile(`^\s*(class|module)\s`),
	},
	"php": {
		regexp.MustCompile(`^\s*(public|private|protected|static|abstract|final|\s)*function\s`),
		regexp.MustCompile(`^\s*(abstract\s+|final\s+)?(class|interface|trait)\s`),
	},
}

// extractSymbols returns the declarations found in content for the given file
// extension. Deterministic, regex-based, no LLM. Unknown extensions yield nil.
func extractSymbols(content, ext string) []symbol {
	lang := extToLang(ext)
	patterns := symbolPatterns[lang]
	if len(patterns) == 0 {
		return nil
	}
	var out []symbol
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		for _, re := range patterns {
			if re.MatchString(line) {
				out = append(out, symbol{Line: i + 1, Text: cleanSignature(line)})
				break
			}
		}
	}
	return out
}

// cleanSignature trims a declaration line down to its signature: drop the body
// opener and trailing whitespace so "func A() {}" becomes "func A()".
func cleanSignature(line string) string {
	sig := line
	if idx := strings.IndexByte(sig, '{'); idx >= 0 {
		sig = sig[:idx]
	}
	return strings.TrimRight(strings.TrimSpace(sig), " \t")
}

type codeMapFile struct {
	rel     string
	abs     string
	symbols []symbol
}

// CodeMapHandler builds a structural map of a repository (or subtree): source
// files with their symbol signatures. depth=tree (default) lists files with a
// symbol count; depth=symbols drills into signatures with line numbers. A
// single-file target always shows signatures. Deterministic, computed fresh on
// every call (no persisted map, no cache).
func CodeMapHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	args := call.Arguments

	path, _ := args["path"].(string)
	if strings.TrimSpace(path) == "" {
		path = "."
	}
	path = resolveReadablePath(ctx, path)

	depth, _ := args["depth"].(string)
	depth = strings.TrimSpace(strings.ToLower(depth))
	if depth != "symbols" {
		depth = "tree"
	}

	maxFiles := codeMapDefaultMaxFiles
	if v, ok := args["max_files"].(float64); ok && int(v) > 0 {
		maxFiles = int(v)
	}
	includeHidden, _ := args["include_hidden"].(bool)

	info, err := os.Stat(path)
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("code_map failed: %w", err)
	}

	// Single file: always render its signatures directly.
	if !info.IsDir() {
		if extToLang(filepath.Ext(path)) == "" {
			return models.ToolResult{
				CallID:   call.ID,
				ToolName: call.Name,
				Content:  "No source files found (not a recognized code file).",
			}, nil
		}
		f := readCodeMapFile(path, filepath.Base(path))
		content := renderSymbols(ctx, []codeMapFile{f}, false)
		return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: content}, nil
	}

	files, total, err := walkCodeMap(path, includeHidden, maxFiles)
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("code_map failed: %w", err)
	}
	if len(files) == 0 {
		return models.ToolResult{
			CallID:   call.ID,
			ToolName: call.Name,
			Content:  "No source files found.",
		}, nil
	}

	truncated := total > len(files)
	var content string
	if depth == "symbols" {
		content = renderSymbols(ctx, files, true)
	} else {
		content = renderTree(files)
	}
	if truncated {
		content += fmt.Sprintf("\n[Showing %d of %d files. Narrow with path= or raise max_files.]", len(files), total)
	}

	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Content:  content,
		Data: map[string]any{
			"file_count": len(files),
			"total":      total,
			"truncated":  truncated,
			"depth":      depth,
		},
	}, nil
}

// walkCodeMap collects recognized code files under root, folding build/vendor
// dirs. It returns up to maxFiles entries plus the true total count so callers
// can report pagination.
func walkCodeMap(root string, includeHidden bool, maxFiles int) ([]codeMapFile, int, error) {
	var collected []string
	total := 0
	err := filepath.WalkDir(root, func(fp string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if fp == root {
				return nil
			}
			// include_hidden=true descends into everything — fold dirs included —
			// as the tool schema promises; it is the recovery path for repos whose
			// real sources live in a directory named build/target/etc.
			name := d.Name()
			if !includeHidden && (codeMapFoldDirs[name] || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !includeHidden && strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if extToLang(filepath.Ext(d.Name())) == "" {
			return nil
		}
		total++
		if len(collected) < maxFiles {
			collected = append(collected, fp)
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	sort.Strings(collected)
	files := make([]codeMapFile, 0, len(collected))
	for _, fp := range collected {
		rel, rerr := filepath.Rel(root, fp)
		if rerr != nil {
			rel = fp
		}
		files = append(files, readCodeMapFile(fp, filepath.ToSlash(rel)))
	}
	return files, total, nil
}

func readCodeMapFile(abs, rel string) codeMapFile {
	f := codeMapFile{rel: rel, abs: abs}
	data, err := os.ReadFile(abs)
	if err != nil {
		return f
	}
	f.symbols = extractSymbols(string(data), filepath.Ext(abs))
	return f
}

// renderTree lists each file with its symbol count. Cheap orientation without
// dumping signatures.
func renderTree(files []codeMapFile) string {
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "%s  (%d symbols)\n", f.rel, len(f.symbols))
	}
	b.WriteString("\n[Structure only. Use code_map with depth=symbols and path= to see signatures.]")
	return b.String()
}

// renderSymbols prints each file's signatures with line numbers so the model
// can range-read the implementation directly.
func renderSymbols(ctx context.Context, files []codeMapFile, hint bool) string {
	var b strings.Builder
	for _, f := range files {
		if len(f.symbols) == 0 {
			continue
		}
		b.WriteString(displayVirtualPath(ctx, f.abs))
		b.WriteByte('\n')
		width := numWidth(f.symbols[len(f.symbols)-1].Line)
		for _, s := range f.symbols {
			fmt.Fprintf(&b, "  L %*d  %s\n", width, s.Line, s.Text)
		}
	}
	if b.Len() == 0 {
		return "No symbols found."
	}
	if hint {
		b.WriteString("\n[Symbol outline only. Use read_file with start_line/end_line to see implementations.]")
	}
	return b.String()
}

// CodeMapTool exposes the repository structure map as a read-only tool.
func CodeMapTool() models.Tool {
	return models.Tool{
		Name:         "code_map",
		Description:  "Map a codebase's structure without reading full files: lists source files and their function/type/class signatures with line numbers. Use this FIRST when exploring an unfamiliar repo, instead of many read_file/grep calls. depth=tree (default) shows files with symbol counts; depth=symbols shows signatures. Then read_file with start_line/end_line to see an implementation.",
		Groups:       []string{"builtin", "file_ops"},
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":           map[string]any{"type": "string", "description": "Directory or file to map (default: current directory). Scope to a subtree to reduce output."},
				"depth":          map[string]any{"type": "string", "description": "'tree' (default: files + symbol counts) or 'symbols' (signatures with line numbers)"},
				"max_files":      map[string]any{"type": "number", "description": "Maximum files to include (default: 100); excess is paginated with a notice"},
				"include_hidden": map[string]any{"type": "boolean", "description": "Descend into dotted/build/vendor dirs (default: false)"},
			},
			"required": []any{},
		},
		Handler: CodeMapHandler,
	}
}
