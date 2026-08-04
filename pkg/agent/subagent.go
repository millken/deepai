package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
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
	agentType := normalizeAgentType(AgentType(task.Config.EffectiveAgentType()))
	if agentType == "" {
		agentType = AgentTypeGeneral
	}
	profileCfg, profileProblems, typeResolved := resolveAgentTypeConfigResolved(agentType, e.workDir, e.pluginAgentDirs)
	if !typeResolved {
		// Nothing on disk and no builtin defines this type. Refusing is the
		// same policy selectSubagentTools applies to an unmatched tools
		// selector: a typo must not silently widen privileges. The lenient
		// general-purpose fallback that used to happen here was strictly worse
		// than an explicit general-purpose — it left DefaultTools empty, which
		// selectSubagentTools reads as "no restriction", so a hallucinated
		// agent_type ran with EVERY registered tool.
		return subagent.ExecutionResult{}, e.unknownAgentTypeError(agentType, profileProblems)
	}

	// Determine tools: explicit Tools > AgentType DefaultTools > all
	var toolSelectors []string
	if len(task.Config.Tools) > 0 {
		toolSelectors = task.Config.Tools
	} else if len(profileCfg.DefaultTools) > 0 {
		toolSelectors = profileCfg.DefaultTools
	}

	selectedTools, err := selectSubagentTools(e.tools.List(), toolSelectors)
	if err != nil {
		return subagent.ExecutionResult{}, err
	}
	registry := tools.NewRegistry()
	for _, tool := range selectedTools {
		_ = registry.Register(tool)
	}

	// Determine system prompt: explicit > AgentType default
	systemPrompt := task.Config.SystemPrompt
	if strings.TrimSpace(systemPrompt) == "" {
		systemPrompt = profileCfg.SystemPrompt
	}

	// MaxTurns priority: caller-explicit (max_turns arg) > agent type profile
	// (builtin/YAML/MD) > safety floor. The pool contributes nothing here — it
	// deliberately injects no per-type defaults (see Pool.resolveConfig), so a
	// profile's MaxTurns can no longer be shadowed for the two types the pool
	// used to special-case.
	maxTurns := task.Config.MaxTurns
	if maxTurns <= 0 {
		maxTurns = profileCfg.MaxTurns
	}
	if maxTurns <= 0 {
		// Last resort safety floor: a profile with no MaxTurns (e.g. builtin
		// general-purpose) must still be bounded. 15 lets a delegated subagent
		// do meaningful multi-file work (explore + edit + verify) without
		// hitting the wall mid-task, which at the old 6 turned most delegations
		// into wasted tokens — the parent had to redo everything from scratch.
		maxTurns = 15
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

	// profileTemperature must be handed to the agent EXPLICITLY: New() runs
	// ApplyAgentType(&cfg, cfg.AgentType), and the config below deliberately
	// leaves AgentType unset (setting it would make ApplyAgentType re-resolve
	// the profile WITHOUT e.workDir and then Restrict the already-selected
	// registry against that wrong tool list — a project YAML that adds a tool
	// the builtin profile lacks would lose it). With AgentType empty,
	// ApplyAgentType filled in general-purpose's temperature for every
	// subagent, so the per-type Temperature (coder 0.1, bash 0.0, ...) reached
	// the provider for no type at all. Passing it explicitly wins because
	// ApplyAgentType only defaults a nil Temperature. Read from the RESOLVED
	// profile, so a project YAML's `temperature:` applies too.
	profileTemperature := profileCfg.Temperature

	// buildAgentConfig is factored out so a schema-validation retry (below)
	// constructs its fresh agent (agents are single-use, react.go's
	// a.started guard) from the EXACT same config as the original run —
	// duplicating the struct literal at each retry call site would risk the
	// two drifting apart over time. tokenBudget is a parameter (not always
	// task.Config.TokenBudget) so a retry can be given the REMAINING budget
	// instead of the full budget again (see the retry loop below).

	buildAgentConfig := func(tokenBudget int) AgentConfig {
		return AgentConfig{
			LLMProvider: provider,
			Tools:       registry,
			MaxTurns:    maxTurns,
			Model:       modelName,
			MaxTokens:   e.maxTokens,
			Temperature: &profileTemperature,
			Sandbox:     e.sandbox,
			// runCtx (built by Pool.runTask via context.WithTimeout) always carries
			// a deadline, so react.go's requestTimeout-from-bare-ctx branch never
			// fires here — this value only feeds TimeoutError.Duration reporting.
			RequestTimeout: task.Config.Timeout,
			ContextWindow:  e.contextWindow,
			SystemPrompt:   systemPrompt,
			NonInteractive: true,
			// MaxTokensBudget: optional per-task total-token cap (task tool's
			// token_budget arg → SubagentConfig.TokenBudget). 0 = unlimited, same
			// as AgentConfig's own zero value. react.go's turn-loop budget check
			// (a.maxTokensBudget) enforces this inside the subagent's own Run.
			MaxTokensBudget: tokenBudget,
		}
	}

	// A subagent is delegated work — it must never block on the user. Strip any
	// inherited UserInteraction so plan confirmations auto-approve and
	// clarifications fall back to best-judgment instead of prompting.
	ctx = tools.WithUserInteraction(ctx, nil)

	// runOnce creates a fresh single-use agent, pumps its events through the
	// same emit() pattern as the original run, and blocks on <-eventsDone
	// before returning — every attempt (initial and each retry) needs this
	// identically, since Events() is per-Agent-instance.
	runOnce := func(msgs []models.Message, tokenBudget int) (*RunResult, error) {
		runAgent := New(buildAgentConfig(tokenBudget))
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
		result, err := runAgent.Run(ctx, task.ID, msgs)
		<-eventsDone
		return result, err
	}

	// Context bundling happens at seed-message construction, BEFORE any run
	// or validation logic below — a retry (the schema-validation loop
	// further down) reuses this seeded message via result.Messages, so the
	// context block only needs to be built once, here, and every retry
	// inherits it for free.
	seedContent := task.Prompt
	if len(task.Config.ContextFiles) > 0 {
		block, cfErr := e.buildContextFilesBlock(task.Config.ContextFiles)
		if cfErr != nil {
			return subagent.ExecutionResult{}, cfErr
		}
		seedContent = block + task.Prompt
	}

	result, err := runOnce([]models.Message{
		{
			ID:        newSubagentMessageID("human"),
			SessionID: task.ID,
			Role:      models.RoleHuman,
			Content:   seedContent,
			CreatedAt: time.Now().UTC(),
		},
	}, task.Config.TokenBudget)
	if err != nil {
		// H1: Run() populates Usage on every error path it can (max turns,
		// token budget exceeded, stream/context errors, ...), so dropping it
		// here would silently under-report the subagent's real consumption
		// to the parent run's roll-up (react.go's addSubagentUsage) whenever
		// a subagent fails instead of completing cleanly. result can be nil
		// on the handful of paths that error out before any run state exists
		// (e.g. ctx already done) — guard against that.
		var errUsage *subagent.TokenUsage
		if result != nil {
			errUsage = convertSubagentUsage(result.Usage)
		}
		return subagent.ExecutionResult{Usage: errUsage}, err
	}

	// totalUsage accumulates across every attempt (initial + retries) — a
	// failed schema-validation retry still spent real tokens, and the
	// caller's cost accounting (react.go's addSubagentUsage) must see them.
	// sumUsage(nil, x) is the one clone path in this file (no hand-rolled
	// second copy) — nil+x returns a fresh clone of x, so mutating totalUsage
	// later never aliases result.Usage.
	totalUsage := sumUsage(nil, result.Usage)

	// L3 (schema leftover, coordinator decision): a non-Strict OutputSchema
	// only ever contributed a prompt suffix (the "Output your response as
	// JSON matching this schema" injection above) — it must never validate,
	// retry, or fail-soft. Gating the whole block on Strict here (rather
	// than just the retry loop below) means ValidateOutput is never even
	// called for a non-Strict schema, so a mismatch can no longer produce
	// the WARNING-prefixed fail-soft Result for a schema that was never
	// supposed to be enforced. All three builtin reviewer schemas are
	// Strict, so this is a no-op for them.
	if profileCfg.OutputSchema != nil && profileCfg.OutputSchema.Strict {
		valErr := ValidateOutput(profileCfg.OutputSchema, result.FinalOutput)
		if valErr != nil {
			for retry := 0; retry < profileCfg.OutputSchema.MaxRetries; retry++ {
				// M2-3 MEDIUM (budget multiplication): a retry must draw down
				// the SAME task-level budget, not get a fresh one — otherwise
				// N retries could spend up to N times task.Config.TokenBudget.
				// Pass the remaining allowance (original minus everything
				// spent so far); once nothing remains, stop retrying instead
				// of running one more attempt with a bogus budget (0 means
				// *unlimited* to AgentConfig, so we must never pass 0 here to
				// mean "none left") and fall through to the fail-soft return
				// below with whatever output we already have.
				retryBudget := task.Config.TokenBudget
				if task.Config.TokenBudget > 0 {
					spent := 0
					if totalUsage != nil {
						spent = totalUsage.TotalTokens
					}
					remaining := task.Config.TokenBudget - spent
					if remaining <= 0 {
						break
					}
					retryBudget = remaining
				}

				retryMsgs := appendParseError(result.Messages, result.FinalOutput, valErr)
				// appendParseError deliberately leaves the seeded message's
				// ID/SessionID/CreatedAt at their zero value — stamp them
				// here (not in appendParseError itself) so the retry's seed
				// message is consistent with the initial human message
				// constructed above.
				seeded := &retryMsgs[len(retryMsgs)-1]
				seeded.ID = newSubagentMessageID("human")
				seeded.SessionID = task.ID
				seeded.CreatedAt = time.Now().UTC()

				emit(subagent.TaskEvent{
					Type:        "task_running",
					TaskID:      task.ID,
					Description: task.Description,
					Message:     "retrying: output failed schema validation",
				})

				retryResult, retryErr := runOnce(retryMsgs, retryBudget)
				if retryResult != nil {
					totalUsage = sumUsage(totalUsage, retryResult.Usage)
				}
				if retryErr != nil {
					// LOW (approved fall-through): the task-level deadline
					// (runCtx, built by Pool.runTask via context.WithTimeout)
					// is shared across every attempt — initial run AND every
					// retry draw against the SAME clock — so a retry dying to
					// the shared deadline here is the common case, not a rare
					// one. When a previous attempt already produced a
					// FinalOutput, treat this exactly like the retries-
					// exhausted fail-soft path below (break, keep the last
					// attempt's `result` and `valErr` as-is) rather than
					// returning a hard error: react.go's runOneTool discards
					// ToolResult.Content on any non-nil Execute error, so a
					// hard error here would silently drop the entire raw
					// output over a shared-clock race, not a genuine failure.
					//
					// Two distinct shapes reach here for the SAME underlying
					// cause: if the retry's Agent.Run() sees the ctx already
					// expired at its own entry point (before any Stream call),
					// it returns the bare, unwrapped context.DeadlineExceeded
					// (errors.Is catches this). If instead the deadline fires
					// mid-request/mid-stream, react.go's normalizeRunError
					// converts it into a *TimeoutError before returning
					// (errors.As catches this). TimeoutError now also
					// implements Unwrap() -> context.DeadlineExceeded, so
					// errors.Is alone would catch this second shape too — the
					// errors.As arm below is kept anyway (harmless
					// redundancy, not a correctness dependency) rather than
					// narrowed, since removing it buys nothing and this is
					// not the place to relitigate that.
					var timeoutErr *TimeoutError
					if (errors.Is(retryErr, context.DeadlineExceeded) || errors.As(retryErr, &timeoutErr)) && strings.TrimSpace(result.FinalOutput) != "" {
						break
					}
					// Any other retry error is a genuine failure (not just
					// schema validation) — a genuine error, unlike the
					// fail-soft path below. `result` still holds the last
					// attempt that DID complete (it is only reassigned below
					// on success), so return that instead of losing it to a
					// zero-value ExecutionResult.
					return subagent.ExecutionResult{
						Result:   result.FinalOutput,
						Messages: result.Messages,
						Usage:    convertSubagentUsage(totalUsage),
					}, fmt.Errorf("subagent retry failed after schema validation error (%v): %w", valErr, retryErr)
				}
				result = retryResult
				valErr = ValidateOutput(profileCfg.OutputSchema, result.FinalOutput)
				if valErr == nil {
					break
				}
			}
		}
		if valErr != nil {
			// M2-3 HIGH (fail-soft): do NOT return an error here, even though
			// every retry still failed validation (this block only runs at
			// all for a Strict schema — see the L3 gate above).
			// react.go's runOneTool rebuilds a brand-new ToolResult on any
			// non-nil Execute error (react.go, the `if err != nil` branch in
			// runOneTool) and does NOT copy the original result.Content across
			// — only Data survives. So a non-nil error here would silently
			// drop the entire, possibly-substantive raw output over one
			// missing/malformed field. Instead, fold the failure into Result
			// itself (Execute still succeeds) so the parent model sees both
			// the warning and the raw content and can judge for itself.
			return subagent.ExecutionResult{
				Result:   fmt.Sprintf("%s%v. Raw output follows:\n\n%s", outputSchemaWarningPrefix, valErr, result.FinalOutput),
				Messages: result.Messages,
				Usage:    convertSubagentUsage(totalUsage),
			}, nil
		}
	}

	return subagent.ExecutionResult{
		Result:   result.FinalOutput,
		Messages: result.Messages,
		Usage:    convertSubagentUsage(totalUsage),
	}, nil
}

// unknownAgentTypeError builds the rejection for an agent_type nothing defines.
// It lists the types that DO resolve so the model can correct itself on the next
// call instead of retrying the same bad name — the same self-correction the
// unmatched-tools-selector error offers. Enumeration touches disk, which is fine
// on this error-only path. Any load problems collected while resolving the type
// are appended: a type whose ONLY definition is a broken file lands here too,
// and "unknown" alone would be actively misleading in that case.
func (e *SubagentExecutor) unknownAgentTypeError(t AgentType, problems []string) error {
	var names []string
	for _, info := range EnumerateAgents(e.workDir, e.pluginAgentDirs) {
		names = append(names, string(info.Type))
	}
	sort.Strings(names)
	err := fmt.Errorf("unknown agent_type %q; available agent types: %s", t, strings.Join(names, ", "))
	if len(problems) > 0 {
		err = fmt.Errorf("%w (agent config load problems: %s)", err, strings.Join(problems, "; "))
	}
	return err
}

// outputSchemaWarningPrefix marks a fail-soft ExecutionResult.Result: the
// output never passed OutputSchema validation after retries were exhausted
// (Strict schemas only — see the L3 gate above; a non-Strict schema never
// validates at all, so this prefix never applies to one), but Execute still
// returns a nil error so the raw content is not dropped by react.go's
// runOneTool (see the comment at the fail-soft return site above for why
// that matters). Tests assert this prefix rather than an error string.
const outputSchemaWarningPrefix = "WARNING: output failed schema validation: "

// NewSubagentPool creates a pool with a SubagentExecutor.
// Chain WithContextWindow on the result of NewSubagentExecutor if needed.
func NewSubagentPool(executor *SubagentExecutor, maxConcurrent int, timeout time.Duration) *subagent.Pool {
	return subagent.NewPool(executor, subagent.PoolConfig{
		MaxConcurrent: maxConcurrent,
		Timeout:       timeout,
	})
}

func selectSubagentTools(all []models.Tool, selectors []string) ([]models.Tool, error) {
	if len(selectors) == 0 {
		return filterTaskTool(all), nil
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
		return selected, nil
	}
	// Selectors were given but none matched a registered tool: this is a
	// misconfiguration (e.g. a typo'd tools list), not "no restriction" —
	// widening to all tools here would be a privilege escalation. Fail hard
	// so the model sees the error and can correct the tools list.
	err := fmt.Errorf("agent type tools list matched no registered tools: %v", selectors)
	// "task" is unconditionally stripped from `all` above (subagents never
	// recurse into further subagents), so it can never satisfy a selector —
	// naming it verbatim in the plain error above reads as a typo/missing
	// tool rather than "categorically unavailable here". Clarify when it's
	// among the selectors so the misconfiguration is actionable.
	for _, selector := range selectors {
		if strings.TrimSpace(selector) == "task" {
			err = fmt.Errorf("%w (note: \"task\" is unavailable to subagents and is always excluded)", err)
			break
		}
	}
	return nil, err
}

// filterTaskTool returns a copy of tools with the task tool removed.
func filterTaskTool(tools []models.Tool) []models.Tool {
	out := make([]models.Tool, 0, len(tools))
	for _, t := range tools {
		if t.Name != "task" {
			out = append(out, t)
		}
	}
	return out
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
