//go:build !darwin && !linux

package chat

// processStopped is conservative on platforms without a proc-status probe:
// report "not stopped" so a live holder is judged as running and keeps the
// full D1 veto (10-minute pid-reuse window) rather than the short clock.
func processStopped(pid int) bool {
	return false
}
