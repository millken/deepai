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

	"github.com/millken/deepai/pkg/models"
	pkgsandbox "github.com/millken/deepai/pkg/sandbox"
)

type Sandbox = pkgsandbox.Sandbox

type contextKey string

const (
	sandboxContextKey         contextKey = "tool_sandbox"
	threadIDContextKey        contextKey = "tool_thread_id"
	runtimeContextKey         contextKey = "tool_runtime_context"
	userInteractionContextKey contextKey = "tool_user_interaction"
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

var toolCallSeq uint64

type Registry struct {
	mu    sync.RWMutex
	tools map[string]models.Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]models.Tool)}
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

func (r *Registry) ListByGroup(group string) []models.Tool {
	if r == nil {
		return nil
	}
	group = strings.TrimSpace(group)
	if group == "" {
		return r.List()
	}

	all := r.List()
	filtered := make([]models.Tool, 0, len(all))
	for _, tool := range all {
		for _, candidate := range tool.Groups {
			if candidate == group {
				filtered = append(filtered, tool)
				break
			}
		}
	}
	return filtered
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

func WithRuntimeContext(ctx context.Context, values map[string]any) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(values) == 0 {
		return ctx
	}

	cloned := make(map[string]any, len(values))
	for key, value := range values {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		cloned[key] = value
	}
	if len(cloned) == 0 {
		return ctx
	}
	return context.WithValue(ctx, runtimeContextKey, cloned)
}

func RuntimeContextFromContext(ctx context.Context) map[string]any {
	if ctx == nil {
		return nil
	}
	values, _ := ctx.Value(runtimeContextKey).(map[string]any)
	if len(values) == 0 {
		return nil
	}

	cloned := make(map[string]any, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
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
		return r
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

	required, _ := schema["required"].([]any)
	if len(required) == 0 {
		if typed, ok := schema["required"].([]string); ok {
			required = make([]any, 0, len(typed))
			for _, item := range typed {
				required = append(required, item)
			}
		}
	}
	for _, raw := range required {
		name, _ := raw.(string)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		value, ok := args[name]
		if !ok || value == nil {
			return fmt.Errorf("missing required argument %q", name)
		}
	}

	properties, _ := schema["properties"].(map[string]any)
	for name, rawSpec := range properties {
		value, ok := args[name]
		if !ok || value == nil {
			continue
		}
		spec, _ := rawSpec.(map[string]any)
		if err := validateType(name, spec["type"], value); err != nil {
			return err
		}
	}
	return nil
}

func validateType(name string, expected any, value any) error {
	kind, _ := expected.(string)
	switch kind {
	case "", "any":
		return nil
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("argument %q must be a string", name)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("argument %q must be a boolean", name)
		}
	case "integer":
		switch value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return nil
		case float32:
			if float32(int64(value.(float32))) == value.(float32) {
				return nil
			}
		case float64:
			if float64(int64(value.(float64))) == value.(float64) {
				return nil
			}
		}
		return fmt.Errorf("argument %q must be an integer", name)
	case "number":
		switch value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
			return nil
		default:
			return fmt.Errorf("argument %q must be a number", name)
		}
	case "array":
		switch value.(type) {
		case []any, []string:
			return nil
		default:
			return fmt.Errorf("argument %q must be an array", name)
		}
	case "object":
		switch value.(type) {
		case map[string]any:
			return nil
		default:
			return fmt.Errorf("argument %q must be an object", name)
		}
	default:
		return nil
	}
	return nil
}
