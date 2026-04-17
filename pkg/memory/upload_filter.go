package memory

import (
	"regexp"
	"strings"

	"github.com/millken/deepai/pkg/models"
)

var uploadBlockRE = regexp.MustCompile(`(?is)<uploaded_files>[\s\S]*?</uploaded_files>\n*`)

var uploadMentionRE = regexp.MustCompile(`(?i)(upload(?:ed|ing)?(?:\s+\w+){0,3}\s+(?:file|files?|doc|docs|document|documents?|attachment|attachments?)|file\s+upload|/mnt/user-data/uploads/|<uploaded_files>)`)

func filterMessagesForMemory(messages []models.Message) []models.Message {
	if len(messages) == 0 {
		return nil
	}

	filtered := make([]models.Message, 0, len(messages))
	for i := 0; i < len(messages); i++ {
		msg := messages[i]
		if msg.Role == models.RoleTool {
			continue
		}
		if msg.Role == models.RoleAI && len(msg.ToolCalls) > 0 {
			continue
		}

		if msg.Role != models.RoleHuman {
			filtered = append(filtered, msg)
			continue
		}

		cleaned := stripUploadBlock(msg.Content)
		if cleaned == "" {
			if i+1 < len(messages) && messages[i+1].Role == models.RoleAI {
				i++
			}
			continue
		}

		msg.Content = cleaned
		filtered = append(filtered, msg)
	}
	return filtered
}

func sanitizeUpdateForStorage(update Update) Update {
	update.User.WorkContext = stripUploadSentences(update.User.WorkContext)
	update.User.PersonalContext = stripUploadSentences(update.User.PersonalContext)
	update.User.TopOfMind = stripUploadSentences(update.User.TopOfMind)
	update.History.RecentMonths = stripUploadSentences(update.History.RecentMonths)
	update.History.EarlierContext = stripUploadSentences(update.History.EarlierContext)
	update.History.LongTermBackground = stripUploadSentences(update.History.LongTermBackground)

	facts := make([]Fact, 0, len(update.Facts))
	for _, fact := range update.Facts {
		if uploadMentionRE.MatchString(fact.Content) || uploadBlockRE.MatchString(fact.Content) {
			continue
		}
		fact.Content = stripUploadSentences(fact.Content)
		if strings.TrimSpace(fact.Content) == "" {
			continue
		}
		facts = append(facts, fact)
	}
	update.Facts = facts
	return update
}

func stripUploadBlock(content string) string {
	content = uploadBlockRE.ReplaceAllString(content, "")
	return strings.TrimSpace(content)
}

func stripUploadSentences(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}

	parts := splitIntoSentences(trimmed)
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || uploadMentionRE.MatchString(part) {
			continue
		}
		kept = append(kept, part)
	}
	return strings.Join(kept, " ")
}

func splitIntoSentences(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		switch r {
		case '.', '!', '?', '\n', '\r', '。', '！', '？', ';', '；':
			return true
		default:
			return false
		}
	})
}
