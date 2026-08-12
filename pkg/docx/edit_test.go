package docx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// editableDoc copies outline.docx (which has no revision marks) into a temp
// dir and opens it, so edit tests can mutate freely.
func editableDoc(t *testing.T) *Document {
	t.Helper()
	data, err := os.ReadFile(outlineFixture)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "edit.docx")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := OpenDocument(p)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	return d
}

// strp returns a pointer to s, for building Edit.Find values (a *string,
// deliberately, so tests can distinguish "not given" (nil) from "given as
// an empty string" (a non-nil pointer to "") — see the Find field's doc
// comment).
func strp(s string) *string {
	return &s
}

func paraTextAt(t *testing.T, d *Document, idx int) string {
	t.Helper()
	var b strings.Builder
	for _, r := range d.Paras()[idx-1].Runs {
		b.WriteString(r.Text)
	}
	return b.String()
}

func TestEdit_RefusesDocumentWithExistingRevisions(t *testing.T) {
	d, err := OpenDocument(fixture) // structure.docx has w:ins / w:del
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	_, err = d.Edit([]Edit{{Para: 1, Text: "x"}}, EditOptions{})
	if err == nil {
		t.Fatal("Edit on a document with revision marks returned nil error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "revision") {
		t.Errorf("error = %q, want it to mention revisions", err)
	}
}

func TestEdit_FindReplacesOnlyTheMatch(t *testing.T) {
	d := editableDoc(t)
	before := paraTextAt(t, d, 2)
	if !strings.Contains(before, "Body") {
		t.Fatalf("fixture paragraph 2 = %q, expected it to contain %q", before, "Body")
	}
	res, err := d.Edit([]Edit{{Para: 2, Find: strp("Body"), Text: "BODY"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("Applied = %d, want 1; outcome = %+v", res.Applied, res.Outcomes[0])
	}
	after := paraTextAt(t, d, 2)
	if !strings.Contains(after, "BODY") {
		t.Errorf("paragraph 2 = %q, want it to contain BODY", after)
	}
	if strings.Replace(before, "Body", "BODY", 1) != after {
		t.Errorf("more than the match changed:\n before %q\n after  %q", before, after)
	}
}

func TestEdit_FindNotFoundIsRefusedNotGuessed(t *testing.T) {
	d := editableDoc(t)
	res, err := d.Edit([]Edit{{Para: 2, Find: strp("no such text"), Text: "x"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 0 || res.Outcomes[0].Applied {
		t.Fatal("a non-matching find was applied")
	}
	if !strings.Contains(res.Outcomes[0].Reason, "0") && !strings.Contains(res.Outcomes[0].Reason, "not found") {
		t.Errorf("Reason = %q, want it to state the match count", res.Outcomes[0].Reason)
	}
}

func TestEdit_FindMatchingTwiceIsRefused(t *testing.T) {
	d := editableDoc(t)
	// Build a deterministic two-match case.
	if _, err := d.Edit([]Edit{{Para: 2, Text: "dup dup"}}, EditOptions{}); err != nil {
		t.Fatalf("setup Edit: %v", err)
	}
	res, err := d.Edit([]Edit{{Para: 2, Find: strp("dup"), Text: "x"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 0 {
		t.Fatal("an ambiguous find was applied")
	}
	// A bare Contains(Reason, "2") is satisfied by the substring "paragraph
	// 2" regardless of the reported match count — a wrong count like
	// "matched 5 times in paragraph 2" would pass it too. Assert the exact
	// count phrase instead.
	if !strings.Contains(res.Outcomes[0].Reason, "matched 2 times") {
		t.Errorf("Reason = %q, want it to state the match count precisely (\"matched 2 times\")", res.Outcomes[0].Reason)
	}
}

func TestEdit_WholeParagraphReplaceWarnsOnMultiRun(t *testing.T) {
	d := editableDoc(t)
	// The fixture's "Plain bold tail" paragraph is the only multi-run one.
	// Locate it rather than hardcoding an index, so the test fails loudly if
	// the fixture layout changes instead of silently testing a single-run
	// paragraph (which would make the warning assertion vacuous).
	target := 0
	for _, p := range d.Paras() {
		if len(p.Runs) > 1 {
			target = p.Index
			break
		}
	}
	if target == 0 {
		t.Fatal("fixture has no multi-run paragraph; this test would be vacuous")
	}

	res, err := d.Edit([]Edit{{Para: target, Text: "flattened"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("whole-paragraph replace was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[0].Warning == "" {
		t.Error("multi-run whole-paragraph replace produced no warning about flattened formatting")
	}
}

// TestEdit_SingleRunReplaceDoesNotWarn is the negative half: the warning must
// be specific to multi-run paragraphs, not emitted for every whole-paragraph
// replace.
func TestEdit_SingleRunReplaceDoesNotWarn(t *testing.T) {
	d := editableDoc(t)
	target := 0
	for _, p := range d.Paras() {
		if len(p.Runs) == 1 {
			target = p.Index
			break
		}
	}
	if target == 0 {
		t.Fatal("fixture has no single-run paragraph")
	}
	res, err := d.Edit([]Edit{{Para: target, Text: "replaced"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Outcomes[0].Warning != "" {
		t.Errorf("single-run replace warned unnecessarily: %q", res.Outcomes[0].Warning)
	}
}

// TestEdit_FindSpanningRunsIsRefused pins the P1 limitation: a match crossing
// run boundaries needs coordinated multi-patch editing, which is P2 work.
func TestEdit_FindSpanningRunsIsRefused(t *testing.T) {
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
	// "Plain bold tail" — this substring straddles run 1 and run 2.
	res, err := d.Edit([]Edit{{Para: target, Find: strp("Plain bold"), Text: "x"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 0 {
		t.Fatal("a find spanning two runs was applied, want refusal")
	}
	if !strings.Contains(res.Outcomes[0].Reason, "run") {
		t.Errorf("Reason = %q, want it to explain the cross-run limitation", res.Outcomes[0].Reason)
	}
}

func TestEdit_InsertRejectsRunAndFind(t *testing.T) {
	d := editableDoc(t)
	for _, e := range []Edit{
		{Para: 2, Op: "insert_after", Run: 1, Text: "x"},
		{Para: 2, Op: "insert_after", Find: strp("Body"), Text: "x"},
	} {
		res, err := d.Edit([]Edit{e}, EditOptions{})
		if err != nil {
			t.Fatalf("Edit: %v", err)
		}
		if res.Outcomes[0].Applied {
			t.Errorf("%+v was applied, want refusal", e)
		}
	}
}

func TestEdit_InsertAfterAddsTheParagraphBelow(t *testing.T) {
	d := editableDoc(t)
	total := d.TotalParas()
	original2 := paraTextAt(t, d, 2)

	res, err := d.Edit([]Edit{{Para: 2, Op: "insert_after", Text: "inserted"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("Applied = %d, want 1: %s", res.Applied, res.Outcomes[0].Reason)
	}
	if d.TotalParas() != total+1 {
		t.Fatalf("TotalParas = %d, want %d", d.TotalParas(), total+1)
	}
	if got := paraTextAt(t, d, 2); got != original2 {
		t.Errorf("paragraph 2 = %q, want the original %q to stay put", got, original2)
	}
	if got := paraTextAt(t, d, 3); got != "inserted" {
		t.Errorf("paragraph 3 = %q, want %q", got, "inserted")
	}
}

// TestEdit_InsertBeforeAddsTheParagraphAbove is a separate test on purpose:
// insert_before anchors at Para.Span.Start while insert_after anchors at
// Para.Span.End, so they are distinct code paths. Testing only one would let
// a mis-wired anchor through unnoticed.
func TestEdit_InsertBeforeAddsTheParagraphAbove(t *testing.T) {
	d := editableDoc(t)
	total := d.TotalParas()
	original2 := paraTextAt(t, d, 2)

	res, err := d.Edit([]Edit{{Para: 2, Op: "insert_before", Text: "inserted"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("Applied = %d, want 1: %s", res.Applied, res.Outcomes[0].Reason)
	}
	if d.TotalParas() != total+1 {
		t.Fatalf("TotalParas = %d, want %d", d.TotalParas(), total+1)
	}
	if got := paraTextAt(t, d, 2); got != "inserted" {
		t.Errorf("paragraph 2 = %q, want %q", got, "inserted")
	}
	// The paragraph that was at 2 must have shifted down to 3. Without this
	// assertion an insert_before wired to Span.End would still look right if
	// the caller only checked the new text's presence.
	if got := paraTextAt(t, d, 3); got != original2 {
		t.Errorf("paragraph 3 = %q, want the displaced original %q", got, original2)
	}
}

func TestEdit_DeleteParagraphRemovesIt(t *testing.T) {
	d := editableDoc(t)
	total := d.TotalParas()
	target := paraTextAt(t, d, 2)
	res, err := d.Edit([]Edit{{Para: 2, Op: "delete"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("Applied = %d, want 1: %s", res.Applied, res.Outcomes[0].Reason)
	}
	if d.TotalParas() != total-1 {
		t.Errorf("TotalParas = %d, want %d", d.TotalParas(), total-1)
	}
	if paraTextAt(t, d, 2) == target {
		t.Error("the deleted paragraph is still present")
	}
}

// TestEdit_ProtectRefusesWhenAProtectedItemIsAltered is §4.2's core boundary.
func TestEdit_ProtectRefusesWhenAProtectedItemIsAltered(t *testing.T) {
	d := editableDoc(t)
	if _, err := d.Edit([]Edit{{Para: 2, Text: "Release v1.2.3 shipped"}}, EditOptions{}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	res, err := d.Edit(
		[]Edit{{Para: 2, Find: strp("v1.2.3"), Text: "v1.2.4"}},
		EditOptions{Protect: []string{`v\d+\.\d+\.\d+`}},
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 0 {
		t.Fatal("an edit that altered a protected item was applied")
	}
	if !strings.Contains(res.Outcomes[0].Reason, "v1.2.3") {
		t.Errorf("Reason = %q, want it to name the broken protected item", res.Outcomes[0].Reason)
	}
}

func TestEdit_ProtectAllowsEditsThatPreserveTheItem(t *testing.T) {
	d := editableDoc(t)
	if _, err := d.Edit([]Edit{{Para: 2, Text: "Release v1.2.3 shipped"}}, EditOptions{}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	res, err := d.Edit(
		[]Edit{{Para: 2, Find: strp("shipped"), Text: "released"}},
		EditOptions{Protect: []string{`v\d+\.\d+\.\d+`}},
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("a protection-preserving edit was refused: %s", res.Outcomes[0].Reason)
	}
	if !strings.Contains(paraTextAt(t, d, 2), "v1.2.3") {
		t.Error("the protected item did not survive")
	}
}

// TestEdit_DeleteSkipsProtectValidationButWarns is §4.2's explicit carve-out.
func TestEdit_DeleteSkipsProtectValidationButWarns(t *testing.T) {
	d := editableDoc(t)
	if _, err := d.Edit([]Edit{{Para: 2, Text: "Release v1.2.3 shipped"}}, EditOptions{}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	res, err := d.Edit(
		[]Edit{{Para: 2, Op: "delete"}},
		EditOptions{Protect: []string{`v\d+\.\d+\.\d+`}},
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("delete was refused by protect validation: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[0].Warning == "" {
		t.Error("deleting protected content produced no warning")
	}
}

func TestEdit_LiteralProtectItemWhenRegexFails(t *testing.T) {
	d := editableDoc(t)
	if _, err := d.Edit([]Edit{{Para: 2, Text: "build (beta of the tool"}}, EditOptions{}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// "(beta" has an unclosed group, so regexp.Compile fails and the item
	// must fall back to a literal match. Note "[unknown]" would NOT work here:
	// it is a VALID regex (a character class), so it would never exercise the
	// fallback path.
	res, err := d.Edit(
		[]Edit{{Para: 2, Find: strp("(beta"), Text: "stable"}},
		EditOptions{Protect: []string{"(beta"}},
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 0 {
		t.Error("an edit destroying a literal protected item was applied")
	}
}

func TestEdit_BatchAppliesAllIndependentEdits(t *testing.T) {
	d := editableDoc(t)
	res, err := d.Edit([]Edit{
		{Para: 2, Find: strp("Body"), Text: "AAA"},
		{Para: 3, Find: strp("Body"), Text: "BBB"},
	}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 2 {
		t.Fatalf("Applied = %d, want 2 (%+v)", res.Applied, res.Outcomes)
	}
	if !strings.Contains(paraTextAt(t, d, 2), "AAA") || !strings.Contains(paraTextAt(t, d, 3), "BBB") {
		t.Error("not all batched edits landed")
	}
}

func TestEdit_OneRefusalDoesNotBlockOthers(t *testing.T) {
	d := editableDoc(t)
	res, err := d.Edit([]Edit{
		{Para: 2, Find: strp("no such text"), Text: "x"},
		{Para: 3, Find: strp("Body"), Text: "OK"},
	}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("Applied = %d, want 1", res.Applied)
	}
	if res.Outcomes[0].Applied || !res.Outcomes[1].Applied {
		t.Errorf("wrong outcome pattern: %+v", res.Outcomes)
	}
}

func TestEdit_OutOfRangeParaIsRefused(t *testing.T) {
	d := editableDoc(t)
	res, err := d.Edit([]Edit{{Para: 99999, Text: "x"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 0 || res.Outcomes[0].Reason == "" {
		t.Error("an out-of-range paragraph index was not cleanly refused")
	}
}

func TestEdit_RunAndFindTogetherIsRefused(t *testing.T) {
	d := editableDoc(t)
	res, err := d.Edit([]Edit{{Para: 2, Run: 1, Find: strp("Body"), Text: "x"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Applied != 0 {
		t.Error("run and find given together were accepted")
	}
}

// TestEdit_SavePersistsAndPreservesUntouchedEntries ties the layer back to the
// byte-fidelity guarantee.
func TestEdit_SavePersistsAndPreservesUntouchedEntries(t *testing.T) {
	data, err := os.ReadFile(outlineFixture)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "edit.docx")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := OpenDocument(p)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if _, err := d.Edit([]Edit{{Para: 2, Find: strp("Body"), Text: "EDITED"}}, EditOptions{}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if err := d.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	assertEntriesEqual(t, outlineFixture, p, map[string]bool{DocumentPart: true})

	reopened, err := OpenDocument(p)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !strings.Contains(paraTextAt(t, reopened, 2), "EDITED") {
		t.Error("the edit did not survive save/reopen")
	}
}

// ---------------------------------------------------------------------------
// I7 coverage gap 1: Before/After for every op, per design §4.2's table.
// Before this wave, no test anywhere asserted EditOutcome.Before or .After —
// exactly how wave 1's C2 defect (a report that said After == "flat" while
// the document actually read "flatb") survived undetected.
// ---------------------------------------------------------------------------

func TestEditBeforeAfter_ReplaceWholeParagraph(t *testing.T) {
	d := editableDoc(t)
	target := 0
	for _, p := range d.Paras() {
		if len(p.Runs) == 1 {
			target = p.Index
			break
		}
	}
	if target == 0 {
		t.Fatal("fixture has no single-run paragraph")
	}
	before := paraTextAt(t, d, target)

	res, err := d.Edit([]Edit{{Para: target, Text: "replaced whole paragraph"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("replace was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[0].Before != before {
		t.Errorf("Before = %q, want the original paragraph text %q", res.Outcomes[0].Before, before)
	}
	if res.Outcomes[0].After != "replaced whole paragraph" {
		t.Errorf("After = %q, want %q", res.Outcomes[0].After, "replaced whole paragraph")
	}
	// Before/After are computed independently of the patch (see planEdit),
	// so asserting only them — as this test did before this check was
	// added — would pass even if the patch itself were wrong. This is
	// exactly the C2 defect shape: a report that said After == "flat" while
	// the document actually read "flatb".
	if got := paraTextAt(t, d, target); got != "replaced whole paragraph" {
		t.Errorf("paragraph %d = %q, want %q (Outcome.After said %q)", target, got, "replaced whole paragraph", res.Outcomes[0].After)
	}
}

// TestEditBeforeAfter_ReplaceRun also serves as the run-level replace
// success-path coverage (design's recommended granularity had zero
// coverage before this wave — Run only appeared in refusal-case tests).
func TestEditBeforeAfter_ReplaceRun(t *testing.T) {
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
	before := d.Paras()[target-1].Runs[1].Text // run 2: "bold"

	res, err := d.Edit([]Edit{{Para: target, Run: 2, Text: "BOLD"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("run replace was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[0].Before != before {
		t.Errorf("Before = %q, want %q", res.Outcomes[0].Before, before)
	}
	if res.Outcomes[0].After != "BOLD" {
		t.Errorf("After = %q, want %q", res.Outcomes[0].After, "BOLD")
	}
	if got, want := paraTextAt(t, d, target), "Plain BOLD tail"; got != want {
		t.Errorf("paragraph %d = %q, want %q", target, got, want)
	}
}

func TestEditBeforeAfter_ReplaceFind(t *testing.T) {
	d := editableDoc(t)
	res, err := d.Edit([]Edit{{Para: 2, Find: strp("Body"), Text: "BODY"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("find replace was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[0].Before != "Body" {
		t.Errorf("Before = %q, want %q", res.Outcomes[0].Before, "Body")
	}
	if res.Outcomes[0].After != "BODY" {
		t.Errorf("After = %q, want %q", res.Outcomes[0].After, "BODY")
	}
	// See TestEditBeforeAfter_ReplaceWholeParagraph: Before/After alone
	// don't prove the patch itself did the right thing.
	want := "BODY paragraph 1 of section Chapter One."
	if got := paraTextAt(t, d, 2); got != want {
		t.Errorf("paragraph 2 = %q, want %q (Outcome.After said %q)", got, want, res.Outcomes[0].After)
	}
}

func TestEditBeforeAfter_InsertBefore(t *testing.T) {
	d := editableDoc(t)
	res, err := d.Edit([]Edit{{Para: 2, Op: "insert_before", Text: "new para"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("insert_before was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[0].Before != "" {
		t.Errorf("Before = %q, want empty (nothing is replaced by an insert)", res.Outcomes[0].Before)
	}
	if res.Outcomes[0].After != "new para" {
		t.Errorf("After = %q, want %q", res.Outcomes[0].After, "new para")
	}
}

func TestEditBeforeAfter_InsertAfter(t *testing.T) {
	d := editableDoc(t)
	res, err := d.Edit([]Edit{{Para: 2, Op: "insert_after", Text: "new para"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("insert_after was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[0].Before != "" {
		t.Errorf("Before = %q, want empty (nothing is replaced by an insert)", res.Outcomes[0].Before)
	}
	if res.Outcomes[0].After != "new para" {
		t.Errorf("After = %q, want %q", res.Outcomes[0].After, "new para")
	}
}

func TestEditBeforeAfter_DeleteWholeParagraph(t *testing.T) {
	d := editableDoc(t)
	before := paraTextAt(t, d, 2)

	res, err := d.Edit([]Edit{{Para: 2, Op: "delete"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("delete was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[0].Before != before {
		t.Errorf("Before = %q, want the removed paragraph's original text %q", res.Outcomes[0].Before, before)
	}
	if res.Outcomes[0].After != "" {
		t.Errorf("After = %q, want empty", res.Outcomes[0].After)
	}
}

// TestEditBeforeAfter_DeleteRun also serves as the run-level delete
// success-path coverage.
func TestEditBeforeAfter_DeleteRun(t *testing.T) {
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

	res, err := d.Edit([]Edit{{Para: target, Run: 2, Op: "delete"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("run delete was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[0].Before != "bold" {
		t.Errorf("Before = %q, want %q", res.Outcomes[0].Before, "bold")
	}
	if res.Outcomes[0].After != "" {
		t.Errorf("After = %q, want empty", res.Outcomes[0].After)
	}
	if got, want := paraTextAt(t, d, target), "Plain  tail"; got != want {
		t.Errorf("paragraph %d = %q, want %q", target, got, want)
	}
}

func TestEditBeforeAfter_DeleteFind(t *testing.T) {
	d := editableDoc(t)
	res, err := d.Edit([]Edit{{Para: 2, Find: strp("Body"), Op: "delete"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("find delete was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[0].Before != "Body" {
		t.Errorf("Before = %q, want %q", res.Outcomes[0].Before, "Body")
	}
	if res.Outcomes[0].After != "" {
		t.Errorf("After = %q, want empty", res.Outcomes[0].After)
	}
	// Unlike the other two Before/After tests above, delete-find's document
	// effect was pinned NOWHERE before this assertion: this is the gap the
	// C2 defect shape could hide in undetected.
	want := " paragraph 1 of section Chapter One."
	if got := paraTextAt(t, d, 2); got != want {
		t.Errorf("paragraph 2 = %q, want %q (Outcome.After said the match was removed)", got, want)
	}
}

// ---------------------------------------------------------------------------
// I7 coverage gap 3: previously-unpinned branches.
// ---------------------------------------------------------------------------

func TestEdit_RunTargetSelfClosingIsRefused(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t/></w:r><w:r><w:t>real</w:t></w:r></w:p>`)
	if !d.Paras()[0].Runs[0].SelfClosing {
		t.Fatal("test setup did not produce a self-closing run")
	}
	res, err := d.Edit([]Edit{
		{Para: 1, Run: 1, Text: "x"},
		{Para: 1, Run: 1, Op: "delete"},
	}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	for i, outcome := range res.Outcomes {
		if outcome.Applied {
			t.Errorf("edit %d targeting a self-closing run was applied", i)
		}
		if !strings.Contains(outcome.Reason, "self-closing") {
			t.Errorf("edit %d Reason = %q, want it to mention self-closing", i, outcome.Reason)
		}
	}
}

func TestEdit_WholeParagraphReplaceRefusedWhenFirstRunIsSelfClosing(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t/></w:r></w:p>`)
	res, err := d.Edit([]Edit{{Para: 1, Text: "x"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Outcomes[0].Applied {
		t.Error("whole-paragraph replace with a self-closing first run was applied")
	}
	if !strings.Contains(res.Outcomes[0].Reason, "self-closing") {
		t.Errorf("Reason = %q, want it to mention self-closing", res.Outcomes[0].Reason)
	}
}

func TestEdit_UnknownOpIsRefused(t *testing.T) {
	d := editableDoc(t)
	res, err := d.Edit([]Edit{{Para: 2, Op: "reticulate", Text: "x"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Outcomes[0].Applied {
		t.Error("an edit with an unknown op was applied")
	}
	if !strings.Contains(res.Outcomes[0].Reason, "unknown op") {
		t.Errorf("Reason = %q, want it to mention the unknown op", res.Outcomes[0].Reason)
	}
}

// TestEdit_ExplicitEmptyFindIsRefused pins the minor fix: a Find pointing at
// "" cannot usefully match anything, and since a JSON tool schema decoded
// into a plain string field cannot tell "find omitted" from "find sent as
// empty", the two must behave differently at the *string layer where the
// distinction still exists.
func TestEdit_ExplicitEmptyFindIsRefused(t *testing.T) {
	d := editableDoc(t)
	res, err := d.Edit([]Edit{{Para: 2, Find: strp(""), Text: "x"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Outcomes[0].Applied {
		t.Error("an edit with an explicit empty Find was applied")
	}
	// Without the dedicated check, planFindTarget's own count != 1 guard
	// would still refuse this (strings.Count(text, "") is len(text)+1), but
	// with a confusing "matched N times" message instead of a reason that
	// actually explains an empty find is meaningless. Assert the specific,
	// clear reason rather than just "some non-empty Reason".
	if !strings.Contains(res.Outcomes[0].Reason, "empty string") {
		t.Errorf("Reason = %q, want it to clearly explain that find was an explicit empty string", res.Outcomes[0].Reason)
	}
}

// TestEdit_NilFindStillDoesWholeParagraphReplace is the negative half: Find
// left nil (the zero value, i.e. genuinely not given) must keep working
// exactly as "" used to before Find became a *string.
func TestEdit_NilFindStillDoesWholeParagraphReplace(t *testing.T) {
	d := editableDoc(t)
	res, err := d.Edit([]Edit{{Para: 2, Text: "flattened"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Errorf("a nil Find (whole-paragraph replace) was refused: %s", res.Outcomes[0].Reason)
	}
}

// ---------------------------------------------------------------------------
// API addition: EditResult.TotalParas / ParaCountChanged.
// ---------------------------------------------------------------------------

func TestEditResult_ParaCountChangedOnInsert(t *testing.T) {
	d := editableDoc(t)
	before := d.TotalParas()
	res, err := d.Edit([]Edit{{Para: 2, Op: "insert_after", Text: "new"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.ParaCountChanged {
		t.Error("ParaCountChanged = false, want true after an insert")
	}
	if res.TotalParas != before+1 {
		t.Errorf("TotalParas = %d, want %d", res.TotalParas, before+1)
	}
	if res.TotalParas != d.TotalParas() {
		t.Errorf("EditResult.TotalParas = %d, want it to match Document.TotalParas() = %d", res.TotalParas, d.TotalParas())
	}
}

func TestEditResult_ParaCountChangedOnWholeParagraphDelete(t *testing.T) {
	d := editableDoc(t)
	before := d.TotalParas()
	res, err := d.Edit([]Edit{{Para: 2, Op: "delete"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.ParaCountChanged {
		t.Error("ParaCountChanged = false, want true after a whole-paragraph delete")
	}
	if res.TotalParas != before-1 {
		t.Errorf("TotalParas = %d, want %d", res.TotalParas, before-1)
	}
}

func TestEditResult_ParaCountUnchangedOnFindReplace(t *testing.T) {
	d := editableDoc(t)
	before := d.TotalParas()
	res, err := d.Edit([]Edit{{Para: 2, Find: strp("Body"), Text: "BODY"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.ParaCountChanged {
		t.Error("ParaCountChanged = true, want false for a find-replace (run-level delete doesn't change para count either)")
	}
	if res.TotalParas != before {
		t.Errorf("TotalParas = %d, want %d (unchanged)", res.TotalParas, before)
	}
}

// ---------------------------------------------------------------------------
// I7 coverage gap 5: nothing previously fed a ParaView.Index from Read into
// an Edit.Para.
// ---------------------------------------------------------------------------

func TestIntegration_ReadThenEditByReportedIndex(t *testing.T) {
	d := editableDoc(t)
	r, err := d.Read(ReadOptions{StartPara: 2, EndPara: 3})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(r.Paras) != 2 {
		t.Fatalf("got %d paragraphs from Read, want 2", len(r.Paras))
	}

	target := r.Paras[len(r.Paras)-1]
	if !strings.Contains(target.Text, "Body") {
		t.Fatalf("test setup: paragraph %d = %q, expected it to contain %q", target.Index, target.Text, "Body")
	}

	res, err := d.Edit([]Edit{{Para: target.Index, Find: strp("Body"), Text: "REVISED"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("edit by the Read-reported index was refused: %s", res.Outcomes[0].Reason)
	}
	if got := paraTextAt(t, d, target.Index); !strings.Contains(got, "REVISED") {
		t.Errorf("paragraph %d = %q, want it to contain REVISED", target.Index, got)
	}

	// The OTHER paragraph Read returned in the same chunk must be untouched.
	for _, pv := range r.Paras {
		if pv.Index == target.Index {
			continue
		}
		if got := paraTextAt(t, d, pv.Index); got != pv.Text {
			t.Errorf("paragraph %d changed unexpectedly: was %q, now %q", pv.Index, pv.Text, got)
		}
	}
}

// ---------------------------------------------------------------------------
// P2b Task 2: TrackChanges wiring.
// ---------------------------------------------------------------------------

// docXMLOf returns d's current word/document.xml bytes, for tests that need
// to inspect the raw markup a tracked-change edit produced rather than just
// the Scan-derived visible text.
func docXMLOf(t *testing.T, d *Document) string {
	t.Helper()
	data, ok := d.pkg.Part(DocumentPart)
	if !ok {
		t.Fatal("document has no document.xml part")
	}
	return string(data)
}

// TestEdit_TrackChangesOffIsByteIdenticalToBeforeThisFeature is the single
// most important regression guard for this task: EditOptions gained three
// new fields (TrackChanges, Author, Now), and every planner gained a branch
// point on rc == nil vs rc != nil. This proves that branch point is a true
// no-op when TrackChanges is false — even when Author/Now are populated
// anyway, which must be silently ignored — by running the SAME batch (one
// of each op) against two fresh copies of the same fixture and diffing
// document.xml byte for byte.
func TestEdit_TrackChangesOffIsByteIdenticalToBeforeThisFeature(t *testing.T) {
	batch := []Edit{
		{Para: 2, Find: strp("Body"), Text: "BODY"},
		{Para: 2, Op: "insert_after", Text: "inserted"},
		{Para: 4, Op: "delete"},
	}

	dOld := editableDoc(t)
	resOld, err := dOld.Edit(batch, EditOptions{})
	if err != nil {
		t.Fatalf("Edit (implicit-off): %v", err)
	}

	dNew := editableDoc(t)
	resNew, err := dNew.Edit(batch, EditOptions{TrackChanges: false, Author: "someone", Now: testNow})
	if err != nil {
		t.Fatalf("Edit (explicit-off with Author/Now set): %v", err)
	}

	if resOld.Applied == 0 || resNew.Applied == 0 {
		t.Fatalf("test setup: batch did not apply (old Applied=%d, new Applied=%d)", resOld.Applied, resNew.Applied)
	}
	if resOld.Applied != resNew.Applied {
		t.Fatalf("Applied differs: old=%d new=%d", resOld.Applied, resNew.Applied)
	}

	oldXML := docXMLOf(t, dOld)
	newXML := docXMLOf(t, dNew)
	if oldXML != newXML {
		t.Errorf("document.xml differs with TrackChanges left at its zero value vs explicitly false with Author/Now set:\nold: %s\nnew: %s", oldXML, newXML)
	}
}

// TestEdit_TrackChanges_ReplaceRun_ProducesDelInsAndPreservesFormatting
// covers the run-target replace row of the plan's table, and doubles as the
// "preserve original formatting" pin (brief's must-have test 5): both the
// <w:del> and <w:ins> sides of a bold run's replace must still carry <w:b/>.
func TestEdit_TrackChanges_ReplaceRun_ProducesDelInsAndPreservesFormatting(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:rPr><w:b/></w:rPr><w:t>old text</w:t></w:r></w:p>`)
	res, err := d.Edit(
		[]Edit{{Para: 1, Run: 1, Text: "new text"}},
		EditOptions{TrackChanges: true, Author: "tester", Now: testNow},
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("tracked run replace was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[0].Before != "old text" || res.Outcomes[0].After != "new text" {
		t.Errorf("Before/After = %q/%q, want %q/%q", res.Outcomes[0].Before, res.Outcomes[0].After, "old text", "new text")
	}

	xml := docXMLOf(t, d)
	if !strings.Contains(xml, "<w:del ") || !strings.Contains(xml, "<w:ins ") {
		t.Fatalf("document.xml does not contain both <w:del> and <w:ins>: %s", xml)
	}
	if !strings.Contains(xml, "<w:delText>old text</w:delText>") {
		t.Errorf("old text was not converted to delText: %s", xml)
	}
	if !strings.Contains(xml, "<w:t>new text</w:t>") {
		t.Errorf("new text was not written as plain w:t inside w:ins: %s", xml)
	}
	if strings.Count(xml, "<w:b/>") != 2 {
		t.Errorf("want <w:b/> preserved on BOTH the del and ins clones, got %d occurrences: %s", strings.Count(xml, "<w:b/>"), xml)
	}

	// Independent self-consistency check, per the task's instructions: reuse
	// the scanner's own visible-text rules (delText excluded, ins included)
	// rather than trusting the XML assertions above alone.
	if got := paraTextAt(t, d, 1); got != "new text" {
		t.Errorf("Scan-derived visible text = %q, want %q", got, "new text")
	}
}

// TestEdit_TrackChanges_DeleteRun_ProducesDelOnly is the delete-side
// counterpart: no <w:ins> should appear at all.
func TestEdit_TrackChanges_DeleteRun_ProducesDelOnly(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>gone</w:t></w:r><w:r><w:t> stays</w:t></w:r></w:p>`)
	res, err := d.Edit(
		[]Edit{{Para: 1, Run: 1, Op: "delete"}},
		EditOptions{TrackChanges: true, Now: testNow},
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("tracked run delete was refused: %s", res.Outcomes[0].Reason)
	}
	xml := docXMLOf(t, d)
	if strings.Contains(xml, "<w:ins ") {
		t.Errorf("a plain delete must not produce any <w:ins>: %s", xml)
	}
	if !strings.Contains(xml, "<w:delText>gone</w:delText>") {
		t.Errorf("deleted text was not converted to delText: %s", xml)
	}
	if got := paraTextAt(t, d, 1); got != " stays" {
		t.Errorf("Scan-derived visible text = %q, want %q (delText excluded)", got, " stays")
	}
}

// TestEdit_TrackChanges_ReplaceFind_SplitsPrefixDelInsSuffix pins the find
// op's four-piece shape, including the "no empty run for an empty
// prefix/suffix" rule: the match sits at the very end of the run's text, so
// there must be no third clone (an empty suffix run) in the output.
func TestEdit_TrackChanges_ReplaceFind_SplitsPrefixDelInsSuffix(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>hello world</w:t></w:r></w:p>`)
	res, err := d.Edit(
		[]Edit{{Para: 1, Find: strp("world"), Text: "there"}},
		EditOptions{TrackChanges: true, Now: testNow},
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("tracked find replace was refused: %s", res.Outcomes[0].Reason)
	}
	xml := docXMLOf(t, d)
	// "hello " has a trailing space, so cloneRunWithText adds
	// xml:space="preserve" (needsPreserve) — this is not the original run's
	// tag (which had none), it is added because the SPLIT-OUT prefix text
	// itself now ends in whitespace that Word would otherwise collapse.
	if !strings.Contains(xml, `<w:t xml:space="preserve">hello </w:t>`) {
		t.Errorf("prefix run \"hello \" not found intact (with xml:space=\"preserve\" for its trailing space): %s", xml)
	}
	if !strings.Contains(xml, "<w:delText>world</w:delText>") {
		t.Errorf("matched text not converted to delText: %s", xml)
	}
	if !strings.Contains(xml, "<w:t>there</w:t>") {
		t.Errorf("replacement text not found as plain w:t: %s", xml)
	}
	// No empty suffix run: exactly one <w:t...> holds "hello " and one holds
	// "there" — a third, empty <w:t></w:t> would mean an unwanted empty
	// suffix run was emitted.
	if strings.Contains(xml, "<w:t></w:t>") || strings.Contains(xml, `<w:t xml:space="preserve"></w:t>`) {
		t.Errorf("an empty run was emitted for the empty suffix: %s", xml)
	}
	if got := paraTextAt(t, d, 1); got != "hello there" {
		t.Errorf("Scan-derived visible text = %q, want %q", got, "hello there")
	}
}

// TestEdit_TrackChanges_DeleteFind_NoInsertionProduced is the find-delete
// counterpart, and also exercises a match with a non-empty SUFFIX (unlike
// the replace test above, which put the match at the end).
func TestEdit_TrackChanges_DeleteFind_NoInsertionProduced(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>hello world today</w:t></w:r></w:p>`)
	res, err := d.Edit(
		[]Edit{{Para: 1, Find: strp("world "), Op: "delete"}},
		EditOptions{TrackChanges: true, Now: testNow},
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("tracked find delete was refused: %s", res.Outcomes[0].Reason)
	}
	xml := docXMLOf(t, d)
	if strings.Contains(xml, "<w:ins ") {
		t.Errorf("a plain delete must not produce any <w:ins>: %s", xml)
	}
	// "world " (the match) and "hello " (the prefix) both have a trailing
	// space, so both clones pick up xml:space="preserve" — see the analogous
	// comment in TestEdit_TrackChanges_ReplaceFind_SplitsPrefixDelInsSuffix.
	if !strings.Contains(xml, `<w:delText xml:space="preserve">world </w:delText>`) {
		t.Errorf("matched text not converted to delText: %s", xml)
	}
	if !strings.Contains(xml, `<w:t xml:space="preserve">hello </w:t>`) {
		t.Errorf("prefix run not found intact: %s", xml)
	}
	if !strings.Contains(xml, "<w:t>today</w:t>") {
		t.Errorf("suffix run not found intact: %s", xml)
	}
	if got := paraTextAt(t, d, 1); got != "hello today" {
		t.Errorf("Scan-derived visible text = %q, want %q", got, "hello today")
	}
}

// TestEdit_TrackChanges_ParagraphDelete_DoesNotRemoveParagraph is the
// brief's must-have test 3: paragraph count must not change, and
// ParaCountChanged must be false, because the paragraph is marked deleted
// rather than removed.
func TestEdit_TrackChanges_ParagraphDelete_DoesNotRemoveParagraph(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>first</w:t></w:r></w:p><w:p><w:r><w:t>second</w:t></w:r></w:p>`)
	before := d.TotalParas()

	res, err := d.Edit(
		[]Edit{{Para: 1, Op: "delete"}},
		EditOptions{TrackChanges: true, Now: testNow},
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("tracked paragraph delete was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[0].Before != "first" || res.Outcomes[0].After != "" {
		t.Errorf("Before/After = %q/%q, want %q/%q", res.Outcomes[0].Before, res.Outcomes[0].After, "first", "")
	}
	if res.ParaCountChanged {
		t.Error("ParaCountChanged = true, want false for a tracked paragraph delete")
	}
	if res.TotalParas != before {
		t.Errorf("TotalParas = %d, want unchanged %d", res.TotalParas, before)
	}
	if d.TotalParas() != before {
		t.Errorf("Document.TotalParas() = %d, want unchanged %d", d.TotalParas(), before)
	}

	xml := docXMLOf(t, d)
	if !strings.Contains(xml, "<w:delText>first</w:delText>") {
		t.Errorf("paragraph's run text was not converted to delText: %s", xml)
	}
	// The paragraph MARK itself must also be flagged deleted (plan's OOXML
	// shape note 5), or Word would merge this paragraph into its neighbour
	// once the revision is accepted.
	if !strings.Contains(xml, "<w:pPr><w:rPr><w:del ") {
		t.Errorf("paragraph mark was not flagged deleted via <w:pPr><w:rPr><w:del/>: %s", xml)
	}
	// Scan-derived visible text: the deleted paragraph's run text must be
	// gone (delText excluded), while the second paragraph is untouched.
	if got := paraTextAt(t, d, 1); got != "" {
		t.Errorf("paragraph 1 visible text = %q, want empty (delText excluded)", got)
	}
	if got := paraTextAt(t, d, 2); got != "second" {
		t.Errorf("paragraph 2 = %q, want untouched %q", got, "second")
	}
}

// TestEdit_TrackChanges_ParagraphReplace_CollapsesFormattingLikeUntrackedMode
// pins the whole-paragraph replace row: the first run's <w:r> gets
// del(oldText)+ins(newText), every other run's <w:r> gets del(oldText) only,
// and the same "collapsed formatting" Warning fires as in untracked mode.
func TestEdit_TrackChanges_ParagraphReplace_CollapsesFormattingLikeUntrackedMode(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>one</w:t></w:r><w:r><w:rPr><w:i/></w:rPr><w:t>two</w:t></w:r></w:p>`)
	res, err := d.Edit(
		[]Edit{{Para: 1, Text: "flat"}},
		EditOptions{TrackChanges: true, Now: testNow},
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("tracked paragraph replace was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[0].Warning == "" {
		t.Error("multi-run tracked whole-paragraph replace produced no collapsed-formatting warning")
	}
	xml := docXMLOf(t, d)
	if !strings.Contains(xml, "<w:delText>one</w:delText>") || !strings.Contains(xml, "<w:delText>two</w:delText>") {
		t.Errorf("both runs' old text must be converted to delText: %s", xml)
	}
	if strings.Count(xml, "<w:ins ") != 1 {
		t.Errorf("want exactly one <w:ins> (on the first run only), got %d: %s", strings.Count(xml, "<w:ins "), xml)
	}
	if !strings.Contains(xml, "<w:i/>") {
		t.Errorf("second run's <w:i/> formatting was lost: %s", xml)
	}
	if got := paraTextAt(t, d, 1); got != "flat" {
		t.Errorf("Scan-derived visible text = %q, want %q", got, "flat")
	}
}

// TestEdit_TrackChanges_InsertAfter_MarksNewParagraphAndRunAsInserted covers
// the insert_before/insert_after row: the new paragraph is a real <w:p>
// (paragraph count DOES change — nothing here mirrors the paragraph-delete
// carve-out, since nothing pre-existing is being hidden), but both the
// paragraph mark and its run must carry <w:ins/>.
func TestEdit_TrackChanges_InsertAfter_MarksNewParagraphAndRunAsInserted(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>only</w:t></w:r></w:p>`)
	before := d.TotalParas()

	res, err := d.Edit(
		[]Edit{{Para: 1, Op: "insert_after", Text: "brand new"}},
		EditOptions{TrackChanges: true, Author: "tester", Now: testNow},
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("tracked insert_after was refused: %s", res.Outcomes[0].Reason)
	}
	if !res.ParaCountChanged || res.TotalParas != before+1 {
		t.Errorf("ParaCountChanged=%v TotalParas=%d, want true/%d (a real new paragraph was added)", res.ParaCountChanged, res.TotalParas, before+1)
	}

	xml := docXMLOf(t, d)
	if !strings.Contains(xml, "<w:pPr><w:rPr><w:ins ") {
		t.Errorf("new paragraph's mark was not flagged inserted: %s", xml)
	}
	if !strings.Contains(xml, "<w:ins ") || !strings.Contains(xml, "<w:t>brand new</w:t>") {
		t.Errorf("new paragraph's run was not wrapped in <w:ins> holding the inserted text: %s", xml)
	}
	if got := paraTextAt(t, d, 2); got != "brand new" {
		t.Errorf("paragraph 2 visible text = %q, want %q", got, "brand new")
	}
}

// TestEdit_TrackChanges_TwoConsecutiveChunkedEditsBothSucceed is the reason
// this phase exists (brief's must-have test 4): the FIRST tracked edit
// writes real w:ins/w:del into the document and rescans, which flips
// HasRevisions() to true — proving the gate genuinely uses
// hadRevisionsAtOpen (captured once, at OpenDocument) rather than
// HasRevisions() (which would now block every subsequent call). A SECOND
// tracked edit in the same session, after the first has already landed,
// must still succeed.
func TestEdit_TrackChanges_TwoConsecutiveChunkedEditsBothSucceed(t *testing.T) {
	d := editableDoc(t)

	res1, err := d.Edit(
		[]Edit{{Para: 2, Find: strp("Body"), Text: "BODY"}},
		EditOptions{TrackChanges: true, Now: testNow},
	)
	if err != nil {
		t.Fatalf("first tracked Edit: %v", err)
	}
	if !res1.Outcomes[0].Applied {
		t.Fatalf("first tracked edit was refused: %s", res1.Outcomes[0].Reason)
	}
	if !d.HasRevisions() {
		t.Fatal("test setup: HasRevisions() = false after a tracked edit landed; the rest of this test would be vacuous")
	}

	res2, err := d.Edit(
		[]Edit{{Para: 3, Find: strp("Body"), Text: "BODY2"}},
		EditOptions{TrackChanges: true, Now: testNow},
	)
	if err != nil {
		t.Fatalf("second tracked Edit returned an error — the gate is blocking on the first edit's own revisions: %v", err)
	}
	if !res2.Outcomes[0].Applied {
		t.Fatalf("second tracked edit was refused: %s", res2.Outcomes[0].Reason)
	}

	if got := paraTextAt(t, d, 2); !strings.Contains(got, "BODY") {
		t.Errorf("paragraph 2 = %q, want it to contain BODY (first edit's ins)", got)
	}
	if got := paraTextAt(t, d, 3); !strings.Contains(got, "BODY2") {
		t.Errorf("paragraph 3 = %q, want it to contain BODY2 (second edit's ins)", got)
	}

	// Both revisions must have distinct w:id values: newRevisionCtx is
	// rebuilt from the CURRENT document.xml on every Edit call, so the
	// second batch's ids must start above whatever the first batch wrote,
	// never colliding.
	xml := docXMLOf(t, d)
	firstID := strings.Index(xml, `w:id="1"`)
	if firstID == -1 {
		t.Fatalf("expected the first tracked edit's ids to start at 1: %s", xml)
	}
	if strings.Count(xml, `w:id="1"`) != 1 {
		t.Errorf("w:id=\"1\" must be unique across the whole document, found %d occurrences: %s", strings.Count(xml, `w:id="1"`), xml)
	}

	// The document must still be a genuinely valid, reopenable package —
	// the real end-to-end proof that neither batch corrupted anything.
	if err := d.Save(); err != nil {
		t.Fatalf("Save after two chunked tracked edits: %v", err)
	}
	reopened, err := OpenDocument(d.path)
	if err != nil {
		t.Fatalf("saved document is not reopenable: %v", err)
	}

	// This is the part the test used to be missing (the C1/task-3 false
	// positive): everything above ran on the SAME *Document instance for
	// both tracked edits, so d.hadRevisionsAtOpen never actually became true
	// mid-test — that only proved the gate tolerates revisions ADDED after
	// open, never the tool layer's real shape, where every docx_edit call
	// opens a FRESH Document (pkg/tools/builtin/docx.go re-OpenDocuments the
	// path on every call). reopened.hadRevisionsAtOpen IS true here (both
	// prior batches' w:ins/w:del are now on disk), so a third tracked edit
	// on reopened is what actually exercises the author-based gate
	// (edit.go): it must still land, because this batch's author ("deepai",
	// the default both prior batches also used) matches what is already on
	// disk.
	res3, err := reopened.Edit(
		[]Edit{{Para: 6, Find: strp("Body"), Text: "BODY3"}},
		EditOptions{TrackChanges: true, Now: testNow},
	)
	if err != nil {
		t.Fatalf("third tracked Edit on a REOPENED document returned an error — the gate is blocking a same-author reopen: %v", err)
	}
	if !res3.Outcomes[0].Applied {
		t.Fatalf("third tracked edit on a reopened document was refused: %s", res3.Outcomes[0].Reason)
	}
	if got := paraTextAt(t, reopened, 6); !strings.Contains(got, "BODY3") {
		t.Errorf("paragraph 6 = %q, want it to contain BODY3 (third edit's ins, on the reopened document)", got)
	}
}

// TestEdit_TrackChanges_ThreeConsecutiveEditsAllSucceed extends the above to
// three rounds, since two could theoretically pass by coincidence if the
// gate merely tolerated exactly one prior tracked edit rather than genuinely
// decoupling from HasRevisions().
func TestEdit_TrackChanges_ThreeConsecutiveEditsAllSucceed(t *testing.T) {
	d := editableDoc(t)
	targets := []struct {
		para int
		find string
		text string
	}{
		{2, "Body", "BODY-1"},
		{3, "Body", "BODY-2"},
		{6, "Body", "BODY-3"},
	}
	for i, tc := range targets {
		res, err := d.Edit(
			[]Edit{{Para: tc.para, Find: strp(tc.find), Text: tc.text}},
			EditOptions{TrackChanges: true, Now: testNow},
		)
		if err != nil {
			t.Fatalf("round %d: Edit returned an error: %v", i+1, err)
		}
		if !res.Outcomes[0].Applied {
			t.Fatalf("round %d: tracked edit was refused: %s", i+1, res.Outcomes[0].Reason)
		}
	}
	for _, tc := range targets {
		if got := paraTextAt(t, d, tc.para); !strings.Contains(got, tc.text) {
			t.Errorf("paragraph %d = %q, want it to contain %q", tc.para, got, tc.text)
		}
	}
}

// TestEdit_RefusesReopenedDocumentWithOtherAuthorRevisions pins the half of
// the author-based gate that must still refuse: a document reopened from
// disk carrying a DIFFERENT author's pending revisions is not something a
// same-author-default batch may silently build on top of, tracked or not.
// The refusal must name the other author and give an actionable next step
// (per task-3's brief: no "that's not a bug" dead end).
func TestEdit_RefusesReopenedDocumentWithOtherAuthorRevisions(t *testing.T) {
	d := editableDoc(t)
	if _, err := d.Edit(
		[]Edit{{Para: 2, Find: strp("Body"), Text: "BODY"}},
		EditOptions{TrackChanges: true, Author: "reviewer", Now: testNow},
	); err != nil {
		t.Fatalf("setup tracked Edit: %v", err)
	}
	if err := d.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reopened, err := OpenDocument(d.path)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}

	_, err = reopened.Edit([]Edit{{Para: 3, Find: strp("Body"), Text: "BODY2"}}, EditOptions{})
	if err == nil {
		t.Fatal("Edit on a document with another author's pending revisions returned nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "reviewer") {
		t.Errorf("error = %q, want it to name the other author (reviewer)", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "word") {
		t.Errorf("error = %q, want it to point at resolving the pending revisions in Word", msg)
	}
	if !strings.Contains(msg, "author=") {
		t.Errorf("error = %q, want it to give the actionable next step of retrying with a matching author", msg)
	}
	if strings.Contains(strings.ToLower(msg), "not a bug") {
		t.Errorf("error = %q, must not use the unhelpful \"not a bug\" dead-end wording", msg)
	}

	// tracked or not, the same other-author revisions still refuse the batch.
	_, err = reopened.Edit(
		[]Edit{{Para: 3, Find: strp("Body"), Text: "BODY2"}},
		EditOptions{TrackChanges: true, Now: testNow},
	)
	if err == nil {
		t.Fatal("tracked Edit on a document with another author's pending revisions returned nil error")
	}
}

// TestEdit_AllowsReopenedDocumentWhenAuthorMatchesExistingRevisions covers
// the other half: once the user explicitly authorizes continuing as the
// same author the existing revisions carry (the gate's own suggested next
// step), the edit must land — with track_changes on OR off, since an
// untracked "finish this polish, tracking off now" call is exactly the
// third-call shape the tool layer's document-editor profile also needs to
// work.
func TestEdit_AllowsReopenedDocumentWhenAuthorMatchesExistingRevisions(t *testing.T) {
	d := editableDoc(t)
	if _, err := d.Edit(
		[]Edit{{Para: 2, Find: strp("Body"), Text: "BODY"}},
		EditOptions{TrackChanges: true, Now: testNow}, // Author "" -> "deepai"
	); err != nil {
		t.Fatalf("setup tracked Edit: %v", err)
	}
	if err := d.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reopened, err := OpenDocument(d.path)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}

	// Untracked, author left "" -> defaults to "deepai", matching what is
	// already on disk.
	res, err := reopened.Edit([]Edit{{Para: 3, Find: strp("Body"), Text: "BODY2"}}, EditOptions{})
	if err != nil {
		t.Fatalf("untracked Edit on a same-author reopened document returned an error: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("untracked edit was refused: %s", res.Outcomes[0].Reason)
	}
	if got := paraTextAt(t, reopened, 3); !strings.Contains(got, "BODY2") {
		t.Errorf("paragraph 3 = %q, want it to contain BODY2", got)
	}
}

// TestEdit_RefusesWhenTextBoxContainsOtherAuthorRevision is a review-round
// fix: the gate used to trigger on d.hadRevisionsAtOpen, which comes from
// Scan's per-PARAGRAPH HasRevisions flag — and that flag is never set for a
// <w:ins>/<w:del> found INSIDE a <w:txbxContent> subtree, because scan.go's
// ins/del cases are guarded by "&& txbxDepth == 0" (the whole subtree is
// skipped, on purpose, so its duplicated mc:Choice/mc:Fallback text isn't
// indexed twice — see Scan's doc comment). d.revisionAuthorsAtOpen, by
// contrast, comes from scanRevisions' whole-document token walk, which does
// NOT skip text boxes, so it already sees "reviewer" here — the bug was
// gating on the wrong signal, not missing data. Before the fix, this
// document had hadRevisionsAtOpen == false (no paragraph's flag was ever
// set) while revisionAuthorsAtOpen == ["reviewer"], so Edit fell straight
// through the gate and applied the edit even though a human reviewer's
// pending revision — invisible to the paragraph scan, but named in
// docx_read's notes — was sitting in the same file.
func TestEdit_RefusesWhenTextBoxContainsOtherAuthorRevision(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:drawing><wps:txbx><w:txbxContent>`+
		`<w:p><w:ins w:id="1" w:author="reviewer" w:date="2026-01-01T00:00:00Z">`+
		`<w:r><w:t>inside box</w:t></w:r></w:ins></w:p>`+
		`</w:txbxContent></wps:txbx></w:drawing></w:r></w:p>`+
		`<w:p><w:r><w:t>second</w:t></w:r></w:p>`)

	if d.HasRevisions() {
		t.Fatal("test setup: HasRevisions() = true; this test needs the paragraph-level scan to miss the textbox revision for the gate bug to be exercised")
	}
	if len(d.revisionAuthorsAtOpen) == 0 {
		t.Fatal("test setup: revisionAuthorsAtOpen is empty; scanRevisions should have found \"reviewer\" inside the text box")
	}

	_, err := d.Edit([]Edit{{Para: 2, Text: "x"}}, EditOptions{})
	if err == nil {
		t.Fatal("Edit succeeded despite an other-author revision hidden inside a text box — false allow")
	}
	if !strings.Contains(err.Error(), "reviewer") {
		t.Errorf("error = %q, want it to name the other author (reviewer)", err)
	}
}

// TestEdit_RefusesWhenBodyLevelInsWrapsWholeParagraph covers the other shape
// the same bug missed: a <w:ins> that wraps an entire <w:p> as the BODY's
// direct child (rather than a run inside an already-open paragraph). When
// Scan sees this <w:ins> StartElement, inPara is still false (the <w:p>
// hasn't opened yet), so paraHasRevisions is never set for the paragraph
// inside it either — same false-negative on the paragraph-level signal,
// same true-positive on scanRevisions' unconditional whole-document walk.
func TestEdit_RefusesWhenBodyLevelInsWrapsWholeParagraph(t *testing.T) {
	d := bodyDoc(t, `<w:ins w:id="1" w:author="reviewer" w:date="2026-01-01T00:00:00Z">`+
		`<w:p><w:r><w:t>inserted paragraph</w:t></w:r></w:p>`+
		`</w:ins>`+
		`<w:p><w:r><w:t>second</w:t></w:r></w:p>`)

	if d.HasRevisions() {
		t.Fatal("test setup: HasRevisions() = true; this test needs the paragraph-level scan to miss the body-level ins for the gate bug to be exercised")
	}
	if len(d.revisionAuthorsAtOpen) == 0 {
		t.Fatal("test setup: revisionAuthorsAtOpen is empty; scanRevisions should have found \"reviewer\" from the body-level <w:ins>")
	}

	_, err := d.Edit([]Edit{{Para: 2, Text: "x"}}, EditOptions{})
	if err == nil {
		t.Fatal("Edit succeeded despite an other-author revision hidden in a body-level <w:ins><w:p> — false allow")
	}
	if !strings.Contains(err.Error(), "reviewer") {
		t.Errorf("error = %q, want it to name the other author (reviewer)", err)
	}
}

// TestEdit_TrackChanges_RunWithSeveralTextNodesIsRefusedPerEditNotBatch is
// the self-review's most important check: cloneRunWithText refuses a run
// holding more than one <w:t> (see its doc comment — this is exactly the
// shape that caused a silent text-loss defect earlier in this package), and
// that refusal must surface as ONE edit's Reason, never as a whole-batch
// error that also blocks an unrelated edit in the same call.
func TestEdit_TrackChanges_RunWithSeveralTextNodesIsRefusedPerEditNotBatch(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>a</w:t><w:t>b</w:t></w:r><w:r><w:t>other</w:t></w:r></w:p>`)
	para := d.Paras()[0]
	if len(para.Runs) != 3 {
		t.Fatalf("test setup: got %d runs, want 3 (two sharing one <w:r>, plus one more)", len(para.Runs))
	}
	if para.Runs[0].Elem != para.Runs[1].Elem {
		t.Fatal("test setup: runs 1 and 2 do not share an Elem; this test would be vacuous")
	}

	res, err := d.Edit(
		[]Edit{
			{Para: 1, Run: 1, Text: "x"}, // shared-Elem run: must be refused
			{Para: 1, Run: 3, Text: "y"}, // unrelated run: must still succeed
		},
		EditOptions{TrackChanges: true, Now: testNow},
	)
	if err != nil {
		t.Fatalf("Edit returned a whole-batch error instead of a per-edit Reason: %v", err)
	}
	if res.Outcomes[0].Applied {
		t.Error("a tracked edit on a run with several <w:t> children was applied instead of refused")
	}
	if res.Outcomes[0].Reason == "" {
		t.Error("no Reason given for the shared-Elem refusal")
	}
	if !strings.Contains(res.Outcomes[0].Reason, "text-holding") && !strings.Contains(res.Outcomes[0].Reason, "tracked change") {
		t.Errorf("Reason = %q, want it to explain the tracked-change construction failure", res.Outcomes[0].Reason)
	}
	if !res.Outcomes[1].Applied {
		t.Errorf("the unrelated edit was blocked too: %s", res.Outcomes[1].Reason)
	}
	if got := paraTextAt(t, d, 1); !strings.Contains(got, "y") {
		t.Errorf("paragraph 1 = %q, want the unrelated edit's %q to have landed", got, "y")
	}
}

// TestEdit_TrackChanges_ParagraphDeleteOnSharedElemIsRefused extends the
// same shared-Elem refusal to the whole-paragraph delete path
// (wrapParagraphRunsInDel), a separate code path from the run-target one
// above.
func TestEdit_TrackChanges_ParagraphDeleteOnSharedElemIsRefused(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>a</w:t><w:t>b</w:t></w:r></w:p>`)
	res, err := d.Edit(
		[]Edit{{Para: 1, Op: "delete"}},
		EditOptions{TrackChanges: true, Now: testNow},
	)
	if err != nil {
		t.Fatalf("Edit returned a whole-batch error instead of a per-edit Reason: %v", err)
	}
	if res.Outcomes[0].Applied {
		t.Error("a tracked whole-paragraph delete on a shared-Elem run was applied instead of refused")
	}
	if res.Outcomes[0].Reason == "" {
		t.Error("no Reason given for the refusal")
	}
}

// TestEdit_TrackChanges_CollisionDetectionStillWorksWithLargerPatches is the
// self-review's collision-detector check: a tracked patch's NewText is much
// larger than the span it replaces (it now contains <w:del>/<w:ins> markup
// plus cloned <w:rPr>), but Patch.Content — the span collision detection
// actually compares — is still the ORIGINAL run.Elem, unchanged in size by
// TrackChanges. Two finds landing in the SAME run must still collide (only
// the first lands), and two finds in DIFFERENT runs of the same paragraph
// must both land, exactly as in untracked mode (see
// TestEdit_TwoFindsInTheSameRunRefuseOnlyTheLater).
func TestEdit_TrackChanges_CollisionDetectionStillWorksWithLargerPatches(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>hello world</w:t></w:r><w:r><w:t> second run</w:t></w:r></w:p>`)

	// Same run: must collide, refusing only the later edit.
	res, err := d.Edit([]Edit{
		{Para: 1, Find: strp("hello"), Text: "hi"},
		{Para: 1, Find: strp("world"), Text: "there"},
	}, EditOptions{TrackChanges: true, Now: testNow})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Errorf("first tracked edit was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[1].Applied {
		t.Error("second tracked edit colliding with the first (same run) was applied")
	}
	if !strings.Contains(res.Outcomes[1].Reason, "edit 1") {
		t.Errorf("Reason = %q, want it to name edit 1 as the collision", res.Outcomes[1].Reason)
	}

	d2 := bodyDoc(t, `<w:p><w:r><w:t>hello world</w:t></w:r><w:r><w:t> second run</w:t></w:r></w:p>`)
	res2, err := d2.Edit([]Edit{
		{Para: 1, Find: strp("hello"), Text: "hi"},
		{Para: 1, Find: strp("second"), Text: "SECOND"},
	}, EditOptions{TrackChanges: true, Now: testNow})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res2.Outcomes[0].Applied || !res2.Outcomes[1].Applied {
		t.Errorf("two tracked edits on DIFFERENT runs falsely collided: %+v", res2.Outcomes)
	}
	if got := paraTextAt(t, d2, 1); got != "hi world SECOND run" {
		t.Errorf("paragraph 1 = %q, want %q", got, "hi world SECOND run")
	}
}

// TestEdit_TrackChanges_DefaultAuthorIsDeepai pins EditOptions.Author's ""
// -> "deepai" default reaching all the way through to the written XML.
func TestEdit_TrackChanges_DefaultAuthorIsDeepai(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>hi</w:t></w:r></w:p>`)
	res, err := d.Edit(
		[]Edit{{Para: 1, Text: "bye"}},
		EditOptions{TrackChanges: true, Now: testNow}, // Author left ""
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("tracked edit was refused: %s", res.Outcomes[0].Reason)
	}
	xml := docXMLOf(t, d)
	if !strings.Contains(xml, `w:author="deepai"`) {
		t.Errorf(`document.xml = %s, want w:author="deepai" as the default`, xml)
	}
}
