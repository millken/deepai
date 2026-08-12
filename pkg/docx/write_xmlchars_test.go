package docx

import (
	"strings"
	"testing"
)

// Task 2 (P0): docx_write must never hand XML 1.0 illegal characters
// (control codes, etc.) to <w:t> content -- doing so today produces a
// .docx that WriteDocx reports as written successfully but that this
// package's own OpenDocument (and Word) refuses to parse. See
// write-review.md finding C2 and .superpowers/sdd/task-2-brief.md.
//
// These tests cover the four example inputs the brief calls out
// (\x1b, \x0c pasted raw, and &#1;/&#x0B; as numeric character
// references that decode to the same illegal codepoints), and the four
// text paths the brief requires the filter reach: ordinary body text,
// headings, code blocks, table cells, and hyperlink text.

// A raw control byte (e.g. ESC, \x1b -- exactly what a pasted ANSI
// terminal transcript carries) pasted into an ordinary paragraph must not
// corrupt the document: WriteDocx must still report success, the file
// must still be openable, the illegal character must not appear in the
// visible text, and Notes must declare that something was stripped.
func TestWrite_IllegalControlCharIsStrippedFromBodyText(t *testing.T) {
	d, res, _ := writeAndReopen(t, "line one \x1bescaped\n")

	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	if strings.ContainsRune(text.String(), '\x1b') {
		t.Errorf("visible text still contains the illegal control character: %q", text.String())
	}

	joined := strings.Join(res.Notes, " | ")
	if !strings.Contains(joined, "stripped 1 invalid XML character") {
		t.Errorf("Notes = %v, want a note declaring 1 stripped invalid XML character", res.Notes)
	}
}

// Legal whitespace -- a literal tab in the middle of a line -- must
// survive untouched, and input with no illegal character at all must
// produce no stripped-character note.
func TestWrite_LegalTabSurvivesAndProducesNoNote(t *testing.T) {
	d, res, _ := writeAndReopen(t, "before\tafter\n")

	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	if !strings.Contains(text.String(), "before\tafter") {
		t.Errorf("visible text = %q, want the tab preserved", text.String())
	}

	for _, n := range res.Notes {
		if strings.Contains(n, "invalid XML character") {
			t.Errorf("Notes = %v, want no stripped-character note for fully legal input", res.Notes)
		}
	}
}

// A numeric character reference that decodes to an illegal codepoint
// (&#1; is SOH, &#x0B; is vertical tab -- neither is legal XML 1.0
// content) must not be decoded into the raw illegal byte: the decode site
// (decodeHTMLEntities) must itself refuse, and escapeXMLText must not see
// (and therefore never write) the illegal character either way.
func TestWrite_IllegalNumericEntityDoesNotProduceRawIllegalByte(t *testing.T) {
	d, res, _ := writeAndReopen(t, "before &#1; and &#x0B; after\n")

	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	if strings.ContainsRune(text.String(), '\x01') || strings.ContainsRune(text.String(), '\x0B') {
		t.Errorf("visible text contains a raw illegal codepoint decoded from a numeric entity: %q", text.String())
	}

	joined := strings.Join(res.Notes, " | ")
	if !strings.Contains(joined, "stripped 2 invalid XML character") {
		t.Errorf("Notes = %v, want a note declaring 2 stripped invalid XML characters (one per illegal numeric entity)", res.Notes)
	}
}

// The filter must be reached from every path that lands text in <w:t>:
// a heading, a fenced code block, a table cell, and hyperlink display
// text. One illegal character is planted in each; the resulting document
// must still open, and Notes must report all four.
func TestWrite_IllegalXMLCharsStrippedAcrossHeadingCodeTableAndLinkText(t *testing.T) {
	md := "# Heading\x1bTitle\n\n" +
		"```\n" +
		"code\x0cline\n" +
		"```\n\n" +
		"| a | b |\n|---|---|\n| cell\x1bvalue | y |\n\n" +
		"[link\x1btext](https://example.com/x)\n"

	d, res, _ := writeAndReopen(t, md)

	var all strings.Builder
	for _, p := range d.Paras() {
		for _, r := range p.Runs {
			all.WriteString(r.Text)
		}
	}
	full := all.String()
	for _, bad := range []rune{'\x1b', '\x0c'} {
		if strings.ContainsRune(full, bad) {
			t.Errorf("document text still contains illegal character %U: %q", bad, full)
		}
	}

	joined := strings.Join(res.Notes, " | ")
	if !strings.Contains(joined, "stripped 4 invalid XML character") {
		t.Errorf("Notes = %v, want a note declaring 4 stripped invalid XML characters (heading+code+table+link)", res.Notes)
	}
}

// WriteOptions.Title lands in docProps/core.xml's <dc:title>, a path that
// never goes through renderRun (parseMarkdown only ever copies Title into
// a body block when the markdown does NOT already open with its own H1 --
// see markdownStartsWithH1). An illegal character in Title must still be
// replaced (never corrupt docProps/core.xml) AND still be counted into the
// same "stripped N invalid XML character(s)" note renderRun's own findings
// use -- a caller must never see "Notes: []" on a document that silently
// sanitized its own declared title.
func TestWrite_IllegalCharInTitleIsCountedInNotes(t *testing.T) {
	opts := WriteOptions{
		Title:    "Bad\x1bTitle",
		Markdown: "# Real Heading\n\nbody\n",
	}
	d, res, _ := writeAndReopen2(t, opts)

	coreXML, ok := d.Part(docPropsCorePart)
	if !ok {
		t.Fatalf("no %s part; hasTitle path did not run", docPropsCorePart)
	}
	if strings.ContainsRune(string(coreXML), '\x1b') {
		t.Errorf("docProps/core.xml still contains the illegal control character: %s", coreXML)
	}

	joined := strings.Join(res.Notes, " | ")
	if !strings.Contains(joined, "stripped 1 invalid XML character") {
		t.Errorf("Notes = %v, want a note declaring 1 stripped invalid XML character for the illegal Title", res.Notes)
	}
}

// A hyperlink's URL lands in word/_rels/document.xml.rels's Target
// attribute (buildDocRelsXML), a second path that never goes through
// renderRun (renderRun only ever sees the link's DISPLAY text). An illegal
// character in the URL itself must likewise be replaced and counted into
// the same total.
func TestWrite_IllegalCharInLinkURLIsCountedInNotes(t *testing.T) {
	d, res, _ := writeAndReopen(t, "[text](https://example.com/\x1bpath)\n")

	relsXML, ok := d.Part("word/_rels/document.xml.rels")
	if !ok {
		t.Fatalf("no word/_rels/document.xml.rels part")
	}
	if strings.ContainsRune(string(relsXML), '\x1b') {
		t.Errorf("word/_rels/document.xml.rels still contains the illegal control character: %s", relsXML)
	}

	joined := strings.Join(res.Notes, " | ")
	if !strings.Contains(joined, "stripped 1 invalid XML character") {
		t.Errorf("Notes = %v, want a note declaring 1 stripped invalid XML character for the illegal link URL", res.Notes)
	}
}
