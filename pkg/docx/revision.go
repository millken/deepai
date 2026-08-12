package docx

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// defaultRevisionAuthor is the w:author value a tracked change gets when its
// batch's EditOptions.Author is "" — see effectiveAuthor and attrs below.
const defaultRevisionAuthor = "deepai"

// effectiveAuthor returns author, trimmed of surrounding whitespace, or
// defaultRevisionAuthor when that trims to "". This is the single place
// that default lives, so both attrs (which stamps it onto every w:ins/w:del
// a TrackChanges batch produces) and edit.go's track_changes author gate
// (which must judge an UNTRACKED batch — one that never builds a
// revisionCtx at all — by the same identity a tracked one would have
// carried) agree on what "no author given" means. Trimming here, not just
// at the tool layer (pkg/tools/builtin/docx.go already trims author before
// it reaches EditOptions), makes the comparison correct for any caller of
// this package directly, and keeps it symmetric with scanRevisions, which
// trims every w:author value it reads off disk the same way — otherwise an
// author matching an existing revision except for incidental whitespace
// would manufacture a spurious "different author" refusal that trimming on
// only one side could not fix.
func effectiveAuthor(author string) string {
	author = strings.TrimSpace(author)
	if author == "" {
		return defaultRevisionAuthor
	}
	return author
}

// formatAuthorList renders a list of revision authors for a human/LLM
// facing message: comma-separated, or "(unnamed)" when empty. The empty
// case is reachable only when every revision element scanRevisions found
// had no w:author attribute at all (or one that was empty/whitespace-only)
// — malformed input, since Word always writes a real reviewer name, but not
// something a caller-facing message should render as a bare "[]".
func formatAuthorList(authors []string) string {
	if len(authors) == 0 {
		return "(unnamed)"
	}
	return strings.Join(authors, ", ")
}

// revisionSummary aggregates every revision-tracking element found anywhere
// in a document.xml — <w:ins>/<w:del> (run-level wraps and the self-closing
// paragraph-mark form alike), plus every other WordprocessingML element that
// carries its own w:author (w:moveFrom/w:moveTo for tracked text moves,
// w:cellIns/w:cellDel for tracked table-cell insert/delete, and
// w:rPrChange/w:pPrChange for tracked formatting changes) — into a count of
// <w:ins>/<w:del> specifically and the distinct w:author values ANY of them
// carry. Document computes this twice: once at OpenDocument time, to seed
// revisionAuthorsAtOpen (edit.go's track_changes author gate), and again on
// every rescan, to feed computeNotes' "this document has unreviewed
// revisions" declaration — see document.go.
//
// The author set intentionally covers more elements than the ins/del count
// does: missing an author here is a false-ALLOW hazard (the gate would let
// an edit past a reviewer's pending w:rPrChange, say, because it only ever
// heard about w:ins/w:del authors), whereas the ins/del count staying
// narrower only affects the read-side "N insertions/M deletions" note's
// wording, not any safety property. See task-3's review round for the
// concrete case (moveFrom/moveTo/cellIns/cellDel/rPrChange/pPrChange all
// carry w:author per their CT_TrackChange-family schemas, and none of them
// are w:ins/w:del themselves).
type revisionSummary struct {
	InsCount int
	DelCount int
	// Authors is sorted and deduplicated, and never contains "": a
	// revision element with no w:author attribute, or one that is empty or
	// whitespace-only (malformed input; Word always writes a real name),
	// contributes to the ins/del count but not to this list. Every value
	// here is also trimmed of surrounding whitespace (see effectiveAuthor's
	// doc comment for why both sides of the comparison need to agree on
	// that).
	Authors []string
}

// isRevisionAuthorElement reports whether n is one of the WordprocessingML
// elements that carries its own w:author identifying who made a tracked
// change: w:ins/w:del themselves, plus w:moveFrom/w:moveTo (tracked text
// moves), w:cellIns/w:cellDel (tracked table-cell insert/delete), and
// w:rPrChange/w:pPrChange (tracked formatting changes). scanRevisions uses
// this to decide which elements' w:author to collect; over-collecting
// (treating an element as a revision source when it turns out not to carry
// one) is harmless (wordAttrVal simply finds nothing), while
// under-collecting is the false-allow hazard the type's doc comment
// describes, so this list errs toward including any TrackChange-family
// element task-3's review named.
func isRevisionAuthorElement(n xml.Name) bool {
	switch {
	case isWordElement(n, "ins"),
		isWordElement(n, "del"),
		isWordElement(n, "moveFrom"),
		isWordElement(n, "moveTo"),
		isWordElement(n, "cellIns"),
		isWordElement(n, "cellDel"),
		isWordElement(n, "rPrChange"),
		isWordElement(n, "pPrChange"):
		return true
	default:
		return false
	}
}

// scanRevisions walks documentXML as a token stream (independent of Scan's
// paragraph indexing, and independent of any txbxDepth/inPara-style nesting
// guard, so it also sees a revision inside a text box or one wrapping a
// whole paragraph at the body level — see edit.go's Edit doc comment for
// why that unconditional walk matters to the gate) and builds a
// revisionSummary: InsCount/DelCount from <w:ins>/<w:del> specifically, and
// Authors from every element isRevisionAuthorElement recognizes. A decode
// error (should not happen for bytes that already passed Scan) just ends
// the walk early with whatever was accumulated so far, mirroring
// maxWordID's best-effort tolerance above.
func scanRevisions(documentXML []byte) revisionSummary {
	dec := xml.NewDecoder(bytes.NewReader(documentXML))
	var sum revisionSummary
	seen := make(map[string]bool)
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch {
		case isWordElement(se.Name, "ins"):
			sum.InsCount++
		case isWordElement(se.Name, "del"):
			sum.DelCount++
		}
		if !isRevisionAuthorElement(se.Name) {
			continue
		}
		val, ok := wordAttrVal(se, "author")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		if val == "" || seen[val] {
			continue
		}
		seen[val] = true
		sum.Authors = append(sum.Authors, val)
	}
	sort.Strings(sum.Authors)
	return sum
}

// revisionCtx carries the identity a tracked-change constructor needs to
// stamp onto every <w:ins>/<w:del> (and paragraph-mark <w:ins/>/<w:del/>)
// it produces: who authored it, when, and the next available w:id.
//
// A revisionCtx is meant to be built once per Edit batch (see Task 2) and
// reused across every wrapDel/wrapIns/markParagraph call in that batch, so
// nextID hands out a strictly increasing sequence and Now is called once
// per revision rather than drifting mid-batch.
type revisionCtx struct {
	// Author is the value written into every w:author attribute this
	// revisionCtx produces. attrs() substitutes "deepai" when this is "",
	// so callers (Task 2's EditOptions.Author) don't have to apply that
	// default themselves.
	Author string
	// Now returns the instant written into every w:date attribute.
	// newRevisionCtx defaults it to time.Now when nil; tests inject a fixed
	// clock so output bytes are assertable.
	Now func() time.Time
	// nextID is the next w:id value attrs() will hand out. It only ever
	// increases within one revisionCtx's lifetime.
	nextID int
}

// newRevisionCtx builds a revisionCtx whose id sequence starts one past the
// highest existing w:id anywhere in documentXML (0, i.e. ids start at 1, if
// none are found), so revisions it produces can never collide with an id
// already present in the document — see maxWordID for why the scan is not
// restricted to w:ins/w:del specifically.
func newRevisionCtx(documentXML []byte, author string, now func() time.Time) *revisionCtx {
	if now == nil {
		now = time.Now
	}
	return &revisionCtx{
		Author: author,
		Now:    now,
		nextID: maxWordID(documentXML) + 1,
	}
}

// maxWordID scans documentXML for the highest numeric value any element's
// w:id attribute carries, returning 0 if none is found. It is deliberately
// not limited to <w:ins>/<w:del>: w:id also appears on <w:bookmarkStart>,
// <w:comment>, <w:footnoteReference>, and others, and Word requires every
// w:id in a document to be globally unique regardless of which element
// carries it — reusing one that happens to belong to a bookmark would be
// just as much a collision as reusing one from an existing revision.
// Attribute values that don't parse as a non-negative integer, and w:id-
// named attributes belonging to some other, non-WordprocessingML
// vocabulary, are silently skipped rather than treated as a scan failure:
// this is a best-effort maximum, not a validator.
func maxWordID(documentXML []byte) int {
	dec := xml.NewDecoder(bytes.NewReader(documentXML))
	max := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			// Any decode error (including a genuinely malformed document,
			// which should never reach here in practice since d.doc is
			// always the product of a successful Scan) just ends the scan
			// early with whatever max was found so far, rather than
			// panicking or requiring this function to return an error the
			// interface (newRevisionCtx) has no way to propagate.
			return max
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		val, ok := wordAttrVal(se, "id")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(val)
		if err != nil || n < 0 {
			continue
		}
		if n > max {
			max = n
		}
	}
}

// attrs returns the w:id/w:author/w:date attribute string for one revision
// mark (a <w:ins>, <w:del>, or paragraph-mark <w:ins/>/<w:del/>), with a
// leading space so callers can splice it directly after an element name,
// e.g. "<w:ins" + rc.attrs() + ">". Each call consumes the next id in the
// sequence, so two revisions built from the same revisionCtx never share
// one.
func (rc *revisionCtx) attrs() string {
	id := rc.nextID
	rc.nextID++
	author := effectiveAuthor(rc.Author)
	// Author is attribute-value text, so it goes through the same escaping
	// as element content (both cover the same five XML metacharacters,
	// including the quote characters an attribute value needs escaped that
	// element content technically doesn't).
	escapedAuthor, _, err := escapeXMLText(author)
	if err != nil {
		// escapeXMLText only ever errors from xmlEscapeText, which (per its
		// own doc comment) never returns a non-nil error for well-formed
		// UTF-8 byte-at-a-time escaping; this is unreachable in practice.
		escapedAuthor = []byte(author)
	}
	date := rc.Now().UTC().Format(time.RFC3339)
	return fmt.Sprintf(` w:id="%d" w:author="%s" w:date="%s"`, id, escapedAuthor, date)
}

// isSelfClosingStartTag reports whether the start tag occupying
// data[tagStart:tagEnd] (as bracketed by two consecutive Decoder.Token/
// InputOffset calls the way scan.go's Scan does) is self-closing, i.e. ends
// in "/>" rather than being a "<name ...>" tag with a separate closing tag
// still to come. See scan.go's Run.SelfClosing doc comment for why
// encoding/xml collapses a self-closing element's StartElement and
// EndElement onto the same InputOffset, which is what makes this byte-
// suffix check both correct and necessary.
func isSelfClosingStartTag(data []byte, tagStart, tagEnd int) bool {
	return tagEnd-tagStart >= 2 && string(data[tagEnd-2:tagEnd]) == "/>"
}

// cloneRunWithText returns a deep copy of runElem with its text content
// replaced by newText and, when asDelText is true, its text-holding element
// rewritten from <w:t> to <w:delText> (Word's tag for text that has been
// marked deleted — leaving it as <w:t> makes Word treat the text as still
// present, which is item 2 of the plan's OOXML shape notes). Everything
// else in runElem — most importantly <w:rPr> (bold, colour, hyperlink
// styling) — survives byte-for-byte untouched; this is why del/ins
// revisions are built by cloning the original run rather than constructing
// a fresh bare <w:r>.
//
// xml:space="preserve" is carried over from the original tag if present,
// and added if newText itself has leading/trailing whitespace that would
// otherwise be collapsed by Word — the same rule Apply already applies to
// ordinary (non-tracked) replacements, via splice.go's needsPreserve.
//
// runElem must contain at most one <w:t> or <w:delText> element (searched
// recursively, at any depth). More than one is refused rather than
// guessed at: a single <w:r> holding several <w:t> children (e.g. split by
// a <w:br/>) is exactly the shape that caused a silent text-loss defect
// earlier in this package (see edit.go's runElemSharedWithSibling) —
// picking one node to receive newText and blanking the other(s) would
// repeat that mistake under a different name, so this function reports an
// error instead and leaves the choice to the caller. A run with NO
// text-holding element at all (e.g. one that holds only a <w:br/>) is
// returned unchanged when newText is "" (nothing to replace), and refused
// when newText is non-empty (there is no defined place to put it relative
// to the run's other content).
func cloneRunWithText(runElem []byte, newText string, asDelText bool) ([]byte, error) {
	dec := xml.NewDecoder(bytes.NewReader(runElem))

	var prevOffset int
	matches := 0
	var elemStart, elemEnd int
	var hasPreserve bool

	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("clone run: %w", err)
		}
		offset := int(dec.InputOffset())

		switch t := tok.(type) {
		case xml.StartElement:
			if isWordElement(t.Name, "t") || isWordElement(t.Name, "delText") {
				matches++
				if matches == 1 {
					elemStart = prevOffset
					hasPreserve = hasPreserveAttr(t)
				}
			}
		case xml.EndElement:
			// For a self-closing <w:t/>, encoding/xml emits this EndElement
			// immediately after the StartElement above, at the SAME
			// InputOffset (no CharData token in between) — see scan.go's
			// Run.SelfClosing doc comment. That means this single branch
			// correctly captures elemEnd for both the self-closing and the
			// ordinary "<w:t>...</w:t>" shape with no extra self-closing
			// detection needed.
			if matches == 1 && (isWordElement(t.Name, "t") || isWordElement(t.Name, "delText")) {
				elemEnd = offset
			}
		}
		prevOffset = offset
	}

	if matches > 1 {
		return nil, fmt.Errorf(
			"clone run: run contains %d text-holding elements (<w:t>/<w:delText>); "+
				"callers must isolate a single text node before cloning it for a tracked change, or text would be silently dropped choosing which one to keep",
			matches)
	}
	if matches == 0 {
		if newText == "" {
			out := make([]byte, len(runElem))
			copy(out, runElem)
			return out, nil
		}
		return nil, fmt.Errorf(
			"clone run: run has no <w:t> or <w:delText> element to hold newText %q", newText)
	}

	escaped, _, err := escapeXMLText(newText)
	if err != nil {
		return nil, fmt.Errorf("clone run: %w", err)
	}

	name := "t"
	if asDelText {
		name = "delText"
	}
	preserve := hasPreserve || needsPreserve(newText)

	var openTag strings.Builder
	openTag.WriteString("<w:")
	openTag.WriteString(name)
	if preserve {
		openTag.WriteString(` xml:space="preserve"`)
	}
	openTag.WriteString(">")

	out := make([]byte, 0, len(runElem)+len(escaped)+32)
	out = append(out, runElem[:elemStart]...)
	out = append(out, openTag.String()...)
	out = append(out, escaped...)
	out = append(out, "</w:"...)
	out = append(out, name...)
	out = append(out, ">"...)
	out = append(out, runElem[elemEnd:]...)
	return out, nil
}

// wrapDel clones runElem with its text converted to <w:delText> holding
// oldText (see cloneRunWithText), then wraps the clone in a <w:del> element
// carrying this revisionCtx's next id/author/date.
func (rc *revisionCtx) wrapDel(runElem []byte, oldText string) ([]byte, error) {
	cloned, err := cloneRunWithText(runElem, oldText, true)
	if err != nil {
		return nil, fmt.Errorf("wrap del: %w", err)
	}
	var out bytes.Buffer
	out.WriteString("<w:del")
	out.WriteString(rc.attrs())
	out.WriteString(">")
	out.Write(cloned)
	out.WriteString("</w:del>")
	return out.Bytes(), nil
}

// wrapIns clones runElem with its text replaced by newText (kept as plain
// <w:t>, never delText — inserted text is not deleted text), then wraps the
// clone in a <w:ins> element carrying this revisionCtx's next
// id/author/date.
func (rc *revisionCtx) wrapIns(runElem []byte, newText string) ([]byte, error) {
	cloned, err := cloneRunWithText(runElem, newText, false)
	if err != nil {
		return nil, fmt.Errorf("wrap ins: %w", err)
	}
	var out bytes.Buffer
	out.WriteString("<w:ins")
	out.WriteString(rc.attrs())
	out.WriteString(">")
	out.Write(cloned)
	out.WriteString("</w:ins>")
	return out.Bytes(), nil
}

// markParagraph returns a copy of paraElem (a whole <w:p>...</w:p>) with
// the paragraph MARK itself — the invisible pilcrow, not any text inside
// the paragraph — flagged as inserted (inserted == true) or deleted
// (inserted == false). Per the plan's OOXML shape notes item 5, this is
// what stops Word from merging the paragraph into its neighbour once the
// revision is accepted: without it, only the paragraph's text content
// would be marked, not the paragraph break.
//
// The marker is a self-closing <w:ins/> or <w:del/> placed inside the
// paragraph's own <w:pPr><w:rPr>, per CT_ParaRPr's schema (ins/del must be
// the first child of rPr, before rStyle/rFonts/b/...) and CT_PPr's (rPr
// sits near the end of pPr's content model). markParagraph creates
// whichever of <w:pPr>/<w:rPr> the paragraph is missing, preserving every
// byte of what already exists.
//
// Known simplification: if the paragraph's own <w:pPr> already contains a
// <w:sectPr> or <w:pPrChange> (section-break paragraphs), schema order
// wants a freshly created <w:rPr> placed BEFORE either — this function
// always places it immediately before </w:pPr>, which would land after
// them. Section-break paragraphs are not the paragraphs a polish workflow
// marks up, so this is accepted rather than handled; flag it if it turns
// out to matter for Task 2.
//
// A self-closing <w:p/> (no content at all — the shape
// planParagraphTarget's only-paragraph-in-a-table-cell delete carve-out
// produces) is refused: there is no content model to insert a marker into,
// and silently placing one after the "/>" would orphan it outside the
// paragraph entirely.
func (rc *revisionCtx) markParagraph(paraElem []byte, inserted bool) ([]byte, error) {
	name := "del"
	if inserted {
		name = "ins"
	}
	marker := "<w:" + name + rc.attrs() + "/>"

	dec := xml.NewDecoder(bytes.NewReader(paraElem))

	var prevOffset int
	var pDepth, pPrDepth, rPrDepth int

	pOpenEnd := -1
	foundPPr, foundRPr := false, false
	pPrOpenEnd, pPrCloseStart := -1, -1
	pPrSelfClosing := false
	rPrOpenEnd, rPrCloseStart := -1, -1
	rPrSelfClosing := false

	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("mark paragraph: %w", err)
		}
		offset := int(dec.InputOffset())

		switch t := tok.(type) {
		case xml.StartElement:
			switch {
			case isWordElement(t.Name, "p"):
				pDepth++
				if pDepth == 1 {
					if isSelfClosingStartTag(paraElem, prevOffset, offset) {
						return nil, errors.New("mark paragraph: paragraph is a self-closing <w:p/> with no content to mark")
					}
					pOpenEnd = offset
				}
			case isWordElement(t.Name, "pPr"):
				pPrDepth++
				if pPrDepth == 1 && pDepth == 1 && !foundPPr {
					foundPPr = true
					pPrOpenEnd = offset
					pPrSelfClosing = isSelfClosingStartTag(paraElem, prevOffset, offset)
				}
			case isWordElement(t.Name, "rPr"):
				rPrDepth++
				if rPrDepth == 1 && pPrDepth == 1 && pDepth == 1 && !foundRPr {
					foundRPr = true
					rPrOpenEnd = offset
					rPrSelfClosing = isSelfClosingStartTag(paraElem, prevOffset, offset)
				}
			}
		case xml.EndElement:
			switch {
			case isWordElement(t.Name, "rPr"):
				if rPrDepth > 0 {
					if rPrDepth == 1 && foundRPr && rPrCloseStart == -1 {
						rPrCloseStart = prevOffset
					}
					rPrDepth--
				}
			case isWordElement(t.Name, "pPr"):
				if pPrDepth > 0 {
					if pPrDepth == 1 && foundPPr && pPrCloseStart == -1 {
						pPrCloseStart = prevOffset
					}
					pPrDepth--
				}
			case isWordElement(t.Name, "p"):
				if pDepth > 0 {
					pDepth--
				}
			}
		}
		prevOffset = offset
	}
	_ = rPrCloseStart // recorded for symmetry/documentation; not needed by any branch below.
	_ = pPrCloseStart

	switch {
	case foundRPr && !rPrSelfClosing:
		// Existing, non-empty <w:rPr>...</w:rPr>: splice the marker in as
		// the first child, right after the open tag.
		out := make([]byte, 0, len(paraElem)+len(marker))
		out = append(out, paraElem[:rPrOpenEnd]...)
		out = append(out, marker...)
		out = append(out, paraElem[rPrOpenEnd:]...)
		return out, nil

	case foundRPr && rPrSelfClosing:
		// <w:rPr/> (or "<w:rPr attrs/>") must become a real pair to hold a
		// child: drop the trailing "/>" and reopen it as ">...</w:rPr>".
		out := make([]byte, 0, len(paraElem)+len(marker)+8)
		out = append(out, paraElem[:rPrOpenEnd-2]...)
		out = append(out, ">"...)
		out = append(out, marker...)
		out = append(out, "</w:rPr>"...)
		out = append(out, paraElem[rPrOpenEnd:]...)
		return out, nil

	case foundPPr && !pPrSelfClosing:
		// pPr exists but has no rPr: add one right before </w:pPr>.
		out := make([]byte, 0, len(paraElem)+len(marker)+16)
		out = append(out, paraElem[:pPrCloseStart]...)
		out = append(out, "<w:rPr>"...)
		out = append(out, marker...)
		out = append(out, "</w:rPr>"...)
		out = append(out, paraElem[pPrCloseStart:]...)
		return out, nil

	case foundPPr && pPrSelfClosing:
		// <w:pPr/> must become a real pair to hold the new rPr.
		out := make([]byte, 0, len(paraElem)+len(marker)+32)
		out = append(out, paraElem[:pPrOpenEnd-2]...)
		out = append(out, ">"...)
		out = append(out, "<w:rPr>"...)
		out = append(out, marker...)
		out = append(out, "</w:rPr>"...)
		out = append(out, "</w:pPr>"...)
		out = append(out, paraElem[pPrOpenEnd:]...)
		return out, nil

	default:
		// No <w:pPr> at all: create one, which must be the paragraph's
		// FIRST child per CT_P's schema.
		if pOpenEnd == -1 {
			return nil, errors.New("mark paragraph: no <w:p> start tag found")
		}
		out := make([]byte, 0, len(paraElem)+len(marker)+32)
		out = append(out, paraElem[:pOpenEnd]...)
		out = append(out, "<w:pPr><w:rPr>"...)
		out = append(out, marker...)
		out = append(out, "</w:rPr></w:pPr>"...)
		out = append(out, paraElem[pOpenEnd:]...)
		return out, nil
	}
}
