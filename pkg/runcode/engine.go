// Package runcode implements the run_code scripted tool orchestration tool
// (docs/RUN_CODE_DESIGN.md): a QuickJS-ng/WASM sandbox whose only capability is
// calling the caller's CURRENT tool registry via tools.<name>(args), with
// batch scheduling that mirrors the top-level ReAct loop's concurrency rules.
package runcode

import (
	"context"
	"time"
)

// Outcome classifies a finished (or aborted) run (§6.2). models.CallStatus has
// no "cancelled"; cancelled vs timeout is carried here and in the first text
// line of the rendered result.
type Outcome string

const (
	OutcomeOK        Outcome = "ok"
	OutcomeError     Outcome = "error"
	OutcomeTimeout   Outcome = "timeout"
	OutcomeCallLimit Outcome = "call_limit"
	OutcomeMemory    Outcome = "memory"
	OutcomeCancelled Outcome = "cancelled"
)

// HostResult is one nested tool call as the script sees it (§4.4): resolve
// carries {status, text, data}; a non-empty Err rejects with an Error bearing
// .message/.tool/.text/.data.
type HostResult struct {
	Status string
	Text   string
	Data   any
	Err    string // non-empty → reject
	Tool   string // set on reject
}

// Host bridges the engine to the caller's live registry (§4.4). ToolInfo runs
// on the engine's registration path (inside VM entry); Call runs on driver or
// worker goroutines — implementations must be safe for concurrent Call.
type Host interface {
	// ToolInfo reports whether name is callable inside the script and whether
	// it may run alongside sibling calls (ParallelSafe).
	ToolInfo(name string) (visible, parallelSafe bool)
	// Call executes one nested tool invocation, ctx already carrying the
	// script budget deadline.
	Call(ctx context.Context, name string, args map[string]any) HostResult
}

// Limits are the script's budget (§6.1). Concurrency reuses the top-level cap
// (a.maxToolConcurrency); Timeout is the script wall-clock budget.
type Limits struct {
	MaxToolCalls   int
	MaxConcurrency int
	Timeout        time.Duration
}

// JournalStatus is the per-call lifecycle recorded in the result Data.
type JournalStatus string

const (
	JournalRunning     JournalStatus = "running"
	JournalCompleted   JournalStatus = "completed"
	JournalFailed      JournalStatus = "failed"
	JournalInterrupted JournalStatus = "interrupted"
)

// JournalEntry is one nested call's record (§6.3). Write marks a
// non-parallel-safe (potentially side-effecting) call that was STARTED; its
// args are kept unexported for edited-path extraction, never serialized.
type JournalEntry struct {
	Seq         int           `json:"seq"`
	Tool        string        `json:"tool"`
	ArgsPreview string        `json:"args_preview"`
	Status      JournalStatus `json:"status"`
	MS          int64         `json:"ms"`
	Bytes       int           `json:"bytes"`
	Write       bool          `json:"write"`

	args map[string]any
}

// RunOutput is the engine's verdict: the script's return value, its captured
// console output, the failure classification and the call journal.
type RunOutput struct {
	Outcome Outcome
	Value   any
	Logs    string
	Err     string
	Calls   []JournalEntry
	Elapsed time.Duration
}

// Engine executes one wrapped script per Run; instances are stateless. Run
// never blocks past ctx's own deadline plus small ε: the VM is driven on an
// inner goroutine and the caller is served by a watchdog when the VM cannot
// be preempted (a pure-JS infinite loop — see engine_test.go S3).
type Engine interface {
	Run(ctx context.Context, script string, host Host, lim Limits) RunOutput
}
