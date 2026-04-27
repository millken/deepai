package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/memory"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// loadOrCreate returns the current document for the session, or an empty one if none exists yet.
func loadOrCreate(ctx context.Context, svc *memory.Service, sessionID string) (memory.Document, error) {
	doc, err := svc.Load(ctx, sessionID)
	if err != nil {
		if errors.Is(err, memory.ErrNotFound) {
			return memory.Document{SessionID: sessionID}, nil
		}
		return memory.Document{}, err
	}
	return doc, nil
}

// MemoryTool returns a tool that lets the agent manage persistent memory facts.
// If memService is nil the tool is still registered but all actions return an error.
func MemoryTool(memService *memory.Service) models.Tool {
	handler := func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
		if memService == nil {
			return models.ToolResult{
				CallID:   call.ID,
				ToolName: call.Name,
				Status:   models.CallStatusFailed,
				Error:    "memory service is not configured",
			}, nil
		}

		sessionID, _ := call.Arguments["session_id"].(string)
		sessionID = strings.TrimSpace(sessionID)
		if sessionID == "" {
			sessionID = tools.ThreadIDFromContext(ctx)
		}
		if sessionID == "" {
			return models.ToolResult{
				CallID:   call.ID,
				ToolName: call.Name,
				Status:   models.CallStatusFailed,
				Error:    "session id is required",
			}, nil
		}

		action, _ := call.Arguments["action"].(string)
		switch action {
		case "add_fact":
			return memoryAddFact(ctx, memService, call, sessionID)
		case "replace_fact":
			return memoryReplaceFact(ctx, memService, call, sessionID)
		case "remove_fact":
			return memoryRemoveFact(ctx, memService, call, sessionID)
		case "read":
			return memoryRead(ctx, memService, call, sessionID)
		default:
			return models.ToolResult{
				CallID:   call.ID,
				ToolName: call.Name,
				Status:   models.CallStatusFailed,
				Error:    fmt.Sprintf("unknown action %q; valid: add_fact, replace_fact, remove_fact, read", action),
			}, nil
		}
	}

	return models.Tool{
		Name:        "memory",
		Description: "Manage persistent memory facts about the user, environment, and stable conventions. session_id is optional; when omitted the current thread id is used. Use add_fact to remember new information, replace_fact to update existing facts, remove_fact to delete outdated ones, and read to view current facts.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type":        "string",
					"enum":        []string{"add_fact", "replace_fact", "remove_fact", "read"},
					"description": "Operation type",
				},
				"fact_id": map[string]any{
					"type":        "string",
					"description": "Fact ID (required for replace/remove; auto-generated for add)",
				},
				"session_id": map[string]any{
					"type":        "string",
					"description": "Optional explicit memory session identifier. Defaults to the current thread id when omitted.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "Fact content (required for add/replace)",
				},
				"category": map[string]any{
					"type":        "string",
					"enum":        []string{"work", "personal", "preference", "project", "other"},
					"description": "Fact category",
				},
			},
			"required": []string{"action"},
		},
		Handler: handler,
	}
}

func memoryAddFact(ctx context.Context, svc *memory.Service, call models.ToolCall, sessionID string) (models.ToolResult, error) {
	content, _ := call.Arguments["content"].(string)
	content = strings.TrimSpace(content)
	if content == "" {
		return toolError(call, "content is required for add_fact"), nil
	}
	if len(content) > memory.MaxFactContentLen {
		return toolError(call, fmt.Sprintf("content exceeds %d character limit", memory.MaxFactContentLen)), nil
	}
	if err := memory.ScanContent(content); err != nil {
		return toolError(call, fmt.Sprintf("content blocked by security filter: %v", err)), nil
	}

	category := "other"
	if c, ok := call.Arguments["category"].(string); ok && c != "" {
		category = c
	}

	doc, err := loadOrCreate(ctx, svc, sessionID)
	if err != nil {
		return toolError(call, fmt.Sprintf("load memory: %v", err)), nil
	}

	if len(doc.Facts) >= memory.MaxFactsPerSession {
		return toolError(call, fmt.Sprintf("fact limit reached (%d); remove an existing fact first", memory.MaxFactsPerSession)), nil
	}

	now := time.Now().UTC()
	fact := memory.Fact{
		ID:         fmt.Sprintf("f_%d", now.UnixNano()),
		Content:    content,
		Category:   category,
		Confidence: 0.8,
		Source:     sessionID,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	doc.Facts = append(doc.Facts, fact)
	if doc.SessionID == "" {
		doc.SessionID = sessionID
	}
	doc.UpdatedAt = now

	if err := svc.Save(ctx, doc); err != nil {
		return toolError(call, fmt.Sprintf("save memory: %v", err)), nil
	}

	data, _ := json.Marshal(map[string]any{
		"status":  "added",
		"fact_id": fact.ID,
	})
	return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusCompleted, Content: string(data)}, nil
}

func memoryReplaceFact(ctx context.Context, svc *memory.Service, call models.ToolCall, sessionID string) (models.ToolResult, error) {
	factID, _ := call.Arguments["fact_id"].(string)
	factID = strings.TrimSpace(factID)
	if factID == "" {
		return toolError(call, "fact_id is required for replace_fact"), nil
	}

	doc, err := loadOrCreate(ctx, svc, sessionID)
	if err != nil {
		return toolError(call, fmt.Sprintf("load memory: %v", err)), nil
	}

	idx := -1
	for i, f := range doc.Facts {
		if f.ID == factID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return toolError(call, fmt.Sprintf("fact %q not found", factID)), nil
	}

	if content, ok := call.Arguments["content"].(string); ok {
		content = strings.TrimSpace(content)
		if content == "" {
			return toolError(call, "content cannot be empty"), nil
		}
		if len(content) > memory.MaxFactContentLen {
			return toolError(call, fmt.Sprintf("content exceeds %d character limit", memory.MaxFactContentLen)), nil
		}
		if err := memory.ScanContent(content); err != nil {
			return toolError(call, fmt.Sprintf("content blocked by security filter: %v", err)), nil
		}
		doc.Facts[idx].Content = content
	}
	if c, ok := call.Arguments["category"].(string); ok && c != "" {
		doc.Facts[idx].Category = c
	}
	doc.Facts[idx].UpdatedAt = time.Now().UTC()
	doc.UpdatedAt = time.Now().UTC()

	if err := svc.Save(ctx, doc); err != nil {
		return toolError(call, fmt.Sprintf("save memory: %v", err)), nil
	}

	data, _ := json.Marshal(map[string]any{"status": "updated", "fact_id": factID})
	return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusCompleted, Content: string(data)}, nil
}

func memoryRemoveFact(ctx context.Context, svc *memory.Service, call models.ToolCall, sessionID string) (models.ToolResult, error) {
	factID, _ := call.Arguments["fact_id"].(string)
	factID = strings.TrimSpace(factID)
	if factID == "" {
		return toolError(call, "fact_id is required for remove_fact"), nil
	}

	doc, err := loadOrCreate(ctx, svc, sessionID)
	if err != nil {
		return toolError(call, fmt.Sprintf("load memory: %v", err)), nil
	}

	found := false
	filtered := doc.Facts[:0]
	for _, f := range doc.Facts {
		if f.ID == factID {
			found = true
			continue
		}
		filtered = append(filtered, f)
	}
	if !found {
		return toolError(call, fmt.Sprintf("fact %q not found", factID)), nil
	}

	doc.Facts = filtered
	doc.UpdatedAt = time.Now().UTC()

	if err := svc.Save(ctx, doc); err != nil {
		return toolError(call, fmt.Sprintf("save memory: %v", err)), nil
	}

	data, _ := json.Marshal(map[string]any{"status": "removed", "fact_id": factID})
	return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusCompleted, Content: string(data)}, nil
}

func memoryRead(ctx context.Context, svc *memory.Service, call models.ToolCall, sessionID string) (models.ToolResult, error) {
	doc, err := loadOrCreate(ctx, svc, sessionID)
	if err != nil {
		return toolError(call, fmt.Sprintf("load memory: %v", err)), nil
	}

	type factSummary struct {
		ID         string  `json:"id"`
		Content    string  `json:"content"`
		Category   string  `json:"category"`
		Confidence float64 `json:"confidence"`
		UpdatedAt  string  `json:"updated_at"`
	}
	facts := make([]factSummary, 0, len(doc.Facts))
	for _, f := range doc.Facts {
		facts = append(facts, factSummary{
			ID:         f.ID,
			Content:    f.Content,
			Category:   f.Category,
			Confidence: f.Confidence,
			UpdatedAt:  f.UpdatedAt.Format(time.RFC3339),
		})
	}
	data, _ := json.Marshal(map[string]any{
		"session_id": sessionID,
		"fact_count": len(facts),
		"facts":      facts,
	})
	return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusCompleted, Content: string(data)}, nil
}

func toolError(call models.ToolCall, msg string) models.ToolResult {
	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Status:   models.CallStatusFailed,
		Error:    msg,
	}
}
