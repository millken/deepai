package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/docx"
	"github.com/millken/deepai/pkg/models"
)

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
	// Each call returns a payload proportional to the budget, so the loop has
	// to halve twice before it fits.
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
	if len(budgets) < 2 {
		t.Fatalf("budgets tried = %v, want the loop to shrink at least once", budgets)
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
	const diagnostic = "paragraph 7 is 99999 rendered chars, exceeding the 4096-char MaxChars budget; returned whole so the read cursor still advances"
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
func TestDocxEdit_StyleIsRejectedNotIgnored(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	_, err := callDocxEdit(t, map[string]any{
		"path":  p,
		"edits": []any{map[string]any{"para": float64(2), "text": "x", "style": "Heading2"}},
	})
	if err == nil {
		t.Fatal("style was accepted; want an explicit error")
	}
	if !strings.Contains(err.Error(), "docx_format") {
		t.Errorf("error = %q, want it to point at docx_format", err)
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

// TestDocxFormat_PageNumbersErrorsExplicitly pins design §4.3's requirement
// directly: page_numbers is Tier 3/P3 (needs a new footer part, a
// sectPr/footerReference, a Content_Types entry, and a rels entry — none of
// which this tool can create), and requesting it must return an explicit
// error, never be silently dropped while the rest of the rules apply.
func TestDocxFormat_PageNumbersErrorsExplicitly(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	original, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = callDocxFormat(t, map[string]any{
		"path":  p,
		"rules": map[string]any{"body_font": "Georgia", "page_numbers": true},
	})
	if err == nil {
		t.Fatal("page_numbers=true was accepted; want an explicit error")
	}
	if !strings.Contains(err.Error(), "page_numbers") {
		t.Errorf("error = %q, want it to name page_numbers", err)
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Error("a call that errors on page_numbers must not apply any of the other rules in the same batch either")
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
