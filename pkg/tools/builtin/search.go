package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/millken/deepai/pkg/models"
)

// MessageSearcher provides full-text search across session messages.
type MessageSearcher interface {
	SearchMessages(ctx context.Context, query string, limit int) ([]SearchHit, error)
}

// SearchHit represents a single search result.
type SearchHit struct {
	SessionID string `json:"session_id"`
	MessageID string `json:"message_id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
}

// SearchFunc adapts a function to the MessageSearcher interface.
type SearchFunc func(ctx context.Context, query string, limit int) ([]SearchHit, error)

func (f SearchFunc) SearchMessages(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	return f(ctx, query, limit)
}

// SessionSearchTool returns a tool that searches across all session messages
// using PostgreSQL full-text search. If searcher is nil, all calls return an error.
func SessionSearchTool(searcher MessageSearcher) models.Tool {
	handler := func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
		if searcher == nil {
			return models.ToolResult{
				CallID:   call.ID,
				ToolName: call.Name,
				Status:   models.CallStatusFailed,
				Error:    "session search is not available (requires PostgreSQL)",
			}, nil
		}

		query, _ := call.Arguments["query"].(string)
		query = strings.TrimSpace(query)
		if query == "" {
			return toolError(call, "query is required"), nil
		}

		limit := 20
		if v, ok := call.Arguments["limit"].(float64); ok && v > 0 {
			limit = int(v)
		}

		hits, err := searcher.SearchMessages(ctx, query, limit)
		if err != nil {
			return toolError(call, fmt.Sprintf("search failed: %v", err)), nil
		}

		data, _ := json.Marshal(map[string]any{
			"query":       query,
			"total":       len(hits),
			"results":     hits,
		})
		return models.ToolResult{
			CallID:   call.ID,
			ToolName: call.Name,
			Status:   models.CallStatusCompleted,
			Content:  string(data),
		}, nil
	}

	return models.Tool{
		Name:        "session_search",
		Description: "Search across all past session messages using full-text search. Returns matching messages ranked by relevance. Useful for finding previous conversations, decisions, or context.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Search query (plain text, full-text matching)",
				},
				"limit": map[string]any{
					"type":        "number",
					"description": "Maximum results to return (default 20, max 50)",
				},
			},
			"required": []string{"query"},
		},
		Handler: handler,
	}
}
