package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/secret"
)

func TestSaveEnvValueWritesAtomicallyWith0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")

	if err := saveEnvValue(path, "ANTHROPIC_API_KEY", "enc:v1:AAAA"); err != nil {
		t.Fatalf("saveEnvValue: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Errorf("mode = %o, want 600", perm)
	}

	// No temp file may survive: a leftover would be a second copy of the
	// credentials with no cleanup owner.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only .env", names)
	}
}

func TestSaveEnvValueReplacesExistingKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("ANTHROPIC_API_KEY=old\nOTHER=keep\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := saveEnvValue(path, "ANTHROPIC_API_KEY", "new"); err != nil {
		t.Fatalf("saveEnvValue: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if strings.Contains(got, "ANTHROPIC_API_KEY=old") {
		t.Error("old value survived")
	}
	if !strings.Contains(got, "ANTHROPIC_API_KEY=new") {
		t.Errorf("new value missing: %q", got)
	}
	if !strings.Contains(got, "OTHER=keep") {
		t.Errorf("unrelated entry lost: %q", got)
	}
}

func TestSealedValueSurvivesEnvFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")

	sealed, err := secret.Seal("sk-ant-roundtrip-test")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := saveEnvValue(path, "ANTHROPIC_API_KEY", sealed); err != nil {
		t.Fatalf("saveEnvValue: %v", err)
	}

	// The base64url alphabet has no shell metacharacters, so a sealed value
	// must survive .env parsing byte for byte.
	if got := loadEnvValue(path, "ANTHROPIC_API_KEY"); got != sealed {
		t.Errorf("loadEnvValue = %q, want %q", got, sealed)
	}
	plain, err := secret.Reveal(loadEnvValue(path, "ANTHROPIC_API_KEY"))
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	if plain != "sk-ant-roundtrip-test" {
		t.Errorf("Reveal = %q", plain)
	}
}

func TestSealWarningEmptyWhenHardwareBound(t *testing.T) {
	// The dev machine has fixed disks with serials, so no warning is due.
	// On a host without them the warning must be non-empty and must name
	// the mode so the weaker state is never silently assumed.
	got := sealWarning()
	if secret.Fingerprint().Mode == secret.ModeHardware {
		if got != "" {
			t.Errorf("sealWarning = %q, want empty when hardware-bound", got)
		}
		return
	}
	if got == "" {
		t.Error("sealWarning is empty on a host without hardware binding")
	}
}

func TestLoadEnvValueStripsQuotes(t *testing.T) {
	// A key written as KEY="value" must resolve to value (no quotes), the
	// same way goenv's env.Load resolves it for the runtime provider path.
	// If loadEnvValue returned the quotes verbatim, key seal would encrypt
	// the quote characters into the ciphertext and every request would 401.
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := `PLAIN=plain
DOUBLE="double-value"
SINGLE='single-value'
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadEnvValue(path, "PLAIN"); got != "plain" {
		t.Errorf("PLAIN = %q, want plain", got)
	}
	if got := loadEnvValue(path, "DOUBLE"); got != "double-value" {
		t.Errorf("DOUBLE = %q, want double-value (quotes must be stripped)", got)
	}
	if got := loadEnvValue(path, "SINGLE"); got != "single-value" {
		t.Errorf("SINGLE = %q, want single-value (quotes must be stripped)", got)
	}
}
