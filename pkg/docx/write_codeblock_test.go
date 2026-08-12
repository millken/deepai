package docx

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// This file covers three defects in how docx_write renders code blocks,
// reported against real generated output (see
// .superpowers/sdd/code-block-report.md for the first two, and
// .superpowers/sdd/code-single-paragraph-report.md for the third):
//
//   - Defect 1: CommonMark's INDENTED code block (four spaces or a tab, no
//     fence) was not recognized at all -- it fell through to buildBlocks'
//     ordinary-paragraph fallback, which is exactly what the user's
//     screenshot showed (JSON in a proportional font, indentation collapsed
//     by the soft-line-break join, no shading).
//   - Defect 2: a code block had no visible container -- Word's paragraph
//     shading draws edge to edge with no padding, so shaded code sat flush
//     against the page margin and the surrounding prose with nothing
//     visually containing it.
//   - Defect 3 (later task): a code block rendered as one <w:p> PER LINE,
//     relying on Word's own behavior of merging adjacent paragraphs that
//     share byte-identical <w:pBdr>/<w:shd> into a single visual box. A
//     renderer that does not do that merging (the user's own renderer,
//     per a second screenshot) instead draws one box per line -- a
//     multi-line block looks like a stack of separate boxes with a line
//     between every row. The fix, covered by the tests below whose names
//     mention "one paragraph"/"SingleParagraph"/"Breaks", is to always
//     render a whole code BLOCK as one paragraph, its lines separated by
//     <w:br/>, so the border/shading is structurally one box regardless of
//     what the renderer does or does not merge.

// sourceCodeParas returns, in document order, every paragraph in d styled
// SourceCode.
func sourceCodeParas(d *Document) []Para {
	var out []Para
	for _, p := range d.Paras() {
		if p.Style == StyleSourceCode {
			out = append(out, p)
		}
	}
	return out
}

// codeBlockLines returns the per-line view these tests have always wanted,
// recovered from the post-code-single-paragraph shape: one code BLOCK is
// now one SourceCode paragraph (write.go's mergeCodeBlocks) whose lines are
// joined internally by <w:br/> (renderCodeBlockRuns) rather than each being
// its own paragraph. paraTextWithBreaks (read.go, already exercised by
// TestScan_BreaksRecordRunPositions/TestRead_ParaTextWithBreaks* before this
// task ever touched write.go) turns those <w:br/> positions back into "\n",
// so splitting on "\n" recovers exactly the lines write.go was given. Every
// SourceCode paragraph found contributes its own lines, in order, so a
// document with more than one separate code block still flattens to the
// full, in-order line list callers used to get for free from "one paragraph
// per line".
func codeBlockLines(d *Document) []string {
	var lines []string
	for _, p := range sourceCodeParas(d) {
		lines = append(lines, strings.Split(paraTextWithBreaks(p), "\n")...)
	}
	return lines
}

// ---------------------------------------------------------------------------
// Defect 1: every code-block form must produce a SourceCode-styled
// paragraph. Table-driven so a future form (e.g. another fence variant)
// cannot be added without a case here.
//
// As of the code-single-paragraph task, a code BLOCK -- fenced or indented,
// one line or many -- renders as exactly ONE SourceCode paragraph, not one
// per line (see write.go's mergeCodeBlocks/renderCodeBlockRuns' own doc
// comments for why: a renderer that does not merge adjacent identical
// <w:pBdr> paragraphs the way Word does would otherwise draw a separate box
// around every line). wantParas pins that paragraph count directly, and
// wantLines pins the per-line content recovered via codeBlockLines.
// ---------------------------------------------------------------------------

func TestWrite_CodeBlockFormsAllProduceSourceCodeStyle(t *testing.T) {
	cases := []struct {
		name      string
		md        string
		wantLines []string
	}{
		{"bare fence", "```\ncode line\n```\n", []string{"code line"}},
		{"fence with info string", "```go\ncode line\n```\n", []string{"code line"}},
		{"tilde fence", "~~~\ncode line\n~~~\n", []string{"code line"}},
		{"four-backtick fence", "````\ncode line\n````\n", []string{"code line"}},
		{"indented (four spaces, no fence)", "    code line\n", []string{"code line"}},
		{"indented (one tab, no fence)", "\tcode line\n", []string{"code line"}},
		{"indented, multiple lines", "    line one\n    line two\n", []string{"line one", "line two"}},
		{"fenced, multiple lines", "```\nline one\nline two\n```\n", []string{"line one", "line two"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _ := writeAndReopen(t, tc.md)
			paras := sourceCodeParas(d)
			if len(paras) != 1 {
				t.Fatalf("got %d SourceCode paragraphs, want exactly 1 (one per BLOCK, not one per line): %+v", len(paras), paras)
			}
			got := strings.Split(paraTextWithBreaks(paras[0]), "\n")
			if !reflect.DeepEqual(got, tc.wantLines) {
				t.Errorf("SourceCode paragraph lines = %q, want %q", got, tc.wantLines)
			}
		})
	}
}

// The exact stripping rule: exactly four leading spaces (or one tab) come
// off each line, and the REST -- including any further indentation -- must
// survive verbatim. This is the user's own JSON example.
func TestWrite_IndentedCodeBlockStripsExactlyFourSpaces(t *testing.T) {
	md := "**Request**\n\n    {\n      \"a\": 1\n    }\n"
	d, _, _ := writeAndReopen(t, md)

	paras := sourceCodeParas(d)
	if len(paras) != 1 {
		t.Fatalf("got %d SourceCode paragraphs, want 1 (the whole indented block is one paragraph): %+v", len(paras), paras)
	}
	codeTexts := strings.Split(paraTextWithBreaks(paras[0]), "\n")
	want := []string{"{", "  \"a\": 1", "}"}
	if !reflect.DeepEqual(codeTexts, want) {
		t.Errorf("code lines = %q, want %q (six leading spaces must strip to two, not zero or six)", codeTexts, want)
	}

	// And the paragraph that opened the block ("**Request**") must NOT have
	// been swallowed into it -- it stays an ordinary bold paragraph.
	doc, _ := d.Part(DocumentPart)
	if !strings.Contains(string(doc), "<w:b/>") {
		t.Error("the paragraph preceding the indented code block lost its bold run")
	}
}

// An indented code block continues through blank lines -- a blank line
// inside the block becomes a literal empty code paragraph, not a block
// terminator.
func TestWrite_IndentedCodeBlockContinuesThroughBlankLines(t *testing.T) {
	md := "    line one\n\n    line two\n"
	d, _, _ := writeAndReopen(t, md)

	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1 (the whole indented block, blank line included, is one paragraph): %+v", len(paras), paras)
	}
	if paras[0].Style != StyleSourceCode {
		t.Errorf("paras[0].Style = %q, want %q", paras[0].Style, StyleSourceCode)
	}
	got := strings.Split(paraTextWithBreaks(paras[0]), "\n")
	want := []string{"line one", "", "line two"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("code lines = %q, want %q (blank line must survive as an empty code line, i.e. a bare <w:br/> with no <w:t>)", got, want)
	}
}

// Trailing blank lines are NOT part of the block: a blank line (or several)
// followed by dedented content ends the block right where the indentation
// dropped, and the blanks are simply dropped rather than becoming empty
// code paragraphs.
func TestWrite_IndentedCodeBlockDropsTrailingBlankLines(t *testing.T) {
	md := "    code one\n\n\nNext paragraph\n"
	d, _, _ := writeAndReopen(t, md)
	paras := d.Paras()
	if len(paras) != 2 {
		t.Fatalf("got %d paragraphs, want 2 (trailing blanks must not become empty code paragraphs): %+v", len(paras), paras)
	}
	if paras[0].Style != StyleSourceCode {
		t.Errorf("paras[0].Style = %q, want %q", paras[0].Style, StyleSourceCode)
	}
	if paras[1].Style == StyleSourceCode {
		t.Error("the dedented paragraph after the trailing blanks was misclassified as code")
	}
	var text strings.Builder
	for _, r := range paras[1].Runs {
		text.WriteString(r.Text)
	}
	if text.String() != "Next paragraph" {
		t.Errorf("paras[1] text = %q, want %q", text.String(), "Next paragraph")
	}
}

// ---------------------------------------------------------------------------
// Defect 1's hazard: the list interaction. Continuation content inside a
// list item is also indented, so a naive four-space rule turns list bodies
// into code -- these pin the cases the brief calls out by name.
// ---------------------------------------------------------------------------

// A two-level nested list must still render as a list (numPr/ilvl), not as
// code, once indented-code detection exists alongside it.
func TestWrite_TwoLevelNestedListStillRendersAsList(t *testing.T) {
	md := "- top\n    - nested\n        - double nested\n"
	d, _, _ := writeAndReopen(t, md)
	doc, _ := d.Part(DocumentPart)
	s := string(doc)

	for _, want := range []string{`<w:ilvl w:val="0"/>`, `<w:ilvl w:val="1"/>`, `<w:ilvl w:val="2"/>`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s: %s", want, s)
		}
	}
	if strings.Contains(s, `<w:pStyle w:val="SourceCode"/>`) {
		t.Error("nested list content was misclassified as an indented code block")
	}
	paras := d.Paras()
	if len(paras) != 3 {
		t.Fatalf("got %d paragraphs, want 3 (one per list item)", len(paras))
	}
	for i, p := range paras {
		if p.Style == StyleSourceCode {
			t.Errorf("paras[%d] rendered as code, not a list item", i)
		}
	}
}

// A list item's own indented continuation paragraph -- content belonging to
// the item but written on a following, indented line -- must not be
// swallowed as code. (This package does not reconstruct it as part of the
// list item either; the point pinned here is narrower: it must not become
// code.)
func TestWrite_ListItemIndentedContinuationNotSwallowedAsCode(t *testing.T) {
	md := "- top item\n    continuation still about top item\n"
	d, _, _ := writeAndReopen(t, md)
	doc, _ := d.Part(DocumentPart)
	s := string(doc)
	if strings.Contains(s, `<w:pStyle w:val="SourceCode"/>`) {
		t.Errorf("list-item continuation became code:\n%s", s)
	}

	var texts []string
	for _, p := range d.Paras() {
		var b strings.Builder
		for _, r := range p.Runs {
			b.WriteString(r.Text)
		}
		texts = append(texts, b.String())
	}
	found := false
	for _, tx := range texts {
		if tx == "continuation still about top item" {
			found = true
		}
	}
	if !found {
		t.Errorf("continuation text not found verbatim among paragraphs: %q", texts)
	}
}

// Self-review case: an eight-space line under a NESTED list item (which
// itself sits at a deeper indent) must also stay out of code -- the list
// context, once open, is not reset merely because the nesting is deeper.
func TestWrite_ContinuationUnderNestedListItemNotSwallowedAsCode(t *testing.T) {
	md := "- top\n  - nested\n        deep continuation\n"
	d, _, _ := writeAndReopen(t, md)
	doc, _ := d.Part(DocumentPart)
	if strings.Contains(string(doc), `<w:pStyle w:val="SourceCode"/>`) {
		t.Errorf("continuation under a nested list item became code:\n%s", string(doc))
	}
}

// Self-review case: a fenced code block INSIDE a list item must still work
// as a fence -- the list-interaction guard for the new INDENTED form must
// not interfere with the pre-existing, independent fence mechanism.
func TestWrite_FencedBlockInsideListItemStillBecomesCode(t *testing.T) {
	md := "- top\n  ```\n  code\n  ```\n"
	d, _, _ := writeAndReopen(t, md)
	var codeTexts []string
	for _, p := range d.Paras() {
		if p.Style == StyleSourceCode {
			var b strings.Builder
			for _, r := range p.Runs {
				b.WriteString(r.Text)
			}
			codeTexts = append(codeTexts, b.String())
		}
	}
	want := []string{"  code"}
	if !reflect.DeepEqual(codeTexts, want) {
		t.Errorf("fenced code inside a list item = %q, want %q", codeTexts, want)
	}
}

// Once a list has unambiguously ended (an unindented, non-list line), a
// LATER indented block in the same document must be recognized as code
// again -- inListContext must not stay stuck true for the rest of the
// document.
func TestWrite_IndentedCodeBlockRecognizedAfterListEnds(t *testing.T) {
	md := "- item\n\nParagraph back at the top level.\n\n    now this is code\n"
	d, _, _ := writeAndReopen(t, md)
	var codeTexts []string
	for _, p := range d.Paras() {
		if p.Style == StyleSourceCode {
			var b strings.Builder
			for _, r := range p.Runs {
				b.WriteString(r.Text)
			}
			codeTexts = append(codeTexts, b.String())
		}
	}
	want := []string{"now this is code"}
	if !reflect.DeepEqual(codeTexts, want) {
		t.Errorf("code lines after the list ended = %q, want %q", codeTexts, want)
	}
}

// ---------------------------------------------------------------------------
// Defect 2: a code block needs a real visual container -- a border with
// padding, referenced from a style AND, as of the GenOffice-compatibility
// task, ALSO copied directly onto each code paragraph (see styles.go's
// codeBorderXML doc comment: GenOffice does not apply a paragraph style's
// <w:pBdr> at all, so the style-only reference this test used to require is
// not sufficient on its own).
// ---------------------------------------------------------------------------

func TestWrite_CodeBlockContainerBorderLivesInStyleAndInline(t *testing.T) {
	md := "```\nline one\nline two\n```\n"
	d, _, _ := writeAndReopen(t, md)
	doc, _ := d.Part(DocumentPart)
	if !strings.Contains(string(doc), "<w:pBdr") {
		t.Errorf("document.xml has no inline <w:pBdr> on the code paragraph; GenOffice does not resolve the SourceCode style's border, so a code block would show no box in it:\n%s", string(doc))
	}

	styles, _ := d.Part("word/styles.xml")
	sc := styleBlock(t, styles, StyleSourceCode)
	if !strings.Contains(sc, "<w:pBdr>") {
		t.Fatalf("SourceCode style has no <w:pBdr>; a code block has no visible container: %s", sc)
	}
	for _, side := range []string{"top", "left", "bottom", "right"} {
		if !strings.Contains(sc, "<w:"+side+` w:val="single"`) {
			t.Errorf("SourceCode's pBdr is missing a %s border (not a full box): %s", side, sc)
		}
	}
	// A border with no padding just moves the "flush against the text"
	// problem to a new edge -- w:space is what actually inserts a gap
	// between the border line and the text.
	if !strings.Contains(sc, `w:space="`) {
		t.Errorf("SourceCode's pBdr carries no w:space (padding): %s", sc)
	}
}

// The background shading must still be contiguous across a multi-line code
// block -- unchanged behavior, re-pinned here alongside the new border so a
// regression in either doesn't silently pass the other.
func TestWrite_CodeBlockShadingStaysContiguousAlongsideBorder(t *testing.T) {
	styles := buildStylesXML()
	sc := styleBlock(t, styles, StyleSourceCode)
	if !strings.Contains(sc, `w:fill="F5F5F5"`) {
		t.Errorf("SourceCode lost its shading: %s", sc)
	}
	if !strings.Contains(sc, "<w:contextualSpacing/>") {
		t.Errorf("SourceCode lost contextualSpacing; adjacent code lines would show gaps again: %s", sc)
	}
}

// A code block must avoid being stranded away from the paragraph that
// immediately follows it: keepNext, the same mechanism the heading styles
// use for the identical reason (TestStyles_HeadingsKeepWithNext).
//
// keepLines is deliberately absent -- re-pinned here as the negative half
// of the same assertion, not merely an accommodation. Before the
// code-single-paragraph task, a code block was one paragraph PER LINE, so
// <w:keepLines/> ("keep every line of THIS paragraph on one page") was a
// near no-op there; the real "whole block stays together" effect came
// instead from keepNext chaining each line-paragraph to the next one.
// Collapsing the block to a single paragraph turns THAT SAME keepLines
// property into "keep the entire block on one page", which cannot be
// honored for a block longer than a page -- see
// TestWrite_LongCodeBlockDoesNotForceKeepLines for what carrying it anyway
// would do to pagination, and sourceCodeStyleXML's own doc comment for the
// full reasoning.
func TestWrite_CodeBlockKeepsNextButNotLines(t *testing.T) {
	styles := buildStylesXML()
	sc := styleBlock(t, styles, StyleSourceCode)
	if !strings.Contains(sc, "<w:keepNext/>") {
		t.Errorf("SourceCode has no <w:keepNext/>; a code block can be stranded away from the text that follows it: %s", sc)
	}
	if strings.Contains(sc, "<w:keepLines/>") {
		t.Errorf("SourceCode carries <w:keepLines/>; on a single-paragraph code block that means \"keep the WHOLE block on one page\", which is unsatisfiable for a block longer than a page: %s", sc)
	}
}

// Self-review: does a code block longer than a page paginate sensibly, or
// does it try to keep a whole page's worth together and leave a gap? This
// exercises the actual generated document (not just the static style
// string above) end to end: a 200-line fenced block is still exactly one
// SourceCode paragraph, and that paragraph's own <w:pPr> -- like the style
// it references -- must not carry <w:keepLines/>, so Word is free to break
// it at a page boundary like ordinary body text instead of forcing the
// whole thing onto whichever page has room.
func TestWrite_LongCodeBlockDoesNotForceKeepLines(t *testing.T) {
	var b strings.Builder
	b.WriteString("```\n")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	b.WriteString("```\n")
	d, _, _ := writeAndReopen(t, b.String())

	paras := sourceCodeParas(d)
	if len(paras) != 1 {
		t.Fatalf("got %d SourceCode paragraphs, want 1 even for a 200-line block", len(paras))
	}
	lines := strings.Split(paraTextWithBreaks(paras[0]), "\n")
	if len(lines) != 200 {
		t.Fatalf("got %d lines back, want 200", len(lines))
	}

	doc, _ := d.Part(DocumentPart)
	s := string(doc)
	paraRE := regexp.MustCompile(`<w:p>.*?</w:p>`)
	codeP := ""
	for _, p := range paraRE.FindAllString(s, -1) {
		if strings.Contains(p, `<w:pStyle w:val="SourceCode"/>`) {
			codeP = p
			break
		}
	}
	if codeP == "" {
		t.Fatal("no SourceCode paragraph found in document.xml")
	}
	if strings.Contains(codeP, "<w:keepLines/>") {
		t.Errorf("the code paragraph carries an inline <w:keepLines/>; a long block would be forced onto one page, leaving a gap on the page before it: %s", codeP[:200])
	}
}

// ---------------------------------------------------------------------------
// The code-single-paragraph task itself: one <w:p> per code BLOCK, its
// lines separated by <w:br/> rather than by paragraph boundaries, so a
// renderer that (unlike Word) draws every paragraph's own <w:pBdr>
// separately still shows one box, not one per line.
// ---------------------------------------------------------------------------

// The exact wire shape: a multi-line block is one <w:p>, and consecutive
// lines are separated by a textless "<w:r><w:br/></w:r>" -- the same shape
// scan.go's Para.Breaks already recognizes (TestScan_BreaksRecordRunPositions
// pins that scanner-side half against synthetic input; this pins write.go's
// production side against a real generated document), never an embedded
// literal "\n" character inside a single <w:t>, which scan.go has no way to
// tell apart from ordinary text.
func TestWrite_MultiLineCodeBlockUsesSingleParagraphWithBreaks(t *testing.T) {
	d, _, _ := writeAndReopen(t, "```\nfirst\nsecond\nthird\n```\n")

	doc, _ := d.Part(DocumentPart)
	s := string(doc)
	paraRE := regexp.MustCompile(`<w:p>.*?</w:p>`)
	paras := paraRE.FindAllString(s, -1)
	var codeParas []string
	for _, p := range paras {
		if strings.Contains(p, `<w:pStyle w:val="SourceCode"/>`) {
			codeParas = append(codeParas, p)
		}
	}
	if len(codeParas) != 1 {
		t.Fatalf("got %d <w:p> carrying pStyle=SourceCode, want exactly 1: %v", len(codeParas), codeParas)
	}
	codeP := codeParas[0]

	if n := strings.Count(codeP, "<w:br/>"); n != 2 {
		t.Errorf("code paragraph has %d <w:br/>, want 2 (one between each of the 3 lines): %s", n, codeP)
	}
	if !strings.Contains(codeP, "<w:r><w:br/></w:r>") {
		t.Errorf("a <w:br/> does not sit alone in its own textless <w:r>: %s", codeP)
	}
	// The three lines must each still be their own <w:t>, in order --
	// confirming the break is a separate run, not text spliced into one
	// <w:t> alongside a literal newline character.
	tRE := regexp.MustCompile(`<w:t[^>]*>([^<]*)</w:t>`)
	var texts []string
	for _, m := range tRE.FindAllStringSubmatch(codeP, -1) {
		texts = append(texts, m[1])
	}
	if want := []string{"first", "second", "third"}; !reflect.DeepEqual(texts, want) {
		t.Errorf("<w:t> texts in order = %q, want %q", texts, want)
	}

	// And the scanner recovers exactly that, via Para.Breaks.
	paras2 := sourceCodeParas(d)
	if len(paras2) != 1 {
		t.Fatalf("got %d SourceCode paragraphs via Scan, want 1", len(paras2))
	}
	if len(paras2[0].Breaks) != 2 {
		t.Errorf("Para.Breaks = %v, want 2 entries (one break after run 1, one after run 2)", paras2[0].Breaks)
	}
}

// Leading indentation must survive verbatim on EVERY line of a fenced
// block, not just the first -- each line is its own <w:t xml:space=
// "preserve">, independent of its neighbors, exactly as it was when each
// line was its own paragraph.
func TestWrite_FencedCodeBlockPreservesLeadingWhitespacePerLine(t *testing.T) {
	md := "```\nif true {\n    return 1\n        return 2\n}\n```\n"
	d, _, _ := writeAndReopen(t, md)

	paras := sourceCodeParas(d)
	if len(paras) != 1 {
		t.Fatalf("got %d SourceCode paragraphs, want 1", len(paras))
	}
	got := strings.Split(paraTextWithBreaks(paras[0]), "\n")
	want := []string{"if true {", "    return 1", "        return 2", "}"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("lines = %q, want %q (leading whitespace must survive on every line)", got, want)
	}

	doc, _ := d.Part(DocumentPart)
	s := string(doc)
	for _, indented := range []string{"    return 1", "        return 2"} {
		re := regexp.MustCompile(`<w:t xml:space="preserve">` + regexp.QuoteMeta(indented) + `</w:t>`)
		if !re.MatchString(s) {
			t.Errorf("no xml:space=\"preserve\" <w:t> found for indented line %q in: %s", indented, s)
		}
	}
}

// A blank line inside a FENCED block (the indented-block case is already
// covered by TestWrite_IndentedCodeBlockContinuesThroughBlankLines) must
// also survive as a bare <w:br/> with no accompanying <w:t>, not be dropped.
func TestWrite_FencedCodeBlockBlankLineSurvives(t *testing.T) {
	d, _, _ := writeAndReopen(t, "```\nabove\n\nbelow\n```\n")
	paras := sourceCodeParas(d)
	if len(paras) != 1 {
		t.Fatalf("got %d SourceCode paragraphs, want 1", len(paras))
	}
	got := strings.Split(paraTextWithBreaks(paras[0]), "\n")
	want := []string{"above", "", "below"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
}

// Self-review case, named directly in renderRun's own doc comment: a blank
// line as the very FIRST line of a fenced block (immediately after the
// opening fence) puts a <w:br/> before any <w:t> has appeared in the
// paragraph at all. read.go's paraTextWithBreaks only ever emits "\n"
// AFTER a run's text in its loop over p.Runs, so a break with no
// preceding run would otherwise vanish outright, not merely merge with a
// neighbor -- worse than the interior-blank-line case
// TestWrite_FencedCodeBlockBlankLineSurvives covers. Giving the blank
// first line its own (empty) run, exactly like any other line, is what
// keeps this case from being a second, independent bug.
func TestWrite_FencedCodeBlockLeadingBlankLineSurvives(t *testing.T) {
	d, _, _ := writeAndReopen(t, "```\n\nbelow\n```\n")
	paras := sourceCodeParas(d)
	if len(paras) != 1 {
		t.Fatalf("got %d SourceCode paragraphs, want 1", len(paras))
	}
	got := strings.Split(paraTextWithBreaks(paras[0]), "\n")
	want := []string{"", "below"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("lines = %q, want %q (a blank line opening the block must survive)", got, want)
	}
}

// Multiple consecutive blank lines inside a block, not just one, must all
// survive: N blank lines in a row need N breaks in a row with N-1 empty
// runs anchoring them, or scan.go's paraBreaks would record every one of
// them as landing "after the same run", collapsing all but the first.
func TestWrite_FencedCodeBlockMultipleConsecutiveBlankLinesSurvive(t *testing.T) {
	d, _, _ := writeAndReopen(t, "```\na\n\n\nb\n```\n")
	paras := sourceCodeParas(d)
	if len(paras) != 1 {
		t.Fatalf("got %d SourceCode paragraphs, want 1", len(paras))
	}
	got := strings.Split(paraTextWithBreaks(paras[0]), "\n")
	want := []string{"a", "", "", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("lines = %q, want %q (both blank lines must survive)", got, want)
	}
}

// Both forms take the single-paragraph path: a fenced AND an indented
// multi-line block, in the SAME document, must each collapse to exactly
// one SourceCode paragraph -- not just in isolation (already pinned by
// TestWrite_CodeBlockFormsAllProduceSourceCodeStyle's table), but also when
// they appear back to back with ordinary prose keeping them apart, so
// there is no cross-contamination between the two code paths' own
// accumulation.
func TestWrite_FencedAndIndentedBlocksBothCollapseToOneParagraphEach(t *testing.T) {
	md := "```\nfenced one\nfenced two\n```\n\nprose in between\n\n    indented one\n    indented two\n"
	d, _, _ := writeAndReopen(t, md)
	paras := sourceCodeParas(d)
	if len(paras) != 2 {
		t.Fatalf("got %d SourceCode paragraphs, want 2 (one for the fenced block, one for the indented block): %+v", len(paras), paras)
	}
	if got, want := strings.Split(paraTextWithBreaks(paras[0]), "\n"), []string{"fenced one", "fenced two"}; !reflect.DeepEqual(got, want) {
		t.Errorf("fenced block lines = %q, want %q", got, want)
	}
	if got, want := strings.Split(paraTextWithBreaks(paras[1]), "\n"), []string{"indented one", "indented two"}; !reflect.DeepEqual(got, want) {
		t.Errorf("indented block lines = %q, want %q", got, want)
	}
}

// The generated document must still reopen through OpenDocument (already
// exercised by every test above via writeAndReopen) AND format through
// every docx_format rule this package's Document.Format supports, with a
// multi-line, <w:br/>-bearing code block present -- the compose guarantee,
// applied specifically to this task's new shape rather than only to the
// pre-existing one-paragraph-per-line documents write_format_compose_test.go
// covers.
func TestWrite_MultiLineCodeBlockDocumentReopensAndFormats(t *testing.T) {
	md := "# Title\n\nSome body text.\n\n```go\nfunc main() {\n    fmt.Println(\"hi\")\n\n    return\n}\n```\n\nMore text after.\n"
	d, _, _ := writeAndReopen(t, md)

	paras := sourceCodeParas(d)
	if len(paras) != 1 {
		t.Fatalf("got %d SourceCode paragraphs, want 1", len(paras))
	}
	wantLines := []string{"func main() {", "    fmt.Println(\"hi\")", "", "    return", "}"}
	if got := strings.Split(paraTextWithBreaks(paras[0]), "\n"); !reflect.DeepEqual(got, wantLines) {
		t.Fatalf("code lines = %q, want %q", got, wantLines)
	}

	if _, err := d.Format(FormatOptions{
		BodyFont:    "Calibri",
		BodySizePt:  12,
		LineSpacing: 1.5,
		Align:       "justify",
		MarginsMM:   []float64{20, 20, 20, 20},
		Normalize:   true,
	}); err != nil {
		t.Errorf("Format failed on a document with a multi-line, <w:br/>-bearing code block: %v", err)
	}
}

// docx_read must still describe the block sensibly for a model that has to
// edit it afterwards: paraTextWithBreaks-based rendering (read.go,
// unmodified by this task) means the whole block surfaces as ONE "[para N]"
// citation with internal "\n" line breaks, and -- with Runs requested --
// the underlying per-line Run structure (one Run per source line, since
// each is its own <w:t>) stays fully recoverable even though it is no
// longer one Run per paragraph.
func TestRead_CodeBlockIsOneParaWithLineBreaksAndRecoverableRuns(t *testing.T) {
	d, _, _ := writeAndReopen(t, "before\n\n```\nline one\nline two\nline three\n```\n\nafter\n")

	rr, err := d.Read(ReadOptions{Runs: true})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(rr.Markdown, "line one\nline two\nline three") {
		t.Errorf("Markdown does not contain the code block's lines joined by \\n:\n%s", rr.Markdown)
	}
	if n := strings.Count(rr.Markdown, "line one"); n != 1 {
		t.Errorf("code block content appears %d times in Markdown, want 1 (one paragraph, not three)", n)
	}

	var codePV *ParaView
	for i := range rr.Paras {
		if rr.Paras[i].Style == StyleSourceCode {
			codePV = &rr.Paras[i]
			break
		}
	}
	if codePV == nil {
		t.Fatalf("no SourceCode ParaView found among %d paras", len(rr.Paras))
	}
	var runTexts []string
	for _, r := range codePV.Runs {
		runTexts = append(runTexts, r.Text)
	}
	if want := []string{"line one", "line two", "line three"}; !reflect.DeepEqual(runTexts, want) {
		t.Errorf("code ParaView.Runs texts = %q, want %q (per-line structure must survive even inside one paragraph)", runTexts, want)
	}
}
