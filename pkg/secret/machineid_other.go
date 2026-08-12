//go:build !linux && !windows

package secret

// machineID has no portable equivalent on the remaining platforms. macOS
// hardware reports real disk serials, so the hardware tier covers it; a
// macOS VM without one falls through to the constant tier.
func machineID() string { return "" }
