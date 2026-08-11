package docx

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// applyToFixtureRun scans the fixture, patches run runIdx (0-based) of the
// paragraph whose visible text is wantPara, and returns the original and
// patched XML.
func applyToFixtureRun(t *testing.T, wantPara string, runIdx int, newText string) ([]byte, []byte, Run) {
	t.Helper()
	pkg, err := Open(fixture)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	doc, _ := pkg.Part(DocumentPart)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	p, ok := findPara(paras, wantPara)
	if !ok {
		t.Fatalf("paragraph %q not found", wantPara)
	}
	if runIdx >= len(p.Runs) {
		t.Fatalf("paragraph %q has %d runs, want index %d", wantPara, len(p.Runs), runIdx)
	}
	target := p.Runs[runIdx]
	out, err := Apply(doc, []Patch{PatchRun(doc, target, newText)})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return doc, out, target
}

// applyToFixture patches the first run of the named paragraph.
func applyToFixture(t *testing.T, wantPara, newText string) ([]byte, []byte) {
	t.Helper()
	doc, out, _ := applyToFixtureRun(t, wantPara, 0, newText)
	return doc, out
}

func TestApply_ReplacesOnlyTargetRun(t *testing.T) {
	doc, out := applyToFixture(t, "Hello bold world", "Howdy ")
	if !strings.Contains(string(out), "<w:t>Howdy </w:t>") &&
		!strings.Contains(string(out), `<w:t xml:space="preserve">Howdy </w:t>`) {
		t.Errorf("replacement not found in output")
	}
	// The other two runs of the same paragraph must survive untouched.
	for _, want := range []string{"bold", " world"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("sibling run %q was lost", want)
		}
	}
	if len(out) == len(doc) && string(out) == string(doc) {
		t.Error("output is identical to input, patch did not apply")
	}
}

// TestApply_EscapesNewText is the guard for the most common corruption
// cause: unescaped & or < produces XML Word refuses to open.
func TestApply_EscapesNewText(t *testing.T) {
	_, out := applyToFixture(t, "Hello bold world", "A & B < C")
	s := string(out)
	if !strings.Contains(s, "A &amp; B &lt; C") {
		t.Errorf("new text was not XML-escaped; output lacks the escaped form")
	}
	if strings.Contains(s, "A & B < C") {
		t.Errorf("raw unescaped text leaked into the document")
	}
}

// TestApply_RewritesEntityRunFromDecodedText pins the find rule: the caller
// works on decoded text and the whole content span is rewritten, so no
// character-to-byte mapping is ever needed.
func TestApply_RewritesEntityRunFromDecodedText(t *testing.T) {
	_, out := applyToFixture(t, "Tom & Jerry <fast>", "Tom & Jerry <slow>")
	s := string(out)
	if !strings.Contains(s, "Tom &amp; Jerry &lt;slow&gt;") {
		t.Errorf("rewritten entity run not found in escaped form")
	}
	if strings.Contains(s, "&lt;fast&gt;") {
		t.Errorf("old content survived the patch")
	}
}

// TestApply_AddsPreserveWhenNewTextHasEdgeWhitespace targets the "bold" run
// specifically: python-docx already emits xml:space="preserve" on the runs
// whose text has edge whitespace ("Hello " and " world"), so patching those
// would pass without ever exercising the attribute-adding path. "bold" has
// no such attribute, which is exactly what makes it the right target.
func TestApply_AddsPreserveWhenNewTextHasEdgeWhitespace(t *testing.T) {
	_, out, target := applyToFixtureRun(t, "Hello bold world", 1, "  spaced out  ")
	if target.Text != "bold" {
		t.Fatalf("target run text = %q, want %q — fixture layout changed", target.Text, "bold")
	}
	if target.HasPreserve {
		t.Fatal("target run already has xml:space=preserve; this test would pass vacuously")
	}
	if !strings.Contains(string(out), `<w:t xml:space="preserve">  spaced out  </w:t>`) {
		t.Error("xml:space=preserve was not added to the patched <w:t>")
	}
}

// TestApply_LeavesTagAloneWhenNoEdgeWhitespace is the negative half: a run
// without the attribute must not gain one when it does not need it.
func TestApply_LeavesTagAloneWhenNoEdgeWhitespace(t *testing.T) {
	_, out, target := applyToFixtureRun(t, "Hello bold world", 1, "italic")
	if target.HasPreserve {
		t.Fatal("target run already has xml:space=preserve; this test would pass vacuously")
	}
	if !strings.Contains(string(out), "<w:t>italic</w:t>") {
		t.Error("patched tag should stay bare when the text has no edge whitespace")
	}
}

func TestApply_KeepsExistingPreserveWithoutDuplicating(t *testing.T) {
	_, out := applyToFixture(t, " padded text ", " still padded ")
	s := string(out)
	if strings.Contains(s, `xml:space="preserve" xml:space="preserve"`) {
		t.Error("xml:space=preserve was duplicated")
	}
	if !strings.Contains(s, " still padded ") {
		t.Error("replacement text not found")
	}
}

// TestApply_DescendingOrderKeepsOffsetsValid pins the Global Constraint that
// patches are applied in descending offset order so earlier splices never
// invalidate the byte offsets of patches still waiting to be applied.
//
// A prior version of this test asserted only Contains("AAAA"),
// Contains("ZZZZ"), and Contains("bold"), which is too weak to pin the rule:
// a mutant that reverses only the APPLY loop (leaving the sort and overlap
// check intact) still produces output containing all three substrings —
// they just land in the wrong place, e.g.
// `<w:t xml:space="preserve"> wZZZZw:t></w:r>`, which corrupts the tag
// structure ("element <t> closed by </r>") while still literally containing
// "ZZZZ" and "bold" as substrings. assertWellFormed catches that corruption
// directly, and the exact-string check on the whole patched paragraph region
// pins that both patches landed in exactly the right place relative to the
// untouched middle run and to each run's own tag.
func TestApply_DescendingOrderKeepsOffsetsValid(t *testing.T) {
	pkg, err := Open(fixture)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	doc, _ := pkg.Part(DocumentPart)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	p, ok := findPara(paras, "Hello bold world")
	if !ok {
		t.Fatal("multi-run paragraph not found")
	}
	// Patch runs 1 and 3, deliberately passed in ascending order so the
	// implementation must sort them itself.
	out, err := Apply(doc, []Patch{
		PatchRun(doc, p.Runs[0], "AAAA"),
		PatchRun(doc, p.Runs[2], "ZZZZ"),
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertWellFormed(t, out)

	s := string(out)
	if !strings.Contains(s, "AAAA") || !strings.Contains(s, "ZZZZ") {
		t.Errorf("both patches should be present")
	}
	if !strings.Contains(s, "bold") {
		t.Errorf("untouched middle run was damaged")
	}

	// Exact-string pin of the whole patched paragraph: first and third runs
	// replaced in place, untouched middle run byte-for-byte intact, and both
	// <w:t> tags kept their original xml:space="preserve" attribute (since
	// "AAAA" and "ZZZZ" have no edge whitespace, needsPreserve is false, but
	// HasPreserve was already true on both runs in the fixture, so Apply
	// must leave the attribute in place rather than only adding it).
	const wantParagraph = `<w:p><w:r><w:t xml:space="preserve">AAAA</w:t></w:r>` +
		`<w:r><w:rPr><w:b/></w:rPr><w:t>bold</w:t></w:r>` +
		`<w:r><w:t xml:space="preserve">ZZZZ</w:t></w:r></w:p>`
	if !strings.Contains(s, wantParagraph) {
		t.Errorf("patched paragraph region = missing exact expected form; want it to contain:\n%s", wantParagraph)
	}
}

// TestApply_RejectsOutOfRangeTagSpan pins I2: Content is validated, but
// TagSpan was not, and TagSpan is sliced unconditionally whenever the
// replacement text needs xml:space="preserve" added (edge whitespace) and
// the run does not already have it. A hand-built (or stale/foreign, see the
// Old-context check) Patch with a TagSpan pointing past the end of the
// document must produce an error, not a panic that would crash the whole
// process — Registry.executeWithSandbox calls handlers bare, so nothing in
// the tool path recovers from this.
func TestApply_RejectsOutOfRangeTagSpan(t *testing.T) {
	// 38-byte document, mirroring the reviewer's repro.
	doc := []byte(`<w:p><w:r><w:t>hello world</w:t></w:r></w:p>`)[:38]
	p := Patch{
		Content: Span{Start: 15, End: 20},
		TagSpan: Span{Start: 900, End: 910},
		NewText: " pad ", // edge whitespace triggers the TagSpan rewrite path
	}
	_, err := Apply(doc, []Patch{p})
	if err == nil {
		t.Fatal("Apply accepted an out-of-range TagSpan, want error")
	}
	if !strings.Contains(err.Error(), "tag span") && !strings.Contains(err.Error(), "TagSpan") {
		t.Errorf("error = %q, want it to mention the tag span", err)
	}
}

// TestApply_RejectsTagSpanOverlappingContent pins the second half of I2's
// fix: TagSpan must end at or before Content.Start, since the start tag
// always sits before its own content. A TagSpan that reaches into or past
// Content is nonsensical and must be rejected rather than silently
// corrupting the splice.
func TestApply_RejectsTagSpanOverlappingContent(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:t>hello world</w:t></w:r></w:p>`)
	p := Patch{
		Content: Span{Start: 15, End: 20},
		TagSpan: Span{Start: 10, End: 16}, // ends inside Content
		NewText: " pad ",
	}
	_, err := Apply(doc, []Patch{p})
	if err == nil {
		t.Fatal("Apply accepted a TagSpan overlapping Content, want error")
	}
}

func TestApply_RejectsOverlappingPatches(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:t>hello</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	r := paras[0].Runs[0]
	_, err = Apply(doc, []Patch{PatchRun(doc, r, "a"), PatchRun(doc, r, "b")})
	if err == nil {
		t.Fatal("Apply accepted overlapping patches, want error")
	}
	if !strings.Contains(err.Error(), "overlap") {
		t.Errorf("error = %q, want it to mention overlap", err)
	}
}

// TestApply_RejectsTwoPatchesAtTheSameEmptySpan pins I3: the overlap guard
// is `ordered[i-1].Content.Start < p.Content.End`, which is false whenever
// two patches share the same EMPTY span (Content.Start == Content.End, as
// P1b's delete op produces for an emptied <w:t></w:t> run) — 15 < 15 is
// false, so both patches silently apply at the same offset instead of being
// rejected as overlapping. Which one ends up first in the output then
// depends on sort.Slice's unspecified tie-break order, not on the caller's
// intent.
func TestApply_RejectsTwoPatchesAtTheSameEmptySpan(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:t></w:t></w:r></w:p>`)
	empty := Span{Start: 15, End: 15}
	if string(doc[empty.Start:empty.End]) != "" {
		t.Fatalf("fixture span %v is not empty in %q; test setup is wrong", empty, doc)
	}
	_, err := Apply(doc, []Patch{
		{Content: empty, TagSpan: Span{Start: 10, End: 15}, NewText: "AAA"},
		{Content: empty, TagSpan: Span{Start: 10, End: 15}, NewText: "BBB"},
	})
	if err == nil {
		t.Fatal("Apply accepted two patches at the same empty span, want error")
	}
}

// TestApply_RejectsStaleOldSnapshot pins I4: PatchRun snapshots the raw
// content bytes into Patch.Old at scan time, and Apply must verify that
// snapshot still matches before splicing. Without the check, a span scanned
// from an earlier version of the document (or, equally, a foreign document
// entirely) applies with a nil error and silently splices at whatever bytes
// now sit at that offset — only caught by luck if the target happens to be
// shorter than expected.
func TestApply_RejectsStaleOldSnapshot(t *testing.T) {
	pkg, err := Open(fixture)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	doc, _ := pkg.Part(DocumentPart)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	p, ok := findPara(paras, "Hello bold world")
	if !ok {
		t.Fatal("target paragraph not found")
	}
	target := p.Runs[0]

	patch := PatchRun(doc, target, "Howdy ")
	// The document changed underneath the scanned Run (e.g. a prior edit in
	// the same batch, or a re-open between scan and apply): the same offsets
	// now cover different bytes than what PatchRun snapshotted.
	staleDoc := append([]byte(nil), doc...)
	copy(staleDoc[target.Content.Start:target.Content.End], []byte(strings.Repeat("X", target.Content.End-target.Content.Start)))

	_, err = Apply(staleDoc, []Patch{patch})
	if err == nil {
		t.Fatal("Apply accepted a patch whose Old snapshot no longer matches the document, want error")
	}
}

// TestApply_NilOldSkipsTheCheck pins the escape hatch: hand-constructed
// patches (as used throughout this file, and by any future caller not going
// through PatchRun) must keep working without fabricating a matching Old.
func TestApply_NilOldSkipsTheCheck(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:t>hello</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	r := paras[0].Runs[0]
	p := Patch{Content: r.Content, TagSpan: r.Start, NewText: "bye"} // Old left nil
	out, err := Apply(doc, []Patch{p})
	if err != nil {
		t.Fatalf("Apply with nil Old: %v", err)
	}
	if !strings.Contains(string(out), "bye") {
		t.Error("patch did not apply")
	}
}

// TestApply_RejectsPatchOnSelfClosingRun pins I7: for <w:t/>, Content is a
// zero-length span sitting right after the "/>", outside any <w:t> element.
// Splicing replacement text there appends character data directly inside
// <w:r>, which is not in the content model — Word reports unreadable
// content, and worse, err was nil. Apply must refuse this with a clear
// error instead of producing invalid OOXML silently.
func TestApply_RejectsPatchOnSelfClosingRun(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:t/></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	r := paras[0].Runs[0]
	if !r.SelfClosing {
		t.Fatal("fixture run is not self-closing; test setup is wrong")
	}
	patch := PatchRun(doc, r, "hello")

	out, err := Apply(doc, []Patch{patch})
	if err == nil {
		t.Fatalf("Apply accepted a patch on a self-closing <w:t/> run, want error; got %q", out)
	}
}

func TestApply_NoPatchesReturnsInputUnchanged(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:t>hello</w:t></w:r></w:p>`)
	out, err := Apply(doc, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if string(out) != string(doc) {
		t.Errorf("output = %q, want the input unchanged", out)
	}
}

// buildBenchDoc synthesizes a document.xml with n one-run paragraphs, sized
// to land in the same ballpark (hundreds of KB for n in the low thousands)
// the design doc's real documents and edit-batch sizes imply.
func buildBenchDoc(n int) []byte {
	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-8"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&buf, `<w:p><w:r><w:t>run number %06d with some representative padding text so each paragraph is a realistic size</w:t></w:r></w:p>`, i)
	}
	buf.WriteString(`</w:body></w:document>`)
	return buf.Bytes()
}

// BenchmarkApply pins I8: Apply must run in O(document_bytes + patch_count),
// not O(document_bytes * patch_count). spliceBytes reallocates the whole
// document on every patch (and again on every preserve-attribute rewrite),
// so before the fix this benchmark's allocated bytes scale with
// patches * document size; after the fix it should scale with roughly
// document size + total patch text, independent of patch count.
func BenchmarkApply(b *testing.B) {
	const n = 2000
	doc := buildBenchDoc(n)
	paras, err := Scan(doc)
	if err != nil {
		b.Fatalf("Scan: %v", err)
	}
	patches := make([]Patch, 0, n)
	for _, p := range paras {
		for _, r := range p.Runs {
			patches = append(patches, PatchRun(doc, r, "patched run text"))
		}
	}
	if len(patches) != n {
		b.Fatalf("built %d patches, want %d", len(patches), n)
	}
	b.Logf("document size: %d bytes, patches: %d", len(doc), len(patches))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Apply(doc, patches); err != nil {
			b.Fatalf("Apply: %v", err)
		}
	}
}

// TestApply_RawPatchIsNotEscaped pins §4.2's paragraph-level insert: a whole
// <w:p> subtree must land verbatim, not as &lt;w:p&gt;.
func TestApply_RawPatchIsNotEscaped(t *testing.T) {
	doc := []byte(`<w:body><w:p><w:r><w:t>one</w:t></w:r></w:p></w:body>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	newPara := `<w:p><w:r><w:t>two</w:t></w:r></w:p>`
	at := Span{Start: paras[0].Span.End, End: paras[0].Span.End}
	out, err := Apply(doc, []Patch{PatchRawSpan(doc, at, newPara)})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if strings.Contains(string(out), "&lt;w:p&gt;") {
		t.Fatalf("raw XML was escaped: %s", out)
	}
	got, err := Scan(out)
	if err != nil {
		t.Fatalf("Scan(out): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d paragraphs after insert, want 2", len(got))
	}
	if paraText(got[1]) != "two" {
		t.Errorf("inserted paragraph text = %q, want %q", paraText(got[1]), "two")
	}
}

// TestApply_RawPatchCanDeleteAParagraph pins §4.2's paragraph-level delete.
func TestApply_RawPatchCanDeleteAParagraph(t *testing.T) {
	doc := []byte(`<w:body><w:p><w:r><w:t>one</w:t></w:r></w:p><w:p><w:r><w:t>two</w:t></w:r></w:p></w:body>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	out, err := Apply(doc, []Patch{PatchRawSpan(doc, paras[0].Span, "")})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := Scan(out)
	if err != nil {
		t.Fatalf("Scan(out): %v", err)
	}
	if len(got) != 1 || paraText(got[0]) != "two" {
		t.Fatalf("after delete got %d paragraphs, first = %q; want 1 / %q", len(got), paraText(got[0]), "two")
	}
}

// TestApply_RawPatchRejectsMalformedResult guards the one escaping bypass in
// the package: a raw patch that produces invalid XML must be refused, not
// written out for Word to choke on.
func TestApply_RawPatchRejectsMalformedResult(t *testing.T) {
	doc := []byte(`<w:body><w:p><w:r><w:t>one</w:t></w:r></w:p></w:body>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	at := Span{Start: paras[0].Span.End, End: paras[0].Span.End}
	_, err = Apply(doc, []Patch{PatchRawSpan(doc, at, `<w:p><w:r>`)})
	if err == nil {
		t.Fatal("Apply accepted a raw patch producing malformed XML, want error")
	}
	if !strings.Contains(err.Error(), "well-formed") {
		t.Errorf("error = %q, want it to mention well-formedness", err)
	}
}

// TestApplyToPart_ReadModifyWriteAvoidsLostUpdate pins the fix for the stale
// alias hazard: two sequential batches must both survive.
func TestApplyToPart_ReadModifyWriteAvoidsLostUpdate(t *testing.T) {
	pkg, err := Open(fixture)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	doc, _ := pkg.Part(DocumentPart)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	p, ok := findPara(paras, "Hello bold world")
	if !ok {
		t.Fatal("target paragraph not found")
	}
	if err := pkg.ApplyToPart(DocumentPart, []Patch{PatchRun(doc, p.Runs[0], "Howdy ")}); err != nil {
		t.Fatalf("ApplyToPart batch 1: %v", err)
	}

	// Re-read and re-scan, as P1b must after every write-back (design §5.4).
	doc2, _ := pkg.Part(DocumentPart)
	paras2, err := Scan(doc2)
	if err != nil {
		t.Fatalf("Scan after batch 1: %v", err)
	}
	p2, ok := findPara(paras2, "Howdy bold world")
	if !ok {
		t.Fatalf("batch 1 was lost; paragraph text is %q", paraText(paras2[p.Index-1]))
	}
	if err := pkg.ApplyToPart(DocumentPart, []Patch{PatchRun(doc2, p2.Runs[2], " planet")}); err != nil {
		t.Fatalf("ApplyToPart batch 2: %v", err)
	}

	doc3, _ := pkg.Part(DocumentPart)
	paras3, err := Scan(doc3)
	if err != nil {
		t.Fatalf("Scan after batch 2: %v", err)
	}
	if _, ok := findPara(paras3, "Howdy bold planet"); !ok {
		t.Errorf("both batches should survive; got %q", paraText(paras3[p.Index-1]))
	}
}

// TestApplyToPart_UnknownPartErrors keeps ApplyToPart honest about the same
// contract SetPart enforces.
func TestApplyToPart_UnknownPartErrors(t *testing.T) {
	pkg, err := Open(fixture)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := pkg.ApplyToPart("word/nope.xml", nil); err == nil {
		t.Fatal("ApplyToPart on an unknown part returned nil, want error")
	}
}
