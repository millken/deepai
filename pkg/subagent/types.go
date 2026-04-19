package subagent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/models"
)

// SubagentType is kept for backward compatibility.
// New code should use AgentType from pkg/agent instead.
type SubagentType string

const (
	SubagentGeneralPurpose SubagentType = "general-purpose"
	SubagentBash           SubagentType = "bash"
)

// TaskStatus represents the current state of a task.
type TaskStatus string

const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusCompleted TaskStatus = "completed"
	TaskStatusFailed    TaskStatus = "failed"
	TaskStatusTimedOut  TaskStatus = "timed_out"
)

// SubagentConfig holds the configuration for a subagent task.
type SubagentConfig struct {
	// AgentType is the unified agent type (e.g. "coder", "bash", "security-reviewer").
	// Takes precedence over Type (SubagentType).
	AgentType    string
	Type         SubagentType // Deprecated: use AgentType
	MaxTurns     int
	Timeout      time.Duration
	SystemPrompt string
	Tools        []string
}

// EffectiveAgentType returns the resolved agent type string.
// AgentType takes precedence over the deprecated Type field.
func (c SubagentConfig) EffectiveAgentType() string {
	if c.AgentType != "" {
		return c.AgentType
	}
	return string(c.Type)
}

// PoolConfig holds the configuration for a subagent pool.
type PoolConfig struct {
	MaxConcurrent int
	Timeout       time.Duration
	Logger        *slog.Logger
	Defaults      map[string]SubagentConfig // key is agent type string
}

// Task represents a subagent task.
type Task struct {
	ID          string
	RequestID   string
	Type        SubagentType
	Config      SubagentConfig
	Status      TaskStatus
	Description string
	Prompt      string
	Result      string
	Error       string
	Messages    []models.Message
	createdAt   time.Time
	completedAt time.Time
	done        chan struct{}
	mu          sync.RWMutex
}

// ExecutionResult holds the result of a task execution.
type ExecutionResult struct {
	Result   string
	Messages []models.Message
}

// Executor is the interface for executing subagent tasks.
type Executor interface {
	Execute(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error)
}

func (t *Task) snapshot() *Task {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()

	return &Task{
		ID:          t.ID,
		RequestID:   t.RequestID,
		Type:        t.Type,
		Config:      t.Config,
		Status:      t.Status,
		Description: t.Description,
		Prompt:      t.Prompt,
		Result:      t.Result,
		Error:       t.Error,
		Messages:    append([]models.Message(nil), t.Messages...),
		createdAt:   t.createdAt,
		completedAt: t.completedAt,
		done:        nil,
	}
}
