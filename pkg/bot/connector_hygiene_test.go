package bot

import (
	"context"
	"errors"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/tools"
)

func TestManagerCreateAndUpdateBotConnectorHygiene(t *testing.T) {
	ctx := context.Background()
	modelReg := llm.NewSingleModelRegistry("test", "m", "")
	rt, _ := NewAgentRuntime(AgentRuntimeConfig{Models: modelReg, Tools: tools.NewRegistry(), SandboxBaseDir: t.TempDir()})
	mgr, err := NewManager(Options{Runtime: rt, Store: NewMemoryStore(), Secrets: NewMemorySecretStore(), IdleTimeout: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(ctx)

	_, err = mgr.CreateBot(ctx, CreateBotRequest{
		ID:   "secbot",
		Name: "Sec Bot",
		Extensions: &BotExtensions{
			Connectors: []APIConnector{{
				Name:    "ext_api",
				BaseURL: "https://api.example.com",
				Headers: map[string]string{"X-Service-Key": "raw_unsealed_value"},
			}},
		},
	})
	if !errors.Is(err, ErrInvalidBot) {
		t.Fatalf("expected ErrInvalidBot for plaintext sensitive header on CreateBot, got %v", err)
	}

	if _, err := mgr.CreateBot(ctx, CreateBotRequest{ID: "secbot", Name: "Sec Bot"}); err != nil {
		t.Fatal(err)
	}
	_, _ = mgr.SetSecret(ctx, "secbot", "FIN_HDR", "k9z")

	instWithVal := "Use k9z on every call."
	if _, err := mgr.UpdateBot(ctx, "secbot", UpdateBotRequest{Instructions: &instWithVal}); !errors.Is(err, ErrInvalidBot) {
		t.Fatalf("expected ErrInvalidBot when stored short secret appears in Instructions, got %v", err)
	}

	updated, err := mgr.UpdateBot(ctx, "secbot", UpdateBotRequest{
		Extensions: &BotExtensions{
			Connectors: []APIConnector{{
				Name:    "ext_api",
				BaseURL: "https://api.example.com/v1",
				Headers: map[string]string{"X-Service-Key": "{{secret:FIN_HDR}}"},
			}},
		},
	})
	if err != nil || len(updated.Extensions.Connectors) != 1 {
		t.Fatalf("expected valid connector to succeed, got updated=%+v err=%v", updated, err)
	}
}
