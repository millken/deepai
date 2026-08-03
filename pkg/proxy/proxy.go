package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/millken/deepai/pkg/netutil"
)

const (
	defaultAddr            = ":9090"
	defaultShutdownTimeout = 15 * time.Second
	defaultMaxRequestBody  = 10 << 20 // 10 MB
	logChannelSize         = 1024
)

// UpstreamConfig configures an upstream API endpoint.
type UpstreamConfig struct {
	BaseURL string // e.g. "https://api.openai.com/v1"
	APIKey  string // Bearer token or x-api-key value
}

// Config holds proxy configuration.
type Config struct {
	Addr            string
	ShutdownTimeout time.Duration
	MaxRequestBody  int64
	OpenAI          UpstreamConfig
	Anthropic       UpstreamConfig
}

// Proxy is a transparent reverse proxy for OpenAI/Anthropic APIs
// that logs all requests and responses as event streams.
type Proxy struct {
	cfg          Config
	httpServer   *http.Server
	logger       *slog.Logger
	store        EventStore
	storeMu      sync.RWMutex
	httpClient   *http.Client
	logCh        chan []LogEvent
	logDone      chan struct{}
	shutdownOnce sync.Once
	shuttingDown atomic.Bool
}

var requestSeq uint64

type contextKey string

const requestIDKey contextKey = "request_id"

// NewProxy creates a new Proxy with the given configuration.
func NewProxy(logger *slog.Logger, cfg Config) (*Proxy, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		cfg.Addr = defaultAddr
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = defaultShutdownTimeout
	}
	if cfg.MaxRequestBody <= 0 {
		cfg.MaxRequestBody = defaultMaxRequestBody
	}

	p := &Proxy{
		cfg:    cfg,
		logger: logger,
		store:  NewMemoryEventStore(),
		httpClient: &http.Client{
			Transport: &http.Transport{
				// Reach the upstream APIs through the user's proxy when one is
				// configured; a hand-built Transport has no proxy support unless
				// Proxy is set. See pkg/netutil for why not
				// http.ProxyFromEnvironment.
				Proxy:                 netutil.EnvProxyFunc,
				ResponseHeaderTimeout: 30 * time.Second,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   10,
				IdleConnTimeout:       90 * time.Second,
			},
		},
		logCh:   make(chan []LogEvent, logChannelSize),
		logDone: make(chan struct{}),
	}

	p.httpServer = &http.Server{
		Addr:              cfg.Addr,
		Handler:           p.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// logWorker uses context.Background() so that shutdown drain is never
	// interrupted by a cancelled context — queued batches must persist.
	go p.logWorker()

	return p, nil
}

// logWorker reads event batches from the channel and persists them.
// Uses context.Background() intentionally: during shutdown the channel is
// closed first, and all remaining batches must be drained regardless of
// any external context cancellation.
func (p *Proxy) logWorker() {
	defer close(p.logDone)

	for batch := range p.logCh {
		store := p.getStore()
		if err := store.Append(context.Background(), batch...); err != nil {
			p.logger.Error("proxy log save failed", "err", err)
		}
	}
}

// WithStore replaces the default in-memory store with a custom EventStore.
// Must be called before ListenAndServe.
func (p *Proxy) WithStore(store EventStore) *Proxy {
	if store != nil {
		p.storeMu.Lock()
		p.store = store
		p.storeMu.Unlock()
	}
	return p
}

func (p *Proxy) getStore() EventStore {
	p.storeMu.RLock()
	defer p.storeMu.RUnlock()
	return p.store
}

// Handler returns the http.Handler for embedding in another server.
func (p *Proxy) Handler() http.Handler {
	return p.routes()
}

// ListenAndServe starts the proxy server.
func (p *Proxy) ListenAndServe() error {
	if p == nil || p.httpServer == nil {
		return errors.New("proxy server is not initialized")
	}
	p.logger.Info("proxy listening", "addr", p.httpServer.Addr)
	err := p.httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown gracefully shuts down the proxy server.
func (p *Proxy) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	var shutdownErr error
	p.shutdownOnce.Do(func() {
		p.shuttingDown.Store(true)
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, p.cfg.ShutdownTimeout)
			defer cancel()
		}
		if p.httpServer != nil {
			shutdownErr = p.httpServer.Shutdown(ctx)
		}
		close(p.logCh)
		<-p.logDone
	})
	return shutdownErr
}

func (p *Proxy) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", p.handleHealth)
	mux.HandleFunc("POST /v1/chat/completions", p.handleProxy)
	mux.HandleFunc("POST /v1/messages", p.handleProxy)
	return p.withMiddleware(mux)
}

func (p *Proxy) withMiddleware(next http.Handler) http.Handler {
	return p.withRecover(p.withRequestID(p.withLogging(next)))
}

func (p *Proxy) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// debugHeaders indicates whether to dump request headers in logs.
// Controlled by the DEEPAI_DEBUG environment variable.
var debugHeaders = os.Getenv("DEEPAI_DEBUG") != ""

func (p *Proxy) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		if debugHeaders {
			for k, vv := range r.Header {
				for _, v := range vv {
					p.logger.Debug("header", "key", k, "value", v)
				}
			}
		}
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		p.logger.Info("request",
			"method", r.Method, "path", r.URL.Path,
			"status", rw.status,
			"duration", time.Since(started).Round(time.Millisecond),
			"request_id", requestIDFromCtx(r.Context()))
	})
}

func (p *Proxy) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				p.logger.Error("panic", "request_id", requestIDFromCtx(r.Context()), "err", rec)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// emitEvents sends a batch of log events to the background writer channel.
// Non-blocking: if the channel is full or the proxy is shutting down,
// the batch is dropped and a warning is logged.
func (p *Proxy) emitEvents(events []LogEvent) {
	if p.shuttingDown.Load() || len(events) == 0 {
		return
	}
	select {
	case p.logCh <- events:
	default:
		p.logger.Warn("proxy log channel full, dropping events", "count", len(events))
	}
}

func (p *Proxy) upstreamURL(apiFormat, path string) string {
	var base string
	switch apiFormat {
	case "anthropic":
		base = strings.TrimRight(p.cfg.Anthropic.BaseURL, "/")
	default:
		base = strings.TrimRight(p.cfg.OpenAI.BaseURL, "/")
	}
	return base + path
}

func (p *Proxy) upstreamAPIKey(apiFormat string) string {
	if apiFormat == "anthropic" {
		return p.cfg.Anthropic.APIKey
	}
	return p.cfg.OpenAI.APIKey
}

func newRequestID() string {
	seq := atomic.AddUint64(&requestSeq, 1)
	return fmt.Sprintf("%d-%08x", seq, rand.Uint32())
}

func requestIDFromCtx(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
