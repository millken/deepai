package llm

import (
	"context"
	"testing"
)

func TestNewModelRegistry_RequiresAtLeastOneDef(t *testing.T) {
	if _, err := NewModelRegistry(nil, ""); err == nil {
		t.Fatal("expected error for empty defs")
	}
}

func TestNewModelRegistry_DuplicateAlias(t *testing.T) {
	defs := []ModelDef{
		{Name: "fast", Provider: "openai", Model: "gpt-4o-mini"},
		{Name: "Fast", Provider: "openai", Model: "gpt-4o"},
	}
	if _, err := NewModelRegistry(defs, ""); err == nil {
		t.Fatal("expected error for duplicate alias (case-insensitive)")
	}
}

func TestNewModelRegistry_MissingProvider(t *testing.T) {
	defs := []ModelDef{
		{Name: "fast", Provider: "", Model: "gpt-4o-mini"},
	}
	if _, err := NewModelRegistry(defs, ""); err == nil {
		t.Fatal("expected error for missing provider")
	}
}

func TestNewModelRegistry_MissingModel(t *testing.T) {
	defs := []ModelDef{
		{Name: "fast", Provider: "openai", Model: ""},
	}
	if _, err := NewModelRegistry(defs, ""); err == nil {
		t.Fatal("expected error for missing model")
	}
}

func TestNewModelRegistry_InvalidDefault(t *testing.T) {
	defs := []ModelDef{
		{Name: "fast", Provider: "openai", Model: "gpt-4o-mini"},
	}
	if _, err := NewModelRegistry(defs, "nonexistent"); err == nil {
		t.Fatal("expected error for invalid default name")
	}
}

func TestNewModelRegistry_DefaultsToFirst(t *testing.T) {
	defs := []ModelDef{
		{Name: "fast", Provider: "openai", Model: "gpt-4o-mini"},
		{Name: "smart", Provider: "anthropic", Model: "claude-sonnet-4-20250514"},
	}
	r, err := NewModelRegistry(defs, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.DefaultName() != "fast" {
		t.Fatalf("default = %q, want fast", r.DefaultName())
	}
}

func TestModelRegistry_Resolve(t *testing.T) {
	defs := []ModelDef{
		{Name: "fast", Provider: "openai", Model: "gpt-4o-mini"},
		{Name: "smart", Provider: "anthropic", Model: "claude-sonnet-4-20250514"},
	}
	r, err := NewModelRegistry(defs, "smart")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Exact match.
	d, ok := r.Resolve("smart")
	if !ok || d.Model != "claude-sonnet-4-20250514" {
		t.Fatalf("Resolve(smart) = %+v, %v", d, ok)
	}

	// Case-insensitive.
	d, ok = r.Resolve("FAST")
	if !ok || d.Model != "gpt-4o-mini" {
		t.Fatalf("Resolve(FAST) = %+v, %v", d, ok)
	}

	// Non-existent.
	_, ok = r.Resolve("unknown")
	if ok {
		t.Fatal("Resolve(unknown) should return false")
	}
}

func TestModelRegistry_Has(t *testing.T) {
	defs := []ModelDef{
		{Name: "fast", Provider: "openai", Model: "gpt-4o-mini"},
	}
	r, err := NewModelRegistry(defs, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !r.Has("fast") {
		t.Fatal("Has(fast) should be true")
	}
	if !r.Has("FAST") {
		t.Fatal("Has(FAST) should be true (case-insensitive)")
	}
	if r.Has("unknown") {
		t.Fatal("Has(unknown) should be false")
	}
}

func TestModelRegistry_List(t *testing.T) {
	defs := []ModelDef{
		{Name: "fast", Provider: "openai", Model: "gpt-4o-mini"},
		{Name: "smart", Provider: "anthropic", Model: "claude-sonnet-4-20250514"},
		{Name: "local", Provider: "ollama", Model: "qwen3:32b"},
	}
	r, err := NewModelRegistry(defs, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	list := r.List()
	if len(list) != 3 {
		t.Fatalf("List() returned %d items, want 3", len(list))
	}
	// Verify config order is preserved.
	if list[0].Name != "fast" || list[1].Name != "smart" || list[2].Name != "local" {
		t.Fatalf("List() order = %s, %s, %s; want fast, smart, local",
			list[0].Name, list[1].Name, list[2].Name)
	}
}

func TestModelRegistry_ProviderFor_UnknownAlias(t *testing.T) {
	defs := []ModelDef{
		{Name: "fast", Provider: "openai", Model: "gpt-4o-mini"},
	}
	r, err := NewModelRegistry(defs, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, _, err = r.ProviderFor("nonexistent")
	if err == nil {
		t.Fatal("ProviderFor(nonexistent) should return error")
	}
}

func TestModelRegistry_ProviderFor_CachesByConfig(t *testing.T) {
	defs := []ModelDef{
		{Name: "fast", Provider: "test", Model: "gpt-4o-mini"},
		{Name: "fast2", Provider: "test", Model: "gpt-4o"},
	}
	r, err := NewModelRegistry(defs, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Inject a mock provider for the shared config.
	mock := &unavailableProviderForTest{}
	r.InjectProvider("test", "", "", mock)

	// Both aliases use the same provider config (test, same baseURL, same apiKey).
	// They should share the same cached provider instance.
	p1, _, err := r.ProviderFor("fast")
	if err != nil {
		t.Fatalf("ProviderFor(fast): %v", err)
	}
	p2, _, err := r.ProviderFor("fast2")
	if err != nil {
		t.Fatalf("ProviderFor(fast2): %v", err)
	}
	if p1 != p2 {
		t.Fatal("providers with same config should be cached and identical")
	}
}

func TestModelRegistry_ProviderFor_InjectAndCache(t *testing.T) {
	defs := []ModelDef{
		{Name: "default", Provider: "test", Model: "test-model"},
	}
	r, err := NewModelRegistry(defs, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Inject a mock provider.
	mock := &unavailableProviderForTest{}
	r.InjectProvider("test", "", "", mock)

	// ProviderFor should return the injected provider.
	p, model, err := r.ProviderFor("default")
	if err != nil {
		t.Fatalf("ProviderFor: %v", err)
	}
	if p != mock {
		t.Fatal("ProviderFor should return injected provider")
	}
	if model != "test-model" {
		t.Fatalf("model = %q, want test-model", model)
	}

	// Second call should return the same cached provider.
	p2, _, _ := r.ProviderFor("default")
	if p2 != mock {
		t.Fatal("second ProviderFor should return same cached provider")
	}
}

func TestModelRegistry_ProviderFor_EmptyNameUsesDefault(t *testing.T) {
	defs := []ModelDef{
		{Name: "default", Provider: "test", Model: "test-model"},
	}
	r, err := NewModelRegistry(defs, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mock := &unavailableProviderForTest{}
	r.InjectProvider("test", "", "", mock)

	p, model, err := r.ProviderFor("")
	if err != nil {
		t.Fatalf("ProviderFor(empty): %v", err)
	}
	if p != mock {
		t.Fatal("ProviderFor(empty) should use default model")
	}
	if model != "test-model" {
		t.Fatalf("model = %q, want test-model", model)
	}
}

func TestNewSingleModelRegistry(t *testing.T) {
	r := NewSingleModelRegistry("openai", "gpt-4o", "")
	if r == nil {
		t.Fatal("NewSingleModelRegistry returned nil")
	}
	if r.DefaultName() != "default" {
		t.Fatalf("default = %q, want default", r.DefaultName())
	}
	list := r.List()
	if len(list) != 1 {
		t.Fatalf("List() returned %d items, want 1", len(list))
	}
	if list[0].Provider != "openai" || list[0].Model != "gpt-4o" {
		t.Fatalf("List()[0] = %+v, want openai/gpt-4o", list[0])
	}
}

// unavailableProviderForTest is a minimal LLMProvider for testing that does
// nothing — used to verify provider caching without real API calls.
type unavailableProviderForTest struct{}

func (u *unavailableProviderForTest) Chat(_ context.Context, _ ChatRequest) (ChatResponse, error) {
	return ChatResponse{}, nil
}

func (u *unavailableProviderForTest) Stream(_ context.Context, _ ChatRequest) (<-chan StreamChunk, error) {
	return nil, nil
}
