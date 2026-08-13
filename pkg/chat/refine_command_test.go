package chat

import (
	"strings"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/memory"
)

func TestParseRefineCommand(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		args    string
		want    refineAction
		wantArg string
		wantErr bool
	}{
		{name: "bare runs a refine", args: "", want: refineRun},
		{name: "whitespace only", args: "   ", want: refineRun},
		{name: "undo", args: "undo", want: refineUndo},
		{name: "list", args: "list", want: refineList},
		{name: "status", args: "status", want: refineStatus},
		{name: "on", args: "on", want: refineOn},
		{name: "off", args: "off", want: refineOff},
		{name: "case insensitive", args: "OFF", want: refineOff},
		{name: "rollback with id", args: "rollback refine_17", want: refineRollback, wantArg: "refine_17"},
		{name: "rollback without id", args: "rollback", wantErr: true},

		// Free text would be ambiguous with the reserved subcommands: is
		// "/refine list everything I said" a listing or an instruction?
		{name: "free text rejected", args: "remember that I use tabs", wantErr: true},
		{name: "unknown single word rejected", args: "everything", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			action, arg, err := parseRefineCommand(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error for %q, got action %v", tc.args, action)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRefineCommand(%q) error = %v", tc.args, err)
			}
			if action != tc.want {
				t.Fatalf("action = %v, want %v", action, tc.want)
			}
			if arg != tc.wantArg {
				t.Fatalf("arg = %q, want %q", arg, tc.wantArg)
			}
		})
	}
}

func TestFormatRefinementListMergesScopesNewestFirst(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	session := []memory.RefinementRecord{
		{ID: "refine_3", Rationale: "third", CreatedAt: base.Add(2 * time.Minute)},
		{ID: "refine_1", Rationale: "first", CreatedAt: base},
	}
	user := []memory.RefinementRecord{
		{ID: "refine_2", Rationale: "second", CreatedAt: base.Add(time.Minute)},
	}

	out := formatRefinementList(session, user, 10)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 entries, got %d:\n%s", len(lines), out)
	}
	// Both partitions belong to one history; interleaving them by time is the
	// only ordering that reads correctly.
	for i, wantID := range []string{"refine_3", "refine_2", "refine_1"} {
		if !strings.Contains(lines[i], wantID) {
			t.Fatalf("line %d = %q, want it to mention %s", i, lines[i], wantID)
		}
	}
	if !strings.Contains(lines[0], "session") || !strings.Contains(lines[1], "user") {
		t.Fatalf("each entry must name its scope:\n%s", out)
	}
}

func TestFormatRefinementListHonorsTheLimit(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	var session []memory.RefinementRecord
	for i := 0; i < 10; i++ {
		session = append(session, memory.RefinementRecord{
			ID:        "refine_" + string(rune('a'+i)),
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}

	out := formatRefinementList(session, nil, 3)
	if got := len(strings.Split(strings.TrimSpace(out), "\n")); got != 3 {
		t.Fatalf("want 3 lines, got %d:\n%s", got, out)
	}
}

func TestFormatRefinementListWhenEmpty(t *testing.T) {
	t.Parallel()

	out := formatRefinementList(nil, nil, 10)
	if strings.TrimSpace(out) == "" {
		t.Fatal("an empty history still needs to say so")
	}
}

func TestFindUndoTargetsPairsBothScopes(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	session := []memory.RefinementRecord{
		{ID: "refine_3", PairID: "pair-b", CreatedAt: base.Add(time.Minute)},
		{ID: "refine_1", PairID: "pair-a", CreatedAt: base},
	}
	user := []memory.RefinementRecord{
		{ID: "refine_4", PairID: "pair-b", CreatedAt: base.Add(time.Minute)},
		{ID: "refine_2", PairID: "pair-a", CreatedAt: base},
	}

	sessionID, userID, err := findUndoTargets(session, user)
	if err != nil {
		t.Fatalf("findUndoTargets() error = %v", err)
	}
	if sessionID != "refine_3" {
		t.Fatalf("session target = %q, want the newest record", sessionID)
	}
	if userID != "refine_4" {
		t.Fatalf("user target = %q, want the record sharing its PairID", userID)
	}
}

func TestFindUndoTargetsToleratesAMissingHalf(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	session := []memory.RefinementRecord{{ID: "refine_1", PairID: "pair-a", CreatedAt: base}}

	// The user scope legitimately has no record when that extraction changed
	// nothing, so it was never written.
	sessionTarget, userTarget, err := findUndoTargets(session, nil)
	if err != nil {
		t.Fatalf("findUndoTargets() error = %v", err)
	}
	if sessionTarget != "refine_1" || userTarget != "" {
		t.Fatalf("targets = %q/%q, want refine_1 and no pair", sessionTarget, userTarget)
	}
}

func TestFindUndoTargetsWithNoHistory(t *testing.T) {
	t.Parallel()

	if _, _, err := findUndoTargets(nil, nil); err == nil {
		t.Fatal("want an error when there is nothing to undo")
	}
}

func TestAutoRefineEnabledSessionOverride(t *testing.T) {
	t.Parallel()

	enabled := &ChatRepl{cfg: ReplConfig{MemoryAutoRefine: true}}
	if !enabled.autoRefineEnabled() {
		t.Fatal("config default should apply when no override is set")
	}

	// "/refine off" is session-scoped: it must not rewrite config.yaml, so a
	// restart returns to the configured default.
	enabled.setAutoRefine(false)
	if enabled.autoRefineEnabled() {
		t.Fatal("session override should win over the config default")
	}

	disabled := &ChatRepl{cfg: ReplConfig{MemoryAutoRefine: false}}
	disabled.setAutoRefine(true)
	if !disabled.autoRefineEnabled() {
		t.Fatal("session override should be able to enable as well as disable")
	}
}

func TestRefineStatusReportsTheCadenceActuallyInEffect(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		cfg      ReplConfig
		override *bool
		wantSaid string
	}{
		{
			name:     "gate on uses the configured cadence",
			cfg:      ReplConfig{MemoryAutoRefine: true, MemoryRefineInterval: 7},
			wantSaid: "every 7 turns",
		},
		{
			// resolveRefineInterval maps a disabling config to 0, and the REPL then
			// extracts unconditionally on the fallback cadence. Reporting "every 0
			// turns" describes something that never happens.
			name:     "disabled cadence reports the fallback",
			cfg:      ReplConfig{MemoryAutoRefine: true, MemoryRefineInterval: 0},
			wantSaid: "every 5 turns",
		},
		{
			name:     "session override reports the fallback",
			cfg:      ReplConfig{MemoryAutoRefine: true, MemoryRefineInterval: 7},
			override: boolPtr(false),
			wantSaid: "every 5 turns",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := &ChatRepl{cfg: tc.cfg}
			if tc.override != nil {
				r.setAutoRefine(*tc.override)
			}
			got := r.refineStatusText()
			if !strings.Contains(got, tc.wantSaid) {
				t.Fatalf("status = %q, want it to contain %q", got, tc.wantSaid)
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }
