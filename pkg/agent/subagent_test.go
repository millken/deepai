package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/subagent"
	"github.com/millken/deepai/pkg/tools"
)

type fakeUI struct{ tools.UserInteraction }

// A subagent strips inherited UserInteraction so delegated work never prompts
// the user (plan confirmations auto-approve, clarifications use best judgment).
func TestSubagentStripsUserInteraction(t *testing.T) {
	ctx := tools.WithUserInteraction(context.Background(), fakeUI{})
	if tools.UserInteractionFromContext(ctx) == nil {
		t.Fatal("precondition: UI should be present before strip")
	}
	ctx = tools.WithUserInteraction(ctx, nil)
	if tools.UserInteractionFromContext(ctx) != nil {
		t.Fatal("nil strip failed: delegated subagent would still prompt the user")
	}
}

func TestSubagentMessageFromAgentEvent(t *testing.T) {
	cases := []struct {
		name string
		evt  AgentEvent
		want string
	}{
		{"tool start", AgentEvent{Type: AgentEventToolCallStart, ToolEvent: &ToolCallEvent{Name: "edit_file"}}, "⚙ edit_file"},
		{"tool ok end", AgentEvent{Type: AgentEventToolCallEnd, ToolEvent: &ToolCallEvent{Name: "edit_file"}}, "✓ edit_file"},
		{"tool error end", AgentEvent{Type: AgentEventToolCallEnd, ToolEvent: &ToolCallEvent{Name: "bash", Error: "boom"}}, "✗ bash: boom"},
		{"agent error", AgentEvent{Type: AgentEventError, Err: "blew up"}, "✗ blew up"},
	}
	for _, c := range cases {
		if got := subagentMessageFromAgentEvent(c.evt); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// TestResolveMaxTurns_Priority verifies the MaxTurns resolution chain:
// caller-explicit > agent type profile > safety floor (6).
func TestResolveMaxTurns_Priority(t *testing.T) {
	// Simulate the executor's maxTurns resolution logic.
	// In production this runs inside Execute(), but the logic is:
	//   maxTurns = task.Config.MaxTurns    // caller-explicit (max_turns arg)
	//   if maxTurns <= 0: maxTurns = profileCfg.MaxTurns  // builtin/YAML
	//   if maxTurns <= 0: maxTurns = 6     // safety floor
	resolve := func(callerMaxTurns, profileMaxTurns int) int {
		maxTurns := callerMaxTurns
		if maxTurns <= 0 {
			maxTurns = profileMaxTurns
		}
		if maxTurns <= 0 {
			maxTurns = 6
		}
		return maxTurns
	}

	tests := []struct {
		name            string
		callerMaxTurns  int
		profileMaxTurns int
		want            int
	}{
		{"caller overrides profile", 20, 10, 20},
		{"profile used when caller is 0", 0, 10, 10},
		{"safety floor when both 0", 0, 0, 6},
		{"caller=0 profile=0 → floor", 0, 0, 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolve(tt.callerMaxTurns, tt.profileMaxTurns); got != tt.want {
				t.Errorf("resolve(%d, %d) = %d, want %d", tt.callerMaxTurns, tt.profileMaxTurns, got, tt.want)
			}
		})
	}
}

// budgetReportingProvider always reports Usage.TotalTokens per turn and
// keeps calling a tool so the run runs multiple turns — enough for the
// budget check (react.go, top of the turn loop) to observe the accumulated
// usage from the previous turn and trip once it crosses TokenBudget.
type budgetReportingProvider struct {
	turn            int
	tokensPerTurn   int
	toolCallsRemain int
}

func (p *budgetReportingProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *budgetReportingProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.turn++
	ch := make(chan llm.StreamChunk, 1)
	usage := &llm.Usage{InputTokens: p.tokensPerTurn / 2, OutputTokens: p.tokensPerTurn / 2, TotalTokens: p.tokensPerTurn}
	if p.toolCallsRemain > 0 {
		p.toolCallsRemain--
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{ID: "noop-1", Name: "noop", Arguments: map[string]any{}}},
				Usage:     usage,
				Stop:      "tool_calls",
				Done:      true,
			}
		}()
		return ch, nil
	}
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{Usage: usage, Done: true, Stop: "stop"}
	}()
	return ch, nil
}

func noopTool() models.Tool {
	return models.Tool{
		Name: "noop",
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name, Status: models.CallStatusCompleted, Content: "ok"}, nil
		},
	}
}

// TestSubagentExecutor_PassesTokenBudgetToAgentConfig is the RED test for
// M2-2 (12d): task.Config.TokenBudget must reach the subagent's
// AgentConfig.MaxTokensBudget, which react.go's turn-loop budget check then
// enforces. A tiny budget (10) with a provider that reports 20 tokens on its
// first turn must make the SECOND turn's budget check trip — proving the
// value was actually threaded through Execute, not just accepted and
// dropped.
//
// RED signature (today): SubagentExecutor.Execute never sets
// AgentConfig.MaxTokensBudget from task.Config.TokenBudget, so the budget
// check never fires and Execute returns no error regardless of TokenBudget.
func TestSubagentExecutor_PassesTokenBudgetToAgentConfig(t *testing.T) {
	reg := llm.NewSingleModelRegistry("test", "configured-model", "")
	provider := &budgetReportingProvider{tokensPerTurn: 20, toolCallsRemain: 1}
	reg.InjectProvider("test", "", "", provider)

	toolReg := tools.NewRegistry()
	if err := toolReg.Register(noopTool()); err != nil {
		t.Fatalf("register noop tool: %v", err)
	}

	exec := NewSubagentExecutor(reg, toolReg, nil)
	execResult, err := exec.Execute(context.Background(),
		&subagent.Task{ID: "t1", Prompt: "hi", Config: subagent.SubagentConfig{AgentType: "general-purpose", TokenBudget: 10}},
		func(subagent.TaskEvent) {})
	if err == nil {
		t.Fatal("Execute() error = nil, want a token-budget-exceeded error (TokenBudget=10 with 20 tokens/turn)")
	}
	if !strings.Contains(err.Error(), "token budget") {
		t.Fatalf("Execute() error = %q, want it to mention the token budget", err.Error())
	}
	// H1: Run() populates Usage on ALL error paths (including the token-budget
	// error above), so ExecutionResult.Usage must carry it through even when
	// Execute returns a non-nil error — dropping it here is exactly what a
	// pool.finishTask caller (which stores result.Usage regardless of err)
	// would silently lose.
	if execResult.Usage == nil {
		t.Fatal("ExecutionResult.Usage = nil on the error path, want the pre-trip usage preserved (H1)")
	}
	if execResult.Usage.PromptTokens != 10 || execResult.Usage.CompletionTokens != 10 || execResult.Usage.TotalTokens != 20 {
		t.Fatalf("ExecutionResult.Usage = %+v, want {PromptTokens:10 CompletionTokens:10 TotalTokens:20} (the turn-0 usage recorded before the budget trip)", execResult.Usage)
	}

	// Sanity: without a TokenBudget, the same provider/tool setup must NOT
	// trip any budget error (rules out a false positive from something else
	// in the harness, e.g. MaxTurns).
	reg2 := llm.NewSingleModelRegistry("test", "configured-model", "")
	provider2 := &budgetReportingProvider{tokensPerTurn: 20, toolCallsRemain: 1}
	reg2.InjectProvider("test", "", "", provider2)
	toolReg2 := tools.NewRegistry()
	if err := toolReg2.Register(noopTool()); err != nil {
		t.Fatalf("register noop tool: %v", err)
	}
	exec2 := NewSubagentExecutor(reg2, toolReg2, nil)
	_, err = exec2.Execute(context.Background(),
		&subagent.Task{ID: "t2", Prompt: "hi", Config: subagent.SubagentConfig{AgentType: "general-purpose"}},
		func(subagent.TaskEvent) {})
	if err != nil {
		t.Fatalf("Execute() (no budget) error = %v, want nil", err)
	}
}

// --- M2-3: OutputSchema validation + retry in SubagentExecutor.Execute ---
//
// scriptedOutputProvider scripts each Stream() call by its 0-based global
// call index (the SAME provider instance backs every attempt — the initial
// run and every retry each construct a fresh *Agent, but Execute reuses one
// provider across all of them via buildAgentConfig's closure — so a test can
// script the entire attempt sequence up front). By default a call ends the
// turn with outputs[idx] as the final text (no tool calls). toolCalls[idx],
// if set, makes that call emit tool calls instead (to drive multi-turn
// budget-check scenarios). streamErr[idx], if set, makes Stream() itself
// return that error (simulating a hard failure mid-retry, e.g. a dropped
// connection, independent of schema validation). It also records the
// messages of each Stream request so a test can assert retry history.
type scriptedOutputProvider struct {
	mu        sync.Mutex
	outputs   map[int]string
	usages    map[int]*llm.Usage
	toolCalls map[int][]models.ToolCall
	streamErr map[int]error
	calls     int
	reqMsgs   [][]models.Message
}

func (p *scriptedOutputProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *scriptedOutputProvider) Stream(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.mu.Lock()
	idx := p.calls
	p.calls++
	p.reqMsgs = append(p.reqMsgs, append([]models.Message(nil), req.Messages...))
	streamErr := p.streamErr[idx]
	output := p.outputs[idx]
	usage := p.usages[idx]
	toolCalls := p.toolCalls[idx]
	p.mu.Unlock()

	if streamErr != nil {
		return nil, streamErr
	}

	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		if len(toolCalls) > 0 {
			ch <- llm.StreamChunk{ToolCalls: toolCalls, Usage: usage, Stop: "tool_calls", Done: true}
			return
		}
		ch <- llm.StreamChunk{Delta: output, Usage: usage, Stop: "stop", Done: true}
	}()
	return ch, nil
}

func (p *scriptedOutputProvider) messagesForCall(idx int) []models.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx < 0 || idx >= len(p.reqMsgs) {
		return nil
	}
	return p.reqMsgs[idx]
}

func (p *scriptedOutputProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// reviewerExecutor builds a SubagentExecutor whose Execute call resolves the
// builtin "security-reviewer" agent type — the only way to reach a real,
// Strict+MaxRetries(1) OutputSchema, since the YAML loader can't set one
// (OutputSchema is `yaml:"-"`, so it can only come from a builtin profile).
// task.Config.Tools=["noop"] overrides the profile's DefaultTools
// (read_file/grep/...), so only the fake noop tool needs to be registered
// for the run to proceed.
func reviewerExecutor(t *testing.T, provider llm.LLMProvider) *SubagentExecutor {
	t.Helper()
	reg := llm.NewSingleModelRegistry("test", "configured-model", "")
	reg.InjectProvider("test", "", "", provider)
	toolReg := tools.NewRegistry()
	if err := toolReg.Register(noopTool()); err != nil {
		t.Fatalf("register noop tool: %v", err)
	}
	return NewSubagentExecutor(reg, toolReg, nil)
}

const invalidReviewJSON = `{"agent":"security-reviewer","summary":"no verdict","issues":[]}`
const validReviewJSON = `{"agent":"security-reviewer","verdict":"pass","summary":"clean","issues":[]}`

// securityReviewerMaxRetries reads MaxRetries from the actual builtin profile
// instead of a hardcoded literal (coordinator review item 4), so these tests
// keep proving the real wiring even if types_config.go's
// WithMaxRetries(...) value ever changes.
func securityReviewerMaxRetries(t *testing.T) int {
	t.Helper()
	schema := BuiltinAgentTypes[AgentTypeSecurityReviewer].OutputSchema
	if schema == nil || !schema.Strict || schema.MaxRetries < 1 {
		t.Fatalf("security-reviewer OutputSchema = %+v, want Strict=true and MaxRetries>=1 for these tests to be meaningful", schema)
	}
	return schema.MaxRetries
}

// TestSubagentExecutor_OutputSchemaRetrySucceeds is the RED test for M2-3:
// the first attempt's output fails ReviewResult schema validation (missing
// "verdict"); Strict+MaxRetries must trigger a retry, which succeeds on its
// first try. Execute must return the valid output and the SUM of both
// attempts' usage, not just the second attempt's, and must have emitted a
// "retrying: output failed schema validation" task_running event (coordinator
// review item 5) through the existing emit path.
//
// RED signature (today): Execute never calls ValidateOutput at all, so it
// returns the FIRST (invalid) output with a nil error and only the first
// attempt's usage.
func TestSubagentExecutor_OutputSchemaRetrySucceeds(t *testing.T) {
	securityReviewerMaxRetries(t) // asserts the schema is Strict with MaxRetries>=1
	provider := &scriptedOutputProvider{
		outputs: map[int]string{0: invalidReviewJSON, 1: validReviewJSON},
		usages: map[int]*llm.Usage{
			0: {InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
			1: {InputTokens: 20, OutputTokens: 10, TotalTokens: 30},
		},
	}
	exec := reviewerExecutor(t, provider)

	var events []subagent.TaskEvent
	var eventsMu sync.Mutex
	execResult, err := exec.Execute(context.Background(),
		&subagent.Task{ID: "t1", Prompt: "review this", Config: subagent.SubagentConfig{
			AgentType: "security-reviewer",
			Tools:     []string{"noop"},
		}},
		func(evt subagent.TaskEvent) {
			eventsMu.Lock()
			events = append(events, evt)
			eventsMu.Unlock()
		})
	if err != nil {
		t.Fatalf("Execute() error = %v, want nil (retry should have produced valid output)", err)
	}
	if execResult.Result != validReviewJSON {
		t.Fatalf("Result = %q, want the valid retried output %q", execResult.Result, validReviewJSON)
	}
	if provider.callCount() != 2 {
		t.Fatalf("provider callCount = %d, want 2 (initial + 1 retry)", provider.callCount())
	}
	if execResult.Usage == nil {
		t.Fatal("Usage = nil, want the summed usage across both attempts")
	}
	if execResult.Usage.PromptTokens != 30 || execResult.Usage.CompletionTokens != 15 || execResult.Usage.TotalTokens != 45 {
		t.Fatalf("Usage = %+v, want {PromptTokens:30 CompletionTokens:15 TotalTokens:45} (sum of both attempts)", execResult.Usage)
	}

	// The retry's request history must be seeded via appendParseError: it
	// should contain a human message describing the parse failure and
	// echoing the invalid output.
	retryMsgs := provider.messagesForCall(1)
	found := false
	for _, m := range retryMsgs {
		if m.Role == models.RoleHuman && strings.Contains(m.Content, "could not be parsed") && strings.Contains(m.Content, invalidReviewJSON) {
			found = true
			// Coordinator review item 6: the seeded retry message must be
			// stamped with ID/SessionID/CreatedAt consistent with the initial
			// seed message, not left at zero value.
			if m.ID == "" || m.SessionID != "t1" || m.CreatedAt.IsZero() {
				t.Errorf("retry seed message = %+v, want stamped ID/SessionID=%q/non-zero CreatedAt", m, "t1")
			}
		}
	}
	if !found {
		t.Fatalf("retry messages = %+v, want a human message from appendParseError referencing the invalid output", retryMsgs)
	}

	// Coordinator review item 5: a "retrying" event must be emitted at the
	// top of the retry, through the same emit() path as everything else.
	eventsMu.Lock()
	defer eventsMu.Unlock()
	retryEventFound := false
	for _, evt := range events {
		if evt.Type == "task_running" && evt.Message == "retrying: output failed schema validation" {
			retryEventFound = true
		}
	}
	if !retryEventFound {
		t.Fatalf("events = %+v, want a task_running event with Message %q", events, "retrying: output failed schema validation")
	}
}

// TestSubagentExecutor_OutputSchemaFailSoftAfterRetriesExhausted is the RED
// test for M2-3 coordinator review item 1 (HIGH, fail-soft redesign): when
// every attempt (initial + all retries) fails schema validation, Execute
// must NOT return an error — react.go's runOneTool discards ToolResult.Content
// whenever the tool's Execute returns a non-nil error (verified: react.go's
// runOneTool rebuilds a fresh ToolResult with only Data carried over, not
// Content), so an error here would silently drop the model's raw output.
// Instead Execute must return (nil error, Result = a warning-prefixed payload
// that still contains the last raw output).
//
// Reads MaxRetries dynamically (item 4) rather than assuming a literal, and
// scripts exactly maxRetries+1 invalid outputs so the schema fails on every
// attempt regardless of the configured retry count.
func TestSubagentExecutor_OutputSchemaFailSoftAfterRetriesExhausted(t *testing.T) {
	maxRetries := securityReviewerMaxRetries(t)
	attempts := maxRetries + 1
	outputs := make(map[int]string, attempts)
	usages := make(map[int]*llm.Usage, attempts)
	for i := 0; i < attempts; i++ {
		outputs[i] = fmt.Sprintf(`{"agent":"security-reviewer","summary":"still no verdict %d","issues":[]}`, i)
		usages[i] = &llm.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
	}
	lastInvalid := outputs[attempts-1]
	provider := &scriptedOutputProvider{outputs: outputs, usages: usages}
	exec := reviewerExecutor(t, provider)

	execResult, err := exec.Execute(context.Background(),
		&subagent.Task{ID: "t2", Prompt: "review this", Config: subagent.SubagentConfig{
			AgentType: "security-reviewer",
			Tools:     []string{"noop"},
		}},
		func(subagent.TaskEvent) {})
	if err != nil {
		t.Fatalf("Execute() error = %v, want nil (fail-soft: schema failures no longer surface as an Execute error)", err)
	}
	if !strings.HasPrefix(execResult.Result, outputSchemaWarningPrefix) {
		t.Fatalf("Result = %q, want it to start with the warning prefix %q", execResult.Result, outputSchemaWarningPrefix)
	}
	if !strings.Contains(execResult.Result, lastInvalid) {
		t.Fatalf("Result = %q, want it to still contain the last raw output %q", execResult.Result, lastInvalid)
	}
	if !strings.Contains(execResult.Result, "schema validation failed") {
		t.Fatalf("Result = %q, want it to explain why validation failed", execResult.Result)
	}
	if provider.callCount() != attempts {
		t.Fatalf("provider callCount = %d, want %d (initial + %d retries)", provider.callCount(), attempts, maxRetries)
	}
	wantTotal := attempts * 15
	if execResult.Usage == nil || execResult.Usage.TotalTokens != wantTotal {
		t.Fatalf("Usage = %+v, want TotalTokens=%d (summed across all %d failed attempts — the tokens were still spent)", execResult.Usage, wantTotal, attempts)
	}
}

// TestSubagentExecutor_OutputSchemaRetryUsesRemainingBudget is the RED test
// for M2-3 coordinator review item 2 (MEDIUM, budget multiplication): a retry
// must draw down the SAME task-level TokenBudget, not a fresh one, otherwise
// N retries could spend up to N times task.Config.TokenBudget. TokenBudget=20
// and the initial attempt reports 15 tokens, so the retry must run with
// MaxTokensBudget=5 (20-15), not 20 again.
//
// To observe the retry's actual budget (AgentConfig isn't otherwise
// inspectable from outside), the retry is scripted as a tool-call turn
// reporting 10 tokens: react.go's turn-loop budget check
// (`usage.TotalTokens >= a.maxTokensBudget`) runs at the START of the
// following turn using the PREVIOUS turn's accumulated usage. With the
// correct remaining budget (5), 10 >= 5 trips immediately after the retry's
// first turn (no second Stream call). If the bug were present (budget=20
// again), 10 >= 20 would NOT trip and a second Stream call would happen
// instead — the callCount and "(10/5)" assertions below distinguish the two.
func TestSubagentExecutor_OutputSchemaRetryUsesRemainingBudget(t *testing.T) {
	securityReviewerMaxRetries(t)
	provider := &scriptedOutputProvider{
		outputs: map[int]string{0: invalidReviewJSON},
		usages: map[int]*llm.Usage{
			0: {InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
			1: {InputTokens: 5, OutputTokens: 5, TotalTokens: 10},
		},
		toolCalls: map[int][]models.ToolCall{
			1: {{ID: "noop-1", Name: "noop", Arguments: map[string]any{}}},
		},
	}
	exec := reviewerExecutor(t, provider)

	execResult, err := exec.Execute(context.Background(),
		&subagent.Task{ID: "t4", Prompt: "review this", Config: subagent.SubagentConfig{
			AgentType:   "security-reviewer",
			Tools:       []string{"noop"},
			TokenBudget: 20,
		}},
		func(subagent.TaskEvent) {})
	if err == nil {
		t.Fatal("Execute() error = nil, want the retry to hit the token budget (proves it was NOT given the full 20 again)")
	}
	if !strings.Contains(err.Error(), "token budget (10/5)") {
		t.Fatalf("Execute() error = %q, want it to mention \"token budget (10/5)\" (retry budget = 20-15 = 5)", err.Error())
	}
	if provider.callCount() != 2 {
		t.Fatalf("provider callCount = %d, want 2 (initial attempt + retry's single tool-call turn; the budget must trip before a second retry turn)", provider.callCount())
	}
	// Item 3 (hard-error preservation) applies here too: the retry itself
	// errored (token budget), so Result must still carry the last attempt
	// that DID complete — the initial (invalid) output.
	if execResult.Result != invalidReviewJSON {
		t.Fatalf("Result = %q, want the last completed attempt's output %q preserved despite the retry's hard error", execResult.Result, invalidReviewJSON)
	}
}

// TestSubagentExecutor_OutputSchemaRetryHardErrorPreservesLastOutput is the
// RED test for M2-3 coordinator review item 3 (MEDIUM): if a retry's Run()
// itself fails for a reason UNRELATED to schema validation (e.g. a dropped
// connection), that is a genuine error and Execute must keep returning one —
// but it must not lose the last output that DID complete. Result/Messages
// must come from the last successful attempt (the initial one here), Usage
// must be the sum of both attempts, and the error must name both the
// original schema validation failure and the retry's own error.
func TestSubagentExecutor_OutputSchemaRetryHardErrorPreservesLastOutput(t *testing.T) {
	securityReviewerMaxRetries(t)
	provider := &scriptedOutputProvider{
		outputs: map[int]string{0: invalidReviewJSON},
		usages: map[int]*llm.Usage{
			0: {InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
		},
		streamErr: map[int]error{
			1: fmt.Errorf("connection reset by peer"),
		},
	}
	exec := reviewerExecutor(t, provider)

	execResult, err := exec.Execute(context.Background(),
		&subagent.Task{ID: "t5", Prompt: "review this", Config: subagent.SubagentConfig{
			AgentType: "security-reviewer",
			Tools:     []string{"noop"},
		}},
		func(subagent.TaskEvent) {})
	if err == nil {
		t.Fatal("Execute() error = nil, want the retry's hard error surfaced")
	}
	if !strings.Contains(err.Error(), "schema validation error") || !strings.Contains(err.Error(), "connection reset by peer") {
		t.Fatalf("Execute() error = %q, want it to mention both the schema validation failure and the retry's own error", err.Error())
	}
	if execResult.Result != invalidReviewJSON {
		t.Fatalf("Result = %q, want the last completed attempt's output %q, not dropped", execResult.Result, invalidReviewJSON)
	}
	if execResult.Usage == nil || execResult.Usage.TotalTokens != 15 {
		t.Fatalf("Usage = %+v, want TotalTokens=15 (the initial attempt's usage; the failed retry reported none)", execResult.Usage)
	}
}

// TestSubagentExecutor_OutputSchemaRetryDeadlineExceededFallsSoft is the RED
// test for the approved fall-through (M2-3 coordinator review item 3's
// hard-error branch, LOW): when the retry's error is context.DeadlineExceeded
// (errors.Is) AND a previous attempt already produced a FinalOutput, Execute
// must fall through to the fail-soft WARNING return (nil error, the last good
// output) instead of returning a wrapped hard error. The task-level deadline
// is shared across every attempt (initial + retries), so this is the common
// case, not the rare one — a retry simply running out of the same shared
// clock the initial attempt used.
//
// The test lets the retry's Run() see an already-expired ctx: attempt 0
// completes fast (well inside the ctx deadline) and produces invalid output,
// triggering a retry; the harness's own emit() callback (not production code)
// sleeps past the ctx deadline right when it observes the "retrying:" event —
// the same point production code emits it, immediately before calling
// runOnce for the retry — so by the time the retry's fresh Agent.Run() checks
// ctx.Err() at its own entry (react.go, before any Stream call), the shared
// ctx has already tripped and Run() returns the literal (unwrapped)
// context.DeadlineExceeded with a nil *RunResult, exactly mirroring the real
// shared-deadline race in production.
//
// RED signature (today): the retryErr != nil branch unconditionally returns a
// wrapped hard error regardless of the cause, so Execute returns a non-nil
// error here instead of a WARNING-prefixed Result with a nil error.
func TestSubagentExecutor_OutputSchemaRetryDeadlineExceededFallsSoft(t *testing.T) {
	securityReviewerMaxRetries(t)
	provider := &scriptedOutputProvider{
		outputs: map[int]string{0: invalidReviewJSON},
		usages: map[int]*llm.Usage{
			0: {InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
		},
	}
	exec := reviewerExecutor(t, provider)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	emit := func(evt subagent.TaskEvent) {
		if strings.Contains(evt.Message, "retrying:") {
			// Let the shared ctx's deadline actually elapse before the
			// retry's Agent.Run() call begins.
			time.Sleep(40 * time.Millisecond)
		}
	}

	execResult, err := exec.Execute(ctx,
		&subagent.Task{ID: "t6", Prompt: "review this", Config: subagent.SubagentConfig{
			AgentType: "security-reviewer",
			Tools:     []string{"noop"},
		}},
		emit)
	if err != nil {
		t.Fatalf("Execute() error = %v, want nil (ctx-deadline retry death with a prior good output falls soft)", err)
	}
	if !strings.HasPrefix(execResult.Result, outputSchemaWarningPrefix) {
		t.Fatalf("Result = %q, want it to start with the warning prefix %q", execResult.Result, outputSchemaWarningPrefix)
	}
	if !strings.Contains(execResult.Result, invalidReviewJSON) {
		t.Fatalf("Result = %q, want it to still contain attempt 1's raw output %q", execResult.Result, invalidReviewJSON)
	}
	if provider.callCount() != 1 {
		t.Fatalf("provider callCount = %d, want 1 (the retry died to ctx deadline before any second Stream call)", provider.callCount())
	}
	if execResult.Usage == nil || execResult.Usage.TotalTokens != 15 {
		t.Fatalf("Usage = %+v, want TotalTokens=15 (attempt 1's usage; the retry died before reporting any)", execResult.Usage)
	}
}

// hangingRetryProvider scripts a two-call sequence: the first Stream() call
// (attempt 1, index 0) returns immediately with the given output/usage; every
// subsequent call (the retry, index >= 1) BLOCKS on ctx.Done() instead of
// returning anything, then returns (nil, ctx.Err()) once the caller's ctx
// actually expires. This reproduces the "mid-stream" shared-deadline shape
// (as opposed to the entry-time shape, where Agent.Run's own ctx.Err() check
// fires before any Stream call is even made): react.go's Stream error
// handling routes this ctx.Err() through normalizeRunError, which — because
// ctx really is past its deadline at that point — converts it into a
// *TimeoutError (not the bare context.DeadlineExceeded), before returning it
// from Agent.Run.
type hangingRetryProvider struct {
	mu         sync.Mutex
	calls      int
	firstOut   string
	firstUsage *llm.Usage
}

func (p *hangingRetryProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *hangingRetryProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.mu.Lock()
	idx := p.calls
	p.calls++
	p.mu.Unlock()

	if idx == 0 {
		ch := make(chan llm.StreamChunk, 1)
		go func() {
			defer close(ch)
			ch <- llm.StreamChunk{Delta: p.firstOut, Usage: p.firstUsage, Stop: "stop", Done: true}
		}()
		return ch, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestSubagentExecutor_OutputSchemaRetryDeadlineExceededMidStreamFallsSoft is
// the RED test for the verifier's follow-up finding on the ctx-deadline
// fall-through (item 3): the original guard only checked
// errors.Is(retryErr, context.DeadlineExceeded), which catches the retry
// dying at Agent.Run's own entry-time ctx check (before any Stream call) —
// but a retry that starts and then hits the SAME shared deadline MID-STREAM
// gets react.go's normalizeRunError-wrapped *TimeoutError instead, which at
// the time did NOT implement Unwrap, so errors.Is was false and the
// hard-error branch still dropped the output for this (more common in
// practice) shape. (TimeoutError now implements Unwrap() ->
// context.DeadlineExceeded, so both arms of the guard match; the errors.As
// arm remains for clarity.)
//
// The provider's retry call (hangingRetryProvider, idx>=1) blocks on
// ctx.Done() — modeling a request that has actually started streaming — and
// only returns once the shared, short-lived ctx deadline fires, so Execute
// must still fall soft: nil error, WARNING-prefixed attempt-1 output, summed
// usage.
//
// RED signature (before widening the guard to errors.As(*TimeoutError)):
// Execute returns a non-nil wrapped hard error and drops the attempt-1
// output, exactly like the pre-fix entry-time case did.
func TestSubagentExecutor_OutputSchemaRetryDeadlineExceededMidStreamFallsSoft(t *testing.T) {
	securityReviewerMaxRetries(t)
	provider := &hangingRetryProvider{
		firstOut:   invalidReviewJSON,
		firstUsage: &llm.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	}
	exec := reviewerExecutor(t, provider)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	execResult, err := exec.Execute(ctx,
		&subagent.Task{ID: "t7", Prompt: "review this", Config: subagent.SubagentConfig{
			AgentType: "security-reviewer",
			Tools:     []string{"noop"},
		}},
		func(subagent.TaskEvent) {})
	if err != nil {
		t.Fatalf("Execute() error = %v, want nil (mid-stream ctx-deadline retry death with a prior good output falls soft)", err)
	}
	if !strings.HasPrefix(execResult.Result, outputSchemaWarningPrefix) {
		t.Fatalf("Result = %q, want it to start with the warning prefix %q", execResult.Result, outputSchemaWarningPrefix)
	}
	if !strings.Contains(execResult.Result, invalidReviewJSON) {
		t.Fatalf("Result = %q, want it to still contain attempt 1's raw output %q", execResult.Result, invalidReviewJSON)
	}
	if execResult.Usage == nil || execResult.Usage.TotalTokens != 15 {
		t.Fatalf("Usage = %+v, want TotalTokens=15 (attempt 1's usage; the hung retry reported none)", execResult.Usage)
	}
}

// TestSubagentExecutor_NoOutputSchema_NoRetryBehaviorChange confirms a
// profile without OutputSchema (general-purpose) is entirely unaffected:
// whatever the provider says is returned as-is on the first call, no matter
// how it's shaped.
func TestSubagentExecutor_NoOutputSchema_NoRetryBehaviorChange(t *testing.T) {
	provider := &scriptedOutputProvider{outputs: map[int]string{0: "not json at all, just prose"}}
	exec := reviewerExecutor(t, provider)

	execResult, err := exec.Execute(context.Background(),
		&subagent.Task{ID: "t3", Prompt: "hi", Config: subagent.SubagentConfig{
			AgentType: "general-purpose",
			Tools:     []string{"noop"},
		}},
		func(subagent.TaskEvent) {})
	if err != nil {
		t.Fatalf("Execute() error = %v, want nil (no OutputSchema on general-purpose)", err)
	}
	if execResult.Result != "not json at all, just prose" {
		t.Fatalf("Result = %q, want the raw output unchanged", execResult.Result)
	}
	if provider.callCount() != 1 {
		t.Fatalf("provider callCount = %d, want 1 (no retry without OutputSchema)", provider.callCount())
	}
}

// TestSubagentExecutor_NonStrictOutputSchema_SkipsValidation is the RED test
// for schema leftover L3 (coordinator decision): a non-Strict OutputSchema
// must skip validation entirely — no retry, and no WARNING-prefixed result —
// exactly the prompt-suffix-only behavior from before M2-3. Before this fix,
// Execute called ValidateOutput unconditionally and, since the non-Strict
// branch never retries, always fell through to the fail-soft WARNING-prefix
// return on any mismatch — even though Strict is false. All three real
// builtin reviewer schemas are Strict, so this can only be exercised via a
// synthetic BuiltinAgentTypes entry (temporarily registered, restored via
// defer) with an explicitly non-Strict schema.
func TestSubagentExecutor_NonStrictOutputSchema_SkipsValidation(t *testing.T) {
	nonStrictSchema := FromStruct[ReviewResult](WithStrict(false), WithMaxRetries(3))
	const fakeType = AgentType("nonstrict-fake-reviewer")
	BuiltinAgentTypes[fakeType] = AgentTypeConfig{
		Type:         fakeType,
		Name:         "nonstrict-fake-reviewer",
		SystemPrompt: "test",
		OutputSchema: nonStrictSchema,
	}
	defer delete(BuiltinAgentTypes, fakeType)

	// Missing required "verdict" field — would fail ReviewResult schema
	// validation if ValidateOutput were called.
	invalidOutput := `{"agent":"x","summary":"no verdict","issues":[]}`
	provider := &scriptedOutputProvider{outputs: map[int]string{0: invalidOutput}}
	exec := reviewerExecutor(t, provider)

	execResult, err := exec.Execute(context.Background(),
		&subagent.Task{ID: "t-nonstrict", Prompt: "review", Config: subagent.SubagentConfig{
			AgentType: string(fakeType),
			Tools:     []string{"noop"},
		}},
		func(subagent.TaskEvent) {})
	if err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}
	if provider.callCount() != 1 {
		t.Fatalf("provider callCount = %d, want 1 (non-Strict schema must never retry)", provider.callCount())
	}
	if strings.HasPrefix(execResult.Result, outputSchemaWarningPrefix) {
		t.Fatalf("Result = %q, must NOT carry the WARNING prefix for a non-Strict schema (L3: skip validation entirely, prompt-suffix behavior only)", execResult.Result)
	}
	if execResult.Result != invalidOutput {
		t.Fatalf("Result = %q, want the raw output unchanged: %q", execResult.Result, invalidOutput)
	}
}

// --- M2-4: task tool context_files — explicit delegation context bundle ---

// contextFilesExecutor builds a SubagentExecutor for context_files tests: a
// general-purpose agent type (no OutputSchema, so no retry complexity),
// backed by a scriptedOutputProvider that captures every Stream() request's
// messages so a test can inspect the seeded first human message.
func contextFilesExecutor(t *testing.T, provider *scriptedOutputProvider) *SubagentExecutor {
	t.Helper()
	reg := llm.NewSingleModelRegistry("test", "configured-model", "")
	reg.InjectProvider("test", "", "", provider)
	toolReg := tools.NewRegistry()
	if err := toolReg.Register(noopTool()); err != nil {
		t.Fatalf("register noop tool: %v", err)
	}
	return NewSubagentExecutor(reg, toolReg, nil)
}

// firstHumanMessage returns the first RoleHuman message in msgs, or nil.
func firstHumanMessage(msgs []models.Message) *models.Message {
	for i := range msgs {
		if msgs[i].Role == models.RoleHuman {
			return &msgs[i]
		}
	}
	return nil
}

// TestSubagentExecutor_ContextFiles_PrependedToFirstMessage is the RED test
// for M2-4: task.Config.ContextFiles must be read and prepended to the
// subagent's first human message as a <context-files> block, with the
// original prompt following it.
//
// RED signature (today): SubagentConfig has no ContextFiles field, so this
// doesn't compile until types.go is updated; once it does, Execute ignores
// the field entirely and the seeded message is just task.Prompt.
func TestSubagentExecutor_ContextFiles_PrependedToFirstMessage(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.go")
	fileB := filepath.Join(dir, "b.md")
	if err := os.WriteFile(fileA, []byte("package a\n"), 0o644); err != nil {
		t.Fatalf("write a.go: %v", err)
	}
	if err := os.WriteFile(fileB, []byte("# doc\n"), 0o644); err != nil {
		t.Fatalf("write b.md: %v", err)
	}

	provider := &scriptedOutputProvider{outputs: map[int]string{0: "done"}}
	exec := contextFilesExecutor(t, provider)

	_, err := exec.Execute(context.Background(),
		&subagent.Task{ID: "ctx1", Prompt: "review these files", Config: subagent.SubagentConfig{
			AgentType:    "general-purpose",
			Tools:        []string{"noop"},
			ContextFiles: []string{fileA, fileB},
		}},
		func(subagent.TaskEvent) {})
	if err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}

	human := firstHumanMessage(provider.messagesForCall(0))
	if human == nil {
		t.Fatalf("reqMsgs = %+v, want a human message", provider.messagesForCall(0))
	}
	content := human.Content
	if !strings.Contains(content, "<context-files>") || !strings.Contains(content, "</context-files>") {
		t.Fatalf("content = %q, want a <context-files> block", content)
	}
	if !strings.Contains(content, "## "+fileA) || !strings.Contains(content, "package a") {
		t.Fatalf("content = %q, want a section for %s", content, fileA)
	}
	if !strings.Contains(content, "## "+fileB) || !strings.Contains(content, "# doc") {
		t.Fatalf("content = %q, want a section for %s", content, fileB)
	}
	// Original prompt must follow the context block.
	blockEnd := strings.Index(content, "</context-files>")
	promptIdx := strings.Index(content, "review these files")
	if promptIdx < 0 || promptIdx < blockEnd {
		t.Fatalf("content = %q, want the original prompt after the context block", content)
	}
}

// TestSubagentExecutor_ContextFiles_PerFileCapTruncates is the RED test for
// M2-4: a file over the per-file 64KB cap must be truncated to the first
// 64KB with a truncation marker line, not fail the whole task.
func TestSubagentExecutor_ContextFiles_PerFileCapTruncates(t *testing.T) {
	dir := t.TempDir()
	bigFile := filepath.Join(dir, "big.txt")
	const size = contextFilePerFileCap + 1000
	if err := os.WriteFile(bigFile, bytes.Repeat([]byte("x"), size), 0o644); err != nil {
		t.Fatalf("write big.txt: %v", err)
	}

	provider := &scriptedOutputProvider{outputs: map[int]string{0: "done"}}
	exec := contextFilesExecutor(t, provider)

	_, err := exec.Execute(context.Background(),
		&subagent.Task{ID: "ctx2", Prompt: "go", Config: subagent.SubagentConfig{
			AgentType:    "general-purpose",
			Tools:        []string{"noop"},
			ContextFiles: []string{bigFile},
		}},
		func(subagent.TaskEvent) {})
	if err != nil {
		t.Fatalf("Execute() error = %v, want nil (over-cap truncates, doesn't fail)", err)
	}

	human := firstHumanMessage(provider.messagesForCall(0))
	if human == nil {
		t.Fatalf("reqMsgs = %+v, want a human message", provider.messagesForCall(0))
	}
	wantMarker := fmt.Sprintf("[truncated: %s is %d bytes, showing first %d]", bigFile, size, contextFilePerFileCap)
	if !strings.Contains(human.Content, wantMarker) {
		t.Fatalf("content = %q, want truncation marker %q", human.Content, wantMarker)
	}
	// Assert the file's content run is exactly capped, not just present:
	// look for a run of exactly contextFilePerFileCap consecutive x's, and
	// confirm one more x than that does NOT appear anywhere (a scattered 'x'
	// in e.g. the tempdir path is harmless to this check; a genuine
	// off-by-one in the truncation logic is not). Checking a bare
	// substring count of "x" would be fragile: t.TempDir() paths can contain
	// stray 'x' characters that have nothing to do with the file content.
	wantRun := strings.Repeat("x", contextFilePerFileCap)
	if !strings.Contains(human.Content, wantRun) {
		t.Fatalf("content missing a run of %d consecutive x's (the capped file content)", contextFilePerFileCap)
	}
	if strings.Contains(human.Content, wantRun+"x") {
		t.Fatalf("content contains a run of more than %d consecutive x's, want truncated exactly at the cap", contextFilePerFileCap)
	}
}

// TestSubagentExecutor_ContextFiles_PerFileCapTruncationIsRuneSafe is the RED
// test for the code-review nit: a raw content[:contextFilePerFileCap] byte
// slice can land in the middle of a multi-byte UTF-8 rune, producing invalid
// UTF-8 that reaches the provider as U+FFFD. The fix must use the package's
// existing rune-safe helper (truncateRuneSafe, aging.go) so the boundary
// backs up to the nearest rune start instead.
//
// The file is built so the per-file cap boundary lands exactly inside a
// multi-byte rune: contextFilePerFileCap-1 ASCII bytes, then a 3-byte rune
// ("日", occupying byte indices cap-1, cap, cap+1), then more filler so the
// file exceeds the cap and truncation triggers. A naive content[:cap] would
// therefore include only the rune's first byte — invalid UTF-8.
//
// RED signature (before the fix): the per-file cut was a raw byte slice
// (content[:contextFilePerFileCap]), so utf8.ValidString on the resulting
// message content is false for this file.
func TestSubagentExecutor_ContextFiles_PerFileCapTruncationIsRuneSafe(t *testing.T) {
	dir := t.TempDir()
	mbFile := filepath.Join(dir, "multibyte.txt")

	var buf bytes.Buffer
	buf.Write(bytes.Repeat([]byte("a"), contextFilePerFileCap-1))
	buf.WriteString("日") // 3-byte rune straddling the cap boundary
	buf.Write(bytes.Repeat([]byte("b"), 1000))
	if err := os.WriteFile(mbFile, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write multibyte.txt: %v", err)
	}

	provider := &scriptedOutputProvider{outputs: map[int]string{0: "done"}}
	exec := contextFilesExecutor(t, provider)

	_, err := exec.Execute(context.Background(),
		&subagent.Task{ID: "ctx-rune", Prompt: "go", Config: subagent.SubagentConfig{
			AgentType:    "general-purpose",
			Tools:        []string{"noop"},
			ContextFiles: []string{mbFile},
		}},
		func(subagent.TaskEvent) {})
	if err != nil {
		t.Fatalf("Execute() error = %v, want nil (over-cap truncates, doesn't fail)", err)
	}

	human := firstHumanMessage(provider.messagesForCall(0))
	if human == nil {
		t.Fatalf("reqMsgs = %+v, want a human message", provider.messagesForCall(0))
	}
	if !utf8.ValidString(human.Content) {
		t.Fatalf("content is not valid UTF-8: the per-file truncation split a multi-byte rune at the cap boundary:\n%q", human.Content)
	}
}

// TestSubagentExecutor_ContextFiles_TotalCapExceededFails is the RED test for
// M2-4: even though each file is under the per-file cap, if the SUM exceeds
// the 256KB total bundle cap, Execute must fail the task naming the
// offending file rather than silently dropping files.
func TestSubagentExecutor_ContextFiles_TotalCapExceededFails(t *testing.T) {
	dir := t.TempDir()
	const perFile = 60 * 1024 // under the 64KB per-file cap
	var paths []string
	for i := 0; i < 5; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%d.txt", i))
		if err := os.WriteFile(p, bytes.Repeat([]byte("y"), perFile), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		paths = append(paths, p)
	}
	// 5 * 60KB = 300KB > 256KB total cap; the 5th file (index 4) pushes it over.
	offending := paths[4]

	provider := &scriptedOutputProvider{outputs: map[int]string{0: "done"}}
	exec := contextFilesExecutor(t, provider)

	_, err := exec.Execute(context.Background(),
		&subagent.Task{ID: "ctx3", Prompt: "go", Config: subagent.SubagentConfig{
			AgentType:    "general-purpose",
			Tools:        []string{"noop"},
			ContextFiles: paths,
		}},
		func(subagent.TaskEvent) {})
	if err == nil {
		t.Fatal("Execute() error = nil, want the total bundle cap to trip")
	}
	if !strings.Contains(err.Error(), offending) {
		t.Fatalf("Execute() error = %q, want it to name the offending file %q", err.Error(), offending)
	}
	if provider.callCount() != 0 {
		t.Fatalf("provider callCount = %d, want 0 (task must fail before the subagent ever runs)", provider.callCount())
	}
}

// TestSubagentExecutor_ContextFiles_MissingFileFails is the RED test for
// M2-4: a nonexistent context file must fail the task with the path
// (surfacing the parent's mistake beats a subagent hallucinating around a
// missing file), not silently skip it.
func TestSubagentExecutor_ContextFiles_MissingFileFails(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.go")

	provider := &scriptedOutputProvider{outputs: map[int]string{0: "done"}}
	exec := contextFilesExecutor(t, provider)

	_, err := exec.Execute(context.Background(),
		&subagent.Task{ID: "ctx4", Prompt: "go", Config: subagent.SubagentConfig{
			AgentType:    "general-purpose",
			Tools:        []string{"noop"},
			ContextFiles: []string{missing},
		}},
		func(subagent.TaskEvent) {})
	if err == nil {
		t.Fatal("Execute() error = nil, want it to fail on the missing file")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("Execute() error = %q, want it to name the missing path %q", err.Error(), missing)
	}
	if provider.callCount() != 0 {
		t.Fatalf("provider callCount = %d, want 0 (task must fail before the subagent ever runs)", provider.callCount())
	}
}

// TestSubagentExecutor_ContextFiles_RelativePathResolvedAgainstWorkDir is the
// RED test for M2-4: a relative context_files path must resolve against
// e.workDir (set via WithWorkDir), the same convention initPlanFile uses for
// plan files.
func TestSubagentExecutor_ContextFiles_RelativePathResolvedAgainstWorkDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rel.txt"), []byte("relative content"), 0o644); err != nil {
		t.Fatalf("write rel.txt: %v", err)
	}

	provider := &scriptedOutputProvider{outputs: map[int]string{0: "done"}}
	exec := contextFilesExecutor(t, provider).WithWorkDir(dir)

	_, err := exec.Execute(context.Background(),
		&subagent.Task{ID: "ctx5", Prompt: "go", Config: subagent.SubagentConfig{
			AgentType:    "general-purpose",
			Tools:        []string{"noop"},
			ContextFiles: []string{"rel.txt"},
		}},
		func(subagent.TaskEvent) {})
	if err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}

	human := firstHumanMessage(provider.messagesForCall(0))
	if human == nil || !strings.Contains(human.Content, "relative content") {
		t.Fatalf("human message = %+v, want it to contain the relative file's content", human)
	}
}

// TestSubagentExecutor_ContextFiles_ComposesWithSchemaRetry is the RED test
// for M2-4's composition requirement: the context block is built once, at
// seed-message construction, and a schema-validation retry (M2-3's retry
// loop) reuses that SAME seeded message via result.Messages/appendParseError
// — it must not be dropped or rebuilt (and in particular must not be
// duplicated) on retry. Uses the security-reviewer builtin profile (the only
// way to reach a real Strict+MaxRetries OutputSchema, per the existing M2-3
// tests above) with an invalid first output to force exactly one retry.
func TestSubagentExecutor_ContextFiles_ComposesWithSchemaRetry(t *testing.T) {
	securityReviewerMaxRetries(t)
	dir := t.TempDir()
	ctxFile := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(ctxFile, []byte("reviewer notes content"), 0o644); err != nil {
		t.Fatalf("write notes.txt: %v", err)
	}

	provider := &scriptedOutputProvider{
		outputs: map[int]string{0: invalidReviewJSON, 1: validReviewJSON},
		usages: map[int]*llm.Usage{
			0: {InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
			1: {InputTokens: 20, OutputTokens: 10, TotalTokens: 30},
		},
	}
	exec := reviewerExecutor(t, provider)

	execResult, err := exec.Execute(context.Background(),
		&subagent.Task{ID: "ctx6", Prompt: "review this", Config: subagent.SubagentConfig{
			AgentType:    "security-reviewer",
			Tools:        []string{"noop"},
			ContextFiles: []string{ctxFile},
		}},
		func(subagent.TaskEvent) {})
	if err != nil {
		t.Fatalf("Execute() error = %v, want nil (retry should have produced valid output)", err)
	}
	if execResult.Result != validReviewJSON {
		t.Fatalf("Result = %q, want the valid retried output %q", execResult.Result, validReviewJSON)
	}

	// Both the initial call (idx 0) and the retry (idx 1) must carry the
	// context block on their first human message, verbatim and exactly once.
	for _, idx := range []int{0, 1} {
		human := firstHumanMessage(provider.messagesForCall(idx))
		if human == nil {
			t.Fatalf("call %d: reqMsgs = %+v, want a human message", idx, provider.messagesForCall(idx))
		}
		if got := strings.Count(human.Content, "<context-files>"); got != 1 {
			t.Fatalf("call %d: content has %d <context-files> openers, want exactly 1 (not dropped, not duplicated):\n%s", idx, got, human.Content)
		}
		if !strings.Contains(human.Content, "reviewer notes content") {
			t.Fatalf("call %d: content = %q, want the context file's content preserved through the retry", idx, human.Content)
		}
	}
}

// TestSubagentExecutor_ContextFiles_ThroughRealPool pins the whole
// task-tool-to-executor chain for context_files end-to-end: it goes through
// a REAL *subagent.Pool (NewSubagentPool→StartTask→Wait), not a direct
// exec.Execute call like the tests above. This catches a regression in
// pool.go's resolveConfig, which copies Tools/Model/TokenBudget/etc. from the
// caller's SubagentConfig onto the resolved config but historically forgot
// ContextFiles — silently dropping the task tool's context_files argument
// before it ever reached the executor, even though Execute itself fully
// supports it (as the tests above show in isolation).
func TestSubagentExecutor_ContextFiles_ThroughRealPool(t *testing.T) {
	dir := t.TempDir()
	ctxFile := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(ctxFile, []byte("pool notes content"), 0o644); err != nil {
		t.Fatalf("write notes.txt: %v", err)
	}

	provider := &scriptedOutputProvider{outputs: map[int]string{0: "done"}}
	exec := contextFilesExecutor(t, provider)
	pool := NewSubagentPool(exec, 1, time.Second)

	task, err := pool.StartTask(context.Background(), "test", "review", subagent.SubagentConfig{
		AgentType:    "general-purpose",
		Tools:        []string{"noop"},
		ContextFiles: []string{ctxFile},
	})
	if err != nil {
		t.Fatalf("StartTask() error = %v", err)
	}
	completed, err := pool.Wait(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if completed.Status != subagent.TaskStatusCompleted {
		t.Fatalf("status = %s, want %s", completed.Status, subagent.TaskStatusCompleted)
	}

	human := firstHumanMessage(provider.messagesForCall(0))
	if human == nil || !strings.Contains(human.Content, "pool notes content") {
		t.Fatalf("human message = %+v, want the context file's content injected through the real pool chain", human)
	}
}

// TestSubagentExecutor_SessionIsolation_NoBreakerCarryAcrossExecutes is the
// isolation guard for M4-3: AgentConfig.Session must stay nil for every
// subagent Agent (buildAgentConfig, above, never sets it — confirmed by
// `grep -n Session pkg/agent/subagent.go` returning nothing), so the
// repeat-call circuit breaker must start FRESH on every Execute call rather
// than carrying counters across them, unlike the REPL's carried session (see
// session_carry_test.go's TestSessionCarry_BreakerTripsAcrossRuns, which
// proves the OPPOSITE for an explicitly-shared *SessionCarry). Two Execute
// calls on the SAME executor, each driving 5 identical failing tool calls
// (5+5=10 >= maxRepeatFails=8), must BOTH complete without error: if breaker
// state ever leaked across Execute calls (e.g. a future regression added a
// persistent Session to SubagentExecutor), the second call's 5 failures
// would combine with the first's and trip fatally instead.
func TestSubagentExecutor_SessionIsolation_NoBreakerCarryAcrossExecutes(t *testing.T) {
	reg := llm.NewSingleModelRegistry("test", "configured-model", "")
	toolReg := tools.NewRegistry()
	registerSFailTool(t, toolReg)
	exec := NewSubagentExecutor(reg, toolReg, nil)

	for i := 0; i < 2; i++ {
		provider := &repeatFailProvider{maxCalls: 5}
		reg.InjectProvider("test", "", "", provider)
		_, err := exec.Execute(context.Background(),
			&subagent.Task{
				ID:     fmt.Sprintf("t%d", i),
				Prompt: "hi",
				Config: subagent.SubagentConfig{AgentType: "general-purpose", MaxTurns: 10, Tools: []string{"sfail"}},
			},
			func(subagent.TaskEvent) {})
		if err != nil {
			t.Fatalf("Execute #%d: unexpected error (5 identical failures alone must not trip a fresh breaker — "+
				"isolation broken if this only fails on the 2nd iteration): %v", i, err)
		}
	}
}
