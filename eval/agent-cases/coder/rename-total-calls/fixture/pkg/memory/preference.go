package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
)

// PreferenceExtractor extracts user preferences from conversation history.
// It implements the Extractor interface and produces facts with category "preference".
type PreferenceExtractor struct {
	provider llm.LLMProvider
	model    string
}

// NewPreferenceExtractor creates a preference extractor using the given LLM provider.
func NewPreferenceExtractor(provider llm.LLMProvider, model string) *PreferenceExtractor {
	return &PreferenceExtractor{
		provider: provider,
		model:    strings.TrimSpace(model),
	}
}

// preferenceThrottle controls how often preference extraction runs.
type preferenceThrottle struct {
	mu            sync.Mutex
	turnCount     int            // turns since last extraction
	lastLang      string         // last detected user language
	langSwitch    bool           // language shift detected this turn
	toolShift     bool           // tool distribution shift detected this turn
	toolDistrib   map[string]int // tool name → cumulative call count
	consecNeg     int            // consecutive negative feedback count
	toolShiftCool int            // cooldown turns remaining after a tool shift trigger
}

const (
	preferenceIntervalFloor = 10 // extract at least every N turns
	toolShiftCooldownTurns  = 5  // cooldown after tool shift trigger before another can fire
)

// ExtractUpdate extracts user preferences from the conversation.
// It produces an Update containing only preference-category facts.
func (e *PreferenceExtractor) ExtractUpdate(ctx context.Context, current Document, messages []models.Message) (Update, error) {
	if e == nil || e.provider == nil {
		return Update{}, fmt.Errorf("preference extractor: provider not configured")
	}
	if len(messages) == 0 {
		return Update{}, nil
	}

	existingPrefs := filterPreferenceFacts(current.Facts)
	resp, err := e.provider.Chat(ctx, llm.ChatRequest{
		Model:           e.model,
		SystemPrompt:    preferenceSystemPrompt,
		ReasoningEffort: "disabled",
		Messages: []models.Message{
			{
				ID:      "pref-extract",
				Role:    models.RoleHuman,
				Content: buildPreferencePrompt(messages, existingPrefs),
			},
		},
	})
	if err != nil {
		return Update{}, fmt.Errorf("preference extraction failed: %w", err)
	}

	var update Update
	if err := json.Unmarshal([]byte(extractJSON(resp.Message.Content)), &update); err != nil {
		return Update{}, fmt.Errorf("decode preference response: %w", err)
	}

	// Tag all facts as preference category.
	for i := range update.Facts {
		if update.Facts[i].Category == "" {
			update.Facts[i].Category = "preference"
		}
	}
	return update, nil
}

// preferenceSystemPrompt is the system prompt for preference extraction.
const preferenceSystemPrompt = `You analyze conversation history to extract user preferences, habits, and working style.

Rules:
- Only extract stable preferences (coding style, tool preferences, communication style, workflow patterns).
- Do NOT extract one-off requests or task-specific information.
- If a preference already exists in the provided existing preferences, update its confidence instead of creating a new fact.
- Use category "preference" for all facts.
- Each fact should be concise (under 200 characters).
- Set confidence between 0.5 (guessed) and 1.0 (explicitly stated).
- Return empty facts array if no clear preferences are detected.

Respond with a JSON object:
{"facts": [{"id": "pref-<short-name>", "content": "<preference description>", "category": "preference", "confidence": 0.8}]}`

// buildPreferencePrompt builds the user prompt for preference extraction.
func buildPreferencePrompt(messages []models.Message, existingPrefs []Fact) string {
	var sb strings.Builder

	sb.WriteString("## Recent Conversation\n\n")
	// Include last 20 messages for context.
	start := 0
	if len(messages) > 20 {
		start = len(messages) - 20
	}
	for _, msg := range messages[start:] {
		role := "User"
		if msg.Role == models.RoleAI {
			role = "AI"
		}
		fmt.Fprintf(&sb, "%s: %s\n\n", role, msg.Content)
	}

	if len(existingPrefs) > 0 {
		sb.WriteString("\n## Existing Preferences\n\n")
		for _, f := range existingPrefs {
			fmt.Fprintf(&sb, "- [%s] (confidence: %.1f)\n", f.Content, f.Confidence)
		}
	}

	sb.WriteString("\nExtract any user preferences visible in the conversation above.")
	return sb.String()
}

// filterPreferenceFacts returns only facts with category "preference".
func filterPreferenceFacts(facts []Fact) []Fact {
	var prefs []Fact
	for _, f := range facts {
		if f.Category == "preference" {
			prefs = append(prefs, f)
		}
	}
	return prefs
}

// PreferenceScheduler manages throttling and event-triggered preference extraction.
type PreferenceScheduler struct {
	throttle preferenceThrottle
}

// NewPreferenceScheduler creates a new preference scheduler.
func NewPreferenceScheduler() *PreferenceScheduler {
	return &PreferenceScheduler{
		throttle: preferenceThrottle{
			toolDistrib: make(map[string]int),
		},
	}
}

// RecordTurn increments the turn counter and returns true if preference
// extraction should run this turn (floor interval or event-triggered).
func (ps *PreferenceScheduler) RecordTurn() bool {
	ps.throttle.mu.Lock()
	defer ps.throttle.mu.Unlock()

	ps.throttle.turnCount++

	// Decrement tool shift cooldown.
	if ps.throttle.toolShiftCool > 0 {
		ps.throttle.toolShiftCool--
	}

	// Floor: every N turns.
	if ps.throttle.turnCount >= preferenceIntervalFloor {
		ps.throttle.turnCount = 0
		return true
	}

	// Event: consecutive negative feedback.
	if ps.throttle.consecNeg >= 3 {
		ps.throttle.consecNeg = 0
		return true
	}

	// Event: language switch detected in this turn.
	if ps.throttle.langSwitch {
		ps.throttle.langSwitch = false
		return true
	}

	// Event: tool distribution shift detected in this turn (only if not in cooldown).
	if ps.throttle.toolShift && ps.throttle.toolShiftCool == 0 {
		ps.throttle.toolShift = false
		ps.throttle.toolShiftCool = toolShiftCooldownTurns
		return true
	}
	// Clear the flag even if in cooldown so it doesn't carry over.
	ps.throttle.toolShift = false
	return false
}

// CheckLanguageSwitch detects if the user's language changed from the previous message.
// If so, it flags a language switch event that will trigger extraction on the next RecordTurn.
func (ps *PreferenceScheduler) CheckLanguageSwitch(currentMsg string) {
	ps.throttle.mu.Lock()
	defer ps.throttle.mu.Unlock()

	lang := detectLanguage(currentMsg)
	if lang == "" || ps.throttle.lastLang == "" {
		ps.throttle.lastLang = lang
		return
	}
	if lang != ps.throttle.lastLang && ps.throttle.lastLang != "mixed" && lang != "mixed" {
		ps.throttle.langSwitch = true
	}
	ps.throttle.lastLang = lang
}

// RecordNegativeFeedback increments the consecutive negative feedback counter.
func (ps *PreferenceScheduler) RecordNegativeFeedback() {
	ps.throttle.mu.Lock()
	defer ps.throttle.mu.Unlock()
	ps.throttle.consecNeg++
}

// RecordNonNegativeFeedback resets the consecutive negative feedback counter.
// Should be called when the user response is positive or neutral.
func (ps *PreferenceScheduler) RecordNonNegativeFeedback() {
	ps.throttle.mu.Lock()
	defer ps.throttle.mu.Unlock()
	ps.throttle.consecNeg = 0
}

// RecordToolCalls updates the cumulative tool distribution and detects
// a shift if the current turn's distribution differs significantly from history.
func (ps *PreferenceScheduler) RecordToolCalls(calls []ToolCallInfo) {
	if len(calls) == 0 {
		return
	}
	ps.throttle.mu.Lock()
	defer ps.throttle.mu.Unlock()

	// Build current-turn distribution.
	turnDist := make(map[string]int, len(calls))
	for _, c := range calls {
		turnDist[c.Name]++
	}

	// Compare with cumulative: if a tool appears this turn but has < 20% of
	// total cumulative calls, it's a new pattern → shift detected.
	totalCalls := 0
	for _, n := range ps.throttle.toolDistrib {
		totalCalls += n
	}
	if totalCalls > 10 { // need enough history to be meaningful
		for name := range turnDist {
			prev := ps.throttle.toolDistrib[name]
			if prev == 0 || float64(prev)/float64(totalCalls) < 0.2 {
				ps.throttle.toolShift = true
				break
			}
		}
	}

	// Update cumulative distribution.
	for name, count := range turnDist {
		ps.throttle.toolDistrib[name] += count
	}
}

// ToolCallInfo is a lightweight record of a tool call for distribution tracking.
type ToolCallInfo struct {
	Name string
}

// detectLanguage returns "zh", "en", or "other" based on CJK character ratio.
func detectLanguage(s string) string {
	total := utf8.RuneCountInString(s)
	if total == 0 {
		return ""
	}
	cjk := 0
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			cjk++
		}
	}
	ratio := float64(cjk) / float64(total)
	if ratio > 0.3 {
		return "zh"
	}
	if ratio < 0.05 {
		return "en"
	}
	return "mixed"
}

// SchedulePreferenceUpdate records the turn and schedules preference extraction
// if the throttle allows. Throttle state is managed internally.
func (s *Service) SchedulePreferenceUpdate(sessionID string, messages []models.Message, ext Extractor, scheduler *PreferenceScheduler) {
	if s == nil || s.storage == nil || ext == nil || scheduler == nil {
		return
	}
	if !scheduler.RecordTurn() {
		return
	}
	if s.queue != nil {
		s.queue.submit(updateJob{
			typ:       jobPreferenceUpdate,
			sessionID: sessionID,
			messages:  prepareAsyncMessages(messages),
			ext:       ext,
		})
		slog.Debug("preference extraction scheduled",
			"session", sessionID,
		)
	}
}
