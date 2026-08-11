package docx

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Span is a half-open byte range [Start, End) into the raw document.xml.
type Span struct {
	Start int
	End   int
}

// Run is one <w:t> text node together with the byte ranges needed to patch
// it in place.
type Run struct {
	// Index is the 1-based position of this run within its paragraph.
	Index int
	// Text is the DECODED text. Its character offsets do NOT map linearly
	// onto Content's byte offsets, because entities and character
	// references decode to a different length. Never derive byte offsets
	// from Text — replace the whole Content span instead.
	Text string
	// Content is the raw byte range between <w:t> and </w:t>.
	Content Span
	// Start is the byte range of the <w:t> start tag itself, so callers can
	// rewrite it to add xml:space="preserve".
	Start Span
	// HasPreserve reports whether the start tag already carries
	// xml:space="preserve".
	HasPreserve bool
	// SelfClosing reports whether this run's <w:t/> is self-closing rather
	// than a <w:t>...</w:t> pair. encoding/xml emits the StartElement and
	// EndElement for a self-closing tag at the SAME input offset (no
	// CharData token between them), so Content ends up as a zero-length
	// span sitting right after the "/>" — inside <w:r> but outside any
	// <w:t> content model. Patching such a run would splice character data
	// directly into <w:r>, which Word reports as unreadable content, so
	// callers must check this before building a Patch for the run.
	SelfClosing bool
	// Elem is the byte range of the whole <w:r> element that produced this
	// run, start tag through end tag. §4.2's run-level delete needs this to
	// remove the entire run (including <w:rPr> formatting) rather than
	// leaving an empty <w:r></w:r> behind. If a single <w:r> contains
	// multiple <w:t> children, every run it produces shares the same Elem
	// span.
	Elem Span
	// InInsertion reports whether this run sits inside a <w:ins> (tracked
	// insertion) element. §4.1's revision policy needs to distinguish
	// already-inserted text from plain text even though both are visible.
	InInsertion bool
}

// Para is one <w:p> element and the visible-text runs inside it.
type Para struct {
	// Index is the 1-based position of this paragraph in the linear
	// document order, table paragraphs included.
	Index int
	Runs  []Run
	// Span is the byte range of the whole <w:p> element, start tag through
	// end tag.
	Span Span
	// InTable reports whether this paragraph lives inside a <w:tbl>.
	InTable bool
	// Style is the <w:pPr><w:pStyle w:val="..."/> value, or "" if the
	// paragraph has no <w:pPr> or no <w:pStyle>. §4.1's heading outline is
	// built from this.
	Style string
	// Cell is non-nil when this paragraph lives inside a table cell, giving
	// its 1-based table/row/column coordinates per §4.1. It is nil for
	// paragraphs outside any table.
	Cell *CellRef
	// HasRevisions reports whether this paragraph contains a <w:ins> or
	// <w:del> element, so callers can apply §4.1's P1 revision policy
	// without re-scanning bytes themselves.
	HasRevisions bool
	// Breaks lists, in ascending order, the 1-based Run.Index values after
	// which a <w:br/> occurs. A <w:br/> is never appended to Run.Text —
	// doing so would break the invariant that Text is exactly the decoding
	// of Content — so P1b's markdown renderer consults Breaks to insert
	// line breaks between runs.
	Breaks []int
	// SkippedTextBox reports that a <w:txbxContent> subtree inside this
	// paragraph was skipped, so the paragraph's runs do not cover all text a
	// reader sees. docx_read must say so in its output rather than silently
	// presenting a partial paragraph.
	SkippedTextBox bool
}

// tableFrame is Scan's per-open-<w:tbl> bookkeeping: table is the
// document-wide index this table was assigned when it opened (see
// tblIndex), and row/col are the 1-based coordinates of the most recently
// entered <w:tr>/<w:tc> within THIS table specifically. A stack of these,
// one pushed per open <w:tbl> and popped on its close, is what keeps a
// nested table from drifting the enclosing table's coordinates.
type tableFrame struct {
	table int
	row   int
	col   int
}

// CellRef locates a table cell by 1-based coordinates: the table's position
// among all tables in the document, and the cell's row/column within that
// table.
type CellRef struct {
	// Table is the 1-based index of the <w:tbl> among all tables in the
	// document.
	Table int
	// Row is the 1-based index of the <w:tr> within its table.
	Row int
	// Col is the 1-based index of the <w:tc> within its row.
	Col int
}

// Scan indexes document.xml without building a DOM. It walks tokens with
// encoding/xml purely as a scanner, recording byte offsets from
// Decoder.InputOffset so edits can splice the original bytes directly.
//
// Visible-text rules: <w:delText> (already-deleted revision text) is
// skipped, while runs inside <w:ins> are included, matching what a reader
// sees in Word. Runs are found recursively, because <w:r> is not always a
// direct child of <w:p> — <w:hyperlink>, <w:smartTag> and <w:ins> all nest
// it one level deeper.
//
// <w:txbxContent> (text-box body) subtrees are skipped entirely: none of
// their paragraphs are indexed and none of their runs are collected. Word's
// real serializer duplicates the same text into <mc:Choice> and
// <mc:Fallback>, so indexing it would make the text appear twice in the
// linear paragraph order — which would make callers that reject a
// multiply-matched find/replace refuse to edit any paragraph containing a
// text box. The containing paragraph is marked Para.SkippedTextBox instead.
func Scan(documentXML []byte) ([]Para, error) {
	dec := xml.NewDecoder(bytes.NewReader(documentXML))

	var (
		paras []Para
		// prevOffset trails InputOffset by one token. InputOffset reports
		// the END of the token just returned, so the start of an element's
		// content is the offset taken right after its StartElement, and the
		// end of that content is the offset recorded BEFORE its EndElement
		// was consumed — that is, prevOffset.
		prevOffset int
		tableDepth int

		inPara      bool
		paraDepth   int
		paraStart   int
		paraRuns    []Run
		paraInTable bool
		// paraStyle, paraCell, paraHasRevisions, paraBreaks and
		// paraSkippedTextBox accumulate the new per-paragraph metadata the
		// same way paraRuns/paraInTable already do: reset when a paragraph
		// opens (paraDepth 0 -> 1), consumed into the Para literal when it
		// closes (paraDepth 1 -> 0).
		paraStyle          string
		paraCell           *CellRef
		paraHasRevisions   bool
		paraBreaks         []int
		paraSkippedTextBox bool

		inText          bool
		textStart       int
		textTagSpan     Span
		textPreserve    bool
		textSelfClosing bool
		textBuf         strings.Builder

		// tblIndex is a document-wide 1-based table counter: it only ever
		// increments, on every <w:tbl> enter, so a table nested inside a
		// cell of an earlier table still gets the next number (e.g. the
		// inner table of the first nested pair is table 2), never reusing or
		// resetting.
		//
		// tableStack holds one frame per currently-open <w:tbl>, innermost
		// last. Row/col coordinates live on the frame, not in scalars,
		// because a <w:tbl> nested inside a <w:tc> (common in real
		// documents — layout tables, a table inside a cell) must not drift
		// the enclosing table's row/col: pushing a new frame on entry and
		// popping it on exit means the outer table's counters are exactly as
		// they were before the nested table opened, once the nested table's
		// frame is gone. Para.Cell always reads the top frame while
		// tableDepth > 0.
		tblIndex   int
		tableStack []tableFrame

		// runStart/runRunsStart bracket the current <w:r> the same way
		// paraStart/paraRuns bracket the current <w:p>: runStart is the
		// offset right after <w:r> opens (via prevOffset), and
		// runRunsStart is how many runs paraRuns already held at that
		// point. A <w:r> may contain zero or more than one <w:t>, so on
		// </w:r> every run appended since runRunsStart (i.e.
		// paraRuns[runRunsStart:]) gets the same Elem span.
		runStart     int
		runRunsStart int

		// insDepth counts nested <w:ins> (tracked-insertion) elements; a run
		// created while insDepth > 0 is marked InInsertion. pPrDepth counts
		// nested <w:pPr> elements, gating <w:pStyle> capture to only inside
		// a paragraph's own properties block.
		insDepth int
		pPrDepth int

		// txbxDepth counts nested <w:txbxContent> elements. While it is > 0,
		// every case below that would mutate paragraph/table/run/style/
		// revision/break state is skipped (guarded by "&& txbxDepth == 0"),
		// so the subtree's content never reaches paras, paraRuns, or any of
		// the paragraph metadata above.
		txbxDepth int
	)

	for {
		tok, err := dec.Token()
		if err != nil {
			// io.EOF is the only clean termination; anything else is a real
			// parse failure.
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("scan document.xml: %w", err)
		}
		offset := int(dec.InputOffset())

		switch t := tok.(type) {
		case xml.StartElement:
			switch {
			case isWordElement(t.Name, "tbl") && txbxDepth == 0:
				tableDepth++
				tblIndex++
				tableStack = append(tableStack, tableFrame{table: tblIndex})
			case isWordElement(t.Name, "tr") && txbxDepth == 0:
				if n := len(tableStack); n > 0 {
					tableStack[n-1].row++
					tableStack[n-1].col = 0
				}
			case isWordElement(t.Name, "tc") && txbxDepth == 0:
				if n := len(tableStack); n > 0 {
					tableStack[n-1].col++
				}
			case isWordElement(t.Name, "txbxContent"):
				txbxDepth++
				// The whole subtree is skipped, so the containing paragraph
				// (still open; its own <w:p>/<w:r> already passed through
				// the cases above) must say so.
				if inPara {
					paraSkippedTextBox = true
				}
			case isWordElement(t.Name, "p") && txbxDepth == 0:
				// Depth-counted, not a flat flag: a literally nested <w:p>
				// (malformed, but seen in the wild) must not reset
				// paraStart/paraRuns out from under the paragraph that is
				// really open. Only the transition 0 -> 1 starts capture;
				// the matching transition 1 -> 0 in the EndElement case
				// below is what actually closes it. Namespace filtering
				// (isWordElement) is what keeps DrawingML's <a:p> — which
				// shares the local name "p" — from ever reaching this case
				// at all.
				paraDepth++
				if paraDepth == 1 {
					inPara = true
					// prevOffset is the end of the previous token, which is
					// exactly where this <w:p> start tag begins.
					paraStart = prevOffset
					paraRuns = nil
					paraInTable = tableDepth > 0
					paraStyle = ""
					paraHasRevisions = false
					paraBreaks = nil
					paraSkippedTextBox = false
					if tableDepth > 0 && len(tableStack) > 0 {
						top := tableStack[len(tableStack)-1]
						paraCell = &CellRef{Table: top.table, Row: top.row, Col: top.col}
					} else {
						paraCell = nil
					}
				}
			case isWordElement(t.Name, "pPr") && txbxDepth == 0:
				pPrDepth++
			case isWordElement(t.Name, "pStyle") && txbxDepth == 0:
				if inPara && pPrDepth > 0 {
					if val, ok := wordAttrVal(t, "val"); ok {
						paraStyle = val
					}
				}
			case isWordElement(t.Name, "r") && txbxDepth == 0:
				// prevOffset is exactly where this <w:r> start tag begins,
				// the same reasoning as paraStart above.
				runStart = prevOffset
				runRunsStart = len(paraRuns)
			case isWordElement(t.Name, "ins") && txbxDepth == 0:
				insDepth++
				if inPara {
					paraHasRevisions = true
				}
			case isWordElement(t.Name, "del") && txbxDepth == 0:
				// <w:del> does not affect insDepth/InInsertion: its content
				// is <w:delText>, never indexed as a run (see the "delText"
				// note below), so there is no run to mark either way.
				if inPara {
					paraHasRevisions = true
				}
			case isWordElement(t.Name, "br") && txbxDepth == 0:
				if inPara {
					// The run count so far IS the 1-based index of the run
					// most recently appended (Run.Index == len(paraRuns) at
					// the moment the NEXT run would become len+1), so this
					// records "a break after run number len(paraRuns)".
					paraBreaks = append(paraBreaks, len(paraRuns))
				}
			case isWordElement(t.Name, "t") && txbxDepth == 0:
				if inPara {
					inText = true
					textStart = offset
					textTagSpan = Span{Start: prevOffset, End: offset}
					textPreserve = hasPreserveAttr(t)
					// A self-closing <w:t/> has its whole tag, including the
					// closing "/>", already consumed into this StartElement
					// token, so textTagSpan (== the raw bytes documentXML
					// [prevOffset:offset]) ends in "/>" precisely when the
					// tag is self-closing. An ordinary "<w:t ...>" or
					// "<w:t>" start tag always ends in a bare '>'.
					textSelfClosing = offset-prevOffset >= 2 &&
						string(documentXML[offset-2:offset]) == "/>"
					textBuf.Reset()
				}
				// No case for "delText": <w:delText> holds its deleted text
				// directly as character data (it has no nested <w:t>), and
				// inText is only ever set by the "t" case above, so its content
				// is excluded automatically. No state change is needed here.
			}

		case xml.CharData:
			if inText {
				textBuf.Write(t)
			}

		case xml.EndElement:
			switch {
			case isWordElement(t.Name, "tbl") && txbxDepth == 0:
				if tableDepth > 0 {
					tableDepth--
				}
				if n := len(tableStack); n > 0 {
					tableStack = tableStack[:n-1]
				}
			case isWordElement(t.Name, "txbxContent"):
				if txbxDepth > 0 {
					txbxDepth--
				}
			case isWordElement(t.Name, "pPr") && txbxDepth == 0:
				if pPrDepth > 0 {
					pPrDepth--
				}
			case isWordElement(t.Name, "ins") && txbxDepth == 0:
				if insDepth > 0 {
					insDepth--
				}
			case isWordElement(t.Name, "t") && txbxDepth == 0:
				if inText {
					paraRuns = append(paraRuns, Run{
						Index:       len(paraRuns) + 1,
						Text:        textBuf.String(),
						Content:     Span{Start: textStart, End: prevOffset},
						Start:       textTagSpan,
						HasPreserve: textPreserve,
						SelfClosing: textSelfClosing,
						InInsertion: insDepth > 0,
					})
					inText = false
				}
			case isWordElement(t.Name, "r") && txbxDepth == 0:
				runElem := Span{Start: runStart, End: offset}
				for i := runRunsStart; i < len(paraRuns); i++ {
					paraRuns[i].Elem = runElem
				}
			case isWordElement(t.Name, "p") && txbxDepth == 0:
				// Symmetric with the StartElement case: only the 1 -> 0
				// transition closes the paragraph, so a stray or
				// nested close (or a namespace-mismatched one, which
				// isWordElement already excludes) cannot end a paragraph
				// that a different element opened.
				if paraDepth > 0 {
					paraDepth--
					if paraDepth == 0 && inPara {
						paras = append(paras, Para{
							Index:          len(paras) + 1,
							Runs:           paraRuns,
							Span:           Span{Start: paraStart, End: offset},
							InTable:        paraInTable,
							Style:          paraStyle,
							Cell:           paraCell,
							HasRevisions:   paraHasRevisions,
							Breaks:         paraBreaks,
							SkippedTextBox: paraSkippedTextBox,
						})
						inPara = false
						paraRuns = nil
					}
				}
			}
		}
		prevOffset = offset
	}
	return paras, nil
}

// isWordprocessingMLNamespace reports whether space is one Go's
// encoding/xml would resolve a WordprocessingML "w:" prefix to. Go resolves
// Name.Space to the full declared URI when the prefix has an xmlns
// declaration in scope, but falls back to the raw prefix string itself
// ("w") when it is undeclared, and to "" when there is no prefix at all.
// All three are accepted here: real-world documents always declare the
// namespace, but hand-written test XML and some sloppy real files don't.
// Anything else — notably DrawingML's URI, which shares local names like
// "p" and "t" with WordprocessingML inside embedded shapes/text boxes — is
// namespace-blind matching's failure mode and must be excluded.
func isWordprocessingMLNamespace(space string) bool {
	switch space {
	case "", "w", "http://schemas.openxmlformats.org/wordprocessingml/2006/main":
		return true
	default:
		return false
	}
}

// isWordElement reports whether n is the WordprocessingML element named
// local, as opposed to some other vocabulary's element that happens to
// share the same local name (see isWordprocessingMLNamespace).
func isWordElement(n xml.Name, local string) bool {
	return n.Local == local && isWordprocessingMLNamespace(n.Space)
}

// isXMLNamespace reports whether space is one Go's encoding/xml would
// resolve the predefined "xml:" prefix to, with the same undeclared-prefix
// and no-prefix tolerance as isWordprocessingMLNamespace.
func isXMLNamespace(space string) bool {
	switch space {
	case "", "xml", "http://www.w3.org/XML/1998/namespace":
		return true
	default:
		return false
	}
}

// hasPreserveAttr reports whether a <w:t> start tag carries
// xml:space="preserve", which tells Word to keep leading and trailing
// whitespace.
func hasPreserveAttr(t xml.StartElement) bool {
	for _, a := range t.Attr {
		if a.Name.Local == "space" && a.Value == "preserve" && isXMLNamespace(a.Name.Space) {
			return true
		}
	}
	return false
}

// wordAttrVal reports the value of t's WordprocessingML attribute named
// local (e.g. "val" for w:val), with the same namespace tolerance as
// isWordElement: real documents write it as "w:val", but undeclared or
// unprefixed forms are accepted too.
func wordAttrVal(t xml.StartElement, local string) (string, bool) {
	for _, a := range t.Attr {
		if a.Name.Local == local && isWordprocessingMLNamespace(a.Name.Space) {
			return a.Value, true
		}
	}
	return "", false
}
