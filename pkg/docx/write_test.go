package docx

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeAndReopen writes md to a fresh temp file via WriteDocx and reopens it
// through this package's own OpenDocument/Scan, per the brief: verifying the
// output with our own reader checks how a reader — and by extension Word —
// resolves what WriteDocx produced, rather than asserting on XML strings we
// just wrote ourselves.
func writeAndReopen(t *testing.T, md string) (*Document, WriteResult, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "out.docx")
	res, err := WriteDocx(p, WriteOptions{Markdown: md})
	if err != nil {
		t.Fatalf("WriteDocx: %v", err)
	}
	d, err := OpenDocument(p)
	if err != nil {
		t.Fatalf("the generated file cannot be reopened: %v", err)
	}
	return d, res, p
}

func TestWrite_HeadingsCarryHeadingStyles(t *testing.T) {
	d, _, _ := writeAndReopen(t, "# Chapter\n\nbody text\n\n## Section\n\nmore text\n")
	paras := d.Paras()
	if len(paras) != 4 {
		t.Fatalf("got %d paragraphs, want 4", len(paras))
	}
	if paras[0].Style != "Heading1" {
		t.Errorf("paras[0].Style = %q, want Heading1", paras[0].Style)
	}
	if paras[2].Style != "Heading2" {
		t.Errorf("paras[2].Style = %q, want Heading2", paras[2].Style)
	}
	// An ordinary body paragraph references BodyText (docx-chinese-
	// typography plan, Task 2: the reference document's first-line indent
	// + 1.5x line spacing) -- not "" and not "Normal" as it did before
	// that task, but also not a heading style, which is what this test
	// actually guards against.
	if paras[1].Style != StyleBodyText {
		t.Errorf("body paragraph has style %q, want %q", paras[1].Style, StyleBodyText)
	}
}

// The style definitions must actually exist, or Word renders headings as body
// text -- the failure mode that looks like success.
func TestWrite_HeadingStylesAreDefinedInStylesXML(t *testing.T) {
	d, _, _ := writeAndReopen(t, "# H\n")
	s, ok := d.Part("word/styles.xml")
	if !ok {
		t.Fatal("styles.xml missing")
	}
	for _, id := range []string{"Normal", "Heading1", "Heading2", "Heading3", "Heading4", "Heading5", "Heading6"} {
		if !strings.Contains(string(s), `w:styleId="`+id+`"`) {
			t.Errorf("styles.xml does not define %s; Word would render it as plain text", id)
		}
	}
}

func TestWrite_BoldAndItalicBecomeRuns(t *testing.T) {
	d, _, _ := writeAndReopen(t, "plain **bold** and *italic* end\n")
	if len(d.Paras()) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(d.Paras()))
	}
	var text strings.Builder
	for _, r := range d.Paras()[0].Runs {
		text.WriteString(r.Text)
	}
	if text.String() != "plain bold and italic end" {
		t.Errorf("visible text = %q, want the markers stripped", text.String())
	}
	if len(d.Paras()[0].Runs) < 3 {
		t.Errorf("got %d runs; bold and italic must be their own runs", len(d.Paras()[0].Runs))
	}
	doc, _ := d.Part(DocumentPart)
	if !strings.Contains(string(doc), "<w:b/>") {
		t.Error("no bold run property")
	}
	if !strings.Contains(string(doc), "<w:i/>") {
		t.Error("no italic run property")
	}
}

func TestWrite_XMLMetacharactersAreEscaped(t *testing.T) {
	d, _, _ := writeAndReopen(t, "A & B < C > D\n")
	var text strings.Builder
	for _, r := range d.Paras()[0].Runs {
		text.WriteString(r.Text)
	}
	if text.String() != "A & B < C > D" {
		t.Errorf("round trip corrupted the text: %q", text.String())
	}
}

// Lists and tables are now rendered structurally (Task 2), so they must no
// longer be declared unsupported -- doing so would be actively wrong, not
// just imprecise. Images remain unsupported (Task 3 territory) and must
// still be declared: this is the one assertion from Task 1 that must
// survive, per the brief ("do not just delete the test -- that would
// discard the guarantee that unsupported syntax is always declared").
func TestWrite_UnsupportedSyntaxIsDeclared(t *testing.T) {
	_, res, _ := writeAndReopen(t, "- item one\n- item two\n\n"+
		"| a | b |\n|---|---|\n| 1 | 2 |\n\n"+
		"![alt](pic.png)\n")
	joined := strings.Join(res.Notes, " | ")
	if strings.Contains(joined, "list") {
		t.Errorf("lists are now rendered structurally; notes must not mention them: %q", joined)
	}
	if strings.Contains(joined, "table") {
		t.Errorf("a well-formed table is now rendered structurally; notes must not mention it: %q", joined)
	}
	if !strings.Contains(joined, "image") {
		t.Errorf("images remain unsupported and must still be declared: %q", joined)
	}
}

func TestWrite_SupportedOnlyInputProducesNoNotes(t *testing.T) {
	_, res, _ := writeAndReopen(t, "# H\n\nbody **bold**\n")
	if len(res.Notes) != 0 {
		t.Errorf("Notes = %v, want none for fully supported input", res.Notes)
	}
}

// Same input twice must produce the same bytes.
func TestWrite_IsDeterministic(t *testing.T) {
	md := "# H\n\nbody\n"
	a := filepath.Join(t.TempDir(), "a.docx")
	b := filepath.Join(t.TempDir(), "b.docx")
	if _, err := WriteDocx(a, WriteOptions{Markdown: md}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // cross a DOS timestamp bucket
	if _, err := WriteDocx(b, WriteOptions{Markdown: md}); err != nil {
		t.Fatal(err)
	}
	ab, _ := os.ReadFile(a)
	bb, _ := os.ReadFile(b)
	if !bytes.Equal(ab, bb) {
		t.Error("two runs produced different bytes; zip timestamps are not pinned")
	}
}

func TestWrite_EmptyMarkdownProducesAValidEmptyDocument(t *testing.T) {
	d, _, _ := writeAndReopen(t, "")
	if d.TotalParas() > 1 {
		t.Errorf("empty input produced %d paragraphs", d.TotalParas())
	}
}

func TestWrite_RefusesToOverwriteAnExistingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.docx")
	if err := os.WriteFile(p, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteDocx(p, WriteOptions{Markdown: "# H\n"}); err == nil {
		t.Fatal("WriteDocx overwrote an existing file; creating must not destroy")
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "existing" {
		t.Errorf("existing file content changed: %q", got)
	}
}

// --- Additional coverage beyond the brief, driven by the self-review questions ---

// A heading deeper than level 6 (seven '#' characters) is not a valid ATX
// heading in CommonMark either; it must fall through to an ordinary
// paragraph, hashes and all, rather than being clamped to Heading6 or
// crashing.
func TestWrite_SevenHashesIsNotAHeading(t *testing.T) {
	d, _, _ := writeAndReopen(t, "####### Not a heading\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	// See TestWrite_HeadingsCarryHeadingStyles' comment: an ordinary
	// paragraph (which is what seven hashes falls back to) references
	// BodyText, not "" or "Normal", since the docx-chinese-typography
	// plan's Task 2. This test's actual point -- that seven hashes is NOT
	// treated as a heading -- is unaffected.
	if paras[0].Style != StyleBodyText {
		t.Errorf("seven-hash line got style %q, want plain paragraph style %q", paras[0].Style, StyleBodyText)
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	if text.String() != "####### Not a heading" {
		t.Errorf("text = %q, want the hashes preserved literally", text.String())
	}
}

// An unclosed bold marker must not swallow the rest of the document or
// produce invalid XML; the rule this package applies is that the marker
// characters are kept as literal text.
func TestWrite_UnclosedBoldIsKeptLiteral(t *testing.T) {
	d, _, _ := writeAndReopen(t, "**unclosed bold\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	if text.String() != "**unclosed bold" {
		t.Errorf("text = %q, want markers kept literally", text.String())
	}
	doc, _ := d.Part(DocumentPart)
	if strings.Contains(string(doc), "<w:b/>") {
		t.Error("unclosed bold marker must not turn into a bold run")
	}
}

// Nested emphasis ("**bold *and italic* together**") must resolve to a run
// carrying both bold and italic for the inner span, and must not produce
// invalid XML (no dangling <w:rPr> or unbalanced tags).
func TestWrite_NestedBoldAndItalic(t *testing.T) {
	d, _, _ := writeAndReopen(t, "**bold *and italic* together**\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	if text.String() != "bold and italic together" {
		t.Errorf("text = %q, want markers stripped", text.String())
	}
	doc, _ := d.Part(DocumentPart)
	docStr := string(doc)
	if !strings.Contains(docStr, "<w:b/><w:i/>") && !strings.Contains(docStr, "<w:i/><w:b/>") {
		t.Errorf("expected one run with both bold and italic properties: %s", docStr)
	}
}

// A paragraph consisting only of whitespace must not produce an empty
// paragraph: it is indistinguishable from an ordinary blank-line separator
// and is dropped the same way.
func TestWrite_WhitespaceOnlyLineProducesNoParagraph(t *testing.T) {
	d, _, _ := writeAndReopen(t, "first\n\n   \n\nsecond\n")
	paras := d.Paras()
	if len(paras) != 2 {
		t.Fatalf("got %d paragraphs, want 2 (whitespace-only line must not produce one)", len(paras))
	}
}

// Title becomes the document's first paragraph, styled Heading1, ahead of
// anything parsed from Markdown.
func TestWrite_TitleBecomesLeadingHeading1(t *testing.T) {
	d, res, _ := writeAndReopen2(t, WriteOptions{Markdown: "body\n", Title: "My Title"})
	paras := d.Paras()
	if len(paras) != 2 {
		t.Fatalf("got %d paragraphs, want 2", len(paras))
	}
	if paras[0].Style != "Heading1" {
		t.Errorf("paras[0].Style = %q, want Heading1", paras[0].Style)
	}
	if len(paras[0].Runs) == 0 || paras[0].Runs[0].Text != "My Title" {
		t.Errorf("paras[0] text = %+v, want %q", paras[0].Runs, "My Title")
	}
	if res.Paras != 2 {
		t.Errorf("WriteResult.Paras = %d, want 2", res.Paras)
	}
}

func writeAndReopen2(t *testing.T, opts WriteOptions) (*Document, WriteResult, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "out.docx")
	res, err := WriteDocx(p, opts)
	if err != nil {
		t.Fatalf("WriteDocx: %v", err)
	}
	d, err := OpenDocument(p)
	if err != nil {
		t.Fatalf("the generated file cannot be reopened: %v", err)
	}
	return d, res, p
}

// --- Task 2: lists and tables ---

// Scan (scan.go) does not expose numPr/ilvl/numId -- that metadata is not a
// Para field -- so list structure is verified against the raw document.xml
// bytes, the same idiom TestWrite_BoldAndItalicBecomeRuns already uses for
// <w:b/>/<w:i/>. The visible text is still checked through Scan, to confirm
// the marker itself was stripped rather than merely accompanied by numPr.
func TestWrite_UnorderedListProducesNumPr(t *testing.T) {
	d, _, _ := writeAndReopen(t, "- item one\n- item two\n")
	paras := d.Paras()
	if len(paras) != 2 {
		t.Fatalf("got %d paragraphs, want 2", len(paras))
	}
	for i, p := range paras {
		var text strings.Builder
		for _, r := range p.Runs {
			text.WriteString(r.Text)
		}
		if strings.Contains(text.String(), "-") {
			t.Errorf("paras[%d] visible text still carries the marker: %q", i, text.String())
		}
	}
	doc, _ := d.Part(DocumentPart)
	s := string(doc)
	if !strings.Contains(s, "<w:numPr>") {
		t.Error("list item paragraph has no <w:numPr>")
	}
	if !strings.Contains(s, `<w:numId w:val="1"/>`) {
		t.Error("unordered list item does not reference numId 1")
	}
}

// Nesting is encoded purely via <w:ilvl>. The input uses 2-space indent for
// the nested item; the document's only positive indent is 2 spaces, so the
// inferred unit (inferListIndentUnit) is 2 and level = 2/2 = 1.
func TestWrite_NestedListLevelsUseIlvl(t *testing.T) {
	d, _, _ := writeAndReopen(t, "- top\n  - nested\n")
	doc, _ := d.Part(DocumentPart)
	s := string(doc)
	if !strings.Contains(s, `<w:ilvl w:val="0"/>`) {
		t.Error("top-level item missing <w:ilvl w:val=\"0\"/>")
	}
	if !strings.Contains(s, `<w:ilvl w:val="1"/>`) {
		t.Error("nested item (2-space indent) missing <w:ilvl w:val=\"1\"/>")
	}
}

// Item 0's fix: the indent unit is now INFERRED per document (the smallest
// positive leading-indent among its list items), not pinned at a fixed 2.
// A document indented entirely with 4 spaces has no 2-space item to infer
// from, so its own smallest positive indent (4) becomes ITS unit, and the
// nested item still lands at ilvl 1 -- not ilvl 2, which is what a FIXED
// 2-space unit would have produced (4 spaces / 2 = 2), silently doubling
// the depth and shifting every bullet glyph to the wrong level in Word.
func TestWrite_FourSpaceIndentMapsToLevelOne(t *testing.T) {
	d, _, _ := writeAndReopen(t, "- top\n    - nested\n")
	doc, _ := d.Part(DocumentPart)
	s := string(doc)
	if !strings.Contains(s, `<w:ilvl w:val="0"/>`) {
		t.Errorf("top-level item missing <w:ilvl w:val=\"0\"/>: %s", s)
	}
	if !strings.Contains(s, `<w:ilvl w:val="1"/>`) {
		t.Errorf("4-space indent did not map to ilvl 1: %s", s)
	}
	if strings.Contains(s, `<w:ilvl w:val="2"/>`) {
		t.Errorf("4-space indent must not map to ilvl 2 (the old, fixed-unit bug): %s", s)
	}
}

// The brief's own pinning test: a document indented with 2 spaces per level
// and one indented with 4 spaces per level must produce the EXACT SAME
// ilvl sequence (0, 1, 2), because each document's unit is inferred from
// itself -- 2-space document infers unit 2, 4-space document infers unit 4,
// and both then compute the same three levels.
func TestWrite_TwoAndFourSpaceIndentsProduceSameIlvlSequence(t *testing.T) {
	ilvlRE := regexp.MustCompile(`<w:ilvl w:val="(\d+)"/>`)
	seq := func(md string) []string {
		d, _, _ := writeAndReopen(t, md)
		doc, _ := d.Part(DocumentPart)
		matches := ilvlRE.FindAllStringSubmatch(string(doc), -1)
		out := make([]string, len(matches))
		for i, m := range matches {
			out[i] = m[1]
		}
		return out
	}

	twoSpace := seq("- a\n  - b\n    - c\n")
	fourSpace := seq("- a\n    - b\n        - c\n")

	want := []string{"0", "1", "2"}
	if !reflect.DeepEqual(twoSpace, want) {
		t.Errorf("2-space document ilvl sequence = %v, want %v", twoSpace, want)
	}
	if !reflect.DeepEqual(fourSpace, want) {
		t.Errorf("4-space document ilvl sequence = %v, want %v", fourSpace, want)
	}
	if !reflect.DeepEqual(twoSpace, fourSpace) {
		t.Errorf("2-space and 4-space documents produced different ilvl sequences: %v vs %v", twoSpace, fourSpace)
	}
}

// A document that mixes indentation widths must still produce a
// predictable (if lossy) result: every item's level is its own leading-
// space count divided by the document-wide inferred unit (integer
// division), never a value that depends on encounter order or produces an
// out-of-range ilvl. Here the smallest positive indent is 2 (item "b"), so
// the unit is 2; item "c" at 3 spaces gets level 3/2 = 1 -- the same level
// as "b", a predictable rounding rather than a crash or a level 2/3 split
// that would depend on parse order.
func TestWrite_MixedIndentationRoundsPredictably(t *testing.T) {
	d, _, _ := writeAndReopen(t, "- a\n  - b\n   - c\n")
	doc, _ := d.Part(DocumentPart)
	s := string(doc)
	if got := strings.Count(s, `<w:ilvl w:val="1"/>`); got != 2 {
		t.Errorf("expected both the 2-space and 3-space items to round to ilvl 1 (count = %d): %s", got, s)
	}
}

// Ordered and unordered lists must reference different numIds -- sharing
// one (or pointing both at the same abstractNum) is exactly what makes an
// ordered list render as bullets instead of numbers.
func TestWrite_OrderedAndUnorderedUseDifferentNumIds(t *testing.T) {
	d, _, _ := writeAndReopen(t, "- bullet\n\n1. ordered\n")
	doc, _ := d.Part(DocumentPart)
	s := string(doc)
	if !strings.Contains(s, `<w:numId w:val="1"/>`) {
		t.Error("missing bullet numId 1")
	}
	if !strings.Contains(s, `<w:numId w:val="2"/>`) {
		t.Error("missing ordered numId 2")
	}

	num, ok := d.Part("word/numbering.xml")
	if !ok {
		t.Fatal("word/numbering.xml missing")
	}
	ns := string(num)
	if !strings.Contains(ns, `<w:num w:numId="1"><w:abstractNumId w:val="0"/></w:num>`) {
		t.Errorf("numId 1 does not map to abstractNumId 0: %s", ns)
	}
	if !strings.Contains(ns, `<w:num w:numId="2"><w:abstractNumId w:val="1"/></w:num>`) {
		t.Errorf("numId 2 does not map to abstractNumId 1: %s", ns)
	}
	if !strings.Contains(ns, `w:abstractNumId="0"`) || !strings.Contains(ns, `w:numFmt w:val="bullet"`) {
		t.Error("abstractNumId 0 is not a bullet list")
	}
	if !strings.Contains(ns, `w:abstractNumId="1"`) || !strings.Contains(ns, `w:numFmt w:val="decimal"`) {
		t.Error("abstractNumId 1 is not a decimal (ordered) list")
	}
}

// Missing either registration (Content_Types or document.xml.rels) makes
// Word declare the file corrupt -- both must be present, not just one.
func TestWrite_NumberingXMLIsDeclaredInContentTypes(t *testing.T) {
	d, _, _ := writeAndReopen(t, "- item\n")
	ct, ok := d.Part("[Content_Types].xml")
	if !ok {
		t.Fatal("[Content_Types].xml missing")
	}
	if !strings.Contains(string(ct), `PartName="/word/numbering.xml"`) {
		t.Error("[Content_Types].xml does not declare word/numbering.xml")
	}
	rels, ok := d.Part("word/_rels/document.xml.rels")
	if !ok {
		t.Fatal("word/_rels/document.xml.rels missing")
	}
	if !strings.Contains(string(rels), "numbering.xml") {
		t.Error("word/_rels/document.xml.rels does not register a relationship to numbering.xml")
	}
}

// The strongest test available: it uses Scan's independently-implemented
// Para.Cell coordinates (table index, row, column -- P1a.5) to verify the
// table WriteDocx just generated. The scanner and the generator were
// written independently, so their agreement is real evidence, not a
// restatement of what was just written.
func TestWrite_TableCellsAreScannedAsParagraphs(t *testing.T) {
	md := "| a | b |\n|---|---|\n| 1 | 2 |\n"
	d, _, _ := writeAndReopen(t, md)

	var cellParas []Para
	for _, p := range d.Paras() {
		if p.Cell != nil {
			cellParas = append(cellParas, p)
		}
	}
	if len(cellParas) != 4 {
		t.Fatalf("got %d cell paragraphs, want 4", len(cellParas))
	}
	wantCells := []CellRef{{Table: 1, Row: 1, Col: 1}, {Table: 1, Row: 1, Col: 2}, {Table: 1, Row: 2, Col: 1}, {Table: 1, Row: 2, Col: 2}}
	wantText := []string{"a", "b", "1", "2"}
	for i, p := range cellParas {
		if *p.Cell != wantCells[i] {
			t.Errorf("cellParas[%d].Cell = %+v, want %+v", i, *p.Cell, wantCells[i])
		}
		var text strings.Builder
		for _, r := range p.Runs {
			text.WriteString(r.Text)
		}
		if text.String() != wantText[i] {
			t.Errorf("cellParas[%d] text = %q, want %q", i, text.String(), wantText[i])
		}
	}
}

// An empty <w:tc> (no <w:p> at all) makes Word offer to repair the
// document -- this package hit that exact defect once already (a
// paragraph delete emptying a table cell). Every cell, including an empty
// one, must still scan as a paragraph.
func TestWrite_EmptyTableCellStillHasAParagraph(t *testing.T) {
	md := "| a | b |\n|---|---|\n| 1 |  |\n"
	d, _, _ := writeAndReopen(t, md)

	var cellParas []Para
	for _, p := range d.Paras() {
		if p.Cell != nil {
			cellParas = append(cellParas, p)
		}
	}
	if len(cellParas) != 4 {
		t.Fatalf("got %d cell paragraphs, want 4 (an empty cell must still produce a paragraph)", len(cellParas))
	}
	empty := cellParas[3]
	if empty.Cell.Row != 2 || empty.Cell.Col != 2 {
		t.Fatalf("expected the empty cell at row 2 col 2, got %+v", *empty.Cell)
	}
	if len(empty.Runs) != 0 {
		t.Errorf("empty cell has runs: %+v", empty.Runs)
	}
}

// The header row (the line before the GFM separator) is bold; data rows
// are not.
func TestWrite_TableHeaderRowIsBold(t *testing.T) {
	d, _, _ := writeAndReopen(t, "| a | b |\n|---|---|\n| 1 | 2 |\n")
	doc, _ := d.Part(DocumentPart)
	s := string(doc)

	firstTR := strings.Index(s, "<w:tr>")
	if firstTR < 0 {
		t.Fatal("no <w:tr> found")
	}
	secondTR := strings.Index(s[firstTR+len("<w:tr>"):], "<w:tr>")
	if secondTR < 0 {
		t.Fatal("only one <w:tr> found; expected header + data row")
	}
	secondTR += firstTR + len("<w:tr>")
	tblEnd := strings.Index(s[secondTR:], "</w:tbl>")
	if tblEnd < 0 {
		t.Fatal("no </w:tbl> found")
	}
	tblEnd += secondTR

	headerRow := s[firstTR:secondTR]
	dataRow := s[secondTR:tblEnd]
	if !strings.Contains(headerRow, "<w:b/>") {
		t.Errorf("header row is not bold: %s", headerRow)
	}
	if strings.Contains(dataRow, "<w:b/>") {
		t.Errorf("data row is unexpectedly bold: %s", dataRow)
	}
}

// GFM's alignment row (:---, :---:, ---:) must set <w:jc> on the cell's
// paragraph.
func TestWrite_TableAlignmentRowSetsJc(t *testing.T) {
	d, _, _ := writeAndReopen(t, "| L | C | R |\n|:---|:---:|---:|\n| a | b | c |\n")
	doc, _ := d.Part(DocumentPart)
	s := string(doc)
	if !strings.Contains(s, `<w:jc w:val="left"/>`) {
		t.Error("missing explicit left alignment from ':---'")
	}
	if !strings.Contains(s, `<w:jc w:val="center"/>`) {
		t.Error("missing center alignment from ':---:'")
	}
	if !strings.Contains(s, `<w:jc w:val="right"/>`) {
		t.Error("missing right alignment from '---:'")
	}
}

// Column count comes from the header row (2, here). A data row with an
// extra cell is truncated, and a data row with a missing cell is padded --
// both declared in Notes rather than silently changing the table shape.
func TestWrite_RaggedTableRowIsPaddedAndDeclared(t *testing.T) {
	md := "| a | b |\n|---|---|\n| 1 | 2 | 3 |\n| 4 |\n"
	d, res, _ := writeAndReopen(t, md)

	var cellParas []Para
	for _, p := range d.Paras() {
		if p.Cell != nil {
			cellParas = append(cellParas, p)
		}
	}
	// header (2) + row1 truncated to 2 + row2 padded to 2 = 6, never 3+1.
	if len(cellParas) != 6 {
		t.Fatalf("got %d cell paragraphs, want 6 (2 columns x 3 rows)", len(cellParas))
	}
	joined := strings.Join(res.Notes, " | ")
	if !strings.Contains(joined, "table") {
		t.Errorf("notes do not declare the ragged table: %q", joined)
	}
}

// A pipe-containing line with no GFM separator row right after it is not a
// real table (see the brief's self-review question). It must fall back to
// plain text -- neither structured into table cells nor declared in Notes,
// the same treatment any other unrecognized line already gets.
func TestWrite_PipeLineWithoutSeparatorIsNotATable(t *testing.T) {
	d, res, _ := writeAndReopen(t, "cost | benefit\nthis is not a separator row\n")
	for _, p := range d.Paras() {
		if p.Cell != nil {
			t.Errorf("a non-table pipe line was structured into a table cell: %+v", p)
		}
	}
	if strings.Contains(strings.Join(res.Notes, " | "), "table") {
		t.Error("a non-table pipe line must not produce a table note")
	}
}

// This test used to be named TestWrite_ListInterruptedThenResumedSharesNumId
// and pinned the OPPOSITE of what it asserts below: this package used one
// fixed numId per list kind for the WHOLE document (bulletNumID/
// orderedNumID in write.go, before Task 11), so two ordered lists separated
// by an intervening paragraph shared the same numId and their numbering
// silently CONTINUED rather than restarting -- e.g. a contract's numbered
// clauses, or a procedure's numbered steps, resuming after an unrelated
// paragraph would read "3. 4. 5." instead of restarting at "1. 2. 3.". The
// write-quality report's I7 judged that a real defect (this package's own
// report reasoned it was "deliberate", but a caller writing a real
// contract or procedure has no way to know that, and gets silently wrong
// output), so this test is rewritten rather than left pinning the bug: two
// ordered lists separated by a non-list block are now independent RUNS
// (computeListRuns), each getting its OWN, freshly allocated numId
// (assignListNumIDs) whose counter starts fresh at its abstractNum's own
// default of 1 -- a numId's counter never carries over from a different
// numId, even one referencing the SAME abstractNum.
func TestWrite_ListInterruptedThenResumedGetsFreshNumId(t *testing.T) {
	d, _, _ := writeAndReopen(t, "1. one\n\ntext in between\n\n1. two\n")
	doc, _ := d.Part(DocumentPart)
	s := string(doc)

	numIDRE := regexp.MustCompile(`<w:numId w:val="(\d+)"/>`)
	matches := numIDRE.FindAllStringSubmatch(s, -1)
	if len(matches) != 2 {
		t.Fatalf("got %d <w:numId> references, want 2 (one per ordered item): %s", len(matches), s)
	}
	if matches[0][1] == matches[1][1] {
		t.Errorf("both ordered items reference numId %s -- the second, interrupted-then-resumed list must get its own fresh numId instead of continuing the first's", matches[0][1])
	}

	num, ok := d.Part("word/numbering.xml")
	if !ok {
		t.Fatal("word/numbering.xml missing")
	}
	ns := string(num)
	for _, numID := range []string{matches[0][1], matches[1][1]} {
		want := `<w:num w:numId="` + numID + `"><w:abstractNumId w:val="1"/></w:num>`
		if !strings.Contains(ns, want) {
			t.Errorf("numbering.xml has no plain (no-override) entry for numId %s -- both items in the markdown started at \"1.\", so neither run should need a startOverride: %s", numID, ns)
		}
	}
}

// I7 also requires an ordered list's OWN starting number (e.g. a contract
// clause numbered "5. Fifth clause", continuing a numbering scheme this
// package never saw the earlier part of) to be honored rather than reset to
// 1 -- numbering.xml's <w:lvlOverride>/<w:startOverride> is exactly the
// OOXML mechanism for that, and assignListNumIDs/buildNumEntry are what
// wire an ordered run's own first item's number into it.
func TestWrite_OrderedListHonorsItsOwnStartingNumber(t *testing.T) {
	d, _, _ := writeAndReopen(t, "5. five\n6. six\n")
	doc, _ := d.Part(DocumentPart)
	s := string(doc)
	m := regexp.MustCompile(`<w:numId w:val="(\d+)"/>`).FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("no <w:numId> found in document.xml: %s", s)
	}
	numID := m[1]

	num, ok := d.Part("word/numbering.xml")
	if !ok {
		t.Fatal("word/numbering.xml missing")
	}
	ns := string(num)
	want := `<w:num w:numId="` + numID + `"><w:abstractNumId w:val="1"/>` +
		`<w:lvlOverride w:ilvl="0"><w:startOverride w:val="5"/></w:lvlOverride></w:num>`
	if !strings.Contains(ns, want) {
		t.Errorf("numbering.xml missing a startOverride=5 entry for numId %s: %s", numID, ns)
	}
}

// I6: each independent list infers its OWN indent unit (computeListRuns),
// not one unit shared across the whole document (the old
// inferListIndentUnit this replaces). A document combining a 4-space-nested
// list and a SEPARATE, later 2-space-nested list must still put BOTH nested
// items at ilvl 1 -- under the old whole-document inference, the smaller
// 2-space indent would have become the document's only unit, silently
// mis-leveling the 4-space list's nested item to ilvl 2 instead of 1 (the
// exact defect TestWrite_FourSpaceIndentMapsToLevelOne already pins for a
// document with only ONE list; this is the two-lists-in-one-document case
// that whole-document inference could not handle at all).
func TestWrite_TwoIndependentListsEachInferOwnIndentUnit(t *testing.T) {
	md := "- a\n    - nested four\n\ntext in between\n\n- b\n  - nested two\n"
	d, _, _ := writeAndReopen(t, md)
	doc, _ := d.Part(DocumentPart)
	s := string(doc)
	if got := strings.Count(s, `<w:ilvl w:val="1"/>`); got != 2 {
		t.Errorf("expected both nested items (4-space list and 2-space list) at ilvl 1 (count = %d): %s", got, s)
	}
	if strings.Contains(s, `<w:ilvl w:val="2"/>`) {
		t.Errorf("the 4-space list's nested item must not fall back to a document-wide 2-space unit and land at ilvl 2: %s", s)
	}
}

// I8: a list item's own indented continuation paragraph must stay attached
// to the list -- styled ListContinue and carrying a <w:numPr> that borrows
// the list's own per-level indent (continuationNumID) -- rather than
// falling out as an ordinary top-level BodyText paragraph with no list
// indent at all.
func TestWrite_ListItemContinuationStaysInListWithMatchingIndent(t *testing.T) {
	md := "- top item\n    continuation still about top item\n"
	d, _, _ := writeAndReopen(t, md)
	doc, _ := d.Part(DocumentPart)
	s := string(doc)

	contPPr := regexp.MustCompile(`<w:pPr><w:pStyle w:val="ListContinue"/>.*?</w:pPr>`).FindString(s)
	if contPPr == "" {
		t.Fatalf("no ListContinue paragraph found in document.xml: %s", s)
	}
	if !strings.Contains(contPPr, "<w:numPr>") {
		t.Errorf("ListContinue paragraph missing <w:numPr> (the list indent it should borrow): %s", contPPr)
	}
	if !strings.Contains(contPPr, `<w:ilvl w:val="0"/>`) {
		t.Errorf("ListContinue paragraph's numPr should be at ilvl 0, matching the item it continues: %s", contPPr)
	}

	// The continuation's indent (abstractNumId 2, continuationAbstractNumXML)
	// must equal the list item's own text indent (abstractNumId 0's ilvl=0
	// <w:ind>), or it would not visually align with the item it continues.
	num, _ := d.Part("word/numbering.xml")
	ns := string(num)
	bulletLvl0 := regexp.MustCompile(`<w:abstractNum w:abstractNumId="0">.*?<w:lvl w:ilvl="0">.*?<w:ind w:left="(\d+)"`).FindStringSubmatch(ns)
	contLvl0 := regexp.MustCompile(`<w:abstractNum w:abstractNumId="2">.*?<w:lvl w:ilvl="0">.*?<w:ind w:left="(\d+)"`).FindStringSubmatch(ns)
	if bulletLvl0 == nil || contLvl0 == nil {
		t.Fatalf("could not extract ilvl=0 indents from numbering.xml: %s", ns)
	}
	if bulletLvl0[1] != contLvl0[1] {
		t.Errorf("continuation indent (%s) does not match the list item's own text indent (%s)", contLvl0[1], bulletLvl0[1])
	}
}

// I8: a FENCED code block inside a list item keeps the list's own indent
// (borrowed via the same continuationNumID mechanism as a continuation
// paragraph), instead of sitting flush with the page margin the way a
// top-level code block does.
func TestWrite_FencedCodeInsideListItemKeepsListIndent(t *testing.T) {
	md := "- top\n  ```\n  code\n  ```\n"
	d, _, _ := writeAndReopen(t, md)
	doc, _ := d.Part(DocumentPart)
	s := string(doc)

	codePPr := regexp.MustCompile(`<w:pPr><w:pStyle w:val="SourceCode"/>.*?</w:pPr>`).FindString(s)
	if codePPr == "" {
		t.Fatalf("no SourceCode paragraph found in document.xml: %s", s)
	}
	if !strings.Contains(codePPr, "<w:numPr>") {
		t.Errorf("in-list fenced code block is missing <w:numPr>; the list's indent was not preserved: %s", codePPr)
	}
	if !strings.Contains(codePPr, fmt.Sprintf(`<w:numId w:val="%d"/>`, continuationNumID)) {
		t.Errorf("in-list fenced code block's numPr does not reference continuationNumID: %s", codePPr)
	}
}

// Review round-2 finding (a4): a fence delimiter used to be entirely
// exempt from the hasLeadingIndent-based "list ends here" check every
// other line goes through (it always `continue`s before ever reaching
// it), so a TOP-LEVEL (unindented) fenced code block sitting between two
// otherwise-unrelated ordered lists left inListContext/computeListRuns'
// activeRun stuck true across it -- silently merging the second list into
// the first one's run and so its numId, exactly the I7 defect this task
// otherwise fixes. Both lists here start "1.", so under the bug they
// would render as a single continuing sequence ("1. 2.") instead of each
// restarting.
func TestWrite_TopLevelFencedCodeBetweenListsDoesNotMergeTheirNumIds(t *testing.T) {
	md := "1. one\n\n```\ntop level code\n```\n\n1. two\n"
	d, _, _ := writeAndReopen(t, md)
	doc, _ := d.Part(DocumentPart)
	s := string(doc)

	matches := regexp.MustCompile(`<w:numId w:val="(\d+)"/>`).FindAllStringSubmatch(s, -1)
	if len(matches) != 2 {
		t.Fatalf("got %d <w:numId> references, want 2 (one per ordered item): %s", len(matches), s)
	}
	if matches[0][1] == matches[1][1] {
		t.Errorf("both ordered items reference numId %s across a top-level fenced code block -- they must get independent numIds, not be merged into one run", matches[0][1])
	}
}

// Review round-2 finding (c3): the same stuck-true inListContext bug also
// wrongly tagged a top-level fenced code block codeInList (and so gave it
// a borrowed list indent it must not have) whenever it happened to follow
// a list with nothing else in between -- a regression Task 11 itself
// introduced (before this task, appendCode never wrote a <w:numPr> at
// all, so this specific shape had no visible defect). Only a fence
// delimiter that is ITSELF indented (genuinely inside a list item) should
// get that treatment -- see TestWrite_FencedCodeInsideListItemKeepsListIndent.
func TestWrite_TopLevelFencedCodeAfterListIsNotTaggedCodeInList(t *testing.T) {
	md := "- item\n\n```\ntop level code\n```\n"
	d, _, _ := writeAndReopen(t, md)
	doc, _ := d.Part(DocumentPart)
	s := string(doc)

	codePPr := regexp.MustCompile(`<w:pPr><w:pStyle w:val="SourceCode"/>.*?</w:pPr>`).FindString(s)
	if codePPr == "" {
		t.Fatalf("no SourceCode paragraph found in document.xml: %s", s)
	}
	if strings.Contains(codePPr, "<w:numPr>") {
		t.Errorf("a top-level fenced code block was wrongly tagged codeInList (carries a <w:numPr>): %s", codePPr)
	}
}

// Review round-2 finding: allocateOrderedNumID is the one place an ordered
// run's numId is ever minted, and must never hand out continuationNumID
// itself -- doing so would produce numbering.xml with TWO
// <w:num w:numId="1000"> entries (one for the ordinary run, one for
// continuationNumID's own always-present entry), which is invalid OOXML.
// Checked directly against the pure function rather than by actually
// building a ~1000-list document to reach the collision.
func TestWrite_OrderedNumIDAllocationNeverCollidesWithContinuationNumID(t *testing.T) {
	id, following := allocateOrderedNumID(continuationNumID)
	if id == continuationNumID {
		t.Errorf("allocateOrderedNumID(%d) = %d, want it to skip continuationNumID", continuationNumID, id)
	}
	if following == continuationNumID {
		t.Errorf("allocateOrderedNumID(%d) following = %d, want it to also skip continuationNumID for the NEXT allocation", continuationNumID, following)
	}

	// The ordinary case (nowhere near the reserved id) must still just be
	// (n, n+1) -- the skip must not perturb every other allocation.
	if id2, following2 := allocateOrderedNumID(5); id2 != 5 || following2 != 6 {
		t.Errorf("allocateOrderedNumID(5) = (%d, %d), want (5, 6)", id2, following2)
	}
}

// Review round-2 finding: continuationAbstractNumXML's per-level <w:ind>
// carries no w:hanging (see its own doc comment for why), which means
// CT_Lvl's w:suff -- defaulting to "tab" -- would otherwise push a
// continuation paragraph's first line out to the next tab stop instead of
// leaving it at w:left, re-breaking the very alignment
// TestWrite_ListItemContinuationStaysInListWithMatchingIndent checks.
// codeInList's SourceCode paragraphs borrow this SAME abstractNum, so this
// covers both.
func TestWrite_ContinuationAbstractNumSuppressesTabSuffix(t *testing.T) {
	d, _, _ := writeAndReopen(t, "- top item\n    continuation still about top item\n")
	num, ok := d.Part("word/numbering.xml")
	if !ok {
		t.Fatal("word/numbering.xml missing")
	}
	ns := string(num)

	m := regexp.MustCompile(`(?s)<w:abstractNum w:abstractNumId="2">(.*?)</w:abstractNum>`).FindStringSubmatch(ns)
	if m == nil {
		t.Fatalf("abstractNumId=2 (the continuation abstractNum) not found: %s", ns)
	}
	if got, want := strings.Count(m[1], `<w:suff w:val="nothing"/>`), maxListLevel+1; got != want {
		t.Errorf("abstractNumId=2 has %d <w:suff w:val=\"nothing\"/> entries, want %d (one per level 0..%d)", got, want, maxListLevel)
	}
}

// Inline emphasis must still resolve inside a list item, the same as any
// other paragraph.
func TestWrite_ListItemRunsInlineEmphasis(t *testing.T) {
	d, _, _ := writeAndReopen(t, "- plain **bold** end\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	if text.String() != "plain bold end" {
		t.Errorf("visible text = %q, want markers stripped", text.String())
	}
	doc, _ := d.Part(DocumentPart)
	if !strings.Contains(string(doc), "<w:b/>") {
		t.Error("bold marker inside a list item did not produce a bold run")
	}
}

// --- Task 3: code blocks, inline code, links, block quotes, horizontal rules ---

// A fenced code block becomes one paragraph per line, referencing the
// SourceCode style (monospace font + light shading -- see styles.go), not
// carrying either property inline. The fence delimiters themselves are
// consumed, never becoming a paragraph of their own.
func TestWrite_FencedCodeBlockUsesMonospaceAndShading(t *testing.T) {
	d, _, _ := writeAndReopen(t, "```\nfunc main() {}\n```\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1 (fence delimiters must not become paragraphs): %+v", len(paras), paras)
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	if text.String() != "func main() {}" {
		t.Errorf("code text = %q, want %q", text.String(), "func main() {}")
	}
	doc, _ := d.Part(DocumentPart)
	s := string(doc)
	if !strings.Contains(s, `<w:pStyle w:val="SourceCode"/>`) {
		t.Error("code paragraph does not reference the SourceCode style")
	}
	styles, _ := d.Part("word/styles.xml")
	sc := styleBlock(t, styles, "SourceCode")
	if !strings.Contains(sc, `w:fill="F5F5F5"`) {
		t.Error("SourceCode style has no light shading")
	}
	if !strings.Contains(sc, `w:ascii="Consolas"`) {
		t.Error("SourceCode style is not in a monospace font")
	}
}

// Leading whitespace inside a fenced code block must survive exactly,
// which requires xml:space="preserve" on the <w:t> (Word otherwise
// collapses leading/trailing whitespace).
func TestWrite_FencedCodeBlockPreservesLeadingWhitespace(t *testing.T) {
	d, _, _ := writeAndReopen(t, "```\n    indented line\n```\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	if text.String() != "    indented line" {
		t.Errorf("code text = %q, want leading spaces preserved", text.String())
	}
	doc, _ := d.Part(DocumentPart)
	if !strings.Contains(string(doc), `xml:space="preserve"`) {
		t.Error("indented code line has no xml:space=\"preserve\"; Word would collapse the leading spaces")
	}
}

// Markdown is deliberately NOT interpreted inside a fenced code block:
// "**bold**" must stay completely literal, never becoming a <w:b/> run.
func TestWrite_FencedCodeBlockDoesNotInterpretMarkdown(t *testing.T) {
	d, _, _ := writeAndReopen(t, "```\n**not bold** and `not inline code either`\n```\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	want := "**not bold** and `not inline code either`"
	if text.String() != want {
		t.Errorf("code text = %q, want %q (markers must survive literally)", text.String(), want)
	}
	doc, _ := d.Part(DocumentPart)
	if strings.Contains(string(doc), "<w:b/>") {
		t.Error("markdown was interpreted inside a fenced code block")
	}
}

// An info string after the opening fence (the "go" in "```go") is ignored,
// not an error, and must never leak into the rendered document as text.
func TestWrite_FenceInfoStringIsIgnoredNotError(t *testing.T) {
	d, res, _ := writeAndReopen(t, "```go\nfmt.Println(\"hi\")\n```\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	if text.String() != `fmt.Println("hi")` {
		t.Errorf("code text = %q, want the code line only", text.String())
	}
	doc, _ := d.Part(DocumentPart)
	if strings.Contains(string(doc), "```") || strings.Contains(string(doc), ">go<") {
		t.Errorf("fence delimiter or info string leaked into the document: %s", string(doc))
	}
	if len(res.Notes) != 0 {
		t.Errorf("Notes = %v, want none (an info string is not an error)", res.Notes)
	}
}

// An unterminated fence runs to the end of the document -- matching
// CommonMark's own rule for an unclosed fenced code block, not a defect
// unique to this package (see buildBlocks' inFence branch). Everything
// after the opening fence, headings included, becomes literal code.
func TestWrite_UnterminatedFenceRunsToEndOfDocument(t *testing.T) {
	// The trailing "\n" splits into one extra, empty final line -- still
	// inside the never-closed fence, so it becomes its own (empty) code
	// LINE too, per the same "every line inside a fence, blank ones
	// included, is part of the block" rule this file's inFence branch
	// documents. As of the code-single-paragraph task (write.go's
	// mergeCodeBlocks/renderCodeBlockRuns) all four lines collapse into
	// ONE SourceCode paragraph, recovered here via paraTextWithBreaks
	// rather than as four separate paragraphs.
	d, _, _ := writeAndReopen(t, "```\nfoo\n# Heading\nbar\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1 (the whole unterminated fence is one code block/paragraph)", len(paras))
	}
	if paras[0].Style != StyleSourceCode {
		t.Fatalf("paras[0].Style = %q, want %q", paras[0].Style, StyleSourceCode)
	}
	got := strings.Split(paraTextWithBreaks(paras[0]), "\n")
	want := []string{"foo", "# Heading", "bar", ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("code block lines = %q, want %q", got, want)
	}
}

// Inline code (backticks) becomes a monospace run via the VerbatimChar
// character style, not a whole-paragraph treatment and not a direct
// <w:rFonts> -- it must sit inline among ordinary text.
func TestWrite_InlineCodeBecomesMonospaceRun(t *testing.T) {
	d, _, _ := writeAndReopen(t, "before `code` after\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	if text.String() != "before code after" {
		t.Errorf("visible text = %q, want backticks stripped", text.String())
	}
	doc, _ := d.Part(DocumentPart)
	if !strings.Contains(string(doc), `<w:rStyle w:val="VerbatimChar"/>`) {
		t.Error("inline code run does not reference the VerbatimChar character style")
	}
	styles, _ := d.Part("word/styles.xml")
	vc := styleBlock(t, styles, "VerbatimChar")
	if !strings.Contains(vc, `w:ascii="Consolas"`) {
		t.Error("VerbatimChar style is not in a monospace font")
	}
}

// Inline code must not swallow the text around it -- two separate code
// spans in one paragraph must leave the plain text between and around them
// intact.
func TestWrite_InlineCodeDoesNotSwallowSurroundingText(t *testing.T) {
	d, _, _ := writeAndReopen(t, "a `x` b `y` c\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	if text.String() != "a x b y c" {
		t.Errorf("visible text = %q, want %q", text.String(), "a x b y c")
	}
	if len(paras[0].Runs) < 5 {
		t.Errorf("got %d runs, want at least 5 (a / x / b / y / c as separate runs)", len(paras[0].Runs))
	}
}

// A [text](url) link becomes a real hyperlink: a relationship with
// TargetMode="External" in document.xml.rels, and <w:hyperlink r:id="..">
// around the run in the body.
func TestWrite_LinkBecomesHyperlinkWithExternalRelationship(t *testing.T) {
	d, _, _ := writeAndReopen(t, "See [Example](https://example.com/page) for more.\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	if text.String() != "See Example for more." {
		t.Errorf("visible text = %q, want the [ ]( ) markers stripped", text.String())
	}

	doc, _ := d.Part(DocumentPart)
	docStr := string(doc)
	re := regexp.MustCompile(`<w:hyperlink r:id="(rId\d+)">`)
	m := re.FindStringSubmatch(docStr)
	if m == nil {
		t.Fatalf("no <w:hyperlink r:id=\"...\"> found: %s", docStr)
	}
	rid := m[1]

	rels, ok := d.Part("word/_rels/document.xml.rels")
	if !ok {
		t.Fatal("word/_rels/document.xml.rels missing")
	}
	relsStr := string(rels)
	if !strings.Contains(relsStr, `Id="`+rid+`"`) {
		t.Errorf("relationship %s referenced by the body is not declared in document.xml.rels: %s", rid, relsStr)
	}
	if !strings.Contains(relsStr, `Target="https://example.com/page"`) {
		t.Errorf("relationship does not target the link's URL: %s", relsStr)
	}
	if !strings.Contains(relsStr, `TargetMode="External"`) {
		t.Error("hyperlink relationship is missing TargetMode=\"External\"")
	}
}

// The Hyperlink character style referenced by <w:rStyle> must actually be
// defined in styles.xml, or Word renders the link as ordinary text -- the
// same "looks like it worked but didn't" trap as an undefined heading
// style.
func TestWrite_HyperlinkStyleIsDefinedInStylesXML(t *testing.T) {
	d, _, _ := writeAndReopen(t, "[a](https://example.com)\n")
	s, ok := d.Part("word/styles.xml")
	if !ok {
		t.Fatal("styles.xml missing")
	}
	if !strings.Contains(string(s), `w:styleId="Hyperlink"`) {
		t.Error("styles.xml does not define Hyperlink; Word would render the link as plain text")
	}
	doc, _ := d.Part(DocumentPart)
	if !strings.Contains(string(doc), `<w:rStyle w:val="Hyperlink"/>`) {
		t.Error("link run does not reference the Hyperlink character style")
	}
}

// A link inside a list item must still become a real hyperlink -- the
// list-item paragraph and an ordinary paragraph share the same
// renderParagraph path.
func TestWrite_LinkInsideListItemGetsAHyperlink(t *testing.T) {
	d, _, _ := writeAndReopen(t, "- see [docs](https://example.com/docs)\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	if text.String() != "see docs" {
		t.Errorf("visible text = %q, want %q", text.String(), "see docs")
	}
	doc, _ := d.Part(DocumentPart)
	s := string(doc)
	if !strings.Contains(s, "<w:hyperlink") {
		t.Error("no hyperlink inside the list item")
	}
	if !strings.Contains(s, "<w:numPr>") {
		t.Error("list item lost its <w:numPr> alongside the link")
	}
}

// A link inside a table cell must still become a real hyperlink, verified
// through the same Para.Cell cross-check Task 2 used for plain table text.
func TestWrite_LinkInsideTableCellGetsAHyperlink(t *testing.T) {
	md := "| a | b |\n|---|---|\n| [x](https://example.com/x) | y |\n"
	d, _, _ := writeAndReopen(t, md)

	var cellParas []Para
	for _, p := range d.Paras() {
		if p.Cell != nil {
			cellParas = append(cellParas, p)
		}
	}
	if len(cellParas) != 4 {
		t.Fatalf("got %d cell paragraphs, want 4", len(cellParas))
	}
	linkCell := cellParas[2] // row 2, col 1
	if linkCell.Cell.Row != 2 || linkCell.Cell.Col != 1 {
		t.Fatalf("expected the link at row 2 col 1, got %+v", *linkCell.Cell)
	}
	var text strings.Builder
	for _, r := range linkCell.Runs {
		text.WriteString(r.Text)
	}
	if text.String() != "x" {
		t.Errorf("cell text = %q, want %q", text.String(), "x")
	}

	doc, _ := d.Part(DocumentPart)
	if !strings.Contains(string(doc), "<w:hyperlink") {
		t.Error("no hyperlink found anywhere in the document; a link inside a table cell did not render")
	}
}

// Many links in one document must each get their own, never-repeated
// relationship id -- this is the failure mode "relationship ids collide"
// would produce (Word treating two different links as the same target, or
// refusing to open the file over a duplicate id).
func TestWrite_ManyLinksGetUniqueRelationshipIds(t *testing.T) {
	md := "[one](https://example.com/1) [two](https://example.com/2) " +
		"[three](https://example.com/3) [four](https://example.com/4)\n"
	d, _, _ := writeAndReopen(t, md)
	doc, _ := d.Part(DocumentPart)
	re := regexp.MustCompile(`<w:hyperlink r:id="(rId\d+)">`)
	matches := re.FindAllStringSubmatch(string(doc), -1)
	if len(matches) != 4 {
		t.Fatalf("got %d hyperlinks, want 4", len(matches))
	}
	seen := map[string]bool{}
	for _, m := range matches {
		id := m[1]
		if seen[id] {
			t.Errorf("relationship id %s used by more than one hyperlink", id)
		}
		seen[id] = true
		if id == "rId1" || id == "rId2" {
			t.Errorf("hyperlink reused a reserved id (%s belongs to styles.xml/numbering.xml)", id)
		}
	}
	if len(seen) != 4 {
		t.Errorf("got %d distinct relationship ids, want 4", len(seen))
	}

	rels, _ := d.Part("word/_rels/document.xml.rels")
	relsStr := string(rels)
	for id := range seen {
		if !strings.Contains(relsStr, `Id="`+id+`"`) {
			t.Errorf("relationship %s referenced by the body is not declared: %s", id, relsStr)
		}
	}
}

// rId1 and rId2 are permanently reserved for styles.xml and numbering.xml;
// a document with links must not repurpose or collide with either.
func TestWrite_LinkRelationshipIdsDoNotCollideWithStylesOrNumbering(t *testing.T) {
	d, _, _ := writeAndReopen(t, "[a](https://example.com/a)\n")
	rels, _ := d.Part("word/_rels/document.xml.rels")
	relsStr := string(rels)
	if !strings.Contains(relsStr, `Id="rId1"`) || !strings.Contains(relsStr, `styles.xml`) {
		t.Error("rId1/styles.xml relationship is missing or was overwritten")
	}
	if !strings.Contains(relsStr, `Id="rId2"`) || !strings.Contains(relsStr, `numbering.xml`) {
		t.Error("rId2/numbering.xml relationship is missing or was overwritten")
	}
	if strings.Contains(relsStr, `Id="rId1"`) && strings.Count(relsStr, `Id="rId1"`) != 1 {
		t.Error("rId1 is declared more than once")
	}
}

// A "> " line becomes a paragraph referencing the Quote style, which
// supplies the left indent and left border (styles.go) -- neither is
// written inline on the paragraph itself.
func TestWrite_BlockQuoteGetsLeftBorderAndIndent(t *testing.T) {
	d, _, _ := writeAndReopen(t, "> quoted text\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	if text.String() != "quoted text" {
		t.Errorf("visible text = %q, want %q", text.String(), "quoted text")
	}
	doc, _ := d.Part(DocumentPart)
	s := string(doc)
	if !strings.Contains(s, `<w:pStyle w:val="Quote"/>`) {
		t.Error("block quote paragraph does not reference the Quote style")
	}
	styles, _ := d.Part("word/styles.xml")
	q := styleBlock(t, styles, "Quote")
	if !strings.Contains(q, "<w:pBdr><w:left") {
		t.Error("Quote style has no left border")
	}
	if !strings.Contains(q, "<w:ind ") {
		t.Error("Quote style has no left indent")
	}
}

// A standalone "---" becomes an empty paragraph with a bottom border.
func TestWrite_HorizontalRuleGetsBottomBorderedEmptyParagraph(t *testing.T) {
	d, _, _ := writeAndReopen(t, "before\n\n---\n\nafter\n")
	paras := d.Paras()
	if len(paras) != 3 {
		t.Fatalf("got %d paragraphs, want 3 (before / rule / after)", len(paras))
	}
	rule := paras[1]
	if len(rule.Runs) != 0 {
		t.Errorf("horizontal rule paragraph has %d runs, want 0", len(rule.Runs))
	}
	doc, _ := d.Part(DocumentPart)
	if !strings.Contains(string(doc), "<w:pBdr><w:bottom") {
		t.Error("horizontal rule paragraph has no bottom border")
	}
}

// A GFM table separator row ("|---|---|") must never be mistaken for a
// horizontal rule: it is consumed entirely by table parsing before the
// horizontal-rule check ever runs (buildBlocks' table branch always has
// first refusal), so the table must come through completely intact.
func TestWrite_HorizontalRuleDoesNotEatTableSeparatorRow(t *testing.T) {
	d, _, _ := writeAndReopen(t, "| a | b |\n|---|---|\n| 1 | 2 |\n")
	var cellParas int
	for _, p := range d.Paras() {
		if p.Cell != nil {
			cellParas++
		}
	}
	if cellParas != 4 {
		t.Fatalf("got %d cell paragraphs, want 4 -- the table separator row was swallowed as a horizontal rule", cellParas)
	}
	doc, _ := d.Part(DocumentPart)
	if strings.Contains(string(doc), "<w:pBdr><w:bottom") {
		t.Error("a table separator row produced a horizontal rule's bottom border")
	}
}

// A bare "---" directly under a paragraph line, with no blank line between
// them, is a setext heading underline in CommonMark -- not a horizontal
// rule. Superseded by the M1 fix (task-5-brief.md): this used to pin the
// pre-fix fallback of printing the whole thing as one literal paragraph
// ("Heading ---"), which was itself the M1 defect this task closes; now it
// pins the actual CommonMark-correct outcome, a real Heading2, while still
// checking the negative half of the original assertion (never rendered as
// a horizontal rule either) -- see TestWrite_SetextH2BecomesHeading2 and
// TestWrite_TableSetextAndHRDoNotMisfireOnEachOther for the fuller Setext
// coverage this test's name predates.
func TestWrite_HorizontalRuleDoesNotEatSetextHeadingUnderline(t *testing.T) {
	d, _, _ := writeAndReopen(t, "Heading\n---\n\nNext para\n")
	paras := d.Paras()
	if len(paras) != 2 {
		t.Fatalf("got %d paragraphs, want 2 (the setext Heading2, then \"Next para\"): %+v", len(paras), paras)
	}
	if paras[0].Style != "Heading2" {
		t.Errorf("paras[0].Style = %q, want Heading2 (a setext underline must produce a real heading)", paras[0].Style)
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	if text.String() != "Heading" {
		t.Errorf("paras[0] text = %q, want %q (must not contain the literal '---' underline)", text.String(), "Heading")
	}
	doc, _ := d.Part(DocumentPart)
	if strings.Contains(string(doc), "<w:pBdr><w:bottom") {
		t.Error("a setext-style underline was rendered as a horizontal rule")
	}
}

// --- Final acceptance: a realistic design-document markdown survives the
// round trip through WriteDocx and this package's own OpenDocument/Scan. ---

// This is the closest automated proxy this package has for "would a
// person accept this document": a markdown document shaped like something
// a model would actually write (title, several heading levels, bold and
// inline code inside a paragraph, a nested list, a GFM table with
// alignment, a fenced code block, a link, and a block quote), generated
// through the whole WriteDocx pipeline and then read back with the
// package's own reader -- never asserting on the XML this task just wrote,
// only on what an independent reader (Scan, written before this task
// existed) resolves it to.
func TestWrite_RealisticDesignDocumentSurvivesRoundTrip(t *testing.T) {
	md := "# Project Zephyr Design\n\n" +
		"## Overview\n\n" +
		"This document describes **Project Zephyr**, a service for `zephyr-cli` users.\n\n" +
		"### Goals\n\n" +
		"- Reduce latency\n" +
		"  - Cache hot paths\n" +
		"  - Precompute indexes\n" +
		"- Support offline mode\n\n" +
		"## Data Model\n\n" +
		"| Field | Type | Notes |\n" +
		"|---|:---:|---:|\n" +
		"| id | string | primary key |\n" +
		"| ttl | int | seconds |\n\n" +
		"## Example\n\n" +
		"```go\n" +
		"func Run() error {\n" +
		"    return nil\n" +
		"}\n" +
		"```\n\n" +
		"See the [full spec](https://example.com/spec) for details.\n\n" +
		"> Note: this is still a draft.\n"

	d, res, _ := writeAndReopen(t, md)
	paras := d.Paras()

	textOf := func(p Para) string {
		var b strings.Builder
		for _, r := range p.Runs {
			b.WriteString(r.Text)
		}
		return b.String()
	}

	// A fully-supported document (no images) must produce no notes: this
	// is the "good enough that a model reaches for this instead of a
	// fallback" bar the brief sets, and a stray note here would mean this
	// package quietly failed to render something a real design doc needs.
	if len(res.Notes) != 0 {
		t.Errorf("Notes = %v, want none for this fully-supported document", res.Notes)
	}

	// Heading styles and text, at every level this document uses.
	var h1, h3 []Para
	h2ByText := map[string]Para{}
	for _, p := range paras {
		switch p.Style {
		case "Heading1":
			h1 = append(h1, p)
		case "Heading2":
			h2ByText[textOf(p)] = p
		case "Heading3":
			h3 = append(h3, p)
		}
	}
	if len(h1) != 1 || textOf(h1[0]) != "Project Zephyr Design" {
		t.Errorf("Heading1 paragraphs = %v, want exactly one reading %q", h1, "Project Zephyr Design")
	}
	for _, want := range []string{"Overview", "Data Model", "Example"} {
		if _, ok := h2ByText[want]; !ok {
			t.Errorf("no Heading2 paragraph reads %q", want)
		}
	}
	if len(h3) != 1 || textOf(h3[0]) != "Goals" {
		t.Errorf("Heading3 paragraphs = %v, want exactly one reading %q", h3, "Goals")
	}

	// Bold and inline code inside the same ordinary paragraph.
	var overviewBody Para
	for _, p := range paras {
		if textOf(p) == "This document describes Project Zephyr, a service for zephyr-cli users." {
			overviewBody = p
		}
	}
	if overviewBody.Runs == nil {
		t.Fatal("could not find the overview paragraph with markers stripped")
	}
	doc, _ := d.Part(DocumentPart)
	docStr := string(doc)
	if !strings.Contains(docStr, "<w:b/>") {
		t.Error("no bold run found anywhere (expected from **Project Zephyr**)")
	}
	if !strings.Contains(docStr, `<w:rStyle w:val="VerbatimChar"/>`) {
		t.Error("no inline-code run referencing VerbatimChar found (expected from `zephyr-cli`)")
	}
	if !strings.Contains(docStr, `<w:pStyle w:val="SourceCode"/>`) {
		t.Error("no code-block paragraph referencing SourceCode found (expected from the fenced code block)")
	}
	styles, _ := d.Part("word/styles.xml")
	if !strings.Contains(string(styles), `w:ascii="Consolas"`) {
		t.Error("no monospace font declared anywhere in styles.xml (expected on VerbatimChar/SourceCode)")
	}

	// Nested list: two distinct texts at ilvl 1, matching the two
	// sub-bullets, plus ilvl 0 for the two top-level items.
	ilvlRE := regexp.MustCompile(`<w:ilvl w:val="(\d+)"/>`)
	levels := ilvlRE.FindAllStringSubmatch(docStr, -1)
	if len(levels) != 4 {
		t.Fatalf("got %d list items with an <w:ilvl>, want 4", len(levels))
	}
	wantLevels := []string{"0", "1", "1", "0"}
	for i, m := range levels {
		if m[1] != wantLevels[i] {
			t.Errorf("list item %d has ilvl %s, want %s", i, m[1], wantLevels[i])
		}
	}
	foundSub := map[string]bool{}
	for _, p := range paras {
		t := textOf(p)
		if t == "Cache hot paths" || t == "Precompute indexes" {
			foundSub[t] = true
		}
	}
	if !foundSub["Cache hot paths"] || !foundSub["Precompute indexes"] {
		t.Errorf("nested list item text missing: %+v", foundSub)
	}

	// Table cell coordinates and alignment.
	var cellParas []Para
	for _, p := range paras {
		if p.Cell != nil {
			cellParas = append(cellParas, p)
		}
	}
	if len(cellParas) != 9 {
		t.Fatalf("got %d table cell paragraphs, want 9 (3 columns x 3 rows)", len(cellParas))
	}
	wantCells := map[CellRef]string{
		{Table: 1, Row: 1, Col: 1}: "Field",
		{Table: 1, Row: 1, Col: 2}: "Type",
		{Table: 1, Row: 1, Col: 3}: "Notes",
		{Table: 1, Row: 2, Col: 1}: "id",
		{Table: 1, Row: 2, Col: 2}: "string",
		{Table: 1, Row: 2, Col: 3}: "primary key",
		{Table: 1, Row: 3, Col: 1}: "ttl",
		{Table: 1, Row: 3, Col: 2}: "int",
		{Table: 1, Row: 3, Col: 3}: "seconds",
	}
	for _, p := range cellParas {
		want, ok := wantCells[*p.Cell]
		if !ok {
			t.Errorf("unexpected cell coordinate %+v", *p.Cell)
			continue
		}
		if got := textOf(p); got != want {
			t.Errorf("cell %+v text = %q, want %q", *p.Cell, got, want)
		}
	}
	if !strings.Contains(docStr, `<w:jc w:val="center"/>`) {
		t.Error("no center-aligned table cell found (expected from the :---: Type column)")
	}
	if !strings.Contains(docStr, `<w:jc w:val="right"/>`) {
		t.Error("no right-aligned table cell found (expected from the ---: Notes column)")
	}

	// Fenced code block: three literal lines, markdown not interpreted, no
	// fence marker or info string leaked into the text. As of the
	// code-single-paragraph task the whole block is one SourceCode
	// paragraph (write.go's mergeCodeBlocks/renderCodeBlockRuns), its
	// three lines joined by <w:br/> rather than split across three
	// paragraphs -- recovered here via paraTextWithBreaks.
	var codeBlockText string
	var foundCodeBlock bool
	for _, p := range paras {
		if p.Style == StyleSourceCode {
			codeBlockText = paraTextWithBreaks(p)
			foundCodeBlock = true
			break
		}
	}
	if !foundCodeBlock {
		t.Fatal("no SourceCode paragraph found in the round-tripped document")
	}
	if want := "func Run() error {\n    return nil\n}"; codeBlockText != want {
		t.Errorf("code block text = %q, want %q", codeBlockText, want)
	}
	if strings.Contains(docStr, "```") {
		t.Error("a fence delimiter leaked into the rendered document")
	}

	// Link: hyperlink relationship + visible text with markers stripped.
	if !strings.Contains(docStr, "<w:hyperlink") {
		t.Error("no hyperlink found")
	}
	rels, _ := d.Part("word/_rels/document.xml.rels")
	if !strings.Contains(string(rels), `Target="https://example.com/spec"`) {
		t.Error("hyperlink relationship does not target the expected URL")
	}
	foundLinkPara := false
	for _, p := range paras {
		if textOf(p) == "See the full spec for details." {
			foundLinkPara = true
		}
	}
	if !foundLinkPara {
		t.Error("could not find the paragraph containing the link with markers stripped")
	}

	// Block quote.
	foundQuote := false
	for _, p := range paras {
		if textOf(p) == "Note: this is still a draft." {
			foundQuote = true
		}
	}
	if !foundQuote {
		t.Error("could not find the block quote paragraph")
	}
	if !strings.Contains(docStr, `<w:pStyle w:val="Quote"/>`) {
		t.Error("block quote paragraph does not reference the Quote style")
	}
	if !strings.Contains(string(styles), "<w:pBdr><w:left") {
		t.Error("no block quote left border found in the Quote style")
	}
}

// --- Task 2 of the docx-style-architecture plan: document.xml references
// named styles instead of carrying visual properties inline. ---

// generateAndReadDocumentXML writes md through WriteDocx and returns
// word/document.xml as a string, the one part the core invariant below
// inspects.
func generateAndReadDocumentXML(t *testing.T, md string) string {
	t.Helper()
	d, _, _ := writeAndReopen(t, md)
	doc, ok := d.Part(DocumentPart)
	if !ok {
		t.Fatal("word/document.xml missing")
	}
	return string(doc)
}

// TestWrite_NoInlineVisualPropertiesInDocumentXML is the core, deliberately
// exhaustive invariant this task exists to establish: document.xml must
// never carry a paragraph-level visual property (<w:spacing>, <w:ind>,
// <w:shd>, or <w:pBdr>) inline outside a few narrow, deliberate exceptions.
// Those properties belong in styles.xml, referenced by name
// (<w:pStyle>/<w:rStyle>/<w:tblStyle>), so a future change that reaches for
// "just inline it, it's simpler" regresses this test immediately instead
// of silently reintroducing the striped-code-block/tall-table-row/
// gapped-list defects this architecture fixes.
//
// <w:spacing> and <w:ind> remain banned inline with NO exception, full
// stop: every construct that needs either still gets it exclusively from a
// style.
//
// <w:shd> and <w:pBdr> get three named, narrow exceptions, added by the
// renderer-compatibility task that also added this comment plus the later
// I9 fix (see styles.go's codeBorderXML/codeShadingXML/
// tableHeaderShadingXML/quoteBorderXML doc comments for the full
// reasoning) -- this is a deliberate compatibility measure, not an erosion
// of the invariant, because the renderer that motivates it (GenOffice, the
// user's own local previewer; Google Docs for the table case) does not
// resolve those two style properties AT ALL, so a style-only version is
// invisible there no matter how correct the style itself is:
//
//  1. A fenced-code-block paragraph (<w:pStyle w:val="SourceCode"/>) may
//     carry BOTH <w:pBdr> and <w:shd> inline, byte-identical to
//     SourceCode's own copy -- GenOffice does not apply a paragraph
//     style's border or shading.
//  2. A table HEADER cell's own <w:tcPr> (not the paragraph inside it) may
//     carry <w:shd> (never <w:pBdr> -- TableGrid has none to copy), also
//     byte-identical to TableGrid's <w:tblStylePr> copy -- neither Google
//     Docs nor GenOffice applies a table style's conditional formatting.
//  3. A block-quote paragraph (<w:pStyle w:val="Quote"/>) may carry
//     <w:pBdr> (never <w:shd> -- Quote has none to copy), byte-identical
//     to Quote's own left-border copy -- GenOffice does not apply a
//     paragraph style's border here either (I9).
//
// A horizontal rule's own <w:pBdr> (hrBorderXML) is a FOURTH, pre-existing
// exception that predates this task entirely: an isHR paragraph was never
// routed through a shared style to begin with (see renderParagraph's own
// doc comment), so there is nothing for it to have "moved out of" -- it is
// listed here so the loop below does not misreport it as a new violation,
// not because it is part of this task's compatibility work.
//
// Every OTHER paragraph, and every DATA-row table cell, must still carry
// neither <w:shd> nor <w:pBdr> -- the exceptions are scoped to exactly the
// constructs named above, not "anywhere convenient."
//
// The markdown below exercises every construct that used to (or still
// does) write one of these properties: a heading, an ordinary paragraph, a
// nested list, a block quote, a horizontal rule, a fenced code block, and
// a table with a header and a data row. Task 11 (I8) added two more
// paragraph kinds that also carry a <w:numPr> (a list-item continuation
// paragraph, and a fenced code block INSIDE a list item) -- a review round
// found this test's markdown never exercised either, so it was passing
// vacuously with respect to them; "continuation for b" and the fenced
// block right after it are that coverage now.
func TestWrite_NoInlineVisualPropertiesInDocumentXML(t *testing.T) {
	md := "# H\n\nBody.\n\n- a\n    - b\n\n    continuation for b\n\n    ```\n    code in list\n    ```\n\n> quote\n\n---\n\n```\ncode\n```\n\n| x | y |\n|---|---|\n| 1 | 2 |\n"
	x := generateAndReadDocumentXML(t, md)

	// Guard against this test silently going back to exercising neither new
	// paragraph kind (the vacuous-coverage finding above): a ListContinue
	// paragraph and a SECOND SourceCode paragraph (the in-list one, on top
	// of the top-level fenced block already covered) must both be present.
	if !strings.Contains(x, `<w:pStyle w:val="ListContinue"/>`) {
		t.Fatalf("no ListContinue paragraph found; the continuation-paragraph exception is untested: %s", x)
	}
	if got, want := strings.Count(x, `<w:pStyle w:val="SourceCode"/>`), 2; got != want {
		t.Fatalf("got %d SourceCode paragraphs, want %d (one top-level, one inside the list); the in-list-code exception is untested: %s", got, want, x)
	}

	// <w:spacing>/<w:ind> stay banned inline everywhere, no exceptions.
	for _, banned := range []string{"<w:spacing", "<w:ind "} {
		if strings.Contains(x, banned) {
			t.Errorf("%s appears inline in document.xml; paragraph-level visual "+
				"properties belong in styles.xml", banned)
		}
	}

	// <w:shd>/<w:pBdr> are banned on every <w:p> except a SourceCode
	// paragraph (both), a Quote paragraph (pBdr only), or an isHR paragraph
	// (pBdr only, via hrBorderXML).
	paraRE := regexp.MustCompile(`<w:p>.*?</w:p>`)
	paras := paraRE.FindAllString(x, -1)
	if len(paras) == 0 {
		t.Fatal("no <w:p> found; test would be vacuous")
	}
	for _, p := range paras {
		isCodePara := strings.Contains(p, `<w:pStyle w:val="SourceCode"/>`)
		isQuotePara := strings.Contains(p, `<w:pStyle w:val="Quote"/>`)
		isHRPara := strings.Contains(p, hrBorderXML)
		if strings.Contains(p, "<w:shd") && !isCodePara {
			t.Errorf("<w:shd appears inline on a non-code paragraph: %s", p)
		}
		if strings.Contains(p, "<w:pBdr") && !isCodePara && !isQuotePara && !isHRPara {
			t.Errorf("<w:pBdr appears inline on a paragraph that is neither code, quote, nor an hr: %s", p)
		}
	}

	// <w:shd>/<w:pBdr> are banned on every table cell except a HEADER
	// cell's own <w:tcPr>, which may carry <w:shd> (never <w:pBdr>).
	rowRE := regexp.MustCompile(`<w:tr>.*?</w:tr>`)
	rows := rowRE.FindAllString(x, -1)
	if len(rows) == 0 {
		t.Fatal("no <w:tr> found; test would be vacuous")
	}
	tcRE := regexp.MustCompile(`<w:tc>.*?</w:tc>`)
	for _, row := range rows {
		isHeader := strings.Contains(row, "<w:tblHeader/>")
		for _, tc := range tcRE.FindAllString(row, -1) {
			if strings.Contains(tc, "<w:pBdr") {
				t.Errorf("table cell carries inline <w:pBdr>, which has no exception: %s", tc)
			}
			if strings.Contains(tc, "<w:shd") && !isHeader {
				t.Errorf("non-header table cell carries inline <w:shd>: %s", tc)
			}
		}
	}
}

// TestWrite_InvariantAllowsThreeNamedExceptions pins the three allowances
// the plan calls out BY NAME as structural or per-document data, not
// styling, and therefore deliberately exempt from the ban above --
// checking they are present (not merely that the banned strings are
// absent) so this test cannot pass vacuously by, say, a change that
// happens to stop emitting numPr/tblW/jc altogether:
//
//   - <w:numPr>: attaches a list item to a numbering definition --
//     structure, not styling. Task 11 (I8) later reuses this SAME
//     exception for a purpose beyond an actual list item: a list-item
//     continuation paragraph or an in-list fenced code block also carries
//     a <w:numPr>, borrowed purely for its per-level <w:ind> (see
//     paraBlock.isListContinue's doc comment) -- still <w:numPr>, so still
//     this one named exception, not a new one, and per-level indent values
//     that could not live in a single static style anyway (see
//     TestWrite_ListItemContinuationStaysInListWithMatchingIndent).
//   - <w:tblW>/<w:gridCol>: column widths depend on each table's column
//     count and the page geometry, so they cannot live in a shared style.
//   - <w:jc> on a table cell: GFM's per-column alignment is data the
//     author wrote, not a style decision.
func TestWrite_InvariantAllowsThreeNamedExceptions(t *testing.T) {
	md := "- a\n\n| x | y |\n|---|:---:|\n| 1 | 2 |\n"
	x := generateAndReadDocumentXML(t, md)
	if !strings.Contains(x, "<w:numPr>") {
		t.Error("no <w:numPr> found; the list-item allowance is untested")
	}
	if !strings.Contains(x, "<w:tblW ") {
		t.Error("no <w:tblW> found; the table-width allowance is untested")
	}
	if !strings.Contains(x, "<w:gridCol ") {
		t.Error("no <w:gridCol> found; the column-width allowance is untested")
	}
	if !strings.Contains(x, `<w:jc w:val="center"/>`) {
		t.Error("no <w:jc> found; the table-cell-alignment allowance is untested")
	}
}

// TestWrite_EveryConstructReferencesItsStyle is Step 3's "观感回归测试": a
// realistic document exercising headings, an ordinary paragraph, a nested
// list, a block quote, a fenced code block, a table, and a link, checked
// against the brief's mapping table -- the code BLOCK's one paragraph
// references SourceCode (see the code-single-paragraph task: a whole block
// is now one paragraph, not one per line -- TestWrite_CodeBlockParagraphsSuppressSpacing's
// own doc comment has the full reasoning), every list item references ListParagraph AND
// still carries numPr, the table references TableGrid with column widths
// still summing exactly to the content width, inline code references
// VerbatimChar, and a link's text references Hyperlink. The document must
// also still reopen through this package's own reader and survive every
// docx_format rule (the compose guarantee), proving the style-reference
// switch did not just satisfy a string check while breaking something an
// independent reader or downstream tool would notice.
func TestWrite_EveryConstructReferencesItsStyle(t *testing.T) {
	md := "# Design Doc\n\n" +
		"Body text with `inline code` and a [link](https://example.com/x).\n\n" +
		"- top item\n" +
		"  - nested item\n\n" +
		"> a quoted line\n\n" +
		"```\nline one\nline two\n```\n\n" +
		"| a | b | c |\n|---|---|---|\n| 1 | 2 | 3 |\n"

	d, _, _ := writeAndReopen(t, md)
	doc, _ := d.Part(DocumentPart)
	s := string(doc)

	if got, want := strings.Count(s, `<w:pStyle w:val="SourceCode"/>`), 1; got != want {
		t.Errorf("SourceCode pStyle count = %d, want %d (one per code BLOCK, not one per line)", got, want)
	}
	if got, want := strings.Count(s, `<w:pStyle w:val="ListParagraph"/>`), 2; got != want {
		t.Errorf("ListParagraph pStyle count = %d, want %d (one per list item)", got, want)
	}
	if got, want := strings.Count(s, "<w:numPr>"), 2; got != want {
		t.Errorf("numPr count = %d, want %d (list items must keep numPr alongside the style)", got, want)
	}
	if !strings.Contains(s, `<w:pStyle w:val="Quote"/>`) {
		t.Error("block quote paragraph does not reference Quote")
	}
	if !strings.Contains(s, `<w:tblStyle w:val="TableGrid"/>`) {
		t.Error("table does not reference TableGrid")
	}
	if !strings.Contains(s, `<w:rStyle w:val="VerbatimChar"/>`) {
		t.Error("inline code run does not reference VerbatimChar")
	}
	if !strings.Contains(s, `<w:rStyle w:val="Hyperlink"/>`) {
		t.Error("link run does not reference Hyperlink")
	}

	want := contentWidthFromSectPr(t, s)
	gridStart := strings.Index(s, "<w:tblGrid>")
	gridEnd := strings.Index(s, "</w:tblGrid>")
	if gridStart < 0 || gridEnd < 0 {
		t.Fatal("no <w:tblGrid> found")
	}
	cols := gridRE.FindAllStringSubmatch(s[gridStart:gridEnd], -1)
	if len(cols) != 3 {
		t.Fatalf("got %d gridCol entries, want 3", len(cols))
	}
	sum := 0
	for _, c := range cols {
		n, _ := strconv.Atoi(c[1])
		sum += n
	}
	if sum != want {
		t.Errorf("table gridCol widths sum to %d, want the content width %d", sum, want)
	}

	// The compose guarantee: every docx_format rule must still work on this
	// document, not just on the narrower fixtures write_format_compose_test.go
	// already covers.
	if _, err := d.Format(FormatOptions{BodyFont: "Calibri", BodySizePt: 13, LineSpacing: 1.5, Align: "justify"}); err != nil {
		t.Errorf("Format failed on a document exercising every construct: %v", err)
	}
}
