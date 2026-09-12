package commands

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/memory"
	"github.com/spf13/cobra"
)

func addMemory(topLevel *cobra.Command) {
	cmd := &cobra.Command{
		Use:   "memory",
		Short: "Inspect stored memory",
	}
	cmd.AddCommand(cmdMemoryGateStats())
	cmd.AddCommand(cmdMemoryList())
	cmd.AddCommand(cmdMemoryConsolidate())
	topLevel.AddCommand(cmd)
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

func cmdMemoryList() *cobra.Command {
	var category string
	var minConfidence float64
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List stored memory facts across all scopes, filterable by category and confidence",
		RunE: func(cmd *cobra.Command, args []string) error {
			report, err := listFactsReport(cmd.Context(), DBFile(), category, minConfidence, limit)
			if err != nil {
				return err
			}
			fmt.Fprint(os.Stdout, report)
			return nil
		},
	}
	cmd.Flags().StringVar(&category, "category", "", "only facts with this category (e.g. preference)")
	cmd.Flags().Float64Var(&minConfidence, "min-confidence", 0, "only facts with confidence >= this (0-1)")
	cmd.Flags().IntVar(&limit, "limit", 100, "maximum facts to print (0 = all)")
	return cmd
}

// listFactsReport mirrors gateStatsReport: takes a dbPath (not DBFile()) so
// tests run against a t.TempDir() database, never the user's real store.
func listFactsReport(ctx context.Context, dbPath, category string, minConfidence float64, limit int) (string, error) {
	store, err := memory.NewSQLiteStore(ctx, dbPath)
	if err != nil {
		return "", err
	}
	defer store.Close()
	if err := store.AutoMigrate(ctx); err != nil {
		return "", err
	}
	facts, err := store.ListAllFacts(ctx)
	if err != nil {
		return "", err
	}
	return renderFactList(facts, category, minConfidence, limit), nil
}

// renderFactList is a pure function of the fetched rows so the filter and
// layout behaviours are unit-testable without a database.
func renderFactList(facts []memory.ScopedFact, category string, minConfidence float64, limit int) string {
	category = strings.TrimSpace(category)
	filtered := make([]memory.ScopedFact, 0, len(facts))
	for _, f := range facts {
		if category != "" && f.Category != category {
			continue
		}
		if f.Confidence < minConfidence {
			continue
		}
		filtered = append(filtered, f)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Memory facts\n")
	fmt.Fprintf(&b, "============\n\n")

	if len(filtered) == 0 {
		fmt.Fprintf(&b, "No memory facts recorded for this filter.\n")
		fmt.Fprintf(&b, "\nFilters: category=%q, min-confidence=%.2f, limit=%d\n", category, minConfidence, limit)
		fmt.Fprintf(&b, "\nSQL used:\n%s\n", memory.ListFactsQuerySQL)
		return b.String()
	}

	shown := filtered
	if limit > 0 && len(filtered) > limit {
		shown = filtered[:limit]
	}

	fmt.Fprintf(&b, "%-24s %-28s %-12s %5s  %s\n", "scope", "id", "category", "conf", "content")
	for _, f := range shown {
		fmt.Fprintf(&b, "%-24s %-28s %-12s %5.2f  %s\n", shortScope(f.ScopeKey), truncate(f.ID, 28), truncate(f.Category, 12), f.Confidence, firstLine(f.Content))
	}

	if len(shown) < len(facts) {
		fmt.Fprintf(&b, "\n(%d of %d facts shown; filters: category=%q, min-confidence=%.2f, limit=%d)\n", len(shown), len(facts), category, minConfidence, limit)
	}
	return b.String()
}

// shortScope renders a storage key as a compact scope label: bare session
// ids become session:<first 8 chars>, __scope__:user:<id>: becomes user.
func shortScope(scopeKey string) string {
	scope := memory.ParseScopeKey(scopeKey)
	switch scope.Type {
	case memory.ScopeUser:
		return "user"
	case memory.ScopeAgent:
		return "agent:" + scope.ID
	case memory.ScopeGroup:
		return "group:" + scope.ID
	default:
		id := scope.ID
		if len(id) > 8 {
			id = id[:8]
		}
		if id == "" {
			return "session"
		}
		return "session:" + id
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// firstLine collapses a fact to its first line so multi-line content cannot
// break the one-fact-per-row layout the skill's diff preview parses.
func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}

// ---------------------------------------------------------------------------
// consolidate
// ---------------------------------------------------------------------------

func cmdMemoryConsolidate() *cobra.Command {
	var category string
	var threshold float64
	var user string
	var apply bool
	cmd := &cobra.Command{
		Use:   "consolidate",
		Short: "Collapse near-duplicate memory facts across scopes (dry-run by default; --apply writes)",
		Long: `Collapse near-duplicate facts of one category into a single survivor each.

Preferences used to be learned per session, so the same preference re-emerged
as a new fact under every session that expressed it. This command clusters
those rows by content similarity, keeps the highest-confidence member, drops
the rest, and moves survivors into the user-scope document the agent injects.

Prints the plan without writing anything unless --apply is passed.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if user == "" {
				cwd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("determine --user default from cwd: %w", err)
				}
				user = cwd
			}
			report, err := consolidateReport(cmd.Context(), DBFile(), category, threshold, user, apply)
			if err != nil {
				return err
			}
			fmt.Fprint(os.Stdout, report)
			return nil
		},
	}
	cmd.Flags().StringVar(&category, "category", "preference", "category to consolidate")
	cmd.Flags().Float64Var(&threshold, "threshold", 0.55, "cosine similarity above which two facts are duplicates (0-1)")
	cmd.Flags().StringVar(&user, "user", "", "user scope id for survivors (default: current directory, matching the REPL's WorkDir scoping)")
	cmd.Flags().BoolVar(&apply, "apply", false, "write the plan; without it nothing is modified")
	return cmd
}

// consolidateReport is split from the cobra RunE for the same reason as
// gateStatsReport/listFactsReport: it takes a dbPath so tests never touch
// the user's real store.
func consolidateReport(ctx context.Context, dbPath, category string, threshold float64, user string, apply bool) (string, error) {
	store, err := memory.NewSQLiteStore(ctx, dbPath)
	if err != nil {
		return "", err
	}
	defer store.Close()
	if err := store.AutoMigrate(ctx); err != nil {
		return "", err
	}
	facts, err := store.ListAllFacts(ctx)
	if err != nil {
		return "", err
	}
	targetKey := memory.UserScope(user).Key()
	groups := memory.PlanConsolidation(facts, category, threshold)

	if apply {
		if _, err := store.ApplyConsolidation(ctx, groups, targetKey, time.Now().UTC()); err != nil {
			return "", err
		}
	}
	return renderConsolidationPlan(groups, targetKey, apply), nil
}

// renderConsolidationPlan is pure so grouping display and the dry-run marker
// are unit-testable without a database.
func renderConsolidationPlan(groups []memory.ConsolidationGroup, targetKey string, applied bool) string {
	var b strings.Builder
	if applied {
		fmt.Fprintf(&b, "Consolidation applied\n")
		fmt.Fprintf(&b, "====================\n\n")
	} else {
		fmt.Fprintf(&b, "Consolidation plan (dry run — pass --apply to write)\n")
		fmt.Fprintf(&b, "====================================================\n\n")
	}

	dropTotal := 0
	for _, g := range groups {
		dropTotal += g.DroppedCount()
	}
	if len(groups) == 0 {
		fmt.Fprintf(&b, "No near-duplicate groups found. Nothing to do.\n")
		return b.String()
	}

	fmt.Fprintf(&b, "%d groups, %d facts dropped, survivors move to %s\n\n", len(groups), dropTotal, targetKey)
	for i, g := range groups {
		origin := "already in target"
		if g.Survivor.ScopeKey != targetKey {
			origin = "moves from " + shortScope(g.Survivor.ScopeKey)
		}
		fmt.Fprintf(&b, "group %d: keep %s (conf %.2f, %s)\n", i+1, g.Survivor.ID, g.Survivor.Confidence, origin)
		for _, f := range g.Dropped {
			fmt.Fprintf(&b, "  drop %-14s %-28s (conf %.2f) %s\n", shortScope(f.ScopeKey), truncate(f.ID, 28), f.Confidence, firstLine(f.Content))
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// gate-stats
// ---------------------------------------------------------------------------

func cmdMemoryGateStats() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gate-stats",
		Short: "Summarize recorded auto-refine gate verdicts (approve/reject/error, timing, no-op rate)",
		RunE: func(cmd *cobra.Command, args []string) error {
			report, err := gateStatsReport(cmd.Context(), DBFile())
			if err != nil {
				return err
			}
			fmt.Fprint(os.Stdout, report)
			return nil
		},
	}
	return cmd
}

// gateStatsReport opens the memory store at dbPath, migrates it (so a
// database that predates this feature — or has no memory tables at all —
// just reports zero rows rather than erroring on a missing table), and
// renders the full gate-stats report as text.
//
// Split out from the cobra RunE purely for testability: it takes a path
// rather than reading DBFile() itself, so tests exercise it against a
// t.TempDir() database and never touch the user's real ~/.deepai/deepai.db.
func gateStatsReport(ctx context.Context, dbPath string) (string, error) {
	store, err := memory.NewSQLiteStore(ctx, dbPath)
	if err != nil {
		return "", err
	}
	defer store.Close()
	if err := store.AutoMigrate(ctx); err != nil {
		return "", err
	}
	records, err := store.ListGateVerdicts(ctx)
	if err != nil {
		return "", err
	}
	return renderGateStats(memory.ComputeGateStats(records)), nil
}

// minGateSampleForConfidence is the smallest approve+reject sample this
// report treats as informative enough to read the rejection rate off of.
// HANDOFF.md §5.4 records the same lesson from the eval harness: at n=9, 80%
// and 100% are not distinguishable from noise. The auto-refine gate fires
// roughly once per five human turns (defaultRefineInterval), so production
// history here is measured in dozens of rows, not thousands — this warning
// is expected to fire for a long time, not a defensive-programming leftover.
const minGateSampleForConfidence = 30

// renderGateStats is a pure function of GateStats so the three required
// report shapes (empty, small-n, full data) are unit-testable without a
// database. It must not let n=1 through as a quiet, confident-looking number:
// silence here is exactly how this data went unmeasured for three months
// (see docs/REFINE_DESIGN.md §7.2 and HANDOFF.md §12).
func renderGateStats(s memory.GateStats) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Auto-refine gate verdicts\n")
	fmt.Fprintf(&b, "=========================\n")

	if s.Total == 0 {
		fmt.Fprintf(&b, "No gate verdicts recorded yet.\n")
		fmt.Fprintf(&b, "\nSQL used:\n%s\n", memory.GateStatsQuerySQL)
		return b.String()
	}

	span := s.NewestAt.Sub(s.OldestAt).Round(time.Hour)
	fmt.Fprintf(&b, "n = %d verdicts, %s .. %s (span %s)\n",
		s.Total, s.OldestAt.Format("2006-01-02"), s.NewestAt.Format("2006-01-02"), span)
	fmt.Fprintf(&b, "  approve: %d   reject: %d   error (fail-open, excluded below): %d\n",
		s.Approved, s.Rejected, s.Errored)

	fmt.Fprintf(&b, "\nRejection rate (denominator = approve+reject; error is excluded — see docs/REFINE_DESIGN.md §7.2):\n")
	if rate, ok := s.RejectionRate(); !ok {
		fmt.Fprintf(&b, "  n/a: no approve/reject verdicts recorded (only errors, or nothing yet).\n")
	} else {
		fmt.Fprintf(&b, "  %.1f%% (%d of %d)\n", rate*100, s.Rejected, s.Approved+s.Rejected)
		switch {
		case rate < 0.60:
			fmt.Fprintf(&b, "  -> BELOW the 60-80%% break-even band. On this evidence the gate costs more than it saves.\n")
		case rate > 0.80:
			fmt.Fprintf(&b, "  -> ABOVE the 60-80%% break-even band. The gate is paying for itself.\n")
		default:
			fmt.Fprintf(&b, "  -> INSIDE the 60-80%% break-even band. Marginal either way.\n")
		}
		if n := s.Approved + s.Rejected; n < minGateSampleForConfidence {
			fmt.Fprintf(&b, "  CAUTION: only %d approve/reject verdicts (< %d). This rate is not resolvable from noise yet\n", n, minGateSampleForConfidence)
			fmt.Fprintf(&b, "  (HANDOFF.md §5.4: n=9 could not tell 80%% from 100%% apart). Read this as a trend, not a verdict.\n")
		}
	}

	fmt.Fprintf(&b, "\nTiming (ms) — the LLM relay does not return token usage, so latency is the only cost proxy (HANDOFF.md §7):\n")
	fmt.Fprintf(&b, "  gate_ms:    p50=%.0f  p90=%.0f  (n=%d)\n", s.GateMsP50, s.GateMsP90, s.Total)
	if s.ExtractSampleSize == 0 {
		fmt.Fprintf(&b, "  extract_ms: no completed extractions recorded yet.\n")
	} else {
		fmt.Fprintf(&b, "  extract_ms: p50=%.0f  p90=%.0f  (n=%d)\n", s.ExtractMsP50, s.ExtractMsP90, s.ExtractSampleSize)
		if ratio, ok := s.GateToExtractRatio(); ok {
			fmt.Fprintf(&b, "  measured C(gate)/C(extract) [median ratio] = %.2f  (docs/REFINE_DESIGN.md §7.2 assumed 0.6-0.8)\n", ratio)
		}
	}

	fmt.Fprintf(&b, "\nOf approved verdicts with a completed extraction, the share that saved nothing (saved=0):\n")
	if noOp, ok := s.NoOpRate(); !ok {
		fmt.Fprintf(&b, "  n/a: no approved extraction has completed yet.\n")
	} else {
		fmt.Fprintf(&b, "  %.1f%% (%d of %d)\n", noOp*100, s.NoOpApproved, s.ApprovedWithSample)
	}

	fmt.Fprintf(&b, "\nSQL used:\n%s\n", memory.GateStatsQuerySQL)
	return b.String()
}
