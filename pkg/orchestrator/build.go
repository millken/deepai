package orchestrator

import (
	"context"
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

	implCfg := cfg.Implement
	if design != nil {
		implCfg.Plan = design.Plan
	}
	impl, err := Run(ctx, implCfg, taskPrompt, runner, verifier, differ)
	res.Implement = impl
	return res, err
}
