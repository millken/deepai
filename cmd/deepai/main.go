package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/sandbox"
	"github.com/millken/deepai/pkg/subagent"
	"github.com/millken/deepai/pkg/tools"
	"github.com/millken/deepai/pkg/tools/builtin"
)

func main() {
	ctx := context.Background()
	baseDir, err := os.MkdirTemp("", "deepai-demo-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(baseDir)

	sb, err := sandbox.New("demo", baseDir)
	if err != nil {
		log.Fatal(err)
	}
	defer sb.Close()

	subPool := subagent.NewPool(subagent.FuncExecutor(func(ctx context.Context, task *subagent.Task, emit func(subagent.TaskEvent)) (subagent.ExecutionResult, error) {
		emit(subagent.TaskEvent{Type: "task_running", TaskID: task.ID, Description: task.Description, Message: "subagent is analyzing the task"})
		time.Sleep(50 * time.Millisecond)
		return subagent.ExecutionResult{
			Result: fmt.Sprintf("subagent[%s] completed: %s", task.Type, task.Prompt),
			Messages: []models.Message{{
				ID:        "subagent-message-1",
				SessionID: task.ID,
				Role:      models.RoleAI,
				Content:   "subagent finished successfully",
			}},
		}, nil
	}), subagent.PoolConfig{
		MaxConcurrent: 1,
		Timeout:       30 * time.Second,
	})

	registry := tools.NewRegistry()
	if err := registry.Register(builtin.BashTool()); err != nil {
		log.Fatal(err)
	}
	if err := registry.Register(tools.TaskTool(subPool)); err != nil {
		log.Fatal(err)
	}

	bashOutput, err := registry.Call(ctx, "bash", map[string]interface{}{
		"command": "echo hello from bash tool",
	}, nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("[tool:bash]", bashOutput)

	runCtx := subagent.WithEventSink(ctx, func(evt subagent.TaskEvent) {
		fmt.Printf("[subagent] %s: %s\n", evt.Type, evt.Message)
	})

	// Create real provider from env (DEEPAI_PROVIDER). Falls back to scripted provider for local demos.
	provName := strings.TrimSpace(os.Getenv("DEEPAI_PROVIDER"))
	var provider llm.LLMProvider
	if provName == "" {
		// no provider configured -> use scripted provider
		provider = newScriptedProvider()
		log.Println("DEEPAI_PROVIDER not set; using scripted provider")
	} else {
		provider = llm.NewProvider(provName)
		log.Printf("using provider: %s", provName)
	}

	modelName := strings.TrimSpace(os.Getenv("DEEPAI_MODEL"))
	if modelName == "" {
		modelName = "demo-model"
	}

	agentRun := agent.New(agent.AgentConfig{
		LLMProvider: provider,
		Tools:       registry,
		Sandbox:     sb,
		AgentType:   agent.AgentTypeCoder,
		Model:       modelName,
		MaxTurns:    3,
	})

	var wg sync.WaitGroup
	wg.Go(func() {
		for evt := range agentRun.Events() {
			switch evt.Type {
			case agent.AgentEventToolCall:
				if evt.ToolCall != nil {
					fmt.Printf("[event] tool call: %s %s\n", evt.ToolCall.Name, evt.ToolCall.ID)
				}
			case agent.AgentEventToolResult:
				if evt.Result != nil {
					fmt.Printf("[event] tool result: %s\n", evt.Result.Content)
				}
			case agent.AgentEventEnd:
				fmt.Printf("[event] agent end: %s\n", evt.Text)
			case agent.AgentEventError:
				fmt.Printf("[event] error: %s\n", evt.Err)
			}
		}
	})

	// Use a more specific prompt to encourage automatic delegation
	specificPrompt := "Delegate a short subagent job via the task tool: instruct the subagent to inspect the repository layout and list top-level packages with a 1-2 sentence description for each. Then summarize findings concisely."
	result, err := agentRun.Run(runCtx, "demo-session", []models.Message{{
		ID:        "m1",
		SessionID: "demo-session",
		Role:      models.RoleHuman,
		Content:   specificPrompt,
	}})
	if err != nil {
		log.Fatal(err)
	}
	wg.Wait()

	fmt.Println("\n[agent final output]")
	fmt.Println(result.FinalOutput)
	if result.Usage != nil {
		fmt.Printf("usage: input=%d output=%d total=%d\n", result.Usage.InputTokens, result.Usage.OutputTokens, result.Usage.TotalTokens)
	}
}

type scriptedProvider struct {
	mu   sync.Mutex
	step int
}

func newScriptedProvider() *scriptedProvider {
	return &scriptedProvider{}
}

func (p *scriptedProvider) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	stream, err := p.Stream(ctx, req)
	if err != nil {
		return llm.ChatResponse{}, err
	}
	var last llm.StreamChunk
	for chunk := range stream {
		last = chunk
	}
	if last.Message == nil {
		return llm.ChatResponse{Model: req.Model}, nil
	}
	return llm.ChatResponse{Model: req.Model, Message: *last.Message}, nil
}

func (p *scriptedProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.mu.Lock()
	step := p.step
	p.step++
	p.mu.Unlock()

	out := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(out)
		switch step {
		case 0:
			out <- llm.StreamChunk{
				Model: req.Model,
				Message: &models.Message{
					Role: models.RoleAI,
					ToolCalls: []models.ToolCall{{
						ID:        "call-task-1",
						Name:      "task",
						Status:    models.CallStatusPending,
						Arguments: map[string]any{"description": "summarize the repo", "prompt": "Inspect the repository layout and report the main packages.", "subagent_type": "general-purpose", "max_turns": 2},
					}},
				},
				Done: true,
			}
		default:
			out <- llm.StreamChunk{
				Model: req.Model,
				Message: &models.Message{
					Role:    models.RoleAI,
					Content: "The delegated subagent completed and the tool chain works end to end.",
				},
				Done: true,
			}
		}
	}()
	return out, nil
}
