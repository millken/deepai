package mcp

import (
	"context"
	"fmt"
	"sync"

	"github.com/millken/deepai/pkg/models"
)

// lazyClient owns one MCP server connection that is established in the
// background instead of blocking REPL startup: warm-cache sessions register
// tools from the local cache and dial concurrently, so a slow TLS handshake
// or npx subprocess launch overlaps with the user typing. Tool handlers go
// through get(): it returns the connected client, waits for an in-flight
// dial, or — after a failed background dial — redials once synchronously so
// a transient startup failure is not permanent for the whole session.
//
// ctx discipline: connectServer binds the ctx it receives to the transport
// lifetime (stdio binds its subprocess), so every dial MUST use the
// long-lived session ctx captured here, never a per-call ctx; a caller's ctx
// only bounds how long get() waits.
type lazyClient struct {
	name        string
	sc          ServerConfig
	sessionCtx  context.Context
	postConnect func(*Client) // cache reconcile after a successful dial; may be nil

	mu      sync.Mutex
	client  *Client
	lastErr error
	closed  bool
	dialing bool          // an attempt is in flight
	current chan struct{} // closed when the in-flight attempt finishes
}

func startLazyClient(ctx context.Context, name string, sc ServerConfig, postConnect func(*Client)) *lazyClient {
	lc := &lazyClient{name: name, sc: sc, sessionCtx: ctx, postConnect: postConnect}
	lc.startAttempt()
	return lc
}

// startAttempt launches a dial goroutine. Idempotent: if an attempt is
// already in flight or a client is connected, it does nothing — concurrent
// get() callers on a dead server must not spawn one subprocess each, and
// the loser's client would leak unclosed.
func (lc *lazyClient) startAttempt() {
	lc.mu.Lock()
	if lc.closed || lc.dialing || lc.client != nil {
		lc.mu.Unlock()
		return
	}
	done := make(chan struct{})
	lc.dialing = true
	lc.current = done
	lc.mu.Unlock()
	go func() {
		c, err := connectServer(lc.sessionCtx, lc.name, lc.sc)
		lc.mu.Lock()
		lc.dialing = false
		if lc.closed {
			lc.mu.Unlock()
			if c != nil {
				_ = c.Close()
			}
			close(done)
			return
		}
		lc.client, lc.lastErr = c, err
		lc.mu.Unlock()
		close(done)
		if c != nil && err == nil && lc.postConnect != nil {
			lc.postConnect(c)
		}
	}()
}

func (lc *lazyClient) get(ctx context.Context) (*Client, error) {
	redials := 0
	for {
		lc.mu.Lock()
		switch {
		case lc.closed:
			lc.mu.Unlock()
			return nil, fmt.Errorf("mcp server %q is shut down", lc.name)
		case lc.client != nil:
			c := lc.client
			lc.mu.Unlock()
			return c, nil
		case lc.dialing:
			done := lc.current
			lc.mu.Unlock()
			select {
			case <-done:
			case <-ctx.Done():
				return nil, fmt.Errorf("mcp server %q connect: %w", lc.name, ctx.Err())
			}
		default:
			err := lc.lastErr
			lc.mu.Unlock()
			if redials >= 1 {
				if err == nil {
					err = fmt.Errorf("connection lost")
				}
				return nil, fmt.Errorf("mcp server %q connect: %w", lc.name, err)
			}
			redials++
			lc.startAttempt()
		}
	}
}

func (lc *lazyClient) Close() {
	lc.mu.Lock()
	lc.closed = true
	c := lc.client
	lc.client = nil
	lc.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

// handlerFor adapts one remote tool (registered from cache) to a live call:
// fetch or establish the connection, then delegate to the plain client
// handler for that remote name.
func (lc *lazyClient) handlerFor(remote string) models.ToolHandler {
	return func(ctx context.Context, call models.ToolCall) (models.ToolResult, error) {
		c, err := lc.get(ctx)
		if err != nil {
			return models.ToolResult{
				CallID:   call.ID,
				ToolName: call.Name,
				Status:   models.CallStatusFailed,
				Error:    fmt.Sprintf("mcp server %q unavailable: %v", lc.name, err),
			}, err
		}
		return c.handler(remote)(ctx, call)
	}
}
