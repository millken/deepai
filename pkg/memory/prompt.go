package memory

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

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
    "earlierContext": "string",
    "longTermBackground": "string"
  },
  "facts": [
    {
      "id": "stable_fact_id",
      "content": "fact text",
      "category": "work|personal|preference|project|other",
      "confidence": 0.0,
      "source": "session id where this fact was learned"
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
	return BuildInjectionWithContext(doc, "", 0)
}

func BuildInjectionWithContext(doc Document, currentContext string, maxTokens int) string {
	injection, _ := buildInjectionWithIDs(doc, currentContext, maxTokens, "")
	return injection
}

// buildInjectionWithIDs returns the injection text and IDs of facts selected for retrieval tracking.
// activeSource is used for memory fence: when non-empty, facts from other skill sources are penalized.
func buildInjectionWithIDs(doc Document, currentContext string, maxTokens int, activeSource string) (string, []string) {
	if maxTokens <= 0 {
		maxTokens = 2000
	}

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
	if v := strings.TrimSpace(doc.History.LongTermBackground); v != "" {
		lines = append(lines, "Long Term Background: "+v)
	}
	base := strings.Join(lines, "\n")
	// Reserve tokens for XML wrapper overhead (~45 tokens).
	const wrapperOverhead = 45
	remainingBudget := maxTokens - approximateTokenCount(base) - wrapperOverhead
	selectedFacts, selectedIDs := selectRelevantFacts(doc.Facts, currentContext, remainingBudget, activeSource)
	if len(selectedFacts) > 0 {
		lines = append(lines, "Known Facts:")
		for _, fact := range selectedFacts {
			if strings.TrimSpace(fact.Category) != "" {
				lines = append(lines, fmt.Sprintf("- [%s] %s", fact.Category, fact.Content))
				continue
			}
			lines = append(lines, "- "+fact.Content)
		}
	}
	if len(lines) == 1 {
		return "", nil
	}

	body := strings.Join(lines, "\n") + "\n\n## Current Session\n"
	wrapped := "<memory-context>\n[System note: The following is recalled memory, not new user input.]\n" + body + "</memory-context>"
	return wrapped, selectedIDs
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

// selectRelevantFacts picks the most relevant facts for injection.
// It returns the selected facts and their IDs for retrieval tracking.
// When activeSource is non-empty (e.g. "skill:commit"), facts from other skill
// sources are penalized to prevent cross-skill memory contamination.
func selectRelevantFacts(facts []Fact, currentContext string, remainingTokens int, activeSource string) ([]Fact, []string) {
	if len(facts) == 0 || remainingTokens <= 0 {
		return nil, nil
	}

	type scoredFact struct {
		fact       Fact
		score      float64
		similarity float64
		confidence float64
	}

	contextTerms := extractTerms(currentContext)
	useContext := len(contextTerms) > 0
	scored := make([]scoredFact, 0, len(facts))
	hasContextMatch := false
	for _, fact := range facts {
		fact.Content = strings.TrimSpace(fact.Content)
		if fact.Content == "" {
			continue
		}
		// Hard filter: facts with sustained negative feedback are removed entirely.
		if fact.SuspectCount >= 3 {
			continue
		}
		confidence := clamp01(fact.Confidence)
		score := confidence
		similarity := 0.0
		if useContext {
			similarity = cosineSimilarity(contextTerms, extractTerms(fact.Content))
			score = (similarity * 0.6) + (confidence * 0.4)
			if similarity > 0 {
				hasContextMatch = true
			}
		}
		// Apply fence: penalize facts from other skill sources.
		if activeSource != "" {
			src := strings.TrimSpace(fact.Source)
			if strings.HasPrefix(src, "skill:") && src != activeSource {
				score *= 0.3
			}
		}
		// Soft penalty for occasional negative feedback (1 or 2 suspect hits).
		if fact.SuspectCount > 0 {
			score *= 1.0 - 0.3*float64(fact.SuspectCount)
		}
		scored = append(scored, scoredFact{
			fact:       fact,
			score:      score,
			similarity: similarity,
			confidence: confidence,
		})
	}
	if useContext && hasContextMatch {
		filtered := scored[:0]
		for _, item := range scored {
			if item.similarity > 0 {
				filtered = append(filtered, item)
			}
		}
		scored = filtered
	}

	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		if scored[i].confidence != scored[j].confidence {
			return scored[i].confidence > scored[j].confidence
		}
		if !scored[i].fact.UpdatedAt.Equal(scored[j].fact.UpdatedAt) {
			return scored[i].fact.UpdatedAt.After(scored[j].fact.UpdatedAt)
		}
		if !scored[i].fact.CreatedAt.Equal(scored[j].fact.CreatedAt) {
			return scored[i].fact.CreatedAt.After(scored[j].fact.CreatedAt)
		}
		return scored[i].fact.ID < scored[j].fact.ID
	})

	selected := make([]Fact, 0, len(scored))
	selectedIDs := make([]string, 0, len(scored))
	usedTokens := 0
	for _, item := range scored {
		line := item.fact.Content
		if strings.TrimSpace(item.fact.Category) != "" {
			line = fmt.Sprintf("- [%s] %s", item.fact.Category, item.fact.Content)
		} else {
			line = "- " + item.fact.Content
		}
		cost := approximateTokenCount(line)
		if usedTokens > 0 && usedTokens+cost > remainingTokens {
			break
		}
		selected = append(selected, item.fact)
		selectedIDs = append(selectedIDs, item.fact.ID)
		usedTokens += cost
	}
	return selected, selectedIDs
}

func approximateTokenCount(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	runes := len([]rune(text))
	if runes <= 4 {
		return 1
	}
	return (runes + 3) / 4
}

func extractTerms(text string) map[string]float64 {
	terms := make(map[string]float64)
	var current []rune
	flush := func() {
		if len(current) == 0 {
			return
		}
		token := strings.ToLower(strings.TrimSpace(string(current)))
		if token != "" {
			terms[token]++
		}
		current = current[:0]
	}

	for _, r := range strings.ToLower(text) {
		switch {
		case unicode.In(r, unicode.Han):
			flush()
			terms[string(r)]++
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			current = append(current, r)
		default:
			flush()
		}
	}
	flush()
	return terms
}

func cosineSimilarity(left, right map[string]float64) float64 {
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	var dot float64
	var leftNorm float64
	var rightNorm float64
	for token, value := range left {
		leftNorm += value * value
		dot += value * right[token]
	}
	for _, value := range right {
		rightNorm += value * value
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0
	}
	return dot / (math.Sqrt(leftNorm) * math.Sqrt(rightNorm))
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
