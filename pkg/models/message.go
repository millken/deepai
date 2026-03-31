package models

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Role identifies the actor that produced a message in a session.
type Role string

const (
	RoleHuman  Role = "human"
	RoleAI     Role = "ai"
	RoleSystem Role = "system"
	RoleTool   Role = "tool"
)

// Validate reports whether the role is one of the supported message roles.
func (r Role) Validate() error {
	switch r {
	case RoleHuman, RoleAI, RoleSystem, RoleTool:
		return nil
	default:
		return fmt.Errorf("invalid role %q", r)
	}
}

// CallStatus tracks the lifecycle of a tool call or tool result.
type CallStatus string

const (
	CallStatusPending   CallStatus = "pending"
	CallStatusRunning   CallStatus = "running"
	CallStatusCompleted CallStatus = "completed"
	CallStatusFailed    CallStatus = "failed"
)

// Validate reports whether the call status is one of the supported values.
func (s CallStatus) Validate() error {
	switch s {
	case CallStatusPending, CallStatusRunning, CallStatusCompleted, CallStatusFailed:
		return nil
	default:
		return fmt.Errorf("invalid call status %q", s)
	}
}

// ToolCall represents a single invocation request for a tool.
type ToolCall struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Arguments   map[string]any `json:"arguments,omitempty"`
	Status      CallStatus     `json:"status"`
	RequestedAt time.Time      `json:"requested_at,omitempty"`
	StartedAt   time.Time      `json:"started_at,omitempty"`
	CompletedAt time.Time      `json:"completed_at,omitempty"`
}

// Validate checks whether the tool call contains the minimum valid data.
func (c ToolCall) Validate() error {
	if strings.TrimSpace(c.ID) == "" {
		return errors.New("tool call id is required")
	}
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("tool call name is required")
	}
	if err := c.Status.Validate(); err != nil {
		return err
	}
	if !c.StartedAt.IsZero() && c.StartedAt.Before(c.RequestedAt) {
		return errors.New("tool call started_at cannot be before requested_at")
	}
	if !c.CompletedAt.IsZero() && c.CompletedAt.Before(c.StartedAt) {
		return errors.New("tool call completed_at cannot be before started_at")
	}
	return nil
}

// ToolResult stores the normalized outcome of a tool execution.
type ToolResult struct {
	CallID      string         `json:"call_id"`
	ToolName    string         `json:"tool_name"`
	Status      CallStatus     `json:"status"`
	Content     string         `json:"content,omitempty"`
	Data        map[string]any `json:"data,omitempty"`
	Error       string         `json:"error,omitempty"`
	CompletedAt time.Time      `json:"completed_at,omitempty"`
	Duration    time.Duration  `json:"duration,omitempty"`
}

// Validate checks whether the tool result is internally consistent.
func (r ToolResult) Validate() error {
	if strings.TrimSpace(r.CallID) == "" {
		return errors.New("tool result call_id is required")
	}
	if strings.TrimSpace(r.ToolName) == "" {
		return errors.New("tool result tool_name is required")
	}
	if err := r.Status.Validate(); err != nil {
		return err
	}
	if r.Status == CallStatusCompleted && strings.TrimSpace(r.Error) != "" {
		return errors.New("completed tool result cannot include an error")
	}
	if r.Status == CallStatusFailed && strings.TrimSpace(r.Error) == "" {
		return errors.New("failed tool result must include an error")
	}
	if r.Duration < 0 {
		return errors.New("tool result duration cannot be negative")
	}
	return nil
}

// Message is the canonical unit of conversation state within a session.
type Message struct {
	ID         string            `json:"id"`
	SessionID  string            `json:"session_id"`
	Role       Role              `json:"role"`
	Content    string            `json:"content,omitempty"`
	ToolCalls  []ToolCall        `json:"tool_calls,omitempty"`
	ToolResult *ToolResult       `json:"tool_result,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	CreatedAt  time.Time         `json:"created_at,omitempty"`
}

// Validate checks whether the message is structurally valid.
func (m Message) Validate() error {
	if strings.TrimSpace(m.ID) == "" {
		return errors.New("message id is required")
	}
	if strings.TrimSpace(m.SessionID) == "" {
		return errors.New("message session_id is required")
	}
	if err := m.Role.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(m.Content) == "" && len(m.ToolCalls) == 0 && m.ToolResult == nil {
		return errors.New("message must include content, tool calls, or a tool result")
	}
	if m.Role == RoleTool && m.ToolResult == nil {
		return errors.New("tool messages must include a tool result")
	}
	if m.Role != RoleAI && len(m.ToolCalls) > 0 {
		return errors.New("only ai messages may include tool calls")
	}
	for i, call := range m.ToolCalls {
		if err := call.Validate(); err != nil {
			return fmt.Errorf("tool_calls[%d]: %w", i, err)
		}
	}
	if m.ToolResult != nil {
		if err := m.ToolResult.Validate(); err != nil {
			return fmt.Errorf("tool_result: %w", err)
		}
	}
	return nil
}

// Tool describes a callable tool that can be exposed to the agent runtime.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
	Groups      []string       `json:"groups,omitempty"`
	Handler     ToolHandler    `json:"-"`
}

// Validate checks whether the tool definition is ready for registration.
func (t Tool) Validate() error {
	if strings.TrimSpace(t.Name) == "" {
		return errors.New("tool name is required")
	}
	if t.Handler == nil {
		return errors.New("tool handler is required")
	}
	return nil
}

// ToolHandler executes a tool call and returns a normalized result payload.
type ToolHandler func(ctx context.Context, call ToolCall) (ToolResult, error)
