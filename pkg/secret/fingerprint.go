package secret

import "strings"

// Mode identifies which tier of binding material a sealed value was created
// with. It is stored as one byte in the blob header and drives diagnostics
// only — it never reveals anything about the material itself.
type Mode uint8

const (
	// ModeHardware binds to physical disk serial numbers. Copying the
	// ciphertext to another machine makes it undecryptable.
	ModeHardware Mode = 1
	// ModeInstall binds to the OS machine ID, used when no disk serial is
	// available (cloud instances, WSL2, some VMs). The machine ID is a file
	// or registry value and so can be copied along with the ciphertext,
	// which is why it never participates at ModeHardware strength.
	ModeInstall Mode = 2
	// ModeObfuscate binds to a compiled-in constant and therefore provides
	// no machine binding at all: anyone holding the ciphertext and this
	// binary can decrypt it. It still blocks the primary threat — an agent
	// reading .env gets ciphertext rather than a usable key — which is
	// strictly better than storing plaintext.
	ModeObfuscate Mode = 3
)

func (m Mode) String() string {
	switch m {
	case ModeHardware:
		return "hardware-bound"
	case ModeInstall:
		return "install-bound"
	case ModeObfuscate:
		return "obfuscation-only"
	default:
		return "unknown"
	}
}

// tier is the HKDF domain-separation label for a mode. Distinct labels keep
// the same value from deriving the same KEK at two different tiers.
func (m Mode) tier() string {
	switch m {
	case ModeHardware:
		return "disk-serial"
	case ModeInstall:
		return "machine-id"
	case ModeObfuscate:
		return "constant"
	default:
		return "unknown"
	}
}

// source is one piece of machine-bound key material.
type source struct {
	mode  Mode
	value string
}

// obfuscationConstant is the last-resort binding material. It is public, so
// it provides no confidentiality — see ModeObfuscate.
const obfuscationConstant = "deepai-no-machine-binding-v1"

// discoverAll returns candidate sources grouped by descending strength:
// index 0 is ModeHardware, 1 is ModeInstall, 2 is ModeObfuscate. Earlier
// groups may be empty; the last is always populated. Replaced in tests.
var discoverAll = defaultDiscoverAll

func defaultDiscoverAll() [][]source {
	return [][]source{
		nil, // ModeHardware — filled in by disk serial discovery
		nil, // ModeInstall — filled in by OS machine ID lookup
		{{mode: ModeObfuscate, value: obfuscationConstant}},
	}
}

// sealSources returns the material to seal with: every source from the
// strongest available tier and nothing from weaker ones. Mixing a weaker
// tier in would put a universally-openable wrap on every blob, collapsing
// machine binding for all of them at once.
func sealSources() []source {
	for _, group := range discoverAll() {
		if len(group) > 0 {
			return group
		}
	}
	return nil
}

// candidates returns every source that could unwrap a blob on this host,
// across all tiers. Unlike sealSources this is deliberately permissive:
// a blob sealed at a weaker tier must still open here.
func candidates() []source {
	var out []source
	for _, group := range discoverAll() {
		out = append(out, group...)
	}
	return out
}

// placeholderIDs are values that firmware, DMI tables, and ghw itself report
// when a real identifier is unavailable — ghw returns the literal "unknown".
// Any of them would become key material shared by every machine that reports
// it, so none may ever be used for binding.
//
// "none", "n/a", and "na" are deliberately absent: all three are shorter
// than minIDLen and are already rejected by the length check below, so
// listing them here would be dead code.
var placeholderIDs = map[string]bool{
	"unknown":                true,
	"not specified":          true,
	"not available":          true,
	"to be filled by o.e.m.": true,
	"default string":         true,
	"system serial number":   true,
}

// minIDLen rejects identifiers too short to carry meaningful entropy.
const minIDLen = 6

// usableID normalizes an identifier and returns "" if it is degenerate.
func usableID(raw string) string {
	s := strings.TrimSpace(raw)
	if len(s) < minIDLen {
		return ""
	}
	if placeholderIDs[strings.ToLower(s)] {
		return ""
	}
	if isZeroish(s) {
		return ""
	}
	return s
}

// isZeroish reports whether s carries no entropy — only zeros and
// separators, the shape some firmware reports for an absent serial.
func isZeroish(s string) bool {
	for _, r := range s {
		switch r {
		case '0', '.', '-', '_', ' ', ':':
		default:
			return false
		}
	}
	return true
}
