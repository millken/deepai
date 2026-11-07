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
	"strconv"
	"strings"
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
	// DisableWebUI stops serving the embedded console at /.
	DisableWebUI bool
	Logger       *slog.Logger
}

// Server is the bot's http.Handler.
type Server struct {
	opts      ServerOptions
	m         *Manager
	schedules *schedule.Service
	log       *slog.Logger
	mux       *http.ServeMux
}

// NewServer builds the handler. Routes are documented in docs/BOT_DESIGN.md.
func NewServer(opts ServerOptions) *Server {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	s := &Server{opts: opts, m: opts.Manager, schedules: opts.Schedules, log: log, mux: http.NewServeMux()}

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.Handle("GET /v1/info", s.auth(s.handleInfo))
	s.mux.Handle("GET /v1/sessions", s.auth(s.handleListSessions))
	s.mux.Handle("POST /v1/sessions", s.auth(s.handleCreateSession))
	s.mux.Handle("GET /v1/sessions/{id}", s.auth(s.handleGetSession))
	s.mux.Handle("PATCH /v1/sessions/{id}", s.auth(s.handleRenameSession))
	s.mux.Handle("DELETE /v1/sessions/{id}", s.auth(s.handleDeleteSession))
	s.mux.Handle("POST /v1/sessions/{id}/messages", s.auth(s.handleSend))
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
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Last-Event-ID")
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
		if s.opts.Token != "" {
			got := r.URL.Query().Get("token")
			if v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
				got = strings.TrimSpace(v)
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.opts.Token)) != 1 {
				writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid token")
				return
			}
		}
		h(w, r)
	})
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.m.Info())
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.m.ListSessions(limit)
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
	info, err := s.m.CreateSession(req)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	info, msgs, err := s.m.GetSession(r.PathValue("id"))
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
	info, err := s.m.RenameSession(r.PathValue("id"), req.Title)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if err := s.m.DeleteSession(r.PathValue("id")); err != nil {
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
	id := r.PathValue("id")
	var req SendRequest
	if !readJSON(w, r, &req, false) {
		return
	}
	mode := streamMode(r)

	var sub *Subscription
	if mode != "" {
		var err error
		sub, _, _, err = s.m.Subscribe(id, -1)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		defer sub.Close()
	}
	turn, err := s.m.Send(id, req)
	if err != nil {
		s.writeErr(w, err)
		return
	}

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

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.m.Cancel(r.PathValue("id")); err != nil {
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
	if err := s.m.Answer(r.PathValue("id"), req.QuestionID, req.Answer); err != nil {
		s.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	sub, backlog, gap, err := s.m.Subscribe(r.PathValue("id"), sinceParam(r))
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
	case errors.Is(err, ErrBusy):
		return http.StatusConflict, "busy"
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
