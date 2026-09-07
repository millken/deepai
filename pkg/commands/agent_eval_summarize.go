// Package commands: `deepai eval summarize` (M5 baseline-rescue).
//
// A real `deepai eval agents` round can run for over an hour and is written
// incrementally to runs.jsonl (see runWriter's doc comment) precisely so an
// interruption never loses completed work. But writeEvalSummary — the step
// that produces summary.json/summary.md — only runs once, at the very end,
// after every case has finished. If the process is killed partway through
// (e.g. by the OOM-killer), runs.jsonl survives with every completed run,
// but there is no summary.json at all: nothing downstream (`eval compare`,
// a human skimming summary.md) can read the round.
//
// `eval summarize` closes that gap by reading one or more existing
// runs.jsonl files (or result directories containing one — the shape
// prepareEvalResultDir/`agents --filter <role>` reruns produce, one per
// role), merging their records, and calling the exact same
// buildEvalSummary/writeEvalSummary a normal `agents` invocation calls. It
// deliberately does not reimplement or alter either of those — merging is
// pure record-list assembly, so the same aggregation logic that is already
// tested and already runs in production is the only aggregation logic that
// should ever run here.
//
// The hard part of "merge N runs.jsonl files into one baseline" is not the
// merge itself, it's refusing to merge when doing so would fabricate a
// baseline that looks whole but isn't. See validateAndMergeEvalRuns for the
// three checks this file treats as non-negotiable (no --force, ever):
// fingerprint-per-agent_type, no duplicate (agent_type,case,run) triples,
// and model consistency. A silently-merged baseline that mixes prompts,
// silently drops a rerun's conflicting attempt, or blends two models is
// worse than no baseline at all — it looks authoritative and isn't.
package commands

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// CLI wiring
// ---------------------------------------------------------------------------

var agentEvalSummarizeFlags struct {
	Out     string
	Runs    int
	Timeout string
}

// addEvalSummarize registers `deepai eval summarize` onto the same `eval`
// parent command addEval builds. It is a separate function (rather than
// folded into addEval in agent_eval.go) so this file can be reviewed and,
// if ever necessary, reverted independently of the `agents`/`compare`
// wiring — none of which this command touches.
func addEvalSummarize(evalCmd *cobra.Command) {
	summarizeCmd := &cobra.Command{
		Use:   "summarize --out <new-dir> --runs N --timeout T <dir|runs.jsonl>...",
		Short: "Rebuild summary.json/summary.md from one or more runs.jsonl sources (e.g. after an interrupted `agents` round left no summary.json)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEvalSummarizeCmd(cmd.OutOrStdout(), agentEvalSummarizeFlags.Out, agentEvalSummarizeFlags.Runs, agentEvalSummarizeFlags.Timeout, args)
		},
	}
	// No defaults on any of these three: see runEvalSummarizeCmd's doc
	// comment for why --runs/--timeout cannot be read from summary.json
	// (the round this command rescues never produced one), and --out has no
	// safe default at all — a merge command silently landing in some
	// implicit directory is exactly the failure class a16a9ab closed for
	// runs.jsonl itself. MarkFlagRequired gives a clean CLI-level error;
	// runEvalSummarizeCmd re-checks the same thing so direct callers (and
	// these tests) get the same guarantee without going through cobra.
	summarizeCmd.Flags().StringVar(&agentEvalSummarizeFlags.Out, "out", "", "Output directory for summary.json/summary.md (must not already exist)")
	summarizeCmd.Flags().IntVar(&agentEvalSummarizeFlags.Runs, "runs", 0, "Repetitions per case that produced these runs.jsonl files (not read from summary.json, see doc comment; `eval compare` hard-errors if this doesn't match the other side's --runs)")
	summarizeCmd.Flags().StringVar(&agentEvalSummarizeFlags.Timeout, "timeout", "", "Per-run wall-clock timeout used to produce these runs.jsonl files (not read from summary.json, same reason as --runs; `eval compare` hard-errors on a mismatch here too, unless one side predates this field)")
	_ = summarizeCmd.MarkFlagRequired("out")
	_ = summarizeCmd.MarkFlagRequired("runs")
	_ = summarizeCmd.MarkFlagRequired("timeout")
	evalCmd.AddCommand(summarizeCmd)
}

// runEvalSummarizeCmd is the entire command body, kept independent of cobra
// (plain io.Writer + plain args) so it can be exercised directly in tests
// without constructing a *cobra.Command.
//
// --runs/--timeout come from the caller (flags), never from summary.json,
// because the round this command exists to rescue was killed before
// writeEvalSummary ever ran — there is no summary.json on disk to read them
// from. This is not a shortcut: the three checks that actually decide
// whether THIS merge is safe (fingerprint/triple/model — see
// validateAndMergeEvalRuns) are derived entirely from fields every record in
// runs.jsonl already carries (agent_type/case/run/fingerprint/model), so the
// missing round-level metadata never blocks those checks. But --runs and
// --timeout are NOT merely descriptive: renderEvalCompare (agent_eval.go)
// hard-errors "not comparable" on a Runs mismatch, and on a Timeout
// mismatch unless one side predates this field. Get either wrong here and
// this baseline either can never be compared against, or gets compared
// under a false "same protocol" assumption — so they decide who this
// baseline can be compared with, not just how the header reads.
func runEvalSummarizeCmd(out io.Writer, outDir string, runs int, timeoutStr string, sources []string) error {
	if outDir == "" {
		return fmt.Errorf("eval summarize: --out is required (no default: this writes a merged baseline and must never land somewhere by accident)")
	}
	if runs <= 0 {
		return fmt.Errorf("eval summarize: --runs is required and must be > 0 (not read from summary.json — see doc comment: the round this rescues never produced one)")
	}
	if timeoutStr == "" {
		return fmt.Errorf("eval summarize: --timeout is required (not read from summary.json — see doc comment: the round this rescues never produced one)")
	}
	if _, err := parseEvalTimeout(timeoutStr); err != nil {
		return err
	}
	if len(sources) == 0 {
		return fmt.Errorf("eval summarize: at least one source (a result directory or a runs.jsonl path) is required")
	}
	// Cheap fail-fast: check --out before doing any real work (loading
	// sources, validating, printing the merge manifest to stdout). Without
	// this, an operator retrying a typo'd/reused --out saw a full "looks
	// successful" manifest table printed BEFORE the error — this Stat is not
	// the authoritative check (prepareEvalSummarizeOutDir's os.Mkdir still is
	// — see its doc comment for why atomicity is claimed there, not here),
	// just a fast, common-case exit before any output is produced.
	if _, err := os.Stat(outDir); err == nil {
		return fmt.Errorf("eval summarize: refusing to write to existing directory %s (--out must not already exist)", outDir)
	}

	// resolvedPaths catches the same source being supplied twice — e.g.
	// "summarize dirD dirD/runs.jsonl" both resolve to dirD/runs.jsonl. Left
	// uncaught, that produces one duplicate-(agent_type,case,run) conflict
	// per record in the file, each one reporting the exact same location
	// against itself ("dirD/runs.jsonl:1 conflicts with dirD/runs.jsonl:1")
	// — technically correct, useless to read, and it buries the actually
	// common mistake (a source pasted twice) under a wall of noise that
	// looks like it's about something else. Catching it here, before any
	// record is even loaded, gives one clear message instead.
	resolvedPaths := map[string]string{}
	var tagged []taggedRunRecord
	for _, src := range sources {
		path, err := resolveEvalRunsPath(src)
		if err != nil {
			return err
		}
		if first, dup := resolvedPaths[path]; dup {
			return fmt.Errorf("eval summarize: source %q and source %q both resolve to %s — one source was supplied more than once (e.g. as both a directory and its runs.jsonl)", first, src, path)
		}
		resolvedPaths[path] = src
		recs, err := loadTaggedRunRecords(path)
		if err != nil {
			return err
		}
		tagged = append(tagged, recs...)
	}

	records, model, manifest, err := validateAndMergeEvalRuns(tagged)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "deepai eval summarize: merging %d record(s) from %d source(s), model=%s\n", len(records), len(sources), model)
	writeEvalMergeManifest(out, manifest)
	// See evalRunCoverageWarnings' doc comment for why an incomplete
	// per-(agent_type,case) run set is a WARNING here, not a fourth hard
	// refusal alongside validateAndMergeEvalRuns' three.
	warnings := evalRunCoverageWarnings(tagged, runs)
	for _, w := range warnings {
		fmt.Fprintln(out, w)
	}

	// The merge is validated and the manifest already printed before this
	// creates anything on disk — a rejected merge (see the three checks
	// above) must leave no partial output directory behind for a caller to
	// trip over on retry.
	mergedAt := time.Now().UTC()
	if err := prepareEvalSummarizeOutDir(outDir); err != nil {
		return err
	}
	if err := writeEvalSummary(outDir, model, runs, timeoutStr, records); err != nil {
		return err
	}
	// merge-manifest.txt is the provenance record writeEvalSummary's output
	// otherwise has none of: summary.json produced by summarize is
	// byte-for-byte the same shape as one a normal `agents` round writes, so
	// without this file nothing on disk says a given baseline was ever
	// stitched together from N sources, let alone which ones, or that a
	// case fell short of --runs. See writeEvalMergeManifestFile's doc
	// comment for exactly what it records.
	if err := writeEvalMergeManifestFile(outDir, sources, runs, timeoutStr, manifest, warnings, mergedAt); err != nil {
		return err
	}
	fmt.Fprintf(out, "results written to %s\n", outDir)
	return nil
}

// ---------------------------------------------------------------------------
// Loading: source -> runs.jsonl path -> tagged records
// ---------------------------------------------------------------------------

// taggedRunRecord decorates a decoded runRecord with exactly where it came
// from (source path + 1-based line number within that file), which is what
// lets every error below name precise locations instead of just "some
// record somewhere disagrees."
type taggedRunRecord struct {
	runRecord
	Source string
	Line   int
}

func (t taggedRunRecord) loc() string {
	return fmt.Sprintf("%s:%d", t.Source, t.Line)
}

// resolveEvalRunsPath accepts either a result directory (containing
// runs.jsonl, the shape prepareEvalResultDir produces) or a direct path to
// a runs.jsonl file, matching the CLI brief's "<dir|runs.jsonl>..." contract.
func resolveEvalRunsPath(source string) (string, error) {
	info, err := os.Stat(source)
	if err != nil {
		return "", fmt.Errorf("eval summarize: source %s: %w", source, err)
	}
	if !info.IsDir() {
		return source, nil
	}
	path := filepath.Join(source, "runs.jsonl")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("eval summarize: source %s: %w", source, err)
	}
	return path, nil
}

// loadTaggedRunRecords parses path as newline-delimited JSON runRecord
// values. Blank lines are skipped (a trailing newline is normal); any line
// that fails to parse is a hard error naming the exact line number — a
// silently-skipped malformed line would just be a quieter version of the
// same "confidently wrong baseline" problem this whole file exists to
// prevent.
func loadTaggedRunRecords(path string) ([]taggedRunRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("eval summarize: open %s: %w", path, err)
	}
	defer f.Close()

	var out []taggedRunRecord
	scanner := bufio.NewScanner(f)
	// A run record's `output` field can carry a full subagent transcript;
	// bufio.Scanner's default 64KiB token limit is comfortably too small for
	// that, so grow it well past anything a real run has produced.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		raw := scanner.Bytes()
		if len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		var rec runRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("eval summarize: %s:%d: malformed run record: %w", path, line, err)
		}
		// A real harness run never produces one of these (see
		// buildEvalSummary/runOneCase) — it takes a hand-edited or
		// truncated file to get here. But left unchecked, an empty
		// agent_type/case produces a nameless row in summary.md/the merge
		// manifest that is easy to skim right past, silently degrading the
		// baseline it's rolled into. A cheap check here turns that into a
		// hard, precisely-located error instead, matching this file's
		// standing rule against a baseline that looks whole but isn't.
		if strings.TrimSpace(rec.AgentType) == "" {
			return nil, fmt.Errorf("eval summarize: %s:%d: run record has an empty agent_type", path, line)
		}
		if strings.TrimSpace(rec.Case) == "" {
			return nil, fmt.Errorf("eval summarize: %s:%d: run record has an empty case", path, line)
		}
		if rec.Run <= 0 {
			return nil, fmt.Errorf("eval summarize: %s:%d: run record has an invalid run number %d (must be > 0)", path, line, rec.Run)
		}
		out = append(out, taggedRunRecord{runRecord: rec, Source: path, Line: line})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("eval summarize: read %s: %w", path, err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Merge + the three hard refusals
// ---------------------------------------------------------------------------

// mergeManifestEntry is one (source, agent_type) row of the human-readable
// report printed to stdout before anything is written — see
// writeEvalMergeManifest. Cases is a set (not a count) so its size is the
// number of DISTINCT case ids contributed, which is what makes a partial
// baseline (e.g. 2 of a role's 3 cases) visible at a glance instead of
// indistinguishable from a full one with extra reruns.
type mergeManifestEntry struct {
	Source         string
	AgentType      string
	Records        int
	Cases          map[string]bool
	DispatchErrors int
	EmptyModel     int
}

// validateAndMergeEvalRuns is the one place all three hard refusals live.
// None of them take an override flag — see this file's package doc comment
// for why: a baseline that silently blends prompts, drops a conflicting
// rerun, or mixes models is more dangerous than having no baseline, because
// it looks complete. If a genuine need to bypass one of these ever comes up,
// the fix is to fix the inputs (e.g. delete the stale directory, don't feed
// both attempts in), not to add a flag that trusts the caller to have
// checked by hand what this function exists to check mechanically.
func validateAndMergeEvalRuns(tagged []taggedRunRecord) (records []runRecord, model string, manifest []mergeManifestEntry, err error) {
	if len(tagged) == 0 {
		return nil, "", nil, fmt.Errorf("eval summarize: no run records found in any source")
	}

	// --- ① fingerprint must be identical for a given agent_type ----------
	// The fingerprint's entire purpose (docs/AGENT_CAPABILITY_DESIGN.md §8
	// 修订四: "指纹机制存在的理由正是防止拿旧基线给新提示词背书，这条不能自己
	// 破例") is to make exactly this mistake structurally impossible: a role
	// whose system prompt/skills/output schema changed between two sources
	// must never be merged into one baseline as if nothing changed.
	fpLocs := map[string]map[string][]string{} // agent_type -> fingerprint -> locations
	for _, t := range tagged {
		if fpLocs[t.AgentType] == nil {
			fpLocs[t.AgentType] = map[string][]string{}
		}
		fpLocs[t.AgentType][t.Fingerprint] = append(fpLocs[t.AgentType][t.Fingerprint], t.loc())
	}
	for _, agentType := range sortedKeys(fpLocs) {
		byFP := fpLocs[agentType]
		if len(byFP) <= 1 {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "eval summarize: agent_type %q has inconsistent fingerprints across sources — refusing to merge (a stale baseline must never vouch for a different prompt/skill/schema; see docs/AGENT_CAPABILITY_DESIGN.md §8 修订四):\n", agentType)
		for _, fp := range sortedKeys(byFP) {
			locs := append([]string(nil), byFP[fp]...)
			sort.Strings(locs)
			fmt.Fprintf(&b, "  fingerprint %s:\n", fp)
			for _, loc := range locs {
				fmt.Fprintf(&b, "    %s\n", loc)
			}
		}
		return nil, "", nil, fmt.Errorf("%s", b.String())
	}

	// --- ② no duplicate (agent_type, case, run) triples ------------------
	// This is precisely what "rerun a role's cases to double-check a
	// result" produces if the rerun's directory is fed in alongside the
	// original: two records claiming to be the same (role, case, run).
	// Picking one silently is the same bug just fixed for runs.jsonl itself
	// in a16a9ab (a silent overwrite of one run's result by another's) —
	// refusing outright, with every conflict listed, is the fix here too.
	type tripleKey struct {
		agentType, caseID string
		run               int
	}
	tripleLocs := map[tripleKey][]string{}
	var tripleOrder []tripleKey
	for _, t := range tagged {
		k := tripleKey{t.AgentType, t.Case, t.Run}
		if _, seen := tripleLocs[k]; !seen {
			tripleOrder = append(tripleOrder, k)
		}
		tripleLocs[k] = append(tripleLocs[k], t.loc())
	}
	var dupKeys []tripleKey
	for _, k := range tripleOrder {
		if len(tripleLocs[k]) > 1 {
			dupKeys = append(dupKeys, k)
		}
	}
	if len(dupKeys) > 0 {
		sort.Slice(dupKeys, func(i, j int) bool {
			if dupKeys[i].agentType != dupKeys[j].agentType {
				return dupKeys[i].agentType < dupKeys[j].agentType
			}
			if dupKeys[i].caseID != dupKeys[j].caseID {
				return dupKeys[i].caseID < dupKeys[j].caseID
			}
			return dupKeys[i].run < dupKeys[j].run
		})
		var b strings.Builder
		fmt.Fprintf(&b, "eval summarize: duplicate (agent_type, case, run) across sources — refusing to merge (silently keeping one copy is the same class of bug a16a9ab fixed for runs.jsonl itself); every conflict:\n")
		for _, k := range dupKeys {
			locs := append([]string(nil), tripleLocs[k]...)
			sort.Strings(locs)
			fmt.Fprintf(&b, "  %s/%s run %d: %s\n", k.agentType, k.caseID, k.run, strings.Join(locs, ", "))
		}
		return nil, "", nil, fmt.Errorf("%s", b.String())
	}

	// --- ③ model must be identical across every non-empty-model record ---
	// A record's Model is "" only when task.Stats was never recovered at
	// all (see applyRunStats/HasStats) — that run tells us nothing about
	// which model produced it, so it cannot possibly conflict with anything
	// and is exempted, but is still worth surfacing (see EmptyModel below)
	// since a baseline with many of them is missing a lot of its stats.
	modelLocs := map[string][]string{}
	for _, t := range tagged {
		if t.Model == "" {
			// Exempt: see the function doc above. Its count is surfaced
			// per-role in the manifest's EmptyModel column (built below),
			// not aggregated here.
			continue
		}
		modelLocs[t.Model] = append(modelLocs[t.Model], t.loc())
	}
	if len(modelLocs) > 1 {
		var b strings.Builder
		fmt.Fprintf(&b, "eval summarize: inconsistent model across sources — a baseline is defined per-model, refusing to merge:\n")
		for _, m := range sortedKeys(modelLocs) {
			locs := append([]string(nil), modelLocs[m]...)
			sort.Strings(locs)
			fmt.Fprintf(&b, "  model %q:\n", m)
			for _, loc := range locs {
				fmt.Fprintf(&b, "    %s\n", loc)
			}
		}
		return nil, "", nil, fmt.Errorf("%s", b.String())
	}
	for m := range modelLocs {
		model = m // at most one key survives the check above
	}

	// --- merge: flatten to []runRecord + build the per-source/role manifest
	type manifestKey struct{ source, agentType string }
	byKey := map[manifestKey]*mergeManifestEntry{}
	var order []manifestKey
	for _, t := range tagged {
		k := manifestKey{t.Source, t.AgentType}
		e, ok := byKey[k]
		if !ok {
			e = &mergeManifestEntry{Source: t.Source, AgentType: t.AgentType, Cases: map[string]bool{}}
			byKey[k] = e
			order = append(order, k)
		}
		e.Records++
		e.Cases[t.Case] = true
		if t.Error != "" {
			e.DispatchErrors++
		}
		if t.Model == "" {
			e.EmptyModel++
		}
		records = append(records, t.runRecord)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].source != order[j].source {
			return order[i].source < order[j].source
		}
		return order[i].agentType < order[j].agentType
	})
	for _, k := range order {
		manifest = append(manifest, *byKey[k])
	}
	return records, model, manifest, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// writeEvalMergeManifest prints the per-(source, agent_type) merge report:
// how many records each source contributed to each role, how many of those
// were dispatch errors, and how many carried no model (never got stats).
// This is the thing that lets an operator tell a 6/15-case partial baseline
// apart from a 15/15 complete one at a glance — a partial baseline is
// entirely legal here (this command's very first real use is rescuing one),
// it just must never be silently indistinguishable from a full one.
func writeEvalMergeManifest(out io.Writer, manifest []mergeManifestEntry) {
	fmt.Fprintf(out, "%-48s %-16s %8s %8s %14s %12s\n", "source", "agent_type", "records", "cases", "dispatch_err", "empty_model")
	for _, m := range manifest {
		fmt.Fprintf(out, "%-48s %-16s %8d %8d %14d %12d\n", m.Source, m.AgentType, m.Records, len(m.Cases), m.DispatchErrors, m.EmptyModel)
	}
}

// ---------------------------------------------------------------------------
// Run-coverage warning (a fourth check, deliberately NOT a hard refusal)
// ---------------------------------------------------------------------------

// evalRunCoverageWarnings flags every (agent_type, case) whose contributed
// run numbers are not exactly {1..runs} declared by --runs.
//
// This is a WARNING, not a fourth hard refusal alongside
// validateAndMergeEvalRuns' three (fingerprint/triple/model): this
// command's very first real use is rescuing a round that was killed
// partway through, and that data will routinely have a case with fewer
// runs than --runs declares — that is the expected shape of the input, not
// a corruption of it. A hard refusal here would block the command's main
// use case. But saying nothing is also wrong: --runs=3 with a case that
// only ever got runs {1,2} (or, worse, {1,3} — run 2 specifically missing,
// which a bare "2 of 3" count would hide) is silently invisible today,
// which cuts against this whole file's rule that a partial baseline must
// never look complete. A loud, explicit warning is the middle path: it
// doesn't block the rescue, but it also doesn't let the gap disappear.
func evalRunCoverageWarnings(tagged []taggedRunRecord, runs int) []string {
	type key struct{ agentType, caseID string }
	runSets := map[key]map[int]bool{}
	var order []key
	for _, t := range tagged {
		k := key{t.AgentType, t.Case}
		if runSets[k] == nil {
			runSets[k] = map[int]bool{}
			order = append(order, k)
		}
		runSets[k][t.Run] = true
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].agentType != order[j].agentType {
			return order[i].agentType < order[j].agentType
		}
		return order[i].caseID < order[j].caseID
	})

	var warnings []string
	for _, k := range order {
		runSet := runSets[k]
		complete := len(runSet) == runs
		if complete {
			for r := 1; r <= runs; r++ {
				if !runSet[r] {
					complete = false
					break
				}
			}
		}
		if complete {
			continue
		}
		var got []int
		for r := range runSet {
			got = append(got, r)
		}
		sort.Ints(got)
		warnings = append(warnings, fmt.Sprintf("WARNING: %s/%s has runs %s, expected %s", k.agentType, k.caseID, formatEvalRunSet(got), formatEvalRunSet(evalRunRange(runs))))
	}
	return warnings
}

func evalRunRange(runs int) []int {
	out := make([]int, runs)
	for i := range out {
		out[i] = i + 1
	}
	return out
}

func formatEvalRunSet(runs []int) string {
	parts := make([]string, len(runs))
	for i, r := range runs {
		parts[i] = strconv.Itoa(r)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// ---------------------------------------------------------------------------
// merge-manifest.txt: the merge report, on disk
// ---------------------------------------------------------------------------

// writeEvalMergeManifestFile writes <outDir>/merge-manifest.txt: the same
// report writeEvalMergeManifest prints to stdout, PLUS the absolute path of
// every source, the merge timestamp, and --runs/--timeout, all persisted
// next to summary.json.
//
// Without this file, summary.json produced by `eval summarize` is
// byte-for-byte indistinguishable from one a normal `agents` round
// produced — GeneratedAt is the MERGE moment, not when any run actually
// executed, and stdout (where the manifest otherwise only exists) gets
// scrolled away or redirected. Three months on, nothing on disk would say
// a given baseline was stitched from N sources, which ones, or that a case
// fell short of --runs. This is additive only: it does not touch
// buildEvalSummary/writeEvalSummary or the evalSummary struct.
func writeEvalMergeManifestFile(outDir string, sources []string, runs int, timeoutStr string, manifest []mergeManifestEntry, warnings []string, mergedAt time.Time) error {
	var b strings.Builder
	fmt.Fprintf(&b, "deepai eval summarize — merge manifest\n")
	fmt.Fprintf(&b, "merged_at: %s\n", mergedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "runs: %d\n", runs)
	fmt.Fprintf(&b, "timeout: %s\n", timeoutStr)
	fmt.Fprintf(&b, "\nsources (as given on the command line):\n")
	for _, s := range sources {
		abs, err := filepath.Abs(s)
		if err != nil {
			abs = s // best effort: still record something rather than fail the whole merge
		}
		fmt.Fprintf(&b, "  %s\n", abs)
	}
	fmt.Fprintf(&b, "\nper-source/role contribution:\n")
	writeEvalMergeManifest(&b, manifest)
	if len(warnings) > 0 {
		fmt.Fprintf(&b, "\nwarnings:\n")
		for _, w := range warnings {
			fmt.Fprintf(&b, "  %s\n", w)
		}
	}
	path := filepath.Join(outDir, "merge-manifest.txt")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("eval summarize: write %s: %w", path, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Output directory
// ---------------------------------------------------------------------------

// prepareEvalSummarizeOutDir creates outDir with os.Mkdir, the same
// atomic-name-claim primitive prepareEvalResultDir uses, but WITHOUT that
// function's auto-suffix retry: a rerun-for-review same-day collision is
// routine for `agents` (see prepareEvalResultDir's doc comment) but `--out`
// naming an already-existing directory here is much more likely a mistake
// (e.g. --out repeated across two invocations, or pointed at a directory
// that itself was one of the merge inputs) — and unlike `agents`, this
// command is decoupled from any deadline pressure that would make a hard
// stop costly, so there is nothing to gain by being permissive here the way
// the -2/-3 suffix search is for a multi-hour dispatch run.
//
// The parent directory is created with MkdirAll first (a plain convenience:
// --out is typically a fresh subdirectory of an existing eval/results/,
// but there's no reason to require the parent to already exist) — MkdirAll
// is safe here because it is the PARENT, never outDir itself, that this
// call can silently already contain: the leaf is still claimed atomically
// by the os.Mkdir immediately below.
func prepareEvalSummarizeOutDir(outDir string) error {
	if parent := filepath.Dir(outDir); parent != "." && parent != string(filepath.Separator) {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("eval summarize: mkdir %s: %w", parent, err)
		}
	}
	if err := os.Mkdir(outDir, 0o755); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("eval summarize: refusing to write to existing directory %s (--out must not already exist)", outDir)
		}
		return fmt.Errorf("eval summarize: mkdir %s: %w", outDir, err)
	}
	return nil
}
