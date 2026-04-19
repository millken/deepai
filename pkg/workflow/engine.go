package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/subagent"
)

const maxOutputLen = 10000

// ConditionFunc evaluates whether a stage should execute.
type ConditionFunc func(results map[string]*StageResult) bool

var (
	conditionMu  sync.RWMutex
	conditionReg = map[string]ConditionFunc{
		"has_critical_issues": hasCriticalIssuesCondition,
		"always":              func(map[string]*StageResult) bool { return true },
		"never":               func(map[string]*StageResult) bool { return false },
	}
)

// RegisterCondition adds a custom condition function.
func RegisterCondition(name string, fn ConditionFunc) {
	conditionMu.Lock()
	defer conditionMu.Unlock()
	conditionReg[name] = fn
}

func hasCriticalIssuesCondition(results map[string]*StageResult) bool {
	for _, r := range results {
		if r.Status != "completed" {
			continue
		}
		var review agent.ReviewResult
		if err := json.Unmarshal([]byte(r.Output), &review); err != nil {
			continue
		}
		for _, issue := range review.Issues {
			if issue.Severity == "critical" {
				return true
			}
		}
	}
	return false
}

func evaluateCondition(name string, results map[string]*StageResult) (bool, error) {
	if name == "" {
		return true, nil
	}
	conditionMu.RLock()
	fn, ok := conditionReg[name]
	conditionMu.RUnlock()
	if !ok {
		return false, fmt.Errorf("unknown condition %q", name)
	}
	return fn(results), nil
}

// EnvEvent represents a stage lifecycle event for real-time rendering.
type EnvEvent struct {
	Type  string // "stage_started", "stage_completed", "stage_skipped", "stage_failed"
	Stage string
	Role  string
}

// Engine executes Workflows using the subagent infrastructure.
type Engine struct {
	executor subagent.Executor
	pool     *subagent.Pool
	workDir  string
	env      *Environment // optional message bus
}

// NewEngine creates a workflow engine.
// pool is required for workflows with parallel stages; single-stage waves use executor directly.
func NewEngine(executor subagent.Executor, pool *subagent.Pool, workDir string) *Engine {
	if pool == nil {
		pool = subagent.NewPool(executor, subagent.PoolConfig{
			MaxConcurrent: 1,
		})
	}
	return &Engine{executor: executor, pool: pool, workDir: workDir}
}

// WithEnvironment attaches a message bus for inter-agent communication.
func (e *Engine) WithEnvironment(env *Environment) *Engine {
	if e != nil {
		e.env = env
	}
	return e
}

// Run executes a workflow with the given user input.
func (e *Engine) Run(ctx context.Context, wf *Workflow, userInput string) (*WorkflowResult, error) {
	if wf == nil {
		return nil, fmt.Errorf("workflow is required")
	}
	if err := wf.Validate(); err != nil {
		return nil, err
	}

	waves, err := topologicalSort(wf.Stages)
	if err != nil {
		return nil, err
	}

	baseline := e.recordBaseline()
	results := make(map[string]*StageResult, len(wf.Stages))
	var lastOutput string
	var stageOrder []string

	for _, wave := range waves {
		if ctx.Err() != nil {
			return e.buildResult(wf.Name, "failed", results, stageOrder, lastOutput), ctx.Err()
		}

		if len(wave) == 1 {
			sr, err := e.executeStage(ctx, wave[0], userInput, results, baseline)
			if err != nil {
				return e.buildResult(wf.Name, "failed", results, stageOrder, lastOutput), err
			}
			results[wave[0].Name] = sr
			stageOrder = append(stageOrder, wave[0].Name)
			if sr.Status == "completed" {
				lastOutput = sr.Output
			}
		} else {
			waveResults, err := e.executeParallel(ctx, wave, userInput, results, baseline)
			if err != nil {
				return e.buildResult(wf.Name, "failed", results, stageOrder, lastOutput), err
			}
			for _, sr := range waveResults {
				results[sr.Name] = sr
				stageOrder = append(stageOrder, sr.Name)
				if sr.Status == "completed" {
					lastOutput = sr.Output
				}
			}
		}
	}

	status := "completed"
	for _, sr := range results {
		if sr != nil && sr.Status == "failed" {
			status = "partial"
			break
		}
	}
	return e.buildResult(wf.Name, status, results, stageOrder, lastOutput), nil
}

func (e *Engine) executeStage(ctx context.Context, s WorkflowStage, userInput string, results map[string]*StageResult, baseline string) (*StageResult, error) {
	ok, err := evaluateCondition(s.Condition, results)
	if err != nil {
		return nil, err
	}
	if !ok {
		e.emitStageEvent(ctx, EnvEvent{Type: "stage_skipped", Stage: s.Name, Role: string(s.Role)})
		return &StageResult{Name: s.Name, Status: "skipped"}, nil
	}

	e.emitStageEvent(ctx, EnvEvent{Type: "stage_started", Stage: s.Name, Role: string(s.Role)})
	prompt := e.buildPrompt(s, userInput, results, baseline)
	task := &subagent.Task{
		ID:     fmt.Sprintf("wf-%s", s.Name),
		Prompt: prompt,
		Config: subagent.SubagentConfig{
			AgentType: string(s.Role),
		},
	}

	var execErr error
	var execResult subagent.ExecutionResult
	for attempt := 0; attempt <= s.MaxRetries; attempt++ {
		execResult, execErr = e.executor.Execute(ctx, task, func(subagent.TaskEvent) {})
		if execErr == nil {
			output := truncateOutput(execResult.Result)
			e.publishMessage(ctx, s, output)
			e.emitStageEvent(ctx, EnvEvent{Type: "stage_completed", Stage: s.Name, Role: string(s.Role)})
			return &StageResult{Name: s.Name, Output: output, Status: "completed"}, nil
		}
		if ctx.Err() != nil {
			e.emitStageEvent(ctx, EnvEvent{Type: "stage_failed", Stage: s.Name, Role: string(s.Role)})
			return &StageResult{Name: s.Name, Status: "failed"}, ctx.Err()
		}
	}
	e.emitStageEvent(ctx, EnvEvent{Type: "stage_failed", Stage: s.Name, Role: string(s.Role)})
	return &StageResult{Name: s.Name, Status: "failed"}, fmt.Errorf("stage %q failed after %d attempts: %w", s.Name, s.MaxRetries+1, execErr)
}

func (e *Engine) executeParallel(ctx context.Context, wave []WorkflowStage, userInput string, results map[string]*StageResult, baseline string) ([]*StageResult, error) {
	g, gctx := errgroup.WithContext(ctx)
	stageResults := make([]*StageResult, len(wave))

	for i, s := range wave {
		i, s := i, s
		g.Go(func() error {
			ok, err := evaluateCondition(s.Condition, results)
			if err != nil {
				return err
			}
			if !ok {
				e.emitStageEvent(gctx, EnvEvent{Type: "stage_skipped", Stage: s.Name, Role: string(s.Role)})
				stageResults[i] = &StageResult{Name: s.Name, Status: "skipped"}
				return nil
			}

			e.emitStageEvent(gctx, EnvEvent{Type: "stage_started", Stage: s.Name, Role: string(s.Role)})
			prompt := e.buildPrompt(s, userInput, results, baseline)
			var lastErr error
			for attempt := 0; attempt <= s.MaxRetries; attempt++ {
				if gctx.Err() != nil {
					e.emitStageEvent(gctx, EnvEvent{Type: "stage_failed", Stage: s.Name, Role: string(s.Role)})
					return gctx.Err()
				}
				task, err := e.pool.StartTask(gctx, s.Name, prompt, subagent.SubagentConfig{
					AgentType: string(s.Role),
				})
				if err != nil {
					lastErr = fmt.Errorf("stage %q start: %w", s.Name, err)
					continue
				}
				completed, err := e.pool.Wait(gctx, task.ID)
				if err != nil {
					lastErr = fmt.Errorf("stage %q wait: %w", s.Name, err)
					continue
				}
				if completed.Status == subagent.TaskStatusFailed || completed.Status == subagent.TaskStatusTimedOut {
					lastErr = fmt.Errorf("stage %q: %s", s.Name, completed.Error)
					continue
				}
				output := truncateOutput(completed.Result)
				e.publishMessage(gctx, s, output)
				e.emitStageEvent(gctx, EnvEvent{Type: "stage_completed", Stage: s.Name, Role: string(s.Role)})
				stageResults[i] = &StageResult{Name: s.Name, Output: output, Status: "completed"}
				return nil
			}
			e.emitStageEvent(gctx, EnvEvent{Type: "stage_failed", Stage: s.Name, Role: string(s.Role)})
			return lastErr
		})
	}

	if err := g.Wait(); err != nil {
		return stageResults, err
	}
	return stageResults, nil
}

func (e *Engine) buildPrompt(s WorkflowStage, userInput string, results map[string]*StageResult, baseline string) string {
	vars := map[string]string{
		"UserInput": userInput,
	}
	for _, dep := range s.InputFrom {
		if sr, ok := results[dep]; ok {
			vars["outputs."+dep] = sr.Output
		}
	}
	if baseline != "" {
		vars["diff"] = e.collectDiffSince(baseline)
	}
	return expandTemplate(s.Prompt, vars)
}

func (e *Engine) buildResult(name, status string, stages map[string]*StageResult, stageOrder []string, finalOutput string) *WorkflowResult {
	return &WorkflowResult{
		Name:        name,
		Status:      status,
		Stages:      stages,
		StageOrder:  stageOrder,
		FinalOutput: finalOutput,
	}
}

func (e *Engine) recordBaseline() string {
	if e.workDir == "" {
		return ""
	}
	cmd := exec.Command("git", "stash", "create")
	cmd.Dir = e.workDir
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return "HEAD"
	}
	return strings.TrimSpace(string(out))
}

func (e *Engine) collectDiffSince(baseline string) string {
	if e.workDir == "" || baseline == "" {
		return ""
	}
	cmd := exec.Command("git", "diff", baseline)
	cmd.Dir = e.workDir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	if len(out) > 50000 {
		out = out[:50000]
	}
	return string(out)
}

func expandTemplate(tmpl string, vars map[string]string) string {
	result := tmpl
	for k, v := range vars {
		result = strings.ReplaceAll(result, "{{."+k+"}}", v)
	}
	return result
}

func truncateOutput(s string) string {
	if len(s) > maxOutputLen {
		return s[:maxOutputLen] + "\n... [truncated]"
	}
	return s
}

// emitStageEvent sends a stage lifecycle event via the subagent event sink in context.
func (e *Engine) emitStageEvent(ctx context.Context, evt EnvEvent) {
	subagent.EmitEvent(ctx, subagent.TaskEvent{
		Type:        evt.Type,
		TaskID:      fmt.Sprintf("wf-%s", evt.Stage),
		Description: evt.Stage,
		Message:     fmt.Sprintf("%s %s (%s)", evt.Type, evt.Stage, evt.Role),
	})
}

// publishMessage sends a stage result as an AgentMessage to the Environment.
func (e *Engine) publishMessage(ctx context.Context, s WorkflowStage, output string) {
	if e.env == nil {
		return
	}
	msg := newAgentMessage(s.Name, "", stageMsgType(s.Role), output)
	e.env.Publish(ctx, msg)
}

// stageMsgType maps agent roles to message types.
func stageMsgType(role agent.AgentType) string {
	switch role {
	case agent.AgentTypeCoder:
		return MsgTypeCodeChange
	case agent.AgentTypeSecurityReviewer, agent.AgentTypeArchReviewer, agent.AgentTypePerfReviewer:
		return MsgTypeReviewResult
	case agent.AgentTypeProductManager:
		return MsgTypePRD
	case agent.AgentTypeArchitect:
		return MsgTypeDesign
	default:
		return "result"
	}
}
