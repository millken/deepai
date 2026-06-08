//go:build !linux

package sandbox

import "errors"

var errLandlockUnsupported = errors.New("landlock is only supported on linux")

// CheckLandlockAvailable always reports false on non-Linux platforms.
func CheckLandlockAvailable() bool {
	return false
}

func probeLandlock(sessionDir string) error {
	return errLandlockUnsupported
}

func applyLandlock(sessionDir string) error {
	return errLandlockUnsupported
}
