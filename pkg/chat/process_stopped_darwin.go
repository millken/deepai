//go:build darwin

package chat

import "golang.org/x/sys/unix"

// darwinSSTOP mirrors SSTOP from XNU's sys/proc.h (process suspended by job
// control). x/sys does not export it.
const darwinSSTOP = 4

// processStopped reports whether pid is job-control-stopped (SIGTSTP/SIGTTIN
// — e.g. a deepai suspended with Ctrl+Z or backgrounded onto a tty read). A
// stopped process keeps its pid (no reuse) but cannot run its heartbeat
// ticker — see canAcquireSessionLock for why that combination is reclaimable
// on the short clock. Probe failure returns false: "can't tell" must fall
// back to the conservative running path, never unlock on a guess.
func processStopped(pid int) bool {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return false
	}
	return k.Proc.P_stat == darwinSSTOP
}
