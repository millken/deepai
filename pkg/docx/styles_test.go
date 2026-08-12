package docx

import (
	"encoding/xml"
	"regexp"
	"strings"
	"testing"
)

// 1. Every style this package's document renderer will reference must
// actually be DEFINED in stylesXML — Word does not error on a dangling
// reference, it silently falls back to Normal, which is the "looks like
// it worked but didn't" trap write.go's own doc comments already call out
// for Heading1..6 and Hyperlink.
func TestStyles_AllReferencedStylesAreDefined(t *testing.T) {
	s := string(buildStylesXML())
	for _, id := range allStyleIDs {
		if !strings.Contains(s, `w:styleId="`+id+`"`) {
			t.Errorf("styleId %q is listed in allStyleIDs but not defined in stylesXML", id)
		}
	}
}

// 2. Every basedOn target named anywhere in stylesXML must itself be a
// defined style, or Word silently falls back for the CHILD style too.
func TestStyles_BasedOnTargetsExist(t *testing.T) {
	s := string(buildStylesXML())
	defined := make(map[string]bool, len(allStyleIDs))
	for _, id := range allStyleIDs {
		defined[id] = true
	}
	basedOnRE := regexp.MustCompile(`<w:basedOn w:val="([^"]+)"/>`)
	matches := basedOnRE.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		t.Fatal("no <w:basedOn> found at all; test would be vacuous if styles never inherit")
	}
	for _, m := range matches {
		target := m[1]
		if !defined[target] {
			t.Errorf("basedOn target %q is not a defined style; Word falls back silently", target)
		}
	}
}

// 3. SourceCode must zero its own spacing AND carry contextualSpacing.
// contextualSpacing alone is what collapses the gap between ADJACENT
// paragraphs of the same style; zeroing spacing is needed too since a
// code block's shading has to read as one contiguous band, not stacked
// bars each with their own residual line spacing.
func TestStyles_SourceCodeCollapsesSpacing(t *testing.T) {
	block := styleBlock(t, buildStylesXML(), "SourceCode")
	if !strings.Contains(block, "<w:contextualSpacing/>") {
		t.Error("SourceCode does not carry <w:contextualSpacing/>; code lines would show gaps")
	}
	if !strings.Contains(block, `w:before="0"`) || !strings.Contains(block, `w:after="0"`) {
		t.Error("SourceCode does not zero its own spacing; inherits docDefaults' 200-twip after-spacing")
	}
}

// 4. ListParagraph must carry contextualSpacing, or consecutive list
// items show gaps between them (docDefaults' 200-twip after-spacing).
func TestStyles_ListParagraphCollapsesSpacing(t *testing.T) {
	block := styleBlock(t, buildStylesXML(), "ListParagraph")
	if !strings.Contains(block, "<w:contextualSpacing/>") {
		t.Error("ListParagraph does not carry <w:contextualSpacing/>; list items would show gaps")
	}
}

// 5. TableGrid must be a table-type style (only that type can carry both
// tblPr for borders and pPr for cell-paragraph spacing in one style), and
// it must zero that cell-paragraph spacing or table rows render tall.
func TestStyles_TableGridZeroesCellSpacing(t *testing.T) {
	s := string(buildStylesXML())
	m := regexp.MustCompile(`<w:style w:type="table" w:styleId="TableGrid">.*?</w:style>`).FindString(s)
	if m == "" {
		t.Fatal("TableGrid is not defined as a table-type style")
	}
	if !strings.Contains(m, `w:after="0"`) {
		t.Error("TableGrid does not zero cell paragraph spacing; table rows will be tall")
	}
}

// 6. Every heading style must carry keepNext, or a heading can strand at
// the bottom of a page while its body text flows onto the next one.
func TestStyles_HeadingsKeepWithNext(t *testing.T) {
	for _, id := range []string{"Heading1", "Heading2", "Heading3", "Heading4", "Heading5", "Heading6"} {
		block := styleBlock(t, buildStylesXML(), id)
		if !strings.Contains(block, "<w:keepNext/>") {
			t.Errorf("%s does not carry <w:keepNext/>; it can strand at the bottom of a page", id)
		}
	}
}

// 7. docDefaults' chain must be complete: rPrDefault (with rFonts/sz/
// szCs/lang) followed by pPrDefault (with spacing). docx_format's
// BodyFont/BodySizePt/LineSpacing patches land here, and fb3e09e fixed
// docx_write once already for omitting exactly this.
func TestStyles_DocDefaultsChainIsComplete(t *testing.T) {
	s := string(buildStylesXML())
	dd := regexp.MustCompile(`<w:docDefaults>.*?</w:docDefaults>`).FindString(s)
	if dd == "" {
		t.Fatal("no <w:docDefaults> found")
	}
	rPrDefaultIdx := strings.Index(dd, "<w:rPrDefault>")
	pPrDefaultIdx := strings.Index(dd, "<w:pPrDefault>")
	if rPrDefaultIdx == -1 {
		t.Fatal("docDefaults has no <w:rPrDefault>")
	}
	if pPrDefaultIdx == -1 {
		t.Fatal("docDefaults has no <w:pPrDefault>")
	}
	if rPrDefaultIdx > pPrDefaultIdx {
		t.Error("<w:rPrDefault> must precede <w:pPrDefault> within docDefaults")
	}
	for _, want := range []string{"<w:rFonts", "<w:sz ", "<w:szCs ", "<w:lang "} {
		if !strings.Contains(dd, want) {
			t.Errorf("docDefaults' rPrDefault is missing %s; docx_format has nowhere to land a body font/size change", want)
		}
	}
	if !strings.Contains(dd, "<w:spacing") {
		t.Error("docDefaults' pPrDefault is missing <w:spacing>; docx_format has nowhere to land a line-spacing change")
	}
}

// 7b. <w:lang>'s w:eastAsia attribute is a LANGUAGE (used by Word for East
// Asian line-breaking/禁则 rules and proofing), not a font -- unrelated to
// which font w:eastAsia on <w:rFonts> names. This package generates
// Chinese documents by design (the docx-chinese-typography plan; see
// defaultBodyEastAsiaFont/defaultCodeEastAsiaFont), so declaring the East
// Asian proofing language "en-US" (a real defect this pinned once it was
// found: docDefaultsXML previously copied Word's own US-English stock
// default for this attribute verbatim, which was never actually correct
// for this package's own output) mislabels every Chinese character this
// package ever writes. w:val (the Latin-script language) is deliberately
// left "en-US" -- unrelated claim, unaffected by this fix.
func TestStyles_DocDefaultsEastAsianLanguageIsChinese(t *testing.T) {
	s := string(buildStylesXML())
	dd := regexp.MustCompile(`<w:docDefaults>.*?</w:docDefaults>`).FindString(s)
	if dd == "" {
		t.Fatal("no <w:docDefaults> found")
	}
	if !strings.Contains(dd, `<w:lang w:val="en-US" w:eastAsia="zh-CN" w:bidi="ar-SA"/>`) {
		t.Errorf("docDefaults <w:lang> = %s, want w:eastAsia=\"zh-CN\" (found lang element in: %s)",
			regexp.MustCompile(`<w:lang[^/]*/>`).FindString(dd), dd)
	}
}

// 8. docDefaults must be w:styles' FIRST child. Word treats an
// out-of-schema-order styles part as corrupt and gives no diagnostic, so
// this has to be checked, not assumed.
func TestStyles_DocDefaultsIsFirstChild(t *testing.T) {
	s := string(buildStylesXML())
	opening := regexp.MustCompile(`<w:styles[^>]*>`).FindString(s)
	if opening == "" {
		t.Fatal("no <w:styles> opening tag found")
	}
	rest := strings.TrimPrefix(s, s[:strings.Index(s, opening)+len(opening)])
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "<w:docDefaults>") {
		t.Fatalf("first child of <w:styles> is not <w:docDefaults>; got: %.60s", rest)
	}
}

// stylesXML is built by hand as one long concatenated string (like
// write.go's existing stylesPartXML), so a stray typo could produce
// invalid XML that nonetheless still "matches" a loose regexp. Decoding
// it with the standard library's own XML parser is an independent check
// that every tag actually opens and closes correctly.
func TestStyles_XMLIsWellFormed(t *testing.T) {
	dec := xml.NewDecoder(strings.NewReader(string(buildStylesXML())))
	for {
		_, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("stylesXML is not well-formed XML: %v", err)
		}
	}
}

// CT_PPr's child sequence is fixed by the schema: among others,
// pBdr/shd precede spacing, and spacing precedes ind precedes
// contextualSpacing precedes outlineLvl precedes rPr. This walks every
// <w:pPr> block this package emits and checks its children never regress
// against that canonical order — mechanical enforcement of the plan's
// "schema-ordered" requirement rather than an eyeball read.
func TestStyles_PPrChildrenAreInSchemaOrder(t *testing.T) {
	canonical := []string{
		"pStyle", "keepNext", "keepLines", "pageBreakBefore", "numPr",
		"pBdr", "shd", "tabs", "spacing", "ind", "contextualSpacing",
		"jc", "outlineLvl", "rPr",
	}
	rank := make(map[string]int, len(canonical))
	for i, n := range canonical {
		rank[n] = i
	}
	s := string(buildStylesXML())
	for _, block := range regexp.MustCompile(`<w:pPr>(.*?)</w:pPr>`).FindAllStringSubmatch(s, -1) {
		last := -1
		lastName := ""
		for _, m := range regexp.MustCompile(`<w:(\w+)`).FindAllStringSubmatch(block[1], -1) {
			name := m[1]
			r, ok := rank[name]
			if !ok {
				continue // element not in our canonical subset (e.g. self-closing attrs' children)
			}
			if r < last {
				t.Errorf("in pPr block %q: %s (rank %d) appears after %s (rank %d), violating CT_PPr's schema order",
					block[1], name, r, lastName, last)
			}
			last, lastName = r, name
		}
	}
}

// CT_Style's own child sequence: name, basedOn, next, qFormat, pPr, rPr,
// tblPr, tblStylePr (0 or more, always last -- CT_Style's own sequence puts
// trPr/tcPr between tblPr and tblStylePr, but this package's styles never
// use those two, so they are simply absent from the canonical list rather
// than tracked with nothing to check). Same rationale as the pPr check
// above, applied to the enclosing <w:style> element.
//
// tblStylePr's own children (CT_TblStylePr: pPr?, rPr?, tblPr?, trPr?,
// tcPr?) are a SEPARATE nested schema scope from the enclosing <w:style>'s
// -- TableGrid's own <w:tblStylePr><w:rPr>...</w:rPr><w:tcPr>...</w:tcPr>
// carries an "rPr" tag that would otherwise look, to this test's flat
// tag-name scan, like a second top-level <w:rPr> appearing suspiciously
// late (after <w:tblPr>) in the outer <w:style> — a false positive, not a
// real ordering defect. Each <w:tblStylePr>...</w:tblStylePr> span is
// collapsed to a bare self-closing placeholder before the scan below, so
// only the outer style's own direct-descendant-shaped tags are checked
// against CT_Style's order, exactly as intended.
func TestStyles_StyleChildrenAreInSchemaOrder(t *testing.T) {
	canonical := []string{"name", "basedOn", "next", "qFormat", "pPr", "rPr", "tblPr", "tblStylePr"}
	rank := make(map[string]int, len(canonical))
	for i, n := range canonical {
		rank[n] = i
	}
	collapseTblStylePr := regexp.MustCompile(`<w:tblStylePr[^>]*>.*?</w:tblStylePr>`)
	s := string(buildStylesXML())
	for _, block := range regexp.MustCompile(`<w:style [^>]*>(.*?)</w:style>`).FindAllStringSubmatch(s, -1) {
		content := collapseTblStylePr.ReplaceAllString(block[1], `<w:tblStylePr/>`)
		last := -1
		lastName := ""
		for _, m := range regexp.MustCompile(`<w:(\w+)`).FindAllStringSubmatch(content, -1) {
			name := m[1]
			r, ok := rank[name]
			if !ok {
				continue
			}
			if r < last {
				t.Errorf("in style block %q: %s (rank %d) appears after %s (rank %d), violating CT_Style's schema order",
					content, name, r, lastName, last)
			}
			last, lastName = r, name
		}
	}
}

// styleBlock extracts the <w:style ... w:styleId="id">...</w:style> block
// so a test can assert on just that style's own properties without a
// substring match accidentally landing in a neighboring style.
func styleBlock(t *testing.T, xmlDoc []byte, id string) string {
	t.Helper()
	re := regexp.MustCompile(`<w:style [^>]*w:styleId="` + id + `"[^>]*>.*?</w:style>`)
	m := re.FindString(string(xmlDoc))
	if m == "" {
		t.Fatalf("style %q not found in stylesXML", id)
	}
	return m
}
