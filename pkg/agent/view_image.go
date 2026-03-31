package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
)

type viewedImage struct {
	Path     string `json:"path"`
	MIMEType string `json:"mime_type"`
	Base64   string `json:"base64"`
}

func collectViewedImages(result models.ToolResult) []viewedImage {
	if len(result.Data) == 0 {
		return nil
	}
	raw, ok := result.Data["viewed_images"]
	if !ok {
		return nil
	}
	items, ok := raw.([]map[string]any)
	if ok {
		out := make([]viewedImage, 0, len(items))
		for _, item := range items {
			out = appendIfViewedImage(out, item)
		}
		return out
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]viewedImage, 0, len(arr))
	for _, item := range arr {
		obj, _ := item.(map[string]any)
		out = appendIfViewedImage(out, obj)
	}
	return out
}

func appendIfViewedImage(dst []viewedImage, item map[string]any) []viewedImage {
	if len(item) == 0 {
		return dst
	}
	path := strings.TrimSpace(asString(item["path"]))
	mimeType := strings.TrimSpace(asString(item["mime_type"]))
	base64 := strings.TrimSpace(asString(item["base64"]))
	if path == "" || mimeType == "" || base64 == "" {
		return dst
	}
	return append(dst, viewedImage{Path: path, MIMEType: mimeType, Base64: base64})
}

func sanitizedToolResult(result models.ToolResult) models.ToolResult {
	images := collectViewedImages(result)
	if len(images) == 0 {
		return result
	}
	copyResult := result
	copyResult.Data = map[string]any{
		"viewed_images": summarizeViewedImages(images),
	}
	return copyResult
}

func summarizeViewedImages(images []viewedImage) []map[string]any {
	out := make([]map[string]any, 0, len(images))
	for _, image := range images {
		out = append(out, map[string]any{
			"path":      image.Path,
			"mime_type": image.MIMEType,
		})
	}
	return out
}

func viewedImagesMessage(sessionID string, images []viewedImage, includeImageData bool) models.Message {
	content := formatViewedImagesText(images)
	msg := models.Message{
		ID:        newMessageID("human"),
		SessionID: sessionID,
		Role:      models.RoleHuman,
		Content:   content,
		Metadata:  map[string]string{"transient_viewed_images": "true"},
		CreatedAt: time.Now().UTC(),
	}
	if !includeImageData || len(images) == 0 {
		return msg
	}

	parts := make([]map[string]any, 0, len(images)*2+1)
	parts = append(parts, map[string]any{"type": "text", "text": "Here are the images you've viewed:"})
	for _, image := range images {
		parts = append(parts, map[string]any{"type": "text", "text": fmt.Sprintf("\n- %s (%s)", image.Path, image.MIMEType)})
		parts = append(parts, map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": "data:" + image.MIMEType + ";base64," + image.Base64,
			},
		})
	}
	if raw, err := json.Marshal(parts); err == nil {
		msg.Metadata["multi_content"] = string(raw)
	}
	return msg
}

func formatViewedImagesText(images []viewedImage) string {
	if len(images) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Here are the images you've viewed:\n")
	for _, image := range images {
		b.WriteString("- ")
		b.WriteString(image.Path)
		if image.MIMEType != "" {
			b.WriteString(" (")
			b.WriteString(image.MIMEType)
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	b.WriteString("Use them when continuing the task.")
	return b.String()
}

func modelLikelySupportsVision(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	for _, token := range []string{
		"gpt-4o", "gpt-4.1", "gpt-4.5", "gpt-5", "claude-3", "claude-sonnet-4", "gemini", "qwen-vl", "qvq", "internvl", "vision",
	} {
		if strings.Contains(model, token) {
			return true
		}
	}
	return false
}

func ModelLikelySupportsVision(model string) bool {
	return modelLikelySupportsVision(model)
}

func asString(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case fmt.Stringer:
		return value.String()
	default:
		return ""
	}
}
