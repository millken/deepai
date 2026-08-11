package commands

import (
	"context"
	"testing"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/tools"
)

// stubProvider satisfies llm.LLMProvider without making any network call;
// registerChatTools only needs a provider to construct GitAutoCommitTool and
// the subagent executor, it never calls either during registration.
type stubProvider struct{}

func (stubProvider) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (stubProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	return nil, nil
}

// TestRegisterChatTools_RegistersDocxTools guards P1c Task 3's wiring point 1
// (docs/DOCX_TOOLS_DESIGN.md §4 "汇总注册"): DocxTools() is only visible to the
// agent once registerChatTools registers it alongside FileTools()/WebTools().
// Also pins docx_edit.ParallelSafe == false, since it writes to disk and must
// never be scheduled in a parallel tool-call batch alongside another edit.
func TestRegisterChatTools_RegistersDocxTools(t *testing.T) {
	registry := tools.NewRegistry()
	modelRegistry := llm.NewSingleModelRegistry("test", "test-model", "")
	registerChatTools(registry, modelRegistry, stubProvider{}, false, t.TempDir(), 0, nil, nil)

	read := registry.Get("docx_read")
	if read == nil {
		t.Fatal("registry.Get(docx_read) = nil, want the tool registered by DocxTools()")
	}
	if read.Handler == nil {
		t.Error("docx_read registered with a nil Handler")
	}

	edit := registry.Get("docx_edit")
	if edit == nil {
		t.Fatal("registry.Get(docx_edit) = nil, want the tool registered by DocxTools()")
	}
	if edit.Handler == nil {
		t.Error("docx_edit registered with a nil Handler")
	}
	if edit.ParallelSafe {
		t.Error("docx_edit.ParallelSafe = true, want false: it writes to disk and must not run in a parallel batch")
	}
}

// TestSubagentMaxTokens_UsesSharedConstant pins that the subagent wiring
// reads its MaxTokens from agent.ResolveMaxOutputTokens rather than a
// literal of its own. This is the anti-drift guard the P2c5 brief asks for:
// pkg/chat/repl.go's mainAgentMaxTokens must produce the identical value for
// the agent the user actually talks to, and both are pinned against the one
// resolver so they cannot silently diverge.
func TestSubagentMaxTokens_UsesSharedConstant(t *testing.T) {
	t.Setenv(agent.EnvMaxOutputTokens, "")
	got := subagentMaxTokens()
	if got == nil {
		t.Fatal("subagentMaxTokens() = nil, want a pointer to agent.DefaultMaxOutputTokens")
	}
	if *got != agent.DefaultMaxOutputTokens {
		t.Errorf("subagentMaxTokens() = %d, want agent.DefaultMaxOutputTokens (%d)", *got, agent.DefaultMaxOutputTokens)
	}
}

// TestSubagentMaxTokens_ExplicitSettingWins pins that a valid
// DEEPAI_MAX_OUTPUT_TOKENS setting reaches the subagent's MaxTokens, not
// just the default.
func TestSubagentMaxTokens_ExplicitSettingWins(t *testing.T) {
	t.Setenv(agent.EnvMaxOutputTokens, "40000")
	got := subagentMaxTokens()
	if got == nil {
		t.Fatal("subagentMaxTokens() = nil, want a pointer to 40000")
	}
	if *got != 40000 {
		t.Errorf("subagentMaxTokens() = %d, want 40000 (explicit setting)", *got)
	}
}

// TestSubagentMaxTokens_MatchesResolver pins subagentMaxTokens to
// agent.ResolveMaxOutputTokens under both a valid override and an invalid
// one, mirroring TestMainAgentMaxTokens_MatchesResolver in pkg/chat. Together
// the two prove the main agent and every subagent resolve to the identical
// value from one setting; either wiring point drifting to a separate
// literal breaks its own half of this pair.
func TestSubagentMaxTokens_MatchesResolver(t *testing.T) {
	for _, raw := range []string{"", "50000", "not-a-number", "-5"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(agent.EnvMaxOutputTokens, raw)
			want := agent.ResolveMaxOutputTokens()
			got := subagentMaxTokens()
			if got == nil || *got != want {
				t.Errorf("subagentMaxTokens() = %v, want pointer to agent.ResolveMaxOutputTokens() = %d", got, want)
			}
		})
	}
}
