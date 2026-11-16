package commands

import (
	"fmt"
	"sort"
	"strings"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
)

// Phase 3 (RUN_CODE_DESIGN §9): the eval harness runs the same corpus as two
// arms — A (baseline) and B (run_code available to the dispatched subagent) —
// and compare judges the three default-on criteria. The B arm must NOT touch
// any production AgentTypeConfig: run_code reaches the subagent via
// task-level tool selectors (task.Config.Tools overrides DefaultTools),
// exactly how SubagentExecutor.Execute resolves tools.

// evalRunCodeInputTokenCut is the default-on token gate: input tokens must
// drop by at least 25% on the B arm (§9 判据「输入 token 下降 ≥ 25%」).
const evalRunCodeInputTokenCut = 0.25

// evalRunCodeArm resolves the arm gate with the same precedence as the CLI
// registration path (chat.go): env DEEPAI_RUN_CODE wins, else config
// run_code: true, else off. evalToolCandidates honors the same gate so the
// dispatch registry, the prompt and the fingerprint all move together.
func evalRunCodeArm(cfg Config) bool {
	return cfg.RunCode || runCodeEnabled()
}

// armToolSelectors returns the B arm's task-level tool selectors: the
// profile's DefaultTools plus run_code. nil when the arm is off, and when
// the profile declares no DefaultTools — overriding an empty list with
// ["run_code"] would restrict that subagent to ONLY run_code.
func armToolSelectors(agentType, repoRoot string, armOn bool) []string {
	if !armOn {
		return nil
	}
	profileCfg, problems, resolved := agent.ResolveAgentTypeConfig(agent.AgentType(agentType), repoRoot, nil)
	if !resolved || len(problems) > 0 || len(profileCfg.DefaultTools) == 0 {
		return nil
	}
	out := append([]string(nil), profileCfg.DefaultTools...)
	for _, n := range out {
		if n == "run_code" {
			return out
		}
	}
	return append(out, "run_code")
}

// appendRunCodeCriteria renders the three default-on gates (§9: 完成率不降、
// 输入 token 降 ≥ 25%、轮数不升) and reports whether any gate failed. Emitted
// only for a genuine A→B pair (before arm off, after arm on). Token/turn
// gates read "usage unavailable" rather than failing when the before side
// never recorded the figure (AvgInputTokens/AvgLLMTurns zero).
func appendRunCodeCriteria(b *strings.Builder, before, after roleSummary) bool {
	fmt.Fprintf(b, "  run_code default-on criteria (arm A -> arm B):\n")
	var failed bool

	status := "PASS"
	if after.CompletionRate < before.CompletionRate {
		status = "FAIL"
		failed = true
	}
	fmt.Fprintf(b, "  [%s] completion rate:      %.1f%% -> %.1f%%  (threshold >= %.1f%%)\n",
		status, before.CompletionRate*100, after.CompletionRate*100, before.CompletionRate*100)

	if before.AvgInputTokens > 0 {
		cut := (1 - after.AvgInputTokens/before.AvgInputTokens) * 100
		status = "PASS"
		if cut < evalRunCodeInputTokenCut*100 {
			status = "FAIL"
			failed = true
		}
		fmt.Fprintf(b, "  [%s] input tokens:          %.0f -> %.0f  (-%.1f%%, threshold >= %.0f%% cut)\n",
			status, before.AvgInputTokens, after.AvgInputTokens, cut, evalRunCodeInputTokenCut*100)
	} else {
		fmt.Fprintf(b, "  input tokens: usage unavailable on the before arm\n")
	}

	if before.AvgLLMTurns > 0 {
		status = "PASS"
		if after.AvgLLMTurns > before.AvgLLMTurns {
			status = "FAIL"
			failed = true
		}
		fmt.Fprintf(b, "  [%s] llm turns:             %.1f -> %.1f  (threshold <= %.1f)\n",
			status, before.AvgLLMTurns, after.AvgLLMTurns, before.AvgLLMTurns)
	} else {
		fmt.Fprintf(b, "  llm turns: usage unavailable on the before arm\n")
	}
	return failed
}

// fingerprintToolNames returns the sorted SELECTED tool names for a
// (profile, selector) pair — the tool-identity component caseFingerprint
// hashes alongside the system prompt. The prompt alone is byte-identical
// between the A/B arms (run_code triggers none of the gated prompt
// sections), so without this component a real A/B pair compares as
// "fingerprint unchanged".
func fingerprintToolNames(agentType, repoRoot string, evalTools []models.Tool, extraSelectors []string) []string {
	selectors := extraSelectors
	if selectors == nil {
		profileCfg, problems, resolved := agent.ResolveAgentTypeConfig(agent.AgentType(agentType), repoRoot, nil)
		if !resolved || len(problems) > 0 {
			return nil
		}
		selectors = profileCfg.DefaultTools
	}
	selected, err := agent.SelectSubagentTools(evalTools, selectors)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(selected))
	for _, t := range selected {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return names
}
