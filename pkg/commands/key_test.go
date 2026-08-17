package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/secret"
)

func TestApiKeyVarNamesCoversProviders(t *testing.T) {
	names := apiKeyVarNames(Config{})
	for _, want := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "DEEPSEEK_API_KEY"} {
		if !names[want] {
			t.Errorf("%s missing from the sealable set", want)
		}
	}
	if names["ANTHROPIC_BASE_URL"] {
		t.Error("base URLs must not be sealed")
	}
}

func TestParseEnvFileKeepsCommentsAndOrder(t *testing.T) {
	content := "# a comment\nANTHROPIC_API_KEY=sk-one\n\nOPENAI_API_KEY=sk-two\n"
	got := parseEnvFile(content)

	if len(got) != 4 {
		t.Fatalf("len = %d, want 4: %+v", len(got), got)
	}
	if got[0].Key != "" || got[0].Line != "# a comment" {
		t.Errorf("comment line = %+v", got[0])
	}
	if got[1].Key != "ANTHROPIC_API_KEY" || got[1].Value != "sk-one" {
		t.Errorf("first entry = %+v", got[1])
	}
	if got[3].Key != "OPENAI_API_KEY" || got[3].Value != "sk-two" {
		t.Errorf("last entry = %+v", got[3])
	}
}

func TestSealEnvFileSealsPlaintextKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "# keep me\nANTHROPIC_API_KEY=sk-ant-plain\nANTHROPIC_BASE_URL=https://example.test\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	n, err := sealEnvFile(path)
	if err != nil {
		t.Fatalf("sealEnvFile: %v", err)
	}
	if n != 1 {
		t.Errorf("sealed %d entries, want 1", n)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if strings.Contains(got, "sk-ant-plain") {
		t.Error("plaintext key survived in .env")
	}
	if !strings.Contains(got, "# keep me") {
		t.Error("comment lost")
	}
	if !strings.Contains(got, "ANTHROPIC_BASE_URL=https://example.test") {
		t.Error("non-key entry was altered")
	}
	if plain, err := secret.Reveal(loadEnvValue(path, "ANTHROPIC_API_KEY")); err != nil || plain != "sk-ant-plain" {
		t.Errorf("Reveal = %q, %v; want the original key", plain, err)
	}
}

func TestSealEnvFileLeavesNoPlaintextResidue(t *testing.T) {
	// Backing up to .env.bak would leave a plaintext copy behind -- and
	// .env.bak is not in .gitignore, making it more dangerous than the
	// original. Nothing but .env may remain.
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("ANTHROPIC_API_KEY=sk-ant-plain\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := sealEnvFile(path); err != nil {
		t.Fatalf("sealEnvFile: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != ".env" {
			t.Errorf("unexpected file %q left behind", e.Name())
			continue
		}
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "sk-ant-plain") {
			t.Errorf("%s contains the plaintext key", e.Name())
		}
	}
}

func TestSealEnvFileSkipsAlreadySealed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	sealed, err := secret.Seal("sk-ant-already")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := os.WriteFile(path, []byte("ANTHROPIC_API_KEY="+sealed+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	n, err := sealEnvFile(path)
	if err != nil {
		t.Fatalf("sealEnvFile: %v", err)
	}
	if n != 0 {
		t.Errorf("sealed %d entries, want 0", n)
	}

	if got := loadEnvValue(path, "ANTHROPIC_API_KEY"); got != sealed {
		t.Error("an already-sealed value was re-sealed")
	}
}

func TestSealEnvFileLeavesFileUntouchedOnVerifyFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	const original = "ANTHROPIC_API_KEY=sk-ant-plain\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}

	// Force the roundtrip check to fail, standing in for a broken
	// fingerprint layer. The file must not be rewritten.
	prev := sealFn
	sealFn = func(string) (string, error) { return "enc:v1:not-a-real-blob", nil }
	t.Cleanup(func() { sealFn = prev })

	if _, err := sealEnvFile(path); err == nil {
		t.Fatal("sealEnvFile succeeded despite a failed roundtrip check")
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != original {
		t.Errorf("file was modified: %q, want %q", string(b), original)
	}
}

func TestSealEnvFileMissingFile(t *testing.T) {
	n, err := sealEnvFile(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Errorf("sealEnvFile on a missing file returned %v, want nil", err)
	}
	if n != 0 {
		t.Errorf("sealed %d entries, want 0", n)
	}
}

func TestApiKeyVarNamesIncludesCustomAPIKeyEnv(t *testing.T) {
	cfg := Config{Models: []llm.ModelDef{{
		Name: "claude", Provider: "anthropic", Model: "claude-opus-4-8",
		APIKeyEnv: "ANTHROPIC_RELAY_API_KEY",
	}}}
	if !apiKeyVarNames(cfg)["ANTHROPIC_RELAY_API_KEY"] {
		t.Error("models[].api_key_env variable missing from the sealable set")
	}
}

func TestSealEnvFileSealsCustomAPIKeyEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "ANTHROPIC_RELAY_API_KEY=relay-plain\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	n, err := sealEntries(path, parseEnvFile(content), map[string]bool{"ANTHROPIC_RELAY_API_KEY": true})
	if err != nil {
		t.Fatalf("sealEntries: %v", err)
	}
	if n != 1 {
		t.Fatalf("sealed %d entries, want 1", n)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "relay-plain") {
		t.Error("plaintext custom key survived in .env")
	}
	if plain, err := secret.Reveal(loadEnvValue(path, "ANTHROPIC_RELAY_API_KEY")); err != nil || plain != "relay-plain" {
		t.Errorf("Reveal = %q, %v; want the original key", plain, err)
	}
}
