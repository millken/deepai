package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// sliceMetricsSink collects records for assertions.
type sliceMetricsSink struct {
	mu    sync.Mutex
	turns []TurnMetrics
	tools []ToolResultMetric
}

func (s *sliceMetricsSink) RecordTurn(m TurnMetrics) {
	s.mu.Lock()
	s.turns = append(s.turns, m)
	s.mu.Unlock()
}

func (s *sliceMetricsSink) RecordToolResult(m ToolResultMetric) {
	s.mu.Lock()
	s.tools = append(s.tools, m)
	s.mu.Unlock()
}

func TestComputeContextBytes_Buckets(t *testing.T) {
	msgs := []models.Message{
		{Role: models.RoleHuman, Content: "hello"}, // 5
		{
			Role:      models.RoleAI,
			Content:   "reasoning", // 9
			ToolCalls: []models.ToolCall{{ID: "c", Name: "read_file", Arguments: map[string]any{"path": "x"}}},
		},
		{Role: models.RoleTool, Content: "file contents here"}, // 18
	}
	c := computeContextBytes(msgs, "sys", 100) // system 3, schema 100

	if c.SystemBytes != 3 {
		t.Errorf("system_bytes = %d, want 3", c.SystemBytes)
	}
	if c.SchemaBytes != 100 {
		t.Errorf("schema_bytes = %d, want 100", c.SchemaBytes)
	}
	if c.HumanBytes != 5 {
		t.Errorf("human_bytes = %d, want 5", c.HumanBytes)
	}
	if c.AIContentBytes != 9 {
		t.Errorf("ai_content_bytes = %d, want 9", c.AIContentBytes)
	}
	argBytes := len(`{"path":"x"}`)
	if c.AIArgsBytes != argBytes {
		t.Errorf("ai_args_bytes = %d, want %d", c.AIArgsBytes, argBytes)
	}
	if c.ToolBytes != 18 {
		t.Errorf("tool_bytes = %d, want 18", c.ToolBytes)
	}
	wantTotal := 3 + 100 + 5 + 9 + argBytes + 18
	if c.TotalBytes != wantTotal {
		t.Errorf("total_bytes = %d, want %d", c.TotalBytes, wantTotal)
	}
}

func TestContextBytes_ToolFraction(t *testing.T) {
	c := ContextBytes{ToolBytes: 30, TotalBytes: 120}
	if got := c.ToolFraction(); got != 0.25 {
		t.Errorf("ToolFraction = %v, want 0.25", got)
	}
	if got := (ContextBytes{}).ToolFraction(); got != 0 {
		t.Errorf("empty ToolFraction = %v, want 0", got)
	}
}

// metricsProvider emits one tool call on the first turn, then ends.
type metricsProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *metricsProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *metricsProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.mu.Lock()
	turn := p.calls
	p.calls++
	p.mu.Unlock()

	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		if turn == 0 {
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{ID: "c1", Name: "echo", Arguments: map[string]any{}}},
				Usage:     &llm.Usage{InputTokens: 111, OutputTokens: 22},
				Stop:      "tool_calls",
				Done:      true,
			}
			return
		}
		ch <- llm.StreamChunk{
			Message: &models.Message{Role: models.RoleAI, Content: "done"},
			Usage:   &llm.Usage{InputTokens: 222, OutputTokens: 33},
			Done:    true,
		}
	}()
	return ch, nil
}

func TestMetrics_EndToEnd(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name: "echo",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{CallID: c.ID, ToolName: c.Name, Content: "hello result", Status: models.CallStatusCompleted}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	sink := &sliceMetricsSink{}
	a := New(AgentConfig{
		LLMProvider: &metricsProvider{},
		Tools:       reg,
		Model:       "m",
		Metrics:     sink,
	})

	_, err := a.Run(context.Background(), "sess-metrics",
		[]models.Message{{Role: models.RoleHuman, Content: "please echo"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Two turns => two per-turn records with provider tokens.
	if len(sink.turns) != 2 {
		t.Fatalf("want 2 turn records, got %d", len(sink.turns))
	}
	if sink.turns[0].Turn != 0 || sink.turns[0].InputTokens != 111 || sink.turns[0].OutputTokens != 22 {
		t.Errorf("turn 0 record wrong: %+v", sink.turns[0])
	}
	if sink.turns[1].Turn != 1 || sink.turns[1].InputTokens != 222 {
		t.Errorf("turn 1 record wrong: %+v", sink.turns[1])
	}
	// The human message bytes must show up in the byte breakdown.
	if sink.turns[0].Context.HumanBytes == 0 || sink.turns[0].Context.TotalBytes == 0 {
		t.Errorf("turn 0 context byte buckets not populated: %+v", sink.turns[0].Context)
	}

	// One tool result record with the raw size of "hello result".
	if len(sink.tools) != 1 {
		t.Fatalf("want 1 tool result record, got %d", len(sink.tools))
	}
	if sink.tools[0].ToolName != "echo" || sink.tools[0].ResultBytes != len("hello result") {
		t.Errorf("tool record wrong: %+v", sink.tools[0])
	}
	if sink.tools[0].Turn != 0 {
		t.Errorf("tool record turn = %d, want 0", sink.tools[0].Turn)
	}
}

func TestMetrics_DisabledByDefault(t *testing.T) {
	// No panic / no-op when Metrics is nil (the default). This mainly guards the
	// nil-guard at each recording site.
	clearTokenEnv(t) // isolate from ambient DEEPAI_TOKEN_* exports
	a := New(AgentConfig{LLMProvider: &captureProvider{}, Tools: tools.NewRegistry(), Model: "m"})
	if a.metrics != nil {
		t.Fatal("metrics should be nil by default")
	}
	if _, err := a.Run(context.Background(), "s", []models.Message{{Role: models.RoleHuman, Content: "hi"}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
