package docx

import (
	"reflect"
	"strings"
	"testing"
)

// This file covers two defects in how docx_write renders code blocks,
// reported against real generated output (see
// .superpowers/sdd/code-block-report.md for the fuller writeup):
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

// ---------------------------------------------------------------------------
// Defect 1: every code-block form must produce SourceCode-styled paragraphs.
// Table-driven so a future form (e.g. another fence variant) cannot be added
// without a case here.
// ---------------------------------------------------------------------------

func TestWrite_CodeBlockFormsAllProduceSourceCodeStyle(t *testing.T) {
	cases := []struct {
		name string
		md   string
		want []string
	}{
		{"bare fence", "```\ncode line\n```\n", []string{"code line"}},
		{"fence with info string", "```go\ncode line\n```\n", []string{"code line"}},
		{"tilde fence", "~~~\ncode line\n~~~\n", []string{"code line"}},
		{"four-backtick fence", "````\ncode line\n````\n", []string{"code line"}},
		{"indented (four spaces, no fence)", "    code line\n", []string{"code line"}},
		{"indented (one tab, no fence)", "\tcode line\n", []string{"code line"}},
		{"indented, multiple lines", "    line one\n    line two\n", []string{"line one", "line two"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _ := writeAndReopen(t, tc.md)
			var got []string
			for _, p := range d.Paras() {
				if p.Style != StyleSourceCode {
					continue
				}
				var b strings.Builder
				for _, r := range p.Runs {
					b.WriteString(r.Text)
				}
				got = append(got, b.String())
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("SourceCode paragraph texts = %q, want %q", got, tc.want)
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

	var codeTexts []string
	for _, p := range d.Paras() {
		if p.Style != StyleSourceCode {
			continue
		}
		var b strings.Builder
		for _, r := range p.Runs {
			b.WriteString(r.Text)
		}
		codeTexts = append(codeTexts, b.String())
	}
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

	var got []string
	for _, p := range d.Paras() {
		if p.Style != StyleSourceCode {
			t.Errorf("non-code paragraph found: style=%q", p.Style)
			continue
		}
		var b strings.Builder
		for _, r := range p.Runs {
			b.WriteString(r.Text)
		}
		got = append(got, b.String())
	}
	want := []string{"line one", "", "line two"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("code lines = %q, want %q (blank line must survive as an empty code line)", got, want)
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
// padding, referenced from a style, never inline (the project's core
// invariant).
// ---------------------------------------------------------------------------

func TestWrite_CodeBlockContainerBorderLivesInStyleNotInline(t *testing.T) {
	md := "```\nline one\nline two\n```\n"
	d, _, _ := writeAndReopen(t, md)
	doc, _ := d.Part(DocumentPart)
	if strings.Contains(string(doc), "<w:pBdr") {
		t.Errorf("document.xml carries an inline <w:pBdr> for a code block; it must live in the SourceCode style instead:\n%s", string(doc))
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

// A code block must survive a page boundary sensibly: keepNext chains
// consecutive SourceCode paragraphs together (the same mechanism headings
// already use for the identical reason), and keepLines keeps each line's
// own content from splitting mid-line.
func TestWrite_CodeBlockKeepsTogetherAcrossPageBreaks(t *testing.T) {
	styles := buildStylesXML()
	sc := styleBlock(t, styles, StyleSourceCode)
	if !strings.Contains(sc, "<w:keepNext/>") {
		t.Errorf("SourceCode has no <w:keepNext/>; a multi-line code block can split awkwardly across a page: %s", sc)
	}
	if !strings.Contains(sc, "<w:keepLines/>") {
		t.Errorf("SourceCode has no <w:keepLines/>: %s", sc)
	}
}
