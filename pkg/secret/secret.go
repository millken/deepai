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
