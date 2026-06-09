package orchestrator

import (
	"context"
	"strings"
)

type BuildConfig struct {
	Design    DesignConfig
	Implement Config
}

type BuildResult struct {
	Design    *DesignResult
	Implement *Result
}

func Build(ctx context.Context, cfg BuildConfig, taskPrompt string, runner SubagentRunner, verifier Verifier, differ Differ) (*BuildResult, error) {
	res := &BuildResult{}

	design, err := Design(ctx, cfg.Design, taskPrompt, runner)
	res.Design = design
	if err != nil {
		return res, err
	}

	plan := ""
	if design != nil {
		plan = design.Plan
	}
	impl, err := Run(ctx, cfg.Implement, combineTaskAndPlan(taskPrompt, plan), runner, verifier, differ)
	res.Implement = impl
	return res, err
}

func combineTaskAndPlan(task, plan string) string {
	if strings.TrimSpace(plan) == "" {
		return task
	}
	return "Implement the task below by following the vetted plan. If the plan conflicts with the task, the task wins.\n\n## Task\n" +
		task + "\n\n## Vetted plan\n" + plan
}
