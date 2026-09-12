package commands

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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
		// The full fact ID is printed (not truncated): the update-instructions
		// skill matches on it verbatim, so a truncated ID here would be
		// unusable as a reference.
		fmt.Fprintf(&b, "%-24s %-28s %-12s %5.2f  %s\n", shortScope(f.ScopeKey), f.ID, truncate(f.Category, 12), f.Confidence, firstLine(f.Content))
	}

	if len(shown) < len(facts) {
		fmt.Fprintf(&b, "\n(%d of %d facts shown; filters: category=%q, min-confidence=%.2f, limit=%d)\n", len(shown), len(facts), category, minConfidence, limit)
	}
	return b.String()
}

// shortScope renders a storage key as a compact scope label: bare session
// ids become session:<first 8 chars>, __scope__:user:<id>: becomes
// user:<last path segment of id> — user scope IDs are work directories
// (UserScope(WorkDir)), so different projects' user scopes would otherwise
// all render as the same indistinguishable "user" label.
func shortScope(scopeKey string) string {
	scope := memory.ParseScopeKey(scopeKey)
	switch scope.Type {
	case memory.ScopeUser:
		if scope.ID == "" {
			return "user"
		}
		return "user:" + lastPathSegment(scope.ID)
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

// lastPathSegment returns the final "/"-separated component of a path-like
// scope ID (e.g. "/Users/x/proj" -> "proj"), so two different projects'
// otherwise-identical "user" scope labels can be told apart at a glance.
func lastPathSegment(id string) string {
	id = strings.TrimRight(id, "/")
	if idx := strings.LastIndexByte(id, '/'); idx >= 0 {
		id = id[idx+1:]
	}
	return id
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
	var allScopes bool
	cmd := &cobra.Command{
		Use:   "consolidate",
		Short: "Collapse near-duplicate memory facts across scopes (dry-run by default; --apply writes)",
		Long: `Collapse near-duplicate facts of one category into a single survivor each.

Preferences used to be learned per session, so the same preference re-emerged
as a new fact under every session that expressed it. This command clusters
those rows by content similarity, keeps the highest-confidence member, drops
the rest, and moves survivors into the user-scope document the agent injects.

Prints the plan without writing anything unless --apply is passed.

By default only this project's user scope (--user, default: current
directory) and this project's session scopes are considered — a session
belongs to this project when its cwd matches --user, or when its cwd is
unknown (unset, or the session row can't be found at all), since ownership
can't be determined either way and excluding it would defeat the point of
cleaning up the pre-existing per-session preferences this command exists
for. Both "user scope" and "session scope" are really per-project data:
ListAllFacts sees every project's memories, and without this restriction
--apply here would move or delete another project's preferences. Pass
--all-scopes to consolidate across every scope in the database, including
other projects'.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if user == "" {
				cwd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("determine --user default from cwd: %w", err)
				}
				user = cwd
			}
			report, err := consolidateReport(cmd.Context(), DBFile(), category, threshold, user, apply, allScopes)
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
	cmd.Flags().BoolVar(&allScopes, "all-scopes", false, "consolidate across every scope in the database, not just this project's user scope and this project's sessions — WARNING: with --apply this can move or delete another project's memories")
	return cmd
}

// consolidateReport is split from the cobra RunE for the same reason as
// gateStatsReport/listFactsReport: it takes a dbPath so tests never touch
// the user's real store.
func consolidateReport(ctx context.Context, dbPath, category string, threshold float64, user string, apply, allScopes bool) (string, error) {
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
	if !allScopes {
		facts, err = restrictToProjectScopes(ctx, dbPath, facts, targetKey, user)
		if err != nil {
			return "", err
		}
	}
	groups := memory.PlanConsolidation(facts, category, threshold, targetKey)

	if apply {
		if _, err := store.ApplyConsolidation(ctx, groups, targetKey, time.Now().UTC()); err != nil {
			return "", err
		}
	}
	return renderConsolidationPlan(groups, targetKey, apply), nil
}

// restrictToProjectScopes keeps only facts that belong to this project:
// targetKey itself, and session scopes owned by this project (see
// loadSessionCWDs / sessionBelongsToProject). Every other scope is dropped —
// UserScope(WorkDir) makes "user scope" effectively per-project, and
// ListAllFacts is whole-database, so without this filter a consolidate run
// in project A can move or delete project B's memories, including project
// B's own session-scoped ones (a bare session scope is not "everyone's" any
// more than a user scope is — it belongs to whichever project it was
// recorded under).
func restrictToProjectScopes(ctx context.Context, dbPath string, facts []memory.ScopedFact, targetKey, user string) ([]memory.ScopedFact, error) {
	cwds, haveSessions, err := loadSessionCWDs(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	targetNorm := normalizeCWDForScopeFilter(user)

	out := make([]memory.ScopedFact, 0, len(facts))
	for _, f := range facts {
		if f.ScopeKey == targetKey {
			out = append(out, f)
			continue
		}
		if memory.ParseScopeKey(f.ScopeKey).Type != memory.ScopeSession {
			continue // another project's user/agent/group scope: only --all-scopes includes these
		}
		if !haveSessions {
			// No `sessions` table at all (a memory-only database, or one that
			// predates the chat schema migration): session ownership can't be
			// determined, so fall back to the pre-fix behavior of keeping
			// every bare session scope rather than erroring or excluding data
			// this command has no way to attribute.
			out = append(out, f)
			continue
		}
		cwd, known := cwds[f.ScopeKey]
		// An empty cwd is a session that predates cwd tracking (see
		// pkg/chat/session.go's `cwd TEXT DEFAULT ''`) — exactly the
		// pre-8b1295e per-session preference rows this command exists to
		// clean up, so excluding them by default would defeat the command's
		// purpose. A session id with no row at all (deleted session, or a
		// document written under a key that was never a real session) is
		// the same "unknown owner" situation and is treated the same way.
		if !known || cwd == "" || cwd == user || cwd == targetNorm {
			out = append(out, f)
		}
	}
	return out, nil
}

// loadSessionCWDs reads sessions.id -> cwd from the chat schema living in
// the same database file, so restrictToProjectScopes can tell which project
// a bare session scope belongs to. It opens its own short-lived connection
// rather than reaching into memory.SQLiteStore's unexported *sql.DB (which
// pkg/memory does not expose) — a second connection to the same WAL-mode
// file alongside the store's own is a normal, cheap read.
//
// The second return value is false when the `sessions` table does not exist
// at all (a memory-only database, or one that predates the chat schema
// migration): the caller then falls back to keeping every bare session
// scope, since ownership cannot be determined either way.
func loadSessionCWDs(ctx context.Context, dbPath string) (map[string]string, bool, error) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, false, fmt.Errorf("open database for session scope filter: %w", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `select id, cwd from sessions`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("list sessions for scope filter: %w", err)
	}
	defer rows.Close()

	cwds := make(map[string]string)
	for rows.Next() {
		var id, cwd string
		if err := rows.Scan(&id, &cwd); err != nil {
			return nil, false, fmt.Errorf("scan session for scope filter: %w", err)
		}
		cwds[id] = cwd
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("list sessions for scope filter: %w", err)
	}
	return cwds, true, nil
}

// normalizeCWDForScopeFilter mirrors pkg/chat's unexported normalizeCWD
// (EvalSymlinks + Clean): sessions.cwd is written through that function, but
// the target project's directory here comes from a bare os.Getwd() (or
// --user), so without normalizing both sides, a symlinked path (macOS
// /tmp vs /private/tmp) would fail to match its own project's sessions.
func normalizeCWDForScopeFilter(cwd string) string {
	if cwd == "" {
		return cwd
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(cwd)
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
		rename := ""
		if g.TargetID != "" {
			rename = fmt.Sprintf(", renamed to %s in target", g.TargetID)
		}
		fmt.Fprintf(&b, "group %d: keep %s (conf %.2f, %s)%s\n", i+1, g.Survivor.ID, g.Survivor.Confidence, origin, rename)
		for _, f := range g.Dropped {
			// The full fact ID is printed (not truncated), same reasoning as
			// renderFactList: the update-instructions skill matches on it verbatim.
			fmt.Fprintf(&b, "  drop %-14s %-28s (conf %.2f) %s\n", shortScope(f.ScopeKey), f.ID, f.Confidence, firstLine(f.Content))
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
