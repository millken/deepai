package docx

import (
	"strings"
	"testing"
)

// --- Task 6 (P1): notes contract + strikethrough/hard-break implementation ---
//
// write-quality report's C3: docx_write's tool description promises the
// model "an empty notes field means the input rendered exactly as written"
// (pkg/tools/builtin/docx.go), but buildNotes only ever declared images and
// ragged tables while a dozen other constructs silently degraded
// undeclared. This task implements the two constructs cheaper to render
// than to declare (~~strikethrough~~ -> <w:strike/>, and a hard line break
// -> <w:br/>) and declares the rest (inline/block HTML, footnote markers,
// task-list checkboxes, autolinks/bare URLs, unrecognized HTML entities),
// each with an occurrence count.

// TestWrite_StrikethroughBecomesRunProperty is the core red test for the
// first "cheaper to implement" item: "~~x~~" must produce a run carrying
// <w:strike/>, with the marker characters stripped from the visible text.
func TestWrite_StrikethroughBecomesRunProperty(t *testing.T) {
	d, res, _ := writeAndReopen(t, "~~deleted~~ text\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	if got, want := paraVisibleText(paras[0]), "deleted text"; got != want {
		t.Errorf("visible text = %q, want %q (strike markers stripped)", got, want)
	}
	doc, _ := d.Part(DocumentPart)
	if !strings.Contains(string(doc), "<w:strike/>") {
		t.Errorf("no <w:strike/> run property: %s", doc)
	}
	if len(res.Notes) != 0 {
		t.Errorf("Notes = %v, want none -- strikethrough is now rendered structurally", res.Notes)
	}
}

// TestWrite_StrikethroughNestsInsideBold pins that strike, like italic
// inside bold, threads through parseInlineCtx's ambient state rather than
// only working at the top level.
func TestWrite_StrikethroughNestsInsideBold(t *testing.T) {
	d, _, _ := writeAndReopen(t, "**bold ~~gone~~ still**\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	if got, want := paraVisibleText(paras[0]), "bold gone still"; got != want {
		t.Errorf("visible text = %q, want %q", got, want)
	}
	doc, _ := d.Part(DocumentPart)
	docStr := string(doc)
	if !strings.Contains(docStr, "<w:b/><w:strike/>") {
		t.Errorf("expected a run with BOTH bold and strike properties: %s", docStr)
	}
}

// TestWrite_UnclosedStrikethroughStaysLiteral matches every other marker's
// well-defined degrade for an unclosed pair (see parseInlineCtx's doc
// comment): the "~~" characters survive as plain text rather than being
// swallowed or erroring.
func TestWrite_UnclosedStrikethroughStaysLiteral(t *testing.T) {
	d, _, _ := writeAndReopen(t, "a ~~b\n")
	if got, want := paraVisibleText(d.Paras()[0]), "a ~~b"; got != want {
		t.Errorf("visible text = %q, want %q", got, want)
	}
}

// TestWrite_TrailingTwoSpacesIsHardLineBreak is the core red test for the
// second "cheaper to implement" item: a line ending in two-or-more trailing
// spaces, immediately followed by more paragraph text, becomes a <w:br/>
// instead of being soft-wrapped into the next line with an ordinary space.
func TestWrite_TrailingTwoSpacesIsHardLineBreak(t *testing.T) {
	d, res, _ := writeAndReopen(t, "line one  \nline two\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1 (still one soft-wrapped paragraph, not two)", len(paras))
	}
	p := paras[0]
	if len(p.Runs) != 2 {
		t.Fatalf("got %d runs, want 2 (one per line, split by the hard break): %+v", len(p.Runs), p.Runs)
	}
	if p.Runs[0].Text != "line one" || p.Runs[1].Text != "line two" {
		t.Errorf("run text = %q / %q, want %q / %q -- no leftover trailing spaces and no leading space on the second line", p.Runs[0].Text, p.Runs[1].Text, "line one", "line two")
	}
	if len(p.Breaks) != 1 || p.Breaks[0] != 1 {
		t.Errorf("Breaks = %v, want [1] (a <w:br/> after run 1)", p.Breaks)
	}
	doc, _ := d.Part(DocumentPart)
	if !strings.Contains(string(doc), "<w:r><w:br/></w:r>") {
		t.Errorf("no textless <w:r><w:br/></w:r>: %s", doc)
	}
	if len(res.Notes) != 0 {
		t.Errorf("Notes = %v, want none -- hard line breaks are now rendered structurally", res.Notes)
	}
}

// TestWrite_TrailingBackslashIsHardLineBreak is the same construct's other
// CommonMark spelling. Task 4 (write_inline_fixes_test.go) made a
// backslash before a NON-punctuation character (a letter, e.g. "C:\path")
// survive as a literal backslash; this is a different shape entirely -- a
// backslash at the true end of a source line, with more paragraph text
// following on the next line -- and must now become a break, not literal
// text, reversing this package's own prior behavior for that specific
// shape.
func TestWrite_TrailingBackslashIsHardLineBreak(t *testing.T) {
	d, _, _ := writeAndReopen(t, "line one\\\nline two\n")
	paras := d.Paras()
	if len(paras) != 1 {
		t.Fatalf("got %d paragraphs, want 1", len(paras))
	}
	p := paras[0]
	if len(p.Runs) != 2 || p.Runs[0].Text != "line one" || p.Runs[1].Text != "line two" {
		t.Errorf("runs = %+v, want [\"line one\" \"line two\"] with no backslash surviving", p.Runs)
	}
	if len(p.Breaks) != 1 {
		t.Errorf("Breaks = %v, want exactly 1 entry", p.Breaks)
	}
}

// TestWrite_BackslashBeforeNonPunctuationStaysLiteral (Task 4) must be
// unaffected: "C:\path" has no line break at all -- the backslash there
// sits mid-line, before a letter, not at the end of a line with more
// paragraph text following it. This test re-pins that alongside Task 6's
// new trailing-backslash rule specifically so the two behaviors are
// checked side by side, not just independently.
func TestWrite_BackslashBeforeLetterStillLiteralAlongsideHardBreak(t *testing.T) {
	d, _, _ := writeAndReopen(t, `C:\path`+"\n")
	if got, want := paraVisibleText(d.Paras()[0]), `C:\path`; got != want {
		t.Errorf("visible text = %q, want %q", got, want)
	}
}

// TestWrite_DoubledTrailingBackslashStaysLiteral: "\\\\" at the end of a
// line is CommonMark's escaped, literal backslash (the first backslash
// escapes the second), not a hard-break marker -- splitTrailingHardBreak's
// own doc comment calls this out explicitly.
func TestWrite_DoubledTrailingBackslashStaysLiteral(t *testing.T) {
	d, _, _ := writeAndReopen(t, "line one\\\\\nline two\n")
	p := d.Paras()[0]
	if len(p.Breaks) != 0 {
		t.Errorf("Breaks = %v, want none -- a doubled trailing backslash is literal, not a break", p.Breaks)
	}
}

// TestWrite_TrailingBackslashFollowedBySpaceIsNotAHardBreak is code-review
// fix #2: a backslash followed by a trailing space ("line\ ") is neither
// hard-break shape -- not 2+ trailing spaces (there's only one), and not a
// backslash at the true end of the line either (a space follows it) --  so
// it must fall through to the ordinary case: the single trailing space is
// swallowed by the general trim, and the lone backslash survives as
// literal text (CommonMark: backslash before whitespace is not an
// escape). An earlier version of splitTrailingHardBreak trimmed trailing
// spaces/tabs BEFORE checking the backslash suffix, which wrongly treated
// this the same as a true trailing "\" with nothing after it.
func TestWrite_TrailingBackslashFollowedBySpaceIsNotAHardBreak(t *testing.T) {
	d, _, _ := writeAndReopen(t, "line one\\ \nline two\n")
	p := d.Paras()[0]
	if len(p.Breaks) != 0 {
		t.Errorf("Breaks = %v, want none -- a trailing backslash followed by a space is not a hard break", p.Breaks)
	}
	if !strings.Contains(paraVisibleText(p), `\`) {
		t.Errorf("visible text = %q, want the lone backslash to survive as literal text", paraVisibleText(p))
	}
}

// TestWrite_HardBreakInListItemDoesNotLeakMarker guards against the
// hardBreakMarker sentinel character ever surviving into rendered text for
// a paraBlock kind that never goes through buildBlocks' accLines/flush path
// at all (a list item's own text is a single line, assigned directly by
// buildBlocks' listItemRE branch, never joined via joinWithHardBreaks) --
// i.e. this is a basic sanity check that ordinary list items are
// unaffected by this task.
func TestWrite_HardBreakInListItemDoesNotLeakMarker(t *testing.T) {
	d, _, _ := writeAndReopen(t, "- item one\n- item two\n")
	for _, p := range d.Paras() {
		if strings.Contains(paraVisibleText(p), hardBreakMarker) {
			t.Fatalf("hardBreakMarker leaked into rendered text: %q", paraVisibleText(p))
		}
	}
}

// TestWrite_HardBreakInsideBlockQuote is code-review fix #3: a block
// quote joins its own lines the same soft-wrap-by-default way an ordinary
// paragraph does (flushQuote's prior plain strings.Join(quoteLines, " ")
// mirrored flush()'s own prior behavior exactly), so a hard line break
// inside a quote must get the same <w:br/> treatment a hard break in an
// ordinary paragraph already does, not silently do nothing (the two-heads-
// empty gap the review flagged: neither implemented nor declared).
func TestWrite_HardBreakInsideBlockQuote(t *testing.T) {
	d, _, _ := writeAndReopen(t, "> line one  \n> line two\n")
	var quote *Para
	for i, p := range d.Paras() {
		if p.Style == StyleQuote {
			quote = &d.Paras()[i]
		}
	}
	if quote == nil {
		t.Fatal("no Quote-styled paragraph found")
	}
	if len(quote.Runs) != 2 || quote.Runs[0].Text != "line one" || quote.Runs[1].Text != "line two" {
		t.Errorf("quote runs = %+v, want [\"line one\" \"line two\"]", quote.Runs)
	}
	if len(quote.Breaks) != 1 {
		t.Errorf("Breaks = %v, want exactly 1 entry (a <w:br/> inside the quote)", quote.Breaks)
	}
}

// TestWrite_TripleAsteriskAloneIsHorizontalRule and
// TestWrite_TripleUnderscoreAloneIsHorizontalRule pin the "implement rather
// than declare" choice for CommonMark's other two thematic-break spellings
// (the brief allows either implementing them as a real hr or declaring them
// undeclared-literal; extending hrRE is the cheaper of the two).
func TestWrite_TripleAsteriskAloneIsHorizontalRule(t *testing.T) {
	d, res, _ := writeAndReopen(t, "text\n\n***\n\nmore\n")
	doc, _ := d.Part(DocumentPart)
	if !strings.Contains(string(doc), hrBorderXML) {
		t.Error("a standalone '***' line was not rendered as a horizontal rule")
	}
	if len(res.Notes) != 0 {
		t.Errorf("Notes = %v, want none", res.Notes)
	}
}

func TestWrite_TripleUnderscoreAloneIsHorizontalRule(t *testing.T) {
	d, _, _ := writeAndReopen(t, "text\n\n___\n\nmore\n")
	doc, _ := d.Part(DocumentPart)
	if !strings.Contains(string(doc), hrBorderXML) {
		t.Error("a standalone '___' line was not rendered as a horizontal rule")
	}
}

// TestWrite_TripleAsteriskEmphasisStillWorksAlongsideHR guards the new hr
// spellings against the pre-existing "***bold italic***" inline marker
// (write_inline_fixes_test.go): only a line consisting ENTIRELY of 3+
// asterisks is a rule; "***text***" mid-paragraph is unaffected.
func TestWrite_TripleAsteriskEmphasisStillWorksAlongsideHR(t *testing.T) {
	d, _, _ := writeAndReopen(t, "***bold italic***\n")
	doc, _ := d.Part(DocumentPart)
	if strings.Contains(string(doc), hrBorderXML) {
		t.Error("\"***bold italic***\" was misclassified as a horizontal rule")
	}
	if got, want := paraVisibleText(d.Paras()[0]), "bold italic"; got != want {
		t.Errorf("visible text = %q, want %q", got, want)
	}
}

// --- The notes-declaration half: five silent-degradation shapes that stay
// undeclared literal text, each now declared with an occurrence count. ---

func TestWrite_InlineHTMLIsDeclaredInNotes(t *testing.T) {
	_, res, _ := writeAndReopen(t, "before <span>middle</span> after\n\n<div>block</div>\n")
	joined := strings.Join(res.Notes, " | ")
	if !strings.Contains(joined, "HTML tag") {
		t.Errorf("Notes = %v, want an HTML-tag note", res.Notes)
	}
	if !strings.Contains(joined, "4 ") {
		t.Errorf("Notes = %v, want the HTML-tag note to count all 4 tags (<span>,</span>,<div>,</div>)", res.Notes)
	}
}

func TestWrite_FootnoteIsDeclaredInNotes(t *testing.T) {
	_, res, _ := writeAndReopen(t, "See the note[^1] for detail.\n\n[^1]: Some detail.\n")
	joined := strings.Join(res.Notes, " | ")
	if !strings.Contains(joined, "footnote") {
		t.Errorf("Notes = %v, want a footnote note", res.Notes)
	}
	if !strings.Contains(joined, "2 ") {
		t.Errorf("Notes = %v, want the footnote note to count both the reference and the definition marker", res.Notes)
	}
}

func TestWrite_TaskListIsDeclaredInNotes(t *testing.T) {
	_, res, _ := writeAndReopen(t, "- [ ] todo one\n- [x] done one\n")
	joined := strings.Join(res.Notes, " | ")
	if !strings.Contains(joined, "task-list") {
		t.Errorf("Notes = %v, want a task-list note", res.Notes)
	}
	if !strings.Contains(joined, "2 ") {
		t.Errorf("Notes = %v, want the task-list note to count both checkboxes", res.Notes)
	}
}

func TestWrite_AutolinkAndBareURLAreDeclaredInNotes(t *testing.T) {
	_, res, _ := writeAndReopen(t, "See <https://example.com> or just https://example.org directly.\n")
	joined := strings.Join(res.Notes, " | ")
	if !strings.Contains(joined, "autolink") || !strings.Contains(joined, "bare URL") {
		t.Errorf("Notes = %v, want an autolink/bare-URL note", res.Notes)
	}
	if !strings.Contains(joined, "2 ") {
		t.Errorf("Notes = %v, want the note to count both the autolink and the bare URL", res.Notes)
	}
}

// TestWrite_RealHyperlinkIsNotCountedAsBareURL guards
// detectStructuralGaps' own linkSpanRE exclusion: a URL that IS already a
// real `[text](url)` hyperlink must not also be declared as an
// undeclared-literal bare URL.
func TestWrite_RealHyperlinkIsNotCountedAsBareURL(t *testing.T) {
	_, res, _ := writeAndReopen(t, "[see this](https://example.com/page)\n")
	joined := strings.Join(res.Notes, " | ")
	if strings.Contains(joined, "autolink") || strings.Contains(joined, "bare URL") {
		t.Errorf("Notes = %v, want no autolink/bare-URL note -- this URL is already a real hyperlink", res.Notes)
	}
}

func TestWrite_UnknownHTMLEntityIsDeclaredInNotes(t *testing.T) {
	_, res, _ := writeAndReopen(t, "weird &foo; entity\n")
	joined := strings.Join(res.Notes, " | ")
	if !strings.Contains(joined, "unrecognized HTML entit") {
		t.Errorf("Notes = %v, want an unrecognized-entity note", res.Notes)
	}
	if !strings.Contains(joined, "1 ") {
		t.Errorf("Notes = %v, want the note to count the single occurrence", res.Notes)
	}
}

// TestWrite_KnownHTMLEntityIsNotDeclaredUnknown guards against a false
// positive: &nbsp;/&amp;/etc. are recognized and decoded (pre-Task-6
// behavior, namedHTMLEntities), so they must never appear in an
// "unrecognized" note.
func TestWrite_KnownHTMLEntityIsNotDeclaredUnknown(t *testing.T) {
	_, res, _ := writeAndReopen(t, "a&nbsp;b &amp; &hellip;\n")
	joined := strings.Join(res.Notes, " | ")
	if strings.Contains(joined, "unrecognized") {
		t.Errorf("Notes = %v, want no unrecognized-entity note -- all three are recognized", res.Notes)
	}
}

// TestWrite_ComprehensiveSupportedDocumentProducesNoNotes is the notes
// contract's positive half: a document exercising every construct this
// package DOES fully support -- across Tasks 2-6 -- must still produce an
// empty Notes slice. This is the fixture the brief asks for: "构造一个覆盖
// 全部已支持构造的 markdown 断言 notes==[]". Per the code-review pass on
// this task, it also folds in a fenced code block and an inline `code`
// span whose CONTENT is deliberately shaped like every one of the five
// declared-in-notes constructs (an HTML tag, a bare URL, a task-list
// checkbox, a footnote marker) -- code content renders completely
// verbatim (Item 1) and so must never itself trigger any of those notes;
// see structuralGaps/collectProseText's own doc comments for the false-
// positive this guards against.
func TestWrite_ComprehensiveSupportedDocumentProducesNoNotes(t *testing.T) {
	md := strings.Join([]string{
		"# Heading One",
		"",
		"Setext Heading",
		"==============",
		"",
		"Setext Two",
		"----------",
		"",
		"Body **bold** and *italic* and ***both*** and ~~struck~~ and `code`, " +
			"escaped \\*not em\\* and 100\\% done.",
		"",
		"An inline code span that looks like HTML/a URL: `<div>https://fake.example/x</div>`.",
		"",
		"Hard break line one  ",
		"hard break line two.",
		"",
		"- bullet one",
		"- bullet two",
		"  - nested bullet",
		"",
		"1. ordered one",
		"2. ordered two",
		"",
		"| a | b |",
		"| --- | --- |",
		"| 1 | a\\|b |",
		"",
		"> a block quote  ",
		"> with a hard break inside it",
		"",
		"---",
		"",
		"***",
		"",
		"___",
		"",
		"```",
		"<div>fake html in a code block</div>",
		"- [ ] fake task list item in a code block",
		"see https://fake.example.com/in-code-block",
		"[^1] fake footnote marker in a code block",
		"```",
		"",
		"    <span>fake html in an indented code block</span>",
		"",
		"[a link](https://example.com/page \"Example Title\")",
		"",
	}, "\n")
	_, res, _ := writeAndReopen(t, md)
	if len(res.Notes) != 0 {
		t.Errorf("Notes = %v, want none for fully supported input", res.Notes)
	}
}

// TestWrite_CodeBlockContentDoesNotTriggerNotesFalsePositives is the
// isolated, minimal version of the same code-review fix, kept separate
// from the larger comprehensive fixture above so a future regression here
// points directly at the cause: a document consisting of ONLY a heading
// plus a fenced code block whose content is shaped like HTML/a bare URL/a
// task-list item/a footnote marker, plus one inline code span shaped like
// an HTML tag, must produce no notes at all -- none of that content is
// ever run through parseInline, so none of it can have silently degraded.
func TestWrite_CodeBlockContentDoesNotTriggerNotesFalsePositives(t *testing.T) {
	md := strings.Join([]string{
		"# Title",
		"",
		"See `<div>inline code, not real HTML</div>`.",
		"",
		"```",
		"<div>not real html</div>",
		"- [ ] not a real task list item",
		"https://fake.example.com/not-a-real-bare-url",
		"<https://fake.example.com/not-a-real-autolink>",
		"[^1] not a real footnote marker",
		"```",
		"",
	}, "\n")
	_, res, _ := writeAndReopen(t, md)
	if len(res.Notes) != 0 {
		t.Errorf("Notes = %v, want none -- code block/inline code content must never trigger a notes false positive", res.Notes)
	}
}
