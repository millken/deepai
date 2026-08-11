package docx

// Edit tests keyed on specific document.xml shapes.
//
// bodyDoc below builds a minimal synthetic .docx from a raw document.xml
// body fragment, because none of the behaviour pinned here is reachable
// from either committed fixture: each case needs a particular XML shape —
// several <w:t> children inside one <w:r>, a hyperlink-wrapped first run, a
// table cell holding exactly one paragraph — that the fixtures happen not
// to contain. Word produces all of them routinely, which is precisely why
// they need pinning: every defect in this file was silently wrong output or
// a false audit trail, not a crash.

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bodyDoc builds a temp .docx whose word/document.xml body is exactly
// bodyXML (e.g. "<w:p>...</w:p>"), wrapped in the ordinary
// <w:document><w:body>...</w:body></w:document> envelope, and opens it as a
// Document.
func bodyDoc(t *testing.T, bodyXML string) *Document {
	t.Helper()
	docXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:body>` + bodyXML + `</w:body></w:document>`

	p := filepath.Join(t.TempDir(), "synthetic.docx")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entries := []struct{ name, content string }{
		{"[Content_Types].xml", `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`},
		{"word/document.xml", docXML},
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
	d, err := OpenDocument(p)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	return d
}

// ---------------------------------------------------------------------------
// C2: runs sharing one <w:r> caused silent text loss and a false audit trail.
// ---------------------------------------------------------------------------

// TestEdit_RunDeleteOnSharedElemRemovesOnlyThatRun pins the first C2
// scenario: <w:r><w:t>a</w:t><w:t>b</w:t></w:r> produces two Runs sharing
// one Elem span. Deleting run 1 must remove only "a"; "b" must survive.
func TestEdit_RunDeleteOnSharedElemRemovesOnlyThatRun(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>a</w:t><w:t>b</w:t></w:r></w:p>`)
	para := d.Paras()[0]
	if len(para.Runs) != 2 {
		t.Fatalf("got %d runs, want 2 (shared-Elem shape)", len(para.Runs))
	}
	if para.Runs[0].Elem != para.Runs[1].Elem {
		t.Fatalf("runs do not share Elem, test setup is not exercising the shared-Elem shape")
	}

	res, err := d.Edit([]Edit{{Para: 1, Run: 1, Op: "delete"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("delete was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[0].Before != "a" {
		t.Errorf("Outcome.Before = %q, want %q", res.Outcomes[0].Before, "a")
	}
	got := paraTextAt(t, d, 1)
	if got != "b" {
		t.Errorf("paragraph 1 = %q, want %q (only run 1 deleted, run 2's text survives)", got, "b")
	}
}

// TestEdit_WholeParagraphReplaceOnSharedElemClearsTrailingText pins the
// second C2 scenario: a whole-paragraph replace on the same shared-Elem
// shape must not leave the second <w:t>'s text appended to the new text.
// Before this fix, first.Elem.End == last.Elem.End (both runs share the
// same <w:r>), so the tail-deletion patch was skipped entirely and the
// result read "flatb" instead of "flat" — while Outcome.After still (falsely)
// reported "flat".
func TestEdit_WholeParagraphReplaceOnSharedElemClearsTrailingText(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>a</w:t><w:t>b</w:t></w:r></w:p>`)

	res, err := d.Edit([]Edit{{Para: 1, Text: "flat"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("replace was refused: %s", res.Outcomes[0].Reason)
	}
	got := paraTextAt(t, d, 1)
	if got != "flat" {
		t.Errorf("paragraph 1 = %q, want %q (Outcome.After said %q)", got, "flat", res.Outcomes[0].After)
	}
}

// ---------------------------------------------------------------------------
// C3: a failed rescan must never leave bad bytes for Save to write out.
// ---------------------------------------------------------------------------

// TestEdit_IllegalXMLCharacterIsRefusedPerEdit pins the first C3 half: a
// caller-controlled illegal XML character (a literal NUL) must be refused
// as this one edit's Reason, not surface as a whole-batch error — and an
// unrelated edit in the same batch must still apply.
func TestEdit_IllegalXMLCharacterIsRefusedPerEdit(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>one</w:t></w:r></w:p><w:p><w:r><w:t>two</w:t></w:r></w:p>`)

	res, err := d.Edit([]Edit{
		{Para: 1, Text: "bad\x00char"},
		{Para: 2, Text: "fine"},
	}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit returned a whole-batch error instead of a per-edit Reason: %v", err)
	}
	if res.Outcomes[0].Applied {
		t.Error("an edit containing an XML-illegal character was applied")
	}
	if res.Outcomes[0].Reason == "" {
		t.Error("the illegal-character edit has no Reason")
	}
	if !res.Outcomes[1].Applied {
		t.Errorf("the unrelated edit was blocked too: %s", res.Outcomes[1].Reason)
	}
	if got := paraTextAt(t, d, 2); got != "fine" {
		t.Errorf("paragraph 2 = %q, want %q", got, "fine")
	}
	// The document must genuinely still open and scan cleanly afterward —
	// this is the ultimate proof no bad bytes reached the package.
	if _, err := OpenDocument(d.path); err != nil {
		t.Errorf("document is no longer openable after the refused edit: %v", err)
	}
}

// TestEdit_RescanFailureRestoresPreEditContent pins the second C3 half
// through the REAL Document.Edit() API.
//
// It originally reproduced the rescan failure with an invalid UTF-8 byte in
// Text ("bad\xffbyte"): decoding it rune-by-rune turned the lone 0xff byte
// into U+FFFD (a legal XML codepoint), so firstIllegalXMLChar passed it
// through, and — since no Patch in that batch was Raw — Apply's
// well-formedness gate (gated behind hasRaw) never ran either, so the bad
// byte reached document.xml untouched and only rescan caught it. The P1b
// planEdit's utf8.ValidString check closes that
// specific gap by refusing invalid UTF-8 per-edit before Apply ever runs —
// which is exactly the intended, better outcome, but it also means that
// input can no longer reach THIS test's target code (the rescan-failure
// rollback) through Document.Edit's public surface at all.
//
// So this test now reaches the same rollback code a different way: it
// corrupts the LIVE part directly (bypassing SetPart, so p.modified stays
// false — see TestEdit_RescanFailureRollbackDoesNotFlagAnUntouchedPartModified)
// in a region paragraph 1's patch never touches. Document's cached scan
// (d.doc/d.paras) still points at the original, valid slice — Part()
// aliases the map entry, and this replaces the map entry with a different
// slice rather than mutating in place — so planning against paragraph 1
// proceeds normally; Apply's Old-snapshot check only verifies the patched
// span's own bytes (unchanged elsewhere), and no patch in this batch is Raw,
// so nothing catches the corruption before SetPart commits it. Only rescan
// does, exactly the C3 gap this test exists to pin.
func TestEdit_RescanFailureRestoresPreEditContent(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>MARKERTEXT</w:t></w:r></w:p><w:p><w:r><w:t>second</w:t></w:r></w:p>`)
	live, ok := d.pkg.Part(DocumentPart)
	if !ok {
		t.Fatal("no document part")
	}
	corrupted := append([]byte(nil), live...)
	idx := bytes.LastIndex(corrupted, []byte("</w:p>"))
	if idx < 0 {
		t.Fatal("test setup: could not find a closing </w:p> tag to corrupt")
	}
	corrupted[idx+1] = 'X' // "<Xw:p>" instead of "</w:p>": same length, breaks well-formedness.
	d.pkg.parts[DocumentPart] = corrupted
	beforeCopy := append([]byte(nil), corrupted...)

	_, err := d.Edit([]Edit{{Para: 1, Text: "changed"}}, EditOptions{})
	if err == nil {
		t.Fatal("Edit unexpectedly succeeded; test setup did not actually corrupt the document")
	}

	after, _ := d.pkg.Part(DocumentPart)
	if !bytes.Equal(after, beforeCopy) {
		t.Error("the package's part was not restored to its pre-edit content after a failed rescan")
	}
}

// TestEdit_BadEditDoesNotCorruptSavedFile is the end-to-end pin: run an
// Edit that is refused per-edit for an illegal character, then Save, then
// reopen — the file on disk must be exactly what it was, never a half-
// applied or unscannable document.
func TestEdit_BadEditDoesNotCorruptSavedFile(t *testing.T) {
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

	res, err := d.Edit([]Edit{{Para: 2, Text: "bad\x00char"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit returned a whole-batch error: %v", err)
	}
	if res.Applied != 0 {
		t.Fatal("the illegal-character edit was applied")
	}
	if err := d.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	assertEntriesEqual(t, outlineFixture, p, nil)
	if _, err := OpenDocument(p); err != nil {
		t.Errorf("saved file is not openable: %v", err)
	}
}

// ---------------------------------------------------------------------------
// C4: any span collision in a batch must refuse only the later edit.
// ---------------------------------------------------------------------------

func twoRunPara() string {
	return `<w:p><w:r><w:t>hello world</w:t></w:r></w:p>`
}

// TestEdit_TwoFindsInTheSameRunRefuseOnlyTheLater pins the most common
// collision shape named in the review: two find replaces landing in the
// same run.
func TestEdit_TwoFindsInTheSameRunRefuseOnlyTheLater(t *testing.T) {
	d := bodyDoc(t, twoRunPara())
	res, err := d.Edit([]Edit{
		{Para: 1, Find: strp("hello"), Text: "hi"},
		{Para: 1, Find: strp("world"), Text: "there"},
	}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit returned an error instead of refusing one edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Errorf("first edit was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[1].Applied {
		t.Error("second, colliding edit was applied")
	}
	if !strings.Contains(res.Outcomes[1].Reason, "edit 1") {
		t.Errorf("Reason = %q, want it to name edit 1", res.Outcomes[1].Reason)
	}
	if strings.Contains(res.Outcomes[1].Reason, "byte") {
		t.Errorf("Reason = %q, must not surface a byte offset", res.Outcomes[1].Reason)
	}
	if got := paraTextAt(t, d, 1); got != "hi world" {
		t.Errorf("paragraph 1 = %q, want %q", got, "hi world")
	}
}

// TestEdit_CollisionDoesNotBlockAnUnrelatedEdit pins §4.2's "one refusal
// must never block the rest of the batch": a colliding pair plus an
// unrelated third edit on another paragraph must still let the third one
// through.
func TestEdit_CollisionDoesNotBlockAnUnrelatedEdit(t *testing.T) {
	d := bodyDoc(t, twoRunPara()+`<w:p><w:r><w:t>second para</w:t></w:r></w:p>`)
	res, err := d.Edit([]Edit{
		{Para: 1, Find: strp("hello"), Text: "hi"},
		{Para: 1, Find: strp("world"), Text: "there"},
		{Para: 2, Find: strp("second"), Text: "SECOND"},
	}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Outcomes[1].Applied {
		t.Error("the colliding second edit was applied")
	}
	if !res.Outcomes[2].Applied {
		t.Errorf("the unrelated third edit was blocked: %s", res.Outcomes[2].Reason)
	}
	if got := paraTextAt(t, d, 2); got != "SECOND para" {
		t.Errorf("paragraph 2 = %q, want %q", got, "SECOND para")
	}
}

// TestEdit_InsertAfterTwiceRefusesOnlyTheSecond pins another table row:
// insert_after(N) issued twice both anchor at the exact same zero-length
// offset.
func TestEdit_InsertAfterTwiceRefusesOnlyTheSecond(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>only</w:t></w:r></w:p>`)
	res, err := d.Edit([]Edit{
		{Para: 1, Op: "insert_after", Text: "first insert"},
		{Para: 1, Op: "insert_after", Text: "second insert"},
	}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Errorf("first insert_after was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[1].Applied {
		t.Error("second, colliding insert_after was applied")
	}
	if d.TotalParas() != 2 {
		t.Errorf("TotalParas = %d, want 2 (only the first insert landed)", d.TotalParas())
	}
}

// TestEdit_DeleteParaPlusFindInsideItRefusesOnlyTheLater pins the
// containment row: delete(N) whole-paragraph plus a find inside paragraph N
// in the same batch.
func TestEdit_DeleteParaPlusFindInsideItRefusesOnlyTheLater(t *testing.T) {
	d := bodyDoc(t, twoRunPara())
	res, err := d.Edit([]Edit{
		{Para: 1, Op: "delete"},
		{Para: 1, Find: strp("hello"), Text: "hi"},
	}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Errorf("delete was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[1].Applied {
		t.Error("the find inside the deleted paragraph was applied")
	}
}

// ---------------------------------------------------------------------------
// I1: insert_before/insert_after must validate protect against forgery.
// ---------------------------------------------------------------------------

// TestEdit_InsertForgingAProtectedPatternIsRefused pins I1's exact repro: an
// insert_after whose text matches a protect pattern with a value that never
// appears anywhere else in the document is a forged/mistyped protected item.
func TestEdit_InsertForgingAProtectedPatternIsRefused(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>Currently on v1.0.0</w:t></w:r></w:p>`)
	res, err := d.Edit(
		[]Edit{{Para: 1, Op: "insert_after", Text: "Now on v9.9.9 instead"}},
		EditOptions{Protect: []string{`v\d+\.\d+\.\d+`}},
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Outcomes[0].Applied {
		t.Error("an insert forging a protected version number was applied")
	}
	if !strings.Contains(res.Outcomes[0].Reason, "v9.9.9") {
		t.Errorf("Reason = %q, want it to name the forged item", res.Outcomes[0].Reason)
	}
}

// TestEdit_InsertRepeatingAnExistingProtectedItemIsAllowed is the negative
// half: inserting text that repeats a protected item ALREADY present
// elsewhere in the document is not forgery and must be allowed.
func TestEdit_InsertRepeatingAnExistingProtectedItemIsAllowed(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>Currently on v1.0.0</w:t></w:r></w:p>`)
	res, err := d.Edit(
		[]Edit{{Para: 1, Op: "insert_after", Text: "See also v1.0.0 release notes"}},
		EditOptions{Protect: []string{`v\d+\.\d+\.\d+`}},
	)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Errorf("a legitimate repeat of an existing protected item was refused: %s", res.Outcomes[0].Reason)
	}
}

// ---------------------------------------------------------------------------
// I2: whole-paragraph replace must not cut through a closing tag.
// ---------------------------------------------------------------------------

// TestEdit_WholeParagraphReplaceRefusesWhenFirstRunIsInAHyperlink pins I2's
// hyperlink repro: the tail span between the first and last run's Elem
// slices through </w:hyperlink> when the first run sits one level deeper
// than the last.
func TestEdit_WholeParagraphReplaceRefusesWhenFirstRunIsInAHyperlink(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:hyperlink><w:r><w:t>link</w:t></w:r></w:hyperlink><w:r><w:t> tail</w:t></w:r></w:p>`)
	res, err := d.Edit([]Edit{{Para: 1, Text: "flat"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit returned a whole-batch error instead of refusing this edit: %v", err)
	}
	if res.Outcomes[0].Applied {
		t.Error("a whole-paragraph replace that would cut through </w:hyperlink> was applied")
	}
	if res.Outcomes[0].Reason == "" {
		t.Error("no Reason given for the refusal")
	}
}

// TestEdit_WholeParagraphReplaceStillWorksForOrdinarySiblingRuns is the
// negative half: ordinary sibling <w:r> elements at the same depth (the
// existing "Plain bold tail" shape) must keep working exactly as before.
func TestEdit_WholeParagraphReplaceStillWorksForOrdinarySiblingRuns(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:t>one</w:t></w:r><w:r><w:t>two</w:t></w:r><w:r><w:t>three</w:t></w:r></w:p>`)
	res, err := d.Edit([]Edit{{Para: 1, Text: "flat"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("ordinary multi-run replace was refused: %s", res.Outcomes[0].Reason)
	}
	if got := paraTextAt(t, d, 1); got != "flat" {
		t.Errorf("paragraph 1 = %q, want %q", got, "flat")
	}
}

// ---------------------------------------------------------------------------
// I3: deleting a table cell's only paragraph must not leave an empty <w:tc>.
// ---------------------------------------------------------------------------

// TestEdit_DeleteLastParagraphInCellLeavesEmptyParagraph pins I3: <w:tc> must
// always retain at least one <w:p>.
func TestEdit_DeleteLastParagraphInCellLeavesEmptyParagraph(t *testing.T) {
	d := bodyDoc(t, `<w:tbl><w:tr><w:tc><w:p><w:r><w:t>cell text</w:t></w:r></w:p></w:tc></w:tr></w:tbl>`)
	para := d.Paras()[0]
	if para.Cell == nil {
		t.Fatal("test setup did not produce a table-cell paragraph")
	}

	res, err := d.Edit([]Edit{{Para: 1, Op: "delete"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("delete was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[0].Warning == "" {
		t.Error("no Warning explaining the paragraph was kept empty rather than removed")
	}
	if d.TotalParas() != 1 {
		t.Fatalf("TotalParas = %d, want 1 (the cell must still contain exactly one, now-empty, paragraph)", d.TotalParas())
	}
	if got := paraTextAt(t, d, 1); got != "" {
		t.Errorf("paragraph 1 = %q, want empty", got)
	}
	doc, _ := d.pkg.Part(DocumentPart)
	if !strings.Contains(string(doc), "<w:tc>") || strings.Contains(string(doc), "<w:tc></w:tc>") {
		t.Errorf("document.xml = %s, want a non-empty <w:tc> containing an empty <w:p/>", doc)
	}
}

// TestEdit_DeleteParagraphInMultiParaCellRemovesItNormally is the negative
// half: a cell with more than one paragraph must still have the deleted one
// actually removed, not preserved-empty.
func TestEdit_DeleteParagraphInMultiParaCellRemovesItNormally(t *testing.T) {
	d := bodyDoc(t, `<w:tbl><w:tr><w:tc>`+
		`<w:p><w:r><w:t>first</w:t></w:r></w:p>`+
		`<w:p><w:r><w:t>second</w:t></w:r></w:p>`+
		`</w:tc></w:tr></w:tbl>`)

	res, err := d.Edit([]Edit{{Para: 1, Op: "delete"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("delete was refused: %s", res.Outcomes[0].Reason)
	}
	if res.Outcomes[0].Warning != "" {
		t.Errorf("unexpected Warning on a multi-paragraph cell delete: %q", res.Outcomes[0].Warning)
	}
	if d.TotalParas() != 1 {
		t.Fatalf("TotalParas = %d, want 1 (the first paragraph was actually removed)", d.TotalParas())
	}
	if got := paraTextAt(t, d, 1); got != "second" {
		t.Errorf("paragraph 1 = %q, want %q", got, "second")
	}
}
