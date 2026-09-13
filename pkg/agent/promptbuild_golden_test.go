package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// --- M6 latency, fingerprint fix: BuildSystemPrompt refactor safety net ----
//
// caseFingerprint (pkg/commands/agent_eval.go) used to hash only the role's
// base system prompt, silently missing every gated section
// BuildSystemPrompt actually appends (file-op rule, search-tool
// recommendations, batchToolCallsPrompt, todo guidance, delegation) — see
// that function's doc comment for the full defect writeup. The fix pulls
// BuildSystemPrompt's section-assembly out into the exported, *Agent-free
// AssembleSystemPrompt so the eval harness can call the SAME code
// path instead of re-deriving the gate logic a second time (see that
// function's doc comment for why a second implementation is the wrong
// fix).
//
// That refactor must not change BuildSystemPrompt's OUTPUT by a single
// byte — it is still the exact string sent to the model as the system
// role. These hashes were captured from the pre-refactor implementation
// (inline section-assembly in BuildSystemPrompt, no
// AssembleSystemPrompt) across representative configurations —
// interactive with every gate plus delegation, and a bare non-interactive
// subagent (the shape that matters most: it's what every eval-harness
// equivalence case below actually compares against) — and must keep
// matching after the refactor. Plan mode is deliberately NOT included here:
// its "Plan file: <path>" line embeds time.Now()-derived filename, so its
// BuildSystemPrompt output is never byte-stable across two runs even
// without any refactor — plan mode's behavior is instead pinned by the
// extensive strings.Contains-based tests already in this package
// (TestTeamDelegation_OmittedInPlanMode and neighbors in systemprompt_test.go).

func goldenNoopHandler(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
	return models.ToolResult{}, nil
}

func buildGoldenInteractiveWithDelegation() string {
	reg := tools.NewRegistry()
	for _, tool := range builtinFileToolsForTest() {
		_ = reg.Register(tool)
	}
	_ = reg.Register(models.Tool{Name: "grep", ParallelSafe: true, Handler: goldenNoopHandler})
	_ = reg.Register(models.Tool{Name: "task", Handler: goldenNoopHandler})
	a := New(AgentConfig{
		LLMProvider:  &captureProvider{},
		Tools:        reg,
		Model:        "m",
		AgentCatalog: []AgentInfo{{Type: "coder", Description: "writes code"}},
	})
	return a.BuildSystemPrompt()
}

// buildGoldenAllGatesOn turns EVERY remaining assembleSystemPromptSections
// gate on at once: file tools + grep (fileop + search), todo_write (todo),
// and task + a non-empty catalog while interactive (delegation). Unlike
// buildGoldenInteractiveWithDelegation above, which omits todo_write, this
// case carries both the todo and delegation sections at once (read_file and
// grep are still registered ParallelSafe: true — harmless now that no gate
// reads ParallelSafe, kept only so this fixture still resembles a real file+
// search-tool registry).
//
// This is the fix for a real, verified golden-test blind spot (M1 mutation:
// swapping the (then also present) batch and todo sections in
// assembleSystemPromptSections was invisible to the pre-existing two cases,
// because NEITHER case ever carried both sections at once —
// buildGoldenInteractiveWithDelegation omits todo_write; buildGoldenNon
// InteractiveBashOnly has neither). One hash here pins the presence AND
// relative order of all section kinds (BASE, FILEOP, SEARCH, TODO, DELEG)
// simultaneously. batchToolCallsPrompt (M6 latency) was removed after a
// real-world eval found it never reduced turn count on any of three task
// shapes while adding ~15% more tool calls on one of them — see this
// package's git history (the batchToolCallsPrompt removal commit) for the
// measurement. This case's hash changed as a result; its coverage of the
// TODO/DELEG ordering is unaffected and stays valuable.
func buildGoldenAllGatesOn() string {
	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{Name: "read_file", ParallelSafe: true, Handler: goldenNoopHandler})
	_ = reg.Register(models.Tool{Name: "grep", ParallelSafe: true, Handler: goldenNoopHandler})
	_ = reg.Register(models.Tool{Name: "todo_write", Handler: goldenNoopHandler})
	_ = reg.Register(models.Tool{Name: "task", Handler: goldenNoopHandler})
	a := New(AgentConfig{
		LLMProvider:  &captureProvider{},
		Tools:        reg,
		Model:        "m",
		AgentCatalog: []AgentInfo{{Type: "coder", Description: "writes code"}},
	})
	return a.BuildSystemPrompt()
}

func buildGoldenNonInteractiveBashOnly() string {
	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{Name: "bash", Handler: goldenNoopHandler})
	a := New(AgentConfig{LLMProvider: &captureProvider{}, Tools: reg, Model: "m", NonInteractive: true})
	return a.BuildSystemPrompt()
}

// Golden values move only when the prompt TEXT is deliberately changed, never
// as a side effect of restructuring how it is assembled. Last moved: the
// "Parallel delegation" section gained the rule tying fan-out width to the
// size of the change (+447 bytes on both delegation-carrying cases;
// nonInteractive_bash_only carries no delegation section and is unchanged).
func TestBuildSystemPrompt_GoldenBytesUnchangedByRefactor(t *testing.T) {
	cases := []struct {
		name     string
		build    func() string
		wantLen  int
		wantHash string
	}{
		{
			name:     "interactive_full_with_delegation",
			build:    buildGoldenInteractiveWithDelegation,
			wantLen:  5234,
			wantHash: "c030cd97a8f6d5074b748ba07c7511f894a32a307ba5a0dd3721f79c538ee3a7",
		},
		{
			name:     "nonInteractive_bash_only",
			build:    buildGoldenNonInteractiveBashOnly,
			wantLen:  1863,
			wantHash: "333d6ecc6cc3b57c6bab549bf9c3e7982ae74e35a69e47fe95abf669306e4bbb",
		},
		{
			name:     "all_gates_on",
			build:    buildGoldenAllGatesOn,
			wantLen:  6006,
			wantHash: "c21387ae21878035681867ec07cdc1b3b605ace10e2c43a6fdaae2dddc4327d8",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sp := c.build()
			if len(sp) != c.wantLen {
				t.Errorf("%s: len = %d, want %d (BuildSystemPrompt output size changed)", c.name, len(sp), c.wantLen)
			}
			sum := sha256.Sum256([]byte(sp))
			if got := hex.EncodeToString(sum[:]); got != c.wantHash {
				t.Errorf("%s: BuildSystemPrompt output hash = %s, want %s\n(the section-assembly refactor must not change a single byte of BuildSystemPrompt's output)\ngot:\n%s", c.name, got, c.wantHash, sp)
			}
		})
	}
}
