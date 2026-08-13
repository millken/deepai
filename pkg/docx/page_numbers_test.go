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

// TestNextFooterPartName pins the collision-avoidance rule directly.
func TestNextFooterPartName(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		want  string
	}{
		{"no footer at all", []string{"word/document.xml"}, "word/footer1.xml"},
		{"footer1 taken", []string{"word/footer1.xml"}, "word/footer2.xml"},
		{"footer1 and footer2 taken, gap ignored", []string{"word/footer1.xml", "word/footer2.xml"}, "word/footer3.xml"},
		{"non-matching names ignored", []string{"word/footer.xml", "word/footerX.xml"}, "word/footer1.xml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextFooterPartName(tt.names); got != tt.want {
				t.Errorf("nextFooterPartName(%v) = %q, want %q", tt.names, got, tt.want)
			}
		})
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
