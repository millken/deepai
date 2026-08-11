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
