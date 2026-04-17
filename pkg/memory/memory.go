package memory

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
)

const defaultUpdateTimeout = 30 * time.Second

// Capacity limits for memory content.
const (
	MaxFactsPerSession = 30   // Maximum facts per session
	MaxFactContentLen  = 500  // Maximum characters per fact content
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
	ID            string    `json:"id"`
	Content       string    `json:"content"`
	Category      string    `json:"category,omitempty"`
	Confidence    float64   `json:"confidence,omitempty"`
	Source        string    `json:"source,omitempty"`
	RetrievalCount int      `json:"retrieval_count,omitempty"`
	HelpfulCount  int      `json:"helpful_count,omitempty"`
	CreatedAt     time.Time `json:"created_at,omitempty"`
	UpdatedAt     time.Time `json:"updated_at,omitempty"`
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
}

type Extractor interface {
	ExtractUpdate(ctx context.Context, current Document, messages []models.Message) (Update, error)
}

type Service struct {
	storage       Storage
	extractor     Extractor
	logger        *log.Logger
	updateTimeout time.Duration
}

func NewService(storage Storage, extractor Extractor) *Service {
	return &Service{
		storage:       storage,
		extractor:     extractor,
		logger:        log.New(os.Stderr, "memory: ", log.LstdFlags),
		updateTimeout: defaultUpdateTimeout,
	}
}

func (s *Service) WithLogger(logger *log.Logger) *Service {
	if s != nil && logger != nil {
		s.logger = logger
	}
	return s
}

func (s *Service) WithUpdateTimeout(timeout time.Duration) *Service {
	if s != nil && timeout > 0 {
		s.updateTimeout = timeout
	}
	return s
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
func (s *Service) Save(ctx context.Context, doc Document) error {
	if s == nil || s.storage == nil {
		return errors.New("memory storage is not configured")
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
	return s.storage.Save(ctx, merged)
}

// UpdateScopeWith is the scope variant of UpdateWith.
func (s *Service) UpdateScopeWith(ctx context.Context, scope Scope, messages []models.Message, ext Extractor) error {
	return s.UpdateWith(ctx, scope.Key(), messages, ext)
}

// ScheduleUpdateWith runs UpdateWith in the background and never propagates errors.
func (s *Service) ScheduleUpdateWith(sessionID string, messages []models.Message, ext Extractor) {
	if s == nil || s.storage == nil || ext == nil || len(messages) == 0 {
		return
	}

	timeout := s.updateTimeout
	if timeout <= 0 {
		timeout = defaultUpdateTimeout
	}
	cloned := cloneMessages(messages)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		if err := s.UpdateWith(ctx, sessionID, cloned, ext); err != nil {
			s.logf("async update with failed for session %s: %v", sessionID, err)
		}
	}()
}

// ScheduleUpdate runs memory extraction in the background and never propagates errors.
func (s *Service) ScheduleUpdate(sessionID string, messages []models.Message) {
	if s == nil || s.storage == nil || s.extractor == nil || len(messages) == 0 {
		return
	}

	timeout := s.updateTimeout
	if timeout <= 0 {
		timeout = defaultUpdateTimeout
	}
	cloned := cloneMessages(messages)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		if err := s.Update(ctx, sessionID, cloned); err != nil {
			s.logf("async update failed for session %s: %v", sessionID, err)
		}
	}()
}

func (s *Service) Inject(ctx context.Context, sessionID string) string {
	return s.InjectWithContext(ctx, sessionID, "")
}

func (s *Service) InjectScope(ctx context.Context, scope Scope) string {
	return s.InjectScopeWithContext(ctx, scope, "")
}

func (s *Service) InjectWithContext(ctx context.Context, sessionID string, currentContext string) string {
	if s == nil || s.storage == nil || strings.TrimSpace(sessionID) == "" {
		return ""
	}

	doc, err := s.storage.Load(ctx, sessionID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			s.logf("load memory for injection failed for session %s: %v", sessionID, err)
		}
		return ""
	}
	injection, retrievedIDs := buildInjectionWithIDs(doc, currentContext, 2000)
	if len(retrievedIDs) > 0 {
		s.scheduleRetrievalIncrement(sessionID, retrievedIDs)
	}
	return injection
}

func (s *Service) InjectScopeWithContext(ctx context.Context, scope Scope, currentContext string) string {
	return s.InjectWithContext(ctx, scope.Key(), currentContext)
}

// scheduleRetrievalIncrement asynchronously increments RetrievalCount for facts
// that were selected during injection. Uses atomic storage-layer update to avoid
// load-modify-save races.
func (s *Service) scheduleRetrievalIncrement(sessionID string, factIDs []string) {
	if s == nil || s.storage == nil || len(factIDs) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.storage.IncrementRetrievalCounts(ctx, sessionID, factIDs); err != nil {
			s.logf("increment retrieval counts failed for session %s: %v", sessionID, err)
		}
	}()
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

func (s *Service) logf(format string, args ...any) {
	if s != nil && s.logger != nil {
		s.logger.Printf(format, args...)
	}
}
