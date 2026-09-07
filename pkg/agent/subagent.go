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
	"github.com/millken/deepai/pkg/skill"
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
	temperature     *float64
	workDir         string
	pluginAgentDirs []string
	// skills is the skill registry used for role-carried Skills preload
	// (profileCfg.Skills) and task.Config.Skill fork-skill resolution (M5-1
	// §2.6). nil means neither feature is available; Execute hard-fails
	// rather than silently ignoring a profile/task that asked for one.
	skills *skill.Registry
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

// WithTemperature sets the global fallback sampling temperature for
// subagents. A models[].temperature entry for the task's resolved alias, or
// an explicit agent-type `temperature:`, wins over it. nil sends none.
func (e *SubagentExecutor) WithTemperature(t *float64) *SubagentExecutor {
	if e != nil {
		e.temperature = t
	}
	return e
}

// WithSkillRegistry wires the skill registry used for a profile's Skills
// preload and a task's fork-skill resolution (§2.6). nil-receiver safe, like
// every other With* here, so a nil *SubagentExecutor chain doesn't panic.
func (e *SubagentExecutor) WithSkillRegistry(r *skill.Registry) *SubagentExecutor {
	if e != nil {
		e.skills = r
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

	// Determine system prompt: explicit > AgentType default (L1,
	// AGENT_CAPABILITY_DESIGN.md §1/§2.2).
	systemPrompt := task.Config.SystemPrompt
	if strings.TrimSpace(systemPrompt) == "" {
		systemPrompt = profileCfg.SystemPrompt
	}

	// L2: role-carried skills preload (profileCfg.Skills). A profile
	// declaring skills with no registry wired, or naming a skill the
	// registry doesn't have, is a misconfiguration — same policy as an
	// unknown agent_type (see the typeResolved check above): failing loudly
	// beats silently running the role without the playbook it depends on.
	// Order is profileCfg.Skills' own order, so the assembled prompt is
	// deterministic across Runs (see the prefix-stability test). Factored
	// into PreloadSkillsProfile (below) so the eval harness's caseFingerprint
	// (pkg/commands/agent_eval.go) computes this step's exact bytes instead
	// of re-deriving the "\n\n"-separator rule independently.
	systemPrompt, err = PreloadSkillsProfile(systemPrompt, agentType, profileCfg.Skills, e.skills)
	if err != nil {
		return subagent.ExecutionResult{}, err
	}

	// §2.6 step 3: task.Config.Skill selects a context:fork skill to run
	// inside THIS subagent (the task tool's `skill` argument — the first
	// real consumer of Meta.Model/MaxTurns/Temperature since Phase 3). Model/
	// MaxTurns/Temperature are captured here and consumed below, alongside
	// the profile's own values and the model registry's, rather than
	// applied immediately — see the priority-chain comments at each site.
	var forkModelAlias string
	var forkMaxTurns *int
	var forkTemperature *float64
	if forkName := strings.TrimSpace(task.Config.Skill); forkName != "" {
		if e.skills == nil {
			return subagent.ExecutionResult{}, fmt.Errorf("task requested skill %q but no skill registry is configured", forkName)
		}
		sk := e.skills.Get(forkName)
		if sk == nil {
			return subagent.ExecutionResult{}, fmt.Errorf("unknown skill %q", forkName)
		}
		if sk.Meta.Agent != "" && sk.Meta.Agent != string(agentType) {
			return subagent.ExecutionResult{}, fmt.Errorf("skill %q is bound to agent %q, not %q", forkName, sk.Meta.Agent, agentType)
		}
		body, err := e.skills.LoadBody(forkName)
		if err != nil {
			return subagent.ExecutionResult{}, fmt.Errorf("load skill %q: %w", forkName, err)
		}
		// args = task.Prompt: the fork skill's $ARGUMENTS is the request the
		// parent agent (or main agent) wrote for this delegation, exactly
		// as if the user had typed "/skill-name <that request>".
		rendered, err := skill.Render(ctx, body, task.Prompt, sk)
		if err != nil {
			return subagent.ExecutionResult{}, fmt.Errorf("render skill %q: %w", forkName, err)
		}
		systemPrompt += "\n\n" + rendered
		forkModelAlias = strings.TrimSpace(sk.Meta.Model)
		forkMaxTurns = sk.Meta.MaxTurns
		forkTemperature = sk.Meta.Temperature
	}

	// §2.6 step 4: when the subagent's own tool set includes "skill",
	// advertise the catalog it can actually act on — excluding a fork skill
	// bound to a DIFFERENT agent type, which the tool call would succeed at
	// reaching but the routing in pkg/skill/tool.go always refuses (see
	// Registry.DescriptionsForAgent).
	if e.skills != nil {
		for _, t := range selectedTools {
			if t.Name == "skill" {
				if desc := e.skills.DescriptionsForAgent(string(agentType)); desc != "" {
					systemPrompt += "\n\n" + desc
				}
				break
			}
		}
	}

	// MaxToolCalls priority: caller-explicit (task tool's max_tool_calls arg)
	// > fork skill's max-turns > agent type profile (builtin/YAML/MD). The
	// fork skill's value is folded into the "caller" argument passed to
	// resolveMaxToolCalls — NOT a new priority tier inside that function —
	// so the one PRODUCTION chain tests already pin
	// (TestResolveMaxToolCalls_Priority) stays the single source of truth;
	// when task.Config.MaxToolCalls is explicit (>0) it always wins, exactly
	// as before this change. 0 (the common case, no skill and no explicit
	// arg) means NO workload cap by design: a fixed default cannot fit both
	// small tasks and whole-project reviews, so the run is bounded by the
	// parent context, the optional token budget, context compaction, and the
	// repeat-call breaker instead. When a cap IS set, react.go counts
	// executed tool calls (model-agnostic) and gracefully wraps up with a
	// final no-tools answer on exhaustion.
	callerMaxToolCalls := task.Config.MaxToolCalls
	if callerMaxToolCalls <= 0 && forkMaxTurns != nil {
		callerMaxToolCalls = *forkMaxTurns
	}
	maxToolCalls := resolveMaxToolCalls(callerMaxToolCalls, profileCfg.MaxToolCalls)

	// Inject OutputSchema prompt into system prompt when available (last,
	// unchanged from before this change). Factored into
	// AppendOutputSchemaPrompt (below) for the same reason as
	// PreloadSkillsProfile above.
	systemPrompt = AppendOutputSchemaPrompt(systemPrompt, profileCfg.OutputSchema)

	// Resolve model alias: task.Config.Model (caller-explicit) > fork
	// skill's model: > agent type YAML model > registry default. The fork
	// skill sits between the caller-explicit override and the profile's own
	// default: it is more specific than "whatever this role normally runs
	// on" (the skill was picked for THIS task) but must not silently
	// override an explicit task-tool `model` argument. This resolution MUST
	// run after the fork-skill block above (which is what determines
	// forkModelAlias) and before e.registry.ProviderFor below — reordering
	// choice for this M5-1 change: the model-resolution block (previously
	// positioned right after the OutputSchema injection) was moved DOWN, to
	// after system-prompt/fork-skill assembly, rather than moving fork-skill
	// resolution up before tool selection — model resolution has no
	// dependency on the selected tool set, so moving it down keeps the
	// system-prompt-building code contiguous instead.
	modelAlias := strings.TrimSpace(task.Config.Model)
	if modelAlias == "" {
		modelAlias = forkModelAlias
	}
	if modelAlias == "" {
		modelAlias = strings.TrimSpace(profileCfg.Model)
	}
	provider, modelName, err := e.registry.ProviderFor(modelAlias)
	if err != nil {
		return subagent.ExecutionResult{}, fmt.Errorf("resolve subagent model: %w", err)
	}

	// Temperature priority: the RESOLVED profile's explicit `temperature:`
	// (builtin profiles carry none) > the fork skill's temperature: > the
	// task's resolved model alias's models[].temperature > the
	// session-level fallback from WithTemperature. nil sends none — Claude
	// 4.7+ rejects sampling parameters outright.
	subTemperature := e.temperature
	if def, ok := e.registry.Resolve(modelAlias); ok && def.Temperature != nil {
		subTemperature = def.Temperature
	}
	if forkTemperature != nil {
		subTemperature = forkTemperature
	}
	if profileCfg.temperatureSet {
		t := profileCfg.Temperature
		subTemperature = &t
	}

	// buildAgentConfig is factored out so a schema-validation retry (below)
	// constructs its fresh agent (agents are single-use, react.go's
	// a.started guard) from the EXACT same config as the original run —
	// duplicating the struct literal at each retry call site would risk the
	// two drifting apart over time. tokenBudget/toolCallBudget are parameters
	// (not always task.Config.TokenBudget / maxToolCalls) so a retry can be
	// given the REMAINING budget instead of the full budget again (see the
	// retry loop below — the same anti-multiplication rule M2-3 applied to
	// TokenBudget).

	buildAgentConfig := func(tokenBudget, toolCallBudget int) AgentConfig {
		return AgentConfig{
			LLMProvider:  provider,
			Tools:        registry,
			MaxToolCalls: toolCallBudget,
			Model:        modelName,
			MaxTokens:    e.maxTokens,
			Temperature:  subTemperature,
			Sandbox:      e.sandbox,
			// runCtx (built by Pool.runTask) carries a deadline only when a
			// task- or pool-level timeout is configured; with none, this value
			// only feeds TimeoutError.Duration reporting.
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
	// Stamp the resolved agent type as the "caller" identity for anything
	// this subagent itself invokes — in particular the skill tool's fork
	// routing (pkg/skill/tool.go), which needs to tell "I'm already running
	// inside my bound agent" apart from "a different subagent" or "the main
	// agent" (empty string).
	ctx = skill.WithCallerAgentType(ctx, string(agentType))

	// stats is the task's workload profile, returned on EVERY exit path below
	// (including error/fail-soft ones — failed delegations are exactly the
	// runs whose cost and shape need analyzing afterwards). Accumulated from
	// each attempt's RunResult so schema retries can't hide their share.
	started := time.Now()
	stats := &subagent.RunStats{
		AgentType:    string(agentType),
		Model:        modelName,
		MaxToolCalls: maxToolCalls,
	}
	accumulateStats := func(r *RunResult) {
		if r == nil {
			return
		}
		stats.ToolCalls += r.ToolCalls
		stats.LLMTurns += r.LLMTurns
		stats.BudgetExhausted = stats.BudgetExhausted || r.BudgetExhausted
	}
	// execStats stamps the elapsed wall time and hands the accumulated stats
	// to a return site — the single place DurationMS is written.
	execStats := func() *subagent.RunStats {
		stats.DurationMS = time.Since(started).Milliseconds()
		return stats
	}

	// runOnce creates a fresh single-use agent, pumps its events through the
	// same emit() pattern as the original run, and blocks on <-eventsDone
	// before returning — every attempt (initial and each retry) needs this
	// identically, since Events() is per-Agent-instance.
	// One progress accumulator for the whole task, not per attempt: a schema
	// retry re-enters runOnce, and the tool/token totals the UI shows are the
	// task's, not the attempt's.
	progress := &subagentProgress{agentType: string(agentType)}

	runOnce := func(msgs []models.Message, tokenBudget, toolCallBudget int) (*RunResult, error) {
		runAgent := New(buildAgentConfig(tokenBudget, toolCallBudget))
		eventsDone := make(chan struct{})
		go func() {
			defer close(eventsDone)
			for evt := range runAgent.Events() {
				progressEvt, ok := progress.event(evt)
				if !ok {
					continue
				}
				progressEvt.Type = "task_running"
				progressEvt.TaskID = task.ID
				progressEvt.Description = task.Description
				emit(progressEvt)
			}
		}()
		result, err := runAgent.Run(ctx, task.ID, msgs)
		<-eventsDone
		accumulateStats(result)
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
	}, task.Config.TokenBudget, maxToolCalls)
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
		return subagent.ExecutionResult{Usage: errUsage, Stats: execStats()}, err
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
				// the SAME task-level budgets, not get fresh ones — otherwise
				// N retries could spend up to N times task.Config.TokenBudget
				// or maxToolCalls. Pass the remaining allowance (original
				// minus everything spent so far); once nothing remains, stop
				// retrying instead of running one more attempt with a bogus
				// budget (0 means *unlimited* to AgentConfig, so we must never
				// pass 0 here to mean "none left") and fall through to the
				// fail-soft return below with whatever output we already have.
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
				// Same rule for the tool-call budget: count the tool-result
				// messages the last attempt appended (every executed call —
				// and every synthesized refusal — produces exactly one), give
				// the retry only what's left, and stop retrying when the
				// budget is already spent. Without this, a capped Strict-
				// schema subagent could execute up to (1+MaxRetries)× its
				// tool-call budget across attempts.
				retryToolCalls := maxToolCalls
				if maxToolCalls > 0 {
					remaining := maxToolCalls - countToolResultMessages(result.Messages)
					if remaining <= 0 {
						break
					}
					retryToolCalls = remaining
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
					AgentType:   string(agentType),
					Message:     "retrying: output failed schema validation",
				})
				stats.SchemaRetries++

				retryResult, retryErr := runOnce(retryMsgs, retryBudget, retryToolCalls)
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
				Stats:    execStats(),
			}, nil
		}
	}

	return subagent.ExecutionResult{
		Result:   result.FinalOutput,
		Messages: result.Messages,
		Usage:    convertSubagentUsage(totalUsage),
		Stats:    execStats(),
	}, nil
}

// resolveMaxToolCalls is the MaxToolCalls resolution chain: caller-explicit
// (task tool's max_tool_calls arg) > agent-type profile (builtin/YAML/MD) >
// 0 (no cap). A named function (not inline in Execute) so tests pin the
// production chain rather than a local copy of it.
func resolveMaxToolCalls(caller, profile int) int {
	if caller > 0 {
		return caller
	}
	return profile
}

// countToolResultMessages counts RoleTool messages in a run's history — one
// per executed tool call (and per synthesized refusal) — which is how the
// schema-retry loop derives the tool-call budget an attempt already spent.
func countToolResultMessages(messages []models.Message) int {
	n := 0
	for _, msg := range messages {
		if msg.Role == models.RoleTool {
			n++
		}
	}
	return n
}

// unknownAgentTypeError builds the rejection for an agent_type nothing defines.// It lists the types that DO resolve so the model can correct itself on the next
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

// NewSubagentPool creates a pool with a SubagentExecutor. The pool has no
// local concurrency cap — see subagent.NewPool for why the provider's own
// rate limiting governs parallelism.
// Chain WithContextWindow on the result of NewSubagentExecutor if needed.
func NewSubagentPool(executor *SubagentExecutor, timeout time.Duration) *subagent.Pool {
	return subagent.NewPool(executor, subagent.PoolConfig{
		Timeout: timeout,
	})
}

// SelectSubagentTools is the exported form of selectSubagentTools —
// SubagentExecutor.Execute's own tool-selection rule (name-or-group match,
// "task" always stripped from the candidate set, empty selectors keeps
// everything but "task", and a selector list that matches nothing is a hard
// error rather than silently widening to "no restriction"). Exported so a
// caller that needs the EXACT tool set a dispatched subagent would get —
// currently only the eval harness's caseFingerprint (pkg/commands/
// agent_eval.go), computing the restricted registry it feeds to
// AssembleSystemPrompt — derives it via this function instead of
// re-implementing the name/group/empty/no-match rules a second time.
func SelectSubagentTools(all []models.Tool, selectors []string) ([]models.Tool, error) {
	return selectSubagentTools(all, selectors)
}

// PreloadSkillsProfile appends each named skill's body (in profileSkills'
// own order), separated by a blank line, onto base — the exact formatting
// SubagentExecutor.Execute applies for a role's Skills preload (L2,
// AGENT_CAPABILITY_DESIGN.md §1). len(profileSkills) == 0 is a no-op
// regardless of skillReg (including a nil one), matching Execute exactly —
// a role that declares no Skills never requires a skill registry to be
// wired. Exported for the same reason as SelectSubagentTools above: this
// step's separator rule and its "skills declared but no registry configured"
// failure mode must not be re-derived independently by a caller outside
// this package.
func PreloadSkillsProfile(base string, agentType AgentType, profileSkills []string, skillReg *skill.Registry) (string, error) {
	if len(profileSkills) == 0 {
		return base, nil
	}
	if skillReg == nil {
		return "", fmt.Errorf("agent type %q declares skills but no skill registry is configured", agentType)
	}
	out := base
	for _, name := range profileSkills {
		body, err := skillReg.LoadBody(name)
		if err != nil {
			return "", fmt.Errorf("preload skill %q for agent type %q: %w", name, agentType, err)
		}
		out += "\n\n" + body
	}
	return out, nil
}

// AppendOutputSchemaPrompt appends the "Output your response as JSON..."
// wrapper SubagentExecutor.Execute injects when a role's OutputSchema is
// set and carries a non-empty Prompt (the last step of Execute's
// system-prompt assembly). schema == nil (no schema mounted) or an empty
// Prompt is a no-op, returning base unchanged. Exported for the same reason
// as PreloadSkillsProfile above: the wrapper text itself must not be
// re-derived independently by a caller outside this package.
func AppendOutputSchemaPrompt(base string, schema *OutputSchema) string {
	if schema != nil && schema.Prompt != "" {
		return base + "\n\nOutput your response as JSON matching this schema:\n" + schema.Prompt
	}
	return base
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

// subagentProgress converts a subagent's own AgentEvents into structured
// TaskEvents, carrying the running totals the parent UI needs. One instance
// per subagent run; not safe for concurrent use (each run pumps its events on
// a single goroutine).
type subagentProgress struct {
	agentType string
	toolCalls int
	tokens    int
	// lastPing throttles the liveness events (thinking pings and text
	// chunks). Tool events are NEVER throttled — they carry content.
	lastPing time.Time
}

// subagentPingInterval bounds how often a subagent forwards a liveness ping
// (AgentEventProgress / AgentEventTextChunk) to its parent. The parent redraws
// its status block about once a second; anything faster is wasted work on a
// channel whose overflow policy is to drop.
const subagentPingInterval = time.Second

// maxToolArgsSummary bounds ToolArgs: it renders on one status line next to
// the tool name, so a whole write_file body must never reach the UI.
const maxToolArgsSummary = 60

// event returns the progress event for one AgentEvent, or ok=false when the
// event carries nothing worth showing.
func (p *subagentProgress) event(evt AgentEvent) (subagent.TaskEvent, bool) {
	message := subagentMessageFromAgentEvent(evt)
	if strings.TrimSpace(message) == "" {
		return subagent.TaskEvent{}, false
	}
	// A liveness ping says only "the model is still working". It is what keeps
	// the parent's status line moving through a minutes-long thinking phase,
	// but it arrives far too often to forward every one.
	switch subagentPhaseFromAgentEvent(evt) {
	case subagent.PhaseThinking, subagent.PhaseGenerating:
		now := time.Now()
		if now.Sub(p.lastPing) < subagentPingInterval {
			return subagent.TaskEvent{}, false
		}
		p.lastPing = now
	}
	// Everything else (tool events, errors) is content and always forwarded.
	// Usage is cumulative-to-date when present; a later event without it must
	// not reset the total to zero.
	if evt.Usage != nil && evt.Usage.TotalTokens > 0 {
		p.tokens = evt.Usage.TotalTokens
	}
	out := subagent.TaskEvent{
		Message:   message,
		AgentType: p.agentType,
		Tokens:    p.tokens,
		Phase:     subagentPhaseFromAgentEvent(evt),
	}
	if evt.ToolEvent != nil {
		if evt.Type == AgentEventToolCallEnd {
			p.toolCalls++
		}
		out.ToolName = evt.ToolEvent.Name
		out.ToolArgs = summarizeToolArgs(evt.ToolEvent.ArgumentsText)
		out.DurationMS = evt.ToolEvent.DurationMS
		switch {
		case evt.ToolEvent.Error != "":
			out.ToolStatus = "error"
		case evt.Type == AgentEventToolCallEnd:
			out.ToolStatus = "ok"
		default:
			out.ToolStatus = "running"
		}
	}
	out.ToolCalls = p.toolCalls
	return out, true
}

// summarizeToolArgs flattens a tool's argument JSON into a short single-line
// hint. Structure is not preserved — this is a glance, not a record.
func summarizeToolArgs(args string) string {
	s := strings.TrimSpace(args)
	if s == "" {
		return ""
	}
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxToolArgsSummary {
		s = string(r[:maxToolArgsSummary-1]) + "…"
	}
	return s
}

// subagentPhaseFromAgentEvent classifies one AgentEvent into the phase the
// subagent is in while producing it.
func subagentPhaseFromAgentEvent(evt AgentEvent) string {
	switch evt.Type {
	case AgentEventProgress:
		return subagent.PhaseThinking
	case AgentEventTextChunk:
		return subagent.PhaseGenerating
	case AgentEventToolCallStart, AgentEventToolCallEnd:
		if evt.ToolEvent != nil {
			return subagent.PhaseTool
		}
	}
	return ""
}

func subagentMessageFromAgentEvent(evt AgentEvent) string {
	switch evt.Type {
	case AgentEventProgress:
		return "⋯ thinking"
	case AgentEventTextChunk:
		// The text itself streams into the subagent's own transcript; the
		// parent's one-line status only needs the fact that it is flowing.
		if strings.TrimSpace(evt.Text) == "" {
			return ""
		}
		return "⋯ generating"
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
