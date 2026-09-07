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
// (TestBatchGuidance_PresentInPlanMode and neighbors in batch_prompt_test.go,
// TestTeamDelegation_OmittedInPlanMode and neighbors in systemprompt_test.go).

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

// buildGoldenAllGatesOn turns EVERY assembleSystemPromptSections gate on at
// once: file tools + grep (fileop + search), two ParallelSafe tools (batch),
// todo_write (todo), and task + a non-empty catalog while interactive
// (delegation). Unlike buildGoldenInteractiveWithDelegation above — which
// only has ONE ParallelSafe tool (grep) and so never crosses
// hasMultipleParallelSafeTools' >=2 threshold — read_file here is
// registered ParallelSafe: true too, so batch actually fires.
//
// This is the fix for a real, verified golden-test blind spot (M1 mutation:
// swapping the batch and todo sections in assembleSystemPromptSections was
// invisible to the pre-existing two cases, because NEITHER case ever
// carried both sections at once — buildGoldenInteractiveWithDelegation
// omits todo_write, and its single ParallelSafe tool (grep) means batch
// itself is silently absent too; buildGoldenNonInteractiveBashOnly has
// neither). One hash here pins the presence AND relative order of all five
// section kinds (BASE, FILEOP, SEARCH, BATCH, TODO, DELEG) simultaneously.
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
			wantLen:  4787,
			wantHash: "2ce4f901021133ebe149173039508e120a8bb1f605f7a624d32f8e68965cb9d1",
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
			wantLen:  6722,
			wantHash: "5dbe9b8f75396ff11f9653ec2b7557a798b23ad7144f52ef73285d5ef6636761",
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
