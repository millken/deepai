package agent

import (
	"fmt"
	"unicode/utf8"

	"github.com/millken/deepai/pkg/models"
)

// defaultMinContextPressure is the fraction of the context window that must be
// occupied before aging kicks in. Below it, buildPromptView returns messages
// untouched — short sessions and early exploration pay no fidelity cost.
const defaultMinContextPressure = 0.4

// defaultToolBudgetsByTool maps tool name -> age -> byte cap.
// Based on §5.4 measured p90/p99 percentiles. The "default" key is the
// catch-all for any tool not explicitly listed (global p90 = 3.1KB).
//
// Layering note: L0 source guards (offload at 24KB, bash 50KB truncate) run
// BEFORE aging in react.go. So by the time aging sees a tool result, extreme
// tails are already removed. The budgets below handle the remaining
// long-tail of "moderately large" results.
var defaultToolBudgetsByTool = map[string]map[int]int{
	"default":    {1: 4096, 2: 1024, 3: 300}, // §5.4 "其他/默认", global p90=3.1KB
	"read_file":  {1: 8192, 2: 2048, 3: 300}, // high: preserve latest reads, prevent re-read
	"bash":       {1: 4096, 2: 1024, 3: 300},
	"edit_file":  {1: 300, 2: 300, 3: 300}, // confirmation messages, p99 < 105B
	"write_file": {1: 300, 2: 300, 3: 300},
	"grep":       {1: 4096, 2: 1024, 3: 300}, // grep p99=168KB tail is eaten by L0 offload (24KB) first
	"web_fetch":  {1: 8192, 2: 2048, 3: 300},
	"docx_read":  {1: 8192, 2: 2048, 3: 300}, // same as read_file: re-reading a chunk is expensive
}

// Default per-age byte budgets. See docs/spec/token-efficiency.md §T1.
var (
	// age 1 keeps most of the previous turn; age 2 trims; age>=3 matches the
	// existing compaction floor (compactToolResultKeep = 300).
	defaultToolResultBudgets = map[int]int{1: 8192, 2: 2048, 3: 300}
	// T4 (conversation text): age 0-1 untouched, age 2-3 keep 500, age>=4 keep 200.
	defaultConversationBudgets = map[int]int{2: 500, 4: 200}
)

// AgingConfig controls buildPromptView's information-decay compression (T1 tool
// results + T4 conversation text). It drives a request-scoped prompt VIEW; the
// canonical runMessages are never modified. A nil config or Enabled=false makes
// buildPromptView a zero-copy pass-through.
type AgingConfig struct {
	Enabled bool

	// MinContextPressure gates aging on context-window occupancy (fraction,
	// e.g. 0.4). When estimated tokens < MinContextPressure × contextWindow,
	// buildPromptView returns messages unchanged. <=0 disables the gate
	// (always age by the budgets below). The recommended default is 0.4.
	MinContextPressure float64

	// ToolResultBudgets maps age -> byte cap for RoleTool Content (T1). Age
	// resolves to the largest key <= age (a step function); no key <= age means
	// "don't compress". nil uses defaultToolResultBudgets. age 0 is never
	// compressed (current turn).
	ToolResultBudgets map[int]int

	// ToolResultBudgetsByTool maps tool name -> age -> byte cap, overriding
	// the per-tool defaults from §5.4. nil = use built-in defaults.
	// ToolResultBudgets (the age-only map above) is the fallback for any tool
	// name not found in either the override or the built-in defaults.
	ToolResultBudgetsByTool map[string]map[int]int

	// ConversationBudgets maps age -> byte cap for RoleAI Content (T4). nil uses
	// defaultConversationBudgets; an empty (non-nil) map disables T4 entirely
	// (all ages return 0 = no compression) — this is how T1-only mode is run.
	// Only Content is affected; ToolCalls (including Arguments) are never touched.
	ConversationBudgets map[int]int
}

// truncateRuneSafe cuts s to at most n bytes without splitting a multi-byte
// UTF-8 rune (backing up to the nearest rune start). Slicing by raw byte index
// would emit invalid UTF-8 for CJK content, which strict providers reject.
func truncateRuneSafe(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// budgetForAge returns the byte cap for the given age: the value of the largest
// key <= age, or 0 (no compression) if no such key exists.
func budgetForAge(budgets map[int]int, age int) int {
	bestKey := -1
	val := 0
	for k, v := range budgets {
		if k <= age && k > bestKey {
			bestKey = k
			val = v
		}
	}
	return val
}

func (c *AgingConfig) toolResultBudget(age int, toolName string) int {
	// 1. Caller-provided per-tool overrides win.
	if byTool := c.ToolResultBudgetsByTool; byTool != nil {
		if budgets, ok := byTool[toolName]; ok {
			return budgetForAge(budgets, age)
		}
	}
	// 2. Built-in per-tool defaults (§5.4 measured p90/p99).
	if budgets, ok := defaultToolBudgetsByTool[toolName]; ok {
		return budgetForAge(budgets, age)
	}
	// 3. Fallback to the "default" row in the per-tool table (§5.4 "其他/默认").
	//    This is tighter than the legacy defaultToolResultBudgets (which was
	//    {1:8192, 2:2048, 3:300}) because global p90 is only 3.1KB.
	if budgets, ok := defaultToolBudgetsByTool["default"]; ok {
		return budgetForAge(budgets, age)
	}
	// 4. Last resort: legacy age-only budget (should never reach here).
	b := c.ToolResultBudgets
	if b == nil {
		b = defaultToolResultBudgets
	}
	return budgetForAge(b, age)
}

func (c *AgingConfig) conversationBudget(age int) int {
	b := c.ConversationBudgets
	if b == nil {
		b = defaultConversationBudgets
	}
	return budgetForAge(b, age)
}

// buildPromptView derives a request-scoped compressed view of the canonical
// message history. Historical RoleTool Content (T1) and RoleAI Content (T4) are
// truncated by age; ToolResult/ToolCalls structure and the canonical messages
// themselves are left intact. Age is computed by scanning assistant-turn indices
// (see docs/spec/token-efficiency.md §T1), never from a Run turn counter.
//
// contextWindow is the model's window in tokens, used only for the pressure gate.
func buildPromptView(messages []models.Message, cfg *AgingConfig, contextWindow int) []models.Message {
	if cfg == nil || !cfg.Enabled {
		return messages // pass-through, zero copy
	}

	// Pressure gate: skip all compression while the window is far from full.
	// Uses the same byte heuristic as compaction's threshold; it excludes the
	// system prompt and tool schema, so it under-estimates true pressure — a
	// conservative bias (aging starts slightly later, never earlier).
	if cfg.MinContextPressure > 0 {
		if contextWindow <= 0 {
			// Window unknown (ContextWindow=0 is a valid "no compaction" state,
			// e.g. subagents without WithContextWindow). The gate cannot be
			// evaluated, so fail safe: no aging, rather than aging from turn one.
			return messages
		}
		estimated := estimateTokens(messages, "", 0)
		if float64(estimated) < cfg.MinContextPressure*float64(contextWindow) {
			return messages
		}
	}

	// Pass 1: assign each message the aiTurnIndex it belongs to. Every RoleAI
	// message (with or without tool_calls) advances the index; a RoleTool message
	// inherits the index of the most recent RoleAI before it.
	aiTurnIndex := -1
	ownerTurn := make([]int, len(messages))
	for i, msg := range messages {
		if msg.Role == models.RoleAI {
			aiTurnIndex++
		}
		ownerTurn[i] = aiTurnIndex
	}
	totalAITurns := aiTurnIndex + 1
	if totalAITurns == 0 {
		return messages // no assistant turns yet, nothing to age
	}

	// Pass 2: derive the view. Shallow-copy the slice; only Content strings are
	// reassigned (to new strings) when a budget applies, so canonical messages —
	// and the shared ToolResult pointers — are never mutated.
	view := make([]models.Message, len(messages))
	copy(view, messages)

	for i := range view {
		msg := &view[i]
		age := totalAITurns - 1 - ownerTurn[i]
		if age <= 0 {
			continue // current turn (or pre-first-AI): keep full fidelity
		}

		// Images: strip base64 data from aged messages to save tokens. The
		// image is no longer useful for historical context — a text placeholder
		// suffices to indicate an image was once attached.
		if len(msg.Images) > 0 {
			msg.Images = nil
			if msg.Content == "" {
				msg.Content = "[image attachments removed for context compression]"
			}
		}

		switch msg.Role {
		case models.RoleTool: // T1
			toolName := ""
			if msg.ToolResult != nil {
				toolName = msg.ToolResult.ToolName
			}
			budget := cfg.toolResultBudget(age, toolName)
			if budget > 0 && len(msg.Content) > budget {
				msg.Content = truncateRuneSafe(msg.Content, budget) + agedToolHint(toolName, age)
			}
		case models.RoleAI: // T4 (only Content; ToolCalls untouched)
			budget := cfg.conversationBudget(age)
			if budget > 0 && len(msg.Content) > budget {
				msg.Content = truncateRuneSafe(msg.Content, budget) + "\n[...earlier response truncated]"
			}
		}
	}
	return view
}

// agedToolHint generates the suffix appended to a truncated tool result.
// Tools whose output can look "complete" even when heavily truncated (grep,
// web_fetch) get a stronger warning that the visible content is only a
// fragment and important matches/details may have been omitted.
func agedToolHint(toolName string, age int) string {
	switch toolName {
	case "grep", "web_fetch":
		return fmt.Sprintf("\n[...aged %d: this output was truncated to fit context — re-call %s for the complete result, important content may be missing]", age, toolName)
	default:
		return fmt.Sprintf("\n[...aged: re-call %s to see full output]", toolName)
	}
}
