package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/clarification"
	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/logs"
	"github.com/millken/deepai/pkg/memory"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/sandbox"
	"github.com/millken/deepai/pkg/skill"
	"github.com/millken/deepai/pkg/subagent"
	"github.com/millken/deepai/pkg/tools"
	"github.com/millken/deepai/pkg/tools/builtin"
)

func main() {
	ctx := context.Background()

	// Configure logging (optional — debug file if env set).
	cleanup, err := logs.Setup(logs.FromEnv())
	if err != nil {
		slog.Error("log setup failed", "err", err)
		os.Exit(1)
	}
	defer cleanup()

	workDir, err := os.Getwd()
	if err != nil {
		slog.Error("getwd failed", "err", err)
		os.Exit(1)
	}
	sb, err := sandbox.New("demo", workDir)
	if err != nil {
		slog.Error("sandbox init failed", "err", err)
		os.Exit(1)
	}
	defer sb.Close()

	provName := strings.TrimSpace(os.Getenv("DEEPAI_PROVIDER"))
	var provider llm.LLMProvider
	if provName == "" {
		provider = newScriptedProvider()
		slog.Warn("DEEPAI_PROVIDER not set; using scripted provider")
	} else {
		provider = llm.NewProvider(provName)
		slog.Info("using provider", "name", provName)
	}

	modelName := strings.TrimSpace(os.Getenv("DEEPAI_MODEL"))
	if modelName == "" {
		modelName = "demo-model"
	}

	// Memory service initialization
	var memService *memory.Service
	var memExtractor memory.Extractor
	if databaseURL := strings.TrimSpace(os.Getenv("DEEPAI_DATABASE_URL")); databaseURL != "" {
		memStore, err := memory.OpenStore(ctx, databaseURL)
		if err != nil {
			slog.Error("memory store init failed", "err", err)
			os.Exit(1)
		}
		defer memStore.Close()
		memService = memory.NewService(slog.Default(), memStore, nil)
		if err := memService.AutoMigrate(ctx); err != nil {
			slog.Warn("memory auto-migrate failed", "err", err)
		}
	}
	if memService != nil {
		memExtractor = memory.NewLLMClient(provider, modelName)
		slog.Info("memory service enabled")
	}

	contextWindow := 0
	if v := strings.TrimSpace(os.Getenv("DEEPAI_CONTEXT_WINDOW")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			contextWindow = n
		}
	}

	subExecutor := agent.NewSubagentExecutor(provider, nil, sb, modelName).WithContextWindow(contextWindow)
	subPool := agent.NewSubagentPool(subExecutor, 1, 30*time.Second)

	registry := tools.NewRegistry()
	for _, tool := range append(
		[]models.Tool{builtin.BashTool(), clarification.AskClarificationTool(nil), tools.TaskTool(subPool), tools.GitAutoCommitTool(provider)},
		append(append(builtin.FileTools(), builtin.GitTools()...), builtin.WebTools()...)...,
	) {
		if err := registry.Register(tool); err != nil {
			slog.Error("register tool failed", "err", err)
			os.Exit(1)
		}
	}
	if memService != nil {
		if err := registry.Register(builtin.MemoryTool(memService)); err != nil {
			slog.Error("register memory tool failed", "err", err)
			os.Exit(1)
		}
	}

	skillReg := skill.NewRegistry()
	if err := skillReg.LoadAll(workDir, nil); err != nil {
		slog.Warn("skill load failed", "err", err)
	}
	if skillReg.Count() > 0 {
		if err := registry.Register(skill.SkillToolWithRegistry(skillReg)); err != nil {
			slog.Error("register skill tool failed", "err", err)
			os.Exit(1)
		}
		slog.Info("loaded skills", "count", skillReg.Count(), "names", strings.Join(skillReg.AvailableNames(), ", "))
	}

	ui := &cliUserInteraction{In: os.Stdin, Out: os.Stderr}
	runCtx := tools.WithUserInteraction(ctx, ui)
	runCtx = subagent.WithEventSink(runCtx, func(evt subagent.TaskEvent) {
		fmt.Fprintf(os.Stderr, "[subagent] %s: %s\n", evt.Type, evt.Message)
	})

	// Load DEEPAI.md: global + project.
	home, _ := os.UserHomeDir()
	var deepaiMD string
	for _, p := range []string{
		filepath.Join(home, ".deepai", "DEEPAI.md"),
		filepath.Join(workDir, ".deepai", "DEEPAI.md"),
	} {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if content := strings.TrimSpace(string(data)); content != "" {
			if deepaiMD != "" {
				deepaiMD += "\n\n"
			}
			deepaiMD += content
			slog.Info("loaded DEEPAI.md", "path", p, "chars", len(content))
		}
	}

	fmt.Println("deepai interactive mode — type your prompt, Ctrl+D or 'exit' to quit")
	fmt.Println()

	scanner := bufio.NewScanner(os.Stdin)
	sessionID := "interactive-session"
	msgSeq := 0
	turnsSinceMemory := 0
	const memoryNudgeInterval = 10
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
		slog.Info("--- user turn ---", "turn", msgSeq)
		slog.Info("user input", "text", input)

		history = append(history, models.Message{
			ID:        fmt.Sprintf("m-%d", msgSeq),
			SessionID: sessionID,
			Role:      models.RoleHuman,
			Content:   input,
		})

	agentRun := agent.New(agent.AgentConfig{
			LLMProvider:     provider,
			Tools:           registry,
			Sandbox:         sb,
			AgentType:       agent.AgentTypeCoder,
			Model:           modelName,
			ContextWindow:   contextWindow,
			MemoryService:   memService,
			MemoryExtractor: memExtractor,
		})

		if desc := skillReg.Descriptions(); desc != "" {
			agentRun.AppendSystemPrompt(desc)
		}
		if deepaiMD != "" {
			agentRun.AppendSystemPrompt(deepaiMD)
		}

		go func() {
			for evt := range agentRun.Events() {
				switch evt.Type {
				case agent.AgentEventToolCall:
					if evt.ToolCall != nil {
						fmt.Fprintf(os.Stderr, "[tool call] %s(%s)\n", evt.ToolCall.Name, evt.ToolCall.ID)
						argsJSON, _ := json.Marshal(evt.ToolCall.Arguments)
						slog.Debug("tool call", "name", evt.ToolCall.Name, "id", evt.ToolCall.ID, "args", string(argsJSON))
					}
				case agent.AgentEventToolResult:
					if evt.Result != nil {
						content := evt.Result.Content
						if len(content) > 200 {
							fmt.Fprintf(os.Stderr, "[tool result] %s...\n", content[:200])
							slog.Debug("tool result", "content", content)
						} else {
							fmt.Fprintf(os.Stderr, "[tool result] %s\n", content)
						}
					}
				case agent.AgentEventTextChunk:
					if evt.Text != "" {
						slog.Debug("text chunk", "text", evt.Text)
					}
				case agent.AgentEventError:
					fmt.Fprintf(os.Stderr, "[error] %s\n", evt.Err)
					slog.Error("agent error", "err", evt.Err)
				}
			}
		}()

		result, err := agentRun.Run(runCtx, sessionID, history)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[error] %v\n", err)
			continue
		}

		if memService != nil && memExtractor != nil {
			used := usedMemoryTool(result.Messages)
			if used {
				turnsSinceMemory = 0
			} else {
				turnsSinceMemory++
			}
			if turnsSinceMemory >= memoryNudgeInterval {
				turnsSinceMemory = 0
				slog.Debug("memory nudge triggered", "turns", memoryNudgeInterval)
			}
			if !used {
				memService.ScheduleUpdateWith(sessionID, result.Messages, memExtractor)
			}
		}

		history = result.Messages

		if result.FinalOutput != "" {
			fmt.Fprintf(os.Stderr, "\n%s\n\n", result.FinalOutput)
			slog.Info("response", "text", result.FinalOutput)
		}
		if result.Usage != nil {
			fmt.Fprintf(os.Stderr, "[usage] in=%d out=%d total=%d\n\n", result.Usage.InputTokens, result.Usage.OutputTokens, result.Usage.TotalTokens)
			slog.Info("usage", "in", result.Usage.InputTokens, "out", result.Usage.OutputTokens, "total", result.Usage.TotalTokens)
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

func newScriptedProvider() *scriptedProvider { return &scriptedProvider{} }

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
		if step == 0 {
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
		} else {
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

// usedMemoryTool checks if the agent invoked the memory tool in the given messages.
func usedMemoryTool(messages []models.Message) bool {
	for _, msg := range messages {
		for _, call := range msg.ToolCalls {
			if call.Name == "memory" {
				return true
			}
		}
	}
	return false
}
