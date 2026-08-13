package docx

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

const fixture = "testdata/structure.docx"

func TestOpen_ReadsKnownParts(t *testing.T) {
	pkg, err := Open(fixture)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	doc, ok := pkg.Part(DocumentPart)
	if !ok {
		t.Fatalf("Part(%q) missing", DocumentPart)
	}
	if !bytes.Contains(doc, []byte("<w:p")) {
		t.Errorf("document.xml has no paragraphs")
	}
	for _, want := range []string{"[Content_Types].xml", "word/header1.xml", "word/footer1.xml"} {
		if _, ok := pkg.Part(want); !ok {
			t.Errorf("Part(%q) missing", want)
		}
	}
}

// TestWriteTo_UntouchedPartsAreByteIdentical is the core Ground Truth guard:
// rewriting without any SetPart must reproduce every entry's decompressed
// bytes exactly, and keep the entry set and order intact.
func TestWriteTo_UntouchedPartsAreByteIdentical(t *testing.T) {
	pkg, err := Open(fixture)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	out := filepath.Join(t.TempDir(), "out.docx")
	if err := pkg.WriteTo(out); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	assertEntriesEqual(t, fixture, out, nil)
}

// assertEntriesEqual compares two .docx by decompressed entry content.
// Entries named in changed are expected to differ; all others must match
// byte for byte. Entry names and their order must match exactly.
func assertEntriesEqual(t *testing.T, oldPath, newPath string, changed map[string]bool) {
	t.Helper()
	oldZ, err := zip.OpenReader(oldPath)
	if err != nil {
		t.Fatalf("open old: %v", err)
	}
	defer oldZ.Close()
	newZ, err := zip.OpenReader(newPath)
	if err != nil {
		t.Fatalf("open new: %v", err)
	}
	defer newZ.Close()

	if len(oldZ.File) != len(newZ.File) {
		t.Fatalf("entry count: old %d, new %d", len(oldZ.File), len(newZ.File))
	}
	for i := range oldZ.File {
		oldName, newName := oldZ.File[i].Name, newZ.File[i].Name
		if oldName != newName {
			t.Fatalf("entry %d: name old %q, new %q", i, oldName, newName)
		}
		oldData := readZipEntry(t, oldZ.File[i])
		newData := readZipEntry(t, newZ.File[i])
		if changed[oldName] {
			if bytes.Equal(oldData, newData) {
				t.Errorf("%s: expected to change but is identical", oldName)
			}
			continue
		}
		if !bytes.Equal(oldData, newData) {
			t.Errorf("%s: not byte-identical (old %d bytes, new %d bytes)", oldName, len(oldData), len(newData))
		}
	}
}

func readZipEntry(t *testing.T, f *zip.File) []byte {
	t.Helper()
	rc, err := f.Open()
	if err != nil {
		t.Fatalf("open entry %s: %v", f.Name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read entry %s: %v", f.Name, err)
	}
	return data
}

func TestWriteTo_ModifiedPartOnly(t *testing.T) {
	pkg, err := Open(fixture)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	doc, _ := pkg.Part(DocumentPart)
	if err := pkg.SetPart(DocumentPart, bytes.Replace(doc, []byte("Hello "), []byte("Howdy "), 1)); err != nil {
		t.Fatalf("SetPart: %v", err)
	}

	out := filepath.Join(t.TempDir(), "out.docx")
	if err := pkg.WriteTo(out); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	assertEntriesEqual(t, fixture, out, map[string]bool{DocumentPart: true})
}

func TestOpen_RejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{
			name:    "encrypted CFB container",
			data:    append([]byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}, make([]byte, 64)...),
			wantErr: "encrypted",
		},
		{
			name:    "not a zip",
			data:    []byte("this is plain text, not a docx at all"),
			wantErr: "not a valid .docx",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := filepath.Join(dir, "bad.docx")
			if err := os.WriteFile(p, tt.data, 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Open(p)
			if err == nil {
				t.Fatalf("Open succeeded, want error containing %q", tt.wantErr)
			}
			if !bytes.Contains([]byte(err.Error()), []byte(tt.wantErr)) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestOpen_RejectsDuplicateEntryNames(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dup.docx")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"[Content_Types].xml", "word/document.xml", "word/document.xml"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("<x/>")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(p)
	if err == nil {
		t.Fatal("Open succeeded on duplicate entry names, want error")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("duplicate")) {
		t.Errorf("error = %q, want it to mention duplicate", err)
	}
}

func TestOpen_RejectsPathTraversal(t *testing.T) {
	p := filepath.Join(t.TempDir(), "trav.docx")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"[Content_Types].xml", "word/document.xml", "../../evil.sh"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(p)
	if err == nil {
		t.Fatal("Open succeeded on traversal entry, want error")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("unsafe entry name")) {
		t.Errorf("error = %q, want it to mention unsafe entry name", err)
	}
}

func TestOpen_RejectsMissingContentTypes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "noct.docx")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("<x/>")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Open(p)
	if err == nil {
		t.Fatal("Open succeeded without [Content_Types].xml, want error")
	}
	// Assert on the specific missing part, not the "not a valid .docx"
	// prefix: that prefix is shared with the "cannot read as zip" error, so
	// asserting only on it would stay green even if this check regressed.
	if !bytes.Contains([]byte(err.Error()), []byte("[Content_Types].xml")) {
		t.Errorf("error = %q, want it to mention [Content_Types].xml", err)
	}
}

// TestSetPart_UnknownNameReturnsError pins the real SetPart contract: an
// unknown name is an error, not a silent no-op that WriteTo later reports.
func TestSetPart_UnknownNameReturnsError(t *testing.T) {
	pkg, err := Open(fixture)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	before, _ := pkg.Part(DocumentPart)
	beforeCopy := append([]byte(nil), before...)

	if err := pkg.SetPart("word/does-not-exist.xml", []byte("data")); err == nil {
		t.Fatal("SetPart on unknown name succeeded, want error")
	}
	if _, ok := pkg.Part("word/does-not-exist.xml"); ok {
		t.Errorf("SetPart on unknown name created a new entry")
	}
	after, _ := pkg.Part(DocumentPart)
	if !bytes.Equal(beforeCopy, after) {
		t.Errorf("SetPart on unknown name mutated an existing part")
	}
}

// TestAddPart_AppendsNewEntryKeepingOriginalsByteIdentical is task 12's
// core fidelity gate for AddPart (brief item 1): adding a brand-new entry
// must leave every one of the original entries byte-for-byte identical —
// the same guarantee TestWriteTo_ModifiedPartOnly pins for SetPart — and
// the new entry must land at the END of the zip, after every original one.
func TestAddPart_AppendsNewEntryKeepingOriginalsByteIdentical(t *testing.T) {
	pkg, err := Open(fixture)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	origNames := pkg.Names()

	const newName = "word/footer2.xml"
	newContent := []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:ftr/>`)
	if err := pkg.AddPart(newName, newContent); err != nil {
		t.Fatalf("AddPart: %v", err)
	}

	out := filepath.Join(t.TempDir(), "added.docx")
	if err := pkg.WriteTo(out); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	oldZ, err := zip.OpenReader(fixture)
	if err != nil {
		t.Fatalf("open old: %v", err)
	}
	defer oldZ.Close()
	newZ, err := zip.OpenReader(out)
	if err != nil {
		t.Fatalf("open new: %v", err)
	}
	defer newZ.Close()

	if got, want := len(newZ.File), len(oldZ.File)+1; got != want {
		t.Fatalf("entry count = %d, want %d (original %d + 1 new)", got, want, len(oldZ.File))
	}

	// Every ORIGINAL entry, in its original order, byte-identical.
	for i, want := range origNames {
		if newZ.File[i].Name != want {
			t.Fatalf("entry %d name = %q, want %q (original order not preserved)", i, newZ.File[i].Name, want)
		}
		oldData := readZipEntry(t, oldZ.File[i])
		newData := readZipEntry(t, newZ.File[i])
		if !bytes.Equal(oldData, newData) {
			t.Errorf("%s: not byte-identical after AddPart (old %d bytes, new %d bytes)", want, len(oldData), len(newData))
		}
	}

	// The new entry is last, with the content AddPart was given.
	last := newZ.File[len(newZ.File)-1]
	if last.Name != newName {
		t.Fatalf("last entry name = %q, want %q (new entry must be appended at the tail)", last.Name, newName)
	}
	if got := readZipEntry(t, last); !bytes.Equal(got, newContent) {
		t.Errorf("new entry content = %q, want %q", got, newContent)
	}
}

// TestAddPart_RejectsExistingName pins that AddPart is SetPart's mirror
// image: an existing name must be refused (use SetPart instead), and the
// package must be left completely unchanged by the rejected call.
func TestAddPart_RejectsExistingName(t *testing.T) {
	pkg, err := Open(fixture)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	before, _ := pkg.Part(DocumentPart)
	beforeCopy := append([]byte(nil), before...)
	namesBefore := pkg.Names()

	if err := pkg.AddPart(DocumentPart, []byte("data")); err == nil {
		t.Fatal("AddPart on an existing name succeeded, want error")
	}

	after, _ := pkg.Part(DocumentPart)
	if !bytes.Equal(beforeCopy, after) {
		t.Errorf("AddPart on an existing name mutated that entry's content")
	}
	if len(pkg.Names()) != len(namesBefore) {
		t.Errorf("AddPart on an existing name changed the entry count")
	}
}

// TestAddPart_RejectsUnsafeName pins that AddPart runs the exact same
// checkEntryName guard Open applies to every entry read from disk: a
// caller-constructed part name is just as capable of a path-traversal
// escape as one read from an untrusted zip.
func TestAddPart_RejectsUnsafeName(t *testing.T) {
	pkg, err := Open(fixture)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := pkg.AddPart("../../evil.xml", []byte("x")); err == nil {
		t.Fatal("AddPart on an unsafe name succeeded, want error")
	}
	if _, ok := pkg.Part("../../evil.xml"); ok {
		t.Error("AddPart on an unsafe name still added the entry")
	}
}

// TestCheckEntryName is a table-driven pin of every branch in
// checkEntryName, including the control-byte and Windows drive-relative
// cases that have no fixture-level coverage elsewhere.
func TestCheckEntryName(t *testing.T) {
	reject := []string{
		"",
		"..",
		"../x",
		"a/../../x",
		"/etc/passwd",
		`a\b`,
		"\x00bad",
		"C:evil.txt",
	}
	for _, name := range reject {
		t.Run(fmt.Sprintf("reject_%q", name), func(t *testing.T) {
			if err := checkEntryName(name); err == nil {
				t.Errorf("checkEntryName(%q) = nil, want error", name)
			}
		})
	}

	allow := []string{
		"a/..",
		"./x",
		"word/document.xml",
		"[Content_Types].xml",
	}
	for _, name := range allow {
		t.Run(fmt.Sprintf("allow_%q", name), func(t *testing.T) {
			if err := checkEntryName(name); err != nil {
				t.Errorf("checkEntryName(%q) = %v, want nil", name, err)
			}
		})
	}
}

// TestOpen_RejectsTooManyEntries pins the maxZipEntries guard, which
// otherwise has no coverage: a package with an absurd number of tiny
// entries should be rejected before any content is read.
func TestOpen_RejectsTooManyEntries(t *testing.T) {
	p := filepath.Join(t.TempDir(), "many.docx")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i < maxZipEntries+1; i++ {
		w, err := zw.Create(fmt.Sprintf("f%d.xml", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(p)
	if err == nil {
		t.Fatal("Open succeeded with more than maxZipEntries entries, want error")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("limit is")) {
		t.Errorf("error = %q, want it to mention the entry limit", err)
	}
}

// TestNames_OrderAndDefensiveCopy pins that Names() preserves the original
// zip entry order and returns a copy the caller cannot use to corrupt the
// package's internal state.
func TestNames_OrderAndDefensiveCopy(t *testing.T) {
	pkg, err := Open(fixture)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	names := pkg.Names()
	if len(names) == 0 {
		t.Fatal("Names() returned an empty slice")
	}
	if names[0] != "[Content_Types].xml" {
		t.Errorf("Names()[0] = %q, want %q (original zip order)", names[0], "[Content_Types].xml")
	}

	names[0] = "tampered"
	again := pkg.Names()
	if again[0] == "tampered" {
		t.Errorf("Names() returned an aliased slice: caller mutation leaked into the package")
	}
}

// TestOpen_RejectsNonRegularFile pins finding 1: a FIFO's path-based
// os.Stat reports Size() 0, so a naive stat-then-ReadFile guard would pass
// it through and then block/OOM reading an endless stream. Open must reject
// it outright by checking the held fd's mode. Windows has no FIFOs, so this
// is skipped there.
//
// Opening a FIFO for read blocks the calling goroutine until some writer
// opens the other end, so a concurrent writer is opened here purely to
// unblock Open's os.Open call; Open must still reject the file once it gets
// past that point, without reading anything from it.
func TestOpen_RejectsNonRegularFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFOs are not available on windows")
	}
	fifoPath := filepath.Join(t.TempDir(), "fifo.docx")
	if err := mkfifo(fifoPath); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	go func() {
		wf, err := os.OpenFile(fifoPath, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer wf.Close()
		// If Open ever regresses to reading from the fd, give it something
		// to read so a test failure shows the wrong error instead of a
		// deadlock hanging the whole test run.
		wf.Write(make([]byte, 4096))
	}()

	done := make(chan struct{})
	var openErr error
	go func() {
		_, openErr = Open(fifoPath)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Open did not return within 5s on a FIFO; it likely blocked reading it")
	}

	if openErr == nil {
		t.Fatal("Open succeeded on a FIFO, want error")
	}
	if !bytes.Contains([]byte(openErr.Error()), []byte("not a regular file")) {
		t.Errorf("error = %q, want it to mention that the path is not a regular file", openErr)
	}
}

// TestOpen_DoesNotBlockOnFIFOWithNoWriter pins finding 4: opening a FIFO for
// read blocks in open(2) until some other process opens it for write, unless
// the open itself uses O_NONBLOCK. Unlike TestOpen_RejectsNonRegularFile
// above, this test deliberately opens NO writer at all, so a regression back
// to a plain os.Open would hang past the timeout and fail this test instead
// of passing for the wrong reason.
func TestOpen_DoesNotBlockOnFIFOWithNoWriter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFOs are not available on windows")
	}
	fifoPath := filepath.Join(t.TempDir(), "fifo.docx")
	if err := mkfifo(fifoPath); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := Open(fifoPath)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Open succeeded on a FIFO, want error")
		}
		if !bytes.Contains([]byte(err.Error()), []byte("not a regular file")) {
			t.Errorf("error = %q, want it to mention that the path is not a regular file", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Open did not return within 2s on a FIFO with no writer; the open call is blocking")
	}
}

// TestOpen_RejectsOversizeFile pins the maxDocxBytes guard directly, and
// pins the boundary: exactly-at-the-limit must NOT be rejected by the size
// guard (it is later rejected for not being a valid zip, which is fine and
// is asserted here only by exclusion). The oversize file is created with
// Truncate on an empty file rather than by writing real bytes, so the test
// stays instant and does not require materializing 25+MB on disk.
func TestOpen_RejectsOversizeFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("over the limit is rejected by the size guard", func(t *testing.T) {
		p := filepath.Join(dir, "big.docx")
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(maxDocxBytes + 1); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}

		_, err = Open(p)
		if err == nil {
			t.Fatal("Open succeeded on an oversize file, want error")
		}
		if !bytes.Contains([]byte(err.Error()), []byte("exceeds")) {
			t.Errorf("error = %q, want it to mention the size limit", err)
		}
	})

	t.Run("exactly at the limit is not rejected by the size guard", func(t *testing.T) {
		p := filepath.Join(dir, "atlimit.docx")
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(maxDocxBytes); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}

		_, err = Open(p)
		if err == nil {
			t.Fatal("Open succeeded on a sparse all-zero file, want a not-a-zip error")
		}
		if bytes.Contains([]byte(err.Error()), []byte("exceeds")) {
			t.Errorf("error = %q, want the not-a-zip error, not the size guard rejecting the boundary case", err)
		}
	})
}

// TestReadEntry_BudgetBoundary calls the unexported readEntry directly with
// a pre-seeded running total sitting exactly at maxDecompressedBytes, so it
// pins the remaining < 0 (not remaining <= 0) fix without needing a real
// 200MB fixture: a 0-byte entry exactly at the budget must still succeed,
// and a 1-byte entry exactly at the budget must fail. Against the old
// `remaining <= 0` guard the 0-byte case would incorrectly fail.
func TestReadEntry_BudgetBoundary(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if _, err := zw.Create("zero.bin"); err != nil {
		t.Fatal(err)
	}
	w1, err := zw.Create("one.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w1.Write([]byte{0x42}); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var zeroFile, oneFile *zip.File
	for _, f := range zr.File {
		switch f.Name {
		case "zero.bin":
			zeroFile = f
		case "one.bin":
			oneFile = f
		}
	}
	if zeroFile == nil || oneFile == nil {
		t.Fatal("in-memory fixture zip is missing expected entries")
	}

	t.Run("0-byte entry exactly at the budget succeeds", func(t *testing.T) {
		total := int64(maxDecompressedBytes)
		data, err := readEntry(zeroFile, &total)
		if err != nil {
			t.Fatalf("readEntry: %v", err)
		}
		if len(data) != 0 {
			t.Errorf("len(data) = %d, want 0", len(data))
		}
	})

	t.Run("1-byte entry exactly at the budget fails", func(t *testing.T) {
		total := int64(maxDecompressedBytes)
		_, err := readEntry(oneFile, &total)
		if err == nil {
			t.Fatal("readEntry succeeded 1 byte over the decompression budget, want error")
		}
		if !bytes.Contains([]byte(err.Error()), []byte("decompresses to more than")) {
			t.Errorf("error = %q, want it to mention the decompression limit", err)
		}
	})
}

// TestWriteTo_PreservesDestinationPermissions pins that a rewrite does not
// silently tighten a document's permissions. os.CreateTemp creates 0600, so
// without the explicit Chmod in WriteTo the renamed file would come back
// owner-only and a shared document would stop being readable by its group.
func TestWriteTo_PreservesDestinationPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}
	pkg, err := Open(fixture)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Run("existing destination keeps its mode", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "existing.docx")
		if err := os.WriteFile(dest, []byte("placeholder"), 0o664); err != nil {
			t.Fatal(err)
		}
		// os.WriteFile is subject to umask, and under a common umask of 022
		// 0o664 lands at exactly 0o644 — the same value as WriteTo's
		// no-pre-existing-destination fallback. That would make this
		// subtest pass even if the "stat the destination and reuse its
		// mode" branch were deleted entirely. os.Chmod is NOT
		// umask-filtered, so force the file to a mode (0640) that cannot
		// coincide with the fallback, ensuring the subtest actually
		// exercises the reuse-existing-mode branch.
		if err := os.Chmod(dest, 0o640); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(dest)
		if err != nil {
			t.Fatal(err)
		}
		if err := pkg.WriteTo(dest); err != nil {
			t.Fatalf("WriteTo: %v", err)
		}
		after, err := os.Stat(dest)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := after.Mode().Perm(), before.Mode().Perm(); got != want {
			t.Errorf("mode = %v, want the pre-existing %v", got, want)
		}
	})

	t.Run("new destination is umask-faithful, not a hardcoded 0644", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "new.docx")

		// Derive the expected mode the same way WriteTo's probeUmaskMode
		// does, rather than hardcoding 0644: that keeps the test correct
		// under any umask instead of only under the common umask 022 (where
		// 0666&^022 happens to equal 0644).
		probe := filepath.Join(dir, "expect-probe")
		pf, err := os.OpenFile(probe, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666)
		if err != nil {
			t.Fatal(err)
		}
		fi, err := pf.Stat()
		if err != nil {
			t.Fatal(err)
		}
		pf.Close()
		os.Remove(probe)
		want := fi.Mode().Perm()

		if err := pkg.WriteTo(dest); err != nil {
			t.Fatalf("WriteTo: %v", err)
		}
		got, err := os.Stat(dest)
		if err != nil {
			t.Fatal(err)
		}
		if got.Mode().Perm() != want {
			t.Errorf("mode = %v, want %v (0666 &^ umask)", got.Mode().Perm(), want)
		}
	})
}

// buildRawAmplificationZip hand-forges a zip whose central directory
// contains nForged entries with unique names that all point their "relative
// offset of local header" field at the SAME real local header (the one for
// "word/blob.xml"), while each central-directory record's CompressedSize
// field lies about how much compressed data sits there (bogusCompSize, far
// larger than the file actually is).
//
// This cannot be built with the high-level zip.Writer API (it always
// advances the offset for a new entry), so the central directory is edited
// at the byte level: build a normal 3-entry zip, locate word/blob.xml's real
// local-header offset via DataOffset() minus its own header+name length,
// then append nForged synthesized central-directory file header records
// (APPNOTE.TXT central directory file header, 46 bytes fixed + name) ahead
// of the End Of Central Directory record, and patch the EOCD's entry counts
// and central-directory size to match.
//
// Every entry's real (shared) local header is a genuine tiny deflate stream
// that decompresses to exactly one byte, so f.Open() (decompression) always
// terminates at the real end-of-stream marker regardless of the forged
// CompressedSize — decompression is self-terminating, raw copying is not.
// That asymmetry is exactly what makes readEntryRaw exploitable while
// readEntry already was not.
func buildRawAmplificationZip(t *testing.T, nForged int, bogusCompSize uint32) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("[Content_Types].xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("<Types/>")); err != nil {
		t.Fatal(err)
	}
	w, err = zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("<w:document/>")); err != nil {
		t.Fatal(err)
	}
	w, err = zw.Create("word/blob.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("A")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	baseData := buf.Bytes()

	zr, err := zip.NewReader(bytes.NewReader(baseData), int64(len(baseData)))
	if err != nil {
		t.Fatal(err)
	}
	var blob *zip.File
	for _, f := range zr.File {
		if f.Name == "word/blob.xml" {
			blob = f
		}
	}
	if blob == nil {
		t.Fatal("word/blob.xml not found in freshly built zip")
	}
	dataOffset, err := blob.DataOffset()
	if err != nil {
		t.Fatal(err)
	}
	// DataOffset is headerOffset + fixed-header(30) + name length (extra
	// field is empty for entries zip.Writer produces this way).
	blobHeaderOffset := uint32(dataOffset) - 30 - uint32(len("word/blob.xml"))

	eocdOffset := len(baseData) - 22
	if !bytes.Equal(baseData[eocdOffset:eocdOffset+4], []byte{0x50, 0x4b, 0x05, 0x06}) {
		t.Fatal("EOCD not found where expected; zip.Writer output format changed")
	}
	origNumEntries := binary.LittleEndian.Uint16(baseData[eocdOffset+10 : eocdOffset+12])
	cdOffset := binary.LittleEndian.Uint32(baseData[eocdOffset+16 : eocdOffset+20])
	cdSize := binary.LittleEndian.Uint32(baseData[eocdOffset+12 : eocdOffset+16])
	if int(cdOffset+cdSize) != eocdOffset {
		t.Fatal("central directory does not end immediately before the EOCD")
	}

	var forged bytes.Buffer
	for i := 0; i < nForged; i++ {
		name := fmt.Sprintf("dup%03d.bin", i)
		rec := make([]byte, 46+len(name))
		binary.LittleEndian.PutUint32(rec[0:4], 0x02014b50) // central dir signature
		binary.LittleEndian.PutUint16(rec[4:6], 20)         // version made by
		binary.LittleEndian.PutUint16(rec[6:8], 20)         // version needed
		binary.LittleEndian.PutUint16(rec[8:10], 0)         // flags: no data descriptor
		binary.LittleEndian.PutUint16(rec[10:12], uint16(blob.Method))
		binary.LittleEndian.PutUint16(rec[12:14], 0) // mod time
		binary.LittleEndian.PutUint16(rec[14:16], 0) // mod date
		binary.LittleEndian.PutUint32(rec[16:20], blob.CRC32)
		binary.LittleEndian.PutUint32(rec[20:24], bogusCompSize) // THE LIE
		binary.LittleEndian.PutUint32(rec[24:28], uint32(blob.UncompressedSize64))
		binary.LittleEndian.PutUint16(rec[28:30], uint16(len(name)))
		binary.LittleEndian.PutUint16(rec[30:32], 0) // extra field length
		binary.LittleEndian.PutUint16(rec[32:34], 0) // comment length
		binary.LittleEndian.PutUint16(rec[34:36], 0) // disk number start
		binary.LittleEndian.PutUint16(rec[36:38], 0) // internal attributes
		binary.LittleEndian.PutUint32(rec[38:42], 0) // external attributes
		binary.LittleEndian.PutUint32(rec[42:46], blobHeaderOffset)
		copy(rec[46:], name)
		forged.Write(rec)
	}

	final := make([]byte, 0, eocdOffset+forged.Len()+22)
	final = append(final, baseData[:eocdOffset]...)
	final = append(final, forged.Bytes()...)
	newEOCD := append([]byte(nil), baseData[eocdOffset:]...)
	binary.LittleEndian.PutUint16(newEOCD[8:10], origNumEntries+uint16(nForged))
	binary.LittleEndian.PutUint16(newEOCD[10:12], origNumEntries+uint16(nForged))
	binary.LittleEndian.PutUint32(newEOCD[12:16], cdSize+uint32(forged.Len()))
	final = append(final, newEOCD...)
	return final
}

// TestOpen_RejectsUnboundedRawEntryAmplification pins C1: readEntryRaw must
// meter its reads the same way readEntry already does, because
// f.OpenRaw()'s SectionReader is sized from the CENTRAL DIRECTORY's
// CompressedSize64 — attacker-controlled and never cross-checked against
// reality. Without a budget, hundreds of entries can all point at one small
// real local header while each declaring a huge CompressedSize, and
// io.ReadAll(rc) hands back nearly the whole remaining file on every single
// one of them: the sum of all "raw" bytes read balloons far past the size of
// the file that supposedly contains them, while decompression (readEntry)
// stays small because a deflate stream is self-terminating and stops at its
// own end-of-block marker regardless of what the central directory claims.
//
// The forged file here is ~11KB and the amplified read total is ~2.3MB
// (about 200x) — small enough to keep this test fast and memory-bounded,
// but the ratio is the same mechanism the reviewer scaled to 555MB from a
// 3MB file (and ~50GB from a 25MB file at the real caps).
func TestOpen_RejectsUnboundedRawEntryAmplification(t *testing.T) {
	const nForged = 200
	const bogusCompSize = 100000

	data := buildRawAmplificationZip(t, nForged, bogusCompSize)
	if len(data) >= bogusCompSize {
		t.Fatalf("forged file is %d bytes, want it much smaller than the declared per-entry size %d for the exploit to be meaningful", len(data), bogusCompSize)
	}

	p := filepath.Join(t.TempDir(), "rawbomb.docx")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Open(p)
	if err == nil {
		t.Fatalf("Open succeeded on a zip whose declared raw entry sizes sum to ~%d bytes from an %d byte file, want an error", nForged*bogusCompSize, len(data))
	}
	if !bytes.Contains([]byte(err.Error()), []byte("raw")) {
		t.Errorf("error = %q, want it to mention the raw entry budget", err)
	}
}
