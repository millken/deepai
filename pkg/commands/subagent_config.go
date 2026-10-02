package commands

import "time"

// resolveSubagentTimeout maps config.yaml's subagent_timeout (minutes) to the
// pool's per-task deadline.
//
// Absent (0) and any negative value mean no deadline: the task lives as long
// as the parent turn. That is the default on purpose. A wall clock that is a
// little too tight does not fail the run, it forces a tool-less wrap-up and
// returns a shorter answer as success — fine for a review verdict, wrong for
// a coder that still had edits to make. No single clock fits both a quick
// lookup and a whole-project change, so the interactive pool stays unbounded
// unless the operator names a limit. A positive value is that many minutes.
// pkg/subagent spells "no deadline" as 0.
func resolveSubagentTimeout(minutes int) time.Duration {
	if minutes <= 0 {
		return 0
	}
	return time.Duration(minutes) * time.Minute
}
