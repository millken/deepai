package docx

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// This file covers Task 5 ("write 块级修复") of
// .superpowers/sdd/task-5-brief.md: five independent block-level defects in
// docx_write, reported in .superpowers/sdd/docx-capability-review-write.md
// as I1 (code-block merge regression), I10 (BOM), M1 (Setext headings
// printed literally), I2 (unescaped table pipe), and I9 (Quote's left
// border missing an inline GenOffice-compatibility copy).

// ---------------------------------------------------------------------------
// I1: mergeCodeBlocks must not merge two code blocks that a blank line
// genuinely separates in the source. Only a truly separator-free run of
// fenced/indented lines -- including two fenced blocks with NOTHING at all
// between them -- may still collapse into one paragraph (that narrower case
// is pinned separately below as a non-regression).
// ---------------------------------------------------------------------------

// Two fenced code blocks (different info strings/languages) with a blank
// line between them must render as two independent SourceCode paragraphs,
// not one merged box.
func TestWrite_TwoFencedBlocksSeparatedByBlankLineStayIndependent(t *testing.T) {
	md := "```go\ncmd\n```\n\n```text\noutput\n```\n"
	d, _, _ := writeAndReopen(t, md)
	paras := sourceCodeParas(d)
	if len(paras) != 2 {
		t.Fatalf("got %d SourceCode paragraphs, want 2 (a blank line separates two distinct code blocks): %+v", len(paras), paras)
	}
	if got := paraTextWithBreaks(paras[0]); got != "cmd" {
		t.Errorf("first code block text = %q, want %q", got, "cmd")
	}
	if got := paraTextWithBreaks(paras[1]); got != "output" {
		t.Errorf("second code block text = %q, want %q", got, "output")
	}
}

// An indented code block followed by a blank line and then a fenced code
// block must also stay independent -- the blank-line separator matters
// regardless of which two code-block FORMS sit on either side of it.
func TestWrite_IndentedThenFencedBlockSeparatedByBlankLineStayIndependent(t *testing.T) {
	md := "    indented\n\n```\nfenced\n```\n"
	d, _, _ := writeAndReopen(t, md)
	paras := sourceCodeParas(d)
	if len(paras) != 2 {
		t.Fatalf("got %d SourceCode paragraphs, want 2: %+v", len(paras), paras)
	}
	if got := paraTextWithBreaks(paras[0]); got != "indented" {
		t.Errorf("first code block text = %q, want %q", got, "indented")
	}
	if got := paraTextWithBreaks(paras[1]); got != "fenced" {
		t.Errorf("second code block text = %q, want %q", got, "fenced")
	}
}

// Non-regression: two fenced blocks with NOTHING at all between them (no
// blank line) still merge into one paragraph/one box -- this is the
// original, narrower range the 6822a00 comment describes, not something
// this task's fix should remove.
func TestWrite_TwoFencedBlocksWithNoSeparatorStillMerge(t *testing.T) {
	md := "```\na\n```\n```\nb\n```\n"
	d, _, _ := writeAndReopen(t, md)
	paras := sourceCodeParas(d)
	if len(paras) != 1 {
		t.Fatalf("got %d SourceCode paragraphs, want 1 (no blank line separates these two fences, so they still merge): %+v", len(paras), paras)
	}
	want := []string{"a", "b"}
	if got := strings.Split(paraTextWithBreaks(paras[0]), "\n"); !reflect.DeepEqual(got, want) {
		t.Errorf("merged code lines = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// I10: a UTF-8 BOM at the very start of the input must not defeat
// heading/other line-based detection.
// ---------------------------------------------------------------------------

func TestWrite_LeadingBOMDoesNotBreakHeadingDetection(t *testing.T) {
	md := "\ufeff# Title\n\nBody.\n"
	d, _, _ := writeAndReopen(t, md)
	paras := d.Paras()
	if len(paras) < 1 {
		t.Fatal("no paragraphs written")
	}
	if paras[0].Style != "Heading1" {
		t.Errorf("paras[0].Style = %q, want Heading1 (a leading BOM must not stop the first line from being recognized as an ATX heading)", paras[0].Style)
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	if strings.Contains(text.String(), "\ufeff") {
		t.Errorf("heading text = %q, still contains the BOM", text.String())
	}
	if text.String() != "Title" {
		t.Errorf("heading text = %q, want %q", text.String(), "Title")
	}
}

// ---------------------------------------------------------------------------
// M1: a Setext heading (text line immediately followed by a full "="/"-"
// underline) must become a real Heading1/Heading2, not print the literal
// underline as body text.
// ---------------------------------------------------------------------------

func TestWrite_SetextH1BecomesHeading1(t *testing.T) {
	md := "Title\n=====\n"
	d, _, _ := writeAndReopen(t, md)
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1: %+v", len(paras), paras)
	}
	if paras[0].Style != "Heading1" {
		t.Errorf("paras[0].Style = %q, want Heading1", paras[0].Style)
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	if text.String() != "Title" {
		t.Errorf("heading text = %q, want %q (must not contain the literal '=' underline)", text.String(), "Title")
	}
}

func TestWrite_SetextH2BecomesHeading2(t *testing.T) {
	md := "Subtitle\n---\n"
	d, _, _ := writeAndReopen(t, md)
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1: %+v", len(paras), paras)
	}
	if paras[0].Style != "Heading2" {
		t.Errorf("paras[0].Style = %q, want Heading2", paras[0].Style)
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	if text.String() != "Subtitle" {
		t.Errorf("heading text = %q, want %q (must not contain the literal '---' underline)", text.String(), "Subtitle")
	}
	doc, _ := d.Part(DocumentPart)
	if strings.Contains(string(doc), "Subtitle ---") {
		t.Error("document.xml still contains the literal 'Subtitle ---' text")
	}
}

func TestWrite_SetextH2RequiresAtLeastTwoDashes(t *testing.T) {
	// A single dash is neither a valid hr nor a valid setext underline --
	// existing behavior (merged as literal paragraph text) must be
	// unaffected.
	md := "Title\n-\n"
	d, _, _ := writeAndReopen(t, md)
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1: %+v", len(paras), paras)
	}
	if paras[0].Style == "Heading2" {
		t.Error("a single dash must not be treated as a setext H2 underline")
	}
}

// Self-review: a list item's own indented continuation line accumulates in
// the same accLines buffer an ordinary paragraph would, but a setext
// underline right after it must not retroactively turn that continuation
// text into a heading -- the list stays a list.
func TestWrite_ListContinuationFollowedByDashesIsNotSetext(t *testing.T) {
	md := "- item\n  more\n---\n"
	d, _, _ := writeAndReopen(t, md)
	for _, p := range d.Paras() {
		if p.Style == "Heading2" {
			t.Errorf("a list item's continuation text became a Heading2: %+v", d.Paras())
		}
	}
}

// Self-review: table separator rows, setext underlines, and horizontal
// rules must not misfire on each other -- table gets first refusal, then
// setext, then hr, per the task brief's required precedence.
func TestWrite_TableSetextAndHRDoNotMisfireOnEachOther(t *testing.T) {
	md := "| a | b |\n|---|---|\n| 1 | 2 |\n\nTitle\n---\n\nParagraph before rule.\n\n---\n\nAfter rule.\n"
	d, _, _ := writeAndReopen(t, md)

	// The table must still be a real table, not consumed as a setext
	// underline for the header row: exactly one distinct table index, with
	// two rows of cells, among the document's paragraphs.
	tableIndexes := map[int]bool{}
	for _, p := range d.Paras() {
		if p.Cell != nil {
			tableIndexes[p.Cell.Table] = true
		}
	}
	if len(tableIndexes) != 1 {
		t.Fatalf("got %d distinct tables, want 1: table must not be swallowed by setext/hr detection", len(tableIndexes))
	}

	var styles []string
	var texts []string
	for _, p := range d.Paras() {
		if p.Cell != nil {
			continue // table-cell paragraphs -- not part of this assertion
		}
		styles = append(styles, p.Style)
		var b strings.Builder
		for _, r := range p.Runs {
			b.WriteString(r.Text)
		}
		texts = append(texts, b.String())
	}

	foundHeading2 := false
	foundHR := false
	for i, s := range styles {
		if s == "Heading2" && texts[i] == "Title" {
			foundHeading2 = true
		}
	}
	doc, _ := d.Part(DocumentPart)
	if strings.Contains(string(doc), hrBorderXML) {
		foundHR = true
	}
	if !foundHeading2 {
		t.Errorf("Setext H2 'Title' not found among top-level paragraphs: styles=%v texts=%v", styles, texts)
	}
	if !foundHR {
		t.Error("the standalone '---' horizontal rule was not rendered (its <w:pBdr> is missing)")
	}
	for _, tx := range texts {
		if strings.Contains(tx, "---") {
			t.Errorf("a paragraph still contains the literal '---': %q", tx)
		}
	}
}

// ---------------------------------------------------------------------------
// I2: a backslash-escaped pipe inside a table cell must not be treated as a
// cell delimiter.
// ---------------------------------------------------------------------------

func TestWrite_EscapedPipeInTableCellIsLiteral(t *testing.T) {
	md := "| a\\|b | c |\n|---|---|\n| 1 | 2 |\n"
	d, res, _ := writeAndReopen(t, md)

	// Header row (row 1) of table 1 must have exactly 2 cells: the escaped
	// pipe must not split the first cell in two.
	var headerCols []int
	cellText := map[int]string{}
	for _, p := range d.Paras() {
		if p.Cell == nil || p.Cell.Table != 1 || p.Cell.Row != 1 {
			continue
		}
		headerCols = append(headerCols, p.Cell.Col)
		var b strings.Builder
		for _, r := range p.Runs {
			b.WriteString(r.Text)
		}
		cellText[p.Cell.Col] = b.String()
	}
	if len(headerCols) != 2 {
		t.Fatalf("header row has %d cell paragraphs, want exactly 2 (the escaped pipe must not split the first cell in two): %v", len(headerCols), cellText)
	}
	if got := cellText[1]; got != "a|b" {
		t.Errorf("first header cell text = %q, want %q", got, "a|b")
	}
	if len(res.Notes) != 0 {
		t.Errorf("Notes = %v, want empty -- an escaped pipe is fully supported, not a ragged-row compromise", res.Notes)
	}
}

// ---------------------------------------------------------------------------
// I9: a block-quote paragraph's left border must be written directly onto
// the paragraph, byte-identical to the Quote style's own copy, the same
// GenOffice-compatibility mechanism already applied to code blocks and
// table headers.
// ---------------------------------------------------------------------------

func TestWrite_QuoteLeftBorderLivesInStyleAndInline(t *testing.T) {
	md := "> quoted text\n"
	d, _, _ := writeAndReopen(t, md)
	doc, _ := d.Part(DocumentPart)
	s := string(doc)

	paraRE := regexp.MustCompile(`<w:p>.*?</w:p>`)
	var quoteP string
	for _, p := range paraRE.FindAllString(s, -1) {
		if strings.Contains(p, `<w:pStyle w:val="Quote"/>`) {
			quoteP = p
			break
		}
	}
	if quoteP == "" {
		t.Fatalf("no Quote paragraph found in document.xml: %s", s)
	}
	if !strings.Contains(quoteP, "<w:pBdr") {
		t.Errorf("Quote paragraph has no inline <w:pBdr>; GenOffice does not resolve the Quote style's border, so a quote would show no left border in it: %s", quoteP)
	}

	styles, _ := d.Part("word/styles.xml")
	q := styleBlock(t, styles, StyleQuote)
	if !strings.Contains(q, "<w:pBdr") {
		t.Fatalf("Quote style has no <w:pBdr>: %s", q)
	}
}
