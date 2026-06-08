package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSandboxCreate(t *testing.T) {
	baseDir := t.TempDir()
	sb, err := New("create", baseDir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer sb.Close()

	info, err := os.Stat(sb.GetDir())
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("expected %s to be a directory", sb.GetDir())
	}
}

// TestSandboxClosePreservesExistingDir is the regression guard for the ctrl+c
// data-loss bug: when the session directory already exists (e.g. a project's
// own ./cli folder), Close must NOT delete it or its contents.
func TestSandboxClosePreservesExistingDir(t *testing.T) {
	baseDir := t.TempDir()
	sessionDir := filepath.Join(baseDir, "cli")
	if err := os.MkdirAll(filepath.Join(sessionDir, "internal"), 0o755); err != nil {
		t.Fatalf("setup MkdirAll() error = %v", err)
	}
	userFile := filepath.Join(sessionDir, "main.go")
	if err := os.WriteFile(userFile, []byte("user code"), 0o644); err != nil {
		t.Fatalf("setup WriteFile() error = %v", err)
	}

	sb, err := New("cli", baseDir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := sb.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if _, err := os.Stat(userFile); err != nil {
		t.Fatalf("pre-existing user file was deleted on Close: %v", err)
	}
}

// TestNewSessionClosesOwnDir verifies NewSession creates a unique, owned
// directory under the base and removes only that directory on Close, leaving
// the base (and any sibling content) intact.
func TestNewSessionClosesOwnDir(t *testing.T) {
	baseDir := t.TempDir()
	sibling := filepath.Join(baseDir, "keep.txt")
	if err := os.WriteFile(sibling, []byte("keep"), 0o644); err != nil {
		t.Fatalf("setup WriteFile() error = %v", err)
	}

	sb, err := NewSession(baseDir, Config{})
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	sessionDir := sb.GetDir()
	if filepath.Dir(sessionDir) != filepath.Clean(baseDir) {
		t.Fatalf("session dir %q is not under base %q", sessionDir, baseDir)
	}
	if _, err := os.Stat(sessionDir); err != nil {
		t.Fatalf("session dir not created: %v", err)
	}

	if err := sb.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Fatalf("owned session dir not removed on Close: err = %v", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("sibling content under base was deleted: %v", err)
	}
}

func TestSandboxExec(t *testing.T) {
	baseDir := t.TempDir()
	sb, err := New("exec", baseDir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer sb.Close()

	result, err := sb.Exec(context.Background(), "echo hello", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
	if got := strings.TrimSpace(result.Stdout()); got != "hello" {
		t.Fatalf("stdout = %q, want hello", got)
	}
	if result.ExitCode() != 0 {
		t.Fatalf("exit code = %d, want 0", result.ExitCode())
	}
}

func TestSandboxWriteRead(t *testing.T) {
	baseDir := t.TempDir()
	sb, err := New("files", baseDir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer sb.Close()

	path := filepath.Join("nested", "message.txt")
	want := []byte("sandbox data")
	if err := sb.WriteFile(path, want); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, err := sb.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("ReadFile() = %q, want %q", string(got), string(want))
	}
}

func TestSandboxTimeout(t *testing.T) {
	baseDir := t.TempDir()
	sb, err := New("timeout", baseDir)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer sb.Close()

	result, err := sb.Exec(context.Background(), "sleep 2", 200*time.Millisecond)
	if err == nil {
		t.Fatal("Exec() error = nil, want timeout")
	}
	if result == nil {
		t.Fatal("Exec() result = nil")
	}
	if result.Error() == nil {
		t.Fatal("result.Error() = nil, want timeout")
	}
	var timeoutErr *TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("Exec() error = %T, want *TimeoutError", err)
	}
}

func TestExecDirectTimeout(t *testing.T) {
	result, err := ExecDirect(context.Background(), "sleep 1", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("ExecDirect() error = %v", err)
	}
	if result == nil {
		t.Fatal("ExecDirect() result = nil")
	}
	if result.ExitCode() == 0 {
		t.Fatal("ExecDirect() exit code = 0, want non-zero timeout exit")
	}
}
