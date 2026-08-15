package agent

import (
	"context"
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
	// ToolCalls counts the tool calls this run EXECUTED (parallel and serial
	// paths). LLMTurns counts LLM round-trips issued (compaction retries
	// included — each is a real request). Their ratio diagnoses a model's
	// batching behavior: ~1 call/turn (GLM-style) means N calls cost N
	// sequential round-trips, while 3+/turn amortizes them. BudgetExhausted
	// reports whether the MaxToolCalls cap forced the wrap-up turn.
	ToolCalls       int  `json:"tool_calls"`
	LLMTurns        int  `json:"llm_turns"`
	BudgetExhausted bool `json:"budget_exhausted"`
}

// DefaultMaxOutputTokens is the fallback max output tokens applied to both
// the main (interactive REPL) agent and every subagent's LLM calls, in place
// of each provider's own default (8192 for Anthropic — see anthropic.go's
// buildMessageParams). That default is easy to exceed while streaming a
// single large tool-call argument (e.g. a design document's markdown passed
// inline to docx_write): the provider cuts the response off mid-string once
// it hits the limit, and the truncated JSON then fails to parse.
//
// This is only the fallback: an explicit DEEPAI_MAX_OUTPUT_TOKENS setting
// wins over it (see ResolveMaxOutputTokens in config_env.go). Both
// pkg/commands/chat.go's subagent wiring and pkg/chat/repl.go's main-agent
// wiring must set MaxTokens by calling ResolveMaxOutputTokens, never a
// separate literal or a direct read of this constant, or the two can
// silently drift apart the way they did before this was introduced (the
// main agent had no override at all).
const DefaultMaxOutputTokens = 16384

// AgentConfig holds the dependencies required to construct an agent.
type AgentConfig struct {
	LLMProvider     llm.LLMProvider
	Tools           *tools.Registry
	PresentFiles    *tools.PresentFileRegistry
	AgentType       AgentType
	MaxToolCalls    int // safety valve: cap on executed tool calls (0 = unlimited); on exhaustion the run wraps up with a final no-tools answer instead of failing
	MaxTokensBudget int // total token budget across all turns (0 = unlimited)
	Model           string
	ReasoningEffort string
	SystemPrompt    string
	Temperature     *float64
	MaxTokens       *int
	Sandbox         *sandbox.Sandbox
	// RequestTimeout bounds the *entire* Run (all turns + tool calls).
	// Despite the name it is run-level, not per-request. It is used as-is
	// with no floor or fallback: 0 means unlimited, and the caller (e.g. the
	// interactive REPL) governs the run's lifetime via context cancellation
	// instead.
	RequestTimeout time.Duration

	// Context compaction
	ContextWindow       int     // model context window in tokens; 0 = no compaction
	CompactionThreshold float64 // fraction of ContextWindow to trigger compaction (default 0.75)
	CompactionKeepTail  int     // number of recent messages to preserve (default 6)

	// Aging controls the prompt-view information-decay compression (T1 tool
	// results + T4 conversation text). nil = disabled (behavior unchanged). It
	// derives a per-request compressed view; canonical messages are untouched.
	Aging *AgingConfig

	// Metrics is the Phase 0 measurement sink (per-turn provider tokens + byte
	// buckets, per-tool result sizes). nil = disabled (zero overhead).
	Metrics MetricsSink

	// Memory integration
	MemoryService   *memory.Service  // used for Inject into system prompt
	MemoryExtractor memory.Extractor // per-request extractor (matches the LLM model of the current request)
	MemoryUserID    string           // user ID for cross-session UserScope memory (empty = disabled)

	// User interaction (e.g. ask_clarification tool)
	UserInteraction tools.UserInteraction // nil = non-interactive, tools proceed without user input

	// Plan mode: restrict agent to read-only tools until user approves the plan
	PlanMode bool
	WorkDir  string // working directory for writing plan files

	// NonInteractive marks delegated sub-agents and other headless contexts.
	// It disables plan mode (no user to approve) and suppresses team-delegation
	// prompt injection (sub-agents don't orchestrate).
	NonInteractive bool

	// AgentCatalog lists the sub-agent types available via the task tool, used
	// to render delegation guidance in the system prompt. Empty = no delegation
	// prompt injected. Only meaningful for top-level (interactive) agents.
	AgentCatalog []AgentInfo

	// OffloadDir is where tool results exceeding the offload threshold are
	// written to disk. Empty = auto-derive ~/.deepai/offload in New().
	OffloadDir string

	// ImageDetail controls the vision detail level for image attachments.
	// "low" (default, ~170 tokens/image) or "high" (multi-tile, finer detail).
	ImageDetail string

	// Session carries Agent state across successive single-use Agent Runs
	// within one conversation (M4-3, task-23-brief.md): the tool-call
	// circuit breaker, the active skill (+ its system-prompt body), and the
	// context-compaction anchors/stall state. nil (the default) preserves
	// today's per-Run-only behavior — every field starts fresh, exactly as
	// before this option existed. This is the right default for one-shot
	// callers and, critically, for subagents: a delegated Agent must NEVER
	// receive a Session shared with its parent or siblings (see
	// subagent.go's buildAgentConfig, which never sets this field) since a
	// subagent's Run can execute concurrently with others on a different
	// goroutine, and SessionCarry has no internal locking (see its doc
	// comment). The REPL is the intended caller: it holds one *SessionCarry
	// for the life of a conversation and passes it, unchanged, into every
	// turn's AgentConfig, resetting it (to a fresh NewSessionCarry()) only
	// on a full history wipe (e.g. /clear).
	Session *SessionCarry
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

// Unwrap exposes context.DeadlineExceeded as this error's cause for EVERY
// *TimeoutError, from both of this package's two construction sites — but
// that only reflects the literal cause at one of them:
//
//   - normalizeRunError (this file's caller) constructs one only after
//     checking errors.Is(ctx.Err(), context.DeadlineExceeded) itself, so for
//     that site Unwrap restates a real context deadline that already fired.
//   - The stream-idle watchdog (streaming.go's consumeStream, the
//     `case <-idleTimer.C` branch) constructs one from its OWN idleTimer
//     firing and calls cancel() itself right there — the request's own ctx
//     has NOT expired at that point, and canceling it this way makes
//     reqCtx.Err() become context.Canceled, never context.DeadlineExceeded.
//     Unwrap's answer for THIS site is a deliberate classification choice
//     ("stream went idle" is treated as the same kind of failure as "ran out
//     of time" for callers that check the cause), not a restatement of an
//     observed deadline.
//
// The practical effect is that errors.Is(err, context.DeadlineExceeded) is
// true for a stream-idle timeout too, which existing callers were not
// written to expect but which is not a misclassification requiring a code
// fix (both are still Retryable/CallStatusFailed at their respective
// layers): newAgentError (streaming.go) now reports Code:"deadline_exceeded"
// (previously "run_error") for a stream-idle timeout, and
// pkg/subagent/pool.go's status switch now reports TaskStatusTimedOut
// (previously TaskStatusFailed) for the same case. Both are arguably more
// accurate descriptions of "the request took too long," which is exactly
// what happened either way — recorded here as an intended consequence of
// this Unwrap, not an oversight.
//
// Existing consumers use errors.As(err, *TimeoutError) (e.g. subagent.go's
// mid-stream fall-soft guard) and keep working unchanged; this additionally
// makes errors.Is(err, context.DeadlineExceeded) hold for a *TimeoutError,
// which it did not before.
func (e *TimeoutError) Unwrap() error { return context.DeadlineExceeded }

type AgentEventType string

const (
	// AgentEventChunk is no longer emitted by the ReAct loop (superseded by
	// AgentEventTextChunk, which every internal consumer already used); the
	// constant is kept only for external consumers that may still switch on it.
	//
	// Deprecated: use AgentEventTextChunk instead.
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
	AfterTokens    int     `json:"after_tokens"`
	ContextWindow  int     `json:"context_window"`
	Ratio          float64 `json:"ratio"`
	AfterRatio     float64 `json:"after_ratio"`
}
