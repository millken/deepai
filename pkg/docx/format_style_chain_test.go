package docx

import (
	"strings"
	"testing"
)

const styleChainTestNormal = `<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>`

// TestPlanStyleChainShadowPatches_ExactFamilyMatchNotSubstring is the red
// test for follow-up review F2: an earlier version of isQuoteLikeStyle/
// isCodeLikeStyle matched any style whose name or styleId merely CONTAINED
// "quote"/"code" (case-insensitively), which silently exempted ordinary
// body-type styles named "Unquoted"/"Barcode"/"Encoded" from every
// whole-document rule with neither a rewrite nor a note. All three are
// basedOn Normal, carry no relation to Quote/SourceCode/Header/Footer/
// Caption, and must be rewritten like any other body-type style.
func TestPlanStyleChainShadowPatches_ExactFamilyMatchNotSubstring(t *testing.T) {
	styles := []byte(`<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		styleChainTestNormal +
		`<w:style w:type="paragraph" w:styleId="Unquoted"><w:name w:val="Unquoted Body"/><w:basedOn w:val="Normal"/>` +
		`<w:rPr><w:rFonts w:ascii="Times New Roman"/></w:rPr></w:style>` +
		`<w:style w:type="paragraph" w:styleId="Barcode"><w:name w:val="Barcode Body"/><w:basedOn w:val="Normal"/>` +
		`<w:rPr><w:sz w:val="20"/></w:rPr></w:style>` +
		`<w:style w:type="paragraph" w:styleId="Encoded"><w:name w:val="Encoded Body"/><w:basedOn w:val="Normal"/>` +
		`<w:pPr><w:spacing w:line="240" w:lineRule="auto"/></w:pPr></w:style>` +
		`</w:styles>`)

	opts := FormatOptions{BodyFont: "Georgia", BodySizePt: 13, LineSpacing: 1.5}
	patches, changed, notes, err := planStyleChainShadowPatches(styles, opts, nil, nil)
	if err != nil {
		t.Fatalf("planStyleChainShadowPatches: %v", err)
	}
	if len(patches) != 3 {
		t.Fatalf("patches = %d, want 3 (one leaf each for Unquoted/Barcode/Encoded); got %+v", len(patches), patches)
	}
	if !changed["body font"] || !changed["body size"] || !changed["line spacing"] {
		t.Errorf("changed = %v, want all three fields flagged", changed)
	}
	if len(notes) != 0 {
		t.Errorf("notes = %v, want none: Unquoted/Barcode/Encoded merely contain \"quote\"/\"code\" as a name substring, they are not the Quote/SourceCode family", notes)
	}

	out, err := Apply(styles, patches)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `w:styleId="Unquoted"`) || !strings.Contains(extractElem(t, s[strings.Index(s, `w:styleId="Unquoted"`):], "<w:rPr>", "</w:rPr>"), `w:ascii="Georgia"`) {
		t.Errorf("Unquoted's own rFonts was not rewritten to Georgia:\n%s", s)
	}
}

// TestPlanStyleChainShadowPatches_QuoteAndSourceCodeStillExcluded is the
// regression guard alongside F2: switching quote/code detection from a
// substring match to an exact-match set must not stop matching this
// package's OWN Quote/SourceCode styleIds, nor Word's built-in "Intense
// Quote".
func TestPlanStyleChainShadowPatches_QuoteAndSourceCodeStillExcluded(t *testing.T) {
	styles := []byte(`<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		styleChainTestNormal +
		`<w:style w:type="paragraph" w:styleId="Quote"><w:name w:val="Quote"/><w:basedOn w:val="Normal"/>` +
		`<w:rPr><w:rFonts w:ascii="Times New Roman"/></w:rPr></w:style>` +
		`<w:style w:type="paragraph" w:styleId="IntenseQuote"><w:name w:val="Intense Quote"/><w:basedOn w:val="Normal"/>` +
		`<w:rPr><w:rFonts w:ascii="Times New Roman"/></w:rPr></w:style>` +
		`<w:style w:type="paragraph" w:styleId="SourceCode"><w:name w:val="Source Code"/><w:basedOn w:val="Normal"/>` +
		`<w:rPr><w:rFonts w:ascii="Consolas"/></w:rPr></w:style>` +
		`</w:styles>`)

	opts := FormatOptions{BodyFont: "Georgia"}
	patches, changed, notes, err := planStyleChainShadowPatches(styles, opts, map[string]bool{"Quote": true, "IntenseQuote": true, "SourceCode": true}, nil)
	if err != nil {
		t.Fatalf("planStyleChainShadowPatches: %v", err)
	}
	if len(patches) != 0 || changed["body font"] {
		t.Errorf("Quote/IntenseQuote/SourceCode must never be rewritten by body_font; got %d patches, changed=%v", len(patches), changed)
	}
	if len(notes) != 0 {
		t.Errorf("notes = %v, want none: Quote/SourceCode's shadowing is deliberate and silent, per this task's own invariant", notes)
	}
}

// TestPlanStyleChainShadowPatches_RootLevelStyleNotedOnlyWhenUsed is the red
// test for PROBE-H from the follow-up review (F3/F4/F5): a root-level style
// with NO basedOn at all ("Corp Body") is not part of the Normal cascade at
// all, so it is correctly never rewritten — but the previous version's
// eligibility-gated loop skipped it ENTIRELY, silently, even though it
// carries an explicit font that will keep shadowing body_font forever. It
// must now be named in notes, but ONLY when a real paragraph in the
// document actually uses it (F5) — otherwise every document defining this
// style in its Word-template boilerplate would get constant noise.
func TestPlanStyleChainShadowPatches_RootLevelStyleNotedOnlyWhenUsed(t *testing.T) {
	styles := []byte(`<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		styleChainTestNormal +
		`<w:style w:type="paragraph" w:styleId="CorpBody"><w:name w:val="Corp Body"/>` +
		`<w:rPr><w:rFonts w:ascii="Arial"/></w:rPr></w:style>` +
		`</w:styles>`)
	opts := FormatOptions{BodyFont: "Georgia"}

	patches, changed, notes, err := planStyleChainShadowPatches(styles, opts, map[string]bool{}, nil)
	if err != nil {
		t.Fatalf("planStyleChainShadowPatches (unused): %v", err)
	}
	if len(patches) != 0 || changed["body font"] {
		t.Errorf("a root-level style with no basedOn chain to Normal must never be rewritten")
	}
	if len(notes) != 0 {
		t.Errorf("notes = %v, want none: nothing in the document references Corp Body (F5)", notes)
	}

	patches, changed, notes, err = planStyleChainShadowPatches(styles, opts, map[string]bool{"CorpBody": true}, nil)
	if err != nil {
		t.Fatalf("planStyleChainShadowPatches (used): %v", err)
	}
	if len(patches) != 0 || changed["body font"] {
		t.Errorf("a root-level style with no basedOn chain to Normal must never be rewritten, even when used")
	}
	if len(notes) == 0 || !strings.Contains(notes[0], "Corp Body") {
		t.Errorf("notes = %v, want it to name \"Corp Body\" (PROBE-H)", notes)
	}
}

// TestPlanStyleChainShadowPatches_UnreachableBuiltinNotedWhenUsed is the red
// test for PROBE-N: Word's own NoSpacing/MacroText styles carry no basedOn
// at all (verified against testdata/structure.docx), so they were also
// silently skipped by the previous version's loop. Modeled directly on that
// real fixture's NoSpacing style.
func TestPlanStyleChainShadowPatches_UnreachableBuiltinNotedWhenUsed(t *testing.T) {
	styles := []byte(`<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		styleChainTestNormal +
		`<w:style w:type="paragraph" w:styleId="NoSpacing"><w:name w:val="No Spacing"/>` +
		`<w:pPr><w:spacing w:line="240" w:lineRule="auto"/></w:pPr></w:style>` +
		`</w:styles>`)
	opts := FormatOptions{LineSpacing: 2.0}

	_, _, notes, err := planStyleChainShadowPatches(styles, opts, map[string]bool{}, nil)
	if err != nil {
		t.Fatalf("planStyleChainShadowPatches (unused): %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("notes = %v, want none: no paragraph in the document uses No Spacing (F5)", notes)
	}

	_, _, notes, err = planStyleChainShadowPatches(styles, opts, map[string]bool{"NoSpacing": true}, nil)
	if err != nil {
		t.Fatalf("planStyleChainShadowPatches (used): %v", err)
	}
	if len(notes) == 0 || !strings.Contains(notes[0], "No Spacing") {
		t.Errorf("notes = %v, want it to name \"No Spacing\" (PROBE-N)", notes)
	}
}

// TestPlanStyleChainShadowPatches_PeripheralStylesExcludedButNoted is the
// red test for the follow-up review's "激进副作用纠正": Header/Footer/Caption
// must never be rewritten by body_size_pt (or any other whole-document
// rule) — a page header/footer/figure caption is not "body text" — but,
// unlike Quote/SourceCode, they ARE reported via notes when they shadow a
// requested field and are actually used, since nothing else in this task
// gives a caller another way to find out.
func TestPlanStyleChainShadowPatches_PeripheralStylesExcludedButNoted(t *testing.T) {
	styles := []byte(`<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		styleChainTestNormal +
		`<w:style w:type="paragraph" w:styleId="Header"><w:name w:val="header"/><w:basedOn w:val="Normal"/>` +
		`<w:pPr><w:spacing w:after="0" w:line="240" w:lineRule="auto"/></w:pPr></w:style>` +
		`<w:style w:type="paragraph" w:styleId="Footer"><w:name w:val="footer"/><w:basedOn w:val="Normal"/>` +
		`<w:pPr><w:spacing w:after="0" w:line="240" w:lineRule="auto"/></w:pPr></w:style>` +
		`<w:style w:type="paragraph" w:styleId="Caption"><w:name w:val="caption"/><w:basedOn w:val="Normal"/>` +
		`<w:rPr><w:sz w:val="18"/></w:rPr></w:style>` +
		`</w:styles>`)
	opts := FormatOptions{LineSpacing: 1.5, BodySizePt: 13}
	used := map[string]bool{"Header": true, "Footer": true, "Caption": true}

	patches, changed, notes, err := planStyleChainShadowPatches(styles, opts, used, nil)
	if err != nil {
		t.Fatalf("planStyleChainShadowPatches: %v", err)
	}
	if len(patches) != 0 || changed["line spacing"] || changed["body size"] {
		t.Errorf("Header/Footer/Caption must never be rewritten by whole-document rules; got %d patches, changed=%v", len(patches), changed)
	}
	joined := strings.Join(notes, " | ")
	for _, want := range []string{"header", "footer", "caption"} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes = %v, want it to name %q", notes, want)
		}
	}
}

// TestPlanStyleChainShadowPatches_HeadingFontSameCallExemptsHeadingNote is
// the red test for F3/F4's "no longer self-contradictory" fix: when
// heading_font is ALSO requested in the same call and it structurally
// matched a Heading1..9 style, that style must not simultaneously be named
// as "masking" body_font — it just received its own deliberate font from
// the other rule. Without heading_font in the same call, the same style
// legitimately still masks body_font and must be reported.
func TestPlanStyleChainShadowPatches_HeadingFontSameCallExemptsHeadingNote(t *testing.T) {
	styles := []byte(`<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		styleChainTestNormal +
		`<w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/><w:basedOn w:val="Normal"/>` +
		`<w:rPr><w:rFonts w:ascii="Cambria"/></w:rPr></w:style>` +
		`</w:styles>`)
	used := map[string]bool{"Heading1": true}

	// heading_font requested this call AND structurally matched Heading1 ->
	// exempt from the body-font note.
	_, _, notes, err := planStyleChainShadowPatches(styles,
		FormatOptions{BodyFont: "Georgia", HeadingFont: "Cambria"}, used, map[string]bool{"Heading1": true})
	if err != nil {
		t.Fatalf("planStyleChainShadowPatches (same-call): %v", err)
	}
	for _, n := range notes {
		if strings.Contains(n, "heading 1") {
			t.Errorf("notes = %v, want no mention of heading 1: it was rewritten by heading_font this same call", notes)
		}
	}

	// heading_font NOT requested this call -> Heading1 legitimately still
	// masks body_font.
	_, _, notes, err = planStyleChainShadowPatches(styles, FormatOptions{BodyFont: "Georgia"}, used, nil)
	if err != nil {
		t.Fatalf("planStyleChainShadowPatches (no heading_font): %v", err)
	}
	found := false
	for _, n := range notes {
		if strings.Contains(n, "heading 1") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %v, want it to name \"heading 1\": nothing rewrote its font this call", notes)
	}
}

// TestDirectFormatMaskingNotes_ExcludesSourceCodeParagraphs is the red test
// for F6: a fenced code block's own run carries a DIRECT <w:rFonts> copy of
// SourceCode's monospace font on purpose (styles.go's codeRunFontsXML), so
// it must never be flagged as a paragraph the caller should "fix" with
// start_para/end_para — doing so would suggest overwriting the very font
// that makes it look like code.
func TestDirectFormatMaskingNotes_ExcludesSourceCodeParagraphs(t *testing.T) {
	doc := []byte(`<w:body>` +
		`<w:p><w:pPr><w:pStyle w:val="SourceCode"/></w:pPr><w:r><w:rPr><w:rFonts w:ascii="Consolas"/></w:rPr><w:t>code</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>plain</w:t></w:r></w:p>` +
		`</w:body>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	notes, err := directFormatMaskingNotes(doc, paras, FormatOptions{BodyFont: "Georgia"})
	if err != nil {
		t.Fatalf("directFormatMaskingNotes: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("notes = %v, want none: the only direct rFonts belongs to a SourceCode paragraph, which must be excluded", notes)
	}
}

// TestDirectFormatMaskingNotes_ListsExactParagraphNumbersNotARange is the
// red test for F6's other half: a min/max range spanning two flagged,
// non-adjacent paragraphs can silently sweep in an untouched one sitting
// between them. Paragraph 2 here carries no direct formatting at all and
// must never appear as part of a "start_para=1/end_para=3"-shaped
// suggestion.
func TestDirectFormatMaskingNotes_ListsExactParagraphNumbersNotARange(t *testing.T) {
	doc := []byte(`<w:body>` +
		`<w:p><w:r><w:rPr><w:rFonts w:ascii="Times"/></w:rPr><w:t>a</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>b</w:t></w:r></w:p>` +
		`<w:p><w:r><w:rPr><w:rFonts w:ascii="Times"/></w:rPr><w:t>c</w:t></w:r></w:p>` +
		`</w:body>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	notes, err := directFormatMaskingNotes(doc, paras, FormatOptions{BodyFont: "Georgia"})
	if err != nil {
		t.Fatalf("directFormatMaskingNotes: %v", err)
	}
	if len(notes) != 1 {
		t.Fatalf("notes = %v, want exactly one (font)", notes)
	}
	if strings.Contains(notes[0], "start_para=1/end_para=3") || strings.Contains(notes[0], "end_para=3") {
		t.Errorf("note still uses an unsafe min-max range that would sweep in untouched paragraph 2: %s", notes[0])
	}
	if !strings.Contains(notes[0], "1") || !strings.Contains(notes[0], "3") {
		t.Errorf("note = %q, want it to list paragraphs 1 and 3 explicitly", notes[0])
	}
}

// TestPlanHeadingFontPatches_SecondIdenticalCallProducesNoPatches is the red
// test for round 2's F1: planHeadingFontPatches always rewrote a found
// <w:rFonts> regardless of whether it already matched the requested font, so
// len(headingPatches) > 0 was trivially always true for any document with at
// least one Heading1..9 style — a second heading_font call with the SAME
// font kept producing a patch (and, one level up, a real SetPart + Save +
// .bak) even though styles.xml would end up byte-identical.
func TestPlanHeadingFontPatches_SecondIdenticalCallProducesNoPatches(t *testing.T) {
	styles := []byte(`<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		styleChainTestNormal +
		`<w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/><w:basedOn w:val="Normal"/>` +
		`<w:rPr><w:rFonts w:ascii="Cambria"/></w:rPr></w:style>` +
		`</w:styles>`)

	first, touched, err := planHeadingFontPatches(styles, "Georgia", "")
	if err != nil {
		t.Fatalf("first planHeadingFontPatches: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("first call produced no patches; the font never took effect to begin with")
	}
	if !touched["Heading1"] {
		t.Fatalf("touched = %v, want Heading1", touched)
	}
	out, err := Apply(styles, first)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	second, _, err := planHeadingFontPatches(out, "Georgia", "")
	if err != nil {
		t.Fatalf("second planHeadingFontPatches: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second identical call produced %d patches, want 0: Heading1's rFonts already renders as Georgia", len(second))
	}
}

// TestWriteThenFormat_SecondIdenticalHeadingFontCallDoesNotWriteAgain is the
// compose-level companion: a second Document.Format call with the exact
// same HeadingFont must report an empty Applied and must NOT mark the
// document modified again (which is what would trigger a redundant
// Save/backup one level up, in the docx_format tool handler, for a
// styles.xml that ends up byte-identical).
func TestWriteThenFormat_SecondIdenticalHeadingFontCallDoesNotWriteAgain(t *testing.T) {
	md := "# Title\n\nSome body text here.\n"
	d, _, _ := writeAndReopen(t, md)
	opts := FormatOptions{HeadingFont: "Georgia"}

	first, err := d.Format(opts)
	if err != nil {
		t.Fatalf("first Format: %v", err)
	}
	if len(first.Applied) == 0 {
		t.Fatal("first call's Applied is empty; the rule never took effect to begin with")
	}
	if !d.Modified() {
		t.Fatal("first call did not mark the document modified")
	}
	if err := d.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	second, err := d.Format(opts)
	if err != nil {
		t.Fatalf("second Format: %v", err)
	}
	if len(second.Applied) != 0 {
		t.Errorf("second identical call's Applied = %v, want empty", second.Applied)
	}
	if d.Modified() {
		t.Error("second identical call marked the document modified again; styles.xml should be byte-identical")
	}
}

// TestPlanStyleChainShadowPatches_ProbeQ_WordBuiltinMonospaceStylesExcludedButNoted
// is the red test for round 2's second nit: Word's own built-in Plain Text/
// HTML Code/HTML Preformatted/Block Text styles use a monospace font
// (Consolas/Courier-class) the same way this package's own SourceCode does,
// but the exact-match sets F2 introduced only covered THIS package's own
// styleIds — a real Word document's own "Plain Text" style was treated as
// an ordinary body-type style and silently rewritten to a proportional
// body_font. All four must be excluded from the rewrite AND (unlike
// SourceCode, which stays silent) named in notes when used.
func TestPlanStyleChainShadowPatches_ProbeQ_WordBuiltinMonospaceStylesExcludedButNoted(t *testing.T) {
	styles := []byte(`<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		styleChainTestNormal +
		`<w:style w:type="paragraph" w:styleId="PlainText"><w:name w:val="Plain Text"/><w:basedOn w:val="Normal"/>` +
		`<w:rPr><w:rFonts w:ascii="Consolas"/></w:rPr></w:style>` +
		`<w:style w:type="paragraph" w:styleId="HTMLCode"><w:name w:val="HTML Code"/><w:basedOn w:val="Normal"/>` +
		`<w:rPr><w:rFonts w:ascii="Courier New"/></w:rPr></w:style>` +
		`<w:style w:type="paragraph" w:styleId="HTMLPreformatted"><w:name w:val="HTML Preformatted"/><w:basedOn w:val="Normal"/>` +
		`<w:rPr><w:rFonts w:ascii="Courier New"/></w:rPr></w:style>` +
		`<w:style w:type="paragraph" w:styleId="BlockText"><w:name w:val="Block Text"/><w:basedOn w:val="Normal"/>` +
		`<w:rPr><w:rFonts w:ascii="Consolas"/></w:rPr></w:style>` +
		`</w:styles>`)
	opts := FormatOptions{BodyFont: "Georgia"}
	used := map[string]bool{"PlainText": true, "HTMLCode": true, "HTMLPreformatted": true, "BlockText": true}

	patches, changed, notes, err := planStyleChainShadowPatches(styles, opts, used, nil)
	if err != nil {
		t.Fatalf("planStyleChainShadowPatches: %v", err)
	}
	if len(patches) != 0 || changed["body font"] {
		t.Errorf("Plain Text/HTML Code/HTML Preformatted/Block Text must never be rewritten by body_font; got %d patches, changed=%v", len(patches), changed)
	}
	joined := strings.Join(notes, " | ")
	for _, want := range []string{"Plain Text", "HTML Code", "HTML Preformatted", "Block Text"} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes = %v, want it to name %q (PROBE-Q)", notes, want)
		}
	}

	// Unused -> no noise (F5 still applies to this family).
	_, _, notes, err = planStyleChainShadowPatches(styles, opts, map[string]bool{}, nil)
	if err != nil {
		t.Fatalf("planStyleChainShadowPatches (unused): %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("notes = %v, want none: nothing in the document references any of these styles", notes)
	}
}
