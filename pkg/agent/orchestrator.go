package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/millken/deepai/pkg/subagent"
)

// OrchestratorEvent represents a progress event during pipeline execution.
type OrchestratorEvent struct {
	Type    string // "actor_started", "actor_completed", "reviewer_started", "reviewer_completed", "reviewer_failed"
	Round   int
	Name    string // agent type or reviewer key
	Message string
}

// Orchestrator executes a Pipeline: actor → reviewers → [actor(retry) → reviewers]*
type Orchestrator struct {
	executor subagent.Executor
	pool     *subagent.Pool
	workDir  string
	onEvent  func(OrchestratorEvent)
}

// NewOrchestrator creates a new orchestrator.
func NewOrchestrator(executor subagent.Executor, pool *subagent.Pool, workDir string) *Orchestrator {
	return &Orchestrator{
		executor: executor,
		pool:     pool,
		workDir:  workDir,
	}
}

// WithEventSink sets a callback for progress events.
func (o *Orchestrator) WithEventSink(fn func(OrchestratorEvent)) *Orchestrator {
	if o != nil {
		o.onEvent = fn
	}
	return o
}

func (o *Orchestrator) emit(evt OrchestratorEvent) {
	if o.onEvent != nil {
		o.onEvent(evt)
	}
}

// OrchestratorInput is the input for an orchestrator run.
type OrchestratorInput struct {
	UserInput string
	WorkDir   string
}

// OrchestratorResult is the output of an orchestrator run.
type OrchestratorResult struct {
	Verdict     string                  // "pass" | "issues_found"
	ActorOutput string                  // final actor output
	Reviews     map[string]ReviewResult // key = ReviewerRef.ReviewerKey()
	Rounds      int                     // actual rounds executed
}

// Run executes the full pipeline.
func (o *Orchestrator) Run(ctx context.Context, pipeline *Pipeline, input OrchestratorInput) (*OrchestratorResult, error) {
	if pipeline == nil {
		return nil, fmt.Errorf("pipeline is required")
	}
	if err := pipeline.Validate(); err != nil {
		return nil, err
	}

	maxRounds := pipeline.EffectiveMaxRounds()
	workDir := input.WorkDir
	if workDir == "" {
		workDir = o.workDir
	}

	var (
		actorOutput  string
		lastReviews  map[string]ReviewResult
		previousDiff string
	)

	baseline := o.recordBaseline(workDir)

	for round := 0; round < maxRounds; round++ {
		// 1. Build actor prompt
		actorPrompt := o.buildActorPrompt(pipeline.Actor, input.UserInput, previousDiff, lastReviews)

		// 2. Execute actor via executor (creates new Agent per Execute, satisfies single-use)
		o.emit(OrchestratorEvent{Type: "actor_started", Round: round, Name: string(pipeline.Actor.AgentType)})
		actorTask := &subagent.Task{
			ID:     fmt.Sprintf("pipeline-actor-round-%d", round),
			Prompt: actorPrompt,
			Config: subagent.SubagentConfig{
				AgentType: string(pipeline.Actor.AgentType),
			},
		}
		actorResult, err := o.executor.Execute(ctx, actorTask, func(subagent.TaskEvent) {})
		if err != nil {
			return nil, fmt.Errorf("actor run round %d: %w", round, err)
		}
		actorOutput = actorResult.Result
		o.emit(OrchestratorEvent{Type: "actor_completed", Round: round, Name: string(pipeline.Actor.AgentType)})

		// 3. No reviewers → return
		if len(pipeline.Reviewers) == 0 {
			return &OrchestratorResult{
				Verdict:     "pass",
				ActorOutput: actorOutput,
				Rounds:      round + 1,
			}, nil
		}

		// 4. Collect diff since baseline
		previousDiff = o.collectDiffSince(baseline, workDir)

		// 5. Run reviewers in parallel
		changedFiles := o.collectChangedFiles(baseline, workDir)
		baseline = o.recordBaseline(workDir)
		reviewInput := ReviewInput{Diff: previousDiff, Output: actorOutput, Files: changedFiles}
		results, err := o.runReviewers(ctx, pipeline.Reviewers, reviewInput)
		if err != nil {
			return nil, err
		}
		lastReviews = results

		// 6. All pass → return
		if aggregateVerdict(results) == "pass" {
			return &OrchestratorResult{
				Verdict:     "pass",
				ActorOutput: actorOutput,
				Reviews:     results,
				Rounds:      round + 1,
			}, nil
		}

		// 7. No critical issues → report and return
		if !hasCriticalIssues(results) {
			return &OrchestratorResult{
				Verdict:     "issues_found",
				ActorOutput: actorOutput,
				Reviews:     results,
				Rounds:      round + 1,
			}, nil
		}

		// 8. OnIssues check
		switch pipeline.OnIssues {
		case "report":
			return &OrchestratorResult{
				Verdict:     "issues_found",
				ActorOutput: actorOutput,
				Reviews:     results,
				Rounds:      round + 1,
			}, nil
		case "retry":
			// continue to next round
		default:
			return nil, fmt.Errorf("invalid on_issues value %q: must be \"retry\" or \"report\"", pipeline.OnIssues)
		}
	}

	// Exhausted max rounds
	return &OrchestratorResult{
		Verdict:     "issues_found",
		ActorOutput: actorOutput,
		Reviews:     lastReviews,
		Rounds:      maxRounds,
	}, nil
}

func (o *Orchestrator) buildActorPrompt(actor ActorRef, userInput, previousDiff string, lastReviews map[string]ReviewResult) string {
	prompt := expandTemplate(actor.Prompt, map[string]string{"UserInput": userInput})

	if lastReviews == nil {
		return prompt
	}

	return fmt.Sprintf(`You previously implemented the following and review found issues that need fixing.

User request:
%s

Previous changes (diff):
%s

Review feedback:
%s

Only fix the issues pointed out by reviewers. Do not rewrite the entire implementation.`,
		prompt, previousDiff, summarizeReviews(lastReviews))
}

func (o *Orchestrator) runReviewers(ctx context.Context, reviewers []ReviewerRef, input ReviewInput) (map[string]ReviewResult, error) {
	g, gctx := errgroup.WithContext(ctx)
	results := make([]ReviewResult, len(reviewers))

	for i, r := range reviewers {
		i, r := i, r
		g.Go(func() error {
			o.emit(OrchestratorEvent{Type: "reviewer_started", Name: r.ReviewerKey()})
			prompt := expandTemplate(r.Prompt, map[string]string{
				"diff":   input.Diff,
				"output": input.Output,
				"files":  input.Files,
			})

			task, err := o.pool.StartTask(gctx, string(r.AgentType), prompt, subagent.SubagentConfig{
				AgentType: string(r.AgentType),
			})
			if err != nil {
				return fmt.Errorf("reviewer %s start: %w", r.ReviewerKey(), err)
			}
			completed, err := o.pool.Wait(gctx, task.ID)
			if err != nil {
				return fmt.Errorf("reviewer %s wait: %w", r.ReviewerKey(), err)
			}
			if completed.Status == subagent.TaskStatusFailed || completed.Status == subagent.TaskStatusTimedOut {
				o.emit(OrchestratorEvent{Type: "reviewer_failed", Name: r.ReviewerKey(), Message: completed.Error})
				return fmt.Errorf("reviewer %s: %s", r.ReviewerKey(), completed.Error)
			}
			o.emit(OrchestratorEvent{Type: "reviewer_completed", Name: r.ReviewerKey()})

			// Extract JSON from agent output (handles markdown fences, preamble)
			jsonStr := extractJSON(completed.Result)
			if jsonStr == "" {
				return fmt.Errorf("reviewer %s: no JSON object found in output", r.ReviewerKey())
			}

			// Validate output schema if available
			reviewSchema := resolveOutputSchema(r.AgentType, o.workDir)
			if reviewSchema != nil && reviewSchema.Resolved != nil {
				var raw any
				if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
					return fmt.Errorf("reviewer %s: invalid JSON: %w", r.ReviewerKey(), err)
				}
				if err := reviewSchema.Resolved.Validate(raw); err != nil {
					return fmt.Errorf("reviewer %s: schema validation: %w", r.ReviewerKey(), err)
				}
			}

			if err := json.Unmarshal([]byte(jsonStr), &results[i]); err != nil {
				return fmt.Errorf("reviewer %s: unmarshal: %w", r.ReviewerKey(), err)
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	mapped := make(map[string]ReviewResult, len(results))
	for i, r := range reviewers {
		mapped[r.ReviewerKey()] = results[i]
	}
	return mapped, nil
}

func (o *Orchestrator) recordBaseline(workDir string) string {
	if workDir == "" {
		return ""
	}
	cmd := exec.Command("git", "stash", "create")
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return "HEAD"
	}
	return strings.TrimSpace(string(out))
}

func (o *Orchestrator) collectDiffSince(baseline, workDir string) string {
	if workDir == "" || baseline == "" {
		return ""
	}
	cmd := exec.Command("git", "diff", baseline)
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	// Truncate to avoid context overflow
	const maxDiffLen = 50000
	if len(out) > maxDiffLen {
		out = out[:maxDiffLen]
	}
	return string(out)
}

func (o *Orchestrator) collectChangedFiles(baseline, workDir string) string {
	if workDir == "" || baseline == "" {
		return ""
	}
	cmd := exec.Command("git", "diff", "--name-status", baseline)
	cmd.Dir = workDir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}
