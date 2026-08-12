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
	"unicode"
	"unicode/utf8"
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
	// real hyperlinks -- see renderCtx), "> " block quotes, ATX-style setext
	// headings (a text line immediately followed by a full "="/"-{2,}"
	// underline becomes Heading1/Heading2 -- see buildBlocks' setextH1RE/
	// setextH2RE branch), a "---"/"***"/"___" horizontal rule (disambiguated
	// from a table separator row and a setext heading underline by
	// precedence -- table, then setext, then hr -- see buildBlocks' hrRE
	// branch), ~~strikethrough~~ (a run-level <w:strike/>, nestable inside
	// bold/italic like any other marker -- see parseInlineCtx's "~~"
	// branch), and a hard line break: a line ending in two-or-more trailing
	// spaces or a single trailing backslash becomes a <w:br/> instead of
	// being soft-wrapped into the next line with a plain space (see
	// splitTrailingHardBreak/expandHardBreaks). The one thing this package
	// never renders is an image (![alt](url)): no part of its OOXML
	// skeleton embeds binary data, so an image is written verbatim as plain
	// text and declared in WriteResult.Notes -- see detectImages/buildNotes.
	// A handful of other constructs are recognized but deliberately not
	// rendered structurally -- inline/block HTML, footnote markers
	// ([^label]), GFM task-list checkboxes ([ ]/[x]), autolinks (<url>) and
	// bare URLs, and unrecognized HTML entities -- each written as literal
	// text and declared once, with an occurrence count, in
	// WriteResult.Notes; see detectStructuralGaps/buildNotes and
	// renderCtx.unknownEntities.
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
	// fontTableRelID draws from the exact same shared counter, right after
	// the footer's -- see addFontTableRelID's doc comment for why a
	// separate counter for this third part would reopen the identical
	// collision hazard the footer's own id already had to avoid.
	fontTableRelID := ctx.addFontTableRelID()

	hasTitle := opts.Title != ""
	// buildDocRelsXML escapes every link's URL and (per its own doc
	// comment) folds any illegal-character count straight into ctx, so it
	// must run before the stripped-count note below is built -- not just
	// before writeNewDocx.
	docRelsXML := buildDocRelsXML(ctx.rels, footerRelID, fontTableRelID, ctx)
	entries := []zipEntry{
		{name: contentTypesPart, data: []byte(buildContentTypesXML(hasTitle))},
		{name: "_rels/.rels", data: []byte(buildRootRelsXML(hasTitle))},
		{name: DocumentPart, data: []byte(body.String())},
		{name: "word/_rels/document.xml.rels", data: []byte(docRelsXML)},
		{name: "word/styles.xml", data: buildStylesXMLWithFonts(fonts)},
		{name: "word/numbering.xml", data: []byte(numberingXML)},
		{name: footer1Part, data: []byte(footer1XML)},
		{name: fontTablePart, data: []byte(fontTableXML(fonts))},
	}
	if hasTitle {
		// docPropsCoreXML likewise escapes Title and folds its count into
		// ctx -- same ordering requirement as buildDocRelsXML above.
		coreXML, err := docPropsCoreXML(opts.Title, ctx)
		if err != nil {
			return WriteResult{}, fmt.Errorf("docx: render docProps/core.xml: %w", err)
		}
		// Appended after the fixed five parts: entry order only has to be
		// deterministic (writeNewDocx replays this exact slice every call),
		// not match any particular position Word itself would choose.
		entries = append(entries, zipEntry{name: docPropsCorePart, data: []byte(coreXML)})
	}

	// Every escapeXMLText call this render could have made -- renderRun
	// (body text, headings, table cells, code-block lines, hyperlink
	// display text), docPropsCoreXML (Title), and buildDocRelsXML (link
	// URLs) -- has now run and folded its count into ctx.strippedXMLChars
	// (see renderCtx's own doc comment). Declaring the total here, once,
	// rather than per-call, is what keeps the note singular and additive
	// instead of one repeated line per occurrence; doing it any earlier
	// (e.g. right after the render loop, before Title/URL escaping ran)
	// would silently drop whatever those two contributed -- exactly the
	// gap TestWrite_IllegalCharInTitleIsCountedInNotes and
	// TestWrite_IllegalCharInLinkURLIsCountedInNotes pin.
	if ctx.strippedXMLChars > 0 {
		notes = append(notes, stripNoteFor(ctx.strippedXMLChars))
	}
	// ctx.unknownEntities is folded in at the same point, for the same
	// reason (renderRun -- the only place that increments it -- has now
	// run for every segment in the document): see renderCtx.unknownEntities'
	// own doc comment.
	if ctx.unknownEntities > 0 {
		notes = append(notes, unknownEntityNoteFor(ctx.unknownEntities))
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
// (buildBlocks never sets more than one). isCode marks a WHOLE fenced or
// indented code block, not one line of it: buildBlocks still builds one
// paraBlock per source line internally, exactly as before, but
// mergeCodeBlocks (its own last step) joins every consecutive run of them
// into a single paraBlock whose text is those lines joined with "\n" --
// see mergeCodeBlocks' and renderCodeBlockRuns' doc comments for why (one
// <w:p>, one border, in every renderer, not just Word). Text is rendered
// completely literally — renderParagraph never runs it through parseInline,
// so "**bold**" inside a code block never becomes a run — in a monospace
// font on a lightly shaded paragraph. isQuote marks a block-quote paragraph
// (left border + indent); isHR marks
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
	// codeBreak is set (by buildBlocks, only on the first isCode paraBlock
	// of a fresh code block) when the source actually had a blank line
	// between this block and whatever isCode block precedes it -- see
	// mergeCodeBlocks' doc comment for why this exists: a blank line
	// between two code blocks produces no block of its own (buildBlocks'
	// general blank-line branch just flushes and continues), so without
	// this marker mergeCodeBlocks cannot tell "two blocks the source
	// genuinely separated" apart from "one block's own internal lines" by
	// adjacency in the blocks slice alone. Meaningless when isCode is
	// false.
	codeBreak bool
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
	// hrRE matches a thematic-break line: three or more hyphens, asterisks,
	// or underscores and nothing else -- CommonMark's three interchangeable
	// thematic-break spellings ("---", "***", "___"). Interior spaces
	// ("* * *") are still not implemented, matching this package's existing
	// simplification for the dash form. Task 6 added the "*"/"_" spellings
	// (write-quality report's C3: these two used to fall through to being
	// printed as literal text, undeclared); the dash form predates that
	// task. See buildBlocks' hrRE branch for how this is disambiguated from
	// a GFM table separator row and a setext heading underline.
	hrRE = regexp.MustCompile(`^(?:-{3,}|\*{3,}|_{3,})$`)
	// setextH1RE/setextH2RE match a setext heading's underline: one or more
	// '=' characters for H1, two or more '-' characters for H2 (the task
	// brief's own threshold -- a single '-' is neither a valid hr nor,
	// here, a valid H2 underline, so it stays ordinary paragraph text, same
	// as before this fix existed). buildBlocks only consults these when a
	// paragraph is already accumulating in accLines -- a setext heading is
	// always "text line(s) immediately followed by an underline line", never
	// the underline appearing on its own -- and checks them BEFORE hrRE, so
	// "---" right after a text line becomes a Heading2, not a swallowed or
	// literal horizontal rule; hrRE only ever matches "---" with no
	// preceding paragraph in progress (accLines empty). setextH2RE's 2-or-
	// more threshold deliberately overlaps hrRE's 3-or-more range (e.g.
	// "---" satisfies both): that overlap is exactly what the accLines-
	// empty/non-empty split disambiguates, not a separate character-count
	// rule.
	setextH1RE = regexp.MustCompile(`^=+$`)
	setextH2RE = regexp.MustCompile(`^-{2,}$`)
	// quoteRE matches one block-quote line, capturing its content after the
	// "> " (or bare ">") marker. The optional single space after '>' mirrors
	// CommonMark, which does not require it.
	quoteRE = regexp.MustCompile(`^>\s?(.*)$`)
	// refDefRE matches a reference-link definition line, e.g.
	// "[1]: https://example.com/docs" or
	// "[label]: https://example.com \"Title\"". buildBlocks uses this to
	// keep such a line from falling through to the ordinary-paragraph
	// case and being printed as literal body text -- Item I3's "definition
	// line prints in the body" defect -- even though resolving the
	// corresponding `[text][label]` USAGE elsewhere in the document into
	// an actual hyperlink is out of scope (see buildBlocks' refDefRE
	// branch and buildNotes' hasRefDef branch). Group 1 is the label (checked
	// separately for a leading '^', which marks a footnote definition, a
	// different and already-unsupported construct this must not also
	// swallow); group 2 is the destination (checked separately by
	// looksLikeLinkDestination -- this regex alone is deliberately loose
	// about group 2's shape, matching any non-whitespace run, since
	// requiring a URL-like pattern here as well would just duplicate that
	// function inline); group 3, if present, is the quoted title.
	refDefRE = regexp.MustCompile(`^\[([^\]]+)\]:\s+(\S+)(?:\s+"([^"]*)")?\s*$`)
)

// looksLikeLinkDestination reports whether dest (refDefRE's group 2) is
// shaped enough like a URL to treat "[label]: dest" as a genuine
// reference-link definition rather than an ordinary line of prose that
// happens to start with "[something]:" -- a code-review finding against
// the first version of this fix: "[Alice]: Hi", "[TODO]: later", and a
// "[label]: word" line sitting in the middle of an otherwise ordinary
// paragraph are all far more common in real documents (dialogue,
// checklists) than an actual reference-link definition, and matching
// refDefRE alone silently deleted every one of them -- worse than the
// original defect, which at least left the text visible.
//
// The bar for "looks like a URL" is deliberately narrow rather than
// "contains a colon" or similar: dest must either contain "://" (an
// absolute URL with any scheme, e.g. "https://example.com"), or start
// with one of a short list of shapes that are unambiguously a link
// target and never the start of an ordinary word -- "/" or "#" (a
// site-relative or same-page fragment reference) or "mailto:". Anything
// else -- "Hi", "later", "fix", a bare word or sentence -- is treated as
// ordinary paragraph text, not a definition to drop.
func looksLikeLinkDestination(dest string) bool {
	if strings.Contains(dest, "://") {
		return true
	}
	for _, prefix := range []string{"/", "#", "mailto:"} {
		if strings.HasPrefix(dest, prefix) {
			return true
		}
	}
	return false
}

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
// buildBlocks). Also strips a leading UTF-8 byte-order mark (U+FEFF),
// present unconditionally regardless of which of docx_write's two mutually
// exclusive input paths (inline markdown or markdown_path) produced opts --
// see WriteOptions.Markdown's doc comment for why: a BOM is an
// editor/Windows-tooling artifact of how the text was SAVED, orthogonal to
// which of the two ways it reached this function, so both must be treated
// identically rather than only stripping it on one path and leaving the
// other to silently misdetect its first line. Left unstripped, "\ufeff#
// Title" fails headingRE (the BOM is not "#") and the whole first line
// falls through to being an ordinary paragraph instead of Heading1 -- I10.
func parseMarkdown(opts WriteOptions) ([]block, []string) {
	markdown := strings.TrimPrefix(opts.Markdown, "\ufeff")
	unit := inferListIndentUnit(markdown)
	blocks, tableNotes, hasRefDef := buildBlocks(markdown, unit)
	hasImage := detectImages(markdown)
	gaps := detectStructuralGaps(markdown)
	notes := buildNotes(hasImage, tableNotes, hasRefDef, gaps)

	if opts.Title != "" && !markdownStartsWithH1(markdown) {
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
// per horizontal rule, and -- internally, one per line -- per line of a
// fenced OR indented code block; its own final step, mergeCodeBlocks,
// then collapses each maximal run of those line-blocks into one block per
// code BLOCK rather than one per line (see its doc comment for why). So a
// caller of buildBlocks never sees a raw per-line code block, only the
// merged result. Ordinary paragraph lines and block-quote lines are each
// merged with a
// single space within their own run (Markdown's soft-line-break model —
// see flush/flushQuote below); everything else is emitted immediately as
// its own block rather than merged with neighbors, since merging e.g. two
// list items into one paragraph would make already-special content
// actively misleading. unit is the number of leading spaces that make one
// list nesting level — see inferListIndentUnit, which computes it once per
// document before this function is called.
//
// Indented code blocks (CommonMark's other code-block form, alongside a
// fence) were not recognized at all before this task: a four-space-indented
// line with no fence fell straight through to the ordinary-paragraph
// fallback at the bottom of this loop, which trimmed its leading whitespace
// away and merged it into a soft-wrapped body paragraph -- the exact defect
// a real generated document was measured against. inListContext,
// inIndentedCode and pendingBlankIndentedLines below implement it: a line
// indented by a tab or >=4 spaces (stripIndentedCodePrefix) opens/continues
// a code block, EXCEPT while inListContext is true, since a list item's own
// indented continuation content looks identical (both are just "an indented
// line") and swallowing it as code would be actively wrong -- see this
// task's report for how each list/indent interaction case was checked.
func buildBlocks(markdown string, unit int) (blocks []block, tableNotes []string, hasRefDef bool) {
	markdown = strings.ReplaceAll(markdown, "\r\n", "\n")
	lines := strings.Split(markdown, "\n")

	var accLines []string
	// accInList records whether ANY line currently buffered in accLines was
	// appended while inListContext was true -- i.e. accLines holds a list
	// item's indented continuation text, not an ordinary top-level
	// paragraph (see the accLines-append fallback below, and this
	// function's own doc comment on why a continuation line falls all the
	// way through to that same buffer an ordinary paragraph uses). This
	// cannot simply be read off inListContext at setext-check time instead:
	// inListContext is reset per-line based on THAT line's own indentation
	// (see the hasLeadingIndent check below), so by the time an unindented
	// setext underline is itself being classified, inListContext has
	// already flipped back to false even though the text it would attach
	// to came from inside a list. See
	// TestWrite_ListContinuationFollowedByDashesIsNotSetext.
	accInList := false
	// accBreaks[i] records whether a hard line break (see
	// splitTrailingHardBreak) sat at the end of accLines[i] in the source --
	// i.e. whether the join between accLines[i] and accLines[i+1] must
	// become hardBreakMarker instead of the ordinary single-space soft-wrap
	// join. It is only ever appended to in lockstep with accLines (one
	// entry per line, trailing entry meaningless since there is no i+1 to
	// join to) and reset alongside it everywhere accLines is reset -- see
	// flush and the setext branches further down, which both discard
	// accLines without going through flush.
	accBreaks := []bool{}
	flush := func() {
		if len(accLines) == 0 {
			return
		}
		text := joinWithHardBreaks(accLines, accBreaks)
		accLines = accLines[:0]
		accBreaks = accBreaks[:0]
		accInList = false
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

	// pendingCodeBreak/appendCode implement the I1 fix: a blank line that
	// genuinely separates two code blocks in the source must stop them from
	// merging into one paraBlock/one box, even though the blank line itself
	// produces no block at all (see the trimmed=="" branch below) and so
	// leaves nothing in the blocks slice for mergeCodeBlocks to see. Every
	// isCode paraBlock this function appends goes through appendCode, which
	// stamps codeBreak=true on exactly the first line of a fresh code block
	// IF a real separating blank line was consumed since the previous code
	// line -- see paraBlock.codeBreak's own doc comment. pendingCodeBreak
	// being left true across intervening non-code blocks (a heading, an
	// ordinary paragraph, ...) is harmless: those blocks already break
	// mergeCodeBlocks' adjacency requirement on their own, so the flag's
	// value on the next isCode block that eventually follows one of them
	// never actually gets consulted for a merge decision against something
	// non-adjacent.
	pendingCodeBreak := false
	appendCode := func(text string) {
		cb := pendingCodeBreak
		pendingCodeBreak = false
		blocks = append(blocks, block{para: &paraBlock{text: text, isCode: true, codeBreak: cb}})
	}

	inFence := false
	// See this function's own doc comment for what these three implement.
	inListContext := false
	inIndentedCode := false
	pendingBlankIndentedLines := 0
	tableIndex := 0
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if fenceRE.MatchString(trimmed) {
			flush()
			flushQuote()
			inFence = !inFence
			// A fence marker unconditionally ends any indented code block
			// in progress, opening or closing: without this, a literal
			// "```"-looking line encountered while accumulating an indented
			// block would leave inIndentedCode dangling true underneath the
			// fenced section, corrupting whatever line follows the fence's
			// close. Any buffered blank lines are dropped with it, same as
			// any other dedent that ends the block -- and, same as that
			// other dedent below, dropping a nonzero buffered count here
			// means a real blank-line separator existed between the
			// indented block that just ended and whatever this fence opens
			// or closes, so pendingCodeBreak is set the same way.
			if pendingBlankIndentedLines > 0 {
				pendingCodeBreak = true
			}
			inIndentedCode = false
			pendingBlankIndentedLines = 0
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
			appendCode(line)
			continue
		}
		if trimmed == "" {
			if inIndentedCode {
				// Buffered, not emitted yet: a blank line is only part of
				// the block if a further indented line follows it (see the
				// "Trailing blank lines are not part of the block" rule
				// below, at the dedent branch).
				pendingBlankIndentedLines++
				continue
			}
			flush()
			flushQuote()
			// A blank line reaching here (i.e. NOT swallowed by an
			// in-progress indented block above) is a real separator in the
			// source: whatever code block preceded it, if any, ends here,
			// and whatever code block follows, if any, must not be merged
			// back into it -- see appendCode's doc comment.
			pendingCodeBreak = true
			continue
		}

		if inIndentedCode {
			if rest, ok := stripIndentedCodePrefix(line); ok {
				for ; pendingBlankIndentedLines > 0; pendingBlankIndentedLines-- {
					appendCode("")
				}
				appendCode(rest)
				continue
			}
			// Dedented: the block ends here. Any buffered blank lines were
			// trailing it, not part of it, so they are simply dropped, and
			// THIS line falls through to be classified fresh below -- it is
			// not itself part of the block that just ended. Those dropped
			// blanks were still a real separator between this block and
			// whatever code block might come next, so pendingCodeBreak is
			// set the same way the ordinary blank-line branch above sets it
			// -- this indented block just buffered them instead of hitting
			// that branch directly.
			if pendingBlankIndentedLines > 0 {
				pendingCodeBreak = true
			}
			inIndentedCode = false
			pendingBlankIndentedLines = 0
		}

		// A line with no leading indentation at all is the unambiguous end
		// of whatever list was open (a genuine list item, or its indented
		// continuation, is always indented by definition) -- clearing this
		// here, before classifying the line, is what lets a LATER indented
		// block elsewhere in the document be recognized as code again
		// after the list has ended. A still-indented line (a nested list
		// item, or its own continuation) leaves this untouched; the list-
		// item branch below sets it back to true on an actual match.
		if !hasLeadingIndent(line) {
			inListContext = false
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
			inListContext = true
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
		if m := refDefRE.FindStringSubmatch(trimmed); m != nil && !strings.HasPrefix(m[1], "^") && looksLikeLinkDestination(m[2]) {
			// A reference-link definition line: dropped rather than
			// printed as a literal paragraph (Item I3) and flagged via
			// hasRefDef so buildNotes can declare it, per the notes
			// contract every other silent structural decision in this
			// function already honors (see e.g. tableNotes above). The
			// leading-'^' exclusion leaves footnote definitions --
			// "[^1]: ..." -- alone: those are a different, separately
			// unsupported construct (see the write-quality report's C3),
			// and this line shape would otherwise misparse a footnote's
			// prose as if it were a URL. The looksLikeLinkDestination
			// check is what keeps this branch from also swallowing
			// ordinary "[Label]: word" lines -- dialogue, TODOs -- that
			// are not link definitions at all; see that function's doc
			// comment for the code-review finding that made this
			// necessary and exactly where the line is drawn.
			flush()
			flushQuote()
			hasRefDef = true
			continue
		}
		// Setext heading (M1): a paragraph already accumulating in accLines
		// (i.e. NOT a blank line, a list item, a table, or nothing at all --
		// none of those populate accLines) immediately followed by an
		// all-'=' or all-'-{2,}' underline becomes a real Heading1/Heading2,
		// consuming the ENTIRE accumulated paragraph as the heading's text
		// (joined with " ", the same soft-line-break rule flush() itself
		// uses) rather than printing the underline as literal body text.
		// accInList must also be false: a list ITEM's own text never
		// reaches accLines (it becomes its own block immediately -- see the
		// list-item branch above), but its indented CONTINUATION lines do
		// fall all the way through to the accLines append at the bottom of
		// this loop while still under an open list -- accInList (not
		// inListContext, which the hasLeadingIndent reset above may have
		// already flipped back to false for THIS unindented underline line
		// by the time we get here) is what remembers that. Without this
		// guard, "- item\n  more\n---\n" would turn the list item's own
		// continuation text into a Heading2 instead of leaving it, and the
		// list, alone -- see accInList's own doc comment and
		// TestWrite_ListContinuationFollowedByDashesIsNotSetext.
		//
		// This must run BEFORE the hrRE check just below, per the task
		// brief's required precedence (table, then setext, then hr): an
		// all-dash underline like "---" satisfies BOTH setextH2RE (>=2
		// dashes) and hrRE (>=3 dashes), and it is accLines being non-empty
		// -- a text paragraph genuinely precedes it -- that must decide it
		// is a setext underline, not a rule, before hrRE ever gets a look.
		// The table branch above already has its own, earlier first refusal
		// (a table's separator row is consumed as part of parseTable and
		// this loop never revisits it), so by the time either check here
		// runs, a genuine table separator has already been ruled out.
		if len(accLines) > 0 && !accInList {
			if setextH1RE.MatchString(trimmed) {
				// A heading's text is always a single line visually -- a
				// hard break recorded in accBreaks here (a setext heading's
				// source text ending a line in "  " or "\") is simply
				// ignored, same as the ordinary strings.Join(accLines, " ")
				// this replaces; heading text has never carried an embedded
				// <w:br/> in this package, and this task's brief does not
				// ask for that to change.
				text := strings.Join(accLines, " ")
				accLines = accLines[:0]
				accBreaks = accBreaks[:0]
				flushQuote()
				blocks = append(blocks, block{para: &paraBlock{heading: 1, text: text}})
				continue
			}
			if setextH2RE.MatchString(trimmed) {
				text := strings.Join(accLines, " ")
				accLines = accLines[:0]
				accBreaks = accBreaks[:0]
				flushQuote()
				blocks = append(blocks, block{para: &paraBlock{heading: 2, text: text}})
				continue
			}
		}
		// hrRE only ever matches here, after the table branch above and the
		// setext branch just above have already had first refusal: a line
		// that legitimately serves as a GFM separator row was already
		// consumed as part of its table (the "i += consumed - 1" above skips
		// past it), so this loop iteration never revisits it, and a "---"
		// immediately continuing an in-progress paragraph is now a setext
		// Heading2, not a rule. What reaches here is exactly a "---" with NO
		// paragraph in progress -- checked via len(accLines) == 0 -- which is
		// unambiguously a horizontal rule: preceded by a blank line, a
		// heading, a list item, or nothing at all.
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
		if !inListContext {
			// Opens a NEW indented code block. Only reached once none of
			// heading/list-item/table/hr/quote matched -- exactly the
			// "ordinary paragraph" cases this line would otherwise fall
			// into below -- and only when no list is currently open, per
			// this function's own doc comment on the list-interaction
			// hazard.
			if rest, ok := stripIndentedCodePrefix(line); ok {
				flush()
				inIndentedCode = true
				pendingBlankIndentedLines = 0
				appendCode(rest)
				continue
			}
		}
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
		text, hardBreak := splitTrailingHardBreak(line)
		accLines = append(accLines, text)
		accBreaks = append(accBreaks, hardBreak)
		accInList = accInList || inListContext
	}
	flush()
	flushQuote()
	return mergeCodeBlocks(blocks), tableNotes, hasRefDef
}

// mergeCodeBlocks collapses every maximal run of consecutive isCode
// paraBlocks the loop above still emits one per source line, exactly as it
// always has -- into a single paraBlock whose text is those lines' text
// joined with "\n". This is the code-block-single-paragraph fix: a code
// BLOCK is one logical unit, and this is the one place that fact is turned
// into one <w:p> instead of N (renderCodeBlockRuns, further down this
// file, is what turns each joined line back into its own run separated by
// a <w:br/>). Doing the merge here, as a pass over the already-built
// blocks slice, rather than threading an accumulator through every one of
// buildBlocks' four isCode-append call sites (fenced, indented-continue,
// indented-open, indented-blank-catchup) keeps this fix a single, easily
// re-checked function instead of a change smeared across the whole state
// machine above.
//
// It is agnostic to WHY a run of isCode blocks is adjacent -- a fenced
// block's lines, an indented block's lines including its buffered blank
// ones, or (the one edge case worth naming) two textually separate fenced
// blocks with nothing at all between them ("```\na\n```\n```\nb\n```\n")
// also merge into one paragraph/one box. That last case is not a
// regression: Word's own border-merging behavior already drew those two
// blocks as one visual box before this task, for the identical reason
// (byte-identical adjacent <w:pBdr>/<w:shd>); this change keeps that
// combined appearance but makes it explicit and universal (every
// renderer, not just Word) instead of relying on the border-merge
// assumption the rest of this task removes everywhere else.
//
// It is NOT agnostic to a genuine blank-line separator, though (the I1 fix,
// task-5-brief.md): b.para.codeBreak, stamped by buildBlocks' appendCode
// helper only on the first line of a fresh code block that a real blank
// line separated from whatever code came before it, stops the merge here
// even though the two blocks are still adjacent in the blocks slice (the
// blank line itself produced no block for this loop to see otherwise). Two
// fenced blocks with a blank line between them are therefore two boxes;
// two fenced blocks with nothing at all between them (codeBreak never set,
// since no blank line was ever consumed) are still the one edge case above.
func mergeCodeBlocks(blocks []block) []block {
	merged := make([]block, 0, len(blocks))
	for _, b := range blocks {
		if b.para != nil && b.para.isCode && !b.para.codeBreak && len(merged) > 0 {
			if prev := merged[len(merged)-1].para; prev != nil && prev.isCode {
				prev.text += "\n" + b.para.text
				continue
			}
		}
		merged = append(merged, b)
	}
	return merged
}

// stripIndentedCodePrefix reports whether line opens or continues a
// CommonMark indented code block: prefixed by exactly one tab, or by at
// least four literal spaces. Exactly one tab, or exactly four spaces, is
// stripped -- the rest of the line, including any further indentation,
// survives verbatim: the six-space "      \"a\": 1" strips to the two-space
// "  \"a\": 1", preserving the source's own relative indentation, per this
// task's own rule ("Strip exactly the four leading spaces from each line
// and keep the rest verbatim").
//
// This does not implement CommonMark's tab-stop expansion (a tab advances
// to the next 4-column stop, so e.g. one space then one tab would also
// qualify under full CommonMark) -- only a bare leading tab or >=4 literal
// spaces are recognized, a deliberate, documented simplification rather
// than an oversight; see this task's report.
func stripIndentedCodePrefix(line string) (rest string, ok bool) {
	if strings.HasPrefix(line, "\t") {
		return line[1:], true
	}
	if strings.HasPrefix(line, "    ") {
		return line[4:], true
	}
	return line, false
}

// hasLeadingIndent reports whether line starts with a space or a tab --
// buildBlocks' signal that a list item, or its own indented continuation,
// might still be open. A line with NO leading indentation at all is the
// unambiguous end of whatever list was open.
func hasLeadingIndent(line string) bool {
	return len(line) > 0 && (line[0] == ' ' || line[0] == '\t')
}

// hardBreakMarker is a private-use-area sentinel character embedded by
// joinWithHardBreaks between two lines that a hard line break separated in
// the source, and later consumed by expandHardBreaks -- never by
// parseInlineCtx, whose default byte-copy case simply copies its 3 UTF-8
// bytes through unexamined (none of them equal an ASCII byte parseInlineCtx
// treats specially: '\\', '`', '[', '*', '_' are all < 0x80, and U+E000
// encodes as the three bytes 0xEE 0x80 0x80) -- and never written to a
// <w:t> itself, since expandHardBreaks always splits it back out into a
// separate <w:br/> run before renderRun ever sees the segment text. Using a
// real (if unlikely-to-occur-in-prose) character rather than, say, a NUL
// byte sidesteps any risk of colliding with escapeXMLText/isLegalXMLChar's
// own control-character handling in the rare case a marker somehow survived
// unexpanded (e.g. inside a code span -- see expandHardBreaks' doc comment).
const hardBreakMarker = ""

// splitTrailingHardBreak reports whether line -- a single raw source line,
// BEFORE the strings.TrimSpace every other classification in buildBlocks
// applies -- ends in one of CommonMark's two hard-line-break markers: two
// or more trailing spaces, or a single trailing backslash (not a doubled
// "\\\\", which is itself an escaped, literal backslash per parseInlineCtx's
// own escape rule, not a break marker -- see Task 4/C4). Either marker, if
// present, is stripped from the returned text along with any whitespace
// around it; text is otherwise identical to strings.TrimSpace(line) (see
// the non-hard-break return below), so callers can use this in place of
// TrimSpace without changing behavior for the common (non-hard-break) case.
//
// This must run on line, never on the already-computed `trimmed` variable
// buildBlocks' loop uses for every other check: strings.TrimSpace(line)
// unconditionally erases exactly the two-or-more-trailing-spaces shape this
// function needs to detect BEFORE it can classify the line, so `trimmed`
// itself can never distinguish a hard-break line from an ordinary one.
//
// Task 6's brief flips this package's own prior behavior for the trailing-
// backslash case: before this task, a line ending "...text\" fell through
// to parseInlineCtx's escape branch, which -- backslash followed by
// whitespace or end-of-string, neither ASCII punctuation -- left the
// backslash as literal text (see TestWrite_BackslashBeforeNonPunctuationStaysLiteral,
// which covers a DIFFERENT shape, backslash-before-a-letter, and is
// unaffected). A trailing backslash caught here is now consumed into a
// <w:br/> instead and never reaches parseInlineCtx at all.
func splitTrailingHardBreak(line string) (text string, hardBreak bool) {
	trimmedRight := strings.TrimRight(line, " ")
	if len(line)-len(trimmedRight) >= 2 {
		return strings.TrimSpace(trimmedRight), true
	}
	trimmedRight = strings.TrimRight(line, " \t")
	if strings.HasSuffix(trimmedRight, `\`) && !strings.HasSuffix(trimmedRight, `\\`) {
		return strings.TrimSpace(trimmedRight[:len(trimmedRight)-1]), true
	}
	return strings.TrimSpace(line), false
}

// joinWithHardBreaks joins lines the same way flush()'s prior plain
// strings.Join(accLines, " ") did, except that the separator after
// lines[i] becomes hardBreakMarker instead of a single space wherever
// breaks[i] is true (breaks[len(lines)-1], if present, is never consulted --
// there is no line after the last one to join to). See hardBreakMarker's
// own doc comment for why this marker, rather than a literal "\n" or
// "<w:br/>" string, is safe to embed in text that is about to be handed to
// parseInline.
func joinWithHardBreaks(lines []string, breaks []bool) string {
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			if breaks[i-1] {
				b.WriteString(hardBreakMarker)
			} else {
				b.WriteByte(' ')
			}
		}
		b.WriteString(line)
	}
	return b.String()
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

// structuralGaps counts, across the whole raw markdown document, four more
// silent-degradation shapes Task 6 (write-quality report's C3) requires
// buildNotes to declare -- none of which is cheaper to actually render
// structurally than to detect and declare (unlike strikethrough/hard line
// breaks, which this task implements outright -- see parseInlineCtx's "~~"
// branch and splitTrailingHardBreak). See detectStructuralGaps for how each
// field is counted, and its own doc comment for why a blanket regex scan
// over the raw markdown -- the same pragmatic approach detectImages and
// hasRefDef already use -- is enough here rather than a construct-aware
// pass tied into buildBlocks' line-by-line state machine.
type structuralGaps struct {
	// htmlTags counts an inline or block-level HTML tag ("<div>", "</div>",
	// "<br/>", "<span class=\"x\">") -- written as literal text, since this
	// package has no HTML parser and no way to translate arbitrary HTML
	// into OOXML.
	htmlTags int
	// footnotes counts a footnote marker, "[^label]" -- both a reference
	// usage and the leading marker of a "[^label]: text" definition line
	// (see refDefRE's own doc comment for why a footnote definition is
	// deliberately excluded from that regex and left to print as an
	// ordinary paragraph instead of being resolved into a real Word
	// footnote).
	footnotes int
	// taskListItems counts a GFM task-list item's checkbox marker at the
	// front of a list item ("- [ ] todo", "* [x] done"): buildBlocks'
	// listItemRE already turns the surrounding line into an ordinary list
	// item (Task 2), but the "[ ]"/"[x]" text itself renders as literal
	// characters, never an interactive checkbox or content control.
	taskListItems int
	// autolinkAndBareURLs counts a CommonMark autolink ("<https://...>",
	// "<user@example.com>") or a bare "http(s)://" URL sitting directly in
	// prose -- neither is resolved into a real hyperlink the way a
	// "[text](url)" link is (see parseInlineCtx's '[' branch): both render
	// as literal text. The two are declared together, in one note, because
	// the brief treats them as one user-facing gap ("a URL you can see but
	// not click"), not two.
	autolinkAndBareURLs int
}

var (
	// htmlTagRE matches an HTML tag: '<', an optional '/', then a name
	// starting with a letter (a valid tag name must), then anything up to
	// the closing '>' that is not itself '<' or a newline (so a tag can
	// never swallow a second, unrelated '<...>' or span multiple lines).
	// This is deliberately broad rather than an exhaustive real-HTML-tag
	// allowlist, matching this package's existing pragmatic-regex style
	// (imageRE, refDefRE) -- detectStructuralGaps runs it only AFTER
	// autolinkRE's matches have been removed from consideration (below),
	// since an autolink's content starts with a letter too ("<https://...>"
	// looks exactly like a tag named "https" would to this regex alone).
	htmlTagRE = regexp.MustCompile(`</?[a-zA-Z][^<>\n]*>`)
	// autolinkRE matches CommonMark's <scheme:...>/<user@host> autolink
	// form: an angle-bracket-enclosed absolute URI or email address with no
	// internal whitespace. See htmlTagRE's doc comment for why this must be
	// matched, and its matches discarded, before htmlTagRE runs.
	autolinkRE = regexp.MustCompile(`<(?:[a-zA-Z][a-zA-Z0-9+.\-]*://[^\s<>]+|[^\s<>@]+@[^\s<>]+\.[^\s<>]+)>`)
	// bareURLRE matches a plain "http://" or "https://" URL with no
	// enclosing markup at all. detectStructuralGaps runs this only after
	// removing every resolved `[text](url)` link span (linkSpanRE) and
	// every autolink (autolinkRE) from consideration, so it only ever
	// counts a URL sitting directly in prose with nothing marking it up as
	// a link at all.
	bareURLRE = regexp.MustCompile(`\bhttps?://[^\s<>()\[\]]+`)
	// linkSpanRE approximates a resolved `[text](url)` link span for the
	// sole purpose of removing it before bareURLRE/htmlTagRE scan the rest
	// of the text in detectStructuralGaps -- otherwise a real hyperlink's
	// own URL would be double-counted as an unsupported "bare URL". It
	// mirrors imageRE's simpler [^\]\n]/[^)\n] character classes, not
	// matchLinkAt's exact balanced-parenthesis handling: a rough
	// approximation is enough for a detection-only pass (worst case, an
	// unusual URL containing its own literal parentheses is overcounted by
	// one -- a wrong count in a note, not a misrendered document).
	linkSpanRE = regexp.MustCompile(`\[[^\]\n]*\]\([^)\n]*\)`)
	// footnoteRE matches a footnote marker/definition, "[^1]"/"[^note]" --
	// see structuralGaps.footnotes' own doc comment.
	footnoteRE = regexp.MustCompile(`\[\^[^\]\n]+\]`)
	// taskListItemRE matches a GFM task-list item's checkbox marker at the
	// start of a list item's own content -- see
	// structuralGaps.taskListItems' own doc comment. (?m) makes '^' match
	// after every "\n", not just at the start of the whole document, since
	// a task-list item can appear anywhere in a multi-line document.
	taskListItemRE = regexp.MustCompile(`(?m)^[ \t]*[-*+][ \t]+\[[ xX]\][ \t]`)
)

// detectStructuralGaps scans markdown once for each of structuralGaps'
// four counted shapes. Like detectImages/hasRefDef before it, this is a
// blanket regex pass over the raw text rather than something threaded
// through buildBlocks' own line-by-line classification: none of these four
// constructs changes how a LINE is classified (an HTML tag, footnote
// marker, task-list checkbox, or bare URL can all sit inside an otherwise
// perfectly ordinary paragraph/list-item/table-cell line), so there is no
// natural hook in buildBlocks' state machine to count them from, and
// re-scanning the whole document once more here is cheap next to actually
// rendering it.
func detectStructuralGaps(markdown string) structuralGaps {
	working := linkSpanRE.ReplaceAllString(markdown, "")
	working = imageRE.ReplaceAllString(working, "")

	autolinks := autolinkRE.FindAllString(working, -1)
	withoutAutolinks := autolinkRE.ReplaceAllString(working, "")
	bareURLs := bareURLRE.FindAllString(withoutAutolinks, -1)
	tags := htmlTagRE.FindAllString(withoutAutolinks, -1)

	return structuralGaps{
		htmlTags:            len(tags),
		footnotes:           len(footnoteRE.FindAllString(markdown, -1)),
		taskListItems:       len(taskListItemRE.FindAllString(markdown, -1)),
		autolinkAndBareURLs: len(autolinks) + len(bareURLs),
	}
}

// buildNotes turns what buildBlocks/detectImages/detectStructuralGaps
// observed into WriteResult.Notes. Lists, tables, code blocks, and links
// are NOT declared here: all four are now rendered structurally
// (lists/tables as of Task 2, code/links as of this task), so declaring
// them unsupported would be actively wrong (see the updated
// TestWrite_UnsupportedSyntaxIsDeclared, which now asserts their absence).
// Images remain the one inline construct this package never renders (no
// part of the OOXML skeleton embeds binary data), so they are still
// declared. tableNotes carries a narrower declaration for a specific
// structural compromise even within an otherwise-supported,
// well-formed-enough table: a ragged row's cells were dropped or padded
// rather than silently misaligning the rest of the table — see
// parseTable. gaps declares the four Task 6 (C3) additions -- HTML,
// footnotes, task-list checkboxes, autolinks/bare URLs -- each only when
// its count is nonzero. Nothing here is declared unconditionally:
// TestWrite_SupportedOnlyInputProducesNoNotes depends on fully-supported
// input producing an empty slice.
func buildNotes(hasImage bool, tableNotes []string, hasRefDef bool, gaps structuralGaps) []string {
	var notes []string
	if hasImage {
		notes = append(notes, "images are not embedded; written as plain text")
	}
	notes = append(notes, tableNotes...)
	if hasRefDef {
		// See buildBlocks' refDefRE branch: the definition line itself is
		// dropped (not printed as a body paragraph), and the
		// corresponding [text][label] reference usage elsewhere in the
		// document is left as literal text rather than resolved into a
		// hyperlink -- both halves of that behavior are declared here
		// together, per the notes contract (an empty Notes must mean the
		// input rendered exactly as written, so this half-supported
		// construct cannot pass through silently).
		notes = append(notes, "reference-style link definitions ([label]: url) are dropped, and [text][label] references are written as plain text, not hyperlinks")
	}
	if gaps.htmlTags > 0 {
		notes = append(notes, htmlTagNoteFor(gaps.htmlTags))
	}
	if gaps.footnotes > 0 {
		notes = append(notes, footnoteNoteFor(gaps.footnotes))
	}
	if gaps.taskListItems > 0 {
		notes = append(notes, taskListNoteFor(gaps.taskListItems))
	}
	if gaps.autolinkAndBareURLs > 0 {
		notes = append(notes, autolinkBareURLNoteFor(gaps.autolinkAndBareURLs))
	}
	return notes
}

// htmlTagNoteFor, footnoteNoteFor, taskListNoteFor and autolinkBareURLNoteFor
// render buildNotes' four Task 6 (C3) declarations, each with its own
// occurrence count (n is always > 0 -- buildNotes only calls these when a
// count is nonzero) and its own singular/plural wording, matching
// stripNoteFor's existing convention just below.
func htmlTagNoteFor(n int) string {
	if n == 1 {
		return "1 inline/block HTML tag is written as literal text, not interpreted"
	}
	return fmt.Sprintf("%d inline/block HTML tags are written as literal text, not interpreted", n)
}

func footnoteNoteFor(n int) string {
	if n == 1 {
		return "1 footnote marker ([^...]) is not supported; written as literal text"
	}
	return fmt.Sprintf("%d footnote markers ([^...]) are not supported; written as literal text", n)
}

func taskListNoteFor(n int) string {
	if n == 1 {
		return "1 task-list checkbox ([ ]/[x]) is written as literal text, not a real checkbox"
	}
	return fmt.Sprintf("%d task-list checkboxes ([ ]/[x]) are written as literal text, not real checkboxes", n)
}

func autolinkBareURLNoteFor(n int) string {
	if n == 1 {
		return "1 autolink or bare URL is written as literal text, not a hyperlink"
	}
	return fmt.Sprintf("%d autolinks or bare URLs are written as literal text, not hyperlinks", n)
}

// unknownEntityNoteFor renders WriteDocx's declaration that n (n > 0)
// HTML/XML entity references were left exactly as written because
// decodeHTMLEntities did not recognize them -- see
// renderCtx.unknownEntities' own doc comment. Parallels stripNoteFor
// immediately below, which declares a related but distinct count (a
// RECOGNIZED numeric entity naming an illegal XML codepoint).
func unknownEntityNoteFor(n int) string {
	if n == 1 {
		return "1 unrecognized HTML entity is left as literal text"
	}
	return fmt.Sprintf("%d unrecognized HTML entities are left as literal text", n)
}

// stripNoteFor renders WriteDocx's declaration that n characters (n > 0)
// were replaced for being illegal in XML 1.0 content -- see
// renderCtx.strippedXMLChars and xmlEscapeText's doc comment (splice.go).
// "stripped" describes the user-facing outcome (the character is gone from
// the visible text, replaced by nothing recognizable), even though the
// underlying mechanism substitutes U+FFFD rather than deleting outright.
func stripNoteFor(n int) string {
	if n == 1 {
		return "stripped 1 invalid XML character (not valid in a .docx; replaced)"
	}
	return fmt.Sprintf("stripped %d invalid XML characters (not valid in a .docx; replaced)", n)
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
// ["a", "b"]) and a backslash-escaped pipe ("\|") inside a cell, which is
// unescaped to a literal "|" in that cell's text rather than being treated
// as a delimiter (I2: "| a\|b | c |" must split into exactly two cells,
// "a|b" and "c", not three via an unescaped split that then silently drops
// the row as ragged). This is GFM's own minimum bar for pipe escaping in a
// table cell; it does not also special-case a pipe inside inline `code`
// (CommonMark itself does not require table-cell splitting to look inside
// inline code spans, and doing so here would need a second, code-aware pass
// duplicating parseInline's own lexing).
//
// The leading/trailing pipe strip below runs BEFORE the escape-aware split
// and is deliberately not itself escape-aware: an actual GFM table row's
// optional delimiter pipe is punctuation supplied by the row's shape, not
// cell content, and cannot itself be the first/last character of an
// escaped sequence (there is nothing for a backslash immediately at
// position 0 to have escaped).
func splitTableRow(line string) []string {
	t := strings.TrimSpace(line)
	t = strings.TrimPrefix(t, "|")
	t = strings.TrimSuffix(t, "|")

	var parts []string
	var cur strings.Builder
	for i := 0; i < len(t); i++ {
		if t[i] == '\\' && i+1 < len(t) && t[i+1] == '|' {
			cur.WriteByte('|')
			i++
			continue
		}
		if t[i] == '|' {
			parts = append(parts, strings.TrimSpace(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteByte(t[i])
	}
	parts = append(parts, strings.TrimSpace(cur.String()))
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
			if row.header {
				// GenOffice-compatibility copy of TableGrid's own header
				// shading (see styles.go's tableHeaderShadingXML): TableGrid's
				// <w:tblStylePr w:type="firstRow"> already carries this by
				// reference, but Google Docs and GenOffice do not apply a
				// table style's conditional formatting at all, so a header
				// cell also gets it written directly. <w:tcPr> is CT_Tc's
				// first child, so it must come before the cell's <w:p>.
				out.WriteString("<w:tcPr>" + tableHeaderShadingXML + "</w:tcPr>")
			}
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
// other bold/italic combination with code. strike marks ~~strikethrough~~
// (Task 6, C3): unlike the code-block border/shading duplication elsewhere
// in this file, <w:strike/> is a plain run property with no paragraph-level
// or style-level counterpart to keep in sync, so it needs no GenOffice-
// compatibility copy the way isCode/isQuote's <w:pBdr> do.
//
// isBreak marks a synthetic, textless segment standing in for a hard line
// break (see expandHardBreaks): when true, every other field is meaningless
// and renderRun emits a bare "<w:r><w:br/></w:r>" instead of consulting
// text/bold/italic/code/link at all.
type segment struct {
	text    string
	bold    bool
	italic  bool
	code    bool
	strike  bool
	link    string
	isBreak bool
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
	return parseInlineCtx(s, false, false, false)
}

// parseInlineCtx is parseInline's recursive worker: bold, italic and strike
// are the formatting already in effect from an enclosing marker pair
// (ambient state), and any marker resolved here sets its own flag with the
// others left untouched.
func parseInlineCtx(s string, bold, italic, strike bool) []segment {
	var segs []segment
	var buf strings.Builder
	flush := func() {
		if buf.Len() > 0 {
			segs = append(segs, segment{text: buf.String(), bold: bold, italic: italic, strike: strike})
			buf.Reset()
		}
	}

	i, n := 0, len(s)
	for i < n {
		c := s[i]
		if c == '\\' && i+1 < n && isASCIIPunct(s[i+1]) {
			// CommonMark backslash escape: '\' followed by ASCII
			// punctuation consumes the backslash and emits the
			// punctuation character literally, so it can never open or
			// close emphasis, a code span, or a link -- "\*not em\*" must
			// stay plain text, not italic (see Item C4/Task 4). A
			// backslash followed by anything else (a letter, digit,
			// whitespace, or nothing at all -- i+1 == n) is NOT an escape
			// per CommonMark and is left as a literal backslash, handled
			// by the default byte-copy case at the bottom of this loop;
			// that other character is then reprocessed normally on the
			// next iteration (e.g. "C:\path" keeps both the backslash and
			// every letter of "path", none of it consumed here).
			buf.WriteByte(s[i+1])
			i += 2
			continue
		}
		if c == '`' {
			// Inline code has the highest precedence: whatever is between
			// the backticks is emitted completely literally, with no
			// recursion — "`**not bold**`" must stay literal text inside a
			// monospace run, per Item 1.
			if j := strings.IndexByte(s[i+1:], '`'); j >= 0 {
				flush()
				segs = append(segs, segment{text: s[i+1 : i+1+j], bold: bold, italic: italic, strike: strike, code: true})
				i += 1 + j + 1
				continue
			}
			// Unclosed inline-code marker: keep the backtick as literal text.
			buf.WriteByte(c)
			i++
			continue
		}
		if c == '~' && i+1 < n && s[i+1] == '~' {
			// GFM strikethrough: "~~text~~" -> a run-level <w:strike/>
			// (Task 6, C3). Only the exact two-tilde delimiter is
			// recognized -- GFM does not define a single-"~" form the way
			// CommonMark defines single-"*"/"_" italics, so there is no
			// third, one-character branch here the way there is for
			// */_ below. indexClosingMarker's underscore-specific intraword
			// filtering never triggers for '~' (marker[0] != '_'), so this
			// is a plain substring search for the closing "~~", same as the
			// "**" bold branch below.
			marker := s[i : i+2]
			if j := indexClosingMarker(s, i+2, marker); j >= 0 {
				flush()
				segs = append(segs, parseInlineCtx(s[i+2:i+2+j], bold, italic, true)...)
				i += 2 + j + 2
				continue
			}
			// Unclosed strikethrough marker: keep it as literal text.
			buf.WriteString(marker)
			i += 2
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
				inner := parseInlineCtx(text, bold, italic, strike)
				for _, seg := range inner {
					seg.link = url
					segs = append(segs, seg)
				}
				i = end
				continue
			}
		}
		if (c == '*' || c == '_') && i+2 < n && s[i+1] == c && s[i+2] == c {
			// A run of THREE identical marker characters -- "***"/"___" --
			// is bold-and-italic combined (CommonMark: 2+1 or 1+2
			// delimiters stacked). This must be checked, and its closing
			// run of three found, BEFORE the plain "**" branch below gets
			// a chance to run: that branch searches for its close
			// starting right after only the first TWO marker characters,
			// which for "***x***" finds the THIRD marker character of the
			// OPENING run itself (mistaking it for part of the content
			// text, then for half of a close two characters later) --
			// exactly Item C1's "*x" + stray "*" bug, which also
			// corrupted every subsequent emphasis span in the same
			// paragraph because the leftover "*" kept participating in
			// later marker searches. If no closing run of three exists,
			// this falls through (no `continue`) to the "**" branch below
			// unchanged, so a merely-unclosed "***" still degrades the
			// same well-defined way every other unclosed marker does.
			marker3 := s[i : i+3]
			if !(c == '_' && intrawordUnderscore(s, i, 3)) {
				if j := indexClosingMarker(s, i+3, marker3); j >= 0 {
					flush()
					segs = append(segs, parseInlineCtx(s[i+3:i+3+j], true, true, strike)...)
					i += 3 + j + 3
					continue
				}
			}
		}
		if (c == '*' || c == '_') && i+1 < n && s[i+1] == c {
			marker := s[i : i+2]
			// CommonMark: '_' (unlike '*') may not open or close emphasis
			// when it sits inside a word -- immediately preceded AND
			// followed by a letter or digit (see intrawordUnderscore). A
			// "__" run flanked that way on both outer edges (e.g. the
			// "__" in "co__de__will") can't open here at all; fall
			// through to the literal-text path below. This check is not
			// redundant with indexClosingMarker's own intraword filtering
			// on the CLOSING side: without it, a "__" that is itself
			// intraword-disqualified as an opener could still be accepted
			// as one if a later, non-flanked "__" happens to close it
			// (e.g. "foo__bar__ end" -- the second "__" is followed by a
			// space, so it is a valid closer on its own, but the first
			// "__" must never have been allowed to open in the first
			// place).
			if c == '_' && intrawordUnderscore(s, i, len(marker)) {
				buf.WriteString(marker)
				i += 2
				continue
			}
			if j := indexClosingMarker(s, i+2, marker); j >= 0 {
				flush()
				segs = append(segs, parseInlineCtx(s[i+2:i+2+j], true, italic, strike)...)
				i += 2 + j + 2
				continue
			}
			// Unclosed bold marker: keep it as literal text.
			buf.WriteString(marker)
			i += 2
			continue
		}
		if c == '*' || c == '_' {
			marker := s[i : i+1]
			if c == '_' && intrawordUnderscore(s, i, len(marker)) {
				buf.WriteByte(c)
				i++
				continue
			}
			if j := indexClosingMarker(s, i+1, marker); j >= 0 {
				flush()
				segs = append(segs, parseInlineCtx(s[i+1:i+1+j], bold, true, strike)...)
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

// isWordRune reports whether r counts as a "word" character for the
// intraword-underscore rule below: a letter or digit. unicode.IsLetter
// classifies CJK ideographs (Unicode category Lo) as letters, so "word"
// here already includes Chinese/Japanese/Korean text, which is the
// documents this rule exists to protect ("PROXY_ORDER" mixed into Chinese
// prose) -- there is no separate CJK carve-out to add.
func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// isASCIIPunct reports whether b is one of CommonMark's ASCII punctuation
// characters -- the set a backslash escape (see parseInlineCtx's '\'
// branch) may precede: !"#$%&'()*+,-./:;<=>?@[\]^_`{|}~. This is exactly
// the four contiguous byte ranges that make up that set on the ASCII table.
func isASCIIPunct(b byte) bool {
	return (b >= '!' && b <= '/') ||
		(b >= ':' && b <= '@') ||
		(b >= '[' && b <= '`') ||
		(b >= '{' && b <= '~')
}

// intrawordUnderscore reports whether the underscore run s[i:i+width]
// ("_" or "__") is disqualified from opening OR closing emphasis because
// it sits inside a word: immediately preceded AND followed by a letter or
// digit (isWordRune). This is CommonMark's rule that only '*', not '_',
// may be used for intraword emphasis -- "a*b*c" italicises "b" but
// "snake_case_name" must stay literal. A run at a string edge, or next to
// whitespace/punctuation on either side, is never disqualified: only both
// sides being word characters rules it out, which is why a lone
// underscore at a word boundary (no partner to pair with) still survives,
// and why "_word_" at the very start/end of a paragraph still opens/closes
// (nothing precedes the opener, nothing follows the closer).
func intrawordUnderscore(s string, i, width int) bool {
	before := i > 0
	if before {
		r, _ := utf8.DecodeLastRuneInString(s[:i])
		before = isWordRune(r)
	}
	if !before {
		return false
	}
	after := i+width < len(s)
	if after {
		r, _ := utf8.DecodeRuneInString(s[i+width:])
		after = isWordRune(r)
	}
	return after
}

// indexClosingMarker finds the next occurrence of marker in s starting at
// byte offset start, returning its offset relative to start (the same
// convention as strings.Index(s[start:], marker)), or -1 if none exists.
// For '*' this is a plain substring search, unchanged from before this
// rule existed. For '_' it additionally skips any occurrence that
// intrawordUnderscore disqualifies as a closer -- e.g. in "_word_more_",
// the "_" right before "more" is intraword (preceded by 'd', followed by
// 'm') and cannot close, so the search continues past it to the final "_",
// which is followed by nothing and so can.
func indexClosingMarker(s string, start int, marker string) int {
	underscore := marker[0] == '_'
	for pos := start; ; {
		rel := strings.Index(s[pos:], marker)
		if rel < 0 {
			return -1
		}
		abs := pos + rel
		if !underscore || !intrawordUnderscore(s, abs, len(marker)) {
			return abs - start
		}
		pos = abs + 1
	}
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
	// Find the ')' that closes the '(' just consumed, tracking nesting
	// depth rather than stopping at the first ')' anywhere in the rest of
	// the string: a bare (unescaped, un-angle-bracketed) URL is allowed to
	// contain its own balanced parentheses -- e.g.
	// "https://example.com/wiki/Foo_(bar)" -- and the naive first-')'
	// search used to cut such a URL off right after "Foo_(bar", leaving a
	// dangling, un-openable target (Item I3).
	depth := 1
	pos := j + 2
	for pos < len(s) && depth > 0 {
		switch s[pos] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 {
			break
		}
		pos++
	}
	if depth != 0 {
		return "", "", 0, false
	}
	k := pos
	text = s[i+1 : j]
	url, _ = splitLinkDestTitle(s[j+2 : k])
	if strings.Contains(text, "\n") || strings.Contains(url, "\n") {
		return "", "", 0, false
	}
	return text, url, k + 1, true
}

// splitLinkDestTitle splits the raw content between a link's parentheses
// -- e.g. `url "title"` from `[t](url "title")` -- into the destination and
// an optional title, per Item I3: without this, a titled link's title text
// used to be concatenated straight onto the URL (the relationship target
// became `https://example.com/page "Example Title"`, which nothing can
// follow). A title is recognized only as a trailing, double- or
// single-quoted span preceded by whitespace -- e.g. content ending in
// `"Example Title"` or `'Example Title'` -- which is the common case this
// package's simplified (not full CommonMark) link syntax needs to support;
// a title written with parentheses instead of quotes is not recognized
// (parentheses there would be ambiguous with the URL's own balanced-paren
// handling in matchLinkAt) and is left as part of the destination, same as
// today. When no such trailing quoted span is found, title is empty and
// url is content unchanged -- the common, title-less case is a no-op.
func splitLinkDestTitle(content string) (url, title string) {
	trimmed := strings.TrimRight(content, " \t")
	if len(trimmed) < 2 {
		return content, ""
	}
	last := trimmed[len(trimmed)-1]
	if last != '"' && last != '\'' {
		return content, ""
	}
	body := trimmed[:len(trimmed)-1]
	openIdx := strings.LastIndexByte(body, last)
	if openIdx <= 0 || body[openIdx-1] != ' ' {
		// No matching opening quote, or nothing (not even a space)
		// separates it from the destination -- e.g. a URL that simply
		// ends in a quote character with no title intended. Treat the
		// whole thing as the destination, unchanged.
		return content, ""
	}
	url = strings.TrimRight(body[:openIdx-1], " \t")
	title = body[openIdx+1:]
	return url, title
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
	// strippedXMLChars accumulates, across every renderRun call this
	// document's render pass makes (body text, headings, table cells,
	// code-block lines, and hyperlink display text all go through
	// renderRun -- see its own doc comment), how many characters
	// escapeXMLText and decodeHTMLEntities each had to replace with U+FFFD
	// for being illegal in XML 1.0 content. WriteDocx sums this into a
	// "stripped N invalid XML character(s)" note once rendering finishes,
	// so a document that silently sanitized a pasted control character
	// still tells the caller it did -- see xmlEscapeText's doc comment
	// (splice.go) for why silence there would be actively wrong.
	strippedXMLChars int
	// unknownEntities accumulates, the same way strippedXMLChars does,
	// every HTML/XML entity reference decodeHTMLEntities did not recognize
	// (see namedHTMLEntities) and so left exactly as written -- e.g.
	// "&foo;" or a malformed "&#zz;". WriteDocx folds this into an
	// "unrecognized HTML entity" note once rendering finishes (Task 6,
	// C3): decodeHTMLEntities itself already had a considered, documented
	// reason to leave such an entity alone rather than drop or guess at
	// it, but that decision must still be declared, not left for the
	// caller to notice only by opening the file and seeing a literal
	// "&foo;" in the text.
	unknownEntities int
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

// addFontTableRelID allocates the relationship id for word/fontTable.xml
// from the exact same shared counter addLink/addFooterRelID draw from, for
// the identical reason addFooterRelID's doc comment gives: a second,
// independent counter for a THIRD part would still only need one of the
// two spaces to overlap for Word to resolve a relationship id to the wrong
// target, which is indistinguishable from file corruption to a user. There
// is exactly one counter in this package, and every relationship beyond the
// two permanently-fixed ones (styles.xml, numbering.xml) draws from it.
func (c *renderCtx) addFontTableRelID() string {
	id := fmt.Sprintf("rId%d", c.nextRelID)
	c.nextRelID++
	return id
}

// codeFontXML renders c.fonts' code Latin/East-Asian pair as a direct
// <w:rFonts> via styles.go's codeRunFontsXML — the single source SourceCode
// and VerbatimChar themselves build their own <w:rFonts> from, so this
// method's output is always byte-identical to whichever of those two styles
// applies. It has two call sites now: the one narrow edge case (inline code
// that is ALSO a hyperlink's text, "[`code`](url)") that cannot reference
// either style at all, because a run's <w:rPr> permits at most one
// <w:rStyle> and this combination already needs Hyperlink's; and, as of the
// GenOffice-compatibility task, every ordinary fenced-code-block line too
// (see renderRun's codeBlockLine case) — GenOffice does not apply a
// paragraph style's <w:rFonts>, so a code run needs this written directly
// even though pStyle="SourceCode" already carries the identical font by
// reference. It is run-level FONT formatting, not one of the three
// paragraph-level properties (spacing/ind) the styles-architecture invariant
// still bans inline everywhere.
func (c *renderCtx) codeFontXML() string {
	return codeRunFontsXML(c.fonts)
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
// <w:pBdr> survives inline for isHR (a horizontal rule is a one-off empty
// paragraph, never repeated as a shared visual identity the way
// SourceCode/Quote/ListParagraph/TableGrid are, so there is no shared style
// for it to move into -- this predates the styles-architecture plan
// entirely) AND, as of the GenOffice-compatibility task, for isCode AND
// isQuote too: SourceCode's own <w:pBdr>/<w:shd>, and Quote's own <w:pBdr>
// (I9), are each copied directly onto their paragraph alongside the pStyle
// reference, because GenOffice does not resolve a paragraph style's border
// or shading at all. See TestWrite_NoInlineVisualPropertiesInDocumentXML's
// own doc comment for the full, narrowed statement of which properties are
// still banned inline and which named exceptions (plus this pre-existing
// isHR one) are not.
//
// forceBold ORs bold onto every inline segment (used for a table header
// row) regardless of the markdown markers parseInline already resolved.
// isCode paragraphs skip parseInline entirely: b.text (after
// mergeCodeBlocks, above) is the whole block's lines joined with "\n", and
// renderCodeBlockRuns turns each line back into its own literal run --
// monospace via pStyle="SourceCode" rather than a direct <w:rFonts> on the
// run -- separated by a <w:br/>, never interpreting markdown inside any of
// them. That is how Item 1's "markdown is not interpreted inside a fenced
// code block" is enforced structurally rather than by convention. isHR
// paragraphs carry no runs at all: text is never even looked at when isHR
// is set, since a horizontal rule is exactly one empty paragraph with a
// bottom border.
func renderParagraph(b paraBlock, ctx *renderCtx) (string, error) {
	var runsXML string
	var err error
	switch {
	case b.isHR:
		// No runs: a horizontal rule is an empty paragraph carrying only
		// the bottom-border <w:pBdr> assembled below.
	case b.isCode:
		// One <w:p>, one border, one shading -- see renderCodeBlockRuns'
		// own doc comment for why this no longer goes through
		// renderRuns/parseInline the way every other paragraph kind does.
		runsXML, err = renderCodeBlockRuns(b.text, ctx)
	default:
		segs := parseInline(b.text)
		if b.forceBold {
			for i := range segs {
				segs[i].bold = true
			}
		}
		// expandHardBreaks turns any hardBreakMarker left inside a
		// segment's text (see splitTrailingHardBreak/joinWithHardBreaks in
		// buildBlocks) into a real, textless isBreak segment between the
		// text on either side of it. It must run after parseInline, which
		// is what splits b.text into per-formatting segments in the first
		// place; running it after the forceBold loop above (rather than
		// before) means a split-off piece inherits forceBold's already-set
		// bold flag along with everything else the original segment
		// carried, instead of needing forceBold's loop to also walk the
		// newly split pieces.
		segs = expandHardBreaks(segs)
		// codeBlockLine is always false here: a code-block paragraph is
		// handled entirely above, so renderRuns/renderRun's codeBlockLine
		// branch is now reached only via renderCodeBlockRuns's own direct
		// renderRun calls, never through this path.
		runsXML, err = renderRuns(segs, ctx)
	}
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
		// GenOffice-compatibility copy of Quote's own left border (see
		// styles.go's quoteBorderXML doc comment): the pStyle reference
		// above already carries it by name, but GenOffice does not apply a
		// paragraph style's <w:pBdr> at all, so every quote paragraph also
		// gets it written directly -- the same mechanism as the isCode
		// case just below (I9).
		pPr.WriteString(quoteBorderXML)
	case b.isCode:
		fmt.Fprintf(&pPr, `<w:pStyle w:val="%s"/>`, StyleSourceCode)
		// GenOffice-compatibility copy of SourceCode's own border and
		// shading (see styles.go's codeBorderXML/codeShadingXML): the
		// pStyle reference above already carries both by name, but
		// GenOffice does not apply a paragraph style's <w:pBdr> or <w:shd>
		// at all, so every code paragraph also gets them written directly.
		// <w:pBdr> then <w:shd> is CT_PPr's schema order, both ahead of the
		// <w:jc> switch below (which isCode never actually sets, but keeps
		// this correct even if it did).
		pPr.WriteString(codeBorderXML)
		pPr.WriteString(codeShadingXML)
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
// renderRuns is renderParagraph's default (non-code, non-HR) path, so it
// always calls renderRun with codeBlockLine false: a code-block paragraph
// is handled entirely by renderCodeBlockRuns instead, which calls
// renderRun directly (codeBlockLine true) for each of its own lines. See
// renderRun's own doc comment for what that flag changes.
func renderRuns(segs []segment, ctx *renderCtx) (string, error) {
	var out strings.Builder
	i := 0
	for i < len(segs) {
		seg := segs[i]
		if seg.link == "" {
			r, err := renderRun(seg, ctx, false)
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
			r, err := renderRun(segs[j], ctx, false)
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

// expandHardBreaks scans parseInline's output for any segment whose text
// still contains hardBreakMarker -- planted by joinWithHardBreaks when
// buildBlocks joined two source lines that a hard line break (two-or-more
// trailing spaces, or a trailing backslash -- see splitTrailingHardBreak)
// separated, rather than an ordinary soft-wrap space -- and splits it into
// however many text-carrying segments the marker demands, with a
// textless, isBreak segment (rendered as a bare <w:r><w:br/></w:r> by
// renderRun) spliced in at each split point. A segment with no marker at
// all (the common case: most paragraphs have no hard break) is returned
// unchanged, not copied, so this is a no-op pass over an already-supported
// document.
//
// A code segment is deliberately left untouched even if it somehow
// contains the marker: Item 1's rule that markdown/formatting is never
// interpreted inside inline `code` extends to this marker too, since it is
// itself just another (private-use-area) character as far as a `code` span
// is concerned. This can only actually arise from an unusual unterminated
// inline-code span spanning a hard-broken line -- see parseInlineCtx's '`'
// branch -- and, left alone, the marker's 3 raw UTF-8 bytes simply render
// as one more literal (if invisible) character inside the code run; no
// existing test exercises this corner, and it is not what this task's
// brief asks for.
//
// An empty text piece adjacent to a break (e.g. two consecutive hard
// breaks, or a break at the very start/end of the segment) is dropped
// rather than turned into an empty-text run: renderRun's own early return
// already treats seg.text == "" as "nothing to write" for every non-code-
// block caller, so keeping it here would just be a run that renders as
// nothing between two <w:br/>s -- unlike renderCodeBlockRuns' blank code
// LINE, an empty inline-text piece carries no positional information a
// reader (or scan.go's Para.Breaks) needs to preserve.
func expandHardBreaks(segs []segment) []segment {
	out := make([]segment, 0, len(segs))
	for _, seg := range segs {
		if seg.code || !strings.Contains(seg.text, hardBreakMarker) {
			out = append(out, seg)
			continue
		}
		parts := strings.Split(seg.text, hardBreakMarker)
		for i, part := range parts {
			if i > 0 {
				out = append(out, segment{isBreak: true})
			}
			if part == "" {
				continue
			}
			piece := seg
			piece.text = part
			out = append(out, piece)
		}
	}
	return out
}

// renderCodeBlockRuns renders one code-block paragraph's full text -- b.text
// after mergeCodeBlocks (buildBlocks, above) has joined every line of the
// block with "\n" -- as the runs of a SINGLE <w:p>: one <w:r> per line,
// blank lines included, each carrying seg.code's literal-text/
// xml:space="preserve" handling exactly as renderRun always has, with a
// separate, textless "<w:r><w:br/></w:r>" between every pair of adjacent
// lines standing in for the line break a paragraph boundary used to carry.
//
// This is the fix for the box-per-line defect a renderer that does not
// merge adjacent identical-<w:pBdr> paragraphs (unlike Word) draws: one
// <w:p> means exactly one <w:pPr><w:pBdr>/<w:shd> for the whole block, in
// every renderer, with no merging assumption required at all. Splitting on
// "\n" and rejoining with an explicit run-level <w:br/> (rather than one
// run holding embedded "\n" characters inside its own <w:t>) is what keeps
// this readable back out: scan.go's Para.Breaks -- already built, before
// this task, for exactly this shape (see its own doc comment and
// TestScan_BreaksRecordRunPositions) -- records a break as "after Run.Index
// N", and read.go's paraTextWithBreaks turns that back into "\n" in the
// markdown Read renders, recovering the same per-line view an LLM had
// before when each line was its own paragraph, just under one shared
// "[para N]" marker instead of N of them.
//
// EVERY line gets its own run, even a blank one — renderRun's codeBlockLine
// argument (true here) is what lets an empty-text segment through instead
// of being skipped the way it would be anywhere else. This looks
// redundant (why write "<w:t></w:t>" for nothing?) until two adjacent
// blank lines, or a blank line right after the fence, are considered: see
// renderRun's own doc comment for exactly why a blank line MUST anchor its
// own run rather than contribute nothing between two <w:br/>s, and
// TestWrite_FencedCodeBlockBlankLineSurvives/
// TestWrite_IndentedCodeBlockContinuesThroughBlankLines/
// TestWrite_FencedCodeBlockLeadingBlankLineSurvives for the read-back
// regressions this closes. For N lines there are always exactly N-1
// separators, one per adjacent PAIR, regardless of which individual lines
// are empty: "a\n\nb" (a blank line in the middle) becomes
// run(a) + break + run("") + break + run(b), two breaks for three lines,
// exactly as CommonMark's own count would predict.
func renderCodeBlockRuns(text string, ctx *renderCtx) (string, error) {
	lines := strings.Split(text, "\n")
	var out strings.Builder
	for i, line := range lines {
		if i > 0 {
			out.WriteString("<w:r><w:br/></w:r>")
		}
		r, err := renderRun(segment{text: line, code: true}, ctx, true)
		if err != nil {
			return "", err
		}
		out.WriteString(r)
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
//
// A numeric reference (either form) that names a codepoint XML 1.0
// forbids in document content -- "&#1;" is SOH, "&#x0B;" is vertical tab,
// neither legal -- is NOT decoded into that raw illegal character: doing
// so would hand escapeXMLText (splice.go) text containing an already-
// decoded control byte no different from one pasted in directly, except
// arriving one step removed from where that function's own doc comment
// says to look for it. decodeNumericRune (below) substitutes U+FFFD
// instead, the same replacement xmlEscapeText makes for a directly-pasted
// illegal character, and counts it the same way, so the two sources add up
// into one honest total for WriteDocx's "stripped N invalid XML
// character(s)" note. The second return value is that count.
//
// The third return value counts a DIFFERENT thing (Task 6, C3): every
// entity reference htmlEntityRE matched but this function did not
// recognize at all -- an unlisted name ("&foo;"), or a numeric reference
// whose digits failed to parse -- and so left completely unchanged rather
// than decoding. This is not the same population strippedXMLChars/invalid
// counts (a RECOGNIZED numeric entity naming an illegal XML codepoint,
// still decoded via decodeNumericRune, just replaced with U+FFFD); an
// unrecognized entity's "return m" branch never touches invalid at all.
// renderRun folds this into ctx.unknownEntities, which WriteDocx declares
// in Notes once rendering finishes, the same way strippedXMLChars is.
func decodeHTMLEntities(s string) (out string, invalid, unknown int) {
	if !strings.Contains(s, "&") {
		return s, 0, 0
	}
	out = htmlEntityRE.ReplaceAllStringFunc(s, func(m string) string {
		body := m[1 : len(m)-1] // strip leading '&' and trailing ';'
		switch {
		case strings.HasPrefix(body, "#x") || strings.HasPrefix(body, "#X"):
			if n, err := strconv.ParseInt(body[2:], 16, 32); err == nil && n > 0 {
				return decodeNumericRune(rune(n), &invalid)
			}
		case strings.HasPrefix(body, "#"):
			if n, err := strconv.ParseInt(body[1:], 10, 32); err == nil && n > 0 {
				return decodeNumericRune(rune(n), &invalid)
			}
		default:
			if r, ok := namedHTMLEntities[body]; ok {
				return r
			}
		}
		unknown++
		return m // unrecognized: stays literal
	})
	return out, invalid, unknown
}

// decodeNumericRune returns the character a numeric entity's codepoint r
// decodes to, unless isLegalXMLChar (edit.go) rejects r as illegal in XML
// 1.0 content, in which case it increments *invalid and returns U+FFFD
// instead -- see decodeHTMLEntities' doc comment for why a numeric
// reference must never be allowed to decode into a raw illegal character.
func decodeNumericRune(r rune, invalid *int) string {
	if !isLegalXMLChar(r) {
		*invalid++
		return string(utf8.RuneError)
	}
	return string(r)
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
// codeBlockLine is true only when this run is one line of a code-block
// paragraph (renderCodeBlockRuns calls this directly, once per line, with
// it set) — never for a `code` span inline inside an ordinary paragraph.
// It suppresses the VerbatimChar rStyle below, because a code-block
// paragraph already gets its monospace font from pStyle="SourceCode"
// (styles.go's rPr cascades to every run in a paragraph of that style);
// adding VerbatimChar there too would be a second, redundant source of the
// identical formatting.
//
// codeBlockLine ALSO disables the empty-text early return immediately
// below: for every other caller, a segment with no text genuinely has
// nothing to write, but renderCodeBlockRuns needs a real (if textless)
// <w:r><w:t></w:t></w:r> for a blank code line, not nothing at all.
// scan.go always appends a Run on </w:t> regardless of its content
// (Run.Text == "" is a valid run, not a skipped one), so this still shows
// up as a genuine run with its own Run.Index — which is exactly the point:
// it gives the <w:br/> immediately before and/or after a blank line
// somewhere to anchor to. Without it, two (or more) consecutive
// "<w:r><w:br/></w:r>" elements with no run between them would all be
// recorded by scan.go's paraBreaks as "after the same run index" (it
// counts runs seen so far, and none were seen between them), and
// read.go's paraTextWithBreaks — a map[int]bool keyed by that index, not a
// counter — collapses any number of same-indexed entries down to one "\n",
// silently swallowing every blank line but the first. A break before the
// very first run in the paragraph (a blank line opening a fenced block)
// has the same root cause one level further: paraTextWithBreaks only ever
// emits "\n" AFTER a run's own text in its loop over p.Runs, so a break
// with NO preceding run at all is dropped entirely, not merely merged.
// Giving every line — blank ones included — its own run, first line
// included, means a leading blank line is never that "break before any
// run" case either: see
// TestWrite_FencedCodeBlockBlankLineSurvives/TestWrite_IndentedCodeBlockContinuesThroughBlankLines
// for the regression this closes and
// TestWrite_FencedCodeBlockLeadingBlankLineSurvives for the leading case
// specifically. Neither scan.go nor read.go is on this task's list of
// files it may modify, so the fix has to live entirely on the write side.
//
// seg.isBreak (Task 6) short-circuits everything above: a hard-line-break
// segment carries no text, formatting, or link at all, and renders as
// nothing but a bare "<w:r><w:br/></w:r>" -- deliberately checked before
// the seg.text == "" early return just below, since an isBreak segment's
// text is always "" and would otherwise hit that return and vanish instead
// of producing the break it exists to produce.
func renderRun(seg segment, ctx *renderCtx, codeBlockLine bool) (string, error) {
	if seg.isBreak {
		return "<w:r><w:br/></w:r>", nil
	}
	if seg.text == "" && !codeBlockLine {
		return "", nil
	}
	text := seg.text
	if !seg.code {
		var entityInvalid, entityUnknown int
		text, entityInvalid, entityUnknown = decodeHTMLEntities(text)
		ctx.strippedXMLChars += entityInvalid
		ctx.unknownEntities += entityUnknown
	}
	escaped, invalid, err := escapeXMLText(text)
	ctx.strippedXMLChars += invalid
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
	case codeBlockLine:
		// GenOffice-compatibility copy of SourceCode's own <w:rFonts> (see
		// codeFontXML's doc comment and styles.go's codeRunFontsXML): the
		// pStyle="SourceCode" reference below already carries this font by
		// name, but GenOffice does not resolve a paragraph style's
		// <w:rFonts>, so every code-block-line run also gets it written
		// directly.
		rPr.WriteString(ctx.codeFontXML())
	}
	if seg.bold {
		rPr.WriteString("<w:b/>")
	}
	if seg.italic {
		rPr.WriteString("<w:i/>")
	}
	if seg.strike {
		// CT_RPr schema order places strike after b/i, matching this
		// function's existing "schema order even though this package
		// rarely combines them" convention. Unlike isCode/isQuote's
		// <w:pBdr>/<w:shd>, there is no GenOffice-compatibility copy to
		// write here: <w:strike/> is already a direct run property, not
		// something a style reference could fail to carry through (see
		// segment.strike's own doc comment).
		rPr.WriteString("<w:strike/>")
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

// buildContentTypesXML builds [Content_Types].xml. word/footer1.xml's and
// word/fontTable.xml's Overrides are both unconditional -- Part C of the
// docx-chinese-typography plan adds a footer to every document WriteDocx
// produces, and this task adds a font table to every document the same way,
// neither an opt-in --  so neither needs a hasTitle-style flag guarding it.
// The docProps/core.xml Override is included only when hasTitle is true,
// matching WriteDocx only adding that entry in the same condition -- an
// Override for a part that does not exist would itself make the package
// invalid.
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
	b.WriteString(`<Override PartName="/word/fontTable.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.fontTable+xml"/>`)
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
//
// title is NOT markdown body content, so it is not one of the "code
// blocks/tables/headings/hyperlink text" paths renderRun covers -- but
// WriteOptions.Title can independently carry an XML-1.0-illegal character
// (e.g. Title == "Bad\x1bTitle" with the body's own first line supplying a
// different H1, so parseMarkdown never copies Title into a block renderRun
// ever sees). escapeXMLText replaces such a character the same as it does
// everywhere else, and ctx accumulates the count into the SAME
// ctx.strippedXMLChars renderRun feeds, so WriteDocx's one "stripped N
// invalid XML character(s)" note still covers it -- a caller must never be
// able to observe "Notes: []" on a document that silently substituted a
// character in its own declared title. See
// TestWrite_IllegalCharInTitleIsCountedInNotes.
func docPropsCoreXML(title string, ctx *renderCtx) (string, error) {
	escaped, n, err := escapeXMLText(title)
	ctx.strippedXMLChars += n
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
// docx-chinese-typography plan — see renderCtx.addFooterRelID), the one
// font-table relationship every document now also gets (fontTableRelID,
// this task — see renderCtx.addFontTableRelID; like the footer and
// styles/numbering, nothing in document.xml's body references it by r:id,
// Word finds it purely by relationship Type, the same as python-docx's own
// bundled default.docx), plus one hyperlink relationship per link the
// document's render pass collected (rId3 upward, see renderCtx.addLink).
// This can no longer be a constant, unlike Task 1/2's docRelsXML: the set
// of hyperlink relationships depends on the document's content, and
// footerRelID/fontTableRelID vary with how many of those there are. rels is
// built by walking blocks in a fixed, deterministic order (WriteDocx's
// render loop), and footerRelID/fontTableRelID are allocated exactly once
// right after that loop finishes, so the SAME markdown always allocates the
// SAME ids in the SAME order — the determinism guarantee
// (TestWrite_IsDeterministic) extends to this part too, not just to
// document.xml.
//
// A link's URL is XML-escaped before going into the Target attribute: a
// URL containing "&" (an ordinary query-string separator) would otherwise
// produce the same "unreadable content" failure escapeXMLText already
// guards against for run text.
//
// A URL is not the "hyperlink text" renderRun covers (that's the link's
// display text, e.g. "x" in "[x](url)") -- but a URL can independently
// carry an XML-1.0-illegal character (a stray control byte pasted into the
// href, e.g. "https://example.com/\x1bpath"), and escapeXMLText replaces
// it here exactly as it does everywhere else. ctx accumulates that count
// into the SAME ctx.strippedXMLChars renderRun and docPropsCoreXML feed,
// so it is never lost from WriteDocx's one "stripped N invalid XML
// character(s)" note -- see TestWrite_IllegalCharInLinkURLIsCountedInNotes.
func buildDocRelsXML(rels []hyperlinkRel, footerRelID, fontTableRelID string, ctx *renderCtx) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	b.WriteString(`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`)
	b.WriteString(`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/numbering" Target="numbering.xml"/>`)
	fmt.Fprintf(&b, `<Relationship Id="%s" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/footer" Target="footer1.xml"/>`, footerRelID)
	fmt.Fprintf(&b, `<Relationship Id="%s" Type="%s" Target="fontTable.xml"/>`, fontTableRelID, fontTableRelType)
	for _, r := range rels {
		escapedURL, n, _ := escapeXMLText(r.url)
		ctx.strippedXMLChars += n
		fmt.Fprintf(&b, `<Relationship Id="%s" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/hyperlink" Target="%s" TargetMode="External"/>`,
			r.id, escapedURL)
	}
	b.WriteString(`</Relationships>`)
	return b.String()
}

// ---------------------------------------------------------------------------
// word/fontTable.xml
// ---------------------------------------------------------------------------

// fontTablePart is the OPC part name for the font table: the ECMA-376
// §17.8.3 mechanism that lets Word substitute more sensibly for a font it
// cannot find, by hinting what KIND of font is missing --
// <w:charset>/<w:family>/<w:pitch> -- rather than leaving Word to guess with
// no information at all. Before this task WriteDocx wrote no
// word/fontTable.xml whatsoever, which is exactly why a machine missing the
// CJK font named for a code block's East Asian glyphs got an arbitrary
// substitute with no signal about whether it should even be monospace --
// the box-drawing misalignment reported against this package's output.
//
// This raises the ceiling on substitution; it does not reach web-font
// fallback. OOXML has no ordered priority list of alternative fonts for
// Word to walk through -- fontTableXML's <w:pitch>/<w:charset>/<w:family>
// only bias the SINGLE metric-guided substitution Word already performs on
// its own, and even a same-shaped substitute will not reproduce the exact
// 2:1 Latin-to-full-width advance ratio ASCII box-drawing needs against CJK
// text (see defaultCodeEastAsiaFont's doc comment in styles.go for why even
// choosing the RIGHT font name does not get that ratio for free). Treat
// this as "Word now knows the code font was supposed to be fixed-pitch,"
// not "the alignment problem is solved."
//
// <w:altName> (see knownFontMetadata) is narrower still and easy to
// mis-read as a fallback font. Per ECMA-376 17.8.3.1 (confirmed against
// three independent renderings of the spec text while building this: an
// application "should attempt to locate a font with the name specified in
// the altName element" -- i.e. try the SAME font under a second name --
// "before doing substitution based on font metrics"). It is an alternate
// NAME for the identical font (e.g. a Chinese-named font's English name),
// tried before metric substitution kicks in at all, never a different,
// visually-substitute font. This package only emits it for name pairs it
// has independently verified are the same font under two names.
const fontTablePart = "word/fontTable.xml"

// fontTableRelType is the OPC relationship type Word uses to locate the
// font table. As with styles.xml and numbering.xml, nothing in
// document.xml's body references fontTable.xml by r:id -- Word finds it
// purely by this relationship Type, verified directly against
// python-docx's own bundled default.docx (word/_rels/document.xml.rels
// there carries "Type=.../relationships/fontTable Target=fontTable.xml"
// with no corresponding r:id anywhere in that package's document.xml).
const fontTableRelType = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/fontTable"

// fontMeta is the real, independently-checked metadata this package
// carries for one specific, named font -- see knownFontMetadata.
type fontMeta struct {
	// charset is the legacy Windows charset byte, hex-encoded: "00" (ANSI,
	// Western/Latin) or "86" (GB2312, Simplified Chinese) or "80"
	// (SHIFTJIS, Japanese) for every font below -- the same values
	// observed directly in python-docx's bundled default.docx
	// (word/fontTable.xml there charsets Times New Roman/Calibri/Cambria
	// "00" and ＭＳ 明朝/ＭＳ ゴシック "80").
	charset string
	// family is CT_Font's coarse design classification. "auto" (let the
	// consumer decide) is what real Word-authored fontTable.xml uses for
	// nearly everything, INCLUDING genuinely monospace fonts (python-docx's
	// default.docx classifies its own Courier entry "auto", not "modern") --
	// so "auto" is not a weaker or lazier choice than a specific
	// classification, it is what real Word output actually does for a font
	// whose true design category it cannot otherwise assert. "modern" is
	// used only for the two fonts below verified by that same real example
	// (ＭＳ ゴシック).
	family string
	// altName is a genuine alternate NAME for this exact font (its name
	// under a different locale), never a different substitute font -- see
	// fontTablePart's doc comment on what ECMA-376 17.8.3.1 actually
	// specifies. Empty when this package has not independently verified a
	// real alternate name.
	altName string
	// pitch is this font's own, VERIFIED, real fixed/variable-pitch design
	// -- independent of which role (body or code) a WriteOptions caller
	// happens to use it for in a given document. Empty when this package
	// has not independently verified the font's true pitch; fontTableXML
	// then falls back to a role-based guess for that one name (see
	// fontTableXML's dedup loop) rather than asserting something unverified
	// here.
	//
	// This field is deliberately NOT keyed off role the way it was before a
	// verification pass caught the bug: a font's real pitch is a property
	// of the font, not of how one particular document happens to be using
	// it, and asserting the wrong one for a font this package's own docs
	// already describe correctly elsewhere (微软雅黑's doc comment in
	// styles.go: "an ordinary proportional UI font") would tell Word
	// something false about it. See fontTableXML's doc comment for the
	// concrete failure this caused before the fix: this package's own
	// DEFAULT configuration uses 微软雅黑 for both the body's and the code
	// block's East Asian font, and declaring the shared entry "fixed"
	// (because the code role asked for that) meant a machine missing
	// 微软雅黑 could get a monospace substitute for ordinary Chinese body
	// text -- exactly the kind of visible misalignment this feature exists
	// to prevent, just relocated from the code block to the body text.
	pitch string
}

// knownFontMetadata carries real, checked metadata for exactly the fonts
// this package's own documentation already names by value (styles.go's
// default*Font constants, and defaultCodeEastAsiaFont's doc comment's list
// of fonts that hold the 2:1 Latin/CJK advance ratio: NSimSun, MS Gothic).
// It is deliberately NOT a general font database: this package cannot
// inspect an arbitrary installed font's real PANOSE/OS-2 signature, and
// asserting one for a name it has never seen would be a fabricated claim,
// not a verified one -- see fontTableXML's handling of an unrecognized name
// for what it gets INSTEAD of a (potentially wrong) guess.
//
// Each Chinese/Japanese entry's altName is the same font's English-locale
// name (both directions are listed so the table works no matter which
// name a caller passes) -- e.g. a font installed as "微软雅黑" on a
// Chinese-locale system is the identical font Windows lists as "Microsoft
// YaHei" in English; ECMA-376 17.8.3.1's altName exists precisely for this
// same-font-different-locale-name case, not for a different, merely
// similar-looking font.
//
// pitch is filled in only where this package has independent, confident
// grounds to assert it: Calibri and Consolas are unambiguous (Calibri is a
// standard proportional UI face; Consolas is Microsoft's own monospaced
// coding font, marketed as such). 微软雅黑/Microsoft YaHei is "variable"
// on the explicit authority of this package's OWN doc comment elsewhere
// (defaultCodeEastAsiaFont in styles.go: "微软雅黑 does not have that
// [2:1] ratio -- it is an ordinary proportional UI font"). 新宋体/NSimSun
// and ＭＳ ゴシック/MS Gothic are "fixed" on the same doc comment's
// authority, which names them (alongside Sarasa Gothic and Noto Sans Mono
// CJK, NOT included here -- see this task's report for why) as fonts that
// DO hold the 2:1 ratio, i.e. were specifically designed fixed-pitch for
// this exact code-alignment purpose. 宋体/SimSun and 黑体/SimHei are left
// with pitch "" (unverified): this package has no independently-confirmed
// claim about their true pitch flag one way or the other, so they fall
// back to fontTableXML's role-based guess like any unrecognized font.
var knownFontMetadata = map[string]fontMeta{
	"Calibri":         {charset: "00", family: "auto", pitch: "variable"},
	"Consolas":        {charset: "00", family: "auto", pitch: "fixed"},
	"微软雅黑":            {charset: "86", family: "auto", altName: "Microsoft YaHei", pitch: "variable"},
	"Microsoft YaHei": {charset: "86", family: "auto", altName: "微软雅黑", pitch: "variable"},
	"宋体":              {charset: "86", family: "auto", altName: "SimSun"},
	"SimSun":          {charset: "86", family: "auto", altName: "宋体"},
	"黑体":              {charset: "86", family: "auto", altName: "SimHei"},
	"SimHei":          {charset: "86", family: "auto", altName: "黑体"},
	"新宋体":             {charset: "86", family: "modern", altName: "NSimSun", pitch: "fixed"},
	"NSimSun":         {charset: "86", family: "modern", altName: "新宋体", pitch: "fixed"},
	"ＭＳ ゴシック":         {charset: "80", family: "modern", altName: "MS Gothic", pitch: "fixed"},
	"MS Gothic":       {charset: "80", family: "modern", altName: "ＭＳ ゴシック", pitch: "fixed"},
}

// fontTableRole is one (name, intended pitch, script bucket) triple
// fontTableXML resolves into a <w:font> entry. bucket only matters for a
// name knownFontMetadata does not recognize (see fontTableXML) -- it is
// what lets an arbitrary custom East-Asian font still get charset "86"
// rather than the Latin default "00" without this package pretending to
// know anything more specific about it.
type fontTableRole struct {
	name   string
	pitch  string
	bucket string // "latin" | "eastAsia"
}

// fontTableRoles lists the four fonts fontOptions actually names, paired
// with the pitch the task calls for per role: "fixed for the code fonts,
// variable for the body fonts". A caller-supplied custom font (any of
// WriteOptions' four font fields) flows into fontOptions before this is
// called (see resolveFontOptions), so this always reflects what THIS
// document's styles.xml actually uses, not a fixed set of names.
func fontTableRoles(f fontOptions) []fontTableRole {
	return []fontTableRole{
		{name: f.bodyLatin, pitch: "variable", bucket: "latin"},
		{name: f.bodyEastAsia, pitch: "variable", bucket: "eastAsia"},
		{name: f.codeLatin, pitch: "fixed", bucket: "latin"},
		{name: f.codeEastAsia, pitch: "fixed", bucket: "eastAsia"},
	}
}

// rolePitchOrVerified resolves the pitch fontTableXML should claim for one
// role sighting of name: knownFontMetadata's verified, role-INDEPENDENT
// truth when this package has one (see fontMeta.pitch's doc comment),
// otherwise r's role-based guess (variable for a body role, fixed for a
// code role). A verified truth always wins over a role guess -- a role is
// only ever a stand-in for what this package does not actually know.
func rolePitchOrVerified(name, rolePitch string) string {
	if meta, ok := knownFontMetadata[name]; ok && meta.pitch != "" {
		return meta.pitch
	}
	return rolePitch
}

// fontTableXML renders word/fontTable.xml for f -- the four (deduplicated)
// font names this document's styles.xml actually references, each with a
// <w:font> entry carrying at least charset/family/pitch (see
// fontTablePart's doc comment for why pitch is the load-bearing one).
//
// Self-review, pinned by TestWrite_FontTableSharedFontBetweenBodyAndCodeIsMarkedFixed
// (renamed post-fix; see its own comment): this package's own
// zero-configuration default sets BodyEastAsiaFont and CodeEastAsiaFont to
// the identical name (微软雅黑 -- see defaultBodyEastAsiaFont/
// defaultCodeEastAsiaFont in styles.go), so the dedup loop below sees that
// one name twice, once per role. A verified font (微软雅黑 is one --
// rolePitchOrVerified returns "variable" for it regardless of which role
// asked) never actually conflicts: both sightings resolve to the same
// truthful value, so there is nothing to arbitrate. This is the fix for a
// real bug an earlier version of this function had: it computed pitch from
// ROLE ALONE and let a later "fixed" (code) sighting overwrite an earlier
// "variable" (body) one, which for 微软雅黑 specifically asserted something
// false -- 微软雅黑 is an ordinary proportional UI font (see
// defaultCodeEastAsiaFont's own doc comment in styles.go) -- and meant a
// machine missing 微软雅黑 could get a monospace substitute for ordinary
// Chinese BODY text, not just the code block. That is worse than the
// problem this feature exists to fix, not a harmless simplification.
//
// A genuine conflict can still occur for a name knownFontMetadata does NOT
// carry a verified pitch for (an unrecognized custom font, or 宋体/黑体,
// which this package has deliberately left unverified -- see
// knownFontMetadata's doc comment): if the SAME unverified name is used for
// both a body and a code role, this package has no truthful answer either
// way, and the last sighting -- always "variable" wins is enforced below
// -- is chosen for the same reason "fixed" was too aggressive above: a
// false "variable" only costs the code-alignment hint (no worse than
// omitting pitch for that font, which is this package's pre-existing
// behavior), while a false "fixed" risks substituting a monospace face for
// ordinary running text. See this task's report for the cost this pays in
// the one case it is NOT free: a genuinely fixed-pitch font shared between
// body and code (unlikely, and not this package's own default) loses its
// fixed-pitch hint for the code role.
func fontTableXML(f fontOptions) string {
	roles := fontTableRoles(f)

	order := make([]string, 0, len(roles))
	pitchOf := make(map[string]string, len(roles))
	bucketOf := make(map[string]string, len(roles))
	for _, r := range roles {
		p := rolePitchOrVerified(r.name, r.pitch)
		if _, ok := pitchOf[r.name]; !ok {
			order = append(order, r.name)
			bucketOf[r.name] = r.bucket
			pitchOf[r.name] = p
			continue
		}
		if p != pitchOf[r.name] {
			// A real conflict: either an unverified font is doing double
			// duty as both a body and a code font, or (impossible in
			// practice, since a verified pitch never varies by role) two
			// verified claims disagree. Either way, "variable" is the
			// lower-harm resolution -- see this function's own doc comment.
			pitchOf[r.name] = "variable"
		}
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<w:fonts xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">`)
	for _, name := range order {
		b.WriteString(fontEntryXML(name, pitchOf[name], bucketOf[name]))
	}
	b.WriteString(`</w:fonts>`)
	return b.String()
}

// fontEntryXML renders one <w:font> element for name. When name is not in
// knownFontMetadata (a custom font a WriteOptions caller supplied that this
// package has no verified metadata for), it still gets a complete,
// non-empty entry rather than being skipped: charset/family fall back to a
// generic, honestly-labeled guess (bucket-appropriate charset, family
// "auto", no altName) instead of either omitting the font entirely (worse:
// Word would then have exactly the same zero-information substitution this
// whole part exists to improve on) or inventing metadata this package has
// not actually verified (worse: a specific-looking but false claim). altName
// is never fabricated for an unrecognized name -- see fontMeta's doc
// comment on what altName actually asserts.
//
// CT_Font's child sequence is altName?, panose1?, charset?, family?,
// notTrueType?, pitch?, sig? -- all optional per schema, which is why this
// package can validly omit panose1/notTrueType/sig (it has no real data for
// any of the three for a font it cannot inspect) while still emitting a
// schema-valid entry in altName-before-charset-before-family-before-pitch
// order.
func fontEntryXML(name, pitch, bucket string) string {
	meta, known := knownFontMetadata[name]
	charset, family, altName := meta.charset, meta.family, meta.altName
	if !known {
		family = "auto"
		charset = "00"
		if bucket == "eastAsia" {
			charset = "86"
		}
	}

	var b strings.Builder
	b.WriteString(`<w:font w:name="` + name + `">`)
	if altName != "" {
		b.WriteString(`<w:altName w:val="` + altName + `"/>`)
	}
	b.WriteString(`<w:charset w:val="` + charset + `"/>`)
	b.WriteString(`<w:family w:val="` + family + `"/>`)
	b.WriteString(`<w:pitch w:val="` + pitch + `"/>`)
	b.WriteString(`</w:font>`)
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
