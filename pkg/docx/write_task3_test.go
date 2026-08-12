package docx

import (
	"regexp"
	"strings"
	"testing"
)

// This file covers Task 3 ("表格与页脚") of docs/superpowers/plans/
// 2026-08-12-docx-chinese-typography.md, plus the font-parameterization half
// of Part A that typography_test.go's Task 1 tests do not cover (Task 1
// pinned the DEFAULT font values; this file pins that a caller can override
// them via WriteOptions and that an empty value still falls back).

// ---------------------------------------------------------------------------
// Part A: fonts become WriteOptions parameters
// ---------------------------------------------------------------------------

// A custom body font must reach docDefaults -- the one place every style
// except SourceCode/VerbatimChar inherits its font from.
func TestWrite_CustomBodyFontsReachDocDefaults(t *testing.T) {
	styles := buildStylesXMLWithFonts(fontOptions{
		bodyLatin:    "Times New Roman",
		bodyEastAsia: "宋体",
		codeLatin:    defaultCodeLatinFont,
		codeEastAsia: defaultCodeEastAsiaFont,
	})
	s := string(styles)
	dd := s[strings.Index(s, "<w:docDefaults>"):strings.Index(s, "</w:docDefaults>")]
	if !strings.Contains(dd, `<w:rFonts w:ascii="Times New Roman" w:eastAsia="宋体"/>`) {
		t.Errorf("docDefaults = %s, want the custom body fonts", dd)
	}
}

// A custom code font must reach BOTH SourceCode (the fenced-code-block
// style) and VerbatimChar (the inline-code character style) -- the two
// places a document's code font is declared. Missing either would mean a
// fenced code block and an inline `code` span render in different fonts
// within the same document.
func TestWrite_CustomCodeFontsReachSourceCodeAndVerbatimChar(t *testing.T) {
	f := fontOptions{
		bodyLatin:    defaultBodyLatinFont,
		bodyEastAsia: defaultBodyEastAsiaFont,
		codeLatin:    "Cascadia Code",
		codeEastAsia: "NSimSun",
	}
	styles := buildStylesXMLWithFonts(f)
	for _, id := range []string{StyleSourceCode, StyleVerbatimChar} {
		block := styleBlock(t, styles, id)
		m := rFontsRE.FindString(block)
		if m == "" {
			t.Fatalf("%s has no <w:rFonts> at all", id)
		}
		if !strings.Contains(m, `w:ascii="Cascadia Code"`) || !strings.Contains(m, `w:eastAsia="NSimSun"`) {
			t.Errorf("%s's rFonts = %s, want the custom code fonts", id, m)
		}
	}
}

// The custom code font must also reach renderCtx.codeFontXML's direct
// <w:rFonts> fallback -- the one case (inline code that is also a
// hyperlink's text) that cannot pick up SourceCode/VerbatimChar via a
// style reference. Exercised end-to-end through WriteDocx, not just the
// method in isolation, so this also proves WriteOptions' fields actually
// reach renderCtx.fonts and not just buildStylesXMLWithFonts.
func TestWrite_CustomCodeFontReachesCodeFontXMLFallback(t *testing.T) {
	p := t.TempDir() + "/out.docx"
	_, err := WriteDocx(p, WriteOptions{
		Markdown:         "[`code`](https://example.com)\n",
		CodeLatinFont:    "Cascadia Code",
		CodeEastAsiaFont: "NSimSun",
	})
	if err != nil {
		t.Fatalf("WriteDocx: %v", err)
	}
	d, err := OpenDocument(p)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	doc, _ := d.Part(DocumentPart)
	s := string(doc)
	if !strings.Contains(s, `w:ascii="Cascadia Code" w:eastAsia="NSimSun"`) {
		t.Errorf("document.xml does not carry the custom code font for the linked code span:\n%s", s)
	}
}

// An empty WriteOptions (every font field left "") must still fall back to
// this package's own defaults -- Calibri/微软雅黑 for body, Consolas/微软雅黑
// for code -- exactly as if fonts had never been made configurable at all.
// This is the self-review question "does an empty value still fall back?"
// pinned directly, end-to-end through WriteDocx rather than only against
// resolveFontOptions/defaultFontOptions in isolation.
func TestWrite_EmptyFontOptionsFallBackToDefaults(t *testing.T) {
	p := t.TempDir() + "/out.docx"
	_, err := WriteDocx(p, WriteOptions{Markdown: "body\n\n```\ncode\n```\n"})
	if err != nil {
		t.Fatalf("WriteDocx: %v", err)
	}
	d, err := OpenDocument(p)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	styles, _ := d.Part("word/styles.xml")
	s := string(styles)
	dd := s[strings.Index(s, "<w:docDefaults>"):strings.Index(s, "</w:docDefaults>")]
	if !strings.Contains(dd, `<w:rFonts w:ascii="`+defaultBodyLatinFont+`" w:eastAsia="`+defaultBodyEastAsiaFont+`"/>`) {
		t.Errorf("docDefaults = %s, want the default body fonts when WriteOptions leaves them empty", dd)
	}
	sc := styleBlock(t, styles, StyleSourceCode)
	if !strings.Contains(sc, `w:ascii="`+defaultCodeLatinFont+`"`) || !strings.Contains(sc, `w:eastAsia="`+defaultCodeEastAsiaFont+`"`) {
		t.Errorf("SourceCode = %s, want the default code fonts when WriteOptions leaves them empty", sc)
	}
}

// resolveFontOptions itself, directly: a partially-filled WriteOptions
// (only ONE of the four fields set) must fall back to the default for
// every OTHER field, not just when all four are empty -- a slightly
// different case than TestWrite_EmptyFontOptionsFallBackToDefaults, which
// only exercises "all four empty".
func TestResolveFontOptions_PartialOverrideFallsBackForTheRest(t *testing.T) {
	f := resolveFontOptions(WriteOptions{CodeEastAsiaFont: "NSimSun"})
	want := fontOptions{
		bodyLatin:    defaultBodyLatinFont,
		bodyEastAsia: defaultBodyEastAsiaFont,
		codeLatin:    defaultCodeLatinFont,
		codeEastAsia: "NSimSun",
	}
	if f != want {
		t.Errorf("resolveFontOptions = %+v, want %+v", f, want)
	}
}

// ---------------------------------------------------------------------------
// Part B: table header shading (via conditional formatting), cell margins,
// and cross-page header repeat
// ---------------------------------------------------------------------------

// The header row's shading must come from TableGrid's own
// <w:tblStylePr w:type="firstRow"> conditional formatting (the semantic
// anchor -- see TestType_TableLookActivatesFirstRowShading) AND, as of the
// GenOffice/Google-Docs-compatibility task, ALSO be copied directly onto
// each header cell's own <w:tcPr> (see styles.go's tableHeaderShadingXML
// doc comment: neither Google Docs nor GenOffice applies a table style's
// conditional formatting at all, so the style-only version this test used
// to require left the header unshaded in both). Bold needs no such change:
// it has been written directly via paraBlock.forceBold on the header run
// since before this task, so it was never at risk of this defect. Data-row
// cells must still carry no shd at all -- the header exception is scoped to
// the header row only, not the whole table.
func TestType_TableHeaderShadedViaStyleAndInline(t *testing.T) {
	styles := string(buildStylesXML())
	tg := styleBlock(t, []byte(styles), StyleTableGrid)
	if !strings.Contains(tg, `<w:tblStylePr w:type="firstRow">`) {
		t.Fatalf("TableGrid = %s, want a firstRow tblStylePr", tg)
	}
	if !strings.Contains(tg, `<w:shd w:val="clear" w:fill="DDE5F0"/>`) {
		t.Errorf("TableGrid's firstRow tblStylePr = %s, want the reference's DDE5F0 shading", tg)
	}
	if !strings.Contains(tg, "<w:b/>") {
		t.Errorf("TableGrid's firstRow tblStylePr = %s, want bold", tg)
	}

	md := "| a | b |\n|---|---|\n| 1 | 2 |\n"
	s := generateAndReadDocumentXML(t, md)
	rowRE := regexp.MustCompile(`<w:tr>.*?</w:tr>`)
	rows := rowRE.FindAllString(s, -1)
	if len(rows) != 2 {
		t.Fatalf("got %d <w:tr> rows, want 2 (1 header + 1 data)", len(rows))
	}
	if !strings.Contains(rows[0], `<w:tcPr><w:shd w:val="clear" w:fill="DDE5F0"/></w:tcPr>`) {
		t.Errorf("header row = %s, want an inline header-shading <w:tcPr><w:shd>", rows[0])
	}
	if strings.Contains(rows[1], "<w:shd") {
		t.Errorf("data row = %s, must not carry inline <w:shd> -- the header exception is scoped to the header row only", rows[1])
	}
}

// TableGrid carrying tblStylePr is not enough on its own -- CT_TblLook has
// to opt a given table INTO applying it (Word treats a table style's
// conditional formatting as inert unless the table's own tblLook enables
// the matching bit). This checks both halves together, not the style in
// isolation, since a style-only check could pass even if no table ever
// actually activated the shading.
func TestType_TableLookActivatesFirstRowShading(t *testing.T) {
	md := "| a | b |\n|---|---|\n| 1 | 2 |\n"
	s := generateAndReadDocumentXML(t, md)
	tblStart := strings.Index(s, "<w:tbl>")
	tblPrEnd := strings.Index(s, "</w:tblPr>")
	if tblStart < 0 || tblPrEnd < 0 {
		t.Fatal("no <w:tbl>/<w:tblPr> found; test would be vacuous")
	}
	tblPr := s[tblStart:tblPrEnd]
	if !strings.Contains(tblPr, `<w:tblLook w:firstRow="1"`) {
		t.Errorf("table's own tblPr = %s, want a tblLook with firstRow=\"1\" to activate TableGrid's conditional formatting", tblPr)
	}
}

// A long table's header row must repeat on every page it spans in Word --
// <w:trPr><w:tblHeader/></w:trPr> on the header <w:tr>, structural
// per-row data (not a style property), so it belongs in document.xml
// itself rather than styles.xml. A data row must NOT carry it.
func TestType_HeaderRowRepeatsAcrossPages(t *testing.T) {
	md := "| a | b |\n|---|---|\n| 1 | 2 |\n| 3 | 4 |\n"
	s := generateAndReadDocumentXML(t, md)
	rowRE := regexp.MustCompile(`<w:tr>.*?</w:tr>`)
	rows := rowRE.FindAllString(s, -1)
	if len(rows) != 3 {
		t.Fatalf("got %d <w:tr> rows, want 3 (1 header + 2 data)", len(rows))
	}
	if !strings.Contains(rows[0], "<w:trPr><w:tblHeader/></w:trPr>") {
		t.Errorf("header row = %s, want <w:trPr><w:tblHeader/></w:trPr>", rows[0])
	}
	for i, row := range rows[1:] {
		if strings.Contains(row, "<w:tblHeader") {
			t.Errorf("data row %d = %s, must NOT repeat as a header", i+1, row)
		}
	}
}

// Every cell must carry the reference's inner margins (60 twips top/
// bottom, 100 twips left/right) via TableGrid's <w:tblCellMar> -- not as an
// inline property on any individual cell, since <w:tblCellMar> is a
// table-style-level property Word applies to every cell in a table using
// that style.
func TestType_CellsHaveInnerMargins(t *testing.T) {
	tg := styleBlock(t, buildStylesXML(), StyleTableGrid)
	want := `<w:tblCellMar><w:top w:w="60" w:type="dxa"/><w:left w:w="100" w:type="dxa"/>` +
		`<w:bottom w:w="60" w:type="dxa"/><w:right w:w="100" w:type="dxa"/></w:tblCellMar>`
	if !strings.Contains(tg, want) {
		t.Errorf("TableGrid = %s, want %s", tg, want)
	}
}

// ---------------------------------------------------------------------------
// Part C: page footer with a page number
// ---------------------------------------------------------------------------

// word/footer1.xml must hold a centered PAGE field at 9pt (sz=18
// half-points), per the reference document's own footer.
func TestType_FooterHasPageNumberField(t *testing.T) {
	if !strings.Contains(footer1XML, `<w:jc w:val="center"/>`) {
		t.Errorf("footer1XML = %s, want a centered paragraph", footer1XML)
	}
	if !strings.Contains(footer1XML, `w:instr=" PAGE "`) {
		t.Errorf("footer1XML = %s, want a PAGE field", footer1XML)
	}
	if !strings.Contains(footer1XML, `<w:sz w:val="18"/>`) {
		t.Errorf("footer1XML = %s, want 9pt (sz=18)", footer1XML)
	}
}

// The footer part must be registered in all three places a new OOXML part
// needs: [Content_Types].xml (or Word calls the whole package invalid),
// word/_rels/document.xml.rels (or the sectPr's r:id resolves to nothing),
// and the body's own <w:sectPr><w:footerReference> (or nothing ever asks
// for the footer to be shown at all). All three must agree on the SAME
// relationship id, not just each independently exist.
func TestType_FooterIsRegisteredEverywhere(t *testing.T) {
	d, _, _ := writeAndReopen(t, "# H\n")

	ct, ok := d.Part("[Content_Types].xml")
	if !ok {
		t.Fatal("[Content_Types].xml missing")
	}
	if !strings.Contains(string(ct), `<Override PartName="/word/footer1.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.footer+xml"/>`) {
		t.Errorf("[Content_Types].xml does not declare word/footer1.xml: %s", ct)
	}

	doc, _ := d.Part(DocumentPart)
	m := regexp.MustCompile(`<w:footerReference w:type="default" r:id="(rId\d+)"/>`).FindStringSubmatch(string(doc))
	if m == nil {
		t.Fatalf("document.xml has no <w:footerReference>:\n%s", doc)
	}
	footerID := m[1]

	rels, _ := d.Part("word/_rels/document.xml.rels")
	wantRel := `<Relationship Id="` + footerID + `" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/footer" Target="footer1.xml"/>`
	if !strings.Contains(string(rels), wantRel) {
		t.Errorf("word/_rels/document.xml.rels = %s, want %s", rels, wantRel)
	}

	if _, ok := d.Part(footer1Part); !ok {
		t.Error("word/footer1.xml itself is missing from the package")
	}
}

// The load-bearing hazard the plan calls out by name: a document with
// several hyperlinks plus the footer must never let the footer's
// relationship id collide with any hyperlink's. A collision would make
// Word resolve <w:footerReference r:id="rIdN"> against whatever Target
// rIdN happens to have -- after a collision, a hyperlink's URL instead of
// footer1.xml -- surfacing as either a repair prompt or a footer that
// silently fails to show a page number.
func TestType_HyperlinkAndFooterRelIDsDoNotCollide(t *testing.T) {
	md := "[one](https://example.com/1) [two](https://example.com/2) " +
		"[three](https://example.com/3) [four](https://example.com/4) " +
		"[five](https://example.com/5)\n"
	d, _, _ := writeAndReopen(t, md)
	doc, _ := d.Part(DocumentPart)
	s := string(doc)

	linkIDs := regexp.MustCompile(`<w:hyperlink r:id="(rId\d+)">`).FindAllStringSubmatch(s, -1)
	if len(linkIDs) != 5 {
		t.Fatalf("got %d hyperlinks, want 5", len(linkIDs))
	}
	footerMatch := regexp.MustCompile(`<w:footerReference w:type="default" r:id="(rId\d+)"/>`).FindStringSubmatch(s)
	if footerMatch == nil {
		t.Fatal("no <w:footerReference> found")
	}

	seen := map[string]string{} // id -> what claimed it
	for _, m := range linkIDs {
		id := m[1]
		if owner, ok := seen[id]; ok {
			t.Errorf("relationship id %s used by both %s and a hyperlink", id, owner)
		}
		seen[id] = "a hyperlink"
	}
	footerID := footerMatch[1]
	if owner, ok := seen[footerID]; ok {
		t.Fatalf("footer relationship id %s collides with %s's id", footerID, owner)
	}
	seen[footerID] = "the footer"

	// Every id referenced from the body must be declared exactly once in
	// the rels part, and every one of THOSE declarations must point at the
	// right target -- a collision-free id space is only half the
	// guarantee; the other half is that the rels part actually agrees.
	rels, _ := d.Part("word/_rels/document.xml.rels")
	relsStr := string(rels)
	for id, owner := range seen {
		count := strings.Count(relsStr, `Id="`+id+`"`)
		if count != 1 {
			t.Errorf("relationship %s (%s) is declared %d times in document.xml.rels, want exactly 1", id, owner, count)
		}
	}
	if !strings.Contains(relsStr, `Id="`+footerID+`" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/footer" Target="footer1.xml"`) {
		t.Errorf("footer relationship %s does not point at footer1.xml: %s", footerID, relsStr)
	}
}
