//go:build !windows

package docx

import "syscall"

// mkfifo creates a FIFO for TestOpen_RejectsNonRegularFile.
func mkfifo(path string) error {
	return syscall.Mkfifo(path, 0o600)
}
