package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrNotFound is returned by EventStore.GetTimeline when no events match.
var ErrNotFound = errors.New("proxy log not found")

// EventType identifies the kind of log event.
type EventType string

const (
	EventStart    EventType = "start"     // request arrived: method, path, model, format, stream, upstream
	EventReqBody  EventType = "req_body"  // request body (may be truncated with body_file pointer)
	EventDelta    EventType = "delta"     // streaming text fragment (extracted plain text, not raw SSE)
	EventUsage    EventType = "usage"     // token usage
	EventRespBody EventType = "resp_body" // non-streaming response body (may be truncated)
	EventDone     EventType = "done"      // request completed: status, dur, ttfb, chunks, error
)

// RawBody holds arbitrary byte content. It marshals as raw JSON when valid,
// or as a JSON string otherwise. This handles both JSON API bodies and
// non-JSON content (SSE streams, error pages, etc.).
type RawBody []byte

// MarshalJSON implements json.Marshaler.
func (r RawBody) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte(`""`), nil
	}
	if json.Valid([]byte(r)) {
		return []byte(r), nil
	}
	return json.Marshal(string(r))
}

// UnmarshalJSON implements json.Unmarshaler.
func (r *RawBody) UnmarshalJSON(data []byte) error {
	*r = RawBody(data)
	return nil
}

// LogEvent represents a single point in a request's timeline.
type LogEvent struct {
	Timestamp time.Time `json:"ts"`
	Type      EventType `json:"type"`
	ID        string    `json:"id"`

	// start event fields
	Method       string `json:"method,omitempty"`
	Path         string `json:"path,omitempty"`
	APIFormat    string `json:"format,omitempty"`
	Model        string `json:"model,omitempty"`
	Streaming    bool   `json:"stream,omitempty"`
	UpstreamAddr string `json:"upstream,omitempty"`
	ClientID     string `json:"session_id,omitempty"` // source client session (e.g. X-Claude-Code-Session-Id)

	// req_body / resp_body event fields
	Body      RawBody `json:"body,omitempty"`
	BodySize  int     `json:"size,omitempty"`
	BodyFile  string  `json:"body_file,omitempty"`
	Truncated bool    `json:"truncated,omitempty"` // true when body exceeded MaxRequestBody and was cut

	// delta event fields
	Text string `json:"text,omitempty"`

	// usage event fields
	InputTokens  int `json:"input,omitempty"`
	OutputTokens int `json:"output,omitempty"`
	TotalTokens  int `json:"total,omitempty"`

	// done event fields
	StatusCode int    `json:"status,omitempty"`
	Duration   string `json:"dur,omitempty"`
	TTFB       string `json:"ttfb,omitempty"`
	ChunkCount int    `json:"chunks,omitempty"`
	Error      string `json:"error,omitempty"`
}

// RequestSummary is a lightweight summary of a completed request,
// reconstructed from start + usage + done events.
type RequestSummary struct {
	ID           string    `json:"id"`
	Method       string    `json:"method"`
	Path         string    `json:"path"`
	Model        string    `json:"model"`
	APIFormat    string    `json:"format"`
	Streaming    bool      `json:"stream"`
	StatusCode   int       `json:"status"`
	Duration     string    `json:"dur"`
	InputTokens  int       `json:"input"`
	OutputTokens int       `json:"output"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	ClientID     string    `json:"session_id,omitempty"`
}

// EventStore persists proxy log events.
type EventStore interface {
	// Append persists one or more log events atomically.
	Append(ctx context.Context, events ...LogEvent) error
	// ListRequests returns a paginated list of request summaries, newest first.
	ListRequests(ctx context.Context, offset, limit int) ([]RequestSummary, error)
	// GetTimeline returns all events for a specific request, in chronological order.
	GetTimeline(ctx context.Context, id string) ([]LogEvent, error)
	// Close releases resources.
	Close() error
}
