package chat

import (
	"testing"
	"time"

	"github.com/millken/deepai/pkg/models"
)

// run_code Phase 2 (§9): the session record keeps the script source for
// post-hoc review — Data["script"]/["journal"]/["outcome"] must survive the
// AppendMessage → LoadMessages roundtrip (they are stored as the tool_result
// JSON column).
func TestSessionRunCodeDataRoundtrip(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Model: "test-model", CWD: "/tmp"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	script := "const hits = await tools.grep({pattern: \"Close\"});\nreturn hits.data.matches.length;"
	res := models.ToolResult{
		CallID:   "rc_1",
		ToolName: "run_code",
		Status:   models.CallStatusCompleted,
		Content:  "run_code ok — 1 tool call in 0.1s (grep×1)\nresult:\n3\nwrites: none",
		Data: map[string]any{
			"script":  script,
			"outcome": "ok",
			"journal": []map[string]any{
				{"seq": 1, "tool": "grep", "status": "completed", "ms": 41, "bytes": 3120, "write": false},
			},
			"edited_paths": []string{"/abs/pkg/tools/builtin/grep.go"},
		},
	}
	msg := models.Message{
		ID:         "m1",
		SessionID:  sess.ID,
		Role:       models.RoleTool,
		ToolResult: &res,
		CreatedAt:  time.Now().UTC(),
	}
	if err := store.AppendMessage(sess.ID, msg); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	loaded, err := store.LoadMessages(sess.ID)
	if err != nil {
		t.Fatalf("LoadMessages: %v", err)
	}
	if len(loaded) != 1 || loaded[0].ToolResult == nil {
		t.Fatalf("loaded = %+v", loaded)
	}
	data := loaded[0].ToolResult.Data
	if got, _ := data["script"].(string); got != script {
		t.Fatalf("script roundtrip:\n got %q\nwant %q", got, script)
	}
	if got, _ := data["outcome"].(string); got != "ok" {
		t.Fatalf("outcome roundtrip = %q", got)
	}
	paths, _ := data["edited_paths"].([]any)
	if len(paths) != 1 || paths[0] != "/abs/pkg/tools/builtin/grep.go" {
		t.Fatalf("edited_paths roundtrip = %#v", data["edited_paths"])
	}
	journal, _ := data["journal"].([]any)
	if len(journal) != 1 {
		t.Fatalf("journal roundtrip = %#v", data["journal"])
	}
	if e, _ := journal[0].(map[string]any); e["tool"] != "grep" {
		t.Fatalf("journal[0] = %#v", journal[0])
	}
}
