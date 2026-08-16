package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

// Binary content reaching the model is not a cosmetic problem: read_file on
// this repo's own bin/deepai (79 MB, no extension) would pull the whole thing
// into context. grep skipped binaries by EXTENSION only, so anything
// extensionless or unlisted (.bin, .class, .pyc, .jar, .ttf, .pack) walked
// straight through, and read_file/code_map had no check at all.

func writeBinaryFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	// An ELF-ish header: high bytes and, crucially, NUL bytes.
	data := append([]byte{0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00}, make([]byte, 512)...)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIsBinaryContent_DetectsNulByte(t *testing.T) {
	if !isBinaryContent([]byte("hello\x00world")) {
		t.Fatal("a NUL byte marks the content binary")
	}
}

func TestIsBinaryContent_PlainTextIsNotBinary(t *testing.T) {
	if isBinaryContent([]byte("package main\n\nfunc main() {}\n")) {
		t.Fatal("plain source must not be classified binary")
	}
}

func TestIsBinaryContent_MultiByteUTF8IsNotBinary(t *testing.T) {
	// The false positive that would matter most for this user: CJK source and
	// comments are high-bit UTF-8 but perfectly readable text.
	if isBinaryContent([]byte("// 中文注释：并行分派 4 个 review 任务\nconst x = \"日本語とEmoji 🚀\"\n")) {
		t.Fatal("multi-byte UTF-8 text must not be classified binary")
	}
}

func TestIsBinaryContent_EmptyIsNotBinary(t *testing.T) {
	if isBinaryContent(nil) || isBinaryContent([]byte{}) {
		t.Fatal("an empty file is not binary")
	}
}

func TestReadFile_BinaryReturnsNoticeNotContent(t *testing.T) {
	dir := t.TempDir()
	path := writeBinaryFile(t, dir, "deepai") // no extension, like bin/deepai

	res, err := ReadFileHandler(context.Background(), models.ToolCall{
		ID: "r", Name: "read_file", Arguments: map[string]any{"path": path},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(res.Content, "ELF") || strings.ContainsRune(res.Content, 0) {
		t.Fatalf("binary bytes leaked into the result: %q", res.Content[:min(80, len(res.Content))])
	}
	if !strings.Contains(res.Content, "binary") {
		t.Fatalf("result should say the file is binary, got %q", res.Content)
	}
}

func TestReadFile_TextFileUnaffected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("package main\n// 中文\n"), 0644); err != nil {
		t.Fatal(err)
	}
	res, err := ReadFileHandler(context.Background(), models.ToolCall{
		ID: "r", Name: "read_file", Arguments: map[string]any{"path": path},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "package main\n// 中文\n" {
		t.Fatalf("text read changed: %q", res.Content)
	}
}

func TestReadFile_BinaryDetectedEvenWithLineRange(t *testing.T) {
	// A range read must not become a bypass for the check.
	dir := t.TempDir()
	path := writeBinaryFile(t, dir, "blob.dat")
	res, err := ReadFileHandler(context.Background(), models.ToolCall{
		ID: "r", Name: "read_file",
		Arguments: map[string]any{"path": path, "start_line": float64(1), "end_line": float64(5)},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.ContainsRune(res.Content, 0) {
		t.Fatal("binary bytes leaked through the range path")
	}
	if !strings.Contains(res.Content, "binary") {
		t.Fatalf("range read should report binary, got %q", res.Content)
	}
}

func TestGrep_SkipsExtensionlessBinary(t *testing.T) {
	dir := t.TempDir()
	// The binary contains the search term as literal bytes.
	path := filepath.Join(dir, "compiled")
	if err := os.WriteFile(path, append([]byte("needle\x00\x01\x02"), make([]byte, 256)...), 0644); err != nil {
		t.Fatal(err)
	}
	// A text file that legitimately matches, to prove grep still works.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("needle here\n"), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := GrepHandler(context.Background(), models.ToolCall{
		ID: "g", Name: "grep",
		Arguments: map[string]any{"pattern": "needle", "path": dir},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Content, "compiled") {
		t.Fatalf("grep matched inside an extensionless binary:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "notes.txt") {
		t.Fatalf("grep lost the legitimate text match:\n%s", res.Content)
	}
}

func TestCodeMap_SkipsBinary(t *testing.T) {
	dir := t.TempDir()
	writeBinaryFile(t, dir, "blob.zig") // a source extension, binary content
	if err := os.WriteFile(filepath.Join(dir, "real.zig"), []byte("pub fn main() void {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := CodeMapHandler(context.Background(), models.ToolCall{
		ID: "c", Name: "code_map", Arguments: map[string]any{"path": dir},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(res.Content, 0) {
		t.Fatal("binary bytes leaked into the code map")
	}
}
