# API Key 密封 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 `~/.deepai/.env` 里的 API key 明文替换成绑定本机磁盘序列号的密文，使任何 CLI agent 读取该文件的收益归零，同时不破坏任何现有的密钥消费路径。

**Architecture:** 新增 `pkg/secret` 包，两层职责严格分离：`secret.go` 只管密封格式与 AEAD 运算（不知道硬件存在），`fingerprint.go` 只管发现绑定材料（不知道加密存在），二者通过 `[]source` 通信。随机 DEK 加密密钥本体，DEK 按每个绑定源各包裹一份（N-of-M），因此换掉两块盘中的一块不会锁死。密钥继续存在 `.env` 里、带 `enc:v1:` 前缀，解密发生在使用点，所以 `root.go` 的加载逻辑、现存明文密钥、`export FOO=...` 全部不变。

**Tech Stack:** Go 1.26、`crypto/hkdf`（Go 1.24+ 标准库）、`crypto/aes` + `crypto/cipher` GCM、`github.com/jaypipes/ghw` v0.25.0（跨平台磁盘序列号）、`golang.org/x/sys/windows/registry`（已是现有依赖）、`charm.land/huh/v2`（交互录入）、`spf13/cobra`。

设计文档：`docs/superpowers/specs/2026-08-12-apikey-sealing-design.md`

## Global Constraints

- 包路径前缀 `github.com/millken/deepai`。
- 密文前缀 `enc:`，当前格式的完整前缀 `enc:v1:`，blob 魔数 `DPK1`，blob 版本字节 `1`。
- 数据层 AAD 字符串 `deepai-secret-v1`；wrap 层 AAD 字符串 `deepai-kek-v1`；HKDF `info` 为 `deepai-kek-v1/` 拼接层级名。
- 层级名固定三个值：`disk-serial`（mode 1）、`machine-id`（mode 2）、`constant`（mode 3）。
- DEK 32 字节，GCM nonce 12 字节，GCM tag 16 字节，故每个 wrap 恰好 60 字节，blob 头恰好 7 字节。
- **blob 内不得写入任何源值的痕迹**（不含源名、序列号、哈希、摘要）。`buildSecret` 与格式均公开，序列号是唯一机密材料；一旦写入，`.env` 就成了解密它自己的钥匙。
- **密封只取最高可用层级；解密要试全部层级。** 反过来会让每个硬件绑定密文都带一个万能 wrap。
- 未知版本必须报错，**不得**静默当明文透传。
- 解密失败必须返回错误并中止 provider 初始化，**不得**降级为空 key。
- 错误信息与日志中永不含密钥内容。
- 任何时刻不得在磁盘上留下明文密钥副本（不写 `.env.bak`）。
- 不提供导出明文的命令或导出函数（无 `Show`/`Export`/`Dump`）。
- 明文永久向后兼容：`Reveal` 对无 `enc:` 前缀的值原样返回且不报错。
- 注释与标识符用英文（与现有代码一致）；提交信息用英文。

---

### Task 1: `pkg/secret` 密封格式与 AEAD

建立密封格式、KEK 派生、`Seal`/`Reveal`/`IsSealed`/`Inspect`/`Fingerprint`。源发现此刻只是一个可注入的桩，只返回常量层级 —— 真实硬件采集在 Task 2/3 填入。这样整个加密层的测试完全不依赖硬件。

**Files:**
- Create: `pkg/secret/fingerprint.go`
- Create: `pkg/secret/secret.go`
- Test: `pkg/secret/secret_test.go`

**Interfaces:**
- Consumes: 无（本任务是叶子）
- Produces:
  - `type Mode uint8`，常量 `ModeHardware Mode = 1`、`ModeInstall Mode = 2`、`ModeObfuscate Mode = 3`
  - `func (m Mode) String() string`（`"hardware-bound"` / `"install-bound"` / `"obfuscation-only"` / `"unknown"`）
  - `func (m Mode) tier() string`（`"disk-serial"` / `"machine-id"` / `"constant"` / `"unknown"`，未导出）
  - `type source struct { mode Mode; value string }`（未导出）
  - `var discoverAll func() [][]source`（包级变量，测试可替换；返回三个层级组，下标 0=层级 1，2=层级 3）
  - `const obfuscationConstant = "deepai-no-machine-binding-v1"`（未导出）
  - `func usableID(raw string) string`（未导出，退化值过滤，Task 2 复用）
  - `func Seal(plaintext string) (string, error)`
  - `func Reveal(raw string) (string, error)`
  - `func IsSealed(raw string) bool`
  - `type Header struct { Mode Mode; Wraps int }`
  - `func Inspect(raw string) (Header, error)`
  - `type SourceInfo struct { Tier string; Digest string; Used bool }`
  - `type Info struct { Mode Mode; Sources []SourceInfo }`
  - `func Fingerprint() Info`

- [ ] **Step 1: 写下失败的测试**

创建 `pkg/secret/secret_test.go`：

```go
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
		"S7U4NU0Y444140F":                  "S7U4NU0Y444140F",
		"2425130401001":                    "2425130401001",
		"  d28d273a06f44c9b9c9c5bc966b0c43d  ": "d28d273a06f44c9b9c9c5bc966b0c43d",
	}
	for raw, want := range accepted {
		if got := usableID(raw); got != want {
			t.Errorf("usableID(%q) = %q, want %q", raw, got, want)
		}
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./pkg/secret/ -v 2>&1 | head -20
```

Expected: 编译失败，`undefined: source`、`undefined: Seal` 等。

- [ ] **Step 3: 写 `fingerprint.go`（含常量层级桩）**

创建 `pkg/secret/fingerprint.go`：

```go
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
var placeholderIDs = map[string]bool{
	"unknown":                true,
	"none":                   true,
	"n/a":                    true,
	"na":                     true,
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
```

- [ ] **Step 4: 写 `secret.go`**

创建 `pkg/secret/secret.go`：

```go
// Package secret seals API keys so that reading ~/.deepai/.env yields
// ciphertext rather than a usable credential. A random data key encrypts the
// secret and is wrapped once per machine-bound source (disk serial numbers,
// or weaker fallbacks), so replacing one of several disks does not lock the
// user out while copying the file to another machine still fails.
//
// This is not cryptographic protection against a local attacker. The binary
// runs as the user, buildSecret is public, and the binding material is
// readable on the host — so a deliberate local extraction succeeds. What it
// does provide is a barrier against accidental exfiltration: an agent that
// reads the file, a config pasted into a chat, a .env committed to git.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os/user"
	"strings"
	"sync"
)

// buildSecret is the compiled-in HKDF input keying material. It is public in
// this repository and provides domain separation only — confidentiality rests
// entirely on the machine-bound material in fingerprint.go. Keeping it a
// fixed default means rebuilding deepai never invalidates stored keys.
// Override at build time if desired:
//
//	go build -ldflags "-X github.com/millken/deepai/pkg/secret.buildSecret=..."
var buildSecret = "deepai-apikey-sealing-v1"

const (
	// sealPrefix marks any sealed value, current or future. IsSealed matches
	// on this rather than on the versioned prefix so that a value from a
	// newer format is reported as sealed-but-unsupported instead of being
	// mistaken for a plaintext key.
	sealPrefix = "enc:"
	// v1Prefix is the complete prefix of the format this binary writes.
	v1Prefix = "enc:v1:"

	magic   = "DPK1"
	version = 1

	dekLen   = 32
	nonceLen = 12
	tagLen   = 16
	wrapLen  = nonceLen + dekLen + tagLen // 60
	// headerLen covers magic(4) + version(1) + mode(1) + wrapCount(1).
	headerLen = len(magic) + 3

	dataAAD = "deepai-secret-v1"
	wrapAAD = "deepai-kek-v1"
	kekInfo = "deepai-kek-v1/"
)

// userID salts the KEK so a sealed value is bound to one account as well as
// one machine. user.Current().Uid is the portable choice: it is the numeric
// uid on Unix and the SID on Windows, where os.Getuid() returns -1.
var userID = sync.OnceValue(func() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.Uid
})

// IsSealed reports whether raw is in sealed form.
func IsSealed(raw string) bool {
	return strings.HasPrefix(raw, sealPrefix)
}

// Header describes a sealed value without decrypting it.
type Header struct {
	Mode  Mode
	Wraps int
}

// Inspect parses a sealed value's header. It needs no binding material and
// so works even on a machine that cannot decrypt the value.
func Inspect(raw string) (Header, error) {
	p, err := parseBlob(raw)
	if err != nil {
		return Header{}, err
	}
	return Header{Mode: p.mode, Wraps: len(p.wraps)}, nil
}

// Seal encrypts plaintext, binding it to this machine.
func Seal(plaintext string) (string, error) {
	srcs := sealSources()
	if len(srcs) == 0 {
		return "", errors.New("secret: no binding sources available on this host")
	}

	dek := make([]byte, dekLen)
	if _, err := rand.Read(dek); err != nil {
		return "", fmt.Errorf("secret: generate data key: %w", err)
	}
	dataAEAD, err := newAEAD(dek)
	if err != nil {
		return "", err
	}
	dataNonce := make([]byte, nonceLen)
	if _, err := rand.Read(dataNonce); err != nil {
		return "", fmt.Errorf("secret: generate nonce: %w", err)
	}
	dataCT := dataAEAD.Seal(nil, dataNonce, []byte(plaintext), []byte(dataAAD))

	blob := make([]byte, 0, headerLen+len(srcs)*wrapLen+nonceLen+len(dataCT))
	blob = append(blob, magic...)
	// Every source in srcs comes from a single tier, so srcs[0] names the
	// mode of the whole blob.
	blob = append(blob, version, byte(srcs[0].mode), byte(len(srcs)))

	uid := userID()
	for _, s := range srcs {
		k, err := deriveKEK(s, uid)
		if err != nil {
			return "", err
		}
		a, err := newAEAD(k)
		if err != nil {
			return "", err
		}
		n := make([]byte, nonceLen)
		if _, err := rand.Read(n); err != nil {
			return "", fmt.Errorf("secret: generate nonce: %w", err)
		}
		blob = append(blob, n...)
		blob = append(blob, a.Seal(nil, n, dek, []byte(wrapAAD))...)
	}
	blob = append(blob, dataNonce...)
	blob = append(blob, dataCT...)

	return v1Prefix + base64.RawURLEncoding.EncodeToString(blob), nil
}

// Reveal decrypts a sealed value. Values without the sealed prefix are
// returned unchanged, which is what keeps existing plaintext keys working.
func Reveal(raw string) (string, error) {
	if !IsSealed(raw) {
		return raw, nil
	}
	p, err := parseBlob(raw)
	if err != nil {
		return "", err
	}

	// Try every candidate source against every wrap. Both sides are small
	// (a handful each), and letting GCM's authentication tag decide which
	// pair matches avoids a whole class of source-matching bugs.
	uid := userID()
	cands := candidates()
	for _, s := range cands {
		k, err := deriveKEK(s, uid)
		if err != nil {
			continue
		}
		a, err := newAEAD(k)
		if err != nil {
			continue
		}
		for _, w := range p.wraps {
			dek, err := a.Open(nil, w[:nonceLen], w[nonceLen:], []byte(wrapAAD))
			if err != nil {
				continue
			}
			da, err := newAEAD(dek)
			if err != nil {
				return "", err
			}
			pt, err := da.Open(nil, p.dataNonce, p.dataCT, []byte(dataAAD))
			if err != nil {
				return "", errors.New("secret: sealed value is corrupt: the data key unwrapped but the payload failed authentication")
			}
			return string(pt), nil
		}
	}

	// Counts and mode come from the header and from this host's own
	// discovery — nothing is read back out of the encrypted material.
	return "", fmt.Errorf(
		"secret: cannot decrypt: value was sealed against %d source(s) in %s mode, and none of this host's %d candidate(s) match; the disk may have been replaced. Re-enter the key with `deepai key set`",
		len(p.wraps), p.mode, len(cands))
}

// SourceInfo describes one binding source for diagnostics. Digest is the
// first 8 hex characters of the value's SHA-256 — enough to tell two hosts
// apart, never enough to reconstruct the material.
type SourceInfo struct {
	Tier   string
	Digest string
	Used   bool
}

// Info reports what this host would seal with.
type Info struct {
	Mode    Mode
	Sources []SourceInfo
}

// Fingerprint returns diagnostics for `deepai key check`.
func Fingerprint() Info {
	groups := discoverAll()
	info := Info{}
	usedTier := -1
	for i, g := range groups {
		if len(g) > 0 && usedTier < 0 {
			usedTier = i
			info.Mode = g[0].mode
		}
	}
	for i, g := range groups {
		for _, s := range g {
			sum := sha256.Sum256([]byte(s.value))
			info.Sources = append(info.Sources, SourceInfo{
				Tier:   s.mode.tier(),
				Digest: hex.EncodeToString(sum[:])[:8],
				Used:   i == usedTier,
			})
		}
	}
	return info
}

// parsedBlob is a decoded sealed value, still encrypted.
type parsedBlob struct {
	mode      Mode
	wraps     [][]byte
	dataNonce []byte
	dataCT    []byte
}

func parseBlob(raw string) (parsedBlob, error) {
	body, ok := strings.CutPrefix(raw, v1Prefix)
	if !ok {
		// Never fall through to plaintext here: a value from a future format
		// would otherwise be sent to a provider as if it were the API key.
		label, _, _ := strings.Cut(strings.TrimPrefix(raw, sealPrefix), ":")
		return parsedBlob{}, fmt.Errorf("secret: unsupported sealed format %q; this binary supports %s — upgrade deepai", label, v1Prefix)
	}
	blob, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return parsedBlob{}, fmt.Errorf("secret: malformed sealed value: %w", err)
	}
	if len(blob) < headerLen || string(blob[:len(magic)]) != magic {
		return parsedBlob{}, errors.New("secret: malformed sealed value: bad magic")
	}
	if blob[4] != version {
		return parsedBlob{}, fmt.Errorf("secret: unsupported blob version %d; this binary supports %d — upgrade deepai", blob[4], version)
	}
	p := parsedBlob{mode: Mode(blob[5])}
	n := int(blob[6])
	if n == 0 {
		return parsedBlob{}, errors.New("secret: malformed sealed value: no wraps")
	}
	off := headerLen
	// Each wrap is fixed-size; the payload needs at least a nonce and a tag.
	if len(blob) < off+n*wrapLen+nonceLen+tagLen {
		return parsedBlob{}, errors.New("secret: malformed sealed value: truncated")
	}
	p.wraps = make([][]byte, n)
	for i := range p.wraps {
		p.wraps[i] = blob[off : off+wrapLen]
		off += wrapLen
	}
	p.dataNonce = blob[off : off+nonceLen]
	p.dataCT = blob[off+nonceLen:]
	return p, nil
}

// deriveKEK derives a source's key-encryption key. The source value is
// hashed into the HKDF salt and the tier name into info, so the same value
// never yields the same key at two tiers.
func deriveKEK(s source, uid string) ([]byte, error) {
	salt := sha256.Sum256([]byte(s.value + "\x00" + uid))
	k, err := hkdf.Key(sha256.New, []byte(buildSecret), salt[:], kekInfo+s.mode.tier(), dekLen)
	if err != nil {
		return nil, fmt.Errorf("secret: derive key: %w", err)
	}
	return k, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: init cipher: %w", err)
	}
	a, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: init GCM: %w", err)
	}
	return a, nil
}
```

- [ ] **Step 5: 运行测试确认通过**

```bash
go test ./pkg/secret/ -v 2>&1 | tail -40
```

Expected: 全部 `PASS`，包括 `TestSealUsesHighestTierOnly`、`TestRevealSucceedsWithOneSurvivingSource`、`TestUsableIDFiltersDegenerateValues`。

- [ ] **Step 6: 确认 wrap 与 header 长度符合设计**

```bash
go test ./pkg/secret/ -run TestSealRevealRoundTrip -v
```

再加一个断言长度的测试，追加到 `pkg/secret/secret_test.go`：

```go
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
```

```bash
go test ./pkg/secret/ -run TestBlobLayoutSizes -v
```

Expected: PASS，blob 长度 263。

- [ ] **Step 7: 提交**

```bash
gofmt -l pkg/secret/ && go vet ./pkg/secret/
git add pkg/secret/
git commit -m "feat(secret): seal API keys with a machine-bound wrapped data key

A random data key encrypts the secret and is wrapped once per binding
source, so a host that still has one of its several sources can decrypt
while a copy of the file on another machine cannot.

Sealing takes only the strongest available tier; revealing tries every
tier. Mixing a weaker tier into a seal would put a universally-openable
wrap on every blob and collapse machine binding for all of them.

The blob carries no trace of the source values it was sealed against --
not the names, not a digest. buildSecret and the format are both public,
so the binding material is the only secret; storing any of it alongside
the ciphertext would make the file the key to itself. Failure diagnostics
come from the wrap count and this host's own discovery instead.

Source discovery is a stub returning only the constant tier; disk serial
numbers and the OS machine ID land in following commits."
```

---

### Task 2: 层级 1 —— ghw 磁盘序列号

把真实的磁盘序列号采集接进层级 1。ghw 在 Linux 读 sysfs/udev、macOS 走 ioreg+plist、Windows 走 WMI，三者都不需要提权，因此这一层不需要任何 build tag。

**Files:**
- Modify: `go.mod`, `go.sum`（新增 ghw）
- Modify: `pkg/secret/fingerprint.go`（填充 `defaultDiscoverAll` 的层级 1）
- Test: `pkg/secret/fingerprint_test.go`

**Interfaces:**
- Consumes: Task 1 的 `source`、`Mode`、`usableID`、`discoverAll`、`defaultDiscoverAll`
- Produces:
  - `type diskInfo struct { Serial string; Removable bool }`（未导出）
  - `var listDisks func() []diskInfo`（包级变量，测试可替换；默认 `ghwListDisks`）
  - `func diskSources() []source`（未导出）

- [ ] **Step 1: 加入 ghw 依赖**

```bash
go get github.com/jaypipes/ghw@v0.25.0
```

Expected 输出包含：
```
go: added github.com/jaypipes/ghw v0.25.0
go: added github.com/jaypipes/pcidb v1.1.1
go: added github.com/yusufpapurcu/wmi v1.2.4
go: added howett.net/plist v1.0.2-...
go: added github.com/go-ole/go-ole v1.2.6
```

- [ ] **Step 2: 写下失败的测试**

创建 `pkg/secret/fingerprint_test.go`：

```go
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
```

- [ ] **Step 3: 运行测试确认失败**

```bash
go test ./pkg/secret/ -run 'TestDisk|TestDefaultDiscoverAll' -v 2>&1 | head -20
```

Expected: 编译失败，`undefined: diskInfo`、`undefined: listDisks`、`undefined: diskSources`。

- [ ] **Step 4: 实现磁盘采集**

在 `pkg/secret/fingerprint.go` 的 import 块改为：

```go
import (
	"sort"
	"strings"

	"github.com/jaypipes/ghw"
)
```

把 `defaultDiscoverAll` 替换为：

```go
func defaultDiscoverAll() [][]source {
	return [][]source{
		diskSources(),
		nil, // ModeInstall — filled in by OS machine ID lookup
		{{mode: ModeObfuscate, value: obfuscationConstant}},
	}
}
```

并在文件末尾追加：

```go
// diskInfo is the subset of a block device that matters for binding.
type diskInfo struct {
	Serial    string
	Removable bool
}

// listDisks enumerates block devices. Replaced in tests so the fingerprint
// logic can be exercised without real hardware.
var listDisks = ghwListDisks

// ghwListDisks reads block devices through ghw, which handles the platform
// differences itself: sysfs/udev on Linux, ioreg on macOS, WMI on Windows.
// None of those paths need root or admin. Only Block() is called, so ghw's
// ethtool shellout (a network-subsystem path) never runs.
func ghwListDisks() []diskInfo {
	info, err := ghw.Block()
	if err != nil {
		return nil
	}
	out := make([]diskInfo, 0, len(info.Disks))
	for _, d := range info.Disks {
		if d == nil {
			continue
		}
		out = append(out, diskInfo{Serial: d.SerialNumber, Removable: d.IsRemovable})
	}
	return out
}

// diskSources returns one binding source per fixed disk with a usable
// serial number, sorted so wrap order is stable across runs.
//
// Only SerialNumber is used, never WWN: cheap NVMe firmware reports
// meaningless near-zero WWNs (eui.0000000000000002 on one of the
// development machine's drives) while the serial is fine. Removable
// devices are skipped so a USB stick present at seal time does not become
// an extra decryption path.
//
// Motherboard and chassis serials are not used at all: on modern Linux
// kernels /sys/class/dmi/id/product_uuid, product_serial, and board_serial
// are 0400 root-only, precisely to prevent fingerprinting.
func diskSources() []source {
	seen := make(map[string]bool)
	var serials []string
	for _, d := range listDisks() {
		if d.Removable {
			continue
		}
		s := usableID(d.Serial)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		serials = append(serials, s)
	}
	sort.Strings(serials)

	out := make([]source, 0, len(serials))
	for _, s := range serials {
		out = append(out, source{mode: ModeHardware, value: s})
	}
	return out
}
```

- [ ] **Step 5: 运行测试确认通过**

```bash
go test ./pkg/secret/ -v 2>&1 | tail -40
```

Expected: 全部 PASS。

- [ ] **Step 6: 在真实硬件上确认能采到源**

创建临时验证测试 `pkg/secret/live_probe_test.go`：

```go
package secret

import "testing"

// TestLiveFingerprint is a diagnostic, not an assertion: it prints what this
// host actually offers so the tier and source count can be eyeballed.
func TestLiveFingerprint(t *testing.T) {
	info := Fingerprint()
	t.Logf("mode = %v", info.Mode)
	for _, s := range info.Sources {
		t.Logf("  tier=%-12s digest=%s used=%v", s.Tier, s.Digest, s.Used)
	}
}
```

```bash
go test ./pkg/secret/ -run TestLiveFingerprint -v
```

Expected（开发机有两块非移动 NVMe）：`mode = hardware-bound`，两条 `tier=disk-serial used=true`，一条 `tier=constant used=false`。

若 `mode` 不是 `hardware-bound`，说明磁盘序列号没采到，先排查再继续。确认后删除该文件：

```bash
rm pkg/secret/live_probe_test.go
```

- [ ] **Step 7: 提交**

```bash
gofmt -l pkg/secret/ && go vet ./pkg/secret/
git add go.mod go.sum pkg/secret/
git commit -m "feat(secret): bind sealed keys to fixed-disk serial numbers

ghw reads block devices through sysfs/udev on Linux, ioreg on macOS, and
WMI on Windows, none of which need privileges, so one source type covers
all three platforms with no build tags here.

Only SerialNumber is read, never WWN: cheap NVMe firmware reports a
near-zero WWN while the serial is fine. Serials that carry no entropy are
dropped -- ghw returns the literal 'unknown' when it cannot read one, and
using that would hand every such machine the same key. Removable devices
are skipped so a USB stick attached at seal time does not become a
decryption path that travels with the stick.

Motherboard serials are not used: product_uuid, product_serial, and
board_serial are 0400 root-only on modern kernels."
```

---

### Task 3: 层级 2/3 —— OS 机器 ID 与常量兜底

云主机、WSL2 与部分虚拟机的虚拟磁盘不报可用序列号。此时降级到 OS 机器 ID，再不行降级到编译期常量 —— 但绝不退回明文。这是本设计中唯一需要 build tag 的代码。

**Files:**
- Create: `pkg/secret/machineid_linux.go`
- Create: `pkg/secret/machineid_windows.go`
- Create: `pkg/secret/machineid_other.go`
- Create: `pkg/secret/machineid_linux_test.go`
- Modify: `pkg/secret/fingerprint.go`（填充层级 2）
- Test: `pkg/secret/fingerprint_test.go`（追加层级选择测试）

**Interfaces:**
- Consumes: Task 1 的 `source`、`Mode`、`usableID`；Task 2 的 `diskSources`、`listDisks`
- Produces:
  - `func machineID() string`（未导出，按 GOOS 分实现）
  - `var machineIDFiles = []string{"/etc/machine-id", "/var/lib/dbus/machine-id"}`（仅 linux 文件，测试可替换）
  - `func installSources() []source`（未导出）

- [ ] **Step 1: 写下失败的测试**

追加到 `pkg/secret/fingerprint_test.go`：

```go
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
```

创建 `pkg/secret/machineid_linux_test.go`：

```go
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
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./pkg/secret/ -run 'TestInstallTier|TestSealOnCloud|TestLinuxMachineID' -v 2>&1 | head -20
```

Expected: 编译失败，`undefined: machineIDFn`、`undefined: machineIDFiles`、`undefined: machineID`。

- [ ] **Step 3: 实现三个平台的 `machineID`**

创建 `pkg/secret/machineid_linux.go`：

```go
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
```

创建 `pkg/secret/machineid_windows.go`：

```go
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
```

创建 `pkg/secret/machineid_other.go`：

```go
//go:build !linux && !windows

package secret

// machineID has no portable equivalent on the remaining platforms. macOS
// hardware reports real disk serials, so the hardware tier covers it; a
// macOS VM without one falls through to the constant tier.
func machineID() string { return "" }
```

- [ ] **Step 4: 接入层级 2**

在 `pkg/secret/fingerprint.go` 中把 `defaultDiscoverAll` 替换为：

```go
func defaultDiscoverAll() [][]source {
	disks := diskSources()
	var install []source
	// The machine ID is a file or registry value and so travels with a
	// copied config. It is therefore only consulted when no real hardware
	// serial exists -- never alongside one.
	if len(disks) == 0 {
		install = installSources()
	}
	return [][]source{
		disks,
		install,
		{{mode: ModeObfuscate, value: obfuscationConstant}},
	}
}
```

并在文件末尾追加：

```go
// machineIDFn is the OS machine ID lookup. Replaced in tests.
var machineIDFn = machineID

// installSources returns the OS machine ID as binding material, used on
// cloud instances, WSL2, and VMs whose virtual disks report no usable
// serial. Weaker than a disk serial but far better than a bare constant:
// it keeps a sealed .env from opening on a different instance.
func installSources() []source {
	id := usableID(machineIDFn())
	if id == "" {
		return nil
	}
	return []source{{mode: ModeInstall, value: id}}
}
```

- [ ] **Step 5: 运行测试确认通过**

```bash
go test ./pkg/secret/ -v 2>&1 | tail -40
```

Expected: 全部 PASS。

- [ ] **Step 6: 确认三平台都能编译**

```bash
GOOS=linux   GOARCH=amd64 go build ./pkg/secret/ && echo linux ok
GOOS=darwin  GOARCH=arm64 go build ./pkg/secret/ && echo darwin ok
GOOS=windows GOARCH=amd64 go build ./pkg/secret/ && echo windows ok
```

Expected: 三行 `ok`。

- [ ] **Step 7: 提交**

```bash
gofmt -l pkg/secret/ && go vet ./pkg/secret/
git add pkg/secret/
git commit -m "feat(secret): fall back to the OS machine ID, never to plaintext

Cloud instances, WSL2, and some VMs expose virtual disks with no usable
serial. Rather than make those hosts store keys in the clear, sealing
drops to the OS machine ID and then to a compiled-in constant.

The machine ID is a file on Linux and a registry value on Windows, so it
travels with a copied config -- which is why it is consulted only when no
disk serial exists, never alongside one. In that position it still earns
its place: a sealed .env from a cloud box will not open on a different
box, which a bare constant cannot promise.

The constant tier provides no machine binding at all, and the mode byte
records that so the state stays visible rather than silently assumed."
```

---

### Task 4: `pkg/llm` 接线

让 provider 解析路径解密密文，顺带修掉缓存键持有明文与重复解析两个现存问题。

**Files:**
- Modify: `pkg/llm/registry.go`（`resolveConfig`、`resolveAPIKey`、`providerCacheKey`、`ProviderFor`、`buildProviderFromDef`、`InjectProvider`）
- Test: `pkg/llm/registry_seal_test.go`

**Interfaces:**
- Consumes: `secret.Seal`、`secret.Reveal`
- Produces:
  - `func resolveAPIKey(def ModelDef) (string, error)`（签名变更）
  - `func providerCacheKey(def ModelDef, apiKey string) string`（签名变更）
  - `func buildProviderFromDef(def ModelDef, apiKey string) (LLMProvider, error)`（签名变更）
  - `func keyDigest(apiKey string) string`（未导出）

- [ ] **Step 1: 写下失败的测试**

创建 `pkg/llm/registry_seal_test.go`：

```go
package llm

import (
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/secret"
)

func TestResolveAPIKeyRevealsSealedValue(t *testing.T) {
	sealed, err := secret.Seal("sk-ant-sealed-value")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	t.Setenv("SEAL_TEST_KEY", sealed)

	got, err := resolveAPIKey(ModelDef{Provider: "anthropic", APIKeyEnv: "SEAL_TEST_KEY"})
	if err != nil {
		t.Fatalf("resolveAPIKey: %v", err)
	}
	if got != "sk-ant-sealed-value" {
		t.Errorf("resolveAPIKey = %q, want the revealed key", got)
	}
}

func TestResolveAPIKeyPassesThroughPlaintext(t *testing.T) {
	t.Setenv("SEAL_TEST_KEY", "sk-plaintext-still-works")

	got, err := resolveAPIKey(ModelDef{Provider: "anthropic", APIKeyEnv: "SEAL_TEST_KEY"})
	if err != nil {
		t.Fatalf("resolveAPIKey: %v", err)
	}
	if got != "sk-plaintext-still-works" {
		t.Errorf("resolveAPIKey = %q, want the plaintext key unchanged", got)
	}
}

func TestResolveAPIKeyErrorsOnUndecryptableValue(t *testing.T) {
	// A value sealed on another machine, or a future format. Either way the
	// caller must see an error rather than an empty key that yields a
	// baffling 401 from the provider.
	t.Setenv("SEAL_TEST_KEY", "enc:v1:AAAAAAAAAAAAAAAAAAAAAAAA")

	got, err := resolveAPIKey(ModelDef{Provider: "anthropic", APIKeyEnv: "SEAL_TEST_KEY"})
	if err == nil {
		t.Fatalf("resolveAPIKey returned %q with no error", got)
	}
	if got != "" {
		t.Errorf("resolveAPIKey = %q, want empty on error", got)
	}
}

func TestProviderForErrorsRatherThanUsingEmptyKey(t *testing.T) {
	t.Setenv("SEAL_TEST_KEY", "enc:v1:AAAAAAAAAAAAAAAAAAAAAAAA")

	r, err := NewModelRegistry([]ModelDef{{
		Name: "broken", Provider: "anthropic", Model: "claude-sonnet-4-20250514",
		APIKeyEnv: "SEAL_TEST_KEY",
	}}, "broken")
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}

	if _, _, err := r.ProviderFor("broken"); err == nil {
		t.Fatal("ProviderFor succeeded with an undecryptable key")
	}
}

func TestProviderCacheKeyOmitsPlaintextKey(t *testing.T) {
	const plain = "sk-ant-super-secret-value"
	def := ModelDef{Provider: "anthropic", BaseURL: "https://example.test"}

	got := providerCacheKey(def, plain)
	if strings.Contains(got, plain) {
		t.Errorf("cache key %q contains the plaintext API key", got)
	}
	if got == providerCacheKey(def, "sk-a-different-key") {
		t.Error("cache key must still distinguish different keys")
	}
	if got != providerCacheKey(def, plain) {
		t.Error("cache key must be stable for the same inputs")
	}
}

func TestInjectProviderMatchesCacheKey(t *testing.T) {
	// InjectProvider builds the cache key by hand; it must agree with
	// providerCacheKey or test injection silently stops working.
	const plain = "sk-inject-test"
	r, err := NewModelRegistry([]ModelDef{{
		Name: "m", Provider: "anthropic", Model: "claude-sonnet-4-20250514",
		APIKeyEnv: "SEAL_INJECT_KEY",
	}}, "m")
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	t.Setenv("SEAL_INJECT_KEY", plain)

	sentinel := &UnavailableProvider{err: errSentinel}
	r.InjectProvider("anthropic", "", plain, sentinel)

	p, _, err := r.ProviderFor("m")
	if err != nil {
		t.Fatalf("ProviderFor: %v", err)
	}
	if p != sentinel {
		t.Error("ProviderFor did not return the injected provider")
	}
}

var errSentinel = errTest("sentinel")

type errTest string

func (e errTest) Error() string { return string(e) }
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./pkg/llm/ -run 'TestResolveAPIKey|TestProviderFor|TestProviderCacheKey|TestInjectProvider' -v 2>&1 | head -30
```

Expected: 编译失败 —— `resolveAPIKey` 返回值数量不符、`providerCacheKey` 参数数量不符。

- [ ] **Step 3: 改 `registry.go`**

import 块加入 `"crypto/sha256"`、`"encoding/hex"` 和 `"github.com/millken/deepai/pkg/secret"`。

`resolveConfig`（`registry.go:47`）中的 `apiKey` 解析替换为：

```go
	apiKey := overrides.APIKey
	if apiKey == "" && def.apiKeyVar != "" {
		apiKey = env.Get(def.apiKeyVar, "")
	}
	// A key may arrive sealed from .env or from an explicit ProviderConfig.
	// Reveal is a no-op on plaintext, so calling it here is safe even when
	// ProviderFor has already revealed the value.
	apiKey, err := secret.Reveal(apiKey)
	if err != nil {
		return resolvedConfig{}, fmt.Errorf("api key for provider %q: %w", name, err)
	}
```

`resolveAPIKey`（`registry.go:299`）整体替换：

```go
// resolveAPIKey determines the API key for a ModelDef: if APIKeyEnv is set,
// read from that env var; otherwise fall back to the provider's default env
// var. Sealed values are revealed here, at the point of use, so the process
// environment holds only ciphertext.
func resolveAPIKey(def ModelDef) (string, error) {
	if envVar := strings.TrimSpace(def.APIKeyEnv); envVar != "" {
		return secret.Reveal(env.Get(envVar, ""))
	}
	pd, ok := providerDefs[strings.ToLower(strings.TrimSpace(def.Provider))]
	if !ok || pd.apiKeyVar == "" {
		return "", nil
	}
	return secret.Reveal(env.Get(pd.apiKeyVar, ""))
}
```

`providerCacheKey`（`registry.go:275`）整体替换：

```go
// providerCacheKey builds a cache key that uniquely identifies a provider
// configuration so identical configs reuse the same LLMProvider instance.
// The key takes a digest of the API key rather than the key itself — the
// cache map has no need to hold a live credential.
func providerCacheKey(def ModelDef, apiKey string) string {
	return strings.ToLower(strings.TrimSpace(def.Provider)) + "|" +
		strings.TrimSpace(def.BaseURL) + "|" + keyDigest(apiKey)
}

// keyDigest hashes a resolved API key for use in cache keys.
func keyDigest(apiKey string) string {
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:8])
}
```

`ProviderFor`（`registry.go:236`）中从 `cacheKey := providerCacheKey(def)` 到 `p, err := buildProviderFromDef(def)` 之间替换为：

```go
	// Resolve once and thread it through: this used to be decrypted twice
	// per call, once for the cache key and once to build the provider.
	apiKey, err := resolveAPIKey(def)
	if err != nil {
		return nil, "", fmt.Errorf("api key for model %q: %w", def.Name, err)
	}

	cacheKey := providerCacheKey(def, apiKey)
	// Fast path: read lock.
	r.mu.RLock()
	if p, ok := r.providers[cacheKey]; ok {
		r.mu.RUnlock()
		return p, def.Model, nil
	}
	r.mu.RUnlock()

	// Slow path: create provider under write lock.
	r.mu.Lock()
	defer r.mu.Unlock()
	// Double-check after acquiring write lock.
	if p, ok := r.providers[cacheKey]; ok {
		return p, def.Model, nil
	}
	p, err := buildProviderFromDef(def, apiKey)
```

`buildProviderFromDef`（`registry.go:312`）整体替换：

```go
// buildProviderFromDef creates a new LLMProvider from a ModelDef using an
// already-resolved API key, with the same base-URL env fallback as
// resolveConfig.
func buildProviderFromDef(def ModelDef, apiKey string) (LLMProvider, error) {
	name := strings.ToLower(strings.TrimSpace(def.Provider))
	rc, err := resolveConfig(name, ProviderConfig{
		APIKey:  apiKey,
		BaseURL: def.BaseURL,
	})
	if err != nil {
		return nil, err
	}
	return buildProvider(name, rc)
}
```

`InjectProvider`（`registry.go:327`）中手工拼装的 key 替换为：

```go
	// Must match providerCacheKey exactly, or injection silently stops
	// resolving.
	key := strings.ToLower(strings.TrimSpace(provider)) + "|" +
		strings.TrimSpace(baseURL) + "|" + keyDigest(apiKey)
```

- [ ] **Step 4: 运行测试确认通过**

```bash
go test ./pkg/llm/ -v 2>&1 | tail -40
```

Expected: 新测试与既有测试全部 PASS。

- [ ] **Step 5: 确认既有依赖方仍然通过**

```bash
go build ./... && go test ./pkg/chat/ ./pkg/commands/ 2>&1 | tail -20
```

Expected: 构建成功，`pkg/chat`（用到 `InjectProvider`）与 `pkg/commands` 测试 PASS。

- [ ] **Step 6: 提交**

```bash
gofmt -l pkg/llm/ && go vet ./pkg/llm/
git add pkg/llm/
git commit -m "feat(llm): reveal sealed API keys at the point of use

Keys are decrypted where the provider is built rather than where .env is
loaded, so the process environment carries ciphertext and printenv or
/proc/<pid>/environ give nothing away. Plaintext keys pass through
untouched, so existing installs keep working.

An undecryptable value now fails provider initialization instead of
resolving to an empty key -- that path produced a 401 from the provider
whose real cause was a changed disk.

Two problems on the same path: the cache key held the plaintext key, and
resolveAPIKey ran twice per ProviderFor call, once for the cache key and
once to build the provider. The key is now resolved once and threaded
through, and the cache holds a digest. InjectProvider builds the same
key by hand and moves with it."
```

---

### Task 5: `setup provider` 写入密文

让交互式配置写密文而非明文，并停止把已存密钥回填进表单。同时把 `.env` 的写入改为原子写。

**Files:**
- Modify: `pkg/commands/setup.go:306-328`（录入与保存）、`pkg/commands/setup.go:489-511`（`saveEnvValue`）
- Test: `pkg/commands/setup_seal_test.go`

**Interfaces:**
- Consumes: `secret.Seal`、`secret.IsSealed`、`secret.Fingerprint`、`secret.ModeHardware`
- Produces:
  - `func writeEnvAtomic(path string, content []byte) error`（未导出，Task 6 复用）
  - `func sealWarning() string`（未导出，降级时的提示文案，Task 6 复用）

- [ ] **Step 1: 写下失败的测试**

创建 `pkg/commands/setup_seal_test.go`：

```go
package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/secret"
)

func TestSaveEnvValueWritesAtomicallyWith0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")

	if err := saveEnvValue(path, "ANTHROPIC_API_KEY", "enc:v1:AAAA"); err != nil {
		t.Fatalf("saveEnvValue: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Errorf("mode = %o, want 600", perm)
	}

	// No temp file may survive: a leftover would be a second copy of the
	// credentials with no cleanup owner.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only .env", names)
	}
}

func TestSaveEnvValueReplacesExistingKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("ANTHROPIC_API_KEY=old\nOTHER=keep\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := saveEnvValue(path, "ANTHROPIC_API_KEY", "new"); err != nil {
		t.Fatalf("saveEnvValue: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if strings.Contains(got, "ANTHROPIC_API_KEY=old") {
		t.Error("old value survived")
	}
	if !strings.Contains(got, "ANTHROPIC_API_KEY=new") {
		t.Errorf("new value missing: %q", got)
	}
	if !strings.Contains(got, "OTHER=keep") {
		t.Errorf("unrelated entry lost: %q", got)
	}
}

func TestSealedValueSurvivesEnvFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")

	sealed, err := secret.Seal("sk-ant-roundtrip-test")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := saveEnvValue(path, "ANTHROPIC_API_KEY", sealed); err != nil {
		t.Fatalf("saveEnvValue: %v", err)
	}

	// The base64url alphabet has no shell metacharacters, so a sealed value
	// must survive .env parsing byte for byte.
	if got := loadEnvValue(path, "ANTHROPIC_API_KEY"); got != sealed {
		t.Errorf("loadEnvValue = %q, want %q", got, sealed)
	}
	plain, err := secret.Reveal(loadEnvValue(path, "ANTHROPIC_API_KEY"))
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	if plain != "sk-ant-roundtrip-test" {
		t.Errorf("Reveal = %q", plain)
	}
}

func TestSealWarningEmptyWhenHardwareBound(t *testing.T) {
	// The dev machine has fixed disks with serials, so no warning is due.
	// On a host without them the warning must be non-empty and must name
	// the mode so the weaker state is never silently assumed.
	got := sealWarning()
	if secret.Fingerprint().Mode == secret.ModeHardware {
		if got != "" {
			t.Errorf("sealWarning = %q, want empty when hardware-bound", got)
		}
		return
	}
	if got == "" {
		t.Error("sealWarning is empty on a host without hardware binding")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./pkg/commands/ -run 'TestSaveEnvValue|TestSealedValue|TestSealWarning' -v 2>&1 | head -20
```

Expected: 编译失败，`undefined: sealWarning`；`TestSaveEnvValueWritesAtomicallyWith0600` 关于目录只含一个文件的断言可能已通过（现有实现用 `os.WriteFile`），但权限与原子性尚未成立。

- [ ] **Step 3: 实现原子写与降级提示**

`pkg/commands/setup.go` 的 import 块加入 `"github.com/millken/deepai/pkg/secret"`。

`saveEnvValue`（`setup.go:489`）末尾两行替换：

```go
	content := strings.Join(lines, "\n")
	return writeEnvAtomic(path, []byte(content))
}

// writeEnvAtomic replaces path's contents without ever exposing a partial
// or world-readable file. os.CreateTemp creates at 0600, so the mode is
// right from creation rather than being widened and then narrowed. The temp
// file is made in the same directory because rename is only atomic within
// one filesystem.
func writeEnvAtomic(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".env-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmp := f.Name()
	// A no-op once the rename below succeeds; on any earlier failure it
	// keeps a copy of the credentials from being left behind.
	defer os.Remove(tmp)

	if _, err := f.Write(content); err != nil {
		f.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	return os.Rename(tmp, path)
}

// sealWarning returns a message when this host cannot bind sealed keys to
// hardware, and "" when it can. Degrading is a silent loss of protection,
// so it must be stated rather than assumed.
func sealWarning() string {
	info := secret.Fingerprint()
	switch info.Mode {
	case secret.ModeHardware:
		return ""
	case secret.ModeInstall:
		return "  Note: no disk serial number is readable here, so the key is bound to\n" +
			"  this OS install rather than to hardware. Reinstalling the OS will\n" +
			"  require re-entering it."
	default:
		return "  Warning: no disk serial number and no OS machine ID are readable here\n" +
			"  (common on cloud instances and WSL2), so the key is obfuscated but NOT\n" +
			"  bound to this machine: anyone with the file and a deepai binary can\n" +
			"  read it. It is still safe from tools that merely read the file."
	}
}
```

- [ ] **Step 4: 改录入与保存路径**

`setup.go:306-328` 从 `info := providerInfo[provider]` 到 `fmt.Printf("  Saved API key to %s\n", envPath)` 所在的整个块替换为：

```go
	info := providerInfo[provider]
	// The stored key is sealed on disk. Prefilling the form would mean
	// decrypting it back into memory for no benefit, so an empty answer
	// means "keep what is there".
	title := fmt.Sprintf("API key (%s)", info.envVar)
	if loadEnvValue(envPath, info.envVar) != "" {
		title += " — leave blank to keep the current key"
	}

	var apiKey string
	if err := huh.NewInput().
		Title(title).
		Value(&apiKey).
		EchoMode(huh.EchoModePassword).
		Run(); err != nil {
		return err
	}

	if apiKey != "" {
		sealed, err := secret.Seal(apiKey)
		if err != nil {
			return fmt.Errorf("seal API key: %w", err)
		}
		if err := saveEnvValue(envPath, info.envVar, sealed); err != nil {
			return fmt.Errorf("save .env: %w", err)
		}
		fmt.Printf("  Saved sealed API key to %s\n", envPath)
		if w := sealWarning(); w != "" {
			fmt.Println(w)
		}
	}
```

同时删掉紧接在 `apiKey := loadEnvValue(...)` 之后的这段现已无意义的逻辑（provider 变更时清空默认值 —— 现在从不预填）：

```go
	// If provider changed and new provider has no key, clear default.
	if provider != oldProvider && apiKey == "" {
		apiKey = ""
	}
```

若 `oldProvider` 因此不再被使用，编译器会报 `declared and not used`；此时把 `oldProvider := cfg.Provider` 一并删除。

- [ ] **Step 5: 运行测试确认通过**

```bash
go test ./pkg/commands/ -v 2>&1 | tail -30
go build ./...
```

Expected: 全部 PASS，构建成功。

- [ ] **Step 6: 提交**

```bash
gofmt -l pkg/commands/ && go vet ./pkg/commands/
git add pkg/commands/
git commit -m "feat(setup): seal the API key as setup writes it

The wizard also stops prefilling the stored key into the form: it is
sealed on disk, and decrypting it back into the form would put plaintext
in memory to no purpose. An empty answer now keeps the current key, which
also retires the provider-changed branch that existed to clear the
prefill.

.env writes go through an atomic replace: os.CreateTemp gives 0600 from
creation rather than widening then narrowing, the temp file shares the
directory so rename stays atomic, and a failed write removes it instead
of leaving a second copy of the credentials behind.

Degrading below hardware binding is a silent loss of protection, so
sealing that falls back to the OS machine ID or to the constant says so
at the point it happens."
```

---

### Task 6: `deepai key` 命令

给用户加密、检查、迁移密钥的入口。刻意不含导出明文的子命令。

**Files:**
- Create: `pkg/commands/key.go`
- Modify: `pkg/commands/commands.go`（注册 `addKey`）
- Test: `pkg/commands/key_test.go`

**Interfaces:**
- Consumes: Task 5 的 `writeEnvAtomic`、`sealWarning`；`providerInfo`、`loadEnvValue`、`saveEnvValue`、`EnvFile`；`secret.Seal`/`Reveal`/`IsSealed`/`Inspect`/`Fingerprint`
- Produces:
  - `func addKey(topLevel *cobra.Command)`（未导出）
  - `type envEntry struct { Key string; Value string; Line string }`（未导出）
  - `func parseEnvFile(content string) []envEntry`（未导出）
  - `func apiKeyVarNames() map[string]bool`（未导出）
  - `func sealEnvFile(path string) (int, error)`（未导出，返回封装条数）

- [ ] **Step 1: 写下失败的测试**

创建 `pkg/commands/key_test.go`：

```go
package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/secret"
)

func TestApiKeyVarNamesCoversProviders(t *testing.T) {
	names := apiKeyVarNames()
	for _, want := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "DEEPSEEK_API_KEY"} {
		if !names[want] {
			t.Errorf("%s missing from the sealable set", want)
		}
	}
	if names["ANTHROPIC_BASE_URL"] {
		t.Error("base URLs must not be sealed")
	}
}

func TestParseEnvFileKeepsCommentsAndOrder(t *testing.T) {
	content := "# a comment\nANTHROPIC_API_KEY=sk-one\n\nOPENAI_API_KEY=sk-two\n"
	got := parseEnvFile(content)

	if len(got) != 4 {
		t.Fatalf("len = %d, want 4: %+v", len(got), got)
	}
	if got[0].Key != "" || got[0].Line != "# a comment" {
		t.Errorf("comment line = %+v", got[0])
	}
	if got[1].Key != "ANTHROPIC_API_KEY" || got[1].Value != "sk-one" {
		t.Errorf("first entry = %+v", got[1])
	}
	if got[3].Key != "OPENAI_API_KEY" || got[3].Value != "sk-two" {
		t.Errorf("last entry = %+v", got[3])
	}
}

func TestSealEnvFileSealsPlaintextKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "# keep me\nANTHROPIC_API_KEY=sk-ant-plain\nANTHROPIC_BASE_URL=https://example.test\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	n, err := sealEnvFile(path)
	if err != nil {
		t.Fatalf("sealEnvFile: %v", err)
	}
	if n != 1 {
		t.Errorf("sealed %d entries, want 1", n)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if strings.Contains(got, "sk-ant-plain") {
		t.Error("plaintext key survived in .env")
	}
	if !strings.Contains(got, "# keep me") {
		t.Error("comment lost")
	}
	if !strings.Contains(got, "ANTHROPIC_BASE_URL=https://example.test") {
		t.Error("non-key entry was altered")
	}
	if plain, err := secret.Reveal(loadEnvValue(path, "ANTHROPIC_API_KEY")); err != nil || plain != "sk-ant-plain" {
		t.Errorf("Reveal = %q, %v; want the original key", plain, err)
	}
}

func TestSealEnvFileLeavesNoPlaintextResidue(t *testing.T) {
	// Backing up to .env.bak would leave a plaintext copy behind -- and
	// .env.bak is not in .gitignore, making it more dangerous than the
	// original. Nothing but .env may remain.
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("ANTHROPIC_API_KEY=sk-ant-plain\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := sealEnvFile(path); err != nil {
		t.Fatalf("sealEnvFile: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != ".env" {
			t.Errorf("unexpected file %q left behind", e.Name())
			continue
		}
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "sk-ant-plain") {
			t.Errorf("%s contains the plaintext key", e.Name())
		}
	}
}

func TestSealEnvFileSkipsAlreadySealed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	sealed, err := secret.Seal("sk-ant-already")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := os.WriteFile(path, []byte("ANTHROPIC_API_KEY="+sealed+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	n, err := sealEnvFile(path)
	if err != nil {
		t.Fatalf("sealEnvFile: %v", err)
	}
	if n != 0 {
		t.Errorf("sealed %d entries, want 0", n)
	}

	if got := loadEnvValue(path, "ANTHROPIC_API_KEY"); got != sealed {
		t.Error("an already-sealed value was re-sealed")
	}
}

func TestSealEnvFileLeavesFileUntouchedOnVerifyFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	const original = "ANTHROPIC_API_KEY=sk-ant-plain\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}

	// Force the roundtrip check to fail, standing in for a broken
	// fingerprint layer. The file must not be rewritten.
	prev := sealFn
	sealFn = func(string) (string, error) { return "enc:v1:not-a-real-blob", nil }
	t.Cleanup(func() { sealFn = prev })

	if _, err := sealEnvFile(path); err == nil {
		t.Fatal("sealEnvFile succeeded despite a failed roundtrip check")
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != original {
		t.Errorf("file was modified: %q, want %q", string(b), original)
	}
}

func TestSealEnvFileMissingFile(t *testing.T) {
	n, err := sealEnvFile(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Errorf("sealEnvFile on a missing file returned %v, want nil", err)
	}
	if n != 0 {
		t.Errorf("sealed %d entries, want 0", n)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

```bash
go test ./pkg/commands/ -run 'TestApiKeyVarNames|TestParseEnvFile|TestSealEnvFile' -v 2>&1 | head -20
```

Expected: 编译失败，`undefined: apiKeyVarNames`、`undefined: parseEnvFile`、`undefined: sealEnvFile`、`undefined: sealFn`。

- [ ] **Step 3: 实现 `key.go`**

创建 `pkg/commands/key.go`：

```go
package commands

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"charm.land/huh/v2"
	"github.com/millken/deepai/pkg/secret"
	"github.com/spf13/cobra"
)

// sealFn is secret.Seal, indirected so tests can force a roundtrip failure.
var sealFn = secret.Seal

func addKey(topLevel *cobra.Command) {
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Manage sealed API keys",
		Long: `Store API keys in ~/.deepai/.env as ciphertext bound to this machine's
disk serial numbers, so a tool that reads the file gets nothing usable.

This is not protection against a local attacker: deepai runs as you, so a
deliberate extraction still succeeds. It stops accidental exposure -- an
agent reading the file, a config pasted into a chat, a .env committed to git.

There is deliberately no command to print a stored key.`,
	}
	cmd.AddCommand(newKeySetCmd(), newKeyListCmd(), newKeySealCmd(), newKeyCheckCmd())
	topLevel.AddCommand(cmd)
}

func newKeySetCmd() *cobra.Command {
	var envVar string
	c := &cobra.Command{
		Use:   "set [provider]",
		Short: "Enter an API key and store it sealed",
		Long: `Prompt for an API key and write it to ~/.deepai/.env in sealed form.

Give a provider name to use its standard variable, or --env-var to name the
variable directly (matching a models[].api_key_env entry in config.yaml).`,
		Example: "  deepai key set anthropic\n  deepai key set --env-var MY_CUSTOM_KEY",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(envVar)
			if name == "" {
				if len(args) == 0 {
					return fmt.Errorf("give a provider name or --env-var; known providers: %s",
						strings.Join(providerNames(), ", "))
				}
				info, ok := providerInfo[strings.ToLower(args[0])]
				if !ok {
					return fmt.Errorf("unknown provider %q; known providers: %s",
						args[0], strings.Join(providerNames(), ", "))
				}
				name = info.envVar
			}

			var apiKey string
			if err := huh.NewInput().
				Title(fmt.Sprintf("API key (%s)", name)).
				Value(&apiKey).
				EchoMode(huh.EchoModePassword).
				Run(); err != nil {
				return err
			}
			if strings.TrimSpace(apiKey) == "" {
				return fmt.Errorf("no key entered")
			}

			sealed, err := sealFn(apiKey)
			if err != nil {
				return fmt.Errorf("seal API key: %w", err)
			}
			// Verify before writing: a sealed value that cannot be revealed
			// on this very host would silently lock the key away.
			if back, err := secret.Reveal(sealed); err != nil || back != apiKey {
				return fmt.Errorf("sealed key failed its own roundtrip check; nothing was written")
			}
			if err := saveEnvValue(EnvFile(), name, sealed); err != nil {
				return fmt.Errorf("save .env: %w", err)
			}

			fmt.Printf("  Sealed %s into %s\n", name, EnvFile())
			if w := sealWarning(); w != "" {
				fmt.Println(w)
			}
			return nil
		},
	}
	c.Flags().StringVar(&envVar, "env-var", "", "environment variable to store the key under")
	return c
}

func newKeyListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "Show which API keys are stored and whether they are sealed",
		Example: "  deepai key list",
		RunE: func(cmd *cobra.Command, args []string) error {
			path := EnvFile()
			names := make([]string, 0, len(providerInfo))
			for n := range apiKeyVarNames() {
				names = append(names, n)
			}
			sort.Strings(names)

			fmt.Printf("  %s\n\n", path)
			for _, n := range names {
				v := loadEnvValue(path, n)
				switch {
				case v == "":
					fmt.Printf("  %-22s absent\n", n)
				case secret.IsSealed(v):
					h, err := secret.Inspect(v)
					if err != nil {
						fmt.Printf("  %-22s sealed     unreadable: %v\n", n, err)
						continue
					}
					status := "ok"
					if _, err := secret.Reveal(v); err != nil {
						status = "CANNOT DECRYPT"
					}
					fmt.Printf("  %-22s sealed     %s, %d wrap(s), %s\n", n, h.Mode, h.Wraps, status)
				default:
					fmt.Printf("  %-22s plaintext  run `deepai key seal`\n", n)
				}
			}
			return nil
		},
	}
}

func newKeySealCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "seal",
		Short:   "Encrypt any plaintext API keys already in .env",
		Long: `Rewrite ~/.deepai/.env with every plaintext API key sealed in place.

Each value is verified by sealing and revealing it before anything is
written, and the file is replaced atomically, so no plaintext copy is ever
left on disk.`,
		Example: "  deepai key seal",
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := sealEnvFile(EnvFile())
			if err != nil {
				return err
			}
			if n == 0 {
				fmt.Println("  No plaintext API keys found; nothing to do.")
				return nil
			}
			fmt.Printf("  Sealed %d key(s) in %s\n", n, EnvFile())
			if w := sealWarning(); w != "" {
				fmt.Println(w)
			}
			return nil
		},
	}
}

func newKeyCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "check",
		Short:   "Show this machine's binding sources and each key's status",
		Example: "  deepai key check",
		RunE: func(cmd *cobra.Command, args []string) error {
			info := secret.Fingerprint()
			fmt.Printf("  Binding: %v\n", info.Mode)
			for _, s := range info.Sources {
				note := "unused (a stronger tier is available)"
				if s.Used {
					note = "in use"
				}
				fmt.Printf("    %-12s %s  %s\n", s.Tier, s.Digest, note)
			}
			if w := sealWarning(); w != "" {
				fmt.Println(w)
			}
			fmt.Println()
			return newKeyListCmd().RunE(cmd, args)
		},
	}
}

// apiKeyVarNames returns the environment variables that hold API keys and
// may therefore be sealed. Base URLs and other settings are excluded --
// sealing them would break config that is not secret.
func apiKeyVarNames() map[string]bool {
	out := make(map[string]bool, len(providerInfo))
	for _, info := range providerInfo {
		out[info.envVar] = true
	}
	return out
}

// envEntry is one line of a .env file. Key is empty for blank lines and
// comments, whose Line is preserved verbatim.
type envEntry struct {
	Key   string
	Value string
	Line  string
}

func parseEnvFile(content string) []envEntry {
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	out := make([]envEntry, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			out = append(out, envEntry{Line: line})
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			out = append(out, envEntry{Line: line})
			continue
		}
		out = append(out, envEntry{Key: strings.TrimSpace(k), Value: v, Line: line})
	}
	return out
}

// sealEnvFile seals every plaintext API key in path and returns how many it
// sealed. Every value is sealed and revealed before anything is written, so
// a broken fingerprint layer leaves the file untouched rather than
// destroying the keys. The rewrite is atomic and leaves no plaintext copy --
// notably no .env.bak, which would be a plaintext duplicate that is not
// even covered by .gitignore.
func sealEnvFile(path string) (int, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read %s: %w", path, err)
	}

	sealable := apiKeyVarNames()
	entries := parseEnvFile(string(content))
	sealed := 0
	for i, e := range entries {
		if e.Key == "" || !sealable[e.Key] {
			continue
		}
		v := strings.TrimSpace(e.Value)
		if v == "" || secret.IsSealed(v) {
			continue
		}
		out, err := sealFn(v)
		if err != nil {
			return 0, fmt.Errorf("seal %s: %w", e.Key, err)
		}
		back, err := secret.Reveal(out)
		if err != nil {
			return 0, fmt.Errorf("seal %s: sealed value failed its roundtrip check (%w); %s was not modified", e.Key, err, path)
		}
		if back != v {
			return 0, fmt.Errorf("seal %s: sealed value did not survive its roundtrip check; %s was not modified", e.Key, path)
		}
		entries[i].Line = e.Key + "=" + out
		sealed++
	}
	if sealed == 0 {
		return 0, nil
	}

	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, e.Line)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := writeEnvAtomic(path, []byte(body)); err != nil {
		return 0, fmt.Errorf("write %s: %w", path, err)
	}
	return sealed, nil
}
```

- [ ] **Step 4: 注册命令**

`pkg/commands/commands.go` 替换为：

```go
package commands

import "github.com/spf13/cobra"

func AddCommands(topLevel *cobra.Command) {
	addChat(topLevel)
	addSetup(topLevel)
	addSession(topLevel)
	addVersion(topLevel)
	addPlugin(topLevel)
	addKey(topLevel)
}
```

- [ ] **Step 5: 运行测试确认通过**

```bash
go test ./pkg/commands/ -v 2>&1 | tail -40
```

Expected: 全部 PASS。

- [ ] **Step 6: 手工验证命令**

```bash
go build -o ./bin/deepai ./cmd/deepai && ./bin/deepai key check
```

Expected：`Binding: hardware-bound`，两条 `disk-serial ... in use`，一条 `constant ... unused`，随后列出各 provider 的密钥状态。

```bash
./bin/deepai key --help && ./bin/deepai key list
```

Expected：四个子命令 `set`/`list`/`seal`/`check`，**没有** `show` 或 `export`。

在临时 home 下验证 `seal` 的端到端行为，不触碰真实配置：

```bash
export DEEPAI_TEST_HOME=$(mktemp -d)
mkdir -p "$DEEPAI_TEST_HOME/.deepai"
printf 'ANTHROPIC_API_KEY=sk-ant-fake-plaintext\n' > "$DEEPAI_TEST_HOME/.deepai/.env"
HOME="$DEEPAI_TEST_HOME" ./bin/deepai key seal
echo "--- .env after sealing ---"
cat "$DEEPAI_TEST_HOME/.deepai/.env"
echo "--- files present (only .env expected) ---"
ls -a "$DEEPAI_TEST_HOME/.deepai/"
echo "--- plaintext must not appear anywhere ---"
grep -r "sk-ant-fake-plaintext" "$DEEPAI_TEST_HOME" && echo "LEAK" || echo "no plaintext residue"
rm -rf "$DEEPAI_TEST_HOME"
```

Expected：`.env` 中是 `ANTHROPIC_API_KEY=enc:v1:...`；目录里只有 `.env`（无 `.env.bak`、无临时文件）；最后一行输出 `no plaintext residue`。

- [ ] **Step 7: 全量测试与提交**

```bash
gofmt -l pkg/ && go vet ./... && go test ./... 2>&1 | grep -v "^ok" | head -20
```

Expected: 无 gofmt 输出、无 vet 报错、无测试失败。

```bash
git add pkg/commands/
git commit -m "feat(commands): add deepai key for sealing and inspecting keys

set, list, seal, and check. There is deliberately no command that prints
a stored key: 'deepai key show' would be an easier route to the plaintext
than reading .env, which is the very thing this feature closes off.

seal verifies each value by sealing and revealing it before anything is
written and replaces the file atomically, so a broken fingerprint layer
leaves the keys intact instead of destroying them. It writes no .env.bak
-- a plaintext duplicate that .gitignore does not even cover would be
worse than the original.

check exists mainly for the first run of a macOS or Windows build: ghw's
Darwin serial support carries a caveat in its own docs and this project's
development environment is Linux-only, so it prints which tier and how
many sources were actually found."
```

---

### Task 7: 文档

把功能与其边界写进 README，特别是"不防什么"—— 一个被误信为强保护的机制比没有保护更危险。

**Files:**
- Modify: `README.md`

**Interfaces:**
- Consumes: Task 6 的 CLI 表面
- Produces: 无代码

- [ ] **Step 1: 找到插入位置**

```bash
grep -n "^## " README.md | head -30
```

- [ ] **Step 2: 新增一节**

在讲配置/环境变量的章节之后插入：

```markdown
## API Key 密封

API key 默认以明文存在 `~/.deepai/.env`。任何 CLI agent —— 包括 deepai 自己 —— 都能用 `read_file` 或 `grep` 读到它，然后把它送进远端模型的上下文与日志。密封把文件里的值换成绑定本机磁盘序列号的密文，让读取这个文件的收益归零。

```bash
deepai key seal            # 把 .env 里现存的明文密钥就地加密
deepai key set anthropic   # 录入新密钥并直接以密文存储
deepai key list            # 查看每个密钥是密文、明文还是缺失
deepai key check           # 查看本机的绑定来源与各密钥能否解密
```

密封后 `.env` 长这样：

```
ANTHROPIC_API_KEY=enc:v1:RFBLMQEBAWQ4ZDI3M2EwNmY0NGM5YjljOWM1YmM5...
```

解密发生在真正发出请求之前，所以进程环境变量里存的也是密文 —— `printenv` 和 `/proc/<pid>/environ` 同样看不到明文。

**绑定与容错。** 密钥绑定到本机固定磁盘的序列号，每块盘各封装一份，因此换掉多块盘中的一块不会锁死。密文拷到另一台机器则无法解密。云主机、WSL2 与部分虚拟机不报可用的磁盘序列号，此时自动降级绑定到 OS 安装 ID，再不行降级为纯混淆 —— 但绝不退回明文。`deepai key check` 会明确显示当前处于哪一级。

**这不是抗本地攻击者的密码学保护。** deepai 以你自己的身份运行，因此凡是它能无交互拿到的密钥材料，同用户的其他进程原理上也能拿到。它提供的是成本壁垒：意外泄露被完全阻断，而拿到明文需要刻意的多步操作。刻意没有提供打印已存密钥的命令。

**向后兼容。** 明文密钥永久可用，现有安装无需任何改动。CI 与容器环境可以继续用明文环境变量。
```

- [ ] **Step 3: 核对示例命令真的存在**

```bash
go build -o ./bin/deepai ./cmd/deepai
for sub in seal set list check; do ./bin/deepai key $sub --help >/dev/null 2>&1 && echo "key $sub ok" || echo "key $sub MISSING"; done
```

Expected: 四行 `ok`（`key set --help` 不会进入交互，只打帮助）。

- [ ] **Step 4: 提交**

```bash
git add README.md
git commit -m "docs: document API key sealing and what it does not protect

States the limit as plainly as the feature: deepai runs as the user, so a
deliberate local extraction still succeeds. A mechanism mistaken for
strong protection is worse than none, and the fallback tiers are a silent
downgrade unless the docs and key check both name them."
```

---

## Self-Review

**1. Spec coverage**

| Spec 章节 | 实现任务 |
| --- | --- |
| 威胁模型与非目标 | Task 1（包注释）、Task 7（README） |
| 线格式（魔数/版本/mode/wrap/数据） | Task 1 Step 4、Step 6 长度断言 |
| blob 内不得含源值痕迹 | Task 1 `TestSealNeverLeaksSourceValue` |
| 解密全试 | Task 1 `Reveal` 双层循环 + `TestRevealSucceedsWithOneSurvivingSource` |
| 加解密非对称（密封只取最高层级） | Task 1 `sealSources` + `TestSealUsesHighestTierOnly` |
| KEK 派生（HKDF + userID + tier） | Task 1 `deriveKEK` |
| tier/mode 对应表 | Task 1 `Mode.tier()`、`Mode.String()` |
| 层级 1 磁盘序列号 + 退化值过滤 + 不用 WWN + 排除 removable | Task 2 |
| 层级 2 OS 机器 ID（Linux 文件 / Windows 注册表） | Task 3 |
| 层级 3 常量 | Task 1 `obfuscationConstant`、Task 3 `TestSealOnCloudHostStillProducesCiphertext` |
| 模式可见（警告 + list/check 显示） | Task 5 `sealWarning`、Task 6 `key list`/`key check` |
| 失败必须响亮 + 未知版本报错 | Task 1 `parseBlob`、Task 4 `TestProviderForErrorsRatherThanUsingEmptyKey` |
| 集成点 1/2/3 | Task 4（前两个）、Task 5（第三个） |
| `providerCacheKey` 去明文 | Task 4 |
| `resolveAPIKey` 单次解析 | Task 4 |
| CLI 四命令 + 无 `key show` | Task 6 |
| `key seal` 不留明文备份 + 原子写 | Task 5 `writeEnvAtomic`、Task 6 `sealEnvFile` + `TestSealEnvFileLeavesNoPlaintextResidue` |
| 依赖变更（ghw） | Task 2 Step 1 |
| 迁移与向后兼容 | Task 1 `TestRevealPassesThroughPlaintext`、Task 4 `TestResolveAPIKeyPassesThroughPlaintext`、Task 7 |

无缺口。

**2. Placeholder scan**

所有步骤均含可直接运行的完整代码与预期输出，无 TBD/TODO/"类似 Task N"、无"添加适当的错误处理"这类空话。每个 `- [ ]` 步骤都是单一动作。

**3. Type consistency**

- `discoverAll` 在 Task 1 声明为 `var discoverAll = defaultDiscoverAll`，Task 2、Task 3 只改 `defaultDiscoverAll` 的函数体，测试通过 `withSources` 替换 `discoverAll` —— 一致。
- `usableID` 在 Task 1 定义，Task 2（磁盘序列号）与 Task 3（机器 ID）复用，命名不含 "serial"，与两种用途都相符。
- `machineIDFn` 在 Task 3 Step 4 声明，`withMachineID` 在 Task 3 Step 1 引用它 —— 测试先写、实现后补，符合 TDD，但同一任务内闭合。
- `machineIDFiles` 只存在于 `machineid_linux.go`，其测试文件带 `//go:build linux` —— 一致。
- `resolveAPIKey`、`providerCacheKey`、`buildProviderFromDef` 三处签名变更在 Task 4 内一次性完成，`InjectProvider` 的手工拼键同步更新 —— 一致。
- `writeEnvAtomic`、`sealWarning` 在 Task 5 定义，Task 6 消费 —— 顺序正确。
- `sealFn` 在 Task 6 声明并被 Task 6 的两处（`key set`、`sealEnvFile`）与测试使用 —— 一致。Task 5 的 `setup.go` 直接调 `secret.Seal` 而非 `sealFn`，两者不冲突。
