package memory

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/millken/deepai/pkg/models"
)

const MemoryUpdateSystemPrompt = `You maintain durable user memory for an AI agent.
Return strict JSON only.
Capture stable, high-value memory from the recent conversation.
Do not repeat transient filler or tool chatter.
If nothing should change, return an empty update with the same schema.

Schema:
{
  "user": {
    "workContext": "string",
    "personalContext": "string",
    "topOfMind": "string"
  },
  "history": {
    "recentMonths": "string",
    "earlierContext": "string"
  },
  "facts": [
    {
      "id": "stable_fact_id",
      "content": "fact text",
      "category": "work|personal|preference|project|other",
      "confidence": 0.0
    }
  ],
  "source": "session id"
}`

func BuildMemoryUpdatePrompt(messages []models.Message, current Document) string {
	currentJSON, _ := json.MarshalIndent(current, "", "  ")
	return fmt.Sprintf(
		"Current memory:\n%s\n\nRecent conversation:\n%s\n\nProduce the memory update JSON.",
		string(currentJSON),
		renderMessagesForPrompt(messages),
	)
}

func BuildInjection(doc Document) string {
	lines := []string{"## User Memory"}

	if v := strings.TrimSpace(doc.User.WorkContext); v != "" {
		lines = append(lines, "Work Context: "+v)
	}
	if v := strings.TrimSpace(doc.User.PersonalContext); v != "" {
		lines = append(lines, "Personal Context: "+v)
	}
	if v := strings.TrimSpace(doc.User.TopOfMind); v != "" {
		lines = append(lines, "Top Of Mind: "+v)
	}
	if v := strings.TrimSpace(doc.History.RecentMonths); v != "" {
		lines = append(lines, "Recent Months: "+v)
	}
	if v := strings.TrimSpace(doc.History.EarlierContext); v != "" {
		lines = append(lines, "Earlier Context: "+v)
	}
	if len(doc.Facts) > 0 {
		lines = append(lines, "Known Facts:")
		for _, fact := range doc.Facts {
			if strings.TrimSpace(fact.Content) == "" {
				continue
			}
			if strings.TrimSpace(fact.Category) != "" {
				lines = append(lines, fmt.Sprintf("- [%s] %s", fact.Category, fact.Content))
				continue
			}
			lines = append(lines, "- "+fact.Content)
		}
	}
	if len(lines) == 1 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n\n## Current Session\n"
}

func renderMessagesForPrompt(messages []models.Message) string {
	if len(messages) == 0 {
		return "(no messages)"
	}

	var b strings.Builder
	for _, msg := range messages {
		role := string(msg.Role)
		content := strings.TrimSpace(msg.Content)
		if content == "" && msg.ToolResult != nil {
			content = strings.TrimSpace(msg.ToolResult.Content)
			if content == "" {
				content = strings.TrimSpace(msg.ToolResult.Error)
			}
		}
		if content == "" {
			continue
		}
		b.WriteString(role)
		b.WriteString(": ")
		b.WriteString(content)
		b.WriteString("\n")
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "(no textual messages)"
	}
	return out
}
