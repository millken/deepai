package docx

import (
	"strings"
	"testing"
)

// TestWriteThenFormat_EveryRuleWorksOnAWrittenDocument is the regression test
// for the bug a user hit in real use: WriteDocx's word/styles.xml carried no
// <w:docDefaults> chain at all, so every Document.Format rule that lands in
// docDefaults (BodyFont, BodySizePt, LineSpacing, Align, and Template, which
// expands into those same fields) errored against a document this package
// had JUST created itself -- "docx_format: docx: styles.xml has no
// <w:docDefaults><w:rPrDefault><w:rPr> chain; cannot set body font/size".
// Each tool was tested only against its own fixtures (WriteDocx's own
// assertions, Format's testdata/structure.docx, a real Word file that
// already has the chain); nothing exercised them together, which is exactly
// how this shipped. This applies every rule Document.Format supports --
// including the paragraph-range direct-formatting forms, which never touch
// styles.xml and were never broken -- to a WriteDocx output and asserts each
// one succeeds.
func TestWriteThenFormat_EveryRuleWorksOnAWrittenDocument(t *testing.T) {
	md := "# Title\n\nSome body text here.\n\n## Section\n\nMore body text.\n"

	cases := []struct {
		name string
		opts FormatOptions
	}{
		{"BodyFont", FormatOptions{BodyFont: "Calibri"}},
		{"BodySizePt", FormatOptions{BodySizePt: 13}},
		{"LineSpacing", FormatOptions{LineSpacing: 1.5}},
		{"Align", FormatOptions{Align: "justify"}},
		{"HeadingFont", FormatOptions{HeadingFont: "Georgia"}},
		{"MarginsMM", FormatOptions{MarginsMM: []float64{25.4, 25.4, 25.4, 25.4}}},
		{"Template", FormatOptions{Template: "corporate"}},
		{"Normalize", FormatOptions{Normalize: true}},
		{"ParagraphRange", FormatOptions{StartPara: 1, EndPara: 2, BodyFont: "Georgia", BodySizePt: 14, LineSpacing: 1.5, Align: "left"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _ := writeAndReopen(t, md)
			if _, err := d.Format(tc.opts); err != nil {
				t.Fatalf("Format(%s) against a docx_write document: %v", tc.name, err)
			}
		})
	}
}

// TestWriteThenFormat_BodyFontSizeLineSpacingAlignLandCorrectly goes past
// "did not error" and checks the actual landing values, plus that the
// result still reopens -- an out-of-schema-order insert (e.g. pPrDefault
// before rPrDefault) is exactly the kind of mistake Word reports as "the
// file is corrupt" with no further diagnostic, so a reopen after the write
// is the only way to be sure the insert was schema-valid.
func TestWriteThenFormat_BodyFontSizeLineSpacingAlignLandCorrectly(t *testing.T) {
	d, _, p := writeAndReopen(t, "# Title\n\nbody\n")
	if _, err := d.Format(FormatOptions{
		BodyFont: "Calibri", BodySizePt: 13, LineSpacing: 1.5, Align: "justify",
	}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	s := stylesXML(t, d)
	dd := s[strings.Index(s, "<w:docDefaults>"):strings.Index(s, "</w:docDefaults>")]

	if !strings.Contains(dd, `w:ascii="Calibri"`) {
		t.Errorf("docDefaults lacks the new body font:\n%s", dd)
	}
	if strings.Contains(dd, "Theme=") {
		t.Errorf("docDefaults still carries theme font attributes, which override a literal one:\n%s", dd)
	}
	if !strings.Contains(dd, `<w:sz w:val="26"/>`) {
		t.Errorf("docDefaults lacks sz=26 (13pt in half-points):\n%s", dd)
	}
	if !strings.Contains(dd, `<w:szCs w:val="26"/>`) {
		t.Errorf("docDefaults lacks szCs=26:\n%s", dd)
	}
	if !strings.Contains(dd, `w:line="360"`) {
		t.Errorf("docDefaults lacks line spacing 360 (1.5x 240ths):\n%s", dd)
	}
	if !strings.Contains(dd, `<w:jc w:val="justify"/>`) {
		t.Errorf("docDefaults lacks alignment justify:\n%s", dd)
	}

	if err := d.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := OpenDocument(p); err != nil {
		t.Fatalf("the formatted document cannot be reopened: %v", err)
	}
}

// TestWriteThenFormat_LineSpacingLandsOnTheEffectiveStyleNotJustDocDefaults
// is the compose-test upgrade the format capability review's Critical 4 and
// the write review's I4 both call for: docx_write's own BodyText style
// carries an explicit <w:spacing w:line="360"/> (bodyTextStyleXML, 1.5x),
// which OUTRANKS docDefaults in Word's cascade. Before this task,
// Document.Format(LineSpacing) only ever rewrote docDefaults, so this
// exact composition — docx_write's own product, fed straight into
// docx_format — silently did nothing: docDefaults changed, applied
// reported success, and every ordinary paragraph (styled BodyText) kept
// rendering at the OLD 360 spacing. This test goes past "did not error"
// (the pre-upgrade version of this file) to the actual effective value:
// BodyText's own w:line must become 480 (2.0 in 240ths), not just
// docDefaults'.
func TestWriteThenFormat_LineSpacingLandsOnTheEffectiveStyleNotJustDocDefaults(t *testing.T) {
	d, _, _ := writeAndReopen(t, "# Title\n\nSome body text here.\n")
	if _, err := d.Format(FormatOptions{LineSpacing: 2.0}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	s := stylesXML(t, d)

	bodyText := s[strings.Index(s, `w:styleId="BodyText"`):]
	bodyText = bodyText[:strings.Index(bodyText, "</w:style>")]
	if !strings.Contains(bodyText, `w:line="480"`) {
		t.Errorf("BodyText's own <w:spacing> was not rewritten to 480 (2.0x in 240ths); it still shadows docDefaults:\n%s", bodyText)
	}

	dd := s[strings.Index(s, "<w:docDefaults>"):strings.Index(s, "</w:docDefaults>")]
	if !strings.Contains(dd, `w:line="480"`) {
		t.Errorf("docDefaults itself was not updated either:\n%s", dd)
	}
}

// TestWriteThenFormat_TemplateCorporatePreservesEastAsiaFont is the compose-
// test upgrade for the write review's I5: docx_write's own docDefaults
// carries <w:rFonts w:ascii="Calibri" w:eastAsia="微软雅黑"/> (see
// docDefaultsXML), and template=corporate sets BodyFont="Calibri" — which,
// before this task, landed via rFontsLiteralAttrs and clobbered ascii AND
// hAnsi AND eastAsia AND cs to the SAME literal value, silently replacing
// every Chinese run's font with Calibri (which has no CJK glyphs at all;
// Word substitutes an arbitrary fallback). BodyFont must only ever touch
// the Latin half (ascii/hAnsi); an existing eastAsia value must survive
// byte for byte.
func TestWriteThenFormat_TemplateCorporatePreservesEastAsiaFont(t *testing.T) {
	d, _, _ := writeAndReopen(t, "# 标题\n\n正文内容。\n")
	if _, err := d.Format(FormatOptions{Template: "corporate"}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	s := stylesXML(t, d)
	dd := s[strings.Index(s, "<w:docDefaults>"):strings.Index(s, "</w:docDefaults>")]

	if !strings.Contains(dd, `w:eastAsia="微软雅黑"`) {
		t.Errorf("docDefaults lost its eastAsia font (Chinese text would fall back to a substitute font):\n%s", dd)
	}
	if !strings.Contains(dd, `w:ascii="Calibri"`) {
		t.Errorf("docDefaults lacks the corporate template's Latin body font:\n%s", dd)
	}
}

// TestWriteThenFormat_BodyFontNeverTouchesSourceCodeOrHeadingFonts pins the
// task 7 brief's own explicit invariant (§1: "SourceCode 的等宽字体、HeadingN
// 的字体绝不被 body_font 触碰"): a document-wide body_font change must never
// rewrite SourceCode's monospace font — a fenced code block would silently
// start rendering in a proportional font — nor a heading style's own font.
// SourceCode and the Heading family are both excluded from
// planStyleChainShadowPatches' effective-chain rewrite specifically so this
// holds even when (unlike this fixture) a heading DOES carry its own
// explicit <w:rFonts> that shadows docDefaults.
func TestWriteThenFormat_BodyFontNeverTouchesSourceCodeOrHeadingFonts(t *testing.T) {
	md := "# Title\n\nSome body text.\n\n```\ncode line\n```\n"
	d, _, _ := writeAndReopen(t, md)
	if _, err := d.Format(FormatOptions{BodyFont: "Georgia"}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	s := stylesXML(t, d)

	sc := s[strings.Index(s, `w:styleId="SourceCode"`):]
	sc = sc[:strings.Index(sc, "</w:style>")]
	if !strings.Contains(sc, `w:ascii="Consolas"`) {
		t.Errorf("SourceCode's own monospace font is gone or was rewritten by body_font:\n%s", sc)
	}
	if strings.Contains(sc, `w:ascii="Georgia"`) {
		t.Errorf("SourceCode picked up the new body font, which must never happen:\n%s", sc)
	}

	h1 := s[strings.Index(s, `w:styleId="Heading1"`):]
	h1 = h1[:strings.Index(h1, "</w:style>")]
	if strings.Contains(h1, `w:ascii="Georgia"`) {
		t.Errorf("Heading1 picked up the new body font, which must never happen without heading_font:\n%s", h1)
	}
}

// TestWriteThenFormat_SecondIdenticalCallReportsNothingChanged is the red
// test for the task 7 follow-up review's F1: Applied must reflect a real,
// byte-different write, not merely "a rule was requested." Before this fix,
// planStylesPatches unconditionally appended an Applied entry for every
// requested field regardless of whether anything on disk actually differed
// — running the exact same FormatOptions twice against the same document
// would report success both times even though the second call's docDefaults
// and every shadowing style already carried the requested value and nothing
// was left to change.
func TestWriteThenFormat_SecondIdenticalCallReportsNothingChanged(t *testing.T) {
	md := "# Title\n\nSome body text here.\n\n## Section\n\nMore body text.\n"
	d, _, _ := writeAndReopen(t, md)
	opts := FormatOptions{BodyFont: "Georgia", BodySizePt: 13, LineSpacing: 1.5, Align: "justify"}

	first, err := d.Format(opts)
	if err != nil {
		t.Fatalf("first Format: %v", err)
	}
	if len(first.Applied) == 0 {
		t.Fatal("first call's Applied is empty; the rule never took effect to begin with")
	}

	second, err := d.Format(opts)
	if err != nil {
		t.Fatalf("second Format: %v", err)
	}
	if len(second.Applied) != 0 {
		t.Errorf("second identical call's Applied = %v, want empty: docDefaults and every shadowing style already carry the requested value", second.Applied)
	}
}

// TestWriteThenFormat_FiveNewFieldsSecondIdenticalCallIsIdempotent pins task
// 8 review's item 8 requirement: running the FIVE new whole-document fields
// (BodyEastAsiaFont, FirstLineIndentChars, SpaceBeforePt, SpaceAfterPt,
// LineSpacingExactPt) together a second time with identical FormatOptions
// must report an empty Applied AND leave word/styles.xml byte-for-byte
// identical to what the first call produced — not just "Applied is empty"
// (which F1's per-attribute fix could satisfy while still silently
// rewriting bytes that already matched), but the actual on-disk bytes
// staying put, the same "second call is a true no-op" guarantee
// TestWriteThenFormat_SecondIdenticalCallReportsNothingChanged already
// pins for the FIRST four fields (BodyFont/BodySizePt/LineSpacing/Align).
func TestWriteThenFormat_FiveNewFieldsSecondIdenticalCallIsIdempotent(t *testing.T) {
	md := "# Title\n\nSome body text here.\n\n## Section\n\nMore body text.\n"
	d, _, _ := writeAndReopen(t, md)
	opts := FormatOptions{
		BodyEastAsiaFont:     "SimSun",
		FirstLineIndentChars: 2,
		SpaceBeforePt:        6,
		SpaceAfterPt:         12,
		LineSpacingExactPt:   24,
	}

	first, err := d.Format(opts)
	if err != nil {
		t.Fatalf("first Format: %v", err)
	}
	if len(first.Applied) == 0 {
		t.Fatal("first call's Applied is empty; none of the five new fields ever took effect")
	}
	stylesAfterFirst := stylesXML(t, d)
	docAfterFirst, _ := d.Part(DocumentPart)
	docAfterFirstStr := string(docAfterFirst)

	second, err := d.Format(opts)
	if err != nil {
		t.Fatalf("second Format: %v", err)
	}
	if len(second.Applied) != 0 {
		t.Errorf("second identical call's Applied = %v, want empty", second.Applied)
	}
	stylesAfterSecond := stylesXML(t, d)
	if stylesAfterFirst != stylesAfterSecond {
		t.Errorf("word/styles.xml changed on the second identical call:\nfirst:  %s\nsecond: %s", stylesAfterFirst, stylesAfterSecond)
	}
	docAfterSecond, _ := d.Part(DocumentPart)
	if docAfterFirstStr != string(docAfterSecond) {
		t.Error("word/document.xml changed on the second identical call (these fields never touch it, but pin it anyway)")
	}
}

// TestWriteThenFormat_TemplateWithLineSpacingExactPtDoesNotFalselyConflict
// is review F5's red test: corporate (which sets LineSpacing: 1.15) plus an
// explicit LineSpacingExactPt used to merge into BOTH fields non-zero
// (mergeFormatOptions filled LineSpacing in from the template because the
// caller's own LineSpacing was still at its zero value), tripping
// validateAlignAndLineSpacingMutex's mutual-exclusion check even though the
// caller only ever explicitly asked for ONE of the two. LineSpacing/
// LineSpacingExactPt are now merged as a pair: explicit wins for the whole
// pair, not per-field.
func TestWriteThenFormat_TemplateWithLineSpacingExactPtDoesNotFalselyConflict(t *testing.T) {
	d, _, _ := writeAndReopen(t, "Some body text.\n")
	res, err := d.Format(FormatOptions{Template: "corporate", LineSpacingExactPt: 20})
	if err != nil {
		t.Fatalf("Format(template=corporate, line_spacing_exact_pt=20): %v", err)
	}
	if len(res.Applied) == 0 {
		t.Fatal("Applied is empty; the exact line spacing never took effect")
	}
	dd := stylesXML(t, d)
	dd = dd[strings.Index(dd, "<w:docDefaults>"):strings.Index(dd, "</w:docDefaults>")]
	if !strings.Contains(dd, `w:line="400"`) || !strings.Contains(dd, `w:lineRule="exact"`) {
		t.Errorf("docDefaults lacks w:line=400/w:lineRule=exact (20pt exact, not corporate's own 1.15 multiple): %s", dd)
	}
	if strings.Contains(dd, `w:lineRule="auto"`) {
		t.Errorf("docDefaults still carries corporate's own auto line spacing; explicit line_spacing_exact_pt must win outright: %s", dd)
	}
}
