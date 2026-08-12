package docx

// Task 9 (P2): docx_format's matching robustness against documents this
// package did not itself generate -- zh-CN/WPS heading styleIds, headings
// with no <w:rPr> at all, documents missing <w:pgMar>/word/styles.xml
// entirely, a non-"w:" wordprocessingml namespace prefix, and self-closing
// <w:pPr/>/<w:rPr/> inside docDefaults. See .superpowers/sdd/task-9-brief.md.

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Item 1: heading_font's dual-channel match (styleId OR w:name).
// ---------------------------------------------------------------------------

// stylesZhCNHeadingByNameOnly is a zh-CN/WPS-shaped fixture: the styleId is
// a bare "1", not "Heading1", but the w:name is still the English
// "heading 1" Word's own UI derives from -- exactly Important 5's own
// observation. The pre-task-9 styleId-only regex silently did nothing here.
const stylesZhCNHeadingByNameOnly = `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
	`<w:style w:type="paragraph" w:styleId="1"><w:name w:val="heading 1"/><w:basedOn w:val="Normal"/>` +
	`<w:rPr><w:rFonts w:ascii="SimSun" w:hAnsi="SimSun"/></w:rPr></w:style>` +
	`</w:styles>`

func TestFormat_HeadingFontMatchesZhCNStyleIDViaNameChannel(t *testing.T) {
	patches, applied, notes, err := planStylesPatches([]byte(stylesZhCNHeadingByNameOnly), FormatOptions{HeadingFont: "Georgia"}, nil)
	if err != nil {
		t.Fatalf("planStylesPatches: %v", err)
	}
	if len(patches) == 0 {
		t.Fatal("want at least one patch for the name-channel heading match, got none")
	}
	out, err := Apply([]byte(stylesZhCNHeadingByNameOnly), patches)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	mustWellFormedXML(t, out)
	h1 := string(out)[strings.Index(string(out), `w:styleId="1"`):]
	if !strings.Contains(h1, `w:ascii="Georgia"`) {
		t.Errorf("zh-CN heading (styleId=\"1\", name=\"heading 1\") was not matched:\n%.300s", h1)
	}
	var appliedHeading bool
	for _, a := range applied {
		if strings.Contains(a, "heading font") {
			appliedHeading = true
		}
	}
	if !appliedHeading {
		t.Errorf("applied = %v, want a heading font entry", applied)
	}
	for _, n := range notes {
		if strings.Contains(n, "no heading styles found") {
			t.Errorf("notes = %v, want no \"no heading styles found\" note: one WAS found", notes)
		}
	}
}

// TestFormat_HeadingFontNameChannelIsCaseInsensitive pins headingLikeNameRe's
// own case-insensitivity for the NEW matching channel (it already was for
// the style-chain classification it was originally written for).
func TestFormat_HeadingFontNameChannelIsCaseInsensitive(t *testing.T) {
	styles := `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
		`<w:style w:type="paragraph" w:styleId="2"><w:name w:val="Heading 2"/><w:rPr><w:rFonts w:ascii="SimSun"/></w:rPr></w:style>` +
		`</w:styles>`
	patches, _, _, err := planStylesPatches([]byte(styles), FormatOptions{HeadingFont: "Georgia"}, nil)
	if err != nil {
		t.Fatalf("planStylesPatches: %v", err)
	}
	if len(patches) == 0 {
		t.Fatal("want a patch for \"Heading 2\" (capitalized), got none")
	}
}

// ---------------------------------------------------------------------------
// Item 2: insert a brand new <w:rPr> when a matched heading style has none
// at all, at the schema-correct position.
// ---------------------------------------------------------------------------

const stylesHeadingWithPPrNoRPr = `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
	`<w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/><w:basedOn w:val="Normal"/>` +
	`<w:pPr><w:keepNext/><w:outlineLvl w:val="0"/></w:pPr></w:style>` +
	`</w:styles>`

func TestFormat_HeadingFontInsertsRPrRightAfterPPrWhenMissing(t *testing.T) {
	out := applyStylesPatches(t, []byte(stylesHeadingWithPPrNoRPr), FormatOptions{HeadingFont: "Georgia"})
	want := `</w:pPr><w:rPr><w:rFonts w:ascii="Georgia" w:hAnsi="Georgia"/></w:rPr></w:style>`
	if !strings.Contains(out, want) {
		t.Errorf("rPr was not inserted right after </w:pPr>, before </w:style>:\nwant substring: %s\ngot: %s", want, out)
	}
}

const stylesHeadingNoChildrenAtAll = `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
	`<w:style w:type="paragraph" w:styleId="Heading2"><w:name w:val="heading 2"/></w:style>` +
	`</w:styles>`

func TestFormat_HeadingFontInsertsRPrWhenStyleHasNoChildrenAtAll(t *testing.T) {
	out := applyStylesPatches(t, []byte(stylesHeadingNoChildrenAtAll), FormatOptions{HeadingFont: "Georgia"})
	want := `<w:style w:type="paragraph" w:styleId="Heading2"><w:name w:val="heading 2"/><w:rPr><w:rFonts w:ascii="Georgia" w:hAnsi="Georgia"/></w:rPr></w:style>`
	if !strings.Contains(out, want) {
		t.Errorf("rPr was not inserted as the style's own child:\nwant substring: %s\ngot: %s", want, out)
	}
}

const stylesHeadingFullySelfClosing = `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
	`<w:style w:type="paragraph" w:styleId="Heading3"/>` +
	`</w:styles>`

// TestFormat_HeadingFontExpandsFullySelfClosingStyle covers a fully empty
// <w:style .../> (matched via styleId only, since it has no <w:name> for
// the name channel to see at all) -- the old code silently did nothing for
// this shape too.
func TestFormat_HeadingFontExpandsFullySelfClosingStyle(t *testing.T) {
	out := applyStylesPatches(t, []byte(stylesHeadingFullySelfClosing), FormatOptions{HeadingFont: "Georgia"})
	want := `<w:style w:type="paragraph" w:styleId="Heading3"><w:rPr><w:rFonts w:ascii="Georgia" w:hAnsi="Georgia"/></w:rPr></w:style>`
	if !strings.Contains(out, want) {
		t.Errorf("self-closing heading style was not expanded:\nwant substring: %s\ngot: %s", want, out)
	}
}

// ---------------------------------------------------------------------------
// Item 3: heading_font must land in Notes, not total silence, when it
// matches zero styles at all.
// ---------------------------------------------------------------------------

const stylesNoHeadingsAtAll = `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
	`</w:styles>`

func TestFormat_HeadingFontNoMatchAddsNote(t *testing.T) {
	patches, applied, notes, err := planStylesPatches([]byte(stylesNoHeadingsAtAll), FormatOptions{HeadingFont: "Georgia"}, nil)
	if err != nil {
		t.Fatalf("planStylesPatches: %v", err)
	}
	if len(patches) != 0 {
		t.Errorf("want zero patches, got %d", len(patches))
	}
	for _, a := range applied {
		if strings.Contains(a, "heading font") {
			t.Errorf("applied = %v, want no heading font entry", applied)
		}
	}
	want := "no heading styles found; nothing changed"
	var found bool
	for _, n := range notes {
		if n == want {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %v, want %q", notes, want)
	}
}

// ---------------------------------------------------------------------------
// Item 4a: a section missing <w:pgMar> gets one INSERTED (OOXML defaults
// for header/footer/gutter, marginsMM for top/right/bottom/left) instead
// of failing the whole call.
// ---------------------------------------------------------------------------

func TestPlanMarginPatches_InsertsPgMarWhenMissing(t *testing.T) {
	doc := []byte(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:body><w:p/><w:sectPr><w:pgSz w:w="12240" w:h="15840"/></w:sectPr></w:body></w:document>`)
	patches, err := planMarginPatches(doc, []float64{25.4, 25.4, 25.4, 25.4})
	if err != nil {
		t.Fatalf("planMarginPatches: %v", err)
	}
	if len(patches) == 0 {
		t.Fatal("want a patch inserting pgMar, got none")
	}
	out, err := Apply(doc, patches)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	mustWellFormedXML(t, out)
	want := `<w:pgSz w:w="12240" w:h="15840"/><w:pgMar w:header="720" w:footer="720" w:gutter="0" w:top="1440" w:right="1440" w:bottom="1440" w:left="1440"/></w:sectPr>`
	if !strings.Contains(string(out), want) {
		t.Errorf("pgMar was not inserted right after pgSz with OOXML defaults:\nwant substring: %s\ngot: %s", want, out)
	}
}

func TestPlanMarginPatches_ExpandsSelfClosingSectPr(t *testing.T) {
	doc := []byte(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:body><w:p/><w:sectPr/></w:body></w:document>`)
	patches, err := planMarginPatches(doc, []float64{10, 10, 10, 10})
	if err != nil {
		t.Fatalf("planMarginPatches: %v", err)
	}
	out, err := Apply(doc, patches)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	mustWellFormedXML(t, out)
	// 10mm * 1440/25.4 = 566.9... -> rounds to 567.
	want := `<w:sectPr><w:pgMar w:header="720" w:footer="720" w:gutter="0" w:top="567" w:right="567" w:bottom="567" w:left="567"/></w:sectPr>`
	if !strings.Contains(string(out), want) {
		t.Errorf("self-closing sectPr was not expanded to hold pgMar:\nwant substring: %s\ngot: %s", want, out)
	}
}

func TestPlanMarginPatches_PreservesHeaderFooterGutterWhenPgMarAlreadyExists(t *testing.T) {
	doc := []byte(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:body><w:p/><w:sectPr><w:pgMar w:top="1000" w:right="1000" w:bottom="1000" w:left="1000" w:header="500" w:footer="500" w:gutter="100"/></w:sectPr></w:body></w:document>`)
	patches, err := planMarginPatches(doc, []float64{25.4, 25.4, 25.4, 25.4})
	if err != nil {
		t.Fatalf("planMarginPatches: %v", err)
	}
	out, err := Apply(doc, patches)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	mustWellFormedXML(t, out)
	s := string(out)
	if !strings.Contains(s, `w:top="1440"`) || !strings.Contains(s, `w:left="1440"`) {
		t.Errorf("requested margins were not applied:\n%s", s)
	}
	if !strings.Contains(s, `w:header="500"`) || !strings.Contains(s, `w:footer="500"`) || !strings.Contains(s, `w:gutter="100"`) {
		t.Errorf("pre-existing header/footer/gutter were not preserved:\n%s", s)
	}
}

// TestFormat_MarginsInsertsPgMarWhenMissingEndToEnd exercises the same fix
// through the real Document.Format entry point (not just the plan-level
// helper), pinning that Applied reports the change honestly and that the
// written word/document.xml actually carries the new pgMar.
func TestFormat_MarginsInsertsPgMarWhenMissingEndToEnd(t *testing.T) {
	d := bodyDoc(t, `<w:p/><w:sectPr><w:pgSz w:w="12240" w:h="15840"/></w:sectPr>`)
	res, err := d.Format(FormatOptions{MarginsMM: []float64{25.4, 25.4, 25.4, 25.4}})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	var found bool
	for _, a := range res.Applied {
		if strings.Contains(a, "margins") {
			found = true
		}
	}
	if !found {
		t.Errorf("Applied = %v, want a margins entry", res.Applied)
	}
	doc, _ := d.Part(DocumentPart)
	if !strings.Contains(string(doc), `<w:pgMar w:header="720" w:footer="720" w:gutter="0" w:top="1440"`) {
		t.Errorf("pgMar was not inserted into the saved document.xml:\n%s", doc)
	}
}

// ---------------------------------------------------------------------------
// Item 4b: a missing word/styles.xml part must produce a rich error naming
// which rules are affected (and, when margins_mm/normalize were ALSO
// requested this same call, that those are unaffected).
// ---------------------------------------------------------------------------

// docWithoutStylesXML builds a minimal .docx with ONLY [Content_Types].xml
// and word/document.xml -- Open (zipio.go) only requires those two, so
// word/styles.xml being entirely absent is a legal (if unusual) package
// this test can construct directly, rather than needing SetPart (which
// cannot add a brand new zip entry).
func docWithoutStylesXML(t *testing.T, bodyXML string) *Document {
	t.Helper()
	docXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:body>` + bodyXML + `</w:body></w:document>`
	return docFromRawParts(t, map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`,
		"word/document.xml":   docXML,
	})
}

func TestFormat_MissingStylesXMLListsAffectedRules(t *testing.T) {
	d := docWithoutStylesXML(t, `<w:p><w:r><w:t>hi</w:t></w:r></w:p><w:sectPr><w:pgSz w:w="12240" w:h="15840"/></w:sectPr>`)
	_, err := d.Format(FormatOptions{BodyFont: "Calibri", MarginsMM: []float64{25.4, 25.4, 25.4, 25.4}})
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"word/styles.xml", "body_font", "margins_mm"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Item 5: a non-"w:" wordprocessingml namespace prefix must be a pinpoint
// error, never a silent corruption (rPr landing outside any real element,
// or an attribute/element carrying an undeclared prefix).
// ---------------------------------------------------------------------------

// docFromRawParts builds a minimal .docx from exactly the given parts (no
// implicit envelope, unlike bodyDoc) -- needed here because these tests
// control document.xml/styles.xml's own ROOT element and namespace
// declarations directly.
func docFromRawParts(t *testing.T, parts map[string]string) *Document {
	t.Helper()
	p := filepath.Join(t.TempDir(), "raw.docx")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// A fixed, deterministic order; entries not present in parts are
	// skipped.
	for _, name := range []string{"[Content_Types].xml", "word/document.xml", "word/styles.xml"} {
		content, ok := parts[name]
		if !ok {
			continue
		}
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := OpenDocument(p)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	return d
}

const wantNonStandardNamespaceErr = "docx: document uses a non-standard namespace prefix; formatting is not supported for this file"

// TestFormat_NonWPrefixStylesXMLErrorsInsteadOfCorrupting is scenario R5b:
// styles.xml legitimately, validly declares the wordprocessingml namespace
// under prefix "ns0" instead of "w". buildTag's hardcoded "w:" emission
// cannot honor that, so Format must refuse outright rather than insert a
// <w:rFonts> under a prefix this document never declared.
func TestFormat_NonWPrefixStylesXMLErrorsInsteadOfCorrupting(t *testing.T) {
	stylesXML := `<?xml version="1.0"?><ns0:styles xmlns:ns0="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<ns0:docDefaults><ns0:rPrDefault><ns0:rPr><ns0:rFonts ns0:ascii="Existing"/></ns0:rPr></ns0:rPrDefault></ns0:docDefaults>` +
		`<ns0:style ns0:type="paragraph" ns0:default="1" ns0:styleId="Normal"><ns0:name ns0:val="Normal"/></ns0:style>` +
		`</ns0:styles>`
	docXML := `<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:body><w:p/></w:body></w:document>`
	d := docFromRawParts(t, map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`,
		"word/document.xml":   docXML,
		"word/styles.xml":     stylesXML,
	})
	before, _ := d.Part("word/styles.xml")
	beforeCopy := append([]byte(nil), before...)

	_, err := d.Format(FormatOptions{BodyFont: "Calibri"})
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if err.Error() != wantNonStandardNamespaceErr {
		t.Errorf("error = %q, want %q", err.Error(), wantNonStandardNamespaceErr)
	}

	after, _ := d.Part("word/styles.xml")
	if !bytes.Equal(beforeCopy, after) {
		t.Errorf("word/styles.xml was modified despite the error:\nbefore: %s\nafter:  %s", beforeCopy, after)
	}
}

// TestFormat_NonWPrefixDocumentXMLErrorsInsteadOfCorrupting is scenario
// R5c: document.xml (not styles.xml) uses the non-standard prefix, hit via
// the margins path.
func TestFormat_NonWPrefixDocumentXMLErrorsInsteadOfCorrupting(t *testing.T) {
	docXML := `<?xml version="1.0"?><ns0:document xmlns:ns0="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<ns0:body><ns0:p/><ns0:sectPr><ns0:pgSz ns0:w="12240" ns0:h="15840"/></ns0:sectPr></ns0:body></ns0:document>`
	d := docFromRawParts(t, map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`,
		"word/document.xml":   docXML,
	})
	before, _ := d.Part(DocumentPart)
	beforeCopy := append([]byte(nil), before...)

	_, err := d.Format(FormatOptions{MarginsMM: []float64{25.4, 25.4, 25.4, 25.4}})
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if err.Error() != wantNonStandardNamespaceErr {
		t.Errorf("error = %q, want %q", err.Error(), wantNonStandardNamespaceErr)
	}

	after, _ := d.Part(DocumentPart)
	if !bytes.Equal(beforeCopy, after) {
		t.Errorf("word/document.xml was modified despite the error:\nbefore: %s\nafter:  %s", beforeCopy, after)
	}
}

// TestFormatDirectRange_NonWPrefixDocumentXMLErrorsInsteadOfCorrupting
// covers the SAME non-standard-prefix guard on the paragraph-range path
// (formatDirectRange), which edits word/document.xml directly and was the
// other call site the original review named (format_direct.go:389).
func TestFormatDirectRange_NonWPrefixDocumentXMLErrorsInsteadOfCorrupting(t *testing.T) {
	docXML := `<?xml version="1.0"?><ns0:document xmlns:ns0="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<ns0:body><ns0:p><ns0:r><ns0:t>hi</ns0:t></ns0:r></ns0:p></ns0:body></ns0:document>`
	d := docFromRawParts(t, map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`,
		"word/document.xml":   docXML,
	})
	before, _ := d.Part(DocumentPart)
	beforeCopy := append([]byte(nil), before...)

	_, err := d.Format(FormatOptions{StartPara: 1, BodyFont: "Calibri"})
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if err.Error() != wantNonStandardNamespaceErr {
		t.Errorf("error = %q, want %q", err.Error(), wantNonStandardNamespaceErr)
	}

	after, _ := d.Part(DocumentPart)
	if !bytes.Equal(beforeCopy, after) {
		t.Errorf("word/document.xml was modified despite the error:\nbefore: %s\nafter:  %s", beforeCopy, after)
	}
}

// ---------------------------------------------------------------------------
// Item 6 (Task 8 review round 2's old debt): a self-closing <w:pPr/>/
// <w:rPr/> inside docDefaults must be EXPANDED so new content becomes a
// real child, not appended as a trailing sibling of the self-closing tag.
// ---------------------------------------------------------------------------

const stylesPPrDefaultSelfClosingPPr = `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	`<w:docDefaults><w:rPrDefault><w:rPr><w:rFonts w:ascii="Existing"/></w:rPr></w:rPrDefault>` +
	`<w:pPrDefault><w:pPr/></w:pPrDefault></w:docDefaults>` +
	`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
	`</w:styles>`

func TestFormat_DocDefaultsSelfClosingPPrGetsChildNotSibling(t *testing.T) {
	out := applyStylesPatches(t, []byte(stylesPPrDefaultSelfClosingPPr), FormatOptions{FirstLineIndentChars: 2})
	if strings.Contains(out, `<w:pPr/>`) {
		t.Errorf("pPr is still self-closing; the new <w:ind> must have become a CHILD:\n%s", out)
	}
	if strings.Contains(out, `<w:pPr/><w:ind`) {
		t.Errorf("<w:ind> landed as pPr's SIBLING, not its child:\n%s", out)
	}
	if !strings.Contains(out, `<w:pPr><w:ind`) {
		t.Errorf("<w:ind> was not inserted as pPr's own child:\n%s", out)
	}
}

const stylesRPrDefaultSelfClosingRPr = `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	`<w:docDefaults><w:rPrDefault><w:rPr/></w:rPrDefault>` +
	`<w:pPrDefault><w:pPr><w:jc w:val="left"/></w:pPr></w:pPrDefault></w:docDefaults>` +
	`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
	`</w:styles>`

func TestFormat_DocDefaultsSelfClosingRPrGetsChildNotSibling(t *testing.T) {
	out := applyStylesPatches(t, []byte(stylesRPrDefaultSelfClosingRPr), FormatOptions{BodyFont: "Calibri"})
	if strings.Contains(out, `<w:rPr/>`) {
		t.Errorf("rPr is still self-closing; the new <w:rFonts> must have become a CHILD:\n%s", out)
	}
	if strings.Contains(out, `<w:rPr/><w:rFonts`) {
		t.Errorf("<w:rFonts> landed as rPr's SIBLING, not its child:\n%s", out)
	}
	if !strings.Contains(out, `<w:rPr><w:rFonts`) {
		t.Errorf("<w:rFonts> was not inserted as rPr's own child:\n%s", out)
	}
	if !strings.Contains(out, `<w:jc w:val="left"/>`) {
		t.Errorf("the unrelated pPrDefault sibling was not preserved:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// Item 7b (Task 8 review round 2's third nit): silently dropping an
// existing w:hanging/w:hangingChars or w:beforeAutospacing/
// w:afterAutospacing must be surfaced in Notes, not left as a silent
// side effect of setting first_line_indent_chars/space_before_pt/
// space_after_pt.
// ---------------------------------------------------------------------------

const stylesDocDefaultsIndWithHanging = `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	`<w:docDefaults><w:rPrDefault><w:rPr><w:rFonts w:ascii="Existing"/></w:rPr></w:rPrDefault>` +
	`<w:pPrDefault><w:pPr><w:ind w:hanging="240" w:hangingChars="200"/></w:pPr></w:pPrDefault></w:docDefaults>` +
	`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
	`</w:styles>`

func TestFormat_FirstLineIndentDropsHangingWithNoteInDocDefaults(t *testing.T) {
	_, _, notes, err := planStylesPatches([]byte(stylesDocDefaultsIndWithHanging), FormatOptions{FirstLineIndentChars: 2}, nil)
	if err != nil {
		t.Fatalf("planStylesPatches: %v", err)
	}
	var found bool
	for _, n := range notes {
		if strings.Contains(n, "hanging") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %v, want a mention of the dropped hanging indent", notes)
	}
}

const stylesDocDefaultsSpacingWithBeforeAutospacing = `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	`<w:docDefaults><w:rPrDefault><w:rPr><w:rFonts w:ascii="Existing"/></w:rPr></w:rPrDefault>` +
	`<w:pPrDefault><w:pPr><w:spacing w:before="0" w:beforeAutospacing="1"/></w:pPr></w:pPrDefault></w:docDefaults>` +
	`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
	`</w:styles>`

func TestFormat_SpaceBeforeDropsAutospacingWithNoteInDocDefaults(t *testing.T) {
	_, _, notes, err := planStylesPatches([]byte(stylesDocDefaultsSpacingWithBeforeAutospacing), FormatOptions{SpaceBeforePt: 6}, nil)
	if err != nil {
		t.Fatalf("planStylesPatches: %v", err)
	}
	var found bool
	for _, n := range notes {
		if strings.Contains(n, "beforeAutospacing") || strings.Contains(n, "space_before_pt") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %v, want a mention of the dropped w:beforeAutospacing flag", notes)
	}
}

// stylesNormalIndWithHangingAndFirstLine gives Normal itself an explicit
// <w:ind> that already carries BOTH a firstLine (so it is eligible for the
// style-chain "already shadows this field" rewrite at all) AND a hanging
// indent (the attribute the rewrite silently drops).
const stylesNormalIndWithHangingAndFirstLine = `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	`<w:docDefaults><w:rPrDefault><w:rPr><w:rFonts w:ascii="Existing"/></w:rPr></w:rPrDefault><w:pPrDefault><w:pPr></w:pPr></w:pPrDefault></w:docDefaults>` +
	`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/>` +
	`<w:pPr><w:ind w:firstLine="210" w:firstLineChars="100" w:hanging="240" w:hangingChars="200"/></w:pPr></w:style>` +
	`</w:styles>`

func TestPlanStyleChainShadowPatches_FirstLineIndentDropsHangingWithNote(t *testing.T) {
	_, _, notes, err := planStyleChainShadowPatches([]byte(stylesNormalIndWithHangingAndFirstLine),
		FormatOptions{FirstLineIndentChars: 2}, map[string]bool{"Normal": true}, nil)
	if err != nil {
		t.Fatalf("planStyleChainShadowPatches: %v", err)
	}
	var found bool
	for _, n := range notes {
		if strings.Contains(n, "hanging") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %v, want a mention of the dropped hanging indent", notes)
	}
}

// TestFormatDirectRange_FirstLineIndentDropsHangingWithNote covers the same
// caveat on the paragraph-range path.
func TestFormatDirectRange_FirstLineIndentDropsHangingWithNote(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:pPr><w:ind w:hanging="240" w:hangingChars="200"/></w:pPr><w:r><w:t>hi</w:t></w:r></w:p>`)
	res, err := d.Format(FormatOptions{StartPara: 1, FirstLineIndentChars: 2})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	var found bool
	for _, n := range res.Notes {
		if strings.Contains(n, "hanging") {
			found = true
		}
	}
	if !found {
		t.Errorf("Notes = %v, want a mention of the dropped hanging indent", res.Notes)
	}
}
