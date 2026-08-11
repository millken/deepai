package commands

import (
	"context"
	"testing"

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
