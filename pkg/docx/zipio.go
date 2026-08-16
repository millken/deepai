// Package docx provides byte-faithful reading and editing of .docx (OOXML)
// files. The design principle is Ground Truth + narrow patch: the original
// file is the single source of truth, and edits replace only the byte ranges
// that actually changed. Nothing is ever round-tripped through a DOM.
package docx

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// DocumentPart is the zip entry holding the main document body.
const DocumentPart = "word/document.xml"

// contentTypesPart must be present in every valid OOXML package.
const contentTypesPart = "[Content_Types].xml"

const (
	// maxDocxBytes caps the on-disk (compressed) size.
	maxDocxBytes = 25 << 20
	// maxDecompressedBytes caps the total decompressed size, guarding
	// against zip bombs that are small on disk but huge in memory.
	maxDecompressedBytes = 200 << 20
	// maxZipEntries caps the entry count, guarding against packages with
	// an absurd number of tiny entries.
	maxZipEntries = 2000
)

// cfbMagic identifies an OLE2/Compound File Binary container. Password
// encrypted OOXML files are CFB, not zip: they hold EncryptionInfo and
// EncryptedPackage streams. Detecting this up front lets us report
// "encrypted" instead of leaking a confusing zip or XML parse error.
var cfbMagic = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}

// Package is an opened .docx: every entry's decompressed content plus the
// original entry order. Modifications are staged with SetPart and only hit
// disk in WriteTo.
type Package struct {
	// names preserves the original entry order; WriteTo replays it.
	names []string
	// parts maps entry name to decompressed content.
	parts map[string][]byte
	// raw holds the original compressed bytes for each entry, so untouched
	// entries can be copied verbatim instead of recompressed.
	raw map[string][]byte
	// headers holds each entry's original zip metadata.
	headers map[string]*zip.FileHeader
	// modified marks entries replaced via SetPart.
	modified map[string]bool
}

// Open reads a .docx, validating it against the zip-layer guards before any
// content is parsed. The whole package is held in memory; callers should
// treat Open as the size-bounded entry point.
func Open(docxPath string) (*Package, error) {
	// O_NONBLOCK makes the open itself non-blocking: opening a FIFO with no
	// writer on the other end would otherwise park open(2) forever, before
	// the IsRegular check below ever gets a chance to run. It is a no-op for
	// regular files, which is the only case that reaches the read below.
	f, err := os.OpenFile(docxPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open docx: %w", err)
	}
	defer f.Close()

	// Stat the held fd, not the path: stat-then-read on a path is a TOCTOU
	// race, and it lets a FIFO or character device (whose path-based Size()
	// reports 0) slip past the size guard and read unbounded.
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat docx: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("docx path %q is not a regular file", docxPath)
	}

	// Read one byte past the budget so an over-limit file is detectable
	// without ever buffering more than maxDocxBytes+1 bytes.
	data, err := io.ReadAll(io.LimitReader(f, maxDocxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read docx: %w", err)
	}
	if len(data) > maxDocxBytes {
		return nil, fmt.Errorf("docx exceeds %d MB limit", maxDocxBytes>>20)
	}
	if bytes.HasPrefix(data, cfbMagic) {
		return nil, errors.New("encrypted or password-protected .docx is not supported")
	}

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("not a valid .docx (cannot read as zip): %w", err)
	}
	if len(zr.File) > maxZipEntries {
		return nil, fmt.Errorf("docx has %d entries, limit is %d", len(zr.File), maxZipEntries)
	}

	pkg := &Package{
		names:    make([]string, 0, len(zr.File)),
		parts:    make(map[string][]byte, len(zr.File)),
		raw:      make(map[string][]byte, len(zr.File)),
		headers:  make(map[string]*zip.FileHeader, len(zr.File)),
		modified: make(map[string]bool),
	}

	var total int64
	// rawTotal accumulates raw (still-compressed) bytes read across all
	// entries, mirroring total above. See readEntryRaw for why this is
	// needed even though total already bounds decompressed bytes.
	var rawTotal int64
	for _, f := range zr.File {
		if err := checkEntryName(f.Name); err != nil {
			return nil, err
		}
		if _, dup := pkg.parts[f.Name]; dup {
			// Duplicate names make the package ambiguous: Word reads the
			// entry the central directory points at, while a different
			// reader may pick the other one. Refuse rather than guess.
			return nil, fmt.Errorf("docx has duplicate entry name %q", f.Name)
		}

		content, err := readEntry(f, &total)
		if err != nil {
			return nil, err
		}
		rawBytes, err := readEntryRaw(f, &rawTotal, int64(len(data)))
		if err != nil {
			return nil, err
		}

		header := f.FileHeader
		pkg.names = append(pkg.names, f.Name)
		pkg.parts[f.Name] = content
		pkg.raw[f.Name] = rawBytes
		pkg.headers[f.Name] = &header
	}

	if _, ok := pkg.parts[contentTypesPart]; !ok {
		return nil, fmt.Errorf("not a valid .docx (missing %s)", contentTypesPart)
	}
	if _, ok := pkg.parts[DocumentPart]; !ok {
		return nil, fmt.Errorf("not a valid .docx (missing %s)", DocumentPart)
	}
	return pkg, nil
}

// checkEntryName rejects absolute paths, traversal segments, control bytes,
// and Windows drive-relative names ("C:evil.txt"), any of which could
// otherwise escape a target directory or corrupt a path if an entry were
// ever written out.
func checkEntryName(name string) error {
	if name == "" {
		return errors.New("docx has an unsafe entry name (empty)")
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 {
			return fmt.Errorf("docx has an unsafe entry name %q", name)
		}
	}
	if strings.HasPrefix(name, "/") || filepath.IsAbs(name) || strings.Contains(name, `\`) {
		return fmt.Errorf("docx has an unsafe entry name %q", name)
	}
	if len(name) >= 2 && isASCIILetter(name[0]) && name[1] == ':' {
		return fmt.Errorf("docx has an unsafe entry name %q", name)
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("docx has an unsafe entry name %q", name)
	}
	return nil
}

// isASCIILetter reports whether b is an ASCII letter, used to detect a
// Windows drive-relative prefix such as "C:".
func isASCIILetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// readEntry decompresses one entry while accumulating the running total, so
// a zip bomb is stopped mid-stream rather than after it has been fully
// expanded into memory.
func readEntry(f *zip.File, total *int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("open entry %s: %w", f.Name, err)
	}
	defer rc.Close()

	remaining := maxDecompressedBytes - *total
	if remaining < 0 {
		return nil, fmt.Errorf("docx decompresses to more than %d MB", int64(maxDecompressedBytes)>>20)
	}
	// Read one byte past the budget so an over-limit entry is detectable.
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(rc, remaining+1))
	if err != nil {
		return nil, fmt.Errorf("read entry %s: %w", f.Name, err)
	}
	if n > remaining {
		return nil, fmt.Errorf("docx decompresses to more than %d MB", int64(maxDecompressedBytes)>>20)
	}
	*total += n
	return buf.Bytes(), nil
}

// readEntryRaw captures the entry's compressed bytes so WriteTo can copy
// untouched entries verbatim, while accumulating the running total the same
// way readEntry does for decompressed bytes.
//
// This budget matters for a different reason than the decompression one:
// f.OpenRaw() sizes its reader from the entry's CompressedSize64 as recorded
// in the CENTRAL DIRECTORY, which is attacker-controlled and never
// cross-checked against the local header it is claimed to describe. A
// package can declare hundreds of entries whose CompressedSize64 vastly
// overstates the real data — or that all alias the very same (tiny) local
// header — and io.ReadAll(rc) will happily hand back nearly the whole
// remaining file on every single one of them, multiplying memory by the
// entry count. Decompression (readEntry) does not have this problem because
// a deflate stream is self-terminating at its own end-of-block marker
// regardless of what the central directory claims, but a raw byte copy has
// no such internal boundary to stop at. The sum of every entry's raw bytes
// can never legitimately exceed the size of the file that contains them, so
// that file size is the budget here, metered with the same
// read-one-byte-past-budget pattern readEntry already uses.
func readEntryRaw(f *zip.File, rawTotal *int64, budget int64) ([]byte, error) {
	rc, err := f.OpenRaw()
	if err != nil {
		return nil, fmt.Errorf("open raw entry %s: %w", f.Name, err)
	}
	remaining := budget - *rawTotal
	if remaining < 0 {
		return nil, fmt.Errorf("docx raw entry data exceeds the package's own file size")
	}
	// Read one byte past the budget so an over-limit entry is detectable
	// without ever buffering more than the budget+1 bytes.
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(rc, remaining+1))
	if err != nil {
		return nil, fmt.Errorf("read raw entry %s: %w", f.Name, err)
	}
	if n > remaining {
		return nil, fmt.Errorf("docx raw entry data exceeds the package's own file size")
	}
	*rawTotal += n
	return buf.Bytes(), nil
}

// Part returns an entry's decompressed content. The returned slice ALIASES
// the package's internal storage — it is not a copy, deliberately, since
// document.xml can be large and a later byte-range scanner would otherwise
// pay a copy on every call. A consequence of that aliasing: mutating the
// returned slice in place mutates the package's stored content too, but does
// NOT mark the entry modified, so WriteTo would still copy the entry's
// original compressed bytes and silently drop the edit. All edits must go
// through SetPart, never through in-place mutation of a slice from Part.
func (p *Package) Part(name string) ([]byte, bool) {
	data, ok := p.parts[name]
	return data, ok
}

// SetPart stages replacement content for an existing entry, to be written by
// WriteTo. Adding new entries is out of scope for SetPart: name must already
// be one of the package's entries, or SetPart returns a non-nil error and
// leaves the package unchanged. See AddPart for adding a brand-new entry.
func (p *Package) SetPart(name string, data []byte) error {
	if _, ok := p.parts[name]; !ok {
		return fmt.Errorf("docx has no entry named %q", name)
	}
	p.parts[name] = data
	p.modified[name] = true
	return nil
}

// AddPart adds a brand-new entry to the package, to be written by WriteTo.
// This is SetPart's counterpart for the one case SetPart deliberately
// refuses: name must NOT already exist, or AddPart returns a non-nil error
// and leaves the package unchanged (use SetPart to replace an existing
// entry's content instead).
//
// The new entry is appended to the END of p.names, so WriteTo's replay of
// that order writes it as the LAST entry in the output zip, after every
// original entry — never inserted in the middle, which would renumber
// nothing (WriteTo iterates by name, not by index) but would still be a
// gratuitous reordering of entries a byte-diff-minded caller has no reason
// to expect. Every original entry keeps its own original position, and
// (being untouched) is still copied via WriteTo's raw-bytes path, so this
// is the one way to grow a package that preserves every existing entry's
// bytes exactly (task 12 brief, item 1) — the same fidelity guarantee
// TestWriteTo_UntouchedPartsAreByteIdentical pins for SetPart already
// applies here, just for a name that did not exist before AddPart.
//
// name is validated with the exact same checkEntryName Open applies to
// every entry read from disk: a caller-constructed part name is just as
// capable of smuggling a path-traversal or control-byte entry as one read
// from an untrusted zip, and AddPart is the only place in this package that
// invents a NEW name rather than replaying one Open already validated.
//
// data is stored as-is; WriteTo synthesizes a fresh zip.FileHeader for it
// (Name, a Deflate method, and the current time as its Modified stamp,
// since there is no original header to carry forward the way SetPart's
// target always has) rather than requiring the caller to supply one.
func (p *Package) AddPart(name string, data []byte) error {
	if _, ok := p.parts[name]; ok {
		return fmt.Errorf("docx already has an entry named %q; use SetPart to replace its content", name)
	}
	if err := checkEntryName(name); err != nil {
		return err
	}
	p.names = append(p.names, name)
	p.parts[name] = data
	p.raw[name] = nil
	p.headers[name] = &zip.FileHeader{
		Name:     name,
		Method:   zip.Deflate,
		Modified: time.Now(),
	}
	p.modified[name] = true
	return nil
}

// Names returns the entry names in their original order.
func (p *Package) Names() []string {
	out := make([]string, len(p.names))
	copy(out, p.names)
	return out
}

// probeUmaskMode reports the permission mode the kernel actually applies to
// a freshly created file requested at 0o666 in dir, i.e. 0o666 &^ umask.
// This mirrors what os.WriteFile(path, data, 0644) would land at elsewhere
// in the codebase, without reading the process umask directly: chmod(2) is
// not umask-filtered, but open(2)/O_CREAT is, so creating (and immediately
// removing) a zero-byte probe file is a umask-faithful read that never
// mutates any state shared with other goroutines. The probe never holds
// content, so nothing is exposed through it. If any step fails — e.g. the
// directory is not writable — this falls back to 0o644 rather than failing
// the write that called it.
func probeUmaskMode(dir string) os.FileMode {
	const attempts = 4
	for i := 0; i < attempts; i++ {
		name := filepath.Join(dir, fmt.Sprintf(".docx-perm-probe-%d-%d-%d", os.Getpid(), time.Now().UnixNano(), i))
		f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666)
		if err != nil {
			if os.IsExist(err) {
				continue // distinctive name collided; try another
			}
			return 0o644
		}
		fi, statErr := f.Stat()
		f.Close()
		os.Remove(name)
		if statErr != nil {
			return 0o644
		}
		return fi.Mode().Perm()
	}
	return 0o644
}

// WriteTo writes the package to path atomically (temp file + rename), so an
// interrupted write can never leave a truncated .docx behind. Untouched
// entries are copied as raw compressed bytes, which both preserves their
// decompressed content exactly and avoids recompressing megabytes of media.
func (p *Package) WriteTo(dest string) error {
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".docx-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once the rename below has succeeded
	}()

	zw := zip.NewWriter(tmp)
	for _, name := range p.names {
		header := *p.headers[name]
		if p.modified[name] {
			header.Method = zip.Deflate
			w, err := zw.CreateHeader(&header)
			if err != nil {
				return fmt.Errorf("write entry %s: %w", name, err)
			}
			if _, err := w.Write(p.parts[name]); err != nil {
				return fmt.Errorf("write entry %s: %w", name, err)
			}
			continue
		}
		w, err := zw.CreateRaw(&header)
		if err != nil {
			return fmt.Errorf("copy entry %s: %w", name, err)
		}
		if _, err := w.Write(p.raw[name]); err != nil {
			return fmt.Errorf("copy entry %s: %w", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("finalize zip: %w", err)
	}

	// os.CreateTemp always creates with mode 0600, so without this the
	// rename below would silently tighten the destination's permissions to
	// owner-only regardless of the replaced file's mode. Match the existing
	// destination's mode when there is one; otherwise probe the umask (see
	// probeUmaskMode) so a brand-new .docx lands at the same mode
	// os.WriteFile(dest, data, 0644) would produce elsewhere in this
	// codebase, rather than always 0644 regardless of a stricter umask.
	//
	// The probe is deliberately NOT syscall.Umask(0)+restore: that call
	// mutates PROCESS-GLOBAL state, and deepai executes tools concurrently
	// (see partitionToolCalls in pkg/agent/toolexec.go) and runs a subagent pool,
	// so any file another goroutine created inside that window would be
	// produced with no umask applied. Creating and immediately removing a
	// throwaway probe file achieves the same read without ever touching
	// state shared with other goroutines.
	var mode os.FileMode
	if fi, statErr := os.Stat(dest); statErr == nil {
		mode = fi.Mode().Perm()
	} else {
		mode = probeUmaskMode(filepath.Dir(dest))
	}
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}

	// Sync before close so a power loss right after rename can't leave the
	// destination zero-length: rename is atomic against other processes,
	// but not against the temp file's own dirty pages still being unflushed.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("rename temp file into place: %w", err)
	}
	return nil
}
