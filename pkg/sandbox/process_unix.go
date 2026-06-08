//go:build !windows

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup places the command in its own process group so the whole
// group can be signalled on cleanup.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func forceKillProcess(proc *os.Process) {
	if proc == nil {
		return
	}
	if proc.Pid > 0 {
		_ = syscall.Kill(-proc.Pid, syscall.SIGKILL)
	}
	_ = proc.Kill()
}

// execProgram replaces the current process image with the target program.
func execProgram(path string, args []string, env []string) int {
	if err := syscall.Exec(path, args, env); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "exec %s: %v\n", path, err)
		return 127
	}
	return 0
}
