package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

func probeRegistry(seen **tools.Dispatch) *tools.Registry {
	reg := tools.NewRegistry()
	_ = reg.Register(models.Tool{
		Name:         "probe",
		ParallelSafe: true,
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			*seen = tools.DispatchFromContext(ctx)
			return models.ToolResult{CallID: c.ID, ToolName: c.Name, Status: models.CallStatusCompleted, Content: "ok"}, nil
		},
	})
	return reg
}

// Criterion 8: runOneTool must inject the agent's CURRENT tool registry so a
// nested executor (run_code) mirrors the live capability set at call time.
func TestRunOneToolInjectsDispatch(t *testing.T) {
	var seen *tools.Dispatch
	reg := probeRegistry(&seen)
	a := &Agent{tools: reg, maxToolConcurrency: 7}

	res := a.runOneTool(context.Background(), "s1", models.ToolCall{ID: "c1", Name: "probe"})
	if res.Status != models.CallStatusCompleted {
		t.Fatalf("probe status = %s, want completed", res.Status)
	}
	if seen == nil {
		t.Fatal("DispatchFromContext inside nested handler: nil, want non-nil")
	}
	if seen.Registry != reg {
		t.Error("Dispatch.Registry is not the agent's a.tools pointer")
	}
	if seen.MaxConcurrency != 7 {
		t.Errorf("Dispatch.MaxConcurrency = %d, want 7 (a.maxToolConcurrency)", seen.MaxConcurrency)
	}
}

// Criterion 8, plan-mode half: after a.tools is swapped for a restricted set
// (what enterPlanMode does via RestrictTo, plan.go), the dispatch seen by a
// nested handler is the RESTRICTED registry — the injection reads a.tools at
// call time, not a closure captured at registration.
func TestRunOneToolDispatchMirrorsRestrictedRegistry(t *testing.T) {
	var seen *tools.Dispatch
	full := probeRegistry(&seen)
	_ = full.Register(models.Tool{
		Name: "edit_file",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{CallID: c.ID, ToolName: c.Name}, nil
		},
	})
	restricted := full.RestrictTo([]string{"probe"})
	a := &Agent{tools: restricted, maxToolConcurrency: 4}

	res := a.runOneTool(context.Background(), "s1", models.ToolCall{ID: "c1", Name: "probe"})
	if res.Status != models.CallStatusCompleted {
		t.Fatalf("probe status = %s, want completed", res.Status)
	}
	if seen == nil || seen.Registry != restricted {
		t.Errorf("Dispatch.Registry under restricted a.tools: %v, want the restricted registry", seen)
	}
}

// Without injection the legacy ctx carries no dispatch — the mirror is absent,
// run_code must refuse rather than guess.
func TestDispatchFromContextAbsentByDefault(t *testing.T) {
	if d := tools.DispatchFromContext(context.Background()); d != nil {
		t.Errorf("DispatchFromContext on plain ctx = %v, want nil", d)
	}
}

// Criterion 26, agent side: the offload preview keeps the tail of the result —
// run_code's single trailing writes: line must survive it.
func TestOffloadPreviewKeepsWritesLine(t *testing.T) {
	var b strings.Builder
	b.WriteString(strings.Repeat("x", 64*1024) + "\n")
	for i := 0; i < 40; i++ {
		b.WriteString("filler line to push the head out of the preview window\n")
	}
	b.WriteString("run_code ok — 3 tool calls in 0.1s\n")
	b.WriteString("writes: edit_file pkg/tools/builtin/grep.go [completed]")
	tail := offloadPreview(b.String())
	if !strings.Contains(tail, "writes: edit_file pkg/tools/builtin/grep.go [completed]") {
		t.Errorf("offloadPreview dropped the writes: line:\n%q", tail)
	}
	if !strings.HasSuffix(tail, "[completed]") {
		t.Errorf("writes: line not the final content, got tail %q", tail[len(tail)-80:])
	}
}
