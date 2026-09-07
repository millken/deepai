package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/clarification"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
	"github.com/millken/deepai/pkg/tools/builtin"
)

// --- M6 latency: batch-independent-tool-calls guidance ---------------------
//
// The real-world eval (glm-5.3, 45 runs) found per-turn latency dominated by
// model generation (46-48s median) while tool execution itself is
// millisecond-cheap, and the median tool-calls-per-turn was 1.50 and NEVER
// exceeded 2.00 across 45 runs — the harness (partitionToolCalls,
// pkg/agent/toolexec.go) already supports an unbounded number of
// parallel-safe calls in one assistant message, but nothing in the system
// prompt ever told the model that batching independent calls into one
// message is allowed, let alone cheaper. These tests pin the fix: a static
// system-prompt section teaching the model when to batch (independent calls)
// and when not to (dependent calls), gated on having at least two
// ParallelSafe tools registered (a single parallel-safe tool has nothing to
// batch with; zero has nothing at all).

// parallelSafeToolForTest returns a minimal registerable tool with the given
// name and ParallelSafe value, standing in for a real builtin (read_file,
// grep, etc.) without importing the builtin package.
func parallelSafeToolForTest(name string, parallelSafe bool) models.Tool {
	return models.Tool{
		Name:         name,
		ParallelSafe: parallelSafe,
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{}, nil
		},
	}
}

// TestBatchGuidance_PresentWithTwoParallelSafeTools: an agent with 2+
// registered ParallelSafe tools (e.g. read_file + grep) gets the batching
// guidance in its system prompt.
func TestBatchGuidance_PresentWithTwoParallelSafeTools(t *testing.T) {
	reg := tools.NewRegistry()
	_ = reg.Register(parallelSafeToolForTest("read_file", true))
	_ = reg.Register(parallelSafeToolForTest("grep", true))
	a := New(AgentConfig{LLMProvider: &captureProvider{}, Tools: reg, Model: "m"})

	sp := a.BuildSystemPrompt()
	if !strings.Contains(sp, batchToolCallsPrompt) {
		t.Errorf("system prompt missing batch-tool-calls guidance with 2 ParallelSafe tools registered:\n%s", sp)
	}
}

// TestBatchGuidance_SaysBatchingEditsIsFine: the original wording's only
// edit-shaped example was the DEPENDENT case (read_file then edit_file on
// unconfirmed text), with no example of the mutating batching that actually
// saves turns — editing several different files, or making several already-
// known edits to one file, in a single message. A weak model reading only
// the negative example could easily conclude edits must never be batched,
// when partitionToolCalls (toolexec.go) in fact runs them sequentially
// within the same turn/round-trip, which is exactly what makes it safe.
// This pins that the prompt now states the positive case explicitly.
func TestBatchGuidance_SaysBatchingEditsIsFine(t *testing.T) {
	if !strings.Contains(batchToolCallsPrompt, "editing several different files") {
		t.Errorf("batchToolCallsPrompt should explicitly say batching edits/writes across files is fine, not just reads/greps:\n%s", batchToolCallsPrompt)
	}
}

// TestBatchGuidance_WarnsAgainstSpeculativeBatching: the risk flagged during
// review — a weak model that over-internalizes "batch independent calls"
// could guess a large batch of unrelated reads instead of narrowing first,
// trading fewer turns for a context-budget regression. The prompt must tell
// the model to batch only once it already knows what it needs, and to use a
// single grep/glob to find out first when it doesn't.
func TestBatchGuidance_WarnsAgainstSpeculativeBatching(t *testing.T) {
	if !strings.Contains(batchToolCallsPrompt, "don't batch on a guess") {
		t.Errorf("batchToolCallsPrompt should warn against speculative/guessed batching:\n%s", batchToolCallsPrompt)
	}
}

// TestBatchGuidance_AbsentWithZeroParallelSafeTools: an agent with no
// ParallelSafe tools at all (e.g. only bash, mutating) must not carry the
// guidance — there is nothing to batch.
func TestBatchGuidance_AbsentWithZeroParallelSafeTools(t *testing.T) {
	reg := tools.NewRegistry()
	_ = reg.Register(parallelSafeToolForTest("bash", false))
	_ = reg.Register(parallelSafeToolForTest("edit_file", false))
	a := New(AgentConfig{LLMProvider: &captureProvider{}, Tools: reg, Model: "m"})

	sp := a.BuildSystemPrompt()
	if strings.Contains(sp, batchToolCallsPrompt) {
		t.Errorf("system prompt should not carry batch-tool-calls guidance with 0 ParallelSafe tools:\n%s", sp)
	}
}

// TestBatchGuidance_AbsentWithOneParallelSafeTool: exactly one ParallelSafe
// tool means there is nothing else to batch it with — the guidance would be
// pure noise.
func TestBatchGuidance_AbsentWithOneParallelSafeTool(t *testing.T) {
	reg := tools.NewRegistry()
	_ = reg.Register(parallelSafeToolForTest("read_file", true))
	_ = reg.Register(parallelSafeToolForTest("bash", false))
	a := New(AgentConfig{LLMProvider: &captureProvider{}, Tools: reg, Model: "m"})

	sp := a.BuildSystemPrompt()
	if strings.Contains(sp, batchToolCallsPrompt) {
		t.Errorf("system prompt should not carry batch-tool-calls guidance with only 1 ParallelSafe tool:\n%s", sp)
	}
}

// TestBatchGuidance_TaskIsNotMisclassifiedAsSerial: the "independent mutating
// calls (edit_file, write_file, bash, ...)" sentence classifies tools along
// a read-only/mutating axis that does NOT match the axis the code actually
// uses (ParallelSafe, pkg/tools/subagent.go:48 sets task's ParallelSafe to
// true even though task plainly mutates — a sub-agent it delegates to can
// edit files or run git). The open-ended "..." after bash invites a weak
// model to bucket task under "mutating calls (... run one at a time)"
// because task fits that description by any natural reading, giving it a
// false mechanistic belief: that two task calls batched together run
// serially, each seeing the previous one's on-disk effect. In truth
// react.go's partitionToolCalls fuses consecutive ParallelSafe calls
// (including task) into one concurrent segment — see react.go:845-853's
// "ParallelSafe is a handler-level thread-safety promise, not a
// side-effect-freedom promise". This also collides with the SAME system
// prompt's "## Parallel delegation" section (~190 lines away), which
// requires SERIAL task calls when one depends on another's result. Pin that
// the prompt explicitly carves task out of the mutating-calls bucket instead
// of leaving it to be inferred (or misinferred) from the axis mismatch.
func TestBatchGuidance_TaskIsNotMisclassifiedAsSerial(t *testing.T) {
	if !strings.Contains(batchToolCallsPrompt, "task is the exception") {
		t.Errorf("batchToolCallsPrompt must explicitly except task from the mutating-calls-run-serially claim (task is ParallelSafe: true and runs concurrently like any other batched ParallelSafe call, contradicting the read-only/mutating framing this sentence otherwise uses):\n%s", batchToolCallsPrompt)
	}
}

// TestBatchGuidance_PresentInPlanMode: plan mode (plan.go's enterPlanMode)
// restricts the tool set to a read-only allowlist (read_file, grep, glob,
// list_dir, find, code_map, ...) that itself contains several ParallelSafe
// tools — exploration is exactly when batching independent reads matters
// most, so the guidance must survive the restriction, not just the
// unrestricted case.
func TestBatchGuidance_PresentInPlanMode(t *testing.T) {
	reg := tools.NewRegistry()
	for _, name := range []string{"read_file", "list_dir", "glob", "grep", "find", "code_map"} {
		_ = reg.Register(parallelSafeToolForTest(name, true))
	}
	for _, name := range []string{"ask_clarification", "present_file", "bash", "write_file", "edit_file"} {
		_ = reg.Register(parallelSafeToolForTest(name, false))
	}
	a := New(AgentConfig{
		LLMProvider: &captureProvider{},
		Tools:       reg,
		Model:       "m",
		PlanMode:    true,
	})

	sp := a.BuildSystemPrompt()
	if !strings.Contains(sp, batchToolCallsPrompt) {
		t.Errorf("plan-mode system prompt missing batch-tool-calls guidance despite several ParallelSafe tools surviving the restriction:\n%s", sp)
	}
}

// TestPlanToolAllowlist_HasMultipleRealParallelSafeTools: the test above
// (TestBatchGuidance_PresentInPlanMode) proves the WIRING — that
// hasMultipleParallelSafeTools's answer flows into BuildSystemPrompt — but
// it does so with hand-built fake tools that assign ParallelSafe: true to
// EVERY name in planToolNames by construction. That only pins that
// RestrictTo preserves the ParallelSafe field it's handed; it says nothing
// about whether the REAL plan-mode allowlist (plan.go's planToolNames) truly
// contains 2+ real parallel-safe builtin tools. If every real tool in that
// allowlist were ever changed to ParallelSafe: false, this fake-tool test
// would keep passing while the guidance became a lie in production. Build
// the plan-mode registry out of the actual builtin constructors (the same
// ones enterPlanMode restricts in production) and assert the invariant
// directly on the real tool set.
func TestPlanToolAllowlist_HasMultipleRealParallelSafeTools(t *testing.T) {
	full := tools.NewRegistry()
	realTools := []models.Tool{
		// planToolNames members:
		builtin.ReadFileTool(),
		builtin.ListDirTool(),
		builtin.GlobTool(),
		builtin.GrepTool(),
		builtin.FindTool(),
		builtin.CodeMapTool(),
		clarification.AskClarificationToolWithMode(false),
		tools.PresentFileTool(tools.NewPresentFileRegistry()),
		// Non-allowlisted tools a real full agent registry also carries, to
		// mirror what RestrictTo actually strips away in enterPlanMode.
		builtin.BashTool(),
		builtin.WriteFileTool(),
		builtin.EditFileTool(),
	}
	for _, tool := range realTools {
		if err := full.Register(tool); err != nil {
			t.Fatalf("register %s: %v", tool.Name, err)
		}
	}

	restricted := full.RestrictTo(planToolNames)

	n := 0
	for _, tool := range restricted.List() {
		if tool.ParallelSafe {
			n++
		}
	}
	if n < 2 {
		t.Fatalf("plan-mode allowlist (planToolNames=%v) has only %d real ParallelSafe tool(s) after RestrictTo; batchToolCallsPrompt needs >=2 to earn its place in the plan-mode system prompt", planToolNames, n)
	}
}

// TestBatchGuidance_AbsentInPlanModeWhenAllowlistLacksParallelSafeTools is
// the mutation-catching counterpart to TestBatchGuidance_PresentInPlanMode.
// The variant under review made hasMultipleParallelSafeTools read
// a.fullTools (the UNRESTRICTED tool set) whenever it is non-nil, instead of
// a.tools (the set actually in play, restricted in plan mode) — and every
// prior test still passed, because every prior fake-tool fixture gave the
// restricted and unrestricted sets the exact same ParallelSafe answer, so
// the two code paths were indistinguishable.
//
// This fixture breaks that symmetry on purpose: only ONE tool inside the
// plan allowlist is ParallelSafe (read_file), while two tools OUTSIDE the
// allowlist (web_search, web_fetch — stripped by RestrictTo when entering
// plan mode, mirroring real tools that plan mode never grants) are
// ParallelSafe. The restricted set therefore has 1 ParallelSafe tool (no
// guidance should be injected); the unrestricted a.fullTools has 3 (a
// fullTools-reading gate would wrongly inject it). A gate that reads the
// wrong tool set fails this test — see this file's mutation-recreation note
// in the PR/report for the reproduction (revert hasMultipleParallelSafeTools
// to prefer a.fullTools, confirm this test goes red, then restore it).
func TestBatchGuidance_AbsentInPlanModeWhenAllowlistLacksParallelSafeTools(t *testing.T) {
	reg := tools.NewRegistry()
	// Inside planToolNames: only read_file is ParallelSafe.
	_ = reg.Register(parallelSafeToolForTest("read_file", true))
	for _, name := range []string{"list_dir", "glob", "grep", "find", "code_map", "ask_clarification", "present_file"} {
		_ = reg.Register(parallelSafeToolForTest(name, false))
	}
	// Outside planToolNames: ParallelSafe tools that RestrictTo strips away
	// when entering plan mode (mirrors real tools like web_search/web_fetch,
	// which plan mode never grants).
	_ = reg.Register(parallelSafeToolForTest("web_search", true))
	_ = reg.Register(parallelSafeToolForTest("web_fetch", true))
	_ = reg.Register(parallelSafeToolForTest("bash", false))
	_ = reg.Register(parallelSafeToolForTest("write_file", false))
	_ = reg.Register(parallelSafeToolForTest("edit_file", false))

	a := New(AgentConfig{
		LLMProvider: &captureProvider{},
		Tools:       reg,
		Model:       "m",
		PlanMode:    true,
	})

	sp := a.BuildSystemPrompt()
	if strings.Contains(sp, batchToolCallsPrompt) {
		t.Errorf("plan-mode system prompt must NOT carry batch-tool-calls guidance when the RESTRICTED (plan) tool set has fewer than 2 ParallelSafe tools, even though the unrestricted full set has 3 — this means the gate is reading a.fullTools instead of a.tools:\n%s", sp)
	}
}

// TestBatchGuidance_NotInTurnInjection: this is STATIC guidance (whether to
// batch never varies turn to turn) and must ride the stable system prompt,
// never buildTurnInjection — putting it there would defeat the M4-2 prefix
// cache for the same reason formatTodoNote's doc comment explains the todo
// list itself can't live in BuildSystemPrompt.
func TestBatchGuidance_NotInTurnInjection(t *testing.T) {
	reg := tools.NewRegistry()
	_ = reg.Register(parallelSafeToolForTest("read_file", true))
	_ = reg.Register(parallelSafeToolForTest("grep", true))
	a := New(AgentConfig{LLMProvider: &captureProvider{}, Tools: reg, Model: "m"})

	injection := a.buildTurnInjection(context.Background(), "sess", nil)
	if strings.Contains(injection.Content, batchToolCallsPrompt) {
		t.Errorf("buildTurnInjection must never carry the static batch-tool-calls guidance:\n%s", injection.Content)
	}
}

// TestBatchGuidance_SystemPromptByteStableAcrossRequests pins the M4-2
// prefix-caching guarantee the same way todo_prefix_test.go does for the
// todo tool: with the guidance gated in, the system prompt sent on every
// request of a multi-turn Run must be byte-identical.
func TestBatchGuidance_SystemPromptByteStableAcrossRequests(t *testing.T) {
	reg := tools.NewRegistry()
	_ = reg.Register(parallelSafeToolForTest("read_file", true))
	_ = reg.Register(parallelSafeToolForTest("grep", true))

	log := &prefixRequestLog{}
	p := &prefixBatchProvider{log: log, batches: [][]models.ToolCall{
		{{ID: "c1", Name: "read_file", Arguments: map[string]any{}}},
		{{ID: "c2", Name: "grep", Arguments: map[string]any{}}},
	}}
	a := New(AgentConfig{LLMProvider: p, Tools: reg, Model: "m"})
	if _, err := a.Run(context.Background(), "sess-batch-stable", []models.Message{
		{Role: models.RoleHuman, Content: "hello"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	requests := log.snapshot()
	if len(requests) < 2 {
		t.Fatalf("expected at least 2 requests, got %d", len(requests))
	}
	for i := 1; i < len(requests); i++ {
		if requests[i].systemPrompt != requests[0].systemPrompt {
			t.Fatalf("request %d system prompt differs from request 0's — prefix cache would be invalidated:\n0: %q\n%d: %q",
				i, requests[0].systemPrompt, i, requests[i].systemPrompt)
		}
	}
	if !strings.Contains(requests[0].systemPrompt, batchToolCallsPrompt) {
		t.Fatalf("system prompt should contain the batch guidance:\n%s", requests[0].systemPrompt)
	}
}
