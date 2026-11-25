package bot

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/schedule"
)

//go:embed web
var webFS embed.FS

// maxBodyBytes bounds JSON request bodies.
const maxBodyBytes = 4 << 20

// heartbeatInterval keeps idle streams alive through proxies.
const heartbeatInterval = 15 * time.Second

// ServerOptions configures the HTTP front end.
type ServerOptions struct {
	Manager *Manager
	// Token is the shared secret clients send as "Authorization: Bearer
	// <token>" (or ?token= for EventSource/WebSocket, which cannot set
	// headers). Empty disables auth; the deepai-bot command only allows that
	// on a loopback listener.
	Token string
	// Schedules is the (optional) timed-task service. Nil means the feature
	// is disabled: every schedule endpoint returns 404 and the tool is not
	// registered (docs/BOT_SCHEDULER_DESIGN.md §11).
	Schedules *schedule.Service
	// AllowedOrigins lists browser origins (scheme://host[:port]) that may
	// call the API cross-origin, e.g. a separately hosted web client. "*"
	// allows any origin. The bot's own web console is always same-origin.
	AllowedOrigins []string
	// Tokens optionally enables multi-token RBAC (admin/user/readonly) on top
	// of the master Token (docs/BOT_NEXT_DESIGN.md §6.1).
	Tokens TokenStore
	// Audit optionally writes structured security/governance JSONL records.
	Audit *AuditLogger
	// Metrics optionally overrides Manager.Metrics() for GET /metrics.
	Metrics *Metrics
	// DisableWebUI stops serving the embedded console at /.
	DisableWebUI bool
	Logger       *slog.Logger
}

// Server is the bot's http.Handler.
type Server struct {
	opts      ServerOptions
	m         *Manager
	schedules *schedule.Service
	tokens    TokenStore
	audit     *AuditLogger
	metrics   *Metrics
	log       *slog.Logger
	mux       *http.ServeMux

	oaiMu       sync.Mutex
	oaiSessions map[string]string
}

// NewServer builds the handler. Routes are documented in docs/BOT_DESIGN.md.
func NewServer(opts ServerOptions) *Server {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	met := opts.Metrics
	if met == nil && opts.Manager != nil {
		met = opts.Manager.Metrics()
	}
	if met == nil {
		met = NewMetrics()
	}
	s := &Server{
		opts: opts, m: opts.Manager, schedules: opts.Schedules,
		tokens: opts.Tokens, audit: opts.Audit, metrics: met,
		log: log, mux: http.NewServeMux(),
	}

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.Handle("GET /v1/info", s.auth(s.handleInfo))

	// Bot management + bot-scoped session/schedule routes (docs/BOT_MULTI_DESIGN.md §5).
	s.mux.Handle("GET /v1/bots", s.auth(s.handleListBots))
	s.mux.Handle("POST /v1/bots", s.auth(s.handleCreateBot))
	s.mux.Handle("GET /v1/bots/{bot_id}", s.auth(s.handleGetBot))
	s.mux.Handle("PATCH /v1/bots/{bot_id}", s.auth(s.handleUpdateBot))
	s.mux.Handle("DELETE /v1/bots/{bot_id}", s.auth(s.handleDeleteBot))
	s.mux.Handle("GET /v1/bots/{bot_id}/sessions", s.auth(s.handleListSessions))
	s.mux.Handle("POST /v1/bots/{bot_id}/sessions", s.auth(s.handleCreateSession))
	s.mux.Handle("GET /v1/bots/{bot_id}/sessions/{id}", s.auth(s.handleGetSession))
	s.mux.Handle("PATCH /v1/bots/{bot_id}/sessions/{id}", s.auth(s.handleRenameSession))
	s.mux.Handle("DELETE /v1/bots/{bot_id}/sessions/{id}", s.auth(s.handleDeleteSession))
	s.mux.Handle("POST /v1/bots/{bot_id}/sessions/{id}/messages", s.auth(s.handleSend))
	s.mux.Handle("POST /v1/bots/{bot_id}/sessions/{id}/attachments", s.auth(s.handleUploadAttachment))
	s.mux.Handle("GET /v1/bots/{bot_id}/sessions/{id}/files/{path...}", s.auth(s.handleDownloadFile))
	s.mux.Handle("GET /v1/bots/{bot_id}/sessions/{id}/queue", s.auth(s.handleListQueue))
	s.mux.Handle("DELETE /v1/bots/{bot_id}/sessions/{id}/queue/{queue_id}", s.auth(s.handleRemoveQueueItem))
	s.mux.Handle("DELETE /v1/bots/{bot_id}/sessions/{id}/queue", s.auth(s.handleClearQueue))
	s.mux.Handle("POST /v1/bots/{bot_id}/sessions/{id}/cancel", s.auth(s.handleCancel))
	s.mux.Handle("POST /v1/bots/{bot_id}/sessions/{id}/answer", s.auth(s.handleAnswer))
	s.mux.Handle("GET /v1/bots/{bot_id}/sessions/{id}/events", s.auth(s.handleSessionEvents))
	s.mux.Handle("GET /v1/bots/{bot_id}/schedules", s.auth(s.handleListSchedules))
	s.mux.Handle("POST /v1/bots/{bot_id}/schedules", s.auth(s.handleCreateSchedule))

	// Flat aliases bound to default bot + current workdir.
	s.mux.Handle("GET /v1/sessions", s.auth(s.handleListSessions))
	s.mux.Handle("POST /v1/sessions", s.auth(s.handleCreateSession))
	s.mux.Handle("GET /v1/sessions/{id}", s.auth(s.handleGetSession))
	s.mux.Handle("PATCH /v1/sessions/{id}", s.auth(s.handleRenameSession))
	s.mux.Handle("DELETE /v1/sessions/{id}", s.auth(s.handleDeleteSession))
	s.mux.Handle("POST /v1/sessions/{id}/messages", s.auth(s.handleSend))
	s.mux.Handle("POST /v1/sessions/{id}/attachments", s.auth(s.handleUploadAttachment))
	s.mux.Handle("GET /v1/sessions/{id}/files/{path...}", s.auth(s.handleDownloadFile))
	s.mux.Handle("GET /v1/sessions/{id}/queue", s.auth(s.handleListQueue))
	s.mux.Handle("DELETE /v1/sessions/{id}/queue/{queue_id}", s.auth(s.handleRemoveQueueItem))
	s.mux.Handle("DELETE /v1/sessions/{id}/queue", s.auth(s.handleClearQueue))
	s.mux.Handle("POST /v1/sessions/{id}/cancel", s.auth(s.handleCancel))
	s.mux.Handle("POST /v1/sessions/{id}/answer", s.auth(s.handleAnswer))
	s.mux.Handle("GET /v1/sessions/{id}/events", s.auth(s.handleSessionEvents))
	s.mux.Handle("GET /v1/events", s.auth(s.handleGlobalEvents))
	s.mux.Handle("GET /v1/ws", s.auth(s.handleWS))

	// schedule endpoints (handlers report 404 when Schedules is nil)
	s.mux.Handle("GET /v1/schedules", s.auth(s.handleListSchedules))
	s.mux.Handle("POST /v1/schedules", s.auth(s.handleCreateSchedule))
	s.mux.Handle("GET /v1/schedules/{id}", s.auth(s.handleGetSchedule))
	s.mux.Handle("PATCH /v1/schedules/{id}", s.auth(s.handlePatchSchedule))
	s.mux.Handle("DELETE /v1/schedules/{id}", s.auth(s.handleDeleteSchedule))
	s.mux.Handle("POST /v1/schedules/{id}/pause", s.auth(s.handlePauseSchedule))
	s.mux.Handle("POST /v1/schedules/{id}/resume", s.auth(s.handleResumeSchedule))
	s.mux.Handle("POST /v1/schedules/{id}/approve", s.auth(s.handleApproveSchedule))
	s.mux.Handle("POST /v1/schedules/{id}/run", s.auth(s.handleRunSchedule))
	s.mux.Handle("POST /v1/schedules/{id}/skip", s.auth(s.handleSkipSchedule))
	s.mux.Handle("GET /v1/schedules/{id}/runs", s.auth(s.handleListRuns))
	s.mux.Handle("GET /v1/runs/{id}", s.auth(s.handleGetRun))
	s.mux.Handle("POST /v1/runs/{id}/cancel", s.auth(s.handleCancelRun))
	s.mux.Handle("POST /v1/schedule/validate", s.auth(s.handleValidateSchedule))
	// Inbound webhook trigger endpoint: authenticated solely by the schedule's
	// own webhook signature/secret (never the bot's main bearer token, per
	// docs/BOT_NEXT_DESIGN.md §4.2).
	s.mux.HandleFunc("POST /v1/hooks/{schedule_id}", s.handleWebhookHook)

	// OpenAI-compatible API (docs/BOT_NEXT_DESIGN.md §6.2).
	s.mux.Handle("GET /v1/models", s.auth(s.handleOpenAIModels))
	s.mux.Handle("POST /v1/chat/completions", s.auth(s.handleOpenAIChatCompletions))

	// Scoped token management & Prometheus metrics (admin-only, §6.1 & §6.3).
	s.mux.Handle("GET /v1/tokens", s.auth(s.handleListTokens))
	s.mux.Handle("POST /v1/tokens", s.auth(s.handleCreateToken))
	s.mux.Handle("DELETE /v1/tokens/{id}", s.auth(s.handleRevokeToken))
	s.mux.Handle("GET /metrics", s.auth(s.handleMetrics))

	if !opts.DisableWebUI {
		sub, _ := fs.Sub(webFS, "web")
		s.mux.Handle("GET /", http.FileServer(http.FS(sub)))
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.hostAllowed(r) {
		writeError(w, http.StatusForbidden, "forbidden_host", "host not allowed")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		if !s.originAllowed(origin, r.Host) {
			writeError(w, http.StatusForbidden, "forbidden_origin", "origin not allowed")
			return
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Vary", "Origin")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Last-Event-ID, X-DeepAI-Session")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

// hostAllowed defeats DNS rebinding when auth is off: a page on
// evil.example that re-resolves to 127.0.0.1 still sends Host: evil.example.
func (s *Server) hostAllowed(r *http.Request) bool {
	if s.opts.Token != "" {
		return true
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) originAllowed(origin, host string) bool {
	u, err := url.Parse(origin)
	if err == nil && u.Host != "" && strings.EqualFold(u.Host, host) {
		return true
	}
	for _, o := range s.opts.AllowedOrigins {
		if o == "*" || strings.EqualFold(strings.TrimRight(o, "/"), origin) {
			return true
		}
	}
	return false
}

func (s *Server) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := Principal{ID: "master", Name: "master", Role: RoleAdmin}
		if s.opts.Token != "" {
			got := r.URL.Query().Get("token")
			if v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
				got = strings.TrimSpace(v)
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.opts.Token)) != 1 {
				matched := false
				if s.tokens != nil && got != "" {
					if rec, err := s.tokens.LookupToken(r.Context(), HashToken(got)); err == nil {
						now := time.Now().UTC()
						if rec.RevokedAt == nil && (rec.ExpiresAt == nil || rec.ExpiresAt.After(now)) {
							p = Principal{ID: rec.ID, Name: rec.Name, Role: rec.Role, BotIDs: rec.BotIDs}
							matched = true
							_ = s.tokens.TouchToken(r.Context(), rec.ID, now)
						}
					}
				}
				if !matched {
					s.audit.Record(AuditEntry{
						Action:  "auth_failed",
						Target:  r.Method + " " + r.URL.Path,
						Outcome: "denied",
						Detail:  r.RemoteAddr,
					})
					writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid token")
					return
				}
			}
		}

		// Baseline HTTP RBAC enforcement (docs/BOT_NEXT_DESIGN.md §6.1).
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.URL.Path != "/v1/schedule/validate" {
			if !p.CanWrite() {
				writeError(w, http.StatusForbidden, "forbidden", "readonly token cannot mutate resources")
				return
			}
		}
		if pathBot := strings.TrimSpace(r.PathValue("bot_id")); pathBot != "" {
			if !p.AllowsBot(pathBot) {
				writeError(w, http.StatusForbidden, "forbidden", "token is not authorized for bot "+pathBot)
				return
			}
		} else if strings.HasPrefix(r.URL.Path, "/v1/sessions") {
			if !p.AllowsBot(DefaultBotID) {
				writeError(w, http.StatusForbidden, "forbidden", "token is not authorized for default bot")
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/v1/bots") && r.Method != http.MethodGet {
			if r.URL.Path == "/v1/bots" || !strings.Contains(strings.TrimPrefix(r.URL.Path, "/v1/bots/"), "/") {
				if !p.IsAdmin() {
					writeError(w, http.StatusForbidden, "forbidden", "admin role required to modify bots")
					return
				}
			}
		}

		h(w, r.WithContext(withPrincipal(r.Context(), p)))
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !principalFromContext(r.Context()).IsAdmin() {
		writeError(w, http.StatusForbidden, "forbidden", "admin role required for /metrics")
		return
	}
	s.metrics.WritePrometheus(w, s.m)
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	if !principalFromContext(r.Context()).IsAdmin() {
		writeError(w, http.StatusForbidden, "forbidden", "admin role required")
		return
	}
	if s.tokens == nil {
		writeError(w, http.StatusNotFound, "not_found", "scoped tokens are not enabled")
		return
	}
	list, err := s.tokens.ListTokens(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if list == nil {
		list = []TokenRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": list})
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if !p.IsAdmin() {
		writeError(w, http.StatusForbidden, "forbidden", "admin role required")
		return
	}
	if s.tokens == nil {
		writeError(w, http.StatusNotFound, "not_found", "scoped tokens are not enabled")
		return
	}
	var req CreateTokenRequest
	if !readJSON(w, r, &req, false) {
		return
	}
	rec, err := s.tokens.CreateToken(r.Context(), req)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	s.audit.Record(AuditEntry{
		Action:  "token_created",
		Actor:   p.Name,
		Role:    string(p.Role),
		Target:  rec.ID,
		Outcome: "ok",
		Detail:  string(rec.Role),
	})
	writeJSON(w, http.StatusCreated, rec)
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if !p.IsAdmin() {
		writeError(w, http.StatusForbidden, "forbidden", "admin role required")
		return
	}
	if s.tokens == nil {
		writeError(w, http.StatusNotFound, "not_found", "scoped tokens are not enabled")
		return
	}
	id := r.PathValue("id")
	if err := s.tokens.RevokeToken(r.Context(), id); err != nil {
		s.writeErr(w, err)
		return
	}
	s.audit.Record(AuditEntry{
		Action:  "token_revoked",
		Actor:   p.Name,
		Role:    string(p.Role),
		Target:  id,
		Outcome: "ok",
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.m.Info())
}

func botIDFromRequest(r *http.Request) string {
	return effectiveBotID(r.PathValue("bot_id"))
}

func (s *Server) handleListBots(w http.ResponseWriter, r *http.Request) {
	list, err := s.m.ListBots(r.Context())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bots": list})
}

func (s *Server) handleCreateBot(w http.ResponseWriter, r *http.Request) {
	var req CreateBotRequest
	if !readJSON(w, r, &req, false) {
		return
	}
	b, err := s.m.CreateBot(r.Context(), req)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	p := principalFromContext(r.Context())
	s.audit.Record(AuditEntry{Action: "bot_created", Actor: p.Name, Role: string(p.Role), BotID: b.ID, Target: b.ID, Outcome: "ok"})
	writeJSON(w, http.StatusCreated, b)
}

func (s *Server) handleGetBot(w http.ResponseWriter, r *http.Request) {
	b, err := s.m.GetBot(r.Context(), r.PathValue("bot_id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) handleUpdateBot(w http.ResponseWriter, r *http.Request) {
	var req UpdateBotRequest
	if !readJSON(w, r, &req, false) {
		return
	}
	b, err := s.m.UpdateBot(r.Context(), r.PathValue("bot_id"), req)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) handleDeleteBot(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1" || strings.EqualFold(r.URL.Query().Get("force"), "true")
	if err := s.m.DeleteBot(r.Context(), r.PathValue("bot_id"), force); err != nil {
		s.writeErr(w, err)
		return
	}
	p := principalFromContext(r.Context())
	s.audit.Record(AuditEntry{Action: "bot_deleted", Actor: p.Name, Role: string(p.Role), BotID: r.PathValue("bot_id"), Target: r.PathValue("bot_id"), Outcome: "ok"})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.m.ListSessionsForBot(botIDFromRequest(r), limit)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": list})
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req CreateSessionRequest
	if !readJSON(w, r, &req, true) {
		return
	}
	info, err := s.m.CreateSessionForBot(botIDFromRequest(r), req)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	info, msgs, err := s.m.GetSessionForBot(botIDFromRequest(r), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": info, "messages": msgs})
}

func (s *Server) handleRenameSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title string `json:"title"`
	}
	if !readJSON(w, r, &req, false) {
		return
	}
	info, err := s.m.RenameSessionForBot(botIDFromRequest(r), r.PathValue("id"), req.Title)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if err := s.m.DeleteSessionForBot(botIDFromRequest(r), r.PathValue("id")); err != nil {
		s.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSend starts a turn. Three response modes, chosen by the client:
//
//   - Accept: text/event-stream (or ?stream=sse): stream this turn's events
//     as SSE until turn_end.
//   - Accept: application/x-ndjson (or ?stream=ndjson): the same events, one
//     JSON object per line — simplest for non-browser apps.
//   - otherwise: 202 with the turn id; ?wait=1 instead blocks and returns the
//     TurnSummary.
//
// The turn belongs to the bot, not the request: a client that disconnects
// mid-stream does not cancel it (use /cancel), and can pick the events back
// up from /events?since=<last seq>.
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	botID := botIDFromRequest(r)
	id := r.PathValue("id")
	var req SendRequest
	if !readJSON(w, r, &req, false) {
		return
	}
	mode := streamMode(r)

	var sub *Subscription
	if mode != "" {
		var err error
		sub, _, _, err = s.m.SubscribeForBot(botID, id, -1)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		defer sub.Close()
	}
	res, err := s.m.SubmitForBot(botID, id, req, SendOptions{})
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if res.Queued || res.Steered || res.Turn == nil {
		writeJSON(w, http.StatusAccepted, res)
		return
	}
	turn := res.Turn

	switch {
	case mode != "":
		sw := newStreamWriter(w, mode)
		s.pump(r.Context(), sw, sub, func(e Event) (keep, last bool) {
			if e.TurnID != turn.ID {
				return false, false
			}
			return true, e.Type == EventTurnEnd
		})
	case r.URL.Query().Get("wait") != "":
		select {
		case <-turn.Done():
			writeJSON(w, http.StatusOK, turn.Summary())
		case <-r.Context().Done():
		}
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"session_id": id, "turn_id": turn.ID})
	}
}

func (s *Server) handleListQueue(w http.ResponseWriter, r *http.Request) {
	items, err := s.m.ListQueueForBot(botIDFromRequest(r), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queue": items})
}

func (s *Server) handleRemoveQueueItem(w http.ResponseWriter, r *http.Request) {
	if err := s.m.RemoveQueuedMessageForBot(botIDFromRequest(r), r.PathValue("id"), r.PathValue("queue_id")); err != nil {
		s.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleClearQueue(w http.ResponseWriter, r *http.Request) {
	if err := s.m.ClearQueueForBot(botIDFromRequest(r), r.PathValue("id")); err != nil {
		s.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUploadAttachment(w http.ResponseWriter, r *http.Request) {
	botID := botIDFromRequest(r)
	id := r.PathValue("id")
	// Cap total multipart body slightly above MaxAttachmentBytes so oversized
	// files fail fast without buffering arbitrary payloads in memory.
	r.Body = http.MaxBytesReader(w, r.Body, MaxAttachmentBytes+(1<<20))
	if err := r.ParseMultipartForm(MaxAttachmentBytes); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeErr(w, ErrAttachmentTooLarge)
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid multipart upload: "+err.Error())
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "missing multipart field 'file'")
		return
	}
	defer f.Close()

	att, err := s.m.UploadAttachmentForBot(botID, id, hdr.Filename, f)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, att)
}

func (s *Server) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	rf, err := s.m.ResolveSessionFileForBot(botIDFromRequest(r), r.PathValue("id"), r.PathValue("path"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	f, err := os.Open(rf.AbsPath)
	if err != nil {
		if os.IsNotExist(err) {
			s.writeErr(w, ErrAttachmentNotFound)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		s.writeErr(w, ErrAttachmentNotFound)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	if rf.MimeType != "" {
		w.Header().Set("Content-Type", rf.MimeType)
	}
	if rf.ForceDownload || r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+rf.Name+`"`)
	}
	http.ServeContent(w, r, rf.Name, st.ModTime(), f)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.m.CancelForBot(botIDFromRequest(r), r.PathValue("id")); err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	var req AnswerRequest
	if !readJSON(w, r, &req, false) {
		return
	}
	if err := s.m.AnswerForBot(botIDFromRequest(r), r.PathValue("id"), req.QuestionID, req.Answer); err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	sub, backlog, gap, err := s.m.SubscribeForBot(botIDFromRequest(r), r.PathValue("id"), sinceParam(r))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	defer sub.Close()
	s.streamSubscription(w, r, sub, backlog, gap)
}

func (s *Server) handleGlobalEvents(w http.ResponseWriter, r *http.Request) {
	sub, backlog, gap := s.m.SubscribeAll(sinceParam(r))
	defer sub.Close()
	s.streamSubscription(w, r, sub, backlog, gap)
}

func (s *Server) streamSubscription(w http.ResponseWriter, r *http.Request, sub *Subscription, backlog []Event, gap bool) {
	mode := streamMode(r)
	if mode == "" {
		mode = "sse"
	}
	sw := newStreamWriter(w, mode)
	if gap {
		sw.write(Event{Type: EventGap})
	}
	for _, e := range backlog {
		sw.write(e)
	}
	sw.flush()
	s.pump(r.Context(), sw, sub, func(Event) (bool, bool) { return true, false })
}

// pump forwards sub to sw until ctx ends, filter reports last, or the
// subscription closes. A subscription dropped for lagging ends with a gap
// event so the client knows to resubscribe from its last seq.
func (s *Server) pump(ctx context.Context, sw *streamWriter, sub *Subscription, filter func(Event) (keep, last bool)) {
	hb := time.NewTicker(heartbeatInterval)
	defer hb.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hb.C:
			sw.heartbeat()
		case e, ok := <-sub.C:
			if !ok {
				if sub.Lagged() {
					sw.write(Event{Type: EventGap})
					sw.flush()
				}
				return
			}
			keep, last := filter(e)
			if keep {
				sw.write(e)
				sw.flush()
			}
			if last {
				return
			}
		}
	}
}

func streamMode(r *http.Request) string {
	switch strings.ToLower(r.URL.Query().Get("stream")) {
	case "sse", "1", "true":
		return "sse"
	case "ndjson":
		return "ndjson"
	}
	accept := r.Header.Get("Accept")
	switch {
	case strings.Contains(accept, "text/event-stream"):
		return "sse"
	case strings.Contains(accept, "application/x-ndjson"):
		return "ndjson"
	}
	return ""
}

func sinceParam(r *http.Request) int64 {
	v := r.Header.Get("Last-Event-ID")
	if q := r.URL.Query().Get("since"); q != "" {
		v = q
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

type streamWriter struct {
	w    http.ResponseWriter
	rc   *http.ResponseController
	mode string
	enc  *json.Encoder
}

func newStreamWriter(w http.ResponseWriter, mode string) *streamWriter {
	h := w.Header()
	if mode == "ndjson" {
		h.Set("Content-Type", "application/x-ndjson")
	} else {
		h.Set("Content-Type", "text/event-stream")
	}
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	sw := &streamWriter{w: w, rc: http.NewResponseController(w), mode: mode, enc: json.NewEncoder(w)}
	sw.flush()
	return sw
}

func (sw *streamWriter) write(e Event) {
	if sw.mode == "ndjson" {
		_ = sw.enc.Encode(e)
		return
	}
	data, _ := json.Marshal(e)
	if e.Seq > 0 {
		_, _ = sw.w.Write([]byte("id: " + strconv.FormatInt(e.Seq, 10) + "\n"))
	}
	_, _ = sw.w.Write([]byte("event: " + string(e.Type) + "\ndata: "))
	_, _ = sw.w.Write(data)
	_, _ = sw.w.Write([]byte("\n\n"))
}

func (sw *streamWriter) heartbeat() {
	if sw.mode == "ndjson" {
		_ = sw.enc.Encode(Event{Type: EventHeartbeat, Time: time.Now().UTC()})
	} else {
		_, _ = sw.w.Write([]byte(": heartbeat\n\n"))
	}
	sw.flush()
}

func (sw *streamWriter) flush() {
	_ = sw.rc.Flush()
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any, allowEmpty bool) bool {
	// Requiring a JSON content type forces a CORS preflight for browser
	// requests, so a foreign page cannot fire a "simple" text/plain POST.
	ct := r.Header.Get("Content-Type")
	if r.ContentLength == 0 && allowEmpty {
		return true
	}
	if !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(dst); err != nil {
		if allowEmpty && errors.Is(err, io.EOF) {
			return true
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg}})
}

// errorStatus maps Manager errors onto HTTP statuses and stable codes; the
// WebSocket transport reuses the codes.
func errorStatus(err error) (int, string) {
	var locked *models.ErrSessionLocked
	var schedErr *schedule.Error
	switch {
	case errors.As(err, &schedErr), errors.Is(err, schedule.ErrNotFound),
		errors.Is(err, schedule.ErrConflict), errors.Is(err, schedule.ErrBusy):
		return schedule.HTTPStatus(err), string(schedule.ErrorCode(err))
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, ErrUnknownBot):
		return http.StatusNotFound, "unknown_bot"
	case errors.Is(err, ErrBotDisabled):
		return http.StatusForbidden, "bot_disabled"
	case errors.Is(err, ErrBotHasSessions):
		return http.StatusConflict, "bot_has_sessions"
	case errors.Is(err, ErrBotDeleteForbidden):
		return http.StatusMethodNotAllowed, "method_not_allowed"
	case errors.Is(err, ErrBotExists):
		return http.StatusConflict, "bot_exists"
	case errors.Is(err, ErrInvalidBot):
		return http.StatusBadRequest, "bad_request"
	case errors.Is(err, ErrUnknownTool):
		return http.StatusBadRequest, "unknown_tool"
	case errors.Is(err, ErrBusy):
		return http.StatusConflict, "busy"
	case errors.Is(err, ErrQueueFull):
		return http.StatusConflict, "queue_full"
	case errors.Is(err, ErrQueueItemNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, ErrInvalidMode):
		return http.StatusBadRequest, "bad_request"
	case errors.Is(err, ErrAttachmentTooLarge):
		return http.StatusRequestEntityTooLarge, "attachment_too_large"
	case errors.Is(err, ErrUploadQuotaExceeded):
		return http.StatusRequestEntityTooLarge, "upload_quota_exceeded"
	case errors.Is(err, ErrAttachmentNotFound):
		return http.StatusNotFound, "attachment_not_found"
	case errors.Is(err, ErrInvalidAttachment):
		return http.StatusBadRequest, "invalid_attachment"
	case errors.Is(err, ErrForbiddenPath):
		return http.StatusForbidden, "forbidden_path"
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden, "forbidden"
	case errors.Is(err, ErrTokenNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, ErrInvalidToken):
		return http.StatusBadRequest, "bad_request"
	case errors.As(err, &locked):
		return http.StatusConflict, "session_locked"
	case errors.Is(err, ErrNotRunning):
		return http.StatusConflict, "not_running"
	case errors.Is(err, ErrUnknownQuestion):
		return http.StatusNotFound, "unknown_question"
	case errors.Is(err, ErrUnknownModel):
		return http.StatusBadRequest, "unknown_model"
	case errors.Is(err, ErrEmptyMessage):
		return http.StatusBadRequest, "empty_message"
	case errors.Is(err, ErrClosed):
		return http.StatusServiceUnavailable, "shutting_down"
	}
	return http.StatusInternalServerError, "internal"
}

func (s *Server) writeErr(w http.ResponseWriter, err error) {
	status, code := errorStatus(err)
	if status == http.StatusInternalServerError {
		s.log.Error("bot: request failed", "err", err)
	}
	writeError(w, status, code, err.Error())
}
