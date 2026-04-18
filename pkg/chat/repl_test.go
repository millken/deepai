package chat

import (
	"context"
	"errors"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
)

// mockLLMProvider implements llm.LLMProvider for testing.
type mockLLMProvider struct {
	response string
	err      error
}

func (m *mockLLMProvider) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	if m.err != nil {
		return llm.ChatResponse{}, m.err
	}
	return llm.ChatResponse{
		Message: models.Message{
			Role:    models.RoleAI,
			Content: m.response,
		},
	}, nil
}

func (m *mockLLMProvider) Stream(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	// Not used in generateTitle.
	return nil, nil
}

func TestGenerateTitle_Success(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Model: "test-model"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	mock := &mockLLMProvider{response: "Test Title"}
	r := &ChatRepl{
		cfg: ReplConfig{
			LLMProvider: mock,
			Model:       "test-model",
		},
		sessMgr: store,
	}

	r.generateTitle(sess.ID, "Hello, this is a test message")

	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load session: %v", err)
	}
	if loaded.Title != "Test Title" {
		t.Fatalf("expected title %q, got %q", "Test Title", loaded.Title)
	}
}

func TestGenerateTitle_LongTitle(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Model: "test-model"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	// LLM returns a title longer than 30 characters.
	longTitle := "This is a very long title that exceeds thirty characters"
	mock := &mockLLMProvider{response: longTitle}
	r := &ChatRepl{
		cfg: ReplConfig{
			LLMProvider: mock,
			Model:       "test-model",
		},
		sessMgr: store,
	}

	r.generateTitle(sess.ID, "Hello")

	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load session: %v", err)
	}
	// Should be truncated to 30 characters.
	expected := longTitle[:30]
	if loaded.Title != expected {
		t.Fatalf("expected title %q, got %q", expected, loaded.Title)
	}
}

func TestGenerateTitle_EmptyResponse(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Model: "test-model"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	mock := &mockLLMProvider{response: ""}
	r := &ChatRepl{
		cfg: ReplConfig{
			LLMProvider: mock,
			Model:       "test-model",
		},
		sessMgr: store,
	}

	r.generateTitle(sess.ID, "Hello, this is a test message")

	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load session: %v", err)
	}
	// Should use fallback: first 20 chars + "..."
	expected := "Hello, this is a tes..."
	if loaded.Title != expected {
		t.Fatalf("expected title %q, got %q", expected, loaded.Title)
	}
}

func TestGenerateTitle_LLMError(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Model: "test-model"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	mock := &mockLLMProvider{err: errors.New("unavailable")}
	r := &ChatRepl{
		cfg: ReplConfig{
			LLMProvider: mock,
			Model:       "test-model",
		},
		sessMgr: store,
	}

	r.generateTitle(sess.ID, "Hello, this is a test message")

	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load session: %v", err)
	}
	// Should use fallback: first 20 chars + "..."
	expected := "Hello, this is a tes..."
	if loaded.Title != expected {
		t.Fatalf("expected title %q, got %q", expected, loaded.Title)
	}
}

func TestGenerateTitle_ShortFallback(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Model: "test-model"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	mock := &mockLLMProvider{err: errors.New("unavailable")}
	r := &ChatRepl{
		cfg: ReplConfig{
			LLMProvider: mock,
			Model:       "test-model",
		},
		sessMgr: store,
	}

	r.generateTitle(sess.ID, "Short")

	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load session: %v", err)
	}
	// Should use fallback: "Short" (no truncation).
	if loaded.Title != "Short" {
		t.Fatalf("expected title %q, got %q", "Short", loaded.Title)
	}
}

func TestGenerateTitle_NoProvider(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Model: "test-model"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	r := &ChatRepl{
		cfg: ReplConfig{
			LLMProvider: nil,
			Model:       "test-model",
		},
		sessMgr: store,
	}

	// Should not panic, should skip.
	r.generateTitle(sess.ID, "Hello")

	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load session: %v", err)
	}
	// Title should remain empty.
	if loaded.Title != "" {
		t.Fatalf("expected empty title, got %q", loaded.Title)
	}
}

func TestGenerateTitle_EmptyMessage(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	sess, err := store.Create(models.CreateOpts{Model: "test-model"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	mock := &mockLLMProvider{response: "Test Title"}
	r := &ChatRepl{
		cfg: ReplConfig{
			LLMProvider: mock,
			Model:       "test-model",
		},
		sessMgr: store,
	}

	// Should skip when firstUserMsg is empty.
	r.generateTitle(sess.ID, "")

	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load session: %v", err)
	}
	// Title should remain empty.
	if loaded.Title != "" {
		t.Fatalf("expected empty title, got %q", loaded.Title)
	}
}
