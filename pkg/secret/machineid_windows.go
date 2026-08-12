//go:build windows

package secret

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// machineID reads the Windows MachineGuid, generated at install time.
// WOW64_64KEY makes a 32-bit build read the same 64-bit view of the
// registry as a 64-bit one, so the value does not change with build arch.
func machineID() string {
	k, err := registry.OpenKey(
		registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Cryptography`,
		registry.QUERY_VALUE|registry.WOW64_64KEY,
	)
	if err != nil {
		return ""
	}
	defer k.Close()

	v, _, err := k.GetStringValue("MachineGuid")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}
