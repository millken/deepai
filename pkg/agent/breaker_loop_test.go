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
