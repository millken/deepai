package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/millken/deepai/pkg/models"
)

var presentFileSeq uint64

type PresentFile struct {
	ID          string    `json:"id"`
	Path        string    `json:"path"`
	Description string    `json:"description,omitempty"`
	MimeType    string    `json:"mime_type"`
	Content     string    `json:"content,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type PresentFileRegistry struct {
	mu    sync.RWMutex
	files map[string]PresentFile
}

func NewPresentFileRegistry() *PresentFileRegistry {
	return &PresentFileRegistry{
		files: make(map[string]PresentFile),
	}
}

func (r *PresentFileRegistry) Register(file PresentFile) error {
	if r == nil {
		return fmt.Errorf("present file registry is nil")
	}

	path := strings.TrimSpace(file.Path)
	if path == "" {
		return fmt.Errorf("path is required")
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	mimeType := strings.TrimSpace(file.MimeType)
	if mimeType == "" {
		mimeType = detectMimeType(absPath, data)
	}

	content := file.Content
	if strings.TrimSpace(content) == "" {
		content = encodePresentFileContent(mimeType, data)
	}

	description := strings.TrimSpace(file.Description)

	r.mu.Lock()
	defer r.mu.Unlock()

	id := strings.TrimSpace(file.ID)
	if id == "" {
		if existingID, existing, ok := r.findByPathLocked(absPath); ok {
			id = existingID
			if file.CreatedAt.IsZero() {
				file.CreatedAt = existing.CreatedAt
			}
		} else {
			id = newPresentFileID()
		}
	}

	createdAt := file.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	r.files[id] = PresentFile{
		ID:          id,
		Path:        absPath,
		Description: description,
		MimeType:    mimeType,
		Content:     content,
		CreatedAt:   createdAt,
	}
	return nil
}

func (r *PresentFileRegistry) List() []PresentFile {
	if r == nil {
		return nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	files := make([]PresentFile, 0, len(r.files))
	for _, file := range r.files {
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].CreatedAt.Equal(files[j].CreatedAt) {
			return files[i].ID < files[j].ID
		}
		return files[i].CreatedAt.Before(files[j].CreatedAt)
	})
	return files
}

func (r *PresentFileRegistry) Get(id string) (PresentFile, bool) {
	if r == nil {
		return PresentFile{}, false
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	file, ok := r.files[strings.TrimSpace(id)]
	return file, ok
}

func (r *PresentFileRegistry) Clear() {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.files = make(map[string]PresentFile)
}

func (r *PresentFileRegistry) findByPathLocked(path string) (string, PresentFile, bool) {
	for id, file := range r.files {
		if file.Path == path {
			return id, file, true
		}
	}
	return "", PresentFile{}, false
}

func PresentFileTool(registry *PresentFileRegistry) models.Tool {
	return models.Tool{
		Name:        "present_file",
		Description: "Register a file as a generated artifact for the UI to display.",
		Groups:      []string{"builtin", "file_ops"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":        map[string]any{"type": "string", "description": "Path to the generated file"},
				"description": map[string]any{"type": "string", "description": "Short description shown in the UI"},
				"mime_type":   map[string]any{"type": "string", "description": "Optional MIME type override"},
			},
			"required": []any{"path"},
		},
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			if registry == nil {
				err := fmt.Errorf("present file registry is required")
				return models.ToolResult{
					CallID:   call.ID,
					ToolName: call.Name,
					Status:   models.CallStatusFailed,
					Error:    err.Error(),
				}, err
			}

			path, _ := call.Arguments["path"].(string)
			description, _ := call.Arguments["description"].(string)
			mimeType, _ := call.Arguments["mime_type"].(string)
			resolvedPath := ResolveVirtualPath(ctx, path)

			file := PresentFile{
				Path:        resolvedPath,
				Description: description,
				MimeType:    mimeType,
			}
			if err := registry.Register(file); err != nil {
				return models.ToolResult{
					CallID:   call.ID,
					ToolName: call.Name,
					Status:   models.CallStatusFailed,
					Error:    err.Error(),
				}, err
			}

			registered, ok := latestRegisteredFile(registry, resolvedPath)
			if !ok {
				err := fmt.Errorf("registered file not found")
				return models.ToolResult{
					CallID:   call.ID,
					ToolName: call.Name,
					Status:   models.CallStatusFailed,
					Error:    err.Error(),
				}, err
			}

			return models.ToolResult{
				CallID:   call.ID,
				ToolName: call.Name,
				Status:   models.CallStatusCompleted,
				Content:  fmt.Sprintf("Registered file %s", registered.Path),
				Data: map[string]any{
					"id":          registered.ID,
					"path":        registered.Path,
					"description": registered.Description,
					"mime_type":   registered.MimeType,
					"created_at":  registered.CreatedAt.Format(time.RFC3339Nano),
				},
			}, nil
		},
	}
}

func latestRegisteredFile(registry *PresentFileRegistry, path string) (PresentFile, bool) {
	if registry == nil {
		return PresentFile{}, false
	}

	absPath, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return PresentFile{}, false
	}

	for _, file := range registry.List() {
		if file.Path == absPath {
			return file, true
		}
	}
	return PresentFile{}, false
}

func newPresentFileID() string {
	seq := atomic.AddUint64(&presentFileSeq, 1)
	return fmt.Sprintf("file_%d_%d", time.Now().UTC().UnixNano(), seq)
}

func detectMimeType(path string, data []byte) string {
	if ext := strings.TrimSpace(filepath.Ext(path)); ext != "" {
		if detected := mime.TypeByExtension(ext); detected != "" {
			return detected
		}
	}
	if len(data) == 0 {
		return "application/octet-stream"
	}
	return http.DetectContentType(data)
}

func encodePresentFileContent(mimeType string, data []byte) string {
	if len(data) == 0 {
		return ""
	}
	if isTextMimeType(mimeType) && utf8.Valid(data) {
		return string(data)
	}
	return base64.StdEncoding.EncodeToString(data)
}

func isTextMimeType(mimeType string) bool {
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	if strings.HasPrefix(mimeType, "text/") {
		return true
	}
	switch mimeType {
	case "application/json", "application/xml", "application/yaml", "application/x-yaml", "image/svg+xml":
		return true
	default:
		return false
	}
}
