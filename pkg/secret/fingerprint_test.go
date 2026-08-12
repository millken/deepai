package secret

import (
	"testing"
)

// withDisks replaces disk enumeration for one test.
func withDisks(t *testing.T, disks []diskInfo) {
	t.Helper()
	prev := listDisks
	listDisks = func() []diskInfo { return disks }
	t.Cleanup(func() { listDisks = prev })
}

func TestDiskSourcesSkipsRemovable(t *testing.T) {
	// A USB stick attached at seal time would otherwise become an extra
	// decryption path that travels with whoever holds the stick.
	withDisks(t, []diskInfo{
		{Serial: "S7U4NU0Y444140F", Removable: false},
		{Serial: "USBSTICK12345", Removable: true},
	})

	got := diskSources()
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1: %+v", len(got), got)
	}
	if got[0].value != "S7U4NU0Y444140F" {
		t.Errorf("value = %q, want the fixed disk's serial", got[0].value)
	}
	if got[0].mode != ModeHardware {
		t.Errorf("mode = %v, want ModeHardware", got[0].mode)
	}
}

func TestDiskSourcesFiltersDegenerateSerials(t *testing.T) {
	// ghw reports the literal "unknown" when it cannot read a serial. Using
	// it would hand every such machine the same key.
	withDisks(t, []diskInfo{
		{Serial: "unknown"},
		{Serial: ""},
		{Serial: "   "},
		{Serial: "0000000000000000"},
		{Serial: "abc"},
		{Serial: "2425130401001"},
	})

	got := diskSources()
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1: %+v", len(got), got)
	}
	if got[0].value != "2425130401001" {
		t.Errorf("value = %q, want 2425130401001", got[0].value)
	}
}

func TestDiskSourcesSortedForStableWrapOrder(t *testing.T) {
	withDisks(t, []diskInfo{
		{Serial: "ZZZZ111111"},
		{Serial: "AAAA222222"},
		{Serial: "MMMM333333"},
	})

	got := diskSources()
	want := []string{"AAAA222222", "MMMM333333", "ZZZZ111111"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].value != w {
			t.Errorf("[%d] = %q, want %q", i, got[i].value, w)
		}
	}
}

func TestDiskSourcesDeduplicates(t *testing.T) {
	// Some controllers report the same serial for multiple namespaces;
	// duplicate wraps add bytes without adding resilience.
	withDisks(t, []diskInfo{
		{Serial: "SAME111111"},
		{Serial: "SAME111111"},
	})

	got := diskSources()
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1: %+v", len(got), got)
	}
}

func TestDefaultDiscoverAllUsesDiskTierWhenAvailable(t *testing.T) {
	withDisks(t, []diskInfo{{Serial: "S7U4NU0Y444140F"}})

	groups := defaultDiscoverAll()
	if len(groups) != 3 {
		t.Fatalf("len(groups) = %d, want 3", len(groups))
	}
	if len(groups[0]) != 1 {
		t.Fatalf("hardware tier = %+v, want 1 source", groups[0])
	}
	if groups[0][0].value != "S7U4NU0Y444140F" {
		t.Errorf("hardware source = %q", groups[0][0].value)
	}
	if len(groups[2]) != 1 || groups[2][0].mode != ModeObfuscate {
		t.Errorf("constant tier = %+v, want exactly one ModeObfuscate source", groups[2])
	}
}

func TestDefaultDiscoverAllEmptyHardwareTierWhenNoDisks(t *testing.T) {
	withDisks(t, nil)

	groups := defaultDiscoverAll()
	if len(groups[0]) != 0 {
		t.Errorf("hardware tier = %+v, want empty", groups[0])
	}
}

// withMachineID replaces the OS machine ID lookup for one test.
func withMachineID(t *testing.T, id string) {
	t.Helper()
	prev := machineIDFn
	machineIDFn = func() string { return id }
	t.Cleanup(func() { machineIDFn = prev })
}

func TestInstallTierUsedOnlyWhenNoDisks(t *testing.T) {
	withMachineID(t, "d28d273a06f44c9b9c9c5bc966b0c43d")

	// Disks present: the install tier must stay empty so its copyable
	// material never joins a hardware-bound seal.
	withDisks(t, []diskInfo{{Serial: "S7U4NU0Y444140F"}})
	groups := defaultDiscoverAll()
	if len(groups[0]) == 0 {
		t.Fatal("hardware tier should be populated")
	}
	if len(groups[1]) != 0 {
		t.Errorf("install tier = %+v, want empty while disks exist", groups[1])
	}

	// No disks: the install tier takes over.
	withDisks(t, nil)
	groups = defaultDiscoverAll()
	if len(groups[1]) != 1 {
		t.Fatalf("install tier = %+v, want 1 source", groups[1])
	}
	if groups[1][0].mode != ModeInstall {
		t.Errorf("mode = %v, want ModeInstall", groups[1][0].mode)
	}
	if groups[1][0].value != "d28d273a06f44c9b9c9c5bc966b0c43d" {
		t.Errorf("value = %q", groups[1][0].value)
	}
}

func TestInstallTierRejectsDegenerateMachineID(t *testing.T) {
	withDisks(t, nil)
	withMachineID(t, "")

	groups := defaultDiscoverAll()
	if len(groups[1]) != 0 {
		t.Errorf("install tier = %+v, want empty for an unreadable machine ID", groups[1])
	}
	// The constant tier must still be there, so sealing never fails and
	// never falls back to plaintext.
	if len(groups[2]) != 1 {
		t.Errorf("constant tier = %+v, want exactly one source", groups[2])
	}
}

func TestSealOnCloudHostStillProducesCiphertext(t *testing.T) {
	// A cloud instance with no disk serial and no machine ID must still
	// seal: the alternative is storing the key in plaintext.
	withDisks(t, nil)
	withMachineID(t, "")

	prev := discoverAll
	discoverAll = defaultDiscoverAll
	t.Cleanup(func() { discoverAll = prev })

	sealed, err := Seal(testKey)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !IsSealed(sealed) {
		t.Fatal("value is not sealed")
	}
	h, err := Inspect(sealed)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if h.Mode != ModeObfuscate {
		t.Errorf("Mode = %v, want ModeObfuscate", h.Mode)
	}
	got, err := Reveal(sealed)
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	if got != testKey {
		t.Errorf("Reveal = %q, want %q", got, testKey)
	}
}
