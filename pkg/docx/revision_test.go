package docx

import (
	"strings"
	"testing"
	"time"
)

// fixedClock returns a Now func pinned to a single instant, so tests can
// assert exact bytes for w:date instead of merely "looks like a timestamp".
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

var testNow = fixedClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

func TestRevision_IDsStartAboveExistingMax(t *testing.T) {
	doc := []byte(`<w:body><w:ins w:id="7"/><w:del w:id="42"/></w:body>`)
	rc := newRevisionCtx(doc, "A", testNow)
	a := rc.attrs()
	if !strings.Contains(a, `w:id="43"`) {
		t.Errorf("attrs = %q, want the first id to be 43 (one past the existing max)", a)
	}
}

// TestRevision_IDStartsAtOneWhenNoExistingIDs proves the "no existing w:id"
// branch specifically: without this, a document with zero revisions could
// accidentally rely on the same code path as "some non-zero max was found"
// and the test suite would never have exercised nextID's default.
func TestRevision_IDStartsAtOneWhenNoExistingIDs(t *testing.T) {
	doc := []byte(`<w:body><w:p><w:r><w:t>plain</w:t></w:r></w:p></w:body>`)
	rc := newRevisionCtx(doc, "A", testNow)
	a := rc.attrs()
	if !strings.Contains(a, `w:id="1"`) {
		t.Errorf("attrs = %q, want the first id to be 1 when the document has no existing w:id", a)
	}
}

// TestRevision_IDScanCoversNonRevisionElements proves the id scan looks at
// w:id on ANY element, not just <w:ins>/<w:del>: Word requires every w:id
// in a document to be globally unique regardless of which element carries
// it, so a bookmark's id is just as much a collision risk as an existing
// revision's.
func TestRevision_IDScanCoversNonRevisionElements(t *testing.T) {
	doc := []byte(`<w:body><w:bookmarkStart w:id="5" w:name="x"/></w:body>`)
	rc := newRevisionCtx(doc, "A", testNow)
	a := rc.attrs()
	if !strings.Contains(a, `w:id="6"`) {
		t.Errorf("attrs = %q, want 6 (one past the bookmarkStart's w:id=5)", a)
	}
}

// TestRevision_IDScanIgnoresUnparseableID proves a w:id value that LOOKS
// numeric but overflows int (so strconv.Atoi returns an error alongside a
// clamped, non-zero value — unlike a plain non-numeric string, which
// Atoi reports as 0) does not get treated as a legitimate, enormous
// existing id. Without the error check, nextID would balloon to whatever
// clamped value Atoi returned instead of one past the real max (5).
func TestRevision_IDScanIgnoresUnparseableID(t *testing.T) {
	doc := []byte(`<w:body><w:bookmarkStart w:id="5" w:name="x"/><w:tbl w:id="99999999999999999999999999"/></w:body>`)
	rc := newRevisionCtx(doc, "A", testNow)
	a := rc.attrs()
	if !strings.Contains(a, `w:id="6"`) {
		t.Errorf("attrs = %q, want 6 (one past the real max of 5, ignoring the unparseable overflow id)", a)
	}
}

func TestRevision_IDsAreUniqueAcrossCalls(t *testing.T) {
	rc := newRevisionCtx([]byte(`<w:body/>`), "A", testNow)
	a1 := rc.attrs()
	a2 := rc.attrs()
	a3 := rc.attrs()
	if a1 == a2 || a2 == a3 || a1 == a3 {
		t.Errorf("three calls to attrs() produced non-unique output: %q, %q, %q", a1, a2, a3)
	}
	if !strings.Contains(a1, `w:id="1"`) || !strings.Contains(a2, `w:id="2"`) || !strings.Contains(a3, `w:id="3"`) {
		t.Errorf("ids did not increment 1,2,3: %q, %q, %q", a1, a2, a3)
	}
}

func TestRevision_ClockIsInjectable(t *testing.T) {
	rc1 := newRevisionCtx([]byte(`<w:body/>`), "A", testNow)
	rc2 := newRevisionCtx([]byte(`<w:body/>`), "A", testNow)
	a1 := rc1.attrs()
	a2 := rc2.attrs()
	if a1 != a2 {
		t.Errorf("two revisionCtx built with the same injected clock produced different bytes: %q vs %q", a1, a2)
	}
	if !strings.Contains(a1, `w:date="2026-01-01T00:00:00Z"`) {
		t.Errorf("attrs = %q, want the injected date to appear verbatim", a1)
	}
}

// TestRevision_DefaultAuthorWhenEmpty pins the "" -> "deepai" default the
// plan calls out for EditOptions.Author, applied centrally here so Task 2
// does not have to duplicate the fallback.
func TestRevision_DefaultAuthorWhenEmpty(t *testing.T) {
	rc := newRevisionCtx([]byte(`<w:body/>`), "", testNow)
	a := rc.attrs()
	if !strings.Contains(a, `w:author="deepai"`) {
		t.Errorf("attrs = %q, want w:author=\"deepai\" as the default", a)
	}
}

// Most likely to be gotten wrong: cloning must preserve <w:rPr> formatting.
func TestRevision_CloneKeepsRunProperties(t *testing.T) {
	run := []byte(`<w:r><w:rPr><w:b/><w:color w:val="FF0000"/></w:rPr><w:t>old</w:t></w:r>`)
	got, err := cloneRunWithText(run, "new", false)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "<w:b/>") || !strings.Contains(s, `w:val="FF0000"`) {
		t.Errorf("run properties were lost: %s", s)
	}
	if !strings.Contains(s, "<w:t>new</w:t>") {
		t.Errorf("text not replaced: %s", s)
	}
	if strings.Contains(s, "old") {
		t.Errorf("old text survived: %s", s)
	}
}

func TestRevision_DelTextConversion(t *testing.T) {
	run := []byte(`<w:r><w:t xml:space="preserve"> gone </w:t></w:r>`)
	got, err := cloneRunWithText(run, " gone ", true)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "<w:delText") {
		t.Errorf("w:t was not converted to w:delText: %s", s)
	}
	if strings.Contains(s, "<w:t ") || strings.Contains(s, "<w:t>") {
		t.Errorf("a w:t survived: %s", s)
	}
	if !strings.Contains(s, `xml:space="preserve"`) {
		t.Error("xml:space was dropped; Word would eat the spaces")
	}
}

// TestRevision_AddsPreserveWhenNewTextNeedsIt proves xml:space is added even
// when the ORIGINAL run's <w:t> did not carry it, but the newText being
// written in does have leading/trailing whitespace.
func TestRevision_AddsPreserveWhenNewTextNeedsIt(t *testing.T) {
	run := []byte(`<w:r><w:t>old</w:t></w:r>`)
	got, err := cloneRunWithText(run, "  new  ", false)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, `xml:space="preserve"`) {
		t.Errorf("new text has leading/trailing spaces but xml:space was not added: %s", s)
	}
}

func TestRevision_EscapesNewText(t *testing.T) {
	run := []byte(`<w:r><w:t>old</w:t></w:r>`)
	got, err := cloneRunWithText(run, "A & B", false)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "A &amp; B") {
		t.Errorf("newText was not escaped: %s", s)
	}
	if strings.Contains(s, "A & B</w:t>") {
		t.Errorf("raw unescaped '&' leaked into output: %s", s)
	}
}

// TestRevision_CloneRunWithNoTextNode_EmptyTextIsANoop covers a run holding
// only a <w:br/> (no <w:t> at all): with newText == "", there is nothing to
// replace, so the clone must come back byte-identical, formatting (and the
// break) intact.
func TestRevision_CloneRunWithNoTextNode_EmptyTextIsANoop(t *testing.T) {
	run := []byte(`<w:r><w:rPr><w:b/></w:rPr><w:br/></w:r>`)
	got, err := cloneRunWithText(run, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(run) {
		t.Errorf("clone of a no-text run with newText=\"\" changed bytes: got %s, want unchanged %s", got, run)
	}
}

// TestRevision_CloneRunWithNoTextNode_NonEmptyTextIsRefused covers the same
// shape but with actual text to insert: there is no defined place to put
// it (before or after the break?), so this must be a loud error, not a
// guess.
func TestRevision_CloneRunWithNoTextNode_NonEmptyTextIsRefused(t *testing.T) {
	run := []byte(`<w:r><w:br/></w:r>`)
	_, err := cloneRunWithText(run, "new text", false)
	if err == nil {
		t.Fatal("want an error for a non-empty newText on a run with no <w:t>/<w:delText>, got nil")
	}
}

// TestRevision_CloneRunWithSeveralTextNodesIsRefused covers the shape that
// caused a silent text-loss defect earlier in this codebase (a single <w:r>
// producing more than one <w:t>, e.g. split by a <w:br/>): guessing which
// node keeps newText and which gets emptied would silently drop text, so
// this must refuse rather than guess.
func TestRevision_CloneRunWithSeveralTextNodesIsRefused(t *testing.T) {
	run := []byte(`<w:r><w:t>flat</w:t><w:br/><w:t>b</w:t></w:r>`)
	_, err := cloneRunWithText(run, "new", false)
	if err == nil {
		t.Fatal("want an error for a run with more than one text node, got nil")
	}
}

// TestRevision_CloneRunHandlesSelfClosingText covers a self-closing
// <w:t/> (zero-length text): it still counts as ONE text node, so
// newText must land inside a rewritten, non-self-closing pair.
func TestRevision_CloneRunHandlesSelfClosingText(t *testing.T) {
	run := []byte(`<w:r><w:t/></w:r>`)
	got, err := cloneRunWithText(run, "now has text", false)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "<w:t>now has text</w:t>") {
		t.Errorf("self-closing <w:t/> was not converted to hold newText: %s", s)
	}
}

func TestRevision_WrapDel(t *testing.T) {
	rc := newRevisionCtx([]byte(`<w:body/>`), "tester", testNow)
	run := []byte(`<w:r><w:rPr><w:b/></w:rPr><w:t>removed</w:t></w:r>`)
	got, err := rc.wrapDel(run, "removed")
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.HasPrefix(s, "<w:del ") {
		t.Errorf("wrapDel output does not start with <w:del: %s", s)
	}
	if !strings.Contains(s, `w:id="1"`) || !strings.Contains(s, `w:author="tester"`) || !strings.Contains(s, `w:date="2026-01-01T00:00:00Z"`) {
		t.Errorf("wrapDel missing expected attrs: %s", s)
	}
	if !strings.Contains(s, "<w:delText>removed</w:delText>") {
		t.Errorf("wrapDel did not convert text to delText: %s", s)
	}
	if !strings.Contains(s, "<w:b/>") {
		t.Errorf("wrapDel lost run formatting: %s", s)
	}
	if !strings.HasSuffix(s, "</w:del>") {
		t.Errorf("wrapDel output does not end with </w:del>: %s", s)
	}
}

func TestRevision_WrapIns(t *testing.T) {
	rc := newRevisionCtx([]byte(`<w:body/>`), "tester", testNow)
	run := []byte(`<w:r><w:rPr><w:i/></w:rPr><w:t>old</w:t></w:r>`)
	got, err := rc.wrapIns(run, "added")
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.HasPrefix(s, "<w:ins ") {
		t.Errorf("wrapIns output does not start with <w:ins: %s", s)
	}
	if !strings.Contains(s, "<w:t>added</w:t>") {
		t.Errorf("wrapIns did not carry the new text as plain w:t: %s", s)
	}
	if strings.Contains(s, "delText") {
		t.Errorf("wrapIns must not produce delText: %s", s)
	}
	if !strings.Contains(s, "<w:i/>") {
		t.Errorf("wrapIns lost run formatting: %s", s)
	}
}

func TestRevision_WrapDelAndWrapInsUseDistinctIDs(t *testing.T) {
	rc := newRevisionCtx([]byte(`<w:body/>`), "tester", testNow)
	run := []byte(`<w:r><w:t>x</w:t></w:r>`)
	del, err := rc.wrapDel(run, "x")
	if err != nil {
		t.Fatal(err)
	}
	ins, err := rc.wrapIns(run, "y")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(del), `w:id="1"`) == strings.Contains(string(ins), `w:id="1"`) {
		t.Errorf("wrapDel and wrapIns did not consume distinct ids: del=%s ins=%s", del, ins)
	}
}

// TestRevision_MarkParagraph_NoExistingPPr covers a paragraph with no
// <w:pPr> at all: markParagraph must create one (as the first child of
// <w:p>, per CT_P's schema) holding the marker.
func TestRevision_MarkParagraph_NoExistingPPr(t *testing.T) {
	rc := newRevisionCtx([]byte(`<w:body/>`), "tester", testNow)
	para := []byte(`<w:p><w:r><w:t>hi</w:t></w:r></w:p>`)
	got, err := rc.markParagraph(para, true)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "<w:pPr><w:rPr><w:ins ") {
		t.Errorf("markParagraph(inserted) did not create <w:pPr><w:rPr><w:ins .../>: %s", s)
	}
	if !strings.Contains(s, "<w:r><w:t>hi</w:t></w:r>") {
		t.Errorf("markParagraph corrupted the paragraph's run content: %s", s)
	}
	if !checkWellFormedOK(t, s) {
		t.Errorf("markParagraph produced non-well-formed XML: %s", s)
	}
}

// TestRevision_MarkParagraph_DeletedUsesDelMarker proves the "inserted"
// bool actually selects between <w:ins/> and <w:del/> rather than always
// emitting the same tag.
func TestRevision_MarkParagraph_DeletedUsesDelMarker(t *testing.T) {
	rc := newRevisionCtx([]byte(`<w:body/>`), "tester", testNow)
	para := []byte(`<w:p><w:r><w:t>hi</w:t></w:r></w:p>`)
	got, err := rc.markParagraph(para, false)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "<w:del ") {
		t.Errorf("markParagraph(false) did not use <w:del/>: %s", s)
	}
	if strings.Contains(s, "<w:ins ") {
		t.Errorf("markParagraph(false) unexpectedly also produced <w:ins/>: %s", s)
	}
}

// TestRevision_MarkParagraph_ExistingPPrNoRPr covers a paragraph whose
// <w:pPr> already exists (with sibling content like <w:pStyle>) but has no
// <w:rPr>: the existing pPr content must survive, and a fresh rPr must be
// added to hold the marker.
func TestRevision_MarkParagraph_ExistingPPrNoRPr(t *testing.T) {
	rc := newRevisionCtx([]byte(`<w:body/>`), "tester", testNow)
	para := []byte(`<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>hi</w:t></w:r></w:p>`)
	got, err := rc.markParagraph(para, true)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, `<w:pStyle w:val="Heading1"/>`) {
		t.Errorf("existing pPr content was lost: %s", s)
	}
	if !strings.Contains(s, "<w:rPr><w:ins ") {
		t.Errorf("did not add a fresh <w:rPr><w:ins .../>: %s", s)
	}
	// The new rPr must land INSIDE the existing pPr, not in a second,
	// separately-created one: exactly one <w:pPr> open tag must appear in
	// the output. Without this check, a defect that falls through to the
	// "no pPr at all" branch (creating a brand new <w:pPr><w:rPr>... right
	// after <w:p>, orphaned from the paragraph's real, pre-existing pPr)
	// would still satisfy the two Contains checks above and pass, even
	// though it leaves the paragraph with two <w:pPr> siblings — invalid
	// per CT_P's schema (pPr is not a repeatable element).
	if n := strings.Count(s, "<w:pPr>"); n != 1 {
		t.Errorf("want exactly one <w:pPr> in the output, got %d: %s", n, s)
	}
	if !checkWellFormedOK(t, s) {
		t.Errorf("markParagraph produced non-well-formed XML: %s", s)
	}
}

// TestRevision_MarkParagraph_ExistingRPrGetsMarkerFirst covers a paragraph
// whose <w:pPr><w:rPr> already exists with sibling properties (e.g. <w:b/>
// on the paragraph mark): the marker must be added as the FIRST child of
// rPr (CT_ParaRPr requires ins/del before rStyle/rFonts/...), and the
// existing properties must survive untouched.
func TestRevision_MarkParagraph_ExistingRPrGetsMarkerFirst(t *testing.T) {
	rc := newRevisionCtx([]byte(`<w:body/>`), "tester", testNow)
	para := []byte(`<w:p><w:pPr><w:rPr><w:b/></w:rPr></w:pPr><w:r><w:t>hi</w:t></w:r></w:p>`)
	got, err := rc.markParagraph(para, true)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "<w:rPr><w:ins ") {
		t.Errorf("marker was not inserted as the first child of the existing rPr: %s", s)
	}
	if !strings.Contains(s, "<w:b/>") {
		t.Errorf("existing rPr content (<w:b/>) was lost: %s", s)
	}
	if !checkWellFormedOK(t, s) {
		t.Errorf("markParagraph produced non-well-formed XML: %s", s)
	}
}

// TestRevision_MarkParagraph_SelfClosingRPr covers a paragraph mark rPr
// written as the common empty self-closing <w:rPr/>: it must become a real
// open/close pair to hold the marker rather than being left dangling.
func TestRevision_MarkParagraph_SelfClosingRPr(t *testing.T) {
	rc := newRevisionCtx([]byte(`<w:body/>`), "tester", testNow)
	para := []byte(`<w:p><w:pPr><w:rPr/></w:pPr><w:r><w:t>hi</w:t></w:r></w:p>`)
	got, err := rc.markParagraph(para, true)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if strings.Contains(s, "<w:rPr/>") {
		t.Errorf("self-closing <w:rPr/> was left dangling instead of becoming a real pair: %s", s)
	}
	if !strings.Contains(s, "<w:rPr><w:ins ") {
		t.Errorf("marker was not placed inside the (now non-self-closing) rPr: %s", s)
	}
	if !checkWellFormedOK(t, s) {
		t.Errorf("markParagraph produced non-well-formed XML: %s", s)
	}
}

// TestRevision_MarkParagraph_SelfClosingPPr covers a paragraph mark pPr
// written as self-closing <w:pPr/> (no rPr, no other properties at all):
// it must gain a real rPr child, not stay self-closing.
func TestRevision_MarkParagraph_SelfClosingPPr(t *testing.T) {
	rc := newRevisionCtx([]byte(`<w:body/>`), "tester", testNow)
	para := []byte(`<w:p><w:pPr/><w:r><w:t>hi</w:t></w:r></w:p>`)
	got, err := rc.markParagraph(para, true)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if strings.Contains(s, "<w:pPr/>") {
		t.Errorf("self-closing <w:pPr/> was left dangling instead of gaining a real rPr child: %s", s)
	}
	if !strings.Contains(s, "<w:pPr><w:rPr><w:ins ") {
		t.Errorf("marker was not placed inside a freshly opened pPr/rPr: %s", s)
	}
	if !checkWellFormedOK(t, s) {
		t.Errorf("markParagraph produced non-well-formed XML: %s", s)
	}
}

// TestRevision_MarkParagraph_SelfClosingParagraphRefused covers the
// degenerate <w:p/> (no content at all, as planParagraphTarget's
// only-paragraph-in-a-table-cell delete carve-out produces): there is no
// content model to insert a marker into, so this must be a loud error
// rather than silently producing an orphaned pPr/rPr outside the paragraph.
func TestRevision_MarkParagraph_SelfClosingParagraphRefused(t *testing.T) {
	rc := newRevisionCtx([]byte(`<w:body/>`), "tester", testNow)
	_, err := rc.markParagraph([]byte(`<w:p/>`), true)
	if err == nil {
		t.Fatal("want an error for a self-closing <w:p/>, got nil")
	}
}

// checkWellFormedOK is a small test helper wrapping this package's own
// checkWellFormed (splice.go) so revision tests can assert their output is
// at least syntactically valid XML, independent of the specific substrings
// they also assert on.
func checkWellFormedOK(t *testing.T, s string) bool {
	t.Helper()
	// markParagraph's output is a <w:p> fragment, not a full document, but
	// checkWellFormed only requires balanced, well-formed tokens - a single
	// rooted fragment satisfies that.
	if err := checkWellFormed([]byte(s)); err != nil {
		t.Logf("checkWellFormed: %v", err)
		return false
	}
	return true
}

// TestHadRevisionsAtOpen_TrueWhenFixtureAlreadyHasRevisions pins the other
// half of hadRevisionsAtOpen's contract: it is not merely "always false
// until something changes" (that alone would let the OpenDocument
// assignment be deleted without any test noticing, since a clean
// document's zero-value bool is already false) — it must become true when
// the document already carried revisions the moment OpenDocument scanned
// it. structure.docx (see TestHasRevisions_DetectsExistingMarks) is real
// fixture content with genuine w:ins/w:del, not synthetic bytes.
func TestHadRevisionsAtOpen_TrueWhenFixtureAlreadyHasRevisions(t *testing.T) {
	d, err := OpenDocument(fixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if !d.hadRevisionsAtOpen {
		t.Error("hadRevisionsAtOpen = false for structure.docx, want true (it already contains w:ins/w:del)")
	}
}

// TestHadRevisionsAtOpen_DivergesFromHasRevisionsAfterAnEditProducesOne is
// the load-bearing test for the hadRevisionsAtOpen/HasRevisions split: open
// a clean document, hand-splice in a tracked change built from this file's
// own constructors (wrapIns), and confirm hadRevisionsAtOpen is UNCHANGED
// by rescan while HasRevisions() reflects the new state. Without this
// divergence, Task 2's gate (hadRevisionsAtOpen) cannot let a second
// tracked-change edit land after the first one already wrote w:ins/w:del.
func TestHadRevisionsAtOpen_DivergesFromHasRevisionsAfterAnEditProducesOne(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if d.hadRevisionsAtOpen {
		t.Fatal("hadRevisionsAtOpen = true for a clean fixture, want false")
	}
	if d.HasRevisions() {
		t.Fatal("HasRevisions() = true for a clean fixture, want false")
	}

	paras := d.Paras()
	var target Run
	found := false
	for _, p := range paras {
		if len(p.Runs) > 0 && !p.Runs[0].SelfClosing {
			target = p.Runs[0]
			found = true
			break
		}
	}
	if !found {
		t.Fatal("fixture has no usable run to splice a revision onto")
	}

	rc := newRevisionCtx(d.doc, "tester", testNow)
	runBytes := d.doc[target.Elem.Start:target.Elem.End]
	insXML, err := rc.wrapIns(runBytes, "inserted via revisionCtx")
	if err != nil {
		t.Fatalf("wrapIns: %v", err)
	}
	patch := PatchRawSpan(d.doc, target.Elem, string(insXML))
	newDoc, err := Apply(d.doc, []Patch{patch})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := d.pkg.SetPart(DocumentPart, newDoc); err != nil {
		t.Fatalf("SetPart: %v", err)
	}
	if err := d.rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	if !d.HasRevisions() {
		t.Fatal("HasRevisions() = false after splicing in a w:ins, want true")
	}
	if d.hadRevisionsAtOpen {
		t.Fatal("hadRevisionsAtOpen flipped to true after rescan; it must only ever reflect the FIRST scan at OpenDocument time")
	}
}

// ---------------------------------------------------------------------------
// scanRevisions: author collection beyond w:ins/w:del (review-round fix).
// ---------------------------------------------------------------------------

// TestScanRevisions_CollectsAuthorFromEveryTrackChangeElement is a
// table-driven, minimal-fixture pin for each element task-3's review named
// as a false-allow hazard if scanRevisions only looked at w:ins/w:del: a
// reviewer's tracked move, table-cell insert/delete, or formatting change
// carries its own w:author and must show up in Authors, even though none of
// these elements are w:ins or w:del themselves and so must NOT move
// InsCount/DelCount.
func TestScanRevisions_CollectsAuthorFromEveryTrackChangeElement(t *testing.T) {
	tests := []struct {
		name string
		xml  string
	}{
		{"moveFrom", `<w:p><w:moveFrom w:id="1" w:author="Mover" w:date="2026-01-01T00:00:00Z">` +
			`<w:r><w:t>moved away</w:t></w:r></w:moveFrom></w:p>`},
		{"moveTo", `<w:p><w:moveTo w:id="1" w:author="Mover" w:date="2026-01-01T00:00:00Z">` +
			`<w:r><w:t>moved here</w:t></w:r></w:moveTo></w:p>`},
		{"cellIns", `<w:tbl><w:tr><w:tc><w:tcPr><w:cellIns w:id="1" w:author="Mover" w:date="2026-01-01T00:00:00Z"/></w:tcPr>` +
			`<w:p><w:r><w:t>cell</w:t></w:r></w:p></w:tc></w:tr></w:tbl>`},
		{"cellDel", `<w:tbl><w:tr><w:tc><w:tcPr><w:cellDel w:id="1" w:author="Mover" w:date="2026-01-01T00:00:00Z"/></w:tcPr>` +
			`<w:p><w:r><w:t>cell</w:t></w:r></w:p></w:tc></w:tr></w:tbl>`},
		{"rPrChange", `<w:p><w:r><w:rPr><w:b/><w:rPrChange w:id="1" w:author="Mover" w:date="2026-01-01T00:00:00Z">` +
			`<w:rPr/></w:rPrChange></w:rPr><w:t>bold now</w:t></w:r></w:p>`},
		{"pPrChange", `<w:p><w:pPr><w:jc w:val="center"/><w:pPrChange w:id="1" w:author="Mover" w:date="2026-01-01T00:00:00Z">` +
			`<w:pPr/></w:pPrChange></w:pPr><w:r><w:t>centered now</w:t></w:r></w:p>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sum := scanRevisions([]byte(tt.xml))
			if len(sum.Authors) != 1 || sum.Authors[0] != "Mover" {
				t.Errorf("Authors = %v, want [\"Mover\"]", sum.Authors)
			}
			if sum.InsCount != 0 || sum.DelCount != 0 {
				t.Errorf("InsCount/DelCount = %d/%d, want 0/0 (a %s is not a w:ins/w:del)", sum.InsCount, sum.DelCount, tt.name)
			}
		})
	}
}

// TestScanRevisions_InsDelCountsUnaffectedByOtherTrackChangeElements pins
// the other half: a document mixing w:ins/w:del with the other
// track-change-family elements must count only the former, so
// computeNotes' "N insertion(s), M deletion(s)" wording in read notes stays
// accurate even once the author set (correctly) grew to cover more element
// kinds.
func TestScanRevisions_InsDelCountsUnaffectedByOtherTrackChangeElements(t *testing.T) {
	doc := []byte(`<w:p><w:ins w:id="1" w:author="A" w:date="2026-01-01T00:00:00Z"><w:r><w:t>x</w:t></w:r></w:ins>` +
		`<w:moveFrom w:id="2" w:author="B" w:date="2026-01-01T00:00:00Z"><w:r><w:t>y</w:t></w:r></w:moveFrom>` +
		`<w:del w:id="3" w:author="A" w:date="2026-01-01T00:00:00Z"><w:delText>z</w:delText></w:del></w:p>`)
	sum := scanRevisions(doc)
	if sum.InsCount != 1 {
		t.Errorf("InsCount = %d, want 1", sum.InsCount)
	}
	if sum.DelCount != 1 {
		t.Errorf("DelCount = %d, want 1", sum.DelCount)
	}
	if len(sum.Authors) != 2 || sum.Authors[0] != "A" || sum.Authors[1] != "B" {
		t.Errorf("Authors = %v, want [\"A\" \"B\"] (sorted, deduplicated)", sum.Authors)
	}
}

// TestScanRevisions_TrimsWhitespaceFromAuthor and
// TestEffectiveAuthor_TrimsWhitespace pin the review's TrimSpace requirement
// (finding 5): both sides of the gate's author comparison must agree that
// incidental leading/trailing whitespace is not part of the identity, or a
// caller-supplied author differing from an on-disk one only by whitespace
// would manufacture a spurious "different author" refusal that trimming on
// only one side could never fix.
func TestScanRevisions_TrimsWhitespaceFromAuthor(t *testing.T) {
	doc := []byte(`<w:p><w:ins w:id="1" w:author="  Reviewer  " w:date="2026-01-01T00:00:00Z">` +
		`<w:r><w:t>x</w:t></w:r></w:ins></w:p>`)
	sum := scanRevisions(doc)
	if len(sum.Authors) != 1 || sum.Authors[0] != "Reviewer" {
		t.Errorf("Authors = %v, want [\"Reviewer\"] (trimmed)", sum.Authors)
	}
}

func TestEffectiveAuthor_TrimsWhitespace(t *testing.T) {
	if got := effectiveAuthor("  Reviewer  "); got != "Reviewer" {
		t.Errorf("effectiveAuthor(%q) = %q, want %q", "  Reviewer  ", got, "Reviewer")
	}
	if got := effectiveAuthor("   "); got != defaultRevisionAuthor {
		t.Errorf("effectiveAuthor of a whitespace-only author = %q, want the default %q", got, defaultRevisionAuthor)
	}
}

// TestFormatAuthorList pins finding 5's other half: a comma-separated list
// for a human/LLM-facing message, and "(unnamed)" — not a bare "[]" — when
// no author names were found at all.
func TestFormatAuthorList(t *testing.T) {
	if got := formatAuthorList(nil); got != "(unnamed)" {
		t.Errorf("formatAuthorList(nil) = %q, want %q", got, "(unnamed)")
	}
	if got := formatAuthorList([]string{"A"}); got != "A" {
		t.Errorf("formatAuthorList([A]) = %q, want %q", got, "A")
	}
	if got := formatAuthorList([]string{"A", "B"}); got != "A, B" {
		t.Errorf("formatAuthorList([A B]) = %q, want %q", got, "A, B")
	}
}
