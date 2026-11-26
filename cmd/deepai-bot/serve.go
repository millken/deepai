package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/bot"
	"github.com/millken/deepai/pkg/logs"
	"github.com/millken/deepai/pkg/schedule"
	"github.com/spf13/cobra"
)

const defaultAddr = "127.0.0.1:8787"

type serveFlags struct {
	addr         string
	token        string
	noAuth       bool
	workDir      string
	model        string
	maxToolCalls int
	ephemeral    bool
	corsOrigins  []string
	noWeb        bool
	turnTimeout  time.Duration
	idleTimeout  time.Duration
	verbose      bool
	noSchedule   bool
	metricsAddr  string
}

func newRootCmd() *cobra.Command {
	var f serveFlags
	root := &cobra.Command{
		Use:          "deepai-bot",
		Short:        "Run the deepai agent as a background service (HTTP / SSE / WebSocket)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServe(cmd.Context(), f)
		},
	}
	registerServeFlags(root, &f)

	serve := &cobra.Command{
		Use:   "serve",
		Short: "Run the bot in the foreground (for systemd, launchd, Docker)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServe(cmd.Context(), f)
		},
	}
	registerServeFlags(serve, &f)

	start := &cobra.Command{
		Use:   "start [serve flags]",
		Short: "Start the bot detached in the background",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStart(cmd.Context(), f)
		},
	}
	registerServeFlags(start, &f)

	root.AddCommand(serve, start, newStopCmd(), newStatusCmd(), newTokenCmd(), newSecretCmd(), &cobra.Command{
		Use:   "version",
		Short: "Print the build version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(buildVersion())
		},
	})
	root.CompletionOptions.HiddenDefaultCmd = true
	return root
}

func registerServeFlags(cmd *cobra.Command, f *serveFlags) {
	fl := cmd.Flags()
	fl.StringVar(&f.addr, "addr", envOr("DEEPAI_BOT_ADDR", defaultAddr), "Listen address")
	fl.StringVar(&f.token, "token", "", "API token (default: $DEEPAI_BOT_TOKEN, else ~/.deepai/bot/token, generated on first run)")
	fl.BoolVar(&f.noAuth, "no-auth", false, "Disable token auth (only allowed on a loopback address)")
	fl.StringVarP(&f.workDir, "workdir", "w", "", "Directory the agent works in (default: current directory)")
	fl.StringVarP(&f.model, "model", "m", "", "Default model alias (overrides config)")
	fl.IntVar(&f.maxToolCalls, "max-tool-calls", 0, "Max executed tool calls per turn (0 = unlimited)")
	fl.BoolVar(&f.ephemeral, "ephemeral", false, "Keep sessions in memory only (no SQLite, no long-term memory)")
	fl.StringSliceVar(&f.corsOrigins, "cors-origin", nil, "Extra browser origin allowed to call the API (repeatable, '*' for any)")
	fl.BoolVar(&f.noWeb, "no-web", false, "Do not serve the built-in web console")
	fl.DurationVar(&f.turnTimeout, "turn-timeout", 0, "Abort a turn after this long (0 = no limit)")
	fl.DurationVar(&f.idleTimeout, "idle-timeout", 30*time.Minute, "Unload a session after this long without activity")
	fl.BoolVar(&f.noSchedule, "no-schedule", false, "Disable timed tasks (do not register the schedule tool or start the scheduler)")
	fl.StringVar(&f.metricsAddr, "metrics-addr", "", "Optional dedicated address for Prometheus /metrics (e.g. 127.0.0.1:9090)")
	fl.BoolVarP(&f.verbose, "verbose", "v", false, "Debug logging")
}

func runServe(ctx context.Context, f serveFlags) error {
	level := slog.LevelInfo
	if f.verbose {
		level = slog.LevelDebug
	}
	if cleanup, err := logs.Setup(logs.Config{Level: level, StderrLevel: level}); err == nil {
		defer cleanup()
	}
	loadEnvFile()

	token, err := resolveToken(f)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", f.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", f.addr, err)
	}
	if token == "" && !isLoopback(ln.Addr()) {
		_ = ln.Close()
		return fmt.Errorf("--no-auth is only allowed on a loopback address, not %s", ln.Addr())
	}

	// rtCtx outlives the HTTP server: MCP servers are bound to it and must
	// stay up until the last turn has been cancelled and persisted.
	rtCtx, stopRuntime := context.WithCancel(context.WithoutCancel(ctx))
	defer stopRuntime()
	rt, err := newBotRuntime(rtCtx, botOptions{
		WorkDir:         f.workDir,
		Model:           f.model,
		MaxToolCalls:    f.maxToolCalls,
		Ephemeral:       f.ephemeral,
		DisableSchedule: f.noSchedule,
	})
	if err != nil {
		_ = ln.Close()
		return err
	}
	defer rt.Close()

	mgr, err := bot.NewManager(bot.Options{
		Runtime:     rt.Runtime,
		Store:       rt.Store,
		Secrets:     rt.Secrets,
		BotDataDir:  rt.BotDataDir,
		WorkDir:     rt.WorkDir,
		IdleTimeout: f.idleTimeout,
		TurnTimeout: f.turnTimeout,
		Hook:        rt.Hook,
	})
	if err != nil {
		_ = ln.Close()
		return err
	}

	// The scheduler starts only after the Manager exists: Fire needs a
	// Manager to Send a turn into a session.
	var sched *schedule.Scheduler
	var disp *bot.ScheduleDispatcher
	if rt.Schedule != nil {
		disp = bot.AttachSchedule(mgr, rt.Schedule)
		var lease schedule.LeaseStore
		if l, ok := rt.ScheduleStore.(schedule.LeaseStore); ok {
			lease = l
		}
		sched = schedule.NewScheduler(rt.Schedule, disp, schedule.SchedulerOptions{Lease: lease})
		if err := sched.Start(rtCtx); err != nil {
			mctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = mgr.Close(mctx)
			cancel()
			_ = ln.Close()
			return fmt.Errorf("start scheduler: %w", err)
		}
	}

	var tokStore bot.TokenStore
	if rt.DBConn != nil {
		if ts, err := bot.NewSQLiteTokenStore(rt.DBConn); err == nil {
			tokStore = ts
		} else {
			slog.Warn("init scoped token store failed", "err", err)
		}
	} else {
		tokStore = bot.NewMemoryTokenStore()
	}

	var auditLog *bot.AuditLogger
	if !f.ephemeral {
		if al, err := bot.NewAuditLogger(filepath.Join(botHome(), "audit.jsonl"), 0); err == nil {
			auditLog = al
		} else {
			slog.Warn("init audit logger failed", "err", err)
		}
	}
	mgr.SetAudit(auditLog)

	gwCtx, cancelGW := context.WithCancel(rtCtx)
	defer cancelGW()
	gw, err := startChannelGateway(gwCtx, mgr, rt.Schedule, rt.DBConn, auditLog)
	if err != nil {
		slog.Warn("start channel gateway failed", "err", err)
	}

	handlerCtx, cancelHandlers := context.WithCancel(context.Background())
	defer cancelHandlers()
	srv := &http.Server{
		Handler: bot.NewServer(bot.ServerOptions{
			Manager:        mgr,
			Schedules:      rt.Schedule,
			Token:          token,
			Tokens:         tokStore,
			Audit:          auditLog,
			AllowedOrigins: f.corsOrigins,
			DisableWebUI:   f.noWeb,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return handlerCtx },
	}

	addr := ln.Addr().String()
	if err := writeState(daemonState{PID: os.Getpid(), Addr: addr, StartedAt: time.Now().UTC(), Version: buildVersion(), WorkDir: rt.WorkDir}); err != nil {
		slog.Warn("write state file", "err", err)
	}
	defer removeState(rt.WorkDir, os.Getpid())

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	var metricsSrv *http.Server
	if mAddr := strings.TrimSpace(f.metricsAddr); mAddr != "" {
		mLn, mErr := net.Listen("tcp", mAddr)
		if mErr != nil {
			slog.Warn("listen metrics-addr failed", "addr", mAddr, "err", mErr)
		} else {
			mMux := http.NewServeMux()
			mMux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
				mgr.Metrics().WritePrometheus(w, mgr)
			})
			metricsSrv = &http.Server{Handler: mMux, ReadHeaderTimeout: 5 * time.Second}
			slog.Info("deepai-bot metrics listening", "metrics_addr", mLn.Addr().String())
			go func() {
				if err := metricsSrv.Serve(mLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
					slog.Warn("metrics server stopped", "err", err)
				}
			}()
		}
	}

	slog.Info("deepai-bot listening", "addr", addr, "workdir", rt.WorkDir, "db", rt.DB,
		"auth", token != "", "web", !f.noWeb, "schedule", rt.Schedule != nil)
	if !f.noWeb {
		url := "http://" + displayAddr(addr) + "/"
		if token != "" {
			url += "#token=" + token
		}
		fmt.Fprintf(os.Stderr, "\n  deepai-bot is running. Web console: %s\n\n", url)
	}

	var fatalServeErr error
	select {
	case <-ctx.Done():
		slog.Info("deepai-bot shutting down")
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server stopped", "err", err)
			fatalServeErr = fmt.Errorf("http server stopped: %w", err)
		}
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		sctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		if metricsSrv != nil {
			_ = metricsSrv.Shutdown(sctx)
		}
	}()
	// Stop accepting new fires & IM messages before cancelling turns, then let
	// turn_end callbacks write their final Run states before db.Close.
	cancelGW()
	if gw != nil {
		gw.Wait()
	}
	if sched != nil {
		sctx, stopSched := context.WithTimeout(context.Background(), 5*time.Second)
		if err := sched.Stop(sctx); err != nil {
			slog.Warn("scheduler shutdown", "err", err)
		}
		stopSched()
	}
	// Cancel running turns and let them persist before the streams that
	// report them are torn down, so connected clients receive turn_end.
	mctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := mgr.Close(mctx); err != nil {
		slog.Warn("turns still running at shutdown", "err", err)
	}
	cancel()
	if disp != nil {
		wctx, cancelWait := context.WithTimeout(context.Background(), 10*time.Second)
		if err := disp.Wait(wctx); err != nil {
			slog.Warn("schedule run callbacks still pending at shutdown", "err", err)
		}
		cancelWait()
	}
	cancelHandlers()
	<-shutdownDone
	return fatalServeErr
}

// resolveToken picks the API token: --token, $DEEPAI_BOT_TOKEN, or the
// persisted ~/.deepai/bot/token (created with a random value on first use).
func resolveToken(f serveFlags) (string, error) {
	if f.noAuth {
		return "", nil
	}
	if t := strings.TrimSpace(f.token); t != "" {
		return t, nil
	}
	if t := strings.TrimSpace(os.Getenv("DEEPAI_BOT_TOKEN")); t != "" {
		return t, nil
	}
	return loadOrCreateToken(false)
}

func tokenFile() string { return filepath.Join(botHome(), "token") }

func loadOrCreateToken(rotate bool) (string, error) {
	if !rotate {
		if data, err := os.ReadFile(tokenFile()); err == nil {
			if t := strings.TrimSpace(string(data)); t != "" {
				return t, nil
			}
		}
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	t := hex.EncodeToString(buf)
	if err := os.MkdirAll(botHome(), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(tokenFile(), []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	return t, nil
}

func isLoopback(a net.Addr) bool {
	tcp, ok := a.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}

// displayAddr turns a wildcard listen address into one a browser can open.
func displayAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
