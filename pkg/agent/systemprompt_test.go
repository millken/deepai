package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// TestT5c_GeneralPromptNoFileOpDup guards T5c: the general-purpose base prompt
// must not re-state the file-operation routing that BuildSystemPrompt appends.
func TestT5c_GeneralPromptNoFileOpDup(t *testing.T) {
	if strings.Contains(generalPurposeSystemPrompt, "Tool preference") ||
		strings.Contains(generalPurposeSystemPrompt, "read_file (not") {
		t.Errorf("generalPurposeSystemPrompt still duplicates the file-op rule: %q", generalPurposeSystemPrompt)
	}
}

// TestT5c_FileOpRuleAppendedWhenToolsPresent: an agent with file tools still
// gets the authoritative file-operation rule exactly once.
func TestT5c_FileOpRuleAppendedWhenToolsPresent(t *testing.T) {
	reg := tools.NewRegistry()
	for _, tool := range builtinFileToolsForTest() {
		_ = reg.Register(tool)
	}
	a := New(AgentConfig{LLMProvider: &captureProvider{}, Tools: reg, Model: "m"})

	sp := a.BuildSystemPrompt(context.Background(), "s", nil)
	if got := strings.Count(sp, "File-operation rule:"); got != 1 {
		t.Errorf("file-op rule count = %d, want exactly 1:\n%s", got, sp)
	}
}

// TestT5c_FileOpRuleOmittedWithoutFileTools: a tool-less agent must not carry the
// ~400-char rule referencing tools it doesn't have.
func TestT5c_FileOpRuleOmittedWithoutFileTools(t *testing.T) {
	a := New(AgentConfig{LLMProvider: &captureProvider{}, Tools: tools.NewRegistry(), Model: "m"})

	sp := a.BuildSystemPrompt(context.Background(), "s", nil)
	if strings.Contains(sp, "File-operation rule:") {
		t.Errorf("agent without read_file should not carry the file-op rule:\n%s", sp)
	}
}

// TestT5c_FileOpRulePresentWithAnyFileTool: an agent with edit_file but NOT
// read_file must still get the rule — it's what steers the model away from
// bash sed -i for edits.
func TestT5c_FileOpRulePresentWithAnyFileTool(t *testing.T) {
	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{
		Name: "edit_file",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{}, nil
		},
	})
	a := New(AgentConfig{LLMProvider: &captureProvider{}, Tools: reg, Model: "m"})

	sp := a.BuildSystemPrompt(context.Background(), "s", nil)
	if !strings.Contains(sp, "File-operation rule:") {
		t.Errorf("agent with edit_file (sans read_file) must still carry the file-op rule:\n%s", sp)
	}
}

// TestPlanMode_IncludesCodeMap guards that the exploration-first tool survives
// the plan-mode restriction (it's advertised as "use FIRST when exploring").
func TestPlanMode_IncludesCodeMap(t *testing.T) {
	found := false
	for _, name := range planToolNames {
		if name == "code_map" {
			found = true
		}
	}
	if !found {
		t.Error("planToolNames must include code_map")
	}
}

// TestTeamDelegation_InjectedWithTaskToolAndCatalog: a top-level agent with the
// task tool and a non-empty catalog gets the delegation prompt with the catalog
// rendered dynamically.
func TestTeamDelegation_InjectedWithTaskToolAndCatalog(t *testing.T) {
	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{
		Name: "task",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{}, nil
		},
	})
	a := New(AgentConfig{
		LLMProvider:  &captureProvider{},
		Tools:        reg,
		Model:        "m",
		AgentCatalog: []AgentInfo{{Type: "coder", Description: "writes code"}},
	})

	sp := a.BuildSystemPrompt(context.Background(), "s", nil)
	if !strings.Contains(sp, "Team Delegation") {
		t.Errorf("delegation prompt missing:\n%s", sp)
	}
	if !strings.Contains(sp, "coder") {
		t.Errorf("catalog entry 'coder' not rendered:\n%s", sp)
	}
}

// TestTeamDelegation_OmittedForSubagent: NonInteractive agents (sub-agents) must
// not get the delegation prompt — they can't spawn their own sub-agents.
func TestTeamDelegation_OmittedForSubagent(t *testing.T) {
	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{
		Name: "task",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{}, nil
		},
	})
	a := New(AgentConfig{
		LLMProvider:    &captureProvider{},
		Tools:          reg,
		Model:          "m",
		NonInteractive: true,
		AgentCatalog:   []AgentInfo{{Type: "coder", Description: "writes code"}},
	})

	sp := a.BuildSystemPrompt(context.Background(), "s", nil)
	if strings.Contains(sp, "Team Delegation") {
		t.Errorf("sub-agent should not get delegation prompt:\n%s", sp)
	}
}

// TestTeamDelegation_OmittedWithoutTaskTool: no task tool → no delegation prompt,
// even for interactive agents with a catalog.
func TestTeamDelegation_OmittedWithoutTaskTool(t *testing.T) {
	a := New(AgentConfig{
		LLMProvider:  &captureProvider{},
		Tools:        tools.NewRegistry(),
		Model:        "m",
		AgentCatalog: []AgentInfo{{Type: "coder", Description: "writes code"}},
	})

	sp := a.BuildSystemPrompt(context.Background(), "s", nil)
	if strings.Contains(sp, "Team Delegation") {
		t.Errorf("agent without task tool should not get delegation prompt:\n%s", sp)
	}
}

// TestTeamDelegation_OmittedWithEmptyCatalog: task tool present but no agents to
// delegate to → skip the prompt (saves tokens, avoids advertising nothing).
func TestTeamDelegation_OmittedWithEmptyCatalog(t *testing.T) {
	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{
		Name: "task",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{}, nil
		},
	})
	a := New(AgentConfig{
		LLMProvider: &captureProvider{},
		Tools:       reg,
		Model:       "m",
	})

	sp := a.BuildSystemPrompt(context.Background(), "s", nil)
	if strings.Contains(sp, "Team Delegation") {
		t.Errorf("agent with empty catalog should not get delegation prompt:\n%s", sp)
	}
}

// TestTeamDelegation_OmittedInPlanMode: plan mode replaces a.tools with
// planToolNames (no task), so the delegation prompt must not appear. This
// prevents using sub-agents to bypass plan-mode file restrictions.
func TestTeamDelegation_OmittedInPlanMode(t *testing.T) {
	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{
		Name: "task",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{}, nil
		},
	})
	for _, n := range planToolNames {
		_ = reg.Register(models.Tool{
			Name: n,
			Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
				return models.ToolResult{}, nil
			},
		})
	}
	a := New(AgentConfig{
		LLMProvider:  &captureProvider{},
		Tools:        reg,
		Model:        "m",
		PlanMode:     true,
		AgentCatalog: []AgentInfo{{Type: "coder", Description: "writes code"}},
	})

	sp := a.BuildSystemPrompt(context.Background(), "s", nil)
	if strings.Contains(sp, "Team Delegation") {
		t.Errorf("plan mode must not show delegation prompt:\n%s", sp)
	}
}

// TestSelectSubagentTools_FiltersTaskInFallback: the no-selectors and
// no-match fallback paths must both exclude the task tool to prevent
// unbounded sub-agent recursion.
func TestSelectSubagentTools_FiltersTaskInFallback(t *testing.T) {
	tools := []models.Tool{
		{Name: "bash"},
		{Name: "task"},
		{Name: "read_file"},
	}

	// No selectors → fallback path.
	got := selectSubagentTools(tools, nil)
	for _, tt := range got {
		if tt.Name == "task" {
			t.Error("task tool should be filtered in no-selectors fallback")
		}
	}

	// Non-matching selectors → fallback path.
	got = selectSubagentTools(tools, []string{"nonexistent_tool"})
	for _, tt := range got {
		if tt.Name == "task" {
			t.Error("task tool should be filtered in no-match fallback")
		}
	}
}

// builtinFileToolsForTest registers a minimal read_file so BuildSystemPrompt's
// tool gate fires, without importing the builtin package (avoids an import cycle
// risk in agent tests).
func builtinFileToolsForTest() []models.Tool {
	return []models.Tool{{
		Name: "read_file",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{}, nil
		},
	}}
}
