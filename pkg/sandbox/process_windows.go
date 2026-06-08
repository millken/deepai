//go:build windows

package sandbox

import (
	"errors"
	"os"
	"os/exec"
)

// setProcessGroup is a no-op on Windows; process-group signalling is not used.
func setProcessGroup(cmd *exec.Cmd) {}

func forceKillProcess(proc *os.Process) {
	if proc == nil {
		return
	}
	_ = proc.Kill()
}

// execProgram runs the target program as a child process and returns its exit
// code. Windows has no execve-style image replacement, so the helper proxies
// stdio and forwards the result.
func execProgram(path string, args []string, env []string) int {
	cmd := exec.Command(path)
	if len(args) > 1 {
		cmd.Args = args
	}
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		return 127
	}
	return 0
}
