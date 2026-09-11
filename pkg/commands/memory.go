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
	topLevel.AddCommand(cmd)
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
