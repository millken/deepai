package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/memory"
	"github.com/millken/deepai/pkg/sandbox"
	"github.com/millken/deepai/pkg/subagent"
	"github.com/millken/deepai/pkg/tools"
)

// orphanWait is how long a cancelled turn waits for agent.Run to return
// before abandoning it, matching the REPL's defensive bound for tools that
// ignore ctx. A var so tests can shorten it.
var orphanWait = 10 * time.Second

// DefaultMemoryUserID scopes the bot's long-term memory. It lives in bot.db
// and never shares rows with the CLI's workdir-keyed memory; with multiple
// bots each gets "bot:<id>" (docs/BOT_MULTI_DESIGN.md).
const DefaultMemoryUserID = "bot:default"

// AgentRuntimeConfig is everything the production Runtime needs. The
// deepai-bot command fills it from ~/.deepai/config.yaml the same way the
// interactive REPL is configured (cmd/deepai-bot/runtime.go).
type AgentRuntimeConfig struct {
	Models *llm.ModelRegistry
	Tools  *tools.Registry
	// SystemPrompt is appended to every turn's system prompt (DEEPAI.md
	// instructions, skill descriptions).
	SystemPrompt    string
	WorkDir         string
	SandboxBaseDir  string
	ContextWindow   int
	ReasoningEffort string
	Temperature     *float64
	MaxToolCalls    int
	MaxTokens       *int
	// RequestTimeout bounds one agent run; 0 = unlimited.
	RequestTimeout time.Duration

	MemoryService   *memory.Service
	MemoryExtractor memory.Extractor
	// MemoryUserID is the long-term memory scope; empty = DefaultMemoryUserID.
	MemoryUserID string

	AgentCatalog []agent.AgentInfo

	KeepFullTurns          int
	SessionSummaryDisabled bool
	SessionSummaryModel    string
	ToolOffloadBytes       int

	Version string
	Logger  *slog.Logger
}

// AgentRuntime is the Runtime backed by pkg/agent.
type AgentRuntime struct {
	cfg AgentRuntimeConfig
	log *slog.Logger
}

// NewAgentRuntime validates cfg.
func NewAgentRuntime(cfg AgentRuntimeConfig) (*AgentRuntime, error) {
	if cfg.Models == nil {
		return nil, errors.New("bot: model registry is required")
	}
	if cfg.Tools == nil {
		cfg.Tools = tools.NewRegistry()
	}
	if strings.TrimSpace(cfg.MemoryUserID) == "" {
		cfg.MemoryUserID = DefaultMemoryUserID
	}
	if strings.TrimSpace(cfg.SandboxBaseDir) == "" {
		cfg.SandboxBaseDir = filepath.Join(os.TempDir(), "deepai-bot-sandbox")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &AgentRuntime{cfg: cfg, log: log}, nil
}

func (r *AgentRuntime) Info() Info {
	info := Info{Name: "deepai-bot", Version: r.cfg.Version, WorkDir: r.cfg.WorkDir, DefaultModel: r.cfg.Models.DefaultName()}
	for _, d := range r.cfg.Models.List() {
		info.Models = append(info.Models, ModelInfo{Name: d.Name, Provider: d.Provider, Model: d.Model})
	}
	return info
}

func (r *AgentRuntime) DefaultModel() string { return r.cfg.Models.DefaultName() }

func (r *AgentRuntime) HasModel(alias string) bool { return r.cfg.Models.Has(alias) }

func (r *AgentRuntime) NewConversation(sessionID string) (Conversation, error) {
	sb, err := sandbox.NewSession(r.cfg.SandboxBaseDir, sandbox.Config{})
	if err != nil {
		return nil, fmt.Errorf("sandbox init: %w", err)
	}
	return &agentConversation{r: r, sb: sb, carry: agent.NewSessionCarry()}, nil
}

// agentConversation carries per-session agent state across turns, like the
// REPL does for its one session.
type agentConversation struct {
	r  *AgentRuntime
	sb *sandbox.Sandbox

	mu    sync.Mutex
	carry *agent.SessionCarry
}

func (c *agentConversation) Close() error { return c.sb.Close() }

func (c *agentConversation) RunTurn(ctx context.Context, in TurnInput) (*TurnOutput, error) {
	cfg := c.r.cfg
	provider, modelName, err := cfg.Models.ProviderFor(in.Model)
	if err != nil {
		return nil, fmt.Errorf("resolve model %q: %w", in.Model, err)
	}
	def, _ := cfg.Models.Resolve(in.Model)

	contextWindow := cfg.ContextWindow
	if def.ContextWindow > 0 {
		contextWindow = def.ContextWindow
	}
	effort := cfg.ReasoningEffort
	if def.Effort != "" {
		effort = def.Effort
	}
	temperature := cfg.Temperature
	if def.Temperature != nil {
		temperature = def.Temperature
	}

	c.mu.Lock()
	carry := c.carry
	c.mu.Unlock()

	a := agent.New(agent.AgentConfig{
		LLMProvider:     provider,
		Tools:           cfg.Tools,
		Sandbox:         c.sb,
		Model:           modelName,
		ContextWindow:   contextWindow,
		ReasoningEffort: effort,
		MaxToolCalls:    cfg.MaxToolCalls,
		MaxTokens:       cfg.MaxTokens,
		Temperature:     temperature,
		RequestTimeout:  cfg.RequestTimeout,
		UserInteraction: in.Interaction,
		WorkDir:         cfg.WorkDir,
		MemoryService:   cfg.MemoryService,
		MemoryExtractor: cfg.MemoryExtractor,
		MemoryUserID:    cfg.MemoryUserID,
		ImageDetail:     "low",
		AgentCatalog:    cfg.AgentCatalog,
		Session:         carry,
		KeepFullTurns:   cfg.KeepFullTurns,
		OffloadBytes:    cfg.ToolOffloadBytes,
		Summarizer:      c.summarizer(in.Model),
	})
	if cfg.SystemPrompt != "" {
		a.AppendSystemPrompt(cfg.SystemPrompt)
	}

	gate := &emitGate{emit: in.Emit}
	ctx = subagent.WithEventSink(ctx, func(evt subagent.TaskEvent) {
		gate.send(Event{Type: EventSubagent, Subagent: &evt})
	})

	forwarded := make(chan struct{})
	go func() {
		defer close(forwarded)
		for evt := range a.Events() {
			if out, ok := translateAgentEvent(ctx, evt); ok {
				gate.send(out)
			}
		}
	}()

	type outcome struct {
		res *agent.RunResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := a.Run(ctx, in.SessionID, in.Messages)
		done <- outcome{res, err}
	}()

	var out outcome
	select {
	case out = <-done:
	case <-ctx.Done():
		select {
		case out = <-done:
		case <-time.After(orphanWait):
			// The abandoned Run keeps going in the background. Its
			// events must not outlive this turn (the Manager is about to
			// publish turn_end), and it still owns carry: never share a
			// SessionCarry between two live agents. The forwarder keeps
			// draining a.Events() into the closed gate until Run returns.
			gate.close()
			c.mu.Lock()
			c.carry = agent.NewSessionCarry()
			c.mu.Unlock()
			c.r.log.Warn("bot: agent run did not stop after cancel; abandoned", "session", in.SessionID)
			return &TurnOutput{Messages: in.Messages}, ctx.Err()
		}
	}
	<-forwarded

	result := &TurnOutput{Messages: in.Messages}
	if out.res != nil {
		result.Messages = out.res.Messages
		result.FinalOutput = out.res.FinalOutput
		result.Usage = out.res.Usage
		result.ToolCalls = out.res.ToolCalls
		result.LLMTurns = out.res.LLMTurns
	}
	return result, out.err
}

func (c *agentConversation) summarizer(model string) agent.SessionSummarizer {
	cfg := c.r.cfg
	if cfg.SessionSummaryDisabled || cfg.KeepFullTurns <= 0 {
		return nil
	}
	alias := cfg.SessionSummaryModel
	if alias == "" {
		alias = model
	}
	provider, name, err := cfg.Models.ProviderFor(alias)
	if err != nil {
		c.r.log.Warn("bot: session summary model", "alias", alias, "err", err)
		return nil
	}
	return agent.NewLLMSessionSummarizer(provider, name)
}

// emitGate forwards events until closed. close waits out any send in
// flight, so once it returns no event of this turn can reach Emit again.
type emitGate struct {
	mu     sync.Mutex
	closed bool
	emit   func(Event)
}

func (g *emitGate) send(e Event) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closed {
		g.emit(e)
	}
}

func (g *emitGate) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

// translateAgentEvent maps the agent's event stream onto the wire protocol.
// tool_call/tool_result duplicate tool_call_start/tool_call_end and "end"
// is superseded by turn_end, so they are dropped. Full tool results stay
// out of the stream (the preview is enough to render; the transcript has
// the rest), which keeps a big file read from flooding every client.
func translateAgentEvent(ctx context.Context, evt agent.AgentEvent) (Event, bool) {
	switch evt.Type {
	case agent.AgentEventTextChunk:
		if evt.Text == "" {
			return Event{}, false
		}
		return Event{Type: EventTextDelta, Text: evt.Text}, true
	case agent.AgentEventToolCallStart, agent.AgentEventToolCallEnd:
		if evt.ToolEvent == nil {
			return Event{}, false
		}
		te := *evt.ToolEvent
		te.Result = nil
		typ := EventToolCallStart
		if evt.Type == agent.AgentEventToolCallEnd {
			typ = EventToolCallEnd
		}
		return Event{Type: typ, Tool: &te}, true
	case agent.AgentEventProgress:
		return Event{Type: EventProgress}, true
	case agent.AgentEventCompact:
		return Event{Type: EventCompact, Compact: evt.CompactStats}, true
	case agent.AgentEventError:
		// The run's own cancellation surfaces once, as turn_end status
		// "cancelled"; repeating it as an error event is noise.
		if ctx.Err() != nil {
			return Event{}, false
		}
		e := evt.Error
		if e == nil {
			e = &agent.AgentError{Message: evt.Err}
		}
		return Event{Type: EventError, Error: e}, true
	}
	return Event{}, false
}
