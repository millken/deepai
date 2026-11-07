package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/millken/deepai/pkg/models"
)

const (
	// recallDefaultBytes is the page size when limit is omitted or zero.
	recallDefaultBytes = 32 * 1024
	// recallMinBytes is the smallest page. A smaller limit is raised so a
	// multi-byte character cannot make the next offset stay put.
	recallMinBytes = 256
	// recallMaxBytes is the hard per-call cap. A larger limit is clamped.
	recallMaxBytes = 64 * 1024
)

// RecallOutputTool reads a tool result that was written out of the prompt.
// ref is the offload id (the filename without .txt) or an absolute path
// that stays inside dir.
func RecallOutputTool(dir string) models.Tool {
	return models.Tool{
		Name: "recall_output",
		Description: "Read a tool result that was stored outside the prompt. " +
			"Pass ref from the [offloaded: ...] stub (the id, not a guessed path). " +
			"Each call returns at most 64 KiB (default 32 KiB, at least 256 bytes). " +
			"When the notice says bytes remain, call again with its offset.",
		Groups:       []string{"builtin"},
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"ref": map[string]any{
					"type":        "string",
					"description": "Offload id from the stub, or the absolute path named there.",
				},
				"offset": map[string]any{
					"type":        "integer",
					"description": "Byte offset to start at. Default 0. Use the offset from a previous truncated result.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum bytes to return. Default 32768. Values below 256 are raised to 256. Values above 65536 are clamped.",
				},
			},
			"required": []any{"ref"},
		},
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			ref, _ := call.Arguments["ref"].(string)
			path, err := recallOutputPath(dir, ref)
			if err != nil {
				return models.ToolResult{CallID: call.ID, ToolName: call.Name}, err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("recall_output: %w", err)
			}
			chunk, notice := recallPage(data, intArg(call.Arguments, "offset"), intArg(call.Arguments, "limit"))
			// The page is what this call asked to bring into the prompt.
			// ToolDataNoOffload keeps offloadIfNeeded from replacing the
			// page with another stub. The page size is already capped.
			return models.ToolResult{
				CallID:   call.ID,
				ToolName: call.Name,
				Content:  chunk + notice,
				Data:     map[string]any{models.ToolDataNoOffload: true},
			}, nil
		},
	}
}

func recallOutputPath(dir, ref string) (string, error) {
	dir = filepath.Clean(strings.TrimSpace(dir))
	if dir == "" || dir == "." {
		return "", fmt.Errorf("recall_output: offload directory is not configured")
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("recall_output: ref is required")
	}
	var path string
	if filepath.IsAbs(ref) || strings.ContainsAny(ref, `/\`) {
		path = filepath.Clean(ref)
	} else {
		id := strings.TrimSuffix(ref, ".txt")
		if id == "" || id == "." || id == ".." {
			return "", fmt.Errorf("recall_output: invalid ref %q", ref)
		}
		path = filepath.Join(dir, id+".txt")
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("recall_output: ref escapes the offload directory")
	}
	return path, nil
}

func intArg(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	default:
		return 0
	}
}

// recallPage returns at most recallMaxBytes of data starting at offset, plus
// a notice when the caller needs another call to see the rest. A zero limit
// uses recallDefaultBytes. A positive limit below recallMinBytes is raised
// so the next offset always moves forward. A complete read from the start
// has an empty notice so a small file is returned unchanged.
func recallPage(data []byte, offset, limit int) (chunk, notice string) {
	total := len(data)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	for offset < total && !utf8.RuneStart(data[offset]) {
		offset++
	}
	if limit <= 0 {
		limit = recallDefaultBytes
	}
	if limit < recallMinBytes {
		limit = recallMinBytes
	}
	if limit > recallMaxBytes {
		limit = recallMaxBytes
	}
	end := offset + limit
	if end > total {
		end = total
	} else {
		for end > offset && !utf8.RuneStart(data[end]) {
			end--
		}
	}
	// A limit smaller than the rune at offset used to walk end back onto
	// offset, so the next-chunk notice pointed at the same place.
	if end == offset && offset < total {
		end = offset + 1
		for end < total && !utf8.RuneStart(data[end]) {
			end++
		}
	}
	chunk = string(data[offset:end])
	if end >= total && offset == 0 {
		return chunk, ""
	}
	remain := total - end
	if remain > 0 {
		notice = fmt.Sprintf("\n[recall_output: bytes %d-%d of %d. %d bytes remain. Next chunk starts at offset %d.]\n", offset, end, total, remain, end)
	} else {
		notice = fmt.Sprintf("\n[recall_output: bytes %d-%d of %d. End of output.]\n", offset, end, total)
	}
	return chunk, notice
}
