package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func waitFor(t *testing.T, desc string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func readPid(t *testing.T, pidfile string) int {
	t.Helper()
	data, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse pid %q: %v", data, err)
	}
	return pid
}

// Review C2: Close() while a dial is still in flight must terminate the
// dialing subprocess promptly (the dial ctx binds its transport lifetime),
// not leave it orphaned until the 30s handshake timeout.
func TestLazyClient_CloseKillsInFlightDial(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "pid")
	slow := writeScript(t, dir, "slow.sh", "#!/bin/sh\necho $$ > "+pidfile+"\nexec sleep 60\n")

	lc := startLazyClient(context.Background(), "slow", ServerConfig{Command: slow}, nil)
	waitFor(t, "dial subprocess to start", 15*time.Second, func() bool {
		_, err := os.Stat(pidfile)
		return err == nil
	})
	pid := readPid(t, pidfile)

	lc.Close()
	waitFor(t, "in-flight dial subprocess to be killed by Close", 5*time.Second, func() bool {
		return !processAlive(pid)
	})
}

// Review C1: the redial budget is session-level (background dial + at most
// one redial). After exhaustion, get() must fail fast with lastErr instead
// of spawning another attempt per call.
func TestLazyClient_RedialBudgetIsSessionLevel(t *testing.T) {
	dir := t.TempDir()
	attempts := filepath.Join(dir, "attempts")
	fail := writeScript(t, dir, "fail.sh", "#!/bin/sh\necho x >> "+attempts+"\nexit 1\n")

	count := func() int {
		data, _ := os.ReadFile(attempts)
		return strings.Count(string(data), "x")
	}

	lc := startLazyClient(context.Background(), "dead", ServerConfig{Command: fail}, nil)
	defer lc.Close()
	waitFor(t, "background dial attempt to record", 15*time.Second, func() bool { return count() >= 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := lc.get(ctx); err == nil {
		t.Fatal("first get after failed background dial should redial and fail")
	}
	if _, err := lc.get(ctx); err == nil {
		t.Fatal("second get should fail fast with lastErr")
	}

	waitFor(t, "redial attempt to record", 15*time.Second, func() bool { return count() >= 2 })
	time.Sleep(300 * time.Millisecond) // grace window to catch a late third spawn
	if n := count(); n != 2 {
		t.Fatalf("session must dial at most twice (background + one redial), got %d", n)
	}
}
