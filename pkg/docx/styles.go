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
	StyleBodyText      = "BodyText"
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
	StyleBodyText,
	StyleHeading1, StyleHeading2, StyleHeading3,
	StyleHeading4, StyleHeading5, StyleHeading6,
	StyleSourceCode,
	StyleVerbatimChar,
	StyleQuote,
	StyleListParagraph,
	StyleTableGrid,
	StyleHyperlink,
}

// fontOptions is the resolved (always-non-empty) set of literal font names
// one WriteDocx call bakes into styles.xml. bodyLatin/bodyEastAsia land in
// docDefaultsXML, which is why every style below EXCEPT SourceCode/
// VerbatimChar inherits them without declaring any <w:rFonts> of its own
// (Normal and, transitively, everything basedOn Normal — see Normal's own
// doc comment). codeLatin/codeEastAsia land in sourceCodeStyleXML/
// verbatimCharStyleXML and, on write.go's side, renderCtx.codeFontXML's
// direct-<w:rFonts> fallback for the one case a shared style cannot cover.
//
// resolveFontOptions is the only place that produces a fontOptions from a
// caller-facing WriteOptions; every other function that takes one as a
// parameter assumes all four fields are already non-empty.
type fontOptions struct {
	bodyLatin    string
	bodyEastAsia string
	codeLatin    string
	codeEastAsia string
}

// defaultBodyLatinFont/defaultBodyEastAsiaFont are copied verbatim from the
// docx-chinese-typography plan's reference document's own docDefaults (see
// docDefaultsXML's doc comment for the full provenance).
const (
	defaultBodyLatinFont    = "Calibri"
	defaultBodyEastAsiaFont = "微软雅黑"
	// defaultCodeLatinFont is this package's own pre-typography-task choice
	// for the code block's Latin half, unchanged by this task -- Consolas is
	// a monospace font readers already associate with source code.
	defaultCodeLatinFont = "Consolas"
)

// defaultCodeEastAsiaFont is the code block's East Asian font whenever
// WriteOptions.CodeEastAsiaFont is left empty. It is 微软雅黑 (Microsoft
// YaHei) — deliberately NOT NSimSun, which is what an earlier pass at this
// task chose and which this comment replaces.
//
// That earlier choice was reasoned, not measured: "Office for Mac ships
// NSimSun" was never checked against a real machine. It was checked here,
// on the machine of the person who reported the box-drawing misalignment
// this font exists to help with: system_profiler on that Mac lists 微软雅黑,
// 宋体-简, 黑体-简, and 华文宋体 as installed fonts. NSimSun is not among
// them. Shipping NSimSun as the default would have "fixed" the defect only
// on paper — on the one machine that reported it, Word would have silently
// substituted an ordinary proportional font for the missing NSimSun, so the
// fix would never actually have rendered for the person it was written
// for. 微软雅黑 is used instead because it is actually installed here and on
// Windows, and because it is the same face the reference document
// (.superpowers/sdd/reference-values.md) uses for ordinary body text.
//
// What this default does NOT achieve, and must not be described as fixing:
// exact alignment of ASCII box-drawing characters (│ ├ ─ └) against Chinese
// text on the same monospace grid. That needs the Latin glyph's advance
// width to be exactly HALF the CJK glyph's — a strict 2:1 ratio. 微软雅黑
// does not have that ratio (it is an ordinary proportional UI font), and
// neither does pairing Consolas (defaultCodeLatinFont) against any ordinary
// CJK font: Consolas's own advance is 0.55em against a 1.0em full-width CJK
// character, so doubling it (1.1em) already overshoots by about 10% per
// character, and that error accumulates across a line — a mixed
// ASCII/Chinese diagram still visibly drifts under this default. Fonts that
// actually hold a 2:1 ratio across their whole character set — NSimSun, MS
// Gothic, Sarasa Gothic (更纱黑体), Noto Sans Mono CJK — are not preinstalled
// on macOS (NSimSun/MS Gothic ship with Windows; the other two need a
// manual install on either OS). WriteOptions.CodeEastAsiaFont exists so a
// document author targeting Windows readers, or with one of those fonts
// installed locally, can opt into exact alignment — this default cannot
// provide it by itself.
const defaultCodeEastAsiaFont = "微软雅黑"

// defaultFontOptions returns this package's four built-in font choices,
// used both as resolveFontOptions' fallback for whatever WriteOptions field
// the caller left empty, and directly by every pre-existing zero-argument
// buildStylesXML()/test call site in this package that predates fonts being
// configurable at all.
func defaultFontOptions() fontOptions {
	return fontOptions{
		bodyLatin:    defaultBodyLatinFont,
		bodyEastAsia: defaultBodyEastAsiaFont,
		codeLatin:    defaultCodeLatinFont,
		codeEastAsia: defaultCodeEastAsiaFont,
	}
}

// resolveFontOptions fills WriteOptions' four font fields with this
// package's defaults (defaultFontOptions) wherever the caller left them
// empty -- see WriteOptions' own doc comment for what each field controls.
// Every consumer of a fontOptions value downstream (docDefaultsXML,
// sourceCodeStyleXML, verbatimCharStyleXML, renderCtx.codeFontXML) assumes
// this resolution already happened and never itself checks for "".
func resolveFontOptions(o WriteOptions) fontOptions {
	f := defaultFontOptions()
	if o.BodyLatinFont != "" {
		f.bodyLatin = o.BodyLatinFont
	}
	if o.BodyEastAsiaFont != "" {
		f.bodyEastAsia = o.BodyEastAsiaFont
	}
	if o.CodeLatinFont != "" {
		f.codeLatin = o.CodeLatinFont
	}
	if o.CodeEastAsiaFont != "" {
		f.codeEastAsia = o.CodeEastAsiaFont
	}
	return f
}

// buildStylesXML returns the complete word/styles.xml document using this
// package's own default fonts (defaultFontOptions) -- see
// buildStylesXMLWithFonts for the parameterized form write.go's WriteDocx
// actually calls. Kept as the zero-argument form because every test in this
// package written before fonts became configurable calls it this way, and
// none of them need a custom font to make their point.
//
// The docDefaults chain (must be <w:styles>'s first child; see
// TestStyles_DocDefaultsIsFirstChild) comes first, followed by every named
// style.
//
// Schema order matters and is not arbitrary — Word calls a
// mis-ordered styles part corrupt without saying why:
//   - <w:docDefaults>: <w:rPrDefault> before <w:pPrDefault>.
//   - Each <w:style>: name?, basedOn?, next?, qFormat?, pPr?, rPr?, tblPr?
//     (CT_Style's sequence — pPr before rPr before tblPr before
//     tblStylePr).
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
	return buildStylesXMLWithFonts(defaultFontOptions())
}

// buildStylesXMLWithFonts is buildStylesXML's parameterized form: f's four
// resolved font names land in docDefaultsXML (body Latin/East Asian) and in
// sourceCodeStyleXML/verbatimCharStyleXML (the code block's own Latin/East
// Asian pair). Every other style below is font-independent — in
// particular, none of the six heading styles carry a <w:rFonts> of their
// own, so they inherit docDefaultsXML's pair through Normal exactly like an
// ordinary body paragraph does.
func buildStylesXMLWithFonts(f fontOptions) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		docDefaultsXML(f) +
		normalStyleXML +
		bodyTextStyleXML +
		heading1StyleXML +
		heading2StyleXML +
		heading3StyleXML +
		heading4StyleXML +
		heading5StyleXML +
		heading6StyleXML +
		sourceCodeStyleXML(f) +
		verbatimCharStyleXML(f) +
		quoteStyleXML +
		listParagraphStyleXML +
		tableGridStyleXML +
		hyperlinkStyleXML +
		`</w:styles>`)
}

// docDefaultsXML's rPrDefault font/size defaults are copied verbatim from
// the docx-chinese-typography plan's reference document (a real,
// professional Chinese business document the user judged our previous
// output against unfavorably) — see .superpowers/sdd/reference-values.md,
// which was extracted by measuring that file's actual XML: <w:rFonts
// w:ascii="Calibri" w:eastAsia="微软雅黑"/> appears there 682 times, and its
// default run size is 21 half-points (10.5pt), the conventional Chinese
// body size. f.bodyLatin/f.bodyEastAsia are those two literal font names,
// now configurable via WriteOptions (resolveFontOptions) rather than fixed
// -- Calibri/微软雅黑 remain the DEFAULT (defaultFontOptions), just no
// longer the only possible value. Neither is a theme reference
// (w:asciiTheme/w:eastAsiaTheme/w:hAnsiTheme/w:cstheme, this function's own
// pre-typography-task value) and neither carries hAnsi/cs attributes either
// — the reference's own rFonts never carries either, so this doesn't
// either. Literal beats theme in Word's own resolution order, which is also
// why format.go's rFontsLiteralAttrs strips *Theme attributes whenever a
// literal font is set (see TestFormat_HeadingFontRemovesThemeAttributes'
// doc comment: a literal font added BESIDE a theme one is ignored, the
// theme wins) — carrying no Theme attributes at all here avoids that trap
// entirely rather than relying on it being harmless.
//
// <w:docDefaults><w:spacing w:after="200" .../> is the root cause the plan's
// preamble traces all three visual defects to: every paragraph inherits 200
// twips of trailing space unless a style clears it (SourceCode,
// ListParagraph) or a table style zeroes it for cell paragraphs
// (TableGrid) — see the doc comments on those styles below.
//
// docx_format's docDefaults patches (see format.go's planStylesPatches)
// require this exact chain to exist; commit fb3e09e fixed docx_write for
// having omitted it once already. Those patches rewrite whatever literal
// font f already put here, so they are unaffected by f being configurable
// now instead of fixed.
//
// <w:lang w:eastAsia="zh-CN"> (fixed from a prior "en-US") is what Word
// actually reads for East Asian line-breaking rules (禁则 -- which
// punctuation may not start/end a line) and proofing, completely
// independently of any font choice: w:eastAsia here names a LANGUAGE, not a
// font, and "en-US" describing East Asian text was simply wrong for a
// generator whose entire reason for existing (the docx-chinese-typography
// plan) is Chinese documents. w:val stays "en-US" (this package's body text
// is authored/reviewed in English even when the East Asian glyphs it embeds
// are Chinese; changing it would be a different, unrelated claim about the
// Latin-script language) and w:bidi stays "ar-SA" (Word's own stock
// default for the field, present before this task and not something this
// task's bug report is about).
//
// This is a constant, not threaded through fontOptions/WriteOptions the way
// BodyEastAsiaFont is: unlike a font name, this package has no per-caller
// signal to derive an East Asian language from. A caller CAN already set
// BodyEastAsiaFont to a non-Chinese CJK face (e.g. "MS Gothic" for
// Japanese, per that field's own doc comment), and "zh-CN" would then be
// the wrong proofing language for the SAME reason "en-US" is wrong today --
// but guessing a language from a font family name is unreliable (many CJK
// font names are ambiguous or shared across locales), and adding a real
// WriteOptions.EastAsiaLanguage field is a bigger, separate feature (new
// option, new resolution/defaulting logic, its own tests) than this task's
// mandate. Fixing the concrete, always-wrong-for-this-package's-actual-
// output default (en-US on Chinese text) now, while leaving fine-grained
// per-caller control as a follow-up if it's ever needed, is the narrower
// change; see this task's report for the fuller argument either way.
func docDefaultsXML(f fontOptions) string {
	return `<w:docDefaults><w:rPrDefault><w:rPr>` +
		`<w:rFonts w:ascii="` + f.bodyLatin + `" w:eastAsia="` + f.bodyEastAsia + `"/>` +
		`<w:sz w:val="21"/><w:szCs w:val="21"/>` +
		`<w:lang w:val="en-US" w:eastAsia="zh-CN" w:bidi="ar-SA"/>` +
		`</w:rPr></w:rPrDefault>` +
		`<w:pPrDefault><w:pPr><w:spacing w:after="200" w:line="276" w:lineRule="auto"/></w:pPr></w:pPrDefault>` +
		`</w:docDefaults>`
}

// normalStyleXML is the baseline every other paragraph style basedOn's,
// directly or transitively. It deliberately carries no properties of its
// own beyond being the document default style — docDefaultsXML already
// supplies the body font/size/spacing every paragraph needs, so Normal
// does not repeat them.
const normalStyleXML = `<w:style w:type="paragraph" w:default="1" w:styleId="Normal">` +
	`<w:name w:val="Normal"/><w:qFormat/></w:style>`

// bodyTextStyleXML is Task 2 of the docx-chinese-typography plan (段落排版):
// the reference document's "正文段落" row, copied verbatim --
// <w:spacing w:after="120" w:line="360" w:lineRule="auto"/> (1.5x line
// spacing) followed by <w:ind w:firstLine="420"/> (a two-character
// first-line indent, the standard opening of a Chinese paragraph).
//
// This is deliberately its OWN style, basedOn Normal, rather than being
// folded into Normal itself. Every other paragraph style in this file
// (SourceCode, ListParagraph, Quote) is ALSO basedOn Normal, and so is an
// unstyled table-cell paragraph (see write.go's paraBlock.isCell) -- had
// the first-line indent instead been added directly to Normal's own pPr,
// all four would have silently inherited a two-character indent that makes
// no sense for a code line, a list bullet, a block quote, or a table cell
// (and, for a fenced code block, would have re-broken the ASCII-diagram
// alignment Task 1 just fixed). Putting it in a sibling style that only
// write.go's ordinary-paragraph branch references keeps Normal itself, and
// everything that stays on Normal, completely untouched -- see
// TestType_FirstLineIndentDoesNotLeakIntoOtherBlocks, which pins exactly
// that for all four cases directly instead of trusting this reasoning by
// eyeball.
const bodyTextStyleXML = `<w:style w:type="paragraph" w:styleId="BodyText">` +
	`<w:name w:val="Body Text"/><w:basedOn w:val="Normal"/><w:qFormat/>` +
	`<w:pPr><w:spacing w:after="120" w:line="360" w:lineRule="auto"/><w:ind w:firstLine="420"/></w:pPr>` +
	`</w:style>`

// headingStyleXML: Heading1..Heading6 all basedOn Normal and all carry
// <w:keepNext/> (required per the brief: without it a heading can strand at
// the bottom of a page while its body text flows to the next, which is what
// produced the large blank areas in the user's screenshots). keepNext is
// this package's own pre-typography-task choice; color, size, AND spacing
// are now all copied verbatim from the typography plan's reference document
// (.superpowers/sdd/reference-values.md) instead of chosen independently.
//
// Color/size: the reference does NOT step the font size down through all
// six levels the way this style set previously did (28/26/24/22/20). It
// only grows H1-H3 (32/26/24 half-points) and alternates two blues across
// all six (2E74B5 for H1/H2/H4/H5, 1F4D78 for H3/H6); H4-H6 carry NO
// <w:sz> at all and so inherit Normal's body size, distinguished from body
// text only by color, bold, and (H4 only) italic.
//
// Spacing: the reference's "段落间距" table measures ONE heading spacing
// rule (before=240, after=160, line=360/1.5x), not a different rule per
// level -- unlike color/size, spacing does not step down by level. That
// single rule replaces this package's previous stepped before/after values
// (240/120, 200/100, 160/80, 120/60 x3, no line spacing at all) uniformly
// across H1-H6; see TestType_HeadingSpacingMatchesReference.
//
// Copying this scheme rather than inventing a new one is deliberate: a
// flatter, color-led hierarchy for the deeper levels, with one shared
// spacing rule, is what the reference (which the user called professional)
// actually does.
const headingSpacingXML = `<w:spacing w:before="240" w:after="160" w:line="360" w:lineRule="auto"/>`

const heading1StyleXML = `<w:style w:type="paragraph" w:styleId="Heading1">` +
	`<w:name w:val="heading 1"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/>` +
	`<w:pPr><w:keepNext/>` + headingSpacingXML + `<w:outlineLvl w:val="0"/></w:pPr>` +
	`<w:rPr><w:b/><w:color w:val="2E74B5"/><w:sz w:val="32"/></w:rPr></w:style>`

const heading2StyleXML = `<w:style w:type="paragraph" w:styleId="Heading2">` +
	`<w:name w:val="heading 2"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/>` +
	`<w:pPr><w:keepNext/>` + headingSpacingXML + `<w:outlineLvl w:val="1"/></w:pPr>` +
	`<w:rPr><w:b/><w:color w:val="2E74B5"/><w:sz w:val="26"/></w:rPr></w:style>`

const heading3StyleXML = `<w:style w:type="paragraph" w:styleId="Heading3">` +
	`<w:name w:val="heading 3"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/>` +
	`<w:pPr><w:keepNext/>` + headingSpacingXML + `<w:outlineLvl w:val="2"/></w:pPr>` +
	`<w:rPr><w:b/><w:color w:val="1F4D78"/><w:sz w:val="24"/></w:rPr></w:style>`

const heading4StyleXML = `<w:style w:type="paragraph" w:styleId="Heading4">` +
	`<w:name w:val="heading 4"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/>` +
	`<w:pPr><w:keepNext/>` + headingSpacingXML + `<w:outlineLvl w:val="3"/></w:pPr>` +
	`<w:rPr><w:b/><w:i/><w:color w:val="2E74B5"/></w:rPr></w:style>`

const heading5StyleXML = `<w:style w:type="paragraph" w:styleId="Heading5">` +
	`<w:name w:val="heading 5"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/>` +
	`<w:pPr><w:keepNext/>` + headingSpacingXML + `<w:outlineLvl w:val="4"/></w:pPr>` +
	`<w:rPr><w:b/><w:color w:val="2E74B5"/></w:rPr></w:style>`

const heading6StyleXML = `<w:style w:type="paragraph" w:styleId="Heading6">` +
	`<w:name w:val="heading 6"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/>` +
	`<w:pPr><w:keepNext/>` + headingSpacingXML + `<w:outlineLvl w:val="5"/></w:pPr>` +
	`<w:rPr><w:b/><w:color w:val="1F4D78"/></w:rPr></w:style>`

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
//
// <w:keepNext/><w:keepLines/> and <w:pBdr> are this task's own addition
// (the code-block-report's "Defect 2": a code block was a run of shaded
// paragraphs with no visible container -- Word draws paragraph shading
// edge to edge with no padding, so the code sat flush against the
// surrounding text). Two containers were considered: a single-cell table
// (the technique real Word documents commonly use, since a cell supplies a
// border, background AND internal margins for free), and a paragraph
// border. The table was rejected specifically for THIS codebase: wrapping
// every code line in a <w:tc> would make docx_read's scanner report each
// one as a table cell (Para.Cell != nil), which read.go's renderReadPara
// cannot be taught to treat differently (it is on this task's forbidden-
// to-modify list, same as scan.go) -- every code block would start
// rendering as "(table N row 1 col 1) ..." in Read's markdown output and
// trip its table-structure note, for a document that has no real table at
// all. A table would also shift every REAL data table after it to a
// higher table index, a second, independent behavior change with its own
// blast radius. A paragraph border pays for none of that: Word merges the
// borders of contiguous paragraphs that share byte-identical <w:pBdr> XML
// into a single box around the whole run (the same mechanism "Borders and
// Shading" applied to a multi-paragraph selection produces), so this one
// style-level property still yields ONE bordered box around a multi-line
// code block, not a border around every individual line. w:space on each
// side is what turns the border into real padding rather than a second
// line flush against the text: 4pt top/bottom (the gap above the first
// line and below the last, since Word merges the interior lines' borders
// away) and 8pt left/right (applied to every line, since a vertical border
// spans the whole box). keepNext/keepLines is the paragraph-only answer to
// "keeps together sensibly across pages": the same chaining mechanism
// heading styles above already use for the identical reason (a heading
// stranding at the bottom of a page while its body flows to the next) --
// consecutive keepNext paragraphs pull each other onto the same page
// wherever Word can manage it.
//
// f.codeLatin/f.codeEastAsia (resolved from WriteOptions.CodeLatinFont/
// CodeEastAsiaFont, defaulting to Consolas/微软雅黑 — see
// defaultCodeEastAsiaFont's doc comment for what that default does and does
// not achieve) replace what used to be a fixed pair here.
func sourceCodeStyleXML(f fontOptions) string {
	return `<w:style w:type="paragraph" w:styleId="SourceCode">` +
		`<w:name w:val="Source Code"/><w:basedOn w:val="Normal"/><w:qFormat/>` +
		`<w:pPr><w:keepNext/><w:keepLines/>` +
		`<w:pBdr>` +
		`<w:top w:val="single" w:sz="4" w:space="4" w:color="BFBFBF"/>` +
		`<w:left w:val="single" w:sz="4" w:space="8" w:color="BFBFBF"/>` +
		`<w:bottom w:val="single" w:sz="4" w:space="4" w:color="BFBFBF"/>` +
		`<w:right w:val="single" w:sz="4" w:space="8" w:color="BFBFBF"/>` +
		`</w:pBdr>` +
		`<w:shd w:val="clear" w:color="auto" w:fill="F5F5F5"/>` +
		`<w:spacing w:before="0" w:after="0" w:line="240" w:lineRule="auto"/>` +
		`<w:ind w:left="120"/>` +
		`<w:contextualSpacing/></w:pPr>` +
		`<w:rPr><w:rFonts w:ascii="` + f.codeLatin + `" w:eastAsia="` + f.codeEastAsia + `" w:hAnsi="` + f.codeLatin + `" w:cs="` + f.codeLatin + `"/></w:rPr>` +
		`</w:style>`
}

// verbatimCharStyleXML is the character-level counterpart to SourceCode:
// inline code spans (`like this`) sit inside an ordinary body paragraph,
// so they need a run-level (not paragraph-level) style carrying the same
// monospace font. f is the same fontOptions sourceCodeStyleXML takes, and
// for the same reason: the two styles must always agree on the code font,
// or inline code and a fenced code block would render in different fonts
// within the same document.
func verbatimCharStyleXML(f fontOptions) string {
	return `<w:style w:type="character" w:styleId="VerbatimChar">` +
		`<w:name w:val="Verbatim Char"/>` +
		`<w:rPr><w:rFonts w:ascii="` + f.codeLatin + `" w:eastAsia="` + f.codeEastAsia + `" w:hAnsi="` + f.codeLatin + `" w:cs="` + f.codeLatin + `"/></w:rPr>` +
		`</w:style>`
}

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
//
// <w:pPr>'s own <w:spacing> is Task 2 of the docx-chinese-typography plan's
// "表格单元格" row, copied verbatim from the reference
// (.superpowers/sdd/reference-values.md): after="0", line="260" — tighter
// than BodyText's 1.5x line spacing (line="360"), which is the point: a
// wide table with 1.5x-spaced cells would be needlessly tall. This is the
// ONLY place table-cell spacing is declared — a cell's own <w:p> carries no
// pStyle at all (see write.go's paraBlock.isCell), so it resolves straight
// to Normal, and Word still applies THIS style's <w:pPr> to it because the
// paragraph lives inside a table using this table style (see
// tableTblPrXML's doc comment).
//
// <w:tblCellMar> (inside <w:tblPr>, per Task 3's "表格" row) gives every
// cell 60 twips top/bottom and 100 twips left/right of inner margin, copied
// verbatim from the reference — without it, cell text sits flush against
// the border, which is the "文字贴着框线" defect the plan calls out.
//
// <w:tblStylePr w:type="firstRow"> is Task 3's header-row shading (DDE5F0)
// and bold — deliberately NOT a per-cell inline <w:shd> on the header row's
// <w:tc> elements in document.xml. This project's core invariant (see
// TestWrite_NoInlineVisualPropertiesInDocumentXML) bans inline
// <w:spacing>/<w:ind>/<w:shd> in document.xml; an inline header shd would
// violate it the same way an inline paragraph shd would. A table style's
// conditional formatting is how Word's OWN built-in table styles shade a
// header row — see CT_TblStylePr — so referencing it keeps the shading (and
// the bold) entirely inside styles.xml, one styleId lookup away from being
// changed for every table in the document at once. This only takes effect
// when the TABLE ITSELF also sets <w:tblLook w:firstRow="1" .../> (see
// write.go's tableTblPrXML) — a table style carrying tblStylePr with no
// tblLook enabling it is silently inert, which is why
// TestType_TableLookActivatesFirstRowShading checks both sides together
// rather than the style alone.
const tableGridStyleXML = `<w:style w:type="table" w:styleId="TableGrid">` +
	`<w:name w:val="Table Grid"/>` +
	`<w:pPr><w:spacing w:after="0" w:line="260"/></w:pPr>` +
	`<w:tblPr><w:tblBorders>` +
	`<w:top w:val="single" w:sz="4" w:space="0" w:color="auto"/>` +
	`<w:left w:val="single" w:sz="4" w:space="0" w:color="auto"/>` +
	`<w:bottom w:val="single" w:sz="4" w:space="0" w:color="auto"/>` +
	`<w:right w:val="single" w:sz="4" w:space="0" w:color="auto"/>` +
	`<w:insideH w:val="single" w:sz="4" w:space="0" w:color="auto"/>` +
	`<w:insideV w:val="single" w:sz="4" w:space="0" w:color="auto"/>` +
	`</w:tblBorders>` +
	`<w:tblCellMar>` +
	`<w:top w:w="60" w:type="dxa"/><w:left w:w="100" w:type="dxa"/>` +
	`<w:bottom w:w="60" w:type="dxa"/><w:right w:w="100" w:type="dxa"/>` +
	`</w:tblCellMar>` +
	`</w:tblPr>` +
	`<w:tblStylePr w:type="firstRow">` +
	`<w:rPr><w:b/></w:rPr>` +
	`<w:tcPr><w:shd w:val="clear" w:fill="DDE5F0"/></w:tcPr>` +
	`</w:tblStylePr>` +
	`</w:style>`

// hyperlinkStyleXML matches write.go's existing Hyperlink definition
// exactly: a character style a run picks up via <w:rStyle> inside its own
// <w:rPr>, not via <w:pStyle> on the paragraph.
const hyperlinkStyleXML = `<w:style w:type="character" w:styleId="Hyperlink">` +
	`<w:name w:val="Hyperlink"/>` +
	`<w:rPr><w:color w:val="0563C1"/><w:u w:val="single"/></w:rPr></w:style>`
