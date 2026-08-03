package agent

import (
	"context"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
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
			{ID: "c3", Name: "edit_file", Arguments: map[string]any{"file": "test.go"}},
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

	// Arguments should be stripped from compacted tool calls to save space.
	for _, m := range out {
		if m.Role == models.RoleAI && strings.HasPrefix(m.Content, "[Called") {
			for _, tc := range m.ToolCalls {
				if tc.Arguments != nil {
					t.Errorf("compacted tool call %s retained arguments (should be stripped)", tc.ID)
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
	// 400 + 200 + 300 + overhead ~= 900+ bytes. At the calibrated bytesPerToken
	// (3.3), should be ~300 tokens.
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

// TestEstimateTokens_UsesCalibratedRatio pins estimateTokens to the calibrated
// 3.3 bytes/token ratio (commit 695bd80's content-weighted measurement),
// replacing the old flat /3 heuristic. metrics.go's estimateInputTokens
// already used 3.3; this guards that estimateTokens (the compaction-trigger
// path) and toolSchemaTokens agree with it via one shared constant
// (bytesPerToken), rather than three independent literals drifting apart.
func TestEstimateTokens_UsesCalibratedRatio(t *testing.T) {
	// A single message whose content length is exactly known, no system
	// prompt, so totalBytes == len(content) + the fixed per-message overhead
	// (30 bytes, see estimateTokens). Pick a byte count where /3 and /3.3
	// disagree by more than rounding noise so the test is a real pin, not a
	// coincidence.
	content := strings.Repeat("a", 3300) // + 30 overhead = 3330 bytes
	msgs := []models.Message{{Role: models.RoleHuman, Content: content}}

	totalBytes := 3300 + 30
	wantOld := totalBytes / 3                           // 1110
	wantNew := int(float64(totalBytes) / bytesPerToken) // 1009
	if wantOld == wantNew {
		t.Fatal("test setup does not distinguish /3 from /bytesPerToken(3.3); pick a different byte count")
	}

	got := estimateTokens(msgs, "", 0)
	if got != wantNew {
		t.Fatalf("estimateTokens = %d, want %d (calibrated %.1f bytes/token); got the old /3 value %d instead",
			got, wantNew, bytesPerToken, wantOld)
	}
}

// TestToolSchemaTokens_AccountsForToolPayload guards the fix for the bug where
// the context-window estimate ignored tool schemas (sent on every request) and
// so compacted later than the real payload warranted, letting a session slip
// past the threshold and hit the provider's hard limit. A non-trivial tool set
// must contribute a non-trivial token count, and an empty/absent registry must
// be safe.
func TestToolSchemaTokens_AccountsForToolPayload(t *testing.T) {
	var nilAgent *Agent
	if got := nilAgent.toolSchemaTokens(); got != 0 {
		t.Fatalf("nil agent toolSchemaTokens = %d, want 0", got)
	}

	empty := &Agent{tools: tools.NewRegistry()}
	if got := empty.toolSchemaTokens(); got != 0 {
		t.Fatalf("empty registry toolSchemaTokens = %d, want 0", got)
	}

	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name:        "bash",
		Description: strings.Repeat("run shell commands ", 20),
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": strings.Repeat("x", 300)},
			},
		},
		Handler: func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{}, nil
		},
	}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	a := &Agent{tools: reg}
	if got := a.toolSchemaTokens(); got < 100 {
		t.Fatalf("toolSchemaTokens = %d, want a meaningful (>=100) contribution", got)
	}
}

// TestEstimateContextTokens_PrefersProviderCount guards the fix for context
// growth slipping past the compaction threshold: the byte heuristic
// underestimates real tokens for CJK/dense content, so the estimate must defer
// to the provider's own reported input-token count once it is known.
func TestEstimateContextTokens_PrefersProviderCount(t *testing.T) {
	a := &Agent{compactionKeepTail: 6}
	canonical := []models.Message{
		{Role: models.RoleHuman, Content: "你好"},
		{Role: models.RoleAI, Content: "世界"},
	}
	// M4-2: the anchor path assumes the real react.go invariant — the view
	// passed in is canonical messages PLUS the trailing turn injection (see
	// appendTurnInjection), while lastTokenCountMsgs anchors only the
	// CANONICAL length. Modeling that here (rather than passing a bare
	// canonical slice as the view) is what TestEstimateContextTokens_
	// AnchorDoesNotDoubleCountInjection in injection_test.go guards directly;
	// this test still needs the same shape or it exercises a view/anchor
	// pairing that never occurs in production.
	injection := models.Message{Role: models.RoleHuman, Content: "[System note: irrelevant]"}
	view := appendTurnInjection(canonical, injection)

	heuristic := a.estimateContextTokens(view, "") // no anchor yet → small heuristic

	// Provider reports the real (much larger) count for these same messages.
	a.lastInputTokens = 50000
	a.lastTokenCountMsgs = len(canonical)
	got := a.estimateContextTokens(view, "")
	if got < 50000 {
		t.Fatalf("estimate = %d, want >= provider count 50000 (heuristic was %d)", got, heuristic)
	}

	// Growth since the anchor must be added on top of the provider count.
	canonical = append(canonical, models.Message{Role: models.RoleTool, Content: strings.Repeat("x", 3000)})
	view = appendTurnInjection(canonical, injection)
	if grown := a.estimateContextTokens(view, ""); grown <= got {
		t.Fatalf("estimate did not grow after appending a message: %d <= %d", grown, got)
	}

	// A stale anchor (more messages counted than now present, e.g. after a
	// compaction) must fall back to the heuristic rather than over-report.
	a.lastTokenCountMsgs = len(canonical) + 5
	if fallback := a.estimateContextTokens(view, ""); fallback >= 50000 {
		t.Fatalf("stale anchor not ignored: estimate=%d still reflects old provider count", fallback)
	}
}

// TestCompactOnOverflow_ActuallyCompacts guards against the dead-backstop
// regression: compactMessages preserves message count, so gating the retry on
// len() was always false. The reactive path must reduce the estimated token
// size and report success so the outer loop retries instead of hard-failing.
func TestCompactOnOverflow_ActuallyCompacts(t *testing.T) {
	a := &Agent{compactionKeepTail: 6, systemPrompt: "sys", logger: slog.Default()}
	msgs := []models.Message{{Role: models.RoleHuman, Content: "start"}}
	for i := 0; i < 20; i++ {
		msgs = append(msgs, models.Message{Role: models.RoleAI, Content: strings.Repeat("x", 4000)})
	}

	before := estimateTokens(msgs, "sys", 0)
	out, ok := a.compactOnOverflow(msgs, "sys", 1, "test")
	if !ok {
		t.Fatal("compactOnOverflow returned false: reactive overflow backstop is dead")
	}
	if after := estimateTokens(out, "sys", 0); after >= before {
		t.Fatalf("compactOnOverflow did not reduce tokens: before=%d after=%d", before, after)
	}
}

// TestCompactOnOverflow_StaleProviderAnchorDoesNotBlockCompaction guards a
// code-review finding on M3-3: the provider anchor (lastInputTokens /
// lastTokenCountMsgs) describes the request that JUST overflowed, so inside
// compactOnOverflow it is stale by definition. compactMessages preserves
// message *count*, so the anchor's cutoff index is still valid for the
// compacted messages too — meaning both the "before" and "after" estimates
// resolve to the same stale provider count (lastInputTokens + 0 delta),
// before==after, "after < before" is never true, and every tail candidate is
// rejected. Since this repo targets DeepSeek/Qwen/GLM (which do report
// input_tokens), the anchor is populated on the common path, so this isn't a
// rare edge case — it's the expected shape of a real overflow. This also
// doubles as M3-3 item 3's aging-off coverage: a is built without an aging
// config, so buildPromptView's view equals canonical here, and the fix must
// still hold under that pass-through case.
func TestCompactOnOverflow_StaleProviderAnchorDoesNotBlockCompaction(t *testing.T) {
	a := &Agent{compactionKeepTail: 6, systemPrompt: "sys", logger: slog.Default()}
	msgs := []models.Message{{Role: models.RoleHuman, Content: "start"}}
	for i := 0; i < 20; i++ {
		msgs = append(msgs, models.Message{Role: models.RoleAI, Content: strings.Repeat("x", 4000)})
	}

	// Simulate the provider having just reported usage for exactly this
	// message set — the request that then overflowed.
	a.lastInputTokens = 999999
	a.lastTokenCountMsgs = len(msgs)

	out, ok := a.compactOnOverflow(msgs, "sys", 1, "test")
	if !ok {
		t.Fatal("compactOnOverflow returned false with a stale provider anchor active: " +
			"the reactive overflow backstop is dead whenever the provider reports usage " +
			"(the common case for DeepSeek/Qwen/GLM)")
	}
	if len(out) != len(msgs) {
		t.Fatalf("compactMessages must preserve message count: got %d, want %d", len(out), len(msgs))
	}
}

// TestCompactOnOverflow_MeasuresAgedViewNotCanonicalBytes guards M3-3 item 3:
// compactOnOverflow must measure the same aged VIEW the main compaction
// trigger sends (buildPromptView's output), not raw canonical message bytes,
// even though it still mutates and returns canonical messages. With Aging
// enabled and most of the history old enough to be heavily compressed by the
// §5.4 read_file budget, the view is far smaller than canonical — this pins
// that compactOnOverflow's logged "before_tokens" reflects the (much
// smaller) view, not the raw canonical estimate.
func TestCompactOnOverflow_MeasuresAgedViewNotCanonicalBytes(t *testing.T) {
	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	big := strings.Repeat("z", 8000)
	var msgs []models.Message
	for i := 0; i < 10; i++ {
		msgs = append(msgs, aiTools(""), toolMsg("read_file", big))
	}

	a := &Agent{
		compactionKeepTail: 6,
		logger:             logger,
		contextWindow:      100000,
		aging: &AgingConfig{
			Enabled:             true,
			MinContextPressure:  0,
			ConversationBudgets: map[int]int{},
		},
	}

	rawCanonicalEstimate := estimateTokens(msgs, "sys", 0)

	_, ok := a.compactOnOverflow(msgs, "sys", 1, "test")
	if !ok {
		t.Fatal("compactOnOverflow returned false")
	}

	m := regexp.MustCompile(`before_tokens=(\d+)`).FindStringSubmatch(logBuf.String())
	if m == nil {
		t.Fatalf("could not find before_tokens in log output: %s", logBuf.String())
	}
	beforeTokens, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parse before_tokens: %v", err)
	}
	if beforeTokens >= rawCanonicalEstimate {
		t.Fatalf("compactOnOverflow's before-estimate (%d) was not smaller than the raw canonical "+
			"estimate (%d); it is measuring canonical bytes instead of the aged view", beforeTokens, rawCanonicalEstimate)
	}
}
