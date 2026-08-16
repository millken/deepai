package chat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/subagent"
)

// Turn-boundary worktree snapshots for the adversarial-review gate
// (docs/ADVERSARIAL_REVIEW_DESIGN.md §4.1-C). A snapshot taken before a
// turn (S0) and another at gate time (S1) attribute to the turn every file
// that is new or changed in S1 relative to S0. This catches edits made
// through bash (go fmt, sed -i, scripts) that the edit_file/write_file
// records are blind to, while the user's own between-turn modifications
// stay in the S0 baseline and are never attributed to the agent.
//
// Attribution is (status, size, mtime)-based rather than porcelain-status-
// based alone: a file that was already dirty in S0 and was modified again
// during the turn keeps the same "M" status in both snapshots, and only
// the stat fingerprint reveals the change. Known residual (accepted in the
// design): a file the user edits externally while the agent's turn is
// running is misattributed to the turn.

// gitCommandTimeout bounds each git invocation so a hung git (e.g. a stale
// index lock) degrades the snapshot to "unavailable" instead of blocking
// the REPL goroutine.
const gitCommandTimeout = 5 * time.Second

// worktreeSnapshot is the dirty state of a git worktree at one instant.
// The zero value (and any snapshot with root == "") means "no snapshot":
// not a git worktree, git missing, or a git invocation failed. changedSince
// on such a snapshot returns nil — the gate then falls back to tool-record
// attribution only, per the design's non-git degradation.
type worktreeSnapshot struct {
	root    string // absolute worktree toplevel
	entries map[string]fileStamp
}

// fileStamp fingerprints one dirty path. size/modTime are zero for paths
// that cannot be stat'ed (e.g. deleted from the worktree); the porcelain
// status still participates in comparison so a deletion that happens
// during a turn (M → D) is attributed.
type fileStamp struct {
	status  string
	size    int64
	modTime int64
}

// takeWorktreeSnapshot captures the dirty state of the git worktree
// containing dir. It never fails hard: any error yields the zero snapshot.
func takeWorktreeSnapshot(dir string) worktreeSnapshot {
	root, ok := gitToplevel(dir)
	if !ok {
		return worktreeSnapshot{}
	}
	out, err := runGit(dir, "status", "--porcelain", "-z")
	if err != nil {
		return worktreeSnapshot{}
	}
	entries := make(map[string]fileStamp)
	for _, e := range parsePorcelainZ(out) {
		stamp := fileStamp{status: e.status}
		if info, err := os.Stat(filepath.Join(root, e.path)); err == nil && !info.IsDir() {
			stamp.size = info.Size()
			stamp.modTime = info.ModTime().UnixNano()
		}
		entries[e.path] = stamp
	}
	return worktreeSnapshot{root: root, entries: entries}
}

// changedSince returns the absolute paths of files that are new or changed
// in s relative to prev, sorted. Either snapshot being unavailable yields
// nil: without a trustworthy baseline, snapshot attribution would blame
// the user's entire dirty tree on the turn, which is worse than degrading
// to tool-record attribution alone.
func (s worktreeSnapshot) changedSince(prev worktreeSnapshot) []string {
	if s.root == "" || prev.root == "" || s.root != prev.root {
		return nil
	}
	var changed []string
	for path, stamp := range s.entries {
		if before, ok := prev.entries[path]; !ok || before != stamp {
			changed = append(changed, filepath.Join(s.root, path))
		}
	}
	sort.Strings(changed)
	return changed
}

type porcelainEntry struct {
	status string
	path   string
}

// parsePorcelainZ parses `git status --porcelain -z` output: NUL-separated
// "XY path" records, where rename/copy records (R or C in either column)
// carry a second NUL-terminated field holding the origin path — consumed
// and ignored here, since only the current path exists in the worktree.
func parsePorcelainZ(out []byte) []porcelainEntry {
	fields := bytes.Split(out, []byte{0})
	var entries []porcelainEntry
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 || f[2] != ' ' {
			continue
		}
		status := string(f[:2])
		entries = append(entries, porcelainEntry{status: status, path: string(f[3:])})
		if status[0] == 'R' || status[0] == 'C' || status[1] == 'R' || status[1] == 'C' {
			i++ // skip the origin-path field
		}
	}
	return entries
}

func gitToplevel(dir string) (string, bool) {
	out, err := runGit(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", false
	}
	root := string(bytes.TrimSpace(out))
	return root, root != ""
}

func runGit(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Stderr = nil
	return cmd.Output()
}

// isUntracked reports whether absPath is an untracked ("??") file in this
// snapshot. Untracked files need special diff treatment: git diff cannot
// show them (design §4.4 N1).
func (s worktreeSnapshot) isUntracked(absPath string) bool {
	if s.root == "" {
		return false
	}
	rel, err := filepath.Rel(s.root, absPath)
	if err != nil {
		return false
	}
	stamp, ok := s.entries[filepath.ToSlash(rel)]
	return ok && stamp.status == "??"
}

// ---------------------------------------------------------------------------
// Review episode: the bounded implement→review→fix loop around runTurn
// (docs/ADVERSARIAL_REVIEW_DESIGN.md §4.4). Deliberately an OUTER loop at
// runTurn's call sites — each fix round is a complete, ordinary turn
// (persisted, memory-scheduled, r.turn-incremented), and runTurn itself
// never learns the episode exists.
// ---------------------------------------------------------------------------

// maxReviewRounds is the fix-round cap. A constant, not config: the bound
// is the safety property (design §八-2), and making it configurable would
// make "unbounded" configurable. After the cap, a still-failing verdict is
// presented to the user for human judgment — never auto-fixed further,
// never rolled back.
const maxReviewRounds = 2

// defaultReviewTimeout bounds one reviewer run when ReplConfig.ReviewTimeout
// is unset.
const defaultReviewTimeout = 5 * time.Minute

// reviewDiffByteCap is degradation rung (c) of design §六-3: a diff bigger
// than this is not reviewed at all — the user is told, loudly, instead of
// the gate silently fail-softing on an oversized context bundle.
const reviewDiffByteCap = 200 << 10

// reviewContextPerFileCap/reviewContextBundleCap mirror pkg/agent's
// buildContextFilesBlock caps (subagent_context.go): 64KiB per file
// (truncation), 256KiB per bundle (hard task failure). The gate pre-checks
// against them because exceeding the bundle cap FAILS the review task —
// precisely the biggest changes would silently go unreviewed (§六-3).
// Keep in step with pkg/agent.
const (
	reviewContextPerFileCap = 64 << 10
	reviewContextBundleCap  = 256 << 10
)

// runEpisode runs one user request as a review episode: the initial turn,
// then — when the gate demands it — up to maxReviewRounds fix turns, each
// followed by a re-review of the accumulated changes. firstTurn runs the
// episode's first turn (a plain runTurn, a continuation, or a command-body
// turn); fix rounds are always plain runTurn calls with a synthesized fix
// message that is persisted into history like any user input, carrying a
// recognizable "[adversarial-review round N/M]" prefix.
func (r *ChatRepl) runEpisode(parentCtx context.Context, initialRequest string, firstTurn func(ctx context.Context) error) *turnError {
	// A fresh user request wipes the pending-review slate: the user has seen
	// the session state as of their prompt, and attributions left over from
	// a previous (skipped or interrupted) episode must not leak into this
	// one (design §4.4 clearing rules).
	r.carry.ClearEditedFiles()

	turn := firstTurn
	for round := 0; ; round++ {
		var before worktreeSnapshot
		if r.cfg.ReviewAfterEdit {
			before = takeWorktreeSnapshot(r.cfg.WorkDir)
		}
		if turnErr := r.runTurnWithSignal(parentCtx, turn); turnErr != nil {
			// Interrupted or errored turn: its changes are incomplete, so
			// reviewing them is meaningless — the episode ends unreviewed
			// (design §4.2).
			return turnErr
		}
		fixMsg := r.reviewGate(parentCtx, initialRequest, before, round)
		if fixMsg == "" {
			return nil
		}
		r.turn++
		fix := fixMsg
		turn = func(ctx context.Context) error { return r.runTurn(ctx, fix, nil, false) }
	}
}

// reviewGate decides, after a completed turn, whether the episode continues
// with a fix round. It returns the synthesized fix message, or "" when the
// episode is over — pass, nothing to review, gate disabled, any fail-soft
// path, or the round cap presenting unresolved issues to the user. Every
// non-reviewed outcome that leaves changes behind warns explicitly: the
// user must never mistake an unreviewed change for a reviewed one.
func (r *ChatRepl) reviewGate(parentCtx context.Context, initialRequest string, before worktreeSnapshot, round int) string {
	// r.planMode is read AFTER the turn (the post-turn readback may have
	// entered plan mode mid-turn); plan-mode turns are read-only in intent
	// and their gate is skipped defensively (design §4.2).
	if !r.cfg.ReviewAfterEdit || r.planMode {
		return ""
	}
	after := takeWorktreeSnapshot(r.cfg.WorkDir)

	// Attribution = tool records ∪ snapshot delta (design §4.1). The
	// snapshot side catches bash-mediated edits (go fmt, sed -i, scripts);
	// the tool side survives non-git directories and snapshot failures.
	scope := unionSorted(r.carry.EditedFiles(), after.changedSince(before))
	if len(scope) == 0 {
		return ""
	}
	if after.root == "" && !r.reviewNonGitWarned {
		r.reviewNonGitWarned = true
		r.ui.Info("  review: not a git worktree — bash-side edits are invisible to attribution, and reviewer writes cannot be detected")
	}

	verdict, ok := r.dispatchReview(parentCtx, initialRequest, scope, after)
	if !ok {
		return "" // fail-soft; dispatchReview already warned
	}
	if isPassVerdict(verdict) {
		r.carry.ClearEditedFiles()
		r.ui.Info("  review: pass — " + verdictSummary(verdict))
		return ""
	}
	if round >= maxReviewRounds {
		r.presentIssues(fmt.Sprintf(
			"  review: STILL FAILING after %d fix rounds — human judgment needed. Unresolved issues:", maxReviewRounds), verdict)
		return ""
	}
	r.ui.Info(fmt.Sprintf("  review: %d issue(s) — entering fix round %d/%d", len(verdict.Issues), round+1, maxReviewRounds))
	return synthesizeFixMessage(round+1, verdict)
}

// dispatchReview runs the degradation ladder and the reviewer for one scope.
// Shared by the automatic gate and the manual /review command; ok=false is
// always fail-soft and already warned.
func (r *ChatRepl) dispatchReview(parentCtx context.Context, initialRequest string, scope []string, snap worktreeSnapshot) (*agent.ReviewResult, bool) {
	diff, oversized := buildReviewDiff(r.cfg.WorkDir, snap, scope)
	if oversized {
		r.ui.Info(fmt.Sprintf("  review: change set exceeds %dKB of diff — NOT reviewed; consider reviewing in smaller batches", reviewDiffByteCap>>10))
		return nil, false
	}
	// Degradation rung (b): when the full-text bundle would blow the
	// subagent context cap (a hard task failure, not a truncation), drop
	// context_files and let the read-only reviewer pull what it needs.
	contextFiles := scope
	if contextBundleBytes(scope) > reviewContextBundleCap {
		contextFiles = nil
	}
	return r.runReview(parentCtx, initialRequest, diff, contextFiles, snap)
}

func isPassVerdict(v *agent.ReviewResult) bool {
	return strings.EqualFold(v.Verdict, "pass") || len(v.Issues) == 0
}

func verdictSummary(v *agent.ReviewResult) string {
	if s := strings.TrimSpace(v.Summary); s != "" {
		return s
	}
	return "no reproducible failure scenario found"
}

// runReview dispatches one correctness-reviewer subagent through the
// existing task tool (pool, schema validation, progress events — the whole
// chain is reused; the REPL never touches the pool directly) and returns
// the parsed verdict. ok=false is the fail-soft path: interrupted, timed
// out, tool failure, tampered worktree, or unparseable output — all warned,
// none fatal (design §六-1).
func (r *ChatRepl) runReview(parentCtx context.Context, initialRequest, diff string, contextFiles []string, preReview worktreeSnapshot) (*agent.ReviewResult, bool) {
	args := map[string]any{
		"description": "Adversarial correctness review",
		"agent_type":  string(agent.AgentTypeCorrectnessReviewer),
		"prompt":      buildReviewPrompt(initialRequest, diff, contextFiles == nil),
	}
	if r.cfg.ReviewTokenBudget > 0 {
		args["token_budget"] = r.cfg.ReviewTokenBudget
	}
	if len(contextFiles) > 0 {
		files := make([]any, len(contextFiles))
		for i, f := range contextFiles {
			files[i] = f
		}
		args["context_files"] = files
	}

	var result models.ToolResult
	var execErr error
	turnErr := r.runTurnWithSignal(parentCtx, func(ctx context.Context) error {
		timeout := r.cfg.ReviewTimeout
		if timeout <= 0 {
			timeout = defaultReviewTimeout
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		ctx = subagent.WithEventSink(ctx, func(evt subagent.TaskEvent) {
			r.ui.RenderSubagentEvent(evt)
		})
		result, execErr = r.cfg.ToolRegistry.Execute(ctx, models.ToolCall{
			ID:        fmt.Sprintf("review-t%d-%d", r.turn, time.Now().UnixNano()),
			Name:      "task",
			Arguments: args,
		})
		return nil // review failures are fail-soft, never a turn error
	})

	// Reviewer-write defense runs FIRST, before any trust decision — a
	// timed-out reviewer may still have written the tree (design §4.4 B4:
	// bash is unsandboxed; this snapshot is the only hard line).
	if tampered := takeWorktreeSnapshot(r.cfg.WorkDir).changedSince(preReview); len(tampered) > 0 {
		r.ui.Info(fmt.Sprintf(
			"  review: reviewer modified the working tree (%s) — verdict DISCARDED, changes are unreviewed; inspect these files",
			strings.Join(relToWorkDir(r.cfg.WorkDir, tampered), ", ")))
		return nil, false
	}
	if turnErr != nil && turnErr.cancelled {
		r.ui.Info("  review: skipped (interrupted) — changes are unreviewed")
		return nil, false
	}
	if execErr != nil {
		r.ui.Info(fmt.Sprintf("  review: reviewer failed (%v) — changes are unreviewed", execErr))
		return nil, false
	}

	schema := agent.GetAgentTypeConfig(agent.AgentTypeCorrectnessReviewer).OutputSchema
	verdict, err := agent.ParseOutput[agent.ReviewResult](schema, result.Content)
	if err != nil {
		r.ui.Info(fmt.Sprintf("  review: verdict unparseable (%v) — changes are unreviewed", err))
		return nil, false
	}
	return verdict, true
}

// buildReviewPrompt assembles the reviewer's seed message: the episode's
// initial user request (fixed anchor across all rounds — never a synthetic
// fix message) and the scoped diff. Deliberately absent: the implementer's
// reasoning and the session history (design §4.4 information isolation).
func buildReviewPrompt(initialRequest, diff string, diffOnly bool) string {
	var b strings.Builder
	b.WriteString("Adversarially review the code changes below.\n\n")
	b.WriteString("## Original task (verbatim user request)\n\n")
	b.WriteString(initialRequest)
	b.WriteString("\n\n## Changes\n\n```diff\n")
	b.WriteString(diff)
	b.WriteString("\n```\n")
	if diffOnly {
		b.WriteString("\n(File contents omitted for size — use read_file on any file you need.)\n")
	}
	return b.String()
}

// buildReviewDiff produces the scoped change view: `git diff -- <scope>`
// for tracked files (never a bare git diff — the user's own unrelated
// uncommitted changes must stay out, design §4.4 B2), plus a
// `git diff --no-index` synthesized new-file diff per untracked file
// (design N1; `git add -N` was rejected because mutating the user's index
// violates review-has-zero-side-effects). oversized=true means rung (c):
// do not review.
func buildReviewDiff(workDir string, snap worktreeSnapshot, scope []string) (diff string, oversized bool) {
	var b strings.Builder
	if snap.root == "" {
		b.WriteString("(diff unavailable: not a git worktree — attached file contents are the full change view)\n")
	} else {
		var tracked []string
		var untracked []string
		for _, f := range scope {
			if snap.isUntracked(f) {
				untracked = append(untracked, f)
			} else {
				tracked = append(tracked, f)
			}
		}
		if len(tracked) > 0 {
			out, err := runGit(workDir, append([]string{"diff", "--"}, tracked...)...)
			if err != nil {
				b.WriteString("(git diff failed for tracked files — rely on attached file contents)\n")
			} else {
				b.Write(out)
			}
		}
		for _, f := range untracked {
			out, err := runGitNoIndexDiff(workDir, f)
			if err != nil {
				fmt.Fprintf(&b, "(new file %s: diff unavailable — see attached contents)\n", f)
				continue
			}
			b.WriteString("# new file (untracked)\n")
			b.Write(out)
		}
	}
	if b.Len() > reviewDiffByteCap {
		return "", true
	}
	return b.String(), false
}

// runGitNoIndexDiff diffs an untracked file against /dev/null. git diff
// --no-index follows diff(1) exit-code semantics — 1 means "differences
// found", which here is the success case.
func runGitNoIndexDiff(workDir, file string) ([]byte, error) {
	out, err := runGit(workDir, "diff", "--no-index", "--", os.DevNull, file)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return out, nil
		}
		return nil, err
	}
	return out, nil
}

// contextBundleBytes estimates what the scope would cost inside a subagent
// context bundle, mirroring buildContextFilesBlock's accounting: each file
// contributes at most the per-file truncation cap.
func contextBundleBytes(scope []string) int {
	total := 0
	for _, f := range scope {
		info, err := os.Stat(f)
		if err != nil {
			continue
		}
		size := int(info.Size())
		if size > reviewContextPerFileCap {
			size = reviewContextPerFileCap
		}
		total += size
	}
	return total
}

// synthesizeFixMessage formats a failing verdict as the next round's user
// input. "Fix it OR state explicitly why it is not a real problem" is
// deliberate: reviewers err too, and the implementer's rebuttal flows into
// the next review round for the reviewer to judge (design §4.4).
func synthesizeFixMessage(round int, v *agent.ReviewResult) string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"[adversarial-review round %d/%d] An independent correctness review of your changes found the following issues. For each one: either fix it, or state explicitly why it is not a real problem.\n",
		round, maxReviewRounds)
	writeIssueList(&b, v.Issues)
	return b.String()
}

// presentIssues renders a verdict's issue list to the user under the given
// header. Used both at the round cap (design §八-6: nothing is auto-fixed
// or rolled back — the findings go to the human) and by manual /review.
func (r *ChatRepl) presentIssues(header string, v *agent.ReviewResult) {
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n")
	writeIssueList(&b, v.Issues)
	r.ui.Info(b.String())
}

// handleReviewCommand implements /review: no argument runs one manual
// review of the pending edit scope (falling back to the whole dirty
// worktree — manual mode means the user explicitly asked, so their own
// uncommitted changes are fair game, design §4.5); on/off toggles the
// automatic gate for this session; status reports the configuration.
func (r *ChatRepl) handleReviewCommand(parentCtx context.Context, args string) {
	switch strings.ToLower(strings.TrimSpace(args)) {
	case "on":
		r.cfg.ReviewAfterEdit = true
		r.ui.Info("  review: automatic post-edit review ON for this session")
	case "off":
		r.cfg.ReviewAfterEdit = false
		r.ui.Info("  review: automatic post-edit review OFF for this session")
	case "status":
		state := "off"
		if r.cfg.ReviewAfterEdit {
			state = "on"
		}
		budget := "unlimited"
		if r.cfg.ReviewTokenBudget > 0 {
			budget = fmt.Sprintf("%d tokens", r.cfg.ReviewTokenBudget)
		}
		timeout := r.cfg.ReviewTimeout
		if timeout <= 0 {
			timeout = defaultReviewTimeout
		}
		r.ui.Info(fmt.Sprintf("  review: auto %s | budget %s | timeout %s | pending files %d",
			state, budget, timeout, len(r.carry.EditedFiles())))
	case "":
		r.runManualReview(parentCtx)
	default:
		r.ui.Info("  Usage: /review [on|off|status]")
	}
}

func (r *ChatRepl) runManualReview(parentCtx context.Context) {
	snap := takeWorktreeSnapshot(r.cfg.WorkDir)
	scope := r.carry.EditedFiles()
	if len(scope) == 0 {
		scope = snap.dirtyFiles()
	}
	if len(scope) == 0 {
		r.ui.Info("  review: nothing to review — no recorded edits and a clean worktree")
		return
	}
	verdict, ok := r.dispatchReview(parentCtx, r.lastUserRequest(), scope, snap)
	if !ok {
		return
	}
	if isPassVerdict(verdict) {
		r.carry.ClearEditedFiles()
		r.ui.Info("  review: pass — " + verdictSummary(verdict))
		return
	}
	r.presentIssues(fmt.Sprintf("  review: %d issue(s) found:", len(verdict.Issues)), verdict)
}

// lastUserRequest recovers the review anchor for a manual review: the most
// recent genuine user message — synthesized fix messages are skipped by
// their prefix. Falls back to a neutral instruction in an empty session.
func (r *ChatRepl) lastUserRequest() string {
	if r.sess != nil {
		for i := len(r.sess.Messages) - 1; i >= 0; i-- {
			m := r.sess.Messages[i]
			if m.Role != models.RoleHuman {
				continue
			}
			if strings.HasPrefix(m.Content, "[adversarial-review") {
				continue
			}
			if strings.TrimSpace(m.Content) != "" {
				return m.Content
			}
		}
	}
	return "Review the current uncommitted changes on their own merits."
}

// dirtyFiles lists every dirty path in the snapshot (tracked modifications
// and untracked files alike), as absolute sorted paths.
func (s worktreeSnapshot) dirtyFiles() []string {
	if s.root == "" || len(s.entries) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.entries))
	for p := range s.entries {
		out = append(out, filepath.Join(s.root, p))
	}
	sort.Strings(out)
	return out
}

func writeIssueList(b *strings.Builder, issues []agent.Issue) {
	for i, is := range issues {
		fmt.Fprintf(b, "\n%d. [%s] %s:%d — %s\n", i+1, is.Severity, is.File, is.Line, is.Message)
		if is.Scenario != "" {
			fmt.Fprintf(b, "   failure scenario: %s\n", is.Scenario)
		}
		if is.Suggestion != "" {
			fmt.Fprintf(b, "   suggestion: %s\n", is.Suggestion)
		}
	}
}

// unionSorted merges two path sets, deduplicated and sorted.
func unionSorted(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(a)+len(b))
	for _, p := range a {
		set[p] = struct{}{}
	}
	for _, p := range b {
		set[p] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// relToWorkDir renders paths relative to the working directory for display.
func relToWorkDir(workDir string, paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		if rel, err := filepath.Rel(workDir, p); err == nil && !strings.HasPrefix(rel, "..") {
			out[i] = rel
		} else {
			out[i] = p
		}
	}
	return out
}
