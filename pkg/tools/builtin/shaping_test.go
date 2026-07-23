package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func readFileCall(args map[string]any) models.ToolCall {
	return models.ToolCall{ID: "rf", Name: "read_file", Status: models.CallStatusPending, Arguments: args}
}

// bigGoFile builds a Go source file with > ReadFileOutlineThreshold lines and a
// couple of functions placed in the middle (outside head/tail windows).
func bigGoFile(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("package big\n")
	for i := 0; i < 300; i++ { // padding lines 2..301
		fmt.Fprintf(&b, "// filler line %d\n", i)
	}
	b.WriteString("func MiddleOne() error { return nil }\n") // ~line 302
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, "// more filler %d\n", i)
	}
	b.WriteString("func MiddleTwo(x int) int { return x }\n") // ~line 603
	root := t.TempDir()
	path := filepath.Join(root, "big.go")
	if err := os.WriteFile(path, []byte(b.String()), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadFile_OutlineMode(t *testing.T) {
	path := bigGoFile(t)
	res, err := ReadFileHandler(context.Background(), readFileCall(map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Head is present.
	if !strings.Contains(res.Content, "package big") {
		t.Errorf("outline should include head, got start: %.60q", res.Content)
	}
	// Middle symbols surface with line numbers even though they're outside head/tail.
	if !strings.Contains(res.Content, "func MiddleOne() error") {
		t.Errorf("outline should list MiddleOne signature, got: %s", res.Content)
	}
	if !strings.Contains(res.Content, "func MiddleTwo(x int) int") {
		t.Errorf("outline should list MiddleTwo signature, got: %s", res.Content)
	}
	// Total-line hint + recovery path.
	if !strings.Contains(res.Content, "lines") || !strings.Contains(res.Content, "full=true") {
		t.Errorf("outline should carry line count + recovery hint, got tail: %s", res.Content[len(res.Content)-120:])
	}
	// A filler line deep in the middle (not head/tail/symbol) must be omitted.
	if strings.Contains(res.Content, "more filler 150") {
		t.Errorf("outline should omit deep middle content, but included 'more filler 150'")
	}
}

func TestReadFile_FullBypassesOutline(t *testing.T) {
	path := bigGoFile(t)
	res, err := ReadFileHandler(context.Background(), readFileCall(map[string]any{"path": path, "full": true}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Content, "more filler 150") {
		t.Errorf("full=true should return complete content including deep middle lines")
	}
	if strings.Contains(res.Content, "symbol outline") {
		t.Errorf("full=true should not produce an outline")
	}
}

func TestReadFile_RangeBypassesOutline(t *testing.T) {
	path := bigGoFile(t)
	res, err := ReadFileHandler(context.Background(), readFileCall(map[string]any{
		"path": path, "start_line": float64(1), "end_line": float64(1),
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(res.Content, "symbol outline") {
		t.Errorf("explicit range should bypass outline mode, got: %s", res.Content)
	}
	if !strings.Contains(res.Content, "package big") {
		t.Errorf("range read should return the requested line, got: %s", res.Content)
	}
}

func TestReadFile_SmallFileUnaffected(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "small.go")
	os.WriteFile(path, []byte("package p\nfunc A() {}\n"), 0644)
	res, err := ReadFileHandler(context.Background(), readFileCall(map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(res.Content, "symbol outline") {
		t.Errorf("small file should return full content, not outline")
	}
	if !strings.Contains(res.Content, "func A()") {
		t.Errorf("small file should return its content, got: %s", res.Content)
	}
}

func TestReadFile_NonCodeFileNeverOutlined(t *testing.T) {
	// A 600-line CSV has no symbol extractor: outlining it is silent data loss
	// (middle rows vanish). Non-code files keep full-content behavior.
	var b strings.Builder
	for i := 0; i < 600; i++ {
		fmt.Fprintf(&b, "row-%d,value-%d\n", i, i)
	}
	root := t.TempDir()
	path := filepath.Join(root, "data.csv")
	os.WriteFile(path, []byte(b.String()), 0644)

	res, err := ReadFileHandler(context.Background(), readFileCall(map[string]any{"path": path}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Content, "row-300,value-300") {
		t.Errorf("non-code file must return full content including middle rows")
	}
	if strings.Contains(res.Content, "symbol outline") {
		t.Errorf("non-code file must not be outlined")
	}
}

func TestReadFile_LimitZeroStillOutlines(t *testing.T) {
	// limit=0 means "no byte limit" (matching the limit branch), so it must not
	// double as an outline bypass on a large code file.
	path := bigGoFile(t)
	res, err := ReadFileHandler(context.Background(), readFileCall(map[string]any{
		"path": path, "limit": float64(0),
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(res.Content, "more filler 150") {
		t.Errorf("limit=0 on a large code file should still produce the outline, got full content")
	}
	if !strings.Contains(res.Content, "--- symbols ---") {
		t.Errorf("expected outline for limit=0, got: %.120s", res.Content)
	}
}

func TestListDir_DoesNotFoldAmbiguousNames(t *testing.T) {
	// build/dist/target often hold hand-written sources; folding them hides real
	// work. Only unambiguous dependency/VCS/cache dirs fold.
	root := t.TempDir()
	for _, d := range []string{"build", "dist", "target"} {
		os.MkdirAll(filepath.Join(root, d), 0755)
	}
	res, err := ListDirHandler(context.Background(), models.ToolCall{
		ID: "ls", Name: "list_dir", Arguments: map[string]any{"path": root},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(res.Content, "omitted") {
		t.Errorf("build/dist/target must not be folded, got: %s", res.Content)
	}
}

func TestGrep_DefaultCapIs50(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 60; i++ {
		files[fmt.Sprintf("f%02d.txt", i)] = "needle here\n"
	}
	root := createTestTree(t, files)
	res, err := GrepHandler(context.Background(), models.ToolCall{
		ID: "g", Name: "grep", Arguments: map[string]any{"pattern": "needle", "path": root},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !contains(res.Content, "results capped at 50") {
		t.Errorf("grep default cap should be 50, got tail: %s", res.Content[len(res.Content)-80:])
	}
}

func TestFind_DefaultCapIs50(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 60; i++ {
		files[fmt.Sprintf("f%02d.go", i)] = "x"
	}
	root := createTestTree(t, files)
	res, err := FindHandler(context.Background(), models.ToolCall{
		ID: "f", Name: "find", Arguments: map[string]any{"path": root, "name": "*.go"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !contains(res.Content, "results capped at 50") {
		t.Errorf("find default cap should be 50, got tail: %s", res.Content[len(res.Content)-80:])
	}
}

func TestListDir_FoldsGeneratedDirs(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "vendor", "dep"), 0755)
	os.WriteFile(filepath.Join(root, "vendor", "a.go"), []byte("x"), 0644)
	os.MkdirAll(filepath.Join(root, "src"), 0755)
	os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0644)

	res, err := ListDirHandler(context.Background(), models.ToolCall{
		ID: "ls", Name: "list_dir", Arguments: map[string]any{"path": root},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// vendor folded to a marker; normal entries untouched.
	if !strings.Contains(res.Content, "[vendor/ (") || !strings.Contains(res.Content, "omitted") {
		t.Errorf("vendor should be folded, got: %s", res.Content)
	}
	if !strings.Contains(res.Content, "expand_dirs=true") {
		t.Errorf("fold marker should carry recovery hint, got: %s", res.Content)
	}
	if !strings.Contains(res.Content, "main.go") || !strings.Contains(res.Content, "src") {
		t.Errorf("non-folded entries should still appear, got: %s", res.Content)
	}
	// Structured data must stay complete (vendor present).
	entries, _ := res.Data["entries"].([]dirEntry)
	foundVendor := false
	for _, e := range entries {
		if e.Name == "vendor" {
			foundVendor = true
		}
	}
	if !foundVendor {
		t.Errorf("Data[entries] must remain complete including vendor, got: %+v", entries)
	}
}

func TestListDir_ExpandDirsShowsThem(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "node_modules"), 0755)

	res, err := ListDirHandler(context.Background(), models.ToolCall{
		ID: "ls", Name: "list_dir", Arguments: map[string]any{"path": root, "expand_dirs": true},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(res.Content, "omitted") {
		t.Errorf("expand_dirs=true should not fold, got: %s", res.Content)
	}
	if !strings.Contains(res.Content, "node_modules") {
		t.Errorf("expand_dirs=true should show node_modules normally, got: %s", res.Content)
	}
}
