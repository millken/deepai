package sandbox

import (
	"fmt"
	"time"
)

// Result captures command execution details.
type Result struct {
	stdout   string
	stderr   string
	exitCode int
	duration time.Duration
	err      error
	timedOut bool
}

func (r *Result) Stdout() string {
	if r == nil {
		return ""
	}
	return r.stdout
}

func (r *Result) Stderr() string {
	if r == nil {
		return ""
	}
	return r.stderr
}

func (r *Result) ExitCode() int {
	if r == nil {
		return -1
	}
	return r.exitCode
}

func (r *Result) Duration() time.Duration {
	if r == nil {
		return 0
	}
	return r.duration
}

func (r *Result) Error() error {
	if r == nil {
		return nil
	}
	return r.err
}

// TimedOut reports whether the command was killed because it ran out of time
// rather than exiting on its own.
//
// Without this the two are indistinguishable to the caller: a killed process
// reports exit code -1, which is the same value used for "failed to start",
// and a command that hangs silently produces no output to explain itself. An
// agent handed `{"stdout":"","stderr":"","exit_code":-1}` cannot tell that it
// timed out, so it re-runs the command — which is exactly what happened in
// session 20260812_093415_fc6e: eight `dart test` retries, 120s apiece.
func (r *Result) TimedOut() bool {
	if r == nil {
		return false
	}
	return r.timedOut
}

// WithTimedOut returns r marked as timed out. Separate from NewResult so the
// common construction path stays a five-argument call.
func (r *Result) WithTimedOut(timedOut bool) *Result {
	if r == nil {
		return nil
	}
	r.timedOut = timedOut
	return r
}

// String formats the execution result for display.
func (r *Result) String() string {
	if r == nil {
		return "<nil>"
	}
	if r.err != nil {
		return fmt.Sprintf("exit=%d duration=%s stdout=%q stderr=%q error=%v", r.exitCode, r.duration, r.stdout, r.stderr, r.err)
	}
	return fmt.Sprintf("exit=%d duration=%s stdout=%q stderr=%q", r.exitCode, r.duration, r.stdout, r.stderr)
}

// NewResult creates a Result with the given values.
func NewResult(stdout, stderr string, exitCode int, duration time.Duration, err error) *Result {
	return &Result{stdout: stdout, stderr: stderr, exitCode: exitCode, duration: duration, err: err}
}
