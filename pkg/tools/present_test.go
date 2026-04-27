package tools

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func TestPresentFileRegistryRegisterTextFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(path, []byte("hello report"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	registry := NewPresentFileRegistry()
	err := registry.Register(PresentFile{
		Path:        path,
		Description: "Generated report",
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	files := registry.List()
	if len(files) != 1 {
		t.Fatalf("List length = %d, want 1", len(files))
	}

	file := files[0]
	if file.Path != path {
		t.Errorf("Path = %q, want %q", file.Path, path)
	}
	if file.Description != "Generated report" {
		t.Errorf("Description = %q, want %q", file.Description, "Generated report")
	}
	if file.MimeType != "text/plain; charset=utf-8" {
		t.Errorf("MimeType = %q, want text/plain; charset=utf-8", file.MimeType)
	}
	if file.Content != "hello report" {
		t.Errorf("Content = %q, want %q", file.Content, "hello report")
	}
	if file.ID == "" {
		t.Error("ID should not be empty")
	}
	if file.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set")
	}
}

func TestPresentFileRegistryRegisterBinaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "image.bin")
	data := []byte{0x00, 0x01, 0x02, 0xff}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	registry := NewPresentFileRegistry()
	err := registry.Register(PresentFile{Path: path})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	file := registry.List()[0]
	if file.MimeType != "application/octet-stream" {
		t.Errorf("MimeType = %q, want application/octet-stream", file.MimeType)
	}
	if file.Content != base64.StdEncoding.EncodeToString(data) {
		t.Errorf("Content = %q, want base64 payload", file.Content)
	}
}

func TestPresentFileRegistryRegisterUpdatesExistingPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "summary.md")
	if err := os.WriteFile(path, []byte("# summary"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	registry := NewPresentFileRegistry()
	if err := registry.Register(PresentFile{Path: path, Description: "first"}); err != nil {
		t.Fatalf("Register first failed: %v", err)
	}
	initial := registry.List()[0]

	if err := registry.Register(PresentFile{Path: path, Description: "second"}); err != nil {
		t.Fatalf("Register second failed: %v", err)
	}

	files := registry.List()
	if len(files) != 1 {
		t.Fatalf("List length = %d, want 1", len(files))
	}
	if files[0].ID != initial.ID {
		t.Errorf("ID = %q, want %q", files[0].ID, initial.ID)
	}
	if files[0].Description != "second" {
		t.Errorf("Description = %q, want second", files[0].Description)
	}
}

func TestPresentFileRegistryGetAndClear(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(path, []byte("notes"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	registry := NewPresentFileRegistry()
	if err := registry.Register(PresentFile{Path: path}); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	file := registry.List()[0]

	got, ok := registry.Get(file.ID)
	if !ok {
		t.Fatal("Get should find file")
	}
	if got.Path != file.Path {
		t.Errorf("Get path = %q, want %q", got.Path, file.Path)
	}

	registry.Clear()
	if len(registry.List()) != 0 {
		t.Error("List should be empty after Clear")
	}
}

func TestPresentFileTool(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "artifact.json")
	if err := os.WriteFile(path, []byte("{\"ok\":true}\n"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	registry := NewPresentFileRegistry()
	tool := PresentFileTool(registry)
	result, err := tool.Handler(context.Background(), models.ToolCall{
		ID:   "call_1",
		Name: "present_file",
		Arguments: map[string]any{
			"path":        path,
			"description": "Generated artifact",
		},
	})
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}
	if result.Status != models.CallStatusCompleted {
		t.Errorf("Status = %q, want %q", result.Status, models.CallStatusCompleted)
	}
	if result.Data["path"] != path {
		t.Errorf("Data path = %v, want %q", result.Data["path"], path)
	}
	if result.Data["mime_type"] != "application/json" {
		t.Errorf("Data mime_type = %v, want application/json", result.Data["mime_type"])
	}

	files := registry.List()
	if len(files) != 1 {
		t.Fatalf("Registry length = %d, want 1", len(files))
	}
	if files[0].Content != "{\"ok\":true}\n" {
		t.Errorf("Content = %q, want file contents", files[0].Content)
	}
}

func TestPresentFileToolMissingPath(t *testing.T) {
	registry := NewPresentFileRegistry()
	tool := PresentFileTool(registry)

	result, err := tool.Handler(context.Background(), models.ToolCall{
		ID:        "call_2",
		Name:      "present_file",
		Arguments: map[string]any{},
	})
	if err == nil {
		t.Fatal("Handler should fail when path is missing")
	}
	if result.Status != models.CallStatusFailed {
		t.Errorf("Status = %q, want %q", result.Status, models.CallStatusFailed)
	}
}

func TestPresentFileToolResolvesACPWorkspaceVirtualPath(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DEEPAI_DATA_ROOT", root)
	threadID := "thread-present-1"
	resolved := filepath.Join(root, "threads", threadID, "acp-workspace", "out", "artifact.txt")
	if err := os.MkdirAll(filepath.Dir(resolved), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(resolved, []byte("artifact output"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	registry := NewPresentFileRegistry()
	tool := PresentFileTool(registry)
	ctx := WithThreadID(context.Background(), threadID)
	result, err := tool.Handler(ctx, models.ToolCall{
		ID:   "call_virtual",
		Name: "present_file",
		Arguments: map[string]any{
			"path": "/mnt/acp-workspace/out/artifact.txt",
		},
	})
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}
	if result.Data["path"] != resolved {
		t.Fatalf("resolved path = %v, want %q", result.Data["path"], resolved)
	}
}
