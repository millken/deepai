package builtin

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/millken/deepai/pkg/models"
)

// EditFileHandler replaces an exact substring in a file. To make it tolerant
// of common AI failure modes (tab vs space, CRLF vs LF, collapsed whitespace
// runs), the handler retries with whitespace-normalized matching when the
// literal match fails.
func EditFileHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	args := call.Arguments
	path, _ := args["path"].(string)
	oldStr, _ := args["old_string"].(string)
	newStr, _ := args["new_string"].(string)

	if strings.TrimSpace(path) == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("path is required")
	}
	if oldStr == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("old_string is required")
	}
	if newStr == oldStr {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("old_string and new_string are identical")
	}

	displayPath := strings.TrimSpace(path)
	path = resolveWritablePath(ctx, path)
	replaceAll, _ := args["replace_all"].(bool)

	data, err := os.ReadFile(path)
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("read failed: %w", err)
	}
	content := string(data)

	type candidate struct {
		oldS, newS string
		note       string
	}
	candidates := []candidate{{oldStr, newStr, ""}}
	if uOld, uNew := unescapeLiteral(oldStr), unescapeLiteral(newStr); uOld != oldStr && uOld != uNew {
		candidates = append(candidates, candidate{uOld, uNew, "escape-normalized"})
	}

	for _, c := range candidates {
		updated, n, offset, kind, err := applyEdit(content, c.oldS, c.newS, replaceAll, displayPath)
		if err != nil {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name}, err
		}
		if n == 0 {
			continue
		}
		if writeErr := os.WriteFile(path, []byte(updated), filePerm(path, 0644)); writeErr != nil {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("write failed: %w", writeErr)
		}
		notes := []string{}
		if kind != "" {
			notes = append(notes, kind)
		}
		if c.note != "" {
			notes = append(notes, c.note)
		}
		msg := fmt.Sprintf("Replaced %d occurrence(s) in %s", n, displayPath)
		if len(notes) > 0 {
			msg += " (" + strings.Join(notes, ", ") + ")"
		}
		// start_line lets the TUI render the diff with real file line numbers
		// (1-based, in original-file coordinates at the first replacement).
		return models.ToolResult{
			CallID:   call.ID,
			ToolName: call.Name,
			Content:  msg,
			Data:     map[string]any{"start_line": 1 + strings.Count(content[:offset], "\n")},
		}, nil
	}

	return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf(
		"old_string not found in %s; copy old_string verbatim from read_file output (real newlines and exact whitespace, not escaped \\n/\\t), then retry edit_file",
		displayPath,
	)
}

// applyEdit returns the rewritten content, the number of replacements, the
// byte offset of the first replacement (for line-number reporting), an optional
// match-kind note, and any error.
func applyEdit(content, oldS, newS string, replaceAll bool, displayPath string) (updated string, n int, offset int, kind string, err error) {
	if count := strings.Count(content, oldS); count > 0 {
		if !replaceAll && count > 1 {
			return "", 0, 0, "", fmt.Errorf(
				"old_string matches %d times in %s; provide more context to make it unique, or set replace_all=true",
				count, displayPath,
			)
		}
		if replaceAll {
			updated = strings.ReplaceAll(content, oldS, newS)
		} else {
			updated = strings.Replace(content, oldS, newS, 1)
		}
		return updated, count, strings.Index(content, oldS), "", nil
	}

	normOld := normalizeWhitespace(oldS)
	if len(strings.TrimSpace(normOld)) >= 8 {
		spans := findWhitespaceTolerantSpans(content, oldS)
		if len(spans) > 0 {
			if !replaceAll && len(spans) > 1 {
				return "", 0, 0, "", fmt.Errorf(
					"old_string matches %d locations in %s after whitespace normalization; provide more context or set replace_all=true",
					len(spans), displayPath,
				)
			}
			conformed := conformLineEndings(newS, content)
			updated = replaceSpans(content, spans, conformed, replaceAll)
			count := 1
			if replaceAll {
				count = len(spans)
			}
			return updated, count, spans[0][0], "whitespace-tolerant match", nil
		}
	}

	return "", 0, 0, "", nil
}

func unescapeLiteral(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			case 'r':
				b.WriteByte('\r')
				i++
				continue
			case 't':
				b.WriteByte('\t')
				i++
				continue
			case '\\':
				b.WriteByte('\\')
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func conformLineEndings(s, content string) string {
	if !strings.Contains(content, "\r\n") {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

// normalizeWhitespace collapses CRLF/CR to LF and runs of horizontal
// whitespace (space, tab) to a single space. Used only for matching.
func normalizeWhitespace(s string) string {
	out, _ := normalizeWithIndex(s)
	return out
}

// normalizeWithIndex returns the normalized byte string and a slice mapping
// each normalized byte position back to its source byte offset in s.
func normalizeWithIndex(s string) (string, []int) {
	nb := make([]byte, 0, len(s))
	origIdx := make([]int, 0, len(s))
	prevWS := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\r' {
			if i+1 < len(s) && s[i+1] == '\n' {
				continue // skip the CR; the LF will be appended next loop
			}
			nb = append(nb, '\n')
			origIdx = append(origIdx, i)
			prevWS = false
			continue
		}
		if c == ' ' || c == '\t' {
			if !prevWS {
				nb = append(nb, ' ')
				origIdx = append(origIdx, i)
				prevWS = true
			}
			continue
		}
		prevWS = false
		nb = append(nb, c)
		origIdx = append(origIdx, i)
	}
	return string(nb), origIdx
}

// findWhitespaceTolerantSpans returns non-overlapping byte spans in content
// whose whitespace-normalized form equals normalizeWhitespace(needle).
func findWhitespaceTolerantSpans(content, needle string) [][2]int {
	target := normalizeWhitespace(needle)
	if target == "" {
		return nil
	}
	norm, origIdx := normalizeWithIndex(content)
	var spans [][2]int
	for i := 0; i+len(target) <= len(norm); {
		if norm[i:i+len(target)] == target {
			start := origIdx[i]
			end := origIdx[i+len(target)-1] + 1
			// If the matched normalized span ends with our collapsed-space
			// marker, extend `end` over any trailing original whitespace bytes
			// so the replacement consumes them too.
			if target[len(target)-1] == ' ' {
				for end < len(content) && (content[end] == ' ' || content[end] == '\t') {
					end++
				}
			}
			spans = append(spans, [2]int{start, end})
			i += len(target)
			continue
		}
		i++
	}
	return spans
}

// replaceSpans applies newStr to each given byte span in content. When
// replaceAll is false, only the first span is replaced.
func replaceSpans(content string, spans [][2]int, newStr string, replaceAll bool) string {
	if len(spans) == 0 {
		return content
	}
	if !replaceAll {
		spans = spans[:1]
	}
	var b strings.Builder
	b.Grow(len(content) + len(spans)*len(newStr))
	cursor := 0
	for _, sp := range spans {
		if sp[0] < cursor {
			continue
		}
		b.WriteString(content[cursor:sp[0]])
		b.WriteString(newStr)
		cursor = sp[1]
	}
	b.WriteString(content[cursor:])
	return b.String()
}

// filePerm returns the existing file's mode bits, falling back to def when
// the file doesn't exist or cannot be stat-ed. Preserves executable bits and
// other permission information on rewrite.
func filePerm(path string, def os.FileMode) os.FileMode {
	if info, err := os.Stat(path); err == nil {
		return info.Mode().Perm()
	}
	return def
}

func EditFileTool() models.Tool {
	return models.Tool{
		Name: "edit_file",
		Description: "Replace exact text in a file for in-place edits. old_string must uniquely match (use replace_all for multiple matches). " +
			"Falls back to whitespace-tolerant matching (tab vs space, CRLF vs LF, collapsed runs) when literal match fails. " +
			"Fails safely if no match or ambiguous match; on failure, re-read the file with read_file and retry this tool.",
		Groups: []string{"builtin", "file_ops"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":        map[string]any{"type": "string", "description": "File path to edit"},
				"old_string":  map[string]any{"type": "string", "description": "Exact text to find (must be unique unless replace_all is set)"},
				"new_string":  map[string]any{"type": "string", "description": "Replacement text"},
				"replace_all": map[string]any{"type": "boolean", "description": "Replace all occurrences instead of requiring a unique match"},
			},
			"required": []any{"path", "old_string", "new_string"},
		},
		Handler: EditFileHandler,
	}
}
