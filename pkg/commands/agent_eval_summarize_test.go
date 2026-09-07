package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// `deepai eval summarize` — rebuilds summary.json/summary.md from one or more
// runs.jsonl sources, for the case where writeEvalSummary never ran (e.g. the
// process was OOM-killed mid-round; see the M5 handoff). See
// agent_eval_summarize.go's package doc for why the three merge checks below
// are hard errors with no override flag.
// ---------------------------------------------------------------------------

// writeRunsJSONLFile writes one JSON object per line (encoding/json, not
// yaml) into dir/runs.jsonl and returns the directory (a valid `summarize`
// source, since sources may be a result dir OR a direct runs.jsonl path).
func writeRunsJSONLFile(t *testing.T, dir string, records []runRecord) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	var b strings.Builder
	for _, r := range records {
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal record: %v", err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	path := filepath.Join(dir, "runs.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return dir
}

func sampleRecords(agentType, fingerprint, model string, n int) []runRecord {
	var out []runRecord
	for i := 1; i <= n; i++ {
		out = append(out, runRecord{
			Case:        "case-1",
			AgentType:   agentType,
			Run:         i,
			Model:       model,
			Fingerprint: fingerprint,
			Assertions: []assertionResult{
				{Name: "mentions:Foo", Status: "pass"},
			},
			Tokens:     100 + i,
			DurationMS: int64(1000 * i),
		})
	}
	return out
}

// manifestRow is one parsed row of writeEvalMergeManifest's printed table,
// decoded by COLUMN rather than by substring search. The prior version of
// these tests asserted with strings.Contains(stdout, "3") /
// strings.Contains(line, "2") against a report whose "source" column is a
// t.TempDir() path (e.g. /var/folders/ck/.../001/) — a path that itself
// contains arbitrary digits, so those assertions were true no matter what
// the records/cases/dispatch_err/empty_model columns actually held. A
// mutant that hardcodes any of those columns to 0, or reports len(records)
// instead of the distinct case count, passed every existing test. Parsing
// by column position closes that gap.
type manifestRow struct {
	Source                                     string
	AgentType                                  string
	Records, Cases, DispatchErrors, EmptyModel int
}

// parseManifestRows scans stdout for writeEvalMergeManifest's table (it
// starts at the "source ... agent_type ..." header line) and decodes each
// data row by taking the last four whitespace-separated fields as the
// numeric columns (records, cases, dispatch_err, empty_model), the field
// before those as agent_type, and everything before THAT as source. This
// only requires that source/agent_type themselves contain no whitespace,
// which holds for every path/name this package produces.
func parseManifestRows(t *testing.T, stdout string) map[string]manifestRow {
	t.Helper()
	rows := map[string]manifestRow{}
	inTable := false
	for _, line := range strings.Split(stdout, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "source ") && strings.Contains(trimmed, "agent_type") {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 6 {
			// Table ended (e.g. "results written to ..." or a WARNING line).
			continue
		}
		n := len(fields)
		emptyModel, e1 := strconv.Atoi(fields[n-1])
		dispatchErr, e2 := strconv.Atoi(fields[n-2])
		cases, e3 := strconv.Atoi(fields[n-3])
		records, e4 := strconv.Atoi(fields[n-4])
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
			continue // not a data row (e.g. a WARNING line that slipped in)
		}
		agentType := fields[n-5]
		source := strings.Join(fields[:n-5], " ")
		key := source + "\x00" + agentType
		rows[key] = manifestRow{Source: source, AgentType: agentType, Records: records, Cases: cases, DispatchErrors: dispatchErr, EmptyModel: emptyModel}
	}
	return rows
}

func mustManifestRow(t *testing.T, stdout, source, agentType string) manifestRow {
	t.Helper()
	rows := parseManifestRows(t, stdout)
	row, ok := rows[source+"\x00"+agentType]
	if !ok {
		t.Fatalf("no manifest row for source=%s agent_type=%s; parsed rows: %+v; stdout:\n%s", source, agentType, rows, stdout)
	}
	return row
}

// ---------------------------------------------------------------------------
// Happy path: single source.
// ---------------------------------------------------------------------------

func TestRunEvalSummarizeCmd_SingleSourceMatchesBuildEvalSummary(t *testing.T) {
	srcDir := t.TempDir()
	records := sampleRecords("architect", "fp1", "glm-5.3", 3)
	writeRunsJSONLFile(t, srcDir, records)

	outDir := filepath.Join(t.TempDir(), "out")
	var stdout bytes.Buffer
	if err := runEvalSummarizeCmd(&stdout, outDir, 3, "5m", []string{srcDir}); err != nil {
		t.Fatalf("runEvalSummarizeCmd: %v", err)
	}

	gotBytes, err := os.ReadFile(filepath.Join(outDir, "summary.json"))
	if err != nil {
		t.Fatalf("read summary.json: %v", err)
	}
	var got evalSummary
	if err := json.Unmarshal(gotBytes, &got); err != nil {
		t.Fatalf("unmarshal summary.json: %v", err)
	}

	want := buildEvalSummary("glm-5.3", 3, "5m", records)

	if len(got.Roles) != len(want.Roles) {
		t.Fatalf("len(Roles) = %d, want %d", len(got.Roles), len(want.Roles))
	}
	if got.Model != want.Model || got.Runs != want.Runs || got.Timeout != want.Timeout {
		t.Errorf("summary header = %+v, want model/runs/timeout to match %+v", got, want)
	}
	for i := range want.Roles {
		if got.Roles[i] != want.Roles[i] {
			t.Errorf("Roles[%d] = %+v, want %+v", i, got.Roles[i], want.Roles[i])
		}
	}

	if _, err := os.Stat(filepath.Join(outDir, "summary.md")); err != nil {
		t.Errorf("summary.md not written: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Multiple sources merge.
// ---------------------------------------------------------------------------

func TestRunEvalSummarizeCmd_MultipleSourcesMergeRecordsAndRoles(t *testing.T) {
	dirA := filepath.Join(t.TempDir(), "a")
	dirB := filepath.Join(t.TempDir(), "b")
	writeRunsJSONLFile(t, dirA, sampleRecords("architect", "fp1", "glm-5.3", 3))
	writeRunsJSONLFile(t, dirB, sampleRecords("tester", "fp2", "glm-5.3", 3))

	outDir := filepath.Join(t.TempDir(), "out")
	var stdout bytes.Buffer
	if err := runEvalSummarizeCmd(&stdout, outDir, 3, "5m", []string{dirA, dirB}); err != nil {
		t.Fatalf("runEvalSummarizeCmd: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(outDir, "summary.json"))
	if err != nil {
		t.Fatalf("read summary.json: %v", err)
	}
	var got evalSummary
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Roles) != 2 {
		t.Fatalf("len(Roles) = %d, want 2 (architect + tester)", len(got.Roles))
	}
	total := 0
	for _, r := range got.Roles {
		total += r.DispatchedRuns
	}
	if total != 6 {
		t.Errorf("total DispatchedRuns = %d, want 6 (3 per role x 2 roles)", total)
	}

	// The merge manifest must be visible on stdout (operator-legibility
	// requirement): both agent types and both source directories should be
	// named somewhere in the printed report.
	out := stdout.String()
	for _, want := range []string{"architect", "tester", dirA, dirB} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout manifest missing %q; got:\n%s", want, out)
		}
	}
}

// Also accept a direct runs.jsonl path (not just a directory) as a source.
func TestRunEvalSummarizeCmd_AcceptsDirectRunsJSONLPath(t *testing.T) {
	srcDir := t.TempDir()
	writeRunsJSONLFile(t, srcDir, sampleRecords("architect", "fp1", "glm-5.3", 2))
	runsPath := filepath.Join(srcDir, "runs.jsonl")

	outDir := filepath.Join(t.TempDir(), "out")
	var stdout bytes.Buffer
	if err := runEvalSummarizeCmd(&stdout, outDir, 2, "5m", []string{runsPath}); err != nil {
		t.Fatalf("runEvalSummarizeCmd with direct runs.jsonl path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "summary.json")); err != nil {
		t.Errorf("summary.json not written: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Partial baseline: fewer cases than a full corpus is legal, but the
// manifest must make that visible (distinct case count per role).
// ---------------------------------------------------------------------------

func TestRunEvalSummarizeCmd_PartialBaselineManifestShowsCaseCounts(t *testing.T) {
	srcDir := t.TempDir()
	// Two distinct cases for "architect": case-a x2 runs, case-b x1 run —
	// simulates a round killed partway through, well short of a full corpus.
	records := []runRecord{
		{Case: "case-a", AgentType: "architect", Run: 1, Model: "glm-5.3", Fingerprint: "fp1"},
		{Case: "case-a", AgentType: "architect", Run: 2, Model: "glm-5.3", Fingerprint: "fp1"},
		{Case: "case-b", AgentType: "architect", Run: 1, Model: "glm-5.3", Fingerprint: "fp1"},
	}
	writeRunsJSONLFile(t, srcDir, records)

	outDir := filepath.Join(t.TempDir(), "out")
	var stdout bytes.Buffer
	if err := runEvalSummarizeCmd(&stdout, outDir, 3, "5m", []string{srcDir}); err != nil {
		t.Fatalf("runEvalSummarizeCmd: %v", err)
	}
	out := stdout.String()
	// The manifest line for architect must show 2 DISTINCT cases (case-a,
	// case-b) out of 3 records (case-a has 2 runs), so an operator can tell
	// this is a partial (2-case, not full-corpus) baseline at a glance. A
	// mutant that reports len(records) instead of the distinct case-id set
	// would print cases=3, not 2 — parsing by column (not substring) is what
	// catches that.
	row := mustManifestRow(t, out, filepath.Join(srcDir, "runs.jsonl"), "architect")
	if row.Records != 3 {
		t.Errorf("manifest records column = %d, want 3", row.Records)
	}
	if row.Cases != 2 {
		t.Errorf("manifest cases column = %d, want 2 (distinct case ids, not record count)", row.Cases)
	}
}

// ---------------------------------------------------------------------------
// Hard refusal ①: fingerprint mismatch for the same agent_type.
// ---------------------------------------------------------------------------

func TestRunEvalSummarizeCmd_FingerprintConflictRejected(t *testing.T) {
	dirA := filepath.Join(t.TempDir(), "a")
	dirB := filepath.Join(t.TempDir(), "b")
	// Distinct case ids across the two sources (like ModelConflictRejected
	// below): this must fail on the fingerprint check specifically, not get
	// preempted by the duplicate-triple check (①/② both a case-1/run1
	// collision if sampleRecords' fixed "case-1" id were reused here).
	writeRunsJSONLFile(t, dirA, []runRecord{
		{Case: "case-a", AgentType: "architect", Run: 1, Model: "glm-5.3", Fingerprint: "fp1"},
	})
	writeRunsJSONLFile(t, dirB, []runRecord{
		{Case: "case-b", AgentType: "architect", Run: 1, Model: "glm-5.3", Fingerprint: "fp2"},
	})

	outDir := filepath.Join(t.TempDir(), "out")
	err := runEvalSummarizeCmd(&bytes.Buffer{}, outDir, 3, "5m", []string{dirA, dirB})
	if err == nil {
		t.Fatal("expected a fingerprint-conflict error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"fp1", "fp2", dirA, dirB} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q; got: %s", want, msg)
		}
	}
	if _, statErr := os.Stat(outDir); !os.IsNotExist(statErr) {
		t.Errorf("outDir must not be created when the merge is rejected")
	}
}

// ---------------------------------------------------------------------------
// Hard refusal ②: duplicate (agent_type, case, run) triples — and ALL
// conflicts must be listed, not just the first.
// ---------------------------------------------------------------------------

func TestRunEvalSummarizeCmd_DuplicateTripleRejected_ListsAllConflicts(t *testing.T) {
	dirA := filepath.Join(t.TempDir(), "a")
	dirB := filepath.Join(t.TempDir(), "b")
	// Two different (case, run) pairs collide across the two sources.
	recordsA := []runRecord{
		{Case: "case-1", AgentType: "architect", Run: 1, Model: "glm-5.3", Fingerprint: "fp1"},
		{Case: "case-2", AgentType: "architect", Run: 1, Model: "glm-5.3", Fingerprint: "fp1"},
	}
	recordsB := []runRecord{
		{Case: "case-1", AgentType: "architect", Run: 1, Model: "glm-5.3", Fingerprint: "fp1"}, // dup of A's case-1/run1
		{Case: "case-2", AgentType: "architect", Run: 1, Model: "glm-5.3", Fingerprint: "fp1"}, // dup of A's case-2/run1
	}
	writeRunsJSONLFile(t, dirA, recordsA)
	writeRunsJSONLFile(t, dirB, recordsB)

	outDir := filepath.Join(t.TempDir(), "out")
	err := runEvalSummarizeCmd(&bytes.Buffer{}, outDir, 3, "5m", []string{dirA, dirB})
	if err == nil {
		t.Fatal("expected a duplicate-triple error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"case-1", "case-2", "run 1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q (all conflicts must be listed); got: %s", want, msg)
		}
	}
}

// ---------------------------------------------------------------------------
// Hard refusal ③: model mismatch (empty model exempt).
// ---------------------------------------------------------------------------

func TestRunEvalSummarizeCmd_ModelConflictRejected(t *testing.T) {
	dirA := filepath.Join(t.TempDir(), "a")
	dirB := filepath.Join(t.TempDir(), "b")
	// Distinct case ids across the two sources: this must fail on the model
	// check specifically, not get preempted by the duplicate-triple check.
	writeRunsJSONLFile(t, dirA, []runRecord{
		{Case: "case-a", AgentType: "architect", Run: 1, Model: "glm-5.3", Fingerprint: "fp1"},
	})
	writeRunsJSONLFile(t, dirB, []runRecord{
		{Case: "case-b", AgentType: "architect", Run: 1, Model: "gpt-5", Fingerprint: "fp1"},
	})

	outDir := filepath.Join(t.TempDir(), "out")
	err := runEvalSummarizeCmd(&bytes.Buffer{}, outDir, 3, "5m", []string{dirA, dirB})
	if err == nil {
		t.Fatal("expected a model-conflict error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"glm-5.3", "gpt-5"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q; got: %s", want, msg)
		}
	}
}

func TestRunEvalSummarizeCmd_EmptyModelRecordsExemptFromModelConflict(t *testing.T) {
	srcDir := t.TempDir()
	records := []runRecord{
		// A dispatch-timeout record with no recovered stats: Model is "".
		{Case: "case-1", AgentType: "architect", Run: 1, Model: "", Fingerprint: "fp1",
			Error: "context deadline exceeded", Assertions: []assertionResult{{Name: "dispatch", Status: "fail"}}},
		{Case: "case-2", AgentType: "architect", Run: 1, Model: "glm-5.3", Fingerprint: "fp1"},
	}
	writeRunsJSONLFile(t, srcDir, records)

	outDir := filepath.Join(t.TempDir(), "out")
	var stdout bytes.Buffer
	if err := runEvalSummarizeCmd(&stdout, outDir, 1, "5m", []string{srcDir}); err != nil {
		t.Fatalf("empty-model record must not trip the model-consistency check: %v", err)
	}
	// The manifest should still surface that one record had no model (and,
	// separately, that one record was a dispatch error — same record here,
	// but they are different columns and must not be confused), so an
	// operator can see how many runs never got real stats.
	row := mustManifestRow(t, stdout.String(), filepath.Join(srcDir, "runs.jsonl"), "architect")
	if row.Records != 2 {
		t.Errorf("manifest records column = %d, want 2", row.Records)
	}
	if row.EmptyModel != 1 {
		t.Errorf("manifest empty_model column = %d, want 1", row.EmptyModel)
	}
	if row.DispatchErrors != 1 {
		t.Errorf("manifest dispatch_err column = %d, want 1", row.DispatchErrors)
	}
}

// A dedicated dispatch_err-only case: the errored record HAS a model (stats
// were recovered after the timeout — see dispatchEvalTask's post-timeout
// GetTask poll), so dispatch_err must be 1 while empty_model stays 0. This
// is what pins down mutant 9 (dispatch_err hardcoded to 0) independently of
// mutant 8 (empty_model hardcoded to 0): the previous test alone can't tell
// which column a failure came from because both happened to be 1 on the
// same record.
func TestRunEvalSummarizeCmd_ManifestDispatchErrColumnCountsErrorsIndependentlyOfEmptyModel(t *testing.T) {
	srcDir := t.TempDir()
	records := []runRecord{
		{Case: "case-1", AgentType: "architect", Run: 1, Model: "glm-5.3", Fingerprint: "fp1",
			Error: "context deadline exceeded", Assertions: []assertionResult{{Name: "dispatch", Status: "fail"}}},
		{Case: "case-2", AgentType: "architect", Run: 1, Model: "glm-5.3", Fingerprint: "fp1"},
		{Case: "case-3", AgentType: "architect", Run: 1, Model: "glm-5.3", Fingerprint: "fp1"},
	}
	writeRunsJSONLFile(t, srcDir, records)

	outDir := filepath.Join(t.TempDir(), "out")
	var stdout bytes.Buffer
	if err := runEvalSummarizeCmd(&stdout, outDir, 1, "5m", []string{srcDir}); err != nil {
		t.Fatalf("runEvalSummarizeCmd: %v", err)
	}
	row := mustManifestRow(t, stdout.String(), filepath.Join(srcDir, "runs.jsonl"), "architect")
	if row.Records != 3 {
		t.Errorf("manifest records column = %d, want 3", row.Records)
	}
	if row.DispatchErrors != 1 {
		t.Errorf("manifest dispatch_err column = %d, want 1", row.DispatchErrors)
	}
	if row.EmptyModel != 0 {
		t.Errorf("manifest empty_model column = %d, want 0 (the errored record still had a model)", row.EmptyModel)
	}
}

// ---------------------------------------------------------------------------
// --out must not already exist.
// ---------------------------------------------------------------------------

func TestRunEvalSummarizeCmd_RejectsExistingOutDir(t *testing.T) {
	srcDir := t.TempDir()
	writeRunsJSONLFile(t, srcDir, sampleRecords("architect", "fp1", "glm-5.3", 1))

	outDir := t.TempDir() // already exists
	var stdout bytes.Buffer
	err := runEvalSummarizeCmd(&stdout, outDir, 1, "5m", []string{srcDir})
	if err == nil {
		t.Fatal("expected an error when --out already exists")
	}
	if !strings.Contains(err.Error(), outDir) {
		t.Errorf("error should name the existing dir %s; got: %v", outDir, err)
	}
	// This must fail BEFORE the merge manifest (a "looks successful" report)
	// is printed to stdout — an operator retrying a typo'd --out should see
	// only the error, not a table that makes the run look like it worked.
	if stdout.Len() != 0 {
		t.Errorf("expected no stdout output when --out already exists (fail fast, before the manifest is printed); got:\n%s", stdout.String())
	}
}

// ---------------------------------------------------------------------------
// --out/--runs/--timeout have no usable default and must be explicit.
// ---------------------------------------------------------------------------

func TestRunEvalSummarizeCmd_RequiresOut(t *testing.T) {
	srcDir := t.TempDir()
	writeRunsJSONLFile(t, srcDir, sampleRecords("architect", "fp1", "glm-5.3", 1))
	err := runEvalSummarizeCmd(&bytes.Buffer{}, "", 1, "5m", []string{srcDir})
	if err == nil {
		t.Fatal("expected an error when --out is empty")
	}
}

func TestRunEvalSummarizeCmd_RequiresRuns(t *testing.T) {
	srcDir := t.TempDir()
	writeRunsJSONLFile(t, srcDir, sampleRecords("architect", "fp1", "glm-5.3", 1))
	outDir := filepath.Join(t.TempDir(), "out")
	err := runEvalSummarizeCmd(&bytes.Buffer{}, outDir, 0, "5m", []string{srcDir})
	if err == nil {
		t.Fatal("expected an error when --runs is 0/unset")
	}
}

func TestRunEvalSummarizeCmd_RequiresTimeout(t *testing.T) {
	srcDir := t.TempDir()
	writeRunsJSONLFile(t, srcDir, sampleRecords("architect", "fp1", "glm-5.3", 1))
	outDir := filepath.Join(t.TempDir(), "out")
	err := runEvalSummarizeCmd(&bytes.Buffer{}, outDir, 1, "", []string{srcDir})
	if err == nil {
		t.Fatal("expected an error when --timeout is empty")
	}
}

func TestRunEvalSummarizeCmd_RequiresAtLeastOneSource(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "out")
	err := runEvalSummarizeCmd(&bytes.Buffer{}, outDir, 1, "5m", nil)
	if err == nil {
		t.Fatal("expected an error with zero sources")
	}
}

func TestRunEvalSummarizeCmd_NonexistentSourceErrorsCleanly(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "out")
	err := runEvalSummarizeCmd(&bytes.Buffer{}, outDir, 1, "5m", []string{filepath.Join(t.TempDir(), "does-not-exist")})
	if err == nil {
		t.Fatal("expected an error for a nonexistent source")
	}
}

// ---------------------------------------------------------------------------
// Malformed JSON lines must report a line number, not panic.
// ---------------------------------------------------------------------------

func TestRunEvalSummarizeCmd_MalformedLineReportsLineNumber(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	good, _ := json.Marshal(runRecord{Case: "case-1", AgentType: "architect", Run: 1, Model: "glm-5.3", Fingerprint: "fp1"})
	content := string(good) + "\n" + "{not valid json" + "\n"
	if err := os.WriteFile(filepath.Join(srcDir, "runs.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(t.TempDir(), "out")
	err := runEvalSummarizeCmd(&bytes.Buffer{}, outDir, 1, "5m", []string{srcDir})
	if err == nil {
		t.Fatal("expected a parse error for the malformed line")
	}
	if !strings.Contains(err.Error(), ":2") {
		t.Errorf("error should cite line 2; got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Run-coverage WARNING (not a hard refusal — see the doc comment on
// evalRunCoverageWarnings for why): a case whose contributed run numbers
// aren't exactly {1..--runs} must be flagged, because merging a killed
// round is this command's primary use case and a case that only got 2 of 3
// runs in must remain mergeable, just not silently.
// ---------------------------------------------------------------------------

func TestRunEvalSummarizeCmd_RunCoverageWarningWhenRunsIncomplete(t *testing.T) {
	srcDir := t.TempDir()
	// architect/case-1 has runs {1,2} but --runs 3 is declared: run 3 never
	// happened (e.g. the round was killed after run 2 started case-2).
	records := []runRecord{
		{Case: "case-1", AgentType: "architect", Run: 1, Model: "glm-5.3", Fingerprint: "fp1"},
		{Case: "case-1", AgentType: "architect", Run: 2, Model: "glm-5.3", Fingerprint: "fp1"},
	}
	writeRunsJSONLFile(t, srcDir, records)

	outDir := filepath.Join(t.TempDir(), "out")
	var stdout bytes.Buffer
	if err := runEvalSummarizeCmd(&stdout, outDir, 3, "5m", []string{srcDir}); err != nil {
		t.Fatalf("runEvalSummarizeCmd: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "WARNING") {
		t.Fatalf("expected a run-coverage WARNING; got stdout:\n%s", out)
	}
	if !strings.Contains(out, "architect/case-1") {
		t.Errorf("warning should name architect/case-1; got:\n%s", out)
	}
	if !strings.Contains(out, "{1,2}") {
		t.Errorf("warning should show the actual run set {1,2}; got:\n%s", out)
	}
	if !strings.Contains(out, "{1,2,3}") {
		t.Errorf("warning should show the expected run set {1,2,3}; got:\n%s", out)
	}
}

func TestRunEvalSummarizeCmd_RunCoverageMissingMiddleRunIsWarned(t *testing.T) {
	// The sneakiest case named in the review: run 2 is missing but run 3 is
	// present, so a naive "did we get N records" count (2 of 3) would look
	// the same as a clean {1,2} partial and hide that run 2 specifically
	// never landed.
	srcDir := t.TempDir()
	records := []runRecord{
		{Case: "case-1", AgentType: "architect", Run: 1, Model: "glm-5.3", Fingerprint: "fp1"},
		{Case: "case-1", AgentType: "architect", Run: 3, Model: "glm-5.3", Fingerprint: "fp1"},
	}
	writeRunsJSONLFile(t, srcDir, records)

	outDir := filepath.Join(t.TempDir(), "out")
	var stdout bytes.Buffer
	if err := runEvalSummarizeCmd(&stdout, outDir, 3, "5m", []string{srcDir}); err != nil {
		t.Fatalf("runEvalSummarizeCmd: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "{1,3}") {
		t.Errorf("warning should show the actual (gapped) run set {1,3}; got:\n%s", out)
	}
}

func TestRunEvalSummarizeCmd_RunCoverageNoWarningWhenComplete(t *testing.T) {
	srcDir := t.TempDir()
	writeRunsJSONLFile(t, srcDir, sampleRecords("architect", "fp1", "glm-5.3", 3))

	outDir := filepath.Join(t.TempDir(), "out")
	var stdout bytes.Buffer
	if err := runEvalSummarizeCmd(&stdout, outDir, 3, "5m", []string{srcDir}); err != nil {
		t.Fatalf("runEvalSummarizeCmd: %v", err)
	}
	if strings.Contains(stdout.String(), "WARNING") {
		t.Errorf("expected no WARNING for complete run coverage; got:\n%s", stdout.String())
	}
}

// ---------------------------------------------------------------------------
// merge-manifest.txt: the merge report must survive on disk, not just on
// stdout (which gets scrolled away or redirected), so a summary.json three
// months from now can be traced back to what it was assembled from.
// ---------------------------------------------------------------------------

func TestRunEvalSummarizeCmd_WritesMergeManifestFile(t *testing.T) {
	dirA := filepath.Join(t.TempDir(), "a")
	writeRunsJSONLFile(t, dirA, []runRecord{
		{Case: "case-1", AgentType: "architect", Run: 1, Model: "glm-5.3", Fingerprint: "fp1"},
		{Case: "case-1", AgentType: "architect", Run: 2, Model: "glm-5.3", Fingerprint: "fp1"},
	})

	outDir := filepath.Join(t.TempDir(), "out")
	if err := runEvalSummarizeCmd(&bytes.Buffer{}, outDir, 3, "5m", []string{dirA}); err != nil {
		t.Fatalf("runEvalSummarizeCmd: %v", err)
	}

	manifestBytes, err := os.ReadFile(filepath.Join(outDir, "merge-manifest.txt"))
	if err != nil {
		t.Fatalf("read merge-manifest.txt: %v", err)
	}
	manifest := string(manifestBytes)

	absA, err := filepath.Abs(dirA)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		absA,        // absolute path of the source
		"architect", // role contributed
		"runs: 3",   // --runs value
		"timeout: 5m",
		"WARNING", // incomplete run coverage (only runs 1,2 of 3) must show here too
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("merge-manifest.txt missing %q; got:\n%s", want, manifest)
		}
	}
}

// ---------------------------------------------------------------------------
// The same source supplied twice (once as a directory, once as its
// runs.jsonl) must not be reported as an 18-way "line 1 conflicts with line
// 1" duplicate-triple wall of noise — it must say plainly that one source
// was supplied more than once.
// ---------------------------------------------------------------------------

func TestRunEvalSummarizeCmd_SameSourceSuppliedTwice_ClearError(t *testing.T) {
	dirD := t.TempDir()
	writeRunsJSONLFile(t, dirD, sampleRecords("architect", "fp1", "glm-5.3", 3))
	runsPath := filepath.Join(dirD, "runs.jsonl")

	outDir := filepath.Join(t.TempDir(), "out")
	err := runEvalSummarizeCmd(&bytes.Buffer{}, outDir, 3, "5m", []string{dirD, runsPath})
	if err == nil {
		t.Fatal("expected an error when the same source is supplied as both a directory and its runs.jsonl")
	}
	msg := err.Error()
	if !strings.Contains(msg, "more than once") {
		t.Errorf("error should clearly say the source was supplied more than once, not a generic line-conflict dump; got: %s", msg)
	}
	// Must not degrade into per-record "line 1 conflicts with line 1" noise
	// (one such line would be tolerable; many would defeat the point).
	if strings.Count(msg, "conflict") > 1 {
		t.Errorf("expected one clear message, not a per-record conflict dump; got: %s", msg)
	}
}

// ---------------------------------------------------------------------------
// Malformed records: empty agent_type / empty case / non-positive run must
// be hard errors (file:line), not a silently nameless row in the manifest
// and summary.md.
// ---------------------------------------------------------------------------

func TestRunEvalSummarizeCmd_EmptyAgentTypeRejected(t *testing.T) {
	srcDir := t.TempDir()
	writeRunsJSONLFile(t, srcDir, []runRecord{
		{Case: "case-1", AgentType: "", Run: 1, Model: "glm-5.3", Fingerprint: "fp1"},
	})
	outDir := filepath.Join(t.TempDir(), "out")
	err := runEvalSummarizeCmd(&bytes.Buffer{}, outDir, 1, "5m", []string{srcDir})
	if err == nil {
		t.Fatal("expected an error for an empty agent_type")
	}
	if !strings.Contains(err.Error(), ":1") {
		t.Errorf("error should cite the line number; got: %v", err)
	}
}

func TestRunEvalSummarizeCmd_EmptyCaseRejected(t *testing.T) {
	srcDir := t.TempDir()
	writeRunsJSONLFile(t, srcDir, []runRecord{
		{Case: "", AgentType: "architect", Run: 1, Model: "glm-5.3", Fingerprint: "fp1"},
	})
	outDir := filepath.Join(t.TempDir(), "out")
	err := runEvalSummarizeCmd(&bytes.Buffer{}, outDir, 1, "5m", []string{srcDir})
	if err == nil {
		t.Fatal("expected an error for an empty case")
	}
	if !strings.Contains(err.Error(), ":1") {
		t.Errorf("error should cite the line number; got: %v", err)
	}
}

func TestRunEvalSummarizeCmd_NonPositiveRunRejected(t *testing.T) {
	srcDir := t.TempDir()
	writeRunsJSONLFile(t, srcDir, []runRecord{
		{Case: "case-1", AgentType: "architect", Run: 0, Model: "glm-5.3", Fingerprint: "fp1"},
	})
	outDir := filepath.Join(t.TempDir(), "out")
	err := runEvalSummarizeCmd(&bytes.Buffer{}, outDir, 1, "5m", []string{srcDir})
	if err == nil {
		t.Fatal("expected an error for run <= 0")
	}
	if !strings.Contains(err.Error(), ":1") {
		t.Errorf("error should cite the line number; got: %v", err)
	}
}
