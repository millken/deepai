package docx

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// This file pins the four rendering defects found by a user reviewing a
// real generated design document in Word: literal HTML entities, tables
// that use a fraction of the page and wrap every cell, striped-looking
// code blocks, and a duplicated title. See
// .superpowers/sdd/docx-write-quality-report.md for the full write-up.

// --- Defect 1: HTML entities are written literally -----------------------

// The user's exact repro: "&nbsp;" must become a real no-break space
// (U+00A0), not survive as the seven literal characters "&nbsp;", and
// "&amp;" must decode to a literal "&".
func TestWrite_HTMLEntitiesAreDecoded(t *testing.T) {
	md := "版本：V2.0 &nbsp;|&nbsp; 日期 &amp; 时间\n"
	d, _, _ := writeAndReopen(t, md)
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	want := "版本：V2.0  |  日期 & 时间"
	if text.String() != want {
		t.Errorf("text = %q, want %q (entities decoded)", text.String(), want)
	}
	doc, _ := d.Part(DocumentPart)
	if strings.Contains(string(doc), "nbsp") {
		t.Errorf("literal \"nbsp\" leaked into the XML: %s", string(doc))
	}
}

// A numeric entity, decimal or hex, decodes the same way a named one does.
func TestWrite_NumericEntitiesAreDecoded(t *testing.T) {
	md := "a&#160;b &#x2014; c\n"
	d, _, _ := writeAndReopen(t, md)
	var text strings.Builder
	for _, r := range d.Paras()[0].Runs {
		text.WriteString(r.Text)
	}
	want := "a b — c"
	if text.String() != want {
		t.Errorf("text = %q, want %q", text.String(), want)
	}
}

// An entity this package does not recognize must stay exactly as written,
// not vanish and not error.
func TestWrite_UnrecognizedEntityStaysLiteral(t *testing.T) {
	md := "price &pound;10\n"
	d, _, _ := writeAndReopen(t, md)
	var text strings.Builder
	for _, r := range d.Paras()[0].Runs {
		text.WriteString(r.Text)
	}
	if text.String() != "price &pound;10" {
		t.Errorf("text = %q, want the unrecognized entity kept literally", text.String())
	}
}

// Self-review question: a literal "&amp;lt;" already in the source (someone
// else's own escaped "&lt;") must decode EXACTLY ONCE, left to right, into
// the four literal characters "&lt;" -- never into a real "<". Decoding
// twice (or decoding after escaping instead of before) is exactly the bug
// that would corrupt this into structural markup.
func TestWrite_DoubleEscapedEntityStaysLiteral(t *testing.T) {
	md := "see &amp;lt;tag&amp;gt;\n"
	d, _, _ := writeAndReopen(t, md)
	var text strings.Builder
	for _, r := range d.Paras()[0].Runs {
		text.WriteString(r.Text)
	}
	want := "see &lt;tag&gt;"
	if text.String() != want {
		t.Errorf("text = %q, want %q (decoded exactly once)", text.String(), want)
	}
	doc, _ := d.Part(DocumentPart)
	if strings.Contains(string(doc), "<tag>") {
		t.Error("a literal \"&amp;lt;tag&amp;gt;\" in the source became real XML markup <tag> -- corrupted")
	}
}

// Item 1's own rule: code is meant to survive verbatim, so entities inside a
// fenced code block must NOT be decoded.
func TestWrite_EntitiesInsideFencedCodeBlockAreNotDecoded(t *testing.T) {
	md := "```\nif a &amp;&amp; b {}\n```\n"
	d, _, _ := writeAndReopen(t, md)
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	want := "if a &amp;&amp; b {}"
	if text.String() != want {
		t.Errorf("code text = %q, want the entity left undecoded (verbatim code)", text.String())
	}
}

// --- Defect 2: tables must use the real content width --------------------

var (
	pgSzRE  = regexp.MustCompile(`<w:pgSz w:w="(\d+)" w:h="(\d+)"/>`)
	pgMarRE = regexp.MustCompile(`<w:pgMar w:top="(\d+)" w:right="(\d+)" w:bottom="(\d+)" w:left="(\d+)"`)
	gridRE  = regexp.MustCompile(`<w:gridCol w:w="(\d+)"/>`)
)

// contentWidthFromSectPr parses <w:pgSz>/<w:pgMar> out of documentXML and
// computes the usable content width the same way a reader would: page
// width minus left and right margins. The test that uses this deliberately
// never hardcodes 9360 -- it reads the section properties WriteDocx itself
// wrote, so it keeps checking the right thing even if the page geometry
// this package writes ever changes.
func contentWidthFromSectPr(t *testing.T, documentXML string) int {
	t.Helper()
	sz := pgSzRE.FindStringSubmatch(documentXML)
	if sz == nil {
		t.Fatal("no <w:pgSz> found in document.xml")
	}
	mar := pgMarRE.FindStringSubmatch(documentXML)
	if mar == nil {
		t.Fatal("no <w:pgMar> found in document.xml")
	}
	pageW, _ := strconv.Atoi(sz[1])
	left, _ := strconv.Atoi(mar[4])
	right, _ := strconv.Atoi(mar[2])
	return pageW - left - right
}

// The three <w:gridCol> widths of a three-column table must sum to exactly
// the page's real content width, not to 6000 (three hardcoded 2000-twip
// columns, 64% of a 9360-wide page) and not to anything computed
// independently of the section properties WriteDocx itself writes.
func TestWrite_TableColumnWidthsSumToContentWidth(t *testing.T) {
	md := "| a | b | c |\n|---|---|---|\n| 1 | 2 | 3 |\n"
	d, _, _ := writeAndReopen(t, md)
	doc, _ := d.Part(DocumentPart)
	s := string(doc)

	want := contentWidthFromSectPr(t, s)

	gridStart := strings.Index(s, "<w:tblGrid>")
	gridEnd := strings.Index(s, "</w:tblGrid>")
	if gridStart < 0 || gridEnd < 0 {
		t.Fatal("no <w:tblGrid> found")
	}
	cols := gridRE.FindAllStringSubmatch(s[gridStart:gridEnd], -1)
	if len(cols) != 3 {
		t.Fatalf("got %d <w:gridCol> entries, want 3", len(cols))
	}
	sum := 0
	for _, c := range cols {
		n, _ := strconv.Atoi(c[1])
		sum += n
	}
	if sum != want {
		t.Errorf("gridCol widths sum to %d, want the page's content width %d", sum, want)
	}
	// Each column must use a meaningful share of the page, not the old
	// hardcoded 2000 -- with a 9360-wide default content area, three even
	// columns should each land near 3120.
	for _, c := range cols {
		n, _ := strconv.Atoi(c[1])
		if n < want/4 {
			t.Errorf("gridCol width %d is far narrower than a %d-wide page justifies", n, want)
		}
	}
}

// The same even-division rule must still sum exactly when the content width
// does NOT divide evenly by the column count (9360 / 7 = 1337.14...): no
// twip may be lost or invented by rounding.
func TestWrite_TableColumnWidthsSumExactlyWhenNotEvenlyDivisible(t *testing.T) {
	md := "| a | b | c | d | e | f | g |\n" +
		"|---|---|---|---|---|---|---|\n" +
		"| 1 | 2 | 3 | 4 | 5 | 6 | 7 |\n"
	d, _, _ := writeAndReopen(t, md)
	doc, _ := d.Part(DocumentPart)
	s := string(doc)

	want := contentWidthFromSectPr(t, s)
	if want%7 == 0 {
		t.Fatalf("test assumption violated: content width %d divides evenly by 7", want)
	}

	gridStart := strings.Index(s, "<w:tblGrid>")
	gridEnd := strings.Index(s, "</w:tblGrid>")
	cols := gridRE.FindAllStringSubmatch(s[gridStart:gridEnd], -1)
	if len(cols) != 7 {
		t.Fatalf("got %d <w:gridCol> entries, want 7", len(cols))
	}
	sum := 0
	widths := make([]int, len(cols))
	for i, c := range cols {
		n, _ := strconv.Atoi(c[1])
		widths[i] = n
		sum += n
	}
	if sum != want {
		t.Errorf("gridCol widths %v sum to %d, want exactly %d", widths, sum, want)
	}
	// No column should differ from another by more than a single twip --
	// the remainder-distribution rounding must stay even, not dump every
	// leftover twip onto one column.
	min, max := widths[0], widths[0]
	for _, w := range widths {
		if w < min {
			min = w
		}
		if w > max {
			max = w
		}
	}
	if max-min > 1 {
		t.Errorf("column widths %v vary by more than 1 twip", widths)
	}
}

// The table itself must be told to use the full content width (pct 100,
// i.e. w:w="5000") and must not be left as w:type="auto", which is what let
// Word shrink columns below even their declared <w:tblGrid> values to fit
// cell content -- the direct cause of cells wrapping after two or three
// characters in the user's document.
func TestWrite_TableWidthIsFullPercentNotAuto(t *testing.T) {
	md := "| a | b |\n|---|---|\n| 1 | 2 |\n"
	d, _, _ := writeAndReopen(t, md)
	doc, _ := d.Part(DocumentPart)
	s := string(doc)
	if !strings.Contains(s, `<w:tblW w:w="5000" w:type="pct"/>`) {
		t.Errorf("table is not set to 100%% (pct 5000) width: %s", s)
	}
	if strings.Contains(s, `w:type="auto"`) {
		t.Error("table still carries w:type=\"auto\", which lets Word shrink columns to fit content")
	}
}

// --- Defect 3: code blocks must not render as striped bands ---------------
//
// As of the docx-style-architecture plan's Task 2, the zero-spacing, the
// shading, and the left indent that fix this defect no longer live inline
// in document.xml at all (see TestWrite_NoInlineVisualPropertiesInDocumentXML
// -- <w:spacing>/<w:shd>/<w:ind> are banned there outright) -- they live
// once, in styles.go's SourceCode style, and every code-block paragraph
// picks all three up by referencing it via <w:pStyle w:val="SourceCode"/>.
// The three tests below now check that indirection end to end: the
// paragraph references the style, AND the style (in styles.xml) actually
// carries the property, so a dangling or incomplete style reference would
// still be caught -- checking only one half would let either "paragraph
// forgot to reference the style" or "style forgot the property" regress
// unnoticed.

// Every fenced-code-block paragraph must reference the SourceCode style,
// which suppresses the document's default paragraph spacing (before/after
// 0, single line spacing) -- or Word draws a gap between every code line
// and the shared shading reads as separate bars instead of one contiguous
// block.
func TestWrite_CodeBlockParagraphsSuppressSpacing(t *testing.T) {
	md := "```\nline one\nline two\nline three\n```\n"
	d, _, _ := writeAndReopen(t, md)
	paras := d.Paras()
	if len(paras) != 3 {
		t.Fatalf("got %d paragraphs, want 3", len(paras))
	}
	doc, _ := d.Part(DocumentPart)
	s := string(doc)
	n := strings.Count(s, `<w:pStyle w:val="SourceCode"/>`)
	if n != 3 {
		t.Errorf("found <w:pStyle w:val=\"SourceCode\"/> %d times, want 3 (one per code line)", n)
	}

	styles, _ := d.Part("word/styles.xml")
	sc := styleBlock(t, styles, "SourceCode")
	if !strings.Contains(sc, `<w:spacing w:before="0" w:after="0" w:line="240" w:lineRule="auto"/>`) {
		t.Errorf("SourceCode style does not zero paragraph spacing: %s", sc)
	}
}

// The shading must be a <w:pPr> child of the SourceCode style itself (a
// paragraph-level <w:shd>, not a per-run highlight), which is what makes
// shaded code lines read as one contiguous band rather than separate runs.
func TestWrite_CodeBlockShadingIsOnTheParagraph(t *testing.T) {
	md := "```\ncode\n```\n"
	d, _, _ := writeAndReopen(t, md)
	doc, _ := d.Part(DocumentPart)
	s := string(doc)
	if !strings.Contains(s, `<w:pStyle w:val="SourceCode"/>`) {
		t.Fatal("code paragraph does not reference the SourceCode style")
	}

	styles, _ := d.Part("word/styles.xml")
	sc := styleBlock(t, styles, "SourceCode")
	pPrStart := strings.Index(sc, "<w:pPr>")
	pPrEnd := strings.Index(sc, "</w:pPr>")
	if pPrStart < 0 || pPrEnd < 0 {
		t.Fatal("SourceCode style has no <w:pPr>")
	}
	if !strings.Contains(sc[pPrStart:pPrEnd], `w:fill="F5F5F5"`) {
		t.Errorf("shading is not inside SourceCode's <w:pPr>: %s", sc[pPrStart:pPrEnd])
	}
}

// A code block must carry a modest, non-zero left indent -- not flush at
// the page margin, which is what made the user's code sit far off to the
// left of everything else around it. The indent now lives on the
// SourceCode style rather than inline on the paragraph.
func TestWrite_CodeBlockHasAModestLeftIndent(t *testing.T) {
	md := "```\ncode\n```\n"
	d, _, _ := writeAndReopen(t, md)
	doc, _ := d.Part(DocumentPart)
	if !strings.Contains(string(doc), `<w:pStyle w:val="SourceCode"/>`) {
		t.Fatal("code paragraph does not reference the SourceCode style")
	}

	styles, _ := d.Part("word/styles.xml")
	sc := styleBlock(t, styles, "SourceCode")
	indRE := regexp.MustCompile(`<w:ind w:left="(\d+)"/>`)
	m := indRE.FindStringSubmatch(sc)
	if m == nil {
		t.Fatal("SourceCode style has no <w:ind w:left=...>")
	}
	n, _ := strconv.Atoi(m[1])
	if n <= 0 {
		t.Errorf("code indent = %d, want a modest positive value", n)
	}
	if n > 720 {
		t.Errorf("code indent = %d, want something modest (<= 720 twips, half an inch)", n)
	}
}

// --- Defect 4: Title must not duplicate a Markdown-provided H1 ------------

// The normal case a model actually hits: Title is passed AND the Markdown
// itself opens with "# <same title>". WriteDocx must not prepend a second
// Heading1 on top of it.
func TestWrite_TitleNotDuplicatedWhenMarkdownStartsWithH1(t *testing.T) {
	d, res, _ := writeAndReopen2(t, WriteOptions{
		Markdown: "# Project Report\n\nbody text\n",
		Title:    "Project Report",
	})
	paras := d.Paras()
	var h1 []Para
	for _, p := range paras {
		if p.Style == "Heading1" {
			h1 = append(h1, p)
		}
	}
	if len(h1) != 1 {
		t.Fatalf("got %d Heading1 paragraphs, want exactly 1 (Title must not duplicate Markdown's own H1): %+v", len(h1), h1)
	}
	var text strings.Builder
	for _, r := range h1[0].Runs {
		text.WriteString(r.Text)
	}
	if text.String() != "Project Report" {
		t.Errorf("Heading1 text = %q, want %q", text.String(), "Project Report")
	}
	// Total paragraphs: heading + body, not heading + heading + body.
	if len(paras) != 2 {
		t.Errorf("got %d paragraphs, want 2 (no duplicated title paragraph)", len(paras))
	}
	if res.Paras != 2 {
		t.Errorf("WriteResult.Paras = %d, want 2", res.Paras)
	}
}

// Title IS still prepended when Markdown's first heading is not level 1
// (e.g. it opens with an H2, or with no heading at all) -- the guard is
// specifically "starts with H1", not "has any heading anywhere".
func TestWrite_TitleStillPrependedWhenMarkdownStartsWithH2(t *testing.T) {
	d, _, _ := writeAndReopen2(t, WriteOptions{
		Markdown: "## Not a title\n\nbody\n",
		Title:    "Real Title",
	})
	paras := d.Paras()
	var h1, h2 int
	for _, p := range paras {
		switch p.Style {
		case "Heading1":
			h1++
		case "Heading2":
			h2++
		}
	}
	if h1 != 1 {
		t.Errorf("got %d Heading1 paragraphs, want 1 (Title should still be prepended)", h1)
	}
	if h2 != 1 {
		t.Errorf("got %d Heading2 paragraphs, want 1 (Markdown's own H2 must survive)", h2)
	}
}

// A blank line (or several) before Markdown's "# ..." must not defeat the
// duplicate-detection.
func TestWrite_TitleNotDuplicatedAcrossLeadingBlankLines(t *testing.T) {
	d, _, _ := writeAndReopen2(t, WriteOptions{
		Markdown: "\n\n  \n# Same Title\n\nbody\n",
		Title:    "Same Title",
	})
	paras := d.Paras()
	var h1 int
	for _, p := range paras {
		if p.Style == "Heading1" {
			h1++
		}
	}
	if h1 != 1 {
		t.Errorf("got %d Heading1 paragraphs, want 1", h1)
	}
}

// Title must additionally populate docProps/core.xml's <dc:title>, the
// OPC-standard location Word's File > Info panel reads from.
func TestWrite_TitlePopulatesDocPropsCoreXML(t *testing.T) {
	d, _, _ := writeAndReopen2(t, WriteOptions{Markdown: "body\n", Title: "My Report"})

	core, ok := d.Part("docProps/core.xml")
	if !ok {
		t.Fatal("docProps/core.xml part is missing")
	}
	if !strings.Contains(string(core), "<dc:title>My Report</dc:title>") {
		t.Errorf("docProps/core.xml does not carry the title: %s", string(core))
	}

	ct, ok := d.Part("[Content_Types].xml")
	if !ok {
		t.Fatal("[Content_Types].xml missing")
	}
	if !strings.Contains(string(ct), `PartName="/docProps/core.xml"`) {
		t.Error("[Content_Types].xml does not declare docProps/core.xml")
	}

	rels, ok := d.Part("_rels/.rels")
	if !ok {
		t.Fatal("_rels/.rels missing")
	}
	if !strings.Contains(string(rels), `Target="docProps/core.xml"`) {
		t.Error("_rels/.rels does not register a relationship to docProps/core.xml")
	}
}

// A title-less document must NOT gain a docProps/core.xml part at all --
// the extra part and its registrations are conditional on Title being set.
func TestWrite_NoTitleMeansNoDocPropsPart(t *testing.T) {
	d, _, _ := writeAndReopen(t, "# H\n\nbody\n")
	if _, ok := d.Part("docProps/core.xml"); ok {
		t.Error("docProps/core.xml exists even though no Title was given")
	}
	ct, _ := d.Part("[Content_Types].xml")
	if strings.Contains(string(ct), "core.xml") {
		t.Error("[Content_Types].xml declares core.xml even though no Title was given")
	}
}

// --- All four together, on one realistic design document -----------------

// A single design document exercising headings, entity-bearing prose, a
// nested list, a wide three-column table, a fenced code block, and a link
// -- with Title passed AND Markdown opening with its own "# ..." line, the
// normal case that used to triple the title. Every one of the four fixes
// must hold at once, not just in isolation.
func TestWrite_DesignDocumentAllFourDefectsHoldTogether(t *testing.T) {
	md := "# 设计文档\n\n" +
		"## 概述\n\n" +
		"版本：V2.0 &nbsp;|&nbsp; 日期 &amp; 时间\n\n" +
		"## 计划\n\n" +
		"- 平台选品\n" +
		"  - 提前上架\n" +
		"  - 完成审核\n" +
		"- 上线发布\n\n" +
		"## 排期\n\n" +
		"| 阶段 | 负责人 | 说明 |\n" +
		"|---|---|---|\n" +
		"| 平台选品，提前上架 | 张三 | 需要协调多个部门资源 |\n" +
		"| 内容审核与合规检查 | 李四 | 涉及法务与运营两方确认 |\n\n" +
		"## 示例代码\n\n" +
		"```go\n" +
		"func Run() error {\n" +
		"    return nil\n" +
		"}\n" +
		"```\n\n" +
		"详见 [设计规范](https://example.com/spec) 说明。\n"

	d, res, _ := writeAndReopen2(t, WriteOptions{Markdown: md, Title: "设计文档"})
	paras := d.Paras()

	textOf := func(p Para) string {
		var b strings.Builder
		for _, r := range p.Runs {
			b.WriteString(r.Text)
		}
		return b.String()
	}

	if len(res.Notes) != 0 {
		t.Errorf("Notes = %v, want none for this fully-supported document", res.Notes)
	}

	// Defect 4: exactly one Heading1, reading the title once, not thrice.
	var h1 []Para
	for _, p := range paras {
		if p.Style == "Heading1" {
			h1 = append(h1, p)
		}
	}
	if len(h1) != 1 || textOf(h1[0]) != "设计文档" {
		t.Errorf("Heading1 paragraphs = %+v, want exactly one reading %q", h1, "设计文档")
	}
	core, ok := d.Part("docProps/core.xml")
	if !ok || !strings.Contains(string(core), "<dc:title>设计文档</dc:title>") {
		t.Errorf("docProps/core.xml title missing or wrong: %v %s", ok, string(core))
	}

	// Defect 1: entities decoded in ordinary prose.
	foundDecoded := false
	for _, p := range paras {
		if textOf(p) == "版本：V2.0  |  日期 & 时间" {
			foundDecoded = true
		}
	}
	if !foundDecoded {
		t.Error("could not find the paragraph with entities decoded")
	}
	doc, _ := d.Part(DocumentPart)
	docStr := string(doc)
	if strings.Contains(docStr, "nbsp") {
		t.Error("literal \"nbsp\" leaked into the XML")
	}

	// Nested list still renders (unrelated to the four defects, but part of
	// "a realistic design document").
	foundSub := false
	for _, p := range paras {
		if textOf(p) == "提前上架" {
			foundSub = true
		}
	}
	if !foundSub {
		t.Error("nested list item missing")
	}

	// Defect 2: the three-column table's grid sums to the real content
	// width, computed from this document's own section properties.
	want := contentWidthFromSectPr(t, docStr)
	gridStart := strings.Index(docStr, "<w:tblGrid>")
	gridEnd := strings.Index(docStr, "</w:tblGrid>")
	if gridStart < 0 || gridEnd < 0 {
		t.Fatal("no <w:tblGrid> found")
	}
	cols := gridRE.FindAllStringSubmatch(docStr[gridStart:gridEnd], -1)
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
	if !strings.Contains(docStr, `<w:tblW w:w="5000" w:type="pct"/>`) {
		t.Error("table is not set to full content width")
	}
	// The long Chinese cell text must still be present in full (not
	// truncated) -- what matters for the defect is that the COLUMN is wide
	// enough for Word to lay it out without wrapping every couple of
	// characters, which this test cannot observe directly, but the cell's
	// full text surviving is at least a necessary condition.
	foundLongCell := false
	for _, p := range paras {
		if textOf(p) == "平台选品，提前上架" {
			foundLongCell = true
		}
	}
	if !foundLongCell {
		t.Error("the long table cell text did not survive intact")
	}

	// Defect 3: every code-block paragraph references SourceCode, which
	// (per TestWrite_CodeBlockParagraphsSuppressSpacing /
	// TestWrite_CodeBlockShadingIsOnTheParagraph) is the style now
	// carrying the zero-spacing and shading that keep code lines
	// contiguous -- document.xml itself carries neither inline anymore.
	styleRefCount := strings.Count(docStr, `<w:pStyle w:val="SourceCode"/>`)
	if styleRefCount != 3 {
		t.Errorf("<w:pStyle w:val=\"SourceCode\"/> found %d times, want 3 (one per code line: func Run/return nil/})", styleRefCount)
	}
	styles, _ := d.Part("word/styles.xml")
	if !strings.Contains(string(styles), `w:fill="F5F5F5"`) {
		t.Error("no code shading found in the SourceCode style")
	}
	foundCodeLine := false
	for _, p := range paras {
		if textOf(p) == "    return nil" {
			foundCodeLine = true
		}
	}
	if !foundCodeLine {
		t.Error("code block line missing or markdown was interpreted inside it")
	}

	// Link still renders as a real hyperlink.
	if !strings.Contains(docStr, "<w:hyperlink") {
		t.Error("no hyperlink found")
	}
	rels, _ := d.Part("word/_rels/document.xml.rels")
	if !strings.Contains(string(rels), `Target="https://example.com/spec"`) {
		t.Error("hyperlink relationship does not target the expected URL")
	}
}
