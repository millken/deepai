package agent

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/millken/deepai/pkg/models"
)

// aiTools builds a RoleAI message that carries tool calls (a tool-call turn).
func aiTools(content string) models.Message {
	return models.Message{
		Role:      models.RoleAI,
		Content:   content,
		ToolCalls: []models.ToolCall{{ID: "c", Name: "read_file", Status: models.CallStatusCompleted}},
	}
}

func aiText(content string) models.Message {
	return models.Message{Role: models.RoleAI, Content: content}
}

func toolMsg(name, content string) models.Message {
	return models.Message{
		Role:       models.RoleTool,
		Content:    content,
		ToolResult: &models.ToolResult{CallID: "c", ToolName: name, Status: models.CallStatusCompleted},
	}
}

// t1Config returns an aging config with T4 disabled (empty ConversationBudgets)
// and the pressure gate off, so tests exercise T1 deterministically.
func t1Config() *AgingConfig {
	return &AgingConfig{
		Enabled:             true,
		MinContextPressure:  0, // gate off
		ConversationBudgets: map[int]int{},
	}
}

func TestBuildPromptView_NilConfigPassthrough(t *testing.T) {
	msgs := []models.Message{aiTools(""), toolMsg("read_file", "big")}
	out := buildPromptView(msgs, nil, 100000)
	if len(out) != len(msgs) || out[1].Content != "big" {
		t.Fatalf("nil config should pass through unchanged, got %+v", out)
	}
}

func TestBuildPromptView_DisabledPassthrough(t *testing.T) {
	msgs := []models.Message{aiTools(""), toolMsg("read_file", strings.Repeat("x", 10000))}
	out := buildPromptView(msgs, &AgingConfig{Enabled: false}, 100000)
	if len(out[1].Content) != 10000 {
		t.Fatalf("disabled config should not compress, got %d bytes", len(out[1].Content))
	}
}

func TestBuildPromptView_AgesToolResultsByStep(t *testing.T) {
	big := strings.Repeat("x", 10000)
	// 3 tool-call turns; the last tool result is the current turn (age 0).
	msgs := []models.Message{
		{Role: models.RoleHuman, Content: "q"},
		aiTools(""),                 // aiTurnIndex 0
		toolMsg("read_file", big),   // owner 0 -> age 2
		aiTools(""),                 // aiTurnIndex 1
		toolMsg("grep", big),        // owner 1 -> age 1
		aiTools(""),                 // aiTurnIndex 2 (current)
		toolMsg("list_dir", big),    // owner 2 -> age 0
	}
	out := buildPromptView(msgs, t1Config(), 0)

	// age 2 -> budget 2048
	if got := len(out[2].Content); got != 2048+len("\n[...aged: re-call read_file to see full output]") {
		t.Errorf("age2 tool: want 2048+hint bytes, got %d", got)
	}
	if !strings.Contains(out[2].Content, "re-call read_file") {
		t.Errorf("age2 tool: missing re-call hint: %q", out[2].Content[len(out[2].Content)-80:])
	}
	// age 1 -> budget 8192
	if got := len(out[4].Content); got != 8192+len("\n[...aged: re-call grep to see full output]") {
		t.Errorf("age1 tool: want 8192+hint bytes, got %d", got)
	}
	// age 0 -> untouched
	if len(out[6].Content) != 10000 {
		t.Errorf("age0 tool (current turn) must be untouched, got %d bytes", len(out[6].Content))
	}
}

func TestBuildPromptView_DoesNotMutateCanonical(t *testing.T) {
	big := strings.Repeat("x", 10000)
	msgs := []models.Message{
		aiTools(""),               // idx 0
		toolMsg("read_file", big), // age 1
		aiTools(""),               // idx 1 (current)
		toolMsg("grep", "small"),  // age 0
	}
	_ = buildPromptView(msgs, t1Config(), 0)
	if len(msgs[1].Content) != 10000 {
		t.Fatalf("canonical message was mutated: got %d bytes, want 10000", len(msgs[1].Content))
	}
}

func TestBuildPromptView_PressureGate(t *testing.T) {
	big := strings.Repeat("x", 10000)
	msgs := []models.Message{
		aiTools(""),               // idx 0
		toolMsg("read_file", big), // age 1
		aiTools(""),               // idx 1 (current)
		toolMsg("grep", "small"),  // age 0
	}
	cfg := &AgingConfig{Enabled: true, MinContextPressure: 0.4, ConversationBudgets: map[int]int{}}

	// Large window: ~ (30KB bytes)/3 ≈ 10k tokens < 0.4 * 100000 = 40k -> gate holds.
	out := buildPromptView(msgs, cfg, 100000)
	if len(out[1].Content) != 10000 {
		t.Errorf("under pressure threshold: should NOT compress, got %d bytes", len(out[1].Content))
	}

	// Small window: threshold 0.4*1000 = 400 tokens; ~10k tokens -> gate passes.
	out = buildPromptView(msgs, cfg, 1000)
	if len(out[1].Content) >= 10000 {
		t.Errorf("over pressure threshold: should compress, got %d bytes", len(out[1].Content))
	}
}

func TestBuildPromptView_TextAITurnIsBoundary(t *testing.T) {
	big := strings.Repeat("x", 10000)
	// A pure-text AI message between turns must still advance the turn index,
	// so the tool below it ages correctly.
	msgs := []models.Message{
		aiTools(""),               // idx 0
		toolMsg("read_file", big), // owner 0
		aiText("done reasoning"),  // idx 1 (pure text, no tools)
		aiTools(""),               // idx 2 (current)
		toolMsg("grep", "small"),  // owner 2 -> age 0
	}
	out := buildPromptView(msgs, t1Config(), 0)
	// totalAITurns=3, tool owner 0 -> age 2 -> budget 2048 -> compressed.
	if len(out[1].Content) >= 10000 {
		t.Errorf("tool under text-AI boundary should be aged, got %d bytes", len(out[1].Content))
	}
}

func TestBuildPromptView_T4DisabledByEmptyMap(t *testing.T) {
	bigText := strings.Repeat("y", 5000)
	msgs := []models.Message{
		aiText(bigText), // idx 0 -> age 1 historical AI text
		{Role: models.RoleHuman, Content: "next"},
		aiText("now"), // idx 1 (current)
	}
	out := buildPromptView(msgs, t1Config(), 0) // ConversationBudgets empty
	if len(out[0].Content) != 5000 {
		t.Errorf("T1-only mode must not compress AI text, got %d bytes", len(out[0].Content))
	}
}

func TestBuildPromptView_T4CompressesWhenConfigured(t *testing.T) {
	bigText := strings.Repeat("y", 5000)
	msgs := []models.Message{
		aiText(bigText), // idx 0
		{Role: models.RoleHuman, Content: "a"},
		aiText("b"), // idx 1
		{Role: models.RoleHuman, Content: "c"},
		aiText("now"), // idx 2 (current); idx0 age=2
	}
	cfg := &AgingConfig{Enabled: true, ConversationBudgets: map[int]int{2: 500}}
	out := buildPromptView(msgs, cfg, 0)
	if len(out[0].Content) != 500+len("\n[...earlier response truncated]") {
		t.Errorf("T4: age2 AI text should truncate to 500+hint, got %d bytes", len(out[0].Content))
	}
}

func TestBuildPromptView_PreservesToolCallsStructure(t *testing.T) {
	big := strings.Repeat("x", 10000)
	msgs := []models.Message{
		aiTools("reasoning text"), // idx 0 (has tool calls)
		toolMsg("read_file", big), // owner 0
		aiTools(""),               // idx 1 (current)
		toolMsg("grep", "s"),      // age 0
	}
	out := buildPromptView(msgs, t1Config(), 0)
	// The historical AI message keeps its ToolCalls untouched.
	if len(out[0].ToolCalls) != 1 || out[0].ToolCalls[0].Name != "read_file" {
		t.Errorf("ToolCalls structure must be preserved, got %+v", out[0].ToolCalls)
	}
}

func TestBuildPromptView_NoWindowFailsSafe(t *testing.T) {
	// With a pressure gate configured but contextWindow unknown (0), aging must
	// NOT apply — failing open would age from turn one in short sessions.
	big := strings.Repeat("x", 10000)
	msgs := []models.Message{
		aiTools(""),               // idx 0
		toolMsg("read_file", big), // age 1
		aiTools(""),               // idx 1 (current)
		toolMsg("grep", "small"),  // age 0
	}
	cfg := &AgingConfig{Enabled: true, MinContextPressure: 0.4, ConversationBudgets: map[int]int{}}
	out := buildPromptView(msgs, cfg, 0) // window unknown
	if len(out[1].Content) != 10000 {
		t.Errorf("contextWindow=0 with gate configured must skip aging, got %d bytes", len(out[1].Content))
	}
}

func TestBuildPromptView_UTF8SafeTruncation(t *testing.T) {
	// CJK content: byte-index cuts would split 3-byte runes at the budget
	// boundary; the view must remain valid UTF-8.
	cjk := strings.Repeat("中文内容测试", 800) // 3 bytes/rune, not aligned to budgets
	msgs := []models.Message{
		aiTools(""),               // idx 0
		toolMsg("read_file", cjk), // age 2 -> 2048-byte budget
		aiTools(""),               // idx 1
		toolMsg("grep", cjk),      // age 1 -> 8192-byte budget
		aiTools(""),               // idx 2 (current)
	}
	out := buildPromptView(msgs, t1Config(), 0)
	for i, m := range out {
		if !utf8.ValidString(m.Content) {
			t.Errorf("message %d: aged content is invalid UTF-8", i)
		}
	}
	if len(out[1].Content) >= len(cjk) {
		t.Error("age-2 CJK content should still be compressed")
	}
}

func TestBudgetForAge_StepFunction(t *testing.T) {
	b := map[int]int{1: 8192, 2: 2048, 3: 300}
	cases := map[int]int{0: 0, 1: 8192, 2: 2048, 3: 300, 5: 300}
	for age, want := range cases {
		if got := budgetForAge(b, age); got != want {
			t.Errorf("budgetForAge(age=%d) = %d, want %d", age, got, want)
		}
	}
}
