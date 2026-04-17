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
	"github.com/millken/deepai/pkg/memory"
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
	defaultSandboxRoot     = "/tmp/deepai-gateway-sandbox"
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
	memService      *memory.Service
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
	LoadSession(ctx context.Context, sessionID string) (models.Session, error)
	Save(ctx context.Context, session models.Session) error
}

type memoryStore struct {
	mu       sync.RWMutex
	sessions map[string]models.Session
}

type postgresSessionStore struct {
	store *checkpoint.PostgresStore
}

// postgresSearchAdapter adapts checkpoint.PostgresStore.SearchMessages to builtin.MessageSearcher.
type postgresSearchAdapter struct {
	store *checkpoint.PostgresStore
}

func (a postgresSearchAdapter) SearchMessages(ctx context.Context, query string, limit int) ([]builtin.SearchHit, error) {
	results, err := a.store.SearchMessages(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	hits := make([]builtin.SearchHit, len(results))
	for i, r := range results {
		hits[i] = builtin.SearchHit{
			SessionID: r.SessionID,
			MessageID: r.MessageID,
			Role:      r.Role,
			Content:   r.Content,
		}
	}
	return hits, nil
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

	var memService *memory.Service
	if memDBURL := strings.TrimSpace(cfg.DatabaseURL); memDBURL != "" {
		memStore, err := memory.OpenStore(context.Background(), memDBURL)
		if err != nil {
			for _, fn := range cleanupFns {
				fn()
			}
			return nil, fmt.Errorf("create memory store: %w", err)
		}
		cleanupFns = append(cleanupFns, memStore.Close)
		memService = memory.NewService(memStore, nil)
		// Register memService.Close AFTER memStore.Close so that on reversed
		// cleanup the queue drains first, then the underlying store closes.
		cleanupFns = append(cleanupFns, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = memService.Close(ctx)
		})
		if err := memService.AutoMigrate(context.Background()); err != nil {
			cfg.Logger.Printf("memory auto-migrate warning: %v", err)
		}
	}

	s := &Server{
		cfg:             cfg,
		logger:          cfg.Logger,
		store:           store,
		tools:           registry,
		sandbox:         sb,
		memService:      memService,
		providers:       make(map[string]llm.LLMProvider),
		providerFactory: defaultProviderFactory,
		cleanupFns:      cleanupFns,
		startedAt:       time.Now().UTC(),
		shutdownTimeout: cfg.ShutdownTimeout,
	}

	if memService != nil {
		if err := registry.Register(builtin.MemoryTool(memService)); err != nil {
			for _, fn := range cleanupFns {
				fn()
			}
			return nil, fmt.Errorf("register memory tool: %w", err)
		}
	}

	if pgS, ok := store.(*postgresSessionStore); ok {
		if err := registry.Register(builtin.SessionSearchTool(
			postgresSearchAdapter{store: pgS.store},
		)); err != nil {
			for _, fn := range cleanupFns {
				fn()
			}
			return nil, fmt.Errorf("register session search tool: %w", err)
		}
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

func (s *Server) newRuntime(modelRef string, allowedTools []string, userID string) (*agent.Agent, string, memory.Extractor, error) {
	providerName, modelName := splitModelRef(modelRef)
	provider, err := s.providerFor(providerName)
	if err != nil {
		return nil, "", nil, err
	}

	cfg := agent.AgentConfig{
		LLMProvider: provider,
		Tools:       s.tools.Restrict(allowedTools),
		Sandbox:     s.sandbox,
		MaxTurns:    defaultMaxTurns,
		Model:       modelName,
	}

	var ext memory.Extractor
	if s.memService != nil {
		cfg.MemoryService = s.memService
		ext = memory.NewLLMClient(provider, modelName)
		cfg.MemoryExtractor = ext
		cfg.MemoryUserID = userID
	}

	return agent.New(cfg), modelName, ext, nil
}

// userIDFromContext extracts the user ID from the chat request for memory scoping.
func userIDForRequest(req chatRequest) string {
	return strings.TrimSpace(req.UserID)
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
	for _, tool := range builtin.WebTools() {
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

func (s *memoryStore) LoadSession(_ context.Context, sessionID string) (models.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	session, ok := s.sessions[sessionID]
	if !ok {
		return models.Session{ID: sessionID, Metadata: map[string]string{}}, nil
	}
	return cloneSession(session), nil
}

func cloneSession(s models.Session) models.Session {
	c := s
	if s.Messages != nil {
		c.Messages = make([]models.Message, len(s.Messages))
		copy(c.Messages, s.Messages)
	}
	if s.Metadata != nil {
		c.Metadata = make(map[string]string, len(s.Metadata))
		for k, v := range s.Metadata {
			c.Metadata[k] = v
		}
	}
	return c
}

func (s *memoryStore) Save(_ context.Context, session models.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[session.ID] = session
	return nil
}

func (s *postgresSessionStore) LoadSession(ctx context.Context, sessionID string) (models.Session, error) {
	session, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return models.Session{ID: sessionID, Metadata: map[string]string{}}, nil
		}
		return models.Session{}, err
	}
	return session, nil
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
