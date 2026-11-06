package agent

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/memory"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

func TestSessionLedger_TracksFailureAndClearsItOnSuccess(t *testing.T) {
	l := &sessionLedger{}
	call := models.ToolCall{
		ID:        "1",
		Name:      "edit_file",
		Arguments: map[string]any{"path": "pkg/a.go"},
	}
	failed := models.ToolResult{CallID: "1", ToolName: "edit_file", Status: models.CallStatusFailed, Error: "UNIQUE_HASH_MISS"}
	if !l.note(call, failed, "", false) {
		t.Fatal("first failure should change the brief")
	}
	if l.note(call, failed, "", false) {
		t.Fatal("repeating the same failure should not churn the brief")
	}
	got := l.render()
	if !strings.Contains(got, "pkg/a.go") || !strings.Contains(got, "UNIQUE_HASH_MISS") {
		t.Fatalf("brief missing path or failure: %s", got)
	}

	summary := "[tool result: edit_file, short...]"
	if l.note(call, models.ToolResult{ToolName: "edit_file", Status: models.CallStatusCompleted}, summary, true) {
		t.Fatal("a compaction summary must not overwrite a live failure")
	}
	if !strings.Contains(l.render(), "UNIQUE_HASH_MISS") {
		t.Fatalf("summary clobbered the live failure: %s", l.render())
	}

	ok := models.ToolResult{ToolName: "edit_file", Status: models.CallStatusCompleted, Content: "updated"}
	if !l.note(call, ok, "updated", false) {
		t.Fatal("a later success should update the brief")
	}
	got = l.render()
	if strings.Contains(got, "UNIQUE_HASH_MISS") {
		t.Fatalf("success left the old failure in the brief: %s", got)
	}
	if !strings.Contains(got, "edited pkg/a.go — ok") {
		t.Fatalf("success not recorded: %s", got)
	}
}

func TestSessionLedger_BashExitAndObjective(t *testing.T) {
	l := &sessionLedger{}
	l.absorb([]models.Message{{Role: models.RoleHuman, Content: "fix the parser\nplease"}})
	body := `{"stdout":"ok","stderr":"undefined: foo","exit_code":1}`
	call := models.ToolCall{Name: "bash", Arguments: map[string]any{"command": "go test ./pkg/parser"}}
	res := models.ToolResult{ToolName: "bash", Status: models.CallStatusCompleted, Content: body}
	l.note(call, res, body, false)
	got := l.render()
	if !strings.Contains(got, "Objective: fix the parser please") {
		t.Fatalf("objective not collapsed onto one line: %s", got)
	}
	if !strings.Contains(got, "bash exit 1") || !strings.Contains(got, "undefined: foo") {
		t.Fatalf("bash failure missing: %s", got)
	}
	// A successful bash (exit 0) is not a file touch and not a failure.
	okBody := `{"stdout":"pass","stderr":"","exit_code":0}`
	if l.note(call, models.ToolResult{ToolName: "bash", Content: okBody, Status: models.CallStatusCompleted}, okBody, false) {
		t.Fatal("successful bash should not change the brief")
	}
}

func TestSessionLedger_ResumeFillsPathFromSummaryOnly(t *testing.T) {
	l := &sessionLedger{}
	l.absorb([]models.Message{
		{Role: models.RoleHuman, Content: "task"},
		{Role: models.RoleAI, ToolCalls: []models.ToolCall{{
			ID: "c", Name: "read_file", Arguments: map[string]any{"path": "main.go"},
		}}},
		{Role: models.RoleTool, Content: "[tool result: read_file, package main...]", ToolResult: &models.ToolResult{
			CallID: "c", ToolName: "read_file", Status: models.CallStatusCompleted,
		}},
	})
	got := l.render()
	if !strings.Contains(got, "read main.go — compacted") {
		t.Fatalf("restart should still recover the path: %s", got)
	}
}

func TestSessionLedger_RenderCapDoesNotShrinkLedger(t *testing.T) {
	l := &sessionLedger{}
	var last string
	for i := 0; i < maxSessionFiles; i++ {
		last = "LAST_" + strings.Repeat("b", 300)
		if i == 0 {
			last = "FIRST_" + strings.Repeat("a", 300)
		}
		path := last
		if i > 0 {
			path = "LAST_" + strings.Repeat("b", 280) + string(rune('a'+i))
			last = path
		}
		call := models.ToolCall{Name: "read_file", Arguments: map[string]any{"path": path}}
		l.note(call, models.ToolResult{ToolName: "read_file", Status: models.CallStatusCompleted}, "ok", false)
	}
	if len(l.touches) != maxSessionFiles {
		t.Fatalf("ledger kept %d files, want %d", len(l.touches), maxSessionFiles)
	}
	got := l.render()
	if len(got) > sessionBriefMaxBytes {
		t.Fatalf("brief is %d bytes, cap is %d", len(got), sessionBriefMaxBytes)
	}
	if !strings.Contains(got, "LAST_") {
		t.Fatalf("brief dropped the newest file: %s", got)
	}
	if strings.Contains(got, "FIRST_") {
		t.Fatalf("brief kept the oldest file past the byte cap: %s", got)
	}
	if len(l.touches) != maxSessionFiles {
		t.Fatal("render mutated the ledger")
	}
}

func TestSessionLedger_CarriesAcrossAgents(t *testing.T) {
	carry := NewSessionCarry()
	a1 := New(AgentConfig{Session: carry})
	call := models.ToolCall{Name: "read_file", Arguments: map[string]any{"path": "pkg/uniq.go"}}
	a1.noteSessionTool(call, models.ToolResult{ToolName: "read_file", Status: models.CallStatusFailed, Error: "UNIQUE_READ_ERROR"})
	a2 := New(AgentConfig{Session: carry})
	if a2.ledger != a1.ledger {
		t.Fatal("new agent did not share the carried ledger")
	}
	got := a2.buildTurnInjection(context.Background(), "s", nil).Content
	if !strings.Contains(got, "pkg/uniq.go") || !strings.Contains(got, "UNIQUE_READ_ERROR") {
		t.Fatalf("carried brief missing from the next run's injection: %s", got)
	}
}

func TestMaybeCompact_SkipsMemoryFlushUnlessOptedIn(t *testing.T) {
	ctx := context.Background()
	store, err := memory.NewSQLiteStore(ctx, filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(store.Close)
	if err := store.AutoMigrate(ctx); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	svc := memory.NewService(slog.Default(), store, nil)
	t.Cleanup(func() { svc.Close(ctx) })

	msgs := []models.Message{{Role: models.RoleHuman, Content: "task"}}
	for i := 0; i < 4; i++ {
		id := "c" + string(rune('a'+i))
		msgs = append(msgs,
			models.Message{Role: models.RoleAI, ToolCalls: []models.ToolCall{{
				ID: id, Name: "read_file", Arguments: map[string]any{"path": "f.go"},
			}}},
			models.Message{Role: models.RoleTool, Content: strings.Repeat("x", 4000), ToolResult: &models.ToolResult{
				CallID: id, ToolName: "read_file", Status: models.CallStatusCompleted,
			}},
		)
	}

	for _, inject := range []bool{false, true} {
		ext := &countingExtractor{}
		a := New(AgentConfig{
			ContextWindow:       200,
			CompactionThreshold: 0.2,
			MemoryService:       svc,
			MemoryExtractor:     ext,
			InjectMemory:        inject,
		})
		compacted, _ := a.maybeCompact(ctx, "sess", 1, msgs, msgs, "sys", func(AgentEvent) {})
		sawSummary := false
		for _, m := range compacted {
			if strings.Contains(m.Content, "[tool result:") {
				sawSummary = true
			}
		}
		if !sawSummary {
			t.Fatal("setup: compaction did not rewrite a tool result")
		}
		got := ext.calls.Load()
		if inject && got != 1 {
			t.Fatalf("InjectMemory true: flush ran %d times, want 1", got)
		}
		if !inject && got != 0 {
			t.Fatalf("InjectMemory false: flush ran %d times, want 0", got)
		}
	}
}

// briefCaptureProvider issues one edit_file call, then a final answer, and
// records every request view.
type briefCaptureProvider struct {
	mu   sync.Mutex
	seen [][]models.Message
	n    int
}

func (p *briefCaptureProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *briefCaptureProvider) Stream(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.mu.Lock()
	view := append([]models.Message(nil), req.Messages...)
	p.seen = append(p.seen, view)
	p.n++
	n := p.n
	p.mu.Unlock()

	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		if n == 1 {
			ch <- llm.StreamChunk{
				Message: &models.Message{Role: models.RoleAI, ToolCalls: []models.ToolCall{{
					ID:        "e1",
					Name:      "edit_file",
					Arguments: map[string]any{"path": "pkg/parser.go"},
				}}},
				Done: true,
			}
			return
		}
		ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, Content: "done"}, Done: true}
	}()
	return ch, nil
}

func TestTurnInjection_BriefUpdatesAfterToolFailure(t *testing.T) {
	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name: "edit_file",
		Handler: func(ctx context.Context, c models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{Status: models.CallStatusFailed, Error: "UNIQUE_HASH_MISS"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	p := &briefCaptureProvider{}
	carry := NewSessionCarry()
	a := New(AgentConfig{LLMProvider: p, Tools: reg, Model: "m", Session: carry})
	if _, err := a.Run(context.Background(), "sess-brief", []models.Message{
		{Role: models.RoleHuman, Content: "fix the parser"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.seen) < 2 {
		t.Fatalf("expected 2 requests, got %d", len(p.seen))
	}
	first := p.seen[0][len(p.seen[0])-1].Content
	if !strings.Contains(first, "Objective: fix the parser") {
		t.Fatalf("first request missing the objective: %s", first)
	}
	if strings.Contains(first, "pkg/parser.go") {
		t.Fatalf("first request named a file the tool had not touched yet: %s", first)
	}
	second := p.seen[1][len(p.seen[1])-1].Content
	if !strings.Contains(second, "pkg/parser.go") || !strings.Contains(second, "UNIQUE_HASH_MISS") {
		t.Fatalf("second request missing the working set: %s", second)
	}
	if strings.Contains(second, "<memory-context>") {
		t.Fatalf("memory was injected without InjectMemory: %s", second)
	}
}
