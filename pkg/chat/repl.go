package chat

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/imageproc"
	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/memory"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/sandbox"
	"github.com/millken/deepai/pkg/skill"
	"github.com/millken/deepai/pkg/subagent"
	"github.com/millken/deepai/pkg/tools"
)

// ReplConfig holds configuration for the chat REPL.
type ReplConfig struct {
	Provider       string
	ModelRegistry  *llm.ModelRegistry
	DatabaseURL    string
	ContextWindow  int
	MaxToolCalls   int
	RequestTimeout time.Duration
	Temperature    *float64 // global fallback; models[].temperature wins per alias
	Query          string   // non-interactive single query
	ResumeSession  string   // session ID or title to resume
	ContinueLast   bool     // resume most recent session
	// ContinueAny widens ContinueLast from "latest session in WorkDir" back
	// to the old, unscoped "latest session anywhere" (models.SessionRepository
	// .Latest()). Off by default: an unscoped -c is what let a `deepai -c` in
	// one repo silently resume a DIFFERENT repo's session and interleave with
	// whatever process was still running there.
	ContinueAny bool
	// ForceSession steals a session lock held by another live deepai process
	// (see models.ErrSessionLocked). Dangerous if that process is still
	// running — the two will then both append messages to the same session,
	// which is the exact seq-interleaving incident this locking feature
	// exists to prevent. Only meant for a holder that is confirmed stuck.
	ForceSession bool
	// ForkSession, when the resolved session is locked by another live
	// process, copies its history into a brand new session and continues
	// there instead of failing — the original session and its lock are left
	// untouched.
	ForkSession         bool
	SystemPrompt        string
	WorkDir             string
	ToolRegistry        *tools.Registry
	SkillRegistry       *skill.Registry
	MemoryService       *memory.Service
	MemoryExtractor     memory.Extractor
	PreferenceExtractor memory.Extractor
	SessionRepo         models.SessionRepository // injected from outside
	InputHistoryFile    string                   // path for persisting input history (optional)
	// TaskCanceller stops a single running subagent by ID. Supplied by the
	// composition root, which owns the pool. nil disables per-task
	// cancellation (Ctrl+C still cancels the whole turn).
	TaskCanceller  TaskCanceller
	SandboxBaseDir string // root for sandbox session dirs; must NOT be the user's workdir
	MCPReport      string // one-line MCP load summary; printed after banner when non-empty
	AgentCatalog   []agent.AgentInfo
	Commands       map[string]Command // file-based slash commands; body injected as a user turn

	// MemoryAutoRefine enables the auto-refine review gate. When false the REPL
	// falls back to unconditional extraction rather than skipping memory work.
	MemoryAutoRefine bool
	// MemoryRefineInterval is the gate cadence in turns, already resolved by the
	// caller (see commands.resolveRefineInterval); 0 means "no gate".
	MemoryRefineInterval int

	// ReviewAfterEdit enables the adversarial post-edit review gate
	// (docs/ADVERSARIAL_REVIEW_DESIGN.md). Default off — the gate blocks the
	// turn synchronously and spends reviewer tokens, so it is an explicit
	// opt-in until the detection-rate baseline justifies flipping the
	// default (design §八-1).
	ReviewAfterEdit bool
	// ReviewTokenBudget caps each review subagent's total tokens; 0 = unlimited.
	ReviewTokenBudget int
	// ReviewTimeout bounds one review subagent run; 0 uses DefaultReviewTimeout.
	ReviewTimeout time.Duration
}

// fallbackExtractInterval is the turn cadence for unconditional async memory
// extraction — the behaviour used when the auto-refine gate is switched off.
// Set to 5: covers a typical short exchange in one batch while keeping LLM
// extraction cost bounded. Compaction always flushes synchronously, so facts
// are never lost across the context boundary.
const fallbackExtractInterval = 5

// memoryScheduleMode is what a finished turn should queue for memory.
type memoryScheduleMode int

const (
	memoryScheduleNone memoryScheduleMode = iota
	// memoryScheduleRefine runs the review gate, which decides whether to pay
	// for an extraction.
	memoryScheduleRefine
	// memoryScheduleUnconditional extracts without asking, which is what the
	// REPL did before the gate existed.
	memoryScheduleUnconditional
)

// memoryScheduleFor decides what a finished turn should queue.
//
// Any interval that is not a usable cadence means "no gate", never "no memory":
// the fallback branch keeps extraction running at the pre-gate cadence. Without
// it, a config that never mentions the key would stop memory extraction outright
// rather than merely turning off an optimisation.
func memoryScheduleFor(turn, refineInterval int, autoRefine bool) memoryScheduleMode {
	if autoRefine && refineInterval > 0 {
		if turn%refineInterval == 0 {
			return memoryScheduleRefine
		}
		return memoryScheduleNone
	}
	if turn%fallbackExtractInterval == 0 {
		return memoryScheduleUnconditional
	}
	return memoryScheduleNone
}

// ReplUI is the subset of TUI methods the REPL needs. *TUI satisfies it
// implicitly. Defining it as an interface lets tests inject a mock.
type ReplUI interface {
	Info(msg string)
	SetStatus(model string, planMode bool)
	// SetLockLost toggles a PERSISTENT, always-rendered notice that this
	// process no longer owns (or has re-confirmed owning) its session lock
	// — see onLockLost and startNewSession (D5, session-lock review round
	// 2). Unlike Info, which commits a one-off line to scrollback, this
	// must stay visible on every frame until cleared.
	SetLockLost(lost bool)
	Banner(info BannerInfo)
	AskQuestion(ctx context.Context, question string, options []string) (string, error)
	ReadPrompt(ctx context.Context) (string, []models.MessageImage, error)
	TurnStart(turn int, userInput string)
	TurnEnd(usage *agent.Usage)
	RenderEvent(evt agent.AgentEvent)
	RenderSubagentEvent(evt subagent.TaskEvent)
	RenderInterrupted()
	InterruptCh() <-chan struct{}
	CancelTaskCh() <-chan string
	LoadHistory(path string)
	SaveHistory()
	Close()
}

// TaskCanceller is the narrow slice of the subagent pool the REPL needs: stop
// one task by ID. Kept minimal so the REPL does not depend on the pool type.
type TaskCanceller interface {
	CancelTask(taskID string) bool
}

// sessionLockState bundles the two booleans that together describe this
// process's session-lock situation — writes suspended, and whether the
// user has already been notified about the CURRENT loss — behind one
// mutex, so every transition (a loss detected by the heartbeat, or a
// recovery via /new or /fork) happens as a single atomic step.
//
// Round 3 of the session-lock review (L-A) flagged the PREVIOUS shape —
// three independent atomics (writesSuspended, lockLostNotified, plus the
// TUI's own lockLost flag reached via r.ui.SetLockLost) toggled by separate
// statements — as leaving windows where a concurrently-firing heartbeat
// tick could observe an inconsistent combination: e.g. startNewSession
// clearing writesSuspended, then a heartbeat landing before
// lockLostNotified was also cleared, then startNewSession finishing the
// reset — nothing corrupted, but a notification could be swallowed or
// (worse ordering) writes could look suspended with no banner up to
// explain why. Bundling suspended+notified behind one mutex collapses that
// window to nothing: setLost and clear each change both fields in one
// critical section, so any concurrent isSuspended() reader sees a value
// from before or after the whole transition, never partway through it.
//
// r.ui.SetLockLost(bool) itself stays a separate call after clear()/
// setLost() return — it is the TUI's own concern (documented safe for
// concurrent use from any goroutine, see onLockLost's doc comment) and has
// no data this type needs to protect. That call is NOT inside this type's
// critical section, though, so it does NOT inherit the atomicity above:
// a heartbeat's onLockLost (setLost() then SetLockLost(true)) and a REPL-
// goroutine clear() (startNewSession/forkCurrentSession finishing a
// switch, then SetLockLost(false)) can each get to their own UI call in
// either order relative to the other's mutex write, independently of
// which mutex write happened first — see heartbeatTick's doc comment (N2)
// for a concrete way this happens even without two genuinely concurrent
// goroutines. So the banner can transiently disagree with isSuspended():
// up while writes are enabled, or down while they're suspended. This is
// NOT "only ever drifts toward the safe combination" — either mismatch is
// reachable. What DOES still hold is narrower: r.lockState.isSuspended()
// itself is always internally consistent (never read mid-transition), and
// it alone is what every write path actually gates on — the banner being
// transiently wrong is a user-visible annoyance, never a path to writing
// into a session this process doesn't hold.
type sessionLockState struct {
	mu        sync.Mutex
	suspended bool
	notified  bool
}

// setLost marks writes suspended and this loss notified, atomically, and
// reports whether THIS call is the one that transitioned notified from
// false to true. Callers use that to fire onLockLost's user-facing
// notification exactly once per loss — RefreshSessionLock keeps returning
// the same definitive error every tick once the lock is truly gone, so
// without this guard the heartbeat goroutine would re-notify (and
// re-render the banner) forever.
func (s *sessionLockState) setLost() (firstNotification bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.notified {
		return false
	}
	s.suspended = true
	s.notified = true
	return true
}

// clear resets both flags together, restoring normal operation — called by
// startNewSession/forkCurrentSession once a fresh lock is confirmed held on
// the session being switched to. A later loss on THAT session must still be
// reported, so this re-arms notified rather than leaving it permanently
// tripped (see TestLockLost_FullRecoveryPath_SuspendsThenNewSessionRestores).
func (s *sessionLockState) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.suspended = false
	s.notified = false
}

// isSuspended reports whether writes are currently suspended — the single
// check every session-level write/delete path in the REPL makes before
// touching r.sessMgr for r.sess.ID (appendMessage, saveSession,
// clearSession, undoLastTurn, the /title command, and generateTitle's
// SetTitle calls). appendMessage/saveSession stay silent no-ops when this
// is true (matching their existing, already-reviewed contract — a turn's
// output still needs to render even though it can't be saved); every OTHER
// caller must show the user an explicit rejection instead (H-A, round 3):
// a delete or rename that silently no-ops looks like it succeeded, which is
// worse than refusing loudly. See lockLostRejectMsg.
func (s *sessionLockState) isSuspended() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.suspended
}

// ChatRepl is the interactive chat REPL.
type ChatRepl struct {
	cfg               ReplConfig
	ui                ReplUI
	historyFile       string
	sess              *models.Session
	sessMgr           models.SessionRepository
	sb                *sandbox.Sandbox
	turn              int
	prefSched         *memory.PreferenceScheduler
	consecCorrections int
	planMode          bool   // restrict to read-only tools until user approves plan
	currentModel      string // selected model alias (from ModelRegistry)
	currentEffort     string // reasoning effort: "low", "medium", "high", "disabled", or "" (provider default)
	imageDetail       string // vision detail: "low" (default), "high", or "" (use "low")
	refineOverride    *bool  // session-level /refine on|off; nil defers to config

	// carry holds cross-turn Agent state (circuit breaker, active skill,
	// compaction anchors — see agent.SessionCarry's doc comment) that would
	// otherwise silently reset every turn, since runTurn builds a fresh,
	// single-use Agent per turn (M4-3, task-23-brief.md). Passed unchanged
	// into every turn's AgentConfig.Session; reset to a fresh
	// agent.NewSessionCarry() by clearSession (/clear), startNewSession
	// (/new), and undoLastTurn (/undo), alongside the state each of those
	// already invalidates. The REPL drives turns serially (one runTurn
	// completes before the next begins) EXCEPT on the orphan path (see
	// orphanWaitOrDefault) — do not share this pointer with anything that
	// could run concurrently with a turn (e.g. a subagent).
	carry *agent.SessionCarry

	// reviewNonGitWarned makes the "not a git worktree" review-gate warning
	// (degraded attribution, no reviewer-write detection) fire once per
	// session instead of once per edited turn.
	reviewNonGitWarned bool

	// reviewPrev is the failing verdict from the previous review round of the
	// CURRENT episode, replayed to the next round's reviewer so a re-review
	// verifies those findings against the fixed code instead of starting from
	// zero. Cleared when an episode starts and whenever one ends (pass, round
	// cap, or fail-soft) — a stale verdict leaking into the next episode
	// would have the reviewer chase findings about a different change.
	reviewPrev *agent.ReviewResult

	// orphanWait overrides the orphan-turn wait (see orphanWaitOrDefault)
	// for tests. Zero (the field's default in every real ChatRepl, since
	// NewRepl never sets it) means "use the production default" — this
	// mirrors the Agent.streamIdleTimeout pattern (pkg/agent/react.go): a
	// field only tests reach into directly, never exposed via ReplConfig.
	orphanWait time.Duration

	// lockOwner identifies this OS process for session_locks (see
	// models.SessionRepository.AcquireSessionLock). PID/host are constant
	// for the process's lifetime, so resolveSession computes this once and
	// every later Acquire/Refresh/Release reuses the same value — a fresh
	// os.Hostname() call per lock op would be pointless and, worse, could
	// theoretically disagree with itself across calls.
	lockOwner models.LockOwner

	// lockedSessionID is the ID of the session this process currently holds
	// the lock on, read by Run()'s heartbeat/release goroutine. It exists
	// so that goroutine never has to read r.sess.ID: r.sess is read and
	// written all over the REPL goroutine with no synchronization (see
	// carry's doc comment above for the same pattern), so a second
	// goroutine reading r.sess.ID concurrently with e.g. startNewSession's
	// r.sess = sess is a data race — one go test -race could not previously
	// catch because no test exercised /new while a heartbeat goroutine was
	// running (see repl_lock_test.go's TestStartNewSession_NoRaceWithHeartbeat).
	// Updated via setLockedSession every place the REPL switches which
	// session holds the lock: resolveSession, acquireOrHandleLock,
	// startNewSession.
	lockedSessionID atomic.Pointer[string]

	// lockHeartbeatInterval overrides sessionLockHeartbeatInterval for
	// tests, mirroring the orphanWait field immediately above: zero (every
	// real ChatRepl's default, since NewRepl never sets it) means "use the
	// production interval."
	lockHeartbeatInterval time.Duration

	// lockState is this process's session-lock situation (writes
	// suspended? has the current loss been notified yet?), touched by the
	// heartbeat goroutine (onLockLost) and read/reset by the REPL goroutine
	// (appendMessage, saveSession, clearSession, undoLastTurn, the /title
	// command, generateTitle, startNewSession, forkCurrentSession). See
	// sessionLockState's doc comment for why this replaced the three
	// independent atomics (writesSuspended/lockLostNotified fields plus
	// ui.SetLockLost) a previous review round used.
	//
	// round 3 (H-B/M-A) also DELETED the "escalate a run of transient
	// RefreshSessionLock failures to a lock loss" heuristic that used to
	// live alongside this field (lockRefreshFailures, and the threshold
	// check in heartbeatTick) — see heartbeatTick's doc comment for why.
	lockState sessionLockState
}

// setLockedSession records id as the session currently locked by this
// process. A fresh copy of id is stored — never a pointer into r.sess —
// so the atomic value's lifetime never depends on the models.Session object
// the REPL goroutine may go on to mutate or replace out from under it.
func (r *ChatRepl) setLockedSession(id string) {
	r.lockedSessionID.Store(&id)
}

// holdsSession reports whether this process may still write to session id
// — both that writes are not currently suspended (r.lockState.isSuspended())
// AND that id is still the session r.lockedSessionID names, not one this
// process has since switched away from via /new or /fork.
//
// N1 (session-lock review, mechanical-cleanup pass): a background goroutine
// that captures a sessionID up front (generateTitle) cannot use
// r.lockState.isSuspended() alone as its later recheck — /new and /fork
// both call r.lockState.clear() as part of a successful switch, which
// resets isSuspended() to false for the NEW session even though the
// goroutine's captured id names the OLD one it no longer owns. Comparing
// against r.lockedSessionID closes that gap: a nil pointer or a mismatched
// id both count as "no longer held," regardless of the global suspended
// flag's current value.
func (r *ChatRepl) holdsSession(id string) bool {
	if r.lockState.isSuspended() {
		return false
	}
	idPtr := r.lockedSessionID.Load()
	return idPtr != nil && *idPtr == id
}

// lockHeartbeatIntervalOrDefault returns r.lockHeartbeatInterval if a test
// has set it, else sessionLockHeartbeatInterval.
func (r *ChatRepl) lockHeartbeatIntervalOrDefault() time.Duration {
	if r.lockHeartbeatInterval > 0 {
		return r.lockHeartbeatInterval
	}
	return sessionLockHeartbeatInterval
}

// heartbeatTick refreshes this process's lock on whichever session
// r.lockedSessionID currently names. Called by Run()'s heartbeat goroutine
// once per tick; pulled out to its own method (rather than inlined in the
// goroutine closure) so a test can call it directly, on demand, against the
// exact same field reads a running Repl uses — including racing it against
// startNewSession from another goroutine under go test -race — without
// needing a full Run() (real TTY, TUI, banner) just to exercise this path.
//
// lost reports whether this process has DEFINITIVELY lost the lock — i.e.
// RefreshSessionLock returned models.ErrLockNotHeld, meaning the row itself
// says someone else holds it now (most likely: force-stolen, or the row
// was deleted out from under it, e.g. by `deepai session delete` racing
// this process — see onLockLost's message). Any OTHER error is transient
// contention, not proof of loss: RefreshSessionLock is a bare UPDATE
// backed only by busy_timeout(5000), and a second writer on the same DB
// holding a transaction open past that window — another `deepai -c`
// appending its own messages (precisely the scenario this feature exists
// to support), `deepai analyze`, memory's Save, a WAL auto-checkpoint —
// can trip it with no relation whatsoever to the lock itself.
//
// round 3 of the session-lock review (H-B/M-A) DELETED the heuristic that
// used to live here — escalating a RUN of consecutive transient failures
// to lost=true once they'd piled up past staleLockAfter/2. That heuristic
// had, at that point, produced two separate blocking-severity defects
// across two review rounds: the FIRST version treated every single
// transient error as loss and killed a live, healthy turn over an
// ordinary multi-second SQLITE_BUSY blip; the version it was replaced
// with (the escalate-after-N-failures form removed here) surfaced a NEW
// bug instead of fixing the class of bug — three consecutive transient
// failures (the lock row never actually changing hands) tripped
// writesSuspended permanently, because nothing except startNewSession ever
// cleared it, and a `/new` escape hatch that itself starts failing under
// the same transient condition (e.g. sustained SQLITE_BUSY from real -c
// contention) has no way back in. Per this repo's own established
// practice for a heuristic that measurably doesn't pay for itself (see
// e.g. the M5-4 output-contract revert, the M6-4 batching-prompt revert):
// delete it rather than patch it again. What it was trying to buy —
// catching a steal faster than waiting for the NEXT definitive signal —
// is close to nothing: RefreshSessionLock's UPDATE ... WHERE pid=? AND
// host=? already yields a DETERMINISTIC, immediate models.ErrLockNotHeld
// the moment the row's owner actually changes (RowsAffected==0 — verified
// reliable against modernc sqlite in this review round), so the "faster
// detection" a transient-failure count could add is illusory; all it ever
// reliably detected was "this DB is having a bad few seconds," which is
// not the same fact and must never be treated as if it were.
//
// The result: only models.ErrLockNotHeld is ever trustworthy. Everything
// else is retried, forever, on every tick, with no counter, no threshold,
// and no effect on lost.
func (r *ChatRepl) heartbeatTick() (lost bool, err error) {
	idPtr := r.lockedSessionID.Load()
	if idPtr == nil {
		return false, nil
	}
	id := *idPtr
	err = r.sessMgr.RefreshSessionLock(id, r.lockOwner)
	if err == nil {
		return false, nil
	}
	if errors.Is(err, models.ErrLockNotHeld) {
		// N2: this UPDATE is a bare statement with no snapshot of its
		// own — RefreshSessionLock's own busy_timeout(5000) can queue it
		// behind a concurrent /new or /fork's write transaction (creating
		// the new session, releasing id's lock) for the whole 5s window.
		// If that switch finishes first, THIS UPDATE runs against a row id
		// this process deliberately released moments ago — RowsAffected==0
		// is then a true but STALE fact about id, not evidence of loss on
		// whatever session is current now. Re-Load before trusting it: only
		// report lost when id is still what r.lockedSessionID names: if a
		// switch already moved it elsewhere, this tick is answering a
		// question nobody is asking anymore. See
		// TestHeartbeatTick_StaleIDDuringSwitchWindow_DoesNotReportLoss.
		if cur := r.lockedSessionID.Load(); cur == nil || *cur != id {
			return false, nil
		}
		// Definitive — no amount of retrying changes this.
		return true, err
	}
	// Transient. Never escalates, no matter how many times in a row this
	// fires — see the doc comment above.
	return false, err
}

// handleHeartbeatResult is Run()'s heartbeat goroutine's reaction to one
// heartbeatTick call: log every transient failure (so it is at least
// visible with -v, even though it does not by itself mean anything is
// wrong — see heartbeatTick), and react to a DEFINITIVE loss by calling
// onLockLost, which itself guards against re-notifying for a loss already
// reported (r.lockState.setLost() — see that type's doc comment). Pulled
// out of the goroutine closure, like heartbeatTick itself, purely for
// direct testability.
func (r *ChatRepl) handleHeartbeatResult(lost bool, err error) {
	if err != nil {
		slog.Warn("refresh session lock", "err", err)
	}
	if lost {
		r.onLockLost()
	}
}

// onLockLost is what handleHeartbeatResult calls every time heartbeatTick
// reports a DEFINITIVE loss (RefreshSessionLock returned
// models.ErrLockNotHeld) — which, since that error is sticky once the lock
// is truly gone, means every tick from here on until /new or /fork
// recovers. r.lockState.setLost() is what makes repeat calls a no-op past
// the first: it atomically suspends writes AND records that this loss has
// been notified, so everything below only ever runs once per loss.
//
// D5's ORIGINAL contract (session-lock review round 1) was: notify, then
// cancel Run()'s context to unwind the whole REPL. Review round 2 rejected
// that trade — cancelling ctx killed whatever turn was in flight, silently
// destroying the user's in-progress work to react to what is, from their
// perspective, a mere notification. The REVISED contract implemented here:
//
//  1. Do NOT cancel anything. The running turn (if any) keeps running to
//     completion and its result is shown normally — the user's work is not
//     the casualty of a lock notification.
//  2. Suspend ALL further persistence (r.lockState.isSuspended()), which
//     appendMessage/saveSession/clearSession/undoLastTurn/the /title
//     command/generateTitle all check before touching the DB. THIS is the
//     actual enforcement of "never interleave writes with whoever now owns
//     the session" — cancelling ctx alone never was.
//  3. Tell the user, PERSISTENTLY: once via r.ui.Info (visible immediately
//     in scrollback) and via r.ui.SetLockLost(true), which the TUI renders
//     on every single frame from here on — not a line that scrolls away —
//     until /new or /fork clears it (see startNewSession/forkCurrentSession).
//
// Recommends /fork FIRST: it is the remedy that preserves whatever this
// run has produced since the loss (including a turn's output that never
// reached the DB at all — see appendMessage's doc comment) by copying the
// in-memory transcript into a brand-new, freshly locked session. /new is
// offered as the "I don't want this session's content" alternative — see
// its own doc comment for why that discard must be stated plainly, not
// implied.
//
// r.ui.Info/.SetLockLost are safe to call from this (non-REPL) goroutine:
// *TUI's exported methods just send a message on bubbletea's Program,
// documented safe for concurrent use from any goroutine — see also the M3
// fix (Run() only starts this goroutine after r.ui is assigned, so reading
// r.ui itself here is race-free per the Go memory model's "go statement"
// rule, without needing r.ui to be atomic).
//
// Extracted to its own method so it can be unit-tested directly against a
// mock UI (TestOnLockLost_SuspendsWritesAndShowsPersistentBanner); Run()
// itself cannot be exercised in a non-interactive test process (it
// requires a real TTY, see isInteractiveTTY), so this is the finest
// granularity at which this specific reaction is independently testable.
func (r *ChatRepl) onLockLost() {
	if !r.lockState.setLost() {
		return
	}
	r.ui.Info("  ⚠ 会话锁已丢失（可能被另一个 deepai --force 接管，也可能该会话已被删除，例如另一个进程运行了 session delete/prune）。本次运行仍会正常完成并显示结果，但从现在起不会再写入磁盘，以避免与新的持有者交错写入。运行 /fork 把当前这段转录（含尚未保存的内容）保存到一个新会话并继续，或运行 /new 直接开始一个全新的空会话（未保存的内容不会带入新会话）。")
	r.ui.SetLockLost(true)
}

// defaultOrphanWait bounds how long runTurn waits for an orphaned Run
// goroutine (one that hasn't returned by the time ctx is cancelled — e.g. a
// tool ignoring ctx) before giving up and returning anyway, so a stuck tool
// can never hang the whole REPL.
const defaultOrphanWait = 10 * time.Second

// sessionLockHeartbeatInterval is how often Run() refreshes this process's
// session_locks row while it holds a session. Must be comfortably under
// staleLockAfter (pkg/chat/session.go, 60s) so a live, healthy process's
// lock never ages out from under it; 15s leaves wide margin even if a beat
// or two is missed under load.
const sessionLockHeartbeatInterval = 15 * time.Second

// orphanWaitOrDefault returns r.orphanWait if a test has set it, else
// defaultOrphanWait.
func (r *ChatRepl) orphanWaitOrDefault() time.Duration {
	if r.orphanWait > 0 {
		return r.orphanWait
	}
	return defaultOrphanWait
}

// NewRepl creates a new chat REPL instance.
func NewRepl(cfg ReplConfig) (*ChatRepl, error) {
	// The sandbox session directory must live outside the user's working
	// directory; otherwise cleanup on exit (incl. ctrl+c) could delete project
	// files — e.g. a pre-existing ./cli folder. Fall back to a temp location
	// when no isolated base is provided.
	sandboxBase := strings.TrimSpace(cfg.SandboxBaseDir)
	if sandboxBase == "" {
		sandboxBase = filepath.Join(os.TempDir(), "deepai-sandbox")
	}
	sb, err := sandbox.NewSession(sandboxBase, sandbox.Config{})
	if err != nil {
		return nil, fmt.Errorf("sandbox init: %w", err)
	}

	repl := &ChatRepl{
		cfg:          cfg,
		historyFile:  cfg.InputHistoryFile,
		sessMgr:      cfg.SessionRepo,
		sb:           sb,
		prefSched:    memory.NewPreferenceScheduler(),
		currentModel: cfg.ModelRegistry.DefaultName(),
		carry:        agent.NewSessionCarry(),
	}
	return repl, nil
}

// Run starts the interactive REPL loop. It requires an interactive terminal;
// single-query (-q) and non-TTY modes are not supported.
func (r *ChatRepl) Run(parentCtx context.Context) error {
	defer r.sb.Close()
	defer func() {
		if r.cfg.MemoryService != nil {
			r.cfg.MemoryService.CleanupStale(time.Hour)
		}
	}()

	// Resolve session.
	if err := r.resolveSession(); err != nil {
		return err
	}

	// D5 (session-lock review round 2) no longer derives a cancellable
	// context here: losing the session lock must NOT tear down a running
	// turn or unwind this loop (see onLockLost's doc comment) — it only
	// stops persistence and raises a banner. parentCtx is used exactly as
	// received from the caller (real Ctrl+C/SIGINT/SIGTERM/SIGHUP
	// cancellation, from cmd/deepai/main.go, still applies normally).

	// Heartbeat the session lock (pkg/chat/session.go) so it never crosses
	// staleLockAfter while this process is actually alive, and release it on
	// every exit path from here down — including the early "unsupported
	// mode" returns just below — so a well-behaved exit never leaves a
	// corpse lock for the next `deepai -c`/`-r` to trip over. A crash skips
	// this defer entirely; that is what the heartbeat's staleness window is
	// for; see AcquireSessionLock.
	//
	// This release-on-exit defer is registered HERE, before the -q/non-TTY
	// early returns below, so those paths still release a lock they
	// acquired even though (see below) the actual heartbeat goroutine has
	// not started yet at this point. lockHeartbeatWG starts at its zero
	// value (no Add yet) in that case, so Wait() below returns immediately
	// — safe either way.
	lockHeartbeatDone := make(chan struct{})
	var lockHeartbeatWG sync.WaitGroup
	defer func() {
		close(lockHeartbeatDone)
		lockHeartbeatWG.Wait()
		if idPtr := r.lockedSessionID.Load(); idPtr != nil {
			if err := r.sessMgr.ReleaseSessionLock(*idPtr, r.lockOwner); err != nil {
				slog.Warn("release session lock", "session", *idPtr, "err", err)
			}
		}
	}()

	// The REPL is TUI-only: it requires an interactive terminal. Single-query
	// (-q) and non-TTY (pipe/CI) modes are not supported.
	if r.cfg.Query != "" {
		return errors.New("single-query (-q) mode is no longer supported; run deepai interactively")
	}
	if !isInteractiveTTY() {
		return errors.New("deepai requires an interactive terminal (stdin and stderr must be a TTY)")
	}

	bannerInfo := r.bannerInfo()

	// Start the persistent Bubble Tea TUI for the whole session.
	tui := NewTUI(os.Stdin, os.Stderr, bannerInfo)
	tui.Start()
	r.ui = tui
	defer r.ui.Close()

	// Start the heartbeat goroutine only NOW, after r.ui is assigned (M3,
	// session-lock review round 2). onLockLost (called from this goroutine)
	// reads r.ui; r.ui is written above with no synchronization, same as
	// r.sess/r.carry elsewhere in this type (see lockedSessionID's doc
	// comment on the ChatRepl struct for the general pattern) — a second
	// goroutine reading it concurrently with that assignment would be a
	// genuine data race (the previous code started this goroutine BEFORE
	// r.ui was set, which go test -race never caught only because the 15s
	// production interval made the window vanishingly small — it would not
	// stay small if that interval were ever tightened). Starting the
	// goroutine with a `go` statement AFTER `r.ui = tui` makes this
	// race-free with no extra synchronization: the Go memory model
	// guarantees everything the parent goroutine did before a `go f()`
	// statement is visible to f when it begins. The release-on-exit defer
	// above is deliberately NOT moved down here with it — see that defer's
	// comment for why the -q/non-TTY early returns above still need it.
	lockHeartbeatWG.Add(1)
	go func() {
		defer lockHeartbeatWG.Done()
		ticker := time.NewTicker(r.lockHeartbeatIntervalOrDefault())
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				lost, err := r.heartbeatTick()
				r.handleHeartbeatResult(lost, err)
			case <-lockHeartbeatDone:
				return
			}
		}
	}()
	if r.historyFile != "" {
		r.ui.LoadHistory(r.historyFile)
	}
	defer r.ui.SaveHistory()

	// Show banner.
	r.ui.Banner(bannerInfo)
	if r.cfg.MCPReport != "" {
		r.ui.Info("  " + r.cfg.MCPReport)
	}
	r.ui.SetStatus(r.currentModel, r.planMode)

	// Interactive loop. Ctrl+C during a turn cancels only that turn (delivered
	// via the TUI interrupt channel); Ctrl+C at the prompt exits the REPL.

	// Auto-continue: if the resumed session was interrupted mid-task,
	// start the agent immediately without waiting for user input.
	autoContinue := (r.cfg.ResumeSession != "" || r.cfg.ContinueLast) && isSessionInterrupted(r.sess.Messages)

	for {
		// Auto-continue: on first iteration of an interrupted session,
		// run the agent immediately without waiting for user input.
		if autoContinue {
			autoContinue = false
			r.ui.Info("  Resuming interrupted session...")
			r.turn++
			if err := r.runEpisode(parentCtx, "Continue from where you left off.", r.continueTurn); err != nil {
				if parentCtx.Err() != nil {
					break
				}
				if err.cancelled {
					r.ui.RenderInterrupted()
					continue
				}
				r.ui.Info(fmt.Sprintf("  Error: %v", err))
			}
		}

		// Wait for user input.
		line, images, err := r.ui.ReadPrompt(parentCtx)
		if err != nil {
			if errors.Is(err, errInterrupted) {
				// Ctrl+C at prompt — exit REPL.
				r.ui.Info("  Interrupted.")
				break
			}
			// io.EOF (Ctrl+D) or context cancellation: exit quietly.
			break
		}
		if line == "" && len(images) == 0 {
			continue
		}

		// Handle slash commands.
		if cmd, ok := ParseSlashCommand(line); ok {
			// File-based command: inject its expanded body as a user turn.
			if c, ok := r.cfg.Commands[cmd.Name]; ok {
				r.turn++
				body := Expand(c.Body, cmd.Args)
				turnErr := r.runEpisode(parentCtx, body, func(ctx context.Context) error {
					return r.runTurn(ctx, body, nil, false)
				})
				if turnErr != nil {
					if turnErr.cancelled {
						r.ui.RenderInterrupted()
						continue
					}
					r.ui.Info(fmt.Sprintf("  Error: %v", turnErr))
				}
				continue
			}
			if r.handleSlashCommand(parentCtx, cmd) {
				break
			}
			continue
		}

		// Continuation input ("继续", "continue", etc.): resume agent
		// without adding a new human message.
		if isContinuationInput(line) && len(r.sess.Messages) > 0 {
			r.turn++
			// The continuation phrase is a weak review anchor, but the gate
			// still sees the full scoped diff; edits made in a continued
			// turn must not escape review.
			if err := r.runEpisode(parentCtx, "Continue from where you left off.", r.continueTurn); err != nil {
				if parentCtx.Err() != nil {
					break
				}
				if err.cancelled {
					r.ui.RenderInterrupted()
					continue
				}
				r.ui.Info(fmt.Sprintf("  Error: %v", err))
			}
			continue
		}

		r.turn++

		// Capture images for this turn (may be nil).
		turnImages := images

		turnErr := r.runEpisode(parentCtx, line, func(ctx context.Context) error {
			return r.runTurn(ctx, line, turnImages, false)
		})
		if turnErr != nil {
			if turnErr.cancelled {
				r.ui.RenderInterrupted()
				continue
			}
			r.ui.Info(fmt.Sprintf("  Error: %v", turnErr))
		}
	}

	// Save session metadata on exit.
	r.saveSession()
	slog.Info("session ended", "session_id", r.sess.ID, "turns", r.turn)
	return nil
}

// bannerInfo gathers the data shown in the startup banner and footer.
func (r *ChatRepl) bannerInfo() BannerInfo {
	toolCount := 0
	if r.cfg.ToolRegistry != nil {
		toolCount = len(r.cfg.ToolRegistry.List())
	}
	skillCount := 0
	var skillNames []string
	if r.cfg.SkillRegistry != nil {
		skillCount = r.cfg.SkillRegistry.Count()
		skillNames = r.cfg.SkillRegistry.AvailableNames()
	}
	// Resolve provider/model from the current model alias for display.
	provider, model := r.cfg.Provider, r.currentModel
	if def, ok := r.cfg.ModelRegistry.Resolve(r.currentModel); ok {
		provider = def.Provider
		model = def.Model
	}
	return BannerInfo{
		Provider:      provider,
		Model:         model,
		ModelAlias:    r.currentModel,
		ToolCount:     toolCount,
		SkillCount:    skillCount,
		SkillNames:    skillNames,
		SessionID:     r.sess.ID,
		ContextWindow: r.currentContextWindow(),
	}
}

func (r *ChatRepl) resolveSession() error {
	r.lockOwner = newLockOwner()

	// --continue-any/--fork/--force only mean anything alongside -c/-r —
	// without one of those there is no lock CONFLICT for --fork/--force to
	// remedy, and no "latest session" lookup for --continue-any to widen.
	// Silently ignoring them left a user typing `deepai --force` (say, out
	// of habit from a previous -c --force) with no idea it did nothing.
	if r.cfg.ResumeSession == "" && !r.cfg.ContinueLast {
		if r.cfg.ContinueAny || r.cfg.ForkSession || r.cfg.ForceSession {
			fmt.Fprintln(os.Stderr, "  提示：--continue-any/--fork/--force 只在搭配 -c 或 -r 时才有意义；本次未使用 -c/-r，已忽略这些参数。")
		}
	}

	// Resume by ID or title.
	if r.cfg.ResumeSession != "" {
		sess, err := r.sessMgr.Resolve(r.cfg.ResumeSession)
		if err != nil {
			return fmt.Errorf("resume session %q: %w", r.cfg.ResumeSession, err)
		}
		if err := r.acquireOrHandleLock(sess); err != nil {
			return err
		}
		// Load messages from DB into memory for the agent. r.sess may be a
		// FORK of sess (see acquireOrHandleLock), so read back by r.sess.ID,
		// never the session we originally resolved.
		msgs, err := r.sessMgr.LoadMessages(r.sess.ID)
		if err != nil {
			slog.Warn("load messages for resumed session", "err", err)
		}
		r.sess.Messages = msgs
		slog.Info("resumed session", "id", r.sess.ID, "messages", len(msgs))
		// Clean up incomplete tool calls from interrupted turn.
		r.sess.Messages = filterUnresolvedToolUses(r.sess.Messages)
		r.restoreModelFromSession()
		return nil
	}

	// Continue most recent. Scoped to this working directory by default: an
	// unscoped `-c` is what let a `deepai -c` in repo A silently resume repo
	// B's session and interleave with whatever process was still running
	// there. --continue-any opts back into the old, unscoped Latest().
	if r.cfg.ContinueLast {
		var sess *models.Session
		var err error
		if r.cfg.ContinueAny {
			sess, err = r.sessMgr.Latest()
		} else {
			sess, err = r.sessMgr.LatestInDir(r.cfg.WorkDir)
		}
		if err != nil {
			return fmt.Errorf("load latest session: %w", err)
		}
		if sess != nil {
			if err := r.acquireOrHandleLock(sess); err != nil {
				return err
			}
			msgs, err := r.sessMgr.LoadMessages(r.sess.ID)
			if err != nil {
				slog.Warn("load messages for continued session", "err", err)
			}
			r.sess.Messages = msgs
			slog.Info("continued session", "id", r.sess.ID, "messages", len(msgs))
			r.sess.Messages = filterUnresolvedToolUses(r.sess.Messages)
			r.restoreModelFromSession()
			return nil
		}
		// No history in THIS directory. Falling back to the global latest
		// session here would silently reproduce the bug this feature fixes,
		// so start a new session instead and say how to opt back in.
		if !r.cfg.ContinueAny {
			fmt.Fprintln(os.Stderr, "  本目录无历史会话，已新建；--continue-any 可续全局最近的会话。")
		}
	}

	// New session. Locked immediately too — otherwise a `-c` from another
	// process racing this one would see it as unlocked and free to resume.
	sess, err := r.createLockedSession()
	if err != nil {
		return err
	}
	r.sess = sess
	r.setLockedSession(sess.ID)
	r.persistModel()
	return nil
}

// deleteOrphanedSession deletes a just-created (or just-forked) session row
// whose lock acquisition failed, so it isn't left behind as the dir's
// updated_at-latest row — a later `deepai -c`/`--continue-any` would
// otherwise silently resume THIS empty/orphaned session instead of
// whatever the caller actually intended to keep using (D7 in the
// session-lock review). Logs rather than propagates the delete's own
// error: the caller already has a more informative error to return (the
// lock failure itself), and a failed best-effort cleanup must not mask it.
// Shared by every "create/fork a session, then immediately lock it" path
// (createLockedSession, forkCurrentSession, acquireOrHandleLock's --fork
// branch) so this cleanup exists exactly once — see N3 in the
// session-lock review's mechanical-cleanup pass.
func (r *ChatRepl) deleteOrphanedSession(id string) {
	if err := r.sessMgr.Delete(id); err != nil {
		slog.Warn("delete orphaned session after lock failure", "session", id, "err", err)
	}
}

// createLockedSession creates a brand-new session (for r.cfg.WorkDir /
// r.currentModel) and immediately locks it, atomically from the caller's
// point of view: if the lock can't be acquired, the just-created row is
// deleted rather than left behind (see deleteOrphanedSession).
func (r *ChatRepl) createLockedSession() (*models.Session, error) {
	sess, err := r.sessMgr.Create(models.CreateOpts{
		Model: r.currentModel,
		CWD:   r.cfg.WorkDir,
	})
	if err != nil {
		return nil, err
	}
	if err := r.sessMgr.AcquireSessionLock(sess.ID, r.lockOwner, false); err != nil {
		r.deleteOrphanedSession(sess.ID)
		return nil, fmt.Errorf("acquire lock on new session %s: %w", sess.ID, err)
	}
	return sess, nil
}

// newLockOwner identifies this OS process for AcquireSessionLock. Falls back
// to unknownHostFallback() on the (rare) chance os.Hostname() fails, so a
// lock is still recorded rather than resolveSession erroring out over a
// display-only detail.
func newLockOwner() models.LockOwner {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = unknownHostFallback()
	}
	return models.LockOwner{PID: os.Getpid(), Host: host}
}

// unknownHostFallback generates the Host value newLockOwner uses when
// os.Hostname() fails: a per-process-UNIQUE placeholder, not a single fixed
// string (M4, session-lock review round 2). A fixed "unknown-host" made any
// two DIFFERENT machines that both happened to hit this rare path (no
// hostname configured, a sandboxed/container environment, a transient
// os.Hostname() failure) collide onto the same lock "host" value —
// canAcquireSessionLock would then probe a foreign machine's pid against
// THIS machine's process table, either judging a live foreign lock dead
// (stealing it out from under a still-running process) or a dead one alive
// (never reclaiming it). A random per-process suffix means two processes
// can only ever collide by drawing the exact same suffix, which only makes
// them look like "the same host" in the SAFE direction: worst case, two
// genuinely different unknown-host processes are (correctly) treated as
// different hosts, falling through to ordinary heartbeat-staleness judging
// — same as any other legitimate cross-host lock — never as the same host
// sharing a meaningless pid.
func unknownHostFallback() string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return fmt.Sprintf("unknown-host-%x", buf)
}

// acquireOrHandleLock takes the session lock for sess, applying the
// --fork/--force remedies from ReplConfig when it is already held by a live
// owner elsewhere (models.ErrSessionLocked). On success it sets r.sess — to
// sess itself, or to a freshly forked session when --fork applied — so
// callers must read the resolved session back from r.sess afterward, never
// from the sess argument.
//
// On failure r.sess is left untouched (nil on a fresh REPL), which is load
// bearing: the caller must not go on to load or write session state it
// never actually locked.
func (r *ChatRepl) acquireOrHandleLock(sess *models.Session) error {
	err := r.sessMgr.AcquireSessionLock(sess.ID, r.lockOwner, r.cfg.ForceSession)
	if err == nil {
		r.sess = sess
		r.setLockedSession(sess.ID)
		return nil
	}

	var lockErr *models.ErrSessionLocked
	if !errors.As(err, &lockErr) {
		return fmt.Errorf("acquire session lock: %w", err)
	}

	if r.cfg.ForkSession {
		forked, ferr := r.sessMgr.ForkSession(sess.ID, r.cfg.WorkDir)
		if ferr != nil {
			return fmt.Errorf("fork session %s: %w", sess.ID, ferr)
		}
		// The fork is brand new, so this acquisition should always succeed;
		// force=false is deliberate here — a genuine conflict on the FORK
		// would mean something else is very wrong and should surface, not
		// be papered over.
		if lerr := r.sessMgr.AcquireSessionLock(forked.ID, r.lockOwner, false); lerr != nil {
			r.deleteOrphanedSession(forked.ID)
			return fmt.Errorf("acquire lock on forked session %s: %w", forked.ID, lerr)
		}
		fmt.Fprintf(os.Stderr, "  会话 %s 正被占用，已分叉为新会话 %s（原会话不受影响）。\n", sess.ID, forked.ID)
		r.sess = forked
		r.setLockedSession(forked.ID)
		return nil
	}

	// Reject and explain the way out. Deliberately NOT auto-forking or
	// falling back to read-only: the incident this feature fixes was two
	// processes silently sharing one session, so the safe default is to stop
	// and make the user choose, not to guess on their behalf.
	//
	// L-B (session-lock review round 3): this message is shown verbatim to
	// the end user by cobra — it must never carry an internal review
	// finding ID/round number (a previous draft appended one after the
	// --force line). Keep any such bookkeeping in code comments, never in
	// a user-facing string; grep the package for user-visible strings
	// (fmt.Errorf/ui.Info/Fprintln et al.) before adding a new one.
	return fmt.Errorf(
		"会话 %s 正在被另一个 deepai 使用（pid %d @ %s，心跳 %s 前）。\n"+
			"  这也可能是上一次 deepai 异常退出（例如被 kill -9）残留下的 pid——如果确认如此，--force 是安全的。\n"+
			"  - 直接运行 deepai（不带 -c/-r）开启新会话\n"+
			"  - 加 --fork：把历史复制到一个新会话里继续，原会话不受影响\n"+
			"  - 加 --force：强行接管；若原进程还活着会造成消息交错，仅在确认它已卡死或已崩溃时使用",
		sess.ID, lockErr.Owner.PID, lockErr.Owner.Host, time.Since(lockErr.HeartbeatAt).Round(time.Second),
	)
}

// filterUnresolvedToolUses removes assistant messages where ALL tool calls
// have no corresponding tool results. Keeps messages with at least one
// resolved tool call so the model has context of completed work.
func filterUnresolvedToolUses(messages []models.Message) []models.Message {
	// Collect all tool result call IDs.
	resolvedResults := make(map[string]bool)
	for _, msg := range messages {
		if msg.Role == models.RoleTool && msg.ToolResult != nil {
			resolvedResults[msg.ToolResult.CallID] = true
		}
	}

	filtered := make([]models.Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Role != models.RoleAI || len(msg.ToolCalls) == 0 {
			filtered = append(filtered, msg)
			continue
		}
		// Keep assistant message if at least one tool call has a result.
		hasResolved := false
		for _, tc := range msg.ToolCalls {
			if resolvedResults[tc.ID] {
				hasResolved = true
				break
			}
		}
		if hasResolved {
			// Strip unresolved tool calls, keep the rest.
			kept := make([]models.ToolCall, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				if resolvedResults[tc.ID] {
					kept = append(kept, tc)
				}
			}
			msg.ToolCalls = kept
			filtered = append(filtered, msg)
		}
		// Drop assistant messages where ALL tool calls are unresolved.
	}
	return filtered
}

// isContinuationInput checks if the user input is a continuation request.
func isContinuationInput(s string) bool {
	s = strings.TrimSpace(strings.ToLower(s))
	return s == "继续" || s == "continue" || s == "go on" || s == "keep going"
}

// isSessionInterrupted checks if the session ended mid-task (last message
// indicates an incomplete turn: tool result without follow-up, or error state).
func isSessionInterrupted(messages []models.Message) bool {
	if len(messages) == 0 {
		return false
	}
	last := messages[len(messages)-1]
	// Last message is a tool result → agent didn't get to respond
	if last.Role == models.RoleTool {
		return true
	}
	// Last assistant message is empty or has tool calls with no following results
	if last.Role == models.RoleAI {
		if strings.TrimSpace(last.Content) == "" && len(last.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// continueTurn resumes the agent from an interrupted session by injecting
// a continuation prompt instead of a real user message.
func (r *ChatRepl) continueTurn(ctx context.Context) error {
	// A turn interrupted mid tool-batch can leave an assistant message whose
	// tool calls never received results. Strip those unresolved calls so the
	// resumed request is well-formed for the provider API (the reload path does
	// this too, but an in-session "continue" never reloads from the DB).
	r.sess.Messages = filterUnresolvedToolUses(r.sess.Messages)
	return r.runTurn(ctx, "Continue from where you left off.", nil, true)
}

// turnError wraps errors from runTurn to distinguish cancellation from real errors.
type turnError struct {
	err       error
	cancelled bool
}

func (e *turnError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return "cancelled"
}

// runTurnWithSignal creates a cancellable turn context, cancels it when the user
// presses Ctrl+C (delivered on the TUI interrupt channel, since raw mode means
// Ctrl+C never raises SIGINT), runs the given turn function, and returns a
// turnError that distinguishes cancellation from real errors. Returns nil on
// clean success.
func (r *ChatRepl) runTurnWithSignal(parentCtx context.Context, fn func(context.Context) error) *turnError {
	turnCtx, turnCancel := context.WithCancel(parentCtx)
	uiInterrupt := r.ui.InterruptCh()

	// sigFired is written by the watcher goroutine before turnCancel(); reading
	// it after fn returns (which implies the context is done) is race-free.
	sigFired := make(chan struct{}, 1)
	cancelTasks := r.ui.CancelTaskCh()
	go func() {
		for {
			select {
			case <-uiInterrupt:
				sigFired <- struct{}{}
				turnCancel()
				return
			case taskID := <-cancelTasks:
				// Per-task cancellation does NOT end the turn: the point is to
				// drop one stuck subagent and let the rest finish.
				if r.cfg.TaskCanceller != nil {
					r.cfg.TaskCanceller.CancelTask(taskID)
				}
			case <-turnCtx.Done():
				return
			}
		}
	}()

	err := fn(turnCtx)
	turnCancel()

	interrupted := false
	select {
	case <-sigFired:
		interrupted = true
	default:
	}

	if err == nil && !interrupted {
		return nil
	}
	return &turnError{err: err, cancelled: interrupted}
}

// mainAgentMaxTokens returns a fresh pointer to agent.ResolveMaxOutputTokens()
// for AgentConfig.MaxTokens, which takes *int. It exists only so this value
// is read from the one shared resolver rather than a local literal — see
// pkg/commands/chat.go's subagentMaxTokens, which the subagent wiring must
// keep in step with. ResolveMaxOutputTokens honors an explicit
// DEEPAI_MAX_OUTPUT_TOKENS setting and otherwise falls back to
// agent.DefaultMaxOutputTokens; it never returns 0.
func mainAgentMaxTokens() *int {
	n := agent.ResolveMaxOutputTokens()
	return &n
}

func (r *ChatRepl) runTurn(ctx context.Context, userInput string, images []models.MessageImage, continuation bool) error {
	ctx = subagent.WithEventSink(ctx, func(evt subagent.TaskEvent) {
		r.ui.RenderSubagentEvent(evt)
	})

	// Evaluate fact feedback from previous turn (consume-once).
	r.evaluateFactFeedback(r.sess.ID, r.turn, userInput)

	// Parse @path image references from the input text.
	cleanedInput, pathImages := parseImageReferences(userInput, r.cfg.WorkDir)
	if len(pathImages) > 0 {
		images = append(images, pathImages...)
		userInput = cleanedInput
	}

	// Append user message to session history. A continuation is a synthetic
	// nudge (e.g. resume after interrupt), not a real user turn: hand it to the
	// agent in-memory but never persist it, so it doesn't pollute the saved
	// transcript or the FTS index.
	userMsg := models.Message{
		SessionID: r.sess.ID,
		Role:      models.RoleHuman,
		Content:   userInput,
		Images:    images,
	}
	if !continuation {
		if err := r.appendMessage(userMsg); err != nil {
			slog.Warn("append user message", "err", err)
		}
	}
	r.sess.Messages = append(r.sess.Messages, userMsg)

	// Resolve the current model's provider + model name.
	provider, modelName, err := r.cfg.ModelRegistry.ProviderFor(r.currentModel)
	if err != nil {
		return fmt.Errorf("resolve model %q: %w", r.currentModel, err)
	}

	// Create a fresh agent for this turn.
	agentCfg := agent.AgentConfig{
		LLMProvider:     provider,
		Tools:           r.cfg.ToolRegistry,
		Sandbox:         r.sb,
		Model:           modelName,
		ContextWindow:   r.currentContextWindow(),
		ReasoningEffort: r.currentReasoningEffort(),
		MaxToolCalls:    r.cfg.MaxToolCalls,
		// MaxTokens: without this the provider default applies (8192 for
		// Anthropic), the same limit pkg/commands/chat.go raises for
		// subagents to avoid truncating a large tool-call argument
		// mid-stream — this is the agent the user actually talks to, so it
		// needs the same headroom. See agent.ResolveMaxOutputTokens and
		// mainAgentMaxTokens below.
		MaxTokens:       mainAgentMaxTokens(),
		Temperature:     r.currentTemperature(),
		RequestTimeout:  r.cfg.RequestTimeout,
		UserInteraction: r.ui,
		PlanMode:        r.planMode,
		WorkDir:         r.cfg.WorkDir,
		MemoryService:   r.cfg.MemoryService,
		MemoryExtractor: r.cfg.MemoryExtractor,
		MemoryUserID:    r.cfg.WorkDir,
		ImageDetail:     r.currentImageDetail(),
		AgentCatalog:    r.cfg.AgentCatalog,
		Session:         r.carry,
	}

	runAgent := agent.New(agentCfg)

	// Append skill descriptions and system prompt.
	if r.cfg.SkillRegistry != nil {
		if desc := r.cfg.SkillRegistry.Descriptions(); desc != "" {
			runAgent.AppendSystemPrompt(desc)
		}
	}
	if r.cfg.SystemPrompt != "" {
		runAgent.AppendSystemPrompt(r.cfg.SystemPrompt)
	}

	r.ui.TurnStart(r.turn, userInput)

	// Remember message count before agent run to only persist new messages.
	prevMsgCount := len(r.sess.Messages)

	// Start event draining goroutine BEFORE Run().
	events := make(chan agent.AgentEvent, 128)
	go func() {
		for evt := range runAgent.Events() {
			events <- evt
		}
		close(events)
	}()

	// Run the agent.
	type outcome struct {
		result *agent.RunResult
		err    error
	}
	outcomes := make(chan outcome, 1)
	go func() {
		result, err := runAgent.Run(ctx, r.sess.ID, r.sess.Messages)
		outcomes <- outcome{result: result, err: err}
	}()

	// Process events as they arrive. The TUI shows a live spinner + elapsed
	// timer while the agent runs, so no separate idle heartbeat is needed.
	var lastUsage *agent.Usage
	var turnErr error
	var turnToolCalls []memory.ToolCallInfo
EventLoop:
	for {
		select {
		case evt, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if evt.Usage != nil {
				lastUsage = evt.Usage
			}
			r.ui.RenderEvent(evt)
			// Collect tool call names for distribution tracking.
			if evt.Type == agent.AgentEventToolCallStart {
				name := ""
				if evt.ToolEvent != nil {
					name = evt.ToolEvent.Name
				} else if evt.ToolCall != nil {
					name = evt.ToolCall.Name
				}
				if name != "" {
					turnToolCalls = append(turnToolCalls, memory.ToolCallInfo{Name: name})
				}
			}
		case out := <-outcomes:
			// Drain remaining events.
			if events != nil {
				for evt := range events {
					if evt.Usage != nil {
						lastUsage = evt.Usage
					}
					r.ui.RenderEvent(evt)
				}
			}
			if out.result != nil {
				r.sess.Messages = out.result.Messages
				if out.result.Usage != nil {
					lastUsage = out.result.Usage
				}
			}
			turnErr = out.err
			break EventLoop
		case <-ctx.Done():
			// The user interrupted (ctrl+c) or the turn timed out. The agent's
			// Run goroutine returns promptly once ctx is cancelled, carrying the
			// messages accumulated so far. Capture them so the partial progress
			// is persisted and an in-session "continue" resumes with full
			// context instead of restarting the turn from scratch. emit() never
			// blocks (it drops on a full buffer), so Run cannot deadlock here.
			turnErr = ctx.Err()
			select {
			case out := <-outcomes:
				if out.result != nil {
					r.sess.Messages = out.result.Messages
					if out.result.Usage != nil {
						lastUsage = out.result.Usage
					}
				}
			case <-time.After(r.orphanWaitOrDefault()):
				// Defensive: a tool ignoring ctx could delay Run's return.
				// Persist what we have rather than hanging the REPL.
				//
				// M4-3 (review r1 F3): the abandoned runAgent.Run goroutine
				// above is still alive and will keep mutating whatever
				// *agent.SessionCarry it was handed (breaker maps,
				// activeSkill/skillPrompt, token anchors) for as long as it
				// runs — SessionCarry has no locking (see its doc comment),
				// and the very next turn is about to build a NEW Agent
				// sharing r.carry, which would then race with the orphan on
				// every one of those fields (and can panic on the map writes
				// inside breaker.observe). Detach: give the next turn a fresh
				// carry and leave the orphaned goroutine as the sole (if now
				// pointless) owner of the old one. This preserves the "never
				// hand one SessionCarry to two concurrently-running Agents"
				// contract instead of violating it, at the cost of losing
				// this orphaned turn's carried state — acceptable, since the
				// turn itself was already abandoned.
				r.carry = agent.NewSessionCarry()
			}
			break EventLoop
		}
	}

	r.ui.TurnEnd(lastUsage)

	// Always persist new messages, even on timeout/cancellation — UNLESS
	// this process has lost its session lock (M1, session-lock review round
	// 2): this is exactly the write path the original incident report was
	// about, and appendMessage's r.lockState.isSuspended() check is what actually
	// stops it now, not the ctx cancellation D5 used to rely on (which
	// never gated this loop at all — a turn that noticed the loss and
	// unwound still reached here and wrote its whole batch to whoever now
	// owns the session).
	for _, msg := range r.sess.Messages[prevMsgCount:] {
		_ = r.appendMessage(msg)
	}

	// Sync plan mode both directions (M4-3): the agent may have exited plan
	// mode this turn (e.g. user confirmed the plan via exit_plan_mode) or
	// ENTERED it mid-turn (e.g. enter_plan_mode, deciding a complex request
	// needs a plan first) — either way, the next turn's AgentConfig.PlanMode
	// must reflect what actually happened, not just the exit direction.
	// Unconditional assignment (not the old if-guarded clear-only form)
	// keeps both directions symmetric with a single readback. Review r1
	// F10/item 6: this now runs BEFORE the turnErr early-return below so an
	// errored/interrupted turn still gets the readback (e.g. the agent
	// called enter_plan_mode and was then Ctrl+C'd) — runAgent.IsPlanMode()
	// is an atomic.Bool read, safe to call even if the Run goroutine that
	// owns it hasn't fully returned yet (the orphan path above).
	r.planMode = runAgent.IsPlanMode()
	// M4 final-phase review F-M4-4: every other write to r.planMode pairs
	// it with a SetStatus call (Run()'s startup, /plan, /run, model
	// switch) so the TUI footer stays in sync — this symmetric readback
	// must too, or a mid-turn enter_plan_mode leaves the footer showing
	// full tool access while the agent has actually restricted itself to
	// read-only tools. Safe to call here: runTurn executes on the REPL's
	// own goroutine, same as every other SetStatus call site.
	r.ui.SetStatus(r.currentModel, r.planMode)

	if turnErr != nil {
		r.saveSession()
		return turnErr
	}

	// Auto-title generation after first turn. Run asynchronously so the user
	// can keep typing while the title LLM call is in flight; a missed title
	// (e.g. REPL exits before goroutine returns) is acceptable since the user
	// can always rename via /title.
	if r.turn == 1 && r.sess.Title == "" {
		sessionID := r.sess.ID
		var firstUserMsg string
		for _, m := range r.sess.Messages {
			if m.Role == models.RoleHuman {
				firstUserMsg = m.Content
				break
			}
		}
		go r.generateTitle(sessionID, firstUserMsg)
	}

	// Record tool call distribution for preference extraction triggers.
	if r.prefSched != nil && len(turnToolCalls) > 0 {
		r.prefSched.RecordToolCalls(turnToolCalls)
	}

	// Schedule memory work for this turn.
	//
	// The throttle keeps LLM extraction cost bounded; compaction performs a
	// synchronous flush ([pkg/agent/react.go] CancelPendingUpdates+UpdateWith),
	// so nothing is lost between checkpoints. With auto-refine on, the gate
	// decides whether a checkpoint is worth extracting at all; with it off, the
	// unconditional extraction below is exactly the pre-gate behaviour.
	userScopeKey := ""
	if uid := strings.TrimSpace(r.cfg.WorkDir); uid != "" {
		userScopeKey = memory.UserScope(uid).Key()
	}

	if r.cfg.MemoryService != nil && r.cfg.MemoryExtractor != nil {
		switch memoryScheduleFor(r.turn, r.cfg.MemoryRefineInterval, r.autoRefineEnabled()) {
		case memoryScheduleRefine:
			// One gate call decides for both scopes; ScheduleRefine queues a job
			// per scope so dedup, cancellation and flush versioning stay sharded
			// by storage key.
			r.cfg.MemoryService.ScheduleRefine(r.sess.ID, userScopeKey, r.sess.Messages, r.cfg.MemoryExtractor)
		case memoryScheduleUnconditional:
			r.cfg.MemoryService.ScheduleUpdateWith(r.sess.ID, r.sess.Messages, r.cfg.MemoryExtractor)
			if userScopeKey != "" {
				r.cfg.MemoryService.ScheduleUpdateWith(userScopeKey, r.sess.Messages, r.cfg.MemoryExtractor)
			}
		}
	}

	// Schedule preference extraction (throttle is handled internally).
	// Preferences are user-scoped: writing them under the user-scope key —
	// the same key the agent injects via MemoryUserID — is what makes the
	// extractor's "update the existing preference instead of creating a new
	// fact" rule work across sessions. Under the session key every new
	// session started from an empty document and re-learned the same
	// preferences as fresh facts with fresh ids.
	if r.cfg.MemoryService != nil && r.cfg.PreferenceExtractor != nil {
		prefScopeKey := userScopeKey
		if prefScopeKey == "" {
			prefScopeKey = r.sess.ID
		}
		r.cfg.MemoryService.SchedulePreferenceUpdate(
			prefScopeKey, r.sess.Messages, r.cfg.PreferenceExtractor, r.prefSched,
		)
	}

	r.saveSession()
	return nil
}

// lockLostRejectMsg is shown by every session-level write/delete path that
// is NOT the low-noise per-message append/save fast path (see
// appendMessage/saveSession's doc comments for why those two stay silent
// no-ops) when r.lockState.isSuspended() blocks it: /clear, /undo, /title,
// and /fork's own failure path. A delete or rename that silently no-ops
// instead looks like it succeeded — the H-A review point (round 3): that
// is worse than refusing loudly, since the caller then has no way to tell
// whether the session was actually wiped/renamed/undone.
const lockLostRejectMsg = "  操作已取消：会话锁已丢失，继续写入这个会话会和新的持有者交错。运行 /fork 把当前这段转录保存到新会话，或 /new 直接开始新会话。"

// appendMessage persists msg to r.sess.ID, UNLESS this process has lost its
// session lock (r.lockState.isSuspended() — see onLockLost), in which case
// it is a silent no-op: the whole point of suspending writes is that
// nothing reaches the DB once another process may legitimately own this
// session, and this is the single choke point every AppendMessage call in
// the REPL goes through to guarantee that. Silent (unlike clearSession/
// undoLastTurn/the /title command below): appendMessage fires many times
// per turn on the REPL's normal hot path, and the turn's result is already
// shown on screen regardless of whether it landed on disk — the user-facing
// notification for this is the persistent lock-lost banner (onLockLost),
// not a per-message rejection.
func (r *ChatRepl) appendMessage(msg models.Message) error {
	if r.lockState.isSuspended() {
		return nil
	}
	return r.sessMgr.AppendMessage(r.sess.ID, msg)
}

func (r *ChatRepl) saveSession() {
	if r.sess == nil || r.sessMgr == nil {
		return
	}
	// See appendMessage's doc comment — same reasoning, same check.
	// saveSession is the other persistence path (session metadata: state,
	// title, model/effort) that must stop the instant this process no
	// longer owns the session, and stays a silent no-op for the same
	// reason appendMessage does.
	if r.lockState.isSuspended() {
		return
	}
	if err := r.sessMgr.Save(r.sess); err != nil {
		slog.Warn("save session failed", "err", err)
	}
}

// persistModel saves the current model alias and effort to the session metadata
// so that resuming the session restores the user's model choice and effort setting.
func (r *ChatRepl) persistModel() {
	if r.sess == nil {
		return
	}
	if r.sess.Metadata == nil {
		r.sess.Metadata = make(map[string]string)
	}
	r.sess.Metadata["model"] = r.currentModel
	r.sess.Metadata["effort"] = r.currentEffort
	r.saveSession()
}

// restoreModelFromSession reads the model alias and effort from session metadata
// and applies them to r.currentModel and r.currentEffort if the alias is still
// available in the registry.
func (r *ChatRepl) restoreModelFromSession() {
	if r.sess == nil || r.cfg.ModelRegistry == nil {
		return
	}
	alias := strings.TrimSpace(r.sess.Metadata["model"])
	if alias != "" && r.cfg.ModelRegistry.Has(alias) {
		r.currentModel = strings.ToLower(alias)
	} else if alias != "" {
		slog.Warn("session model alias not in registry, using default", "alias", alias, "default", r.currentModel)
	}
	// Restore effort; empty string means use model/provider default.
	r.currentEffort = strings.TrimSpace(r.sess.Metadata["effort"])
}

// generateTitle runs on its own goroutine (see runTurn's `go
// r.generateTitle(...)`), so this process's session lock can be lost —
// or, via /new or /fork, this process can switch onto a DIFFERENT session
// entirely — WHILE the provider.Chat call below is in flight; it has up to
// a 30s timeout, plenty of time for either to happen. Checking r.lockState
// only once, at the top, would leave exactly that window open: the
// goroutine could start before the loss/switch, finish the LLM call after
// it, and still call SetTitle on sessionID — a session this process may no
// longer own, or may have already abandoned for a new one. So every branch
// below re-checks r.holdsSession(sessionID) immediately before its
// SetTitle call, not just here. This one (still r.lockState.isSuspended())
// guards only the cheap, common case — skip the LLM call entirely when
// writes are already known to be suspended — correctness never depends on
// it catching a mid-flight session switch too; the three rechecks below do.
//
// N1: the rechecks deliberately use r.holdsSession(sessionID), not the
// bare r.lockState.isSuspended() an earlier version used — isSuspended()
// alone answers "are writes suspended on WHATEVER session is currently
// locked," which /new/forkCurrentSession's r.lockState.clear() resets to
// false the moment the switch completes, even though sessionID (captured
// before the switch) no longer names that current session. holdsSession
// compares sessionID against r.lockedSessionID directly, so it stays
// correct across a mid-call session switch. See
// TestGenerateTitle_RechecksSessionIdentityBeforePersisting.
func (r *ChatRepl) generateTitle(sessionID, firstUserMsg string) {
	if r.cfg.ModelRegistry == nil || firstUserMsg == "" {
		slog.Debug("auto-title skipped", "registry_nil", r.cfg.ModelRegistry == nil, "msg_empty", firstUserMsg == "")
		return
	}
	if r.lockState.isSuspended() {
		slog.Debug("auto-title skipped: session lock lost", "id", sessionID)
		return
	}
	provider, modelName, err := r.cfg.ModelRegistry.ProviderFor(r.currentModel)
	if err != nil {
		slog.Debug("auto-title skipped: model resolve failed", "err", err)
		return
	}
	if len(firstUserMsg) > 500 {
		firstUserMsg = firstUserMsg[:500]
	}

	prompt := fmt.Sprintf(
		"Generate a concise title (no more than 30 chars) in the same language as the user's message. Return only the title text, no quotes or formatting.\nUser: %s",
		firstUserMsg,
	)
	maxTokens := 60
	// Bounded timeout so a stuck LLM never leaks a goroutine for the lifetime
	// of the REPL.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := provider.Chat(ctx, llm.ChatRequest{
		Model:           modelName,
		Messages:        []models.Message{{Role: models.RoleHuman, Content: prompt}},
		MaxTokens:       &maxTokens,
		ReasoningEffort: "disabled",
	})
	if err != nil {
		slog.Debug("auto-title LLM failed", "err", err)
		fallback := firstUserMsg
		if len([]rune(fallback)) > 20 {
			fallback = string([]rune(fallback)[:20]) + "..."
		}
		// Re-check: provider.Chat above can take up to 30s, during which
		// the lock can be lost or this session switched away from — see
		// this function's doc comment.
		if !r.holdsSession(sessionID) {
			slog.Debug("auto-title fallback skipped: session no longer held", "id", sessionID)
			return
		}
		if err := r.sessMgr.SetTitle(sessionID, fallback); err != nil {
			slog.Debug("auto-title fallback SetTitle failed", "err", err)
		} else {
			slog.Debug("auto-title set via fallback", "id", sessionID, "title", fallback)
		}
		return
	}

	title := strings.TrimSpace(resp.Message.Content)
	slog.Debug("auto-title LLM response", "id", sessionID, "raw_title", resp.Message.Content, "title", title)
	if len([]rune(title)) > 30 {
		title = string([]rune(title)[:30])
	}
	if title == "" {
		slog.Debug("auto-title: LLM returned empty content, using fallback")
		fallback := firstUserMsg
		if len([]rune(fallback)) > 20 {
			fallback = string([]rune(fallback)[:20]) + "..."
		}
		// Re-check — see this function's doc comment.
		if !r.holdsSession(sessionID) {
			slog.Debug("auto-title empty-response fallback skipped: session no longer held", "id", sessionID)
			return
		}
		_ = r.sessMgr.SetTitle(sessionID, fallback)
		return
	}

	// Re-check — see this function's doc comment.
	if !r.holdsSession(sessionID) {
		slog.Debug("auto-title skipped: session no longer held after LLM call", "id", sessionID)
		return
	}
	if err := r.sessMgr.SetTitle(sessionID, title); err != nil {
		slog.Debug("auto-title SetTitle failed", "err", err)
	}
	slog.Debug("session title set", "id", sessionID, "title", title)
}

// handleSlashCommand processes a slash command. Returns true if the REPL should exit.
func (r *ChatRepl) handleSlashCommand(parentCtx context.Context, cmd SlashCommand) bool {
	switch cmd.Name {
	case "exit", "quit", "q":
		return true
	case "help", "h":
		r.ui.Info(slashHelpText(SortedCommands(r.cfg.Commands)))
	case "clear":
		r.clearSession()
	case "history":
		var sb strings.Builder
		PrintHistory(&sb, r.sess.Messages)
		r.ui.Info(strings.TrimRight(sb.String(), "\n"))
	case "compact":
		r.ui.Info("  Compaction is automatic when context fills up.")
	case "new":
		r.startNewSession()
	case "title":
		if cmd.Args == "" {
			r.ui.Info("  Usage: /title <name>")
			return false
		}
		if r.lockState.isSuspended() {
			r.ui.Info(lockLostRejectMsg)
			return false
		}
		if err := r.sessMgr.SetTitle(r.sess.ID, cmd.Args); err != nil {
			slog.Warn("set title failed", "err", err)
		}
		r.sess.Title = cmd.Args
		r.ui.Info(fmt.Sprintf("  Title set to: %s", cmd.Args))
	case "fork":
		r.forkCurrentSession()
	case "save":
		r.saveSession()
		r.ui.Info("  Session saved.")
	case "sessions":
		r.ui.Info(r.sessionListText())
	case "undo":
		r.undoLastTurn()
	case "plan":
		r.planMode = true
		r.ui.SetStatus(r.currentModel, r.planMode)
		r.ui.Info("  Plan mode enabled. Agent will explore and plan before writing code.\n  Use /run to disable, or the agent will ask you to approve the plan.")
	case "run", "code":
		r.planMode = false
		r.ui.SetStatus(r.currentModel, r.planMode)
		r.ui.Info("  Plan mode disabled. Agent has full tool access.")
	case "model":
		r.handleModelCommand(parentCtx, cmd.Args)
	case "effort":
		r.handleEffortCommand(parentCtx, cmd.Args)
	case "image":
		r.handleImageCommand(parentCtx, cmd.Args)
	case "imagedetail", "imagequality":
		r.handleImageDetailCommand(cmd.Args)
	case "refine":
		r.handleRefineCommand(parentCtx, cmd.Args)
	case "review":
		r.handleReviewCommand(parentCtx, cmd.Args)
	case "doctor":
		r.ui.Info(r.doctorText(parentCtx))
	case "status", "st":
		r.ui.Info(r.statusText())
	default:
		r.ui.Info(fmt.Sprintf("  Unknown command: /%s", cmd.Name))
		r.ui.Info(slashHelpText(SortedCommands(r.cfg.Commands)))
	}
	return false
}

// handleModelCommand implements /model: show current, list available, switch by
// alias, or launch an interactive picker via the persistent TUI input box.
func (r *ChatRepl) handleModelCommand(ctx context.Context, args string) {
	args = strings.TrimSpace(args)
	reg := r.cfg.ModelRegistry

	// /model (no args) or /model list — show current + available.
	if args == "" || args == "list" {
		var sb strings.Builder
		def, _ := reg.Resolve(r.currentModel)
		fmt.Fprintf(&sb, "  Current model: %s (%s/%s)", r.currentModel, def.Provider, def.Model)
		models := reg.List()
		if len(models) > 1 {
			fmt.Fprintf(&sb, "\n  Available models:")
			for _, m := range models {
				marker := "  "
				if strings.EqualFold(m.Name, r.currentModel) {
					marker = "→ "
				}
				fmt.Fprintf(&sb, "\n    %s%-12s — %s/%s", marker, m.Name, m.Provider, m.Model)
			}
		}
		r.ui.Info(sb.String())
		return
	}

	// /model ? — interactive picker through the TUI's own input box.
	if args == "?" {
		r.pickModel(ctx)
		return
	}

	// /model <alias> — switch by name.
	if !reg.Has(args) {
		r.ui.Info(fmt.Sprintf("  Unknown model %q. Available: %s", args, r.availableModelNames()))
		return
	}
	r.applyModel(ctx, strings.ToLower(args))
}

// pickModel presents an interactive model picker through the persistent TUI
// input box (AskQuestion), avoiding a nested huh/tea.Program that would
// conflict with the running Bubble Tea program.
func (r *ChatRepl) pickModel(ctx context.Context) {
	reg := r.cfg.ModelRegistry
	models := reg.List()
	if len(models) <= 1 {
		r.ui.Info("  Only one model configured.")
		return
	}
	var options []string
	for _, m := range models {
		label := fmt.Sprintf("%s (%s/%s)", m.Name, m.Provider, m.Model)
		if strings.EqualFold(m.Name, r.currentModel) {
			label += " *"
		}
		options = append(options, label)
	}
	header := "Select model:\n"
	for i, opt := range options {
		header += fmt.Sprintf("  %d. %s\n", i+1, opt)
	}
	header += "Enter number or alias (empty to cancel):"
	answer, err := r.ui.AskQuestion(ctx, header, options)
	if err != nil || strings.TrimSpace(answer) == "" {
		return
	}
	answer = strings.TrimSpace(answer)
	// AskQuestion returns the option text when a number is entered; extract alias.
	// Otherwise the user may have typed an alias directly.
	for _, m := range models {
		if strings.EqualFold(answer, m.Name) {
			r.applyModel(ctx, strings.ToLower(m.Name))
			return
		}
		// Match "alias (provider/model)" or "alias (provider/model) *"
		prefix := m.Name + " ("
		if strings.HasPrefix(strings.ToLower(answer), strings.ToLower(prefix)) {
			r.applyModel(ctx, strings.ToLower(m.Name))
			return
		}
	}
	r.ui.Info(fmt.Sprintf("  Unknown selection %q.", answer))
}

// applyModel switches to the named alias, updates UI, and persists to session.
func (r *ChatRepl) applyModel(_ context.Context, alias string) {
	r.currentModel = alias
	r.ui.SetStatus(r.currentModel, r.planMode)
	def, _ := r.cfg.ModelRegistry.Resolve(r.currentModel)
	r.persistModel()
	r.ui.Info(fmt.Sprintf("  Model changed to: %s (%s/%s)\n  (takes effect on next turn)", r.currentModel, def.Provider, def.Model))
}

// availableModelNames returns a comma-separated list of model aliases.
func (r *ChatRepl) availableModelNames() string {
	var names []string
	for _, m := range r.cfg.ModelRegistry.List() {
		names = append(names, m.Name)
	}
	return strings.Join(names, ", ")
}

// currentReasoningEffort returns the effective reasoning effort for the current
// model: model-level override if set, otherwise REPL runtime setting (r.currentEffort),
// finally empty string (provider default).
func (r *ChatRepl) currentReasoningEffort() string {
	if r.cfg.ModelRegistry == nil {
		return r.currentEffort
	}
	def, ok := r.cfg.ModelRegistry.Resolve(r.currentModel)
	if !ok || def.Effort == "" {
		return r.currentEffort
	}
	return def.Effort
}

// currentTemperature returns the effective sampling temperature for the
// current model: models[].temperature if set, else the global config value,
// else nil — no temperature is sent at all (Claude 4.7+ rejects the
// parameter, other modern models ignore it).
func (r *ChatRepl) currentTemperature() *float64 {
	if r.cfg.ModelRegistry != nil {
		if def, ok := r.cfg.ModelRegistry.Resolve(r.currentModel); ok && def.Temperature != nil {
			return def.Temperature
		}
	}
	return r.cfg.Temperature
}

// currentImageDetail returns the vision detail level for image attachments.
// Default is "low" (single tile, ~170 tokens) for token efficiency.
func (r *ChatRepl) currentImageDetail() string {
	if d := strings.TrimSpace(r.imageDetail); d != "" {
		return d
	}
	return "low"
}

// handleEffortCommand implements /effort: show current, list valid values, or set effort.
func (r *ChatRepl) handleEffortCommand(ctx context.Context, args string) {
	args = strings.TrimSpace(args)

	// /effort (no args) — show current effort
	if args == "" {
		effort := r.currentReasoningEffort()
		if effort == "" {
			effort = "(provider default)"
		}
		r.ui.Info(fmt.Sprintf("  Current reasoning effort: %s", effort))
		r.ui.Info("  Valid values: low, medium, high, disabled, default")
		r.ui.Info("  Usage: /effort <value>")
		return
	}

	// /effort default — 重置为空（使用模型配置或 provider default）
	if args == "default" {
		r.currentEffort = ""
		r.persistModel()
		r.ui.Info("  Reasoning effort reset to model/provider default")
		r.ui.Info("  (takes effect on next turn)")
		return
	}

	// Validate and set effort
	validValues := map[string]bool{"low": true, "medium": true, "high": true, "disabled": true}
	argsLower := strings.ToLower(args)
	if !validValues[argsLower] {
		r.ui.Info(fmt.Sprintf("  Invalid effort %q. Valid: low, medium, high, disabled, default", args))
		return
	}

	r.currentEffort = argsLower
	r.persistModel()
	r.ui.Info(fmt.Sprintf("  Reasoning effort set to: %s", r.currentEffort))
	r.ui.Info("  (takes effect on next turn)")
}

// handleImageCommand implements /image: show clipboard support status or
// attach an image file by path for the next turn.
func (r *ChatRepl) handleImageCommand(ctx context.Context, args string) {
	args = strings.TrimSpace(args)

	if args == "" {
		// Show clipboard support status.
		r.ui.Info(fmt.Sprintf("  Clipboard image support: %s", imageproc.ClipboardSupport()))
		r.ui.Info("  Usage:")
		r.ui.Info("    Ctrl+V     Paste image from clipboard")
		r.ui.Info("    @path.png  Reference image file in your message")
		r.ui.Info("    /image <path>  Attach image file explicitly")
		return
	}

	// /image <path> — read and optimize the image, then run a turn with it.
	resolved := args
	if !filepath.IsAbs(resolved) && r.cfg.WorkDir != "" {
		resolved = filepath.Join(r.cfg.WorkDir, resolved)
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		r.ui.Info(fmt.Sprintf("  Error reading image: %v", err))
		return
	}
	result, err := imageproc.Optimize(raw, imageproc.DefaultOptions)
	if err != nil {
		r.ui.Info(fmt.Sprintf("  Error optimizing image: %v", err))
		return
	}
	images := []models.MessageImage{{MimeType: result.MimeType, Base64: result.Base64}}
	r.ui.Info(fmt.Sprintf("  Attached: %s (%s, %dKB, ~%d tokens)",
		filepath.Base(args), result.MimeType, result.Bytes/1024,
		imageproc.EstimateTilesToTokens(1, "low")))

	r.turn++
	turnErr := r.runTurnWithSignal(ctx, func(ctx context.Context) error {
		return r.runTurn(ctx, fmt.Sprintf("Please analyze this image: %s", filepath.Base(args)), images, false)
	})
	if turnErr != nil {
		if turnErr.cancelled {
			r.ui.RenderInterrupted()
			return
		}
		r.ui.Info(fmt.Sprintf("  Error: %v", turnErr))
	}
}

// handleImageDetailCommand implements /imagedetail: set vision detail level.
func (r *ChatRepl) handleImageDetailCommand(args string) {
	args = strings.TrimSpace(strings.ToLower(args))
	if args == "" {
		current := r.currentImageDetail()
		r.ui.Info(fmt.Sprintf("  Current image detail: %s", current))
		r.ui.Info("  Valid values: low (default, ~170 tokens), high (multi-tile, finer detail)")
		r.ui.Info("  Usage: /imagedetail <low|high>")
		return
	}
	if args != "low" && args != "high" && args != "auto" {
		r.ui.Info(fmt.Sprintf("  Invalid value %q. Valid: low, high, auto", args))
		return
	}
	r.imageDetail = args
	r.ui.Info(fmt.Sprintf("  Image detail set to: %s (takes effect on next turn)", args))
}

// currentContextWindow returns the effective context window for the current
// model: model-level override if set, otherwise global default.
func (r *ChatRepl) currentContextWindow() int {
	if r.cfg.ModelRegistry == nil {
		return r.cfg.ContextWindow
	}
	def, ok := r.cfg.ModelRegistry.Resolve(r.currentModel)
	if !ok || def.ContextWindow == 0 {
		return r.cfg.ContextWindow
	}
	return def.ContextWindow
}

// clearSession wipes the current session's history — both the in-memory
// messages and the persisted rows — so a later `deepai -c` resumes it empty
// instead of replaying a cleared conversation. Unlike startNewSession
// (/new), it never changes WHICH session is current, so it never touches
// session_locks or r.lockedSessionID at all — there is nothing to switch.
//
// H-A (session-lock review round 3): if this process's writes are
// suspended (r.lockState.isSuspended() — the lock was lost), r.sess.ID no
// longer belongs to it, and DeleteMessagesAfterSeq would wipe whatever the
// NEW holder has since written — round 3's actual reproduction had this
// zero out a live 2-message session belonging to another process. This
// must reject loudly, not silently no-op: a /clear the user believes
// succeeded, that actually did nothing, is its own kind of data-loss bug
// (the user now assumes the history is gone and may act on that).
// Deliberately changes NOTHING — not even the in-memory r.sess.Messages —
// when rejecting, so there is no divergence between what the user is told
// and what state the REPL is actually in.
func (r *ChatRepl) clearSession() {
	if r.lockState.isSuspended() {
		r.ui.Info(lockLostRejectMsg)
		return
	}
	r.sess.Messages = nil
	r.turn = 0
	// M4-3: the message history is gone, so any carried cross-turn Agent
	// state (breaker counters, active skill, compaction anchors) referring
	// to it must go too — a fresh SessionCarry, not a reset of the old
	// one's fields, matching NewSessionCarry's doc comment.
	r.carry = agent.NewSessionCarry()
	if r.sessMgr != nil {
		if err := r.sessMgr.DeleteMessagesAfterSeq(r.sess.ID, 0); err != nil {
			slog.Warn("clear persisted messages failed", "err", err)
		}
	}
	r.ui.Info("  Session history cleared.")
}

// startNewSession switches the REPL onto a brand-new, freshly locked
// session, replacing the one it currently holds. The switch is ordered to
// never leave the REPL half-migrated (D2 in the session-lock review): the
// NEW session's lock is acquired first, and only once that has actually
// succeeded does this go on to touch the old session's lock or r.sess. If
// locking the new session fails, this returns having changed nothing — the
// old session (and its lock) is exactly as it was, and the error is shown
// to the user instead of silently dropped.
func (r *ChatRepl) startNewSession() {
	// M-B (session-lock review round 3): if writes are currently suspended,
	// whatever is sitting in r.sess.Messages beyond what actually made it
	// to disk is about to be discarded outright — appendMessage silently
	// dropped it (see its doc comment), so this may be the ONLY copy of a
	// turn's output anywhere. /new's job is a clean start, not preserving
	// that content — /fork is the command for that (see forkCurrentSession
	// and onLockLost's banner, which recommends /fork first) — but the
	// discard must be stated plainly in the confirmation, not left for the
	// user to discover later. See TestStartNewSession_WarnsWhenDiscardingUnsavedContent.
	discardingUnsaved := r.lockState.isSuspended() && len(r.sess.Messages) > 0

	sess, err := r.createLockedSession()
	if err != nil {
		r.ui.Info(fmt.Sprintf("  无法创建新会话，仍在当前会话中：%v", err))
		return
	}

	// The new session's lock is confirmed held. Only now finalize the old
	// one — mark it completed, persist that (saveSession is itself a no-op
	// if writes are still suspended from a PRIOR loss on the OLD session —
	// correctly so: we may no longer own it, and writing to it would risk
	// stomping whoever does), and release its lock.
	oldID := r.sess.ID
	r.sess.State = models.SessionStateCompleted
	r.saveSession()

	// setLockedSession(sess.ID) MUST happen BEFORE ReleaseSessionLock(oldID)
	// (M2, session-lock review round 2): the new lock is already confirmed
	// held above, so switching lockedSessionID to it now is safe, and it
	// closes the window the old ordering left open — between releasing
	// oldID and (previously) only THEN repointing lockedSessionID, a
	// heartbeat landing in between refreshed a lock row that no longer
	// existed (0 rows affected) and reported the whole REPL as having lost
	// its lock. See TestStartNewSession_HeartbeatDuringSwitchWindow_RealStore.
	r.setLockedSession(sess.ID)
	if err := r.sessMgr.ReleaseSessionLock(oldID, r.lockOwner); err != nil {
		slog.Warn("release old session lock on /new", "session", oldID, "err", err)
	}

	// A lock lost on the OLD session must not keep persistence disabled (or
	// the banner up) on the NEW one — this process has just demonstrably
	// (re-)proven it holds a lock, on sess.ID, via createLockedSession
	// above. lockState.clear() re-arms the notified flag too: a later loss
	// on THIS new session (e.g. another --force) must still be reported —
	// see TestLockLost_FullRecoveryPath_...
	r.lockState.clear()
	r.ui.SetLockLost(false)

	r.sess = sess
	r.turn = 0
	// M4-3 (review r1 F1): a new session must not inherit the previous
	// conversation's carried Agent state — same reset as clearSession, for
	// the same reason (see SessionCarry's doc comment).
	r.carry = agent.NewSessionCarry()
	r.persistModel()
	if discardingUnsaved {
		r.ui.Info(fmt.Sprintf("  New session started: %s（此前那段未保存的内容不会带入新会话——如果还想保留，下次改用 /fork）", sess.ID))
	} else {
		r.ui.Info(fmt.Sprintf("  New session started: %s", sess.ID))
	}
}

// forkCurrentSession implements /fork: forks the REPL's IN-MEMORY
// transcript (r.sess.Messages) into a brand-new, freshly locked session and
// switches onto it. This is the memory-sourced counterpart to the --fork
// STARTUP flag (acquireOrHandleLock's use of sessMgr.ForkSession), which
// reads the session to fork back from storage — appropriate there, since
// nothing is in memory yet at that point in resolveSession. Mid-REPL, after
// a lock loss, that is exactly the wrong source: the most recently finished
// turn's messages can be sitting ONLY in r.sess.Messages (appendMessage
// silently dropped them — see its doc comment), so re-reading the old
// session from the DB would lose precisely the turn /fork exists to
// rescue. sessMgr.ForkSessionFromMessages therefore takes msgs directly and
// never reads origID's stored rows at all (see its doc comment).
//
// Works whether or not the lock was ever lost — forking the current
// session to keep working elsewhere is reasonable any time — but the
// lock-lost banner (onLockLost) recommends /fork FIRST, ahead of /new,
// because it is the remedy that keeps this run's output.
func (r *ChatRepl) forkCurrentSession() {
	forked, err := r.sessMgr.ForkSessionFromMessages(r.sess.ID, r.sess.Title, r.sess.Metadata, r.cfg.WorkDir, r.sess.Messages)
	if err != nil {
		r.ui.Info(fmt.Sprintf("  分叉失败，仍在当前会话中：%v", err))
		return
	}
	if err := r.sessMgr.AcquireSessionLock(forked.ID, r.lockOwner, false); err != nil {
		// N3: without this cleanup, forked (unlocked, empty of nothing —
		// it already carries the full copied transcript) is left behind
		// as the dir's updated_at-latest row, exactly the D7 hazard
		// createLockedSession already guards against — see
		// deleteOrphanedSession's doc comment.
		r.deleteOrphanedSession(forked.ID)
		r.ui.Info(fmt.Sprintf("  分叉会话已创建但加锁失败，仍在当前会话中：%v", err))
		return
	}

	oldID := r.sess.ID
	// Best-effort tidy-up of the OLD session, exactly mirroring
	// startNewSession: if writes are still enabled (no loss occurred, or
	// this /fork is for an unrelated reason) this marks it completed and
	// releases the lock normally. If the lock WAS lost, saveSession() is
	// already a silent no-op and ReleaseSessionLock no-ops under a
	// mismatched owner (see its doc comment) — either way this never
	// writes to a session another process may now legitimately own.
	r.sess.State = models.SessionStateCompleted
	r.saveSession()
	r.setLockedSession(forked.ID)
	if err := r.sessMgr.ReleaseSessionLock(oldID, r.lockOwner); err != nil {
		slog.Warn("release old session lock on /fork", "session", oldID, "err", err)
	}

	r.lockState.clear()
	r.ui.SetLockLost(false)

	carried := r.sess.Messages
	forked.Messages = carried
	r.sess = forked
	// Deliberately NOT resetting r.turn/r.carry here (unlike /new/
	// /clear/undo): /fork is a continuation of the SAME conversation in a
	// new session, so the cross-turn Agent state and turn counter it was
	// building up should keep going, not reset.
	r.persistModel()
	r.ui.Info(fmt.Sprintf("  Forked into new session: %s (%d messages carried over)", forked.ID, len(carried)))
}

// undoLastTurn removes the last user turn from the session. H-A (session-
// lock review round 3): if writes are suspended, r.sess.ID no longer
// belongs to this process, and DeleteLastUserTurn would delete rows out of
// whatever the NEW holder has since written — the same class of bug
// clearSession's guard fixes, for the same reason. Rejects loudly instead
// of a silent no-op, and — like clearSession — changes nothing (does not
// even touch r.sess.Messages) when rejecting.
func (r *ChatRepl) undoLastTurn() {
	if r.lockState.isSuspended() {
		r.ui.Info(lockLostRejectMsg)
		return
	}
	removed, err := r.sessMgr.DeleteLastUserTurn(r.sess.ID)
	if err != nil {
		slog.Warn("undo: delete last user turn failed", "err", err)
		r.ui.Info("  Undo failed.")
		return
	}
	if removed == 0 {
		r.ui.Info("  Nothing to undo.")
		return
	}

	msgs, err := r.sessMgr.LoadMessages(r.sess.ID)
	if err != nil {
		slog.Warn("undo: reload messages failed", "err", err)
	}
	r.sess.Messages = filterUnresolvedToolUses(msgs)

	// M4-3 (review r1 F8): the removed messages invalidate the carried
	// compaction anchor (lastInputTokens/lastTokenCountMsgs referred to a
	// message count that no longer exists — estimateContextTokens's stale-
	// anchor guard only catches the larger-shrink case) and any
	// skill/breaker state built up during the undone turn. Reset the whole
	// carry rather than only its anchors, matching /clear and /new.
	r.carry = agent.NewSessionCarry()

	r.ui.Info(fmt.Sprintf("  Undone %d messages.", removed))
}

// sessionListText renders the recent-session list as a string.
func (r *ChatRepl) sessionListText() string {
	if r.sessMgr == nil {
		return "  No session repository configured."
	}
	metas, err := r.sessMgr.ListRecent(20)
	if err != nil {
		return fmt.Sprintf("  Error listing sessions: %v", err)
	}
	if len(metas) == 0 {
		return "  No sessions found."
	}
	styles := DefaultStyles()
	var sb strings.Builder
	header := fmt.Sprintf("  %-24s %-40s %5s %s", "ID", "TITLE", "MSGS", "CREATED")
	sb.WriteString(styles.Dim.Render(header))
	sb.WriteString("\n")
	for _, m := range metas {
		title := m.Title
		if title == "" {
			title = "(untitled)"
		}
		if len([]rune(title)) > 40 {
			title = string([]rune(title)[:37]) + "..."
		}
		created := m.CreatedAt.Format("2006-01-02 15:04")
		marker := "  "
		if m.ID == r.sess.ID {
			marker = styles.Highlight.Render(" *")
		}
		sb.WriteString(fmt.Sprintf("%s %-24s %-40s %5d %s\n", marker, m.ID, title, m.MsgCount, created))
	}
	sb.WriteString("  Use 'deepai -r <ID>' to resume a session.")
	return sb.String()
}

func (r *ChatRepl) statusText() string {
	var sb strings.Builder
	sessionID := ""
	if r.sess != nil {
		sessionID = r.sess.ID
	}
	tools := []models.Tool(nil)
	if r.cfg.ToolRegistry != nil {
		tools = r.cfg.ToolRegistry.List()
	}

	mcpTools := 0
	mcpServers := map[string]int{}
	for _, t := range tools {
		if !toolHasGroup(t, "mcp") {
			continue
		}
		mcpTools++
		server := mcpServerFromTool(t)
		if server == "" {
			server = "(unknown)"
		}
		mcpServers[server]++
	}

	pluginCmds := make([]string, 0)
	for _, c := range r.cfg.Commands {
		if c.Source == "plugin" {
			pluginCmds = append(pluginCmds, c.Name)
		}
	}
	sort.Strings(pluginCmds)

	type usage struct {
		count  int
		failed int
	}
	usageByTool := map[string]usage{}
	totalCalls := 0
	failedCalls := 0
	var messages []models.Message
	if r.sess != nil {
		messages = r.sess.Messages
	}
	for _, m := range messages {
		if m.Role != models.RoleTool || m.ToolResult == nil {
			continue
		}
		name := strings.TrimSpace(m.ToolResult.ToolName)
		if name == "" {
			continue
		}
		u := usageByTool[name]
		u.count++
		totalCalls++
		if m.ToolResult.Status == models.CallStatusFailed {
			u.failed++
			failedCalls++
		}
		usageByTool[name] = u
	}

	type kv struct {
		name string
		cnt  int
		fail int
	}
	toolUsage := make([]kv, 0, len(usageByTool))
	for name, u := range usageByTool {
		toolUsage = append(toolUsage, kv{name: name, cnt: u.count, fail: u.failed})
	}
	sort.Slice(toolUsage, func(i, j int) bool {
		if toolUsage[i].cnt != toolUsage[j].cnt {
			return toolUsage[i].cnt > toolUsage[j].cnt
		}
		return toolUsage[i].name < toolUsage[j].name
	})

	fmt.Fprintf(&sb, "  Runtime status:\n")
	// Show alias + provider/model for clarity.
	if def, ok := r.cfg.ModelRegistry.Resolve(r.currentModel); ok {
		fmt.Fprintf(&sb, "  Model: %s (%s/%s)\n", r.currentModel, def.Provider, def.Model)
	} else {
		fmt.Fprintf(&sb, "  Model: %s\n", r.currentModel)
	}
	fmt.Fprintf(&sb, "  Session: %s\n", sessionID)
	fmt.Fprintf(&sb, "  Loaded tools: %d (builtin/custom: %d, mcp: %d)\n", len(tools), len(tools)-mcpTools, mcpTools)
	fmt.Fprintf(&sb, "  Image clipboard: %s\n", imageproc.ClipboardSupport())
	fmt.Fprintf(&sb, "  Image detail: %s\n", r.currentImageDetail())

	if len(mcpServers) == 0 {
		sb.WriteString("  MCP servers: none\n")
	} else {
		names := make([]string, 0, len(mcpServers))
		for name := range mcpServers {
			names = append(names, name)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, name := range names {
			parts = append(parts, fmt.Sprintf("%s(%d)", name, mcpServers[name]))
		}
		fmt.Fprintf(&sb, "  MCP servers: %s\n", strings.Join(parts, ", "))
	}

	if len(pluginCmds) == 0 {
		sb.WriteString("  Plugin commands: none\n")
	} else {
		fmt.Fprintf(&sb, "  Plugin commands: %d (%s)\n", len(pluginCmds), strings.Join(pluginCmds, ", "))
	}

	if strings.TrimSpace(r.cfg.MCPReport) != "" {
		fmt.Fprintf(&sb, "  Startup report: %s\n", r.cfg.MCPReport)
	}

	fmt.Fprintf(&sb, "  Tool calls this session: %d total, %d failed\n", totalCalls, failedCalls)
	if len(toolUsage) > 0 {
		sb.WriteString("  Most used tools:\n")
		maxItems := 8
		if len(toolUsage) < maxItems {
			maxItems = len(toolUsage)
		}
		for i := 0; i < maxItems; i++ {
			line := fmt.Sprintf("    - %s: %d", toolUsage[i].name, toolUsage[i].cnt)
			if toolUsage[i].fail > 0 {
				line += fmt.Sprintf(" (failed %d)", toolUsage[i].fail)
			}
			sb.WriteString(line + "\n")
		}
	}

	return strings.TrimRight(sb.String(), "\n")
}

// doctorText runs environment diagnostics across all three extension surfaces
// — models, skills, and MCP servers — and returns a combined report. This
// mirrors the /doctor command in Claude Code for quick environment checks.
func (r *ChatRepl) doctorText(ctx context.Context) string {
	var sb strings.Builder
	sb.WriteString("  Doctor — environment diagnostics:\n\n")

	// 显示当前 effort 设置
	effort := r.currentEffort
	if effort == "" {
		effort = "(provider default)"
	}
	fmt.Fprintf(&sb, "  Current reasoning effort: %s\n\n", effort)

	sb.WriteString(r.doctorModels(ctx))
	sb.WriteString("\n\n")
	sb.WriteString(r.doctorSkills())
	sb.WriteString("\n\n")
	sb.WriteString(r.doctorMCP())
	return sb.String()
}

// doctorModels probes every configured model with a minimal "hello" request and
// reports per-model reachability, latency, and a summary.
func (r *ChatRepl) doctorModels(ctx context.Context) string {
	reg := r.cfg.ModelRegistry
	if reg == nil {
		return "  Models: no registry configured."
	}
	defs := reg.List()
	if len(defs) == 0 {
		return "  Models: none configured."
	}

	type result struct {
		def         llm.ModelDef
		ok          bool
		latency     time.Duration
		errMsg      string
		reply       string
		actualModel string // 服务端实际返回的模型名
	}

	results := make([]result, len(defs))
	var wg sync.WaitGroup
	// Per-probe timeout: a healthy model should respond to a trivial prompt
	// well under 30s. We bound each probe so one slow/unreachable model does
	// not stall the whole report.
	const probeTimeout = 30 * time.Second

	for i, def := range defs {
		wg.Add(1)
		go func(idx int, d llm.ModelDef) {
			defer wg.Done()
			res := result{def: d}
			provider, modelName, err := reg.ProviderFor(d.Name)
			if err != nil {
				res.errMsg = fmt.Sprintf("init failed: %v", err)
				results[idx] = res
				return
			}
			probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			maxTokens := 16
			start := time.Now()
			resp, err := provider.Chat(probeCtx, llm.ChatRequest{
				Model: modelName,
				Messages: []models.Message{{
					Role:    models.RoleHuman,
					Content: "Reply with exactly: hello",
				}},
				MaxTokens:       &maxTokens,
				ReasoningEffort: "disabled",
			})
			res.latency = time.Since(start)
			if err != nil {
				res.errMsg = err.Error()
				results[idx] = res
				return
			}
			res.ok = true
			res.reply = strings.TrimSpace(resp.Message.Content)
			res.actualModel = resp.Model
			results[idx] = res
		}(i, def)
	}
	wg.Wait()

	var sb strings.Builder
	sb.WriteString("  Models:\n")
	passed := 0
	for _, res := range results {
		marker := "✗"
		if res.ok {
			marker = "✓"
			passed++
		}
		endpoint := llm.ResolveBaseURL(res.def)
		if endpoint == "" {
			endpoint = "(provider default)"
		}
		// 计算有效上下文窗口
		effectiveCtx := r.cfg.ContextWindow
		if res.def.ContextWindow > 0 {
			effectiveCtx = res.def.ContextWindow
		}
		ctxLabel := fmt.Sprintf("%dK", effectiveCtx/1000)
		if res.def.ContextWindow > 0 {
			ctxLabel = fmt.Sprintf("%dK (model)", effectiveCtx/1000)
		}

		fmt.Fprintf(&sb, "    %s %-12s — %s/%s\n        endpoint: %s  ctx: %s", marker, res.def.Name, res.def.Provider, res.def.Model, endpoint, ctxLabel)
		if res.ok {
			fmt.Fprintf(&sb, "  (%dms)", res.latency.Milliseconds())
			// 显示实际模型名（如果与配置不同）
			if res.actualModel != "" && res.actualModel != res.def.Model {
				fmt.Fprintf(&sb, "  [actual: %s]", res.actualModel)
			}
			if res.reply != "" {
				reply := res.reply
				if len([]rune(reply)) > 40 {
					reply = string([]rune(reply)[:40]) + "..."
				}
				fmt.Fprintf(&sb, " → %q", reply)
			}
			sb.WriteString("\n")
		} else {
			msg := res.errMsg
			if msg == "" {
				msg = "unknown error"
			}
			if len([]rune(msg)) > 80 {
				msg = string([]rune(msg)[:80]) + "..."
			}
			fmt.Fprintf(&sb, "\n        ✗ %s\n", msg)
		}
	}
	if passed == len(results) {
		fmt.Fprintf(&sb, "    All %d model(s) healthy.", len(results))
	} else {
		fmt.Fprintf(&sb, "    %d/%d model(s) healthy. Check API keys, base URLs, and network.", passed, len(results))
	}
	return sb.String()
}

// doctorSkills reports the loaded skill set: count, per-skill source, and
// whether each skill's body is loadable. Skills are local files, so the check
// is configuration integrity (present + parseable), not network reachability.
func (r *ChatRepl) doctorSkills() string {
	reg := r.cfg.SkillRegistry
	if reg == nil || reg.Count() == 0 {
		return "  Skills: none loaded."
	}
	skills := reg.List()
	var sb strings.Builder
	fmt.Fprintf(&sb, "  Skills (%d):\n", len(skills))
	for _, s := range skills {
		source := s.Source
		if source == "" {
			source = "local"
		}
		fmt.Fprintf(&sb, "    ✓ %-20s (%s)\n", s.DisplayName(), source)
	}
	fmt.Fprintf(&sb, "    All %d skill(s) loaded.", len(skills))
	return sb.String()
}

// doctorMCP reports MCP server health from the runtime tool registry. MCP
// servers connect at startup and register their tools into ToolRegistry, so
// the presence of tools grouped under a server name signals a live connection.
// The startup report (MCPReport) surfaces any servers that failed to connect.
func (r *ChatRepl) doctorMCP() string {
	var sb strings.Builder
	sb.WriteString("  MCP servers:\n")

	// Collect MCP tools from the registry, grouped by server name.
	mcpTools := 0
	mcpServers := map[string]int{}
	if r.cfg.ToolRegistry != nil {
		for _, t := range r.cfg.ToolRegistry.List() {
			if !toolHasGroup(t, "mcp") {
				continue
			}
			mcpTools++
			server := mcpServerFromTool(t)
			if server == "" {
				server = "(unknown)"
			}
			mcpServers[server]++
		}
	}

	if len(mcpServers) == 0 {
		sb.WriteString("    none connected")
		// Surface startup failures if the config existed but nothing loaded.
		if msg := strings.TrimSpace(r.cfg.MCPReport); msg != "" {
			fmt.Fprintf(&sb, "\n    Startup report: %s", msg)
		}
		return sb.String()
	}

	// Stable server-name ordering.
	names := make([]string, 0, len(mcpServers))
	for name := range mcpServers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&sb, "    ✓ %-20s — %d tool(s)\n", name, mcpServers[name])
	}
	fmt.Fprintf(&sb, "    %d server(s) connected, %d tool(s) registered.", len(mcpServers), mcpTools)
	return sb.String()
}

func toolHasGroup(t models.Tool, group string) bool {
	for _, g := range t.Groups {
		if g == group {
			return true
		}
	}
	return false
}

func mcpServerFromTool(t models.Tool) string {
	for _, g := range t.Groups {
		if g != "" && g != "mcp" {
			return g
		}
	}
	if idx := strings.Index(t.Name, "."); idx > 0 {
		return t.Name[:idx]
	}
	return ""
}

// evaluateFactFeedback classifies the user message for feedback purposes,
// records events for preference extraction triggers, and increments HelpfulCount
// for previously retrieved facts if the signal is positive. Called before the
// agent runs for the current turn.
func (r *ChatRepl) evaluateFactFeedback(sessionID string, turn int, userMessage string) {
	var prevMsg string
	for i := len(r.sess.Messages) - 1; i >= 0; i-- {
		if r.sess.Messages[i].Role == models.RoleHuman {
			prevMsg = r.sess.Messages[i].Content
			break
		}
	}
	similarity := memory.TextCosineSimilarity(userMessage, prevMsg)
	result := memory.ClassifyUserResponse(userMessage, prevMsg, similarity)

	slog.Debug("evaluateFactFeedback",
		"session", sessionID,
		"turn", turn,
		"classification", result.Classification,
		"similarity", similarity,
	)
	r.monitorFalseReward(result.Classification)

	if result.Classification == memory.FeedbackNegative && r.prefSched != nil {
		r.prefSched.RecordNegativeFeedback()
	} else if r.prefSched != nil {
		r.prefSched.RecordNonNegativeFeedback()
	}
	if r.prefSched != nil {
		r.prefSched.CheckLanguageSwitch(userMessage)
	}

	if r.cfg.MemoryService == nil {
		return
	}
	factIDs := r.cfg.MemoryService.LastRetrieved(sessionID)
	if len(factIDs) == 0 {
		return
	}
	switch result.Classification {
	case memory.FeedbackPositive:
		r.cfg.MemoryService.ScheduleHelpfulIncrement(sessionID, turn, factIDs)
	case memory.FeedbackNegative:
		r.cfg.MemoryService.ScheduleSuspectIncrement(sessionID, turn, factIDs)
	}
}

// monitorFalseReward checks if a positive classification was wrong by tracking
// consecutive corrections.
func (r *ChatRepl) monitorFalseReward(classification memory.FeedbackClassification) {
	if classification == memory.FeedbackNegative {
		r.consecCorrections++
		if r.consecCorrections >= 3 {
			slog.Warn("fact feedback: consecutive corrections detected, check for false rewards",
				"consecutive", r.consecCorrections,
			)
		}
	} else if classification == memory.FeedbackPositive {
		if r.consecCorrections >= 3 {
			slog.Warn("fact feedback: positive signal after consecutive corrections — possible false reward",
				"consecutive_before", r.consecCorrections,
			)
		}
		r.consecCorrections = 0
	}
}

var imageFileExts = map[string]bool{
	".png":  true,
	".jpg":  true,
	".jpeg": true,
	".webp": true,
	".gif":  true,
}

// parseImageReferences scans input for @path tokens pointing to image files,
// reads and optimizes them, and returns the cleaned text (with image refs
// replaced by placeholders) and the parsed images.
// Non-image @path references are left untouched. Original whitespace is
// preserved.
func parseImageReferences(input string, workDir string) (string, []models.MessageImage) {
	var images []models.MessageImage
	cleaned := input
	slog.Debug("parseImageReferences", "input", input, "workDir", workDir)

	for _, w := range strings.Fields(input) {
		if !strings.HasPrefix(w, "@") {
			continue
		}
		pathStr := strings.TrimPrefix(w, "@")
		// Strip surrounding quotes.
		pathStr = strings.Trim(pathStr, `"'`)
		if pathStr == "" {
			continue
		}

		ext := strings.ToLower(filepath.Ext(pathStr))
		if !imageFileExts[ext] {
			slog.Debug("parseImageReferences: not an image ext", "word", w, "ext", ext)
			continue
		}

		// Resolve relative to workDir.
		resolved := pathStr
		if !filepath.IsAbs(resolved) && workDir != "" {
			resolved = filepath.Join(workDir, resolved)
		}

		raw, err := os.ReadFile(resolved)
		if err != nil {
			slog.Debug("parseImageReferences: read failed", "path", resolved, "err", err)
			continue // not readable → leave the @ref as-is
		}

		result, err := imageproc.Optimize(raw, imageproc.DefaultOptions)
		if err != nil {
			slog.Warn("image optimization failed", "path", resolved, "err", err)
			continue
		}

		slog.Debug("parseImageReferences: image loaded", "path", resolved, "mime", result.MimeType, "bytes", result.Bytes)

		// Replace just this @path token with a placeholder (preserves spacing).
		// Keep the full resolved path so the model can pass it to vision MCP tools.
		idx := len(images) // 1-based after append below
		images = append(images, models.MessageImage{
			MimeType: result.MimeType,
			Base64:   result.Base64,
		})
		cleaned = strings.Replace(cleaned, w, fmt.Sprintf("[image#%d:%s]", idx, resolved), 1)
	}

	return cleaned, images
}
