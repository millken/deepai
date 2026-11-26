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
	"sync"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/secret"
	"github.com/millken/deepai/pkg/tools"
)

const maxBotSkillBytes = 64 << 10

var (
	ErrBotSkillNotFound = errors.New("bot skill not found")
	ErrInvalidBotSkill  = errors.New("invalid bot skill name")

	botSkillNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,47}$`)
	skillInjectionREs   = []*regexp.Regexp{
		regexp.MustCompile(`(?is)\b(ignore|disregard|forget|override|bypass|disable)\b[^\n]{0,60}\b(previous|prior|above|all|earlier|system|developer|safety|security|existing)\s+(instructions?|rules?|prompts?|constraints?|guardrails?|policies|guidelines?)`),
		regexp.MustCompile(`(?i)<\s*/?\s*(system|developer|assistant|user|tool_call|tool_result|function_calls|untrusted_webhook_payload)\b`),
		regexp.MustCompile(`(?i)(\[\s*/?\s*INST\s*\]|<<\s*/?\s*SYS\s*>>|<\|im_(start|end)\|>|<\|(system|user|assistant|endoftext)\|>|\bsystem\s*:\s*you\s+are\b)`),
		regexp.MustCompile(`(?i)\b(you\s+are\s+now\s+in\s+developer\s+mode|do\s+anything\s+now|\bDAN\s+mode\b|jailbreak|unrestricted\s+mode|act\s+without\s+(restrictions|guardrails|limits))\b`),
		regexp.MustCompile(`(?s)<!--.*?-->`),
		regexp.MustCompile(`(?i)!\[[^\]]*\]\([^)]*\{\{secret:`),
		regexp.MustCompile(`(?is)\b(print|echo|reveal|output|expose|leak|exfiltrate|base64|send\s+to)\b[^\n]{0,60}(\{\{secret:|bot_secrets|plaintext\s+secret)`),
	}
	fallbackPersonaMu sync.Mutex
)

func hasHiddenOrControlUnicode(s string) bool {
	for _, r := range s {
		switch {
		case r < 0x20 && r != '\n' && r != '\r' && r != '\t':
			return true
		case r >= 0x7F && r <= 0x9F:
			return true
		case r >= 0x200B && r <= 0x200F:
			return true
		case r >= 0x202A && r <= 0x202E:
			return true
		case r >= 0x2060 && r <= 0x206F:
			return true
		case r == 0xFEFF:
			return true
		case r >= 0xE0000 && r <= 0xE007F: // Unicode tag block (ASCII smuggling)
			return true
		}
	}
	return false
}

func validateTrainingTextSafety(texts ...string) error {
	for _, t := range texts {
		if hasHiddenOrControlUnicode(t) {
			return errors.New("content rejected due to hidden control, bidirectional, or unicode tag characters")
		}
		for _, re := range skillInjectionREs {
			if re.MatchString(t) {
				return errors.New("content rejected due to unsafe prompt-injection, role-boundary spoofing, covert comment, or secret-exfiltration pattern")
			}
		}
		if secret.IsSealed(t) || strings.Contains(t, "enc:v1:") {
			return errors.New("do not embed sealed ciphertexts; use '{{secret:NAME}}' placeholders")
		}
	}
	return nil
}

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
		Description: "Evolve THIS bot's durable persona rules (instructions) or register/update a third-party APIConnector in its profile when explicitly taught by the user during conversation. Disabled automatically during unattended scheduled runs and on untrusted channels.",
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
	if env.disableSelfTrain || env.extensions.DisableSelfTrain {
		return botTurnEnv{}, fmt.Errorf("self-training is disabled for this turn or bot %q", effectiveBotID(env.botID))
	}
	return env, nil
}

// rejectStoredBotSecrets verifies that none of texts contain sealed ciphertexts
// or any plaintext value stored for botID in secrets. Fails closed on store errors.
func rejectStoredBotSecrets(ctx context.Context, secrets SecretStore, botID string, texts ...string) error {
	for _, t := range texts {
		if secret.IsSealed(t) || strings.Contains(t, "enc:v1:") {
			return errors.New("do not embed sealed ciphertexts; use '{{secret:NAME}}' placeholders")
		}
	}
	if secrets == nil {
		return nil
	}
	botID = effectiveBotID(botID)
	metas, err := secrets.ListSecrets(ctx, botID)
	if err != nil {
		return fmt.Errorf("verify bot secrets before persisting: %w", err)
	}
	for _, m := range metas {
		val, err := secrets.GetSecret(ctx, botID, m.Name)
		if err != nil {
			return fmt.Errorf("verify bot secret %q before persisting: %w", m.Name, err)
		}
		if val == "" {
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

func rejectPlaintextSecrets(ctx context.Context, env botTurnEnv, texts ...string) error {
	if err := validateTrainingTextSafety(texts...); err != nil {
		return err
	}
	return rejectStoredBotSecrets(ctx, env.secrets, effectiveBotID(env.botID), texts...)
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
		return fail(errors.New("bot skill data directory is not configured on this runtime"))
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

func applyPersonaToolCall(call models.ToolCall, cur *BotProfile) ([]string, error) {
	changed := false
	var checkStrings []string

	if rawInst, exists := call.Arguments["instructions"]; exists && rawInst != nil {
		inst, ok := rawInst.(string)
		if !ok {
			return nil, errors.New("instructions must be a string")
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
		kept := make([]APIConnector, 0, len(cur.Extensions.Connectors))
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
			return nil, fmt.Errorf("encode upsert_connector: %w", err)
		}
		var conn APIConnector
		if err := json.Unmarshal(rawBytes, &conn); err != nil {
			return nil, fmt.Errorf("parse upsert_connector: %w", err)
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
		return nil, errors.New("provide at least one field to update (instructions, append_instruction, isolated_prompt, upsert_connector, or remove_connector)")
	}
	if err := validateTrainingTextSafety(checkStrings...); err != nil {
		return nil, err
	}
	return checkStrings, nil
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
	if env.mutateBot == nil && env.bots == nil {
		return fail(errors.New("bot registry is not configured on this runtime"))
	}

	botID := effectiveBotID(env.botID)
	var updated BotProfile
	if env.mutateBot != nil {
		updated, err = env.mutateBot(ctx, botID, func(cur *BotProfile) ([]string, error) {
			return applyPersonaToolCall(call, cur)
		})
		if err != nil {
			return fail(err)
		}
	} else {
		fallbackPersonaMu.Lock()
		cur, gErr := env.bots.GetBot(ctx, botID)
		if gErr != nil {
			fallbackPersonaMu.Unlock()
			return fail(gErr)
		}
		checkStrings, mErr := applyPersonaToolCall(call, &cur)
		if mErr != nil {
			fallbackPersonaMu.Unlock()
			return fail(mErr)
		}
		if sErr := rejectStoredBotSecrets(ctx, env.secrets, botID, checkStrings...); sErr != nil {
			fallbackPersonaMu.Unlock()
			return fail(sErr)
		}
		cur.Extensions = normalizeBotExtensions(cur.Extensions)
		cur.UpdatedAt = time.Now().UTC()
		if vErr := validateBotProfile(cur, false); vErr != nil {
			fallbackPersonaMu.Unlock()
			return fail(vErr)
		}
		updated, err = env.bots.UpdateBot(ctx, cur)
		fallbackPersonaMu.Unlock()
		if err != nil {
			return fail(err)
		}
		if env.onBotSaved != nil {
			env.onBotSaved(updated.Clone())
		}
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
		Content:  fmt.Sprintf("Updated persona/connectors for bot %q (connectors=%d, isolated_prompt=%v). Connectors are immediately available to http_request; prompt changes apply on the next turn.", botID, len(updated.Extensions.Connectors), updated.Extensions.IsolatedPrompt),
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

// BotSkillInfo describes one bot-exclusive skill stored under
// ~/.deepai/bot/bots/<bot_id>/skills/<name>/SKILL.md.
type BotSkillInfo struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Content     string    `json:"content,omitempty"`
	Size        int64     `json:"size"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func parseBotSkillMarkdown(raw string) (desc, body string) {
	text := strings.ReplaceAll(raw, "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return "", strings.TrimSpace(text)
	}
	rest := text[4:]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return "", strings.TrimSpace(text)
	}
	fm := rest[:end]
	body = strings.TrimSpace(rest[end+5:])
	for _, line := range strings.Split(fm, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(k) == "description" {
			v = strings.TrimSpace(v)
			var unq string
			if err := json.Unmarshal([]byte(v), &unq); err == nil {
				desc = unq
			} else {
				desc = strings.Trim(v, `"'`)
			}
		}
	}
	return desc, body
}

func listBotSkillsOnDisk(botDataDir, botID string) ([]BotSkillInfo, error) {
	dir := botSkillsDir(botDataDir, botID)
	if dir == "" {
		return []BotSkillInfo{}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []BotSkillInfo{}, nil
		}
		return nil, fmt.Errorf("read bot skills dir: %w", err)
	}
	out := make([]BotSkillInfo, 0, len(entries))
	for _, ent := range entries {
		name := ent.Name()
		if !ent.IsDir() || !botSkillNamePattern.MatchString(name) {
			continue
		}
		skillPath := filepath.Join(dir, name, "SKILL.md")
		st, err := os.Lstat(skillPath)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(skillPath)
		if err != nil {
			continue
		}
		desc, body := parseBotSkillMarkdown(string(raw))
		out = append(out, BotSkillInfo{
			Name:        name,
			Description: desc,
			Content:     body,
			Size:        st.Size(),
			UpdatedAt:   st.ModTime().UTC(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func deleteBotSkillOnDisk(botDataDir, botID, name string) error {
	name = strings.TrimSpace(name)
	if !botSkillNamePattern.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrInvalidBotSkill, name)
	}
	dir := botSkillsDir(botDataDir, botID)
	if dir == "" {
		return fmt.Errorf("%w: %s", ErrBotSkillNotFound, name)
	}
	target := filepath.Join(dir, name)
	if !isUnderRoot(target, dir) {
		return fmt.Errorf("%w: %q", ErrInvalidBotSkill, name)
	}
	if _, err := os.Lstat(target); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s", ErrBotSkillNotFound, name)
		}
		return err
	}
	return os.RemoveAll(target)
}
