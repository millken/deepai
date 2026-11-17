package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

var (
	ErrNotFound        = errors.New("session not found")
	ErrBusy            = errors.New("session is already running a turn")
	ErrNotRunning      = errors.New("session has no running turn")
	ErrUnknownQuestion = errors.New("no pending question with that id")
	ErrUnknownModel    = errors.New("unknown model")
	ErrEmptyMessage    = errors.New("message content is empty")
	ErrClosed          = errors.New("bot is shutting down")
)

// SessionSource is the `source` column bot-created sessions get in bot.db.
const SessionSource = "bot"

// lockRefreshInterval keeps a turn's session lock well inside the store's
// 60s staleness window (pkg/chat/session.go staleLockAfter).
const lockRefreshInterval = 20 * time.Second

// Options configures a Manager.
type Options struct {
	Runtime Runtime
	Store   Store
	// WorkDir is recorded on created sessions (the store's cwd column) and
	// scopes the Manager to them: sessions from other directories are
	// neither listed nor opened (ErrNotFound). Empty disables scoping.
	WorkDir string
	// ReplayEvents bounds each session's replay buffer (default
	// DefaultReplayEvents).
	ReplayEvents int
	// IdleTimeout unloads a session that has had no turn and no subscriber
	// for this long, releasing its sandbox. 0 means 30 minutes; negative
	// disables eviction.
	IdleTimeout time.Duration
	// TurnTimeout bounds one turn. 0 means no limit beyond Cancel.
	TurnTimeout time.Duration
	Logger      *slog.Logger
}

// Manager owns the bot's sessions. It is safe for concurrent use; every
// transport is a thin adapter over its methods.
type Manager struct {
	opts      Options
	log       *slog.Logger
	owner     models.LockOwner
	startedAt time.Time

	baseCtx   context.Context
	cancelAll context.CancelFunc
	turns     sync.WaitGroup
	global    *bus

	mu       sync.Mutex
	sessions map[string]*session
	seqFloor map[string]int64 // last seq of sessions unloaded by the janitor
	closed   bool
	stopJan  chan struct{}
}

type session struct {
	id string

	mu        sync.Mutex
	title     string
	model     string
	createdAt time.Time
	updatedAt time.Time
	messages  []models.Message
	conv      Conversation
	turn      *activeTurn
	lastUsed  time.Time
	bus       *bus
}

type activeTurn struct {
	id           string
	cancel       context.CancelFunc
	clock        *turnClock
	hooks        turnHooks
	maxToolCalls int
	done         chan struct{}
	summary      *TurnSummary
	questions    map[string]*pendingQuestion
}

type pendingQuestion struct {
	q      Question
	answer chan string
}

// NewManager validates opts and starts the idle-session janitor.
func NewManager(opts Options) (*Manager, error) {
	if opts.Runtime == nil {
		return nil, errors.New("bot: Runtime is required")
	}
	if opts.Store == nil {
		opts.Store = NewMemoryStore()
	}
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = 30 * time.Minute
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	host, _ := os.Hostname()
	ctx, cancel := context.WithCancelCause(context.Background())
	m := &Manager{
		opts:      opts,
		log:       log,
		owner:     models.LockOwner{PID: os.Getpid(), Host: host},
		startedAt: time.Now().UTC(),
		baseCtx:   ctx,
		// The cause propagates to every turn's context, so turn_end can
		// report cancel_source=shutdown (design §3.2).
		cancelAll: func() { cancel(turnStopCause{source: string(cancelSourceShutdown), err: context.Canceled}) },
		global:    newBus(opts.ReplayEvents),
		sessions:  map[string]*session{},
		seqFloor:  map[string]int64{},
		stopJan:   make(chan struct{}),
	}
	if opts.IdleTimeout > 0 {
		go m.janitor()
	}
	return m, nil
}

// Info describes the bot.
func (m *Manager) Info() Info {
	info := m.opts.Runtime.Info()
	if info.Name == "" {
		info.Name = "deepai-bot"
	}
	if info.WorkDir == "" {
		info.WorkDir = m.opts.WorkDir
	}
	info.StartedAt = m.startedAt
	return info
}

// CreateSession creates and loads a new session.
func (m *Manager) CreateSession(req CreateSessionRequest) (SessionInfo, error) {
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = m.opts.Runtime.DefaultModel()
	} else if !m.opts.Runtime.HasModel(model) {
		return SessionInfo{}, fmt.Errorf("%w: %s", ErrUnknownModel, model)
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return SessionInfo{}, ErrClosed
	}
	m.mu.Unlock()

	rec, err := m.opts.Store.Create(models.CreateOpts{
		Title:  strings.TrimSpace(req.Title),
		Model:  model,
		CWD:    m.opts.WorkDir,
		Source: SessionSource,
	})
	if err != nil {
		return SessionInfo{}, fmt.Errorf("create session: %w", err)
	}
	now := time.Now().UTC()
	s := &session{
		id:        rec.ID,
		title:     rec.Title,
		model:     model,
		createdAt: orNow(rec.CreatedAt, now),
		updatedAt: orNow(rec.UpdatedAt, now),
		lastUsed:  now,
		bus:       newBus(m.opts.ReplayEvents),
	}
	m.mu.Lock()
	m.sessions[s.id] = s
	m.mu.Unlock()

	info := s.info()
	m.global.publish(Event{SessionID: s.id, Type: EventSessionCreated, Session: &info})
	return info, nil
}

// ListSessions merges the store's recent sessions with the live state of
// loaded ones.
func (m *Manager) ListSessions(limit int) ([]SessionInfo, error) {
	if limit <= 0 {
		limit = 50
	}
	metas, err := m.opts.Store.ListRecentInDir(m.opts.WorkDir, limit)
	if err != nil {
		return nil, err
	}
	out := make([]SessionInfo, 0, len(metas))
	for _, meta := range metas {
		m.mu.Lock()
		s := m.sessions[meta.ID]
		m.mu.Unlock()
		if s != nil {
			out = append(out, s.info())
			continue
		}
		out = append(out, SessionInfo{
			ID: meta.ID, Title: meta.Title, Status: SessionIdle, MsgCount: meta.MsgCount,
			CreatedAt: meta.CreatedAt, UpdatedAt: meta.UpdatedAt,
		})
	}
	return out, nil
}

// GetSession returns a session's state and transcript, loading it from the
// store if needed.
func (m *Manager) GetSession(id string) (SessionInfo, []models.Message, error) {
	s, err := m.load(id)
	if err != nil {
		return SessionInfo{}, nil, err
	}
	s.mu.Lock()
	msgs := append(make([]models.Message, 0, len(s.messages)), s.messages...)
	s.mu.Unlock()
	return s.info(), msgs, nil
}

// RenameSession sets a session's title.
func (m *Manager) RenameSession(id, title string) (SessionInfo, error) {
	s, err := m.load(id)
	if err != nil {
		return SessionInfo{}, err
	}
	title = strings.TrimSpace(title)
	if err := m.opts.Store.Rename(id, title); err != nil {
		return SessionInfo{}, err
	}
	s.mu.Lock()
	s.title = title
	s.mu.Unlock()
	return s.info(), nil
}

// DeleteSession removes an idle session from the store.
func (m *Manager) DeleteSession(id string) error {
	s, err := m.load(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.turn != nil {
		s.mu.Unlock()
		return ErrBusy
	}
	conv := s.conv
	s.conv = nil
	s.mu.Unlock()

	if err := m.opts.Store.Delete(id); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.sessions, id)
	delete(m.seqFloor, id)
	m.mu.Unlock()
	if conv != nil {
		_ = conv.Close()
	}
	s.bus.publish(Event{SessionID: id, Type: EventSessionDeleted})
	s.bus.close()
	m.global.publish(Event{SessionID: id, Type: EventSessionDeleted})
	return nil
}

// Turn is a handle on a turn started by Send.
type Turn struct {
	ID string
	t  *activeTurn
}

// Done is closed once the turn has ended and its messages are persisted.
func (t *Turn) Done() <-chan struct{} { return t.t.done }

// Summary is the turn's outcome; nil until Done is closed.
func (t *Turn) Summary() *TurnSummary {
	select {
	case <-t.t.done:
		return t.t.summary
	default:
		return nil
	}
}

// SendOptions is the internal counterpart of SendRequest. Transports do not
// expose it: only the schedule Dispatcher uses it to tag a turn as a
// scheduled run (metadata for client rendering + ctx values for tools).
type SendOptions struct {
	Metadata   map[string]string
	ScheduleID string
	RunID      string
	// Timeout is a per-turn execution budget. 0 means "use --turn-timeout";
	// the effective budget is min(Timeout, --turn-timeout) when both are set
	// (design §7.2). The clock pauses while the turn waits for an answer.
	Timeout time.Duration
	// MaxToolCalls overrides the bot-wide limit for this turn only.
	MaxToolCalls int
	// Hooks observes questions; scheduled turns use it for waiting_input /
	// answer_timeout. It is deliberately unexported: transports must not set it.
	Hooks turnHooks
}

// Send starts a turn and returns without waiting for it. Follow it with the
// returned handle or a subscription.
func (m *Manager) Send(id string, req SendRequest) (*Turn, error) {
	return m.SendWithOptions(id, req, SendOptions{})
}

// SendWithOptions starts a turn with internal metadata (scheduled runs).
func (m *Manager) SendWithOptions(id string, req SendRequest, opts SendOptions) (*Turn, error) {
	content := strings.TrimSpace(req.Content)
	if content == "" {
		return nil, ErrEmptyMessage
	}
	model := strings.TrimSpace(req.Model)
	if model != "" && !m.opts.Runtime.HasModel(model) {
		return nil, fmt.Errorf("%w: %s", ErrUnknownModel, model)
	}
	s, err := m.load(id)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	m.turns.Add(1)
	m.mu.Unlock()

	started := false
	defer func() {
		if !started {
			m.turns.Done()
		}
	}()

	s.mu.Lock()
	if s.turn != nil {
		s.mu.Unlock()
		return nil, ErrBusy
	}
	if s.conv == nil {
		conv, err := m.opts.Runtime.NewConversation(id)
		if err != nil {
			s.mu.Unlock()
			return nil, fmt.Errorf("start conversation: %w", err)
		}
		s.conv = conv
	}
	if model != "" {
		s.model = model
	}
	if s.model == "" {
		s.model = m.opts.Runtime.DefaultModel()
	}
	if locker, ok := m.opts.Store.(Locker); ok {
		if err := locker.AcquireSessionLock(id, m.owner, false); err != nil {
			s.mu.Unlock()
			return nil, err
		}
	}
	budget := opts.Timeout
	if m.opts.TurnTimeout > 0 && (budget <= 0 || m.opts.TurnTimeout < budget) {
		budget = m.opts.TurnTimeout
	}
	ctx, clock := newTurnClock(m.baseCtx, budget)
	t := &activeTurn{
		id:           "turn_" + randomHex(6),
		cancel:       clock.cancelPlain,
		clock:        clock,
		hooks:        opts.Hooks,
		maxToolCalls: opts.MaxToolCalls,
		done:         make(chan struct{}),
		questions:    map[string]*pendingQuestion{},
	}
	ctx = tools.WithScheduleRun(ctx, opts.ScheduleID, opts.RunID)
	ctx = tools.WithTurnID(ctx, t.id)
	s.turn = t
	s.lastUsed = time.Now().UTC()
	userMsg := models.Message{
		ID:        id + "_" + randomHex(6),
		SessionID: id,
		Role:      models.RoleHuman,
		Content:   content,
		Metadata:  opts.Metadata,
		CreatedAt: time.Now().UTC(),
	}
	if err := m.opts.Store.AppendMessage(id, userMsg); err != nil {
		m.log.Warn("bot: persist user message", "session", id, "err", err)
	}
	s.messages = append(s.messages, userMsg)
	history := append([]models.Message(nil), s.messages...)
	turnModel := s.model
	conv := s.conv
	needTitle := s.title == ""
	s.mu.Unlock()

	if needTitle {
		m.autoTitle(s, content)
	}

	started = true
	go m.runTurn(ctx, s, t, conv, turnModel, history)
	return &Turn{ID: t.id, t: t}, nil
}

// Closing reports whether Close has begun. The schedule Dispatcher uses it
// to distinguish a shutdown cancellation from a deliberate client cancel.
func (m *Manager) Closing() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// Cancel interrupts the session's running turn. The turn still ends with a
// turn_end event (status "cancelled") once the agent has unwound and its
// partial progress is persisted.
func (m *Manager) Cancel(id string) error {
	return m.cancelWithSource(id, cancelSourceClient)
}

// cancelWithSource lets the schedule Dispatcher attribute a cancellation to
// the scheduler (answer_timeout / overlap=replace) instead of a client.
func (m *Manager) cancelWithSource(id string, src cancelSource) error {
	s, err := m.load(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	t := s.turn
	s.mu.Unlock()
	if t == nil {
		return ErrNotRunning
	}
	t.clock.CancelWithSource(src)
	return nil
}

// Answer resolves a pending ask_clarification question.
func (m *Manager) Answer(id, questionID, answer string) error {
	return m.answer(id, questionID, answer, false)
}

// answerAuto resolves a question on behalf of the bot (unattended runs).
func (m *Manager) answerAuto(id, questionID, answer string) error {
	return m.answer(id, questionID, answer, true)
}

func (m *Manager) answer(id, questionID, answer string, auto bool) error {
	s, err := m.load(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	t := s.turn
	var pq *pendingQuestion
	if t != nil {
		pq = t.questions[questionID]
		delete(t.questions, questionID)
	}
	if pq == nil {
		s.mu.Unlock()
		return ErrUnknownQuestion
	}
	pq.answer <- answer
	q := pq.q
	q.Answer, q.Auto = answer, auto
	s.bus.publish(Event{SessionID: id, TurnID: t.id, Type: EventAnswered, Question: &q})
	hooks := t.hooks
	s.mu.Unlock()
	if hooks != nil {
		hooks.QuestionSettled(questionID, answer, auto)
	}
	return nil
}

// Subscribe follows one session's events. since is the last Seq the client
// has seen (0 for everything buffered, -1 for live only).
func (m *Manager) Subscribe(id string, since int64) (*Subscription, []Event, bool, error) {
	s, err := m.load(id)
	if err != nil {
		return nil, nil, false, err
	}
	s.mu.Lock()
	s.lastUsed = time.Now().UTC()
	s.mu.Unlock()
	sub, backlog, gap := s.bus.subscribe(since)
	return sub, backlog, gap, nil
}

// SubscribeAll follows bot-wide lifecycle events: session_created,
// session_deleted, turn_start and turn_end of every session.
func (m *Manager) SubscribeAll(since int64) (*Subscription, []Event, bool) {
	return m.global.subscribe(since)
}

// Close cancels every running turn, waits for them to persist (or for ctx),
// and releases all sessions.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	close(m.stopJan)
	m.mu.Unlock()

	m.cancelAll()
	done := make(chan struct{})
	go func() {
		m.turns.Wait()
		close(done)
	}()
	var err error
	select {
	case <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}

	m.mu.Lock()
	sessions := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = map[string]*session{}
	m.mu.Unlock()
	for _, s := range sessions {
		s.mu.Lock()
		conv := s.conv
		s.conv = nil
		s.mu.Unlock()
		if conv != nil {
			_ = conv.Close()
		}
		s.bus.close()
	}
	m.global.close()
	return err
}

func (m *Manager) runTurn(ctx context.Context, s *session, t *activeTurn, conv Conversation, model string, history []models.Message) {
	defer m.turns.Done()
	defer t.cancel()

	started := time.Now().UTC()
	startEvt := Event{SessionID: s.id, TurnID: t.id, Type: EventTurnStart, Text: history[len(history)-1].Content}
	s.bus.publish(startEvt)
	m.global.publish(startEvt)

	stopRefresh := m.holdLock(s.id)

	// Publishing under s.mu orders every turn event before the s.turn = nil
	// below, so nothing a Runtime emits late (e.g. from an abandoned run)
	// can appear after this turn's turn_end.
	emit := func(evt Event) {
		evt.SessionID = s.id
		evt.TurnID = t.id
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.turn == t {
			s.bus.publish(evt)
		}
	}
	out, runErr := conv.RunTurn(ctx, TurnInput{
		SessionID:    s.id,
		TurnID:       t.id,
		Model:        model,
		Messages:     history,
		Emit:         emit,
		Interaction:  &turnInteraction{m: m, s: s, t: t},
		MaxToolCalls: t.maxToolCalls,
	})
	if t.hooks != nil {
		t.hooks.TurnEnded()
	}

	summary := &TurnSummary{ID: t.id, Status: TurnCompleted, Model: model, StartedAt: started}
	if out != nil {
		summary.FinalOutput = out.FinalOutput
		summary.Usage = out.Usage
		summary.ToolCalls = out.ToolCalls
		summary.LLMTurns = out.LLMTurns
	}
	// A pausable clock cancels the context with a cause; ctx.Err() is always
	// context.Canceled, so the status comes from context.Cause (design §7.2).
	cause := context.Cause(ctx)
	switch {
	case errors.Is(cause, context.DeadlineExceeded):
		summary.Status = TurnTimedOut
		summary.Error = "turn exceeded its execution budget"
		if runErr != nil {
			summary.Error = runErr.Error()
		}
	case runErr == nil:
	case errors.Is(runErr, context.DeadlineExceeded):
		summary.Status = TurnTimedOut
		summary.Error = runErr.Error()
	case errors.Is(cause, context.Canceled), errors.Is(runErr, context.Canceled):
		summary.Status = TurnCancelled
		summary.CancelSource = stopSource(ctx)
	default:
		summary.Status = TurnFailed
		summary.Error = runErr.Error()
	}

	s.mu.Lock()
	if out != nil && len(out.Messages) >= len(history) {
		for _, msg := range out.Messages[len(history):] {
			if err := m.opts.Store.AppendMessage(s.id, msg); err != nil {
				m.log.Warn("bot: persist message", "session", s.id, "err", err)
			}
		}
		s.messages = out.Messages
	}
	s.updatedAt = time.Now().UTC()
	s.lastUsed = s.updatedAt
	for qid, pq := range t.questions {
		close(pq.answer)
		delete(t.questions, qid)
	}
	s.mu.Unlock()

	stopRefresh()
	summary.EndedAt = time.Now().UTC()
	t.summary = summary

	// turn_end goes out while s.turn is still t: a Send racing Done() must
	// not get its turn_start onto either bus ahead of this turn's end.
	endEvt := Event{SessionID: s.id, TurnID: t.id, Type: EventTurnEnd, Turn: summary, Usage: summary.Usage}
	s.mu.Lock()
	s.bus.publish(endEvt)
	m.global.publish(endEvt)
	s.turn = nil
	s.mu.Unlock()
	close(t.done)
	afterTurnDone()
}

// afterTurnDone is a test hook run once a turn's Done() has fired.
var afterTurnDone = func() {}

// holdLock keeps the session lock (acquired in Send) fresh until the
// returned func releases it.
func (m *Manager) holdLock(id string) func() {
	locker, ok := m.opts.Store.(Locker)
	if !ok {
		return func() {}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(lockRefreshInterval)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if err := locker.RefreshSessionLock(id, m.owner); err != nil {
					m.log.Warn("bot: refresh session lock", "session", id, "err", err)
				}
			}
		}
	}()
	return func() {
		close(stop)
		<-done
		if err := locker.ReleaseSessionLock(id, m.owner); err != nil {
			m.log.Warn("bot: release session lock", "session", id, "err", err)
		}
	}
}

func (m *Manager) autoTitle(s *session, content string) {
	title := strings.Join(strings.Fields(content), " ")
	if r := []rune(title); len(r) > 60 {
		title = string(r[:60]) + "…"
	}
	if err := m.opts.Store.Rename(s.id, title); err != nil {
		m.log.Debug("bot: auto title", "session", s.id, "err", err)
		return
	}
	s.mu.Lock()
	if s.title == "" {
		s.title = title
	}
	s.mu.Unlock()
}

// load returns the in-memory session, loading it from the store on first
// use. Store I/O runs under m.mu so two requests cannot load one session
// twice; loads are rare (once per session per process lifetime).
func (m *Manager) load(id string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	if s := m.sessions[id]; s != nil {
		return s, nil
	}
	if m.opts.WorkDir != "" {
		ok, err := m.opts.Store.InDir(id, m.opts.WorkDir)
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				return nil, ErrNotFound
			}
			return nil, err
		}
		if !ok {
			return nil, ErrNotFound
		}
	}
	rec, err := m.opts.Store.Load(id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil, ErrNotFound
		}
		return nil, err
	}
	msgs, err := m.opts.Store.LoadMessages(id)
	if err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}
	for i := range msgs {
		if msgs[i].SessionID == "" {
			msgs[i].SessionID = id
		}
	}
	model := rec.Metadata["model"]
	if model == "" || !m.opts.Runtime.HasModel(model) {
		model = m.opts.Runtime.DefaultModel()
	}
	now := time.Now().UTC()
	s := &session{
		id:        id,
		title:     rec.Title,
		model:     model,
		createdAt: orNow(rec.CreatedAt, now),
		updatedAt: orNow(rec.UpdatedAt, now),
		messages:  msgs,
		lastUsed:  now,
		bus:       newBusAt(m.opts.ReplayEvents, m.reloadBaseLocked(id)),
	}
	m.sessions[id] = s
	return s, nil
}

func (m *Manager) janitor() {
	every := m.opts.IdleTimeout / 4
	if every < time.Second {
		every = time.Second
	}
	if every > time.Minute {
		every = time.Minute
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-m.stopJan:
			return
		case <-tick.C:
			m.evictIdle(time.Now().UTC().Add(-m.opts.IdleTimeout))
		}
	}
}

// reloadBaseLocked picks where a session loaded from the store resumes its
// event numbering. Clients may still hold cursors from the bus that served
// it before it was unloaded, or from a previous bot process; starting at the
// current time in microseconds (and never below this process's last seq for
// it) keeps every such cursor below the new range, so Subscribe reports a
// gap instead of silently replaying from the wrong place.
func (m *Manager) reloadBaseLocked(id string) int64 {
	return max(time.Now().UnixMicro(), m.seqFloor[id])
}

func (m *Manager) evictIdle(before time.Time) {
	m.mu.Lock()
	var victims []*session
	for id, s := range m.sessions {
		s.mu.Lock()
		idle := s.turn == nil && s.lastUsed.Before(before) && s.bus.subscriberCount() == 0
		s.mu.Unlock()
		if idle {
			victims = append(victims, s)
			delete(m.sessions, id)
			m.seqFloor[id] = s.bus.lastSeq()
		}
	}
	m.mu.Unlock()
	for _, s := range victims {
		s.mu.Lock()
		conv := s.conv
		s.conv = nil
		s.mu.Unlock()
		if conv != nil {
			_ = conv.Close()
		}
		s.bus.close()
		m.log.Debug("bot: unloaded idle session", "session", s.id)
	}
}

func (s *session) info() SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := SessionInfo{
		ID:        s.id,
		Title:     s.title,
		Model:     s.model,
		Status:    SessionIdle,
		MsgCount:  len(s.messages),
		LastSeq:   s.bus.lastSeq(),
		CreatedAt: s.createdAt,
		UpdatedAt: s.updatedAt,
	}
	if s.turn != nil {
		info.Status = SessionRunning
		info.TurnID = s.turn.id
		for _, pq := range s.turn.questions {
			info.Pending = append(info.Pending, pq.q)
		}
	}
	return info
}

// turnInteraction routes ask_clarification to whichever client answers
// first. The question stays pending (and listed in SessionInfo.Pending) until
// answered or the turn ends, so a client that connects late still sees it.
type turnInteraction struct {
	m *Manager
	s *session
	t *activeTurn
}

func (ti *turnInteraction) AskQuestion(ctx context.Context, question string, options []string) (string, error) {
	pq := &pendingQuestion{
		q:      Question{ID: "q_" + randomHex(6), Text: question, Options: options},
		answer: make(chan string, 1),
	}
	ti.s.mu.Lock()
	if ti.s.turn != ti.t {
		ti.s.mu.Unlock()
		return "", ErrNotRunning
	}
	ti.t.questions[pq.q.ID] = pq
	q := pq.q
	// Freeze the execution budget before the question reaches clients: any
	// unanswered question keeps the clock paused (reference counted).
	ti.t.clock.Pause()
	ti.s.bus.publish(Event{SessionID: ti.s.id, TurnID: ti.t.id, Type: EventQuestion, Question: &q})
	hooks := ti.t.hooks
	ti.s.mu.Unlock()
	if hooks != nil {
		hooks.QuestionAsked(q)
	}
	defer ti.t.clock.Resume()

	select {
	case ans, ok := <-pq.answer:
		if !ok {
			return "", context.Canceled
		}
		return ans, nil
	case <-ctx.Done():
		ti.s.mu.Lock()
		delete(ti.t.questions, pq.q.ID)
		ti.s.mu.Unlock()
		if hooks != nil {
			hooks.QuestionSettled(pq.q.ID, "", false)
		}
		return "", ctx.Err()
	}
}

func (b *bus) subscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func orNow(t, now time.Time) time.Time {
	if t.IsZero() {
		return now
	}
	return t
}
