package docx

import (
	"regexp"
	"strings"
	"testing"
)

// This file covers the GenOffice/Google-Docs-compatibility task: certain
// visual properties that used to live ONLY in a named style are now ALSO
// written directly onto the affected paragraph/run/cell, because GenOffice
// does not resolve a paragraph style's <w:pBdr>, <w:shd>, or <w:rFonts> at
// all, and neither Google Docs nor GenOffice applies a table style's
// <w:tblStylePr> conditional formatting. Both copies come from the exact
// same source in styles.go (codeBorderXML, codeShadingXML,
// codeRunFontsXML, tableHeaderShadingXML) so they cannot hand-drift apart.
//
// The tests below extract both copies independently from a real
// WriteDocx+reopen round trip and compare them as strings, rather than
// trusting that calling the same Go function twice proves anything about
// the bytes actually written -- a test that built its "expected" value by
// calling the very same helper the production code calls would not catch
// one of the two call sites drifting onto a hand-typed literal instead.

// A fenced code block's border and shading must be byte-identical between
// the SourceCode style (styles.xml) and the direct copy on each code
// paragraph (document.xml).
func TestWrite_InlineCodeVisualsMatchStyle(t *testing.T) {
	md := "```\ncode\n```\n"
	d, _, _ := writeAndReopen(t, md)

	styles, _ := d.Part("word/styles.xml")
	sc := styleBlock(t, styles, StyleSourceCode)
	pBdrRE := regexp.MustCompile(`<w:pBdr>.*?</w:pBdr>`)
	shdRE := regexp.MustCompile(`<w:shd[^/]*/>`)
	styleBorder := pBdrRE.FindString(sc)
	styleShading := shdRE.FindString(sc)
	if styleBorder == "" || styleShading == "" {
		t.Fatalf("SourceCode style = %s, missing pBdr or shd; test would be vacuous", sc)
	}

	doc, _ := d.Part(DocumentPart)
	s := string(doc)
	paraRE := regexp.MustCompile(`<w:pPr><w:pStyle w:val="SourceCode"/>.*?</w:pPr>`)
	codePPr := paraRE.FindString(s)
	if codePPr == "" {
		t.Fatalf("no SourceCode paragraph pPr found in document.xml: %s", s)
	}
	inlineBorder := pBdrRE.FindString(codePPr)
	inlineShading := shdRE.FindString(codePPr)
	if inlineBorder == "" || inlineShading == "" {
		t.Fatalf("code paragraph's pPr = %s, missing inline pBdr or shd", codePPr)
	}

	if inlineBorder != styleBorder {
		t.Errorf("inline pBdr = %s, style's pBdr = %s -- the two copies have drifted apart", inlineBorder, styleBorder)
	}
	if inlineShading != styleShading {
		t.Errorf("inline shd = %s, style's shd = %s -- the two copies have drifted apart", inlineShading, styleShading)
	}
}

// A table header row's shading must be byte-identical between TableGrid's
// <w:tblStylePr w:type="firstRow"> (styles.xml) and the direct copy on each
// header cell's own <w:tcPr> (document.xml).
func TestWrite_InlineHeaderShadingMatchesStyle(t *testing.T) {
	md := "| a | b |\n|---|---|\n| 1 | 2 |\n"
	d, _, _ := writeAndReopen(t, md)

	styles, _ := d.Part("word/styles.xml")
	tg := styleBlock(t, styles, StyleTableGrid)
	shdRE := regexp.MustCompile(`<w:shd[^/]*/>`)
	styleShading := shdRE.FindString(tg)
	if styleShading == "" {
		t.Fatalf("TableGrid = %s, missing shd; test would be vacuous", tg)
	}

	doc, _ := d.Part(DocumentPart)
	s := string(doc)
	rowRE := regexp.MustCompile(`<w:tr>.*?</w:tr>`)
	rows := rowRE.FindAllString(s, -1)
	if len(rows) == 0 {
		t.Fatal("no <w:tr> found; test would be vacuous")
	}
	header := rows[0]
	inlineShading := shdRE.FindString(header)
	if inlineShading == "" {
		t.Fatalf("header row = %s, missing inline shd", header)
	}
	if inlineShading != styleShading {
		t.Errorf("inline header shd = %s, style's shd = %s -- the two copies have drifted apart", inlineShading, styleShading)
	}
}

// A custom code font must reach the SourceCode style's <w:rPr> AND the
// direct <w:rFonts> copy on an ordinary code-block-line run, and the two
// must match -- not just the pre-existing hyperlink+code edge case
// (TestWrite_CustomCodeFontReachesCodeFontXMLFallback), which does not
// exercise a plain code line at all.
func TestWrite_InlineCodeFontMatchesStyleWithCustomFont(t *testing.T) {
	p := t.TempDir() + "/out.docx"
	_, err := WriteDocx(p, WriteOptions{
		Markdown:         "```\ncode\n```\n",
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

	styles, _ := d.Part("word/styles.xml")
	sc := styleBlock(t, styles, StyleSourceCode)
	styleFonts := rFontsRE.FindString(sc)
	if styleFonts == "" {
		t.Fatalf("SourceCode = %s, missing rFonts; test would be vacuous", sc)
	}

	doc, _ := d.Part(DocumentPart)
	s := string(doc)
	if !strings.Contains(s, `<w:pStyle w:val="SourceCode"/>`) {
		t.Fatal("no SourceCode paragraph found in document.xml")
	}
	runRE := regexp.MustCompile(`<w:r>.*?</w:r>`)
	run := runRE.FindString(s)
	if run == "" {
		t.Fatal("no <w:r> found in document.xml; test would be vacuous")
	}
	inlineFonts := rFontsRE.FindString(run)
	if inlineFonts == "" {
		t.Fatalf("code run = %s, missing inline rFonts", run)
	}
	if inlineFonts != styleFonts {
		t.Errorf("inline run rFonts = %s, style's rFonts = %s -- the two copies have drifted apart", inlineFonts, styleFonts)
	}
	if !strings.Contains(inlineFonts, `w:ascii="Cascadia Code"`) || !strings.Contains(inlineFonts, `w:eastAsia="NSimSun"`) {
		t.Errorf("inline rFonts = %s, want the custom code font", inlineFonts)
	}
}
