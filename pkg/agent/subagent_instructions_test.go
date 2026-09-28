package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/subagent"
)

// TestSubagentExecutor_InstructionsTail guards the DEEPAI.md injection: a
// dispatched subagent used to see only its profile prompt, so operator
// constraints ("使用中文") were invisible to every reviewer — the main agent
// obeyed them, its delegates did not. The tail must APPEND (profile prompt
// survives) and must be absent from an executor that never set it.
func TestSubagentExecutor_InstructionsTail(t *testing.T) {
	exec, provider := profileTestExecutor(t)
	exec.WithInstructions("使用中文，回答要简短专业")
	if _, err := exec.Execute(context.Background(),
		&subagent.Task{ID: "t", Prompt: "hi", Config: subagent.SubagentConfig{AgentType: "correctness-reviewer"}},
		func(subagent.TaskEvent) {}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	sp := provider.firstRequest().SystemPrompt
	if !strings.Contains(sp, "使用中文，回答要简短专业") {
		t.Fatal("system prompt missing the instructions tail — DEEPAI.md constraints never reach the subagent")
	}
	if !strings.Contains(sp, "independent adversarial correctness reviewer") {
		t.Fatal("instructions must append to the profile prompt, not replace it")
	}

	bareExec, bareProvider := profileTestExecutor(t)
	if _, err := bareExec.Execute(context.Background(),
		&subagent.Task{ID: "t", Prompt: "hi", Config: subagent.SubagentConfig{AgentType: "correctness-reviewer"}},
		func(subagent.TaskEvent) {}); err != nil {
		t.Fatalf("Execute() (no instructions) error = %v", err)
	}
	if strings.Contains(bareProvider.firstRequest().SystemPrompt, "使用中文") {
		t.Fatal("instructions leaked into an executor that never set them")
	}
}
