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
// Zero production behavior changes ship with this file. The one production
// change this period is the additive export in pkg/chat/review.go
// (WorktreeSnapshot/TakeWorktreeSnapshot/ChangedSince) so the `no_writes`
// assertion below reuses the exact dirty-worktree detection the review gate
// uses, instead of a second copy that could silently diverge from it.
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

	pool, skillReg, err := buildEvalStack(modelRegistry, repoRoot, cfg)
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
	records, runErr := runEvalCases(ctx, pool, repoRoot, skillReg, cases, opts, func(rec runRecord) error {
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
func buildEvalStack(modelRegistry *llm.ModelRegistry, repoRoot string, cfg Config) (evalTaskPool, *skill.Registry, error) {
	registry := tools.NewRegistry()
	mustRegisterTool(registry, builtin.BashTool())
	mustRegisterTool(registry, clarification.AskClarificationToolWithMode(cfg.IsAutonomous()))
	for _, t := range builtin.FileTools() {
		mustRegisterTool(registry, t)
	}

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

	return pool, skillReg, nil
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
	Error          string            `json:"error,omitempty"`
	// ToolCalls/LLMTurns/MaxToolCalls/BudgetExhausted are the run's workload
	// profile, copied from task.Stats (see subagent.RunStats) so a later
	// tool-call budget can be sized from real distributions instead of
	// guesswork — this period only collects and summarizes them, see
	// buildEvalSummary's ToolCallsP50/ToolCallsMax.
	ToolCalls       int  `json:"tool_calls"`
	LLMTurns        int  `json:"llm_turns"`
	MaxToolCalls    int  `json:"max_tool_calls"`
	BudgetExhausted bool `json:"budget_exhausted"`
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
func runEvalCases(ctx context.Context, pool evalTaskPool, repoRoot string, skillReg *skill.Registry, cases []evalCase, opts evalOptions, onRun func(runRecord) error) ([]runRecord, error) {
	var records []runRecord
	for _, c := range cases {
		fingerprint, err := caseFingerprint(c.Manifest.AgentType, repoRoot, skillReg)
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

	dispatchCtx := ctx
	var cancel context.CancelFunc
	if opts.Timeout > 0 {
		dispatchCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
	}
	task, dispatchErr := dispatchEvalTask(dispatchCtx, pool, c, contextFiles, opts.Budget)
	if cancel != nil {
		cancel()
	}

	after := chat.TakeWorktreeSnapshot(worktree)
	changed := after.ChangedSince(before)

	restore()

	rec := runRecord{
		Case:           c.ID,
		AgentType:      c.Manifest.AgentType,
		Run:            run,
		Fingerprint:    fingerprint,
		WriteViolation: len(changed) > 0,
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
	rec.Assertions = evaluateCase(c.Manifest, task.Result, task.Stats, task.Usage, rec.WriteViolation)
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
}

// dispatchEvalTask calls pool.StartTask+Wait, recovering any panic from
// either call into a plain error. This is what makes "even a panicking fake
// subagent must not leak a bad cwd" provable in a test: the panic never
// escapes this function, so the caller's chdir-restore always runs on the
// normal return path, not by accident of unwinding.
func dispatchEvalTask(ctx context.Context, pool evalTaskPool, c evalCase, contextFiles []string, budget int) (task *subagent.Task, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic dispatching case %s/%s: %v", c.AgentType, c.ID, r)
		}
	}()
	started, startErr := pool.StartTask(ctx, "eval:"+c.AgentType+"/"+c.ID, c.Manifest.Task, subagent.SubagentConfig{
		AgentType:    c.Manifest.AgentType,
		ContextFiles: contextFiles,
		TokenBudget:  budget,
	})
	if startErr != nil {
		return nil, startErr
	}
	completed, waitErr := pool.Wait(ctx, started.ID)
	if waitErr != nil {
		// Wait bailed on its own ctx (opts.Timeout in runOneCase), not
		// because the task reached a terminal state — per Pool.Wait's
		// contract the task entry is deliberately left in the pool so it
		// can keep running (Pool.runTask's runCtx derives from the same ctx
		// StartTask received, so the subagent is already unwinding from
		// cancellation, just not done unwinding yet). Recover its Stats if
		// the pool can still give them to us; this is the run.jsonl's
		// right-tail sample for a tool-call budget, so it's worth a short,
		// bounded wait rather than losing it outright.
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

func evaluateCase(m caseManifest, output string, stats *subagent.RunStats, usage *subagent.TokenUsage, writeViolation bool) []assertionResult {
	var results []assertionResult

	for _, exp := range m.Expect {
		key, val, ok := singleKV(exp)
		if !ok {
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
		}
	}
	return results
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

// projectAgentYAMLMinimal reads just enough of a project .deepai/agents/<t>.yaml
// to compute a fingerprint — the same file loadAgentYAML (pkg/agent/yaml_loader.go)
// parses in full for actual execution. Parsing it a second time here (rather
// than exporting a helper from pkg/agent) is deliberate: this period allows
// no production exports beyond the pkg/chat snapshot wrapper.
type projectAgentYAMLMinimal struct {
	SystemPrompt     string   `yaml:"system_prompt"`
	SystemPromptFile string   `yaml:"system_prompt_file"`
	Skills           []string `yaml:"skills"`
	// OutputSchema mirrors yamlAgentConfig.OutputSchema (pkg/agent/
	// yaml_loader.go) — a name into agent.NamedSchema, not inline JSON
	// Schema. Absent (""), the role's OutputSchema for fingerprint purposes
	// falls back to the builtin's mounted schema (mergeConfig's "nil
	// override keeps base" semantics), matching production.
	OutputSchema string `yaml:"output_schema"`
}

// resolveEvalSystemPrompt returns the effective system prompt, Skills list,
// and OutputSchema.Prompt for agentType exactly as SubagentExecutor.Execute
// would resolve them: a project .deepai/agents/<type>.yaml, if present, wins
// outright for SystemPrompt/Skills (matches mergeConfig's "override present
// -> replace" semantics for a non-builtin type); the schema Prompt follows
// mergeConfig's OutputSchema rule specifically — a project YAML's own
// output_schema: key replaces the base's, but an ABSENT key keeps the base
// (builtin) schema rather than clearing it, which is why the builtin lookup
// runs unconditionally below rather than only in the no-project-YAML branch.
//
// Known gap: this does not consider a project .deepai/agents/<type>.md
// override — none of the five M5-2 baseline types (architect,
// product-manager, researcher, analyst, tester) has one, only tester.yaml,
// so the gap is inert for this corpus but would need closing before a
// project MD-based role joins the eval corpus.
func resolveEvalSystemPrompt(agentType, repoRoot string) (string, []string, string, error) {
	builtinCfg := agent.GetAgentTypeConfig(agent.AgentType(agentType))
	baseSchemaPrompt := ""
	if builtinCfg.OutputSchema != nil {
		baseSchemaPrompt = builtinCfg.OutputSchema.Prompt
	}

	path := filepath.Join(repoRoot, ".deepai", "agents", agentType+".yaml")
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var y projectAgentYAMLMinimal
		if yerr := yaml.Unmarshal(data, &y); yerr != nil {
			return "", nil, "", fmt.Errorf("parse %s: %w", path, yerr)
		}
		prompt := y.SystemPrompt
		if prompt == "" && y.SystemPromptFile != "" {
			fdata, ferr := os.ReadFile(filepath.Join(repoRoot, ".deepai", "agents", y.SystemPromptFile))
			if ferr != nil {
				return "", nil, "", fmt.Errorf("read system_prompt_file for %s: %w", agentType, ferr)
			}
			prompt = string(fdata)
		}
		schemaPrompt := baseSchemaPrompt
		if y.OutputSchema != "" {
			schema, ok := agent.NamedSchema(y.OutputSchema)
			if !ok {
				return "", nil, "", fmt.Errorf("%s: unknown output_schema %q", path, y.OutputSchema)
			}
			schemaPrompt = schema.Prompt
		}
		return prompt, y.Skills, schemaPrompt, nil
	case os.IsNotExist(err):
		return builtinCfg.SystemPrompt, builtinCfg.Skills, baseSchemaPrompt, nil
	default:
		return "", nil, "", fmt.Errorf("read %s: %w", path, err)
	}
}

// caseFingerprint = sha256(resolved SystemPrompt + preloaded skill bodies,
// concatenated in Skills order + OutputSchema.Prompt)[:8], hex-encoded — the
// M5-2 brief's fingerprint definition.
//
// M5-3 mounted a non-Strict OutputSchema on architect/product-manager/
// researcher/analyst, so the schema-Prompt component is no longer
// unconditionally "" the way the M5-2 baseline comment here used to say —
// resolveEvalSystemPrompt now resolves it per role (builtin mount, or a
// project YAML's own output_schema: override) instead of this function
// hardcoding it away. The M5-2-era before/after runs (kept only on the
// machine that ran them; eval/results/ is not tracked) were computed with
// the OLD (schema-blind) formula; this fix does not retroactively recompute
// them —
// it protects fingerprints computed FROM HERE ON (M5-4 and later), so that a
// schema change with the system prompt held byte-for-byte constant (e.g.
// adding a field to a named schema, or wiring a role's output_schema) still
// produces a different fingerprint instead of silently reusing a stale
// before-run's fingerprint to vouch for an untested contract.
func caseFingerprint(agentType, repoRoot string, skillReg *skill.Registry) (string, error) {
	systemPrompt, skillNames, schemaPrompt, err := resolveEvalSystemPrompt(agentType, repoRoot)
	if err != nil {
		return "", err
	}
	var bodies []string
	for _, name := range skillNames {
		body, err := skillReg.LoadBody(name)
		if err != nil {
			return "", fmt.Errorf("skill %q: %w", name, err)
		}
		bodies = append(bodies, body)
	}
	return computeFingerprint(systemPrompt, bodies, schemaPrompt), nil
}

func computeFingerprint(systemPrompt string, skillBodies []string, schemaPrompt string) string {
	h := sha256.New()
	h.Write([]byte(systemPrompt))
	for _, b := range skillBodies {
		h.Write([]byte(b))
	}
	h.Write([]byte(schemaPrompt))
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
			if r.WriteViolation {
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
	fmt.Fprintf(&b, "| agent_type | fingerprint | cases | dispatched | assert pass | assert fail | mentions hit | not_mentions viol | avg tokens | avg ms (dispatched) | stats runs | tool_calls p50 | tool_calls max | budget exhausted | no_writes viol | guard viol | dispatch err |\n")
	fmt.Fprintf(&b, "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range s.Roles {
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %.1f%% | %.1f%% | %.1f%% | %.1f%% | %.0f | %.0f | %d | %.1f | %d | %d | %d | %d | %d |\n",
			r.AgentType, r.Fingerprint, r.Cases, r.DispatchedRuns,
			r.AssertionPassRate*100, r.AssertionFailRate*100,
			r.MentionsHitRate*100, r.NotMentionsViolationRate*100,
			r.AvgTokens, r.AvgDurationMS,
			r.StatsRuns, r.ToolCallsP50, r.ToolCallsMax, r.BudgetExhaustedRuns,
			r.NoWritesViolations, r.GuardViolations, r.DispatchErrors)
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
// per-role gates plus the five-role combined mentions check. It returns an
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

	for _, a := range after.Roles {
		bRole, ok := byRole[a.AgentType]
		fmt.Fprintf(&b, "## %s\n", a.AgentType)
		if !ok {
			fmt.Fprintf(&b, "  (no before-baseline row for this agent_type)\n\n")
			continue
		}
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
	fmt.Fprintf(&b, "## combined (5 roles)\n  [%s] mentions hit rate: %.2f%% (%d/%d) -> %.2f%% (%d/%d)  (threshold >= before)\n",
		combinedStatus, combinedBeforeRate*100, combinedHitsBefore, combinedTotalBefore, combinedAfterRate*100, combinedHitsAfter, combinedTotalAfter)

	return b.String(), nil
}
