package docx

import (
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"testing"
)

// assertPPrChildOrder decodes pPrXML (a whole <w:pPr>...</w:pPr> element)
// and fails the test if any two of its DIRECT children appear out of the
// CT_PPr schema order pPrChildOrder encodes. pPrChildOrder is transcribed
// verbatim from the design brief (.superpowers/sdd/task-1-brief.md, Task 1's
// element sequence list), which in turn transcribes ECMA-376's
// CT_PPrBase/CT_PPr sequence (rPr must follow every CT_PPrBase element and
// precede sectPr; sectPr must precede pPrChange).
func assertPPrChildOrder(t *testing.T, pPrXML string) {
	t.Helper()
	rank := make(map[string]int, len(pPrChildOrder))
	for i, n := range pPrChildOrder {
		rank[n] = i
	}
	dec := xml.NewDecoder(strings.NewReader(pPrXML))
	depth := 0
	lastRank := -1
	lastName := ""
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("pPr fixture is not well-formed XML: %v\n%s", err, pPrXML)
		}
		switch se := tok.(type) {
		case xml.StartElement:
			if depth == 1 {
				if r, ok := rank[se.Name.Local]; ok {
					if r < lastRank {
						t.Errorf("pPr child order violated (CT_PPr schema): <w:%s> (rank %d) appears after <w:%s> (rank %d):\n%s",
							se.Name.Local, r, lastName, lastRank, pPrXML)
					}
					lastRank = r
					lastName = se.Name.Local
				}
			}
			depth++
		case xml.EndElement:
			depth--
		}
	}
}

// extractElem returns the substring of s spanning the first <tag>...</tag>
// (or self-closing <tag/>) occurrence, inclusive, or fails the test if tag
// is not present as an OPEN (non-self-closing) element -- every fixture
// below is deliberately non-self-closing so this always finds a matching
// close.
func extractElem(t *testing.T, s, openTag, closeTag string) string {
	t.Helper()
	start := strings.Index(s, openTag)
	if start == -1 {
		t.Fatalf("no %s found in: %s", openTag, s)
	}
	end := strings.Index(s[start:], closeTag)
	if end == -1 {
		t.Fatalf("no matching %s found in: %s", closeTag, s)
	}
	return s[start : start+end+len(closeTag)]
}

// --- Critical 1: new <w:spacing>/<w:jc> must land in CT_PPr schema order,
// not merely after the two leaves this package tracks. Three fixtures
// (rPr, ind, sectPr), matching format-review.md's Critical 1 examples. ---

// TestDirect_ParagraphFormat_SpacingAndJcLandBeforeExistingRPr covers the
// single most common real-world shape: a paragraph mark's own <w:rPr>
// (e.g. carrying bold for the pilcrow) sitting inside <w:pPr>. CT_PPr
// requires rPr to come AFTER every other paragraph property, so a newly
// inserted <w:spacing>/<w:jc> must land BEFORE it.
func TestDirect_ParagraphFormat_SpacingAndJcLandBeforeExistingRPr(t *testing.T) {
	doc := []byte(`<w:p><w:pPr><w:rPr><w:b/></w:rPr></w:pPr><w:r><w:t>x</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, n, err := applyDirectParaFormat(doc, paras, 1, 1, 1.5, "justify")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("changed %d paragraphs, want 1", n)
	}
	s := string(got)
	if err := checkWellFormed(got); err != nil {
		t.Fatalf("result is not well-formed XML: %v", err)
	}
	pPrXML := extractElem(t, s, "<w:pPr>", "</w:pPr>")
	assertPPrChildOrder(t, pPrXML)

	if !strings.Contains(pPrXML, "<w:b/>") {
		t.Errorf("existing paragraph-mark rPr content was wiped: %s", pPrXML)
	}
	if !strings.Contains(pPrXML, `w:line="360"`) || !strings.Contains(pPrXML, `<w:jc w:val="justify"/>`) {
		t.Errorf("spacing/alignment not applied: %s", pPrXML)
	}
	rprIdx := strings.Index(pPrXML, "<w:rPr>")
	spacingIdx := strings.Index(pPrXML, "<w:spacing")
	jcIdx := strings.Index(pPrXML, "<w:jc")
	if spacingIdx == -1 || spacingIdx > rprIdx {
		t.Errorf("<w:spacing> must precede <w:rPr> (CT_PPr schema order): %s", pPrXML)
	}
	if jcIdx == -1 || jcIdx > rprIdx {
		t.Errorf("<w:jc> must precede <w:rPr> (CT_PPr schema order): %s", pPrXML)
	}
}

// TestDirect_ParagraphFormat_SpacingLandsBeforeExistingInd covers <w:ind>:
// CT_PPrBase's sequence puts spacing BEFORE ind, so a newly inserted
// <w:spacing> landing after an existing <w:ind> is schema-invalid.
func TestDirect_ParagraphFormat_SpacingLandsBeforeExistingInd(t *testing.T) {
	doc := []byte(`<w:p><w:pPr><w:ind w:left="720"/></w:pPr><w:r><w:t>x</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, n, err := applyDirectParaFormat(doc, paras, 1, 1, 1.5, "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("changed %d paragraphs, want 1", n)
	}
	s := string(got)
	if err := checkWellFormed(got); err != nil {
		t.Fatalf("result is not well-formed XML: %v", err)
	}
	pPrXML := extractElem(t, s, "<w:pPr>", "</w:pPr>")
	assertPPrChildOrder(t, pPrXML)

	if !strings.Contains(pPrXML, `w:left="720"`) {
		t.Errorf("existing <w:ind> was wiped: %s", pPrXML)
	}
	spacingIdx := strings.Index(pPrXML, "<w:spacing")
	indIdx := strings.Index(pPrXML, "<w:ind")
	if spacingIdx == -1 || spacingIdx > indIdx {
		t.Errorf("<w:spacing> must precede <w:ind> (CT_PPrBase schema order): %s", pPrXML)
	}
}

// TestDirect_ParagraphFormat_JcLandsBeforeExistingSectPr covers <w:sectPr>:
// a section-break paragraph's <w:pPr><w:sectPr> is common in real multi-
// section documents, and CT_PPr puts jc (a CT_PPrBase member) BEFORE rPr
// and sectPr, both of which are CT_PPr's own trailing additions.
func TestDirect_ParagraphFormat_JcLandsBeforeExistingSectPr(t *testing.T) {
	doc := []byte(`<w:p><w:pPr><w:sectPr><w:pgSz w:w="12240" w:h="15840"/></w:sectPr></w:pPr><w:r><w:t>x</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, n, err := applyDirectParaFormat(doc, paras, 1, 1, 0, "justify")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("changed %d paragraphs, want 1", n)
	}
	s := string(got)
	if err := checkWellFormed(got); err != nil {
		t.Fatalf("result is not well-formed XML: %v", err)
	}
	pPrXML := extractElem(t, s, "<w:pPr>", "</w:pPr>")
	assertPPrChildOrder(t, pPrXML)

	if !strings.Contains(pPrXML, `w:w="12240"`) {
		t.Errorf("existing <w:sectPr> content was wiped: %s", pPrXML)
	}
	jcIdx := strings.Index(pPrXML, "<w:jc")
	sectPrIdx := strings.Index(pPrXML, "<w:sectPr")
	if jcIdx == -1 || jcIdx > sectPrIdx {
		t.Errorf("<w:jc> must precede <w:sectPr> (CT_PPr schema order): %s", pPrXML)
	}
}

// --- Critical 2: a character-spacing <w:spacing w:val=...> nested inside a
// paragraph mark's <w:rPr> must not be mistaken for the paragraph's own
// line-spacing <w:spacing w:line=... w:lineRule=...>. ---

// TestDirect_ParagraphFormat_IgnoresCharacterSpacingInsideNestedRPr is the
// direct-formatting-path (word/document.xml) regression test for Critical 2:
// the old scanner's "inPPR" boolean never turned off on entering the nested
// <w:rPr>, so the existing character-spacing <w:spacing w:val="-2"/> was
// mistaken for pPr's own line-spacing leaf and rewritten in place into an
// illegal attribute combination, while the real line spacing was never
// added at all.
func TestDirect_ParagraphFormat_IgnoresCharacterSpacingInsideNestedRPr(t *testing.T) {
	doc := []byte(`<w:p><w:pPr><w:rPr><w:spacing w:val="-2"/></w:rPr></w:pPr><w:r><w:t>x</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, n, err := applyDirectParaFormat(doc, paras, 1, 1, 1.5, "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("changed %d paragraphs, want 1", n)
	}
	s := string(got)
	if err := checkWellFormed(got); err != nil {
		t.Fatalf("result is not well-formed XML: %v", err)
	}
	pPrXML := extractElem(t, s, "<w:pPr>", "</w:pPr>")
	assertPPrChildOrder(t, pPrXML)

	if !strings.Contains(pPrXML, `<w:spacing w:val="-2"/>`) {
		t.Errorf("the run-level character spacing inside the nested <w:rPr> was corrupted: %s", pPrXML)
	}
	if !strings.Contains(pPrXML, `<w:spacing w:line="360" w:lineRule="auto"/>`) {
		t.Errorf("pPr's own line spacing was not added as a separate direct child: %s", pPrXML)
	}
	if strings.Count(pPrXML, "<w:spacing") != 2 {
		t.Errorf("want exactly two <w:spacing> elements (nested char-spacing + pPr's own line-spacing), got:\n%s", pPrXML)
	}
}

// TestPlanStylesPatches_IgnoresCharacterSpacingInsideDocDefaultsNestedRPr is
// the same Critical 2 bug on the whole-document path: styles.xml's
// <w:pPrDefault><w:pPr> can itself carry an <w:rPr> (CT_PPrGeneral allows
// it), and format.go's scanDocDefaults had the identical unbounded "inPPR"
// flag.
func TestPlanStylesPatches_IgnoresCharacterSpacingInsideDocDefaultsNestedRPr(t *testing.T) {
	const styles = `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:docDefaults><w:pPrDefault><w:pPr><w:rPr><w:spacing w:val="-2"/></w:rPr></w:pPr></w:pPrDefault></w:docDefaults>` +
		`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
		`</w:styles>`
	out := applyStylesPatches(t, []byte(styles), FormatOptions{LineSpacing: 1.5})

	ppr := extractElem(t, out, "<w:pPr>", "</w:pPr>")
	assertPPrChildOrder(t, ppr)
	if !strings.Contains(ppr, `<w:spacing w:val="-2"/>`) {
		t.Errorf("the nested character spacing inside docDefaults' pPr>rPr was corrupted:\n%s", ppr)
	}
	if !strings.Contains(ppr, `<w:spacing w:line="360" w:lineRule="auto"/>`) {
		t.Errorf("docDefaults' own line spacing was not added as pPr's direct child:\n%s", ppr)
	}
	if strings.Count(ppr, "<w:spacing") != 2 {
		t.Errorf("want exactly two <w:spacing> elements (nested char-spacing + pPr's own line-spacing), got:\n%s", ppr)
	}
}

// --- Minor 13: a newly inserted <w:rFonts> must land as rPr's FIRST child
// (EG_RPrBase order), not after whatever direct formatting already occupies
// rPr (e.g. <w:b/>). Same root cause as Critical 1: an incomplete anchor
// set for "what comes after this insertion point". ---

// TestDirect_RunFormat_InsertedRFontsLandsBeforeExistingBold is the
// direct-formatting-path (word/document.xml run rPr) regression test.
func TestDirect_RunFormat_InsertedRFontsLandsBeforeExistingBold(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:rPr><w:b/></w:rPr><w:t>x</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, n, err := applyDirectRunFormat(doc, paras, 1, 1, "Georgia", 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("changed %d paragraphs, want 1", n)
	}
	s := string(got)
	if err := checkWellFormed(got); err != nil {
		t.Fatalf("result is not well-formed XML: %v", err)
	}
	if !strings.Contains(s, "<w:b/>") {
		t.Errorf("existing bold was wiped: %s", s)
	}
	rprOpen := strings.Index(s, "<w:rPr>")
	rfontsIdx := strings.Index(s, "<w:rFonts")
	bIdx := strings.Index(s, "<w:b/>")
	if rfontsIdx == -1 || rfontsIdx != rprOpen+len("<w:rPr>") {
		t.Errorf("<w:rFonts> was not inserted as rPr's very first child: %s", s)
	}
	if rfontsIdx > bIdx {
		t.Errorf("<w:rFonts> must precede <w:b/> (EG_RPrBase schema order); got rFonts at %d, b at %d: %s", rfontsIdx, bIdx, s)
	}
}

// TestPlanStylesPatches_InsertedRFontsLandsBeforeExistingBold is the same
// fix applied consistently to the whole-document docDefaults rPr chain
// (format.go's planStylesPatches), which shares applyLeafOps with
// format_direct.go and had the identical bug.
func TestPlanStylesPatches_InsertedRFontsLandsBeforeExistingBold(t *testing.T) {
	const styles = `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:docDefaults><w:rPrDefault><w:rPr><w:b/></w:rPr></w:rPrDefault></w:docDefaults>` +
		`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
		`</w:styles>`
	out := applyStylesPatches(t, []byte(styles), FormatOptions{BodyFont: "Georgia"})

	rpr := extractElem(t, out, "<w:rPr>", "</w:rPr>")
	if !strings.Contains(rpr, "<w:b/>") {
		t.Errorf("existing bold was wiped: %s", rpr)
	}
	rfontsIdx := strings.Index(rpr, "<w:rFonts")
	bIdx := strings.Index(rpr, "<w:b/>")
	if rfontsIdx == -1 || bIdx == -1 || rfontsIdx > bIdx {
		t.Errorf("<w:rFonts> must precede the pre-existing <w:b/>: %s", rpr)
	}
}

// --- Follow-up review findings (same root cause: "a boolean flag never
// closes over a nested container of the same kind"), found by a later
// review pass over this same fix. <w:rPrChange> wraps a historical snapshot
// of an OLDER <w:rPr> (revision tracking); <inRPR-as-boolean> code mistook
// that nested rPr's own rFonts/sz/szCs — and even its closing tag — for the
// CURRENT rPr's. ---

// TestDirect_RunFormat_IgnoresHistoricalPropertiesInsideRPrChange is the
// direct-formatting-path regression test: a run's rPr carrying revision
// history (<w:rPrChange><w:rPr>...old properties...</w:rPr></w:rPrChange>)
// must have its historical rFonts/sz left completely alone, and the new
// font/size must land in the CURRENT rPr (before <w:rPrChange>), not be
// silently absorbed into editing the historical copy in place.
func TestDirect_RunFormat_IgnoresHistoricalPropertiesInsideRPrChange(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:rPr><w:rPrChange w:id="1" w:author="A" w:date="2020-01-01T00:00:00Z">` +
		`<w:rPr><w:rFonts w:ascii="Old"/><w:sz w:val="20"/></w:rPr></w:rPrChange></w:rPr><w:t>x</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, n, err := applyDirectRunFormat(doc, paras, 1, 1, "Georgia", 14)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("changed %d paragraphs, want 1", n)
	}
	s := string(got)
	if err := checkWellFormed(got); err != nil {
		t.Fatalf("result is not well-formed XML: %v", err)
	}
	if !strings.Contains(s, `w:ascii="Old"`) {
		t.Errorf("historical rFonts inside <w:rPrChange> was corrupted: %s", s)
	}
	if !strings.Contains(s, `w:val="20"`) {
		t.Errorf("historical sz inside <w:rPrChange> was corrupted: %s", s)
	}
	if !strings.Contains(s, `w:ascii="Georgia"`) {
		t.Errorf("the new font was not applied to the run's CURRENT rPr: %s", s)
	}
	if !strings.Contains(s, `<w:sz w:val="28"/>`) {
		t.Errorf("the new size was not applied to the run's CURRENT rPr: %s", s)
	}
	newRFontsIdx := strings.Index(s, `w:ascii="Georgia"`)
	rprChangeIdx := strings.Index(s, "<w:rPrChange")
	if newRFontsIdx == -1 || rprChangeIdx == -1 || newRFontsIdx > rprChangeIdx {
		t.Errorf("the new rFonts must land in the current rPr, before <w:rPrChange>: %s", s)
	}
}

// TestPlanStylesPatches_IgnoresHistoricalPropertiesInsideRPrChange is the
// same bug on the whole-document docDefaults rPr chain (format.go's
// scanDocDefaults/planStylesPatches).
func TestPlanStylesPatches_IgnoresHistoricalPropertiesInsideRPrChange(t *testing.T) {
	const styles = `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:docDefaults><w:rPrDefault><w:rPr><w:rPrChange w:id="1" w:author="A" w:date="2020-01-01T00:00:00Z">` +
		`<w:rPr><w:rFonts w:ascii="Old"/><w:sz w:val="20"/></w:rPr></w:rPrChange></w:rPr></w:rPrDefault></w:docDefaults>` +
		`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
		`</w:styles>`
	out := applyStylesPatches(t, []byte(styles), FormatOptions{BodyFont: "Georgia", BodySizePt: 14})

	rpd := extractElem(t, out, "<w:rPrDefault>", "</w:rPrDefault>")
	if !strings.Contains(rpd, `w:ascii="Old"`) {
		t.Errorf("historical rFonts inside <w:rPrChange> was corrupted: %s", rpd)
	}
	if !strings.Contains(rpd, `w:val="20"`) {
		t.Errorf("historical sz inside <w:rPrChange> was corrupted: %s", rpd)
	}
	if !strings.Contains(rpd, `w:ascii="Georgia"`) {
		t.Errorf("the new body font was not applied: %s", rpd)
	}
	newRFontsIdx := strings.Index(rpd, `w:ascii="Georgia"`)
	rprChangeIdx := strings.Index(rpd, "<w:rPrChange")
	if newRFontsIdx == -1 || rprChangeIdx == -1 || newRFontsIdx > rprChangeIdx {
		t.Errorf("the new rFonts must land in the current rPr, before <w:rPrChange>: %s", rpd)
	}
}

// TestFormat_HeadingFontInsertedRFontsLandsBeforeExistingBold is
// planHeadingFontPatches's own instance of the same rFonts-must-be-first
// bug (Minor 13's root cause): a heading style's rPr that already carries
// other direct formatting (bold) but no rFonts had a newly inserted rFonts
// land at rPr's END (right before </w:rPr>) instead of its first child
// slot (EG_RPrBase order).
func TestFormat_HeadingFontInsertedRFontsLandsBeforeExistingBold(t *testing.T) {
	const styles = `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/><w:rPr><w:b/></w:rPr></w:style>` +
		`</w:styles>`
	patches, err := planHeadingFontPatches([]byte(styles), "Georgia")
	if err != nil {
		t.Fatal(err)
	}
	out, err := Apply([]byte(styles), patches)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if err := checkWellFormed(out); err != nil {
		t.Fatalf("result is not well-formed XML: %v", err)
	}
	rpr := extractElem(t, s, "<w:rPr>", "</w:rPr>")
	if !strings.Contains(rpr, "<w:b/>") {
		t.Errorf("existing bold was wiped: %s", rpr)
	}
	rfontsIdx := strings.Index(rpr, "<w:rFonts")
	bIdx := strings.Index(rpr, "<w:b/>")
	if rfontsIdx == -1 || bIdx == -1 || rfontsIdx > bIdx {
		t.Errorf("<w:rFonts> must precede the pre-existing <w:b/> (EG_RPrBase schema order): %s", rpr)
	}
}
