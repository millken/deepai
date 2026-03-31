package tools

import (
	"context"
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
			if cfg.Type != subagent.SubagentBash {
				t.Fatalf("cfg.Type = %s, want %s", cfg.Type, subagent.SubagentBash)
			}
			if cfg.MaxTurns != 3 {
				t.Fatalf("cfg.MaxTurns = %d, want 3", cfg.MaxTurns)
			}
			return &subagent.Task{ID: "task-1"}, nil
		},
		wait: func(ctx context.Context, taskID string) (*subagent.Task, error) {
			return &subagent.Task{ID: taskID, Status: subagent.TaskStatusCompleted, Result: "ok"}, nil
		},
	})

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
	})

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
