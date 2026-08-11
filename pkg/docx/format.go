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

// scanDocDefaults locates styles.xml's <w:docDefaults> chain in one pass:
// the rPr triple (rFonts/sz/szCs, under rPrDefault) and the pPr pair
// (spacing/jc, under pPrDefault). Each returned elemInfo's found field says
// whether that element exists at all; rpr/ppr additionally carry
// closeStart, the insertion point to use when every one of their tracked
// children is missing.
//
// Only docDefaults and its direct rPrDefault/pPrDefault/rPr/pPr descendants
// are tracked structurally (booleans gate matches the same way scan.go's
// paraDepth/pPrDepth do) — this is deliberately narrower than a general
// path engine, scoped to exactly the chain §4.3's measured facts describe.
func scanDocDefaults(styles []byte) (dd, rpd, rpr, rfonts, sz, szcs, ppd, ppr, spacing, jc elemInfo, err error) {
	dec := xml.NewDecoder(bytes.NewReader(styles))
	var prevOffset int
	var inRPD, inRPR, inPPD, inPPR bool

	for {
		tok, terr := dec.Token()
		if terr != nil {
			if errors.Is(terr, io.EOF) {
				break
			}
			return dd, rpd, rpr, rfonts, sz, szcs, ppd, ppr, spacing, jc,
				fmt.Errorf("scan styles.xml docDefaults: %w", terr)
		}
		offset := int(dec.InputOffset())

		switch t := tok.(type) {
		case xml.StartElement:
			span := Span{prevOffset, offset}
			sc := isSelfClosingSpan(styles, span)
			switch {
			case isWordElement(t.Name, "docDefaults") && !dd.found:
				dd = elemInfo{found: true, tagSpan: span, selfClosing: sc, attrs: t.Attr}
			case isWordElement(t.Name, "rPrDefault") && dd.found && !rpd.found:
				rpd = elemInfo{found: true, tagSpan: span, selfClosing: sc, attrs: t.Attr}
				if !sc {
					inRPD = true
				}
			case isWordElement(t.Name, "rPr") && inRPD && !rpr.found:
				rpr = elemInfo{found: true, tagSpan: span, selfClosing: sc, attrs: t.Attr}
				if !sc {
					inRPR = true
				}
			case isWordElement(t.Name, "rFonts") && inRPR && !rfonts.found:
				rfonts = elemInfo{found: true, tagSpan: span, selfClosing: sc, attrs: t.Attr}
			case isWordElement(t.Name, "sz") && inRPR && !sz.found:
				sz = elemInfo{found: true, tagSpan: span, selfClosing: sc, attrs: t.Attr}
			case isWordElement(t.Name, "szCs") && inRPR && !szcs.found:
				szcs = elemInfo{found: true, tagSpan: span, selfClosing: sc, attrs: t.Attr}
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
			case isWordElement(t.Name, "spacing") && inPPR && !spacing.found:
				spacing = elemInfo{found: true, tagSpan: span, selfClosing: sc, attrs: t.Attr}
			case isWordElement(t.Name, "jc") && inPPR && !jc.found:
				jc = elemInfo{found: true, tagSpan: span, selfClosing: sc, attrs: t.Attr}
			}

		case xml.EndElement:
			switch {
			case isWordElement(t.Name, "rPr") && inRPR:
				rpr.closeStart = prevOffset
				inRPR = false
			case isWordElement(t.Name, "rPrDefault") && inRPD:
				inRPD = false
			case isWordElement(t.Name, "pPr") && inPPR:
				ppr.closeStart = prevOffset
				inPPR = false
			case isWordElement(t.Name, "pPrDefault") && inPPD:
				inPPD = false
			}
		}
		prevOffset = offset
	}

	if rpr.found && rpr.selfClosing {
		rpr.closeStart = rpr.tagSpan.End
	}
	if ppr.found && ppr.selfClosing {
		ppr.closeStart = ppr.tagSpan.End
	}
	return dd, rpd, rpr, rfonts, sz, szcs, ppd, ppr, spacing, jc, nil
}

// planStylesPatches builds every styles.xml patch resolved requests
// (docDefaults body font/size/line-spacing/alignment, plus every
// Heading1..9's font) and a human-readable Applied entry per change made.
//
// It refuses outright, rather than guessing, when the docDefaults chain a
// requested field needs to land in does not exist — see Document.Format's
// self-review note on a docDefaults-less styles.xml.
func planStylesPatches(styles []byte, opts FormatOptions) ([]Patch, []string, error) {
	var patches []Patch
	var applied []string

	wantRPrChain := opts.BodyFont != "" || opts.BodySizePt != 0
	wantPPrChain := opts.LineSpacing != 0 || opts.Align != ""

	if wantRPrChain || wantPPrChain {
		dd, rpd, rpr, rfonts, sz, szcs, ppd, ppr, spacing, jc, err := scanDocDefaults(styles)
		if err != nil {
			return nil, nil, err
		}

		if wantRPrChain {
			if !dd.found || !rpd.found || !rpr.found {
				return nil, nil, fmt.Errorf(
					"docx: styles.xml has no <w:docDefaults><w:rPrDefault><w:rPr> chain; cannot set body font/size")
			}
			ops := []leafOp{{info: rfonts, local: "rFonts"}, {info: sz, local: "sz"}, {info: szcs, local: "szCs"}}
			if opts.BodyFont != "" {
				ops[0].active = true
				ops[0].attrs = rFontsLiteralAttrs(rfonts.attrs, opts.BodyFont)
				applied = append(applied, fmt.Sprintf("body font -> %s", opts.BodyFont))
			}
			if opts.BodySizePt != 0 {
				half := ptToHalfPoints(opts.BodySizePt)
				ops[1].active = true
				ops[1].attrs = setAttr(sz.attrs, "val", half)
				ops[2].active = true
				ops[2].attrs = setAttr(szcs.attrs, "val", half)
				applied = append(applied, fmt.Sprintf("body size -> %gpt", opts.BodySizePt))
			}
			patches = append(patches, applyLeafOps(styles, rpr.closeStart, ops)...)
		}

		if wantPPrChain {
			if !dd.found || !ppd.found || !ppr.found {
				return nil, nil, fmt.Errorf(
					"docx: styles.xml has no <w:docDefaults><w:pPrDefault><w:pPr> chain; cannot set line spacing/alignment")
			}
			ops := []leafOp{{info: spacing, local: "spacing"}, {info: jc, local: "jc"}}
			if opts.LineSpacing != 0 {
				ops[0].active = true
				line := lineSpacingTo240ths(opts.LineSpacing)
				ops[0].attrs = setAttr(setAttr(spacing.attrs, "line", line), "lineRule", "auto")
				applied = append(applied, fmt.Sprintf("line spacing -> %g", opts.LineSpacing))
			}
			if opts.Align != "" {
				ops[1].active = true
				ops[1].attrs = setAttr(jc.attrs, "val", opts.Align)
				applied = append(applied, fmt.Sprintf("alignment -> %s", opts.Align))
			}
			patches = append(patches, applyLeafOps(styles, ppr.closeStart, ops)...)
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
func planHeadingFontPatches(styles []byte, font string) ([]Patch, error) {
	dec := xml.NewDecoder(bytes.NewReader(styles))
	var prevOffset int
	var patches []Patch

	var inHeading bool
	var pPrDepth int
	var rprSeen bool
	var lookingForRFonts bool

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
					lookingForRFonts = false
				}
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
					lookingForRFonts = true
				}
			case inHeading && lookingForRFonts && isWordElement(t.Name, "rFonts"):
				newTag := buildTag("rFonts", rFontsLiteralAttrs(t.Attr, font), true)
				patches = append(patches, PatchRawSpan(styles, span, newTag))
				lookingForRFonts = false
			}

		case xml.EndElement:
			switch {
			case isWordElement(t.Name, "pPr") && pPrDepth > 0:
				pPrDepth--
			case isWordElement(t.Name, "rPr") && inHeading && rprSeen:
				if lookingForRFonts {
					// The style's rPr closed without ever containing
					// rFonts: insert one as its only child. prevOffset is
					// exactly where </w:rPr> begins, i.e. right after
					// whatever (if anything) rPr already contains.
					patches = append(patches, PatchRawSpan(styles, Span{prevOffset, prevOffset},
						buildTag("rFonts", rFontsLiteralAttrs(nil, font), true)))
					lookingForRFonts = false
				}
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
