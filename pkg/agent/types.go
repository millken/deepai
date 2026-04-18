package agent

import (
	"fmt"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/memory"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/sandbox"
	"github.com/millken/deepai/pkg/tools"
)

// Usage tracks token counts for a run.
type Usage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	TotalTokens  int `json:"total_tokens,omitempty"`
}

// RunResult is the normalized outcome of an agent run.
type RunResult struct {
	Messages    []models.Message `json:"messages"`
	FinalOutput string           `json:"final_output"`
	Usage       *Usage           `json:"usage,omitempty"`
}

// AgentConfig holds the dependencies required to construct an agent.
type AgentConfig struct {
	LLMProvider     llm.LLMProvider
	Tools           *tools.Registry
	PresentFiles    *tools.PresentFileRegistry
	AgentType       AgentType
	MaxTurns        int // safety valve: hard cap on turns (0 = unlimited)
	MaxTokensBudget int // total token budget across all turns (0 = unlimited)
	Model           string
	ReasoningEffort string
	SystemPrompt    string
	Temperature     *float64
	MaxTokens       *int
	Sandbox         *sandbox.Sandbox
	RequestTimeout  time.Duration

	// Context compaction
	ContextWindow       int     // model context window in tokens; 0 = no compaction
	CompactionThreshold float64 // fraction of ContextWindow to trigger compaction (default 0.75)
	CompactionKeepTail  int     // number of recent messages to preserve (default 6)

	// Memory integration
	MemoryService   *memory.Service  // used for Inject into system prompt
	MemoryExtractor memory.Extractor // per-request extractor (matches the LLM model of the current request)
	MemoryUserID    string           // user ID for cross-session UserScope memory (empty = disabled)
}

type TimeoutError struct {
	Duration time.Duration
	Message  string
}

func (e *TimeoutError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return fmt.Sprintf("%s after %s", e.Message, e.Duration)
	}
	return fmt.Sprintf("request timed out after %s", e.Duration)
}

type AgentEventType string

const (
	AgentEventChunk         AgentEventType = "chunk"
	AgentEventTextChunk     AgentEventType = "text_chunk"
	AgentEventToolCall      AgentEventType = "tool_call"
	AgentEventToolCallStart AgentEventType = "tool_call_start"
	AgentEventToolResult    AgentEventType = "tool_result"
	AgentEventToolCallEnd   AgentEventType = "tool_call_end"
	AgentEventEnd           AgentEventType = "end"
	AgentEventError         AgentEventType = "error"
	AgentEventCompact       AgentEventType = "compact"
)

type ToolCallEvent struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Arguments     map[string]any     `json:"arguments,omitempty"`
	ArgumentsText string             `json:"arguments_text,omitempty"`
	Status        models.CallStatus  `json:"status"`
	ResultPreview string             `json:"result_preview,omitempty"`
	Error         string             `json:"error,omitempty"`
	RequestedAt   string             `json:"requested_at,omitempty"`
	StartedAt     string             `json:"started_at,omitempty"`
	CompletedAt   string             `json:"completed_at,omitempty"`
	DurationMS    int64              `json:"duration_ms,omitempty"`
	Result        *models.ToolResult `json:"result,omitempty"`
}

type AgentError struct {
	Code       string `json:"code,omitempty"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion,omitempty"`
	Retryable  bool   `json:"retryable,omitempty"`
}

// AgentEvent is emitted while the agent is running.
type AgentEvent struct {
	Type         AgentEventType     `json:"type"`
	SessionID    string             `json:"session_id,omitempty"`
	RequestID    string             `json:"request_id,omitempty"`
	MessageID    string             `json:"message_id,omitempty"`
	Text         string             `json:"text,omitempty"`
	ToolCall     *models.ToolCall   `json:"tool_call,omitempty"`
	ToolEvent    *ToolCallEvent     `json:"tool_event,omitempty"`
	Result       *models.ToolResult `json:"result,omitempty"`
	Usage        *Usage             `json:"usage,omitempty"`
	Err          string             `json:"error,omitempty"`
	Error        *AgentError        `json:"error_detail,omitempty"`
	CompactStats *CompactStats      `json:"compact_stats,omitempty"`
}

// CompactStats describes the outcome of a context compaction pass.
type CompactStats struct {
	MessagesBefore int     `json:"messages_before"`
	MessagesAfter  int     `json:"messages_after"`
	InputTokens    int     `json:"input_tokens"`
	ContextWindow  int     `json:"context_window"`
	Ratio          float64 `json:"ratio"`
}
