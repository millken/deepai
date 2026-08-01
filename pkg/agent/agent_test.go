package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/memory"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/subagent"
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

// repeatLoopProvider emits the same bash tool call every turn, simulating a
// model stuck re-running `go test` after a failed build.
type repeatLoopProvider struct {
	callCount int
}

func (p *repeatLoopProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *repeatLoopProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.callCount++
	ch := make(chan llm.StreamChunk, 1)
	// After enough turns, stop emitting tool calls so the agent terminates
	// cleanly if the breaker doesn't catch it (safety net for the test).
	if p.callCount > 20 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{Done: true, Stop: "stop"}
		}()
		return ch, nil
	}
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{
			ToolCalls: []models.ToolCall{
				{ID: "bash1", Name: "bash", Arguments: map[string]any{"command": "go test ./..."}},
			},
			Stop: "tool_calls",
			Done: true,
		}
	}()
	return ch, nil
}

// TestRepeatBreaker_StopsOnRepeatedFailedBash verifies the repeat-call circuit
// breaker terminates the run when the model re-runs the exact same bash command
// (which exits non-zero) more than maxRepeatFails times. This is the exact loop
// pattern observed in session 20260801_091600_6f08 (420 identical `go test`
// calls). The key insight: bash returns CallStatusCompleted with exit_code in
// the JSON body, so the existing validation breaker cannot catch it.
func TestRepeatBreaker_StopsOnRepeatedFailedBash(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name: "bash",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string"},
			},
			"required": []any{"command"},
		},
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			// Simulate `go test` failure: exit_code 1 but tool "completed".
			return models.ToolResult{
				CallID:   c.ID,
				ToolName: c.Name,
				Content:  `{"stdout":"FAIL\texample [build failed]\n","stderr":"undefined: fmt\n","exit_code":1}`,
			}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	a := New(AgentConfig{
		LLMProvider: &repeatLoopProvider{},
		Tools:       reg,
		MaxTurns:    50, // safety net; breaker should trip well before this
	})

	_, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "run tests"},
	})
	if err == nil {
		t.Fatal("expected the agent to stop with tool_repeat_loop error")
	}
	// The error message contains "repeated identical failed tool call".
	if !strings.Contains(err.Error(), "repeated identical failed tool call") {
		t.Fatalf("expected repeat-loop error, got: %v", err)
	}
}

// TestRepeatBreaker_HintOnRepeatedIdenticalCalls verifies that when the model
// repeats the same successful tool call, a non-fatal hint is injected at
// maxRepeatCalls but the run continues.
func TestRepeatBreaker_HintOnRepeatedIdenticalCalls(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name: "echo",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"msg": map[string]any{"type": "string"},
			},
			"required": []any{"msg"},
		},
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{CallID: c.ID, ToolName: c.Name, Content: "ok"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	provider := &repeatSuccessProvider{}
	a := New(AgentConfig{
		LLMProvider: provider,
		Tools:       reg,
		MaxTurns:    50,
	})

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "echo"},
	})
	if err != nil {
		t.Fatalf("expected no fatal error for successful repeats, got: %v", err)
	}
	// Check that a hint was injected (human message with "loop" in content).
	hintFound := false
	for _, msg := range result.Messages {
		if msg.Role == models.RoleHuman && strings.Contains(strings.ToLower(msg.Content), "loop") {
			hintFound = true
			break
		}
	}
	if !hintFound {
		t.Fatal("expected a non-fatal hint about the loop to be injected into messages")
	}
}

// repeatSuccessProvider emits the same successful tool call every turn, then
// stops after enough turns.
type repeatSuccessProvider struct {
	callCount int
}

func (p *repeatSuccessProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *repeatSuccessProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.callCount++
	ch := make(chan llm.StreamChunk, 1)
	if p.callCount > 12 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{Done: true, Stop: "stop"}
		}()
		return ch, nil
	}
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{
			ToolCalls: []models.ToolCall{
				{ID: "e1", Name: "echo", Arguments: map[string]any{"msg": "hi"}},
			},
			Stop: "tool_calls",
			Done: true,
		}
	}()
	return ch, nil
}

// TestRepeatBreaker_ResetsOnDifferentCall verifies that alternating between
// two different tool calls does NOT trigger the breaker — the model is making
// progress, not looping.
func TestRepeatBreaker_ResetsOnDifferentCall(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name: "a",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"v": map[string]any{"type": "string"}},
		},
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{CallID: c.ID, ToolName: c.Name, Content: "a-ok"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(models.Tool{
		Name: "b",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"v": map[string]any{"type": "string"}},
		},
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{CallID: c.ID, ToolName: c.Name, Content: "b-ok"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	provider := &alternatingProvider{}
	a := New(AgentConfig{
		LLMProvider: provider,
		Tools:       reg,
		MaxTurns:    50,
	})

	_, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "alternate"},
	})
	if err != nil {
		t.Fatalf("alternating calls should not trigger the breaker, got: %v", err)
	}
	if provider.callCount < 15 {
		t.Fatalf("expected at least 15 turns of alternating calls (no breaker trip), got %d", provider.callCount)
	}
}

// alternatingProvider emits tool "a" then "b" then "a" ... never repeating the
// same call twice in a row.
type alternatingProvider struct {
	callCount int
}

func (p *alternatingProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *alternatingProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.callCount++
	ch := make(chan llm.StreamChunk, 1)
	if p.callCount > 20 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{Done: true, Stop: "stop"}
		}()
		return ch, nil
	}
	name := "a"
	if p.callCount%2 == 0 {
		name = "b"
	}
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{
			ToolCalls: []models.ToolCall{
				{ID: name, Name: name, Arguments: map[string]any{"v": "x"}},
			},
			Stop: "tool_calls",
			Done: true,
		}
	}()
	return ch, nil
}

// offloadTestProvider emits one tool call then stops, so the agent terminates
// after a single tool execution.
type offloadTestProvider struct {
	done bool
}

func (p *offloadTestProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *offloadTestProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		if p.done {
			ch <- llm.StreamChunk{Done: true, Stop: "stop"}
			return
		}
		p.done = true
		ch <- llm.StreamChunk{
			ToolCalls: []models.ToolCall{
				{ID: "big1", Name: "big_tool", Arguments: map[string]any{}},
			},
			Stop: "tool_calls",
			Done: true,
		}
	}()
	return ch, nil
}

// TestOffload_LargeResultWrittenToDisk verifies that a tool result exceeding
// the offload threshold (24KB) is written to disk and replaced in-context with
// a compact reference containing first/last 50 lines.
func TestOffload_LargeResultWrittenToDisk(t *testing.T) {
	tmpDir := t.TempDir()
	offloadDir := tmpDir + "/offload"

	// Generate 30KB of content (200 lines × ~150 chars).
	var contentBuilder strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&contentBuilder, "line %03d: %s\n", i, strings.Repeat("x", 140))
	}
	largeContent := contentBuilder.String()

	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name: "big_tool",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{
				CallID:   "big1",
				ToolName: "big_tool",
				Content:  largeContent,
			}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	a := New(AgentConfig{
		LLMProvider: &offloadTestProvider{},
		Tools:       reg,
		OffloadDir:  offloadDir,
	})

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "run"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Find the tool result message.
	var toolMsg *models.Message
	for i := range result.Messages {
		if result.Messages[i].Role == models.RoleTool {
			toolMsg = &result.Messages[i]
			break
		}
	}
	if toolMsg == nil {
		t.Fatal("no tool result message found")
	}

	// Content should contain the offload reference, not the full output.
	if !strings.Contains(toolMsg.Content, "[offloaded:") {
		t.Fatalf("expected [offloaded:] marker in content, got first 200 chars: %q", toolMsg.Content[:min(200, len(toolMsg.Content))])
	}
	// Content should be much smaller than the original 30KB.
	// First+last 50 lines at ~145 chars/line ≈ 14.5KB, which is expected.
	if len(toolMsg.Content) > 20000 {
		t.Fatalf("offloaded content too large: %d bytes (expected <20KB)", len(toolMsg.Content))
	}
	// The full output should be on disk.
	entries, err := os.ReadDir(offloadDir)
	if err != nil {
		t.Fatalf("cannot read offload dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 offload file, got %d", len(entries))
	}
	// Disk file should contain the full original content.
	diskContent, err := os.ReadFile(filepath.Join(offloadDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("cannot read offload file: %v", err)
	}
	if len(diskContent) != len(largeContent) {
		t.Fatalf("disk content size %d != original %d", len(diskContent), len(largeContent))
	}
}

// TestOffload_SmallResultNotOffloaded verifies that results under the threshold
// are left untouched.
func TestOffload_SmallResultNotOffloaded(t *testing.T) {
	tmpDir := t.TempDir()
	offloadDir := tmpDir + "/offload"

	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name: "small_tool",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{
				CallID:   "s1",
				ToolName: "small_tool",
				Content:  "small result",
			}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Provider that emits a small_tool call.
	provider := &offloadTestProvider{}
	// Override: emit small_tool instead of big_tool.
	a := New(AgentConfig{
		LLMProvider: provider,
		Tools:       reg,
		OffloadDir:  offloadDir,
	})
	// We need the provider to call "small_tool". Since offloadTestProvider is
	// hardcoded to "big_tool", register big_tool name but with small handler.
	// Simpler: just test offloadIfNeeded directly.

	_ = a // keep linter happy

	// Direct unit test of the offload logic.
	result := models.ToolResult{
		CallID:   "test1",
		ToolName: "small_tool",
		Content:  "small result",
	}
	offloaded := a.offloadIfNeeded(&result, offloadDir)
	if offloaded {
		t.Fatal("small result should not be offloaded")
	}
	if result.Content != "small result" {
		t.Fatalf("content modified unexpectedly: %q", result.Content)
	}
	// No file should exist.
	if entries, _ := os.ReadDir(offloadDir); len(entries) > 0 {
		t.Fatalf("expected no offload files, got %d", len(entries))
	}
}

// TestOffload_DisabledWhenDirEmpty verifies that offload is a no-op when
// OffloadDir is empty (offload disabled).
func TestOffload_DisabledWhenDirEmpty(t *testing.T) {
	// Create agent with empty offload dir by setting it explicitly.
	a := New(AgentConfig{
		LLMProvider: &offloadTestProvider{},
		Tools:       tools.NewRegistry(),
		OffloadDir:  "", // explicitly disabled
	})
	// Override the auto-derived dir.
	a.offloadDir = ""

	largeContent := strings.Repeat("x", 30000)
	result := models.ToolResult{
		CallID:   "test2",
		ToolName: "big_tool",
		Content:  largeContent,
	}
	offloaded := a.offloadIfNeeded(&result, a.offloadDir)
	if offloaded {
		t.Fatal("offload should be disabled when dir is empty")
	}
	if result.Content != largeContent {
		t.Fatal("content should be unchanged when offload is disabled")
	}
}

func TestMergeToolCalls_EmptyIDsDoNotCollide(t *testing.T) {
	// Two parallel tool calls streamed across chunks with empty IDs must remain
	// distinct, not overwrite each other via the empty-string map key.
	out := mergeToolCalls(
		[]models.ToolCall{{ID: "", Name: "bash", Arguments: map[string]any{"cmd": "a"}}},
		[]models.ToolCall{{ID: "", Name: "read_file", Arguments: map[string]any{"path": "b"}}},
	)
	if len(out) != 2 {
		t.Fatalf("empty-ID calls collided: got %d, want 2", len(out))
	}
	// Non-empty IDs still merge.
	merged := mergeToolCalls(
		[]models.ToolCall{{ID: "x", Name: "bash"}},
		[]models.ToolCall{{ID: "x", Arguments: map[string]any{"cmd": "ls"}}},
	)
	if len(merged) != 1 || len(merged[0].Arguments) == 0 {
		t.Fatalf("same-ID calls should merge: %+v", merged)
	}
}

type modelCaptureProvider struct {
	mu    sync.Mutex
	model string
}

func (p *modelCaptureProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *modelCaptureProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.mu.Lock()
	p.model = req.Model
	p.mu.Unlock()
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, Content: "done"}, Done: true}
	}()
	return ch, nil
}

func (p *modelCaptureProvider) seen() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.model
}

func TestSubagentExecutor_UsesConfiguredModel(t *testing.T) {
	p := &modelCaptureProvider{}
	reg := llm.NewSingleModelRegistry("test", "configured-model", "")
	reg.InjectProvider("test", "", "", p)

	exec := NewSubagentExecutor(reg, tools.NewRegistry(), nil)
	_, err := exec.Execute(context.Background(),
		&subagent.Task{ID: "t1", Prompt: "hi", Config: subagent.SubagentConfig{AgentType: "general-purpose"}},
		func(subagent.TaskEvent) {})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := p.seen(); got != "configured-model" {
		t.Fatalf("subagent used model %q, want configured-model (the /build 模型不存在 bug)", got)
	}

	// Per-task model override wins.
	p2 := &modelCaptureProvider{}
	regOverride, err := llm.NewModelRegistry([]llm.ModelDef{
		{Name: "default", Provider: "test", Model: "configured-model"},
		{Name: "review", Provider: "test", Model: "review-model"},
	}, "default")
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	regOverride.InjectProvider("test", "", "", p2)

	exec2 := NewSubagentExecutor(regOverride, tools.NewRegistry(), nil)
	_, err = exec2.Execute(context.Background(),
		&subagent.Task{ID: "t2", Prompt: "hi", Config: subagent.SubagentConfig{AgentType: "general-purpose", Model: "review"}},
		func(subagent.TaskEvent) {})
	if err != nil {
		t.Fatalf("Execute (override): %v", err)
	}
	if got := p2.seen(); got != "review-model" {
		t.Fatalf("per-task model override = %q, want review-model", got)
	}
}

func TestNonInteractiveAgentHasNoPlanMode(t *testing.T) {
	reg := tools.NewRegistry()
	interactive := New(AgentConfig{LLMProvider: &modelCaptureProvider{}, Tools: reg, Model: "m"})
	if interactive.tools.Get("enter_plan_mode") == nil {
		t.Fatal("interactive agent should register enter_plan_mode")
	}

	reg2 := tools.NewRegistry()
	sub := New(AgentConfig{LLMProvider: &modelCaptureProvider{}, Tools: reg2, Model: "m", NonInteractive: true})
	if sub.tools.Get("enter_plan_mode") != nil {
		t.Fatal("non-interactive subagent must NOT have enter_plan_mode (causes 15m plan-mode stalls)")
	}
}
