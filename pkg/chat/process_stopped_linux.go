//go:build linux

package chat

import (
	"os"
	"strconv"
	"strings"
)

// processStopped reports whether pid is stopped rather than runnable:
// state T (stopped by a job-control signal — SIGTSTP from Ctrl+Z, or
// SIGTTIN/SIGTTOU from a background tty read/write) or state t (tracing
// stop under a debugger). Darwin's SSTOP covers both classes in one
// value, so linux recognizing both keeps the two platforms' semantics
// aligned; the accepted consequence is that a holder parked on a
// debugger breakpoint past staleLockAfter is reclaimed like a Ctrl+Z'd
// one — it keeps its pid and its writes are frozen either way. A stopped
// process cannot run its heartbeat ticker (see canAcquireSessionLock for
// why it is reclaimable on the short clock). comm (field 2) may itself
// contain spaces and parens, so the state field is parsed after the LAST
// ')'. Read failure returns false: "can't tell" must fall back to the
// conservative running path, never unlock on a guess.
func processStopped(pid int) bool {
	if pid <= 0 {
		return false
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 > len(s) {
		return false
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) == 0 {
		return false
	}
	state := fields[0]
	return state == "T" || state == "t"
}
