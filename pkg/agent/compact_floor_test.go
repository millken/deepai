package agent

import (
	"fmt"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func TestCompaction_LongSessionDropsUnderThreshold(t *testing.T) {
	var msgs []models.Message
	msgs = append(msgs, models.Message{Role: models.RoleSystem, Content: "sys"})
	msgs = append(msgs, models.Message{Role: models.RoleHuman, Content: "do a big task"})
	for i := 0; i < 419; i++ {
		msgs = append(msgs, models.Message{Role: models.RoleAI,
			ToolCalls: []models.ToolCall{{ID: fmt.Sprintf("c%d", i), Name: "read_file", Arguments: map[string]any{"path": "x"}}}})
		body := make([]byte, 3000)
		for j := range body {
			body[j] = 'a'
		}
		msgs = append(msgs, models.Message{Role: models.RoleTool, Content: string(body),
			ToolResult: &models.ToolResult{CallID: fmt.Sprintf("c%d", i), ToolName: "read_file"}})
	}

	const window = 192000
	before := estimateTokens(msgs, "you are a helpful agent", 0)
	if float64(before)/float64(window) < 1.0 {
		t.Fatalf("setup too small to exercise overflow: before=%d", before)
	}

	c, ok := compactMessages(msgs, defaultCompactionKeepTail)
	if !ok {
		t.Fatal("expected compaction to occur on a long session")
	}
	after := estimateTokens(c, "you are a helpful agent", 0)
	ratio := float64(after) / float64(window)
	if ratio >= defaultCompactionThreshold {
		t.Fatalf("compaction left the session above the trigger threshold: after=%d ratio=%.2f (want < %.2f)",
			after, ratio, defaultCompactionThreshold)
	}
}
