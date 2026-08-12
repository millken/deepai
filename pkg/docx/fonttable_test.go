package docx

import (
	"regexp"
	"strings"
	"testing"
)

// This file covers the fontTable.xml addition: OOXML's font-substitution
// hint mechanism (ECMA-376 17.8.3), added so Word has SOME information --
// at minimum, fixed- vs. variable-pitch -- when a named font is missing,
// instead of substituting with no information at all (this package's prior
// state: no word/fontTable.xml was ever written). See fontTablePart's doc
// comment in write.go for what this does and, just as importantly, does
// NOT do (it is not web-font fallback; there is no priority list).

// fontNamesInStylesXML extracts every distinct font name referenced by any
// w:rFonts element's ascii/eastAsia/hAnsi/cs attribute anywhere in
// styles.xml. Deriving the expected set this way -- from the actual
// generated styles.xml, not a hand-maintained literal list -- is what makes
// TestWrite_FontTableCoversEveryStylesXMLFont resistant to a future style
// quietly adding a new font and nobody updating a hardcoded expectation to
// match: if styles.xml ever names a font, this helper will find it without
// being told to look for that name specifically.
//
// The outer match is scoped to <w:rFonts .../> itself, not to a bare
// "w:eastAsia=" anywhere in the document: <w:lang w:eastAsia="en-US" .../>
// also carries an attribute literally named w:eastAsia, but its value is a
// locale code, not a font name, and an earlier version of this regexp that
// matched it directly produced a false "en-US" font name.
var (
	rFontsElementRE = regexp.MustCompile(`<w:rFonts\b[^/>]*/?>`)
	rFontsAttrRE    = regexp.MustCompile(`w:(?:ascii|eastAsia|hAnsi|cs)="([^"]+)"`)
)

func fontNamesInStylesXML(stylesXML []byte) map[string]bool {
	names := map[string]bool{}
	for _, elem := range rFontsElementRE.FindAllString(string(stylesXML), -1) {
		for _, m := range rFontsAttrRE.FindAllStringSubmatch(elem, -1) {
			names[m[1]] = true
		}
	}
	return names
}

func TestWrite_FontTablePartExistsAndDocumentReopens(t *testing.T) {
	d, _, _ := writeAndReopen(t, "hello world\n")
	if _, ok := d.Part(fontTablePart); !ok {
		t.Fatal("word/fontTable.xml is missing from the generated package")
	}
}

func TestWrite_FontTableRegisteredInContentTypes(t *testing.T) {
	d, _, _ := writeAndReopen(t, "hello world\n")
	ct, ok := d.Part(contentTypesPart)
	if !ok {
		t.Fatal("[Content_Types].xml missing")
	}
	want := `<Override PartName="/word/fontTable.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.fontTable+xml"/>`
	if !strings.Contains(string(ct), want) {
		t.Errorf("[Content_Types].xml = %s, want it to contain %s", ct, want)
	}
}

func TestWrite_FontTableRegisteredInDocumentRels(t *testing.T) {
	d, _, _ := writeAndReopen(t, "hello world\n")
	rels, ok := d.Part("word/_rels/document.xml.rels")
	if !ok {
		t.Fatal("word/_rels/document.xml.rels missing")
	}
	re := regexp.MustCompile(`<Relationship Id="(rId\d+)" Type="http://schemas\.openxmlformats\.org/officeDocument/2006/relationships/fontTable" Target="fontTable\.xml"/>`)
	if !re.MatchString(string(rels)) {
		t.Errorf("word/_rels/document.xml.rels = %s, want a fontTable relationship", rels)
	}
}

// This is the mechanical, non-hardcoded check the task asks for: every font
// name styles.xml actually references must have its own <w:font> entry in
// fontTable.xml. The expected set comes from parsing the generated
// styles.xml itself (fontNamesInStylesXML), so a future font addition that
// forgot to also add a fontTable entry fails this test without anyone
// having to remember to update a separate literal list.
func TestWrite_FontTableCoversEveryStylesXMLFont(t *testing.T) {
	d, _, _ := writeAndReopen(t, "# Title\n\nbody `code` text\n\n```\nfenced\n```\n")
	stylesXML, ok := d.Part("word/styles.xml")
	if !ok {
		t.Fatal("word/styles.xml missing")
	}
	fontTableXML, ok := d.Part(fontTablePart)
	if !ok {
		t.Fatal("word/fontTable.xml missing")
	}
	names := fontNamesInStylesXML(stylesXML)
	if len(names) == 0 {
		t.Fatal("fontNamesInStylesXML found nothing -- the extraction regexp itself is broken")
	}
	for name := range names {
		want := `<w:font w:name="` + name + `">`
		if !strings.Contains(string(fontTableXML), want) {
			t.Errorf("styles.xml names font %q but fontTable.xml has no entry for it:\n%s", name, fontTableXML)
		}
	}
}

// fontEntry extracts one <w:font w:name="name">...</w:font> block so a test
// can inspect its children without a giant single regexp per assertion.
func fontEntry(t *testing.T, fontTableXML []byte, name string) string {
	t.Helper()
	re := regexp.MustCompile(`<w:font w:name="` + regexp.QuoteMeta(name) + `">(.*?)</w:font>`)
	m := re.FindStringSubmatch(string(fontTableXML))
	if m == nil {
		t.Fatalf("no <w:font w:name=%q> entry found in fontTable.xml:\n%s", name, fontTableXML)
	}
	return m[1]
}

// Distinct fonts for every one of the four roles, so pitch can be checked
// unambiguously per role with no risk of a body/code name collision (the
// package's own DEFAULT config collides on the East Asian pair -- see
// TestWrite_FontTableSharedFontBetweenBodyAndCodeIsMarkedFixed for that
// case specifically).
func distinctFontOptions() WriteOptions {
	return WriteOptions{
		Markdown:         "text\n",
		BodyLatinFont:    "Times New Roman",
		BodyEastAsiaFont: "SimSun",
		CodeLatinFont:    "Courier New",
		CodeEastAsiaFont: "NSimSun",
	}
}

func TestWrite_FontTableCodeFontsAreFixedPitch(t *testing.T) {
	d, _, _ := writeAndReopen2(t, distinctFontOptions())
	ft, _ := d.Part(fontTablePart)
	for _, name := range []string{"Courier New", "NSimSun"} {
		entry := fontEntry(t, ft, name)
		if !strings.Contains(entry, `<w:pitch w:val="fixed"/>`) {
			t.Errorf("code font %q entry = %s, want pitch=fixed", name, entry)
		}
	}
}

func TestWrite_FontTableBodyFontsAreVariablePitch(t *testing.T) {
	d, _, _ := writeAndReopen2(t, distinctFontOptions())
	ft, _ := d.Part(fontTablePart)
	for _, name := range []string{"Times New Roman", "SimSun"} {
		entry := fontEntry(t, ft, name)
		if !strings.Contains(entry, `<w:pitch w:val="variable"/>`) {
			t.Errorf("body font %q entry = %s, want pitch=variable", name, entry)
		}
	}
}

// This package's own zero-configuration default sets BodyEastAsiaFont and
// CodeEastAsiaFont to the exact same name (微软雅黑 -- see
// defaultBodyEastAsiaFont/defaultCodeEastAsiaFont in styles.go), so the
// general "body variable, code fixed" rule above cannot apply to both roles
// at once purely by role. But 微软雅黑 is NOT actually fixed-pitch -- it is
// an ordinary proportional UI font, per this package's own doc comment on
// defaultCodeEastAsiaFont in styles.go -- so fontTableXML must declare it
// "variable" regardless of which role asked for it. An earlier version of
// this function computed pitch from role alone and declared the shared
// entry "fixed" (because the code role's guess overwrote the body role's);
// that was a real bug, not a simplification: a machine missing 微软雅黑
// could then get a monospace substitute for ordinary Chinese BODY text.
// This test pins the fix -- "variable", truthfully -- plus that the name
// still appears exactly ONCE (a duplicate <w:font> for the same name would
// itself be invalid).
func TestWrite_FontTableSharedVerifiedFontIsDeclaredTruthfully(t *testing.T) {
	d, _, _ := writeAndReopen(t, "body text\n")
	ft, _ := d.Part(fontTablePart)
	count := strings.Count(string(ft), `<w:font w:name="微软雅黑">`)
	if count != 1 {
		t.Fatalf("微软雅黑 appears %d times in fontTable.xml, want exactly 1:\n%s", count, ft)
	}
	entry := fontEntry(t, ft, "微软雅黑")
	if !strings.Contains(entry, `<w:pitch w:val="variable"/>`) {
		t.Errorf("shared body/code font entry = %s, want pitch=variable (微软雅黑 is proportional, not fixed-pitch)", entry)
	}
}

// A font this package HAS independently verified is genuinely fixed-pitch
// (NSimSun -- see knownFontMetadata's doc comment, sourced from
// defaultCodeEastAsiaFont's own list of 2:1-ratio fonts in styles.go) must
// stay "fixed" even when a caller (unusually) asks for it in BOTH the body
// and the code role: unlike 微软雅黑's case above, this is not a role
// conflict to arbitrate -- the font truly is fixed-pitch regardless of
// which role uses it, so both role sightings resolve to the identical,
// truthful value with nothing to arbitrate.
func TestWrite_FontTableVerifiedFixedFontStaysFixedRegardlessOfRole(t *testing.T) {
	d, _, _ := writeAndReopen2(t, WriteOptions{
		Markdown:         "text\n",
		BodyEastAsiaFont: "NSimSun",
		CodeEastAsiaFont: "NSimSun",
	})
	ft, _ := d.Part(fontTablePart)
	if count := strings.Count(string(ft), `<w:font w:name="NSimSun">`); count != 1 {
		t.Fatalf("NSimSun appears %d times in fontTable.xml, want exactly 1:\n%s", count, ft)
	}
	entry := fontEntry(t, ft, "NSimSun")
	if !strings.Contains(entry, `<w:pitch w:val="fixed"/>`) {
		t.Errorf("shared, verified-fixed-pitch font entry = %s, want pitch=fixed", entry)
	}
}

// A font this package has NO verified pitch for at all (not even the
// role-based guess's usual tie-break material) still needs SOME resolution
// when the same unrecognized name is asked to serve both the body and the
// code role. "variable" is the chosen default for the same reason it is
// for 微软雅黑 above: a false "variable" only costs the code-alignment
// hint (no worse than this package's pre-existing no-fontTable-at-all
// behavior for that font), while a false "fixed" risks a monospace
// substitute landing on ordinary running text.
func TestWrite_FontTableSharedUnverifiedFontDefaultsToVariable(t *testing.T) {
	d, _, _ := writeAndReopen2(t, WriteOptions{
		Markdown:         "text\n",
		BodyEastAsiaFont: "Totally Unverified Shared Font",
		CodeEastAsiaFont: "Totally Unverified Shared Font",
	})
	ft, _ := d.Part(fontTablePart)
	if count := strings.Count(string(ft), `<w:font w:name="Totally Unverified Shared Font">`); count != 1 {
		t.Fatalf("font appears %d times in fontTable.xml, want exactly 1:\n%s", count, ft)
	}
	entry := fontEntry(t, ft, "Totally Unverified Shared Font")
	if !strings.Contains(entry, `<w:pitch w:val="variable"/>`) {
		t.Errorf("shared, unverified font entry = %s, want pitch=variable", entry)
	}
}

// A font name this package has no built-in metadata for must still get a
// usable entry -- not be silently omitted -- since even a generic
// charset/family/pitch hint is strictly more information than Word gets
// from no fontTable.xml at all. It must NOT get a fabricated <w:altName>:
// this package has no verified alternate name for a font it does not
// recognize, and inventing one would be worse than omitting it.
func TestWrite_FontTableUnknownFontGetsGenericEntryNoAltName(t *testing.T) {
	d, _, _ := writeAndReopen2(t, WriteOptions{
		Markdown:      "text\n",
		BodyLatinFont: "Totally Made Up Display Font",
	})
	ft, _ := d.Part(fontTablePart)
	entry := fontEntry(t, ft, "Totally Made Up Display Font")
	if !strings.Contains(entry, `<w:pitch w:val="variable"/>`) {
		t.Errorf("unknown body font entry = %s, want pitch=variable", entry)
	}
	// Checking for a non-empty, bucket-appropriate value, not merely that
	// the element tag is present: an entry with the right tags but an empty
	// w:val (e.g. `<w:charset w:val=""/>`) would still "have charset and
	// family present" while carrying no actual information, which is a
	// regression this test must not let through unnoticed. The font here
	// is BodyLatinFont, so bucket is "latin": charset "00" (ANSI/Western).
	if !strings.Contains(entry, `<w:charset w:val="00"/>`) {
		t.Errorf("unknown Latin-bucket font entry = %s, want charset w:val=\"00\"", entry)
	}
	if !strings.Contains(entry, `<w:family w:val="auto"/>`) {
		t.Errorf("unknown font entry = %s, want family w:val=\"auto\"", entry)
	}
	if strings.Contains(entry, `<w:altName`) {
		t.Errorf("unknown font entry = %s, must not fabricate an altName", entry)
	}
}

// altName is only emitted for a name this package has independently
// verified is a genuine alternate name for the SAME font (its English name
// where the primary is written in Chinese, matching how Word's own
// fontTable.xml records this -- see ECMA-376 17.8.3.1: altName is a name to
// try locating the SAME font under, not a different, substitute font).
func TestWrite_FontTableAltNameIsGenuineNamePairOnly(t *testing.T) {
	d, _, _ := writeAndReopen(t, "body text\n")
	ft, _ := d.Part(fontTablePart)
	entry := fontEntry(t, ft, "微软雅黑")
	if !strings.Contains(entry, `<w:altName w:val="Microsoft YaHei"/>`) {
		t.Errorf("微软雅黑 entry = %s, want altName Microsoft YaHei", entry)
	}
}

// The load-bearing safety property: fontTable.xml's own relationship id
// must draw from the SAME shared counter hyperlinks and the footer already
// use (renderCtx.nextRelID) -- a second, independent counter for it would
// be exactly the collision hazard addFooterRelID's doc comment warns about,
// just for a third part instead of a second. This is checked with several
// hyperlinks present (not zero), since a collision is invisible when there
// is nothing else drawing from the counter to collide with.
func TestWrite_FontTableRelIDStaysUniqueAlongsideHyperlinksAndFooter(t *testing.T) {
	md := "[one](https://example.com/1) [two](https://example.com/2) " +
		"[three](https://example.com/3) [four](https://example.com/4)\n"
	d, _, _ := writeAndReopen(t, md)
	doc, _ := d.Part(DocumentPart)
	s := string(doc)

	seen := map[string]string{}
	for _, m := range regexp.MustCompile(`<w:hyperlink r:id="(rId\d+)">`).FindAllStringSubmatch(s, -1) {
		seen[m[1]] = "a hyperlink"
	}
	if len(seen) != 4 {
		t.Fatalf("got %d distinct hyperlink ids, want 4", len(seen))
	}
	footerMatch := regexp.MustCompile(`<w:footerReference w:type="default" r:id="(rId\d+)"/>`).FindStringSubmatch(s)
	if footerMatch == nil {
		t.Fatal("no <w:footerReference> found")
	}
	if owner, ok := seen[footerMatch[1]]; ok {
		t.Fatalf("footer relationship id %s collides with %s", footerMatch[1], owner)
	}
	seen[footerMatch[1]] = "the footer"

	rels, _ := d.Part("word/_rels/document.xml.rels")
	fontTableMatch := regexp.MustCompile(`<Relationship Id="(rId\d+)" Type="http://schemas\.openxmlformats\.org/officeDocument/2006/relationships/fontTable"`).FindStringSubmatch(string(rels))
	if fontTableMatch == nil {
		t.Fatal("no fontTable relationship found in document.xml.rels")
	}
	if owner, ok := seen[fontTableMatch[1]]; ok {
		t.Fatalf("fontTable relationship id %s collides with %s", fontTableMatch[1], owner)
	}
	seen[fontTableMatch[1]] = "the fontTable"

	relsStr := string(rels)
	for id, owner := range seen {
		count := strings.Count(relsStr, `Id="`+id+`"`)
		if count != 1 {
			t.Errorf("relationship %s (%s) is declared %d times in document.xml.rels, want exactly 1", id, owner, count)
		}
	}
}

// The part's content must actually depend on the configured fonts, not be
// a constant blob written once and forgotten -- see this task's own
// self-review requirement. Two calls with different custom fonts must
// produce different fontTable.xml bytes.
func TestWrite_FontTableContentChangesWithConfiguredFonts(t *testing.T) {
	d1, _, _ := writeAndReopen2(t, WriteOptions{Markdown: "text\n", BodyLatinFont: "Arial"})
	d2, _, _ := writeAndReopen2(t, WriteOptions{Markdown: "text\n", BodyLatinFont: "Georgia"})
	ft1, _ := d1.Part(fontTablePart)
	ft2, _ := d2.Part(fontTablePart)
	if string(ft1) == string(ft2) {
		t.Error("fontTable.xml did not change when BodyLatinFont changed -- it looks like a constant blob")
	}
	if !strings.Contains(string(ft1), `w:name="Arial"`) {
		t.Errorf("fontTable.xml (Arial) = %s, want an Arial entry", ft1)
	}
	if !strings.Contains(string(ft2), `w:name="Georgia"`) {
		t.Errorf("fontTable.xml (Georgia) = %s, want a Georgia entry", ft2)
	}
}
