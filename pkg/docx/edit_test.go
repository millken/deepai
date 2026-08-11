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
