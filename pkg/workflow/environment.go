package workflow

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const defaultInboxSize = 64
const defaultMaxHistory = 1000

// Subscription defines what messages a role is interested in.
type Subscription struct {
	Role     string   // agent type or stage name
	MsgTypes []string // subscribed message types; empty means all types
}

// MsgFilter is a functional filter for querying message history.
type MsgFilter func(AgentMessage) bool

// EnvOption configures an Environment.
type EnvOption func(*Environment)

// WithMaxHistory sets the maximum number of messages retained in history.
func WithMaxHistory(n int) EnvOption {
	return func(e *Environment) { e.maxHistory = n }
}

// WithInboxSize sets the buffered channel size per inbox.
func WithInboxSize(n int) EnvOption {
	return func(e *Environment) { e.inboxSize = n }
}

// Environment provides decoupled pub/sub message routing between agents.
type Environment struct {
	mu         sync.RWMutex
	roles      map[string]Subscription
	inboxes    map[string]chan AgentMessage
	history    []AgentMessage
	maxHistory int
	inboxSize  int
	closed     bool
}

// NewEnvironment creates a new message environment.
func NewEnvironment(opts ...EnvOption) *Environment {
	e := &Environment{
		roles:      make(map[string]Subscription),
		inboxes:    make(map[string]chan AgentMessage),
		maxHistory: defaultMaxHistory,
		inboxSize:  defaultInboxSize,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Register adds a role subscription to the environment.
// Re-registering an existing role replaces its subscription and resets its inbox.
func (e *Environment) Register(sub Subscription) error {
	if sub.Role == "" {
		return fmt.Errorf("subscription role is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return fmt.Errorf("environment is closed")
	}
	// Close old inbox if re-registering
	if old, ok := e.inboxes[sub.Role]; ok {
		close(old)
	}
	e.roles[sub.Role] = sub
	e.inboxes[sub.Role] = make(chan AgentMessage, e.inboxSize)
	return nil
}

// Unregister removes a role and closes its inbox.
func (e *Environment) Unregister(role string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ch, ok := e.inboxes[role]; ok {
		close(ch)
		delete(e.inboxes, role)
	}
	delete(e.roles, role)
}

// Publish sends a message. If msg.To is non-empty, delivers to that role only.
// If msg.To is empty, broadcasts to all roles subscribed to msg.Type.
// Always appends to history.
func (e *Environment) Publish(ctx context.Context, msg AgentMessage) error {
	if msg.ID == "" {
		msg = newAgentMessage(msg.From, msg.To, msg.Type, msg.Content)
	}

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return fmt.Errorf("environment is closed")
	}
	e.appendHistory(msg)

	if msg.To != "" {
		// Directed message
		ch := e.inboxes[msg.To]
		e.mu.Unlock()
		if ch == nil {
			return nil // no subscriber, silently drop
		}
		return e.send(ctx, ch, msg)
	}

	// Broadcast: collect matching inboxes
	var targets []chan AgentMessage
	for role, sub := range e.roles {
		if matchesSubscription(sub, msg.Type) {
			if ch, ok := e.inboxes[role]; ok {
				targets = append(targets, ch)
			}
		}
	}
	e.mu.Unlock()

	for _, ch := range targets {
		if err := e.send(ctx, ch, msg); err != nil {
			return err
		}
	}
	return nil
}

// Receive blocks until a message arrives for the given role or ctx is cancelled.
func (e *Environment) Receive(ctx context.Context, role string) (AgentMessage, error) {
	e.mu.RLock()
	ch := e.inboxes[role]
	e.mu.RUnlock()
	if ch == nil {
		return AgentMessage{}, fmt.Errorf("role %q not registered", role)
	}
	select {
	case msg, ok := <-ch:
		if !ok {
			return AgentMessage{}, fmt.Errorf("inbox for %q closed", role)
		}
		return msg, nil
	case <-ctx.Done():
		return AgentMessage{}, ctx.Err()
	}
}

// History returns messages matching all filters. No filter returns all history.
func (e *Environment) History(filters ...MsgFilter) []AgentMessage {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if len(filters) == 0 {
		result := make([]AgentMessage, len(e.history))
		copy(result, e.history)
		return result
	}
	var result []AgentMessage
	for _, msg := range e.history {
		match := true
		for _, f := range filters {
			if !f(msg) {
				match = false
				break
			}
		}
		if match {
			result = append(result, msg)
		}
	}
	return result
}

// Roles returns all registered role names.
func (e *Environment) Roles() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	names := make([]string, 0, len(e.roles))
	for r := range e.roles {
		names = append(names, r)
	}
	return names
}

// Close closes all inboxes and prevents further operations.
func (e *Environment) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	for role, ch := range e.inboxes {
		close(ch)
		delete(e.inboxes, role)
	}
}

func (e *Environment) appendHistory(msg AgentMessage) {
	e.history = append(e.history, msg)
	if len(e.history) > e.maxHistory {
		e.history = e.history[len(e.history)-e.maxHistory:]
	}
}

func (e *Environment) send(ctx context.Context, ch chan AgentMessage, msg AgentMessage) error {
	select {
	case ch <- msg:
		return nil
	default:
		// Inbox full — drop message to avoid blocking publisher.
		// History already has a copy for traceability.
		return nil
	}
}

func matchesSubscription(sub Subscription, msgType string) bool {
	if len(sub.MsgTypes) == 0 {
		return true
	}
	for _, t := range sub.MsgTypes {
		if t == msgType {
			return true
		}
	}
	return false
}

// Filter helpers

// From filters messages by sender.
func From(role string) MsgFilter {
	return func(msg AgentMessage) bool { return msg.From == role }
}

// Type filters messages by type.
func Type(t string) MsgFilter {
	return func(msg AgentMessage) bool { return msg.Type == t }
}

// Since filters messages after the given time.
func Since(t time.Time) MsgFilter {
	return func(msg AgentMessage) bool { return !msg.Timestamp.Before(t) }
}
