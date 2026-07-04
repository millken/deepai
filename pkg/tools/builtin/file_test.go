package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

func TestReadFileHandlerResolvesThreadVirtualPath(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DEEPAI_DATA_ROOT", root)

	threadID := "thread-file-tool"
	target := filepath.Join(root, "threads", threadID, "user-data", "uploads", "notes.txt")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(target, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	ctx := tools.WithThreadID(context.Background(), threadID)
	result, err := ReadFileHandler(ctx, models.ToolCall{
		ID:   "call-1",
		Name: "read_file",
		Arguments: map[string]any{
			"path": "/mnt/user-data/uploads/notes.txt",
		},
	})
	if err != nil {
		t.Fatalf("ReadFileHandler() error = %v", err)
	}
	if result.Content != "hello" {
		t.Fatalf("content=%q want hello", result.Content)
	}
}

func TestWriteFileHandlerWritesToResolvedVirtualPath(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DEEPAI_DATA_ROOT", root)

	threadID := "thread-write-tool"
	ctx := tools.WithThreadID(context.Background(), threadID)
	_, err := WriteFileHandler(ctx, models.ToolCall{
		ID:   "call-2",
		Name: "write_file",
		Arguments: map[string]any{
			"path":    "/mnt/user-data/uploads/out.txt",
			"content": "created",
		},
	})
	if err != nil {
		t.Fatalf("WriteFileHandler() error = %v", err)
	}
	if resultContent := strings.TrimSpace((func() string {
		res, _ := WriteFileHandler(ctx, models.ToolCall{
			ID:   "call-2b",
			Name: "write_file",
			Arguments: map[string]any{
				"path":    "/mnt/user-data/uploads/out2.txt",
				"content": "created",
			},
		})
		return res.Content
	})()); !strings.Contains(resultContent, "/mnt/user-data/uploads/out2.txt") {
		t.Fatalf("content=%q want virtual path", resultContent)
	}

	data, err := os.ReadFile(filepath.Join(root, "threads", threadID, "user-data", "uploads", "out.txt"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(data) != "created" {
		t.Fatalf("content=%q want created", string(data))
	}
}

func TestReadFileHandlerResolvesACPWorkspaceVirtualPath(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DEEPAI_DATA_ROOT", root)

	threadID := "thread-read-acp"
	target := filepath.Join(root, "threads", threadID, "acp-workspace", "out", "report.txt")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(target, []byte("from acp"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	ctx := tools.WithThreadID(context.Background(), threadID)
	result, err := ReadFileHandler(ctx, models.ToolCall{
		ID:   "call-acp-read",
		Name: "read_file",
		Arguments: map[string]any{
			"path": "/mnt/acp-workspace/out/report.txt",
		},
	})
	if err != nil {
		t.Fatalf("ReadFileHandler() error = %v", err)
	}
	if result.Content != "from acp" {
		t.Fatalf("content=%q want %q", result.Content, "from acp")
	}
}

func TestReadFileHandler_PathAliases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alias.txt")
	if err := os.WriteFile(path, []byte("alias-content"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	for _, args := range []map[string]any{
		{"file_path": path},
		{"filePath": path},
	} {
		result, err := ReadFileHandler(context.Background(), models.ToolCall{
			ID:        "call-read-alias",
			Name:      "read_file",
			Arguments: args,
		})
		if err != nil {
			t.Fatalf("ReadFileHandler() alias args=%v error = %v", args, err)
		}
		if result.Content != "alias-content" {
			t.Fatalf("content=%q want alias-content", result.Content)
		}
	}
}

func TestGlobHandlerReturnsVirtualPaths(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DEEPAI_DATA_ROOT", root)

	threadID := "thread-glob-virtual"
	dir := filepath.Join(root, "threads", threadID, "user-data", "uploads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	ctx := tools.WithThreadID(context.Background(), threadID)
	result, err := GlobHandler(ctx, models.ToolCall{
		ID:   "call-glob-virtual",
		Name: "glob",
		Arguments: map[string]any{
			"pattern": "/mnt/user-data/uploads/*.txt",
		},
	})
	if err != nil {
		t.Fatalf("GlobHandler() error = %v", err)
	}
	var matches []string
	if err := json.Unmarshal([]byte(result.Content), &matches); err != nil {
		t.Fatalf("unmarshal glob: %v", err)
	}
	if len(matches) != 1 || matches[0] != "/mnt/user-data/uploads/a.txt" {
		t.Fatalf("matches=%v", matches)
	}
}

func TestWriteFileHandlerAppendsContent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DEEPAI_DATA_ROOT", root)

	threadID := "thread-append-tool"
	ctx := tools.WithThreadID(context.Background(), threadID)
	_, err := WriteFileHandler(ctx, models.ToolCall{
		ID:   "call-append",
		Name: "write_file",
		Arguments: map[string]any{
			"path":    "/mnt/user-data/uploads/out.txt",
			"content": "hello",
		},
	})
	if err != nil {
		t.Fatalf("initial WriteFileHandler() error = %v", err)
	}
	_, err = WriteFileHandler(ctx, models.ToolCall{
		ID:   "call-append-2",
		Name: "write_file",
		Arguments: map[string]any{
			"path":    "/mnt/user-data/uploads/out.txt",
			"content": " world",
			"append":  true,
		},
	})
	if err != nil {
		t.Fatalf("append WriteFileHandler() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(root, "threads", threadID, "user-data", "uploads", "out.txt"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(data) != "hello world" {
		t.Fatalf("content=%q want hello world", string(data))
	}
}

func TestWriteFileHandlerAppendStartLine(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DEEPAI_DATA_ROOT", root)
	threadID := "thread-append-line"
	ctx := tools.WithThreadID(context.Background(), threadID)

	// Seed a 3-line file (trailing newline → 3 complete lines).
	if _, err := WriteFileHandler(ctx, models.ToolCall{
		ID:   "call-seed",
		Name: "write_file",
		Arguments: map[string]any{
			"path":    "/mnt/user-data/uploads/lines.txt",
			"content": "l1\nl2\nl3\n",
		},
	}); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	// Overwrite reports start_line 1.
	over, err := WriteFileHandler(ctx, models.ToolCall{
		ID:   "call-over",
		Name: "write_file",
		Arguments: map[string]any{
			"path":    "/mnt/user-data/uploads/over.txt",
			"content": "x\n",
		},
	})
	if err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if got := dataLine(over); got != 1 {
		t.Fatalf("overwrite start_line = %v, want 1", got)
	}

	// Append to the 3-line file → appended content begins at line 4.
	app, err := WriteFileHandler(ctx, models.ToolCall{
		ID:   "call-app",
		Name: "write_file",
		Arguments: map[string]any{
			"path":    "/mnt/user-data/uploads/lines.txt",
			"content": "l4\n",
			"append":  true,
		},
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if got := dataLine(app); got != 4 {
		t.Fatalf("append start_line = %v, want 4", got)
	}
}

// dataLine extracts the "start_line" int from a ToolResult's Data side channel.
func dataLine(r models.ToolResult) int {
	switch v := r.Data["start_line"].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

func TestGlobHandlerResolvesVirtualPattern(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DEEPAI_DATA_ROOT", root)

	threadID := "thread-glob-tool"
	dir := filepath.Join(root, "threads", threadID, "user-data", "uploads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	ctx := tools.WithThreadID(context.Background(), threadID)
	result, err := GlobHandler(ctx, models.ToolCall{
		ID:   "call-3",
		Name: "glob",
		Arguments: map[string]any{
			"pattern": "/mnt/user-data/uploads/*.txt",
		},
	})
	if err != nil {
		t.Fatalf("GlobHandler() error = %v", err)
	}
	if !strings.Contains(result.Content, "a.txt") || !strings.Contains(result.Content, "b.txt") {
		t.Fatalf("glob result=%q", result.Content)
	}
}

func TestGlobHandlerHonorsRoot(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "nested")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	result, err := GlobHandler(context.Background(), models.ToolCall{
		ID:   "call-4",
		Name: "glob",
		Arguments: map[string]any{
			"root":    root,
			"pattern": "nested/*.txt",
		},
	})
	if err != nil {
		t.Fatalf("GlobHandler() error = %v", err)
	}
	if !strings.Contains(result.Content, "note.txt") {
		t.Fatalf("glob result=%q, want note.txt", result.Content)
	}
}

func TestResolveWritablePath_MatchesReadableForVirtualPaths(t *testing.T) {
	t.Setenv("DEEPAI_DATA_ROOT", t.TempDir())
	ctx := tools.WithThreadID(context.Background(), "thread-x")

	// Writes must resolve virtual prefixes the same way reads do; previously
	// resolveWritablePath only handled /mnt/user-data, so /mnt/acp-workspace
	// writes leaked to a literal host path while reads went to the thread dir.
	for _, p := range []string{
		"/mnt/user-data/out.txt",
		"/mnt/acp-workspace/result.txt",
		"/some/real/path.go",
	} {
		if w, r := resolveWritablePath(ctx, p), resolveReadablePath(ctx, p); w != r {
			t.Fatalf("resolve mismatch for %q: write=%q read=%q", p, w, r)
		}
	}
	// And the acp-workspace write must NOT stay the literal virtual path.
	if got := resolveWritablePath(ctx, "/mnt/acp-workspace/result.txt"); strings.HasPrefix(got, "/mnt/") {
		t.Fatalf("acp-workspace write not resolved: %q", got)
	}
}

func TestResolvePaths_ExpandHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ctx := context.Background()

	if got := resolveReadablePath(ctx, "~"); got != home {
		t.Fatalf("resolveReadablePath(~) = %q, want %q", got, home)
	}
	if got := resolveWritablePath(ctx, "~/notes.txt"); got != filepath.Join(home, "notes.txt") {
		t.Fatalf("resolveWritablePath(~/notes.txt) = %q", got)
	}
}

func TestReadFileHandler_ExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, "sample.txt")
	if err := os.WriteFile(target, []byte("from-home"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	result, err := ReadFileHandler(context.Background(), models.ToolCall{
		ID:   "call-home-read",
		Name: "read_file",
		Arguments: map[string]any{
			"path": "~/sample.txt",
		},
	})
	if err != nil {
		t.Fatalf("ReadFileHandler() error = %v", err)
	}
	if result.Content != "from-home" {
		t.Fatalf("content=%q want from-home", result.Content)
	}
}
