package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/secret"
	"github.com/millken/deepai/pkg/tools"
)

const maxBotSkillBytes = 64 << 10

var (
	botSkillNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,47}$`)
	skillInjectionREs   = []*regexp.Regexp{
		regexp.MustCompile(`(?i)ignore\s+(previous|all|above|prior)\s+instructions`),
		regexp.MustCompile(`(?i)system\s*:\s*you\s+are`),
		regexp.MustCompile("[\u200B\u200C\u200D\u200E\u200F\u2060\uFEFF\u202A-\u202E\u2066-\u2069]"),
	}
)

// SaveBotSkillTool lets a bot crystallize domain workflows or third-party API
// manuals taught during conversation into its own isolated `~/.deepai/bot/bots/<id>/skills/<name>/SKILL.md`.
func SaveBotSkillTool() models.Tool {
	return models.Tool{
		Name:        "save_bot_skill",
		Description: "Persist or update a reusable skill playbook scoped exclusively to THIS bot (invisible to other bots). Use when the user teaches you a third-party API specification, SOP workflow, or domain playbook that exceeds short-term facts. Never write raw secret values — reference credentials via '{{secret:NAME}}'.",
		Groups:      []string{"bot", "training"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "Skill slug matching ^[a-z0-9][a-z0-9_-]{0,47}$ (e.g. 'notion-daily-sync').",
				},
				"description": map[string]any{
					"type":        "string",
					"description": "Concise single-line trigger description stating when this bot should load and apply the skill.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "Markdown skill instructions, endpoints, '{{secret:NAME}}' header/query templates, and SOP steps.",
				},
			},
			"required": []any{"name", "description", "content"},
		},
		Handler: handleSaveBotSkillTool,
	}
}

// UpdateBotPersonaTool lets a bot evolve its durable persona (`BotProfile.Instructions`)
// and register/update bound third-party `APIConnector` definitions during interactive training chats.
func UpdateBotPersonaTool() models.Tool {
	return models.Tool{
		Name:        "update_bot_persona",
		Description: "Evolve THIS bot's durable persona rules (instructions) or register/update a third-party APIConnector in its profile when explicitly taught by the user during conversation. Disabled automatically during unattended scheduled runs.",
		Groups:      []string{"bot", "training"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"instructions": map[string]any{
					"type":        "string",
					"description": "Optional full replacement for this bot's durable instructions.",
				},
				"append_instruction": map[string]any{
					"type":        "string",
					"description": "Optional standing rule or persona trait to append to this bot's existing instructions.",
				},
				"isolated_prompt": map[string]any{
					"type":        "boolean",
					"description": "Optional: set true to isolate this bot from workspace code instructions (DEEPAI.md).",
				},
				"upsert_connector": map[string]any{
					"type":        "object",
					"description": "Optional third-party API connector to add or update on this bot (use '{{secret:NAME}}' in headers/query_params).",
					"properties": map[string]any{
						"name":          map[string]any{"type": "string"},
						"base_url":      map[string]any{"type": "string"},
						"allowed_hosts": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"headers":       map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
						"query_params":  map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
						"timeout_sec":   map[string]any{"type": "integer"},
					},
					"required": []any{"name", "base_url"},
				},
				"remove_connector": map[string]any{
					"type":        "string",
					"description": "Optional name of a connector to remove from this bot.",
				},
			},
		},
		Handler: handleUpdateBotPersonaTool,
	}
}

func checkSelfTrainingAllowed(ctx context.Context) (botTurnEnv, error) {
	if tools.IsScheduledRun(ctx) {
		return botTurnEnv{}, errors.New("self-training tools cannot be called during unattended scheduled or webhook runs")
	}
	env, ok := botTurnEnvFromContext(ctx)
	if !ok {
		return botTurnEnv{}, errors.New("bot training context is unavailable outside deepai-bot")
	}
	if env.extensions.DisableSelfTrain {
		return botTurnEnv{}, fmt.Errorf("self-training is disabled on bot %q", effectiveBotID(env.botID))
	}
	return env, nil
}

func rejectPlaintextSecrets(ctx context.Context, env botTurnEnv, texts ...string) error {
	for _, t := range texts {
		for _, re := range skillInjectionREs {
			if re.MatchString(t) {
				return errors.New("content rejected due to unsafe prompt-injection or invisible unicode pattern")
			}
		}
		if secret.IsSealed(t) || strings.Contains(t, "enc:v1:") {
			return errors.New("do not embed sealed ciphertexts; use '{{secret:NAME}}' placeholders")
		}
	}
	if env.secrets == nil {
		return nil
	}
	botID := effectiveBotID(env.botID)
	metas, err := env.secrets.ListSecrets(ctx, botID)
	if err != nil {
		return nil
	}
	for _, m := range metas {
		val, err := env.secrets.GetSecret(ctx, botID, m.Name)
		if err != nil || len(val) < 4 {
			continue
		}
		for _, t := range texts {
			if strings.Contains(t, val) {
				return fmt.Errorf("plaintext value of secret %q detected; replace it with '{{secret:%s}}'", m.Name, m.Name)
			}
		}
	}
	return nil
}

func handleSaveBotSkillTool(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	fail := func(err error) (models.ToolResult, error) {
		return models.ToolResult{
			CallID:   call.ID,
			ToolName: call.Name,
			Status:   models.CallStatusFailed,
			Error:    err.Error(),
		}, err
	}

	env, err := checkSelfTrainingAllowed(ctx)
	if err != nil {
		return fail(err)
	}
	if strings.TrimSpace(env.botDataDir) == "" {
		return fail(errors.New("per-bot skill persistence is unavailable in ephemeral mode"))
	}

	name, _ := call.Arguments["name"].(string)
	name = strings.TrimSpace(name)
	if !botSkillNamePattern.MatchString(name) {
		return fail(fmt.Errorf("skill name %q must match ^[a-z0-9][a-z0-9_-]{0,47}$", name))
	}
	desc, _ := call.Arguments["description"].(string)
	desc = strings.Join(strings.Fields(strings.TrimSpace(desc)), " ")
	if desc == "" || len([]rune(desc)) > 300 {
		return fail(errors.New("description is required and must be at most 300 characters"))
	}
	body, _ := call.Arguments["content"].(string)
	body = strings.TrimSpace(body)
	if body == "" || len(body) > maxBotSkillBytes {
		return fail(fmt.Errorf("content is required and must be at most %d bytes", maxBotSkillBytes))
	}
	if strings.Contains(body, "!`") {
		return fail(errors.New("dynamic shell execution blocks (!`...`) are forbidden in self-trained bot skills; use http_request instead"))
	}
	if err := rejectPlaintextSecrets(ctx, env, desc, body); err != nil {
		return fail(err)
	}

	botID := effectiveBotID(env.botID)
	skillDir := filepath.Join(env.botDataDir, botID, "skills", name)
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		return fail(fmt.Errorf("create bot skill dir: %w", err))
	}

	descJSON, _ := json.Marshal(desc)
	doc := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s\n", name, string(descJSON), body)
	target := filepath.Join(skillDir, "SKILL.md")
	tmp := fmt.Sprintf("%s.%d.tmp", target, time.Now().UnixNano())
	if err := os.WriteFile(tmp, []byte(doc), 0o600); err != nil {
		return fail(fmt.Errorf("write SKILL.md: %w", err))
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fail(fmt.Errorf("commit SKILL.md: %w", err))
	}

	env.audit.Record(AuditEntry{
		Action:  "bot_skill_saved",
		Actor:   "bot:" + botID,
		BotID:   botID,
		Target:  name,
		Outcome: "ok",
	})
	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Status:   models.CallStatusCompleted,
		Content:  fmt.Sprintf("Saved bot-exclusive skill %q for bot %q (%s).", name, botID, target),
	}, nil
}

func handleUpdateBotPersonaTool(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
	fail := func(err error) (models.ToolResult, error) {
		return models.ToolResult{
			CallID:   call.ID,
			ToolName: call.Name,
			Status:   models.CallStatusFailed,
			Error:    err.Error(),
		}, err
	}

	env, err := checkSelfTrainingAllowed(ctx)
	if err != nil {
		return fail(err)
	}
	if env.bots == nil {
		return fail(errors.New("bot registry is not configured on this runtime"))
	}

	botID := effectiveBotID(env.botID)
	cur, err := env.bots.GetBot(ctx, botID)
	if err != nil {
		return fail(err)
	}

	changed := false
	var checkStrings []string

	if rawInst, exists := call.Arguments["instructions"]; exists && rawInst != nil {
		inst, ok := rawInst.(string)
		if !ok {
			return fail(errors.New("instructions must be a string"))
		}
		cur.Instructions = strings.TrimSpace(inst)
		checkStrings = append(checkStrings, cur.Instructions)
		changed = true
	}
	if addInst, ok := call.Arguments["append_instruction"].(string); ok && strings.TrimSpace(addInst) != "" {
		addInst = strings.TrimSpace(addInst)
		checkStrings = append(checkStrings, addInst)
		if strings.TrimSpace(cur.Instructions) == "" {
			cur.Instructions = addInst
		} else {
			cur.Instructions = strings.TrimSpace(cur.Instructions) + "\n\n" + addInst
		}
		changed = true
	}
	if iso, ok := call.Arguments["isolated_prompt"].(bool); ok {
		cur.Extensions.IsolatedPrompt = iso
		changed = true
	}
	if rmConn, ok := call.Arguments["remove_connector"].(string); ok && strings.TrimSpace(rmConn) != "" {
		rmConn = strings.TrimSpace(rmConn)
		kept := cur.Extensions.Connectors[:0]
		for _, c := range cur.Extensions.Connectors {
			if !strings.EqualFold(c.Name, rmConn) {
				kept = append(kept, c)
			}
		}
		cur.Extensions.Connectors = kept
		changed = true
	}
	if rawConn, ok := call.Arguments["upsert_connector"].(map[string]any); ok && len(rawConn) > 0 {
		rawBytes, err := json.Marshal(rawConn)
		if err != nil {
			return fail(fmt.Errorf("encode upsert_connector: %w", err))
		}
		var conn APIConnector
		if err := json.Unmarshal(rawBytes, &conn); err != nil {
			return fail(fmt.Errorf("parse upsert_connector: %w", err))
		}
		checkStrings = append(checkStrings, string(rawBytes))
		replaced := false
		for i := range cur.Extensions.Connectors {
			if strings.EqualFold(cur.Extensions.Connectors[i].Name, conn.Name) {
				cur.Extensions.Connectors[i] = conn
				replaced = true
				break
			}
		}
		if !replaced {
			cur.Extensions.Connectors = append(cur.Extensions.Connectors, conn)
		}
		changed = true
	}

	if !changed {
		return fail(errors.New("provide at least one field to update (instructions, append_instruction, isolated_prompt, upsert_connector, or remove_connector)"))
	}
	if err := rejectPlaintextSecrets(ctx, env, checkStrings...); err != nil {
		return fail(err)
	}

	cur.Extensions = normalizeBotExtensions(cur.Extensions)
	cur.UpdatedAt = time.Now().UTC()
	if err := validateBotProfile(cur, false); err != nil {
		return fail(err)
	}
	updated, err := env.bots.UpdateBot(ctx, cur)
	if err != nil {
		return fail(err)
	}
	if env.onBotSaved != nil {
		env.onBotSaved(updated.Clone())
	}
	env.audit.Record(AuditEntry{
		Action:  "bot_persona_updated",
		Actor:   "bot:" + botID,
		BotID:   botID,
		Target:  botID,
		Outcome: "ok",
	})

	return models.ToolResult{
		CallID:   call.ID,
		ToolName: call.Name,
		Status:   models.CallStatusCompleted,
		Content:  fmt.Sprintf("Updated persona/connectors for bot %q (connectors=%d, isolated_prompt=%v). Takes full effect on the next turn.", botID, len(updated.Extensions.Connectors), updated.Extensions.IsolatedPrompt),
	}, nil
}

// botCapabilitiesPrompt renders a deterministic, byte-stable summary of the
// bot's configured connectors and secret placeholder names (never values) so
// both interactive and scheduled turns know which credentials/APIs they have.
func botCapabilitiesPrompt(ctx context.Context, botID string, ext BotExtensions, secrets SecretStore) string {
	var sections []string
	if len(ext.Connectors) > 0 {
		conns := append([]APIConnector(nil), ext.Connectors...)
		sort.Slice(conns, func(i, j int) bool { return conns[i].Name < conns[j].Name })
		lines := make([]string, 0, len(conns)+1)
		lines = append(lines, "Configured API connectors for `http_request`:")
		for _, c := range conns {
			lines = append(lines, fmt.Sprintf("- %s: %s", c.Name, c.BaseURL))
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}
	if secrets != nil {
		if list, err := secrets.ListSecrets(ctx, botID); err == nil && len(list) > 0 {
			placeholders := make([]string, 0, len(list))
			for _, m := range list {
				placeholders = append(placeholders, "{{secret:"+m.Name+"}}")
			}
			sections = append(sections, "Configured secret placeholders for `http_request` (values are injected by the host and never shown in plaintext): "+strings.Join(placeholders, ", "))
		}
	}
	if len(sections) == 0 {
		return ""
	}
	return "## Bot API Capabilities\n\n" + strings.Join(sections, "\n\n")
}

func botSkillsDir(botDataDir, botID string) string {
	base := strings.TrimSpace(botDataDir)
	if base == "" {
		return ""
	}
	return filepath.Join(base, effectiveBotID(botID), "skills")
}
