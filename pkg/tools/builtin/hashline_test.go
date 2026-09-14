package builtin

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

// hashEditCall invokes edit_file in hash mode. newString may be omitted from
// the arguments entirely by passing hasNew=false, to test the missing-key path.
func hashEditCall(t *testing.T, path, startHash, endHash string, newString any) (models.ToolResult, error) {
	t.Helper()
	args := map[string]any{"path": path, "start_hash": startHash}
	if endHash != "" {
		args["end_hash"] = endHash
	}
	if newString != nil {
		args["new_string"] = newString
	}
	return EditFileHandler(context.Background(), models.ToolCall{ID: "he", Name: "edit_file", Arguments: args})
}

// prefixFor renders the "N:hhhhhh" ref for the 1-based line number in content,
// the way a model would copy it out of read_file's numbered output.
func prefixFor(t *testing.T, content string, lineno int) string {
	t.Helper()
	lines := splitFileLines(content)
	if lineno < 1 || lineno > len(lines) {
		t.Fatalf("prefixFor: line %d out of range (%d lines)", lineno, len(lines))
	}
	return numberedRef(lineno, lines[lineno-1])
}

func numberedRef(lineno int, line string) string {
	var b strings.Builder
	writeHashNumberedLine(&b, 1, lineno, line)
	s := b.String()
	return s[:strings.IndexByte(s, '\t')]
}

func readBack(t *testing.T, path string) string {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

// twoFuncs is the D1 shape: the target function's closing "}" also closes
// every later function, so its hash is shared and only the line hint from the
// copied prefix can disambiguate.
const twoFuncs = "func Foo() {\n\ta()\n\tb()\n}\n\nfunc Bar() {\n\tc()\n}\n"

func TestHashEdit_WholeFunctionWithSharedClosingBrace(t *testing.T) {
	path := writeTestFile(t, twoFuncs)
	res, err := hashEditCall(t, path,
		prefixFor(t, twoFuncs, 1), // func Foo() {
		prefixFor(t, twoFuncs, 4), // } — same content (and hash) as line 8
		"func Foo() {\n\tz()\n}")  // no trailing newline: §7 must append it
	if err != nil {
		t.Fatalf("whole-function hash edit failed: %v", err)
	}
	want := "func Foo() {\n\tz()\n}\n\nfunc Bar() {\n\tc()\n}\n"
	if got := readBack(t, path); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if !strings.Contains(res.Content, "Replaced lines 1-4") {
		t.Fatalf("result message = %q, want a Replaced lines 1-4 report", res.Content)
	}
	if old, _ := res.Data["old_text"].(string); old != "func Foo() {\n\ta()\n\tb()\n}\n" {
		t.Fatalf("Data[old_text] = %q", old)
	}
	if sl, _ := res.Data["start_line"].(int); sl != 1 {
		t.Fatalf("Data[start_line] = %v", res.Data["start_line"])
	}
}

func TestHashEdit_BareHexSharedBraceIsAmbiguous(t *testing.T) {
	path := writeTestFile(t, twoFuncs)
	// Same edit but with bare hashes (no line hints): the "}" hash matches
	// lines 4 and 8, spans {(1,4),(1,8)} — must reject, not guess.
	_, err := hashEditCall(t, path, lineHash("func Foo() {"), lineHash("}"), "x")
	if err == nil || !strings.Contains(err.Error(), "N:hhhhhh") {
		t.Fatalf("want ambiguity error telling the model to copy the full prefix, got %v", err)
	}
	if got := readBack(t, path); got != twoFuncs {
		t.Fatal("file must be untouched on ambiguity")
	}
}

func TestHashEdit_StaleHintsFallBackToNearestSpan(t *testing.T) {
	// E5: edit the top of the file first, then reuse the ORIGINAL prefixes for
	// a function that moved down. With a function still below the target, the
	// shared "}" yields several hash-matching spans, the stale exact hit
	// fails, and the nearest-span scoring must pick the shifted target — no
	// re-read, and no touching func C.
	content := "func A() {\n}\n\nfunc B() {\n}\n\nfunc C() {\n}\n"
	path := writeTestFile(t, content)
	bStart := prefixFor(t, content, 4) // func B() {
	bEnd := prefixFor(t, content, 5)   // } — also matches lines 2 and 8

	if _, err := hashEditCall(t, path, prefixFor(t, content, 1), prefixFor(t, content, 1),
		"// x\n// y\nfunc A() {"); err != nil {
		t.Fatalf("first edit: %v", err)
	}
	if _, err := hashEditCall(t, path, bStart, bEnd, "func B() {\n\tz()\n}"); err != nil {
		t.Fatalf("second edit with stale hints: %v", err)
	}
	want := "// x\n// y\nfunc A() {\n}\n\nfunc B() {\n\tz()\n}\n\nfunc C() {\n}\n"
	if got := readBack(t, path); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestHashEdit_AnchorContentChangedFails(t *testing.T) {
	content := "alpha\nbeta\ngamma\n"
	path := writeTestFile(t, content)
	ref := prefixFor(t, content, 2)
	if err := os.WriteFile(path, []byte("alpha\nBETA\ngamma\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := hashEditCall(t, path, ref, "", "replacement")
	if err == nil || !strings.Contains(err.Error(), "not in") {
		t.Fatalf("want hash-miss error, got %v", err)
	}
	if got := readBack(t, path); got != "alpha\nBETA\ngamma\n" {
		t.Fatal("file must be untouched on hash miss")
	}
}

func TestHashEdit_SingleLineDefaultEnd(t *testing.T) {
	content := "one\ntwo\nthree\n"
	path := writeTestFile(t, content)
	if _, err := hashEditCall(t, path, prefixFor(t, content, 2), "", "TWO"); err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, path); got != "one\nTWO\nthree\n" {
		t.Fatalf("got %q", got)
	}
}

func TestHashEdit_RepeatedSingleLineWithoutHintIsAmbiguous(t *testing.T) {
	content := "a\n}\nb\n}\nc\n}\n"
	path := writeTestFile(t, content)
	_, err := hashEditCall(t, path, lineHash("}"), "", "x")
	if err == nil || !strings.Contains(err.Error(), "occurs 3 times") {
		t.Fatalf("want occurs-N-times error, got %v", err)
	}
	if got := readBack(t, path); got != content {
		t.Fatal("must not silently edit the first occurrence")
	}
}

func TestHashEdit_SwappedStartEnd(t *testing.T) {
	content := "one\ntwo\nthree\n"
	path := writeTestFile(t, content)
	_, err := hashEditCall(t, path, prefixFor(t, content, 3), prefixFor(t, content, 1), "x")
	if err == nil || !strings.Contains(err.Error(), "swap") {
		t.Fatalf("want swap error, got %v", err)
	}
}

func TestHashEdit_TypoHintsClosestHash(t *testing.T) {
	content := "one\ntwo\nthree\n"
	path := writeTestFile(t, content)
	real := lineHash("two")
	// Flip one hex character to a value that stays valid hex.
	typo := []byte(real)
	if typo[0] == 'a' {
		typo[0] = 'b'
	} else {
		typo[0] = 'a'
	}
	_, err := hashEditCall(t, path, string(typo), "", "x")
	if err == nil || !strings.Contains(err.Error(), "closest hash is "+real) {
		t.Fatalf("want closest-hash hint for %s, got %v", real, err)
	}
}

func TestHashEdit_NoTrailingNewlineMidFileGetsOne(t *testing.T) {
	content := "one\ntwo\nthree\n"
	path := writeTestFile(t, content)
	if _, err := hashEditCall(t, path, prefixFor(t, content, 2), "", "2a\n2b"); err != nil {
		t.Fatal(err)
	}
	// D2: without the appended newline this would read "2a\n2bthree\n".
	if got := readBack(t, path); got != "one\n2a\n2b\nthree\n" {
		t.Fatalf("got %q", got)
	}
}

func TestHashEdit_LastLineFollowsEOFNewlineState(t *testing.T) {
	withNL := "one\ntwo\n"
	path := writeTestFile(t, withNL)
	if _, err := hashEditCall(t, path, prefixFor(t, withNL, 2), "", "TWO"); err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, path); got != "one\nTWO\n" {
		t.Fatalf("file with final newline: got %q", got)
	}

	withoutNL := "one\ntwo"
	path = writeTestFile(t, withoutNL)
	if _, err := hashEditCall(t, path, prefixFor(t, withoutNL, 2), "", "TWO"); err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, path); got != "one\nTWO" {
		t.Fatalf("file without final newline: got %q", got)
	}
}

func TestHashEdit_DeleteRange(t *testing.T) {
	content := "one\ntwo\nthree\nfour\n"
	path := writeTestFile(t, content)
	res, err := hashEditCall(t, path, prefixFor(t, content, 2), prefixFor(t, content, 3), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, path); got != "one\nfour\n" {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(res.Content, "Deleted lines 2-3") {
		t.Fatalf("want Deleted variant, got %q", res.Content)
	}
}

func TestHashEdit_MissingNewStringKeyErrors(t *testing.T) {
	content := "one\ntwo\n"
	path := writeTestFile(t, content)
	_, err := hashEditCall(t, path, prefixFor(t, content, 1), "", nil)
	if err == nil || !strings.Contains(err.Error(), "new_string is required") {
		t.Fatalf("missing new_string must error, not delete; got %v", err)
	}
	if got := readBack(t, path); got != content {
		t.Fatal("file must be untouched when new_string is missing")
	}
}

func TestHashEdit_CRLFPreserved(t *testing.T) {
	content := "one\r\ntwo\r\nthree\r\n"
	path := writeTestFile(t, content)
	// LF in new_string conforms to CRLF; the appended trailing newline must be CRLF too.
	if _, err := hashEditCall(t, path, prefixFor(t, content, 2), "", "2a\n2b"); err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, path); got != "one\r\n2a\r\n2b\r\nthree\r\n" {
		t.Fatalf("got %q", got)
	}
}

func TestHashEdit_EmptyFileErrors(t *testing.T) {
	path := writeTestFile(t, "")
	_, err := hashEditCall(t, path, "0000:abcdef", "", "x")
	if err == nil || !strings.Contains(err.Error(), "write_file") {
		t.Fatalf("empty file must point at write_file, got %v", err)
	}
}

func TestHashEdit_EndHashWithoutStartHash(t *testing.T) {
	content := "one\ntwo\n"
	path := writeTestFile(t, content)
	_, err := EditFileHandler(context.Background(), models.ToolCall{
		ID: "he", Name: "edit_file",
		Arguments: map[string]any{"path": path, "end_hash": prefixFor(t, content, 2), "new_string": "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "end_hash without start_hash") {
		t.Fatalf("want explicit end_hash-without-start_hash error, got %v", err)
	}
}

func TestHashEdit_ReplaceAllAndOldStringIgnored(t *testing.T) {
	content := "dup\nx\ndup\n"
	path := writeTestFile(t, content)
	_, err := EditFileHandler(context.Background(), models.ToolCall{
		ID: "he", Name: "edit_file",
		Arguments: map[string]any{
			"path":        path,
			"start_hash":  prefixFor(t, content, 2),
			"new_string":  "X",
			"old_string":  "this is not in the file",
			"replace_all": true,
		},
	})
	if err != nil {
		t.Fatalf("hash mode must ignore old_string/replace_all: %v", err)
	}
	if got := readBack(t, path); got != "dup\nX\ndup\n" {
		t.Fatalf("got %q (replace_all must not fan out a hash edit)", got)
	}
}

func TestHashEdit_InvalidRefRejected(t *testing.T) {
	content := "one\ntwo\n"
	path := writeTestFile(t, content)
	for _, bad := range []string{"12", "xyzxyz", "a3f2b", "12:", ":a3f2b1", "12:A3F2B1"} {
		if _, err := hashEditCall(t, path, bad, "", "x"); err == nil {
			t.Errorf("ref %q must be rejected", bad)
		}
	}
	if got := readBack(t, path); got != content {
		t.Fatal("file must be untouched by invalid refs")
	}
}

func TestHashEdit_PermissionBitsPreserved(t *testing.T) {
	content := "#!/bin/sh\necho hi\n"
	path := writeTestFile(t, content)
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := hashEditCall(t, path, prefixFor(t, content, 2), "", "echo bye"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("perm = %v, want 0755", info.Mode().Perm())
	}
}

// --- strip / looksLineNumbered interplay with the new prefix (D3) ---

func TestEditFile_HashlinePrefixPastedIntoOldString(t *testing.T) {
	content := "alpha\nbeta\ngamma\n"
	path := writeTestFile(t, content)
	// The model pastes read_file's hashline output back verbatim; strip must
	// remove the whole N:hhhhhh<TAB> prefix and match the body.
	oldS := numberedRef(2, "beta") + "\tbeta"
	if _, err := editCall(t, path, oldS, "BETA", false); err != nil {
		t.Fatalf("hashline prefix in old_string must strip: %v", err)
	}
	if got := readBack(t, path); got != "alpha\nBETA\ngamma\n" {
		t.Fatalf("got %q", got)
	}
}

func TestEditFile_HashlineNewStringWithGapNeverWritesPrefixes(t *testing.T) {
	content := "a\nb\nc\nd\n"
	path := writeTestFile(t, content)
	lines := splitFileLines(content)
	pref := func(n int) string { return numberedRef(n, lines[n-1]) + "\t" + lines[n-1] }
	// old_string strips cleanly; new_string drops line 3 so its numbering
	// jumps and cannot strip. looksLineNumbered must recognize the hashline
	// prefix and skip the candidate — never write "2:xxxxxx<TAB>b" into the file.
	oldS := pref(1) + "\n" + pref(2) + "\n" + pref(3) + "\n" + pref(4)
	newS := pref(1) + "\n" + pref(2) + "\n" + pref(4)
	if _, err := editCall(t, path, oldS, newS, false); err == nil {
		t.Fatal("gap-numbered hashline new_string must fail, not write prefixes")
	}
	if got := readBack(t, path); got != content {
		t.Fatalf("file corrupted: %q", got)
	}
}

func TestSplitLineNumberPrefix_HashlineForm(t *testing.T) {
	num, rest, ok := splitLineNumberPrefix("  12:a3f2b1\tfunc Foo() {")
	if !ok || num != 12 || rest != "func Foo() {" {
		t.Fatalf("got %d %q %v", num, rest, ok)
	}
	// Wrong hex width or missing TAB must not parse as a prefix.
	for _, bad := range []string{"12:a3f2b\tx", "12:a3f2b12\tx", "12:a3f2b1 x", "12:A3F2B1\tx"} {
		if _, _, ok := splitLineNumberPrefix(bad); ok {
			t.Errorf("%q must not parse as a line-number prefix", bad)
		}
	}
}

// --- read side ---

func TestReadFile_RawSpanHasNoHash(t *testing.T) {
	content := "a\nb\nc\n"
	path := writeTestFile(t, content)
	res, err := ReadFileHandler(context.Background(), models.ToolCall{
		ID: "r", Name: "read_file",
		Arguments: map[string]any{
			"path": path, "start_line": float64(1), "end_line": float64(2), "line_numbers": false,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "a\nb\n" {
		t.Fatalf("raw span must stay hash-free, got %q", res.Content)
	}
}

func TestReadFile_OutlineHeadTailCarryHashes(t *testing.T) {
	old := ReadFileOutlineThreshold
	ReadFileOutlineThreshold = 10
	defer func() { ReadFileOutlineThreshold = old }()

	var b strings.Builder
	for i := 0; i < 80; i++ {
		b.WriteString("var x = 1\n")
	}
	path := writeTestFile(t, b.String())
	goPath := strings.TrimSuffix(path, ".txt") + ".go"
	if err := os.Rename(path, goPath); err != nil {
		t.Fatal(err)
	}
	res, err := ReadFileHandler(context.Background(), models.ToolCall{
		ID: "r", Name: "read_file", Arguments: map[string]any{"path": goPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantLine := numberedRef(1, "var x = 1")
	if !strings.Contains(res.Content, wantLine+"\tvar x = 1") {
		t.Fatalf("outline head must carry hashline prefixes, got %q", res.Content[:120])
	}
}
