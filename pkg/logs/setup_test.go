package logs

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetup_NoDebugFile(t *testing.T) {
	cleanup, err := Setup(Config{Level: 0})
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}
	cleanup()
}

func TestSetup_WithDebugFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "debug.log")

	cleanup, err := Setup(Config{Level: 0, DebugFile: path})
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	slog.Debug("debug-msg", "key", "val")
	slog.Warn("warn-msg")
	slog.Error("error-msg")

	cleanup()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read debug file: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "debug-msg") {
		t.Errorf("file should contain debug-msg, got: %s", content)
	}
	if !strings.Contains(content, "warn-msg") {
		t.Errorf("file should contain warn-msg, got: %s", content)
	}
	if !strings.Contains(content, "error-msg") {
		t.Errorf("file should contain error-msg, got: %s", content)
	}
}

func TestSetup_CleanupIdempotent(t *testing.T) {
	cleanup, err := Setup(Config{Level: 0})
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}
	cleanup()
	cleanup() // should not panic
}

func TestSetup_MultipleCalls(t *testing.T) {
	cleanup1, err := Setup(Config{Level: 0})
	if err != nil {
		t.Fatalf("first Setup failed: %v", err)
	}

	cleanup2, err := Setup(Config{Level: 0})
	if err != nil {
		cleanup1()
		t.Fatalf("second Setup failed: %v", err)
	}

	cleanup2()
	cleanup1() // first cleanup should be safe after second Setup
}

func TestFromEnv(t *testing.T) {
	t.Setenv("DEEPAI_DEBUG_FILE", "/tmp/test-debug.log")
	cfg := FromEnv()
	if cfg.DebugFile != "/tmp/test-debug.log" {
		t.Errorf("expected /tmp/test-debug.log, got %s", cfg.DebugFile)
	}
}

func TestFromEnv_Fallback(t *testing.T) {
	t.Setenv("DEEPAI_DEBUG", "1")
	cfg := FromEnv()
	if !strings.HasSuffix(cfg.DebugFile, "/deepai-debug.log") {
		t.Errorf("expected fallback path, got %s", cfg.DebugFile)
	}
}

func TestSetup_WithErrorFile(t *testing.T) {
	dir := t.TempDir()
	debugPath := filepath.Join(dir, "debug.log")
	errorPath := filepath.Join(dir, "error.log")

	cleanup, err := Setup(Config{Level: 0, DebugFile: debugPath, ErrorFile: errorPath})
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	slog.Debug("debug-msg")
	slog.Info("info-msg")
	slog.Warn("warn-msg")
	slog.Error("error-msg")

	cleanup()

	debugData, _ := os.ReadFile(debugPath)
	debugContent := string(debugData)
	if !strings.Contains(debugContent, "debug-msg") {
		t.Errorf("debug file should contain debug-msg, got: %s", debugContent)
	}
	if !strings.Contains(debugContent, "warn-msg") {
		t.Errorf("debug file should contain warn-msg, got: %s", debugContent)
	}

	errorData, _ := os.ReadFile(errorPath)
	errorContent := string(errorData)
	if !strings.Contains(errorContent, "warn-msg") {
		t.Errorf("error file should contain warn-msg, got: %s", errorContent)
	}
	if !strings.Contains(errorContent, "error-msg") {
		t.Errorf("error file should contain error-msg, got: %s", errorContent)
	}
	if strings.Contains(errorContent, "debug-msg") {
		t.Errorf("error file should NOT contain debug-msg, got: %s", errorContent)
	}
	if strings.Contains(errorContent, "info-msg") {
		t.Errorf("error file should NOT contain info-msg, got: %s", errorContent)
	}
}

func TestSetup_ErrorFileOnly(t *testing.T) {
	dir := t.TempDir()
	errorPath := filepath.Join(dir, "error.log")

	cleanup, err := Setup(Config{Level: 0, ErrorFile: errorPath})
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	slog.Debug("debug-msg")
	slog.Info("info-msg")
	slog.Warn("warn-msg")
	slog.Error("error-msg")

	cleanup()

	errorData, _ := os.ReadFile(errorPath)
	errorContent := string(errorData)
	if !strings.Contains(errorContent, "warn-msg") {
		t.Errorf("error file should contain warn-msg, got: %s", errorContent)
	}
	if !strings.Contains(errorContent, "error-msg") {
		t.Errorf("error file should contain error-msg, got: %s", errorContent)
	}
	if strings.Contains(errorContent, "debug-msg") {
		t.Errorf("error file should NOT contain debug-msg, got: %s", errorContent)
	}
	if strings.Contains(errorContent, "info-msg") {
		t.Errorf("error file should NOT contain info-msg, got: %s", errorContent)
	}
}

func TestFromEnv_Empty(t *testing.T) {
	cfg := FromEnv()
	if cfg.DebugFile != "" {
		t.Errorf("expected empty DebugFile, got %s", cfg.DebugFile)
	}
	if cfg.Level != slog.LevelDebug {
		t.Errorf("expected LevelDebug, got %v", cfg.Level)
	}
}
