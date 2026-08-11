package docx

import (
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
	if strings.Contains(h1, "Theme=") {
		t.Errorf("Heading1 still carries theme font attributes, which override the literal one:\n%s", h1)
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
