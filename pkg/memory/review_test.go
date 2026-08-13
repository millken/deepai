package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
)

func reviewResponse(content string) llm.ChatResponse {
	return llm.ChatResponse{Message: models.Message{Role: models.RoleAI, Content: content}}
}

func reviewMessages() []models.Message {
	return []models.Message{
		{Role: models.RoleHuman, Content: "always run gofmt before committing"},
		{Role: models.RoleAI, Content: "noted"},
	}
}

func TestReviewRefineApproves(t *testing.T) {
	t.Parallel()

	provider := &mockMemoryLLMProvider{
		chatResp: reviewResponse(`{"shouldRefine":true,"rationale":"a stable workflow preference appeared"}`),
	}
	client := NewLLMClient(provider, "test-model")

	got, err := client.ReviewRefine(context.Background(), Document{SessionID: "s1"}, reviewMessages())
	if err != nil {
		t.Fatalf("ReviewRefine() error = %v", err)
	}
	if !got.ShouldRefine {
		t.Fatal("want ShouldRefine=true")
	}
	if got.Rationale != "a stable workflow preference appeared" {
		t.Fatalf("rationale = %q", got.Rationale)
	}
}

func TestReviewRefineRejects(t *testing.T) {
	t.Parallel()

	provider := &mockMemoryLLMProvider{
		chatResp: reviewResponse(`{"shouldRefine":false,"rationale":"transient tool output only"}`),
	}
	client := NewLLMClient(provider, "test-model")

	got, err := client.ReviewRefine(context.Background(), Document{SessionID: "s1"}, reviewMessages())
	if err != nil {
		t.Fatalf("ReviewRefine() error = %v", err)
	}
	if got.ShouldRefine {
		t.Fatal("want ShouldRefine=false")
	}
	if got.Rationale != "transient tool output only" {
		t.Fatalf("rationale = %q", got.Rationale)
	}
}

func TestReviewRefineToleratesWrappedJSON(t *testing.T) {
	t.Parallel()

	provider := &mockMemoryLLMProvider{
		chatResp: reviewResponse("Let me consider the trajectory.\n```json\n{\"shouldRefine\":true,\"rationale\":\"ok\"}\n```"),
	}
	client := NewLLMClient(provider, "test-model")

	got, err := client.ReviewRefine(context.Background(), Document{SessionID: "s1"}, reviewMessages())
	if err != nil {
		t.Fatalf("ReviewRefine() error = %v", err)
	}
	if !got.ShouldRefine {
		t.Fatal("fenced JSON with a prose preamble must still parse")
	}
}

func TestReviewRefineUsesASmallTokenBudget(t *testing.T) {
	t.Parallel()

	// The gate only pays for itself if it is much cheaper than the extraction it
	// replaces; a boolean verdict never needs a large completion.
	provider := &mockMemoryLLMProvider{chatResp: reviewResponse(`{"shouldRefine":false}`)}
	client := NewLLMClient(provider, "test-model")

	if _, err := client.ReviewRefine(context.Background(), Document{SessionID: "s1"}, reviewMessages()); err != nil {
		t.Fatalf("ReviewRefine() error = %v", err)
	}
	if provider.lastReq.MaxTokens == nil {
		t.Fatal("gate must cap MaxTokens")
	}
	if *provider.lastReq.MaxTokens > refineReviewMaxTokens {
		t.Fatalf("MaxTokens = %d, want <= %d", *provider.lastReq.MaxTokens, refineReviewMaxTokens)
	}
}

func TestReviewRefineDoesNotCallTheModelWithoutMessages(t *testing.T) {
	t.Parallel()

	provider := &mockMemoryLLMProvider{chatErr: errors.New("must not be called")}
	client := NewLLMClient(provider, "test-model")

	got, err := client.ReviewRefine(context.Background(), Document{SessionID: "s1"}, nil)
	if err != nil {
		t.Fatalf("ReviewRefine() error = %v", err)
	}
	if got.ShouldRefine {
		t.Fatal("nothing to extract from an empty trajectory")
	}
}

func TestReviewRefineSurfacesProviderErrors(t *testing.T) {
	t.Parallel()

	// The executor turns an error into fail-open (extract anyway); the gate's job
	// is to report that it could not decide, not to decide for it.
	provider := &mockMemoryLLMProvider{chatErr: errors.New("upstream 503")}
	client := NewLLMClient(provider, "test-model")

	if _, err := client.ReviewRefine(context.Background(), Document{SessionID: "s1"}, reviewMessages()); err == nil {
		t.Fatal("want the provider error surfaced")
	}
}

func TestReviewRefineRejectsUndecodableVerdict(t *testing.T) {
	t.Parallel()

	provider := &mockMemoryLLMProvider{chatResp: reviewResponse("I am not going to answer that.")}
	client := NewLLMClient(provider, "test-model")

	if _, err := client.ReviewRefine(context.Background(), Document{SessionID: "s1"}, reviewMessages()); err == nil {
		t.Fatal("a response with no verdict must be an error, so the caller can fail open")
	}
}

func TestReviewRefineRequiresAProvider(t *testing.T) {
	t.Parallel()

	client := NewLLMClient(nil, "test-model")
	if _, err := client.ReviewRefine(context.Background(), Document{SessionID: "s1"}, reviewMessages()); err == nil {
		t.Fatal("want an error when no provider is configured")
	}
}

func TestLLMClientSatisfiesReviewer(t *testing.T) {
	t.Parallel()

	var _ Reviewer = (*LLMClient)(nil)
}
