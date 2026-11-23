package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

// Serial batch cancelled mid-way (issue #50): the serial tool loop's
// ctx.Err() branch returned batch.runMessages without synthesizing
// placeholders for the tool calls that never executed, so a persisted
// session carried a tool_use with no matching tool_result and every later
// request failed with a 400.
func TestSerialBatch_CtxCancelSynthesizesRemainingResults(t *testing.T) {
	reg := tools.NewRegistry()
	release := make(chan struct{})
	if err := reg.Register(models.Tool{
		Name: "blocker",
		Handler: func(hctx context.Context, _ models.ToolCall) (models.ToolResult, error) {
			select {
			case <-release:
				return models.ToolResult{Content: "done"}, nil
			case <-hctx.Done():
				return models.ToolResult{}, hctx.Err()
			}
		},
	}); err != nil {
		t.Fatal(err)
	}

	calls := []models.ToolCall{
		{ID: "call-0", Name: "blocker", Arguments: map[string]any{}},
		{ID: "call-1", Name: "blocker", Arguments: map[string]any{}},
		{ID: "call-2", Name: "blocker", Arguments: map[string]any{}},
	}
	a := New(AgentConfig{LLMProvider: &batchProvider{calls: calls}, Tools: reg})

	ctx, cancelCtx := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancelCtx()
		close(release)
	}()
	res, err := a.Run(ctx, "s1", []models.Message{
		{ID: "m1", SessionID: "s1", Role: models.RoleHuman, Content: "go"},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}
	_ = cancelCtx

	got := map[string]bool{}
	for _, m := range res.Messages {
		if m.Role == models.RoleTool && m.ToolResult != nil {
			got[m.ToolResult.CallID] = true
		}
	}
	for _, c := range calls {
		if !got[c.ID] {
			t.Fatalf("RED: tool_use %s has no matching tool_result (got %v) — unexecuted serial calls left unpaired", c.ID, got)
		}
	}
}
