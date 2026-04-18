package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/models"
)

const defaultUpdateTimeout = 60 * time.Second

// Capacity limits for memory content.
const (
	MaxFactsPerSession = 30  // Maximum facts per session
	MaxFactContentLen  = 500 // Maximum characters per fact content
)

// Document is the durable memory snapshot for a single session.
type Document struct {
	SessionID string        `json:"session_id"`
	User      UserMemory    `json:"user"`
	History   HistoryMemory `json:"history"`
	Facts     []Fact        `json:"facts,omitempty"`
	Source    string        `json:"source,omitempty"`
	UpdatedAt time.Time     `json:"updated_at,omitempty"`
}

type UserMemory struct {
	WorkContext     string `json:"workContext,omitempty"`
	PersonalContext string `json:"personalContext,omitempty"`
	TopOfMind       string `json:"topOfMind,omitempty"`
}

type HistoryMemory struct {
	RecentMonths       string `json:"recentMonths,omitempty"`
	EarlierContext     string `json:"earlierContext,omitempty"`
	LongTermBackground string `json:"longTermBackground,omitempty"`
}

type Fact struct {
	ID             string    `json:"id"`
	Content        string    `json:"content"`
	Category       string    `json:"category,omitempty"`
	Confidence     float64   `json:"confidence,omitempty"`
	Source         string    `json:"source,omitempty"`
	RetrievalCount int       `json:"retrieval_count,omitempty"`
	HelpfulCount   int       `json:"helpful_count,omitempty"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
	UpdatedAt      time.Time `json:"updated_at,omitempty"`
}

// Update is the incremental output returned by the memory LLM.
type Update struct {
	User    UserMemory    `json:"user"`
	History HistoryMemory `json:"history"`
	Facts   []Fact        `json:"facts,omitempty"`
	Source  string        `json:"source,omitempty"`
}

type Storage interface {
	AutoMigrate(ctx context.Context) error
	Load(ctx context.Context, sessionID string) (Document, error)
	Save(ctx context.Context, doc Document) error
	IncrementRetrievalCounts(ctx context.Context, sessionID string, factIDs []string) error
	IncrementHelpfulCounts(ctx context.Context, sessionID string, factIDs []string) (int, error)
}

type Extractor interface {
	ExtractUpdate(ctx context.Context, current Document, messages []models.Message) (Update, error)
}

type Service struct {
	storage       Storage
	extractor     Extractor
	logger        *slog.Logger
	updateTimeout time.Duration
	queue         *UpdateQueue
	sessionMu     sync.Map // sessionID → *sync.Mutex ; serializes Update-to-Save per session
	lastRetrieved struct {
		mu   sync.RWMutex
		data map[string]*lastRetrieval // sessionID → last retrieved facts
	}
}

func NewService(logger *slog.Logger, storage Storage, extractor Extractor) *Service {
	svc := &Service{
		storage:       storage,
		extractor:     extractor,
		logger:        logger,
		updateTimeout: defaultUpdateTimeout,
	}
	svc.lastRetrieved.data = make(map[string]*lastRetrieval)
	svc.queue = newUpdateQueue(svc, defaultQueueSize)
	return svc
}

func (s *Service) WithUpdateTimeout(timeout time.Duration) *Service {
	if s != nil && timeout > 0 {
		s.updateTimeout = timeout
	}
	return s
}

// Close drains the update queue and releases resources.
func (s *Service) Close(ctx context.Context) error {
	if s == nil || s.queue == nil {
		return nil
	}
	return s.queue.Close(ctx)
}

// CancelPendingUpdates removes any queued update for the given session,
// preventing stale async jobs from overwriting a subsequent synchronous flush.
// Acquires the session lock to ensure no in-flight task is between capture and Save.
func (s *Service) CancelPendingUpdates(sessionID string) {
	if s == nil || s.queue == nil {
		return
	}
	mu := s.getSessionLock(sessionID)
	mu.Lock()
	defer mu.Unlock()
	s.queue.cancelPending("update:" + sessionID)
}

func (s *Service) getSessionLock(sessionID string) *sync.Mutex {
	mu, _ := s.sessionMu.LoadOrStore(sessionID, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// captureFlushVersion returns the current flush generation for a session.
// The caller should pass the returned value to isFlushStale before Save.
func (s *Service) captureFlushVersion(sessionID string) uint64 {
	if s == nil || s.queue == nil {
		return 0
	}
	return s.queue.captureFlushVersion(sessionID)
}

// isFlushStale returns true if a newer sync flush has occurred since capture.
func (s *Service) isFlushStale(sessionID string, captured uint64) bool {
	if s == nil || s.queue == nil {
		return false
	}
	return s.queue.isFlushStale(sessionID, captured)
}

func (s *Service) AutoMigrate(ctx context.Context) error {
	if s == nil || s.storage == nil {
		return errors.New("memory storage is not configured")
	}
	return s.storage.AutoMigrate(ctx)
}

func (s *Service) Load(ctx context.Context, sessionID string) (Document, error) {
	if s == nil || s.storage == nil {
		return Document{}, errors.New("memory storage is not configured")
	}
	if strings.TrimSpace(sessionID) == "" {
		return Document{}, errors.New("session id is required")
	}
	return s.storage.Load(ctx, sessionID)
}

func (s *Service) LoadScope(ctx context.Context, scope Scope) (Document, error) {
	return s.Load(ctx, scope.Key())
}

// Save persists a document directly (used by memory tool).
// Acquires the session lock to avoid racing with async updates.
func (s *Service) Save(ctx context.Context, doc Document) error {
	if s == nil || s.storage == nil {
		return errors.New("memory storage is not configured")
	}
	if doc.SessionID != "" {
		mu := s.getSessionLock(doc.SessionID)
		mu.Lock()
		defer mu.Unlock()
	}
	return s.storage.Save(ctx, doc)
}

func (s *Service) Update(ctx context.Context, sessionID string, messages []models.Message) error {
	if s == nil {
		return errors.New("memory service is nil")
	}
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("session id is required")
	}
	if s.storage == nil {
		return errors.New("memory storage is not configured")
	}
	if s.extractor == nil {
		return errors.New("memory extractor is not configured")
	}
	if len(messages) == 0 {
		return nil
	}
	filteredMessages := filterMessagesForMemory(messages)
	if len(filteredMessages) == 0 {
		return nil
	}

	// Per-session lock serializes capture-through-Save, eliminating the race
	// window between stale check and Save where a concurrent flush could be
	// overwritten. CancelPendingUpdates also acquires this lock.
	mu := s.getSessionLock(sessionID)
	mu.Lock()
	defer mu.Unlock()

	captured := s.captureFlushVersion(sessionID)

	current, err := s.storage.Load(ctx, sessionID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("load memory %q: %w", sessionID, err)
	}

	update, err := s.extractor.ExtractUpdate(ctx, current, cloneMessages(filteredMessages))
	if err != nil {
		return err
	}
	update = sanitizeUpdateForStorage(update)

	merged := MergeWithFactSource(current, update, sessionID, factSourceFromMessages(filteredMessages), time.Now().UTC())
	if s.isFlushStale(sessionID, captured) {
		return nil
	}
	return s.storage.Save(ctx, merged)
}

func (s *Service) UpdateScope(ctx context.Context, scope Scope, messages []models.Message) error {
	return s.Update(ctx, scope.Key(), messages)
}

// UpdateWith performs a memory update using an externally provided extractor.
// Unlike Update, it does not depend on the Service's own extractor field.
func (s *Service) UpdateWith(ctx context.Context, sessionID string, messages []models.Message, ext Extractor) error {
	if s == nil {
		return errors.New("memory service is nil")
	}
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("session id is required")
	}
	if s.storage == nil {
		return errors.New("memory storage is not configured")
	}
	if ext == nil {
		return errors.New("memory extractor is required")
	}
	filteredMessages := filterMessagesForMemory(messages)
	if len(filteredMessages) == 0 {
		return nil
	}

	mu := s.getSessionLock(sessionID)
	mu.Lock()
	defer mu.Unlock()

	captured := s.captureFlushVersion(sessionID)

	current, err := s.storage.Load(ctx, sessionID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("load memory %q: %w", sessionID, err)
	}

	update, err := ext.ExtractUpdate(ctx, current, cloneMessages(filteredMessages))
	if err != nil {
		return err
	}
	update = sanitizeUpdateForStorage(update)

	merged := MergeWithFactSource(current, update, sessionID, factSourceFromMessages(filteredMessages), time.Now().UTC())
	if s.isFlushStale(sessionID, captured) {
		return nil
	}
	return s.storage.Save(ctx, merged)
}

// UpdateScopeWith is the scope variant of UpdateWith.
func (s *Service) UpdateScopeWith(ctx context.Context, scope Scope, messages []models.Message, ext Extractor) error {
	return s.UpdateWith(ctx, scope.Key(), messages, ext)
}

// UpdateWithFactSource is like UpdateWith but overrides the fact source
// for all new facts in this update. Pass "" for factSource to use default behavior.
func (s *Service) UpdateWithFactSource(ctx context.Context, sessionID string, messages []models.Message, ext Extractor, factSource string) error {
	if s == nil {
		return errors.New("memory service is nil")
	}
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("session id is required")
	}
	if s.storage == nil {
		return errors.New("memory storage is not configured")
	}
	if ext == nil {
		return errors.New("memory extractor is required")
	}
	filteredMessages := filterMessagesForMemory(messages)
	if len(filteredMessages) == 0 {
		return nil
	}

	mu := s.getSessionLock(sessionID)
	mu.Lock()
	defer mu.Unlock()

	captured := s.captureFlushVersion(sessionID)

	current, err := s.storage.Load(ctx, sessionID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("load memory %q: %w", sessionID, err)
	}

	update, err := ext.ExtractUpdate(ctx, current, cloneMessages(filteredMessages))
	if err != nil {
		return err
	}
	update = sanitizeUpdateForStorage(update)

	source := strings.TrimSpace(factSource)
	if source == "" {
		source = factSourceFromMessages(filteredMessages)
	}
	merged := MergeWithFactSource(current, update, sessionID, source, time.Now().UTC())
	if s.isFlushStale(sessionID, captured) {
		return nil
	}
	return s.storage.Save(ctx, merged)
}

// UpdateScopeWithSkillUsage performs a user-scope memory update and records skill
// usage in a single Load+Save cycle, avoiding concurrent-write data loss.
func (s *Service) UpdateScopeWithSkillUsage(ctx context.Context, scope Scope, messages []models.Message, ext Extractor, skillName string) error {
	if s == nil {
		return errors.New("memory service is nil")
	}
	sessionID := scope.Key()
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("scope key is required")
	}
	if s.storage == nil {
		return errors.New("memory storage is not configured")
	}

	filteredMessages := filterMessagesForMemory(messages)

	mu := s.getSessionLock(sessionID)
	mu.Lock()
	defer mu.Unlock()

	captured := s.captureFlushVersion(sessionID)

	// Load doc once (session lock held, no concurrent modification).
	current, err := s.storage.Load(ctx, sessionID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("load memory %q: %w", sessionID, err)
	}

	// Run LLM extraction against loaded doc.
	var update Update
	if ext != nil && len(filteredMessages) > 0 {
		extracted, err := ext.ExtractUpdate(ctx, current, cloneMessages(filteredMessages))
		if err != nil {
			return err
		}
		update = sanitizeUpdateForStorage(extracted)
	}

	// Merge LLM extraction + skill usage, save once.

	source := factSourceFromMessages(filteredMessages)
	if skillName != "" {
		source = "skill:" + skillName
	}
	merged := MergeWithFactSource(current, update, sessionID, source, time.Now().UTC())

	// Append skill usage fact (idempotent by fact ID).
	skillName = strings.TrimSpace(skillName)
	if skillName != "" {
		factID := "skill-usage:" + skillName
		found := false
		for _, f := range merged.Facts {
			if f.ID == factID {
				found = true
				break
			}
		}
		if !found {
			factContent := "用户使用了 /" + skillName + " 技能"
			if scanErr := ScanContent(factContent); scanErr != nil {
				s.logger.Warn("skill usage fact blocked by security", "key", skillName, "err", scanErr)
			} else {
				now := time.Now().UTC()
				merged.Facts = append(merged.Facts, Fact{
					ID:         factID,
					Content:    factContent,
					Category:   "skill_usage",
					Confidence: 1.0,
					Source:     "skill:" + skillName,
					CreatedAt:  now,
					UpdatedAt:  now,
				})
				merged.UpdatedAt = now
			}
		}
	}

	if s.isFlushStale(sessionID, captured) {
		return nil
	}
	return s.storage.Save(ctx, merged)
}

// RecordSkillUsage saves a fact recording that a skill was used.
// Idempotent per (sessionID, skillName) — skips if already recorded.
func (s *Service) RecordSkillUsage(ctx context.Context, sessionID string, skillName string) error {
	if s == nil || s.storage == nil {
		return errors.New("memory service or storage is nil")
	}
	sessionID = strings.TrimSpace(sessionID)
	skillName = strings.TrimSpace(skillName)
	if sessionID == "" || skillName == "" {
		return nil
	}

	mu := s.getSessionLock(sessionID)
	mu.Lock()
	defer mu.Unlock()

	factID := "skill-usage:" + skillName
	factContent := "用户使用了 /" + skillName + " 技能"

	current, err := s.storage.Load(ctx, sessionID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("load memory for skill usage %q: %w", sessionID, err)
	}

	// Dedup: skip if already recorded.
	for _, f := range current.Facts {
		if f.ID == factID {
			return nil
		}
	}

	if err := ScanContent(factContent); err != nil {
		return fmt.Errorf("skill usage fact blocked by security: %w", err)
	}

	now := time.Now().UTC()
	current.SessionID = sessionID
	current.Facts = append(current.Facts, Fact{
		ID:         factID,
		Content:    factContent,
		Category:   "skill_usage",
		Confidence: 1.0,
		Source:     "skill:" + skillName,
		CreatedAt:  now,
		UpdatedAt:  now,
	})
	current.UpdatedAt = now
	return s.storage.Save(ctx, current)
}

func (s *Service) Inject(ctx context.Context, sessionID string) string {
	return s.InjectWithContext(ctx, sessionID, "", "")
}

func (s *Service) InjectScope(ctx context.Context, scope Scope) string {
	return s.InjectScopeWithContext(ctx, scope, "", "")
}

func (s *Service) InjectWithContext(ctx context.Context, sessionID string, currentContext string, activeSource string) string {
	if s == nil || s.storage == nil || strings.TrimSpace(sessionID) == "" {
		return ""
	}

	doc, err := s.storage.Load(ctx, sessionID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			s.logger.Warn("load memory for injection failed", "session", sessionID, "err", err)
		}
		return ""
	}
	injection, retrievedIDs := buildInjectionWithIDs(doc, currentContext, 2000, activeSource)
	slog.Debug("memory injected",
		"session", sessionID,
		"retrieved_facts", len(retrievedIDs),
		"injection_len", len(injection),
	)
	if len(retrievedIDs) > 0 {
		s.scheduleRetrievalIncrement(sessionID, retrievedIDs)
		// Store for feedback loop consumption.
		// Cap entries to prevent unbounded growth in long-lived processes
		// (e.g. gateway) where LastRetrieved may not be called.
		s.lastRetrieved.mu.Lock()
		_, exists := s.lastRetrieved.data[sessionID]
		if exists || len(s.lastRetrieved.data) < 10000 {
			s.lastRetrieved.data[sessionID] = &lastRetrieval{
				ids: retrievedIDs,
				ts:  time.Now().UnixNano(),
			}
		}
		s.lastRetrieved.mu.Unlock()
	}
	return injection
}

func (s *Service) InjectScopeWithContext(ctx context.Context, scope Scope, currentContext string, activeSource string) string {
	return s.InjectWithContext(ctx, scope.Key(), currentContext, activeSource)
}

// LastRetrieved returns and clears the fact IDs retrieved in the previous
// InjectWithContext call for the given session. Returns nil if nothing was retrieved.
// This is a consume-once operation: the second call returns nil.
func (s *Service) LastRetrieved(sessionID string) []string {
	if s == nil {
		return nil
	}
	s.lastRetrieved.mu.Lock()
	defer s.lastRetrieved.mu.Unlock()
	r, ok := s.lastRetrieved.data[sessionID]
	if !ok {
		return nil
	}
	ids := r.ids
	delete(s.lastRetrieved.data, sessionID)
	return ids
}

// CleanupStale removes lastRetrieved entries older than the given duration.
func (s *Service) CleanupStale(maxAge time.Duration) {
	if s == nil {
		return
	}
	cutoff := time.Now().Add(-maxAge).UnixNano()
	s.lastRetrieved.mu.Lock()
	defer s.lastRetrieved.mu.Unlock()
	for k, v := range s.lastRetrieved.data {
		if v.ts < cutoff {
			delete(s.lastRetrieved.data, k)
		}
	}
}

func Merge(current Document, update Update, sessionID string, now time.Time) Document {
	return MergeWithFactSource(current, update, sessionID, "", now)
}

func MergeWithFactSource(current Document, update Update, sessionID string, factSource string, now time.Time) Document {
	if now.IsZero() {
		now = time.Now().UTC()
	}

	merged := current
	merged.SessionID = strings.TrimSpace(sessionID)
	if merged.SessionID == "" {
		merged.SessionID = current.SessionID
	}

	if v := strings.TrimSpace(update.User.WorkContext); v != "" {
		merged.User.WorkContext = v
	}
	if v := strings.TrimSpace(update.User.PersonalContext); v != "" {
		merged.User.PersonalContext = v
	}
	if v := strings.TrimSpace(update.User.TopOfMind); v != "" {
		merged.User.TopOfMind = v
	}
	if v := strings.TrimSpace(update.History.RecentMonths); v != "" {
		merged.History.RecentMonths = v
	}
	if v := strings.TrimSpace(update.History.EarlierContext); v != "" {
		merged.History.EarlierContext = v
	}
	if v := strings.TrimSpace(update.History.LongTermBackground); v != "" {
		merged.History.LongTermBackground = v
	}
	if v := strings.TrimSpace(update.Source); v != "" {
		merged.Source = v
	}
	if merged.Source == "" {
		merged.Source = merged.SessionID
	}

	defaultFactSource := strings.TrimSpace(factSource)
	if defaultFactSource == "" {
		defaultFactSource = merged.Source
	}
	merged.Facts = mergeFacts(current.Facts, update.Facts, defaultFactSource, now)
	merged.UpdatedAt = now
	return merged
}

func factSourceFromMessages(messages []models.Message) string {
	for _, msg := range messages {
		source := strings.TrimSpace(msg.SessionID)
		if source != "" {
			return source
		}
	}
	return ""
}

func mergeFacts(existing, incoming []Fact, defaultSource string, now time.Time) []Fact {
	index := make(map[string]int, len(existing))
	merged := make([]Fact, 0, len(existing)+len(incoming))
	defaultSource = strings.TrimSpace(defaultSource)

	for _, fact := range existing {
		if strings.TrimSpace(fact.ID) == "" || strings.TrimSpace(fact.Content) == "" {
			continue
		}
		fact.Source = strings.TrimSpace(fact.Source)
		if fact.Source == "" {
			fact.Source = defaultSource
		}
		if fact.CreatedAt.IsZero() {
			fact.CreatedAt = now
		}
		if fact.UpdatedAt.IsZero() {
			fact.UpdatedAt = fact.CreatedAt
		}
		index[fact.ID] = len(merged)
		merged = append(merged, fact)
	}

	for _, fact := range incoming {
		fact.ID = strings.TrimSpace(fact.ID)
		fact.Content = strings.TrimSpace(fact.Content)
		fact.Category = strings.TrimSpace(fact.Category)
		fact.Source = strings.TrimSpace(fact.Source)
		if fact.Source == "" {
			fact.Source = defaultSource
		}
		if fact.ID == "" || fact.Content == "" {
			continue
		}
		// Security: skip facts with dangerous content.
		if err := ScanContent(fact.Content); err != nil {
			continue
		}
		if fact.Confidence < 0 {
			fact.Confidence = 0
		}
		if fact.Confidence > 1 {
			fact.Confidence = 1
		}

		if idx, ok := index[fact.ID]; ok {
			current := merged[idx]
			current.Content = fact.Content
			if fact.Category != "" {
				current.Category = fact.Category
			}
			if fact.Confidence > 0 {
				current.Confidence = fact.Confidence
			}
			current.UpdatedAt = now
			merged[idx] = current
			continue
		}

		fact.CreatedAt = now
		fact.UpdatedAt = now
		index[fact.ID] = len(merged)
		merged = append(merged, fact)
	}

	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].UpdatedAt.Equal(merged[j].UpdatedAt) {
			return merged[i].ID < merged[j].ID
		}
		return merged[i].UpdatedAt.After(merged[j].UpdatedAt)
	})

	return evictLowScoreFacts(merged, now)
}

// factScore computes an aging score for eviction ranking.
// Higher score = more valuable, less likely to be evicted.
// Formula: confidence * (1 + helpfulRatio) * recencyFactor
func factScore(f Fact, now time.Time) float64 {
	confidence := f.Confidence
	if confidence <= 0 {
		confidence = 0.1
	}
	helpfulRatio := 0.0
	if f.RetrievalCount > 0 {
		helpfulRatio = float64(f.HelpfulCount) / float64(f.RetrievalCount)
	}
	// Recency decay: half-life of 30 days.
	ageDays := now.Sub(f.UpdatedAt).Hours() / 24
	recency := 1.0 / (1.0 + ageDays/30.0)
	return confidence * (1.0 + helpfulRatio) * recency
}

// evictLowScoreFacts trims facts to MaxFactsPerSession by removing the lowest-scored.
func evictLowScoreFacts(facts []Fact, now time.Time) []Fact {
	if len(facts) <= MaxFactsPerSession {
		return facts
	}
	keep := MaxFactsPerSession
	if keep <= 0 {
		keep = 30
	}
	// Preserve newest facts (already sorted by UpdatedAt desc), evict from the tail.
	candidateStart := keep / 2 // Always keep the top half by recency.
	candidates := facts[candidateStart:]
	sort.SliceStable(candidates, func(i, j int) bool {
		return factScore(candidates[i], now) > factScore(candidates[j], now)
	})
	remaining := keep - candidateStart
	if remaining > len(candidates) {
		remaining = len(candidates)
	}
	result := make([]Fact, 0, keep)
	result = append(result, facts[:candidateStart]...)
	result = append(result, candidates[:remaining]...)
	return result
}

func cloneMessages(messages []models.Message) []models.Message {
	if len(messages) == 0 {
		return nil
	}
	cloned := make([]models.Message, 0, len(messages))
	for _, msg := range messages {
		copyMsg := msg
		if len(msg.ToolCalls) > 0 {
			copyMsg.ToolCalls = append([]models.ToolCall(nil), msg.ToolCalls...)
		}
		if msg.Metadata != nil {
			copyMsg.Metadata = make(map[string]string, len(msg.Metadata))
			for k, v := range msg.Metadata {
				copyMsg.Metadata[k] = v
			}
		}
		if msg.ToolResult != nil {
			result := *msg.ToolResult
			if result.Data != nil {
				result.Data = cloneAnyMap(result.Data)
			}
			copyMsg.ToolResult = &result
		}
		cloned = append(cloned, copyMsg)
	}
	return cloned
}

func cloneAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
