package builtin

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/docx"
)

// This file is Task 14's systemic gap closer: pkg/tools/builtin's 86-odd
// docx tests are almost all single-handler (one docx_read call, one
// docx_edit call, ...); before this file only one test in the whole package
// chained two handlers together. Every bug design review 1-13 actually found
// in real use lived exactly in the seam BETWEEN two tool calls (docx_write's
// styles.xml missing a docDefaults chain that docx_format needed; docx_edit's
// revision gate re-opening the document fresh on every call and tripping over
// its OWN first tracked edit; docx_format's normalize shifting every later
// paragraph's index with nothing telling a caller mid-edit-session). This
// file drives every case through the REAL exported handlers
// (DocxWriteHandler/DocxReadHandler/DocxEditHandler/DocxFormatHandler), never
// pkg/docx directly, so a regression that only shows up through the tool
// layer's argument parsing/JSON marshaling/backup bookkeeping can't hide
// behind a green pkg/docx test suite the way it did three times before.
//
// Fixtures come in the two flavors the brief asks for: a docx_write product
// (fresh styles.xml/docDefaults exactly as WriteDocx builds it), and
// hand-written XML mimicking a real python-docx/Word document — including,
// for TestPipeline_TrackedEditChain, a Normal style that carries its own
// <w:rPr> (masking docDefaults, since neither committed testdata fixture
// happens to have that: see gen_fixtures.py) and a paragraph with its own
// paragraph-mark <w:pPr><w:rPr>.

// docxStyledFixture builds a minimal .docx like bodyDocxFixture (same
// package, docx_test.go), but with a real word/styles.xml too. Used only
// where a test needs a Normal style shaped like a real-world document
// (masking docDefaults with its own explicit rPr) rather than the bare
// Normal outline.docx/structure.docx both happen to carry.
func docxStyledFixture(t *testing.T, bodyXML, stylesXML string) string {
	t.Helper()
	docXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:body>` + bodyXML + `</w:body></w:document>`
	p := filepath.Join(t.TempDir(), "synthetic-styled.docx")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entries := []struct{ name, content string }{
		{"[Content_Types].xml", `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`},
		{docx.DocumentPart, docXML},
		{"word/styles.xml", stylesXML},
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

// ---------------------------------------------------------------------------
// 1. write -> format
// ---------------------------------------------------------------------------

// TestPipeline_WriteThenFormat_LineSpacingLandsOnBodyTextActualValue is the
// tool-layer complement to pkg/docx's own
// TestWriteThenFormat_LineSpacingLandsOnTheEffectiveStyleNotJustDocDefaults
// (task 7): that test proves the *Document in-memory value; this one drives
// the exact same composition through DocxWriteHandler and DocxFormatHandler
// and inspects the SAVED FILE's raw word/styles.xml, so a regression in the
// tool layer's own argument parsing or its Save/backup bookkeeping (neither
// of which the pkg/docx-level test can see) would be caught here even if
// pkg/docx's own suite stayed green.
func TestPipeline_WriteThenFormat_LineSpacingLandsOnBodyTextActualValue(t *testing.T) {
	p := filepath.Join(t.TempDir(), "written.docx")
	if _, err := callDocxWrite(t, map[string]any{
		"path":     p,
		"markdown": "# Title\n\nSome body text here.\n",
	}); err != nil {
		t.Fatalf("DocxWriteHandler: %v", err)
	}

	res, err := callDocxFormat(t, map[string]any{
		"path":  p,
		"rules": map[string]any{"line_spacing": float64(2.0)},
	})
	if err != nil {
		t.Fatalf("DocxFormatHandler: %v", err)
	}
	out := decodeRead(t, res)
	if applied, _ := out["applied"].([]any); len(applied) == 0 {
		t.Fatalf("applied is empty; line_spacing never took effect (content=%s)", res.Content)
	}

	styles := string(zipEntry(t, p, "word/styles.xml"))
	bodyText := styles[strings.Index(styles, `w:styleId="BodyText"`):]
	bodyText = bodyText[:strings.Index(bodyText, "</w:style>")]
	if !strings.Contains(bodyText, `w:line="480"`) {
		t.Errorf("BodyText's own <w:spacing> was not rewritten to 480 (2.0x in 240ths) on the SAVED file:\n%s", bodyText)
	}
}

// TestPipeline_WriteThenFormat_TemplatePreservesEastAsiaFont is the
// tool-layer complement to pkg/docx's own
// TestWriteThenFormat_TemplateCorporatePreservesEastAsiaFont (task 7's I5):
// docx_write's default east-asia font (微软雅黑) must survive a
// template=corporate docx_format call unharmed on the file actually written
// to disk, not just in an open *Document.
func TestPipeline_WriteThenFormat_TemplatePreservesEastAsiaFont(t *testing.T) {
	p := filepath.Join(t.TempDir(), "written-cjk.docx")
	if _, err := callDocxWrite(t, map[string]any{
		"path":     p,
		"markdown": "# 标题\n\n正文内容。\n",
	}); err != nil {
		t.Fatalf("DocxWriteHandler: %v", err)
	}

	res, err := callDocxFormat(t, map[string]any{
		"path":  p,
		"rules": map[string]any{"template": "corporate"},
	})
	if err != nil {
		t.Fatalf("DocxFormatHandler: %v", err)
	}
	out := decodeRead(t, res)
	if applied, _ := out["applied"].([]any); len(applied) == 0 {
		t.Fatalf("applied is empty; template=corporate never took effect (content=%s)", res.Content)
	}

	styles := string(zipEntry(t, p, "word/styles.xml"))
	dd := styles[strings.Index(styles, "<w:docDefaults>"):strings.Index(styles, "</w:docDefaults>")]
	if !strings.Contains(dd, `w:eastAsia="微软雅黑"`) {
		t.Errorf("docDefaults lost its eastAsia font on the SAVED file (Chinese text would fall back to a substitute):\n%s", dd)
	}
	if !strings.Contains(dd, `w:ascii="Calibri"`) {
		t.Errorf("docDefaults lacks the corporate template's Latin body font on the SAVED file:\n%s", dd)
	}
}

// ---------------------------------------------------------------------------
// 2. write -> edit (tracked) -> read
// ---------------------------------------------------------------------------

// TestPipeline_WriteEditRead_TrackedInsertIsVisibleAndDeclaredPending drives
// all three of docx_write/docx_edit/docx_read in sequence: a document
// docx_write just produced gets a tracked insert_after from docx_edit, and
// docx_read on the result must (a) show the inserted text as if already
// accepted (per Read's own rendering contract) and (b) declare, in notes,
// that this is a pending revision naming the actual author -- otherwise a
// caller reading the document back would believe "Inserted commentary." is
// simply part of the document's current text, indistinguishable from
// anything already reviewed.
func TestPipeline_WriteEditRead_TrackedInsertIsVisibleAndDeclaredPending(t *testing.T) {
	p := filepath.Join(t.TempDir(), "written.docx")
	if _, err := callDocxWrite(t, map[string]any{
		"path":     p,
		"markdown": "# Title\n\nOriginal body paragraph.\n",
	}); err != nil {
		t.Fatalf("DocxWriteHandler: %v", err)
	}

	editRes, err := callDocxEdit(t, map[string]any{
		"path":          p,
		"track_changes": true,
		"author":        "Alice",
		"edits": []any{map[string]any{
			"para": float64(2), "op": "insert_after", "text": "Inserted commentary.",
		}},
	})
	if err != nil {
		t.Fatalf("DocxEditHandler: %v", err)
	}
	editOut := decodeRead(t, editRes)
	if editOut["applied"] != float64(1) {
		t.Fatalf("applied = %v, want 1 (content=%s)", editOut["applied"], editRes.Content)
	}
	if editOut["track_changes"] != true {
		t.Errorf("track_changes = %v, want true", editOut["track_changes"])
	}
	if editOut["para_count_changed"] != true {
		t.Error("para_count_changed = false after an insert; the new paragraph must shift the count")
	}

	xml := readDocumentXML(t, p)
	if !strings.Contains(xml, "<w:ins ") || !strings.Contains(xml, `w:author="Alice"`) {
		t.Errorf("the saved file has no <w:ins> stamped with author Alice: %s", xml)
	}

	readRes, err := callDocxRead(t, map[string]any{"path": p, "full": true})
	if err != nil {
		t.Fatalf("DocxReadHandler: %v", err)
	}
	readOut := decodeRead(t, readRes)
	md, _ := readOut["markdown"].(string)
	if !strings.Contains(md, "Inserted commentary.") {
		t.Errorf("markdown = %q, want the tracked insert's text visible (Read renders it as if accepted)", md)
	}
	notes, _ := readOut["notes"].([]any)
	joined := fmt.Sprint(notes)
	if !strings.Contains(joined, "Alice") {
		t.Errorf("notes = %v, want the pending revision's author (Alice) named", notes)
	}
	if !strings.Contains(strings.ToLower(joined), "unreviewed") {
		t.Errorf("notes = %v, want it to declare the revision as pending/unreviewed", notes)
	}
}

// ---------------------------------------------------------------------------
// 3. edit(tracked) -> edit(tracked) -> edit(plain), same path, three calls
// ---------------------------------------------------------------------------

// pipelineStyledStyles is a python-docx-style word/styles.xml whose Normal
// paragraph style carries its own explicit <w:rPr> (Times New Roman/24
// half-points, plus a SimSun east-asia fallback) that MASKS docDefaults
// (Calibri/22) -- a shape neither committed testdata fixture has (both
// outline.docx and structure.docx's own Normal style is bare; see
// gen_fixtures.py), but common in a real customized Word/python-docx
// document.
const pipelineStyledStyles = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	`<w:docDefaults><w:rPrDefault><w:rPr><w:rFonts w:ascii="Calibri" w:hAnsi="Calibri"/><w:sz w:val="22"/><w:szCs w:val="22"/></w:rPr></w:rPrDefault>` +
	`<w:pPrDefault><w:pPr><w:spacing w:after="200" w:line="276" w:lineRule="auto"/></w:pPr></w:pPrDefault></w:docDefaults>` +
	`<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/>` +
	`<w:rPr><w:rFonts w:ascii="Times New Roman" w:hAnsi="Times New Roman" w:eastAsia="SimSun"/><w:sz w:val="24"/></w:rPr></w:style>` +
	`</w:styles>`

// pipelineStyledBody has three ordinary Normal-styled paragraphs; the first
// carries its own paragraph-mark <w:pPr><w:rPr><w:i/></w:pPr> (italic
// paragraph mark), the other real-world artifact the brief calls out
// alongside Normal masking.
const pipelineStyledBody = `<w:p><w:pPr><w:pStyle w:val="Normal"/><w:rPr><w:i/></w:rPr></w:pPr><w:r><w:t>First body line.</w:t></w:r></w:p>` +
	`<w:p><w:pPr><w:pStyle w:val="Normal"/></w:pPr><w:r><w:t>Second body line.</w:t></w:r></w:p>` +
	`<w:p><w:pPr><w:pStyle w:val="Normal"/></w:pPr><w:r><w:t>Third body line.</w:t></w:r></w:p>`

// TestPipeline_TrackedEditChain_ThreeCallsSameAuthorAllSucceed is the
// hand-XML-fixture sibling of TestDocxEdit_ThreeConsecutiveCallsOnSamePathAllSucceed
// (docx_test.go, task 3's C1 regression guard): three REAL, independent
// DocxEditHandler calls against the same path (not the same in-memory
// *Document -- the tool layer always re-opens fresh) must all succeed as
// long as every call in the round shares an author, even though the first
// two calls each leave real w:ins/w:del sitting in the file the next
// OpenDocument will see. It additionally proves the OTHER half of the same
// gate through the tool layer: once those tracked revisions exist, a FOURTH
// call from a genuinely different author must be refused outright, tracked
// or not, since Word has nothing recorded yet accepting or rejecting Rev's
// pending changes.
func TestPipeline_TrackedEditChain_ThreeCallsSameAuthorAllSucceed(t *testing.T) {
	p := docxStyledFixture(t, pipelineStyledBody, pipelineStyledStyles)

	res1, err := callDocxEdit(t, map[string]any{
		"path":          p,
		"track_changes": true,
		"author":        "Rev",
		"edits":         []any{map[string]any{"para": float64(1), "find": "First", "text": "FIRST"}},
	})
	if err != nil {
		t.Fatalf("first docx_edit call: %v", err)
	}
	if out := decodeRead(t, res1); out["applied"] != float64(1) {
		t.Fatalf("first call: applied = %v, want 1 (content=%s)", out["applied"], res1.Content)
	}

	res2, err := callDocxEdit(t, map[string]any{
		"path":          p,
		"track_changes": true,
		"author":        "Rev",
		"edits":         []any{map[string]any{"para": float64(2), "find": "Second", "text": "SECOND"}},
	})
	if err != nil {
		t.Fatalf("second docx_edit call on the same path (this is the C1 regression shape): %v", err)
	}
	if out := decodeRead(t, res2); out["applied"] != float64(1) {
		t.Fatalf("second call: applied = %v, want 1 (content=%s)", out["applied"], res2.Content)
	}

	res3, err := callDocxEdit(t, map[string]any{
		"path":          p,
		"track_changes": false,
		"author":        "Rev",
		"edits":         []any{map[string]any{"para": float64(3), "find": "Third", "text": "THIRD"}},
	})
	if err != nil {
		t.Fatalf("third docx_edit call (plain, same author) on the same path: %v", err)
	}
	if out := decodeRead(t, res3); out["applied"] != float64(1) {
		t.Fatalf("third call: applied = %v, want 1 (content=%s)", out["applied"], res3.Content)
	}

	xml := readDocumentXML(t, p)
	if strings.Count(xml, `w:author="Rev"`) < 2 {
		t.Errorf("want at least two revisions stamped author=Rev (the first two tracked calls): %s", xml)
	}
	// The third (plain) call's own replaced text must land as ordinary
	// content, never wrapped in its own <w:ins>.
	if strings.Contains(xml, "<w:ins ") && strings.Contains(xml, "<w:t>THIRD</w:t>") {
		thirdIns := xml[:strings.Index(xml, "<w:t>THIRD</w:t>")]
		if strings.LastIndex(thirdIns, "<w:ins ") > strings.LastIndex(thirdIns, "</w:ins>") {
			t.Error("THIRD (the plain, untracked edit) landed inside its own <w:ins>; want ordinary content")
		}
	}

	after, err := callDocxRead(t, map[string]any{"path": p, "full": true})
	if err != nil {
		t.Fatalf("callDocxRead: %v", err)
	}
	md, _ := decodeRead(t, after)["markdown"].(string)
	for _, want := range []string{"FIRST", "SECOND", "THIRD"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown = %q, want it to contain %q", md, want)
		}
	}

	// Fourth call, different (unrelated) author: the document still carries
	// Rev's two unreviewed tracked edits, so Mallory's call -- tracked or
	// not -- must be refused before touching anything.
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = callDocxEdit(t, map[string]any{
		"path":   p,
		"author": "Mallory",
		"edits":  []any{map[string]any{"para": float64(1), "find": "FIRST", "text": "HIJACKED"}},
	})
	if err == nil {
		t.Fatal("a different author's edit was accepted while Rev's tracked changes are still pending; want refusal")
	}
	if !strings.Contains(err.Error(), "Rev") {
		t.Errorf("error = %q, want it to name the pending author (Rev)", err)
	}
	afterRefusal, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, afterRefusal) {
		t.Error("the refused fourth call still modified the file on disk")
	}
}

// ---------------------------------------------------------------------------
// 4. format(normalize) -> edit
// ---------------------------------------------------------------------------

// TestPipeline_NormalizeThenEdit_TargetsTheCorrectShiftedParagraph pins task
// 10's own contract (docx_format's total_paras/para_count_changed/
// index_advice mirroring docx_edit's) all the way through an actual
// follow-up docx_edit call: normalize collapses three consecutive empty
// paragraphs down to one, shifting every later paragraph's index by -2. A
// caller that reads the new total_paras back and edits by the NEW index must
// land on the paragraph it actually means ("two"), not on whatever used to
// sit at that index before the collapse, and not on "three" one over.
func TestPipeline_NormalizeThenEdit_TargetsTheCorrectShiftedParagraph(t *testing.T) {
	p := bodyDocxFixture(t,
		`<w:p><w:r><w:t>one</w:t></w:r></w:p><w:p/><w:p/><w:p/>`+
			`<w:p><w:r><w:t>two</w:t></w:r></w:p><w:p><w:r><w:t>three</w:t></w:r></w:p>`)

	formatRes, err := callDocxFormat(t, map[string]any{"path": p, "rules": map[string]any{"normalize": true}})
	if err != nil {
		t.Fatalf("DocxFormatHandler: %v", err)
	}
	formatOut := decodeRead(t, formatRes)
	totalParas, _ := formatOut["total_paras"].(float64)
	if totalParas != 4 {
		t.Fatalf("total_paras = %v, want 4 (one/empty/two/three after collapsing 3 empties to 1)", formatOut["total_paras"])
	}
	if formatOut["para_count_changed"] != true {
		t.Fatal("para_count_changed = false after normalize collapsed paragraphs")
	}
	if formatOut["index_advice"] != docxIndexAdvice {
		t.Fatalf("index_advice = %q, want %q", formatOut["index_advice"], docxIndexAdvice)
	}

	// "two" is now paragraph 3 (was 5 before the collapse); the STALE index 5
	// now names "three" instead.
	newIndex := int(totalParas) - 1
	editRes, err := callDocxEdit(t, map[string]any{
		"path":  p,
		"edits": []any{map[string]any{"para": float64(newIndex), "find": "two", "text": "TWO-CHANGED"}},
	})
	if err != nil {
		t.Fatalf("DocxEditHandler: %v", err)
	}
	editOut := decodeRead(t, editRes)
	if editOut["applied"] != float64(1) {
		t.Fatalf("applied = %v, want 1 against the shifted index %d (content=%s)", editOut["applied"], newIndex, editRes.Content)
	}
	outcomes, _ := editOut["outcomes"].([]any)
	first, _ := outcomes[0].(map[string]any)
	if first["before"] != "two" || first["after"] != "TWO-CHANGED" {
		t.Errorf("before/after = %v/%v, want two/TWO-CHANGED", first["before"], first["after"])
	}

	readRes, err := callDocxRead(t, map[string]any{"path": p, "full": true})
	if err != nil {
		t.Fatal(err)
	}
	md, _ := decodeRead(t, readRes)["markdown"].(string)
	if !strings.Contains(md, "TWO-CHANGED") {
		t.Errorf("markdown = %q, want TWO-CHANGED to have landed", md)
	}
	if !strings.Contains(md, "three") {
		t.Errorf("markdown = %q, want the untouched \"three\" paragraph to survive unchanged", md)
	}
	if !strings.Contains(md, "one") {
		t.Errorf("markdown = %q, want the untouched \"one\" paragraph to survive unchanged", md)
	}
}

// ---------------------------------------------------------------------------
// 5. format -> format (idempotent, second call does not write to disk)
// ---------------------------------------------------------------------------

// TestPipeline_FormatThenFormat_SecondIdenticalCallDoesNotTouchDisk goes past
// TestDocxFormat_SecondIdenticalCallReportsNoChangeNote's applied/notes
// check to the file itself: a second, byte-for-byte identical docx_format
// call must report applied:[] AND must never reach Save at all -- proven
// here by the file's mtime staying put, not just its bytes, since bytes
// staying the same alone would not rule out a rewrite that happened to
// produce identical output.
func TestPipeline_FormatThenFormat_SecondIdenticalCallDoesNotTouchDisk(t *testing.T) {
	p := docxFixture(t, "outline.docx")
	args := map[string]any{"path": p, "rules": map[string]any{"body_font": "Georgia", "line_spacing": float64(1.5)}}

	first, err := callDocxFormat(t, args)
	if err != nil {
		t.Fatalf("first DocxFormatHandler: %v", err)
	}
	firstOut := decodeRead(t, first)
	if applied, _ := firstOut["applied"].([]any); len(applied) == 0 {
		t.Fatalf("first call's applied is empty; the rules never took effect (content=%s)", first.Content)
	}
	if firstOut["backup_created"] != true {
		t.Errorf("backup_created = %v on the first (change-making) call, want true", firstOut["backup_created"])
	}

	statBefore, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	bytesBefore, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}

	second, err := callDocxFormat(t, args)
	if err != nil {
		t.Fatalf("second DocxFormatHandler: %v", err)
	}
	secondOut := decodeRead(t, second)
	if applied, _ := secondOut["applied"].([]any); len(applied) != 0 {
		t.Errorf("second identical call's applied = %v, want empty", applied)
	}
	if secondOut["backup_created"] != false {
		t.Errorf("backup_created = %v on the second (no-op) call, want false: nothing changed, so no fresh backup either", secondOut["backup_created"])
	}

	statAfter, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !statAfter.ModTime().Equal(statBefore.ModTime()) {
		t.Errorf("mtime changed on a no-op second call (before=%v after=%v); the file was written even though nothing changed",
			statBefore.ModTime(), statAfter.ModTime())
	}
	bytesAfter, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytesBefore, bytesAfter) {
		t.Error("the second identical docx_format call rewrote the file's bytes")
	}
}

// ---------------------------------------------------------------------------
// 6. read -> edit (find built from read's own \n-carrying text)
// ---------------------------------------------------------------------------

// TestPipeline_ReadThenEdit_FindBuiltFromReadsOwnLineBreakText pins task 13's
// I6 fix (edit.go's planFindTarget switching from outlineParaText to the
// same paraTextWithBreaks model Read renders) at the seam a real caller
// actually hits: copy text straight out of docx_read's markdown for a
// paragraph that contains a <w:br/>, and feed it back as docx_edit's find.
//
// Two paragraphs are exercised through the SAME read-then-edit shape:
//   - the full two-line text (spanning the <w:br/>) must be genuinely
//     LOCATED (not the pre-fix "matched 0 times") but is still correctly
//     refused, since no single run can anchor a replace across the break;
//   - a substring confined to one line of that same paragraph, extracted by
//     trimming read's own marker and trailing blank line off the markdown
//     (not hand-typed), must apply cleanly and the change must be visible
//     back through a second docx_read call.
func TestPipeline_ReadThenEdit_FindBuiltFromReadsOwnLineBreakText(t *testing.T) {
	p := bodyDocxFixture(t, `<w:p><w:r><w:t>line1</w:t></w:r><w:r><w:br/></w:r><w:r><w:t>line2</w:t></w:r></w:p>`)

	readRes, err := callDocxRead(t, map[string]any{"path": p, "full": true})
	if err != nil {
		t.Fatalf("DocxReadHandler: %v", err)
	}
	md, _ := decodeRead(t, readRes)["markdown"].(string)
	const marker = "[para 1] "
	idx := strings.Index(md, marker)
	if idx < 0 {
		t.Fatalf("markdown = %q, want a [para 1] marker", md)
	}
	rendered := strings.TrimSuffix(md[idx+len(marker):], "\n\n")
	if rendered != "line1\nline2" {
		t.Fatalf("rendered paragraph text = %q, want %q (Read's own paraTextWithBreaks rendering)", rendered, "line1\nline2")
	}

	// The full read-rendered text, \n and all: must be LOCATED, then refused
	// for spanning more than one run -- never the old silent "matched 0
	// times".
	crossRunRes, err := callDocxEdit(t, map[string]any{
		"path":  p,
		"edits": []any{map[string]any{"para": float64(1), "find": rendered, "text": "x"}},
	})
	if err != nil {
		t.Fatalf("DocxEditHandler (cross-run find): %v", err)
	}
	crossOut := decodeRead(t, crossRunRes)
	if crossOut["applied"] != float64(0) {
		t.Fatalf("applied = %v, want 0: a find spanning the <w:br/> cannot be applied as a single replace", crossOut["applied"])
	}
	outcomes, _ := crossOut["outcomes"].([]any)
	reason, _ := outcomes[0].(map[string]any)["reason"].(string)
	if strings.Contains(reason, "matched 0 times") {
		t.Fatalf("reason = %q, want the read-rendered text to have been LOCATED, not reported as 0 matches", reason)
	}
	if !strings.Contains(reason, "spans more than one run") {
		t.Errorf("reason = %q, want the cross-run refusal", reason)
	}

	// A substring confined to one line -- extracted from the SAME rendered
	// text, not hand-typed -- must apply cleanly.
	line2 := strings.Split(rendered, "\n")[1]
	editRes, err := callDocxEdit(t, map[string]any{
		"path":  p,
		"edits": []any{map[string]any{"para": float64(1), "find": line2, "text": "LINE2-CHANGED"}},
	})
	if err != nil {
		t.Fatalf("DocxEditHandler (single-line find): %v", err)
	}
	editOut := decodeRead(t, editRes)
	if editOut["applied"] != float64(1) {
		t.Fatalf("applied = %v, want 1 (content=%s)", editOut["applied"], editRes.Content)
	}

	after, err := callDocxRead(t, map[string]any{"path": p, "full": true})
	if err != nil {
		t.Fatal(err)
	}
	afterMD, _ := decodeRead(t, after)["markdown"].(string)
	if !strings.Contains(afterMD, "line1\nLINE2-CHANGED") {
		t.Errorf("markdown = %q, want %q to have taken effect", afterMD, "line1\nLINE2-CHANGED")
	}
}
