package agent

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/sandbox"
	"github.com/millken/deepai/pkg/subagent"
	"github.com/millken/deepai/pkg/tools"
)

var subagentMessageSeq uint64

type SubagentExecutor struct {
	registry        *llm.ModelRegistry
	tools           *tools.Registry
	sandbox         *sandbox.Sandbox
	contextWindow   int
	maxTokens       *int
	workDir         string
	pluginAgentDirs []string
}

func NewSubagentExecutor(registry *llm.ModelRegistry, toolReg *tools.Registry, sb *sandbox.Sandbox) *SubagentExecutor {
	if toolReg == nil {
		toolReg = tools.NewRegistry()
	}
	return &SubagentExecutor{
		registry: registry,
		tools:    toolReg,
		sandbox:  sb,
	}
}

// WithWorkDir sets the working directory for YAML agent config loading.
func (e *SubagentExecutor) WithWorkDir(dir string) *SubagentExecutor {
	if e != nil {
		e.workDir = dir
	}
	return e
}

// WithContextWindow sets the context window for subagents.
func (e *SubagentExecutor) WithContextWindow(n int) *SubagentExecutor {
	if e != nil {
		e.contextWindow = n
	}
	return e
}

// WithMaxTokens sets the max output tokens for subagent LLM calls. When nil,
// the provider default applies (e.g. 8192 for Anthropic), which may truncate
// large tool call arguments (e.g. write_file with a big file).
func (e *SubagentExecutor) WithMaxTokens(n *int) *SubagentExecutor {
	if e != nil {
		e.maxTokens = n
	}
	return e
}

// WithPluginAgentDirs sets the plugin agent directories (<plugin>/agents) used
// to resolve plugin-bundled agents. The slice must be the claudeplugin.Discover
// result order — the same slice EnumerateAgents consumes — so advertising and
// execution agree on which source backs a given agent type.
func (e *SubagentExecutor) WithPluginAgentDirs(dirs []string) *SubagentExecutor {
	if e != nil {
		e.pluginAgentDirs = dirs
	}
	return e
}

func (e *SubagentExecutor) Execute(ctx context.Context, task *subagent.Task, emit func(subagent.TaskEvent)) (subagent.ExecutionResult, error) {
	if e == nil || e.registry == nil {
		return subagent.ExecutionResult{}, fmt.Errorf("subagent model registry is required")
	}

	// Resolve agent type config: project YAML/MD > plugin MD > builtin > general
	agentType := AgentType(task.Config.EffectiveAgentType())
	if agentType == "" {
		agentType = AgentTypeGeneral
	}
	profileCfg := resolveAgentTypeConfigWithPlugins(agentType, e.workDir, e.pluginAgentDirs)

	// Determine tools: explicit Tools > AgentType DefaultTools > all
	var toolSelectors []string
	if len(task.Config.Tools) > 0 {
		toolSelectors = task.Config.Tools
	} else if len(profileCfg.DefaultTools) > 0 {
		toolSelectors = profileCfg.DefaultTools
	}

	registry := tools.NewRegistry()
	for _, tool := range selectSubagentTools(e.tools.List(), toolSelectors) {
		_ = registry.Register(tool)
	}

	// Determine system prompt: explicit > AgentType default
	systemPrompt := task.Config.SystemPrompt
	if strings.TrimSpace(systemPrompt) == "" {
		systemPrompt = profileCfg.SystemPrompt
	}

	maxTurns := task.Config.MaxTurns
	if maxTurns <= 0 && profileCfg.MaxTurns > 0 {
		maxTurns = profileCfg.MaxTurns
	}

	// Inject OutputSchema prompt into system prompt when available
	if profileCfg.OutputSchema != nil && profileCfg.OutputSchema.Prompt != "" {
		systemPrompt += "\n\nOutput your response as JSON matching this schema:\n" + profileCfg.OutputSchema.Prompt
	}

	// Resolve model alias: task.Config.Model > agent type YAML model > registry default.
	modelAlias := strings.TrimSpace(task.Config.Model)
	if modelAlias == "" {
		modelAlias = strings.TrimSpace(profileCfg.Model)
	}
	provider, modelName, err := e.registry.ProviderFor(modelAlias)
	if err != nil {
		return subagent.ExecutionResult{}, fmt.Errorf("resolve subagent model: %w", err)
	}

	runAgent := New(AgentConfig{
		LLMProvider:    provider,
		Tools:          registry,
		MaxTurns:       maxTurns,
		Model:          modelName,
		MaxTokens:      e.maxTokens,
		Sandbox:        e.sandbox,
		RequestTimeout: task.Config.Timeout,
		ContextWindow:  e.contextWindow,
		SystemPrompt:   systemPrompt,
		NonInteractive: true,
	})

	eventsDone := make(chan struct{})
	go func() {
		defer close(eventsDone)
		for evt := range runAgent.Events() {
			message := subagentMessageFromAgentEvent(evt)
			if strings.TrimSpace(message) == "" {
				continue
			}
			emit(subagent.TaskEvent{
				Type:        "task_running",
				TaskID:      task.ID,
				Description: task.Description,
				Message:     message,
			})
		}
	}()

	// A subagent is delegated work — it must never block on the user. Strip any
	// inherited UserInteraction so plan confirmations auto-approve and
	// clarifications fall back to best-judgment instead of prompting.
	ctx = tools.WithUserInteraction(ctx, nil)

	result, err := runAgent.Run(ctx, task.ID, []models.Message{
		{
			ID:        newSubagentMessageID("human"),
			SessionID: task.ID,
			Role:      models.RoleHuman,
			Content:   task.Prompt,
			CreatedAt: time.Now().UTC(),
		},
	})
	<-eventsDone
	if err != nil {
		return subagent.ExecutionResult{}, err
	}
	return subagent.ExecutionResult{
		Result:   result.FinalOutput,
		Messages: result.Messages,
	}, nil
}

// NewSubagentPool creates a pool with a SubagentExecutor.
// Chain WithContextWindow on the result of NewSubagentExecutor if needed.
func NewSubagentPool(executor *SubagentExecutor, maxConcurrent int, timeout time.Duration) *subagent.Pool {
	return subagent.NewPool(executor, subagent.PoolConfig{
		MaxConcurrent: maxConcurrent,
		Timeout:       timeout,
	})
}

func selectSubagentTools(all []models.Tool, selectors []string) []models.Tool {
	if len(selectors) == 0 {
		return append([]models.Tool(nil), all...)
	}

	allowNames := make(map[string]struct{}, len(selectors))
	allowGroups := make(map[string]struct{}, len(selectors))
	for _, selector := range selectors {
		selector = strings.TrimSpace(selector)
		if selector == "" {
			continue
		}
		allowNames[selector] = struct{}{}
		allowGroups[selector] = struct{}{}
	}

	selected := make([]models.Tool, 0, len(all))
	for _, tool := range all {
		if tool.Name == "task" {
			continue
		}
		if _, ok := allowNames[tool.Name]; ok {
			selected = append(selected, tool)
			continue
		}
		for _, group := range tool.Groups {
			if _, ok := allowGroups[group]; ok {
				selected = append(selected, tool)
				break
			}
		}
	}
	if len(selected) > 0 {
		return selected
	}
	return append([]models.Tool(nil), all...)
}

func subagentMessageFromAgentEvent(evt AgentEvent) string {
	switch evt.Type {
	case AgentEventToolCallStart:
		if evt.ToolEvent != nil {
			return "⚙ " + evt.ToolEvent.Name
		}
	case AgentEventToolCallEnd:
		if evt.ToolEvent != nil {
			if evt.ToolEvent.Error != "" {
				return "✗ " + evt.ToolEvent.Name + ": " + evt.ToolEvent.Error
			}
			return "✓ " + evt.ToolEvent.Name
		}
	case AgentEventError:
		if s := strings.TrimSpace(evt.Err); s != "" {
			return "✗ " + s
		}
	}
	return ""
}

func newSubagentMessageID(prefix string) string {
	seq := atomic.AddUint64(&subagentMessageSeq, 1)
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UTC().UnixNano(), seq)
}
