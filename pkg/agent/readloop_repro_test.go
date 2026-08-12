package agent

import (
	"fmt"
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

// TestCompactMessages_IsIdempotent documents the second half of the failure:
// compaction re-wraps content it already summarized. Because only the first 300
// bytes survive each pass and the wrapper prefix is 25 bytes, ~12 passes leave
// nothing but nested "[tool result: read_file," prefixes — and the payload is
// written back into the canonical history (and the session DB), so it is gone
// for good. It also never reports "nothing left to compact", so the stall guard
// never engages and every later turn wraps again.
func TestCompactMessages_IsIdempotent(t *testing.T) {
	msgs := []models.Message{
		{ID: "sys", SessionID: "s1", Role: models.RoleSystem, Content: "system"},
		{ID: "h1", SessionID: "s1", Role: models.RoleHuman, Content: "task"},
	}
	for _, c := range []string{"c1", "c2", "c3", "c4", "c5"} {
		msgs = append(msgs, mkReadRound(c, c+".dart", "// "+c+"\n"+strings.Repeat("x", 5_000))...)
	}

	once, did := compactMessages(msgs, 2)
	if !did {
		t.Fatal("expected first pass to compact")
	}
	twice, didAgain := compactMessages(once, 2)
	if didAgain {
		t.Error("second pass over already-compacted messages reported another compaction; " +
			"the stall guard can never engage")
	}
	for i := range twice {
		if twice[i].Role != models.RoleTool {
			continue
		}
		if n := strings.Count(twice[i].Content, "[tool result:"); n > 1 {
			t.Errorf("message %q: placeholder nested %d deep after 2 passes: %q",
				twice[i].ID, n, twice[i].Content)
		}
		if twice[i].Content != once[i].Content {
			t.Errorf("message %q: content changed on an idempotent re-compaction:\n once: %q\ntwice: %q",
				twice[i].ID, once[i].Content, twice[i].Content)
		}
	}
}
