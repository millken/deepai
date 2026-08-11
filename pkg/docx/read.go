package docx

import (
	"fmt"
	"regexp"
	"strings"
)

// DocxOutlineParaThreshold is the paragraph-count threshold above which a
// caller should prefer Outline() plus chunked reading (a later task) over
// pulling every paragraph at once.
const DocxOutlineParaThreshold = 200

// Section is one contiguous run of paragraphs in the document's linear
// order: either the content introduced by a single heading paragraph, or —
// for Level 0 — a stretch of body text that precedes the first heading (or
// the whole document, if it has no headings at all).
type Section struct {
	// Heading is the heading paragraph's text, or "" for the unnamed section
	// that covers content before the first heading.
	Heading string
	// Style is the heading paragraph's Para.Style (e.g. "Heading1"), or ""
	// for the unnamed section.
	Style string
	// Level is the number parsed from the end of Style, or 0 for the
	// unnamed section.
	Level int
	// StartPara and EndPara are 1-based, inclusive paragraph indexes (see
	// Para.Index). StartPara is the heading paragraph itself when Heading
	// != "".
	StartPara int
	EndPara   int
	// Paras is EndPara - StartPara + 1.
	Paras int
	// Words is this section's word count, on the same accounting as
	// Outline.Words.
	Words int
}

// Outline is a heading-based summary of a document: every paragraph tiled
// into exactly one Section, in document order.
type Outline struct {
	TotalParas int
	// Words is the document's total word count, counted by strings.Fields
	// on each paragraph's concatenated run text. This is a whitespace split,
	// not a character count: for languages that don't separate words with
	// spaces (e.g. Chinese), it degrades to counting whitespace-delimited
	// runs of text rather than actual words. That is accepted for P1.
	Words    int
	Sections []Section
	// Notes carries Document.Notes() through, so a caller consulting only
	// the outline still sees what content was omitted.
	Notes []string
}

// Outline computes a heading-based outline of the document. Heading
// detection is deliberately narrow: a paragraph is a heading only if its
// Style starts with "Heading" (case-insensitive) followed immediately by
// exactly one ASCII digit 1-9, and Level is that digit. §4.1 only ever
// needs Heading1-9, so anything else — "Heading0" (which would collide with
// the unnamed-section sentinel Level 0), "Heading10"+, "HeadingFoo", or a
// pathological run of digits — is NOT a heading; see headingLevel. Other
// styles that Word treats visually as headings (e.g. "Title", "Subtitle",
// "Caption") are NOT treated as headings here either — misclassifying them
// would scramble the section structure, whereas letting them fall into the
// surrounding body section is comparatively harmless.
//
// Sections tile the document exactly: every paragraph belongs to exactly
// one section, with no gaps and no overlaps. If the document has content
// before its first heading (or no headings at all), that content forms a
// leading unnamed (Heading: "", Level: 0) section. A heading paragraph
// itself belongs to the section it introduces (StartPara points at it).
func (d *Document) Outline() Outline {
	return buildOutline(d.paras, d.Notes())
}

// buildOutline is the pure core of Outline. It is separated from the method
// so the sectioning rules can be tested against synthetic paragraph shapes
// that no committed fixture happens to contain — notably a document with
// body text BEFORE its first heading, which neither structure.docx (no
// headings at all) nor outline.docx (starts with a heading) exercises.
func buildOutline(paras []Para, notes []string) Outline {
	var sections []Section
	cur := -1 // index into sections of the section currently being built
	total := 0

	for _, p := range paras {
		text := outlineParaText(p)
		words := len(strings.Fields(text))
		total += words

		if level, ok := headingLevel(p.Style); ok {
			sections = append(sections, Section{
				Heading:   text,
				Style:     p.Style,
				Level:     level,
				StartPara: p.Index,
				EndPara:   p.Index,
				Paras:     1,
				Words:     words,
			})
			cur = len(sections) - 1
			continue
		}

		if cur == -1 {
			sections = append(sections, Section{
				StartPara: p.Index,
				EndPara:   p.Index,
				Paras:     1,
				Words:     words,
			})
			cur = len(sections) - 1
			continue
		}

		sections[cur].EndPara = p.Index
		sections[cur].Paras++
		sections[cur].Words += words
	}

	return Outline{
		TotalParas: len(paras),
		Words:      total,
		Sections:   sections,
		Notes:      notes,
	}
}

// outlineParaText returns p's text as the concatenation of its runs' decoded Text,
// in run order. It does not represent line breaks (see Para.Breaks) since
// those are never appended to a Run's Text either.
func outlineParaText(p Para) string {
	var b strings.Builder
	for _, r := range p.Runs {
		b.WriteString(r.Text)
	}
	return b.String()
}

// headingLevel reports whether style is a heading style — "Heading"
// (case-insensitive) followed by EXACTLY ONE ASCII digit 1-9 — and if so,
// that digit as Level. Anything else (including "Heading" alone, "Heading "
// with a space, "HeadingFoo", "Heading0", or a run of more than one digit)
// is not a heading.
//
// The single-digit restriction is deliberate, not an oversight: §4.1 never
// needs Heading10+, and the original unbounded "one or more digits" parse
// (n = n*10 + digit, with no length or magnitude check) let a
// caller-controlled w:pStyle value like "Heading99999999999999999999" or
// "Heading12345678901234567890" overflow int and panic
// strings.Repeat("#", level) in renderReadPara — or, for something merely
// large like "Heading100000", produce a 100KB rendered block for a
// one-character paragraph. Rejecting anything but a single 1-9 digit closes
// all three: the level Read ever multiplies into strings.Repeat is bounded
// to [1,9] by construction. "Heading0" is rejected too, since Level 0 is
// the sentinel buildOutline already uses for the unnamed leading section.
func headingLevel(style string) (int, bool) {
	const prefix = "heading"
	if len(style) <= len(prefix) || !strings.EqualFold(style[:len(prefix)], prefix) {
		return 0, false
	}
	rest := style[len(prefix):]
	if len(rest) != 1 || rest[0] < '1' || rest[0] > '9' {
		return 0, false
	}
	return int(rest[0] - '0'), true
}

// DefaultReadBudget is the render-body-character budget Read applies to a
// Full read when ReadOptions.MaxChars is left at 0. It matches design §5.1's
// recommendation to stay far below the 24KB tool-result offload threshold
// that deepai applies with no error and no visible warning.
const DefaultReadBudget = 8192

// tableStructureNote is appended to ReadResult.Notes whenever a returned
// paragraph carries table-cell coordinates, per §4.1/§5.2: P1 renders each
// cell's paragraphs individually rather than reconstructing a markdown
// table, and a caller must be told so rather than mistaking the flattened
// cells for prose.
const tableStructureNote = "tables are rendered cell-by-cell as individual paragraphs; table structure (rows and columns) is not reconstructed into markdown"

// ReadOptions selects what Read returns and how it is chunked. Heading is
// mutually exclusive with StartPara/EndPara: combining them is an error
// rather than a guess at which one wins.
type ReadOptions struct {
	StartPara int    // 1-based inclusive; 0 = from the start
	EndPara   int    // 1-based inclusive; 0 = to the end
	Heading   string // if non-empty, scopes the read to that section
	Runs      bool   // include each paragraph's per-run breakdown
	MaxChars  int    // rendered-body character budget; 0 = unlimited
	Full      bool   // whole selection at once; errors instead of degrading over budget
}

// RunView is one paragraph's run, exposed only when ReadOptions.Runs is set.
type RunView struct {
	Index int
	Text  string
}

// ParaView is one paragraph as Read renders it: enough to display and, via
// Index, to anchor a later docx_edit call.
type ParaView struct {
	Index int
	Text  string
	Style string
	Cell  *CellRef
	// Runs is populated only when ReadOptions.Runs is true.
	Runs []RunView
	// Note carries a per-paragraph caveat, e.g. that a text box inside this
	// paragraph was not shown. "" means no caveat.
	Note string
}

// ReadResult is one call's worth of rendered document content plus the
// cursor a caller repeats the call with to continue past it.
type ReadResult struct {
	Markdown string
	Paras    []ParaView
	// NextStartPara is the index of the first paragraph NOT returned, i.e.
	// where a caller resumes with StartPara: NextStartPara. It is set only
	// when MaxChars forced this call to stop short of the requested range;
	// 0 means the requested range (or, with no range given, the whole
	// document) was returned in full.
	NextStartPara int
	// RangeStart and RangeEnd are the resolved, 1-based inclusive paragraph
	// bounds this call selected: the named Heading section's bounds when
	// Heading was given, or StartPara/EndPara with their 0-means-open-ended
	// defaults already applied otherwise. They are set on every non-error
	// return, including the "nothing to read" case.
	//
	// A caller resuming a heading-scoped chunked read MUST carry RangeEnd
	// forward explicitly: Heading and StartPara are mutually exclusive, so
	// the natural-looking Read{StartPara: NextStartPara} on its own would
	// run open-ended past this section into whatever follows it. Resume
	// with Read{StartPara: NextStartPara, EndPara: RangeEnd} instead — that
	// stays inside the same section until NextStartPara is finally 0.
	RangeStart int
	RangeEnd   int
	TotalParas int
	Notes      []string
}

// Read renders a selection of the document's paragraphs to markdown,
// chunking by ReadOptions.MaxChars so a caller can walk an arbitrarily large
// document without ever asking for more than a bounded number of rendered
// characters at once (design §5.2).
//
// Selection is either a StartPara/EndPara range (both 1-based inclusive, 0
// meaning "from the start" / "to the end") or a Heading naming one of
// Outline()'s sections — never both. A StartPara past the end of the
// document is not an error: it returns an empty ReadResult with
// NextStartPara 0, matching "nothing left to read" rather than "invalid
// request".
//
// Chunking cuts only at paragraph boundaries: Read accumulates each
// paragraph's rendered markdown block and stops BEFORE adding one that would
// push the running total over MaxChars, reporting that paragraph's index as
// NextStartPara. The one exception is a paragraph whose own rendered block
// already exceeds MaxChars: returning nothing would leave the cursor
// pointing at the same paragraph forever, so that paragraph is returned
// alone and the overage is recorded in Notes.
//
// MaxChars is measured against the rendered markdown Read actually
// produces for each paragraph — the "[para N]" marker, heading "#"
// prefix, and table-cell annotation included, not just the paragraph's bare
// text — because that rendered output is what a caller receives and what
// risks tripping deepai's 24KB tool-result offload (design §5.1).
//
// Full changes the chunking behavior rather than skipping it: it renders
// the whole selected range and, if that exceeds MaxChars (or
// DefaultReadBudget when MaxChars is 0), returns an error instead of a
// truncated result — silently dropping the back half of a document is
// exactly what design §5.1 says this package must never do. A non-Full call
// never errors on size; it just chunks. MaxChars <= 0 does NOT mean
// unlimited on this path either: it defaults to DefaultReadBudget, same as
// Full. Full is the only way to ask for the whole selection in one call
// (and even then only within budget).
//
// Resuming a heading-scoped chunked read (ReadResult.NextStartPara != 0 from
// a call with Heading set) must not simply repeat Heading — Heading and
// StartPara are mutually exclusive by construction, precisely because a
// second Read{Heading: X} call would silently re-resolve X's FULL section
// again instead of continuing from the cursor. Resume with
// Read{StartPara: NextStartPara, EndPara: RangeEnd}, using the RangeEnd this
// same call reported, so the walk stays inside the original section instead
// of running open-ended into whatever follows it.
func (d *Document) Read(opts ReadOptions) (ReadResult, error) {
	if opts.Heading != "" && (opts.StartPara != 0 || opts.EndPara != 0) {
		return ReadResult{}, fmt.Errorf("docx: Heading and StartPara/EndPara are mutually exclusive")
	}

	total := d.TotalParas()
	start, end, err := d.resolveReadRange(opts, total)
	if err != nil {
		return ReadResult{}, err
	}
	if end > total {
		end = total
	}

	notes := d.Notes()
	if start < 1 || start > total || start > end {
		// Nothing to read: past the end, or an empty/inverted range.
		return ReadResult{TotalParas: total, Notes: notes, RangeStart: start, RangeEnd: end}, nil
	}

	all := d.paras

	if opts.Full {
		budget := opts.MaxChars
		if budget <= 0 {
			budget = DefaultReadBudget
		}
		var md strings.Builder
		views := make([]ParaView, 0, end-start+1)
		hasCell := false
		for i := start; i <= end; i++ {
			pv, block := renderReadPara(all[i-1], opts.Runs)
			md.WriteString(block)
			views = append(views, pv)
			if pv.Cell != nil {
				hasCell = true
			}
		}
		if md.Len() > budget {
			return ReadResult{}, fmt.Errorf(
				"docx: full read of paragraphs %d-%d is %d rendered chars, over the %d-char budget; "+
					"read the outline (Document.Outline) and fetch a StartPara/EndPara range or Heading section instead",
				start, end, md.Len(), budget)
		}
		if hasCell {
			notes = append(notes, tableStructureNote)
		}
		return ReadResult{
			Markdown:      md.String(),
			Paras:         views,
			NextStartPara: 0,
			RangeStart:    start,
			RangeEnd:      end,
			TotalParas:    total,
			Notes:         notes,
		}, nil
	}

	// MaxChars <= 0 does not mean unlimited here either — only Full offers
	// that escape hatch (and even then only within budget, or it refuses).
	// A caller that passes max_chars straight through from a JSON tool
	// schema and simply omits it must not get the entire document back.
	budget := opts.MaxChars
	if budget <= 0 {
		budget = DefaultReadBudget
	}
	var (
		md      strings.Builder
		views   []ParaView
		cum     int
		next    int
		hasCell bool
	)
	for i := start; i <= end; i++ {
		p := all[i-1]
		pv, block := renderReadPara(p, opts.Runs)
		blockLen := len(block)

		if cum+blockLen > budget {
			if len(views) == 0 {
				// This one paragraph alone is over budget. Returning
				// nothing would never advance the cursor (the next call
				// would be handed the very same StartPara), so take it
				// whole and note the overage instead.
				notes = append(notes, fmt.Sprintf(
					"paragraph %d is %d rendered chars, exceeding the %d-char MaxChars budget; returned whole so the read cursor still advances",
					p.Index, blockLen, budget))
				md.WriteString(block)
				views = append(views, pv)
				if pv.Cell != nil {
					hasCell = true
				}
				if i < end {
					next = i + 1
				}
			} else {
				// Adding this paragraph would cross the budget and this
				// chunk already has content: stop before the boundary.
				next = i
			}
			break
		}

		md.WriteString(block)
		views = append(views, pv)
		if pv.Cell != nil {
			hasCell = true
		}
		cum += blockLen
	}
	if hasCell {
		notes = append(notes, tableStructureNote)
	}

	return ReadResult{
		Markdown:      md.String(),
		Paras:         views,
		NextStartPara: next,
		RangeStart:    start,
		RangeEnd:      end,
		TotalParas:    total,
		Notes:         notes,
	}, nil
}

// resolveReadRange turns opts into a concrete 1-based inclusive [start, end]
// paragraph range: either the bounds of the named Heading's section, or
// StartPara/EndPara with their 0-means-open-ended defaults applied. It is
// the only place that consults Outline(), so an unknown heading is reported
// here as an error rather than silently returning no paragraphs.
//
// A Heading that matches more than one section is refused rather than
// resolved to the first match: two Heading1 "Intro" sections would
// otherwise make the second permanently unreachable through Heading (every
// call would silently resolve to the first one's range), which is exactly
// the kind of silent content loss §5.1 exists to prevent. The refusal names
// how many sections matched and points the caller at StartPara/EndPara,
// which can always disambiguate by paragraph index.
func (d *Document) resolveReadRange(opts ReadOptions, total int) (start, end int, err error) {
	if opts.Heading != "" {
		var matches []Section
		for _, s := range d.Outline().Sections {
			if s.Heading == opts.Heading {
				matches = append(matches, s)
			}
		}
		switch len(matches) {
		case 0:
			return 0, 0, fmt.Errorf("docx: unknown heading %q", opts.Heading)
		case 1:
			return matches[0].StartPara, matches[0].EndPara, nil
		default:
			return 0, 0, fmt.Errorf(
				"docx: heading %q matches %d sections; it is ambiguous which one to read — use start_para/end_para to pick one instead",
				opts.Heading, len(matches))
		}
	}

	if opts.StartPara < 0 {
		return 0, 0, fmt.Errorf("docx: StartPara must not be negative (got %d)", opts.StartPara)
	}
	if opts.EndPara < 0 {
		return 0, 0, fmt.Errorf("docx: EndPara must not be negative (got %d)", opts.EndPara)
	}

	start = opts.StartPara
	if start <= 0 {
		start = 1
	}
	end = opts.EndPara
	if end <= 0 {
		end = total
	}
	return start, end, nil
}

// paraTextWithBreaks is p's run text concatenated in order, with a "\n"
// inserted after every run index listed in p.Breaks. Unlike
// outlineParaText/ParaView.Text, this represents <w:br/> line breaks, since
// markdown rendering (unlike the plain-text Text field) needs to show them.
func paraTextWithBreaks(p Para) string {
	if len(p.Breaks) == 0 {
		return outlineParaText(p)
	}
	breakAfter := make(map[int]bool, len(p.Breaks))
	for _, idx := range p.Breaks {
		breakAfter[idx] = true
	}
	var b strings.Builder
	for _, r := range p.Runs {
		b.WriteString(r.Text)
		if breakAfter[r.Index] {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// paraMarkerPattern matches the exact "[para N]" shape Read's own citation
// markers use. neutralizeParaMarkers uses it to find and defuse look-alike
// sequences that occur inside untrusted PARAGRAPH TEXT, as opposed to a
// marker Read itself inserts.
var paraMarkerPattern = regexp.MustCompile(`\[para \d+\]`)

// neutralizeParaMarkers breaks up any "[para N]" look-alike sequence in s by
// inserting a zero-width space (U+200B) right after the "[". Untrusted
// paragraph text is rendered interleaved with Read's own "[para N]"
// citation marker with no other separator, so a document whose visible text
// happens to contain the literal string "[para 7]" (a citation, a code
// sample, anything) would otherwise be byte-for-byte indistinguishable from
// a real marker once embedded in the markdown Read returns — an LLM told to
// treat "[para N]" as a trustworthy index anchor back to docx_edit could
// then cite the wrong paragraph. A zero-width space is invisible in any
// rendering, so the text still reads naturally to both a human and an LLM,
// but the exact byte sequence Read's marker uses never appears inside
// paragraph content, only in markers Read itself constructs.
func neutralizeParaMarkers(s string) string {
	if !strings.Contains(s, "[para ") {
		return s
	}
	zeroWidthSpace := string(rune(0x200B))
	return paraMarkerPattern.ReplaceAllStringFunc(s, func(m string) string {
		return "[" + zeroWidthSpace + m[len("["):]
	})
}

// renderReadPara builds p's ParaView and the markdown block Read appends
// for it. The block always carries a "[para N]" marker (so an LLM can cite
// para_index back to docx_edit); a heading paragraph (per headingLevel) gets
// a "#"-repeated prefix instead of the marker leading; a table-cell
// paragraph (p.Cell != nil) gets its coordinates annotated instead, since
// P1 does not reconstruct table structure (see tableStructureNote).
func renderReadPara(p Para, includeRuns bool) (ParaView, string) {
	pv := ParaView{
		Index: p.Index,
		Text:  outlineParaText(p),
		Style: p.Style,
		Cell:  p.Cell,
	}
	if p.SkippedTextBox {
		pv.Note = "contains a text box whose content is not shown"
	}
	if includeRuns {
		pv.Runs = make([]RunView, len(p.Runs))
		for i, r := range p.Runs {
			pv.Runs[i] = RunView{Index: r.Index, Text: r.Text}
		}
	}

	// neutralizeParaMarkers: rendered is untrusted paragraph text that goes
	// out interleaved with marker (below) with no other separator, so a
	// paragraph containing the literal text "[para 7]" must not be allowed
	// to look like a real marker to whatever reads this block.
	rendered := neutralizeParaMarkers(paraTextWithBreaks(p))
	marker := fmt.Sprintf("[para %d]", p.Index)

	var block string
	switch {
	case p.Cell != nil:
		block = fmt.Sprintf("%s (table %d row %d col %d) %s\n\n",
			marker, p.Cell.Table, p.Cell.Row, p.Cell.Col, rendered)
	default:
		if level, ok := headingLevel(p.Style); ok {
			block = fmt.Sprintf("%s %s %s\n\n", strings.Repeat("#", level), rendered, marker)
		} else {
			block = fmt.Sprintf("%s %s\n\n", marker, rendered)
		}
	}
	return pv, block
}
