//go:build linux

package chat

import (
	"os"
	"strconv"
	"strings"
)

// processStopped reports whether pid is job-control-stopped (state T/t in
// /proc/<pid>/stat — SIGTSTP/SIGTTIN/SIGTRAP, e.g. a deepai suspended with
// Ctrl+Z). A stopped process keeps its pid (no reuse) but cannot run its
// heartbeat ticker — see canAcquireSessionLock for why that combination is
// reclaimable on the short clock. comm (field 2) may itself contain spaces
// and parens, so the state field is parsed after the LAST ')'. Read failure
// returns false: "can't tell" must fall back to the conservative running
// path, never unlock on a guess.
func processStopped(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(data), ')')
	if i < 0 || i+2 > len(data) {
		return false
	}
	fields := strings.Fields(string(data)[i+1:])
	if len(fields) == 0 {
		return false
	}
	state := fields[0]
	return state == "T" || state == "t"
}
