package chat

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/subagent"
)

// prPreflightTimeout bounds the host-side go vet / go test pre-flight run so
// slow or stuck test suites never dominate review latency.
const prPreflightTimeout = 45 * time.Second

// prShardFileThreshold and prShardByteThreshold decide when a full PR review
// fans out across parallel module shards instead of handing everything to a
// single sequential reviewer.
const (
	prShardFileThreshold = 10
	prShardByteThreshold = 48 << 10
	prMaxShards          = 4
)

// prSeverityRank maps issue severities to a numeric rank for threshold checks.
// Unknown/unrecognized severities default to medium (blocking) so a typo in a
// model verdict never silently suppresses a real finding.
func prSeverityRank(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 2
	}
}

func (r *ChatRepl) prMinBlockingSeverity() string {
	switch strings.ToLower(strings.TrimSpace(r.cfg.PRReviewMinSeverity)) {
	case "critical", "high", "medium", "low":
		return strings.ToLower(strings.TrimSpace(r.cfg.PRReviewMinSeverity))
	default:
		return "medium"
	}
}

// prHasBlockingIssues reports whether v should keep the PR review loop open.
//
// Rules (order matters):
//  1. Any issue at or above PRReviewMinSeverity (default "medium") blocks,
//     even when the model labels the verdict "pass" — convergence prompts
//     sometimes emit pass while still listing real findings.
//  2. A non-pass verdict with an empty issues slice always blocks (parked
//     summary finding, or a malformed empty fail that must not silently close
//     the loop).
//  3. Issues strictly below the threshold (e.g. low-only under default medium)
//     do not block; the loop posts them as advisory notes instead.
func (r *ChatRepl) prHasBlockingIssues(v *agent.ReviewResult) bool {
	if v == nil {
		return false
	}
	if len(v.Issues) == 0 {
		if strings.EqualFold(strings.TrimSpace(v.Verdict), "pass") {
			return false
		}
		// fail/unknown + empty issues: always block (parked summary OR empty payload).
		return true
	}
	minRank := prSeverityRank(r.prMinBlockingSeverity())
	for _, is := range v.Issues {
		if prSeverityRank(is.Severity) >= minRank {
			return true
		}
	}
	return false
}

// isPRPassVerdict reports whether a PR review verdict closes the review loop.
// Severity gating always wins over the model's verdict string: "pass" with
// blocking-severity issues still fails, and "fail" with only below-threshold
// nits (default: low) still passes as advisory.
func (r *ChatRepl) isPRPassVerdict(v *agent.ReviewResult) bool {
	if v == nil {
		return false
	}
	return !r.prHasBlockingIssues(v)
}

// prLocalWorktreeAligned reports whether the worktree is safe to use as a
// stand-in for the remote PR tip: HEAD is on expectedBranch and equals
// origin/<branch> (so we never review unpushed, unpushed-local-only, or stale
// commits while claiming to review the PR).
func prLocalWorktreeAligned(workDir, expectedBranch string) bool {
	branch := strings.TrimSpace(expectedBranch)
	if workDir == "" || branch == "" {
		return false
	}
	cur, err := runGit(workDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || strings.TrimSpace(string(cur)) != branch {
		return false
	}
	remoteRef := "origin/" + branch
	if _, err := runGit(workDir, "rev-parse", "--verify", "--quiet", remoteRef); err != nil {
		// Without a local remote-tracking ref we cannot confirm HEAD matches
		// the remote PR tip (e.g. unpushed commits or non-origin remote);
		// fall back to gh.Diff rather than risking diff drift.
		return false
	}
	head, err1 := runGit(workDir, "rev-parse", "HEAD")
	remote, err2 := runGit(workDir, "rev-parse", remoteRef)
	if err1 != nil || err2 != nil {
		return false
	}
	return strings.TrimSpace(string(head)) == strings.TrimSpace(string(remote))
}

// prLocalDiff computes the PR diff from the local git repo against the PR's
// base branch (preferring origin/<base>, falling back to <base>) without a
// slow network `gh pr diff` round-trip. Returns ok=false whenever the base
// ref cannot be resolved locally or the branch does not match, so callers
// safely fall back to gh.Diff.
func prLocalDiff(workDir, baseBranch string) (diff string, files []string, ok bool) {
	if workDir == "" || strings.TrimSpace(baseBranch) == "" {
		return "", nil, false
	}
	// Resolve base ref: origin/<base> first (most up-to-date remote tracking
	// ref), then local <base>.
	var baseRef string
	for _, cand := range []string{"origin/" + baseBranch, baseBranch} {
		if _, err := runGit(workDir, "rev-parse", "--verify", "--quiet", cand); err == nil {
			baseRef = cand
			break
		}
	}
	if baseRef == "" {
		return "", nil, false
	}
	mbOut, err := runGit(workDir, "merge-base", baseRef, "HEAD")
	if err != nil {
		return "", nil, false
	}
	mb := strings.TrimSpace(string(mbOut))
	if mb == "" {
		return "", nil, false
	}
	diffOut, err := runGit(workDir, "diff", "--unified=3", mb+"..HEAD")
	if err != nil {
		return "", nil, false
	}
	namesOut, err := runGit(workDir, "diff", "--name-only", mb+"..HEAD")
	if err != nil {
		return "", nil, false
	}
	for _, f := range strings.Split(string(namesOut), "\n") {
		if f = strings.TrimSpace(f); f != "" {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return "", nil, false
	}
	return string(diffOut), files, true
}

// runPRPreflight runs fast deterministic Go static/unit checks (go vet +
// targeted go test -short) on the changed Go packages BEFORE invoking the LLM
// reviewer. Embedding the outcome in the prompt saves the reviewer multiple
// slow bash turns and avoids redundant full-suite test sweeps.
func runPRPreflight(parentCtx context.Context, workDir string, scope []string) string {
	if workDir == "" || len(scope) == 0 {
		return ""
	}
	if _, err := os.Stat(filepath.Join(workDir, "go.mod")); err != nil {
		return ""
	}
	pkgSet := map[string]struct{}{}
	for _, f := range scope {
		if !strings.HasSuffix(f, ".go") {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(f))
		if dir == "." || dir == "" {
			pkgSet["."] = struct{}{}
		} else {
			pkgSet["./"+strings.TrimPrefix(dir, "./")] = struct{}{}
		}
	}
	if len(pkgSet) == 0 {
		return ""
	}
	pkgs := make([]string, 0, len(pkgSet))
	for p := range pkgSet {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	if len(pkgs) > 8 {
		pkgs = pkgs[:8]
	}

	ctx, cancel := context.WithTimeout(parentCtx, prPreflightTimeout)
	defer cancel()

	var b strings.Builder
	runCmd := func(label string, args ...string) {
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = workDir
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		if err == nil {
			fmt.Fprintf(&b, "- `%s`: PASS\n", label)
			return
		}
		if ctx.Err() != nil {
			fmt.Fprintf(&b, "- `%s`: TIMED OUT after %s\n", label, prPreflightTimeout)
			return
		}
		fmt.Fprintf(&b, "- `%s`: FAIL\n```\n%s\n```\n", label, clip(strings.TrimSpace(out.String()), 3072))
	}

	vetArgs := append([]string{"vet"}, pkgs...)
	runCmd("go "+strings.Join(vetArgs, " "), vetArgs...)
	if ctx.Err() == nil {
		testArgs := append([]string{"test", "-short", "-count=1"}, pkgs...)
		runCmd("go "+strings.Join(testArgs, " "), testArgs...)
	}
	return strings.TrimSpace(b.String())
}

// prDiffShard represents one directory/package slice of a large unified diff.
type prDiffShard struct {
	name  string
	files []string
	diff  string
}

// splitUnifiedDiffByModule splits a multi-file unified diff into at most
// maxShards module-grouped shards (by top-level two-segment directory, e.g.
// pkg/schedule, pkg/bot, cmd/deepai-bot) so large PRs can be reviewed
// concurrently across independent subagents.
func splitUnifiedDiffByModule(diff string, maxShards int) []prDiffShard {
	if maxShards <= 1 {
		return nil
	}
	type fileChunk struct {
		path  string
		group string
		text  string
	}
	// Split on "\ndiff --git " boundaries while preserving leading header.
	var rawChunks []string
	const marker = "diff --git "
	idx := strings.Index(diff, marker)
	if idx < 0 {
		return nil
	}
	rest := diff[idx:]
	for {
		next := strings.Index(rest[len(marker):], "\n"+marker)
		if next < 0 {
			rawChunks = append(rawChunks, rest)
			break
		}
		cut := len(marker) + next + 1
		rawChunks = append(rawChunks, rest[:cut])
		rest = rest[cut:]
	}
	if len(rawChunks) < prShardFileThreshold {
		return nil
	}

	groups := map[string]*prDiffShard{}
	var order []string
	for _, chunk := range rawChunks {
		path := parseDiffChunkPath(chunk)
		grp := moduleGroupKey(path)
		sh, ok := groups[grp]
		if !ok {
			sh = &prDiffShard{name: grp}
			groups[grp] = sh
			order = append(order, grp)
		}
		if path != "" {
			sh.files = append(sh.files, path)
		}
		sh.diff += chunk
	}
	if len(order) <= 1 {
		return nil
	}

	// Sort groups largest-first and bin-pack into at most maxShards balanced buckets.
	sort.Slice(order, func(i, j int) bool {
		return len(groups[order[i]].diff) > len(groups[order[j]].diff)
	})
	numBuckets := len(order)
	if numBuckets > maxShards {
		numBuckets = maxShards
	}
	buckets := make([]prDiffShard, numBuckets)
	for _, grp := range order {
		g := groups[grp]
		// Pick the currently smallest bucket.
		best := 0
		for i := 1; i < len(buckets); i++ {
			if len(buckets[i].diff) < len(buckets[best].diff) {
				best = i
			}
		}
		if buckets[best].name == "" {
			buckets[best].name = g.name
		} else {
			buckets[best].name += ", " + g.name
		}
		buckets[best].files = append(buckets[best].files, g.files...)
		buckets[best].diff += g.diff
	}
	return buckets
}

func parseDiffChunkPath(chunk string) string {
	line, _, _ := strings.Cut(chunk, "\n")
	// Format: diff --git a/path/to/file b/path/to/file
	fields := strings.Fields(line)
	if len(fields) >= 4 && strings.HasPrefix(fields[3], "b/") {
		return strings.TrimPrefix(fields[3], "b/")
	}
	if len(fields) >= 3 && strings.HasPrefix(fields[2], "a/") {
		return strings.TrimPrefix(fields[2], "a/")
	}
	return ""
}

func moduleGroupKey(path string) string {
	clean := filepath.ToSlash(strings.TrimPrefix(path, "./"))
	parts := strings.Split(clean, "/")
	if len(parts) >= 2 {
		return parts[0] + "/" + parts[1]
	}
	return "root"
}

// mergeShardVerdicts combines parallel shard review results into one
// ReviewResult, deduplicating identical (file, line, message) findings and
// ordering highest severity first.
func mergeShardVerdicts(results []*agent.ReviewResult) *agent.ReviewResult {
	merged := &agent.ReviewResult{
		Agent:   string(agent.AgentTypeCorrectnessReviewer),
		Verdict: "pass",
	}
	type issueKey struct {
		file string
		line int
		msg  string
	}
	seen := map[issueKey]struct{}{}
	var summaries []string
	for _, v := range results {
		if v == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(v.Verdict), "fail") {
			merged.Verdict = "fail"
		}
		if s := strings.TrimSpace(v.Summary); s != "" {
			summaries = append(summaries, s)
		}
		for _, is := range v.Issues {
			k := issueKey{file: is.File, line: is.Line, msg: strings.TrimSpace(is.Message)}
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			merged.Issues = append(merged.Issues, is)
		}
	}
	sort.SliceStable(merged.Issues, func(i, j int) bool {
		return prSeverityRank(merged.Issues[i].Severity) > prSeverityRank(merged.Issues[j].Severity)
	})
	if len(merged.Issues) > 0 {
		merged.Verdict = "fail"
	}
	if len(summaries) > 0 {
		merged.Summary = strings.Join(summaries, "; ")
	}
	return merged
}

// runShardedPRReview fans out a large PR diff across parallel module reviewer
// subagents inside a SINGLE runTurnWithSignal wrapper so UI flushing and
// Ctrl+C signal handling stay single-threaded on the REPL goroutine while all
// shard LLM subagents run concurrently in the tool registry / subagent pool.
func (r *ChatRepl) runShardedPRReview(parentCtx context.Context, base reviewPromptInput, shards []prDiffShard, preReview worktreeSnapshot) (*agent.ReviewResult, reviewOutcome) {
	r.ui.Info(fmt.Sprintf("  pr: #%d large diff (%d files, %dKB) — running %d parallel module reviewers",
		base.prNumber, len(base.scope), len(base.diff)>>10, len(shards)))

	timeout := r.cfg.ReviewTimeout
	if timeout <= 0 {
		timeout = DefaultReviewTimeout
	}
	effort := strings.TrimSpace(r.cfg.ReviewReasoningEffort)
	if effort == "" {
		effort = defaultReviewReasoningEffort
	}

	calls := make([]models.ToolCall, len(shards))
	for i, sh := range shards {
		in := base
		in.diff = sh.diff
		in.scope = sh.files
		in.maxToolCalls = r.scaleReviewToolBudget(len(sh.files))
		in.timeout = timeout
		ctxFiles := readableFiles(absPaths(r.cfg.WorkDir, sh.files))
		if contextBundleBytes(ctxFiles) > reviewContextBundleCap {
			ctxFiles = nil
		}
		in.bundled = len(ctxFiles) > 0
		args := map[string]any{
			"description":      fmt.Sprintf("Adversarial correctness review (%s)", sh.name),
			"agent_type":       string(agent.AgentTypeCorrectnessReviewer),
			"prompt":           buildReviewPrompt(in),
			"reasoning_effort": effort,
			"max_tool_calls":   in.maxToolCalls,
		}
		if r.cfg.ReviewTokenBudget > 0 {
			args["token_budget"] = r.cfg.ReviewTokenBudget
		}
		if m := strings.TrimSpace(r.cfg.ReviewModel); m != "" {
			args["model"] = m
		}
		if len(ctxFiles) > 0 {
			files := make([]any, len(ctxFiles))
			for j, f := range ctxFiles {
				files[j] = f
			}
			args["context_files"] = files
		}
		calls[i] = models.ToolCall{
			ID:        fmt.Sprintf("review-shard%d-t%d-%d", i, r.turn, time.Now().UnixNano()),
			Name:      "task",
			Arguments: args,
		}
	}

	schema := agent.GetAgentTypeConfig(agent.AgentTypeCorrectnessReviewer).OutputSchema
	results := make([]models.ToolResult, len(shards))
	errs := make([]error, len(shards))
	parseErrs := make([]error, len(shards))
	verdicts := make([]*agent.ReviewResult, len(shards))
	pending := make([]int, len(shards))
	for i := range shards {
		pending[i] = i
	}

	// One transient retry, matching runReview: deadline hits and worktree
	// tampering stay terminal and abort retries immediately; other transient or
	// parse failures get a single second chance per failing shard.
	for attempt := 0; attempt < 2 && len(pending) > 0; attempt++ {
		batch := append([]int(nil), pending...)
		pending = pending[:0]
		te := r.runTurnWithSignal(parentCtx, func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			var uiMu sync.Mutex
			ctx = subagent.WithEventSink(ctx, func(evt subagent.TaskEvent) {
				uiMu.Lock()
				r.ui.RenderSubagentEvent(evt)
				uiMu.Unlock()
			})
			var wg sync.WaitGroup
			for _, idx := range batch {
				wg.Add(1)
				go func(i int, c models.ToolCall) {
					defer wg.Done()
					results[i], errs[i] = r.cfg.ToolRegistry.Execute(ctx, c)
				}(idx, calls[idx])
			}
			wg.Wait()
			return nil
		})
		r.ui.FlushSubagentBlock()

		// Reviewer-write defense runs after every attempt before any retry or trust decision.
		if tampered := takeWorktreeSnapshot(r.cfg.WorkDir).changesSince(preReview); len(tampered) > 0 {
			r.ui.Info(fmt.Sprintf(
				"  review: reviewer modified the working tree (%s) — verdict DISCARDED, changes are unreviewed; inspect these files",
				strings.Join(relToWorkDir(r.cfg.WorkDir, tampered), ", ")))
			return nil, reviewFailedTerminal
		}
		if te != nil && te.cancelled {
			r.ui.Info("  review: skipped (interrupted) — changes are unreviewed")
			return nil, reviewInterrupted
		}

		// If any shard hit its deadline (terminal), abort retries immediately so
		// we don't burn another full timeout retrying sibling shards.
		hasDeadline := false
		for _, idx := range batch {
			if errs[idx] != nil && isReviewDeadline(errs[idx]) {
				hasDeadline = true
				break
			}
		}
		if hasDeadline {
			break
		}

		for _, idx := range batch {
			parseErrs[idx] = nil
			verdicts[idx] = nil
			if errs[idx] != nil {
				if attempt == 0 {
					r.ui.Info(fmt.Sprintf("  review: shard %s failed (%v) — retrying once", shards[idx].name, errs[idx]))
					pending = append(pending, idx)
				}
				continue
			}
			v, err := agent.ParseOutput[agent.ReviewResult](schema, results[idx].Content)
			if err != nil {
				parseErrs[idx] = err
				if attempt == 0 {
					r.ui.Info(fmt.Sprintf("  review: shard %s verdict unparseable (%v) — retrying once", shards[idx].name, err))
					pending = append(pending, idx)
				}
				continue
			}
			verdicts[idx] = v
		}
	}

	for i := range shards {
		if errs[i] != nil {
			if isReviewDeadline(errs[i]) {
				r.ui.Info(fmt.Sprintf("  review: shard %s hit its %s deadline — changes are unreviewed", shards[i].name, timeout))
				return nil, reviewFailedTerminal
			}
			r.ui.Info(fmt.Sprintf("  review: shard %s failed (%v) — changes are unreviewed", shards[i].name, errs[i]))
			return nil, reviewFailedTransient
		}
		if parseErrs[i] != nil {
			r.ui.Info(fmt.Sprintf("  review: shard %s verdict unparseable (%v) — changes are unreviewed", shards[i].name, parseErrs[i]))
			return nil, reviewFailedTransient
		}
	}
	return mergeShardVerdicts(verdicts), reviewOK
}
