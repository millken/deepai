package docx

import (
	"strings"
	"testing"
)

func TestOutline_BuildsHeadingTree(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	o := d.Outline()
	if o.TotalParas != d.TotalParas() {
		t.Errorf("TotalParas = %d, want %d", o.TotalParas, d.TotalParas())
	}

	var headings []string
	for _, s := range o.Sections {
		if s.Heading != "" {
			headings = append(headings, s.Heading)
		}
	}
	want := []string{"Chapter One", "Section 1.1", "Chapter Two", "Section 2.1"}
	if len(headings) != len(want) {
		t.Fatalf("got %d headings %v, want %d %v", len(headings), headings, len(want), want)
	}
	for i := range want {
		if headings[i] != want[i] {
			t.Errorf("heading[%d] = %q, want %q", i, headings[i], want[i])
		}
	}
}

func TestOutline_LevelsComeFromStyle(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	want := map[string]int{
		"Chapter One": 1,
		"Section 1.1": 2,
		"Chapter Two": 1,
		"Section 2.1": 2,
	}
	for _, s := range d.Outline().Sections {
		if s.Heading == "" {
			continue
		}
		if got := want[s.Heading]; got != s.Level {
			t.Errorf("section %q: Level = %d, want %d", s.Heading, s.Level, got)
		}
	}
}

// TestOutline_SectionRangesTileTheDocument pins that every paragraph belongs
// to exactly one section — a gap or an overlap would make `heading` selection
// silently skip or duplicate content.
func TestOutline_SectionRangesTileTheDocument(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	o := d.Outline()
	covered := make([]int, o.TotalParas+1)
	for _, s := range o.Sections {
		if s.StartPara < 1 || s.EndPara > o.TotalParas || s.StartPara > s.EndPara {
			t.Fatalf("section %q has invalid range [%d,%d] (total %d)", s.Heading, s.StartPara, s.EndPara, o.TotalParas)
		}
		for i := s.StartPara; i <= s.EndPara; i++ {
			covered[i]++
		}
		if s.Paras != s.EndPara-s.StartPara+1 {
			t.Errorf("section %q: Paras = %d, but range [%d,%d] spans %d",
				s.Heading, s.Paras, s.StartPara, s.EndPara, s.EndPara-s.StartPara+1)
		}
	}
	for i := 1; i <= o.TotalParas; i++ {
		if covered[i] != 1 {
			t.Fatalf("paragraph %d is covered by %d sections, want exactly 1", i, covered[i])
		}
	}
}

func TestOutline_WordCountsSumToTotal(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	o := d.Outline()
	sum := 0
	for _, s := range o.Sections {
		sum += s.Words
	}
	if sum != o.Words {
		t.Errorf("section words sum to %d, Outline.Words = %d", sum, o.Words)
	}
	if o.Words == 0 {
		t.Error("Words = 0, want a positive count")
	}
}

// TestOutline_LeadingBodyBecomesAnUnnamedSection pins that content before the
// first heading is not dropped.
func TestOutline_LeadingBodyBecomesAnUnnamedSection(t *testing.T) {
	d, err := OpenDocument(fixture) // structure.docx has no headings at all
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	o := d.Outline()
	if len(o.Sections) != 1 {
		t.Fatalf("got %d sections, want 1 for a heading-less document", len(o.Sections))
	}
	s := o.Sections[0]
	if s.Heading != "" || s.Level != 0 {
		t.Errorf("section = %+v, want an unnamed level-0 section", s)
	}
	if s.StartPara != 1 || s.EndPara != o.TotalParas {
		t.Errorf("section range = [%d,%d], want [1,%d]", s.StartPara, s.EndPara, o.TotalParas)
	}
}

func TestOutline_CarriesNotes(t *testing.T) {
	d, err := OpenDocument(fixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if len(d.Outline().Notes) == 0 {
		t.Error("Outline.Notes is empty, want the header/footer declaration carried through")
	}
}

// synthPara builds a paragraph carrying just the fields buildOutline reads,
// so sectioning rules can be exercised on shapes no committed fixture has.
func synthPara(index int, style, text string) Para {
	return Para{
		Index: index,
		Style: style,
		Runs:  []Run{{Index: 1, Text: text}},
	}
}

// TestBuildOutline_LeadingBodyBeforeFirstHeading covers the one sectioning
// path no fixture reaches: structure.docx has no headings at all and
// outline.docx starts with one, so a document whose body text PRECEDES its
// first heading is otherwise untested. Getting this wrong would silently
// drop the opening paragraphs out of every section.
func TestBuildOutline_LeadingBodyBeforeFirstHeading(t *testing.T) {
	paras := []Para{
		synthPara(1, "", "opening one"),
		synthPara(2, "", "opening two"),
		synthPara(3, "Heading1", "First Heading"),
		synthPara(4, "", "body under heading"),
	}
	o := buildOutline(paras, nil)

	if len(o.Sections) != 2 {
		t.Fatalf("got %d sections, want 2 (leading unnamed + Heading1)", len(o.Sections))
	}
	lead := o.Sections[0]
	if lead.Heading != "" || lead.Level != 0 {
		t.Errorf("leading section = %+v, want unnamed level 0", lead)
	}
	if lead.StartPara != 1 || lead.EndPara != 2 || lead.Paras != 2 {
		t.Errorf("leading section range = [%d,%d] (%d paras), want [1,2] (2)",
			lead.StartPara, lead.EndPara, lead.Paras)
	}
	head := o.Sections[1]
	if head.Heading != "First Heading" || head.Level != 1 {
		t.Errorf("second section = %+v, want the Heading1 section", head)
	}
	if head.StartPara != 3 || head.EndPara != 4 {
		t.Errorf("heading section range = [%d,%d], want [3,4]", head.StartPara, head.EndPara)
	}
}

// TestBuildOutline_TilesEveryShape asserts the tiling invariant across the
// shapes a real corpus mixes: leading body, trailing heading with no body,
// consecutive headings, and a heading-only document.
func TestBuildOutline_TilesEveryShape(t *testing.T) {
	shapes := map[string][]Para{
		"leading body": {
			synthPara(1, "", "a"), synthPara(2, "Heading1", "H"), synthPara(3, "", "b"),
		},
		"trailing heading with no body": {
			synthPara(1, "Heading1", "H1"), synthPara(2, "", "a"), synthPara(3, "Heading2", "H2"),
		},
		"consecutive headings": {
			synthPara(1, "Heading1", "H1"), synthPara(2, "Heading2", "H2"), synthPara(3, "", "a"),
		},
		"headings only": {
			synthPara(1, "Heading1", "H1"), synthPara(2, "Heading1", "H2"),
		},
		"no headings": {
			synthPara(1, "", "a"), synthPara(2, "", "b"),
		},
	}
	for name, paras := range shapes {
		t.Run(name, func(t *testing.T) {
			o := buildOutline(paras, nil)
			covered := make([]int, len(paras)+1)
			for _, s := range o.Sections {
				if s.StartPara < 1 || s.EndPara > len(paras) || s.StartPara > s.EndPara {
					t.Fatalf("section %+v has an invalid range", s)
				}
				for i := s.StartPara; i <= s.EndPara; i++ {
					covered[i]++
				}
			}
			for i := 1; i <= len(paras); i++ {
				if covered[i] != 1 {
					t.Errorf("paragraph %d covered %d times, want exactly 1", i, covered[i])
				}
			}
		})
	}
}

func TestRead_RangeSelectsInclusiveBounds(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	r, err := d.Read(ReadOptions{StartPara: 3, EndPara: 5})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(r.Paras) != 3 {
		t.Fatalf("got %d paragraphs, want 3", len(r.Paras))
	}
	for i, want := range []int{3, 4, 5} {
		if r.Paras[i].Index != want {
			t.Errorf("Paras[%d].Index = %d, want %d", i, r.Paras[i].Index, want)
		}
	}
}

func TestRead_HeadingSelectsWholeSection(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	var want Section
	for _, s := range d.Outline().Sections {
		if s.Heading == "Section 1.1" {
			want = s
		}
	}
	if want.Heading == "" {
		t.Fatal("fixture lacks the Section 1.1 heading")
	}
	r, err := d.Read(ReadOptions{Heading: "Section 1.1"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(r.Paras) != want.Paras {
		t.Fatalf("got %d paragraphs, want %d", len(r.Paras), want.Paras)
	}
	if r.Paras[0].Index != want.StartPara {
		t.Errorf("first index = %d, want %d", r.Paras[0].Index, want.StartPara)
	}
}

func TestRead_UnknownHeadingErrors(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if _, err := d.Read(ReadOptions{Heading: "No Such Heading"}); err == nil {
		t.Fatal("Read with an unknown heading returned nil error")
	}
}

// TestRead_MaxCharsCutsAtParagraphBoundary is the core chunking guarantee
// (design §5.2): a chunk never splits a <w:p>, and the cursor points at the
// next unread paragraph.
func TestRead_MaxCharsCutsAtParagraphBoundary(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	r, err := d.Read(ReadOptions{MaxChars: 200})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if r.NextStartPara == 0 {
		t.Fatal("NextStartPara = 0, want a cursor (the fixture is far larger than 200 chars)")
	}
	last := r.Paras[len(r.Paras)-1].Index
	if r.NextStartPara != last+1 {
		t.Errorf("NextStartPara = %d, want %d (one past the last returned paragraph)", r.NextStartPara, last+1)
	}
	// Every returned paragraph must be whole: its text equals the document's.
	all := d.Paras()
	for _, pv := range r.Paras {
		var b strings.Builder
		for _, run := range all[pv.Index-1].Runs {
			b.WriteString(run.Text)
		}
		if pv.Text != b.String() {
			t.Errorf("paragraph %d was truncated: %q vs %q", pv.Index, pv.Text, b.String())
		}
	}
}

// TestRead_CursorWalksTheWholeDocumentExactlyOnce is the coverage guarantee
// behind §10's second acceptance criterion.
func TestRead_CursorWalksTheWholeDocumentExactlyOnce(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	seen := make([]int, d.TotalParas()+1)
	next := 1
	for guard := 0; next != 0; guard++ {
		if guard > 1000 {
			t.Fatal("cursor did not terminate")
		}
		r, err := d.Read(ReadOptions{StartPara: next, MaxChars: 150})
		if err != nil {
			t.Fatalf("Read from %d: %v", next, err)
		}
		if len(r.Paras) == 0 {
			t.Fatalf("Read from %d returned no paragraphs but cursor is %d", next, r.NextStartPara)
		}
		for _, pv := range r.Paras {
			seen[pv.Index]++
		}
		next = r.NextStartPara
	}
	for i := 1; i <= d.TotalParas(); i++ {
		if seen[i] != 1 {
			t.Fatalf("paragraph %d was read %d times, want exactly 1", i, seen[i])
		}
	}
}

// TestRead_OversizedParagraphStillAdvances guards against an infinite loop
// when one paragraph alone exceeds the budget.
func TestRead_OversizedParagraphStillAdvances(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	r, err := d.Read(ReadOptions{MaxChars: 1})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(r.Paras) != 1 {
		t.Fatalf("got %d paragraphs, want exactly 1 (the oversized one)", len(r.Paras))
	}
	if r.NextStartPara != 2 {
		t.Errorf("NextStartPara = %d, want 2", r.NextStartPara)
	}
	if len(r.Notes) == 0 {
		t.Error("Notes is empty, want a note that the paragraph exceeds the budget")
	}
}

// TestRead_FullOverBudgetFallsBackToChunk pins the 2026-08-19 contract change
// recorded in docs/DOCX_TOOLS_DESIGN.md §5: a Full read that does not fit
// the budget used to return a hard error pointing the model at outline +
// start_para/end_para. In practice that trained the model to abandon
// docx_read entirely and fall back to a bash + python script — the exact
// failure mode the docx tools exist to prevent — because from the model's
// side "the tool refused" looked like a dead end, not "there is more to
// read". A first chunk plus a Notes entry plus a non-zero next_start_para is
// the identical cursor contract a non-Full call already uses, so falling
// back degrades no differently than an ordinary chunked read would; it is
// not the kind of silent truncation §5.1 exists to prevent.
func TestRead_FullOverBudgetFallsBackToChunk(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	const budget = 100
	r, err := d.Read(ReadOptions{Full: true, MaxChars: budget})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if r.Markdown == "" {
		t.Fatal("Markdown is empty, want the first in-budget chunk")
	}
	if len(r.Markdown) > budget {
		t.Errorf("Markdown is %d chars, want it within the %d-char budget", len(r.Markdown), budget)
	}
	if r.NextStartPara <= 0 {
		t.Errorf("NextStartPara = %d, want > 0 (more to read)", r.NextStartPara)
	}

	var found bool
	for _, n := range r.Notes {
		if strings.Contains(n, "full read") && strings.Contains(n, "over the") && strings.Contains(n, "next_start_para") {
			found = true
		}
	}
	if !found {
		t.Errorf("Notes = %v, want a note naming the over-budget size and next_start_para as the continuation", r.Notes)
	}

	// Full's degrade path must produce the exact same chunk a non-Full call
	// at the same budget would — the only difference is the extra note.
	chunked, err := d.Read(ReadOptions{MaxChars: budget})
	if err != nil {
		t.Fatalf("Read (chunked): %v", err)
	}
	if r.Markdown != chunked.Markdown {
		t.Errorf("Full-degraded Markdown = %q, want it to match the chunked path's %q", r.Markdown, chunked.Markdown)
	}
	if r.NextStartPara != chunked.NextStartPara {
		t.Errorf("Full-degraded NextStartPara = %d, want it to match the chunked path's %d", r.NextStartPara, chunked.NextStartPara)
	}
	if len(r.Notes) != len(chunked.Notes)+1 {
		t.Errorf("Full-degraded Notes has %d entries, want exactly one more than the chunked path's %d", len(r.Notes), len(chunked.Notes))
	}
}

// TestRead_FullOverBudgetSingleOversizedParagraphHasNoDanglingCursor is a
// regression test for a bug caught in independent review of
// TestRead_FullOverBudgetFallsBackToChunk's fix: a ONE-paragraph document
// whose single paragraph alone renders bigger than budget hits
// readChunkedResult's "this one paragraph alone is over budget, return it
// whole" exception, which — because there is nothing after it — reports
// NextStartPara 0, exactly like a fully-satisfied read. The Full branch's
// pre-check had already decided the range was over budget (totalLen >
// budget), so an earlier version of the fallback unconditionally appended
// "returning the first chunk — continue with start_para=next_start_para".
// With NextStartPara 0, "start_para=next_start_para" resolves through
// resolveReadRange as start_para=0, i.e. "from the start" — the exact same
// call, forever: an unbounded re-read loop that never terminates and never
// tells the caller so. The fix only appends that note when
// result.NextStartPara != 0; this pins the NextStartPara == 0 case getting
// no such note (readChunkedResult's own per-paragraph overage note already
// says everything there is to say).
func TestRead_FullOverBudgetSingleOversizedParagraphHasNoDanglingCursor(t *testing.T) {
	const budget = 100
	d := bodyDoc(t, `<w:p><w:r><w:t>`+strings.Repeat("word ", 4000)+`</w:t></w:r></w:p>`)

	r, err := d.Read(ReadOptions{Full: true, MaxChars: budget})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(r.Markdown) <= budget {
		t.Fatalf("test premise stale: the single paragraph is only %d chars, want it bigger than the %d-char budget", len(r.Markdown), budget)
	}
	if r.NextStartPara != 0 {
		t.Fatalf("NextStartPara = %d, want 0 (nothing follows the document's only paragraph)", r.NextStartPara)
	}

	for _, n := range r.Notes {
		if strings.Contains(n, "start_para=next_start_para") {
			t.Errorf("Notes = %v, want no note telling the caller to resume at start_para=next_start_para — "+
				"next_start_para is 0 here, so that would resolve to start_para=0 (\"from the start\") and loop forever",
				r.Notes)
		}
	}
	// readChunkedResult's own oversized-paragraph note must still be present
	// (see TestRead_OversizedParagraphStillAdvances), so the caller is told
	// SOMETHING about the overage even without the redundant Full-specific
	// note.
	found := false
	for _, n := range r.Notes {
		if strings.HasPrefix(n, OversizedParagraphNotePrefix) {
			found = true
		}
	}
	if !found {
		t.Errorf("Notes = %v, want the oversized-paragraph note explaining the overage", r.Notes)
	}
}

func TestRead_RunsIncludesPerRunBreakdown(t *testing.T) {
	d, err := OpenDocument(fixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	r, err := d.Read(ReadOptions{StartPara: 1, EndPara: 1, Runs: true})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(r.Paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(r.Paras))
	}
	got := r.Paras[0].Runs
	if len(got) != 3 {
		t.Fatalf("got %d runs, want 3", len(got))
	}
	for i, want := range []string{"Hello ", "bold", " world"} {
		if got[i].Text != want {
			t.Errorf("Runs[%d].Text = %q, want %q", i, got[i].Text, want)
		}
		if got[i].Index != i+1 {
			t.Errorf("Runs[%d].Index = %d, want %d", i, got[i].Index, i+1)
		}
	}
}

func TestRead_OmitsRunsUnlessRequested(t *testing.T) {
	d, err := OpenDocument(fixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	r, err := d.Read(ReadOptions{StartPara: 1, EndPara: 1})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(r.Paras[0].Runs) != 0 {
		t.Errorf("Runs were populated without Runs=true")
	}
}

func TestRead_MarkdownRendersHeadingsAndParaMarkers(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	r, err := d.Read(ReadOptions{Heading: "Chapter One"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(r.Markdown, "# Chapter One") {
		t.Errorf("markdown lacks the level-1 heading:\n%s", r.Markdown)
	}
	if !strings.Contains(r.Markdown, "[para ") {
		t.Errorf("markdown lacks para_index markers:\n%s", r.Markdown)
	}
}

func TestRead_TableParagraphsCarryCellCoordinates(t *testing.T) {
	d, err := OpenDocument(fixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	r, err := d.Read(ReadOptions{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var withCell int
	for _, pv := range r.Paras {
		if pv.Cell != nil {
			withCell++
			if pv.Cell.Table != 1 || pv.Cell.Row < 1 || pv.Cell.Col < 1 {
				t.Errorf("paragraph %d has implausible cell %+v", pv.Index, *pv.Cell)
			}
		}
	}
	if withCell != 4 {
		t.Errorf("%d paragraphs carry cell coordinates, want 4", withCell)
	}
}

func TestRead_HeadingAndRangeAreMutuallyExclusive(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if _, err := d.Read(ReadOptions{Heading: "Chapter One", StartPara: 2}); err == nil {
		t.Fatal("Read accepted both Heading and StartPara, want an error")
	}
}

// TestHeadingLevel_RejectsPathologicalStyles pins C1: headingLevel's old
// unbounded "n = n*10 + digit" parse let a document-controlled w:pStyle
// value overflow int (panicking strings.Repeat in renderReadPara) or, for
// something merely large, render a grotesquely oversized heading block for
// a one-character paragraph. §4.1 only ever needs Heading1-9, so anything
// else must fall into ordinary body text instead — never panic, never
// balloon.
func TestHeadingLevel_RejectsPathologicalStyles(t *testing.T) {
	tests := []struct {
		style     string
		wantLevel int
		wantOK    bool
	}{
		{"Heading1", 1, true},
		{"Heading9", 9, true},
		{"heading3", 3, true}, // case-insensitive prefix, still single digit
		{"Heading0", 0, false},
		{"Heading10", 0, false},
		{"Heading99999999999999999999", 0, false}, // would overflow int under the old parse
		{"Heading12345678901234567890", 0, false}, // would go negative under the old parse
		{"Heading100000", 0, false},               // would render a 100KB block under the old parse
		{"HeadingFoo", 0, false},
	}
	for _, tt := range tests {
		level, ok := headingLevel(tt.style)
		if ok != tt.wantOK || level != tt.wantLevel {
			t.Errorf("headingLevel(%q) = (%d, %v), want (%d, %v)", tt.style, level, ok, tt.wantLevel, tt.wantOK)
		}
	}
}

// TestOutline_PathologicalHeadingStyleDoesNotPanic is the integration-level
// pin: buildOutline and renderReadPara must not panic (or emit a
// mega-repeated "#" prefix) when a paragraph's Style is a pathological
// heading-shaped string. Removing headingLevel's digit-count/range guard
// reproduces the reported panics directly from this test.
func TestOutline_PathologicalHeadingStyleDoesNotPanic(t *testing.T) {
	paras := []Para{
		synthPara(1, "Heading99999999999999999999", "overflow"),
		synthPara(2, "Heading12345678901234567890", "negative"),
		synthPara(3, "Heading100000", "big"),
	}
	o := buildOutline(paras, nil)
	if len(o.Sections) != 1 || o.Sections[0].Level != 0 {
		t.Fatalf("pathological heading styles were treated as headings: %+v", o.Sections)
	}
	for _, p := range paras {
		_, block := renderReadPara(p, false)
		if len(block) > 1024 {
			t.Errorf("renderReadPara(%q) produced a %d-byte block, want an ordinary body-text block", p.Style, len(block))
		}
	}
}

// ---------------------------------------------------------------------------
// I4: a heading that matches more than one section must be refused, not
// silently resolved to the first match.
// ---------------------------------------------------------------------------

func TestRead_AmbiguousHeadingIsRefused(t *testing.T) {
	d := bodyDoc(t,
		`<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>Intro</w:t></w:r></w:p>`+
			`<w:p><w:r><w:t>first body</w:t></w:r></w:p>`+
			`<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>Intro</w:t></w:r></w:p>`+
			`<w:p><w:r><w:t>second body</w:t></w:r></w:p>`)

	matches := 0
	for _, s := range d.Outline().Sections {
		if s.Heading == "Intro" {
			matches++
		}
	}
	if matches != 2 {
		t.Fatalf("test setup produced %d \"Intro\" sections, want 2", matches)
	}

	_, err := d.Read(ReadOptions{Heading: "Intro"})
	if err == nil {
		t.Fatal("Read with an ambiguous heading returned nil error")
	}
	if !strings.Contains(err.Error(), "2") {
		t.Errorf("error = %q, want it to state how many sections matched", err)
	}
	if !strings.Contains(err.Error(), "start_para") {
		t.Errorf("error = %q, want it to suggest start_para/end_para as the alternative", err)
	}
}

// TestRead_UniqueHeadingStillWorks is the negative half: a heading that
// matches exactly one section must keep working exactly as before.
func TestRead_UniqueHeadingStillWorks(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if _, err := d.Read(ReadOptions{Heading: "Chapter One"}); err != nil {
		t.Fatalf("Read: %v", err)
	}
}

// ---------------------------------------------------------------------------
// I5: ReadResult exposes the resolved range so a heading-scoped chunked read
// can be resumed without spilling into the next section.
// ---------------------------------------------------------------------------

func TestRead_RangeStartEndReflectResolvedBounds(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	r, err := d.Read(ReadOptions{StartPara: 3, EndPara: 5})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if r.RangeStart != 3 || r.RangeEnd != 5 {
		t.Errorf("RangeStart/RangeEnd = %d/%d, want 3/5", r.RangeStart, r.RangeEnd)
	}
}

func TestRead_HeadingRangeReflectsSectionBounds(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	var want Section
	for _, s := range d.Outline().Sections {
		if s.Heading == "Section 1.1" {
			want = s
		}
	}
	if want.Heading == "" {
		t.Fatal("fixture lacks the Section 1.1 heading")
	}
	r, err := d.Read(ReadOptions{Heading: "Section 1.1"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if r.RangeStart != want.StartPara || r.RangeEnd != want.EndPara {
		t.Errorf("RangeStart/RangeEnd = %d/%d, want %d/%d", r.RangeStart, r.RangeEnd, want.StartPara, want.EndPara)
	}
}

// TestRead_ResumingWithRangeEndStaysWithinTheHeadingSection is I5's core
// integration pin: Read{Heading: X, MaxChars: N} can be resumed via
// Read{StartPara: NextStartPara, EndPara: RangeEnd} without ever reading a
// paragraph belonging to the NEXT section. Before this fix, ReadResult had
// no way to carry the section's end forward, so the only "natural" resume
// (repeating StartPara alone) would run open-ended past "Chapter One" into
// "Section 1.1".
func TestRead_ResumingWithRangeEndStaysWithinTheHeadingSection(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	var want Section
	for _, s := range d.Outline().Sections {
		if s.Heading == "Chapter One" {
			want = s
		}
	}
	if want.Heading == "" {
		t.Fatal("fixture lacks the Chapter One heading")
	}

	first, err := d.Read(ReadOptions{Heading: "Chapter One", MaxChars: 40})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if first.RangeStart != want.StartPara || first.RangeEnd != want.EndPara {
		t.Fatalf("RangeStart/RangeEnd = %d/%d, want %d/%d", first.RangeStart, first.RangeEnd, want.StartPara, want.EndPara)
	}
	if first.NextStartPara == 0 {
		t.Fatal("expected MaxChars: 40 to force chunking within Chapter One")
	}

	seen := make(map[int]bool)
	for _, pv := range first.Paras {
		seen[pv.Index] = true
	}

	next := first.NextStartPara
	for guard := 0; next != 0; guard++ {
		if guard > 100 {
			t.Fatal("resume did not terminate")
		}
		part, err := d.Read(ReadOptions{StartPara: next, EndPara: first.RangeEnd, MaxChars: 40})
		if err != nil {
			t.Fatalf("Read from %d: %v", next, err)
		}
		if len(part.Paras) == 0 {
			t.Fatalf("Read from %d returned nothing, but cursor is %d", next, part.NextStartPara)
		}
		for _, pv := range part.Paras {
			if pv.Index > first.RangeEnd {
				t.Fatalf("resumed read spilled past RangeEnd: paragraph %d > %d (into the next section)", pv.Index, first.RangeEnd)
			}
			seen[pv.Index] = true
		}
		next = part.NextStartPara
	}

	for i := want.StartPara; i <= want.EndPara; i++ {
		if !seen[i] {
			t.Errorf("paragraph %d in \"Chapter One\" was never read during the resume walk", i)
		}
	}
}

// ---------------------------------------------------------------------------
// I6: MaxChars <= 0 must apply the default budget on the chunked (non-Full)
// path too, not just on Full.
// ---------------------------------------------------------------------------

func TestRead_MaxCharsZeroAppliesDefaultBudgetOnChunkedPath(t *testing.T) {
	var body strings.Builder
	// Each paragraph's own rendered block is small, but 400 of them add up
	// to comfortably more than DefaultReadBudget — this only passes if the
	// chunked path actually applies a default budget when MaxChars is left
	// at 0. Before the fix, budget stayed 0 and the "budget > 0" guard made
	// every paragraph fit, returning the entire (oversized) document.
	for i := 0; i < 400; i++ {
		body.WriteString(`<w:p><w:r><w:t>filler paragraph text here</w:t></w:r></w:p>`)
	}
	d := bodyDoc(t, body.String())

	r, err := d.Read(ReadOptions{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if r.NextStartPara == 0 {
		t.Fatal("NextStartPara = 0, want the default budget to force chunking on a document far bigger than DefaultReadBudget")
	}
	if len(r.Markdown) > DefaultReadBudget {
		t.Errorf("Markdown is %d bytes, want at most DefaultReadBudget (%d)", len(r.Markdown), DefaultReadBudget)
	}
}

// ---------------------------------------------------------------------------
// Minor fix: negative StartPara/EndPara must be refused, not silently
// normalized into "from the start"/"to the end".
// ---------------------------------------------------------------------------

func TestRead_NegativeStartParaIsRefused(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if _, err := d.Read(ReadOptions{StartPara: -1}); err == nil {
		t.Fatal("Read with a negative StartPara returned nil error")
	}
}

func TestRead_NegativeEndParaIsRefused(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if _, err := d.Read(ReadOptions{StartPara: 3, EndPara: -1}); err == nil {
		t.Fatal("Read with a negative EndPara returned nil error (would silently read to the end)")
	}
}

// ---------------------------------------------------------------------------
// Minor fix: a "[para N]" look-alike sequence inside untrusted paragraph
// text must not be byte-identical to Read's own trust-bearing marker.
// ---------------------------------------------------------------------------

func TestRenderReadPara_NeutralizesParaMarkerLookalikesInText(t *testing.T) {
	p := synthPara(3, "", "see [para 7] for details")
	_, block := renderReadPara(p, false)

	if !strings.Contains(block, "[para 3]") {
		t.Fatalf("block %q lacks the real marker for this paragraph", block)
	}
	// If the look-alike inside the text were not neutralized, the literal
	// sequence "[para " would appear twice: once as the real marker, once
	// inside the paragraph's own text.
	if n := strings.Count(block, "[para "); n != 1 {
		t.Errorf("block %q contains %d literal \"[para \" sequences, want exactly 1 (the real marker); the in-text look-alike was not neutralized", block, n)
	}
	if !strings.Contains(block, "​para 7]") {
		t.Errorf("block %q does not contain the neutralized look-alike (zero-width space + \"para 7]\")", block)
	}
	if !strings.Contains(block, "para 7]") {
		t.Errorf("block %q lost the paragraph's own text content after neutralization", block)
	}
}

func TestNeutralizeParaMarkers_LeavesOrdinaryTextAlone(t *testing.T) {
	s := "nothing marker-shaped here"
	if got := neutralizeParaMarkers(s); got != s {
		t.Errorf("neutralizeParaMarkers(%q) = %q, want it unchanged", s, got)
	}
}

// ---------------------------------------------------------------------------
// Coverage gap: paraTextWithBreaks' break-insertion branch — neither
// fixture contains a <w:br/>, so this is tested on synthetic input.
// ---------------------------------------------------------------------------

func TestParaTextWithBreaks_InsertsNewlineAfterListedRuns(t *testing.T) {
	p := Para{
		Index: 1,
		Runs: []Run{
			{Index: 1, Text: "line one"},
			{Index: 2, Text: "line two"},
			{Index: 3, Text: "line three"},
		},
		Breaks: []int{1, 2},
	}
	got := paraTextWithBreaks(p)
	want := "line one\nline two\nline three"
	if got != want {
		t.Errorf("paraTextWithBreaks = %q, want %q", got, want)
	}
}

func TestParaTextWithBreaks_NoBreaksMatchesOutlineText(t *testing.T) {
	p := synthPara(1, "", "plain text, no breaks")
	if got, want := paraTextWithBreaks(p), outlineParaText(p); got != want {
		t.Errorf("paraTextWithBreaks = %q, want it to match outlineParaText %q when there are no breaks", got, want)
	}
}

// ---------------------------------------------------------------------------
// Coverage gap: renderReadPara's SkippedTextBox -> ParaView.Note branch.
// ---------------------------------------------------------------------------

func TestRenderReadPara_SkippedTextBoxProducesANote(t *testing.T) {
	p := synthPara(1, "", "before after")
	p.SkippedTextBox = true
	pv, _ := renderReadPara(p, false)
	if pv.Note == "" {
		t.Fatal("ParaView.Note is empty, want a note about the skipped text box")
	}
	if !strings.Contains(pv.Note, "text box") {
		t.Errorf("Note = %q, want it to mention the text box", pv.Note)
	}
}

func TestRenderReadPara_NoNoteWhenNoTextBoxWasSkipped(t *testing.T) {
	p := synthPara(1, "", "plain paragraph")
	pv, _ := renderReadPara(p, false)
	if pv.Note != "" {
		t.Errorf("Note = %q, want empty when no text box was skipped", pv.Note)
	}
}

// ---------------------------------------------------------------------------
// Coverage gap: the table-structure note must actually reach Read's Notes,
// not just be a defined-but-unasserted constant.
// ---------------------------------------------------------------------------

func TestRead_TableStructureNoteIsAppended(t *testing.T) {
	d, err := OpenDocument(fixture) // structure.docx has a table
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	r, err := d.Read(ReadOptions{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	found := false
	for _, n := range r.Notes {
		if n == tableStructureNote {
			found = true
		}
	}
	if !found {
		t.Errorf("Notes = %v, want it to include the table-structure note", r.Notes)
	}
}

// ---------------------------------------------------------------------------
// Coverage gap: Full's happy path (only its over-budget fallback had a test).
// ---------------------------------------------------------------------------

// TestRead_FullReturnsWholeRangeWithinBudget pins what only Full can do:
// return a range whose rendered markdown exceeds DefaultReadBudget in one
// call, instead of chunking it. outline.docx's own paragraphs render to well
// under DefaultReadBudget in total (2447 chars for all 73), so a synthetic
// document (bodyDoc, from edit_shapes_test.go) is needed to exceed it — a
// StartPara/EndPara: 1-3 range against outline.docx passes even with the
// entire `if opts.Full` block deleted from read.go, since the non-Full
// chunked path (default 8192-char budget) already returns those 3
// paragraphs whole; that gave zero Full-specific coverage.
//
// The Markdown/Paras/NextStartPara assertions below are genuine regression
// coverage of Full's happy path. It deliberately does NOT try to detect
// deletion of the `if opts.Full` block, because — verified while writing
// this test — no behavioural assertion can: Full succeeds exactly when the
// range's rendered size fits the budget, and the chunked path given the
// same MaxChars returns everything in one call under exactly that same
// condition, producing identical Markdown/Paras/NextStartPara.
//
// Full's only DISTINCT behaviour (as of the 2026-08-19 contract change) is
// what happens when the range is over budget: it degrades to the same first
// chunk a non-Full call would return, PLUS an explanatory Notes entry,
// instead of refusing outright — pinned by
// TestRead_FullOverBudgetFallsBackToChunk. At the tool layer, Full also
// skips the outline-instead-of-body default that a bodyless, rangeless call
// gets above docx.DocxOutlineParaThreshold paragraphs (see
// shouldReturnOutline in pkg/tools/builtin/docx.go) — but that distinction
// lives above this package, not in Read itself. An earlier version of this
// test asserted cap(r.Paras) to discriminate the branch (Full pre-sizes its
// slice, the chunked path appends). That was removed: slice capacity is an
// implementation detail, so the assertion would break on a harmless
// refactor while proving nothing about behaviour. A test that pins an
// implementation detail buys false confidence, not coverage.
func TestRead_FullReturnsWholeRangeWithinBudget(t *testing.T) {
	var body strings.Builder
	const paraCount = 400
	for i := 0; i < paraCount; i++ {
		body.WriteString(`<w:p><w:r><w:t>` + strings.Repeat("word ", 6) + `</w:t></w:r></w:p>`)
	}
	d := bodyDoc(t, body.String())

	// Sanity: the same range, read without Full, must be forced to chunk —
	// otherwise this test's premise (only Full can return it whole) is
	// stale.
	chunked, err := d.Read(ReadOptions{StartPara: 1, EndPara: paraCount})
	if err != nil {
		t.Fatalf("Read (chunked): %v", err)
	}
	if chunked.NextStartPara == 0 {
		t.Fatal("test premise stale: the synthetic document already fits within the default chunk budget")
	}

	r, err := d.Read(ReadOptions{StartPara: 1, EndPara: paraCount, Full: true, MaxChars: 1 << 20})
	if err != nil {
		t.Fatalf("Read (full): %v", err)
	}
	if len(r.Paras) != paraCount {
		t.Fatalf("got %d paragraphs, want %d (the whole range in one call)", len(r.Paras), paraCount)
	}
	if r.NextStartPara != 0 {
		t.Errorf("NextStartPara = %d, want 0 (Full returns everything in one call)", r.NextStartPara)
	}
	if len(r.Markdown) <= DefaultReadBudget {
		t.Errorf("Markdown is %d chars, want more than DefaultReadBudget (%d) — otherwise Full added nothing over chunking",
			len(r.Markdown), DefaultReadBudget)
	}
}

// ---------------------------------------------------------------------------
// Minor fix: Read must not pay for Document.Paras()' deep copy of every
// paragraph regardless of the requested range — that makes a chunked walk
// of an N-paragraph document cost O(N^2) instead of O(N). Read never
// mutates, so it can safely iterate d.paras directly.
// ---------------------------------------------------------------------------

func TestRead_DoesNotDeepCopyAllParagraphsRegardlessOfRange(t *testing.T) {
	d, err := OpenDocument(outlineFixture) // 73+ paragraphs
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	allocs := testing.AllocsPerRun(20, func() {
		if _, err := d.Read(ReadOptions{StartPara: 1, EndPara: 1}); err != nil {
			t.Fatal(err)
		}
	})
	// Document.Paras() deep-copies every paragraph's Runs/Breaks/Cell on
	// every call regardless of the requested range, so reading just ONE
	// paragraph out of a 70+-paragraph document would cost allocations
	// proportional to the WHOLE document if Read called it. Iterating
	// d.paras directly instead keeps a 1-paragraph read's allocation count
	// small and independent of the document's total size.
	if allocs > 30 {
		t.Errorf("Read(1 paragraph) cost %.0f allocs/op, want it small and independent of document size", allocs)
	}
}
