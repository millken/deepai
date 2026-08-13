package secret

import (
	"encoding/base64"
	"strings"
	"testing"
)

// withSources replaces source discovery for one test.
func withSources(t *testing.T, tiers [][]source) {
	t.Helper()
	prev := discoverAll
	discoverAll = func() [][]source { return tiers }
	t.Cleanup(func() { discoverAll = prev })
}

func hardware(values ...string) []source {
	out := make([]source, 0, len(values))
	for _, v := range values {
		out = append(out, source{mode: ModeHardware, value: v})
	}
	return out
}

func constantTier() []source {
	return []source{{mode: ModeObfuscate, value: obfuscationConstant}}
}

const testKey = "sk-ant-api03-ZZZZfake0000000000000000000000000000"

func TestSealRevealRoundTrip(t *testing.T) {
	withSources(t, [][]source{hardware("SERIAL-AAAA1"), nil, constantTier()})

	sealed, err := Seal(testKey)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !strings.HasPrefix(sealed, "enc:v1:") {
		t.Fatalf("sealed value = %q, want enc:v1: prefix", sealed)
	}
	if strings.Contains(sealed, testKey) {
		t.Fatal("sealed value contains the plaintext key")
	}

	got, err := Reveal(sealed)
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	if got != testKey {
		t.Errorf("Reveal = %q, want %q", got, testKey)
	}
}

func TestSealNeverLeaksSourceValue(t *testing.T) {
	const serial = "SERIAL-AAAA1"
	withSources(t, [][]source{hardware(serial), nil, constantTier()})

	sealed, err := Seal(testKey)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	blob, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealed, "enc:v1:"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The serial is the only secret material; the blob must carry no trace of it.
	if strings.Contains(string(blob), serial) {
		t.Error("blob contains the raw source value")
	}
	if strings.Contains(strings.ToLower(string(blob)), "disk-serial") {
		t.Error("blob contains the tier name")
	}
}

func TestRevealPassesThroughPlaintext(t *testing.T) {
	withSources(t, [][]source{hardware("SERIAL-AAAA1"), nil, constantTier()})

	for _, raw := range []string{"", "sk-plain-12345", "not:sealed:at:all"} {
		got, err := Reveal(raw)
		if err != nil {
			t.Errorf("Reveal(%q) returned error %v, want passthrough", raw, err)
		}
		if got != raw {
			t.Errorf("Reveal(%q) = %q, want unchanged", raw, got)
		}
	}
}

func TestRevealRejectsTamperedCiphertext(t *testing.T) {
	withSources(t, [][]source{hardware("SERIAL-AAAA1"), nil, constantTier()})

	sealed, err := Seal(testKey)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	blob, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealed, "enc:v1:"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	blob[len(blob)-1] ^= 0xff
	tampered := "enc:v1:" + base64.RawURLEncoding.EncodeToString(blob)

	if _, err := Reveal(tampered); err == nil {
		t.Fatal("Reveal accepted a tampered ciphertext")
	}
}

// N-of-M: sealed against two disks, only one survives.
func TestRevealSucceedsWithOneSurvivingSource(t *testing.T) {
	withSources(t, [][]source{hardware("SERIAL-AAAA1", "SERIAL-BBBB2"), nil, constantTier()})
	sealed, err := Seal(testKey)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Second disk replaced; the first still matches.
	withSources(t, [][]source{hardware("SERIAL-AAAA1", "SERIAL-CCCC3"), nil, constantTier()})
	got, err := Reveal(sealed)
	if err != nil {
		t.Fatalf("Reveal with one surviving source: %v", err)
	}
	if got != testKey {
		t.Errorf("Reveal = %q, want %q", got, testKey)
	}
}

func TestRevealFailsWhenAllSourcesChanged(t *testing.T) {
	withSources(t, [][]source{hardware("SERIAL-AAAA1", "SERIAL-BBBB2"), nil, constantTier()})
	sealed, err := Seal(testKey)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Ciphertext copied to a different machine.
	withSources(t, [][]source{hardware("OTHER-XXXX1"), nil, constantTier()})
	_, err = Reveal(sealed)
	if err == nil {
		t.Fatal("Reveal succeeded on a foreign machine")
	}
	msg := err.Error()
	if strings.Contains(msg, testKey) {
		t.Error("error message leaks the key")
	}
	if !strings.Contains(msg, "2") {
		t.Errorf("error should report the wrap count, got %q", msg)
	}
	if !strings.Contains(msg, "hardware-bound") {
		t.Errorf("error should report the seal mode, got %q", msg)
	}
}

func TestRevealRejectsUnknownVersion(t *testing.T) {
	withSources(t, [][]source{hardware("SERIAL-AAAA1"), nil, constantTier()})

	// A future format must never be mistaken for a plaintext key and sent
	// to a provider verbatim.
	_, err := Reveal("enc:v2:AAAAAAAA")
	if err == nil {
		t.Fatal("Reveal accepted an unknown format version")
	}
	if !strings.Contains(err.Error(), "enc:v1:") {
		t.Errorf("error should name the supported format, got %q", err)
	}
}

// Sealing must never mix a weaker tier into a stronger one: a universal
// wrap on every blob would defeat machine binding everywhere at once.
func TestSealUsesHighestTierOnly(t *testing.T) {
	withSources(t, [][]source{
		hardware("SERIAL-AAAA1"),
		{{mode: ModeInstall, value: "machine-id-value-here"}},
		constantTier(),
	})
	sealed, err := Seal(testKey)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	h, err := Inspect(sealed)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if h.Mode != ModeHardware {
		t.Errorf("Mode = %v, want ModeHardware", h.Mode)
	}
	if h.Wraps != 1 {
		t.Errorf("Wraps = %d, want 1 (lower tiers must not be wrapped)", h.Wraps)
	}

	// Only the machine ID remains: the blob must not open.
	withSources(t, [][]source{nil, {{mode: ModeInstall, value: "machine-id-value-here"}}, constantTier()})
	if _, err := Reveal(sealed); err == nil {
		t.Fatal("a hardware-bound blob opened with install-tier material")
	}
}

func TestSealFallsBackDownTiers(t *testing.T) {
	withSources(t, [][]source{nil, {{mode: ModeInstall, value: "machine-id-value-here"}}, constantTier()})
	sealed, err := Seal(testKey)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	h, err := Inspect(sealed)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if h.Mode != ModeInstall {
		t.Errorf("Mode = %v, want ModeInstall", h.Mode)
	}

	withSources(t, [][]source{nil, nil, constantTier()})
	sealed, err = Seal(testKey)
	if err != nil {
		t.Fatalf("Seal at constant tier: %v", err)
	}
	h, err = Inspect(sealed)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if h.Mode != ModeObfuscate {
		t.Errorf("Mode = %v, want ModeObfuscate", h.Mode)
	}
}

func TestSealRejectsEmptySourceSet(t *testing.T) {
	withSources(t, [][]source{nil, nil, nil})
	if _, err := Seal(testKey); err == nil {
		t.Fatal("Seal succeeded with no binding sources")
	}
}

func TestSealEmptyPlaintext(t *testing.T) {
	withSources(t, [][]source{hardware("SERIAL-AAAA1"), nil, constantTier()})
	sealed, err := Seal("")
	if err != nil {
		t.Fatalf("Seal(\"\"): %v", err)
	}
	got, err := Reveal(sealed)
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	if got != "" {
		t.Errorf("Reveal = %q, want empty", got)
	}
}

func TestIsSealed(t *testing.T) {
	cases := map[string]bool{
		"":                false,
		"sk-plain":        false,
		"enc:v1:AAAA":     true,
		"enc:v2:AAAA":     true, // future versions are sealed, just unsupported
		"encrypted:thing": false,
	}
	for raw, want := range cases {
		if got := IsSealed(raw); got != want {
			t.Errorf("IsSealed(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestFingerprintHidesSourceValues(t *testing.T) {
	const serial = "SERIAL-AAAA1"
	withSources(t, [][]source{hardware(serial), nil, constantTier()})

	info := Fingerprint()
	if info.Mode != ModeHardware {
		t.Errorf("Mode = %v, want ModeHardware", info.Mode)
	}
	if len(info.Sources) != 2 {
		t.Fatalf("len(Sources) = %d, want 2 (one disk + constant)", len(info.Sources))
	}
	for _, s := range info.Sources {
		if strings.Contains(s.Digest, serial) {
			t.Error("Digest leaks the source value")
		}
		if len(s.Digest) != 8 {
			t.Errorf("Digest = %q, want 8 hex chars", s.Digest)
		}
	}
	if !info.Sources[0].Used || info.Sources[0].Tier != "disk-serial" {
		t.Errorf("first source = %+v, want used disk-serial", info.Sources[0])
	}
	if info.Sources[1].Used {
		t.Error("constant tier must be reported as unused when a disk exists")
	}
}

func TestBlobLayoutSizes(t *testing.T) {
	if wrapLen != 60 {
		t.Errorf("wrapLen = %d, want 60", wrapLen)
	}
	if headerLen != 7 {
		t.Errorf("headerLen = %d, want 7", headerLen)
	}

	withSources(t, [][]source{hardware("SERIAL-AAAA1", "SERIAL-BBBB2"), nil, constantTier()})
	sealed, err := Seal(strings.Repeat("k", 108))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	blob, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealed, "enc:v1:"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 7 header + 2*60 wraps + 12 nonce + 108 plaintext + 16 tag
	if want := 7 + 2*60 + 12 + 108 + 16; len(blob) != want {
		t.Errorf("blob length = %d, want %d", len(blob), want)
	}
}

func TestUsableIDFiltersDegenerateValues(t *testing.T) {
	rejected := []string{
		"", "   ", "unknown", "UNKNOWN", "none", "N/A", "na",
		"not specified", "Not Available", "To Be Filled By O.E.M.",
		"Default string", "abc", "12345",
		"000000000000", "0000-0000-0000", "0 0 0 0 0 0",
	}
	for _, raw := range rejected {
		if got := usableID(raw); got != "" {
			t.Errorf("usableID(%q) = %q, want \"\"", raw, got)
		}
	}
	accepted := map[string]string{
		"S7U4NU0Y444140F":                      "S7U4NU0Y444140F",
		"2425130401001":                        "2425130401001",
		"  d28d273a06f44c9b9c9c5bc966b0c43d  ": "d28d273a06f44c9b9c9c5bc966b0c43d",
	}
	for raw, want := range accepted {
		if got := usableID(raw); got != want {
			t.Errorf("usableID(%q) = %q, want %q", raw, got, want)
		}
	}
}
