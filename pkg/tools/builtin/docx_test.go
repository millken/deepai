package builtin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/docx"
	"github.com/millken/deepai/pkg/models"
)

// readDocumentXML unzips a .docx and returns word/document.xml verbatim, for
// tests that need to see the raw revision markup (w:ins/w:del/w:author)
// track_changes produces. decodeRead's JSON view can never show this: pkg/docx's
// Read deliberately excludes w:delText and folds w:ins content into plain
// text, since that is what a reader should see, not what a reviewer needs to
// audit.
func readDocumentXML(t *testing.T, path string) string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open %s as zip: %v", path, err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != docx.DocumentPart {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s entry: %v", docx.DocumentPart, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %s entry: %v", docx.DocumentPart, err)
		}
		return string(data)
	}
	t.Fatalf("%s has no %s entry", path, docx.DocumentPart)
	return ""
}

// docxFixture copies the pkg/docx outline fixture into a temp dir so tests
// can edit it freely. The path is relative because pkg/tools/builtin sits
// beside pkg/docx in the module.
func docxFixture(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join("..", "..", "docx", "testdata", name)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture %s: %v", src, err)
	}
	dst := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dst
}

func callDocxRead(t *testing.T, args map[string]any) (models.ToolResult, error) {
	t.Helper()
	return DocxReadHandler(context.Background(), models.ToolCall{
		ID: "c1", Name: "docx_read", Arguments: args,
	})
}

func decodeRead(t *testing.T, res models.ToolResult) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, res.Content)
	}
	return out
}

func TestDocxRead_RequiresPath(t *testing.T) {
	if _, err := callDocxRead(t, map[string]any{}); err == nil {
		t.Fatal("missing path returned nil error")
	}
}

func TestDocxRead_RangeReturnsMarkdownAndParagraphIndex(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxRead(t, map[string]any{"path": p, "start_para": float64(1), "end_para": float64(3)})
	if err != nil {
		t.Fatalf("DocxReadHandler: %v", err)
	}
	out := decodeRead(t, res)
	md, _ := out["markdown"].(string)
	if !strings.Contains(md, "[para 1]") {
		t.Errorf("markdown lacks para markers:\n%s", md)
	}
	paras, _ := out["paragraphs"].([]any)
	if len(paras) != 3 {
		t.Fatalf("got %d paragraphs, want 3", len(paras))
	}
}

// TestDocxRead_ParagraphsCarryNoBodyText pins the "do not duplicate the body"
// decision: the markdown already holds the text (marker-neutralized), and
// re-emitting it per paragraph both doubles the payload and reintroduces the
// raw "[para N]" spoofing hazard the neutralization exists to prevent.
func TestDocxRead_ParagraphsCarryNoBodyText(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxRead(t, map[string]any{"path": p, "start_para": float64(1), "end_para": float64(3)})
	if err != nil {
		t.Fatal(err)
	}
	out := decodeRead(t, res)
	paras, _ := out["paragraphs"].([]any)
	for i, raw := range paras {
		p, _ := raw.(map[string]any)
		if _, ok := p["text"]; ok {
			t.Errorf("paragraphs[%d] carries a text field; body text belongs only in markdown", i)
		}
		if _, ok := p["index"]; !ok {
			t.Errorf("paragraphs[%d] has no index", i)
		}
	}
}

func TestDocxRead_RunsIncludedOnlyWhenRequested(t *testing.T) {
	p := docxFixture(t, "structure.docx")
	without, err := callDocxRead(t, map[string]any{"path": p, "start_para": float64(1), "end_para": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := decodeRead(t, without)["paragraphs"].([]any)
	if pm, _ := first[0].(map[string]any); pm["runs"] != nil {
		t.Error("runs were included without runs=true")
	}

	with, err := callDocxRead(t, map[string]any{"path": p, "start_para": float64(1), "end_para": float64(1), "runs": true})
	if err != nil {
		t.Fatal(err)
	}
	pm, _ := decodeRead(t, with)["paragraphs"].([]any)[0].(map[string]any)
	runs, _ := pm["runs"].([]any)
	if len(runs) != 3 {
		t.Fatalf("got %d runs, want 3", len(runs))
	}
	r0, _ := runs[0].(map[string]any)
	if r0["text"] != "Hello " {
		t.Errorf("runs[0].text = %v, want %q", r0["text"], "Hello ")
	}
}

// TestDocxRead_FitResultShrinksUntilItFits pins the budget loop itself.
//
// It uses a stub instead of a fixture on purpose: outline.docx serializes to
// roughly 6-8 KB even with runs=true, so an integration test against it would
// pass with the entire shrink loop deleted. Testing the loop directly is the
// only way to pin it.
func TestDocxRead_FitResultShrinksUntilItFits(t *testing.T) {
	var budgets []int
	// Each call returns a payload 6x the budget (a stand-in for this tool's
	// JSON inflation ratio). At budget=8192 that is 49152 bytes, over the
	// 20480-byte cap; the proportional rescale (budget*cap/len(payload)*9/10)
	// converges in exactly one step for this stub: 8192*20480/49152*9/10 =
	// 3072, and 3072*6=18432 fits under the cap on the very next attempt.
	// Pinning the exact count (2, not "at least 2") is what would have
	// caught this test's own bug when the rescale replaced a plain halving
	// (that halving needed a second shrink to converge for this same stub;
	// see the independent 2026-08-19 review that caught the stale comment
	// this replaces).
	read := func(budget int) (docxReadOutput, error) {
		budgets = append(budgets, budget)
		return docxReadOutput{Markdown: strings.Repeat("x", budget*6)}, nil
	}
	payload, err := fitDocxReadResult(read, 8192, false)
	if err != nil {
		t.Fatalf("fitDocxReadResult: %v", err)
	}
	if len(payload) > maxDocxResultBytes {
		t.Fatalf("payload is %d bytes, over the %d cap", len(payload), maxDocxResultBytes)
	}
	const wantAttempts = 2
	if len(budgets) != wantAttempts {
		t.Fatalf("budgets tried = %v (%d attempts), want exactly %d", budgets, len(budgets), wantAttempts)
	}
	for i := 1; i < len(budgets); i++ {
		if budgets[i] >= budgets[i-1] {
			t.Errorf("budget did not shrink: %v", budgets)
		}
	}
	// The caller must be told it got less than it asked for.
	var out docxReadOutput
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Notes) == 0 {
		t.Error("the result was shrunk but notes says nothing about it")
	}
}

// TestDocxRead_FitResultNoNoteOnFirstAttempt pins finding 14 of the P1c
// review: the shrink note's negative half was unpinned, so appending it
// unconditionally (rather than only on attempt > 0) would have told the
// model "pass a smaller max_chars" on every ordinary, first-try-fits read
// and still passed every other test in this file.
func TestDocxRead_FitResultNoNoteOnFirstAttempt(t *testing.T) {
	read := func(budget int) (docxReadOutput, error) {
		return docxReadOutput{Markdown: "short"}, nil
	}
	payload, err := fitDocxReadResult(read, 8192, false)
	if err != nil {
		t.Fatalf("fitDocxReadResult: %v", err)
	}
	var out docxReadOutput
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Notes) != 0 {
		t.Errorf("notes = %v, want none on a first-attempt success", out.Notes)
	}
}

// TestDocxRead_FitResultGivesUpRatherThanTruncating pins that an
// irreducible result is an error, never a silently truncated success --
// silent truncation is the exact failure the chunking design exists to
// prevent.
func TestDocxRead_FitResultGivesUpRatherThanTruncating(t *testing.T) {
	read := func(budget int) (docxReadOutput, error) {
		return docxReadOutput{Markdown: strings.Repeat("x", maxDocxResultBytes*2)}, nil
	}
	if _, err := fitDocxReadResult(read, 8192, false); err == nil {
		t.Fatal("an irreducible result returned nil error; it must refuse, not truncate")
	}
}

// TestDocxRead_FitResultErrorCarriesDiagnostic pins finding 3 of the P1c
// review: the terminal error used to discard pkg/docx's own diagnostic
// (carried in the last attempt's Notes) and tell the model to retry with a
// smaller max_chars or a narrower range — advice that can never work when a
// SINGLE paragraph alone renders larger than the cap, since pkg/docx
// deliberately returns that paragraph whole at every budget so the read
// cursor still advances. The error must instead surface the diagnostic and,
// when runs was true, point at the one lever that actually shrinks the
// payload.
func TestDocxRead_FitResultErrorCarriesDiagnostic(t *testing.T) {
	// Built from docx.OversizedParagraphNotePrefix, the same constant
	// hasOversizedParagraphNote matches against, rather than a hand-copied
	// literal: if pkg/docx ever rewords this note, this stub (and therefore
	// this test) must change in lockstep with the matcher, instead of both
	// silently drifting out of sync with the real note pkg/docx produces.
	diagnostic := docx.OversizedParagraphNotePrefix + "7 is 99999 rendered chars, exceeding the 4096-char max_chars budget; returned whole so the read cursor still advances"
	read := func(budget int) (docxReadOutput, error) {
		return docxReadOutput{
			Markdown: strings.Repeat("x", maxDocxResultBytes*2),
			Notes:    []string{diagnostic},
		}, nil
	}

	_, err := fitDocxReadResult(read, 8192, false)
	if err == nil {
		t.Fatal("an irreducible result returned nil error")
	}
	if !strings.Contains(err.Error(), diagnostic) {
		t.Errorf("error = %q, want it to carry pkg/docx's diagnostic %q", err, diagnostic)
	}
	if strings.Contains(err.Error(), "runs=true") {
		t.Errorf("error = %q, mentions runs=true when runs was never set", err)
	}
	if !strings.Contains(err.Error(), "no max_chars value or narrower range fixes this") {
		t.Errorf("error = %q, want the oversized-single-paragraph advice (hasOversizedParagraphNote branch)", err)
	}

	_, err = fitDocxReadResult(read, 8192, true)
	if err == nil {
		t.Fatal("an irreducible result returned nil error")
	}
	if !strings.Contains(err.Error(), "runs=true") {
		t.Errorf("error = %q, want it to advise dropping runs=true since that is the one lever that shrinks the payload", err)
	}
}

// TestDocxRead_RealFixtureStaysUnderTheCap is the weaker integration guard
// that complements the two loop tests above.
func TestDocxRead_RealFixtureStaysUnderTheCap(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	for _, args := range []map[string]any{
		{"path": p},
		{"path": p, "runs": true},
		{"path": p, "start_para": float64(1), "end_para": float64(73), "runs": true},
		{"path": p, "max_chars": float64(1 << 20), "runs": true},
	} {
		res, err := callDocxRead(t, args)
		if err != nil {
			t.Fatalf("args %v: %v", args, err)
		}
		if len(res.Content) > maxDocxResultBytes {
			t.Errorf("args %v: result is %d bytes, over the %d cap", args, len(res.Content), maxDocxResultBytes)
		}
	}
}

// TestDocxRead_OutlineDecisionAtTheThreshold pins the outline-by-default
// rule at its boundary. It calls the decision directly because both fixtures
// are far below DocxOutlineParaThreshold (73 vs 200), so no fixture-based
// test could distinguish the branch.
func TestDocxRead_OutlineDecisionAtTheThreshold(t *testing.T) {
	tests := []struct {
		name  string
		total int
		opts  docxReadArgs
		want  bool
	}{
		{"below threshold", docx.DocxOutlineParaThreshold - 1, docxReadArgs{}, false},
		{"at threshold", docx.DocxOutlineParaThreshold, docxReadArgs{}, false},
		{"above threshold", docx.DocxOutlineParaThreshold + 1, docxReadArgs{}, true},
		{"above threshold but a range was asked for", docx.DocxOutlineParaThreshold + 1, docxReadArgs{StartPara: 3}, false},
		{"above threshold but a heading was asked for", docx.DocxOutlineParaThreshold + 1, docxReadArgs{Heading: "Intro"}, false},
		{"above threshold but full was asked for", docx.DocxOutlineParaThreshold + 1, docxReadArgs{Full: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldReturnOutline(tt.total, tt.opts); got != tt.want {
				t.Errorf("shouldReturnOutline(%d, %+v) = %v, want %v", tt.total, tt.opts, got, tt.want)
			}
		})
	}
}

// TestBuildDocxOutline_ConvertsSections pins finding 2 of the P1c review:
// buildDocxOutline and the outline serialization block had 0% test coverage
// because no fixture exceeds DocxOutlineParaThreshold (200 paragraphs). It
// is tested directly against a synthetic docx.Outline, per the review's
// instruction not to add a fixture.
func TestBuildDocxOutline_ConvertsSections(t *testing.T) {
	o := docx.Outline{
		TotalParas: 42,
		Words:      123,
		Sections: []docx.Section{
			{Heading: "Intro", Style: "Heading1", Level: 1, StartPara: 1, EndPara: 1, Paras: 1, Words: 1},
			{StartPara: 2, EndPara: 10, Paras: 9, Words: 60},
			{Heading: "Details", Style: "Heading2", Level: 2, StartPara: 11, EndPara: 42, Paras: 32, Words: 62},
		},
	}
	out := buildDocxOutline(o)
	if out.TotalParas != 42 || out.Words != 123 {
		t.Fatalf("TotalParas/Words = %d/%d, want 42/123", out.TotalParas, out.Words)
	}
	if len(out.Sections) != 3 {
		t.Fatalf("got %d sections, want 3", len(out.Sections))
	}
	if got := out.Sections[0]; got.Heading != "Intro" || got.Level != 1 || got.StartPara != 1 || got.EndPara != 1 || got.Paras != 1 || got.Words != 1 {
		t.Errorf("Sections[0] = %+v, want the Intro heading section", got)
	}
	if got := out.Sections[1]; got.Heading != "" || got.Level != 0 || got.StartPara != 2 || got.EndPara != 10 || got.Paras != 9 || got.Words != 60 {
		t.Errorf("Sections[1] = %+v, want the unnamed body section", got)
	}
	if got := out.Sections[2]; got.Heading != "Details" || got.Level != 2 {
		t.Errorf("Sections[2] = %+v, want the Details level-2 section", got)
	}
}

// TestMarshalDocxOutlineResult_FitsUnderCap is the ordinary-size half of
// finding 1/2 of the P1c review: a normal outline must marshal and pass
// through unchanged.
func TestMarshalDocxOutlineResult_FitsUnderCap(t *testing.T) {
	outline := docx.Outline{
		TotalParas: 250,
		Words:      2000,
		Sections: []docx.Section{
			{Heading: "Intro", Level: 1, StartPara: 1, EndPara: 20, Paras: 20, Words: 150},
			{Heading: "Body", Level: 1, StartPara: 21, EndPara: 250, Paras: 230, Words: 1850},
		},
	}
	payload, err := marshalDocxOutlineResult(outline, outline.TotalParas)
	if err != nil {
		t.Fatalf("marshalDocxOutlineResult: %v", err)
	}
	if len(payload) > maxDocxResultBytes {
		t.Fatalf("payload is %d bytes, over the %d cap", len(payload), maxDocxResultBytes)
	}
	var out docxReadOutput
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.Outline == nil || len(out.Outline.Sections) != 2 {
		t.Fatalf("Outline = %+v, want 2 sections", out.Outline)
	}
}

// TestMarshalDocxOutlineResult_ErrorsWhenOverCap pins finding 1 of the P1c
// review: the outline branch used to marshal and return directly with no
// size check at all, bypassing maxDocxResultBytes entirely — exactly the
// path every large document takes, since shouldReturnOutline only fires
// above 200 paragraphs. The chosen fix is an actionable error (not a
// level-truncated outline): an outline can't be shrunk by character budget
// the way a ranged read can, so this mirrors fitDocxReadResult's own
// "error rather than silently drop content" choice.
//
// The section count/heading length below are sized to reproduce the
// reviewer's measurement (500 real paragraphs with ~250 headings lands
// near 26 KB, already over the 20 KB cap) without needing a real fixture.
func TestMarshalDocxOutlineResult_ErrorsWhenOverCap(t *testing.T) {
	sections := make([]docx.Section, 1000)
	for i := range sections {
		sections[i] = docx.Section{
			Heading:   fmt.Sprintf("Section heading number %d with some realistic padding text", i+1),
			Style:     "Heading1",
			Level:     1,
			StartPara: i + 1,
			EndPara:   i + 1,
			Paras:     1,
			Words:     8,
		}
	}
	outline := docx.Outline{TotalParas: len(sections), Words: 8000, Sections: sections}

	_, err := marshalDocxOutlineResult(outline, outline.TotalParas)
	if err == nil {
		t.Fatal("an oversized outline returned nil error; it bypassed the size cap")
	}
	if !strings.Contains(err.Error(), "start_para") {
		t.Errorf("error = %q, want it to point at a recovery lever (start_para/end_para)", err)
	}
}

func TestDocxRead_DeclaresOmittedParts(t *testing.T) {
	p := docxFixture(t, "structure.docx")
	res, err := callDocxRead(t, map[string]any{"path": p, "start_para": float64(1), "end_para": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	notes, _ := decodeRead(t, res)["notes"].([]any)
	joined := ""
	for _, n := range notes {
		joined += n.(string) + " | "
	}
	if !strings.Contains(joined, "header") || !strings.Contains(joined, "footer") {
		t.Errorf("notes = %q, want the header/footer declaration", joined)
	}
}

// TestDocxRead_DeclaresPendingRevisions is task-3's I4 fix at the tool
// layer: docx_read on a document that already carries unreviewed
// w:ins/w:del (structure.docx, both authored "fixture") must declare that in
// notes, since the returned markdown otherwise shows w:ins content as
// ordinary text and w:delText not at all, with nothing telling the model the
// body it just read is not simply "the document's current text".
func TestDocxRead_DeclaresPendingRevisions(t *testing.T) {
	p := docxFixture(t, "structure.docx")
	res, err := callDocxRead(t, map[string]any{"path": p, "full": true})
	if err != nil {
		t.Fatal(err)
	}
	notes, _ := decodeRead(t, res)["notes"].([]any)
	joined := ""
	for _, n := range notes {
		joined += n.(string) + " | "
	}
	if !strings.Contains(joined, "fixture") {
		t.Errorf("notes = %q, want it to name the revision author (fixture)", joined)
	}
	if !strings.Contains(strings.ToLower(joined), "revision") && !strings.Contains(strings.ToLower(joined), "tracked change") {
		t.Errorf("notes = %q, want it to declare pending revisions", joined)
	}
}

// wordsDocFixture writes a synthetic .docx of n plain paragraphs, each
// containing w space-separated repetitions of "word", to a fresh file in
// t.TempDir() via docx.WriteDocx. It exists so full=true's over-budget
// fallback and the 8192-vs-16384 budget difference (below) can be exercised
// against a document whose rendered size is under caller control, rather
// than depending on a committed fixture happening to be the right size.
func wordsDocFixture(t *testing.T, n, w int) string {
	t.Helper()
	var md strings.Builder
	for i := 0; i < n; i++ {
		md.WriteString(strings.Repeat("word ", w))
		md.WriteString("\n\n")
	}
	path := filepath.Join(t.TempDir(), "words.docx")
	if _, err := docx.WriteDocx(path, docx.WriteOptions{Markdown: md.String()}); err != nil {
		t.Fatalf("WriteDocx: %v", err)
	}
	return path
}

// wholeMarkdownLen opens path and reads its entire rendered body (Full at an
// effectively unlimited budget), so a test can assert its fixture actually
// landed in the size bracket it needs instead of silently testing nothing
// (the same "test premise stale" discipline pkg/docx/read_test.go's
// TestRead_FullReturnsWholeRangeWithinBudget already follows).
func wholeMarkdownLen(t *testing.T, path string) int {
	t.Helper()
	doc, err := docx.OpenDocument(path)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	r, err := doc.Read(docx.ReadOptions{Full: true, MaxChars: 1 << 20})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return len(r.Markdown)
}

// TestDocxRead_FullOverInitialBudgetFallsBackToChunk pins the handler-layer
// half of the 2026-08-19 contract change (docs/DOCX_TOOLS_DESIGN.md §5):
// full=true against a document whose rendered body exceeds
// fullReadInitialBudget must come back completed — never an error — with
// next_start_para pointing at the resume point and a note explaining why
// only the first chunk came back. Before this change, this exact case
// returned a hard error naming Go-side identifiers (Document.Outline,
// StartPara/EndPara), which is what pushed a real model to abandon the tool
// and fall back to a bash + python script instead of retrying with a range.
func TestDocxRead_FullOverInitialBudgetFallsBackToChunk(t *testing.T) {
	p := wordsDocFixture(t, 400, 8)
	if whole := wholeMarkdownLen(t, p); whole <= fullReadInitialBudget {
		t.Fatalf("test premise stale: whole body is %d chars, want > fullReadInitialBudget (%d)", whole, fullReadInitialBudget)
	}

	res, err := callDocxRead(t, map[string]any{"path": p, "full": true})
	if err != nil {
		t.Fatalf("callDocxRead: %v", err)
	}
	if res.Status != models.CallStatusCompleted {
		t.Errorf("Status = %v, want completed", res.Status)
	}
	out := decodeRead(t, res)
	next, _ := out["next_start_para"].(float64)
	if next <= 0 {
		t.Errorf("next_start_para = %v, want > 0 (more of the document to read)", out["next_start_para"])
	}
	notes, _ := out["notes"].([]any)
	found := false
	for _, n := range notes {
		if s, ok := n.(string); ok && strings.Contains(s, "full read") && strings.Contains(s, "next_start_para") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %v, want a note explaining the full-read fallback and pointing at next_start_para", notes)
	}
}

// TestDocxRead_FullInitialBudgetIsBiggerThanChunkedBudget pins
// fullReadInitialBudget (16384) actually taking effect, not just existing as
// an unused constant: against a document whose rendered body sits strictly
// between docx.DefaultReadBudget (8192, the chunked path's budget) and
// fullReadInitialBudget (16384, full=true's), full=false must still chunk
// (next_start_para > 0) while full=true returns everything in one call
// (next_start_para == 0, AND the whole body actually came back). Both calls
// omit max_chars, so this is exactly the caller-observable difference the
// bigger initial budget exists to produce.
//
// Independent review (2026-08-19) caught two holes in an earlier version of
// this test: (1) next_start_para == 0 on its own is vacuous — a typo'd
// missing "path" key, or shouldReturnOutline's outline branch (which also
// never sets next_start_para), would pass the same assertion for entirely
// the wrong reason — so this version additionally asserts the paragraph
// count and markdown length actually match the whole document. (2) the test
// silently depended on 180 <= docx.DocxOutlineParaThreshold (if it were not,
// the full=false call would hit shouldReturnOutline and return an outline
// instead of a chunked read, invalidating "full=false must still chunk");
// this version checks that premise explicitly instead of assuming it.
func TestDocxRead_FullInitialBudgetIsBiggerThanChunkedBudget(t *testing.T) {
	const n = 180
	if n > docx.DocxOutlineParaThreshold {
		t.Fatalf("test premise stale: n=%d must be <= docx.DocxOutlineParaThreshold (%d), or full=false hits the outline branch instead of chunking",
			n, docx.DocxOutlineParaThreshold)
	}
	p := wordsDocFixture(t, n, 8)
	whole := wholeMarkdownLen(t, p)
	if whole <= docx.DefaultReadBudget || whole > fullReadInitialBudget {
		t.Fatalf("test premise stale: whole body is %d chars, want strictly between DefaultReadBudget (%d) and fullReadInitialBudget (%d)",
			whole, docx.DefaultReadBudget, fullReadInitialBudget)
	}

	chunkedRes, err := callDocxRead(t, map[string]any{"path": p})
	if err != nil {
		t.Fatalf("callDocxRead (chunked): %v", err)
	}
	chunked := decodeRead(t, chunkedRes)
	if next, _ := chunked["next_start_para"].(float64); next <= 0 {
		t.Errorf("full=false next_start_para = %v, want > 0 (body exceeds the %d-char chunked budget)", chunked["next_start_para"], docx.DefaultReadBudget)
	}

	fullRes, err := callDocxRead(t, map[string]any{"path": p, "full": true})
	if err != nil {
		t.Fatalf("callDocxRead (full): %v", err)
	}
	full := decodeRead(t, fullRes)
	if next, _ := full["next_start_para"].(float64); next != 0 {
		t.Errorf("full=true next_start_para = %v, want 0 (body fits within fullReadInitialBudget)", full["next_start_para"])
	}
	fullParas, _ := full["paragraphs"].([]any)
	if len(fullParas) != n {
		t.Errorf("full=true returned %d paragraphs, want all %d (next_start_para==0 alone does not prove the whole body came back)", len(fullParas), n)
	}
	fullMarkdown, _ := full["markdown"].(string)
	if len(fullMarkdown) != whole {
		t.Errorf("full=true markdown is %d chars, want the whole document's %d", len(fullMarkdown), whole)
	}
}

// TestDocxRead_FullWalksIncidentFixtureInFewRoundTrips measures the actual
// round-trip count fitDocxReadResult's proportional rescale (finding 2 of
// the independent 2026-08-19 review) produces on a fixture shaped like the
// real incident that motivated this whole change (docs/DOCX_TOOLS_DESIGN.md
// §5: 476 paragraphs, ~48 KB rendered). Before the rescale replaced a plain
// budget-halving, that reviewer measured full=true taking the SAME number of
// round trips on this shape as an ordinary chunked read at the 8192-char
// default: fullReadInitialBudget's 16384-char starting point bought nothing,
// because this tool's JSON wrapping inflates a character budget by roughly
// 1.35x, so one halving (16384 -> 8192 chars) landed the very first retry
// back below the 8192-char default it was supposed to improve on.
//
// This test pins the number actually observed after the fix (see the
// comment on wantRoundTrips) so that number — also recorded in
// fullReadInitialBudget's doc comment and DOCX_TOOLS_DESIGN.md §5 — cannot
// silently drift out of date if a future change to the JSON shape or the
// rescale formula changes how fast it converges.
func TestDocxRead_FullWalksIncidentFixtureInFewRoundTrips(t *testing.T) {
	const incidentParas = 476
	p := wordsDocFixture(t, incidentParas, 18)
	whole := wholeMarkdownLen(t, p)
	t.Logf("incident-shaped fixture: %d paragraphs render to %d chars", incidentParas, whole)

	roundTrips := 0
	startPara := 0
	for {
		roundTrips++
		if roundTrips > 20 {
			t.Fatalf("round trips did not converge to next_start_para==0 within 20 calls")
		}
		args := map[string]any{"path": p, "full": true}
		if startPara > 0 {
			args["start_para"] = float64(startPara)
		}
		res, err := callDocxRead(t, args)
		if err != nil {
			t.Fatalf("callDocxRead (round trip %d): %v", roundTrips, err)
		}
		out := decodeRead(t, res)
		next, _ := out["next_start_para"].(float64)
		if next == 0 {
			break
		}
		startPara = int(next)
	}

	// Measured on this codebase after the finding-2 rescale fix; update this
	// alongside fullReadInitialBudget's doc comment and
	// DOCX_TOOLS_DESIGN.md §5 if it ever changes.
	const wantRoundTrips = 4
	if roundTrips != wantRoundTrips {
		t.Errorf("round trips = %d, want %d (update this test and the doc comments it is cited from if the change is intentional)",
			roundTrips, wantRoundTrips)
	}
}

// TestDocxRead_SingleOversizedParagraphErrorAdvisesAgainstMaxCharsOrRange is
// an end-to-end regression test for a hole a second independent review round
// (2026-08-19) found in TestDocxRead_FitResultErrorCarriesDiagnostic: that
// test only ever exercised fitDocxReadResult with a STUBBED note, so a
// change to pkg/docx's real note wording (or to the DocxReadHandler/
// pkg/docx wiring between them) could silently regress the
// hasOversizedParagraphNote advice branch to the generic, wrong "retry with
// a smaller max_chars" advice while every existing test kept passing. This
// pushes one real oversized paragraph through the actual handler, with
// runs=false, and pins the specific advice text that branch is supposed to
// produce.
func TestDocxRead_SingleOversizedParagraphErrorAdvisesAgainstMaxCharsOrRange(t *testing.T) {
	// A single paragraph of 5000 repetitions of "word " (25000 chars) plus
	// its "[para 1] "/"\n\n" framing renders to roughly 25010 chars — well
	// over both docx.DefaultReadBudget (8192) and maxDocxResultBytes (20480
	// bytes), and returned whole regardless of budget (see pkg/docx.
	// readChunkedResult), so no max_chars or range narrows it.
	p := wordsDocFixture(t, 1, 5000)
	if whole := wholeMarkdownLen(t, p); whole <= maxDocxResultBytes {
		t.Fatalf("test premise stale: the single paragraph is only %d chars, want it bigger than the %d-byte cap", whole, maxDocxResultBytes)
	}

	_, err := callDocxRead(t, map[string]any{"path": p, "runs": false})
	if err == nil {
		t.Fatal("a document with one paragraph over the result cap returned nil error")
	}
	if !strings.Contains(err.Error(), "no max_chars value or narrower range fixes this") {
		t.Errorf("error = %q, want the oversized-single-paragraph advice, not the generic \"retry with a smaller max_chars\" one", err)
	}
	if strings.Contains(err.Error(), "retry with a smaller max_chars, or narrow the range") {
		t.Errorf("error = %q, want it to NOT fall back to the generic (wrong, for this case) advice", err)
	}
}

// TestDocxRead_HugeMaxCharsIsClampedToTheByteCap is finding 5 of the second
// independent review round (2026-08-19): before fitDocxReadResult clamped
// its incoming budget to maxDocxResultBytes, a caller-supplied max_chars far
// bigger than the byte cap (e.g. 100000000, whether by mistake or an
// attempt to force a whole-document read) made full=true's own over-budget
// check (pkg/docx.Document.Read) never trip — the whole document rendered
// well within such a huge character budget — so the ONLY thing that could
// shrink the resulting oversized JSON payload was fitDocxReadResult's
// rescale, starting from a budget so much bigger than the document that
// every one of maxDocxFitAttempts rescales still left it bigger than the
// document's actual size; the call hard-errored on a document a normal
// chunked read walks in a handful of calls. Clamping the incoming budget
// fixes this: the very first attempt is already at or under the byte cap,
// so Full's own fallback (pkg/docx.Document.Read) trips immediately and the
// call completes as an ordinary chunked read instead of erroring.
func TestDocxRead_HugeMaxCharsIsClampedToTheByteCap(t *testing.T) {
	p := wordsDocFixture(t, 476, 18) // the same incident-shaped fixture as TestDocxRead_FullWalksIncidentFixtureInFewRoundTrips
	if whole := wholeMarkdownLen(t, p); whole <= maxDocxResultBytes {
		t.Fatalf("test premise stale: whole body is %d chars, want it bigger than the %d-byte cap", whole, maxDocxResultBytes)
	}

	res, err := callDocxRead(t, map[string]any{"path": p, "full": true, "max_chars": float64(100_000_000)})
	if err != nil {
		t.Fatalf("callDocxRead: %v (huge max_chars should clamp and chunk, not error)", err)
	}
	if res.Status != models.CallStatusCompleted {
		t.Errorf("Status = %v, want completed", res.Status)
	}
	if len(res.Content) > maxDocxResultBytes {
		t.Errorf("result is %d bytes, over the %d cap", len(res.Content), maxDocxResultBytes)
	}
	out := decodeRead(t, res)
	if next, _ := out["next_start_para"].(float64); next <= 0 {
		t.Errorf("next_start_para = %v, want > 0 (a document this large cannot come back in one call even with a huge max_chars)", out["next_start_para"])
	}
}

func TestDocxRead_UnknownHeadingErrors(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	if _, err := callDocxRead(t, map[string]any{"path": p, "heading": "No Such Heading"}); err == nil {
		t.Fatal("unknown heading returned nil error")
	}
}

func TestDocxRead_RejectsNonDocx(t *testing.T) {
	p := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(p, []byte("plain text"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := callDocxRead(t, map[string]any{"path": p}); err == nil {
		t.Fatal("a non-docx file returned nil error")
	}
}

// TestDocxRead_RejectsWrongTypedOptionalArgs pins finding 4 of the P1c
// review: heading/full/runs used a bare ", ok" type assertion that
// silently coerced a wrong-typed value to its zero value instead of
// erroring, exactly the asymmetry the review calls out against find
// (already type-checked, already errors on a non-string).
func TestDocxRead_RejectsWrongTypedOptionalArgs(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	tests := []struct {
		name string
		args map[string]any
	}{
		{"heading not a string", map[string]any{"path": p, "heading": float64(123)}},
		{"full not a bool", map[string]any{"path": p, "full": "true"}},
		{"runs not a bool", map[string]any{"path": p, "runs": "true"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := callDocxRead(t, tt.args); err == nil {
				t.Fatal("a wrong-typed argument returned nil error")
			}
		})
	}
}

func callDocxEdit(t *testing.T, args map[string]any) (models.ToolResult, error) {
	t.Helper()
	return DocxEditHandler(context.Background(), models.ToolCall{
		ID: "e1", Name: "docx_edit", Arguments: args,
	})
}

func TestDocxEdit_AppliesAFindReplace(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxEdit(t, map[string]any{
		"path":  p,
		"edits": []any{map[string]any{"para": float64(2), "find": "Body", "text": "BODY"}},
	})
	if err != nil {
		t.Fatalf("DocxEditHandler: %v", err)
	}
	out := decodeRead(t, res)
	if out["applied"] != float64(1) {
		t.Fatalf("applied = %v, want 1; content=%s", out["applied"], res.Content)
	}
	after, err := callDocxRead(t, map[string]any{"path": p, "start_para": float64(2), "end_para": float64(2)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(decodeRead(t, after)["markdown"].(string), "BODY") {
		t.Error("the edit did not reach the file")
	}
}

// TestDocxEdit_StyleIsRejectedNotIgnored pins design §4.2's note: style is
// deferred to P2, and silently dropping it would let the model believe it
// restyled a paragraph.
// TestDocxEdit_StyleIsRejectedNotIgnored pins Task 13's I1 fix: the error
// used to point the caller at docx_format ("use docx_format for paragraph
// styling"), but docx.FormatOptions has no field for a paragraph style at
// all, so that advice sent a caller to a tool that cannot do it either, with
// no signal it was a dead end until the very next call also failed. The
// error must instead say plainly that no docx tool can do this yet, and
// that the paragraph keeps its current style.
func TestDocxEdit_StyleIsRejectedNotIgnored(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	_, err := callDocxEdit(t, map[string]any{
		"path":  p,
		"edits": []any{map[string]any{"para": float64(2), "text": "x", "style": "Heading2"}},
	})
	if err == nil {
		t.Fatal("style was accepted; want an explicit error")
	}
	if strings.Contains(err.Error(), "docx_format") {
		t.Errorf("error = %q, want it to no longer point at docx_format (which cannot set paragraph styles either)", err)
	}
	if !strings.Contains(err.Error(), "cannot be set by any docx tool") {
		t.Errorf("error = %q, want it to say plainly that no docx tool can set a paragraph style yet", err)
	}
	if !strings.Contains(err.Error(), "current style") {
		t.Errorf("error = %q, want it to say the paragraph keeps its current style", err)
	}
}

// TestDocxEdit_RejectsWrongTypedTextAndOp pins finding 4 of the P1c review:
// text/op used a bare ", _ :=" type assertion that silently coerced a
// wrong-typed value to "" instead of erroring. The motivating case from the
// review is an edit sent as {"para":5,"find":"2025","text":2026} — a model
// emitting a bare number for a string field — which used to become
// Text: "", apply as a replace, and delete the matched text while
// reporting applied:true with no diagnostic at all.
func TestDocxEdit_RejectsWrongTypedTextAndOp(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	tests := []struct {
		name string
		edit map[string]any
	}{
		{"text not a string", map[string]any{"para": float64(2), "find": "Body", "text": float64(2026)}},
		{"op not a string", map[string]any{"para": float64(2), "text": "x", "op": float64(1)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := callDocxEdit(t, map[string]any{"path": p, "edits": []any{tt.edit}}); err == nil {
				t.Fatal("a wrong-typed argument returned nil error")
			}
			after, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(original, after) {
				t.Error("the rejected edit still modified the file on disk")
			}
		})
	}
}

// TestDocxTools_AreInTheDocumentGroup pins finding 13 of the P1c review:
// the subagent tool-selector test builds its own []models.Tool with
// hand-written Groups, so it would stay green even if Groups were deleted
// from the real docx tool definitions. This pins Groups directly on the
// tools DocxTools() returns.
func TestDocxTools_AreInTheDocumentGroup(t *testing.T) {
	for _, tool := range []models.Tool{DocxReadTool(), DocxEditTool()} {
		found := false
		for _, g := range tool.Groups {
			if g == "document" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s.Groups = %v, want it to contain %q", tool.Name, tool.Groups, "document")
		}
	}
}

// TestDocxEdit_BacksUpOnceBeforeTheFirstOverwrite pins §8. The second edit
// must NOT refresh the backup, or a half-finished polish run would overwrite
// the pristine original.
func TestDocxEdit_BacksUpOnceBeforeTheFirstOverwrite(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	original, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	edit := func(text string) {
		t.Helper()
		if _, err := callDocxEdit(t, map[string]any{
			"path":  p,
			"edits": []any{map[string]any{"para": float64(2), "text": text}},
		}); err != nil {
			t.Fatalf("edit %q: %v", text, err)
		}
	}
	edit("first")
	backup := p + ".bak"
	first, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("no backup after the first edit: %v", err)
	}
	if !bytes.Equal(first, original) {
		t.Error("the backup is not the pristine original")
	}

	edit("second")
	second, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(second, original) {
		t.Error("the second edit refreshed the backup; it must be written once")
	}
}

// TestDocxEdit_ReportsBackupCreated pins finding 8 of the P1c review:
// backup_path alone can't tell a backup this call just created apart from
// one that already existed from an earlier session, so a user told
// "backup_path is your rollback path" on the second call would be misled
// into thinking they could roll back to what they just had, when they'd
// actually roll back past an earlier accepted run.
func TestDocxEdit_ReportsBackupCreated(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	edit := func(text string) map[string]any {
		t.Helper()
		res, err := callDocxEdit(t, map[string]any{
			"path":  p,
			"edits": []any{map[string]any{"para": float64(2), "text": text}},
		})
		if err != nil {
			t.Fatalf("edit %q: %v", text, err)
		}
		return decodeRead(t, res)
	}

	first := edit("first")
	if first["backup_created"] != true {
		t.Errorf("backup_created = %v on the first backup-creating edit, want true", first["backup_created"])
	}

	second := edit("second")
	if second["backup_created"] != false {
		t.Errorf("backup_created = %v on the second edit, want false (the backup already existed)", second["backup_created"])
	}
	if second["backup_path"] != first["backup_path"] {
		t.Errorf("backup_path changed between calls: %v vs %v", first["backup_path"], second["backup_path"])
	}
}

// TestDocxEdit_StaleBackupWarnsOnPreExisting pins Task 13's I3 fix: on the
// second call, backup_created comes back false because backupDocxOnce found
// the .bak from the first call still sitting there — but that same check
// only ever looks at whether a file EXISTS at <path>.bak, never whether it
// actually belongs to the document currently open at path. A caller who
// only reads backup_created/backup_path has no way to tell "this is safe to
// roll back to" apart from "this is a stale backup, possibly of a
// completely different document" — so notes must carry an explicit warning
// whenever backup_created is false but a backup IS in play.
func TestDocxEdit_StaleBackupWarnsOnPreExisting(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	edit := func(text string) map[string]any {
		t.Helper()
		res, err := callDocxEdit(t, map[string]any{
			"path":  p,
			"edits": []any{map[string]any{"para": float64(2), "text": text}},
		})
		if err != nil {
			t.Fatalf("edit %q: %v", text, err)
		}
		return decodeRead(t, res)
	}

	first := edit("first")
	if notes, _ := first["notes"].([]any); len(notes) != 0 {
		t.Errorf("notes on the first (backup-creating) edit = %v, want none", notes)
	}

	second := edit("second")
	notes, _ := second["notes"].([]any)
	joined := fmt.Sprint(notes)
	if !strings.Contains(joined, "pre-existing backup") {
		t.Errorf("notes = %v, want a warning that the .bak pre-existed and should be verified before rollback", notes)
	}
}

// TestDocxEdit_RefusesNonRegularBackupPath pins finding 7 of the P1c
// review: os.Stat succeeding on <path>.bak was treated as "a valid backup
// exists", so a directory left at that path would make Save() overwrite
// the original with no backup at all.
func TestDocxEdit_RefusesNonRegularBackupPath(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	original, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p+".bak", 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := callDocxEdit(t, map[string]any{
		"path":  p,
		"edits": []any{map[string]any{"para": float64(2), "text": "x"}},
	}); err == nil {
		t.Fatal("a directory at <path>.bak was accepted as a valid backup")
	}

	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Error("the document was overwritten even though no valid backup could be made")
	}
}

// TestDocxEdit_RefusesSymlinkBackupPath pins the other half of finding 7:
// os.Stat follows symlinks, so a dangling <path>.bak symlink would make
// Stat report ENOENT (as if no backup existed) and the subsequent
// os.WriteFile would then create the document's bytes at wherever the
// symlink points — an attacker-controlled path if the symlink was planted
// there.
func TestDocxEdit_RefusesSymlinkBackupPath(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	target := filepath.Join(t.TempDir(), "elsewhere.txt")
	if err := os.Symlink(target, p+".bak"); err != nil {
		t.Skipf("symlinks not supported in this environment: %v", err)
	}

	if _, err := callDocxEdit(t, map[string]any{
		"path":  p,
		"edits": []any{map[string]any{"para": float64(2), "text": "x"}},
	}); err == nil {
		t.Fatal("a dangling symlink at <path>.bak was accepted as a valid backup")
	}

	if _, statErr := os.Lstat(target); !os.IsNotExist(statErr) {
		t.Error("the document's bytes were written through the dangling symlink")
	}
}

func TestDocxEdit_ReportsBeforeAndAfterPerEdit(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxEdit(t, map[string]any{
		"path":  p,
		"edits": []any{map[string]any{"para": float64(2), "find": "Body", "text": "BODY"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	outcomes, _ := decodeRead(t, res)["outcomes"].([]any)
	if len(outcomes) != 1 {
		t.Fatalf("got %d outcomes, want 1", len(outcomes))
	}
	o, _ := outcomes[0].(map[string]any)
	if o["before"] != "Body" || o["after"] != "BODY" {
		t.Errorf("before/after = %v/%v, want Body/BODY", o["before"], o["after"])
	}
}

// TestDocxEdit_SignalsParagraphIndexShift pins §5.4: after an insert or
// delete the caller's indices are stale, and nothing else tells it so.
func TestDocxEdit_SignalsParagraphIndexShift(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxEdit(t, map[string]any{
		"path":  p,
		"edits": []any{map[string]any{"para": float64(2), "op": "insert_after", "text": "NEW"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := decodeRead(t, res)
	if out["para_count_changed"] != true {
		t.Error("para_count_changed = false after an insert")
	}
	if out["index_advice"] == nil || out["index_advice"] == "" {
		t.Error("no advice telling the caller its paragraph indices are stale")
	}
}

// TestDocxEdit_NoIndexAdviceWhenParaCountUnchanged is a supplement to the
// brief's TestDocxEdit_SignalsParagraphIndexShift, which only pins the
// advice-present half of §5.4. Without this test, always emitting
// index_advice (regardless of ParaCountChanged) would pass every test in
// this file while wasting a docx_read round trip on every ordinary polish
// batch that never inserts or deletes a paragraph.
func TestDocxEdit_NoIndexAdviceWhenParaCountUnchanged(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxEdit(t, map[string]any{
		"path":  p,
		"edits": []any{map[string]any{"para": float64(2), "find": "Body", "text": "BODY"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := decodeRead(t, res)
	if out["para_count_changed"] == true {
		t.Fatal("a plain replace changed para_count_changed; this test needs an edit that does not")
	}
	if advice, ok := out["index_advice"]; ok && advice != "" {
		t.Errorf("index_advice = %v, want none when the paragraph count did not change", advice)
	}
}

func TestDocxEdit_EchoesReviewedThroughPara(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxEdit(t, map[string]any{
		"path":                  p,
		"reviewed_through_para": float64(12),
		"edits":                 []any{map[string]any{"para": float64(2), "find": "Body", "text": "B"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if decodeRead(t, res)["reviewed_through_para"] != float64(12) {
		t.Error("reviewed_through_para was not echoed back")
	}
}

func TestDocxEdit_RefusesDocumentWithExistingRevisions(t *testing.T) {
	p := docxFixture(t, "structure.docx") // contains w:ins / w:del
	_, err := callDocxEdit(t, map[string]any{
		"path":  p,
		"edits": []any{map[string]any{"para": float64(1), "text": "x"}},
	})
	if err == nil {
		t.Fatal("editing a document with revision marks returned nil error")
	}
}

func TestDocxEdit_PerEditRefusalDoesNotFailTheCall(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxEdit(t, map[string]any{
		"path": p,
		"edits": []any{
			map[string]any{"para": float64(2), "find": "no such text", "text": "x"},
			map[string]any{"para": float64(3), "find": "Body", "text": "OK"},
		},
	})
	if err != nil {
		t.Fatalf("a per-edit refusal must not fail the whole call: %v", err)
	}
	out := decodeRead(t, res)
	if out["applied"] != float64(1) {
		t.Errorf("applied = %v, want 1", out["applied"])
	}
}

// TestDocxEdit_RejectsWrongTypedTrackChangesAndAuthor pins the same
// never-coerce rule the P1c review established for every other docx_edit
// argument (finding 4, see TestDocxEdit_RejectsWrongTypedTextAndOp): a
// wrong-typed track_changes or author must error, not silently discard the
// value as false/"" while the call still applies edits and reports success.
func TestDocxEdit_RejectsWrongTypedTrackChangesAndAuthor(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
	}{
		{"track_changes not a bool", map[string]any{
			"track_changes": "true",
		}},
		{"author not a string", map[string]any{
			"author": float64(1),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := docxFixture(t, "outline.docx")
			original, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			args := map[string]any{
				"path":  p,
				"edits": []any{map[string]any{"para": float64(2), "find": "Body", "text": "BODY"}},
			}
			for k, v := range tt.args {
				args[k] = v
			}
			if _, err := callDocxEdit(t, args); err == nil {
				t.Fatal("a wrong-typed argument returned nil error")
			}
			after, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(original, after) {
				t.Error("the rejected call still modified the file on disk")
			}
		})
	}
}

// TestDocxEdit_TrackChangesDefaultsToFalseAndIsEchoed pins that an ordinary
// docx_edit call (no track_changes given) behaves as untracked AND reports
// that truthfully in the result: a model must be able to tell "applied
// directly" from "pending review" by reading the response, not by assuming
// from field absence.
func TestDocxEdit_TrackChangesDefaultsToFalseAndIsEchoed(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxEdit(t, map[string]any{
		"path":  p,
		"edits": []any{map[string]any{"para": float64(2), "find": "Body", "text": "BODY"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := decodeRead(t, res)
	if v, ok := out["track_changes"]; !ok || v != false {
		t.Errorf("track_changes = %v (present=%v), want false", v, ok)
	}
	xml := readDocumentXML(t, p)
	if strings.Contains(xml, "<w:ins") || strings.Contains(xml, "<w:del") {
		t.Error("track_changes defaulted false but the document still contains revision marks")
	}
}

// TestDocxEdit_TrackChangesTrueProducesRevisionsAndIsEchoed is this task's
// core positive case: track_changes: true must both (a) land in the written
// document as real w:ins/w:del markup stamped with the given author, and (b)
// come back in the result so the model can truthfully tell the user the
// change is pending review in Word, not already applied. It also pins §4.2's
// contract that TrackChanges changes only the produced bytes, never the
// semantic Before/After fields an untracked edit would report.
func TestDocxEdit_TrackChangesTrueProducesRevisionsAndIsEchoed(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxEdit(t, map[string]any{
		"path":          p,
		"track_changes": true,
		"author":        "Alice",
		"edits":         []any{map[string]any{"para": float64(2), "find": "Body", "text": "BODY"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := decodeRead(t, res)
	if v, ok := out["track_changes"]; !ok || v != true {
		t.Errorf("track_changes = %v (present=%v), want true", v, ok)
	}
	xml := readDocumentXML(t, p)
	if !strings.Contains(xml, "<w:ins") || !strings.Contains(xml, "<w:del") {
		t.Errorf("track_changes:true did not produce w:ins/w:del markup: %s", xml)
	}
	if !strings.Contains(xml, `w:author="Alice"`) {
		t.Errorf("author was not stamped on the revision: %s", xml)
	}
	outcomes, ok := out["outcomes"].([]any)
	if !ok || len(outcomes) == 0 {
		t.Fatalf("outcomes missing or empty: %v", out["outcomes"])
	}
	first := outcomes[0].(map[string]any)
	if first["before"] != "Body" || first["after"] != "BODY" {
		t.Errorf("before/after = %v/%v, want Body/BODY unchanged by track_changes", first["before"], first["after"])
	}
}

// TestDocxEdit_ThreeConsecutiveCallsOnSamePathAllSucceed is the tool-layer
// reproduction of task-3's C1 defect: DocxEditHandler calls
// docx.OpenDocument fresh on every invocation (there is no long-lived
// *Document across calls the way pkg/docx's own tests can hold one), so the
// old hadRevisionsAtOpen-is-a-bool gate saw its OWN first tracked edit's
// w:ins/w:del on every later OpenDocument and refused the entire batch —
// including an ordinary, untracked edit — from the second docx_edit call
// onward. That is exactly the shape the document-editor profile's
// chunked-polish workflow (track_changes:true, one docx_edit per chunk)
// hits on its very second call. The fix is author-based: this batch's own
// earlier revisions (same default "deepai" author both calls use) must
// never trigger the refusal. Three real, independent handler calls on the
// same path — not the same in-memory Document — are required to prove it;
// pkg/docx/edit_test.go's own multi-edit tests reuse one *Document and so
// cannot catch this (see that file's TestEdit_TrackChanges_
// TwoConsecutiveChunkedEditsBothSucceed for the now-fixed false positive).
func TestDocxEdit_ThreeConsecutiveCallsOnSamePathAllSucceed(t *testing.T) {
	p := docxFixture(t, "outline.docx")

	res1, err := callDocxEdit(t, map[string]any{
		"path":          p,
		"track_changes": true,
		"edits":         []any{map[string]any{"para": float64(2), "find": "Body", "text": "BODY-1"}},
	})
	if err != nil {
		t.Fatalf("first docx_edit call: %v", err)
	}
	if out := decodeRead(t, res1); out["applied"] != float64(1) {
		t.Fatalf("first call: applied = %v, want 1; content=%s", out["applied"], res1.Content)
	}

	res2, err := callDocxEdit(t, map[string]any{
		"path":          p,
		"track_changes": true,
		"edits":         []any{map[string]any{"para": float64(3), "find": "Body", "text": "BODY-2"}},
	})
	if err != nil {
		t.Fatalf("second docx_edit call on the same path returned an error — this is the C1 regression: %v", err)
	}
	if out := decodeRead(t, res2); out["applied"] != float64(1) {
		t.Fatalf("second call: applied = %v, want 1; content=%s", out["applied"], res2.Content)
	}

	res3, err := callDocxEdit(t, map[string]any{
		"path":          p,
		"track_changes": false,
		"edits":         []any{map[string]any{"para": float64(6), "find": "Body", "text": "BODY-3"}},
	})
	if err != nil {
		t.Fatalf("third docx_edit call (track_changes off) on the same path returned an error: %v", err)
	}
	if out := decodeRead(t, res3); out["applied"] != float64(1) {
		t.Fatalf("third call: applied = %v, want 1; content=%s", out["applied"], res3.Content)
	}

	after, err := callDocxRead(t, map[string]any{"path": p, "start_para": float64(2), "end_para": float64(6)})
	if err != nil {
		t.Fatalf("callDocxRead: %v", err)
	}
	md, _ := decodeRead(t, after)["markdown"].(string)
	for _, want := range []string{"BODY-1", "BODY-2", "BODY-3"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown = %q, want it to contain %q", md, want)
		}
	}
}

// TestDocxEdit_AuthorEmptyOrWhitespaceDefaultsToDeepai pins the self-review
// question the brief raises explicitly: an author that is omitted, "", or
// whitespace-only must all land as the identical "deepai" default in the
// actual w:author attribute — not as literal whitespace Word would show in
// its review pane, and not as three different outcomes.
func TestDocxEdit_AuthorEmptyOrWhitespaceDefaultsToDeepai(t *testing.T) {
	cases := []struct {
		name    string
		author  any
		present bool
	}{
		{"omitted", nil, false},
		{"empty string", "", true},
		{"whitespace only", "   ", true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			p := docxFixture(t, "outline.docx")
			args := map[string]any{
				"path":          p,
				"track_changes": true,
				"edits":         []any{map[string]any{"para": float64(2), "find": "Body", "text": "BODY"}},
			}
			if tt.present {
				args["author"] = tt.author
			}
			if _, err := callDocxEdit(t, args); err != nil {
				t.Fatal(err)
			}
			xml := readDocumentXML(t, p)
			if !strings.Contains(xml, `w:author="deepai"`) {
				t.Errorf("author %q did not default to deepai: %s", tt.author, xml)
			}
		})
	}
}

// TestDocxEdit_TrackChangesFalseIsByteIdenticalToOmitted guards the tool
// layer's own wiring, mirroring pkg/docx's
// TestEdit_TrackChangesOffIsByteIdenticalToBeforeThisFeature one level up:
// an explicit track_changes: false must produce byte-identical output to
// omitting the field entirely, so adding this argument cannot have changed
// what an existing, unaware caller gets.
func TestDocxEdit_TrackChangesFalseIsByteIdenticalToOmitted(t *testing.T) {
	p1 := docxFixture(t, "outline.docx")
	p2 := docxFixture(t, "outline.docx")
	edits := []any{map[string]any{"para": float64(2), "find": "Body", "text": "BODY"}}

	if _, err := callDocxEdit(t, map[string]any{"path": p1, "edits": edits}); err != nil {
		t.Fatal(err)
	}
	if _, err := callDocxEdit(t, map[string]any{"path": p2, "track_changes": false, "edits": edits}); err != nil {
		t.Fatal(err)
	}

	x1, err := os.ReadFile(p1)
	if err != nil {
		t.Fatal(err)
	}
	x2, err := os.ReadFile(p2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(x1, x2) {
		t.Error("track_changes: false produced different bytes than omitting track_changes entirely")
	}
}

// bodyDocxFixture builds a minimal .docx (just [Content_Types].xml and
// word/document.xml, no styles.xml -- mirroring pkg/docx's own internal
// bodyDoc test helper, which this package cannot import since it is
// unexported) whose document.xml body is exactly bodyXML, and returns its
// path. Used by tests that need a specific paragraph shape (e.g. consecutive
// empty paragraphs for normalize) that the committed testdata fixtures do
// not happen to contain.
func bodyDocxFixture(t *testing.T, bodyXML string) string {
	t.Helper()
	docXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:body>` + bodyXML + `</w:body></w:document>`
	p := filepath.Join(t.TempDir(), "synthetic.docx")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entries := []struct{ name, content string }{
		{"[Content_Types].xml", `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`},
		{docx.DocumentPart, docXML},
	}
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// callDocxFormat is docx_format's test-only entry point, mirroring
// callDocxRead/callDocxEdit.
func callDocxFormat(t *testing.T, args map[string]any) (models.ToolResult, error) {
	t.Helper()
	return DocxFormatHandler(context.Background(), models.ToolCall{
		ID: "f1", Name: "docx_format", Arguments: args,
	})
}

func TestDocxFormat_RequiresPath(t *testing.T) {
	if _, err := callDocxFormat(t, map[string]any{}); err == nil {
		t.Fatal("missing path returned nil error")
	}
}

// TestDocxFormat_EmptyRulesIsANoOpWithClearNote pins the task brief's
// self-review question directly: an empty rules object must be
// distinguishable from a call that actually changed something, not a
// silent no-op that looks identical to success. It must also not write the
// file or create a backup, since pkg/docx's Document.Modified() stays false
// when nothing was requested.
func TestDocxFormat_EmptyRulesIsANoOpWithClearNote(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	original, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}

	res, err := callDocxFormat(t, map[string]any{"path": p, "rules": map[string]any{}})
	if err != nil {
		t.Fatalf("DocxFormatHandler: %v", err)
	}
	out := decodeRead(t, res)

	applied, ok := out["applied"].([]any)
	if !ok {
		t.Fatalf("applied is not an array: %v (content=%s)", out["applied"], res.Content)
	}
	if len(applied) != 0 {
		t.Errorf("applied = %v, want empty for an empty rules object", applied)
	}
	notes, _ := out["notes"].([]any)
	if len(notes) == 0 {
		t.Error("notes is empty; the caller cannot tell an empty rules object did nothing versus a real no-op bug")
	}
	if out["backup_path"] != nil && out["backup_path"] != "" {
		t.Errorf("backup_path = %v, want empty; nothing was modified so no backup should be made", out["backup_path"])
	}
	if out["backup_created"] != false {
		t.Errorf("backup_created = %v, want false", out["backup_created"])
	}

	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Error("an empty rules object modified the file on disk")
	}
	if _, err := os.Stat(p + ".bak"); !os.IsNotExist(err) {
		t.Error("an empty rules object created a backup file")
	}
}

// TestDocxFormat_RulesKeyIsOptional pins that omitting "rules" entirely
// behaves exactly like passing rules={} — both mean "nothing requested",
// not an error.
func TestDocxFormat_RulesKeyIsOptional(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxFormat(t, map[string]any{"path": p})
	if err != nil {
		t.Fatalf("DocxFormatHandler with no rules key: %v", err)
	}
	out := decodeRead(t, res)
	applied, _ := out["applied"].([]any)
	if len(applied) != 0 {
		t.Errorf("applied = %v, want empty when rules is omitted entirely", applied)
	}
}

// TestDocxFormat_AppliesBodyFontAndReportsApplied pins the "report what
// changed" responsibility: the tool must surface pkg/docx's Applied list,
// not just report success.
func TestDocxFormat_AppliesBodyFontAndReportsApplied(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxFormat(t, map[string]any{
		"path":  p,
		"rules": map[string]any{"body_font": "Georgia"},
	})
	if err != nil {
		t.Fatalf("DocxFormatHandler: %v", err)
	}
	out := decodeRead(t, res)
	applied, _ := out["applied"].([]any)
	if len(applied) == 0 {
		t.Fatalf("applied is empty; want it to report the body font change (content=%s)", res.Content)
	}
	found := false
	for _, a := range applied {
		if s, ok := a.(string); ok && strings.Contains(s, "Georgia") {
			found = true
		}
	}
	if !found {
		t.Errorf("applied = %v, want an entry mentioning Georgia", applied)
	}
	if out["backup_created"] != true {
		t.Errorf("backup_created = %v, want true on the first modifying call", out["backup_created"])
	}
	if out["backup_path"] == nil || out["backup_path"] == "" {
		t.Error("backup_path is empty after a modifying call")
	}
}

// TestDocxFormat_SecondIdenticalCallReportsNoChangeNote is the tool-layer
// half of pkg/docx's F1 fix (task 7 follow-up review): calling docx_format
// twice with the exact same body_font rule must report an empty "applied"
// and the same "no formatting changes were applied" note an entirely empty
// rules object gets (docxFormatNoChangeNote), on the second call — the
// first call already made docDefaults (and every shadowing style) carry the
// requested font, so there is nothing left for the second call to change.
func TestDocxFormat_SecondIdenticalCallReportsNoChangeNote(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	args := map[string]any{"path": p, "rules": map[string]any{"body_font": "Georgia"}}

	first, err := callDocxFormat(t, args)
	if err != nil {
		t.Fatalf("first DocxFormatHandler: %v", err)
	}
	firstOut := decodeRead(t, first)
	if applied, _ := firstOut["applied"].([]any); len(applied) == 0 {
		t.Fatalf("first call's applied is empty; the rule never took effect (content=%s)", first.Content)
	}

	second, err := callDocxFormat(t, args)
	if err != nil {
		t.Fatalf("second DocxFormatHandler: %v", err)
	}
	secondOut := decodeRead(t, second)
	applied, _ := secondOut["applied"].([]any)
	if len(applied) != 0 {
		t.Errorf("second identical call's applied = %v, want empty (content=%s)", applied, second.Content)
	}
	notes, _ := secondOut["notes"].([]any)
	found := false
	for _, n := range notes {
		if s, ok := n.(string); ok && strings.Contains(s, "no formatting changes were applied") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %v, want the no-change note on a second identical call", notes)
	}
}

// TestDocxFormat_NormalizeReportsParaCountChangedAndIndexAdvice pins task 10
// brief item 1 / seams review C2: normalize deletes paragraphs, which shifts
// every later paragraph's index the same way an insert/delete batch through
// docx_edit does. docx_format must report this the same way docx_edit
// already does (total_paras/para_count_changed/index_advice -- the same
// docxIndexAdvice constant, builtin/docx.go:443), or a caller that read
// paragraph indices before this call and edits by index afterward silently
// targets the wrong paragraph.
func TestDocxFormat_NormalizeReportsParaCountChangedAndIndexAdvice(t *testing.T) {
	p := bodyDocxFixture(t, `<w:p><w:r><w:t>one</w:t></w:r></w:p><w:p/><w:p/><w:p/><w:p><w:r><w:t>two</w:t></w:r></w:p>`)
	res, err := callDocxFormat(t, map[string]any{"path": p, "rules": map[string]any{"normalize": true}})
	if err != nil {
		t.Fatalf("DocxFormatHandler: %v", err)
	}
	out := decodeRead(t, res)

	totalParas, ok := out["total_paras"].(float64)
	if !ok {
		t.Fatalf("total_paras missing or not a number: %v (content=%s)", out["total_paras"], res.Content)
	}
	if totalParas != 3 {
		t.Errorf("total_paras = %v, want 3 (three empties collapsed to one)", totalParas)
	}
	if out["para_count_changed"] != true {
		t.Errorf("para_count_changed = %v, want true", out["para_count_changed"])
	}
	advice, _ := out["index_advice"].(string)
	if advice != docxIndexAdvice {
		t.Errorf("index_advice = %q, want %q", advice, docxIndexAdvice)
	}
}

// TestDocxFormat_NonNormalizeRuleOmitsIndexAdvice is the negative case: a
// rule that never changes paragraph count must report para_count_changed
// false and total_paras still populated (docx_edit parity), with no
// index_advice at all (omitempty), not a stale or fabricated one.
func TestDocxFormat_NonNormalizeRuleOmitsIndexAdvice(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxFormat(t, map[string]any{
		"path":  p,
		"rules": map[string]any{"body_font": "Georgia"},
	})
	if err != nil {
		t.Fatalf("DocxFormatHandler: %v", err)
	}
	out := decodeRead(t, res)
	totalParas, ok := out["total_paras"].(float64)
	if !ok || totalParas <= 0 {
		t.Fatalf("total_paras missing or not a positive number: %v (content=%s)", out["total_paras"], res.Content)
	}
	if out["para_count_changed"] != false {
		t.Errorf("para_count_changed = %v, want false", out["para_count_changed"])
	}
	if _, present := out["index_advice"]; present {
		t.Errorf("index_advice = %v, want the key absent entirely (omitempty)", out["index_advice"])
	}
}

// TestDocxFormat_TemplateAppliesPreset exercises the "template" rule
// end to end through the tool layer, not just pkg/docx directly.
func TestDocxFormat_TemplateAppliesPreset(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxFormat(t, map[string]any{
		"path":  p,
		"rules": map[string]any{"template": "academic"},
	})
	if err != nil {
		t.Fatalf("DocxFormatHandler: %v", err)
	}
	out := decodeRead(t, res)
	applied, _ := out["applied"].([]any)
	if len(applied) == 0 {
		t.Fatal("applied is empty for the academic template")
	}
}

// TestDocxFormat_UnknownTemplateErrors pins that an unknown template name
// is reported, not silently ignored, and does not touch the file.
func TestDocxFormat_UnknownTemplateErrors(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	original, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := callDocxFormat(t, map[string]any{
		"path":  p,
		"rules": map[string]any{"template": "fancy"},
	}); err == nil {
		t.Fatal("an unknown template name was accepted")
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Error("a rejected template still modified the file on disk")
	}
}

// TestDocxFormat_BacksUpOnceBeforeTheFirstOverwrite mirrors
// TestDocxEdit_BacksUpOnceBeforeTheFirstOverwrite: the backup must hold the
// pristine original for the lifetime of a multi-call formatting run, not
// get refreshed by a later call.
func TestDocxFormat_BacksUpOnceBeforeTheFirstOverwrite(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	original, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	format := func(font string) {
		t.Helper()
		if _, err := callDocxFormat(t, map[string]any{
			"path":  p,
			"rules": map[string]any{"body_font": font},
		}); err != nil {
			t.Fatalf("format %q: %v", font, err)
		}
	}
	format("Georgia")
	backup := p + ".bak"
	first, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("no backup after the first format call: %v", err)
	}
	if !bytes.Equal(first, original) {
		t.Error("the backup is not the pristine original")
	}

	format("Verdana")
	second, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(second, original) {
		t.Error("the second format call refreshed the backup; it must be written once")
	}
}

// TestDocxFormat_ReportsBackupCreated mirrors TestDocxEdit_ReportsBackupCreated.
func TestDocxFormat_ReportsBackupCreated(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	format := func(font string) map[string]any {
		t.Helper()
		res, err := callDocxFormat(t, map[string]any{
			"path":  p,
			"rules": map[string]any{"body_font": font},
		})
		if err != nil {
			t.Fatalf("format %q: %v", font, err)
		}
		return decodeRead(t, res)
	}

	first := format("Georgia")
	if first["backup_created"] != true {
		t.Errorf("backup_created = %v on the first backup-creating call, want true", first["backup_created"])
	}
	second := format("Verdana")
	if second["backup_created"] != false {
		t.Errorf("backup_created = %v on the second call, want false (the backup already existed)", second["backup_created"])
	}
	if second["backup_path"] != first["backup_path"] {
		t.Errorf("backup_path changed between calls: %v vs %v", first["backup_path"], second["backup_path"])
	}
}

// TestDocxFormat_StaleBackupWarnsOnPreExisting mirrors
// TestDocxEdit_StaleBackupWarnsOnPreExisting: docx_format shares
// backupDocxOnce with docx_edit, so it has the exact same stale-backup risk
// and must carry the exact same warning.
func TestDocxFormat_StaleBackupWarnsOnPreExisting(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	format := func(font string) map[string]any {
		t.Helper()
		res, err := callDocxFormat(t, map[string]any{
			"path":  p,
			"rules": map[string]any{"body_font": font},
		})
		if err != nil {
			t.Fatalf("format %q: %v", font, err)
		}
		return decodeRead(t, res)
	}

	first := format("Georgia")
	firstNotes, _ := first["notes"].([]any)
	if strings.Contains(fmt.Sprint(firstNotes), "pre-existing backup") {
		t.Errorf("notes on the first (backup-creating) call = %v, want no stale-backup warning yet", firstNotes)
	}
	second := format("Verdana")
	notes, _ := second["notes"].([]any)
	joined := fmt.Sprint(notes)
	if !strings.Contains(joined, "pre-existing backup") {
		t.Errorf("notes = %v, want a warning that the .bak pre-existed and should be verified before rollback", notes)
	}
}

// TestDocxFormat_PageNumbersAddsFooterOnPythonDocxStyleFixture is task 12's
// end-to-end pin for the "no footer yet" path, through the tool layer
// (parseDocxFormatRules -> docx.FormatOptions.PageNumbers ->
// Document.Format): outline.docx is a real python-docx product with no
// header/footer at all (gen_fixtures.py), so a page_numbers:true call must
// add one, report it in applied, and leave the file readable as a zip with
// the new word/footer1.xml entry present.
func TestDocxFormat_PageNumbersAddsFooterOnPythonDocxStyleFixture(t *testing.T) {
	p := docxFixture(t, "outline.docx")

	res, err := callDocxFormat(t, map[string]any{
		"path":  p,
		"rules": map[string]any{"page_numbers": true},
	})
	if err != nil {
		t.Fatalf("DocxFormatHandler: %v", err)
	}
	out := decodeRead(t, res)
	applied, _ := out["applied"].([]any)
	if len(applied) != 1 || !strings.Contains(fmt.Sprint(applied[0]), "footer1.xml") {
		t.Errorf("applied = %v, want one entry naming word/footer1.xml", applied)
	}
	notes, _ := out["notes"].([]any)
	for _, n := range notes {
		if strings.Contains(fmt.Sprint(n), "already has a footer") {
			t.Errorf("notes unexpectedly contains the already-has-a-footer note: %v", notes)
		}
	}

	footer := zipEntry(t, p, "word/footer1.xml")
	if len(footer) == 0 {
		t.Error("word/footer1.xml is empty or missing after page_numbers:true")
	}
	docXML := string(zipEntry(t, p, docx.DocumentPart))
	if !strings.Contains(docXML, "<w:footerReference") {
		t.Error("document.xml has no <w:footerReference> after page_numbers:true")
	}
}

// TestDocxFormat_PageNumbersIsANoOpOnDocxWriteProduct is task 12's
// end-to-end pin for the "already has a footer" path: docx_write's own
// output always carries word/footer1.xml plus a footerReference (footer.go,
// docx-chinese-typography plan Part C), so asking docx_format for
// page_numbers on it must change nothing and say why, rather than adding a
// second footer or erroring.
func TestDocxFormat_PageNumbersIsANoOpOnDocxWriteProduct(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "written.docx")
	if _, err := callDocxWrite(t, map[string]any{
		"path":     p,
		"markdown": "# Title\n\nSome body text.\n",
	}); err != nil {
		t.Fatalf("DocxWriteHandler: %v", err)
	}
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}

	res, err := callDocxFormat(t, map[string]any{
		"path":  p,
		"rules": map[string]any{"page_numbers": true},
	})
	if err != nil {
		t.Fatalf("DocxFormatHandler: %v", err)
	}
	out := decodeRead(t, res)
	applied, _ := out["applied"].([]any)
	if len(applied) != 0 {
		t.Errorf("applied = %v, want none: docx_write's own product already has a footer", applied)
	}
	notes, _ := out["notes"].([]any)
	found := false
	for _, n := range notes {
		if fmt.Sprint(n) == "document already has a footer; not modified" {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %v, want the already-has-a-footer note", notes)
	}

	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("a no-op page_numbers call on a docx_write product rewrote the file's bytes")
	}
}

// TestDocxFormat_RebuildTocErrorsExplicitly is rebuild_toc's half of the
// same requirement.
func TestDocxFormat_RebuildTocErrorsExplicitly(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	original, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = callDocxFormat(t, map[string]any{
		"path":  p,
		"rules": map[string]any{"rebuild_toc": true},
	})
	if err == nil {
		t.Fatal("rebuild_toc=true was accepted; want an explicit error")
	}
	if !strings.Contains(err.Error(), "rebuild_toc") {
		t.Errorf("error = %q, want it to name rebuild_toc", err)
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Error("a rejected rebuild_toc request still modified the file on disk")
	}
}

// TestDocxFormat_PageNumbersFalseIsNotAnError pins the other side: a
// caller that explicitly sends page_numbers=false (meaning "not requested")
// must not be refused just for naming the field.
func TestDocxFormat_PageNumbersFalseIsNotAnError(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	if _, err := callDocxFormat(t, map[string]any{
		"path":  p,
		"rules": map[string]any{"body_font": "Georgia", "page_numbers": false, "rebuild_toc": false},
	}); err != nil {
		t.Fatalf("page_numbers/rebuild_toc explicitly false must not error: %v", err)
	}
}

// TestDocxFormat_RejectsWrongTypedFields pins the brief's "type-checked,
// never coerced" requirement for every field in rules, following the same
// pattern as TestDocxEdit_RejectsWrongTypedTextAndOp: a wrong-typed value
// must be refused with an error, never silently coerced to a zero value
// that then reports success.
func TestDocxFormat_RejectsWrongTypedFields(t *testing.T) {
	tests := []struct {
		name  string
		rules map[string]any
	}{
		{"template not a string", map[string]any{"template": float64(1)}},
		{"heading_font not a string", map[string]any{"heading_font": float64(1)}},
		{"body_font not a string", map[string]any{"body_font": float64(1)}},
		{"body_size_pt not a number", map[string]any{"body_size_pt": "big"}},
		{"line_spacing not a number", map[string]any{"line_spacing": "double"}},
		{"line_spacing_exact_pt not a number", map[string]any{"line_spacing_exact_pt": "double"}},
		{"body_east_asia_font not a string", map[string]any{"body_east_asia_font": float64(1)}},
		{"first_line_indent_chars not a number", map[string]any{"first_line_indent_chars": "two"}},
		{"space_before_pt not a number", map[string]any{"space_before_pt": "six"}},
		{"space_after_pt not a number", map[string]any{"space_after_pt": "six"}},
		{"align not a string", map[string]any{"align": true}},
		{"normalize not a boolean", map[string]any{"normalize": "yes"}},
		{"page_numbers not a boolean", map[string]any{"page_numbers": "yes"}},
		{"rebuild_toc not a boolean", map[string]any{"rebuild_toc": "yes"}},
		{"margins_mm not an array", map[string]any{"margins_mm": "big margins"}},
		{"margins_mm element not a number", map[string]any{"margins_mm": []any{float64(10), "x", float64(10), float64(10)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := docxFixture(t, "outline.docx")
			original, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := callDocxFormat(t, map[string]any{"path": p, "rules": tt.rules}); err == nil {
				t.Fatal("a wrong-typed rules field returned nil error")
			}
			after, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(original, after) {
				t.Error("a rejected wrong-typed field still modified the file on disk")
			}
		})
	}
}

// TestDocxFormat_RejectsBadMarginsMM pins length and sign validation at the
// tool layer, before the request ever reaches pkg/docx.
func TestDocxFormat_RejectsBadMarginsMM(t *testing.T) {
	tests := []struct {
		name string
		mm   []any
	}{
		{"too few", []any{float64(10), float64(10)}},
		{"too many", []any{float64(10), float64(10), float64(10), float64(10), float64(10)}},
		{"zero value", []any{float64(0), float64(10), float64(10), float64(10)}},
		{"negative value", []any{float64(10), float64(-5), float64(10), float64(10)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := docxFixture(t, "outline.docx")
			if _, err := callDocxFormat(t, map[string]any{
				"path":  p,
				"rules": map[string]any{"margins_mm": tt.mm},
			}); err == nil {
				t.Fatalf("margins_mm=%v was accepted", tt.mm)
			}
		})
	}
}

// TestDocxFormat_RejectsNonPositiveNewMeasurementFields is review F6's red
// test: first_line_indent_chars/space_before_pt/space_after_pt/
// line_spacing_exact_pt must reject an EXPLICITLY-sent zero or negative
// value, with the field's own name in the error — this layer, unlike
// pkg/docx's own FormatOptions (where 0 is indistinguishable from "not
// requested"), can tell the caller actually sent the key, so it holds
// these four to a stricter rule than a bare "must not be negative".
func TestDocxFormat_RejectsNonPositiveNewMeasurementFields(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  float64
	}{
		{"first_line_indent_chars zero", "first_line_indent_chars", 0},
		{"first_line_indent_chars negative", "first_line_indent_chars", -2},
		{"space_before_pt zero", "space_before_pt", 0},
		{"space_before_pt negative", "space_before_pt", -6},
		{"space_after_pt zero", "space_after_pt", 0},
		{"space_after_pt negative", "space_after_pt", -12},
		{"line_spacing_exact_pt zero", "line_spacing_exact_pt", 0},
		{"line_spacing_exact_pt negative", "line_spacing_exact_pt", -24},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := docxFixture(t, "outline.docx")
			_, err := callDocxFormat(t, map[string]any{
				"path":  p,
				"rules": map[string]any{tt.key: tt.val},
			})
			if err == nil {
				t.Fatalf("rules.%s = %g was accepted; want an error", tt.key, tt.val)
			}
			if !strings.Contains(err.Error(), tt.key) {
				t.Errorf("error %q does not name the field %q", err.Error(), tt.key)
			}
		})
	}
}

// TestDocxFormat_ZeroRejectedFieldsDocumentItInSchema is task 9 brief item
// 7a (Task 8 review round 2's first nit): the four measurement fields that
// reject an explicit 0 (TestDocxFormat_RejectsNonPositiveNewMeasurementFields,
// above) must SAY so in their own schema description, not rely on a caller
// discovering it only by hitting the error.
func TestDocxFormat_ZeroRejectedFieldsDocumentItInSchema(t *testing.T) {
	schema := DocxFormatTool().InputSchema
	props := schema["properties"].(map[string]any)
	rules := props["rules"].(map[string]any)
	ruleProps := rules["properties"].(map[string]any)

	want := "Must be > 0; omit the field to leave it unchanged (0 is rejected)."
	for _, key := range []string{"line_spacing_exact_pt", "first_line_indent_chars", "space_before_pt", "space_after_pt"} {
		field, ok := ruleProps[key].(map[string]any)
		if !ok {
			t.Fatalf("rules.%s is not present in the schema", key)
		}
		desc, _ := field["description"].(string)
		if !strings.Contains(desc, want) {
			t.Errorf("rules.%s description = %q, want it to contain %q", key, desc, want)
		}
	}
}

// TestDocxFormat_MarginsMMAccepted is the positive counterpart to
// TestDocxFormat_RejectsBadMarginsMM: a valid 4-element array must be
// accepted and reported in applied.
func TestDocxFormat_MarginsMMAccepted(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxFormat(t, map[string]any{
		"path":  p,
		"rules": map[string]any{"margins_mm": []any{float64(20), float64(20), float64(20), float64(20)}},
	})
	if err != nil {
		t.Fatalf("DocxFormatHandler: %v", err)
	}
	out := decodeRead(t, res)
	applied, _ := out["applied"].([]any)
	if len(applied) == 0 {
		t.Fatal("applied is empty for a valid margins_mm change")
	}
}

// TestDocxFormat_RulesMustBeAnObject pins that a non-object rules value
// (e.g. a bare string) is rejected rather than silently treated as empty.
func TestDocxFormat_RulesMustBeAnObject(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	if _, err := callDocxFormat(t, map[string]any{
		"path":  p,
		"rules": "corporate",
	}); err == nil {
		t.Fatal("a non-object rules value was accepted")
	}
}

// TestDocxFormat_IsInDocumentGroupAndNotParallelSafe pins the brief's
// requirement directly: docx_format writes, so it must never run in
// parallel with another tool call, and it must be selectable by the
// document-editor subagent profile's group-based tool selection.
func TestDocxFormat_IsInDocumentGroupAndNotParallelSafe(t *testing.T) {
	tool := DocxFormatTool()
	if tool.ParallelSafe {
		t.Error("docx_format.ParallelSafe = true, want false; it writes to disk")
	}
	found := false
	for _, g := range tool.Groups {
		if g == "document" {
			found = true
		}
	}
	if !found {
		t.Errorf("docx_format.Groups = %v, want it to contain %q", tool.Groups, "document")
	}
}

// zipEntry reads name's raw bytes out of the .docx at path, generalizing
// readDocumentXML to any zip entry. Range-formatting tests use it to prove
// word/styles.xml is left byte-identical when a range is given: the P2a.5
// gap this task closes only makes sense if the range path really lands in
// word/document.xml (direct formatting) and never touches the stylesheet at
// all.
func zipEntry(t *testing.T, path, name string) []byte {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open %s as zip: %v", path, err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s entry: %v", name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %s entry: %v", name, err)
		}
		return data
	}
	t.Fatalf("%s has no %s entry", path, name)
	return nil
}

// TestDocxFormat_RangeAppliesDirectRunFormattingToOnlyThatParagraph is the
// tool-layer half of P2a.5: start_para/end_para must reach
// docx.FormatOptions and take the direct-formatting path (word/document.xml,
// per-run <w:rPr>), never the whole-document styles.xml path. This is the
// exact capability the user was blocked on: changing one paragraph's font
// size without falling back to a script.
func TestDocxFormat_RangeAppliesDirectRunFormattingToOnlyThatParagraph(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	stylesBefore := zipEntry(t, p, "word/styles.xml")

	res, err := callDocxFormat(t, map[string]any{
		"path":       p,
		"start_para": float64(2),
		"end_para":   float64(2),
		"rules":      map[string]any{"body_size_pt": float64(14)},
	})
	if err != nil {
		t.Fatalf("DocxFormatHandler: %v", err)
	}
	out := decodeRead(t, res)
	applied, _ := out["applied"].([]any)
	if len(applied) == 0 {
		t.Fatalf("applied is empty; want an entry reporting the range change (content=%s)", res.Content)
	}
	found := false
	for _, a := range applied {
		s, _ := a.(string)
		if strings.Contains(s, "2-2") && strings.Contains(s, "(1 paragraph(s))") {
			found = true
		}
	}
	if !found {
		t.Errorf("applied = %v, want an entry naming the range (2-2) and the affected paragraph count (1 paragraph(s))", applied)
	}

	docXML := readDocumentXML(t, p)
	if !strings.Contains(docXML, `<w:sz w:val="28"/>`) || !strings.Contains(docXML, `<w:szCs w:val="28"/>`) {
		t.Errorf("word/document.xml does not contain the direct-formatted size: %s", docXML)
	}

	stylesAfter := zipEntry(t, p, "word/styles.xml")
	if !bytes.Equal(stylesBefore, stylesAfter) {
		t.Error("word/styles.xml changed; a ranged call must land purely in word/document.xml, never the stylesheet")
	}
}

// TestDocxFormat_RangeAppliedReportsCountEvenForLargerRanges pins the
// self-review requirement directly: "applied" must say how many paragraphs
// were affected, so a caller can tell a one-paragraph change from a
// document-wide one.
func TestDocxFormat_RangeAppliedReportsCountEvenForLargerRanges(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxFormat(t, map[string]any{
		"path":       p,
		"start_para": float64(5),
		"end_para":   float64(8),
		"rules":      map[string]any{"align": "left"},
	})
	if err != nil {
		t.Fatalf("DocxFormatHandler: %v", err)
	}
	out := decodeRead(t, res)
	applied, _ := out["applied"].([]any)
	found := false
	for _, a := range applied {
		s, _ := a.(string)
		if strings.Contains(s, "5-8") && strings.Contains(s, "(4 paragraph(s))") {
			found = true
		}
	}
	if !found {
		t.Errorf("applied = %v, want an entry naming the range (5-8) and 4 paragraph(s)", applied)
	}
}

// TestDocxFormat_RangeOmittingEndParaDefaultsToStartPara pins that a
// caller wanting exactly one paragraph only has to say so once.
func TestDocxFormat_RangeOmittingEndParaDefaultsToStartPara(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxFormat(t, map[string]any{
		"path":       p,
		"start_para": float64(3),
		"rules":      map[string]any{"body_font": "Georgia"},
	})
	if err != nil {
		t.Fatalf("DocxFormatHandler: %v", err)
	}
	out := decodeRead(t, res)
	applied, _ := out["applied"].([]any)
	found := false
	for _, a := range applied {
		s, _ := a.(string)
		if strings.Contains(s, "3-3") && strings.Contains(s, "Georgia") {
			found = true
		}
	}
	if !found {
		t.Errorf("applied = %v, want an entry naming the single-paragraph range (3-3)", applied)
	}
}

// TestDocxFormat_RangeOutOfBoundsIsActionable pins that an out-of-range
// start_para produces an error naming both the requested value and the
// document's actual paragraph count, not a generic failure.
func TestDocxFormat_RangeOutOfBoundsIsActionable(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	original, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = callDocxFormat(t, map[string]any{
		"path":       p,
		"start_para": float64(9999),
		"rules":      map[string]any{"body_font": "Georgia"},
	})
	if err == nil {
		t.Fatal("start_para beyond the document's paragraph count was accepted")
	}
	if !strings.Contains(err.Error(), "9999") || !strings.Contains(err.Error(), "73") {
		t.Errorf("error = %q, want it to name both the requested start_para (9999) and the document's actual paragraph count (73)", err)
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Error("an out-of-range start_para still modified the file on disk")
	}
}

// TestDocxFormat_RangeInvertedIsActionable pins that end_para before
// start_para produces an actionable error rather than silently doing
// nothing or misinterpreting the range.
func TestDocxFormat_RangeInvertedIsActionable(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	_, err := callDocxFormat(t, map[string]any{
		"path":       p,
		"start_para": float64(5),
		"end_para":   float64(2),
		"rules":      map[string]any{"body_font": "Georgia"},
	})
	if err == nil {
		t.Fatal("end_para before start_para was accepted")
	}
	if !strings.Contains(err.Error(), "5") || !strings.Contains(err.Error(), "2") {
		t.Errorf("error = %q, want it to name both start_para (5) and end_para (2)", err)
	}
}

// TestDocxFormat_RangeRejectsWrongTypedValues pins the brief's
// never-coerce requirement for start_para/end_para specifically: a string
// "2" must be refused outright, never silently read as 0 — which would
// turn a one-paragraph request into a document-wide rewrite instead of an
// error, exactly the failure mode the brief calls out by name.
func TestDocxFormat_RangeRejectsWrongTypedValues(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
	}{
		{"start_para as string", map[string]any{"start_para": "2"}},
		{"end_para as string", map[string]any{"start_para": float64(2), "end_para": "3"}},
		{"start_para as bool", map[string]any{"start_para": true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := docxFixture(t, "outline.docx")
			original, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			args := map[string]any{"path": p, "rules": map[string]any{"body_font": "Georgia"}}
			for k, v := range tt.args {
				args[k] = v
			}
			if _, err := callDocxFormat(t, args); err == nil {
				t.Fatalf("wrong-typed range field %v was accepted", tt.args)
			}
			after, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(original, after) {
				t.Error("a rejected wrong-typed range field still modified the file on disk")
			}
		})
	}
}

// TestDocxFormat_RangeRejectsDocumentWideOnlyRules pins point 4 of the
// brief: template/heading_font/margins_mm/normalize only make sense
// document-wide, and pkg/docx already refuses them when combined with a
// paragraph range (Document.formatDirectRange). This test proves the tool
// layer surfaces that refusal cleanly rather than swallowing it or
// duplicating the check with different wording.
func TestDocxFormat_RangeRejectsDocumentWideOnlyRules(t *testing.T) {
	tests := []struct {
		name  string
		rules map[string]any
	}{
		{"template", map[string]any{"template": "academic"}},
		{"heading_font", map[string]any{"heading_font": "Georgia"}},
		{"margins_mm", map[string]any{"margins_mm": []any{float64(20), float64(20), float64(20), float64(20)}}},
		{"normalize", map[string]any{"normalize": true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := docxFixture(t, "outline.docx")
			original, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			_, err = callDocxFormat(t, map[string]any{
				"path":       p,
				"start_para": float64(2),
				"end_para":   float64(2),
				"rules":      tt.rules,
			})
			if err == nil {
				t.Fatalf("%s combined with a paragraph range was accepted", tt.name)
			}
			if !strings.Contains(err.Error(), "range") {
				t.Errorf("error = %q, want it to explain the rule cannot combine with a paragraph range", err)
			}
			after, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(original, after) {
				t.Errorf("%s rejected with a range still modified the file on disk", tt.name)
			}
		})
	}
}

// TestDocxFormat_RangeSecondIdenticalCallDoesNotRewriteFile is the range
// path's tool-layer counterpart to TestDocxFormat_SecondIdenticalCallReportsNoChangeNote:
// calling docx_format twice with the exact same start_para/end_para and
// rules must report an empty applied (with an "already ..." note) and, most
// importantly, must NOT touch the file or the backup a second time — task 10
// brief item 3 (task 8 review's range-path finding that a repeat call
// unconditionally re-reported success and rewrote the file even though no
// byte actually changed).
func TestDocxFormat_RangeSecondIdenticalCallDoesNotRewriteFile(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	args := map[string]any{
		"path":       p,
		"start_para": float64(2),
		"end_para":   float64(2),
		"rules":      map[string]any{"body_font": "Georgia", "align": "center"},
	}

	first, err := callDocxFormat(t, args)
	if err != nil {
		t.Fatalf("first DocxFormatHandler: %v", err)
	}
	firstOut := decodeRead(t, first)
	if applied, _ := firstOut["applied"].([]any); len(applied) == 0 {
		t.Fatalf("first call's applied is empty; the rule never took effect (content=%s)", first.Content)
	}
	afterFirst, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}

	second, err := callDocxFormat(t, args)
	if err != nil {
		t.Fatalf("second DocxFormatHandler: %v", err)
	}
	secondOut := decodeRead(t, second)
	if applied, _ := secondOut["applied"].([]any); len(applied) != 0 {
		t.Errorf("second identical call's applied = %v, want empty (content=%s)", applied, second.Content)
	}
	notes, _ := secondOut["notes"].([]any)
	found := false
	for _, n := range notes {
		if s, ok := n.(string); ok && strings.Contains(s, "already") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %v, want an \"already ...\" note on the second identical call", notes)
	}
	if secondOut["backup_created"] != false {
		t.Errorf("backup_created = %v on the second call, want false (nothing changed, so no new backup)", secondOut["backup_created"])
	}

	afterSecond, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterFirst, afterSecond) {
		t.Error("the second identical range call rewrote the file even though nothing changed")
	}
}

// TestDocxFormat_RangeRejectsPageNumbersAndRebuildToc pins that page_numbers
// stays rejected when combined with a range: pkg/docx's formatDirectRange
// refuses FormatOptions.PageNumbers outright, the same way it refuses
// Template/HeadingFont/MarginsMM/Normalize, since a footer is a
// section-level concept, not a paragraph's own direct formatting.
func TestDocxFormat_RangeRejectsPageNumbersAndRebuildToc(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	if _, err := callDocxFormat(t, map[string]any{
		"path":       p,
		"start_para": float64(2),
		"end_para":   float64(2),
		"rules":      map[string]any{"page_numbers": true},
	}); err == nil {
		t.Fatal("page_numbers=true combined with a range was accepted")
	}
}

// TestDocxFormat_RangeEndParaWithoutStartParaErrors pins pkg/docx's own
// rule (Document.Format) is surfaced through the tool layer: end_para
// without start_para has no range to end.
func TestDocxFormat_RangeEndParaWithoutStartParaErrors(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	if _, err := callDocxFormat(t, map[string]any{
		"path":     p,
		"end_para": float64(3),
		"rules":    map[string]any{"body_font": "Georgia"},
	}); err == nil {
		t.Fatal("end_para without start_para was accepted")
	}
}

// TestDocxFormat_WithoutRangeRemainsDocumentWide is the regression pin the
// plan requires: omitting start_para/end_para entirely must still take the
// whole-document styles.xml path, byte-identical to pre-P2a.5 behavior.
// This guards against a schema change that accidentally defaults StartPara
// to a nonzero value.
func TestDocxFormat_WithoutRangeRemainsDocumentWide(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	docBefore := zipEntry(t, p, docx.DocumentPart)

	res, err := callDocxFormat(t, map[string]any{
		"path":  p,
		"rules": map[string]any{"body_font": "Georgia"},
	})
	if err != nil {
		t.Fatalf("DocxFormatHandler: %v", err)
	}
	out := decodeRead(t, res)
	applied, _ := out["applied"].([]any)
	if len(applied) == 0 {
		t.Fatal("applied is empty for a document-wide body_font change")
	}

	docAfter := zipEntry(t, p, docx.DocumentPart)
	if !bytes.Equal(docBefore, docAfter) {
		t.Error("a rules-only (no range) call changed word/document.xml; body_font without a range must land only in word/styles.xml")
	}
}

// --- Task 8: align center/right, body_east_asia_font, first_line_indent_chars,
// space_before_pt/space_after_pt, line_spacing_exact_pt, end to end through
// the tool layer ---

// TestDocxFormat_AlignCenterAndRightAppliedDocumentWide pins Critical 3's
// fix at the tool layer: "center"/"right" used to have nowhere to go
// (pkg/docx rejected them outright); both now land in styles.xml's
// docDefaults <w:jc>.
func TestDocxFormat_AlignCenterAndRightAppliedDocumentWide(t *testing.T) {
	for _, align := range []string{"center", "right"} {
		t.Run(align, func(t *testing.T) {
			p := docxFixture(t, "outline.docx")
			res, err := callDocxFormat(t, map[string]any{
				"path":  p,
				"rules": map[string]any{"align": align},
			})
			if err != nil {
				t.Fatalf("DocxFormatHandler: %v", err)
			}
			out := decodeRead(t, res)
			applied, _ := out["applied"].([]any)
			if len(applied) == 0 {
				t.Fatalf("applied is empty for align=%q (content=%s)", align, res.Content)
			}
			styles := string(zipEntry(t, p, "word/styles.xml"))
			if !strings.Contains(styles, `<w:jc w:val="`+align+`"/>`) {
				t.Errorf("styles.xml docDefaults lacks <w:jc w:val=%q/>: %s", align, styles)
			}
		})
	}
}

// TestDocxFormat_BodyEastAsiaFontOrthogonalToBodyFont pins BodyEastAsiaFont's own
// contract at the tool layer: body_font alone must leave docDefaults'
// eastAsia font untouched, and body_east_asia_font alone must leave ascii/hAnsi
// untouched — the fix for "中文宋体+西文 Times 表达不了" (format capability
// review, Important 8).
func TestDocxFormat_BodyEastAsiaFontOrthogonalToBodyFont(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxFormat(t, map[string]any{
		"path":  p,
		"rules": map[string]any{"body_font": "Georgia", "body_east_asia_font": "SimSun"},
	})
	if err != nil {
		t.Fatalf("DocxFormatHandler: %v", err)
	}
	out := decodeRead(t, res)
	applied, _ := out["applied"].([]any)
	var haveFont, haveEastAsia bool
	for _, a := range applied {
		s, _ := a.(string)
		if strings.Contains(s, "body font") {
			haveFont = true
		}
		if strings.Contains(s, "east asia font") {
			haveEastAsia = true
		}
	}
	if !haveFont || !haveEastAsia {
		t.Errorf("applied = %v, want both a body-font and an east-asia-font entry", applied)
	}
	styles := string(zipEntry(t, p, "word/styles.xml"))
	dd := styles[strings.Index(styles, "<w:docDefaults>"):strings.Index(styles, "</w:docDefaults>")]
	if !strings.Contains(dd, `w:ascii="Georgia"`) {
		t.Errorf("docDefaults lacks the Latin font: %s", dd)
	}
	if !strings.Contains(dd, `w:eastAsia="SimSun"`) {
		t.Errorf("docDefaults lacks the east-asia font: %s", dd)
	}
}

// TestDocxFormat_FirstLineIndentCharsLandsOnInd covers the whole-document
// path end to end: 2 characters -> firstLineChars="200" plus the twips
// fallback.
func TestDocxFormat_FirstLineIndentCharsLandsOnInd(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxFormat(t, map[string]any{
		"path":  p,
		"rules": map[string]any{"first_line_indent_chars": float64(2)},
	})
	if err != nil {
		t.Fatalf("DocxFormatHandler: %v", err)
	}
	out := decodeRead(t, res)
	applied, _ := out["applied"].([]any)
	if len(applied) == 0 {
		t.Fatalf("applied is empty (content=%s)", res.Content)
	}
	styles := string(zipEntry(t, p, "word/styles.xml"))
	if !strings.Contains(styles, `w:firstLineChars="200"`) {
		t.Errorf("styles.xml lacks firstLineChars=200: %s", styles)
	}
	if !strings.Contains(styles, `w:firstLine="420"`) {
		t.Errorf("styles.xml lacks the firstLine twips fallback (420): %s", styles)
	}
}

// TestDocxFormat_SpaceBeforeAndAfterLandOnSameSpacingElementAsLineSpacing
// covers space_before_pt/space_after_pt combined with line_spacing in ONE
// call, landing on the SAME <w:spacing> element without duplicating it.
func TestDocxFormat_SpaceBeforeAndAfterLandOnSameSpacingElementAsLineSpacing(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxFormat(t, map[string]any{
		"path": p,
		"rules": map[string]any{
			"line_spacing":    1.5,
			"space_before_pt": float64(6),
			"space_after_pt":  float64(12),
		},
	})
	if err != nil {
		t.Fatalf("DocxFormatHandler: %v", err)
	}
	out := decodeRead(t, res)
	applied, _ := out["applied"].([]any)
	if len(applied) < 3 {
		t.Fatalf("applied = %v, want at least 3 entries (line spacing, space before, space after)", applied)
	}
	styles := string(zipEntry(t, p, "word/styles.xml"))
	dd := styles[strings.Index(styles, "<w:docDefaults>"):strings.Index(styles, "</w:docDefaults>")]
	if strings.Count(dd, "<w:spacing ") != 1 {
		t.Errorf("docDefaults has %d <w:spacing> elements, want exactly 1 (before/after/line must merge into one tag): %s",
			strings.Count(dd, "<w:spacing "), dd)
	}
	if !strings.Contains(dd, `w:before="120"`) || !strings.Contains(dd, `w:after="240"`) || !strings.Contains(dd, `w:line="360"`) {
		t.Errorf("docDefaults spacing lacks before=120/after=240/line=360: %s", dd)
	}
}

// TestDocxFormat_LineSpacingExactPtLandsAsExactLineRule covers
// line_spacing_exact_pt end to end: 24pt -> w:line="480" w:lineRule="exact".
func TestDocxFormat_LineSpacingExactPtLandsAsExactLineRule(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	res, err := callDocxFormat(t, map[string]any{
		"path":  p,
		"rules": map[string]any{"line_spacing_exact_pt": float64(24)},
	})
	if err != nil {
		t.Fatalf("DocxFormatHandler: %v", err)
	}
	out := decodeRead(t, res)
	applied, _ := out["applied"].([]any)
	if len(applied) == 0 {
		t.Fatalf("applied is empty (content=%s)", res.Content)
	}
	styles := string(zipEntry(t, p, "word/styles.xml"))
	if !strings.Contains(styles, `w:line="480" w:lineRule="exact"`) {
		t.Errorf("styles.xml lacks w:line=480/w:lineRule=exact: %s", styles)
	}
}

// TestDocxFormat_LineSpacingMutexRejectedAtToolLayer confirms the domain
// rule (pkg/docx's validateAlignAndLineSpacingMutex) surfaces as an error
// through the tool handler, not just at the pkg/docx API.
func TestDocxFormat_LineSpacingMutexRejectedAtToolLayer(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	if _, err := callDocxFormat(t, map[string]any{
		"path":  p,
		"rules": map[string]any{"line_spacing": 1.5, "line_spacing_exact_pt": float64(24)},
	}); err == nil {
		t.Fatal("line_spacing + line_spacing_exact_pt together was accepted; want an error")
	}
}

// TestDocxFormat_RangeAppliesNewTask8FieldsDirectly exercises the
// paragraph-range path for all four new fields at once, confirming they
// land as DIRECT formatting in word/document.xml, never word/styles.xml.
func TestDocxFormat_RangeAppliesNewTask8FieldsDirectly(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	stylesBefore := zipEntry(t, p, "word/styles.xml")

	res, err := callDocxFormat(t, map[string]any{
		"path":       p,
		"start_para": float64(2),
		"end_para":   float64(2),
		"rules": map[string]any{
			"body_east_asia_font":     "SimSun",
			"first_line_indent_chars": float64(2),
			"space_before_pt":         float64(6),
			"space_after_pt":          float64(12),
			"line_spacing_exact_pt":   float64(24),
		},
	})
	if err != nil {
		t.Fatalf("DocxFormatHandler: %v", err)
	}
	out := decodeRead(t, res)
	applied, _ := out["applied"].([]any)
	if len(applied) < 5 {
		t.Fatalf("applied = %v, want at least 5 entries", applied)
	}

	docXML := readDocumentXML(t, p)
	for _, want := range []string{
		`w:eastAsia="SimSun"`,
		`w:firstLineChars="200"`,
		`w:before="120"`,
		`w:after="240"`,
		`w:line="480" w:lineRule="exact"`,
	} {
		if !strings.Contains(docXML, want) {
			t.Errorf("word/document.xml lacks %s: %s", want, docXML)
		}
	}

	stylesAfter := zipEntry(t, p, "word/styles.xml")
	if !bytes.Equal(stylesBefore, stylesAfter) {
		t.Error("word/styles.xml changed; a ranged call must land purely in word/document.xml")
	}
}

// docxEditProtectSchemaDescription reads docx_edit's protect field
// description out of its InputSchema, the same way a model reads it.
func docxEditProtectSchemaDescription(t *testing.T) string {
	t.Helper()
	props, _ := DocxEditTool().InputSchema["properties"].(map[string]any)
	protect, _ := props["protect"].(map[string]any)
	desc, _ := protect["description"].(string)
	if desc == "" {
		t.Fatal("docx_edit schema has no protect.description")
	}
	return desc
}

// TestDocxEditTool_ProtectSchemaDocumentsTheTwoSpecialCases pins Task 13's
// M3 fix: the protect field's schema description used to say only "must
// survive every edit touching them", with no hint that delete and insert
// are handled differently (delete only warns; insert is checked against
// forgery, not against a "before"). A model reading only the schema (not
// pkg/docx's source) had no way to predict either behavior.
func TestDocxEditTool_ProtectSchemaDocumentsTheTwoSpecialCases(t *testing.T) {
	desc := docxEditProtectSchemaDescription(t)
	if !strings.Contains(desc, "delete") || !strings.Contains(strings.ToLower(desc), "warn") {
		t.Errorf("protect description = %q, want it to say delete only warns, never refuses", desc)
	}
	if !strings.Contains(desc, "insert") || !strings.Contains(strings.ToLower(desc), "forg") {
		t.Errorf("protect description = %q, want it to say insert is checked for forged/mistyped text instead", desc)
	}
}

// TestDocxEditTool_DescriptionPointsToDocxFormatRange pins Task 2's core
// fix: docx_edit's description must no longer send a model to a tool that
// (before P2a.5) could not do the job. It must name docx_format together
// with start_para/end_para, not the old bare "use docx_format" dead end.
func TestDocxEditTool_DescriptionPointsToDocxFormatRange(t *testing.T) {
	d := DocxEditTool().Description
	if strings.Contains(d, "Paragraph styling is out of scope here; use docx_format.") {
		t.Fatal("docx_edit still carries the pre-P2a.5 dead-end sentence")
	}
	if !strings.Contains(d, "docx_format") {
		t.Errorf("docx_edit description no longer mentions docx_format at all: %q", d)
	}
	if !strings.Contains(d, "start_para") || !strings.Contains(d, "end_para") {
		t.Errorf("docx_edit description must name start_para/end_para so the pointer to docx_format lands somewhere real: %q", d)
	}
}

// TestDocxFormatTool_DescriptionDistinguishesTheTwoModes pins that a model
// choosing between docx_edit and docx_format to change one paragraph's
// formatting can tell, from the description alone, that a range exists and
// what it does differently from the no-range path.
func TestDocxFormatTool_DescriptionDistinguishesTheTwoModes(t *testing.T) {
	d := DocxFormatTool().Description
	if !strings.Contains(d, "start_para") || !strings.Contains(d, "end_para") {
		t.Errorf("docx_format description does not mention start_para/end_para: %q", d)
	}
	if strings.Contains(strings.ToLower(d), "document-wide formatting") && !strings.Contains(d, "start_para") {
		t.Errorf("docx_format description still reads as document-wide only: %q", d)
	}
	// The description must make the without-range/with-range distinction
	// legible, not just mention the parameter names in passing.
	lower := strings.ToLower(d)
	if !strings.Contains(lower, "default") && !strings.Contains(lower, "style") {
		t.Errorf("docx_format description does not explain that the no-range path changes the document's default styles: %q", d)
	}
	if !strings.Contains(lower, "direct formatting") && !strings.Contains(lower, "those paragraphs") && !strings.Contains(lower, "paragraphs only") {
		t.Errorf("docx_format description does not explain that the range path applies direct, paragraph-scoped formatting: %q", d)
	}
}

// callDocxWrite invokes DocxWriteHandler the same way callDocxRead/
// callDocxEdit/callDocxFormat do for their tools.
func callDocxWrite(t *testing.T, args map[string]any) (models.ToolResult, error) {
	t.Helper()
	return DocxWriteHandler(context.Background(), models.ToolCall{
		ID: "c1", Name: "docx_write", Arguments: args,
	})
}

// TestDocxWrite_RequiresPath pins the brief's "path (required)": a missing
// or blank path must be refused before pkg/docx is ever touched, the same
// way every other docx tool refuses.
func TestDocxWrite_RequiresPath(t *testing.T) {
	if _, err := callDocxWrite(t, map[string]any{"markdown": "# H\n"}); err == nil {
		t.Fatal("missing path returned nil error")
	}
	if _, err := callDocxWrite(t, map[string]any{"path": "   ", "markdown": "# H\n"}); err == nil {
		t.Fatal("whitespace-only path returned nil error")
	}
}

// TestDocxWrite_RequiresMarkdown pins the brief's "markdown (required)":
// the key must actually be present, distinct from an empty string (which is
// a valid, if degenerate, request — see
// TestDocxWrite_EmptyMarkdownProducesAValidDocument).
func TestDocxWrite_RequiresMarkdown(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.docx")
	if _, err := callDocxWrite(t, map[string]any{"path": p}); err == nil {
		t.Fatal("missing markdown returned nil error")
	}
	if _, err := os.Stat(p); err == nil {
		t.Error("a rejected call must not have created the file")
	}
}

// TestDocxWrite_RejectsWrongTypedArgs pins the brief's "type-check every
// argument; never coerce" requirement, quoting its own example almost
// verbatim: a bare number arriving where a string is expected must be
// refused with an explicit error, never silently read as the zero value
// ("") while still reporting success.
func TestDocxWrite_RejectsWrongTypedArgs(t *testing.T) {
	base := func() (string, map[string]any) {
		p := filepath.Join(t.TempDir(), "out.docx")
		return p, map[string]any{"path": p, "markdown": "# H\n"}
	}

	if p, args := base(); true {
		args["markdown"] = 12345.0
		if _, err := callDocxWrite(t, args); err == nil {
			t.Error("markdown as a number returned nil error")
		}
		if _, statErr := os.Stat(p); statErr == nil {
			t.Error("a rejected call must not have created the file")
		}
	}
	if p, args := base(); true {
		args["title"] = 42.0
		if _, err := callDocxWrite(t, args); err == nil {
			t.Error("title as a number returned nil error")
		}
		if _, statErr := os.Stat(p); statErr == nil {
			t.Error("a rejected call must not have created the file")
		}
	}
	if _, args := base(); true {
		args["path"] = 7.0
		if _, err := callDocxWrite(t, args); err == nil {
			t.Error("path as a number returned nil error")
		}
	}
}

// TestDocxWrite_CreatesDocxAndReportsParaCount is the core happy path: the
// handler must produce a file pkg/docx's own reader can open, and the
// reported paras count must match what Scan finds, so a caller can sanity
// check the output size without reopening the file itself.
func TestDocxWrite_CreatesDocxAndReportsParaCount(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.docx")
	md := "# Chapter\n\nbody text\n\n- item one\n- item two\n\n| a | b |\n|---|---|\n| 1 | 2 |\n"
	res, err := callDocxWrite(t, map[string]any{"path": p, "markdown": md})
	if err != nil {
		t.Fatalf("DocxWriteHandler: %v", err)
	}
	out := decodeRead(t, res)
	paras, ok := out["paras"].(float64)
	if !ok {
		t.Fatalf("result has no numeric paras field: %v", out)
	}

	doc, err := docx.OpenDocument(p)
	if err != nil {
		t.Fatalf("the written file cannot be reopened: %v", err)
	}
	if got := len(doc.Paras()); got != int(paras) {
		t.Errorf("reported paras = %d, but the file actually has %d paragraphs", int(paras), got)
	}
	if doc.Paras()[0].Style != "Heading1" {
		t.Errorf("paras[0].Style = %q, want Heading1", doc.Paras()[0].Style)
	}
}

// TestDocxWrite_TitleBecomesLeadingHeading pins that the optional title
// argument reaches docx.WriteOptions.Title (rendered as a leading Heading1
// paragraph — see pkg/docx.WriteOptions's doc comment).
func TestDocxWrite_TitleBecomesLeadingHeading(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.docx")
	_, err := callDocxWrite(t, map[string]any{"path": p, "markdown": "body\n", "title": "My Title"})
	if err != nil {
		t.Fatalf("DocxWriteHandler: %v", err)
	}
	doc, err := docx.OpenDocument(p)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	paras := doc.Paras()
	if len(paras) == 0 || paras[0].Style != "Heading1" {
		t.Fatalf("paras[0] = %+v, want a leading Heading1", paras)
	}
	var text strings.Builder
	for _, r := range paras[0].Runs {
		text.WriteString(r.Text)
	}
	if text.String() != "My Title" {
		t.Errorf("leading heading text = %q, want %q", text.String(), "My Title")
	}
}

// TestDocxWrite_EmptyMarkdownProducesAValidDocument pins the self-review
// question "does an empty markdown argument behave sensibly?": rather than
// erroring or producing an unopenable file, it must produce the same
// single-empty-paragraph document pkg/docx.WriteDocx itself defines for
// empty input.
func TestDocxWrite_EmptyMarkdownProducesAValidDocument(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.docx")
	res, err := callDocxWrite(t, map[string]any{"path": p, "markdown": ""})
	if err != nil {
		t.Fatalf("DocxWriteHandler with empty markdown: %v", err)
	}
	out := decodeRead(t, res)
	paras, _ := out["paras"].(float64)
	if paras != 1 {
		t.Errorf("paras = %v, want 1 for empty input", out["paras"])
	}
	if _, err := docx.OpenDocument(p); err != nil {
		t.Fatalf("the written file cannot be reopened: %v", err)
	}
}

// TestDocxWrite_RefusesToOverwriteAnExistingFile pins the brief's "no
// backup — creating never overwrites" requirement: the underlying refusal
// from pkg/docx.WriteDocx must surface as a clear, actionable error, and the
// pre-existing file's content must survive untouched.
func TestDocxWrite_RefusesToOverwriteAnExistingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.docx")
	if err := os.WriteFile(p, []byte("existing content"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := callDocxWrite(t, map[string]any{"path": p, "markdown": "# H\n"})
	if err == nil {
		t.Fatal("docx_write overwrote an existing file; creating must not destroy")
	}
	lower := strings.ToLower(err.Error())
	if !strings.Contains(lower, "overwrite") && !strings.Contains(lower, "exist") {
		t.Errorf("error does not clearly explain the refusal: %v", err)
	}
	got, readErr := os.ReadFile(p)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "existing content" {
		t.Error("the existing file's content was modified despite the refusal")
	}
}

// TestDocxWrite_NotesSurfaceUnsupportedSyntax pins the brief's "notes is how
// the model learns that, say, an image was not rendered — it can only relay
// what the result says": the tool layer must pass pkg/docx's Notes through
// verbatim, not swallow them.
func TestDocxWrite_NotesSurfaceUnsupportedSyntax(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.docx")
	res, err := callDocxWrite(t, map[string]any{
		"path":     p,
		"markdown": "before ![alt](pic.png) after\n",
	})
	if err != nil {
		t.Fatalf("DocxWriteHandler: %v", err)
	}
	out := decodeRead(t, res)
	notes, _ := out["notes"].([]any)
	joined := fmt.Sprintf("%v", notes)
	if !strings.Contains(joined, "image") {
		t.Errorf("notes do not mention the unsupported image: %v", notes)
	}
}

// TestDocxWrite_SupportedOnlyInputHasNoNotes pins that fully-supported
// markdown produces no notes field at all in the JSON (omitempty), so a
// model does not have to parse an empty array to conclude "nothing was
// dropped".
func TestDocxWrite_SupportedOnlyInputHasNoNotes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.docx")
	res, err := callDocxWrite(t, map[string]any{"path": p, "markdown": "# H\n\nbody **bold**\n"})
	if err != nil {
		t.Fatalf("DocxWriteHandler: %v", err)
	}
	out := decodeRead(t, res)
	if _, present := out["notes"]; present {
		t.Errorf("notes present for fully-supported input: %v", out["notes"])
	}
}

// TestDocxWrite_CustomFontsReachStylesXML pins the docx-chinese-typography
// plan's Part A at the tool layer: docx_write's four font arguments
// (body_latin_font, body_east_asia_font, code_latin_font,
// code_east_asia_font) must actually reach pkg/docx.WriteOptions and land in
// the generated file's styles.xml -- not just be accepted and silently
// dropped by the schema/handler plumbing.
func TestDocxWrite_CustomFontsReachStylesXML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.docx")
	_, err := callDocxWrite(t, map[string]any{
		"path":                p,
		"markdown":            "body\n\n```\ncode\n```\n",
		"body_latin_font":     "Georgia",
		"body_east_asia_font": "宋体",
		"code_latin_font":     "Cascadia Code",
		"code_east_asia_font": "NSimSun",
	})
	if err != nil {
		t.Fatalf("DocxWriteHandler: %v", err)
	}
	d, err := docx.OpenDocument(p)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	styles, ok := d.Part("word/styles.xml")
	if !ok {
		t.Fatal("styles.xml missing")
	}
	s := string(styles)
	if !strings.Contains(s, `<w:rFonts w:ascii="Georgia" w:eastAsia="宋体"/>`) {
		t.Errorf("styles.xml docDefaults does not carry the custom body fonts: %s", s)
	}
	if !strings.Contains(s, `w:ascii="Cascadia Code" w:eastAsia="NSimSun"`) {
		t.Errorf("styles.xml's SourceCode/VerbatimChar does not carry the custom code fonts: %s", s)
	}
}

// TestDocxWrite_FontArgsAreOptional pins the other half of "each falls back
// to the current default when empty" at the tool layer: omitting all four
// font arguments must still produce a valid, openable document using this
// package's own defaults, not an error.
func TestDocxWrite_FontArgsAreOptional(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.docx")
	_, err := callDocxWrite(t, map[string]any{"path": p, "markdown": "# H\n"})
	if err != nil {
		t.Fatalf("DocxWriteHandler without any font argument: %v", err)
	}
	if _, err := docx.OpenDocument(p); err != nil {
		t.Fatalf("the written file cannot be reopened: %v", err)
	}
}

// TestDocxWriteTool_SchemaExposesFontParameters pins that the four font
// knobs are actually discoverable by a model reading the tool's schema, not
// only usable by a caller who already knows their names from source code.
func TestDocxWriteTool_SchemaExposesFontParameters(t *testing.T) {
	props, ok := DocxWriteTool().InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatal("docx_write InputSchema has no properties map")
	}
	for _, key := range []string{"body_latin_font", "body_east_asia_font", "code_latin_font", "code_east_asia_font"} {
		if _, present := props[key]; !present {
			t.Errorf("docx_write InputSchema.properties is missing %q", key)
		}
	}
}

// TestDocxWrite_IsInDocumentGroupAndNotParallelSafe mirrors
// TestDocxFormat_IsInDocumentGroupAndNotParallelSafe: docx_write writes a
// new file, so it must never run in parallel with another tool call, and it
// must be selectable by the document-editor subagent profile's group-based
// tool selection.
func TestDocxWrite_IsInDocumentGroupAndNotParallelSafe(t *testing.T) {
	tool := DocxWriteTool()
	if tool.ParallelSafe {
		t.Error("docx_write.ParallelSafe = true, want false; it writes to disk")
	}
	found := false
	for _, g := range tool.Groups {
		if g == "document" {
			found = true
		}
	}
	if !found {
		t.Errorf("docx_write.Groups = %v, want it to contain %q", tool.Groups, "document")
	}
}

// TestDocxTools_IncludesWrite pins that DocxWriteTool is actually wired into
// DocxTools(), not just defined and forgotten — the same failure mode design
// §4's "汇总注册" section warns about for every docx tool added after P1.
func TestDocxTools_IncludesWrite(t *testing.T) {
	found := false
	for _, tool := range DocxTools() {
		if tool.Name == "docx_write" {
			found = true
		}
	}
	if !found {
		t.Fatal("DocxTools() does not include docx_write")
	}
}

// TestDocxWriteTool_DescriptionConveysTheMarkdownSubset is the self-review
// question turned into a pinned test: a model deciding whether to write a
// design document with this tool, or fall back to a script, reads only this
// description. It must name the specific constructs supported (headings,
// lists, tables, code, links) rather than reading as "probably just plain
// paragraphs" — the exact ambiguity the brief says has already caused a
// fallback to a script twice in this project.
func TestDocxWriteTool_DescriptionConveysTheMarkdownSubset(t *testing.T) {
	d := DocxWriteTool().Description
	lower := strings.ToLower(d)
	for _, term := range []string{"heading", "list", "table", "code", "link"} {
		if !strings.Contains(lower, term) {
			t.Errorf("docx_write description does not mention %q: %q", term, d)
		}
	}
	if !strings.Contains(lower, "image") {
		t.Errorf("docx_write description does not say images are unsupported: %q", d)
	}
	if !strings.Contains(lower, "overwrite") && !strings.Contains(lower, "exist") {
		t.Errorf("docx_write description does not mention the refuse-to-overwrite behavior: %q", d)
	}
}

// TestDocxWriteTool_DescriptionTeachesTheFileRoute is the self-review
// question turned into a pinned test, for P2c's own motivating bug: a
// design document's markdown exceeded the model's output budget, truncating
// the streamed docx_write tool call and failing it with "invalid arguments
// JSON: unexpected end of JSON input". The model that just hit that error
// has only this description to learn from before its very next attempt, so
// it must name markdown_path, name write_file as how to build the file, and
// name append (the mechanism that lets the file grow past one response's
// output budget) — not just declare the parameter exists in the schema.
func TestDocxWriteTool_DescriptionTeachesTheFileRoute(t *testing.T) {
	d := DocxWriteTool().Description
	lower := strings.ToLower(d)
	for _, term := range []string{"markdown_path", "write_file", "append"} {
		if !strings.Contains(lower, term) {
			t.Errorf("docx_write description does not mention %q; a model that was just truncated cannot discover the file route: %q", term, d)
		}
	}
}

// TestDocxWrite_MarkdownPathMatchesInlineMarkdownByteForByte pins the core
// correctness claim of markdown_path: WriteDocx is deterministic (per the
// brief), so reading the same markdown from a file must produce byte-
// identical output to passing it inline. This is the test that would catch
// a markdown_path implementation that, say, re-encoded line endings on read
// or passed the file's path instead of its contents into WriteOptions.
func TestDocxWrite_MarkdownPathMatchesInlineMarkdownByteForByte(t *testing.T) {
	md := "# Chapter\n\nbody text with **bold** and `code`\n\n- item one\n- item two\n\n" +
		"| a | b |\n|---|---|\n| 1 | 2 |\n\n> a quote\n\n---\n"

	inlinePath := filepath.Join(t.TempDir(), "inline.docx")
	if _, err := callDocxWrite(t, map[string]any{"path": inlinePath, "markdown": md}); err != nil {
		t.Fatalf("inline markdown call: %v", err)
	}

	mdFile := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(mdFile, []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	viaPathOut := filepath.Join(t.TempDir(), "via_path.docx")
	if _, err := callDocxWrite(t, map[string]any{"path": viaPathOut, "markdown_path": mdFile}); err != nil {
		t.Fatalf("markdown_path call: %v", err)
	}

	inlineBytes, err := os.ReadFile(inlinePath)
	if err != nil {
		t.Fatal(err)
	}
	viaPathBytes, err := os.ReadFile(viaPathOut)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(inlineBytes, viaPathBytes) {
		t.Error("markdown_path produced a different .docx than the equivalent inline markdown")
	}
}

// TestDocxWrite_RefusesBothMarkdownAndMarkdownPath pins the brief's "giving
// both... is an error naming what to do": a caller sending both must be
// refused before any file is touched, with a message telling it to pick one.
func TestDocxWrite_RefusesBothMarkdownAndMarkdownPath(t *testing.T) {
	mdFile := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(mdFile, []byte("# H\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "out.docx")
	_, err := callDocxWrite(t, map[string]any{
		"path": p, "markdown": "# H\n", "markdown_path": mdFile,
	})
	if err == nil {
		t.Fatal("giving both markdown and markdown_path returned nil error")
	}
	if !strings.Contains(err.Error(), "markdown") || !strings.Contains(err.Error(), "markdown_path") {
		t.Errorf("error = %q, want it to name both markdown and markdown_path", err)
	}
	if _, statErr := os.Stat(p); statErr == nil {
		t.Error("a rejected call must not have created the file")
	}
}

// TestDocxWrite_RefusesNeitherMarkdownNorMarkdownPath is the other half:
// omitting both must also be refused with a message naming the fix, not
// just "markdown is required" as if markdown_path did not exist.
func TestDocxWrite_RefusesNeitherMarkdownNorMarkdownPath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.docx")
	_, err := callDocxWrite(t, map[string]any{"path": p})
	if err == nil {
		t.Fatal("giving neither markdown nor markdown_path returned nil error")
	}
	if !strings.Contains(err.Error(), "markdown") || !strings.Contains(err.Error(), "markdown_path") {
		t.Errorf("error = %q, want it to name both markdown and markdown_path", err)
	}
	if _, statErr := os.Stat(p); statErr == nil {
		t.Error("a rejected call must not have created the file")
	}
}

// TestDocxWrite_MarkdownPathMissingFileGivesClearError pins the brief's "A
// missing markdown_path file gives a clear error": the underlying os.Open
// failure must be surfaced, not swallowed into a generic docx_write error,
// and the missing path itself must appear so the caller can act on it.
func TestDocxWrite_MarkdownPathMissingFileGivesClearError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does_not_exist.md")
	p := filepath.Join(t.TempDir(), "out.docx")
	_, err := callDocxWrite(t, map[string]any{"path": p, "markdown_path": missing})
	if err == nil {
		t.Fatal("a missing markdown_path file returned nil error")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %q, want it to name the missing path %q", err, missing)
	}
	if _, statErr := os.Stat(p); statErr == nil {
		t.Error("a rejected call must not have created the file")
	}
}

// TestDocxWrite_MarkdownPathBlankIsRefused pins that a whitespace-only
// markdown_path is treated as "not given" for the type-check but still
// refused outright — a blank file path can never be a deliberate request the
// way empty inline markdown is (see TestDocxWrite_EmptyMarkdownProducesAValidDocument).
func TestDocxWrite_MarkdownPathBlankIsRefused(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.docx")
	if _, err := callDocxWrite(t, map[string]any{"path": p, "markdown_path": "   "}); err == nil {
		t.Fatal("a whitespace-only markdown_path returned nil error")
	}
}

// TestDocxWrite_MarkdownPathWrongTypeIsRejected mirrors
// TestDocxWrite_RejectsWrongTypedArgs for the new argument: a non-string
// markdown_path must error, never silently coerce to "".
func TestDocxWrite_MarkdownPathWrongTypeIsRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.docx")
	if _, err := callDocxWrite(t, map[string]any{"path": p, "markdown_path": 12345.0}); err == nil {
		t.Fatal("markdown_path as a number returned nil error")
	}
	if _, statErr := os.Stat(p); statErr == nil {
		t.Error("a rejected call must not have created the file")
	}
}
