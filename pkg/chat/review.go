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
	// -uall is required, not an optimization: the default (-unormal) collapses
	// a wholly-new directory into ONE entry for the directory itself
	// ("?? internal/notify/"). That entry then travels through attribution
	// into the review scope as if it were a file — pkg/agent's
	// buildContextFilesBlock fails the whole task on it ("could not read ...:
	// is a directory"), so a turn that created a new package could not be
	// reviewed at all — and the new files inside it were never listed
	// individually, so nothing in there was ever reviewed on its own merits.
	// Cost: in a repo with a large untracked-but-unignored tree this lists
	// every such file; .gitignore still applies, so the common case is
	// unaffected.
	out, err := runGit(dir, "status", "--porcelain", "-z", "-uall")
	if err != nil {
		return worktreeSnapshot{}
	}
	entries := make(map[string]fileStamp)
	for _, e := range parsePorcelainZ(out) {
		stamp := fileStamp{status: e.status}
		if info, err := os.Stat(filepath.Join(root, e.path)); err == nil {
			// Belt to -uall's braces: anything git still reports as a
			// directory (a submodule, or a future porcelain shape) cannot be
			// diffed or read, so it must never enter attribution.
			if info.IsDir() {
				continue
			}
			stamp.size = info.Size()
			stamp.modTime = info.ModTime().UnixNano()
		}
		// A stat failure is NOT a skip: that is what a file the turn deleted
		// looks like, and a deletion is exactly the kind of change review
		// exists for.
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

// WorktreeSnapshot and TakeWorktreeSnapshot are thin exported wrappers around
// worktreeSnapshot/takeWorktreeSnapshot/changedSince, added for M5-2's
// `deepai eval agents` harness (pkg/commands/agent_eval.go), which needs the
// exact same dirty-worktree detection the review gate uses for its
// no-writes-during-review invariant so a case's `no_writes` assertion is
// judged by production logic instead of a second, possibly-diverging copy.
//
// A full rename of worktreeSnapshot/takeWorktreeSnapshot/changedSince to
// exported names would touch this file plus review_test.go,
// review_gate_test.go and review_prompt_test.go (4 files) — over the
// implementation brief's 3-file threshold for a "pure rename" — so this adds
// wrapper symbols instead of renaming the existing ones. No existing
// identifier below this point changes; this is purely additive.
type WorktreeSnapshot worktreeSnapshot

// TakeWorktreeSnapshot is the exported form of takeWorktreeSnapshot.
func TakeWorktreeSnapshot(dir string) WorktreeSnapshot {
	return WorktreeSnapshot(takeWorktreeSnapshot(dir))
}

// ChangedSince is the exported form of worktreeSnapshot.changedSince.
func (s WorktreeSnapshot) ChangedSince(prev WorktreeSnapshot) []string {
	return worktreeSnapshot(s).changedSince(worktreeSnapshot(prev))
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

// resolveWorktreePath canonicalizes a path the way git reports paths: with
// symlinks evaluated. `git rev-parse --show-toplevel` always returns the REAL
// path (on macOS a /var/... work dir comes back as /private/var/...), while
// the REPL's WorkDir and the edit records carry whatever the user's cwd was —
// possibly the symlinked form. Mixing the two forms breaks attribution
// SILENTLY and in the worst direction: filepath.Rel(root, path) yields a
// "../.." path that matches no snapshot entry, so isUntracked says "tracked"
// for a brand-new file, and `git diff -- <new file>` reports nothing — the
// reviewer would then be handed an EMPTY diff and pass a change it never saw.
//
// A path that no longer exists (a file the turn deleted) canonicalizes
// through its parent directory instead.
func resolveWorktreePath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	dir, base := filepath.Split(p)
	if resolved, err := filepath.EvalSymlinks(filepath.Clean(dir)); err == nil {
		return filepath.Join(resolved, base)
	}
	return p
}

func resolveWorktreePaths(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = resolveWorktreePath(p)
	}
	return out
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
	rel, err := filepath.Rel(s.root, resolveWorktreePath(absPath))
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

// DefaultReviewTimeout bounds one reviewer run when ReplConfig.ReviewTimeout
// is unset. Exported because pkg/commands resolves config.yaml's
// "0 = default" contract before building ReplConfig and used to carry its own
// copy of the value — two constants that had to be kept in step by hand.
//
// It is a LAST-RESORT net, not the reviewer's workload bound: the
// clock expiring kills the run and throws away everything it found, so the
// real bound is reviewMaxToolCalls below, which degrades gracefully into a
// verdict. Sized to be reached only by a genuinely stuck run — 5 minutes was
// routinely hit by an honest reviewer on a reasoning model (minutes of
// thinking per turn), which cost the whole review.
const DefaultReviewTimeout = 10 * time.Minute

// reviewMaxToolCalls bounds the reviewer's workload where exhaustion is
// RECOVERABLE: react.go turns the last call into a forced tool-less wrap-up
// that must still satisfy the Strict output schema, so the gate gets a real
// verdict for the part of the change the reviewer did examine. The profile
// itself stays uncapped (types_config.go) — a direct `task` call to this agent
// type has no wall clock to race, only the gate does.
//
// 20 covers reading every hunk's surroundings plus a build/test to
// substantiate a charge, for the change sizes rung (a) admits at all.
//
// pkg/agent's defaultReviewerMaxToolCalls is deliberately the same number —
// the two review routes had no reason to differ — but it is a SEPARATE
// constant, not an alias: this one is sized against the input the rungs above
// admit, that one is the fallback for a reviewer the model dispatches with no
// budget of its own. If one of those justifications changes, only that one
// moves.
//
// Known trade-off: pkg/agent's schema-validation retry gives a retry only the
// REMAINING tool-call budget and skips the retry entirely when none is left
// (subagent.go), so a reviewer that both exhausts this cap AND then emits
// invalid JSON gets no second attempt — it fail-softs as "verdict
// unparseable". Accepted: that path needs two failures at once, against a
// wall-clock expiry that loses the review every single time it happens.
const reviewMaxToolCalls = 20

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
	r.reviewPrev = nil

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
		// runEpisode reads ONLY next. escalate is always "" outside a
		// mission, and passed is the mission loop's business: an ordinary
		// episode has no third exit to take (R17).
		fixMsg := r.reviewGate(parentCtx, initialRequest, before, round).next
		if fixMsg == "" {
			return nil
		}
		r.turn++
		fix := fixMsg
		turn = func(ctx context.Context) error { return r.runTurn(ctx, fix, nil, false) }
	}
}

// gateResult is what one gate decision produced. It replaced a bare string
// because the mission loop needs a THIRD exit the string could not express
// (R17): "the plan itself is wrong, go back to design". Splitting passed out
// fixes a second conflation the string had — next=="" used to mean pass,
// nothing to review, fail-soft AND round cap all at once, so a mission would
// have reported a turn that changed nothing as a completed, reviewed task
// (R31).
type gateResult struct {
	// next is the synthesized input for another round (a fix, a scope
	// revert, an idle nudge), or "" when this phase does not continue on
	// its own.
	next string
	// escalate names the escalation signal (escalateFaultLayer /
	// escalateRepeatFile / escalateScope), or "" for none. Always "" outside
	// a mission.
	escalate string
	// passed is true ONLY when a correctness reviewer actually returned a
	// pass verdict on a non-empty change set. Nothing else may be reported
	// as a reviewed success.
	passed bool
}

// reviewGate decides, after a completed turn, whether the episode continues
// with a fix round. Outside a mission it returns the synthesized fix message
// in next, or an empty gateResult when the episode is over — pass, nothing to
// review, gate disabled, any fail-soft path, or the round cap presenting
// unresolved issues to the user. Every non-reviewed outcome that leaves
// changes behind warns explicitly: the user must never mistake an unreviewed
// change for a reviewed one.
//
// A mission's implementation phase does NOT come through here: its gate
// (missionReviewGate) is called directly by runImplementPhase, and only by
// it. That is deliberate. The mission gate produces two outcomes this
// function's callers cannot act on — an escalation back to design, and a
// terminal "reviewed and passed" — so reaching it from runEpisode would
// compute those decisions and then drop them on the floor: the mission
// would spend idle rounds on ordinary conversation, lose an escalation the
// reviewer actually raised, and never record a pass it actually got.
func (r *ChatRepl) reviewGate(parentCtx context.Context, initialRequest string, before worktreeSnapshot, round int) gateResult {
	// r.planMode is read AFTER the turn (the post-turn readback may have
	// entered plan mode mid-turn); plan-mode turns are read-only in intent
	// and their gate is skipped defensively (design §4.2).
	if !r.cfg.ReviewAfterEdit || r.planMode {
		return gateResult{}
	}
	after := takeWorktreeSnapshot(r.cfg.WorkDir)

	// Attribution = tool records ∪ snapshot delta (design §4.1). The
	// snapshot side catches bash-mediated edits (go fmt, sed -i, scripts);
	// the tool side survives non-git directories and snapshot failures.
	// Both sides must be in the same path form or the same file appears twice
	// in the scope (once per form) and its untracked/tracked classification
	// flips: changedSince already reports git-canonical paths, so the tool
	// records are canonicalized to match.
	scope := unionSorted(resolveWorktreePaths(r.carry.EditedFiles()), after.changedSince(before))
	if len(scope) == 0 {
		return gateResult{}
	}
	if after.root == "" && !r.reviewNonGitWarned {
		r.reviewNonGitWarned = true
		r.ui.Info("  review: not a git worktree — bash-side edits are invisible to attribution, and reviewer writes cannot be detected")
	}

	verdict, ok := r.dispatchReview(parentCtx, initialRequest, scope, after, r.reviewPrev)
	if !ok {
		r.reviewPrev = nil
		return gateResult{} // fail-soft; dispatchReview already warned
	}
	if isPassVerdict(verdict) {
		r.carry.ClearEditedFiles()
		r.reviewPrev = nil
		r.ui.Info("  review: pass — " + verdictSummary(verdict))
		return gateResult{passed: true}
	}
	if round >= maxReviewRounds {
		r.reviewPrev = nil
		r.presentIssues(fmt.Sprintf(
			"  review: STILL FAILING after %d fix rounds — human judgment needed. Unresolved issues:", maxReviewRounds), verdict)
		return gateResult{}
	}
	// Carried into the next round's reviewer so it verifies these findings
	// instead of re-deriving the whole review from scratch — and so a finding
	// the implementer rebutted is judged on the rebuttal's merits rather than
	// silently re-reported in different words.
	r.reviewPrev = verdict
	r.ui.Info(fmt.Sprintf("  review: %d issue(s) — entering fix round %d/%d", len(verdict.Issues), round+1, maxReviewRounds))
	return gateResult{next: synthesizeFixMessage(round+1, verdict)}
}

// dispatchReview runs the degradation ladder and the reviewer for one scope.
// Shared by the automatic gate and the manual /review command; ok=false is
// always fail-soft and already warned.
func (r *ChatRepl) dispatchReview(parentCtx context.Context, initialRequest string, scope []string, snap worktreeSnapshot, prev *agent.ReviewResult) (*agent.ReviewResult, bool) {
	diff, oversized := buildReviewDiff(r.cfg.WorkDir, snap, scope)
	if oversized {
		r.ui.Info(fmt.Sprintf("  review: change set exceeds %dKB of diff — NOT reviewed; consider reviewing in smaller batches", reviewDiffByteCap>>10))
		return nil, false
	}
	// context_files may only ever contain readable regular files.
	// buildContextFilesBlock fails the WHOLE task on any path it cannot read
	// — deliberately, since a model naming a wrong path should hear about it
	// (see its doc) — so one unreadable entry in the scope costs the entire
	// review. The scope legitimately contains such entries: every file the
	// turn DELETED is in it. Those paths stay in the diff and in the file
	// list, which is where a deletion belongs anyway; they just cannot be
	// attached as content.
	contextFiles := readableFiles(scope)
	// Inside a mission the review's anchor is the CHARTER, not the latest
	// thing anyone said (§5.3/D1): brief + acceptance + scope + the locked
	// plan. That is also what makes rule 3a in the correctness reviewer'"'"'s
	// prompt active — it opens the fault_layer="design" door only when a
	// locked charter is actually present in the message, so a plain /review
	// is unaffected.
	var charter *Charter
	var lockedPlan string
	if r.mission != nil && r.mission.state.Phase == missionPhaseImplement {
		charter = r.mission.charter
		lockedPlan = r.mission.lockedPlan()
	}
	// Degradation rung (b): when the full-text bundle would blow the
	// subagent context cap (a hard task failure, not a truncation), drop
	// context_files and let the read-only reviewer pull what it needs.
	if contextBundleBytes(contextFiles) > reviewContextBundleCap {
		contextFiles = nil
	}
	return r.runReview(parentCtx, reviewPromptInput{
		initialRequest: initialRequest,
		diff:           diff,
		scope:          relToWorkDir(r.cfg.WorkDir, scope),
		bundled:        contextFiles != nil,
		prev:           prev,
		charter:        charter,
		lockedPlan:     lockedPlan,
	}, contextFiles, snap)
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
func (r *ChatRepl) runReview(parentCtx context.Context, in reviewPromptInput, contextFiles []string, preReview worktreeSnapshot) (*agent.ReviewResult, bool) {
	timeout := r.cfg.ReviewTimeout
	if timeout <= 0 {
		timeout = DefaultReviewTimeout
	}
	in.maxToolCalls = reviewMaxToolCalls
	in.timeout = timeout

	args := map[string]any{
		"description": "Adversarial correctness review",
		"agent_type":  string(agent.AgentTypeCorrectnessReviewer),
		"prompt":      buildReviewPrompt(in),
		// The reviewer's profile is uncapped; the GATE caps it, because only
		// the gate races a wall clock. Exhausting this cap forces a tool-less
		// wrap-up that still has to satisfy the output schema, so a reviewer
		// that would otherwise have been killed mid-browse still returns a
		// verdict for what it examined.
		"max_tool_calls": reviewMaxToolCalls,
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
		// The reviewer's own deadline is the most common failure here and the
		// only one the user can act on, so name the window that ran out and
		// where to widen it instead of reporting a bare
		// "context deadline exceeded".
		if errors.Is(execErr, context.DeadlineExceeded) {
			r.ui.Info(fmt.Sprintf(
				"  review: reviewer hit its %s deadline — changes are unreviewed (raise review_timeout in config.yaml, or narrow the change)",
				timeout))
			return nil, false
		}
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

// reviewPromptInput is everything the reviewer's seed message is built from.
// A struct rather than a parameter list because the fields are independent
// switches (was the bundle attached? is this a re-review?) that read far
// better named at the call site than as positional bools.
type reviewPromptInput struct {
	// initialRequest is the episode's original user request — the fixed
	// anchor across all rounds, never a synthesized fix message.
	initialRequest string
	diff           string
	// scope is the change set, workdir-relative, so the reviewer knows the
	// full file list even when the diff is the only content it received.
	scope []string
	// bundled reports whether the changed files' full contents were attached
	// as context_files (degradation rung (b) drops them on large changes).
	bundled bool
	// prev is the previous round's failing verdict; nil on a first review.
	prev *agent.ReviewResult
	// charter/lockedPlan are set only for a mission's implementation review.
	// They replace "whatever the user last said" with the thing the change
	// is actually contracted to do, and they are what activates the
	// correctness reviewer's conditional rule 3a.
	charter    *Charter
	lockedPlan string
	// maxToolCalls/timeout are the reviewer's real operating budget. Told to
	// it explicitly: a reviewer that does not know it is on a clock browses
	// until the clock kills it, which loses the entire review.
	maxToolCalls int
	timeout      time.Duration
}

// buildReviewPrompt assembles the reviewer's seed message. Deliberately
// absent: the implementer's reasoning and the session history (design §4.4
// information isolation) — everything here is either the user's own words,
// the change itself, or the review machinery's own previous output.
func buildReviewPrompt(in reviewPromptInput) string {
	var b strings.Builder
	b.WriteString("Adversarially review the code changes below.\n\n")
	if in.charter != nil {
		b.WriteString(renderCharterForReview(in.charter, in.lockedPlan))
	} else {
		b.WriteString("## Original task (verbatim user request)\n\n")
		b.WriteString(in.initialRequest)
	}
	if len(in.scope) > 0 {
		b.WriteString("\n\n## Files changed\n\n")
		for _, f := range in.scope {
			b.WriteString("- " + f + "\n")
		}
	}
	b.WriteString("\n## Changes\n\n```diff\n")
	b.WriteString(in.diff)
	b.WriteString("\n```\n")
	if !in.bundled {
		b.WriteString("\n(Full file contents are NOT attached — read what you need with read_file, preferring line ranges around the hunks above.)\n")
	}
	if in.prev != nil && len(in.prev.Issues) > 0 {
		// A re-review's job is narrower than a first review: check the fixes.
		// Without this the next reviewer starts from zero, which both wastes
		// its budget re-deriving the same findings and lets it drift onto new
		// nitpicks while the reported defect goes unverified.
		b.WriteString("\n## Previously reported on this change (round now being re-reviewed)\n\n")
		writeIssueList(&b, in.prev.Issues)
		b.WriteString("\nThe implementer has since either fixed each of these or argued it is not a real problem. " +
			"For each one, decide independently whether it still holds: report it again ONLY if you can still construct " +
			"the failure scenario against the current code. Then look for defects the fixes themselves introduced.\n")
	}
	if in.maxToolCalls > 0 || in.timeout > 0 {
		b.WriteString("\n## Your budget\n\n")
		if in.maxToolCalls > 0 {
			fmt.Fprintf(&b, "- At most %d tool calls. Past that you get one final turn with NO tools, in which you must still emit the verdict JSON.\n", in.maxToolCalls)
		}
		if in.timeout > 0 {
			fmt.Fprintf(&b, "- %s of wall clock for the whole review. Running out kills the review and your findings are lost, so emit your verdict while you still have room.\n", in.timeout)
		}
		b.WriteString("- Reason from the diff first; spend tool calls only on questions the diff alone cannot settle.\n")
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

// readableFiles keeps only the paths that exist as regular files. Returns nil
// (not an empty slice) when nothing qualifies, so `contextFiles != nil` still
// answers "was a bundle attached".
func readableFiles(paths []string) []string {
	var out []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, p)
	}
	return out
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
			timeout = DefaultReviewTimeout
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
	verdict, ok := r.dispatchReview(parentCtx, r.lastUserRequest(), scope, snap, nil)
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
			if strings.HasPrefix(m.Content, "[adversarial-review") || strings.HasPrefix(m.Content, missionMessagePrefix) {
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
	// Same-form comparison as isUntracked: a symlinked WorkDir against a
	// git-canonical path otherwise makes every path look outside the tree and
	// renders as an absolute path.
	root := resolveWorktreePath(workDir)
	for i, p := range paths {
		if rel, err := filepath.Rel(root, resolveWorktreePath(p)); err == nil && !strings.HasPrefix(rel, "..") {
			out[i] = rel
		} else {
			out[i] = p
		}
	}
	return out
}

// renderCharterForReview is the review prompt's charter block. The
// fault_layer sentence is repeated here on purpose (R1): the system prompt's
// rule 3a states the exception, and this restates it right beside the
// charter it applies to, because rule 3 immediately above it says in plain
// language that anything outside the diff is out of scope — which is exactly
// what "the plan chose the wrong interface" looks like.
func renderCharterForReview(c *Charter, lockedPlan string) string {
	var b strings.Builder
	b.WriteString("## Locked charter (the stated task — this outranks anything else in the conversation)\n\n")
	b.WriteString("### Original brief (verbatim user request)\n\n")
	b.WriteString(strings.TrimSpace(c.Brief))
	b.WriteString("\n\n### Acceptance criteria the change must satisfy\n\n")
	for i, a := range c.Acceptance {
		fmt.Fprintf(&b, "%d. %s\n", i+1, a)
	}
	b.WriteString("\n### In-scope files\n\n")
	for _, f := range c.ScopeFiles {
		b.WriteString("- " + f + "\n")
	}
	if plan := strings.TrimSpace(lockedPlan); plan != "" {
		b.WriteString("\n### The plan being implemented\n\n")
		if len(plan) > reviewCharterPlanCap {
			b.WriteString(plan[:reviewCharterPlanCap])
			b.WriteString("\n(plan truncated)\n")
		} else {
			b.WriteString(plan)
			b.WriteString("\n")
		}
	}
	b.WriteString("\nJudge the change against BOTH the brief and the plan. If the change faithfully implements the plan but the plan cannot satisfy the brief — wrong interface, a file that must change but is not in scope, an acceptance criterion that cannot hold in this codebase — report that with fault_layer=\"design\" and name the charter clause that cannot hold. That is in scope here, and it is the only way the plan itself can be corrected.\n")
	return b.String()
}

// reviewCharterPlanCap bounds the locked plan inside the review prompt. The
// plan is already capped at 64KiB by write_plan; this keeps the charter
// block from crowding out the diff, which is what the reviewer is actually
// there to read.
const reviewCharterPlanCap = 24 << 10
