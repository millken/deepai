//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"syscall"
)

// detach starts the child in its own session so it has no controlling
// terminal: closing the launching shell does not SIGHUP it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func terminate(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}
