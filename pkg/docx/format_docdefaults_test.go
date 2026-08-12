package docx

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"
)

// mustWellFormedXML fully decodes data, failing the test if it is not
// well-formed XML -- the same check Word (and this package's own OpenDocument
// path) effectively performs, and the only reliable way to catch an
// out-of-schema-order or malformed insert: a corrupt styles.xml still looks
// like a plausible substring match on a lazy test, but a full decode walks
// every open/close tag and fails loudly on a mismatch.
func mustWellFormedXML(t *testing.T, data []byte) {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		_, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				return
			}
			t.Fatalf("output is not well-formed XML: %v\n%s", err, data)
		}
	}
}

// applyStylesPatches is a small helper shared by the docDefaults synthesis
// tests below: plan the patches Format's styles.xml path would produce for
// opts against styles, apply them, and hand back the resulting bytes.
func applyStylesPatches(t *testing.T, styles []byte, opts FormatOptions) string {
	t.Helper()
	patches, _, _, err := planStylesPatches(styles, opts)
	if err != nil {
		t.Fatalf("planStylesPatches: %v", err)
	}
	out, err := Apply(styles, patches)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	mustWellFormedXML(t, out)
	return string(out)
}

const stylesNoDocDefaults = `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
	`</w:styles>`

// TestPlanStylesPatches_NoDocDefaultsAtAll is the fully-missing case a
// minimal, non-docx_write generator could hand docx_format: no
// <w:docDefaults> element anywhere in styles.xml. It must be created as
// <w:styles>'s FIRST child (schema-mandated position), with rPrDefault
// preceding pPrDefault, rather than the old "cannot set body font/size"
// error.
func TestPlanStylesPatches_NoDocDefaultsAtAll(t *testing.T) {
	out := applyStylesPatches(t, []byte(stylesNoDocDefaults), FormatOptions{BodyFont: "Calibri", LineSpacing: 1.5})

	ddStart := strings.Index(out, "<w:docDefaults>")
	if ddStart == -1 {
		t.Fatalf("no <w:docDefaults> was created:\n%s", out)
	}
	stylesTagEnd := strings.Index(out, ">") + 1 // end of the root <w:styles ...> start tag
	if ddStart != stylesTagEnd {
		t.Errorf("<w:docDefaults> is not <w:styles>'s first child (starts at %d, styles tag ends at %d):\n%s",
			ddStart, stylesTagEnd, out)
	}
	rpdIdx := strings.Index(out, "<w:rPrDefault>")
	ppdIdx := strings.Index(out, "<w:pPrDefault>")
	if rpdIdx == -1 || ppdIdx == -1 {
		t.Fatalf("rPrDefault/pPrDefault missing from the created chain:\n%s", out)
	}
	if rpdIdx > ppdIdx {
		t.Errorf("rPrDefault must precede pPrDefault (schema order); got rPrDefault at %d, pPrDefault at %d", rpdIdx, ppdIdx)
	}
	if !strings.Contains(out, `w:ascii="Calibri"`) {
		t.Errorf("body font was not applied:\n%s", out)
	}
	if !strings.Contains(out, `w:line="360"`) {
		t.Errorf("line spacing was not applied:\n%s", out)
	}
	if !strings.Contains(out, `w:styleId="Normal"`) {
		t.Errorf("the pre-existing Normal style was lost:\n%s", out)
	}
}

const stylesEmptyDocDefaults = `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	`<w:docDefaults></w:docDefaults>` +
	`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
	`</w:styles>`

// TestPlanStylesPatches_EmptyDocDefaults covers <w:docDefaults></w:docDefaults>
// present but with neither child: this is the case where a naive
// implementation would insert rPrDefault and pPrDefault as two separate
// patches sharing the exact same offset (docDefaults' start == its own
// close, since it is empty) and Apply would reject them as overlapping.
func TestPlanStylesPatches_EmptyDocDefaults(t *testing.T) {
	out := applyStylesPatches(t, []byte(stylesEmptyDocDefaults), FormatOptions{BodyFont: "Georgia", Align: "left"})

	dd := out[strings.Index(out, "<w:docDefaults>"):strings.Index(out, "</w:docDefaults>")]
	rpdIdx := strings.Index(dd, "<w:rPrDefault>")
	ppdIdx := strings.Index(dd, "<w:pPrDefault>")
	if rpdIdx == -1 || ppdIdx == -1 {
		t.Fatalf("rPrDefault/pPrDefault were not both created inside the pre-existing empty docDefaults:\n%s", dd)
	}
	if rpdIdx > ppdIdx {
		t.Errorf("rPrDefault must precede pPrDefault; got rPrDefault at %d, pPrDefault at %d in:\n%s", rpdIdx, ppdIdx, dd)
	}
	if !strings.Contains(dd, `w:ascii="Georgia"`) {
		t.Errorf("body font was not applied:\n%s", dd)
	}
	if !strings.Contains(dd, `<w:jc w:val="left"/>`) {
		t.Errorf("alignment was not applied:\n%s", dd)
	}
}

const stylesPPrDefaultOnly = `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	`<w:docDefaults><w:pPrDefault><w:pPr><w:spacing w:after="200" w:line="276" w:lineRule="auto"/></w:pPr></w:pPrDefault></w:docDefaults>` +
	`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
	`</w:styles>`

// TestPlanStylesPatches_DocDefaultsPresentButRPrDefaultMissing is exactly the
// self-review scenario: <w:docDefaults> exists (with a real, populated
// pPrDefault already inside it -- something a truncated or hand-edited
// styles.xml plausibly has) but <w:rPrDefault> is entirely absent.
// rPrDefault must be created as docDefaults' FIRST child, ahead of the
// pre-existing pPrDefault, and that pPrDefault must survive byte-for-byte.
func TestPlanStylesPatches_DocDefaultsPresentButRPrDefaultMissing(t *testing.T) {
	out := applyStylesPatches(t, []byte(stylesPPrDefaultOnly), FormatOptions{BodyFont: "Calibri", BodySizePt: 12})

	dd := out[strings.Index(out, "<w:docDefaults>"):strings.Index(out, "</w:docDefaults>")]
	rpdIdx := strings.Index(dd, "<w:rPrDefault>")
	ppdIdx := strings.Index(dd, "<w:pPrDefault>")
	if rpdIdx == -1 {
		t.Fatalf("rPrDefault was not created:\n%s", dd)
	}
	if rpdIdx > ppdIdx {
		t.Errorf("rPrDefault must precede the pre-existing pPrDefault; got rPrDefault at %d, pPrDefault at %d in:\n%s", rpdIdx, ppdIdx, dd)
	}
	if !strings.Contains(dd, `w:ascii="Calibri"`) {
		t.Errorf("body font was not applied:\n%s", dd)
	}
	if !strings.Contains(dd, `<w:sz w:val="24"/>`) {
		t.Errorf("body size (12pt -> 24 half-points) was not applied:\n%s", dd)
	}
	if !strings.Contains(dd, `<w:spacing w:after="200" w:line="276" w:lineRule="auto"/>`) {
		t.Errorf("the pre-existing pPrDefault was not preserved byte-for-byte:\n%s", dd)
	}
}

const stylesRPrDefaultOnly = `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	`<w:docDefaults><w:rPrDefault><w:rPr><w:rFonts w:ascii="Existing"/></w:rPr></w:rPrDefault></w:docDefaults>` +
	`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
	`</w:styles>`

// TestPlanStylesPatches_DocDefaultsPresentButPPrDefaultMissing is the mirror
// image: rPrDefault already exists and must be left alone (this request
// doesn't even touch the rPr chain), pPrDefault is missing and must be
// created as docDefaults' LAST child (after the pre-existing rPrDefault).
func TestPlanStylesPatches_DocDefaultsPresentButPPrDefaultMissing(t *testing.T) {
	out := applyStylesPatches(t, []byte(stylesRPrDefaultOnly), FormatOptions{LineSpacing: 2.0})

	dd := out[strings.Index(out, "<w:docDefaults>"):strings.Index(out, "</w:docDefaults>")]
	rpdIdx := strings.Index(dd, "<w:rPrDefault>")
	ppdIdx := strings.Index(dd, "<w:pPrDefault>")
	if ppdIdx == -1 {
		t.Fatalf("pPrDefault was not created:\n%s", dd)
	}
	if rpdIdx > ppdIdx {
		t.Errorf("the pre-existing rPrDefault must precede the new pPrDefault; got rPrDefault at %d, pPrDefault at %d in:\n%s", rpdIdx, ppdIdx, dd)
	}
	if !strings.Contains(dd, `w:ascii="Existing"`) {
		t.Errorf("the pre-existing rPrDefault/rFonts was not preserved:\n%s", dd)
	}
	if !strings.Contains(dd, `w:line="480"`) {
		t.Errorf("line spacing (2.0 -> 480 240ths) was not applied:\n%s", dd)
	}
}

const stylesRPrDefaultPresentButEmpty = `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	`<w:docDefaults><w:rPrDefault></w:rPrDefault><w:pPrDefault><w:pPr><w:jc w:val="left"/></w:pPr></w:pPrDefault></w:docDefaults>` +
	`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
	`</w:styles>`

// TestPlanStylesPatches_RPrDefaultPresentButRPrMissing goes one level
// deeper still: rPrDefault itself exists but is empty (no <w:rPr> inside
// it at all) -- rPr must be inserted as rPrDefault's sole child, and the
// unrelated, already-populated pPrDefault sibling must be untouched.
func TestPlanStylesPatches_RPrDefaultPresentButRPrMissing(t *testing.T) {
	out := applyStylesPatches(t, []byte(stylesRPrDefaultPresentButEmpty), FormatOptions{BodySizePt: 11})

	dd := out[strings.Index(out, "<w:docDefaults>"):strings.Index(out, "</w:docDefaults>")]
	if !strings.Contains(dd, `<w:rPrDefault><w:rPr>`) {
		t.Fatalf("rPr was not created inside the pre-existing empty rPrDefault:\n%s", dd)
	}
	if !strings.Contains(dd, `<w:sz w:val="22"/>`) {
		t.Errorf("body size (11pt -> 22 half-points) was not applied:\n%s", dd)
	}
	if !strings.Contains(dd, `<w:pPrDefault><w:pPr><w:jc w:val="left"/></w:pPr></w:pPrDefault>`) {
		t.Errorf("the unrelated pPrDefault sibling was not preserved byte-for-byte:\n%s", dd)
	}
}

const stylesPPrDefaultPresentButEmpty = `<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	`<w:docDefaults><w:rPrDefault><w:rPr><w:rFonts w:ascii="Existing"/></w:rPr></w:rPrDefault><w:pPrDefault></w:pPrDefault></w:docDefaults>` +
	`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
	`</w:styles>`

// TestPlanStylesPatches_PPrDefaultPresentButPPrMissing mirrors
// TestPlanStylesPatches_RPrDefaultPresentButRPrMissing for the pPr side:
// pPrDefault itself exists but is empty (no <w:pPr> inside it) -- pPr must
// be inserted as pPrDefault's sole child, and the unrelated, already-
// populated rPrDefault sibling must be untouched.
func TestPlanStylesPatches_PPrDefaultPresentButPPrMissing(t *testing.T) {
	out := applyStylesPatches(t, []byte(stylesPPrDefaultPresentButEmpty), FormatOptions{Align: "justify"})

	dd := out[strings.Index(out, "<w:docDefaults>"):strings.Index(out, "</w:docDefaults>")]
	if !strings.Contains(dd, `<w:pPrDefault><w:pPr>`) {
		t.Fatalf("pPr was not created inside the pre-existing empty pPrDefault:\n%s", dd)
	}
	if !strings.Contains(dd, `<w:jc w:val="justify"/>`) {
		t.Errorf("alignment was not applied:\n%s", dd)
	}
	if !strings.Contains(dd, `<w:rPrDefault><w:rPr><w:rFonts w:ascii="Existing"/></w:rPr></w:rPrDefault>`) {
		t.Errorf("the unrelated rPrDefault sibling was not preserved byte-for-byte:\n%s", dd)
	}
}
