package runcode

import (
	"context"
	"testing"
	"time"
)

// Review H2, engine level: slow + edit_file drain into ONE batch (two serial
// segments). The watchdog fires mid-slow; the driver then reaches the edit
// segment past the deadline — the gate must reject it WITHOUT calling the
// host, so the write never lands on disk (invisible to edited_paths).
func TestEngineTimeoutGatesUnstartedWrites(t *testing.T) {
	warm := newEchoHost()
	warm.declare("a", true, true)
	if w := runScript(t, warm, Limits{}, `return 1;`); w.Outcome != OutcomeOK {
		t.Fatalf("warmup: %s %s", w.Outcome, w.Err)
	}

	h := newEchoHost()
	h.declare("slow", true, false)
	h.declare("edit_file", true, false)
	h.fn = func(name string, args map[string]any) HostResult {
		if name == "slow" {
			time.Sleep(400 * time.Millisecond)
		}
		return HostResult{Status: "completed"}
	}

	out := NewEngine().Run(context.Background(), `
		await Promise.all([tools.slow({}), tools.edit_file({path: "/tmp/x/late.go"})]);
		return "never";
	`, h, Limits{Timeout: 150 * time.Millisecond, MaxToolCalls: 10})
	if out.Outcome != OutcomeTimeout {
		t.Fatalf("outcome=%s, want timeout", out.Outcome)
	}
	// Let the abandoned driver finish the slow segment and reach the edit
	// segment — the gate's decision happens there.
	time.Sleep(700 * time.Millisecond)

	h.mu.Lock()
	calls := append([]string(nil), h.calls...)
	h.mu.Unlock()
	sawSlow, sawEdit := false, false
	for _, c := range calls {
		if c == "slow" {
			sawSlow = true
		}
		if c == "edit_file" {
			sawEdit = true
		}
	}
	if !sawSlow {
		t.Fatal("slow never started — the batch shape changed, test is no longer exercising the gate")
	}
	if sawEdit {
		t.Fatal("edit_file STARTED after the budget expired — an unstarted write landed past the watchdog")
	}
	// And the returned snapshot never contained it either.
	for _, c := range out.Calls {
		if c.Tool == "edit_file" {
			t.Fatalf("gated edit leaked into journal: %+v", c)
		}
	}
}
