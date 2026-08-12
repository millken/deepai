package agent

import (
	"fmt"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

// TestRepeatBreaker_CatchesCyclingReadLoop covers the third defense that failed
// in session 20260812_093415_fc6e: the breaker only counted CONSECUTIVE
// identical calls, so cycling read_file over three files reset the counter
// before it ever reached the threshold — 1136 calls, zero hints.
func TestRepeatBreaker_CatchesCyclingReadLoop(t *testing.T) {
	b := newToolCallBreaker()
	files := []string{"tcp.dart", "udp.dart", "reconnect.dart"}

	hints, fatalAt := 0, 0
	for i := 0; i < 100; i++ {
		path := files[i%len(files)] // never the same path twice in a row
		call := models.ToolCall{
			ID:        fmt.Sprintf("c%d", i),
			Name:      "read_file",
			Arguments: map[string]any{"path": path},
		}
		res := models.ToolResult{
			CallID:   call.ID,
			ToolName: "read_file",
			Status:   models.CallStatusCompleted,
			Content:  "// " + path,
		}
		obs := b.observe("s1", call, res)
		hints += len(obs.hintMessages)
		if obs.fatalErr != nil {
			fatalAt = i + 1
			break
		}
	}

	if hints == 0 {
		t.Error("a cycling identical-call loop produced no hint at all")
	}
	if fatalAt == 0 {
		t.Fatal("a cycling identical-call loop never hard-stopped the run")
	}
	// 3-cycle × 24 per key ≈ 72 calls; anything near 1136 is a failed guard.
	if fatalAt > 80 {
		t.Errorf("loop ran %d calls before the breaker stopped it; too slow", fatalAt)
	}
}

// TestRepeatBreaker_NormalReReadIsNotALoop is the false-positive guard for the
// test above: re-reading a file after editing it is ordinary work.
func TestRepeatBreaker_NormalReReadIsNotALoop(t *testing.T) {
	b := newToolCallBreaker()
	for i := 0; i < 3; i++ {
		read := models.ToolCall{ID: fmt.Sprintf("r%d", i), Name: "read_file", Arguments: map[string]any{"path": "a.go"}}
		obs := b.observe("s1", read, models.ToolResult{
			CallID: read.ID, ToolName: "read_file", Status: models.CallStatusCompleted, Content: "package a",
		})
		if len(obs.hintMessages) > 0 || obs.fatalErr != nil {
			t.Fatalf("read #%d of the same file after an edit tripped the breaker", i+1)
		}
		edit := models.ToolCall{ID: fmt.Sprintf("e%d", i), Name: "edit_file", Arguments: map[string]any{"path": "a.go", "old": fmt.Sprint(i)}}
		if obs := b.observe("s1", edit, models.ToolResult{
			CallID: edit.ID, ToolName: "edit_file", Status: models.CallStatusCompleted, Content: "ok",
		}); obs.fatalErr != nil {
			t.Fatalf("edit #%d tripped the breaker", i+1)
		}
	}
}

// TestSameFailureBreaker_CatchesRewordedRetries replays the second loop from
// session 20260812_093415_fc6e: `dart test` timed out with no output, and the
// model retried eight times, changing the command text every couple of tries
// (`| tail -20` → `| tail -30` → no pipe → a single test file). Every change
// produced a new (tool, arguments) key, so both argument-keyed counters stayed
// at 1-2 and nothing ever fired.
func TestSameFailureBreaker_CatchesRewordedRetries(t *testing.T) {
	b := newToolCallBreaker()
	commands := []string{
		"cd flutter && dart test test/reconnect/ 2>&1 | tail -20",
		"cd flutter && dart test test/reconnect/ 2>&1 | tail -20",
		"cd flutter && dart test test/reconnect/ 2>&1 | tail -30",
		"cd flutter && dart test test/reconnect/ 2>&1 | tail -30",
		"cd flutter && dart test test/reconnect/ 2>&1",
		"cd flutter && dart test test/reconnect/reconnect_helper_test.dart 2>&1",
		"cd flutter && dart test test/reconnect/reconnect_helper_test.dart 2>&1 | tail -30",
		"cd flutter && dart test test/reconnect/reconnect_helper_test.dart 2>&1 | tail -40",
		"cd flutter && dart test test/ 2>&1",
		"cd flutter && dart test 2>&1",
	}
	// The identical outcome every time: killed at the limit, nothing captured.
	const timeoutErr = "command TIMED OUT after 120s (limit 120s) and its process group was killed. " +
		"It produced NO output at all before the kill"

	hints, fatalAt := 0, 0
	for i, cmd := range commands {
		call := models.ToolCall{ID: fmt.Sprintf("c%d", i), Name: "bash", Arguments: map[string]any{"command": cmd, "timeout": 120}}
		obs := b.observe("s1", call, models.ToolResult{
			CallID:   call.ID,
			ToolName: "bash",
			Status:   models.CallStatusFailed,
			Error:    timeoutErr,
			Content:  `{"stdout":"","stderr":"","exit_code":-1,"timed_out":true}`,
		})
		hints += len(obs.hintMessages)
		if obs.fatalErr != nil {
			fatalAt = i + 1
			break
		}
	}

	if hints == 0 {
		t.Error("eight reworded retries of an identically failing command produced no hint")
	}
	if fatalAt == 0 {
		t.Fatalf("reworded retries never hard-stopped the run (%d hints only)", hints)
	}
	if fatalAt != maxSameFailureHardStop {
		t.Errorf("hard stop at attempt %d, want %d", fatalAt, maxSameFailureHardStop)
	}
}

// TestSameFailureBreaker_DifferentFailuresAreNotALoop is the false-positive
// guard: a model working through genuinely different errors is making progress,
// however many of them there are.
func TestSameFailureBreaker_DifferentFailuresAreNotALoop(t *testing.T) {
	b := newToolCallBreaker()
	for i := 0; i < 12; i++ {
		call := models.ToolCall{ID: fmt.Sprintf("c%d", i), Name: "bash", Arguments: map[string]any{"command": fmt.Sprintf("go build ./pkg/%d", i)}}
		obs := b.observe("s1", call, models.ToolResult{
			CallID:   call.ID,
			ToolName: "bash",
			Status:   models.CallStatusFailed,
			Error:    fmt.Sprintf("pkg/%d/main.go:%d: undefined: helper%d", i, i+10, i),
		})
		if obs.fatalErr != nil {
			t.Fatalf("distinct failures tripped the breaker at attempt %d", i+1)
		}
		if len(obs.hintMessages) > 0 {
			t.Fatalf("distinct failures produced a loop hint at attempt %d", i+1)
		}
	}
}
