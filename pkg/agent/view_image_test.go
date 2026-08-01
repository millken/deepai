package agent

import (
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func TestSanitizeToolResultImagesStripsBase64Payload(t *testing.T) {
	result := models.ToolResult{
		CallID:   "call-1",
		ToolName: "view_image",
		Data: map[string]any{
			"viewed_images": []map[string]any{{
				"path":      "/mnt/user-data/uploads/demo.png",
				"mime_type": "image/png",
				"base64":    "abc",
			}},
		},
	}

	sanitized := sanitizeToolResultImages(result)
	images, _ := sanitized.Data["viewed_images"].([]map[string]any)
	if len(images) != 1 {
		t.Fatalf("viewed_images=%v", sanitized.Data["viewed_images"])
	}
	if _, ok := images[0]["base64"]; ok {
		t.Fatalf("expected base64 to be removed: %v", images[0])
	}
}

func TestExtractImagesFromToolResult(t *testing.T) {
	result := models.ToolResult{
		CallID:   "call-1",
		ToolName: "view_image",
		Data: map[string]any{
			"viewed_images": []map[string]any{
				{"path": "/a.png", "mime_type": "image/png", "base64": "AAA"},
				{"path": "/b.jpg", "mime_type": "image/jpeg", "base64": "BBB"},
			},
		},
	}

	images := extractImagesFromToolResult(result)
	if len(images) != 2 {
		t.Fatalf("expected 2 images, got %d", len(images))
	}
	if images[0].MimeType != "image/png" || images[0].Base64 != "AAA" {
		t.Errorf("image[0] = %+v", images[0])
	}
	if images[1].MimeType != "image/jpeg" || images[1].Base64 != "BBB" {
		t.Errorf("image[1] = %+v", images[1])
	}
}

func TestExtractImagesFromToolResult_Empty(t *testing.T) {
	result := models.ToolResult{
		CallID:   "call-1",
		ToolName: "bash",
		Data:     map[string]any{"exit_code": 0},
	}
	if images := extractImagesFromToolResult(result); images != nil {
		t.Fatalf("expected nil for non-image result, got %d", len(images))
	}
}

func TestImagesMessage(t *testing.T) {
	images := []models.MessageImage{
		{MimeType: "image/png", Base64: "AAA"},
	}
	msg := imagesMessage("session-1", images)
	if msg.Role != models.RoleHuman {
		t.Fatalf("role=%s", msg.Role)
	}
	if len(msg.Images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(msg.Images))
	}
	if msg.Metadata["transient_images"] != "true" {
		t.Fatalf("transient_images=%q", msg.Metadata["transient_images"])
	}
	if msg.Content == "" {
		t.Fatal("content should not be empty")
	}
}
