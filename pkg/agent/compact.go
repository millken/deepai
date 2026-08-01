package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/millken/deepai/pkg/models"
)

const (
	defaultCompactionThreshold = 0.75
	defaultCompactionKeepTail  = 6
	compactToolResultKeep      = 300
	compactAssistantTextKeep   = 200
	// maxToolContentBytes is a hard cap applied when storing tool results in
	// runMessages. Prevents individual bash/web-fetch outputs from inflating
	// the context beyond any provider's practical limit.
	maxToolContentBytes = 50_000
	// offloadThresholdBytes is the size above which a tool result is written
	// to disk and replaced in-context with a summary + first/last 50 lines.
	// Per design doc §9 (l0_source_guard.offload_threshold_bytes).
	offloadThresholdBytes = 24 * 1024
)

// compactMessages applies heuristic compression to old messages.
// It preserves: system messages, the first human message, and the last keepTail messages.
// Messages in the middle ("old" region) are summarized:
//   - Tool results → "[tool result: {name}, {first N chars}...]"
//   - Assistant with tool calls → keep tool_calls (ID+Name only, strip Arguments), add summary text
//   - Assistant with text → first N chars + "[...]"
//
// Compacted messages preserve SessionID and critical struct fields (ToolResult, ToolCalls)
// to maintain provider mapping integrity and session persistence validation.
//
// Returns the compacted slice and whether any compaction occurred.
func compactMessages(messages []models.Message, keepTail int) ([]models.Message, bool) {
	n := len(messages)
	if n <= keepTail+2 {
		return messages, false
	}

	tailStart := n - keepTail
	if tailStart < 2 {
		tailStart = 2
	}

	// Find protected head: system messages + first human message.
	headEnd := 0
	for i, msg := range messages {
		if msg.Role == models.RoleSystem {
			headEnd = i + 1
			continue
		}
		if msg.Role == models.RoleHuman {
			headEnd = i + 1
			break
		}
		break
	}

	if headEnd >= tailStart {
		return messages, false
	}

	// Don't cut in the middle of a tool_call/tool_result chain.
	// If the tail would start with a tool result, move back to include
	// the preceding assistant message that issued the tool calls.
	for tailStart > headEnd && messages[tailStart].Role == models.RoleTool {
		tailStart--
	}

	var result []models.Message
	compacted := false

	// Phase 1: preserve head verbatim.
	result = append(result, messages[:headEnd]...)

	// Phase 2: compact the middle region.
	for i := headEnd; i < tailStart; i++ {
		msg := messages[i]
		switch msg.Role {
		case models.RoleTool:
			compactedMsg := compactToolMessage(msg)
			result = append(result, compactedMsg)
			if compactedMsg.Content != msg.Content {
				compacted = true
			}

		case models.RoleAI:
			compactedMsg := compactAssistantMessage(msg)
			result = append(result, compactedMsg)
			if compactedMsg.Content != msg.Content || len(compactedMsg.ToolCalls) != len(msg.ToolCalls) {
				compacted = true
			}

		default:
			result = append(result, msg)
		}
	}

	// Phase 3: append tail verbatim.
	result = append(result, messages[tailStart:]...)

	return result, compacted
}

// compactToolMessage creates a compressed copy of a tool result message.
// Preserves SessionID, ToolResult (CallID/ToolName/Status), truncates Content.
func compactToolMessage(msg models.Message) models.Message {
	summary := summarizeOldToolResult(msg)
	out := models.Message{
		ID:        msg.ID,
		SessionID: msg.SessionID,
		Role:      models.RoleTool,
		Content:   summary,
	}
	if msg.ToolResult != nil {
		out.ToolResult = &models.ToolResult{
			CallID:   msg.ToolResult.CallID,
			ToolName: msg.ToolResult.ToolName,
			Status:   msg.ToolResult.Status,
		}
	}
	return out
}

// compactAssistantMessage creates a compressed copy of an assistant message.
// For messages with tool_calls: keeps ToolCalls with ID+Name (strips large Arguments),
// and adds a summary text. This preserves the tool_calls→tool_result chain.
// For text-only messages: truncates content.
func compactAssistantMessage(msg models.Message) models.Message {
	out := models.Message{
		ID:        msg.ID,
		SessionID: msg.SessionID,
		Role:      models.RoleAI,
	}

	if len(msg.ToolCalls) > 0 {
		// Preserve tool calls with ID+Name, strip Arguments to save space.
		out.ToolCalls = make([]models.ToolCall, len(msg.ToolCalls))
		for i, tc := range msg.ToolCalls {
			out.ToolCalls[i] = models.ToolCall{
				ID:     tc.ID,
				Name:   tc.Name,
				Status: tc.Status,
			}
		}
		if strings.TrimSpace(msg.Content) == "" {
			names := make([]string, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				names = append(names, tc.Name)
			}
			out.Content = fmt.Sprintf("[Called %s]", strings.Join(names, ", "))
		} else if len(msg.Content) > compactAssistantTextKeep {
			out.Content = msg.Content[:compactAssistantTextKeep] + " [...]"
		} else {
			out.Content = msg.Content
		}
	} else if len(msg.Content) > compactAssistantTextKeep {
		out.Content = msg.Content[:compactAssistantTextKeep] + " [...]"
	} else {
		// Short enough, keep verbatim (but still copy to new struct).
		out.Content = msg.Content
	}

	return out
}

func summarizeOldToolResult(msg models.Message) string {
	toolName := "unknown"
	if msg.ToolResult != nil {
		toolName = msg.ToolResult.ToolName
	}
	content := msg.Content
	if len(content) > compactToolResultKeep {
		content = content[:compactToolResultKeep]
	}
	return fmt.Sprintf("[tool result: %s, %s...]", toolName, content)
}

func resolveCompactionThreshold(v float64) float64 {
	if v > 0 && v <= 1 {
		return v
	}
	return defaultCompactionThreshold
}

func resolveCompactionKeepTail(v int) int {
	if v > 0 {
		return v
	}
	return defaultCompactionKeepTail
}

// estimateTokens estimates token count from the current message history.
// Always computes from actual message bytes to catch context growth
// that lastInputTokens (a lagging indicator) would miss.
func estimateTokens(messages []models.Message, systemPrompt string, _ int) int {
	totalBytes := len(systemPrompt)
	for _, msg := range messages {
		// msg.Content already contains the tool result text (same bytes as
		// ToolResult.Content), so only count it once via msg.Content.
		size := len(msg.Content)
		for _, tc := range msg.ToolCalls {
			argsJSON, _ := json.Marshal(tc.Arguments)
			size += len(tc.ID) + len(tc.Name) + len(argsJSON) + 20 // overhead
		}
		if msg.ToolResult != nil {
			size += len(msg.ToolResult.CallID) + len(msg.ToolResult.ToolName)
			size += len(msg.ToolResult.Error)
			size += 20
		}
		totalBytes += size + 30 // role/ID/metadata overhead per message
	}
	// ~4 chars per token for English/code, ~2 for CJK. Use 3 as compromise.
	if totalBytes/3 == 0 && totalBytes > 0 {
		return 1
	}
	return totalBytes / 3
}
