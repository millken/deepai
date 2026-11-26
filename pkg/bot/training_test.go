package bot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/skill"
	"github.com/millken/deepai/pkg/tools"
)

type scriptedTrainProvider struct {
	crmBaseURL   string
	mu           sync.Mutex
	seenRequests []llm.ChatRequest
	step         int
}

func (p *scriptedTrainProvider) Chat(context.Context, llm.ChatRequest) (llm.ChatResponse, error) {
	return llm.ChatResponse{}, nil
}

func (p *scriptedTrainProvider) Stream(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	p.mu.Lock()
	p.seenRequests = append(p.seenRequests, req)
	p.step++
	step := p.step
	p.mu.Unlock()

	ch := make(chan llm.StreamChunk, 2)
	switch step {
	case 1:
		ch <- llm.StreamChunk{
			ToolCalls: []models.ToolCall{
				{
					ID:   "tc_persona",
					Name: "update_bot_persona",
					Arguments: map[string]any{
						"isolated_prompt":    true,
						"append_instruction": "Always sync daily notes via crm_api.",
						"upsert_connector": map[string]any{
							"name":     "crm_api",
							"base_url": p.crmBaseURL,
							"headers": map[string]any{
								"Authorization": "Bearer {{secret:CRM_TOKEN}}",
							},
						},
					},
				},
				{
					ID:   "tc_skill",
					Name: "save_bot_skill",
					Arguments: map[string]any{
						"name":        "crm-daily-sync",
						"description": "Use when syncing CRM contacts via crm_api.",
						"content":     "Call `http_request` with connector `crm_api` and path `/contacts`.",
					},
				},
			},
			Stop: "tool_calls",
			Done: true,
		}
	case 2:
		ch <- llm.StreamChunk{Delta: "trained!", Stop: "stop", Done: true}
	case 3:
		ch <- llm.StreamChunk{
			ToolCalls: []models.ToolCall{
				{
					ID:        "tc_load_skill",
					Name:      "skill",
					Arguments: map[string]any{"name": "crm-daily-sync"},
				},
				{
					ID:   "tc_http",
					Name: "http_request",
					Arguments: map[string]any{
						"connector": "crm_api",
						"url":       "/contacts",
					},
				},
			},
			Stop: "tool_calls",
			Done: true,
		}
	case 4:
		ch <- llm.StreamChunk{Delta: "synced contacts!", Stop: "stop", Done: true}
	default:
		ch <- llm.StreamChunk{Delta: "hello from coder", Stop: "stop", Done: true}
	}
	close(ch)
	return ch, nil
}

func TestConversationalSelfTrainingEndToEndAndBotIsolation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()

	var crmHits int
	var crmAuthHeader string
	crmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/contacts" {
			crmHits++
			crmAuthHeader = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"contacts":["Ada"],"echo":"crm_val_7777"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer crmServer.Close()

	baseSkills := skill.NewRegistry()
	wsSkillDir := filepath.Join(t.TempDir(), "ws-skills", "repo-lint")
	_ = os.MkdirAll(wsSkillDir, 0o755)
	_ = os.WriteFile(filepath.Join(wsSkillDir, "SKILL.md"), []byte("---\nname: repo-lint\ndescription: Workspace lint helper\n---\nRun golangci-lint.\n"), 0o644)
	_ = baseSkills.LoadFromDir(filepath.Dir(wsSkillDir))

	prov := &scriptedTrainProvider{crmBaseURL: crmServer.URL}
	modelReg := llm.NewSingleModelRegistry("test", "m", "")
	modelReg.InjectProvider("test", "", "", prov)

	botDataDir := filepath.Join(t.TempDir(), "bots")
	memStore := NewMemoryStore()
	secStore := NewMemorySecretStore()

	rt, err := NewAgentRuntime(AgentRuntimeConfig{
		Models:             modelReg,
		Tools:              tools.NewRegistry(),
		BaseSkills:         baseSkills,
		GlobalInstructions: "WORKSPACE_CODE_INSTRUCTION_MARKER",
		WorkDir:            t.TempDir(),
		SandboxBaseDir:     t.TempDir(),
		BotDataDir:         botDataDir,
		AllowLoopbackHTTP:  true,
	})
	if err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(Options{
		Runtime:     rt,
		Store:       memStore,
		Secrets:     secStore,
		BotDataDir:  botDataDir,
		IdleTimeout: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close(ctx)

	_, err = mgr.CreateBot(ctx, CreateBotRequest{ID: "sales", Name: "Sales Bot"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = mgr.CreateBot(ctx, CreateBotRequest{ID: "coder", Name: "Coder Bot"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := mgr.SetSecret(ctx, "sales", "CRM_TOKEN", "crm_val_7777"); err != nil {
		t.Fatal(err)
	}

	// Guard checks: raw secret value leak & unattended schedule run block.
	leakCtx := withBotTurnEnv(ctx, botTurnEnv{
		botID:      "sales",
		secrets:    secStore,
		bots:       memStore,
		botDataDir: botDataDir,
	})
	if _, err := SaveBotSkillTool().Handler(leakCtx, models.ToolCall{
		ID:   "leak",
		Name: "save_bot_skill",
		Arguments: map[string]any{
			"name":        "bad-skill",
			"description": "desc",
			"content":     "Header value crm_val_7777",
		},
	}); err == nil || !strings.Contains(err.Error(), "plaintext value of secret") {
		t.Fatalf("expected plaintext secret leak rejection, got err=%v", err)
	}
	if _, err := SaveBotSkillTool().Handler(leakCtx, models.ToolCall{
		ID:   "dyn_shell",
		Name: "save_bot_skill",
		Arguments: map[string]any{
			"name":        "dyn-skill",
			"description": "desc",
			"content":     "Output: !`uname -a`",
		},
	}); err == nil || !strings.Contains(err.Error(), "dynamic shell execution") {
		t.Fatalf("expected dynamic shell block rejection, got err=%v", err)
	}
	schedCtx := tools.WithScheduleRun(leakCtx, "sch_1", "run_1")
	if _, err := UpdateBotPersonaTool().Handler(schedCtx, models.ToolCall{
		ID:        "sched_block",
		Name:      "update_bot_persona",
		Arguments: map[string]any{"append_instruction": "mutated"},
	}); err == nil || !strings.Contains(err.Error(), "scheduled") {
		t.Fatalf("expected scheduled run training block, got err=%v", err)
	}

	// Turn 1 on "sales": conversational training updates persona/connector & writes SKILL.md.
	sSales, err := mgr.CreateSessionForBot("sales", CreateSessionRequest{Title: "train"})
	if err != nil {
		t.Fatal(err)
	}
	t1, err := mgr.SendForBot("sales", sSales.ID, SendRequest{Content: crmServer.URL}, SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-t1.Done()
	if t1.Summary().Status != TurnCompleted {
		t.Fatalf("Turn 1 summary = %+v", t1.Summary())
	}

	updatedSales, err := mgr.GetBot(ctx, "sales")
	if err != nil {
		t.Fatal(err)
	}
	if !updatedSales.Extensions.IsolatedPrompt || len(updatedSales.Extensions.Connectors) != 1 || updatedSales.Extensions.Connectors[0].Name != "crm_api" {
		t.Fatalf("updatedSales after Turn 1 = %+v", updatedSales)
	}

	// Turn 2 on "sales": IsolatedPrompt drops workspace code prompt, exposes crm-daily-sync, and calls crm_api!
	t2, err := mgr.SendForBot("sales", sSales.ID, SendRequest{Content: "sync now"}, SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-t2.Done()
	if t2.Summary().Status != TurnCompleted || t2.Summary().FinalOutput != "synced contacts!" {
		t.Fatalf("Turn 2 summary = %+v", t2.Summary())
	}
	if crmHits != 1 || crmAuthHeader != "Bearer crm_val_7777" {
		t.Fatalf("crmHits=%d auth=%q", crmHits, crmAuthHeader)
	}

	// Turn 1 on "coder": completely isolated from sales's skills, connectors, and secrets.
	sCoder, err := mgr.CreateSessionForBot("coder", CreateSessionRequest{Title: "code"})
	if err != nil {
		t.Fatal(err)
	}
	t3, err := mgr.SendForBot("coder", sCoder.ID, SendRequest{Content: "check"}, SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-t3.Done()

	prov.mu.Lock()
	reqs := append([]llm.ChatRequest(nil), prov.seenRequests...)
	prov.mu.Unlock()
	if len(reqs) < 5 {
		t.Fatalf("captured %d LLM requests, want >= 5", len(reqs))
	}

	salesTurn2Sys := systemPromptOf(reqs[2])
	if strings.Contains(salesTurn2Sys, "WORKSPACE_CODE_INSTRUCTION_MARKER") || strings.Contains(salesTurn2Sys, "repo-lint") {
		t.Fatalf("IsolatedPrompt failed to strip workspace prompt/skills on sales Turn 2:\n%s", salesTurn2Sys)
	}
	if !strings.Contains(salesTurn2Sys, "crm-daily-sync") || !strings.Contains(salesTurn2Sys, "{{secret:CRM_TOKEN}}") || strings.Contains(salesTurn2Sys, "crm_val_7777") {
		t.Fatalf("sales Turn 2 system prompt missing bot skill or capability summary:\n%s", salesTurn2Sys)
	}
	for _, m := range reqs[3].Messages {
		if strings.Contains(m.Content, "crm_val_7777") {
			t.Fatalf("LLM transcript on sales Turn 2 leaked echoed secret: %+v", m)
		}
	}

	coderSys := systemPromptOf(reqs[4])
	if !strings.Contains(coderSys, "WORKSPACE_CODE_INSTRUCTION_MARKER") || !strings.Contains(coderSys, "repo-lint") {
		t.Fatalf("coder bot lost workspace instructions/skills:\n%s", coderSys)
	}
	if strings.Contains(coderSys, "crm-daily-sync") || strings.Contains(coderSys, "CRM_TOKEN") || strings.Contains(coderSys, "crm_api") {
		t.Fatalf("sales bot skills/connectors/secrets leaked into coder bot:\n%s", coderSys)
	}
	coderSkillNames := skillEnumOf(reqs[4].Tools)
	if slices.Contains(coderSkillNames, "crm-daily-sync") || !slices.Contains(coderSkillNames, "repo-lint") {
		t.Fatalf("coder skill tool enum = %v, want [repo-lint] without crm-daily-sync", coderSkillNames)
	}

	salesSkillFile := filepath.Join(botDataDir, "sales", "skills", "crm-daily-sync", "SKILL.md")
	if _, err := os.Stat(salesSkillFile); err != nil {
		t.Fatalf("expected saved skill file at %s: %v", salesSkillFile, err)
	}
	if err := mgr.DeleteBot(ctx, "sales", true); err != nil {
		t.Fatalf("DeleteBot(sales): %v", err)
	}
	if _, err := os.Stat(filepath.Join(botDataDir, "sales")); !os.IsNotExist(err) {
		t.Fatalf("per-bot data directory was not removed on DeleteBot: err=%v", err)
	}
	if rem, _ := secStore.ListSecrets(ctx, "sales"); len(rem) != 0 {
		t.Fatalf("secrets remained after DeleteBot: %+v", rem)
	}
}

func systemPromptOf(req llm.ChatRequest) string {
	var parts []string
	if req.SystemPrompt != "" {
		parts = append(parts, req.SystemPrompt)
	}
	for _, m := range req.Messages {
		if m.Role == models.RoleSystem {
			parts = append(parts, m.Content)
		}
	}
	return strings.Join(parts, "\n")
}

func skillEnumOf(toolList []models.Tool) []string {
	for _, t := range toolList {
		if t.Name != "skill" {
			continue
		}
		props, _ := t.InputSchema["properties"].(map[string]any)
		nameProp, _ := props["name"].(map[string]any)
		switch raw := nameProp["enum"].(type) {
		case []string:
			return raw
		case []any:
			out := make([]string, 0, len(raw))
			for _, v := range raw {
				if s, ok := v.(string); ok {
					out = append(out, s)
				}
			}
			return out
		}
	}
	return nil
}
