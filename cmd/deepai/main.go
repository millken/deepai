package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/clarification"
	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/sandbox"
	"github.com/millken/deepai/pkg/subagent"
	"github.com/millken/deepai/pkg/tools"
	"github.com/millken/deepai/pkg/tools/builtin"
)

func main() {
	ctx := context.Background()

	// Use current working directory as sandbox root so tools operate on the actual project.
	// Falls back to a temp dir if not in a project.
	workDir, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	sb, err := sandbox.New("demo", workDir)
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
	for _, tool := range builtin.FileTools() {
		if err := registry.Register(tool); err != nil {
			log.Fatal(err)
		}
	}
	if err := registry.Register(clarification.AskClarificationTool(nil)); err != nil {
		log.Fatal(err)
	}
	if err := registry.Register(tools.TaskTool(subPool)); err != nil {
		log.Fatal(err)
	}

	provName := strings.TrimSpace(os.Getenv("DEEPAI_PROVIDER"))
	var provider llm.LLMProvider
	if provName == "" {
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

	ui := &cliUserInteraction{In: os.Stdin, Out: os.Stderr}
	runCtx := tools.WithUserInteraction(ctx, ui)
	runCtx = subagent.WithEventSink(runCtx, func(evt subagent.TaskEvent) {
		fmt.Fprintf(os.Stderr, "[subagent] %s: %s\n", evt.Type, evt.Message)
	})

	fmt.Println("deepai interactive mode — type your prompt, Ctrl+D or 'exit' to quit")
	fmt.Println()

	scanner := bufio.NewScanner(os.Stdin)
	sessionID := "interactive-session"
	msgSeq := 0
	debug := os.Getenv("DEEPAI_DEBUG") != ""
	var history []models.Message

	for {
		fmt.Fprint(os.Stderr, "> ")
		if !scanner.Scan() {
			break
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		if input == "exit" || input == "quit" {
			break
		}

		msgSeq++

		// Append user message to session history
		history = append(history, models.Message{
			ID:        fmt.Sprintf("m-%d", msgSeq),
			SessionID: sessionID,
			Role:      models.RoleHuman,
			Content:   input,
		})

		// Create a fresh agent per turn (agents are single-use).
		agentRun := agent.New(agent.AgentConfig{
			LLMProvider: provider,
			Tools:       registry,
			Sandbox:     sb,
			AgentType:   agent.AgentTypeCoder,
			Model:       modelName,
		})

		go func() {
			for evt := range agentRun.Events() {
				switch evt.Type {
				case agent.AgentEventToolCall:
					if evt.ToolCall != nil {
						if debug {
							argsJSON, _ := json.Marshal(evt.ToolCall.Arguments)
							fmt.Fprintf(os.Stderr, "[tool call] %s(%s) %s\n", evt.ToolCall.Name, evt.ToolCall.ID, argsJSON)
						} else {
							fmt.Fprintf(os.Stderr, "[tool call] %s(%s)\n", evt.ToolCall.Name, evt.ToolCall.ID)
						}
					}
				case agent.AgentEventToolResult:
					if evt.Result != nil {
						content := evt.Result.Content
						if !debug && len(content) > 200 {
							content = content[:200] + "..."
						}
						fmt.Fprintf(os.Stderr, "[tool result] %s\n", content)
					}
				case agent.AgentEventError:
					fmt.Fprintf(os.Stderr, "[error] %s\n", evt.Err)
				}
			}
		}()

		result, err := agentRun.Run(runCtx, sessionID, history)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[error] %v\n", err)
			continue
		}

		// Append agent's messages to history for session continuity
		history = result.Messages

		if result.FinalOutput != "" {
			fmt.Fprintf(os.Stderr, "\n%s\n\n", result.FinalOutput)
		}
		if result.Usage != nil {
			fmt.Fprintf(os.Stderr, "[usage] in=%d out=%d total=%d\n\n", result.Usage.InputTokens, result.Usage.OutputTokens, result.Usage.TotalTokens)
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "[error] reading input: %v\n", err)
	}
	fmt.Fprintln(os.Stderr, "bye.")
}

// cliUserInteraction implements tools.UserInteraction via stdin/stdout.
type cliUserInteraction struct {
	In  io.Reader
	Out io.Writer
}

func (c *cliUserInteraction) AskQuestion(_ context.Context, question string, options []string) (string, error) {
	fmt.Fprintf(c.Out, "\n[agent asks]: %s\n", question)
	for i, opt := range options {
		fmt.Fprintf(c.Out, "  %d. %s\n", i+1, opt)
	}
	fmt.Fprint(c.Out, "> ")

	scanner := bufio.NewScanner(c.In)
	if !scanner.Scan() {
		return "", scanner.Err()
	}
	return scanner.Text(), nil
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
