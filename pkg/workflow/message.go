package workflow

import (
	"fmt"
	"sync/atomic"
	"time"
)

// Message type constants for common inter-agent communication patterns.
const (
	MsgTypeUserRequest  = "user_request"
	MsgTypeCodeChange   = "code_change"
	MsgTypeReviewResult = "review_result"
	MsgTypePRD          = "prd"
	MsgTypeDesign       = "design"
)

// AgentMessage is a structured message passed between agents.
type AgentMessage struct {
	ID        string         `json:"id"`
	From      string         `json:"from"`
	To        string         `json:"to,omitempty"`
	Type      string         `json:"type"`
	Content   string         `json:"content"`
	Artifacts map[string]any `json:"artifacts,omitempty"`
	Timestamp time.Time      `json:"timestamp"`
}

var msgSeq uint64

func newAgentMessage(from, to, msgType, content string) AgentMessage {
	seq := atomic.AddUint64(&msgSeq, 1)
	return AgentMessage{
		ID:        fmt.Sprintf("msg_%d_%d", time.Now().UTC().UnixNano(), seq),
		From:      from,
		To:        to,
		Type:      msgType,
		Content:   content,
		Timestamp: time.Now().UTC(),
	}
}
