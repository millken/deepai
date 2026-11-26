package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/millken/deepai/pkg/bot"
	"github.com/spf13/cobra"
)

// startupTimeout bounds how long `deepai-bot start` waits for the detached
// child to write its state file and pass /healthz. Overridable in tests.
var startupTimeout = 20 * time.Second

// daemonState is written by a running server to ~/.deepai/bot/instances/<hash>.json
// (and mirrored to ~/.deepai/bot/bot.json when no other live daemon owns it) so
// stop/status and GUI launchers can discover running instances safely across workdirs.
type daemonState struct {
	PID       int       `json:"pid"`
	Addr      string    `json:"addr"`
	StartedAt time.Time `json:"started_at"`
	Version   string    `json:"version,omitempty"`
	WorkDir   string    `json:"work_dir,omitempty"`
}

func stateFile() string    { return filepath.Join(botHome(), "bot.json") }
func instancesDir() string { return filepath.Join(botHome(), "instances") }
func logFile() string      { return filepath.Join(botHome(), "bot.log") }

func workdirStateFile(workDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(workDir)))
	return filepath.Join(instancesDir(), hex.EncodeToString(sum[:8])+".json")
}

func resolveCanonicalWorkDir(raw string) (string, error) {
	wd := strings.TrimSpace(raw)
	var err error
	if wd == "" {
		if wd, err = os.Getwd(); err != nil {
			return "", err
		}
	}
	if wd, err = filepath.Abs(wd); err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(wd); err == nil {
		wd = resolved
	}
	return filepath.Clean(wd), nil
}

func writeJSONAtomic(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeState(st daemonState) error {
	if st.WorkDir != "" {
		wf := workdirStateFile(st.WorkDir)
		if existing, err := readStateFrom(wf); err != nil || existing.PID == st.PID || !processAlive(existing.PID) {
			if err := writeJSONAtomic(wf, st); err != nil {
				return err
			}
		}
	}
	// Preserve ~/.deepai/bot/bot.json if another live daemon already owns it,
	// so a second foreground `serve` on another workdir never clobbers it.
	if existing, err := readStateFrom(stateFile()); err == nil && existing.PID != st.PID && processAlive(existing.PID) {
		return nil
	}
	return writeJSONAtomic(stateFile(), st)
}

// removeState deletes the per-workdir state file and updates ~/.deepai/bot/bot.json
// only if it still describes pid, promoting another live instance when available.
func removeState(workDir string, pid int) {
	if workDir != "" {
		wf := workdirStateFile(workDir)
		if st, err := readStateFrom(wf); err == nil && st.PID == pid {
			_ = os.Remove(wf)
		}
	}
	if st, err := readStateFrom(stateFile()); err == nil && st.PID == pid {
		_ = os.Remove(stateFile())
		if live := liveInstances(); len(live) > 0 {
			_ = writeJSONAtomic(stateFile(), live[0])
		}
	}
}

func readStateFrom(path string) (daemonState, error) {
	var st daemonState
	data, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(data, &st)
}

func readState() (daemonState, error) {
	return readStateFrom(stateFile())
}

func readStateForPID(workDir string, pid int) (daemonState, error) {
	if workDir != "" {
		if st, err := readStateFrom(workdirStateFile(workDir)); err == nil && st.PID == pid {
			return st, nil
		}
	}
	if st, err := readStateFrom(stateFile()); err == nil && st.PID == pid {
		return st, nil
	}
	return daemonState{}, errors.New("state not ready")
}

// liveInstances returns all running deepai-bot instances recorded on disk,
// pruning any stale state files whose PIDs are no longer alive.
func liveInstances() []daemonState {
	byPID := map[int]daemonState{}
	entries, _ := os.ReadDir(instancesDir())
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		p := filepath.Join(instancesDir(), ent.Name())
		st, err := readStateFrom(p)
		if err != nil {
			continue
		}
		if !processAlive(st.PID) {
			_ = os.Remove(p)
			continue
		}
		byPID[st.PID] = st
	}
	if st, err := readStateFrom(stateFile()); err == nil {
		if !processAlive(st.PID) {
			_ = os.Remove(stateFile())
		} else if _, seen := byPID[st.PID]; !seen {
			byPID[st.PID] = st
		}
	}
	out := make([]daemonState, 0, len(byPID))
	for _, st := range byPID {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}
		return out[i].PID < out[j].PID
	})
	return out
}

func runningStateInExactWorkDir(canonicalWorkDir string) (daemonState, error) {
	if canonicalWorkDir != "" {
		wf := workdirStateFile(canonicalWorkDir)
		if st, err := readStateFrom(wf); err == nil {
			if processAlive(st.PID) {
				return st, nil
			}
			_ = os.Remove(wf)
		}
	}
	for _, st := range liveInstances() {
		if canonicalWorkDir != "" && filepath.Clean(st.WorkDir) == canonicalWorkDir {
			return st, nil
		}
	}
	return daemonState{}, errors.New("deepai-bot is not running")
}

// runningStateFor resolves the target running daemon:
//   - when workDirFlag is non-empty, requires a live instance for that workdir;
//   - otherwise, returns the sole live instance if only one is running, or the
//     instance matching cwd when multiple workdirs are active.
func runningStateFor(workDirFlag string) (daemonState, error) {
	if strings.TrimSpace(workDirFlag) != "" {
		target, err := resolveCanonicalWorkDir(workDirFlag)
		if err != nil {
			return daemonState{}, err
		}
		st, err := runningStateInExactWorkDir(target)
		if err != nil {
			return st, fmt.Errorf("deepai-bot is not running in %s", target)
		}
		return st, nil
	}
	live := liveInstances()
	if len(live) == 0 {
		return daemonState{}, errors.New("deepai-bot is not running")
	}
	if len(live) == 1 {
		return live[0], nil
	}
	if cwd, err := resolveCanonicalWorkDir(""); err == nil {
		for _, st := range live {
			if filepath.Clean(st.WorkDir) == cwd {
				return st, nil
			}
		}
	}
	parts := make([]string, 0, len(live))
	for _, st := range live {
		parts = append(parts, fmt.Sprintf("pid %d (%s, %s)", st.PID, st.Addr, st.WorkDir))
	}
	return daemonState{}, fmt.Errorf("multiple deepai-bot instances running (%s); specify -w <workdir>", strings.Join(parts, "; "))
}

// runningState returns the state of a live server when unambiguous.
func runningState() (daemonState, error) {
	return runningStateFor("")
}

func runStart(ctx context.Context, f serveFlags) error {
	targetWorkDir, err := resolveCanonicalWorkDir(f.workDir)
	if err != nil {
		return err
	}
	if st, err := runningStateInExactWorkDir(targetWorkDir); err == nil {
		return fmt.Errorf("deepai-bot is already running in %s (pid %d, %s)", targetWorkDir, st.PID, st.Addr)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return startDetachedProcess(ctx, exe, buildStartServeArgs(os.Args), f.workDir, targetWorkDir)
}

func buildStartServeArgs(osArgs []string) []string {
	args := []string{"serve"}
	for i := 1; i < len(osArgs); i++ {
		if osArgs[i] == "start" {
			args = append(args, osArgs[i+1:]...)
			break
		}
	}
	return args
}

func startDetachedProcess(ctx context.Context, exe string, args []string, rawWorkDir, targetWorkDir string) error {
	if err := os.MkdirAll(botHome(), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(logFile(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()

	cmd := exec.Command(exe, args...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Stdin = nil
	if rawWorkDir == "" {
		cmd.Dir, _ = os.Getwd()
	}
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start deepai-bot: %w", err)
	}
	pid := cmd.Process.Pid

	// Reap the child in a background goroutine while polling for readiness:
	// calling Process.Release() without Wait() leaves an exited child as a
	// zombie until this parent exits, causing kill(pid, 0) to report alive
	// for the entire startup deadline.
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	stopChild := func() {
		_ = terminate(pid)
		select {
		case <-waitCh:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			select {
			case <-waitCh:
			case <-time.After(500 * time.Millisecond):
			}
		}
	}

	deadline := time.Now().Add(startupTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			stopChild()
			return ctx.Err()
		case waitErr := <-waitCh:
			if waitErr != nil {
				return fmt.Errorf("deepai-bot exited during startup (%v); see %s", waitErr, logFile())
			}
			return fmt.Errorf("deepai-bot exited during startup; see %s", logFile())
		case <-time.After(100 * time.Millisecond):
		}
		if !processAlive(pid) {
			return fmt.Errorf("deepai-bot exited during startup; see %s", logFile())
		}
		st, err := readStateForPID(targetWorkDir, pid)
		if err != nil {
			continue
		}
		if healthy(st.Addr) {
			fmt.Printf("deepai-bot started (pid %d) on http://%s\n  log: %s\n", pid, displayAddr(st.Addr), logFile())
			return nil
		}
	}
	stopChild()
	return fmt.Errorf("deepai-bot (pid %d) did not become healthy in time; see %s", pid, logFile())
}

func newStopCmd() *cobra.Command {
	var (
		timeout time.Duration
		workDir string
	)
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the background bot (running turns are cancelled and saved)",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := runningStateFor(workDir)
			if err != nil {
				return err
			}
			if err := terminate(st.PID); err != nil {
				return fmt.Errorf("signal pid %d: %w", st.PID, err)
			}
			deadline := time.Now().Add(timeout)
			for time.Now().Before(deadline) {
				if !processAlive(st.PID) {
					removeState(st.WorkDir, st.PID)
					fmt.Printf("deepai-bot stopped (pid %d)\n", st.PID)
					return nil
				}
				time.Sleep(200 * time.Millisecond)
			}
			return fmt.Errorf("deepai-bot (pid %d) still running after %s", st.PID, timeout)
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 60*time.Second, "How long to wait for a graceful exit")
	cmd.Flags().StringVarP(&workDir, "workdir", "w", "", "Target bot working directory when multiple instances run")
	return cmd
}

func newStatusCmd() *cobra.Command {
	var (
		asJSON  bool
		workDir string
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether the background bot is running",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := runningStateFor(workDir)
			if err != nil {
				if asJSON {
					live := liveInstances()
					if strings.TrimSpace(workDir) == "" && len(live) > 1 {
						out, _ := json.Marshal(map[string]any{"running": true, "instances": live})
						fmt.Println(string(out))
						return nil
					}
					fmt.Println(`{"running":false}`)
					return nil
				}
				return err
			}
			ok := healthy(st.Addr)
			if asJSON {
				out, _ := json.Marshal(map[string]any{"running": true, "healthy": ok, "state": st})
				fmt.Println(string(out))
				return nil
			}
			fmt.Printf("deepai-bot running\n  pid:      %d\n  addr:     http://%s\n  healthy:  %v\n  started:  %s\n  workdir:  %s\n  version:  %s\n  log:      %s\n",
				st.PID, displayAddr(st.Addr), ok, st.StartedAt.Local().Format(time.RFC3339), st.WorkDir, st.Version, logFile())
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Machine-readable output")
	cmd.Flags().StringVarP(&workDir, "workdir", "w", "", "Target bot working directory when multiple instances run")
	return cmd
}

func newTokenCmd() *cobra.Command {
	var rotate bool
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Print (or --rotate) the master API token, or manage scoped tokens (create/list/revoke)",
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := loadOrCreateToken(rotate)
			if err != nil {
				return err
			}
			fmt.Println(t)
			if rotate {
				fmt.Fprintln(os.Stderr, "token rotated; restart deepai-bot for it to take effect")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&rotate, "rotate", false, "Generate a new master token")

	var (
		tokName string
		tokRole string
		tokBots []string
		tokTTL  string
	)
	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Issue a scoped API token (admin | user | readonly)",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, cleanup, err := openBotTokenStore()
			if err != nil {
				return err
			}
			defer cleanup()
			rec, err := store.CreateToken(cmd.Context(), bot.CreateTokenRequest{
				Name:   tokName,
				Role:   bot.TokenRole(tokRole),
				BotIDs: tokBots,
				TTL:    tokTTL,
			})
			if err != nil {
				return err
			}
			fmt.Printf("id:    %s\nname:  %s\nrole:  %s\nbots:  %s\ntoken: %s\n",
				rec.ID, rec.Name, rec.Role, strings.Join(rec.BotIDs, ","), rec.Token)
			fmt.Fprintln(os.Stderr, "(save this token now; it will not be shown again)")
			return nil
		},
	}
	createCmd.Flags().StringVar(&tokName, "name", "", "Human-readable token name (required)")
	createCmd.Flags().StringVar(&tokRole, "role", "user", "Token role: admin | user | readonly")
	createCmd.Flags().StringSliceVar(&tokBots, "bots", nil, "Restrict token to specific bot IDs (empty = all)")
	createCmd.Flags().StringVar(&tokTTL, "ttl", "", "Optional expiration duration (e.g. 720h)")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List issued scoped tokens",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, cleanup, err := openBotTokenStore()
			if err != nil {
				return err
			}
			defer cleanup()
			list, err := store.ListTokens(cmd.Context())
			if err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Println("no scoped tokens issued")
				return nil
			}
			for _, rec := range list {
				status := "active"
				if rec.RevokedAt != nil {
					status = "revoked"
				} else if rec.ExpiresAt != nil && time.Now().After(*rec.ExpiresAt) {
					status = "expired"
				}
				bots := "*"
				if len(rec.BotIDs) > 0 {
					bots = strings.Join(rec.BotIDs, ",")
				}
				fmt.Printf("%s\t%s\t%s\tbots=%s\t%s\n", rec.ID, rec.Name, rec.Role, bots, status)
			}
			return nil
		},
	}

	revokeCmd := &cobra.Command{
		Use:   "revoke <token_id>",
		Short: "Revoke an issued scoped token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, cleanup, err := openBotTokenStore()
			if err != nil {
				return err
			}
			defer cleanup()
			if err := store.RevokeToken(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Printf("revoked %s\n", args[0])
			return nil
		},
	}

	cmd.AddCommand(createCmd, listCmd, revokeCmd)
	return cmd
}

func healthy(addr string) bool {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + displayAddr(addr) + "/healthz")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
