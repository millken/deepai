//go:build windows

package docx

import "errors"

// mkfifo has no Windows equivalent; TestOpen_RejectsNonRegularFile skips
// before this is ever called there.
func mkfifo(path string) error {
	return errors.New("FIFOs are not supported on windows")
}
