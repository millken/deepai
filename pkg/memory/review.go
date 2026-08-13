package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
)

// refineReviewMaxTokens caps the gate's completion. The gate exists to avoid
// paying for a full extraction, so it must stay much cheaper than one; a
// boolean verdict plus a one-line rationale never needs more than this.
//
// Note this bounds the OUTPUT only. The gate's input is the same trajectory the
// extractor would read, so the saving is real but bounded — see the rejection
// rate discussion in the design doc.
const refineReviewMaxTokens = 2048

// RefineReviewSystemPrompt asks for a spend/skip decision, not for facts. The
// gate must never produce memory itself; that is the extractor's job, and a
// second producer would fight it for the per-session fact budget.
const RefineReviewSystemPrompt = `You are the review gate for an AI agent's memory refinement.
Decide whether this checkpoint is worth a full memory extraction pass.
Return strict JSON only. Do not extract or restate any memory yourself.

Approve only when the recent conversation contains something durable and useful
for future turns: a stable fact, a decision, a preference, or a lesson from a
failure.

Reject one-off noise, unsupported speculation, transient tool output, and plain
question-and-answer exchanges that leave nothing reusable behind.

Schema:
{
  "shouldRefine": true,
  "rationale": "one short sentence"
}`

// BuildRefineReviewPrompt renders the gate's user turn.
func BuildRefineReviewPrompt(messages []models.Message, current Document) string {
	return fmt.Sprintf(
		"Memory already stored (%d facts):\n%s\n\nRecent conversation:\n%s\n\nShould this checkpoint trigger a memory extraction?",
		len(current.Facts),
		renderFactsForReview(current),
		renderMessagesForPrompt(messages),
	)
}

func renderFactsForReview(current Document) string {
	if len(current.Facts) == 0 {
		return "(none)"
	}
	out := ""
	for _, fact := range current.Facts {
		out += "- " + fact.Content + "\n"
	}
	return out
}

// ReviewRefine implements Reviewer.
//
// An error means "could not decide", not "do not refine". Callers fail open on
// error and extract anyway, so a flaky gate degrades to the unconditional
// behaviour that predates it rather than silently stopping extraction.
func (c *LLMClient) ReviewRefine(ctx context.Context, current Document, messages []models.Message) (RefineReview, error) {
	if c == nil || c.provider == nil {
		return RefineReview{}, errors.New("memory llm provider is not configured")
	}
	if len(messages) == 0 {
		// Nothing to extract from, so nothing to approve. No call, no cost.
		return RefineReview{}, nil
	}

	maxTokens := refineReviewMaxTokens
	resp, err := c.provider.Chat(ctx, llm.ChatRequest{
		Model:           c.model,
		SystemPrompt:    RefineReviewSystemPrompt,
		ReasoningEffort: "disabled",
		Messages: []models.Message{
			{
				ID:        "refine-review",
				SessionID: current.SessionID,
				Role:      models.RoleHuman,
				Content:   BuildRefineReviewPrompt(messages, current),
				CreatedAt: time.Now().UTC(),
			},
		},
		Temperature: c.temperature,
		MaxTokens:   &maxTokens,
	})
	if err != nil {
		return RefineReview{}, fmt.Errorf("refine review llm call failed: %w", err)
	}

	content := extractJSON(resp.Message.Content)
	if content == "" {
		slog.Debug(
			"refine review returned no json",
			"session", current.SessionID,
			"raw_preview", previewForLog(resp.Message.Content, 200),
		)
		return RefineReview{}, errors.New("refine review response contained no json")
	}
	var review RefineReview
	if err := json.Unmarshal([]byte(content), &review); err != nil {
		slog.Debug(
			"refine review decode failed",
			"session", current.SessionID,
			"json_preview", previewForLog(content, 200),
			"err", err,
		)
		return RefineReview{}, fmt.Errorf("decode refine review response: %w", err)
	}
	slog.Debug(
		"refine review verdict",
		"session", current.SessionID,
		"should_refine", review.ShouldRefine,
		"rationale", review.Rationale,
	)
	return review, nil
}
