package agent

import (
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

// mkReadRound builds one assistant tool-call round: an AI message issuing a
// read_file call plus the tool result carrying the file body.
func mkReadRound(callID, path, body string) []models.Message {
	return []models.Message{
		{
			ID:        "ai-" + callID,
			SessionID: "s1",
			Role:      models.RoleAI,
			ToolCalls: []models.ToolCall{{ID: callID, Name: "read_file", Arguments: map[string]any{"path": path}}},
		},
		{
			ID:         "tool-" + callID,
			SessionID:  "s1",
			Role:       models.RoleTool,
			Content:    body,
			ToolResult: &models.ToolResult{CallID: callID, ToolName: "read_file", Status: models.CallStatusCompleted},
		},
	}
}

// TestAging_ToolRoundsWithinOneRequestKeepFullFidelity is the regression test
// for the read loop of session 20260812_093415_fc6e (1136 read_file calls over
// 7 files, 858 of them the same file).
//
// Aging used to measure age in *assistant messages*, and this agent's models
// emit one assistant message per tool call, so a file read only three tool
// calls ago was already cut to 300 bytes and stamped "re-call read_file" —
// while the model was still working on it. Reading a handful of files in a row
// evicted the first ones, the model re-read them as instructed, and each
// re-read evicted the others: an unbreakable cycle. Age is now measured in user
// turns, so everything the agent did for the current request stays whole.
func TestAging_ToolRoundsWithinOneRequestKeepFullFidelity(t *testing.T) {
	bodyA := "// tcp.dart\n" + strings.Repeat("a", 20_000)
	msgs := []models.Message{
		{ID: "sys", SessionID: "s1", Role: models.RoleSystem, Content: "system"},
		{ID: "h1", SessionID: "s1", Role: models.RoleHuman, Content: "给 connectWithRetry 加一个 handshake 参数"},
	}
	msgs = append(msgs, mkReadRound("c1", "tcp.dart", bodyA)...)
	msgs = append(msgs, mkReadRound("c2", "udp.dart", "// udp.dart\n"+strings.Repeat("b", 20_000))...)
	msgs = append(msgs, mkReadRound("c3", "reconnect_helper.dart", "// reconnect\n"+strings.Repeat("c", 20_000))...)
	msgs = append(msgs, mkReadRound("c4", "connection_manager.dart", "// manager\n"+strings.Repeat("d", 20_000))...)

	cfg := &AgingConfig{Enabled: true, MinContextPressure: defaultMinContextPressure}
	// Pressure gate open: same situation as a real long session.
	view := buildPromptView(msgs, cfg, 1000)

	var got string
	for _, m := range view {
		if m.ID == "tool-c1" {
			got = m.Content
		}
	}
	if got == "" {
		t.Fatal("tool-c1 missing from view")
	}
	if len(got) != len(bodyA) {
		t.Errorf("read from 3 tool calls ago in the SAME user request was truncated to %d bytes "+
			"(from %d); the model would have no way to finish the edit except to re-read the file, "+
			"which ages out the others in turn", len(got), len(bodyA))
	}

	// A second user request does age it — decay across requests is the point of T1.
	next := append(append([]models.Message{}, msgs...),
		models.Message{ID: "h2", SessionID: "s1", Role: models.RoleHuman, Content: "现在跑一下测试"})
	next = append(next, mkReadRound("c5", "tcp_test.dart", "// test\n"+strings.Repeat("e", 20_000))...)
	for _, m := range buildPromptView(next, cfg, 1000) {
		if m.ID == "tool-c1" && len(m.Content) >= len(bodyA) {
			t.Errorf("read from the PREVIOUS user request should have been aged, got %d bytes", len(m.Content))
		}
	}
}

// TestAging_InjectedHintDoesNotOpenAUserTurn guards the age axis against the
// agent's own synthetic RoleHuman messages. A circuit-breaker hint (or a
// view_image attachment, or a schema-retry prompt) is appended to the history
// mid-run; if it counted as a user turn it would instantly age out everything
// the model is working on — reintroducing the read loop through the back door,
// and specifically at the exact moment the breaker suspects a loop.
func TestAging_InjectedHintDoesNotOpenAUserTurn(t *testing.T) {
	bodyA := "// tcp.dart\n" + strings.Repeat("a", 20_000)
	msgs := []models.Message{
		{ID: "sys", SessionID: "s1", Role: models.RoleSystem, Content: "system"},
		{ID: "h1", SessionID: "s1", Role: models.RoleHuman, Content: "改一下重连逻辑"},
	}
	msgs = append(msgs, mkReadRound("c1", "tcp.dart", bodyA)...)
	msgs = append(msgs, mkReadRound("c2", "udp.dart", "// udp\n"+strings.Repeat("b", 20_000))...)
	// The breaker fires a hint, exactly as observe() would append it.
	msgs = append(msgs, models.Message{
		ID:        "hint1",
		SessionID: "s1",
		Role:      models.RoleHuman,
		Metadata:  map[string]string{metaAgentInjected: "true"},
		Content:   "You have run \"read_file\" 5 times with identical arguments.",
	})
	msgs = append(msgs, mkReadRound("c3", "reconnect.dart", "// reconnect\n"+strings.Repeat("c", 20_000))...)

	cfg := &AgingConfig{Enabled: true, MinContextPressure: defaultMinContextPressure}
	for _, m := range buildPromptView(msgs, cfg, 1000) {
		if m.ID == "tool-c1" && len(m.Content) != len(bodyA) {
			t.Errorf("an agent-injected hint aged the current request's working set: "+
				"tool-c1 went from %d to %d bytes", len(bodyA), len(m.Content))
		}
	}
}
