package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
)

// DefaultKeepFullTurns is the number of recent user turns sent verbatim.
// Older turns are left out of the provider request and consolidated by a
// background model into one session summary. The current turn counts as one
// of these ten; tool schemas travel on the request's tools field, not as
// another copy of the dialogue. Zero on AgentConfig disables the window
// (tests and one-shot callers). The REPL resolves an absent config value to
// this default.
const DefaultKeepFullTurns = 10

// metaSessionSummary marks the synthetic message that carries the session
// summary. It must not open a user turn: counting it would age or re-window
// the real dialogue around a message the model did not write.
const metaSessionSummary = "session_summary"

const sessionSummaryTimeout = 60 * time.Second

const sessionSummarySystemPrompt = `You consolidate older turns of a coding session into one session summary for the main model.
The summary replaces those turns in its prompt. Keep only what the next turns need:
- the user's goals and constraints
- files read or changed, with paths
- decisions and why
- unresolved errors, including the error text
- attempts that failed
Drop file dumps, repeated tool output, and chatter.
When a previous summary is included, merge the new turns into it. Do not drop an unresolved item unless a later turn resolved it.
Return only the summary text.`

// SessionSummarizer turns older dialogue into the session summary. The
// implementation is a separate model call and must not run on the
// interactive turn's critical path.
type SessionSummarizer interface {
	Summarize(ctx context.Context, previousSummary, transcript string) (string, error)
}

// sessionSummary is the consolidated text of user turns that have fallen
// out of the verbatim window. It is shared across the single-use Agents of
// one conversation (see SessionCarry) and is written from a background
// goroutine, so unlike the rest of the carry it has its own lock.
type sessionSummary struct {
	mu      sync.Mutex
	text    string
	covered int // user turns [0, covered) included in text
	flight  uint64
	nextID  uint64
}

func (s *sessionSummary) snapshot() (text string, covered int, busy bool) {
	if s == nil {
		return "", 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text, s.covered, s.flight != 0
}

// begin starts a job that will cover user turns [0, boundary). It fails
// when a job is already running or the boundary is already covered, so two
// turns cannot publish over each other.
func (s *sessionSummary) begin(boundary int) (id uint64, previous string, ok bool) {
	if s == nil || boundary <= 0 {
		return 0, "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.flight != 0 || boundary <= s.covered {
		return 0, "", false
	}
	s.nextID++
	s.flight = s.nextID
	return s.flight, s.text, true
}

// complete publishes a job's summary. A stale id (the job was aborted, or a
// newer job holds the flight) is ignored so a slow model cannot overwrite a
// newer consolidation.
func (s *sessionSummary) complete(id uint64, text string, coveredTo int) bool {
	if s == nil || id == 0 {
		return false
	}
	text = strings.TrimSpace(text)
	if text == "" || coveredTo <= 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.flight != id || coveredTo < s.covered {
		return false
	}
	s.flight = 0
	s.text = text
	s.covered = coveredTo
	return true
}

func (s *sessionSummary) abort(id uint64) {
	if s == nil || id == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.flight == id {
		s.flight = 0
	}
}

// turnStarts lists the index of each message that opens a user turn.
func turnStarts(msgs []models.Message) []int {
	var starts []int
	for i, m := range msgs {
		if opensUserTurn(m) {
			starts = append(starts, i)
		}
	}
	return starts
}

// splitKept returns how many leading user turns fall outside the verbatim
// window and the messages that stay. keep <= 0 or a short transcript returns
// the messages unchanged and a zero boundary.
func splitKept(msgs []models.Message, keep int) (boundary int, kept []models.Message) {
	if keep <= 0 {
		return 0, msgs
	}
	starts := turnStarts(msgs)
	if len(starts) <= keep {
		return 0, msgs
	}
	boundary = len(starts) - keep
	return boundary, msgs[starts[boundary]:]
}

func messagesForTurns(msgs []models.Message, from, to int) []models.Message {
	starts := turnStarts(msgs)
	if from < 0 {
		from = 0
	}
	if to > len(starts) {
		to = len(starts)
	}
	if from >= to || len(starts) == 0 {
		return nil
	}
	end := len(msgs)
	if to < len(starts) {
		end = starts[to]
	}
	out := make([]models.Message, end-starts[from])
	copy(out, msgs[starts[from]:end])
	return out
}

// sessionSummaryMessage is RoleHuman on purpose. Both provider mappers drop
// a RoleSystem message that sits inside the message list; only
// ChatRequest.SystemPrompt becomes the system message. opensUserTurn ignores
// this message so it is not itself a user turn.
func sessionSummaryMessage(text string, covered, boundary int, summaryEnabled bool) models.Message {
	var b strings.Builder
	if covered > 0 {
		fmt.Fprintf(&b, "[Session summary of user turns 1–%d. Later turns follow in full. This is a consolidation of the dialogue, not a new user request.]\n", covered)
		if strings.TrimSpace(text) != "" {
			b.WriteString(strings.TrimSpace(text))
			b.WriteString("\n")
		}
	}
	if covered < boundary {
		if summaryEnabled {
			fmt.Fprintf(&b, "[User turns %d–%d are still being consolidated in the background and are not in this prompt.]\n", covered+1, boundary)
		} else {
			fmt.Fprintf(&b, "[User turns 1–%d are omitted from this prompt. Session summary is off.]\n", boundary)
		}
	}
	return models.Message{
		Role:     models.RoleHuman,
		Content:  b.String(),
		Metadata: map[string]string{metaSessionSummary: "true"},
	}
}

// promptViewForRequest is the transcript before the session window and the
// turn injection. Aging stays on only when the window is off. Inside the
// window the kept turns are sent verbatim, which is what the keep-window
// setting means; turns outside it are replaced by the session summary
// rather than by an aged stub.
func (a *Agent) promptViewForRequest(messages []models.Message) []models.Message {
	if a != nil && a.keepFullTurns > 0 {
		return messages
	}
	var cfg *AgingConfig
	window := 0
	if a != nil {
		cfg = a.aging
		window = a.contextWindow
	}
	return buildPromptView(messages, cfg, window)
}

// providerTranscriptView is the message list a request sends, minus the
// trailing turn injection. compactOnOverflow uses it so a compaction is
// accepted only when it shrinks that list.
func (a *Agent) providerTranscriptView(messages []models.Message) []models.Message {
	view := a.promptViewForRequest(messages)
	if a != nil && a.keepFullTurns > 0 {
		view = a.windowPromptView(view)
	}
	return view
}

// windowPromptView is the provider view of a transcript: one session-summary
// message, then the last keepFullTurns user turns in full. Canonical
// messages are not modified. keepFullTurns <= 0 returns view unchanged.
func (a *Agent) windowPromptView(view []models.Message) []models.Message {
	if a == nil || a.keepFullTurns <= 0 {
		return view
	}
	boundary, kept := splitKept(view, a.keepFullTurns)
	if boundary <= 0 {
		return view
	}
	text, covered, _ := a.summary.snapshot()
	if covered > boundary {
		covered = boundary
	}
	head := sessionSummaryMessage(text, covered, boundary, a.summarizer != nil)
	out := make([]models.Message, 0, 1+len(kept))
	out = append(out, head)
	out = append(out, kept...)
	return out
}

// maybeStartSessionSummary kicks a background consolidation of the user
// turns that no longer fit in the verbatim window. It returns immediately.
// The job reads a rendered transcript, not the live message slice, and
// publishes only if it is still the in-flight job when it finishes.
func (a *Agent) maybeStartSessionSummary(ctx context.Context, msgs []models.Message) {
	if a == nil || a.keepFullTurns <= 0 || a.summarizer == nil || a.summary == nil {
		return
	}
	boundary, _ := splitKept(msgs, a.keepFullTurns)
	if boundary <= 0 {
		return
	}
	id, previous, ok := a.summary.begin(boundary)
	if !ok {
		return
	}
	// begin does not change covered. Re-read it so the span starts where the
	// published summary ends. Nothing else can complete while this id holds
	// the flight.
	_, covered, _ := a.summary.snapshot()
	span := messagesForTurns(msgs, covered, boundary)
	transcript := renderSummaryTranscript(span)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				if a.logger != nil {
					a.logger.Error("session summary panicked", "err", fmt.Sprint(rec))
				}
				a.summary.abort(id)
			}
		}()
		jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionSummaryTimeout)
		defer cancel()
		text, err := a.summarizer.Summarize(jobCtx, previous, transcript)
		if err != nil {
			a.logger.Warn("session summary failed", "err", err)
			a.summary.abort(id)
			return
		}
		if !a.summary.complete(id, text, boundary) {
			a.summary.abort(id)
		}
	}()
}

// Summary-input budgets. The cheap model does not see canonical history
// verbatim. User turns keep a long head, because goals and constraints are
// stated there, plus a shorter tail. Tool and assistant messages keep a
// shorter head and a longer tail, because failures and exit codes land at
// the end. The whole transcript stays within summaryTranscriptCap. Under
// pressure the oldest tool bodies go first, then user-turn tails. User-turn
// heads are kept, and if they still do not fit they are shortened together
// rather than dropping the earliest turns.
const (
	summaryTranscriptCap = 48 * 1024
	summaryHumanHead     = 2400
	summaryHumanTail     = 800
	summaryOtherHead     = 700
	summaryOtherTail     = 1500
)

func renderSummaryTranscript(msgs []models.Message) string {
	items := make([]summaryItem, 0, len(msgs))
	for _, m := range msgs {
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		items = append(items, summaryItem{role: m.Role, text: content})
	}
	return fitSummaryItems(items, summaryTranscriptCap)
}

type summaryItem struct {
	role models.Role
	text string
}

func fitSummaryItems(items []summaryItem, cap int) string {
	if cap <= 0 || len(items) == 0 {
		return ""
	}
	heads := make([]int, len(items))
	tails := make([]int, len(items))
	for i, it := range items {
		if it.role == models.RoleHuman {
			heads[i] = summaryHumanHead
			tails[i] = summaryHumanTail
		} else {
			heads[i] = summaryOtherHead
			tails[i] = summaryOtherTail
		}
	}
	for {
		out := renderSummaryItems(items, heads, tails)
		if len(out) <= cap {
			return out
		}
		if shrinkOldestNonHuman(items, heads, tails) {
			continue
		}
		if zeroOldestHumanTail(items, tails) {
			continue
		}
		fair := fairHumanHead(countHumans(items), cap)
		if fair <= 0 {
			return clipPrefix(out, cap)
		}
		shrunk := false
		for i, it := range items {
			if it.role == models.RoleHuman && heads[i] > fair {
				heads[i] = fair
				shrunk = true
			}
		}
		if !shrunk {
			return clipPrefix(out, cap)
		}
	}
}

func renderSummaryItems(items []summaryItem, heads, tails []int) string {
	var b strings.Builder
	for i, it := range items {
		body := clipHeadTail(it.text, heads[i], tails[i])
		if body == "" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", it.role, body)
	}
	return b.String()
}

func shrinkOldestNonHuman(items []summaryItem, heads, tails []int) bool {
	for i, it := range items {
		if it.role == models.RoleHuman {
			continue
		}
		if heads[i] > 0 {
			heads[i] = 0
			return true
		}
		if tails[i] > 0 {
			tails[i] = 0
			return true
		}
	}
	return false
}

func zeroOldestHumanTail(items []summaryItem, tails []int) bool {
	for i, it := range items {
		if it.role == models.RoleHuman && tails[i] > 0 {
			tails[i] = 0
			return true
		}
	}
	return false
}

func countHumans(items []summaryItem) int {
	n := 0
	for _, it := range items {
		if it.role == models.RoleHuman {
			n++
		}
	}
	return n
}

// fairHumanHead is the per-turn head budget that lets every user turn keep
// its beginning inside cap, including the role label and the omission marker.
func fairHumanHead(n, cap int) int {
	if n <= 0 || cap <= 0 {
		return 0
	}
	overhead := len("human: \n[…omitted…]\n")
	b := (cap / n) - overhead
	if b < 0 {
		return 0
	}
	return b
}

func clipHeadTail(s string, headN, tailN int) string {
	if headN < 0 {
		headN = 0
	}
	if tailN < 0 {
		tailN = 0
	}
	if s == "" || (headN == 0 && tailN == 0) {
		return ""
	}
	if len(s) <= headN+tailN {
		return s
	}
	head := clipPrefix(s, headN)
	tail := clipSuffix(s, tailN)
	switch {
	case head == "":
		return "[…omitted…]\n" + tail
	case tail == "":
		return head + "\n[…omitted…]"
	default:
		return head + "\n[…omitted…]\n" + tail
	}
}

func clipPrefix(s string, n int) string {
	if n <= 0 || len(s) <= n {
		if n <= 0 {
			return ""
		}
		return s
	}
	return truncateRuneSafe(s, n)
}

func clipSuffix(s string, n int) string {
	if n <= 0 || len(s) <= n {
		if n <= 0 {
			return ""
		}
		return s
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

type llmSessionSummarizer struct {
	provider llm.LLMProvider
	model    string
}

// NewLLMSessionSummarizer returns a summarizer that calls provider.Chat.
// model is the provider's model name, not the REPL alias.
func NewLLMSessionSummarizer(provider llm.LLMProvider, model string) SessionSummarizer {
	if provider == nil {
		return nil
	}
	return llmSessionSummarizer{provider: provider, model: model}
}

func (s llmSessionSummarizer) Summarize(ctx context.Context, previousSummary, transcript string) (string, error) {
	if s.provider == nil {
		return "", fmt.Errorf("session summarizer has no provider")
	}
	var b strings.Builder
	if strings.TrimSpace(previousSummary) != "" {
		b.WriteString("Previous summary:\n")
		b.WriteString(strings.TrimSpace(previousSummary))
		b.WriteString("\n\n")
	}
	b.WriteString("Turns to consolidate:\n")
	if strings.TrimSpace(transcript) == "" {
		b.WriteString("(empty)\n")
	} else {
		b.WriteString(transcript)
	}
	maxTokens := 2048
	resp, err := s.provider.Chat(ctx, llm.ChatRequest{
		Model:        s.model,
		SystemPrompt: sessionSummarySystemPrompt,
		Messages: []models.Message{{
			Role:    models.RoleHuman,
			Content: b.String(),
		}},
		MaxTokens: &maxTokens,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Message.Content), nil
}

// ResolveKeepFullTurns maps a config value to the verbatim-window size.
// 0 means the default. Negative disables the window (the provider sees the
// full transcript).
func ResolveKeepFullTurns(v int) int {
	if v < 0 {
		return 0
	}
	if v == 0 {
		return DefaultKeepFullTurns
	}
	return v
}
