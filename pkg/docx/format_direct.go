package docx

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// formatDirectRange is Format's paragraph-range path, taken whenever a
// caller sets FormatOptions.StartPara > 0. Unlike the whole-document path
// (which edits word/styles.xml's docDefaults, changing the STYLESHEET'S
// default), this path edits word/document.xml directly: font/size land on
// each targeted run's own <w:rPr>, and line spacing/alignment land on each
// targeted paragraph's own <w:pPr>. That is direct formatting, which
// outranks whatever the stylesheet says — the same effect a user gets by
// selecting text in Word and setting a size.
//
// Every FormatOptions field that only makes sense for the WHOLE document —
// Template (also sets margins), HeadingFont (edits styles.xml, not a
// paragraph), MarginsMM (a section-level concept), and Normalize (collapses
// empty paragraphs document-wide) — is rejected outright rather than
// silently ignored or silently applied document-wide despite the range.
func (d *Document) formatDirectRange(opts FormatOptions) (FormatResult, error) {
	if opts.Template != "" {
		return FormatResult{}, fmt.Errorf(
			"docx: template %q is a document-level preset (it also sets margins) and cannot be combined with a paragraph range", opts.Template)
	}
	if opts.HeadingFont != "" {
		return FormatResult{}, fmt.Errorf(
			"docx: heading_font edits the Heading1..9 STYLE DEFINITIONS in styles.xml, not a paragraph's direct formatting, so it cannot be combined with a paragraph range")
	}
	if opts.MarginsMM != nil {
		return FormatResult{}, fmt.Errorf(
			"docx: margins_mm is a document/section-level concept (word/document.xml's <w:sectPr><w:pgMar>) and cannot be combined with a paragraph range")
	}
	if opts.Normalize {
		return FormatResult{}, fmt.Errorf(
			"docx: normalize collapses empty paragraphs document-wide and cannot be combined with a paragraph range")
	}
	if err := validateAlignAndLineSpacingMutex(opts.Align, opts.LineSpacing, opts.LineSpacingExactPt); err != nil {
		return FormatResult{}, err
	}
	if err := validateNonNegativeMeasurements(opts); err != nil {
		return FormatResult{}, err
	}

	total := d.TotalParas()
	from := opts.StartPara
	to := opts.EndPara
	if to == 0 {
		to = from
	}
	if from < 1 || from > total {
		return FormatResult{}, fmt.Errorf("docx: start_para %d is out of range; document has %d paragraph(s)", from, total)
	}
	if to < from {
		return FormatResult{}, fmt.Errorf("docx: end_para %d is before start_para %d", to, from)
	}
	if to > total {
		return FormatResult{}, fmt.Errorf("docx: end_para %d is out of range; document has %d paragraph(s)", to, total)
	}

	doc, ok := d.Part(DocumentPart)
	if !ok {
		return FormatResult{}, fmt.Errorf("docx: package has no %s part", DocumentPart)
	}
	if err := requireWordNamespacePrefix(DocumentPart, doc); err != nil {
		return FormatResult{}, err
	}
	working := doc
	paras := d.Paras()
	changed := false

	var result FormatResult

	wantsRunFormat := opts.BodyFont != "" || opts.BodySizePt != 0 || opts.BodyEastAsiaFont != ""
	if wantsRunFormat {
		// A paragraph with no runs at all is either genuinely empty OR
		// holds ONLY a skipped text box (Para.SkippedTextBox) -- Scan does
		// not surface a text box's own text as Runs, so "no runs" does not
		// mean "no visible content" for those. Reporting the latter as
		// "empty paragraph" would be a lie (task 10 brief item 4 / format
		// capability review Minor 15); the two are tracked and reported
		// separately below.
		emptyCount := 0
		var textBoxParas []int
		for _, p := range paras {
			if p.Index < from || p.Index > to || len(p.Runs) != 0 {
				continue
			}
			if p.SkippedTextBox {
				textBoxParas = append(textBoxParas, p.Index)
				continue
			}
			emptyCount++
		}

		out, n, err := applyDirectRunFormat(working, paras, from, to, opts.BodyFont, opts.BodyEastAsiaFont, opts.BodySizePt)
		if err != nil {
			return FormatResult{}, fmt.Errorf("docx: apply direct run formatting: %w", err)
		}
		if n > 0 {
			working = out
			changed = true
			paras, err = Scan(working)
			if err != nil {
				return FormatResult{}, fmt.Errorf("docx: rescan after direct run formatting: %w", err)
			}
		}
		// n counts only paragraphs a byte actually changed for (task 10
		// brief item 3): a repeat call whose runs already carry exactly the
		// requested font/size reports n==0, so the field is reported as an
		// "already ..." note instead of a (false) Applied entry, mirroring
		// the whole-document path's own already-empty-Applied convention
		// (docxFormatNoChangeNote at the tool layer).
		if opts.BodyFont != "" {
			if n > 0 {
				result.Applied = append(result.Applied, fmt.Sprintf(
					"paragraph %d-%d font -> %s (%d paragraph(s))", from, to, opts.BodyFont, n))
			} else {
				result.Notes = append(result.Notes, fmt.Sprintf(
					"paragraph %d-%d font already %s; no change", from, to, opts.BodyFont))
			}
		}
		if opts.BodyEastAsiaFont != "" {
			if n > 0 {
				result.Applied = append(result.Applied, fmt.Sprintf(
					"paragraph %d-%d east asia font -> %s (%d paragraph(s))", from, to, opts.BodyEastAsiaFont, n))
			} else {
				result.Notes = append(result.Notes, fmt.Sprintf(
					"paragraph %d-%d east asia font already %s; no change", from, to, opts.BodyEastAsiaFont))
			}
		}
		if opts.BodySizePt != 0 {
			if n > 0 {
				result.Applied = append(result.Applied, fmt.Sprintf(
					"paragraph %d-%d size -> %gpt (%d paragraph(s))", from, to, opts.BodySizePt, n))
			} else {
				result.Notes = append(result.Notes, fmt.Sprintf(
					"paragraph %d-%d size already %gpt; no change", from, to, opts.BodySizePt))
			}
		}
		if emptyCount > 0 {
			result.Notes = append(result.Notes, fmt.Sprintf(
				"%d empty paragraph(s) in the range have no runs, so font/size direct formatting was skipped for them; "+
					"paragraph-level formatting (line spacing/alignment/first-line indent/spacing before/after) still applies", emptyCount))
		}
		for _, idx := range textBoxParas {
			result.Notes = append(result.Notes, fmt.Sprintf(
				"paragraph %d is inside a text box; direct formatting skipped", idx))
		}
	}

	wantsParaFormat := opts.LineSpacing != 0 || opts.LineSpacingExactPt != 0 || opts.Align != "" ||
		opts.FirstLineIndentChars != 0 || opts.SpaceBeforePt != 0 || opts.SpaceAfterPt != 0
	if wantsParaFormat {
		req := pParaRequest{
			LineSpacing:          opts.LineSpacing,
			LineSpacingExactPt:   opts.LineSpacingExactPt,
			SpaceBeforePt:        opts.SpaceBeforePt,
			SpaceAfterPt:         opts.SpaceAfterPt,
			Align:                opts.Align,
			FirstLineIndentChars: opts.FirstLineIndentChars,
		}
		out, n, dropNotes, err := applyDirectParaFormat(working, paras, from, to, req)
		if err != nil {
			return FormatResult{}, fmt.Errorf("docx: apply direct paragraph formatting: %w", err)
		}
		if n > 0 {
			working = out
			changed = true
		}
		result.Notes = append(result.Notes, dropNotes...)
		// n counts only paragraphs a byte actually changed for (task 10
		// brief item 3) -- see the run-format block's identical comment
		// above.
		if opts.LineSpacing != 0 {
			if n > 0 {
				result.Applied = append(result.Applied, fmt.Sprintf(
					"paragraph %d-%d line spacing -> %g (%d paragraph(s))", from, to, opts.LineSpacing, n))
			} else {
				result.Notes = append(result.Notes, fmt.Sprintf(
					"paragraph %d-%d line spacing already %g; no change", from, to, opts.LineSpacing))
			}
		}
		if opts.LineSpacingExactPt != 0 {
			if n > 0 {
				result.Applied = append(result.Applied, fmt.Sprintf(
					"paragraph %d-%d line spacing -> exact %gpt (%d paragraph(s))", from, to, opts.LineSpacingExactPt, n))
			} else {
				result.Notes = append(result.Notes, fmt.Sprintf(
					"paragraph %d-%d line spacing already exact %gpt; no change", from, to, opts.LineSpacingExactPt))
			}
		}
		if opts.SpaceBeforePt != 0 {
			if n > 0 {
				result.Applied = append(result.Applied, fmt.Sprintf(
					"paragraph %d-%d space before -> %gpt (%d paragraph(s))", from, to, opts.SpaceBeforePt, n))
			} else {
				result.Notes = append(result.Notes, fmt.Sprintf(
					"paragraph %d-%d space before already %gpt; no change", from, to, opts.SpaceBeforePt))
			}
		}
		if opts.SpaceAfterPt != 0 {
			if n > 0 {
				result.Applied = append(result.Applied, fmt.Sprintf(
					"paragraph %d-%d space after -> %gpt (%d paragraph(s))", from, to, opts.SpaceAfterPt, n))
			} else {
				result.Notes = append(result.Notes, fmt.Sprintf(
					"paragraph %d-%d space after already %gpt; no change", from, to, opts.SpaceAfterPt))
			}
		}
		if opts.Align != "" {
			if n > 0 {
				result.Applied = append(result.Applied, fmt.Sprintf(
					"paragraph %d-%d alignment -> %s (%d paragraph(s))", from, to, opts.Align, n))
			} else {
				result.Notes = append(result.Notes, fmt.Sprintf(
					"paragraph %d-%d alignment already %s; no change", from, to, opts.Align))
			}
		}
		if opts.FirstLineIndentChars != 0 {
			if n > 0 {
				result.Applied = append(result.Applied, fmt.Sprintf(
					"paragraph %d-%d first line indent -> %g chars (%d paragraph(s))", from, to, opts.FirstLineIndentChars, n))
			} else {
				result.Notes = append(result.Notes, fmt.Sprintf(
					"paragraph %d-%d first line indent already %g chars; no change", from, to, opts.FirstLineIndentChars))
			}
		}
	}

	if changed {
		if err := d.SetPart(DocumentPart, working); err != nil {
			return FormatResult{}, err
		}
	}
	// TotalParas is always populated (docx_edit parity, task 10 brief item
	// 1); ParaCountChanged is always false -- no field this path accepts can
	// change paragraph count (Normalize, the only one that can, is refused
	// outright above when combined with a range). total was already
	// captured before any mutation, so it is exactly the current count.
	result.TotalParas = total
	return result, nil
}

// applyDirectRunFormat rewrites every run's own <w:rPr> for every paragraph
// in [from,to] (1-based, inclusive) that has at least one run, setting font
// (via <w:rFonts> ascii/hAnsi/eastAsia/cs, skipped when "") and/or
// eastAsiaFont (via the SAME <w:rFonts>'s eastAsia ONLY, skipped when "")
// and/or sizePt (via <w:sz>+<w:szCs> kept in sync, skipped when 0). A
// paragraph with zero runs is left completely untouched — there is no
// <w:r> to attach a <w:rPr> to — and does not count toward the returned
// paragraph count; callers that need to report this (Format's Notes) must
// compute it themselves from paras, the same slice this function was
// given.
//
// A single <w:r> is only ever patched once even if it produced more than
// one Run (scan.go's Run.Elem doc comment: a <w:r> with multiple <w:t>
// children shares one Elem across all of them), tracked via seenElems.
//
// paras must be Scan's output for documentXML (or an equivalent rescan) —
// stale offsets from an earlier version of the bytes would corrupt the
// splice.
func applyDirectRunFormat(documentXML []byte, paras []Para, from, to int, font, eastAsiaFont string, sizePt float64) ([]byte, int, error) {
	if font == "" && eastAsiaFont == "" && sizePt == 0 {
		out := make([]byte, len(documentXML))
		copy(out, documentXML)
		return out, 0, nil
	}

	var patches []Patch
	seenElems := make(map[Span]bool)
	changed := 0

	for _, p := range paras {
		if p.Index < from || p.Index > to {
			continue
		}
		if len(p.Runs) == 0 {
			continue
		}
		touchedThisPara := false
		for _, r := range p.Runs {
			if seenElems[r.Elem] {
				continue
			}
			seenElems[r.Elem] = true

			openEnd, rpr, children, err := scanRunProps(documentXML, r.Elem)
			if err != nil {
				return nil, 0, fmt.Errorf("paragraph %d: %w", p.Index, err)
			}
			// filterChangedPatches drops any leaf rewrite that would leave
			// its span byte-identical to what it already is -- the
			// byte-level idempotency check task 10 brief item 3 requires: a
			// run whose rPr already carries exactly the requested font/size
			// must not be counted as "changed" on a repeat call (task 8
			// review's range-path finding).
			runPatches := filterChangedPatches(planRunRPrPatches(documentXML, openEnd, rpr, children, font, eastAsiaFont, sizePt))
			if len(runPatches) == 0 {
				continue
			}
			patches = append(patches, runPatches...)
			touchedThisPara = true
		}
		if touchedThisPara {
			changed++
		}
	}

	if len(patches) == 0 {
		out := make([]byte, len(documentXML))
		copy(out, documentXML)
		return out, changed, nil
	}
	out, err := Apply(documentXML, patches)
	if err != nil {
		return nil, 0, fmt.Errorf("docx: apply direct run formatting: %w", err)
	}
	return out, changed, nil
}

// applyDirectParaFormat rewrites every paragraph's own <w:pPr> for every
// paragraph in [from,to] (1-based, inclusive), applying req's fields
// (LineSpacing/LineSpacingExactPt/SpaceBeforePt/SpaceAfterPt all land on
// the SAME <w:spacing>; Align on <w:jc>; FirstLineIndentChars on <w:ind>).
// Unlike applyDirectRunFormat, a paragraph with zero runs is NOT skipped:
// paragraph-level properties apply to an empty paragraph exactly as they
// do to any other, including a genuinely self-closing <w:p/> (expanded in
// place into <w:p><w:pPr>...</w:pPr></w:p> — there is no content model to
// insert into otherwise).
//
// notes reports, aggregated across the whole range rather than once per
// paragraph, the same silent-drop caveats pPrDropNotes covers for the
// whole-document path (task 9 brief, item 7b): a pre-existing w:hanging/
// w:hangingChars removed by a real FirstLineIndentChars, or a pre-existing
// w:beforeAutospacing/w:afterAutospacing removed by a real SpaceBeforePt/
// SpaceAfterPt.
func applyDirectParaFormat(documentXML []byte, paras []Para, from, to int, req pParaRequest) ([]byte, int, []string, error) {
	if req.isZero() {
		out := make([]byte, len(documentXML))
		copy(out, documentXML)
		return out, 0, nil, nil
	}

	var patches []Patch
	changed := 0
	var hangingDropped, beforeAutoDropped, afterAutoDropped int

	for _, p := range paras {
		if p.Index < from || p.Index > to {
			continue
		}

		if isSelfClosingSpan(documentXML, p.Span) {
			newXML := expandSelfClosingParagraph(documentXML, p.Span, req)
			patches = append(patches, PatchRawSpan(documentXML, p.Span, newXML))
			changed++
			continue
		}

		openEnd, ppr, children, err := scanParaProps(documentXML, p.Span)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("paragraph %d: %w", p.Index, err)
		}
		if req.FirstLineIndentChars != 0 {
			if ind, ok := children["ind"]; ok && (hasAttr(ind.attrs, "hanging") || hasAttr(ind.attrs, "hangingChars")) {
				hangingDropped++
			}
		}
		if req.SpaceBeforePt != 0 {
			if sp, ok := children["spacing"]; ok && hasAttr(sp.attrs, "beforeAutospacing") {
				beforeAutoDropped++
			}
		}
		if req.SpaceAfterPt != 0 {
			if sp, ok := children["spacing"]; ok && hasAttr(sp.attrs, "afterAutospacing") {
				afterAutoDropped++
			}
		}
		// filterChangedPatches: see applyDirectRunFormat's identical comment
		// above -- a paragraph whose pPr already carries exactly the
		// requested spacing/align/indent must not be counted as "changed"
		// on a repeat call (task 10 brief item 3; this is the specific
		// function task 8's review named: "applyDirectParaFormat 对区间每段
		// 无条件 changed++").
		paraPatches := filterChangedPatches(planParaPPrPatches(documentXML, openEnd, ppr, children, req))
		if len(paraPatches) == 0 {
			continue
		}
		patches = append(patches, paraPatches...)
		changed++
	}

	var notes []string
	if hangingDropped > 0 {
		notes = append(notes, fmt.Sprintf(
			"first_line_indent_chars removed an existing hanging indent (w:hanging/w:hangingChars) in %d paragraph(s) in the range, which would otherwise have taken precedence and made it invisible",
			hangingDropped))
	}
	if beforeAutoDropped > 0 {
		notes = append(notes, fmt.Sprintf(
			"space_before_pt removed an existing w:beforeAutospacing flag in %d paragraph(s) in the range, which would otherwise have made the new value invisible",
			beforeAutoDropped))
	}
	if afterAutoDropped > 0 {
		notes = append(notes, fmt.Sprintf(
			"space_after_pt removed an existing w:afterAutospacing flag in %d paragraph(s) in the range, which would otherwise have made the new value invisible",
			afterAutoDropped))
	}

	if len(patches) == 0 {
		out := make([]byte, len(documentXML))
		copy(out, documentXML)
		return out, changed, notes, nil
	}
	out, err := Apply(documentXML, patches)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("docx: apply direct paragraph formatting: %w", err)
	}
	return out, changed, notes, nil
}

// expandSelfClosingParagraph rewrites a self-closing <w:p .../> into
// <w:p ...><w:pPr>...</w:pPr></w:p>, carrying over whatever attributes the
// original tag had (e.g. w:rsidR) by editing the existing bytes rather than
// synthesizing a bare "<w:p>" that would silently drop them.
func expandSelfClosingParagraph(doc []byte, span Span, req pParaRequest) string {
	openTag := string(doc[span.Start:span.End])
	withoutSlash := strings.TrimSuffix(openTag, "/>") + ">"

	var b strings.Builder
	b.WriteString(withoutSlash)
	b.WriteString(buildParaPPr(nil, req))
	b.WriteString("</w:p>")
	return b.String()
}

// buildParaPPr renders a brand new <w:pPr>...</w:pPr> element from scratch,
// used both by expandSelfClosingParagraph and by planParaPPrPatches when no
// <w:pPr> exists at all. pprAttrs carries attributes to place on the
// <w:pPr> tag itself (preserved when an existing self-closing <w:pPr/> is
// being expanded; nil for a brand new one). Delegating to
// buildPPrOps(nil, req)/renderActiveLeaves (rather than hand-rendering
// spacing/jc in a fixed order, as an earlier version of this function did)
// is what lets a brand new pPr pick up FirstLineIndentChars' <w:ind> in the
// schema-correct position (between spacing and jc) for free.
func buildParaPPr(pprAttrs []xml.Attr, req pParaRequest) string {
	var b strings.Builder
	b.WriteString(buildTag("pPr", pprAttrs, false))
	b.WriteString(renderActiveLeaves(buildPPrOps(nil, req)))
	b.WriteString("</w:pPr>")
	return b.String()
}

// buildRunRPr renders a brand new <w:rPr>...</w:rPr> element from scratch,
// mirroring buildParaPPr for the run-level case. rprAttrs carries
// attributes for the <w:rPr> tag itself (preserved when an existing
// self-closing <w:rPr/> is being expanded; nil for a brand new one).
func buildRunRPr(rprAttrs []xml.Attr, font, eastAsiaFont string, sizePt float64) string {
	var b strings.Builder
	b.WriteString(buildTag("rPr", rprAttrs, false))
	if font != "" || eastAsiaFont != "" {
		b.WriteString(buildTag("rFonts", rFontsLatinAndEastAsiaAttrs(nil, font, eastAsiaFont), true))
	}
	if sizePt != 0 {
		half := ptToHalfPoints(sizePt)
		b.WriteString(buildTag("sz", setAttr(nil, "val", half), true))
		b.WriteString(buildTag("szCs", setAttr(nil, "val", half), true))
	}
	b.WriteString("</w:rPr>")
	return b.String()
}

// planRunRPrPatches builds the patches for one run's direct font/size
// formatting, given what scanRunProps found for it. Three shapes, matching
// the plan's three merge cases:
//
//  1. no <w:rPr> at all: insert a brand new one at openEnd, the run's
//     content start (right after its own <w:r ...> start tag) — <w:rPr>
//     must be the run's FIRST child per the OOXML schema.
//  2. <w:rPr/> present but self-closing (no properties at all): expand it
//     in place, preserving whatever attributes the <w:rPr> tag itself
//     carried.
//  3. <w:rPr>...</w:rPr> present with content: delegate to
//     planRPrFontSizePatches, the merge-not-replace machinery
//     planStylesPatches's docDefaults path also uses — an existing
//     <w:rFonts>/<w:sz>/<w:szCs> is rewritten in place (never duplicated), a
//     newly inserted <w:rFonts> lands as rPr's first child (EG_RPrBase
//     order), a newly inserted <w:sz>/<w:szCs> lands before whichever later
//     EG_RPrBase sibling already exists (task 8 brief, §3a), and everything
//     else already inside <w:rPr> (bold, colour, ...) is left completely
//     alone. children is rPr's full set of tracked direct children
//     (scanRunProps, via scanDirectChildren against rPrChildOrder).
func planRunRPrPatches(doc []byte, openEnd int, rpr elemInfo, children map[string]elemInfo, font, eastAsiaFont string, sizePt float64) []Patch {
	if !rpr.found {
		return []Patch{PatchRawSpan(doc, Span{openEnd, openEnd}, buildRunRPr(nil, font, eastAsiaFont, sizePt))}
	}
	if rpr.selfClosing {
		return []Patch{PatchRawSpan(doc, rpr.tagSpan, buildRunRPr(rpr.attrs, font, eastAsiaFont, sizePt))}
	}
	return planRPrFontSizePatches(doc, rpr.closeStart, children, font, eastAsiaFont, sizePt, rFontsLatinAndEastAsiaAttrs)
}

// planParaPPrPatches is planRunRPrPatches's paragraph-level twin: the same
// three shapes (missing / self-closing / present-with-content), landing on
// <w:pPr><w:spacing>/<w:ind>/<w:jc> instead of <w:rPr><w:rFonts>/<w:sz>/
// <w:szCs>. The self-closing-<w:p/> case is handled one level up, by
// expandSelfClosingParagraph, before this function is ever called — by the
// time openEnd/ppr/children reach here, the paragraph itself is known to
// have a content model. children is pPr's full set of tracked direct
// children (scanParaProps, via scanDirectChildren) — buildPPrOps uses all of
// it as an anchor set so a newly inserted spacing/ind/jc lands in CT_PPr's
// schema order even when pPr also carries an <w:rPr> or a <w:sectPr>
// (format capability review, Critical 1), not merely relative to
// spacing/ind/jc themselves.
func planParaPPrPatches(doc []byte, openEnd int, ppr elemInfo, children map[string]elemInfo, req pParaRequest) []Patch {
	if !ppr.found {
		return []Patch{PatchRawSpan(doc, Span{openEnd, openEnd}, buildParaPPr(nil, req))}
	}
	if ppr.selfClosing {
		return []Patch{PatchRawSpan(doc, ppr.tagSpan, buildParaPPr(ppr.attrs, req))}
	}
	return applyLeafOps(doc, ppr.closeStart, buildPPrOps(children, req))
}

// scanRunProps scans elem — a single run's <w:r>...</w:r> byte range, as
// captured by Run.Elem — for its own direct <w:rPr> (NOT docDefaults; this
// is a completely separate scan from scanDocDefaults, scoped to one run
// instead of the whole stylesheet) and, within it, <w:rFonts>/<w:sz>/
// <w:szCs>. openEnd is the absolute offset immediately after the run's own
// <w:r ...> start tag — where a brand new <w:rPr> must be inserted as the
// run's first child when none exists.
//
// elem is guaranteed non-self-closing here: every Run in Para.Runs was
// produced by scan.go from a <w:t> element, so the <w:r> that contains it
// always has content and is never itself a bare "<w:r/>".
//
// A run nested inside <w:hyperlink> (or <w:smartTag>, <w:ins>, ...) is
// scanned exactly the same way: elem is only ever the <w:r>...</w:r> span
// itself, never the enclosing wrapper, so this function neither sees nor
// needs to care what (if anything) contains the run.
//
// rPr's own true closing tag is found via a small depth counter (targetDepth
// below), not by matching the next EndElement literally named "rPr" the way
// an earlier version of this function did: <w:rPr> can legally wrap a
// <w:rPrChange><w:rPr>...</w:rPr></w:rPrChange> holding a REVISION'S
// historical run properties — a same-named element nested inside itself.
// Matching by name alone found the NESTED rPr's close instead of the outer
// one's, truncating rpr.closeStart early; the follow-up scanDirectChildren
// call below inherits the same fix for rFonts/sz/szCs, which would
// otherwise be read from (and rewritten into) the historical copy instead
// of the run's current properties (format capability review follow-up,
// same root cause as Critical 2). children is rPr's full set of tracked
// direct children (scanDirectChildren against rPrChildOrder — not merely
// rFonts/sz/szCs), the same "full anchor set, not three individually named
// leaves" shape scanParaProps already returns for pPr, needed so a newly
// inserted sz/szCs can anchor against whichever later EG_RPrBase sibling
// (u, rPrChange, ...) already exists (task 8 brief, §3a).
func scanRunProps(doc []byte, elem Span) (openEnd int, rpr elemInfo, children map[string]elemInfo, err error) {
	sub := doc[elem.Start:elem.End]
	dec := xml.NewDecoder(bytes.NewReader(sub))

	var prevOffset int
	var rootSeen bool
	var tracking bool
	var targetDepth int

	for {
		tok, terr := dec.Token()
		if terr != nil {
			if errors.Is(terr, io.EOF) {
				break
			}
			return 0, rpr, nil, fmt.Errorf("scan run properties: %w", terr)
		}
		offset := int(dec.InputOffset())

		switch t := tok.(type) {
		case xml.StartElement:
			span := Span{elem.Start + prevOffset, elem.Start + offset}
			sc := isSelfClosingSpan(doc, span)
			switch {
			case isWordElement(t.Name, "r") && !rootSeen:
				rootSeen = true
				openEnd = elem.Start + offset
			case tracking:
				// Anything encountered once the outer rPr is being tracked
				// (including, critically, a NESTED <w:rPr> such as
				// <w:rPrChange>'s historical copy) only ever deepens
				// targetDepth; it can never re-trigger the case below.
				targetDepth++
			case !rpr.found && isWordElement(t.Name, "rPr"):
				rpr = elemInfo{found: true, tagSpan: span, selfClosing: sc, attrs: t.Attr}
				if sc {
					rpr.closeStart = rpr.tagSpan.End
				} else {
					tracking = true
					targetDepth = 0
				}
			}
		case xml.EndElement:
			if tracking {
				if targetDepth == 0 {
					rpr.closeStart = elem.Start + prevOffset
					tracking = false
				} else {
					targetDepth--
				}
			}
		}
		prevOffset = offset
	}

	if rpr.found && !rpr.selfClosing {
		children = scanDirectChildren(doc[rpr.tagSpan.End:rpr.closeStart], rpr.tagSpan.End, rPrChildOrder)
	}
	return openEnd, rpr, children, nil
}

// scanParaProps is scanRunProps's paragraph-level twin: scans span — a
// single paragraph's <w:p>...</w:p> byte range, as captured by Para.Span —
// for its own direct <w:pPr> and, within it, <w:spacing>/<w:jc>. openEnd is
// the absolute offset immediately after the paragraph's own <w:p ...>
// start tag, where a brand new <w:pPr> must be inserted as the paragraph's
// first child when none exists.
//
// Callers must not pass a self-closing <w:p/> here (see
// expandSelfClosingParagraph, which handles that shape one level up): a
// self-closing paragraph's decoder would emit its StartElement and
// EndElement at the SAME offset with no content in between, making
// "openEnd" land right after the "/>" — outside the (nonexistent) content
// model — rather than anywhere a <w:pPr> could actually be inserted.
//
// children is the full set of pPr's direct children scanDirectChildren
// tracks (pPrChildOrder) — not merely spacing/jc — found via a SEPARATE
// pass over pPr's own inner bytes once its span is known, rather than
// inline in the loop below via a boolean flag: a boolean here would
// misidentify a <w:spacing>/<w:jc>-shaped element nested inside pPr's own
// <w:rPr> (paragraph mark run properties) as one of pPr's own direct
// children, exactly the "inPPR never closes over the nested rPr" bug from
// the format capability review (Critical 2). children is nil when pPr
// itself is missing or self-closing (nothing to scan).
func scanParaProps(doc []byte, span Span) (openEnd int, ppr elemInfo, children map[string]elemInfo, err error) {
	sub := doc[span.Start:span.End]
	dec := xml.NewDecoder(bytes.NewReader(sub))

	var prevOffset int
	var rootSeen, pprSeen, inPPR bool

	for {
		tok, terr := dec.Token()
		if terr != nil {
			if errors.Is(terr, io.EOF) {
				break
			}
			return 0, ppr, nil, fmt.Errorf("scan paragraph properties: %w", terr)
		}
		offset := int(dec.InputOffset())

		switch t := tok.(type) {
		case xml.StartElement:
			s := Span{span.Start + prevOffset, span.Start + offset}
			sc := isSelfClosingSpan(doc, s)
			switch {
			case isWordElement(t.Name, "p") && !rootSeen:
				rootSeen = true
				openEnd = span.Start + offset
			case isWordElement(t.Name, "pPr") && !pprSeen:
				pprSeen = true
				ppr = elemInfo{found: true, tagSpan: s, selfClosing: sc, attrs: t.Attr}
				if !sc {
					inPPR = true
				}
			}
		case xml.EndElement:
			if isWordElement(t.Name, "pPr") && inPPR {
				ppr.closeStart = span.Start + prevOffset
				inPPR = false
			}
		}
		prevOffset = offset
	}

	if ppr.found && ppr.selfClosing {
		ppr.closeStart = ppr.tagSpan.End
	}
	if ppr.found && !ppr.selfClosing {
		children = scanDirectChildren(doc[ppr.tagSpan.End:ppr.closeStart], ppr.tagSpan.End, pPrChildOrder)
	}
	return openEnd, ppr, children, nil
}
