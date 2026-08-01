package agent

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
)

// viewedImage is the intermediate struct for images returned by the view_image
// tool (stored in ToolResult.Data["viewed_images"]).
type viewedImage struct {
	Path     string `json:"path"`
	MIMEType string `json:"mime_type"`
	Base64   string `json:"base64"`
}

// extractImagesFromToolResult pulls image attachments from a tool result's
// Data["viewed_images"] field (populated by the view_image tool). Returns nil
// if the result has no images.
func extractImagesFromToolResult(result models.ToolResult) []models.MessageImage {
	images := collectViewedImages(result)
	if len(images) == 0 {
		return nil
	}
	out := make([]models.MessageImage, 0, len(images))
	for _, img := range images {
		if img.Base64 == "" || img.MIMEType == "" {
			continue
		}
		out = append(out, models.MessageImage{
			MimeType: img.MIMEType,
			Base64:   img.Base64,
		})
	}
	return out
}

// sanitizeToolResultImages returns a copy of the tool result with image base64
// data stripped from Data["viewed_images"], keeping only path/mime_type
// metadata. This prevents large base64 blobs from bloating the persisted
// message Content while the actual image data travels via the Images field.
func sanitizeToolResultImages(result models.ToolResult) models.ToolResult {
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

// imagesMessage creates a human message carrying image attachments (from
// view_image tool results) so the LLM can see them in the next turn.
func imagesMessage(sessionID string, images []models.MessageImage) models.Message {
	return models.Message{
		ID:        newMessageID("human"),
		SessionID: sessionID,
		Role:      models.RoleHuman,
		Content:   formatImagesText(len(images)),
		Images:    images,
		Metadata:  map[string]string{"transient_images": "true"},
		CreatedAt: time.Now().UTC(),
	}
}

func formatImagesText(count int) string {
	if count == 0 {
		return ""
	}
	if count == 1 {
		return "Here is the image you requested:"
	}
	return formatImagesCount(count)
}

func formatImagesCount(count int) string {
	return "Here are the images you requested (" + strconv.Itoa(count) + " total):"
}

// appendToolResultMessage appends a tool result message to runMessages. If the
// result contains viewed images (from view_image tool), it sanitizes the
// stored result (strips base64) and injects a follow-up human message carrying
// the image attachments so the LLM can see them.
// Returns the new slice (append may reallocate).
func appendToolResultMessage(runMessages []models.Message, sessionID string, result models.ToolResult) []models.Message {
	// Extract images before sanitizing (sanitize strips base64 from Data).
	imgs := extractImagesFromToolResult(result)
	if len(imgs) > 0 {
		result = sanitizeToolResultImages(result)
	}
	runMessages = append(runMessages, models.Message{
		ID:         newMessageID("tool"),
		SessionID:  sessionID,
		Role:       models.RoleTool,
		Content:    toolMessageContent(result),
		ToolResult: &result,
		CreatedAt:  time.Now().UTC(),
	})
	// Inject images as a human message so the LLM sees them.
	if len(imgs) > 0 {
		runMessages = append(runMessages, imagesMessage(sessionID, imgs))
	}
	return runMessages
}

// collectViewedImages extracts viewedImage entries from a tool result's
// Data["viewed_images"] field, handling both []map[string]any and []any types.
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
