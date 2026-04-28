package logs

import (
	"io"
	"log/slog"
	"os"
	"sync"
)

var (
	setupMu sync.Mutex
	closers []io.Closer
)

// Setup configures the global slog default handler.
// Optional — if not called, slog uses its built-in default.
// Returns a cleanup function that closes all resources (async workers, files).
// The cleanup function is idempotent (safe to call multiple times).
func Setup(cfg Config) (func(), error) {
	setupMu.Lock()
	defer setupMu.Unlock()

	// Close previous resources if Setup was called before.
	closeClosers()

	stderrH := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.stderrMin()})

	if cfg.DebugFile == "" && cfg.ErrorFile == "" {
		slog.SetDefault(slog.New(stderrH))
		return func() {}, nil
	}

	var routes []route

	if cfg.DebugFile != "" {
		f, err := os.OpenFile(cfg.DebugFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, err
		}
		fileH := slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})
		asyncH := NewAsyncHandler(fileH, 256)
		closers = append(closers, f, asyncH)
		routes = append(routes, route{min: slog.LevelDebug - 4, max: slog.Level(100), handler: asyncH})
	}

	if cfg.ErrorFile != "" {
		f, err := os.OpenFile(cfg.ErrorFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			closeClosers()
			return nil, err
		}
		fileH := slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelWarn})
		asyncH := NewAsyncHandler(fileH, 256)
		closers = append(closers, f, asyncH)
		routes = append(routes, route{min: slog.LevelWarn, max: slog.Level(100), handler: asyncH})
	}

	routes = append(routes, route{min: cfg.stderrMin(), max: slog.Level(100), handler: stderrH})

	router := NewRouterHandler(routes...)
	slog.SetDefault(slog.New(router))

	var cleanupOnce sync.Once
	return func() {
		cleanupOnce.Do(func() {
			setupMu.Lock()
			defer setupMu.Unlock()
			closeClosers()
		})
	}, nil
}

func closeClosers() {
	for i := len(closers) - 1; i >= 0; i-- {
		closers[i].Close()
	}
	closers = nil
}
