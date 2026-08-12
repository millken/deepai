package docx

import (
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"testing"
)

// --- Task 8, §3a: rPr's own anchor table (rPrChildOrder), the rPr-side
// counterpart of Task 1's pPrChildOrder. A missing <w:sz>/<w:szCs> must
// land before whichever LATER EG_RPrBase sibling already exists -- most
// importantly a trailing <w:rPrChange>, which CT_RPr requires be the LAST
// child, so blindly appending at the container's end (the pre-task-8
// behavior) produced well-formed but schema-illegal XML. ---

// assertRPrChildOrder is assertPPrChildOrder's rPr-side twin: fails the
// test if any two of rPrXML's DIRECT children appear out of rPrChildOrder's
// schema sequence.
func assertRPrChildOrder(t *testing.T, rPrXML string) {
	t.Helper()
	rank := make(map[string]int, len(rPrChildOrder))
	for i, n := range rPrChildOrder {
		rank[n] = i
	}
	dec := xml.NewDecoder(strings.NewReader(rPrXML))
	depth := 0
	lastRank := -1
	lastName := ""
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("rPr fixture is not well-formed XML: %v\n%s", err, rPrXML)
		}
		switch se := tok.(type) {
		case xml.StartElement:
			if depth == 1 {
				if r, ok := rank[se.Name.Local]; ok {
					if r < lastRank {
						t.Errorf("rPr child order violated (CT_RPr schema): <w:%s> (rank %d) appears after <w:%s> (rank %d):\n%s",
							se.Name.Local, r, lastName, lastRank, rPrXML)
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

// extractOuterElem is extractElem's depth-tracking twin: it returns the
// FIRST occurrence of localName's own start tag through ITS OWN matching
// close, tolerating a nested element of the SAME name one or more levels
// down (e.g. <w:rPrChange>'s historical <w:rPr>) -- extractElem's naive
// substring search for closeTag would instead stop at the nested element's
// close, producing a truncated, malformed fragment.
func extractOuterElem(t *testing.T, s, localName string) string {
	t.Helper()
	start := strings.Index(s, "<w:"+localName+">")
	if start == -1 {
		t.Fatalf("no <w:%s> found in: %s", localName, s)
	}
	dec := xml.NewDecoder(strings.NewReader(s[start:]))
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("failed to find matching close for <w:%s>: %v\n%s", localName, err, s)
		}
		switch se := tok.(type) {
		case xml.StartElement:
			if se.Name.Local == localName {
				depth++
			}
		case xml.EndElement:
			if se.Name.Local == localName {
				depth--
				if depth == 0 {
					return s[start : start+int(dec.InputOffset())]
				}
			}
		}
	}
}

// TestDirect_RunFormat_SzLandsBeforeRPrChangeNotAfter is the red/green test
// for §3a at the paragraph-range (format_direct.go) level: a run's rPr
// carries <w:u/> then a trailing <w:rPrChange> (a revision's historical
// properties) but no current <w:sz>/<w:szCs>. Before the fix, sz/szCs were
// inserted via a bare 2-element ops list with no anchor for "u"/"rPrChange"
// at all, so applyLeafOps' fallback (insert at containerCloseStart, i.e.
// the very end of rPr) landed them AFTER rPrChange -- illegal, since
// CT_RPr requires rPrChange be the LAST child.
func TestDirect_RunFormat_SzLandsBeforeRPrChangeNotAfter(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:rPr><w:u w:val="single"/>` +
		`<w:rPrChange w:id="1" w:author="A" w:date="2024-01-01T00:00:00Z">` +
		`<w:rPr><w:rFonts w:ascii="OldFont"/><w:sz w:val="20"/></w:rPr>` +
		`</w:rPrChange></w:rPr><w:t>x</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, n, err := applyDirectRunFormat(doc, paras, 1, 1, "", "", 14)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("changed %d paragraphs, want 1", n)
	}
	s := string(got)

	rpr := extractOuterElem(t, s, "rPr")
	assertRPrChildOrder(t, rpr)
	if !strings.Contains(rpr, `<w:sz w:val="28"/>`) {
		t.Errorf("new sz was not inserted into the current rPr: %s", rpr)
	}
	if strings.Index(rpr, `<w:sz w:val="28"/>`) > strings.Index(rpr, "<w:rPrChange") {
		t.Errorf("new sz landed AFTER rPrChange (illegal -- rPrChange must be last): %s", rpr)
	}
	// EG_RPrBase orders sz/szCs BEFORE u (color, spacing, w, kern,
	// position, sz, szCs, highlight, u, ...), so the new sz/szCs landing
	// immediately before the pre-existing <w:u/> is the SCHEMA-CORRECT
	// position, not a bug — assertRPrChildOrder above is what actually
	// pins the full order; this is just a human-readable sanity check on
	// top of it.
	if !strings.Contains(rpr, `<w:sz w:val="28"/><w:szCs w:val="28"/><w:u `) {
		t.Errorf("new sz/szCs did not land immediately before the existing <w:u/>: %s", rpr)
	}
	// The historical sz/rFonts inside rPrChange must survive untouched.
	if !strings.Contains(rpr, `<w:rFonts w:ascii="OldFont"/>`) || !strings.Contains(rpr, `<w:sz w:val="20"/>`) {
		t.Errorf("historical properties inside rPrChange were corrupted: %s", rpr)
	}
	if err := checkWellFormed(got); err != nil {
		t.Errorf("result is not well-formed XML: %v", err)
	}
}

// TestPlanStylesPatches_SzLandsBeforeRPrChangeNotAfter is the same §3a
// fix's docDefaults-path instance (planStylesPatches -> scanDocDefaults ->
// planRPrFontSizePatches), the whole-document mirror of the test above.
func TestPlanStylesPatches_SzLandsBeforeRPrChangeNotAfter(t *testing.T) {
	styles := stylesRPrDefaultOnly[:strings.Index(stylesRPrDefaultOnly, "<w:rPr>")] +
		`<w:rPr><w:u w:val="single"/>` +
		`<w:rPrChange w:id="1" w:author="A" w:date="2024-01-01T00:00:00Z">` +
		`<w:rPr><w:sz w:val="20"/></w:rPr></w:rPrChange></w:rPr>` +
		stylesRPrDefaultOnly[strings.Index(stylesRPrDefaultOnly, "</w:rPrDefault>"):]

	out := applyStylesPatches(t, []byte(styles), FormatOptions{BodySizePt: 14})
	rpr := extractOuterElem(t, out, "rPr")
	assertRPrChildOrder(t, rpr)
	if !strings.Contains(rpr, `<w:sz w:val="28"/>`) {
		t.Errorf("new sz was not inserted: %s", rpr)
	}
	if strings.Index(rpr, `<w:sz w:val="28"/>`) > strings.Index(rpr, "<w:rPrChange") {
		t.Errorf("new sz landed AFTER rPrChange (illegal): %s", rpr)
	}
	if strings.Contains(out, `<w:sz w:val="28"/>`) && strings.Contains(out, `<w:sz w:val="20"/>`) == false {
		t.Errorf("historical sz inside rPrChange was overwritten instead of left alone: %s", out)
	}
}

// --- Task 8, §2: first_line_indent_chars ---

// TestDirect_ParagraphFormat_FirstLineIndentChars covers the direct-format
// (paragraph-range) path: 2 characters -> w:firstLineChars="200" plus the
// w:firstLine twips fallback, landing on <w:ind> in CT_PPr schema order
// (between spacing and jc).
func TestDirect_ParagraphFormat_FirstLineIndentChars(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:t>x</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, n, err := applyDirectParaFormat(doc, paras, 1, 1, pParaRequest{FirstLineIndentChars: 2})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("changed %d paragraphs, want 1", n)
	}
	s := string(got)
	if !strings.Contains(s, `w:firstLineChars="200"`) {
		t.Errorf("firstLineChars not inserted: %s", s)
	}
	if !strings.Contains(s, `w:firstLine="420"`) {
		t.Errorf("firstLine twips fallback not inserted: %s", s)
	}
	ppr := extractElem(t, s, "<w:pPr>", "</w:pPr>")
	assertPPrChildOrder(t, ppr)
}

// TestDirect_ParagraphFormat_FirstLineIndentCharsMergesWithExistingInd
// covers the merge case: an existing <w:ind w:left="720"/> (e.g. a list
// item's indent) must survive, with firstLineChars/firstLine added
// alongside it rather than replacing the element.
func TestDirect_ParagraphFormat_FirstLineIndentCharsMergesWithExistingInd(t *testing.T) {
	doc := []byte(`<w:p><w:pPr><w:ind w:left="720"/></w:pPr><w:r><w:t>x</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, n, err := applyDirectParaFormat(doc, paras, 1, 1, pParaRequest{FirstLineIndentChars: 2})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("changed %d paragraphs, want 1", n)
	}
	s := string(got)
	if strings.Count(s, "<w:ind ") != 1 {
		t.Errorf("w:ind was duplicated instead of merged: %s", s)
	}
	if !strings.Contains(s, `w:left="720"`) {
		t.Errorf("existing left indent was wiped: %s", s)
	}
	if !strings.Contains(s, `w:firstLineChars="200"`) {
		t.Errorf("firstLineChars not merged in: %s", s)
	}
}

// TestFormat_FirstLineIndentCharsSynthesizesDocDefaults covers the
// whole-document path when docDefaults' pPr chain does not exist yet at
// all -- the synthesis branch (buildPPrOps(nil, req)/renderActiveLeaves).
func TestFormat_FirstLineIndentCharsSynthesizesDocDefaults(t *testing.T) {
	out := applyStylesPatches(t, []byte(stylesNoDocDefaults), FormatOptions{FirstLineIndentChars: 2})
	if !strings.Contains(out, `w:firstLineChars="200"`) || !strings.Contains(out, `w:firstLine="420"`) {
		t.Errorf("synthesized docDefaults lacks the first-line indent: %s", out)
	}
}

// --- Task 8, §2: space_before_pt / space_after_pt, combined with
// line_spacing on the SAME <w:spacing> element ---

// TestDirect_ParagraphFormat_SpaceBeforeAfterCombineWithLineSpacing pins
// that all three land as ONE <w:spacing> tag, not two/three competing
// patches on the same span (which Apply would reject as overlapping).
func TestDirect_ParagraphFormat_SpaceBeforeAfterCombineWithLineSpacing(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:t>x</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, n, err := applyDirectParaFormat(doc, paras, 1, 1, pParaRequest{
		LineSpacing: 1.5, SpaceBeforePt: 6, SpaceAfterPt: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("changed %d paragraphs, want 1", n)
	}
	s := string(got)
	if strings.Count(s, "<w:spacing ") != 1 {
		t.Fatalf("w:spacing appears %d times, want exactly 1: %s", strings.Count(s, "<w:spacing "), s)
	}
	if !strings.Contains(s, `w:before="120"`) || !strings.Contains(s, `w:after="240"`) ||
		!strings.Contains(s, `w:line="360"`) || !strings.Contains(s, `w:lineRule="auto"`) {
		t.Errorf("spacing tag lacks before=120/after=240/line=360/lineRule=auto: %s", s)
	}
}

// TestDirect_ParagraphFormat_SpaceBeforeMergesIntoExistingSpacing covers
// the merge case: an existing <w:spacing w:line="240" .../> must keep its
// line value untouched when only space_before_pt is requested.
func TestDirect_ParagraphFormat_SpaceBeforeMergesIntoExistingSpacing(t *testing.T) {
	doc := []byte(`<w:p><w:pPr><w:spacing w:line="240" w:lineRule="auto"/></w:pPr><w:r><w:t>x</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, n, err := applyDirectParaFormat(doc, paras, 1, 1, pParaRequest{SpaceBeforePt: 6})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("changed %d paragraphs, want 1", n)
	}
	s := string(got)
	if strings.Count(s, "<w:spacing ") != 1 {
		t.Errorf("w:spacing was duplicated: %s", s)
	}
	if !strings.Contains(s, `w:line="240"`) {
		t.Errorf("existing line spacing was wiped: %s", s)
	}
	if !strings.Contains(s, `w:before="120"`) {
		t.Errorf("space before not merged in: %s", s)
	}
}

// --- Task 8, §2: line_spacing_exact_pt ---

// TestDirect_ParagraphFormat_LineSpacingExactPt covers 24pt -> w:line="480"
// w:lineRule="exact" (not "auto").
func TestDirect_ParagraphFormat_LineSpacingExactPt(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:t>x</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, n, err := applyDirectParaFormat(doc, paras, 1, 1, pParaRequest{LineSpacingExactPt: 24})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("changed %d paragraphs, want 1", n)
	}
	s := string(got)
	if !strings.Contains(s, `w:line="480"`) || !strings.Contains(s, `w:lineRule="exact"`) {
		t.Errorf("exact line spacing not applied: %s", s)
	}
}

// --- Task 8, §2: body_east_asia_font, direct-format (paragraph-range) path ---

// TestDirect_RunFormat_BodyEastAsiaFontAloneLeavesLatinFontsUntouched covers
// body_east_asia_font with NO body_font in the same call: only w:eastAsia is
// set/replaced, ascii/hAnsi/cs survive untouched.
func TestDirect_RunFormat_BodyEastAsiaFontAloneLeavesLatinFontsUntouched(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:rPr><w:rFonts w:ascii="Times New Roman" w:hAnsi="Times New Roman" w:eastAsia="OldCJK"/></w:rPr><w:t>x</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, n, err := applyDirectRunFormat(doc, paras, 1, 1, "", "SimSun", 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("changed %d paragraphs, want 1", n)
	}
	s := string(got)
	if !strings.Contains(s, `w:ascii="Times New Roman"`) || !strings.Contains(s, `w:hAnsi="Times New Roman"`) {
		t.Errorf("body_east_asia_font alone disturbed the existing Latin font: %s", s)
	}
	if !strings.Contains(s, `w:eastAsia="SimSun"`) {
		t.Errorf("east asia font not applied: %s", s)
	}
	if strings.Contains(s, "OldCJK") {
		t.Errorf("old east asia font survived alongside the new one: %s", s)
	}
}

// TestDirect_RunFormat_BodyFontAndBodyEastAsiaFontTogether covers both given
// in the SAME call: ascii/hAnsi = body_font, eastAsia = body_east_asia_font
// — both via rFontsLatinAndEastAsiaAttrs, cs is untouched by either (review
// F3: the range path now matches the whole-document path's orthogonality
// exactly, instead of the pre-fix literal-all-four behavior).
func TestDirect_RunFormat_BodyFontAndBodyEastAsiaFontTogether(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:t>x</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, n, err := applyDirectRunFormat(doc, paras, 1, 1, "Georgia", "SimSun", 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("changed %d paragraphs, want 1", n)
	}
	s := string(got)
	if !strings.Contains(s, `w:ascii="Georgia"`) || !strings.Contains(s, `w:hAnsi="Georgia"`) {
		t.Errorf("Latin font not set to body_font: %s", s)
	}
	if !strings.Contains(s, `w:eastAsia="SimSun"`) {
		t.Errorf("east asia font not set to body_east_asia_font: %s", s)
	}
	if strings.Contains(s, `w:cs=`) {
		t.Errorf("cs must be untouched by body_font/body_east_asia_font (out of scope for both): %s", s)
	}
}

// TestDirect_RunFormat_BodyFontAloneLeavesEastAsiaAndCsUntouched is review
// F3's own red test: body_font alone on a range used to write ALL FOUR of
// ascii/hAnsi/eastAsia/cs to the same literal value, silently clobbering an
// existing CJK font — exactly the Important-8-shaped bug the whole-document
// path was already fixed for (Task 7), which this schema's own doc comment
// ("Orthogonal to body_east_asia_font — an existing CJK font survives
// untouched unless body_east_asia_font is also given") promises
// unconditionally, not just for the whole-document path. body_font alone
// now only ever touches ascii/hAnsi, on BOTH paths.
func TestDirect_RunFormat_BodyFontAloneLeavesEastAsiaAndCsUntouched(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:rPr><w:rFonts w:eastAsia="宋体" w:cs="Times New Roman"/></w:rPr><w:t>x</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, n, err := applyDirectRunFormat(doc, paras, 1, 1, "Georgia", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("changed %d paragraphs, want 1", n)
	}
	s := string(got)
	if !strings.Contains(s, `w:ascii="Georgia"`) || !strings.Contains(s, `w:hAnsi="Georgia"`) {
		t.Errorf("Latin font not applied: %s", s)
	}
	if !strings.Contains(s, `w:eastAsia="宋体"`) {
		t.Errorf("body_font alone clobbered the existing east-asia font (review F3): %s", s)
	}
	if !strings.Contains(s, `w:cs="Times New Roman"`) {
		t.Errorf("body_font alone clobbered the existing cs font: %s", s)
	}
}

// --- Task 8, §2: style-chain masking notes for the new fields ---

// TestPlanStyleChainShadowPatches_NewFieldsNotedWhenShadowed covers
// space_before_pt/space_after_pt/first_line_indent_chars/body_east_asia_font's
// own masking-note behavior, mirroring the pre-existing line-spacing/
// alignment pattern: a heading-like style that already carries its own
// explicit before/after/firstLine/eastAsia and IS used by a paragraph gets
// named in notes, never silently rewritten.
func TestPlanStyleChainShadowPatches_NewFieldsNotedWhenShadowed(t *testing.T) {
	styles := []byte(`<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		styleChainTestNormal +
		`<w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/><w:basedOn w:val="Normal"/>` +
		`<w:pPr><w:spacing w:before="240" w:after="60"/><w:ind w:firstLine="480" w:firstLineChars="240"/></w:pPr>` +
		`<w:rPr><w:rFonts w:ascii="Cambria" w:eastAsia="SimHei"/></w:rPr></w:style>` +
		`</w:styles>`)

	opts := FormatOptions{SpaceBeforePt: 6, SpaceAfterPt: 12, FirstLineIndentChars: 2, BodyEastAsiaFont: "SimSun"}
	patches, changed, notes, err := planStyleChainShadowPatches(styles, opts, map[string]bool{"Heading1": true}, nil)
	if err != nil {
		t.Fatalf("planStyleChainShadowPatches: %v", err)
	}
	if len(patches) != 0 {
		t.Errorf("Heading1 must never be rewritten by these rules; got %d patches", len(patches))
	}
	if changed["space before"] || changed["space after"] || changed["first line indent"] || changed["east asia font"] {
		t.Errorf("changed = %v, want none of the new fields flagged (Heading1 is excluded from rewrite)", changed)
	}
	joined := strings.Join(notes, " | ")
	for _, want := range []string{"space before", "space after", "first line indent", "east asia font"} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes = %v, want a note mentioning %q", notes, want)
		}
	}
}

// TestPlanStyleChainShadowPatches_NewFieldsRewrittenOnEligibleStyle is the
// positive counterpart: a Normal-based BODY style (not heading-like) that
// already shadows these fields gets rewritten in place, combined into ONE
// patch for the shared <w:spacing> element.
func TestPlanStyleChainShadowPatches_NewFieldsRewrittenOnEligibleStyle(t *testing.T) {
	styles := []byte(`<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		styleChainTestNormal +
		`<w:style w:type="paragraph" w:styleId="BodyText"><w:name w:val="Body Text"/><w:basedOn w:val="Normal"/>` +
		`<w:pPr><w:spacing w:before="240" w:after="60"/><w:ind w:firstLine="480" w:firstLineChars="240"/></w:pPr>` +
		`<w:rPr><w:rFonts w:ascii="Calibri" w:eastAsia="SimHei"/></w:rPr></w:style>` +
		`</w:styles>`)

	opts := FormatOptions{SpaceBeforePt: 6, SpaceAfterPt: 12, FirstLineIndentChars: 2, BodyEastAsiaFont: "SimSun"}
	patches, changed, notes, err := planStyleChainShadowPatches(styles, opts, nil, nil)
	if err != nil {
		t.Fatalf("planStyleChainShadowPatches: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("notes = %v, want none: BodyText is eligible for rewrite", notes)
	}
	if !changed["space before"] || !changed["space after"] || !changed["first line indent"] || !changed["east asia font"] {
		t.Errorf("changed = %v, want all four new fields flagged", changed)
	}
	// Exactly 2 patches: one combined <w:spacing> (before+after), one <w:ind>.
	// rFonts is a separate element from spacing/ind, so 3 total (rFonts,
	// spacing, ind).
	if len(patches) != 3 {
		t.Fatalf("patches = %d, want 3 (rFonts, spacing, ind); got %+v", len(patches), patches)
	}

	out, err := Apply(styles, patches)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	s := string(out)
	bodyText := s[strings.Index(s, `w:styleId="BodyText"`):]
	bodyText = bodyText[:strings.Index(bodyText, "</w:style>")]
	if !strings.Contains(bodyText, `w:before="120"`) || !strings.Contains(bodyText, `w:after="240"`) {
		t.Errorf("BodyText's own spacing was not rewritten: %s", bodyText)
	}
	if !strings.Contains(bodyText, `w:firstLineChars="200"`) {
		t.Errorf("BodyText's own ind was not rewritten: %s", bodyText)
	}
	if !strings.Contains(bodyText, `w:eastAsia="SimSun"`) {
		t.Errorf("BodyText's own eastAsia font was not rewritten: %s", bodyText)
	}
	if !strings.Contains(bodyText, `w:ascii="Calibri"`) {
		t.Errorf("BodyText's own Latin font must survive untouched (body_east_asia_font is orthogonal): %s", bodyText)
	}
}

// --- Task 8, §2: direct-format masking notes for the new fields ---

// TestDirectFormatMaskingNotes_NewFieldsReported mirrors the pre-existing
// font/size/line-spacing/alignment masking-note tests for the four new
// fields: a paragraph's own direct pPr/rPr formatting must be reported by
// name, not silently overridden-and-unmentioned.
func TestDirectFormatMaskingNotes_NewFieldsReported(t *testing.T) {
	doc := []byte(
		`<w:p><w:pPr><w:spacing w:before="240" w:after="60"/><w:ind w:firstLine="480" w:firstLineChars="240"/></w:pPr>` +
			`<w:r><w:rPr><w:rFonts w:eastAsia="SimHei"/></w:rPr><w:t>x</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	notes, err := directFormatMaskingNotes(doc, paras, FormatOptions{
		SpaceBeforePt: 6, SpaceAfterPt: 12, FirstLineIndentChars: 2, BodyEastAsiaFont: "SimSun",
	})
	if err != nil {
		t.Fatalf("directFormatMaskingNotes: %v", err)
	}
	joined := strings.Join(notes, " | ")
	for _, want := range []string{"space before", "space after", "first-line indent", "east asia font"} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes = %v, want a note mentioning %q", notes, want)
		}
	}
}

// --- Coordinator review round (Needs fixes): F1-F6 ---

// TestPlanStyleChainShadowPatches_UnchangedFieldSharingSpacingNotMisreported
// is review F1's red test: Normal's own <w:spacing w:line="360"
// w:before="0"/> already shadows BOTH line spacing (360 = 1.5 * 240,
// exactly what's requested — genuinely unchanged) and space-before (0,
// which the request WOULD change to 120). The pre-fix code gated
// lineChanges/beforeChanges on "requested AND the style has the attribute
// at all", then let a single whole-tag tagUnchanged decide whether to
// record ANY of them — since the combined tag differs (because before
// really changes), it credited BOTH "line spacing" and "space before" as
// changed, even though line never moved. Each field must now be judged on
// its OWN attribute value, independently of its sibling on the same tag.
func TestPlanStyleChainShadowPatches_UnchangedFieldSharingSpacingNotMisreported(t *testing.T) {
	styles := []byte(`<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/>` +
		`<w:pPr><w:spacing w:line="360" w:lineRule="auto" w:before="0"/></w:pPr></w:style>` +
		`</w:styles>`)

	opts := FormatOptions{LineSpacing: 1.5, SpaceBeforePt: 6}
	patches, changed, notes, err := planStyleChainShadowPatches(styles, opts, nil, nil)
	if err != nil {
		t.Fatalf("planStyleChainShadowPatches: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("notes = %v, want none: Normal is eligible for rewrite", notes)
	}
	if changed["line spacing"] {
		t.Errorf("changed = %v, want \"line spacing\" NOT flagged: 1.5 * 240 = 360, identical to what Normal already had", changed)
	}
	if !changed["space before"] {
		t.Errorf("changed = %v, want \"space before\" flagged: 0 -> 120 is a real change", changed)
	}
	if len(patches) != 1 {
		t.Fatalf("patches = %d, want exactly 1 (one combined <w:spacing> patch); got %+v", len(patches), patches)
	}
	out, err := Apply(styles, patches)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `w:line="360"`) {
		t.Errorf("line spacing value must be preserved at 360: %s", s)
	}
	if !strings.Contains(s, `w:before="120"`) {
		t.Errorf("space before must be updated to 120: %s", s)
	}
}

// TestDirect_ParagraphFormat_FirstLineIndentCharsDropsHanging is review F2's
// red test: CT_Ind semantics have w:hanging win over w:firstLine when both
// are present (Word ignores firstLine entirely in that case) — a
// pre-existing <w:ind w:hanging="360"/> (e.g. a numbered-list-shaped
// paragraph) must have its hanging/hangingChars dropped whenever
// first_line_indent_chars writes a real firstLine, or the new indent would
// silently do nothing.
func TestDirect_ParagraphFormat_FirstLineIndentCharsDropsHanging(t *testing.T) {
	doc := []byte(`<w:p><w:pPr><w:ind w:hanging="360" w:hangingChars="200"/></w:pPr><w:r><w:t>x</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, n, err := applyDirectParaFormat(doc, paras, 1, 1, pParaRequest{FirstLineIndentChars: 2})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("changed %d paragraphs, want 1", n)
	}
	s := string(got)
	if strings.Contains(s, "hanging") {
		t.Errorf("w:hanging/w:hangingChars must be dropped when firstLine is written (CT_Ind: hanging wins over firstLine): %s", s)
	}
	if !strings.Contains(s, `w:firstLineChars="200"`) || !strings.Contains(s, `w:firstLine="420"`) {
		t.Errorf("firstLine/firstLineChars not applied: %s", s)
	}
}

// TestPlanStyleChainShadowPatches_FirstLineIndentDropsHanging is F2's
// style-chain-path instance of the same fix.
func TestPlanStyleChainShadowPatches_FirstLineIndentDropsHanging(t *testing.T) {
	styles := []byte(`<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		styleChainTestNormal +
		`<w:style w:type="paragraph" w:styleId="BodyText"><w:name w:val="Body Text"/><w:basedOn w:val="Normal"/>` +
		`<w:pPr><w:ind w:hanging="360" w:firstLine="0"/></w:pPr></w:style>` +
		`</w:styles>`)
	opts := FormatOptions{FirstLineIndentChars: 2}
	patches, changed, _, err := planStyleChainShadowPatches(styles, opts, nil, nil)
	if err != nil {
		t.Fatalf("planStyleChainShadowPatches: %v", err)
	}
	if !changed["first line indent"] {
		t.Fatalf("changed = %v, want first line indent flagged", changed)
	}
	out, err := Apply(styles, patches)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "hanging") {
		t.Errorf("w:hanging must be dropped: %s", s)
	}
	if !strings.Contains(s, `w:firstLineChars="200"`) {
		t.Errorf("firstLineChars not applied: %s", s)
	}
}

// TestDirect_ParagraphFormat_SpaceBeforeDropsBeforeAutospacing is review
// F2's "顺带" fix: w:beforeAutospacing/w:afterAutospacing tell Word to
// IGNORE any explicit w:before/w:after and auto-compute it instead — the
// same "one wins, the other is silently ignored" relationship as hanging/
// firstLine. Writing a real w:before/w:after must clear the corresponding
// autospacing flag.
func TestDirect_ParagraphFormat_SpaceBeforeDropsBeforeAutospacing(t *testing.T) {
	doc := []byte(`<w:p><w:pPr><w:spacing w:beforeAutospacing="1" w:afterAutospacing="1"/></w:pPr><w:r><w:t>x</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, n, err := applyDirectParaFormat(doc, paras, 1, 1, pParaRequest{SpaceBeforePt: 6, SpaceAfterPt: 12})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("changed %d paragraphs, want 1", n)
	}
	s := string(got)
	if strings.Contains(s, "Autospacing") {
		t.Errorf("w:beforeAutospacing/w:afterAutospacing must be dropped when before/after are written: %s", s)
	}
	if !strings.Contains(s, `w:before="120"`) || !strings.Contains(s, `w:after="240"`) {
		t.Errorf("before/after not applied: %s", s)
	}
}

// TestDirect_RunFormat_RFontsLandsAfterRStyle is review F4's red test: a
// hyperlink run's rPr commonly carries <w:rStyle w:val="Hyperlink"/> before
// anything else — EG_RPrBase requires rStyle precede rFonts. The pre-fix
// code always anchored a newly-inserted rFonts directly at the container's
// own opening position (bypassing the anchor table entirely), which would
// insert it BEFORE rStyle. Routing rFonts through the SAME rPrChildOrder-
// walked ops list sz/szCs already use fixes this generically.
func TestDirect_RunFormat_RFontsLandsAfterRStyle(t *testing.T) {
	doc := []byte(`<w:p><w:hyperlink r:id="rId1"><w:r><w:rPr><w:rStyle w:val="Hyperlink"/></w:rPr><w:t>link</w:t></w:r></w:hyperlink></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, n, err := applyDirectRunFormat(doc, paras, 1, 1, "Georgia", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("changed %d paragraphs, want 1", n)
	}
	s := string(got)
	rpr := extractOuterElem(t, s, "rPr")
	assertRPrChildOrder(t, rpr)
	if !strings.Contains(rpr, `<w:rStyle w:val="Hyperlink"/>`) {
		t.Errorf("rStyle was lost: %s", rpr)
	}
	if strings.Index(rpr, "<w:rStyle") > strings.Index(rpr, "<w:rFonts") {
		t.Errorf("rFonts landed BEFORE rStyle (EG_RPrBase violation): %s", rpr)
	}
	if !strings.Contains(rpr, `w:ascii="Georgia"`) {
		t.Errorf("font not applied: %s", rpr)
	}
}

// TestPlanStylesPatches_RFontsLandsAfterRStyle is F4's docDefaults-path
// instance of the same fixture/fix.
func TestPlanStylesPatches_RFontsLandsAfterRStyle(t *testing.T) {
	styles := stylesRPrDefaultOnly[:strings.Index(stylesRPrDefaultOnly, "<w:rPr>")] +
		`<w:rPr><w:rStyle w:val="Hyperlink"/></w:rPr>` +
		stylesRPrDefaultOnly[strings.Index(stylesRPrDefaultOnly, "</w:rPrDefault>"):]

	out := applyStylesPatches(t, []byte(styles), FormatOptions{BodyFont: "Georgia"})
	rpr := extractOuterElem(t, out, "rPr")
	assertRPrChildOrder(t, rpr)
	if strings.Index(rpr, "<w:rStyle") > strings.Index(rpr, "<w:rFonts") {
		t.Errorf("rFonts landed BEFORE rStyle (EG_RPrBase violation): %s", rpr)
	}
}

// --- F6: non-negative validation for the four new measurement fields ---

func TestFormat_NegativeMeasurementFieldsRejected(t *testing.T) {
	cases := []struct {
		name string
		opts FormatOptions
	}{
		{"first_line_indent_chars", FormatOptions{FirstLineIndentChars: -1}},
		{"space_before_pt", FormatOptions{SpaceBeforePt: -1}},
		{"space_after_pt", FormatOptions{SpaceAfterPt: -1}},
		{"line_spacing_exact_pt", FormatOptions{LineSpacingExactPt: -1}},
	}
	for _, c := range cases {
		t.Run(c.name+"/whole-document", func(t *testing.T) {
			d := directFixtureDoc(t)
			if _, err := d.Format(c.opts); err == nil {
				t.Fatalf("a negative %s was accepted; want an error", c.name)
			}
		})
		t.Run(c.name+"/range", func(t *testing.T) {
			d := directFixtureDoc(t)
			opts := c.opts
			opts.StartPara = 1
			if _, err := d.Format(opts); err == nil {
				t.Fatalf("a negative %s was accepted with a range; want an error", c.name)
			}
		})
	}
}
