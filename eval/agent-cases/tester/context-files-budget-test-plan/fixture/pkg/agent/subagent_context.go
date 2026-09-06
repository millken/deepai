package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/millken/deepai/pkg/subagent"
)

const (
	// contextFilePerFileCap bounds how much of any single context_files
	// entry is inlined into the subagent's seed message: keeps the bundle
	// bounded — the subagent has read_file for more.
	contextFilePerFileCap = 64 * 1024
	// contextFilesTotalCap bounds the sum of all context_files content
	// injected into the seed message. Exceeding it fails the task (naming
	// the offending file) rather than silently dropping files — the parent
	// should trim the list instead.
	contextFilesTotalCap = 256 * 1024
)

// buildContextFilesBlock reads each path in paths and renders a
// <context-files> block to prepend to the subagent's seed message:
//
//	<context-files>
//	## <path>
//	<content>
//	## <path2>
//	...
//	</context-files>
//
// Relative paths resolve against e.workDir, falling back to the process cwd
// when empty — the same convention initPlanFile (plan.go) uses for plan
// files. Absolute paths outside workDir are allowed as-is (logs, /tmp
// artifacts are legitimate context) and read with a plain os.ReadFile, the
// same as other executor-side reads: the task tool is parent-invoked, so the
// parent already has full file access and this is not a privilege
// escalation, just a direct read (no sandbox indirection needed).
//
// An unreadable file fails the whole task with the path and the underlying
// os error (the parent named a wrong path; surfacing beats a subagent
// hallucinating around a missing file). A file over contextFilePerFileCap is
// truncated to the cap with a marker line rather than failing. Exceeding
// contextFilesTotalCap across all files fails the task naming the offending
// file — files are never silently dropped to fit.
func (e *SubagentExecutor) buildContextFilesBlock(paths []string) (string, error) {
	var b strings.Builder
	b.WriteString("<context-files>\n")
	total := 0
	for _, p := range paths {
		resolved := p
		if !filepath.IsAbs(resolved) {
			dir := e.workDir
			if dir == "" {
				dir, _ = os.Getwd()
			}
			resolved = filepath.Join(dir, resolved)
		}

		data, err := os.ReadFile(resolved)
		if err != nil {
			return "", fmt.Errorf("context_files: could not read %s: %w", p, err)
		}

		content := data
		truncated := false
		if len(content) > contextFilePerFileCap {
			// truncateRuneSafe (aging.go) backs up to the nearest rune
			// boundary instead of cutting the raw byte slice — a plain
			// content[:cap] can split a multi-byte UTF-8 rune in half,
			// which would reach the provider as invalid UTF-8 (U+FFFD).
			content = []byte(truncateRuneSafe(string(data), contextFilePerFileCap))
			truncated = true
		}

		total += len(content)
		if total > contextFilesTotalCap {
			return "", fmt.Errorf("context_files: bundle exceeds the %d byte total cap at %s (contributes %d bytes) — trim the context_files list", contextFilesTotalCap, p, len(content))
		}

		b.WriteString("## ")
		b.WriteString(p)
		b.WriteString("\n")
		b.Write(content)
		if len(content) == 0 || content[len(content)-1] != '\n' {
			b.WriteString("\n")
		}
		if truncated {
			fmt.Fprintf(&b, "[truncated: %s is %d bytes, showing first %d]\n", p, len(data), contextFilePerFileCap)
		}
	}
	b.WriteString("</context-files>\n\n")
	return b.String(), nil
}

// convertSubagentUsage converts the subagent's own run Usage (pkg/agent) into
// the subagent package's local TokenUsage (pkg/subagent), avoiding an import
// cycle (pkg/agent already imports pkg/subagent, so the conversion lives on
// this side). nil in, nil out.
func convertSubagentUsage(u *Usage) *subagent.TokenUsage {
	if u == nil {
		return nil
	}
	return &subagent.TokenUsage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.TotalTokens,
	}
}

// sumUsage adds b's token counts into a copy of a and returns it, for
// summing Usage across a subagent's schema-validation retry attempts. Nil
// safe in both directions: nil+nil = nil, nil+b = clone of b, a+nil = a
// unchanged (same pointer, no allocation).
func sumUsage(a, b *Usage) *Usage {
	if b == nil {
		return a
	}
	if a == nil {
		out := *b
		return &out
	}
	return &Usage{
		InputTokens:  a.InputTokens + b.InputTokens,
		OutputTokens: a.OutputTokens + b.OutputTokens,
		TotalTokens:  a.TotalTokens + b.TotalTokens,
	}
}
