package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func TestRecallOutput_PagesByOffset(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("A", 20_000) + strings.Repeat("B", 20_000) + strings.Repeat("C", 40_000)
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := RecallOutputTool(dir)
	call := func(args map[string]any) string {
		t.Helper()
		res, err := tool.Handler(context.Background(), models.ToolCall{
			ID: "c", Name: "recall_output", Arguments: args,
		})
		if err != nil {
			t.Fatal(err)
		}
		return res.Content
	}
	first := call(map[string]any{"ref": "big"})
	if !strings.HasPrefix(first, strings.Repeat("A", 100)) {
		t.Fatalf("default page does not start at byte 0: %.40q", first)
	}
	if strings.Contains(first, strings.Repeat("C", 50)) {
		t.Fatal("default page included the end of an 80 KiB file")
	}
	if !strings.Contains(first, "bytes 0-32768 of 80000") || !strings.Contains(first, "Next chunk starts at offset 32768") {
		t.Fatalf("default page notice = %.180q", first[len(first)-180:])
	}
	second := call(map[string]any{"ref": "big", "offset": float64(32768)})
	if !strings.HasPrefix(second, body[32768:32768+80]) {
		t.Fatalf("second page does not continue at offset 32768: %.40q", second)
	}
	if !strings.Contains(second, "Next chunk starts at offset 65536") {
		t.Fatalf("second page notice = %.180q", second[len(second)-180:])
	}
	clamped := call(map[string]any{"ref": "big", "offset": float64(65536), "limit": float64(1_000_000)})
	if !strings.HasPrefix(clamped, body[65536:65536+80]) {
		t.Fatalf("clamped page does not start at offset 65536: %.40q", clamped)
	}
	if !strings.Contains(clamped, "End of output") {
		t.Fatalf("last page = %.180q", clamped[len(clamped)-180:])
	}
	page := strings.SplitN(clamped, "\n[recall_output:", 2)[0]
	if len(page) > recallMaxBytes {
		t.Fatalf("page is %d bytes, hard cap is %d", len(page), recallMaxBytes)
	}
	narrow := call(map[string]any{"ref": "big", "limit": float64(512)})
	if !strings.HasPrefix(narrow, strings.Repeat("A", 512)) || !strings.Contains(narrow, "offset 512") {
		t.Fatalf("limit=512 page = %.160q", narrow)
	}
}

func TestRecallPage_TinyLimitAdvances(t *testing.T) {
	body := strings.Repeat("中", 100) + strings.Repeat("文", 100)
	first, notice := recallPage([]byte(body), 0, 1)
	if first == "" || !strings.HasPrefix(first, "中") || strings.Contains(first, "文") {
		t.Fatalf("tiny limit returned %q", first)
	}
	if !strings.Contains(notice, "Next chunk starts at offset 255") {
		t.Fatalf("notice did not advance past the first rune: %q", notice)
	}
	second, notice2 := recallPage([]byte(body), 255, 1)
	if !strings.Contains(second, "文") {
		t.Fatalf("second page did not advance: %q", second)
	}
	if !strings.Contains(notice2, "Next chunk starts at offset 510") {
		t.Fatalf("second notice stayed on the same offset: %q", notice2)
	}
}

func TestRecallOutputPath_RejectsEscape(t *testing.T) {
	dir := t.TempDir()
	if _, err := recallOutputPath(dir, ""); err == nil {
		t.Fatal("empty ref")
	}
	if _, err := recallOutputPath(dir, ".."); err == nil {
		t.Fatal("..")
	}
	if _, err := recallOutputPath(dir, "nested/id"); err == nil {
		t.Fatal("nested id")
	}
	outside := filepath.Join(filepath.Dir(dir), "secret.txt")
	if _, err := recallOutputPath(dir, outside); err == nil {
		t.Fatal("absolute path outside the directory")
	}
	inside := filepath.Join(dir, "call-1.txt")
	if err := os.WriteFile(inside, []byte("full"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := recallOutputPath(dir, "call-1")
	if err != nil || got != inside {
		t.Fatalf("bare id: path=%q err=%v", got, err)
	}
	got, err = recallOutputPath(dir, inside)
	if err != nil || got != inside {
		t.Fatalf("absolute path: path=%q err=%v", got, err)
	}

	tool := RecallOutputTool(dir)
	res, err := tool.Handler(context.Background(), models.ToolCall{
		ID: "c", Name: "recall_output", Arguments: map[string]any{"ref": "call-1"},
	})
	if err != nil || res.Content != "full" {
		t.Fatalf("content=%q err=%v", res.Content, err)
	}
}
