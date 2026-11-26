package bot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/millken/deepai/pkg/models"
)

type errSecretStore struct {
	SecretStore
	listErr error
	getErr  error
}

func (e *errSecretStore) ListSecrets(ctx context.Context, botID string) ([]SecretMeta, error) {
	if e.listErr != nil {
		return nil, e.listErr
	}
	return e.SecretStore.ListSecrets(ctx, botID)
}

func (e *errSecretStore) GetSecret(ctx context.Context, botID, name string) (string, error) {
	if e.getErr != nil {
		return "", e.getErr
	}
	return e.SecretStore.GetSecret(ctx, botID, name)
}

func TestRejectPlaintextSecretsFailsClosedAndCatchesShortSecrets(t *testing.T) {
	ctx := context.Background()
	botDataDir := filepath.Join(t.TempDir(), "bots")
	memStore := NewMemoryStore()
	_ = memStore.EnsureDefault(ctx)
	_, _ = memStore.CreateBot(ctx, BotProfile{ID: "demobot", Name: "Demo Bot", Enabled: true})

	baseSecrets := NewMemorySecretStore()
	if _, err := baseSecrets.SetSecret(ctx, "demobot", "FIN_HDR", "k9z"); err != nil {
		t.Fatal(err)
	}

	listFailEnv := botTurnEnv{
		botID:      "demobot",
		bots:       memStore,
		botDataDir: botDataDir,
		secrets:    &errSecretStore{SecretStore: baseSecrets, listErr: errors.New("store list failure")},
	}
	if err := rejectPlaintextSecrets(ctx, listFailEnv, "normal text"); err == nil || !strings.Contains(err.Error(), "store list failure") {
		t.Fatalf("expected fail-closed error on ListSecrets failure, got %v", err)
	}
	if _, err := SaveBotSkillTool().Handler(withBotTurnEnv(ctx, listFailEnv), models.ToolCall{
		ID:   "s_list_err",
		Name: "save_bot_skill",
		Arguments: map[string]any{
			"name":        "should-not-write",
			"description": "desc",
			"content":     "value k9z",
		},
	}); err == nil || !strings.Contains(err.Error(), "store list failure") {
		t.Fatalf("expected save_bot_skill to fail closed, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(botDataDir, "demobot")); !os.IsNotExist(statErr) {
		t.Fatalf("bot directory was created despite ListSecrets error: %v", statErr)
	}

	getFailEnv := listFailEnv
	getFailEnv.secrets = &errSecretStore{SecretStore: baseSecrets, getErr: errors.New("store read failure")}
	if err := rejectPlaintextSecrets(ctx, getFailEnv, "normal text"); err == nil || !strings.Contains(err.Error(), "store read failure") {
		t.Fatalf("expected fail-closed error on GetSecret failure, got %v", err)
	}

	okEnv := botTurnEnv{
		botID:      "demobot",
		bots:       memStore,
		botDataDir: botDataDir,
		secrets:    baseSecrets,
	}
	if _, err := SaveBotSkillTool().Handler(withBotTurnEnv(ctx, okEnv), models.ToolCall{
		ID:   "s_short",
		Name: "save_bot_skill",
		Arguments: map[string]any{
			"name":        "short-leak",
			"description": "desc",
			"content":     "Header value k9z in call.",
		},
	}); err == nil || !strings.Contains(err.Error(), "FIN_HDR") {
		t.Fatalf("expected 3-char secret rejection in save_bot_skill, got %v", err)
	}
	if _, err := UpdateBotPersonaTool().Handler(withBotTurnEnv(ctx, okEnv), models.ToolCall{
		ID:        "p_short",
		Name:      "update_bot_persona",
		Arguments: map[string]any{"append_instruction": "Send k9z."},
	}); err == nil || !strings.Contains(err.Error(), "FIN_HDR") {
		t.Fatalf("expected 3-char secret rejection in update_bot_persona, got %v", err)
	}
}
