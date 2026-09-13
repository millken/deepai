package memory

import (
	"regexp"
	"strings"
	"unicode"

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
	// Nothing to strip: return the text byte-for-byte. Sentence splitting is
	// lossy at the edges (it normalizes inter-sentence whitespace), and the
	// overwhelming majority of memory text mentions no upload at all, so the
	// filter must not touch it. The earlier unconditional split/rejoin is how
	// every stored memory lost its sentence punctuation — "github.com" came
	// back as "github com" and the model then fed that path to read_file.
	if !uploadMentionRE.MatchString(trimmed) {
		return trimmed
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

// splitIntoSentences splits text at sentence boundaries, KEEPING each
// sentence's terminating punctuation attached to the sentence it ends.
// Dropping the terminators (strings.FieldsFunc) corrupted every surviving
// sentence, and splitting on a bare '.' also cut inside file paths, domains
// and versions ("HANDOFF.md", "github.com", "v1.2"), so an upload mention
// anywhere in a sentence could take half a path with it.
//
// ASCII terminators therefore only end a sentence when the next rune is
// whitespace or the text ends; CJK terminators are unambiguous and always
// do. Newlines split but are not retained — the caller rejoins with a space.
func splitIntoSentences(text string) []string {
	runes := []rune(text)
	parts := make([]string, 0, 8)
	var current []rune
	flush := func() {
		if len(current) > 0 {
			parts = append(parts, string(current))
			current = current[:0]
		}
	}
	for i, r := range runes {
		switch r {
		case '\n', '\r':
			flush()
		case '。', '！', '？', '；':
			current = append(current, r)
			flush()
		case '.', '!', '?', ';':
			current = append(current, r)
			if i+1 >= len(runes) || unicode.IsSpace(runes[i+1]) {
				flush()
			}
		default:
			current = append(current, r)
		}
	}
	flush()
	return parts
}
