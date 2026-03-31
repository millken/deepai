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
	RecentMonths   string `json:"recentMonths,omitempty"`
	EarlierContext string `json:"earlierContext,omitempty"`
}

type Fact struct {
	ID         string    `json:"id"`
	Content    string    `json:"content"`
	Category   string    `json:"category,omitempty"`
	Confidence float64   `json:"confidence,omitempty"`
	CreatedAt  time.Time `json:"created_at,omitempty"`
	UpdatedAt  time.Time `json:"updated_at,omitempty"`
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

	merged := Merge(current, update, sessionID, time.Now().UTC())
	return s.storage.Save(ctx, merged)
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
	return BuildInjection(doc)
}

func Merge(current Document, update Update, sessionID string, now time.Time) Document {
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
	if v := strings.TrimSpace(update.Source); v != "" {
		merged.Source = v
	}
	if merged.Source == "" {
		merged.Source = merged.SessionID
	}

	merged.Facts = mergeFacts(current.Facts, update.Facts, now)
	merged.UpdatedAt = now
	return merged
}

func mergeFacts(existing, incoming []Fact, now time.Time) []Fact {
	index := make(map[string]int, len(existing))
	merged := make([]Fact, 0, len(existing)+len(incoming))

	for _, fact := range existing {
		if strings.TrimSpace(fact.ID) == "" || strings.TrimSpace(fact.Content) == "" {
			continue
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
		if fact.ID == "" || fact.Content == "" {
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

	return merged
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
