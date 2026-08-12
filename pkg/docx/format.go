package docx

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
)

// FormatOptions requests document-wide formatting changes: fonts, sizes,
// line spacing, alignment, margins, and collapsing runs of empty
// paragraphs. See Document.Format's doc comment for how each field maps
// onto the underlying XML (the "landing points" table in the design brief).
type FormatOptions struct {
	// Template names a formatting preset ("corporate", "academic",
	// "minimal") that is expanded into the fields below before anything
	// else is applied. Any field the caller also sets explicitly overrides
	// the template's value for that field; "" means no template.
	Template string
	// HeadingFont, if non-"", replaces Heading1..Heading9's font, landing on
	// each heading style's <w:rPr><w:rFonts> in styles.xml. It removes the
	// *Theme attributes Word would otherwise prefer over a literal font
	// name.
	HeadingFont string
	// BodyFont, if non-"", replaces the document's default font, landing on
	// styles.xml's <w:docDefaults><w:rPrDefault><w:rPr><w:rFonts> (the
	// fixture's Normal style carries no rPr of its own, so the cascade's
	// real body-font source is docDefaults, not Normal).
	BodyFont string
	// BodySizePt, if non-zero, replaces the document's default font size in
	// points, landing on docDefaults's <w:sz> AND <w:szCs> (kept in sync so
	// CJK/complex-script runs don't keep the old size).
	BodySizePt float64
	// LineSpacing, if non-zero, is a multiple of a single line (1.0, 1.15,
	// 2.0, ...), landing on docDefaults's <w:pPrDefault><w:pPr><w:spacing>
	// as w:line (240ths of a line) with w:lineRule="auto".
	LineSpacing float64
	// Align, if non-"", is "left" or "justify", landing on docDefaults's
	// <w:pPrDefault><w:pPr><w:jc>.
	Align string
	// MarginsMM, if non-nil, must have exactly 4 values: top, right,
	// bottom, left, in millimeters. It lands on word/document.xml's
	// <w:sectPr><w:pgMar> (every one found, in case of multiple sections),
	// converted to twips (1440 per inch). nil means "leave margins alone".
	MarginsMM []float64
	// Normalize collapses runs of two or more consecutive empty paragraphs
	// down to one, in word/document.xml. This is the ONLY FormatOptions
	// field allowed to change paragraph count/text — see Document.Format's
	// promise that formatting never touches body text otherwise. It does
	// NOT normalize punctuation spacing; see FormatResult.Notes.
	Normalize bool
	// StartPara, if > 0, switches BodyFont/BodySizePt/LineSpacing/Align
	// from the whole-document path above to DIRECT FORMATTING scoped to
	// paragraphs [StartPara, EndPara] (1-based, inclusive) of
	// word/document.xml: font/size land on each targeted run's own
	// <w:rPr>, and line spacing/alignment land on each targeted
	// paragraph's own <w:pPr> — direct formatting, which outranks
	// whatever word/styles.xml says, exactly what a user gets by
	// selecting text in Word and setting a size. 0 (the zero value) means
	// "whole document", the original, unchanged behavior — see
	// formatDirectRange for the range path and its own doc comment for
	// which fields it rejects outright (Template, HeadingFont, MarginsMM,
	// Normalize all only make sense document-wide).
	StartPara int
	// EndPara, if StartPara > 0 and EndPara == 0, defaults to StartPara
	// (formatting exactly that one paragraph). Setting EndPara while
	// StartPara is 0 is an error — there is no range to end. 1-based,
	// inclusive.
	EndPara int
}

// FormatResult reports what Document.Format actually changed.
type FormatResult struct {
	// Applied is a human-readable list of changes that were made, e.g.
	// "body font -> Georgia". Empty when nothing in FormatOptions requested
	// a change.
	Applied []string
	// Notes carries caveats a caller should know about even though nothing
	// went wrong: most notably, that Normalize's punctuation-spacing pass is
	// out of scope for this task and was not performed.
	Notes []string
}

// formatTemplates holds the P2a-brief's three named presets, already
// expanded into concrete field values. minimal deliberately leaves BodyFont
// and BodySizePt at their zero values ("" and 0), which Format's "zero
// means unset" convention treats as "leave the body font/size alone" — that
// IS "minimal"'s stated behavior ("body 保持不动").
var formatTemplates = map[string]FormatOptions{
	"corporate": {
		BodyFont:    "Calibri",
		BodySizePt:  11,
		LineSpacing: 1.15,
		Align:       "justify",
		MarginsMM:   []float64{25.4, 25.4, 25.4, 25.4},
	},
	"academic": {
		BodyFont:    "Times New Roman",
		BodySizePt:  12,
		LineSpacing: 2.0,
		Align:       "left",
		MarginsMM:   []float64{25.4, 25.4, 25.4, 25.4},
	},
	"minimal": {
		LineSpacing: 1.0,
		Align:       "left",
		MarginsMM:   []float64{20, 20, 20, 20},
	},
}

// headingStyleIDRe matches the w:styleId values Format treats as headings:
// Heading1 through Heading9, the range the fixture's styles.xml actually
// defines.
var headingStyleIDRe = regexp.MustCompile(`^Heading[1-9]$`)

// twipsPerMM converts millimeters to twips (1440 twips per inch, 25.4mm per
// inch).
const twipsPerMM = 1440.0 / 25.4

// Part returns the named zip entry's current content. It delegates to the
// underlying Package, aliasing its internal storage exactly as
// Package.Part does (see that method's doc comment on the aliasing
// hazard): the slice is only valid until the next SetPart on the same
// name.
func (d *Document) Part(name string) ([]byte, bool) {
	return d.pkg.Part(name)
}

// SetPart replaces the named zip entry's content and marks the document
// modified. When name is DocumentPart, it also re-runs rescan so Paras()
// and TotalParas() reflect the new content immediately, the same guarantee
// Edit already gives callers for its own document.xml changes.
func (d *Document) SetPart(name string, data []byte) error {
	if err := d.pkg.SetPart(name, data); err != nil {
		return err
	}
	d.modified = true
	if name == DocumentPart {
		return d.rescan()
	}
	return nil
}

// Format applies FormatOptions to the document. Everything it touches is a
// narrow byte-range splice against word/styles.xml and/or word/document.xml
// — never a DOM round-trip — so a Format call that only sets, say,
// BodySizePt leaves word/document.xml (and every other zip entry) byte for
// byte identical to what it was before, and vice versa for margins/
// Normalize (which touch only word/document.xml).
//
// The only field allowed to change paragraph count or text is Normalize;
// every other field only ever touches formatting properties, never a
// <w:t>'s content.
func (d *Document) Format(opts FormatOptions) (FormatResult, error) {
	// A range switches to direct formatting entirely (formatDirectRange),
	// never falling through to the whole-document path below — see that
	// function's doc comment for the landing-point table and why several
	// FormatOptions fields are refused outright when combined with a
	// range.
	if opts.StartPara > 0 {
		return d.formatDirectRange(opts)
	}
	if opts.EndPara != 0 {
		return FormatResult{}, fmt.Errorf("docx: end_para %d was given without start_para; there is no range to end", opts.EndPara)
	}

	resolved, err := resolveFormatOptions(opts)
	if err != nil {
		return FormatResult{}, err
	}

	var result FormatResult

	wantsStyles := resolved.BodyFont != "" || resolved.BodySizePt != 0 ||
		resolved.LineSpacing != 0 || resolved.Align != "" || resolved.HeadingFont != ""
	if wantsStyles {
		styles, ok := d.Part("word/styles.xml")
		if !ok {
			return FormatResult{}, fmt.Errorf("docx: package has no word/styles.xml part")
		}
		patches, applied, err := planStylesPatches(styles, resolved)
		if err != nil {
			return FormatResult{}, err
		}
		if len(patches) > 0 {
			out, err := Apply(styles, patches)
			if err != nil {
				return FormatResult{}, fmt.Errorf("docx: apply style formatting: %w", err)
			}
			if err := d.SetPart("word/styles.xml", out); err != nil {
				return FormatResult{}, err
			}
		}
		result.Applied = append(result.Applied, applied...)
	}

	wantsMargins := resolved.MarginsMM != nil
	if wantsMargins || resolved.Normalize {
		doc, ok := d.Part(DocumentPart)
		if !ok {
			return FormatResult{}, fmt.Errorf("docx: package has no %s part", DocumentPart)
		}
		working := doc
		changed := false

		if wantsMargins {
			patches, err := planMarginPatches(working, resolved.MarginsMM)
			if err != nil {
				return FormatResult{}, err
			}
			out, err := Apply(working, patches)
			if err != nil {
				return FormatResult{}, fmt.Errorf("docx: apply margins: %w", err)
			}
			working = out
			changed = true
			result.Applied = append(result.Applied, fmt.Sprintf(
				"margins -> top %gmm / right %gmm / bottom %gmm / left %gmm",
				resolved.MarginsMM[0], resolved.MarginsMM[1], resolved.MarginsMM[2], resolved.MarginsMM[3]))
		}

		if resolved.Normalize {
			if d.HasRevisions() {
				result.Notes = append(result.Notes,
					"normalize skipped: the document contains revision marks (w:ins/w:del)")
			} else {
				out, removed, err := normalizeEmptyParagraphs(working)
				if err != nil {
					return FormatResult{}, fmt.Errorf("docx: normalize paragraphs: %w", err)
				}
				if removed > 0 {
					working = out
					changed = true
					result.Applied = append(result.Applied, fmt.Sprintf("collapsed %d empty paragraph(s)", removed))
				}
				result.Notes = append(result.Notes,
					"normalize only collapses consecutive empty paragraphs; punctuation-spacing normalization is out of scope for this task and was not done")
			}
		}

		if changed {
			if err := d.SetPart(DocumentPart, working); err != nil {
				return FormatResult{}, err
			}
		}
	}

	return result, nil
}

// resolveFormatOptions expands opts.Template (if any) into concrete field
// values, letting any field opts also set explicitly win over the
// template's value for that field ("zero value" — "", 0, nil — means
// unset, both for a raw caller-supplied FormatOptions and for the
// template's own defaults), then validates the result.
func resolveFormatOptions(opts FormatOptions) (FormatOptions, error) {
	resolved := opts
	if opts.Template != "" {
		tmpl, ok := formatTemplates[opts.Template]
		if !ok {
			return FormatOptions{}, fmt.Errorf(
				"docx: unknown format template %q; want \"corporate\", \"academic\", or \"minimal\"", opts.Template)
		}
		resolved = mergeFormatOptions(opts, tmpl)
	}
	resolved.Template = ""

	switch resolved.Align {
	case "", "left", "justify":
	default:
		return FormatOptions{}, fmt.Errorf("docx: unknown alignment %q; want \"left\" or \"justify\"", resolved.Align)
	}
	if err := validateMargins(resolved.MarginsMM); err != nil {
		return FormatOptions{}, err
	}
	return resolved, nil
}

// mergeFormatOptions returns explicit with every zero-valued field
// ("" / 0 / nil) filled in from tmpl. explicit's non-zero fields always
// win, which is how "显式给出的字段覆盖模板值" (explicit fields override the
// template) is implemented.
func mergeFormatOptions(explicit, tmpl FormatOptions) FormatOptions {
	out := explicit
	if out.HeadingFont == "" {
		out.HeadingFont = tmpl.HeadingFont
	}
	if out.BodyFont == "" {
		out.BodyFont = tmpl.BodyFont
	}
	if out.BodySizePt == 0 {
		out.BodySizePt = tmpl.BodySizePt
	}
	if out.LineSpacing == 0 {
		out.LineSpacing = tmpl.LineSpacing
	}
	if out.Align == "" {
		out.Align = tmpl.Align
	}
	if out.MarginsMM == nil {
		out.MarginsMM = tmpl.MarginsMM
	}
	return out
}

// validateMargins enforces MarginsMM's documented contract: nil (leave
// margins alone) is fine, but anything else must be exactly 4 positive
// values.
func validateMargins(mm []float64) error {
	if mm == nil {
		return nil
	}
	if len(mm) != 4 {
		return fmt.Errorf("docx: MarginsMM must have exactly 4 values (top, right, bottom, left); got %d", len(mm))
	}
	for _, v := range mm {
		if v <= 0 {
			return fmt.Errorf("docx: margin value %gmm must be positive", v)
		}
	}
	return nil
}

// ptToHalfPoints converts points to OOXML's half-point unit for w:sz/w:szCs
// (14pt -> 28).
func ptToHalfPoints(pt float64) string {
	return fmt.Sprintf("%d", int(math.Round(pt*2)))
}

// lineSpacingTo240ths converts a line-spacing multiple to OOXML's 240ths-
// of-a-line unit for w:spacing w:line (1.15 -> 276).
func lineSpacingTo240ths(mult float64) string {
	return fmt.Sprintf("%d", int(math.Round(mult*240)))
}

// mmToTwips converts millimeters to twips for w:pgMar (25.4mm -> 1440).
func mmToTwips(mm float64) string {
	return fmt.Sprintf("%d", int(math.Round(mm*twipsPerMM)))
}

// elemInfo describes one XML element Format either found while scanning a
// part, or needs to synthesize because it was missing. found is false for
// an element that does not exist at all: tagSpan/selfClosing/attrs are then
// meaningless, and the caller must insert a brand new element instead of
// editing this one in place.
type elemInfo struct {
	found       bool
	tagSpan     Span
	selfClosing bool
	attrs       []xml.Attr
	// closeStart is the offset where this element's own closing tag begins
	// (</...> or, for a self-closing element, tagSpan.End). It is only ever
	// populated for elements Format uses as a CONTAINER (docDefaults'
	// rPr/pPr, a heading style's rPr) — the point to insert a new last
	// child at when every candidate child is missing.
	closeStart int
}

// isSelfClosingSpan reports whether the raw bytes of span (a StartElement's
// start tag, as captured by [prevOffset, offset) around an xml.Decoder
// token) end in "/>", the same test scan.go uses for <w:t/>.
func isSelfClosingSpan(data []byte, span Span) bool {
	return span.End-span.Start >= 2 && string(data[span.End-2:span.End]) == "/>"
}

// setAttr returns a copy of attrs with name's value set to newVal, adding
// the attribute at the end if it was not already present. This is the one
// function behind BOTH "change an existing attribute's value" and "add an
// attribute to an existing element that lacks it" — the same code path
// covers both of §4.3's required capabilities, because from the tag's own
// perspective they are the same operation.
func setAttr(attrs []xml.Attr, name, newVal string) []xml.Attr {
	out := make([]xml.Attr, 0, len(attrs)+1)
	found := false
	for _, a := range attrs {
		if a.Name.Local == name {
			out = append(out, xml.Attr{Name: a.Name, Value: newVal})
			found = true
			continue
		}
		out = append(out, a)
	}
	if !found {
		out = append(out, xml.Attr{Name: xml.Name{Local: name}, Value: newVal})
	}
	return out
}

// dropAttrs returns a copy of attrs with every attribute whose local name
// is in names removed.
func dropAttrs(attrs []xml.Attr, names ...string) []xml.Attr {
	out := make([]xml.Attr, 0, len(attrs))
	for _, a := range attrs {
		drop := false
		for _, n := range names {
			if a.Name.Local == n {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, a)
		}
	}
	return out
}

// buildTag renders a WordprocessingML start tag from a local element name
// and an attribute list, always using the literal "w:" prefix — the same
// hardcoded-prefix convention edit.go already uses for synthesized
// <w:p>/<w:r>/<w:t> XML, valid because every real .docx this package has
// been measured against declares "w" for that namespace at the root
// element.
func buildTag(local string, attrs []xml.Attr, selfClosing bool) string {
	var b strings.Builder
	b.WriteString("<w:")
	b.WriteString(local)
	for _, a := range attrs {
		b.WriteByte(' ')
		b.WriteString("w:")
		b.WriteString(a.Name.Local)
		b.WriteString(`="`)
		b.WriteString(escapeAttrValue(a.Value))
		b.WriteByte('"')
	}
	if selfClosing {
		b.WriteString("/>")
	} else {
		b.WriteByte('>')
	}
	return b.String()
}

// escapeAttrValue escapes the characters that would break a double-quoted
// XML attribute value.
func escapeAttrValue(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '"':
			b.WriteString("&quot;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// rFontsLiteralAttrs returns rFonts attributes that set every one of
// ascii/hAnsi/eastAsia/cs to font, dropping both the four *Theme attributes
// (asciiTheme/eastAsiaTheme/hAnsiTheme/cstheme) — which Word resolves in
// preference to a literal face name, silently ignoring the literal
// attribute otherwise — and any pre-existing literal attributes of the
// same names, so the result never carries a stale value alongside the new
// one.
func rFontsLiteralAttrs(existing []xml.Attr, font string) []xml.Attr {
	kept := dropAttrs(existing, "asciiTheme", "eastAsiaTheme", "hAnsiTheme", "cstheme",
		"ascii", "hAnsi", "eastAsia", "cs")
	kept = append(kept,
		xml.Attr{Name: xml.Name{Local: "ascii"}, Value: font},
		xml.Attr{Name: xml.Name{Local: "hAnsi"}, Value: font},
		xml.Attr{Name: xml.Name{Local: "eastAsia"}, Value: font},
		xml.Attr{Name: xml.Name{Local: "cs"}, Value: font},
	)
	return kept
}

// pPrChildOrder is CT_PPr's child sequence: CT_PPrBase's own sequence,
// followed by CT_PPr's trailing rPr/sectPr/pPrChange (rPr must follow every
// CT_PPrBase element and precede sectPr; sectPr must precede pPrChange).
// Copied verbatim from the design brief (.superpowers/sdd/task-1-brief.md,
// Task 1), which in turn transcribes ECMA-376's CT_PPrBase/CT_PPr
// definitions. Format only ever edits "spacing" and "jc" itself, but the
// FULL list is needed as an anchor set: a newly inserted <w:spacing>/<w:jc>
// must land immediately before whichever LATER sibling in this order
// already exists in the document, even when that sibling is something this
// package never edits (rPr, ind, sectPr, ...) -- see applyLeafOps and its
// callers. Getting this wrong produces well-formed but schema-illegal XML
// that Word "repairs" by silently dropping the offending properties
// (format capability review, Critical 1).
var pPrChildOrder = []string{
	"pStyle", "keepNext", "keepLines", "pageBreakBefore", "framePr",
	"widowControl", "numPr", "suppressLineNumbers", "pBdr", "shd", "tabs",
	"suppressAutoHyphens", "kinsoku", "wordWrap", "overflowPunct",
	"topLinePunct", "autoSpaceDE", "autoSpaceDN", "bidi", "adjustRightInd",
	"snapToGrid", "spacing", "ind", "contextualSpacing", "mirrorIndents",
	"suppressOverlap", "jc", "textDirection", "textAlignment",
	"textboxTightWrap", "outlineLvl", "divId", "cnfStyle",
	"rPr", "sectPr", "pPrChange",
}

// rPrFontSizeOrder names the three EG_RPrBase leaves Format itself ever
// edits (rFonts, sz, szCs), used as the "want" list for scanDirectChildren
// when scanning an rPr's own DIRECT children — as opposed to a same-named
// element that happens to be nested arbitrarily deeper inside it (most
// notably a <w:rPrChange> wrapping a revision's historical <w:rPr>, which
// carries its own rFonts/sz/szCs that must never be mistaken for the
// current ones).
var rPrFontSizeOrder = []string{"rFonts", "sz", "szCs"}

// leafOp is one candidate edit against a sibling leaf element inside a
// known container (docDefaults' rPr/pPr, or a heading style's rPr), given
// in the same order those siblings appear in the OOXML schema. active
// reports whether this call actually wants to change this particular leaf;
// info/local/attrs describe what "changed" means for it. A leaf that is not
// active is still included so applyLeafOps can correctly anchor an
// insertion for an EARLIER missing-and-active sibling in front of whichever
// real element comes next, active or not.
type leafOp struct {
	active bool
	info   elemInfo
	local  string
	attrs  []xml.Attr
}

// applyLeafOps emits patches for ops, a fixed set of sibling leaf elements
// within one container, walked in schema order. A found leaf's whole start
// tag is rewritten in place (via PatchRawSpan on its own span) when active;
// a missing-and-active leaf is synthesized and merged into whichever comes
// first: the next found sibling's own patch (as a text prefix, when that
// sibling is also active), a small standalone insert placed immediately
// before that next found sibling (when it is not itself being edited), or —
// if no later sibling exists at all — a standalone insert immediately
// before the container's closing tag.
//
// The merge-into-the-next-real-element rule exists to satisfy splice.go's
// Apply, which rejects two patches sharing the same start offset: naively
// inserting new content as its own zero-width patch positioned exactly at
// the next sibling's start tag would collide with that sibling's own edit
// patch, which starts at the same offset.
func applyLeafOps(doc []byte, containerCloseStart int, ops []leafOp) []Patch {
	var patches []Patch
	var pending strings.Builder
	for _, op := range ops {
		if !op.info.found {
			if op.active {
				pending.WriteString(buildTag(op.local, op.attrs, true))
			}
			continue
		}
		prefix := pending.String()
		pending.Reset()
		if op.active {
			patches = append(patches, PatchRawSpan(doc, op.info.tagSpan, prefix+buildTag(op.local, op.attrs, true)))
		} else if prefix != "" {
			patches = append(patches, PatchRawSpan(doc, Span{op.info.tagSpan.Start, op.info.tagSpan.Start}, prefix))
		}
	}
	if pending.Len() > 0 {
		patches = append(patches, PatchRawSpan(doc, Span{containerCloseStart, containerCloseStart}, pending.String()))
	}
	return patches
}

// scanDirectChildren decodes fragment — the raw bytes strictly BETWEEN some
// container's own start and end tags, e.g. a <w:pPr>'s or <w:rPr>'s inner
// content — and reports, for each name in order that occurs as a DIRECT
// child (nesting depth 0 within fragment), its elemInfo (first occurrence
// only; found is the zero value, i.e. absent, for every other name). base
// is added to every offset so the returned spans are absolute into the full
// document/part fragment was sliced from.
//
// Depth is tracked structurally with a single counter, incremented on every
// StartElement and decremented on every EndElement, while inside the
// fragment — including a self-closing element's own, since
// encoding/xml.Decoder always synthesizes a matching EndElement for one, so
// the increment/decrement pair still nets to zero and never disturbs a
// sibling's depth. This is what lets a leaf nested inside an UNTRACKED
// sibling (e.g. a <w:spacing> nested inside a paragraph's own <w:rPr>,
// itself a direct child of <w:pPr>) be told apart from the identically
// named element sitting directly in the container being scanned — the fix
// for the "a boolean flag never closes over a nested container of the same
// kind" class of bug (format capability review, Critical 2).
func scanDirectChildren(fragment []byte, base int, order []string) map[string]elemInfo {
	want := make(map[string]bool, len(order))
	for _, n := range order {
		want[n] = true
	}
	found := make(map[string]elemInfo, len(order))

	dec := xml.NewDecoder(bytes.NewReader(fragment))
	var prevOffset, depth int
	for {
		tok, err := dec.Token()
		if err != nil {
			// Ordinarily plain EOF: fragment is a complete run of sibling
			// elements sliced from [container.tagSpan.End,
			// container.closeStart), so decoding it in isolation normally
			// reaches EOF cleanly without ever erroring. This is NOT a
			// guarantee that fragment is correctly bounded, though: a
			// container's own closeStart can itself be wrong if the
			// caller's OUTER scan mistook a same-named nested element's
			// close for the container's own (the exact class of bug this
			// function exists to fix one level down — see scanRunProps'
			// and scanDocDefaults' <w:rPrChange> handling, and pPr's own
			// unresolved <w:pPrChange><w:pPr> case, which is NOT fixed
			// here). A wrongly-bounded fragment is typically still
			// well-formed on its own (it just ends at a legitimate
			// sub-boundary), so it would silently produce an incomplete or
			// wrong "found" map rather than an error — breaking out on any
			// decode error is only a safety net for genuinely malformed
			// input, not a correctness guarantee for the fragment's bounds.
			break
		}
		offset := int(dec.InputOffset())
		switch t := tok.(type) {
		case xml.StartElement:
			localSpan := Span{prevOffset, offset}
			if depth == 0 && want[t.Name.Local] && isWordprocessingMLNamespace(t.Name.Space) {
				if _, exists := found[t.Name.Local]; !exists {
					found[t.Name.Local] = elemInfo{
						found:       true,
						tagSpan:     Span{base + prevOffset, base + offset},
						selfClosing: isSelfClosingSpan(fragment, localSpan),
						attrs:       t.Attr,
					}
				}
			}
			depth++
		case xml.EndElement:
			if depth > 0 {
				depth--
			}
		}
		prevOffset = offset
	}
	return found
}

// buildPPrOps returns the leafOps applyLeafOps (or renderActiveLeaves, for a
// brand new pPr being synthesized from scratch) needs to land <w:spacing>/
// <w:jc> at the schema-correct position among a pPr's other children: one
// leafOp per name in pPrChildOrder, in that order, with "spacing"/"jc"
// marked active (and given their new attributes) when lineSpacing/align
// requests a change, and every other name serving purely as an ANCHOR —
// present (when found in children) so a new element merges in immediately
// before whichever later, otherwise-untracked sibling (rPr, ind, sectPr,
// ...) already exists, instead of always landing at pPr's very end
// (format capability review, Critical 1). children may be nil — reading a
// nil map always returns the zero elemInfo (not found) — which is exactly
// "no pPr exists yet to anchor against", the shape a brand new pPr needs.
func buildPPrOps(children map[string]elemInfo, lineSpacing float64, align string) []leafOp {
	ops := make([]leafOp, 0, len(pPrChildOrder))
	for _, name := range pPrChildOrder {
		op := leafOp{info: children[name], local: name}
		switch name {
		case "spacing":
			if lineSpacing != 0 {
				op.active = true
				line := lineSpacingTo240ths(lineSpacing)
				op.attrs = setAttr(setAttr(op.info.attrs, "line", line), "lineRule", "auto")
			}
		case "jc":
			if align != "" {
				op.active = true
				op.attrs = setAttr(op.info.attrs, "val", align)
			}
		}
		ops = append(ops, op)
	}
	return ops
}

// planRPrFontSizePatches builds the patches for one rPr's direct font/size
// formatting (an rPr that already exists with real content — the caller
// handles "no rPr at all" and "self-closing <w:rPr/>" itself), given
// containerOpenEnd/containerCloseStart (rPr's own tagSpan.End/closeStart)
// and what the caller already scanned for rfonts/sz/szcs.
//
// rFonts, when newly inserted, always lands as rPr's very FIRST child
// (EG_RPrBase requires it precede every other run property) by anchoring
// directly to containerOpenEnd, rather than being merged via applyLeafOps'
// normal "insert immediately before the next TRACKED sibling" rule — which
// would let it land after existing but UNTRACKED content such as <w:b/>,
// since that rule only ever looks at siblings later in ITS OWN ops list,
// never at what (if anything) precedes them (format capability review,
// Minor 13, same root cause as Critical 1). sz/szCs keep using
// applyLeafOps' ordinary merge, unchanged from before.
//
// The one collision this has to guard against: if sz or szCs is itself
// rPr's literal first child (its tagSpan starts exactly at
// containerOpenEnd) and is being rewritten in place, applyLeafOps already
// produces a patch starting at that same offset — a second, independent
// patch at the identical zero-width offset would make Apply reject the
// whole batch as overlapping. So the newly built rFonts tag is merged as a
// TEXT PREFIX onto that existing patch instead of being appended as its own.
func planRPrFontSizePatches(doc []byte, containerOpenEnd, containerCloseStart int, rfonts, sz, szcs elemInfo, font string, sizePt float64) []Patch {
	remOps := []leafOp{{info: sz, local: "sz"}, {info: szcs, local: "szCs"}}
	if sizePt != 0 {
		half := ptToHalfPoints(sizePt)
		remOps[0].active = true
		remOps[0].attrs = setAttr(sz.attrs, "val", half)
		remOps[1].active = true
		remOps[1].attrs = setAttr(szcs.attrs, "val", half)
	}
	remPatches := applyLeafOps(doc, containerCloseStart, remOps)

	var patches []Patch
	if font != "" {
		if rfonts.found {
			patches = append(patches, PatchRawSpan(doc, rfonts.tagSpan,
				buildTag("rFonts", rFontsLiteralAttrs(rfonts.attrs, font), true)))
		} else {
			newTag := buildTag("rFonts", rFontsLiteralAttrs(nil, font), true)
			merged := false
			for i := range remPatches {
				if remPatches[i].Content.Start == containerOpenEnd {
					remPatches[i].NewText = newTag + remPatches[i].NewText
					merged = true
					break
				}
			}
			if !merged {
				patches = append(patches, PatchRawSpan(doc, Span{containerOpenEnd, containerOpenEnd}, newTag))
			}
		}
	}
	patches = append(patches, remPatches...)
	return patches
}

// renderActiveLeaves concatenates ops' active leaves, in the schema order
// they were given in, as brand-new self-closing tags. It is
// synthesizeDocDefaultsPatches's counterpart to applyLeafOps: there is no
// existing container byte range to splice into when a container is being
// created from scratch, so there is nothing to merge against and no
// "found" span to preserve — every active leaf is simply rendered in
// order, and an inactive leaf contributes nothing (there is no pre-existing
// copy of it to keep, unlike applyLeafOps' found-but-inactive case).
func renderActiveLeaves(ops []leafOp) string {
	var b strings.Builder
	for _, op := range ops {
		if op.active {
			b.WriteString(buildTag(op.local, op.attrs, true))
		}
	}
	return b.String()
}

// synthesizeDocDefaultsPatches creates whichever part of styles.xml's
// <w:docDefaults> chain planStylesPatches determined is missing —
// <w:docDefaults> itself, <w:rPrDefault>/<w:pPrDefault>, or the <w:rPr>/
// <w:pPr> directly inside an already-present rPrDefault/pPrDefault — with
// rprInner/pprInner (built by renderActiveLeaves) as the new element's
// content. needRPr/needPPr say which of the two sub-chains this call
// actually needs; a false one is left completely alone, including when
// docDefaults itself has to be created (its body only ever contains the
// sub-chain(s) actually requested, never an empty rPrDefault/pPrDefault
// nobody asked for).
//
// Every insertion respects CT_DocDefaults' schema order — rPrDefault
// precedes pPrDefault — and, critically, never emits two patches sharing
// the same start offset (Apply rejects that as an overlap): when
// docDefaults doesn't exist yet, both needed sub-chains are combined into
// ONE patch inserted right after <w:styles>'s own start tag; when
// docDefaults already exists but BOTH rPrDefault and pPrDefault are
// missing, both are likewise combined into one patch inserted right after
// docDefaults' own start tag — the two insertion points would otherwise
// coincide whenever docDefaults is currently empty.
func synthesizeDocDefaultsPatches(styles []byte, rootEnd int, dd, rpd, ppd elemInfo, needRPr bool, rprInner string, needPPr bool, pprInner string) []Patch {
	rprWrapperXML := "<w:rPrDefault><w:rPr>" + rprInner + "</w:rPr></w:rPrDefault>"
	pprWrapperXML := "<w:pPrDefault><w:pPr>" + pprInner + "</w:pPr></w:pPrDefault>"

	// dd.found&&!dd.selfClosing means docDefaults exists with a real body
	// to insert into. A self-closing <w:docDefaults/> (or its outright
	// absence) has nowhere for a child to live, so both are handled by
	// building the whole element fresh — the only difference is whether an
	// existing self-closing tag is expanded in place or a new one is
	// inserted at rootEnd.
	if !dd.found || dd.selfClosing {
		var body strings.Builder
		if needRPr {
			body.WriteString(rprWrapperXML)
		}
		if needPPr {
			body.WriteString(pprWrapperXML)
		}
		full := "<w:docDefaults>" + body.String() + "</w:docDefaults>"
		if dd.found {
			return []Patch{PatchRawSpan(styles, dd.tagSpan, full)}
		}
		return []Patch{PatchRawSpan(styles, Span{rootEnd, rootEnd}, full)}
	}

	var patches []Patch

	rpdOpen := rpd.found && !rpd.selfClosing
	ppdOpen := ppd.found && !ppd.selfClosing
	needRPrWrapper := needRPr && !rpdOpen
	needPPrWrapper := needPPr && !ppdOpen

	switch {
	case needRPrWrapper && needPPrWrapper && !rpd.found && !ppd.found:
		// Neither wrapper exists at all, and docDefaults' body is
		// otherwise empty of them: combine into one insert right after
		// docDefaults' own start tag so the two insertion points, which
		// coincide when docDefaults has no other content, never collide.
		patches = append(patches, PatchRawSpan(styles, Span{dd.tagSpan.End, dd.tagSpan.End}, rprWrapperXML+pprWrapperXML))
	default:
		if needRPrWrapper {
			if rpd.found {
				// Self-closing <w:rPrDefault/>: expand it in place.
				patches = append(patches, PatchRawSpan(styles, rpd.tagSpan, rprWrapperXML))
			} else {
				patches = append(patches, PatchRawSpan(styles, Span{dd.tagSpan.End, dd.tagSpan.End}, rprWrapperXML))
			}
		}
		if needPPrWrapper {
			if ppd.found {
				patches = append(patches, PatchRawSpan(styles, ppd.tagSpan, pprWrapperXML))
			} else {
				patches = append(patches, PatchRawSpan(styles, Span{dd.closeStart, dd.closeStart}, pprWrapperXML))
			}
		}
	}

	// rPrDefault/pPrDefault themselves already exist with a real body here
	// (rpdOpen/ppdOpen) — only rPr/pPr is missing from inside them.
	if needRPr && rpdOpen {
		patches = append(patches, PatchRawSpan(styles, Span{rpd.closeStart, rpd.closeStart}, "<w:rPr>"+rprInner+"</w:rPr>"))
	}
	if needPPr && ppdOpen {
		patches = append(patches, PatchRawSpan(styles, Span{ppd.closeStart, ppd.closeStart}, "<w:pPr>"+pprInner+"</w:pPr>"))
	}

	return patches
}

// scanDocDefaults locates styles.xml's <w:docDefaults> chain in one pass:
// the rPr triple (rFonts/sz/szCs, under rPrDefault) directly, and pPrDefault
// itself (as ppr) so pprChildren can be filled in via a SEPARATE
// scanDirectChildren pass over exactly ppr's own inner bytes once its span
// is known. Each returned elemInfo's found field says whether that element
// exists at all; rpr/ppr additionally carry closeStart, the insertion point
// to use when every one of their tracked children is missing. dd/rpd/ppd
// also carry closeStart now (populated the same way, on their own end tag)
// so planStylesPatches can synthesize whichever part of the chain a minimal
// generator's styles.xml omitted — see synthesizeDocDefaultsPatches —
// rather than only ever editing an already-complete chain. rootEnd is the
// offset right after the root <w:styles ...> element's own start tag: the
// insertion point for a brand new <w:docDefaults>, which must land as
// <w:styles>'s FIRST child.
//
// Only docDefaults and its direct rPrDefault/pPrDefault/rPr/pPr descendants
// are tracked structurally in THIS pass (booleans gate matches the same way
// scan.go's paraDepth/pPrDepth do) — this is deliberately narrower than a
// general path engine, scoped to exactly the chain §4.3's measured facts
// describe. Neither rPr's nor pPrDefault's pPr's own DIRECT children
// (rFonts/sz/szCs; spacing/jc plus every other CT_PPr-schema anchor
// buildPPrOps needs) are matched inline here with a boolean: rPr can wrap a
// <w:rPrChange> holding a REVISION'S historical <w:rPr> (with its own
// rFonts/sz/szCs), and pPr is itself CT_PPrGeneral, which may carry a
// nested <w:rPr> (paragraph mark run properties) that can carry its OWN
// <w:spacing> (character spacing) — in both cases a same-named but
// different property nested one or more levels down that a boolean can't
// tell apart from the container's own, because the boolean never closes
// over the nested container (format capability review, Critical 2 and its
// rPr/rPrChange follow-up). rPr's own true closing tag is likewise found
// via a small depth counter (rprDepth/trackingRPr below), not by matching
// the next EndElement literally named "rPr", for the same reason: that
// would find the NESTED rPr's close instead of the outer one's. The
// scanDirectChildren calls below (for both rPr's and pPr's own direct
// children) inherit the same fix.
func scanDocDefaults(styles []byte) (dd, rpd, rpr, rfonts, sz, szcs, ppd, ppr elemInfo, pprChildren map[string]elemInfo, rootEnd int, err error) {
	dec := xml.NewDecoder(bytes.NewReader(styles))
	var prevOffset int
	var inDD, inRPD, inPPD, inPPR bool
	var trackingRPr bool
	var rprDepth int
	var sawRoot bool

	for {
		tok, terr := dec.Token()
		if terr != nil {
			if errors.Is(terr, io.EOF) {
				break
			}
			return dd, rpd, rpr, rfonts, sz, szcs, ppd, ppr, pprChildren, rootEnd,
				fmt.Errorf("scan styles.xml docDefaults: %w", terr)
		}
		offset := int(dec.InputOffset())

		switch t := tok.(type) {
		case xml.StartElement:
			span := Span{prevOffset, offset}
			sc := isSelfClosingSpan(styles, span)
			if !sawRoot {
				sawRoot = true
				rootEnd = offset
			}
			switch {
			case isWordElement(t.Name, "docDefaults") && !dd.found:
				dd = elemInfo{found: true, tagSpan: span, selfClosing: sc, attrs: t.Attr}
				if !sc {
					inDD = true
				}
			case isWordElement(t.Name, "rPrDefault") && dd.found && !rpd.found:
				rpd = elemInfo{found: true, tagSpan: span, selfClosing: sc, attrs: t.Attr}
				if !sc {
					inRPD = true
				}
			case trackingRPr:
				// Anything encountered once rPr is being tracked (including
				// a NESTED <w:rPr>, e.g. <w:rPrChange>'s historical copy)
				// only ever deepens rprDepth; it can never re-trigger the
				// case below.
				rprDepth++
			case isWordElement(t.Name, "rPr") && inRPD && !rpr.found:
				rpr = elemInfo{found: true, tagSpan: span, selfClosing: sc, attrs: t.Attr}
				if sc {
					rpr.closeStart = rpr.tagSpan.End
				} else {
					trackingRPr = true
					rprDepth = 0
				}
			case isWordElement(t.Name, "pPrDefault") && dd.found && !ppd.found:
				ppd = elemInfo{found: true, tagSpan: span, selfClosing: sc, attrs: t.Attr}
				if !sc {
					inPPD = true
				}
			case isWordElement(t.Name, "pPr") && inPPD && !ppr.found:
				ppr = elemInfo{found: true, tagSpan: span, selfClosing: sc, attrs: t.Attr}
				if !sc {
					inPPR = true
				}
			}

		case xml.EndElement:
			switch {
			case trackingRPr:
				if rprDepth == 0 {
					rpr.closeStart = prevOffset
					trackingRPr = false
				} else {
					rprDepth--
				}
			case isWordElement(t.Name, "rPrDefault") && inRPD:
				rpd.closeStart = prevOffset
				inRPD = false
			case isWordElement(t.Name, "pPr") && inPPR:
				ppr.closeStart = prevOffset
				inPPR = false
			case isWordElement(t.Name, "pPrDefault") && inPPD:
				ppd.closeStart = prevOffset
				inPPD = false
			case isWordElement(t.Name, "docDefaults") && inDD:
				dd.closeStart = prevOffset
				inDD = false
			}
		}
		prevOffset = offset
	}

	if rpr.found && !rpr.selfClosing {
		rprChildren := scanDirectChildren(styles[rpr.tagSpan.End:rpr.closeStart], rpr.tagSpan.End, rPrFontSizeOrder)
		rfonts, sz, szcs = rprChildren["rFonts"], rprChildren["sz"], rprChildren["szCs"]
	}
	if ppr.found && ppr.selfClosing {
		ppr.closeStart = ppr.tagSpan.End
	}
	if ppr.found && !ppr.selfClosing {
		pprChildren = scanDirectChildren(styles[ppr.tagSpan.End:ppr.closeStart], ppr.tagSpan.End, pPrChildOrder)
	}
	return dd, rpd, rpr, rfonts, sz, szcs, ppd, ppr, pprChildren, rootEnd, nil
}

// planStylesPatches builds every styles.xml patch resolved requests
// (docDefaults body font/size/line-spacing/alignment, plus every
// Heading1..9's font) and a human-readable Applied entry per change made.
//
// A requested field's docDefaults chain (rPrDefault/rPr for BodyFont/
// BodySizePt, pPrDefault/pPr for LineSpacing/Align) is edited in place when
// it already fully exists — the ordinary case for a document Word or
// python-docx produced — and synthesized (via synthesizeDocDefaultsPatches)
// at whichever point it is missing otherwise: docx_write's own styles.xml
// now always carries the full chain (see stylesPartXML), but docx_format
// must not be brittle against every OTHER minimal generator that omits it,
// or a caller hits this same failure again with a document this package
// did not write. Creating <w:docDefaults><w:rPrDefault><w:rPr> is not a
// guess: it is the standard OOXML structure a document default lives in,
// so building it is exactly what "set the document default" means when it
// is not there yet.
func planStylesPatches(styles []byte, opts FormatOptions) ([]Patch, []string, error) {
	var patches []Patch
	var applied []string

	wantRPrChain := opts.BodyFont != "" || opts.BodySizePt != 0
	wantPPrChain := opts.LineSpacing != 0 || opts.Align != ""

	if wantRPrChain || wantPPrChain {
		dd, rpd, rpr, rfonts, sz, szcs, ppd, ppr, pprChildren, rootEnd, err := scanDocDefaults(styles)
		if err != nil {
			return nil, nil, err
		}

		var rprInner, pprInner string
		needRPrSynthesis := false
		needPPrSynthesis := false

		if wantRPrChain {
			if opts.BodyFont != "" {
				applied = append(applied, fmt.Sprintf("body font -> %s", opts.BodyFont))
			}
			if opts.BodySizePt != 0 {
				applied = append(applied, fmt.Sprintf("body size -> %gpt", opts.BodySizePt))
			}
			if dd.found && rpd.found && rpr.found {
				// The chain fully exists already: edit its leaves in place —
				// byte-identical output for a document that already has a
				// complete rFonts/sz/szCs is a hard guarantee for the case
				// nothing here needed to move. planRPrFontSizePatches (not
				// the old bare applyLeafOps call) additionally makes sure a
				// newly inserted rFonts lands as rPr's first child rather
				// than after unrelated existing content (Minor 13).
				patches = append(patches, planRPrFontSizePatches(
					styles, rpr.tagSpan.End, rpr.closeStart, rfonts, sz, szcs, opts.BodyFont, opts.BodySizePt)...)
			} else {
				needRPrSynthesis = true
				ops := []leafOp{{info: rfonts, local: "rFonts"}, {info: sz, local: "sz"}, {info: szcs, local: "szCs"}}
				if opts.BodyFont != "" {
					ops[0].active = true
					ops[0].attrs = rFontsLiteralAttrs(rfonts.attrs, opts.BodyFont)
				}
				if opts.BodySizePt != 0 {
					half := ptToHalfPoints(opts.BodySizePt)
					ops[1].active = true
					ops[1].attrs = setAttr(sz.attrs, "val", half)
					ops[2].active = true
					ops[2].attrs = setAttr(szcs.attrs, "val", half)
				}
				rprInner = renderActiveLeaves(ops)
			}
		}

		if wantPPrChain {
			if opts.LineSpacing != 0 {
				applied = append(applied, fmt.Sprintf("line spacing -> %g", opts.LineSpacing))
			}
			if opts.Align != "" {
				applied = append(applied, fmt.Sprintf("alignment -> %s", opts.Align))
			}
			if dd.found && ppd.found && ppr.found {
				// buildPPrOps (not a bare 2-element ops list) anchors the
				// insertion against pPrDefault's pPr's FULL set of tracked
				// children, so a newly inserted spacing/jc lands in
				// CT_PPr's schema order even when that pPr also carries a
				// paragraph-mark <w:rPr> or other properties (Critical 1).
				patches = append(patches, applyLeafOps(styles, ppr.closeStart, buildPPrOps(pprChildren, opts.LineSpacing, opts.Align))...)
			} else {
				needPPrSynthesis = true
				pprInner = renderActiveLeaves(buildPPrOps(nil, opts.LineSpacing, opts.Align))
			}
		}

		if needRPrSynthesis || needPPrSynthesis {
			patches = append(patches, synthesizeDocDefaultsPatches(
				styles, rootEnd, dd, rpd, ppd, needRPrSynthesis, rprInner, needPPrSynthesis, pprInner)...)
		}
	}

	if opts.HeadingFont != "" {
		headingPatches, err := planHeadingFontPatches(styles, opts.HeadingFont)
		if err != nil {
			return nil, nil, err
		}
		if len(headingPatches) > 0 {
			patches = append(patches, headingPatches...)
			applied = append(applied, fmt.Sprintf("heading font -> %s", opts.HeadingFont))
		}
	}

	return patches, applied, nil
}

// planHeadingFontPatches rewrites every Heading1..Heading9 style's
// <w:rPr><w:rFonts> to font, stripping *Theme attributes the same way
// planStylesPatches's body-font path does. If a matched heading style has
// an <w:rPr> but no <w:rFonts> inside it, one is inserted as that rPr's
// first child; if the rPr itself is self-closing (<w:rPr/>, carrying no
// properties at all), it is expanded in place to hold the new rFonts.
//
// A heading's rPr can wrap a <w:rPrChange><w:rPr>...</w:rPr></w:rPrChange>
// holding a REVISION'S historical run properties — a same-named <w:rPr>
// (and, inside it, a same-named <w:rFonts>) nested inside the current one.
// rprDepth/trackingRPr below find the CURRENT rPr's own true close via
// depth tracking rather than matching the next EndElement literally named
// "rPr" (which would be the nested one's), the same fix already applied to
// scanRunProps and scanDocDefaults; scanDirectChildren then reads only the
// rPr's own DIRECT rFonts, so the historical copy is never mistaken for the
// current one (format capability review follow-up, same root cause as
// Critical 2 — this function had it twice: once for the close/rFonts-search
// itself, fixed here, and once for the insertion position, fixed earlier).
func planHeadingFontPatches(styles []byte, font string) ([]Patch, error) {
	dec := xml.NewDecoder(bytes.NewReader(styles))
	var prevOffset int
	var patches []Patch

	var inHeading bool
	var pPrDepth int
	var rprSeen bool
	var trackingRPr bool
	var rprDepth int
	var rprTagSpan Span

	for {
		tok, terr := dec.Token()
		if terr != nil {
			if errors.Is(terr, io.EOF) {
				break
			}
			return nil, fmt.Errorf("scan styles.xml for heading fonts: %w", terr)
		}
		offset := int(dec.InputOffset())

		switch t := tok.(type) {
		case xml.StartElement:
			span := Span{prevOffset, offset}
			switch {
			case isWordElement(t.Name, "style") && !inHeading:
				if id, ok := wordAttrVal(t, "styleId"); ok && headingStyleIDRe.MatchString(id) {
					inHeading = true
					pPrDepth = 0
					rprSeen = false
					trackingRPr = false
				}
			case trackingRPr:
				// Anything encountered once the heading's own rPr is being
				// tracked (including a NESTED <w:rPr>, e.g. <w:rPrChange>'s
				// historical copy) only ever deepens rprDepth; it can never
				// re-trigger the "found rPr" case below.
				rprDepth++
			case inHeading && isWordElement(t.Name, "pPr"):
				pPrDepth++
			case inHeading && isWordElement(t.Name, "rPr") && pPrDepth == 0 && !rprSeen:
				rprSeen = true
				sc := isSelfClosingSpan(styles, span)
				if sc {
					// No properties at all: expand in place so the new
					// rFonts has somewhere to live.
					newTag := "<w:rPr>" + buildTag("rFonts", rFontsLiteralAttrs(nil, font), true) + "</w:rPr>"
					patches = append(patches, PatchRawSpan(styles, span, newTag))
				} else {
					trackingRPr = true
					rprDepth = 0
					rprTagSpan = span
				}
			}

		case xml.EndElement:
			switch {
			case trackingRPr:
				if rprDepth > 0 {
					rprDepth--
					break
				}
				// The style's rPr's TRUE close (depth-matched to its own
				// open, not merely "the next thing literally named rPr").
				// prevOffset is exactly where this </w:rPr> begins.
				trackingRPr = false
				children := scanDirectChildren(styles[rprTagSpan.End:prevOffset], rprTagSpan.End, []string{"rFonts"})
				if rf, ok := children["rFonts"]; ok {
					newTag := buildTag("rFonts", rFontsLiteralAttrs(rf.attrs, font), true)
					patches = append(patches, PatchRawSpan(styles, rf.tagSpan, newTag))
				} else {
					// No rFonts among this rPr's own direct children:
					// insert one as its FIRST child, at rprTagSpan.End —
					// not at this close (LAST), which would land the new
					// rFonts after whatever direct formatting (e.g. <w:b/>)
					// this rPr already carries, violating EG_RPrBase's
					// "rFonts precedes every other run property" order.
					patches = append(patches, PatchRawSpan(styles, Span{rprTagSpan.End, rprTagSpan.End},
						buildTag("rFonts", rFontsLiteralAttrs(nil, font), true)))
				}
			case isWordElement(t.Name, "pPr") && pPrDepth > 0:
				pPrDepth--
			case isWordElement(t.Name, "style") && inHeading:
				inHeading = false
			}
		}
		prevOffset = offset
	}
	return patches, nil
}

// planMarginPatches rewrites every <w:pgMar> in documentXML (there is
// ordinarily exactly one, a direct child of <w:body>, but a
// multi-section document can have more) to carry marginsMM (top, right,
// bottom, left) converted to twips.
func planMarginPatches(documentXML []byte, marginsMM []float64) ([]Patch, error) {
	top := mmToTwips(marginsMM[0])
	right := mmToTwips(marginsMM[1])
	bottom := mmToTwips(marginsMM[2])
	left := mmToTwips(marginsMM[3])

	dec := xml.NewDecoder(bytes.NewReader(documentXML))
	var prevOffset int
	var patches []Patch

	for {
		tok, terr := dec.Token()
		if terr != nil {
			if errors.Is(terr, io.EOF) {
				break
			}
			return nil, fmt.Errorf("scan document.xml for pgMar: %w", terr)
		}
		offset := int(dec.InputOffset())

		if se, ok := tok.(xml.StartElement); ok && isWordElement(se.Name, "pgMar") {
			attrs := setAttr(se.Attr, "top", top)
			attrs = setAttr(attrs, "right", right)
			attrs = setAttr(attrs, "bottom", bottom)
			attrs = setAttr(attrs, "left", left)
			patches = append(patches, PatchRawSpan(documentXML, Span{prevOffset, offset}, buildTag("pgMar", attrs, true)))
		}
		prevOffset = offset
	}

	if len(patches) == 0 {
		return nil, fmt.Errorf("docx: document.xml has no <w:pgMar> element; cannot set margins")
	}
	return patches, nil
}

// normalizeEmptyParagraphs collapses every maximal run of two or more
// consecutive empty paragraphs down to one, keeping the first and deleting
// the rest. A lone empty paragraph (a run of exactly one) is left alone —
// it reads as deliberate spacing, not accidental duplication.
//
// A paragraph counts as "empty" here when the concatenation of its runs'
// text (Para.Runs[*].Text, exactly what Document.Paras() would report) is
// "" — which includes a <w:p/> with nothing at all AND a <w:p><w:pPr>...
// </w:pPr></w:p> that carries paragraph properties but no runs: neither
// produces any visible text, so both read as a blank line for this
// purpose. A paragraph is never treated as empty, even with no runs, if it
// has HasRevisions (an underlying <w:ins>/<w:del> the caller may still care
// about) or SkippedTextBox (a <w:txbxContent> subtree Scan does not surface
// as runs, so "no runs" would not mean "no visible content").
//
// "consecutive" additionally requires the same table/cell context (or, for
// two non-table paragraphs, both outside any table): merging across a
// <w:tbl> boundary, or across a <w:tc> boundary within one, is never done,
// because a table cell must keep at least one paragraph, and this
// function's own rule (keep the first of a run) already guarantees that —
// but only if a run is never allowed to span two different cells to begin
// with.
func normalizeEmptyParagraphs(doc []byte) ([]byte, int, error) {
	paras, err := Scan(doc)
	if err != nil {
		return nil, 0, err
	}

	type cellKey struct {
		inTable         bool
		table, row, col int
	}
	keyOf := func(p Para) cellKey {
		if p.Cell != nil {
			return cellKey{true, p.Cell.Table, p.Cell.Row, p.Cell.Col}
		}
		return cellKey{}
	}
	isEmpty := func(p Para) bool {
		if p.HasRevisions || p.SkippedTextBox {
			return false
		}
		for _, r := range p.Runs {
			if r.Text != "" {
				return false
			}
		}
		return true
	}

	var patches []Patch
	removed := 0
	i := 0
	for i < len(paras) {
		if !isEmpty(paras[i]) {
			i++
			continue
		}
		key := keyOf(paras[i])
		j := i + 1
		for j < len(paras) && isEmpty(paras[j]) && keyOf(paras[j]) == key {
			j++
		}
		for _, p := range paras[i+1 : j] {
			patches = append(patches, PatchRawSpan(doc, p.Span, ""))
			removed++
		}
		i = j
	}

	out, err := Apply(doc, patches)
	if err != nil {
		return nil, 0, err
	}
	return out, removed, nil
}
