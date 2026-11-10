package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// wsMaxFrame bounds one inbound WebSocket message.
const wsMaxFrame = 4 << 20

// WSRequest is a client → server WebSocket frame. ID is echoed in the
// matching reply so a client can correlate concurrent requests.
//
// Ops: ping, info, list_sessions, create_session, get_session,
// delete_session, rename_session, subscribe, unsubscribe, send, cancel,
// answer, plus the schedule_* ops in schedule_http.go. subscribe with
// session_id "*" follows the bot-wide lifecycle feed.
type WSRequest struct {
	ID         string `json:"id,omitempty"`
	Op         string `json:"op"`
	SessionID  string `json:"session_id,omitempty"`
	Since      *int64 `json:"since,omitempty"`
	Content    string `json:"content,omitempty"`
	Model      string `json:"model,omitempty"`
	Title      string `json:"title,omitempty"`
	QuestionID string `json:"question_id,omitempty"`
	Answer     string `json:"answer,omitempty"`
	Limit      int    `json:"limit,omitempty"`

	// Schedule ops (docs/BOT_SCHEDULER_DESIGN.md §9.3). Params mirrors the
	// REST body; ScheduleID/RunID keep ids out of the correlation ID field.
	ScheduleID string         `json:"schedule_id,omitempty"`
	RunID      string         `json:"run_id,omitempty"`
	Params     map[string]any `json:"params,omitempty"`
}

// WSMessage is a server → client WebSocket frame: either a reply to a
// request (Kind "reply") or a pushed event (Kind "event").
//
// Sub names the subscription an event belongs to ("*" for the lifecycle
// feed, else the session id). Each feed has its own Seq counter, so clients
// must track their resume cursor per Sub.
type WSMessage struct {
	Kind  string    `json:"kind"`
	ID    string    `json:"id,omitempty"`
	Sub   string    `json:"sub,omitempty"`
	OK    bool      `json:"ok,omitempty"`
	Data  any       `json:"data,omitempty"`
	Error *apiError `json:"error,omitempty"`
	Event *Event    `json:"event,omitempty"`
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	srv := websocket.Server{
		// Origin was already vetted by Server.ServeHTTP; the library's
		// default handshake would additionally reject requests without an
		// Origin header, which native (non-browser) clients do not send.
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler: func(conn *websocket.Conn) {
			conn.MaxPayloadBytes = wsMaxFrame
			newWSConn(s, conn).serve(r.Context())
		},
	}
	srv.ServeHTTP(w, r)
}

type wsConn struct {
	s    *Server
	conn *websocket.Conn
	out  chan WSMessage

	mu   sync.Mutex
	subs map[string]*Subscription
}

func newWSConn(s *Server, conn *websocket.Conn) *wsConn {
	return &wsConn{s: s, conn: conn, out: make(chan WSMessage, 256), subs: map[string]*Subscription{}}
}

func (c *wsConn) serve(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer c.closeSubs()

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		c.writeLoop(ctx)
	}()

	for {
		var req WSRequest
		if err := websocket.JSON.Receive(c.conn, &req); err != nil {
			var syn *json.SyntaxError
			if errors.As(err, &syn) {
				c.send(ctx, WSMessage{Kind: "reply", Error: &apiError{Code: "bad_request", Message: err.Error()}})
				continue
			}
			break
		}
		c.handle(ctx, req)
	}
	cancel()
	<-writerDone
}

func (c *wsConn) writeLoop(ctx context.Context) {
	hb := time.NewTicker(heartbeatInterval)
	defer hb.Stop()
	for {
		var msg WSMessage
		select {
		case <-ctx.Done():
			_ = c.conn.Close()
			return
		case <-hb.C:
			msg = WSMessage{Kind: "event", Event: &Event{Type: EventHeartbeat, Time: time.Now().UTC()}}
		case msg = <-c.out:
		}
		if err := websocket.JSON.Send(c.conn, msg); err != nil {
			_ = c.conn.Close()
			return
		}
	}
}

// send queues msg, blocking while the client is slow. A forwarder blocked
// here stops draining its Subscription, so the bus eventually drops it as
// lagged and the client is told to resync — the bot itself never stalls.
func (c *wsConn) send(ctx context.Context, msg WSMessage) {
	select {
	case c.out <- msg:
	case <-ctx.Done():
	}
}

func (c *wsConn) reply(ctx context.Context, req WSRequest, data any, err error) {
	msg := WSMessage{Kind: "reply", ID: req.ID}
	if err != nil {
		_, code := errorStatus(err)
		msg.Error = &apiError{Code: code, Message: err.Error()}
	} else {
		msg.OK = true
		msg.Data = data
	}
	c.send(ctx, msg)
}

func (c *wsConn) handle(ctx context.Context, req WSRequest) {
	m := c.s.m
	switch req.Op {
	case "ping":
		c.reply(ctx, req, "pong", nil)
	case "info":
		c.reply(ctx, req, m.Info(), nil)
	case "list_sessions":
		list, err := m.ListSessions(req.Limit)
		c.reply(ctx, req, map[string]any{"sessions": list}, err)
	case "create_session":
		info, err := m.CreateSession(CreateSessionRequest{Title: req.Title, Model: req.Model})
		c.reply(ctx, req, info, err)
	case "get_session":
		info, msgs, err := m.GetSession(req.SessionID)
		c.reply(ctx, req, map[string]any{"session": info, "messages": msgs}, err)
	case "rename_session":
		info, err := m.RenameSession(req.SessionID, req.Title)
		c.reply(ctx, req, info, err)
	case "delete_session":
		c.reply(ctx, req, nil, m.DeleteSession(req.SessionID))
	case "subscribe":
		c.reply(ctx, req, nil, c.subscribe(ctx, req))
	case "unsubscribe":
		c.mu.Lock()
		if sub := c.subs[req.SessionID]; sub != nil {
			sub.Close()
			delete(c.subs, req.SessionID)
		}
		c.mu.Unlock()
		c.reply(ctx, req, nil, nil)
	case "send":
		turn, err := m.Send(req.SessionID, SendRequest{Content: req.Content, Model: req.Model})
		var data any
		if turn != nil {
			data = map[string]string{"session_id": req.SessionID, "turn_id": turn.ID}
		}
		c.reply(ctx, req, data, err)
	case "cancel":
		c.reply(ctx, req, nil, m.Cancel(req.SessionID))
	case "answer":
		c.reply(ctx, req, nil, m.Answer(req.SessionID, req.QuestionID, req.Answer))
	default:
		if data, handled, err := c.s.handleScheduleWS(ctx, req); handled {
			c.reply(ctx, req, data, err)
			return
		}
		c.reply(ctx, req, nil, fmt.Errorf("unknown op %q", req.Op))
	}
}

func (c *wsConn) subscribe(ctx context.Context, req WSRequest) error {
	since := int64(-1)
	if req.Since != nil {
		since = *req.Since
	}
	key := req.SessionID
	var (
		sub     *Subscription
		backlog []Event
		gap     bool
	)
	if key == "*" || key == "" {
		key = "*"
		sub, backlog, gap = c.s.m.SubscribeAll(since)
	} else {
		var err error
		sub, backlog, gap, err = c.s.m.Subscribe(key, since)
		if err != nil {
			return err
		}
	}
	c.mu.Lock()
	if old := c.subs[key]; old != nil {
		old.Close()
	}
	c.subs[key] = sub
	c.mu.Unlock()

	sessionID := key
	if sessionID == "*" {
		sessionID = ""
	}
	push := func(e Event) { c.send(ctx, WSMessage{Kind: "event", Sub: key, Event: &e}) }
	go func() {
		if gap {
			push(Event{Type: EventGap, SessionID: sessionID})
		}
		for _, e := range backlog {
			push(e)
		}
		for e := range sub.C {
			push(e)
		}
		if sub.Lagged() {
			push(Event{Type: EventGap, SessionID: sessionID})
		}
	}()
	return nil
}

func (c *wsConn) closeSubs() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, sub := range c.subs {
		sub.Close()
		delete(c.subs, k)
	}
}
