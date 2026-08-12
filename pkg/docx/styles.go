package docx

// This file is the single source of truth for word/styles.xml's visual
// properties, per the docx-style-architecture plan (docs/superpowers/plans/
// 2026-08-12-docx-style-architecture.md). It exists because putting every
// paragraph-level visual property inline in document.xml — the previous
// approach, still used by write.go's stylesPartXML/codeSpacingXML/etc. at
// the time this file was added — makes the same class of defect surface
// one property at a time: a real Word-authored styles.xml (see the plan's
// dissection of python-docx's default.docx) instead concentrates layout in
// named styles, so document.xml only ever writes a style reference.
//
// Task 1 (this file) only defines the style set and proves its internal
// consistency. Task 2 rewires write.go's renderer to reference these
// styles instead of writing inline <w:spacing>/<w:ind>/<w:shd>; until then
// buildStylesXML is exercised only by styles_test.go, and write.go keeps
// using its own stylesPartXML unchanged.
//
// Every style referenced by name anywhere below (an rStyle, a pStyle, or a
// basedOn/next target) MUST be defined by one of the w:style blocks in
// stylesXML, and allStyleIDs MUST list every styleId stylesXML defines —
// styles_test.go checks both directions mechanically. Word does not error
// on a dangling reference; it silently falls back to Normal, which is why
// this has to be enforced by a test rather than by inspection.

// Style IDs this package's generated documents use. write.go's Task 2
// change references these constants instead of literal strings.
const (
	StyleNormal        = "Normal"
	StyleHeading1      = "Heading1"
	StyleHeading2      = "Heading2"
	StyleHeading3      = "Heading3"
	StyleHeading4      = "Heading4"
	StyleHeading5      = "Heading5"
	StyleHeading6      = "Heading6"
	StyleSourceCode    = "SourceCode"
	StyleVerbatimChar  = "VerbatimChar"
	StyleQuote         = "Quote"
	StyleListParagraph = "ListParagraph"
	StyleTableGrid     = "TableGrid"
	StyleHyperlink     = "Hyperlink"
)

// allStyleIDs lists every styleId stylesXML defines. Kept as an explicit
// slice (rather than derived by parsing stylesXML back apart) so
// styles_test.go's TestStyles_AllReferencedStylesAreDefined has an
// independent list to check the XML against — a test that derived its
// expectation from the same string it is checking would be vacuous.
var allStyleIDs = []string{
	StyleNormal,
	StyleHeading1, StyleHeading2, StyleHeading3,
	StyleHeading4, StyleHeading5, StyleHeading6,
	StyleSourceCode,
	StyleVerbatimChar,
	StyleQuote,
	StyleListParagraph,
	StyleTableGrid,
	StyleHyperlink,
}

// buildStylesXML returns the complete word/styles.xml document: the
// docDefaults chain (must be <w:styles>'s first child; see
// TestStyles_DocDefaultsIsFirstChild) followed by every named style.
//
// Schema order matters and is not arbitrary — Word calls a
// mis-ordered styles part corrupt without saying why:
//   - <w:docDefaults>: <w:rPrDefault> before <w:pPrDefault>.
//   - Each <w:style>: name?, basedOn?, next?, qFormat?, pPr?, rPr?, tblPr?
//     (CT_Style's sequence — pPr before rPr before tblPr).
//   - Each <w:pPr>: ..., pBdr?, shd?, ..., spacing?, ind?,
//     contextualSpacing?, ..., jc?, ..., outlineLvl?, ... (CT_PPr's
//     sequence — spacing before ind before contextualSpacing, and
//     shading/borders before spacing).
//
// TestStyles_PPrChildrenAreInSchemaOrder and
// TestStyles_StyleChildrenAreInSchemaOrder check both orderings
// mechanically across every style this function emits, rather than
// trusting a one-off eyeball read.
func buildStylesXML() []byte {
	return []byte(generatedStylesXML)
}

// generatedStylesXML (not stylesXML — format_test.go already has an
// unrelated helper function of that name) is the full document this
// package's Task 2 change will hand to write.go.
const generatedStylesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	docDefaultsXML +
	normalStyleXML +
	heading1StyleXML +
	heading2StyleXML +
	heading3StyleXML +
	heading4StyleXML +
	heading5StyleXML +
	heading6StyleXML +
	sourceCodeStyleXML +
	verbatimCharStyleXML +
	quoteStyleXML +
	listParagraphStyleXML +
	tableGridStyleXML +
	hyperlinkStyleXML +
	`</w:styles>`

// docDefaultsXML mirrors testdata/structure.docx's own docDefaults (a real
// Word-authored file's defaults), matching stylesPartXML's docDefaults in
// write.go exactly. <w:docDefaults><w:spacing w:after="200" .../> is the
// root cause the plan's preamble traces all three visual defects to: every
// paragraph inherits 200 twips of trailing space unless a style clears it
// (SourceCode, ListParagraph) or a table style zeroes it for cell
// paragraphs (TableGrid) — see the doc comments on those styles below.
//
// docx_format's docDefaults patches (see format.go's planStylesPatches)
// require this exact chain to exist; commit fb3e09e fixed docx_write for
// having omitted it once already.
const docDefaultsXML = `<w:docDefaults><w:rPrDefault><w:rPr>` +
	`<w:rFonts w:asciiTheme="minorHAnsi" w:eastAsiaTheme="minorEastAsia" w:hAnsiTheme="minorHAnsi" w:cstheme="minorBidi"/>` +
	`<w:sz w:val="22"/><w:szCs w:val="22"/>` +
	`<w:lang w:val="en-US" w:eastAsia="en-US" w:bidi="ar-SA"/>` +
	`</w:rPr></w:rPrDefault>` +
	`<w:pPrDefault><w:pPr><w:spacing w:after="200" w:line="276" w:lineRule="auto"/></w:pPr></w:pPrDefault>` +
	`</w:docDefaults>`

// normalStyleXML is the baseline every other paragraph style basedOn's,
// directly or transitively. It deliberately carries no properties of its
// own beyond being the document default style — docDefaultsXML already
// supplies the body font/size/spacing every paragraph needs, so Normal
// does not repeat them.
const normalStyleXML = `<w:style w:type="paragraph" w:default="1" w:styleId="Normal">` +
	`<w:name w:val="Normal"/><w:qFormat/></w:style>`

// headingStyleXML: Heading1..Heading6 all basedOn Normal, all carry
// <w:keepNext/>, and font size decreases from Heading1 (32 half-points =
// 16pt) to Heading6 (20 half-points = 10pt). keepNext is required per the
// brief: without it a heading can strand at the bottom of a page while its
// body text flows to the next, which is what produced the large blank
// areas in the user's screenshots. Values match write.go's existing
// stylesPartXML so Task 2's swap-over changes nothing about how headings
// look.
const heading1StyleXML = `<w:style w:type="paragraph" w:styleId="Heading1">` +
	`<w:name w:val="heading 1"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/>` +
	`<w:pPr><w:keepNext/><w:spacing w:before="240" w:after="120"/><w:outlineLvl w:val="0"/></w:pPr>` +
	`<w:rPr><w:b/><w:sz w:val="32"/></w:rPr></w:style>`

const heading2StyleXML = `<w:style w:type="paragraph" w:styleId="Heading2">` +
	`<w:name w:val="heading 2"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/>` +
	`<w:pPr><w:keepNext/><w:spacing w:before="200" w:after="100"/><w:outlineLvl w:val="1"/></w:pPr>` +
	`<w:rPr><w:b/><w:sz w:val="28"/></w:rPr></w:style>`

const heading3StyleXML = `<w:style w:type="paragraph" w:styleId="Heading3">` +
	`<w:name w:val="heading 3"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/>` +
	`<w:pPr><w:keepNext/><w:spacing w:before="160" w:after="80"/><w:outlineLvl w:val="2"/></w:pPr>` +
	`<w:rPr><w:b/><w:sz w:val="26"/></w:rPr></w:style>`

const heading4StyleXML = `<w:style w:type="paragraph" w:styleId="Heading4">` +
	`<w:name w:val="heading 4"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/>` +
	`<w:pPr><w:keepNext/><w:spacing w:before="120" w:after="60"/><w:outlineLvl w:val="3"/></w:pPr>` +
	`<w:rPr><w:b/><w:sz w:val="24"/></w:rPr></w:style>`

const heading5StyleXML = `<w:style w:type="paragraph" w:styleId="Heading5">` +
	`<w:name w:val="heading 5"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/>` +
	`<w:pPr><w:keepNext/><w:spacing w:before="120" w:after="60"/><w:outlineLvl w:val="4"/></w:pPr>` +
	`<w:rPr><w:b/><w:sz w:val="22"/></w:rPr></w:style>`

const heading6StyleXML = `<w:style w:type="paragraph" w:styleId="Heading6">` +
	`<w:name w:val="heading 6"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/>` +
	`<w:pPr><w:keepNext/><w:spacing w:before="120" w:after="60"/><w:outlineLvl w:val="5"/></w:pPr>` +
	`<w:rPr><w:b/><w:sz w:val="20"/></w:rPr></w:style>`

// sourceCodeStyleXML is the fix for the striped-code-block defect. A
// fenced code block renders one paragraph per line (see write.go's
// paraBlock.isCode); without both zeroed spacing AND
// <w:contextualSpacing/>, each line inherits docDefaultsXML's 200-twip
// after-spacing, and the shading on each paragraph reads as a stack of
// separate bars rather than one contiguous block. contextualSpacing is
// what collapses the gap between adjacent paragraphs of the SAME style —
// zeroing spacing alone is not sufficient once a document's default line
// spacing is non-zero, so this style carries both per the brief.
//
// <w:ind w:left="120"/> is Task 2's addition on top of Task 1's original
// definition: write.go's pre-Task-2 codeIndentXML gave every fenced-code
// paragraph a modest left indent (one of write-quality-report's four
// defect fixes, "Defect 3", covered by
// TestWrite_CodeBlockHasAModestLeftIndent), and Task 1 omitted it from
// this style — an oversight caught only once write.go stopped writing the
// indent inline and started relying on this style for it instead. Without
// it, moving code blocks over to pStyle="SourceCode" would have silently
// dropped the indent rather than merely relocating it.
const sourceCodeStyleXML = `<w:style w:type="paragraph" w:styleId="SourceCode">` +
	`<w:name w:val="Source Code"/><w:basedOn w:val="Normal"/><w:qFormat/>` +
	`<w:pPr><w:shd w:val="clear" w:color="auto" w:fill="F5F5F5"/>` +
	`<w:spacing w:before="0" w:after="0" w:line="240" w:lineRule="auto"/>` +
	`<w:ind w:left="120"/>` +
	`<w:contextualSpacing/></w:pPr>` +
	`<w:rPr><w:rFonts w:ascii="Consolas" w:hAnsi="Consolas" w:cs="Consolas"/></w:rPr>` +
	`</w:style>`

// verbatimCharStyleXML is the character-level counterpart to SourceCode:
// inline code spans (`like this`) sit inside an ordinary body paragraph,
// so they need a run-level (not paragraph-level) style carrying the same
// monospace font.
const verbatimCharStyleXML = `<w:style w:type="character" w:styleId="VerbatimChar">` +
	`<w:name w:val="Verbatim Char"/>` +
	`<w:rPr><w:rFonts w:ascii="Consolas" w:hAnsi="Consolas" w:cs="Consolas"/></w:rPr>` +
	`</w:style>`

// quoteStyleXML renders a "> " blockquote line as an indented,
// left-bordered, italicized paragraph — the plan's style table's three
// properties for Quote, nothing more.
const quoteStyleXML = `<w:style w:type="paragraph" w:styleId="Quote">` +
	`<w:name w:val="Quote"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/>` +
	`<w:pPr><w:pBdr><w:left w:val="single" w:sz="12" w:space="8" w:color="7F7F7F"/></w:pBdr>` +
	`<w:ind w:left="360"/></w:pPr>` +
	`<w:rPr><w:i/></w:rPr></w:style>`

// listParagraphStyleXML is the fix for the gaps-between-list-items
// defect, the same mechanism as SourceCode: <w:contextualSpacing/>
// collapses the space docDefaultsXML would otherwise insert between
// consecutive list-item paragraphs of this same style. w:ind left="720"
// (half an inch) is the indent level-0 list items sit at; nested levels
// are handled by numbering.xml's own indents, not this style.
const listParagraphStyleXML = `<w:style w:type="paragraph" w:styleId="ListParagraph">` +
	`<w:name w:val="List Paragraph"/><w:basedOn w:val="Normal"/><w:qFormat/>` +
	`<w:pPr><w:ind w:left="720"/><w:contextualSpacing/></w:pPr>` +
	`</w:style>`

// tableGridStyleXML is the fix for the tall-table-row defect. It MUST be
// w:type="table" (not paragraph) — only a table-type style can carry both
// <w:tblPr> (the borders) and <w:pPr> (spacing for the paragraphs living
// inside each cell) in the one style, so a table that references it via
// <w:tblPr><w:tblStyle w:val="TableGrid"/></w:tblPr> gets zeroed
// cell-paragraph spacing for free, exactly like a real Word document's
// built-in "Table Grid" style. Border values (single, sz=4, color=auto,
// space=0) match Word's own TableGrid.
const tableGridStyleXML = `<w:style w:type="table" w:styleId="TableGrid">` +
	`<w:name w:val="Table Grid"/>` +
	`<w:pPr><w:spacing w:after="0" w:line="240" w:lineRule="auto"/></w:pPr>` +
	`<w:tblPr><w:tblBorders>` +
	`<w:top w:val="single" w:sz="4" w:space="0" w:color="auto"/>` +
	`<w:left w:val="single" w:sz="4" w:space="0" w:color="auto"/>` +
	`<w:bottom w:val="single" w:sz="4" w:space="0" w:color="auto"/>` +
	`<w:right w:val="single" w:sz="4" w:space="0" w:color="auto"/>` +
	`<w:insideH w:val="single" w:sz="4" w:space="0" w:color="auto"/>` +
	`<w:insideV w:val="single" w:sz="4" w:space="0" w:color="auto"/>` +
	`</w:tblBorders></w:tblPr>` +
	`</w:style>`

// hyperlinkStyleXML matches write.go's existing Hyperlink definition
// exactly: a character style a run picks up via <w:rStyle> inside its own
// <w:rPr>, not via <w:pStyle> on the paragraph.
const hyperlinkStyleXML = `<w:style w:type="character" w:styleId="Hyperlink">` +
	`<w:name w:val="Hyperlink"/>` +
	`<w:rPr><w:color w:val="0563C1"/><w:u w:val="single"/></w:rPr></w:style>`
