package logs

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func newTestHandler(w *bytes.Buffer) slog.Handler {
	return slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})
}

func TestRouterHandler_LevelRouting(t *testing.T) {
	var debugBuf, defaultBuf bytes.Buffer
	router := NewRouterHandler(
		route{min: slog.LevelDebug - 4, max: slog.LevelDebug, handler: newTestHandler(&debugBuf)},
		route{min: slog.LevelInfo, max: slog.Level(100), handler: newTestHandler(&defaultBuf)},
	)
	logger := slog.New(router)

	logger.Debug("debug-msg", "key", "val")
	logger.Info("info-msg", "key", "val")
	logger.Warn("warn-msg")
	logger.Error("error-msg")

	if !strings.Contains(debugBuf.String(), "debug-msg") {
		t.Errorf("debug handler should contain debug-msg, got: %s", debugBuf.String())
	}
	if !strings.Contains(defaultBuf.String(), "info-msg") {
		t.Errorf("default handler should contain info-msg, got: %s", defaultBuf.String())
	}
	if !strings.Contains(defaultBuf.String(), "warn-msg") {
		t.Errorf("default handler should contain warn-msg, got: %s", defaultBuf.String())
	}
	if !strings.Contains(defaultBuf.String(), "error-msg") {
		t.Errorf("default handler should contain error-msg, got: %s", defaultBuf.String())
	}
	if strings.Contains(debugBuf.String(), "info-msg") {
		t.Errorf("debug handler should NOT contain info-msg, got: %s", debugBuf.String())
	}
	if strings.Contains(defaultBuf.String(), "debug-msg") {
		t.Errorf("default handler should NOT contain debug-msg, got: %s", defaultBuf.String())
	}
}

func TestRouterHandler_WithAttrs(t *testing.T) {
	var buf bytes.Buffer
	router := NewRouterHandler(
		route{min: slog.LevelDebug - 4, max: slog.Level(100), handler: newTestHandler(&buf)},
	)
	logger := slog.New(router).With("component", "test")

	logger.Info("with-attrs")

	if !strings.Contains(buf.String(), "component=test") {
		t.Errorf("should contain component=test, got: %s", buf.String())
	}
}

func TestRouterHandler_Enabled(t *testing.T) {
	var buf bytes.Buffer
	router := NewRouterHandler(
		route{min: slog.LevelInfo, max: slog.LevelDebug + 8, handler: newTestHandler(&buf)},
	)
	if router.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("Debug should not be enabled when min level is Info")
	}
	if !router.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Info should be enabled")
	}
}
