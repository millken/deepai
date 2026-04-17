package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

func TestHandleChatJSON(t *testing.T) {
	server := newTestServer()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(`{"session_id":"s1","user_id":"u1","message":"hello","model":"openai/test-model"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"output":"hello world"`) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestHandleChatSSE(t *testing.T) {
	server := newTestServer()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(`{"session_id":"s2","message":"stream","stream":true}`))
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()

	server.routes().ServeHTTP(rec, req)

	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, body)
	}
	if !strings.Contains(body, "event: ready") {
		t.Fatalf("missing ready event: %s", body)
	}
	if !strings.Contains(body, "event: text_chunk") {
		t.Fatalf("missing text_chunk event: %s", body)
	}
	if !strings.Contains(body, "event: done") {
		t.Fatalf("missing done event: %s", body)
	}
}

func TestServerShutdownRunsCleanup(t *testing.T) {
	cleaned := false
	server := newTestServer()
	server.cleanupFns = []func(){func() { cleaned = true }}
	server.shutdownTimeout = 20 * time.Millisecond

	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if !cleaned {
		t.Fatal("cleanup function was not called")
	}
}

func newTestServer() *Server {
	return &Server{
		cfg: Config{
			DefaultModel: "openai/test-model",
		},
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:           newMemoryStore(),
		tools:           tools.NewRegistry(),
		providers:       map[string]llm.LLMProvider{},
		providerFactory: func(string) (llm.LLMProvider, error) { return fakeProvider{}, nil },
	}
}

type fakeProvider struct{}

func (fakeProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (fakeProvider) Stream(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk, 2)
	go func() {
		defer close(ch)
		ch <- llm.StreamChunk{Model: req.Model, Delta: "hello world"}
		ch <- llm.StreamChunk{
			Model: req.Model,
			Message: &models.Message{
				Role:    models.RoleAI,
				Content: "hello world",
			},
			Usage: &llm.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
			Done:  true,
		}
	}()
	return ch, nil
}
