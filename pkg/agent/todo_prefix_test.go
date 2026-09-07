package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
)

// --- Review fix #3: a permanent test for the M4-2 prefix-caching guarantee
// M5's todo tool depends on -----------------------------------------------
//
// TestTodoInjection_PresentAfterWriteNotInSystemPrompt (todo_test.go) only
// asserts BuildSystemPrompt() once, AFTER the whole Run finishes — it never
// looks at what actually went out on the WIRE, and never checks that the
// system prompt stays byte-identical ACROSS requests (only that it never
// contains the todo marker). The actual property M4-2 bought (and that this
// feature depends on to not defeat it) is: every request in a session shares
// one byte-identical system prompt, and the message history sent on request
// N+1 is a byte-identical, monotonically growing extension of what was sent
// on request N — so an OpenAI-compat provider's automatic prefix cache
// (DeepSeek/Qwen/GLM) reuses everything up to the trailing injection on
// every single request, across Runs, even as todo_write repeatedly changes
// the injection's own byte length.
//
// prefixCaptureRequest is one Stream call's captured (SystemPrompt,
// Messages) pair, copied out so later mutation of the live slice (there is
// none, but see prefixBatchProvider.Stream's own comment) can't retroactively
// change what was recorded.
type prefixCaptureRequest struct {
	systemPrompt string
	messages     []models.Message
}

// prefixRequestLog accumulates captured requests across MULTIPLE Agent
// instances (one per Run, mirroring the REPL's real "Agent is single-use"
// design — see SessionCarry's doc comment) sharing one *SessionCarry, so the
// assertions below can inspect the request sequence across Run boundaries,
// not just within a single Run.
type prefixRequestLog struct {
	mu   sync.Mutex
	reqs []prefixCaptureRequest
}

func (l *prefixRequestLog) record(req llm.ChatRequest) {
	msgs := make([]models.Message, len(req.Messages))
	copy(msgs, req.Messages)
	l.mu.Lock()
	l.reqs = append(l.reqs, prefixCaptureRequest{systemPrompt: req.SystemPrompt, messages: msgs})
	l.mu.Unlock()
}

func (l *prefixRequestLog) snapshot() []prefixCaptureRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]prefixCaptureRequest, len(l.reqs))
	copy(out, l.reqs)
	return out
}

// prefixBatchProvider drives one Run's worth of scripted tool-call turns
// (one []models.ToolCall batch per turn, so a turn can carry several calls)
// then ends that Run with plain text, recording every Stream request into a
// SHARED log so a test can drive several Runs (several provider instances,
// one SessionCarry) and inspect the combined request sequence.
type prefixBatchProvider struct {
	log     *prefixRequestLog
	batches [][]models.ToolCall
	turn    int
}

func (p *prefixBatchProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *prefixBatchProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.log.record(req)
	turn := p.turn
	p.turn++

	ch := make(chan llm.StreamChunk, 1)
	go func() {
		defer close(ch)
		if turn < len(p.batches) {
			ch <- llm.StreamChunk{ToolCalls: p.batches[turn], Stop: "tool_calls", Done: true}
			return
		}
		ch <- llm.StreamChunk{Message: &models.Message{Role: models.RoleAI, Content: "done"}, Done: true}
	}()
	return ch, nil
}

// messageBytes renders one message the same way on every call, so two
// captures of the SAME underlying message compare byte-equal and two
// captures of DIFFERENT messages compare byte-different — a direct proxy for
// "would this serialize identically on the wire".
func messageBytes(t *testing.T, m models.Message) string {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	return string(raw)
}

// TestTodoInjection_PrefixStableAcrossRunsAndWrites is the permanent
// regression test for review fix #3. It drives TWO Runs sharing one
// SessionCarry, with a todo_write call in each — the second with a list
// whose rendered byte length differs from the first (shorter -> longer),
// which is exactly the case that would break a naive "diff the system
// prompt" check but not a byte-accounting one — and asserts, over every
// captured request in order:
//
//  1. every request's SystemPrompt is byte-identical to every other's;
//  2. every request's message PREFIX (all messages except the trailing
//     injection) is a byte-identical, strictly non-shrinking extension of
//     the previous request's prefix — never edited, never reordered, only
//     grown;
//  3. the todo note header (todoNoteHeader) appears ONLY in the trailing
//     injection message of a request, never in any earlier message.
func TestTodoInjection_PrefixStableAcrossRunsAndWrites(t *testing.T) {
	const shortItem = "SHORT_ITEM"
	const longItem = "MUCH_LONGER_REPLACEMENT_PLAN_ITEM_WITH_MORE_TEXT_IN_IT"

	log := &prefixRequestLog{}
	session := NewSessionCarry()

	// Run 1: one todo_write (short), then a plain text reply.
	p1 := &prefixBatchProvider{log: log, batches: [][]models.ToolCall{
		{todoWriteCall("c1", shortItem, "in_progress")},
	}}
	a1 := New(AgentConfig{LLMProvider: p1, Tools: newTodoRegistry(t), Model: "m", Session: session})
	res1, err := a1.Run(context.Background(), "sess-prefix-stable", []models.Message{
		{Role: models.RoleHuman, Content: "please plan this"},
	})
	if err != nil {
		t.Fatalf("Run 1: %v", err)
	}

	// Run 2 (same session): mirrors the REPL's real call pattern
	// (pkg/chat/repl.go passes r.sess.Messages, the FULL accumulated
	// history, into every Run) — feed Run 1's own canonical output plus one
	// new human turn back in, then have Run 2 REPLACE the plan with a
	// differently-sized list.
	messages2 := append(append([]models.Message{}, res1.Messages...), models.Message{
		Role: models.RoleHuman, Content: "now update the plan",
	})
	p2 := &prefixBatchProvider{log: log, batches: [][]models.ToolCall{
		{todoWriteCall("c2", longItem, "in_progress")},
	}}
	a2 := New(AgentConfig{LLMProvider: p2, Tools: newTodoRegistry(t), Model: "m", Session: session})
	if _, err := a2.Run(context.Background(), "sess-prefix-stable", messages2); err != nil {
		t.Fatalf("Run 2: %v", err)
	}

	requests := log.snapshot()
	if len(requests) != 4 {
		t.Fatalf("expected 4 requests (2 per Run: todo_write turn + final), got %d", len(requests))
	}

	// (1) System prompt byte-identical across every request.
	for i := 1; i < len(requests); i++ {
		if requests[i].systemPrompt != requests[0].systemPrompt {
			t.Fatalf("request %d system prompt differs from request 0's — prefix cache would be invalidated:\n0: %q\n%d: %q",
				i, requests[0].systemPrompt, i, requests[i].systemPrompt)
		}
	}

	// (2) Each request's prefix (all but the trailing injection) is a
	// byte-identical, strictly non-shrinking extension of the previous
	// request's prefix.
	var prevPrefix []string
	for i, r := range requests {
		if len(r.messages) == 0 {
			t.Fatalf("request %d has no messages at all", i)
		}
		prefix := make([]string, len(r.messages)-1) // exclude the trailing injection
		for j, m := range r.messages[:len(r.messages)-1] {
			prefix[j] = messageBytes(t, m)
		}
		if i > 0 {
			if len(prefix) < len(prevPrefix) {
				t.Fatalf("request %d prefix (%d messages) is SHORTER than request %d's (%d messages) — history was not monotonically growing",
					i, len(prefix), i-1, len(prevPrefix))
			}
			for j, want := range prevPrefix {
				if prefix[j] != want {
					t.Fatalf("request %d prefix message %d differs from request %d's — the shared prefix was mutated, not just extended:\nold: %s\nnew: %s",
						i, j, i-1, want, prefix[j])
				}
			}
		}
		prevPrefix = prefix
	}

	// (3) The todo note header appears ONLY in the trailing injection
	// message, never in any earlier message of any request.
	for i, r := range requests {
		last := len(r.messages) - 1
		for j, m := range r.messages {
			if j == last {
				continue
			}
			if strings.Contains(m.Content, todoNoteHeader) {
				t.Fatalf("request %d message %d (non-trailing) contains the todo note header — it must only ride the trailing injection:\n%s",
					i, j, m.Content)
			}
		}
	}

	// Sanity: the feature actually worked (short item in Run 1's post-write
	// requests, long item replacing it in Run 2's), so a change that broke
	// the injection entirely wouldn't pass this test vacuously.
	afterRun1Write := requests[1].messages[len(requests[1].messages)-1].Content
	if !strings.Contains(afterRun1Write, shortItem) {
		t.Fatalf("request 1 (after Run 1's write) missing %q: %q", shortItem, afterRun1Write)
	}
	afterRun2Write := requests[3].messages[len(requests[3].messages)-1].Content
	if !strings.Contains(afterRun2Write, longItem) || strings.Contains(afterRun2Write, shortItem) {
		t.Fatalf("request 3 (after Run 2's write) should contain %q and not the replaced %q: %q", longItem, shortItem, afterRun2Write)
	}
}
