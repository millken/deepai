package agent

import (
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func TestSanitizedToolResultStripsBase64Payload(t *testing.T) {
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

	sanitized := sanitizedToolResult(result)
	images, _ := sanitized.Data["viewed_images"].([]map[string]any)
	if len(images) != 1 {
		t.Fatalf("viewed_images=%v", sanitized.Data["viewed_images"])
	}
	if _, ok := images[0]["base64"]; ok {
		t.Fatalf("expected base64 to be removed: %v", images[0])
	}
}

func TestViewedImagesMessageAddsMultiContentMetadata(t *testing.T) {
	msg := viewedImagesMessage("thread-1", []viewedImage{{
		Path:     "/mnt/user-data/uploads/demo.png",
		MIMEType: "image/png",
		Base64:   "abc",
	}}, true)

	if msg.Role != models.RoleHuman {
		t.Fatalf("role=%s", msg.Role)
	}
	if msg.Metadata["transient_viewed_images"] != "true" {
		t.Fatalf("transient_viewed_images=%q want=true", msg.Metadata["transient_viewed_images"])
	}
	if msg.Metadata["multi_content"] == "" {
		t.Fatal("expected multi_content metadata")
	}
}
