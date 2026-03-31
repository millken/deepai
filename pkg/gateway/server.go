package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/millken/deepai/pkg/agent"
	"github.com/millken/deepai/pkg/checkpoint"
	"github.com/millken/deepai/pkg/llm"
	"github.com/millken/deepai/pkg/models"
	"github.com/millken/deepai/pkg/sandbox"
	"github.com/millken/deepai/pkg/tools"
	"github.com/millken/deepai/pkg/tools/builtin"
)

const (
	defaultAddr            = ":8080"
	defaultModelRef        = "openai/gpt-4o"
	defaultShutdownTimeout = 15 * time.Second
	defaultMaxTurns        = 8
	defaultSandboxRoot     = "/tmp/deerflow-gateway-sandbox"
)

type Config struct {
	Addr            string
	DatabaseURL     string
	DefaultModel    string
	Logger          *log.Logger
	ShutdownTimeout time.Duration
}

type Server struct {
	cfg             Config
	httpServer      *http.Server
	logger          *log.Logger
	store           sessionStore
	tools           *tools.Registry
	sandbox         *sandbox.Sandbox
	providerMu      sync.Mutex
	providers       map[string]llm.LLMProvider
	providerFactory func(string) (llm.LLMProvider, error)
	cleanupFns      []func()
	shutdownOnce    sync.Once
	startedAt       time.Time
	shutdownTimeout time.Duration
	inFlight        sync.WaitGroup
	inFlightCount   int64
	shuttingDown    atomic.Bool
}

type sessionStore interface {
	Load(ctx context.Context, sessionID string) ([]models.Message, error)
	Save(ctx context.Context, session models.Session) error
}

type memoryStore struct {
	mu       sync.RWMutex
	sessions map[string]models.Session
}

type postgresSessionStore struct {
	store *checkpoint.PostgresStore
}

var messageSeq uint64

func NewServer(cfg Config) (*Server, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		cfg.Addr = defaultAddr
	}
	if strings.TrimSpace(cfg.DefaultModel) == "" {
		cfg.DefaultModel = defaultModelRef
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(os.Stderr, "gateway ", log.LstdFlags)
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = defaultShutdownTimeout
	}

	sb, err := sandbox.New("gateway", defaultSandboxRoot)
	if err != nil {
		return nil, fmt.Errorf("create sandbox: %w", err)
	}

	registry := tools.NewRegistry()
	registerBuiltins(registry)

	store := sessionStore(newMemoryStore())
	cleanupFns := []func(){func() { _ = sb.Close() }}
	if strings.TrimSpace(cfg.DatabaseURL) != "" {
		pgStore, err := checkpoint.NewPostgresStore(context.Background(), cfg.DatabaseURL)
		if err != nil {
			for _, fn := range cleanupFns {
				fn()
			}
			return nil, fmt.Errorf("create postgres store: %w", err)
		}
		store = &postgresSessionStore{store: pgStore}
		cleanupFns = append(cleanupFns, pgStore.Close)
	}

	s := &Server{
		cfg:             cfg,
		logger:          cfg.Logger,
		store:           store,
		tools:           registry,
		sandbox:         sb,
		providers:       make(map[string]llm.LLMProvider),
		providerFactory: defaultProviderFactory,
		cleanupFns:      cleanupFns,
		startedAt:       time.Now().UTC(),
		shutdownTimeout: cfg.ShutdownTimeout,
	}

	s.httpServer = &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	return s, nil
}

func (s *Server) ListenAndServe() error {
	if s == nil || s.httpServer == nil {
		return errors.New("gateway server is not initialized")
	}
	s.logger.Printf("gateway listening on %s", s.httpServer.Addr)
	err := s.httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}

	var shutdownErr error
	s.shutdownOnce.Do(func() {
		s.shuttingDown.Store(true)
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, s.shutdownTimeout)
			defer cancel()
		}
		started := time.Now()
		s.logger.Printf("gateway shutdown started inflight=%d", atomic.LoadInt64(&s.inFlightCount))

		if s.httpServer != nil {
			shutdownErr = s.httpServer.Shutdown(ctx)
		}

		drained := make(chan struct{})
		go func() {
			s.inFlight.Wait()
			close(drained)
		}()
		select {
		case <-drained:
		case <-ctx.Done():
			if shutdownErr == nil {
				shutdownErr = ctx.Err()
			}
		}

		for i := len(s.cleanupFns) - 1; i >= 0; i-- {
			s.cleanupFns[i]()
		}
		s.logger.Printf(
			"gateway shutdown finished duration=%s inflight=%d uptime=%s err=%v",
			time.Since(started).Round(time.Millisecond),
			atomic.LoadInt64(&s.inFlightCount),
			time.Since(s.startedAt).Round(time.Second),
			shutdownErr,
		)
	})
	return shutdownErr
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /api/v1/chat", s.handleChat)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	return s.withMiddleware(mux)
}

func (s *Server) newRuntime(modelRef string, allowedTools []string) (*agent.Agent, string, error) {
	providerName, modelName := splitModelRef(modelRef)
	provider, err := s.providerFor(providerName)
	if err != nil {
		return nil, "", err
	}

	return agent.New(agent.AgentConfig{
		LLMProvider: provider,
		Tools:       s.tools.Restrict(allowedTools),
		Sandbox:     s.sandbox,
		MaxTurns:    defaultMaxTurns,
		Model:       modelName,
	}), modelName, nil
}

func (s *Server) providerFor(name string) (llm.LLMProvider, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "openai"
	}

	s.providerMu.Lock()
	defer s.providerMu.Unlock()

	if provider, ok := s.providers[name]; ok {
		return provider, nil
	}

	provider, err := s.providerFactory(name)
	if err != nil {
		return nil, err
	}
	s.providers[name] = provider
	return provider, nil
}

func defaultProviderFactory(name string) (llm.LLMProvider, error) {
	provider := llm.NewProvider(name)
	if provider == nil {
		return nil, fmt.Errorf("unsupported llm provider %q", name)
	}
	if unavailable, ok := provider.(*llm.UnavailableProvider); ok {
		_, err := unavailable.Chat(context.Background(), llm.ChatRequest{})
		if err != nil {
			return nil, err
		}
	}
	return provider, nil
}

func registerBuiltins(registry *tools.Registry) {
	mustRegister(registry, builtin.BashTool())
	for _, tool := range builtin.FileTools() {
		mustRegister(registry, tool)
	}
}

func mustRegister(registry *tools.Registry, tool models.Tool) {
	if err := registry.Register(tool); err != nil {
		panic(fmt.Sprintf("register tool %s: %v", tool.Name, err))
	}
}

func newMemoryStore() *memoryStore {
	return &memoryStore{sessions: make(map[string]models.Session)}
}

func (s *memoryStore) Load(_ context.Context, sessionID string) ([]models.Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	session, ok := s.sessions[sessionID]
	if !ok {
		return nil, nil
	}
	return append([]models.Message(nil), session.Messages...), nil
}

func (s *memoryStore) Save(_ context.Context, session models.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[session.ID] = session
	return nil
}

func (s *postgresSessionStore) Load(ctx context.Context, sessionID string) ([]models.Message, error) {
	session, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return session.Messages, nil
}

func (s *postgresSessionStore) Save(ctx context.Context, session models.Session) error {
	return s.store.SaveSession(ctx, session)
}

func splitModelRef(modelRef string) (string, string) {
	modelRef = strings.TrimSpace(modelRef)
	if modelRef == "" {
		modelRef = defaultModelRef
	}

	parts := strings.SplitN(modelRef, "/", 2)
	if len(parts) == 2 {
		switch parts[0] {
		case "openai", "anthropic", "siliconflow":
			if strings.TrimSpace(parts[1]) != "" {
				return parts[0], strings.TrimSpace(parts[1])
			}
		}
	}
	return "openai", modelRef
}

func newMessageID(prefix string) string {
	seq := atomic.AddUint64(&messageSeq, 1)
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UTC().UnixNano(), seq)
}

func defaultUserID(userID string) string {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return "local"
	}
	return userID
}

func defaultSessionID(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID != "" {
		return sessionID
	}
	return newMessageID("session")
}

func firstCreatedAt(messages []models.Message) time.Time {
	if len(messages) == 0 {
		return time.Now().UTC()
	}
	return messages[0].CreatedAt
}
