package agent

import (
	"context"
	"testing"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// --- M6 latency, fingerprint fix: AssembleSystemPrompt ----------------------
//
// BuildSystemPrompt's section-assembly is pulled out into the exported,
// *Agent-free AssembleSystemPrompt so a caller with no live Agent to
// construct (the eval harness's resolveEvalSubagentPrompt/caseFingerprint,
// pkg/commands/agent_eval.go) can compute the IDENTICAL bytes a real
// dispatched subagent's BuildSystemPrompt() produces — see that function's
// doc comment for the full rationale. This test pins the wiring: for a
// NonInteractive agent (the only shape a subagent ever is — plan mode is
// unconditionally inert there, see AssembleSystemPrompt's doc comment),
// BuildSystemPrompt() must equal exactly AssembleSystemPrompt(...) — i.e.
// BuildSystemPrompt must genuinely CALL the same section-assembly, not
// merely produce output that happens to match it.
func TestAssembleSystemPrompt_MatchesBuildSystemPromptForNonInteractiveAgent(t *testing.T) {
	reg := tools.NewRegistry()
	for _, tool := range builtinFileToolsForTest() {
		_ = reg.Register(tool)
	}
	_ = reg.Register(models.Tool{Name: "grep", ParallelSafe: true, Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
		return models.ToolResult{}, nil
	}})
	_ = reg.Register(models.Tool{Name: "todo_write", Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
		return models.ToolResult{}, nil
	}})

	base := "You are a test role."
	a := New(AgentConfig{
		LLMProvider:    &captureProvider{},
		Tools:          reg,
		Model:          "m",
		SystemPrompt:   base,
		NonInteractive: true,
	})

	got := a.BuildSystemPrompt()
	want := AssembleSystemPrompt(base, reg, true, nil)
	if got != want {
		t.Errorf("BuildSystemPrompt() != AssembleSystemPrompt(...):\nBuildSystemPrompt:\n%s\n\nAssembleSystemPrompt:\n%s", got, want)
	}
}

// TestAssembleSystemPrompt_IsPureFunctionOfItsInputs: calling it twice with
// the same inputs (no *Agent, no shared mutable state) produces
// byte-identical output — required for the eval harness to trust it as a
// deterministic fingerprint input.
func TestAssembleSystemPrompt_IsPureFunctionOfItsInputs(t *testing.T) {
	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{Name: "read_file", ParallelSafe: true, Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
		return models.ToolResult{}, nil
	}})
	_ = reg.Register(models.Tool{Name: "grep", ParallelSafe: true, Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
		return models.ToolResult{}, nil
	}})

	a := AssembleSystemPrompt("role prompt", reg, true, nil)
	b := AssembleSystemPrompt("role prompt", reg, true, nil)
	if a != b {
		t.Errorf("AssembleSystemPrompt is not deterministic across identical calls:\n%s\n\nvs\n\n%s", a, b)
	}
}
