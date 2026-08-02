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
