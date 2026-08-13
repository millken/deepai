package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/millken/deepai/pkg/docx"
	"github.com/millken/deepai/pkg/models"
)

// maxDocxResultBytes is the cap fitDocxReadResult and
// marshalDocxOutlineResult enforce on the marshalled JSON payload docx_read
// returns. Design §5.1 has deepai silently offload any tool result over 24
// KB (pkg/agent's offloadThresholdBytes, 24576 bytes), replacing it with
// "first 50 lines + last 50 lines" and no error — an oversized read does
// not fail loudly, it quietly drops the middle of the document while
// looking fine.
//
// This tool stays 4 KB under that 24576-byte line rather than riding right
// up against it. That margin is NOT because anything wraps this payload
// before the offload check: offloadIfNeeded (pkg/agent/toolexec.go)
// compares len(result.Content) against the threshold verbatim, with no
// added framing, so the bytes produced here are exactly the bytes that
// check sees. The margin exists anyway as plain safety slack — e.g. so a
// future change to offloadThresholdBytes or to this package's own JSON
// shape doesn't require immediately re-tuning this constant too. 20 KB is
// that margin.
const maxDocxResultBytes = 20 << 10

// maxDocxFitAttempts bounds fitDocxReadResult's shrink loop: the initial
// attempt at the caller's budget, plus at most 4 halvings.
const maxDocxFitAttempts = 5

// docxReadArgs is docx_read's parsed arguments.
type docxReadArgs struct {
	Path      string
	StartPara int
	EndPara   int
	Heading   string
	Full      bool
	Runs      bool
	MaxChars  int
}

// docxReadOutput is the JSON shape docx_read returns to the model.
type docxReadOutput struct {
	Markdown      string          `json:"markdown"`
	Paras         []docxParaIndex `json:"paragraphs,omitempty"`
	Outline       *docxOutline    `json:"outline,omitempty"`
	NextStartPara int             `json:"next_start_para"`
	RangeStart    int             `json:"range_start,omitempty"`
	RangeEnd      int             `json:"range_end,omitempty"`
	TotalParas    int             `json:"total_paras"`
	Notes         []string        `json:"notes,omitempty"`
}

// docxParaIndex is one paragraph's index and metadata, deliberately without
// its text: Markdown already carries every paragraph's (marker-neutralized)
// text under its "[para N]" marker, so repeating it here would both double
// the payload and reintroduce the marker-spoofing hazard that
// neutralization exists to prevent. Runs is populated only when the caller
// asked for runs=true, since only run-level edits need run text.
type docxParaIndex struct {
	Index int            `json:"index"`
	Style string         `json:"style,omitempty"`
	Cell  *docx.CellRef  `json:"cell,omitempty"`
	Runs  []docxRunIndex `json:"runs,omitempty"`
}

// docxRunIndex is one run's index and exact text, included only when the
// caller passed runs=true.
type docxRunIndex struct {
	Index int    `json:"index"`
	Text  string `json:"text"`
}

// docxOutline is the serialized form of docx.Outline.
type docxOutline struct {
	TotalParas int                  `json:"total_paras"`
	Words      int                  `json:"words"`
	Sections   []docxOutlineSection `json:"sections"`
}

// docxOutlineSection is the serialized form of docx.Section.
type docxOutlineSection struct {
	Heading   string `json:"heading,omitempty"`
	Level     int    `json:"level"`
	StartPara int    `json:"start_para"`
	EndPara   int    `json:"end_para"`
	Paras     int    `json:"paras"`
	Words     int    `json:"words"`
}

// DocxReadTool describes docx_read to the model.
func DocxReadTool() models.Tool {
	return models.Tool{
		Name:         "docx_read",
		Groups:       []string{"builtin", "document"},
		ParallelSafe: true,
		Description: "Read a .docx as structured content. Returns a heading outline by default for " +
			"large documents; pass heading or start_para/end_para for a section or range, or full=true " +
			"for the whole body. Large ranges are chunked: next_start_para is the cursor for the next " +
			"call, and 0 means the range is exhausted. Headers, footers, footnotes and text boxes are " +
			"not included and are declared in notes.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":       map[string]any{"type": "string", "description": "Path to the .docx file"},
				"heading":    map[string]any{"type": "string", "description": "Restrict to the section under this heading; mutually exclusive with start_para/end_para"},
				"start_para": map[string]any{"type": "number", "description": "1-based inclusive first paragraph"},
				"end_para":   map[string]any{"type": "number", "description": "1-based inclusive last paragraph"},
				"full":       map[string]any{"type": "boolean", "description": "Return the whole body; errors instead of chunking when it exceeds the budget"},
				"runs":       map[string]any{"type": "boolean", "description": "Include each paragraph's runs, needed to edit by run index"},
				"max_chars":  map[string]any{"type": "number", "description": "Body character budget for this chunk"},
			},
			"required": []any{"path"},
		},
		Handler: DocxReadHandler,
	}
}

// DocxReadHandler parses docx_read's arguments, delegates all document
// semantics (heading resolution, chunking, marker neutralization) to
// pkg/docx, and enforces the serialized-result size cap that pkg/docx has
// no visibility into.
func DocxReadHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	result := models.ToolResult{CallID: call.ID, ToolName: call.Name}

	args, err := parseDocxReadArgs(call.Arguments)
	if err != nil {
		return result, err
	}

	resolved := resolveReadablePath(ctx, args.Path)
	doc, err := docx.OpenDocument(resolved)
	if err != nil {
		return result, fmt.Errorf("docx_read: %w", err)
	}

	total := doc.TotalParas()
	if shouldReturnOutline(total, args) {
		payload, err := marshalDocxOutlineResult(doc.Outline(), total)
		if err != nil {
			return result, fmt.Errorf("docx_read: %w", err)
		}
		result.Status = models.CallStatusCompleted
		result.Content = string(payload)
		return result, nil
	}

	initialBudget := args.MaxChars
	if initialBudget <= 0 {
		initialBudget = docx.DefaultReadBudget
	}

	read := func(budget int) (docxReadOutput, error) {
		rr, err := doc.Read(docx.ReadOptions{
			StartPara: args.StartPara,
			EndPara:   args.EndPara,
			Heading:   args.Heading,
			Runs:      args.Runs,
			MaxChars:  budget,
			Full:      args.Full,
		})
		if err != nil {
			return docxReadOutput{}, err
		}
		return docxReadOutputFromResult(rr), nil
	}

	payload, err := fitDocxReadResult(read, initialBudget, args.Runs)
	if err != nil {
		return result, fmt.Errorf("docx_read: %w", err)
	}
	result.Status = models.CallStatusCompleted
	result.Content = string(payload)
	return result, nil
}

// parseDocxReadArgs extracts docxReadArgs from the raw JSON arguments map.
// It only converts types (JSON numbers decode as float64) and checks that
// path is present; every other default and mutual-exclusion rule is
// pkg/docx's to enforce.
func parseDocxReadArgs(raw map[string]any) (docxReadArgs, error) {
	path, _ := raw["path"].(string)
	path = strings.TrimSpace(path)
	if path == "" {
		return docxReadArgs{}, fmt.Errorf("docx_read: path is required")
	}

	var args docxReadArgs
	args.Path = path
	// heading/full/runs are type-checked the same way find already is
	// (finding 4 of the P1c review): a bare `raw["x"].(T)` type assertion
	// with the ", ok" form silently discards a wrong-typed value as the
	// zero value instead of erroring, so e.g. runs sent as the string
	// "true" reads as false with no diagnostic, and the model gets a
	// chunked read it believes was full=true.
	if v, present := raw["heading"]; present && v != nil {
		s, ok := v.(string)
		if !ok {
			return docxReadArgs{}, fmt.Errorf("docx_read: heading must be a string")
		}
		args.Heading = s
	}
	if v, present := raw["full"]; present && v != nil {
		b, ok := v.(bool)
		if !ok {
			return docxReadArgs{}, fmt.Errorf("docx_read: full must be a boolean")
		}
		args.Full = b
	}
	if v, present := raw["runs"]; present && v != nil {
		b, ok := v.(bool)
		if !ok {
			return docxReadArgs{}, fmt.Errorf("docx_read: runs must be a boolean")
		}
		args.Runs = b
	}

	var err error
	if args.StartPara, err = docxIntArg(raw, "start_para"); err != nil {
		return docxReadArgs{}, err
	}
	if args.EndPara, err = docxIntArg(raw, "end_para"); err != nil {
		return docxReadArgs{}, err
	}
	if args.MaxChars, err = docxIntArg(raw, "max_chars"); err != nil {
		return docxReadArgs{}, err
	}
	return args, nil
}

// docxIntArg reads key from raw as an int. JSON numbers decode as float64
// through encoding/json's default map[string]any handling, so that is the
// only numeric type accepted from a real tool call; a plain int is also
// accepted so callers constructing arguments in Go (as the tests do) work
// too. A missing or nil key is not an error: it reports 0, which every
// caller here treats as "not given".
func docxIntArg(raw map[string]any, key string) (int, error) {
	v, ok := raw[key]
	if !ok || v == nil {
		return 0, nil
	}
	switch n := v.(type) {
	case float64:
		return int(n), nil
	case int:
		return n, nil
	default:
		return 0, fmt.Errorf("docx_read: %s must be a number", key)
	}
}

// shouldReturnOutline implements §4.1's outline-by-default rule: with no
// heading, no explicit range, and no full=true, a document bigger than
// docx.DocxOutlineParaThreshold gets an outline instead of a wall of
// markdown the caller never asked to chunk through.
func shouldReturnOutline(total int, args docxReadArgs) bool {
	if args.Heading != "" || args.StartPara != 0 || args.EndPara != 0 || args.Full {
		return false
	}
	return total > docx.DocxOutlineParaThreshold
}

// buildDocxOutline converts a docx.Outline into its compact serialized
// form.
func buildDocxOutline(o docx.Outline) *docxOutline {
	sections := make([]docxOutlineSection, len(o.Sections))
	for i, s := range o.Sections {
		sections[i] = docxOutlineSection{
			Heading:   s.Heading,
			Level:     s.Level,
			StartPara: s.StartPara,
			EndPara:   s.EndPara,
			Paras:     s.Paras,
			Words:     s.Words,
		}
	}
	return &docxOutline{
		TotalParas: o.TotalParas,
		Words:      o.Words,
		Sections:   sections,
	}
}

// marshalDocxOutlineResult serializes an outline read result and enforces
// the same maxDocxResultBytes cap fitDocxReadResult applies to ranged reads
// (finding 1 of the P1c review: the outline branch used to marshal and
// return directly, with no cap check at all — exactly the path every large
// document takes, since shouldReturnOutline only fires above 200
// paragraphs).
//
// Unlike a ranged/chunked read, an outline cannot be shrunk by lowering a
// character budget: every section is one fixed-size JSON object, and there
// is no smaller unit to chunk by without losing sections outright. Rather
// than silently emit a level-truncated (and therefore lossy) outline, this
// returns an actionable error naming the actual size and pointing at the
// tool's other read modes — consistent with fitDocxReadResult's own choice
// to error rather than truncate when a ranged read can't be shrunk to fit
// either (see its doc comment).
func marshalDocxOutlineResult(outline docx.Outline, total int) ([]byte, error) {
	out := docxReadOutput{
		Outline:    buildDocxOutline(outline),
		TotalParas: total,
		Notes:      outline.Notes,
	}
	payload, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal outline: %w", err)
	}
	if len(payload) > maxDocxResultBytes {
		return nil, fmt.Errorf(
			// Deliberately does NOT suggest the heading parameter: the caller
			// only learns a document's heading names FROM the outline, so at
			// the moment this error fires that lever is unavailable. Paging
			// with start_para/end_para always works, and the headings become
			// visible in the returned markdown as the caller pages through.
			"the outline is %d bytes, over the %d-byte cap; page through the document with start_para/end_para instead (headings appear in the returned markdown)",
			len(payload), maxDocxResultBytes)
	}
	return payload, nil
}

// docxReadOutputFromResult converts a docx.ReadResult into the JSON shape
// docx_read returns, dropping ParaView.Text (see docxParaIndex) and
// including run text only when pkg/docx populated it (i.e. only when the
// caller asked for runs=true).
func docxReadOutputFromResult(rr docx.ReadResult) docxReadOutput {
	paras := make([]docxParaIndex, len(rr.Paras))
	for i, pv := range rr.Paras {
		paras[i] = docxParaIndexFromView(pv)
	}
	return docxReadOutput{
		Markdown:      rr.Markdown,
		Paras:         paras,
		NextStartPara: rr.NextStartPara,
		RangeStart:    rr.RangeStart,
		RangeEnd:      rr.RangeEnd,
		TotalParas:    rr.TotalParas,
		Notes:         rr.Notes,
	}
}

// docxParaIndexFromView converts one docx.ParaView, carrying its runs only
// when there are any to carry — which pkg/docx.Read only populates when
// ReadOptions.Runs was true.
func docxParaIndexFromView(pv docx.ParaView) docxParaIndex {
	out := docxParaIndex{
		Index: pv.Index,
		Style: pv.Style,
		Cell:  pv.Cell,
	}
	if len(pv.Runs) > 0 {
		out.Runs = make([]docxRunIndex, len(pv.Runs))
		for i, r := range pv.Runs {
			out.Runs[i] = docxRunIndex{Index: r.Index, Text: r.Text}
		}
	}
	return out
}

// fitDocxReadResult calls read at budget, marshals the result, and — if the
// marshalled payload exceeds maxDocxResultBytes — halves the budget and
// retries, up to maxDocxFitAttempts total attempts. It never truncates a
// result to make it fit: a body that is still over the cap at the smallest
// attempted budget is reported as an error instead, since silently
// dropping content is exactly the failure design §5.1 exists to prevent.
// Every attempt after the first appends a note to the result explaining
// that it was shrunk, so the caller is told it received less than it
// asked for.
//
// runs is args.Runs from the original call: the terminal error (finding 3
// of the P1c review) can only fire when a single paragraph's rendered block
// alone exceeds every attempted budget, in which case pkg/docx returns that
// paragraph whole every time regardless of budget — halving budget again
// changes nothing, so "retry with a smaller max_chars" is not just
// unhelpful there, it is actively wrong advice that a compliant caller
// would loop forever on. Dropping runs=true is the one lever that actually
// shrinks that paragraph's rendered size (each run's own JSON overhead), so
// the error says that instead whenever runs was set.
func fitDocxReadResult(read func(budget int) (docxReadOutput, error), budget int, runs bool) ([]byte, error) {
	if budget <= 0 {
		budget = docx.DefaultReadBudget
	}

	var lastNotes []string
	for attempt := 0; attempt < maxDocxFitAttempts; attempt++ {
		out, err := read(budget)
		if err != nil {
			return nil, err
		}
		if attempt > 0 {
			out.Notes = append(out.Notes, fmt.Sprintf(
				"the result exceeded the %d-byte tool result cap and was shrunk to a %d-character body budget; "+
					"pass a smaller max_chars or narrow the range to get the rest",
				maxDocxResultBytes, budget))
		}
		lastNotes = out.Notes

		payload, err := json.Marshal(out)
		if err != nil {
			return nil, fmt.Errorf("marshal docx_read result: %w", err)
		}
		if len(payload) <= maxDocxResultBytes {
			return payload, nil
		}
		if attempt == maxDocxFitAttempts-1 {
			advice := "retry with a smaller max_chars, or narrow the range with heading or start_para/end_para"
			if runs {
				advice = "retry without runs=true — that is the one lever that actually shrinks this payload; a smaller max_chars or a narrower range will not, since pkg/docx returns an over-budget paragraph whole regardless of budget"
			}
			diag := ""
			if len(lastNotes) > 0 {
				diag = fmt.Sprintf(" (pkg/docx reported: %s)", strings.Join(lastNotes, "; "))
			}
			return nil, fmt.Errorf(
				"docx_read: result is %d bytes even at a %d-character body budget, over the %d-byte cap%s; %s",
				len(payload), budget, maxDocxResultBytes, diag, advice)
		}
		budget /= 2
		if budget < 1 {
			budget = 1
		}
	}
	// Unreachable: the loop above always returns on its last iteration.
	return nil, fmt.Errorf("docx_read: shrink loop exhausted without resolving")
}

// docxIndexAdvice is appended to docxEditOutput.IndexAdvice whenever a batch
// changed the document's paragraph count (design §5.4): insert_before,
// insert_after, and a whole-paragraph delete all shift every later
// paragraph's index, so a caller holding indices from an earlier docx_read
// needs to be told they are stale before it issues another edit against
// them. It is deliberately omitted (see DocxEditHandler) when the count did
// not change, since an ordinary replace-only polish batch never shifts
// anything and telling the model to re-read anyway would waste a docx_read
// round trip on every such batch.
const docxIndexAdvice = "the paragraph count changed; paragraph indices from any earlier read are now stale — re-read the outline or range before issuing further edits"

// docxEditArgs is docx_edit's parsed arguments.
type docxEditArgs struct {
	Path                string
	Edits               []docx.Edit
	Protect             []string
	ReviewedThroughPara int
	TrackChanges        bool
	Author              string
}

// docxEditOutput is the JSON shape docx_edit returns to the model.
type docxEditOutput struct {
	Applied          int               `json:"applied"`
	Outcomes         []docxEditOutcome `json:"outcomes"`
	TotalParas       int               `json:"total_paras"`
	ParaCountChanged bool              `json:"para_count_changed"`
	IndexAdvice      string            `json:"index_advice,omitempty"`
	// Notes carries call-level caveats that apply to the whole batch rather
	// than one outcome — currently only docxStaleBackupNote, when this
	// call's backup turned out to be one that already existed. Mirrors
	// docxFormatOutput's own Notes field.
	Notes      []string `json:"notes,omitempty"`
	BackupPath string   `json:"backup_path,omitempty"`
	// BackupCreated distinguishes a backup this call just created from one
	// that already existed from an earlier call (finding 8 of the P1c
	// review): BackupPath alone can't tell those apart, so a second session
	// would be told an identical backup_path both times, and the skill's
	// reporting step would call it "the rollback path" when the user would
	// actually roll back past an earlier accepted run, not just this one.
	// Deliberately not omitempty: false is a meaningful, distinct answer
	// (pre-existing backup) from omitting the field entirely (no backup was
	// involved because nothing was modified, in which case BackupPath is
	// also empty).
	BackupCreated       bool `json:"backup_created"`
	ReviewedThroughPara int  `json:"reviewed_through_para,omitempty"`
	// TrackChanges echoes whether this batch was written as Word tracked
	// changes (w:ins/w:del) rather than applied directly. It is always
	// present, never omitempty: false is exactly as meaningful an answer as
	// true — a model must be able to read this field and truthfully tell
	// the user either "the changes are pending your review in Word" or "the
	// changes were applied directly", never guess from the field's absence.
	//
	// This mirrors the request verbatim rather than something pkg/docx
	// computed post hoc, because "requested" and "took effect" cannot
	// diverge in the current design: pkg/docx.Document.Edit applies
	// TrackChanges uniformly to every patch in the batch (see EditOptions.
	// TrackChanges's doc comment) or refuses the ENTIRE call before
	// producing any result at all (the hadRevisionsAtOpen gate) — there is
	// no partial-tracking outcome for this field to distinguish. If
	// pkg/docx ever grows one (e.g. per-edit tracking), this field must
	// change to report the actual outcome, not the request.
	TrackChanges bool `json:"track_changes"`
}

// docxEditOutcome is the serialized form of one docx.EditOutcome, reporting
// what happened to a single requested edit within the batch.
type docxEditOutcome struct {
	Para    int    `json:"para"`
	Applied bool   `json:"applied"`
	Before  string `json:"before,omitempty"`
	After   string `json:"after,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Warning string `json:"warning,omitempty"`
}

// DocxEditTool describes docx_edit to the model. style is deliberately not
// declared here: design §4.2 defers paragraph styling to docx_format (P2),
// and DocxEditHandler returns an explicit error naming docx_format if any
// edit object carries it, rather than silently dropping it.
func DocxEditTool() models.Tool {
	return models.Tool{
		Name:         "docx_edit",
		Groups:       []string{"builtin", "document"},
		ParallelSafe: false,
		Description: "Edit a .docx in place with byte-faithful, format-preserving patches. Each edit targets a " +
			"paragraph, optionally narrowed to a run index or a literal find substring, and applies replace " +
			"(default), insert_before, insert_after, or delete. A refused edit does not block the rest of the " +
			"batch. Pass track_changes to write every edit in the batch as a Word tracked-change revision instead " +
			"of rewriting text directly. Refuses, before applying anything, only when the document already " +
			"contains unreviewed revision marks from a DIFFERENT author than this call's own author (see the " +
			"author argument) — your own earlier revisions under the same author never block a later call, so a " +
			"multi-call chunked polish keeps working as long as every call in the round uses the same author. " +
			"Backs up the original file once, before the first overwrite, to <path>.bak. This tool does not touch " +
			"fonts, size, spacing, or alignment — for that, including changing just one paragraph's font size, " +
			"call docx_format with start_para/end_para set to the paragraph range to change.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Path to the .docx file"},
				"edits": map[string]any{
					"type":        "array",
					"description": "Edits to apply in one batch; a refused edit does not block the rest",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"para": map[string]any{"type": "number", "description": "1-based paragraph index"},
							"run":  map[string]any{"type": "number", "description": "1-based run index within the paragraph; mutually exclusive with find"},
							"find": map[string]any{"type": "string", "description": "Literal substring to locate within the paragraph's text; mutually exclusive with run"},
							"text": map[string]any{"type": "string", "description": "Replacement or inserted text; ignored by delete"},
							"op":   map[string]any{"type": "string", "description": "replace (default), insert_before, insert_after, or delete"},
						},
						"required": []any{"para"},
					},
				},
				"protect": map[string]any{
					"type":  "array",
					"items": map[string]any{"type": "string"},
					"description": "Regex or literal patterns that must survive every edit touching them. Two ops check this " +
						"differently: delete is never refused for removing a protected item — it still applies, and only adds " +
						"a warning that the removed text matched one; insert_before/insert_after has no \"before\" to compare " +
						"against, so it is refused only when the inserted text itself matches a pattern in a form (e.g. a " +
						"version number) that does not already appear anywhere else in the document — i.e. it looks forged or " +
						"mistyped.",
				},
				"reviewed_through_para": map[string]any{
					"type":        "number",
					"description": "Echoed back verbatim. Set this to the end_para of the chunk you just wrote back, so a resumed session (e.g. a fresh subagent picking up after this one ran out of turns) can tell how far this batch reviewed",
				},
				"track_changes": map[string]any{
					"type": "boolean",
					"description": "When true, every edit in this batch lands as a Word revision (w:ins/w:del) instead of being written " +
						"directly — the user opens the document in Word and accepts or rejects each change in the review pane; " +
						"nothing is silently finalized. Defaults to false (direct edit). Refuses, before applying anything, when the " +
						"document already contains unreviewed revisions from a different author than this call's author (see the " +
						"author argument) — accept or reject those in Word first, or retry with author set to match them once the " +
						"user has explicitly confirmed that is fine.",
				},
				"author": map[string]any{
					"type": "string",
					"description": "The reviewer name stamped as w:author on every revision this batch produces, AND compared against " +
						"any revisions already in the document to decide whether to refuse the call — that comparison runs even " +
						"when track_changes is false. Defaults to \"deepai\" when omitted, empty, or whitespace-only. Use the exact " +
						"same author on every docx_edit call within one editing round, the same way you repeat protect on every " +
						"call: switching authors mid-round makes the next call look like someone else's unreviewed work and " +
						"triggers the same refusal.",
				},
			},
			"required": []any{"path", "edits"},
		},
		Handler: DocxEditHandler,
	}
}

// docxFormatOutput is the JSON shape docx_format returns to the model.
type docxFormatOutput struct {
	// Applied is always present, even when empty, so an empty rules object
	// (or one whose fields matched nothing to change) is reported the same
	// explicit way a real change is: as data the model can read, not as a
	// response shape it has to infer "nothing happened" from.
	Applied []string `json:"applied"`
	Notes   []string `json:"notes,omitempty"`
	// TotalParas/ParaCountChanged/IndexAdvice mirror docxEditOutput's own
	// fields exactly (task 10 brief, item 1 / seams review C2): normalize is
	// the only rule that can delete paragraphs, but a caller cannot tell
	// that from applied/notes alone -- an earlier read→normalize→edit
	// sequence would otherwise silently target the wrong paragraph by index,
	// with the edit itself reporting applied:true. Always present (like
	// docx_edit's own TotalParas), so a caller can validate a range it is
	// about to request next regardless of whether ParaCountChanged is true.
	TotalParas       int    `json:"total_paras"`
	ParaCountChanged bool   `json:"para_count_changed"`
	IndexAdvice      string `json:"index_advice,omitempty"`
	// BackupPath/BackupCreated mirror docxEditOutput's fields exactly (see
	// its doc comment): BackupPath alone can't distinguish a backup this
	// call just created from one an earlier call already made.
	BackupPath    string `json:"backup_path,omitempty"`
	BackupCreated bool   `json:"backup_created"`
}

// docxFormatNoChangeNote is appended to docxFormatOutput.Notes whenever
// Applied comes back empty, so an empty rules object (or one whose fields
// all matched nothing to change) is never indistinguishable from a call
// that silently did nothing by mistake.
const docxFormatNoChangeNote = "no formatting changes were applied: either no rules were given, or none of the given rules changed anything in this document"

// docxStaleBackupNote is appended to a docx_edit/docx_format result's notes
// whenever BackupPath is set but BackupCreated is false, i.e. this call
// found a pre-existing <path>.bak rather than writing a fresh one (I3 of the
// docx capability review): backupDocxOnce only checks whether a file
// already sits at that path, never whether it actually belongs to the
// document currently open at path. A caller pointed at the wrong document
// (or one restored/copied from elsewhere under the same name) would
// otherwise be told "backup_path is your rollback point" about a .bak that
// silently holds a completely different document's content — reporting
// backup_created:false with no comment leaves that risk for the caller to
// discover only after already trusting the rollback.
const docxStaleBackupNote = "pre-existing backup file; verify it belongs to this document before using it for rollback"

// DocxFormatTool describes docx_format to the model. Without start_para/
// end_para it applies document-wide formatting (fonts, size, line spacing,
// alignment, margins, a named template, and collapsing empty paragraphs) by
// changing word/styles.xml's defaults; with a range it applies DIRECT
// formatting (font, size, line spacing, alignment) to only those
// paragraphs' own <w:rPr>/<w:pPr> in word/document.xml, which is how one
// paragraph's font size gets changed without touching the rest of the
// document — see pkg/docx.Document.Format's doc comment for the byte-range
// promise that backs both paths, and formatDirectRange's for which fields
// only make sense document-wide and are refused when combined with a range.
// page_numbers is one of those document-wide-only fields (it can add new
// package parts and a section-level footerReference, neither of which is a
// paragraph's own formatting) — see DocxFormatHandler's doc comment.
// rebuild_toc, unlike page_numbers, is named in the schema but always
// refused regardless of range: see DocxFormatHandler's doc comment for why.
func DocxFormatTool() models.Tool {
	return models.Tool{
		Name:         "docx_format",
		Groups:       []string{"builtin", "document"},
		ParallelSafe: false,
		Description: "Apply formatting to a .docx. Two modes, chosen by whether start_para/end_para is given: " +
			"(1) WITHOUT a range (the default): changes the document's DEFAULT styles — a named template " +
			"(corporate, academic, minimal), heading/body/east-asia font, body size, line spacing (multiple or " +
			"exact points), alignment (left/center/right/justify), first-line indent, space before/after, " +
			"margins, and collapsing runs of consecutive empty paragraphs. This is document-wide: every " +
			"paragraph that does not already carry its own direct formatting picks up the new default. " +
			"(2) WITH start_para/end_para (1-based, inclusive): applies DIRECT formatting — the same font/size/" +
			"spacing/alignment/indent fields — to only the paragraphs in that range, which overrides the " +
			"document's default styles for exactly those paragraphs. This is the way to change one paragraph's " +
			"(or a few paragraphs') formatting without reformatting the rest of the document — use this instead " +
			"of editing the file with a script. template, heading_font, margins_mm, and normalize are " +
			"document-level concepts and are refused with an explicit error if combined with a range. " +
			"line_spacing and line_spacing_exact_pt are mutually exclusive — giving both is an error, on either " +
			"path. Every rule except normalize only changes formatting, never body text. normalize is the one " +
			"exception: it deletes runs of consecutive empty paragraphs, which shifts every later paragraph's " +
			"index the same way an insert/delete through docx_edit does — the response's total_paras/" +
			"para_count_changed/index_advice fields report this exactly like docx_edit's own, so re-read the " +
			"outline or range before issuing further paragraph-indexed calls whenever para_count_changed is " +
			"true. Reports which rules actually changed something in applied — including how many paragraphs " +
			"were affected when a range was used — with an empty or no-op rules object, or a rule whose target " +
			"already matched (reported in notes as \"already ...\" instead), saying so explicitly rather than " +
			"looking identical to a real change; a call that changed nothing at all does not rewrite the file or " +
			"touch the backup. page_numbers adds a centered page-number footer to a document that has none — see " +
			"its own schema description for the no-op case where one already exists. rebuild_toc is not supported " +
			"and returns an explicit error rather than being silently ignored, with or without a range. Backs up " +
			"the original file once, before the first overwrite, to " +
			"<path>.bak.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Path to the .docx file"},
				"start_para": map[string]any{
					"type": "number",
					"description": "1-based inclusive first paragraph to apply DIRECT formatting to instead of " +
						"changing the document's defaults. Get paragraph indices from docx_read. Omit both " +
						"start_para and end_para to format the whole document instead.",
				},
				"end_para": map[string]any{
					"type": "number",
					"description": "1-based inclusive last paragraph of the range; defaults to start_para (a " +
						"single paragraph) when omitted. Requires start_para to also be set.",
				},
				"rules": map[string]any{
					"type":        "object",
					"description": "Formatting rules to apply. Every field is optional; an empty or omitted object is a no-op.",
					"properties": map[string]any{
						"template":     map[string]any{"type": "string", "description": "Named preset: corporate, academic, or minimal. Explicit fields below override the preset's values."},
						"heading_font": map[string]any{"type": "string", "description": "Replaces Heading1-9's font (ascii/hAnsi only; combine with body_east_asia_font in the same call to also set the heading's CJK font)"},
						"body_font":    map[string]any{"type": "string", "description": "Replaces the document's default LATIN font (ascii/hAnsi only). Orthogonal to body_east_asia_font — an existing CJK font survives untouched unless body_east_asia_font is also given."},
						"body_east_asia_font": map[string]any{
							"type":        "string",
							"description": "Replaces the document's default EAST ASIAN font only (w:eastAsia). Independent of body_font/heading_font — set it alongside either to change both the Latin and CJK font pair in one call, or alone to change only the CJK font.",
						},
						"body_size_pt": map[string]any{"type": "number", "description": "Replaces the document's default font size, in points"},
						"line_spacing": map[string]any{
							"type":        "number",
							"description": "Line spacing as a multiple of a single line (1.0, 1.15, 2.0, ...); w:lineRule=\"auto\". Mutually exclusive with line_spacing_exact_pt — giving both is an error.",
						},
						"line_spacing_exact_pt": map[string]any{
							"type":        "number",
							"description": "Fixed line height in points (w:line = pt*20 twips, w:lineRule=\"exact\"), instead of a multiple of a line. Mutually exclusive with line_spacing — giving both is an error. Must be > 0; omit the field to leave it unchanged (0 is rejected).",
						},
						"align": map[string]any{"type": "string", "description": "One of: left, center, right, justify"},
						"first_line_indent_chars": map[string]any{
							"type":        "number",
							"description": "First-line indent measured in CHARACTER widths (2 is the conventional opening indent for a Chinese paragraph). Writes w:ind w:firstLineChars=n*100 (hundredths of a character, what Word renders relative to the current font size) plus a fixed w:firstLine twips fallback for readers that ignore firstLineChars. Must be > 0; omit the field to leave it unchanged (0 is rejected).",
						},
						"space_before_pt": map[string]any{"type": "number", "description": "Paragraph spacing before, in points (w:spacing w:before = pt*20 twips). Lands on the same element as line_spacing/line_spacing_exact_pt/space_after_pt. Must be > 0; omit the field to leave it unchanged (0 is rejected)."},
						"space_after_pt":  map[string]any{"type": "number", "description": "Paragraph spacing after, in points (w:spacing w:after = pt*20 twips). Lands on the same element as line_spacing/line_spacing_exact_pt/space_before_pt. Must be > 0; omit the field to leave it unchanged (0 is rejected)."},
						"margins_mm": map[string]any{
							"type":        "array",
							"items":       map[string]any{"type": "number"},
							"description": "Exactly 4 positive values in millimeters: top, right, bottom, left",
						},
						"normalize": map[string]any{"type": "boolean", "description": "Collapse runs of two or more consecutive empty paragraphs down to one"},
						"page_numbers": map[string]any{
							"type": "boolean",
							"description": "Add a centered page-number footer. If the document already has ANY footer, this is a no-op (reported in notes as " +
								"\"document already has a footer; not modified\") rather than adding a second one or touching the existing footer. Cannot be combined with a paragraph range.",
						},
						"rebuild_toc": map[string]any{"type": "boolean", "description": "Not supported; setting this true returns an error instead of being ignored. Rebuilding a table of contents needs repagination, which this pure-Go tool cannot do."},
					},
				},
			},
			"required": []any{"path"},
		},
		Handler: DocxFormatHandler,
	}
}

// DocxFormatHandler parses docx_format's arguments, delegates all
// formatting semantics to pkg/docx.Document.Format, and owns the two
// responsibilities pkg/docx deliberately leaves to the tool layer: backing
// up the file once before the first overwrite (reusing backupDocxOnce), and
// surfacing FormatResult.Applied/Notes so a caller can tell an empty or
// no-op rules object apart from one that actually changed the document.
//
// page_numbers used to be refused outright here too, on the theory that it
// needed a multi-part write pkg/docx could not do (a new word/footerN.xml
// entry plus sectPr/footerReference plus a [Content_Types].xml declaration
// plus a rels entry) — task 12 closed that gap: pkg/docx.Package now has
// AddPart for the one genuinely missing piece (a brand-new zip entry), and
// docx.FormatOptions.PageNumbers (Document.Format) does the rest, so
// page_numbers is parsed as an ordinary field below instead of being
// refused. rebuild_toc is still refused outright (requireNotRequested,
// below): a table of contents needs actual repagination — recomputing
// which heading lands on which printed page — which needs a rendering
// engine such as LibreOffice, not wired in here.
func DocxFormatHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	result := models.ToolResult{CallID: call.ID, ToolName: call.Name}

	args, err := parseDocxFormatArgs(call.Arguments)
	if err != nil {
		return result, err
	}

	resolved := resolveWritablePath(ctx, args.Path)
	doc, err := docx.OpenDocument(resolved)
	if err != nil {
		return result, fmt.Errorf("docx_format: %w", err)
	}

	formatResult, err := doc.Format(args.Opts)
	if err != nil {
		return result, fmt.Errorf("docx_format: %w", err)
	}

	// Only a call that actually changed something reaches disk, the same
	// gate docx_edit uses: an empty (or fully no-op) rules object leaves
	// doc.Modified() false, so nothing is written and no backup is made.
	// The backup is written BEFORE Save for the same reason docx_edit's is:
	// if Save then fails partway, the pristine original is already safely
	// copied aside.
	var backupPath string
	var backupCreated bool
	if doc.Modified() {
		backupPath, backupCreated, err = backupDocxOnce(resolved)
		if err != nil {
			return result, fmt.Errorf("docx_format: back up %s before saving: %w", resolved, err)
		}
		if err := doc.Save(); err != nil {
			return result, fmt.Errorf("docx_format: save %s: %w", resolved, err)
		}
	}

	out := docxFormatOutput{
		Applied:          formatResult.Applied,
		Notes:            formatResult.Notes,
		TotalParas:       formatResult.TotalParas,
		ParaCountChanged: formatResult.ParaCountChanged,
		BackupPath:       backupPath,
		BackupCreated:    backupCreated,
	}
	if backupPath != "" && !backupCreated {
		out.Notes = append(out.Notes, docxStaleBackupNote)
	}
	if formatResult.ParaCountChanged {
		out.IndexAdvice = docxIndexAdvice
	}
	if out.Applied == nil {
		out.Applied = []string{}
	}
	if len(out.Applied) == 0 {
		out.Notes = append(out.Notes, docxFormatNoChangeNote)
	}

	payload, err := json.Marshal(out)
	if err != nil {
		return result, fmt.Errorf("docx_format: marshal result: %w", err)
	}
	result.Status = models.CallStatusCompleted
	result.Content = string(payload)
	return result, nil
}

// docxFormatArgs is docx_format's parsed arguments.
type docxFormatArgs struct {
	Path string
	Opts docx.FormatOptions
}

// parseDocxFormatArgs extracts docxFormatArgs from the raw JSON arguments
// map. Like parseDocxEditArgs, it only converts and type-checks: every
// domain rule pkg/docx has an opinion on (unknown template name, alignment
// value, whether a document already has a footer) is left for
// Document.Format to enforce. rebuild_toc is the one field still refused
// here outright, since FormatOptions has no field for it at all (a table of
// contents needs actual repagination, which this pure-Go package cannot do
// — see DocxFormatHandler's doc comment). page_numbers, as of task 12, is a
// real FormatOptions field (docx.FormatOptions.PageNumbers) parsed just
// like normalize below, no longer refused here.
func parseDocxFormatArgs(raw map[string]any) (docxFormatArgs, error) {
	path, _ := raw["path"].(string)
	path = strings.TrimSpace(path)
	if path == "" {
		return docxFormatArgs{}, fmt.Errorf("docx_format: path is required")
	}

	var rules map[string]any
	if v, present := raw["rules"]; present && v != nil {
		m, ok := v.(map[string]any)
		if !ok {
			return docxFormatArgs{}, fmt.Errorf("docx_format: rules must be an object")
		}
		rules = m
	}

	opts, err := parseDocxFormatRules(rules)
	if err != nil {
		return docxFormatArgs{}, err
	}

	// start_para/end_para are top-level, siblings of rules (mirroring
	// docx_read's own start_para/end_para), because they name a SCOPE for
	// the rules to land in, not a rule themselves. Type-checked the same
	// never-coerce way as every other numeric docx_format field (brief's
	// explicit requirement): a string "2" must be refused outright, not
	// silently read as 0 — which would turn a one-paragraph request into a
	// document-wide rewrite with no error at all.
	startPara, err := docxFormatIntArg(raw, "start_para")
	if err != nil {
		return docxFormatArgs{}, err
	}
	endPara, err := docxFormatIntArg(raw, "end_para")
	if err != nil {
		return docxFormatArgs{}, err
	}
	opts.StartPara = startPara
	opts.EndPara = endPara

	return docxFormatArgs{Path: path, Opts: opts}, nil
}

// docxFormatIntArg reads key from raw as an int, exactly like
// docxEditNumberArg's float64/int handling, but with an error message
// scoped to docx_format. A missing or nil key reports 0, which
// docx.FormatOptions treats as "not given" for both StartPara and EndPara.
func docxFormatIntArg(raw map[string]any, key string) (int, error) {
	v, ok := raw[key]
	if !ok || v == nil {
		return 0, nil
	}
	switch n := v.(type) {
	case float64:
		return int(n), nil
	case int:
		return n, nil
	default:
		return 0, fmt.Errorf("docx_format: %s must be a number", key)
	}
}

// parseDocxFormatRules converts one raw rules object into a
// docx.FormatOptions, type-checking every field the same way
// parseDocxEditItem already does for docx_edit (finding 4 of the P1c
// review: a bare type assertion silently coerces a wrong-typed value to
// its zero value instead of erroring, which for a formatting call would
// mean e.g. body_size_pt sent as "big" silently becoming 0 — "leave the
// size alone" — while the call still reports success).
//
// rebuild_toc is checked and refused here, before anything is applied:
// design §4.3 requires an explicit error, not a silently dropped parameter,
// and refusing before Format is called means a batch that also asked for
// legitimate changes (e.g. body_font) never applies any of them either —
// the caller gets one unambiguous outcome (an error identical to what would
// happen alone) rather than a partial apply it would have to reconcile
// against what it asked for. page_numbers, as of task 12, is no longer in
// this category: it is parsed into opts.PageNumbers below like any other
// field, and Document.Format decides what to do with it (add a footer, or
// no-op with a note if one already exists).
func parseDocxFormatRules(raw map[string]any) (docx.FormatOptions, error) {
	var opts docx.FormatOptions
	if raw == nil {
		return opts, nil
	}

	if v, present := raw["template"]; present && v != nil {
		s, ok := v.(string)
		if !ok {
			return docx.FormatOptions{}, fmt.Errorf("docx_format: rules.template must be a string")
		}
		opts.Template = s
	}
	if v, present := raw["heading_font"]; present && v != nil {
		s, ok := v.(string)
		if !ok {
			return docx.FormatOptions{}, fmt.Errorf("docx_format: rules.heading_font must be a string")
		}
		opts.HeadingFont = s
	}
	if v, present := raw["body_font"]; present && v != nil {
		s, ok := v.(string)
		if !ok {
			return docx.FormatOptions{}, fmt.Errorf("docx_format: rules.body_font must be a string")
		}
		opts.BodyFont = s
	}
	if v, present := raw["body_east_asia_font"]; present && v != nil {
		s, ok := v.(string)
		if !ok {
			return docx.FormatOptions{}, fmt.Errorf("docx_format: rules.body_east_asia_font must be a string")
		}
		opts.BodyEastAsiaFont = s
	}
	if v, present := raw["align"]; present && v != nil {
		s, ok := v.(string)
		if !ok {
			return docx.FormatOptions{}, fmt.Errorf("docx_format: rules.align must be a string")
		}
		opts.Align = s
	}
	if v, present := raw["normalize"]; present && v != nil {
		b, ok := v.(bool)
		if !ok {
			return docx.FormatOptions{}, fmt.Errorf("docx_format: rules.normalize must be a boolean")
		}
		opts.Normalize = b
	}

	bodySize, err := docxFormatNumberArg(raw, "body_size_pt")
	if err != nil {
		return docx.FormatOptions{}, err
	}
	opts.BodySizePt = bodySize

	lineSpacing, err := docxFormatNumberArg(raw, "line_spacing")
	if err != nil {
		return docx.FormatOptions{}, err
	}
	opts.LineSpacing = lineSpacing

	// line_spacing_exact_pt is line_spacing's mutually-exclusive sibling —
	// both type-checked here the same never-coerce way, but the actual
	// mutual-exclusion RULE (both non-zero is an error) is enforced by
	// pkg/docx (validateAlignAndLineSpacingMutex), the same domain-rule
	// split this file's own doc comment already documents for align/
	// template (task 8 brief's "参数校验层拒绝" requirement). Unlike
	// line_spacing (whose own positivity was never in scope here),
	// line_spacing_exact_pt and the three fields below use
	// docxFormatPositiveNumberArg — task 8 review F6's non-negative
	// validation, with an error message naming the field.
	lineSpacingExactPt, err := docxFormatPositiveNumberArg(raw, "line_spacing_exact_pt")
	if err != nil {
		return docx.FormatOptions{}, err
	}
	opts.LineSpacingExactPt = lineSpacingExactPt

	firstLineIndentChars, err := docxFormatPositiveNumberArg(raw, "first_line_indent_chars")
	if err != nil {
		return docx.FormatOptions{}, err
	}
	opts.FirstLineIndentChars = firstLineIndentChars

	spaceBeforePt, err := docxFormatPositiveNumberArg(raw, "space_before_pt")
	if err != nil {
		return docx.FormatOptions{}, err
	}
	opts.SpaceBeforePt = spaceBeforePt

	spaceAfterPt, err := docxFormatPositiveNumberArg(raw, "space_after_pt")
	if err != nil {
		return docx.FormatOptions{}, err
	}
	opts.SpaceAfterPt = spaceAfterPt

	if v, present := raw["page_numbers"]; present && v != nil {
		b, ok := v.(bool)
		if !ok {
			return docx.FormatOptions{}, fmt.Errorf("docx_format: rules.page_numbers must be a boolean")
		}
		opts.PageNumbers = b
	}

	if err := requireNotRequested(raw, "rebuild_toc",
		"docx_format: rebuild_toc is not supported — rebuilding a table of contents means repaginating the whole "+
			"document (recomputing which heading lands on which printed page), which this pure-Go tool cannot do; "+
			"that needs a rendering engine such as LibreOffice, which is not wired in. Word itself can refresh a "+
			"TOC field automatically on open via settings.xml's <w:updateFields/>, but that is a future option this "+
			"tool does not yet write either. Tell the user the TOC was not rebuilt rather than working around this."); err != nil {
		return docx.FormatOptions{}, err
	}

	if v, present := raw["margins_mm"]; present && v != nil {
		mm, err := parseDocxFormatMarginsMM(v)
		if err != nil {
			return docx.FormatOptions{}, err
		}
		opts.MarginsMM = mm
	}

	return opts, nil
}

// requireNotRequested checks a boolean rules field that has no
// corresponding docx.FormatOptions field at all (rebuild_toc, since task
// 12; page_numbers used to be the other one before it got a real field —
// see parseDocxFormatRules): it type-checks the raw value the same as every
// other field, then errors with msg if and only if the caller actually
// asked for it (true). A field that is absent, nil, or explicitly false is
// not a request for the feature — refusing those too would make a caller
// that always sends the full rules shape (with unwanted features set to
// false) unable to use docx_format at all.
func requireNotRequested(raw map[string]any, key, msg string) error {
	v, present := raw[key]
	if !present || v == nil {
		return nil
	}
	b, ok := v.(bool)
	if !ok {
		return fmt.Errorf("docx_format: rules.%s must be a boolean", key)
	}
	if b {
		return errors.New(msg)
	}
	return nil
}

// parseDocxFormatMarginsMM converts a raw JSON value into the []float64
// pkg/docx.FormatOptions.MarginsMM wants. Per-element type-checking is this
// layer's load-bearing job: pkg/docx never sees the raw JSON array, only a
// Go []float64, so a smuggled non-numeric element (e.g. a string in the
// array) has to be caught here or it never gets caught at all. The length
// and positivity checks below are deliberately redundant with pkg/docx's
// own validateMargins (Document.Format calls it before touching any part,
// so a bad length/sign would be refused there too, just wrapped as
// "docx_format: docx: ..."): keeping them here as well fails fast, before
// even opening the document, with a cleaner single-source error message.

func parseDocxFormatMarginsMM(v any) ([]float64, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("docx_format: rules.margins_mm must be an array")
	}
	if len(arr) != 4 {
		return nil, fmt.Errorf(
			"docx_format: rules.margins_mm must have exactly 4 values (top, right, bottom, left); got %d", len(arr))
	}
	mm := make([]float64, 4)
	for i, e := range arr {
		f, ok := docxAsFloat(e)
		if !ok {
			return nil, fmt.Errorf("docx_format: rules.margins_mm[%d] must be a number", i)
		}
		if f <= 0 {
			return nil, fmt.Errorf("docx_format: rules.margins_mm[%d] = %g must be positive", i, f)
		}
		mm[i] = f
	}
	return mm, nil
}

// docxAsFloat converts a decoded JSON number (float64) or a Go-literal int
// (as tests construct arguments with) to float64. Any other type is
// reported as not-a-number rather than defaulting to 0, the same
// never-coerce rule docxIntArg/docxEditNumberArg already follow for
// integer fields.
func docxAsFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	default:
		return 0, false
	}
}

// docxFormatNumberArg reads key from raw as a float64 via docxAsFloat. A
// missing or nil key reports 0, which docx.FormatOptions treats as "not
// given" for every numeric field it has.
func docxFormatNumberArg(raw map[string]any, key string) (float64, error) {
	v, ok := raw[key]
	if !ok || v == nil {
		return 0, nil
	}
	f, ok := docxAsFloat(v)
	if !ok {
		return 0, fmt.Errorf("docx_format: rules.%s must be a number", key)
	}
	return f, nil
}

// docxFormatPositiveNumberArg is docxFormatNumberArg's stricter sibling for
// task 8's four new measurement fields (first_line_indent_chars/
// space_before_pt/space_after_pt/line_spacing_exact_pt): none of them has a
// sensible zero-or-negative meaning as an EXPLICIT request. docx.
// FormatOptions' own convention treats 0 as "not requested" for every
// numeric field (the same as BodySizePt/LineSpacing always have), but this
// layer — unlike pkg/docx's own validateNonNegativeMeasurements — CAN tell
// "the caller actually sent key:0" apart from "the caller omitted key
// entirely" (raw[key]'s presence), so it holds these four fields to a
// stricter rule: an explicitly-sent value that is zero or negative is
// rejected outright (review F6), rather than silently becoming the same
// no-op omitting the key would have been. A key that is absent or nil is
// untouched (0, nil error) — exactly docxFormatNumberArg's own "not
// requested" case.
func docxFormatPositiveNumberArg(raw map[string]any, key string) (float64, error) {
	v, present := raw[key]
	if !present || v == nil {
		return 0, nil
	}
	f, ok := docxAsFloat(v)
	if !ok {
		return 0, fmt.Errorf("docx_format: rules.%s must be a number", key)
	}
	if f <= 0 {
		return 0, fmt.Errorf("docx_format: rules.%s = %g must be positive", key, f)
	}
	return f, nil
}

// docxWriteArgs is docx_write's parsed arguments.
type docxWriteArgs struct {
	Path         string
	Markdown     string
	MarkdownPath string
	Title        string
	// The four font fields mirror docx.WriteOptions' own — see that type's
	// doc comment (pkg/docx/write.go) for what each controls and what each
	// falls back to when left empty ("" here, same as an omitted argument).
	BodyLatinFont    string
	BodyEastAsiaFont string
	CodeLatinFont    string
	CodeEastAsiaFont string
}

// docxWriteOutput is the JSON shape docx_write returns to the model.
// Paras is always present (never omitempty), since the count is exactly
// what lets a caller sanity-check the output size — a body that came out
// far shorter than expected is the caller's only signal that something
// went wrong, and a field that vanishes on a legitimate empty document
// (paras: 0 would never actually happen, but paras: 1 for a near-empty one
// must still show up) would defeat that. Notes carries every markdown
// construct pkg/docx.WriteDocx did not render structurally (currently only
// images), verbatim and unfiltered: this tool has no domain opinion of its
// own about what is or is not supported, and swallowing a note here would
// leave the model believing something rendered that did not.
type docxWriteOutput struct {
	Paras int      `json:"paras"`
	Notes []string `json:"notes,omitempty"`
}

// DocxWriteTool describes docx_write to the model. The description is
// deliberately explicit about every structural construct WriteDocx renders
// (headings, lists, tables, code, links, emphasis, quotes, rules) rather
// than a vague "converts markdown": design's brief for this task records
// that a model reading a vaguer description has twice fallen back to a bash
// + python script rather than trust a docx tool to handle a document with
// real structure. A description that reads as "probably just paragraphs"
// would reproduce that failure here.
//
// It is equally deliberate about markdown_path, and puts that guidance
// FIRST, ahead of the markdown-subset explanation: a model that just had a
// docx_write call fail with "invalid arguments JSON: unexpected end of JSON
// input" (its own streamed tool-call JSON truncated mid-string by a large
// inline markdown argument) needs to learn the file-based escape route from
// this description alone, on its very next attempt, without any human in
// the loop pointing it there. Burying that below the syntax reference would
// risk exactly that model skimming past it and inlining the same oversized
// document again.
func DocxWriteTool() models.Tool {
	return models.Tool{
		Name:         "docx_write",
		Groups:       []string{"builtin", "document"},
		ParallelSafe: false,
		Description: "Create a new .docx from markdown, given either inline via markdown or from a file via " +
			"markdown_path — pass exactly one of the two, never both and never neither. For anything longer " +
			"than a few pages (a design document, a report, anything with real length), do NOT inline the " +
			"whole document as the markdown argument: a large inline argument can exceed this model's own " +
			"output budget while it is being streamed, cutting the tool call's JSON off mid-string and failing " +
			"the whole call with no document written. Instead, build the markdown in a file first — write_file " +
			"for the first chunk, then write_file with append: true for every chunk after, so the file can grow " +
			"past any single response's output budget — and call docx_write once with markdown_path set to that " +
			"file's path. That makes document length effectively unbounded, and converting the whole thing in " +
			"one pass keeps list numbering, style ids, and hyperlink relationship ids consistent throughout; " +
			"writing several smaller .docx files and merging them instead would collide those three id spaces. " +
			"This renders real Word structure, not a plain-text dump: # .. ###### become Heading1-6 styles; " +
			"**bold**/*italic*/`inline code` become formatted runs; - / * and 1. lists (including nested ones) " +
			"become properly indented bulleted or numbered lists; GFM pipe tables (| a | b |, with an alignment " +
			"row) become bordered Word tables with a bold header row and per-column left/center/right alignment; " +
			"fenced ``` code blocks (and 4-space/tab-indented ones) become a single monospace, shaded, " +
			"bordered paragraph per block with indentation preserved and lines separated by line breaks; [text](url) " +
			"becomes a clickable hyperlink; ~~strikethrough~~ becomes struck-through text (nestable inside " +
			"bold/italic); a line ending in two or more trailing spaces, or in a single trailing backslash, " +
			"becomes a hard line break within the same paragraph instead of just soft-wrapping; a line of text " +
			"immediately followed by a line of one or more '=' or two-or-more '-' (a setext heading) becomes " +
			"Heading1/Heading2, the same as # and ##; > quotes become a bordered block quote, and a standalone " +
			"---, ***, or ___ becomes a horizontal rule. A document with tables, nested lists, and code blocks " +
			"is exactly what this tool is for — there is no need to write a script instead. The one thing it " +
			"cannot do is embed images: " +
			"![alt](url) is written as plain text and declared in notes, never silently dropped. Text containing " +
			"characters XML forbids (e.g. raw control codes from a pasted ANSI terminal transcript) has those " +
			"characters replaced rather than producing a file that reports success but will not open, and notes " +
			"reports how many. Every other unsupported edge case (e.g. a ragged table row) is declared in notes the " +
			"same way, so an empty notes field means the input rendered exactly as written. Refuses to create the file if path " +
			"already exists — creating never overwrites; delete the existing file or choose another path " +
			"first. Returns paras, the number of paragraphs written, so the caller can sanity-check the output " +
			"size.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path for the new .docx file. Must not already exist; this tool never overwrites.",
				},
				"markdown": map[string]any{
					"type": "string",
					"description": "Inline source markdown. Mutually exclusive with markdown_path — give exactly " +
						"one. Fine for short documents; for anything longer than a few pages, write the markdown " +
						"to a file with write_file (append: true for each chunk after the first) and pass " +
						"markdown_path instead, to avoid the inline argument being truncated. See the tool " +
						"description for the exact supported subset.",
				},
				"markdown_path": map[string]any{
					"type": "string",
					"description": "Path to a file holding the source markdown, read in full and converted in " +
						"one pass. Mutually exclusive with markdown — give exactly one. This is the route for " +
						"long documents: build the file incrementally with write_file (append: true for each " +
						"chunk after the first), then pass the finished file's path here.",
				},
				"title": map[string]any{
					"type":        "string",
					"description": "Optional title, rendered as the document's very first paragraph, styled as Heading1, ahead of anything parsed from markdown.",
				},
				"body_latin_font": map[string]any{
					"type":        "string",
					"description": "Latin font for ordinary body text and headings. Defaults to Calibri when omitted.",
				},
				"body_east_asia_font": map[string]any{
					"type":        "string",
					"description": "East Asian (e.g. Chinese) font for ordinary body text and headings. Defaults to 微软雅黑 (Microsoft YaHei) when omitted.",
				},
				"code_latin_font": map[string]any{
					"type":        "string",
					"description": "Latin font for fenced code blocks and inline code. Defaults to Consolas when omitted.",
				},
				"code_east_asia_font": map[string]any{
					"type": "string",
					"description": "East Asian font for fenced code blocks and inline code. Defaults to 微软雅黑 when omitted, which does " +
						"NOT give exact alignment between ASCII box-drawing characters and Chinese text (it is a proportional " +
						"font, not a 2:1 CJK/Latin monospace one) — pass a font with a true 2:1 width ratio (e.g. NSimSun or MS " +
						"Gothic, neither preinstalled on macOS) for exact alignment, typically when targeting Windows readers " +
						"or when such a font is installed locally.",
				},
			},
			"required": []any{"path"},
		},
		Handler: DocxWriteHandler,
	}
}

// DocxWriteHandler parses docx_write's arguments, resolves whichever of
// markdown/markdown_path was given into an in-memory markdown string, and
// delegates all rendering and OOXML skeleton construction to
// pkg/docx.WriteDocx unchanged — WriteDocx already takes markdown as a
// plain string, so markdown_path is purely a tool-layer convenience for
// getting that string without an inline argument. Unlike docx_edit/
// docx_format there is no backup to make (WriteDocx never overwrites, so
// there is nothing pre-existing to protect), and pkg/docx.WriteDocx's own
// refusal error (when path already exists) is already the clear, actionable
// message the brief asks for, so it is surfaced verbatim rather than
// reworded.
func DocxWriteHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	result := models.ToolResult{CallID: call.ID, ToolName: call.Name}

	args, err := parseDocxWriteArgs(call.Arguments)
	if err != nil {
		return result, err
	}

	markdown := args.Markdown
	if args.MarkdownPath != "" {
		resolvedMarkdownPath := resolveReadablePath(ctx, args.MarkdownPath)
		data, err := os.ReadFile(resolvedMarkdownPath)
		if err != nil {
			return result, fmt.Errorf("docx_write: read markdown_path %s: %w", resolvedMarkdownPath, err)
		}
		markdown = string(data)
	}

	resolved := resolveWritablePath(ctx, args.Path)
	writeResult, err := docx.WriteDocx(resolved, docx.WriteOptions{
		Markdown:         markdown,
		Title:            args.Title,
		BodyLatinFont:    args.BodyLatinFont,
		BodyEastAsiaFont: args.BodyEastAsiaFont,
		CodeLatinFont:    args.CodeLatinFont,
		CodeEastAsiaFont: args.CodeEastAsiaFont,
	})
	if err != nil {
		return result, fmt.Errorf("docx_write: %w", err)
	}

	out := docxWriteOutput{Paras: writeResult.Paras, Notes: writeResult.Notes}
	payload, err := json.Marshal(out)
	if err != nil {
		return result, fmt.Errorf("docx_write: marshal result: %w", err)
	}
	result.Status = models.CallStatusCompleted
	result.Content = string(payload)
	return result, nil
}

// parseDocxWriteArgs extracts docxWriteArgs from the raw JSON arguments map,
// type-checking every field and never coercing (the brief's explicit
// warning: a bare `raw["x"].(string)` with the ", ok" form silently
// discards a wrong-typed value as "" instead of erroring, which for
// markdown would mean a caller's mistaken non-string value producing an
// empty document while still reporting success).
//
// Exactly one of markdown/markdown_path must be present as a key: markdown
// alone is checked against presence rather than non-empty (distinct from an
// empty string, which pkg/docx.WriteDocx already defines sensible behavior
// for — see TestWrite_EmptyMarkdownProducesAValidEmptyDocument in pkg/docx
// and its tool-layer mirror here) because an entirely absent key is far more
// likely a caller mistake than a deliberate request for a near-empty
// document. markdown_path is checked the ordinary trimmed-non-empty way,
// like path, since a file path has no equivalent "deliberately near-empty"
// reading for a blank string.
func parseDocxWriteArgs(raw map[string]any) (docxWriteArgs, error) {
	path, _ := raw["path"].(string)
	path = strings.TrimSpace(path)
	if path == "" {
		return docxWriteArgs{}, fmt.Errorf("docx_write: path is required")
	}

	rawMarkdown, hasMarkdown := raw["markdown"]
	hasMarkdown = hasMarkdown && rawMarkdown != nil

	rawMarkdownPath, hasMarkdownPath := raw["markdown_path"]
	hasMarkdownPath = hasMarkdownPath && rawMarkdownPath != nil

	switch {
	case hasMarkdown && hasMarkdownPath:
		return docxWriteArgs{}, fmt.Errorf(
			"docx_write: give exactly one of markdown or markdown_path, not both")
	case !hasMarkdown && !hasMarkdownPath:
		return docxWriteArgs{}, fmt.Errorf(
			"docx_write: give exactly one of markdown (inline) or markdown_path (a file to read); " +
				"for anything longer than a few pages, write the markdown to a file with write_file " +
				"(append: true for each chunk after the first) and pass markdown_path instead of inlining it")
	}

	var markdown, markdownPath string
	if hasMarkdown {
		s, ok := rawMarkdown.(string)
		if !ok {
			return docxWriteArgs{}, fmt.Errorf("docx_write: markdown must be a string")
		}
		markdown = s
	} else {
		s, ok := rawMarkdownPath.(string)
		if !ok {
			return docxWriteArgs{}, fmt.Errorf("docx_write: markdown_path must be a string")
		}
		markdownPath = strings.TrimSpace(s)
		if markdownPath == "" {
			return docxWriteArgs{}, fmt.Errorf("docx_write: markdown_path must not be blank")
		}
	}

	var title string
	if v, present := raw["title"]; present && v != nil {
		s, ok := v.(string)
		if !ok {
			return docxWriteArgs{}, fmt.Errorf("docx_write: title must be a string")
		}
		title = s
	}

	fonts := make(map[string]string, 4)
	for _, key := range []string{"body_latin_font", "body_east_asia_font", "code_latin_font", "code_east_asia_font"} {
		if v, present := raw[key]; present && v != nil {
			s, ok := v.(string)
			if !ok {
				return docxWriteArgs{}, fmt.Errorf("docx_write: %s must be a string", key)
			}
			fonts[key] = s
		}
	}

	return docxWriteArgs{
		Path:             path,
		Markdown:         markdown,
		MarkdownPath:     markdownPath,
		Title:            title,
		BodyLatinFont:    fonts["body_latin_font"],
		BodyEastAsiaFont: fonts["body_east_asia_font"],
		CodeLatinFont:    fonts["code_latin_font"],
		CodeEastAsiaFont: fonts["code_east_asia_font"],
	}, nil
}

// DocxTools returns every docx tool.
func DocxTools() []models.Tool {
	return []models.Tool{DocxReadTool(), DocxEditTool(), DocxFormatTool(), DocxWriteTool()}
}

// DocxEditHandler parses docx_edit's arguments, delegates all editing
// semantics (locating by run/find, protect validation, collision detection,
// the revision gate) to pkg/docx, and owns only the four responsibilities
// pkg/docx deliberately leaves to the tool layer: rejecting style outright,
// backing up the file once before the first overwrite, echoing
// reviewed_through_para, and translating ParaCountChanged into actionable
// advice.
func DocxEditHandler(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	result := models.ToolResult{CallID: call.ID, ToolName: call.Name}

	args, err := parseDocxEditArgs(call.Arguments)
	if err != nil {
		return result, err
	}

	resolved := resolveWritablePath(ctx, args.Path)
	doc, err := docx.OpenDocument(resolved)
	if err != nil {
		return result, fmt.Errorf("docx_edit: %w", err)
	}

	editResult, err := doc.Edit(args.Edits, docx.EditOptions{
		Protect:      args.Protect,
		TrackChanges: args.TrackChanges,
		Author:       args.Author,
	})
	if err != nil {
		return result, fmt.Errorf("docx_edit: %w", err)
	}

	// Only a batch that actually changed something reaches disk: a batch
	// where every edit was refused leaves doc.Modified() false, and this
	// call must return cleanly without writing the file or touching the
	// backup. The backup itself is written BEFORE Save, not after: if Save
	// then fails (e.g. a disk error), the pristine original is already
	// safely copied aside, which is the ordering that actually protects the
	// user — backing up after a successful Save would be too late to help if
	// Save itself is what failed partway.
	var backupPath string
	var backupCreated bool
	if doc.Modified() {
		backupPath, backupCreated, err = backupDocxOnce(resolved)
		if err != nil {
			return result, fmt.Errorf("docx_edit: back up %s before saving: %w", resolved, err)
		}
		if err := doc.Save(); err != nil {
			return result, fmt.Errorf("docx_edit: save %s: %w", resolved, err)
		}
	}

	out := docxEditOutput{
		Applied:             editResult.Applied,
		Outcomes:            docxEditOutcomesFromResult(editResult.Outcomes),
		TotalParas:          editResult.TotalParas,
		ParaCountChanged:    editResult.ParaCountChanged,
		BackupPath:          backupPath,
		BackupCreated:       backupCreated,
		ReviewedThroughPara: args.ReviewedThroughPara,
		TrackChanges:        args.TrackChanges,
	}
	if backupPath != "" && !backupCreated {
		out.Notes = append(out.Notes, docxStaleBackupNote)
	}
	if editResult.ParaCountChanged {
		out.IndexAdvice = docxIndexAdvice
	}

	payload, err := json.Marshal(out)
	if err != nil {
		return result, fmt.Errorf("docx_edit: marshal result: %w", err)
	}
	result.Status = models.CallStatusCompleted
	result.Content = string(payload)
	return result, nil
}

// docxEditOutcomesFromResult converts pkg/docx's EditOutcome slice into the
// JSON shape docx_edit returns, echoing each outcome's target paragraph from
// the edit it corresponds to.
func docxEditOutcomesFromResult(outcomes []docx.EditOutcome) []docxEditOutcome {
	out := make([]docxEditOutcome, len(outcomes))
	for i, o := range outcomes {
		out[i] = docxEditOutcome{
			Para:    o.Edit.Para,
			Applied: o.Applied,
			Before:  o.Before,
			After:   o.After,
			Reason:  o.Reason,
			Warning: o.Warning,
		}
	}
	return out
}

// backupDocxOnce copies path to path+".bak" the first time it is called for
// that path, and does nothing on every later call. It keys that decision on
// the backup file's existence on disk, not on any in-process record: that
// makes it stateless (no global map, nothing to reset between calls or
// processes) and correct across restarts, and it is naturally idempotent —
// design §8/§5.7 require the backup to hold the ORIGINAL, pre-edit content
// for the entire lifetime of a multi-call polish run, so a second call must
// never refresh it once it exists.
//
// It uses Lstat, not Stat, to decide "does a backup already exist" (finding
// 7 of the P1c review): Stat follows symlinks, so a directory left at
// <path>.bak would make Stat succeed and this function would report a
// backup "exists" when Save() is about to overwrite the original with none
// at all, and a DANGLING <path>.bak symlink would make Stat fail with
// os.IsNotExist (since the symlink's target is missing), which would fall
// through to the os.WriteFile below and write the document's bytes to
// wherever the symlink points — an attacker-controlled path, if the symlink
// was planted there. Lstat reports on the symlink/directory entry itself in
// both cases, so requiring Mode().IsRegular() catches both without ever
// following the link.
//
// The returned bool reports whether THIS call created the backup (true) or
// found a pre-existing one and left it untouched (false) — see
// docxEditOutput.BackupCreated's doc comment for why that distinction
// matters to a caller.
func backupDocxOnce(path string) (backupPath string, created bool, err error) {
	backupPath = path + ".bak"
	info, statErr := os.Lstat(backupPath)
	switch {
	case statErr == nil:
		if !info.Mode().IsRegular() {
			return "", false, fmt.Errorf(
				"%s exists but is not a regular file (mode %s); refusing to treat it as a valid backup",
				backupPath, info.Mode())
		}
		return backupPath, false, nil
	case !os.IsNotExist(statErr):
		return "", false, statErr
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	if err := os.WriteFile(backupPath, data, filePerm(path, 0o644)); err != nil {
		return "", false, err
	}
	return backupPath, true, nil
}

// parseDocxEditArgs extracts docxEditArgs from the raw JSON arguments map.
// Like parseDocxReadArgs, it only converts types and checks presence; every
// editing rule (mutual exclusion of run/find, op legality, paragraph/run
// range, the revision gate, protect matching) is pkg/docx's to enforce. The
// one rule enforced here that pkg/docx has no opinion on at all is style:
// design §4.2 defers it to docx_format, and the schema never declares it, so
// any edit object that carries the key at all is refused outright rather
// than silently ignored.
func parseDocxEditArgs(raw map[string]any) (docxEditArgs, error) {
	path, _ := raw["path"].(string)
	path = strings.TrimSpace(path)
	if path == "" {
		return docxEditArgs{}, fmt.Errorf("docx_edit: path is required")
	}

	rawEdits, ok := raw["edits"].([]any)
	if !ok || len(rawEdits) == 0 {
		return docxEditArgs{}, fmt.Errorf("docx_edit: edits is required and must be a non-empty array")
	}

	edits := make([]docx.Edit, len(rawEdits))
	for i, re := range rawEdits {
		em, ok := re.(map[string]any)
		if !ok {
			return docxEditArgs{}, fmt.Errorf("docx_edit: edits[%d] must be an object", i)
		}
		// Checked before anything else, and independently for EVERY edit in
		// the batch (not just the first): silently dropping style anywhere
		// in the batch would let the model believe that edit restyled its
		// paragraph, when nothing about styling happened at all.
		if _, hasStyle := em["style"]; hasStyle {
			// Task 13's I1 fix: this used to say "use docx_format for
			// paragraph styling", but docx.FormatOptions has no field for a
			// paragraph style at all (only direct font/size/spacing/
			// alignment) — that advice sent a caller to a tool that cannot
			// do it either, with no signal that it was a dead end until the
			// next call also failed. Saying so plainly here, instead of
			// pointing anywhere, is the actionable version: there is
			// currently nowhere to send the caller, and the paragraph this
			// edit touches (or, for insert_before/insert_after, the new
			// paragraph it would have created) is left exactly as styled as
			// it already is.
			return docxEditArgs{}, fmt.Errorf(
				"docx_edit: edits[%d] sets style, which is not supported: paragraph styles cannot be set by any docx tool yet; "+
					"the inserted/edited paragraph keeps its current style", i)
		}
		edit, err := parseDocxEditItem(em)
		if err != nil {
			return docxEditArgs{}, fmt.Errorf("docx_edit: edits[%d]: %w", i, err)
		}
		edits[i] = edit
	}

	var protect []string
	if rawProtect, ok := raw["protect"].([]any); ok {
		for _, p := range rawProtect {
			if s, ok := p.(string); ok {
				protect = append(protect, s)
			}
		}
	}

	reviewedThroughPara, err := docxEditNumberArg(raw, "reviewed_through_para")
	if err != nil {
		return docxEditArgs{}, err
	}

	// track_changes/author are type-checked the same never-coerce way as
	// every other docx_edit argument (finding 4 of the P1c review): a bare
	// ", ok" assertion would let e.g. track_changes: "true" (a string, not a
	// bool) silently read as false and apply the edit directly while still
	// reporting success — exactly the kind of mismatch between what was
	// requested and what the model tells the user that this feature exists
	// to prevent.
	var trackChanges bool
	if v, present := raw["track_changes"]; present && v != nil {
		b, ok := v.(bool)
		if !ok {
			return docxEditArgs{}, fmt.Errorf("docx_edit: track_changes must be a boolean")
		}
		trackChanges = b
	}

	var author string
	if v, present := raw["author"]; present && v != nil {
		s, ok := v.(string)
		if !ok {
			return docxEditArgs{}, fmt.Errorf("docx_edit: author must be a string")
		}
		// Trimmed here, not left for pkg/docx: revisionCtx.attrs only
		// substitutes its "deepai" default for an EXACT empty string, so a
		// whitespace-only author ("   ") sent from the tool layer would
		// otherwise reach Word as a literal blank reviewer name in the
		// review pane rather than the same "deepai" default an omitted or
		// ""-valued author gets. Trimming here makes those three inputs
		// (omitted, "", whitespace-only) converge on one behavior before
		// EditOptions.Author is ever set.
		author = strings.TrimSpace(s)
	}

	return docxEditArgs{
		Path:                path,
		Edits:               edits,
		Protect:             protect,
		ReviewedThroughPara: reviewedThroughPara,
		TrackChanges:        trackChanges,
		Author:              author,
	}, nil
}

// parseDocxEditItem converts one raw edit object into a docx.Edit. find's
// nil-vs-pointer-to-empty-string distinction (see docx.Edit.Find's doc
// comment) is preserved here: an absent "find" key leaves Find nil, and a
// present key decodes to a non-nil pointer even when its value is "" — that
// second case is deliberately handed to pkg/docx to refuse, not resolved
// here, since planEdit's refusal message is the one that should reach the
// caller.
func parseDocxEditItem(em map[string]any) (docx.Edit, error) {
	para, err := docxEditNumberArg(em, "para")
	if err != nil {
		return docx.Edit{}, err
	}
	if para == 0 {
		return docx.Edit{}, fmt.Errorf("para is required")
	}
	run, err := docxEditNumberArg(em, "run")
	if err != nil {
		return docx.Edit{}, err
	}

	var find *string
	if v, present := em["find"]; present && v != nil {
		s, ok := v.(string)
		if !ok {
			return docx.Edit{}, fmt.Errorf("find must be a string")
		}
		find = &s
	}

	// text/op are type-checked the same way find already is (finding 4 of
	// the P1c review): the previous ", _ :=" form silently coerced a
	// wrong-typed value to "" instead of erroring, so an edit sent as
	// {"para":5,"find":"2025","text":2026} (a bare number where a model
	// meant a string) applied as a replace with Text="", deleting the
	// matched text and reporting applied:true with no diagnostic at all.
	var text string
	if v, present := em["text"]; present && v != nil {
		s, ok := v.(string)
		if !ok {
			return docx.Edit{}, fmt.Errorf("text must be a string")
		}
		text = s
	}
	var op string
	if v, present := em["op"]; present && v != nil {
		s, ok := v.(string)
		if !ok {
			return docx.Edit{}, fmt.Errorf("op must be a string")
		}
		op = s
	}

	return docx.Edit{Para: para, Run: run, Find: find, Text: text, Op: op}, nil
}

// docxEditNumberArg reads key from raw as an int, exactly like docxIntArg's
// float64/int handling, but with an error message scoped to docx_edit
// (docxIntArg's own message is hardcoded to docx_read, so it is not reused
// here). A missing or nil key reports 0, which every caller here treats as
// "not given".
func docxEditNumberArg(raw map[string]any, key string) (int, error) {
	v, ok := raw[key]
	if !ok || v == nil {
		return 0, nil
	}
	switch n := v.(type) {
	case float64:
		return int(n), nil
	case int:
		return n, nil
	default:
		return 0, fmt.Errorf("docx_edit: %s must be a number", key)
	}
}
