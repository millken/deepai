package builtin

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

func TestViewImageHandlerResolvesThreadVirtualPath(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DEERFLOW_DATA_ROOT", root)

	threadID := "thread-view-image"
	target := filepath.Join(root, "threads", threadID, "user-data", "uploads", "pixel.png")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	image := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	if err := os.WriteFile(target, image, 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}

	ctx := tools.WithThreadID(context.Background(), threadID)
	result, err := ViewImageHandler(ctx, models.ToolCall{
		ID:   "call-view-image",
		Name: "view_image",
		Arguments: map[string]any{
			"image_path": "/mnt/user-data/uploads/pixel.png",
		},
	})
	if err != nil {
		t.Fatalf("ViewImageHandler() error = %v", err)
	}

	images, _ := result.Data["viewed_images"].([]map[string]any)
	if len(images) != 1 {
		t.Fatalf("viewed_images=%v", result.Data["viewed_images"])
	}
	if got := images[0]["path"]; got != "/mnt/user-data/uploads/pixel.png" {
		t.Fatalf("path=%v", got)
	}
	if got := images[0]["mime_type"]; got != "image/png" {
		t.Fatalf("mime_type=%v", got)
	}
	if got := images[0]["base64"]; got != base64.StdEncoding.EncodeToString(image) {
		t.Fatalf("base64=%v", got)
	}
}

func TestViewImageHandlerRejectsUnsupportedExtension(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DEERFLOW_DATA_ROOT", root)

	threadID := "thread-view-image-bad"
	target := filepath.Join(root, "threads", threadID, "user-data", "uploads", "notes.txt")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(target, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	ctx := tools.WithThreadID(context.Background(), threadID)
	_, err := ViewImageHandler(ctx, models.ToolCall{
		ID:   "call-view-image-bad",
		Name: "view_image",
		Arguments: map[string]any{
			"image_path": "/mnt/user-data/uploads/notes.txt",
		},
	})
	if err == nil {
		t.Fatal("expected error for unsupported extension")
	}
}
