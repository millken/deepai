package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/hook"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/schedule"
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
	// Bots persists multi-bot personas (optional; defaults to Store when it
	// implements BotRegistry, else an in-memory registry with "default").
	Bots BotRegistry
	// Secrets persists machine-sealed per-bot credentials for {{secret:NAME}}
	// placeholders (optional; defaults to an in-memory store).
	Secrets SecretStore
	// BotDataDir is the root directory for per-bot state on disk (e.g.
	// ~/.deepai/bot/bots/<bot_id>/skills/); cleaned up on DeleteBot.
	BotDataDir string
	// WorkDir is recorded on created sessions (the store's cwd column) and
	// scopes the Manager to them: sessions from other directories are
	// neither listed nor opened (ErrNotFound). Empty disables scoping.
	WorkDir string
	// UploadBaseDir is the fallback root directory for per-session uploaded
	// attachments when Conversation does not implement SandboxConversation.
	UploadBaseDir string
	// ReplayEvents bounds each session's replay buffer (default
	// DefaultReplayEvents).
	ReplayEvents int
	// IdleTimeout unloads a session that has had no turn and no subscriber
	// for this long, releasing its sandbox. 0 means 30 minutes; negative
	// disables eviction.
	IdleTimeout time.Duration
	// TurnTimeout bounds one turn. 0 means no limit beyond Cancel.
	TurnTimeout time.Duration
	// Hook is an optional outbound notification emitter (pkg/hook).
	Hook   hook.Emitter
	Logger *slog.Logger
}

// Manager owns the bot's sessions. It is safe for concurrent use; every
// transport is a thin adapter over its methods.
type Manager struct {
	opts      Options
	bots      BotRegistry
	secrets   SecretStore
	schedules *schedule.Service
	log       *slog.Logger
	owner     models.LockOwner
	startedAt time.Time

	baseCtx   context.Context
	cancelAll context.CancelFunc
	turns     sync.WaitGroup
	global    *bus

	botMu sync.Mutex // serializes CreateBot / UpdateBot / DeleteBot

	mu           sync.Mutex
	sessions     map[string]*session
	seqFloor     map[string]int64 // last seq of sessions unloaded by the janitor
	deletingBots map[string]struct{}
	botTurns     map[string]int
	closed       bool
	stopJan      chan struct{}
	metrics      *Metrics
	audit        *AuditLogger
}

type session struct {
	id    string
	botID string

	mu             sync.Mutex
	title          string
	model          string
	createdAt      time.Time
	updatedAt      time.Time
	messages       []models.Message
	conv           Conversation
	turn           *activeTurn
	queue          []QueuedMessage
	attachments    map[string]Attachment
	artifacts      map[string]Artifact
	uploadBytes    int64
	ownedUploadDir string
	lastUsed       time.Time
	bus            *bus
}

type activeTurn struct {
	id               string
	cancel           context.CancelFunc
	clock            *turnClock
	hooks            turnHooks
	maxToolCalls     int
	botProfile       BotProfile
	toolAllowlist    []string
	disableSelfTrain bool
	steer            *steerBuffer
	done             chan struct{}
	summary          *TurnSummary
	questions        map[string]*pendingQuestion
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
	bots := opts.Bots
	if bots == nil {
		if br, ok := opts.Store.(BotRegistry); ok {
			bots = br
		} else {
			bots = NewMemoryStore()
		}
	}
	if err := bots.EnsureDefault(context.Background()); err != nil {
		return nil, fmt.Errorf("bot: ensure default bot: %w", err)
	}
	secrets := opts.Secrets
	if secrets == nil {
		secrets = NewMemorySecretStore()
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
		bots:      bots,
		secrets:   secrets,
		log:       log,
		owner:     models.LockOwner{PID: os.Getpid(), Host: host},
		startedAt: time.Now().UTC(),
		baseCtx:   ctx,
		// The cause propagates to every turn's context, so turn_end can
		// report cancel_source=shutdown (design §3.2).
		cancelAll:    func() { cancel(turnStopCause{source: string(cancelSourceShutdown), err: context.Canceled}) },
		global:       newBus(opts.ReplayEvents),
		sessions:     map[string]*session{},
		seqFloor:     map[string]int64{},
		deletingBots: map[string]struct{}{},
		botTurns:     map[string]int{},
		stopJan:      make(chan struct{}),
		metrics:      NewMetrics(),
	}
	if ar, ok := opts.Runtime.(*AgentRuntime); ok {
		ar.BindGovernance(m.bots, m.secrets, nil, m.NotifyBotUpdated, m.MutateBot)
	}
	if opts.IdleTimeout > 0 {
		go m.janitor()
	}
	return m, nil
}

// NotifyBotUpdated broadcasts a bot_updated lifecycle event when an in-turn
// self-training tool mutates a BotProfile directly via BotRegistry.
func (m *Manager) NotifyBotUpdated(b BotProfile) {
	cp := b.Clone()
	m.global.publish(Event{BotID: cp.ID, Type: EventBotUpdated, Bot: &cp})
}

// SetSchedules attaches the optional schedule.Service so ListBots and
// DeleteBot can inspect, cancel and cascade-delete bot-owned schedules.
// Metrics returns the Manager's Prometheus metrics collector.
func (m *Manager) Metrics() *Metrics { return m.metrics }

// SetAudit attaches an optional AuditLogger so schedule run completions and
// other manager-level lifecycle actions record audit entries.
func (m *Manager) SetAudit(a *AuditLogger) {
	m.mu.Lock()
	m.audit = a
	m.mu.Unlock()
	if ar, ok := m.opts.Runtime.(*AgentRuntime); ok {
		ar.BindGovernance(m.bots, m.secrets, a, m.NotifyBotUpdated, m.MutateBot)
	}
}

func (m *Manager) auditLogger() *AuditLogger {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.audit
}

func (m *Manager) SetSchedules(svc *schedule.Service) {
	m.mu.Lock()
	m.schedules = svc
	m.mu.Unlock()
}

// IsBotDeleting reports whether DeleteBot is currently draining botID.
func (m *Manager) IsBotDeleting(id string) bool {
	id = effectiveBotID(id)
	m.mu.Lock()
	_, ok := m.deletingBots[id]
	m.mu.Unlock()
	return ok
}

func (m *Manager) decBotTurn(botID string) {
	m.mu.Lock()
	if n := m.botTurns[botID]; n <= 1 {
		delete(m.botTurns, botID)
	} else {
		m.botTurns[botID] = n - 1
	}
	m.mu.Unlock()
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
	info.DefaultBotID = DefaultBotID
	if bots, err := m.ListBots(context.Background()); err == nil {
		info.Bots = bots
	}
	info.StartedAt = m.startedAt
	return info
}

// ─────────────────────────── Bot CRUD ───────────────────────────

// ListBots lists all registered bots with per-workdir live counts.
func (m *Manager) ListBots(ctx context.Context) ([]BotSummary, error) {
	profiles, err := m.bots.ListBots(ctx)
	if err != nil {
		return nil, err
	}
	activeByBot := map[string]int{}
	m.mu.Lock()
	for _, s := range m.sessions {
		s.mu.Lock()
		running := s.turn != nil
		s.mu.Unlock()
		if running {
			activeByBot[effectiveBotID(s.botID)]++
		}
	}
	sched := m.schedules
	m.mu.Unlock()

	bs, hasBotStore := m.opts.Store.(BotStore)
	out := make([]BotSummary, 0, len(profiles))
	for _, b := range profiles {
		sum := BotSummary{BotProfile: b, ActiveTurns: activeByBot[b.ID]}
		if hasBotStore {
			if list, err := bs.ListRecentForBot(b.ID, m.opts.WorkDir, 0); err == nil {
				sum.SessionCount = len(list)
			}
		} else if b.ID == DefaultBotID {
			if list, err := m.opts.Store.ListRecentInDir(m.opts.WorkDir, 0); err == nil {
				sum.SessionCount = len(list)
			}
		}
		if sched != nil {
			if list, err := sched.ListForBot(ctx, b.ID, "", 0); err == nil {
				sum.ScheduleCount = len(list)
			}
		}
		out = append(out, sum)
	}
	return out, nil
}

// GetBot returns one BotProfile by ID.
func (m *Manager) GetBot(ctx context.Context, id string) (BotProfile, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		id = DefaultBotID
	}
	return m.bots.GetBot(ctx, id)
}

// CreateBot validates and registers a new bot persona.
func (m *Manager) CreateBot(ctx context.Context, req CreateBotRequest) (BotProfile, error) {
	m.botMu.Lock()
	defer m.botMu.Unlock()
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	now := time.Now().UTC()
	var ext BotExtensions
	if req.Extensions != nil {
		ext = normalizeBotExtensions(*req.Extensions)
	}
	b := BotProfile{
		ID:            strings.TrimSpace(req.ID),
		Name:          strings.TrimSpace(req.Name),
		Title:         strings.TrimSpace(req.Title),
		Description:   strings.TrimSpace(req.Description),
		Instructions:  req.Instructions,
		DefaultModel:  strings.TrimSpace(req.DefaultModel),
		ToolAllowlist: normalizeToolAllowlist(req.ToolAllowlist),
		Enabled:       enabled,
		Extensions:    ext,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := m.validateBotConfigWithSecrets(ctx, b, true); err != nil {
		return BotProfile{}, err
	}
	created, err := m.bots.CreateBot(ctx, b)
	if err != nil {
		return BotProfile{}, err
	}
	cp := created.Clone()
	m.global.publish(Event{BotID: cp.ID, Type: EventBotCreated, Bot: &cp})
	return created, nil
}

// UpdateBot patches mutable fields of an existing bot persona.
func (m *Manager) UpdateBot(ctx context.Context, id string, req UpdateBotRequest) (BotProfile, error) {
	m.botMu.Lock()
	defer m.botMu.Unlock()
	cur, err := m.bots.GetBot(ctx, strings.TrimSpace(id))
	if err != nil {
		return BotProfile{}, err
	}
	wasEnabled := cur.Enabled
	if req.Name != nil {
		cur.Name = strings.TrimSpace(*req.Name)
	}
	if req.Title != nil {
		cur.Title = strings.TrimSpace(*req.Title)
	}
	if req.Description != nil {
		cur.Description = strings.TrimSpace(*req.Description)
	}
	if req.Instructions != nil {
		cur.Instructions = *req.Instructions
	}
	if req.DefaultModel != nil {
		cur.DefaultModel = strings.TrimSpace(*req.DefaultModel)
	}
	if req.ToolAllowlist != nil {
		cur.ToolAllowlist = normalizeToolAllowlist(*req.ToolAllowlist)
	}
	if req.Enabled != nil {
		cur.Enabled = *req.Enabled
	}
	if req.Extensions != nil {
		cur.Extensions = normalizeBotExtensions(*req.Extensions)
	}
	cur.UpdatedAt = time.Now().UTC()
	if err := m.validateBotConfigWithSecrets(ctx, cur, false); err != nil {
		return BotProfile{}, err
	}
	updated, err := m.bots.UpdateBot(ctx, cur)
	if err != nil {
		return BotProfile{}, err
	}
	cp := updated.Clone()
	m.global.publish(Event{BotID: cp.ID, Type: EventBotUpdated, Bot: &cp})
	if !wasEnabled && updated.Enabled {
		m.wakeQueuedForBot(updated.ID)
	}
	return updated, nil
}

// MutateBot applies an atomic read-modify-write mutation to a bot profile under
// m.botMu so in-turn update_bot_persona calls serialize cleanly with REST/WS
// UpdateBot and DeleteBot without lost updates.
func (m *Manager) MutateBot(ctx context.Context, id string, mutate func(*BotProfile) ([]string, error)) (BotProfile, error) {
	m.botMu.Lock()
	defer m.botMu.Unlock()
	id = effectiveBotID(id)
	if m.IsBotDeleting(id) {
		return BotProfile{}, fmt.Errorf("%w: %s", ErrBotDisabled, id)
	}
	cur, err := m.bots.GetBot(ctx, id)
	if err != nil {
		return BotProfile{}, err
	}
	extraChecks, err := mutate(&cur)
	if err != nil {
		return BotProfile{}, err
	}
	cur.Extensions = normalizeBotExtensions(cur.Extensions)
	cur.UpdatedAt = time.Now().UTC()
	if err := m.validateBotConfigWithSecrets(ctx, cur, false, extraChecks...); err != nil {
		return BotProfile{}, err
	}
	updated, err := m.bots.UpdateBot(ctx, cur)
	if err != nil {
		return BotProfile{}, err
	}
	cp := updated.Clone()
	m.global.publish(Event{BotID: cp.ID, Type: EventBotUpdated, Bot: &cp})
	return updated, nil
}

// wakeQueuedForBot resumes queued follow-up turns on idle sessions when a
// disabled bot transitions back to enabled.
func (m *Manager) wakeQueuedForBot(botID string) {
	botID = effectiveBotID(botID)
	var idle []*session
	m.mu.Lock()
	for _, s := range m.sessions {
		if effectiveBotID(s.botID) == botID {
			s.mu.Lock()
			if s.turn == nil && len(s.queue) > 0 {
				idle = append(idle, s)
			}
			s.mu.Unlock()
		}
	}
	m.mu.Unlock()
	for _, s := range idle {
		m.startNextQueued(s)
	}
}

func (m *Manager) validateBotConfig(b BotProfile, isCreate bool) error {
	if err := validateBotProfile(b, isCreate); err != nil {
		return err
	}
	if b.DefaultModel != "" && !m.opts.Runtime.HasModel(b.DefaultModel) {
		return fmt.Errorf("%w: %s", ErrUnknownModel, b.DefaultModel)
	}
	if len(b.ToolAllowlist) > 0 {
		if tv, ok := m.opts.Runtime.(ToolValidator); ok {
			if missing := tv.ValidateToolAllowlist(b.ToolAllowlist); len(missing) > 0 {
				return fmt.Errorf("%w: %s", ErrUnknownTool, strings.Join(missing, ", "))
			}
		}
	}
	return nil
}

func (m *Manager) validateBotConfigWithSecrets(ctx context.Context, b BotProfile, isCreate bool, extraTexts ...string) error {
	if err := m.validateBotConfig(b, isCreate); err != nil {
		return err
	}
	extJSON, _ := json.Marshal(b.Extensions)
	texts := make([]string, 0, 4+len(extraTexts))
	texts = append(texts, b.Title, b.Description, b.Instructions, string(extJSON))
	texts = append(texts, extraTexts...)
	if err := rejectStoredBotSecrets(ctx, m.secrets, b.ID, texts...); err != nil {
		if strings.Contains(err.Error(), "plaintext value of secret") || strings.Contains(err.Error(), "sealed ciphertexts") {
			return fmt.Errorf("%w: %v", ErrInvalidBot, err)
		}
		return err
	}
	return nil
}

// DeleteBot removes a bot persona. Without force, refuses if any sessions,
// schedules or active turns exist; with force, cancels local active turns,
// verifies no external process holds a session lock, and cascades deletion
// across schedules, sessions, messages and memory (docs/BOT_MULTI_DESIGN.md §5.2).
func (m *Manager) DeleteBot(ctx context.Context, id string, force bool) error {
	m.botMu.Lock()
	defer m.botMu.Unlock()
	id = strings.TrimSpace(id)
	if id == DefaultBotID {
		return ErrBotDeleteForbidden
	}
	cur, err := m.bots.GetBot(ctx, id)
	if err != nil {
		return err
	}
	bs, hasBotStore := m.opts.Store.(BotStore)
	if hasBotStore {
		locked, err := bs.HasExternalBotLock(id, m.owner)
		if err != nil {
			return err
		}
		if locked {
			return &models.ErrSessionLocked{SessionID: id}
		}
	}

	// Mark the bot as deleting before draining so concurrent SendForBot,
	// CreateSessionForBot, and schedule Fire calls are rejected during the
	// cancel/wait window.
	m.mu.Lock()
	m.deletingBots[id] = struct{}{}
	sched := m.schedules
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.deletingBots, id)
		m.mu.Unlock()
	}()

	waitCtx, cancelWait := context.WithTimeout(ctx, 10*time.Second)
	defer cancelWait()
	cancelledSchedules := false

	for {
		var activeTurns []*activeTurn
		m.mu.Lock()
		for _, s := range m.sessions {
			if effectiveBotID(s.botID) == id {
				s.mu.Lock()
				if s.turn != nil {
					activeTurns = append(activeTurns, s.turn)
				}
				s.mu.Unlock()
			}
		}
		inFlight := m.botTurns[id]
		m.mu.Unlock()

		if len(activeTurns) == 0 && inFlight == 0 {
			break
		}
		if !force {
			return ErrBusy
		}
		if sched != nil && !cancelledSchedules {
			_ = sched.CancelActiveByBot(ctx, id)
			cancelledSchedules = true
		}
		for _, t := range activeTurns {
			t.clock.CancelWithSource(cancelSourceClient)
		}
		if len(activeTurns) > 0 {
			for _, t := range activeTurns {
				select {
				case <-t.done:
				case <-waitCtx.Done():
					return ErrBusy
				}
			}
		} else {
			select {
			case <-time.After(10 * time.Millisecond):
			case <-waitCtx.Done():
				return ErrBusy
			}
		}
	}

	var sessCount, schedCount int
	if hasBotStore {
		sessCount, err = bs.CountBotSessions(id)
		if err != nil {
			return err
		}
	}
	if sched != nil {
		schedCount, err = sched.CountByBot(ctx, id)
		if err != nil {
			return err
		}
	}
	if !force && (sessCount > 0 || schedCount > 0) {
		return &BotHasSessionsError{Sessions: sessCount, Schedules: schedCount}
	}

	// Re-scan and unload all in-memory sessions belonging to this bot and release sandboxes.
	var loaded []*session
	m.mu.Lock()
	for sid, s := range m.sessions {
		if effectiveBotID(s.botID) == id {
			loaded = append(loaded, s)
			delete(m.sessions, sid)
			delete(m.seqFloor, sid)
		}
	}
	m.mu.Unlock()
	for _, s := range loaded {
		s.mu.Lock()
		conv := s.conv
		s.conv = nil
		ownedDir := s.ownedUploadDir
		s.ownedUploadDir = ""
		s.mu.Unlock()
		if conv != nil {
			_ = conv.Close()
		}
		if ownedDir != "" {
			_ = os.RemoveAll(ownedDir)
		}
		s.bus.publish(Event{BotID: effectiveBotID(s.botID), SessionID: s.id, Type: EventSessionDeleted})
		s.bus.close()
	}

	if sched != nil {
		if err := sched.DeleteByBot(ctx, id); err != nil {
			return err
		}
	}
	if m.secrets != nil {
		_ = m.secrets.DeleteAllSecretsForBot(ctx, id)
	}
	if hasBotStore {
		if err := bs.DeleteBotCascade(id); err != nil {
			return err
		}
	} else {
		if err := m.bots.DeleteBot(ctx, id); err != nil {
			return err
		}
	}
	if base := strings.TrimSpace(m.opts.BotDataDir); base != "" {
		_ = os.RemoveAll(filepath.Join(base, id))
	}

	cp := cur.Clone()
	m.global.publish(Event{BotID: cp.ID, Type: EventBotDeleted, Bot: &cp})
	return nil
}

// Secrets returns the Manager's per-bot SecretStore.
func (m *Manager) Secrets() SecretStore { return m.secrets }

// SetSecret seals and stores a named credential scoped to botID.
func (m *Manager) SetSecret(ctx context.Context, botID, name, plaintext string) (SecretMeta, error) {
	botID = effectiveBotID(botID)
	if _, err := m.bots.GetBot(ctx, botID); err != nil {
		return SecretMeta{}, err
	}
	return m.secrets.SetSecret(ctx, botID, name, plaintext)
}

// ListSecrets lists credential metadata (never plaintext values) scoped to botID.
func (m *Manager) ListSecrets(ctx context.Context, botID string) ([]SecretMeta, error) {
	botID = effectiveBotID(botID)
	if _, err := m.bots.GetBot(ctx, botID); err != nil {
		return nil, err
	}
	return m.secrets.ListSecrets(ctx, botID)
}

// DeleteSecret removes one named credential scoped to botID.
func (m *Manager) DeleteSecret(ctx context.Context, botID, name string) error {
	botID = effectiveBotID(botID)
	if _, err := m.bots.GetBot(ctx, botID); err != nil {
		return err
	}
	return m.secrets.DeleteSecret(ctx, botID, name)
}

// ListBotSkills lists bot-exclusive skills persisted under BotDataDir/<bot_id>/skills.
func (m *Manager) ListBotSkills(ctx context.Context, botID string) ([]BotSkillInfo, error) {
	botID = effectiveBotID(botID)
	if _, err := m.bots.GetBot(ctx, botID); err != nil {
		return nil, err
	}
	return listBotSkillsOnDisk(m.opts.BotDataDir, botID)
}

// DeleteBotSkill removes one bot-exclusive skill directory for botID.
func (m *Manager) DeleteBotSkill(ctx context.Context, botID, name string) error {
	botID = effectiveBotID(botID)
	if _, err := m.bots.GetBot(ctx, botID); err != nil {
		return err
	}
	return deleteBotSkillOnDisk(m.opts.BotDataDir, botID, name)
}

func (m *Manager) botDisplayName(botID string) string {
	if b, err := m.bots.GetBot(context.Background(), effectiveBotID(botID)); err == nil && b.Name != "" {
		return b.Name
	}
	return ""
}

func (m *Manager) sessionInfoWithBotName(s *session, botName string) SessionInfo {
	info := s.info()
	if botName != "" {
		info.BotName = botName
	} else {
		info.BotName = m.botDisplayName(s.botID)
	}
	return info
}

// CreateSession creates and loads a new session for the default bot.
func (m *Manager) CreateSession(req CreateSessionRequest) (SessionInfo, error) {
	return m.CreateSessionForBot(DefaultBotID, req)
}

// CreateSessionForBot creates and loads a new session owned by botID.
func (m *Manager) CreateSessionForBot(botID string, req CreateSessionRequest) (SessionInfo, error) {
	botID = effectiveBotID(botID)
	profile, err := m.bots.GetBot(context.Background(), botID)
	if err != nil {
		return SessionInfo{}, err
	}
	if !profile.Enabled {
		return SessionInfo{}, fmt.Errorf("%w: %s", ErrBotDisabled, botID)
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		if profile.DefaultModel != "" && m.opts.Runtime.HasModel(profile.DefaultModel) {
			model = profile.DefaultModel
		} else {
			model = m.opts.Runtime.DefaultModel()
		}
	} else if !m.opts.Runtime.HasModel(model) {
		return SessionInfo{}, fmt.Errorf("%w: %s", ErrUnknownModel, model)
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return SessionInfo{}, ErrClosed
	}
	if _, deleting := m.deletingBots[botID]; deleting {
		m.mu.Unlock()
		return SessionInfo{}, fmt.Errorf("%w: %s", ErrBotDisabled, botID)
	}
	m.mu.Unlock()

	createOpts := models.CreateOpts{
		Title:  strings.TrimSpace(req.Title),
		Model:  model,
		CWD:    m.opts.WorkDir,
		Source: SessionSource,
	}
	var rec *models.Session
	if bs, ok := m.opts.Store.(BotStore); ok {
		rec, err = bs.CreateForBot(botID, createOpts)
	} else {
		if botID != DefaultBotID {
			return SessionInfo{}, fmt.Errorf("%w: store does not support multi-bot", ErrUnknownBot)
		}
		rec, err = m.opts.Store.Create(createOpts)
	}
	if err != nil {
		return SessionInfo{}, fmt.Errorf("create session: %w", err)
	}
	now := time.Now().UTC()
	s := &session{
		id:        rec.ID,
		botID:     botID,
		title:     rec.Title,
		model:     model,
		createdAt: orNow(rec.CreatedAt, now),
		updatedAt: orNow(rec.UpdatedAt, now),
		lastUsed:  now,
		bus:       newBus(m.opts.ReplayEvents),
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = m.opts.Store.Delete(s.id)
		return SessionInfo{}, ErrClosed
	}
	if _, deleting := m.deletingBots[botID]; deleting {
		m.mu.Unlock()
		_ = m.opts.Store.Delete(s.id)
		return SessionInfo{}, fmt.Errorf("%w: %s", ErrBotDisabled, botID)
	}
	m.sessions[s.id] = s
	m.mu.Unlock()

	info := m.sessionInfoWithBotName(s, profile.Name)
	m.global.publish(Event{BotID: effectiveBotID(s.botID), SessionID: s.id, Type: EventSessionCreated, Session: &info})
	return info, nil
}

// ListSessions lists recent sessions for the default bot in this workdir.
func (m *Manager) ListSessions(limit int) ([]SessionInfo, error) {
	return m.ListSessionsForBot(DefaultBotID, limit)
}

// ListSessionsForBot merges the store's recent sessions for botID with live state.
func (m *Manager) ListSessionsForBot(botID string, limit int) ([]SessionInfo, error) {
	botID = effectiveBotID(botID)
	profile, err := m.bots.GetBot(context.Background(), botID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	var metas []models.SessionMeta
	if bs, ok := m.opts.Store.(BotStore); ok {
		metas, err = bs.ListRecentForBot(botID, m.opts.WorkDir, limit)
	} else {
		metas, err = m.opts.Store.ListRecentInDir(m.opts.WorkDir, limit)
	}
	if err != nil {
		return nil, err
	}
	out := make([]SessionInfo, 0, len(metas))
	for _, meta := range metas {
		m.mu.Lock()
		s := m.sessions[meta.ID]
		m.mu.Unlock()
		if s != nil {
			out = append(out, m.sessionInfoWithBotName(s, profile.Name))
			continue
		}
		out = append(out, SessionInfo{
			ID: meta.ID, BotID: botID, BotName: profile.Name, Title: meta.Title, Status: SessionIdle, MsgCount: meta.MsgCount,
			CreatedAt: meta.CreatedAt, UpdatedAt: meta.UpdatedAt,
		})
	}
	return out, nil
}

// GetSession returns a default-bot session's state and transcript.
func (m *Manager) GetSession(id string) (SessionInfo, []models.Message, error) {
	return m.GetSessionForBot(DefaultBotID, id)
}

// GetSessionForBot returns a bot-scoped session's state and transcript.
func (m *Manager) GetSessionForBot(botID, id string) (SessionInfo, []models.Message, error) {
	s, err := m.loadForBot(botID, id)
	if err != nil {
		return SessionInfo{}, nil, err
	}
	s.mu.Lock()
	msgs := append(make([]models.Message, 0, len(s.messages)), s.messages...)
	s.mu.Unlock()
	return m.sessionInfoWithBotName(s, ""), msgs, nil
}

// RenameSession sets a default-bot session's title.
func (m *Manager) RenameSession(id, title string) (SessionInfo, error) {
	return m.RenameSessionForBot(DefaultBotID, id, title)
}

// RenameSessionForBot sets a bot-scoped session's title.
func (m *Manager) RenameSessionForBot(botID, id, title string) (SessionInfo, error) {
	s, err := m.loadForBot(botID, id)
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
	return m.sessionInfoWithBotName(s, ""), nil
}

// DeleteSession removes an idle default-bot session from the store.
func (m *Manager) DeleteSession(id string) error {
	return m.DeleteSessionForBot(DefaultBotID, id)
}

// DeleteSessionForBot removes an idle bot-scoped session from the store.
func (m *Manager) DeleteSessionForBot(botID, id string) error {
	s, err := m.loadForBot(botID, id)
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
	ownedDir := s.ownedUploadDir
	s.ownedUploadDir = ""
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
	if ownedDir != "" {
		_ = os.RemoveAll(ownedDir)
	}
	delEvt := Event{BotID: effectiveBotID(s.botID), SessionID: id, Type: EventSessionDeleted}
	s.bus.publish(delEvt)
	s.bus.close()
	m.global.publish(delEvt)
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
	// ToolAllowlist optionally narrows the tool registry for this turn (e.g.
	// per-schedule tool_allowlist, intersected with the bot's own allowlist).
	ToolAllowlist []string
	// DisableSelfTrain suppresses save_bot_skill and update_bot_persona for
	// this turn (used by IM channel adapters unless explicitly opted in).
	DisableSelfTrain bool
	// Hooks observes questions; scheduled turns use it for waiting_input /
	// answer_timeout. It is deliberately unexported: transports must not set it.
	Hooks turnHooks
}

// Send starts a turn on a default-bot session and returns without waiting for it.
func (m *Manager) Send(id string, req SendRequest) (*Turn, error) {
	return m.SendForBot(DefaultBotID, id, req, SendOptions{})
}

// SendWithOptions starts a turn on a default-bot session with internal metadata.
func (m *Manager) SendWithOptions(id string, req SendRequest, opts SendOptions) (*Turn, error) {
	return m.SendForBot(DefaultBotID, id, req, opts)
}

// SendForBot starts a turn on a bot-scoped session, loading the bot's latest
// BotProfile on every turn so PATCH edits take effect immediately without cache
// invalidation (docs/BOT_MULTI_DESIGN.md §2). For queue/steer semantics use
// SubmitForBot.
func (m *Manager) SendForBot(botID, id string, req SendRequest, opts SendOptions) (*Turn, error) {
	res, err := m.SubmitForBot(botID, id, req, opts)
	if err != nil {
		return nil, err
	}
	return res.Turn, nil
}

type preparedTurn struct {
	ctx       context.Context
	turn      *activeTurn
	conv      Conversation
	model     string
	history   []models.Message
	needTitle bool
}

// prepareTurnLocked initializes a new activeTurn on an idle session. Caller
// must hold s.mu and have already incremented m.turns and m.botTurns[botID].
func (m *Manager) prepareTurnLocked(s *session, profile BotProfile, content, model string, images []models.MessageImage, extraMeta map[string]string, opts SendOptions) (preparedTurn, error) {
	if s.conv == nil {
		conv, err := m.opts.Runtime.NewConversation(s.id)
		if err != nil {
			return preparedTurn{}, fmt.Errorf("start conversation: %w", err)
		}
		s.conv = conv
	}
	if model != "" {
		s.model = model
	}
	if s.model == "" {
		if profile.DefaultModel != "" && m.opts.Runtime.HasModel(profile.DefaultModel) {
			s.model = profile.DefaultModel
		} else {
			s.model = m.opts.Runtime.DefaultModel()
		}
	}
	if locker, ok := m.opts.Store.(Locker); ok {
		if err := locker.AcquireSessionLock(s.id, m.owner, false); err != nil {
			return preparedTurn{}, err
		}
	}
	budget := opts.Timeout
	if m.opts.TurnTimeout > 0 && (budget <= 0 || m.opts.TurnTimeout < budget) {
		budget = m.opts.TurnTimeout
	}
	ctx, clock := newTurnClock(m.baseCtx, budget)
	t := &activeTurn{
		id:               "turn_" + randomHex(6),
		cancel:           clock.cancelPlain,
		clock:            clock,
		hooks:            opts.Hooks,
		maxToolCalls:     opts.MaxToolCalls,
		botProfile:       profile.Clone(),
		toolAllowlist:    append([]string(nil), opts.ToolAllowlist...),
		disableSelfTrain: opts.DisableSelfTrain,
		steer:            newSteerBuffer(),
		done:             make(chan struct{}),
		questions:        map[string]*pendingQuestion{},
	}
	ctx = tools.WithScheduleRun(ctx, opts.ScheduleID, opts.RunID)
	ctx = tools.WithTurnID(ctx, t.id)
	s.turn = t
	s.lastUsed = time.Now().UTC()
	var mergedMeta map[string]string
	if len(opts.Metadata) > 0 || len(extraMeta) > 0 {
		mergedMeta = make(map[string]string, len(opts.Metadata)+len(extraMeta))
		for k, v := range extraMeta {
			mergedMeta[k] = v
		}
		for k, v := range opts.Metadata {
			mergedMeta[k] = v
		}
	}
	userMsg := models.Message{
		ID:        s.id + "_" + randomHex(6),
		SessionID: s.id,
		Role:      models.RoleHuman,
		Content:   content,
		Images:    images,
		Metadata:  mergedMeta,
		CreatedAt: time.Now().UTC(),
	}
	if err := m.opts.Store.AppendMessage(s.id, userMsg); err != nil {
		m.log.Warn("bot: persist user message", "session", s.id, "err", err)
	}
	s.messages = append(s.messages, userMsg)
	return preparedTurn{
		ctx:       ctx,
		turn:      t,
		conv:      s.conv,
		model:     s.model,
		history:   append([]models.Message(nil), s.messages...),
		needTitle: s.title == "",
	}, nil
}

// Closing reports whether Close has begun. The schedule Dispatcher uses it
// to distinguish a shutdown cancellation from a deliberate client cancel.
func (m *Manager) Closing() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// Cancel interrupts a default-bot session's running turn.
func (m *Manager) Cancel(id string) error {
	return m.CancelForBot(DefaultBotID, id)
}

// CancelForBot interrupts a bot-scoped session's running turn.
func (m *Manager) CancelForBot(botID, id string) error {
	s, err := m.loadForBot(botID, id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	t := s.turn
	s.mu.Unlock()
	if t == nil {
		return ErrNotRunning
	}
	t.clock.CancelWithSource(cancelSourceClient)
	return nil
}

// cancelWithSource lets the schedule Dispatcher attribute a cancellation to
// the scheduler (answer_timeout / overlap=replace) regardless of owning bot.
func (m *Manager) cancelWithSource(id string, src cancelSource) error {
	s, err := m.loadAny(id)
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

// Answer resolves a pending ask_clarification question on a default-bot session.
func (m *Manager) Answer(id, questionID, answer string) error {
	return m.AnswerForBot(DefaultBotID, id, questionID, answer)
}

// AnswerForBot resolves a pending ask_clarification question on a bot-scoped session.
func (m *Manager) AnswerForBot(botID, id, questionID, answer string) error {
	s, err := m.loadForBot(botID, id)
	if err != nil {
		return err
	}
	return m.answerSession(s, questionID, answer, false)
}

// answerAuto resolves a question on behalf of the bot (unattended runs, any bot).
func (m *Manager) answerAuto(id, questionID, answer string) error {
	s, err := m.loadAny(id)
	if err != nil {
		return err
	}
	return m.answerSession(s, questionID, answer, true)
}

func (m *Manager) answerSession(s *session, questionID, answer string, auto bool) error {
	id := s.id
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

// Subscribe follows one default-bot session's events.
func (m *Manager) Subscribe(id string, since int64) (*Subscription, []Event, bool, error) {
	return m.SubscribeForBot(DefaultBotID, id, since)
}

// SubscribeForBot follows one bot-scoped session's events.
func (m *Manager) SubscribeForBot(botID, id string, since int64) (*Subscription, []Event, bool, error) {
	s, err := m.loadForBot(botID, id)
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
		ownedDir := s.ownedUploadDir
		s.ownedUploadDir = ""
		if len(s.queue) > 0 {
			s.queue = nil
			s.bus.publish(Event{
				BotID:     effectiveBotID(s.botID),
				SessionID: s.id,
				Type:      EventQueueCleared,
				Reason:    "shutdown",
			})
		}
		s.mu.Unlock()
		if conv != nil {
			_ = conv.Close()
		}
		if ownedDir != "" {
			_ = os.RemoveAll(ownedDir)
		}
		s.bus.close()
	}
	m.global.close()
	return err
}

func (m *Manager) runTurn(ctx context.Context, s *session, t *activeTurn, conv Conversation, model string, history []models.Message) {
	defer m.decBotTurn(effectiveBotID(s.botID))
	defer m.turns.Done()
	defer t.cancel()

	started := time.Now().UTC()
	botID := effectiveBotID(s.botID)
	startEvt := Event{BotID: botID, SessionID: s.id, TurnID: t.id, Type: EventTurnStart, Text: history[len(history)-1].Content}
	s.bus.publish(startEvt)
	m.global.publish(startEvt)

	stopRefresh := m.holdLock(s.id)

	// Publishing under s.mu orders every turn event before the s.turn = nil
	// below, so nothing a Runtime emits late (e.g. from an abandoned run)
	// can appear after this turn's turn_end.
	emit := func(evt Event) {
		evt.BotID = botID
		evt.SessionID = s.id
		evt.TurnID = t.id
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.turn == t {
			if evt.Type == EventToolCallEnd && evt.Tool != nil {
				m.metrics.ObserveToolCall(botID, evt.Tool.Name, string(evt.Tool.Status))
			}
			if evt.Type == EventArtifact && evt.Artifact != nil && evt.Artifact.ID != "" {
				if s.artifacts == nil {
					s.artifacts = map[string]Artifact{}
				}
				s.artifacts[evt.Artifact.ID] = *evt.Artifact
			}
			s.bus.publish(evt)
		}
	}
	out, runErr := conv.RunTurn(ctx, TurnInput{
		SessionID:         s.id,
		TurnID:            t.id,
		Model:             model,
		Messages:          history,
		Emit:              emit,
		Interaction:       &turnInteraction{m: m, s: s, t: t},
		Steer:             t.steer,
		MaxToolCalls:      t.maxToolCalls,
		BotID:             effectiveBotID(t.botProfile.ID),
		SystemPromptExtra: t.botProfile.Instructions,
		MemoryUserID:      "bot:" + effectiveBotID(t.botProfile.ID),
		BotToolAllowlist:  t.botProfile.ToolAllowlist,
		ToolAllowlist:     t.toolAllowlist,
		Extensions:        t.botProfile.Extensions.Clone(),
		DisableSelfTrain:  t.disableSelfTrain,
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
	m.metrics.ObserveTurnEnd(botID, summary)

	var unconsumedSteers []agent.SteerMessage
	if t.steer != nil {
		unconsumedSteers = t.steer.CloseAndDrain()
	}

	// turn_end goes out while s.turn is still t: a Send racing Done() must
	// not get its turn_start onto either bus ahead of this turn's end.
	endEvt := Event{BotID: botID, SessionID: s.id, TurnID: t.id, Type: EventTurnEnd, Turn: summary, Usage: summary.Usage}
	s.mu.Lock()
	s.bus.publish(endEvt)
	m.global.publish(endEvt)
	switch summary.Status {
	case TurnCancelled:
		if len(s.queue) > 0 {
			s.queue = nil
			s.bus.publish(Event{
				BotID:     botID,
				SessionID: s.id,
				Type:      EventQueueCleared,
				Reason:    "cancelled",
			})
		}
	case TurnCompleted, TurnFailed, TurnTimedOut:
		if len(unconsumedSteers) > 0 {
			promoted := make([]QueuedMessage, 0, len(unconsumedSteers))
			now := time.Now().UTC()
			for _, st := range unconsumedSteers {
				item := QueuedMessage{
					ID:               "q_" + randomHex(6),
					Content:          st.Content,
					Images:           st.Images,
					DisableSelfTrain: t.disableSelfTrain,
					CreatedAt:        now,
				}
				promoted = append(promoted, item)
				cp := item
				s.bus.publish(Event{
					BotID:     botID,
					SessionID: s.id,
					Type:      EventQueueEnqueued,
					QueueItem: &cp,
					Text:      item.Content,
				})
			}
			s.queue = append(promoted, s.queue...)
		}
	}
	s.turn = nil
	hasQueued := summary.Status == TurnCompleted && len(s.queue) > 0
	s.mu.Unlock()
	close(t.done)
	if m.opts.Hook != nil && t.hooks == nil {
		// Interactive turn notification (scheduled turns emit schedule_run_end).
		msg := hookSummary("Turn "+string(summary.Status), summary.FinalOutput, summary.Error)
		m.opts.Hook.Fire(context.Background(), hook.Event{
			Kind:      hook.EventTurnEnd,
			SessionID: s.id,
			WorkDir:   m.opts.WorkDir,
			Message:   msg,
		})
	}
	afterTurnDone()
	if hasQueued {
		m.startNextQueued(s)
	}
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

// load loads a default-bot session (kept as a convenience alias for tests).
func (m *Manager) load(id string) (*session, error) {
	return m.loadForBot(DefaultBotID, id)
}

// loadForBot loads session id and enforces that it belongs to botID and this
// process's WorkDir (returning ErrNotFound on mismatch).
func (m *Manager) loadForBot(botID, id string) (*session, error) {
	botID = effectiveBotID(botID)
	s, err := m.loadAny(id)
	if err != nil {
		return nil, err
	}
	if effectiveBotID(s.botID) != botID {
		return nil, ErrNotFound
	}
	return s, nil
}

// loadAny returns the in-memory session (regardless of botID, still enforcing
// WorkDir isolation), loading it from the store on first use.
func (m *Manager) loadAny(id string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	if s := m.sessions[id]; s != nil {
		return s, nil
	}
	ownerBot := DefaultBotID
	if bs, ok := m.opts.Store.(BotStore); ok {
		gotBot, gotCWD, err := bs.SessionOwner(id)
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				return nil, ErrNotFound
			}
			return nil, err
		}
		if m.opts.WorkDir != "" && gotCWD != m.opts.WorkDir {
			return nil, ErrNotFound
		}
		ownerBot = effectiveBotID(gotBot)
	} else if m.opts.WorkDir != "" {
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
		botID:     ownerBot,
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
		idle := s.turn == nil && len(s.queue) == 0 && s.lastUsed.Before(before) && s.bus.subscriberCount() == 0
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
		ownedDir := s.ownedUploadDir
		s.ownedUploadDir = ""
		s.mu.Unlock()
		if conv != nil {
			_ = conv.Close()
		}
		if ownedDir != "" {
			_ = os.RemoveAll(ownedDir)
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
		BotID:     effectiveBotID(s.botID),
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
	if len(s.queue) > 0 {
		info.Queue = make([]QueuedMessage, len(s.queue))
		copy(info.Queue, s.queue)
	}
	if len(s.artifacts) > 0 {
		arts := make([]Artifact, 0, len(s.artifacts))
		for _, a := range s.artifacts {
			arts = append(arts, a)
		}
		info.Artifacts = arts
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
	} else if ti.m.opts.Hook != nil {
		ti.m.opts.Hook.Fire(context.Background(), hook.Event{
			Kind:      hook.EventAsk,
			SessionID: ti.s.id,
			WorkDir:   ti.m.opts.WorkDir,
			Message:   "需要你回答：" + truncateRunes(q.Text, 500),
		})
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
