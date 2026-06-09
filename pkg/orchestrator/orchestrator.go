package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

type Issue struct {
	Severity   string `json:"severity"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion"`
}

type Verdict struct {
	Pass    bool
	Summary string
	Issues  []Issue
	Parsed  bool
	Raw     string
}

type SubagentRunner interface {
	Run(ctx context.Context, agentType, description, prompt string) (string, error)
}

type VerifyResult struct {
	Ran    bool
	Passed bool
	Output string
}

type Verifier interface {
	Verify(ctx context.Context) (VerifyResult, error)
}

type Differ interface {
	Diff(ctx context.Context) (string, error)
}

type Config struct {
	MaxRounds           int
	CoderType           string
	ReviewerType        string
	Reviewers           []string
	MajorityReview      bool
	ReviewConcurrency   int
	MaxAgentCalls       int
	RequireVerification bool
	Plan                string
	Progress            func(string)
}

type RoundResult struct {
	Round        int
	ImplSummary  string
	VerifyRan    bool
	VerifyPassed bool
	VerifyOutput string
	Diff         string
	Verdict      Verdict
	Reviews      []Verdict
}

type Result struct {
	Done       bool
	Verified   bool
	Reason     string
	Rounds     []RoundResult
	AgentCalls int
}

const (
	defaultMaxRounds    = 4
	defaultCoderType    = "coder"
	defaultReviewerType = "arch-reviewer"
	maxFeedbackBytes    = 6000
)

func normalizeConfig(cfg Config) Config {
	if cfg.MaxRounds <= 0 {
		cfg.MaxRounds = defaultMaxRounds
	}
	if strings.TrimSpace(cfg.CoderType) == "" {
		cfg.CoderType = defaultCoderType
	}
	if strings.TrimSpace(cfg.ReviewerType) == "" {
		cfg.ReviewerType = defaultReviewerType
	}
	if len(cfg.Reviewers) == 0 {
		cfg.Reviewers = []string{cfg.ReviewerType}
	}
	return cfg
}

func Run(ctx context.Context, cfg Config, taskPrompt string, runner SubagentRunner, verifier Verifier, differ Differ) (*Result, error) {
	if runner == nil {
		return nil, fmt.Errorf("subagent runner is required")
	}
	if strings.TrimSpace(taskPrompt) == "" {
		return nil, fmt.Errorf("task prompt is required")
	}
	cfg = normalizeConfig(cfg)

	res := &Result{}
	feedback := ""
	board := &Blackboard{Plan: cfg.Plan}
	perRoundCalls := 1 + len(cfg.Reviewers)

	for round := 1; round <= cfg.MaxRounds; round++ {
		if err := ctx.Err(); err != nil {
			res.Reason = "cancelled"
			return res, err
		}

		if cfg.MaxAgentCalls > 0 && res.AgentCalls+perRoundCalls > cfg.MaxAgentCalls {
			res.Reason = fmt.Sprintf("agent-call budget (%d) would be exceeded by round %d; stopping", cfg.MaxAgentCalls, round)
			return res, nil
		}

		coderPrompt := taskPrompt
		if shared := board.Render(); shared != "" {
			coderPrompt += "\n\n" + shared
		}
		if feedback != "" {
			coderPrompt += "\n\nThe previous attempt did not pass. Address this feedback, then stop:\n" + feedback
		}
		res.AgentCalls++
		coderLabel := fmt.Sprintf("implementing · round %d/%d", round, cfg.MaxRounds)
		if feedback != "" {
			coderLabel = fmt.Sprintf("fixing review feedback · round %d/%d", round, cfg.MaxRounds)
		}
		implOut, err := runner.Run(ctx, cfg.CoderType, coderLabel, coderPrompt)
		if err != nil {
			res.Reason = fmt.Sprintf("coder failed in round %d: %v", round, err)
			return res, err
		}

		rr := RoundResult{Round: round, ImplSummary: implOut}

		var vr VerifyResult
		if verifier != nil {
			v, verr := verifier.Verify(ctx)
			if verr != nil {
				res.Reason = fmt.Sprintf("verify errored in round %d: %v", round, verr)
				res.Rounds = append(res.Rounds, rr)
				return res, verr
			}
			vr = v
		}
		rr.VerifyRan = vr.Ran
		rr.VerifyPassed = vr.Passed
		rr.VerifyOutput = vr.Output
		if vr.Ran {
			if vr.Passed {
				progress(cfg, fmt.Sprintf("round %d: verify passed", round))
			} else {
				progress(cfg, fmt.Sprintf("round %d: verify FAILED", round))
			}
		}

		diffKnown := differ != nil
		diff := ""
		if diffKnown {
			diff, _ = differ.Diff(ctx)
			rr.Diff = diff
		}
		hasDiff := !diffKnown || strings.TrimSpace(diff) != ""

		if diffKnown && !hasDiff {
			rr.Verdict = Verdict{Parsed: true, Pass: false, Summary: "no file changes were produced"}
			res.Rounds = append(res.Rounds, rr)
			feedback = "No file changes were detected. You must actually modify files to implement the task, then stop."
			continue
		}

		reviewPrompt := buildReviewPrompt(taskPrompt, diff, vr.Output, vr.Ran, board.Render())
		res.AgentCalls += len(cfg.Reviewers)
		verdicts, rerr := fanOutReviews(ctx, runner, cfg.Reviewers, reviewPrompt, cfg.ReviewConcurrency, round)
		if rerr != nil {
			res.Reason = fmt.Sprintf("reviewer failed in round %d: %v", round, rerr)
			res.Rounds = append(res.Rounds, rr)
			return res, rerr
		}
		rr.Reviews = verdicts
		rr.Verdict = aggregateVerdicts(verdicts, cfg.Reviewers, cfg.MajorityReview)
		res.Rounds = append(res.Rounds, rr)
		if rr.Verdict.Pass {
			progress(cfg, fmt.Sprintf("round %d: review passed", round))
		} else {
			progress(cfg, fmt.Sprintf("round %d: review FAILED — %s", round, truncate(rr.Verdict.Summary, 120)))
		}

		objectiveOK := vr.Passed || (!vr.Ran && !cfg.RequireVerification)
		if rr.Verdict.Pass && hasDiff && objectiveOK {
			res.Done = true
			res.Verified = vr.Ran && vr.Passed
			if res.Verified {
				res.Reason = fmt.Sprintf("passed verification and review in round %d", round)
			} else {
				res.Reason = fmt.Sprintf("review passed in round %d, but no objective verification ran (no verify_command) — result is UNVERIFIED", round)
			}
			return res, nil
		}

		feedback = buildFeedback(rr)
		if summary := strings.TrimSpace(rr.Verdict.Summary); summary != "" {
			board.AddNote(fmt.Sprintf("Round %d review: %s", round, truncate(summary, 400)))
		}
	}

	if cfg.RequireVerification {
		res.Reason = fmt.Sprintf("reached max rounds (%d) without a passing verification + review", cfg.MaxRounds)
	} else {
		res.Reason = fmt.Sprintf("reached max rounds (%d) without passing", cfg.MaxRounds)
	}
	return res, nil
}

func progress(cfg Config, msg string) {
	if cfg.Progress != nil {
		cfg.Progress(msg)
	}
}

func buildReviewPrompt(taskPrompt, diff, verifyOutput string, verifyRan bool, shared string) string {
	var b strings.Builder
	b.WriteString("You are an adversarial code reviewer. Your job is to FIND PROBLEMS, not to approve. ")
	b.WriteString("Default to verdict \"fail\" unless you are confident the change fully and correctly satisfies the task with no blocking issues.\n\n")
	b.WriteString("Respond ONLY with a JSON object: ")
	b.WriteString(`{"verdict":"pass"|"fail","summary":"...","issues":[{"severity":"...","file":"...","line":0,"message":"...","suggestion":"..."}]}.`)
	b.WriteString("\nRules:\n")
	b.WriteString("- Judge ONLY the diff below against the task. Do not assume code you cannot see is correct.\n")
	b.WriteString("- Every issue MUST cite a concrete file and line drawn from the diff; issues without a file:line will be ignored.\n")
	b.WriteString("- If the diff does not actually address the task, verdict MUST be \"fail\".\n")
	if verifyRan {
		b.WriteString("- The verification command was executed; its output is below. A passing build/test is necessary but NOT sufficient — also judge correctness and completeness.\n")
	} else {
		b.WriteString("- No automated verification was run, so you cannot rely on tests passing — be more skeptical about correctness.\n")
	}
	b.WriteString("- Output \"pass\" only if you would stake your reputation on this change being correct and complete.\n")
	if strings.TrimSpace(shared) != "" {
		b.WriteString("- Judge the change against the approved plan below; do NOT flag intentional decisions listed there as defects.\n")
	}
	b.WriteString("\n## Task\n")
	b.WriteString(taskPrompt)
	if strings.TrimSpace(shared) != "" {
		b.WriteString("\n\n")
		b.WriteString(shared)
	}
	if strings.TrimSpace(verifyOutput) != "" {
		b.WriteString("\n\n## Verification output\n")
		b.WriteString(truncate(verifyOutput, 4000))
	}
	b.WriteString("\n\n## Diff\n")
	b.WriteString(truncate(diff, 12000))
	return b.String()
}

func buildFeedback(rr RoundResult) string {
	var b strings.Builder
	if rr.VerifyRan && !rr.VerifyPassed {
		b.WriteString("Verification failed:\n")
		b.WriteString(truncate(rr.VerifyOutput, 3000))
		b.WriteString("\n\n")
	}
	if rr.Verdict.Summary != "" {
		b.WriteString("Reviewer summary: ")
		b.WriteString(rr.Verdict.Summary)
		b.WriteString("\n")
	}
	for _, iss := range rr.Verdict.Issues {
		loc := iss.File
		if iss.Line > 0 {
			loc = fmt.Sprintf("%s:%d", iss.File, iss.Line)
		}
		b.WriteString(fmt.Sprintf("- [%s] %s — %s", iss.Severity, loc, iss.Message))
		if iss.Suggestion != "" {
			b.WriteString(" (suggestion: " + iss.Suggestion + ")")
		}
		b.WriteString("\n")
	}
	if !rr.Verdict.Parsed && b.Len() == 0 {
		b.WriteString("Reviewer did not return a parseable verdict; ensure the change is correct and complete:\n")
		b.WriteString(truncate(rr.Verdict.Raw, 2000))
	}
	return truncate(b.String(), maxFeedbackBytes)
}

func fanOutReviews(ctx context.Context, runner SubagentRunner, reviewers []string, prompt string, concurrency, round int) ([]Verdict, error) {
	if concurrency <= 0 || concurrency > len(reviewers) {
		concurrency = len(reviewers)
	}
	if concurrency <= 0 {
		return nil, nil
	}
	verdicts := make([]Verdict, len(reviewers))
	errs := make([]error, len(reviewers))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, reviewerType := range reviewers {
		wg.Add(1)
		go func(i int, reviewerType string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out, err := runner.Run(ctx, reviewerType, fmt.Sprintf("reviewing (%s) · round %d", reviewerType, round), prompt)
			if err != nil {
				errs[i] = fmt.Errorf("reviewer %q: %w", reviewerType, err)
				return
			}
			verdicts[i] = parseVerdict(out)
		}(i, reviewerType)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return nil, e
		}
	}
	return verdicts, nil
}

func aggregateVerdicts(verdicts []Verdict, reviewers []string, majority bool) Verdict {
	agg := Verdict{}
	passCount := 0
	var summaries []string
	for i, v := range verdicts {
		if v.Parsed {
			agg.Parsed = true
		}
		if v.Pass {
			passCount++
		}
		agg.Issues = append(agg.Issues, v.Issues...)
		if strings.TrimSpace(v.Summary) != "" {
			label := "reviewer"
			if i < len(reviewers) {
				label = reviewers[i]
			}
			summaries = append(summaries, label+": "+v.Summary)
		}
	}
	agg.Summary = strings.Join(summaries, " | ")
	if len(verdicts) == 0 {
		return agg
	}
	if majority {
		agg.Pass = passCount*2 > len(verdicts)
	} else {
		agg.Pass = passCount == len(verdicts)
	}
	return agg
}

func parseVerdict(raw string) Verdict {
	v := Verdict{Raw: raw}
	obj := extractJSONObject(raw)
	if obj == "" {
		return v
	}
	var parsed struct {
		Verdict string  `json:"verdict"`
		Pass    *bool   `json:"pass"`
		Summary string  `json:"summary"`
		Issues  []Issue `json:"issues"`
	}
	if err := json.Unmarshal([]byte(obj), &parsed); err != nil {
		return v
	}
	v.Parsed = true
	v.Summary = parsed.Summary
	v.Issues = parsed.Issues
	switch {
	case parsed.Pass != nil:
		v.Pass = *parsed.Pass
	default:
		v.Pass = strings.EqualFold(strings.TrimSpace(parsed.Verdict), "pass")
	}
	return v
}

func extractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n... [truncated %d bytes]", len(s)-max)
}
