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
