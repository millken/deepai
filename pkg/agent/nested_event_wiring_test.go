package agent

import (
	"context"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/runcode"
	"github.com/millken/deepai/pkg/tools"
)

// The nested-event adapter maps tools.NestedToolEvent → AgentEvent with
// ParentID set, running → start, anything else → end (§7.2 Phase 2).
func TestNestedEventAdapterMapping(t *testing.T) {
	a := &Agent{events: make(chan AgentEvent, 16)}
	sink := a.nestedEventSink("sess1", "rc_9")

	sink(nestedEventForTest("grep", "running", 1, 0, ""))
	sink(nestedEventForTest("grep", "completed", 1, 12, ""))
	sink(nestedEventForTest("bash", "failed", 2, 40, "boom"))

	close(a.events)
	var got []AgentEvent
	for ev := range a.events {
		got = append(got, ev)
	}
	if len(got) != 3 {
		t.Fatalf("events = %d, want 3", len(got))
	}
	if got[0].Type != AgentEventToolCallStart || got[1].Type != AgentEventToolCallEnd || got[2].Type != AgentEventToolCallEnd {
		t.Fatalf("types = %v %v %v", got[0].Type, got[1].Type, got[2].Type)
	}
	for i, ev := range got {
		te := ev.ToolEvent
		if te == nil || te.ParentID != "rc_9" {
			t.Fatalf("event[%d] missing ParentID: %+v", i, ev)
		}
		if ev.SessionID != "sess1" {
			t.Fatalf("event[%d] SessionID = %q", i, ev.SessionID)
		}
	}
	if got[0].ToolEvent.ID != "rc_9.1" || got[2].ToolEvent.ID != "rc_9.2" {
		t.Fatalf("nested IDs = %q %q, want rc_9.1 / rc_9.2", got[0].ToolEvent.ID, got[2].ToolEvent.ID)
	}
	if got[2].ToolEvent.Error != "boom" {
		t.Fatalf("failed event lost error: %+v", got[2].ToolEvent)
	}
}

func nestedEventForTest(tool, status string, seq int, ms int64, errText string) (out tools.NestedToolEvent) {
	out.Tool = tool
	out.Status = status
	out.Seq = seq
	out.MS = ms
	out.Err = errText
	return
}

// End-to-end wiring: runOneTool on a run_code call attaches the sink, and the
// engine's journal events surface on the agent's events channel with the
// outer call ID as ParentID.
func TestRunOneToolEmitsNestedEvents(t *testing.T) {
	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{
		Name: "grep", ParallelSafe: true,
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{CallID: c.ID, ToolName: c.Name, Status: models.CallStatusCompleted, Content: "1:abc"}, nil
		},
	})
	_ = reg.Register(runcode.RunCodeTool())
	a := &Agent{tools: reg, events: make(chan AgentEvent, 32)}

	res := a.runOneTool(context.Background(), "sess1", models.ToolCall{
		ID: "rc_1", Name: "run_code", Status: models.CallStatusPending,
		Arguments: map[string]any{"script": "await tools.grep({pattern: \"x\"}); return 1;"},
	})
	if res.Status != models.CallStatusCompleted {
		t.Fatalf("run_code status=%s err=%q", res.Status, res.Error)
	}

	var sawRunning, sawCompleted bool
	deadline := time.After(2 * time.Second)
	for (!sawRunning || !sawCompleted) && deadline != nil {
		select {
		case ev, ok := <-a.events:
			if !ok {
				deadline = nil
				break
			}
			if ev.ToolEvent == nil || ev.ToolEvent.ParentID != "rc_1" {
				continue
			}
			if ev.ToolEvent.Name != "grep" {
				t.Fatalf("unexpected nested tool %q", ev.ToolEvent.Name)
			}
			switch ev.ToolEvent.Status {
			case models.CallStatusRunning:
				sawRunning = true
			case models.CallStatusCompleted:
				sawCompleted = true
			}
		case <-deadline:
			deadline = nil
		}
	}
	if !sawRunning || !sawCompleted {
		t.Fatalf("nested lifecycle incomplete: running=%v completed=%v", sawRunning, sawCompleted)
	}
}
