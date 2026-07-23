package builtin

import (
	"context"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func codeMapCall(args map[string]any) models.ToolCall {
	return models.ToolCall{
		ID:        "codemap-1",
		Name:      "code_map",
		Status:    models.CallStatusPending,
		Arguments: args,
	}
}

func TestExtractSymbols_Go(t *testing.T) {
	src := "package main\n\nfunc A() {}\ntype B struct{}\nfunc (b B) M() error { return nil }\n"
	syms := extractSymbols(src, ".go")
	if len(syms) != 3 {
		t.Fatalf("want 3 symbols, got %d: %+v", len(syms), syms)
	}
	if syms[0].Line != 3 || syms[1].Line != 4 || syms[2].Line != 5 {
		t.Errorf("unexpected lines: %+v", syms)
	}
	if syms[0].Text != "func A()" {
		t.Errorf("want cleaned signature 'func A()', got %q", syms[0].Text)
	}
	if syms[1].Text != "type B struct" {
		t.Errorf("want 'type B struct', got %q", syms[1].Text)
	}
}

func TestExtractSymbols_Python(t *testing.T) {
	src := "import os\n\ndef foo():\n    pass\n\nclass Bar:\n    def m(self):\n        pass\n"
	syms := extractSymbols(src, ".py")
	if len(syms) != 3 {
		t.Fatalf("want 3 symbols, got %d: %+v", len(syms), syms)
	}
	// Method inside class should keep its indented signature but be detected.
	if syms[0].Line != 3 || syms[1].Line != 6 || syms[2].Line != 7 {
		t.Errorf("unexpected lines: %+v", syms)
	}
}

func TestExtractSymbols_Zig(t *testing.T) {
	src := "const std = @import(\"std\");\n\n" + // line 1
		"pub fn main() !void {}\n\n" + // line 3
		"fn helper(x: u32) u32 {\n    return x;\n}\n\n" + // line 5
		"pub const Point = struct {\n    x: i32,\n    y: i32,\n};\n\n" + // line 9
		"const Color = enum { red, green, blue };\n" + // line 14
		"const Payload = union(enum) { a: u8, b: u16 };\n" + // line 15
		"extern \"c\" fn cFunc() void;\n" // line 16
	syms := extractSymbols(src, ".zig")
	if len(syms) != 6 {
		t.Fatalf("want 6 symbols, got %d: %+v", len(syms), syms)
	}
	wantLines := []int{3, 5, 9, 14, 15, 16}
	for i, w := range wantLines {
		if syms[i].Line != w {
			t.Errorf("symbol %d: want line %d, got %d (%q)", i, w, syms[i].Line, syms[i].Text)
		}
	}
	if syms[0].Text != "pub fn main() !void" {
		t.Errorf("want 'pub fn main() !void', got %q", syms[0].Text)
	}
	if syms[2].Text != "pub const Point = struct" {
		t.Errorf("want 'pub const Point = struct', got %q", syms[2].Text)
	}
	// The plain import const must NOT be treated as a symbol.
	for _, s := range syms {
		if s.Line == 1 {
			t.Errorf("import const should not be a symbol: %+v", s)
		}
	}
}

func TestExtractSymbols_UnknownExt(t *testing.T) {
	syms := extractSymbols("just some prose\nmore prose\n", ".txt")
	if len(syms) != 0 {
		t.Errorf("want no symbols for unknown ext, got %+v", syms)
	}
}

func TestCodeMapHandler_TreeMode(t *testing.T) {
	root := createTestTree(t, map[string]string{
		"main.go":           "package main\n\nfunc main() {}\nfunc helper() {}\n",
		"util.go":           "package main\n\ntype Foo struct{}\n",
		"vendor/dep.go":     "package dep\n\nfunc X() {}\n",
		"node_modules/x.js": "function y() {}\n",
		"README.md":         "# docs\n",
	})

	result, err := CodeMapHandler(context.Background(), codeMapCall(map[string]any{
		"path": root,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("result error: %s", result.Error)
	}
	if !contains(result.Content, "main.go") || !contains(result.Content, "util.go") {
		t.Errorf("expected code files listed, got: %s", result.Content)
	}
	if !contains(result.Content, "2 symbols") {
		t.Errorf("expected main.go to report 2 symbols, got: %s", result.Content)
	}
	if contains(result.Content, "vendor") {
		t.Errorf("should fold vendor/, got: %s", result.Content)
	}
	if contains(result.Content, "node_modules") {
		t.Errorf("should fold node_modules/, got: %s", result.Content)
	}
	// Non-code files are not part of a code map.
	if contains(result.Content, "README.md") {
		t.Errorf("should omit non-code README.md, got: %s", result.Content)
	}
	// Tree mode should NOT dump signatures.
	if contains(result.Content, "func main()") {
		t.Errorf("tree mode should not show signatures, got: %s", result.Content)
	}
}

func TestCodeMapHandler_SymbolsMode(t *testing.T) {
	root := createTestTree(t, map[string]string{
		"main.go": "package main\n\nfunc main() {}\n\nfunc helper(x int) error { return nil }\n",
	})

	result, err := CodeMapHandler(context.Background(), codeMapCall(map[string]any{
		"path":  root,
		"depth": "symbols",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("result error: %s", result.Error)
	}
	if !contains(result.Content, "func main()") {
		t.Errorf("expected func main signature, got: %s", result.Content)
	}
	if !contains(result.Content, "func helper(x int) error") {
		t.Errorf("expected func helper signature, got: %s", result.Content)
	}
	// Line numbers must be present so the model can range-read.
	if !contains(result.Content, "L") || !contains(result.Content, "5") {
		t.Errorf("expected line numbers, got: %s", result.Content)
	}
	// Recovery hint.
	if !contains(result.Content, "read_file") {
		t.Errorf("expected read_file recovery hint, got: %s", result.Content)
	}
}

func TestCodeMapHandler_MaxFiles(t *testing.T) {
	root := createTestTree(t, map[string]string{
		"a.go": "package p\nfunc A() {}\n",
		"b.go": "package p\nfunc B() {}\n",
		"c.go": "package p\nfunc C() {}\n",
		"d.go": "package p\nfunc D() {}\n",
		"e.go": "package p\nfunc E() {}\n",
	})

	result, err := CodeMapHandler(context.Background(), codeMapCall(map[string]any{
		"path":      root,
		"max_files": float64(2),
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !contains(result.Content, "Showing 2 of 5") {
		t.Errorf("expected pagination notice, got: %s", result.Content)
	}
	if !contains(result.Content, "max_files") {
		t.Errorf("expected max_files hint, got: %s", result.Content)
	}
}

func TestCodeMapHandler_PathScope(t *testing.T) {
	root := createTestTree(t, map[string]string{
		"pkg/agent/a.go": "package agent\nfunc A() {}\n",
		"pkg/tools/b.go": "package tools\nfunc B() {}\n",
	})

	result, err := CodeMapHandler(context.Background(), codeMapCall(map[string]any{
		"path": root + "/pkg/agent",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !contains(result.Content, "a.go") {
		t.Errorf("expected a.go in scoped map, got: %s", result.Content)
	}
	if contains(result.Content, "b.go") {
		t.Errorf("path scope should exclude b.go, got: %s", result.Content)
	}
}

func TestCodeMapHandler_SingleFile(t *testing.T) {
	root := createTestTree(t, map[string]string{
		"only.go": "package p\n\nfunc Solo() {}\n",
	})

	result, err := CodeMapHandler(context.Background(), codeMapCall(map[string]any{
		"path": root + "/only.go",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// A single file target should show its signatures directly.
	if !contains(result.Content, "func Solo()") {
		t.Errorf("expected signatures for single file, got: %s", result.Content)
	}
}

func TestCodeMapHandler_Empty(t *testing.T) {
	root := createTestTree(t, map[string]string{
		"notes.md": "# no code here\n",
	})

	result, err := CodeMapHandler(context.Background(), codeMapCall(map[string]any{
		"path": root,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !contains(result.Content, "No source files") {
		t.Errorf("expected empty notice, got: %s", result.Content)
	}
}

func TestCodeMapHandler_IncludeHiddenDescendsFoldDirs(t *testing.T) {
	// include_hidden=true is the documented recovery path for repos whose real
	// sources live in vendor/build/etc. — it must descend into fold dirs.
	root := createTestTree(t, map[string]string{
		"main.go":        "package main\nfunc Main() {}\n",
		"vendor/dep.go":  "package dep\nfunc Dep() {}\n",
		"build/gen.go":   "package gen\nfunc Gen() {}\n",
	})

	res, err := CodeMapHandler(context.Background(), codeMapCall(map[string]any{
		"path": root, "include_hidden": true,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !contains(res.Content, "vendor/dep.go") {
		t.Errorf("include_hidden=true should map vendor/, got: %s", res.Content)
	}
	if !contains(res.Content, "build/gen.go") {
		t.Errorf("include_hidden=true should map build/, got: %s", res.Content)
	}
}

func TestCodeMapTool_Registered(t *testing.T) {
	found := false
	for _, tool := range FileTools() {
		if tool.Name == "code_map" {
			found = true
			if !tool.ParallelSafe {
				t.Errorf("code_map should be parallel-safe (read-only)")
			}
		}
	}
	if !found {
		t.Errorf("code_map not registered in FileTools()")
	}
}
