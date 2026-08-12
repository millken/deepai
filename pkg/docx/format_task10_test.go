package docx

// Task 10 (P2): docx_format's index-invalidation signal, honest
// descriptions/notes, byte-level idempotency on the paragraph-range path,
// and an atomic (all-or-nothing) namespace precheck. See
// .superpowers/sdd/task-10-brief.md.

import (
	"bytes"
	"strings"
	"testing"
)

// TestFormat_MixedNamespacePrefixesFailsAtomicallyBeforeAnyPartIsRewritten
// covers task 10 brief item 5 / task 9 review follow-up: styles.xml and
// document.xml can independently use the standard "w:" prefix or not.
// Before this fix, Format's three requireWordNamespacePrefix checks were
// scattered across each block in mutation order, so a call whose rule set
// touches BOTH parts (body_font: styles.xml's docDefaults AND, via the
// direct-formatting masking scan, document.xml) would already have
// rewritten styles.xml in memory (via d.SetPart) by the time document.xml's
// own prefix was checked and found invalid -- an error return with a
// half-applied Document, violating the all-or-nothing promise even though
// nothing ever reaches disk. Both parts must now be validated BEFORE either
// is touched.
func TestFormat_MixedNamespacePrefixesFailsAtomicallyBeforeAnyPartIsRewritten(t *testing.T) {
	stylesXML := `<?xml version="1.0"?><w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:docDefaults><w:rPrDefault><w:rPr><w:rFonts w:ascii="Existing" w:hAnsi="Existing"/></w:rPr></w:rPrDefault></w:docDefaults>` +
		`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
		`</w:styles>`
	docXML := `<?xml version="1.0"?><ns0:document xmlns:ns0="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<ns0:body><ns0:p><ns0:r><ns0:t>hi</ns0:t></ns0:r></ns0:p></ns0:body></ns0:document>`
	d := docFromRawParts(t, map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`,
		"word/document.xml":   docXML,
		"word/styles.xml":     stylesXML,
	})
	beforeStyles, _ := d.Part("word/styles.xml")
	beforeStylesCopy := append([]byte(nil), beforeStyles...)

	_, err := d.Format(FormatOptions{BodyFont: "Calibri"})
	if err == nil {
		t.Fatal("want an error (document.xml's non-standard prefix), got nil")
	}
	if err.Error() != wantNonStandardNamespaceErr {
		t.Errorf("error = %q, want %q", err.Error(), wantNonStandardNamespaceErr)
	}

	afterStyles, _ := d.Part("word/styles.xml")
	if !bytes.Equal(beforeStylesCopy, afterStyles) {
		t.Errorf("word/styles.xml was rewritten in memory even though Format returned an error for document.xml's prefix (half-applied Document):\nbefore: %s\nafter:  %s", beforeStylesCopy, afterStyles)
	}
}

// TestFormat_HeadingRPrSelfClosingExpansionIsIdempotent formalizes a
// scenario the task 9 review probe verified but never committed as a test:
// a heading style whose own <w:rPr/> is self-closing (properties-free, but
// the STYLE tag itself is not) gets expanded on the first HeadingFont call,
// and a second, identical call against the already-expanded result is a
// true no-op (byte-identical, no duplicated <w:rFonts>).
func TestFormat_HeadingRPrSelfClosingExpansionIsIdempotent(t *testing.T) {
	styles := []byte(`<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
		`<w:style w:type="paragraph" w:styleId="Heading4"><w:name w:val="heading 4"/><w:rPr/></w:style>` +
		`</w:styles>`)

	once := applyStylesPatches(t, styles, FormatOptions{HeadingFont: "Georgia"})
	if strings.Contains(once, `<w:rPr/>`) {
		t.Fatalf("heading's rPr is still self-closing after the first call:\n%s", once)
	}
	if !strings.Contains(once, `<w:rPr><w:rFonts w:ascii="Georgia" w:hAnsi="Georgia"/></w:rPr>`) {
		t.Fatalf("heading's self-closing rPr was not expanded with the new font:\n%s", once)
	}

	twice := applyStylesPatches(t, []byte(once), FormatOptions{HeadingFont: "Georgia"})
	if once != twice {
		t.Errorf("applying the same heading_font twice was not idempotent:\nonce:  %s\ntwice: %s", once, twice)
	}
}

// TestFormat_DocDefaultsSelfClosingPPrExpansionIsIdempotent extends the
// existing TestFormat_DocDefaultsSelfClosingPPrGetsChildNotSibling (which
// only ever runs the expansion once) with the review-probe-verified second
// run: applying the same rule again against the now-expanded pPr must be a
// true no-op.
func TestFormat_DocDefaultsSelfClosingPPrExpansionIsIdempotent(t *testing.T) {
	once := applyStylesPatches(t, []byte(stylesPPrDefaultSelfClosingPPr), FormatOptions{FirstLineIndentChars: 2})
	if strings.Contains(once, `<w:pPr/>`) {
		t.Fatalf("pPr is still self-closing after the first call:\n%s", once)
	}
	twice := applyStylesPatches(t, []byte(once), FormatOptions{FirstLineIndentChars: 2})
	if once != twice {
		t.Errorf("applying the same first_line_indent_chars twice was not idempotent:\nonce:  %s\ntwice: %s", once, twice)
	}
	if strings.Count(twice, "<w:ind ") != 1 {
		t.Errorf("w:ind was duplicated on the second run instead of left alone: %s", twice)
	}
}

// TestFormat_DocDefaultsSelfClosingRPrExpansionIsIdempotent is
// TestFormat_DocDefaultsSelfClosingPPrExpansionIsIdempotent's rPr/rFonts
// counterpart.
func TestFormat_DocDefaultsSelfClosingRPrExpansionIsIdempotent(t *testing.T) {
	once := applyStylesPatches(t, []byte(stylesRPrDefaultSelfClosingRPr), FormatOptions{BodyFont: "Calibri"})
	if strings.Contains(once, `<w:rPr/>`) {
		t.Fatalf("rPr is still self-closing after the first call:\n%s", once)
	}
	twice := applyStylesPatches(t, []byte(once), FormatOptions{BodyFont: "Calibri"})
	if once != twice {
		t.Errorf("applying the same body_font twice was not idempotent:\nonce:  %s\ntwice: %s", once, twice)
	}
	if strings.Count(twice, "<w:rFonts ") != 1 {
		t.Errorf("w:rFonts was duplicated on the second run instead of left alone: %s", twice)
	}
}
