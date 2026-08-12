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
