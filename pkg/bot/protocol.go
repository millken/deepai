// Package bot turns the deepai agent runtime into a long-running service.
//
// A Manager owns sessions; each session runs at most one agent turn at a
// time and publishes everything that happens during the turn as a stream of
// sequenced Events. Transports (REST, SSE, WebSocket — see server.go) are
// thin adapters over the Manager, so a web page, a desktop GUI, a mobile app
// or a chat-platform adapter all drive the bot through the same API and see
// the same event stream. docs/BOT_DESIGN.md describes the protocol.
package bot

import (
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/subagent"
)

// EventType names one kind of Event on the wire. The set is append-only:
// clients must ignore types they do not know.
type EventType string

const (
	EventSessionCreated EventType = "session_created"
	EventSessionDeleted EventType = "session_deleted"
	EventTurnStart      EventType = "turn_start"
	EventTextDelta      EventType = "text_delta"
	EventToolCallStart  EventType = "tool_call_start"
	EventToolCallEnd    EventType = "tool_call_end"
	EventSubagent       EventType = "subagent"
	EventProgress       EventType = "progress"
	EventCompact        EventType = "compact"
	EventQuestion       EventType = "question"
	EventAnswered       EventType = "question_answered"
	EventError          EventType = "error"
	EventTurnEnd        EventType = "turn_end"

	// EventGap (seq 0) tells a client that events it asked for were evicted
	// from the replay buffer or that it fell behind: reload the session
	// transcript, then resubscribe from the session's last_seq.
	EventGap EventType = "gap"
	// EventHeartbeat (seq 0) keeps idle NDJSON/WebSocket streams alive.
	EventHeartbeat EventType = "heartbeat"
)

// Event is the single envelope every transport delivers. Seq is assigned by
// the session's bus and increases by one per event, which is what lets a
// reconnecting client resume with ?since=<last seq> (or Last-Event-ID) and
// miss nothing that is still in the replay buffer.
type Event struct {
	Seq       int64     `json:"seq"`
	SessionID string    `json:"session_id"`
	TurnID    string    `json:"turn_id,omitempty"`
	Type      EventType `json:"type"`
	Time      time.Time `json:"time"`

	Text     string               `json:"text,omitempty"`
	Tool     *agent.ToolCallEvent `json:"tool,omitempty"`
	Subagent *subagent.TaskEvent  `json:"subagent,omitempty"`
	Question *Question            `json:"question,omitempty"`
	Compact  *agent.CompactStats  `json:"compact,omitempty"`
	Usage    *agent.Usage         `json:"usage,omitempty"`
	Error    *agent.AgentError    `json:"error,omitempty"`
	Turn     *TurnSummary         `json:"turn,omitempty"`
	Session  *SessionInfo         `json:"session,omitempty"`
}

// Question is an ask_clarification call waiting for a client to answer it.
type Question struct {
	ID      string   `json:"id"`
	Text    string   `json:"text"`
	Options []string `json:"options,omitempty"`
	Answer  string   `json:"answer,omitempty"`
}

// TurnStatus is how a turn ended.
type TurnStatus string

const (
	TurnCompleted TurnStatus = "completed"
	TurnCancelled TurnStatus = "cancelled"
	TurnFailed    TurnStatus = "failed"
)

// TurnSummary is carried by turn_end and returned by a blocking send.
type TurnSummary struct {
	ID          string       `json:"id"`
	Status      TurnStatus   `json:"status"`
	Model       string       `json:"model,omitempty"`
	FinalOutput string       `json:"final_output,omitempty"`
	Error       string       `json:"error,omitempty"`
	Usage       *agent.Usage `json:"usage,omitempty"`
	ToolCalls   int          `json:"tool_calls"`
	LLMTurns    int          `json:"llm_turns"`
	StartedAt   time.Time    `json:"started_at"`
	EndedAt     time.Time    `json:"ended_at"`
}

// SessionStatus is whether a session is currently running a turn.
type SessionStatus string

const (
	SessionIdle    SessionStatus = "idle"
	SessionRunning SessionStatus = "running"
)

// SessionInfo is the listing/detail view of a session.
type SessionInfo struct {
	ID        string        `json:"id"`
	Title     string        `json:"title,omitempty"`
	Model     string        `json:"model,omitempty"`
	Status    SessionStatus `json:"status"`
	TurnID    string        `json:"turn_id,omitempty"`
	MsgCount  int           `json:"msg_count"`
	LastSeq   int64         `json:"last_seq"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	// Pending lists questions the running turn is blocked on.
	Pending []Question `json:"pending,omitempty"`
}

// SendRequest is the body of POST /v1/sessions/{id}/messages and of the
// WebSocket "send" op.
type SendRequest struct {
	Content string `json:"content"`
	// Model overrides the session's model alias for this turn and becomes
	// the session's model from then on.
	Model string `json:"model,omitempty"`
}

// CreateSessionRequest is the body of POST /v1/sessions.
type CreateSessionRequest struct {
	Title string `json:"title,omitempty"`
	Model string `json:"model,omitempty"`
}

// AnswerRequest is the body of POST /v1/sessions/{id}/answer.
type AnswerRequest struct {
	QuestionID string `json:"question_id"`
	Answer     string `json:"answer"`
}

// ModelInfo is one entry of GET /v1/info's model list.
type ModelInfo struct {
	Name     string `json:"name"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

// Info is the body of GET /v1/info.
type Info struct {
	Name         string      `json:"name"`
	Version      string      `json:"version,omitempty"`
	WorkDir      string      `json:"work_dir,omitempty"`
	DefaultModel string      `json:"default_model,omitempty"`
	Models       []ModelInfo `json:"models,omitempty"`
	StartedAt    time.Time   `json:"started_at"`
}
