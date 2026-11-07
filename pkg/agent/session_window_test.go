package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
	"github.com/millken/deepai/pkg/tools/builtin"
)

func TestResolveKeepFullTurns(t *testing.T) {
	if got := ResolveKeepFullTurns(0); got != DefaultKeepFullTurns {
		t.Fatalf("absent config = %d, want default %d", got, DefaultKeepFullTurns)
	}
	if got := ResolveKeepFullTurns(-1); got != 0 {
		t.Fatalf("negative config = %d, want window disabled", got)
	}
	if got := ResolveKeepFullTurns(4); got != 4 {
		t.Fatalf("explicit config = %d, want 4", got)
	}
}

func TestSplitKept_DropsWholeTurns(t *testing.T) {
	var msgs []models.Message
	msgs = append(msgs, models.Message{
		Role:     models.RoleHuman,
		Content:  "injected",
		Metadata: map[string]string{metaAgentInjected: "true"},
	})
	for i := 1; i <= 4; i++ {
		msgs = append(msgs,
			models.Message{Role: models.RoleHuman, Content: fmt.Sprintf("u%d", i)},
			models.Message{Role: models.RoleAI, Content: fmt.Sprintf("a%d", i)},
		)
	}
	boundary, kept := splitKept(msgs, 2)
	if boundary != 2 {
		t.Fatalf("boundary = %d, want 2", boundary)
	}
	if kept[0].Content != "u3" {
		t.Fatalf("kept starts at %q, want u3", kept[0].Content)
	}
	blob := joinContents(kept)
	if strings.Contains(blob, "u1") || strings.Contains(blob, "u2") || strings.Contains(blob, "injected") {
		t.Fatalf("dropped turns leaked into the window: %s", blob)
	}
	if _, all := splitKept(msgs, 0); len(all) != len(msgs) {
		t.Fatal("keep <= 0 must return the transcript unchanged")
	}
	if b, _ := splitKept(msgs, 10); b != 0 {
		t.Fatalf("short transcript boundary = %d, want 0", b)
	}
}

func TestSessionSummary_StaleJobDoesNotClobber(t *testing.T) {
	s := &sessionSummary{}
	id1, _, ok := s.begin(3)
	if !ok {
		t.Fatal("begin")
	}
	if _, _, ok := s.begin(4); ok {
		t.Fatal("a second job started while one was in flight")
	}
	if !s.complete(id1, "A", 3) {
		t.Fatal("complete")
	}
	if s.complete(id1, "B", 3) {
		t.Fatal("completing the same id twice published again")
	}

	id2, prev, ok := s.begin(5)
	if !ok || prev != "A" {
		t.Fatalf("second begin ok=%v prev=%q", ok, prev)
	}
	if s.complete(id1, "STALE", 5) {
		t.Fatal("stale id published over the in-flight job")
	}
	if s.complete(id2, "BACK", 2) {
		t.Fatal("complete moved covered backwards")
	}
	text, covered, busy := s.snapshot()
	if text != "A" || covered != 3 || !busy {
		t.Fatalf("after rejected complete: text=%q covered=%d busy=%v", text, covered, busy)
	}
	if !s.complete(id2, "C", 5) {
		t.Fatal("in-flight complete")
	}
	text, covered, busy = s.snapshot()
	if text != "C" || covered != 5 || busy {
		t.Fatalf("after fresh complete: text=%q covered=%d busy=%v", text, covered, busy)
	}

	id3, _, ok := s.begin(6)
	if !ok {
		t.Fatal("begin after success")
	}
	s.abort(id3)
	if _, _, ok := s.begin(6); !ok {
		t.Fatal("begin after abort")
	}
}

func TestPromptAssembly_SummaryThenRecentTurns(t *testing.T) {
	p := &promptCaptureProvider{}
	reg := tools.NewRegistry()
	if err := reg.Register(models.Tool{
		Name:        "probe_tool_schema",
		Description: "probe",
		InputSchema: map[string]any{"type": "object"},
		Handler: func(context.Context, models.ToolCall) (models.ToolResult, error) {
			return models.ToolResult{}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	carry := NewSessionCarry()
	a := New(AgentConfig{
		LLMProvider:   p,
		Tools:         reg,
		KeepFullTurns: 2,
		Session:       carry,
		SystemPrompt:  "BASE_SYSTEM",
	})
	a.summary.text = "OLD_SUMMARY"
	a.summary.covered = 3

	var msgs []models.Message
	for i := 1; i <= 5; i++ {
		msgs = append(msgs, models.Message{Role: models.RoleHuman, Content: fmt.Sprintf("turn-%d", i)})
		if i < 5 {
			msgs = append(msgs, models.Message{Role: models.RoleAI, Content: fmt.Sprintf("ack-%d", i)})
		}
	}
	result, err := a.Run(context.Background(), "s1", msgs)
	if err != nil {
		t.Fatal(err)
	}
	got := p.snapshot()
	if len(got) != 1 {
		t.Fatalf("requests = %d, want 1", len(got))
	}
	req := got[0]
	if strings.Contains(req.system, "OLD_SUMMARY") || strings.Contains(req.system, "turn-1") {
		t.Fatalf("session summary leaked into the system prompt: %q", req.system)
	}
	if !strings.Contains(req.system, "BASE_SYSTEM") {
		t.Fatalf("system prompt = %q", req.system)
	}
	if len(req.messages) < 3 {
		t.Fatalf("messages = %#v", req.messages)
	}
	head := req.messages[0]
	if head.Metadata[metaSessionSummary] != "true" {
		t.Fatal("first message is not the session summary")
	}
	if !strings.Contains(head.Content, "OLD_SUMMARY") || !strings.Contains(head.Content, "Session summary") {
		t.Fatalf("summary message = %q", head.Content)
	}
	if strings.Contains(head.Content, "turn-1") || strings.Contains(head.Content, "probe_tool_schema") {
		t.Fatalf("summary inlined a dropped turn or a tool schema: %q", head.Content)
	}
	body := joinContents(req.messages[1:])
	if strings.Contains(body, "turn-1") || strings.Contains(body, "turn-3") {
		t.Fatalf("verbatim window contains a dropped turn: %s", body)
	}
	i4 := strings.Index(body, "turn-4")
	i5 := strings.Index(body, "turn-5")
	if i4 < 0 || i5 < 0 || i4 > i5 {
		t.Fatalf("recent turns out of order: %s", body)
	}
	last := req.messages[len(req.messages)-1].Content
	if !strings.Contains(last, time.Now().Format("2006-01-02")) {
		t.Fatalf("last message is not the current-turn note: %q", last)
	}
	foundTool := false
	for _, name := range req.tools {
		if name == "probe_tool_schema" {
			foundTool = true
		}
	}
	if !foundTool {
		t.Fatalf("tool schemas missing from the request: %v", req.tools)
	}
	for _, m := range result.Messages {
		if strings.Contains(m.Content, "Session summary of user turns") {
			t.Fatal("session summary was written into canonical history")
		}
	}
	if !strings.Contains(joinContents(result.Messages), "turn-1") {
		t.Fatal("canonical history lost the dropped turn")
	}
}

func TestSessionSummary_DoesNotBlockTheTurn(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	sum := &gateSummarizer{started: started, release: release, text: "SUM"}
	p := &promptCaptureProvider{}
	a := New(AgentConfig{
		LLMProvider:   p,
		Tools:         tools.NewRegistry(),
		KeepFullTurns: 1,
		Summarizer:    sum,
		Session:       NewSessionCarry(),
	})
	msgs := []models.Message{
		{Role: models.RoleHuman, Content: "turn-1"},
		{Role: models.RoleAI, Content: "ack-1"},
		{Role: models.RoleHuman, Content: "turn-2"},
		{Role: models.RoleAI, Content: "ack-2"},
		{Role: models.RoleHuman, Content: "turn-3"},
	}
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(context.Background(), "s1", msgs)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("summary job did not start")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run blocked on the summary model")
	}
	first := p.snapshot()[0]
	blob := joinContents(first.messages)
	if strings.Contains(blob, "turn-1") || strings.Contains(blob, "SUM") {
		t.Fatalf("first request saw dropped turns or a summary that had not finished: %s", blob)
	}
	if !strings.Contains(blob, "turn-3") || !strings.Contains(blob, "still being consolidated") {
		t.Fatalf("first request = %s", blob)
	}

	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for {
		text, covered, busy := a.summary.snapshot()
		if text == "SUM" && covered == 2 && !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("summary did not land: text=%q covered=%d busy=%v", text, covered, busy)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A later turn is a new Agent. The summary lives on the shared carry.
	a2 := New(AgentConfig{
		LLMProvider:   p,
		Tools:         tools.NewRegistry(),
		KeepFullTurns: 1,
		Session:       a.session,
	})
	if _, err := a2.Run(context.Background(), "s1", msgs); err != nil {
		t.Fatal(err)
	}
	second := p.snapshot()[1]
	if !strings.Contains(second.messages[0].Content, "SUM") {
		t.Fatalf("later request missed the summary: %q", second.messages[0].Content)
	}
	if strings.Contains(joinContents(second.messages[1:]), "turn-1") {
		t.Fatal("later request inlined a summarized turn")
	}
}

func TestLLMSessionSummarizer_UsesSeparateChat(t *testing.T) {
	p := &chatOnlyProvider{reply: " merged "}
	s := NewLLMSessionSummarizer(p, "cheap-model")
	got, err := s.Summarize(context.Background(), "previous text", "human: turn-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "merged" {
		t.Fatalf("summary = %q", got)
	}
	if p.req.Model != "cheap-model" {
		t.Fatalf("model = %q", p.req.Model)
	}
	if !strings.Contains(p.req.SystemPrompt, "consolidat") {
		t.Fatalf("system prompt = %q", p.req.SystemPrompt)
	}
	body := p.req.Messages[0].Content
	if !strings.Contains(body, "previous text") || !strings.Contains(body, "human: turn-1") {
		t.Fatalf("summarizer input = %q", body)
	}
}

func TestRecallOutput_StaysInPromptAfterOffload(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("RECALLED_BODY_", (maxToolContentBytes/len("RECALLED_BODY_"))+8)
	if len(body) <= maxToolContentBytes || len(body) <= offloadThresholdBytes {
		t.Fatalf("fixture is %d bytes, want above both caps", len(body))
	}
	if err := os.WriteFile(filepath.Join(dir, "call-recall.txt"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry()
	if err := reg.Register(builtin.RecallOutputTool(dir)); err != nil {
		t.Fatal(err)
	}
	p := &recallThenStopProvider{}
	a := New(AgentConfig{
		LLMProvider:  p,
		Tools:        reg,
		OffloadDir:   dir,
		OffloadBytes: 64,
	})
	result, err := a.Run(context.Background(), "s1", []models.Message{
		{Role: models.RoleHuman, Content: "need the full output"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	for _, m := range result.Messages {
		if m.Role == models.RoleTool {
			stored = m.Content
			break
		}
	}
	if stored != body {
		t.Fatalf("stored tool content = %d bytes, want the full %d (offload or the 50KB cap rewrote the recall)", len(stored), len(body))
	}
	if strings.Contains(stored, "[offloaded:") || strings.Contains(stored, "[truncated:") {
		t.Fatal("recalled body was stubbed or truncated")
	}
	follow := p.followUp()
	if !strings.Contains(joinContents(follow), "RECALLED_BODY_") || strings.Contains(joinContents(follow), "[offloaded:") {
		t.Fatal("the next provider request did not contain the recalled body")
	}
	if !strings.Contains(joinContents(follow), body) {
		t.Fatal("the next provider request clipped the recalled body")
	}
}

func TestWindow_KeptTurnsAreNotAged(t *testing.T) {
	big := strings.Repeat("X", 8000)
	var msgs []models.Message
	msgs = append(msgs,
		models.Message{Role: models.RoleHuman, Content: "turn-1"},
		models.Message{Role: models.RoleTool, Content: big, ToolResult: &models.ToolResult{CallID: "c1", ToolName: "bash", Content: big}},
		models.Message{Role: models.RoleHuman, Content: "turn-2"},
		models.Message{Role: models.RoleAI, Content: strings.Repeat("B", 2000)},
		models.Message{Role: models.RoleTool, Content: big, ToolResult: &models.ToolResult{CallID: "c2", ToolName: "bash", Content: big}},
		models.Message{Role: models.RoleHuman, Content: "turn-3"},
	)
	aging := &AgingConfig{Enabled: true, MinContextPressure: 0}

	windowed := &promptCaptureProvider{}
	a := New(AgentConfig{LLMProvider: windowed, KeepFullTurns: 2, Aging: aging})
	if _, err := a.Run(context.Background(), "s1", msgs); err != nil {
		t.Fatal(err)
	}
	got := joinContents(windowed.snapshot()[0].messages)
	if strings.Contains(got, "turn-1") {
		t.Fatal("window kept a dropped turn")
	}
	if strings.Count(got, "X") < len(big) {
		t.Fatal("kept turn's tool result was aged inside the window")
	}
	if strings.Contains(got, "re-call bash") {
		t.Fatal("aging hint landed inside the verbatim window")
	}

	aged := &promptCaptureProvider{}
	full := New(AgentConfig{LLMProvider: aged, Aging: aging})
	if _, err := full.Run(context.Background(), "s1", msgs); err != nil {
		t.Fatal(err)
	}
	fullText := joinContents(aged.snapshot()[0].messages)
	if !strings.Contains(fullText, "re-call bash") {
		t.Fatal("window off: expected aging to still trim the previous turn's tool result")
	}
}

func TestCompactOnOverflow_DoesNotRewriteUnsentHistory(t *testing.T) {
	huge := strings.Repeat("OLDDATA", 4000)
	var msgs []models.Message
	for i := 0; i < 6; i++ {
		msgs = append(msgs,
			models.Message{Role: models.RoleHuman, Content: fmt.Sprintf("old-%d", i)},
			models.Message{Role: models.RoleTool, Content: huge, ToolResult: &models.ToolResult{CallID: fmt.Sprintf("c%d", i), ToolName: "bash", Content: huge}},
		)
	}
	msgs = append(msgs, models.Message{Role: models.RoleHuman, Content: "current-turn"})
	a := New(AgentConfig{KeepFullTurns: 1})
	out, ok := a.compactOnOverflow(msgs, "sys", 1, "test")
	if ok {
		t.Fatal("compaction shrunk history the window does not send")
	}
	if !strings.Contains(joinContents(out), "OLDDATA") {
		t.Fatal("unsent history was rewritten")
	}
}

func TestOffloadStub_RecallRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := New(AgentConfig{OffloadBytes: 64, OffloadDir: dir})
	var body strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&body, "HEADLINE %d\n", i)
	}
	body.WriteString("FAIL_TAIL\n")
	original := body.String()
	result := models.ToolResult{CallID: "call-9", ToolName: "bash", Content: original}
	if !a.offloadIfNeeded(&result, dir) {
		t.Fatal("expected offload")
	}
	if !strings.HasPrefix(result.Content, "[offloaded: full output (") {
		t.Fatalf("stub = %q", result.Content)
	}
	if !strings.Contains(result.Content, "ref: call-9") || !strings.Contains(result.Content, "recall_output") {
		t.Fatalf("stub missing ref: %q", result.Content)
	}
	if !strings.Contains(result.Content, "FAIL_TAIL") {
		t.Fatalf("stub dropped the tail: %q", result.Content)
	}
	if strings.Contains(result.Content, "HEADLINE 0") {
		t.Fatalf("stub inlined the head: %q", result.Content)
	}
	if len(result.Content) >= len(original) {
		t.Fatalf("stub is not smaller: %d >= %d", len(result.Content), len(original))
	}

	tool := builtin.RecallOutputTool(dir)
	got, err := tool.Handler(context.Background(), models.ToolCall{
		ID: "r1", Name: "recall_output", Arguments: map[string]any{"ref": "call-9"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != original {
		t.Fatal("recall did not return the stored body")
	}

	for _, ref := range []string{"../secret", "call-9/../../secret", dir + "/../secret"} {
		if _, err := tool.Handler(context.Background(), models.ToolCall{
			ID: "bad", Name: "recall_output", Arguments: map[string]any{"ref": ref},
		}); err == nil {
			t.Fatalf("ref %q escaped the offload directory", ref)
		}
	}

	disabled := New(AgentConfig{OffloadBytes: -1})
	big := models.ToolResult{CallID: "big", ToolName: "bash", Content: strings.Repeat("x", offloadThresholdBytes+32)}
	if disabled.offloadIfNeeded(&big, dir) {
		t.Fatal("negative offload threshold still offloaded")
	}
}

type promptCapture struct {
	system   string
	tools    []string
	messages []models.Message
}

type promptCaptureProvider struct {
	mu   sync.Mutex
	reqs []promptCapture
}

func (p *promptCaptureProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *promptCaptureProvider) Stream(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	snap := promptCapture{system: req.SystemPrompt, messages: append([]models.Message(nil), req.Messages...)}
	for _, tool := range req.Tools {
		snap.tools = append(snap.tools, tool.Name)
	}
	p.mu.Lock()
	p.reqs = append(p.reqs, snap)
	p.mu.Unlock()
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, Content: "done"}, Done: true}
	}()
	return ch, nil
}

func (p *promptCaptureProvider) snapshot() []promptCapture {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]promptCapture, len(p.reqs))
	copy(out, p.reqs)
	return out
}

type recallThenStopProvider struct {
	n    int
	next []models.Message
}

func (p *recallThenStopProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *recallThenStopProvider) Stream(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.n++
	snap := append([]models.Message(nil), req.Messages...)
	callN := p.n
	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		if callN == 1 {
			ch <- llm.StreamChunk{
				ToolCalls: []models.ToolCall{{
					ID:        "call-recall",
					Name:      "recall_output",
					Arguments: map[string]any{"ref": "call-recall"},
				}},
				Stop: "tool_calls",
				Done: true,
			}
			return
		}
		p.next = snap
		ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, Content: "done"}, Done: true}
	}()
	return ch, nil
}

func (p *recallThenStopProvider) followUp() []models.Message {
	return p.next
}

type gateSummarizer struct {
	started chan struct{}
	release chan struct{}
	text    string
	once    sync.Once
}

func (g *gateSummarizer) Summarize(ctx context.Context, _, _ string) (string, error) {
	g.once.Do(func() { close(g.started) })
	select {
	case <-g.release:
		return g.text, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

type chatOnlyProvider struct {
	req   llm.ChatRequest
	reply string
}

func (p *chatOnlyProvider) Chat(_ context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	p.req = req
	return llm.ChatResponse{Message: models.Message{Content: p.reply}}, nil
}

func (p *chatOnlyProvider) Stream(context.Context, llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	return nil, fmt.Errorf("stream should not be used for the session summary")
}

func TestRenderSummaryTranscript_KeepsHumanHeadAndToolTail(t *testing.T) {
	goal := "GOAL: ship the migration before Friday"
	tail := "ENDCONSTRAINT: do not touch pkg/legacy"
	middle := "HUMAN_MIDDLE_TOKEN"
	human := goal + "\n" + strings.Repeat("a", 4000) + middle + strings.Repeat("b", 8000) + "\n" + tail
	toolMiddle := "TOOL_MIDDLE_TOKEN"
	tool := "RAN: go test ./pkg/agent\n" + strings.Repeat("log ", 400) + toolMiddle + strings.Repeat(" log", 2000) + "\nFAIL_TAIL"
	out := renderSummaryTranscript([]models.Message{
		{Role: models.RoleHuman, Content: human},
		{Role: models.RoleTool, Content: tool},
	})
	if len(out) > summaryTranscriptCap {
		t.Fatalf("transcript is %d bytes, cap is %d", len(out), summaryTranscriptCap)
	}
	for _, want := range []string{goal, tail, "RAN: go test ./pkg/agent", "FAIL_TAIL"} {
		if !strings.Contains(out, want) {
			t.Fatalf("summarizer input missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, middle) || strings.Contains(out, toolMiddle) {
		t.Fatal("summarizer input kept the middle of a long message")
	}
}

func TestRenderSummaryTranscript_BudgetKeepsEarlyGoals(t *testing.T) {
	const n = 40
	msgs := make([]models.Message, 0, n)
	for i := 0; i < n; i++ {
		goal := fmt.Sprintf("GOAL-%02d: keep this opening", i)
		body := goal + "\n" + strings.Repeat("x", 6000) + "LATE_MIDDLE_TOKEN" + strings.Repeat("y", 6000)
		msgs = append(msgs, models.Message{Role: models.RoleHuman, Content: body})
		msgs = append(msgs, models.Message{Role: models.RoleTool, Content: strings.Repeat("dump ", 3000) + "OLD_TOOL_TAIL"})
	}
	out := renderSummaryTranscript(msgs)
	if len(out) > summaryTranscriptCap {
		t.Fatalf("transcript is %d bytes, cap is %d", len(out), summaryTranscriptCap)
	}
	if !strings.Contains(out, "GOAL-00: keep this opening") {
		t.Fatal("the first dropped turn's goal was cut from the summarizer input")
	}
	if !strings.Contains(out, fmt.Sprintf("GOAL-%02d: keep this opening", n-1)) {
		t.Fatal("the last dropped turn's goal was cut while shrinking heads")
	}
	if strings.Contains(out, "LATE_MIDDLE_TOKEN") {
		t.Fatal("budget shrink kept the middle of a user turn")
	}
}

func TestSessionSummary_EchoesEarlyGoal(t *testing.T) {
	goal := "GOAL: ship the migration before Friday"
	human := goal + "\n" + strings.Repeat("a", 5000) + "HUMAN_MIDDLE_TOKEN" + strings.Repeat("b", 8000) + "\nENDCONSTRAINT: do not touch pkg/legacy"
	echo := &echoSummarizer{}
	a := New(AgentConfig{
		LLMProvider:   &promptCaptureProvider{},
		Tools:         tools.NewRegistry(),
		KeepFullTurns: 1,
		Summarizer:    echo,
		Session:       NewSessionCarry(),
	})
	msgs := []models.Message{
		{Role: models.RoleHuman, Content: human},
		{Role: models.RoleTool, Content: "RAN: go test ./pkg/agent\n" + strings.Repeat("log ", 800) + "\nFAIL_TAIL"},
		{Role: models.RoleHuman, Content: "current turn"},
	}
	if _, err := a.Run(context.Background(), "s1", msgs); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var text string
	for {
		var busy bool
		text, _, busy = a.summary.snapshot()
		if strings.Contains(text, goal) && !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("published summary = %q", text)
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := echo.transcript()
	if !strings.Contains(got, goal) || !strings.Contains(got, "FAIL_TAIL") || !strings.Contains(got, "ENDCONSTRAINT") {
		t.Fatalf("summarizer input = %s", got)
	}
	if strings.Contains(got, "HUMAN_MIDDLE_TOKEN") {
		t.Fatal("summarizer input included the middle of the dropped turn")
	}
	if !strings.Contains(text, goal) || !strings.Contains(text, "FAIL_TAIL") {
		t.Fatalf("echoed summary dropped the goal or the failure tail: %s", text)
	}
}

func TestSessionSummary_PanicAbortsFlight(t *testing.T) {
	a := New(AgentConfig{
		LLMProvider:   &promptCaptureProvider{},
		Tools:         tools.NewRegistry(),
		KeepFullTurns: 1,
		Summarizer:    panicSummarizer{},
		Session:       NewSessionCarry(),
	})
	msgs := []models.Message{
		{Role: models.RoleHuman, Content: "turn-1"},
		{Role: models.RoleHuman, Content: "turn-2"},
	}
	if _, err := a.Run(context.Background(), "s1", msgs); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, _, busy := a.summary.snapshot()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("summary flight stayed set after the background job panicked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	id, _, ok := a.summary.begin(1)
	if !ok {
		t.Fatal("a later summary could not start")
	}
	a.summary.abort(id)
}

type echoSummarizer struct {
	mu  sync.Mutex
	got string
}

func (e *echoSummarizer) Summarize(_ context.Context, _, transcript string) (string, error) {
	e.mu.Lock()
	e.got = transcript
	e.mu.Unlock()
	return transcript, nil
}

func (e *echoSummarizer) transcript() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.got
}

type panicSummarizer struct{}

func (panicSummarizer) Summarize(context.Context, string, string) (string, error) {
	panic("summary boom")
}

func joinContents(msgs []models.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}
