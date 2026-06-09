package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/memory"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

func TestAgentConfig_DefaultMaxTurns(t *testing.T) {
	cfg := AgentConfig{}
	agent := New(cfg)
	// 0 = unlimited (no hard turn cap)
	if agent.maxTurns != 0 {
		t.Errorf("Expected default MaxTurns=0 (unlimited), got %d", agent.maxTurns)
	}
}

func TestAgentConfig_CustomMaxTurns(t *testing.T) {
	cfg := AgentConfig{
		MaxTurns: 20,
	}
	agent := New(cfg)
	if agent.maxTurns != 20 {
		t.Errorf("Expected MaxTurns=20, got %d", agent.maxTurns)
	}
}

func TestAgentConfig_TokenBudget(t *testing.T) {
	cfg := AgentConfig{
		MaxTokensBudget: 50000,
	}
	agent := New(cfg)
	if agent.maxTokensBudget != 50000 {
		t.Errorf("Expected MaxTokensBudget=50000, got %d", agent.maxTokensBudget)
	}
}

func TestAgent_Events(t *testing.T) {
	cfg := AgentConfig{}
	agent := New(cfg)
	events := agent.Events()

	if events == nil {
		t.Error("Events channel should not be nil")
	}
}

func TestAgent_Run_NoLLMProvider(t *testing.T) {
	cfg := AgentConfig{}
	agent := New(cfg)
	_, err := agent.Run(context.Background(), "session_1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "Hello"},
	})

	if err == nil {
		t.Error("Expected error when LLM provider is nil")
	}
}

func TestAgent_New_WithTools(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(models.Tool{
		Name:        "test",
		Description: "Test tool",
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{}, nil
		},
	})

	cfg := AgentConfig{
		Tools: registry,
	}

	agent := New(cfg)

	if agent.tools != registry {
		t.Error("Tools registry not set correctly")
	}
}

func TestAgent_BuildSystemPrompt(t *testing.T) {
	cfg := AgentConfig{
		SystemPrompt: "custom system prompt",
	}

	agent := New(cfg)
	ctx := context.Background()

	prompt := agent.BuildSystemPrompt(ctx, "test_session", nil)

	if prompt == "" {
		t.Error("System prompt should not be empty")
	}
	if prompt == "custom system prompt" {
		t.Error("BuildSystemPrompt should include runtime instructions in addition to the base prompt")
	}
}

func TestAgent_emit(t *testing.T) {
	cfg := AgentConfig{}
	agent := New(cfg)

	agent.emit(AgentEvent{
		Type:      AgentEventError,
		SessionID: "test_session",
		Err:       "test error",
	})
}

func TestResolveModel(t *testing.T) {
	os.Unsetenv("DEFAULT_LLM_MODEL")

	tests := []struct {
		input    string
		expected string
	}{
		{"gpt-4", "gpt-4"},
		{"claude-3-opus", "claude-3-opus"},
		{"", "gpt-4.1-mini"},
		{"qwen/qwen3-9b", "qwen/qwen3-9b"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := resolveModel(tt.input)
			if result != tt.expected {
				t.Errorf("resolveModel(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestUsage(t *testing.T) {
	usage := Usage{
		InputTokens:  100,
		OutputTokens: 50,
		TotalTokens:  150,
	}

	if usage.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", usage.InputTokens)
	}
	if usage.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50", usage.OutputTokens)
	}
	if usage.TotalTokens != 150 {
		t.Errorf("TotalTokens = %d, want 150", usage.TotalTokens)
	}
}

func TestRunResult(t *testing.T) {
	result := RunResult{
		Messages: []models.Message{
			{ID: "m1", SessionID: "test_session", Role: models.RoleAI, Content: "Hello"},
		},
		FinalOutput: "Hello",
		Usage: &Usage{
			InputTokens:  10,
			OutputTokens: 5,
			TotalTokens:  15,
		},
	}

	if len(result.Messages) != 1 {
		t.Errorf("Messages count = %d, want 1", len(result.Messages))
	}
	if result.FinalOutput != "Hello" {
		t.Errorf("FinalOutput = %s, want 'Hello'", result.FinalOutput)
	}
}

func TestAgentRunUsesRequestTimeout(t *testing.T) {
	runAgent := New(AgentConfig{
		LLMProvider:    timeoutProvider{},
		RequestTimeout: 20 * time.Millisecond,
	})

	_, err := runAgent.Run(context.Background(), "session_1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "Hello"},
	})
	if err == nil {
		t.Fatal("Run() error = nil, want timeout")
	}

	var timeoutErr *TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("Run() error = %T, want *TimeoutError", err)
	}
}

func TestNormalizeRunError_DoesNotMaskInnerDeadlineExceeded(t *testing.T) {
	err := normalizeRunError(context.Background(), context.DeadlineExceeded, time.Hour)

	var timeoutErr *TimeoutError
	if errors.As(err, &timeoutErr) {
		t.Fatalf("normalizeRunError() unexpectedly wrapped inner deadline as TimeoutError: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("normalizeRunError() = %v, want context.DeadlineExceeded", err)
	}
}

func TestNormalizeRunError_WrapsRunContextDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	<-ctx.Done()

	err := normalizeRunError(ctx, ctx.Err(), time.Hour)
	var timeoutErr *TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("normalizeRunError() error = %T, want *TimeoutError", err)
	}
}

func TestApplyAgentType(t *testing.T) {
	registry := tools.NewRegistry()
	_ = registry.Register(models.Tool{Name: "bash", Handler: func(context.Context, models.ToolCall) (models.ToolResult, error) { return models.ToolResult{}, nil }})
	_ = registry.Register(models.Tool{Name: "read_file", Handler: func(context.Context, models.ToolCall) (models.ToolResult, error) { return models.ToolResult{}, nil }})
	_ = registry.Register(models.Tool{Name: "write_file", Handler: func(context.Context, models.ToolCall) (models.ToolResult, error) { return models.ToolResult{}, nil }})
	_ = registry.Register(models.Tool{Name: "ask_clarification", Handler: func(context.Context, models.ToolCall) (models.ToolResult, error) { return models.ToolResult{}, nil }})

	cfg := AgentConfig{
		Tools:     registry,
		AgentType: AgentTypeCoder,
	}
	if err := ApplyAgentType(&cfg, cfg.AgentType); err != nil {
		t.Fatalf("ApplyAgentType() error = %v", err)
	}
	if cfg.SystemPrompt == "" {
		t.Fatal("ApplyAgentType() did not set system prompt")
	}
	if cfg.Temperature == nil {
		t.Fatal("ApplyAgentType() did not set temperature")
	}
	if cfg.Tools.Get("bash") == nil {
		t.Fatal("ApplyAgentType() removed allowed tool bash")
	}
	if cfg.Tools.Get("read_file") == nil {
		t.Fatal("ApplyAgentType() removed allowed tool read_file")
	}
}

type timeoutProvider struct{}

func (timeoutProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (timeoutProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		<-ctx.Done()
		ch <- llm.StreamChunk{Err: ctx.Err(), Done: true}
	}()
	return ch, nil
}

// TestBuildSystemPrompt_InjectsProjectMemory guards the fix for the CLI bug
// where the per-turn agent was built without a MemoryService, so stored facts
// were never injected. With a MemoryService and a project (UserScope) memoryUserID
// set, a relevant stored fact must appear in the built system prompt.
func TestBuildSystemPrompt_InjectsProjectMemory(t *testing.T) {
	ctx := context.Background()
	store, err := memory.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	svc := memory.NewService(slog.Default(), store, nil)
	defer svc.Close(ctx)

	uid := "/projects/zephyr"
	now := time.Now().UTC()
	doc := memory.Document{
		SessionID: memory.UserScope(uid).Key(),
		Facts: []memory.Fact{{
			ID:         "f1",
			Content:    "Project zephyr deploys via the deploy-zephyr script",
			Confidence: 0.9,
			CreatedAt:  now,
			UpdatedAt:  now,
		}},
		UpdatedAt: now,
	}
	if err := svc.Save(ctx, doc); err != nil {
		t.Fatalf("save memory: %v", err)
	}

	a := New(AgentConfig{
		SystemPrompt:  "base",
		MemoryService: svc,
		MemoryUserID:  uid,
	})

	msgs := []models.Message{{Role: models.RoleHuman, Content: "how do I deploy zephyr?"}}
	prompt := a.BuildSystemPrompt(ctx, "session-xyz", msgs)

	if !strings.Contains(prompt, "deploy-zephyr script") {
		t.Fatalf("project memory not injected into system prompt:\n%s", prompt)
	}

	bare := New(AgentConfig{SystemPrompt: "base"})
	if got := bare.BuildSystemPrompt(ctx, "session-xyz", msgs); strings.Contains(got, "deploy-zephyr script") {
		t.Fatalf("bare agent should not inject memory, but did:\n%s", got)
	}
}

type validationLoopProvider struct{}

func (validationLoopProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (validationLoopProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{
			ToolCalls: []models.ToolCall{
				{ID: "bad", Name: "needs_arg", Arguments: map[string]any{}},
				{ID: "ok", Name: "freebie", Arguments: map[string]any{}},
			},
			Stop: "tool_calls",
			Done: true,
		}
	}()
	return ch, nil
}

// TestValidationBreaker_TripsDespiteMixedBatch guards M4: a batch that always
// contains one passing tool must not keep resetting the global consecutive-
// validation-failure counter. The breaker must still trip within a bounded
// number of turns rather than looping until MaxTurns.
func TestValidationBreaker_TripsDespiteMixedBatch(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name: "needs_arg",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"x": map[string]any{"type": "string"}},
			"required":   []any{"x"},
		},
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(models.Tool{
		Name: "freebie",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{Content: "ok"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	a := New(AgentConfig{
		LLMProvider: validationLoopProvider{},
		Tools:       reg,
		MaxTurns:    50,
	})

	_, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "go"},
	})
	if err == nil {
		t.Fatal("expected the agent to stop with an error")
	}
	if !strings.Contains(err.Error(), "consecutive tool argument validation failures") {
		t.Fatalf("breaker did not trip (got %v); a mixed batch is still resetting the global counter", err)
	}
}
