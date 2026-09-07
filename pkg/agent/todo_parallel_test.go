package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// --- Review fix #1: todo_write / skill must not be dropped when they land
// inside a PARALLEL tool batch ---------------------------------------------
//
// hasParallelRun (react.go) is a whole-BATCH decision: a batch is only
// dispatched through the parallel path when it contains a run of 2+
// CONSECUTIVE ParallelSafe calls somewhere in it. partitionToolCalls then
// carves the batch into segments — a ParallelSafe:false call like
// "todo_write" or "skill" becomes its own serial SEGMENT, but that segment
// still executes INSIDE the parallel dispatch path (react.go's `if
// hasParallelRun { ... }` branch), never the separate serial dispatch path
// below it. Before this fix, only the serial path special-cased
// result.ToolName == "skill"/"todo_write"; the parallel path's observation
// loop just called batch.handleResult with no knowledge of either tool. A
// batch shaped like [todo_write, safe, safe] — "mark step 2 done, then read
// these 3 files", the exact shape todoUsagePrompt/delegationStrategy
// encourage — silently discarded the todo_write result: a.todos (and, worse,
// an EXISTING plan) never updated, with no error surfaced anywhere.
//
// batchedToolCallProvider drives a scripted sequence of assistant turns, one
// []models.ToolCall PER TURN (so a single turn can carry a multi-call
// PARALLEL batch), then ends the run with plain text. It records every
// Stream request's Messages, like todoCaptureProvider/multiTurnCaptureProvider,
// so the trailing turn injection can be inspected across the whole Run.
type batchedToolCallProvider struct {
	mu       sync.Mutex
	requests [][]models.Message
	batches  [][]models.ToolCall
	turn     int
}

func (p *batchedToolCallProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *batchedToolCallProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
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
		if turn < len(p.batches) {
			ch <- llm.StreamChunk{ToolCalls: p.batches[turn], Stop: "tool_calls", Done: true}
			return
		}
		ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, Content: "done"}, Done: true}
	}()
	return ch, nil
}

func (p *batchedToolCallProvider) seenRequests() [][]models.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]models.Message, len(p.requests))
	copy(out, p.requests)
	return out
}

// safeReadTool is a benign ParallelSafe stand-in for read_file/grep/etc — any
// tool with ParallelSafe:true works to force the batch through the parallel
// dispatch path (react.go requires 2+ CONSECUTIVE ParallelSafe calls to set
// hasParallelRun).
func safeReadTool() models.Tool {
	return models.Tool{
		Name:         "safe_read",
		Description:  "benign parallel-safe stand-in for read_file/grep",
		ParallelSafe: true,
		InputSchema:  map[string]any{"type": "object"},
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{CallID: call.ID, ToolName: call.Name, Content: "read:" + call.ID}, nil
		},
	}
}

func safeReadCall(id string) models.ToolCall {
	return models.ToolCall{ID: id, Name: "safe_read", Arguments: map[string]any{}}
}

// fakeSkillTool mirrors the stub used by
// TestTurnInjection_RecomputesFenceOnSkillLoadMidRun (injection_test.go):
// a "skill" tool that returns the Data shape react.go's applySkillResult
// consumes, without depending on the real pkg/skill package.
func fakeSkillTool(name, body string) models.Tool {
	return models.Tool{
		Name:        "skill",
		Description: "load a skill",
		InputSchema: map[string]any{"type": "object"},
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{
				CallID:   call.ID,
				ToolName: call.Name,
				Content:  "loaded",
				Data: map[string]any{
					"system_prompt": body,
					"skill_name":    name,
				},
			}, nil
		},
	}
}

func skillCall(id, name string) models.ToolCall {
	return models.ToolCall{ID: id, Name: "skill", Arguments: map[string]any{"name": name}}
}

// TestTodoWriteInParallelBatch_NotDiscarded is the RED test for the primary
// bug: a batch of [todo_write, safe_read, safe_read] — 2+ trailing
// ParallelSafe calls force the WHOLE batch through the parallel dispatch
// path — must still apply the todo_write result. Before the fix, a.todos
// stayed empty and the write vanished with no error.
func TestTodoWriteInParallelBatch_NotDiscarded(t *testing.T) {
	const marker = "PARALLEL_BATCH_TODO_MARKER"
	reg := newTodoRegistry(t)
	if err := reg.Register(safeReadTool()); err != nil {
		t.Fatal(err)
	}

	batch := []models.ToolCall{
		todoWriteCall("c1", marker, "in_progress"),
		safeReadCall("r1"),
		safeReadCall("r2"),
	}
	p := &batchedToolCallProvider{batches: [][]models.ToolCall{batch}}
	session := NewSessionCarry()
	a := New(AgentConfig{LLMProvider: p, Tools: reg, Model: "m", Session: session})

	if _, err := a.Run(context.Background(), "sess-parallel-todo", []models.Message{
		{Role: models.RoleHuman, Content: "mark step done, then read these files"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(a.todos) != 1 || a.todos[0].Content != marker {
		t.Fatalf("a.todos = %+v, want one item with content %q — the write was discarded", a.todos, marker)
	}
	if len(session.todos) != 1 || session.todos[0].Content != marker {
		t.Fatalf("session.todos = %+v, want one item with content %q — carry never received it either", session.todos, marker)
	}

	requests := p.seenRequests()
	if len(requests) != 2 {
		t.Fatalf("expected 2 requests (parallel batch turn + final), got %d", len(requests))
	}
	after := requests[1][len(requests[1])-1].Content
	if !strings.Contains(after, marker) {
		t.Fatalf("post-batch injection (same Run, very next request) missing the todo — "+
			"todo_write inside a parallel batch must still rebuild a.turnInjection. got: %q", after)
	}
}

// TestTodoWriteInParallelBatch_StaleExistingPlanReplaced is the RED test for
// the worse sub-case the review flagged: when a plan ALREADY exists and its
// update lands inside a parallel batch, a.todos must not keep asserting the
// STALE plan — that is actively worse than no plan tool at all, since the
// injection would keep re-asserting drift instead of the model's correction.
func TestTodoWriteInParallelBatch_StaleExistingPlanReplaced(t *testing.T) {
	const stale = "STALE_PLAN_ITEM"
	const fresh = "FRESH_PLAN_ITEM"
	reg := newTodoRegistry(t)
	if err := reg.Register(safeReadTool()); err != nil {
		t.Fatal(err)
	}

	batches := [][]models.ToolCall{
		// Turn 0: establish a plan via a plain serial call.
		{todoWriteCall("c0", stale, "in_progress")},
		// Turn 1: a parallel batch that REPLACES the plan.
		{todoWriteCall("c1", fresh, "in_progress"), safeReadCall("r1"), safeReadCall("r2")},
	}
	p := &batchedToolCallProvider{batches: batches}
	a := New(AgentConfig{LLMProvider: p, Tools: reg, Model: "m"})

	if _, err := a.Run(context.Background(), "sess-parallel-stale", []models.Message{
		{Role: models.RoleHuman, Content: "plan, then update it via a parallel batch"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(a.todos) != 1 || a.todos[0].Content != fresh {
		t.Fatalf("a.todos = %+v, want exactly the fresh item %q — the parallel-batch update was dropped, "+
			"leaving the STALE plan in place", a.todos, fresh)
	}

	requests := p.seenRequests()
	if len(requests) != 3 {
		t.Fatalf("expected 3 requests (2 tool turns + final), got %d", len(requests))
	}
	final := requests[2][len(requests[2])-1].Content
	if strings.Contains(final, stale) {
		t.Fatalf("final injection still asserts the STALE plan item: %q", final)
	}
	if !strings.Contains(final, fresh) {
		t.Fatalf("final injection missing the FRESH plan item: %q", final)
	}
}

// TestSkillLoadInParallelBatch_NotDiscarded is the equivalent RED test for
// the "skill" tool, which the review confirmed has the identical exposure:
// a skill load landing inside a parallel batch must still fold its body into
// the system prompt and update ActiveSkill/appliedSkillPrompt.
func TestSkillLoadInParallelBatch_NotDiscarded(t *testing.T) {
	const skillName = "target"
	const skillBody = "TARGET_SKILL_BODY_MARKER"
	reg := tools.NewRegistry()
	if err := reg.Register(fakeSkillTool(skillName, skillBody)); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(safeReadTool()); err != nil {
		t.Fatal(err)
	}

	batch := []models.ToolCall{
		skillCall("c1", skillName),
		safeReadCall("r1"),
		safeReadCall("r2"),
	}
	p := &batchedToolCallProvider{batches: [][]models.ToolCall{batch}}
	a := New(AgentConfig{LLMProvider: p, Tools: reg, Model: "m"})

	if _, err := a.Run(context.Background(), "sess-parallel-skill", []models.Message{
		{Role: models.RoleHuman, Content: "load the skill and read these files"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if a.ActiveSkill() != skillName {
		t.Fatalf("a.ActiveSkill() = %q, want %q — skill load inside a parallel batch was discarded", a.ActiveSkill(), skillName)
	}
	if a.appliedSkillPrompt != skillBody {
		t.Fatalf("a.appliedSkillPrompt = %q, want %q", a.appliedSkillPrompt, skillBody)
	}
	if !strings.Contains(a.systemPrompt, skillBody) {
		t.Fatalf("skill body never appended to the system prompt: %q", a.systemPrompt)
	}
}

// TestTodoWriteInParallelBatch_ValidationFailureLeavesStateUntouched is a
// companion sanity check: a REJECTED todo_write (two in_progress items)
// landing inside a parallel batch must leave a.todos untouched, exactly like
// the serial-path equivalent in todo_test.go — the fix must not start
// applying Data on a call that returned a validation error (no Data at all).
func TestTodoWriteInParallelBatch_ValidationFailureLeavesStateUntouched(t *testing.T) {
	reg := newTodoRegistry(t)
	if err := reg.Register(safeReadTool()); err != nil {
		t.Fatal(err)
	}
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
	batch := []models.ToolCall{badCall, safeReadCall("r1"), safeReadCall("r2")}
	p := &batchedToolCallProvider{batches: [][]models.ToolCall{batch}}
	a := New(AgentConfig{LLMProvider: p, Tools: reg, Model: "m"})

	if _, err := a.Run(context.Background(), "sess-parallel-reject", []models.Message{
		{Role: models.RoleHuman, Content: "go"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(a.todos) != 0 {
		t.Fatalf("a.todos = %+v, want empty after a rejected call inside a parallel batch", a.todos)
	}
}
