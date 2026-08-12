//go:build linux

package secret

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLinuxMachineIDReadsFirstAvailableFile(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	present := filepath.Join(dir, "present")
	if err := os.WriteFile(present, []byte("d28d273a06f44c9b9c9c5bc966b0c43d\n"), 0644); err != nil {
		t.Fatal(err)
	}

	prev := machineIDFiles
	machineIDFiles = []string{missing, present}
	t.Cleanup(func() { machineIDFiles = prev })

	if got := machineID(); got != "d28d273a06f44c9b9c9c5bc966b0c43d" {
		t.Errorf("machineID() = %q, want the trimmed contents of the second file", got)
	}
}

func TestLinuxMachineIDEmptyWhenNoFiles(t *testing.T) {
	dir := t.TempDir()
	prev := machineIDFiles
	machineIDFiles = []string{filepath.Join(dir, "nope")}
	t.Cleanup(func() { machineIDFiles = prev })

	if got := machineID(); got != "" {
		t.Errorf("machineID() = %q, want empty", got)
	}
}
