package docx

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
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
	// BodyFont, if non-"", replaces the document's default LATIN font: it
	// lands on styles.xml's <w:docDefaults><w:rPrDefault><w:rPr><w:rFonts>
	// AND, wherever it would otherwise be silently shadowed, on the SAME
	// rFonts of Normal and any style basedOn it (transitively) other than
	// the Heading1..9/Title/Subtitle, Header/Footer/Caption, Quote, and
	// SourceCode families — see planStyleChainShadowPatches. Only ascii/
	// hAnsi are ever touched; an existing eastAsia (and cs) font is left
	// completely alone, so a CJK document's Chinese/Japanese/Korean font
	// survives a Latin body-font change untouched (format capability
	// review, Important 8; write review, I5). FormatResult.Notes names
	// whichever excluded style still shadows this rule AND is actually
	// referenced by a paragraph (a heading no caller uses is not worth a
	// caveat about) and any paragraph/run still carrying direct formatting
	// that does the same; Quote/SourceCode's own shadowing is never
	// mentioned — see isSilentExclusionStyle.
	BodyFont string
	// BodySizePt, if non-zero, replaces the document's default font size in
	// points: it lands on docDefaults's <w:sz> AND <w:szCs> (kept in sync so
	// CJK/complex-script runs don't keep the old size), plus the same pair
	// wherever a style basedOn Normal (per BodyFont's own exclusions above)
	// already declares its own size, shadowing docDefaults otherwise.
	BodySizePt float64
	// LineSpacing, if non-zero, is a multiple of a single line (1.0, 1.15,
	// 2.0, ...): it lands on docDefaults's <w:pPrDefault><w:pPr><w:spacing>
	// as w:line (240ths of a line) with w:lineRule="auto", plus the same
	// <w:spacing>'s w:line wherever a style basedOn Normal (per BodyFont's
	// own exclusions) already sets one, shadowing docDefaults otherwise —
	// e.g. docx_write's own BodyText style (format capability review,
	// Critical 4; write review, I4).
	LineSpacing float64
	// Align, if non-"", is one of "left"/"center"/"right"/"justify": it
	// lands on docDefaults's <w:pPrDefault><w:pPr><w:jc>, plus the same
	// <w:jc> wherever a style basedOn Normal (per BodyFont's own
	// exclusions) already sets one.
	Align string
	// BodyEastAsiaFont, if non-"", replaces the document's default EAST ASIAN
	// font ONLY: it lands on docDefaults' rPr's <w:rFonts w:eastAsia=...>,
	// dropping any competing eastAsiaTheme, and on the SAME rFonts of
	// Normal and any style basedOn it (per BodyFont's own exclusions) that
	// already declares its own eastAsia font, shadowing docDefaults
	// otherwise. It never touches ascii/hAnsi/cs — orthogonal to BodyFont
	// (task 8 brief, §2; format capability review, Important 8's own
	// "中文宋体+西文 Times 表达不了" complaint). With a paragraph range
	// (StartPara/EndPara), it lands on each targeted run's own
	// <w:rPr><w:rFonts w:eastAsia=...> instead. With HeadingFont in the
	// SAME call, it also sets Heading1..9's own eastAsia font — see
	// HeadingFont's own doc comment for why HeadingFont alone no longer
	// touches eastAsia at all.
	BodyEastAsiaFont string
	// FirstLineIndentChars, if non-zero, sets a first-line indent measured
	// in CHARACTER widths (2 is the conventional opening indent for a
	// Chinese paragraph — see styles.go's bodyTextStyleXML for this
	// package's own docx_write equivalent): it lands on <w:ind
	// w:firstLineChars="n*100"/> (hundredths of a character — the unit
	// Word actually renders relative to the paragraph's current font
	// size) AND, as a fixed-size fallback for any reader that ignores
	// firstLineChars, w:firstLine in twips — see firstLineTwipsFromChars's
	// doc comment for the fixed points-per-character assumption behind
	// that fallback value. Lands on docDefaults' pPr (plus the style-chain
	// rewrite, per BodyFont's own exclusions) whole-document, or each
	// targeted paragraph's own <w:pPr><w:ind> with a range.
	FirstLineIndentChars float64
	// SpaceBeforePt/SpaceAfterPt, if non-zero, set paragraph spacing
	// before/after in points: they land as w:before/w:after (twips,
	// pt*20) on the SAME <w:spacing> element LineSpacing/
	// LineSpacingExactPt land w:line/w:lineRule on — CT_PPr only has one
	// <w:spacing> child, so a call setting more than one of these fields
	// together always merges into a single tag, never two.
	SpaceBeforePt float64
	SpaceAfterPt  float64
	// LineSpacingExactPt, if non-zero, sets a FIXED line height in points
	// (as opposed to LineSpacing's multiple-of-a-single-line): it lands on
	// <w:spacing w:line="pt*20" w:lineRule="exact"/> instead of
	// LineSpacing's w:lineRule="auto". Mutually exclusive with
	// LineSpacing — both non-zero in the same call is a validation error
	// (resolveFormatOptions/formatDirectRange), since they would
	// otherwise silently fight over the same w:line/w:lineRule attributes.
	LineSpacingExactPt float64
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
	// TotalParas is the document's paragraph count AFTER this call, i.e.
	// Document.TotalParas() immediately after Format returns — the same
	// field EditResult already reports (edit.go), for the same reason: a
	// caller needs it regardless of whether ParaCountChanged is true, to
	// validate a range it is about to request next.
	TotalParas int
	// ParaCountChanged reports whether TotalParas differs from the
	// paragraph count when this call started. Normalize is the only
	// FormatOptions field that can make this true (it deletes empty
	// paragraphs); every other field only ever changes formatting, never
	// paragraph count. When true, every paragraph index a caller obtained
	// from an earlier docx_read (or an earlier docx_format range) may now
	// point at the wrong paragraph — see the tool layer's docxIndexAdvice,
	// which this field is what triggers (task 10 brief, item 1 / seams
	// review C2: "normalize 改段落数却无索引失效信号").
	ParaCountChanged bool
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

// wordNamespaceRootPrefix returns the literal XML prefix used on partXML's
// root element — "w" for "<w:styles>"/"<w:document>", "" for a bare
// "<styles>" — using xml.Decoder.RawToken rather than the ordinary Token:
// RawToken never resolves a prefix against its xmlns declaration, so a
// document that DECLARES the wordprocessingml namespace under some OTHER
// prefix ("<ns0:document xmlns:ns0=\"...wordprocessingml/2006/main\">", a
// real shape some non-Word tools produce) is still reported as "ns0", not
// silently normalized to "w" the way ordinary Token's namespace resolution
// would report it (isWordprocessingMLNamespace, used for READING elsewhere
// in this package, deliberately treats both as the same URI — the gap is
// entirely on the WRITE side: buildTag always emits a literal "w:" prefix,
// which is only valid when the document actually declares/uses that exact
// prefix). ok is false only if partXML has no root element at all (empty or
// malformed content).
func wordNamespaceRootPrefix(partXML []byte) (prefix string, ok bool) {
	dec := xml.NewDecoder(bytes.NewReader(partXML))
	for {
		tok, err := dec.RawToken()
		if err != nil {
			return "", false
		}
		if se, isStart := tok.(xml.StartElement); isStart {
			return se.Name.Space, true
		}
	}
}

// requireWordNamespacePrefix returns a descriptive error if partXML's root
// element does not use the literal "w:" prefix for wordprocessingml (or no
// prefix at all is also refused — buildTag's hardcoded "<w:..." emission
// has no way to honor either), rather than letting Format proceed to
// insert content under an UNDECLARED "w" prefix, which is what happened
// silently before (format capability review, Important 9 / task 9 brief,
// item 5): identify-able-or-not, this package refuses definitively instead
// of ever writing a byte-corrupt or namespace-broken part. name is only
// used to phrase the "cannot be read" fallback; the caller-facing message
// intentionally does not name which part, matching the brief's exact
// wording.
func requireWordNamespacePrefix(name string, partXML []byte) error {
	prefix, ok := wordNamespaceRootPrefix(partXML)
	if !ok {
		return fmt.Errorf("docx: %s could not be read as XML", name)
	}
	if prefix != "w" {
		return errors.New("docx: document uses a non-standard namespace prefix; formatting is not supported for this file")
	}
	return nil
}

// patchIsNoop reports whether applying p would leave its target span
// byte-identical to what it already is. Every patch pkg/docx's formatting
// code builds is Raw (PatchRawSpan, never PatchRun — see planStylesPatches,
// planMarginPatches, applyLeafOps, applyDirectRunFormat/applyDirectParaFormat),
// which snapshots the pre-patch bytes into Old at scan time (PatchRawSpan's
// own doc comment): comparing that snapshot to NewText is exactly the
// byte-level idempotency check task 10 brief item 3 requires ("幂等判定必须
// 是字节级（splice 结果与原字节比较），不是‘applied 为空’这种弱信号"). A
// non-raw patch (never produced by this package's own formatting code, but
// checked here defensively) is never treated as a no-op — Old there tracks
// PatchRun's own <w:t> content, not a full-tag replacement, and comparing it
// against NewText the way a raw patch's is compared would be meaningless.
func patchIsNoop(p Patch) bool {
	return p.Raw && p.Old != nil && string(p.Old) == p.NewText
}

// filterChangedPatches drops every patch in patches that patchIsNoop reports
// as a no-op, keeping Apply's overlap/ordering invariants intact (fewer
// patches, same non-overlapping spans). Used by the paths that do not
// already pre-suppress a leaf's own request field to ""/0 before building a
// patch for it (planMarginPatches, applyDirectRunFormat, applyDirectParaFormat)
// — the whole-document docDefaults/style-chain path achieves the same
// no-rewrite-when-unchanged result earlier, per leaf, via tagUnchanged/
// attrEquals suppression (see planStylesPatches' "eff*" locals), so it does
// not need this filter, but running it there too would be a no-op in
// itself.
func filterChangedPatches(patches []Patch) []Patch {
	out := make([]Patch, 0, len(patches))
	for _, p := range patches {
		if !patchIsNoop(p) {
			out = append(out, p)
		}
	}
	return out
}

// stylesMissingErr builds a rich error for "package has no word/styles.xml
// part": names exactly which of resolved's requested rules cannot be
// applied without it, and — when the SAME call also asked for margins_mm
// and/or normalize, which only ever touch word/document.xml — says those
// are unaffected and can still be retried on their own (format capability
// review, Important 10 / task 9 brief, item 4): a bare "no styles.xml"
// error left a caller no way to tell a document-wide font/size/spacing
// request apart from ones margins/normalize could have satisfied anyway.
func stylesMissingErr(resolved FormatOptions) error {
	var affected []string
	if resolved.HeadingFont != "" {
		affected = append(affected, "heading_font")
	}
	if resolved.BodyFont != "" {
		affected = append(affected, "body_font")
	}
	if resolved.BodyEastAsiaFont != "" {
		affected = append(affected, "body_east_asia_font")
	}
	if resolved.BodySizePt != 0 {
		affected = append(affected, "body_size_pt")
	}
	if resolved.LineSpacing != 0 {
		affected = append(affected, "line_spacing")
	}
	if resolved.LineSpacingExactPt != 0 {
		affected = append(affected, "line_spacing_exact_pt")
	}
	if resolved.Align != "" {
		affected = append(affected, "align")
	}
	if resolved.FirstLineIndentChars != 0 {
		affected = append(affected, "first_line_indent_chars")
	}
	if resolved.SpaceBeforePt != 0 {
		affected = append(affected, "space_before_pt")
	}
	if resolved.SpaceAfterPt != 0 {
		affected = append(affected, "space_after_pt")
	}
	msg := fmt.Sprintf("docx: package has no word/styles.xml part; %s cannot be applied without it — retry the call without %s",
		strings.Join(affected, ", "), strings.Join(affected, "/"))
	if resolved.MarginsMM != nil || resolved.Normalize {
		msg += "; margins_mm/normalize only touch word/document.xml and are unaffected, so they can still be applied on their own"
	}
	return errors.New(msg)
}

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

	// totalBefore is captured before ANY mutation, so it reflects the
	// document as it stood when this call started -- the "before" half of
	// ParaCountChanged's before/after comparison (task 10 brief, item 1 /
	// seams review C2).
	totalBefore := d.TotalParas()

	var result FormatResult

	wantsStyles := resolved.BodyFont != "" || resolved.BodySizePt != 0 ||
		resolved.LineSpacing != 0 || resolved.LineSpacingExactPt != 0 || resolved.Align != "" ||
		resolved.HeadingFont != "" || resolved.BodyEastAsiaFont != "" ||
		resolved.FirstLineIndentChars != 0 || resolved.SpaceBeforePt != 0 || resolved.SpaceAfterPt != 0
	wantsMargins := resolved.MarginsMM != nil
	// wantsDirectFormatMaskingScan mirrors the condition guarding the
	// masking-notes block near the end of this function: both need
	// word/document.xml, just for a read-only scan rather than a mutation.
	wantsDirectFormatMaskingScan := resolved.BodyFont != "" || resolved.BodySizePt != 0 ||
		resolved.LineSpacing != 0 || resolved.LineSpacingExactPt != 0 || resolved.Align != "" ||
		resolved.BodyEastAsiaFont != "" || resolved.FirstLineIndentChars != 0 ||
		resolved.SpaceBeforePt != 0 || resolved.SpaceAfterPt != 0
	wantsDocumentPart := wantsMargins || resolved.Normalize || wantsDirectFormatMaskingScan

	// Task 10 brief item 5 (task 9 review follow-up): validate BOTH parts'
	// namespace prefix up front, before mutating anything, rather than
	// interleaved with each block's own mutation the way the pre-task-10
	// code had it -- which let a call needing BOTH parts (e.g. body_font:
	// styles.xml's docDefaults AND, via the masking scan below,
	// document.xml) rewrite styles.xml in memory via d.SetPart and only
	// THEN discover document.xml's prefix was invalid, leaving this
	// Document half-applied even though the error return means nothing
	// reaches disk. Checking both here, before either SetPart call, makes
	// the whole operation all-or-nothing again.
	if wantsStyles {
		styles, ok := d.Part("word/styles.xml")
		if !ok {
			return FormatResult{}, stylesMissingErr(resolved)
		}
		if err := requireWordNamespacePrefix("word/styles.xml", styles); err != nil {
			return FormatResult{}, err
		}
	}
	if wantsDocumentPart {
		doc, ok := d.Part(DocumentPart)
		if !ok {
			return FormatResult{}, fmt.Errorf("docx: package has no %s part", DocumentPart)
		}
		if err := requireWordNamespacePrefix(DocumentPart, doc); err != nil {
			return FormatResult{}, err
		}
	}

	if wantsStyles {
		// Already validated above; d.Part is re-read here (rather than
		// reusing the slice from the precheck) because nothing else has
		// touched this part in between, so this is simply the same bytes.
		styles, _ := d.Part("word/styles.xml")
		patches, applied, notes, err := planStylesPatches(styles, resolved, usedStyleIDs(d.Paras()))
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
		result.Notes = append(result.Notes, notes...)
	}

	if wantsMargins || resolved.Normalize {
		// Re-read: wantsStyles' block above may have run (styles.xml only,
		// never document.xml), so this is still document.xml's pristine
		// bytes -- already namespace-validated above.
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

	// docDefaults and the style chain are only the STYLE layer of Word's
	// cascade; direct formatting on a specific paragraph/run (e.g. a
	// previous docx_format start_para/end_para call, or hand-authored
	// content) outranks even a correctly-rewritten style. Report that
	// honestly instead of letting Applied read as "every paragraph now
	// looks like this" when some do not (format capability review,
	// Critical 4 / §2's masking-detection requirement).
	if wantsDirectFormatMaskingScan {
		// Re-read: the margins/normalize block above may have rewritten
		// document.xml via d.SetPart, so this must be the CURRENT bytes, not
		// the ones validated in the precheck -- but since neither this
		// package's patches nor SetPart ever change the root element's own
		// namespace declaration, that earlier validation still holds; no
		// need to check again.
		doc, ok := d.Part(DocumentPart)
		if !ok {
			return FormatResult{}, fmt.Errorf("docx: package has no %s part", DocumentPart)
		}
		maskNotes, err := directFormatMaskingNotes(doc, d.Paras(), resolved)
		if err != nil {
			return FormatResult{}, fmt.Errorf("docx: scan for direct-formatting overrides: %w", err)
		}
		result.Notes = append(result.Notes, maskNotes...)
	}

	// TotalParas/ParaCountChanged mirror EditResult's own fields (edit.go):
	// Normalize is the only field above that can ever change paragraph
	// count, but this is computed generically (compare before/after) rather
	// than special-cased on Normalize, the same way edit.go does it.
	result.TotalParas = d.TotalParas()
	result.ParaCountChanged = result.TotalParas != totalBefore
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

	if err := validateAlignAndLineSpacingMutex(resolved.Align, resolved.LineSpacing, resolved.LineSpacingExactPt); err != nil {
		return FormatOptions{}, err
	}
	if err := validateNonNegativeMeasurements(resolved); err != nil {
		return FormatOptions{}, err
	}
	if err := validateMargins(resolved.MarginsMM); err != nil {
		return FormatOptions{}, err
	}
	return resolved, nil
}

// validateAlignAndLineSpacingMutex enforces the two cross-field rules
// shared by BOTH Format paths (whole-document, via resolveFormatOptions,
// and paragraph-range, via formatDirectRange): Align must be one of the
// four values <w:jc> supports, and LineSpacing/LineSpacingExactPt are
// mutually exclusive — giving both would otherwise silently fight over the
// same <w:spacing> w:line/w:lineRule attributes, whichever leafOp happened
// to be built last winning with no error at all (task 8 brief's explicit
// "参数校验层拒绝" requirement).
func validateAlignAndLineSpacingMutex(align string, lineSpacing, lineSpacingExactPt float64) error {
	switch align {
	case "", "left", "center", "right", "justify":
	default:
		return fmt.Errorf("docx: unknown alignment %q; want \"left\", \"center\", \"right\", or \"justify\"", align)
	}
	if lineSpacing != 0 && lineSpacingExactPt != 0 {
		return fmt.Errorf("docx: line_spacing and line_spacing_exact_pt are mutually exclusive; give at most one")
	}
	return nil
}

// validateNonNegativeMeasurements rejects a NEGATIVE value for any of task
// 8's four measurement fields (FirstLineIndentChars/SpaceBeforePt/
// SpaceAfterPt/LineSpacingExactPt), shared by both Format paths the same
// way validateAlignAndLineSpacingMutex is (review F6). None of the four has
// a sensible negative meaning: a negative first-line indent, spacing, or
// line height is a caller mistake to report, not silently apply. Zero is
// deliberately NOT rejected here — it already means "not requested" for
// every FormatOptions numeric field throughout this package (the same
// convention BodySizePt/LineSpacing already rely on), so this validator
// cannot and does not try to distinguish an explicit zero from an absent
// field; the tool layer (docx.go's docxFormatPositiveNumberArg), which DOES
// see whether the JSON key was present at all, is stricter and rejects an
// explicit zero too.
func validateNonNegativeMeasurements(opts FormatOptions) error {
	if opts.FirstLineIndentChars < 0 {
		return fmt.Errorf("docx: first_line_indent_chars %g must not be negative", opts.FirstLineIndentChars)
	}
	if opts.SpaceBeforePt < 0 {
		return fmt.Errorf("docx: space_before_pt %g must not be negative", opts.SpaceBeforePt)
	}
	if opts.SpaceAfterPt < 0 {
		return fmt.Errorf("docx: space_after_pt %g must not be negative", opts.SpaceAfterPt)
	}
	if opts.LineSpacingExactPt < 0 {
		return fmt.Errorf("docx: line_spacing_exact_pt %g must not be negative", opts.LineSpacingExactPt)
	}
	return nil
}

// mergeFormatOptions returns explicit with every zero-valued field
// ("" / 0 / nil) filled in from tmpl. explicit's non-zero fields always
// win, which is how "显式给出的字段覆盖模板值" (explicit fields override the
// template) is implemented.
//
// LineSpacing/LineSpacingExactPt are treated as ONE pair, not two
// independent fields: review F5 caught a caller who explicitly sets ONLY
// LineSpacingExactPt (leaving LineSpacing at its zero value) together with
// a template that sets LineSpacing (every built-in template does) ending
// up with BOTH fields non-zero after a naive per-field merge — tripping
// validateAlignAndLineSpacingMutex's mutual-exclusion check even though
// the caller never actually asked for two conflicting things, only one
// explicit field plus whatever the template contributes. If explicit
// already set EITHER half of the pair, the template contributes NEITHER
// half; only when explicit set neither does the template's own value (of
// either field) apply.
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
	switch {
	case out.LineSpacing != 0 || out.LineSpacingExactPt != 0:
		// explicit already picked one half of the pair -- never let the
		// template's OTHER half leak in and create a spurious conflict.
	case tmpl.LineSpacing != 0:
		out.LineSpacing = tmpl.LineSpacing
	case tmpl.LineSpacingExactPt != 0:
		out.LineSpacingExactPt = tmpl.LineSpacingExactPt
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

// ptToTwips converts points to twips (20 twips per point) for
// w:spacing's w:before/w:after and its w:line when w:lineRule="exact":
// 6pt -> 120, 24pt -> 480.
func ptToTwips(pt float64) string {
	return fmt.Sprintf("%d", int(math.Round(pt*20)))
}

// firstLineCharsHundredths converts a character count to w:ind's
// w:firstLineChars unit (hundredths of a character, the value Word
// actually renders relative to the paragraph's own current font size):
// 2 -> "200".
func firstLineCharsHundredths(chars float64) string {
	return fmt.Sprintf("%d", int(math.Round(chars*100)))
}

// twipsPerIndentChar is the fixed twips-per-character assumption behind
// w:firstLine's fallback value (for a reader that ignores firstLineChars
// entirely): one full-width CJK character renders at roughly its font
// size's width, and this package's own docx_write output already bakes in
// exactly this assumption — styles.go's bodyTextStyleXML hardcodes
// `<w:ind w:firstLine="420"/>` for a two-character indent, and
// reference-values.md spells out why: "firstLine="420" = 2 个中文字符
// (10.5pt 时约 420 twips)", i.e. 210 twips/char = 10.5pt * 20. Reusing
// that exact constant here keeps FirstLineIndentChars' twips fallback
// consistent with this package's own generator instead of inventing a
// second, different approximation.
const twipsPerIndentChar = 210

// firstLineTwipsFromChars converts a character count to the w:firstLine
// twips fallback — see twipsPerIndentChar's own doc comment for the
// assumption behind the conversion factor: 2 -> "420".
func firstLineTwipsFromChars(chars float64) string {
	return fmt.Sprintf("%d", int(math.Round(chars*twipsPerIndentChar)))
}

// attrEquals reports whether attrs carries local literally equal to want —
// the per-attribute analogue of tagUnchanged (below), needed wherever more
// than one independently-requested field lands on the SAME element
// (rFonts' ascii/hAnsi vs eastAsia; spacing's line/lineRule vs before vs
// after; ind's firstLineChars/firstLine) and each field's own "did THIS
// specific thing change" status must not be polluted by whichever sibling
// attribute on that same tag also changed.
func attrEquals(attrs []xml.Attr, local, want string) bool {
	v, ok := wordAttrVal(xml.StartElement{Attr: attrs}, local)
	return ok && v == want
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

// rFontsLatinAndEastAsiaAttrs sets ascii/hAnsi to latinFont (when non-"")
// and eastAsia to eastAsiaFont (when non-""), each COMPLETELY
// independently — the shared plumbing behind BodyFont/BodyEastAsiaFont's
// orthogonality (task 8 brief, §2) and, combined, HeadingFont+
// BodyEastAsiaFont's (planHeadingFontPatches). Dropping a field's own *Theme
// counterpart (asciiTheme/hAnsiTheme for latinFont, eastAsiaTheme for
// eastAsiaFont — Word resolves a *Theme attribute in preference to a
// literal face name, silently ignoring the literal one otherwise) only
// ever happens when that field's OWN literal value is being set, so a call
// that only touches one of the two pairs never disturbs whatever the OTHER
// pair (literal or *Theme) already had. cs/csTheme are never touched by
// either field: complex-script runs are out of scope for BodyFont,
// HeadingFont, AND BodyEastAsiaFont alike (format capability review, Important
// 8, and its heading-path follow-up — task 8 brief's own "heading_font 路径
// 也不应再把 eastAsia 打成 Latin 字体" note).
//
// This is the SOLE font-attrs builder for BodyFont everywhere now —
// whole-document (docDefaults, the style-chain rewrite), HeadingFont, AND
// the paragraph-range direct-formatting path (format_direct.go's
// planRunRPrPatches/buildRunRPr) all use it: an earlier version of the
// range path kept its own separate rFontsDirectRangeAttrs that special-
// cased "no BodyEastAsiaFont given" to fall back to the OLD literal-all-
// four behavior, which review F3 caught as contradicting this very
// function's own doc comment ("BodyFont only ever touches ascii/hAnsi") —
// the range path was silently the one exception. Removed; body_font on a
// range now leaves an existing eastAsia/cs font alone exactly like the
// whole-document path always has.
func rFontsLatinAndEastAsiaAttrs(existing []xml.Attr, latinFont, eastAsiaFont string) []xml.Attr {
	kept := existing
	if latinFont != "" {
		kept = dropAttrs(kept, "asciiTheme", "hAnsiTheme", "ascii", "hAnsi")
		kept = append(kept,
			xml.Attr{Name: xml.Name{Local: "ascii"}, Value: latinFont},
			xml.Attr{Name: xml.Name{Local: "hAnsi"}, Value: latinFont},
		)
	}
	if eastAsiaFont != "" {
		kept = dropAttrs(kept, "eastAsiaTheme", "eastAsia")
		kept = append(kept, xml.Attr{Name: xml.Name{Local: "eastAsia"}, Value: eastAsiaFont})
	}
	return kept
}

// rFontsLatinUnchanged/rFontsEastAsiaUnchanged report whether attrs already
// renders font literally for ascii+hAnsi (respectively eastAsia alone),
// with no *Theme attribute competing for precedence — i.e. whether
// rFontsLatinAndEastAsiaAttrs(attrs, font, "") / ("", font) would be a
// byte-for-byte no-op for that ONE field. Needed (instead of the whole-tag
// tagUnchanged below) wherever BodyFont and BodyEastAsiaFont might BOTH be
// requested against the SAME <w:rFonts> in one call: each field's own
// "did this actually change" status must not be polluted by whatever the
// OTHER field's value already was.
func rFontsLatinUnchanged(attrs []xml.Attr, font string) bool {
	return !hasAttr(attrs, "asciiTheme") && !hasAttr(attrs, "hAnsiTheme") &&
		attrEquals(attrs, "ascii", font) && attrEquals(attrs, "hAnsi", font)
}

func rFontsEastAsiaUnchanged(attrs []xml.Attr, font string) bool {
	return !hasAttr(attrs, "eastAsiaTheme") && attrEquals(attrs, "eastAsia", font)
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

// rPrChildOrder is CT_RPr's child sequence: EG_RPrBase's own sequence
// (ECMA-376 §17.3.1.24's rPr/rStyle-through-oMath group, in schema order),
// followed by CT_RPr's own trailing rPrChange, which must always be LAST
// (it holds a revision's historical run properties). This is rPr's
// counterpart to pPrChildOrder above — same purpose, same technique (task
// 8 brief, §3a; a gap Task 1's own report explicitly flagged as left open:
// "rPr 的其余 EG_RPrBase 属性... 不在本任务范围内"). Format only ever edits
// rFonts/sz/szCs itself, but the FULL list is needed as an anchor set: a
// newly inserted <w:sz>/<w:szCs> must land immediately before whichever
// LATER sibling in this order already exists — most importantly
// <w:rPrChange>, since CT_RPr requires it be the LAST child, so blindly
// inserting at the container's end (the pre-Task-8 behavior) would land
// AFTER it, producing well-formed but schema-illegal XML — the same
// failure mode as Critical 1, one level down. rFonts itself is NOT
// inserted through this anchor set: EG_RPrBase requires it precede every
// other run property, so it is always anchored directly to the
// container's own opening position instead (planRPrFontSizePatches).
var rPrChildOrder = []string{
	"rStyle", "rFonts", "b", "bCs", "i", "iCs", "caps", "smallCaps",
	"strike", "dstrike", "outline", "shadow", "emboss", "imprint",
	"noProof", "snapToGrid", "vanish", "webHidden", "color", "spacing",
	"w", "kern", "position", "sz", "szCs", "highlight", "u", "effect",
	"bdr", "shd", "fitText", "vertAlign", "rtl", "cs", "em", "lang",
	"eastAsianLayout", "specVanish", "oMath",
	"rPrChange",
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

// pParaRequest bundles every pPr-level field Format's whole-document and
// paragraph-range paths can request against a pPr's own spacing/ind/jc
// leaves, so buildPPrOps' signature doesn't keep growing one positional
// float64/string parameter at a time as Task 8 adds first-line indent and
// before/after spacing alongside the pre-existing line-spacing-multiple/
// align pair. LineSpacing and LineSpacingExactPt are mutually exclusive
// (validated before a request is ever built — resolveFormatOptions/
// formatDirectRange); SpaceBeforePt/SpaceAfterPt land on the SAME
// <w:spacing> element as whichever line-spacing field is set, never a
// second one, since CT_PPr only has one <w:spacing> child.
type pParaRequest struct {
	LineSpacing          float64
	LineSpacingExactPt   float64
	SpaceBeforePt        float64
	SpaceAfterPt         float64
	Align                string
	FirstLineIndentChars float64
}

// isZero reports whether req asks for nothing at all — the no-op case
// applyDirectParaFormat short-circuits on, mirroring
// applyDirectRunFormat's own "font == "" && sizePt == 0" check.
func (req pParaRequest) isZero() bool {
	return req.LineSpacing == 0 && req.LineSpacingExactPt == 0 &&
		req.SpaceBeforePt == 0 && req.SpaceAfterPt == 0 &&
		req.Align == "" && req.FirstLineIndentChars == 0
}

// buildPPrOps returns the leafOps applyLeafOps (or renderActiveLeaves, for a
// brand new pPr being synthesized from scratch) needs to land <w:spacing>/
// <w:ind>/<w:jc> at the schema-correct position among a pPr's other
// children: one leafOp per name in pPrChildOrder, in that order, with
// "spacing"/"ind"/"jc" marked active (and given their new attributes) when
// req asks for a change, and every other name serving purely as an
// ANCHOR — present (when found in children) so a new element merges in
// immediately before whichever later, otherwise-untracked sibling (rPr,
// sectPr, ...) already exists, instead of always landing at pPr's very end
// (format capability review, Critical 1). children may be nil — reading a
// nil map always returns the zero elemInfo (not found) — which is exactly
// "no pPr exists yet to anchor against", the shape a brand new pPr needs.
//
// "spacing" combines LineSpacing/LineSpacingExactPt (w:line/w:lineRule) AND
// SpaceBeforePt/SpaceAfterPt (w:before/w:after) into ONE tag/patch, since
// they are all attributes of the SAME <w:spacing> element — building two
// separate patches against the same span would make Apply reject the batch
// as overlapping.
func buildPPrOps(children map[string]elemInfo, req pParaRequest) []leafOp {
	ops := make([]leafOp, 0, len(pPrChildOrder))
	for _, name := range pPrChildOrder {
		op := leafOp{info: children[name], local: name}
		switch name {
		case "spacing":
			attrs := op.info.attrs
			active := false
			switch {
			case req.LineSpacing != 0:
				attrs = setAttr(setAttr(attrs, "line", lineSpacingTo240ths(req.LineSpacing)), "lineRule", "auto")
				active = true
			case req.LineSpacingExactPt != 0:
				attrs = setAttr(setAttr(attrs, "line", ptToTwips(req.LineSpacingExactPt)), "lineRule", "exact")
				active = true
			}
			if req.SpaceBeforePt != 0 {
				// review F2: w:beforeAutospacing set means Word ignores
				// any explicit w:before entirely (auto-computed instead)
				// -- drop it whenever we write a real w:before value, the
				// same "one wins, the other is silently ignored" relation
				// w:hanging has with w:firstLine (see the "ind" case
				// below).
				attrs = dropAttrs(attrs, "beforeAutospacing")
				attrs = setAttr(attrs, "before", ptToTwips(req.SpaceBeforePt))
				active = true
			}
			if req.SpaceAfterPt != 0 {
				attrs = dropAttrs(attrs, "afterAutospacing")
				attrs = setAttr(attrs, "after", ptToTwips(req.SpaceAfterPt))
				active = true
			}
			op.active = active
			op.attrs = attrs
		case "ind":
			if req.FirstLineIndentChars != 0 {
				op.active = true
				// review F2: CT_Ind's hanging indent always wins over
				// firstLine when both are present (Word ignores firstLine
				// entirely) -- drop any pre-existing w:hanging/
				// w:hangingChars whenever we write a real firstLine, or
				// the new value would be silently invisible.
				attrs := dropAttrs(op.info.attrs, "hanging", "hangingChars")
				op.attrs = setAttr(setAttr(attrs,
					"firstLineChars", firstLineCharsHundredths(req.FirstLineIndentChars)),
					"firstLine", firstLineTwipsFromChars(req.FirstLineIndentChars))
			}
		case "jc":
			if req.Align != "" {
				op.active = true
				op.attrs = setAttr(op.info.attrs, "val", req.Align)
			}
		}
		ops = append(ops, op)
	}
	return ops
}

// pPrDropNotes reports which of req's leaves would silently discard an
// existing attribute Word treats as mutually exclusive with the new one —
// the same two "one wins, the other becomes invisible" pairs buildPPrOps'
// own "spacing"/"ind" cases already drop without comment (review F2): a
// pre-existing w:hanging/w:hangingChars when FirstLineIndentChars sets
// w:firstLine (CT_Ind always prefers hanging over firstLine when both are
// present, so leaving it in place would make the new firstLine silently
// invisible), and a pre-existing w:beforeAutospacing/w:afterAutospacing
// when SpaceBeforePt/SpaceAfterPt sets a literal w:before/w:after (Word
// ignores the literal value entirely whenever the auto-spacing flag is
// set). Dropping these is correct — the alternative is a requested change
// that silently does nothing — but doing it with no caveat at all was the
// gap (task 9 brief, item 7b / task 8 review round 2's third nit): a
// caller who explicitly set a hanging indent or auto-spacing has no way to
// learn it was just removed. children may be nil (nothing exists yet to
// drop, e.g. a brand new pPr being synthesized from scratch), in which
// case this always returns nil.
func pPrDropNotes(children map[string]elemInfo, req pParaRequest) []string {
	var notes []string
	if req.FirstLineIndentChars != 0 {
		if ind, ok := children["ind"]; ok && (hasAttr(ind.attrs, "hanging") || hasAttr(ind.attrs, "hangingChars")) {
			notes = append(notes, "first_line_indent_chars removed an existing hanging indent (w:hanging/w:hangingChars), which would otherwise have taken precedence over the new first-line indent and made it invisible")
		}
	}
	if req.SpaceBeforePt != 0 {
		if sp, ok := children["spacing"]; ok && hasAttr(sp.attrs, "beforeAutospacing") {
			notes = append(notes, "space_before_pt removed an existing w:beforeAutospacing flag, which would otherwise have made the new space_before_pt value invisible")
		}
	}
	if req.SpaceAfterPt != 0 {
		if sp, ok := children["spacing"]; ok && hasAttr(sp.attrs, "afterAutospacing") {
			notes = append(notes, "space_after_pt removed an existing w:afterAutospacing flag, which would otherwise have made the new space_after_pt value invisible")
		}
	}
	return notes
}

// planRPrFontSizePatches builds the patches for one rPr's direct font/size
// formatting (an rPr that already exists with real content — the caller
// handles "no rPr at all" and "self-closing <w:rPr/>" itself), given
// containerCloseStart (rPr's own closeStart, the fallback insertion point
// when nothing later is found at all) and children, the FULL set of rPr's
// own direct children scanDirectChildren
// tracks against rPrChildOrder (not merely rFonts/sz/szCs) — used the same
// way buildPPrOps uses pPrChildOrder's full set: a newly inserted sz/szCs
// must land immediately before whichever LATER sibling in CT_RPr's schema
// order already exists (u, rPrChange, ...), never always at the container's
// end, which would land AFTER a trailing <w:rPrChange> — illegal, since
// CT_RPr requires rPrChange be its LAST child (task 8 brief, §3a).
//
// rFonts is now just another entry in the rPrChildOrder-walked ops list,
// exactly like sz/szCs — review F4 caught the PREVIOUS version's special
// case (always anchoring a newly inserted rFonts directly to the
// container's own opening position) violating EG_RPrBase order whenever a
// REAL earlier sibling existed: a run inside a hyperlink commonly carries
// <w:rPr><w:rStyle w:val="Hyperlink"/></w:rPr>, and rStyle must precede
// rFonts, but the old code's "always first" rule ignored that and inserted
// the new rFonts BEFORE rStyle regardless. Walking the FULL anchor list —
// the same technique already used for sz/szCs (task 8 brief, §3a) and for
// pPr's spacing/ind/jc (buildPPrOps) — lets applyLeafOps' ordinary
// found/missing merge logic place rFonts correctly relative to rStyle (or
// anything else) without any special-casing, and removes the manual
// "merge as a text prefix to dodge a same-offset patch collision" hack
// that special case needed.
//
// latinFont/eastAsiaFont are independent (BodyFont/BodyEastAsiaFont's own
// orthogonality) — rFonts is only ever marked active at all when at least
// one of them is non-"". fontAttrs builds the new <w:rFonts> attribute
// list from whatever the element already carried (nil when synthesizing a
// brand new one) — every caller now passes rFontsLatinAndEastAsiaAttrs
// (the whole-document docDefaults/style-chain path AND the paragraph-range
// direct-formatting path, format_direct.go — see that function's own doc
// comment for why the range path no longer has its own separate builder).
func planRPrFontSizePatches(doc []byte, containerCloseStart int, children map[string]elemInfo, latinFont, eastAsiaFont string, sizePt float64, fontAttrs func([]xml.Attr, string, string) []xml.Attr) []Patch {
	ops := make([]leafOp, 0, len(rPrChildOrder))
	for _, name := range rPrChildOrder {
		op := leafOp{info: children[name], local: name}
		switch {
		case name == "rFonts" && (latinFont != "" || eastAsiaFont != ""):
			op.active = true
			op.attrs = fontAttrs(op.info.attrs, latinFont, eastAsiaFont)
		case (name == "sz" || name == "szCs") && sizePt != 0:
			op.active = true
			op.attrs = setAttr(op.info.attrs, "val", ptToHalfPoints(sizePt))
		}
		ops = append(ops, op)
	}
	return applyLeafOps(doc, containerCloseStart, ops)
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
func scanDocDefaults(styles []byte) (dd, rpd, rpr elemInfo, rprChildren map[string]elemInfo, ppd, ppr elemInfo, pprChildren map[string]elemInfo, rootEnd int, err error) {
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
			return dd, rpd, rpr, rprChildren, ppd, ppr, pprChildren, rootEnd,
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
		rprChildren = scanDirectChildren(styles[rpr.tagSpan.End:rpr.closeStart], rpr.tagSpan.End, rPrChildOrder)
	}
	if ppr.found && ppr.selfClosing {
		ppr.closeStart = ppr.tagSpan.End
	}
	if ppr.found && !ppr.selfClosing {
		pprChildren = scanDirectChildren(styles[ppr.tagSpan.End:ppr.closeStart], ppr.tagSpan.End, pPrChildOrder)
	}
	return dd, rpd, rpr, rprChildren, ppd, ppr, pprChildren, rootEnd, nil
}

// planStylesPatches builds every styles.xml patch resolved requests
// (docDefaults body font/size/line-spacing/alignment plus the effective-
// chain rewrite that keeps a shadowing style in sync, and every Heading1..9's
// font) and a human-readable Applied entry per change ACTUALLY made.
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
//
// usedIDs (Format's usedStyleIDs(d.Paras()), or nil from a caller that only
// cares about the docDefaults/rewrite behavior, e.g. this package's own
// low-level docDefaults tests) is threaded straight through to
// planStyleChainShadowPatches' own usedIDs parameter — see that function's
// doc comment for why a style must actually be referenced by a paragraph
// before its shadowing is worth a note (task 7 follow-up review, F5).
//
// fontChanged/sizeChanged/spacingChanged/alignChanged track, independently
// of whether a patch OBJECT was produced, whether a docDefaults or
// style-chain leaf was actually rewritten to a DIFFERENT value than it
// already had (via tagUnchanged) — an Applied entry for a field is only
// ever appended when its own flag is true, mirroring the pre-existing
// HeadingFont branch's "len(headingPatches) > 0" gate below, so a second
// Format call with identical options reports an honestly empty Applied
// instead of re-claiming success for a no-op rewrite (task 7 follow-up
// review, F1).
func planStylesPatches(styles []byte, opts FormatOptions, usedIDs map[string]bool) ([]Patch, []string, []string, error) {
	var patches []Patch
	var applied []string
	var notes []string
	var fontChanged, sizeChanged, eastAsiaChanged bool
	var spacingChanged, spaceBeforeChanged, spaceAfterChanged, alignChanged, firstLineIndentChanged bool

	wantRPrChain := opts.BodyFont != "" || opts.BodySizePt != 0 || opts.BodyEastAsiaFont != ""
	wantPPrChain := opts.LineSpacing != 0 || opts.LineSpacingExactPt != 0 || opts.Align != "" ||
		opts.SpaceBeforePt != 0 || opts.SpaceAfterPt != 0 || opts.FirstLineIndentChars != 0

	if wantRPrChain || wantPPrChain {
		dd, rpd, rpr, rprChildren, ppd, ppr, pprChildren, rootEnd, err := scanDocDefaults(styles)
		if err != nil {
			return nil, nil, nil, err
		}
		rfonts, sz, szcs := rprChildren["rFonts"], rprChildren["sz"], rprChildren["szCs"]

		var rprInner, pprInner string
		needRPrSynthesis := false
		needPPrSynthesis := false

		if wantRPrChain {
			if dd.found && rpd.found && rpr.found && !rpr.selfClosing {
				// The chain fully exists already: edit its leaves in place —
				// byte-identical output for a document that already has a
				// complete rFonts/sz/szCs is a hard guarantee for the case
				// nothing here needed to move. planRPrFontSizePatches (not
				// the old bare applyLeafOps call) additionally makes sure a
				// newly inserted rFonts lands as rPr's first child rather
				// than after unrelated existing content (Minor 13), and a
				// newly inserted sz/szCs lands before whichever later
				// EG_RPrBase sibling (u, rPrChange, ...) already exists
				// (task 8 brief, §3a). rFontsLatinAndEastAsiaAttrs is
				// BodyFont/BodyEastAsiaFont's shared font-attrs builder: it
				// touches ascii/hAnsi and eastAsia completely independently
				// (format capability review, Important 8).
				//
				// effFont/effBodyEastAsiaFont/effSize are opts.BodyFont/
				// opts.BodyEastAsiaFont/opts.BodySizePt SUPPRESSED to ""/0 when
				// that ONE field already renders byte-identically to what's
				// requested (rFontsLatinUnchanged/rFontsEastAsiaUnchanged/
				// tagUnchanged) — planRPrFontSizePatches already treats
				// ""/0 as "not requested" for exactly this leaf, the same
				// code path a caller who only ever asked for ONE of the
				// fields already exercises, so no new merge/anchoring logic
				// is needed to get idempotency for free.
				effFont := opts.BodyFont
				if effFont != "" {
					if rFontsLatinUnchanged(rfonts.attrs, effFont) {
						effFont = ""
					} else {
						fontChanged = true
					}
				}
				effBodyEastAsiaFont := opts.BodyEastAsiaFont
				if effBodyEastAsiaFont != "" {
					if rFontsEastAsiaUnchanged(rfonts.attrs, effBodyEastAsiaFont) {
						effBodyEastAsiaFont = ""
					} else {
						eastAsiaChanged = true
					}
				}
				effSize := opts.BodySizePt
				if effSize != 0 {
					half := ptToHalfPoints(effSize)
					szTag := buildTag("sz", setAttr(sz.attrs, "val", half), true)
					szcsTag := buildTag("szCs", setAttr(szcs.attrs, "val", half), true)
					if tagUnchanged(styles, sz, szTag) && tagUnchanged(styles, szcs, szcsTag) {
						effSize = 0
					} else {
						sizeChanged = true
					}
				}
				patches = append(patches, planRPrFontSizePatches(
					styles, rpr.closeStart, rprChildren, effFont, effBodyEastAsiaFont, effSize, rFontsLatinAndEastAsiaAttrs)...)
			} else {
				// Either rPr is missing entirely, or it is PRESENT but
				// self-closing (<w:rPr/>, no properties at all) -- both
				// need the exact same brand-new leaf content, since a
				// self-closing rPr has no children to edit in place
				// either (rprChildren is nil/empty in both shapes, per
				// scanDocDefaults).
				if opts.BodyFont != "" {
					fontChanged = true // synthesis always creates something new
				}
				if opts.BodyEastAsiaFont != "" {
					eastAsiaChanged = true
				}
				if opts.BodySizePt != 0 {
					sizeChanged = true
				}
				ops := make([]leafOp, 0, len(rPrChildOrder))
				for _, name := range rPrChildOrder {
					op := leafOp{info: rprChildren[name], local: name}
					switch name {
					case "rFonts":
						if opts.BodyFont != "" || opts.BodyEastAsiaFont != "" {
							op.active = true
							op.attrs = rFontsLatinAndEastAsiaAttrs(rfonts.attrs, opts.BodyFont, opts.BodyEastAsiaFont)
						}
					case "sz", "szCs":
						if opts.BodySizePt != 0 {
							op.active = true
							op.attrs = setAttr(op.info.attrs, "val", ptToHalfPoints(opts.BodySizePt))
						}
					}
					ops = append(ops, op)
				}
				leaves := renderActiveLeaves(ops)
				if rpr.found && rpr.selfClosing {
					// Expand the self-closing tag IN PLACE so the new
					// leaves land as its own children, not as trailing
					// SIBLINGS of it (task 9 brief, item 6 / Task 8 review
					// round 2's old debt: "<w:pPrDefault><w:pPr/></w:pPrDefault>
					// + any pPr-level field -> <w:pPr/><w:ind/> 平级" — the
					// identical bug one container up, for rPr). This is the
					// SAME fix format_direct.go's planRunRPrPatches already
					// applies to a run's own self-closing <w:rPr/>.
					patches = append(patches, PatchRawSpan(styles, rpr.tagSpan, "<w:rPr>"+leaves+"</w:rPr>"))
				} else {
					needRPrSynthesis = true
					rprInner = leaves
				}
			}
		}

		if wantPPrChain {
			if dd.found && ppd.found && ppr.found && !ppr.selfClosing {
				// buildPPrOps (not a bare handful of leafOps) anchors the
				// insertion against pPrDefault's pPr's FULL set of tracked
				// children, so a newly inserted spacing/ind/jc lands in
				// CT_PPr's schema order even when that pPr also carries a
				// paragraph-mark <w:rPr> or other properties (Critical 1).
				// effLineSpacing/effLineSpacingExactPt/effSpaceBeforePt/
				// effSpaceAfterPt/effAlign/effFirstLineIndentChars mirror
				// effFont/effSize above: each is independently suppressed
				// to ""/0 when that ONE field (not the whole shared
				// <w:spacing>/<w:ind> tag) already renders what's
				// requested — attrEquals, the per-attribute analogue of
				// tagUnchanged, is what makes that possible even though
				// line/before/after all share ONE <w:spacing> element.
				effLineSpacing := opts.LineSpacing
				effLineSpacingExactPt := opts.LineSpacingExactPt
				effSpaceBeforePt := opts.SpaceBeforePt
				effSpaceAfterPt := opts.SpaceAfterPt
				if sp, ok := pprChildren["spacing"]; ok {
					if effLineSpacing != 0 && attrEquals(sp.attrs, "line", lineSpacingTo240ths(effLineSpacing)) && attrEquals(sp.attrs, "lineRule", "auto") {
						effLineSpacing = 0
					} else if effLineSpacing != 0 {
						spacingChanged = true
					}
					if effLineSpacingExactPt != 0 && attrEquals(sp.attrs, "line", ptToTwips(effLineSpacingExactPt)) && attrEquals(sp.attrs, "lineRule", "exact") {
						effLineSpacingExactPt = 0
					} else if effLineSpacingExactPt != 0 {
						spacingChanged = true
					}
					if effSpaceBeforePt != 0 && attrEquals(sp.attrs, "before", ptToTwips(effSpaceBeforePt)) {
						effSpaceBeforePt = 0
					} else if effSpaceBeforePt != 0 {
						spaceBeforeChanged = true
					}
					if effSpaceAfterPt != 0 && attrEquals(sp.attrs, "after", ptToTwips(effSpaceAfterPt)) {
						effSpaceAfterPt = 0
					} else if effSpaceAfterPt != 0 {
						spaceAfterChanged = true
					}
				} else {
					spacingChanged = spacingChanged || effLineSpacing != 0 || effLineSpacingExactPt != 0
					spaceBeforeChanged = spaceBeforeChanged || effSpaceBeforePt != 0
					spaceAfterChanged = spaceAfterChanged || effSpaceAfterPt != 0
				}
				effAlign := opts.Align
				if effAlign != "" {
					if jc, ok := pprChildren["jc"]; ok {
						if attrEquals(jc.attrs, "val", effAlign) {
							effAlign = ""
						} else {
							alignChanged = true
						}
					} else {
						alignChanged = true
					}
				}
				effFirstLineIndentChars := opts.FirstLineIndentChars
				if effFirstLineIndentChars != 0 {
					if ind, ok := pprChildren["ind"]; ok &&
						attrEquals(ind.attrs, "firstLineChars", firstLineCharsHundredths(effFirstLineIndentChars)) &&
						attrEquals(ind.attrs, "firstLine", firstLineTwipsFromChars(effFirstLineIndentChars)) {
						effFirstLineIndentChars = 0
					} else {
						firstLineIndentChanged = true
					}
				}
				notes = append(notes, pPrDropNotes(pprChildren, pParaRequest{
					SpaceBeforePt: effSpaceBeforePt, SpaceAfterPt: effSpaceAfterPt, FirstLineIndentChars: effFirstLineIndentChars,
				})...)
				patches = append(patches, applyLeafOps(styles, ppr.closeStart, buildPPrOps(pprChildren, pParaRequest{
					LineSpacing: effLineSpacing, LineSpacingExactPt: effLineSpacingExactPt,
					SpaceBeforePt: effSpaceBeforePt, SpaceAfterPt: effSpaceAfterPt,
					Align: effAlign, FirstLineIndentChars: effFirstLineIndentChars,
				}))...)
			} else {
				// Either pPr is missing entirely, or it is PRESENT but
				// self-closing (<w:pPr/>, no properties at all) -- both
				// need the exact same brand-new leaf content, since a
				// self-closing pPr has no children to edit in place either
				// (pprChildren is nil/empty in both shapes, per
				// scanDocDefaults). Nothing pre-existing to drop a
				// hanging/autospacing attribute FROM in either shape, so
				// pPrDropNotes is not consulted here (it would always
				// return nil against a nil children map).
				if opts.LineSpacing != 0 || opts.LineSpacingExactPt != 0 {
					spacingChanged = true
				}
				if opts.SpaceBeforePt != 0 {
					spaceBeforeChanged = true
				}
				if opts.SpaceAfterPt != 0 {
					spaceAfterChanged = true
				}
				if opts.Align != "" {
					alignChanged = true
				}
				if opts.FirstLineIndentChars != 0 {
					firstLineIndentChanged = true
				}
				leaves := renderActiveLeaves(buildPPrOps(nil, pParaRequest{
					LineSpacing: opts.LineSpacing, LineSpacingExactPt: opts.LineSpacingExactPt,
					SpaceBeforePt: opts.SpaceBeforePt, SpaceAfterPt: opts.SpaceAfterPt,
					Align: opts.Align, FirstLineIndentChars: opts.FirstLineIndentChars,
				}))
				if ppr.found && ppr.selfClosing {
					// Expand the self-closing tag IN PLACE so the new
					// leaves land as its own children, not as trailing
					// SIBLINGS of it (task 9 brief, item 6 / Task 8 review
					// round 2's old debt: "<w:pPrDefault><w:pPr/></w:pPrDefault>
					// + any pPr-level field -> <w:pPr/><w:ind/> 平级").
					patches = append(patches, PatchRawSpan(styles, ppr.tagSpan, "<w:pPr>"+leaves+"</w:pPr>"))
				} else {
					needPPrSynthesis = true
					pprInner = leaves
				}
			}
		}

		if needRPrSynthesis || needPPrSynthesis {
			patches = append(patches, synthesizeDocDefaultsPatches(
				styles, rootEnd, dd, rpd, ppd, needRPrSynthesis, rprInner, needPPrSynthesis, pprInner)...)
		}
	}

	// HeadingFont is computed BEFORE the style-chain call below so
	// touchedHeadingIDs is ready for planStyleChainShadowPatches' same-call
	// exemption (task 7 follow-up review, F3/F4) — patch ORDER in the
	// returned slice does not matter (Apply sorts by offset), only this
	// data dependency does. opts.BodyEastAsiaFont is threaded through so a
	// heading's own eastAsia font is fixed in the SAME pass whenever both
	// fields are requested together — see planHeadingFontPatches and
	// HeadingFont's own doc comment for why HeadingFont alone no longer
	// touches eastAsia at all (task 8 brief; task 7 复审遗留).
	var touchedHeadingIDs map[string]bool
	if opts.HeadingFont != "" {
		headingPatches, touched, err := planHeadingFontPatches(styles, opts.HeadingFont, opts.BodyEastAsiaFont)
		if err != nil {
			return nil, nil, nil, err
		}
		touchedHeadingIDs = touched
		if len(headingPatches) > 0 {
			patches = append(patches, headingPatches...)
			applied = append(applied, fmt.Sprintf("heading font -> %s", opts.HeadingFont))
		}
		if len(touched) == 0 {
			// Neither channel (styleId nor w:name) matched any style at
			// all — heading_font had nothing to act on. Previously this
			// fell through in total silence: no applied entry (correct)
			// but no note either, indistinguishable from "matched
			// everything, nothing needed to change" (task 9 brief, item 3
			// — format capability review, Important 7).
			notes = append(notes, "no heading styles found; nothing changed")
		}
	}

	// docDefaults is the cascade's WEAKEST layer: Normal and any style
	// basedOn it (BodyText, and every real Word/WPS document's own named
	// styles) can carry the very same rFonts/sz/spacing/jc/ind explicitly,
	// which then keeps outranking whatever docDefaults now says — the whole
	// point of this task (format capability review, Critical 4; write
	// review, I4/I5). planStyleChainShadowPatches finds and rewrites every
	// such shadowing leaf that is safe to touch, and reports (via notes,
	// not applied) whichever style it deliberately left alone.
	if wantRPrChain || wantPPrChain {
		chainPatches, chainChanged, chainNotes, err := planStyleChainShadowPatches(styles, opts, usedIDs, touchedHeadingIDs)
		if err != nil {
			return nil, nil, nil, err
		}
		patches = append(patches, chainPatches...)
		notes = append(notes, chainNotes...)
		fontChanged = fontChanged || chainChanged["body font"]
		sizeChanged = sizeChanged || chainChanged["body size"]
		eastAsiaChanged = eastAsiaChanged || chainChanged["east asia font"]
		spacingChanged = spacingChanged || chainChanged["line spacing"]
		spaceBeforeChanged = spaceBeforeChanged || chainChanged["space before"]
		spaceAfterChanged = spaceAfterChanged || chainChanged["space after"]
		alignChanged = alignChanged || chainChanged["alignment"]
		firstLineIndentChanged = firstLineIndentChanged || chainChanged["first line indent"]
	}

	if opts.BodyFont != "" && fontChanged {
		applied = append(applied, fmt.Sprintf("body font -> %s", opts.BodyFont))
	}
	if opts.BodyEastAsiaFont != "" && eastAsiaChanged {
		applied = append(applied, fmt.Sprintf("east asia font -> %s", opts.BodyEastAsiaFont))
	}
	if opts.BodySizePt != 0 && sizeChanged {
		applied = append(applied, fmt.Sprintf("body size -> %gpt", opts.BodySizePt))
	}
	if opts.LineSpacing != 0 && spacingChanged {
		applied = append(applied, fmt.Sprintf("line spacing -> %g", opts.LineSpacing))
	}
	if opts.LineSpacingExactPt != 0 && spacingChanged {
		applied = append(applied, fmt.Sprintf("line spacing -> exact %gpt", opts.LineSpacingExactPt))
	}
	if opts.SpaceBeforePt != 0 && spaceBeforeChanged {
		applied = append(applied, fmt.Sprintf("space before -> %gpt", opts.SpaceBeforePt))
	}
	if opts.SpaceAfterPt != 0 && spaceAfterChanged {
		applied = append(applied, fmt.Sprintf("space after -> %gpt", opts.SpaceAfterPt))
	}
	if opts.Align != "" && alignChanged {
		applied = append(applied, fmt.Sprintf("alignment -> %s", opts.Align))
	}
	if opts.FirstLineIndentChars != 0 && firstLineIndentChanged {
		applied = append(applied, fmt.Sprintf("first line indent -> %g chars", opts.FirstLineIndentChars))
	}

	return patches, applied, notes, nil
}

// planHeadingFontPatches rewrites every heading style's <w:rPr><w:rFonts>
// ascii/hAnsi to font (stripping their *Theme counterparts), and eastAsia to
// eastAsiaFont too — but ONLY when eastAsiaFont is non-"" (i.e.
// FormatOptions.BodyEastAsiaFont was ALSO given in this same Format call).
// HeadingFont on its own no longer touches eastAsia (or cs) at all: it used
// to, via rFontsLiteralAttrs, which silently turned a Chinese heading's own
// CJK font into whatever Latin heading_font was requested — the exact
// "heading_font 路径也不应再把 eastAsia 打成 Latin 字体" defect Task 7's own
// review left open for this task to close (task 8 brief; format capability
// review, Important 8's heading-path follow-up).
//
// A style counts as a heading through EITHER of two independent channels
// (task 9 brief, item 1 — format capability review, Important 5): its
// w:styleId matching headingStyleIDRe (Heading1..Heading9, the ordinary
// Word/docx_write convention), OR its own <w:name w:val="heading N"/>
// matching headingLikeNameRe case-insensitively, regardless of what its
// styleId happens to be. zh-CN Word and WPS commonly localize/renumber the
// styleId itself (typically down to a bare "1".."9") while still writing
// the SAME English w:name Word's own UI derives it from — styleId-only
// matching was a silent no-op on exactly those documents. Since <w:name>
// always precedes <w:pPr>/<w:rPr> in CT_Style's own child order, a style's
// heading-or-not status is always settled by the time either of those is
// seen, so both channels can be decided in one linear pass with no
// lookahead/buffering. touched is keyed by styleId regardless of which
// channel matched, so a name-only match still participates correctly in
// planStylesPatches' touchedHeadingIDs exemption and in its "no heading
// styles found" note (item 3, below).
//
// If a matched heading style has an <w:rPr> but no <w:rFonts> inside it,
// one is inserted as that rPr's first child; if the rPr itself is
// self-closing (<w:rPr/>, carrying no properties at all), it is expanded
// in place to hold the new rFonts; and if the style has NO <w:rPr> AT ALL —
// a heading whose font is entirely inherited via basedOn, format capability
// review Important 6 — one is synthesized and inserted at the
// schema-correct position (right after </w:pPr> when the style has a pPr —
// CT_Style requires rPr immediately follow pPr — otherwise right before
// </w:style>, computed inline via pPrFound/pPrCloseEnd below rather than a
// generic anchor-set scan: HeadingFont never needs to anchor against any
// OTHER CT_Style child, since none of name/aliases/basedOn/.../rsid ever
// gets a patch here), rather than left as a silent no-op (task 9 brief,
// item 2). A fully self-closing
// <w:style .../> (no children of any kind) is likewise expanded in place.
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
// Critical 2). pPrDepth/pPrFound/pPrCloseEnd give the SAME depth-tracking
// treatment to the style's own <w:pPr> (which can equally wrap a
// <w:pPrChange>'s historical <w:pPr>), needed to find that pPr's own true
// close for the "insert a brand new rPr right after it" anchor above.
//
// touched reports every styleId this function structurally matched
// (through either channel), REGARDLESS of whether a byte-different patch
// actually resulted — planStylesPatches uses it to exempt exactly these
// heading styles from planStyleChainShadowPatches' "body font" masking note
// when HeadingFont was also requested in this same call (task 7 follow-up
// review, F3/F4), and to report "no heading styles found; nothing changed"
// when it is empty (task 9 brief, item 3): a heading that legitimately got
// its own new font this call is not masking body_font in any sense worth a
// caveat about, even on a second identical call where the heading's font
// already matched and no patch was needed.
func planHeadingFontPatches(styles []byte, font, eastAsiaFont string) ([]Patch, map[string]bool, error) {
	dec := xml.NewDecoder(bytes.NewReader(styles))
	var prevOffset int
	var patches []Patch
	touched := map[string]bool{}

	var insideStyle, curIsHeading bool
	var curStyleID string
	var styleSpan Span
	var styleAttrs []xml.Attr
	var pPrDepth int
	var pPrFound bool
	var pPrCloseEnd int
	var rprSeen bool
	var trackingRPr bool
	var rprDepth int
	var rprTagSpan Span

	markHeading := func() {
		curIsHeading = true
		if curStyleID != "" {
			touched[curStyleID] = true
		}
	}
	newRFonts := func(existing []xml.Attr) string {
		return buildTag("rFonts", rFontsLatinAndEastAsiaAttrs(existing, font, eastAsiaFont), true)
	}

	for {
		tok, terr := dec.Token()
		if terr != nil {
			if errors.Is(terr, io.EOF) {
				break
			}
			return nil, nil, fmt.Errorf("scan styles.xml for heading fonts: %w", terr)
		}
		offset := int(dec.InputOffset())

		switch t := tok.(type) {
		case xml.StartElement:
			span := Span{prevOffset, offset}
			switch {
			case isWordElement(t.Name, "style") && !insideStyle:
				insideStyle = true
				styleSpan = span
				styleAttrs = t.Attr
				curStyleID, _ = wordAttrVal(t, "styleId")
				curIsHeading = false
				pPrDepth, pPrFound, pPrCloseEnd = 0, false, 0
				rprSeen, trackingRPr = false, false
				if curStyleID != "" && headingStyleIDRe.MatchString(curStyleID) {
					markHeading()
				}
				if isSelfClosingSpan(styles, span) {
					// No <w:name>/<w:pPr>/<w:rPr> at all to see: this
					// style's fate (heading via styleId, or not a heading
					// at all — a fully empty style has no <w:name> for the
					// name channel to ever match) is already final.
					if curIsHeading {
						patches = append(patches, PatchRawSpan(styles, styleSpan,
							buildTag("style", styleAttrs, false)+"<w:rPr>"+newRFonts(nil)+"</w:rPr></w:style>"))
					}
					insideStyle = false
				}
			case insideStyle && trackingRPr:
				// Anything encountered once the heading's own rPr is being
				// tracked (including a NESTED <w:rPr>, e.g. <w:rPrChange>'s
				// historical copy) only ever deepens rprDepth; it can never
				// re-trigger the "found rPr" case below.
				rprDepth++
			case insideStyle && !curIsHeading && isWordElement(t.Name, "name") && pPrDepth == 0:
				if v, ok := wordAttrVal(t, "val"); ok && headingLikeNameRe.MatchString(strings.TrimSpace(v)) {
					markHeading()
				}
			case insideStyle && isWordElement(t.Name, "pPr"):
				if pPrDepth == 0 {
					pPrFound = true
				}
				pPrDepth++
			case insideStyle && isWordElement(t.Name, "rPr") && pPrDepth == 0 && !rprSeen:
				rprSeen = true
				if isSelfClosingSpan(styles, span) {
					// No properties at all: expand in place so the new
					// rFonts has somewhere to live.
					if curIsHeading {
						patches = append(patches, PatchRawSpan(styles, span, "<w:rPr>"+newRFonts(nil)+"</w:rPr>"))
					}
				} else {
					trackingRPr = true
					rprDepth = 0
					rprTagSpan = span
				}
			}

		case xml.EndElement:
			switch {
			case insideStyle && trackingRPr:
				if rprDepth > 0 {
					rprDepth--
					break
				}
				// The style's rPr's TRUE close (depth-matched to its own
				// open, not merely "the next thing literally named rPr").
				// prevOffset is exactly where this </w:rPr> begins.
				trackingRPr = false
				if curIsHeading {
					children := scanDirectChildren(styles[rprTagSpan.End:prevOffset], rprTagSpan.End, []string{"rFonts"})
					if rf, ok := children["rFonts"]; ok {
						// tagUnchanged: a heading whose rFonts already
						// renders byte-for-byte identically to font has
						// nothing left to rewrite — skipping the patch here
						// (rather than always emitting one) is what makes a
						// second heading_font call with the same font
						// produce zero patches, so planStylesPatches'
						// len(headingPatches)>0 gate correctly reports
						// nothing changed instead of unconditionally
						// re-writing (and re-saving/backing-up) a
						// byte-identical styles.xml (task 7 follow-up
						// review round 2, F1).
						newTag := newRFonts(rf.attrs)
						if !tagUnchanged(styles, rf, newTag) {
							patches = append(patches, PatchRawSpan(styles, rf.tagSpan, newTag))
						}
					} else {
						// No rFonts among this rPr's own direct children:
						// insert one as its FIRST child, at rprTagSpan.End —
						// not at this close (LAST), which would land the new
						// rFonts after whatever direct formatting (e.g.
						// <w:b/>) this rPr already carries, violating
						// EG_RPrBase's "rFonts precedes every other run
						// property" order.
						patches = append(patches, PatchRawSpan(styles, Span{rprTagSpan.End, rprTagSpan.End}, newRFonts(nil)))
					}
				}
			case insideStyle && isWordElement(t.Name, "pPr") && pPrDepth > 0:
				pPrDepth--
				if pPrDepth == 0 {
					pPrCloseEnd = offset
				}
			case insideStyle && isWordElement(t.Name, "style"):
				if curIsHeading && !rprSeen {
					// No <w:rPr> anywhere in this heading style at all
					// (format capability review, Important 6 / task 9
					// brief, item 2): synthesize one at the schema-correct
					// position — right after </w:pPr> when this style has
					// one (CT_Style requires rPr immediately follow pPr),
					// otherwise right before </w:style> (rPr is then the
					// first thing CT_Style allows after name/aliases/
					// basedOn/.../rsid, none of which this package ever
					// needs to anchor against since it never inserts
					// anything before them).
					anchor := prevOffset
					if pPrFound {
						anchor = pPrCloseEnd
					}
					patches = append(patches, PatchRawSpan(styles, Span{anchor, anchor}, "<w:rPr>"+newRFonts(nil)+"</w:rPr>"))
				}
				insideStyle = false
			}
		}
		prevOffset = offset
	}
	return patches, touched, nil
}

// styleElem is one <w:style> block's classification-relevant facts, plus,
// for its own DIRECT rPr/pPr (never a nested rPrChange/pPrChange's
// historical copy — see scanDocDefaults' doc comment for why that
// distinction matters), whichever of rFonts/sz/szCs/spacing/jc it already
// declares explicitly. It intentionally carries no insertion-anchor data
// the way scanDocDefaults' rpr/ppr do (no closeStart, no "missing but
// wanted" tracking): planStyleChainShadowPatches below only ever REWRITES a
// leaf that already exists — a style with no explicit rFonts/sz/spacing/jc
// of its own has nothing shadowing docDefaults to fix, since it already
// inherits whatever docDefaults now says — so there is never a need to
// synthesize a brand new element inside some OTHER style, unlike
// docDefaults' own chain (which docx_write-shaped minimal generators can
// omit entirely and which planStylesPatches does synthesize).
type styleElem struct {
	id, typ, basedOn, name string
	rprChildren            map[string]elemInfo // rFonts/sz/szCs (and other rPrChildOrder anchors, unused here)
	pprChildren            map[string]elemInfo // spacing/ind/jc (and other pPrChildOrder anchors, unused here)
}

// styleChildNames is the direct children of a <w:style> element this
// package ever needs to read: basedOn (for the cascade graph), name (for
// classification and for a human-readable label in notes), and pPr/rPr
// (each rescanned one level deeper for the actual leaves below).
var styleChildNames = []string{"basedOn", "name", "pPr", "rPr"}

// scanAllStyles decodes styles — a whole word/styles.xml document — into one
// styleElem per top-level <w:style> element, in document order. It reuses
// scanDirectChildren (Task 1's depth-tracked mechanism) rather than a new
// hand-rolled boolean scan: a <w:style>'s own pPr can carry a paragraph-mark
// <w:rPr> nested inside it, and a table-type style's <w:tblStylePr> carries
// its OWN nested pPr/rPr/rFonts — exactly the "same-named element nested one
// level down" trap scanDocDefaults and planHeadingFontPatches already had to
// fix (format capability review, Critical 2 and its follow-ups), so this
// scan inherits their fix instead of reintroducing the bug a third time.
//
// A <w:style> element never nests inside another <w:style> (CT_Styles is a
// flat list), so finding each one's own close is a plain "the next
// </w:style> after this open" — no depth counter needed for THAT part,
// unlike pPr/rPr.
func scanAllStyles(styles []byte) ([]styleElem, error) {
	dec := xml.NewDecoder(bytes.NewReader(styles))
	var prevOffset int
	var result []styleElem
	var inStyle bool
	var openEnd int
	var openAttrs []xml.Attr
	var buildErr error

	build := func(innerStart, innerEnd int) styleElem {
		var e styleElem
		if v, ok := wordAttrVal(xml.StartElement{Attr: openAttrs}, "styleId"); ok {
			e.id = v
		}
		if v, ok := wordAttrVal(xml.StartElement{Attr: openAttrs}, "type"); ok {
			e.typ = v
		}
		if innerEnd <= innerStart {
			return e
		}
		// scanDirectChildren finds each depth-0 child's OPEN tag span
		// correctly (including skipping past a table-type style's
		// <w:tblStylePr>, which carries its own nested pPr/rPr one level
		// down) but never computes a container's own closeStart — that is
		// scanDocDefaults/scanRunProps/scanParaProps's own job, via their
		// depth-tracked scans, and styleLeafClose below does the same thing
		// for a style's pPr/rPr specifically.
		direct := scanDirectChildren(styles[innerStart:innerEnd], innerStart, styleChildNames)
		if bo, ok := direct["basedOn"]; ok {
			if v, ok2 := wordAttrVal(xml.StartElement{Attr: bo.attrs}, "val"); ok2 {
				e.basedOn = v
			}
		}
		if nm, ok := direct["name"]; ok {
			if v, ok2 := wordAttrVal(xml.StartElement{Attr: nm.attrs}, "val"); ok2 {
				e.name = v
			}
		}
		if ppr, ok := direct["pPr"]; ok && !ppr.selfClosing {
			closeStart, err := styleLeafClose(styles, ppr, "pPr")
			if err != nil {
				buildErr = err
				return e
			}
			e.pprChildren = scanDirectChildren(styles[ppr.tagSpan.End:closeStart], ppr.tagSpan.End, pPrChildOrder)
		}
		if rpr, ok := direct["rPr"]; ok && !rpr.selfClosing {
			closeStart, err := styleLeafClose(styles, rpr, "rPr")
			if err != nil {
				buildErr = err
				return e
			}
			e.rprChildren = scanDirectChildren(styles[rpr.tagSpan.End:closeStart], rpr.tagSpan.End, rPrChildOrder)
		}
		return e
	}

	for {
		tok, terr := dec.Token()
		if terr != nil {
			if errors.Is(terr, io.EOF) {
				break
			}
			return nil, fmt.Errorf("scan styles.xml style elements: %w", terr)
		}
		offset := int(dec.InputOffset())

		switch t := tok.(type) {
		case xml.StartElement:
			span := Span{prevOffset, offset}
			if !inStyle && isWordElement(t.Name, "style") {
				inStyle = true
				openEnd = offset
				openAttrs = t.Attr
				if isSelfClosingSpan(styles, span) {
					result = append(result, build(openEnd, openEnd))
					if buildErr != nil {
						return nil, buildErr
					}
					inStyle = false
				}
			}
		case xml.EndElement:
			if inStyle && isWordElement(t.Name, "style") {
				result = append(result, build(openEnd, prevOffset))
				if buildErr != nil {
					return nil, buildErr
				}
				inStyle = false
			}
		}
		prevOffset = offset
	}
	return result, nil
}

// styleLeafClose finds elem's own matching closing tag inside styles,
// tolerating a NESTED element of the same tagName one level down — a
// <w:rPr> can wrap a <w:rPrChange>'s historical <w:rPr>, and (symmetrically)
// a <w:pPr> can wrap a <w:pPrChange>'s historical <w:pPr> — the same
// depth-tracking fix already applied to scanDocDefaults/scanRunProps/
// scanParaProps for exactly this class of bug (format capability review,
// Critical 2 and its follow-ups). elem must be a found, non-self-closing
// elemInfo (callers only ever call this after checking both).
//
// It decodes forward starting at elem's own OPEN tag (not just after it) so
// the decoder's own element stack is primed with that open — decoding from
// tagSpan.End alone would hand encoding/xml.Decoder a stream whose closing
// </w:pPr>/</w:rPr> has no matching open it ever saw, which it rejects
// outright as "unexpected end element" — and stops at the first depth-0
// close, so in practice it only ever reads a small distance past elem's own
// content, not the rest of styles.xml, even though nothing bounds the byte
// slice it is handed on the far end.
func styleLeafClose(styles []byte, elem elemInfo, tagName string) (int, error) {
	base := elem.tagSpan.Start
	dec := xml.NewDecoder(bytes.NewReader(styles[base:]))
	var prevOffset, depth int
	seenOpen := false
	for {
		tok, terr := dec.Token()
		if terr != nil {
			return 0, fmt.Errorf("scan styles.xml for %s close: %w", tagName, terr)
		}
		offset := int(dec.InputOffset())
		switch t := tok.(type) {
		case xml.StartElement:
			if isWordElement(t.Name, tagName) {
				depth++
				seenOpen = true
			}
		case xml.EndElement:
			if isWordElement(t.Name, tagName) {
				depth--
				if seenOpen && depth == 0 {
					return base + prevOffset, nil
				}
			}
		}
		prevOffset = offset
	}
}

// headingLikeNameRe matches a <w:name> value of "heading 1".."heading 9"
// (any whitespace, case-insensitive) — the actual value real Word/WPS
// documents write regardless of localized UI (format capability review,
// Important 5's own observation that a localized STYLEID like "1"/"2" still
// carries this English w:name). Used for the style-CHAIN classification
// (isHeadingLikeStyle) AND, since task 9, as planHeadingFontPatches' own
// second matching channel alongside headingStyleIDRe — a zh-CN/WPS document
// whose Heading1..9 styleId is localized down to a bare "1".."9" still
// matches via this regex, so heading_font is no longer a silent no-op on
// those documents (task 9 brief, item 1).
var headingLikeNameRe = regexp.MustCompile(`(?i)^heading\s*[1-9]$`)

// normalizeStyleKey folds a style's own w:styleId or w:name into a
// comparison key — lowercased, with spaces removed — so "Source Code",
// "SourceCode", and "sourcecode" all compare equal without every caller
// having to enumerate every spacing/casing variant separately.
func normalizeStyleKey(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, " ", ""))
}

// quoteFamilyKeys/codeFamilyKeys/peripheralFamilyKeys are EXACT (post-
// normalizeStyleKey) name/styleId matches, deliberately NOT substring
// matches. An earlier version of this file matched any style whose name or
// styleId merely CONTAINED "quote"/"code" case-insensitively, which silently
// exempted a perfectly ordinary body-type style — "Unquoted", "Barcode",
// "Encoded" — from body_font/body_size_pt/line_spacing/align entirely, with
// no note either (task 7 follow-up review, F2). Each set below lists both
// this package's own styleIds (styles.go) and Word's own built-in style
// names for the same concept, normalized the same way their styleId/name
// pair would be.
var (
	quoteFamilyKeys = map[string]bool{
		"quote":        true, // this package's own Quote; Word's built-in "Quote"
		"intensequote": true, // Word's built-in "Intense Quote" / "IntenseQuote"
	}
	codeFamilyKeys = map[string]bool{
		"sourcecode":   true, // this package's own SourceCode / "Source Code"
		"verbatimchar": true, // this package's own VerbatimChar / "Verbatim Char"
	}
	// peripheralFamilyKeys names styles whose content is not "body text" in
	// the sense body_font/body_size_pt/line_spacing/align mean it, even
	// though nothing about their OWN basedOn chain would otherwise exclude
	// them: Header/Footer repeat on every page independently of the
	// document's running text, and Caption labels a figure/table rather
	// than being a paragraph of prose. A follow-up review caught the
	// original implementation silently rewriting these alongside Normal/
	// BodyText — asking for a new body_size should not also resize page
	// headers/footers/captions (task 7 follow-up review, "激进副作用纠正").
	// Unlike Quote/SourceCode, this family is still reported via masking
	// notes when it shadows a requested field (isNotedExclusionStyle).
	peripheralFamilyKeys = map[string]bool{
		"header":  true,
		"footer":  true,
		"caption": true,
	}
	// monospaceBuiltinKeys names WORD'S OWN built-in fixed-width-font
	// styles that are not this package's own SourceCode/VerbatimChar but
	// serve the same "keep this content in a monospace face" role: "Plain
	// Text" (Outlook/mail-quote import), "HTML Code"/"HTML Preformatted"
	// (pasted-from-web import), and "Block Text" (an older built-in,
	// sometimes surfaced as "Block Quote" in Word's UI, also monospaced in
	// Word's own default template). A follow-up review's PROBE-Q caught
	// these falling through the SAME gap F2 fixed for Quote/SourceCode —
	// this package's exact-match sets covered only ITS OWN monospace
	// styles, so a real Word document's own built-in ones were treated as
	// ordinary body-type styles and silently rewritten from Consolas/
	// Courier-class fonts to whatever proportional body_font was requested.
	// Unlike SourceCode/VerbatimChar (this package's own, silently excluded
	// per the task's original invariant), these ARE reported via masking
	// notes when they shadow a requested field and are actually used
	// (isNotedExclusionStyle) — a caller is less likely to already assume
	// "Plain Text" is off-limits for body_font the unambiguous way
	// "SourceCode" reads, so the honesty is worth the extra note.
	monospaceBuiltinKeys = map[string]bool{
		"plaintext":        true, // Word's built-in "Plain Text"
		"htmlcode":         true, // Word's built-in "HTML Code"
		"htmlpreformatted": true, // Word's built-in "HTML Preformatted"
		"blocktext":        true, // Word's built-in "Block Text" ("Block Quote" in some UIs)
	}
)

// isHeadingLikeStyle reports whether s is a heading-or-heading-adjacent
// display style: Heading1..9 (by styleId or by w:name), or Title/Subtitle
// (by either). Title/Subtitle are grouped with headings, not left as plain
// "custom" styles, because they play the exact same role in the format
// capability review's own evidence (Critical 4: "真实 python-docx 文档：
// Title/Subtitle 仍是 asciiTheme 主题字体") and in this task's brief (§2's
// worked example of a style that must be reported, not silently rewritten).
func isHeadingLikeStyle(s styleElem) bool {
	if headingStyleIDRe.MatchString(s.id) || headingLikeNameRe.MatchString(strings.TrimSpace(s.name)) {
		return true
	}
	return strings.EqualFold(s.id, "Title") || strings.EqualFold(s.name, "Title") ||
		strings.EqualFold(s.id, "Subtitle") || strings.EqualFold(s.name, "Subtitle")
}

// isCodeLikeName/isQuoteLikeName test a bare styleId or w:name string (not a
// full styleElem) against the exact-match sets above — used both by
// isCodeLikeStyle/isQuoteLikeStyle below (which check a style's OWN id/name)
// and by directFormatMaskingNotes (which only ever has a paragraph's pStyle
// STRING, via Para.Style, not a resolved styleElem).
func isCodeLikeName(name string) bool  { return codeFamilyKeys[normalizeStyleKey(name)] }
func isQuoteLikeName(name string) bool { return quoteFamilyKeys[normalizeStyleKey(name)] }

// isQuoteLikeStyle/isCodeLikeStyle classify the two families whose shadowing
// is NEVER mentioned, not even in notes (isSilentExclusionStyle) —
// SourceCode's monospace font and Quote's distinct look are deliberate, not
// an oversight, so nobody is surprised body_font left their code blocks and
// blockquotes alone.
func isQuoteLikeStyle(s styleElem) bool {
	return isQuoteLikeName(s.id) || isQuoteLikeName(s.name)
}

func isCodeLikeStyle(s styleElem) bool {
	return isCodeLikeName(s.id) || isCodeLikeName(s.name)
}

// isPeripheralStyle classifies the Header/Footer/Caption family: excluded
// from the rewrite (like Quote/SourceCode) but, unlike them, still reported
// via masking notes (like headings) — see isNotedExclusionStyle.
func isPeripheralStyle(s styleElem) bool {
	return peripheralFamilyKeys[normalizeStyleKey(s.id)] || peripheralFamilyKeys[normalizeStyleKey(s.name)]
}

// isMonospaceBuiltinStyle classifies Word's own Plain Text/HTML Code/HTML
// Preformatted/Block Text family — see monospaceBuiltinKeys' own doc
// comment for why these are excluded from the rewrite but, unlike this
// package's own SourceCode/VerbatimChar, DO surface via masking notes.
func isMonospaceBuiltinStyle(s styleElem) bool {
	return monospaceBuiltinKeys[normalizeStyleKey(s.id)] || monospaceBuiltinKeys[normalizeStyleKey(s.name)]
}

// isSilentExclusionStyle families are excluded from BOTH the rewrite AND
// masking notes: their shadowing is expected and not a caller-facing
// caveat.
func isSilentExclusionStyle(s styleElem) bool {
	return isQuoteLikeStyle(s) || isCodeLikeStyle(s)
}

// isNotedExclusionStyle families are excluded from the rewrite but DO get
// reported via masking notes when they shadow a requested field and are
// actually referenced by a paragraph (planStyleChainShadowPatches' usedIDs
// parameter, task 7 follow-up review F5).
func isNotedExclusionStyle(s styleElem) bool {
	return isHeadingLikeStyle(s) || isPeripheralStyle(s) || isMonospaceBuiltinStyle(s)
}

func isExcludedFamilyStyle(s styleElem) bool {
	return isSilentExclusionStyle(s) || isNotedExclusionStyle(s)
}

// reachesNormalCleanly walks the basedOn chain starting at id, returning
// true only if it reaches "Normal" WITHOUT passing through a style
// isExcludedFamilyStyle classifies as heading/quote/code-like along the way
// (id itself is checked by the caller, not here — this only inspects id and
// its ANCESTORS). A missing basedOn target, an empty basedOn (chain ends
// without reaching Normal), or a cycle (guarded by seen) all report false:
// none of those is "based on Normal", so there is nothing here to safely
// rewrite.
func reachesNormalCleanly(id string, byID map[string]styleElem) bool {
	seen := make(map[string]bool)
	for i := 0; i < 64; i++ {
		if id == "Normal" {
			return true
		}
		if id == "" || seen[id] {
			return false
		}
		seen[id] = true
		s, ok := byID[id]
		if !ok {
			return false
		}
		if isExcludedFamilyStyle(s) {
			return false
		}
		id = s.basedOn
	}
	return false
}

// eligibleForBodyChainRewrite reports whether s is safe for
// planStyleChainShadowPatches to REWRITE (as opposed to merely note): Normal
// itself always is (it IS the cascade's root, not merely reachable from it),
// and any other paragraph-type style is, provided it is not itself
// heading/quote/code-like and its basedOn chain reaches Normal without
// passing through one of those families either. This covers docx_write's
// own BodyText, a real Word template's Body Text/List Continue/Caption/etc.
// styles, and any similarly-named custom style — exactly "非
// Heading/SourceCode/Quote 家族的正文类样式" (task 7 brief, §1).
func eligibleForBodyChainRewrite(s styleElem, byID map[string]styleElem) bool {
	if s.id == "Normal" {
		return true
	}
	if s.typ != "paragraph" {
		return false
	}
	if isExcludedFamilyStyle(s) {
		return false
	}
	return reachesNormalCleanly(s.basedOn, byID)
}

// styleLabel returns s's human-readable w:name, falling back to its
// styleId when the style has no <w:name> at all (schema-legal, if unusual)
// — used only for the notes planStyleChainShadowPatches produces.
func styleLabel(s styleElem) string {
	if s.name != "" {
		return s.name
	}
	return s.id
}

// hasAttr reports whether attrs contains an attribute named local,
// regardless of value — used by planStyleChainShadowPatches to tell "this
// style's own <w:spacing> sets w:line" (a real line-spacing override) apart
// from "this style's own <w:spacing> only sets w:after/w:before" (paragraph
// spacing, unrelated to line spacing, common on real Word template styles —
// see docDefaultsXML and every heading/body style in this package's own
// styles.go).
func hasAttr(attrs []xml.Attr, local string) bool {
	_, ok := wordAttrVal(xml.StartElement{Attr: attrs}, local)
	return ok
}

// tagUnchanged reports whether elem — a found, self-closing leaf like
// <w:rFonts>/<w:sz>/<w:spacing>/<w:jc> — already renders byte-for-byte
// identically to newTagXML, the tag planStyleChainShadowPatches/
// planStylesPatches is about to write. It is the idempotency check behind
// "applied only reports a real, byte-different write": without it, a second
// Format call with the exact same options would still rewrite every leaf to
// the value it already has and unconditionally report success (task 7
// follow-up review, F1). A not-found elem is never "unchanged" — there is
// nothing on disk to compare against, so the caller's insert-vs-rewrite
// branch decides what happens, not this function.
func tagUnchanged(doc []byte, elem elemInfo, newTagXML string) bool {
	if !elem.found {
		return false
	}
	return string(doc[elem.tagSpan.Start:elem.tagSpan.End]) == newTagXML
}

// usedStyleIDs collects every <w:pStyle w:val="..."/> value at least one
// paragraph in paras directly references. planStyleChainShadowPatches uses
// it to keep a masking note from firing on a style nothing in the document
// actually uses — a document with no headings should not be told "Heading1
// still has its own font" just because styles.xml happens to define one,
// unused, the way every Word template does (task 7 follow-up review, F5).
// Only DIRECT references count, not a transitive basedOn chain: that is
// enough to silence the "no headings at all" noise this review flagged,
// without needing a second graph walk over the style chain itself.
func usedStyleIDs(paras []Para) map[string]bool {
	used := make(map[string]bool)
	for _, p := range paras {
		if p.Style != "" {
			used[p.Style] = true
		}
	}
	return used
}

// planStyleChainShadowPatches walks styles' full style graph and handles
// every paragraph-type style other than the silently-excluded Quote/
// SourceCode families (isSilentExclusionStyle) one of two ways:
//
//   - eligibleForBodyChainRewrite (Normal itself, or basedOn it without
//     passing through a heading/peripheral/quote/code style): REWRITE
//     whichever of its own EXPLICIT rFonts/sz/szCs/spacing[line]/jc already
//     shadows the docDefaults value planStylesPatches's docDefaults section
//     just wrote, skipping any leaf that would already render byte-for-byte
//     identically (tagUnchanged) so a repeat call is a true no-op. This
//     never inserts a new element into another style — nothing to fix when
//     no explicit property exists — so only an EXISTING leaf is ever
//     rewritten in place, with no anchoring/ordering concerns (nothing is
//     being inserted, so CT_PPr/EG_RPrBase's child order can never be
//     violated here).
//   - everything else (Heading1..9/Title/Subtitle, Header/Footer/Caption,
//     AND any style whose basedOn chain does not cleanly reach Normal at
//     all — a root-level custom style, or one like Word's own NoSpacing/
//     MacroText): never rewritten. If it carries the shadowing property
//     explicitly AND is referenced by at least one real paragraph
//     (usedIDs, F5) AND was not already handled some other way this same
//     call (touchedHeadingIDs — see below), it is named in notes instead —
//     task 7 follow-up review F3/F4's unified rule: "shadows AND wasn't
//     rewritten this call" is the one test for whether to name a style,
//     replacing the previous version's inconsistent "only heading-like
//     styles reachable from Normal" carve-out, which silently said nothing
//     at all about a root-level style or an unreachable built-in like
//     NoSpacing/MacroText even when both cases are exactly the same
//     "docDefaults doesn't actually win here" situation a caller needs to
//     know about.
//
// touchedHeadingIDs (populated by planHeadingFontPatches when opts.
// HeadingFont is also set in this SAME call) exempts exactly those
// Heading1..9 styles from the "body font" note only: a heading that
// heading_font just gave its own deliberate font is not "masking" body_font
// in any sense the caller needs a caveat about — the previous version
// contradicted itself by rewriting a heading's font via HeadingFont and
// then, in the same result, complaining that the very same style still
// "shadows" body_font.
func planStyleChainShadowPatches(styles []byte, opts FormatOptions, usedIDs, touchedHeadingIDs map[string]bool) ([]Patch, map[string]bool, []string, error) {
	elems, err := scanAllStyles(styles)
	if err != nil {
		return nil, nil, nil, err
	}
	byID := make(map[string]styleElem, len(elems))
	for _, s := range elems {
		if s.id != "" {
			byID[s.id] = s
		}
	}
	if _, ok := byID["Normal"]; !ok {
		// No conventional Normal style to anchor the cascade against.
		// docDefaults alone (planStylesPatches' existing, unconditional
		// behavior) still applies; guessing at a different root here would
		// risk rewriting the wrong styles entirely.
		return nil, nil, nil, nil
	}

	var patches []Patch
	changed := map[string]bool{}    // field label -> a real (byte-different) rewrite happened
	masked := map[string][]string{} // field label -> style names that still shadow it
	// dropNotes collects pPrDropNotes' silent-drop caveats (task 9 brief,
	// item 7b) as each eligible style's own spacing/ind is rewritten below —
	// the SAME two "one wins, the other becomes invisible" pairs
	// buildPPrOps' docDefaults path already surfaces via pPrDropNotes, just
	// duplicated inline here (rather than calling pPrDropNotes itself)
	// because this loop mutates attrs incrementally per-field instead of
	// building one pParaRequest up front.
	var dropNotes []string

	for _, s := range elems {
		if s.typ != "paragraph" || s.id == "" {
			continue
		}
		if isSilentExclusionStyle(s) {
			continue
		}

		if eligibleForBodyChainRewrite(s, byID) {
			if opts.BodyFont != "" || opts.BodyEastAsiaFont != "" {
				if rf, ok := s.rprChildren["rFonts"]; ok {
					fontChanges := opts.BodyFont != "" && !rFontsLatinUnchanged(rf.attrs, opts.BodyFont)
					eastAsiaChanges := opts.BodyEastAsiaFont != "" && !rFontsEastAsiaUnchanged(rf.attrs, opts.BodyEastAsiaFont)
					if fontChanges || eastAsiaChanges {
						newTag := buildTag("rFonts", rFontsLatinAndEastAsiaAttrs(rf.attrs, opts.BodyFont, opts.BodyEastAsiaFont), true)
						patches = append(patches, PatchRawSpan(styles, rf.tagSpan, newTag))
						if fontChanges {
							changed["body font"] = true
						}
						if eastAsiaChanges {
							changed["east asia font"] = true
						}
					}
				}
			}
			if opts.BodySizePt != 0 {
				half := ptToHalfPoints(opts.BodySizePt)
				if sz, ok := s.rprChildren["sz"]; ok {
					newTag := buildTag("sz", setAttr(sz.attrs, "val", half), true)
					if !tagUnchanged(styles, sz, newTag) {
						patches = append(patches, PatchRawSpan(styles, sz.tagSpan, newTag))
						changed["body size"] = true
					}
				}
				if szcs, ok := s.rprChildren["szCs"]; ok {
					newTag := buildTag("szCs", setAttr(szcs.attrs, "val", half), true)
					if !tagUnchanged(styles, szcs, newTag) {
						patches = append(patches, PatchRawSpan(styles, szcs.tagSpan, newTag))
						changed["body size"] = true
					}
				}
			}
			if opts.LineSpacing != 0 || opts.LineSpacingExactPt != 0 || opts.SpaceBeforePt != 0 || opts.SpaceAfterPt != 0 {
				if sp, ok := s.pprChildren["spacing"]; ok {
					// lineChanges/beforeChanges/afterChanges are gated on
					// attrEquals (per-attribute), NOT on a single whole-tag
					// tagUnchanged check at the end: review F1 caught the
					// earlier version marking ALL THREE true together
					// whenever ANY one of them made the combined tag
					// byte-different, misreporting e.g. "line spacing" as
					// applied when only "space before" actually moved and
					// line already matched byte for byte.
					attrs := sp.attrs
					var lineChanges, beforeChanges, afterChanges bool
					if opts.LineSpacing != 0 && hasAttr(sp.attrs, "line") {
						want := lineSpacingTo240ths(opts.LineSpacing)
						if !attrEquals(sp.attrs, "line", want) || !attrEquals(sp.attrs, "lineRule", "auto") {
							attrs = setAttr(setAttr(attrs, "line", want), "lineRule", "auto")
							lineChanges = true
						}
					} else if opts.LineSpacingExactPt != 0 && hasAttr(sp.attrs, "line") {
						want := ptToTwips(opts.LineSpacingExactPt)
						if !attrEquals(sp.attrs, "line", want) || !attrEquals(sp.attrs, "lineRule", "exact") {
							attrs = setAttr(setAttr(attrs, "line", want), "lineRule", "exact")
							lineChanges = true
						}
					}
					if opts.SpaceBeforePt != 0 && hasAttr(sp.attrs, "before") {
						want := ptToTwips(opts.SpaceBeforePt)
						// review F2: an explicit w:before is meaningless
						// (and ignored by Word) while w:beforeAutospacing
						// is set, the same "one wins, the other is
						// silently ignored" relationship w:hanging has
						// with w:firstLine below -- clear it whenever we
						// write a real w:before value.
						if !attrEquals(sp.attrs, "before", want) || hasAttr(sp.attrs, "beforeAutospacing") {
							if hasAttr(sp.attrs, "beforeAutospacing") {
								dropNotes = append(dropNotes, fmt.Sprintf(
									"space_before_pt removed style %q's own w:beforeAutospacing flag, which would otherwise have made the new value invisible", styleLabel(s)))
							}
							attrs = dropAttrs(attrs, "beforeAutospacing")
							attrs = setAttr(attrs, "before", want)
							beforeChanges = true
						}
					}
					if opts.SpaceAfterPt != 0 && hasAttr(sp.attrs, "after") {
						want := ptToTwips(opts.SpaceAfterPt)
						if !attrEquals(sp.attrs, "after", want) || hasAttr(sp.attrs, "afterAutospacing") {
							if hasAttr(sp.attrs, "afterAutospacing") {
								dropNotes = append(dropNotes, fmt.Sprintf(
									"space_after_pt removed style %q's own w:afterAutospacing flag, which would otherwise have made the new value invisible", styleLabel(s)))
							}
							attrs = dropAttrs(attrs, "afterAutospacing")
							attrs = setAttr(attrs, "after", want)
							afterChanges = true
						}
					}
					if lineChanges || beforeChanges || afterChanges {
						newTag := buildTag("spacing", attrs, true)
						patches = append(patches, PatchRawSpan(styles, sp.tagSpan, newTag))
						if lineChanges {
							changed["line spacing"] = true
						}
						if beforeChanges {
							changed["space before"] = true
						}
						if afterChanges {
							changed["space after"] = true
						}
					}
				}
			}
			if opts.Align != "" {
				if jc, ok := s.pprChildren["jc"]; ok {
					newTag := buildTag("jc", setAttr(jc.attrs, "val", opts.Align), true)
					if !tagUnchanged(styles, jc, newTag) {
						patches = append(patches, PatchRawSpan(styles, jc.tagSpan, newTag))
						changed["alignment"] = true
					}
				}
			}
			if opts.FirstLineIndentChars != 0 {
				if ind, ok := s.pprChildren["ind"]; ok && (hasAttr(ind.attrs, "firstLine") || hasAttr(ind.attrs, "firstLineChars")) {
					// review F2: w:hanging/w:hangingChars must be dropped
					// whenever we write a real w:firstLine/firstLineChars
					// -- CT_Ind's hanging indent always wins over
					// firstLine when both are present, so leaving a
					// pre-existing hanging in place would make our new
					// firstLine silently invisible.
					newAttrs := dropAttrs(ind.attrs, "hanging", "hangingChars")
					newAttrs = setAttr(setAttr(newAttrs,
						"firstLineChars", firstLineCharsHundredths(opts.FirstLineIndentChars)),
						"firstLine", firstLineTwipsFromChars(opts.FirstLineIndentChars))
					newTag := buildTag("ind", newAttrs, true)
					if !tagUnchanged(styles, ind, newTag) {
						if hasAttr(ind.attrs, "hanging") || hasAttr(ind.attrs, "hangingChars") {
							dropNotes = append(dropNotes, fmt.Sprintf(
								"first_line_indent_chars removed style %q's own hanging indent (w:hanging/w:hangingChars), which would otherwise have taken precedence and made it invisible", styleLabel(s)))
						}
						patches = append(patches, PatchRawSpan(styles, ind.tagSpan, newTag))
						changed["first line indent"] = true
					}
				}
			}
			continue
		}

		// Not eligible for rewrite: report it, by name, only when it
		// actually shadows a requested field AND a real paragraph uses it
		// (F5) — the exact-match family/PROBE fixtures this rule was
		// written against are format_style_chain_test.go's own tests.
		if !usedIDs[s.id] {
			continue
		}
		label := styleLabel(s)
		exemptFont := isHeadingLikeStyle(s) && opts.HeadingFont != "" && touchedHeadingIDs[s.id]
		if opts.BodyFont != "" && !exemptFont {
			if _, ok := s.rprChildren["rFonts"]; ok {
				masked["body font"] = append(masked["body font"], label)
			}
		}
		if opts.BodyEastAsiaFont != "" && !exemptFont {
			if _, ok := s.rprChildren["rFonts"]; ok {
				masked["east asia font"] = append(masked["east asia font"], label)
			}
		}
		if opts.BodySizePt != 0 {
			_, hasSz := s.rprChildren["sz"]
			_, hasSzCs := s.rprChildren["szCs"]
			if hasSz || hasSzCs {
				masked["body size"] = append(masked["body size"], label)
			}
		}
		if opts.LineSpacing != 0 || opts.LineSpacingExactPt != 0 {
			if sp, ok := s.pprChildren["spacing"]; ok && hasAttr(sp.attrs, "line") {
				masked["line spacing"] = append(masked["line spacing"], label)
			}
		}
		if opts.SpaceBeforePt != 0 {
			if sp, ok := s.pprChildren["spacing"]; ok && hasAttr(sp.attrs, "before") {
				masked["space before"] = append(masked["space before"], label)
			}
		}
		if opts.SpaceAfterPt != 0 {
			if sp, ok := s.pprChildren["spacing"]; ok && hasAttr(sp.attrs, "after") {
				masked["space after"] = append(masked["space after"], label)
			}
		}
		if opts.Align != "" {
			if _, ok := s.pprChildren["jc"]; ok {
				masked["alignment"] = append(masked["alignment"], label)
			}
		}
		if opts.FirstLineIndentChars != 0 {
			if ind, ok := s.pprChildren["ind"]; ok && (hasAttr(ind.attrs, "firstLine") || hasAttr(ind.attrs, "firstLineChars")) {
				masked["first line indent"] = append(masked["first line indent"], label)
			}
		}
	}

	var notes []string
	for _, field := range []string{
		"body font", "east asia font", "body size", "line spacing",
		"space before", "space after", "alignment", "first line indent",
	} {
		names := masked[field]
		if len(names) == 0 {
			continue
		}
		quoted := make([]string, len(names))
		for i, n := range names {
			quoted[i] = fmt.Sprintf("%q", n)
		}
		notes = append(notes, fmt.Sprintf(
			"style(s) %s carry their own %s that overrides this rule and were left unchanged",
			strings.Join(quoted, ", "), field))
	}
	notes = append(notes, dropNotes...)
	return patches, changed, notes, nil
}

// directFormatMaskingNotes scans doc's paragraphs for DIRECT formatting (a
// paragraph's own <w:pPr>, or one of its runs' own <w:rPr>) that would keep
// outranking whatever the docDefaults/style-chain rewrite above just wrote —
// Word's cascade puts direct formatting above every style, including a
// freshly-rewritten Normal/BodyText. It reports one honest note per
// requested field that is actually shadowed this way, naming the exact
// paragraphs affected, rather than letting the whole-document path's
// Applied read as "every paragraph now looks like this" the way it used to
// (format capability review, Critical 4 / this task's §2).
//
// A paragraph styled SourceCode is skipped entirely (isCodeLikeName(p.
// Style)), not merely excluded from whatever range gets suggested: a code
// block's own run carries a DIRECT <w:rFonts>/<w:sz> copy of SourceCode's
// monospace font on purpose (styles.go's codeRunFontsXML — this package's
// own GenOffice-compatibility mechanism, copied onto the run so a renderer
// that ignores paragraph styles still shows the right font), which is not a
// caller mistake to "fix" by restyling it toward the new body font — quite
// the opposite, that direct copy is exactly what keeps the code block
// looking like code (task 7 follow-up review, F6).
//
// Exact paragraph numbers are listed (not a start_para/end_para range) for
// the same reason: a min/max range spanning two flagged paragraphs can
// silently sweep in an untouched one sitting between them — e.g. a
// SourceCode paragraph the check above deliberately skipped — and
// formatDirectRange's start_para/end_para only ever accepts one contiguous
// range, so a caller acting on a range-shaped suggestion would restyle that
// untouched paragraph too. An exact list has no such gap (task 7 follow-up
// review, F6).
func directFormatMaskingNotes(doc []byte, paras []Para, opts FormatOptions) ([]string, error) {
	wantFont := opts.BodyFont != ""
	wantEastAsia := opts.BodyEastAsiaFont != ""
	wantSize := opts.BodySizePt != 0
	wantSpacing := opts.LineSpacing != 0 || opts.LineSpacingExactPt != 0
	wantSpaceBefore := opts.SpaceBeforePt != 0
	wantSpaceAfter := opts.SpaceAfterPt != 0
	wantAlign := opts.Align != ""
	wantFirstLineIndent := opts.FirstLineIndentChars != 0
	if !wantFont && !wantEastAsia && !wantSize && !wantSpacing && !wantSpaceBefore &&
		!wantSpaceAfter && !wantAlign && !wantFirstLineIndent {
		return nil, nil
	}

	var fontHits, eastAsiaHits, sizeHits, spacingHits, spaceBeforeHits, spaceAfterHits []int
	var alignHits, firstLineIndentHits []int

	for _, p := range paras {
		if isCodeLikeName(p.Style) {
			continue
		}
		if wantSpacing || wantSpaceBefore || wantSpaceAfter || wantAlign || wantFirstLineIndent {
			_, ppr, children, err := scanParaProps(doc, p.Span)
			if err != nil {
				return nil, fmt.Errorf("paragraph %d: %w", p.Index, err)
			}
			if ppr.found && !ppr.selfClosing {
				if wantSpacing {
					if sp, ok := children["spacing"]; ok && hasAttr(sp.attrs, "line") {
						spacingHits = append(spacingHits, p.Index)
					}
				}
				if wantSpaceBefore {
					if sp, ok := children["spacing"]; ok && hasAttr(sp.attrs, "before") {
						spaceBeforeHits = append(spaceBeforeHits, p.Index)
					}
				}
				if wantSpaceAfter {
					if sp, ok := children["spacing"]; ok && hasAttr(sp.attrs, "after") {
						spaceAfterHits = append(spaceAfterHits, p.Index)
					}
				}
				if wantAlign {
					if _, ok := children["jc"]; ok {
						alignHits = append(alignHits, p.Index)
					}
				}
				if wantFirstLineIndent {
					if ind, ok := children["ind"]; ok && (hasAttr(ind.attrs, "firstLine") || hasAttr(ind.attrs, "firstLineChars")) {
						firstLineIndentHits = append(firstLineIndentHits, p.Index)
					}
				}
			}
		}
		if wantFont || wantEastAsia || wantSize {
			paraFont, paraEastAsia, paraSize := false, false, false
			for _, r := range p.Runs {
				_, rpr, children, err := scanRunProps(doc, r.Elem)
				if err != nil {
					return nil, fmt.Errorf("paragraph %d: %w", p.Index, err)
				}
				if !rpr.found || rpr.selfClosing {
					continue
				}
				rfonts, sz := children["rFonts"], children["sz"]
				if (wantFont || wantEastAsia) && rfonts.found {
					if wantFont {
						paraFont = true
					}
					if wantEastAsia {
						paraEastAsia = true
					}
				}
				if wantSize && sz.found {
					paraSize = true
				}
			}
			if paraFont {
				fontHits = append(fontHits, p.Index)
			}
			if paraEastAsia {
				eastAsiaHits = append(eastAsiaHits, p.Index)
			}
			if paraSize {
				sizeHits = append(sizeHits, p.Index)
			}
		}
	}

	var notes []string
	add := func(hits []int, label string) {
		if len(hits) == 0 {
			return
		}
		nums := make([]string, len(hits))
		for i, n := range hits {
			nums[i] = strconv.Itoa(n)
		}
		notes = append(notes, fmt.Sprintf(
			"%d paragraph(s) carry direct %s formatting that overrides this rule: paragraph(s) %s; use start_para/end_para on them to restyle",
			len(hits), label, strings.Join(nums, ", ")))
	}
	add(fontHits, "font")
	add(eastAsiaHits, "east asia font")
	add(sizeHits, "size")
	add(spacingHits, "line spacing")
	add(spaceBeforeHits, "space before")
	add(spaceAfterHits, "space after")
	add(alignHits, "alignment")
	add(firstLineIndentHits, "first-line indent")
	return notes, nil
}

// sectPrChildOrder is CT_SectPr's child sequence (ECMA-376 §17.6.17): the
// anchor set planMarginPatches needs so a brand-new <w:pgMar> inserted for
// a section that has none at all (task 9 brief, item 4 — format
// capability review, Important 10) lands in the schema-correct position —
// after any header/footerReference/type/pgSz, before paperSrc/pgBorders/
// and everything else — rather than failing the whole call the way a
// missing <w:pgMar> used to. headerReference/footerReference can repeat,
// but scanDirectChildren only ever needs to know whether AT LEAST ONE
// occurs before pgMar, so tracking just the first is sufficient as an
// anchor.
var sectPrChildOrder = []string{
	"headerReference", "footerReference", "footnotePr", "endnotePr", "type",
	"pgSz", "pgMar", "paperSrc", "pgBorders", "lnNumType", "pgNumType",
	"cols", "formProt", "vAlign", "noEndnote", "titlePg", "textDirection",
	"bidi", "rtlGutter", "docGrid", "printerSettings", "sectPrChange",
}

// scanSectPrs finds every <w:sectPr> in documentXML (ordinarily one, the
// document body's own trailing section properties, but a multi-section
// document can have one per section, each living inside the LAST
// paragraph's own <w:pPr> of its section) and, for each, its own elemInfo
// (found/tagSpan/selfClosing/closeStart) plus the full set of its direct
// children scanDirectChildren tracks against sectPrChildOrder — the same
// "full anchor set, not just the one leaf this package edits" shape
// scanDocDefaults/scanParaProps already return for pPr/rPr, needed so a
// newly inserted pgMar lands in schema order relative to whatever else the
// section already carries. <w:sectPr> never nests (CT_PPrBase, which a
// pPrChange's historical pPr copy is typed as, does not include sectPr —
// unlike pPr/rPr, there is no same-named-nested-element trap here), so a
// plain "in/out" boolean is enough to find each one's own close.
func scanSectPrs(documentXML []byte) ([]elemInfo, []map[string]elemInfo, error) {
	dec := xml.NewDecoder(bytes.NewReader(documentXML))
	var prevOffset int
	var infos []elemInfo
	var childrenList []map[string]elemInfo
	var in bool
	var cur elemInfo

	for {
		tok, terr := dec.Token()
		if terr != nil {
			if errors.Is(terr, io.EOF) {
				break
			}
			return nil, nil, fmt.Errorf("scan document.xml for sectPr: %w", terr)
		}
		offset := int(dec.InputOffset())

		switch t := tok.(type) {
		case xml.StartElement:
			span := Span{prevOffset, offset}
			if !in && isWordElement(t.Name, "sectPr") {
				sc := isSelfClosingSpan(documentXML, span)
				cur = elemInfo{found: true, tagSpan: span, selfClosing: sc, attrs: t.Attr}
				if sc {
					infos = append(infos, cur)
					childrenList = append(childrenList, nil)
				} else {
					in = true
				}
			}
		case xml.EndElement:
			if in && isWordElement(t.Name, "sectPr") {
				cur.closeStart = prevOffset
				infos = append(infos, cur)
				childrenList = append(childrenList,
					scanDirectChildren(documentXML[cur.tagSpan.End:cur.closeStart], cur.tagSpan.End, sectPrChildOrder))
				in = false
			}
		}
		prevOffset = offset
	}
	return infos, childrenList, nil
}

// planMarginPatches rewrites every <w:sectPr>'s <w:pgMar> in documentXML
// (there is ordinarily exactly one, a direct child of <w:body>, but a
// multi-section document can have more) to carry marginsMM (top, right,
// bottom, left) converted to twips, preserving whatever else that pgMar
// already had (header/footer/gutter, in particular). A section that has
// NO <w:pgMar> at all gets a brand new one INSERTED, at the schema-correct
// position (right after pgSz/type/headerReference.../footerReference...,
// whichever of those exist, else as sectPr's first child), carrying OOXML's
// own conventional pgMar defaults for the three attributes this call never
// touches (header/footer 720 twips = 0.5in, gutter 0) with marginsMM's four
// values layered on top — rather than failing the whole call the way a
// missing <w:pgMar> used to (format capability review, Important 10 /
// task 9 brief, item 4): a document is not required to declare an explicit
// <w:pgMar> just because most Word/docx_write output does.
func planMarginPatches(documentXML []byte, marginsMM []float64) ([]Patch, error) {
	top := mmToTwips(marginsMM[0])
	right := mmToTwips(marginsMM[1])
	bottom := mmToTwips(marginsMM[2])
	left := mmToTwips(marginsMM[3])

	sects, childrenList, err := scanSectPrs(documentXML)
	if err != nil {
		return nil, err
	}
	if len(sects) == 0 {
		return nil, fmt.Errorf("docx: document.xml has no <w:sectPr> element; cannot set margins")
	}

	buildAttrs := func(existing []xml.Attr) []xml.Attr {
		attrs := existing
		if attrs == nil {
			// Brand-new <w:pgMar>: fall back to OOXML's own conventional
			// defaults for the three attributes marginsMM never touches, so
			// an inserted pgMar does not silently claim zero header/footer/
			// gutter space that was never requested.
			attrs = []xml.Attr{
				{Name: xml.Name{Local: "header"}, Value: "720"},
				{Name: xml.Name{Local: "footer"}, Value: "720"},
				{Name: xml.Name{Local: "gutter"}, Value: "0"},
			}
		}
		attrs = setAttr(attrs, "top", top)
		attrs = setAttr(attrs, "right", right)
		attrs = setAttr(attrs, "bottom", bottom)
		attrs = setAttr(attrs, "left", left)
		return attrs
	}

	var patches []Patch
	for i, sect := range sects {
		children := childrenList[i]
		if pm, ok := children["pgMar"]; ok {
			patches = append(patches, PatchRawSpan(documentXML, pm.tagSpan, buildTag("pgMar", buildAttrs(pm.attrs), true)))
			continue
		}
		if sect.selfClosing {
			// <w:sectPr/> with no properties at all: expand it to hold the
			// new pgMar, exactly like a self-closing pPr/rPr elsewhere in
			// this file.
			patches = append(patches, PatchRawSpan(documentXML, sect.tagSpan,
				buildTag("sectPr", sect.attrs, false)+buildTag("pgMar", buildAttrs(nil), true)+"</w:sectPr>"))
			continue
		}
		ops := make([]leafOp, 0, len(sectPrChildOrder))
		for _, name := range sectPrChildOrder {
			op := leafOp{info: children[name], local: name}
			if name == "pgMar" {
				op.active = true
				op.attrs = buildAttrs(nil)
			}
			ops = append(ops, op)
		}
		patches = append(patches, applyLeafOps(documentXML, sect.closeStart, ops)...)
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
