package docx

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// This file is Task 1 ("字体与页面") of docs/superpowers/plans/
// 2026-08-12-docx-chinese-typography.md: every rFonts declaration must name
// an East Asian font (or Chinese text falls back to whatever Word's theme
// picks, which is what made the user's code-block box-drawing diagrams
// ragged), and the page must be A4, not US Letter. Font/size/page values
// are copied verbatim from a real, user-approved reference document's
// measured XML (.superpowers/sdd/reference-values.md) rather than chosen —
// see that file for the exact numbers and where they came from. The one
// value the reference cannot supply is the code block's CJK font, since the
// reference has no code blocks; that choice is this task's own and is
// covered separately below (TestType_CodeUsesCJKCapableMonospaceFont).

// rFontsRE matches one self-closing <w:rFonts .../> element so tests below
// can inspect its attributes without a full XML parse.
var rFontsRE = regexp.MustCompile(`<w:rFonts[^/]*/>`)

// 1. Every <w:rFonts> this package's styles.xml emits must carry
// w:eastAsia, or Chinese text through that font declaration falls back to
// whatever Word's own theme picks -- silently, with no error -- which is
// the root cause the plan traces the ragged code-block diagrams to.
func TestType_EveryFontHasEastAsia(t *testing.T) {
	s := string(buildStylesXML())
	matches := rFontsRE.FindAllString(s, -1)
	if len(matches) == 0 {
		t.Fatal("no <w:rFonts> found at all; test would be vacuous")
	}
	for _, m := range matches {
		if !strings.Contains(m, "w:eastAsia=") {
			t.Errorf("rFonts without eastAsia: %s -- Chinese text falls back to Word's own pick", m)
		}
	}
}

// 2. The code block's eastAsia font must be explicitly set to this
// package's own default CJK font, not left for Word's own theme pick (this
// task's own choice -- the reference document has no code blocks to copy
// from). Leaving eastAsia unset entirely is the exact defect the user
// screenshotted: box-drawing characters (│ ├ ─ └) misalign against Chinese
// labels because the two scripts' glyph widths no longer line up on the
// monospace grid Consolas draws for the ASCII half of the same line.
//
// This does NOT assert the default is a true monospace-CJK font (a 2:1
// Latin:CJK advance ratio) -- it is not, as of the docx-chinese-typography
// plan's font decision (see defaultCodeEastAsiaFont's doc comment in
// styles.go): 微软雅黑 is a proportional UI font, chosen because it is
// actually installed on the machine that reported this defect, not because
// it achieves exact alignment. TestType_CustomCodeFontsReachStylesAndFallback
// below covers the part of this task that DOES matter for exact alignment:
// that a caller can override this default with a true 2:1 font via
// WriteOptions.
func TestType_CodeUsesCJKCapableMonospaceFont(t *testing.T) {
	for _, id := range []string{"SourceCode", "VerbatimChar"} {
		block := styleBlock(t, buildStylesXML(), id)
		m := rFontsRE.FindString(block)
		if m == "" {
			t.Fatalf("%s has no <w:rFonts> at all", id)
		}
		if !strings.Contains(m, `w:eastAsia="`+defaultCodeEastAsiaFont+`"`) {
			t.Errorf("%s's rFonts is %s, want eastAsia=%q", id, m, defaultCodeEastAsiaFont)
		}
	}
}

// 3. write.go's renderCtx.codeFontXML is the one narrow edge case (inline
// code that is ALSO a hyperlink's text -- "[`code`](url)") that still
// writes a direct <w:rFonts> instead of referencing SourceCode/VerbatimChar
// (see its doc comment). It must carry the same eastAsia font those styles
// do, by default, or exactly that one combination falls out of sync with
// the rest of the document's code font.
func TestType_CodeFontXMLFallbackHasEastAsia(t *testing.T) {
	want := (&renderCtx{fonts: defaultFontOptions()}).codeFontXML()
	if !strings.Contains(want, "w:eastAsia=\""+defaultCodeEastAsiaFont+"\"") {
		t.Errorf("codeFontXML() = %s, want eastAsia=%q", want, defaultCodeEastAsiaFont)
	}
	md := "[`code`](https://example.com)\n"
	s := generateAndReadDocumentXML(t, md)
	if !strings.Contains(s, want) {
		t.Errorf("document.xml for a linked code span does not contain codeFontXML:\n%s", s)
	}
}

// 4. Every default font/size declaration is copied verbatim from the
// reference document's docDefaults, not chosen independently: ascii
// "Calibri", eastAsia "微软雅黑" -- exactly the pair the reference repeats
// 682 times -- and no hAnsi/cs attribute, matching the reference's own
// rFonts shape exactly (see reference-values.md).
func TestType_DefaultFontsMatchReference(t *testing.T) {
	s := string(buildStylesXML())
	dd := s[strings.Index(s, "<w:docDefaults>"):strings.Index(s, "</w:docDefaults>")]
	if !strings.Contains(dd, `<w:rFonts w:ascii="Calibri" w:eastAsia="微软雅黑"/>`) {
		t.Errorf("docDefaults rFonts does not match the reference exactly:\n%s", dd)
	}
}

// 5. Default body size is 21 half-points (10.5pt), the reference's own
// value and the conventional Chinese body size -- not this package's old
// 22 (11pt, a Western-document default).
func TestType_DefaultSizeMatchesReference(t *testing.T) {
	s := string(buildStylesXML())
	dd := s[strings.Index(s, "<w:docDefaults>"):strings.Index(s, "</w:docDefaults>")]
	if !strings.Contains(dd, `<w:sz w:val="21"/>`) {
		t.Errorf("docDefaults lacks sz=21 (10.5pt):\n%s", dd)
	}
	if !strings.Contains(dd, `<w:szCs w:val="21"/>`) {
		t.Errorf("docDefaults lacks szCs=21:\n%s", dd)
	}
}

// 6. The page is A4 (11906x16838 twips, portrait), matching the reference
// exactly -- not this package's old 12240x15840 (US Letter), which is
// simply the wrong paper for a Chinese document.
func TestType_PageIsA4(t *testing.T) {
	s := generateAndReadDocumentXML(t, "# H\n")
	if !strings.Contains(s, `<w:pgSz w:w="11906" w:h="16838" w:orient="portrait"/>`) {
		t.Errorf("page is not A4 portrait (11906x16838 twips); a Chinese document on US Letter is wrong:\n%s", s)
	}
}

// 7. Page margins match the reference exactly: 1440 twips (1 inch) on all
// four sides, but 708 twips (0.5cm) for header/footer -- this package's old
// 720 (0.5in) was a Western convention, not copied from anything.
func TestType_PageMarginsMatchReference(t *testing.T) {
	s := generateAndReadDocumentXML(t, "# H\n")
	want := `<w:pgMar w:top="1440" w:right="1440" w:bottom="1440" w:left="1440" w:header="708" w:footer="708" w:gutter="0"/>`
	if !strings.Contains(s, want) {
		t.Errorf("pgMar does not match the reference exactly; want %s in:\n%s", want, s)
	}
}

// 8. sectPr carries <w:docGrid w:linePitch="360"/>, the East Asian layout
// grid Word uses for line pitch -- present in the reference, absent from
// our output before this task.
func TestType_HasDocGrid(t *testing.T) {
	s := generateAndReadDocumentXML(t, "# H\n")
	sectPr := s[strings.Index(s, "<w:sectPr>"):]
	if !strings.Contains(sectPr, `<w:docGrid w:linePitch="360"/>`) {
		t.Errorf("sectPr lacks <w:docGrid w:linePitch=\"360\"/>:\n%s", sectPr)
	}
}

// 9. Regression guard called out explicitly by the plan: table column
// widths are computed from the section's page size and margins
// (contentWidthTwips in write.go), so changing the page to A4 must change
// the content width tables divide, automatically, with no hardcoded twip
// count anywhere in the computation. This test ties the actual rendered
// <w:gridCol> sum to the package's own named geometry constant
// (contentWidthTwips) rather than to a number typed into the test --
// exactly the failure mode ("if a width test has a literal 9360 in it,
// that is the bug") the plan warns against.
func TestType_TableWidthsFollowA4Geometry(t *testing.T) {
	if contentWidthTwips != pageWidthTwips-pageMarginLeftTwips-pageMarginRightTwips {
		t.Fatalf("contentWidthTwips (%d) is not derived from pageWidthTwips/margins -- geometry drifted out of sync with itself", contentWidthTwips)
	}
	md := "| a | b | c |\n|---|---|---|\n| 1 | 2 | 3 |\n"
	s := generateAndReadDocumentXML(t, md)
	gridStart := strings.Index(s, "<w:tblGrid>")
	gridEnd := strings.Index(s, "</w:tblGrid>")
	if gridStart < 0 || gridEnd < 0 {
		t.Fatal("no <w:tblGrid> found")
	}
	cols := gridRE.FindAllStringSubmatch(s[gridStart:gridEnd], -1)
	sum := 0
	for _, c := range cols {
		n, _ := strconv.Atoi(c[1])
		sum += n
	}
	if sum != contentWidthTwips {
		t.Errorf("gridCol widths sum to %d, want the A4 content width %d (pageWidthTwips=%d minus margins)", sum, contentWidthTwips, pageWidthTwips)
	}
}

// 10. Heading colors/sizes are copied verbatim from the reference: H1-H3
// step down in size (32/26/24 half-points) and alternate between two blues
// (2E74B5/1F4D78); H4-H6 do NOT keep stepping the size down -- they inherit
// Normal's body size and are distinguished only by color (and, for H4,
// italic), exactly as the reference does. This replaces this package's
// prior scheme (28/26/24/22/20, no color).
func TestType_HeadingColorsAndSizesMatchReference(t *testing.T) {
	cases := []struct {
		id     string
		color  string
		sz     string // "" means: must NOT carry an explicit <w:sz>
		italic bool
	}{
		{"Heading1", "2E74B5", "32", false},
		{"Heading2", "2E74B5", "26", false},
		{"Heading3", "1F4D78", "24", false},
		{"Heading4", "2E74B5", "", true},
		{"Heading5", "2E74B5", "", false},
		{"Heading6", "1F4D78", "", false},
	}
	for _, c := range cases {
		block := styleBlock(t, buildStylesXML(), c.id)
		rPr := block[strings.Index(block, "<w:rPr>"):]
		if !strings.Contains(rPr, `<w:color w:val="`+c.color+`"/>`) {
			t.Errorf("%s: rPr = %s, want color %s", c.id, rPr, c.color)
		}
		if c.sz != "" {
			if !strings.Contains(rPr, `<w:sz w:val="`+c.sz+`"/>`) {
				t.Errorf("%s: rPr = %s, want sz %s", c.id, rPr, c.sz)
			}
		} else if strings.Contains(rPr, "<w:sz ") {
			t.Errorf("%s: rPr = %s carries an explicit <w:sz>, want it to inherit Normal's size instead", c.id, rPr)
		}
		if c.italic && !strings.Contains(rPr, "<w:i/>") {
			t.Errorf("%s: rPr = %s, want <w:i/> (italic)", c.id, rPr)
		}
		if !c.italic && strings.Contains(rPr, "<w:i/>") {
			t.Errorf("%s: rPr = %s carries <w:i/>, want it plain (only Heading4 is italic)", c.id, rPr)
		}
	}
}

// ---------------------------------------------------------------------------
// Task 2 ("段落排版 -- 首行缩进与行距") of the same plan: body paragraphs get
// a two-character first-line indent and 1.5x line spacing, headings get a
// before/after/line spacing rule, and table cells stay compact -- all
// copied verbatim from the reference document's "段落间距" table
// (.superpowers/sdd/reference-values.md) rather than invented. The one
// decision this task itself has to make (the reference hands over a value,
// not a place to put it) is WHERE the first-line indent lives: putting it
// on Normal would leak it into every basedOn=Normal style --
// SourceCode/ListParagraph/Quote, and table-cell paragraphs, which carry no
// pStyle of their own and so resolve straight to Normal -- exactly the
// mistake the plan's Task 2 section calls out by name. This package instead
// defines a dedicated BodyText style that ordinary top-level paragraphs
// reference (see write.go's renderParagraph/paraBlock.isCell), leaving
// Normal itself, and everything that stays on Normal, untouched.
// ---------------------------------------------------------------------------

// 11. BodyText's own <w:pPr> must match the reference's "正文段落" row
// exactly: <w:spacing w:after="120" w:line="360" w:lineRule="auto"/> (1.5x
// line spacing) followed by <w:ind w:firstLine="420"/> (a two-character
// indent at this document's 10.5pt body size).
func TestType_BodyTextSpacingMatchesReference(t *testing.T) {
	block := styleBlock(t, buildStylesXML(), StyleBodyText)
	if !strings.Contains(block, `<w:spacing w:after="120" w:line="360" w:lineRule="auto"/>`) {
		t.Errorf("BodyText = %s, want the reference's exact spacing", block)
	}
	if !strings.Contains(block, `<w:ind w:firstLine="420"/>`) {
		t.Errorf("BodyText = %s, want a 420-twip first-line indent", block)
	}
}

// 12. The style existing in styles.xml is not enough on its own -- an
// ordinary generated body paragraph must actually reference it.
func TestType_OrdinaryParagraphReferencesBodyText(t *testing.T) {
	s := generateAndReadDocumentXML(t, "Body text.\n")
	if !strings.Contains(s, `<w:pStyle w:val="`+StyleBodyText+`"/>`) {
		t.Errorf("ordinary paragraph does not reference BodyText:\n%s", s)
	}
}

// 13. This task's central hazard, pinned directly: the first-line indent
// must not leak into any of the four non-body cases the plan calls out by
// name. SourceCode/ListParagraph/Quote are checked against their own style
// definitions; table cells are checked against the actual rendered
// document, since a table-cell paragraph carries no pStyle of its own at
// all (see write.go's isCell) -- there is no "TableCell" style definition
// to inspect the way there is for the other three.
func TestType_FirstLineIndentDoesNotLeakIntoOtherBlocks(t *testing.T) {
	xmlDoc := buildStylesXML()
	for _, id := range []string{StyleSourceCode, StyleListParagraph, StyleQuote} {
		block := styleBlock(t, xmlDoc, id)
		if strings.Contains(block, "w:firstLine=") {
			t.Errorf("%s carries a first-line indent; only BodyText should:\n%s", id, block)
		}
	}

	md := "| a | b |\n|---|---|\n| 1 | 2 |\n"
	s := generateAndReadDocumentXML(t, md)
	tblStart := strings.Index(s, "<w:tbl>")
	tblEnd := strings.Index(s, "</w:tbl>")
	if tblStart < 0 || tblEnd < 0 {
		t.Fatal("no <w:tbl> found; test would be vacuous")
	}
	cell := s[tblStart:tblEnd]
	if strings.Contains(cell, "w:firstLine=") {
		t.Error("a table cell paragraph carries a first-line indent")
	}
	if strings.Contains(cell, `<w:pStyle w:val="`+StyleBodyText+`"/>`) {
		t.Error("a table cell paragraph references BodyText; it must stay on Normal so TableGrid's own pPr cascade governs its spacing instead")
	}
}

// 14. Table-cell paragraphs get the reference's compact "表格单元格" row:
// after=0, line=260 -- tighter than BodyText's 1.5x line spacing, which is
// the whole point (a wide table with 1.5x-spaced cells would be needlessly
// tall). This lives on TableGrid (a table-type style), not on any
// paragraph style, since a table-cell paragraph carries no pStyle of its
// own -- see tableTblPrXML's doc comment on why TableGrid's own <w:pPr>
// cascades to cell paragraphs without one.
func TestType_TableCellSpacingMatchesReference(t *testing.T) {
	block := styleBlock(t, buildStylesXML(), StyleTableGrid)
	if !strings.Contains(block, `<w:spacing w:after="0" w:line="260"/>`) {
		t.Errorf("TableGrid = %s, want the reference's exact cell spacing", block)
	}
}

// 15. Headings get the reference's single "标题" spacing rule -- before=240,
// after=160, line=360 -- applied identically to H1..H6: the reference
// measures one heading spacing rule, not a different one per level (unlike
// color/size, which DO step down -- see
// TestType_HeadingColorsAndSizesMatchReference). Differentiation between
// levels comes entirely from color/size/italic, not from spacing.
func TestType_HeadingSpacingMatchesReference(t *testing.T) {
	for _, id := range []string{"Heading1", "Heading2", "Heading3", "Heading4", "Heading5", "Heading6"} {
		block := styleBlock(t, buildStylesXML(), id)
		if !strings.Contains(block, `<w:spacing w:before="240" w:after="160" w:line="360" w:lineRule="auto"/>`) {
			t.Errorf("%s = %s, want the reference's exact heading spacing", id, block)
		}
	}
}
