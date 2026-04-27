// Package sandbox provides isolated command execution and workspace-bound file access.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	helperEnvEnabled    = "DEEPAI_SANDBOX_HELPER"
	helperEnvBackend    = "DEEPAI_SANDBOX_BACKEND"
	helperEnvDir        = "DEEPAI_SANDBOX_DIR"
	helperEnvCmd        = "DEEPAI_SANDBOX_CMD"
	defaultTimeout      = 5 * time.Minute
	defaultCleanupDelay = 250 * time.Millisecond
)

type backend string

const (
	backendDirect   backend = "direct"
	backendBwrap    backend = "bwrap"
	backendLandlock backend = "landlock"
)

type Config struct {
	Timeout      time.Duration
	MaxInstances int
	CleanupDelay time.Duration
}

type TimeoutError struct {
	Duration time.Duration
	Message  string
}

func (e *TimeoutError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Message) != "" {
		return fmt.Sprintf("%s after %s", e.Message, e.Duration)
	}
	return fmt.Sprintf("sandbox timed out after %s", e.Duration)
}

// Sandbox isolates commands and files inside a per-session directory.
type Sandbox struct {
	sessionDir string
	processes  []*os.Process

	mu      sync.Mutex
	backend backend
	cfg     Config
}

func init() {
	if os.Getenv(helperEnvEnabled) != "1" {
		return
	}
	os.Exit(runHelper())
}

// New creates a session directory below baseDir and selects the best available backend.
func New(sessionID string, baseDir string) (*Sandbox, error) {
	return NewWithConfig(sessionID, baseDir, Config{})
}

// NewWithConfig creates a session directory below baseDir and applies sandbox settings.
func NewWithConfig(sessionID string, baseDir string, cfg Config) (*Sandbox, error) {
	sessionID = strings.TrimSpace(sessionID)
	baseDir = strings.TrimSpace(baseDir)
	if sessionID == "" {
		return nil, errors.New("sessionID is required")
	}
	if baseDir == "" {
		return nil, errors.New("baseDir is required")
	}

	sessionDir := filepath.Join(baseDir, sessionID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}

	sb := &Sandbox{
		sessionDir: sessionDir,
		backend:    backendDirect,
		cfg:        normalizeConfig(cfg),
	}

	if CheckLandlockAvailable() {
		slog.Info("landlock is available, using landlock sandbox backend", "session", sessionID)
		if err := probeLandlock(sessionDir); err == nil {
			sb.backend = backendLandlock
			return sb, nil
		}
	}

	if probeBubblewrap(sessionDir) == nil {
		sb.backend = backendBwrap
	}

	return sb, nil
}

// Exec executes a shell command inside the sandbox backend.
func (s *Sandbox) Exec(ctx context.Context, cmd string, timeout time.Duration) (*Result, error) {
	if s == nil {
		return nil, errors.New("sandbox is nil")
	}
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return nil, errors.New("cmd is required")
	}
	if timeout <= 0 {
		timeout = s.cfg.Timeout
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()
	exePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve helper executable: %w", err)
	}

	command := exec.CommandContext(runCtx, exePath)
	command.Dir = s.sessionDir
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Env = append(os.Environ(),
		helperEnvEnabled+"=1",
		helperEnvBackend+"="+string(s.backend),
		helperEnvDir+"="+s.sessionDir,
		helperEnvCmd+"="+cmd,
	)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start sandbox command: %w", err)
	}

	s.trackProcess(command.Process)
	defer s.untrackProcess(command.Process)

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- command.Wait()
	}()

	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-runCtx.Done():
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			slog.Warn("sandbox timeout", "session", filepath.Base(s.sessionDir), "timeout", timeout, "cmd", cmd)
		}
		s.forceKill(command.Process)
		waitErr = s.waitAfterKill(waitDone)
	}

	result := &Result{
		stdout:   stdout.String(),
		stderr:   stderr.String(),
		exitCode: exitCode(command.ProcessState, waitErr),
		duration: time.Since(started),
	}

	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		timeoutErr := &TimeoutError{
			Duration: timeout,
			Message:  "sandbox execution timed out",
		}
		result.err = timeoutErr
		return result, timeoutErr
	}

	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		result.err = waitErr
		return result, waitErr
	}

	if waitErr != nil {
		result.err = waitErr
	}

	return result, nil
}

// WriteFile writes data under the sandbox session directory.
func (s *Sandbox) WriteFile(path string, data []byte) error {
	resolved, err := s.resolvePath(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	if err := os.WriteFile(resolved, data, 0o644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	return nil
}

// ReadFile reads data from the sandbox session directory.
func (s *Sandbox) ReadFile(path string) ([]byte, error) {
	resolved, err := s.resolvePath(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	return data, nil
}

// Close terminates tracked processes and removes the session directory.
func (s *Sandbox) Close() error {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	processes := append([]*os.Process(nil), s.processes...)
	s.processes = nil
	s.mu.Unlock()

	for _, proc := range processes {
		if proc == nil {
			continue
		}
		s.forceKill(proc)
	}

	if delay := s.cfg.CleanupDelay; delay > 0 {
		time.Sleep(delay)
	}

	if err := os.RemoveAll(s.sessionDir); err != nil {
		return fmt.Errorf("remove session directory: %w", err)
	}
	return nil
}

// GetDir returns the sandbox session directory.
func (s *Sandbox) GetDir() string {
	if s == nil {
		return ""
	}
	return s.sessionDir
}

func (s *Sandbox) resolvePath(path string) (string, error) {
	if s == nil {
		return "", errors.New("sandbox is nil")
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("path is required")
	}

	if filepath.IsAbs(path) {
		path = strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator))
	}
	resolved := filepath.Join(s.sessionDir, path)
	resolved = filepath.Clean(resolved)

	relative, err := filepath.Rel(s.sessionDir, resolved)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes sandbox: %s", path)
	}
	return resolved, nil
}

func (s *Sandbox) trackProcess(proc *os.Process) {
	if proc == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.processes = append(s.processes, proc)
}

func (s *Sandbox) forceKill(proc *os.Process) {
	forceKillProcess(proc)
}

func (s *Sandbox) waitAfterKill(waitDone <-chan error) error {
	if s == nil || s.cfg.CleanupDelay <= 0 {
		return <-waitDone
	}

	select {
	case err := <-waitDone:
		return err
	case <-time.After(s.cfg.CleanupDelay):
		return context.DeadlineExceeded
	}
}

func (s *Sandbox) untrackProcess(proc *os.Process) {
	if proc == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, candidate := range s.processes {
		if candidate != nil && candidate.Pid == proc.Pid {
			s.processes = append(s.processes[:i], s.processes[i+1:]...)
			return
		}
	}
}

func exitCode(state *os.ProcessState, waitErr error) int {
	if state != nil {
		return state.ExitCode()
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func forceKillProcess(proc *os.Process) {
	if proc == nil {
		return
	}
	if proc.Pid > 0 {
		_ = syscall.Kill(-proc.Pid, syscall.SIGKILL)
	}
	_ = proc.Kill()
}

func normalizeConfig(cfg Config) Config {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.CleanupDelay <= 0 {
		cfg.CleanupDelay = defaultCleanupDelay
	}
	return cfg
}

func runHelper() int {
	dir := os.Getenv(helperEnvDir)
	cmd := os.Getenv(helperEnvCmd)
	selectedBackend := backend(os.Getenv(helperEnvBackend))

	if strings.TrimSpace(dir) == "" || strings.TrimSpace(cmd) == "" {
		_, _ = io.WriteString(os.Stderr, "sandbox helper missing configuration\n")
		return 2
	}

	if err := os.Chdir(dir); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "sandbox helper chdir: %v\n", err)
		return 2
	}

	env := helperEnv(os.Environ(), dir)

	switch selectedBackend {
	case backendLandlock:
		if err := applyLandlock(dir); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "landlock setup failed: %v\n", err)
			if hasBubblewrap() {
				return execBubblewrap(dir, cmd, env)
			}
			return execShell(cmd, env)
		}
		return execShell(cmd, env)
	case backendBwrap:
		return execBubblewrap(dir, cmd, env)
	default:
		return execShell(cmd, env)
	}
}

func execShell(command string, env []string) int {
	return execProgram("/bin/sh", []string{"/bin/sh", "-lc", command}, env)
}

func execBubblewrap(dir string, command string, env []string) int {
	args, err := bubblewrapArgs(dir, command)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "bubblewrap args: %v\n", err)
		return execShell(command, env)
	}
	return execProgram(bwrapPath, args, env)
}

func execProgram(path string, args []string, env []string) int {
	if err := syscall.Exec(path, args, env); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "exec %s: %v\n", path, err)
		return 127
	}
	return 0
}

func helperEnv(base []string, dir string) []string {
	filtered := make([]string, 0, len(base)+2)
	for _, entry := range base {
		if strings.HasPrefix(entry, helperEnvEnabled+"=") ||
			strings.HasPrefix(entry, helperEnvBackend+"=") ||
			strings.HasPrefix(entry, helperEnvDir+"=") ||
			strings.HasPrefix(entry, helperEnvCmd+"=") {
			continue
		}
		filtered = append(filtered, entry)
	}
	filtered = append(filtered, "HOME="+dir, "PWD="+dir)
	return filtered
}

// ExecDirect runs a command without sandbox restrictions (fallback).
func ExecDirect(ctx context.Context, cmd string, timeout time.Duration) (*Result, error) {
	start := time.Now()
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	execCmd := exec.CommandContext(ctx, "sh", "-c", cmd)
	execCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	execCmd.Stdout = &stdoutBuf
	execCmd.Stderr = &stderrBuf

	if err := execCmd.Start(); err != nil {
		return NewResult("", err.Error(), -1, time.Since(start), err), nil
	}

	err := execCmd.Wait()
	duration := time.Since(start)

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	return NewResult(stdoutBuf.String(), stderrBuf.String(), exitCode, duration, nil), nil
}
