package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
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
	// codeMapFallbackContentBudget is the combined source-byte budget for an
	// include_content result when no context window is known (standalone
	// handler calls, tests). With a window it is ignored: the budget derives
	// from the window instead (see resolveContentBudget). Either way the
	// budget replaces the offload threshold as include_content's size guard,
	// because offloading would write the very content the caller asked to
	// have in context out to a file and leave a stub behind.
	codeMapFallbackContentBudget = 100_000
	// bytesPerToken converts the token budget back to source bytes for the
	// window-derived budget (~4 source bytes per token is the usual
	// code-mix estimate).
	bytesPerToken = 4
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
		// Top-level mutable state, column-0 only (`pub var g: T`, `var x: T`) —
		// anchored WITHOUT the leading \s* the other zig patterns carry so
		// function-local `var`s don't flood the outline. Global state is exactly
		// what a reviewer/Navigator wants to see listed.
		regexp.MustCompile(`^(pub\s+)?var\b`),
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
	lines   int    // total line count (0 when the file could not be read)
	content string // full source, kept for include_content rendering
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
	switch depth {
	case "symbols", "api":
	default:
		depth = "tree"
	}

	maxFiles := codeMapDefaultMaxFiles
	if v := argPositiveInt(args["max_files"]); v > 0 {
		maxFiles = v
	}
	includeHidden, _ := args["include_hidden"].(bool)
	includeContent, _ := args["include_content"].(bool)
	contentBudget := resolveContentBudget(ctx, args)

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
		if includeContent {
			// Full source of one file, still budget-guarded (a single huge
			// generated file should not silently flood the context).
			out := renderWithContent(ctx, []codeMapFile{f}, contentBudget)
			return codeMapResult(call, out, 1, 1, false, "content"), nil
		}
		if depth == "api" {
			f.symbols = exportedSymbols(filepath.Ext(path), f.symbols)
		}
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
	switch {
	case includeContent:
		content = renderWithContent(ctx, files, contentBudget)
	case depth == "symbols":
		content = renderSymbols(ctx, files, true)
	case depth == "api":
		apiFiles := make([]codeMapFile, len(files))
		for i, f := range files {
			f.symbols = exportedSymbols(filepath.Ext(f.abs), f.symbols)
			apiFiles[i] = f
		}
		content = renderSymbols(ctx, apiFiles, true)
	default:
		content = renderTree(files)
	}
	if truncated {
		content += fmt.Sprintf("\n[Showing %d of %d files. Narrow with path= or raise max_files.]", len(files), total)
	}
	if includeContent {
		content += "\n[Use depth=api for exported-API outlines only, or drop include_content for structure.]"
	}

	data := map[string]any{
		"file_count": len(files),
		"total":      total,
		"truncated":  truncated,
		"depth":      depth,
	}
	if includeContent {
		data["content_bytes"] = len(content)
		// A full-source result is exactly what the caller asked to have IN
		// context — flag it so the agent's offload path (which would write
		// it to disk and leave a stub) leaves it alone. The content budget
		// above is the size guard instead.
		data[models.ToolDataNoOffload] = true
	}
	return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: content, Data: data}, nil
}

// codeMapResult is the single-file include_content return shape.
func codeMapResult(call models.ToolCall, content string, files, total int, truncated bool, depth string) models.ToolResult {
	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Content:  content,
		Data: map[string]any{
			"file_count": files,
			"total":      total,
			"truncated":  truncated,
			"depth":      depth,
			// Same no-offload contract as the directory path above.
			models.ToolDataNoOffload: true,
		},
	}
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
	f.content = string(data)
	f.lines = strings.Count(f.content, "\n") + 1
	f.symbols = extractSymbols(f.content, filepath.Ext(abs))
	return f
}

// resolveContentBudget is include_content's size limit, in source bytes.
// Resolution order: an explicit max_total_bytes argument (the model or the
// caller narrows or widens deliberately) > a window-derived budget (10% of
// the model's context window, converted at ~4 bytes/token — one tool result
// should never crowd out the conversation around it) > the static fallback
// when no window is known.
func resolveContentBudget(ctx context.Context, args map[string]any) int {
	if v := argPositiveInt(args["max_total_bytes"]); v > 0 {
		return v
	}
	if window, ok := tools.ContextWindowFromContext(ctx); ok {
		return window / 10 * bytesPerToken
	}
	return codeMapFallbackContentBudget
}

// argPositiveInt coerces a decoded-JSON numeric argument to a positive int.
// Model-issued calls arrive as float64 via JSON, but in-process callers
// (tests, programmatic use) hand over int — accept both so the parameter
// cannot silently fall back to its default depending on the caller.
func argPositiveInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

// renderWithContent emits each file's full source under a header line, in
// walk order, until the byte budget is exhausted — files past the budget (or
// individually oversized ones) degrade to the one-line tree entry so the
// model sees what exists and can re-scope with a narrower path= instead of
// silently missing files.
func renderWithContent(ctx context.Context, files []codeMapFile, budget int) string {
	var b strings.Builder
	used := 0
	included, listedOnly := 0, 0
	for _, f := range files {
		display := displayVirtualPath(ctx, f.abs)
		if len(f.content) == 0 {
			fmt.Fprintf(&b, "%s  (unreadable)\n", display)
			continue
		}
		if used+len(f.content) > budget {
			fmt.Fprintf(&b, "%s  (%d lines) [content omitted: budget exhausted — narrow path= or raise max_total_bytes]\n", display, f.lines)
			listedOnly++
			continue
		}
		used += len(f.content)
		included++
		fmt.Fprintf(&b, "\n===== %s (%d lines) =====\n%s\n", display, f.lines, f.content)
	}
	fmt.Fprintf(&b, "\n[%d files with content (%d bytes), %d listed only; budget %d]\n", included, used, listedOnly, budget)
	return b.String()
}

// exportedSymbols filters a file's declarations down to its public API for
// the depth=api outline. Languages without a usable exported-marker rule (C,
// Ruby) keep everything — a superset outline degrades gracefully, a missing
// one does not.
func exportedSymbols(ext string, syms []symbol) []symbol {
	lang := extToLang(ext)
	out := syms[:0:0]
	for _, s := range syms {
		if isExportedDecl(lang, s.Text) {
			out = append(out, s)
		}
	}
	return out
}

// isExportedDecl reports whether a cleaned declaration line is part of the
// module's public surface, by language convention.
func isExportedDecl(lang, sig string) bool {
	t := strings.TrimSpace(sig)
	f := strings.Fields(t)
	switch lang {
	case "zig", "rust":
		return strings.HasPrefix(t, "pub ")
	case "go":
		// func Foo / type Foo — the identifier after the keyword starts
		// with an uppercase rune.
		if len(f) >= 2 {
			// skip receiver forms: func (b B) M
			name := f[1]
			if strings.HasPrefix(name, "(") && len(f) >= 4 {
				name = f[3]
			}
			r, _ := utf8.DecodeRuneInString(name)
			return unicode.IsUpper(r)
		}
		return false
	case "python":
		// def name / class Name — exported unless underscore-prefixed.
		if len(f) >= 2 {
			return !strings.HasPrefix(f[1], "_")
		}
		return false
	case "js", "ts":
		return strings.HasPrefix(t, "export ")
	case "java", "php":
		return strings.HasPrefix(t, "public ")
	default:
		return true
	}
}

// renderTree lists each file with its symbol count. Cheap orientation without
// dumping signatures.
func renderTree(files []codeMapFile) string {
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "%s  (%d lines, %d symbols)\n", f.rel, f.lines, len(f.symbols))
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
		fmt.Fprintf(&b, "%s  (%d lines)\n", displayVirtualPath(ctx, f.abs), f.lines)
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
		Description:  "Map a codebase's structure without reading full files: lists source files with line counts and their function/type/class/global-var signatures with line numbers. Use this FIRST when exploring an unfamiliar repo or sizing up files (instead of wc -l / cat / grep for symbols). depth=tree (default) shows files with line and symbol counts; depth=symbols shows signatures; depth=api shows EXPORTED declarations only (architecture at a glance, fewest tokens). include_content=true inlines each file's FULL source — one call brings a whole subtree's code into context instead of many read_file round-trips, bounded by max_total_bytes. Then read_file with start_line/end_line to see an implementation.",
		Groups:       []string{"builtin", "file_ops"},
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":            map[string]any{"type": "string", "description": "Directory or file to map (default: current directory). Scope to a subtree to reduce output."},
				"depth":           map[string]any{"type": "string", "description": "'tree' (default: files + line and symbol counts), 'symbols' (signatures with line numbers), or 'api' (exported declarations only — architecture at a glance, fewest tokens)"},
				"include_content": map[string]any{"type": "boolean", "description": "Inline each file's full source (default: false). One call brings a whole subtree's code into context instead of many read_file round-trips; bounded by max_total_bytes."},
				"max_total_bytes": map[string]any{"type": "number", "description": "Total content budget in bytes when include_content=true (default: 100000); files past the budget are listed without content"},
				"max_files":       map[string]any{"type": "number", "description": "Maximum files to include (default: 100); excess is paginated with a notice"},
				"include_hidden":  map[string]any{"type": "boolean", "description": "Descend into dotted/build/vendor dirs (default: false)"},
			},
			"required": []any{},
		},
		Handler: CodeMapHandler,
	}
}
