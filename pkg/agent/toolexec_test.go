package agent

import (
	"testing"

	"github.com/millken/deepai/pkg/models"
)

// TestFlushPendingHints_Idempotent guards the LOW hardening from code review:
// flushPendingHints must be safe to call twice. Without clearing
// b.pendingHints after the append, a future early-return path that calls
// flushPendingHints and then still falls through to finishBatch (which also
// calls flushPendingHints) would duplicate every hint message in
// runMessages.
func TestFlushPendingHints_Idempotent(t *testing.T) {
	b := newToolBatchState(&Agent{}, "sess", 0, nil, &Usage{}, func(AgentEvent) {}, nil)
	b.pendingHints = []models.Message{
		{ID: "hint_1", SessionID: "sess", Role: models.RoleHuman, Content: "hint"},
	}

	b.flushPendingHints()
	if len(b.runMessages) != 1 {
		t.Fatalf("after first flush: runMessages = %d, want 1", len(b.runMessages))
	}
	if len(b.pendingHints) != 0 {
		t.Fatalf("after first flush: pendingHints = %d, want 0 (cleared)", len(b.pendingHints))
	}

	b.flushPendingHints()
	if len(b.runMessages) != 1 {
		t.Fatalf("after second flush: runMessages = %d, want 1 (still, not duplicated)", len(b.runMessages))
	}
}
