package llm

import (
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/secret"
)

func TestResolveAPIKeyRevealsSealedValue(t *testing.T) {
	sealed, err := secret.Seal("sk-ant-sealed-value")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	t.Setenv("SEAL_TEST_KEY", sealed)

	got, err := resolveAPIKey(ModelDef{Provider: "anthropic", APIKeyEnv: "SEAL_TEST_KEY"})
	if err != nil {
		t.Fatalf("resolveAPIKey: %v", err)
	}
	if got != "sk-ant-sealed-value" {
		t.Errorf("resolveAPIKey = %q, want the revealed key", got)
	}
}

func TestResolveAPIKeyPassesThroughPlaintext(t *testing.T) {
	t.Setenv("SEAL_TEST_KEY", "sk-plaintext-still-works")

	got, err := resolveAPIKey(ModelDef{Provider: "anthropic", APIKeyEnv: "SEAL_TEST_KEY"})
	if err != nil {
		t.Fatalf("resolveAPIKey: %v", err)
	}
	if got != "sk-plaintext-still-works" {
		t.Errorf("resolveAPIKey = %q, want the plaintext key unchanged", got)
	}
}

func TestResolveAPIKeyErrorsOnUndecryptableValue(t *testing.T) {
	// A value sealed on another machine, or a future format. Either way the
	// caller must see an error rather than an empty key that yields a
	// baffling 401 from the provider.
	t.Setenv("SEAL_TEST_KEY", "enc:v1:AAAAAAAAAAAAAAAAAAAAAAAA")

	got, err := resolveAPIKey(ModelDef{Provider: "anthropic", APIKeyEnv: "SEAL_TEST_KEY"})
	if err == nil {
		t.Fatalf("resolveAPIKey returned %q with no error", got)
	}
	if got != "" {
		t.Errorf("resolveAPIKey = %q, want empty on error", got)
	}
}

func TestProviderForErrorsRatherThanUsingEmptyKey(t *testing.T) {
	t.Setenv("SEAL_TEST_KEY", "enc:v1:AAAAAAAAAAAAAAAAAAAAAAAA")

	r, err := NewModelRegistry([]ModelDef{{
		Name: "broken", Provider: "anthropic", Model: "claude-sonnet-4-20250514",
		APIKeyEnv: "SEAL_TEST_KEY",
	}}, "broken")
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}

	if _, _, err := r.ProviderFor("broken"); err == nil {
		t.Fatal("ProviderFor succeeded with an undecryptable key")
	}
}

func TestProviderCacheKeyOmitsPlaintextKey(t *testing.T) {
	const plain = "sk-ant-super-secret-value"
	def := ModelDef{Provider: "anthropic", BaseURL: "https://example.test"}

	got := providerCacheKey(def, plain)
	if strings.Contains(got, plain) {
		t.Errorf("cache key %q contains the plaintext API key", got)
	}
	if got == providerCacheKey(def, "sk-a-different-key") {
		t.Error("cache key must still distinguish different keys")
	}
	if got != providerCacheKey(def, plain) {
		t.Error("cache key must be stable for the same inputs")
	}
}

func TestInjectProviderMatchesCacheKey(t *testing.T) {
	// InjectProvider builds the cache key by hand; it must agree with
	// providerCacheKey or test injection silently stops working.
	const plain = "sk-inject-test"
	r, err := NewModelRegistry([]ModelDef{{
		Name: "m", Provider: "anthropic", Model: "claude-sonnet-4-20250514",
		APIKeyEnv: "SEAL_INJECT_KEY",
	}}, "m")
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	t.Setenv("SEAL_INJECT_KEY", plain)

	sentinel := &UnavailableProvider{err: errSentinel}
	r.InjectProvider("anthropic", "", plain, sentinel)

	p, _, err := r.ProviderFor("m")
	if err != nil {
		t.Fatalf("ProviderFor: %v", err)
	}
	if p != sentinel {
		t.Error("ProviderFor did not return the injected provider")
	}
}

var errSentinel = errTest("sentinel")

type errTest string

func (e errTest) Error() string { return string(e) }
