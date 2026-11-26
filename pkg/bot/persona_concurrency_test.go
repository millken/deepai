package bot

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/tools"
)

func TestConcurrentPersonaToolAndManagerUpdateBotNoLostConnectors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx := context.Background()
	modelReg := llm.NewSingleModelRegistry("test", "m", "")
	rt, err := NewAgentRuntime(AgentRuntimeConfig{
		Models:         modelReg,
		Tools:          tools.NewRegistry(),
		SandboxBaseDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := NewManager(Options{
		Runtime:     rt,
		Store:       NewMemoryStore(),
		Secrets:     NewMemorySecretStore(),
		IdleTimeout: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(ctx)

	if _, err := mgr.CreateBot(ctx, CreateBotRequest{ID: "syncbot", Name: "Sync Bot"}); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.SetSecret(ctx, "syncbot", "FIN_HDR", "hdr_val_alpha_123"); err != nil {
		t.Fatal(err)
	}

	turnCtx := withBotTurnEnv(ctx, botTurnEnv{
		botID:      "syncbot",
		bots:       mgr.bots,
		mutateBot:  mgr.MutateBot,
		secrets:    mgr.Secrets(),
		onBotSaved: mgr.NotifyBotUpdated,
	})

	const workers = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, workers*2)

	for i := 0; i < workers; i++ {
		i := i
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			connName := fmt.Sprintf("conn_%02d", i)
			_, err := UpdateBotPersonaTool().Handler(turnCtx, models.ToolCall{
				ID:   fmt.Sprintf("tc_%02d", i),
				Name: "update_bot_persona",
				Arguments: map[string]any{
					"append_instruction": fmt.Sprintf("rule-%02d", i),
					"upsert_connector": map[string]any{
						"name":     connName,
						"base_url": fmt.Sprintf("%s/v%d", srv.URL, i),
						"headers":  map[string]any{"X-Service-Key": "{{secret:FIN_HDR}}"},
					},
				},
			})
			if err != nil {
				errCh <- fmt.Errorf("persona tool %d: %w", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			title := fmt.Sprintf("Title-%02d", i)
			desc := fmt.Sprintf("Desc-%02d", i)
			if _, err := mgr.UpdateBot(ctx, "syncbot", UpdateBotRequest{
				Title:       &title,
				Description: &desc,
			}); err != nil {
				errCh <- fmt.Errorf("UpdateBot %d: %w", i, err)
			}
		}()
	}

	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	got, err := mgr.GetBot(ctx, "syncbot")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Extensions.Connectors) != workers {
		t.Fatalf("got %d connectors after concurrent updates, want %d", len(got.Extensions.Connectors), workers)
	}
	seen := make(map[string]bool, workers)
	for _, c := range got.Extensions.Connectors {
		seen[c.Name] = true
	}
	for i := 0; i < workers; i++ {
		want := fmt.Sprintf("conn_%02d", i)
		if !seen[want] {
			t.Fatalf("connector %q was lost during concurrent PATCH", want)
		}
		wantRule := fmt.Sprintf("rule-%02d", i)
		if !strings.Contains(got.Instructions, wantRule) {
			t.Fatalf("instruction %q was lost during concurrent updates", wantRule)
		}
	}
}
