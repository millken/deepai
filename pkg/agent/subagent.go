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
	llm     llm.LLMProvider
	tools   *tools.Registry
	sandbox *sandbox.Sandbox
	model   string
}

func NewSubagentExecutor(provider llm.LLMProvider, registry *tools.Registry, sb *sandbox.Sandbox, model ...string) *SubagentExecutor {
	if registry == nil {
		registry = tools.NewRegistry()
	}
	selectedModel := resolveModel("")
	if len(model) > 0 {
		selectedModel = resolveModel(model[0])
	}
	return &SubagentExecutor{
		llm:     provider,
		tools:   registry,
		sandbox: sb,
		model:   selectedModel,
	}
}

func (e *SubagentExecutor) Execute(ctx context.Context, task *subagent.Task, emit func(subagent.TaskEvent)) (subagent.ExecutionResult, error) {
	if e == nil || e.llm == nil {
		return subagent.ExecutionResult{}, fmt.Errorf("subagent llm provider is required")
	}

	registry := tools.NewRegistry()
	for _, tool := range selectSubagentTools(e.tools.List(), task.Config.Tools) {
		_ = registry.Register(tool)
	}

	runAgent := New(AgentConfig{
		LLMProvider:    e.llm,
		Tools:          registry,
		MaxTurns:       task.Config.MaxTurns,
		Model:          e.model,
		Sandbox:        e.sandbox,
		RequestTimeout: task.Config.Timeout,
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

	result, err := runAgent.Run(ctx, task.ID, []models.Message{
		{
			ID:        newSubagentMessageID("system"),
			SessionID: task.ID,
			Role:      models.RoleSystem,
			Content:   subagentSystemPrompt(task),
			CreatedAt: time.Now().UTC(),
		},
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

func NewSubagentPool(provider llm.LLMProvider, registry *tools.Registry, sb *sandbox.Sandbox, maxConcurrent int, timeout time.Duration, model ...string) *subagent.Pool {
	return subagent.NewPool(NewSubagentExecutor(provider, registry, sb, model...), subagent.PoolConfig{
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
	return selected
}

func subagentMessageFromAgentEvent(evt AgentEvent) string {
	switch evt.Type {
	case AgentEventChunk, AgentEventTextChunk:
		return strings.TrimSpace(evt.Text)
	case AgentEventToolCallStart:
		if evt.ToolEvent != nil {
			return fmt.Sprintf("calling tool %s", evt.ToolEvent.Name)
		}
	case AgentEventToolCallEnd:
		if evt.ToolEvent != nil {
			if evt.ToolEvent.Error != "" {
				return fmt.Sprintf("tool %s failed: %s", evt.ToolEvent.Name, evt.ToolEvent.Error)
			}
			return fmt.Sprintf("tool %s completed", evt.ToolEvent.Name)
		}
	case AgentEventError:
		return strings.TrimSpace(evt.Err)
	}
	return ""
}

func subagentSystemPrompt(task *subagent.Task) string {
	if task != nil && strings.TrimSpace(task.Config.SystemPrompt) != "" {
		return strings.TrimSpace(task.Config.SystemPrompt)
	}
	return "You are a focused subagent. Complete the assigned task and return the result."
}

func newSubagentMessageID(prefix string) string {
	seq := atomic.AddUint64(&subagentMessageSeq, 1)
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UTC().UnixNano(), seq)
}
