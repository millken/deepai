package commands

import "time"

// defaultSubagentTimeout bounds one dispatched subagent when config.yaml
// doesn't say otherwise.
//
// The pool was deliberately built with no deadline at all (pkg/subagent's
// NewPool documents the reasoning: no single wall clock fits both a quick
// lookup and a whole-project delegation). That reasoning predates the
// graceful wall-clock wind-down in pkg/agent (react.go's
// shouldTriggerWallClockWrapUp): a deadline no longer KILLS a run, it makes
// the run reserve time from its own observed turn pace and write its answer
// without tools before the clock runs out. The cost of a deadline that is a
// little too tight is therefore a shorter answer, not a lost one — while the
// cost of no deadline at all is an interactive turn that cannot end, which is
// what a parallel review fan-out actually did.
//
// 10 minutes matches DefaultReviewTimeout, the one bounded subagent path that
// already existed, so the two review routes (the gate's own reviewer and a
// reviewer the model dispatches itself) no longer behave differently.
//
// Set subagent_timeout in config.yaml to override; negative means unlimited,
// which restores the old behaviour exactly.
const defaultSubagentTimeout = 10 * time.Minute

// resolveSubagentTimeout maps config.yaml's minutes int to an effective
// per-task deadline: 0/absent → defaultSubagentTimeout, negative → 0, which
// is how pkg/subagent spells "no deadline".
func resolveSubagentTimeout(minutes int) time.Duration {
	switch {
	case minutes == 0:
		return defaultSubagentTimeout
	case minutes < 0:
		return 0
	default:
		return time.Duration(minutes) * time.Minute
	}
}
