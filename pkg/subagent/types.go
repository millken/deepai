package subagent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/models"
)

type SubagentType string

const (
	SubagentGeneralPurpose SubagentType = "general-purpose"
	SubagentBash           SubagentType = "bash"
)

type TaskStatus string

const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusCompleted TaskStatus = "completed"
	TaskStatusFailed    TaskStatus = "failed"
	TaskStatusTimedOut  TaskStatus = "timed_out"
)

type SubagentConfig struct {
	Type         SubagentType
	MaxTurns     int
	Timeout      time.Duration
	SystemPrompt string
	Tools        []string
}

type PoolConfig struct {
	MaxConcurrent int
	Timeout       time.Duration
	Logger        *slog.Logger
	Defaults      map[SubagentType]SubagentConfig
}

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

type ExecutionResult struct {
	Result   string
	Messages []models.Message
}

type Executor interface {
	Execute(ctx context.Context, task *Task, emit func(TaskEvent)) (ExecutionResult, error)
}

func (t *Task) snapshot() *Task {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()

	clone := *t
	clone.Messages = append([]models.Message(nil), t.Messages...)
	return &clone
}
