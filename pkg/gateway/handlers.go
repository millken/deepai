package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/memory"
	"github.com/millken/deepai/pkg/models"
)

const (
	maxRequestBodyBytes = 1 << 20
	sseBufferSize       = 128
)

type chatRequest struct {
	SessionID    string   `json:"session_id"`
	UserID       string   `json:"user_id"`
	Message      string   `json:"message"`
	Model        string   `json:"model"`
	SystemPrompt string   `json:"system_prompt,omitempty"`
	Tools        []string `json:"tools,omitempty"`
	Stream       bool     `json:"stream,omitempty"`
}

type chatResponse struct {
	SessionID    string       `json:"session_id"`
	UserID       string       `json:"user_id"`
	Model        string       `json:"model"`
	Output       string       `json:"output"`
	Usage        *agent.Usage `json:"usage,omitempty"`
	MessageCount int          `json:"message_count"`
}

type sseEvent struct {
	Event string
	Data  any
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	req, err := decodeChatRequest(w, r)
	if err != nil {
		writeError(w, errStatus(err), err)
		return
	}

	if wantsSSE(r, req.Stream) {
		s.streamChat(w, r, req)
		return
	}

	resp, err := s.runChat(r.Context(), req)
	if err != nil {
		writeError(w, errStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func decodeChatRequest(w http.ResponseWriter, r *http.Request) (chatRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return chatRequest{}, fmt.Errorf("decode request: %w", err)
	}

	req.SessionID = defaultSessionID(req.SessionID)
	req.UserID = defaultUserID(req.UserID)
	req.Message = strings.TrimSpace(req.Message)
	req.Model = strings.TrimSpace(req.Model)
	req.SystemPrompt = strings.TrimSpace(req.SystemPrompt)

	if req.Message == "" {
		return chatRequest{}, errors.New("message is required")
	}

	return req, nil
}

func (s *Server) runChat(ctx context.Context, req chatRequest) (chatResponse, error) {
	history, modelName, runAgent, ext, session, err := s.prepareRun(ctx, req)
	if err != nil {
		return chatResponse{}, err
	}

	result, err := runAgent.Run(ctx, req.SessionID, history)
	if err != nil {
		return chatResponse{}, err
	}

	session.Metadata = s.updateMemoryAndNudge(req.SessionID, req.UserID, result.Messages, ext, session.Metadata, runAgent)

	resp := chatResponse{
		SessionID:    req.SessionID,
		UserID:       req.UserID,
		Model:        modelName,
		Output:       result.FinalOutput,
		Usage:        result.Usage,
		MessageCount: len(result.Messages),
	}

	if err := s.saveSession(ctx, req, result.Messages, session.Metadata); err != nil {
		return chatResponse{}, err
	}
	return resp, nil
}

func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, req chatRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming is not supported"))
		return
	}

	history, modelName, runAgent, ext, session, err := s.prepareRun(r.Context(), req)
	if err != nil {
		writeError(w, errStatus(err), err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	events := make(chan sseEvent, sseBufferSize)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for evt := range runAgent.Events() {
			enqueueEvent(events, sseEvent{Event: string(evt.Type), Data: evt})
		}
	}()

	type outcome struct {
		resp chatResponse
		err  error
	}
	outcomes := make(chan outcome, 1)
	go func() {
		result, runErr := runAgent.Run(r.Context(), req.SessionID, history)
		if runErr != nil {
			outcomes <- outcome{err: runErr}
			return
		}

		resp := chatResponse{
			SessionID:    req.SessionID,
			UserID:       req.UserID,
			Model:        modelName,
			Output:       result.FinalOutput,
			Usage:        result.Usage,
			MessageCount: len(result.Messages),
		}
		session.Metadata = s.updateMemoryAndNudge(req.SessionID, req.UserID, result.Messages, ext, session.Metadata, runAgent)
		if saveErr := s.saveSession(r.Context(), req, result.Messages, session.Metadata); saveErr != nil {
			outcomes <- outcome{err: saveErr}
			return
		}
		outcomes <- outcome{resp: resp}
	}()

	writeSSE(w, "ready", map[string]string{
		"session_id": req.SessionID,
		"model":      modelName,
	})
	flusher.Flush()

	var eventStream <-chan sseEvent = events
	for {
		select {
		case evt, ok := <-eventStream:
			if !ok {
				eventStream = nil
				continue
			}
			writeSSE(w, evt.Event, evt.Data)
			flusher.Flush()
		case out := <-outcomes:
			<-done
			close(events)
			for evt := range events {
				writeSSE(w, evt.Event, evt.Data)
				flusher.Flush()
			}
			if out.err != nil {
				writeSSE(w, "error", map[string]string{"error": out.err.Error()})
			} else {
				writeSSE(w, "done", out.resp)
			}
			flusher.Flush()
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) prepareRun(ctx context.Context, req chatRequest) ([]models.Message, string, *agent.Agent, memory.Extractor, models.Session, error) {
	session, err := s.store.LoadSession(ctx, req.SessionID)
	if err != nil {
		return nil, "", nil, nil, models.Session{}, err
	}

	runAgent, modelName, ext, err := s.newRuntime(firstNonEmpty(req.Model, s.cfg.DefaultModel), req.Tools, req.UserID)
	if err != nil {
		return nil, "", nil, nil, models.Session{}, err
	}

	history := session.Messages
	if req.SystemPrompt != "" {
		history = append(history, models.Message{
			ID:        newMessageID("system"),
			SessionID: req.SessionID,
			Role:      models.RoleSystem,
			Content:   req.SystemPrompt,
			CreatedAt: time.Now().UTC(),
		})
	}
	history = append(history, models.Message{
		ID:        newMessageID("human"),
		SessionID: req.SessionID,
		Role:      models.RoleHuman,
		Content:   req.Message,
		CreatedAt: time.Now().UTC(),
	})

	return history, modelName, runAgent, ext, session, nil
}

func (s *Server) saveSession(ctx context.Context, req chatRequest, messages []models.Message, metadata map[string]string) error {
	return s.store.Save(ctx, models.Session{
		ID:        req.SessionID,
		UserID:    req.UserID,
		State:     models.SessionStateActive,
		Messages:  messages,
		Metadata:  metadata,
		CreatedAt: firstCreatedAt(messages),
		UpdatedAt: time.Now().UTC(),
	})
}

// updateMemoryAndNudge runs a scheduled memory update and manages the nudge counter
// in session metadata. Also updates UserScope if userID is non-empty. Returns the updated metadata.
func (s *Server) updateMemoryAndNudge(sessionID, userID string, messages []models.Message, ext memory.Extractor, metadata map[string]string, runAgent *agent.Agent) map[string]string {
	if s.memService == nil || ext == nil {
		return metadata
	}

	// Update user-level memory (cross-session) if userID is set.
	// Merge skill usage recording into the same update cycle to avoid concurrent-write races.
	if userID != "" {
		skillName := ""
		if runAgent != nil {
			skillName = runAgent.ActiveSkill()
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if skillName != "" {
				if err := s.memService.UpdateScopeWithSkillUsage(ctx, memory.UserScope(userID), messages, ext, skillName); err != nil {
					s.cfg.Logger.Printf("memory: user-scope update failed for %s: %v", userID, err)
				}
			} else {
				if err := s.memService.UpdateScopeWith(ctx, memory.UserScope(userID), messages, ext); err != nil {
					s.cfg.Logger.Printf("memory: user-scope update failed for %s: %v", userID, err)
				}
			}
		}()
	}

	if metadata == nil {
		metadata = map[string]string{}
	}
	count, _ := strconv.Atoi(metadata["memory_nudge_count"])
	usedMemory := usedMemoryTool(messages)
	if usedMemory {
		count = 0
	} else {
		count++
	}
	if count >= 10 {
		count = 0
	}
	// Schedule async update; skip when memory tool was used (it already saved).
	if !usedMemory {
		if runAgent != nil {
			if skillName := runAgent.ActiveSkill(); skillName != "" {
				s.memService.ScheduleUpdateWithFactSource(sessionID, messages, ext, "skill:"+skillName)
			} else {
				s.memService.ScheduleUpdateWith(sessionID, messages, ext)
			}
		} else {
				s.memService.ScheduleUpdateWith(sessionID, messages, ext)
		}
	}

	metadata["memory_nudge_count"] = strconv.Itoa(count)
	return metadata
}

func enqueueEvent(ch chan<- sseEvent, evt sseEvent) {
	select {
	case ch <- evt:
	default:
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeSSE(w http.ResponseWriter, event string, value any) {
	data, _ := json.Marshal(value)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func wantsSSE(r *http.Request, stream bool) bool {
	return stream || strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

func errStatus(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusRequestTimeout
	case strings.Contains(err.Error(), "required"), strings.Contains(err.Error(), "decode request"):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func usedMemoryTool(messages []models.Message) bool {
	for _, msg := range messages {
		for _, call := range msg.ToolCalls {
			if call.Name == "memory" {
				return true
			}
		}
	}
	return false
}
