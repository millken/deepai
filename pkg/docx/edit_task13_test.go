package docx

// Task 13 (P2): docx_edit's text model now matches docx_read's
// (paraTextWithBreaks, not outlineParaText — see edit.go's planFindTarget/
// planParagraphTarget), find/protect matching strips the same ZWSP Read's
// own neutralizeParaMarkers inserts, error messages name an actual next
// step instead of a dead end or a vague refusal, insert calls out the
// style/numbering gap they always leave behind, and a whole-paragraph
// replace that silently drops a <w:hyperlink> says so.

import (
	"strings"
	"testing"
)

// twoLineBreakPara mimics the shape docx_write renders for a fenced code
// block with two source lines: a single paragraph whose lines are joined by
// a <w:br/>, i.e. exactly the shape neither committed fixture contains (see
// read_test.go's own note to this effect) but that docx_write produces
// routinely.
func twoLineBreakPara() string {
	return `<w:p><w:r><w:t>line1</w:t></w:r><w:r><w:br/></w:r><w:r><w:t>line2</w:t></w:r></w:p>`
}

// ---------------------------------------------------------------------------
// Item 1 (I6): planFindTarget/planParagraphTarget now use paraTextWithBreaks,
// the same text model Read renders, instead of outlineParaText's
// no-separator concatenation.
// ---------------------------------------------------------------------------

// TestEdit_FindLocatesTextAcrossABreakTheWayReadRendersIt pins the read-copy
// scenario the brief's red test names directly: a caller who reads a
// docx_write code-block paragraph via docx_read sees "line1\nline2" (Read's
// own paraTextWithBreaks rendering) and copies exactly that as find. Before
// this fix, outlineParaText's no-separator "line1line2" meant that string
// matched ZERO times — a "matched 0 times" refusal with no hint that the
// paragraph the caller was looking at literally is those two lines. After
// the fix, the find genuinely locates (count == 1); it is then correctly
// refused for spanning both lines (there is no single run to anchor a
// replace on both sides of the <w:br/>), but with the NEW, actionable
// cross-run message (item 3) rather than a bare "0 times".
func TestEdit_FindLocatesTextAcrossABreakTheWayReadRendersIt(t *testing.T) {
	d := bodyDoc(t, twoLineBreakPara())
	res, err := d.Edit([]Edit{{Para: 1, Find: strp("line1\nline2"), Text: "x"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	reason := res.Outcomes[0].Reason
	if strings.Contains(reason, "matched 0 times") {
		t.Fatalf("Reason = %q, want the find to have LOCATED the read-rendered text (not 0 matches) — "+
			"this is exactly the outlineParaText/paraTextWithBreaks mismatch the fix closes", reason)
	}
	if !strings.Contains(reason, "spans more than one run") {
		t.Errorf("Reason = %q, want the cross-run refusal (the match legitimately straddles the <w:br/>)", reason)
	}
	// Item 3: the refusal must name an actual next step, not just "P2 work".
	if !strings.Contains(reason, "docx_read") || !strings.Contains(reason, "runs:true") {
		t.Errorf("Reason = %q, want it to point at re-reading with runs:true", reason)
	}
	if !strings.Contains(reason, "run parameter") {
		t.Errorf("Reason = %q, want it to mention targeting the run parameter", reason)
	}
}

// TestEdit_FindMatchesOneLineOfABreakSeparatedParagraph is the positive
// half: a find confined to a single line (single run) of a <w:br/>-joined
// paragraph must still apply cleanly and leave the other line and the break
// untouched — the text-model switch must not regress the ordinary case.
func TestEdit_FindMatchesOneLineOfABreakSeparatedParagraph(t *testing.T) {
	d := bodyDoc(t, twoLineBreakPara())
	res, err := d.Edit([]Edit{{Para: 1, Find: strp("line2"), Text: "LINE2"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("find on a single line of a break-separated paragraph was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[0].Before != "line2" || res.Outcomes[0].After != "LINE2" {
		t.Errorf("Before/After = %q/%q, want line2/LINE2", res.Outcomes[0].Before, res.Outcomes[0].After)
	}
	d2 := d.Paras()[0]
	if len(d2.Runs) != 2 || d2.Runs[0].Text != "line1" || d2.Runs[1].Text != "LINE2" {
		t.Fatalf("paragraph runs = %+v, want [line1 LINE2] (the break itself must survive too)", d2.Runs)
	}
	if len(d2.Breaks) != 1 {
		t.Errorf("Breaks = %v, want the <w:br/> to survive the edit", d2.Breaks)
	}
}

// TestEditBeforeAfter_WholeParagraphReplaceBeforeIncludesLineBreaks pins the
// brief's other red-test half directly: a whole-paragraph replace's
// reported Before must contain "\n" for a paragraph with a <w:br/>, matching
// what docx_read would have shown for the same paragraph, not the
// no-separator "line1line2" outlineParaText used to report.
func TestEditBeforeAfter_WholeParagraphReplaceBeforeIncludesLineBreaks(t *testing.T) {
	d := bodyDoc(t, twoLineBreakPara())
	res, err := d.Edit([]Edit{{Para: 1, Text: "flat"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("whole-paragraph replace was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[0].Before != "line1\nline2" {
		t.Errorf("Before = %q, want %q (matching Read's own paraTextWithBreaks rendering)", res.Outcomes[0].Before, "line1\nline2")
	}
}

// TestEdit_ProtectCatchesAnItemSpanningABreak pins the "protect can only get
// more accurate" half of item 1: a protected pattern that legitimately spans
// where a <w:br/> sits (crafted, as a caller would, from Read's own
// rendered text) used to silently fail to match against the no-separator
// concatenation outlineParaText produced — meaning the item was never
// actually enforced. With paraTextWithBreaks, the pattern matches Before,
// so an After that drops it is correctly refused.
func TestEdit_ProtectCatchesAnItemSpanningABreak(t *testing.T) {
	d := bodyDoc(t, twoLineBreakPara())
	res, err := d.Edit(
		[]Edit{{Para: 1, Text: "gone"}},
		EditOptions{Protect: []string{`line1\nline2`}},
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Outcomes[0].Applied {
		t.Fatal("a replace that dropped a protected item spanning a <w:br/> was applied, want refusal")
	}
	if !strings.Contains(res.Outcomes[0].Reason, "line1") {
		t.Errorf("Reason = %q, want it to name the broken protected item", res.Outcomes[0].Reason)
	}
}

// ---------------------------------------------------------------------------
// Item 2 (M4): find/protect matching strips U+200B (Read's own
// neutralizeParaMarkers character) from both sides before comparing.
// ---------------------------------------------------------------------------

// TestEdit_FindWithCopiedZWSPStillMatches pins the exact repro: a caller
// reads a paragraph whose visible text happens to look like a "[para N]"
// marker, gets back Read's ZWSP-neutralized rendering, and copies that text
// (ZWSP and all) as find. The real document bytes never contain the ZWSP,
// so before this fix the find matched zero times with no explanation.
func TestEdit_FindWithCopiedZWSPStillMatches(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>See [para 7] for details</w:t></w:r></w:p>`)
	findWithZWSP := "[" + zeroWidthSpace + "para 7]"
	res, err := d.Edit([]Edit{{Para: 1, Find: strp(findWithZWSP), Text: "[note]"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("a find carrying a copied ZWSP was refused: %s", res.Outcomes[0].Reason)
	}
	if got := paraTextAt(t, d, 1); got != "See [note] for details" {
		t.Errorf("paragraph 1 = %q, want %q", got, "See [note] for details")
	}
}

// TestEdit_FindOfOnlyZWSPIsRefusedNotPanicking pins the review's HIGH
// finding on mapStrippedRange: a paragraph whose only content is itself a
// ZWSP (e.g. <w:t>​</w:t>, a stray copy-paste artifact rather than
// anything Read ever writes into a real document) combined with a Find
// that is ALSO nothing but ZWSP strips down to an empty needle. Go's
// strings.Count(s, "") returns len(s)+1, which is 1 whenever s is itself
// empty — so the empty needle used to sail straight past the count!=1
// guard as if it had matched exactly once, at a zero-length position, and
// then panic inside mapStrippedRange (origOffset[b-1] with b-1 == -1)
// instead of failing with an explanation. This must refuse cleanly instead.
func TestEdit_FindOfOnlyZWSPIsRefusedNotPanicking(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>`+zeroWidthSpace+`</w:t></w:r></w:p>`)
	res, err := d.Edit([]Edit{{Para: 1, Find: strp(zeroWidthSpace), Text: "x"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Outcomes[0].Applied {
		t.Fatal("a find containing only zero-width characters was applied, want refusal")
	}
	if res.Outcomes[0].Reason == "" {
		t.Error("no Reason given for the all-ZWSP find refusal")
	}
	if !strings.Contains(res.Outcomes[0].Reason, "zero-width") {
		t.Errorf("Reason = %q, want it to explain the find was entirely zero-width characters", res.Outcomes[0].Reason)
	}
}

// TestEdit_ProtectWithCopiedZWSPStillEnforced is protect's sibling: a
// protect pattern copied out of Read's rendered markdown (ZWSP and all)
// must still catch a violation against the real, ZWSP-free document text.
func TestEdit_ProtectWithCopiedZWSPStillEnforced(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>See [para 7] for details</w:t></w:r></w:p>`)
	protectWithZWSP := "\\[" + zeroWidthSpace + "para 7\\]"
	res, err := d.Edit(
		[]Edit{{Para: 1, Find: strp("for details"), Text: "elsewhere"}},
		EditOptions{Protect: []string{protectWithZWSP}},
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("an edit preserving the protected item was refused: %s", res.Outcomes[0].Reason)
	}
	// Now break the protected item and confirm the ZWSP-carrying pattern
	// still catches it.
	res2, err := d.Edit(
		[]Edit{{Para: 1, Find: strp("[para 7]"), Op: "delete"}},
		EditOptions{Protect: []string{protectWithZWSP}},
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res2.Outcomes[0].Warning == "" {
		t.Error("deleting the protected item produced no warning; the ZWSP-carrying protect pattern was not enforced")
	}
}

// ---------------------------------------------------------------------------
// Item 3 (I2): cross-run find refusal now names an actual next step.
// ---------------------------------------------------------------------------

// TestEdit_FindSpanningRunsNamesAnActionableNextStep supplements the
// existing TestEdit_FindSpanningRunsIsRefused (which only checks the
// message mentions "run") with the brief's exact required wording, on the
// fixture's ordinary (no break) multi-run paragraph.
func TestEdit_FindSpanningRunsNamesAnActionableNextStep(t *testing.T) {
	d := editableDoc(t)
	target := 0
	for _, p := range d.Paras() {
		if len(p.Runs) > 1 {
			target = p.Index
			break
		}
	}
	if target == 0 {
		t.Fatal("fixture has no multi-run paragraph")
	}
	res, err := d.Edit([]Edit{{Para: target, Find: strp("Plain bold"), Text: "x"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	reason := res.Outcomes[0].Reason
	for _, want := range []string{"docx_read", "runs:true", "run parameter", "replace the whole paragraph"} {
		if !strings.Contains(reason, want) {
			t.Errorf("Reason = %q, want it to contain %q", reason, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Item 4 (I1 second half): insert always calls out the style gap it leaves
// behind, and additionally the numbering gap when inserting mid-list.
// ---------------------------------------------------------------------------

// TestEdit_InsertWarnsAboutNoParagraphStyle pins that EVERY insert (there is
// no path through planInsert that attaches a <w:pPr>/<w:pStyle> to the new
// paragraph) says so, so a caller cannot mistake "applied: true" for the new
// paragraph having picked up any style at all.
func TestEdit_InsertWarnsAboutNoParagraphStyle(t *testing.T) {
	d := editableDoc(t)
	res, err := d.Edit([]Edit{{Para: 2, Op: "insert_after", Text: "new"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("insert was refused: %s", res.Outcomes[0].Reason)
	}
	if !strings.Contains(res.Outcomes[0].Warning, "no paragraph style") {
		t.Errorf("Warning = %q, want it to call out the missing paragraph style", res.Outcomes[0].Warning)
	}
	if !strings.Contains(res.Outcomes[0].Warning, "no docx tool can currently set a paragraph style") {
		t.Errorf("Warning = %q, want it to say no docx tool can set a style yet", res.Outcomes[0].Warning)
	}
}

// listItemPara returns a <w:p> that is a numbered/bulleted list item (has
// its own <w:numPr>) holding text.
func listItemPara(text string) string {
	return `<w:p><w:pPr><w:numPr><w:ilvl w:val="0"/><w:numId w:val="1"/></w:numPr></w:pPr>` +
		`<w:r><w:t>` + text + `</w:t></w:r></w:p>`
}

// TestEdit_InsertMidListWarnsAboutNumbering pins the second half of item 4:
// inserting between two list items must additionally warn that the new
// paragraph will not inherit the list's numbering, since a caller seeing
// only applied:true would reasonably expect a mid-list insert to extend the
// list.
func TestEdit_InsertMidListWarnsAboutNumbering(t *testing.T) {
	d := bodyDoc(t, listItemPara("item1")+listItemPara("item2"))
	res, err := d.Edit([]Edit{{Para: 1, Op: "insert_after", Text: "not numbered"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("insert was refused: %s", res.Outcomes[0].Reason)
	}
	if !strings.Contains(res.Outcomes[0].Warning, "will not inherit") {
		t.Errorf("Warning = %q, want it to warn the new paragraph will not inherit the list's numbering", res.Outcomes[0].Warning)
	}
}

// TestEdit_InsertNextToOnlyOneListItemDoesNotClaimMidList is the negative
// half: inserting where only ONE neighbour is a list item (not genuinely
// "the middle of a list") must not claim otherwise.
func TestEdit_InsertNextToOnlyOneListItemDoesNotClaimMidList(t *testing.T) {
	d := bodyDoc(t, listItemPara("item1")+`<w:p><w:r><w:t>plain para</w:t></w:r></w:p>`)
	res, err := d.Edit([]Edit{{Para: 1, Op: "insert_after", Text: "x"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("insert was refused: %s", res.Outcomes[0].Reason)
	}
	if strings.Contains(res.Outcomes[0].Warning, "will not inherit") {
		t.Errorf("Warning = %q, want no numbering claim when only one neighbour is a list item", res.Outcomes[0].Warning)
	}
}

// ---------------------------------------------------------------------------
// Item 6 (M1): insert's forged-protected-item refusal has its own wording,
// distinct from replace/delete's protectReason.
// ---------------------------------------------------------------------------

// TestEdit_InsertForgedProtectReasonHasItsOwnWording supplements the
// existing TestEdit_InsertForgingAProtectedPatternIsRefused (which only
// checks the forged value is named) with the exact wording requirement:
// insert's message must not reuse replace/delete's "edit would remove or
// alter" phrasing, since nothing existing was removed or altered.
func TestEdit_InsertForgedProtectReasonHasItsOwnWording(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>Currently on v1.0.0</w:t></w:r></w:p>`)
	res, err := d.Edit(
		[]Edit{{Para: 1, Op: "insert_after", Text: "Now on v9.9.9 instead"}},
		EditOptions{Protect: []string{`v\d+\.\d+\.\d+`}},
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	reason := res.Outcomes[0].Reason
	if strings.Contains(reason, "remove or alter") {
		t.Errorf("Reason = %q, want insert's own wording, not replace/delete's \"remove or alter\" phrasing", reason)
	}
	if !strings.Contains(reason, "insert would introduce text matching protected pattern") {
		t.Errorf("Reason = %q, want it to say an insert would introduce text matching a protected pattern", reason)
	}
	if !strings.Contains(reason, "v9.9.9") {
		t.Errorf("Reason = %q, want it to name the forged item", reason)
	}
	if !strings.Contains(reason, "does not currently appear in the document") {
		t.Errorf("Reason = %q, want it to say the forged text does not currently appear in the document", reason)
	}
}

// ---------------------------------------------------------------------------
// Item 8 (M5): a whole-paragraph replace that drops a <w:hyperlink> says so.
// ---------------------------------------------------------------------------

// TestEdit_WholeParagraphReplaceRemovingAHyperlinkWarns pins M5: a
// whole-paragraph replace whose tail-splice happens to remove a complete
// <w:hyperlink> element (a link in the MIDDLE of the paragraph, with an
// ordinary run both before and after it) must say so explicitly rather than
// folding it into the generic "collapsed formatting" wording, since the
// link's relationship id is now an orphan the caller should know about.
func TestEdit_WholeParagraphReplaceRemovingAHyperlinkWarns(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>intro </w:t></w:r>`+
		`<w:hyperlink r:id="rId1"><w:r><w:t>link</w:t></w:r></w:hyperlink>`+
		`<w:r><w:t> tail</w:t></w:r></w:p>`)
	res, err := d.Edit([]Edit{{Para: 1, Text: "flat"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("whole-paragraph replace was refused: %s", res.Outcomes[0].Reason)
	}
	if !strings.Contains(res.Outcomes[0].Warning, "hyperlink removed") {
		t.Errorf("Warning = %q, want it to say a hyperlink was removed", res.Outcomes[0].Warning)
	}
	if got := paraTextAt(t, d, 1); got != "flat" {
		t.Errorf("paragraph 1 = %q, want %q", got, "flat")
	}
}

// TestEdit_WholeParagraphReplaceWithoutAHyperlinkDoesNotClaimOne is the
// negative half: the ordinary multi-run collapse (no hyperlink involved)
// must not mention one.
func TestEdit_WholeParagraphReplaceWithoutAHyperlinkDoesNotClaimOne(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>one</w:t></w:r><w:r><w:t>two</w:t></w:r></w:p>`)
	res, err := d.Edit([]Edit{{Para: 1, Text: "flat"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("whole-paragraph replace was refused: %s", res.Outcomes[0].Reason)
	}
	if strings.Contains(res.Outcomes[0].Warning, "hyperlink") {
		t.Errorf("Warning = %q, want no hyperlink claim when none was removed", res.Outcomes[0].Warning)
	}
}
