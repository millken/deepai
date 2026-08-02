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
