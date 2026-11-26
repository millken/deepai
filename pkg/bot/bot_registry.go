package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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
	// MaxBotExtensionsBytes caps serialized BotProfile.Extensions (extra_json) at 64 KiB.
	MaxBotExtensionsBytes = 64 << 10
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

var (
	botIDPattern         = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	connectorNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
)

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

// APIConnector declares one third-party HTTP service endpoint bound to a bot,
// allowing `http_request` to inherit BaseURL, default headers/query params
// (including `{{secret:NAME}}` placeholders), and host allowlists.
type APIConnector struct {
	Name         string            `json:"name"`
	BaseURL      string            `json:"base_url"`
	AllowedHosts []string          `json:"allowed_hosts,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	QueryParams  map[string]string `json:"query_params,omitempty"`
	TimeoutSec   int               `json:"timeout_sec,omitempty"`
}

func (c APIConnector) Clone() APIConnector {
	out := c
	if len(c.AllowedHosts) > 0 {
		out.AllowedHosts = append([]string(nil), c.AllowedHosts...)
	}
	if len(c.Headers) > 0 {
		out.Headers = make(map[string]string, len(c.Headers))
		for k, v := range c.Headers {
			out.Headers[k] = v
		}
	}
	if len(c.QueryParams) > 0 {
		out.QueryParams = make(map[string]string, len(c.QueryParams))
		for k, v := range c.QueryParams {
			out.QueryParams[k] = v
		}
	}
	return out
}

// BotExtensions maps to `bots.extra_json`, storing per-bot training flags and
// bound third-party service API connectors without altering core table columns.
type BotExtensions struct {
	// IsolatedPrompt omits workspace DEEPAI.md and workspace code skills so a
	// domain-focused custom bot runs with a clean persona and its own skills.
	IsolatedPrompt bool `json:"isolated_prompt,omitempty"`
	// DisableSelfTrain freezes the bot's persona and per-bot skills against
	// conversational mutations via update_bot_persona / save_bot_skill.
	DisableSelfTrain bool `json:"disable_self_train,omitempty"`
	// Connectors lists named third-party HTTP API connectors bound to this bot.
	Connectors []APIConnector `json:"connectors,omitempty"`
}

func (e BotExtensions) IsZero() bool {
	return !e.IsolatedPrompt && !e.DisableSelfTrain && len(e.Connectors) == 0
}

func (e BotExtensions) Clone() BotExtensions {
	out := BotExtensions{
		IsolatedPrompt:   e.IsolatedPrompt,
		DisableSelfTrain: e.DisableSelfTrain,
	}
	if len(e.Connectors) > 0 {
		out.Connectors = make([]APIConnector, len(e.Connectors))
		for i, c := range e.Connectors {
			out.Connectors[i] = c.Clone()
		}
	}
	return out
}

// BotProfile is one registered persona inside deepai-bot (docs/BOT_MULTI_DESIGN.md §3).
type BotProfile struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	Title         string        `json:"title,omitempty"`
	Description   string        `json:"description,omitempty"`
	Instructions  string        `json:"instructions,omitempty"`
	DefaultModel  string        `json:"default_model,omitempty"`
	ToolAllowlist []string      `json:"tool_allowlist,omitempty"`
	Enabled       bool          `json:"enabled"`
	Extensions    BotExtensions `json:"extensions,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
}

// Clone copies b including its ToolAllowlist and Extensions slices/maps.
func (b BotProfile) Clone() BotProfile {
	out := b
	out.ToolAllowlist = append([]string(nil), b.ToolAllowlist...)
	out.Extensions = b.Extensions.Clone()
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
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Title         string         `json:"title,omitempty"`
	Description   string         `json:"description,omitempty"`
	Instructions  string         `json:"instructions,omitempty"`
	DefaultModel  string         `json:"default_model,omitempty"`
	ToolAllowlist []string       `json:"tool_allowlist,omitempty"`
	Enabled       *bool          `json:"enabled,omitempty"`
	Extensions    *BotExtensions `json:"extensions,omitempty"`
}

// UpdateBotRequest is the body of PATCH /v1/bots/{bot_id}.
type UpdateBotRequest struct {
	Name          *string        `json:"name,omitempty"`
	Title         *string        `json:"title,omitempty"`
	Description   *string        `json:"description,omitempty"`
	Instructions  *string        `json:"instructions,omitempty"`
	DefaultModel  *string        `json:"default_model,omitempty"`
	ToolAllowlist *[]string      `json:"tool_allowlist,omitempty"`
	Enabled       *bool          `json:"enabled,omitempty"`
	Extensions    *BotExtensions `json:"extensions,omitempty"`
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

func normalizeBotExtensions(in BotExtensions) BotExtensions {
	out := in.Clone()
	for i := range out.Connectors {
		c := &out.Connectors[i]
		c.Name = strings.TrimSpace(c.Name)
		c.BaseURL = strings.TrimSpace(c.BaseURL)
		if len(c.AllowedHosts) > 0 {
			hosts := make([]string, 0, len(c.AllowedHosts))
			seen := map[string]struct{}{}
			for _, h := range c.AllowedHosts {
				h = strings.ToLower(strings.TrimSpace(h))
				if h == "" {
					continue
				}
				if _, dup := seen[h]; dup {
					continue
				}
				seen[h] = struct{}{}
				hosts = append(hosts, h)
			}
			c.AllowedHosts = hosts
		}
	}
	return out
}

func validateBotExtensions(ext BotExtensions) error {
	raw, err := json.Marshal(ext)
	if err != nil {
		return fmt.Errorf("%w: invalid extensions JSON: %v", ErrInvalidBot, err)
	}
	if len(raw) > MaxBotExtensionsBytes {
		return fmt.Errorf("%w: extensions must be at most %d bytes", ErrInvalidBot, MaxBotExtensionsBytes)
	}
	seen := make(map[string]struct{}, len(ext.Connectors))
	for _, c := range ext.Connectors {
		name := strings.TrimSpace(c.Name)
		if !connectorNamePattern.MatchString(name) {
			return fmt.Errorf("%w: connector name %q must match ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$", ErrInvalidBot, c.Name)
		}
		key := strings.ToLower(name)
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%w: duplicate connector %q", ErrInvalidBot, name)
		}
		seen[key] = struct{}{}
		if strings.TrimSpace(c.BaseURL) == "" {
			return fmt.Errorf("%w: connector %q base_url is required", ErrInvalidBot, name)
		}
		if secretPlaceholderRE.MatchString(c.BaseURL) {
			return fmt.Errorf("%w: connector %q base_url must not contain '{{secret:...}}'; put credentials in headers or query_params", ErrInvalidBot, name)
		}
		u, err := url.Parse(strings.TrimSpace(c.BaseURL))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			return fmt.Errorf("%w: connector %q base_url must be a valid http(s) URL", ErrInvalidBot, name)
		}
		if u.User != nil {
			return fmt.Errorf("%w: connector %q base_url must not embed userinfo credentials; use '{{secret:NAME}}'", ErrInvalidBot, name)
		}
		if c.TimeoutSec < 0 || c.TimeoutSec > 300 {
			return fmt.Errorf("%w: connector %q timeout_sec must be between 0 and 300", ErrInvalidBot, name)
		}
		for hk, hv := range c.Headers {
			if err := validateConnectorParamSecretHygiene(name, "header", hk, hv); err != nil {
				return err
			}
		}
		for qk, qv := range c.QueryParams {
			if err := validateConnectorParamSecretHygiene(name, "query_param", qk, qv); err != nil {
				return err
			}
		}
	}
	return nil
}

var (
	authSchemeOnlyRE       = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,23}$`)
	knownSecretTokenPrefix = regexp.MustCompile(`(?i)(^|[\s=:,"'])(sk-[A-Za-z0-9_-]{6,}|rk-[A-Za-z0-9_-]{6,}|pk_(live|test)_[A-Za-z0-9]{6,}|gh[pousr]_[A-Za-z0-9]{6,}|github_pat_[A-Za-z0-9_]{6,}|glpat-[A-Za-z0-9_-]{6,}|xox[baprs]-[A-Za-z0-9-]{6,}|ntn_[A-Za-z0-9]{6,}|AKIA[0-9A-Z]{12,}|AIza[0-9A-Za-z_-]{16,})`)
)

func isSensitiveCredentialKey(k string) bool {
	lower := strings.ToLower(strings.TrimSpace(k))
	switch lower {
	case "authorization", "proxy-authorization", "cookie", "set-cookie":
		return true
	}
	for _, sub := range []string{
		"api-key", "apikey", "api_key",
		"secret", "token", "password", "passwd",
		"access-key", "access_key", "private-key", "private_key",
		"client-secret", "client_secret", "service-key", "service_key", "app_key", "app-key",
	} {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return false
}

func validateConnectorParamSecretHygiene(connName, kind, key, val string) error {
	trimmed := strings.TrimSpace(val)
	if trimmed == "" {
		return nil
	}
	if strings.Contains(trimmed, "enc:v1:") {
		return fmt.Errorf("%w: connector %q %s %q must use '{{secret:NAME}}' instead of sealed ciphertext", ErrInvalidBot, connName, kind, key)
	}
	hasPlaceholder := secretPlaceholderRE.MatchString(trimmed)
	rem := strings.TrimSpace(secretPlaceholderRE.ReplaceAllString(trimmed, ""))
	if knownSecretTokenPrefix.MatchString(rem) {
		return fmt.Errorf("%w: connector %q %s %q appears to contain a plaintext credential; store it in bot secrets and reference '{{secret:NAME}}'", ErrInvalidBot, connName, kind, key)
	}
	if isSensitiveCredentialKey(key) {
		if !hasPlaceholder {
			return fmt.Errorf("%w: connector %q sensitive %s %q must reference '{{secret:NAME}}' instead of a plaintext value", ErrInvalidBot, connName, kind, key)
		}
		if rem != "" && !authSchemeOnlyRE.MatchString(rem) {
			return fmt.Errorf("%w: connector %q sensitive %s %q contains literal text alongside '{{secret:NAME}}'; keep only an auth scheme prefix (e.g. 'Bearer {{secret:NAME}}')", ErrInvalidBot, connName, kind, key)
		}
	}
	return nil
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
	if err := validateBotExtensions(b.Extensions); err != nil {
		return err
	}
	return nil
}
