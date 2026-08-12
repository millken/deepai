package docx

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func formatDoc(t *testing.T) (*Document, string) {
	t.Helper()
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "f.docx")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := OpenDocument(p)
	if err != nil {
		t.Fatal(err)
	}
	return d, p
}

func stylesXML(t *testing.T, d *Document) string {
	t.Helper()
	b, ok := d.Part("word/styles.xml")
	if !ok {
		t.Fatal("styles.xml missing")
	}
	return string(b)
}

func TestFormat_BodySizeLandsInDocDefaultsAndSyncsSzCs(t *testing.T) {
	d, _ := formatDoc(t)
	if _, err := d.Format(FormatOptions{BodySizePt: 14}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	s := stylesXML(t, d)
	dd := s[strings.Index(s, "<w:docDefaults>"):strings.Index(s, "</w:docDefaults>")]
	if !strings.Contains(dd, `<w:sz w:val="28"/>`) {
		t.Errorf("docDefaults lacks sz=28 (14pt in half-points):\n%.400s", dd)
	}
	if !strings.Contains(dd, `<w:szCs w:val="28"/>`) {
		t.Error("szCs was not synced; CJK and complex scripts would keep the old size")
	}
}

// TestFormat_HeadingFontRemovesThemeAttributes is the trap this task exists
// to avoid: the fixture's heading rFonts carries w:asciiTheme="majorHAnsi",
// and a literal w:ascii added beside it is ignored by Word — the theme wins.
// It also pins task 8's follow-on fix: HeadingFont on its own no longer
// touches eastAsia/eastAsiaTheme/cs/cstheme at all (that pair is
// BodyEastAsiaFont's own job — see TestFormat_HeadingFontWithBodyEastAsiaFont
// below), so those two *Theme attributes are expected to SURVIVE here.
func TestFormat_HeadingFontRemovesThemeAttributes(t *testing.T) {
	d, _ := formatDoc(t)
	if _, err := d.Format(FormatOptions{HeadingFont: "Georgia"}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	s := stylesXML(t, d)
	h1 := s[strings.Index(s, `w:styleId="Heading1"`):]
	h1 = h1[:strings.Index(h1, "</w:style>")]
	if !strings.Contains(h1, `w:ascii="Georgia"`) {
		t.Errorf("Heading1 lacks the literal font:\n%s", h1)
	}
	if strings.Contains(h1, "asciiTheme=") || strings.Contains(h1, "hAnsiTheme=") {
		t.Errorf("Heading1 still carries ascii/hAnsi theme font attributes, which override the literal one:\n%s", h1)
	}
	if !strings.Contains(h1, "eastAsiaTheme=") {
		t.Errorf("Heading1 lost its eastAsiaTheme attribute; HeadingFont alone must not touch eastAsia at all:\n%s", h1)
	}
}

// TestFormat_HeadingFontWithBodyEastAsiaFont covers HeadingFont+BodyEastAsiaFont
// given together in the SAME call: only then does the heading's own
// eastAsia font/theme get replaced too (task 8 brief; task 7 复审遗留).
func TestFormat_HeadingFontWithBodyEastAsiaFont(t *testing.T) {
	d, _ := formatDoc(t)
	if _, err := d.Format(FormatOptions{HeadingFont: "Georgia", BodyEastAsiaFont: "SimSun"}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	s := stylesXML(t, d)
	h1 := s[strings.Index(s, `w:styleId="Heading1"`):]
	h1 = h1[:strings.Index(h1, "</w:style>")]
	if !strings.Contains(h1, `w:ascii="Georgia"`) {
		t.Errorf("Heading1 lacks the literal Latin font:\n%s", h1)
	}
	if !strings.Contains(h1, `w:eastAsia="SimSun"`) {
		t.Errorf("Heading1 lacks the literal east-asia font:\n%s", h1)
	}
	if strings.Contains(h1, "Theme=") {
		t.Errorf("Heading1 still carries a theme font attribute:\n%s", h1)
	}
}

func TestFormat_MarginsLandInSectPrAsTwips(t *testing.T) {
	d, _ := formatDoc(t)
	if _, err := d.Format(FormatOptions{MarginsMM: []float64{25.4, 25.4, 25.4, 25.4}}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	doc, _ := d.Part(DocumentPart)
	if !strings.Contains(string(doc), `w:top="1440"`) {
		t.Errorf("pgMar top is not 1440 twips (25.4mm)")
	}
}

func TestFormat_RejectsWrongMarginCount(t *testing.T) {
	d, _ := formatDoc(t)
	if _, err := d.Format(FormatOptions{MarginsMM: []float64{10, 20}}); err == nil {
		t.Fatal("a 2-element margins list was accepted; it must be exactly 4")
	}
}

// TestFormat_LeavesBodyTextUntouched is §4.3's core promise.
func TestFormat_LeavesBodyTextUntouched(t *testing.T) {
	d, _ := formatDoc(t)
	before := make([]string, 0, d.TotalParas())
	for _, p := range d.Paras() {
		var b strings.Builder
		for _, r := range p.Runs {
			b.WriteString(r.Text)
		}
		before = append(before, b.String())
	}
	if _, err := d.Format(FormatOptions{BodyFont: "Georgia", BodySizePt: 13, LineSpacing: 1.5, Align: "justify"}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	if d.TotalParas() != len(before) {
		t.Fatalf("paragraph count changed: %d -> %d", len(before), d.TotalParas())
	}
	for i, p := range d.Paras() {
		var b strings.Builder
		for _, r := range p.Runs {
			b.WriteString(r.Text)
		}
		if b.String() != before[i] {
			t.Errorf("paragraph %d text changed: %q -> %q", i+1, before[i], b.String())
		}
	}
}

// TestFormat_TouchesOnlyTheExpectedParts pins that formatting does not
// disturb entries it has no business in.
func TestFormat_TouchesOnlyTheExpectedParts(t *testing.T) {
	d, p := formatDoc(t)
	if _, err := d.Format(FormatOptions{BodySizePt: 13, MarginsMM: []float64{20, 20, 20, 20}}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	if err := d.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	assertEntriesEqual(t, fixture, p, map[string]bool{
		"word/styles.xml": true,
		DocumentPart:      true,
	})
}

func TestFormat_StylesOnlyChangeLeavesDocumentXMLAlone(t *testing.T) {
	d, p := formatDoc(t)
	if _, err := d.Format(FormatOptions{BodySizePt: 13}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	if err := d.Save(); err != nil {
		t.Fatal(err)
	}
	assertEntriesEqual(t, fixture, p, map[string]bool{"word/styles.xml": true})
}

func TestFormat_NormalizeMergesConsecutiveEmptyParagraphs(t *testing.T) {
	doc := []byte(`<w:body>` +
		`<w:p><w:r><w:t>one</w:t></w:r></w:p>` +
		`<w:p/><w:p/><w:p/>` +
		`<w:p><w:r><w:t>two</w:t></w:r></w:p>` +
		`</w:body>`)
	got, removed, err := normalizeEmptyParagraphs(doc)
	if err != nil {
		t.Fatalf("normalizeEmptyParagraphs: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2 (three empties collapse to one)", removed)
	}
	paras, err := Scan(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(paras) != 3 {
		t.Fatalf("got %d paragraphs, want 3 (one, one empty, two)", len(paras))
	}
}

func TestFormat_NormalizeLeavesASingleEmptyParagraphAlone(t *testing.T) {
	doc := []byte(`<w:body><w:p><w:r><w:t>a</w:t></w:r></w:p><w:p/><w:p><w:r><w:t>b</w:t></w:r></w:p></w:body>`)
	_, removed, err := normalizeEmptyParagraphs(doc)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0; a lone empty paragraph is deliberate spacing", removed)
	}
}

func TestFormat_TemplateExpandsAndExplicitFieldsWin(t *testing.T) {
	d, _ := formatDoc(t)
	res, err := d.Format(FormatOptions{Template: "academic", BodySizePt: 13})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	s := stylesXML(t, d)
	dd := s[strings.Index(s, "<w:docDefaults>"):strings.Index(s, "</w:docDefaults>")]
	if !strings.Contains(dd, `<w:sz w:val="26"/>`) {
		t.Errorf("explicit BodySizePt 13 did not override the academic template's 12pt")
	}
	if !strings.Contains(dd, "Times New Roman") {
		t.Errorf("the academic template's body font was not applied")
	}
	if len(res.Applied) == 0 {
		t.Error("Applied is empty; the caller cannot tell what changed")
	}
}

// TestFormat_NormalizeSetsTotalParasAndParaCountChanged pins task 10 brief
// item 1 / seams review C2: normalize deletes paragraphs, so
// FormatResult.TotalParas/ParaCountChanged must report that the same way
// EditResult already does for docx_edit, letting the tool layer trigger
// docxIndexAdvice instead of leaving a caller's earlier paragraph indices
// silently stale.
func TestFormat_NormalizeSetsTotalParasAndParaCountChanged(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>one</w:t></w:r></w:p><w:p/><w:p/><w:p/><w:p><w:r><w:t>two</w:t></w:r></w:p>`)
	before := d.TotalParas()
	res, err := d.Format(FormatOptions{Normalize: true})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	after := d.TotalParas()
	if after != before-2 {
		t.Fatalf("TotalParas after normalize = %d, want %d (three empties collapse to one, removing 2)", after, before-2)
	}
	if res.TotalParas != after {
		t.Errorf("FormatResult.TotalParas = %d, want %d (Document.TotalParas() immediately after Format)", res.TotalParas, after)
	}
	if !res.ParaCountChanged {
		t.Error("ParaCountChanged = false, want true: normalize deleted paragraphs, so earlier paragraph indices are now stale")
	}
}

// TestFormat_NonNormalizeCallReportsParaCountUnchanged is
// TestFormat_NormalizeSetsTotalParasAndParaCountChanged's negative case: a
// rule that never touches paragraph count must report TotalParas (still
// populated, the same way docx_edit always populates it regardless of
// ParaCountChanged) and ParaCountChanged=false, not the zero value that
// would look identical to "never computed".
func TestFormat_NonNormalizeCallReportsParaCountUnchanged(t *testing.T) {
	d, _ := formatDoc(t)
	before := d.TotalParas()
	res, err := d.Format(FormatOptions{BodySizePt: 14})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if res.TotalParas != before {
		t.Errorf("TotalParas = %d, want %d (body_size_pt never changes paragraph count)", res.TotalParas, before)
	}
	if res.ParaCountChanged {
		t.Error("ParaCountChanged = true, want false")
	}
}

func TestFormat_UnknownTemplateErrors(t *testing.T) {
	d, _ := formatDoc(t)
	if _, err := d.Format(FormatOptions{Template: "fancy"}); err == nil {
		t.Fatal("an unknown template was accepted")
	}
}

func TestFormat_NoOptionsIsANoOp(t *testing.T) {
	d, p := formatDoc(t)
	res, err := d.Format(FormatOptions{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if len(res.Applied) != 0 {
		t.Errorf("Applied = %v, want empty for an empty request", res.Applied)
	}
	if err := d.Save(); err != nil {
		t.Fatal(err)
	}
	assertEntriesEqual(t, fixture, p, nil)
}

// TestFormat_NoRangeOutputIsUnchangedByDirectFormatting pins P2a.5's core
// promise about the EXISTING (whole-document) path: adding paragraph-scoped
// direct formatting must not change a single byte of what a StartPara==0
// call produces. document.xml's hash/length are untouched from the original
// P2a.5 Task 1 capture (this call sets Normalize/MarginsMM too, both of
// which land in document.xml, and neither is touched by task 7 below).
//
// styles.xml's hash/length were re-captured TWICE for task 7:
//
//  1. format capability review, Critical 4 / write review I4-I5 (first
//     pass): the whole-document path started also rewriting whichever real
//     style in this fixture already shadowed docDefaults — Heading1-9's own
//     font via the pre-existing HeadingFont path, plus Header/Footer/
//     BodyText2/BodyText3/Caption's own explicit spacing/size via the new
//     effective-chain rewrite.
//  2. task 7 FOLLOW-UP review's "激进副作用纠正" (second pass, this capture):
//     Header/Footer/Caption were pulled back OUT of the effective-chain
//     rewrite — a page header/footer/figure caption is not "body text" in
//     the sense body_font/body_size_pt/line_spacing/align mean it, and
//     rewriting them was an unintended side effect of the first pass, not a
//     deliberate design choice. They are now isPeripheralStyle-excluded
//     from the rewrite the same way Quote/SourceCode always were, but
//     (unlike Quote/SourceCode) still surface via FormatResult.Notes when
//     they shadow a requested field and the document actually uses them —
//     see planStyleChainShadowPatches. Heading1-9 (via HeadingFont) and
//     BodyText2/BodyText3 (ordinary basedOn-Normal body styles, not
//     peripheral) still change, unaffected by this second pass.
//
// Verified directly against this fixture while re-capturing these
// constants: Title/Subtitle/IntenseQuote/ListContinue*/NoSpacing/MacroText
// still do NOT change (the first three are excluded families or lack a
// based-on-Normal chain; ListContinue's own <w:spacing> never sets w:line,
// so it was never shadowing line spacing to begin with), and Header/Footer/
// Caption now revert to their ORIGINAL (pre-task-7) bytes, confirmed by diff
// against the pristine fixture.
//
// styles.xml's hash/length were re-captured a THIRD time for task 8:
// HeadingFont alone (given here, with no BodyEastAsiaFont) no longer touches
// eastAsia/cs at all (see planHeadingFontPatches' own doc comment) — this
// fixture's nine Heading1-9 styles each keep their original
// eastAsiaTheme="majorEastAsia"/cstheme="majorBidi" instead of having them
// replaced with literal eastAsia="Georgia"/cs="Georgia" the way the
// pre-task-8 code did. Verified directly: the ONLY byte difference from the
// previous capture is exactly those two theme attributes surviving on all
// nine headings (+162 bytes = 18 bytes/heading * 9), confirmed by diffing
// styles.xml before/after this task's change against the previous capture.
func TestFormat_NoRangeOutputIsUnchangedByDirectFormatting(t *testing.T) {
	const (
		wantStylesSHA256 = "64fcc7c0665521939b960fdf1859b9d48e219184946d6b9b4e60261a9f2e248d"
		wantDocSHA256    = "d84400b6456f8f3be5a02edee299c543f191a9aff867d6dbe9598d795b44e5e9"
		wantStylesLen    = 349321
		wantDocLen       = 3709
	)
	d, _ := formatDoc(t)
	if _, err := d.Format(FormatOptions{
		BodyFont: "Georgia", BodySizePt: 13, LineSpacing: 1.5, Align: "justify",
		HeadingFont: "Georgia", MarginsMM: []float64{25.4, 25.4, 25.4, 25.4}, Normalize: true,
	}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	styles := stylesXML(t, d)
	doc, _ := d.Part(DocumentPart)

	if len(styles) != wantStylesLen {
		t.Errorf("styles.xml length = %d, want %d (output changed)", len(styles), wantStylesLen)
	}
	if len(doc) != wantDocLen {
		t.Errorf("document.xml length = %d, want %d (output changed)", len(doc), wantDocLen)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(styles))); got != wantStylesSHA256 {
		t.Errorf("styles.xml sha256 = %s, want %s (no-range output changed byte for byte)", got, wantStylesSHA256)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(doc)); got != wantDocSHA256 {
		t.Errorf("document.xml sha256 = %s, want %s (no-range output changed byte for byte)", got, wantDocSHA256)
	}
}
