package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
	builtin "github.com/millken/deepai/pkg/tools/builtin"
)

// --- M5 todo/plan tool: injection wiring -----------------------------------
//
// These tests guard the M5 design's core constraint: the current todo list
// must ride the per-Run TRAILING turn injection (buildTurnInjection/
// appendTurnInjection, same mechanism M4-2 built for date+memory), and must
// NEVER appear in the system prompt — the system prompt is the stable prefix
// every OpenAI-compat provider (DeepSeek/Qwen/GLM) prefix-caches, and the
// todo list's whole reason for existing is that it changes on every
// todo_write call. Baking it into position 0 would invalidate that cache on
// every single plan update, undoing the M4-2 work this repo already paid
// for. See TestTodoInjection_PresentAfterWriteNotInSystemPrompt.

// todoCaptureProvider drives a fixed sequence of tool calls (one per turn),
// then ends the run with a plain text reply, recording every Stream
// request's Messages — enough to inspect the turn injection across a
// multi-request Run. Modeled on injection_test.go's multiTurnCaptureProvider
// but with real per-turn tool-call Arguments (todo_write needs a "todos"
// argument shape that provider's fixed {"n": turn} can't express).
type todoCaptureProvider struct {
	mu       sync.Mutex
	requests [][]models.Message
	calls    []models.ToolCall
	turn     int
}

func (p *todoCaptureProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *todoCaptureProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.mu.Lock()
	snapshot := make([]models.Message, len(req.Messages))
	copy(snapshot, req.Messages)
	p.requests = append(p.requests, snapshot)
	turn := p.turn
	p.turn++
	p.mu.Unlock()

	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		if turn < len(p.calls) {
			ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, ToolCalls: []models.ToolCall{p.calls[turn]}}, Done: true}
			return
		}
		ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, Content: "done"}, Done: true}
	}()
	return ch, nil
}

func (p *todoCaptureProvider) seenRequests() [][]models.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]models.Message, len(p.requests))
	copy(out, p.requests)
	return out
}

// todoWriteCall builds a real todo_write ToolCall (same argument shape the
// model actually emits) with one item.
func todoWriteCall(id, content, status string) models.ToolCall {
	return models.ToolCall{
		ID:   id,
		Name: "todo_write",
		Arguments: map[string]any{
			"todos": []any{
				map[string]any{"content": content, "status": status},
			},
		},
	}
}

func newTodoRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	if err := reg.Register(builtin.TodoWriteTool()); err != nil {
		t.Fatalf("register todo_write tool: %v", err)
	}
	return reg
}

// TestTodoInjection_PresentAfterWriteNotInSystemPrompt is the RED test for
// the design's central constraint (see file doc comment above): after a
// todo_write call, the trailing turn injection of the VERY NEXT request in
// the SAME Run must contain the written item, and BuildSystemPrompt() must
// never contain it, no matter how many todo_write calls happen.
func TestTodoInjection_PresentAfterWriteNotInSystemPrompt(t *testing.T) {
	const marker = "IMPLEMENT_TODO_MARKER do the thing"
	p := &todoCaptureProvider{calls: []models.ToolCall{todoWriteCall("c1", marker, "in_progress")}}
	a := New(AgentConfig{LLMProvider: p, Tools: newTodoRegistry(t), Model: "m"})

	if _, err := a.Run(context.Background(), "sess-todo-inject", []models.Message{
		{Role: models.RoleHuman, Content: "please plan this"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	requests := p.seenRequests()
	if len(requests) != 2 {
		t.Fatalf("expected 2 requests (todo_write turn + final), got %d", len(requests))
	}

	before := requests[0][len(requests[0])-1].Content
	if strings.Contains(before, marker) {
		t.Fatalf("pre-write injection should not yet contain the todo, got: %q", before)
	}

	after := requests[1][len(requests[1])-1].Content
	if !strings.Contains(after, marker) {
		t.Fatalf("post-write injection (same Run, very next request) missing the todo — "+
			"todo_write must rebuild a.turnInjection immediately, like react.go's skill handling does. got: %q", after)
	}

	if prompt := a.BuildSystemPrompt(); strings.Contains(prompt, marker) {
		t.Fatalf("todo content leaked into the system prompt (breaks prefix caching): %q", prompt)
	}
}

// TestTodoInjection_RejectsMultipleInProgressLeavesStateUntouched drives a
// todo_write call with two in_progress items through a full Run (not just
// the handler in isolation — pkg/tools/builtin/todo_test.go already covers
// that) and checks the failure does not corrupt or partially apply state:
// the turn injection must still show nothing (empty plan), matching the
// pre-call state.
func TestTodoInjection_RejectsMultipleInProgressLeavesStateUntouched(t *testing.T) {
	badCall := models.ToolCall{
		ID:   "c1",
		Name: "todo_write",
		Arguments: map[string]any{
			"todos": []any{
				map[string]any{"content": "a", "status": "in_progress"},
				map[string]any{"content": "b", "status": "in_progress"},
			},
		},
	}
	p := &todoCaptureProvider{calls: []models.ToolCall{badCall}}
	a := New(AgentConfig{LLMProvider: p, Tools: newTodoRegistry(t), Model: "m"})

	if _, err := a.Run(context.Background(), "sess-todo-reject", []models.Message{
		{Role: models.RoleHuman, Content: "please plan this"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	requests := p.seenRequests()
	if len(requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(requests))
	}
	after := requests[1][len(requests[1])-1].Content
	if strings.Contains(after, "\"a\"") || strings.Contains(after, "[~] a") || strings.Contains(after, "[~] b") {
		t.Fatalf("a rejected todo_write call must not partially apply state, got injection: %q", after)
	}
	if len(a.todos) != 0 {
		t.Fatalf("a.todos = %+v, want empty after a rejected call", a.todos)
	}
}

// TestTodoInjection_EmptyListClearsPlan checks that resending an empty list
// (legal — see pkg/tools/builtin/todo_test.go) clears a previously written
// plan out of the injection, not just leaves it unchanged.
func TestTodoInjection_EmptyListClearsPlan(t *testing.T) {
	const marker = "TO_BE_CLEARED_MARKER"
	calls := []models.ToolCall{
		todoWriteCall("c1", marker, "pending"),
		{ID: "c2", Name: "todo_write", Arguments: map[string]any{"todos": []any{}}},
	}
	p := &todoCaptureProvider{calls: calls}
	a := New(AgentConfig{LLMProvider: p, Tools: newTodoRegistry(t), Model: "m"})

	if _, err := a.Run(context.Background(), "sess-todo-clear", []models.Message{
		{Role: models.RoleHuman, Content: "plan then clear"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	requests := p.seenRequests()
	if len(requests) != 3 {
		t.Fatalf("expected 3 requests (2 todo_write turns + final), got %d", len(requests))
	}
	afterWrite := requests[1][len(requests[1])-1].Content
	if !strings.Contains(afterWrite, marker) {
		t.Fatalf("expected the marker to be present after the first write, got: %q", afterWrite)
	}
	afterClear := requests[2][len(requests[2])-1].Content
	if strings.Contains(afterClear, marker) {
		t.Fatalf("empty todo_write should have cleared the plan out of the injection, got: %q", afterClear)
	}
}

// TestSessionCarry_TodosCarryAcrossRuns is the RED test for cross-Run
// carriage: like activeSkill/skillPrompt (session_carry.go), the todo list
// must survive the REPL's per-turn Agent churn (Agent is single-use) when
// the caller threads the same *SessionCarry through AgentConfig.Session.
func TestSessionCarry_TodosCarryAcrossRuns(t *testing.T) {
	const marker = "CARRY_ACROSS_RUNS_MARKER"
	session := NewSessionCarry()

	p1 := &todoCaptureProvider{calls: []models.ToolCall{todoWriteCall("c1", marker, "pending")}}
	a1 := New(AgentConfig{LLMProvider: p1, Tools: newTodoRegistry(t), Model: "m", Session: session})
	if _, err := a1.Run(context.Background(), "sess-todo-carry", []models.Message{
		{Role: models.RoleHuman, Content: "plan it"},
	}); err != nil {
		t.Fatalf("Run 1: %v", err)
	}

	if len(session.todos) != 1 || session.todos[0].Content != marker {
		t.Fatalf("session.todos after Run 1 = %+v, want one item with content %q", session.todos, marker)
	}

	// Run 2: a fresh Agent (single-use, like every REPL turn), same session,
	// no tool calls at all — the carried list must already be visible on
	// Run 2's very FIRST request, before any todo_write call happens in
	// Run 2 itself. Tools registers todo_write again, same as every real
	// REPL turn (registerChatTools re-registers it on every turn outside
	// plan mode) — review fix #2 gates formatTodoNote on hasTodoTool(), so a
	// registry that dropped the tool for reasons OTHER than plan mode is not
	// a scenario this carries a guarantee for.
	p2 := &todoCaptureProvider{}
	a2 := New(AgentConfig{LLMProvider: p2, Tools: newTodoRegistry(t), Model: "m", Session: session})
	if _, err := a2.Run(context.Background(), "sess-todo-carry", []models.Message{
		{Role: models.RoleHuman, Content: "hello again"},
	}); err != nil {
		t.Fatalf("Run 2: %v", err)
	}

	requests := p2.seenRequests()
	if len(requests) != 1 {
		t.Fatalf("expected exactly 1 request in Run 2, got %d", len(requests))
	}
	injection := requests[0][len(requests[0])-1].Content
	if !strings.Contains(injection, marker) {
		t.Fatalf("Run 2's first-request injection missing the carried todo, got: %q", injection)
	}
}

// TestTodoUsagePrompt_PresentWhenToolRegistered guards the OTHER half of the
// design split (see formatTodoNote's doc comment): the STATIC guidance on
// when to write/update a plan belongs in the system prompt (it never
// changes turn to turn, unlike the list itself), gated on tool presence
// exactly like hasAnyFileTool/hasSearchTools (systemprompt_test.go).
func TestTodoUsagePrompt_PresentWhenToolRegistered(t *testing.T) {
	a := New(AgentConfig{LLMProvider: &todoCaptureProvider{}, Tools: newTodoRegistry(t), Model: "m"})
	sp := a.BuildSystemPrompt()
	if !strings.Contains(sp, "todo_write") {
		t.Errorf("agent with todo_write registered should carry usage guidance mentioning it:\n%s", sp)
	}
}

// TestTodoUsagePrompt_CoversInitialStateAndGranularity is the RED test for
// review fix #4 (weak-model hardening, items 1-2): the review found
// todoUsagePrompt silent on two things a GLM-class model reliably gets wrong
// without explicit instruction —
//
//  1. what status the FIRST call's items should carry (the in_progress rule
//     only appeared in the "after finishing each step" sentence, so a weak
//     model tends to write everything "pending" and never mark anything
//     in_progress until told to finish something);
//  2. how many items is reasonable (no guidance at all invites either a
//     single mega-item or a 30-item list that then gets resent every turn).
func TestTodoUsagePrompt_CoversInitialStateAndGranularity(t *testing.T) {
	a := New(AgentConfig{LLMProvider: &todoCaptureProvider{}, Tools: newTodoRegistry(t), Model: "m"})
	sp := a.BuildSystemPrompt()
	if !strings.Contains(sp, "in_progress") || !strings.Contains(strings.ToLower(sp), "first") {
		t.Errorf("todoUsagePrompt should explicitly say the FIRST call marks its first item in_progress:\n%s", sp)
	}
	if !strings.Contains(sp, "3") || !strings.Contains(sp, "7") {
		t.Errorf("todoUsagePrompt should give a granularity target (roughly 3-7 items):\n%s", sp)
	}
}

// TestTodoUsagePrompt_DiscouragesClearingToSignalCompletion is the RED test
// for review fix #4, item 4: the OLD wording ("Pass an empty list to clear
// the plan", from the todo_write tool description, mirrored loosely in the
// usage prompt's framing) invites a model to clear the list on its LAST
// step to signal "done" — exactly the wrong habit, since a cleared list
// looks identical (from the injection's "" return) to a task that was never
// planned at all, losing the completion record. The prompt should instead
// say: mark every item done, don't clear the list.
func TestTodoUsagePrompt_DiscouragesClearingToSignalCompletion(t *testing.T) {
	a := New(AgentConfig{LLMProvider: &todoCaptureProvider{}, Tools: newTodoRegistry(t), Model: "m"})
	sp := a.BuildSystemPrompt()
	if !strings.Contains(sp, "done") {
		t.Errorf("todoUsagePrompt should tell the model to mark every item done on completion:\n%s", sp)
	}
	if !strings.Contains(strings.ToLower(sp), "not") && !strings.Contains(strings.ToLower(sp), "don't") {
		t.Errorf("todoUsagePrompt should explicitly discourage clearing the list to signal completion:\n%s", sp)
	}
}

// TestTodoNoteHeader_RequiresReconciliation is the RED test for review fix
// #4, item 3 — the one the review flagged as most important: the injected
// header re-asserts the plan on EVERY request (the strongest lever available
// for catching drift), but previously only told the model HOW to change the
// list, never WHEN. Without an explicit reconciliation instruction, a model
// that has silently started working on something other than the item marked
// in_progress has no prompted trigger to notice and self-correct.
func TestTodoNoteHeader_RequiresReconciliation(t *testing.T) {
	lower := strings.ToLower(todoNoteHeader)
	if !strings.Contains(lower, "match") && !strings.Contains(lower, "doesn't match") && !strings.Contains(lower, "does not match") {
		t.Errorf("todoNoteHeader should instruct the model to reconcile the list when its current work doesn't match the in_progress item, got: %q", todoNoteHeader)
	}
	if !strings.Contains(lower, "before continuing") && !strings.Contains(lower, "update the list") {
		t.Errorf("todoNoteHeader should tell the model to update the list BEFORE continuing when it drifts, got: %q", todoNoteHeader)
	}
}

// TestPlanMode_ExcludesTodoWrite is a regression guard, not a design change:
// pkg/agent/plan.go is explicitly out of scope for M5 (see its brief), and
// it doesn't need touching — enterPlanMode restricts to a fixed allowlist
// (planToolNames) that never named todo_write, so plan mode automatically
// excludes it (and, as a consequence, hasTodoTool()/todoUsagePrompt too)
// with zero code of this feature's own. This test exists so a future change
// to planToolNames that accidentally widens it to "everything except a
// denylist" gets caught here instead of silently reintroducing a second
// planning mechanism alongside write_plan/exit_plan_mode.
func TestPlanMode_ExcludesTodoWrite(t *testing.T) {
	a := New(AgentConfig{
		Tools:    newTodoRegistry(t),
		PlanMode: true,
	})
	if a.tools.Get("todo_write") != nil {
		t.Fatal("todo_write must not be available in plan mode")
	}
	if strings.Contains(a.BuildSystemPrompt(), "todo_write") {
		t.Fatal("plan-mode system prompt should not mention todo_write")
	}
}

// TestPlanMode_TurnInjectionExcludesLeftoverTodoList is the RED test for
// review fix #2: TestPlanMode_ExcludesTodoWrite above only checks that the
// STATIC usage guidance is gone from the system prompt — it says nothing
// about the LIST itself, which rides the separate turn injection
// (buildTurnInjection/formatTodoNote). Before this fix, formatTodoNote
// rendered a.todos unconditionally, so a plan carried over from BEFORE plan
// mode was entered (a.todos primed from cfg.Session.todos at New(), same as
// TestSessionCarry_TodosCarryAcrossRuns exercises) kept showing up in every
// request's trailing injection even though todo_write itself had been
// removed from the tool registry and the "call todo_write to change it"
// instruction in the header could no longer be followed — a stale list the
// model has no legal way to update or clear.
func TestPlanMode_TurnInjectionExcludesLeftoverTodoList(t *testing.T) {
	const marker = "PLANMODE_LEFTOVER"
	a := New(AgentConfig{Tools: newTodoRegistry(t), PlanMode: true})
	a.todos = []builtin.TodoItem{{Content: marker, Status: builtin.TodoInProgress}}

	injection := a.buildTurnInjection(context.Background(), "sess-planmode-leftover", nil)
	if strings.Contains(injection.Content, marker) {
		t.Fatalf("plan-mode turn injection must not assert a todo list for a tool that was removed, got: %q", injection.Content)
	}
	if strings.Contains(injection.Content, todoNoteHeader) {
		t.Fatalf("plan-mode turn injection must not carry the todo note header at all, got: %q", injection.Content)
	}
}

// TestTodoUsagePrompt_OmittedWithoutTool mirrors
// TestT5c_FileOpRuleOmittedWithoutFileTools: an agent that never gets
// todo_write registered (e.g. a role deliberately excluded from it) must not
// carry dead instructions referencing a tool it doesn't have.
func TestTodoUsagePrompt_OmittedWithoutTool(t *testing.T) {
	a := New(AgentConfig{LLMProvider: &todoCaptureProvider{}, Tools: tools.NewRegistry(), Model: "m"})
	sp := a.BuildSystemPrompt()
	if strings.Contains(sp, "todo_write") {
		t.Errorf("agent without todo_write should not carry its usage guidance:\n%s", sp)
	}
}
