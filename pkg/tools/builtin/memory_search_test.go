package builtin

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	memstore "github.com/millken/deepai/pkg/memory"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

type fakeSearcher struct {
	limit int
	hits  []SearchHit
	err   error
}

func (f *fakeSearcher) SearchMessages(_ context.Context, _ string, limit int) ([]SearchHit, error) {
	f.limit = limit
	return f.hits, f.err
}

func TestMemoryTool_ExplicitSessionIDOverridesThreadContext(t *testing.T) {
	store, err := memstore.NewFileStore(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatal(err)
	}
	svc := memstore.NewService(slog.Default(), store, nil)
	tool := MemoryTool(svc)
	ctx := tools.WithThreadID(context.Background(), "thread-session")

	_, err = tool.Handler(ctx, models.ToolCall{
		ID:   "mem-add",
		Name: "memory",
		Arguments: map[string]any{
			"action":     "add_fact",
			"session_id": "explicit-session",
			"content":    "prefers terse reviews",
			"category":   "preference",
		},
	})
	if err != nil {
		t.Fatalf("add fact: %v", err)
	}

	res, err := tool.Handler(context.Background(), models.ToolCall{
		ID:   "mem-read",
		Name: "memory",
		Arguments: map[string]any{
			"action":     "read",
			"session_id": "explicit-session",
		},
	})
	if err != nil {
		t.Fatalf("read fact: %v", err)
	}
	var payload struct {
		SessionID string `json:"session_id"`
		FactCount int    `json:"fact_count"`
	}
	if err := json.Unmarshal([]byte(res.Content), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.SessionID != "explicit-session" || payload.FactCount != 1 {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	if _, err := svc.Load(context.Background(), "thread-session"); err == nil {
		t.Fatal("fact should not have been written to thread context session")
	}
}

func TestSessionSearchTool_FiltersCapsAndTruncates(t *testing.T) {
	searcher := &fakeSearcher{
		hits: []SearchHit{
			{SessionID: "s1", MessageID: "m1", Role: "human", Content: strings.Repeat("A", 450)},
			{SessionID: "s2", MessageID: "m2", Role: "human", Content: "other session"},
			{SessionID: "s1", MessageID: "m3", Role: "ai", Content: "wrong role"},
		},
	}
	tool := SessionSearchTool(searcher)
	res, err := tool.Handler(context.Background(), models.ToolCall{
		ID:   "search",
		Name: "session_search",
		Arguments: map[string]any{
			"query":      "alpha",
			"limit":      float64(999),
			"session_id": "s1",
			"role":       "human",
		},
	})
	if err != nil {
		t.Fatalf("session search: %v", err)
	}
	if searcher.limit != 250 {
		t.Fatalf("expected widened fetch limit 250 for filtered search, got %d", searcher.limit)
	}
	var payload struct {
		Limit   int         `json:"limit"`
		Total   int         `json:"total"`
		Results []SearchHit `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Content), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Limit != 50 || payload.Total != 1 {
		t.Fatalf("unexpected payload header: %+v", payload)
	}
	if len(payload.Results) != 1 || payload.Results[0].SessionID != "s1" || payload.Results[0].Role != "human" {
		t.Fatalf("unexpected filtered results: %+v", payload.Results)
	}
	if len(payload.Results[0].Content) != 400 || !strings.HasSuffix(payload.Results[0].Content, "...") {
		t.Fatalf("expected truncated snippet, got len=%d content=%q", len(payload.Results[0].Content), payload.Results[0].Content)
	}
}
