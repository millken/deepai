package docx

// This file adds the one capability the rest of pkg/docx deliberately does
// not have: creating a brand-new .docx from scratch. Everything else in
// this package exists to preserve an original file's bytes as closely as
// possible while making a narrow edit (see splice.go, edit.go). There is no
// original file here — WriteDocx authors a minimal OOXML package directly
// with archive/zip rather than going through Package/zipio.go, whose Open
// only accepts existing files and whose SetPart refuses unknown entry
// names (both exist to protect content this file never touches). Building
// the file with a from-scratch DOM is exactly what those guarantees are
// for, and it is safe here because there is nothing to lose.

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// WriteOptions configures WriteDocx.
type WriteOptions struct {
	// Markdown is the source document. Supported syntax: ATX headings
	// (# .. ######), blank-line-separated paragraphs, **bold**/__bold__,
	// *italic*/_italic_, `inline code`, unordered/ordered lists (including
	// nesting -- see inferListIndentUnit for how leading-space indentation
	// maps to nesting level), GFM pipe tables (header row, alignment row,
	// bold header, ragged rows padded/truncated and declared -- see
	// parseTable), fenced ``` code blocks (markdown is not interpreted
	// inside one -- see paraBlock.isCode), [text](url) links (rendered as
	// real hyperlinks -- see renderCtx), "> " block quotes, and a "---"
	// horizontal rule (disambiguated from a table separator row and a
	// setext heading underline -- see buildBlocks' hrRE branch). The one
	// thing this package never renders is an image (![alt](url)): no part
	// of its OOXML skeleton embeds binary data, so an image is written
	// verbatim as plain text and declared in WriteResult.Notes -- see
	// detectImages/buildNotes.
	Markdown string
	// Title, when non-empty, becomes the document's first paragraph,
	// styled as Heading1 ahead of anything parsed from Markdown.
	//
	// The alternative was writing it into docProps/core.xml's <dc:title>
	// instead (or as well). That would need a sixth OOXML part purely to
	// carry a value most readers show only in file-properties dialogs,
	// never in the document body. A visible Heading1 is what a reader
	// opening the file actually sees, needs no extra part, and is what a
	// user asking to "give this document a title" almost always means.
	Title string
}

// WriteResult reports what WriteDocx produced.
type WriteResult struct {
	// Paras is the total number of <w:p> elements written to
	// word/document.xml: Title's heading (if any), ordinary paragraphs,
	// list items, and every table cell's paragraph (header and data,
	// empty cells included) all count. This is deliberately the same
	// count Scan would report via len(Document.Paras()) — see
	// TestWrite_TitleBecomesLeadingHeading1, which checks exactly that.
	Paras int
	// Notes declares markdown syntax WriteDocx does not render
	// structurally (links, images, code fences), plus one specific
	// structural compromise inside an otherwise-supported table: a data
	// row whose cell count did not match its header, whose extra cells
	// were dropped and whose missing cells were padded empty (see
	// parseTable). An empty Notes means every construct in the input was
	// fully supported. This must never be left for the caller to discover
	// by opening the file — see the package doc for why silent flattening
	// is the failure mode this package always avoids.
	Notes []string
}

// docxEpoch pins every zip entry this file writes to the same instant.
// archive/zip stamps entries with time.Now() by default, so without this
// the same markdown would produce different bytes on every run: untestable
// (TestWrite_IsDeterministic depends on it), and a user diffing two
// generated documents would see spurious changes in the zip shell even
// when the visible content is identical. This exact failure mode already
// bit testdata/gen_fixtures.py (see FIXED_DATE_TIME there) before it was
// fixed here from the start.
var docxEpoch = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// WriteDocx creates a new .docx at path from opts.Markdown (see
// WriteOptions for the supported subset), refusing to overwrite an
// existing file: creating must never destroy something the caller did not
// explicitly ask to replace. Callers that want to replace an existing file
// must remove it (or choose another path) first.
func WriteDocx(path string, opts WriteOptions) (WriteResult, error) {
	blocks, notes := parseMarkdown(opts)

	ctx := newRenderCtx()
	var body strings.Builder
	body.WriteString(documentXMLHeader)
	paraCount := 0
	for _, b := range blocks {
		switch {
		case b.para != nil:
			p, err := renderParagraph(*b.para, ctx)
			if err != nil {
				return WriteResult{}, fmt.Errorf("docx: render paragraph: %w", err)
			}
			body.WriteString(p)
			paraCount++
		case b.table != nil:
			t, n, err := renderTable(b.table, ctx)
			if err != nil {
				return WriteResult{}, fmt.Errorf("docx: render table: %w", err)
			}
			body.WriteString(t)
			paraCount += n
		}
	}
	body.WriteString(documentXMLFooter)

	entries := []zipEntry{
		{name: contentTypesPart, data: []byte(contentTypesXML)},
		{name: "_rels/.rels", data: []byte(rootRelsXML)},
		{name: DocumentPart, data: []byte(body.String())},
		{name: "word/_rels/document.xml.rels", data: []byte(buildDocRelsXML(ctx.rels))},
		{name: "word/styles.xml", data: []byte(stylesPartXML)},
		{name: "word/numbering.xml", data: []byte(numberingXML)},
	}

	if err := writeNewDocx(path, entries); err != nil {
		return WriteResult{}, err
	}

	return WriteResult{Paras: paraCount, Notes: notes}, nil
}

// zipEntry is one part to write, paired with an explicit order: the entry
// slice WriteDocx builds, not map iteration, is what makes writeNewDocx's
// output byte-for-byte reproducible across runs.
type zipEntry struct {
	name string
	data []byte
}

// writeNewDocx serializes entries into a zip built entirely in memory, then
// creates path exclusively (O_EXCL) and writes the finished bytes in one
// shot. Building the whole archive before ever touching the filesystem
// means a failure while rendering XML or writing zip entries never creates
// a partial file at path; O_EXCL makes the existence check and the create
// a single atomic syscall, so nothing can race between "does path exist"
// and "create it" the way a separate stat-then-open would.
func writeNewDocx(path string, entries []zipEntry) error {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{
			Name:     e.name,
			Method:   zip.Deflate,
			Modified: docxEpoch,
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return fmt.Errorf("docx: create entry %s: %w", e.name, err)
		}
		if _, err := w.Write(e.data); err != nil {
			return fmt.Errorf("docx: write entry %s: %w", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("docx: finalize zip: %w", err)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("docx: refusing to overwrite existing file %q; delete it or choose another path first", path)
		}
		return fmt.Errorf("docx: create %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(buf.Bytes()); err != nil {
		os.Remove(path)
		return fmt.Errorf("docx: write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		os.Remove(path)
		return fmt.Errorf("docx: sync %s: %w", path, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Markdown -> blocks
// ---------------------------------------------------------------------------

// block is one top-level body element: exactly one of para or table is
// non-nil. A table cannot be represented as a paraBlock — <w:tbl> is a
// sibling of <w:p> in the body, containing its own rows and cells, each
// cell in turn containing one or more <w:p> — so WriteDocx and buildBlocks
// dispatch on which field is set rather than forcing both shapes through
// one struct.
type block struct {
	para  *paraBlock
	table *tableBlock
}

// paraBlock is one paragraph to render: heading is 0 for an ordinary
// paragraph or 1-6 for a heading level, and text carries the still-raw
// markdown (bold/italic markers intact) that renderParagraph parses into
// runs.
//
// isList, listLevel and listOrdered describe a list item's <w:numPr>
// (listLevel/listOrdered are meaningless when isList is false). jc carries
// a table cell's GFM column alignment ("" | "left" | "center" | "right");
// it is meaningless outside a table cell, where paragraphs are always
// left-aligned by Word's default. forceBold ORs bold onto every inline
// segment regardless of markdown markers — used only for a table header
// row (see renderTable); markdown parsing itself never sets it.
//
// isCode, isQuote and isHR are Task 3's three additional paragraph kinds,
// mutually exclusive with each other and with isList/heading in practice
// (buildBlocks never sets more than one). isCode marks one line of a
// fenced code block: text is rendered completely literally — renderParagraph
// skips parseInline for it entirely, so "**bold**" inside a code block
// never becomes a run — in a monospace font on a lightly shaded paragraph.
// isQuote marks a block-quote paragraph (left border + indent); isHR marks
// a horizontal-rule paragraph, which carries no text at all (text is
// ignored when isHR is set — see renderParagraph).
type paraBlock struct {
	heading     int
	text        string
	isList      bool
	listLevel   int
	listOrdered bool
	jc          string
	forceBold   bool
	isCode      bool
	isQuote     bool
	isHR        bool
}

// tableCell is one <w:tc>'s content: raw markdown text (inline emphasis is
// still resolved at render time via parseInline, exactly as for an
// ordinary paragraph — see TestWrite_ListItemRunsInlineEmphasis's table
// analogue) and the column's GFM alignment ("" | "left" | "center" |
// "right").
type tableCell struct {
	text  string
	align string
}

// tableRow is one <w:tr>. header marks the row parsed from the line before
// the GFM separator row; renderTable forces bold on every cell in that row
// (the brief: "the header row is bold").
type tableRow struct {
	header bool
	cells  []tableCell
}

// tableBlock is one parsed GFM pipe table. cols is fixed by the header row
// (the brief: "Column count comes from the header row"); every row's cells
// slice has exactly len == cols, padded/truncated by buildRowCells so
// renderTable never has to special-case a short or long row.
type tableBlock struct {
	cols int
	rows []tableRow
}

var (
	// headingRE matches an ATX heading line: 1-6 '#' characters, then
	// either end of line or whitespace followed by the heading text. A run
	// of 7 or more '#' can never satisfy this — group 1 is capped at 6, and
	// whatever '#' characters remain after that are neither whitespace nor
	// end-of-string, so the match fails entirely and the line falls through
	// to being treated as an ordinary paragraph, hashes included verbatim.
	// That mirrors CommonMark, which caps ATX headings at level 6 for the
	// same reason.
	headingRE = regexp.MustCompile(`^(#{1,6})(?:\s+(.*))?$`)
	// closingHashRE strips a CommonMark-style closing sequence from a
	// heading's text, e.g. "Title ###" -> "Title". The closing run must be
	// preceded by whitespace; "Title###" (no space) is left alone, since
	// without that space the hashes are part of the text, not a closer.
	closingHashRE = regexp.MustCompile(`\s+#+\s*$`)
	// listItemRE matches one list item line, capturing leading indentation
	// (group 1, used to compute nesting level — see inferListIndentUnit),
	// the marker (group 2: "-", "*", "+" for unordered, or "<digits>." /
	// "<digits>)" for ordered), and the item's content after the marker
	// (group 3). The required run of whitespace after the marker, and the
	// required non-whitespace first character of the content, are what
	// keep "*italic*" (no space after the opening '*') from ever being
	// mistaken for a bullet.
	listItemRE = regexp.MustCompile(`^(\s*)([-*+]|\d+[.)])\s+(\S.*)$`)
	// tableSepRE matches a line built only from pipes, colons, dashes, and
	// whitespace -- the header/body separator row of a pipe table, e.g.
	// "|---|---|" or ":-- | --:". isTableSeparator additionally requires
	// at least one '-', so a blank or all-pipe line doesn't count.
	tableSepRE = regexp.MustCompile(`^[\s|:-]+$`)
	// fenceRE matches a fenced-code-block delimiter, ``` or ~~~, opening or
	// closing. Any info string after the delimiter (e.g. "go" in "```go")
	// is part of the same match and is simply discarded by buildBlocks --
	// Item 1 requires it be ignored, not rejected.
	fenceRE = regexp.MustCompile("^(```|~~~)")
	// imageRE matches `![alt](url)` images -- the one inline construct this
	// package never renders structurally (no part of the OOXML skeleton
	// embeds binary image data). detectImages uses it to decide whether
	// WriteResult.Notes must declare images as unsupported. It intentionally
	// does NOT also match plain `[text](url)` links: those are Item 2's
	// hyperlinks now, resolved and rendered inline by parseInlineCtx, not
	// detected up front the way an unsupported construct is.
	imageRE = regexp.MustCompile(`!\[[^\]\n]*\]\([^)\n]*\)`)
	// hrRE matches a thematic-break line: three or more hyphens and nothing
	// else (the brief's own examples, and the one form this package
	// supports -- CommonMark also allows "***"/"___" and interior spaces,
	// which this does not implement). See buildBlocks' hrRE branch for how
	// this is disambiguated from a GFM table separator row and a setext
	// heading underline.
	hrRE = regexp.MustCompile(`^-{3,}$`)
	// quoteRE matches one block-quote line, capturing its content after the
	// "> " (or bare ">") marker. The optional single space after '>' mirrors
	// CommonMark, which does not require it.
	quoteRE = regexp.MustCompile(`^>\s?(.*)$`)
)

// parseMarkdown turns opts into the blocks WriteDocx renders and the Notes
// it reports. Title, when set, is prepended as its own Heading1 block
// ahead of anything parsed from Markdown. If nothing produced any block at
// all (empty or all-blank input, no Title), a single empty paragraph
// block is emitted instead of zero: a .docx body conventionally always
// carries at least one paragraph, and this is the only way to guarantee
// that without also making every ordinary blank separator produce spurious
// empty paragraphs (see buildBlocks).
func parseMarkdown(opts WriteOptions) ([]block, []string) {
	unit := inferListIndentUnit(opts.Markdown)
	blocks, tableNotes := buildBlocks(opts.Markdown, unit)
	hasImage := detectImages(opts.Markdown)
	notes := buildNotes(hasImage, tableNotes)

	if opts.Title != "" {
		blocks = append([]block{{para: &paraBlock{heading: 1, text: opts.Title}}}, blocks...)
	}
	if len(blocks) == 0 {
		blocks = []block{{para: &paraBlock{heading: 0, text: ""}}}
	}
	return blocks, notes
}

// buildBlocks walks Markdown line by line and produces one block per
// heading, per ordinary paragraph, per list item, per table (which
// consumes multiple lines at once via parseTable), per block-quote run,
// per horizontal rule, and per line of a fenced code block. Ordinary
// paragraph lines and block-quote lines are each merged with a single
// space within their own run (Markdown's soft-line-break model — see
// flush/flushQuote below); everything else is emitted immediately as its
// own block rather than merged with neighbors, since merging e.g. two list
// items into one paragraph would make already-special content actively
// misleading. unit is the number of leading spaces that make one list
// nesting level — see inferListIndentUnit, which computes it once per
// document before this function is called.
func buildBlocks(markdown string, unit int) (blocks []block, tableNotes []string) {
	markdown = strings.ReplaceAll(markdown, "\r\n", "\n")
	lines := strings.Split(markdown, "\n")

	var accLines []string
	flush := func() {
		if len(accLines) == 0 {
			return
		}
		text := strings.Join(accLines, " ")
		accLines = accLines[:0]
		if strings.TrimSpace(text) == "" {
			return
		}
		blocks = append(blocks, block{para: &paraBlock{heading: 0, text: text}})
	}

	var quoteLines []string
	flushQuote := func() {
		if len(quoteLines) == 0 {
			return
		}
		text := strings.Join(quoteLines, " ")
		quoteLines = quoteLines[:0]
		blocks = append(blocks, block{para: &paraBlock{text: text, isQuote: true}})
	}

	inFence := false
	tableIndex := 0
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if fenceRE.MatchString(trimmed) {
			flush()
			flushQuote()
			inFence = !inFence
			// The opening/closing delimiter itself -- and any info string
			// after it, e.g. "go" in "```go" -- is consumed here and never
			// becomes a paragraph: Item 1 says the info string is ignored,
			// not an error, and a fence marker is punctuation, not content.
			continue
		}
		if inFence {
			// Inside a fence every line — including a blank one — is its
			// own literal code paragraph, never merged with neighbors (a
			// blank line in source code is meaningful spacing, not "end of
			// paragraph") and never run through parseInline (Item 1:
			// markdown is not interpreted inside code — see
			// paraBlock.isCode and renderParagraph). Leading whitespace is
			// kept verbatim by using line, not trimmed.
			//
			// Self-review: an unterminated fence is never closed, so
			// inFence never flips back to false and every remaining line
			// in the document — headings, lists, everything — is consumed
			// as code, right up to EOF. That matches CommonMark's own rule
			// for an unclosed fenced code block (it runs to the end of the
			// document) rather than being a bug unique to this package;
			// see TestWrite_UnterminatedFenceRunsToEndOfDocument.
			blocks = append(blocks, block{para: &paraBlock{text: line, isCode: true}})
			continue
		}
		if trimmed == "" {
			flush()
			flushQuote()
			continue
		}
		if m := headingRE.FindStringSubmatch(trimmed); m != nil {
			flush()
			flushQuote()
			text := closingHashRE.ReplaceAllString(m[2], "")
			blocks = append(blocks, block{para: &paraBlock{heading: len(m[1]), text: text}})
			continue
		}
		if m := listItemRE.FindStringSubmatch(line); m != nil {
			flush()
			flushQuote()
			level := len(m[1]) / unit
			if level > maxListLevel {
				level = maxListLevel
			}
			ordered := m[2][0] >= '0' && m[2][0] <= '9'
			blocks = append(blocks, block{para: &paraBlock{
				text:        m[3],
				isList:      true,
				listLevel:   level,
				listOrdered: ordered,
			}})
			continue
		}
		if strings.Contains(trimmed, "|") && i+1 < len(lines) && isTableSeparator(lines[i+1]) {
			flush()
			flushQuote()
			tableIndex++
			tb, consumed, note := parseTable(lines, i, tableIndex)
			blocks = append(blocks, block{table: tb})
			if note != "" {
				tableNotes = append(tableNotes, note)
			}
			i += consumed - 1
			continue
		}
		// hrRE only ever matches here, after the table branch above has
		// already had first refusal: a line that legitimately serves as a
		// GFM separator row was already consumed as part of its table (the
		// "i += consumed - 1" above skips past it), so this loop iteration
		// never revisits it. What remains ambiguous is the OTHER case the
		// brief warns about: "---" underlining a preceding paragraph
		// (a setext heading, which this package does not implement). The
		// rule chosen here is "only a horizontal rule when it is not
		// immediately continuing an in-progress paragraph" — checked via
		// len(accLines) == 0 — so "Heading\n---" (no blank line between)
		// falls through to the accLines append below instead, becoming the
		// literal paragraph "Heading ---" rather than either a heading or a
		// swallowed rule. A "---" preceded by a blank line, a heading, a
		// list item, or nothing at all (accLines empty) is a rule.
		if hrRE.MatchString(trimmed) && len(accLines) == 0 {
			flushQuote()
			blocks = append(blocks, block{para: &paraBlock{isHR: true}})
			continue
		}
		if m := quoteRE.FindStringSubmatch(trimmed); m != nil {
			flush()
			quoteLines = append(quoteLines, m[1])
			continue
		}
		flushQuote()
		if strings.Contains(trimmed, "|") {
			// A pipe character with no valid GFM separator row right after
			// it is not a table -- e.g. ordinary prose like "cost |
			// benefit" -- so it is written as plain text and, deliberately,
			// declared nowhere: it was never recognized as an attempted
			// table in the first place, only as a line worth keeping on
			// its own rather than merged into surrounding prose (matching
			// Task 1's behavior for this exact case).
			flush()
			blocks = append(blocks, block{para: &paraBlock{heading: 0, text: trimmed}})
			continue
		}
		accLines = append(accLines, trimmed)
	}
	flush()
	flushQuote()
	return blocks, tableNotes
}

// inferListIndentUnit scans markdown for list-item lines (skipping fenced
// code, so a code sample containing lines that happen to look like list
// items never pollutes this) and returns the smallest positive
// leading-space count among them: that width is treated as "one nesting
// level" for the WHOLE document. This replaces a unit pinned at a fixed
// constant, which made a 4-space-indented document (CommonMark's own
// canonical indent width) come out at ilvl 0, 2, 4 instead of 0, 1, 2 —
// double depth, skipping levels, wrong bullet glyph per level in Word.
//
// A document that mixes indentation widths (say, 2 spaces in one place and
// 3 in another) still gets a single, predictable answer: every item's
// level is that item's leading-space count divided by this one inferred
// unit (integer division, in buildBlocks), never a stack that guesses "is
// this a new level" from context. That can round two DIFFERENT indent
// widths down to the same level (e.g. unit=2: both 2 and 3 spaces map to
// level 1) rather than raising an error — a predictable lossy mapping was
// chosen over rejecting input a person would consider reasonably
// well-formed.
//
// A document with no nested list items at all (every item at indent 0, or
// no list items whatsoever) has nothing to infer a unit from; 2 is
// returned in that case as an arbitrary but harmless default, since no
// item's level calculation is affected by it either way (0 / anything ==
// 0).
func inferListIndentUnit(markdown string) int {
	markdown = strings.ReplaceAll(markdown, "\r\n", "\n")
	lines := strings.Split(markdown, "\n")

	const defaultUnit = 2
	inFence := false
	min := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if fenceRE.MatchString(trimmed) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		m := listItemRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n := len(m[1])
		if n > 0 && (min == 0 || n < min) {
			min = n
		}
	}
	if min == 0 {
		return defaultUnit
	}
	return min
}

// isTableSeparator reports whether line is a pipe-table separator row, e.g.
// "|---|---|" or ":-- | --:".
func isTableSeparator(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" || !strings.Contains(t, "-") {
		return false
	}
	return tableSepRE.MatchString(t)
}

// detectImages scans the raw markdown for `![alt](url)` images,
// independently of buildBlocks' line classification: an image can appear
// inline inside an otherwise ordinary paragraph, so it needs its own pass
// rather than being tied to a specific line shape. Unlike Task 2's
// detectLinksAndImages, this no longer also detects plain `[text](url)`
// links: those are Item 2's hyperlinks now, resolved by parseInlineCtx and
// rendered structurally, not declared unsupported.
func detectImages(markdown string) bool {
	return imageRE.MatchString(markdown)
}

// buildNotes turns what buildBlocks/detectImages observed into
// WriteResult.Notes. Lists, tables, code blocks, and links are NOT
// declared here: all four are now rendered structurally (lists/tables as
// of Task 2, code/links as of this task), so declaring them unsupported
// would be actively wrong (see the updated
// TestWrite_UnsupportedSyntaxIsDeclared, which now asserts their absence).
// Images remain the one inline construct this package never renders (no
// part of the OOXML skeleton embeds binary data), so they are still
// declared. tableNotes carries a narrower declaration for a specific
// structural compromise even within an otherwise-supported,
// well-formed-enough table: a ragged row's cells were dropped or padded
// rather than silently misaligning the rest of the table — see
// parseTable. Nothing here is declared unconditionally:
// TestWrite_SupportedOnlyInputProducesNoNotes depends on fully-supported
// input producing an empty slice.
func buildNotes(hasImage bool, tableNotes []string) []string {
	var notes []string
	if hasImage {
		notes = append(notes, "images are not embedded; written as plain text")
	}
	notes = append(notes, tableNotes...)
	return notes
}

// ---------------------------------------------------------------------------
// Tables
// ---------------------------------------------------------------------------

// parseTable parses a GFM pipe table starting at lines[start] (the header
// row), given that lines[start+1] is already confirmed to be a valid
// separator row (buildBlocks checks this before calling). It returns the
// table block, how many source lines it consumed (header + separator +
// data rows, so the caller can skip past all of them), and a note
// describing a ragged row if any data row's cell count did not match the
// header's.
//
// Self-review: a pipe-containing line with no separator row right after it
// is simply not a table at all — see buildBlocks' fallback branch and
// TestWrite_PipeLineWithoutSeparatorIsNotATable — so that case never
// reaches this function.
//
// A blank line, or a line with no '|' at all, ends the table: real GFM
// tables do not span a blank line, and a data row is defined by having
// cells to split in the first place.
func parseTable(lines []string, start, tableIndex int) (tb *tableBlock, consumed int, note string) {
	headerCells := splitTableRow(lines[start])
	cols := len(headerCells)
	align := parseAlignRow(lines[start+1], cols)

	tb = &tableBlock{cols: cols}
	tb.rows = append(tb.rows, tableRow{header: true, cells: buildRowCells(headerCells, align, cols)})
	consumed = 2

	ragged := false
	for j := start + 2; j < len(lines); j++ {
		t := strings.TrimSpace(lines[j])
		if t == "" || !strings.Contains(t, "|") {
			break
		}
		cells := splitTableRow(lines[j])
		if len(cells) != cols {
			ragged = true
		}
		tb.rows = append(tb.rows, tableRow{cells: buildRowCells(cells, align, cols)})
		consumed++
	}

	if ragged {
		note = fmt.Sprintf(
			"table %d: a row had a different number of cells than its %d-column header; extra cells were dropped and missing cells were padded empty",
			tableIndex, cols,
		)
	}
	return tb, consumed, note
}

// buildRowCells pads or truncates cells to exactly want columns (see
// parseTable — this is where a ragged row's extra cells are silently
// dropped and missing cells silently padded, always paired with the
// caller declaring it in Notes) and pairs each with its column's
// alignment.
func buildRowCells(cells, align []string, want int) []tableCell {
	out := make([]tableCell, want)
	for c := 0; c < want; c++ {
		text := ""
		if c < len(cells) {
			text = cells[c]
		}
		a := ""
		if c < len(align) {
			a = align[c]
		}
		out[c] = tableCell{text: text, align: a}
	}
	return out
}

// splitTableRow splits one pipe-table row into cell strings, tolerating
// optional leading/trailing pipes ("| a | b |" and "a | b" both split into
// ["a", "b"]).
func splitTableRow(line string) []string {
	t := strings.TrimSpace(line)
	t = strings.TrimPrefix(t, "|")
	t = strings.TrimSuffix(t, "|")
	parts := strings.Split(t, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// parseAlignRow reads the GFM alignment markers off the separator row
// (":---" left, ":---:" center, "---:" right, "---" unspecified/default)
// and pads or truncates to cols the same way a data row would be, so a
// separator with a different cell count than the header still lines up
// positionally rather than panicking or misaligning later columns.
func parseAlignRow(sepLine string, cols int) []string {
	cells := splitTableRow(sepLine)
	out := make([]string, cols)
	for c := 0; c < cols; c++ {
		if c < len(cells) {
			out[c] = cellAlign(cells[c])
		}
	}
	return out
}

// cellAlign classifies one separator cell. An explicit leading colon with
// no trailing one ("left") gets its own <w:jc w:val="left"/> even though
// that already matches Word's default paragraph alignment — emitting it
// keeps this function's output symmetric with center/right rather than
// special-casing "explicit but visually identical to the default".
func cellAlign(s string) string {
	s = strings.TrimSpace(s)
	left := strings.HasPrefix(s, ":")
	right := strings.HasSuffix(s, ":")
	switch {
	case left && right:
		return "center"
	case right:
		return "right"
	case left:
		return "left"
	default:
		return ""
	}
}

// renderTable renders one tableBlock as a <w:tbl> element and reports how
// many <w:p> it produced (every cell gets exactly one, header or data,
// empty or not — see the brief on why an empty <w:tc> without a <w:p>
// makes Word offer to repair the file). A single trailing empty paragraph
// is appended after the closing </w:tbl> unconditionally: OOXML technically
// permits a table to be the last block in the body, but this package has
// already hit the "empty/missing paragraph next to a table" defect once
// (P1b's I3, a paragraph delete that emptied a table cell), so the safer
// choice is to never let a table be adjacent to nothing at all.
//
// ctx is threaded into every cell's renderParagraph call, the same as it is
// for top-level paragraphs in WriteDocx: a link inside a table cell (e.g. a
// "see [spec](url)" cell) needs the same document-wide, always-unique
// relationship-id allocation as a link anywhere else in the body — see
// TestWrite_LinkInsideTableCellGetsAHyperlink.
func renderTable(tb *tableBlock, ctx *renderCtx) (string, int, error) {
	var out strings.Builder
	out.WriteString("<w:tbl>")
	out.WriteString(tableBordersXML)
	out.WriteString("<w:tblGrid>")
	for c := 0; c < tb.cols; c++ {
		out.WriteString(`<w:gridCol w:w="2000"/>`)
	}
	out.WriteString("</w:tblGrid>")

	paraCount := 0
	for _, row := range tb.rows {
		out.WriteString("<w:tr>")
		for _, cell := range row.cells {
			p, err := renderParagraph(paraBlock{text: cell.text, jc: cell.align, forceBold: row.header}, ctx)
			if err != nil {
				return "", 0, err
			}
			paraCount++
			out.WriteString("<w:tc>")
			out.WriteString(p)
			out.WriteString("</w:tc>")
		}
		out.WriteString("</w:tr>")
	}
	out.WriteString("</w:tbl>")
	out.WriteString("<w:p></w:p>")
	paraCount++
	return out.String(), paraCount, nil
}

// tableBordersXML is the <w:tblPr> every generated table shares: a single
// thin border on every edge and between every cell. Column widths in
// <w:tblGrid> (see renderTable) are placeholders — w:type="auto" on
// <w:tblW> lets Word size columns to content rather than trusting them.
const tableBordersXML = `<w:tblPr><w:tblW w:w="0" w:type="auto"/><w:tblBorders>` +
	`<w:top w:val="single" w:sz="4" w:space="0" w:color="auto"/>` +
	`<w:left w:val="single" w:sz="4" w:space="0" w:color="auto"/>` +
	`<w:bottom w:val="single" w:sz="4" w:space="0" w:color="auto"/>` +
	`<w:right w:val="single" w:sz="4" w:space="0" w:color="auto"/>` +
	`<w:insideH w:val="single" w:sz="4" w:space="0" w:color="auto"/>` +
	`<w:insideV w:val="single" w:sz="4" w:space="0" w:color="auto"/>` +
	`</w:tblBorders></w:tblPr>`

// ---------------------------------------------------------------------------
// Inline emphasis (bold/italic)
// ---------------------------------------------------------------------------

// segment is one run of paragraph text sharing the same formatting state.
// code marks inline `code` (monospace font, no nested markdown resolved
// inside it). link, when non-empty, is the URL of a `[text](url)` this
// segment's text came from; renderParagraph groups consecutive segments
// sharing the same link URL into one <w:hyperlink>. bold/italic/code/link
// are independent bits — a link's visible text can itself contain bold or
// italic (see parseInlineCtx's '[' branch), though code+link can only
// arise from the unusual “ [`code`](url) “ and is rendered with the
// hyperlink style taking precedence over the monospace font, same as any
// other bold/italic combination with code.
type segment struct {
	text   string
	bold   bool
	italic bool
	code   bool
	link   string
}

// parseInline splits a paragraph's raw text into segments, resolving
// inline `code` spans (highest precedence — nothing inside them is
// interpreted further), `[text](url)` links (whose own text is then
// recursively resolved for bold/italic/code), and
// **bold**/__bold__/*italic*/_italic_ markers, including one level of
// nesting either way (e.g. "**bold *and italic* together**").
//
// The rule for a marker with no matching close before the end of the text —
// "**unclosed bold", say — is to treat the marker characters as literal
// text rather than search across paragraph boundaries or guess: the two (or
// one) marker characters are appended to the current segment as-is and
// scanning continues from there. This is not full CommonMark (which has
// additional rules about intraword underscores and marker precedence this
// does not implement), but it is well-defined and can never produce
// unbalanced or invalid output, because every branch of parseInlineCtx
// either fully resolves a marker pair or emits its characters as plain
// text — there is no path that leaves a dangling <w:rPr> or half-written
// run.
func parseInline(s string) []segment {
	return parseInlineCtx(s, false, false)
}

// parseInlineCtx is parseInline's recursive worker: bold and italic are the
// formatting already in effect from an enclosing marker pair (ambient
// state), and any marker resolved here sets its own flag with the other
// left untouched.
func parseInlineCtx(s string, bold, italic bool) []segment {
	var segs []segment
	var buf strings.Builder
	flush := func() {
		if buf.Len() > 0 {
			segs = append(segs, segment{text: buf.String(), bold: bold, italic: italic})
			buf.Reset()
		}
	}

	i, n := 0, len(s)
	for i < n {
		c := s[i]
		if c == '`' {
			// Inline code has the highest precedence: whatever is between
			// the backticks is emitted completely literally, with no
			// recursion — "`**not bold**`" must stay literal text inside a
			// monospace run, per Item 1.
			if j := strings.IndexByte(s[i+1:], '`'); j >= 0 {
				flush()
				segs = append(segs, segment{text: s[i+1 : i+1+j], bold: bold, italic: italic, code: true})
				i += 1 + j + 1
				continue
			}
			// Unclosed inline-code marker: keep the backtick as literal text.
			buf.WriteByte(c)
			i++
			continue
		}
		if c == '[' && (i == 0 || s[i-1] != '!') {
			// A leading '!' immediately before '[' marks a `![alt](url)`
			// image, which this function deliberately leaves untouched
			// (falls through to the default byte-copy case below): images
			// are declared unsupported at the document level (see
			// detectImages), not resolved into a link here.
			if text, url, end, ok := matchLinkAt(s, i); ok {
				flush()
				inner := parseInlineCtx(text, bold, italic)
				for _, seg := range inner {
					seg.link = url
					segs = append(segs, seg)
				}
				i = end
				continue
			}
		}
		if (c == '*' || c == '_') && i+1 < n && s[i+1] == c {
			marker := s[i : i+2]
			if j := strings.Index(s[i+2:], marker); j >= 0 {
				flush()
				segs = append(segs, parseInlineCtx(s[i+2:i+2+j], true, italic)...)
				i += 2 + j + 2
				continue
			}
			// Unclosed bold marker: keep it as literal text.
			buf.WriteString(marker)
			i += 2
			continue
		}
		if c == '*' || c == '_' {
			if j := strings.IndexByte(s[i+1:], c); j >= 0 {
				flush()
				segs = append(segs, parseInlineCtx(s[i+1:i+1+j], bold, true)...)
				i += 1 + j + 1
				continue
			}
			// Unclosed italic marker: keep it as literal text.
			buf.WriteByte(c)
			i++
			continue
		}
		buf.WriteByte(c)
		i++
	}
	flush()
	return segs
}

// matchLinkAt reports whether s[i] (which must be '[') opens a
// `[text](url)` link, returning the link text, its URL, and the index just
// past the closing ')'. Neither text nor url may contain a newline —
// matching linkImageRE's [^\]\n]/[^)\n] restriction from Task 1/2 — so a
// stray unmatched '[' followed eventually by "](" and ")" much later in a
// multi-line paragraph can never be mistaken for a link spanning line
// breaks that were only ever soft-wrapped prose.
func matchLinkAt(s string, i int) (text, url string, end int, ok bool) {
	j := strings.IndexByte(s[i+1:], ']')
	if j < 0 {
		return "", "", 0, false
	}
	j += i + 1
	if j+1 >= len(s) || s[j+1] != '(' {
		return "", "", 0, false
	}
	k := strings.IndexByte(s[j+2:], ')')
	if k < 0 {
		return "", "", 0, false
	}
	k += j + 2
	text = s[i+1 : j]
	url = s[j+2 : k]
	if strings.Contains(text, "\n") || strings.Contains(url, "\n") {
		return "", "", 0, false
	}
	return text, url, k + 1, true
}

// ---------------------------------------------------------------------------
// XML rendering
// ---------------------------------------------------------------------------

// renderCtx accumulates state shared across the whole document render pass
// that a single paraBlock cannot decide on its own: the hyperlink
// relationships a link segment needs. It is created once per WriteDocx call
// and threaded through every renderParagraph/renderTable call so a link
// inside a list item, a table cell, or an ordinary paragraph all draw from
// the same counter — see addLink.
type renderCtx struct {
	rels []hyperlinkRel
	// nextRelID starts at 3: rId1 and rId2 are permanently reserved for
	// styles.xml and numbering.xml (see docRelsXML's Task 1/2 history), so
	// the first link-relationship id this document ever allocates must not
	// collide with either.
	nextRelID int
}

// hyperlinkRel is one <Relationship> this document needs beyond the two
// fixed ones (styles, numbering): id is the "rIdN" string referenced from
// the body's <w:hyperlink r:id="...">, url is its external target.
type hyperlinkRel struct {
	id  string
	url string
}

func newRenderCtx() *renderCtx {
	return &renderCtx{nextRelID: 3}
}

// addLink allocates a fresh, always-unique relationship id for one link
// occurrence and records it for buildDocRelsXML. It is called once per
// group of consecutive same-URL segments (see renderRuns), not once per
// link occurrence in the source text that happens to share a URL with an
// earlier one: two separate "[docs](url)" links elsewhere in the same
// document each get their OWN relationship and id, even though Word would
// tolerate reusing one — always allocating fresh keeps the id space simple
// to reason about (never anything besides "the next integer") and
// trivially collision-free no matter how many links the document has.
func (c *renderCtx) addLink(url string) string {
	id := fmt.Sprintf("rId%d", c.nextRelID)
	c.nextRelID++
	c.rels = append(c.rels, hyperlinkRel{id: id, url: url})
	return id
}

// renderParagraph renders one paraBlock as a <w:p> element. Its <w:pPr>
// (when non-empty) carries, in CT_PPr's fixed schema order, the heading's
// <w:pStyle>, a list item's <w:numPr>, a block-quote or horizontal-rule's
// <w:pBdr>, a code paragraph's <w:shd>, and finally a table cell's <w:jc>.
// This package never combines more than one or two of these on a single
// paragraph in practice, but emitting them in schema order keeps the
// output valid even if it did.
//
// forceBold ORs bold onto every inline segment (used for a table header
// row) regardless of the markdown markers parseInline already resolved.
// isCode paragraphs skip parseInline entirely — the whole line becomes one
// literal, monospace segment — which is how Item 1's "markdown is not
// interpreted inside a fenced code block" is enforced structurally rather
// than by convention. isHR paragraphs carry no runs at all: text is never
// even looked at when isHR is set, since a horizontal rule is exactly one
// empty paragraph with a bottom border.
func renderParagraph(b paraBlock, ctx *renderCtx) (string, error) {
	var segs []segment
	switch {
	case b.isHR:
		// No runs: a horizontal rule is an empty paragraph carrying only
		// the bottom-border <w:pBdr> assembled below.
	case b.isCode:
		segs = []segment{{text: b.text, code: true}}
	default:
		segs = parseInline(b.text)
		if b.forceBold {
			for i := range segs {
				segs[i].bold = true
			}
		}
	}

	runsXML, err := renderRuns(segs, ctx)
	if err != nil {
		return "", err
	}

	var pPr strings.Builder
	if b.heading > 0 {
		fmt.Fprintf(&pPr, `<w:pStyle w:val="Heading%d"/>`, b.heading)
	}
	if b.isList {
		numID := bulletNumID
		if b.listOrdered {
			numID = orderedNumID
		}
		fmt.Fprintf(&pPr, `<w:numPr><w:ilvl w:val="%d"/><w:numId w:val="%d"/></w:numPr>`, b.listLevel, numID)
	}
	switch {
	case b.isQuote:
		pPr.WriteString(blockquoteBorderXML)
	case b.isHR:
		pPr.WriteString(hrBorderXML)
	}
	if b.isCode {
		pPr.WriteString(codeShadingXML)
	}
	if b.isQuote {
		pPr.WriteString(blockquoteIndentXML)
	}
	switch b.jc {
	case "left", "center", "right":
		fmt.Fprintf(&pPr, `<w:jc w:val="%s"/>`, b.jc)
	}

	var out strings.Builder
	out.WriteString("<w:p>")
	if pPr.Len() > 0 {
		out.WriteString("<w:pPr>")
		out.WriteString(pPr.String())
		out.WriteString("</w:pPr>")
	}
	out.WriteString(runsXML)
	out.WriteString("</w:p>")
	return out.String(), nil
}

// renderRuns renders every segment of a paragraph, wrapping any run of
// consecutive segments that share the same non-empty link URL in a single
// <w:hyperlink r:id="..."> — a link's visible text can be split into
// several segments by nested bold/italic/code (e.g. "[**bold**
// plain](url)"), and those must all sit inside ONE hyperlink element, not
// one each. ctx.addLink is called exactly once per such run, which is what
// keeps relationship ids allocated once per link occurrence rather than
// once per segment within it.
func renderRuns(segs []segment, ctx *renderCtx) (string, error) {
	var out strings.Builder
	i := 0
	for i < len(segs) {
		seg := segs[i]
		if seg.link == "" {
			r, err := renderRun(seg)
			if err != nil {
				return "", err
			}
			out.WriteString(r)
			i++
			continue
		}
		url := seg.link
		j := i
		var inner strings.Builder
		for j < len(segs) && segs[j].link == url {
			r, err := renderRun(segs[j])
			if err != nil {
				return "", err
			}
			inner.WriteString(r)
			j++
		}
		id := ctx.addLink(url)
		fmt.Fprintf(&out, `<w:hyperlink r:id="%s">%s</w:hyperlink>`, id, inner.String())
		i = j
	}
	return out.String(), nil
}

// renderRun renders one segment as a <w:r> element. Text is escaped with
// this package's own escapeXMLText (splice.go) — the same function edit.go
// already relies on to keep "&"/"<" in inserted text from producing
// "unreadable content" — and xml:space="preserve" is added whenever the
// segment has leading or trailing whitespace, via the same needsPreserve
// splice.go uses, so Word does not collapse spaces at a run boundary (e.g.
// the space between "plain " and "bold" in "plain **bold**") and, for a
// code paragraph, so indentation at the start of a code line survives.
//
// <w:rPr> children are emitted in CT_RPr's schema order: rStyle (a link's
// Hyperlink character style) before rFonts (a code segment's monospace
// font) before b/i, matching the same "schema order even though this
// package rarely combines them" reasoning as renderParagraph's <w:pPr>.
func renderRun(seg segment) (string, error) {
	if seg.text == "" {
		return "", nil
	}
	escaped, err := escapeXMLText(seg.text)
	if err != nil {
		return "", err
	}

	var rPr strings.Builder
	if seg.link != "" {
		rPr.WriteString(`<w:rStyle w:val="Hyperlink"/>`)
	}
	if seg.code {
		rPr.WriteString(codeFontXML)
	}
	if seg.bold {
		rPr.WriteString("<w:b/>")
	}
	if seg.italic {
		rPr.WriteString("<w:i/>")
	}

	preserve := ""
	if needsPreserve(seg.text) {
		preserve = ` xml:space="preserve"`
	}

	var out strings.Builder
	out.WriteString("<w:r>")
	if rPr.Len() > 0 {
		out.WriteString("<w:rPr>")
		out.WriteString(rPr.String())
		out.WriteString("</w:rPr>")
	}
	out.WriteString("<w:t")
	out.WriteString(preserve)
	out.WriteString(">")
	out.Write(escaped)
	out.WriteString("</w:t></w:r>")
	return out.String(), nil
}

// ---------------------------------------------------------------------------
// The minimal five-part OOXML skeleton, plus numbering.xml for lists
// ---------------------------------------------------------------------------

// documentXMLHeader declares both the "w" namespace (everything this
// package has ever emitted) and, new in this task, "r" — the
// relationships namespace <w:hyperlink r:id="..."> needs to reference a
// relationship declared in word/_rels/document.xml.rels (see
// buildDocRelsXML). Without this declaration every hyperlink's r:id
// attribute would be an undeclared-prefix parse error, not merely a
// cosmetic omission.
const documentXMLHeader = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" ` +
	`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
	`<w:body>`

const documentXMLFooter = `<w:sectPr>` +
	`<w:pgSz w:w="12240" w:h="15840"/>` +
	`<w:pgMar w:top="1440" w:right="1440" w:bottom="1440" w:left="1440" w:header="720" w:footer="720" w:gutter="0"/>` +
	`</w:sectPr>` +
	`</w:body></w:document>`

const contentTypesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
	`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
	`<Default Extension="xml" ContentType="application/xml"/>` +
	`<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>` +
	`<Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/>` +
	`<Override PartName="/word/numbering.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.numbering+xml"/>` +
	`</Types>`

const rootRelsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
	`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>` +
	`</Relationships>`

// buildDocRelsXML builds word/_rels/document.xml.rels: the two
// permanently-fixed relationships from word/document.xml (styles.xml at
// rId1 from Task 1, numbering.xml at rId2 from Task 2 — missing either
// registration makes Word declare the whole file corrupt, not just fail to
// render lists; see TestWrite_NumberingXMLIsDeclaredInContentTypes), plus
// one hyperlink relationship per link the document's render pass
// collected (rId3 upward, see renderCtx.addLink). This can no longer be a
// constant, unlike Task 1/2's docRelsXML: the set of hyperlink
// relationships depends on the document's content. rels is built by
// walking blocks in a fixed, deterministic order (WriteDocx's render
// loop), so the SAME markdown always allocates the SAME ids in the SAME
// order — the determinism guarantee (TestWrite_IsDeterministic) extends to
// this part too, not just to document.xml.
//
// A link's URL is XML-escaped before going into the Target attribute: a
// URL containing "&" (an ordinary query-string separator) would otherwise
// produce the same "unreadable content" failure escapeXMLText already
// guards against for run text.
func buildDocRelsXML(rels []hyperlinkRel) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	b.WriteString(`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`)
	b.WriteString(`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/numbering" Target="numbering.xml"/>`)
	for _, r := range rels {
		escapedURL, _ := escapeXMLText(r.url)
		fmt.Fprintf(&b, `<Relationship Id="%s" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/hyperlink" Target="%s" TargetMode="External"/>`,
			r.id, escapedURL)
	}
	b.WriteString(`</Relationships>`)
	return b.String()
}

// stylesPartXML defines Normal and Heading1..Heading6. This is the part most
// likely to look like it worked when it did not: writing
// <w:pStyle w:val="Heading1"/> onto a paragraph is not enough on its own —
// if "Heading1" is not actually DEFINED here, Word treats it as an unknown
// style and renders the paragraph as ordinary body text. The file still
// opens fine either way, so the only way to notice the bug is to check, as
// TestWrite_HeadingStylesAreDefinedInStylesXML does.
const stylesPartXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	// docDefaults must be <w:styles>'s first child, and rPrDefault must
	// precede pPrDefault within it -- Word treats an out-of-schema-order
	// styles part as corrupt with no diagnostic. Without this chain at all,
	// docx_format has nowhere to land a document-wide body font/size/line
	// spacing/alignment change (see Document.Format's BodyFont/BodySizePt/
	// LineSpacing/Align doc comments): a document this package writes
	// itself must be as formattable as one Word or python-docx produced,
	// both of which always emit this chain. The values mirror
	// testdata/structure.docx's own docDefaults (a real Word-authored
	// file's defaults), not an arbitrary choice.
	`<w:docDefaults><w:rPrDefault><w:rPr>` +
	`<w:rFonts w:asciiTheme="minorHAnsi" w:eastAsiaTheme="minorEastAsia" w:hAnsiTheme="minorHAnsi" w:cstheme="minorBidi"/>` +
	`<w:sz w:val="22"/><w:szCs w:val="22"/>` +
	`<w:lang w:val="en-US" w:eastAsia="en-US" w:bidi="ar-SA"/>` +
	`</w:rPr></w:rPrDefault>` +
	`<w:pPrDefault><w:pPr><w:spacing w:after="200" w:line="276" w:lineRule="auto"/></w:pPr></w:pPrDefault>` +
	`</w:docDefaults>` +
	`<w:style w:type="paragraph" w:default="1" w:styleId="Normal">` +
	`<w:name w:val="Normal"/><w:qFormat/></w:style>` +
	`<w:style w:type="paragraph" w:styleId="Heading1">` +
	`<w:name w:val="heading 1"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/>` +
	`<w:pPr><w:keepNext/><w:spacing w:before="240" w:after="120"/><w:outlineLvl w:val="0"/></w:pPr>` +
	`<w:rPr><w:b/><w:sz w:val="32"/></w:rPr></w:style>` +
	`<w:style w:type="paragraph" w:styleId="Heading2">` +
	`<w:name w:val="heading 2"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/>` +
	`<w:pPr><w:keepNext/><w:spacing w:before="200" w:after="100"/><w:outlineLvl w:val="1"/></w:pPr>` +
	`<w:rPr><w:b/><w:sz w:val="28"/></w:rPr></w:style>` +
	`<w:style w:type="paragraph" w:styleId="Heading3">` +
	`<w:name w:val="heading 3"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/>` +
	`<w:pPr><w:keepNext/><w:spacing w:before="160" w:after="80"/><w:outlineLvl w:val="2"/></w:pPr>` +
	`<w:rPr><w:b/><w:sz w:val="26"/></w:rPr></w:style>` +
	`<w:style w:type="paragraph" w:styleId="Heading4">` +
	`<w:name w:val="heading 4"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/>` +
	`<w:pPr><w:keepNext/><w:spacing w:before="120" w:after="60"/><w:outlineLvl w:val="3"/></w:pPr>` +
	`<w:rPr><w:b/><w:sz w:val="24"/></w:rPr></w:style>` +
	`<w:style w:type="paragraph" w:styleId="Heading5">` +
	`<w:name w:val="heading 5"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/>` +
	`<w:pPr><w:keepNext/><w:spacing w:before="120" w:after="60"/><w:outlineLvl w:val="4"/></w:pPr>` +
	`<w:rPr><w:b/><w:sz w:val="22"/></w:rPr></w:style>` +
	`<w:style w:type="paragraph" w:styleId="Heading6">` +
	`<w:name w:val="heading 6"/><w:basedOn w:val="Normal"/><w:next w:val="Normal"/><w:qFormat/>` +
	`<w:pPr><w:keepNext/><w:spacing w:before="120" w:after="60"/><w:outlineLvl w:val="5"/></w:pPr>` +
	`<w:rPr><w:b/><w:sz w:val="20"/></w:rPr></w:style>` +
	// Hyperlink is a CHARACTER style (w:type="character"), not a paragraph
	// style like Normal/HeadingN above — a run picks it up via
	// <w:rStyle w:val="Hyperlink"/> inside its own <w:rPr> (see renderRun),
	// not via <w:pStyle> on the paragraph. Item 2 requires this style
	// actually be DEFINED here, exactly the same "looks like it worked but
	// didn't" trap the package doc already calls out for Heading1..6: an
	// <w:rStyle> referencing an undefined style renders as ordinary text,
	// so a link would be structurally a hyperlink (clickable, right r:id)
	// but visually indistinguishable from plain text.
	`<w:style w:type="character" w:styleId="Hyperlink">` +
	`<w:name w:val="Hyperlink"/>` +
	`<w:rPr><w:color w:val="0563C1"/><w:u w:val="single"/></w:rPr></w:style>` +
	`</w:styles>`

// codeFontXML is the <w:rFonts> every code segment's <w:rPr> carries —
// inline code (Item 1) and every line of a fenced code block alike — so
// both render in the same monospace typeface. Consolas is Word's own
// default for code-styled text; declaring all three of ascii/hAnsi/cs
// keeps non-Latin and complex-script runs from silently falling back to
// the surrounding paragraph's proportional font.
const codeFontXML = `<w:rFonts w:ascii="Consolas" w:hAnsi="Consolas" w:cs="Consolas"/>`

// codeShadingXML is the light background every fenced-code-block paragraph
// carries, per Item 1's brief ("light shading <w:shd w:fill="F5F5F5"/>").
// w:val="clear" (no pattern, just the fill color) is what a real Word
// paragraph shading def normally carries alongside w:fill; omitting it
// still renders correctly in this package's own reader, but including it
// matches what Word itself writes and avoids relying on an implicit
// default for a value the schema does not treat as truly optional.
const codeShadingXML = `<w:shd w:val="clear" w:color="auto" w:fill="F5F5F5"/>`

// blockquoteBorderXML/blockquoteIndentXML are Item 3's block-quote
// treatment: a left border (the same "quoted text" visual convention
// nearly every reader recognizes) plus a matching left indent so the
// border doesn't sit flush against the quoted text with no breathing room.
// Both are separate <w:pPr> children (<w:pBdr> and <w:ind>) rather than
// one combined element — CT_PPr has no such combined element — and
// renderParagraph emits them in that relative order, matching CT_PPr's own
// schema sequence (pBdr precedes ind).
const blockquoteBorderXML = `<w:pBdr><w:left w:val="single" w:sz="12" w:space="4" w:color="auto"/></w:pBdr>`
const blockquoteIndentXML = `<w:ind w:left="360"/>`

// hrBorderXML is Item 3's horizontal rule: a bottom border on an otherwise
// completely empty paragraph (renderParagraph never generates any runs for
// an isHR paraBlock). This is the same "border as a drawn line" idiom
// Word's own Home ribbon "Horizontal Line" command produces, just without
// that command's extra shading trick — a single visible rule is enough to
// render as a divider, and needs no additional part or relationship the
// way an actual embedded-picture horizontal line would.
const hrBorderXML = `<w:pBdr><w:bottom w:val="single" w:sz="6" w:space="1" w:color="auto"/></w:pBdr>`

// maxListLevel is the deepest <w:ilvl> this package defines in
// numbering.xml (0-based, so 9 levels total: 0..8) — the same maximum
// Word's own UI offers for a multilevel list. A markdown list indented
// deeper than this is clamped to the last defined level rather than
// referencing an ilvl with no corresponding <w:lvl>, which Word would
// treat as invalid.
const maxListLevel = 8

// bulletNumID and orderedNumID are the fixed, document-wide <w:numId>
// values every unordered/ordered list paragraph's <w:numPr> refers to.
// They point at different abstractNums (0 and 1 respectively, see
// buildNumberingXML) — sharing one numId, or pointing both at the same
// abstractNum, is exactly what makes an ordered list render as bullets
// (the brief).
//
// Because both are single, fixed IDs used for every list of their kind
// anywhere in the document, two separate ordered lists — e.g. separated by
// an intervening ordinary paragraph — share the same numId and so CONTINUE
// the same numbering sequence rather than each restarting at 1. That is a
// deliberate simplification, not an oversight: the alternative (each
// contiguous list run restarting) needs a fresh <w:num> with its own
// <w:lvlOverride>/<w:startOverride> per run, built dynamically per
// document rather than as this fixed, statically-defined numbering.xml.
// This package chooses the static, always-deterministic version and
// accepts the "continues" behavior as its consequence — see
// TestWrite_ListInterruptedThenResumedSharesNumId, which pins exactly
// this.
const (
	bulletNumID  = 1
	orderedNumID = 2
)

// bulletChars cycles every 3 levels — the same filled/hollow/square
// progression Word's own default bullet-list template uses — so a deeply
// nested list stays visually distinguishable from its grandparent rather
// than repeating one glyph at every level.
var bulletChars = []string{"•", "◦", "▪"}

// orderedFormats cycles decimal/lowerLetter/lowerRoman — the same
// arabic-alpha-roman progression Word's own default numbered-list template
// uses for nested levels.
var orderedFormats = []string{"decimal", "lowerLetter", "lowerRoman"}

// numberingXML is word/numbering.xml: one abstractNum for bullets
// (abstractNumId 0) and one for ordered decimal/letter/roman levels
// (abstractNumId 1), each defining levels 0..maxListLevel, plus the two
// <w:num> entries bulletNumID/orderedNumID reference. Built once, here, by
// a loop rather than written out by hand: 2 x (maxListLevel+1) nearly
// identical <w:lvl> blocks would otherwise be a lot of easy-to-typo,
// hard-to-review literal XML.
var numberingXML = buildNumberingXML()

func buildNumberingXML() string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<w:numbering xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">`)
	b.WriteString(bulletAbstractNumXML())
	b.WriteString(orderedAbstractNumXML())
	fmt.Fprintf(&b, `<w:num w:numId="%d"><w:abstractNumId w:val="0"/></w:num>`, bulletNumID)
	fmt.Fprintf(&b, `<w:num w:numId="%d"><w:abstractNumId w:val="1"/></w:num>`, orderedNumID)
	b.WriteString(`</w:numbering>`)
	return b.String()
}

// bulletAbstractNumXML defines abstractNumId 0: a plain-Unicode bullet
// (no Symbol/Wingdings font mapping needed, unlike many hand-authored
// OOXML templates) at each of levels 0..maxListLevel, each indented
// further than its parent.
func bulletAbstractNumXML() string {
	var b strings.Builder
	b.WriteString(`<w:abstractNum w:abstractNumId="0">`)
	b.WriteString(`<w:multiLevelType w:val="hybridMultilevel"/>`)
	for lvl := 0; lvl <= maxListLevel; lvl++ {
		indent := 720 * (lvl + 1)
		fmt.Fprintf(&b,
			`<w:lvl w:ilvl="%d"><w:start w:val="1"/><w:numFmt w:val="bullet"/>`+
				`<w:lvlText w:val="%s"/><w:lvlJc w:val="left"/>`+
				`<w:pPr><w:ind w:left="%d" w:hanging="360"/></w:pPr></w:lvl>`,
			lvl, bulletChars[lvl%len(bulletChars)], indent)
	}
	b.WriteString(`</w:abstractNum>`)
	return b.String()
}

// orderedAbstractNumXML defines abstractNumId 1: each level's own
// independent counter (lvlText "%N." references only its own level, never
// an ancestor's, so nested numbers read "1.", "a.", "i." rather than
// cumulative "1.1.1"), formatted decimal/lowerLetter/lowerRoman in a
// 3-level cycle, each indented further than its parent.
func orderedAbstractNumXML() string {
	var b strings.Builder
	b.WriteString(`<w:abstractNum w:abstractNumId="1">`)
	b.WriteString(`<w:multiLevelType w:val="hybridMultilevel"/>`)
	for lvl := 0; lvl <= maxListLevel; lvl++ {
		indent := 720 * (lvl + 1)
		fmt.Fprintf(&b,
			`<w:lvl w:ilvl="%d"><w:start w:val="1"/><w:numFmt w:val="%s"/>`+
				`<w:lvlText w:val="%%%d."/><w:lvlJc w:val="left"/>`+
				`<w:pPr><w:ind w:left="%d" w:hanging="360"/></w:pPr></w:lvl>`,
			lvl, orderedFormats[lvl%len(orderedFormats)], lvl+1, indent)
	}
	b.WriteString(`</w:abstractNum>`)
	return b.String()
}
