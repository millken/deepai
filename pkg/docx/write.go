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
	"strconv"
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
	// Title, when non-empty, becomes the document's first paragraph, styled
	// as Heading1 ahead of anything parsed from Markdown -- UNLESS Markdown
	// already opens with its own level-1 ATX heading (see
	// markdownStartsWithH1), in which case Title is not prepended a second
	// time. A model asked to "give this a title" and shown the Markdown
	// syntax brief will often do both -- pass Title AND write "# Title" as
	// the document's own first line -- and prepending unconditionally used
	// to duplicate that heading (P1a's Defect 4: the same title rendered
	// three times over in one real generated document). Title is also
	// written into docProps/core.xml's <dc:title>, the OPC-standard
	// location Word's File > Info panel reads from independently of
	// anything in the document body; that part (plus its Content_Types and
	// root .rels registrations) is added only when Title is non-empty, so a
	// title-less document's package shape is unchanged.
	Title string

	// BodyLatinFont/BodyEastAsiaFont replace styles.xml's docDefaults font
	// pair (which every style inherits unless it declares its own
	// <w:rFonts> — today only SourceCode/VerbatimChar do), i.e. the
	// document's ordinary body and heading font. Each falls back to this
	// package's own default (Calibri / 微软雅黑, copied from the
	// docx-chinese-typography plan's reference document) when left empty —
	// see resolveFontOptions.
	BodyLatinFont    string
	BodyEastAsiaFont string

	// CodeLatinFont/CodeEastAsiaFont replace the font pair a fenced code
	// block and an inline `code` span render in (SourceCode/VerbatimChar in
	// styles.xml, plus write.go's own codeFontXML fallback for the one case
	// -- inline code that is also a link's text -- that cannot reference
	// either style). CodeLatinFont falls back to Consolas when empty.
	//
	// CodeEastAsiaFont falls back to 微软雅黑 (Microsoft YaHei) when empty.
	// Read that default's own doc comment (defaultCodeEastAsiaFont in
	// styles.go) before assuming it aligns ASCII box-drawing characters
	// against Chinese text — it does NOT, by itself: exact alignment needs
	// a font where a Latin glyph's advance is exactly half a CJK glyph's
	// (NSimSun, MS Gothic, Sarasa Gothic, Noto Sans Mono CJK), none of
	// which ship on macOS. This field exists so a caller targeting Windows
	// readers, or with one of those fonts installed, can ask for one of
	// them explicitly and get exact alignment; the default cannot provide
	// that on its own.
	CodeLatinFont    string
	CodeEastAsiaFont string
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
	fonts := resolveFontOptions(opts)

	ctx := newRenderCtx(fonts)
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
	// footerRelID is allocated from the SAME counter addLink draws from
	// (ctx.nextRelID), after every hyperlink in the body has already
	// claimed its own id: the two id spaces are one space, not two, so
	// there is no way for this to collide with a hyperlink's id no matter
	// how many links the document has -- see addFooterRelID's doc comment
	// and TestType_HyperlinkAndFooterRelIDsDoNotCollide.
	footerRelID := ctx.addFooterRelID()
	body.WriteString(documentXMLFooterXML(footerRelID))

	hasTitle := opts.Title != ""
	entries := []zipEntry{
		{name: contentTypesPart, data: []byte(buildContentTypesXML(hasTitle))},
		{name: "_rels/.rels", data: []byte(buildRootRelsXML(hasTitle))},
		{name: DocumentPart, data: []byte(body.String())},
		{name: "word/_rels/document.xml.rels", data: []byte(buildDocRelsXML(ctx.rels, footerRelID))},
		{name: "word/styles.xml", data: buildStylesXMLWithFonts(fonts)},
		{name: "word/numbering.xml", data: []byte(numberingXML)},
		{name: footer1Part, data: []byte(footer1XML)},
	}
	if hasTitle {
		coreXML, err := docPropsCoreXML(opts.Title)
		if err != nil {
			return WriteResult{}, fmt.Errorf("docx: render docProps/core.xml: %w", err)
		}
		// Appended after the fixed five parts: entry order only has to be
		// deterministic (writeNewDocx replays this exact slice every call),
		// not match any particular position Word itself would choose.
		entries = append(entries, zipEntry{name: docPropsCorePart, data: []byte(coreXML)})
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
//
// isCell is the docx-chinese-typography plan's Task 2 addition: it marks a
// table cell's own paragraph (set only by renderTable), and exists solely
// so renderParagraph can tell that case apart from an ordinary top-level
// paragraph — both otherwise look identical (heading==0, no
// isList/isQuote/isCode/isHR). Without it, an ordinary paragraph's new
// pStyle="BodyText" (the first-line indent) would apply to table cells too,
// which is exactly the leak the plan's Task 2 warns against by name. A cell
// paragraph with isCell set gets NO pStyle at all — it stays on Normal, and
// TableGrid's own <w:pPr> (styles.go) supplies its compact spacing via the
// table-style cascade instead — see tableTblPrXML's doc comment.
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
	isCell      bool
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
// it reports. Title, when set, is prepended as its own Heading1 block ahead
// of anything parsed from Markdown -- UNLESS Markdown already opens with
// its own level-1 heading (markdownStartsWithH1), which would otherwise
// duplicate it: a model asked to title a document commonly does both,
// passing Title AND writing "# Title" as Markdown's own first line, and
// that combination is the normal case this guards, not an edge case (see
// WriteOptions.Title and Defect 4 in the package's write-quality report).
// If nothing produced any block at all (empty or all-blank input, no
// Title), a single empty paragraph block is emitted instead of zero: a
// .docx body conventionally always carries at least one paragraph, and
// this is the only way to guarantee that without also making every
// ordinary blank separator produce spurious empty paragraphs (see
// buildBlocks).
func parseMarkdown(opts WriteOptions) ([]block, []string) {
	unit := inferListIndentUnit(opts.Markdown)
	blocks, tableNotes := buildBlocks(opts.Markdown, unit)
	hasImage := detectImages(opts.Markdown)
	notes := buildNotes(hasImage, tableNotes)

	if opts.Title != "" && !markdownStartsWithH1(opts.Markdown) {
		blocks = append([]block{{para: &paraBlock{heading: 1, text: opts.Title}}}, blocks...)
	}
	if len(blocks) == 0 {
		blocks = []block{{para: &paraBlock{heading: 0, text: ""}}}
	}
	return blocks, notes
}

// markdownStartsWithH1 reports whether markdown's first non-blank line is
// itself a level-1 ATX heading ("# ..."), skipping any number of leading
// blank lines. It deliberately does not account for a document that opens
// directly inside an unclosed fenced code block -- there is no such thing,
// since a fence has to be opened by a "```"/"~~~" line first, and that
// opening line is not itself blank -- so the simple line-by-line scan here
// never needs buildBlocks' inFence bookkeeping.
func markdownStartsWithH1(markdown string) bool {
	markdown = strings.ReplaceAll(markdown, "\r\n", "\n")
	for _, line := range strings.Split(markdown, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		m := headingRE.FindStringSubmatch(trimmed)
		return m != nil && len(m[1]) == 1
	}
	return false
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
	out.WriteString(tableTblPrXML)
	out.WriteString("<w:tblGrid>")
	for _, w := range tableColumnWidthsTwips(tb.cols) {
		fmt.Fprintf(&out, `<w:gridCol w:w="%d"/>`, w)
	}
	out.WriteString("</w:tblGrid>")

	paraCount := 0
	for _, row := range tb.rows {
		out.WriteString("<w:tr>")
		if row.header {
			// <w:trPr><w:tblHeader/></w:trPr> is Part B of the docx-chinese-
			// typography plan: it makes Word repeat this row at the top of
			// every page a long table spans, instead of the header
			// scrolling off after the first page. trPr is CT_Row's first
			// child, so it must come immediately after <w:tr>, before any
			// <w:tc> -- see TestType_HeaderRowRepeatsAcrossPages.
			out.WriteString("<w:trPr><w:tblHeader/></w:trPr>")
		}
		for _, cell := range row.cells {
			p, err := renderParagraph(paraBlock{text: cell.text, jc: cell.align, forceBold: row.header, isCell: true}, ctx)
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

// tableColumnWidthsTwips divides contentWidthTwips evenly across cols
// columns, in twips, for <w:tblGrid>. Integer division alone would either
// lose twips (floor division, e.g. 9360/7 = 1337, and 7*1337 = 9359 — one
// twip short of the page) or need a separate rounding pass to fix that up;
// the classic remainder-distribution technique used here avoids both in
// one step: the first (contentWidthTwips % cols) columns get one extra
// twip apiece, so the returned slice always sums to EXACTLY
// contentWidthTwips, no column differing from another by more than a
// single twip — an amount no reader could ever perceive. See
// TestWrite_TableColumnWidthsSumToContentWidth, which checks this for
// column counts that do and do not divide evenly.
func tableColumnWidthsTwips(cols int) []int {
	if cols <= 0 {
		return nil
	}
	base := contentWidthTwips / cols
	rem := contentWidthTwips % cols
	widths := make([]int, cols)
	for i := range widths {
		widths[i] = base
		if i < rem {
			widths[i]++
		}
	}
	return widths
}

// tableTblPrXML is the <w:tblPr> every generated table shares. Its border
// declaration used to be written here too (a literal <w:tblBorders>
// repeated on every table this package ever generates) until Task 2 of the
// docx-style-architecture plan: TableGrid (styles.go) is now a w:type="table"
// style carrying that exact same tblBorders in its own <w:tblPr>, and
// <w:tblStyle w:val="TableGrid"/> below pulls it in by reference. Writing
// tblBorders again here on top of that would be the identical duplication
// this task exists to remove — one visual property (the border), two
// sources of truth. Referencing TableGrid also pulls in its <w:pPr>
// (spacing after="0"), which zeroes every cell paragraph's spacing and is
// the actual fix for the tall table rows the user saw: a table style's own
// pPr cascades to the paragraphs inside its cells without those paragraphs
// needing any pStyle of their own.
//
// <w:tblW> (full content width, pct 5000 = 100%) and <w:tblLayout
// w:type="fixed"/> (turns off Word's content-based column autofit) stay
// here rather than moving into TableGrid: unlike a border or a spacing
// rule, they are not shared, table-independent visual choices -- <w:tblW>
// is set to a percentage that is the same for every table regardless of
// column count, but page geometry (contentWidthTwips) is still something
// only this per-table render call knows about, same reasoning as
// <w:tblGrid>'s own <w:gridCol> widths below.
//
// <w:tblLook w:firstRow="1" .../> is Part B of the docx-chinese-typography
// plan: it is what actually ACTIVATES TableGrid's <w:tblStylePr
// w:type="firstRow"> conditional formatting (the header shading/bold) for
// THIS table. A table style can carry tblStylePr and still render with no
// shading at all if the table referencing it has no tblLook (or one with
// firstRow="0") — Word treats tblLook as an opt-in per table, not an
// automatic consequence of the style existing. The other five booleans
// (lastRow/firstColumn/lastColumn/noHBand off, noVBand on) match Word's own
// default for a plain "banded first row only" look, since this package
// never generates banded columns or a distinguished last row/column.
const tableTblPrXML = `<w:tblPr><w:tblStyle w:val="` + StyleTableGrid + `"/>` +
	`<w:tblW w:w="5000" w:type="pct"/><w:tblLayout w:type="fixed"/>` +
	`<w:tblLook w:firstRow="1" w:lastRow="0" w:firstColumn="0" w:lastColumn="0" w:noHBand="0" w:noVBand="1"/>` +
	`</w:tblPr>`

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
// relationships a link segment needs, and (as of the docx-chinese-
// typography plan's Part C) the one footer relationship every document
// needs. It is created once per WriteDocx call and threaded through every
// renderParagraph/renderTable/renderRun call so a link inside a list item,
// a table cell, or an ordinary paragraph -- and the footer -- all draw
// their relationship ids from the same counter. See addLink/addFooterRelID.
//
// fonts carries the resolved (never-empty) font choices this WriteDocx call
// is using; renderRun reads it through codeFontXML for the one code+link
// combination that cannot get its font from a shared style (see that
// method's doc comment).
type renderCtx struct {
	rels  []hyperlinkRel
	fonts fontOptions
	// nextRelID starts at 3: rId1 and rId2 are permanently reserved for
	// styles.xml and numbering.xml (see docRelsXML's Task 1/2 history), so
	// the first link-relationship id this document ever allocates must not
	// collide with either. Every relationship this package ever allocates
	// beyond those two fixed ones -- every hyperlink AND the one footer
	// relationship -- draws from this SAME counter, which is what makes
	// them mutually collision-free by construction rather than by
	// coincidence: see addFooterRelID's doc comment.
	nextRelID int
}

// hyperlinkRel is one <Relationship> this document needs beyond the two
// fixed ones (styles, numbering): id is the "rIdN" string referenced from
// the body's <w:hyperlink r:id="...">, url is its external target.
type hyperlinkRel struct {
	id  string
	url string
}

func newRenderCtx(fonts fontOptions) *renderCtx {
	return &renderCtx{nextRelID: 3, fonts: fonts}
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

// addFooterRelID allocates the relationship id for word/footer1.xml,
// drawing from the exact same counter addLink uses. This is the load-
// bearing fix for the hazard the docx-chinese-typography plan calls out by
// name: if the footer's relationship were numbered by a second, independent
// counter, a document with (say) three hyperlinks already using rId3-rId5
// could easily also hand the footer rId3 or rId4 -- Word does not detect
// that as an error at open time, it just resolves the footer's
// <w:footerReference r:id="rId3"> against WHATEVER Target rId3 happens to
// have in word/_rels/document.xml.rels, which after a collision is a
// hyperlink's URL, not footer1.xml. That surfaces to a user as either "Word
// found a problem with this file" (a repair prompt) or a footer that does
// not show a page number at all, with no indication of why. Drawing from
// the one shared counter makes the id space, structurally, never able to
// double-assign a number no matter how many links the document has -- see
// TestType_HyperlinkAndFooterRelIDsDoNotCollide.
func (c *renderCtx) addFooterRelID() string {
	id := fmt.Sprintf("rId%d", c.nextRelID)
	c.nextRelID++
	return id
}

// codeFontXML renders c.fonts' code Latin/East-Asian pair as a direct
// <w:rFonts> — the one narrow edge case (inline code that is ALSO a
// hyperlink's text, "[`code`](url)") that cannot reference either
// SourceCode or VerbatimChar, because a run's <w:rPr> permits at most one
// <w:rStyle> and this combination already needs Hyperlink's — see
// renderRun's call site. Every OTHER code segment gets its monospace font
// from a style instead, so this is the one place in the whole render pass
// where a font choice is written directly into document.xml rather than
// referenced by name; it is run-level FONT formatting, not one of the three
// paragraph-level properties (spacing/ind/shd) the styles-architecture
// invariant bans.
func (c *renderCtx) codeFontXML() string {
	return `<w:rFonts w:ascii="` + c.fonts.codeLatin + `" w:eastAsia="` + c.fonts.codeEastAsia +
		`" w:hAnsi="` + c.fonts.codeLatin + `" w:cs="` + c.fonts.codeLatin + `"/>`
}

// renderParagraph renders one paraBlock as a <w:p> element. Per the
// docx-style-architecture plan's Task 2, its <w:pPr> (when non-empty) no
// longer carries any paragraph-level visual property directly (no
// <w:spacing>, <w:ind>, or <w:shd> — see
// TestWrite_NoInlineVisualPropertiesInDocumentXML): every construct that
// used to write one now references a named style in styles.go instead,
// which carries the same property. In CT_PPr's fixed schema order, that is
// a heading/list/quote/code paragraph's single mutually-exclusive
// <w:pStyle>, a list item's <w:numPr>, a horizontal-rule's <w:pBdr>, and
// finally a table cell's <w:jc>. This package never combines more than one
// or two of these on a single paragraph in practice, but emitting them in
// schema order keeps the output valid even if it did.
//
// <w:pBdr> survives inline only for isHR: a horizontal rule is a one-off
// empty paragraph, never repeated as a shared visual identity the way
// SourceCode/Quote/ListParagraph/TableGrid are, so there is no shared style
// for it to move into, and the plan's invariant does not ban <w:pBdr>
// (only <w:spacing>/<w:ind>/<w:shd>).
//
// forceBold ORs bold onto every inline segment (used for a table header
// row) regardless of the markdown markers parseInline already resolved.
// isCode paragraphs skip parseInline entirely — the whole line becomes one
// literal segment, monospace via pStyle="SourceCode" rather than a direct
// <w:rFonts> on the run — which is how Item 1's "markdown is not
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

	// b.isCode tells renderRuns/renderRun not to also add a run-level
	// <w:rStyle w:val="VerbatimChar"/> to this paragraph's own segment: the
	// monospace font already comes from pStyle="SourceCode" below, cascading
	// from the style's own <w:rPr> to every run in a paragraph of that
	// style. VerbatimChar is reserved for genuine INLINE code -- a `code`
	// span sitting inside an otherwise ordinary paragraph -- per the plan's
	// mapping ("行内代码 → VerbatimChar"); applying it here too would be
	// harmless (same font) but a second, redundant source of the same
	// formatting, which is exactly the duplication this task removes.
	runsXML, err := renderRuns(segs, ctx, b.isCode)
	if err != nil {
		return "", err
	}

	var pPr strings.Builder
	// pStyle is the one paragraph-level style reference every non-plain
	// paragraph carries; buildBlocks never sets more than one of
	// heading/isList/isQuote/isCode on the same paraBlock (see the
	// paraBlock doc comment), so this is a genuine mutual exclusion, not
	// merely an order-of-precedence choice.
	//
	// The default branch (an ordinary paragraph, heading==0 and none of
	// isList/isQuote/isCode/isHR/isCell) is Task 2 of the docx-chinese-
	// typography plan: it references BodyText (styles.go) for the
	// reference document's first-line indent + 1.5x line spacing. isHR and
	// isCell are excluded from that default on purpose, not merely left
	// unhandled: an isHR paragraph carries no text to indent at all, and an
	// isCell (table-cell) paragraph must NOT pick up BodyText's first-line
	// indent — see paraBlock.isCell's doc comment and
	// TestType_FirstLineIndentDoesNotLeakIntoOtherBlocks, which pins this
	// directly rather than trusting the switch's shape by eyeball.
	switch {
	case b.heading > 0:
		fmt.Fprintf(&pPr, `<w:pStyle w:val="Heading%d"/>`, b.heading)
	case b.isList:
		fmt.Fprintf(&pPr, `<w:pStyle w:val="%s"/>`, StyleListParagraph)
	case b.isQuote:
		fmt.Fprintf(&pPr, `<w:pStyle w:val="%s"/>`, StyleQuote)
	case b.isCode:
		fmt.Fprintf(&pPr, `<w:pStyle w:val="%s"/>`, StyleSourceCode)
	case b.isHR, b.isCell:
		// No pStyle: isHR has no text to indent, and isCell must stay on
		// Normal so TableGrid's own pPr cascade (styles.go) governs its
		// spacing instead of BodyText's.
	default:
		fmt.Fprintf(&pPr, `<w:pStyle w:val="%s"/>`, StyleBodyText)
	}
	if b.isList {
		numID := bulletNumID
		if b.listOrdered {
			numID = orderedNumID
		}
		fmt.Fprintf(&pPr, `<w:numPr><w:ilvl w:val="%d"/><w:numId w:val="%d"/></w:numPr>`, b.listLevel, numID)
	}
	if b.isHR {
		pPr.WriteString(hrBorderXML)
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
//
// codeBlockLine is threaded straight through to renderRun for every
// segment: see renderParagraph's call site for why a fenced-code-block
// paragraph's own segment must NOT also pick up VerbatimChar.
func renderRuns(segs []segment, ctx *renderCtx, codeBlockLine bool) (string, error) {
	var out strings.Builder
	i := 0
	for i < len(segs) {
		seg := segs[i]
		if seg.link == "" {
			r, err := renderRun(seg, ctx, codeBlockLine)
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
			r, err := renderRun(segs[j], ctx, codeBlockLine)
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

// namedHTMLEntities maps the entity names that actually appear in prose to
// their decoded characters. This is not the full HTML5 entity table (there
// are thousands, most obscure) -- it is the small set a markdown author
// realistically types by hand or a renderer commonly emits: &nbsp; (a
// non-breaking space, U+00A0 -- the specific complaint that motivated this,
// since without decoding it Word prints the seven characters "&nbsp;"
// verbatim), the five XML/HTML predefined entities, and three punctuation
// entities (ellipsis, em dash, en dash) common in prose. An entity not in
// this map is left untouched by decodeHTMLEntities rather than dropped or
// guessed at.
var namedHTMLEntities = map[string]string{
	"nbsp":   " ",
	"amp":    "&",
	"lt":     "<",
	"gt":     ">",
	"quot":   "\"",
	"apos":   "'",
	"hellip": "…",
	"mdash":  "—",
	"ndash":  "–",
}

// htmlEntityRE matches one HTML/XML character or numeric entity reference:
// a named form ("&amp;"), a decimal numeric form ("&#160;"), or a
// hexadecimal numeric form ("&#x2014;" or "&#X2014;"). The '&' at the start
// and ';' at the end are both captured (not just the body) so
// decodeHTMLEntities can hand the whole matched text back unchanged when
// the entity turns out to be unrecognized.
var htmlEntityRE = regexp.MustCompile(`&(#[xX][0-9A-Fa-f]+|#[0-9]+|[A-Za-z]+);`)

// decodeHTMLEntities decodes the named and numeric entities markdown prose
// actually carries (see namedHTMLEntities) before the text is XML-escaped
// for <w:t> -- see renderRun, which calls this on every non-code segment.
// An entity this function does not recognize is left exactly as written,
// rather than dropped, since a silently vanished "&foo;" would be a worse
// surprise than a literal one a reader can at least see and search for.
//
// Order matters: this MUST run before escapeXMLText, never after. Consider
// a literal "&amp;lt;" already present in the source (someone's own
// escaped "&lt;", not markup this package produced). regexp's
// ReplaceAllStringFunc scans left to right and never rescans a
// replacement, so it matches only the leftmost, innermost entity: "&amp;"
// at position 0, decoded to "&". The remaining "lt;" has no leading '&' of
// its own, so it is copied through untouched. The result is the four
// literal characters "&lt;" -- not a second decode pass that would turn it
// into a real "<". escapeXMLText then re-escapes that lone "&" back to
// "&amp;" for the XML, so the document's <w:t> ends up containing
// "&amp;lt;", which every XML parser (Word included) resolves back to the
// literal text "&lt;" a reader sees -- exactly the four characters the
// source asked for, never a structural "<". See
// TestWrite_DoubleEscapedEntityStaysLiteral.
func decodeHTMLEntities(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	return htmlEntityRE.ReplaceAllStringFunc(s, func(m string) string {
		body := m[1 : len(m)-1] // strip leading '&' and trailing ';'
		switch {
		case strings.HasPrefix(body, "#x") || strings.HasPrefix(body, "#X"):
			if n, err := strconv.ParseInt(body[2:], 16, 32); err == nil && n > 0 {
				return string(rune(n))
			}
		case strings.HasPrefix(body, "#"):
			if n, err := strconv.ParseInt(body[1:], 10, 32); err == nil && n > 0 {
				return string(rune(n))
			}
		default:
			if r, ok := namedHTMLEntities[body]; ok {
				return r
			}
		}
		return m // unrecognized: stays literal
	})
}

// renderRun renders one segment as a <w:r> element. Text is decoded for HTML
// entities (decodeHTMLEntities, above) -- unless the segment is code, which
// Item 1 requires survive verbatim, entities included -- then escaped with
// this package's own escapeXMLText (splice.go) — the same function edit.go
// already relies on to keep "&"/"<" in inserted text from producing
// "unreadable content" — and xml:space="preserve" is added whenever the
// (decoded) segment has leading or trailing whitespace, via the same
// needsPreserve splice.go uses, so Word does not collapse spaces at a run
// boundary (e.g. the space between "plain " and "bold" in "plain **bold**")
// and, for a code paragraph, so indentation at the start of a code line
// survives.
//
// <w:rPr> children are emitted in CT_RPr's schema order: rStyle (a link's
// Hyperlink character style, or an inline code span's VerbatimChar
// character style) before rFonts (the one narrow case that still needs a
// direct font — see below) before b/i, matching the same "schema order
// even though this package rarely combines them" reasoning as
// renderParagraph's <w:pPr>.
//
// codeBlockLine is true only when this run is one line of a fenced code
// block (renderParagraph passes b.isCode straight through) — never for a
// `code` span inline inside an ordinary paragraph. It suppresses the
// VerbatimChar rStyle below, because a code-block paragraph already gets
// its monospace font from pStyle="SourceCode" (styles.go's rPr cascades to
// every run in a paragraph of that style); adding VerbatimChar there too
// would be a second, redundant source of the identical formatting.
func renderRun(seg segment, ctx *renderCtx, codeBlockLine bool) (string, error) {
	if seg.text == "" {
		return "", nil
	}
	text := seg.text
	if !seg.code {
		text = decodeHTMLEntities(text)
	}
	escaped, err := escapeXMLText(text)
	if err != nil {
		return "", err
	}

	var rPr strings.Builder
	switch {
	case seg.link != "":
		rPr.WriteString(`<w:rStyle w:val="Hyperlink"/>`)
		if seg.code {
			// A run's <w:rPr> may carry at most one <w:rStyle> (CT_RPr
			// permits 0..1), so a segment that is BOTH a link and inline
			// code -- the unusual "[`code`](url)" -- cannot reference both
			// Hyperlink and VerbatimChar at once. Word draws the Hyperlink
			// color/underline either way; ctx.codeFontXML falls back to a
			// direct <w:rFonts> here purely to keep the monospace look for
			// this one combination, matching this package's pre-Task-2
			// behavior for it. This is run-level font formatting, not one
			// of the three paragraph-level properties
			// (spacing/ind/shd) the styles-architecture invariant bans.
			rPr.WriteString(ctx.codeFontXML())
		}
	case seg.code && !codeBlockLine:
		rPr.WriteString(`<w:rStyle w:val="VerbatimChar"/>`)
	}
	if seg.bold {
		rPr.WriteString("<w:b/>")
	}
	if seg.italic {
		rPr.WriteString("<w:i/>")
	}

	preserve := ""
	if needsPreserve(text) {
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

// Page geometry for the generated document's one and only section:
// pgSz.w/h and each pgMar side. These are also the single source of truth
// for contentWidthTwips (below), which every generated table's
// <w:tblGrid> divides -- Defect 2's fix. Before this fix a table hardcoded
// 2000-twip columns with no relationship at all to the page these
// constants describe.
//
// The values themselves are copied verbatim from the docx-chinese-
// typography plan's reference document (a real, professional Chinese
// business document; see .superpowers/sdd/reference-values.md) rather than
// chosen: A4 (11906x16838 twips, portrait) replaces this package's previous
// US Letter (12240x15840) -- a Chinese document on US Letter is simply the
// wrong paper, independent of anything else this task fixes. Side margins
// stay 1440 twips (1 inch, the reference's own value, unchanged from this
// package's prior US Letter default), but header/footer shrink to 708
// twips (the reference's value; this package's prior 720 was never copied
// from anything).
const (
	pageWidthTwips        = 11906
	pageHeightTwips       = 16838
	pageMarginTopTwips    = 1440
	pageMarginRightTwips  = 1440
	pageMarginBottomTwips = 1440
	pageMarginLeftTwips   = 1440
	pageHeaderTwips       = 708
	pageFooterTwips       = 708
	// docGridLinePitchTwips is <w:docGrid>'s w:linePitch: the East Asian
	// line-pitch grid Word lays CJK text onto, present in the reference
	// (.superpowers/sdd/reference-values.md) and absent from this
	// package's output before this task.
	docGridLinePitchTwips = 360
)

// contentWidthTwips is the horizontal space available for body content --
// page width minus the left and right margins -- and is what every
// generated table's columns must sum to exactly (see
// tableColumnWidthsTwips). On the constants above this comes out to
// 11906 - 1440 - 1440 = 9026, but nothing in this package hardcodes that
// number: it is always recomputed from pageWidthTwips/pageMarginLeftTwips/
// pageMarginRightTwips, the same three constants documentXMLFooterXML writes
// into <w:pgSz>/<w:pgMar>, so the two can never drift out of sync with
// each other even if the page geometry above ever changes -- see
// TestType_TableWidthsFollowA4Geometry, which ties a rendered table's
// column widths back to this constant rather than to a literal number.
const contentWidthTwips = pageWidthTwips - pageMarginLeftTwips - pageMarginRightTwips

// documentXMLFooterXML closes the document body: the section's
// <w:footerReference> (Part C of the docx-chinese-typography plan --
// footerRelID is WriteDocx's own footer relationship id, allocated by
// ctx.addFooterRelID from the same counter hyperlinks use), then <w:pgSz>/
// <w:pgMar>/<w:docGrid> exactly as before Part C. footerReference MUST
// precede pgSz: CT_SectPr's schema lists headerReference/footerReference
// ahead of pgSz, and Word treats an out-of-order sectPr as corrupt with no
// diagnostic, the same trap docDefaultsXML's own ordering comment warns
// about.
//
// This was a package-level var (computed once, unconditionally) before
// Part C; it has to be a function now because footerRelID varies with how
// many hyperlinks a given document's body already allocated ids to.
func documentXMLFooterXML(footerRelID string) string {
	return fmt.Sprintf(
		`<w:sectPr><w:footerReference w:type="default" r:id="%s"/>`+
			`<w:pgSz w:w="%d" w:h="%d" w:orient="portrait"/>`+
			`<w:pgMar w:top="%d" w:right="%d" w:bottom="%d" w:left="%d" w:header="%d" w:footer="%d" w:gutter="0"/>`+
			`<w:docGrid w:linePitch="%d"/>`+
			`</w:sectPr></w:body></w:document>`,
		footerRelID,
		pageWidthTwips, pageHeightTwips,
		pageMarginTopTwips, pageMarginRightTwips, pageMarginBottomTwips, pageMarginLeftTwips,
		pageHeaderTwips, pageFooterTwips,
		docGridLinePitchTwips,
	)
}

// docPropsCorePart is the OPC-standard location for core document
// properties (title, author, etc.) -- Word's File > Info panel, and its
// title bar in some views, read <dc:title> from here, independently of
// whatever the document body itself contains. WriteDocx only ever adds
// this entry when Title is non-empty (see docPropsCoreXML/hasTitle in
// WriteDocx); a title-less document's package is exactly the five parts
// it has always had.
const docPropsCorePart = "docProps/core.xml"

// buildContentTypesXML builds [Content_Types].xml. word/footer1.xml's
// Override is unconditional -- Part C of the docx-chinese-typography plan
// adds a footer to every document WriteDocx produces, not an opt-in one, so
// there is no hasTitle-style flag guarding it. The docProps/core.xml
// Override is included only when hasTitle is true, matching WriteDocx only
// adding that entry in the same condition -- an Override for a part that
// does not exist would itself make the package invalid.
func buildContentTypesXML(hasTitle bool) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">`)
	b.WriteString(`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>`)
	b.WriteString(`<Default Extension="xml" ContentType="application/xml"/>`)
	b.WriteString(`<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>`)
	b.WriteString(`<Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/>`)
	b.WriteString(`<Override PartName="/word/numbering.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.numbering+xml"/>`)
	b.WriteString(`<Override PartName="/word/footer1.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.footer+xml"/>`)
	if hasTitle {
		b.WriteString(`<Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>`)
	}
	b.WriteString(`</Types>`)
	return b.String()
}

// buildRootRelsXML builds _rels/.rels, the package's root relationships
// part. The core-properties relationship (rId2, pointing at
// docPropsCorePart) is included only when hasTitle is true, mirroring
// buildContentTypesXML -- a relationship target that does not exist in the
// package is exactly the kind of thing that makes Word declare a file
// corrupt.
func buildRootRelsXML(hasTitle bool) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	b.WriteString(`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>`)
	if hasTitle {
		b.WriteString(`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/>`)
	}
	b.WriteString(`</Relationships>`)
	return b.String()
}

// docPropsCoreXML renders docProps/core.xml, carrying title into
// <dc:title> -- see docPropsCorePart. title is XML-escaped the same way
// any other user-supplied text this package writes is (escapeXMLText),
// since a title containing "&" or "<" would otherwise produce the same
// "unreadable content" failure as an unescaped run.
func docPropsCoreXML(title string) (string, error) {
	escaped, err := escapeXMLText(title)
	if err != nil {
		return "", fmt.Errorf("escape title: %w", err)
	}
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" ` +
		`xmlns:dc="http://purl.org/dc/elements/1.1/">` +
		`<dc:title>` + string(escaped) + `</dc:title>` +
		`</cp:coreProperties>`, nil
}

// buildDocRelsXML builds word/_rels/document.xml.rels: the two
// permanently-fixed relationships from word/document.xml (styles.xml at
// rId1 from Task 1, numbering.xml at rId2 from Task 2 — missing either
// registration makes Word declare the whole file corrupt, not just fail to
// render lists; see TestWrite_NumberingXMLIsDeclaredInContentTypes), the one
// footer relationship every document gets (footerRelID, Part C of the
// docx-chinese-typography plan — see renderCtx.addFooterRelID), plus one
// hyperlink relationship per link the document's render pass collected
// (rId3 upward, see renderCtx.addLink). This can no longer be a constant,
// unlike Task 1/2's docRelsXML: the set of hyperlink relationships depends
// on the document's content, and footerRelID varies with how many of those
// there are. rels is built by walking blocks in a fixed, deterministic
// order (WriteDocx's render loop), and footerRelID is allocated exactly
// once right after that loop finishes, so the SAME markdown always
// allocates the SAME ids in the SAME order — the determinism guarantee
// (TestWrite_IsDeterministic) extends to this part too, not just to
// document.xml.
//
// A link's URL is XML-escaped before going into the Target attribute: a
// URL containing "&" (an ordinary query-string separator) would otherwise
// produce the same "unreadable content" failure escapeXMLText already
// guards against for run text.
func buildDocRelsXML(rels []hyperlinkRel, footerRelID string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	b.WriteString(`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`)
	b.WriteString(`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/numbering" Target="numbering.xml"/>`)
	fmt.Fprintf(&b, `<Relationship Id="%s" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/footer" Target="footer1.xml"/>`, footerRelID)
	for _, r := range rels {
		escapedURL, _ := escapeXMLText(r.url)
		fmt.Fprintf(&b, `<Relationship Id="%s" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/hyperlink" Target="%s" TargetMode="External"/>`,
			r.id, escapedURL)
	}
	b.WriteString(`</Relationships>`)
	return b.String()
}

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
