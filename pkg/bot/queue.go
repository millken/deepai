package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
)

const (
	// SendModeReject is the default mode: sending while a turn is running
	// returns ErrBusy (409 busy).
	SendModeReject = "reject"
	// SendModeQueue enqueues the message onto the session's FIFO follow-up
	// queue when a turn is already running.
	SendModeQueue = "queue"
	// SendModeSteer injects the message into the running turn at the next safe
	// ReAct boundary (falling back to queue if the turn is wrapping up).
	SendModeSteer = "steer"

	// MaxSessionQueueDepth caps per-session in-memory queued follow-up messages
	// (docs/BOT_NEXT_DESIGN.md §2.2).
	MaxSessionQueueDepth = 16
)

var (
	ErrQueueFull         = errors.New("session queue is full")
	ErrInvalidMode       = errors.New("invalid send mode")
	ErrQueueItemNotFound = errors.New("queued message not found")
)

// QueuedMessage is one pending follow-up message waiting in a session's FIFO
// queue (docs/BOT_NEXT_DESIGN.md §2.2).
type QueuedMessage struct {
	ID               string                `json:"id"`
	Content          string                `json:"content"`
	Model            string                `json:"model,omitempty"`
	Images           []models.MessageImage `json:"images,omitempty"`
	Metadata         map[string]string     `json:"metadata,omitempty"`
	DisableSelfTrain bool                  `json:"-"`
	CreatedAt        time.Time             `json:"created_at"`
}

// SendResult describes the outcome of Submit / SubmitForBot across reject,
// queue, and steer modes.
type SendResult struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id,omitempty"`
	Turn      *Turn  `json:"-"`

	Queued   bool   `json:"queued,omitempty"`
	QueueID  string `json:"queue_id,omitempty"`
	Position int    `json:"position,omitempty"`

	Steered  bool   `json:"steered,omitempty"`
	SteerID  string `json:"steer_id,omitempty"`
	Fallback string `json:"fallback,omitempty"`
}

// steerBuffer is the per-turn goroutine-safe channel between Manager.Submit
// (mode=steer) and pkg/agent's ReAct boundary checkpoint. It implements both
// agent.SteerSource and agent.SteerWrapUpNotifier.
type steerBuffer struct {
	mu     sync.Mutex
	msgs   []agent.SteerMessage
	wrapUp bool
	closed bool
}

func newSteerBuffer() *steerBuffer { return &steerBuffer{} }

type steerPushStatus int

const (
	steerAccepted steerPushStatus = iota
	steerFallbackQueue
	steerFull
)

// Push attempts to queue a steer message for the running turn. It returns
// steerFallbackQueue when the turn has entered wrap-up or already finished,
// and steerFull when pending steer messages already reached MaxSessionQueueDepth.
func (b *steerBuffer) Push(m agent.SteerMessage) steerPushStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.wrapUp || b.closed {
		return steerFallbackQueue
	}
	if len(b.msgs) >= MaxSessionQueueDepth {
		return steerFull
	}
	b.msgs = append(b.msgs, m)
	return steerAccepted
}

// DrainSteer implements agent.SteerSource.
func (b *steerBuffer) DrainSteer() []agent.SteerMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.msgs) == 0 {
		return nil
	}
	out := b.msgs
	b.msgs = nil
	return out
}

// MarkWrapUp implements agent.SteerWrapUpNotifier.
func (b *steerBuffer) MarkWrapUp() {
	b.mu.Lock()
	b.wrapUp = true
	b.mu.Unlock()
}

// CloseAndDrain marks the turn finished and returns any unconsumed steer
// messages so runTurn can promote them to the front of the follow-up queue
// without losing user input.
func (b *steerBuffer) CloseAndDrain() []agent.SteerMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	if len(b.msgs) == 0 {
		return nil
	}
	out := b.msgs
	b.msgs = nil
	return out
}

func normalizeSendMode(raw string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(raw))
	switch m {
	case "", SendModeReject:
		return SendModeReject, nil
	case SendModeQueue:
		return SendModeQueue, nil
	case SendModeSteer:
		return SendModeSteer, nil
	default:
		return "", fmt.Errorf("%w: %q (want reject, queue, or steer)", ErrInvalidMode, raw)
	}
}

// Submit sends a message on a default-bot session honoring req.Mode (reject,
// queue, or steer).
func (m *Manager) Submit(id string, req SendRequest) (*SendResult, error) {
	return m.SubmitForBot(DefaultBotID, id, req, SendOptions{})
}

// SubmitForBot sends, enqueues, or steers a message on a bot-scoped session
// according to req.Mode (docs/BOT_NEXT_DESIGN.md §2).
func (m *Manager) SubmitForBot(botID, id string, req SendRequest, opts SendOptions) (*SendResult, error) {
	mode, err := normalizeSendMode(req.Mode)
	if err != nil {
		return nil, err
	}
	botID = effectiveBotID(botID)
	profile, err := m.bots.GetBot(context.Background(), botID)
	if err != nil {
		return nil, err
	}
	if !profile.Enabled {
		return nil, fmt.Errorf("%w: %s", ErrBotDisabled, botID)
	}
	content := strings.TrimSpace(req.Content)
	if content == "" && len(req.AttachmentIDs) == 0 && len(req.Images) == 0 {
		return nil, ErrEmptyMessage
	}
	model := strings.TrimSpace(req.Model)
	if model != "" && !m.opts.Runtime.HasModel(model) {
		return nil, fmt.Errorf("%w: %s", ErrUnknownModel, model)
	}
	s, err := m.loadForBot(botID, id)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	if _, deleting := m.deletingBots[botID]; deleting {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrBotDisabled, botID)
	}
	m.turns.Add(1)
	m.botTurns[botID]++
	m.mu.Unlock()

	started := false
	defer func() {
		if !started {
			m.decBotTurn(botID)
			m.turns.Done()
		}
	}()

	s.mu.Lock()
	resolvedContent, resolvedImages, attMeta, err := resolveAttachmentsLocked(s, content, req.AttachmentIDs, req.Images)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if s.turn != nil {
		switch mode {
		case SendModeReject:
			s.mu.Unlock()
			return nil, ErrBusy

		case SendModeSteer:
			steerID := "st_" + randomHex(6)
			if s.turn.steer != nil {
				switch s.turn.steer.Push(agent.SteerMessage{ID: steerID, Content: resolvedContent, Images: resolvedImages}) {
				case steerAccepted:
					turnID := s.turn.id
					s.lastUsed = time.Now().UTC()
					s.bus.publish(Event{
						BotID:     botID,
						SessionID: id,
						TurnID:    turnID,
						Type:      EventSteerQueued,
						SteerID:   steerID,
						Text:      resolvedContent,
					})
					s.mu.Unlock()
					return &SendResult{
						SessionID: id,
						TurnID:    turnID,
						Steered:   true,
						SteerID:   steerID,
					}, nil
				case steerFull:
					s.mu.Unlock()
					return nil, ErrQueueFull
				}
			}
			// Turn is in wrapUp (or finishing): fall back to follow-up queue.
			if len(s.queue) >= MaxSessionQueueDepth {
				s.mu.Unlock()
				return nil, ErrQueueFull
			}
			item := QueuedMessage{
				ID:               "q_" + randomHex(6),
				Content:          resolvedContent,
				Model:            model,
				Images:           resolvedImages,
				Metadata:         attMeta,
				DisableSelfTrain: opts.DisableSelfTrain,
				CreatedAt:        time.Now().UTC(),
			}
			s.queue = append(s.queue, item)
			pos := len(s.queue)
			s.lastUsed = item.CreatedAt
			cp := item
			s.bus.publish(Event{
				BotID:     botID,
				SessionID: id,
				Type:      EventQueueEnqueued,
				QueueItem: &cp,
				Text:      resolvedContent,
			})
			s.mu.Unlock()
			return &SendResult{
				SessionID: id,
				Queued:    true,
				QueueID:   item.ID,
				Position:  pos,
				Fallback:  SendModeQueue,
			}, nil

		case SendModeQueue:
			if len(s.queue) >= MaxSessionQueueDepth {
				s.mu.Unlock()
				return nil, ErrQueueFull
			}
			item := QueuedMessage{
				ID:               "q_" + randomHex(6),
				Content:          resolvedContent,
				Model:            model,
				Images:           resolvedImages,
				Metadata:         attMeta,
				DisableSelfTrain: opts.DisableSelfTrain,
				CreatedAt:        time.Now().UTC(),
			}
			s.queue = append(s.queue, item)
			pos := len(s.queue)
			s.lastUsed = item.CreatedAt
			cp := item
			s.bus.publish(Event{
				BotID:     botID,
				SessionID: id,
				Type:      EventQueueEnqueued,
				QueueItem: &cp,
				Text:      resolvedContent,
			})
			s.mu.Unlock()
			return &SendResult{
				SessionID: id,
				Queued:    true,
				QueueID:   item.ID,
				Position:  pos,
			}, nil
		}
	}

	launch, err := m.prepareTurnLocked(s, profile, resolvedContent, model, resolvedImages, attMeta, opts)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()

	if launch.needTitle {
		titleSrc := content
		if titleSrc == "" {
			titleSrc = resolvedContent
		}
		m.autoTitle(s, titleSrc)
	}
	started = true
	go m.runTurn(launch.ctx, s, launch.turn, launch.conv, launch.model, launch.history)
	turn := &Turn{ID: launch.turn.id, t: launch.turn}
	return &SendResult{
		SessionID: id,
		TurnID:    turn.ID,
		Turn:      turn,
	}, nil
}

// ListQueue lists pending follow-up messages for a default-bot session.
func (m *Manager) ListQueue(id string) ([]QueuedMessage, error) {
	return m.ListQueueForBot(DefaultBotID, id)
}

// ListQueueForBot lists pending follow-up messages for a bot-scoped session.
func (m *Manager) ListQueueForBot(botID, id string) ([]QueuedMessage, error) {
	s, err := m.loadForBot(botID, id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]QueuedMessage, len(s.queue))
	copy(out, s.queue)
	return out, nil
}

// RemoveQueuedMessage removes one queued message from a default-bot session.
func (m *Manager) RemoveQueuedMessage(id, queueID string) error {
	return m.RemoveQueuedMessageForBot(DefaultBotID, id, queueID)
}

// RemoveQueuedMessageForBot removes one queued message from a bot-scoped session.
func (m *Manager) RemoveQueuedMessageForBot(botID, id, queueID string) error {
	s, err := m.loadForBot(botID, id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, item := range s.queue {
		if item.ID == queueID {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			cp := item
			s.bus.publish(Event{
				BotID:     effectiveBotID(s.botID),
				SessionID: s.id,
				Type:      EventQueueRemoved,
				QueueItem: &cp,
			})
			return nil
		}
	}
	return ErrQueueItemNotFound
}

// ClearQueue clears all queued follow-up messages for a default-bot session.
func (m *Manager) ClearQueue(id string) error {
	return m.ClearQueueForBot(DefaultBotID, id)
}

// ClearQueueForBot clears all queued follow-up messages for a bot-scoped session.
func (m *Manager) ClearQueueForBot(botID, id string) error {
	s, err := m.loadForBot(botID, id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return nil
	}
	s.queue = nil
	s.bus.publish(Event{
		BotID:     effectiveBotID(s.botID),
		SessionID: s.id,
		Type:      EventQueueCleared,
		Reason:    "user",
	})
	return nil
}

// startNextQueued pops the head of s.queue and launches the next turn if the
// manager is still open, the bot is still enabled, and the session is idle.
// Called from runTurn before its deferred m.turns.Done() so Manager.Close waits
// cleanly for chained queued turns.
func (m *Manager) startNextQueued(s *session) {
	botID := effectiveBotID(s.botID)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	if _, deleting := m.deletingBots[botID]; deleting {
		m.mu.Unlock()
		return
	}
	m.turns.Add(1)
	m.botTurns[botID]++
	m.mu.Unlock()

	started := false
	defer func() {
		if !started {
			m.decBotTurn(botID)
			m.turns.Done()
		}
	}()

	profile, err := m.bots.GetBot(context.Background(), botID)
	if err != nil || !profile.Enabled {
		if err == nil {
			s.mu.Lock()
			hasQ := len(s.queue) > 0
			if hasQ {
				s.bus.publish(Event{
					BotID:     botID,
					SessionID: s.id,
					Type:      EventError,
					Error: &agent.AgentError{
						Code:      "bot_disabled",
						Message:   fmt.Sprintf("queued follow-up paused: bot %s is disabled", botID),
						Retryable: true,
					},
				})
			}
			s.mu.Unlock()
		}
		return
	}

	s.mu.Lock()
	if s.turn != nil || len(s.queue) == 0 {
		s.mu.Unlock()
		return
	}
	item := s.queue[0]
	s.queue = s.queue[1:]
	launch, err := m.prepareTurnLocked(s, profile, item.Content, item.Model, item.Images, item.Metadata, SendOptions{DisableSelfTrain: item.DisableSelfTrain})
	if err != nil {
		// Re-queue at front so user input isn't silently dropped on transient lock/conv error.
		s.queue = append([]QueuedMessage{item}, s.queue...)
		s.bus.publish(Event{
			BotID:     botID,
			SessionID: s.id,
			Type:      EventError,
			Error: &agent.AgentError{
				Code:      "queue_launch_failed",
				Message:   err.Error(),
				Retryable: true,
			},
		})
		s.mu.Unlock()
		m.log.Warn("bot: start queued turn failed", "session", s.id, "queue_id", item.ID, "err", err)
		return
	}
	cp := item
	s.bus.publish(Event{
		BotID:     botID,
		SessionID: s.id,
		TurnID:    launch.turn.id,
		Type:      EventQueueDequeued,
		QueueItem: &cp,
		Text:      item.Content,
	})
	s.mu.Unlock()

	if launch.needTitle {
		m.autoTitle(s, item.Content)
	}
	started = true
	go m.runTurn(launch.ctx, s, launch.turn, launch.conv, launch.model, launch.history)
}
