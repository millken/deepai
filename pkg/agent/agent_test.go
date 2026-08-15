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

func TestAgentConfig_DefaultMaxToolCalls(t *testing.T) {
	cfg := AgentConfig{}
	agent := New(cfg)
	// 0 = unlimited (no hard turn cap)
	if agent.maxToolCalls != 0 {
		t.Errorf("Expected default MaxToolCalls=0 (unlimited), got %d", agent.maxToolCalls)
	}
}

func TestAgentConfig_CustomMaxToolCalls(t *testing.T) {
	cfg := AgentConfig{
		MaxToolCalls: 20,
	}
	agent := New(cfg)
	if agent.maxToolCalls != 20 {
		t.Errorf("Expected MaxToolCalls=20, got %d", agent.maxToolCalls)
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

	// New() clones the registry for interactive agents (see P0-1 fix), so
	// agent.tools is no longer the same pointer as the caller's registry —
	// verify the tool made it into the agent's registry instead.
	if agent.tools.Get("test") == nil {
		t.Error("Tools registry not set correctly")
	}
}

func TestAgent_BuildSystemPrompt(t *testing.T) {
	cfg := AgentConfig{
		SystemPrompt: "custom system prompt",
	}

	agent := New(cfg)

	prompt := agent.BuildSystemPrompt()

	if prompt == "" {
		t.Error("System prompt should not be empty")
	}
	// M4-2 CONTRACT CHANGE: this used to assert prompt != the bare base,
	// because construction always baked "Today's date is X" onto the
	// base — the ONLY reason a tool-less, catalog-less agent
	// (like this one) ever differed from its raw base string. The date has
	// moved to the per-Run trailing turn injection (see buildTurnInjection /
	// appendTurnInjection) precisely so the system prompt stays byte-stable
	// across a session for automatic prefix caching — so for an agent with
	// no file tools, no search tools, no delegation catalog, and no plan
	// mode, BuildSystemPrompt legitimately now returns exactly the base,
	// unchanged. The "runtime instructions get appended when applicable"
	// behavior itself is still real and covered by the T5c/TeamDelegation
	// tests in systemprompt_test.go (file-op rule, tool recommendations,
	// delegation prompt) — this test now just pins the "no extras" floor for
	// a minimal agent instead of asserting a difference that no longer has a
	// source.
	if prompt != "custom system prompt" {
		t.Errorf("BuildSystemPrompt() for a tool-less, catalog-less agent = %q, want exactly the base (no runtime instructions apply)", prompt)
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

// TestApplyAgentType_NoDeclaredTypeKeepsFullToolset is the RED test for the
// blast radius of giving general-purpose an explicit DefaultTools allowlist: the
// REPL builds its AgentConfig WITHOUT an AgentType (pkg/chat/repl.go), so
// ApplyAgentType normalizes the empty type to general-purpose. It must keep
// using that profile as the baseline prompt/temperature — the REPL relies on
// both — while leaving the tool registry ALONE. Restricting it would silently
// strip the main agent's task tool, skill tool and every MCP tool, none of which
// any agent-type allowlist can name.
func TestApplyAgentType_NoDeclaredTypeKeepsFullToolset(t *testing.T) {
	registry := tools.NewRegistry()
	// task/skill/some_mcp_tool are exactly the tools no profile allowlist names.
	for _, name := range []string{"read_file", "bash", "task", "skill", "some_mcp_tool", "git_auto_commit"} {
		if err := registry.Register(models.Tool{Name: name, Handler: func(context.Context, models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{}, nil
		}}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	cfg := AgentConfig{Tools: registry} // no AgentType — the REPL's shape
	if err := ApplyAgentType(&cfg, cfg.AgentType); err != nil {
		t.Fatalf("ApplyAgentType() error = %v", err)
	}

	if cfg.AgentType != AgentTypeGeneral {
		t.Fatalf("AgentType = %q, want the general-purpose baseline", cfg.AgentType)
	}
	if cfg.SystemPrompt == "" {
		t.Fatal("ApplyAgentType() did not set the baseline system prompt")
	}
	if cfg.Temperature == nil {
		t.Fatal("ApplyAgentType() did not set the baseline temperature")
	}
	for _, name := range []string{"read_file", "bash", "task", "skill", "some_mcp_tool", "git_auto_commit"} {
		if cfg.Tools.Get(name) == nil {
			t.Fatalf("ApplyAgentType() removed %q from an agent that declared no type", name)
		}
	}
}

// TestApplyAgentType_DeclaredTypeStillRestricts is the companion guard: an
// EXPLICIT agent type must still narrow the registry to its allowlist.
func TestApplyAgentType_DeclaredTypeStillRestricts(t *testing.T) {
	registry := tools.NewRegistry()
	for _, name := range []string{"bash", "read_file", "some_mcp_tool"} {
		if err := registry.Register(models.Tool{Name: name, Handler: func(context.Context, models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{}, nil
		}}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	cfg := AgentConfig{Tools: registry, AgentType: AgentTypeBash}
	if err := ApplyAgentType(&cfg, cfg.AgentType); err != nil {
		t.Fatalf("ApplyAgentType() error = %v", err)
	}
	if cfg.Tools.Get("bash") == nil {
		t.Fatal("ApplyAgentType() removed the bash profile's own tool")
	}
	for _, name := range []string{"read_file", "some_mcp_tool"} {
		if cfg.Tools.Get(name) != nil {
			t.Fatalf("ApplyAgentType() kept %q, which the bash allowlist does not name", name)
		}
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
// set, a relevant stored fact must appear... — but M4-2 CONTRACT CHANGE:
// no longer in the built SYSTEM PROMPT. Baking per-request memory into the
// system prompt (position 0 of every request) defeated automatic prefix
// caching on every OpenAI-compat provider, since the injected content varies
// with the retrieval relevance context on every turn. Project-scoped memory
// injection itself still works exactly as before; it just now rides the
// per-Run trailing turn injection (buildTurnInjection) instead of
// BuildSystemPrompt. This test now pins BOTH halves of that contract: the
// system prompt must NOT contain the fact, and buildTurnInjection must.
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
	prompt := a.BuildSystemPrompt()
	if strings.Contains(prompt, "deploy-zephyr script") {
		t.Fatalf("project memory must NOT be injected into the system prompt (breaks prefix caching):\n%s", prompt)
	}

	injection := a.buildTurnInjection(ctx, "session-xyz", msgs)
	if !strings.Contains(injection.Content, "deploy-zephyr script") {
		t.Fatalf("project memory not injected into the turn injection:\n%s", injection.Content)
	}

	bare := New(AgentConfig{SystemPrompt: "base"})
	if got := bare.buildTurnInjection(ctx, "session-xyz", msgs); strings.Contains(got.Content, "deploy-zephyr script") {
		t.Fatalf("bare agent (no MemoryService) should not inject memory, but did:\n%s", got.Content)
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
// number of turns rather than looping until MaxToolCalls.
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
		LLMProvider:  validationLoopProvider{},
		Tools:        reg,
		MaxToolCalls: 50,
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
		LLMProvider:  &repeatLoopProvider{},
		Tools:        reg,
		MaxToolCalls: 50, // safety net; breaker should trip well before this
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
		LLMProvider:  provider,
		Tools:        reg,
		MaxToolCalls: 50,
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
		LLMProvider:  provider,
		Tools:        reg,
		MaxToolCalls: 50,
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

// parallelRepeatLoopProvider emits a batch of 2 identical ParallelSafe tool
// calls (same name+args, distinct CallIDs) every turn, simulating a model
// stuck re-running a failing parallel-safe tool in lockstep pairs.
type parallelRepeatLoopProvider struct {
	callCount int
}

func (p *parallelRepeatLoopProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *parallelRepeatLoopProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.callCount++
	ch := make(chan llm.StreamChunk, 1)
	// Safety net set well beyond MaxToolCalls (50 in the test): with the breaker
	// absent the run must hit the MaxToolCalls cap first, proving the exact
	// failure signature the fix eliminates (max-turns error, not
	// tool_repeat_loop). With the breaker present it trips within ~4 turns.
	if p.callCount > 200 {
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
				{ID: fmt.Sprintf("pfail-%d-0", p.callCount), Name: "pfail", Arguments: map[string]any{"x": "y"}},
				{ID: fmt.Sprintf("pfail-%d-1", p.callCount), Name: "pfail", Arguments: map[string]any{"x": "y"}},
			},
			Stop: "tool_calls",
			Done: true,
		}
	}()
	return ch, nil
}

// TestRepeatBreaker_StopsOnRepeatedFailedParallelBatch guards the parallel
// tool-execution path (all calls ParallelSafe, batch len > 1): the repeat-call
// circuit breaker must trip there exactly as it does on the serial path.
// Without the fix, the parallel path never touches repeatCalls/repeatFails,
// so the run exhausts MaxToolCalls instead of stopping on tool_repeat_loop.
func TestRepeatBreaker_StopsOnRepeatedFailedParallelBatch(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name:         "pfail",
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"x": map[string]any{"type": "string"}},
		},
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{
				CallID:   c.ID,
				ToolName: c.Name,
				Status:   models.CallStatusFailed,
				Error:    "boom",
			}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	a := New(AgentConfig{
		LLMProvider:  &parallelRepeatLoopProvider{},
		Tools:        reg,
		MaxToolCalls: 50, // safety net; breaker should trip well before this
	})

	_, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "run parallel"},
	})
	if err == nil {
		t.Fatal("expected the agent to stop with an error")
	}
	if !strings.Contains(err.Error(), "repeated identical failed tool call") {
		t.Fatalf("expected repeat-loop error (parallel path breaker absent), got: %v", err)
	}
}

// parallelRepeatLoopProvider3 emits a batch of 3 identical ParallelSafe
// failing tool calls every turn, forever. maxRepeatFails is 8: turn 1 leaves
// repeatFails at 3, turn 2 at 6, and turn 3's batch trips fatal on its SECOND
// call (repeatFails hits 8 at batch index 1 of 3) — one call into the batch
// still remains uncomputed-but-already-run when the fatal return happens,
// reproducing the mid-batch drop.
type parallelRepeatLoopProvider3 struct {
	callCount int
}

func (p *parallelRepeatLoopProvider3) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *parallelRepeatLoopProvider3) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.callCount++
	ch := make(chan llm.StreamChunk, 1)
	// Safety net well beyond the turn at which the breaker must trip (3).
	if p.callCount > 20 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{Done: true, Stop: "stop"}
		}()
		return ch, nil
	}
	turn := p.callCount
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{
			ToolCalls: []models.ToolCall{
				{ID: fmt.Sprintf("pfail3-%d-0", turn), Name: "pfail3", Arguments: map[string]any{"x": "y"}},
				{ID: fmt.Sprintf("pfail3-%d-1", turn), Name: "pfail3", Arguments: map[string]any{"x": "y"}},
				{ID: fmt.Sprintf("pfail3-%d-2", turn), Name: "pfail3", Arguments: map[string]any{"x": "y"}},
			},
			Stop: "tool_calls",
			Done: true,
		}
	}()
	return ch, nil
}

// TestRepeatBreaker_FatalMidBatchKeepsAllToolResults guards against dropping
// tool results that were already computed (by the parallel path's goroutines,
// which run the whole batch up front) but not yet appended to runMessages
// when the repeat-call breaker trips fatally on an earlier result in the same
// batch. Every ToolCall ID on the final assistant message (RoleAI,
// len(ToolCalls) == 3) must have a matching RoleTool message in the returned
// Messages — Anthropic (and the REPL, which persists Messages even on error)
// require a tool_result for every tool_use block in the preceding assistant
// turn, or the session is permanently unusable on the next request.
func TestRepeatBreaker_FatalMidBatchKeepsAllToolResults(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name:         "pfail3",
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"x": map[string]any{"type": "string"}},
		},
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{
				CallID:   c.ID,
				ToolName: c.Name,
				Status:   models.CallStatusFailed,
				Error:    "boom",
				// Every call reports usage so the M1 assertion below can
				// verify ALL of them (including the mid-batch-fatal tail
				// append) rolled into RunResult.Usage, not just the ones
				// processed by the normal per-index loop.
				Data: map[string]any{"subagent_usage": &subagent.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}},
			}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	a := New(AgentConfig{
		LLMProvider:  &parallelRepeatLoopProvider3{},
		Tools:        reg,
		MaxToolCalls: 50, // safety net; breaker should trip on turn 3
	})

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "run parallel"},
	})
	if err == nil {
		t.Fatal("expected the agent to stop with a fatal repeat-loop error")
	}
	if !strings.Contains(err.Error(), "repeated identical failed tool call") {
		t.Fatalf("expected repeat-loop error, got: %v", err)
	}
	if result == nil {
		t.Fatal("expected a non-nil RunResult even on fatal breaker error")
	}

	// Find the final assistant (RoleAI) message carrying tool calls.
	var lastAI *models.Message
	for i := range result.Messages {
		if result.Messages[i].Role == models.RoleAI && len(result.Messages[i].ToolCalls) > 0 {
			lastAI = &result.Messages[i]
		}
	}
	if lastAI == nil {
		t.Fatal("expected an assistant message with tool calls in the run result")
	}
	if len(lastAI.ToolCalls) != 3 {
		t.Fatalf("expected the final assistant message to carry 3 tool calls, got %d", len(lastAI.ToolCalls))
	}

	toolResultIDs := make(map[string]bool)
	for _, msg := range result.Messages {
		if msg.Role == models.RoleTool && msg.ToolResult != nil {
			toolResultIDs[msg.ToolResult.CallID] = true
		}
	}
	for _, call := range lastAI.ToolCalls {
		if !toolResultIDs[call.ID] {
			t.Errorf("tool_use ID %q from the final assistant message has no matching tool_result in runMessages "+
				"(a batch with a mid-batch fatal breaker trip must not drop already-computed trailing results)", call.ID)
		}
	}

	// M1: the mid-batch-fatal tail-append loop (which appends already-computed
	// trailing results to runMessages after a fatal breaker trip) must also
	// roll each one's usage into RunResult.Usage, not just the results
	// processed by the normal per-index loop before the trip. Count actual
	// pfail3 tool_results rather than hardcoding a turn/call count, so this
	// holds regardless of exactly which turn/index the breaker trips on.
	pfail3Count := 0
	for _, msg := range result.Messages {
		if msg.Role == models.RoleTool && msg.ToolResult != nil && msg.ToolResult.ToolName == "pfail3" {
			pfail3Count++
		}
	}
	if pfail3Count == 0 {
		t.Fatal("expected at least one pfail3 tool result")
	}
	wantTotal := pfail3Count * 15
	if result.Usage == nil || result.Usage.TotalTokens != wantTotal {
		t.Fatalf("RunResult.Usage.TotalTokens = %v, want %d (usage from all %d pfail3 results, including the mid-batch-fatal tail append)", result.Usage, wantTotal, pfail3Count)
	}
}

// serialRepeatLoopProvider3 emits 7 turns of a single failing, non-parallel-
// safe tool call (bringing the repeat-fail counter to maxRepeatFails-1 = 7),
// then one turn with a batch of 3 identical calls. The 8th consecutive
// identical failure is the FIRST call of that batch, so the fatal
// repeat-loop trip happens on batch index 0 — the serial loop never reaches
// (never executes) the batch's remaining two calls.
type serialRepeatLoopProvider3 struct {
	callCount int
}

func (p *serialRepeatLoopProvider3) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *serialRepeatLoopProvider3) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.callCount++
	ch := make(chan llm.StreamChunk, 1)
	// Safety net well beyond the turn at which the breaker must trip (8).
	if p.callCount > 20 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{Done: true, Stop: "stop"}
		}()
		return ch, nil
	}
	turn := p.callCount
	var calls []models.ToolCall
	if turn <= 7 {
		calls = []models.ToolCall{
			{ID: fmt.Sprintf("sfail-%d-0", turn), Name: "sfail", Arguments: map[string]any{"x": "y"}},
		}
	} else {
		calls = []models.ToolCall{
			{ID: fmt.Sprintf("sfail-%d-0", turn), Name: "sfail", Arguments: map[string]any{"x": "y"}},
			{ID: fmt.Sprintf("sfail-%d-1", turn), Name: "sfail", Arguments: map[string]any{"x": "y"}},
			{ID: fmt.Sprintf("sfail-%d-2", turn), Name: "sfail", Arguments: map[string]any{"x": "y"}},
		}
	}
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{ToolCalls: calls, Stop: "tool_calls", Done: true}
	}()
	return ch, nil
}

// TestSerialBreaker_FatalMidBatchSynthesizesRemainingResults is the RED test
// for the serial-path analogue of M1's parallel mid-batch-fatal fix: unlike
// the parallel path (whose whole batch is already computed by goroutines
// before the fatal observation loop runs, so the remaining results just need
// appending), the serial loop executes one call at a time and genuinely
// never runs the calls after the one that trips the breaker. Those tool_use
// IDs must still get a synthesized failed tool_result, or the assistant
// message that started this batch carries tool_use IDs with no matching
// tool_result — poisoning the session for the next Anthropic request.
func TestSerialBreaker_FatalMidBatchSynthesizesRemainingResults(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name: "sfail",
		// Deliberately NOT ParallelSafe: forces the serial dispatch path even
		// for a batch of 3.
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"x": map[string]any{"type": "string"}},
		},
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{
				CallID:   c.ID,
				ToolName: c.Name,
				Status:   models.CallStatusFailed,
				Error:    "boom",
			}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	a := New(AgentConfig{
		LLMProvider:  &serialRepeatLoopProvider3{},
		Tools:        reg,
		MaxToolCalls: 50, // safety net; breaker should trip on turn 8
	})

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "run serial"},
	})
	if err == nil {
		t.Fatal("expected the agent to stop with a fatal repeat-loop error")
	}
	if !strings.Contains(err.Error(), "repeated identical failed tool call") {
		t.Fatalf("expected repeat-loop error, got: %v", err)
	}
	if result == nil {
		t.Fatal("expected a non-nil RunResult even on fatal breaker error")
	}

	// Find the final assistant (RoleAI) message carrying tool calls — the
	// batch of 3 from turn 8.
	var lastAI *models.Message
	for i := range result.Messages {
		if result.Messages[i].Role == models.RoleAI && len(result.Messages[i].ToolCalls) > 0 {
			lastAI = &result.Messages[i]
		}
	}
	if lastAI == nil {
		t.Fatal("expected an assistant message with tool calls in the run result")
	}
	if len(lastAI.ToolCalls) != 3 {
		t.Fatalf("expected the final assistant message to carry 3 tool calls, got %d", len(lastAI.ToolCalls))
	}

	toolResultIDs := make(map[string]bool)
	for _, msg := range result.Messages {
		if msg.Role == models.RoleTool && msg.ToolResult != nil {
			toolResultIDs[msg.ToolResult.CallID] = true
		}
	}
	for _, call := range lastAI.ToolCalls {
		if !toolResultIDs[call.ID] {
			t.Errorf("tool_use ID %q from the final assistant message has no matching tool_result in runMessages "+
				"(serial mid-batch fatal trip must synthesize failed results for calls the loop never executed)", call.ID)
		}
	}
}

// parallelRepeatLoopProviderOffload emits 7 turns of a single failing,
// ParallelSafe tool call, then one turn with a batch of 3 identical calls
// (each returning content above the offload threshold). The 8th consecutive
// identical failure lands on batch index 0, so the fatal trip's tail-append
// loop (results[1], results[2] — already computed by the parallel path's
// goroutines) is what gets exercised.
type parallelRepeatLoopProviderOffload struct {
	callCount int
}

func (p *parallelRepeatLoopProviderOffload) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *parallelRepeatLoopProviderOffload) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.callCount++
	ch := make(chan llm.StreamChunk, 1)
	if p.callCount > 20 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{Done: true, Stop: "stop"}
		}()
		return ch, nil
	}
	turn := p.callCount
	var calls []models.ToolCall
	if turn <= 7 {
		calls = []models.ToolCall{
			{ID: fmt.Sprintf("poff-%d-0", turn), Name: "poff", Arguments: map[string]any{"x": "y"}},
		}
	} else {
		calls = []models.ToolCall{
			{ID: fmt.Sprintf("poff-%d-0", turn), Name: "poff", Arguments: map[string]any{"x": "y"}},
			{ID: fmt.Sprintf("poff-%d-1", turn), Name: "poff", Arguments: map[string]any{"x": "y"}},
			{ID: fmt.Sprintf("poff-%d-2", turn), Name: "poff", Arguments: map[string]any{"x": "y"}},
		}
	}
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{ToolCalls: calls, Stop: "tool_calls", Done: true}
	}()
	return ch, nil
}

// TestParallelBreaker_FatalTailAppendAppliesOffload is the RED test for the
// M1 gap in the parallel path's fatal tail-append loop: it appends
// results[j] straight to runMessages without running it through
// a.offloadIfNeeded first (the normal per-index loop above it does apply
// offload before appending). A large trailing result that never goes
// through the normal path must still get shrunk to the offload stub, not
// persisted at full size.
func TestParallelBreaker_FatalTailAppendAppliesOffload(t *testing.T) {
	// Many short lines (not one giant line) so buildOffloadedContent's
	// head+tail truncation actually shrinks the content, rather than just
	// wrapping a single line in a header that's larger than the original.
	bigContent := strings.Repeat("line of filler text\n", (offloadThresholdBytes/20)+200)
	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name:         "poff",
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"x": map[string]any{"type": "string"}},
		},
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{
				CallID:   c.ID,
				ToolName: c.Name,
				Status:   models.CallStatusFailed,
				Error:    "boom",
				Content:  bigContent,
			}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	a := New(AgentConfig{
		LLMProvider:  &parallelRepeatLoopProviderOffload{},
		Tools:        reg,
		MaxToolCalls: 50, // safety net; breaker should trip on turn 8
		OffloadDir:   t.TempDir(),
	})

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "run parallel"},
	})
	if err == nil {
		t.Fatal("expected the agent to stop with a fatal repeat-loop error")
	}
	if result == nil {
		t.Fatal("expected a non-nil RunResult even on fatal breaker error")
	}

	foundShrunk := false
	for _, msg := range result.Messages {
		if msg.Role != models.RoleTool || msg.ToolResult == nil || msg.ToolResult.ToolName != "poff" {
			continue
		}
		if len(msg.ToolResult.Content) >= len(bigContent) {
			t.Errorf("persisted tool result %s content is %d bytes (>= original %d), want the offload stub applied "+
				"even to the fatal tail-append path", msg.ToolResult.CallID, len(msg.ToolResult.Content), len(bigContent))
			continue
		}
		foundShrunk = true
	}
	if !foundShrunk {
		t.Fatal("expected at least one persisted poff tool result with offload-shrunk content")
	}
}

// parallelRepeatSuccessProvider emits one batch of 5 identical successful
// ParallelSafe tool calls, then stops — enough in a single turn to land the
// repeat-call counter exactly on maxRepeatCalls and trigger the one-time hint.
type parallelRepeatSuccessProvider struct {
	callCount int
}

func (p *parallelRepeatSuccessProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *parallelRepeatSuccessProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.callCount++
	ch := make(chan llm.StreamChunk, 1)
	if p.callCount > 1 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{Done: true, Stop: "stop"}
		}()
		return ch, nil
	}
	calls := make([]models.ToolCall, 5)
	for i := range calls {
		calls[i] = models.ToolCall{ID: fmt.Sprintf("pecho-%d", i), Name: "pecho", Arguments: map[string]any{"msg": "hi"}}
	}
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{ToolCalls: calls, Stop: "tool_calls", Done: true}
	}()
	return ch, nil
}

// TestRepeatBreaker_HintOnRepeatedIdenticalParallelCalls verifies the
// non-fatal repeat-call hint (injected at maxRepeatCalls) also fires on the
// parallel tool-execution path, not just the serial one.
func TestRepeatBreaker_HintOnRepeatedIdenticalParallelCalls(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name:         "pecho",
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"msg": map[string]any{"type": "string"}},
		},
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{CallID: c.ID, ToolName: c.Name, Content: "ok"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	provider := &parallelRepeatSuccessProvider{}
	a := New(AgentConfig{
		LLMProvider:  provider,
		Tools:        reg,
		MaxToolCalls: 10,
	})

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "echo parallel"},
	})
	if err != nil {
		t.Fatalf("expected no fatal error for successful repeats, got: %v", err)
	}
	hintFound := false
	for _, msg := range result.Messages {
		if msg.Role == models.RoleHuman && strings.Contains(strings.ToLower(msg.Content), "loop") {
			hintFound = true
			break
		}
	}
	if !hintFound {
		t.Fatal("expected a non-fatal hint about the loop to be injected into messages (parallel path)")
	}
}

// parallelRepeatSuccessProvider6 emits one batch of 6 identical successful
// ParallelSafe tool calls, then stops. maxRepeatCalls is 5, so the repeat
// hint fires while processing the batch's 5th result — with a 6th result
// still to come, this is the concrete shape that reproduces the
// hint-interleaves-mid-batch bug (M1-7).
type parallelRepeatSuccessProvider6 struct {
	callCount int
}

func (p *parallelRepeatSuccessProvider6) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *parallelRepeatSuccessProvider6) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.callCount++
	ch := make(chan llm.StreamChunk, 1)
	if p.callCount > 1 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{Done: true, Stop: "stop"}
		}()
		return ch, nil
	}
	calls := make([]models.ToolCall, 6)
	for i := range calls {
		calls[i] = models.ToolCall{ID: fmt.Sprintf("pecho6-%d", i), Name: "pecho", Arguments: map[string]any{"msg": "hi"}}
	}
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{ToolCalls: calls, Stop: "tool_calls", Done: true}
	}()
	return ch, nil
}

// TestRepeatBreaker_HintDeferredToBatchEnd verifies that a breaker hint
// triggered mid-batch (on a parallel-safe batch of 6 identical calls, where
// maxRepeatCalls=5 fires on the 5th result) is NOT interleaved between tool
// results: every RoleTool message of the batch must precede the batch's
// RoleHuman hint message in the returned Messages. Interleaving the hint mid
// batch breaks the Anthropic/OpenAI provider message contracts (M1-7).
func TestRepeatBreaker_HintDeferredToBatchEnd(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name:         "pecho",
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"msg": map[string]any{"type": "string"}},
		},
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{CallID: c.ID, ToolName: c.Name, Content: "ok"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	provider := &parallelRepeatSuccessProvider6{}
	a := New(AgentConfig{
		LLMProvider:  provider,
		Tools:        reg,
		MaxToolCalls: 10,
	})

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "echo parallel six"},
	})
	if err != nil {
		t.Fatalf("expected no fatal error for successful repeats, got: %v", err)
	}

	hintIdx := -1
	lastToolIdx := -1
	for i, msg := range result.Messages {
		if msg.Role == models.RoleTool {
			lastToolIdx = i
		}
		if msg.Role == models.RoleHuman && strings.Contains(strings.ToLower(msg.Content), "loop") {
			hintIdx = i
		}
	}
	if hintIdx == -1 {
		t.Fatal("expected a non-fatal loop hint to be injected into messages")
	}
	if lastToolIdx == -1 {
		t.Fatal("expected tool result messages in the batch")
	}
	if hintIdx < lastToolIdx {
		t.Fatalf("hint message at index %d appeared before the batch's last tool result at index %d; "+
			"hint must be appended after ALL of the batch's tool results", hintIdx, lastToolIdx)
	}
}

// serialRepeatSuccessProvider emits one batch of 6 identical successful
// tool calls to a NON-ParallelSafe tool, then stops. maxRepeatCalls is 5, so
// the repeat hint fires while processing the batch's 5th result — with a
// 6th result still to come on the SERIAL tool-execution path.
type serialRepeatSuccessProvider struct {
	callCount int
}

func (p *serialRepeatSuccessProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *serialRepeatSuccessProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.callCount++
	ch := make(chan llm.StreamChunk, 1)
	if p.callCount > 1 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{Done: true, Stop: "stop"}
		}()
		return ch, nil
	}
	calls := make([]models.ToolCall, 6)
	for i := range calls {
		calls[i] = models.ToolCall{ID: fmt.Sprintf("secho-%d", i), Name: "secho", Arguments: map[string]any{"msg": "hi"}}
	}
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{ToolCalls: calls, Stop: "tool_calls", Done: true}
	}()
	return ch, nil
}

// TestRepeatBreaker_HintDeferredToBatchEnd_SerialPath is the serial-path
// counterpart of TestRepeatBreaker_HintDeferredToBatchEnd: the "secho" tool
// is registered WITHOUT ParallelSafe, so a batch of 6 identical calls to it
// forces a.allParallelSafe(toolCalls) to be false and the whole batch runs
// through the SERIAL tool-execution loop, not the parallel one. It verifies
// the same invariant — every RoleTool message of the batch must precede the
// batch's RoleHuman hint message — on that other code path.
func TestRepeatBreaker_HintDeferredToBatchEnd_SerialPath(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name: "secho",
		// ParallelSafe intentionally left false (zero value) so this batch
		// is forced onto the serial tool-execution path.
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"msg": map[string]any{"type": "string"}},
		},
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{CallID: c.ID, ToolName: c.Name, Content: "ok"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	provider := &serialRepeatSuccessProvider{}
	a := New(AgentConfig{
		LLMProvider:  provider,
		Tools:        reg,
		MaxToolCalls: 10,
	})

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "echo serial six"},
	})
	if err != nil {
		t.Fatalf("expected no fatal error for successful repeats, got: %v", err)
	}

	hintIdx := -1
	lastToolIdx := -1
	for i, msg := range result.Messages {
		if msg.Role == models.RoleTool {
			lastToolIdx = i
		}
		if msg.Role == models.RoleHuman && strings.Contains(strings.ToLower(msg.Content), "loop") {
			hintIdx = i
		}
	}
	if hintIdx == -1 {
		t.Fatal("expected a non-fatal loop hint to be injected into messages")
	}
	if lastToolIdx == -1 {
		t.Fatal("expected tool result messages in the batch")
	}
	if hintIdx < lastToolIdx {
		t.Fatalf("hint message at index %d appeared before the batch's last tool result at index %d (serial path); "+
			"hint must be appended after ALL of the batch's tool results", hintIdx, lastToolIdx)
	}
}

// serialRepeatLoopProviderHintThenFatal emits 4 turns of a single failing,
// non-parallel-safe call (bringing the repeat counters to 4), then a single
// turn with a batch of 5 identical calls. Within that batch: call index 0
// brings the count to exactly 5 (the non-fatal repeat-hint threshold —
// queued into pendingHints, not yet flushed), and call index 3 brings the
// count to 8 (the fatal repeat-fail threshold) — a hint queued mid-batch,
// then a LATER fatal trip in the SAME batch, with call index 4 never
// executed by the serial loop (requiring a synthesized tail result).
type serialRepeatLoopProviderHintThenFatal struct {
	callCount int
}

func (p *serialRepeatLoopProviderHintThenFatal) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *serialRepeatLoopProviderHintThenFatal) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.callCount++
	ch := make(chan llm.StreamChunk, 1)
	if p.callCount > 20 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{Done: true, Stop: "stop"}
		}()
		return ch, nil
	}
	turn := p.callCount
	var calls []models.ToolCall
	if turn <= 4 {
		calls = []models.ToolCall{
			{ID: fmt.Sprintf("shf-%d-0", turn), Name: "shf", Arguments: map[string]any{"x": "y"}},
		}
	} else {
		calls = make([]models.ToolCall, 5)
		for i := range calls {
			calls[i] = models.ToolCall{ID: fmt.Sprintf("shf-%d-%d", turn, i), Name: "shf", Arguments: map[string]any{"x": "y"}}
		}
	}
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{ToolCalls: calls, Stop: "tool_calls", Done: true}
	}()
	return ch, nil
}

// TestSerialBreaker_HintFlushedAfterSynthesizedResults is the RED test for
// the coordinator review MEDIUM finding: on the fatal path, pendingHints was
// flushed BEFORE the synthesized tail tool_results were appended, producing
// tr(0..3), HINT, tr(4) — a hint landing between the batch's earlier tool
// results and its LAST one, violating the M1-7 invariant that a hint
// (RoleHuman) never lands between an assistant tool_calls message and any of
// its tool results.
func TestSerialBreaker_HintFlushedAfterSynthesizedResults(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name: "shf",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"x": map[string]any{"type": "string"}},
		},
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{
				CallID:   c.ID,
				ToolName: c.Name,
				Status:   models.CallStatusFailed,
				Error:    "boom",
			}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	a := New(AgentConfig{
		LLMProvider:  &serialRepeatLoopProviderHintThenFatal{},
		Tools:        reg,
		MaxToolCalls: 50,
	})

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "run serial"},
	})
	if err == nil {
		t.Fatal("expected the agent to stop with a fatal repeat-loop error")
	}
	if result == nil {
		t.Fatal("expected a non-nil RunResult even on fatal breaker error")
	}

	var lastAI *models.Message
	for i := range result.Messages {
		if result.Messages[i].Role == models.RoleAI && len(result.Messages[i].ToolCalls) > 0 {
			lastAI = &result.Messages[i]
		}
	}
	if lastAI == nil || len(lastAI.ToolCalls) != 5 {
		t.Fatalf("expected the final assistant message to carry 5 tool calls; got %+v", lastAI)
	}

	hintIdx := -1
	lastToolIdx := -1
	toolResultIDs := make(map[string]bool)
	for i, msg := range result.Messages {
		if msg.Role == models.RoleTool && msg.ToolResult != nil {
			toolResultIDs[msg.ToolResult.CallID] = true
			lastToolIdx = i
		}
		if msg.Role == models.RoleHuman && strings.Contains(strings.ToLower(msg.Content), "loop") {
			hintIdx = i
		}
	}
	for _, call := range lastAI.ToolCalls {
		if !toolResultIDs[call.ID] {
			t.Errorf("tool_use ID %q has no matching tool_result", call.ID)
		}
	}
	if hintIdx == -1 {
		t.Fatal("expected a non-fatal loop hint to be injected into messages")
	}
	if hintIdx < lastToolIdx {
		t.Fatalf("hint message at index %d appeared before the batch's LAST tool result (including the "+
			"synthesized one) at index %d; a hint must never land between an assistant tool_calls message "+
			"and any of its tool results, even on the fatal path", hintIdx, lastToolIdx)
	}
}

// parallelRepeatLoopProviderHintThenFatal emits 4 turns of a single failing
// ParallelSafe call (bringing the repeat counters to 4), then a single turn
// with a batch of 5 identical ParallelSafe calls — same count arithmetic as
// serialRepeatLoopProviderHintThenFatal (hint at batch index 0, fatal at
// batch index 3), but exercised on the parallel path's already-computed
// tail-append loop instead of the serial path's synthesize loop.
type parallelRepeatLoopProviderHintThenFatal struct {
	callCount int
}

func (p *parallelRepeatLoopProviderHintThenFatal) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *parallelRepeatLoopProviderHintThenFatal) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.callCount++
	ch := make(chan llm.StreamChunk, 1)
	if p.callCount > 20 {
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{Done: true, Stop: "stop"}
		}()
		return ch, nil
	}
	turn := p.callCount
	var calls []models.ToolCall
	if turn <= 4 {
		calls = []models.ToolCall{
			{ID: fmt.Sprintf("phf-%d-0", turn), Name: "phf", Arguments: map[string]any{"x": "y"}},
		}
	} else {
		calls = make([]models.ToolCall, 5)
		for i := range calls {
			calls[i] = models.ToolCall{ID: fmt.Sprintf("phf-%d-%d", turn, i), Name: "phf", Arguments: map[string]any{"x": "y"}}
		}
	}
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{ToolCalls: calls, Stop: "tool_calls", Done: true}
	}()
	return ch, nil
}

// TestParallelBreaker_HintFlushedAfterTailAppend is the parallel-path
// analogue of TestSerialBreaker_HintFlushedAfterSynthesizedResults — the
// same pendingHints-flushed-too-early bug pre-dates this task (M1) in the
// parallel fatal tail-append loop.
func TestParallelBreaker_HintFlushedAfterTailAppend(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name:         "phf",
		ParallelSafe: true,
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"x": map[string]any{"type": "string"}},
		},
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{
				CallID:   c.ID,
				ToolName: c.Name,
				Status:   models.CallStatusFailed,
				Error:    "boom",
			}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	a := New(AgentConfig{
		LLMProvider:  &parallelRepeatLoopProviderHintThenFatal{},
		Tools:        reg,
		MaxToolCalls: 50,
	})

	result, err := a.Run(context.Background(), "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "run parallel"},
	})
	if err == nil {
		t.Fatal("expected the agent to stop with a fatal repeat-loop error")
	}
	if result == nil {
		t.Fatal("expected a non-nil RunResult even on fatal breaker error")
	}

	var lastAI *models.Message
	for i := range result.Messages {
		if result.Messages[i].Role == models.RoleAI && len(result.Messages[i].ToolCalls) > 0 {
			lastAI = &result.Messages[i]
		}
	}
	if lastAI == nil || len(lastAI.ToolCalls) != 5 {
		t.Fatalf("expected the final assistant message to carry 5 tool calls; got %+v", lastAI)
	}

	hintIdx := -1
	lastToolIdx := -1
	toolResultIDs := make(map[string]bool)
	for i, msg := range result.Messages {
		if msg.Role == models.RoleTool && msg.ToolResult != nil {
			toolResultIDs[msg.ToolResult.CallID] = true
			lastToolIdx = i
		}
		if msg.Role == models.RoleHuman && strings.Contains(strings.ToLower(msg.Content), "loop") {
			hintIdx = i
		}
	}
	for _, call := range lastAI.ToolCalls {
		if !toolResultIDs[call.ID] {
			t.Errorf("tool_use ID %q has no matching tool_result", call.ID)
		}
	}
	if hintIdx == -1 {
		t.Fatal("expected a non-fatal loop hint to be injected into messages")
	}
	if hintIdx < lastToolIdx {
		t.Fatalf("hint message at index %d appeared before the batch's LAST tool result (including the "+
			"already-computed tail append) at index %d; a hint must never land between an assistant "+
			"tool_calls message and any of its tool results, even on the fatal path", hintIdx, lastToolIdx)
	}
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

	// Explicit Tools keeps this test about MODEL resolution: it must not depend
	// on which tools the general-purpose profile happens to allowlist, and
	// selectSubagentTools rejects a selector list that matches nothing.
	exec := NewSubagentExecutor(reg, registryWithNoopTool(t), nil)
	_, err := exec.Execute(context.Background(),
		&subagent.Task{ID: "t1", Prompt: "hi", Config: subagent.SubagentConfig{AgentType: "general-purpose", Tools: []string{"noop"}}},
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

	exec2 := NewSubagentExecutor(regOverride, registryWithNoopTool(t), nil)
	_, err = exec2.Execute(context.Background(),
		&subagent.Task{ID: "t2", Prompt: "hi", Config: subagent.SubagentConfig{AgentType: "general-purpose", Model: "review", Tools: []string{"noop"}}},
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

// TestOffload_NoOffloadFlagKeepsContentInContext pins the exemption contract
// for tools whose deliverable IS large in-context content (code_map with
// include_content): a handler setting models.ToolDataNoOffload in Data must
// keep its full Content in the tool result even past the offload threshold,
// because offloading would write away exactly what the call asked to bring
// IN. Such handlers bound their own output size instead.
func TestOffload_NoOffloadFlagKeepsContentInContext(t *testing.T) {
	tmpDir := t.TempDir()
	offloadDir := tmpDir + "/offload"

	var contentBuilder strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&contentBuilder, "line %03d: %s\n", i, strings.Repeat("y", 140))
	}
	largeContent := contentBuilder.String() // ~30KB, over the 24KB threshold

	reg := tools.NewRegistry()
	// offloadTestProvider hardcodes a call to "big_tool".
	if err := reg.Register(models.Tool{
		Name: "big_tool",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{
				CallID:   "exempt1",
				ToolName: "big_tool",
				Content:  largeContent,
				Data:     map[string]any{models.ToolDataNoOffload: true},
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
	// The full content must still be in context (message Content carries the
	// tool result text — same convention as the offload test above).
	if len(toolMsg.Content) < len(largeContent) {
		t.Fatalf("tool message content = %d bytes, want the full %d bytes kept in context", len(toolMsg.Content), len(largeContent))
	}
	if strings.Contains(toolMsg.Content, "[offloaded:") {
		t.Fatal("exempt tool result was offloaded despite ToolDataNoOffload")
	}
	// And nothing landed on disk.
	if entries, err := os.ReadDir(offloadDir); err == nil && len(entries) != 0 {
		t.Fatalf("offload dir has %d files, want none for an exempt result", len(entries))
	}
}
