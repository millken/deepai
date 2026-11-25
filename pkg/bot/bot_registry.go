package bot

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
)

const (
	// DefaultBotID is the built-in system bot ID. It cannot be deleted and
	// backs the flat /v1/sessions aliases (docs/BOT_MULTI_DESIGN.md).
	DefaultBotID = "default"
	// MaxBotInstructionsBytes caps BotProfile.Instructions at 64 KiB.
	MaxBotInstructionsBytes = 64 << 10
)

var (
	ErrUnknownBot         = errors.New("unknown bot")
	ErrBotDisabled        = errors.New("bot is disabled")
	ErrBotHasSessions     = errors.New("bot still has sessions or schedules")
	ErrBotDeleteForbidden = errors.New("default bot cannot be deleted")
	ErrBotExists          = errors.New("bot already exists")
	ErrInvalidBot         = errors.New("invalid bot profile")
	ErrUnknownTool        = errors.New("unknown tool")
)

var botIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// BotHasSessionsError carries the session/schedule counts preventing a
// non-forced DELETE /v1/bots/{id}.
type BotHasSessionsError struct {
	Sessions  int
	Schedules int
}

func (e *BotHasSessionsError) Error() string {
	return fmt.Sprintf("bot still has %d session(s) and %d schedule(s); pass force=1 to cascade delete", e.Sessions, e.Schedules)
}

func (e *BotHasSessionsError) Unwrap() error { return ErrBotHasSessions }

// BotProfile is one registered persona inside deepai-bot (docs/BOT_MULTI_DESIGN.md §3).
type BotProfile struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Title         string    `json:"title,omitempty"`
	Description   string    `json:"description,omitempty"`
	Instructions  string    `json:"instructions,omitempty"`
	DefaultModel  string    `json:"default_model,omitempty"`
	ToolAllowlist []string  `json:"tool_allowlist,omitempty"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Clone copies b including its ToolAllowlist slice.
func (b BotProfile) Clone() BotProfile {
	out := b
	out.ToolAllowlist = append([]string(nil), b.ToolAllowlist...)
	return out
}

// BotSummary enriches BotProfile with live per-workdir counts for GET /v1/bots.
type BotSummary struct {
	BotProfile
	SessionCount  int `json:"session_count"`
	ActiveTurns   int `json:"active_turns"`
	ScheduleCount int `json:"schedule_count"`
}

// CreateBotRequest is the body of POST /v1/bots.
type CreateBotRequest struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Title         string   `json:"title,omitempty"`
	Description   string   `json:"description,omitempty"`
	Instructions  string   `json:"instructions,omitempty"`
	DefaultModel  string   `json:"default_model,omitempty"`
	ToolAllowlist []string `json:"tool_allowlist,omitempty"`
	Enabled       *bool    `json:"enabled,omitempty"`
}

// UpdateBotRequest is the body of PATCH /v1/bots/{bot_id}.
type UpdateBotRequest struct {
	Name          *string   `json:"name,omitempty"`
	Title         *string   `json:"title,omitempty"`
	Description   *string   `json:"description,omitempty"`
	Instructions  *string   `json:"instructions,omitempty"`
	DefaultModel  *string   `json:"default_model,omitempty"`
	ToolAllowlist *[]string `json:"tool_allowlist,omitempty"`
	Enabled       *bool     `json:"enabled,omitempty"`
}

// BotRegistry persists bot personas (bots table in bot.db or MemoryStore).
type BotRegistry interface {
	EnsureDefault(ctx context.Context) error
	ListBots(ctx context.Context) ([]BotProfile, error)
	GetBot(ctx context.Context, id string) (BotProfile, error)
	CreateBot(ctx context.Context, b BotProfile) (BotProfile, error)
	UpdateBot(ctx context.Context, b BotProfile) (BotProfile, error)
	DeleteBot(ctx context.Context, id string) error
}

// BotStore is the optional multi-bot session extension implemented by
// botSessionStore (bot.db) and MemoryStore. Stores that only implement Store
// fall back cleanly to DefaultBotID.
type BotStore interface {
	CreateForBot(botID string, opts models.CreateOpts) (*models.Session, error)
	ListRecentForBot(botID, cwd string, limit int) ([]models.SessionMeta, error)
	SessionOwner(id string) (botID, cwd string, err error)
	CountBotSessions(botID string) (int, error)
	HasExternalBotLock(botID string, self models.LockOwner) (bool, error)
	DeleteBotCascade(botID string) error
}

// ToolValidator is optionally implemented by Runtime (AgentRuntime implements
// it) to validate BotProfile.ToolAllowlist patterns against registered tools.
type ToolValidator interface {
	ValidateToolAllowlist(allowlist []string) []string
}

func effectiveBotID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return DefaultBotID
	}
	return id
}

func normalizeToolAllowlist(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func validateBotProfile(b BotProfile, isCreate bool) error {
	if isCreate {
		if b.ID == DefaultBotID {
			return fmt.Errorf("%w: id %q is reserved", ErrInvalidBot, DefaultBotID)
		}
		if !botIDPattern.MatchString(b.ID) {
			return fmt.Errorf("%w: id %q must match ^[a-z0-9][a-z0-9_-]{0,31}$", ErrInvalidBot, b.ID)
		}
	}
	name := strings.TrimSpace(b.Name)
	if name == "" || len([]rune(name)) > 64 {
		return fmt.Errorf("%w: name is required and must be at most 64 characters", ErrInvalidBot)
	}
	if len([]rune(b.Title)) > 120 {
		return fmt.Errorf("%w: title must be at most 120 characters", ErrInvalidBot)
	}
	if len([]rune(b.Description)) > 512 {
		return fmt.Errorf("%w: description must be at most 512 characters", ErrInvalidBot)
	}
	if len(b.Instructions) > MaxBotInstructionsBytes {
		return fmt.Errorf("%w: instructions must be at most %d bytes", ErrInvalidBot, MaxBotInstructionsBytes)
	}
	if b.ID == DefaultBotID && !b.Enabled {
		return fmt.Errorf("%w: default bot cannot be disabled", ErrInvalidBot)
	}
	return nil
}
