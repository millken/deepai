package docx

import (
	"strings"
	"testing"
)

// scanFixture opens the shared fixture and scans its document body.
func scanFixture(t *testing.T) ([]byte, []Para) {
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
	return doc, paras
}

// paraText joins a paragraph's run text, which is the visible text of the
// paragraph.
func paraText(p Para) string {
	var b strings.Builder
	for _, r := range p.Runs {
		b.WriteString(r.Text)
	}
	return b.String()
}

func findPara(paras []Para, want string) (Para, bool) {
	for _, p := range paras {
		if paraText(p) == want {
			return p, true
		}
	}
	return Para{}, false
}

func TestScan_MultiRunParagraph(t *testing.T) {
	_, paras := scanFixture(t)
	p, ok := findPara(paras, "Hello bold world")
	if !ok {
		t.Fatalf("multi-run paragraph not found; got %d paragraphs", len(paras))
	}
	if len(p.Runs) != 3 {
		t.Errorf("Runs = %d, want 3", len(p.Runs))
	}
	wantRuns := []string{"Hello ", "bold", " world"}
	for i, want := range wantRuns {
		if i >= len(p.Runs) {
			break
		}
		if p.Runs[i].Text != want {
			t.Errorf("Runs[%d].Text = %q, want %q", i, p.Runs[i].Text, want)
		}
		if p.Runs[i].Index != i+1 {
			t.Errorf("Runs[%d].Index = %d, want %d", i, p.Runs[i].Index, i+1)
		}
	}
}

// TestScan_DecodesEntities pins that Text is the DECODED string. Callers
// must never assume Text's character offsets map linearly onto Content's
// byte offsets.
func TestScan_DecodesEntities(t *testing.T) {
	_, paras := scanFixture(t)
	if _, ok := findPara(paras, "Tom & Jerry <fast>"); !ok {
		var got []string
		for _, p := range paras {
			got = append(got, paraText(p))
		}
		t.Fatalf("entity paragraph not found; paragraphs: %q", got)
	}
}

// TestScan_ContentSpanIsRawBytes pins that Content delimits the RAW
// (still-escaped) bytes inside <w:t>, which is what splice replaces.
func TestScan_ContentSpanIsRawBytes(t *testing.T) {
	doc, paras := scanFixture(t)
	p, ok := findPara(paras, "Tom & Jerry <fast>")
	if !ok {
		t.Fatal("entity paragraph not found")
	}
	if len(p.Runs) != 1 {
		t.Fatalf("Runs = %d, want 1", len(p.Runs))
	}
	raw := string(doc[p.Runs[0].Content.Start:p.Runs[0].Content.End])
	if raw != "Tom &amp; Jerry &lt;fast&gt;" {
		t.Errorf("raw content = %q, want the escaped form", raw)
	}
}

func TestScan_DetectsXMLSpacePreserve(t *testing.T) {
	_, paras := scanFixture(t)
	p, ok := findPara(paras, " padded text ")
	if !ok {
		t.Fatal("preserve paragraph not found")
	}
	if len(p.Runs) != 1 {
		t.Fatalf("Runs = %d, want 1", len(p.Runs))
	}
	if !p.Runs[0].HasPreserve {
		t.Error("HasPreserve = false, want true")
	}
}

// TestScan_RecursesIntoHyperlink guards the container-nesting rule: <w:r>
// is not always a direct child of <w:p>.
// TestScan_DetectsSelfClosingText pins I7's Scan half: for <w:t/>,
// encoding/xml emits the StartElement and EndElement at the same input
// offset (no CharData token in between), so Content ends up as a
// zero-length span {X,X} positioned right after the "/>" — inside <w:r> but
// outside any <w:t> content model. Scan must flag this on the Run so Apply
// can refuse to patch it instead of splicing character data directly into
// <w:r>, which Word reports as unreadable content.
func TestScan_DetectsSelfClosingText(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:t/></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(paras) != 1 || len(paras[0].Runs) != 1 {
		t.Fatalf("paras = %+v, want exactly one paragraph with one run", paras)
	}
	r := paras[0].Runs[0]
	if !r.SelfClosing {
		t.Error("SelfClosing = false, want true for <w:t/>")
	}
	if r.Content.Start != r.Content.End {
		t.Errorf("Content = %+v, want a zero-length span for a self-closing tag", r.Content)
	}
	if r.Text != "" {
		t.Errorf("Text = %q, want empty", r.Text)
	}
}

// TestScan_DoesNotFlagOrdinaryTextAsSelfClosing is the negative half: a
// normal <w:t>...</w:t> run, even one with attributes on its start tag,
// must not be misdetected as self-closing.
func TestScan_DoesNotFlagOrdinaryTextAsSelfClosing(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:t xml:space="preserve">hi</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	r := paras[0].Runs[0]
	if r.SelfClosing {
		t.Error("SelfClosing = true, want false for an ordinary <w:t>...</w:t> run")
	}
}

func TestScan_RecursesIntoHyperlink(t *testing.T) {
	_, paras := scanFixture(t)
	if _, ok := findPara(paras, "link text"); !ok {
		t.Error("hyperlink run was not indexed")
	}
}

// TestScan_RevisionMarks pins the two halves of the revision rule:
// <w:ins> content is visible text and is indexed; <w:delText> is deleted
// text and must be excluded.
func TestScan_RevisionMarks(t *testing.T) {
	_, paras := scanFixture(t)
	if _, ok := findPara(paras, "inserted"); !ok {
		t.Error("w:ins run was not indexed, want it included")
	}
	for _, p := range paras {
		if strings.Contains(paraText(p), "deleted") {
			t.Errorf("w:delText leaked into paragraph %d: %q", p.Index, paraText(p))
		}
	}
}

func TestScan_TableParagraphsAreIndexedInline(t *testing.T) {
	_, paras := scanFixture(t)
	var found int
	for _, p := range paras {
		if strings.HasPrefix(paraText(p), "cell ") {
			found++
			if !p.InTable {
				t.Errorf("paragraph %d %q: InTable = false, want true", p.Index, paraText(p))
			}
		}
	}
	if found != 4 {
		t.Errorf("found %d table paragraphs, want 4", found)
	}
}

// TestScan_InTableIsFalseOutsideTable pins the two mutation-proven holes in
// TestScan_TableParagraphsAreIndexedInline above: that test only asserts
// InTable == true for paragraphs inside the table, so hardcoding
// paraInTable = true unconditionally still passes it, and so does deleting
// the `tableDepth--` decrement on </w:tbl> (both mutations were verified
// against the old test).
//
// The first assertion here (a body paragraph well before the table) catches
// the first mutation. The second (the paragraph immediately after the
// table) catches the second: with tableDepth never decremented back to 0,
// every paragraph after the table stays incorrectly marked InTable == true.
func TestScan_InTableIsFalseOutsideTable(t *testing.T) {
	_, paras := scanFixture(t)

	p, ok := findPara(paras, "Hello bold world")
	if !ok {
		t.Fatal("body paragraph not found")
	}
	if p.InTable {
		t.Error("InTable = true, want false for a paragraph outside any table")
	}

	var lastTableParaIdx int
	for _, q := range paras {
		if strings.HasPrefix(paraText(q), "cell ") && q.Index > lastTableParaIdx {
			lastTableParaIdx = q.Index
		}
	}
	if lastTableParaIdx == 0 {
		t.Fatal("no table paragraphs found; fixture changed")
	}
	var after Para
	found := false
	for _, q := range paras {
		if q.Index == lastTableParaIdx+1 {
			after = q
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no paragraph found immediately after paragraph %d (the last table cell)", lastTableParaIdx)
	}
	if after.InTable {
		t.Errorf("paragraph %d (immediately after the table): InTable = true, want false", after.Index)
	}
}

func TestScan_IndicesAreSequential(t *testing.T) {
	_, paras := scanFixture(t)
	if len(paras) == 0 {
		t.Fatal("no paragraphs")
	}
	for i, p := range paras {
		if p.Index != i+1 {
			t.Fatalf("paras[%d].Index = %d, want %d", i, p.Index, i+1)
		}
	}
}

// TestScan_DrawingMLDoesNotTruncateParagraph guards against namespace-blind
// element matching. Word embeds DrawingML (text boxes, shapes, WordArt)
// inside a run using its own <a:p>/<a:r>/<a:t> elements, which share local
// names with WordprocessingML's <w:p>/<w:r>/<w:t>. A scanner that matches on
// local name alone treats </a:p> as if it were </w:p> and ends the real
// paragraph early, silently dropping every run that follows the drawing.
func TestScan_DrawingMLDoesNotTruncateParagraph(t *testing.T) {
	doc := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:wps="http://schemas.microsoft.com/office/word/2010/wordprocessingShape">
<w:body><w:p><w:r><w:t>before</w:t></w:r>
  <w:drawing><wps:txbx><a:p><a:r><a:t>shape text</a:t></a:r></a:p></wps:txbx></w:drawing>
<w:r><w:t>after</w:t></w:r></w:p></w:body>
</w:document>`)

	realClose := "</w:p>"
	closeIdx := strings.Index(string(doc), realClose)
	if closeIdx < 0 {
		t.Fatal("test fixture has no </w:p>; fixture is broken")
	}
	wantEnd := closeIdx + len(realClose)

	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(paras) != 1 {
		t.Fatalf("paras = %d, want 1", len(paras))
	}
	p := paras[0]

	text := paraText(p)
	if strings.Contains(text, "shape text") {
		t.Errorf("paragraph text = %q, DrawingML shape text must not be indexed", text)
	}
	if len(p.Runs) != 2 {
		t.Fatalf("Runs = %d, want 2 (before, after); got %+v", len(p.Runs), p.Runs)
	}
	if p.Runs[0].Text != "before" {
		t.Errorf("Runs[0].Text = %q, want %q", p.Runs[0].Text, "before")
	}
	if p.Runs[1].Text != "after" {
		t.Errorf("Runs[1].Text = %q, want %q", p.Runs[1].Text, "after")
	}
	if p.Span.End != wantEnd {
		t.Errorf("Span.End = %d, want %d (the real </w:p>); got span %q", p.Span.End, wantEnd, string(doc[p.Span.Start:min(len(doc), p.Span.End+10)]))
	}
}

// TestScan_ParagraphStyle pins that <w:pStyle w:val="..."/> is captured, which
// §4.1's heading outline depends on.
func TestScan_ParagraphStyle(t *testing.T) {
	doc := []byte(`<w:body>` +
		`<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>Title</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>body</w:t></w:r></w:p>` +
		`</w:body>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(paras) != 2 {
		t.Fatalf("got %d paragraphs, want 2", len(paras))
	}
	if paras[0].Style != "Heading1" {
		t.Errorf("paras[0].Style = %q, want %q", paras[0].Style, "Heading1")
	}
	if paras[1].Style != "" {
		t.Errorf("paras[1].Style = %q, want empty", paras[1].Style)
	}
}

// TestScan_CellCoordinates pins §4.1's "标注所属单元格坐标" requirement.
func TestScan_CellCoordinates(t *testing.T) {
	doc := []byte(`<w:body>` +
		`<w:p><w:r><w:t>before</w:t></w:r></w:p>` +
		`<w:tbl>` +
		`<w:tr><w:tc><w:p><w:r><w:t>r1c1</w:t></w:r></w:p></w:tc>` +
		`<w:tc><w:p><w:r><w:t>r1c2</w:t></w:r></w:p></w:tc></w:tr>` +
		`<w:tr><w:tc><w:p><w:r><w:t>r2c1</w:t></w:r></w:p></w:tc></w:tr>` +
		`</w:tbl>` +
		`<w:p><w:r><w:t>after</w:t></w:r></w:p>` +
		`</w:body>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	want := []struct {
		text string
		cell *CellRef
	}{
		{"before", nil},
		{"r1c1", &CellRef{Table: 1, Row: 1, Col: 1}},
		{"r1c2", &CellRef{Table: 1, Row: 1, Col: 2}},
		{"r2c1", &CellRef{Table: 1, Row: 2, Col: 1}},
		{"after", nil},
	}
	if len(paras) != len(want) {
		t.Fatalf("got %d paragraphs, want %d", len(paras), len(want))
	}
	for i, w := range want {
		got := paraText(paras[i])
		if got != w.text {
			t.Fatalf("paras[%d] text = %q, want %q", i, got, w.text)
		}
		switch {
		case w.cell == nil && paras[i].Cell != nil:
			t.Errorf("paras[%d] (%q): Cell = %+v, want nil", i, w.text, *paras[i].Cell)
		case w.cell != nil && paras[i].Cell == nil:
			t.Errorf("paras[%d] (%q): Cell = nil, want %+v", i, w.text, *w.cell)
		case w.cell != nil && *paras[i].Cell != *w.cell:
			t.Errorf("paras[%d] (%q): Cell = %+v, want %+v", i, w.text, *paras[i].Cell, *w.cell)
		}
	}
}

// TestScan_NestedTableRestoresOuterCellCoordinates pins the fix for scalar
// row/col tracking: a <w:tbl> nested inside a <w:tc> (common in real
// documents — layout tables, a table inside a cell) must not leave the
// outer table's row/col counters clobbered once the inner table closes.
// Table stays a document-wide 1-based counter (the inner table is table 2),
// but Row/Col must come from the enclosing frame once the inner table pops.
func TestScan_NestedTableRestoresOuterCellCoordinates(t *testing.T) {
	doc := []byte(`<w:tbl><w:tr><w:tc>` +
		`<w:p><w:r><w:t>outer-r1c1</w:t></w:r></w:p>` +
		`<w:tbl><w:tr><w:tc><w:p><w:r><w:t>inner</w:t></w:r></w:p></w:tc></w:tr></w:tbl>` +
		`</w:tc>` +
		`<w:tc><w:p><w:r><w:t>outer-r1c2</w:t></w:r></w:p></w:tc>` +
		`</w:tr></w:tbl>` +
		`<w:p><w:r><w:t>after</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	want := []struct {
		text string
		cell *CellRef
	}{
		{"outer-r1c1", &CellRef{Table: 1, Row: 1, Col: 1}},
		{"inner", &CellRef{Table: 2, Row: 1, Col: 1}},
		{"outer-r1c2", &CellRef{Table: 1, Row: 1, Col: 2}},
		{"after", nil},
	}
	if len(paras) != len(want) {
		t.Fatalf("got %d paragraphs, want %d", len(paras), len(want))
	}
	for i, w := range want {
		got := paraText(paras[i])
		if got != w.text {
			t.Fatalf("paras[%d] text = %q, want %q", i, got, w.text)
		}
		switch {
		case w.cell == nil && paras[i].Cell != nil:
			t.Errorf("paras[%d] (%q): Cell = %+v, want nil", i, w.text, *paras[i].Cell)
		case w.cell != nil && paras[i].Cell == nil:
			t.Errorf("paras[%d] (%q): Cell = nil, want %+v", i, w.text, *w.cell)
		case w.cell != nil && *paras[i].Cell != *w.cell:
			t.Errorf("paras[%d] (%q): Cell = %+v, want %+v", i, w.text, *paras[i].Cell, *w.cell)
		}
	}
}

// TestScan_RunElemSpanCoversWholeRun pins that Run.Elem delimits the entire
// <w:r> element, which §4.2's run-level delete needs in order to remove the
// run instead of leaving an empty one behind.
func TestScan_RunElemSpanCoversWholeRun(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:rPr><w:b/></w:rPr><w:t>bold</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(paras) != 1 || len(paras[0].Runs) != 1 {
		t.Fatalf("got %d paragraphs / %d runs, want 1/1", len(paras), len(paras[0].Runs))
	}
	got := string(doc[paras[0].Runs[0].Elem.Start:paras[0].Runs[0].Elem.End])
	want := `<w:r><w:rPr><w:b/></w:rPr><w:t>bold</w:t></w:r>`
	if got != want {
		t.Errorf("Elem span = %q, want %q", got, want)
	}
}

// TestScan_RevisionSignals pins §4.1's recommended P1 policy input: callers
// must be able to detect existing revision marks without scanning bytes
// themselves.
func TestScan_RevisionSignals(t *testing.T) {
	doc := []byte(`<w:body>` +
		`<w:p><w:r><w:t>plain</w:t></w:r></w:p>` +
		`<w:p><w:ins w:id="1"><w:r><w:t>added</w:t></w:r></w:ins></w:p>` +
		`<w:p><w:del w:id="2"><w:r><w:delText>gone</w:delText></w:r></w:del><w:r><w:t>kept</w:t></w:r></w:p>` +
		`</w:body>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(paras) != 3 {
		t.Fatalf("got %d paragraphs, want 3", len(paras))
	}
	if paras[0].HasRevisions {
		t.Error("paras[0] (plain): HasRevisions = true, want false")
	}
	if !paras[1].HasRevisions {
		t.Error("paras[1] (w:ins): HasRevisions = false, want true")
	}
	if !paras[2].HasRevisions {
		t.Error("paras[2] (w:del): HasRevisions = false, want true")
	}
	if len(paras[1].Runs) != 1 || !paras[1].Runs[0].InInsertion {
		t.Errorf("paras[1] run InInsertion = false, want true")
	}
	if len(paras[2].Runs) != 1 || paras[2].Runs[0].InInsertion {
		t.Errorf("paras[2] kept-run InInsertion = true, want false")
	}
}

// TestScan_BreaksRecordRunPositions pins §4.1's markdown output need: a <w:br/>
// between runs must be recoverable, without polluting Run.Text (which must stay
// exactly the decoding of Run.Content).
func TestScan_BreaksRecordRunPositions(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:t>line1</w:t></w:r><w:r><w:br/></w:r><w:r><w:t>line2</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	if len(paras[0].Breaks) != 1 || paras[0].Breaks[0] != 1 {
		t.Errorf("Breaks = %v, want [1] (a break after run 1)", paras[0].Breaks)
	}
	// Run.Text must remain the pure decoding of Run.Content.
	for _, r := range paras[0].Runs {
		if strings.ContainsAny(r.Text, "\n\r") {
			t.Errorf("run %d Text = %q: break leaked into Text", r.Index, r.Text)
		}
	}
}

// TestScan_TextBoxContentIsSkipped pins the P1 policy decision: <w:txbxContent>
// subtrees are not indexed, and the containing paragraph says so.
func TestScan_TextBoxContentIsSkipped(t *testing.T) {
	doc := []byte(`<w:body><w:p><w:r><w:t>before</w:t></w:r>` +
		`<w:r><w:drawing><wps:txbx><w:txbxContent>` +
		`<w:p><w:r><w:t>inside box</w:t></w:r></w:p>` +
		`</w:txbxContent></wps:txbx></w:drawing></w:r>` +
		`<w:r><w:t>after</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>next</w:t></w:r></w:p></w:body>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(paras) != 2 {
		var got []string
		for _, p := range paras {
			got = append(got, paraText(p))
		}
		t.Fatalf("got %d paragraphs %q, want 2 (text-box paragraph must not be indexed)", len(paras), got)
	}
	if got := paraText(paras[0]); got != "beforeafter" {
		t.Errorf("paras[0] text = %q, want %q (box text excluded, siblings kept)", got, "beforeafter")
	}
	if !paras[0].SkippedTextBox {
		t.Error("paras[0].SkippedTextBox = false, want true")
	}
	if got := paraText(paras[1]); got != "next" {
		t.Errorf("paras[1] text = %q, want %q", got, "next")
	}
	if paras[1].SkippedTextBox {
		t.Error("paras[1].SkippedTextBox = true, want false")
	}
}

// TestScan_ParaSpanCoversElement pins that Para.Span delimits the whole
// <w:p> element, which later tasks rely on for paragraph-level operations.
func TestScan_ParaSpanCoversElement(t *testing.T) {
	doc, paras := scanFixture(t)
	for _, p := range paras {
		got := string(doc[p.Span.Start:p.Span.End])
		if !strings.HasPrefix(got, "<w:p") {
			t.Fatalf("paragraph %d span does not start at <w:p: %.40q", p.Index, got)
		}
		if !strings.HasSuffix(got, "</w:p>") {
			t.Fatalf("paragraph %d span does not end at </w:p>: %.40q", p.Index, got[max(0, len(got)-40):])
		}
	}
}
