package docx

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Edit describes one requested change, in the vocabulary a caller (or an
// LLM tool) uses: a 1-based paragraph index plus either a run index, a
// find substring, or neither (meaning "the whole paragraph").
//
// Run and Find are mutually exclusive: at most one may be set. Op selects
// the operation; "" defaults to "replace". insert_before/insert_after are
// always paragraph-level and never accept Run or Find, because there is no
// single well-defined run or substring position to anchor a paragraph
// insertion to.
type Edit struct {
	// Para is the 1-based paragraph index (see Para.Index) this edit
	// targets.
	Para int
	// Run is the 1-based run index within the paragraph (see Run.Index).
	// 0 means unspecified.
	Run int
	// Find is a literal (non-regex) substring to locate within the
	// paragraph's concatenated run text. nil means unspecified (the same
	// "whole paragraph" meaning "" used to carry before this field existed).
	//
	// Find is a *string, not a string, specifically so "not given" and
	// "given as an empty string" are two different values: a JSON tool
	// schema decoded straight into a plain string field (the common pattern
	// this package's callers use) cannot tell an omitted "find" key apart
	// from one sent as "". Collapsing both into the same behavior — as an
	// earlier version of this field did — meant a caller-side bug that
	// produced an empty find string silently escalated from "replace a
	// substring" to "replace the whole paragraph, flattening its
	// formatting", with no error. A non-nil pointer to "" is refused
	// outright (see planEdit); only nil gets the whole-paragraph default.
	Find *string
	// Text is the replacement or inserted text, in decoded form. Ignored
	// by "delete".
	Text string
	// Op selects the operation: "replace" (default, i.e. "" behaves the
	// same as "replace"), "insert_before", "insert_after", or "delete".
	Op string
}

// EditOptions configures a batch of Edit calls.
type EditOptions struct {
	// Protect is a list of items that must survive every edit that
	// touches them. Each entry is compiled as a regular expression;
	// entries that fail to compile (e.g. "(beta", an unbalanced group)
	// are matched literally instead — see compileProtect.
	Protect []string
	// TrackChanges, when true, makes every planner in this batch emit Word
	// native tracked-change markup (<w:ins>/<w:del>, plus a paragraph-mark
	// <w:ins/>/<w:del/> for paragraph-level ops) instead of rewriting
	// content in place. Turning this on changes ONLY the bytes each
	// patch contains — Before/After/Warning/Reason/target reported on
	// EditOutcome are identical to what the same edit would report with
	// TrackChanges false. See revision.go and the plan's "各 op 的修订形态"
	// table for the exact shape per op.
	TrackChanges bool
	// Author is the w:author value stamped on every revision this batch
	// produces. "" defaults to "deepai" (applied by revisionCtx.attrs,
	// not here). Ignored when TrackChanges is false.
	Author string
	// Now returns the instant stamped into every w:date attribute this
	// batch produces. nil defaults to time.Now (applied by newRevisionCtx,
	// not here). Ignored when TrackChanges is false; tests inject a fixed
	// clock so tracked-change output bytes are assertable.
	Now func() time.Time
}

// EditOutcome reports what happened to one Edit within a batch.
type EditOutcome struct {
	// Edit is the request this outcome corresponds to, echoed back so a
	// caller can correlate Outcomes[i] without relying on slice order
	// alone.
	Edit Edit
	// Applied reports whether this edit's patch was generated and
	// included in the batch submitted to Apply. It says nothing about
	// whether other edits in the same batch succeeded.
	Applied bool
	// Before and After are the text of the region this edit changed, per
	// §4.2's op table: for "replace" it is the replaced region's
	// original/new text (which may be the whole paragraph, a whole run,
	// or a find match, depending on which of Run/Find was given); for
	// insert_before/insert_after, Before is always "" and After is the
	// inserted text; for "delete", Before is the removed text and After
	// is always "".
	Before string
	After  string
	// Reason explains why an edit was NOT applied. "" when Applied is
	// true.
	Reason string
	// Warning notes something a caller should know about an edit that WAS
	// applied: a whole-paragraph replace on a multi-run paragraph
	// collapsing formatting to the first run's, a delete that removed
	// text matching a Protect pattern (deletes are never refused for
	// this — see §4.2 — but are still worth flagging), or a delete that
	// left an empty paragraph behind instead of removing it because it
	// was its table cell's only paragraph.
	Warning string
}

// EditResult is one Document.Edit call's outcome.
type EditResult struct {
	// Outcomes has exactly one entry per input Edit, in the same order.
	Outcomes []EditOutcome
	// Applied is the count of Outcomes with Applied == true.
	Applied int
	// TotalParas is the document's paragraph count AFTER this call, i.e.
	// Document.TotalParas() immediately after Edit returns.
	TotalParas int
	// ParaCountChanged reports whether TotalParas differs from the
	// paragraph count BEFORE this call. Per design §5.4, only
	// insert_before/insert_after and a whole-paragraph delete change the
	// paragraph count; a caller that has previously-read para_index values
	// (e.g. from Read) needs to know when those may have shifted, since
	// this package leaves index-shift bookkeeping to the caller rather than
	// tracking it itself.
	ParaCountChanged bool
}

// The four operations an Edit can request.
const (
	opReplace      = "replace"
	opInsertBefore = "insert_before"
	opInsertAfter  = "insert_after"
	opDelete       = "delete"
)

// Edit applies a batch of edits to the document. Every patch is computed
// from a single pre-edit scan (d.paras / d.doc as they were when Edit was
// called), so the edits in a batch see consistent, simultaneously-valid
// offsets: an edit to paragraph 2 and an edit to paragraph 7 never
// interfere with each other's byte ranges, because neither edit's plan
// depends on any other edit having already landed.
//
// A refused edit contributes no patch and never blocks the rest of the
// batch (design §4.2: "任一条被拒绝只是不生成补丁，不影响其他条"). That
// includes edits refused because their planned patch collides with an
// earlier edit's planned patch — see planEdit and patchesCollide. Collision
// is detected and resolved HERE, before Apply ever runs: only the LATER of
// two colliding edits (by input order, never by byte offset) is refused,
// and its Reason names the earlier edit's ordinal and target instead of a
// byte offset, which would be meaningless to an LLM caller. Apply's own
// overlap check still runs underneath as a defensive fallback, but it
// should never actually fire from this method now that the pre-Apply
// collision pass has run.
//
// Edit refuses the entire batch up front, with an error rather than a
// per-edit Reason, when the document ALREADY contained revision marks the
// moment it was opened AND at least one of those revisions' w:author values
// is not this batch's own author (d.revisionAuthorsAtOpen minus
// effectiveAuthor(opts.Author) is non-empty).
//
// The trigger for this check is len(d.revisionAuthorsAtOpen) > 0, NOT
// d.hadRevisionsAtOpen. An earlier version of this gate used
// hadRevisionsAtOpen (itself derived from Scan's per-PARAGRAPH HasRevisions
// flag) and that was a false-allow hazard: scan.go's <w:ins>/<w:del> cases
// are guarded by "&& txbxDepth == 0" (a text box's whole subtree, including
// any revision marks inside it, is deliberately not paragraph-indexed — see
// Scan's doc comment), and separately, a <w:ins> that wraps an entire <w:p>
// as the body's direct child fires its StartElement before that <w:p> has
// opened, so paraHasRevisions never gets set for the paragraph inside it
// either. Both shapes leave every Para.HasRevisions flag false (so
// hadRevisionsAtOpen is false) even though a real, other-author revision is
// sitting in the file — and scanRevisions (which builds
// d.revisionAuthorsAtOpen) walks the ENTIRE document.xml token stream with
// no such guard, so it already saw that author correctly. Gating on the
// authors set directly, rather than re-deriving "were there any revisions
// at all" from the paragraph cache and hoping the two data sources agree,
// closes that gap by construction: whenever scanRevisions found ANY author,
// the comparison runs, regardless of what the paragraph-level scan saw.
//
// Gating on AUTHOR, not merely on "were there any revisions", is what lets a
// document reopened between calls — the tool layer's per-call
// OpenDocument/Edit/Save cycle (pkg/tools/builtin/docx.go) reopens fresh
// every time, so d.revisionAuthorsAtOpen is non-empty again on every call
// after the first one wrote its own revisions — keep landing: this batch's
// own earlier tracked changes are recognized as its own and never trigger
// the refusal, while someone else's still-pending revisions (a human
// reviewer's edits sitting in the file, or a different author's polish
// pass) do. This deliberately does NOT use HasRevisions(): that method
// reads the CURRENT paragraph cache, which a TrackChanges edit itself flips
// to true via rescan, and gating on it (even author-aware) would need the
// CURRENT authors too, when what this check needs is what was already
// there BEFORE this session touched anything — exactly what
// revisionAuthorsAtOpen captures once, at open, and never updates.
// HasRevisions() keeps its existing meaning unchanged for read.go/format.go's
// own, unrelated uses of it. hadRevisionsAtOpen itself is left in place
// (see its own doc comment) purely as the bookkeeping it always was; Edit no
// longer reads it.
//
// When the document was never touched by TrackChanges (opts.TrackChanges is
// false), the batch's identity for this comparison is still
// effectiveAuthor(opts.Author) — the same default ("deepai") a TrackChanges
// batch with the same opts.Author would have stamped — so an untracked
// "finish the polish, tracking off" call right after a tracked one is judged
// by the same identity, not treated as authorless. Both sides of the
// comparison are trimmed of surrounding whitespace (effectiveAuthor for the
// caller, scanRevisions for what's already on disk), so a caller-supplied
// author differing from an existing one only by incidental whitespace does
// not manufacture a spurious "different author" refusal.
//
// On success, Edit re-scans the document (rescan) so that Paras() and
// TotalParas() reflect the new state immediately. If the combined patch
// passes Apply but the result then fails to rescan (Scan errors on it),
// Edit restores the part to its pre-edit content before returning the
// error, so — per this method's contract — the document is left
// completely unmodified rather than left corrupted in memory with the bad
// bytes one Save/SaveAs call away from overwriting the user's file on
// disk.
func (d *Document) Edit(edits []Edit, opts EditOptions) (EditResult, error) {
	if len(d.revisionAuthorsAtOpen) > 0 {
		caller := effectiveAuthor(opts.Author)
		var other []string
		for _, a := range d.revisionAuthorsAtOpen {
			if a != caller {
				other = append(other, a)
			}
		}
		if len(other) > 0 {
			return EditResult{}, fmt.Errorf(
				"docx: this document already contains unreviewed revision marks (w:ins/w:del or similar) from "+
					"author(s) %s, which does not include this batch's author %q; tell the user about these pending "+
					"revisions so they can decide what to do: open the file in Word and accept or reject them "+
					"first, or — only once they explicitly confirm it is fine to build on top of them — retry this "+
					"edit with author set to match one of them (e.g. author=%q) so it is recognized as continuing "+
					"the same review rather than starting an unrelated one",
				formatAuthorList(other), caller, other[0])
		}
	}

	matchers := compileProtect(opts.Protect)
	// rc is nil (and stays nil) unless TrackChanges is on; every planner
	// below treats a nil rc as "plain patch" and a non-nil rc as "build the
	// tracked-changes shape instead". Building exactly one revisionCtx per
	// batch — rather than one per edit — is what keeps w:id unique across
	// every revision this call produces: revisionCtx.nextID only ever
	// increases (see revision.go), so two edits in the same batch, or two
	// edits on the same run, never collide.
	var rc *revisionCtx
	if opts.TrackChanges {
		rc = newRevisionCtx(d.doc, opts.Author, opts.Now)
	}
	// paras/doc are read directly from the Document's cache rather than via
	// the deep-copying Paras() accessor: this whole method only reads Run/
	// Para fields (byte spans, text) to plan patches, never mutates them, so
	// the copy Paras() makes would be pure overhead. All edits in the batch
	// plan against this single snapshot, per the doc comment above.
	paras := d.paras
	doc := d.doc
	beforeTotal := len(paras)

	// candidate is one edit whose planning succeeded, carrying just enough
	// to run the collision pass below: which outcome slot it belongs to,
	// the patches planEdit built for it, and a human-readable description
	// of what it targets (for a later edit's collision Reason).
	type candidate struct {
		idx     int
		patches []Patch
		target  string
	}

	outcomes := make([]EditOutcome, len(edits))
	var candidates []candidate

	for i, e := range edits {
		plan, before, after, warning, reason, target, ok := planEdit(doc, e, paras, matchers, rc)
		outcomes[i] = EditOutcome{
			Edit:    e,
			Applied: ok,
			Before:  before,
			After:   after,
			Reason:  reason,
			Warning: warning,
		}
		if ok {
			candidates = append(candidates, candidate{idx: i, patches: plan, target: target})
		}
	}

	// Collision pass: walk candidates in input order, accepting each one
	// unless its patches collide with an already-accepted candidate's. This
	// greedily favors earlier edits (matching "refuse the LATER edit") and,
	// as a side effect, means a refused edit's own would-be conflicts with
	// still-later edits never materialize (it was never added to accepted),
	// so those later edits are judged only against what will actually be
	// applied.
	var accepted []candidate
	var patches []Patch
	applied := 0
	for _, c := range candidates {
		conflictIdx, conflictTarget := -1, ""
		for _, a := range accepted {
			if patchesCollide(c.patches, a.patches) {
				conflictIdx, conflictTarget = a.idx, a.target
				break
			}
		}
		if conflictIdx != -1 {
			outcomes[c.idx].Applied = false
			outcomes[c.idx].Warning = ""
			outcomes[c.idx].Reason = collisionReason(conflictIdx, conflictTarget, outcomes[conflictIdx].Edit, outcomes[c.idx].Edit)
			continue
		}
		accepted = append(accepted, c)
		patches = append(patches, c.patches...)
		applied++
	}

	if len(patches) > 0 {
		snapshot, partOK := d.pkg.Part(DocumentPart)
		if !partOK {
			return EditResult{}, fmt.Errorf("docx: package has no %s part", DocumentPart)
		}
		// Captured right next to the snapshot read, before ApplyToPart's
		// SetPart call (below) unconditionally flips this to true: if the
		// part was never modified before this call, and the rescan-failure
		// rollback below restores it byte-for-byte, the part must end up
		// exactly as unmodified as it started. Without this, SetPart's
		// rollback (which exists to restore CONTENT) also had the side
		// effect of leaving the part flagged dirty even though its bytes
		// are, once again, identical to the original — so WriteTo would
		// re-deflate an untouched entry instead of copying its original
		// compressed bytes verbatim, breaking document.go's SaveAs promise
		// that an untouched document reproduces the source byte for byte
		// (see the C3 rollback finding).
		wasModified := d.pkg.modified[DocumentPart]
		if err := d.pkg.ApplyToPart(DocumentPart, patches); err != nil {
			return EditResult{}, fmt.Errorf("docx: apply edits: %w", err)
		}
		if err := d.rescan(); err != nil {
			// snapshot is never mutated in place: Apply always builds a
			// fresh output slice rather than editing its input in place,
			// and SetPart only ever swaps the map entry to point at that
			// new slice — so snapshot still holds the exact pre-edit bytes,
			// and restoring it here is what makes "the document is left
			// unmodified on failure" true for this path too, not just for
			// the ones that never reach SetPart at all.
			restoreErr := d.pkg.SetPart(DocumentPart, snapshot)
			if restoreErr == nil && !wasModified {
				delete(d.pkg.modified, DocumentPart)
			}
			if restoreErr != nil {
				return EditResult{}, fmt.Errorf(
					"docx: rescan after edit failed (%w) and restoring the pre-edit content also failed (%w); "+
						"the in-memory package may hold unscannable content, but nothing has been written to disk",
					err, restoreErr)
			}
			return EditResult{}, fmt.Errorf("docx: rescan after edit: %w", err)
		}
		d.modified = true
	}

	afterTotal := d.TotalParas()
	return EditResult{
		Outcomes:         outcomes,
		Applied:          applied,
		TotalParas:       afterTotal,
		ParaCountChanged: afterTotal != beforeTotal,
	}, nil
}

// normalizeOp validates op and defaults "" to "replace".
func normalizeOp(op string) (string, error) {
	if op == "" {
		return opReplace, nil
	}
	switch op {
	case opReplace, opInsertBefore, opInsertAfter, opDelete:
		return op, nil
	default:
		return "", fmt.Errorf("unknown op %q; must be one of %q, %q, %q, %q (or \"\" for the default replace)",
			op, opReplace, opInsertBefore, opInsertAfter, opDelete)
	}
}

// planEdit validates e against paras and, if valid, returns the Patch(es)
// that realize it, the Before/After text for the changed region, and target
// — a short human-readable description of what the edit touches (e.g. "run
// 2 in paragraph 4"), used only to name the edit in a LATER edit's collision
// Reason (see Document.Edit). ok is false when the edit is refused, in
// which case reason explains why and patch/before/after/warning/target are
// meaningless.
//
// The parameter validation order (op legality, paragraph range, the
// insert-ops-are-always-paragraph-level rule, run/find mutual exclusion, the
// explicitly-empty-Find refusal, then XML-legal-character check) does not
// matter for any single-violation input, since each check only fires when
// its own precondition holds; it is fixed here purely so refusal messages
// are deterministic when an edit happens to be invalid in more than one way
// at once.
func planEdit(doc []byte, e Edit, paras []Para, matchers []protectMatcher, rc *revisionCtx) (patches []Patch, before, after, warning, reason, target string, ok bool) {
	op, err := normalizeOp(e.Op)
	if err != nil {
		return nil, "", "", "", err.Error(), "", false
	}
	if e.Para < 1 || e.Para > len(paras) {
		return nil, "", "", "", fmt.Sprintf(
			"paragraph %d is out of range: the document has %d paragraphs", e.Para, len(paras)), "", false
	}
	if (op == opInsertBefore || op == opInsertAfter) && (e.Run != 0 || e.Find != nil) {
		return nil, "", "", "", fmt.Sprintf(
			"%s always targets a whole paragraph and does not accept Run or Find; drop them or use replace/delete", op), "", false
	}
	if e.Run != 0 && e.Find != nil {
		return nil, "", "", "", "run and find are mutually exclusive: set at most one", "", false
	}
	// A non-nil Find pointing at "" is refused rather than silently treated
	// as "unspecified": an empty find string cannot usefully locate
	// anything, and since a JSON tool schema decoded into a plain string
	// field cannot distinguish "find omitted" from "find sent as empty"
	// (see the Find field's doc comment), the only safe policy is to make
	// the two cases behave differently at THIS layer, where the distinction
	// still exists as nil vs. non-nil. Omit Find entirely for a
	// whole-paragraph replace/delete instead.
	if e.Find != nil && *e.Find == "" {
		return nil, "", "", "", "find is an explicit empty string, which cannot match anything; " +
			"omit find entirely to target the whole paragraph, or set it to a non-empty substring", "", false
	}
	// "delete" never consults e.Text (After is always ""), so it is the one
	// op exempt from this check; every other op would otherwise splice
	// e.Text into the document one way or another. Checking centrally here
	// — rather than relying on well-formedness gates further down the
	// pipeline — is what turns a single caller-controlled illegal character
	// (e.g. a literal NUL byte) into a per-edit Reason instead of either a
	// whole-batch Apply error (for an insert, whose raw patch DOES get
	// well-formedness-checked) or, worse, a plain PatchRun/PatchFind replace
	// that sails through Apply (no well-formedness check on a non-raw
	// patch) and only fails later at rescan — by which point SetPart has
	// already committed the bad bytes. See the C3 finding.
	if op != opDelete {
		// Checked before firstIllegalXMLChar, and separately from it:
		// firstIllegalXMLChar iterates RUNES, so an invalid UTF-8 byte
		// decodes to U+FFFD (the replacement character) — itself a legal
		// XML codepoint — and slips straight past that check. A plain
		// replace/insert patch built from such text is never
		// well-formedness-checked by Apply either (that gate only runs for
		// a Raw patch), so the bad byte would reach document.xml untouched
		// and only surface later at rescan, by which point SetPart has
		// already committed it — the whole-batch failure the C3 finding
		// pins. Refusing it HERE, per edit, keeps this the same shape as
		// every other input-validation refusal in this function: the rest
		// of the batch is unaffected.
		if !utf8.ValidString(e.Text) {
			return nil, "", "", "", "text is not valid UTF-8; only well-formed UTF-8 can be written into document content", "", false
		}
		if r, bad := firstIllegalXMLChar(e.Text); bad {
			return nil, "", "", "", fmt.Sprintf(
				"text contains U+%04X, which XML 1.0 does not allow in document content; remove or replace that character", r), "", false
		}
	}

	para := paras[e.Para-1]

	if op == opInsertBefore || op == opInsertAfter {
		return planInsert(doc, e, op, para, paras, matchers, rc)
	}
	switch {
	case e.Run != 0:
		return planRunTarget(doc, e, op, para, matchers, rc)
	case e.Find != nil:
		return planFindTarget(doc, e, op, para, matchers, rc)
	default:
		return planParagraphTarget(doc, e, op, para, paras, matchers, rc)
	}
}

// trackChangesReason formats a per-edit refusal reason for a tracked-change
// construction failure (revision.go's cloneRunWithText/wrapDel/wrapIns/
// markParagraph), most commonly cloneRunWithText's refusal of a run holding
// more than one text-holding element — see its doc comment. This turns that
// error into exactly the kind of per-edit Reason every other refusal in this
// file produces, rather than letting it escape as a whole-batch error.
func trackChangesReason(err error) string {
	return fmt.Sprintf("cannot represent this edit as a tracked change: %v", err)
}

// planInsert builds the paragraph-level insert_before/insert_after patch.
// Per §4.2's table, Before is always "" (nothing is replaced) and After is
// the inserted text. Protect validation for an insert checks something
// different from replace/delete, since there is no "before" to compare
// against: it checks whether the inserted text FORGES or mistypes a
// protected item's spelling — e.g. inserting a plausible-looking but wrong
// version number when Protect names a version pattern — by checking whether
// each protect-pattern match found in e.Text already occurs SOMEWHERE in
// the document (paras, the pre-edit snapshot). A match that already exists
// elsewhere is a legitimate repeat of a real protected item; a match that
// does not is forged or mistyped, and is refused. See forgedProtectedItems.
//
// The new paragraph is built by hand as the minimal
// <w:p><w:r><w:t>...</w:t></w:r></w:p> and spliced in raw via
// PatchRawSpan. PatchRawSpan does not escape its input — that is its whole
// point, so structural tags can be written verbatim — which means THIS
// function is responsible for XML-escaping e.Text before it goes anywhere
// near the raw string. Skipping that step is exactly the "well-formed but
// wrong" trap called out in Apply's docs: unescaped "&" or "<" in text
// content does not break well-formedness (the check Apply runs whenever any
// patch is Raw), so a broken document would sail through validation and
// only fail later, inside Word.
func planInsert(doc []byte, e Edit, op string, para Para, paras []Para, matchers []protectMatcher, rc *revisionCtx) ([]Patch, string, string, string, string, string, bool) {
	before := ""
	after := e.Text
	target := fmt.Sprintf("an insert %s paragraph %d", strings.TrimPrefix(op, "insert_"), para.Index)

	if broken := forgedProtectedItems(e.Text, documentText(paras), matchers); len(broken) > 0 {
		return nil, before, after, "", protectReason(broken), "", false
	}

	var rawXML string
	if rc != nil {
		paraXML, err := trackedInsertParagraph(rc, e.Text)
		if err != nil {
			return nil, before, after, "", trackChangesReason(err), "", false
		}
		rawXML = paraXML
	} else {
		// The trailing count is discarded: planEdit's firstIllegalXMLChar
		// check (above, before this function is ever reached for a
		// non-delete op) already refuses any e.Text containing an XML-1.0
		// illegal character, so escapeXMLText never has anything to strip
		// here in practice.
		escaped, _, err := escapeXMLText(e.Text)
		if err != nil {
			return nil, before, after, "", fmt.Sprintf("escape insert text: %v", err), "", false
		}
		rawXML = `<w:p><w:r><w:t xml:space="preserve">` + string(escaped) + `</w:t></w:r></w:p>`
	}

	var span Span
	if op == opInsertBefore {
		span = Span{Start: para.Span.Start, End: para.Span.Start}
	} else {
		span = Span{Start: para.Span.End, End: para.Span.End}
	}
	patch := PatchRawSpan(doc, span, rawXML)
	return []Patch{patch}, before, after, "", "", target, true
}

// trackedInsertParagraph builds the tracked-changes shape of a new
// insert_before/insert_after paragraph: a <w:r> holding text (built with an
// empty placeholder <w:t> so cloneRunWithText has a text node to replace) is
// wrapped in <w:ins>, and the paragraph mark itself is flagged inserted via
// markParagraph — per the plan's OOXML shape notes item 5, without the
// paragraph-mark flag Word would merge this paragraph into its neighbour
// once the insertion is accepted.
func trackedInsertParagraph(rc *revisionCtx, text string) (string, error) {
	insRun, err := rc.wrapIns([]byte(`<w:r><w:t></w:t></w:r>`), text)
	if err != nil {
		return "", err
	}
	marked, err := rc.markParagraph([]byte("<w:p>"+string(insRun)+"</w:p>"), true)
	if err != nil {
		return "", err
	}
	return string(marked), nil
}

// trackedRunPatch builds the tracked-changes shape for a run/find-scoped
// replace or delete, splitting run's <w:r> element into up to four pieces —
// all cloned from the original run so <w:rPr> formatting survives — per the
// plan's "find 的情形要拆成四段" note: a prefix run holding run.Text[:localStart],
// a <w:del> holding run.Text[localStart:localEnd] (the "before" text) as
// delText, optionally (withIns) a <w:ins> holding after, and a suffix run
// holding run.Text[localEnd:]. No run is emitted for an empty prefix/suffix,
// and a plain run-level (not find-scoped) call simply passes
// localStart=0, localEnd=len(run.Text), which makes prefix and suffix both
// "" and so contributes only the del(+ins).
func trackedRunPatch(rc *revisionCtx, doc []byte, run Run, localStart, localEnd int, before, after string, withIns bool) (Patch, error) {
	elemBytes := doc[run.Elem.Start:run.Elem.End]
	prefix := run.Text[:localStart]
	suffix := run.Text[localEnd:]

	var out []byte
	if prefix != "" {
		p, err := cloneRunWithText(elemBytes, prefix, false)
		if err != nil {
			return Patch{}, err
		}
		out = append(out, p...)
	}
	del, err := rc.wrapDel(elemBytes, before)
	if err != nil {
		return Patch{}, err
	}
	out = append(out, del...)
	if withIns {
		ins, err := rc.wrapIns(elemBytes, after)
		if err != nil {
			return Patch{}, err
		}
		out = append(out, ins...)
	}
	if suffix != "" {
		s, err := cloneRunWithText(elemBytes, suffix, false)
		if err != nil {
			return Patch{}, err
		}
		out = append(out, s...)
	}
	return PatchRawSpan(doc, run.Elem, string(out)), nil
}

// planRunTarget handles replace/delete when the edit names a specific Run.
func planRunTarget(doc []byte, e Edit, op string, para Para, matchers []protectMatcher, rc *revisionCtx) ([]Patch, string, string, string, string, string, bool) {
	if e.Run < 1 || e.Run > len(para.Runs) {
		return nil, "", "", "", fmt.Sprintf(
			"run %d is out of range: paragraph %d has %d runs", e.Run, para.Index, len(para.Runs)), "", false
	}
	run := para.Runs[e.Run-1]
	if run.SelfClosing {
		return nil, "", "", "", fmt.Sprintf(
			"run %d in paragraph %d is a self-closing <w:t/> with no text content to edit", e.Run, para.Index), "", false
	}
	before := run.Text
	target := fmt.Sprintf("run %d in paragraph %d", e.Run, para.Index)

	if op == opDelete {
		after := ""
		warning := deleteWarning(before, matchers)
		var patch Patch
		switch {
		case rc != nil:
			// Tracked mode always targets the whole <w:r> (run.Elem), via
			// trackedRunPatch's localStart=0/localEnd=len(before) call
			// shape: if this run's <w:r> is shared with a sibling Run (see
			// the non-tracked branch below), that Elem holds more than one
			// text-holding element, and cloneRunWithText refuses it — see
			// trackChangesReason — rather than guessing which text node to
			// wrap, so this can't repeat the C2 silent-text-loss defect
			// under a different name.
			p, err := trackedRunPatch(rc, doc, run, 0, len(before), before, "", false)
			if err != nil {
				return nil, before, after, "", trackChangesReason(err), "", false
			}
			patch = p
		case runElemSharedWithSibling(para.Runs, e.Run-1):
			// This run's <w:r> also produced at least one sibling Run
			// (scan.go: "If a single <w:r> contains multiple <w:t>
			// children, every run it produces shares the same Elem span"
			// — Word emits this routinely, e.g. text split by a <w:br/>).
			// Deleting the whole shared Elem would silently take every
			// sibling <w:t>'s text with it too, while Before above only
			// ever reported THIS run's text — exactly the "silent text
			// loss + lying audit trail" the C2 finding pins. Emptying just
			// this run's own <w:t> instead removes only what Before said
			// it would.
			patch = PatchRun(doc, run, "")
		default:
			patch = PatchRawSpan(doc, run.Elem, "")
		}
		return []Patch{patch}, before, after, warning, "", target, true
	}

	after := e.Text
	if broken := brokenProtectedItems(before, after, matchers); len(broken) > 0 {
		return nil, before, after, "", protectReason(broken), "", false
	}
	var patch Patch
	if rc != nil {
		p, err := trackedRunPatch(rc, doc, run, 0, len(before), before, after, true)
		if err != nil {
			return nil, before, after, "", trackChangesReason(err), "", false
		}
		patch = p
	} else {
		patch = PatchRun(doc, run, e.Text)
	}
	return []Patch{patch}, before, after, "", "", target, true
}

// planFindTarget handles replace/delete when the edit names a Find
// substring. It locates the (exactly-one, per the caller-side count check)
// match in the paragraph's concatenated run text and maps it back onto the
// single run that contains it.
//
// A match that straddles two runs' Text is refused rather than guessed at:
// realizing it would need coordinated patches across more than one <w:t>
// (and possibly reconstructing run boundaries/formatting in between), which
// is coordinated multi-patch editing left to P2. Saying so explicitly here,
// rather than silently editing only the first run's portion of the match,
// is the whole point of "never guess" in §4.2.
func planFindTarget(doc []byte, e Edit, op string, para Para, matchers []protectMatcher, rc *revisionCtx) ([]Patch, string, string, string, string, string, bool) {
	find := *e.Find // planEdit already refused a nil or explicitly-empty Find before dispatching here.
	text := outlineParaText(para)
	count := strings.Count(text, find)
	if count != 1 {
		return nil, "", "", "", fmt.Sprintf(
			"find %q matched %d times in paragraph %d; it must match exactly once (never guessed)", find, count, para.Index), "", false
	}

	matchStart := strings.Index(text, find)
	matchEnd := matchStart + len(find)

	runIdx := -1
	localStart, localEnd := 0, 0
	cum := 0
	for i, r := range para.Runs {
		runEnd := cum + len(r.Text)
		if matchStart >= cum && matchStart < runEnd {
			if matchEnd > runEnd {
				return nil, "", "", "", fmt.Sprintf(
					"find %q spans more than one run in paragraph %d; narrow the match to a single run's text or use Run instead — "+
						"cross-run replacement needs coordinated multi-patch editing, which is P2 work", find, para.Index), "", false
			}
			runIdx = i
			localStart = matchStart - cum
			localEnd = matchEnd - cum
			break
		}
		cum = runEnd
	}
	if runIdx == -1 {
		// Unreachable given count == 1 (the substring is confirmed present
		// somewhere in the concatenation), kept as a defensive refusal
		// instead of a panic/out-of-range slice access.
		return nil, "", "", "", fmt.Sprintf(
			"could not locate find %q within a single run of paragraph %d", find, para.Index), "", false
	}

	run := para.Runs[runIdx]
	if run.SelfClosing {
		return nil, "", "", "", fmt.Sprintf(
			"the run containing the match in paragraph %d is a self-closing <w:t/> with no text content to edit", para.Index), "", false
	}
	before := run.Text[localStart:localEnd]
	target := fmt.Sprintf("run %d in paragraph %d", runIdx+1, para.Index)

	if op == opDelete {
		after := ""
		warning := deleteWarning(before, matchers)
		var patch Patch
		if rc != nil {
			p, err := trackedRunPatch(rc, doc, run, localStart, localEnd, before, "", false)
			if err != nil {
				return nil, before, after, "", trackChangesReason(err), "", false
			}
			patch = p
		} else {
			newText := run.Text[:localStart] + run.Text[localEnd:]
			patch = PatchRun(doc, run, newText)
		}
		return []Patch{patch}, before, after, warning, "", target, true
	}

	after := e.Text
	if broken := brokenProtectedItems(before, after, matchers); len(broken) > 0 {
		return nil, before, after, "", protectReason(broken), "", false
	}
	var patch Patch
	if rc != nil {
		p, err := trackedRunPatch(rc, doc, run, localStart, localEnd, before, after, true)
		if err != nil {
			return nil, before, after, "", trackChangesReason(err), "", false
		}
		patch = p
	} else {
		newText := run.Text[:localStart] + e.Text + run.Text[localEnd:]
		patch = PatchRun(doc, run, newText)
	}
	return []Patch{patch}, before, after, "", "", target, true
}

// planParagraphTarget handles replace/delete when neither Run nor Find was
// given, i.e. the edit targets the whole paragraph. paras is the full
// pre-edit paragraph snapshot the batch was planned against; delete needs it
// to tell whether para is the only paragraph in its table cell (see the
// Cell branch below).
func planParagraphTarget(doc []byte, e Edit, op string, para Para, paras []Para, matchers []protectMatcher, rc *revisionCtx) ([]Patch, string, string, string, string, string, bool) {
	before := outlineParaText(para)
	target := fmt.Sprintf("paragraph %d", para.Index)

	if op == opDelete {
		after := ""
		warning := deleteWarning(before, matchers)
		if rc != nil {
			// Per the plan's "delete（整段）" row, tracked mode never removes
			// the paragraph itself — its runs are wrapped in <w:del> (text
			// converted to delText) and the paragraph MARK is flagged
			// deleted via markParagraph, but the <w:p> stays. That is also
			// why the table-cell "only paragraph in its cell" carve-out
			// below (which exists solely because REMOVING the paragraph
			// could leave an empty, schema-invalid <w:tc>) does not apply
			// here: nothing is removed, so ParaCountChanged stays false
			// regardless of table placement.
			newParaXML, err := trackedParagraphDelete(rc, doc, para)
			if err != nil {
				return nil, before, after, "", trackChangesReason(err), "", false
			}
			patch := PatchRawSpan(doc, para.Span, newParaXML)
			return []Patch{patch}, before, after, warning, "", target, true
		}
		if para.Cell != nil && isOnlyParaInCell(paras, para) {
			// A <w:tc> with no <w:p> at all is schema-invalid — Word
			// treats it as needing repair — even though it is well-formed
			// XML, so the well-formedness gate never catches it. §4.1
			// deliberately gives table paragraphs ordinary para_index
			// values, so a caller has no way to know this paragraph is its
			// cell's last one without this check. Leaving a single empty
			// <w:p/> keeps the table schema-valid while still clearing all
			// of the paragraph's own content, which is what the caller
			// asked for.
			warning = joinNotes(warning, fmt.Sprintf(
				"paragraph %d is the only paragraph in its table cell; left as an empty paragraph instead of being removed, because an empty <w:tc> is invalid OOXML",
				para.Index))
			patch := PatchRawSpan(doc, para.Span, "<w:p/>")
			return []Patch{patch}, before, after, warning, "", target, true
		}
		patch := PatchRawSpan(doc, para.Span, "")
		return []Patch{patch}, before, after, warning, "", target, true
	}

	after := e.Text
	if broken := brokenProtectedItems(before, after, matchers); len(broken) > 0 {
		return nil, before, after, "", protectReason(broken), "", false
	}
	if len(para.Runs) == 0 {
		return nil, before, after, "", fmt.Sprintf(
			"paragraph %d has no runs to anchor a whole-paragraph replace on", para.Index), "", false
	}

	first := para.Runs[0]
	if first.SelfClosing {
		return nil, before, after, "", fmt.Sprintf(
			"paragraph %d's first run is a self-closing <w:t/>; whole-paragraph replace needs a run with a text content model", para.Index), "", false
	}

	if rc != nil {
		patches, err := trackedParagraphReplace(rc, doc, para, e.Text)
		if err != nil {
			return nil, before, after, "", trackChangesReason(err), "", false
		}
		warning := ""
		if len(para.Runs) > 1 {
			warning = fmt.Sprintf(
				"paragraph %d has %d runs; whole-paragraph replace collapsed formatting to the first run's", para.Index, len(para.Runs))
		}
		return patches, before, after, warning, "", target, true
	}

	// Rewrite the first run's content in place (PatchRun), which keeps its
	// <w:rPr> formatting untouched, and clear every other run so none of
	// the paragraph's other text survives the replace. That is "formatting
	// collapses to the first run's": nothing about the first run's own
	// tags is touched, only its text content and every other run's text.
	patches := []Patch{PatchRun(doc, first, e.Text)}
	warning := ""
	if len(para.Runs) > 1 {
		last := para.Runs[len(para.Runs)-1]
		switch {
		case anyRunsShareElem(para.Runs):
			// At least one <w:r> in this paragraph produced more than one
			// Run (see scan.go's Elem doc comment), so Elem span boundaries
			// alone cannot tell "first run's <w:r>" apart from "a sibling
			// <w:t> in the very same <w:r>" — first.Elem.End can equal
			// last.Elem.End even though there is more text to clear (the
			// C2 finding's second scenario: "flat" + surviving "b").
			// Patching every other run's own <w:t> individually sidesteps
			// that entirely: each Run's Content span is always its own
			// <w:t>, shared Elem or not.
			for _, r := range para.Runs[1:] {
				if r.SelfClosing {
					continue // already contributes no text; nothing to clear
				}
				patches = append(patches, PatchRun(doc, r, ""))
			}
		case last.Elem.End > first.Elem.End:
			// The common case: every run after the first lives in its own,
			// disjoint <w:r>, so the whole tail can be removed in one raw
			// splice. That is only safe when the tail is itself tag-
			// balanced — if the first run sits inside a container (e.g.
			// <w:hyperlink>, <w:sdt>) that closes before the last run, the
			// tail slices through that container's closing tag instead
			// (the I2 finding). checkWellFormed (splice.go) parses just
			// this span standalone: a dangling close tag with no matching
			// open tag inside the span fails immediately, which is exactly
			// the shape that must be refused here rather than left to blow
			// up Apply's own well-formedness gate for the whole batch.
			tail := Span{Start: first.Elem.End, End: last.Elem.End}
			if wfErr := checkWellFormed(doc[tail.Start:tail.End]); wfErr != nil {
				return nil, before, after, "", fmt.Sprintf(
					"paragraph %d's runs are not all at the same nesting depth (e.g. the paragraph starts with a hyperlink or content control); "+
						"whole-paragraph replace cannot remove the trailing runs without breaking the surrounding XML structure — "+
						"target the individual runs with run or find instead", para.Index), "", false
			}
			patches = append(patches, PatchRawSpan(doc, tail, ""))
		}
		warning = fmt.Sprintf(
			"paragraph %d has %d runs; whole-paragraph replace collapsed formatting to the first run's", para.Index, len(para.Runs))
	}
	return patches, before, after, warning, "", target, true
}

// trackedParagraphDelete returns the whole paragraph's replacement XML for a
// tracked-changes whole-paragraph delete: every run's own <w:r> is wrapped
// in <w:del> (text converted to delText via wrapParagraphRunsInDel), and the
// paragraph mark itself is flagged deleted via markParagraph. Per the plan's
// "delete（整段）" row, the paragraph is never actually removed — accepting
// every one of these revisions in Word is what removes it — so callers must
// splice this over the WHOLE para.Span (not drop it), which is exactly why
// ParaCountChanged stays false for a tracked paragraph delete.
func trackedParagraphDelete(rc *revisionCtx, doc []byte, para Para) (string, error) {
	wrapped, err := wrapParagraphRunsInDel(rc, doc, para)
	if err != nil {
		return "", err
	}
	marked, err := rc.markParagraph(wrapped, false)
	if err != nil {
		return "", err
	}
	return string(marked), nil
}

// wrapParagraphRunsInDel returns a copy of para's whole <w:p>...</w:p> bytes
// with every distinct <w:r> element replaced by a <w:del> wrapping a clone
// of that run with its text converted to delText. Runs sharing one <w:r>
// (see scan.go's Elem doc comment) are deduplicated by comparing each run's
// Elem against only the PREVIOUS run's Elem: Scan always appends runs from
// the same <w:r> consecutively, so shared-Elem runs are always adjacent in
// para.Runs (the same assumption anyRunsShareElem relies on), making this a
// single ascending forward-copy pass — mirroring Apply's own splice loop,
// but scoped to just this one paragraph's bytes.
//
// Any run whose <w:r> holds more than one text-holding element makes
// wrapDel (via cloneRunWithText) return an error here — this is the same
// "never guess which text node to keep" refusal every other tracked-change
// path in this file relies on, surfaced by the caller as a per-edit Reason
// rather than a whole-batch failure.
func wrapParagraphRunsInDel(rc *revisionCtx, doc []byte, para Para) ([]byte, error) {
	out := make([]byte, 0, para.Span.End-para.Span.Start+64)
	cursor := para.Span.Start
	seenAny := false
	var lastElem Span
	for _, r := range para.Runs {
		if seenAny && r.Elem == lastElem {
			continue // already handled as part of the same <w:r>
		}
		seenAny = true
		lastElem = r.Elem

		out = append(out, doc[cursor:r.Elem.Start]...)
		del, err := rc.wrapDel(doc[r.Elem.Start:r.Elem.End], r.Text)
		if err != nil {
			return nil, err
		}
		out = append(out, del...)
		cursor = r.Elem.End
	}
	out = append(out, doc[cursor:para.Span.End]...)
	return out, nil
}

// trackedParagraphReplace returns one raw Patch per distinct <w:r> element in
// para (deduplicated the same way wrapParagraphRunsInDel is), realizing a
// tracked whole-paragraph replace: the FIRST run's <w:r> is replaced by
// <w:del>(its old text)</w:del><w:ins>(newText)</w:ins>, and every other
// run's <w:r> is replaced by just <w:del>(its old text)</w:del> — mirroring
// the untracked path's "rewrite the first run in place, clear every other
// run" collapse (see planParagraphTarget above), so accepting every
// revision this produces in Word reproduces exactly what the untracked
// replace would have written directly. Callers must check len(para.Runs) >
// 0 first (planParagraphTarget already refuses that case before reaching
// here).
func trackedParagraphReplace(rc *revisionCtx, doc []byte, para Para, newText string) ([]Patch, error) {
	firstElem := para.Runs[0].Elem
	var patches []Patch
	seenAny := false
	var lastElem Span
	for _, r := range para.Runs {
		if seenAny && r.Elem == lastElem {
			continue
		}
		seenAny = true
		lastElem = r.Elem

		elemBytes := doc[r.Elem.Start:r.Elem.End]
		del, err := rc.wrapDel(elemBytes, r.Text)
		if err != nil {
			return nil, err
		}
		combined := append([]byte{}, del...)
		if r.Elem == firstElem {
			ins, err := rc.wrapIns(elemBytes, newText)
			if err != nil {
				return nil, err
			}
			combined = append(combined, ins...)
		}
		patches = append(patches, PatchRawSpan(doc, r.Elem, string(combined)))
	}
	return patches, nil
}

// runElemSharedWithSibling reports whether para.Runs[idx].Elem equals some
// OTHER run's Elem in the same slice — i.e. whether idx's run came from a
// <w:r> that produced more than one Run (see scan.go's Elem doc comment).
func runElemSharedWithSibling(runs []Run, idx int) bool {
	e := runs[idx].Elem
	for i, r := range runs {
		if i != idx && r.Elem == e {
			return true
		}
	}
	return false
}

// anyRunsShareElem reports whether any two runs in the slice share an Elem
// span. Runs produced by the same <w:r> are always appended consecutively
// by Scan (every <w:t> child closes before its <w:r> does), so shared-Elem
// runs are always adjacent in the slice — an adjacent-pairs scan is
// therefore sufficient and O(n), rather than needing an O(n^2) all-pairs
// comparison.
func anyRunsShareElem(runs []Run) bool {
	for i := 1; i < len(runs); i++ {
		if runs[i].Elem == runs[i-1].Elem {
			return true
		}
	}
	return false
}

// isOnlyParaInCell reports whether target is the only paragraph in its
// table cell, among the full pre-edit paragraph snapshot paras. Callers
// only call this when target.Cell != nil.
func isOnlyParaInCell(paras []Para, target Para) bool {
	for _, p := range paras {
		if p.Index == target.Index {
			continue
		}
		if p.Cell != nil && *p.Cell == *target.Cell {
			return false
		}
	}
	return true
}

// joinNotes appends extra to base with "; " separating them, or returns
// extra alone when base is empty — used to combine a delete's protect
// Warning with its (independent) empty-cell-paragraph Warning without
// losing either.
func joinNotes(base, extra string) string {
	if base == "" {
		return extra
	}
	return base + "; " + extra
}

// documentText concatenates every paragraph's run text, in document order.
// planInsert consults it to tell a forged/mistyped protected item (never
// seen anywhere in the document) apart from a legitimate reference to one
// that already exists — see forgedProtectedItems.
func documentText(paras []Para) string {
	var b strings.Builder
	for _, p := range paras {
		b.WriteString(outlineParaText(p))
	}
	return b.String()
}

// forgedProtectedItems returns every protect-pattern match found in
// inserted that does NOT already occur anywhere in docText. Per §4.2,
// insert_before/insert_after's protect check "仅检查新插入的文本是否伪造/
// 篡改了保护项的写法" (only checks whether the newly inserted text forges or
// mistypes a protected item's spelling): a protect-pattern match that is
// genuinely present elsewhere in the document is not forged just because
// this insertion repeats it, but a match with no existing counterpart
// anywhere is exactly a forged or mistyped protected item (e.g. inserting
// "v9.9.9" when Protect names a version pattern but no "v9.9.9" exists
// anywhere in the document).
func forgedProtectedItems(inserted, docText string, matchers []protectMatcher) []string {
	if inserted == "" || len(matchers) == 0 {
		return nil
	}
	var broken []string
	seen := make(map[string]bool)
	for _, m := range matchers {
		for _, match := range m.re.FindAllString(inserted, -1) {
			if seen[match] || strings.Contains(docText, match) {
				continue
			}
			seen[match] = true
			broken = append(broken, match)
		}
	}
	return broken
}

// isLegalXMLChar reports whether r is a character XML 1.0 permits in
// document content (the Char production, https://www.w3.org/TR/xml/#charsets):
// tab, LF, CR, and everything from U+0020 up to U+10FFFF except the UTF-16
// surrogate range U+D800-U+DFFF (which cannot occur in a rune decoded from
// valid UTF-8 anyway, but is excluded for completeness) and U+FFFE/U+FFFF.
func isLegalXMLChar(r rune) bool {
	switch {
	case r == 0x9 || r == 0xA || r == 0xD:
		return true
	case r >= 0x20 && r <= 0xD7FF:
		return true
	case r >= 0xE000 && r <= 0xFFFD:
		return true
	case r >= 0x10000 && r <= 0x10FFFF:
		return true
	default:
		return false
	}
}

// firstIllegalXMLChar returns the first rune in s that isLegalXMLChar
// rejects, so planEdit can refuse a single edit with an actionable Reason
// instead of letting the character reach the document: Apply's escaping
// (splice.go's escapeXMLText) only escapes the five XML metacharacters, so
// an illegal character like a literal NUL sails through it unescaped, and a
// non-raw patch (an ordinary replace) is never well-formedness-checked by
// Apply at all — the corruption would only surface later, at rescan, by
// which point SetPart has already committed the bad bytes (see the C3
// finding).
func firstIllegalXMLChar(s string) (rune, bool) {
	for _, r := range s {
		if !isLegalXMLChar(r) {
			return r, true
		}
	}
	return 0, false
}

// collisionReason builds accepted's Reason for a LATER edit refused because
// its planned patch collides with an earlier, already-accepted edit
// (conflictIdx, conflictEdit; conflictTarget is conflictEdit's precomputed
// human-readable description). Per §4.2's off-limits collision policy
// (splice.go's Apply, via spansCollide's equal-starts rule — see
// Document.Edit's doc comment), a collision is refused regardless of cause;
// this function only chooses HOW to explain it.
//
// Two paragraphs' spans are exactly adjacent (p1.Span.End == p2.Span.Start),
// so insert_before/insert_after's zero-length anchor span collides with
// anything else that starts at that same boundary — another insert there, or
// an ordinary replace/delete on the neighbouring paragraph — even though
// e.g. "insert_after(1)" and "delete(2)" do not genuinely conflict (insert at
// the boundary, delete a disjoint range are both well-defined). The generic
// "combine them into a single edit" advice is actionable for a genuine
// same-run overlap (the two edits really can be merged), but not here: two
// different paragraphs can't be "combined into a single edit", and an insert
// plus a delete on the neighbour aren't overlapping content at all. When
// either colliding edit is an insert, this names the actual boundary and
// points at the working escape hatch instead: a separate docx_edit call, or
// delete-then-insert ordering (see Document.Edit's doc comment for why that
// ordering, not insert-then-delete, is the one that survives — deleting the
// LATER paragraph never renumbers the EARLIER one the insert anchors to).
func collisionReason(conflictIdx int, conflictTarget string, conflictEdit, thisEdit Edit) string {
	switch {
	case isInsertOp(conflictEdit.Op):
		x, xPlus1 := insertBoundary(conflictEdit.Op, conflictEdit.Para)
		return fmt.Sprintf(
			"edit %d already anchors an insertion at the same position (the boundary between paragraphs %d and %d); "+
				"issue it in a separate docx_edit call, or order it as delete on paragraph %d followed by insert_after on paragraph %d",
			conflictIdx+1, x, xPlus1, xPlus1, x)
	case isInsertOp(thisEdit.Op):
		x, xPlus1 := insertBoundary(thisEdit.Op, thisEdit.Para)
		return fmt.Sprintf(
			"edit %d already targets the exact position this insertion would anchor to (the boundary between paragraphs %d and %d); "+
				"issue it in a separate docx_edit call, or order it as delete on paragraph %d followed by insert_after on paragraph %d",
			conflictIdx+1, x, xPlus1, xPlus1, x)
	default:
		return fmt.Sprintf(
			"conflicts with edit %d, which targets %s; combine them into a single edit",
			conflictIdx+1, conflictTarget)
	}
}

// isInsertOp reports whether op is insert_before or insert_after — the only
// two ops whose planned patch anchors a zero-length span at a paragraph
// boundary (see planInsert).
func isInsertOp(op string) bool {
	return op == opInsertBefore || op == opInsertAfter
}

// insertBoundary returns the pair of paragraph indexes (x, x+1) flanking the
// zero-length anchor point an insert_before/insert_after targeting para
// produces: insert_after(N) anchors at the boundary between N and N+1;
// insert_before(N) anchors at the boundary between N-1 and N. Callers only
// call this when isInsertOp(op) is true.
func insertBoundary(op string, para int) (x, xPlus1 int) {
	if op == opInsertAfter {
		return para, para + 1
	}
	return para - 1, para
}

// patchesCollide reports whether any patch in a targets the same or an
// overlapping byte range as any patch in b, comparing each Patch's Content
// span — the meaningful span for both a PatchRun's <w:t> content and a
// PatchRawSpan's structural subtree.
func patchesCollide(a, b []Patch) bool {
	for _, pa := range a {
		for _, pb := range b {
			if spansCollide(pa.Content, pb.Content) {
				return true
			}
		}
	}
	return false
}

// spansCollide reports whether x and y target the same or an overlapping
// byte range, mirroring the two rules Apply itself enforces on Content
// spans (see splice.go's Apply): equal starts always collide — including
// two zero-length spans at the same offset, the shape insert_before/
// insert_after and the table-cell-safe empty-paragraph delete patches take
// — and otherwise the earlier-starting span's end must not reach into the
// later one. Checking this here, before Apply ever runs, is what lets
// Document.Edit refuse just the later of two colliding edits with a
// paragraph/run-level Reason, instead of failing Apply for the whole batch
// with a raw byte offset.
func spansCollide(x, y Span) bool {
	if x.Start == y.Start {
		return true
	}
	if x.Start > y.Start {
		x, y = y, x
	}
	return x.End > y.Start
}

// protectMatcher is one compiled EditOptions.Protect entry.
type protectMatcher struct {
	// re matches the protected item: either the user's pattern compiled as
	// a regular expression, or — when that fails — the same string matched
	// literally (see compileProtect).
	re *regexp.Regexp
}

// compileProtect compiles each pattern in patterns as a regular expression.
// A pattern that fails to compile (e.g. "(beta", an unclosed group) is not
// rejected or dropped: it is matched literally instead, via
// regexp.QuoteMeta, so a caller who passes a plain piece of text as a
// "protect" entry (which is the common case — most protected items are
// literal strings like a version number or a name, not intentional regexes)
// still gets useful protection instead of a confusing compile error.
func compileProtect(patterns []string) []protectMatcher {
	if len(patterns) == 0 {
		return nil
	}
	out := make([]protectMatcher, len(patterns))
	for i, p := range patterns {
		if re, err := regexp.Compile(p); err == nil {
			out[i] = protectMatcher{re: re}
			continue
		}
		out[i] = protectMatcher{re: regexp.MustCompile(regexp.QuoteMeta(p))}
	}
	return out
}

// brokenProtectedItems returns every protected-item match found in before
// that is no longer present anywhere in after, per §4.2: "every protected
// item present in Before must still be present in After". Presence in
// after is checked with a plain substring test (not a re-match), since the
// requirement is that the exact matched text survives, not that it still
// satisfies the pattern in some possibly-different form.
func brokenProtectedItems(before, after string, matchers []protectMatcher) []string {
	if before == "" || len(matchers) == 0 {
		return nil
	}
	var broken []string
	seen := make(map[string]bool)
	for _, m := range matchers {
		for _, match := range m.re.FindAllString(before, -1) {
			if strings.Contains(after, match) || seen[match] {
				continue
			}
			seen[match] = true
			broken = append(broken, match)
		}
	}
	return broken
}

// protectReason formats a refusal reason naming the broken protected
// item(s), per §4.2's "name the broken item" requirement.
func protectReason(broken []string) string {
	return fmt.Sprintf("edit would remove or alter protected item %q (protected items must survive the edit)", broken[0])
}

// deleteWarning implements §4.2's delete carve-out: delete never runs
// protect VALIDATION (there is nothing to validate against — After is
// always ""), but if the removed text contained something the caller asked
// to protect, that is worth a Warning even though the delete still applies.
func deleteWarning(removed string, matchers []protectMatcher) string {
	broken := brokenProtectedItems(removed, "", matchers)
	if len(broken) == 0 {
		return ""
	}
	return fmt.Sprintf("deleted text contained protected item(s): %s", strings.Join(broken, ", "))
}
