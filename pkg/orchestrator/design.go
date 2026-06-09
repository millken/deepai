package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type Proposal struct {
	Agent   string
	Angle   string
	Content string
}

type DesignResult struct {
	Proposals []Proposal
	Plan      string
	Rationale string
	BestIndex int
	JudgeRaw  string
	Parsed    bool
}

type DesignConfig struct {
	Proposers   []string
	Judge       string
	Concurrency int
}

var proposalAngles = []string{
	"Favor the simplest approach that fully solves the task.",
	"Favor robustness and careful edge-case handling.",
	"Favor minimal change to the existing code.",
	"Favor performance and scalability.",
}

const (
	defaultJudge = "architect"
)

func normalizeDesignConfig(cfg DesignConfig) DesignConfig {
	if len(cfg.Proposers) == 0 {
		cfg.Proposers = []string{"architect", "coder"}
	}
	if strings.TrimSpace(cfg.Judge) == "" {
		cfg.Judge = defaultJudge
	}
	return cfg
}

func Design(ctx context.Context, cfg DesignConfig, taskPrompt string, runner SubagentRunner) (*DesignResult, error) {
	if runner == nil {
		return nil, fmt.Errorf("subagent runner is required")
	}
	if strings.TrimSpace(taskPrompt) == "" {
		return nil, fmt.Errorf("task prompt is required")
	}
	cfg = normalizeDesignConfig(cfg)

	proposals, err := fanOutProposals(ctx, runner, cfg.Proposers, taskPrompt, cfg.Concurrency)
	if err != nil {
		return nil, err
	}
	res := &DesignResult{Proposals: proposals}

	judgeOut, err := runner.Run(ctx, cfg.Judge, "selecting best approach", buildJudgePrompt(taskPrompt, proposals))
	if err != nil {
		return res, fmt.Errorf("judge %q failed: %w", cfg.Judge, err)
	}
	res.JudgeRaw = judgeOut

	plan, rationale, best, ok := parseJudgement(judgeOut)
	res.Parsed = ok
	res.BestIndex = best
	res.Rationale = rationale
	if ok && strings.TrimSpace(plan) != "" {
		res.Plan = plan
	} else {
		res.Plan = strings.TrimSpace(judgeOut)
	}
	return res, nil
}

func fanOutProposals(ctx context.Context, runner SubagentRunner, proposers []string, taskPrompt string, concurrency int) ([]Proposal, error) {
	proposals := make([]Proposal, len(proposers))
	type indexed struct {
		i   int
		err error
	}
	if concurrency <= 0 || concurrency > len(proposers) {
		concurrency = len(proposers)
	}
	if concurrency <= 0 {
		return nil, nil
	}
	results := make([]Proposal, len(proposers))
	errs := make([]error, len(proposers))
	sem := make(chan struct{}, concurrency)
	done := make(chan indexed, len(proposers))
	for i, role := range proposers {
		go func(i int, role string) {
			sem <- struct{}{}
			defer func() { <-sem }()
			angle := proposalAngles[i%len(proposalAngles)]
			out, err := runner.Run(ctx, role, fmt.Sprintf("proposing approach (%s)", angle), buildProposePrompt(taskPrompt, angle))
			results[i] = Proposal{Agent: role, Angle: angle, Content: out}
			done <- indexed{i: i, err: err}
		}(i, role)
	}
	for range proposers {
		d := <-done
		if d.err != nil {
			errs[d.i] = d.err
		}
	}
	for _, e := range errs {
		if e != nil {
			return nil, fmt.Errorf("proposer failed: %w", e)
		}
	}
	copy(proposals, results)
	return proposals, nil
}

func buildProposePrompt(taskPrompt, angle string) string {
	var b strings.Builder
	b.WriteString("Propose a concrete implementation approach for the task below. ")
	b.WriteString(angle)
	b.WriteString(" Be specific: list the files/components to change, the key steps, and the main risks. Do NOT write code or modify files — produce a plan only.\n\n## Task\n")
	b.WriteString(taskPrompt)
	return b.String()
}

func buildJudgePrompt(taskPrompt string, proposals []Proposal) string {
	var b strings.Builder
	b.WriteString("Several proposals for the task below are listed. Evaluate them critically, then produce ONE consolidated final plan that takes the best ideas and discards weak ones. ")
	b.WriteString(`Respond ONLY with JSON: {"best":<0-based index of the strongest single proposal>,"rationale":"why, and what you merged/rejected","plan":"the consolidated, actionable final plan"}.`)
	b.WriteString("\n\n## Task\n")
	b.WriteString(taskPrompt)
	for i, p := range proposals {
		b.WriteString(fmt.Sprintf("\n\n## Proposal %d (%s — angle: %s)\n", i, p.Agent, p.Angle))
		b.WriteString(truncate(p.Content, 6000))
	}
	return b.String()
}

func parseJudgement(raw string) (plan, rationale string, best int, ok bool) {
	obj := extractJSONObject(raw)
	if obj == "" {
		return "", "", 0, false
	}
	var parsed struct {
		Best      int    `json:"best"`
		Rationale string `json:"rationale"`
		Plan      string `json:"plan"`
	}
	if err := json.Unmarshal([]byte(obj), &parsed); err != nil {
		return "", "", 0, false
	}
	return parsed.Plan, parsed.Rationale, parsed.Best, true
}
