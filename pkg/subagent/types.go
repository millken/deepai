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
	TaskStatusCancelled TaskStatus = "cancelled"
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
	Model        string
	// TokenBudget is an optional per-task total-token cap passed through to
	// the subagent's AgentConfig.MaxTokensBudget. 0 = unlimited.
	TokenBudget int
	// ContextFiles is an optional list of repo-relative or absolute file
	// paths whose contents are read and prepended to the subagent's first
	// message as a <context-files> block (pkg/agent's SubagentExecutor.Execute).
	// The parent names files explicitly rather than context being shared
	// automatically — see docs/ARCHITECTURE_REVIEW.md M2.4.
	ContextFiles []string
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
	// Usage is the subagent's total token consumption, populated in
	// finishTask once the executor returns. nil if the task never reached
	// the executor (e.g. it bailed pre-semaphore) or the executor reported
	// no usage.
	Usage       *TokenUsage
	createdAt   time.Time
	completedAt time.Time
	done        chan struct{}
	mu          sync.RWMutex
}

// TokenUsage tracks token consumption for a subagent task. Defined locally
// (rather than reusing agent.Usage) to avoid an import cycle: pkg/agent
// already imports pkg/subagent. json tags use lowercase snake_case so
// persisted sessions (which marshal Task/TokenUsage values) stay consistent
// with the rest of the models/session JSON, rather than leaking Go field
// names.
type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ExecutionResult holds the result of a task execution.
type ExecutionResult struct {
	Result   string
	Messages []models.Message
	// Usage is the subagent run's total token consumption, nil if the
	// executor's agent run never reported any (e.g. it errored before any
	// response, or the provider omitted usage).
	Usage *TokenUsage
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
		Usage:       cloneTokenUsage(t.Usage),
		createdAt:   t.createdAt,
		completedAt: t.completedAt,
		done:        nil,
	}
}

// cloneTokenUsage returns a copy of the TokenUsage value pointed to by u, so
// snapshot() callers get their own isolated Usage rather than sharing the
// live Task's pointer — the same isolation contract snapshot() already
// applies to Messages (a fresh slice, not the live one).
func cloneTokenUsage(u *TokenUsage) *TokenUsage {
	if u == nil {
		return nil
	}
	v := *u
	return &v
}
