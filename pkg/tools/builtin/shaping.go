package builtin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Tool-output pre-shaping defaults (docs/spec/token-efficiency.md §T2). These
// are package vars, not constants, so a future ToolShapingConfig can override
// them; per-call args (start_line/end_line, full, max_results, expand_dirs)
// remain the recovery path in every case.
var (
	// ReadFileOutlineThreshold: files longer than this many lines return a
	// structural outline (head + symbols + tail) instead of full content when
	// no line range, byte limit, or full=true is given. 0 disables outline mode.
	ReadFileOutlineThreshold = 500

	// GrepMaxResults / FindMaxResults: default result caps (were 100 / 200).
	GrepMaxResults = 50
	FindMaxResults = 50
)

const (
	outlineHeadLines = 50
	outlineTailLines = 20
)

// listDirFoldDirs are directories that list_dir collapses to a one-line
// "(N entries, omitted)" marker unless expand_dirs=true. Deliberately limited
// to names that are unambiguously dependency/VCS/cache trees — dist/build/target
// are NOT folded because many projects keep hand-written sources there (CI
// scripts in build/, committed release assets in dist/, etc.), and folding a
// real source directory silently hides work the model was asked to do.
var listDirFoldDirs = map[string]bool{
	"vendor":       true,
	"node_modules": true,
	".git":         true,
	"__pycache__":  true,
}

// buildFileOutline renders a large file as head lines + symbol signatures (with
// line numbers) + tail lines, so the model can navigate to a precise range
// without pulling the whole file. ext drives symbol extraction (T6's extractor).
func buildFileOutline(lines []string, ext string) string {
	total := len(lines)
	width := numWidth(total)
	var b strings.Builder

	head := outlineHeadLines
	if head > total {
		head = total
	}
	for i := 0; i < head; i++ {
		fmt.Fprintf(&b, "%*d\t%s\n", width, i+1, lines[i])
	}

	if syms := extractSymbols(strings.Join(lines, "\n"), ext); len(syms) > 0 {
		b.WriteString("\n--- symbols ---\n")
		for _, s := range syms {
			fmt.Fprintf(&b, "L %*d  %s\n", width, s.Line, s.Text)
		}
	}

	start := total - outlineTailLines
	if start < head {
		start = head // no overlap with the head block
	}
	if start < total {
		b.WriteString("\n--- tail ---\n")
		for i := start; i < total; i++ {
			fmt.Fprintf(&b, "%*d\t%s\n", width, i+1, lines[i])
		}
	}

	fmt.Fprintf(&b, "\n[File has %d lines. Showing head + symbol outline + tail. "+
		"Use read_file with start_line/end_line for a specific range, or full=true for the whole file.]", total)
	return b.String()
}

// countDirEntries returns the number of immediate children of dir, or 0 on error.
func countDirEntries(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	return len(entries)
}

// foldDirLine renders the collapsed marker for a generated directory.
func foldDirLine(dirPath, name string) string {
	return fmt.Sprintf("[%s/ (%d entries, omitted). Use list_dir with expand_dirs=true to expand.]\n",
		name, countDirEntries(filepath.Join(dirPath, name)))
}
