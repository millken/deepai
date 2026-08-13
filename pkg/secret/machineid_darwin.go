//go:build darwin

package secret

import (
	"os/exec"
	"strings"
)

// machineID returns the IOPlatformUUID on macOS, which is the hardware UUID
// shown in "About This Mac". It is stable across reboots, unique per
// machine, and readable without root via ioreg. This fills the role that
// /etc/machine-id plays on Linux — the install-binding tier used when no
// disk serial is available (common on Apple Silicon, whose internal NVMe
// does not expose a serial through IOBlockStorageDriver).
func machineID() string {
	out, err := exec.Command("ioreg", "-d2", "-c", "IOPlatformExpertDevice").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "IOPlatformUUID") {
			continue
		}
		// Line shape: "IOPlatformUUID" = "XXXXXXXX-XXXX-XXXX-XXXX-XXXXXXXXXXXX"
		parts := strings.Split(line, "\"")
		// parts: ["", "IOPlatformUUID", " = ", "UUID", ""]
		if len(parts) < 4 {
			continue
		}
		return strings.TrimSpace(parts[3])
	}
	return ""
}
