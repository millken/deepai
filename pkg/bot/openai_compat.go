package bot

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/models"
)

// OpenAI-compatible Chat Completions & Models endpoints so Open WebUI,
// Cherry Studio, Raycast, and Cursor can talk directly to deepai-bot with full
// tool, memory, and multi-bot persona support (docs/BOT_NEXT_DESIGN.md §6.2).

type oaiChatRequest struct {
	Model    string       `json:"model"`
	Messages []oaiMessage `json:"messages"`
	Stream   bool         `json:"stream,omitempty"`
	User     string       `json:"user,omitempty"`
}

type oaiMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []contentPart
}

// parseOpenAIModel resolves:
//   - "bot:<bot_id>" -> (bot_id, "")
//   - "bot:<bot_id>/<model_alias>" -> (bot_id, model_alias)
//   - "<model_alias>" -> ("default", model_alias)
func parseOpenAIModel(raw string) (botID, model string) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "bot:") {
		rest := strings.TrimPrefix(raw, "bot:")
		if slash := strings.Index(rest, "/"); slash >= 0 {
			return effectiveBotID(rest[:slash]), strings.TrimSpace(rest[slash+1:])
		}
		return effectiveBotID(rest), ""
	}
	return DefaultBotID, raw
}

func extractLatestOpenAIUserMessage(msgs []oaiMessage) (string, []models.MessageImage) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if !strings.EqualFold(msgs[i].Role, "user") {
			continue
		}
		switch c := msgs[i].Content.(type) {
		case string:
			return strings.TrimSpace(c), nil
		case []any:
			var parts []string
			var images []models.MessageImage
			for _, rawPart := range c {
				pm, ok := rawPart.(map[string]any)
				if !ok {
					continue
				}
				switch pm["type"] {
				case "text":
					if t, _ := pm["text"].(string); strings.TrimSpace(t) != "" {
						parts = append(parts, t)
					}
				case "image_url":
					if iu, ok := pm["image_url"].(map[string]any); ok {
						if u, _ := iu["url"].(string); strings.HasPrefix(u, "data:") {
							if mt, b64, ok := parseDataURL(u); ok {
								images = append(images, models.MessageImage{MimeType: mt, Base64: b64})
							}
						}
					}
				}
			}
			return strings.TrimSpace(strings.Join(parts, "\n")), images
		}
	}
	return "", nil
}

func parseDataURL(u string) (mimeType, b64 string, ok bool) {
	rest := strings.TrimPrefix(u, "data:")
	comma := strings.Index(rest, ",")
	if comma <= 0 {
		return "", "", false
	}
	meta := rest[:comma]
	data := strings.TrimSpace(rest[comma+1:])
	if !strings.HasSuffix(strings.ToLower(meta), ";base64") || data == "" {
		return "", "", false
	}
	mt := strings.TrimSpace(meta[:len(meta)-len(";base64")])
	return mt, data, true
}

func (s *Server) handleOpenAIModels(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	info := s.m.Info()
	now := time.Now().Unix()

	type oaiModel struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	var data []oaiModel
	if p.AllowsBot(DefaultBotID) {
		for _, m := range info.Models {
			data = append(data, oaiModel{ID: m.Name, Object: "model", Created: now, OwnedBy: "deepai-bot"})
		}
	}
	for _, b := range info.Bots {
		if !b.Enabled || !p.AllowsBot(b.ID) {
			continue
		}
		data = append(data, oaiModel{ID: "bot:" + b.ID, Object: "model", Created: now, OwnedBy: "deepai-bot"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleOpenAIChatCompletions(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if !p.CanWrite() {
		writeError(w, http.StatusForbidden, "forbidden", "token role does not allow chat completions")
		return
	}
	var req oaiChatRequest
	if !readJSON(w, r, &req, false) {
		return
	}
	botID, modelAlias := parseOpenAIModel(req.Model)
	if !p.AllowsBot(botID) {
		writeError(w, http.StatusForbidden, "forbidden", "token is not authorized for bot "+botID)
		return
	}
	content, images := extractLatestOpenAIUserMessage(req.Messages)
	if content == "" && len(images) == 0 {
		writeError(w, http.StatusBadRequest, "empty_message", "no user message found in messages")
		return
	}

	sessionID := strings.TrimSpace(r.Header.Get("X-DeepAI-Session"))
	if sessionID == "" && strings.TrimSpace(req.User) != "" {
		sessionID = s.getOrCreateOpenAIUserSession(botID, strings.TrimSpace(req.User), modelAlias)
	}
	if sessionID == "" {
		info, err := s.m.CreateSessionForBot(botID, CreateSessionRequest{Model: modelAlias})
		if err != nil {
			s.writeErr(w, err)
			return
		}
		sessionID = info.ID
	}

	var sub *Subscription
	if req.Stream {
		var err error
		sub, _, _, err = s.m.SubscribeForBot(botID, sessionID, -1)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		defer sub.Close()
	}

	res, err := s.m.SubmitForBot(botID, sessionID, SendRequest{
		Content: content,
		Model:   modelAlias,
		Images:  images,
	}, SendOptions{})
	if err != nil {
		s.writeErr(w, err)
		return
	}
	turn := res.Turn
	compID := "chatcmpl-" + turn.ID
	created := time.Now().Unix()
	modelLabel := req.Model
	if modelLabel == "" {
		modelLabel = "bot:" + botID
	}

	w.Header().Set("X-DeepAI-Session", sessionID)

	if !req.Stream {
		select {
		case <-turn.Done():
			sum := turn.Summary()
			if sum == nil {
				writeError(w, http.StatusInternalServerError, "internal", "missing turn summary")
				return
			}
			if sum.Status == TurnFailed || sum.Status == TurnTimedOut {
				writeError(w, http.StatusBadGateway, string(sum.Status), sum.Error)
				return
			}
			finishReason := "stop"
			if sum.Status == TurnCancelled {
				finishReason = "length"
			}
			usageMap := map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
			if sum.Usage != nil {
				usageMap["prompt_tokens"] = sum.Usage.InputTokens
				usageMap["completion_tokens"] = sum.Usage.OutputTokens
				usageMap["total_tokens"] = sum.Usage.InputTokens + sum.Usage.OutputTokens
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"id":      compID,
				"object":  "chat.completion",
				"created": created,
				"model":   modelLabel,
				"choices": []map[string]any{{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": sum.FinalOutput,
					},
					"finish_reason": finishReason,
				}},
				"usage": usageMap,
			})
		case <-r.Context().Done():
		}
		return
	}

	// Stream standard OpenAI chat.completion.chunk SSE frames.
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	writeChunk := func(delta map[string]any, finish any) {
		frame := map[string]any{
			"id":      compID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelLabel,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		}
		raw, _ := json.Marshal(frame)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
		_ = rc.Flush()
	}

	writeChunk(map[string]any{"role": "assistant", "content": ""}, nil)
	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-sub.C:
			if !ok {
				_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
				_ = rc.Flush()
				return
			}
			if e.TurnID != turn.ID {
				continue
			}
			switch e.Type {
			case EventTextDelta:
				if e.Text != "" {
					writeChunk(map[string]any{"content": e.Text}, nil)
				}
			case EventTurnEnd:
				finish := "stop"
				if e.Turn != nil && e.Turn.Status != TurnCompleted {
					finish = string(e.Turn.Status)
				}
				writeChunk(map[string]any{}, finish)
				_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
				_ = rc.Flush()
				return
			}
		}
	}
}

func (s *Server) getOrCreateOpenAIUserSession(botID, user, modelAlias string) string {
	key := botID + "\x00" + user
	s.oaiMu.Lock()
	if s.oaiSessions == nil {
		s.oaiSessions = map[string]string{}
	}
	sid := s.oaiSessions[key]
	s.oaiMu.Unlock()
	if sid != "" {
		if _, _, err := s.m.GetSessionForBot(botID, sid); err == nil {
			return sid
		}
	}
	info, err := s.m.CreateSessionForBot(botID, CreateSessionRequest{
		Title: "OpenAI: " + user,
		Model: modelAlias,
	})
	if err != nil {
		return ""
	}
	s.oaiMu.Lock()
	s.oaiSessions[key] = info.ID
	s.oaiMu.Unlock()
	return info.ID
}
