package docx

// Tests for what Edit REPORTS, as distinct from what it does.
//
// An EditOutcome.Reason is read by an LLM, not by a human debugger, so a
// refusal that states a byte offset or offers advice the caller cannot
// follow is a defect even when the refusal itself is correct. These tests
// pin the wording of the collision reasons, that a rolled-back edit leaves
// no trace in the package's dirty-part bookkeeping, and that a malformed
// Text refuses one edit rather than discarding the whole batch.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Item 1: the collision Reason must name the actual boundary and a working
// escape hatch when either colliding span is zero-length, instead of the
// generic "combine them into a single edit" advice a caller cannot follow
// for two different paragraphs.
// ---------------------------------------------------------------------------

// TestEdit_CollisionOfTwoInsertsNamesTheBoundary pins the first named
// example: insert_after(1) and insert_before(2) both anchor at the exact
// same boundary (paragraph 1's end == paragraph 2's start), so the second is
// refused — but the caller named two different paragraphs and cannot combine
// two paragraph insertions into one edit, so the old "combine them into a
// single edit" wording gave no actionable path forward.
func TestEdit_CollisionOfTwoInsertsNamesTheBoundary(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>one</w:t></w:r></w:p><w:p><w:r><w:t>two</w:t></w:r></w:p>`)
	res, err := d.Edit([]Edit{
		{Para: 1, Op: "insert_after", Text: "A"},
		{Para: 2, Op: "insert_before", Text: "B"},
	}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("first insert was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[1].Applied {
		t.Fatal("second, colliding insert was applied")
	}
	reason := res.Outcomes[1].Reason
	if strings.Contains(reason, "combine them into a single edit") {
		t.Errorf("Reason = %q, still gives the un-actionable combine-into-one-edit advice for two different paragraphs", reason)
	}
	if !strings.Contains(reason, "boundary between paragraphs 1 and 2") {
		t.Errorf("Reason = %q, want it to name the boundary between paragraphs 1 and 2", reason)
	}
	if !strings.Contains(reason, "edit 1") {
		t.Errorf("Reason = %q, want it to name edit 1", reason)
	}
	if !strings.Contains(reason, "separate docx_edit call") {
		t.Errorf("Reason = %q, want it to point at issuing a separate docx_edit call", reason)
	}
}

// TestEdit_InsertAfterCollidesWithDeleteOnNeighbourNamesTheBoundary pins the
// second named example: insert_after(1) and delete(2) do not genuinely
// conflict (insert at the paragraph-1/2 boundary, delete a disjoint range
// are both well-defined), but the refusal is forced by Apply's own
// equal-starts rule. The Reason must say so instead of claiming the two
// edits could be "combined".
func TestEdit_InsertAfterCollidesWithDeleteOnNeighbourNamesTheBoundary(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>one</w:t></w:r></w:p><w:p><w:r><w:t>two</w:t></w:r></w:p>`)
	res, err := d.Edit([]Edit{
		{Para: 1, Op: "insert_after", Text: "A"},
		{Para: 2, Op: "delete"},
	}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("insert_after was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[1].Applied {
		t.Fatal("the delete on the neighbouring paragraph was applied")
	}
	reason := res.Outcomes[1].Reason
	if strings.Contains(reason, "combine them into a single edit") {
		t.Errorf("Reason = %q, still gives the combine-into-one-edit advice, which cannot apply to insert+delete", reason)
	}
	if !strings.Contains(reason, "boundary between paragraphs 1 and 2") {
		t.Errorf("Reason = %q, want it to name the boundary between paragraphs 1 and 2", reason)
	}
	if !strings.Contains(reason, "delete on paragraph 2") || !strings.Contains(reason, "insert_after on paragraph 1") {
		t.Errorf("Reason = %q, want it to point at the working delete-then-insert_after escape hatch", reason)
	}
}

// TestEdit_GenuineSameRunOverlapKeepsCombineWording pins that the existing,
// actionable "combine them into a single edit" wording survives for the
// case it is actually true and useful: two find-replaces landing in the same
// run, which really can be expressed as one edit.
func TestEdit_GenuineSameRunOverlapKeepsCombineWording(t *testing.T) {
	d := bodyDoc(t, twoRunPara())
	res, err := d.Edit([]Edit{
		{Para: 1, Find: strp("hello"), Text: "hi"},
		{Para: 1, Find: strp("world"), Text: "there"},
	}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !strings.Contains(res.Outcomes[1].Reason, "combine them into a single edit") {
		t.Errorf("Reason = %q, want the genuine-overlap case to keep the combine wording", res.Outcomes[1].Reason)
	}
}

// ---------------------------------------------------------------------------
// Item 2: a rescan-failure rollback restores content, but must not leave the
// part flagged modified when it was not modified before this call — else
// WriteTo re-deflates an otherwise-untouched entry instead of copying its
// original compressed bytes verbatim, breaking document.go's "an untouched
// SaveAs reproduces the source byte for byte" promise.
// ---------------------------------------------------------------------------

// TestEdit_RescanFailureRollbackDoesNotFlagAnUntouchedPartModified pins the
// wasModified capture directly: when the part was NOT modified before this
// Edit call, and the rescan-failure rollback restores its pre-call content,
// the part must end up NOT flagged modified either — otherwise WriteTo
// re-deflates an entry it could have copied raw, breaking document.go's
// "an untouched SaveAs reproduces the source byte for byte" promise (see the
// C3 rollback finding). It reaches the rescan-failure path the same way
// TestEdit_RescanFailureRestoresPreEditContent does — see that test's
// comment for why invalid UTF-8 in Text (the original C3 repro) can no
// longer get there, now that this same fix wave's item 3 refuses it
// per-edit before Apply ever runs.
func TestEdit_RescanFailureRollbackDoesNotFlagAnUntouchedPartModified(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>MARKERTEXT</w:t></w:r></w:p><w:p><w:r><w:t>second</w:t></w:r></w:p>`)
	if d.pkg.modified[DocumentPart] {
		t.Fatal("test setup: part already flagged modified before the edit")
	}

	live, ok := d.pkg.Part(DocumentPart)
	if !ok {
		t.Fatal("no document part")
	}
	corrupted := append([]byte(nil), live...)
	idx := bytes.LastIndex(corrupted, []byte("</w:p>"))
	if idx < 0 {
		t.Fatal("test setup: could not find a closing </w:p> tag to corrupt")
	}
	corrupted[idx+1] = 'X'
	// Bypasses SetPart deliberately, so p.modified stays false — this
	// simulates "the part was not modified before this Edit call" while
	// still reaching a genuine rescan failure (see the comment on
	// TestEdit_RescanFailureRestoresPreEditContent).
	d.pkg.parts[DocumentPart] = corrupted

	_, err := d.Edit([]Edit{{Para: 1, Text: "changed"}}, EditOptions{})
	if err == nil {
		t.Fatal("Edit unexpectedly succeeded; test setup did not actually corrupt the document")
	}
	if d.pkg.modified[DocumentPart] {
		t.Error("the part is still flagged modified after the rescan-failure rollback restored its (unmodified-before-this-call) content")
	}
}

// TestEdit_FailedEditLeavesSaveAsByteIdenticalToSource is the end-to-end
// pin: after a failed edit whose rescan-failure rollback fires, SaveAs must
// reproduce the file this Document was opened from byte for byte, because
// the restored part must no longer be flagged modified (see
// TestEdit_RescanFailureRollbackDoesNotFlagAnUntouchedPartModified) —
// otherwise WriteTo re-deflates it instead of copying its original
// compressed bytes verbatim (measured in the review at 36984 bytes from a
// 36927-byte source).
func TestEdit_FailedEditLeavesSaveAsByteIdenticalToSource(t *testing.T) {
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

	// Corrupt the live part directly (bypassing SetPart, so p.modified
	// stays false) in a region paragraph 1's patch never touches — see
	// TestEdit_RescanFailureRestoresPreEditContent for why this technique
	// is needed to reach a genuine rescan failure now that item 3 refuses
	// invalid UTF-8 in Text before Apply ever runs.
	live, ok := d.pkg.Part(DocumentPart)
	if !ok {
		t.Fatal("no document part")
	}
	corrupted := append([]byte(nil), live...)
	idx := bytes.LastIndex(corrupted, []byte("</w:p>"))
	if idx < 0 {
		t.Fatal("test setup: could not find a closing </w:p> tag to corrupt")
	}
	corrupted[idx+1] = 'X'
	d.pkg.parts[DocumentPart] = corrupted

	_, err = d.Edit([]Edit{{Para: 1, Text: "changed"}}, EditOptions{})
	if err == nil {
		t.Fatal("Edit unexpectedly succeeded; test setup did not actually corrupt the document")
	}

	out := filepath.Join(t.TempDir(), "saved.docx")
	if err := d.SaveAs(out); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}

	origInfo, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	savedInfo, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	// The restored part is (once again) untouched, so WriteTo must copy
	// every entry from the ORIGINAL raw compressed bytes captured at Open —
	// which is oblivious to this test's in-memory corruption of p.parts, so
	// the saved file must exactly match the pristine file this Document was
	// opened from, corruption and rollback both included.
	if savedInfo.Size() != origInfo.Size() {
		t.Errorf("SaveAs after a failed edit produced a %d byte file, want %d (raw copy of the source) — "+
			"the part is still flagged modified, so WriteTo re-deflated it instead of copying it raw",
			savedInfo.Size(), origInfo.Size())
	}
}

// ---------------------------------------------------------------------------
// Item 3: invalid UTF-8 in Text must be refused as this one edit's Reason,
// not surface as a whole-batch error that discards every edit including
// unrelated, valid ones.
// ---------------------------------------------------------------------------

func TestEdit_InvalidUTF8IsRefusedPerEditNotWholeBatch(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>one</w:t></w:r></w:p><w:p><w:r><w:t>two</w:t></w:r></w:p>`)

	res, err := d.Edit([]Edit{
		{Para: 1, Text: "bad\xffbyte"},
		{Para: 2, Text: "fine"},
	}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit returned a whole-batch error instead of a per-edit Reason: %v", err)
	}
	if res.Outcomes[0].Applied {
		t.Error("an edit containing invalid UTF-8 was applied")
	}
	if res.Outcomes[0].Reason == "" {
		t.Error("the invalid-UTF-8 edit has no Reason")
	}
	if !res.Outcomes[1].Applied {
		t.Errorf("the unrelated, valid edit was blocked too: %s", res.Outcomes[1].Reason)
	}
	if got := paraTextAt(t, d, 2); got != "fine" {
		t.Errorf("paragraph 2 = %q, want %q", got, "fine")
	}
	if _, err := OpenDocument(d.path); err != nil {
		t.Errorf("document is no longer openable after the refused edit: %v", err)
	}
}
