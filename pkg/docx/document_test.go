package docx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const outlineFixture = "testdata/outline.docx"

func TestOpenDocument_ScansOnce(t *testing.T) {
	d, err := OpenDocument(fixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if d.TotalParas() != len(d.Paras()) {
		t.Errorf("TotalParas = %d, len(Paras) = %d", d.TotalParas(), len(d.Paras()))
	}
	if d.TotalParas() != 10 {
		t.Errorf("TotalParas = %d, want 10 (structure.docx)", d.TotalParas())
	}
}

// TestParas_IsASnapshot pins that callers cannot corrupt the document's
// internal index by mutating what Paras() handed them.
func TestParas_IsASnapshot(t *testing.T) {
	d, err := OpenDocument(fixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	got := d.Paras()
	if len(got) == 0 {
		t.Fatal("no paragraphs")
	}
	got[0].Index = 9999
	if d.Paras()[0].Index == 9999 {
		t.Error("Paras() returned an aliasing slice; mutation leaked into the document")
	}
}

// TestNotes_DeclaresUnreadContent pins §4.1's requirement that the reader
// must never silently present a partial document.
func TestNotes_DeclaresUnreadContent(t *testing.T) {
	d, err := OpenDocument(fixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	notes := strings.Join(d.Notes(), " | ")
	// structure.docx has header1.xml and footer1.xml.
	if !strings.Contains(notes, "header") {
		t.Errorf("Notes = %q, want it to mention headers", notes)
	}
	if !strings.Contains(notes, "footer") {
		t.Errorf("Notes = %q, want it to mention footers", notes)
	}
}

func TestNotes_EmptyWhenNothingIsOmitted(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if len(d.Notes()) != 0 {
		t.Errorf("Notes = %v, want none for a plain document", d.Notes())
	}
}

// TestHasRevisions_DetectsExistingMarks feeds §4.1's recommended P1 policy.
func TestHasRevisions_DetectsExistingMarks(t *testing.T) {
	d, err := OpenDocument(fixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if !d.HasRevisions() {
		t.Error("HasRevisions = false, want true (structure.docx contains w:ins and w:del)")
	}
	plain, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if plain.HasRevisions() {
		t.Error("HasRevisions = true for outline.docx, want false")
	}
}

func TestSaveAs_ProducesAReadableCopy(t *testing.T) {
	d, err := OpenDocument(fixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	out := filepath.Join(t.TempDir(), "copy.docx")
	if err := d.SaveAs(out); err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	reopened, err := OpenDocument(out)
	if err != nil {
		t.Fatalf("OpenDocument(copy): %v", err)
	}
	if reopened.TotalParas() != d.TotalParas() {
		t.Errorf("copy has %d paragraphs, original has %d", reopened.TotalParas(), d.TotalParas())
	}
	// An untouched save must preserve every entry byte for byte.
	assertEntriesEqual(t, fixture, out, nil)
}

func TestSave_WritesBackToTheOriginalPath(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "work.docx")
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(work, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := OpenDocument(work)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if err := d.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	assertEntriesEqual(t, fixture, work, nil)
}

func TestOpenDocument_PropagatesOpenErrors(t *testing.T) {
	if _, err := OpenDocument(filepath.Join(t.TempDir(), "missing.docx")); err == nil {
		t.Fatal("OpenDocument on a missing file returned nil error")
	}
}

// TestComputeNotes_DeclaresEachOmittedPartKind exercises computeNotes
// directly with synthetic part names, covering footnotes, endnotes, and
// comments — none of which either fixture contains, so a whole-Document
// test would leave these branches uncovered.
func TestComputeNotes_DeclaresEachOmittedPartKind(t *testing.T) {
	names := []string{
		"word/document.xml",
		"word/footnotes.xml",
		"word/endnotes.xml",
		"word/comments.xml",
	}
	notes := strings.Join(computeNotes(names, nil), " | ")
	if !strings.Contains(notes, "footnotes") {
		t.Errorf("notes = %q, want it to mention footnotes", notes)
	}
	if !strings.Contains(notes, "endnotes") {
		t.Errorf("notes = %q, want it to mention endnotes", notes)
	}
	if !strings.Contains(notes, "comments") {
		t.Errorf("notes = %q, want it to mention comments", notes)
	}
}

// TestComputeNotes_DeclaresSkippedTextBoxes exercises computeNotes' loop
// over paras looking for SkippedTextBox, which neither fixture triggers.
func TestComputeNotes_DeclaresSkippedTextBoxes(t *testing.T) {
	paras := []Para{{Index: 1}, {Index: 2, SkippedTextBox: true}}
	notes := strings.Join(computeNotes(nil, paras), " | ")
	if !strings.Contains(notes, "text box") {
		t.Errorf("notes = %q, want it to mention skipped text boxes", notes)
	}
}

// TestComputeNotes_EmptyWhenNothingOmitted pins the "no note" baseline so
// the two tests above are meaningfully asserting presence, not just any
// non-empty string.
func TestComputeNotes_EmptyWhenNothingOmitted(t *testing.T) {
	if notes := computeNotes([]string{"word/document.xml"}, []Para{{Index: 1}}); len(notes) != 0 {
		t.Errorf("notes = %v, want none", notes)
	}
}

// TestRescan_RecomputesSkippedTextBoxNoteAfterDeletion pins the minor fix:
// notes was computed once in OpenDocument and never refreshed, so deleting
// the one paragraph that held a skipped text box used to leave "text boxes
// were skipped" in Notes() forever.
func TestRescan_RecomputesSkippedTextBoxNoteAfterDeletion(t *testing.T) {
	d := bodyDoc(t, `<w:p><w:r><w:drawing><wps:txbx><w:txbxContent>`+
		`<w:p><w:r><w:t>inside box</w:t></w:r></w:p>`+
		`</w:txbxContent></wps:txbx></w:drawing></w:r></w:p>`+
		`<w:p><w:r><w:t>second</w:t></w:r></w:p>`)

	if !strings.Contains(strings.Join(d.Notes(), "|"), "text box") {
		t.Fatalf("Notes = %v, want the text-box note before the delete", d.Notes())
	}

	res, err := d.Edit([]Edit{{Para: 1, Op: "delete"}}, EditOptions{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !res.Outcomes[0].Applied {
		t.Fatalf("delete was refused: %s", res.Outcomes[0].Reason)
	}
	if strings.Contains(strings.Join(d.Notes(), "|"), "text box") {
		t.Errorf("Notes = %v, want the text-box note gone after deleting the only paragraph that had one", d.Notes())
	}
}

// ---------------------------------------------------------------------------
// API addition: (*Document).Modified.
// ---------------------------------------------------------------------------

func TestDocument_ModifiedTracksEditsAndResetsOnSave(t *testing.T) {
	data, err := os.ReadFile(outlineFixture)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "mod.docx")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := OpenDocument(p)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if d.Modified() {
		t.Error("Modified() = true immediately after open, want false")
	}

	if _, err := d.Edit([]Edit{{Para: 2, Find: strp("Body"), Text: "BODY"}}, EditOptions{}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !d.Modified() {
		t.Error("Modified() = false after a successful edit, want true")
	}

	if err := d.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if d.Modified() {
		t.Error("Modified() = true after Save, want false")
	}
}

// TestDocument_ModifiedStaysFalseWhenAllEditsAreRefused is the negative
// half: a batch where nothing actually applied must not flip Modified().
func TestDocument_ModifiedStaysFalseWhenAllEditsAreRefused(t *testing.T) {
	d, err := OpenDocument(outlineFixture)
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	if _, err := d.Edit([]Edit{{Para: 99999, Text: "x"}}, EditOptions{}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if d.Modified() {
		t.Error("Modified() = true after a fully-refused batch, want false")
	}
}
