package docx

import (
	"strings"
	"testing"
)

// --- applyDirectRunFormat: font/size direct formatting on <w:r><w:rPr> ---

// TestDirect_OnlyTheRangeIsTouched pins that a range selects exactly the
// named paragraphs: text and tags of every paragraph outside [from,to] must
// come through byte for byte.
func TestDirect_OnlyTheRangeIsTouched(t *testing.T) {
	doc := []byte(
		`<w:p><w:r><w:t>one</w:t></w:r></w:p>` +
			`<w:p><w:r><w:t>two</w:t></w:r></w:p>` +
			`<w:p><w:r><w:t>three</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, n, err := applyDirectRunFormat(doc, paras, 2, 2, "Georgia", "", 0)
	if err != nil {
		t.Fatalf("applyDirectRunFormat: %v", err)
	}
	if n != 1 {
		t.Errorf("changed %d paragraphs, want 1", n)
	}
	s := string(got)

	gotParas, err := Scan(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotParas) != 3 {
		t.Fatalf("got %d paragraphs, want 3", len(gotParas))
	}
	// Paragraph 1 and 3's <w:r> must be untouched, byte for byte.
	p1 := s[gotParas[0].Span.Start:gotParas[0].Span.End]
	if p1 != `<w:p><w:r><w:t>one</w:t></w:r></w:p>` {
		t.Errorf("paragraph 1 was touched: %s", p1)
	}
	p3 := s[gotParas[2].Span.Start:gotParas[2].Span.End]
	if p3 != `<w:p><w:r><w:t>three</w:t></w:r></w:p>` {
		t.Errorf("paragraph 3 was touched: %s", p3)
	}
	p2 := s[gotParas[1].Span.Start:gotParas[1].Span.End]
	if !strings.Contains(p2, `w:ascii="Georgia"`) {
		t.Errorf("paragraph 2 was not given the requested font: %s", p2)
	}
}

// TestDirect_MergesIntoExistingRunProperties is the trap this task exists to
// avoid: a run's existing <w:rPr> (bold, colour) must survive a direct
// format that only wants to change size.
func TestDirect_MergesIntoExistingRunProperties(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:rPr><w:b/><w:color w:val="FF0000"/></w:rPr><w:t>x</w:t></w:r></w:p>`)
	paras, _ := Scan(doc)
	got, n, err := applyDirectRunFormat(doc, paras, 1, 1, "", "", 14)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("changed %d paragraphs, want 1", n)
	}
	s := string(got)
	if !strings.Contains(s, "<w:b/>") || !strings.Contains(s, `w:val="FF0000"`) {
		t.Errorf("existing run properties were wiped: %s", s)
	}
	if !strings.Contains(s, `<w:sz w:val="28"/>`) || !strings.Contains(s, `<w:szCs w:val="28"/>`) {
		t.Errorf("size not applied or szCs not synced: %s", s)
	}
}

// TestDirect_InsertsRunPropertiesWhenAbsent covers a run with no <w:rPr> at
// all: one must be inserted as the run's first child.
func TestDirect_InsertsRunPropertiesWhenAbsent(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:t>x</w:t></w:r></w:p>`)
	paras, _ := Scan(doc)
	got, n, err := applyDirectRunFormat(doc, paras, 1, 1, "Georgia", "", 14)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("changed %d paragraphs, want 1", n)
	}
	s := string(got)
	if !strings.Contains(s, `<w:r><w:rPr>`) {
		t.Errorf("rPr was not inserted as the run's first child: %s", s)
	}
	if !strings.Contains(s, `w:ascii="Georgia"`) {
		t.Errorf("font not applied: %s", s)
	}
	if !strings.Contains(s, `<w:sz w:val="28"/>`) || !strings.Contains(s, `<w:szCs w:val="28"/>`) {
		t.Errorf("size not applied or szCs not synced: %s", s)
	}
	if !strings.Contains(s, `<w:t>x</w:t>`) {
		t.Errorf("run text was disturbed: %s", s)
	}
}

// TestDirect_UpdatesAnExistingSizeInsteadOfDuplicating is idempotency's
// prerequisite: a size that is already present must be changed in place,
// never duplicated as a second <w:sz>.
func TestDirect_UpdatesAnExistingSizeInsteadOfDuplicating(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:rPr><w:sz w:val="20"/><w:szCs w:val="20"/></w:rPr><w:t>x</w:t></w:r></w:p>`)
	paras, _ := Scan(doc)
	got, n, err := applyDirectRunFormat(doc, paras, 1, 1, "", "", 14)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("changed %d paragraphs, want 1", n)
	}
	s := string(got)
	if strings.Count(s, "<w:sz ") != 1 {
		t.Errorf("w:sz was duplicated instead of updated: %s", s)
	}
	if strings.Count(s, "<w:szCs ") != 1 {
		t.Errorf("w:szCs was duplicated instead of updated: %s", s)
	}
	if !strings.Contains(s, `<w:sz w:val="28"/>`) {
		t.Errorf("existing size was not updated: %s", s)
	}
	if strings.Contains(s, `w:val="20"`) {
		t.Errorf("old size value survived alongside the new one: %s", s)
	}
}

// TestDirect_IsIdempotent applies the same run format twice and requires
// byte-identical output — without in-place updates the XML would grow a new
// <w:sz>/<w:rFonts> on every call until Word rejected the file.
func TestDirect_IsIdempotent(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:rPr><w:b/></w:rPr><w:t>x</w:t></w:r></w:p><w:p><w:r><w:t>y</w:t></w:r></w:p>`)
	paras, _ := Scan(doc)
	once, n1, err := applyDirectRunFormat(doc, paras, 1, 2, "Georgia", "", 14)
	if err != nil {
		t.Fatal(err)
	}
	paras2, err := Scan(once)
	if err != nil {
		t.Fatal(err)
	}
	twice, n2, err := applyDirectRunFormat(once, paras2, 1, 2, "Georgia", "", 14)
	if err != nil {
		t.Fatal(err)
	}
	if n1 != 2 || n2 != 2 {
		t.Errorf("changed counts = %d, %d, want 2, 2", n1, n2)
	}
	if string(once) != string(twice) {
		t.Errorf("applying the same format twice was not idempotent:\nonce:  %s\ntwice: %s", once, twice)
	}
}

// TestDirect_EmptyParagraphSkippedForRunFormatButNotForParagraphFormat
// covers the plan's explicit rule: a paragraph with no runs at all is
// skipped by run-level formatting (nothing to attach a <w:rPr> to) but
// still receives paragraph-level formatting (line spacing/alignment lands
// on <w:pPr> regardless of runs).
func TestDirect_EmptyParagraphSkippedForRunFormatButNotForParagraphFormat(t *testing.T) {
	doc := []byte(`<w:p/><w:p><w:r><w:t>x</w:t></w:r></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(paras[0].Runs) != 0 {
		t.Fatalf("paragraph 1 fixture must have zero runs")
	}

	runOut, runN, err := applyDirectRunFormat(doc, paras, 1, 2, "Georgia", "", 14)
	if err != nil {
		t.Fatal(err)
	}
	if runN != 1 {
		t.Errorf("run-level changed %d paragraphs, want 1 (paragraph 1 has no runs to skip)", runN)
	}
	if string(runOut) == string(doc) {
		t.Fatal("run-level formatting made no change at all; the sanity check is vacuous")
	}
	gotParas, err := Scan(runOut)
	if err != nil {
		t.Fatal(err)
	}
	p1 := string(runOut)[gotParas[0].Span.Start:gotParas[0].Span.End]
	if p1 != `<w:p/>` {
		t.Errorf("empty paragraph 1 was touched by run-level formatting: %s", p1)
	}

	paraOut, paraN, err := applyDirectParaFormat(doc, paras, 1, 2, pParaRequest{LineSpacing: 1.5, Align: ""})
	if err != nil {
		t.Fatal(err)
	}
	if paraN != 2 {
		t.Errorf("paragraph-level changed %d paragraphs, want 2 (empty paragraphs are NOT skipped)", paraN)
	}
	gotParas2, err := Scan(paraOut)
	if err != nil {
		t.Fatal(err)
	}
	p1b := string(paraOut)[gotParas2[0].Span.Start:gotParas2[0].Span.End]
	if !strings.Contains(p1b, `w:line="360"`) {
		t.Errorf("empty paragraph 1 did not receive line-spacing formatting: %s", p1b)
	}
}

// TestDirect_LeavesTextUntouched is the run-level analogue of §4.3's core
// promise: direct formatting never rewrites a run's visible text.
func TestDirect_LeavesTextUntouched(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:t>hello world</w:t></w:r></w:p>`)
	paras, _ := Scan(doc)
	got, _, err := applyDirectRunFormat(doc, paras, 1, 1, "Georgia", "", 14)
	if err != nil {
		t.Fatal(err)
	}
	gotParas, err := Scan(got)
	if err != nil {
		t.Fatal(err)
	}
	if gotParas[0].Runs[0].Text != "hello world" {
		t.Errorf("run text changed: %q", gotParas[0].Runs[0].Text)
	}
}

// TestDirect_RunInsideHyperlinkIsFormatted checks that a run nested one
// level deeper (inside <w:hyperlink>, per scan.go's Run.Elem doc comment)
// is still found and formatted correctly: Run.Elem spans only the <w:r>,
// not the enclosing <w:hyperlink>.
func TestDirect_RunInsideHyperlinkIsFormatted(t *testing.T) {
	doc := []byte(`<w:p><w:hyperlink r:id="rId1"><w:r><w:t>link text</w:t></w:r></w:hyperlink></w:p>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, n, err := applyDirectRunFormat(doc, paras, 1, 1, "Georgia", "", 14)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("changed %d paragraphs, want 1", n)
	}
	s := string(got)
	if !strings.Contains(s, "<w:hyperlink") || !strings.Contains(s, "</w:hyperlink>") {
		t.Errorf("hyperlink wrapper was damaged: %s", s)
	}
	if !strings.Contains(s, `w:ascii="Georgia"`) {
		t.Errorf("run inside hyperlink was not formatted: %s", s)
	}
	if err := checkWellFormed(got); err != nil {
		t.Errorf("result is not well-formed XML: %v", err)
	}
}

// --- applyDirectParaFormat: line spacing/alignment on <w:p><w:pPr> ---

// TestDirect_MergesIntoExistingParagraphProperties covers the same three
// merge cases as the run-level test, but for a paragraph's own <w:pPr>: no
// pPr at all, pPr present but lacking the target property, and pPr already
// carrying the target property.
func TestDirect_MergesIntoExistingParagraphProperties(t *testing.T) {
	t.Run("no pPr at all", func(t *testing.T) {
		doc := []byte(`<w:p><w:r><w:t>x</w:t></w:r></w:p>`)
		paras, _ := Scan(doc)
		got, n, err := applyDirectParaFormat(doc, paras, 1, 1, pParaRequest{LineSpacing: 1.5, Align: "justify"})
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("changed %d paragraphs, want 1", n)
		}
		s := string(got)
		if !strings.Contains(s, `w:line="360"`) || !strings.Contains(s, `w:lineRule="auto"`) {
			t.Errorf("line spacing not inserted: %s", s)
		}
		if !strings.Contains(s, `<w:jc w:val="justify"/>`) {
			t.Errorf("alignment not inserted: %s", s)
		}
	})

	t.Run("pPr present but lacking the target property", func(t *testing.T) {
		doc := []byte(`<w:p><w:pPr><w:keepNext/></w:pPr><w:r><w:t>x</w:t></w:r></w:p>`)
		paras, _ := Scan(doc)
		got, n, err := applyDirectParaFormat(doc, paras, 1, 1, pParaRequest{LineSpacing: 1.5, Align: "justify"})
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("changed %d paragraphs, want 1", n)
		}
		s := string(got)
		if !strings.Contains(s, "<w:keepNext/>") {
			t.Errorf("existing paragraph property was wiped: %s", s)
		}
		if !strings.Contains(s, `w:line="360"`) {
			t.Errorf("line spacing not appended: %s", s)
		}
		if !strings.Contains(s, `<w:jc w:val="justify"/>`) {
			t.Errorf("alignment not appended: %s", s)
		}
	})

	t.Run("target property already present", func(t *testing.T) {
		doc := []byte(`<w:p><w:pPr><w:spacing w:line="240" w:lineRule="auto"/><w:jc w:val="left"/></w:pPr><w:r><w:t>x</w:t></w:r></w:p>`)
		paras, _ := Scan(doc)
		got, n, err := applyDirectParaFormat(doc, paras, 1, 1, pParaRequest{LineSpacing: 1.5, Align: "justify"})
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("changed %d paragraphs, want 1", n)
		}
		s := string(got)
		if strings.Count(s, "<w:spacing ") != 1 {
			t.Errorf("w:spacing was duplicated instead of updated: %s", s)
		}
		if strings.Count(s, "<w:jc ") != 1 {
			t.Errorf("w:jc was duplicated instead of updated: %s", s)
		}
		if !strings.Contains(s, `w:line="360"`) || strings.Contains(s, `w:line="240"`) {
			t.Errorf("line spacing value was not updated in place: %s", s)
		}
		if !strings.Contains(s, `w:val="justify"`) || strings.Contains(s, `w:val="left"`) {
			t.Errorf("alignment value was not updated in place: %s", s)
		}
	})
}

// TestDirect_ParagraphFormatExpandsSelfClosingParagraph covers a <w:p/>
// (e.g. a genuinely empty paragraph, the same shape Format's Normalize path
// already treats as empty) receiving paragraph-level formatting: it must be
// expanded into <w:p><w:pPr>...</w:pPr></w:p>, not silently skipped.
func TestDirect_ParagraphFormatExpandsSelfClosingParagraph(t *testing.T) {
	doc := []byte(`<w:p/>`)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, n, err := applyDirectParaFormat(doc, paras, 1, 1, pParaRequest{LineSpacing: 1.5, Align: "justify"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("changed %d paragraphs, want 1", n)
	}
	s := string(got)
	if !strings.Contains(s, `w:line="360"`) || !strings.Contains(s, `<w:jc w:val="justify"/>`) {
		t.Errorf("self-closing paragraph did not receive formatting: %s", s)
	}
	if err := checkWellFormed(got); err != nil {
		t.Errorf("result is not well-formed XML: %v", err)
	}
	// Applying again must be idempotent too, now that the paragraph is no
	// longer self-closing.
	gotParas, err := Scan(got)
	if err != nil {
		t.Fatal(err)
	}
	twice, n2, err := applyDirectParaFormat(got, gotParas, 1, 1, pParaRequest{LineSpacing: 1.5, Align: "justify"})
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 1 {
		t.Errorf("changed %d paragraphs on the second pass, want 1", n2)
	}
	if string(got) != string(twice) {
		t.Errorf("expanding a self-closing paragraph was not idempotent:\nonce:  %s\ntwice: %s", got, twice)
	}
}

// TestDirect_ParagraphFormatIsIdempotent mirrors the run-level idempotency
// test for the paragraph-level path.
func TestDirect_ParagraphFormatIsIdempotent(t *testing.T) {
	doc := []byte(`<w:p><w:pPr><w:jc w:val="left"/></w:pPr><w:r><w:t>x</w:t></w:r></w:p>`)
	paras, _ := Scan(doc)
	once, n1, err := applyDirectParaFormat(doc, paras, 1, 1, pParaRequest{LineSpacing: 1.15, Align: "justify"})
	if err != nil {
		t.Fatal(err)
	}
	paras2, err := Scan(once)
	if err != nil {
		t.Fatal(err)
	}
	twice, n2, err := applyDirectParaFormat(once, paras2, 1, 1, pParaRequest{LineSpacing: 1.15, Align: "justify"})
	if err != nil {
		t.Fatal(err)
	}
	if n1 != 1 || n2 != 1 {
		t.Errorf("changed counts = %d, %d, want 1, 1", n1, n2)
	}
	if string(once) != string(twice) {
		t.Errorf("applying the same paragraph format twice was not idempotent:\nonce:  %s\ntwice: %s", once, twice)
	}
}

// TestDirect_NoFieldsIsANoOp pins that calling either function with no
// font/size (or no spacing/align) requested changes nothing at all, not
// even byte-identical-but-reallocated output that would confuse a caller
// diffing before/after.
func TestDirect_NoFieldsIsANoOp(t *testing.T) {
	doc := []byte(`<w:p><w:r><w:rPr><w:b/></w:rPr><w:t>x</w:t></w:r></w:p>`)
	paras, _ := Scan(doc)

	got, n, err := applyDirectRunFormat(doc, paras, 1, 1, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("changed %d paragraphs, want 0 for an empty request", n)
	}
	if string(got) != string(doc) {
		t.Errorf("output changed for an empty run-format request: %s", got)
	}

	got2, n2, err := applyDirectParaFormat(doc, paras, 1, 1, pParaRequest{LineSpacing: 0, Align: ""})
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Errorf("changed %d paragraphs, want 0 for an empty request", n2)
	}
	if string(got2) != string(doc) {
		t.Errorf("output changed for an empty paragraph-format request: %s", got2)
	}
}

// --- Document.Format dispatch: StartPara/EndPara routing and validation ---

func directFixtureDoc(t *testing.T) *Document {
	t.Helper()
	return bodyDoc(t,
		`<w:p><w:r><w:t>one</w:t></w:r></w:p>`+
			`<w:p><w:r><w:t>two</w:t></w:r></w:p>`+
			`<w:p><w:r><w:t>three</w:t></w:r></w:p>`)
}

// TestFormat_RangeAppliesDirectFormattingToDocumentXML is the end-to-end
// dispatch test: a StartPara/EndPara call must land in word/document.xml as
// direct formatting, touching only the named paragraph.
func TestFormat_RangeAppliesDirectFormattingToDocumentXML(t *testing.T) {
	d := directFixtureDoc(t)
	res, err := d.Format(FormatOptions{StartPara: 2, BodySizePt: 14, LineSpacing: 1.5})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if len(res.Applied) == 0 {
		t.Error("Applied is empty; the caller cannot tell what changed")
	}
	doc, _ := d.Part(DocumentPart)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(paras) != 3 {
		t.Fatalf("got %d paragraphs, want 3", len(paras))
	}
	p1 := string(doc)[paras[0].Span.Start:paras[0].Span.End]
	if p1 != `<w:p><w:r><w:t>one</w:t></w:r></w:p>` {
		t.Errorf("paragraph 1 outside the range was touched: %s", p1)
	}
	p2 := string(doc)[paras[1].Span.Start:paras[1].Span.End]
	if !strings.Contains(p2, `<w:sz w:val="28"/>`) {
		t.Errorf("paragraph 2 did not get the requested size: %s", p2)
	}
	if !strings.Contains(p2, `w:line="360"`) {
		t.Errorf("paragraph 2 did not get the requested line spacing: %s", p2)
	}
}

// TestFormat_RangeNotesSkippedEmptyParagraphsForRunFormat covers the
// dispatch layer's own responsibility (not applyDirectRunFormat's, which
// only returns a count): computing and reporting, via FormatResult.Notes,
// how many paragraphs in the range had no runs and were therefore skipped
// for font/size — while still confirming the (present) paragraph did get
// formatted, so the note is not covering for a call that silently did
// nothing at all.
func TestFormat_RangeNotesSkippedEmptyParagraphsForRunFormat(t *testing.T) {
	d := bodyDoc(t, `<w:p/><w:p><w:r><w:t>x</w:t></w:r></w:p>`)
	res, err := d.Format(FormatOptions{StartPara: 1, EndPara: 2, BodySizePt: 14})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	found := false
	for _, n := range res.Notes {
		if strings.Contains(n, "1 empty paragraph") {
			found = true
		}
	}
	if !found {
		t.Errorf("Notes = %v, want a note about 1 skipped empty paragraph", res.Notes)
	}
	doc, _ := d.Part(DocumentPart)
	if !strings.Contains(string(doc), `<w:sz w:val="28"/>`) {
		t.Errorf("the non-empty paragraph in range was not formatted: %s", doc)
	}
}

// TestFormat_EndParaDefaultsToStartPara covers "EndPara 为 0 且 StartPara
// 设置时表示只该一段" — omitting EndPara must format exactly one paragraph.
func TestFormat_EndParaDefaultsToStartPara(t *testing.T) {
	d := directFixtureDoc(t)
	if _, err := d.Format(FormatOptions{StartPara: 2, BodySizePt: 14}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	doc, _ := d.Part(DocumentPart)
	paras, err := Scan(doc)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range paras {
		text := string(doc)[p.Span.Start:p.Span.End]
		has := strings.Contains(text, `<w:sz w:val="28"/>`)
		if i == 1 && !has {
			t.Errorf("paragraph 2 was not formatted")
		}
		if i != 1 && has {
			t.Errorf("paragraph %d was formatted, want only paragraph 2", i+1)
		}
	}
}

// TestFormat_EndParaWithoutStartParaErrors covers the other half: EndPara
// set while StartPara is 0 (whole-document mode) is ambiguous and must
// error rather than silently doing the whole-document thing or silently
// picking a range.
func TestFormat_EndParaWithoutStartParaErrors(t *testing.T) {
	d := directFixtureDoc(t)
	if _, err := d.Format(FormatOptions{EndPara: 2, BodySizePt: 14}); err == nil {
		t.Fatal("EndPara without StartPara was accepted; want an error")
	}
}

// TestFormat_InvertedRangeErrors covers EndPara < StartPara.
func TestFormat_InvertedRangeErrors(t *testing.T) {
	d := directFixtureDoc(t)
	if _, err := d.Format(FormatOptions{StartPara: 3, EndPara: 1, BodySizePt: 14}); err == nil {
		t.Fatal("an inverted range (end before start) was accepted; want an error")
	}
}

// TestFormat_OutOfBoundsRangeErrors covers both StartPara and EndPara past
// the document's actual paragraph count.
func TestFormat_OutOfBoundsRangeErrors(t *testing.T) {
	d := directFixtureDoc(t)
	if _, err := d.Format(FormatOptions{StartPara: 99, BodySizePt: 14}); err == nil {
		t.Fatal("an out-of-range StartPara was accepted; want an error")
	}
	if _, err := d.Format(FormatOptions{StartPara: 1, EndPara: 99, BodySizePt: 14}); err == nil {
		t.Fatal("an out-of-range EndPara was accepted; want an error")
	}
	// StartPara 0 falls through to the whole-document path; this synthetic
	// fixture (bodyDoc) carries no word/styles.xml, so exercise a field
	// that only touches word/document.xml instead of one that would fail
	// for that unrelated reason.
	if _, err := d.Format(FormatOptions{StartPara: 0, Normalize: true}); err != nil {
		t.Fatalf("StartPara 0 (whole document) must not itself error: %v", err)
	}
}

// TestFormat_DocumentOnlyRulesErrorWithARange pins the plan's explicit
// list: template, heading_font, margins_mm, and normalize only make sense
// document-wide and must error rather than silently apply (or silently be
// dropped) when combined with a paragraph range.
func TestFormat_DocumentOnlyRulesErrorWithARange(t *testing.T) {
	cases := []struct {
		name string
		opts FormatOptions
	}{
		{"template", FormatOptions{StartPara: 1, Template: "corporate"}},
		{"heading_font", FormatOptions{StartPara: 1, HeadingFont: "Georgia"}},
		{"margins_mm", FormatOptions{StartPara: 1, MarginsMM: []float64{25.4, 25.4, 25.4, 25.4}}},
		{"normalize", FormatOptions{StartPara: 1, Normalize: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := directFixtureDoc(t)
			_, err := d.Format(c.opts)
			if err == nil {
				t.Fatalf("%s combined with a range was accepted; want an error", c.name)
			}
		})
	}
}

// TestFormat_RangeAcceptsAllFourAlignments pins task 8's Critical-3 fix:
// align used to accept only "left"/"justify" (and format_direct_test.go
// used to lock in a hard rejection of "center" right here) — "center" and
// "right" are now first-class, on both the range path (this test) and the
// whole-document path (TestFormat_WholeDocumentAcceptsAllFourAlignments
// below).
func TestFormat_RangeAcceptsAllFourAlignments(t *testing.T) {
	for _, align := range []string{"left", "center", "right", "justify"} {
		t.Run(align, func(t *testing.T) {
			d := directFixtureDoc(t)
			if _, err := d.Format(FormatOptions{StartPara: 1, Align: align}); err != nil {
				t.Fatalf("Format with align=%q: %v", align, err)
			}
			doc, _ := d.Part(DocumentPart)
			want := `<w:jc w:val="` + align + `"/>`
			if !strings.Contains(string(doc), want) {
				t.Errorf("document.xml lacks %s: %s", want, doc)
			}
		})
	}
}

// TestFormat_WholeDocumentAcceptsAllFourAlignments is
// TestFormat_RangeAcceptsAllFourAlignments's whole-document-path twin.
func TestFormat_WholeDocumentAcceptsAllFourAlignments(t *testing.T) {
	for _, align := range []string{"left", "center", "right", "justify"} {
		t.Run(align, func(t *testing.T) {
			out := applyStylesPatches(t, []byte(stylesEmptyDocDefaults), FormatOptions{Align: align})
			want := `<w:jc w:val="` + align + `"/>`
			if !strings.Contains(out, want) {
				t.Errorf("styles.xml lacks %s: %s", want, out)
			}
		})
	}
}

// TestFormat_RangeRejectsUnknownAlignment covers a genuinely unknown value
// (not one of the four real ones) — validation must still reject something
// that isn't a real alignment at all.
func TestFormat_RangeRejectsUnknownAlignment(t *testing.T) {
	d := directFixtureDoc(t)
	if _, err := d.Format(FormatOptions{StartPara: 1, Align: "middle"}); err == nil {
		t.Fatal("an unknown alignment was accepted with a range; want an error")
	}
}

// TestFormat_LineSpacingAndLineSpacingExactPtAreMutuallyExclusive covers
// the brief's explicit validation requirement: giving both in the SAME
// call must error, on both paths.
func TestFormat_LineSpacingAndLineSpacingExactPtAreMutuallyExclusive(t *testing.T) {
	t.Run("whole document", func(t *testing.T) {
		d := directFixtureDoc(t)
		if _, err := d.Format(FormatOptions{LineSpacing: 1.5, LineSpacingExactPt: 24}); err == nil {
			t.Fatal("line_spacing + line_spacing_exact_pt together was accepted; want an error")
		}
	})
	t.Run("paragraph range", func(t *testing.T) {
		d := directFixtureDoc(t)
		if _, err := d.Format(FormatOptions{StartPara: 1, LineSpacing: 1.5, LineSpacingExactPt: 24}); err == nil {
			t.Fatal("line_spacing + line_spacing_exact_pt together was accepted with a range; want an error")
		}
	})
}
