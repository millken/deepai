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

const (
	defaultSessionSearchLimit = 20
	maxSessionSearchLimit     = 50
	sessionSearchSnippetLimit = 400
)

func clampSessionSearchLimit(raw float64) int {
	limit := defaultSessionSearchLimit
	if raw > 0 {
		limit = int(raw)
	}
	if limit <= 0 {
		limit = defaultSessionSearchLimit
	}
	if limit > maxSessionSearchLimit {
		limit = maxSessionSearchLimit
	}
	return limit
}

func truncateSearchSnippet(content string, max int) string {
	content = strings.TrimSpace(content)
	if max <= 0 || len(content) <= max {
		return content
	}
	if max <= 3 {
		return content[:max]
	}
	return content[:max-3] + "..."
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

		limit := defaultSessionSearchLimit
		if v, ok := call.Arguments["limit"].(float64); ok {
			limit = clampSessionSearchLimit(v)
		}
		roleFilter, _ := call.Arguments["role"].(string)
		roleFilter = strings.TrimSpace(strings.ToLower(roleFilter))
		sessionFilter, _ := call.Arguments["session_id"].(string)
		sessionFilter = strings.TrimSpace(sessionFilter)
		fetchLimit := limit
		if roleFilter != "" || sessionFilter != "" {
			fetchLimit = limit * 5
			if fetchLimit > maxSessionSearchLimit*5 {
				fetchLimit = maxSessionSearchLimit * 5
			}
		}

		hits, err := searcher.SearchMessages(ctx, query, fetchLimit)
		if err != nil {
			return toolError(call, fmt.Sprintf("search failed: %v", err)), nil
		}
		filtered := make([]SearchHit, 0, limit)
		for _, hit := range hits {
			if sessionFilter != "" && hit.SessionID != sessionFilter {
				continue
			}
			if roleFilter != "" && strings.ToLower(strings.TrimSpace(hit.Role)) != roleFilter {
				continue
			}
			hit.Content = truncateSearchSnippet(hit.Content, sessionSearchSnippetLimit)
			filtered = append(filtered, hit)
			if len(filtered) >= limit {
				break
			}
		}

		data, _ := json.Marshal(map[string]any{
			"query":      query,
			"total":      len(filtered),
			"limit":      limit,
			"session_id": sessionFilter,
			"role":       roleFilter,
			"results":    filtered,
		})
		return models.ToolResult{
			CallID:   call.ID,
			ToolName: call.Name,
			Status:   models.CallStatusCompleted,
			Content:  string(data),
		}, nil
	}

	return models.Tool{
		Name:         "session_search",
		Description:  "Search across past session messages using full-text search. Returns relevance-ranked snippets and supports optional session_id / role filtering.",
		ParallelSafe: true,
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
				"session_id": map[string]any{
					"type":        "string",
					"description": "Optional filter for a specific session id",
				},
				"role": map[string]any{
					"type":        "string",
					"description": "Optional filter for message role (e.g. human, ai, tool)",
				},
			},
			"required": []string{"query"},
		},
		Handler: handler,
	}
}
