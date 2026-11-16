package runcode

import (
	"encoding/json"
	"sync"

	"github.com/millken/deepai/pkg/tools"
)

// journal records every STARTED nested call (§6.2): entries are written
// running BEFORE Registry.Execute and finalized after; on an aborted run the
// snapshot marks still-running entries interrupted — an interrupted write may
// have landed and must surface in writes:/edited_paths.
//
// Mutated on the driver goroutine and batch workers, snapshotted from the Run
// goroutine after a watchdog abort — hence the mutex. When a
// tools.NestedEventSink is attached (Phase 2 §7.2), every lifecycle
// transition also fires an event; sinks are invoked from worker goroutines
// and must be safe for concurrent use. The event's ParentID is left blank
// here — the agent-side adapter that injected the sink knows the outer
// run_code call ID and fills it.
type journal struct {
	mu      sync.Mutex
	entries []*JournalEntry
	sink    tools.NestedEventSink
	// aborted flips true the first time an aborting snapshot(true) runs;
	// from then on finish() updates entries silently (see its doc comment).
	aborted bool
}

func newJournal(sink tools.NestedEventSink) *journal { return &journal{sink: sink} }

func (j *journal) emit(e *JournalEntry, status JournalStatus, errText string) {
	if j.sink == nil {
		return
	}
	j.sink(tools.NestedToolEvent{
		Seq:    e.Seq,
		Tool:   e.Tool,
		Status: string(status),
		MS:     e.MS,
		Err:    errText,
	})
}

// begin appends a running entry and returns it for finalization.
func (j *journal) begin(tool string, args map[string]any, write bool) *JournalEntry {
	e := &JournalEntry{
		Tool:   tool,
		Status: JournalRunning,
		Write:  write,
		args:   args,
	}
	j.mu.Lock()
	e.Seq = len(j.entries) + 1
	j.entries = append(j.entries, e)
	j.mu.Unlock()
	j.emit(e, JournalRunning, "")
	return e
}

func (j *journal) finish(e *JournalEntry, status JournalStatus, ms int64, bytes int, errText string) {
	j.mu.Lock()
	// Post-abort arrival: an abandoned batch worker finishing after the
	// watchdog already snapshotted this entry as interrupted. The entry is
	// still finalized (a later snapshot shows what actually happened), but
	// NO event fires — the model already saw "interrupted"; a trailing
	// "completed" for the same call would contradict the journal the
	// returned result carries.
	aborted := j.aborted
	e.Status = status
	e.MS = ms
	e.Bytes = bytes
	j.mu.Unlock()
	if aborted {
		return
	}
	j.emit(e, status, errText)
}

// snapshot returns plain entries; when the run was aborted, still-running
// calls become interrupted.
func (j *journal) snapshot(aborted bool) []JournalEntry {
	j.mu.Lock()
	defer j.mu.Unlock()
	if aborted {
		j.aborted = true
	}
	out := make([]JournalEntry, 0, len(j.entries))
	for _, e := range j.entries {
		se := *e
		if aborted && se.Status == JournalRunning {
			se.Status = JournalInterrupted
			// Fired here (Run goroutine) rather than in driveVM: the
			// watchdog abandoned the driver, so this is the only party that
			// still observes the entry.
			j.emit(&se, JournalInterrupted, "script budget expired or was cancelled while this call was running")
		}
		se.ArgsPreview = argsPreview(e.args)
		out = append(out, se)
	}
	return out
}

// argsPreview is the ≤200-byte argument digest stored in the journal (§6.3).
func argsPreview(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	s := string(b)
	const previewCap = 200
	if len(s) <= previewCap {
		return s
	}
	return s[:previewCap] + "…"
}

// editedPaths derives Data["edited_paths"] from the journal: completed or
// interrupted edit_file/write_file targets (§4.5). Failed edits produced no
// change; interrupted ones may have landed — review them rather than miss them.
func editedPaths(calls []JournalEntry) []string {
	var out []string
	for _, e := range calls {
		if e.Tool != "edit_file" && e.Tool != "write_file" {
			continue
		}
		if e.Status != JournalCompleted && e.Status != JournalInterrupted {
			continue
		}
		path, _ := e.args["path"].(string)
		if path == "" {
			path, _ = e.args["file_path"].(string)
		}
		if path != "" {
			out = append(out, path)
		}
	}
	return out
}
