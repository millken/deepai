package agent

import "github.com/millken/deepai/pkg/models"

// SteerMessage is a mid-turn user steering message injected at the next safe
// ReAct boundary (after the previous turn's tool calls finish and before the
// next LLM request is built).
type SteerMessage struct {
	ID      string
	Content string
	Images  []models.MessageImage
}

// SteerSource supplies mid-turn user steering messages at safe ReAct boundaries.
// Implementations must be non-blocking and goroutine-safe.
type SteerSource interface {
	DrainSteer() []SteerMessage
}

// SteerWrapUpNotifier is optionally implemented by a SteerSource to be notified
// the moment a Run enters its forced no-tools wrap-up phase, allowing callers
// (e.g. pkg/bot) to synchronously fall back incoming steer requests to a
// follow-up queue.
type SteerWrapUpNotifier interface {
	MarkWrapUp()
}
