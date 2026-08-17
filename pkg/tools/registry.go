package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/millken/deepai/pkg/models"
	pkgsandbox "github.com/millken/deepai/pkg/sandbox"
)

type Sandbox = pkgsandbox.Sandbox

type contextKey string

const (
	sandboxContextKey              contextKey = "tool_sandbox"
	threadIDContextKey             contextKey = "tool_thread_id"
	userInteractionContextKey      contextKey = "tool_user_interaction"
	remainingTokenBudgetContextKey contextKey = "tool_remaining_token_budget"
	contextWindowContextKey        contextKey = "tool_context_window"
)

// UserInteraction handles prompting the human user for input.
// In interactive mode (CLI), this reads from stdin.
// In non-interactive mode (API/server), this is nil — tools should
// return guidance for the AI to decide on its own.
type UserInteraction interface {
	AskQuestion(ctx context.Context, question string, options []string) (string, error)
}

func WithUserInteraction(ctx context.Context, ui UserInteraction) context.Context {
	return context.WithValue(ctx, userInteractionContextKey, ui)
}

func UserInteractionFromContext(ctx context.Context) UserInteraction {
	ui, _ := ctx.Value(userInteractionContextKey).(UserInteraction)
	return ui
}

// WithRemainingTokenBudget attaches the parent agent's remaining token
// budget (MaxTokensBudget - tokens consumed so far) to ctx so a tool that
// spawns further work (e.g. the task tool) can cap its own budget to what
// the parent actually has left, instead of allowing unlimited fan-out
// beneath a budget-constrained parent (plan §M2.2 carry-forward). Callers
// inject this once per tool-dispatch batch; a remaining value of 0 (budget
// exhausted) is a valid, meaningful value, distinguished from "no parent
// budget configured at all" via RemainingTokenBudgetFromContext's second
// return.
// WithContextWindow attaches the model's context-window size (in tokens) to
// ctx so a tool can bound its output relative to what the model can hold
// (code_map's include_content budget is derived from it). Injected by the
// agent's tool dispatch; standalone handler calls see ok=false and fall
// back to a static default.
func WithContextWindow(ctx context.Context, windowTokens int) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, contextWindowContextKey, windowTokens)
}

// ContextWindowFromContext returns the injected context window and whether
// one was present. ok=false means the dispatching agent had no window
// configured (or the ctx never flowed through an agent dispatch).
func ContextWindowFromContext(ctx context.Context) (int, bool) {
	if ctx == nil {
		return 0, false
	}
	w, ok := ctx.Value(contextWindowContextKey).(int)
	return w, ok && w > 0
}

func WithRemainingTokenBudget(ctx context.Context, remaining int) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, remainingTokenBudgetContextKey, remaining)
}

// RemainingTokenBudgetFromContext returns the parent's remaining token
// budget injected via WithRemainingTokenBudget and whether one was present
// at all. ok=false means no parent budget is in play (parent has no budget,
// or this ctx never flowed through a budget-aware dispatch); ok=true with
// remaining=0 means the parent budget is currently exhausted.
func RemainingTokenBudgetFromContext(ctx context.Context) (int, bool) {
	if ctx == nil {
		return 0, false
	}
	remaining, ok := ctx.Value(remainingTokenBudgetContextKey).(int)
	return remaining, ok
}

var toolCallSeq uint64

type Registry struct {
	mu    sync.RWMutex
	tools map[string]models.Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]models.Tool)}
}

// Clone returns a new Registry with the same tool entries. The returned
// registry is independent: registering or unregistering on one does not
// affect the other. Tool values are copied; Handler func values are shared,
// which is expected since handlers are stateless closures over shared deps.
func (r *Registry) Clone() *Registry {
	if r == nil {
		return NewRegistry()
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	cloned := make(map[string]models.Tool, len(r.tools))
	for name, tool := range r.tools {
		cloned[name] = tool
	}
	return &Registry{tools: cloned}
}

func (r *Registry) Register(tool models.Tool) error {
	if err := tool.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[tool.Name]; exists {
		return fmt.Errorf("tool %q already registered", tool.Name)
	}
	r.tools[tool.Name] = tool
	return nil
}

func (r *Registry) Unregister(name string) bool {
	if r == nil {
		return false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[name]; !exists {
		return false
	}
	delete(r.tools, name)
	return true
}

func (r *Registry) Get(name string) *models.Tool {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, ok := r.tools[strings.TrimSpace(name)]
	if !ok {
		return nil
	}
	copy := tool
	return &copy
}

func (r *Registry) List() []models.Tool {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]models.Tool, 0, len(names))
	for _, name := range names {
		out = append(out, r.tools[name])
	}
	return out
}

func (r *Registry) Descriptions() string {
	tools := r.List()
	if len(tools) == 0 {
		return ""
	}
	var lines []string
	for _, tool := range tools {
		line := fmt.Sprintf("- %s: %s", tool.Name, strings.TrimSpace(tool.Description))
		if len(tool.InputSchema) > 0 {
			if raw, err := json.MarshalIndent(tool.InputSchema, "", "  "); err == nil {
				line += "\n  schema: " + strings.ReplaceAll(string(raw), "\n", "\n  ")
			}
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func WithSandbox(ctx context.Context, sandbox *Sandbox) context.Context {
	if sandbox == nil {
		return ctx
	}
	return context.WithValue(ctx, sandboxContextKey, sandbox)
}

func SandboxFromContext(ctx context.Context) *Sandbox {
	if ctx == nil {
		return nil
	}
	sandbox, _ := ctx.Value(sandboxContextKey).(*Sandbox)
	return sandbox
}

func WithThreadID(ctx context.Context, threadID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return ctx
	}
	return context.WithValue(ctx, threadIDContextKey, threadID)
}

func ThreadIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	threadID, _ := ctx.Value(threadIDContextKey).(string)
	return strings.TrimSpace(threadID)
}

func (r *Registry) Call(ctx context.Context, name string, args map[string]interface{}, sandbox *Sandbox) (string, error) {
	if r == nil {
		return "", fmt.Errorf("tool registry is nil")
	}
	tool := r.Get(name)
	if tool == nil {
		return "", fmt.Errorf("tool %q not found", strings.TrimSpace(name))
	}
	if err := validateArgs(tool.InputSchema, args); err != nil {
		return "", err
	}

	call := models.ToolCall{
		ID:          newToolCallID(strings.TrimSpace(name)),
		Name:        strings.TrimSpace(name),
		Arguments:   args,
		Status:      models.CallStatusPending,
		RequestedAt: time.Now().UTC(),
	}
	result, err := r.executeWithSandbox(ctx, call, sandbox)
	if err != nil {
		if strings.TrimSpace(result.Error) != "" {
			return result.Content, errors.New(result.Error)
		}
		return result.Content, err
	}
	if result.Status == models.CallStatusFailed {
		errMsg := result.Error
		if errMsg == "" {
			errMsg = "tool execution failed"
		}
		return result.Content, errors.New(errMsg)
	}
	return result.Content, nil
}

func (r *Registry) Execute(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	return r.executeWithSandbox(ctx, call, SandboxFromContext(ctx))
}

func (r *Registry) executeWithSandbox(ctx context.Context, call models.ToolCall, sandbox *Sandbox) (models.ToolResult, error) {
	if r == nil {
		return models.ToolResult{}, fmt.Errorf("tool registry is nil")
	}

	r.mu.RLock()
	tool, ok := r.tools[call.Name]
	r.mu.RUnlock()
	if !ok {
		return models.ToolResult{}, fmt.Errorf("tool %q not found", call.Name)
	}
	if err := validateArgs(tool.InputSchema, call.Arguments); err != nil {
		return models.ToolResult{
			CallID:      call.ID,
			ToolName:    call.Name,
			Status:      models.CallStatusFailed,
			Error:       err.Error(),
			CompletedAt: time.Now().UTC(),
		}, err
	}

	started := time.Now().UTC()
	call.Status = models.CallStatusRunning
	call.StartedAt = started

	result, err := tool.Handler(WithSandbox(ctx, sandbox), call)
	if result.CallID == "" {
		result.CallID = call.ID
	}
	if result.ToolName == "" {
		result.ToolName = call.Name
	}
	if result.CompletedAt.IsZero() {
		result.CompletedAt = time.Now().UTC()
	}
	if result.Duration == 0 {
		result.Duration = time.Since(started)
	}
	if err != nil {
		if result.Status == "" {
			result.Status = models.CallStatusFailed
		}
		if result.Error == "" {
			result.Error = err.Error()
		}
		return result, err
	}
	if result.Status == "" {
		result.Status = models.CallStatusCompleted
	}
	return result, nil
}

func (r *Registry) Restrict(allowed []string) *Registry {
	if r == nil {
		return NewRegistry()
	}
	if len(allowed) == 0 {
		return r.Clone()
	}
	return r.RestrictTo(allowed)
}

func (r *Registry) RestrictTo(allowed []string) *Registry {
	if r == nil {
		return NewRegistry()
	}

	allow := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		name = strings.TrimSpace(name)
		if name != "" {
			allow[name] = struct{}{}
		}
	}

	restricted := NewRegistry()
	r.mu.RLock()
	defer r.mu.RUnlock()
	for name, tool := range r.tools {
		if _, ok := allow[name]; ok {
			restricted.tools[name] = tool
		}
	}
	return restricted
}

func newToolCallID(name string) string {
	seq := atomic.AddUint64(&toolCallSeq, 1)
	return fmt.Sprintf("%s_%d_%d", name, time.Now().UTC().UnixNano(), seq)
}

func validateArgs(schema map[string]any, args map[string]any) error {
	if len(schema) == 0 {
		return nil
	}
	if args == nil {
		args = map[string]any{}
	}

	// Fast-path: check for missing required fields before full schema validation
	// so the error message is LLM-actionable rather than raw JSON Schema vocabulary.
	// Include field description/type from the schema so the model knows what to provide.
	if required, ok := schema["required"]; ok {
		var missing []string
		props, _ := schema["properties"].(map[string]any)
		addMissing := func(key string) {
			if _, exists := args[key]; !exists {
				desc := ""
				if props != nil {
					if prop, ok := props[key].(map[string]any); ok {
						desc, _ = prop["description"].(string)
						if desc == "" {
							if typ, _ := prop["type"].(string); typ != "" {
								desc = typ
							}
						}
					}
				}
				if desc != "" {
					missing = append(missing, key+" ("+desc+")")
				} else {
					missing = append(missing, key)
				}
			}
		}
		switch rv := required.(type) {
		case []any:
			for _, r := range rv {
				if key, _ := r.(string); key != "" {
					addMissing(key)
				}
			}
		case []string:
			for _, key := range rv {
				addMissing(key)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("missing required argument(s): %s", strings.Join(missing, "; "))
		}
	}

	data, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("invalid schema: %w", err)
	}
	var s jsonschema.Schema
	if err := s.UnmarshalJSON(data); err != nil {
		return fmt.Errorf("invalid schema: %w", err)
	}
	resolved, err := s.Resolve(nil)
	if err != nil {
		return fmt.Errorf("invalid schema: %w", err)
	}

	// Normalize args to JSON types (e.g. Go int → float64) so that
	// the jsonschema validator sees the same types as json.Unmarshal produces.
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("invalid args: %w", err)
	}
	var normalized any
	if err := json.Unmarshal(argsJSON, &normalized); err != nil {
		return fmt.Errorf("invalid args: %w", err)
	}

	if err := resolved.Validate(normalized); err != nil {
		return fmt.Errorf("invalid tool arguments: %w", err)
	}
	return nil
}
