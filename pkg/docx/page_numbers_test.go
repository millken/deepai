package docx

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// formatOutlineDoc copies testdata/outline.docx (a real python-docx product
// with NO header/footer at all -- see gen_fixtures.py) into a fresh temp
// file and opens it, the same pattern formatDoc (format_test.go) uses for
// the header/footer-carrying fixture.
func formatOutlineDoc(t *testing.T) (*Document, string) {
	t.Helper()
	data, err := os.ReadFile(outlineFixture)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "outline.docx")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := OpenDocument(p)
	if err != nil {
		t.Fatal(err)
	}
	return d, p
}

// TestFormat_PageNumbers_AddsFooterOnDocumentWithNone is the "python-docx
// style, no footer" end-to-end fixture task 12 brief item 4 requires: a
// document with no footer at all gets a brand-new word/footer1.xml, wired
// through [Content_Types].xml, word/_rels/document.xml.rels, and a
// <w:footerReference> inserted into its <w:sectPr>.
func TestFormat_PageNumbers_AddsFooterOnDocumentWithNone(t *testing.T) {
	d, p := formatOutlineDoc(t)

	if _, ok := d.Part("word/footer1.xml"); ok {
		t.Fatal("fixture unexpectedly already has word/footer1.xml")
	}

	result, err := d.Format(FormatOptions{PageNumbers: true})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if len(result.Applied) != 1 || !strings.Contains(result.Applied[0], "word/footer1.xml") {
		t.Errorf("Applied = %v, want one entry naming word/footer1.xml", result.Applied)
	}
	if len(result.Notes) != 0 {
		t.Errorf("Notes = %v, want none for a document that had no footer", result.Notes)
	}

	// The new footer part exists and carries the exact footer1XML content
	// docx_write itself would write.
	footer, ok := d.Part("word/footer1.xml")
	if !ok {
		t.Fatal("word/footer1.xml was not added")
	}
	if string(footer) != footer1XML {
		t.Errorf("footer content = %q, want the package's own footer1XML", footer)
	}

	// [Content_Types].xml declares the new part.
	ct, _ := d.Part(contentTypesPart)
	if !strings.Contains(string(ct), `PartName="/word/footer1.xml"`) {
		t.Errorf("[Content_Types].xml missing the new footer's Override: %s", ct)
	}
	if !strings.Contains(string(ct), footerContentType) {
		t.Errorf("[Content_Types].xml missing the footer content type: %s", ct)
	}

	// word/_rels/document.xml.rels declares the relationship, and
	// document.xml's footerReference names that SAME relationship id.
	rels, _ := d.Part("word/_rels/document.xml.rels")
	if !strings.Contains(string(rels), `Type="`+footerRelType+`"`) || !strings.Contains(string(rels), `Target="footer1.xml"`) {
		t.Errorf("rels missing the new footer relationship: %s", rels)
	}
	relID := extractRelID(t, string(rels), "footer1.xml")

	doc, _ := d.Part(DocumentPart)
	wantRef := `<w:footerReference w:type="default" r:id="` + relID + `"/>`
	if !strings.Contains(string(doc), wantRef) {
		t.Errorf("document.xml missing %q; got sectPr area: %s", wantRef, tailAround(string(doc), "sectPr"))
	}
	// footerReference must precede pgSz (schema order).
	refIdx := strings.Index(string(doc), wantRef)
	pgSzIdx := strings.Index(string(doc), "<w:pgSz")
	if refIdx < 0 || pgSzIdx < 0 || refIdx > pgSzIdx {
		t.Errorf("footerReference (at %d) must precede pgSz (at %d)", refIdx, pgSzIdx)
	}

	// Well-formedness of every touched part, and a full round-trip through
	// SaveAs/Open to make sure the package as a whole is still valid.
	assertWellFormed(t, ct)
	assertWellFormed(t, rels)
	assertWellFormed(t, doc)

	if err := d.SaveAs(p); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	reopened, err := OpenDocument(p)
	if err != nil {
		t.Fatalf("reopen after adding page numbers: %v", err)
	}
	if _, ok := reopened.Part("word/footer1.xml"); !ok {
		t.Error("word/footer1.xml missing after a save/reopen round trip")
	}
}

// TestFormat_PageNumbers_ByteFidelityOutsideTouchedParts pins the same
// fidelity discipline the fixture's whole test suite relies on elsewhere
// (assertEntriesEqual): adding a footer must leave every entry that was
// NOT one of the four touched parts byte-for-byte identical to the
// original file.
func TestFormat_PageNumbers_ByteFidelityOutsideTouchedParts(t *testing.T) {
	d, p := formatOutlineDoc(t)
	if _, err := d.Format(FormatOptions{PageNumbers: true}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	if err := d.SaveAs(p); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	assertEntriesEqualAllowingNewEntries(t, outlineFixture, p, map[string]bool{
		DocumentPart:                   true,
		contentTypesPart:               true,
		"word/_rels/document.xml.rels": true,
	}, []string{"word/footer1.xml"})
}

// TestFormat_PageNumbers_AlreadyHasFooterIsANoOp is the "docx_write
// product, already has a footer" end-to-end fixture task 12 brief item 4
// requires: WriteDocx unconditionally gives every document its own
// word/footer1.xml/footerReference (footer.go), so asking for page numbers
// again must change nothing at all and say why.
func TestFormat_PageNumbers_AlreadyHasFooterIsANoOp(t *testing.T) {
	p := filepath.Join(t.TempDir(), "written.docx")
	if _, err := WriteDocx(p, WriteOptions{Markdown: "# Title\n\nSome body text.\n"}); err != nil {
		t.Fatalf("WriteDocx: %v", err)
	}
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}

	d, err := OpenDocument(p)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if _, ok := d.Part("word/footer1.xml"); !ok {
		t.Fatal("docx_write product unexpectedly has no word/footer1.xml")
	}

	result, err := d.Format(FormatOptions{PageNumbers: true})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if len(result.Applied) != 0 {
		t.Errorf("Applied = %v, want none: a document that already has a footer must not be touched", result.Applied)
	}
	if len(result.Notes) != 1 || result.Notes[0] != "document already has a footer; not modified" {
		t.Errorf("Notes = %v, want exactly the already-has-a-footer note", result.Notes)
	}
	if d.Modified() {
		t.Error("Modified() is true after a page_numbers call that changed nothing")
	}

	if err := d.SaveAs(p); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("a no-op page_numbers call rewrote the file's bytes")
	}
}

// TestFormat_PageNumbers_SecondCallIsAlsoANoOp pins idempotency directly:
// calling page_numbers:true twice in a row must not add a second footer
// part or a second footerReference the second time.
func TestFormat_PageNumbers_SecondCallIsAlsoANoOp(t *testing.T) {
	d, _ := formatOutlineDoc(t)
	if _, err := d.Format(FormatOptions{PageNumbers: true}); err != nil {
		t.Fatalf("first Format: %v", err)
	}
	docAfterFirst, _ := d.Part(DocumentPart)
	docAfterFirstCopy := append([]byte(nil), docAfterFirst...)

	result, err := d.Format(FormatOptions{PageNumbers: true})
	if err != nil {
		t.Fatalf("second Format: %v", err)
	}
	if len(result.Applied) != 0 {
		t.Errorf("second call Applied = %v, want none", result.Applied)
	}
	if len(result.Notes) != 1 || result.Notes[0] != "document already has a footer; not modified" {
		t.Errorf("second call Notes = %v, want the already-has-a-footer note", result.Notes)
	}
	if _, ok := d.Part("word/footer2.xml"); ok {
		t.Error("second call added a second footer part")
	}
	docAfterSecond, _ := d.Part(DocumentPart)
	if !bytes.Equal(docAfterFirstCopy, docAfterSecond) {
		t.Error("second call changed document.xml even though it was a no-op")
	}
}

// TestFormat_PageNumbers_AvoidsExistingFooterPartName pins
// nextFooterPartName's collision avoidance in a realistic pipeline: a
// document that already has an ORPHANED word/footer1.xml (present in the
// package but not referenced by any sectPr -- e.g. left over from a prior
// partial edit) must get word/footer2.xml, never collide with (and be
// refused by AddPart for) the one already there.
func TestFormat_PageNumbers_AvoidsExistingFooterPartName(t *testing.T) {
	d, _ := formatOutlineDoc(t)
	if err := d.AddPart("word/footer1.xml", []byte(footer1XML)); err != nil {
		t.Fatalf("seed orphaned word/footer1.xml: %v", err)
	}

	result, err := d.Format(FormatOptions{PageNumbers: true})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if len(result.Applied) != 1 || !strings.Contains(result.Applied[0], "word/footer2.xml") {
		t.Errorf("Applied = %v, want it to name word/footer2.xml", result.Applied)
	}
	if _, ok := d.Part("word/footer2.xml"); !ok {
		t.Error("word/footer2.xml was not added")
	}
}

// TestFormat_PageNumbers_RejectsCombinationWithRange pins that PageNumbers,
// like Template/HeadingFont/MarginsMM/Normalize, only makes sense
// document-wide.
func TestFormat_PageNumbers_RejectsCombinationWithRange(t *testing.T) {
	d, _ := formatOutlineDoc(t)
	if _, err := d.Format(FormatOptions{PageNumbers: true, StartPara: 1}); err == nil {
		t.Fatal("page_numbers combined with a range was accepted")
	}
}

// emptyContentTypes is a [Content_Types].xml with no Override at all, for
// TestNextFooterPartName cases that only care about the zip-entry-name
// half of the collision check.
const emptyContentTypes = `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`

// TestNextFooterPartName pins the collision-avoidance rule directly,
// including review round-3 item 2: a name already declared as a
// [Content_Types].xml Override (even with no matching zip entry at all)
// must be skipped too, so a second Override for the same PartName is never
// produced.
func TestNextFooterPartName(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		ct    string
		want  string
	}{
		{"no footer at all", []string{"word/document.xml"}, emptyContentTypes, "word/footer1.xml"},
		{"footer1 taken (zip entry)", []string{"word/footer1.xml"}, emptyContentTypes, "word/footer2.xml"},
		{"footer1 and footer2 taken, gap ignored", []string{"word/footer1.xml", "word/footer2.xml"}, emptyContentTypes, "word/footer3.xml"},
		{"non-matching names ignored", []string{"word/footer.xml", "word/footerX.xml"}, emptyContentTypes, "word/footer1.xml"},
		{
			"footer1 already declared as a Content_Types Override with no matching zip entry",
			[]string{"word/document.xml"},
			`<?xml version="1.0"?><Types xmlns="ns"><Override PartName="/word/footer1.xml" ContentType="x"/></Types>`,
			"word/footer2.xml",
		},
		{
			"zip entry AND Override disagree on which N is taken -- both counted",
			[]string{"word/footer2.xml"},
			`<?xml version="1.0"?><Types xmlns="ns"><Override PartName="/word/footer1.xml" ContentType="x"/></Types>`,
			"word/footer3.xml",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := nextFooterPartName(tt.names, []byte(tt.ct))
			if err != nil {
				t.Fatalf("nextFooterPartName: %v", err)
			}
			if got != tt.want {
				t.Errorf("nextFooterPartName(%v, %s) = %q, want %q", tt.names, tt.ct, got, tt.want)
			}
		})
	}
}

// TestFormat_PageNumbers_AvoidsDuplicateContentTypesOverride is review
// round-3 item 2's end-to-end pin, through the real Format entry point: a
// document whose [Content_Types].xml ALREADY declares an Override for
// word/footer1.xml (independent of whether that part is an actual zip
// entry -- here it deliberately is not, simulating a package a previous
// partial write left inconsistent) must not get a second Override for the
// same PartName; nextFooterPartName must skip straight to word/footer2.xml.
func TestFormat_PageNumbers_AvoidsDuplicateContentTypesOverride(t *testing.T) {
	d, _ := formatOutlineDoc(t)
	ct, _ := d.Part(contentTypesPart)
	ctWithOrphanOverride, err := insertBeforeRootClose(ct,
		`<Override PartName="/word/footer1.xml" ContentType="`+footerContentType+`"/>`)
	if err != nil {
		t.Fatalf("seed orphan Override: %v", err)
	}
	if err := d.SetPart(contentTypesPart, ctWithOrphanOverride); err != nil {
		t.Fatalf("SetPart: %v", err)
	}

	result, err := d.Format(FormatOptions{PageNumbers: true})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if len(result.Applied) != 1 || !strings.Contains(result.Applied[0], "word/footer2.xml") {
		t.Errorf("Applied = %v, want it to name word/footer2.xml", result.Applied)
	}

	ctAfter, _ := d.Part(contentTypesPart)
	if n := strings.Count(string(ctAfter), `PartName="/word/footer1.xml"`); n != 1 {
		t.Errorf("[Content_Types].xml has %d Override(s) for /word/footer1.xml, want exactly 1 (no duplicate)", n)
	}
	if !strings.Contains(string(ctAfter), `PartName="/word/footer2.xml"`) {
		t.Errorf("[Content_Types].xml missing the new word/footer2.xml Override: %s", ctAfter)
	}
}

// TestNextRelationshipID pins the collision-avoidance rule for relationship
// ids, including the fallback for a non-"rIdN"-shaped id.
func TestNextRelationshipID(t *testing.T) {
	tests := []struct {
		name string
		rels string
		want string
	}{
		{
			"simple ascending ids",
			`<Relationships><Relationship Id="rId1"/><Relationship Id="rId2"/></Relationships>`,
			"rId3",
		},
		{
			"non-conforming id does not confuse the numeric scan",
			`<Relationships><Relationship Id="customId"/><Relationship Id="rId5"/></Relationships>`,
			"rId6",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := nextRelationshipID([]byte(tt.rels))
			if err != nil {
				t.Fatalf("nextRelationshipID: %v", err)
			}
			if got != tt.want {
				t.Errorf("nextRelationshipID(%s) = %q, want %q", tt.rels, got, tt.want)
			}
		})
	}
}

// TestInsertBeforeRootClose pins the narrow-patch primitive both
// [Content_Types].xml and the rels file are patched through: the insert
// lands immediately before the root element's own close tag, and every
// other byte is untouched.
func TestInsertBeforeRootClose(t *testing.T) {
	data := []byte(`<?xml version="1.0"?><Types xmlns="ns"><Default Extension="xml"/></Types>`)
	out, err := insertBeforeRootClose(data, `<Override PartName="/x"/>`)
	if err != nil {
		t.Fatalf("insertBeforeRootClose: %v", err)
	}
	want := `<?xml version="1.0"?><Types xmlns="ns"><Default Extension="xml"/><Override PartName="/x"/></Types>`
	if string(out) != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

// TestInsertBeforeRootClose_ExpandsSelfClosingRoot is review round-3 item
// 3's red-light scenario: a zero-relationship rels part (or an empty
// Content_Types) can legally be a self-closing root element with no
// children at all. Before the fix, findRootCloseTagStart returned the
// offset immediately AFTER the whole self-closing tag (encoding/xml
// synthesizes a same-offset EndElement for one) -- OUTSIDE the root
// element entirely -- so the inserted content landed as a second top-level
// element sibling to the closed root instead of becoming its child,
// producing multiple-root-element XML no parser accepts. This must now
// expand the self-closing tag into an open/close pair holding insertXML.
func TestInsertBeforeRootClose_ExpandsSelfClosingRoot(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			"zero-relationship rels part",
			`<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"/>`,
			`<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1"/></Relationships>`,
		},
		{
			"self-closing root with no attributes at all",
			`<Relationships/>`,
			`<Relationships><Relationship Id="rId1"/></Relationships>`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := insertBeforeRootClose([]byte(tt.data), `<Relationship Id="rId1"/>`)
			if err != nil {
				t.Fatalf("insertBeforeRootClose: %v", err)
			}
			if string(out) != tt.want {
				t.Errorf("got %q, want %q", out, tt.want)
			}
			assertWellFormed(t, out)
			// The inserted element must be a CHILD of the root, not a
			// second top-level sibling: exactly one "<Relationships" open
			// tag, and the new Relationship must sit strictly between it
			// and the corresponding close tag.
			openIdx := strings.Index(string(out), "<Relationships")
			closeIdx := strings.LastIndex(string(out), "</Relationships>")
			insertIdx := strings.Index(string(out), `<Relationship Id="rId1"/>`)
			if openIdx < 0 || closeIdx < 0 || insertIdx < 0 || !(openIdx < insertIdx && insertIdx < closeIdx) {
				t.Errorf("inserted element is not nested inside the root element: %q", out)
			}
		})
	}
}

// TestFormat_PageNumbers_HandlesSelfClosingContentTypesAndRels is the same
// self-closing-root scenario, but exercised end-to-end through
// Document.Format: a document whose [Content_Types].xml and rels are both
// self-closing (the minimal legal shape bodyDoc-style synthetic fixtures
// often use) must still get a working footer wired in, not a corrupted
// multi-root part.
func TestFormat_PageNumbers_HandlesSelfClosingContentTypesAndRels(t *testing.T) {
	d := pageNumbersDoc(t,
		`<w:body><w:p/><w:sectPr><w:pgSz w:w="12240" w:h="15840"/></w:sectPr></w:body>`,
		`<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`,
		`<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"/>`,
	)
	result, err := d.Format(FormatOptions{PageNumbers: true})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if len(result.Applied) != 1 {
		t.Fatalf("Applied = %v, want one entry", result.Applied)
	}

	ct, _ := d.Part(contentTypesPart)
	assertWellFormed(t, ct)
	if !strings.Contains(string(ct), `PartName="/word/footer1.xml"`) {
		t.Errorf("[Content_Types].xml missing the footer Override: %s", ct)
	}
	rels, _ := d.Part("word/_rels/document.xml.rels")
	assertWellFormed(t, rels)
	if !strings.Contains(string(rels), `Target="footer1.xml"`) {
		t.Errorf("rels missing the footer relationship: %s", rels)
	}
	doc, _ := d.Part(DocumentPart)
	assertWellFormed(t, doc)
	if !strings.Contains(string(doc), "<w:footerReference") {
		t.Errorf("document.xml missing footerReference: %s", doc)
	}
}

// TestAddPageNumberFooter_RejectsMissingRelationshipsPrefix is review
// round-3 item 1's red-light scenario (critical): a document.xml root
// element that does not bind the "r" prefix to the relationships namespace
// at all must be refused BEFORE any part is written, rather than silently
// producing a footerReference r:id="..." attribute under an undeclared
// prefix -- a .docx Word refuses to open, while docx_format would have
// reported success.
func TestAddPageNumberFooter_RejectsMissingRelationshipsPrefix(t *testing.T) {
	d, _ := formatOutlineDoc(t)
	doc, _ := d.Part(DocumentPart)
	// Strip the xmlns:r declaration from the root element only -- leave
	// every other namespace (including xmlns:w, so requireWordNamespacePrefix
	// upstream still passes) untouched.
	stripped := bytes.Replace(doc,
		[]byte(`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" `), nil, 1)
	if bytes.Equal(stripped, doc) {
		t.Fatal("test setup did not actually strip xmlns:r from the fixture; fixture shape changed?")
	}
	if err := d.SetPart(DocumentPart, stripped); err != nil {
		t.Fatalf("SetPart: %v", err)
	}

	ctBefore, _ := d.Part(contentTypesPart)
	ctBeforeCopy := append([]byte(nil), ctBefore...)
	relsBefore, _ := d.Part("word/_rels/document.xml.rels")
	relsBeforeCopy := append([]byte(nil), relsBefore...)

	_, err := d.Format(FormatOptions{PageNumbers: true})
	if err == nil {
		t.Fatal("page_numbers succeeded on a document.xml with no \"r\" namespace prefix bound; want an error")
	}
	if !strings.Contains(err.Error(), "r") || !strings.Contains(err.Error(), "relationships") {
		t.Errorf("error = %q, want it to mention the missing r/relationships namespace binding", err)
	}

	// Nothing was written: no footer part, and Content_Types/rels
	// completely unchanged.
	if _, ok := d.Part("word/footer1.xml"); ok {
		t.Error("word/footer1.xml was added despite the missing namespace prefix")
	}
	ctAfter, _ := d.Part(contentTypesPart)
	if !bytes.Equal(ctBeforeCopy, ctAfter) {
		t.Error("[Content_Types].xml was modified despite the rejected call")
	}
	relsAfter, _ := d.Part("word/_rels/document.xml.rels")
	if !bytes.Equal(relsBeforeCopy, relsAfter) {
		t.Error("rels was modified despite the rejected call")
	}
}

// pageNumbersDoc builds a temp .docx from a raw document.xml body fragment
// plus caller-supplied [Content_Types].xml/rels content, and opens it as a
// Document -- bodyDoc's (edit_shapes_test.go) counterpart for tests that
// need addPageNumberFooter's full four-part pipeline (footer part,
// Content_Types, rels, sectPr) rather than bodyDoc's document-only shape.
// The document.xml root element always declares xmlns:r (every real
// producer this package has seen does), so requireRelationshipsPrefix
// passes unless a test deliberately strips it afterward (see
// TestAddPageNumberFooter_RejectsMissingRelationshipsPrefix, which uses
// formatOutlineDoc instead for that reason).
func pageNumbersDoc(t *testing.T, bodyXML, contentTypesXML, relsXML string) *Document {
	t.Helper()
	docXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" ` +
		`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
		bodyXML + `</w:document>`

	p := filepath.Join(t.TempDir(), "synthetic.docx")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entries := []struct{ name, content string }{
		{"[Content_Types].xml", contentTypesXML},
		{"word/document.xml", docXML},
		{"word/_rels/document.xml.rels", relsXML},
	}
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := OpenDocument(p)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	return d
}

// ---------------------------------------------------------------------------
// planFooterReferenceInsertPatch: three shapes review round-3 item 5 named
// as "reviewed correct but zero formal coverage" -- multi-section
// insertion, self-closing <w:sectPr/> expansion, and landing after an
// existing headerReference. These test the plan-level helper directly
// against hand-built document.xml fragments, the same way
// TestPlanMarginPatches_* (format_task9_test.go) already does for its
// sibling planMarginPatches.
// ---------------------------------------------------------------------------

// TestPlanFooterReferenceInsertPatch_MultipleSectPr pins that a
// multi-section document gets a footerReference inserted into EVERY
// section independently, each landing before its own pgSz.
func TestPlanFooterReferenceInsertPatch_MultipleSectPr(t *testing.T) {
	doc := []byte(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:body>` +
		`<w:p><w:pPr><w:sectPr><w:pgSz w:w="12240" w:h="15840"/></w:sectPr></w:pPr></w:p>` +
		`<w:p/>` +
		`<w:sectPr><w:pgSz w:w="16838" w:h="11906"/></w:sectPr>` +
		`</w:body></w:document>`)

	sects, childrenList, err := scanSectPrs(doc)
	if err != nil {
		t.Fatalf("scanSectPrs: %v", err)
	}
	if len(sects) != 2 {
		t.Fatalf("got %d sectPr(s), want 2", len(sects))
	}

	var patches []Patch
	for i, sect := range sects {
		patches = append(patches, planFooterReferenceInsertPatch(doc, sect, childrenList[i], "rId7"))
	}
	out, err := Apply(doc, patches)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	mustWellFormedXML(t, out)

	got := string(out)
	if n := strings.Count(got, `<w:footerReference w:type="default" r:id="rId7"/>`); n != 2 {
		t.Fatalf("got %d footerReference insertions, want 2:\n%s", n, got)
	}
	// Each footerReference must precede its OWN section's pgSz.
	want1 := `<w:footerReference w:type="default" r:id="rId7"/><w:pgSz w:w="12240" w:h="15840"/>`
	want2 := `<w:footerReference w:type="default" r:id="rId7"/><w:pgSz w:w="16838" w:h="11906"/>`
	if !strings.Contains(got, want1) {
		t.Errorf("first section's footerReference not immediately before its own pgSz:\nwant substring: %s\ngot: %s", want1, got)
	}
	if !strings.Contains(got, want2) {
		t.Errorf("second section's footerReference not immediately before its own pgSz:\nwant substring: %s\ngot: %s", want2, got)
	}
}

// TestPlanFooterReferenceInsertPatch_ExpandsSelfClosingSectPr pins the
// self-closing <w:sectPr/> branch (no properties at all -- a section that
// only ever inherited defaults): it must be expanded to hold the new
// footerReference instead of being left alone or corrupted.
func TestPlanFooterReferenceInsertPatch_ExpandsSelfClosingSectPr(t *testing.T) {
	doc := []byte(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:body><w:p/><w:sectPr/></w:body></w:document>`)

	sects, childrenList, err := scanSectPrs(doc)
	if err != nil {
		t.Fatalf("scanSectPrs: %v", err)
	}
	if len(sects) != 1 || !sects[0].selfClosing {
		t.Fatalf("test setup did not produce a self-closing sectPr: %+v", sects)
	}

	patch := planFooterReferenceInsertPatch(doc, sects[0], childrenList[0], "rId7")
	out, err := Apply(doc, []Patch{patch})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	mustWellFormedXML(t, out)

	want := `<w:sectPr><w:footerReference w:type="default" r:id="rId7"/></w:sectPr>`
	if !strings.Contains(string(out), want) {
		t.Errorf("self-closing sectPr was not expanded to hold footerReference:\nwant substring: %s\ngot: %s", want, out)
	}
}

// TestPlanFooterReferenceInsertPatch_LandsAfterHeaderReference pins the
// schema-order requirement task 12 brief item 2 calls out directly: a
// section that already has a headerReference (but no footerReference)
// must get its new footerReference inserted AFTER that headerReference and
// BEFORE pgSz -- never disturbing the existing headerReference itself.
func TestPlanFooterReferenceInsertPatch_LandsAfterHeaderReference(t *testing.T) {
	doc := []byte(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:body><w:p/><w:sectPr><w:headerReference w:type="default" r:id="rId9"/>` +
		`<w:pgSz w:w="12240" w:h="15840"/></w:sectPr></w:body></w:document>`)

	sects, childrenList, err := scanSectPrs(doc)
	if err != nil {
		t.Fatalf("scanSectPrs: %v", err)
	}
	if len(sects) != 1 {
		t.Fatalf("got %d sectPr(s), want 1", len(sects))
	}

	patch := planFooterReferenceInsertPatch(doc, sects[0], childrenList[0], "rId10")
	out, err := Apply(doc, []Patch{patch})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	mustWellFormedXML(t, out)

	want := `<w:headerReference w:type="default" r:id="rId9"/><w:footerReference w:type="default" r:id="rId10"/><w:pgSz w:w="12240" w:h="15840"/>`
	if !strings.Contains(string(out), want) {
		t.Errorf("footerReference did not land between headerReference and pgSz:\nwant substring: %s\ngot: %s", want, out)
	}
}

// TestFormat_PageNumbers_LandsAfterExistingHeaderReferenceEndToEnd is the
// same header-then-footer ordering requirement, exercised end-to-end
// through Document.Format on a REAL fixture: structure.docx already has
// both a headerReference and a footerReference; stripping only the
// footerReference (leaving the orphaned word/footer1.xml part and its rels
// entry/Content_Types Override in place, an entirely realistic shape) means
// page_numbers:true now sees no footer anywhere and must add a new one --
// picking word/footer2.xml (footer1.xml is taken) and landing the new
// footerReference after the existing headerReference and before pgSz.
func TestFormat_PageNumbers_LandsAfterExistingHeaderReferenceEndToEnd(t *testing.T) {
	d, _ := formatDoc(t) // formatDoc (format_test.go) opens the structure.docx fixture
	doc, _ := d.Part(DocumentPart)
	stripped := bytes.Replace(doc, []byte(`<w:footerReference w:type="default" r:id="rId10"/>`), nil, 1)
	if bytes.Equal(stripped, doc) {
		t.Fatal("test setup did not find the fixture's own footerReference to strip; fixture shape changed?")
	}
	if err := d.SetPart(DocumentPart, stripped); err != nil {
		t.Fatalf("SetPart: %v", err)
	}

	result, err := d.Format(FormatOptions{PageNumbers: true})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if len(result.Applied) != 1 || !strings.Contains(result.Applied[0], "word/footer2.xml") {
		t.Errorf("Applied = %v, want it to name word/footer2.xml (footer1.xml is already taken)", result.Applied)
	}

	docAfter, _ := d.Part(DocumentPart)
	got := string(docAfter)
	headerIdx := strings.Index(got, "<w:headerReference")
	footerIdx := strings.Index(got, "<w:footerReference")
	pgSzIdx := strings.Index(got, "<w:pgSz")
	if headerIdx < 0 || footerIdx < 0 || pgSzIdx < 0 || !(headerIdx < footerIdx && footerIdx < pgSzIdx) {
		t.Errorf("footerReference (at %d) must land after headerReference (at %d) and before pgSz (at %d):\n%s",
			footerIdx, headerIdx, pgSzIdx, tailAround(got, "sectPr"))
	}
}

// ---------------------------------------------------------------------------
// pageNumberCaveatNotes: titlePg / evenAndOddHeaders (review round-3 item 4)
// ---------------------------------------------------------------------------

// TestPageNumberCaveatNotes pins both caveat conditions directly, plus the
// "neither condition present" baseline (no notes at all).
func TestPageNumberCaveatNotes(t *testing.T) {
	noTitlePgChildren := []map[string]elemInfo{{"pgSz": {found: true}}}
	titlePgChildren := []map[string]elemInfo{{"titlePg": {found: true}, "pgSz": {found: true}}}

	t.Run("neither condition present", func(t *testing.T) {
		notes := pageNumberCaveatNotes(noTitlePgChildren, []byte(`<w:settings xmlns:w="ns"/>`))
		if len(notes) != 0 {
			t.Errorf("notes = %v, want none", notes)
		}
	})

	t.Run("titlePg present", func(t *testing.T) {
		notes := pageNumberCaveatNotes(titlePgChildren, nil)
		if len(notes) != 1 || !strings.Contains(notes[0], "titlePg") {
			t.Errorf("notes = %v, want one mentioning titlePg", notes)
		}
	})

	t.Run("evenAndOddHeaders present", func(t *testing.T) {
		notes := pageNumberCaveatNotes(noTitlePgChildren, []byte(`<w:settings xmlns:w="ns"><w:evenAndOddHeaders/></w:settings>`))
		if len(notes) != 1 || !strings.Contains(notes[0], "evenAndOddHeaders") {
			t.Errorf("notes = %v, want one mentioning evenAndOddHeaders", notes)
		}
	})

	t.Run("both present", func(t *testing.T) {
		notes := pageNumberCaveatNotes(titlePgChildren, []byte(`<w:settings xmlns:w="ns"><w:evenAndOddHeaders/></w:settings>`))
		if len(notes) != 2 {
			t.Errorf("notes = %v, want two (one per condition)", notes)
		}
	})

	t.Run("nil settings.xml is treated as absent, not a panic", func(t *testing.T) {
		notes := pageNumberCaveatNotes(noTitlePgChildren, nil)
		if len(notes) != 0 {
			t.Errorf("notes = %v, want none", notes)
		}
	})
}

// extractRelID pulls the Id attribute of the Relationship whose Target is
// target, for tests that need to cross-check document.xml's r:id against
// what the rels file actually assigned.
func extractRelID(t *testing.T, relsXML, target string) string {
	t.Helper()
	i := strings.Index(relsXML, `Target="`+target+`"`)
	if i < 0 {
		t.Fatalf("rels has no relationship targeting %q: %s", target, relsXML)
	}
	// Id="..." can appear before or after Target="..." in the same element;
	// search backward and forward from the element's own start/end.
	elemStart := strings.LastIndex(relsXML[:i], "<Relationship")
	elemEnd := strings.Index(relsXML[i:], "/>")
	elem := relsXML[elemStart : i+elemEnd]
	idIdx := strings.Index(elem, `Id="`)
	if idIdx < 0 {
		t.Fatalf("relationship element has no Id attribute: %s", elem)
	}
	rest := elem[idIdx+len(`Id="`):]
	return rest[:strings.IndexByte(rest, '"')]
}

// tailAround returns a small window of s around the first occurrence of
// marker, for a more useful test failure message than the whole document.
func tailAround(s, marker string) string {
	i := strings.Index(s, marker)
	if i < 0 {
		return "(marker not found)"
	}
	start := i - 80
	if start < 0 {
		start = 0
	}
	end := i + 80
	if end > len(s) {
		end = len(s)
	}
	return s[start:end]
}

// assertEntriesEqualAllowingNewEntries is assertEntriesEqual's counterpart
// for AddPart: it allows newPath to have MORE entries than oldPath (the
// ones named in addedNames), while still requiring every entry oldPath DID
// have to be present in newPath, in the same relative order, either
// byte-identical or (if named in changed) merely present and different.
func assertEntriesEqualAllowingNewEntries(t *testing.T, oldPath, newPath string, changed map[string]bool, addedNames []string) {
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

	added := map[string]bool{}
	for _, n := range addedNames {
		added[n] = true
	}
	if got, want := len(newZ.File), len(oldZ.File)+len(addedNames); got != want {
		t.Fatalf("entry count = %d, want %d (original %d + %d added)", got, want, len(oldZ.File), len(addedNames))
	}

	newByName := map[string]*zip.File{}
	for _, f := range newZ.File {
		newByName[f.Name] = f
	}

	for _, of := range oldZ.File {
		nf, ok := newByName[of.Name]
		if !ok {
			t.Errorf("%s: present in the original file but missing from the new one", of.Name)
			continue
		}
		oldData := readZipEntry(t, of)
		newData := readZipEntry(t, nf)
		if changed[of.Name] {
			if bytes.Equal(oldData, newData) {
				t.Errorf("%s: expected to change but is identical", of.Name)
			}
			continue
		}
		if !bytes.Equal(oldData, newData) {
			t.Errorf("%s: not byte-identical (old %d bytes, new %d bytes)", of.Name, len(oldData), len(newData))
		}
	}
	for _, name := range addedNames {
		if _, ok := newByName[name]; !ok {
			t.Errorf("%s: expected new entry is missing", name)
		}
	}
}
