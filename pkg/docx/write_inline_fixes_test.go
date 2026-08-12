package docx

import (
	"strings"
	"testing"
)

// --- Task 4 (P1): ***bold italic***, backslash escapes, link title/parens ---

// TestWrite_TripleAsteriskIsBoldAndItalic is C1's core red test: "***x***"
// must resolve to a single run carrying BOTH bold and italic, not a bold
// run reading "*x" plus a stray leftover "*" (the pre-fix behavior, which
// came from the "**" branch searching for its close two characters too
// early and finding the THIRD "*" of the opening run instead of the real
// closing "***").
func TestWrite_TripleAsteriskIsBoldAndItalic(t *testing.T) {
	d, _, _ := writeAndReopen(t, "***粗斜***\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	if got, want := paraVisibleText(paras[0]), "粗斜"; got != want {
		t.Errorf("visible text = %q, want %q (no stray '*' characters)", got, want)
	}
	doc, _ := d.Part(DocumentPart)
	docStr := string(doc)
	if !strings.Contains(docStr, "<w:b/><w:i/>") && !strings.Contains(docStr, "<w:i/><w:b/>") {
		t.Errorf("expected one run with both bold and italic properties: %s", docStr)
	}
}

// TestWrite_TripleAsteriskDoesNotPolluteSurroundingText is C1's "pollution"
// symptom: the leftover "*" that indexClosingMarker's too-early match left
// behind used to go on and mismatch with unrelated "*" characters later in
// the same paragraph (or, with only one paragraph here, at least surface as
// a stray asterisk in the visible text around the emphasis span).
func TestWrite_TripleAsteriskDoesNotPolluteSurroundingText(t *testing.T) {
	d, _, _ := writeAndReopen(t, "A ***bold italic*** B\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	if got, want := paraVisibleText(paras[0]), "A bold italic B"; got != want {
		t.Errorf("visible text = %q, want %q (surrounding text must not be mangled)", got, want)
	}
	doc, _ := d.Part(DocumentPart)
	docStr := string(doc)
	if !strings.Contains(docStr, "<w:b/><w:i/>") && !strings.Contains(docStr, "<w:i/><w:b/>") {
		t.Errorf("expected one run with both bold and italic properties: %s", docStr)
	}
}

// TestWrite_DoubleAsteriskWithNestedItalicIsUnchanged pins the brief's own
// regression case: "**bold *italic* bold**" (here in Chinese, matching the
// brief's exact example) must keep resolving as bold-with-a-nested-italic-
// span, not be swept up by the new triple-marker handling merely because a
// "*" happens to sit right after the opening "**" run at some OTHER offset.
func TestWrite_DoubleAsteriskWithNestedItalicIsUnchanged(t *testing.T) {
	d, _, _ := writeAndReopen(t, "**粗 *斜* 粗**\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	if got, want := paraVisibleText(paras[0]), "粗 斜 粗"; got != want {
		t.Errorf("visible text = %q, want %q", got, want)
	}
	doc, _ := d.Part(DocumentPart)
	docStr := string(doc)
	if !strings.Contains(docStr, "<w:b/><w:i/>") && !strings.Contains(docStr, "<w:i/><w:b/>") {
		t.Errorf("expected an inner run with both bold and italic properties: %s", docStr)
	}
	if !strings.Contains(docStr, "<w:b/>") {
		t.Error("expected a plain bold run for the outer text")
	}
}

// TestWrite_BackslashEscapedAsteriskStaysLiteral is C4's core red test:
// parseInlineCtx has no '\' branch at all before this fix, so "\*not em\*"
// keeps the backslashes AND still triggers italic -- CommonMark's rule is
// the opposite: a backslash before ASCII punctuation is consumed and the
// punctuation becomes literal text, never markup.
func TestWrite_BackslashEscapedAsteriskStaysLiteral(t *testing.T) {
	d, _, _ := writeAndReopen(t, `\*not em\*`+"\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	if got, want := paraVisibleText(paras[0]), "*not em*"; got != want {
		t.Errorf("visible text = %q, want %q (backslashes consumed, asterisks literal)", got, want)
	}
	doc, _ := d.Part(DocumentPart)
	if strings.Contains(string(doc), "<w:i/>") {
		t.Errorf("escaped asterisks must not trigger italic; got:\n%s", doc)
	}
}

// TestWrite_BackslashEscapedPercentStaysLiteral covers a backslash before a
// punctuation character with no markup meaning of its own -- the backslash
// must still be consumed per CommonMark, not just markup-suppressing
// punctuation.
func TestWrite_BackslashEscapedPercentStaysLiteral(t *testing.T) {
	d, _, _ := writeAndReopen(t, `100\%`+"\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	if got, want := paraVisibleText(paras[0]), "100%"; got != want {
		t.Errorf("visible text = %q, want %q", got, want)
	}
}

// TestWrite_BackslashBeforeNonPunctuationStaysLiteral covers the flip side
// of CommonMark's escape rule: a backslash followed by a NON-punctuation
// character (here, the letter 'p') is not an escape at all -- the backslash
// itself must survive literally, e.g. a Windows path like "C:\path".
func TestWrite_BackslashBeforeNonPunctuationStaysLiteral(t *testing.T) {
	d, _, _ := writeAndReopen(t, `C:\path`+"\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	if got, want := paraVisibleText(paras[0]), `C:\path`; got != want {
		t.Errorf("visible text = %q, want %q (backslash before a non-punctuation character survives)", got, want)
	}
}

// TestWrite_LinkTitleDoesNotPolluteURL is I3's title red test: matchLinkAt
// used to treat everything between the first "(" and the first ")" as the
// URL, so a titled link's URL relationship ended up as
// `https://example.com/page "Example Title"` -- a target no browser or
// Word can follow.
func TestWrite_LinkTitleDoesNotPolluteURL(t *testing.T) {
	d, _, _ := writeAndReopen(t, `[t](https://example.com/page "Example Title")`+"\n")
	doc, _ := d.Part(DocumentPart)
	rels, ok := d.Part("word/_rels/document.xml.rels")
	if !ok {
		t.Fatal("word/_rels/document.xml.rels missing")
	}
	relsStr := string(rels)
	if !strings.Contains(relsStr, `Target="https://example.com/page"`) {
		t.Errorf("relationship target must be exactly the URL, no title text: %s", relsStr)
	}
	if strings.Contains(relsStr, "Example Title") {
		t.Errorf("link title leaked into the relationship target: %s", relsStr)
	}
	if got, want := paraVisibleText(d.Paras()[0]), "t"; got != want {
		t.Errorf("visible text = %q, want %q; doc: %s", got, want, doc)
	}
}

// TestWrite_LinkURLWithBalancedParensIsPreserved is I3's other red test:
// matchLinkAt used to stop at the FIRST ")", truncating any URL that itself
// contains a parenthesised segment -- a common shape for wiki article URLs.
func TestWrite_LinkURLWithBalancedParensIsPreserved(t *testing.T) {
	d, _, _ := writeAndReopen(t, "[t](https://example.com/wiki/Foo_(bar))\n")
	rels, ok := d.Part("word/_rels/document.xml.rels")
	if !ok {
		t.Fatal("word/_rels/document.xml.rels missing")
	}
	relsStr := string(rels)
	if !strings.Contains(relsStr, `Target="https://example.com/wiki/Foo_(bar)"`) {
		t.Errorf("relationship target must keep the balanced parentheses intact: %s", relsStr)
	}
}

// TestWrite_ReferenceLinkDefinitionLineIsNotPrintedAsBodyText is I3's third
// red test, for the minimal reference-style-link requirement the brief
// sets: a definition line like "[1]: https://example.com/docs" must not
// leak into the document as an ordinary paragraph of visible body text --
// at minimum it must be recognized and dropped, with its absence declared
// through Notes (this package's existing buildNotes contract), even though
// resolving [text][1] into an actual hyperlink is not required.
func TestWrite_ReferenceLinkDefinitionLineIsNotPrintedAsBodyText(t *testing.T) {
	md := "See [docs][1] for more.\n\n[1]: https://example.com/docs\n"
	d, res, _ := writeAndReopen(t, md)
	for _, p := range d.Paras() {
		text := paraVisibleText(p)
		if strings.Contains(text, "https://example.com/docs") {
			t.Errorf("reference-link definition line leaked into body text: %q", text)
		}
	}
	joined := strings.Join(res.Notes, " | ")
	if !strings.Contains(joined, "reference-style") {
		t.Errorf("Notes = %v, want a declaration about reference-style link definitions", res.Notes)
	}
}
