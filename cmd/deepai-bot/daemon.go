package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

// daemonState is written by a running server to ~/.deepai/bot/bot.json so
// stop/status (and GUI launchers) can find it without knowing its flags.
type daemonState struct {
	PID       int       `json:"pid"`
	Addr      string    `json:"addr"`
	StartedAt time.Time `json:"started_at"`
	Version   string    `json:"version,omitempty"`
	WorkDir   string    `json:"work_dir,omitempty"`
}

func stateFile() string { return filepath.Join(botHome(), "bot.json") }
func logFile() string   { return filepath.Join(botHome(), "bot.log") }

func writeState(st daemonState) error {
	if err := os.MkdirAll(botHome(), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(st, "", "  ")
	tmp := stateFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, stateFile())
}

// removeState deletes the state file only if it still describes pid, so a
// server that lost a start race never deletes the winner's file.
func removeState(pid int) {
	if st, err := readState(); err == nil && st.PID == pid {
		_ = os.Remove(stateFile())
	}
}

func readState() (daemonState, error) {
	var st daemonState
	data, err := os.ReadFile(stateFile())
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(data, &st)
}

// runningState returns the state of a live server, or an error if none.
func runningState() (daemonState, error) {
	st, err := readState()
	if err != nil {
		return st, errors.New("deepai-bot is not running")
	}
	if !processAlive(st.PID) {
		_ = os.Remove(stateFile())
		return st, errors.New("deepai-bot is not running (removed stale state file)")
	}
	return st, nil
}

func runStart(ctx context.Context, f serveFlags) error {
	if st, err := runningState(); err == nil {
		return fmt.Errorf("deepai-bot is already running (pid %d, %s)", st.PID, st.Addr)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(botHome(), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(logFile(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()

	// Everything after "start" is a serve flag; hand it over verbatim.
	args := []string{"serve"}
	for i, a := range os.Args {
		if a == "start" {
			args = append(args, os.Args[i+1:]...)
			break
		}
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Stdin = nil
	if f.workDir == "" {
		cmd.Dir, _ = os.Getwd()
	}
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start deepai-bot: %w", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		if !processAlive(pid) {
			return fmt.Errorf("deepai-bot exited during startup; see %s", logFile())
		}
		st, err := readState()
		if err != nil || st.PID != pid {
			continue
		}
		if healthy(st.Addr) {
			fmt.Printf("deepai-bot started (pid %d) on http://%s\n  log: %s\n", pid, displayAddr(st.Addr), logFile())
			return nil
		}
	}
	return fmt.Errorf("deepai-bot (pid %d) did not become healthy in time; see %s", pid, logFile())
}

func newStopCmd() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the background bot (running turns are cancelled and saved)",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := runningState()
			if err != nil {
				return err
			}
			if err := terminate(st.PID); err != nil {
				return fmt.Errorf("signal pid %d: %w", st.PID, err)
			}
			deadline := time.Now().Add(timeout)
			for time.Now().Before(deadline) {
				if !processAlive(st.PID) {
					fmt.Printf("deepai-bot stopped (pid %d)\n", st.PID)
					return nil
				}
				time.Sleep(200 * time.Millisecond)
			}
			return fmt.Errorf("deepai-bot (pid %d) still running after %s", st.PID, timeout)
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 60*time.Second, "How long to wait for a graceful exit")
	return cmd
}

func newStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether the background bot is running",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := runningState()
			if err != nil {
				if asJSON {
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
	return cmd
}

func newTokenCmd() *cobra.Command {
	var rotate bool
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Print (or --rotate) the API token stored in ~/.deepai/bot/token",
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
	cmd.Flags().BoolVar(&rotate, "rotate", false, "Generate a new token")
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
