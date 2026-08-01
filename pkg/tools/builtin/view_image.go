package builtin

import (
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/millken/deepai/pkg/models"
)

const maxViewImageBytes = 8 << 20

func ViewImageHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	path, _ := call.Arguments["image_path"].(string)
	path = strings.TrimSpace(path)
	if path == "" {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("image_path is required")
	}

	resolved := resolveReadablePath(ctx, path)
	if resolved == path && strings.HasPrefix(path, "/mnt/user-data/") {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("thread context is required for virtual image paths")
	}
	if !filepath.IsAbs(resolved) {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("image_path must be absolute")
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("image file not found: %w", err)
	}
	if !info.Mode().IsRegular() {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("image_path must point to a file")
	}
	if info.Size() > maxViewImageBytes {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("image exceeds %d MB limit", maxViewImageBytes>>20)
	}

	ext := strings.ToLower(filepath.Ext(resolved))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif":
	default:
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("unsupported image format %q", ext)
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return models.ToolResult{CallID: call.ID, ToolName: call.Name}, fmt.Errorf("read image: %w", err)
	}

	mimeType := mime.TypeByExtension(ext)
	if mimeType == "" {
		switch ext {
		case ".jpg", ".jpeg":
			mimeType = "image/jpeg"
		case ".png":
			mimeType = "image/png"
		case ".webp":
			mimeType = "image/webp"
		case ".gif":
			mimeType = "image/gif"
		}
	}

	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Status:   models.CallStatusCompleted,
		Content:  fmt.Sprintf("Loaded image %s for analysis.", path),
		Data: map[string]any{
			"viewed_images": []map[string]any{{
				"path":      path,
				"mime_type": mimeType,
				"base64":    base64.StdEncoding.EncodeToString(data),
			}},
		},
	}, nil
}

func ViewImageTool() models.Tool {
	return models.Tool{
		Name:         "view_image",
		Description:  "Load an image file so the agent can inspect it in a later model turn.",
		Groups:       []string{"builtin", "image"},
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"image_path": map[string]any{
					"type":        "string",
					"description": "Absolute image path. For thread files use /mnt/user-data/uploads/... or /mnt/user-data/outputs/...",
				},
			},
			"required": []any{"image_path"},
		},
		Handler: ViewImageHandler,
	}
}
