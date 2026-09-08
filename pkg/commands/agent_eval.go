// Package commands: deepai eval agents / deepai eval compare (M5-2).
//
// This harness reuses REVIEW_EVAL_DESIGN.md's P1 skeleton (temp-repo
// materialization, chdir-then-dispatch, worktree snapshot, fingerprint,
// runs.jsonl/summary.{md,json}) for a different judged surface: instead of
// scoring a reviewer's verdict against planted bugs, it scores one of five
// non-reviewer agent types (architect, product-manager, researcher, analyst,
// tester) against deterministic, schema/anchor-based assertions — see
// docs/AGENT_CAPABILITY_DESIGN.md §5.
//
// Zero production BEHAVIOR changes ship with this file. Production code
// does gain a small set of additive exports this file depends on: the
// pkg/chat/review.go WorktreeSnapshot/TakeWorktreeSnapshot/ChangedSince (so
// the `no_writes` assertion below reuses the exact dirty-worktree detection
// the review gate uses, instead of a second copy that could silently
// diverge from it), and — M6 latency, fingerprint-coverage fix —
// pkg/agent's AssembleSystemPrompt/SelectSubagentTools/
// PreloadSkillsProfile/AppendOutputSchemaPrompt/ResolveAgentTypeConfig, so
// caseFingerprint (below) can compute the SAME bytes a real dispatched
// subagent's BuildSystemPrompt() produces, by calling the exact functions
// that produce them, rather than re-deriving that assembly independently.
// Every one of these exports is a pure refactor of existing logic into a
// reusable shape — see each export's doc comment in pkg/agent for the
// before/after proof — not new behavior.
package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/chat"
	"github.com/millken/deepai/pkg/clarification"
	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/skill"
	"github.com/millken/deepai/pkg/subagent"
	"github.com/millken/deepai/pkg/tools"
	"github.com/millken/deepai/pkg/tools/builtin"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// CLI wiring
// ---------------------------------------------------------------------------

var agentEvalFlags struct {
	Cases   string
	Runs    int
	Filter  string
	Out     string
	Model   string
	Budget  int
	Timeout string
}

// addEval registers the `eval` parent command and its `agents`/`compare`
// children, following the topLevel.AddCommand convention (pkg/commands/commands.go).
//
// `eval review` (REVIEW_EVAL_DESIGN.md's P1 harness for correctness-reviewer)
// is deliberately NOT registered here: it was never implemented, and this
// period is scoped to the agent-role baseline only — see the M5-2 brief's
// explicit "leave the position, don't implement it" instruction.
func addEval(topLevel *cobra.Command) {
	evalCmd := &cobra.Command{
		Use:    "eval",
		Short:  "Offline evaluation harnesses (agent role baselines)",
		Hidden: true,
	}

	agentsCmd := &cobra.Command{
		Use:   "agents",
		Short: "Run the deepai eval agents role baseline harness against eval/agent-cases",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEvalAgentsCmd(cmd.Context(), cmd.OutOrStdout())
		},
	}
	agentsCmd.Flags().StringVar(&agentEvalFlags.Cases, "cases", "eval/agent-cases", "Case corpus directory")
	agentsCmd.Flags().IntVar(&agentEvalFlags.Runs, "runs", 3, "Repetitions per case")
	agentsCmd.Flags().StringVar(&agentEvalFlags.Filter, "filter", "", "Only run cases matching this agent_type or tag")
	agentsCmd.Flags().StringVar(&agentEvalFlags.Out, "out", "eval/results", "Output directory root")
	agentsCmd.Flags().StringVar(&agentEvalFlags.Model, "model", "", "Model alias override (default: config default)")
	agentsCmd.Flags().IntVar(&agentEvalFlags.Budget, "budget", 0, "Per-run token budget (0 = unlimited, matches task tool default)")
	agentsCmd.Flags().StringVar(&agentEvalFlags.Timeout, "timeout", "5m", "Per-run wall-clock timeout (e.g. 5m); 0 or empty = unlimited")
	evalCmd.AddCommand(agentsCmd)

	compareCmd := &cobra.Command{
		Use:   "compare <before.json> <after.json>",
		Short: "Diff two `deepai eval agents` summary.json baselines",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEvalCompareCmd(cmd.OutOrStdout(), args[0], args[1])
		},
	}
	evalCmd.AddCommand(compareCmd)

	// `eval summarize` lives in agent_eval_summarize.go — a separate file so
	// it can be read/reviewed/reverted independently of the `agents`/
	// `compare` wiring above, which it does not modify.
	addEvalSummarize(evalCmd)

	topLevel.AddCommand(evalCmd)
}

func runEvalAgentsCmd(ctx context.Context, out io.Writer) error {
	timeout, err := parseEvalTimeout(agentEvalFlags.Timeout)
	if err != nil {
		return err
	}
	opts := evalOptions{
		CasesDir: agentEvalFlags.Cases,
		Runs:     agentEvalFlags.Runs,
		Filter:   agentEvalFlags.Filter,
		OutDir:   agentEvalFlags.Out,
		Budget:   agentEvalFlags.Budget,
		Timeout:  timeout,
	}
	if opts.Runs <= 0 {
		opts.Runs = 1
	}

	repoRoot, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve repo root: %w", err)
	}
	cfg, err := LoadConfig(ConfigFile())
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	modelRegistry, modelAlias, err := buildEvalModelRegistry(cfg, agentEvalFlags.Model)
	if err != nil {
		return err
	}

	pool, skillReg, evalTools, _, err := buildEvalStack(modelRegistry, repoRoot, cfg)
	if err != nil {
		return err
	}

	casesDir := opts.CasesDir
	if !filepath.IsAbs(casesDir) {
		casesDir = filepath.Join(repoRoot, casesDir)
	}
	cases, err := loadEvalCases(casesDir, opts.Filter)
	if err != nil {
		return err
	}
	if len(cases) == 0 {
		return fmt.Errorf("no cases found under %s (filter=%q)", casesDir, opts.Filter)
	}

	outDir := opts.OutDir
	if !filepath.IsAbs(outDir) {
		outDir = filepath.Join(repoRoot, outDir)
	}
	resultDir, err := prepareEvalResultDir(outDir, modelAlias)
	if err != nil {
		return err
	}
	runsPath := filepath.Join(resultDir, "runs.jsonl")
	writer, err := newRunWriter(runsPath)
	if err != nil {
		return err
	}
	defer writer.Close()

	fmt.Fprintf(out, "deepai eval agents: %d case(s), %d run(s) each, model=%s\n", len(cases), opts.Runs, modelAlias)
	fmt.Fprintf(out, "writing incrementally to %s\n", runsPath)
	records, runErr := runEvalCases(ctx, pool, repoRoot, evalTools, skillReg, cases, opts, func(rec runRecord) error {
		if err := writer.Write(rec); err != nil {
			return err
		}
		fmt.Fprintf(out, "  %-24s run %d/%d  tokens=%-6d duration=%-8s writes=%v\n",
			rec.AgentType+"/"+rec.Case, rec.Run, opts.Runs, rec.Tokens, time.Duration(rec.DurationMS)*time.Millisecond, rec.WriteViolation)
		return nil
	})
	if runErr != nil {
		return fmt.Errorf("eval agents: %w (partial results preserved in %s)", runErr, runsPath)
	}

	if err := writeEvalSummary(resultDir, modelAlias, opts.Runs, agentEvalFlags.Timeout, records); err != nil {
		return err
	}
	fmt.Fprintf(out, "results written to %s\n", resultDir)
	return nil
}

func parseEvalTimeout(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid --timeout %q: %w", s, err)
	}
	return d, nil
}

// buildEvalModelRegistry mirrors runChat's model-registry assembly
// (pkg/commands/chat.go) closely enough to resolve the same default model,
// but does not touch chat.go itself (out of bounds this period) — it calls
// chat.go's already-exported-within-package resolveDefaultAlias instead of
// duplicating its precedence rules.
func buildEvalModelRegistry(cfg Config, modelOverride string) (*llm.ModelRegistry, string, error) {
	if cfg.Provider == "" && len(cfg.Models) == 0 {
		return nil, "", fmt.Errorf("no provider configured; run `deepai setup` first")
	}
	if len(cfg.Models) > 0 {
		defaultName := resolveDefaultAlias(cfg.Models, modelOverride, cfg.Model)
		reg, err := llm.NewModelRegistry(cfg.Models, defaultName)
		if err != nil {
			return nil, "", fmt.Errorf("build model registry: %w", err)
		}
		return reg, reg.DefaultName(), nil
	}
	modelName := cfg.Model
	if modelOverride != "" {
		modelName = modelOverride
	}
	if modelName == "" {
		modelName = "default"
	}
	reg := llm.NewSingleModelRegistry(cfg.Provider, modelName, cfg.BaseURL)
	return reg, reg.DefaultName(), nil
}

// evalTaskPool is the minimal subagent-dispatch surface the harness depends
// on — the same two methods tools.TaskTool wraps (pkg/tools/subagent.go).
// Defined locally so *subagent.Pool (production) and a fake (tests) are both
// accepted without needing a new exported interface anywhere else.
type evalTaskPool interface {
	StartTask(ctx context.Context, description, prompt string, cfg subagent.SubagentConfig) (*subagent.Task, error)
	Wait(ctx context.Context, taskID string) (*subagent.Task, error)
}

// buildEvalStack assembles the subagent dispatch stack for `deepai eval
// agents`, shaped like registerChatTools (pkg/commands/chat.go) — same tool
// registry composition, same SubagentExecutor/Pool/TaskTool wiring, same
// WithSkillRegistry — but built independently here (not by calling
// registerChatTools) per the M5-2 brief: chat.go is one of M5-1's files and
// must not be touched or have its assembly refactored out from under it.
//
// The eval registry only registers what the five tested profiles'
// DefaultTools can reach (pkg/agent/types_config.go): read_file, write_file,
// edit_file, list_dir, glob, grep, find, code_map, ask_clarification, bash
// (tester only), plus task for recursion-safety parity with production. No
// web_* or git_auto_commit tools — none of the five tested profiles list
// them, and REVIEW_EVAL's established convention keeps write-capable network
// tools out of an eval registry on principle.
//
// M6 review: returns the *tools.Registry it actually wires into the
// dispatch pool (via subExecutor/TaskTool below), NOT just the evalTools
// slice, so a test can assert the two never silently diverge. Before this,
// the equivalence test's "production-side" registry was rebuilt FROM
// evalToolCandidates a second time (see TestCaseFingerprint_
// EquivalentToRealDispatchedSubagentSystemPrompt in agent_eval_test.go),
// which only proves "given the same candidate list, both sides hash the
// same bytes" — it can never catch buildEvalStack registering a tool
// evalToolCandidates doesn't know about (or vice versa), because that test
// never looks at what buildEvalStack itself actually registered. Returning
// the real registry closes that gap:
// TestBuildEvalStack_RegistryMatchesEvalToolCandidatesPlusTask below
// compares registry.List()'s name set directly against
// evalToolCandidates(cfg) ∪ {"task"}.
func buildEvalStack(modelRegistry *llm.ModelRegistry, repoRoot string, cfg Config) (evalTaskPool, *skill.Registry, []models.Tool, *tools.Registry, error) {
	registry := newEvalToolRegistry(cfg)
	evalTools := evalToolCandidates(cfg)

	skillReg := skill.NewRegistry()
	// Best-effort: a skill load problem must not block the harness (none of
	// the five tested profiles preload a skill today), but is worth a stderr
	// note for whoever runs this.
	if warnings := skillReg.LoadAllReported(repoRoot, nil); len(warnings) > 0 {
		for _, w := range warnings {
			fmt.Fprintf(os.Stderr, "eval agents: skill load issue: %s %s: %s\n", w.Source, w.Dir, w.Msg)
		}
	}

	agentCatalog := agent.EnumerateAgents(repoRoot, nil)
	agentOpts := make([]tools.AgentOption, 0, len(agentCatalog))
	for _, a := range agentCatalog {
		agentOpts = append(agentOpts, tools.AgentOption{Type: string(a.Type), Description: a.Description})
	}

	contextWindow := cfg.ContextWindow
	if contextWindow <= 0 {
		contextWindow = 192000
	}

	subExecutor := agent.NewSubagentExecutor(modelRegistry, registry, nil).
		WithWorkDir(repoRoot).
		WithContextWindow(contextWindow).
		WithMaxTokens(subagentMaxTokens()).
		WithTemperature(cfg.Temperature).
		WithSkillRegistry(skillReg)
	pool := agent.NewSubagentPool(subExecutor, 0)
	mustRegisterTool(registry, tools.TaskTool(pool, agentOpts))

	return pool, skillReg, evalTools, registry, nil
}

// newEvalToolRegistry builds a fresh *tools.Registry containing exactly
// evalToolCandidates(cfg) — nothing more, nothing less, and (deliberately)
// no "task" tool, since task can only be registered once its pool exists
// (see buildEvalStack). Factored out of buildEvalStack so there is exactly
// ONE place that turns evalToolCandidates into a registry: buildEvalStack
// calls it to build its real dispatch registry, and
// TestBuildEvalStack_RegistryMatchesEvalToolCandidatesPlusTask calls it (via
// buildEvalStack's returned registry) to assert the two can never silently
// diverge — see buildEvalStack's doc comment for the defect this closes.
func newEvalToolRegistry(cfg Config) *tools.Registry {
	registry := tools.NewRegistry()
	for _, t := range evalToolCandidates(cfg) {
		mustRegisterTool(registry, t)
	}
	return registry
}

// evalToolCandidates returns the exact tool set buildEvalStack registers
// (minus the task tool, which is added separately once the pool exists,
// and which SelectSubagentTools always strips from its candidate list
// regardless of whether it's present — see its own doc comment): bash,
// ask_clarification, and the eight builtin.FileTools(). Shared between
// buildEvalStack (which actually registers these into the dispatch
// registry) and caseFingerprint (which needs the SAME candidate list to
// compute the restricted tool set a real dispatched subagent would get —
// see resolveEvalSubagentPrompt) so the two can never silently diverge on
// what a case's agent_type has to select from.
func evalToolCandidates(cfg Config) []models.Tool {
	out := []models.Tool{builtin.BashTool(), clarification.AskClarificationToolWithMode(cfg.IsAutonomous())}
	return append(out, builtin.FileTools()...)
}

// ---------------------------------------------------------------------------
// Manifest / case corpus
// ---------------------------------------------------------------------------

type caseManifest struct {
	ID           string           `yaml:"id"`
	AgentType    string           `yaml:"agent_type"`
	Task         string           `yaml:"task"`
	ContextFiles []string         `yaml:"context_files"`
	Tags         []string         `yaml:"tags"`
	Expect       []map[string]any `yaml:"expect"`
}

type evalCase struct {
	AgentType  string
	ID         string
	Dir        string
	FixtureDir string
	Manifest   caseManifest
}

// loadEvalCases walks <casesDir>/<agent_type>/<case-id>/manifest.yaml,
// returning cases in a stable (agent_type, id) order so runs.jsonl ordering
// is reproducible across invocations. filter, when non-empty, keeps only
// cases whose agent_type or one of whose tags equals it.
func loadEvalCases(casesDir, filter string) ([]evalCase, error) {
	agentTypeDirs, err := os.ReadDir(casesDir)
	if err != nil {
		return nil, fmt.Errorf("read cases dir %s: %w", casesDir, err)
	}
	var out []evalCase
	for _, at := range agentTypeDirs {
		if !at.IsDir() {
			continue
		}
		agentTypeDir := filepath.Join(casesDir, at.Name())
		caseDirs, err := os.ReadDir(agentTypeDir)
		if err != nil {
			return nil, fmt.Errorf("read agent type dir %s: %w", agentTypeDir, err)
		}
		for _, cd := range caseDirs {
			if !cd.IsDir() {
				continue
			}
			dir := filepath.Join(agentTypeDir, cd.Name())
			manifestPath := filepath.Join(dir, "manifest.yaml")
			data, err := os.ReadFile(manifestPath)
			if err != nil {
				return nil, fmt.Errorf("read manifest %s: %w", manifestPath, err)
			}
			var m caseManifest
			if err := yaml.Unmarshal(data, &m); err != nil {
				return nil, fmt.Errorf("parse manifest %s: %w", manifestPath, err)
			}
			if m.AgentType == "" {
				m.AgentType = at.Name()
			}
			if m.ID == "" {
				m.ID = cd.Name()
			}
			if m.Task == "" {
				return nil, fmt.Errorf("manifest %s: task is required", manifestPath)
			}
			if len(m.Expect) == 0 {
				return nil, fmt.Errorf("manifest %s: expect must have at least one assertion", manifestPath)
			}
			if err := validateManifestExpect(m, manifestPath); err != nil {
				return nil, err
			}
			if !matchesEvalFilter(m, filter) {
				continue
			}
			out = append(out, evalCase{
				AgentType:  m.AgentType,
				ID:         m.ID,
				Dir:        dir,
				FixtureDir: filepath.Join(dir, "fixture"),
				Manifest:   m,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AgentType != out[j].AgentType {
			return out[i].AgentType < out[j].AgentType
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func matchesEvalFilter(m caseManifest, filter string) bool {
	if filter == "" {
		return true
	}
	if m.AgentType == filter {
		return true
	}
	for _, t := range m.Tags {
		if t == filter {
			return true
		}
	}
	return false
}

// knownExpectKeys is every expect key evaluateCase's switch actually
// understands. validateManifestExpect uses it to reject a manifest that
// misspells one (e.g. file_content, files_change) at load time — before
// materialization, before a real model is ever dispatched against it —
// rather than letting the misspelled key fall through evaluateCase's old
// key-less switch as zero assertions and no warning.
var knownExpectKeys = map[string]bool{
	"mentions":          true,
	"not_mentions":      true,
	"tool_calls_max":    true,
	"no_writes":         true,
	"tokens_max":        true,
	"files_changed":     true,
	"file_contains":     true,
	"file_not_contains": true,
}

// validateManifestExpect rejects, at load time, the manifest shapes that
// would otherwise reach evaluateCase as a silent no-op or an always-true
// assertion — see this file's package doc comment on the M6 "no assertion
// can be un-failable" requirement:
//
//   - an expect entry with anything other than exactly one key (0: nothing
//     to check; >1: singleKV would silently pick one and drop the rest —
//     Go map iteration order is undefined, so which one is not even stable)
//   - an unrecognized key (a manifest typo)
//   - mentions/not_mentions/files_changed whose value isn't a list of
//     non-blank strings
//   - tool_calls_max/tokens_max whose value isn't an integer
//   - no_writes whose value isn't a bool
//   - file_contains/file_not_contains whose value isn't a list of
//     {path, text} mappings, or whose path/text is blank (a blank text
//     makes strings.Contains(x, "") — an assertion that can never fail)
func validateManifestExpect(m caseManifest, manifestPath string) error {
	for i, exp := range m.Expect {
		if len(exp) != 1 {
			return fmt.Errorf("manifest %s: expect[%d]: must have exactly one key, got %d", manifestPath, i, len(exp))
		}
		key, val, ok := singleKV(exp)
		if !ok {
			return fmt.Errorf("manifest %s: expect[%d]: must have exactly one key", manifestPath, i)
		}
		if !knownExpectKeys[key] {
			return fmt.Errorf("manifest %s: expect[%d]: unrecognized expect key %q", manifestPath, i, key)
		}
		switch key {
		case "mentions", "not_mentions", "files_changed":
			items, ok := val.([]any)
			if !ok {
				return fmt.Errorf("manifest %s: expect[%d]: %q must be a list of strings", manifestPath, i, key)
			}
			for j, it := range items {
				s, ok := it.(string)
				if !ok || strings.TrimSpace(s) == "" {
					return fmt.Errorf("manifest %s: expect[%d].%s[%d]: must be a non-empty string", manifestPath, i, key, j)
				}
			}
		case "tool_calls_max", "tokens_max":
			switch val.(type) {
			case int, int64, float64:
			default:
				return fmt.Errorf("manifest %s: expect[%d]: %q must be an integer", manifestPath, i, key)
			}
		case "no_writes":
			if _, ok := val.(bool); !ok {
				return fmt.Errorf("manifest %s: expect[%d]: no_writes must be a bool", manifestPath, i)
			}
		case "file_contains", "file_not_contains":
			items, ok := val.([]any)
			if !ok {
				return fmt.Errorf("manifest %s: expect[%d]: %q must be a list of {path, text} entries, got %T", manifestPath, i, key, val)
			}
			for j, it := range items {
				fm, ok := it.(map[string]any)
				if !ok {
					return fmt.Errorf("manifest %s: expect[%d].%s[%d]: must be a {path, text} mapping, got %T", manifestPath, i, key, j, it)
				}
				path, _ := fm["path"].(string)
				text, _ := fm["text"].(string)
				if strings.TrimSpace(path) == "" {
					return fmt.Errorf("manifest %s: expect[%d].%s[%d]: path must not be empty", manifestPath, i, key, j)
				}
				if text == "" {
					return fmt.Errorf("manifest %s: expect[%d].%s[%d] (path=%s): text must not be empty", manifestPath, i, key, j, path)
				}
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Fixture materialization (ground truth must never be reachable)
// ---------------------------------------------------------------------------

// materializeFixture copies fixtureDir's contents into a fresh temp git
// repository OUTSIDE the deepai repo (os.MkdirTemp's default parent, never
// fixtureDir's own tree) and commits them as a baseline, so
// chat.TakeWorktreeSnapshot (which requires a git worktree) has something to
// diff against. fixtureDir need not exist (an empty-fixture case is valid).
//
// manifest.yaml is never copied: only fixtureDir's own contents are copied,
// and manifest.yaml always lives one directory up (case dir root), never
// inside fixture/. assertNoManifestLeak double-checks this at the caller.
func materializeFixture(fixtureDir string) (worktree string, cleanup func(), err error) {
	worktree, err = os.MkdirTemp("", "deepai-agent-eval-*")
	if err != nil {
		return "", nil, fmt.Errorf("mkdir temp worktree: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(worktree) }

	if info, statErr := os.Stat(fixtureDir); statErr == nil && info.IsDir() {
		if err := copyFixtureTree(fixtureDir, worktree); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("copy fixture tree: %w", err)
		}
	} else if statErr != nil && !os.IsNotExist(statErr) {
		cleanup()
		return "", nil, fmt.Errorf("stat fixture dir %s: %w", fixtureDir, statErr)
	}

	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=eval@deepai.local", "-c", "user.name=deepai-eval", "add", "-A"},
		// --allow-empty: an empty fixture is a valid case (e.g. a
		// from-scratch design task) and must still get a committed HEAD so
		// `git status` has a baseline to diff against.
		{"-c", "user.email=eval@deepai.local", "-c", "user.name=deepai-eval", "commit", "-q", "--allow-empty", "-m", "eval fixture baseline"},
	} {
		if err := runEvalGit(worktree, args...); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	return worktree, cleanup, nil
}

func runEvalGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func copyFixtureTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			if rel == "." {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("fixture entry %s is not a regular file or directory (mode %s)", path, d.Type())
		}
		return copyFixtureFile(path, target)
	})
}

func copyFixtureFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, data, info.Mode().Perm())
}

// assertNoManifestLeak is the explicit, load-bearing guard the M5-2 brief
// requires: ground truth (manifest.yaml) must be physically unreachable from
// the subagent's worktree. Materialization only ever copies fixtureDir's own
// contents, so this should be structurally impossible — but "should be" is
// not the same as an assertion, and a future fixture that nests a
// manifest.yaml of its own would otherwise fail silently.
func assertNoManifestLeak(worktree string) error {
	var found string
	err := filepath.WalkDir(worktree, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "manifest.yaml" {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return err
	}
	if found != "" {
		return fmt.Errorf("ground truth leak: manifest.yaml is reachable in the eval worktree at %s", found)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Case execution
// ---------------------------------------------------------------------------

type evalOptions struct {
	CasesDir string
	Runs     int
	Filter   string
	OutDir   string
	Budget   int
	Timeout  time.Duration
}

type assertionResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // pass | fail | skipped
	Detail string `json:"detail,omitempty"`
}

type runRecord struct {
	Case           string            `json:"case"`
	AgentType      string            `json:"agent_type"`
	Run            int               `json:"run"`
	Model          string            `json:"model"`
	Fingerprint    string            `json:"fingerprint"`
	Assertions     []assertionResult `json:"assertions"`
	Output         string            `json:"output"`
	Tokens         int               `json:"tokens"`
	DurationMS     int64             `json:"duration_ms"`
	WriteViolation bool              `json:"write_violation"`
	// NoWritesDeclared records whether THIS case's manifest actually
	// declares `no_writes: true` — see manifestDeclaresNoWrites. It is what
	// lets buildEvalSummary tell "a case that must not write, wrote anyway"
	// (a real guard violation) apart from "a case whose whole job IS to
	// write files, and did" (WriteViolation is unconditionally set from
	// len(changed) > 0 in runOneCase, for every case, batch-editing corpora
	// included — see WriteViolation's own doc comment on this struct's
	// history vs. this field). Without this flag, NoWritesViolations would
	// count every successful edit a writing case makes as a guard breach —
	// exactly the M6 coder-corpus defect this field exists to fix.
	NoWritesDeclared bool   `json:"no_writes_declared"`
	Error            string `json:"error,omitempty"`
	// ToolCalls/LLMTurns/MaxToolCalls/BudgetExhausted are the run's workload
	// profile, copied from task.Stats (see subagent.RunStats) so a later
	// tool-call budget can be sized from real distributions instead of
	// guesswork — this period only collects and summarizes them, see
	// buildEvalSummary's ToolCallsP50/ToolCallsMax.
	ToolCalls       int  `json:"tool_calls"`
	LLMTurns        int  `json:"llm_turns"`
	MaxToolCalls    int  `json:"max_tool_calls"`
	BudgetExhausted bool `json:"budget_exhausted"`
	// WoundDownReason distinguishes WHY BudgetExhausted is true (M6): the
	// wall-clock deadline ("deadline") or the tool-call budget ("tool_budget")
	// — see subagent.RunStats.WoundDownReason. Without this, runs.jsonl could
	// never confirm the wall-clock wrap-up path actually fired during an eval.
	WoundDownReason string `json:"wound_down_reason,omitempty"`
	// HasStats records whether task.Stats was actually available for this
	// run, distinguishing a genuine zero (a run that made no tool calls)
	// from "we never got Stats at all" (e.g. a dispatch timeout whose
	// post-timeout stats poll — see dispatchEvalTask — didn't land in time).
	// buildEvalSummary uses this, not r.Error, to decide which records feed
	// the tool-call distribution: unlike duration/tokens, a timed-out run
	// with recovered stats DOES count there.
	HasStats bool `json:"has_stats"`
}

// runEvalCases runs every case × opts.Runs sequentially — cases and runs
// alike MUST NOT be parallelized: the sandboxed bash tool inherits the
// process cwd (sandbox.ExecDirect is a bare exec.CommandContext with no
// cmd.Dir), so the runner itself is the only thing that can scope a
// dispatch to one case's worktree, via os.Chdir before dispatch and back
// after. Concurrent cases would race that chdir and make write-violation
// attribution meaningless.
//
// onRun, if non-nil, is called synchronously after each run completes,
// BEFORE the next run starts — the real caller's onRun writes the record to
// runs.jsonl and fsyncs it right there (see runWriter), so a real-model run
// that takes over an hour never has more than one run's worth of otherwise
// unrecoverable API spend sitting unflushed. If onRun returns an error, the
// loop stops immediately and that error is returned — but every record
// already produced (including the one just handed to onRun) is preserved in
// the returned slice AND, because onRun already wrote it before returning,
// on disk. A caller must never buffer records and defer writing them until
// this function returns: that reintroduces exactly the all-or-nothing loss
// this design exists to prevent.
func runEvalCases(ctx context.Context, pool evalTaskPool, repoRoot string, evalTools []models.Tool, skillReg *skill.Registry, cases []evalCase, opts evalOptions, onRun func(runRecord) error) ([]runRecord, error) {
	var records []runRecord
	for _, c := range cases {
		fingerprint, err := caseFingerprint(c.Manifest.AgentType, repoRoot, evalTools, skillReg)
		if err != nil {
			return records, fmt.Errorf("case %s/%s: fingerprint: %w", c.AgentType, c.ID, err)
		}
		for run := 1; run <= opts.Runs; run++ {
			rec, err := runOneCase(ctx, pool, c, run, fingerprint, opts)
			if err != nil {
				return records, fmt.Errorf("case %s/%s run %d: %w", c.AgentType, c.ID, run, err)
			}
			records = append(records, rec)
			if onRun != nil {
				if err := onRun(rec); err != nil {
					return records, fmt.Errorf("case %s/%s run %d: %w", c.AgentType, c.ID, run, err)
				}
			}
		}
	}
	return records, nil
}

func runOneCase(ctx context.Context, pool evalTaskPool, c evalCase, run int, fingerprint string, opts evalOptions) (runRecord, error) {
	worktree, cleanup, err := materializeFixture(c.FixtureDir)
	if err != nil {
		return runRecord{}, err
	}
	defer cleanup()

	if err := assertNoManifestLeak(worktree); err != nil {
		return runRecord{}, err
	}

	contextFiles := make([]string, len(c.Manifest.ContextFiles))
	for i, p := range c.Manifest.ContextFiles {
		if filepath.IsAbs(p) {
			contextFiles[i] = p
		} else {
			contextFiles[i] = filepath.Join(worktree, p)
		}
	}

	prevWD, err := os.Getwd()
	if err != nil {
		return runRecord{}, fmt.Errorf("getwd: %w", err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		_ = os.Chdir(prevWD)
	}
	// Runs even if something below panics (e.g. a misbehaving fake task
	// pool in a test) — cwd must never leak into the next case.
	defer restore()

	if err := os.Chdir(worktree); err != nil {
		return runRecord{}, fmt.Errorf("chdir to worktree: %w", err)
	}

	before := chat.TakeWorktreeSnapshot(worktree)

	// dispatchCtx bounds pool.StartTask — it's what the subagent itself sees
	// as its ctx deadline (Pool.runTask derives runCtx from it when no
	// task/pool-level Timeout is separately configured), so it must stay
	// exactly opts.Timeout: that's the window react.go's wall-clock wrap-up
	// (M6) sizes its own reserve against.
	//
	// waitCtx is DELIBERATELY wider (see evalWaitGrace): a subagent whose
	// wall-clock wrap-up fires can legitimately keep running past
	// dispatchCtx's deadline by up to its own reserve to produce a real
	// final answer instead of being killed mid-generation. If Wait used
	// dispatchCtx directly, it would give up and report a dispatch timeout
	// at the exact moment the subagent is gracefully finishing — discarding
	// the very output this feature exists to keep (see dispatchEvalTask's
	// recoverTimedOutStats path for the pre-existing, narrower safety net
	// this complements, not replaces: that poll is a short fixed budget for
	// recovering STATS after a real timeout, not a substitute for waiting
	// out an expected graceful wrap-up).
	//
	// The overshoot this must absorb is bounded to at most ONE reserve — not
	// "half of dispatchCtx's window" on its own, and not per-request either.
	// react.go's wrapUpReserve caps a SINGLE reserve to at most half of
	// dispatchCtx's window, but a subagent's wrap-up phase can legitimately
	// take more than one request (compaction retries re-entering it); before
	// M6's F4 fix, EACH such request got handed a full fresh reserve again,
	// so the cumulative overshoot across a whole Run was unbounded by
	// construction (up to 4 full reserves observed possible, dwarfing the
	// grace this constant provides). react.go's capCumulativeWrapUpBudget
	// (wrapup_wallclock.go) now caps the ENTIRE wrap-up phase — every
	// request in it combined — to at most that one reserve's worth of
	// overshoot, which is what makes "at most half of dispatchCtx's window,
	// full stop" true here.
	dispatchCtx := ctx
	var dispatchCancel context.CancelFunc
	waitCtx := ctx
	var waitCancel context.CancelFunc
	if opts.Timeout > 0 {
		dispatchCtx, dispatchCancel = context.WithTimeout(ctx, opts.Timeout)
		waitCtx, waitCancel = context.WithTimeout(ctx, opts.Timeout+evalWaitGrace(opts.Timeout))
	}
	task, dispatchErr := dispatchEvalTask(dispatchCtx, waitCtx, pool, c, contextFiles, opts.Budget)
	if dispatchCancel != nil {
		dispatchCancel()
	}
	if waitCancel != nil {
		waitCancel()
	}

	after := chat.TakeWorktreeSnapshot(worktree)
	changed := after.ChangedSince(before)

	restore()

	rec := runRecord{
		Case:             c.ID,
		AgentType:        c.Manifest.AgentType,
		Run:              run,
		Fingerprint:      fingerprint,
		WriteViolation:   len(changed) > 0,
		NoWritesDeclared: manifestDeclaresNoWrites(c.Manifest),
	}
	if dispatchErr != nil {
		rec.Error = dispatchErr.Error()
		rec.Assertions = []assertionResult{{Name: "dispatch", Status: "fail", Detail: dispatchErr.Error()}}
		// task may still carry Stats here: dispatchEvalTask's post-timeout
		// poll recovers them when the pool supports it. Error/Assertions
		// above are unconditional on dispatchErr — a recovered task must
		// never change what a timeout run "means", only add to what it
		// tells us about workload.
		if task != nil {
			applyRunStats(&rec, task.Stats)
		}
		return rec, nil
	}

	rec.Output = task.Result
	if task.Usage != nil {
		rec.Tokens = task.Usage.TotalTokens
	}
	applyRunStats(&rec, task.Stats)
	rec.Assertions = evaluateCase(c.Manifest, task.Result, task.Stats, task.Usage, rec.WriteViolation, changed, worktree)
	return rec, nil
}

// applyRunStats copies a task's workload profile onto rec and records that
// real stats were available (rec.HasStats) — the signal buildEvalSummary
// uses to decide whether a run belongs in the tool-call distribution,
// because a zero ToolCalls is ambiguous on its own (a genuinely quiet run
// vs. stats that were never recovered). DurationMS/Model are folded in here
// too (rather than each call site re-checking stats != nil on its own) since
// they're gated by the exact same nil check as everything else this
// function copies.
func applyRunStats(rec *runRecord, stats *subagent.RunStats) {
	if stats == nil {
		return
	}
	rec.HasStats = true
	rec.DurationMS = stats.DurationMS
	rec.Model = stats.Model
	rec.ToolCalls = stats.ToolCalls
	rec.LLMTurns = stats.LLMTurns
	rec.MaxToolCalls = stats.MaxToolCalls
	rec.BudgetExhausted = stats.BudgetExhausted
	rec.WoundDownReason = stats.WoundDownReason
}

// evalWaitGraceCapFraction/evalWaitGraceSlop shape evalWaitGrace's extra
// allowance for pool.Wait, on top of pool.StartTask's opts.Timeout — see
// evalWaitGrace's doc comment for why Wait needs its own, wider budget.
// half of opts.Timeout mirrors the exact cap react.go's wrapUpReserve
// enforces on the subagent's own wall-clock reserve (never more than half of
// ITS deadline, which IS opts.Timeout here — see dispatchEvalTask's doc
// comment), so this is not a separate guess: it's sized to the worst case
// the subagent side can actually produce. The fixed slop on top absorbs the
// pool's own unwind bookkeeping (finishTask's mutex-guarded field writes,
// channel close, this process's scheduler latency) rather than being tuned
// against the wrap-up formula.
const evalWaitGraceCapFraction = 0.5

// evalWaitGraceSlop is a var, not a const, SPECIFICALLY so a test can shrink
// it (save/restore around the test) to a value small enough that a fast test
// can force evalWaitGraceCapFraction's PROPORTIONAL term to dominate the
// grace window without waiting out a real multi-second slop — see
// TestEvalWaitGrace_CapFractionContributesMeaningfully, the coverage gap the
// M6 review found: with the real 5s slop and any Timeout small enough for a
// fast test, the proportional term (capFraction*Timeout, milliseconds) is
// completely swamped by the slop, so a test built that way can never
// actually exercise capFraction at all — zeroing it out entirely left the
// package green. The DEFAULT here (5s) is unchanged from before; only tests
// override it.
var evalWaitGraceSlop = 5 * time.Second

// evalWaitGrace returns the extra time pool.Wait's ctx gets beyond
// pool.StartTask's opts.Timeout (0 if timeout <= 0, matching "no timeout at
// all" for both).
func evalWaitGrace(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 0
	}
	return time.Duration(float64(timeout)*evalWaitGraceCapFraction) + evalWaitGraceSlop
}

// dispatchEvalTask calls pool.StartTask+Wait, recovering any panic from
// either call into a plain error. This is what makes "even a panicking fake
// subagent must not leak a bad cwd" provable in a test: the panic never
// escapes this function, so the caller's chdir-restore always runs on the
// normal return path, not by accident of unwinding.
//
// startCtx and waitCtx are deliberately DIFFERENT contexts (see runOneCase's
// doc comment on the call site): startCtx is what actually bounds the
// dispatched subagent (handed to pool.StartTask, and from there to
// Pool.runTask's runCtx), while waitCtx — handed only to pool.Wait — carries
// the M6 wall-clock grace on top, so a subagent that gracefully wraps up a
// little past startCtx's deadline is still waited out instead of being
// reported as a dispatch timeout.
func dispatchEvalTask(startCtx, waitCtx context.Context, pool evalTaskPool, c evalCase, contextFiles []string, budget int) (task *subagent.Task, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic dispatching case %s/%s: %v", c.AgentType, c.ID, r)
		}
	}()
	started, startErr := pool.StartTask(startCtx, "eval:"+c.AgentType+"/"+c.ID, c.Manifest.Task, subagent.SubagentConfig{
		AgentType:    c.Manifest.AgentType,
		ContextFiles: contextFiles,
		TokenBudget:  budget,
	})
	if startErr != nil {
		return nil, startErr
	}
	completed, waitErr := pool.Wait(waitCtx, started.ID)
	if waitErr != nil {
		// Wait bailed on ITS OWN ctx (waitCtx: opts.Timeout + evalWaitGrace),
		// not because the task reached a terminal state — per Pool.Wait's
		// contract the task entry is deliberately left in the pool so it
		// can keep running (Pool.runTask's runCtx derives from startCtx, so
		// the subagent is already unwinding from cancellation — its OWN
		// deadline, opts.Timeout, is what fired — just not done unwinding
		// yet). Recover its Stats if the pool can still give them to us;
		// this is the run.jsonl's right-tail sample for a tool-call budget,
		// so it's worth a short, bounded wait rather than losing it
		// outright. Reaching this branch at all now means BOTH the
		// subagent's wall-clock wrap-up (if it triggered) AND the extra
		// wait grace were exhausted — a genuine, unrecovered timeout, not
		// the expected-overshoot case evalWaitGrace exists to absorb.
		if recovered := recoverTimedOutStats(pool, started.ID); recovered != nil {
			return recovered, waitErr
		}
		return completed, waitErr
	}
	return completed, nil
}

// evalTaskPoolStats is an optional capability an evalTaskPool may
// additionally satisfy, checked via type assertion rather than added to
// evalTaskPool itself so existing test fakes that only implement
// StartTask/Wait keep compiling unchanged. *subagent.Pool already exposes
// GetTask for other callers (task_list, cancel), so it satisfies this for
// free.
type evalTaskPoolStats interface {
	GetTask(id string) (*subagent.Task, bool)
}

// statsPollInterval/statsPollBudget bound recoverTimedOutStats: a timed-out
// subagent has already been cancelled and just needs to unwind (write a few
// struct fields under a mutex in finishTask) before Stats is non-nil, so a
// couple seconds is generous headroom without risking turning one slow
// dispatch timeout into a meaningfully slower eval run overall (a full
// harness invocation runs many cases, each already bounded by opts.Timeout,
// which is normally minutes).
//
// statsPollBudget is a var, not a const, so a test can shrink it (with
// t.Cleanup restoring the original) to exercise the "pool supports GetTask
// but Stats never lands" path in milliseconds instead of the full 2s.
var (
	statsPollInterval = 20 * time.Millisecond
	statsPollBudget   = 2 * time.Second
)

// recoverTimedOutStats polls pool.GetTask(taskID) for up to statsPollBudget,
// returning the first snapshot whose Stats has landed. Returns nil (no
// error, nothing logged) if the pool doesn't support GetTask at all, if the
// task is no longer found (see below), or if Stats still hasn't landed once
// the budget is spent — either way the caller falls back to the pre-existing
// behavior of a stats-less timeout record, exactly as it did before this
// poll existed.
//
// !ok (task not found) returns immediately rather than polling out the full
// budget: StartTask always Stores the entry before this is ever called, so
// !ok cannot mean "not there yet" — it means some other successful Wait
// already consumed (deleted) it (Pool.Wait's doc comment), a state that by
// construction never reverses. Treating it as "keep polling, might still
// appear" would burn the entire statsPollBudget on every dispatchErr that
// ISN'T a ctx-timeout race (e.g. Pool.Wait's own "task %q not found" error,
// pool.go:99) for no possible benefit.
//
// Side effect worth flagging for whoever next reads a `no_writes viol` count
// and wonders why it moved: this poll runs INSIDE dispatchEvalTask, and
// runOneCase's `after := chat.TakeWorktreeSnapshot(worktree)` happens after
// dispatchEvalTask returns — so a timed-out run's "after" snapshot is now
// taken up to statsPollBudget later than it would have been taken before
// this poll existed, i.e. only once the subagent has actually finished
// unwinding from cancellation (or the poll budget is exhausted, whichever
// comes first). Writes the subagent makes DURING that unwind window, which
// the pre-poll code would never have seen, can now show up as a
// write_violation on a timed-out run. This is intentional (it makes
// no_writes strictly more accurate, not less), but it is a real, visible
// behavior change on timeout runs specifically — if a `no_writes viol` count
// shifts after this period, look here first, not at the model or the guard
// logic.
func recoverTimedOutStats(pool evalTaskPool, taskID string) *subagent.Task {
	getter, ok := pool.(evalTaskPoolStats)
	if !ok {
		return nil
	}
	deadline := time.Now().Add(statsPollBudget)
	for {
		snap, found := getter.GetTask(taskID)
		if !found {
			return nil
		}
		if snap.Stats != nil {
			return snap
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(statsPollInterval)
	}
}

// ---------------------------------------------------------------------------
// Assertions
// ---------------------------------------------------------------------------

// manifestDeclaresNoWrites reports whether m's `expect` list contains a
// `no_writes: true` entry. This is the ONLY thing that should gate
// buildEvalSummary's NoWritesViolations counter: WriteViolation itself
// (runOneCase, from the worktree snapshot diff) is set unconditionally for
// EVERY case, including a batch-editing corpus (coder/*) whose whole task
// is to write files. Folding an unconditional WriteViolation into a
// "violations" counter would score a perfect, on-task edit as a guard
// breach — see this file's package doc comment and NoWritesDeclared's doc
// comment on runRecord for the M6 defect this exists to fix.
func manifestDeclaresNoWrites(m caseManifest) bool {
	for _, exp := range m.Expect {
		key, val, ok := singleKV(exp)
		if !ok || key != "no_writes" {
			continue
		}
		want, _ := val.(bool)
		if want {
			return true
		}
	}
	return false
}

// evaluateCase scores one dispatched run against its manifest's `expect`
// list. changed/worktree feed the "did the edit actually land" family
// (files_changed/file_contains/file_not_contains, added this period so a
// batch-editing corpus can assert on file CONTENT, not just text output —
// see this file's package doc comment): changed is runOneCase's
// chat.WorktreeSnapshot.ChangedSince(before) result, an ABSOLUTE path list
// (root-joined, see pkg/chat/review.go's changedSince), and worktree is the
// same root those paths were joined against, so this function can both
// convert changed into worktree-relative paths comparable to a manifest's
// repo-relative file lists, and open worktree-relative paths itself to
// check file content. Every existing case (no_writes only, no
// files_changed/file_contains/file_not_contains in its manifest) is
// unaffected: changed/worktree are simply unused for it, exactly as before
// this signature grew them.
func evaluateCase(m caseManifest, output string, stats *subagent.RunStats, usage *subagent.TokenUsage, writeViolation bool, changed []string, worktree string) []assertionResult {
	var results []assertionResult

	for _, exp := range m.Expect {
		key, val, ok := singleKV(exp)
		if !ok {
			// A manifest expect entry with zero keys (or, since singleKV
			// picks an arbitrary key out of Go's undefined map iteration
			// order, more than one) is a malformed manifest, not a no-op —
			// see parseFileTextExpectations' doc comment on why this
			// family refuses to let a bad manifest shape score as "nothing
			// to check here".
			results = append(results, assertionResult{Name: "expect:malformed", Status: "fail", Detail: fmt.Sprintf("expect entry must have exactly one key, got %v", exp)})
			continue
		}
		switch key {
		case "mentions":
			for _, item := range toStringSlice(val) {
				pass := strings.Contains(output, item)
				results = append(results, assertionResult{Name: "mentions:" + item, Status: statusFor(pass)})
			}
		case "not_mentions":
			for _, item := range toStringSlice(val) {
				pass := !strings.Contains(output, item)
				res := assertionResult{Name: "not_mentions:" + item, Status: statusFor(pass)}
				if !pass {
					res.Detail = "fabricated/deleted symbol present in output"
				}
				results = append(results, res)
			}
		case "tool_calls_max":
			max := intFromYAML(val)
			actual := 0
			if stats != nil {
				actual = stats.ToolCalls
			}
			results = append(results, assertionResult{
				Name:   fmt.Sprintf("tool_calls_max:%d", max),
				Status: statusFor(actual <= max),
				Detail: fmt.Sprintf("actual=%d", actual),
			})
		case "no_writes":
			want, _ := val.(bool)
			pass := true
			if want {
				pass = !writeViolation
			}
			results = append(results, assertionResult{Name: "no_writes", Status: statusFor(pass)})
		case "tokens_max":
			max := intFromYAML(val)
			actual := 0
			if usage != nil {
				actual = usage.TotalTokens
			}
			results = append(results, assertionResult{
				Name:   fmt.Sprintf("tokens_max:%d", max),
				Status: statusFor(actual <= max),
				Detail: fmt.Sprintf("actual=%d", actual),
			})
		case "files_changed":
			results = append(results, evaluateFilesChanged(toStringSlice(val), changed, worktree))
		case "file_contains":
			items, problems := parseFileTextExpectations(val)
			for _, p := range problems {
				results = append(results, assertionResult{Name: "file_contains:malformed", Status: "fail", Detail: p})
			}
			for _, fe := range items {
				results = append(results, evaluateFileContains(fe, worktree, true))
			}
		case "file_not_contains":
			items, problems := parseFileTextExpectations(val)
			for _, p := range problems {
				results = append(results, assertionResult{Name: "file_not_contains:malformed", Status: "fail", Detail: p})
			}
			for _, fe := range items {
				results = append(results, evaluateFileContains(fe, worktree, false))
			}
		default:
			// An unrecognized expect key (a typo like file_content or
			// files_change) used to silently produce zero assertions —
			// exactly the "quietest possible failure" this assertion
			// family exists to rule out. loadEvalCases rejects this at
			// manifest-load time for the real corpus (validateManifestExpect);
			// this default case is the same guarantee for any manifest that
			// reaches evaluateCase without going through loadEvalCases.
			results = append(results, assertionResult{Name: "expect:unknown_key:" + key, Status: "fail", Detail: fmt.Sprintf("unrecognized expect key %q", key)})
		}
	}
	return results
}

// evaluateFilesChanged is the `files_changed` assertion: the set of files
// actually touched (changed, converted to worktree-relative paths) must be
// EXACTLY the manifest's declared set — not "at least", not "a subset".
// Exact-set matching (rather than "every declared path was among the
// changed ones") is deliberate: a case whose task names 3 files and gets an
// unrelated 4th file edited alongside them ("fixed something else while I
// was in there") is exactly the kind of scope creep this assertion exists to
// catch, and "contains" would let it through silently.
func evaluateFilesChanged(want []string, changed []string, worktree string) assertionResult {
	actual := relativeChangedPaths(changed, worktree)
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[filepath.ToSlash(w)] = true
	}
	actualSet := map[string]bool{}
	for _, a := range actual {
		actualSet[a] = true
	}
	var missing, extra []string
	for w := range wantSet {
		if !actualSet[w] {
			missing = append(missing, w)
		}
	}
	for a := range actualSet {
		if !wantSet[a] {
			extra = append(extra, a)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	pass := len(missing) == 0 && len(extra) == 0
	res := assertionResult{Name: "files_changed", Status: statusFor(pass)}
	if !pass {
		sortedActual := append([]string(nil), actual...)
		sort.Strings(sortedActual)
		res.Detail = fmt.Sprintf("missing=%v extra=%v actual=%v", missing, extra, sortedActual)
	}
	return res
}

// relativeChangedPaths converts changed (absolute paths, joined against
// chat.WorktreeSnapshot's OWN idea of the worktree root — gitToplevel's
// `git rev-parse --show-toplevel`, see pkg/chat/review.go's changedSince)
// into worktree-relative, forward-slash-normalized paths comparable to a
// manifest's repo-relative file lists (the same convention context_files
// already uses).
//
// worktree here is runOneCase's os.MkdirTemp(...) result — the path BEFORE
// materializeFixture's `git init` resolves it. On macOS (and anywhere
// os.TempDir() is itself a symlink, e.g. /tmp -> /private/tmp) those two
// strings differ only in a resolved-symlink prefix, so a plain
// filepath.Rel(worktree, p) fails (p isn't under the literal, unresolved
// worktree string) and would silently produce garbage ("../../../..."
// relative paths) rather than a clean case-relative one. The fallback
// retries filepath.Rel against filepath.EvalSymlinks(worktree) — the same
// resolution git itself already applied to produce p's prefix — before
// giving up and leaving the absolute path as-is (still usable in a failure
// detail, just not case-relative).
func relativeChangedPaths(changed []string, worktree string) []string {
	var resolvedWorktree string
	out := make([]string, 0, len(changed))
	for _, p := range changed {
		rel := p
		switch {
		case worktree == "":
			// no-op: rel stays the absolute path.
		default:
			if r, err := filepath.Rel(worktree, p); err == nil && !strings.HasPrefix(r, ".."+string(filepath.Separator)) && r != ".." {
				rel = r
				break
			}
			if resolvedWorktree == "" {
				if r, err := filepath.EvalSymlinks(worktree); err == nil {
					resolvedWorktree = r
				} else {
					resolvedWorktree = worktree
				}
			}
			if r, err := filepath.Rel(resolvedWorktree, p); err == nil && !strings.HasPrefix(r, ".."+string(filepath.Separator)) && r != ".." {
				rel = r
			}
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out
}

// fileTextExpectation is one `file_contains`/`file_not_contains` list entry:
// {path, text}, both required to mean anything — parseFileTextExpectations
// below refuses to build one with either blank, precisely because a blank
// text makes strings.Contains(x, "") an assertion that can never fail
// (always "pass"), and a blank path makes the read a no-op. Neither may
// reach evaluateFileContains silently: see parseFileTextExpectations' doc
// comment for where the fail this problem deserves is added instead.
type fileTextExpectation struct {
	Path string
	Text string
}

// parseFileTextExpectations decodes one `file_contains`/`file_not_contains`
// manifest value into the {path,text} entries to actually check, and a
// separate `problems` list describing every entry the manifest got wrong —
// v not a list at all (a manifest author wrote a map instead of a list),
// a list item that isn't a {path,text} mapping, or a mapping with a blank
// path/text.
//
// The split return (rather than silently skipping bad entries, which is
// what this function used to do under the name toFileTextExpectations) is
// the fix for the M6 defect where three shapes of malformed manifest —
// blank text (an assertion that can never fail), file_contains written as
// a map instead of a list, and an unrecognized expect key entirely — were
// all silently accepted as "0 assertions, no warning" instead of the loud
// failure this whole assertion family exists to guarantee (see
// roleSummary.FileEditAssertionFailures' doc comment: "failure is never
// invisible"). evaluateCase turns each problem into its own "...:malformed"
// fail assertionResult rather than dropping it.
func parseFileTextExpectations(v any) (valid []fileTextExpectation, problems []string) {
	items, ok := v.([]any)
	if !ok {
		return nil, []string{fmt.Sprintf("expected a list of {path, text} entries, got %T", v)}
	}
	for i, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			problems = append(problems, fmt.Sprintf("entry %d: expected a {path, text} mapping, got %T", i, it))
			continue
		}
		path, _ := m["path"].(string)
		text, _ := m["text"].(string)
		if strings.TrimSpace(path) == "" {
			problems = append(problems, fmt.Sprintf("entry %d: path must not be empty", i))
			continue
		}
		if text == "" {
			problems = append(problems, fmt.Sprintf("entry %d (path=%s): text must not be empty", i, path))
			continue
		}
		valid = append(valid, fileTextExpectation{Path: path, Text: text})
	}
	return valid, problems
}

// evaluateFileContains backs both file_contains (wantPresent=true) and
// file_not_contains (wantPresent=false): read worktree/fe.Path and check
// whether fe.Text is a substring. A file that cannot be read (most often:
// the case never touched it at all) is ALWAYS a clean fail with a detail
// naming the read error — never a panic, and never a silent pass for
// file_not_contains (an untouched file is not evidence the old text was
// removed; it is evidence the edit never happened).
func evaluateFileContains(fe fileTextExpectation, worktree string, wantPresent bool) assertionResult {
	verb := "file_contains"
	if !wantPresent {
		verb = "file_not_contains"
	}
	name := fmt.Sprintf("%s:%s:%s", verb, fe.Path, fe.Text)
	data, err := os.ReadFile(filepath.Join(worktree, fe.Path))
	if err != nil {
		return assertionResult{Name: name, Status: "fail", Detail: fmt.Sprintf("read %s: %v", fe.Path, err)}
	}
	present := strings.Contains(string(data), fe.Text)
	pass := present == wantPresent
	res := assertionResult{Name: name, Status: statusFor(pass)}
	if !pass {
		if wantPresent {
			res.Detail = fmt.Sprintf("expected text not found in %s", fe.Path)
		} else {
			res.Detail = fmt.Sprintf("old text still present in %s", fe.Path)
		}
	}
	return res
}

func statusFor(pass bool) string {
	if pass {
		return "pass"
	}
	return "fail"
}

func singleKV(m map[string]any) (string, any, bool) {
	for k, v := range m {
		return k, v, true
	}
	return "", nil, false
}

func intFromYAML(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

func toStringSlice(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Fingerprint
// ---------------------------------------------------------------------------

// resolveEvalSubagentPrompt computes the EXACT system prompt bytes
// SubagentExecutor.Execute would assemble into a dispatched subagent's
// a.systemPrompt for agentType, given no task.Config.SystemPrompt/Skill
// override — which is what every eval case dispatches with (agent_type +
// prompt only, see dispatchEvalTask) — and then feeds through
// AssembleSystemPrompt against the RESTRICTED tool set Execute
// would actually hand that subagent, exactly as BuildSystemPrompt does.
// This is the fix for caseFingerprint's defect (see its doc comment): the
// fingerprint must cover what actually reaches the model, not just the
// role's bare base prompt.
//
// Every step below calls an exported pkg/agent function that IS the
// production logic (ResolveAgentTypeConfig wraps the same resolver Execute
// calls; PreloadSkillsProfile/AppendOutputSchemaPrompt are the exact blocks
// Execute runs, factored out; SelectSubagentTools is Execute's own tool
// selector; AssembleSystemPrompt is BuildSystemPrompt's own section
// assembly, already joined) — never a re-derivation of any of their formatting or merge
// rules. That is deliberate: a second implementation of any of this is
// exactly the bug pattern that made the OLD fingerprint (pre-M6) silently
// blind to BuildSystemPrompt's gated sections, and this codebase has hit
// that same "two copies quietly drift apart" failure before (a fixture
// diverging from its source file, a comment diverging from the code it
// describes). Extracting shared functions from pkg/agent — used by both
// production and this harness — is the only way to make that class of bug
// structurally impossible here, rather than merely reviewed-away once.
//
// evalTools is the harness's full tool candidate list (evalToolCandidates) —
// the SAME list buildEvalStack registers into the real dispatch registry —
// so SelectSubagentTools below sees the identical candidate set
// SubagentExecutor.Execute's e.tools.List() would.
//
// Any problem surfaced while resolving the project YAML/MD (a parse error,
// an unknown output_schema: name, ...) is a HARD error here even though
// production (Execute, via ResolveAgentTypeConfig) only WARNS and silently
// falls back to the builtin profile for a type that still resolves to one —
// deliberately stricter than production, matching this eval harness's
// pre-existing policy (see the M5-2/M5-3 history) of never letting a broken
// project override in the corpus silently fingerprint the wrong (fallback)
// prompt.
func resolveEvalSubagentPrompt(agentType, repoRoot string, evalTools []models.Tool, skillReg *skill.Registry) (string, error) {
	at := agent.AgentType(agentType)
	profileCfg, problems, typeResolved := agent.ResolveAgentTypeConfig(at, repoRoot, nil)
	if !typeResolved {
		return "", fmt.Errorf("agent type %q not resolved to any source (problems: %s)", agentType, strings.Join(problems, "; "))
	}
	if len(problems) > 0 {
		return "", fmt.Errorf("agent type %q: %s", agentType, strings.Join(problems, "; "))
	}

	base, err := agent.PreloadSkillsProfile(profileCfg.SystemPrompt, profileCfg.Type, profileCfg.Skills, skillReg)
	if err != nil {
		return "", err
	}
	base = agent.AppendOutputSchemaPrompt(base, profileCfg.OutputSchema)

	selected, err := agent.SelectSubagentTools(evalTools, profileCfg.DefaultTools)
	if err != nil {
		return "", err
	}
	restricted := tools.NewRegistry()
	for _, t := range selected {
		mustRegisterTool(restricted, t)
	}

	// nonInteractive=true, agentCatalog=nil: matches every subagent Execute
	// constructs (AgentConfig.NonInteractive is always true; AgentCatalog is
	// never set — see subagent.go's buildAgentConfig). Plan mode is not
	// replicated here because it is unconditionally inert for a subagent —
	// see AssembleSystemPrompt's doc comment (pkg/agent/
	// promptbuild.go) for why: Execute never sets AgentConfig.PlanMode, and
	// New() only ever honors it when !NonInteractive.
	return agent.AssembleSystemPrompt(base, restricted, true, nil), nil
}

// caseFingerprint = sha256(resolveEvalSubagentPrompt's full assembled system
// prompt)[:8], hex-encoded.
//
// This fingerprint's whole stated purpose (design docs/AGENT_CAPABILITY_
// DESIGN.md §5, and `deepai eval compare`'s "fingerprint unchanged" gate) is
// to catch a stale before-run vouching for a system prompt it never actually
// saw. That only holds if the hashed bytes ARE the bytes the model actually
// receives. Before this fix, caseFingerprint hashed resolveEvalSystemPrompt's
// bare role SystemPrompt + skill bodies + OutputSchema.Prompt — but the
// REAL system prompt a dispatched subagent gets is
// (*agent.Agent).BuildSystemPrompt()'s output, which additionally appends,
// gated on the subagent's RESTRICTED tool set: the file-operation rule
// (hasAnyFileTool), search-tool recommendations (hasSearchTools), and
// todo-tool guidance (hasTodoTool). At the time this was written the M6
// batch-tool-calls guidance was a fourth such section; it has since been
// removed, but it is what exposed the defect described below. The M6
// period added batchToolCallsPrompt (~1.1KB) to the system prompt of all
// five tested roles, and the old fingerprint formula did not move a single
// byte — every role's fingerprint stayed identical across a real prompt
// change, which is precisely the case `eval compare`'s "fingerprint
// unchanged" WARNING exists to catch, and which docs/AGENT_CAPABILITY_
// DESIGN.md §5 (and later, `eval summarize`'s hard-refusal gate) relied on
// as evidence a role's prompt was untouched.
//
// The fix hashes resolveEvalSubagentPrompt's fully assembled string instead
// — see that function's doc comment for how it reconstructs the exact
// bytes Execute+BuildSystemPrompt produce, via shared pkg/agent exports
// rather than a second implementation.
//
// All five tested roles' fingerprints change once, here, as an intentional
// one-time cost: the only extant before-run (eval/results/2026-09-07-
// glm-5.3-merged/, disk-local, not tracked) is invalidated at the hash
// level regardless of whether its underlying prompts changed, because it
// was computed with the OLD (incomplete) formula. There is deliberately no
// compatibility shim or fingerprint version tag to keep the old value
// matching — see this function's own doc comment above for why: an
// approximate fingerprint is worse than an honestly-narrow one that changes
// when it should.
func caseFingerprint(agentType, repoRoot string, evalTools []models.Tool, skillReg *skill.Registry) (string, error) {
	prompt, err := resolveEvalSubagentPrompt(agentType, repoRoot, evalTools, skillReg)
	if err != nil {
		return "", err
	}
	return computeFingerprint(prompt), nil
}

// computeFingerprint hashes the FULL assembled system prompt string (see
// caseFingerprint's doc comment) — not its separate components — because
// the fingerprint's job is to detect any change in what actually reaches
// the model, and the only way to guarantee that is to hash the exact bytes
// BuildSystemPrompt would produce, already joined in the same order.
func computeFingerprint(systemPrompt string) string {
	h := sha256.New()
	h.Write([]byte(systemPrompt))
	return hex.EncodeToString(h.Sum(nil))[:8]
}

// ---------------------------------------------------------------------------
// Output: runs.jsonl / summary.json / summary.md
// ---------------------------------------------------------------------------

type roleSummary struct {
	AgentType      string `json:"agent_type"`
	Fingerprint    string `json:"fingerprint"`
	Cases          int    `json:"cases"`
	Runs           int    `json:"runs"`
	DispatchedRuns int    `json:"dispatched_runs"`
	// AssertionPassRate/Fail/Skip are diagnostic only (see
	// docs/AGENT_CAPABILITY_DESIGN.md §8's "废弃‘整体断言通过率’作为达标线"):
	// reported for continuity with the M5-2 baseline, no longer a gate.
	AssertionPassRate        float64 `json:"assertion_pass_rate"`
	AssertionFailRate        float64 `json:"assertion_fail_rate"`
	AssertionSkipRate        float64 `json:"assertion_skip_rate"`
	MentionsHitRate          float64 `json:"mentions_hit_rate"`
	MentionsHits             int     `json:"mentions_hits"`
	MentionsTotal            int     `json:"mentions_total"`
	NotMentionsViolationRate float64 `json:"not_mentions_violation_rate"`
	// GuardViolations sums the no_writes/tool_calls_max:*/tokens_max:*
	// assertion fail count and the write_violation-flag count. Per the M5-3
	// harness brief this is a literal additive count (a coarse safety-floor
	// tripwire, not a deduplicated metric) — for a case whose manifest
	// declares `no_writes: true`, an actual write violation trips BOTH the
	// no_writes assertion fail AND NoWritesViolations, so it is counted
	// twice. That is intentional: it is still exactly 0 whenever nothing
	// went wrong, which is the only threshold this field is used for
	// (renderEvalCompare's guard-violations gate is "== 0", not a rate).
	GuardViolations    int     `json:"guard_violations"`
	AvgTokens          float64 `json:"avg_tokens"`
	AvgDurationMS      float64 `json:"avg_duration_ms"`
	NoWritesViolations int     `json:"no_writes_violations"`
	DispatchErrors     int     `json:"dispatch_errors"`
	// FileEditAssertionFailures counts fail statuses across the
	// files_changed/file_contains/file_not_contains assertion family — the
	// "did the edit actually land" checks added for the M6 batch-editing
	// corpus (this file's package doc comment). Deliberately NOT folded
	// into GuardViolations: a guard violation means a safety-floor rule was
	// broken (no_writes/tool_calls_max/tokens_max — see GuardViolations'
	// doc comment above), whereas a files_changed/file_contains/
	// file_not_contains failure means the requested work was not done
	// correctly — a quality signal, not a guardrail breach, and mixing the
	// two would let "the model just did less work" hide inside a count
	// whose whole job is "== 0 means nothing unsafe happened". This field
	// exists specifically so that failure is never invisible: it must
	// never disappear into AssertionPassRate alone (diagnostic-only, not a
	// gate — see that field's doc comment), because a run that skipped the
	// edits entirely would still look fast and clean by every other
	// column.
	FileEditAssertionFailures int `json:"file_edit_assertion_failures"`
	// ToolCallsP50/ToolCallsMax/BudgetExhaustedRuns profile tool-call
	// workload, not correctness — collected this period so a later subagent
	// tool-call budget can be sized from a real distribution instead of a
	// guess. Denominator note: unlike every field above (dispatched runs
	// only, see the doc comment on buildEvalSummary), these three are
	// computed over every record with r.HasStats — including a dispatch
	// timeout whose stats were recovered — because a timeout is the
	// right-tail sample a budget most needs to see, not noise to exclude.
	//
	// StatsRuns is that denominator, made explicit: len(recs with
	// r.HasStats). Without it, a reader of summary.md sees
	// "dispatched=0, budget exhausted=3" and cannot tell whether that's 3/3
	// or 3/30 — DispatchedRuns is the wrong denominator for these three
	// fields (see above), so it has to be reported separately.
	StatsRuns           int     `json:"stats_runs"`
	ToolCallsP50        float64 `json:"tool_calls_p50"`
	ToolCallsMax        int     `json:"tool_calls_max"`
	BudgetExhaustedRuns int     `json:"budget_exhausted_runs"`
}

type evalSummary struct {
	GeneratedAt string        `json:"generated_at"`
	Model       string        `json:"model"`
	Runs        int           `json:"runs"`
	Timeout     string        `json:"timeout"`
	Roles       []roleSummary `json:"roles"`
}

// buildEvalSummary aggregates records into one roleSummary per agent_type.
//
// A record with r.Error != "" is a dispatch timeout (or other dispatch
// failure): its duration_ms is always 0 and its lone assertion is a
// synthetic "dispatch" fail with no bearing on any real assertion family.
// Such a record contributes ONLY to DispatchErrors — every other counter
// (assertion pass/fail/skip, schema/field/mentions/not_mentions family
// counts, guard violations, token/duration sums) skips it entirely, and
// DispatchedRuns — not len(recs) — is the denominator for every rate and
// average below. Folding a duration_ms=0 timeout into an averaging
// denominator silently deflates the average by exactly (1 - dispatched/total):
// this is the fix for the bug the M5-3 harness-changeover brief called out
// against the real 2026-09-06-glm-5.3 baseline (analyst's true dispatched-only
// mean duration is 152,923ms; the old code reported 135,931ms — 12.5% low).
//
// ToolCallsP50/ToolCallsMax/BudgetExhaustedRuns are the one deliberate
// exception to "a dispatch-errored record contributes nothing but
// DispatchErrors" above: they are computed over every record with
// r.HasStats, timeouts included. duration_ms/tokens exclude a timeout
// because it has no real duration/token figure to average in (it never
// finished, so 0 would just be wrong) — but a timeout that DID recover
// tool-call stats (see dispatchEvalTask's post-timeout GetTask poll) has a
// perfectly real tool_calls count, and it's the run that most needs to be in
// this particular distribution: a subagent that got cut off mid-work after
// running the tool-call counter way up is exactly the right-tail sample a
// tool-call budget is being collected to size against. Excluding it here
// would hide the one case the whole feature exists to catch.
func buildEvalSummary(model string, runs int, timeout string, records []runRecord) evalSummary {
	byRole := map[string][]runRecord{}
	var order []string
	for _, r := range records {
		if _, ok := byRole[r.AgentType]; !ok {
			order = append(order, r.AgentType)
		}
		byRole[r.AgentType] = append(byRole[r.AgentType], r)
	}
	sort.Strings(order)

	var roles []roleSummary
	for _, agentType := range order {
		recs := byRole[agentType]
		rs := roleSummary{AgentType: agentType, Runs: runs}
		if len(recs) > 0 {
			rs.Fingerprint = recs[0].Fingerprint
		}
		caseSet := map[string]bool{}
		var totalPass, totalFail, totalSkip int
		var mentionsPass, mentionsFail int
		var notMentionsPass, notMentionsFail int
		var guardFail int
		var fileEditFail int
		var tokensSum, durationSum float64
		var toolCallsSamples []int
		var budgetExhaustedRuns int
		for _, r := range recs {
			caseSet[r.Case] = true
			// Collected unconditionally on r.HasStats, ahead of the
			// dispatch-error continue below — see the function doc for why
			// the tool-call distribution deliberately does NOT exclude a
			// timeout the way duration/tokens/assertions do.
			if r.HasStats {
				toolCallsSamples = append(toolCallsSamples, r.ToolCalls)
				if r.BudgetExhausted {
					budgetExhaustedRuns++
				}
			}
			if r.Error != "" {
				rs.DispatchErrors++
				continue // see the function doc: a timeout contributes nothing else
			}
			rs.DispatchedRuns++
			// Gated on r.NoWritesDeclared: a write_violation only counts as
			// a no_writes VIOLATION for a case whose manifest actually
			// declares no_writes: true. A batch-editing case (coder/*) sets
			// WriteViolation unconditionally too (see runOneCase) but never
			// declares no_writes, so it must never land here — see
			// manifestDeclaresNoWrites' doc comment.
			if r.NoWritesDeclared && r.WriteViolation {
				rs.NoWritesViolations++
			}
			tokensSum += float64(r.Tokens)
			durationSum += float64(r.DurationMS)

			for _, a := range r.Assertions {
				switch a.Status {
				case "pass":
					totalPass++
				case "fail":
					totalFail++
				case "skipped":
					totalSkip++
				}
				switch {
				case strings.HasPrefix(a.Name, "mentions:"):
					if a.Status == "pass" {
						mentionsPass++
					} else if a.Status == "fail" {
						mentionsFail++
					}
				case strings.HasPrefix(a.Name, "not_mentions:"):
					if a.Status == "pass" {
						notMentionsPass++
					} else if a.Status == "fail" {
						notMentionsFail++
					}
				case a.Name == "no_writes", strings.HasPrefix(a.Name, "tool_calls_max:"), strings.HasPrefix(a.Name, "tokens_max:"):
					if a.Status == "fail" {
						guardFail++
					}
				case a.Name == "files_changed", strings.HasPrefix(a.Name, "file_contains:"), strings.HasPrefix(a.Name, "file_not_contains:"):
					if a.Status == "fail" {
						fileEditFail++
					}
				}
			}
		}
		rs.Cases = len(caseSet)
		rs.AssertionPassRate = ratio(totalPass, totalPass+totalFail)
		rs.AssertionFailRate = ratio(totalFail, totalPass+totalFail)
		rs.AssertionSkipRate = ratio(totalSkip, totalPass+totalFail+totalSkip)
		rs.MentionsHitRate = ratio(mentionsPass, mentionsPass+mentionsFail)
		rs.MentionsHits = mentionsPass
		rs.MentionsTotal = mentionsPass + mentionsFail
		rs.NotMentionsViolationRate = ratio(notMentionsFail, notMentionsPass+notMentionsFail)
		rs.GuardViolations = guardFail + rs.NoWritesViolations
		rs.FileEditAssertionFailures = fileEditFail
		if rs.DispatchedRuns > 0 {
			rs.AvgTokens = tokensSum / float64(rs.DispatchedRuns)
			rs.AvgDurationMS = durationSum / float64(rs.DispatchedRuns)
		}
		rs.StatsRuns = len(toolCallsSamples)
		rs.ToolCallsP50 = medianInt(toolCallsSamples)
		if len(toolCallsSamples) > 0 {
			rs.ToolCallsMax = slices.Max(toolCallsSamples)
		}
		rs.BudgetExhaustedRuns = budgetExhaustedRuns
		roles = append(roles, rs)
	}

	return evalSummary{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Model:       model,
		Runs:        runs,
		Timeout:     timeout,
		Roles:       roles,
	}
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// medianInt returns the p50 of vals (the standard even-count average of the
// two middle values — a budget-sizing read wants "half the runs are at or
// below this many calls", which the interpolated median gives directly,
// unlike picking one of the two middle samples arbitrarily). 0 on an empty
// input (no run in this role had recoverable stats) rather than a panic or
// NaN, since a bare 0 reads correctly here: "no data yet", not "0 calls".
func medianInt(vals []int) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]int(nil), vals...)
	sort.Ints(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return float64(sorted[mid])
	}
	return float64(sorted[mid-1]+sorted[mid]) / 2
}

// maxEvalResultDirSuffix bounds the "-<n>" search in prepareEvalResultDir.
// 100 same-day same-model reruns in one session is already an absurd
// number; beyond that something is wrong (e.g. a caller looping without
// checking errors) and we should fail loudly rather than spin forever.
const maxEvalResultDirSuffix = 100

// prepareEvalResultDir resolves and creates a fresh
// eval/results/<date>-<model-alias>[-<n>]/ directory, without writing
// anything into it yet.
//
// If the plain <date>-<model-alias> name is already taken, it tries -2, -3,
// … up to maxEvalResultDirSuffix, and creates the first name that does not
// yet exist. This is a deliberate choice over rejecting the run outright:
// the scenario that triggers a collision — re-running the same role,
// same day, for review — is routine here, not exceptional, and a real
// `deepai eval agents` invocation can run for over an hour. Making the
// caller stop and manually rename the previous directory (which is how
// this was worked around before) just reintroduces the failure mode this
// fix exists to close: someone in a hurry skips the rename and the new run
// silently clobbers the old one's runs.jsonl. An automatic suffix costs
// nothing and can never lose data, so there is no tradeoff to make here.
//
// The directory is created with os.Mkdir (not os.MkdirAll) in the
// collision-search loop: os.Mkdir fails atomically if the name already
// exists, whereas os.MkdirAll happily returns nil for an existing
// directory — that MkdirAll behavior is exactly how the original bug let
// two runs share one directory. Using Mkdir here also makes the
// "does this name exist" check and the "claim this name" step a single
// atomic syscall, so two `deepai eval agents` processes starting at
// nearly the same moment cannot both win the same directory name.
func prepareEvalResultDir(outRoot, model string) (string, error) {
	if err := os.MkdirAll(outRoot, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", outRoot, err)
	}

	dateStr := time.Now().UTC().Format("2006-01-02")
	safeModel := strings.NewReplacer("/", "-", " ", "-").Replace(model)
	if safeModel == "" {
		safeModel = "default"
	}
	base := filepath.Join(outRoot, fmt.Sprintf("%s-%s", dateStr, safeModel))

	for n := 1; n <= maxEvalResultDirSuffix; n++ {
		dir := base
		if n > 1 {
			dir = fmt.Sprintf("%s-%d", base, n)
		}
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			if n > 1 {
				fmt.Fprintf(os.Stderr, "eval agents: %s already exists, writing results to %s instead\n", base, dir)
			}
			return dir, nil
		}
		if !os.IsExist(err) {
			return "", fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	return "", fmt.Errorf("prepareEvalResultDir: all of %s through %s-%d already exist", base, base, maxEvalResultDirSuffix)
}

// runWriter appends runRecord values to runs.jsonl one at a time, fsyncing
// after every write. A real `deepai eval agents` invocation can run 15
// cases × 3 runs against a live model for over an hour; a single API error,
// timeout, Ctrl+C, or machine sleep partway through must not destroy every
// dollar of model spend that already happened. Batching all records into one
// os.Create-then-encode-everything at the very end (this file's original
// design, before the M5-2 first real run caught it) had exactly that
// failure mode — a run that dies at case 10 of 15 leaves runs.jsonl
// completely empty, with nothing to show for the 9 completed cases' worth
// of real token spend.
//
// summary.md/summary.json are NOT written incrementally: they are aggregate
// views over the whole run, cheap to regenerate from runs.jsonl (or from
// whatever records a caller collected before an interruption), so there is
// nothing to lose by deferring them to the end.
type runWriter struct {
	f   *os.File
	enc *json.Encoder
}

// newRunWriter opens path with O_EXCL, refusing to reuse a runs.jsonl that
// already exists rather than truncating it (os.Create's behavior). This is
// a second, independent line of defense against the same failure mode
// prepareEvalResultDir's directory-suffix search guards against: (1) should
// never happen, because prepareEvalResultDir always hands back a directory
// it just created, so runs.jsonl inside it cannot already exist; O_EXCL
// turns "should never happen" into "cannot happen even if that invariant is
// ever violated" — by a future refactor, a caller that builds the path by
// hand instead of going through prepareEvalResultDir, or a bug — without
// relying on the directory-naming logic alone being correct forever.
func newRunWriter(path string) (*runWriter, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("refusing to overwrite existing results at %s: %w", path, err)
		}
		return nil, fmt.Errorf("create %s: %w", path, err)
	}
	return &runWriter{f: f, enc: json.NewEncoder(f)}, nil
}

// Write encodes rec as one JSON line and fsyncs it before returning, so the
// record is durable on disk the instant Write returns — not just handed to
// an OS write-back cache that a killed process or a sleeping machine can
// still lose.
func (w *runWriter) Write(rec runRecord) error {
	if err := w.enc.Encode(rec); err != nil {
		return fmt.Errorf("encode run record: %w", err)
	}
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("sync runs.jsonl: %w", err)
	}
	return nil
}

func (w *runWriter) Close() error { return w.f.Close() }

// writeEvalSummary writes summary.md and summary.json into dir (already
// created by prepareEvalResultDir). It does not touch runs.jsonl — see
// runWriter.
func writeEvalSummary(dir, model string, runs int, timeout string, records []runRecord) error {
	summary := buildEvalSummary(model, runs, timeout, records)

	summaryJSONPath := filepath.Join(dir, "summary.json")
	jsonBytes, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(summaryJSONPath, jsonBytes, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", summaryJSONPath, err)
	}

	summaryMDPath := filepath.Join(dir, "summary.md")
	if err := os.WriteFile(summaryMDPath, []byte(renderEvalSummaryMD(summary)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", summaryMDPath, err)
	}

	return nil
}

func renderEvalSummaryMD(s evalSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# deepai eval agents — %s\n\n", s.GeneratedAt)
	fmt.Fprintf(&b, "Model: `%s`  \nRuns per case: %d  \nTimeout: `%s`\n\n", s.Model, s.Runs, s.Timeout)
	// tool_calls p50/max and budget exhausted are the columns a later
	// tool-call budget gets sized from — see roleSummary's doc comment for
	// why their population (every run with recovered stats) differs from
	// every other column here (dispatched runs only). "stats runs" is their
	// denominator (len(recs with r.HasStats)), reported explicitly so
	// "budget exhausted" can be read as a proportion instead of a bare count
	// against the wrong (dispatched-only) denominator.
	// "file edit assert fail" is the files_changed/file_contains/
	// file_not_contains failure count (FileEditAssertionFailures) — see its
	// doc comment on roleSummary for why it is reported here, visibly, and
	// NOT folded into guard viol: a guard violation is a safety-floor
	// breach, this is "the requested edit did not land", and a lazy run
	// that skips the edits must not be able to hide behind a clean-looking
	// guard/duration row.
	fmt.Fprintf(&b, "| agent_type | fingerprint | cases | dispatched | assert pass | assert fail | mentions hit | not_mentions viol | avg tokens | avg ms (dispatched) | stats runs | tool_calls p50 | tool_calls max | budget exhausted | no_writes viol | guard viol | file edit assert fail | dispatch err |\n")
	fmt.Fprintf(&b, "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range s.Roles {
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %.1f%% | %.1f%% | %.1f%% | %.1f%% | %.0f | %.0f | %d | %.1f | %d | %d | %d | %d | %d | %d |\n",
			r.AgentType, r.Fingerprint, r.Cases, r.DispatchedRuns,
			r.AssertionPassRate*100, r.AssertionFailRate*100,
			r.MentionsHitRate*100, r.NotMentionsViolationRate*100,
			r.AvgTokens, r.AvgDurationMS,
			r.StatsRuns, r.ToolCallsP50, r.ToolCallsMax, r.BudgetExhaustedRuns,
			r.NoWritesViolations, r.GuardViolations, r.FileEditAssertionFailures, r.DispatchErrors)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// compare
// ---------------------------------------------------------------------------

func runEvalCompareCmd(out io.Writer, beforePath, afterPath string) error {
	before, err := loadEvalSummary(beforePath)
	if err != nil {
		return fmt.Errorf("load %s: %w", beforePath, err)
	}
	after, err := loadEvalSummary(afterPath)
	if err != nil {
		return fmt.Errorf("load %s: %w", afterPath, err)
	}
	report, err := renderEvalCompare(before, after)
	if err != nil {
		return err
	}
	fmt.Fprint(out, report)
	return nil
}

func loadEvalSummary(path string) (evalSummary, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return evalSummary{}, err
	}
	var s evalSummary
	if err := json.Unmarshal(data, &s); err != nil {
		return evalSummary{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, nil
}

// Thresholds from docs/AGENT_CAPABILITY_DESIGN.md §8's "M5-3 前置任务" /
// "指标" table. These are compare-time gates, independent of any one role's
// derived absolute threshold (the design doc's per-role table is these
// formulas evaluated against the specific 2026-09-06-glm-5.3 baseline).
const (
	evalMentionsDropTolerancePt = 0.08 // metric #2: mentions hit rate >= before - 8pt
	evalAvgDurationMaxRatio     = 1.5  // metric #5: avg duration <= 1.5x before -> else RECHECK
	evalDispatchErrorSlack      = 1    // metric #6: dispatch errors <= before+1 -> else RECHECK
	evalMinDispatchedForValid   = 7    // metric #6: dispatched runs < 7 -> INVALID (undersampled)
)

// renderEvalCompare implements docs/AGENT_CAPABILITY_DESIGN.md §8's six
// per-role gates plus a combined mentions check across every role present
// in both before and after (the heading reports that count — originally
// fixed at 5, now computed, since the corpus has grown past the original
// five roles — see combinedRoleCount below). It returns an
// error (rather than merely warning) when Model or Runs differ between
// before and after — those make the two summaries structurally
// not-comparable, per the design doc's "compare 必须校验前两项一致".
//
// Timeout is the third field the design doc asks compare to validate, but a
// summary.json produced before this revision has no such field at all
// (decodes as ""): treating a MISSING Timeout as a hard mismatch would make
// every pre-revision baseline permanently uncomparable, so a missing value
// on either side is reported as a WARNING, not an error. Once M5-3's `before`
// is regenerated with this harness (task 6 of the same brief), both sides
// carry a real Timeout and the check is exact.
func renderEvalCompare(before, after evalSummary) (string, error) {
	if before.Model != after.Model {
		return "", fmt.Errorf("eval compare: model mismatch: before=%q after=%q (not comparable)", before.Model, after.Model)
	}
	if before.Runs != after.Runs {
		return "", fmt.Errorf("eval compare: runs mismatch: before=%d after=%d (not comparable)", before.Runs, after.Runs)
	}
	timeoutUnknown := before.Timeout == "" || after.Timeout == ""
	if !timeoutUnknown && before.Timeout != after.Timeout {
		return "", fmt.Errorf("eval compare: timeout mismatch: before=%q after=%q (not comparable)", before.Timeout, after.Timeout)
	}

	byRole := map[string]roleSummary{}
	for _, r := range before.Roles {
		byRole[r.AgentType] = r
	}

	var b strings.Builder
	fmt.Fprintf(&b, "deepai eval compare: before=%s after=%s\n\n", before.GeneratedAt, after.GeneratedAt)
	if timeoutUnknown {
		fmt.Fprintf(&b, "WARNING: timeout unknown for before (%q) or after (%q) — missing --timeout predates this harness revision; skipping the timeout consistency check.\n\n", before.Timeout, after.Timeout)
	}

	var combinedHitsBefore, combinedTotalBefore int
	var combinedHitsAfter, combinedTotalAfter int
	var combinedRoleCount int

	for _, a := range after.Roles {
		bRole, ok := byRole[a.AgentType]
		fmt.Fprintf(&b, "## %s\n", a.AgentType)
		if !ok {
			fmt.Fprintf(&b, "  (no before-baseline row for this agent_type)\n\n")
			continue
		}
		combinedRoleCount++
		combinedHitsBefore += bRole.MentionsHits
		combinedTotalBefore += bRole.MentionsTotal
		combinedHitsAfter += a.MentionsHits
		combinedTotalAfter += a.MentionsTotal

		if bRole.Fingerprint != "" && bRole.Fingerprint == a.Fingerprint {
			fmt.Fprintf(&b, "  WARNING: fingerprint unchanged (%s) — this is not a real before/after comparison.\n", a.Fingerprint)
		}

		var isFail, isRecheck, isInvalid bool

		// 1. mentions hit rate >= before - 8pt
		mentionsThreshold := bRole.MentionsHitRate - evalMentionsDropTolerancePt
		status := "PASS"
		if a.MentionsHitRate < mentionsThreshold {
			status = "FAIL"
			isFail = true
		}
		fmt.Fprintf(&b, "  [%s] mentions hit rate:      %.1f%% -> %.1f%%  (threshold >= %.1f%%)\n", status, bRole.MentionsHitRate*100, a.MentionsHitRate*100, mentionsThreshold*100)

		// 3. not_mentions violation rate <= before
		status = "PASS"
		if a.NotMentionsViolationRate > bRole.NotMentionsViolationRate {
			status = "FAIL"
			isFail = true
		}
		fmt.Fprintf(&b, "  [%s] not_mentions violation: %.1f%% -> %.1f%%  (threshold <= %.1f%%)\n", status, bRole.NotMentionsViolationRate*100, a.NotMentionsViolationRate*100, bRole.NotMentionsViolationRate*100)

		// 4. guard violations == 0 (a fixed safety floor, not before-relative)
		status = "PASS"
		if a.GuardViolations != 0 {
			status = "FAIL"
			isFail = true
		}
		fmt.Fprintf(&b, "  [%s] guard violations:        %d -> %d  (threshold = 0)\n", status, bRole.GuardViolations, a.GuardViolations)

		// 5. avg duration ratio <= 1.5x -> else RECHECK (never FAIL: duration
		// is a noisy proxy, see the design doc's "耗时超标时不直接判定不达标")
		if bRole.AvgDurationMS > 0 {
			ratioVal := a.AvgDurationMS / bRole.AvgDurationMS
			status = "PASS"
			if ratioVal > evalAvgDurationMaxRatio {
				status = "RECHECK"
				isRecheck = true
			}
			fmt.Fprintf(&b, "  [%s] avg ms (dispatched):     %.0f -> %.0f  (%.2fx, threshold <= %.1fx)\n", status, bRole.AvgDurationMS, a.AvgDurationMS, ratioVal, evalAvgDurationMaxRatio)
		} else {
			fmt.Fprintf(&b, "  [PASS] avg ms (dispatched):     %.0f -> %.0f  (before baseline is 0; ratio undefined)\n", bRole.AvgDurationMS, a.AvgDurationMS)
		}

		// 6. dispatch errors <= before+1 -> else RECHECK; dispatched runs
		// must be >= 7 or the round is undersampled and unjudgeable.
		if a.DispatchedRuns < evalMinDispatchedForValid {
			status = "INVALID"
			isInvalid = true
		} else {
			status = "PASS"
			if a.DispatchErrors > bRole.DispatchErrors+evalDispatchErrorSlack {
				status = "RECHECK"
				isRecheck = true
			}
		}
		fmt.Fprintf(&b, "  [%s] dispatch timeouts:      %d -> %d  (dispatched=%d, threshold <= %d)\n", status, bRole.DispatchErrors, a.DispatchErrors, a.DispatchedRuns, bRole.DispatchErrors+evalDispatchErrorSlack)

		switch {
		case bRole.AvgTokens == 0 && a.AvgTokens == 0:
			fmt.Fprintf(&b, "  avg tokens: usage unavailable\n")
		case bRole.AvgTokens > 0:
			fmt.Fprintf(&b, "  avg tokens: %.0f -> %.0f (%.2fx)\n", bRole.AvgTokens, a.AvgTokens, a.AvgTokens/bRole.AvgTokens)
		default:
			fmt.Fprintf(&b, "  avg tokens: %.0f -> %.0f\n", bRole.AvgTokens, a.AvgTokens)
		}
		fmt.Fprintf(&b, "  assertion pass rate (diagnostic only, not a gate): %.1f%% -> %.1f%%\n", bRole.AssertionPassRate*100, a.AssertionPassRate*100)

		verdict := "pass"
		switch {
		case isInvalid:
			verdict = "invalid"
		case isFail:
			verdict = "fail"
		case isRecheck:
			verdict = "recheck"
		}
		fmt.Fprintf(&b, "  VERDICT: %s\n\n", verdict)
	}

	combinedBeforeRate := ratio(combinedHitsBefore, combinedTotalBefore)
	combinedAfterRate := ratio(combinedHitsAfter, combinedTotalAfter)
	combinedStatus := "PASS"
	if combinedAfterRate < combinedBeforeRate {
		combinedStatus = "FAIL"
	}
	fmt.Fprintf(&b, "## combined (%d roles)\n  [%s] mentions hit rate: %.2f%% (%d/%d) -> %.2f%% (%d/%d)  (threshold >= before)\n",
		combinedRoleCount, combinedStatus, combinedBeforeRate*100, combinedHitsBefore, combinedTotalBefore, combinedAfterRate*100, combinedHitsAfter, combinedTotalAfter)

	return b.String(), nil
}
