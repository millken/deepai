//go:build linux

package secret

import (
	"os"
	"strings"
)

// machineIDFiles are the standard locations of the systemd machine ID, in
// preference order. A package variable so tests need no root.
var machineIDFiles = []string{"/etc/machine-id", "/var/lib/dbus/machine-id"}

func machineID() string {
	for _, p := range machineIDFiles {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if id := strings.TrimSpace(string(b)); id != "" {
			return id
		}
	}
	return ""
}
