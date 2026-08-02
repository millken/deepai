package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/subagent"
)

type fakeTaskPool struct {
	startTask func(ctx context.Context, description, prompt string, cfg subagent.SubagentConfig) (*subagent.Task, error)
	wait      func(ctx context.Context, taskID string) (*subagent.Task, error)
}

func (f fakeTaskPool) StartTask(ctx context.Context, description, prompt string, cfg subagent.SubagentConfig) (*subagent.Task, error) {
	return f.startTask(ctx, description, prompt, cfg)
}

func (f fakeTaskPool) Wait(ctx context.Context, taskID string) (*subagent.Task, error) {
	return f.wait(ctx, taskID)
}

func TestTaskToolCompleted(t *testing.T) {
	tool := TaskTool(fakeTaskPool{
		startTask: func(ctx context.Context, description, prompt string, cfg subagent.SubagentConfig) (*subagent.Task, error) {
			if cfg.AgentType != "bash" {
				t.Fatalf("cfg.AgentType = %s, want bash", cfg.AgentType)
			}
			if cfg.MaxTurns != 3 {
				t.Fatalf("cfg.MaxTurns = %d, want 3", cfg.MaxTurns)
			}
			return &subagent.Task{ID: "task-1"}, nil
		},
		wait: func(ctx context.Context, taskID string) (*subagent.Task, error) {
			return &subagent.Task{ID: taskID, Status: subagent.TaskStatusCompleted, Result: "ok"}, nil
		},
	}, nil)

	result, err := tool.Handler(context.Background(), models.ToolCall{
		ID:   "call-1",
		Name: "task",
		Arguments: map[string]any{
			"description":   "run shell",
			"prompt":        "echo hi",
			"subagent_type": "bash",
			"max_turns":     3.0,
		},
	})
	if err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	if result.Content != "ok" {
		t.Fatalf("content = %q, want ok", result.Content)
	}
}

func TestTaskToolFailed(t *testing.T) {
	tool := TaskTool(fakeTaskPool{
		startTask: func(ctx context.Context, description, prompt string, cfg subagent.SubagentConfig) (*subagent.Task, error) {
			return &subagent.Task{ID: "task-2"}, nil
		},
		wait: func(ctx context.Context, taskID string) (*subagent.Task, error) {
			return &subagent.Task{ID: taskID, Status: subagent.TaskStatusFailed, Error: "boom"}, nil
		},
	}, nil)

	result, err := tool.Handler(context.Background(), models.ToolCall{
		ID:        "call-2",
		Name:      "task",
		Arguments: map[string]any{"description": "bad", "prompt": "fail"},
	})
	if err == nil {
		t.Fatal("Handler() expected error")
	}
	if result.Error != "boom" {
		t.Fatalf("error = %q, want boom", result.Error)
	}
}

func TestTaskToolCancelled(t *testing.T) {
	tool := TaskTool(fakeTaskPool{
		startTask: func(ctx context.Context, description, prompt string, cfg subagent.SubagentConfig) (*subagent.Task, error) {
			return &subagent.Task{ID: "task-3"}, nil
		},
		wait: func(ctx context.Context, taskID string) (*subagent.Task, error) {
			return &subagent.Task{ID: taskID, Status: subagent.TaskStatusCancelled, Error: "context canceled"}, nil
		},
	}, nil)

	result, err := tool.Handler(context.Background(), models.ToolCall{
		ID:        "call-3",
		Name:      "task",
		Arguments: map[string]any{"description": "cancel me", "prompt": "do work"},
	})
	if err == nil {
		t.Fatal("Handler() expected error for cancelled task")
	}
	if result.Status != models.CallStatusFailed {
		t.Fatalf("status = %s, want %s", result.Status, models.CallStatusFailed)
	}
	if !strings.Contains(err.Error(), "subagent task cancelled") {
		t.Fatalf("error = %q, want it to mention 'subagent task cancelled'", err.Error())
	}
}

// TestTaskToolCompleted_SetsSubagentUsageData is the RED test for M2-2 (12a
// item 4): the task tool must expose the subagent's TokenUsage via
// result.Data["subagent_usage"] for react.go's roll-up (12b) to consume, and
// must leave result.Content byte-for-byte untouched (model-visible text
// stays clean) — any usage summary lives only in Data.
func TestTaskToolCompleted_SetsSubagentUsageData(t *testing.T) {
	usage := &subagent.TokenUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}
	tool := TaskTool(fakeTaskPool{
		startTask: func(ctx context.Context, description, prompt string, cfg subagent.SubagentConfig) (*subagent.Task, error) {
			return &subagent.Task{ID: "task-usage"}, nil
		},
		wait: func(ctx context.Context, taskID string) (*subagent.Task, error) {
			return &subagent.Task{ID: taskID, Status: subagent.TaskStatusCompleted, Result: "ok", Usage: usage}, nil
		},
	}, nil)

	result, err := tool.Handler(context.Background(), models.ToolCall{
		ID:        "call-usage",
		Name:      "task",
		Arguments: map[string]any{"description": "run", "prompt": "do it"},
	})
	if err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	if result.Content != "ok" {
		t.Fatalf("Content = %q, want %q unchanged (usage suffix must not leak into model-visible Content)", result.Content, "ok")
	}
	got, ok := result.Data["subagent_usage"].(*subagent.TokenUsage)
	if !ok {
		t.Fatalf("Data[\"subagent_usage\"] missing or wrong type; got %#v", result.Data["subagent_usage"])
	}
	if got == nil || *got != *usage {
		t.Fatalf("Data[\"subagent_usage\"] = %+v, want %+v", got, usage)
	}
}

// TestTaskToolCompleted_NilUsageIsNilSafe verifies a subagent that completes
// without a Usage (e.g. the provider never reported one) does not panic or
// synthesize a non-nil TokenUsage.
func TestTaskToolCompleted_NilUsageIsNilSafe(t *testing.T) {
	tool := TaskTool(fakeTaskPool{
		startTask: func(ctx context.Context, description, prompt string, cfg subagent.SubagentConfig) (*subagent.Task, error) {
			return &subagent.Task{ID: "task-nousage"}, nil
		},
		wait: func(ctx context.Context, taskID string) (*subagent.Task, error) {
			return &subagent.Task{ID: taskID, Status: subagent.TaskStatusCompleted, Result: "ok"}, nil
		},
	}, nil)

	result, err := tool.Handler(context.Background(), models.ToolCall{
		ID:        "call-nousage",
		Name:      "task",
		Arguments: map[string]any{"description": "run", "prompt": "do it"},
	})
	if err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	if got := result.Data["subagent_usage"]; got != nil {
		if tu, ok := got.(*subagent.TokenUsage); !ok || tu != nil {
			t.Fatalf("Data[\"subagent_usage\"] = %#v, want nil *TokenUsage", got)
		}
	}
}

// TestTaskTool_TokenBudgetArgLandsInConfig is the RED test for M2-2 (12d):
// the task tool's optional token_budget arg must be parsed and threaded into
// SubagentConfig.TokenBudget.
func TestTaskTool_TokenBudgetArgLandsInConfig(t *testing.T) {
	var gotBudget int
	tool := TaskTool(fakeTaskPool{
		startTask: func(ctx context.Context, description, prompt string, cfg subagent.SubagentConfig) (*subagent.Task, error) {
			gotBudget = cfg.TokenBudget
			return &subagent.Task{ID: "task-budget"}, nil
		},
		wait: func(ctx context.Context, taskID string) (*subagent.Task, error) {
			return &subagent.Task{ID: taskID, Status: subagent.TaskStatusCompleted, Result: "ok"}, nil
		},
	}, nil)

	if _, err := tool.Handler(context.Background(), models.ToolCall{
		ID:   "call-budget",
		Name: "task",
		Arguments: map[string]any{
			"description":  "run",
			"prompt":       "do it",
			"token_budget": 500.0,
		},
	}); err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	if gotBudget != 500 {
		t.Fatalf("cfg.TokenBudget = %d, want 500", gotBudget)
	}

	// Omitted token_budget must default to 0 (unlimited), not leak a stale value.
	gotBudget = -1
	if _, err := tool.Handler(context.Background(), models.ToolCall{
		ID:        "call-budget-default",
		Name:      "task",
		Arguments: map[string]any{"description": "run", "prompt": "do it"},
	}); err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	if gotBudget != 0 {
		t.Fatalf("cfg.TokenBudget default = %d, want 0", gotBudget)
	}
}

// TestTaskTool_ContextFilesArgLandsInConfig is the RED test for M2-4: the
// task tool's optional context_files arg (array of strings) must be parsed
// and threaded into SubagentConfig.ContextFiles.
//
// RED signature (today): the task tool schema has no "context_files"
// property and the handler never reads call.Arguments["context_files"], so
// cfg.ContextFiles is always nil regardless of the argument.
func TestTaskTool_ContextFilesArgLandsInConfig(t *testing.T) {
	var gotFiles []string
	tool := TaskTool(fakeTaskPool{
		startTask: func(ctx context.Context, description, prompt string, cfg subagent.SubagentConfig) (*subagent.Task, error) {
			gotFiles = cfg.ContextFiles
			return &subagent.Task{ID: "task-ctxfiles"}, nil
		},
		wait: func(ctx context.Context, taskID string) (*subagent.Task, error) {
			return &subagent.Task{ID: taskID, Status: subagent.TaskStatusCompleted, Result: "ok"}, nil
		},
	}, nil)

	if _, err := tool.Handler(context.Background(), models.ToolCall{
		ID:   "call-ctxfiles",
		Name: "task",
		Arguments: map[string]any{
			"description":   "run",
			"prompt":        "do it",
			"context_files": []any{"a.go", "b.md"},
		},
	}); err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	if len(gotFiles) != 2 || gotFiles[0] != "a.go" || gotFiles[1] != "b.md" {
		t.Fatalf("cfg.ContextFiles = %v, want [a.go b.md]", gotFiles)
	}

	// Omitted context_files must default to nil/empty, not leak a stale value.
	gotFiles = []string{"stale"}
	if _, err := tool.Handler(context.Background(), models.ToolCall{
		ID:        "call-ctxfiles-default",
		Name:      "task",
		Arguments: map[string]any{"description": "run", "prompt": "do it"},
	}); err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	if len(gotFiles) != 0 {
		t.Fatalf("cfg.ContextFiles default = %v, want empty", gotFiles)
	}
}

// TestTaskTool_ContextFilesNonStringEntryFails is the RED test for M2-4: a
// non-string entry in context_files must fail the tool call with a
// ToolResult naming the bad entry — silent coercion would hide a model
// mistake (e.g. passing a number or object where a path string belongs).
func TestTaskTool_ContextFilesNonStringEntryFails(t *testing.T) {
	tool := TaskTool(fakeTaskPool{
		startTask: func(ctx context.Context, description, prompt string, cfg subagent.SubagentConfig) (*subagent.Task, error) {
			t.Fatal("StartTask should not be called when context_files is invalid")
			return nil, nil
		},
		wait: func(ctx context.Context, taskID string) (*subagent.Task, error) {
			t.Fatal("Wait should not be called when context_files is invalid")
			return nil, nil
		},
	}, nil)

	result, err := tool.Handler(context.Background(), models.ToolCall{
		ID:   "call-ctxfiles-bad",
		Name: "task",
		Arguments: map[string]any{
			"description":   "run",
			"prompt":        "do it",
			"context_files": []any{"a.go", 42.0},
		},
	})
	if err == nil {
		t.Fatal("Handler() expected error for non-string context_files entry")
	}
	if result.Status != models.CallStatusFailed {
		t.Fatalf("Status = %s, want %s", result.Status, models.CallStatusFailed)
	}
	if !strings.Contains(result.Error, "42") {
		t.Fatalf("Error = %q, want it to name the bad entry (42)", result.Error)
	}
}

// TestTaskTool_ParentRemainingBudgetCapsConfig is the RED test for the
// M2.2-carry-forward parent-budget passthrough: when the ctx carries a
// parent's remaining token budget (injected by react.go's Run at batch
// dispatch time via tools.WithRemainingTokenBudget), the task tool must fold
// it into SubagentConfig.TokenBudget as min(nonzero values) — the parent
// remaining caps an explicit arg, and becomes the default when no explicit
// arg is given. A parent with no budget in play (ctx never carries one) must
// leave the explicit-arg-or-zero behavior completely unchanged.
//
// RED signature (today): the handler never calls
// tools.RemainingTokenBudgetFromContext, so cfg.TokenBudget is always just
// the explicit token_budget arg (or 0), regardless of what's in ctx.
func TestTaskTool_ParentRemainingBudgetCapsConfig(t *testing.T) {
	var gotBudget int
	newTool := func() models.Tool {
		return TaskTool(fakeTaskPool{
			startTask: func(ctx context.Context, description, prompt string, cfg subagent.SubagentConfig) (*subagent.Task, error) {
				gotBudget = cfg.TokenBudget
				return &subagent.Task{ID: "task-parent-budget"}, nil
			},
			wait: func(ctx context.Context, taskID string) (*subagent.Task, error) {
				return &subagent.Task{ID: taskID, Status: subagent.TaskStatusCompleted, Result: "ok"}, nil
			},
		}, nil)
	}

	call := func(ctx context.Context, args map[string]any) {
		gotBudget = -1
		tool := newTool()
		if _, err := tool.Handler(ctx, models.ToolCall{
			ID:        "call-parent-budget",
			Name:      "task",
			Arguments: args,
		}); err != nil {
			t.Fatalf("Handler() error = %v", err)
		}
	}

	// No explicit arg, parent remaining=600 present → default to 600.
	call(WithRemainingTokenBudget(context.Background(), 600), map[string]any{
		"description": "run", "prompt": "do it",
	})
	if gotBudget != 600 {
		t.Fatalf("no explicit arg, parent remaining=600: cfg.TokenBudget = %d, want 600", gotBudget)
	}

	// Explicit 900, parent remaining=600 → capped to min(600, 900)=600.
	call(WithRemainingTokenBudget(context.Background(), 600), map[string]any{
		"description": "run", "prompt": "do it", "token_budget": 900.0,
	})
	if gotBudget != 600 {
		t.Fatalf("explicit=900, parent remaining=600: cfg.TokenBudget = %d, want 600", gotBudget)
	}

	// Explicit 300, parent remaining=600 → explicit stays (min(600,300)=300).
	call(WithRemainingTokenBudget(context.Background(), 600), map[string]any{
		"description": "run", "prompt": "do it", "token_budget": 300.0,
	})
	if gotBudget != 300 {
		t.Fatalf("explicit=300, parent remaining=600: cfg.TokenBudget = %d, want 300", gotBudget)
	}

	// No parent budget in ctx at all → explicit arg passes through unchanged.
	call(context.Background(), map[string]any{
		"description": "run", "prompt": "do it", "token_budget": 900.0,
	})
	if gotBudget != 900 {
		t.Fatalf("no parent budget, explicit=900: cfg.TokenBudget = %d, want 900 (unchanged)", gotBudget)
	}

	// No parent budget, no explicit arg → stays 0 (unlimited), unchanged.
	call(context.Background(), map[string]any{
		"description": "run", "prompt": "do it",
	})
	if gotBudget != 0 {
		t.Fatalf("no parent budget, no explicit arg: cfg.TokenBudget = %d, want 0 (unchanged)", gotBudget)
	}
}

// TestTaskTool_ParentRemainingBudgetZero_RefusesCall is the RED test for
// review finding #2: when the parent's remaining budget is present in ctx
// but already exhausted (remaining <= 0 — the parent spent its whole
// MaxTokensBudget mid-turn, before this batch's dispatch), the task tool
// must refuse to spawn a subagent at all rather than silently handing it an
// unlimited budget (the old `remaining > 0` guard skipped the fold entirely
// in this case, leaving tokenBudget at whatever the explicit arg — or its
// absence — left it: unlimited by default).
//
// RED signature (today): StartTask is called anyway (with tokenBudget
// unchanged, e.g. 0/unlimited), instead of the handler refusing up front.
func TestTaskTool_ParentRemainingBudgetZero_RefusesCall(t *testing.T) {
	startTaskCalled := false
	tool := TaskTool(fakeTaskPool{
		startTask: func(ctx context.Context, description, prompt string, cfg subagent.SubagentConfig) (*subagent.Task, error) {
			startTaskCalled = true
			return &subagent.Task{ID: "should-not-start"}, nil
		},
		wait: func(ctx context.Context, taskID string) (*subagent.Task, error) {
			return &subagent.Task{ID: taskID, Status: subagent.TaskStatusCompleted, Result: "ok"}, nil
		},
	}, nil)

	result, err := tool.Handler(WithRemainingTokenBudget(context.Background(), 0), models.ToolCall{
		ID:        "call-exhausted",
		Name:      "task",
		Arguments: map[string]any{"description": "run", "prompt": "do it"},
	})
	if err == nil {
		t.Fatal("Handler() error = nil, want a refusal error when the parent's remaining budget is 0")
	}
	if startTaskCalled {
		t.Fatal("StartTask was called — the handler must refuse BEFORE spawning a subagent when the parent budget is exhausted")
	}
	if result.Status != models.CallStatusFailed {
		t.Fatalf("result.Status = %s, want %s", result.Status, models.CallStatusFailed)
	}
	if !strings.Contains(result.Error, "budget exhausted") || !strings.Contains(result.Error, "no budget available for subagents") {
		t.Fatalf("result.Error = %q, want it to explain the parent token budget is exhausted", result.Error)
	}

	// Sanity: an ABSENT parent budget (ctx never carries one) must still
	// pass through completely unchanged — only a PRESENT-but-zero remaining
	// triggers the refusal, not "no parent budget configured at all".
	startTaskCalled = false
	if _, err := tool.Handler(context.Background(), models.ToolCall{
		ID:        "call-no-parent-budget",
		Name:      "task",
		Arguments: map[string]any{"description": "run", "prompt": "do it"},
	}); err != nil {
		t.Fatalf("Handler() error = %v, want nil (no parent budget in ctx must not trigger the refusal)", err)
	}
	if !startTaskCalled {
		t.Fatal("StartTask was not called — an absent parent budget must not be treated as exhausted")
	}
}

func TestTaskTool_AdvertisesAgents(t *testing.T) {
	// No agents → description has no agent_type list.
	bare := TaskTool(nil, nil)
	if strings.Contains(bare.Description, "Available agent_type") {
		t.Fatalf("bare description should not list agents: %q", bare.Description)
	}
	// With agents → both types appear in the description.
	withAgents := TaskTool(nil, []AgentOption{
		{Type: "code-reviewer", Description: "Reviews code"},
		{Type: "devops", Description: "Deploys things"},
	})
	if !strings.Contains(withAgents.Description, "code-reviewer — Reviews code") ||
		!strings.Contains(withAgents.Description, "devops — Deploys things") {
		t.Fatalf("description should advertise agents: %q", withAgents.Description)
	}
}
