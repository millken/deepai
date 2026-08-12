package agent

import (
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

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

// TestCompactMessages_CollapsesLegacyNestedPlaceholders covers histories the
// buggy version already wrecked and persisted: nested placeholders whose
// payload is gone but which still occupy ~330 bytes each (about 110k tokens
// across the session that prompted this work). Compaction cannot bring the
// content back, but it must stop paying for the noise — and must still be
// idempotent afterwards.
func TestCompactMessages_CollapsesLegacyNestedPlaceholders(t *testing.T) {
	nested := "[tool result: read_file, " + strings.Repeat("[tool result: read_file, ", 11) + "113\t/// 轮询切片粒度...]"

	msgs := []models.Message{
		{ID: "sys", SessionID: "s1", Role: models.RoleSystem, Content: "system"},
		{ID: "h1", SessionID: "s1", Role: models.RoleHuman, Content: "resume"},
	}
	for _, c := range []string{"c1", "c2", "c3", "c4"} {
		msgs = append(msgs,
			models.Message{ID: "ai-" + c, SessionID: "s1", Role: models.RoleAI,
				ToolCalls: []models.ToolCall{{ID: c, Name: "read_file", Arguments: map[string]any{"path": c}}}},
			models.Message{ID: "tool-" + c, SessionID: "s1", Role: models.RoleTool, Content: nested,
				ToolResult: &models.ToolResult{CallID: c, ToolName: "read_file", Status: models.CallStatusCompleted}},
		)
	}

	once, did := compactMessages(msgs, 2)
	if !did {
		t.Fatal("expected the legacy junk to be collapsed")
	}
	for _, m := range once {
		if m.Role != models.RoleTool || m.ID == "tool-c4" { // c4 sits in the protected tail
			continue
		}
		if n := strings.Count(m.Content, "[tool result:"); n != 1 {
			t.Errorf("%s: still nested %d deep: %q", m.ID, n, m.Content)
		}
		if len(m.Content) >= len(nested) {
			t.Errorf("%s: collapse saved nothing (%d bytes, was %d)", m.ID, len(m.Content), len(nested))
		}
		if !strings.Contains(m.Content, "read_file") {
			t.Errorf("%s: collapsed marker lost the tool name: %q", m.ID, m.Content)
		}
	}

	// Still idempotent: a second pass changes nothing.
	twice, didAgain := compactMessages(once, 2)
	if didAgain {
		t.Error("collapsing legacy junk must be a one-time transition, not a per-turn rewrite")
	}
	for i := range twice {
		if twice[i].Content != once[i].Content {
			t.Errorf("message %q changed on re-compaction: %q -> %q", twice[i].ID, once[i].Content, twice[i].Content)
		}
	}
}
