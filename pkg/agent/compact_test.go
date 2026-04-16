package agent

import (
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

func TestCompactMessages_NoCompactionWhenSmall(t *testing.T) {
	msgs := []models.Message{
		{ID: "1", Role: models.RoleSystem, Content: "system"},
		{ID: "2", Role: models.RoleHuman, Content: "hello"},
	}
	out, did := compactMessages(msgs, 6)
	if did {
		t.Error("should not compact small message list")
	}
	if len(out) != len(msgs) {
		t.Error("messages should be unchanged")
	}
}

func TestCompactMessages_ToolResultPreservesStruct(t *testing.T) {
	msgs := []models.Message{
		{ID: "1", SessionID: "s1", Role: models.RoleSystem, Content: "system"},
		{ID: "2", SessionID: "s1", Role: models.RoleHuman, Content: "do something"},
		{ID: "3", SessionID: "s1", Role: models.RoleAI, ToolCalls: []models.ToolCall{{ID: "c1", Name: "read_file"}}, Content: ""},
		{ID: "4", SessionID: "s1", Role: models.RoleTool, Content: strings.Repeat("x", 5000), ToolResult: &models.ToolResult{CallID: "c1", ToolName: "read_file", Status: models.CallStatusCompleted}},
		{ID: "5", SessionID: "s1", Role: models.RoleAI, ToolCalls: []models.ToolCall{{ID: "c2", Name: "edit_file"}}, Content: ""},
		{ID: "6", SessionID: "s1", Role: models.RoleTool, Content: "edit ok", ToolResult: &models.ToolResult{CallID: "c2", ToolName: "edit_file", Status: models.CallStatusCompleted}},
		{ID: "7", SessionID: "s1", Role: models.RoleAI, Content: "done"},
	}

	out, did := compactMessages(msgs, 2)
	if !did {
		t.Fatal("expected compaction")
	}

	// Every compacted message must preserve SessionID.
	for _, m := range out {
		if m.SessionID != "s1" {
			t.Errorf("message %q lost SessionID: got %q", m.ID, m.SessionID)
		}
	}

	// Compacted tool results must keep ToolResult with CallID.
	for _, m := range out {
		if m.Role == models.RoleTool && m.ToolResult == nil {
			t.Errorf("tool message %q lost ToolResult", m.ID)
		}
		if m.Role == models.RoleTool && m.ToolResult != nil {
			if m.ToolResult.CallID == "" {
				t.Error("tool message lost CallID")
			}
			if m.ToolResult.ToolName == "" {
				t.Error("tool message lost ToolName")
			}
		}
	}

	// Tool result content should be summarized (not full 5000 chars).
	for _, m := range out {
		if m.Role == models.RoleTool && len(m.Content) > 400 {
			t.Errorf("tool result content too long: %d", len(m.Content))
		}
	}
}

func TestCompactMessages_AssistantToolCallsPreserved(t *testing.T) {
	msgs := []models.Message{
		{ID: "1", SessionID: "s1", Role: models.RoleSystem, Content: "system"},
		{ID: "2", SessionID: "s1", Role: models.RoleHuman, Content: "go"},
		{ID: "3", SessionID: "s1", Role: models.RoleAI, ToolCalls: []models.ToolCall{
			{ID: "c1", Name: "read_file", Arguments: map[string]any{"path": "/very/long/path/to/some/file.go"}},
			{ID: "c2", Name: "grep", Arguments: map[string]any{"pattern": "very long regex pattern here"}},
		}, Content: ""},
		{ID: "4", SessionID: "s1", Role: models.RoleTool, Content: "result", ToolResult: &models.ToolResult{CallID: "c1", ToolName: "read_file"}},
		{ID: "5", SessionID: "s1", Role: models.RoleAI, ToolCalls: []models.ToolCall{
			{ID: "c3", Name: "edit_file"},
		}, Content: ""},
		{ID: "6", SessionID: "s1", Role: models.RoleTool, Content: "ok", ToolResult: &models.ToolResult{CallID: "c3", ToolName: "edit_file"}},
		{ID: "7", SessionID: "s1", Role: models.RoleAI, Content: "final answer"},
	}

	out, did := compactMessages(msgs, 2)
	if !did {
		t.Fatal("expected compaction")
	}

	// Assistant messages with tool_calls must STILL have tool_calls (ID+Name).
	for _, m := range out {
		if m.Role == models.RoleAI && m.Content != "" && strings.HasPrefix(m.Content, "[Called") {
			if len(m.ToolCalls) == 0 {
				t.Errorf("assistant message %q has summary text but lost ToolCalls", m.ID)
			}
			for _, tc := range m.ToolCalls {
				if tc.ID == "" || tc.Name == "" {
					t.Errorf("tool call lost ID or Name: %+v", tc)
				}
			}
		}
	}

	// Arguments should be stripped from compacted tool calls.
	for _, m := range out {
		if m.Role == models.RoleAI && strings.HasPrefix(m.Content, "[Called") {
			for _, tc := range m.ToolCalls {
				if len(tc.Arguments) > 0 {
					t.Errorf("compacted tool call %s still has arguments", tc.ID)
				}
			}
		}
	}

	// SessionID preserved.
	for _, m := range out {
		if m.SessionID != "s1" {
			t.Errorf("message %q lost SessionID", m.ID)
		}
	}
}

func TestCompactMessages_AssistantTextTruncated(t *testing.T) {
	longText := strings.Repeat("hello ", 100) // 600 chars
	msgs := []models.Message{
		{ID: "1", SessionID: "s1", Role: models.RoleSystem, Content: "system"},
		{ID: "2", SessionID: "s1", Role: models.RoleHuman, Content: "go"},
		{ID: "3", SessionID: "s1", Role: models.RoleAI, Content: longText},
		{ID: "4", SessionID: "s1", Role: models.RoleHuman, Content: "next"},
		{ID: "5", SessionID: "s1", Role: models.RoleAI, Content: "short"},
		{ID: "6", SessionID: "s1", Role: models.RoleAI, Content: "tail1"},
		{ID: "7", SessionID: "s1", Role: models.RoleAI, Content: "tail2"},
	}

	out, did := compactMessages(msgs, 2)
	if !did {
		t.Fatal("expected compaction")
	}

	for _, m := range out {
		if m.Role == models.RoleAI && strings.Contains(m.Content, "[...]") {
			if len(m.Content) > 250 {
				t.Errorf("compacted assistant text too long: %d", len(m.Content))
			}
		}
	}
}

func TestCompactMessages_TailPreserved(t *testing.T) {
	msgs := []models.Message{
		{ID: "1", Role: models.RoleSystem, Content: "system"},
		{ID: "2", Role: models.RoleHuman, Content: "go"},
		{ID: "3", Role: models.RoleAI, ToolCalls: []models.ToolCall{{ID: "c1", Name: "bash"}}, Content: ""},
		{ID: "4", Role: models.RoleTool, Content: "output"},
		{ID: "5", Role: models.RoleAI, Content: "thinking..."},
		{ID: "6", Role: models.RoleAI, Content: "more"},
		{ID: "7", Role: models.RoleTool, Content: "tail result"},
		{ID: "8", Role: models.RoleAI, Content: "final"},
	}

	keepTail := 3
	out, _ := compactMessages(msgs, keepTail)

	tail := out[len(out)-keepTail:]
	if tail[0].Content != "more" || tail[1].Content != "tail result" || tail[2].Content != "final" {
		t.Errorf("tail not preserved: %+v", tail)
	}
}

func TestEstimateTokens(t *testing.T) {
	msgs := []models.Message{
		{Role: models.RoleSystem, Content: strings.Repeat("a", 400)},
		{Role: models.RoleHuman, Content: strings.Repeat("b", 200)},
		{Role: models.RoleAI, Content: strings.Repeat("c", 300)},
	}
	// 400 + 200 + 300 + overhead ~= 900+ bytes. At /3, should be ~300 tokens.
	est := estimateTokens(msgs, "sys", 0)
	if est <= 0 {
		t.Error("estimate should be positive")
	}
	if est > 500 {
		t.Errorf("estimate too high: %d", est)
	}

	// Should NOT short-circuit to lastInputTokens when messages exist.
	est2 := estimateTokens(msgs, "sys", 100000)
	if est2 == 100000 {
		t.Error("estimateTokens short-circuited to lastInputTokens")
	}
	if est2 != est {
		t.Errorf("estimate changed with lastInputTokens: %d vs %d", est, est2)
	}
}
